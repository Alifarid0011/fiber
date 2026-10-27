package keyauth

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/extractors"
	"github.com/gofiber/fiber/v3/internal/apptest"
	"github.com/gofiber/fiber/v3/internal/loggertest"
	"github.com/gofiber/fiber/v3/internal/redact"
	fiberlog "github.com/gofiber/fiber/v3/log"
	"github.com/gofiber/fiber/v3/middleware/logger"
)

const CorrectKey = "correct-token_123./~+"

var testConfig = fiber.TestConfig{
	Timeout: 0,
}

const (
	paramExtractorName      = "param"
	formExtractorName       = "form"
	queryExtractorName      = "query"
	headerExtractorName     = "header"
	authHeaderExtractorName = "authHeader"
	cookieExtractorName     = "cookie"
)

func Test_AuthSources(t *testing.T) {
	// define test cases
	testSources := []string{headerExtractorName, authHeaderExtractorName, cookieExtractorName, queryExtractorName, paramExtractorName, formExtractorName}

	tests := []struct {
		route         string
		authTokenName string
		description   string
		APIKey        string
		expectedBody  string
		expectedCode  int
	}{
		{
			route:         "/",
			authTokenName: "access_token",
			description:   "auth with correct key",
			APIKey:        CorrectKey,
			expectedCode:  200,
			expectedBody:  "Success!",
		},
		{
			route:         "/",
			authTokenName: "access_token",
			description:   "auth with no key",
			APIKey:        "",
			expectedCode:  401, // 404 in case of param authentication
			expectedBody:  ErrMissingOrMalformedAPIKey.Error(),
		},
		{
			route:         "/",
			authTokenName: "access_token",
			description:   "auth with wrong key",
			APIKey:        "WRONGKEY",
			expectedCode:  401,
			expectedBody:  ErrMissingOrMalformedAPIKey.Error(),
		},
	}

	for _, authSource := range testSources {
		t.Run(authSource, func(t *testing.T) {
			for _, test := range tests {
				app := fiber.New(fiber.Config{UnescapePath: true})

				testKey := test.APIKey
				correctKey := CorrectKey

				// Use a simple key for param and cookie to avoid encoding issues in the test setup
				if authSource == paramExtractorName || authSource == cookieExtractorName {
					if test.APIKey != "" && test.APIKey != "WRONGKEY" {
						testKey = "simple-key"
						correctKey = "simple-key"
					}
				}

				authMiddleware := New(Config{
					Extractor: func() extractors.Extractor {
						switch authSource {
						case headerExtractorName:
							return extractors.FromHeader(test.authTokenName)
						case authHeaderExtractorName:
							return extractors.FromAuthHeader("Bearer")
						case cookieExtractorName:
							return extractors.FromCookie(test.authTokenName)
						case queryExtractorName:
							return extractors.FromQuery(test.authTokenName)
						case paramExtractorName:
							return extractors.FromParam(test.authTokenName)
						case formExtractorName:
							return extractors.FromForm(test.authTokenName)
						default:
							panic("unknown source")
						}
					}(),
					Validator: func(_ fiber.Ctx, key string) (bool, error) {
						if key == correctKey {
							return true, nil
						}
						return false, errors.New("invalid key")
					},
				})

				handler := func(c fiber.Ctx) error { return c.SendString("Success!") }

				method := fiber.MethodGet
				switch authSource {
				case paramExtractorName:
					app.Get("/:"+test.authTokenName, authMiddleware, handler)
				case formExtractorName:
					method = fiber.MethodPost
					app.Post("/", authMiddleware, handler)
				default:
					app.Get("/", authMiddleware, handler)
				}

				targetURL := "/"
				if authSource == paramExtractorName {
					targetURL = "/" + url.PathEscape(testKey)
				}

				var reqBody io.Reader
				if authSource == formExtractorName {
					form := url.Values{}
					form.Add(test.authTokenName, testKey)
					bodyStr := form.Encode()
					reqBody = strings.NewReader(bodyStr)
				}

				req, err := http.NewRequestWithContext(context.Background(), method, targetURL, reqBody)
				require.NoError(t, err)

				switch authSource {
				case headerExtractorName:
					req.Header.Set(test.authTokenName, testKey)
				case authHeaderExtractorName:
					if testKey != "" {
						req.Header.Set("Authorization", "Bearer "+testKey)
					}
				case cookieExtractorName:
					req.Header.Set("Cookie", test.authTokenName+"="+testKey)
				case queryExtractorName:
					q := req.URL.Query()
					q.Add(test.authTokenName, testKey)
					req.URL.RawQuery = q.Encode()
				case formExtractorName:
					req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
				default:
					// nothing to do for paramExtractorName
				}

				res, err := app.Test(req, testConfig)
				require.NoError(t, err, test.description)

				body := apptest.Body(t, res)
				errClose := res.Body.Close()
				require.NoError(t, errClose)

				expectedCode := test.expectedCode
				expectedBody := test.expectedBody
				if test.APIKey == "" || test.APIKey == "WRONGKEY" {
					expectedBody = ErrMissingOrMalformedAPIKey.Error()
				}

				if authSource == paramExtractorName && testKey == "" {
					expectedCode = 404
					expectedBody = "Not Found"
				}
				require.Equal(t, expectedCode, res.StatusCode, test.description)
				require.Equal(t, expectedBody, body, test.description)
			}
		})
	}
}

// validateCorrectKey accepts CorrectKey and refuses every other key with
// ErrMissingOrMalformedAPIKey.
func validateCorrectKey(_ fiber.Ctx, key string) (bool, error) {
	if key == CorrectKey {
		return true, nil
	}
	return false, ErrMissingOrMalformedAPIKey
}

// rejectAllKeys refuses every key with ErrMissingOrMalformedAPIKey.
func rejectAllKeys(fiber.Ctx, string) (bool, error) {
	return false, ErrMissingOrMalformedAPIKey
}

// bearerCorrectKey authenticates a request with CorrectKey in the Bearer scheme.
var bearerCorrectKey = apptest.WithHeader(fiber.HeaderAuthorization, "Bearer "+CorrectKey)

func TestMultipleKeyLookup(t *testing.T) {
	const (
		desc    = "auth with correct key"
		success = "Success!"
	)

	// set up the fiber endpoint
	app := fiber.New()

	customExtractor := extractors.Chain(
		extractors.FromAuthHeader("Bearer"),
		extractors.FromHeader("key"),
		extractors.FromCookie("key"),
		extractors.FromQuery("key"),
	)

	authMiddleware := New(Config{
		Extractor: customExtractor,
		Validator: func(_ fiber.Ctx, key string) (bool, error) {
			if key == CorrectKey {
				return true, nil
			}
			return false, errors.New("invalid key")
		},
	})
	app.Use(authMiddleware)
	app.Get("/foo", func(c fiber.Ctx) error { return c.SendString(success) })

	// construct the test HTTP request
	req, err := http.NewRequestWithContext(context.Background(), fiber.MethodGet, "/foo", http.NoBody)
	require.NoError(t, err)
	q := req.URL.Query()
	q.Add("key", CorrectKey)
	req.URL.RawQuery = q.Encode()

	res := apptest.Do(t, app, req, testConfig)

	// test the status and the body of the request
	require.Equal(t, 200, res.StatusCode, desc)
	require.Equal(t, success, apptest.Body(t, res), desc)

	// construct a second request without proper key
	req, err = http.NewRequestWithContext(context.Background(), fiber.MethodGet, "/foo", http.NoBody)
	require.NoError(t, err)
	res = apptest.Do(t, app, req, testConfig)
	require.Equal(t, ErrMissingOrMalformedAPIKey.Error(), apptest.Body(t, res))
}

func Test_MultipleKeyAuth(t *testing.T) {
	// set up the fiber endpoint
	app := fiber.New()

	// set up keyauth for /auth1 and /auth2, each with a different password
	for _, auth := range []struct{ route, password string }{{"/auth1", "password1"}, {"/auth2", "password2"}} {
		app.Use(New(Config{
			Next:      func(c fiber.Ctx) bool { return c.Path() != auth.route },
			Extractor: extractors.FromAuthHeader("Bearer"),
			Validator: func(_ fiber.Ctx, key string) (bool, error) {
				if key == auth.password {
					return true, nil
				}
				return false, errors.New("invalid key")
			},
		}))
	}

	app.Get("/", func(c fiber.Ctx) error { return c.SendString("No auth needed!") })

	app.Get("/auth1", func(c fiber.Ctx) error { return c.SendString("Successfully authenticated for auth1!") })

	app.Get("/auth2", func(c fiber.Ctx) error { return c.SendString("Successfully authenticated for auth2!") })

	// define test cases
	tests := []struct {
		route        string
		description  string
		APIKey       string
		expectedBody string
		expectedCode int
	}{
		// No auth needed for /
		{
			route:        "/",
			description:  "No password needed",
			APIKey:       "",
			expectedCode: 200,
			expectedBody: "No auth needed!",
		},

		// auth needed for auth1
		{
			route:        "/auth1",
			description:  "Normal Authentication Case",
			APIKey:       "password1",
			expectedCode: 200,
			expectedBody: "Successfully authenticated for auth1!",
		},
		{
			route:        "/auth1",
			description:  "Wrong API Key",
			APIKey:       "WRONG KEY",
			expectedCode: 401,
			expectedBody: ErrMissingOrMalformedAPIKey.Error(),
		},
		{
			route:        "/auth1",
			description:  "Wrong API Key",
			APIKey:       "", // NO KEY
			expectedCode: 401,
			expectedBody: ErrMissingOrMalformedAPIKey.Error(),
		},

		// Auth 2 has a different password
		{
			route:        "/auth2",
			description:  "Normal Authentication Case for auth2",
			APIKey:       "password2",
			expectedCode: 200,
			expectedBody: "Successfully authenticated for auth2!",
		},
		{
			route:        "/auth2",
			description:  "Wrong API Key",
			APIKey:       "WRONG KEY",
			expectedCode: 401,
			expectedBody: ErrMissingOrMalformedAPIKey.Error(),
		},
		{
			route:        "/auth2",
			description:  "Wrong API Key",
			APIKey:       "", // NO KEY
			expectedCode: 401,
			expectedBody: ErrMissingOrMalformedAPIKey.Error(),
		},
	}

	// run the tests
	for _, test := range tests {
		req, err := http.NewRequestWithContext(context.Background(), fiber.MethodGet, test.route, http.NoBody)
		require.NoError(t, err)
		if test.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+test.APIKey)
		}

		res := apptest.Do(t, app, req, testConfig)

		// test the status and the body of the request
		require.Equal(t, test.expectedCode, res.StatusCode, test.description)
		require.Equal(t, test.expectedBody, apptest.Body(t, res), test.description)
	}
}

func Test_CustomSuccessAndFailureHandlers(t *testing.T) {
	app := fiber.New()

	app.Use(New(Config{
		SuccessHandler: func(c fiber.Ctx) error {
			return c.Status(fiber.StatusOK).SendString("API key is valid and request was handled by custom success handler")
		},
		ErrorHandler: func(c fiber.Ctx, _ error) error {
			return c.Status(fiber.StatusUnauthorized).SendString("API key is invalid and request was handled by custom error handler")
		},
		Validator: validateCorrectKey,
	}))

	// Define a test handler that should not be called
	app.Get("/", func(_ fiber.Ctx) error {
		t.Error("Test handler should not be called")
		return nil
	})

	// A request without an API key is handled by the custom error handler
	apptest.ExpectBody(t, app, "/", http.StatusUnauthorized, "API key is invalid and request was handled by custom error handler")

	// A request with a valid API key in the Authorization header is handled by the custom success handler
	apptest.ExpectBody(t, app, apptest.Req(fiber.MethodGet, "/", bearerCorrectKey), http.StatusOK, "API key is valid and request was handled by custom success handler")
}

func Test_CustomNextFunc(t *testing.T) {
	app := fiber.New()

	app.Use(New(Config{
		Next:      func(c fiber.Ctx) bool { return c.Path() == "/allowed" },
		Validator: validateCorrectKey,
	}))

	// Define a test handler
	app.Get("/allowed", func(c fiber.Ctx) error {
		return c.SendString("API key is valid and request was allowed by custom filter")
	})
	app.Get("/not-allowed", func(c fiber.Ctx) error { return c.SendString("Should be protected") })

	// A request with the "/allowed" path is let through
	apptest.ExpectBody(t, app, "/allowed", http.StatusOK, "API key is valid and request was allowed by custom filter")

	// A request with a different path and without the correct key is refused
	apptest.ExpectBody(t, app, "/not-allowed", http.StatusUnauthorized, ErrMissingOrMalformedAPIKey.Error())

	// A request with a different path and with the correct key is let through
	apptest.ExpectBody(t, app, apptest.Req(fiber.MethodGet, "/not-allowed", bearerCorrectKey), http.StatusOK, "Should be protected")
}

func Test_TokenFromContext_None(t *testing.T) {
	app := fiber.New()
	// Define a test handler that checks TokenFromContext
	app.Get("/", func(c fiber.Ctx) error { return c.SendString(TokenFromContext(c)) })

	// Verify a "" is sent back if nothing sets the token on the context.
	apptest.ExpectBody(t, app, "/", http.StatusOK, "")
}

func Test_TokenFromContext(t *testing.T) {
	app := fiber.New()
	// Wire up keyauth middleware to set TokenFromContext now
	app.Use(New(Config{
		Extractor: extractors.FromAuthHeader("Basic"),
		Validator: validateCorrectKey,
	}))
	// Define a test handler that checks TokenFromContext
	app.Get("/", func(c fiber.Ctx) error { return c.SendString(TokenFromContext(c)) })

	apptest.ExpectBody(t, app, apptest.Req(fiber.MethodGet, "/", apptest.WithHeader(fiber.HeaderAuthorization, "Basic "+CorrectKey)), http.StatusOK, CorrectKey)
}

func Test_LogTagsRedactToken(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	app := fiber.New()
	app.Use(New(Config{
		Extractor: extractors.FromAuthHeader("Bearer"),
		Validator: func(_ fiber.Ctx, key string) (bool, error) { return key == CorrectKey, nil },
	}))
	app.Use(logger.New(logger.Config{
		Format: "${api-key}",
		Stream: &buf,
	}))
	app.Get("/", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

	apptest.Expect(t, app, apptest.Req(fiber.MethodGet, "/", bearerCorrectKey), fiber.StatusOK)
	require.Equal(t, redact.Prefix(CorrectKey), buf.String())
}

// Test_LogContextTagRedactsToken runs serially because it mutates package-global
// default logger output and context format.
func Test_LogContextTagRedactsToken(t *testing.T) {
	buf := loggertest.CaptureContextLog(t, "api-key=${api-key} ")

	app := fiber.New()
	app.Use(New(Config{
		Extractor: extractors.FromAuthHeader("Bearer"),
		Validator: func(_ fiber.Ctx, key string) (bool, error) { return key == CorrectKey, nil },
	}))
	app.Get("/", func(c fiber.Ctx) error {
		fiberlog.WithContext(c).Info("start")
		return c.SendStatus(fiber.StatusOK)
	})

	apptest.Expect(t, app, apptest.Req(fiber.MethodGet, "/", bearerCorrectKey), fiber.StatusOK)
	require.Contains(t, buf.String(), "[Info] api-key="+redact.Prefix(CorrectKey)+" start")
}

func Test_TokenFromContext_Types(t *testing.T) {
	t.Parallel()

	app := fiber.New(fiber.Config{PassLocalsToContext: true})
	app.Use(New(Config{
		Extractor: extractors.FromAuthHeader("Basic"),
		Validator: validateCorrectKey,
	}))

	app.Get("/", func(c fiber.Ctx) error {
		require.Equal(t, CorrectKey, TokenFromContext(c))
		customCtx, ok := c.(fiber.CustomCtx)
		require.True(t, ok)
		require.Equal(t, CorrectKey, TokenFromContext(customCtx))
		require.Equal(t, CorrectKey, TokenFromContext(c.RequestCtx()))
		require.Equal(t, CorrectKey, TokenFromContext(c.Context()))
		return c.SendStatus(fiber.StatusOK)
	})

	apptest.Expect(t, app, apptest.Req(fiber.MethodGet, "/", apptest.WithHeader(fiber.HeaderAuthorization, "Basic "+CorrectKey)), fiber.StatusOK)
}

func Test_AuthScheme(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		scheme        string
		authorization string
		body          string
		status        int
	}{
		{name: "Token scheme with the key", scheme: "Token", authorization: "Token " + CorrectKey, status: http.StatusOK, body: "API key is valid"},
		{name: "Basic scheme without a key", scheme: "Basic", status: http.StatusUnauthorized, body: ErrMissingOrMalformedAPIKey.Error()},
		{name: "Basic scheme with the key", scheme: "Basic", authorization: "Basic " + CorrectKey, status: http.StatusOK, body: "API key is valid"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app := fiber.New()
			app.Use(New(Config{
				Extractor: extractors.FromAuthHeader(tc.scheme),
				Validator: validateCorrectKey,
			}))
			app.Get("/", func(c fiber.Ctx) error { return c.SendString("API key is valid") })

			req := apptest.Req(fiber.MethodGet, "/")
			if tc.authorization != "" {
				req.Header.Add(fiber.HeaderAuthorization, tc.authorization)
			}
			apptest.ExpectBody(t, app, req, tc.status, tc.body)
		})
	}
}

// Test_HeaderScheme covers how the Authorization header is split into a scheme
// and the key, with the default Bearer extractor.
func Test_HeaderScheme(t *testing.T) {
	t.Parallel()

	tests := []struct {
		validator     func(fiber.Ctx, string) (bool, error)
		name          string
		authorization string
		body          string
		challenge     string
		status        int
	}{
		{name: "scheme is case insensitive", validator: validateCorrectKey, authorization: "bearer " + CorrectKey, status: http.StatusOK, body: "OK"},
		{name: "multiple spaces", validator: validateCorrectKey, authorization: "Bearer    " + CorrectKey, status: http.StatusUnauthorized, body: ErrMissingOrMalformedAPIKey.Error()},
		{name: "missing space", validator: rejectAllKeys, authorization: "Bearer" + CorrectKey, status: http.StatusUnauthorized, body: ErrMissingOrMalformedAPIKey.Error()},
		{name: "no token", validator: rejectAllKeys, authorization: "Bearer ", status: http.StatusUnauthorized, body: ErrMissingOrMalformedAPIKey.Error()},
		// No space between "Bearer" and token
		{name: "no separator", validator: rejectAllKeys, authorization: "BearerTokenWithoutSpace", status: http.StatusUnauthorized, body: ErrMissingOrMalformedAPIKey.Error()},
		// Authorization header with scheme followed by only spaces/tabs (no actual token)
		{name: "empty token after trim", validator: rejectAllKeys, authorization: "Bearer \t  \t ", status: http.StatusUnauthorized, body: ErrMissingOrMalformedAPIKey.Error()},
		{
			name:          "invalid key",
			validator:     func(fiber.Ctx, string) (bool, error) { return false, errors.New("invalid") },
			authorization: "Bearer " + CorrectKey,
			status:        http.StatusUnauthorized,
			body:          ErrMissingOrMalformedAPIKey.Error(),
			challenge:     `Bearer realm="Restricted"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app := fiber.New()
			app.Use(New(Config{Validator: tc.validator}))
			app.Get("/", func(c fiber.Ctx) error { return c.SendString("OK") })

			req := apptest.Req(fiber.MethodGet, "/", apptest.WithHeader(fiber.HeaderAuthorization, tc.authorization))
			resp := apptest.ExpectBody(t, app, req, tc.status, tc.body)
			if tc.challenge != "" {
				require.Equal(t, tc.challenge, resp.Header.Get(fiber.HeaderWWWAuthenticate))
			}
		})
	}
}

// Test_Challenge covers the WWW-Authenticate and Proxy-Authenticate headers a
// refused request is answered with.
func Test_Challenge(t *testing.T) {
	t.Parallel()

	failing := func(fiber.Ctx, string) (bool, error) { return false, errors.New("validation failed") }
	invalid := func(fiber.Ctx, string) (bool, error) { return false, errors.New("invalid") }
	valid := func(fiber.Ctx, string) (bool, error) { return true, nil }
	// answerWith refuses every request with the given error.
	answerWith := func(err error) fiber.ErrorHandler { return func(fiber.Ctx, error) error { return err } }
	// answerStatus refuses every request with the given status and text.
	answerStatus := func(status int, text string) fiber.ErrorHandler {
		return func(c fiber.Ctx, _ error) error { return c.Status(status).SendString(text) }
	}

	tests := []struct {
		headers       map[string]string
		name          string
		authorization string
		config        Config
		status        int
	}{
		{
			name:          "default config on failure",
			config:        Config{Validator: failing},
			authorization: "Bearer somekey",
			status:        fiber.StatusUnauthorized,
			headers:       map[string]string{fiber.HeaderWWWAuthenticate: `Bearer realm="Restricted"`},
		},
		{
			name:          "custom realm on failure",
			config:        Config{Validator: failing, Realm: "My Custom Realm"},
			authorization: "Bearer somekey",
			status:        fiber.StatusUnauthorized,
			headers:       map[string]string{fiber.HeaderWWWAuthenticate: `Bearer realm="My Custom Realm"`},
		},
		{
			name:    "default header for non-auth-header extractor",
			config:  Config{Validator: failing, Extractor: extractors.FromQuery("api_key")},
			status:  fiber.StatusUnauthorized,
			headers: map[string]string{fiber.HeaderWWWAuthenticate: `ApiKey realm="Restricted"`},
		},
		{
			name:          "no header on success",
			config:        Config{Validator: valid},
			authorization: "Bearer somekey",
			status:        fiber.StatusOK,
			headers:       map[string]string{fiber.HeaderWWWAuthenticate: ""},
		},
		{
			name:    "chained extractor with auth header",
			config:  Config{Validator: failing, Extractor: extractors.Chain(extractors.FromQuery("q"), extractors.FromAuthHeader("MyScheme"))},
			status:  fiber.StatusUnauthorized,
			headers: map[string]string{fiber.HeaderWWWAuthenticate: `MyScheme realm="Restricted"`},
		},
		{
			name:    "chained extractor without auth header",
			config:  Config{Validator: failing, Extractor: extractors.Chain(extractors.FromQuery("q"), extractors.FromCookie("c"))},
			status:  fiber.StatusUnauthorized,
			headers: map[string]string{fiber.HeaderWWWAuthenticate: `ApiKey realm="Restricted"`},
		},
		{
			name:    "default error handler without a key",
			config:  Config{Validator: rejectAllKeys},
			status:  http.StatusUnauthorized,
			headers: map[string]string{fiber.HeaderWWWAuthenticate: `Bearer realm="Restricted"`},
		},
		{
			name:    "custom challenge",
			config:  Config{Validator: invalid, Extractor: extractors.FromQuery("api_key"), Challenge: `ApiKey realm="Restricted"`},
			status:  http.StatusUnauthorized,
			headers: map[string]string{fiber.HeaderWWWAuthenticate: `ApiKey realm="Restricted"`},
		},
		{
			name: "bearer error fields",
			config: Config{
				Validator:        invalid,
				Error:            "invalid_token",
				ErrorDescription: "token expired",
				ErrorURI:         "https://example.com",
			},
			authorization: "Bearer something",
			status:        http.StatusUnauthorized,
			headers: map[string]string{
				fiber.HeaderWWWAuthenticate: `Bearer realm="Restricted", error="invalid_token", error_description="token expired", error_uri="https://example.com"`,
			},
		},
		{
			name:          "bearer error URI only",
			config:        Config{Validator: invalid, Error: "invalid_token", ErrorURI: "https://example.com/docs"},
			authorization: "Bearer something",
			status:        http.StatusUnauthorized,
			headers: map[string]string{
				fiber.HeaderWWWAuthenticate: `Bearer realm="Restricted", error="invalid_token", error_uri="https://example.com/docs"`,
			},
		},
		{
			name:          "bearer insufficient scope",
			config:        Config{Validator: invalid, Error: ErrorInsufficientScope, Scope: "read"},
			authorization: "Bearer something",
			status:        http.StatusUnauthorized,
			headers: map[string]string{
				fiber.HeaderWWWAuthenticate: `Bearer realm="Restricted", error="insufficient_scope", scope="read"`,
			},
		},
		{
			name:          "no challenge unless the answer is a 401",
			config:        Config{Validator: invalid, ErrorHandler: answerStatus(fiber.StatusForbidden, "forbidden")},
			authorization: "Bearer bad",
			status:        http.StatusForbidden,
			headers:       map[string]string{fiber.HeaderWWWAuthenticate: ""},
		},
		{
			name:    "default challenge for a non-auth extractor",
			config:  Config{Validator: rejectAllKeys, Extractor: extractors.FromQuery("api_key")},
			status:  http.StatusUnauthorized,
			headers: map[string]string{fiber.HeaderWWWAuthenticate: `ApiKey realm="Restricted"`},
		},
		{
			name: "one challenge per auth scheme",
			config: Config{
				Validator: invalid,
				Extractor: extractors.Chain(extractors.FromAuthHeader("Bearer"), extractors.FromAuthHeader("ApiKey")),
			},
			status:  http.StatusUnauthorized,
			headers: map[string]string{fiber.HeaderWWWAuthenticate: `Bearer realm="Restricted", ApiKey realm="Restricted"`},
		},
		{
			name:          "proxy authentication required",
			config:        Config{Validator: invalid, ErrorHandler: answerStatus(fiber.StatusProxyAuthRequired, "proxy auth")},
			authorization: "Bearer bad",
			status:        http.StatusProxyAuthRequired,
			headers: map[string]string{
				fiber.HeaderProxyAuthenticate: `Bearer realm="Restricted"`,
				fiber.HeaderWWWAuthenticate:   "",
			},
		},
		// The challenge must be set when the ErrorHandler returns the *fiber.Error instead of writing it.
		{
			name:    "returned unauthorized error",
			config:  Config{Validator: rejectAllKeys, ErrorHandler: answerWith(fiber.ErrUnauthorized)},
			status:  fiber.StatusUnauthorized,
			headers: map[string]string{fiber.HeaderWWWAuthenticate: `Bearer realm="Restricted"`},
		},
		{
			name:    "returned proxy auth required error",
			config:  Config{Validator: rejectAllKeys, ErrorHandler: answerWith(fiber.ErrProxyAuthRequired)},
			status:  fiber.StatusProxyAuthRequired,
			headers: map[string]string{fiber.HeaderProxyAuthenticate: `Bearer realm="Restricted"`},
		},
	}

	for i := range tests {
		tc := &tests[i]
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app := fiber.New()
			app.Use(New(tc.config))
			app.Get("/", func(c fiber.Ctx) error { return c.SendString("OK") })

			req := apptest.Req(fiber.MethodGet, "/")
			if tc.authorization != "" {
				req.Header.Add(fiber.HeaderAuthorization, tc.authorization)
			}
			resp := apptest.Expect(t, app, req, tc.status)

			for name, want := range tc.headers {
				assert.Equal(t, want, resp.Header.Get(name), name)
			}
		})
	}
}

// Test_ChallengeQuotedStringEscaping verifies both default ApiKey and Bearer
// challenges use HTTP quoted-string escaping rather than Go syntax.
func Test_ChallengeQuotedStringEscaping(t *testing.T) {
	t.Parallel()

	t.Run("default ApiKey challenge", func(t *testing.T) {
		t.Parallel()

		app := fiber.New()
		app.Use(New(Config{
			Extractor: extractors.FromQuery("key"),
			Realm:     "area \"A\"\\\x01\n",
			Validator: func(_ fiber.Ctx, _ string) (bool, error) { return false, errors.New("invalid") },
		}))

		res := apptest.Do(t, app, "/")
		require.Equal(t, `ApiKey realm="area \"A\"\\%01\n"`, res.Header.Get(fiber.HeaderWWWAuthenticate))
	})

	t.Run("Bearer parameters", func(t *testing.T) {
		t.Parallel()

		app := fiber.New()
		app.Use(New(Config{
			Realm:            "area \"A\"\\\x01",
			Error:            ErrorInvalidToken,
			ErrorDescription: "bad \"token\"\\\x02\n",
			ErrorURI:         `https://example.com/docs?q="x"`,
			Validator:        func(_ fiber.Ctx, _ string) (bool, error) { return false, errors.New("invalid") },
		}))

		res := apptest.Do(t, app, apptest.Req(fiber.MethodGet, "/", apptest.WithHeader(fiber.HeaderAuthorization, "Bearer bad")))
		require.Equal(
			t,
			`Bearer realm="area \"A\"\\%01", error="invalid_token", error_description="bad \"token\"\\%02\n", error_uri="https://example.com/docs?q=\"x\""`,
			res.Header.Get(fiber.HeaderWWWAuthenticate),
		)
	})
}

// Test_New_InvalidConfig covers the configurations New refuses with a panic.
func Test_New_InvalidConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		wantPanic string
		config    Config
	}{
		{name: "scope without insufficient_scope", config: Config{Scope: "foo"}, wantPanic: "fiber: keyauth scope requires insufficient_scope error"},
		{name: "insufficient_scope without scope", config: Config{Error: ErrorInsufficientScope}, wantPanic: "fiber: keyauth insufficient_scope requires scope"},
		{name: "scope with an invalid token", config: Config{Error: ErrorInsufficientScope, Scope: "read \"write\""}, wantPanic: "fiber: keyauth scope contains invalid token"},
		{name: "valid scope", config: Config{Error: ErrorInsufficientScope, Scope: "read write:all"}},
		{name: "unsupported error token", config: Config{Error: "unsupported"}, wantPanic: "fiber: keyauth unsupported error token"},
		{name: "error description without error", config: Config{ErrorDescription: "desc"}, wantPanic: "fiber: keyauth error_description requires error"},
		{name: "error URI without error", config: Config{ErrorURI: "https://example.com/docs"}, wantPanic: "fiber: keyauth error_uri requires error"},
		{name: "relative error URI", config: Config{Error: "invalid_token", ErrorURI: "/docs"}, wantPanic: "fiber: keyauth error_uri must be absolute"},
	}

	for i := range tests {
		tc := &tests[i]
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := tc.config
			cfg.Validator = func(fiber.Ctx, string) (bool, error) { return true, nil }
			if tc.wantPanic == "" {
				require.NotPanics(t, func() { New(cfg) })
				return
			}
			require.PanicsWithValue(t, tc.wantPanic, func() { New(cfg) })
		})
	}
}

// benchKey is a JWT-sized token68 credential, the shape a Bearer token has in
// practice.
const benchKey = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9P"

// benchValidator compares in constant time, as a real deployment must, but
// without hashing, so the numbers are the middleware and not a digest.
func benchValidator(_ fiber.Ctx, key string) (bool, error) {
	return subtle.ConstantTimeCompare([]byte(key), []byte(benchKey)) == 1, nil
}

// benchApp registers the middleware with cfg on a route that answers 418. cfg
// is a pointer because Config is 200 bytes and gocritic refuses the copy.
func benchApp(cfg *Config) fasthttp.RequestHandler {
	app := fiber.New()
	app.Use(New(*cfg))
	app.Get("/", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusTeapot) })
	return app.Handler()
}

func Benchmark_KeyAuth_Bearer(b *testing.B) {
	h := benchApp(&Config{Validator: benchValidator})

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fiber.MethodGet)
	ctx.Request.SetRequestURI("/")
	ctx.Request.Header.Set(fiber.HeaderAuthorization, "Bearer "+benchKey)

	b.ReportAllocs()

	for b.Loop() {
		// What fasthttp does between requests. Without it the extractor's
		// request-local state is installed once and reused, and the per-request
		// cost of installing and recycling it is not in the number.
		ctx.ResetUserValues()
		h(ctx)
	}

	require.Equal(b, fiber.StatusTeapot, ctx.Response.Header.StatusCode())
}

// The credential is in the last source, so the walk pays for both misses.
func Benchmark_KeyAuth_Chain(b *testing.B) {
	h := benchApp(&Config{
		Validator: benchValidator,
		Extractor: extractors.Chain(
			extractors.FromAuthHeader("Bearer"),
			extractors.FromHeader("X-API-Key"),
			extractors.FromQuery("api_key"),
		),
	})

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fiber.MethodGet)
	ctx.Request.SetRequestURI("/?api_key=" + benchKey)

	b.ReportAllocs()

	for b.Loop() {
		ctx.ResetUserValues()
		h(ctx)
	}

	require.Equal(b, fiber.StatusTeapot, ctx.Response.Header.StatusCode())
}

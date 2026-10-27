package cors

import (
	"bufio"
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/internal/apptest"
	"github.com/gofiber/fiber/v3/log"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func Test_CORS_Defaults(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	app.Use(New())

	testDefaultOrEmptyConfig(t, app)
}

func Test_CORS_Empty_Config(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	app.Use(New(Config{}))

	testDefaultOrEmptyConfig(t, app)
}

// corsTestPreflight sends a preflight request from origin through h and returns
// the context that holds the answer.
func corsTestPreflight(h fasthttp.RequestHandler, origin string) *fasthttp.RequestCtx {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.SetRequestURI("/")
	ctx.Request.Header.SetMethod(fiber.MethodOptions)
	ctx.Request.Header.Set(fiber.HeaderAccessControlRequestMethod, fiber.MethodGet)
	ctx.Request.Header.Set(fiber.HeaderOrigin, origin)
	h(ctx)
	return ctx
}

// corsTestAllowOrigin returns the Access-Control-Allow-Origin of the answer in ctx.
func corsTestAllowOrigin(ctx *fasthttp.RequestCtx) string {
	return string(ctx.Response.Header.Peek(fiber.HeaderAccessControlAllowOrigin))
}

// corsTestRaw reads the raw HTTP request raw, which keeps its header names as
// written when app does not normalize them, and sends it through app.
func corsTestRaw(t *testing.T, app *fiber.App, raw string) *fasthttp.RequestCtx {
	t.Helper()

	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	if app.Config().DisableHeaderNormalizing {
		req.Header.DisableNormalizing()
	}
	require.NoError(t, req.Read(bufio.NewReader(strings.NewReader(raw))))

	fctx := &fasthttp.RequestCtx{}
	fctx.Init(req, nil, nil)
	app.Handler()(fctx)
	return fctx
}

func Test_CORS_WildcardHeaders(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	app.Use(New(Config{AllowMethods: []string{"*"}, AllowHeaders: []string{"*"}, ExposeHeaders: []string{"*"}}))

	// Test preflight request
	ctx := corsTestPreflight(app.Handler(), "http://localhost")

	require.Equal(t, "*", corsTestAllowOrigin(ctx))
	require.Empty(t, string(ctx.Response.Header.Peek(fiber.HeaderAccessControlAllowCredentials)))
	require.Equal(t, "*", string(ctx.Response.Header.Peek(fiber.HeaderAccessControlAllowMethods)))
	require.Equal(t, "*", string(ctx.Response.Header.Peek(fiber.HeaderAccessControlAllowHeaders)))
	require.Equal(t, "*", string(ctx.Response.Header.Peek(fiber.HeaderAccessControlExposeHeaders)))
}

func Test_CORS_Negative_MaxAge(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(New(Config{MaxAge: -1}))

	ctx := corsTestPreflight(app.Handler(), "http://localhost")

	require.Equal(t, "0", string(ctx.Response.Header.Peek(fiber.HeaderAccessControlMaxAge)))
}

func Test_CORS_MaxAge_NotSetOnSimpleRequest(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(New(Config{MaxAge: 100}))

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fiber.MethodGet)
	ctx.Request.Header.Set(fiber.HeaderOrigin, "http://localhost")
	app.Handler()(ctx)

	require.Empty(t, string(ctx.Response.Header.Peek(fiber.HeaderAccessControlMaxAge)))
}

func Test_CORS_Preserve_Origin_Case(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(New(Config{AllowOrigins: []string{"http://example.com"}}))

	origin := "HTTP://EXAMPLE.COM"

	require.Equal(t, origin, corsTestAllowOrigin(corsTestPreflight(app.Handler(), origin)))
}

func Test_CORS_AllowOrigins_NormalizedExactLookup(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(New(Config{AllowOrigins: []string{" HTTP://EXAMPLE.COM/ "}}))

	origin := "http://example.com"

	require.Equal(t, origin, corsTestAllowOrigin(corsTestPreflight(app.Handler(), origin)))
}

func testDefaultOrEmptyConfig(t *testing.T, app *fiber.App) {
	t.Helper()

	h := app.Handler()

	// Test default GET response headers
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fiber.MethodGet)
	ctx.Request.Header.Set(fiber.HeaderOrigin, "http://localhost")
	h(ctx)

	require.Equal(t, "*", corsTestAllowOrigin(ctx))
	require.Empty(t, string(ctx.Response.Header.Peek(fiber.HeaderAccessControlAllowCredentials)))
	require.Empty(t, string(ctx.Response.Header.Peek(fiber.HeaderAccessControlExposeHeaders)))

	// Test default OPTIONS (preflight) response headers
	ctx = corsTestPreflight(h, "http://localhost")

	require.Equal(t, "GET, POST, HEAD, PUT, DELETE, PATCH, QUERY", string(ctx.Response.Header.Peek(fiber.HeaderAccessControlAllowMethods)))
	require.Empty(t, string(ctx.Response.Header.Peek(fiber.HeaderAccessControlAllowHeaders)))
	require.Empty(t, string(ctx.Response.Header.Peek(fiber.HeaderAccessControlMaxAge)))
}

func Test_CORS_AllowOrigins_Vary(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	app.Use(New(Config{AllowOrigins: []string{"http://localhost"}}))

	h := app.Handler()

	// Test Vary header non-Cors request
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fiber.MethodGet)
	h(ctx)
	require.Contains(t, string(ctx.Response.Header.Peek(fiber.HeaderVary)), fiber.HeaderOrigin, "Vary header should be set")

	// Test Vary header Cors request
	ctx = corsTestPreflight(h, "http://localhost")
	require.Contains(t, string(ctx.Response.Header.Peek(fiber.HeaderVary)), fiber.HeaderOrigin, "Vary header should be set")
}

func Test_CORS_Wildcard(t *testing.T) {
	t.Parallel()
	// New fiber instance
	app := fiber.New()
	// OPTIONS (preflight) response headers when AllowOrigins is *
	app.Use(New(Config{
		MaxAge:        3600,
		ExposeHeaders: []string{"X-Request-ID"},
		AllowHeaders:  []string{"Authentication"},
	}))
	// Get handler pointer
	handler := app.Handler()

	// Make and perform request
	ctx := corsTestPreflight(handler, "http://localhost")

	// Check result
	require.Equal(t, "*", corsTestAllowOrigin(ctx)) // Validates request is not reflecting origin in the response
	require.Contains(t, string(ctx.Response.Header.Peek(fiber.HeaderVary)), fiber.HeaderOrigin, "Vary header should be set")
	require.Empty(t, string(ctx.Response.Header.Peek(fiber.HeaderAccessControlAllowCredentials)))
	require.Equal(t, "3600", string(ctx.Response.Header.Peek(fiber.HeaderAccessControlMaxAge)))
	require.Equal(t, "Authentication", string(ctx.Response.Header.Peek(fiber.HeaderAccessControlAllowHeaders)))

	// Test non OPTIONS (preflight) response headers
	ctx = &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fiber.MethodGet)
	ctx.Request.Header.Set(fiber.HeaderOrigin, "http://localhost")
	handler(ctx)

	require.NotContains(t, string(ctx.Response.Header.Peek(fiber.HeaderVary)), fiber.HeaderOrigin, "Vary header should not be set")
	require.Empty(t, string(ctx.Response.Header.Peek(fiber.HeaderAccessControlAllowCredentials)))
	require.Equal(t, "X-Request-ID", string(ctx.Response.Header.Peek(fiber.HeaderAccessControlExposeHeaders)))
}

func Test_CORS_Origin_AllowCredentials(t *testing.T) {
	t.Parallel()
	// New fiber instance
	app := fiber.New()
	// OPTIONS (preflight) response headers when AllowOrigins is *
	app.Use(New(Config{
		AllowOrigins:     []string{"http://localhost"},
		AllowCredentials: true,
		MaxAge:           3600,
		ExposeHeaders:    []string{"X-Request-ID"},
		AllowHeaders:     []string{"Authentication"},
	}))
	// Get handler pointer
	handler := app.Handler()

	// Make and perform request
	ctx := corsTestPreflight(handler, "http://localhost")

	// Check result
	require.Equal(t, "http://localhost", corsTestAllowOrigin(ctx))
	require.Equal(t, "true", string(ctx.Response.Header.Peek(fiber.HeaderAccessControlAllowCredentials)))
	require.Equal(t, "3600", string(ctx.Response.Header.Peek(fiber.HeaderAccessControlMaxAge)))
	require.Equal(t, "Authentication", string(ctx.Response.Header.Peek(fiber.HeaderAccessControlAllowHeaders)))

	// Test non OPTIONS (preflight) response headers
	ctx = &fasthttp.RequestCtx{}
	ctx.Request.Header.Set(fiber.HeaderOrigin, "http://localhost")
	ctx.Request.Header.SetMethod(fiber.MethodGet)
	handler(ctx)

	require.Equal(t, "true", string(ctx.Response.Header.Peek(fiber.HeaderAccessControlAllowCredentials)))
	require.Equal(t, "X-Request-ID", string(ctx.Response.Header.Peek(fiber.HeaderAccessControlExposeHeaders)))
}

// Test for fiber-ghsa-fmg4-x8pw-hjhg
func Test_CORS_Wildcard_AllowCredentials_Panic(t *testing.T) {
	t.Parallel()

	require.Panics(t, func() {
		fiber.New().Use(New(Config{AllowOrigins: []string{"*"}, AllowCredentials: true}))
	}, "Expected a panic when AllowOrigins is '*' and AllowCredentials is true")
}

// Test that a warning is logged when AllowOrigins allows all origins and
// AllowOriginsFunc is also provided.
func Test_CORS_Warn_AllowAllOrigins_WithFunc(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	fiber.New().Use(New(Config{AllowOrigins: []string{"*"}, AllowOriginsFunc: func(string) bool { return true }}))

	require.Contains(t, buf.String(), "AllowOriginsFunc' will not be used")
}

func Test_CORS_Invalid_Origins_Panic(t *testing.T) {
	t.Parallel()

	invalidOrigins := []string{
		"localhost",
		"http://foo.[a-z]*.example.com",
		"http://*",
		"https://*",
		"http://*.com*",
		"invalid url",
		"*",
		"http://origin.com,invalid url",
		// add more invalid origins as needed
	}

	for _, origin := range invalidOrigins {
		t.Run(origin, func(t *testing.T) {
			t.Parallel()

			require.Panics(t, func() {
				fiber.New().Use(New(Config{AllowOrigins: []string{origin}, AllowCredentials: true}))
			}, "Expected a panic for invalid origin: %s", origin)
		})
	}
}

func Test_CORS_DisableValueRedaction(t *testing.T) {
	t.Parallel()

	require.PanicsWithValue(t, "[CORS] Invalid origin format in configuration: [redacted]", func() {
		New(Config{AllowOrigins: []string{"http://"}, DisableValueRedaction: false})
	})

	require.PanicsWithValue(t, "[CORS] Invalid origin format in configuration: http://", func() {
		New(Config{AllowOrigins: []string{"http://"}, DisableValueRedaction: true})
	})
}

func Test_CORS_Subdomain(t *testing.T) {
	t.Parallel()
	// New fiber instance
	app := fiber.New()
	// OPTIONS (preflight) response headers when AllowOrigins is set to a subdomain
	app.Use("/", New(Config{AllowOrigins: []string{"  http://*.example.com  "}}))

	handler := app.Handler()

	tests := []struct {
		name   string
		origin string
		want   string // the Allow-Origin header, none when empty
	}{
		// Allow-Origin header should be "" because http://google.com does not satisfy http://*.example.com
		{name: "disallowed origin", origin: "http://google.com"},
		{name: "domain only (disallowed)", origin: "http://example.com"},
		{name: "malformed subdomain (disallowed)", origin: "http://evil.comexample.com"},
		{name: "allowed origin", origin: "http://test.example.com", want: "http://test.example.com"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.want, corsTestAllowOrigin(corsTestPreflight(handler, tc.origin)))
		})
	}
}

func Test_CORS_AllowOriginScheme(t *testing.T) {
	t.Parallel()
	tests := []struct {
		reqOrigin         string
		pattern           []string
		shouldAllowOrigin bool
	}{
		{pattern: []string{"http://example.com"}, reqOrigin: "http://example.com", shouldAllowOrigin: true},
		{pattern: []string{"HTTP://EXAMPLE.COM"}, reqOrigin: "http://example.com", shouldAllowOrigin: true},
		{pattern: []string{"https://example.com"}, reqOrigin: "https://example.com", shouldAllowOrigin: true},
		{pattern: []string{"http://example.com"}, reqOrigin: "https://example.com", shouldAllowOrigin: false},
		{pattern: []string{"http://*.example.com"}, reqOrigin: "http://aaa.example.com", shouldAllowOrigin: true},
		{pattern: []string{"http://*.example.com"}, reqOrigin: "http://bbb.aaa.example.com", shouldAllowOrigin: true},
		{pattern: []string{"http://*.aaa.example.com"}, reqOrigin: "http://bbb.aaa.example.com", shouldAllowOrigin: true},
		{pattern: []string{"http://*.example.com:8080"}, reqOrigin: "http://aaa.example.com:8080", shouldAllowOrigin: true},
		{pattern: []string{"http://*.example.com"}, reqOrigin: "http://1.2.aaa.example.com", shouldAllowOrigin: true},
		{pattern: []string{"http://example.com"}, reqOrigin: "http://gofiber.com", shouldAllowOrigin: false},
		{pattern: []string{"http://*.aaa.example.com"}, reqOrigin: "http://ccc.bbb.example.com", shouldAllowOrigin: false},
		{
			pattern:           []string{"http://*.example.com"},
			reqOrigin:         "http://1234567890.1234567890.1234567890.1234567890.1234567890.1234567890.1234567890.1234567890.1234567890.1234567890.1234567890.example.com",
			shouldAllowOrigin: true,
		},
		{pattern: []string{"http://example.com"}, reqOrigin: "http://ccc.bbb.example.com", shouldAllowOrigin: false},
		{pattern: []string{"https://--aaa.bbb.com"}, reqOrigin: "https://prod-preview--aaa.bbb.com", shouldAllowOrigin: false},
		{pattern: []string{"http://*.example.com"}, reqOrigin: "http://ccc.bbb.example.com", shouldAllowOrigin: true},
		{pattern: []string{"http://domain-1.com", "http://example.com"}, reqOrigin: "http://example.com", shouldAllowOrigin: true},
		{pattern: []string{"http://domain-1.com", "http://example.com"}, reqOrigin: "http://domain-2.com", shouldAllowOrigin: false},
		{pattern: []string{"http://domain-1.com", "http://example.com"}, reqOrigin: "http://example.com", shouldAllowOrigin: true},
		{pattern: []string{"http://domain-1.com", "http://example.com"}, reqOrigin: "http://domain-2.com", shouldAllowOrigin: false},
		{pattern: []string{"http://domain-1.com", "http://example.com"}, reqOrigin: "http://domain-1.com", shouldAllowOrigin: true},
	}

	for _, tt := range tests {
		t.Run(strings.Join(tt.pattern, ", ")+" with Origin "+tt.reqOrigin, func(t *testing.T) {
			t.Parallel()

			app := fiber.New()
			app.Use("/", New(Config{AllowOrigins: tt.pattern}))

			got := corsTestAllowOrigin(corsTestPreflight(app.Handler(), tt.reqOrigin))
			if tt.shouldAllowOrigin {
				require.Equal(t, tt.reqOrigin, got)
			} else {
				require.Empty(t, got)
			}
		})
	}
}

func Test_CORS_AllowOriginHeader_NoMatch(t *testing.T) {
	t.Parallel()
	// New fiber instance
	app := fiber.New()
	app.Use("/", New(Config{AllowOrigins: []string{"http://example-1.com", "https://example-1.com"}}))

	// Get handler pointer
	handler := app.Handler()

	// Make request with disallowed origin
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.SetRequestURI("/")
	ctx.Request.Header.SetMethod(fiber.MethodOptions)
	ctx.Request.Header.Set(fiber.HeaderOrigin, "http://google.com")

	// Perform request
	handler(ctx)

	var headerExists bool
	for key := range ctx.Response.Header.All() {
		if string(key) == fiber.HeaderAccessControlAllowOrigin {
			headerExists = true
		}
	}
	require.False(t, headerExists, "Access-Control-Allow-Origin header should not be set")
}

func Test_CORS_Next(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	app.Use(New(Config{Next: func(_ fiber.Ctx) bool { return true }}))

	apptest.Expect(t, app, "/", fiber.StatusNotFound)
}

func Test_CORS_Headers_BasedOnRequestType(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	app.Use(New())
	app.Use(func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

	methods := []string{
		fiber.MethodGet,
		fiber.MethodPost,
		fiber.MethodPut,
		fiber.MethodDelete,
		fiber.MethodPatch,
		fiber.MethodHead,
		fiber.MethodQuery,
	}

	// Get handler pointer
	handler := app.Handler()

	t.Run("Without origin", func(t *testing.T) {
		t.Parallel()
		// Make request without origin header, and without Access-Control-Request-Method
		for _, method := range methods {
			ctx := &fasthttp.RequestCtx{}
			ctx.Request.Header.SetMethod(method)
			ctx.Request.SetRequestURI("https://example.com/")
			handler(ctx)
			require.Equal(t, 200, ctx.Response.StatusCode(), "Status code should be 200")
			require.Empty(t, string(ctx.Response.Header.Peek(fiber.HeaderAccessControlAllowOrigin)), "Access-Control-Allow-Origin header should not be set")
		}
	})

	t.Run("Preflight request with origin and Access-Control-Request-Method headers", func(t *testing.T) {
		t.Parallel()
		// Make preflight request with origin header and with Access-Control-Request-Method
		for _, method := range methods {
			ctx := &fasthttp.RequestCtx{}
			ctx.Request.Header.SetMethod(fiber.MethodOptions)
			ctx.Request.SetRequestURI("https://example.com/")
			ctx.Request.Header.Set(fiber.HeaderOrigin, "http://example.com")
			ctx.Request.Header.Set(fiber.HeaderAccessControlRequestMethod, method)
			handler(ctx)
			require.Equal(t, 204, ctx.Response.StatusCode(), "Status code should be 204")
			require.Equal(t, "*", string(ctx.Response.Header.Peek(fiber.HeaderAccessControlAllowOrigin)), "Access-Control-Allow-Origin header should be set")
			require.Equal(t, "GET, POST, HEAD, PUT, DELETE, PATCH, QUERY", string(ctx.Response.Header.Peek(fiber.HeaderAccessControlAllowMethods)), "Access-Control-Allow-Methods header should be set (preflight request)")
			require.Empty(t, string(ctx.Response.Header.Peek(fiber.HeaderAccessControlAllowHeaders)), "Access-Control-Allow-Headers header should be set (preflight request)")
		}
	})

	t.Run("Non-preflight request with origin", func(t *testing.T) {
		t.Parallel()
		// Make non-preflight request with origin header and with Access-Control-Request-Method
		for _, method := range methods {
			ctx := &fasthttp.RequestCtx{}
			ctx.Request.Header.SetMethod(method)
			ctx.Request.SetRequestURI("https://example.com/api/action")
			ctx.Request.Header.Set(fiber.HeaderOrigin, "http://example.com")
			handler(ctx)
			require.Equal(t, 200, ctx.Response.StatusCode(), "Status code should be 200")
			require.Equal(t, "*", string(ctx.Response.Header.Peek(fiber.HeaderAccessControlAllowOrigin)), "Access-Control-Allow-Origin header should be set")
			require.Empty(t, string(ctx.Response.Header.Peek(fiber.HeaderAccessControlAllowMethods)), "Access-Control-Allow-Methods header should not be set (non-preflight request)")
			require.Empty(t, string(ctx.Response.Header.Peek(fiber.HeaderAccessControlAllowHeaders)), "Access-Control-Allow-Headers header should not be set (non-preflight request)")
		}
	})

	t.Run("Preflight with Access-Control-Request-Headers", func(t *testing.T) {
		t.Parallel()
		// Make preflight request with origin header and with Access-Control-Request-Method
		for _, method := range methods {
			ctx := &fasthttp.RequestCtx{}
			ctx.Request.Header.SetMethod(fiber.MethodOptions)
			ctx.Request.SetRequestURI("https://example.com/")
			ctx.Request.Header.Set(fiber.HeaderOrigin, "http://example.com")
			ctx.Request.Header.Set(fiber.HeaderAccessControlRequestMethod, method)
			ctx.Request.Header.Set(fiber.HeaderAccessControlRequestHeaders, "X-Custom-Header")
			handler(ctx)
			require.Equal(t, 204, ctx.Response.StatusCode(), "Status code should be 204")
			require.Equal(t, "*", string(ctx.Response.Header.Peek(fiber.HeaderAccessControlAllowOrigin)), "Access-Control-Allow-Origin header should be set")
			require.Equal(t, "GET, POST, HEAD, PUT, DELETE, PATCH, QUERY", string(ctx.Response.Header.Peek(fiber.HeaderAccessControlAllowMethods)), "Access-Control-Allow-Methods header should be set (preflight request)")
			require.Equal(t, "X-Custom-Header", string(ctx.Response.Header.Peek(fiber.HeaderAccessControlAllowHeaders)), "Access-Control-Allow-Headers header should be set (preflight request)")
		}
	})
}

func Test_CORS_AllowOriginsAndAllowOriginsFunc(t *testing.T) {
	t.Parallel()
	// New fiber instance
	app := fiber.New()
	app.Use("/", New(Config{
		AllowOrigins:     []string{"http://example-1.com"},
		AllowOriginsFunc: func(origin string) bool { return strings.Contains(origin, "example-2") },
	}))

	handler := app.Handler()

	tests := []struct {
		name   string
		origin string
		want   string // the Allow-Origin header, none when empty
	}{
		// Allow-Origin header should be "" because http://google.com does not satisfy http://example-1.com or 'strings.Contains(origin, "example-2")'
		{name: "disallowed origin", origin: "http://google.com"},
		{name: "allowed origin", origin: "http://example-1.com", want: "http://example-1.com"},
		{name: "allowed origin by the func", origin: "http://example-2.com", want: "http://example-2.com"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.want, corsTestAllowOrigin(corsTestPreflight(handler, tc.origin)))
		})
	}
}

func Test_CORS_AllowOriginsFunc(t *testing.T) {
	t.Parallel()
	// New fiber instance
	app := fiber.New()
	app.Use("/", New(Config{
		AllowOriginsFunc: func(origin string) bool { return strings.Contains(origin, "example-2") },
	}))

	// Get handler pointer
	handler := app.Handler()

	// Make request with disallowed origin
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.SetRequestURI("/")
	ctx.Request.Header.SetMethod(fiber.MethodOptions)
	ctx.Request.Header.Set(fiber.HeaderOrigin, "http://google.com")

	// Perform request
	handler(ctx)

	// Allow-Origin header should be empty because http://google.com does not satisfy 'strings.Contains(origin, "example-2")'
	// and AllowOrigins has not been set
	require.Empty(t, corsTestAllowOrigin(ctx))

	// Make request with allowed origin
	ctx = corsTestPreflight(handler, "http://example-2.com")

	// Allow-Origin header should be "http://example-2.com"
	require.Equal(t, "http://example-2.com", corsTestAllowOrigin(ctx))
}

func Test_CORS_AllowOriginsFuncRejectsNonSerializedOrigins(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		Name               string
		Origin             string
		Method             string
		SetPreflightMethod bool
		ExpectAllowed      bool
	}{
		{Name: "UserInfoPresent", Origin: "http://user:pass@example.com", Method: fiber.MethodGet},
		{Name: "PathPresent", Origin: "http://example.com/path", Method: fiber.MethodOptions, SetPreflightMethod: true},
		{Name: "QueryPresent", Origin: "http://example.com?query=1", Method: fiber.MethodOptions, SetPreflightMethod: true},
		{Name: "FragmentPresent", Origin: "http://example.com#section", Method: fiber.MethodGet},
		{Name: "WildcardHost", Origin: "http://*.example.com", Method: fiber.MethodGet},
		{Name: "StandaloneWildcard", Origin: "*", Method: fiber.MethodGet},
		{Name: "NullOriginUppercase", Origin: "NULL", Method: fiber.MethodGet},
		{Name: "NullOriginMixedCase", Origin: "Null", Method: fiber.MethodGet},
		{Name: "NullOriginLowercase", Origin: "null", Method: fiber.MethodGet, ExpectAllowed: true},
	}

	for _, tc := range testCases {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()

			app := fiber.New()
			app.Use("/", New(Config{AllowOriginsFunc: func(string) bool { return true }}))
			app.All("/", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

			handler := app.Handler()

			ctx := &fasthttp.RequestCtx{}
			ctx.Request.SetRequestURI("/")
			ctx.Request.Header.SetMethod(tc.Method)
			ctx.Request.Header.Set(fiber.HeaderOrigin, tc.Origin)
			if tc.SetPreflightMethod {
				ctx.Request.Header.Set(fiber.HeaderAccessControlRequestMethod, fiber.MethodGet)
			}

			handler(ctx)

			if tc.ExpectAllowed {
				require.Equal(t, "null", corsTestAllowOrigin(ctx))
			} else {
				require.Empty(t, corsTestAllowOrigin(ctx))
			}

			if tc.Method == fiber.MethodOptions {
				require.Equal(t, fiber.StatusNoContent, ctx.Response.StatusCode())
			} else {
				require.Equal(t, fiber.StatusOK, ctx.Response.StatusCode())
			}
		})
	}
}

func Test_CORS_AllowOriginsAndAllowOriginsFunc_AllUseCases(t *testing.T) {
	t.Parallel()

	allowAll := func(string) bool { return true }
	allowNone := func(string) bool { return false }

	testCases := []struct {
		Name           string
		RequestOrigin  string
		ResponseOrigin string
		Config         Config
	}{
		{Name: "AllowOriginsDefined/AllowOriginsFuncUndefined/OriginAllowed", Config: Config{AllowOrigins: []string{"http://aaa.com"}}, RequestOrigin: "http://aaa.com", ResponseOrigin: "http://aaa.com"},
		{Name: "AllowOriginsDefined/AllowOriginsFuncUndefined/MultipleOrigins/OriginAllowed", Config: Config{AllowOrigins: []string{"http://aaa.com", "http://bbb.com"}}, RequestOrigin: "http://bbb.com", ResponseOrigin: "http://bbb.com"},
		{Name: "AllowOriginsDefined/AllowOriginsFuncUndefined/MultipleOrigins/OriginNotAllowed", Config: Config{AllowOrigins: []string{"http://aaa.com", "http://bbb.com"}}, RequestOrigin: "http://ccc.com", ResponseOrigin: ""},
		{Name: "AllowOriginsDefined/AllowOriginsFuncUndefined/MultipleOrigins/Whitespace/OriginAllowed", Config: Config{AllowOrigins: []string{" http://aaa.com ", " http://bbb.com "}}, RequestOrigin: "http://aaa.com", ResponseOrigin: "http://aaa.com"},
		{Name: "AllowOriginsDefined/AllowOriginsFuncUndefined/OriginNotAllowed", Config: Config{AllowOrigins: []string{"http://aaa.com"}}, RequestOrigin: "http://bbb.com", ResponseOrigin: ""},
		{Name: "AllowOriginsDefined/AllowOriginsFuncReturnsTrue/OriginAllowed", Config: Config{AllowOrigins: []string{"http://aaa.com"}, AllowOriginsFunc: allowAll}, RequestOrigin: "http://aaa.com", ResponseOrigin: "http://aaa.com"},
		{Name: "AllowOriginsDefined/AllowOriginsFuncReturnsTrue/OriginNotAllowed", Config: Config{AllowOrigins: []string{"http://aaa.com"}, AllowOriginsFunc: allowAll}, RequestOrigin: "http://bbb.com", ResponseOrigin: "http://bbb.com"},
		{Name: "AllowOriginsDefined/AllowOriginsFuncReturnsFalse/OriginAllowed", Config: Config{AllowOrigins: []string{"http://aaa.com"}, AllowOriginsFunc: allowNone}, RequestOrigin: "http://aaa.com", ResponseOrigin: "http://aaa.com"},
		{Name: "AllowOriginsDefined/AllowOriginsFuncReturnsFalse/OriginNotAllowed", Config: Config{AllowOrigins: []string{"http://aaa.com"}, AllowOriginsFunc: allowNone}, RequestOrigin: "http://bbb.com", ResponseOrigin: ""},
		{Name: "AllowOriginsEmpty/AllowOriginsFuncUndefined/OriginAllowed", Config: Config{}, RequestOrigin: "http://aaa.com", ResponseOrigin: "*"},
		{Name: "AllowOriginsEmpty/AllowOriginsFuncReturnsTrue/OriginAllowed", Config: Config{AllowOriginsFunc: allowAll}, RequestOrigin: "http://aaa.com", ResponseOrigin: "http://aaa.com"},
		{Name: "AllowOriginsEmpty/AllowOriginsFuncReturnsFalse/OriginNotAllowed", Config: Config{AllowOriginsFunc: allowNone}, RequestOrigin: "http://aaa.com", ResponseOrigin: ""},
	}

	for i := range testCases {
		tc := &testCases[i]
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()

			app := fiber.New()
			app.Use("/", New(tc.Config))

			require.Equal(t, tc.ResponseOrigin, corsTestAllowOrigin(corsTestPreflight(app.Handler(), tc.RequestOrigin)))
		})
	}
}

// The fix for issue #2422
func Test_CORS_AllowCredentials(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		Name                string
		RequestOrigin       string
		ResponseOrigin      string
		ResponseCredentials string
		Config              Config
	}{
		{
			Name:          "AllowOriginsFuncDefined",
			Config:        Config{AllowCredentials: true, AllowOriginsFunc: func(_ string) bool { return true }},
			RequestOrigin: "http://aaa.com",
			// The AllowOriginsFunc config was defined, should use the real origin of the function
			ResponseOrigin:      "http://aaa.com",
			ResponseCredentials: "true",
		},
		{
			Name:           "fiber-ghsa-fmg4-x8pw-hjhg-wildcard-credentials",
			Config:         Config{AllowCredentials: true, AllowOriginsFunc: func(_ string) bool { return true }},
			RequestOrigin:  "*",
			ResponseOrigin: "",
			// Middleware will validate that wildcard won't set credentials to true and reject non-serialized origins
			ResponseCredentials: "",
		},
		{
			Name: "AllowOriginsFuncNotDefined",
			Config: Config{
				// Setting this to true will cause the middleware to panic since default AllowOrigins is "*"
				AllowCredentials: false,
			},
			RequestOrigin: "http://aaa.com",
			// None of the AllowOrigins or AllowOriginsFunc config was defined, should use the default origin of "*"
			// which will cause the CORS error in the client:
			// The value of the 'Access-Control-Allow-Origin' header in the response must not be the wildcard '*'
			// when the request's credentials mode is 'include'.
			ResponseOrigin:      "*",
			ResponseCredentials: "",
		},
		{
			Name:                "AllowOriginsDefined",
			Config:              Config{AllowCredentials: true, AllowOrigins: []string{"http://aaa.com"}},
			RequestOrigin:       "http://aaa.com",
			ResponseOrigin:      "http://aaa.com",
			ResponseCredentials: "true",
		},
		{
			Name:                "AllowOriginsDefined/UnallowedOrigin",
			Config:              Config{AllowCredentials: true, AllowOrigins: []string{"http://aaa.com"}},
			RequestOrigin:       "http://bbb.com",
			ResponseOrigin:      "",
			ResponseCredentials: "",
		},
	}

	for i := range testCases {
		tc := &testCases[i]
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()

			app := fiber.New()
			app.Use("/", New(tc.Config))

			ctx := corsTestPreflight(app.Handler(), tc.RequestOrigin)

			require.Equal(t, tc.ResponseCredentials, string(ctx.Response.Header.Peek(fiber.HeaderAccessControlAllowCredentials)))
			require.Equal(t, tc.ResponseOrigin, corsTestAllowOrigin(ctx))
		})
	}
}

// The Enhancement for issue #2804
func Test_CORS_AllowPrivateNetwork(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// method is the method of the request
		method string
		// requestPrivateNetwork is the Access-Control-Request-Private-Network header
		// the client sends, none when empty
		requestPrivateNetwork string
		want                  string
		config                Config
		// preflight sends the Access-Control-Request-Method header
		preflight bool
	}{
		{
			// The Access-Control-Allow-Private-Network header is set to "true" when AllowPrivateNetwork is enabled
			name:                  "enabled preflight",
			config:                Config{AllowPrivateNetwork: true},
			method:                fiber.MethodOptions,
			preflight:             true,
			requestPrivateNetwork: "true",
			want:                  "true",
		},
		{
			// Non-preflight request should not have Access-Control-Allow-Private-Network header
			name:                  "enabled non-preflight GET",
			config:                Config{AllowPrivateNetwork: true},
			method:                fiber.MethodGet,
			requestPrivateNetwork: "true",
		},
		{
			// Non-preflight OPTIONS request should not have Access-Control-Allow-Private-Network header
			name:                  "enabled non-preflight OPTIONS",
			config:                Config{AllowPrivateNetwork: true},
			method:                fiber.MethodOptions,
			requestPrivateNetwork: "true",
		},
		{
			// The Access-Control-Allow-Private-Network header is not present by default
			name:      "disabled by default",
			method:    fiber.MethodOptions,
			preflight: true,
		},
		{
			// AllowPrivateNetwork is disabled but the client sends the header
			name:                  "disabled and the client sends the header",
			method:                fiber.MethodOptions,
			preflight:             true,
			requestPrivateNetwork: "true",
		},
		{
			// AllowPrivateNetwork is enabled and the client does NOT send the header
			name:      "enabled and the client does not send the header",
			config:    Config{AllowPrivateNetwork: true},
			method:    fiber.MethodOptions,
			preflight: true,
		},
		{
			// AllowPrivateNetwork is enabled and the client sends the header with a false value
			name:                  "enabled and the client sends false",
			config:                Config{AllowPrivateNetwork: true},
			method:                fiber.MethodOptions,
			preflight:             true,
			requestPrivateNetwork: "false",
		},
	}

	for i := range tests {
		tc := &tests[i]
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app := fiber.New()
			app.Use(New(tc.config))

			ctx := &fasthttp.RequestCtx{}
			ctx.Request.Header.SetMethod(tc.method)
			ctx.Request.Header.Set(fiber.HeaderOrigin, "https://example.com")
			if tc.preflight {
				ctx.Request.Header.Set(fiber.HeaderAccessControlRequestMethod, fiber.MethodGet)
			}
			if tc.requestPrivateNetwork != "" {
				ctx.Request.Header.Set("Access-Control-Request-Private-Network", tc.requestPrivateNetwork)
			}
			app.Handler()(ctx)

			require.Equal(t, tc.want, string(ctx.Response.Header.Peek("Access-Control-Allow-Private-Network")))
		})
	}
}

// Test_CORS_Query_RequiresPreflight documents RFC 10008: QUERY is not a
// CORS-safelisted method (the safelisted set is GET/HEAD/POST per the Fetch
// spec), so a CORS-aware user agent must preflight it. Server-side that means:
//   - a QUERY preflight must be answered with QUERY in Access-Control-Allow-Methods, and
//   - an actual QUERY request must be handled as a simple/non-preflight response
//     (Allow-Origin set, no Allow-Methods) — i.e. QUERY is gated by preflight, not
//     treated as server-side safelisted.
func Test_CORS_Query_RequiresPreflight(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	app.Use(New()) // default config: QUERY is in AllowMethods
	app.Use(func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })
	handler := app.Handler()

	t.Run("QUERY preflight is answered with QUERY allowed", func(t *testing.T) {
		t.Parallel()
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.Header.SetMethod(fiber.MethodOptions)
		ctx.Request.SetRequestURI("https://example.com/")
		ctx.Request.Header.Set(fiber.HeaderOrigin, "http://example.com")
		ctx.Request.Header.Set(fiber.HeaderAccessControlRequestMethod, fiber.MethodQuery)
		handler(ctx)

		require.Equal(t, fiber.StatusNoContent, ctx.Response.StatusCode(), "preflight should return 204")
		require.Equal(t, "*", string(ctx.Response.Header.Peek(fiber.HeaderAccessControlAllowOrigin)))
		require.Contains(t, string(ctx.Response.Header.Peek(fiber.HeaderAccessControlAllowMethods)), fiber.MethodQuery,
			"preflight must advertise QUERY so the user agent allows the request")
	})

	t.Run("actual QUERY request is a simple response, not safelisted", func(t *testing.T) {
		t.Parallel()
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.Header.SetMethod(fiber.MethodQuery)
		ctx.Request.SetRequestURI("https://example.com/")
		ctx.Request.Header.Set(fiber.HeaderOrigin, "http://example.com")
		handler(ctx)

		require.Equal(t, fiber.StatusOK, ctx.Response.StatusCode())
		require.Equal(t, "*", string(ctx.Response.Header.Peek(fiber.HeaderAccessControlAllowOrigin)))
		require.Empty(t, string(ctx.Response.Header.Peek(fiber.HeaderAccessControlAllowMethods)),
			"actual (non-preflight) QUERY must not carry Access-Control-Allow-Methods")
	})
}

func Benchmark_CORS_NewHandler(b *testing.B) {
	app := fiber.New()
	c := New(Config{
		AllowOrigins:     []string{"http://localhost", "http://example.com"},
		AllowMethods:     []string{fiber.MethodGet, fiber.MethodPost, fiber.MethodPut, fiber.MethodDelete},
		AllowHeaders:     []string{fiber.HeaderOrigin, fiber.HeaderContentType, fiber.HeaderAccept},
		AllowCredentials: true,
		MaxAge:           600,
	})

	app.Use(c)
	app.Use(func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

	h := app.Handler()
	ctx := &fasthttp.RequestCtx{}

	req := &fasthttp.Request{}
	req.Header.SetMethod(fiber.MethodGet)
	req.SetRequestURI("/")
	req.Header.Set(fiber.HeaderOrigin, "http://localhost")
	req.Header.Set(fiber.HeaderAccessControlRequestHeaders, "Origin,Content-Type,Accept")

	ctx.Init(req, nil, nil)

	b.ReportAllocs()

	for b.Loop() {
		h(ctx)
	}
}

func Benchmark_CORS_NewHandler_Parallel(b *testing.B) {
	app := fiber.New()
	c := New(Config{
		AllowOrigins:     []string{"http://localhost", "http://example.com"},
		AllowMethods:     []string{fiber.MethodGet, fiber.MethodPost, fiber.MethodPut, fiber.MethodDelete},
		AllowHeaders:     []string{fiber.HeaderOrigin, fiber.HeaderContentType, fiber.HeaderAccept},
		AllowCredentials: true,
		MaxAge:           600,
	})

	app.Use(c)
	app.Use(func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

	h := app.Handler()

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		ctx := &fasthttp.RequestCtx{}

		req := &fasthttp.Request{}
		req.Header.SetMethod(fiber.MethodGet)
		req.SetRequestURI("/")
		req.Header.Set(fiber.HeaderOrigin, "http://localhost")
		req.Header.Set(fiber.HeaderAccessControlRequestHeaders, "Origin,Content-Type,Accept")

		ctx.Init(req, nil, nil)

		for pb.Next() {
			h(ctx)
		}
	})
}

func Benchmark_CORS_NewHandlerSingleOrigin(b *testing.B) {
	app := fiber.New()
	c := New(Config{
		AllowOrigins:     []string{"http://example.com"},
		AllowMethods:     []string{fiber.MethodGet, fiber.MethodPost, fiber.MethodPut, fiber.MethodDelete},
		AllowHeaders:     []string{fiber.HeaderOrigin, fiber.HeaderContentType, fiber.HeaderAccept},
		AllowCredentials: true,
		MaxAge:           600,
	})

	app.Use(c)
	app.Use(func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

	h := app.Handler()
	ctx := &fasthttp.RequestCtx{}

	req := &fasthttp.Request{}
	req.Header.SetMethod(fiber.MethodGet)
	req.SetRequestURI("/")
	req.Header.Set(fiber.HeaderOrigin, "http://example.com")
	req.Header.Set(fiber.HeaderAccessControlRequestHeaders, "Origin,Content-Type,Accept")

	ctx.Init(req, nil, nil)

	b.ReportAllocs()

	for b.Loop() {
		h(ctx)
	}
}

func Benchmark_CORS_NewHandlerSingleOrigin_Parallel(b *testing.B) {
	app := fiber.New()
	c := New(Config{
		AllowOrigins:     []string{"http://example.com"},
		AllowMethods:     []string{fiber.MethodGet, fiber.MethodPost, fiber.MethodPut, fiber.MethodDelete},
		AllowHeaders:     []string{fiber.HeaderOrigin, fiber.HeaderContentType, fiber.HeaderAccept},
		AllowCredentials: true,
		MaxAge:           600,
	})

	app.Use(c)
	app.Use(func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

	h := app.Handler()

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		ctx := &fasthttp.RequestCtx{}

		req := &fasthttp.Request{}
		req.Header.SetMethod(fiber.MethodGet)
		req.SetRequestURI("/")
		req.Header.Set(fiber.HeaderOrigin, "http://example.com")
		req.Header.Set(fiber.HeaderAccessControlRequestHeaders, "Origin,Content-Type,Accept")

		ctx.Init(req, nil, nil)

		for pb.Next() {
			h(ctx)
		}
	})
}

func Benchmark_CORS_NewHandlerWildcard(b *testing.B) {
	app := fiber.New()
	c := New(Config{
		AllowMethods:     []string{fiber.MethodGet, fiber.MethodPost, fiber.MethodPut, fiber.MethodDelete},
		AllowHeaders:     []string{fiber.HeaderOrigin, fiber.HeaderContentType, fiber.HeaderAccept},
		AllowCredentials: false,
		MaxAge:           600,
	})

	app.Use(c)
	app.Use(func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

	h := app.Handler()
	ctx := &fasthttp.RequestCtx{}

	req := &fasthttp.Request{}
	req.Header.SetMethod(fiber.MethodGet)
	req.SetRequestURI("/")
	req.Header.Set(fiber.HeaderOrigin, "http://example.com")
	req.Header.Set(fiber.HeaderAccessControlRequestHeaders, "Origin,Content-Type,Accept")

	ctx.Init(req, nil, nil)

	b.ReportAllocs()

	for b.Loop() {
		h(ctx)
	}
}

func Benchmark_CORS_NewHandlerWildcard_Parallel(b *testing.B) {
	app := fiber.New()
	c := New(Config{
		AllowMethods:     []string{fiber.MethodGet, fiber.MethodPost, fiber.MethodPut, fiber.MethodDelete},
		AllowHeaders:     []string{fiber.HeaderOrigin, fiber.HeaderContentType, fiber.HeaderAccept},
		AllowCredentials: false,
		MaxAge:           600,
	})

	app.Use(c)
	app.Use(func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

	h := app.Handler()

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		ctx := &fasthttp.RequestCtx{}

		req := &fasthttp.Request{}
		req.Header.SetMethod(fiber.MethodGet)
		req.SetRequestURI("/")
		req.Header.Set(fiber.HeaderOrigin, "http://example.com")
		req.Header.Set(fiber.HeaderAccessControlRequestHeaders, "Origin,Content-Type,Accept")

		ctx.Init(req, nil, nil)

		for pb.Next() {
			h(ctx)
		}
	})
}

func Benchmark_CORS_NewHandlerPreflight(b *testing.B) {
	app := fiber.New()
	c := New(Config{
		AllowOrigins:     []string{"http://localhost", "http://example.com"},
		AllowMethods:     []string{fiber.MethodGet, fiber.MethodPost, fiber.MethodPut, fiber.MethodDelete},
		AllowHeaders:     []string{fiber.HeaderOrigin, fiber.HeaderContentType, fiber.HeaderAccept},
		AllowCredentials: true,
		MaxAge:           600,
	})

	app.Use(c)
	app.Use(func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

	h := app.Handler()
	ctx := &fasthttp.RequestCtx{}

	// Preflight request
	req := &fasthttp.Request{}
	req.Header.SetMethod(fiber.MethodOptions)
	req.SetRequestURI("/")
	req.Header.Set(fiber.HeaderOrigin, "http://example.com")
	req.Header.Set(fiber.HeaderAccessControlRequestMethod, fiber.MethodPost)
	req.Header.Set(fiber.HeaderAccessControlRequestHeaders, "Origin,Content-Type,Accept")

	ctx.Init(req, nil, nil)

	b.ReportAllocs()

	for b.Loop() {
		h(ctx)
	}
}

func Benchmark_CORS_NewHandlerPreflight_Parallel(b *testing.B) {
	app := fiber.New()
	c := New(Config{
		AllowOrigins:     []string{"http://localhost", "http://example.com"},
		AllowMethods:     []string{fiber.MethodGet, fiber.MethodPost, fiber.MethodPut, fiber.MethodDelete},
		AllowHeaders:     []string{fiber.HeaderOrigin, fiber.HeaderContentType, fiber.HeaderAccept},
		AllowCredentials: true,
		MaxAge:           600,
	})

	app.Use(c)
	app.Use(func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

	h := app.Handler()

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		ctx := &fasthttp.RequestCtx{}

		req := &fasthttp.Request{}
		req.Header.SetMethod(fiber.MethodOptions)
		req.SetRequestURI("/")
		req.Header.Set(fiber.HeaderOrigin, "http://example.com")
		req.Header.Set(fiber.HeaderAccessControlRequestMethod, fiber.MethodPost)
		req.Header.Set(fiber.HeaderAccessControlRequestHeaders, "Origin,Content-Type,Accept")

		ctx.Init(req, nil, nil)

		for pb.Next() {
			h(ctx)
		}
	})
}

func Benchmark_CORS_NewHandlerPreflightSingleOrigin(b *testing.B) {
	app := fiber.New()
	c := New(Config{
		AllowOrigins:     []string{"http://example.com"},
		AllowMethods:     []string{fiber.MethodGet, fiber.MethodPost, fiber.MethodPut, fiber.MethodDelete},
		AllowHeaders:     []string{fiber.HeaderOrigin, fiber.HeaderContentType, fiber.HeaderAccept},
		AllowCredentials: true,
		MaxAge:           600,
	})

	app.Use(c)
	app.Use(func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

	h := app.Handler()
	ctx := &fasthttp.RequestCtx{}

	req := &fasthttp.Request{}
	req.Header.SetMethod(fiber.MethodOptions)
	req.SetRequestURI("/")
	req.Header.Set(fiber.HeaderOrigin, "http://example.com")
	req.Header.Set(fiber.HeaderAccessControlRequestMethod, fiber.MethodPost)
	req.Header.Set(fiber.HeaderAccessControlRequestHeaders, "Origin,Content-Type,Accept")

	ctx.Init(req, nil, nil)

	b.ReportAllocs()

	for b.Loop() {
		h(ctx)
	}
}

func Benchmark_CORS_NewHandlerPreflightSingleOrigin_Parallel(b *testing.B) {
	app := fiber.New()
	c := New(Config{
		AllowOrigins:     []string{"http://example.com"},
		AllowMethods:     []string{fiber.MethodGet, fiber.MethodPost, fiber.MethodPut, fiber.MethodDelete},
		AllowHeaders:     []string{fiber.HeaderOrigin, fiber.HeaderContentType, fiber.HeaderAccept},
		AllowCredentials: true,
		MaxAge:           600,
	})

	app.Use(c)
	app.Use(func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

	h := app.Handler()

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		ctx := &fasthttp.RequestCtx{}

		req := &fasthttp.Request{}
		req.Header.SetMethod(fiber.MethodOptions)
		req.SetRequestURI("/")
		req.Header.Set(fiber.HeaderOrigin, "http://example.com")
		req.Header.Set(fiber.HeaderAccessControlRequestMethod, fiber.MethodPost)
		req.Header.Set(fiber.HeaderAccessControlRequestHeaders, "Origin,Content-Type,Accept")

		ctx.Init(req, nil, nil)

		for pb.Next() {
			h(ctx)
		}
	})
}

func Benchmark_CORS_NewHandlerPreflightWildcard(b *testing.B) {
	app := fiber.New()
	c := New(Config{
		AllowMethods:     []string{fiber.MethodGet, fiber.MethodPost, fiber.MethodPut, fiber.MethodDelete},
		AllowHeaders:     []string{fiber.HeaderOrigin, fiber.HeaderContentType, fiber.HeaderAccept},
		AllowCredentials: false,
		MaxAge:           600,
	})

	app.Use(c)
	app.Use(func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

	h := app.Handler()
	ctx := &fasthttp.RequestCtx{}

	req := &fasthttp.Request{}
	req.Header.SetMethod(fiber.MethodOptions)
	req.SetRequestURI("/")
	req.Header.Set(fiber.HeaderOrigin, "http://example.com")
	req.Header.Set(fiber.HeaderAccessControlRequestMethod, fiber.MethodPost)
	req.Header.Set(fiber.HeaderAccessControlRequestHeaders, "Origin,Content-Type,Accept")

	ctx.Init(req, nil, nil)

	b.ReportAllocs()

	for b.Loop() {
		h(ctx)
	}
}

func Benchmark_CORS_NewHandlerPreflightWildcard_Parallel(b *testing.B) {
	app := fiber.New()
	c := New(Config{
		AllowMethods:     []string{fiber.MethodGet, fiber.MethodPost, fiber.MethodPut, fiber.MethodDelete},
		AllowHeaders:     []string{fiber.HeaderOrigin, fiber.HeaderContentType, fiber.HeaderAccept},
		AllowCredentials: false,
		MaxAge:           600,
	})

	app.Use(c)
	app.Use(func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

	h := app.Handler()

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		ctx := &fasthttp.RequestCtx{}

		req := &fasthttp.Request{}
		req.Header.SetMethod(fiber.MethodOptions)
		req.SetRequestURI("/")
		req.Header.Set(fiber.HeaderOrigin, "http://example.com")
		req.Header.Set(fiber.HeaderAccessControlRequestMethod, fiber.MethodPost)
		req.Header.Set(fiber.HeaderAccessControlRequestHeaders, "Origin,Content-Type,Accept")

		ctx.Init(req, nil, nil)

		for pb.Next() {
			h(ctx)
		}
	})
}

// Test_CORS_setSimpleHeaders_NilConfig ensures the nil-config guard returns
// without panicking or mutating the response headers.
func Test_CORS_setSimpleHeaders_NilConfig(t *testing.T) {
	t.Parallel()

	c := apptest.Ctx(t, fiber.New())

	require.NotPanics(t, func() { setSimpleHeaders(c, "https://example.com", nil, nil) })
	require.Empty(t, string(c.Response().Header.Peek(fiber.HeaderAccessControlAllowOrigin)))
}

// Test_CORS_setSimpleHeaders_WildcardWithCredentials verifies that when
// AllowCredentials is true and the resolved origin is "*", the middleware logs
// a warning and still reflects the value (the configuration is considered
// invalid, but the header is set to surface the misconfiguration).
func Test_CORS_setSimpleHeaders_WildcardWithCredentials(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	c := apptest.Ctx(t, fiber.New())

	setSimpleHeaders(c, "*", &Config{AllowCredentials: true}, nil)

	require.Equal(t, "*", string(c.Response().Header.Peek(fiber.HeaderAccessControlAllowOrigin)))
	require.Empty(t, string(c.Response().Header.Peek(fiber.HeaderAccessControlAllowCredentials)))
	require.Contains(t, buf.String(), "'AllowCredentials' is true, but 'AllowOrigins' cannot be set to '*'")
}

// Test_CORS_setPreflightHeaders_NilConfig ensures the preflight helper tolerates
// a nil config (delegating to setSimpleHeaders and skipping MaxAge handling).
func Test_CORS_setPreflightHeaders_NilConfig(t *testing.T) {
	t.Parallel()

	c := apptest.Ctx(t, fiber.New())

	require.NotPanics(t, func() { setPreflightHeaders(c, "https://example.com", "600", nil, nil) })
	require.Empty(t, string(c.Response().Header.Peek(fiber.HeaderAccessControlMaxAge)))
}

// Test_CORS_Security_CredentialsWithSubdomainWildcard ensures that when
// credentials are allowed together with a wildcard-subdomain origin pattern, a
// matching origin is reflected verbatim (never "*") alongside the credentials
// header, while a non-matching origin receives neither.
func Test_CORS_Security_CredentialsWithSubdomainWildcard(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(New(Config{AllowCredentials: true, AllowOrigins: []string{"https://*.example.com"}}))

	handler := app.Handler()

	cases := []struct {
		name          string
		origin        string
		expectOrigin  string
		expectCredHdr string
	}{
		{name: "matching subdomain reflected with credentials", origin: "https://api.example.com", expectOrigin: "https://api.example.com", expectCredHdr: "true"},
		{name: "non-matching origin gets nothing", origin: "https://attacker.evil.com"},
		{name: "apex domain does not match wildcard", origin: "https://example.com"},
		{name: "scheme downgrade does not match", origin: "http://api.example.com"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := corsTestPreflight(handler, tc.origin)

			gotOrigin := corsTestAllowOrigin(ctx)
			gotCred := string(ctx.Response.Header.Peek(fiber.HeaderAccessControlAllowCredentials))
			require.Equal(t, tc.expectOrigin, gotOrigin)
			require.Equal(t, tc.expectCredHdr, gotCred)
			require.NotEqual(t, "*", gotOrigin, "credentialed responses must never use the wildcard")
		})
	}
}

// Test_CORS_Security_CredentialsSimpleRequest ensures the credentialed-origin
// reflection also applies to non-preflight (simple) requests, not just OPTIONS
// preflights.
func Test_CORS_Security_CredentialsSimpleRequest(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(New(Config{AllowCredentials: true, AllowOrigins: []string{"https://trusted.example.com"}}))
	app.Get("/", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

	// Allowed origin is reflected with credentials on a simple GET.
	allowed := httptest.NewRequest(fiber.MethodGet, "/", http.NoBody)
	allowed.Header.Set(fiber.HeaderOrigin, "https://trusted.example.com")
	resp := apptest.Do(t, app, allowed)
	require.Equal(t, "https://trusted.example.com", resp.Header.Get(fiber.HeaderAccessControlAllowOrigin))
	require.Equal(t, "true", resp.Header.Get(fiber.HeaderAccessControlAllowCredentials))

	// Disallowed origin is reflected nowhere.
	denied := httptest.NewRequest(fiber.MethodGet, "/", http.NoBody)
	denied.Header.Set(fiber.HeaderOrigin, "https://evil.example.org")
	resp = apptest.Do(t, app, denied)
	require.Empty(t, resp.Header.Get(fiber.HeaderAccessControlAllowOrigin))
	require.Empty(t, resp.Header.Get(fiber.HeaderAccessControlAllowCredentials))
}

// Test_CORS_Security_NoOriginReflectionForDisallowed ensures a disallowed origin
// is never reflected back even when many origins are configured — the response
// must not echo an arbitrary attacker-controlled Origin.
func Test_CORS_Security_NoOriginReflectionForDisallowed(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(New(Config{AllowOrigins: []string{"https://a.example.com", "https://b.example.com"}}))
	app.Get("/", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

	req := httptest.NewRequest(fiber.MethodGet, "/", http.NoBody)
	req.Header.Set(fiber.HeaderOrigin, "https://attacker.example.net")
	resp := apptest.Do(t, app, req)

	got := resp.Header.Get(fiber.HeaderAccessControlAllowOrigin)
	require.Empty(t, got)
	require.NotEqual(t, "https://attacker.example.net", got)
}

func Test_CORS_OriginWithoutHeaderNormalizing(t *testing.T) {
	t.Parallel()

	app := fiber.New(fiber.Config{DisableHeaderNormalizing: true})
	app.Use(New(Config{AllowOrigins: []string{"http://example.com"}}))
	app.Get("/", func(c fiber.Ctx) error { return c.SendString("ok") })

	do := func(header string) *fasthttp.RequestCtx {
		return corsTestRaw(t, app, "GET / HTTP/1.1\r\nHost: example.com\r\n"+header+"\r\n\r\n")
	}

	lower := do("origin: http://example.com")
	require.Equal(t, "http://example.com", corsTestAllowOrigin(lower))

	canonical := do("Origin: http://example.com")
	require.Equal(t, "http://example.com", corsTestAllowOrigin(canonical))

	other := do("origin: http://evil.example")
	require.Empty(t, corsTestAllowOrigin(other))
}

func Test_CORS_PreflightWithoutHeaderNormalizing(t *testing.T) {
	t.Parallel()

	app := fiber.New(fiber.Config{DisableHeaderNormalizing: true})
	app.Use(New(Config{
		AllowOrigins: []string{"http://example.com"},
		AllowMethods: []string{fiber.MethodGet, fiber.MethodPut},
	}))
	app.Put("/", func(c fiber.Ctx) error { return c.SendString("ok") })

	raw := "OPTIONS / HTTP/1.1\r\nHost: example.com\r\n" +
		"origin: http://example.com\r\n" +
		"access-control-request-method: PUT\r\n" +
		"access-control-request-headers: X-Custom\r\n\r\n"

	fctx := corsTestRaw(t, app, raw)

	require.Equal(t, fiber.StatusNoContent, fctx.Response.StatusCode())
	require.Equal(t, "http://example.com", corsTestAllowOrigin(fctx))
	require.Contains(t, string(fctx.Response.Header.Peek(fiber.HeaderAccessControlAllowMethods)), fiber.MethodPut)
	require.Equal(t, "X-Custom", string(fctx.Response.Header.Peek(fiber.HeaderAccessControlAllowHeaders)))
}

func Test_CORS_PreflightSplitRequestHeaders(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(New(Config{
		AllowOrigins: []string{"http://example.com"},
		AllowMethods: []string{fiber.MethodGet, fiber.MethodPut},
	}))
	app.Put("/", func(c fiber.Ctx) error { return c.SendString("ok") })

	raw := "OPTIONS / HTTP/1.1\r\nHost: example.com\r\n" +
		"Origin: http://example.com\r\n" +
		"Access-Control-Request-Method: PUT\r\n" +
		"Access-Control-Request-Headers: X-One\r\n" +
		"Access-Control-Request-Headers: X-Two\r\n\r\n"

	fctx := corsTestRaw(t, app, raw)

	require.Equal(t, fiber.StatusNoContent, fctx.Response.StatusCode())
	require.Equal(t, "X-One, X-Two", string(fctx.Response.Header.Peek(fiber.HeaderAccessControlAllowHeaders)))
}

// Test_CORS_ConfiguredEmptyAllowHeaders pins the difference between an
// unconfigured AllowHeaders and one configured with nothing usable in it. An
// absent list means "echo whatever the request asked for"; a configured list
// that happens to join to the empty string must not be mistaken for one, or a
// malformed or environment-derived entry would silently authorize every header
// the request names.
func Test_CORS_ConfiguredEmptyAllowHeaders(t *testing.T) {
	t.Parallel()

	preflight := func(t *testing.T, cfg Config) string {
		t.Helper()

		app := fiber.New()
		app.Use(New(cfg))

		ctx := &fasthttp.RequestCtx{}
		ctx.Request.SetRequestURI("/")
		ctx.Request.Header.SetMethod(fiber.MethodOptions)
		ctx.Request.Header.Set(fiber.HeaderOrigin, "http://localhost")
		ctx.Request.Header.Set(fiber.HeaderAccessControlRequestMethod, fiber.MethodGet)
		ctx.Request.Header.Set(fiber.HeaderAccessControlRequestHeaders, "X-Requested-Header")
		app.Handler()(ctx)

		return string(ctx.Response.Header.Peek(fiber.HeaderAccessControlAllowHeaders))
	}

	t.Run("unconfigured echoes the request", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, "X-Requested-Header", preflight(t, Config{}))
	})

	t.Run("configured empty authorizes nothing", func(t *testing.T) {
		t.Parallel()
		require.Empty(t, preflight(t, Config{AllowHeaders: []string{""}}),
			"a configured list must not fall back to the requested headers")
	})
}

// varyMutatingCtx is a custom context whose Vary rewrites the field list it is
// handed. Nothing in Ctx's contract forbids that, so the middleware must not
// hand it a slice that outlives the request.
type varyMutatingCtx struct {
	fiber.DefaultCtx
}

// Vary overwrites every field it is handed before forwarding the call — the
// most a custom Vary can do to a slice its caller still holds.
func (c *varyMutatingCtx) Vary(fields ...string) {
	for i := range fields {
		fields[i] = "X-Mutated"
	}
	c.DefaultCtx.Vary(fields...)
}

// Test_CORS_VaryDoesNotShareFieldsWithCustomCtx pins that the package-level
// field lists survive a custom context. They are shared by every request the
// middleware serves, so a Vary implementation that writes to its argument
// would otherwise corrupt the field names of every later response and race
// with the requests running alongside it.
func Test_CORS_VaryDoesNotShareFieldsWithCustomCtx(t *testing.T) {
	t.Parallel()

	app := fiber.NewWithCustomCtx(func(app *fiber.App) fiber.CustomCtx {
		return &varyMutatingCtx{DefaultCtx: *fiber.NewDefaultCtx(app)}
	})
	app.Use(New(Config{AllowPrivateNetwork: true}))
	handler := app.Handler()

	// One request down each branch that emits a Vary header.
	simple := &fasthttp.RequestCtx{}
	simple.Request.SetRequestURI("/")
	simple.Request.Header.SetMethod(fiber.MethodGet)
	simple.Request.Header.Set(fiber.HeaderOrigin, "http://localhost")
	handler(simple)

	preflight := &fasthttp.RequestCtx{}
	preflight.Request.SetRequestURI("/")
	preflight.Request.Header.SetMethod(fiber.MethodOptions)
	preflight.Request.Header.Set(fiber.HeaderOrigin, "http://localhost")
	preflight.Request.Header.Set(fiber.HeaderAccessControlRequestMethod, fiber.MethodGet)
	handler(preflight)

	private := &fasthttp.RequestCtx{}
	private.Request.SetRequestURI("/")
	private.Request.Header.SetMethod(fiber.MethodOptions)
	private.Request.Header.Set(fiber.HeaderOrigin, "http://localhost")
	private.Request.Header.Set(fiber.HeaderAccessControlRequestMethod, fiber.MethodGet)
	private.Request.Header.Set(fiber.HeaderAccessControlRequestPrivateNetwork, "true")
	handler(private)

	require.Equal(t, []string{fiber.HeaderOrigin}, varyOrigin)
	require.Equal(t, []string{
		fiber.HeaderAccessControlRequestMethod,
		fiber.HeaderAccessControlRequestHeaders,
		fiber.HeaderOrigin,
	}, varyPreflight)
	require.Equal(t, []string{
		fiber.HeaderAccessControlRequestMethod,
		fiber.HeaderAccessControlRequestHeaders,
		fiber.HeaderAccessControlRequestPrivateNetwork,
		fiber.HeaderOrigin,
	}, varyPreflightPrivate)
}

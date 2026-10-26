package healthcheck

import (
	"strconv"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/internal/apptest"
	"github.com/shamaton/msgpack/v3"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func shouldGiveStatus(t *testing.T, app *fiber.App, path string, expectedStatus int) {
	t.Helper()
	req := apptest.Do(t, app, path)
	require.Equal(t, expectedStatus, req.StatusCode, "path: "+path+" should match "+strconv.Itoa(expectedStatus))
}

func shouldGiveOK(t *testing.T, app *fiber.App, path string) {
	t.Helper()
	shouldGiveStatus(t, app, path, fiber.StatusOK)
}

func shouldGiveNotFound(t *testing.T, app *fiber.App, path string) {
	t.Helper()
	shouldGiveStatus(t, app, path, fiber.StatusNotFound)
}

// Test_HealthCheck_Default serves the default endpoints with and without
// StrictRouting: only the answer for a trailing slash differs.
func Test_HealthCheck_Default(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		trailingSlash int
		strict        bool
	}{
		{name: "strict routing", strict: true, trailingSlash: fiber.StatusNotFound},
		{name: "default routing", strict: false, trailingSlash: fiber.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app := fiber.New(fiber.Config{StrictRouting: tc.strict})
			app.Get(LivenessEndpoint, New())
			app.Get(ReadinessEndpoint, New())
			app.Get(StartupEndpoint, New())

			for _, path := range []string{"/readyz", "/livez", "/startupz"} {
				shouldGiveOK(t, app, path)
				shouldGiveStatus(t, app, path+"/", tc.trailingSlash)
				shouldGiveNotFound(t, app, "/notDefined"+path)
			}
		})
	}
}

func Test_HealthCheck_Custom(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	c1 := make(chan struct{}, 1)
	app.Get("/live", New(Config{
		Probe: func(_ fiber.Ctx) bool {
			return true
		},
	}))
	app.Get("/ready", New(Config{
		Probe: func(_ fiber.Ctx) bool {
			select {
			case <-c1:
				return true
			default:
				return false
			}
		},
	}))
	app.Get(StartupEndpoint, New(Config{
		Probe: func(_ fiber.Ctx) bool {
			return false
		},
	}))

	// Setup custom liveness and readiness probes to simulate application health status
	// Live should return 200 with GET request
	shouldGiveOK(t, app, "/live")
	// Live should return 404 with POST request
	apptest.Expect(t, app, apptest.Req(fiber.MethodPost, "/live"), fiber.StatusMethodNotAllowed)

	// Ready should return 404 with POST request
	apptest.Expect(t, app, apptest.Req(fiber.MethodPost, "/ready"), fiber.StatusMethodNotAllowed)

	// Ready should return 503 with GET request before the channel is closed
	shouldGiveStatus(t, app, "/ready", fiber.StatusServiceUnavailable)

	shouldGiveStatus(t, app, "/startupz", fiber.StatusServiceUnavailable)

	// Ready should return 200 with GET request after the channel is closed
	c1 <- struct{}{}
	shouldGiveOK(t, app, "/ready")
}

func Test_HealthCheck_Custom_Nested(t *testing.T) {
	t.Parallel()

	app := fiber.New()

	c1 := make(chan struct{}, 1)
	app.Get("/probe/live", New(Config{
		Probe: func(_ fiber.Ctx) bool {
			return true
		},
	}))
	app.Get("/probe/ready", New(Config{
		Probe: func(_ fiber.Ctx) bool {
			select {
			case <-c1:
				return true
			default:
				return false
			}
		},
	}))

	// Testing custom health check endpoints with nested paths
	shouldGiveOK(t, app, "/probe/live")
	shouldGiveStatus(t, app, "/probe/ready", fiber.StatusServiceUnavailable)
	shouldGiveOK(t, app, "/probe/live/")
	shouldGiveStatus(t, app, "/probe/ready/", fiber.StatusServiceUnavailable)
	shouldGiveNotFound(t, app, "/probe/livez")
	shouldGiveNotFound(t, app, "/probe/readyz")
	shouldGiveNotFound(t, app, "/probe/livez/")
	shouldGiveNotFound(t, app, "/probe/readyz/")
	shouldGiveNotFound(t, app, "/livez")
	shouldGiveNotFound(t, app, "/readyz")
	shouldGiveNotFound(t, app, "/readyz/")
	shouldGiveNotFound(t, app, "/livez/")

	c1 <- struct{}{}
	shouldGiveOK(t, app, "/probe/ready")
	c1 <- struct{}{}
	shouldGiveOK(t, app, "/probe/ready/")
}

func Test_HealthCheck_Head(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Get(LivenessEndpoint, New())
	app.Get(ReadinessEndpoint, New(Config{
		Probe: func(_ fiber.Ctx) bool {
			return false
		},
	}))
	app.All("/healthz", New())

	// HEAD must mirror the GET status (RFC 9110 9.3.2) with an empty body
	for _, tc := range []struct {
		path   string
		status int
	}{
		{path: LivenessEndpoint, status: fiber.StatusOK},
		{path: ReadinessEndpoint, status: fiber.StatusServiceUnavailable},
	} {
		getResp := apptest.Do(t, app, tc.path)
		require.NoError(t, getResp.Body.Close())
		require.Equal(t, tc.status, getResp.StatusCode, "path: "+tc.path)

		resp := apptest.Do(t, app, apptest.Req(fiber.MethodHead, tc.path))
		require.Equal(t, getResp.StatusCode, resp.StatusCode, "path: "+tc.path)
		// Date is left out, fasthttp refreshes its cached value once per second
		for _, name := range []string{fiber.HeaderContentType, fiber.HeaderContentLength} {
			require.Equal(t, getResp.Header.Get(name), resp.Header.Get(name), "path: "+tc.path+" header: "+name)
		}

		body := apptest.BodyBytes(t, resp)
		require.NoError(t, resp.Body.Close())
		require.Empty(t, body)
	}

	// Everything but GET and HEAD keeps falling through to the next handler
	resp := apptest.Do(t, app, apptest.Req(fiber.MethodPost, "/healthz"))
	require.NoError(t, resp.Body.Close())
	require.Equal(t, fiber.StatusNotFound, resp.StatusCode)
}

func Test_HealthCheck_Next(t *testing.T) {
	t.Parallel()

	app := fiber.New()

	checker := New(Config{
		Next: func(_ fiber.Ctx) bool {
			return true
		},
	})

	app.Get(LivenessEndpoint, checker)
	app.Get(ReadinessEndpoint, checker)
	app.Get(StartupEndpoint, checker)

	// This should give not found since there are no other handlers to execute
	// so it's like the route isn't defined at all
	shouldGiveNotFound(t, app, "/readyz")
	shouldGiveNotFound(t, app, "/livez")
	shouldGiveNotFound(t, app, "/startupz")
}

func Benchmark_HealthCheck(b *testing.B) {
	app := fiber.New()

	app.Get(LivenessEndpoint, New())
	app.Get(ReadinessEndpoint, New())
	app.Get(StartupEndpoint, New())

	h := app.Handler()
	fctx := &fasthttp.RequestCtx{}
	fctx.Request.Header.SetMethod(fiber.MethodGet)
	fctx.Request.SetRequestURI("/livez")

	b.ReportAllocs()

	for b.Loop() {
		h(fctx)
	}

	require.Equal(b, fiber.StatusOK, fctx.Response.Header.StatusCode())
}

func Benchmark_HealthCheck_Parallel(b *testing.B) {
	app := fiber.New()

	app.Get(LivenessEndpoint, New())
	app.Get(ReadinessEndpoint, New())
	app.Get(StartupEndpoint, New())

	h := app.Handler()

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		fctx := &fasthttp.RequestCtx{}
		fctx.Request.Header.SetMethod(fiber.MethodGet)
		fctx.Request.SetRequestURI("/livez")

		for pb.Next() {
			h(fctx)
		}
	})
}

func Test_HealthCheck_Text_Format(t *testing.T) {
	t.Parallel()

	app := fiber.New()

	// Test default format (text)
	app.Get(LivenessEndpoint, New())
	app.Get(ReadinessEndpoint, New(Config{
		ResponseFormat: FormatText,
		Probe: func(_ fiber.Ctx) bool {
			return false
		},
	}))

	// Test successful healthcheck with default text format
	resp := apptest.ExpectBody(t, app, LivenessEndpoint, fiber.StatusOK, "OK")
	require.Equal(t, "text/plain; charset=utf-8", resp.Header.Get("Content-Type"))

	// Test failed healthcheck with text format
	resp = apptest.ExpectBody(t, app, ReadinessEndpoint, fiber.StatusServiceUnavailable, "Service Unavailable")
	require.Equal(t, "text/plain; charset=utf-8", resp.Header.Get("Content-Type"))
}

// Test_HealthCheck_Structured_Formats checks the JSON, XML, MsgPack and CBOR
// responses of a passing and of a failing probe.
func Test_HealthCheck_Structured_Formats(t *testing.T) {
	t.Parallel()

	// decoded checks a body that unmarshal decodes to exactly one "status" field.
	decoded := func(unmarshal func([]byte, any) error) func(*testing.T, []byte, string) {
		return func(t *testing.T, body []byte, status string) {
			t.Helper()

			var response map[string]string
			require.NoError(t, unmarshal(body, &response))
			require.Len(t, response, 1)
			require.Contains(t, response, "status")
			require.NotContains(t, response, "Status")
			require.Equal(t, status, response["status"])
		}
	}

	for _, tc := range []struct {
		app         fiber.Config
		check       func(t *testing.T, body []byte, status string)
		name        string
		contentType string
		format      ResponseFormat
	}{
		{
			name:        "JSON",
			format:      FormatJSON,
			contentType: "application/json; charset=utf-8",
			check: func(t *testing.T, body []byte, status string) {
				t.Helper()

				require.JSONEq(t, `{"status":"`+status+`"}`, string(body))
			},
		},
		{
			name:        "XML",
			format:      FormatXML,
			contentType: "application/xml; charset=utf-8",
			check: func(t *testing.T, body []byte, status string) {
				t.Helper()

				require.Contains(t, string(body), "<healthResponse>")
				require.Contains(t, string(body), "<status>"+status+"</status>")
			},
		},
		{
			name:        "MsgPack",
			app:         fiber.Config{MsgPackEncoder: msgpack.Marshal},
			format:      FormatMsgPack,
			contentType: "application/vnd.msgpack",
			check:       decoded(msgpack.Unmarshal),
		},
		{
			name:        "CBOR",
			app:         fiber.Config{CBOREncoder: cbor.Marshal},
			format:      FormatCBOR,
			contentType: "application/cbor",
			check:       decoded(cbor.Unmarshal),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app := fiber.New(tc.app)
			app.Get(LivenessEndpoint, New(Config{ResponseFormat: tc.format}))
			app.Get(ReadinessEndpoint, New(Config{
				ResponseFormat: tc.format,
				Probe: func(_ fiber.Ctx) bool {
					return false
				},
			}))

			// Test successful healthcheck
			resp := apptest.Expect(t, app, LivenessEndpoint, fiber.StatusOK)
			require.Equal(t, tc.contentType, resp.Header.Get("Content-Type"))
			tc.check(t, apptest.BodyBytes(t, resp), "OK")

			// Test failed healthcheck
			resp = apptest.Expect(t, app, ReadinessEndpoint, fiber.StatusServiceUnavailable)
			require.Equal(t, tc.contentType, resp.Header.Get("Content-Type"))
			tc.check(t, apptest.BodyBytes(t, resp), "Service Unavailable")
		})
	}
}

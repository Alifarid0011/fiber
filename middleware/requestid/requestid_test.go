package requestid

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/internal/apptest"
	"github.com/gofiber/fiber/v3/internal/loggertest"
	fiberlog "github.com/gofiber/fiber/v3/log"
	"github.com/gofiber/fiber/v3/middleware/logger"
	"github.com/stretchr/testify/require"
)

func Test_RequestID(t *testing.T) {
	t.Parallel()
	app := fiber.New()

	app.Use(New())

	app.Get("/", func(c fiber.Ctx) error { return c.SendString("Hello, World 👋!") })

	resp := apptest.Expect(t, app, "/", fiber.StatusOK)

	reqid := resp.Header.Get(fiber.HeaderXRequestID)
	require.Len(t, reqid, 43)

	req := httptest.NewRequest(fiber.MethodGet, "/", http.NoBody)
	req.Header.Add(fiber.HeaderXRequestID, reqid)

	resp = apptest.Expect(t, app, req, fiber.StatusOK)
	require.Equal(t, reqid, resp.Header.Get(fiber.HeaderXRequestID))
}

func Test_RequestID_InvalidHeaderValue(t *testing.T) {
	t.Parallel()

	rid := sanitizeRequestID("bad\r\nid", func() string { return "clean-generated-id" })

	require.Equal(t, "clean-generated-id", rid)
}

// requestidTestID serves one request through an app whose middleware draws its
// IDs from generate and returns the X-Request-ID it answers with.
func requestidTestID(t *testing.T, generate func() string) string {
	t.Helper()

	app := fiber.New()
	app.Use(New(Config{Generator: generate}))

	app.Get("/", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

	resp := apptest.Expect(t, app, "/", fiber.StatusOK)

	return resp.Header.Get(fiber.HeaderXRequestID)
}

func Test_RequestID_InvalidGeneratedValue(t *testing.T) {
	t.Parallel()

	rid := requestidTestID(t, func() string { return "bad\r\nid" })

	require.NotEmpty(t, rid)
	require.NotContains(t, rid, "\r")
	require.NotContains(t, rid, "\n")
	require.Len(t, rid, 43, "Fallback should produce a SecureToken")
}

func Test_RequestID_GeneratorAlwaysInvalid(t *testing.T) {
	t.Parallel()

	rid := requestidTestID(t, func() string {
		return "invalid\x00id" // Always invalid due to null byte
	})

	require.NotEmpty(t, rid)
	require.Len(t, rid, 43, "Should fall back to SecureToken after 3 invalid attempts")
}

func Test_RequestID_CustomGenerator(t *testing.T) {
	t.Parallel()

	rid := requestidTestID(t, func() string { return "custom-valid-id" })

	require.Equal(t, "custom-valid-id", rid)
}

func Test_isValidRequestID_VisibleASCII(t *testing.T) {
	t.Parallel()

	require.True(t, isValidRequestID("request-id-09AZaz ~"))
}

func Test_isValidRequestID_Boundaries(t *testing.T) {
	t.Parallel()

	t.Run("allows space and tilde", func(t *testing.T) {
		t.Parallel()

		require.True(t, isValidRequestID(" ~"))
	})

	t.Run("rejects out of range", func(t *testing.T) {
		t.Parallel()

		require.False(t, isValidRequestID(string([]byte{0x1f})))
		require.False(t, isValidRequestID(string([]byte{0x7f})))
		require.False(t, isValidRequestID("a\tb"))
	})

	t.Run("rejects empty", func(t *testing.T) {
		t.Parallel()

		require.False(t, isValidRequestID(""))
	})
}

func Test_isValidRequestID_RejectsObsText(t *testing.T) {
	t.Parallel()

	require.False(t, isValidRequestID("valid\xff"))
}

func Test_isValidRequestID_WordBoundaries(t *testing.T) {
	t.Parallel()

	// Word-at-a-time coverage: valid and invalid bytes at whole-word,
	// overlapping-tail-word, and scalar-tail positions.
	require.True(t, isValidRequestID("6ba7b810-9dad-11d1-80b4-00c04fd430c8"))
	require.True(t, isValidRequestID("abcdefgh"))
	require.False(t, isValidRequestID("abcdefg\x1f"))
	require.False(t, isValidRequestID("abcdefgh\x7f"))
	require.False(t, isValidRequestID("abcdefghij\x00klm"))
	require.False(t, isValidRequestID("ab\x00"))
}

func Test_RequestID_Next(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	app.Use(New(Config{Next: func(_ fiber.Ctx) bool { return true }}))

	resp := apptest.Do(t, app, "/")
	require.Empty(t, resp.Header.Get(fiber.HeaderXRequestID))
	require.Equal(t, fiber.StatusNotFound, resp.StatusCode)
}

func Test_RequestID_FromContext(t *testing.T) {
	t.Parallel()
	reqID := "ThisIsARequestId"

	app := fiber.New()
	app.Use(New(Config{Generator: func() string { return reqID }}))

	var ctxVal string

	app.Use(func(c fiber.Ctx) error {
		ctxVal = FromContext(c)
		return c.Next()
	})

	apptest.Do(t, app, "/")
	require.Equal(t, reqID, ctxVal)
}

func Test_RequestID_LoggerMiddlewareTag(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	app := fiber.New()
	app.Use(New(Config{Generator: func() string { return "req-42" }}))
	app.Use(logger.New(logger.Config{Format: "${requestid} ${request-id}", Stream: &buf}))
	app.Get("/", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

	apptest.Expect(t, app, "/", fiber.StatusOK)
	require.Equal(t, "req-42"+" req-42", buf.String())
}

// Test_RequestID_LogWithContextFormat runs serially because it mutates the
// package-global logger output and context format.
func Test_RequestID_LogWithContextFormat(t *testing.T) {
	buf := loggertest.CaptureContextLog(t, fiberlog.RequestIDFormat)

	app := fiber.New()
	app.Use(New(Config{Generator: func() string { return "req-42" }}))
	app.Get("/", func(c fiber.Ctx) error {
		fiberlog.WithContext(c).Info("start")
		return c.SendStatus(fiber.StatusOK)
	})

	apptest.Expect(t, app, "/", fiber.StatusOK)
	require.Contains(t, buf.String(), "[Info] [req-42] start")
}

func Test_RequestID_FromContext_Empty(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	// No middleware

	app.Use(func(c fiber.Ctx) error {
		ctxVal := FromContext(c)
		require.Empty(t, ctxVal)
		return c.SendStatus(fiber.StatusOK)
	})

	apptest.Do(t, app, "/")
}

func Test_RequestID_FromContext_Types(t *testing.T) {
	t.Parallel()

	reqID := "request-id-123"

	app := fiber.New(fiber.Config{PassLocalsToContext: true})
	app.Use(New(Config{Generator: func() string { return reqID }}))

	app.Get("/", func(c fiber.Ctx) error {
		require.Equal(t, reqID, FromContext(c))
		customCtx, ok := c.(fiber.CustomCtx)
		require.True(t, ok)
		require.Equal(t, reqID, FromContext(customCtx))
		require.Equal(t, reqID, FromContext(c.RequestCtx()))
		require.Equal(t, reqID, FromContext(c.Context()))
		return c.SendStatus(fiber.StatusOK)
	})

	apptest.Expect(t, app, "/", fiber.StatusOK)
}

func Benchmark_isValidRequestID(b *testing.B) {
	inputs := []string{
		"6ba7b810-9dad-11d1-80b4-00c04fd430c8", // UUID, the middleware's own format
		"trace-12345",
		"bad\x00id-aaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	var ok bool
	b.ReportAllocs()
	for b.Loop() {
		for _, in := range inputs {
			ok = isValidRequestID(in)
		}
	}
	_ = ok
}

package favicon

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/internal/apptest"
)

// go test -run Test_Middleware_Favicon
func Test_Middleware_Favicon(t *testing.T) {
	t.Parallel()
	app := fiber.New()

	app.Use(New())

	app.Get("/", func(_ fiber.Ctx) error {
		return nil
	})

	// Skip Favicon middleware
	apptest.Expect(t, app, "/", fiber.StatusOK)

	apptest.Expect(t, app, "/favicon.ico", fiber.StatusNoContent)

	apptest.Expect(t, app, apptest.Req(fiber.MethodOptions, "/favicon.ico"), fiber.StatusOK)

	resp := apptest.Expect(t, app, apptest.Req(fiber.MethodPut, "/favicon.ico"), fiber.StatusMethodNotAllowed)
	require.Equal(t, "GET, HEAD, OPTIONS", resp.Header.Get(fiber.HeaderAllow))
}

// go test -run Test_Middleware_Favicon_Panics
func Test_Middleware_Favicon_Panics(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "favicon.ico"), bytes.Repeat([]byte("a"), 11), 0o600))

	tests := []struct {
		name   string
		config Config
	}{
		{name: "file not found", config: Config{File: "non-exist.ico"}},
		{name: "file larger than MaxBytes", config: Config{File: filepath.Join(dir, "favicon.ico"), MaxBytes: 10}},
		{name: "file in the FileSystem larger than MaxBytes", config: Config{File: "favicon.ico", FileSystem: os.DirFS(dir), MaxBytes: 10}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Panics(t, func() {
				fiber.New().Use(New(tc.config))
			}, "should catch panic")
		})
	}
}

// go test -run Test_Middleware_Favicon_Serves
func Test_Middleware_Favicon_Serves(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("../../.github/testdata/favicon.ico")
	require.NoError(t, err)

	tests := []struct {
		name         string
		target       string
		cacheControl string // not checked when empty
		config       Config
	}{
		{
			name:         "file",
			config:       Config{File: "../../.github/testdata/favicon.ico"},
			target:       "/favicon.ico",
			cacheControl: "public, max-age=31536000",
		},
		{
			name:   "custom URL",
			config: Config{File: "../../.github/testdata/favicon.ico", URL: "/favicon.svg"},
			target: "/favicon.svg",
		},
		{
			name:         "data",
			config:       Config{Data: data},
			target:       "/favicon.ico",
			cacheControl: "public, max-age=31536000",
		},
		{
			name:         "file system",
			config:       Config{File: "favicon.ico", FileSystem: os.DirFS("../../.github/testdata")},
			target:       "/favicon.ico",
			cacheControl: "public, max-age=31536000",
		},
		{
			name:         "custom cache control",
			config:       Config{CacheControl: "public, max-age=100", File: "../../.github/testdata/favicon.ico"},
			target:       "/favicon.ico",
			cacheControl: "public, max-age=100",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := fiber.New()

			app.Use(New(tc.config))

			app.Get("/", func(_ fiber.Ctx) error {
				return nil
			})

			resp := apptest.Expect(t, app, tc.target, fiber.StatusOK)
			require.Equal(t, "image/x-icon", resp.Header.Get(fiber.HeaderContentType))
			if tc.cacheControl != "" {
				require.Equal(t, tc.cacheControl, resp.Header.Get(fiber.HeaderCacheControl), "CacheControl Control")
			}
		})
	}
}

// go test -v -run=^$ -bench=Benchmark_Middleware_Favicon -benchmem -count=4
func Benchmark_Middleware_Favicon(b *testing.B) {
	app := fiber.New()
	app.Use(New())
	app.Get("/", func(_ fiber.Ctx) error {
		return nil
	})
	handler := app.Handler()

	c := &fasthttp.RequestCtx{}
	c.Request.SetRequestURI("/")

	b.ReportAllocs()
	for b.Loop() {
		handler(c)
	}
}

// go test -run Test_Favicon_Next
func Test_Favicon_Next(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	app.Use(New(Config{
		Next: func(_ fiber.Ctx) bool {
			return true
		},
	}))

	apptest.Expect(t, app, "/", fiber.StatusNotFound)
}

func Test_Favicon_MaxBytes_MaxInt64(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(New(Config{File: "../../.github/testdata/favicon.ico", MaxBytes: math.MaxInt64}))

	resp := apptest.Expect(t, app, "/favicon.ico", fiber.StatusOK)
	body := apptest.BodyBytes(t, resp)
	require.NotEmpty(t, body)
}

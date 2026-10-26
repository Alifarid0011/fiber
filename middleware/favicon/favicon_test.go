package favicon

import (
	"bytes"
	"math"
	"os"
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

// go test -run Test_Middleware_Favicon_Not_Found
func Test_Middleware_Favicon_Not_Found(t *testing.T) {
	t.Parallel()
	defer func() {
		if err := recover(); err == nil {
			t.Error("should catch panic")
			return
		}
	}()

	fiber.New().Use(New(Config{
		File: "non-exist.ico",
	}))
}

// go test -run Test_Middleware_Favicon_MaxBytes
func Test_Middleware_Favicon_MaxBytes(t *testing.T) {
	t.Parallel()
	defer func() {
		if err := recover(); err == nil {
			t.Error("should catch panic")
		}
	}()

	dir := t.TempDir()
	path := dir + "/favicon.ico"
	err := os.WriteFile(path, bytes.Repeat([]byte("a"), 11), 0o600)
	require.NoError(t, err)

	fiber.New().Use(New(Config{
		File:     path,
		MaxBytes: 10,
	}))
}

// go test -run Test_Middleware_Favicon_MaxBytes_FileSystem
func Test_Middleware_Favicon_MaxBytes_FileSystem(t *testing.T) {
	t.Parallel()
	defer func() {
		if err := recover(); err == nil {
			t.Error("should catch panic")
		}
	}()

	dir := t.TempDir()
	path := dir + "/favicon.ico"
	err := os.WriteFile(path, bytes.Repeat([]byte("a"), 11), 0o600)
	require.NoError(t, err)

	fiber.New().Use(New(Config{
		File:       "favicon.ico",
		FileSystem: os.DirFS(dir),
		MaxBytes:   10,
	}))
}

// go test -run Test_Middleware_Favicon_Found
func Test_Middleware_Favicon_Found(t *testing.T) {
	t.Parallel()
	app := fiber.New()

	app.Use(New(Config{
		File: "../../.github/testdata/favicon.ico",
	}))

	app.Get("/", func(_ fiber.Ctx) error {
		return nil
	})

	resp := apptest.Expect(t, app, "/favicon.ico", fiber.StatusOK)
	require.Equal(t, "image/x-icon", resp.Header.Get(fiber.HeaderContentType))
	require.Equal(t, "public, max-age=31536000", resp.Header.Get(fiber.HeaderCacheControl), "CacheControl Control")
}

// go test -run Test_Custom_Favicon_Url
func Test_Custom_Favicon_URL(t *testing.T) {
	app := fiber.New()
	const customURL = "/favicon.svg"
	app.Use(New(Config{
		File: "../../.github/testdata/favicon.ico",
		URL:  customURL,
	}))

	app.Get("/", func(_ fiber.Ctx) error {
		return nil
	})

	resp := apptest.Expect(t, app, customURL, fiber.StatusOK)
	require.Equal(t, "image/x-icon", resp.Header.Get(fiber.HeaderContentType))
}

// go test -run Test_Custom_Favicon_Data
func Test_Custom_Favicon_Data(t *testing.T) {
	data, err := os.ReadFile("../../.github/testdata/favicon.ico")
	require.NoError(t, err)

	app := fiber.New()

	app.Use(New(Config{
		Data: data,
	}))

	app.Get("/", func(_ fiber.Ctx) error {
		return nil
	})

	resp := apptest.Expect(t, app, "/favicon.ico", fiber.StatusOK)
	require.Equal(t, "image/x-icon", resp.Header.Get(fiber.HeaderContentType))
	require.Equal(t, "public, max-age=31536000", resp.Header.Get(fiber.HeaderCacheControl), "CacheControl Control")
}

// go test -run Test_Middleware_Favicon_FileSystem
func Test_Middleware_Favicon_FileSystem(t *testing.T) {
	t.Parallel()
	app := fiber.New()

	app.Use(New(Config{
		File:       "favicon.ico",
		FileSystem: os.DirFS("../../.github/testdata"),
	}))

	resp := apptest.Expect(t, app, "/favicon.ico", fiber.StatusOK)
	require.Equal(t, "image/x-icon", resp.Header.Get(fiber.HeaderContentType))
	require.Equal(t, "public, max-age=31536000", resp.Header.Get(fiber.HeaderCacheControl), "CacheControl Control")
}

// go test -run Test_Middleware_Favicon_CacheControl
func Test_Middleware_Favicon_CacheControl(t *testing.T) {
	t.Parallel()
	app := fiber.New()

	app.Use(New(Config{
		CacheControl: "public, max-age=100",
		File:         "../../.github/testdata/favicon.ico",
	}))

	resp := apptest.Expect(t, app, "/favicon.ico", fiber.StatusOK)
	require.Equal(t, "image/x-icon", resp.Header.Get(fiber.HeaderContentType))
	require.Equal(t, "public, max-age=100", resp.Header.Get(fiber.HeaderCacheControl), "CacheControl Control")
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

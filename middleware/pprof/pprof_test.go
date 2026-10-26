package pprof

import (
	"bytes"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/internal/apptest"
	"github.com/stretchr/testify/require"
)

var testConfig = fiber.TestConfig{
	Timeout:       5 * time.Second,
	FailOnTimeout: true,
}

func Test_Non_Pprof_Path(t *testing.T) {
	app := fiber.New()

	app.Use(New())

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("escaped")
	})

	apptest.ExpectBody(t, app, "/", 200, "escaped")
}

func Test_Non_Pprof_Path_WithPrefix(t *testing.T) {
	app := fiber.New()

	app.Use(New(Config{Prefix: "/federated-fiber"}))

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("escaped")
	})

	apptest.ExpectBody(t, app, "/", 200, "escaped")
}

func Test_Pprof_Index(t *testing.T) {
	app := fiber.New()

	app.Use(New())

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("escaped")
	})

	resp := apptest.Expect(t, app, "/debug/pprof/", 200)
	require.Equal(t, fiber.MIMETextHTMLCharsetUTF8, resp.Header.Get(fiber.HeaderContentType))

	b := apptest.BodyBytes(t, resp)
	require.True(t, bytes.Contains(b, []byte("<title>/debug/pprof/</title>")))
}

func Test_Pprof_Index_WithPrefix(t *testing.T) {
	app := fiber.New()

	app.Use(New(Config{Prefix: "/federated-fiber"}))

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("escaped")
	})

	resp := apptest.Expect(t, app, "/federated-fiber/debug/pprof/", 200)
	require.Equal(t, fiber.MIMETextHTMLCharsetUTF8, resp.Header.Get(fiber.HeaderContentType))

	b := apptest.Body(t, resp)
	require.Contains(t, b, "<title>/debug/pprof/</title>")
}

func Test_Pprof_Subs(t *testing.T) {
	app := fiber.New()

	app.Use(New())

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("escaped")
	})

	subs := []string{
		"cmdline", "profile", "symbol", "trace", "allocs", "block",
		"goroutine", "heap", "mutex", "threadcreate",
	}

	for _, sub := range subs {
		t.Run(sub, func(t *testing.T) {
			target := "/debug/pprof/" + sub
			if sub == "profile" {
				target += "?seconds=1"
			}
			resp := apptest.Do(t, app, target, testConfig)
			require.Equal(t, 200, resp.StatusCode)
		})
	}
}

func Test_Pprof_Subs_WithPrefix(t *testing.T) {
	app := fiber.New()

	app.Use(New(Config{Prefix: "/federated-fiber"}))

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("escaped")
	})

	subs := []string{
		"cmdline", "profile", "symbol", "trace", "allocs", "block",
		"goroutine", "heap", "mutex", "threadcreate",
	}

	for _, sub := range subs {
		t.Run(sub, func(t *testing.T) {
			target := "/federated-fiber/debug/pprof/" + sub
			if sub == "profile" {
				target += "?seconds=1"
			}
			resp := apptest.Do(t, app, target, testConfig)
			require.Equal(t, 200, resp.StatusCode)
		})
	}
}

func Test_Pprof_Other(t *testing.T) {
	app := fiber.New()

	app.Use(New())

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("escaped")
	})

	apptest.Expect(t, app, "/debug/pprof/303", fiber.StatusSeeOther)
}

func Test_Pprof_Other_WithPrefix(t *testing.T) {
	app := fiber.New()

	app.Use(New(Config{Prefix: "/federated-fiber"}))

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("escaped")
	})

	apptest.Expect(t, app, "/federated-fiber/debug/pprof/303", fiber.StatusSeeOther)
}

func Test_Pprof_TrailingSlash_RedirectsWithinPrefix(t *testing.T) {
	app := fiber.New()

	app.Use(New())

	resp := apptest.Expect(t, app, "/debug/pprof/heap/", fiber.StatusSeeOther)
	require.Equal(t, "/debug/pprof/heap", resp.Header.Get(fiber.HeaderLocation))
}

func Test_Pprof_TrailingSlash_RedirectsWithinPrefix_WithPrefix(t *testing.T) {
	app := fiber.New()

	app.Use(New(Config{Prefix: "/federated-fiber"}))

	resp := apptest.Expect(t, app, "/federated-fiber/debug/pprof/heap/", fiber.StatusSeeOther)
	require.Equal(t, "/federated-fiber/debug/pprof/heap", resp.Header.Get(fiber.HeaderLocation))
}

// go test -run Test_Pprof_Next
func Test_Pprof_Next(t *testing.T) {
	app := fiber.New()

	app.Use(New(Config{
		Next: func(_ fiber.Ctx) bool {
			return true
		},
	}))

	apptest.Expect(t, app, "/debug/pprof/", 404)
}

// go test -run Test_Pprof_Next_WithPrefix
func Test_Pprof_Next_WithPrefix(t *testing.T) {
	app := fiber.New()

	app.Use(New(Config{
		Next: func(_ fiber.Ctx) bool {
			return true
		},
		Prefix: "/federated-fiber",
	}))

	apptest.Expect(t, app, "/federated-fiber/debug/pprof/", 404)
}

// Test_Pprof_Sibling_Prefix_Path ensures a route that merely starts with the
// same characters as /debug/pprof is not swallowed by the middleware.
func Test_Pprof_Sibling_Prefix_Path(t *testing.T) {
	t.Parallel()
	app := fiber.New()

	app.Use(New())

	app.Get("/debug/pprofiler", func(c fiber.Ctx) error {
		return c.SendString("app route")
	})

	apptest.ExpectBody(t, app, "/debug/pprofiler", fiber.StatusOK, "app route")
}

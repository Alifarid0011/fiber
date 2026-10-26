package expvar

import (
	"bytes"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/internal/apptest"
	"github.com/stretchr/testify/require"
)

func Test_Non_Expvar_Path(t *testing.T) {
	t.Parallel()
	app := fiber.New()

	app.Use(New())

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("escaped")
	})

	apptest.ExpectBody(t, app, "/", 200, "escaped")
}

func Test_Expvar_Index(t *testing.T) {
	t.Parallel()
	app := fiber.New()

	app.Use(New())

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("escaped")
	})

	resp := apptest.Expect(t, app, "/debug/vars", 200)
	require.Equal(t, fiber.MIMEApplicationJSONCharsetUTF8, resp.Header.Get(fiber.HeaderContentType))

	b := apptest.BodyBytes(t, resp)
	require.True(t, bytes.Contains(b, []byte("cmdline")))
	require.True(t, bytes.Contains(b, []byte("memstat")))
}

func Test_Expvar_Filter(t *testing.T) {
	t.Parallel()
	app := fiber.New()

	app.Use(New())

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("escaped")
	})

	resp := apptest.Expect(t, app, "/debug/vars?r=cmd", 200)
	require.Equal(t, fiber.MIMEApplicationJSONCharsetUTF8, resp.Header.Get(fiber.HeaderContentType))

	b := apptest.BodyBytes(t, resp)
	require.True(t, bytes.Contains(b, []byte("cmdline")))
	require.False(t, bytes.Contains(b, []byte("memstat")))
}

func Test_Expvar_Other_Path(t *testing.T) {
	t.Parallel()
	app := fiber.New()

	app.Use(New())

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("escaped")
	})

	apptest.Expect(t, app, "/debug/vars/303", fiber.StatusSeeOther)
}

// go test -run Test_Expvar_Next
func Test_Expvar_Next(t *testing.T) {
	t.Parallel()
	app := fiber.New()

	app.Use(New(Config{
		Next: func(_ fiber.Ctx) bool {
			return true
		},
	}))

	apptest.Expect(t, app, "/debug/vars", 404)
}

// Test_Expvar_Sibling_Prefix_Path ensures a route that merely starts with the
// same characters as /debug/vars is not swallowed by the middleware.
func Test_Expvar_Sibling_Prefix_Path(t *testing.T) {
	t.Parallel()
	app := fiber.New()

	app.Use(New())

	app.Get("/debug/varsdump", func(c fiber.Ctx) error {
		return c.SendString("app route")
	})

	apptest.ExpectBody(t, app, "/debug/varsdump", fiber.StatusOK, "app route")
}

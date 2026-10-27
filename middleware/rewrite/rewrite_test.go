package rewrite

import (
	"bufio"
	"fmt"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/internal/apptest"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func Test_New(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cfg  []Config
	}{
		{name: "no config"},
		{name: "config", cfg: []Config{{Rules: map[string]string{"/old": "/new"}}}},
		{name: "full config", cfg: []Config{{
			Next:  func(fiber.Ctx) bool { return true },
			Rules: map[string]string{"/old": "/new"},
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.NotNil(t, New(tc.cfg...), "Expected middleware to be returned, got nil")
		})
	}
}

func Test_Rewrite(t *testing.T) {
	t.Parallel()
	oldToNew := map[string]string{"/old": "/new"}
	tokens := map[string]string{"/users/*/orders/*": "/user/$1/order/$2"}

	// success answers "Rewrite Successful" at path; orders is the route the
	// token rule rewrites into and answers with the two captured IDs.
	success := func(path string) func(*fiber.App) {
		return func(app *fiber.App) {
			app.Get(path, func(c fiber.Ctx) error { return c.SendString("Rewrite Successful") })
		}
	}
	orders := func(app *fiber.App) {
		app.Get("/user/:userID/order/:orderID", func(c fiber.Ctx) error {
			return c.SendString(fmt.Sprintf("User ID: %s, Order ID: %s", c.Params("userID"), c.Params("orderID")))
		})
	}

	for _, tc := range []struct {
		routes func(*fiber.App)
		name   string
		target string
		body   string
		cfg    Config
	}{
		{
			name:   "Next always returns true",
			cfg:    Config{Next: func(fiber.Ctx) bool { return true }, Rules: oldToNew},
			routes: success("/old"),
			target: "/old", body: "Rewrite Successful",
		},
		{
			name:   "Next always returns false",
			cfg:    Config{Next: func(fiber.Ctx) bool { return false }, Rules: oldToNew},
			routes: success("/new"),
			target: "/old", body: "Rewrite Successful",
		},
		{
			name:   "captured tokens in the rewrite rule",
			cfg:    Config{Rules: tokens},
			routes: orders,
			target: "/users/123/orders/456", body: "User ID: 123, Order ID: 456",
		},
		{
			name: "non-matching request handled by default route",
			cfg:  Config{Rules: tokens},
			routes: func(app *fiber.App) {
				orders(app)
				app.Use(func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })
			},
			target: "/not-matching-any-rule", body: "OK",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := fiber.New()
			app.Use(New(tc.cfg))
			tc.routes(app)
			apptest.ExpectBody(t, app, tc.target, fiber.StatusOK, tc.body)
		})
	}

	// Only the status is pinned here: nothing answers, so the body is Fiber's.
	t.Run("non-matching request with no default route", func(t *testing.T) {
		t.Parallel()
		app := fiber.New()
		app.Use(New(Config{Rules: tokens}))
		orders(app)
		apptest.Expect(t, app, "/not-matching-any-rule", fiber.StatusNotFound)
	})
}

// Test_Rewrite_StartAnchor verifies that a rule only matches from the start of
// the path, so a rule does not fire on an unrelated route that merely contains
// the rule path as a suffix (issue #4476).
func Test_Rewrite_StartAnchor(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	app.Use(New(Config{Rules: map[string]string{"/users/*": "/u/$1"}}))
	app.Get("/u/:id", func(c fiber.Ctx) error { return c.SendString("rewritten:" + c.Params("id")) })
	app.Get("/api/users/:id", func(c fiber.Ctx) error { return c.SendString("api:" + c.Params("id")) })

	// The rule matches from the start of the path and is rewritten.
	apptest.ExpectBody(t, app, "/users/1", fiber.StatusOK, "rewritten:1")

	// A path that only contains the rule path as a suffix must not be rewritten.
	apptest.ExpectBody(t, app, "/api/users/1", fiber.StatusOK, "api:1")
}

func Benchmark_Rewrite(b *testing.B) {
	// Helper function to create a new Fiber app with rewrite middleware
	createApp := func(config Config) *fiber.App {
		app := fiber.New()
		app.Use(New(config))
		return app
	}

	// Benchmark: Rewrite with Next function always returns true
	b.Run("Next always true", func(b *testing.B) {
		app := createApp(Config{Next: func(fiber.Ctx) bool { return true }, Rules: map[string]string{"/old": "/new"}})

		reqCtx := &fasthttp.RequestCtx{}
		reqCtx.Request.SetRequestURI("/old")
		b.ReportAllocs()
		for b.Loop() {
			app.Handler()(reqCtx)
		}
	})

	// Benchmark: Rewrite with Next function always returns false
	b.Run("Next always false", func(b *testing.B) {
		app := createApp(Config{Next: func(fiber.Ctx) bool { return false }, Rules: map[string]string{"/old": "/new"}})

		reqCtx := &fasthttp.RequestCtx{}
		reqCtx.Request.SetRequestURI("/old")
		b.ReportAllocs()
		for b.Loop() {
			app.Handler()(reqCtx)
		}
	})

	// Benchmark: Rewrite with tokens
	b.Run("Rewrite with tokens", func(b *testing.B) {
		app := createApp(Config{Rules: map[string]string{"/users/*/orders/*": "/user/$1/order/$2"}})

		reqCtx := &fasthttp.RequestCtx{}
		reqCtx.Request.SetRequestURI("/users/123/orders/456")
		b.ReportAllocs()
		for b.Loop() {
			app.Handler()(reqCtx)
		}
	})

	// Benchmark: Non-matching request, handled by default route
	b.Run("NonMatch with default", func(b *testing.B) {
		app := createApp(Config{Rules: map[string]string{"/users/*/orders/*": "/user/$1/order/$2"}})
		app.Use(func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

		reqCtx := &fasthttp.RequestCtx{}
		reqCtx.Request.SetRequestURI("/not-matching-any-rule")
		b.ReportAllocs()
		for b.Loop() {
			app.Handler()(reqCtx)
		}
	})

	// Benchmark: Non-matching request, with no default route
	b.Run("NonMatch without default", func(b *testing.B) {
		app := createApp(Config{Rules: map[string]string{"/users/*/orders/*": "/user/$1/order/$2"}})

		reqCtx := &fasthttp.RequestCtx{}
		reqCtx.Request.SetRequestURI("/not-matching-any-rule")
		b.ReportAllocs()
		for b.Loop() {
			app.Handler()(reqCtx)
		}
	})
}

func Benchmark_Rewrite_Parallel(b *testing.B) {
	// Helper function to create a new Fiber app with rewrite middleware
	createApp := func(config Config) *fiber.App {
		app := fiber.New()
		app.Use(New(config))
		return app
	}

	// Parallel Benchmark: Rewrite with Next function always returns true
	b.Run("Next always true", func(b *testing.B) {
		app := createApp(Config{Next: func(fiber.Ctx) bool { return true }, Rules: map[string]string{"/old": "/new"}})
		b.ReportAllocs()
		b.ResetTimer()

		b.RunParallel(func(pb *testing.PB) {
			reqCtx := &fasthttp.RequestCtx{}
			reqCtx.Request.SetRequestURI("/old")
			for pb.Next() {
				app.Handler()(reqCtx)
			}
		})
	})

	// Parallel Benchmark: Rewrite with Next function always returns false
	b.Run("Next always false", func(b *testing.B) {
		app := createApp(Config{Next: func(fiber.Ctx) bool { return false }, Rules: map[string]string{"/old": "/new"}})
		b.ReportAllocs()
		b.ResetTimer()

		b.RunParallel(func(pb *testing.PB) {
			reqCtx := &fasthttp.RequestCtx{}
			reqCtx.Request.SetRequestURI("/old")
			for pb.Next() {
				app.Handler()(reqCtx)
			}
		})
	})

	// Parallel Benchmark: Rewrite with tokens
	b.Run("Rewrite with tokens", func(b *testing.B) {
		app := createApp(Config{Rules: map[string]string{"/users/*/orders/*": "/user/$1/order/$2"}})
		b.ReportAllocs()
		b.ResetTimer()

		b.RunParallel(func(pb *testing.PB) {
			reqCtx := &fasthttp.RequestCtx{}
			reqCtx.Request.SetRequestURI("/users/123/orders/456")
			for pb.Next() {
				app.Handler()(reqCtx)
			}
		})
	})

	// Parallel Benchmark: Non-matching request, handled by default route
	b.Run("NonMatch with default", func(b *testing.B) {
		app := createApp(Config{Rules: map[string]string{"/users/*/orders/*": "/user/$1/order/$2"}})
		app.Use(func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })
		b.ReportAllocs()
		b.ResetTimer()

		b.RunParallel(func(pb *testing.PB) {
			reqCtx := &fasthttp.RequestCtx{}
			reqCtx.Request.SetRequestURI("/not-matching-any-rule")
			for pb.Next() {
				app.Handler()(reqCtx)
			}
		})
	})

	// Parallel Benchmark: Non-matching request, with no default route
	b.Run("NonMatch without default", func(b *testing.B) {
		app := createApp(Config{Rules: map[string]string{"/users/*/orders/*": "/user/$1/order/$2"}})
		b.ReportAllocs()
		b.ResetTimer()

		b.RunParallel(func(pb *testing.PB) {
			reqCtx := &fasthttp.RequestCtx{}
			reqCtx.Request.SetRequestURI("/not-matching-any-rule")
			for pb.Next() {
				app.Handler()(reqCtx)
			}
		})
	})
}

// rewritten runs one request through a rewrite config and reports the path the
// handler saw.
func rewritten(t *testing.T, cfg Config, path string) string {
	t.Helper()

	app := fiber.New()
	app.Use(New(cfg))
	app.Use(func(c fiber.Ctx) error { return c.SendString(c.Path()) })

	return apptest.Body(t, apptest.Do(t, app, path))
}

func Test_Rewrite_RuleListOrderWins(t *testing.T) {
	t.Parallel()
	narrowFirst := Config{RuleList: []Rule{{From: "/p/a*", To: "/narrow/$1"}, {From: "/p/*", To: "/broad/$1"}}}
	broadFirst := Config{RuleList: []Rule{{From: "/p/*", To: "/broad/$1"}, {From: "/p/a*", To: "/narrow/$1"}}}

	require.Equal(t, "/narrow/bc", rewritten(t, narrowFirst, "/p/abc"))
	require.Equal(t, "/broad/abc", rewritten(t, broadFirst, "/p/abc"))
}

func Test_Rewrite_DeprecatedRulesOrderIsStable(t *testing.T) {
	t.Parallel()

	// A map has no order, so this pins that the ranking decides the winner
	// rather than Go's randomized iteration.
	cfg := Config{Rules: map[string]string{"/p/*": "/broad/$1", "/p/a*": "/narrow/$1"}}
	for range 25 {
		require.Equal(t, "/narrow/bc", rewritten(t, cfg, "/p/abc"))
	}
}

func Test_Rewrite_PatternIsPathTextNotRegexp(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		from string
		path string
		want string
	}{
		{name: "dot is a dot", from: "/preis-1.000-euro", path: "/preis-1X000-euro", want: "/preis-1X000-euro"},
		{name: "dot matches itself", from: "/preis-1.000-euro", path: "/preis-1.000-euro", want: "/hit"},
		{name: "parentheses are text", from: "/files(old)", path: "/filesold", want: "/filesold"},
		{name: "parentheses match themselves", from: "/files(old)", path: "/files(old)", want: "/hit"},
		{name: "plus is text", from: "/a+", path: "/aaa", want: "/aaa"},
		{name: "plus matches itself", from: "/a+", path: "/a+", want: "/hit"},
		{name: "brackets are text", from: "/v[1]", path: "/v1", want: "/v1"},
		{name: "brackets match themselves", from: "/v[1]", path: "/v[1]", want: "/hit"},
		// A rule spelling "?" can never fire: the byte starts the query, so no
		// path carries it. Quoted it is inert, whereas as a regexp it made the
		// preceding byte optional and fired on a path the author never wrote.
		{name: "question mark no longer eats a shorter path", from: "/faq?", path: "/fa", want: "/fa"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := Config{RuleList: []Rule{{From: tc.from, To: "/hit"}}}
			require.Equal(t, tc.want, rewritten(t, cfg, tc.path))
		})
	}
}

func Test_Rewrite_AlternationNoLongerEscapesTheAnchors(t *testing.T) {
	t.Parallel()

	// Read as a regexp, "^/a|/b$" is "(^/a)|(/b$)", so the anchors bound one
	// branch each and the rule fired on any path starting "/a" or ending "/b"
	// (issue #4476). Quoted, "|" is path text and the rule claims no path a
	// client can send, since the byte arrives percent-encoded.
	cfg := Config{RuleList: []Rule{{From: "/a|/b", To: "/hit"}}}

	for _, path := range []string{"/a", "/b", "/xx/b", "/a/yy"} {
		require.Equal(t, path, rewritten(t, cfg, path))
	}
}

func Test_Rewrite_BothRuleFieldsPanic(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() {
		New(Config{Rules: map[string]string{"/old": "/new"}, RuleList: []Rule{{From: "/old", To: "/new"}}})
	})
}

func Test_Rewrite_DeprecatedRulesRanking(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		rules map[string]string
		want  []string
	}{
		{
			name:  "longer pinned prefix first",
			rules: map[string]string{"/api/*": "/a", "/api/users/*": "/b"},
			want:  []string{"/api/users/*", "/api/*"},
		},
		{
			name:  "same prefix, more pinned text first",
			rules: map[string]string{"/cdn/*": "/a", "/cdn/*x": "/b"},
			want:  []string{"/cdn/*x", "/cdn/*"},
		},
		{
			name:  "same pinned text, fewer wildcards first",
			rules: map[string]string{"/p/*a*b": "/a", "/p/*ab": "/b"},
			want:  []string{"/p/*ab", "/p/*a*b"},
		},
		{
			name:  "otherwise the key, so the order is total",
			rules: map[string]string{"/b/*": "/x", "/a/*": "/y"},
			want:  []string{"/a/*", "/b/*"},
		},
		{
			name:  "a rule without a wildcard pins its whole length",
			rules: map[string]string{"/exact": "/a", "/ex*": "/b"},
			want:  []string{"/exact", "/ex*"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := make([]string, 0, len(tc.rules))
			for _, rule := range orderedRules(Config{Rules: tc.rules}) {
				got = append(got, rule.From)
			}
			require.Equal(t, tc.want, got)
		})
	}
}

func Test_Rewrite_DeprecatedRulesAreQuotedToo(t *testing.T) {
	t.Parallel()

	// Both entry points share compilePattern, so a future split cannot quietly
	// leave the map path compiling its keys as regular expressions.
	cfg := Config{Rules: map[string]string{"/preis-1.000-euro": "/hit"}}

	require.Equal(t, "/preis-1X000-euro", rewritten(t, cfg, "/preis-1X000-euro"))
	require.Equal(t, "/hit", rewritten(t, cfg, "/preis-1.000-euro"))
}

// Test_Rewrite_TwoDigitCaptures pins that "$10" names the tenth capture rather than "$1" followed by "0".
func Test_Rewrite_TwoDigitCaptures(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	app.Use(New(Config{RuleList: []Rule{{From: "/t/*/*/*/*/*/*/*/*/*/*/*", To: "/r/$11/$10/$1"}}}))
	app.Get("/r/:a/:b/:c", func(c fiber.Ctx) error {
		return c.SendString(c.Params("a") + " " + c.Params("b") + " " + c.Params("c"))
	})

	apptest.ExpectBody(t, app, "/t/a/b/c/d/e/f/g/h/i/j/k", fiber.StatusOK, "k j a")
}

// rewrittenRaw hands app a request line exactly as written and reports the
// path the handler behind the rewrite saw.
func rewrittenRaw(t *testing.T, app *fiber.App, target string) string {
	t.Helper()

	var req fasthttp.Request
	raw := fiber.MethodGet + " " + target + " HTTP/1.1\r\nHost: example.com\r\n\r\n"
	require.NoError(t, req.Read(bufio.NewReader(strings.NewReader(raw))))

	var fctx fasthttp.RequestCtx
	fctx.Init(&req, nil, nil)
	app.Handler()(&fctx)
	require.Equal(t, fiber.StatusOK, fctx.Response.StatusCode(), target)
	return string(fctx.Response.Body())
}

// Test_Rewrite_UnescapePathDecodesOnce pins that a capture is not decoded a
// second time when the rewritten path is set: with UnescapePath the router
// decoded "/files/%252e%252e/secret" into "/files/%2e%2e/secret", and the
// "%2e%2e" carried into "/public/$1" has to stay that name rather than
// become ".." and resolve out of "/public/".
func Test_Rewrite_UnescapePathDecodesOnce(t *testing.T) {
	t.Parallel()

	// Without UnescapePath (raw) the path keeps its escapes, and a capture is
	// carried as is; with it (unescaped) the router has decoded the path once.
	cases := []struct {
		name      string
		target    string
		unescaped string
		raw       string
	}{
		{name: "plain path", target: "/files/a/b", unescaped: "/public/a/b", raw: "/public/a/b"},
		{name: "double-encoded dot dot", target: "/files/%252e%252e/secret", unescaped: "/public/%2e%2e/secret", raw: "/public/%252e%252e/secret"},
		{name: "double-encoded slash", target: "/files/..%252Fsecret", unescaped: "/public/..%2Fsecret", raw: "/public/..%252Fsecret"},
		{name: "encoded space", target: "/files/%41%20b", unescaped: "/public/A b", raw: "/public/A%20b"},
		{name: "encoded query and fragment markers", target: "/files/a%3Fb%23c", unescaped: "/public/a?b#c", raw: "/public/a%3Fb%23c"},
		{name: "encoded multibyte rune", target: "/files/caf%C3%A9", unescaped: "/public/café", raw: "/public/caf%C3%A9"},
		{name: "single-encoded dot dot", target: "/files/%2e%2e/secret", unescaped: "/secret", raw: "/secret"},
		{name: "double-encoded dot dot, trailing slash", target: "/files/%252e%252e/secret/", unescaped: "/public/%2e%2e/secret/", raw: "/public/%252e%252e/secret/"},
	}

	for _, unescape := range []bool{false, true} {
		t.Run(fmt.Sprintf("UnescapePath=%v", unescape), func(t *testing.T) {
			t.Parallel()
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					app := fiber.New(fiber.Config{UnescapePath: unescape})
					app.Use(New(Config{RuleList: []Rule{{From: "/files/*", To: "/public/$1"}}}))
					app.Use(func(c fiber.Ctx) error { return c.SendString(c.Path()) })

					want := tc.raw
					if unescape {
						want = tc.unescaped
					}
					require.Equal(t, want, rewrittenRaw(t, app, tc.target), "UnescapePath=%v %s", unescape, tc.target)
				})
			}
		})
	}
}

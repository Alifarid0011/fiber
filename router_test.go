// ⚡️ Fiber is an Express inspired web framework written in Go with ☕️
// 📃 GitHub Repository: https://github.com/gofiber/fiber
// 📌 API Documentation: https://docs.gofiber.io

package fiber

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"os"
	pathpkg "path"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/gofiber/fiber/v3/internal/apptest"
	"github.com/gofiber/utils/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

var routesFixture routeJSON

// sweepTestConfig is for tests that send hundreds of requests through app.Test: each
// one gets a second by default, which a loaded runner misses once in a few thousand.
var sweepTestConfig = TestConfig{Timeout: 10 * time.Second, FailOnTimeout: true}

func init() {
	dat, err := os.ReadFile("./.github/testdata/testRoutes.json")
	if err != nil {
		panic(err)
	}
	if err := json.Unmarshal(dat, &routesFixture); err != nil {
		panic(err)
	}
}

// routerTestRequestCtx returns a fasthttp request context holding a request for
// method and uri, ready to be handed to app.Handler() or app.AcquireCtx.
func routerTestRequestCtx(method, uri string) *fasthttp.RequestCtx {
	fctx := &fasthttp.RequestCtx{}
	fctx.Request.Header.SetMethod(method)
	fctx.Request.SetRequestURI(uri)
	return fctx
}

// routerTestExpect is [apptest.Expect] for requests whose body is not under
// test. It still drains and closes the response body like a client would, so a
// failing read or close fails the test, and returns the response for header checks.
func routerTestExpect[T apptest.Target](tb testing.TB, app *App, target T, status int) *http.Response {
	tb.Helper()

	resp := apptest.Expect(tb, app, target, status)
	apptest.BodyBytes(tb, resp)
	return resp
}

// routerTestExpectPath is [routerTestExpect] for a table row that names the body
// it expects only sometimes: an empty want leaves the body unchecked.
func routerTestExpectPath(tb testing.TB, app *App, path string, status int, want string) {
	tb.Helper()

	body := apptest.Body(tb, apptest.Expect(tb, app, path, status))
	if want != "" {
		require.Equal(tb, want, body, path)
	}
}

func Test_Route_Handler_Order(t *testing.T) {
	t.Parallel()

	app := New()

	var order []int

	handler1 := func(c Ctx) error {
		order = append(order, 1)
		return c.Next()
	}
	handler2 := func(c Ctx) error {
		order = append(order, 2)
		return c.Next()
	}
	handler3 := func(c Ctx) error {
		order = append(order, 3)
		return c.Next()
	}

	app.Get("/test", handler1, handler2, handler3, func(c Ctx) error {
		order = append(order, 4)
		return c.SendStatus(200)
	})

	apptest.Expect(t, app, "/test", 200)

	expectedOrder := []int{1, 2, 3, 4}
	require.Equal(t, expectedOrder, order, "Handler order")
}

func Test_hasFlashCookieExactMatch(t *testing.T) {
	t.Parallel()

	buildRequestWithCookie := func(t *testing.T, cookie string) *fasthttp.Request {
		t.Helper()

		rawRequest := strings.NewReader(
			"GET / HTTP/1.1\r\nHost: localhost\r\nCookie: " + cookie + "\r\n\r\n",
		)
		req := new(fasthttp.Request)
		require.NoError(t, req.Read(bufio.NewReader(rawRequest)))
		return req
	}

	req := buildRequestWithCookie(t, FlashCookieName+"X=not-the-flash-cookie")
	require.False(t, hasFlashCookie(&req.Header))

	req = buildRequestWithCookie(t, FlashCookieName+"=valid")
	require.True(t, hasFlashCookie(&req.Header))

	// A programmatic request carries no raw headers, so the cookie lookup decides.
	var syntheticReq fasthttp.Request
	syntheticReq.Header.Set(HeaderCookie, FlashCookieName+"=valid")
	require.True(t, hasFlashCookie(&syntheticReq.Header))

	var syntheticOther fasthttp.Request
	syntheticOther.Header.Set(HeaderCookie, FlashCookieName+"X=not-the-flash-cookie")
	require.False(t, hasFlashCookie(&syntheticOther.Header))
}

func Test_Route_MixedFiberAndHTTPHandlers(t *testing.T) {
	t.Parallel()

	app := New()

	var order []string

	fiberBefore := func(c Ctx) error {
		order = append(order, "fiber-before")
		c.Set("X-Fiber", "1")
		return c.Next()
	}

	httpHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		order = append(order, "http-final")
		w.Header().Set("X-HTTP", "true")
		_, err := w.Write([]byte("http"))
		assert.NoError(t, err)
	})

	fiberAfter := func(c Ctx) error {
		order = append(order, "fiber-after")
		return c.SendString("fiber")
	}

	app.Get("/mixed", fiberBefore, httpHandler, fiberAfter)

	resp := apptest.ExpectBody(t, app, "/mixed", 200, "http")
	require.Equal(t, "true", resp.Header.Get("X-HTTP"))
	require.Equal(t, "1", resp.Header.Get("X-Fiber"))

	require.Equal(t, []string{"fiber-before", "http-final"}, order)
}

func Test_Route_Group_WithHTTPHandlers(t *testing.T) {
	t.Parallel()

	app := New()

	var order []string

	app.Use("/api", func(c Ctx) error {
		order = append(order, "app-use")
		return c.Next()
	})

	grp := app.Group("/api", func(c Ctx) error {
		order = append(order, "group-middleware")
		return c.Next()
	})

	grp.Get("/users", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		order = append(order, "http-handler")
		_, err := w.Write([]byte("users"))
		assert.NoError(t, err)
	}))

	apptest.ExpectBody(t, app, "/api/users", 200, "users")

	require.Equal(t, []string{"app-use", "group-middleware", "http-handler"}, order)
}

func Test_RouteChain_WithHTTPHandlers(t *testing.T) {
	t.Parallel()

	app := New()

	chain := app.RouteChain("/combo")
	chain.Get(func(c Ctx) error {
		c.Set("X-Chain", "fiber")
		return c.Next()
	}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, err := w.Write([]byte("combo"))
		assert.NoError(t, err)
	}))

	chain.RouteChain("/nested").Get(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Nested", "true")
		_, err := w.Write([]byte("nested"))
		assert.NoError(t, err)
	}))

	resp := apptest.ExpectBody(t, app, "/combo", 200, "combo")
	require.Equal(t, "fiber", resp.Header.Get("X-Chain"))

	nestedResp := apptest.ExpectBody(t, app, "/combo/nested", 200, "nested")
	require.Equal(t, "true", nestedResp.Header.Get("X-Nested"))
}

// Test_Route_Match_Param checks that a route whose handler answers with its one
// parameter binds the text of the path, including a path spelled exactly like
// the pattern.
func Test_Route_Match_Param(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		requests map[string]string // path -> body
		name     string
		pattern  string
		param    string
		use      bool
	}{
		{
			name: "same length", pattern: "/:param", param: "param",
			requests: map[string]string{"/:param": ":param", "/test": "test"},
		},
		{
			name: "star", pattern: "/*", param: "*",
			requests: map[string]string{"/*": "*", "/test": "test"},
		},
		{
			name: "middleware", use: true, pattern: "/foo/*", param: "*",
			requests: map[string]string{"/foo/*": "*", "/foo/bar/fasel": "bar/fasel"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app := New()

			handler := func(c Ctx) error {
				return c.SendString(c.Params(tc.param))
			}
			if tc.use {
				app.Use(tc.pattern, handler)
			} else {
				app.Get(tc.pattern, handler)
			}

			for path, body := range tc.requests {
				apptest.ExpectBody(t, app, path, 200, body)
			}
		})
	}
}

// Test_Route_Match_SlashInParams locks in the matching quirks where a
// parameter value legitimately contains '/', so the slash-count quick-reject
// must not filter these routes out end-to-end.
func Test_Route_Match_SlashInParams(t *testing.T) {
	t.Parallel()

	app := New()
	app.Get("/api/v1/:param-:param2", func(c Ctx) error {
		return c.SendString(c.Params("param") + "|" + c.Params("param2"))
	})
	app.Get("/test:sign:param", func(c Ctx) error {
		return c.SendString(c.Params("sign") + "|" + c.Params("param"))
	})
	app.Get("/date/:day/:month?/:year?", func(c Ctx) error {
		return c.SendString(c.Params("day") + "|" + c.Params("month") + "|" + c.Params("year"))
	})
	app.Use("/mw/:version", func(c Ctx) error {
		return c.SendString(c.Params("version"))
	})

	testCases := []struct {
		url  string
		body string
	}{
		// param swallows '/' via the unguarded single-byte compare part
		{url: "/api/v1/enti/ty-x", body: "enti/ty|x"},
		// adjacent params consume one byte each, possibly '/'
		{url: "/test/x", body: "/|x"},
		// optional segments drop their leading slashes
		{url: "/date/1", body: "1||"},
		{url: "/date/1/2/3", body: "1|2|3"},
		// param middleware matches deeper paths
		{url: "/mw/v1/users/list", body: "v1"},
	}
	for _, tc := range testCases {
		apptest.ExpectBody(t, app, tc.url, 200, tc.body)
	}
}

// Test_App_HasParamRoutes verifies the slash-count gate: apps whose routes
// never consult the per-request slash count (static and star only) skip the
// counting, and rebuilding the tree after adding a parametric route re-enables
// it so param routes still match.
func Test_App_HasParamRoutes(t *testing.T) {
	t.Parallel()

	app := New()
	app.Get("/static/route", testEmptyHandler)
	app.Get("/*", func(c Ctx) error {
		return c.SendString(c.Params("*"))
	})
	app.startupProcess()
	require.False(t, app.hasParamRoutes)

	// static and star routing work without the per-request count
	apptest.Expect(t, app, "/static/route", StatusOK)
	apptest.ExpectBody(t, app, "/anything/else", StatusOK, "anything/else")

	// the GET star route would shadow a GET param route, so use POST
	app.Post("/users/:id", func(c Ctx) error {
		return c.SendString(c.Params("id"))
	})
	app.RebuildTree()
	require.True(t, app.hasParamRoutes)

	apptest.ExpectBody(t, app, apptest.Req(MethodPost, "/users/42"), StatusOK, "42")
}

// Test_Route_Match_Star calls Route.match directly on a star route; the routing
// of a star pattern through an app is the "star" case of Test_Route_Match_Param.
func Test_Route_Match_Star(t *testing.T) {
	t.Parallel()

	// without parameter
	route := Route{
		star:        true,
		path:        "/*",
		routeParser: routeParser{},
	}
	params := [maxParams]string{}
	match := route.match("", "", &params, 0)
	require.True(t, match)
	require.Equal(t, [maxParams]string{}, params)

	// with parameter
	match = route.match("/favicon.ico", "/favicon.ico", &params, 1)
	require.True(t, match)
	require.Equal(t, [maxParams]string{"favicon.ico"}, params)

	// without parameter again
	match = route.match("", "", &params, 0)
	require.True(t, match)
	require.Equal(t, [maxParams]string{}, params)
}

func Test_Route_Match_Root(t *testing.T) {
	t.Parallel()

	app := New()

	app.Get("/", func(c Ctx) error {
		return c.SendString("root")
	})

	apptest.ExpectBody(t, app, "/", 200, "root")
}

func Test_Route_Match_Parser(t *testing.T) {
	t.Parallel()

	app := New()

	app.Get("/foo/:ParamName", func(c Ctx) error {
		return c.SendString(c.Params("ParamName"))
	})
	app.Get("/Foobar/*", func(c Ctx) error {
		return c.SendString(c.Params("*"))
	})
	apptest.ExpectBody(t, app, "/foo/bar", 200, "bar")

	// with star
	apptest.ExpectBody(t, app, "/Foobar/test", 200, "test")
}

func TestAutoRegisterHeadRoutes(t *testing.T) {
	t.Parallel()

	// expectHead sends HEAD for path, asserts the status and that no body comes
	// back, and returns the response for header checks.
	expectHead := func(t *testing.T, app *App, path string, status int) *http.Response {
		t.Helper()

		resp := apptest.Expect(t, app, apptest.Req(MethodHead, path), status)
		require.Empty(t, apptest.BodyBytes(t, resp))
		return resp
	}

	t.Run("auto registers head for get", func(t *testing.T) {
		t.Parallel()
		app := New()
		app.Get("/", func(c Ctx) error {
			c.Set("X-Test", "auto")
			return c.SendString("Hello")
		})

		respHead := expectHead(t, app, "/", StatusOK)
		require.Equal(t, int64(len("Hello")), respHead.ContentLength)
		require.Equal(t, "auto", respHead.Header.Get("X-Test"))

		respGet := apptest.ExpectBody(t, app, "/", StatusOK, "Hello")
		require.Equal(t, int64(len("Hello")), respGet.ContentLength)
	})

	t.Run("disable auto register config", func(t *testing.T) {
		t.Parallel()
		app := New(Config{DisableHeadAutoRegister: true})
		app.Get("/", func(c Ctx) error {
			return c.SendString("Hello")
		})

		routerTestExpect(t, app, apptest.Req(MethodHead, "/"), StatusMethodNotAllowed)
	})

	t.Run("explicit head overrides auto head route", func(t *testing.T) {
		t.Parallel()
		app := New()
		var getCalls int
		app.Get("/override", func(c Ctx) error {
			getCalls++
			return c.SendString("GET")
		})

		routerTestExpect(t, app, apptest.Req(MethodHead, "/override"), StatusOK)
		require.Equal(t, 1, getCalls)

		var headCalls int
		app.Head("/override", func(c Ctx) error {
			headCalls++
			c.Set("X-Explicit", "true")
			return c.SendStatus(StatusNoContent)
		})

		respHead := expectHead(t, app, "/override", StatusNoContent)
		require.Equal(t, "true", respHead.Header.Get("X-Explicit"))
		require.Equal(t, 1, getCalls)
		require.Equal(t, 1, headCalls)
	})

	t.Run("auto head for grouped routes", func(t *testing.T) {
		t.Parallel()
		app := New()
		group := app.Group("/api")
		group.Get("/users/:id", func(c Ctx) error {
			c.Set("X-User", c.Params("id"))
			return c.SendString("grouped")
		})

		respHead := expectHead(t, app, "/api/users/42", StatusOK)
		require.Equal(t, "42", respHead.Header.Get("X-User"))
	})

	t.Run("static handler auto head", func(t *testing.T) {
		t.Parallel()
		const file = "./.github/testdata/testRoutes.json"
		content, err := os.ReadFile(file)
		require.NoError(t, err)

		app := New()
		app.Get("/file", func(c Ctx) error {
			return c.SendFile(file)
		})

		respHead := expectHead(t, app, "/file", StatusOK)
		require.Equal(t, int64(len(content)), respHead.ContentLength)

		data := apptest.BodyBytes(t, apptest.Do(t, app, "/file"))
		require.Equal(t, content, data)
	})

	t.Run("head without matching route returns 404", func(t *testing.T) {
		t.Parallel()
		app := New()

		routerTestExpect(t, app, apptest.Req(MethodHead, "/missing"), StatusNotFound)
	})

	t.Run("late explicit get keeps explicit head", func(t *testing.T) {
		t.Parallel()
		app := New()
		var headCalls int
		app.Head("/late", func(c Ctx) error {
			headCalls++
			c.Set("X-Late", "head")
			return c.SendStatus(StatusAccepted)
		})

		var getCalls int
		app.Get("/late", func(c Ctx) error {
			getCalls++
			return c.SendString("ok")
		})

		respHead := routerTestExpect(t, app, apptest.Req(MethodHead, "/late"), StatusAccepted)
		require.Equal(t, "head", respHead.Header.Get("X-Late"))
		require.Equal(t, 1, headCalls)
		require.Equal(t, 0, getCalls)

		routerTestExpect(t, app, "/late", StatusOK)
		require.Equal(t, 1, getCalls)
	})

	t.Run("route listing includes auto head", func(t *testing.T) {
		t.Parallel()
		app := New()
		app.Get("/list", func(c Ctx) error {
			return c.SendString("list")
		})

		app.startupProcess()

		routes := app.GetRoutes()
		var hasGet, hasHead bool
		for _, route := range routes {
			if route.Path == "/list" {
				if route.Method == MethodGet {
					hasGet = true
				}
				if route.Method == MethodHead {
					hasHead = true
				}
			}
		}
		require.True(t, hasGet)
		require.True(t, hasHead)
	})

	t.Run("head mirrors status without body", func(t *testing.T) {
		t.Parallel()
		app := New()
		app.Get("/nocontent", func(c Ctx) error {
			return c.SendStatus(StatusNoContent)
		})

		expectHead(t, app, "/nocontent", StatusNoContent)

		routerTestExpect(t, app, "/nocontent", StatusNoContent)
	})
}

func Test_Route_Match_UnescapedPath(t *testing.T) {
	t.Parallel()

	app := New(Config{UnescapePath: true})

	app.Use("/créer", func(c Ctx) error {
		return c.SendString("test")
	})

	apptest.ExpectBody(t, app, "/cr%C3%A9er", StatusOK, "test")
	// without special chars
	apptest.Expect(t, app, "/créer", StatusOK)

	// check deactivated behavior
	app.config.UnescapePath = false
	apptest.Expect(t, app, "/cr%C3%A9er", StatusNotFound)

	// an encoded slash splits the segment only with the flag on
	app.config.UnescapePath = true
	app.Use("/cr/éer", func(c Ctx) error {
		return c.SendString("split")
	})
	apptest.ExpectBody(t, app, "/cr%2F%C3%A9er", StatusOK, "split")

	app.config.UnescapePath = false
	apptest.Expect(t, app, "/cr%2F%C3%A9er", StatusNotFound)
}

// Test_Route_Match_NormalizedPath pins the RFC 3986 normalization the router
// applies before matching, so a guard on a prefix sees the spellings of a path
// under it that name the same resource, and only those.
func Test_Route_Match_NormalizedPath(t *testing.T) {
	t.Parallel()

	newApp := func(cfg Config) *App {
		app := New(cfg)
		app.Use("/static/private", func(c Ctx) error {
			return c.SendStatus(StatusForbidden)
		})
		app.Get("/static/*", func(c Ctx) error {
			return c.SendString(c.Path() + "|" + c.Params("*"))
		})
		app.Get("/users/:id", func(c Ctx) error {
			return c.SendString(c.Params("id"))
		})
		// a name outside the unreserved set is routed in its encoded spelling
		app.Get("/enc/a%7Bb%7D", func(c Ctx) error {
			return c.SendString(c.Path())
		})
		return app
	}

	tests := []struct {
		name       string
		target     string
		wantBody   string
		wantStatus int
		unescape   bool
	}{
		{name: "plain", target: "/static/public.txt", wantStatus: StatusOK, wantBody: "/static/public.txt|public.txt"},
		{name: "guarded", target: "/static/private/secret.txt", wantStatus: StatusForbidden},
		{name: "encoded unreserved character", target: "/static/%70rivate/secret.txt", wantStatus: StatusForbidden},
		{name: "parent segment", target: "/static/x/../private/secret.txt", wantStatus: StatusForbidden},
		{name: "current segment", target: "/static/./private/secret.txt", wantStatus: StatusForbidden},
		{name: "empty segment is kept", target: "/static//private/secret.txt", wantStatus: StatusOK, wantBody: "/static//private/secret.txt|/private/secret.txt"},
		{name: "encoded dot segment", target: "/static/x/%2E%2E/private/secret.txt", wantStatus: StatusForbidden},
		{name: "parent segment after an empty one", target: "/static/x//../private/secret.txt", wantStatus: StatusOK, wantBody: "/static/x/private/secret.txt|x/private/secret.txt"},
		{name: "parent segment above the prefix", target: "/static/../static/private/secret.txt", wantStatus: StatusForbidden},
		{name: "parent segment leaves the route", target: "/static/../other", wantStatus: StatusNotFound},
		{name: "trailing current segment", target: "/users/john/.", wantStatus: StatusOK, wantBody: "john"},
		{name: "trailing parent segment", target: "/static/private/x/..", wantStatus: StatusForbidden},
		{name: "encoded unreserved characters in params", target: "/users/%6Aohn%2Edoe%7E", wantStatus: StatusOK, wantBody: "john.doe~"},
		{name: "encoded space kept", target: "/static/a%20b.txt", wantStatus: StatusOK, wantBody: "/static/a%20b.txt|a%20b.txt"},
		{name: "encoded space decoded under UnescapePath", target: "/static/a%20b.txt", unescape: true, wantStatus: StatusOK, wantBody: "/static/a b.txt|a b.txt"},
		{name: "non-ascii kept with uppercase hex digits", target: "/users/%c3%a9", wantStatus: StatusOK, wantBody: "%C3%A9"},
		{name: "non-ascii decoded under UnescapePath", target: "/users/%c3%a9", unescape: true, wantStatus: StatusOK, wantBody: "é"},
		{name: "hex case does not split a route", target: "/enc/a%7bb%7d", wantStatus: StatusOK, wantBody: "/enc/a%7Bb%7D"},
		{name: "encoded slash stays in the segment", target: "/users/a%2fb", wantStatus: StatusOK, wantBody: "a%2Fb"},
		{name: "encoded slash splits the segment under UnescapePath", target: "/users/a%2Fb", unescape: true, wantStatus: StatusNotFound},
		{name: "reserved character kept", target: "/users/a%40b", wantStatus: StatusOK, wantBody: "a%40b"},
		{name: "reserved character decoded under UnescapePath", target: "/users/a%40b", unescape: true, wantStatus: StatusOK, wantBody: "a@b"},
		{name: "percent sign decoded once", target: "/static/%2570rivate/x", wantStatus: StatusOK, wantBody: "/static/%2570rivate/x|%2570rivate/x"},
		{name: "percent sign decoded once under UnescapePath", target: "/static/%2570rivate/x", unescape: true, wantStatus: StatusOK, wantBody: "/static/%70rivate/x|%70rivate/x"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app := newApp(Config{UnescapePath: tc.unescape})
			routerTestExpectPath(t, app, tc.target, tc.wantStatus, tc.wantBody)
		})
	}
}

// Test_Route_Match_AgreesWithNetHTTP sends the same requests to a net/http
// ServeMux and to Fiber and expects the same route, path and parameters.
// ServeMux unescapes each segment of the escaped path and redirects a path
// with dot or empty segments to its clean form, which the harness routes as
// the redirect would. Three spellings are left out because the two routers
// disagree on purpose: ServeMux keeps "a%2Fb" one segment where UnescapePath
// splits it, it reads "%2E%2E" as a literal name where Fiber decodes the
// unreserved dots first and removes the segment, and it redirects "//a" to
// "/a" where Fiber keeps the empty segment RFC 3986 leaves in place.
func Test_Route_Match_AgreesWithNetHTTP(t *testing.T) {
	t.Parallel()

	routes := []struct {
		mux, fiber string
		params     []string // wildcard names; "rest" reads Fiber's "*"
	}{
		{mux: "/private/secret.txt", fiber: "/private/secret.txt"},
		{mux: "/créer", fiber: "/créer"},
		{mux: "/tag@2x", fiber: "/tag@2x"},
		{mux: "/users/{id}", fiber: "/users/:id", params: []string{"id"}},
		{mux: "/files/{rest...}", fiber: "/files/*", params: []string{"rest"}},
	}

	newMux := func() *http.ServeMux {
		mux := http.NewServeMux()
		for i, r := range routes {
			mux.HandleFunc("GET "+r.mux, func(w http.ResponseWriter, req *http.Request) {
				values := make([]string, len(r.params))
				for j, name := range r.params {
					values[j] = req.PathValue(name)
				}
				fmt.Fprintf(w, "%d %s %q", i, req.URL.Path, values)
			})
		}
		return mux
	}
	newFiber := func(unescape bool) *App {
		app := New(Config{UnescapePath: unescape, CaseSensitive: true, StrictRouting: true})
		for i, r := range routes {
			app.Get(r.fiber, func(c Ctx) error {
				values := make([]string, len(r.params))
				for j, name := range r.params {
					if name == "rest" {
						name = "*"
					}
					values[j] = c.Params(name)
				}
				return c.SendString(fmt.Sprintf("%d %s %q", i, c.Path(), values))
			})
		}
		return app
	}

	// viaMux routes target as ServeMux does: a path that is not clean is
	// answered with a redirect to its clean form (net/http's cleanPath), so
	// the clean form is what gets routed.
	viaMux := func(mux *http.ServeMux, target string) (int, string) {
		req := httptest.NewRequest(MethodGet, target, http.NoBody)
		escaped := req.URL.EscapedPath()
		clean := pathpkg.Clean(escaped)
		if strings.HasSuffix(escaped, "/") && clean != "/" {
			clean += "/"
		}
		if clean != escaped {
			req = httptest.NewRequest(MethodGet, clean, http.NoBody)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	viaFiber := func(app *App, target string) (int, string) {
		resp := apptest.Do(t, app, target)
		body := apptest.Body(t, resp)
		return resp.StatusCode, body
	}

	requests := []struct {
		target string
		// only escapes of unreserved characters are decoded without
		// UnescapePath, so these only agree with the flag on
		needsUnescape bool
	}{
		{target: "/private/secret.txt"},
		{target: "/%70rivate/secret.txt"},
		{target: "/x/../private/secret.txt"},
		{target: "/./private/secret.txt"},
		{target: "/private/x/../secret.txt"},
		{target: "/cr%C3%A9er", needsUnescape: true},
		{target: "/cr%c3%a9er", needsUnescape: true},
		{target: "/créer", needsUnescape: true},
		{target: "/users/%C3%A9", needsUnescape: true},
		{target: "/users/a%20b", needsUnescape: true},
		{target: "/users/a%40b", needsUnescape: true},
		{target: "/tag%402x", needsUnescape: true},
		{target: "/users/100%2525", needsUnescape: true},
		{target: "/users/%2570rivate", needsUnescape: true},
		{target: "/files/a/b/c.txt"},
		{target: "/files/a/../c.txt"},
		{target: "/files/%61/b"},
		{target: "/files/../private/secret.txt"},
		{target: "/users/"},
		{target: "/nope"},
	}

	for _, unescape := range []bool{false, true} {
		app := newFiber(unescape)
		mux := newMux()
		for _, tc := range requests {
			if tc.needsUnescape && !unescape {
				continue
			}
			t.Run(fmt.Sprintf("unescape=%v/%s", unescape, tc.target), func(t *testing.T) {
				t.Parallel()

				wantStatus, wantBody := viaMux(mux, tc.target)
				gotStatus, gotBody := viaFiber(app, tc.target)
				require.Equal(t, wantStatus, gotStatus, "status for %s", tc.target)
				if wantStatus == StatusOK {
					require.Equal(t, wantBody, gotBody, "route, path and params for %s", tc.target)
				}
			})
		}
	}
}

func Test_Route_Match_WithEscapeChar(t *testing.T) {
	t.Parallel()

	app := New()
	// static route and escaped part
	app.Get("/v1/some/resource/name\\:customVerb", func(c Ctx) error {
		return c.SendString("static")
	})
	// group route
	group := app.Group("/v2/\\:firstVerb")
	group.Get("/\\:customVerb", func(c Ctx) error {
		return c.SendString("group")
	})
	// route with resource param and escaped part
	app.Get("/v3/:resource/name\\:customVerb", func(c Ctx) error {
		return c.SendString(c.Params("resource"))
	})

	// check static route
	apptest.ExpectBody(t, app, "/v1/some/resource/name:customVerb", StatusOK, "static")

	// check group route
	apptest.ExpectBody(t, app, "/v2/:firstVerb/:customVerb", StatusOK, "group")

	// check param route
	apptest.ExpectBody(t, app, "/v3/awesome/name:customVerb", StatusOK, "awesome")
}

// Test_Route_Match_Middleware_Prefix pins which paths a Use prefix covers:
// everything below it, but not a sibling that only starts with the same characters.
func Test_Route_Match_Middleware_Prefix(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		prefix string
		path   string
		body   string
		status int
	}{
		{name: "has prefix", prefix: "/foo", path: "/foo/bar", status: StatusOK, body: "middleware"},
		{name: "no boundary", prefix: "/foo", path: "/foobar", status: StatusNotFound},
		{name: "root", prefix: "/", path: "/everything", status: StatusOK, body: "middleware"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app := New()

			app.Use(tc.prefix, func(c Ctx) error {
				return c.SendString("middleware")
			})

			routerTestExpectPath(t, app, tc.path, tc.status, tc.body)
		})
	}
}

func Test_Router_Register_Missing_Handler(t *testing.T) {
	t.Parallel()

	app := New()

	t.Run("No Handler", func(t *testing.T) {
		t.Parallel()

		require.PanicsWithValue(t, "missing handler/middleware in route: /doe\n", func() {
			app.register([]string{"USE"}, "/doe", nil)
		})
	})

	t.Run("Nil Handler", func(t *testing.T) {
		t.Parallel()

		require.PanicsWithValue(t, "nil handler in route: /doe\n", func() {
			app.register([]string{"USE"}, "/doe", nil, nil)
		})
	})
}

func Test_Ensure_Router_Interface_Implementation(t *testing.T) {
	t.Parallel()

	var app any = (*App)(nil)
	_, ok := app.(Router)
	require.True(t, ok)

	var group any = (*Group)(nil)
	_, ok = group.(Router)
	require.True(t, ok)
}

func Test_Router_Handler_Catch_Error(t *testing.T) {
	t.Parallel()

	app := New()
	app.config.ErrorHandler = func(_ Ctx, _ error) error {
		return errors.New("fake error")
	}

	app.Get("/", func(_ Ctx) error {
		return ErrForbidden
	})

	c := &fasthttp.RequestCtx{}

	app.Handler()(c)

	require.Equal(t, StatusInternalServerError, c.Response.Header.StatusCode())
}

func Test_Router_NotFound(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		path string
	}{
		{name: "unknown route", path: "/this/route/does/not/exist"},
		// the body is a fixed message, so markup in the path is never echoed back
		{name: "HTML_Inject", path: "/does/not/exist<script>alert('foo');</script>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := New()
			app.Use(func(c Ctx) error {
				return c.Next()
			})
			appHandler := app.Handler()
			c := &fasthttp.RequestCtx{}

			c.Request.Header.SetMethod("DELETE")
			c.URI().SetPath(tc.path)

			appHandler(c)

			require.Equal(t, 404, c.Response.StatusCode())
			require.Equal(t, "Not Found", string(c.Response.Body()))
		})
	}
}

func registerTreeManipulationRoutes(app *App, middleware ...func(Ctx) error) {
	converted := make([]any, len(middleware))
	for i, h := range middleware {
		converted[i] = h
	}

	app.Get("/test", func(c Ctx) error {
		app.Get("/dynamically-defined", func(c Ctx) error {
			return c.SendStatus(StatusOK)
		})

		app.RebuildTree()

		return c.SendStatus(StatusOK)
	}, converted...)
}

func verifyRouteHandlerCounts(tb testing.TB, app *App, expectedRoutesCount int) {
	tb.Helper()

	//  this is taken from listen.go's printRoutesMessage app method
	var routes []RouteMessage
	for _, routeStack := range app.stack {
		for _, route := range routeStack {
			routeMsg := RouteMessage{
				name:   route.Name,
				method: route.Method,
				path:   route.Path,
			}

			for _, handler := range route.Handlers {
				routeMsg.handlers += runtime.FuncForPC(reflect.ValueOf(handler).Pointer()).Name() + " "
			}

			routes = append(routes, routeMsg)
		}
	}

	for _, route := range routes {
		require.Equal(tb, expectedRoutesCount, strings.Count(route.handlers, " "))
	}
}

func verifyThereAreNoRoutes(tb testing.TB, app *App) {
	tb.Helper()

	require.Equal(tb, uint32(0), app.handlersCount)
	verifyRouteHandlerCounts(tb, app, 0)
}

func Test_App_Rebuild_Tree(t *testing.T) {
	t.Parallel()
	app := New()

	registerTreeManipulationRoutes(app)

	apptest.Expect(t, app, "/dynamically-defined", StatusNotFound)
	apptest.Expect(t, app, "/test", StatusOK)
	apptest.Expect(t, app, "/dynamically-defined", StatusOK)
}

func Test_App_Remove_Route_A_B_Feature_Testing(t *testing.T) {
	t.Parallel()
	app := New()

	features := []string{"a", "b"}
	for _, feature := range features {
		app.Get("/api/feature-"+feature, func(c Ctx) error {
			app.RemoveRoute("/api/feature", MethodGet)
			app.RebuildTree()
			// Redefine route
			app.Get("/api/feature", func(c Ctx) error {
				return c.SendString("Testing feature-" + feature)
			})

			app.RebuildTree()
			return c.SendStatus(StatusOK)
		})
	}

	// Each request swaps /api/feature for its own version, so the order matters.
	for _, feature := range features {
		apptest.Expect(t, app, "/api/feature-"+feature, StatusOK)

		apptest.ExpectBody(t, app, "/api/feature", StatusOK, "Testing feature-"+feature)
	}
}

func Test_App_Remove_Route_By_Name(t *testing.T) {
	t.Parallel()
	app := New()

	app.Get("/api/test", func(c Ctx) error {
		return c.SendStatus(StatusOK)
	}).Name("test")

	app.RemoveRouteByName("test", MethodGet)
	app.RebuildTree()

	apptest.Expect(t, app, "/test", StatusNotFound)
	verifyThereAreNoRoutes(t, app)
}

func Test_App_Remove_Route_By_Name_Non_Existing_Route(t *testing.T) {
	t.Parallel()
	app := New()

	app.RemoveRouteByName("test", MethodGet)
	app.RebuildTree()

	verifyThereAreNoRoutes(t, app)
}

func Test_App_Remove_Route_Nested(t *testing.T) {
	t.Parallel()
	app := New()

	api := app.Group("/api")

	v1 := api.Group("/v1")
	v1.Get("/test", func(c Ctx) error {
		return c.SendStatus(StatusOK)
	})

	apptest.Expect(t, app, "/api/v1/test", StatusOK)
	app.RemoveRoute("/api/v1/test", MethodGet)

	verifyThereAreNoRoutes(t, app)
}

func Test_App_Remove_Route_Parameterized(t *testing.T) {
	t.Parallel()
	app := New()

	app.Get("/test/:id", func(c Ctx) error {
		return c.SendStatus(StatusOK)
	})
	apptest.Expect(t, app, "/test/:id", StatusOK)
	app.RemoveRoute("/test/:id", MethodGet)

	verifyThereAreNoRoutes(t, app)
}

func Test_App_Remove_Route(t *testing.T) {
	t.Parallel()
	app := New()

	app.Get("/test", func(c Ctx) error {
		return c.SendStatus(StatusOK)
	})

	app.RemoveRoute("/test", MethodGet)
	app.RebuildTree()

	apptest.Expect(t, app, "/test", StatusNotFound)
}

func Test_App_Remove_Route_Non_Existing_Route(t *testing.T) {
	t.Parallel()
	app := New()

	app.RemoveRoute("/test", MethodGet, MethodHead)
	app.RebuildTree()

	verifyThereAreNoRoutes(t, app)
}

// Test_Use_StrictRoutingBoundary pins where a Use prefix stops matching, for a
// middleware on the app and on a group: the prefix itself, a trailing slash and
// anything below it match, but a sibling that only starts with the same
// characters never does, with StrictRouting on or off.
func Test_Use_StrictRoutingBoundary(t *testing.T) {
	t.Parallel()

	for _, scope := range []struct {
		register func(app *App, matched *bool)
		name     string
		prefix   string // the path the middleware covers
		sibling  string // shares the prefix's characters without being below it
	}{
		{
			name:    "app",
			prefix:  "/api",
			sibling: "/apiv1",
			register: func(app *App, matched *bool) {
				app.Use("/api", func(c Ctx) error {
					*matched = true
					return c.SendStatus(StatusOK)
				})
			},
		},
		{
			name:    "group",
			prefix:  "/api/v1",
			sibling: "/api/v1beta",
			register: func(app *App, matched *bool) {
				grp := app.Group("/api")
				grp.Use("/v1", func(c Ctx) error {
					*matched = true
					return c.Next()
				})
				grp.Get("/v1", func(c Ctx) error {
					return c.SendStatus(StatusOK)
				})
				grp.Get("/v1/*", func(c Ctx) error {
					return c.SendStatus(StatusOK)
				})
			},
		},
	} {
		cases := []struct {
			name    string
			path    string
			status  int
			matched bool
		}{
			{name: "exact match", path: scope.prefix, status: StatusOK, matched: true},
			{name: "trailing slash partial", path: scope.prefix + "/", status: StatusOK, matched: true},
			{name: "nested partial", path: scope.prefix + "/users", status: StatusOK, matched: true},
			{name: "disallows sibling prefix", path: scope.sibling, status: StatusNotFound, matched: false},
		}

		for _, strict := range []bool{true, false} {
			for _, tc := range cases {
				t.Run(fmt.Sprintf("%s/strict=%v/%s", scope.name, strict, tc.name), func(t *testing.T) {
					t.Parallel()

					app := New(Config{StrictRouting: strict})

					matched := false
					scope.register(app, &matched)

					apptest.Expect(t, app, tc.path, tc.status)
					require.Equal(t, tc.matched, matched)
				})
			}
		}
	}
}

func Test_App_Remove_Route_Concurrent(t *testing.T) {
	t.Parallel()
	app := New()

	// Add test route
	app.Get("/test", func(c Ctx) error {
		return c.SendStatus(StatusOK)
	})

	// Concurrently remove and add routes
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			app.RemoveRoute("/test", MethodGet)
			app.Get("/test", func(c Ctx) error {
				return c.SendStatus(StatusOK)
			})
		})
	}
	wg.Wait()

	// Verify final state
	app.RebuildTree()
	apptest.Expect(t, app, "/test", StatusOK)
}

func Test_Route_Registration_Prevent_Duplicate_With_Middleware(t *testing.T) {
	t.Parallel()
	app := New()

	middleware := func(c Ctx) error {
		return c.Next()
	}

	registerTreeManipulationRoutes(app, middleware)
	registerTreeManipulationRoutes(app)

	apptest.Expect(t, app, "/dynamically-defined", StatusNotFound)
	require.Equal(t, uint32(6), app.handlersCount)

	// RebuildTree registers the automatic HEAD companion right away, hence two.
	apptest.Expect(t, app, "/test", StatusOK)
	require.Equal(t, uint32(8), app.handlersCount)

	apptest.Expect(t, app, "/dynamically-defined", StatusOK)
	require.Equal(t, uint32(8), app.handlersCount)

	apptest.Expect(t, app, "/test", StatusOK)
	require.Equal(t, uint32(9), app.handlersCount)

	apptest.Expect(t, app, "/dynamically-defined", StatusOK)
	require.Equal(t, uint32(9), app.handlersCount)
}

func TestNormalizePath(t *testing.T) {
	tests := []struct {
		name          string
		path          string
		expected      string
		caseSensitive bool
		strictRouting bool
	}{
		{
			name:          "Empty path",
			path:          "",
			caseSensitive: true,
			strictRouting: true,
			expected:      "/",
		},
		{
			name:          "No leading slash",
			path:          "users",
			caseSensitive: true,
			strictRouting: true,
			expected:      "/users",
		},
		{
			name:          "With trailing slash and strict routing",
			path:          "/users/",
			caseSensitive: true,
			strictRouting: true,
			expected:      "/users/",
		},
		{
			name:          "With trailing slash and non-strict routing",
			path:          "/users/",
			caseSensitive: true,
			strictRouting: false,
			expected:      "/users",
		},
		{
			name:          "Case sensitive",
			path:          "/Users",
			caseSensitive: true,
			strictRouting: true,
			expected:      "/Users",
		},
		{
			name:          "Case insensitive",
			path:          "/Users",
			caseSensitive: false,
			strictRouting: true,
			expected:      "/users",
		},
		{
			name:          "With escape characters",
			path:          "/users\\/profile",
			caseSensitive: true,
			strictRouting: true,
			expected:      "/users/profile",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := &App{
				config: Config{
					CaseSensitive: tt.caseSensitive,
					StrictRouting: tt.strictRouting,
				},
			}
			result := app.normalizePath(tt.path)
			require.Equal(t, tt.expected, result)
		})
	}
}

func TestRemoveRoute(t *testing.T) {
	app := New()

	var buf strings.Builder

	app.Use(func(c Ctx) error {
		buf.WriteString("1") //nolint:errcheck // not needed
		return c.Next()
	})

	app.Post("/", func(c Ctx) error {
		buf.WriteString("2") //nolint:errcheck // not needed
		return c.SendStatus(StatusOK)
	})

	app.Use("/test", func(c Ctx) error {
		buf.WriteString("3") //nolint:errcheck // not needed
		return c.Next()
	})

	app.Get("/test", func(c Ctx) error {
		buf.WriteString("4") //nolint:errcheck // not needed
		return c.SendStatus(StatusOK)
	})

	app.Post("/test", func(c Ctx) error {
		buf.WriteString("5") //nolint:errcheck // not needed
		return c.SendStatus(StatusOK)
	})

	app.startupProcess()

	require.Equal(t, uint32(6), app.handlersCount)

	apptest.Expect(t, app, apptest.Req(MethodPost, "/"), 200)
	require.Equal(t, "12", buf.String())

	buf.Reset()

	apptest.Expect(t, app, apptest.Req(MethodGet, "/test"), 200)
	require.Equal(t, "134", buf.String())

	buf.Reset()

	require.Equal(t, uint32(6), app.handlersCount)

	app.RemoveRoute("/test", MethodGet)
	app.RebuildTree()

	app.RemoveRoute("/test", "TEST")
	app.RebuildTree()

	app.RemoveRouteFunc(func(_ *Route) bool {
		return false
	}, MethodGet)

	resp := apptest.Do(t, app, apptest.Req(MethodGet, "/test"))

	buf.Reset()

	require.Equal(t, StatusMethodNotAllowed, resp.StatusCode)

	require.Equal(t, uint32(4), app.handlersCount)

	app.RemoveRoute("/test", MethodPost)
	app.RebuildTree()

	require.Equal(t, uint32(3), app.handlersCount)

	apptest.Expect(t, app, apptest.Req(MethodPost, "/test"), 404)
	require.Equal(t, "1", buf.String())

	buf.Reset()

	apptest.Expect(t, app, apptest.Req(MethodGet, "/test"), 404)
	require.Equal(t, "1", buf.String())

	buf.Reset()

	app.RemoveRoute("/", MethodGet, MethodPost)

	require.Equal(t, uint32(2), app.handlersCount)

	apptest.Expect(t, app, apptest.Req(MethodGet, "/"), 404)
	require.Empty(t, buf.String())

	buf.Reset()

	app.RemoveRoute("/test", MethodGet, MethodPost)

	require.Equal(t, uint32(2), app.handlersCount)

	app.RemoveRoute("/test", app.config.RequestMethods...)

	require.Equal(t, uint32(1), app.handlersCount)

	app.Patch("/test", func(c Ctx) error {
		buf.WriteString("6") //nolint:errcheck // not needed
		return c.SendStatus(StatusOK)
	})

	require.Equal(t, uint32(2), app.handlersCount)

	app.RemoveRoute("/test")
	app.RemoveRoute("/")
	app.RebuildTree()

	require.Equal(t, uint32(0), app.handlersCount)
}

//////////////////////////////////////////////
///////////////// BENCHMARKS /////////////////
//////////////////////////////////////////////

func registerDummyRoutes(app *App) {
	h := func(_ Ctx) error {
		return nil
	}
	for _, r := range routesFixture.GitHubAPI {
		app.Add([]string{r.Method}, r.Path, h)
	}
}

// acquireDefaultCtxForRouterBenchmark acquires a *DefaultCtx inside a benchmark
// loop. It skips b.Helper(), whose stack walk was a quarter of the samples.
//
//nolint:thelper // b.Helper() is the cost being avoided
func acquireDefaultCtxForRouterBenchmark(b *testing.B, app *App, fctx *fasthttp.RequestCtx) *DefaultCtx {
	ctx := app.AcquireCtx(fctx)
	defaultCtx, ok := ctx.(*DefaultCtx)
	if !ok {
		b.Fatal("AcquireCtx did not return *DefaultCtx")
	}
	return defaultCtx
}

func Benchmark_App_RebuildTree(b *testing.B) {
	app := New()
	registerDummyRoutes(app)

	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		app.hasRoutesRefreshed = true
		app.RebuildTree()
	}
}

func Benchmark_App_MethodNotAllowed(b *testing.B) {
	app := New()
	h := func(c Ctx) error {
		return c.SendString("Hello World!")
	}
	app.All("/this/is/a/", h)
	app.Get("/this/is/a/dummy/route/oke", h)
	appHandler := app.Handler()
	c := &fasthttp.RequestCtx{}

	c.Request.Header.SetMethod("DELETE")
	c.URI().SetPath("/this/is/a/dummy/route/oke")

	for b.Loop() {
		appHandler(c)
	}
	require.Equal(b, 405, c.Response.StatusCode())
	require.Equal(b, MethodGet+", "+MethodHead, string(c.Response.Header.Peek("Allow")))
	require.Equal(b, utils.StatusMessage(StatusMethodNotAllowed), string(c.Response.Body()))
}

func Benchmark_Router_NotFound(b *testing.B) {
	b.ReportAllocs()
	app := New()
	app.Use(func(c Ctx) error {
		return c.Next()
	})
	registerDummyRoutes(app)
	appHandler := app.Handler()
	c := &fasthttp.RequestCtx{}

	c.Request.Header.SetMethod("DELETE")
	c.URI().SetPath("/this/route/does/not/exist")

	for b.Loop() {
		appHandler(c)
	}
	require.Equal(b, 404, c.Response.StatusCode())
	require.Equal(b, "Not Found", string(c.Response.Body()))
}

func Benchmark_Router_OptionalMiss(b *testing.B) {
	for _, tc := range []struct {
		name     string
		prefix   string
		path     string
		patterns []string
	}{
		{"lang", "/:lang?", "/en/pricing", []string{"/about", "/contact", "/docs", "/blog", "/support"}},
		{"version", "/api/:version?", "/api/v1/pricing", []string{"/users", "/teams", "/projects", "/settings"}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			app := New()
			handler := func(c Ctx) error { return c.SendStatus(StatusNoContent) }
			for _, pattern := range tc.patterns {
				app.Get(tc.prefix+pattern, handler)
			}
			app.Get(tc.path, handler)
			appHandler := app.Handler()
			ctx := &fasthttp.RequestCtx{}
			ctx.Request.Header.SetMethod(MethodGet)
			ctx.URI().SetPath(tc.path)
			appHandler(ctx)
			require.Equal(b, StatusNoContent, ctx.Response.StatusCode())
			b.ReportAllocs()
			for b.Loop() {
				appHandler(ctx)
			}
			require.Equal(b, StatusNoContent, ctx.Response.StatusCode())
		})
	}
}

func Benchmark_Router_Handler(b *testing.B) {
	app := New()
	registerDummyRoutes(app)
	appHandler := app.Handler()

	c := &fasthttp.RequestCtx{}

	c.Request.Header.SetMethod("DELETE")
	c.URI().SetPath("/user/keys/1337")

	for b.Loop() {
		appHandler(c)
	}
}

func Benchmark_Router_Handler_Strict_Case(b *testing.B) {
	app := New(Config{
		StrictRouting: true,
		CaseSensitive: true,
	})
	registerDummyRoutes(app)
	appHandler := app.Handler()

	c := &fasthttp.RequestCtx{}

	c.Request.Header.SetMethod("DELETE")
	c.URI().SetPath("/user/keys/1337")

	for b.Loop() {
		appHandler(c)
	}
}

func Benchmark_Router_Chain(b *testing.B) {
	app := New()
	handler := func(c Ctx) error {
		return c.Next()
	}
	app.Get("/", handler, handler, handler, handler, handler, handler)

	appHandler := app.Handler()

	c := &fasthttp.RequestCtx{}

	c.Request.Header.SetMethod(MethodGet)
	c.URI().SetPath("/")
	for b.Loop() {
		appHandler(c)
	}
}

// Benchmark_Router_Chain_FilteredBucket runs middleware chains in front of an
// endpoint whose bucket holds enough routes to carry a filter, so each hop
// scans a filtered bucket from the middleware it left:
//   - use: eight middlewares registered between routes of other buckets,
//     which keeps them from merging into one route, so that each hop takes
//     its first candidate
//   - use_interleaved: the same with the routes in the middlewares' bucket,
//     so that each hop steps over one
//   - group: a global middleware, then two nested groups' middlewares
func Benchmark_Router_Chain_FilteredBucket(b *testing.B) {
	handler := func(Ctx) error { return nil }
	mw := func(c Ctx) error { return c.Next() }
	for _, bench := range []struct {
		register func(app *App)
		name     string
	}{
		{name: "use", register: func(app *App) {
			for i := range 8 {
				app.Use("/api", mw)
				app.Get("/web/r"+strconv.Itoa(i), handler)
			}
			app.Get("/api/v1/users", handler)
		}},
		{name: "use_interleaved", register: func(app *App) {
			for i := range 8 {
				app.Use("/api", mw)
				app.Get("/api/v1/r"+strconv.Itoa(i), handler)
			}
			app.Get("/api/v1/users", handler)
		}},
		{name: "group", register: func(app *App) {
			app.Use(mw)
			v1 := app.Group("/api", mw).Group("/v1", mw)
			for i := range 8 {
				v1.Get("/r"+strconv.Itoa(i), handler)
			}
			v1.Get("/users", handler)
		}},
	} {
		b.Run(bench.name, func(b *testing.B) {
			app := New()
			bench.register(app)
			appHandler := app.Handler()
			c := &fasthttp.RequestCtx{}
			c.Request.Header.SetMethod(MethodGet)
			c.URI().SetPath("/api/v1/users")
			appHandler(c)
			require.Equal(b, StatusOK, c.Response.StatusCode())
			for b.Loop() {
				appHandler(c)
			}
		})
	}
}

func Benchmark_Router_WithCompression(b *testing.B) {
	app := New()
	handler := func(c Ctx) error {
		return c.Next()
	}
	app.Get("/", handler)
	app.Get("/", handler)
	app.Get("/", handler)
	app.Get("/", handler)
	app.Get("/", handler)
	app.Get("/", handler)

	appHandler := app.Handler()
	c := &fasthttp.RequestCtx{}

	c.Request.Header.SetMethod(MethodGet)
	c.URI().SetPath("/")
	for b.Loop() {
		appHandler(c)
	}
}

func Benchmark_Startup_Process(b *testing.B) {
	for b.Loop() {
		app := New()
		registerDummyRoutes(app)
		app.startupProcess()
	}
}

func Benchmark_Router_Next(b *testing.B) {
	app := New()
	registerDummyRoutes(app)
	app.startupProcess()

	request := &fasthttp.RequestCtx{}

	request.Request.Header.SetMethod("DELETE")
	request.URI().SetPath("/user/keys/1337")
	var res bool
	var err error

	c := app.AcquireCtx(request).(*DefaultCtx) //nolint:errcheck,forcetypeassert // not needed

	for b.Loop() {
		c.indexRoute = -1
		res, err = app.next(c)
	}
	require.NoError(b, err)
	require.True(b, res)
	require.Equal(b, 4, c.indexRoute)
}

func Benchmark_Router_Next_Default(b *testing.B) {
	app := New()
	app.Get("/", func(_ Ctx) error {
		return nil
	})

	h := app.Handler()

	fctx := &fasthttp.RequestCtx{}
	fctx.Request.Header.SetMethod(MethodGet)
	fctx.Request.SetRequestURI("/")

	b.ReportAllocs()

	for b.Loop() {
		h(fctx)
	}
}

func Benchmark_Router_Next_Default_Parallel(b *testing.B) {
	app := New()
	app.Get("/", func(_ Ctx) error {
		return nil
	})

	h := app.Handler()

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		fctx := &fasthttp.RequestCtx{}
		fctx.Request.Header.SetMethod(MethodGet)
		fctx.Request.SetRequestURI("/")

		for pb.Next() {
			h(fctx)
		}
	})
}

func Benchmark_Router_Next_Default_Immutable(b *testing.B) {
	app := New(Config{Immutable: true})
	app.Get("/", func(_ Ctx) error {
		return nil
	})

	h := app.Handler()

	fctx := &fasthttp.RequestCtx{}
	fctx.Request.Header.SetMethod(MethodGet)
	fctx.Request.SetRequestURI("/")

	b.ReportAllocs()

	for b.Loop() {
		h(fctx)
	}
}

func Benchmark_Router_Next_Default_Parallel_Immutable(b *testing.B) {
	app := New(Config{Immutable: true})
	app.Get("/", func(_ Ctx) error {
		return nil
	})

	h := app.Handler()

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		fctx := &fasthttp.RequestCtx{}
		fctx.Request.Header.SetMethod(MethodGet)
		fctx.Request.SetRequestURI("/")

		for pb.Next() {
			h(fctx)
		}
	})
}

func Benchmark_Route_Match(b *testing.B) {
	var match bool
	var params [maxParams]string

	parsed := parseRoute("/user/keys/:id", regexp.MustCompile)
	route := &Route{
		use:         false,
		root:        false,
		star:        false,
		routeParser: parsed,
		Params:      parsed.params,
		path:        "/user/keys/:id",

		Path:   "/user/keys/:id",
		Method: "DELETE",
	}
	route.Handlers = append(route.Handlers, func(_ Ctx) error {
		return nil
	})
	for b.Loop() {
		match = route.match("/user/keys/1337", "/user/keys/1337", &params, 3)
	}

	require.True(b, match)
	require.Equal(b, []string{"1337"}, params[0:len(parsed.params)])
}

func Benchmark_Route_Match_Star(b *testing.B) {
	var match bool
	var params [maxParams]string

	parsed := parseRoute("/*", regexp.MustCompile)
	route := &Route{
		use:         false,
		root:        false,
		star:        true,
		routeParser: parsed,
		Params:      parsed.params,
		path:        "/user/keys/bla",

		Path:   "/user/keys/bla",
		Method: "DELETE",
	}
	route.Handlers = append(route.Handlers, func(_ Ctx) error {
		return nil
	})

	for b.Loop() {
		match = route.match("/user/keys/bla", "/user/keys/bla", &params, 3)
	}

	require.True(b, match)
	require.Equal(b, []string{"user/keys/bla"}, params[0:len(parsed.params)])
}

func Benchmark_Route_Match_Root(b *testing.B) {
	var match bool
	var params [maxParams]string

	parsed := parseRoute("/", regexp.MustCompile)
	route := &Route{
		use:         false,
		root:        true,
		star:        false,
		path:        "/",
		routeParser: parsed,
		Params:      parsed.params,

		Path:   "/",
		Method: "DELETE",
	}
	route.Handlers = append(route.Handlers, func(_ Ctx) error {
		return nil
	})

	for b.Loop() {
		match = route.match("/", "/", &params, 1)
	}

	require.True(b, match)
	require.Equal(b, []string{}, params[0:len(parsed.params)])
}

func Benchmark_Router_Handler_CaseSensitive(b *testing.B) {
	app := New()
	app.config.CaseSensitive = true
	registerDummyRoutes(app)
	appHandler := app.Handler()

	c := &fasthttp.RequestCtx{}

	c.Request.Header.SetMethod("DELETE")
	c.URI().SetPath("/user/keys/1337")

	for b.Loop() {
		appHandler(c)
	}
}

func Benchmark_Router_Handler_Unescape(b *testing.B) {
	app := New()
	app.config.UnescapePath = true
	registerDummyRoutes(app)
	app.Delete("/créer", func(_ Ctx) error {
		return nil
	})

	appHandler := app.Handler()

	c := &fasthttp.RequestCtx{}

	c.Request.Header.SetMethod(MethodDelete)
	c.URI().SetPath("/cr%C3%A9er")

	for b.Loop() {
		c.URI().SetPath("/cr%C3%A9er")
		appHandler(c)
	}
}

func Benchmark_Router_Handler_StrictRouting(b *testing.B) {
	app := New()
	app.config.CaseSensitive = true
	registerDummyRoutes(app)
	appHandler := app.Handler()

	c := &fasthttp.RequestCtx{}

	c.Request.Header.SetMethod("DELETE")
	c.URI().SetPath("/user/keys/1337")

	for b.Loop() {
		appHandler(c)
	}
}

func Benchmark_Router_GitHub_API(b *testing.B) {
	app := New()
	registerDummyRoutes(app)
	app.startupProcess()

	c := &fasthttp.RequestCtx{}
	var match bool
	var err error

	b.ResetTimer()
	for i := range routesFixture.TestRoutes {
		b.RunParallel(func(pb *testing.PB) {
			c.Request.Header.SetMethod(routesFixture.TestRoutes[i].Method)
			for pb.Next() {
				c.URI().SetPath(routesFixture.TestRoutes[i].Path)

				ctx := app.AcquireCtx(c).(*DefaultCtx) //nolint:errcheck,forcetypeassert // not needed

				match, err = app.next(ctx)
				app.ReleaseCtx(ctx)
			}
		})

		require.NoError(b, err)
		require.True(b, match)
	}
}

type testRoute struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

type routeJSON struct {
	TestRoutes []testRoute `json:"test_routes"`
	GitHubAPI  []testRoute `json:"github_api"`
}

// newCustomApp returns an app that serves every request with a customCtx.
func newCustomApp(cfg ...Config) *App {
	return NewWithCustomCtx(func(app *App) CustomCtx {
		return &customCtx{DefaultCtx: *NewDefaultCtx(app)}
	}, cfg...)
}

func Test_Next_SkipNonUseRoutesSkipsFallback(t *testing.T) {
	t.Parallel()

	app := New()
	app.Get("/foo", func(c Ctx) error { return c.SendStatus(StatusOK) })
	app.startupProcess()

	fctx := routerTestRequestCtx(MethodPost, "/foo")

	ctx := app.AcquireCtx(fctx).(*DefaultCtx) //nolint:errcheck,forcetypeassert // default app returns DefaultCtx
	ctx.setSkipNonUseRoutes(true)
	defer app.ReleaseCtx(ctx)

	matched, err := app.next(ctx)
	require.False(t, matched)
	require.NoError(t, err)
	require.Empty(t, ctx.Response().Header.Peek(HeaderAllow))
}

func Test_NextCustom_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	app := newCustomApp()
	app.Get("/foo", func(c Ctx) error { return c.SendStatus(StatusOK) })
	useRoute := &Route{use: true, path: "/foo", Path: "/foo", routeParser: parseRoute("/foo", regexp.MustCompile)}
	m := app.methodInt(MethodGet)
	app.stack[m] = append([]*Route{useRoute}, app.stack[m]...)
	app.hasRoutesRefreshed = true
	app.ensureAutoHeadRoutes()
	app.RebuildTree()

	ctx := apptest.CtxFor(t, app, routerTestRequestCtx(MethodPost, "/foo"))

	matched, err := app.nextCustom(ctx)
	require.False(t, matched)
	require.ErrorIs(t, err, ErrMethodNotAllowed)
	allow := string(ctx.Response().Header.Peek(HeaderAllow))
	require.Equal(t, "GET, HEAD", allow)
}

func Test_NextCustom_SkipNonUseRoutesSkipsFallback(t *testing.T) {
	t.Parallel()

	app := newCustomApp()
	app.Get("/foo", func(c Ctx) error { return c.SendStatus(StatusOK) })
	app.startupProcess()

	ctx := apptest.CtxFor(t, app, routerTestRequestCtx(MethodPost, "/foo"))
	ctx.setSkipNonUseRoutes(true)

	matched, err := app.nextCustom(ctx)
	require.False(t, matched)
	require.NoError(t, err)
	require.Empty(t, ctx.Response().Header.Peek(HeaderAllow))
}

func Test_NextCustom_NotFound(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		uri     string
		matched bool
	}{
		{name: "no route", uri: "/not-exist"},
		{name: "matched earlier", uri: "/none", matched: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := newCustomApp()
			app.RebuildTree()

			ctx := apptest.CtxFor(t, app, routerTestRequestCtx(MethodGet, tc.uri))
			if tc.matched {
				ctx.setMatched(true)
			}

			matched, err := app.nextCustom(ctx)
			require.False(t, matched)
			var e *Error
			require.ErrorAs(t, err, &e)
			require.Equal(t, StatusNotFound, e.Code)
		})
	}
}

func Test_RequestHandler_CustomCtx_NotImplemented(t *testing.T) {
	t.Parallel()
	app := newCustomApp()

	h := app.Handler()
	fctx := routerTestRequestCtx("UNKNOWN", "/")

	h(fctx)
	require.Equal(t, StatusNotImplemented, fctx.Response.StatusCode())
}

func Test_NextCustom_SkipMountAndNoHandlers(t *testing.T) {
	t.Parallel()
	app := newCustomApp()
	m := app.methodInt(MethodGet)
	mountR := &Route{path: "/skip", Path: "/skip", routeParser: parseRoute("/skip", regexp.MustCompile), mount: true}
	empty := &Route{path: "/foo", Path: "/foo", routeParser: parseRoute("/foo", regexp.MustCompile)}
	app.stack[m] = []*Route{mountR, empty}
	app.hasRoutesRefreshed = true
	app.RebuildTree()

	ctx := apptest.CtxFor(t, app, routerTestRequestCtx(MethodGet, "/foo"))

	matched, err := app.nextCustom(ctx)
	require.True(t, matched)
	require.NoError(t, err)
	require.Equal(t, "/foo", ctx.Route().Path)
}

func Test_AddRoute_MergeHandlers(t *testing.T) {
	t.Parallel()
	app := New()
	count := func(_ Ctx) error { return nil }
	app.Get("/merge", count)
	app.Get("/merge", count)

	require.Len(t, app.stack[app.methodInt(MethodGet)], 1)
	require.Len(t, app.stack[app.methodInt(MethodGet)][0].Handlers, 2)
}

func Benchmark_App_RebuildTree_Parallel(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		// Each worker gets its own App instance to avoid data races on shared state
		localApp := New()
		registerDummyRoutes(localApp)
		for pb.Next() {
			localApp.hasRoutesRefreshed = true
			localApp.RebuildTree()
		}
	})
}

func Benchmark_App_MethodNotAllowed_Parallel(b *testing.B) {
	app := New()
	h := func(c Ctx) error {
		return c.SendString("Hello World!")
	}
	app.All("/this/is/a/", h)
	app.Get("/this/is/a/dummy/route/oke", h)
	appHandler := app.Handler()
	b.RunParallel(func(pb *testing.PB) {
		// Each worker gets its own RequestCtx to avoid data races
		c := &fasthttp.RequestCtx{}
		c.Request.Header.SetMethod("DELETE")
		c.URI().SetPath("/this/is/a/dummy/route/oke")
		for pb.Next() {
			appHandler(c)
		}
	})

	// Single-threaded verification on a fresh context to preserve correctness checks
	verifyCtx := &fasthttp.RequestCtx{}
	verifyCtx.Request.Header.SetMethod("DELETE")
	verifyCtx.URI().SetPath("/this/is/a/dummy/route/oke")
	appHandler(verifyCtx)
	require.Equal(b, 405, verifyCtx.Response.StatusCode())
	require.Equal(b, MethodGet+", "+MethodHead, string(verifyCtx.Response.Header.Peek("Allow")))
	require.Equal(b, utils.StatusMessage(StatusMethodNotAllowed), string(verifyCtx.Response.Body()))
}

func Benchmark_Router_NotFound_Parallel(b *testing.B) {
	b.ReportAllocs()
	app := New()
	app.Use(func(c Ctx) error {
		return c.Next()
	})
	registerDummyRoutes(app)
	appHandler := app.Handler()
	c := &fasthttp.RequestCtx{}
	c.Request.Header.SetMethod("DELETE")
	c.URI().SetPath("/this/route/does/not/exist")
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			appHandler(c)
		}
	})
	require.Equal(b, 404, c.Response.StatusCode())
	require.Equal(b, "Not Found", string(c.Response.Body()))
}

func Benchmark_Router_Handler_Parallel(b *testing.B) {
	app := New()
	registerDummyRoutes(app)
	appHandler := app.Handler()
	c := &fasthttp.RequestCtx{}
	c.Request.Header.SetMethod("DELETE")
	c.URI().SetPath("/user/keys/1337")
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			appHandler(c)
		}
	})
}

func Benchmark_Router_Handler_Strict_Case_Parallel(b *testing.B) {
	app := New(Config{StrictRouting: true, CaseSensitive: true})
	registerDummyRoutes(app)
	appHandler := app.Handler()
	c := &fasthttp.RequestCtx{}
	c.Request.Header.SetMethod("DELETE")
	c.URI().SetPath("/user/keys/1337")
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			appHandler(c)
		}
	})
}

func Benchmark_Router_Chain_Parallel(b *testing.B) {
	app := New()
	handler := func(c Ctx) error {
		return c.Next()
	}
	app.Get("/", handler, handler, handler, handler, handler, handler)
	appHandler := app.Handler()
	c := &fasthttp.RequestCtx{}
	c.Request.Header.SetMethod(MethodGet)
	c.URI().SetPath("/")
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			appHandler(c)
		}
	})
}

func Benchmark_Router_WithCompression_Parallel(b *testing.B) {
	app := New()
	handler := func(c Ctx) error {
		return c.Next()
	}
	app.Get("/", handler)
	app.Get("/", handler)
	app.Get("/", handler)
	app.Get("/", handler)
	app.Get("/", handler)
	app.Get("/", handler)
	appHandler := app.Handler()
	c := &fasthttp.RequestCtx{}
	c.Request.Header.SetMethod(MethodGet)
	c.URI().SetPath("/")
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			appHandler(c)
		}
	})
}

func Benchmark_Startup_Process_Parallel(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			app := New()
			registerDummyRoutes(app)
			app.startupProcess()
		}
	})
}

func Benchmark_Router_Next_Parallel(b *testing.B) {
	app := New()
	registerDummyRoutes(app)
	app.startupProcess()
	b.RunParallel(func(pb *testing.PB) {
		// Each worker gets its own request and context to avoid data races.
		request := &fasthttp.RequestCtx{}
		request.Request.Header.SetMethod("DELETE")
		request.URI().SetPath("/user/keys/1337")
		c := acquireDefaultCtxForRouterBenchmark(b, app, request)
		for pb.Next() {
			c.indexRoute = -1
			//nolint:errcheck // Benchmark hot path - error checked in verification
			_, _ = app.next(c)
		}
	})

	// Single-threaded verification on a fresh context to preserve correctness checks.
	verifyRequest := &fasthttp.RequestCtx{}
	verifyRequest.Request.Header.SetMethod("DELETE")
	verifyRequest.URI().SetPath("/user/keys/1337")
	verifyCtx := acquireDefaultCtxForRouterBenchmark(b, app, verifyRequest)
	verifyCtx.indexRoute = -1
	res, err := app.next(verifyCtx)
	require.NoError(b, err)
	require.True(b, res)
	require.Equal(b, 4, verifyCtx.indexRoute)
}

func Benchmark_Router_Next_Default_Immutable_Parallel(b *testing.B) {
	app := New(Config{Immutable: true})
	app.Get("/", func(_ Ctx) error {
		return nil
	})
	h := app.Handler()
	fctx := &fasthttp.RequestCtx{}
	fctx.Request.Header.SetMethod(MethodGet)
	fctx.Request.SetRequestURI("/")
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			h(fctx)
		}
	})
}

func Benchmark_Route_Match_Parallel(b *testing.B) {
	parsed := parseRoute("/user/keys/:id", regexp.MustCompile)
	route := &Route{use: false, root: false, star: false, routeParser: parsed, Params: parsed.params, path: "/user/keys/:id", Path: "/user/keys/:id", Method: "DELETE"}
	route.Handlers = append(route.Handlers, func(_ Ctx) error {
		return nil
	})
	b.RunParallel(func(pb *testing.PB) {
		// Each worker gets its own local variables to avoid data races
		var params [maxParams]string
		for pb.Next() {
			_ = route.match("/user/keys/1337", "/user/keys/1337", &params, 3)
		}
	})

	// Single-threaded verification to preserve correctness checks
	var verifyParams [maxParams]string
	match := route.match("/user/keys/1337", "/user/keys/1337", &verifyParams, 3)
	require.True(b, match)
	require.Equal(b, []string{"1337"}, verifyParams[0:len(parsed.params)])
}

func Benchmark_Route_Match_Star_Parallel(b *testing.B) {
	var match bool
	var params [maxParams]string
	parsed := parseRoute("/*", regexp.MustCompile)
	route := &Route{use: false, root: false, star: true, routeParser: parsed, Params: parsed.params, path: "/user/keys/bla", Path: "/user/keys/bla", Method: "DELETE"}
	route.Handlers = append(route.Handlers, func(_ Ctx) error {
		return nil
	})
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			match = route.match("/user/keys/bla", "/user/keys/bla", &params, 3)
		}
	})
	require.True(b, match)
	require.Equal(b, []string{"user/keys/bla"}, params[0:len(parsed.params)])
}

func Benchmark_Route_Match_Root_Parallel(b *testing.B) {
	var match bool
	var params [maxParams]string
	parsed := parseRoute("/", regexp.MustCompile)
	route := &Route{use: false, root: true, star: false, path: "/", routeParser: parsed, Params: parsed.params, Path: "/", Method: "DELETE"}
	route.Handlers = append(route.Handlers, func(_ Ctx) error {
		return nil
	})
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			match = route.match("/", "/", &params, 1)
		}
	})
	require.True(b, match)
	require.Equal(b, []string{}, params[0:len(parsed.params)])
}

func Benchmark_Router_Handler_CaseSensitive_Parallel(b *testing.B) {
	app := New()
	app.config.CaseSensitive = true
	registerDummyRoutes(app)
	appHandler := app.Handler()
	c := &fasthttp.RequestCtx{}
	c.Request.Header.SetMethod("DELETE")
	c.URI().SetPath("/user/keys/1337")
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			appHandler(c)
		}
	})
}

func Benchmark_Router_Handler_Unescape_Parallel(b *testing.B) {
	app := New()
	app.config.UnescapePath = true
	registerDummyRoutes(app)
	app.Delete("/créer", func(_ Ctx) error {
		return nil
	})
	appHandler := app.Handler()
	c := &fasthttp.RequestCtx{}
	c.Request.Header.SetMethod(MethodDelete)
	c.URI().SetPath("/cr%C3%A9er")
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c.URI().SetPath("/cr%C3%A9er")
			appHandler(c)
		}
	})
}

func Benchmark_Router_Handler_StrictRouting_Parallel(b *testing.B) {
	app := New()
	app.config.CaseSensitive = true
	registerDummyRoutes(app)
	appHandler := app.Handler()
	c := &fasthttp.RequestCtx{}
	c.Request.Header.SetMethod("DELETE")
	c.URI().SetPath("/user/keys/1337")
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			appHandler(c)
		}
	})
}

func Benchmark_Router_GitHub_API_Parallel(b *testing.B) {
	app := New()
	registerDummyRoutes(app)
	app.startupProcess()
	b.ResetTimer()
	for i := range routesFixture.TestRoutes {
		b.RunParallel(func(pb *testing.PB) {
			// Each worker gets its own RequestCtx and local variables to avoid data races
			c := &fasthttp.RequestCtx{}
			c.Request.Header.SetMethod(routesFixture.TestRoutes[i].Method)
			for pb.Next() {
				c.URI().SetPath(routesFixture.TestRoutes[i].Path)
				ctx := acquireDefaultCtxForRouterBenchmark(b, app, c)
				//nolint:errcheck // Benchmark hot path - error checked in verification
				_, _ = app.next(ctx)
				app.ReleaseCtx(ctx)
			}
		})

		// Single-threaded verification on a fresh context to preserve correctness checks
		verifyC := &fasthttp.RequestCtx{}
		verifyC.Request.Header.SetMethod(routesFixture.TestRoutes[i].Method)
		verifyC.URI().SetPath(routesFixture.TestRoutes[i].Path)
		verifyCtx := acquireDefaultCtxForRouterBenchmark(b, app, verifyC)
		match, err := app.next(verifyCtx)
		app.ReleaseCtx(verifyCtx)
		require.NoError(b, err)
		require.True(b, match)
	}
}

func Test_Route_URL(t *testing.T) {
	t.Parallel()

	// requireURL asserts the URL composed from params for the route called name.
	requireURL := func(t *testing.T, app *App, name string, params Map, want string) {
		t.Helper()

		route := app.GetRoute(name)
		url, err := route.URL(params)
		require.NoError(t, err)
		require.Equal(t, want, url)
	}

	for _, tc := range []struct {
		params  Map
		name    string
		pattern string
		want    string
	}{
		{name: "simple parameter", pattern: "/user/:name", params: Map{"name": "fiber"}, want: "/user/fiber"},
		{name: "multiple parameters", pattern: "/user/:name/:id", params: Map{"name": "john", "id": "123"}, want: "/user/john/123"},
		{
			name:    "wildcard parameters",
			pattern: "/:phone/*/send/*",
			params:  Map{"phone": "23456789", "*1": "sms", "*2": "test-msg"},
			want:    "/23456789/sms/send/test-msg",
		},
		{
			name:    "single wildcard parameter",
			pattern: "/:phone/*/send",
			params:  Map{"phone": "23456789", "*": "sms"},
			want:    "/23456789/sms/send",
		},
		{name: "plus parameters prefer plus fallback", pattern: "/user/+", params: Map{"*": "wildcard", "+": "plus"}, want: "/user/plus"},
		{name: "no parameters", pattern: "/static/path", params: Map{}, want: "/static/path"},
		// a parameter without a value leaves an empty placeholder
		{name: "missing parameter value", pattern: "/user/:name/:id", params: Map{"name": "john"}, want: "/user/john/"},
		{name: "extra parameters ignored", pattern: "/user/:name", params: Map{"name": "fiber", "extra": "ignored"}, want: "/user/fiber"},
		{name: "numeric parameter values", pattern: "/item/:id", params: Map{"id": 42}, want: "/item/42"},
		{
			name:    "complex path with mixed content",
			pattern: "/api/v1/users/:userId/posts/:postId/comments",
			params:  Map{"userId": "user123", "postId": "post456"},
			want:    "/api/v1/users/user123/posts/post456/comments",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := New()
			app.Get(tc.pattern, emptyHandler).Name("route")

			requireURL(t, app, "route", tc.params, tc.want)
		})
	}

	t.Run("wildcard parameters preserve slashes", func(t *testing.T) {
		t.Parallel()
		app := New()
		app.Get("/files/*", func(c Ctx) error {
			return c.SendString(c.Params("*"))
		}).Name("Files")

		requireURL(t, app, "Files", Map{"*": "docs/index.html"}, "/files/docs/index.html")

		apptest.ExpectBody(t, app, "/files/docs/index.html", StatusOK, "docs/index.html")
	})

	t.Run("preferred greedy parameters default fallback", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, preferredPlusGreedyParameters, preferredGreedyParameters("+1"))
		require.Equal(t, []string{"*", "+"}, preferredGreedyParameters("*1"))
		require.Equal(t, defaultGreedyParameterKeys, preferredGreedyParameters(""))
		require.Equal(t, defaultGreedyParameterKeys, preferredGreedyParameters("name"))
	})

	t.Run("case insensitive default", func(t *testing.T) {
		t.Parallel()
		app := New()
		app.Get("/user/:name", emptyHandler).Name("User")

		// Case-insensitive by default (matching Fiber's default behavior)
		requireURL(t, app, "User", Map{"Name": "fiber"}, "/user/fiber")

		// When multiple keys case-fold to the same param name and no exact key
		// exists, the lexicographically-smallest key wins deterministically.
		for range 50 {
			requireURL(t, app, "User", Map{"nAme": "second", "Name": "first"}, "/user/first")
		}

		// When multiple keys case-fold to the same param name, prefer the exact match.
		requireURL(t, app, "User", Map{"name": "exact", "Name": "fallback"}, "/user/exact")
	})

	t.Run("case sensitive", func(t *testing.T) {
		t.Parallel()
		app := New(Config{CaseSensitive: true})
		app.Get("/user/:name", emptyHandler).Name("User")

		// Exact case match succeeds
		requireURL(t, app, "User", Map{"name": "fiber"}, "/user/fiber")
		// Different case does not match when CaseSensitive is true
		requireURL(t, app, "User", Map{"Name": "fiber"}, "/user/")
	})

	t.Run("empty route", func(t *testing.T) {
		t.Parallel()
		route := Route{}
		url, err := route.URL(Map{"name": "fiber"})
		require.Error(t, err)
		require.Equal(t, ErrNotFound, err)
		require.Empty(t, url)
	})

	t.Run("route without parsed segments returns path", func(t *testing.T) {
		t.Parallel()
		// This covers fallback/manual Route values whose Path is set but whose
		// routeParser was never populated, such as synthetic routes outside the
		// normal registration pipeline.
		route := Route{Path: "/error"}
		url, err := route.URL(Map{"name": "fiber"})
		require.NoError(t, err)
		require.Equal(t, "/error", url)
	})

	t.Run("GetRoute direct call", func(t *testing.T) {
		t.Parallel()
		app := New()
		app.Get("/user/:name", emptyHandler).Name("User")

		url, err := app.GetRoute("User").URL(Map{"name": "fiber"})
		require.NoError(t, err)
		require.Equal(t, "/user/fiber", url)
	})

	t.Run("parameter values are path escaped", func(t *testing.T) {
		t.Parallel()
		app := New()
		app.Get("/value/:value", emptyHandler).Name("Value")

		const value = "a/b?c#d% e"
		location, err := app.GetRoute("Value").URL(Map{"value": value})
		require.NoError(t, err)
		require.Equal(t, "/value/a%2Fb%3Fc%23d%25%20e", location)

		decoded, err := neturl.PathUnescape(strings.TrimPrefix(location, "/value/"))
		require.NoError(t, err)
		require.Equal(t, value, decoded)
	})
}

// newSkipApp builds an app with SkipUnmatchedRoutes enabled plus a benign
// middleware, so requests actually exercise the lookahead fast path (which is
// gated on the presence of middleware).
func newSkipApp(cfg ...Config) *App {
	c := Config{SkipUnmatchedRoutes: true}
	if len(cfg) > 0 {
		c = cfg[0]
		c.SkipUnmatchedRoutes = true
	}
	app := New(c)
	app.Use(func(c Ctx) error { return c.Next() })
	return app
}

// Test_App_SkipUnmatchedRoutes_StaticAndParametric sends the same three requests
// to a static and to a parametric endpoint: a match, a miss (404) and a method
// the matched path has no endpoint for (405 listing the methods it does have).
func Test_App_SkipUnmatchedRoutes_StaticAndParametric(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		register func(app *App)
		name     string
		match    string // path the GET endpoint serves
		body     string // what the GET endpoint answers
		miss     string // path no endpoint serves
	}{
		{
			name:  "static",
			match: "/users",
			body:  "users",
			miss:  "/does/not/exist",
			register: func(app *App) {
				app.Get("/users", func(c Ctx) error { return c.SendString("users") })
				app.Post("/users", func(c Ctx) error { return c.SendString("created") })
			},
		},
		{
			name:  "parametric",
			match: "/user/keys/1337",
			body:  "1337",
			miss:  "/user/secrets/1337",
			register: func(app *App) {
				app.Get("/user/keys/:id", func(c Ctx) error { return c.SendString(c.Params("id")) })
				app.Post("/user/keys/:id", func(c Ctx) error { return c.SendString("post") })
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app := newSkipApp()
			tc.register(app)

			t.Run("match", func(t *testing.T) {
				t.Parallel()
				apptest.ExpectBody(t, app, tc.match, StatusOK, tc.body)
			})

			t.Run("not_found", func(t *testing.T) {
				t.Parallel()
				routerTestExpect(t, app, tc.miss, StatusNotFound)
			})

			t.Run("method_not_allowed", func(t *testing.T) {
				t.Parallel()
				resp := routerTestExpect(t, app, apptest.Req(MethodDelete, tc.match), StatusMethodNotAllowed)
				allow := resp.Header.Get(HeaderAllow)
				require.Contains(t, allow, MethodGet)
				require.Contains(t, allow, MethodPost)
			})
		})
	}
}

func Test_App_SkipUnmatchedRoutes_CORSPreflight(t *testing.T) {
	t.Parallel()

	app := New(Config{SkipUnmatchedRoutes: true})
	// Minimal CORS-style middleware: answer preflight, pass everything else through.
	app.Use(func(c Ctx) error {
		if c.IsPreflight() {
			c.Set(HeaderAccessControlAllowOrigin, "*")
			return c.SendStatus(StatusNoContent)
		}
		return c.Next()
	})
	app.Get("/users", func(c Ctx) error { return c.SendString("users") })

	t.Run("preflight_runs_middleware", func(t *testing.T) {
		t.Parallel()
		req := apptest.Req(MethodOptions, "/users",
			apptest.WithHeader(HeaderOrigin, "https://example.com"),
			apptest.WithHeader(HeaderAccessControlRequestMethod, MethodGet))
		resp := routerTestExpect(t, app, req, StatusNoContent)
		require.Equal(t, "*", resp.Header.Get(HeaderAccessControlAllowOrigin))
	})

	t.Run("non_preflight_options_still_405", func(t *testing.T) {
		t.Parallel()
		// Bare OPTIONS (no CORS headers) is not a preflight, so the fast path still answers 405.
		routerTestExpect(t, app, apptest.Req(MethodOptions, "/users"), StatusMethodNotAllowed)
	})
}

// Test_App_SkipUnmatchedRoutes_MiddlewareSkipped verifies that middleware does
// NOT run for unmatched requests but DOES run for matched ones.
func Test_App_SkipUnmatchedRoutes_MiddlewareSkipped(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		path    string
		message string
		status  int
		skip    bool
		called  bool
	}{
		{
			name: "skipped_on_404", skip: true, path: "/nope", status: StatusNotFound,
			called: false, message: "middleware must NOT run for an unmatched route",
		},
		{
			name: "runs_on_match", skip: true, path: "/ok", status: StatusOK,
			called: true, message: "middleware must run for a matched route",
		},
		{
			// SkipUnmatchedRoutes is false by default
			name: "disabled_runs_middleware", skip: false, path: "/nope", status: StatusNotFound,
			called: true, message: "middleware should still run when the feature is disabled",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := New(Config{SkipUnmatchedRoutes: tc.skip})
			called := false
			app.Use(func(c Ctx) error { called = true; return c.Next() })
			app.Get("/ok", func(c Ctx) error { return c.SendString("ok") })

			routerTestExpect(t, app, tc.path, tc.status)
			require.Equal(t, tc.called, called, tc.message)
		})
	}
}

// Test_App_SkipUnmatchedRoutes_NoMiddlewareGate verifies correct results when the
// feature is enabled but no middleware is registered (the lookahead is gated off).
func Test_App_SkipUnmatchedRoutes_NoMiddlewareGate(t *testing.T) {
	t.Parallel()

	app := New(Config{SkipUnmatchedRoutes: true}) // no Use
	app.Get("/ping", func(c Ctx) error { return c.SendString("pong") })

	routerTestExpect(t, app, "/ping", StatusOK)

	routerTestExpect(t, app, "/missing", StatusNotFound)

	routerTestExpect(t, app, apptest.Req(MethodPost, "/ping"), StatusMethodNotAllowed)
}

// Test_App_SkipUnmatchedRoutes_CustomErrorHandler covers the emitSkip path where
// the configured error handler itself returns an error (-> 500).
func Test_App_SkipUnmatchedRoutes_CustomErrorHandler(t *testing.T) {
	t.Parallel()

	app := New(Config{
		SkipUnmatchedRoutes: true,
		ErrorHandler:        func(_ Ctx, _ error) error { return errors.New("boom") },
	})
	app.Use(func(c Ctx) error { return c.Next() })
	app.Get("/ok", func(c Ctx) error { return c.SendString("ok") })

	routerTestExpect(t, app, "/nope", StatusInternalServerError)
}

// Test_App_SkipUnmatchedRoutes_CrossMethodMixed exercises the 405 path where the
// path matches a static endpoint of one method and a parametric endpoint of
// another, so both Allow entries must be reported.
func Test_App_SkipUnmatchedRoutes_CrossMethodMixed(t *testing.T) {
	t.Parallel()

	app := newSkipApp()
	app.Get("/thing/:id", func(c Ctx) error { return c.SendString("get") })  // parametric
	app.Post("/thing/fixed", func(c Ctx) error { return c.SendString("p") }) // static

	resp := routerTestExpect(t, app, apptest.Req(MethodDelete, "/thing/fixed"), StatusMethodNotAllowed)
	allow := resp.Header.Get(HeaderAllow)
	require.Contains(t, allow, MethodGet)
	require.Contains(t, allow, MethodPost)
}

// Test_App_SkipUnmatchedRoutes_Bucket0Fallback exercises the tier-2 fallback to
// the global (bucket 0) candidate list when the request's prefix bucket is absent.
func Test_App_SkipUnmatchedRoutes_Bucket0Fallback(t *testing.T) {
	t.Parallel()

	app := newSkipApp()
	app.Get("/:id", func(c Ctx) error { return c.SendString(c.Params("id")) })

	apptest.ExpectBody(t, app, "/abc", StatusOK, "abc")
}

// Test_App_SkipUnmatchedRoutes_SkipBefore exercises next()'s skip-before guard:
// a static endpoint sits before the parametric endpoint in the same bucket, so
// firstMatchIndex > 0 and the endpoints before it are skipped.
func Test_App_SkipUnmatchedRoutes_SkipBefore(t *testing.T) {
	t.Parallel()

	app := newSkipApp()
	app.Get("/key/aaa", func(c Ctx) error { return c.SendString("static") })
	app.Get("/key/:id", func(c Ctx) error { return c.SendString(c.Params("id")) })

	apptest.ExpectBody(t, app, "/key/123", StatusOK, "123")
}

func Test_App_SkipUnmatchedRoutes_OptionalParam(t *testing.T) {
	t.Parallel()

	app := newSkipApp()
	app.Get("/opt/:v?", func(c Ctx) error { return c.SendString("opt:" + c.Params("v")) })

	for _, path := range []string{"/opt", "/opt/x"} {
		routerTestExpect(t, app, path, StatusOK)
	}

	routerTestExpect(t, app, "/other", StatusNotFound)
}

func Test_App_SkipUnmatchedRoutes_Constraint(t *testing.T) {
	t.Parallel()

	app := newSkipApp()
	app.Get("/num/:id<int>", func(c Ctx) error { return c.SendString(c.Params("id")) })

	routerTestExpect(t, app, "/num/5", StatusOK)

	routerTestExpect(t, app, "/num/abc", StatusNotFound)
}

func Test_App_SkipUnmatchedRoutes_RootAndStar(t *testing.T) {
	t.Parallel()

	t.Run("root", func(t *testing.T) {
		t.Parallel()
		app := newSkipApp()
		app.Get("/", func(c Ctx) error { return c.SendString("root") })

		routerTestExpect(t, app, "/", StatusOK)
	})

	t.Run("star", func(t *testing.T) {
		t.Parallel()
		app := newSkipApp()
		app.Get("/*", func(c Ctx) error { return c.SendString("star") })

		apptest.ExpectBody(t, app, "/anything/here", StatusOK, "star")
	})
}

func Test_App_SkipUnmatchedRoutes_AutoHead(t *testing.T) {
	t.Parallel()

	t.Run("static", func(t *testing.T) {
		t.Parallel()
		app := newSkipApp()
		app.Get("/page", func(c Ctx) error { return c.SendString("page") })

		routerTestExpect(t, app, apptest.Req(MethodHead, "/page"), StatusOK)
	})

	t.Run("param", func(t *testing.T) {
		t.Parallel()
		app := newSkipApp()
		app.Get("/h/:id", func(c Ctx) error { return c.SendString(c.Params("id")) })

		routerTestExpect(t, app, apptest.Req(MethodHead, "/h/1"), StatusOK)
	})
}

// Test_App_SkipUnmatchedRoutes_GroupMiddleware checks that group middleware is
// skipped for unmatched routes and runs for matched ones. The two checks share
// the called flag and must run in order, so they are not split into subtests.
func Test_App_SkipUnmatchedRoutes_GroupMiddleware(t *testing.T) {
	t.Parallel()

	app := New(Config{SkipUnmatchedRoutes: true})
	called := false
	g := app.Group("/g")
	g.Use(func(c Ctx) error { called = true; return c.Next() })
	g.Get("/ok", func(c Ctx) error { return c.SendString("ok") })

	routerTestExpect(t, app, "/g/nope", StatusNotFound)
	require.False(t, called, "group middleware must not run for an unmatched route")

	routerTestExpect(t, app, "/g/ok", StatusOK)
	require.True(t, called, "group middleware must run for a matched route")
}

func Test_App_SkipUnmatchedRoutes_Mount(t *testing.T) {
	t.Parallel()

	sub := New()
	sub.Get("/profile", func(c Ctx) error { return c.SendString("profile") })

	app := newSkipApp()
	app.Use("/account", sub)

	t.Run("mounted_match", func(t *testing.T) {
		t.Parallel()
		apptest.ExpectBody(t, app, "/account/profile", StatusOK, "profile")
	})

	t.Run("mounted_miss", func(t *testing.T) {
		t.Parallel()
		routerTestExpect(t, app, "/account/missing", StatusNotFound)
	})
}

func Test_App_SkipUnmatchedRoutes_CaseSensitiveAndStrict(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		cfg   Config
		name  string
		route string
		hit   string // served with status 200
		miss  string // 404, left out where the case has none
	}{
		{name: "case_sensitive", cfg: Config{CaseSensitive: true}, route: "/Foo", hit: "/Foo", miss: "/foo"},
		{name: "strict_routing", cfg: Config{StrictRouting: true}, route: "/bar", hit: "/bar", miss: "/bar/"},
		{name: "non_strict_trailing_slash", route: "/baz", hit: "/baz/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := newSkipApp(tc.cfg)
			app.Get(tc.route, func(c Ctx) error { return c.SendString("ok") })

			routerTestExpect(t, app, tc.hit, StatusOK)
			if tc.miss != "" {
				routerTestExpect(t, app, tc.miss, StatusNotFound)
			}
		})
	}
}

func Test_App_SkipUnmatchedRoutes_RebuildTree(t *testing.T) {
	t.Parallel()

	app := newSkipApp()
	app.Get("/first", func(c Ctx) error { return c.SendString("first") })

	routerTestExpect(t, app, "/second", StatusNotFound)

	app.Get("/second", func(c Ctx) error { return c.SendString("second") })
	app.RebuildTree()

	routerTestExpect(t, app, "/second", StatusOK)
}

// Test_App_SkipUnmatchedRoutes_RestartRouting ensures a handler that rewrites
// the path and restarts routing re-resolves correctly (no stale lookahead index).
func Test_App_SkipUnmatchedRoutes_RestartRouting(t *testing.T) {
	t.Parallel()

	app := newSkipApp()
	app.Get("/old", func(c Ctx) error {
		c.Path("/new")
		return c.RestartRouting()
	})
	app.Get("/new", func(c Ctx) error { return c.SendString("new") })

	apptest.ExpectBody(t, app, "/old", StatusOK, "new")
}

// Test_App_SkipUnmatchedRoutes_Parity asserts that enabling SkipUnmatchedRoutes
// produces the same status codes and Allow headers as the default behavior.
func Test_App_SkipUnmatchedRoutes_Parity(t *testing.T) {
	t.Parallel()

	build := func(skip bool) *App {
		app := New(Config{SkipUnmatchedRoutes: skip})
		app.Use(func(c Ctx) error { return c.Next() })
		registerDummyRoutes(app)
		return app
	}
	off := build(false)
	on := build(true)

	cases := []struct {
		method string
		path   string
	}{
		{MethodGet, "/user/keys/1337"},
		{MethodGet, "/does/not/exist"},
		{MethodGet, "/repos/gofiber/fiber/stargazers"},
		{MethodPost, "/user/keys/1337"},
		{MethodDelete, "/user/keys/1337"},
		{MethodGet, "/"},
		{MethodGet, "/applications/client/tokens"},
		{MethodGet, "/user/repos"},
		{MethodHead, "/user/keys/1337"},
		{MethodOptions, "/user/keys/1337"},
		{MethodPost, "/"},
	}

	for _, tc := range cases {
		respOff := apptest.Do(t, off, apptest.Req(tc.method, tc.path))
		respOn := apptest.Do(t, on, apptest.Req(tc.method, tc.path))
		require.Equal(t, respOff.StatusCode, respOn.StatusCode, "%s %s", tc.method, tc.path)
		require.Equal(t, respOff.Header.Get(HeaderAllow), respOn.Header.Get(HeaderAllow), "%s %s allow", tc.method, tc.path)
		require.Equal(t, respOff.Header.Get(HeaderContentType), respOn.Header.Get(HeaderContentType), "%s %s content-type", tc.method, tc.path)
		bodyOff := apptest.Body(t, respOff)
		bodyOn := apptest.Body(t, respOn)
		require.Equal(t, bodyOff, bodyOn, "%s %s body", tc.method, tc.path)
	}
}

// Test_App_SkipUnmatchedRoutes_CustomCtx exercises the customRequestHandler path.
func Test_App_SkipUnmatchedRoutes_CustomCtx(t *testing.T) {
	t.Parallel()

	app := newCustomApp(Config{SkipUnmatchedRoutes: true})
	app.Use(func(c Ctx) error {
		if c.IsPreflight() {
			return c.SendStatus(StatusNoContent)
		}
		return c.Next()
	})
	app.Get("/users", func(c Ctx) error { return c.SendString("users") })
	app.Get("/user/keys/:id", func(c Ctx) error { return c.SendString("key") })

	for _, tc := range []struct {
		req    *http.Request
		name   string
		status int
	}{
		{name: "static_match", req: apptest.Req(MethodGet, "/users"), status: StatusOK},
		{name: "param_match", req: apptest.Req(MethodGet, "/user/keys/1"), status: StatusOK},
		{name: "not_found", req: apptest.Req(MethodGet, "/nope"), status: StatusNotFound},
		{name: "method_not_allowed", req: apptest.Req(MethodDelete, "/users"), status: StatusMethodNotAllowed},
		{
			// Preflight is exempt from the skip, so the middleware answers it instead of a 405.
			name: "cors_preflight",
			req: apptest.Req(MethodOptions, "/users",
				apptest.WithHeader(HeaderOrigin, "https://example.com"),
				apptest.WithHeader(HeaderAccessControlRequestMethod, MethodGet)),
			status: StatusNoContent,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			routerTestExpect(t, app, tc.req, tc.status)
		})
	}
}

// Test_App_SkipUnmatchedRoutes_FlashCookieCleared pins the flash check running
// before the short-circuit: a skipped 404 must still clear the flash cookie.
func Test_App_SkipUnmatchedRoutes_FlashCookieCleared(t *testing.T) {
	t.Parallel()

	app := newSkipApp()
	app.Get("/set", func(c Ctx) error { return c.Redirect().With("k", "v").To("/target") })

	resp := apptest.Do(t, app, "/set")
	flash := resp.Header.Get(HeaderSetCookie)
	require.Contains(t, flash, FlashCookieName+"=")
	require.NoError(t, resp.Body.Close())

	req := apptest.Req(MethodGet, "/missing", apptest.WithHeader(HeaderCookie, strings.Split(flash, ";")[0]))
	resp = routerTestExpect(t, app, req, StatusNotFound)
	cleared := resp.Header.Get(HeaderSetCookie)
	require.Contains(t, cleared, FlashCookieName+"=")
	require.Contains(t, strings.ToLower(cleared), "max-age=0")
}

// Test_App_SkipUnmatchedRoutes_PathOverrideMidChain ensures Path(override)
// invalidates the lookahead index even without RestartRouting.
func Test_App_SkipUnmatchedRoutes_PathOverrideMidChain(t *testing.T) {
	t.Parallel()

	app := newSkipApp()
	app.Use(func(c Ctx) error {
		if c.Path() == "/aaa/zzz" {
			c.Path("/bbb")
		}
		return c.Next()
	})
	app.Get("/bbb", func(c Ctx) error { return c.SendString("bbb") })
	app.Get("/aaa/static", func(c Ctx) error { return c.SendString("static") })
	app.Get("/aaa/:id", func(c Ctx) error { return c.SendString(c.Params("id")) })

	apptest.ExpectBody(t, app, "/aaa/zzz", StatusOK, "bbb")
}

// Test_App_SkipUnmatchedRoutes_MountedErrorHandler ensures a skipped 404 under a
// mount prefix still resolves the sub-app's custom error handler.
func Test_App_SkipUnmatchedRoutes_MountedErrorHandler(t *testing.T) {
	t.Parallel()

	sub := New(Config{ErrorHandler: func(c Ctx, _ error) error {
		return c.Status(StatusNotFound).SendString("sub-404")
	}})
	sub.Get("/ok", func(c Ctx) error { return c.SendString("ok") })

	app := newSkipApp()
	app.Use("/sub", sub)

	apptest.ExpectBody(t, app, "/sub/missing", StatusNotFound, "sub-404")
}

// Test_App_SkipUnmatchedRoutes_ErrorSentinels pins the errors handed to the
// error handler on the fast path (users match on them with errors.Is).
func Test_App_SkipUnmatchedRoutes_ErrorSentinels(t *testing.T) {
	t.Parallel()

	var got error
	app := New(Config{
		SkipUnmatchedRoutes: true,
		ErrorHandler: func(c Ctx, err error) error {
			got = err
			return DefaultErrorHandler(c, err)
		},
	})
	app.Use(func(c Ctx) error { return c.Next() })
	app.Get("/ok", func(c Ctx) error { return c.SendString("ok") })

	routerTestExpect(t, app, "/nope", StatusNotFound)
	require.ErrorIs(t, got, ErrNotFound)

	routerTestExpect(t, app, apptest.Req(MethodPost, "/ok"), StatusMethodNotAllowed)
	require.ErrorIs(t, got, ErrMethodNotAllowed)
}

// Test_App_SkipUnmatchedRoutes_StarMiddlewareNoClobber covers the route.star half
// of hasParamUse: wildcard middleware writes c.values, so the endpoint re-matches.
func Test_App_SkipUnmatchedRoutes_StarMiddlewareNoClobber(t *testing.T) {
	t.Parallel()

	app := newSkipApp()
	app.Use("/*", func(c Ctx) error { return c.Next() })
	app.Get("/user/:id", func(c Ctx) error { return c.SendString(c.Params("id")) })

	apptest.ExpectBody(t, app, "/user/42", StatusOK, "42")
}

// Test_App_SkipUnmatchedRoutes_UnescapePath pins the static index lookup by the
// decoded detection path.
func Test_App_SkipUnmatchedRoutes_UnescapePath(t *testing.T) {
	t.Parallel()

	app := newSkipApp(Config{UnescapePath: true})
	app.Get("/some/path", func(c Ctx) error { return c.SendString("ok") })

	routerTestExpect(t, app, "/some/p%61th", StatusOK)
}

// Test_App_SkipUnmatchedRoutes_CustomCtxFailingErrorHandler covers the 500
// fallback in emitSkipCustom.
func Test_App_SkipUnmatchedRoutes_CustomCtxFailingErrorHandler(t *testing.T) {
	t.Parallel()

	app := newCustomApp(Config{
		SkipUnmatchedRoutes: true,
		ErrorHandler:        func(Ctx, error) error { return errors.New("fail") },
	})
	app.Use(func(c Ctx) error { return c.Next() })
	app.Get("/users", func(c Ctx) error { return c.SendString("users") })

	routerTestExpect(t, app, "/nope", StatusInternalServerError)
}

func Benchmark_SkipUnmatchedRoutes(b *testing.B) {
	scenarios := []struct {
		name        string
		method      string
		path        string
		middlewares int
	}{
		{"Unmatched", MethodGet, "/this/route/does/not/exist", 3},
		{"Matched", MethodGet, "/user/keys/1337", 1},
		{"405_Middleware", MethodPost, "/user/keys/1337", 3},
		{"405_NoMiddleware", MethodPost, "/user/keys/1337", 0},
	}
	for _, s := range scenarios {
		for _, skip := range []bool{false, true} {
			variant := "without_skip"
			if skip {
				variant = "with_skip"
			}
			b.Run(s.name+"/"+variant, func(b *testing.B) {
				app := New(Config{SkipUnmatchedRoutes: skip})
				for range s.middlewares {
					app.Use(func(c Ctx) error { return c.Next() })
				}
				registerDummyRoutes(app)
				appHandler := app.Handler()

				c := &fasthttp.RequestCtx{}
				c.Request.Header.SetMethod(s.method)
				c.URI().SetPath(s.path)

				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					appHandler(c)
				}
			})
		}
	}
}

func Benchmark_SkipUnmatchedRoutes_Deep(b *testing.B) {
	deepScenarios := []struct {
		name   string
		method string
		path   string
		status int
	}{
		{"Matched_health_0param", MethodGet, "/health", StatusOK},
		{"Unmatched_Top_level", MethodGet, "/.env", StatusNotFound},
		{"Unmatched_Top_level_SBucket", MethodGet, "/heap", StatusNotFound},
		{"Unmatched_Top_level_SBucket_Diff_Method", MethodDelete, "/heap", StatusNotFound},
		{"Unmatched_Top_level_SBucket_Diff_Method2", MethodPost, "/heap", StatusNotFound},
		{"Unmatched_Top_level_SBucket_WrongMethod", MethodPost, "/health", StatusMethodNotAllowed},
		{"Matched_1param", MethodGet, "/api/v1/matters/m1", StatusOK},
		{"Matched_3param", MethodGet, "/api/v1/matters/m1/staff/s1/docs/d1", StatusOK},
		{"Matched_4param", MethodGet, "/api/v1/matters/m1/staff/s1/docs/d1/versions/v1", StatusOK},
		{"Matched_5param", MethodGet, "/api/v1/matters/m1/staff/s1/docs/d1/versions/v1/notes/n1", StatusOK},
		{"MethodNotAllowed_deep", MethodPost, "/api/v1/matters/m1/staff/s1/primary", StatusMethodNotAllowed},
	}
	for _, s := range deepScenarios {
		for _, skip := range []bool{false, true} {
			variant := "without_skip"
			if skip {
				variant = "with_skip"
			}
			b.Run(s.name+"/"+variant, func(b *testing.B) {
				app := New(Config{SkipUnmatchedRoutes: skip})
				h := func(_ Ctx) error { return nil }
				mw := func(c Ctx) error { return c.Next() }

				app.Get("/health", h)

				api := app.Group("/api/v1", mw)
				matters := api.Group("/matters", mw)

				matters.Get("/:id", h)
				matters.Put("/:matterId/staff/:staffId/primary", h)
				api.Put("/emails/threads/:threadID/pin/:matterID", h)
				api.Post("/conflicts/:entityType/:entityID/refresh", h)

				matters.Get("/:matterId/staff/:staffId/docs/:docId", h)
				matters.Get("/:matterId/staff/:staffId/docs/:docId/versions/:versionId", h)
				matters.Get("/:matterId/staff/:staffId/docs/:docId/versions/:versionId/notes/:noteId", h)

				appHandler := app.Handler()

				c := &fasthttp.RequestCtx{}
				c.Request.Header.SetMethod(s.method)
				c.URI().SetPath(s.path)

				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					appHandler(c)
				}
			})
		}
	}
}

// Test_App_SkipUnmatchedRoutes_MethodOverride ensures Method(...) clears the
// lookahead index: a middleware switches GET->POST mid-chain, and the POST
// endpoint sits at a lower bucket index than the GET match, so a stale
// firstMatchIndex would wrongly skip it.
func Test_App_SkipUnmatchedRoutes_MethodOverride(t *testing.T) {
	t.Parallel()

	app := New(Config{SkipUnmatchedRoutes: true})
	app.Use(func(c Ctx) error {
		if c.Method() == MethodGet {
			c.Method(MethodPost)
		}
		return c.Next()
	})
	app.Get("/zzz/aaa", func(c Ctx) error { return c.SendString("get-static") })
	app.Get("/zzz/:id", func(c Ctx) error { return c.SendString("get-param") })
	app.Post("/zzz/:id", func(c Ctx) error { return c.SendString("post-param") })

	apptest.ExpectBody(t, app, "/zzz/123", StatusOK, "post-param")
}

// Test_App_SkipUnmatchedRoutes_ParamReuse checks that a matched parametric
// endpoint returns correct multi-param values when next() reuses the lookahead's
// params (no parametric middleware, so the fast path is taken).
func Test_App_SkipUnmatchedRoutes_ParamReuse(t *testing.T) {
	t.Parallel()

	app := New(Config{SkipUnmatchedRoutes: true})
	app.Use(func(c Ctx) error { return c.Next() })         // non-param middleware
	app.Use("/api", func(c Ctx) error { return c.Next() }) // static-prefix middleware
	app.Get("/api/users/:uid/books/:bid", func(c Ctx) error {
		return c.SendString(c.Params("uid") + "," + c.Params("bid"))
	})

	apptest.ExpectBody(t, app, "/api/users/u7/books/b9", StatusOK, "u7,b9")
}

// Test_App_SkipUnmatchedRoutes_ParamMiddlewareNoClobber guards the param-reuse
// fast path: a parametric middleware runs before the endpoint and writes into the
// shared param slot, so the fast path must be disabled and the endpoint re-matched.
// If reuse fired incorrectly the endpoint would observe the middleware's value.
func Test_App_SkipUnmatchedRoutes_ParamMiddlewareNoClobber(t *testing.T) {
	t.Parallel()

	app := New(Config{SkipUnmatchedRoutes: true})
	// Parametric middleware whose match writes param slot 0 as the first segment.
	app.Use("/:seg", func(c Ctx) error { return c.Next() })
	app.Get("/foo/:name", func(c Ctx) error { return c.SendString(c.Params("name")) })

	resp := apptest.Expect(t, app, "/foo/bar", StatusOK)
	require.Equal(t, "bar", apptest.Body(t, resp), "endpoint param must not be clobbered by parametric middleware")
}

// Test_App_SkipUnmatchedRoutes_ManyMethods verifies that with more than 64
// RequestMethods the fast path is disabled and routing still returns correct
// results via the normal router.
func Test_App_SkipUnmatchedRoutes_ManyMethods(t *testing.T) {
	t.Parallel()

	methods := append([]string{}, DefaultMethods...)
	for i := 0; len(methods) <= 64; i++ {
		methods = append(methods, "X"+string(rune('A'+i%26))+string(rune('A'+i/26)))
	}

	app := New(Config{SkipUnmatchedRoutes: true, RequestMethods: methods})
	app.Use(func(c Ctx) error { return c.Next() })
	app.Get("/ok", func(c Ctx) error { return c.SendString("ok") })

	routerTestExpect(t, app, "/ok", StatusOK)

	routerTestExpect(t, app, "/nope", StatusNotFound)
}

// Benchmark_Router_HandlerCustom exercises the custom-context request path
// (nextCustom) over a deep bucket, where the per-iteration accessor hoisting
// matters most.
func Benchmark_Router_HandlerCustom(b *testing.B) {
	newCtx := func(app *App) CustomCtx { return &customCtx{DefaultCtx: *NewDefaultCtx(app)} }
	app := NewWithCustomCtx(newCtx)
	registerDummyRoutes(app)
	appHandler := app.Handler()

	c := &fasthttp.RequestCtx{}
	c.Request.Header.SetMethod(MethodGet)
	c.URI().SetPath("/user/keys/1337")

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		appHandler(c)
	}
}

// Benchmark_Router_HandlerCustom_NotFound exercises the custom-context 404/405
// cross-method fallback scan (nextCustom), where accessor hoisting matters.
func Benchmark_Router_HandlerCustom_NotFound(b *testing.B) {
	newCtx := func(app *App) CustomCtx { return &customCtx{DefaultCtx: *NewDefaultCtx(app)} }
	app := NewWithCustomCtx(newCtx)
	registerDummyRoutes(app)
	appHandler := app.Handler()

	c := &fasthttp.RequestCtx{}
	c.Request.Header.SetMethod(MethodGet)
	c.URI().SetPath("/this/route/does/not/exist")

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		appHandler(c)
	}
}

// Test_Route_PrefixFilter_Differential generatively proves the filters App.next
// applies before Route.match are transparent: for every generated pattern and
// path, a route they reject must be a route Route.match, with its own filters
// stood down, would have rejected anyway. The filters only ever get to skip
// work, never to change an outcome, so a false reject here is a routing bug.
func Test_Route_PrefixFilter_Differential(t *testing.T) {
	t.Parallel()

	segments := []string{
		"/api", "/foo/", "/:a", "/:b?", "/*", "/+", "/:a-:b", "/:f.:e?",
		":tail", "/::c", "/:x:y", "/name\\:verb", "/:p/fixed", "/",
		"/verylongconstantsegment",
		// Escaped forms matter: register derives star/root from the
		// escape-stripped path but Params from the escaped one, so these are
		// the shapes where the two disagree.
		"/\\*", "/\\+", "/\\:a", "/a\\*b",
	}
	patterns := make([]string, 0, len(segments)*(len(segments)+1))
	for _, s1 := range segments {
		patterns = append(patterns, s1)
		for _, s2 := range segments {
			patterns = append(patterns, s1+s2)
		}
	}

	pieces := []string{
		"", "/a", "/a/b", "/a-b", "/a.b", "/x/y-z", "/api", "/api/", "/foo",
		"/foo/", "/fixed", "/name:verb", "/verylongconstantsegment", "/",
		"/*", "/+", "/a*b",
	}
	paths := make([]string, 0, len(pieces)*len(pieces))
	for _, p1 := range pieces {
		for _, p2 := range pieces {
			paths = append(paths, p1+p2)
		}
	}

	for _, use := range []bool{false, true} {
		for _, route := range registerFilterRoutes(t, patterns, use) {
			for _, path := range paths {
				var params [maxParams]string
				if !route.match(path, path, &params, 0) {
					continue
				}
				if routeFilterRejects(route, path) {
					t.Fatalf("scan filter rejected a matching route: pattern %q, path %q, use %v",
						route.Path, path, use)
				}
			}
		}
	}
}

// newScanHead returns the scanHead a filtered bucket holds for r.
func newScanHead(r *Route) scanHead {
	var h scanHead
	h.init(r)
	return h
}

// routeFilterRejects mirrors what the scan loops apply before calling
// Route.match: the leading-byte filter App.next, App.nextCustom and resolveSkip
// test on a Route, and the scanHead routeScan.skip tests for a filtered bucket
// -- two words of the prefix, the slash count and the probe. The tests below
// assert that neither rejects a route Route.match accepts.
func routeFilterRejects(route *Route, path string) bool {
	head := pathHeadWord(path)
	if route.prefixRejects(head) {
		return true
	}
	var scan routeScan
	scan.init(path, head, strings.Count(path, "/"))
	h := newScanHead(route)
	return h.rejects(scan.head, scan.head2, scan.slash) ||
		h.probeLen != 0 && scan.probes.headRejects(&h, path)
}

// registerFilterRoutes registers patterns on a real App and returns the routes
// the router will actually serve, tree built and prefix filters computed.
//
// The differential tests must not hand-build Route literals: register derives
// path, Params, star and root from three different strings (raw, prettified and
// escape-stripped), and a filter bug that only shows up when those disagree —
// as the escaped-star case did — is invisible to a stand-in built from one.
//
//nolint:revive // flag-parameter: the two registration modes are the point of the helper
func registerFilterRoutes(t *testing.T, patterns []string, use bool) []*Route {
	t.Helper()

	app := New()
	handler := func(c Ctx) error { return c.Next() }
	for _, pattern := range patterns {
		func() {
			// A few generated shapes are not registrable; they are not the
			// subject of this test, so skip them rather than fail.
			defer func() {
				//nolint:errcheck // the panic value is irrelevant; skipping the pattern is the point
				recover()
			}()
			if use {
				app.Use(pattern, handler)
			} else {
				app.Get(pattern, handler)
			}
		}()
	}
	_ = app.RebuildTree()

	method := app.methodInt(MethodGet)
	require.NotEqual(t, -1, method)
	routes := app.stack[method]
	require.NotEmpty(t, routes, "no routes registered")
	return routes
}

// Test_Route_PrefixFilter_Fixture proves the leading-byte filter agrees with
// Route.match across the exhaustive path-matching fixture, which covers far
// more pattern shapes than the generated set above.
func Test_Route_PrefixFilter_Fixture(t *testing.T) {
	t.Parallel()
	patterns := make([]string, 0, len(routeTestCases))
	for _, testCollection := range routeTestCases {
		patterns = append(patterns, testCollection.pattern)
	}

	for _, use := range []bool{false, true} {
		byPath := make(map[string]*Route)
		for _, route := range registerFilterRoutes(t, patterns, use) {
			byPath[route.Path] = route
		}

		for _, testCollection := range routeTestCases {
			route, ok := byPath[testCollection.pattern]
			if !ok {
				continue
			}
			for _, c := range testCollection.testCases {
				if c.partialCheck != use {
					continue
				}
				// Route.match is the oracle, not the fixture's expectation:
				// the fixture records what getMatch returns, while match wraps
				// it in the root/star/exact branches the filters stay behind.
				// A count of 0 stands match's own filters down.
				var params [maxParams]string
				if !route.match(c.url, c.url, &params, 0) {
					continue
				}
				require.False(t, routeFilterRejects(route, c.url),
					"scan filter rejected a matching route: '%s', url: '%s'", testCollection.pattern, c.url)
			}
		}
	}
}

// Test_Route_PrefixFilter_Rejects proves the filter is actually doing work:
// routes whose leading constant bytes differ from the request must be skipped
// without Route.match ever being consulted.
func Test_Route_PrefixFilter_Rejects(t *testing.T) {
	t.Parallel()

	route := func(pattern string) *Route {
		routes := registerFilterRoutes(t, []string{pattern}, false)
		require.Len(t, routes, 1)
		return routes[0]
	}
	rejects := func(pattern, path string) bool {
		return routeFilterRejects(route(pattern), path)
	}
	// prefixRejects on its own, which routeFilterRejects would hide behind the
	// scan head's first word: an unfiltered bucket relies on it alone.
	prefixRejects := func(pattern, path string) bool {
		return route(pattern).prefixRejects(pathHeadWord(path))
	}

	require.True(t, rejects("/user/subscriptions/:owner", "/user/keys/1337"))
	require.True(t, prefixRejects("/user/subscriptions/:owner", "/user/keys/1337"))
	require.True(t, rejects("/user/keys/:id", "/user/emails"))
	require.True(t, prefixRejects("/user/keys/:id", "/user/emails"))
	require.True(t, rejects("/repos/:owner", "/user/keys"))
	require.True(t, prefixRejects("/repos/:owner", "/user/keys"))
	require.False(t, rejects("/user/keys/:id", "/user/keys/1337"))
	require.False(t, prefixRejects("/user/keys/:id", "/user/keys/1337"))
	// Past the first word only the scan head's second word tells these apart.
	require.True(t, rejects("/api/v1/users/:id", "/api/v1/teams/7"))
	require.False(t, prefixRejects("/api/v1/users/:id", "/api/v1/teams/7"))
	// wildcards and leading parameters constrain nothing, so they must not filter
	require.False(t, rejects("/*", "/anything/at/all"))
	require.False(t, prefixRejects("/*", "/anything/at/all"))
	require.False(t, rejects("/:name", "/anything"))
	require.False(t, prefixRejects("/:name", "/anything"))
}

// Test_Route_PrefixFilter_EscapedStar guards a routing regression the
// leading-byte filter introduced: register derives star from the
// escape-stripped path (isStar := pathClean == "/*") but Params from parsing
// the escaped path, so `/\*` produces star=true with no params. Route.match
// returns true unconditionally for a star route, so the filter must not
// constrain it -- an earlier version only checked star inside the parametric
// branch and 404'd every path that did not literally begin with "/*".
func Test_Route_PrefixFilter_EscapedStar(t *testing.T) {
	t.Parallel()

	// An escaped star is a literal "*", for a plain route and a Use prefix alike.
	for _, caseSensitive := range []bool{false, true} {
		for _, use := range []bool{false, true} {
			app := New(Config{CaseSensitive: caseSensitive})
			handler := func(c Ctx) error { return c.SendString("ok") }
			if use {
				app.Use(`/\*`, handler)
			} else {
				app.Get(`/\*`, handler)
			}

			for path, status := range map[string]int{"/*": StatusOK, "/foo": StatusNotFound, "/bar/baz": StatusNotFound, "/": StatusNotFound} {
				resp := apptest.Do(t, app, path)
				require.Equal(t, status, resp.StatusCode,
					"escaped star route is a literal path: %q (caseSensitive=%v, use=%v)",
					path, caseSensitive, use)
			}
		}
	}
}

// Benchmark_Router_GitHub_API_Serial routes the whole fixture corpus once per
// iteration on a single goroutine.
//
// The RunParallel variants above are the right shape for measuring contention
// but are dominated by scheduler noise on small machines (±40% run to run
// observed on 4 cores), which buries the few-percent deltas that route-scan
// changes produce. This one is deterministic, so it is the benchmark to use
// when comparing scan-side changes across builds, and it exercises the case
// those changes target: a large route set where most candidates are rejected.
func Benchmark_Router_GitHub_API_Serial(b *testing.B) {
	app := New()
	registerDummyRoutes(app)
	app.startupProcess()

	// Fail loudly if the corpus stops matching, rather than silently
	// benchmarking a router that answers 404 for everything.
	c := &fasthttp.RequestCtx{}
	for _, route := range routesFixture.TestRoutes {
		c.Request.Header.SetMethod(route.Method)
		c.URI().SetPath(route.Path)
		ctx := acquireDefaultCtxForRouterBenchmark(b, app, c)
		match, err := app.next(ctx)
		app.ReleaseCtx(ctx)
		require.NoError(b, err, "route: %s %s", route.Method, route.Path)
		require.True(b, match, "route: %s %s", route.Method, route.Path)
	}

	b.ReportAllocs()
	for b.Loop() {
		for i := range routesFixture.TestRoutes {
			c.Request.Header.SetMethod(routesFixture.TestRoutes[i].Method)
			c.URI().SetPath(routesFixture.TestRoutes[i].Path)
			ctx := acquireDefaultCtxForRouterBenchmark(b, app, c)
			//nolint:errcheck // verified above; the loop measures the scan
			_, _ = app.next(ctx)
			app.ReleaseCtx(ctx)
		}
	}
}

// referenceEndpoint resolves a request the slow, obvious way: walk the method's
// route stack in registration order and return the first endpoint whose
// Route.match accepts, with every optimization the router layers on top of that
// deliberately switched off.
//
//   - the leading-byte filter is bypassed by not applying it
//   - the slash-count quick-reject is bypassed by passing an unknown count (0)
//   - the "/const/:param" specialization is bypassed on a parser copy
//
// Those three may only ever skip work, so the router has to land on the same
// route with the same parameters as this does.
//
// Bucket selection is bypassed too -- this walks app.stack rather than a tree
// bucket -- but it does not belong on that list, because it is not purely
// work-skipping: a route whose first constant segment is at least
// maxDetectionPaths bytes is filed under a hashed bucket only, while a request
// shorter than that hashes to 0 and scans globals, so the route is invisible to
// the router but visible here. `/a/*` against `/a` is such a pair. That is
// long-standing router behavior, not something this file's subject introduced,
// so the corpus stays clear of the shape rather than asserting against it.
//
//nolint:gocritic // unnamedResult: the pair is documented above and named results trip nonamedreturns
func referenceEndpoint(app *App, method int, detectionPath, path string) (*Route, [maxParams]string) {
	for _, route := range app.stack[method] {
		if route.use || route.mount {
			continue
		}

		// Route is a value copy; routeParser is a value field and segs is only
		// read, so clearing the specialization here cannot affect the real one.
		ref := *route
		ref.routeParser.constParam = false

		var got [maxParams]string
		if ref.match(detectionPath, path, &got, 0) {
			return route, got
		}
	}
	return nil, [maxParams]string{}
}

// Test_Router_ScanMatchesReference is the end-to-end guard for the router's
// candidate scan. Across every routing-relevant config combination it drives
// real requests through App.next and requires the resulting route and captured
// parameters to equal what an unoptimized linear scan of the route stack picks.
//
// This is the property every scan-side optimization has to preserve, and it
// covers them together rather than one at a time: bucket selection, the
// leading-byte filter, the slash-count bounds and the "/const/:param"
// specialization all have to agree with the obvious implementation. It replaces
// a golden-file comparison, so it needs no regeneration when routes are added.
func Test_Router_ScanMatchesReference(t *testing.T) {
	t.Parallel()

	patterns := []string{
		"/", "/health", "/health/live", "/api", "/api/v1/status",
		"/user/keys/:id", "/user/emails", "/user/following/:user",
		"/repos/:owner/:repo", "/repos/:owner/:repo/issues",
		"/repos/:owner/:repo/issues/:number", "/repos/:owner/:repo/stargazers",
		"/api/v1/:resource", "/api/v1/:resource/:id", "/api/v1/:resource/:id/edit",
		"/files/*", "/assets/+", "/mixed/:a-:b", "/dotted/:a.:b",
		"/opt/:a?", "/opt/:a/:b?", "/num/:id<int>", "/word/:name<alpha>",
		// Optional param whose constant does not end in '/', so the constant
		// carries no optional slash: the one shape where the specialization
		// gate's own optional-param check is what keeps it out.
		"/optx:a?",
		`/esc/\:literal`, `/esc/\*`, "/deep/:a/:b/:c/:d/:e",
		"/CaseSensitive/Path", "/trailing/slash/", "/x/:y/fixedEnd",
		// Registered last so they act as catch-alls: a filter that wrongly
		// constrains a star route shows up here as an unexpected miss.
		`/\*`, "/*",
	}

	// No empty path: httptest.NewRequest rejects it, and fasthttp never yields
	// one anyway. Route.match's empty-detection-path behavior is covered by
	// Test_Route_PrefixFilter_Differential, which calls match directly.
	paths := []string{
		"/", "/health", "/health/", "/health/live", "/HEALTH",
		"/api", "/api/v1/status", "/api/v1/things", "/api/v1/things/7",
		"/api/v1/things/7/edit", "/user/keys/1337", "/user/emails",
		"/user/following/octocat", "/repos/a/b", "/repos/a/b/issues",
		"/repos/a/b/issues/42", "/repos/a/b/stargazers", "/repos/a",
		"/files/any/deep/path", "/assets/x/y", "/mixed/p-q", "/dotted/p.q",
		"/opt", "/opt/", "/opt/one", "/opt/one/two", "/num/123", "/num/abc",
		"/optx", "/optxy", "/optx/z",
		"/word/abc", "/word/123", "/esc/:literal", "/esc/*", "/esc/anything",
		"/deep/1/2/3/4/5", "/CaseSensitive/Path", "/casesensitive/path",
		"/trailing/slash", "/trailing/slash/", "/x/y/fixedEnd", "/x/y/z/fixedEnd",
		"/nope", "/a/b/c/d/e/f/g",
	}

	for _, caseSensitive := range []bool{false, true} {
		for _, strictRouting := range []bool{false, true} {
			for _, skipUnmatched := range []bool{false, true} {
				for _, withMiddleware := range []bool{false, true} {
					for _, filtered := range []bool{false, true} {
						cfg := Config{
							CaseSensitive:       caseSensitive,
							StrictRouting:       strictRouting,
							SkipUnmatchedRoutes: skipUnmatched,
						}
						name := fmt.Sprintf("cs=%v/sr=%v/skip=%v/mw=%v/filtered=%v",
							caseSensitive, strictRouting, skipUnmatched, withMiddleware, filtered)

						t.Run(name, func(t *testing.T) {
							t.Parallel()
							assertScanMatchesReference(t, &cfg, withMiddleware, filtered, patterns, paths)
						})
					}
				}
			}
		}
	}
}

//nolint:revive // flag-parameter: withMiddleware and filtered select the app shape under test
func assertScanMatchesReference(t *testing.T, cfg *Config, withMiddleware, filtered bool, patterns, paths []string) {
	t.Helper()

	app := New(*cfg)
	if withMiddleware {
		// Middleware changes which scan path App.next takes, and it is what
		// enables the SkipUnmatchedRoutes lookahead, so both shapes matter.
		app.Use(func(c Ctx) error { return c.Next() })
	}
	if filtered {
		// Global routes land in every bucket, so filterMinBucket of them
		// carry each bucket past the threshold, and the scan then walks it
		// through routeScan.skip instead of loading every route. Registered
		// first, every request is scanned past them, and they match no path
		// of the corpus.
		for i := range filterMinBucket {
			app.Get("/:filler/zzfill"+strconv.Itoa(i), func(c Ctx) error { return c.Next() })
		}
	}

	// Requests run through the real handler rather than App.next directly, so
	// the lookahead is live: resolveSkip is reached only from requestHandler,
	// and firstMatchIndex is -1 straight out of Reset. Driving next() by hand
	// left the SkipUnmatchedRoutes axis inert -- eight of the sixteen configs
	// re-ran the other eight, and the prefix-filter rejects in resolveSkip and
	// the firstMatchIndex fast path in next() went untouched, which are exactly
	// the paths where a disagreement picks a wrong route rather than a slow one.
	var (
		hitRoute       *Route
		hitParams      [maxParams]string
		hitLookahead   int
		sawLookahead   bool
		registeredHits int
	)
	//nolint:unparam // must satisfy fiber.Handler; the recording is the point
	handler := func(c Ctx) error {
		defaultCtx, ok := c.(*DefaultCtx)
		require.True(t, ok)
		hitRoute = c.Route()
		hitParams = defaultCtx.values
		hitLookahead = defaultCtx.firstMatchIndex
		if hitLookahead >= 0 {
			sawLookahead = true
		}
		return nil
	}
	for _, pattern := range patterns {
		func() {
			//nolint:errcheck // an unregistrable pattern is not this test's subject
			defer func() { recover() }()
			app.Get(pattern, handler)
			registeredHits++
		}()
	}
	require.Equal(t, len(patterns), registeredHits,
		"every pattern in the corpus must register; a silently dropped one shrinks the test")
	app.startupProcess()

	method := app.methodInt(MethodGet)
	require.NotEqual(t, -1, method)
	if _, filter := app.treeIndex[method].lookup(0); filtered {
		require.NotNil(t, filter, "the fillers must carry the buckets past filterMinBucket")
	} else {
		require.Nil(t, filter, "the corpus alone must stay below filterMinBucket")
	}

	fctx := &fasthttp.RequestCtx{}
	fctx.Request.Header.SetMethod(MethodGet)

	for _, path := range paths {
		// The reference needs the detection path the router will derive.
		// Normalization is shared machinery, not something the scan optimizes,
		// so reading it from a real ctx is not begging the question.
		fctx.URI().SetPath(path)
		ctx, ok := app.AcquireCtx(fctx).(*DefaultCtx)
		require.True(t, ok)
		detectionPath := app.toString(ctx.detectionPath)
		routePath := app.toString(ctx.path)
		app.ReleaseCtx(ctx)

		wantRoute, wantParams := referenceEndpoint(app, method, detectionPath, routePath)

		hitRoute, hitParams, hitLookahead = nil, [maxParams]string{}, -1
		resp := apptest.Do(t, app, path)

		if wantRoute == nil {
			require.Nil(t, hitRoute,
				"router matched %q via %q but the reference scan found nothing",
				path, routeLabel(hitRoute))
			require.NotEqual(t, StatusOK, resp.StatusCode,
				"unmatched path %q should not answer 200", path)
			continue
		}

		require.NotNil(t, hitRoute,
			"reference scan matched %q via %q but the router ran no handler",
			path, wantRoute.Path)
		require.Same(t, wantRoute, hitRoute,
			"path %q: router chose %q, reference scan chose %q",
			path, routeLabel(hitRoute), wantRoute.Path)
		require.Equal(t, wantParams[:len(wantRoute.Params)], hitParams[:len(wantRoute.Params)],
			"path %q via %q: captured parameters diverged", path, wantRoute.Path)
	}

	// Pin the axis as live. Without this the test can quietly stop exercising
	// the lookahead again and still pass every assertion above.
	if withMiddleware && cfg.SkipUnmatchedRoutes {
		require.True(t, sawLookahead,
			"SkipUnmatchedRoutes lookahead never resolved an endpoint; the axis is inert again")
	}
}

// routeLabel names a route for a failure message, tolerating nil.
func routeLabel(r *Route) string {
	if r == nil {
		return "<none>"
	}
	return r.Path
}

// Test_ResolveSkip_StaticSlashGate covers the '/'-count gate that lets
// resolveSkip skip hashing the detection path for the static endpoint index.
// The gate is a pure filter over an exact-match lookup, so a wrong bit turns a
// matching static route into a 404 -- silently, and only when
// SkipUnmatchedRoutes and middleware are both on and some route has a
// parameter, which is what makes the '/' census run in the first place.
//
// The routes here deliberately include no wildcard or root fallback: with one
// present, tier 2 would rescue a botched tier 1 and hide the bug.
func Test_ResolveSkip_StaticSlashGate(t *testing.T) {
	t.Parallel()

	statics := []string{
		"/health",
		"/api/status",
		"/a/b/c/d",
		"/one/two/three/four/five/six",
	}

	app := New(Config{SkipUnmatchedRoutes: true})
	app.Use(func(c Ctx) error { return c.Next() })
	for _, path := range statics {
		app.Get(path, func(c Ctx) error { return c.SendString("ok") })
	}
	// A parametric route at a depth none of the statics share, so the slash
	// census actually runs and the static bitmask is what decides tier 1.
	app.Get("/params/:a/:b/:c/:d/:e/:f/:g", func(c Ctx) error { return c.SendString("param") })

	for _, path := range statics {
		resp := apptest.Do(t, app, path)
		require.Equal(t, StatusOK, resp.StatusCode, "static route %q must still match", path)
	}

	resp := apptest.Do(t, app, "/params/1/2/3/4/5/6/7")
	require.Equal(t, StatusOK, resp.StatusCode, "parametric route must still match")

	// Paths that share a '/' count with a registered static but nothing else,
	// so a gate that is too permissive is not mistaken for a correct one.
	for _, path := range []string{"/missing", "/api/missing", "/a/b/c/x", "/nope/nope"} {
		resp := apptest.Do(t, app, path)
		require.Equal(t, StatusNotFound, resp.StatusCode, "unmatched path %q must 404", path)
	}
}

// Test_Route_PrefixFilter_NotStale proves every path that can produce or alter
// a Route leaves its leading-byte filter consistent with the route's current
// path, params and parser.
//
// The filter is computed where routes are built rather than in buildTree,
// because buildTree writes to routes that in-flight requests are reading. That
// trades a torn-read hazard for a staleness hazard, and this is what closes the
// second one: it recomputes the filter for every registered route and requires
// it to equal what is stored. A future in-place rewrite of a route's path that
// forgets to refresh the filter fails here.
func Test_Route_PrefixFilter_NotStale(t *testing.T) {
	t.Parallel()

	assertFresh := func(t *testing.T, app *App, stage string) {
		t.Helper()
		checked := 0
		for method, routes := range app.stack {
			for _, route := range routes {
				wantWord, wantMask := computePrefixFilter(route)
				require.Equal(t, wantWord, route.prefix,
					"%s: stale prefix on %s %q", stage, app.config.RequestMethods[method], route.Path)
				require.Equal(t, wantMask, route.prefixMask,
					"%s: stale prefixMask on %s %q", stage, app.config.RequestMethods[method], route.Path)
				checked++
			}
		}
		require.NotZero(t, checked, "%s: no routes to check", stage)
	}

	handler := func(c Ctx) error { return c.Next() }

	// register, groups, and the auto-HEAD copyRoute path
	app := New()
	app.Get("/plain", handler)
	app.Get("/plain/:id", handler)
	app.Use("/mw", handler)
	app.Get("/*", handler)
	app.Get(`/\*`, handler)
	group := app.Group("/grp", handler)
	group.Get("/:x", handler)
	group.Get("/nested/:y/end", handler)

	// mounted sub-app: addPrefixToRoute rewrites path, parser, star and root
	// in place, which is the only route mutation outside construction
	sub := New()
	sub.Get("/inner/:y", handler)
	sub.Get("/*", handler)
	sub.Use("/subuse", handler)
	app.Use("/mounted", sub)

	app.startupProcess()
	assertFresh(t, app, "after startup")

	// routes added and removed on a live app, the flow RebuildTree exists for
	app.Get("/late/:id", handler)
	app.RemoveRoute("/plain")
	_ = app.RebuildTree()
	assertFresh(t, app, "after runtime registration and RebuildTree")

	// a second app with the opposite config, since the filter is derived from
	// the case-folded and slash-trimmed forms
	strict := New(Config{CaseSensitive: true, StrictRouting: true})
	strict.Get("/Mixed/Case/:Id", handler)
	strict.Get("/trailing/", handler)
	strict.Use("/Pre", handler)
	strict.startupProcess()
	assertFresh(t, strict, "case-sensitive strict app")
}

// Test_RouteTree_SlotSpreadsAcrossLargeTables pins that the probe index is
// taken from the high bits of the multiply, so it can address the whole table.
//
// Fibonacci hashing only works if the index comes off the top: the multiply
// moves the entropy of a tree hash's low bytes upward, and a fixed shift bounds
// the result by whatever it exposes regardless of how large the table is. With
// a fixed >>16 every probe started below slot 65536, so a table sized past that
// had an unreachable upper half and the lower half degraded into a linear scan
// -- in exactly the large-route-set case the index exists to speed up.
func Test_RouteTree_SlotSpreadsAcrossLargeTables(t *testing.T) {
	t.Parallel()

	// Detection paths always begin with '/', so the first hash byte is fixed
	// and the other two vary: 65536 distinct prefixes, sizing the table to
	// 131072 slots -- just past where a 16-bit index runs out.
	buckets := make(map[int][]*Route, 1<<16)
	for b1 := range 256 {
		for b2 := range 256 {
			buckets[int('/')<<16|b1<<8|b2] = make([]*Route, 1)
		}
	}

	tree := buildRouteTree(buckets)
	require.Len(t, tree.hashes, 1<<17)

	var maxSlot uint32
	for hash := range buckets {
		if s := tree.slot(hash); s > maxSlot {
			maxSlot = s
		}
		require.Less(t, tree.slot(hash), uint32(len(tree.hashes)),
			"slot out of range for hash %#x", hash)
	}
	require.Greater(t, maxSlot, uint32(len(tree.hashes)/2),
		"probe starts never reach the upper half of the table")

	// The table still resolves every key to its own bucket.
	for hash, want := range buckets {
		got, _ := tree.lookup(hash)
		if len(got) != 1 || &got[0] != &want[0] {
			t.Fatalf("lookup returned the wrong bucket for hash %#x", hash)
		}
	}
}

// Test_RebuildTree_PreservesPublishedBuckets ensures rebuilding cannot rewrite
// the backing arrays that an in-flight request may still be scanning.
func Test_RebuildTree_PreservesPublishedBuckets(t *testing.T) {
	t.Parallel()

	app := New()
	app.Get("/aa/first", testEmptyHandler)
	app.Get("/aa/removed", testEmptyHandler)
	app.Get("/aa/last", testEmptyHandler)
	app.startupProcess()

	published, _ := routerTestBucket(app, MethodGet, "/aa")
	require.Equal(t, []string{"/aa/first", "/aa/removed", "/aa/last"}, routeTreePaths(published))
	want := append([]*Route(nil), published...)

	app.RemoveRoute("/aa/removed", MethodGet)
	app.RebuildTree()

	// The bucket the previous build published is what an in-flight request may
	// still be scanning, so the rebuild must not have written through it.
	require.Equal(t, want, published)

	// The rebuild must still take effect: the new bucket drops the route.
	rebuilt, _ := routerTestBucket(app, MethodGet, "/aa")
	require.Equal(t, []string{"/aa/first", "/aa/last"}, routeTreePaths(rebuilt))
}

// Test_BuildTree_BucketsAreExactlyFull guards the invariant the shared bucket
// arena rests on: every bucket's cap is exactly what buildTree appends to it.
// Slack would leave a bucket able to grow into the neighboring window, which
// is the cross-bucket write the fresh-arena rule exists to prevent.
func Test_BuildTree_BucketsAreExactlyFull(t *testing.T) {
	t.Parallel()

	app := New()
	app.Use("/", testEmptyHandler)
	app.Get("/aa/first", testEmptyHandler)
	app.Get("/aa/second", testEmptyHandler)
	app.Get("/bb/only", testEmptyHandler)
	app.Post("/aa/first", testEmptyHandler)
	app.Get("/x", testEmptyHandler)
	app.startupProcess()

	for method, buckets := range app.treeStack {
		for treeHash, bucket := range buckets {
			require.Equal(t, len(bucket), cap(bucket),
				"method %d bucket %d: cap must equal len so appends cannot cross into the next bucket",
				method, treeHash)
		}
	}
}

// routeTreePaths returns the normalized paths of a tree bucket's routes, so
// assertions report path names instead of route pointers.
func routeTreePaths(routes []*Route) []string {
	paths := make([]string, len(routes))
	for i, route := range routes {
		paths[i] = route.path
	}
	return paths
}

// routerTestBucket returns the routes and the filter the method's route tree
// holds for the bucket keyed by the first three bytes of prefix, e.g. "/ro".
func routerTestBucket(app *App, method, prefix string) ([]*Route, *bucketFilter) {
	treeHash := int(prefix[0])<<16 | int(prefix[1])<<8 | int(prefix[2])
	return app.treeIndex[app.methodInt(method)].lookup(treeHash)
}

// Test_Route_PrefixFilter_UnconstrainedShapes covers the guards in
// computePrefixFilter that disable the filter when a route's first segment
// constrains nothing.
//
// Neither shape is reachable through the public API -- register always forces a
// leading '/', so a parsed route's first segment is a constant and its parser
// is never empty -- but the guards are what make that an assumption rather than
// a dependency, and a filter that stayed enabled for either would reject paths
// the route matches.
func Test_Route_PrefixFilter_UnconstrainedShapes(t *testing.T) {
	t.Parallel()

	// A pattern whose very first segment is a parameter, which only parses
	// this way without the leading slash register would add.
	parser := parseRoute(":name", regexp.MustCompile)
	require.NotEmpty(t, parser.segs)
	require.True(t, parser.segs[0].IsParam, "expected a leading parameter segment")

	leadingParam := &Route{routeParser: parser, Params: parser.params, path: ":name", Path: ":name"}
	word, mask := computePrefixFilter(leadingParam)
	require.Zero(t, mask, "a leading parameter must disable the filter")
	require.Zero(t, word)

	// A parametric route with no parsed segments at all.
	empty := &Route{Params: []string{"x"}}
	word, mask = computePrefixFilter(empty)
	require.Zero(t, mask, "an empty parser must disable the filter")
	require.Zero(t, word)

	// A disabled filter must never reject, whatever the path.
	for _, path := range []string{"", "/", "/anything", "/a/b/c"} {
		head := pathHeadWord(path)
		require.False(t, leadingParam.prefixRejects(head), "path %q", path)
		require.False(t, empty.prefixRejects(head), "path %q", path)
	}
	// Nor may either prefix word of the scanHead a filtered bucket holds.
	for _, route := range []*Route{leadingParam, empty} {
		h := newScanHead(route)
		require.Zero(t, h.prefixMask)
		require.Zero(t, h.prefixMask2)
	}
}

// Test_Route_OptionalSlash_SingleCharSegment guards the tree-bucket key against
// the optional trailing slash. A leading constant of exactly maxDetectionPaths
// bytes ("/a/") can match a detection path one byte shorter ("/a"), and that
// shorter path always hashes to bucket 0 — so bucketing the route under the
// hash of the full constant made it unreachable, while the otherwise identical
// "/ab/..." routes matched.
func Test_Route_OptionalSlash_SingleCharSegment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		pattern string
		path    string
	}{
		{"/a/:id?", "/a"},
		{"/a/:id?", "/a/"},
		{"/a/:id?", "/a/1"},
		{"/a/*", "/a"},
		{"/a/*", "/a/"},
		{"/a/*", "/a/b"},
		{"/ab/:id?", "/ab"},
		{"/ab/*", "/ab"},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+" "+tt.path, func(t *testing.T) {
			t.Parallel()
			app := New()
			app.Get(tt.pattern, func(c Ctx) error { return c.SendString("ok") })

			apptest.Expect(t, app, tt.path, StatusOK)
		})
	}
}

// Test_Route_URL_RefusesUnrepresentableRoute covers a route whose own path
// starts more than one slash. No relative URL names it: two leading slashes open
// an authority, so "//internal" resolves to the host "internal" rather than a
// path here, and collapsing to one reaches a different route. Both silently
// composed the wrong thing, one of them an open redirect, so it errors instead.
func Test_Route_URL_RefusesUnrepresentableRoute(t *testing.T) {
	t.Parallel()

	app := New()
	app.Get("//internal", func(c Ctx) error { return c.SendString("internal") }).Name("dbl")
	app.Get("//*", func(c Ctx) error { return c.SendString("wild") }).Name("dblwild")
	// The parser deletes the tab, so this path is read as "//internal" too. The
	// byte is not there to be seen by the time anything acts on the composition.
	app.Get("/\t/internal", func(c Ctx) error { return c.SendString("tabbed") }).Name("tab")
	// A trailing space goes the same way: the parser strips a run of controls or
	// spaces from either end, so this route is read as "/internal", which is a
	// different one. Reachable as "/internal%20" under UnescapePath, but no URL
	// composed here can name it.
	app.Get("/internal ", func(c Ctx) error { return c.SendString("spaced") }).Name("space")
	app.Get("/*", func(c Ctx) error { return c.SendString("ok") }).Name("wild")

	for _, name := range []string{"dbl", "dblwild", "tab", "space"} {
		url, err := app.GetRoute(name).URL(Map{"*": "evil.com"})
		require.ErrorIs(t, err, ErrRouteNotRepresentable, name)
		require.Empty(t, url, name)
	}

	// A wildcard route still composes, and a value cannot open an authority.
	url, err := app.GetRoute("wild").URL(Map{"*": "/evil.com"})
	require.NoError(t, err)
	require.Equal(t, "/evil.com", url)

	// What that composed reaches is this origin, not evil.com.
	ref, err := neturl.Parse(url)
	require.NoError(t, err)
	require.Empty(t, ref.Host)
}

// Constraints must not be lowercased along with the pattern.
func Test_Router_Constraints_CaseInsensitiveRouting(t *testing.T) {
	t.Parallel()

	for _, caseSensitive := range []bool{false, true} {
		t.Run(fmt.Sprintf("CaseSensitive=%v", caseSensitive), func(t *testing.T) {
			t.Parallel()

			app := New(Config{CaseSensitive: caseSensitive})
			app.RegisterCustomConstraint(&evenConstraint{})
			sub := New(Config{CaseSensitive: caseSensitive})
			sub.Get("/:code<regex([A-Z]{2})>", func(c Ctx) error { return c.SendString(c.Params("code")) })
			app.Use("/api", sub)

			app.Get("/code/:code<regex([A-Z]{2})>", func(c Ctx) error { return c.SendString(c.Params("code")) })
			app.Get(`/word/:w<regex(^\D+$)>`, func(c Ctx) error { return c.SendString(c.Params("w")) })
			app.Get("/when/:d<datetime(2006-01-02T15\\:04\\:05Z07\\:00)>", func(c Ctx) error { return c.SendString(c.Params("d")) })
			app.Get("/even/:n<isEven>", func(c Ctx) error { return c.SendString(c.Params("n")) })

			testCases := []struct {
				path   string
				status int
			}{
				{path: "/code/US", status: StatusOK},
				{path: "/code/us", status: StatusNotFound},
				{path: "/api/US", status: StatusOK},
				{path: "/api/us", status: StatusNotFound},
				{path: "/word/abc", status: StatusOK},
				{path: "/word/123", status: StatusNotFound},
				{path: "/when/2024-01-02T15:04:05Z", status: StatusOK},
				{path: "/when/not-a-date", status: StatusNotFound},
				{path: "/even/4", status: StatusOK},
				{path: "/even/3", status: StatusNotFound},
				{path: "/even/abc", status: StatusNotFound},
			}
			for _, tc := range testCases {
				resp := apptest.Do(t, app, tc.path)
				require.Equal(t, tc.status, resp.StatusCode, "GET %s", tc.path)
			}

			cfg := Config{CaseSensitive: caseSensitive}
			require.True(t, RoutePatternMatch("/US", "/:code<regex([A-Z]{2})>", cfg))
			require.False(t, RoutePatternMatch("/us", "/:code<regex([A-Z]{2})>", cfg))
		})
	}
}

// evenConstraint accepts even integers; its mixed-case name is the point.
type evenConstraint struct{}

func (*evenConstraint) Name() string {
	return "isEven"
}

func (*evenConstraint) Execute(param string, _ ...string) bool {
	n, err := strconv.Atoi(param)
	return err == nil && n%2 == 0
}

func Test_Router_MethodOverride_ContinuesInNewMethodTree(t *testing.T) {
	t.Parallel()

	build := func(app *App) {
		app.Get("/a", func(c Ctx) error { return c.SendString("a") })
		app.Use(func(c Ctx) error {
			c.Method(MethodPost)
			return c.Next()
		})
		app.Post("/x", func(c Ctx) error { return c.SendString("post x") })
	}

	testCases := []struct {
		app  *App
		name string
	}{
		{name: "default ctx", app: New()},
		{name: "custom ctx", app: newCustomApp()},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			build(tc.app)
			apptest.ExpectBody(t, tc.app, "/x", StatusOK, "post x")
		})
	}

	// SkipUnmatchedRoutes answers 405 before middleware runs, so no override happens.
	t.Run("skip unmatched routes", func(t *testing.T) {
		t.Parallel()

		app := New(Config{SkipUnmatchedRoutes: true})
		build(app)
		apptest.Expect(t, app, "/x", StatusMethodNotAllowed)
	})
}

func Test_Router_EscapedStar_IsLiteral(t *testing.T) {
	t.Parallel()

	app := New()
	app.Get(`/\*`, func(c Ctx) error { return c.SendString("star") })

	apptest.Expect(t, app, "/*", StatusOK)

	for _, path := range []string{"/anything", "/a/b/c", "/"} {
		resp := apptest.Do(t, app, path)
		require.Equal(t, StatusNotFound, resp.StatusCode, "GET %s", path)
	}

	require.True(t, RoutePatternMatch("/*", `/\*`))
	require.False(t, RoutePatternMatch("/anything", `/\*`))
	require.True(t, RoutePatternMatch("/anything", "/*"))
}

func Test_Router_StrictRouting_ParamTrailingSlash(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name   string
		path   string
		status int
		strict bool
	}{
		{name: "strict with slash", strict: true, path: "/a/x/", status: StatusOK},
		{name: "strict without slash", strict: true, path: "/a/x", status: StatusNotFound},
		{name: "strict static with slash", strict: true, path: "/b/c/", status: StatusOK},
		{name: "strict static without slash", strict: true, path: "/b/c", status: StatusNotFound},
		{name: "lenient with slash", strict: false, path: "/a/x/", status: StatusOK},
		{name: "lenient without slash", strict: false, path: "/a/x", status: StatusOK},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app := New(Config{StrictRouting: tc.strict})
			app.Get("/a/:id/", func(c Ctx) error { return c.SendString(c.Params("id")) })
			app.Get("/b/c/", func(c Ctx) error { return c.SendString("c") })

			apptest.Expect(t, app, tc.path, tc.status)

			cfg := Config{StrictRouting: tc.strict}
			pattern := "/a/:id/"
			if strings.HasPrefix(tc.path, "/b") {
				pattern = "/b/c/"
			}
			require.Equal(t, tc.status == StatusOK, RoutePatternMatch(tc.path, pattern, cfg))
		})
	}
}

// routerTestExpectRawBody serves fctx through handler and asserts a 200 response
// whose body is want. It is for request targets httptest rejects, which have to
// reach the router as raw fasthttp requests; msgAndArgs name the case in a failure.
func routerTestExpectRawBody(tb testing.TB, handler fasthttp.RequestHandler, fctx *fasthttp.RequestCtx, want string, msgAndArgs ...any) {
	tb.Helper()

	handler(fctx)
	require.Equal(tb, StatusOK, fctx.Response.StatusCode(), msgAndArgs...)
	require.Equal(tb, want, string(fctx.Response.Body()), msgAndArgs...)
}

func Test_Router_UnescapePath_PlusIsLiteral(t *testing.T) {
	t.Parallel()

	app := New(Config{UnescapePath: true})
	app.Get("/u/:name", func(c Ctx) error {
		return c.SendString(c.Params("name") + "|" + c.Path())
	})

	testCases := []struct {
		path string
		body string
	}{
		{path: "/u/john+doe", body: "john+doe|/u/john+doe"},
		{path: "/u/john%2Bdoe", body: "john+doe|/u/john+doe"},
		{path: "/u/john%20doe", body: "john doe|/u/john doe"},
		{path: "/u/a%zzb", body: "a%zzb|/u/a%zzb"},
		{path: "/u/end%2", body: "end%2|/u/end%2"},
	}
	// Through the raw handler: net/http rejects malformed escapes.
	handler := app.Handler()
	for _, tc := range testCases {
		routerTestExpectRawBody(t, handler, routerTestRequestCtx(MethodGet, tc.path), tc.body, "GET %s", tc.path)
	}

	require.True(t, RoutePatternMatch("/u/john+doe", "/u/john+doe", Config{UnescapePath: true}))
	require.False(t, RoutePatternMatch("/u/john+doe", "/u/john doe", Config{UnescapePath: true}))
}

// Test_Router_StrayPercent pins what a "%" that begins no escape becomes in
// c.Path(). fasthttp copies it as sent, so a path holding nothing else to
// decode or resolve keeps its parsed length, passes pathNeedsNormalization and
// is matched as sent. A path normalized for another reason keeps it as sent
// too, unless an escape decoded after it would complete a new escape with it;
// then it is written as "%25". Either way c.Path() never holds an escape of an
// unreserved character, and the same name comes out alike whatever else the
// path holds. httptest rejects these spellings, so the requests go through the
// raw handler.
func Test_Router_StrayPercent(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		path string
		body string
	}{
		// nothing to decode or resolve: matched as sent
		{path: "/a%zzb", body: "/a%zzb"},
		{path: "/trailing%2", body: "/trailing%2"},
		{path: "/%", body: "/%"},
		// normalized for something else: the stray "%" is kept
		{path: "/%2g%41", body: "/%2gA"},
		{path: "/x%zz/%41", body: "/x%zz/A"},
		{path: "/a%zzb/./c", body: "/a%zzb/c"},
		{path: "/a//%zzb", body: "/a//%zzb"},
		{path: "/files/a%2Fb/100%", body: "/files/a%2Fb/100%"},
		// a decoded escape would complete an escape with it: "%25"
		{path: "/%%370rivate", body: "/%2570rivate"},
		{path: "/x/%%32%65git", body: "/x/%252egit"},
	}
	for _, caseSensitive := range []bool{false, true} {
		app := New(Config{CaseSensitive: caseSensitive})
		app.Use(func(c Ctx) error {
			return c.SendString(c.Path())
		})
		handler := app.Handler()
		for _, tc := range testCases {
			routerTestExpectRawBody(t, handler, routerTestRequestCtx(MethodGet, tc.path), tc.body, "CaseSensitive=%v GET %s", caseSensitive, tc.path)
		}
	}
}

// Test_Router_StrayPercentParamIsStable pins that a route parameter holding a
// stray "%" does not depend on the rest of the path: "/files/100%" is matched
// as sent, "/files/./100%" and "/files/x/../100%" are normalized for their dot
// segments, and under DisablePathNormalizing every "%" sends the path through
// normalization. All of them give the parameter "100%".
func Test_Router_StrayPercentParamIsStable(t *testing.T) {
	t.Parallel()

	app := New()
	app.Get("/files/:name", func(c Ctx) error {
		return c.SendString(c.Params("name"))
	})
	handler := app.Handler()
	for _, disable := range []bool{false, true} {
		for _, target := range []string{"/files/100%", "/files/./100%", "/files/x/../100%", "/files/%2e/100%"} {
			fctx := routerTestRequestCtx(MethodGet, target)
			fctx.Request.URI().DisablePathNormalizing = disable
			routerTestExpectRawBody(t, handler, fctx, "100%", "DisablePathNormalizing=%v GET %s", disable, target)
		}
	}
}

// Test_Router_UnrootedTargetIsRooted pins that a request target without a
// leading slash, the absolute form with an empty path or the asterisk form,
// is routed as the rooted path fasthttp reports for it. Unrooted, it slipped
// past every root-level Use middleware while a "/*" route still matched it.
// httptest rejects these targets, so the requests go through the raw handler.
func Test_Router_UnrootedTargetIsRooted(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		method, target, path string
	}{
		{MethodGet, "http://example.com?x=1", "/"},
		{MethodPost, "http://example.com#f", "/"},
		{MethodOptions, "*", "/*"},
		{MethodGet, "http://example.com/a/b", "/a/b"},
		// the authority form fasthttp admits for CONNECT; with one doubled
		// slash the parsed copy keeps the original's length, since the slash
		// fasthttp adds in front is the one it takes out of the pair, so the
		// length gate alone would not notice the missing slash
		{MethodConnect, "admin//secret", "/admin//secret"},
		{MethodConnect, "secret//", "/secret//"},
		{MethodConnect, "example.com:443", "/example.com:443"},
	}
	// with DisablePathNormalizing the scan, not fasthttp's parsed copy, has
	// to notice the missing slash
	for _, disable := range []bool{false, true} {
		for _, tc := range testCases {
			app := New()
			guardRan, seen := false, ""
			app.Use(func(c Ctx) error {
				guardRan = true
				return c.Next()
			})
			app.All("/*", func(c Ctx) error {
				seen = c.Path()
				return c.SendString("catch-all")
			})

			var req fasthttp.Request
			req.Header.SetMethod(tc.method)
			req.SetRequestURI(tc.target)
			req.Header.SetHost("example.com")
			var fctx fasthttp.RequestCtx
			fctx.Init(&req, nil, nil)
			if disable {
				fctx.Request.URI().DisablePathNormalizing = true
			}
			app.Handler()(&fctx)

			require.Equal(t, StatusOK, fctx.Response.StatusCode(), "disable=%v %s %s", disable, tc.method, tc.target)
			require.True(t, guardRan, "disable=%v %s %s: root middleware skipped", disable, tc.method, tc.target)
			require.Equal(t, tc.path, seen, "disable=%v %s %s", disable, tc.method, tc.target)
		}
	}

	// an override is rooted the same way
	app := New()
	app.Use(func(c Ctx) error {
		c.Path("rewritten")
		return c.Next()
	})
	app.Get("/rewritten", func(c Ctx) error {
		return c.SendString(c.Path())
	})
	resp := apptest.Do(t, app, "/original")
	body := apptest.Body(t, resp)
	require.Equal(t, StatusOK, resp.StatusCode)
	require.Equal(t, "/rewritten", body)
}

// routerTestRouteNames returns "METHOD:name" for every route registered on app
// under path, sorted.
func routerTestRouteNames(app *App, path string) []string {
	var names []string
	routes := app.GetRoutes()
	for i := range routes {
		if routes[i].Path == path {
			names = append(names, routes[i].Method+":"+routes[i].Name)
		}
	}
	slices.Sort(names)
	return names
}

func Test_App_Add_MultipleMethods_Name(t *testing.T) {
	t.Parallel()

	app := New()
	app.Add([]string{MethodGet, MethodPost}, "/x", func(c Ctx) error { return c.SendString("x") }).Name("x")

	require.Equal(t, []string{"GET:x", "POST:x"}, routerTestRouteNames(app, "/x"), "GET /x and POST /x must carry the name")
}

func Test_App_Add_MultipleMethods_NameMergedRoutes(t *testing.T) {
	t.Parallel()

	app := New()
	h := func(c Ctx) error { return c.SendString("ok") }

	app.Get("/x", h).Name("first")
	app.Post("/x", h).Name("first")
	app.Add([]string{MethodGet, MethodPost}, "/x", h).Name("second")

	require.Equal(t, []string{"GET:second", "POST:second"}, routerTestRouteNames(app, "/x"))
}

// Test_App_Add_MergedRoute_KeepsRegistrationID covers what a merge must not
// cost the route it merges into: its id pairs it with the copies the same
// registration filed under the other methods, which is how routeIndexInTree
// resumes the scan when a handler switches method mid-request.
func Test_App_Add_MergedRoute_KeepsRegistrationID(t *testing.T) {
	t.Parallel()

	app := New()
	// Sits ahead of /x in the GET bucket only, so the index the GET match
	// leaves behind is past the one POST /x holds in its own bucket: carrying
	// it over unchanged would step over the wildcard below.
	app.Get("/a", func(c Ctx) error { return c.SendString("a") })
	app.Add([]string{MethodGet, MethodPost}, "/x", func(c Ctx) error { return c.Next() })
	app.Post("/*", func(c Ctx) error { return c.SendString("wildcard") })
	// Merges into the GET route of the registration above, whose POST copy is
	// the one the switch below has to find.
	app.Get("/x", func(c Ctx) error {
		c.Method(MethodPost)
		return c.Next()
	})

	apptest.ExpectBody(t, app, "/x", StatusOK, "wildcard")
}

// Test_App_Name_MergedRouteUsesLatestGroup checks the name a merged route ends
// up with: two groups share a path, so the second registration merges into the
// first group's route, and the name it is given belongs to the group it was
// registered through.
func Test_App_Name_MergedRouteUsesLatestGroup(t *testing.T) {
	t.Parallel()

	app := New()
	h := func(c Ctx) error { return c.SendString("ok") }

	one := app.Group("/api")
	one.Name("one.")
	two := app.Group("/api")
	two.Name("two.")

	one.Get("/x", h).Name("first")
	two.Add([]string{MethodGet, MethodPost}, "/x", h).Name("second")

	require.Equal(t, []string{"GET:two.second", "POST:two.second"}, routerTestRouteNames(app, "/api/x"))
	require.Equal(t, "/api/x", app.GetRoute("two.second").Path)
	require.Empty(t, app.GetRoute("one.second").Path)
}

func Test_App_Name_OnlyLatestRegistration(t *testing.T) {
	t.Parallel()

	app := New()
	h := func(c Ctx) error { return c.SendString("ok") }

	app.Get("/x", h).Name("first")
	app.Post("/x", h).Name("first")
	// Registering another path moves /x off the tail of both stacks, so the
	// routes above survive rather than being merged into the ones below.
	app.Get("/y", h)
	app.Add([]string{MethodGet, MethodPost}, "/x", h).Name("second")

	// The earlier registrations keep their own name; only the routes the latest
	// Add created are renamed.
	require.Equal(t, []string{"GET:first", "GET:second", "POST:second"}, routerTestRouteNames(app, "/x"))
}

// Test_App_Name_OnlyLatestRegistration_ReverseMethodOrder is the test above
// with the methods of the last Add swapped: which method it finishes on is not
// allowed to decide whether the older GET route is renamed too.
func Test_App_Name_OnlyLatestRegistration_ReverseMethodOrder(t *testing.T) {
	t.Parallel()

	app := New()
	h := func(c Ctx) error { return c.SendString("ok") }

	app.Get("/x", h).Name("first")
	app.Get("/y", h)
	app.Add([]string{MethodPost, MethodGet}, "/x", h).Name("second")

	require.Equal(t, []string{"GET:first", "GET:second", "POST:second"}, routerTestRouteNames(app, "/x"))
	// The older route keeps its own name, so looking either name up lands on
	// the registration that carries it.
	require.Equal(t, "/x", app.GetRoute("first").Path)
}

func Test_App_Use_MultiplePrefixes_MountsEachPrefix(t *testing.T) {
	t.Parallel()

	app := New()
	sub := New()
	sub.Get("/x", func(c Ctx) error { return c.SendString("x") })
	app.Use([]string{"/a", "/b"}, sub)

	for _, path := range []string{"/a/x", "/b/x"} {
		apptest.Expect(t, app, path, StatusOK)
	}
}

func Test_App_Mount_Self_Panics(t *testing.T) {
	t.Parallel()

	app := New()
	require.Panics(t, func() { app.Use("/x", app) })
}

func Test_App_RebuildTree_RegistersAutoHead(t *testing.T) {
	t.Parallel()

	app := New()
	app.Get("/early", func(c Ctx) error { return c.SendString("early") })
	handler := app.Handler()

	app.Get("/late", func(c Ctx) error { return c.SendString("late") })
	app.RebuildTree()

	for _, path := range []string{"/early", "/late"} {
		fctx := routerTestRequestCtx(MethodHead, path)
		handler(fctx)
		require.Equal(t, StatusOK, fctx.Response.StatusCode(), "HEAD %s", path)
	}
}

func Test_Mount_Nested_AutoHead_SingleStartup(t *testing.T) {
	t.Parallel()

	nested := New()
	nested.Get("/x", func(c Ctx) error { return c.SendString("x") })
	sub := New()
	app := New()
	// Mount the parent first: a sub-app nested afterwards is only discovered at startup.
	app.Use("/a", sub)
	sub.Use("/b", nested)

	// One startup, then the very first request.
	handler := app.Handler()
	for _, method := range []string{MethodHead, MethodGet} {
		fctx := routerTestRequestCtx(method, "/a/b/x")
		handler(fctx)
		require.Equal(t, StatusOK, fctx.Response.StatusCode(), "%s /a/b/x", method)
	}
}

func Test_Router_PathOverride_ContinuesInNewBucket(t *testing.T) {
	t.Parallel()

	app := New()
	app.Get("/zzz/other", func(c Ctx) error { return c.SendString("other") })
	app.Use(func(c Ctx) error {
		if c.Path() == "/zzz/y" {
			c.Path("/aaa/x")
		}
		return c.Next()
	})
	app.Get("/aaa/x", func(c Ctx) error { return c.SendString("aaa x") })

	apptest.ExpectBody(t, app, "/zzz/y", StatusOK, "aaa x")
}

func Test_Router_Constraints_MixedCaseNames(t *testing.T) {
	t.Parallel()

	// Built-in constraint names are matched case-insensitively, whatever the
	// pattern spells them, and through every entry point that parses one.
	//nolint:usestdlibvars // route constraint names, not go/constant kinds
	names := []string{"INT", "Int", "iNt", "int", "BOOL", "Guid", "ALPHA", "Float", "minLen(3)", "Range(5,10)"}
	//nolint:usestdlibvars // route constraint names, not go/constant kinds
	reject := map[string]string{
		"INT": "abc", "Int": "abc", "iNt": "abc", "int": "abc",
		"BOOL": "abc", "Guid": "abc", "ALPHA": "123", "Float": "abc",
		"minLen(3)": "ab", "Range(5,10)": "99",
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			app := New()
			pattern := "/:id<" + name + ">"
			app.Get(pattern, func(c Ctx) error { return c.SendString(c.Params("id")) })

			sub := New()
			sub.Get(pattern, func(c Ctx) error { return c.SendString(c.Params("id")) })
			app.Use("/api", sub)

			bad := "/" + reject[name]
			resp := apptest.Do(t, app, bad)
			require.Equal(t, StatusNotFound, resp.StatusCode, "%s did not enforce %q", pattern, bad)

			resp = apptest.Do(t, app, "/api"+bad)
			require.Equal(t, StatusNotFound, resp.StatusCode, "mounted %s did not enforce %q", pattern, bad)

			require.False(t, RoutePatternMatch(bad, pattern), "RoutePatternMatch accepted %q for %s", bad, pattern)
		})
	}
}

func Test_App_AutoHead_PanickingHookLeavesScanIncomplete(t *testing.T) {
	t.Parallel()

	app := New()
	fail := true
	// Only the synthesized companion fails, so registration itself succeeds.
	app.Hooks().OnRoute(func(r Route) error {
		if fail && r.Method == MethodHead {
			return errors.New("boom")
		}
		return nil
	})
	app.Get("/ping", func(c Ctx) error { return c.SendString("pong") })

	require.Panics(t, func() { app.RebuildTree() })

	// The aborted scan must not be recorded as complete, or HEAD stays 405.
	fail = false
	app.RebuildTree()

	apptest.Expect(t, app, apptest.Req(MethodHead, "/ping"), StatusOK)
}

// Test_Route_Fingerprint_MatchesMatch is the safety contract of the static-path
// filter in next() and nextCustom(): the filter is only ever allowed to reject
// what Route.match would reject anyway. A route the filter speaks for (a
// non-zero fingerprint) must never match a detection path whose fingerprint
// differs, because the scan drops it without calling match at all.
//
// staticFingerprint's exclusions mirror match's branches, so this walks every
// route shape against every path and fails if the filter is ever stricter than
// the matcher it stands in for.
func Test_Route_Fingerprint_MatchesMatch(t *testing.T) {
	t.Parallel()

	app := New()
	app.Use("/mw", func(c Ctx) error { return c.Next() })
	app.Use(func(c Ctx) error { return c.Next() })
	for _, pattern := range []string{
		"/", "/users", "/users/list", "/users/:id", "/users/:id/posts", "/files/*",
		"/opt/:name?", "/long/static/path/segment", "/a", "/ab", "/abc",
	} {
		app.Get(pattern, func(Ctx) error { return nil })
	}

	paths := []string{
		"/", "/users", "/users/", "/users/list", "/users/42", "/users/42/posts",
		"/files/a/b", "/opt", "/opt/x", "/long/static/path/segment", "/mw",
		"/mw/deep", "/a", "/ab", "/abc", "/abcd", "/nope", "",
	}

	for _, route := range app.stack[app.methodInt(MethodGet)] {
		fp := staticFingerprint(route)
		if fp == 0 {
			continue // the filter does not speak for this route
		}
		for _, path := range paths {
			if fp == pathFingerprint(path) {
				continue // not rejected; match decides as usual
			}
			var params [maxParams]string
			slashes := strings.Count(path, "/")
			require.False(t, route.match(path, path, &params, slashes),
				"filter drops %q for %q but match accepts it", route.path, path)
		}
	}
}

// Test_PathFingerprint_Contract pins what the filter relies on: equal inputs
// hash equal whatever their type, and 0 is reserved for "no fingerprint".
func Test_PathFingerprint_Contract(t *testing.T) {
	t.Parallel()

	for _, path := range []string{
		"", "/", "/a", "/ab", "/abcdefg", "/abcdefgh", "/abcdefghi",
		"/users/list", "/long/static/path/segment/that/exceeds/one/word",
	} {
		require.NotZero(t, pathFingerprint(path), "fingerprint of %q must not be 0", path)
		require.Equal(t, pathFingerprint(path), pathFingerprint([]byte(path)),
			"string and []byte fingerprints must agree for %q", path)
	}

	// Routes are separated by length alone, without reading a byte.
	require.NotEqual(t, pathFingerprint("/routes/1"), pathFingerprint("/routes/11"))
	// ...and by their tail, which is where a bucket of sibling routes differs.
	require.NotEqual(t, pathFingerprint("/routes/998"), pathFingerprint("/routes/999"))
}

// routerTestStatics registers n static GET routes, prefix+"0" through
// prefix+"<n-1>", answered by handler: enough of them put one tree bucket past
// the thresholds at which it carries a filter.
func routerTestStatics(app *App, prefix string, n int, handler func(Ctx) error) {
	for i := range n {
		app.Get(prefix+strconv.Itoa(i), handler)
	}
}

// routerTestPathHandler answers with the path it was asked for.
func routerTestPathHandler(c Ctx) error { return c.SendString(c.Path()) }

// Test_RouteTree_Fingerprints_LineUp pins that a bucket's filter is the one
// built from that same bucket. next() indexes both with the same counter, so a
// slice that drifted by one entry would reject live routes; maxLen must come
// from the static routes alone, as it stands the hash down for longer paths.
// The heads are held to the same line-up.
func Test_RouteTree_Fingerprints_LineUp(t *testing.T) {
	t.Parallel()

	app := New()
	routerTestStatics(app, "/routes/", fingerprintMinBucket*2, func(Ctx) error { return nil })
	app.Use("/routes", func(c Ctx) error { return c.Next() })
	app.Get("/routes/:id/edit", func(Ctx) error { return nil })
	app.RebuildTree()

	routes, filter := routerTestBucket(app, MethodGet, "/ro")
	require.NotNil(t, filter, "a bucket this size must carry a filter")
	require.Len(t, filter.prints, len(routes))
	require.Len(t, filter.heads, len(routes))
	for i, route := range routes {
		require.Equal(t, staticFingerprint(route), filter.prints[i], "fingerprint %d is not this route's", i)
		require.Equal(t, newScanHead(route), filter.heads[i], "head %d is not this route's", i)
	}
	require.Equal(t, len("/routes/"+strconv.Itoa(fingerprintMinBucket*2-1)), filter.maxLen,
		"maxLen must be the longest static path, not the longer param route")
}

// Test_RouteTree_Fingerprints_SmallBucketNil pins the other half: a bucket below
// the threshold carries no filter, so a small app neither allocates the slices
// nor hashes its detection paths, and scans its routes directly.
func Test_RouteTree_Fingerprints_SmallBucketNil(t *testing.T) {
	t.Parallel()

	app := New()
	app.Get("/hello", func(Ctx) error { return nil })
	app.RebuildTree()

	routes, filter := routerTestBucket(app, MethodGet, "/he")
	require.Equal(t, []string{"/hello"}, routeTreePaths(routes))
	require.Nil(t, filter, "a bucket below fingerprintMinBucket must not carry a filter")
}

// Test_RouteTree_Fingerprints_CountsStaticRoutes pins that the fingerprints'
// threshold counts static routes, not bucket size: a bucket full of param
// routes has nothing for them to reject, so it must not make every request
// hash its path. Its filter still carries heads, which do reject param routes.
func Test_RouteTree_Fingerprints_CountsStaticRoutes(t *testing.T) {
	t.Parallel()

	app := New()
	for i := range fingerprintMinBucket * 2 {
		app.Get("/routes/:id/v"+strconv.Itoa(i), func(Ctx) error { return nil })
	}
	routerTestStatics(app, "/routes/", fingerprintMinBucket-1, func(Ctx) error { return nil })
	app.RebuildTree()

	routes, filter := routerTestBucket(app, MethodGet, "/ro")
	require.Len(t, routes, fingerprintMinBucket*3-1)
	require.NotNil(t, filter, "a bucket of filterMinBucket routes must carry a filter")
	require.Len(t, filter.heads, len(routes))
	require.Nil(t, filter.prints, "fewer static routes than fingerprintMinBucket must not carry fingerprints")

	app.Get("/routes/"+strconv.Itoa(fingerprintMinBucket-1), func(Ctx) error { return nil })
	app.RebuildTree()
	_, filter = routerTestBucket(app, MethodGet, "/ro")
	require.NotNil(t, filter, "a bucket of filterMinBucket routes must carry a filter")
	require.NotNil(t, filter.prints, "fingerprintMinBucket static routes must carry fingerprints")
}

// Test_Router_Fingerprint_HashedOnDemand pins when the scan hashes the path:
// never for a hit on the first candidate, never for a path longer than every
// static route in the bucket, and once for anything that scans past a route.
func Test_Router_Fingerprint_HashedOnDemand(t *testing.T) {
	t.Parallel()

	app := New()
	routerTestStatics(app, "/routes/", fingerprintMinBucket*2, func(Ctx) error { return nil })
	app.startupProcess()

	last := "/routes/" + strconv.Itoa(fingerprintMinBucket*2-1)
	miss := "/routes/" + strconv.Itoa(fingerprintMinBucket*2)
	long := "/routes/" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		path      string
		wantPrint uint64
		matched   bool
	}{
		{path: "/routes/0", wantPrint: 0, matched: true},
		{path: last, wantPrint: pathFingerprint(last), matched: true},
		{path: miss, wantPrint: pathFingerprint(miss), matched: false},
		{path: long, wantPrint: 0, matched: false},
	} {
		fctx := routerTestRequestCtx(MethodGet, tc.path)
		ctx := app.AcquireCtx(fctx).(*DefaultCtx) //nolint:errcheck,forcetypeassert // default app returns DefaultCtx

		matched, err := app.next(ctx)
		require.Equal(t, tc.matched, matched, tc.path)
		if tc.matched {
			require.NoError(t, err, tc.path)
		} else {
			require.ErrorIs(t, err, ErrNotFound, tc.path)
		}
		require.Equal(t, tc.wantPrint, ctx.pathPrint, "hashed state after routing %s", tc.path)
		app.ReleaseCtx(ctx)
	}
}

// Test_Router_LargeBucket_RoutesCorrectly drives the filter through the real
// request path, with every route shape sharing one oversized bucket so the
// fingerprint scan is the thing deciding what runs.
func Test_Router_LargeBucket_RoutesCorrectly(t *testing.T) {
	t.Parallel()

	app := New()
	var seen []string
	app.Use("/routes", func(c Ctx) error {
		seen = append(seen, "mw")
		return c.Next()
	})
	routerTestStatics(app, "/routes/", fingerprintMinBucket*4, routerTestPathHandler)
	app.Get("/routes/:id/edit", func(c Ctx) error { return c.SendString("edit:" + c.Params("id")) })
	app.Get("/routes/files/*", func(c Ctx) error { return c.SendString("star:" + c.Params("*")) })

	last := "/routes/" + strconv.Itoa(fingerprintMinBucket*4-1)
	for _, tc := range []struct {
		path, want string
		status     int
	}{
		{path: "/routes/0", want: "/routes/0", status: StatusOK},
		{path: last, want: last, status: StatusOK},
		{path: "/routes/7/edit", want: "edit:7", status: StatusOK},
		{path: "/routes/files/a/b", want: "star:a/b", status: StatusOK},
		{path: "/routes/missing", status: StatusNotFound},
		{path: "/routes/" + strconv.Itoa(fingerprintMinBucket*4), status: StatusNotFound},
	} {
		seen = nil
		routerTestExpectPath(t, app, tc.path, tc.status, tc.want)
		require.Equal(t, []string{"mw"}, seen, "middleware must still run for %s", tc.path)
	}
}

// Test_Router_LargeBucket_SkipUnmatchedRoutes covers the filter meeting the
// SkipUnmatchedRoutes lookahead. The filter runs before next() consults
// firstMatchIndex, so a fingerprint that wrongly rejected the pre-resolved
// endpoint would turn a match into a 404 only when that config is on.
func Test_Router_LargeBucket_SkipUnmatchedRoutes(t *testing.T) {
	t.Parallel()

	app := New(Config{SkipUnmatchedRoutes: true})
	ran := 0
	app.Use("/routes", func(c Ctx) error {
		ran++
		return c.Next()
	})
	routerTestStatics(app, "/routes/", fingerprintMinBucket*4, routerTestPathHandler)
	app.Get("/routes/:id/edit", func(c Ctx) error { return c.SendString("edit:" + c.Params("id")) })

	last := "/routes/" + strconv.Itoa(fingerprintMinBucket*4-1)
	for _, tc := range []struct {
		path, want string
		status     int
	}{
		{path: "/routes/0", want: "/routes/0", status: StatusOK},
		{path: last, want: last, status: StatusOK},
		{path: "/routes/5/edit", want: "edit:5", status: StatusOK},
		{path: "/routes/missing", status: StatusNotFound},
	} {
		routerTestExpectPath(t, app, tc.path, tc.status, tc.want)
	}
	require.Positive(t, ran, "the middleware must still have run")
}

// Test_Router_LargeBucket_CaseInsensitive pins the filter against the default
// case-insensitive matching: the detection path is folded before it is hashed,
// and routes are registered folded, so the two fingerprints have to agree.
func Test_Router_LargeBucket_CaseInsensitive(t *testing.T) {
	t.Parallel()

	app := New()
	routerTestStatics(app, "/Routes/Item", fingerprintMinBucket*2, func(c Ctx) error { return c.SendString("hit") })

	for _, path := range []string{"/Routes/Item3", "/routes/item3", "/ROUTES/ITEM3"} {
		apptest.Expect(t, app, path, StatusOK)
	}
}

// Test_Router_LargeBucket_LongPath covers requests longer than every static
// route in the bucket, which the scan rejects without hashing: they must still
// reach the param route behind the statics, a static route as long as the
// bound must still match, and a miss is a 404.
func Test_Router_LargeBucket_LongPath(t *testing.T) {
	t.Parallel()

	longest := "/routes/" + strings.Repeat("s", 40)
	app := New()
	routerTestStatics(app, "/routes/", fingerprintMinBucket*2, routerTestPathHandler)
	app.Get(longest, routerTestPathHandler)
	app.Get("/routes/:name", func(c Ctx) error { return c.SendString("name:" + c.Params("name")) })

	name := strings.Repeat("n", 64)
	for _, tc := range []struct {
		path, want string
		status     int
	}{
		{path: longest, want: longest, status: StatusOK},
		{path: "/routes/" + name, want: "name:" + name, status: StatusOK},
		{path: "/routes/" + name + "/deeper", status: StatusNotFound},
	} {
		routerTestExpectPath(t, app, tc.path, tc.status, tc.want)
	}
}

// Test_Router_LargeBucket_StaticOnlyMiss covers the scan running off the end
// of a bucket made entirely of static routes: the fingerprint loop rejects
// every entry, the scan stops without loading a Route, and the request is a
// 404 rather than a match against the last route in the bucket.
func Test_Router_LargeBucket_StaticOnlyMiss(t *testing.T) {
	t.Parallel()

	app := New()
	routerTestStatics(app, "/routes/", fingerprintMinBucket*4, routerTestPathHandler)

	apptest.Expect(t, app, "/routes/missing", StatusNotFound)

	apptest.Expect(t, app, "/routes/"+strconv.Itoa(fingerprintMinBucket*4-1), StatusOK)
}

// Test_Router_LargeBucket_CustomCtx drives the filter through nextCustom, the
// scan a custom context takes, where the fingerprint is hashed per call rather
// than kept on the context. Same bucket shapes and outcomes as the default
// context, including the long paths the scan rejects without hashing and the
// miss that runs the scan off the end of the bucket.
func Test_Router_LargeBucket_CustomCtx(t *testing.T) {
	t.Parallel()

	newApp := func() *App {
		return NewWithCustomCtx(func(app *App) CustomCtx {
			return &localsRecordingCtx{DefaultCtx: *NewDefaultCtx(app)}
		})
	}

	app := newApp()
	var seen []string
	app.Use("/routes", func(c Ctx) error {
		seen = append(seen, "mw")
		return c.Next()
	})
	routerTestStatics(app, "/routes/", fingerprintMinBucket*4, routerTestPathHandler)
	app.Get("/routes/:id/edit", func(c Ctx) error { return c.SendString("edit:" + c.Params("id")) })

	for _, tc := range []struct {
		path, want string
		status     int
	}{
		{path: "/routes/0", want: "/routes/0", status: StatusOK},
		{path: "/routes/" + strconv.Itoa(fingerprintMinBucket*4-1), want: "/routes/" + strconv.Itoa(fingerprintMinBucket*4-1), status: StatusOK},
		{path: "/routes/9/edit", want: "edit:9", status: StatusOK},
		{path: "/routes/" + strings.Repeat("e", 64) + "/edit", want: "edit:" + strings.Repeat("e", 64), status: StatusOK},
		{path: "/routes/missing", status: StatusNotFound},
		{path: "/routes/" + strings.Repeat("m", 64), status: StatusNotFound},
	} {
		seen = nil
		routerTestExpectPath(t, app, tc.path, tc.status, tc.want)
		require.Equal(t, []string{"mw"}, seen, "middleware must still run for %s", tc.path)
	}

	// the custom-context miss with no non-static route after the statics runs the scan off the end
	only := newApp()
	routerTestStatics(only, "/routes/", fingerprintMinBucket*2, func(c Ctx) error { return c.SendString("ok") })
	apptest.Expect(t, only, "/routes/missing", StatusNotFound)
}

// Test_ScanHead_SlashMask pins the slash counts newScanHead lets through for
// each route shape: the count of its path for a route without parameters, any
// count from it up for a prefix (use) one, the parser's bounds for a
// parametric route, only the lower one when it is a prefix route, and every
// count for star and root routes. Bit 0, which stands for a count the scan did
// not compute, is always set, and bit 31 stands for 31 or more.
func Test_ScanHead_SlashMask(t *testing.T) {
	t.Parallel()

	// bits returns a mask with bits lo through hi set, plus bit 0.
	bits := func(lo, hi int) uint32 {
		var mask uint32 = 1
		for n := lo; n <= hi; n++ {
			mask |= uint32(1) << n
		}
		return mask
	}
	deep := strings.Repeat("/d", 70)

	app := New()
	handler := func(c Ctx) error { return c.Next() }
	app.Get("/a/b", handler)
	app.Use("/mw", handler)
	app.Get("/p/:id", handler)
	app.Get("/o/:id?", handler)
	app.Use("/u/:id", handler)
	app.Get("/w/*", handler)
	app.Get("/*", handler)
	app.Get("/", handler)
	app.Get(deep, handler)
	app.Get(deep+"/:id", handler)
	app.startupProcess()

	want := map[string]uint32{
		"/a/b":        bits(2, 2),
		"/mw":         bits(1, 31),
		"/p/:id":      bits(2, 2),
		"/o/:id?":     bits(1, 2),
		"/u/:id":      bits(2, 31),
		"/w/*":        bits(1, 31),
		"/*":          ^uint32(0),
		"/":           ^uint32(0),
		deep:          bits(31, 31),
		deep + "/:id": bits(31, 31),
	}
	seen := 0
	for _, route := range app.stack[app.methodInt(MethodGet)] {
		mask, ok := want[route.Path]
		if !ok {
			continue
		}
		seen++
		require.Equal(t, mask, newScanHead(route).slashMask, "slash mask of %q (use %v)", route.Path, route.use)
	}
	require.Len(t, want, seen, "every shape must have been registered")

	require.Equal(t, uint32(1), slashBit(0))
	require.Equal(t, uint32(1)<<5, slashBit(5))
	require.Equal(t, uint32(1)<<31, slashBit(31))
	require.Equal(t, uint32(1)<<31, slashBit(200))
}

// Test_ScanHead_Probe pins how a head holds a route's constant probe in its
// narrow fields: a probe that fits comes back as it went in, and one they
// cannot hold, an offset past them or a mask that does not cover leading
// lanes, leaves the head without a probe, for match to check.
func Test_ScanHead_Probe(t *testing.T) {
	t.Parallel()

	word, mask := packConst("/issues")
	fits := constProbe{word: word, mask: mask, from: 13, skip: 2}
	// A filtered bucket holds a head per route: what a rebuild allocates
	// grows with this.
	require.Equal(t, uintptr(48), unsafe.Sizeof(scanHead{}))
	var h scanHead
	h.setProbe(fits)
	require.Equal(t, uint8(7), h.probeLen)
	require.Equal(t, fits, h.probe())

	full, fullMask := packConst("/comments")
	h = scanHead{}
	h.setProbe(constProbe{word: full, mask: fullMask, from: 1})
	require.Equal(t, uint8(8), h.probeLen)
	require.Equal(t, constProbe{word: full, mask: fullMask, from: 1}, h.probe())

	for _, p := range []constProbe{
		{},
		{word: word, mask: mask, from: math.MaxUint16 + 1},
		{word: word, mask: mask, from: 13, skip: math.MaxUint8 + 1},
		{word: word, mask: mask, from: -1},
		{word: word, mask: mask, from: 0},
		{word: word, mask: mask &^ 0xff00, from: 13},
	} {
		h = scanHead{}
		h.setProbe(p)
		require.Zero(t, h.probeLen, "probe %+v", p)
	}

	// Routes: the head's probe is the parser's.
	app := New()
	handler := func(c Ctx) error { return c.Next() }
	app.Get("/repos/:owner/:repo/issues", handler)
	app.startupProcess()
	route := app.stack[app.methodInt(MethodGet)][0]
	require.NotZero(t, route.routeParser.probe.mask)
	head := newScanHead(route)
	require.Equal(t, route.routeParser.probe, head.probe())

	// headRejects agrees with the parser's probe, found or not.
	for _, path := range []string{"/repos/gofiber/fiber/issues", "/repos/gofiber/fiber/pulls", "/repos/gofiber"} {
		var byHead probeMemo
		probe := route.routeParser.probe
		require.Equal(t, probe.rejects(path), byHead.headRejects(&head, path), path)
	}
}

// Test_ScanHead_PrefixWords pins the two leading-byte words a head compares:
// the first is the route's own leading-byte filter, and the second continues
// knownPrefix past it, which is what tells apart the routes of an API that all
// begin "/api/v1/".
func Test_ScanHead_PrefixWords(t *testing.T) {
	t.Parallel()

	app := New()
	handler := func(c Ctx) error { return c.Next() }
	patterns := []string{
		"/api/v1/users/:id", "/api/v1/users", "/api/v1/posts/:id/comments",
		"/short/:x", "/:x", "/opt/:x?", "/api/v1/",
	}
	for _, pattern := range patterns {
		app.Get(pattern, handler)
	}
	app.startupProcess()

	byPath := make(map[string]*Route)
	for _, route := range app.stack[app.methodInt(MethodGet)] {
		byPath[route.Path] = route
	}
	for _, pattern := range patterns {
		route := byPath[pattern]
		require.NotNil(t, route, pattern)
		h := newScanHead(route)
		require.Equal(t, route.prefix, h.prefix, "first word of %q", pattern)
		require.Equal(t, route.prefixMask, h.prefixMask, "first mask of %q", pattern)

		prefix := knownPrefix(route)
		wantWord, wantMask := uint64(0), uint64(0)
		if len(prefix) > 8 {
			wantWord, wantMask = packConst(prefix[8:])
		}
		require.Equal(t, wantWord, h.prefix2, "second word of %q", pattern)
		require.Equal(t, wantMask, h.prefixMask2, "second mask of %q", pattern)
	}

	// The second word rejects what the first cannot tell apart.
	users := newScanHead(byPath["/api/v1/users/:id"])
	for path, rejected := range map[string]bool{
		"/api/v1/users/1":       false,
		"/api/v1/posts/1":       true,
		"/api/v1/usersx/1":      true,
		"/api/v1/":              true,
		"/api/v2/users/1":       true,
		"/api/v1/users/1/extra": true, // too many slashes for the endpoint
	} {
		var scan routeScan
		scan.init(path, pathHeadWord(path), strings.Count(path, "/"))
		require.Equal(t, rejected, users.rejects(scan.head, scan.head2, scan.slash), "path %q", path)
	}
}

// Test_Router_FilteredBucket_SharedProbeFrom routes through a filtered bucket
// whose probes share their from but read different slashes: "/foo/" follows
// the first parameter's slash, "/bar" the second's. A scan that reused the
// word one of them located for the other would reject the route it tests.
func Test_Router_FilteredBucket_SharedProbeFrom(t *testing.T) {
	t.Parallel()

	apps := map[string]*App{"default": New(), "custom": newCustomApp()}
	for name, app := range apps {
		handler := func(c Ctx) error { return c.SendString(c.Route().Path) }
		routerTestStatics(app, "/api/static", filterMinBucket, handler)
		app.Get("/api/:x/foo/*", handler)
		app.Get("/api/:x/:y/bar", handler)
		app.startupProcess()

		routes, filter := routerTestBucket(app, MethodGet, "/ap")
		require.NotNil(t, filter, "%s: the bucket must be filtered for this test to mean anything", name)
		foo, bar := filter.heads[len(routes)-2], filter.heads[len(routes)-1]
		require.NotZero(t, foo.probeLen, name)
		require.NotZero(t, bar.probeLen, name)
		require.Equal(t, foo.probeFrom, bar.probeFrom, "%s: the probes must share their from", name)
		require.NotEqual(t, foo.probeSkip, bar.probeSkip, name)

		for path, want := range map[string]string{
			"/api/1/2/bar":   "/api/:x/:y/bar",
			"/api/1/foo/z":   "/api/:x/foo/*",
			"/api/1/foo/bar": "/api/:x/foo/*",
			"/api/static3":   "/api/static3",
		} {
			resp := apptest.Do(t, app, path)
			body := apptest.Body(t, resp)
			require.Equal(t, StatusOK, resp.StatusCode, "%s: %s", name, path)
			require.Equal(t, want, body, "%s: %s", name, path)
		}
		resp := apptest.Do(t, app, "/api/1/2/baz")
		require.Equal(t, StatusNotFound, resp.StatusCode, name)
	}
}

// Test_Router_FilteredBucket_MethodNotAllowed drives the 404/405 decision
// through filtered buckets, which routeScan.endpoint walks for the methods the
// request did not use. The Allow header must name exactly the methods with a
// matching endpoint, through a default and a custom context alike.
func Test_Router_FilteredBucket_MethodNotAllowed(t *testing.T) {
	t.Parallel()

	register := func(app *App) {
		handler := func(c Ctx) error { return c.SendString(c.Route().Path) }
		for i := range filterMinBucket * 2 {
			app.Get("/api/v1/res"+strconv.Itoa(i)+"/:id", handler)
			app.Post("/api/v1/res"+strconv.Itoa(i)+"/:id/items", handler)
		}
		app.Put("/api/v1/res3/:id", handler)
		app.Delete("/api/v1/static/path", handler)
	}
	apps := map[string]*App{"default": New(), "custom": newCustomApp()}

	for name, app := range apps {
		register(app)
		app.startupProcess()

		for _, method := range []string{MethodGet, MethodPost} {
			_, filter := routerTestBucket(app, method, "/ap")
			require.NotNil(t, filter, "%s: the %s bucket must be filtered for this test to mean anything", name, method)
		}

		for _, tc := range []struct {
			method, path, allow string
			status              int
		}{
			{method: MethodGet, path: "/api/v1/res5/7", status: StatusOK},
			{method: MethodPost, path: "/api/v1/res5/7/items", status: StatusOK},
			{method: MethodPatch, path: "/api/v1/res3/7", status: StatusMethodNotAllowed, allow: "GET, HEAD, PUT"},
			{method: MethodPatch, path: "/api/v1/res5/7/items", status: StatusMethodNotAllowed, allow: "POST"},
			{method: MethodGet, path: "/api/v1/res5/7/items", status: StatusMethodNotAllowed, allow: "POST"},
			{method: MethodPost, path: "/api/v1/res5/7", status: StatusMethodNotAllowed, allow: "GET, HEAD"},
			{method: MethodGet, path: "/api/v1/static/path", status: StatusMethodNotAllowed, allow: "DELETE"},
			{method: MethodGet, path: "/api/v1/nope/7", status: StatusNotFound},
			{method: MethodGet, path: "/api/v1/res5", status: StatusNotFound},
		} {
			resp := apptest.Do(t, app, apptest.Req(tc.method, tc.path))
			require.Equal(t, tc.status, resp.StatusCode, "%s: %s %s", name, tc.method, tc.path)
			require.Equal(t, tc.allow, resp.Header.Get(HeaderAllow), "%s: %s %s", name, tc.method, tc.path)
		}
	}
}

// Test_Router_FilteredBucket_CustomCtx runs the GitHub API corpus, whose
// larger buckets are filtered, through nextCustom, the scan a custom context
// takes, and requires the route and parameters App.next picks for each request.
func Test_Router_FilteredBucket_CustomCtx(t *testing.T) {
	t.Parallel()

	defaultApp := New()
	registerDummyRoutes(defaultApp)
	defaultApp.startupProcess()
	customApp := newCustomApp()
	registerDummyRoutes(customApp)
	customApp.startupProcess()

	filtered := 0
	for m := range defaultApp.config.RequestMethods {
		tree := defaultApp.treeIndex[m]
		for _, filter := range append([]*bucketFilter{tree.globalFilter}, tree.filters...) {
			if filter != nil {
				filtered++
			}
		}
	}
	require.NotZero(t, filtered, "the corpus must have filtered buckets for this test to mean anything")

	for _, r := range routesFixture.TestRoutes {
		fctx := &fasthttp.RequestCtx{}
		fctx.Request.Header.SetMethod(r.Method)
		fctx.URI().SetPath(r.Path)

		want, ok := defaultApp.AcquireCtx(fctx).(*DefaultCtx)
		require.True(t, ok)
		wantMatched, wantErr := defaultApp.next(want)

		got := customApp.AcquireCtx(fctx)
		gotMatched, gotErr := customApp.nextCustom(got)

		require.Equal(t, wantMatched, gotMatched, "%s %s", r.Method, r.Path)
		require.Equal(t, wantErr, gotErr, "%s %s", r.Method, r.Path)
		require.Equal(t, want.Route().Path, got.Route().Path, "%s %s", r.Method, r.Path)
		n := len(want.Route().Params)
		require.Equal(t, want.values[:n], got.getValues()[:n], "%s %s", r.Method, r.Path)

		defaultApp.ReleaseCtx(want)
		customApp.ReleaseCtx(got)
	}
}

// Test_RouteTree_Heads_NotStale proves every filtered bucket's heads are the
// ones its routes derive, after each way an app gains, loses or rewrites
// routes. The heads are derived when the tree is built, from routes that are
// final by then, so this guards that no path reaches a published tree without
// a build.
func Test_RouteTree_Heads_NotStale(t *testing.T) {
	t.Parallel()

	assertFresh := func(t *testing.T, app *App, stage string) {
		t.Helper()
		checked := 0
		for m, tree := range app.treeIndex {
			check := func(routes []*Route, filter *bucketFilter) {
				if filter == nil {
					require.Less(t, len(routes), filterMinBucket, "%s: a bucket this size must be filtered", stage)
					return
				}
				require.Len(t, filter.heads, len(routes), stage)
				for i, route := range routes {
					require.Equal(t, newScanHead(route), filter.heads[i], "%s: stale head for %s %q",
						stage, app.config.RequestMethods[m], route.Path)
					checked++
				}
			}
			check(tree.globals, tree.globalFilter)
			for i, routes := range tree.buckets {
				check(routes, tree.filters[i])
			}
		}
		require.NotZero(t, checked, "%s: no filtered bucket to check", stage)
	}

	handler := func(c Ctx) error { return c.Next() }
	app := New()
	for i := range filterMinBucket {
		app.Get("/api/v1/r"+strconv.Itoa(i)+"/:id", handler)
		app.Get("/api/v1/s"+strconv.Itoa(i), handler)
	}
	app.Use("/api", handler)
	group := app.Group("/api/v1/grp", handler)
	group.Get("/:x/y", handler)

	sub := New()
	for i := range filterMinBucket {
		sub.Get("/m"+strconv.Itoa(i)+"/:id", handler)
	}
	app.Use("/api/v1/mounted", sub)

	app.startupProcess()
	assertFresh(t, app, "after startup")

	app.Get("/api/v1/late/:id", handler)
	app.RemoveRoute("/api/v1/s0")
	_ = app.RebuildTree()
	assertFresh(t, app, "after runtime registration and RebuildTree")
}

func Test_OptionalParam_ConstraintFailureDoesNotBypassAsEmpty(t *testing.T) {
	t.Parallel()

	app := New()
	app.Use("/api/:id<int>?", func(c Ctx) error {
		return c.SendString("matched")
	})

	apptest.Expect(t, app, "/api/abc", StatusNotFound)

	apptest.Expect(t, app, "/api/123", StatusOK)

	apptest.Expect(t, app, "/api", StatusOK)
}

func Test_RouteParser_BacktrackMaxRetriesBound(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		pattern string
		status  int
	}{
		{"within_limit", "/pre:a?:b?:c?:d?:e?-end", StatusOK},
		{"exceeds_limit", "/pre:a?:b?:c?:d?:e?:f?-end", StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := New()
			app.Get(tc.pattern, func(c Ctx) error {
				for _, name := range []string{"a", "b", "c", "d"} {
					require.Empty(t, c.Params(name))
				}
				return c.SendStatus(StatusOK)
			})

			routerTestExpect(t, app, "/pre-end", tc.status)
		})
	}
}

func Test_RouteParser_GreedyWildcardDoesNotYield(t *testing.T) {
	t.Parallel()

	app := New()
	// Greedy wildcard * must not yield to plus param +
	app.Get("/*+:x?", func(c Ctx) error {
		return c.SendStatus(StatusOK)
	})

	apptest.Expect(t, app, "/-", StatusNotFound)
}

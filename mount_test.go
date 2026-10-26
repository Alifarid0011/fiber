// ⚡️ Fiber is an Express inspired web framework written in Go with ☕️
// 🤖 GitHub Repository: https://github.com/gofiber/fiber
// 📌 API Documentation: https://docs.gofiber.io

package fiber

import (
	"errors"
	"regexp"
	"testing"

	"github.com/gofiber/fiber/v3/internal/apptest"
	"github.com/stretchr/testify/require"
)

func Test_App_Mount_PreservesSubAppRegexHandler(t *testing.T) {
	t.Parallel()

	parent := New(Config{
		CaseSensitive: true,
		RegexHandler: func(pattern string) *matchOnlyRegexCompiler {
			return &matchOnlyRegexCompiler{re: regexp.MustCompile("(?i)" + pattern)}
		},
	})

	sub := New(Config{
		CaseSensitive: true,
		RegexHandler:  regexp.MustCompile,
	})
	sub.Get("/resource/:id<regex(ALLOW)>", func(c Ctx) error {
		return c.SendStatus(StatusOK)
	})

	parent.Use("/mounted", sub)

	apptest.Expect(t, sub, "/resource/ALLOW", StatusOK)

	apptest.Expect(t, parent, "/mounted/resource/ALLOW", StatusOK)

	apptest.Expect(t, sub, "/resource/allow", StatusNotFound)

	apptest.Expect(t, parent, "/mounted/resource/allow", StatusNotFound)
}

// go test -run Test_App_Mount
func Test_App_Mount(t *testing.T) {
	t.Parallel()
	micro := New()
	micro.Get("/doe", func(c Ctx) error {
		return c.SendStatus(StatusOK)
	})

	app := New()
	app.Use("/john", micro)
	apptest.Expect(t, app, "/john/doe", 200)
	require.Equal(t, uint32(2), app.handlersCount)
}

func Test_App_Mount_RootPath_Nested(t *testing.T) {
	t.Parallel()
	app := New()
	dynamic := New()
	apiserver := New()

	apiroutes := apiserver.Group("/v1")
	apiroutes.Get("/home", func(c Ctx) error {
		return c.SendString("home")
	})

	dynamic.Use("/api", apiserver)
	app.Use("/", dynamic)

	apptest.Expect(t, app, "/api/v1/home", 200)
	require.Equal(t, uint32(2), app.handlersCount)
}

// go test -run Test_App_Mount_Nested
func Test_App_Mount_Nested(t *testing.T) {
	t.Parallel()
	app := New()
	one := New()
	two := New()
	three := New()

	two.Use("/three", three)
	app.Use("/one", one)
	one.Use("/two", two)

	one.Get("/doe", func(c Ctx) error {
		return c.SendStatus(StatusOK)
	})

	two.Get("/nested", func(c Ctx) error {
		return c.SendStatus(StatusOK)
	})

	three.Get("/test", func(c Ctx) error {
		return c.SendStatus(StatusOK)
	})

	apptest.Expect(t, app, "/one/doe", 200)

	apptest.Expect(t, app, "/one/two/nested", 200)

	apptest.Expect(t, app, "/one/two/three/test", 200)

	require.Equal(t, uint32(6), app.handlersCount)
}

// go test -run Test_App_Mount_Express_Behavior
func Test_App_Mount_Express_Behavior(t *testing.T) {
	t.Parallel()
	createTestHandler := func(body string) func(c Ctx) error {
		return func(c Ctx) error {
			return c.SendString(body)
		}
	}

	app := New()
	subApp := New()

	// app setup
	subApp.Get("/hello", createTestHandler("subapp hello!"))
	subApp.Get("/world", createTestHandler("subapp world!")) // <- wins

	app.Get("/hello", createTestHandler("app hello!")) // <- wins
	app.Use("/", subApp)                               // <- subApp registration
	app.Get("/world", createTestHandler("app world!"))

	app.Get("/bar", createTestHandler("app bar!"))
	subApp.Get("/bar", createTestHandler("subapp bar!")) // <- wins

	subApp.Get("/foo", createTestHandler("subapp foo!")) // <- wins
	app.Get("/foo", createTestHandler("app foo!"))

	// 404 Handler
	app.Use(func(c Ctx) error {
		return c.SendStatus(StatusNotFound)
	})
	// expectation check
	apptest.ExpectBody(t, app, "/world", StatusOK, "subapp world!")
	apptest.ExpectBody(t, app, "/hello", StatusOK, "app hello!")
	apptest.ExpectBody(t, app, "/bar", StatusOK, "subapp bar!")
	apptest.ExpectBody(t, app, "/foo", StatusOK, "subapp foo!")
	apptest.ExpectBody(t, app, "/unknown", StatusNotFound, ErrNotFound.Message)

	require.Equal(t, uint32(17), app.handlersCount)
}

// go test -run Test_App_Mount_RoutePositions
func Test_App_Mount_RoutePositions(t *testing.T) {
	t.Parallel()

	app := New()
	subApp1 := New()
	subApp2 := New()
	// app setup
	{
		app.Use(func(c Ctx) error {
			// set initial value
			c.Locals("world", "world")
			return c.Next()
		})
		app.Use("/subApp1", subApp1)
		app.Use(func(c Ctx) error {
			return c.Next()
		})
		app.Get("/bar", func(c Ctx) error {
			return c.SendString("ok")
		})
		app.Use(func(c Ctx) error {
			// is overwritten when the positioning is not correct
			c.Locals("world", "hello")
			return c.Next()
		})
		methods := subApp2.Group("/subApp2")
		methods.Get("/world", func(c Ctx) error {
			v, ok := c.Locals("world").(string)
			if !ok {
				panic("unexpected data type")
			}
			return c.SendString(v)
		})
		app.Use("", subApp2)
	}

	apptest.ExpectBody(t, app, "/subApp2/world", StatusOK, "hello")

	routeStackGET := app.Stack()[0]
	require.True(t, routeStackGET[0].use)
	require.Equal(t, "/", routeStackGET[0].path)

	require.True(t, routeStackGET[1].use)
	require.Equal(t, "/", routeStackGET[1].path)

	require.False(t, routeStackGET[2].use)
	require.Equal(t, "/bar", routeStackGET[2].path)

	require.True(t, routeStackGET[3].use)
	require.Equal(t, "/", routeStackGET[3].path)

	require.False(t, routeStackGET[4].use)
	require.Equal(t, "/subapp2/world", routeStackGET[4].path)

	require.Len(t, routeStackGET, 5)
}

// go test -run Test_App_MountPath
func Test_App_MountPath(t *testing.T) {
	t.Parallel()
	app := New()
	one := New()
	two := New()
	three := New()

	two.Use("/three", three)
	one.Use("/two", two)
	app.Use("/one", one)

	require.Equal(t, "/one", one.MountPath())
	require.Equal(t, "/one/two", two.MountPath())
	require.Equal(t, "/one/two/three", three.MountPath())
	require.Empty(t, app.MountPath())
}

// go test -run Test_App_Mount_ErrorHandler
func Test_App_Mount_ErrorHandler(t *testing.T) {
	t.Parallel()

	const (
		routeErr = "0: GET error"
		body     = "1: custom error"
	)

	tests := []struct {
		name   string
		group  string // the mounted app is mounted on this group of the parent when set
		prefix string // where the mounted app is mounted
		route  string // the route of the mounted app, which always fails
		target string // the request that reaches the failing route
		// handledByParent tells which app has the custom error handler: the one mounting or the mounted one.
		handledByParent bool
	}{
		{name: "group mount", group: "/v1", prefix: "/john", route: "/doe", target: "/v1/john/doe"},
		{name: "group mount at the root level", group: "/v1", prefix: "/", route: "/john/doe", target: "/v1/john/doe"},
		{name: "the mounting app's handler is used", prefix: "/api", route: "/", target: "/api", handledByParent: true},
		{name: "the mounted app's handler is used", prefix: "/api", route: "/", target: "/api"},
		{name: "the mounted app's handler is used at the root level", prefix: "/", route: "/api", target: "/api"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			errorHandler := func(c Ctx, err error) error {
				require.Equal(t, routeErr, err.Error())
				return c.Status(StatusInternalServerError).SendString(body)
			}

			app := New()
			micro := New()
			if tc.handledByParent {
				app = New(Config{ErrorHandler: errorHandler})
			} else {
				micro = New(Config{ErrorHandler: errorHandler})
			}
			micro.Get(tc.route, func(_ Ctx) error {
				return errors.New(routeErr)
			})

			var router Router = app
			if tc.group != "" {
				router = app.Group(tc.group)
			}
			router.Use(tc.prefix, micro)

			apptest.ExpectBody(t, app, tc.target, StatusInternalServerError, body)
		})
	}
}

// go test -run Test_App_Group_Mount
func Test_App_Group_Mount(t *testing.T) {
	t.Parallel()
	micro := New()
	micro.Get("/doe", func(c Ctx) error {
		return c.SendStatus(StatusOK)
	})

	app := New()
	v1 := app.Group("/v1")
	v1.Use("/john", micro)

	apptest.Expect(t, app, "/v1/john/doe", 200)
	require.Equal(t, uint32(2), app.handlersCount)
}

func Test_App_UseMountedErrorHandlerForBestPrefixMatch(t *testing.T) {
	t.Parallel()
	app := New()

	tsf := func(c Ctx, _ error) error {
		return c.Status(200).SendString("hi, i'm a custom sub fiber error 2")
	}
	tripleSubFiber := New(Config{
		ErrorHandler: tsf,
	})
	tripleSubFiber.Get("/", func(_ Ctx) error {
		return errors.New("something happened")
	})

	sf := func(c Ctx, _ error) error {
		return c.Status(200).SendString("hi, i'm a custom sub fiber error")
	}
	subfiber := New(Config{
		ErrorHandler: sf,
	})
	subfiber.Get("/", func(_ Ctx) error {
		return errors.New("something happened")
	})
	subfiber.Use("/third", tripleSubFiber)

	f := func(c Ctx, _ error) error {
		return c.Status(200).SendString("hi, i'm a custom error")
	}
	fiber := New(Config{
		ErrorHandler: f,
	})
	fiber.Get("/", func(_ Ctx) error {
		return errors.New("something happened")
	})
	fiber.Use("/sub", subfiber)

	app.Use("/api", fiber)

	apptest.ExpectBody(t, app, "/api/sub", 200, "hi, i'm a custom sub fiber error")
	apptest.ExpectBody(t, app, "/api/sub/third", 200, "hi, i'm a custom sub fiber error 2")
}

// go test -run Test_Mount_Route_Names
func Test_Mount_Route_Names(t *testing.T) {
	t.Parallel()
	// create sub-app with 2 handlers:
	subApp1 := New()
	subApp1.Get("/users", func(c Ctx) error {
		url, err := c.GetRouteURL("add-user", Map{})
		require.NoError(t, err)
		require.Equal(t, "/app1/users", url, "handler: app1.add-user") // the prefix is /app1 because of the mount
		// if subApp1 is not mounted, expected url just /users
		return nil
	}).Name("get-users")
	subApp1.Post("/users", func(c Ctx) error {
		route := c.App().GetRoute("get-users")
		require.Equal(t, MethodGet, route.Method, "handler: app1.get-users method")
		require.Equal(t, "/app1/users", route.Path, "handler: app1.get-users path")
		return nil
	}).Name("add-user")

	// create sub-app with 2 handlers inside a group:
	subApp2 := New()
	app2Grp := subApp2.Group("/users").Name("users.")
	app2Grp.Get("", emptyHandler).Name("get")
	app2Grp.Post("", emptyHandler).Name("add")

	// put both sub-apps into root app
	rootApp := New()
	_ = rootApp.Use("/app1", subApp1)
	_ = rootApp.Use("/app2", subApp2)

	rootApp.startupProcess()

	for _, tc := range []struct {
		app    *App
		name   string
		method string
		path   string
	}{
		// take route directly from sub-app
		{app: subApp1, name: "get-users", method: MethodGet, path: "/users"},
		{app: subApp1, name: "add-user", method: MethodPost, path: "/users"},
		// take route directly from sub-app with group
		{app: subApp2, name: "users.get", method: MethodGet, path: "/users"},
		{app: subApp2, name: "users.add", method: MethodPost, path: "/users"},
		// take route from root app (using names of sub-apps)
		{app: rootApp, name: "add-user", method: MethodPost, path: "/app1/users"},
		{app: rootApp, name: "users.add", method: MethodPost, path: "/app2/users"},
	} {
		route := tc.app.GetRoute(tc.name)
		require.Equal(t, tc.method, route.Method, tc.name)
		require.Equal(t, tc.path, route.Path, tc.name)
	}

	// GetRouteURL inside handler
	apptest.Expect(t, rootApp, "/app1/users", StatusOK)

	// ctx.App().GetRoute() inside handler
	apptest.Expect(t, rootApp, apptest.Req(MethodPost, "/app1/users"), StatusOK)
}

// go test -run Test_Ctx_Render_Mount
func Test_Ctx_Render_Mount(t *testing.T) {
	t.Parallel()

	engine := &testTemplateEngine{}
	err := engine.Load()
	require.NoError(t, err)

	sub := New(Config{
		Views: engine,
	})

	sub.Get("/:name", func(c Ctx) error {
		return c.Render("hello_world.tmpl", Map{
			"Name": c.Params("name"),
		})
	})

	app := New()
	app.Use("/hello", sub)

	apptest.ExpectBody(t, app, "/hello/a", StatusOK, "<h1>Hello a!</h1>")
}

// go test -run Test_Ctx_Render_Mount_ParentOrSubHasViews
func Test_Ctx_Render_Mount_ParentOrSubHasViews(t *testing.T) {
	t.Parallel()

	engine := &testTemplateEngine{}
	err := engine.Load()
	require.NoError(t, err)

	engine2 := &testTemplateEngine{path: "testdata2"}
	err = engine2.Load()
	require.NoError(t, err)

	engine3 := &testTemplateEngine{path: "testdata3"}
	err = engine3.Load()
	require.NoError(t, err)

	sub := New(Config{
		Views: engine3,
	})

	sub2 := New(Config{
		Views: engine2,
	})

	app := New(Config{
		Views: engine,
	})

	app.Get("/test", func(c Ctx) error {
		return c.Render("index.tmpl", Map{
			"Title": "Hello, World!",
		})
	})

	sub.Get("/world/:name", func(c Ctx) error {
		return c.Render("hello_world.tmpl", Map{
			"Name": c.Params("name"),
		})
	})

	sub2.Get("/moment", func(c Ctx) error {
		return c.Render("bruh.tmpl", Map{})
	})

	sub.Use("/bruh", sub2)
	app.Use("/hello", sub)

	apptest.ExpectBody(t, app, "/hello/world/a", StatusOK, "<h1>Hello a!</h1>")
	apptest.ExpectBody(t, app, "/test", StatusOK, "<h1>Hello, World!</h1>")
	apptest.ExpectBody(t, app, "/hello/bruh/moment", StatusOK, "<h1>I'm Bruh</h1>")
}

func Test_Ctx_Render_MountGroup(t *testing.T) {
	t.Parallel()

	engine := &testTemplateEngine{}
	err := engine.Load()
	require.NoError(t, err)

	micro := New(Config{
		Views: engine,
	})

	micro.Get("/doe", func(c Ctx) error {
		return c.Render("hello_world.tmpl", Map{
			"Name": "doe",
		})
	})

	app := New()
	v1 := app.Group("/v1")
	v1.Use("/john", micro)

	apptest.ExpectBody(t, app, "/v1/john/doe", 200, "<h1>Hello doe!</h1>")
}

// Test_App_Mount_FewerRequestMethods verifies that a sub-app serving only some
// of the parent's request methods can be mounted: the parent must read the
// sub-app's routes by method, not by its own stack index.
func Test_App_Mount_FewerRequestMethods(t *testing.T) {
	t.Parallel()

	micro := New(Config{RequestMethods: []string{MethodGet}})
	micro.Get("/doe", func(c Ctx) error {
		return c.SendString("doe")
	})

	app := New()
	app.Use("/john", micro)

	apptest.ExpectBody(t, app, "/john/doe", StatusOK, "doe")
}

// Test_App_Mount_FewerRequestMethodsNested verifies the same for a sub-app
// reached through another mount.
func Test_App_Mount_FewerRequestMethodsNested(t *testing.T) {
	t.Parallel()

	micro := New(Config{RequestMethods: []string{MethodGet}})
	micro.Get("/doe", func(c Ctx) error {
		return c.SendString("doe")
	})

	sub := New()
	sub.Use("/v1", micro)

	app := New()
	app.Use("/john", sub)

	apptest.ExpectBody(t, app, "/john/v1/doe", StatusOK, "doe")
}

// Test_App_Mount_ReorderedRequestMethods verifies that a sub-app which lists
// the same request methods in a different order keeps serving each route under
// the method it was registered with.
func Test_App_Mount_ReorderedRequestMethods(t *testing.T) {
	t.Parallel()

	// Swap GET and POST, so the two apps' stack indexes disagree.
	methods := append([]string{}, DefaultMethods...)
	for i, method := range methods {
		switch method {
		case MethodGet:
			methods[i] = MethodPost
		case MethodPost:
			methods[i] = MethodGet
		}
	}

	micro := New(Config{RequestMethods: methods})
	micro.Get("/doe", func(c Ctx) error {
		return c.SendString("get doe")
	})
	micro.Post("/doe", func(c Ctx) error {
		return c.SendString("post doe")
	})

	app := New()
	app.Use("/john", micro)

	apptest.ExpectBody(t, app, "/john/doe", StatusOK, "get doe")

	apptest.ExpectBody(t, app, apptest.Req(MethodPost, "/john/doe"), StatusOK, "post doe")
}

// onlyFooConstraint and onlyBarConstraint are custom constraints used to check
// that a mounted app's constraints survive the mount's route expansion.
type onlyFooConstraint struct{}

func (*onlyFooConstraint) Name() string { return "onlyfoo" }

func (*onlyFooConstraint) Execute(param string, _ ...string) bool { return param == "foo" }

type onlyBarConstraint struct{}

func (*onlyBarConstraint) Name() string { return "onlybar" }

func (*onlyBarConstraint) Execute(param string, _ ...string) bool { return param == "bar" }

// sameNameFooConstraint and sameNameBarConstraint share a name and accept
// different values, as two apps mounted side by side are free to do.
type sameNameFooConstraint struct{}

func (*sameNameFooConstraint) Name() string { return "same" }

func (*sameNameFooConstraint) Execute(param string, _ ...string) bool { return param == "foo" }

type sameNameBarConstraint struct{}

func (*sameNameBarConstraint) Name() string { return "same" }

func (*sameNameBarConstraint) Execute(param string, _ ...string) bool { return param == "bar" }

// Test_App_Mount_SiblingConstraintNames verifies that a constraint binds only
// the routes of the app that registered it: two apps mounted side by side can
// name one differently, and the app above them must not hold either to the
// other's.
func Test_App_Mount_SiblingConstraintNames(t *testing.T) {
	t.Parallel()

	// A fresh tree per case: mounting one app on two parents would have the
	// first expand it before the second cloned it.
	build := func() *App {
		handler := func(c Ctx) error { return c.SendString("ok") }

		foo := New()
		foo.RegisterCustomConstraint(&sameNameFooConstraint{})
		foo.Get("/:value<same>", handler)

		bar := New()
		bar.RegisterCustomConstraint(&sameNameBarConstraint{})
		bar.Get("/:value<same>", handler)

		subApp := New()
		subApp.Use("/foo", foo)
		subApp.Use("/bar", bar)

		return subApp
	}

	plain := New()
	plain.Use("/sub", build())

	domain := New()
	domain.Domain("api.example.com").Use("/sub", build())

	want := map[string]int{
		"/sub/foo/foo": StatusOK,
		"/sub/foo/bar": StatusNotFound,
		"/sub/bar/bar": StatusOK,
		"/sub/bar/foo": StatusNotFound,
	}

	for path, status := range want {
		resp := apptest.Do(t, plain, path)
		require.Equal(t, status, resp.StatusCode, "plain "+path)

		resp = apptest.Do(t, domain, apptest.Req(MethodGet, path, apptest.WithHost("api.example.com")))
		require.Equal(t, status, resp.StatusCode, "domain "+path)
	}
}

// Test_App_Mount_PreservesNestedCustomConstraint verifies that a custom
// constraint registered on an app reached through two mounts still validates.
// Expanding a mount re-parses the routes against the app above, so the
// constraint has to travel with them or it silently stops rejecting anything.
func Test_App_Mount_PreservesNestedCustomConstraint(t *testing.T) {
	t.Parallel()

	micro := New()
	micro.RegisterCustomConstraint(&onlyFooConstraint{})
	micro.Get("/doe/:name<onlyfoo>", func(c Ctx) error {
		return c.SendString(c.Params("name"))
	})

	sub := New()
	sub.Use("/v1", micro)

	app := New()
	app.Use("/john", sub)

	apptest.Expect(t, app, "/john/v1/doe/foo", StatusOK)

	apptest.Expect(t, app, "/john/v1/doe/bar", StatusNotFound)
}

// Test_App_Mount_CarriesDomainMount verifies that mounting an app which has a
// domain mount of its own carries that mount's config along, host check
// included.
func Test_App_Mount_CarriesDomainMount(t *testing.T) {
	t.Parallel()

	micro := New(Config{
		ErrorHandler: func(c Ctx, _ error) error {
			return c.Status(596).SendString("micro error")
		},
	})
	micro.Get("/doe", func(_ Ctx) error {
		return errors.New("boom")
	})

	sub := New()
	sub.Domain("api.example.com").Use("/v1", micro)

	app := New()
	app.Use("/john", sub)

	apptest.Expect(t, app, apptest.Req(MethodGet, "/john/v1/doe", apptest.WithHost("api.example.com")), 596)

	// The route does not match on another host, and the sub-app's error
	// handler must not answer for it either.
	apptest.Expect(t, app, apptest.Req(MethodGet, "/john/v1/doe", apptest.WithHost("www.example.com")), StatusNotFound)
}

// Test_App_Mount_AutoHeadKeepsMiddleware verifies that a mounted app which does
// not serve HEAD gets no synthesized HEAD routes once its routes are expanded
// into the parent — they would run without the middleware it registered.
func Test_App_Mount_AutoHeadKeepsMiddleware(t *testing.T) {
	t.Parallel()

	micro := New(Config{RequestMethods: []string{MethodGet, MethodPost}})
	micro.Use(func(c Ctx) error {
		return c.SendStatus(StatusUnauthorized)
	})
	micro.Get("/doe", func(c Ctx) error {
		c.Set("X-Secret", "leaked")
		return c.SendString("doe")
	})

	app := New()
	app.Use("/john", micro)

	apptest.Expect(t, app, "/john/doe", StatusUnauthorized)

	// Twice: app.Test runs the startup process on every call, and the second
	// pass sees the mount already expanded into the parent's stack.
	for range 2 {
		resp := apptest.Expect(t, app, apptest.Req(MethodHead, "/john/doe"), StatusMethodNotAllowed)
		require.Empty(t, resp.Header.Get("X-Secret"))
	}
}

// Test_App_Mount_RootMountKeepsParentAutoHead verifies that a mounted app which
// does not serve HEAD only withholds automatic HEAD routes from its own routes.
// A mount at "/" covers every path, and the app above keeps its own.
func Test_App_Mount_RootMountKeepsParentAutoHead(t *testing.T) {
	t.Parallel()

	micro := New(Config{RequestMethods: []string{MethodGet, MethodPost}})
	micro.Get("/doe", func(c Ctx) error {
		return c.SendString("doe")
	})

	app := New()
	app.Get("/own", func(c Ctx) error {
		return c.SendString("own")
	})
	app.Use("/", micro)

	resp := apptest.Do(t, app, apptest.Req(MethodHead, "/own"))
	require.Equal(t, StatusOK, resp.StatusCode, "the app's own route keeps its HEAD route")

	resp = apptest.Do(t, app, apptest.Req(MethodHead, "/doe"))
	require.Equal(t, StatusMethodNotAllowed, resp.StatusCode, "the mounted app serves no HEAD")
}

// Test_App_Mount_ParametricPrefix verifies that a mount prefix carrying a
// parameter is matched as a pattern. The prefix rewrites every route of the
// mounted app, so the parameters it introduces have to be rebuilt onto them.
func Test_App_Mount_ParametricPrefix(t *testing.T) {
	t.Parallel()

	micro := New()
	micro.Get("/doe", func(c Ctx) error {
		return c.SendString("version=" + c.Params("Version"))
	})

	app := New()
	app.Use("/v1/:Version", micro)

	apptest.ExpectBody(t, app, "/v1/42/doe", StatusOK, "version=42")
}

// Test_App_Mount_ParametricPrefixNested verifies the same when the prefix is
// applied a second time, by an outer mount.
func Test_App_Mount_ParametricPrefixNested(t *testing.T) {
	t.Parallel()

	micro := New()
	micro.Get("/doe", func(c Ctx) error {
		return c.SendString("version=" + c.Params("version"))
	})

	sub := New()
	sub.Use("/v1/:version", micro)

	app := New()
	app.Use("/john", sub)

	apptest.ExpectBody(t, app, "/john/v1/42/doe", StatusOK, "version=42")
}

// Test_App_Mount_CaseInsensitivePath verifies that a mount path is matched by
// the app's own routing rules when its config is resolved: a case-insensitive
// app serves "/api/doe" from a mount registered as "/API", so the mounted app's
// error handler has to answer for it too.
func Test_App_Mount_CaseInsensitivePath(t *testing.T) {
	t.Parallel()

	micro := New(Config{
		ErrorHandler: func(c Ctx, _ error) error {
			return c.Status(599).SendString("micro error")
		},
	})
	micro.Get("/doe", func(_ Ctx) error {
		return errors.New("boom")
	})

	app := New()
	app.Use("/API", micro)

	apptest.Expect(t, app, "/api/doe", 599)

	// A case-sensitive app keeps the two apart.
	strict := New(Config{CaseSensitive: true})
	strict.Use("/API", micro)

	apptest.Expect(t, strict, "/API/doe", 599)

	apptest.Expect(t, strict, "/api/doe", StatusNotFound)
}

// Test_App_Mount_ViewsPathOnly verifies that a mounted app's views are chosen
// by the request path, not by its mount path appearing anywhere in the URL.
func Test_App_Mount_ViewsPathOnly(t *testing.T) {
	t.Parallel()

	subEngine := &testTemplateEngine{path: "testdata2"}
	require.NoError(t, subEngine.Load())

	parentEngine := &testTemplateEngine{}
	require.NoError(t, parentEngine.Load())

	micro := New(Config{Views: subEngine})
	micro.Get("/view", func(c Ctx) error {
		return c.Render("bruh.tmpl", Map{})
	})

	app := New(Config{Views: parentEngine})
	app.Use("/john", micro)
	app.Get("/elsewhere", func(c Ctx) error {
		return c.Render("index.tmpl", Map{"Title": "parent"})
	})

	apptest.ExpectBody(t, app, "/elsewhere?next=/john/view", StatusOK, "<h1>parent</h1>")
}

// Test_App_Mount_ParametricPrefixConstraint verifies that a constraint named in
// a mount prefix is enforced. The prefix belongs to the app doing the mounting,
// so its constraints have to reach the re-parse of the mounted routes.
func Test_App_Mount_ParametricPrefixConstraint(t *testing.T) {
	t.Parallel()

	micro := New()
	micro.Get("/doe", func(c Ctx) error {
		return c.SendString("name=" + c.Params("name"))
	})

	app := New()
	app.RegisterCustomConstraint(&onlyFooConstraint{})
	app.Use("/:name<onlyfoo>", micro)

	apptest.Expect(t, app, "/foo/doe", StatusOK)

	resp := apptest.Do(t, app, "/bar/doe")
	require.Equal(t, StatusNotFound, resp.StatusCode, "the prefix constraint still rejects")
}

func Test_App_Mount_StrictRouting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path   string
		strict int
		loose  int
	}{
		{path: "/api/sub", strict: StatusOK, loose: StatusOK},
		{path: "/api/sub/", strict: StatusNotFound, loose: StatusOK},
		{path: "/api/p/7", strict: StatusOK, loose: StatusOK},
		{path: "/api/p/7/", strict: StatusNotFound, loose: StatusOK},
	}

	for _, strict := range []bool{false, true} {
		app := New(Config{StrictRouting: strict})
		sub := New()
		sub.Get("/sub", func(c Ctx) error { return c.SendString("plain") })
		sub.Get("/p/:id", func(c Ctx) error { return c.SendString("param") })
		app.Use("/api", sub)

		for _, tc := range tests {
			want := tc.loose
			if strict {
				want = tc.strict
			}
			resp := apptest.Do(t, app, tc.path)
			require.Equal(t, want, resp.StatusCode, "strict=%v path=%q", strict, tc.path)
		}
	}
}

package apptest_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"

	"github.com/gofiber/fiber/v3/internal/apptest"
)

// fakeTB records what the helpers report instead of failing the real test, so
// the failure paths can be asserted on. FailNow ends the goroutine it is called
// on, like the real one, which is why run executes the helper on its own.
type fakeTB struct {
	testing.TB // satisfies testing.TB's unexported method; anything unused panics

	messages []string
	cleanups []func()

	mu     sync.Mutex
	failed bool
}

func (*fakeTB) Helper() {}

func (*fakeTB) Name() string { return "fakeTB" }

func (f *fakeTB) Errorf(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed = true
	f.messages = append(f.messages, fmt.Sprintf(format, args...))
}

func (f *fakeTB) Fatalf(format string, args ...any) {
	f.Errorf(format, args...)
	f.FailNow()
}

func (f *fakeTB) FailNow() {
	f.mu.Lock()
	f.failed = true
	f.mu.Unlock()
	runtime.Goexit()
}

func (f *fakeTB) Cleanup(fn func()) { f.cleanups = append(f.cleanups, fn) }

// run calls fn with f and then the registered cleanups, like a finished test.
func (f *fakeTB) run(fn func(tb *fakeTB)) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(f)
	}()
	<-done
	for _, cleanup := range slices.Backward(f.cleanups) {
		cleanup()
	}
}

func (f *fakeTB) output() string { return strings.Join(f.messages, "\n") }

// testConfig stands in for fiber.TestConfig.
type testConfig struct{ timeout int }

// fakeApp stands in for *fiber.App: it records the requests it receives and
// answers each with a canned response.
type fakeApp struct {
	err      error
	body     io.ReadCloser
	requests []*http.Request
	configs  [][]testConfig
	status   int
}

func (a *fakeApp) Test(req *http.Request, config ...testConfig) (*http.Response, error) {
	a.requests = append(a.requests, req)
	a.configs = append(a.configs, config)
	if a.err != nil {
		return nil, a.err
	}
	body := a.body
	if body == nil {
		body = io.NopCloser(strings.NewReader(""))
	}
	return &http.Response{StatusCode: a.status, Header: http.Header{}, Body: body}, nil
}

// trackedBody reports whether it was closed.
type trackedBody struct {
	io.Reader
	closed int
}

func (b *trackedBody) Close() error {
	b.closed++
	return nil
}

func newApp(status int, body string) *fakeApp {
	return &fakeApp{status: status, body: &trackedBody{Reader: strings.NewReader(body)}}
}

func Test_Req(t *testing.T) {
	t.Parallel()

	req := apptest.Req(http.MethodPost, "/users?x=1", apptest.WithHeader("X-Test", "a"), apptest.WithHeader("X-Test", "b"), apptest.WithHost("example.com"))

	require.Equal(t, http.MethodPost, req.Method)
	require.Equal(t, "/users", req.URL.Path)
	require.Equal(t, "x=1", req.URL.RawQuery)
	require.Equal(t, "b", req.Header.Get("X-Test"), "the last WithHeader wins")
	require.Equal(t, "example.com", req.Host)
	require.Equal(t, http.NoBody, req.Body)
}

func Test_Do(t *testing.T) {
	t.Parallel()

	t.Run("string target is a GET", func(t *testing.T) {
		t.Parallel()
		app := newApp(http.StatusTeapot, "")

		resp := apptest.Do(t, app, "/brew")

		require.Equal(t, http.StatusTeapot, resp.StatusCode)
		require.Len(t, app.requests, 1)
		require.Equal(t, http.MethodGet, app.requests[0].Method)
		require.Equal(t, "/brew", app.requests[0].URL.Path)
		require.Equal(t, http.NoBody, app.requests[0].Body)
	})

	t.Run("request target is sent as is", func(t *testing.T) {
		t.Parallel()
		app := newApp(http.StatusOK, "")
		req := apptest.Req(http.MethodHead, "/", apptest.WithHost("example.com"))

		apptest.Do(t, app, req)

		require.Len(t, app.requests, 1)
		require.Same(t, req, app.requests[0])
	})

	t.Run("config is passed to the app", func(t *testing.T) {
		t.Parallel()
		app := newApp(http.StatusOK, "")

		apptest.Do(t, app, "/", testConfig{timeout: 7})
		apptest.Do(t, app, "/")

		require.Equal(t, [][]testConfig{{{timeout: 7}}, nil}, app.configs)
	})

	t.Run("app error fails the test", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("boom")
		app := &fakeApp{err: boom}
		fake := &fakeTB{}

		fake.run(func(tb *fakeTB) { apptest.Do(tb, app, "/broken") })

		require.True(t, fake.failed)
		require.Contains(t, fake.output(), "boom")
		require.Contains(t, fake.output(), "GET /broken")
	})
}

func Test_Expect(t *testing.T) {
	t.Parallel()

	t.Run("matching status returns the response", func(t *testing.T) {
		t.Parallel()
		app := newApp(http.StatusCreated, "made")

		resp := apptest.Expect(t, app, "/", http.StatusCreated)

		require.Equal(t, http.StatusCreated, resp.StatusCode)
		require.Equal(t, "made", apptest.Body(t, resp), "the body has not been consumed")
	})

	t.Run("mismatch fails and reports the body", func(t *testing.T) {
		t.Parallel()
		app := newApp(http.StatusInternalServerError, "database is down")
		fake := &fakeTB{}

		fake.run(func(tb *fakeTB) { apptest.Expect(tb, app, apptest.Req(http.MethodPut, "/save"), http.StatusOK) })

		require.True(t, fake.failed)
		out := fake.output()
		require.Contains(t, out, "PUT /save")
		require.Contains(t, out, "status = 500, want 200")
		require.Contains(t, out, "database is down")
	})

	t.Run("app error fails the test", func(t *testing.T) {
		t.Parallel()
		fake := &fakeTB{}

		fake.run(func(tb *fakeTB) { apptest.Expect(tb, &fakeApp{err: errors.New("boom")}, "/", http.StatusOK) })

		require.True(t, fake.failed)
		require.Contains(t, fake.output(), "boom")
	})
}

func Test_ExpectBody(t *testing.T) {
	t.Parallel()

	t.Run("matching status and body", func(t *testing.T) {
		t.Parallel()
		app := newApp(http.StatusOK, "hello")

		resp := apptest.ExpectBody(t, app, "/", http.StatusOK, "hello")

		require.Equal(t, "hello", apptest.Body(t, resp), "the body stays readable")
	})

	t.Run("empty body", func(t *testing.T) {
		t.Parallel()

		apptest.ExpectBody(t, newApp(http.StatusNoContent, ""), "/", http.StatusNoContent, "")
	})

	t.Run("body mismatch fails", func(t *testing.T) {
		t.Parallel()
		fake := &fakeTB{}

		fake.run(func(tb *fakeTB) {
			apptest.ExpectBody(tb, newApp(http.StatusOK, "actual"), "/page", http.StatusOK, "wanted")
		})

		require.True(t, fake.failed)
		out := fake.output()
		require.Contains(t, out, "GET /page: body")
		require.Contains(t, out, "wanted")
		require.Contains(t, out, "actual")
	})

	t.Run("status mismatch fails before the body is compared", func(t *testing.T) {
		t.Parallel()
		fake := &fakeTB{}

		fake.run(func(tb *fakeTB) {
			apptest.ExpectBody(tb, newApp(http.StatusNotFound, "Not Found"), "/", http.StatusOK, "hello")
		})

		require.True(t, fake.failed)
		require.Contains(t, fake.output(), "status = 404, want 200")
		require.NotContains(t, fake.output(), "hello", "the body assertion must not run")
	})
}

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func (failingBody) Close() error             { return nil }

func Test_Body(t *testing.T) {
	t.Parallel()

	t.Run("reads, closes and can be repeated", func(t *testing.T) {
		t.Parallel()
		body := &trackedBody{Reader: strings.NewReader("payload")}
		resp := &http.Response{Body: body}

		require.Equal(t, "payload", apptest.Body(t, resp))
		require.Equal(t, 1, body.closed, "the original body is closed")
		require.Equal(t, "payload", apptest.Body(t, resp), "a second call sees the same body")
		require.Equal(t, []byte("payload"), apptest.BodyBytes(t, resp))

		again, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, "payload", string(again), "resp.Body is still readable")
		require.Equal(t, 1, body.closed, "the original body is closed once")
	})

	t.Run("read error fails the test", func(t *testing.T) {
		t.Parallel()
		fake := &fakeTB{}

		fake.run(func(tb *fakeTB) { apptest.Body(tb, &http.Response{Body: failingBody{}}) })

		require.True(t, fake.failed)
		require.Contains(t, fake.output(), "read failed")
	})
}

// fakeCtx stands in for fiber.CustomCtx.
type fakeCtx interface{ ID() int }

type realCtx struct{ id int }

func (c *realCtx) ID() int { return c.id }

// fakePool stands in for *fiber.App's context pool.
type fakePool struct {
	acquired []*fasthttp.RequestCtx
	released []fakeCtx
	contexts []*realCtx
}

func (p *fakePool) AcquireCtx(fctx *fasthttp.RequestCtx) fakeCtx {
	c := &realCtx{id: len(p.contexts) + 1}
	p.contexts = append(p.contexts, c)
	p.acquired = append(p.acquired, fctx)
	return c
}

func (p *fakePool) ReleaseCtx(c fakeCtx) { p.released = append(p.released, c) }

// otherCtx is a context type that CtxAs is not asked for.
type otherCtx struct{ realCtx }

type otherPool struct{ fakePool }

func (p *otherPool) AcquireCtx(fctx *fasthttp.RequestCtx) fakeCtx {
	p.acquired = append(p.acquired, fctx)
	return &otherCtx{}
}

func Test_Ctx(t *testing.T) {
	t.Parallel()

	t.Run("released when the test ends", func(t *testing.T) {
		t.Parallel()
		pool := &fakePool{}
		fake := &fakeTB{}

		var c fakeCtx
		fake.run(func(tb *fakeTB) {
			c = apptest.Ctx(tb, pool)
			require.Empty(t, pool.released, "still held while the test runs")
		})

		require.False(t, fake.failed)
		require.Equal(t, 1, c.ID())
		require.Len(t, pool.acquired, 1)
		require.NotNil(t, pool.acquired[0], "a blank request context")
		require.Equal(t, []fakeCtx{c}, pool.released, "returned to the pool by the cleanup")
	})

	t.Run("CtxFor uses the given request context", func(t *testing.T) {
		t.Parallel()
		pool := &fakePool{}
		fctx := &fasthttp.RequestCtx{}
		fctx.Request.SetRequestURI("/prepared")
		fake := &fakeTB{}

		fake.run(func(tb *fakeTB) { apptest.CtxFor(tb, pool, fctx) })

		require.Same(t, fctx, pool.acquired[0])
		require.Len(t, pool.released, 1)
	})

	t.Run("each context is released once, newest first", func(t *testing.T) {
		t.Parallel()
		pool := &fakePool{}
		fake := &fakeTB{}

		fake.run(func(tb *fakeTB) {
			apptest.Ctx(tb, pool)
			apptest.Ctx(tb, pool)
		})

		require.Len(t, pool.released, 2)
		require.Equal(t, 2, pool.released[0].ID())
		require.Equal(t, 1, pool.released[1].ID())
	})
}

func Test_CtxAs(t *testing.T) {
	t.Parallel()

	t.Run("returns the concrete type", func(t *testing.T) {
		t.Parallel()
		pool := &fakePool{}
		fake := &fakeTB{}

		var c *realCtx
		fake.run(func(tb *fakeTB) { c = apptest.CtxAs[*realCtx](tb, pool) })

		require.False(t, fake.failed)
		require.Same(t, pool.contexts[0], c)
		require.Len(t, pool.released, 1)
	})

	t.Run("fails when the app hands out another type", func(t *testing.T) {
		t.Parallel()
		pool := &otherPool{}
		fake := &fakeTB{}

		fake.run(func(tb *fakeTB) { apptest.CtxAs[*realCtx](tb, pool) })

		require.True(t, fake.failed)
		require.Contains(t, fake.output(), "*apptest_test.otherCtx")
		require.Contains(t, fake.output(), "*apptest_test.realCtx")
	})
}

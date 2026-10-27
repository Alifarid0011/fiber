package fiber

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3/internal/apptest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func TestToFiberHandler_Nil(t *testing.T) {
	t.Parallel()
	var handler Handler
	converted, ok := toFiberHandler(handler)
	require.False(t, ok)
	require.Nil(t, converted)
}

func TestToFiberHandler_FiberHandler(t *testing.T) {
	t.Parallel()
	fiberHandler := func(c Ctx) error { return c.SendStatus(http.StatusAccepted) }

	converted, ok := toFiberHandler(fiberHandler)
	require.True(t, ok)
	require.NotNil(t, converted)
	require.Equal(t, reflect.ValueOf(fiberHandler).Pointer(), reflect.ValueOf(converted).Pointer())
}

func TestToFiberHandler_FiberHandlerNoErrorReturn(t *testing.T) {
	t.Parallel()
	app, ctx := newTestCtx(t)

	handler := func(c Ctx) {
		require.Equal(t, app, c.App())
		c.Set("X-Handler", "ok")
	}

	converted, ok := toFiberHandler(handler)
	require.True(t, ok)
	require.NotNil(t, converted)

	require.NoError(t, converted(ctx))
	require.Equal(t, "ok", string(ctx.Response().Header.Peek("X-Handler")))
}

func TestNewTestCtx_ReturnsDefaultCtx(t *testing.T) {
	t.Parallel()
	app, ctx := newTestCtx(t)
	require.NotNil(t, app)
	require.NotNil(t, ctx)
	require.Equal(t, app, ctx.App())
}

func newTestCtx(t *testing.T) (*App, *DefaultCtx) {
	t.Helper()

	app := New()
	fasthttpCtx := &fasthttp.RequestCtx{}
	customCtx := apptest.CtxFor(t, app, fasthttpCtx)
	ctx, ok := customCtx.(*DefaultCtx)
	require.True(t, ok)

	return app, ctx
}

func withRouteHandlers(t *testing.T, ctx *DefaultCtx, handlers ...Handler) {
	t.Helper()

	ctx.route = &Route{Handlers: handlers}
	ctx.indexHandler = 0
	t.Cleanup(func() {
		ctx.route = nil
		ctx.indexHandler = 0
	})
}

// rootMiscAAssertBound asserts that the Req and Res an Express-style handler
// receives belong to app.
func rootMiscAAssertBound(t *testing.T, app *App, req Req, res Res) {
	t.Helper()

	assert.Equal(t, app, req.App())
	assert.Equal(t, app, res.App())
}

// rootMiscARunChain converts the Express-style handler and runs it on ctx in
// front of a downstream handler that returns nextErr. It reports whether the
// downstream handler ran and the error the converted handler returned.
func rootMiscARunChain(t *testing.T, ctx *DefaultCtx, nextErr error, handler any) (bool, error) {
	t.Helper()

	converted, ok := toFiberHandler(handler)
	require.True(t, ok)

	nextCalled := false
	nextHandler := func(_ Ctx) error {
		nextCalled = true
		return nextErr
	}
	withRouteHandlers(t, ctx, converted, nextHandler)

	err := converted(ctx)
	return nextCalled, err
}

func TestToFiberHandler_ExpressTwoParamsWithError(t *testing.T) {
	t.Parallel()
	app, ctx := newTestCtx(t)

	handler := func(req Req, res Res) error {
		rootMiscAAssertBound(t, app, req, res)
		return res.SendString("express")
	}

	converted, ok := toFiberHandler(handler)
	require.True(t, ok)

	require.NoError(t, converted(ctx))
	require.Equal(t, "express", string(ctx.Response().Body()))
}

func TestToFiberHandler_ExpressTwoParamsWithoutError(t *testing.T) {
	t.Parallel()
	app, ctx := newTestCtx(t)

	handler := func(req Req, res Res) {
		rootMiscAAssertBound(t, app, req, res)
		require.NoError(t, res.SendStatus(http.StatusCreated))
	}

	converted, ok := toFiberHandler(handler)
	require.True(t, ok)

	require.NoError(t, converted(ctx))
	require.Equal(t, http.StatusCreated, ctx.Response().StatusCode())
}

func TestToFiberHandler_ExpressThreeParamsWithError(t *testing.T) {
	t.Parallel()
	app, ctx := newTestCtx(t)

	handler := func(req Req, res Res, next func() error) error {
		rootMiscAAssertBound(t, app, req, res)
		return next()
	}

	nextErr := errors.New("next")
	nextCalled, err := rootMiscARunChain(t, ctx, nextErr, handler)
	require.ErrorIs(t, err, nextErr)
	require.True(t, nextCalled)
}

func TestToFiberHandler_ExpressThreeParamsWithError_PrefersHandlerErrorOverNextError(t *testing.T) {
	t.Parallel()
	app, ctx := newTestCtx(t)

	handler := func(req Req, res Res, next func() error) error {
		rootMiscAAssertBound(t, app, req, res)
		require.EqualError(t, next(), "next error")
		return errors.New("handler error")
	}

	nextCalled, err := rootMiscARunChain(t, ctx, errors.New("next error"), handler)
	require.EqualError(t, err, "handler error")
	require.True(t, nextCalled)
}

func TestToFiberHandler_ExpressThreeParamsWithError_PropagatesNextErrorWhenNoReturnError(t *testing.T) {
	t.Parallel()
	app, ctx := newTestCtx(t)

	nextErr := errors.New("next error")

	handler := func(req Req, res Res, next func() error) error {
		rootMiscAAssertBound(t, app, req, res)
		require.ErrorIs(t, next(), nextErr)
		return nil
	}

	nextCalled, err := rootMiscARunChain(t, ctx, nextErr, handler)
	require.ErrorIs(t, err, nextErr)
	require.True(t, nextCalled)
}

func TestToFiberHandler_ExpressThreeParamsWithoutError(t *testing.T) {
	t.Parallel()
	app, ctx := newTestCtx(t)

	handler := func(req Req, res Res, next func() error) {
		rootMiscAAssertBound(t, app, req, res)
		err := next()
		require.Error(t, err)
		assert.EqualError(t, err, "next without error")
	}

	nextCalled, err := rootMiscARunChain(t, ctx, errors.New("next without error"), handler)
	require.EqualError(t, err, "next without error")
	require.True(t, nextCalled)
}

func TestToFiberHandler_ExpressNextNoArgWithErrorReturn(t *testing.T) {
	t.Parallel()
	app, ctx := newTestCtx(t)

	handler := func(req Req, res Res, next func()) error {
		rootMiscAAssertBound(t, app, req, res)
		next()
		return nil
	}

	nextErr := errors.New("next without return value")
	nextCalled, err := rootMiscARunChain(t, ctx, nextErr, handler)
	require.ErrorIs(t, err, nextErr)
	require.True(t, nextCalled)
}

func TestToFiberHandler_ExpressNextWithErrorContinuesOnNil(t *testing.T) {
	t.Parallel()
	app, ctx := newTestCtx(t)

	handler := func(req Req, res Res, next func(error)) {
		rootMiscAAssertBound(t, app, req, res)
		next(nil)
	}

	nextCalled, err := rootMiscARunChain(t, ctx, nil, handler)
	require.NoError(t, err)
	require.True(t, nextCalled)
}

func TestToFiberHandler_ExpressNextWithErrorShortCircuitsOnError(t *testing.T) {
	t.Parallel()
	app, ctx := newTestCtx(t)

	handler := func(req Req, res Res, next func(error)) {
		rootMiscAAssertBound(t, app, req, res)
		next(errors.New("next error"))
	}

	nextCalled, err := rootMiscARunChain(t, ctx, nil, handler)
	require.EqualError(t, err, "next error")
	require.False(t, nextCalled)
}

func TestToFiberHandler_ExpressNextWithErrorReturn_ShortCircuitsOnNextError(t *testing.T) {
	t.Parallel()
	app, ctx := newTestCtx(t)

	handler := func(req Req, res Res, next func(error)) error {
		rootMiscAAssertBound(t, app, req, res)
		next(errors.New("next error"))
		return nil
	}

	nextCalled, err := rootMiscARunChain(t, ctx, nil, handler)
	require.EqualError(t, err, "next error")
	require.False(t, nextCalled)
}

func TestToFiberHandler_ExpressNextWithErrorReturnCallback_PropagatesNextError(t *testing.T) {
	t.Parallel()
	app, ctx := newTestCtx(t)

	handler := func(req Req, res Res, next func(error) error) {
		rootMiscAAssertBound(t, app, req, res)
		require.EqualError(t, next(nil), "next error")
	}

	nextErr := errors.New("next error")
	nextCalled, err := rootMiscARunChain(t, ctx, nextErr, handler)
	require.ErrorIs(t, err, nextErr)
	require.True(t, nextCalled)
}

func TestToFiberHandler_ExpressNextWithErrorReturnCallback_ShortCircuitsOnNextError(t *testing.T) {
	t.Parallel()
	app, ctx := newTestCtx(t)

	handler := func(req Req, res Res, next func(error) error) {
		rootMiscAAssertBound(t, app, req, res)
		require.EqualError(t, next(errors.New("next error")), "next error")
	}

	nextCalled, err := rootMiscARunChain(t, ctx, nil, handler)
	require.EqualError(t, err, "next error")
	require.False(t, nextCalled)
}

func TestToFiberHandler_ExpressNextWithErrorReturn_PrefersHandlerErrorOverNextError(t *testing.T) {
	t.Parallel()
	app, ctx := newTestCtx(t)

	handler := func(req Req, res Res, next func(error) error) error {
		rootMiscAAssertBound(t, app, req, res)
		require.EqualError(t, next(errors.New("next error")), "next error")
		return errors.New("handler error")
	}

	nextCalled, err := rootMiscARunChain(t, ctx, nil, handler)
	require.EqualError(t, err, "handler error")
	require.False(t, nextCalled)
}

func TestToFiberHandler_ExpressNextWithErrorReturn_PropagatesNextErrorWhenNoReturnError(t *testing.T) {
	t.Parallel()
	app, ctx := newTestCtx(t)

	handler := func(req Req, res Res, next func(error) error) error {
		rootMiscAAssertBound(t, app, req, res)
		return next(errors.New("next error"))
	}

	nextCalled, err := rootMiscARunChain(t, ctx, nil, handler)
	require.EqualError(t, err, "next error")
	require.False(t, nextCalled)
}

func TestToFiberHandler_ExpressNextWithErrorReturnCallback_StopsChainWithoutNextCall(t *testing.T) {
	t.Parallel()
	app, ctx := newTestCtx(t)

	handler := func(req Req, res Res, _ func(error) error) {
		rootMiscAAssertBound(t, app, req, res)
		// Intentionally do not call next.
	}

	nextCalled, err := rootMiscARunChain(t, ctx, errors.New("should not be called"), handler)
	require.NoError(t, err)
	require.False(t, nextCalled)
}

func TestAdapter_MixedHandlerIntegration(t *testing.T) {
	app := New()

	app.Use(func(c Ctx) error {
		c.Set("X-Middleware", "fiber")
		return c.Next()
	})

	app.Use(func(_ Req, res Res, next func() error) error {
		res.Set("X-Express", "middleware")
		return next()
	})

	app.Get("/fiber", func(c Ctx) error {
		c.Set("X-Route", "fiber")
		return c.SendString("fiber handler")
	})

	app.Post("/express", func(_ Req, res Res) error {
		res.Set("X-Route", "express")
		return res.SendString("express handler")
	})

	var httpHandlerWriteErr error
	app.Put("/http", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Route", "http")
		w.WriteHeader(http.StatusAccepted)
		_, httpHandlerWriteErr = w.Write([]byte("http handler"))
	})

	app.Delete("/fasthttp", func(ctx *fasthttp.RequestCtx) error {
		ctx.Response.Header.Set("X-Route", "fasthttp")
		ctx.SetStatusCode(http.StatusCreated)
		ctx.SetBodyString("fasthttp handler")
		return nil
	})

	tests := []struct {
		name       string
		method     string
		path       string
		wantBody   string
		wantRoute  string
		wantStatus int
	}{
		{name: "fiber", method: http.MethodGet, path: "/fiber", wantStatus: http.StatusOK, wantBody: "fiber handler", wantRoute: "fiber"},
		{name: "express", method: http.MethodPost, path: "/express", wantStatus: http.StatusOK, wantBody: "express handler", wantRoute: "express"},
		{name: "net/http", method: http.MethodPut, path: "/http", wantStatus: http.StatusAccepted, wantBody: "http handler", wantRoute: "http"},
		{name: "fasthttp", method: http.MethodDelete, path: "/fasthttp", wantStatus: http.StatusCreated, wantBody: "fasthttp handler", wantRoute: "fasthttp"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := apptest.Do(t, app, apptest.Req(tc.method, tc.path))
			t.Cleanup(func() { require.NoError(t, resp.Body.Close()) })

			body := apptest.Body(t, resp)

			require.Equal(t, tc.wantStatus, resp.StatusCode)
			require.Equal(t, tc.wantBody, body)
			require.Equal(t, "fiber", resp.Header.Get("X-Middleware"))
			require.Equal(t, "middleware", resp.Header.Get("X-Express"))
			require.Equal(t, tc.wantRoute, resp.Header.Get("X-Route"))
		})
	}

	require.NoError(t, httpHandlerWriteErr)
}

func TestToFiberHandler_ExpressNextNoArgPropagatesError(t *testing.T) {
	t.Parallel()
	app, ctx := newTestCtx(t)

	handler := func(req Req, res Res, next func()) {
		rootMiscAAssertBound(t, app, req, res)
		next()
	}

	nextErr := errors.New("next without return value")
	nextCalled, err := rootMiscARunChain(t, ctx, nextErr, handler)
	require.ErrorIs(t, err, nextErr)
	require.True(t, nextCalled)
}

func TestToFiberHandler_ExpressNextNoArgStopsChain(t *testing.T) {
	t.Parallel()
	app, ctx := newTestCtx(t)

	handler := func(req Req, res Res, _ func()) {
		rootMiscAAssertBound(t, app, req, res)
		// Intentionally do not call next().
	}

	nextCalled, err := rootMiscARunChain(t, ctx, errors.New("should not be called"), handler)
	require.NoError(t, err)
	require.False(t, nextCalled)
}

func TestToFiberHandler_ExpressNextNoArgMiddleware(t *testing.T) {
	t.Parallel()
	app := New()
	t.Cleanup(func() { require.NoError(t, app.Shutdown()) })

	callOrder := make([]string, 0, 2)

	app.Use(func(req Req, res Res, next func()) {
		callOrder = append(callOrder, "middleware")
		next()
		rootMiscAAssertBound(t, app, req, res)
	})

	app.Get("/", func(c Ctx) error {
		callOrder = append(callOrder, "handler")
		return c.SendStatus(http.StatusOK)
	})

	resp := apptest.Expect(t, app, "/", http.StatusOK)
	require.NoError(t, resp.Body.Close())

	require.Equal(t, []string{"middleware", "handler"}, callOrder)
}

func TestCollectHandlers_HTTPHandler(t *testing.T) {
	t.Parallel()
	var writeErr error
	httpHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-HTTP", "ok")
		w.WriteHeader(http.StatusTeapot)
		_, writeErr = w.Write([]byte("http"))
	})

	handlers := collectHandlers("test", httpHandler)
	require.Len(t, handlers, 1)
	converted := handlers[0]
	require.NotNil(t, converted)

	ctx := apptest.Ctx(t, New())

	require.NoError(t, converted(ctx))
	require.Equal(t, http.StatusTeapot, ctx.Response().StatusCode())
	require.Equal(t, "ok", string(ctx.Response().Header.Peek("X-HTTP")))
	require.Equal(t, "http", string(ctx.Response().Body()))
	require.NoError(t, writeErr)
}

func TestToFiberHandler_HTTPHandler(t *testing.T) {
	t.Parallel()
	var writeErr error
	var handler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-HTTP", "handler")
		_, writeErr = w.Write([]byte("through"))
	})

	converted, ok := toFiberHandler(handler)
	require.True(t, ok)
	require.NotNil(t, converted)

	ctx := apptest.Ctx(t, New())

	require.NoError(t, converted(ctx))
	require.Equal(t, "handler", string(ctx.Response().Header.Peek("X-HTTP")))
	require.Equal(t, "through", string(ctx.Response().Body()))
	require.NoError(t, writeErr)
}

func TestToFiberHandler_FasthttpHandlerWithError(t *testing.T) {
	t.Parallel()
	_, ctx := newTestCtx(t)

	fasthttpHandler := func(fctx *fasthttp.RequestCtx) error {
		fctx.Response.Header.Set("X-FASTHTTP", "error")
		return errors.New("fasthttp error")
	}

	converted, ok := toFiberHandler(fasthttpHandler)
	require.True(t, ok)
	require.NotNil(t, converted)

	require.EqualError(t, converted(ctx), "fasthttp error")
	require.Equal(t, "error", string(ctx.Response().Header.Peek("X-FASTHTTP")))
}

func TestToFiberHandler_HTTPHandler_Flush(t *testing.T) {
	t.Parallel()
	var handler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-HTTP", "handler")
		_, err := w.Write([]byte("through"))
		flusher, ok := w.(http.Flusher)
		assert.True(t, ok, "w does not implement http.Flusher")
		flusher.Flush()
		assert.NoError(t, err)
	})

	converted, ok := toFiberHandler(handler)
	require.True(t, ok)
	require.NotNil(t, converted)

	ctx := apptest.Ctx(t, New())

	require.NoError(t, converted(ctx))
	require.Equal(t, "handler", string(ctx.Response().Header.Peek("X-HTTP")))
	require.Equal(t, "through", string(ctx.Response().Body()))
}

func TestWrapHTTPHandler_Flush_App_Test(t *testing.T) {
	t.Parallel()
	app := New()

	app.Get("/", func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("w does not implement http.Flusher")
		}
		w.WriteHeader(StatusOK)
		fmt.Fprintf(w, "Hello ")
		flusher.Flush()
		fmt.Fprintf(w, "World!")
	})

	resp := apptest.Do(t, app, "/")
	defer resp.Body.Close() //nolint:errcheck // not needed

	require.Equal(t, StatusOK, resp.StatusCode)

	require.Equal(t, "Hello World!", apptest.Body(t, resp))
}

func Test_HTTPHandler_App_Test_Interrupted(t *testing.T) {
	t.Parallel()
	app := New()

	app.Get("/", func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("w does not implement http.Flusher")
		}
		w.WriteHeader(StatusOK)
		fmt.Fprintf(w, "Hello ")
		flusher.Flush()
		time.Sleep(500 * time.Millisecond)
		fmt.Fprintf(w, "World!")
	})

	resp, err := app.Test(httptest.NewRequest(MethodGet, "/", http.NoBody), TestConfig{
		Timeout:       200 * time.Millisecond,
		FailOnTimeout: true, // Changed to true to test interrupted behavior
	})
	// With FailOnTimeout: true, we should get a timeout error
	require.ErrorIs(t, err, os.ErrDeadlineExceeded)
	require.Nil(t, resp)
}

func TestToFiberHandler_HTTPHandlerFunc(t *testing.T) {
	t.Parallel()
	httpFunc := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }

	converted, ok := toFiberHandler(httpFunc)
	require.True(t, ok)
	require.NotNil(t, converted)

	ctx := apptest.Ctx(t, New())

	require.NoError(t, converted(ctx))
	require.Equal(t, http.StatusNoContent, ctx.Response().StatusCode())
}

func TestWrapHTTPHandler_Nil(t *testing.T) {
	t.Parallel()
	require.Nil(t, wrapHTTPHandler(nil))
}

func TestCollectHandlers_InvalidType(t *testing.T) {
	t.Parallel()
	require.PanicsWithValue(t, "context: invalid handler #0 (int)\n", func() { collectHandlers("context", 42) })
}

func TestCollectHandlers_TypedNil(t *testing.T) {
	t.Parallel()
	var (
		handlerFunc           http.HandlerFunc
		handler               http.Handler
		raw                   func(http.ResponseWriter, *http.Request)
		requestHandler        fasthttp.RequestHandler
		requestHandlerWithErr func(*fasthttp.RequestCtx) error
	)

	tests := []struct {
		handler any
		name    string
	}{
		{name: "HandlerFunc", handler: handlerFunc},
		{name: "Handler", handler: handler},
		{name: "Function", handler: raw},
		{name: "FuncHTTPHandler", handler: dummyFuncHandler(nil)},
		{name: "RequestHandler", handler: requestHandler},
		{name: "RequestHandlerWithError", handler: requestHandlerWithErr},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			expected := fmt.Sprintf("context: invalid handler #0 (%T)\n", tt.handler)

			require.PanicsWithValue(t, expected, func() { collectHandlers("context", tt.handler) })
		})
	}
}

type dummyHandler struct{}

func (dummyHandler) ServeHTTP(http.ResponseWriter, *http.Request) {}

type dummyFuncHandler func(http.ResponseWriter, *http.Request)

func (handler dummyFuncHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if handler == nil {
		return
	}
	handler(w, r)
}

func TestCollectHandlers_TypedNilPointerHTTPHandler(t *testing.T) {
	t.Parallel()
	var handler http.Handler = (*dummyHandler)(nil)

	require.PanicsWithValue(t, "context: invalid handler #0 (*fiber.dummyHandler)\n", func() {
		collectHandlers("context", handler)
	})
}

func TestCollectHandlers_FasthttpHandler(t *testing.T) {
	t.Parallel()
	before := func(c Ctx) error {
		c.Set("X-Before", "fiber")
		return nil
	}

	fasthttpHandler := fasthttp.RequestHandler(func(ctx *fasthttp.RequestCtx) {
		ctx.Response.Header.Set("X-FASTHTTP", "ok")
		ctx.SetBody([]byte("done"))
	})

	handlers := collectHandlers("fasthttp", before, fasthttpHandler)
	require.Len(t, handlers, 2)

	ctx := apptest.Ctx(t, New())

	for _, handler := range handlers {
		require.NoError(t, handler(ctx))
	}

	require.Equal(t, "fiber", string(ctx.Response().Header.Peek("X-Before")))
	require.Equal(t, "ok", string(ctx.Response().Header.Peek("X-FASTHTTP")))
	require.Equal(t, "done", string(ctx.Response().Body()))
}

func TestCollectHandlers_FiberHandlerNoErrorReturn(t *testing.T) {
	t.Parallel()
	noError := func(c Ctx) { c.Set("X-Handler", "fiber") }

	handlers := collectHandlers("ctx", noError)
	require.Len(t, handlers, 1)

	ctx := apptest.Ctx(t, New())

	require.NoError(t, handlers[0](ctx))
	require.Equal(t, "fiber", string(ctx.Response().Header.Peek("X-Handler")))
}

func TestCollectHandlers_MixedHandlers(t *testing.T) {
	t.Parallel()
	before := func(c Ctx) error {
		c.Set("X-Before", "fiber")
		return nil
	}
	var writeErr error
	httpHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, writeErr = w.Write([]byte("done"))
	})

	handlers := collectHandlers("test", before, httpHandler)
	require.Len(t, handlers, 2)
	require.Equal(t, reflect.ValueOf(before).Pointer(), reflect.ValueOf(handlers[0]).Pointer())
	require.NotNil(t, handlers[1])

	ctx := apptest.Ctx(t, New())

	err := handlers[0](ctx)
	require.NoError(t, err)

	err = handlers[1](ctx)
	require.NoError(t, err)
	require.Equal(t, "done", string(ctx.Response().Body()))
	require.Equal(t, "fiber", string(ctx.Response().Header.Peek("X-Before")))
	require.NoError(t, writeErr)
}

func TestCollectHandlers_Nil(t *testing.T) {
	t.Parallel()
	require.PanicsWithValue(t, "nil: invalid handler #0 (<nil>)\n", func() { collectHandlers("nil", nil) })
}

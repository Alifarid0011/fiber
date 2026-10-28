package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/internal/apptest"
)

// clientTestCCore returns a core that sends a fresh request through client. It
// has no context yet; the tests that run the request set one. The caller
// releases the request.
func clientTestCCore(client *Client) *core {
	c := newCore()
	c.client = client
	c.req = AcquireRequest()

	return c
}

// clientBCore is clientTestCCore with its request released when the test ends.
func clientBCore(t *testing.T, client *Client) *core {
	t.Helper()

	c := clientTestCCore(client)
	t.Cleanup(func() { ReleaseRequest(c.req) })

	return c
}

// clientTestCExecServer starts an app on an in-memory listener and returns the
// listener. The app serves the routes the execution tests send requests to: one
// that answers with the host it was asked for, one that fails, and one that
// answers only after a second. extra adds the routes a test needs besides.
func clientTestCExecServer(t *testing.T, extra func(app *fiber.App)) *fasthttputil.InmemoryListener {
	t.Helper()

	ln := fasthttputil.NewInmemoryListener()
	app := fiber.New()

	app.Get("/normal", func(c fiber.Ctx) error { return c.SendString(c.Hostname()) })

	app.Get("/return-error", func(_ fiber.Ctx) error { return errors.New("the request is error") })

	app.Get("/hang-up", func(c fiber.Ctx) error {
		time.Sleep(time.Second)
		return c.SendString(c.Hostname() + " hang up")
	})

	if extra != nil {
		extra(app)
	}

	go func() { assert.NoError(t, app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true})) }()

	return ln
}

func Test_Exec_Func(t *testing.T) {
	t.Parallel()
	ln := clientTestCExecServer(t, func(app *fiber.App) {
		app.Get("/redirect", func(c fiber.Ctx) error { return c.Redirect().Status(fiber.StatusFound).To("/normal") })
	})

	time.Sleep(300 * time.Millisecond)

	// coreFor returns a core under ctx whose client dials the server above and
	// whose request asks for path on it.
	coreFor := func(ctx context.Context, path string) *core {
		c := clientTestCCore(New())
		c.ctx = ctx
		c.client.SetDial(func(_ string) (net.Conn, error) { return ln.Dial() })
		c.req.RawRequest.SetRequestURI("http://example.com" + path)

		return c
	}

	for _, tc := range []struct {
		tune   func(c *core) // adjusts the client or the request; nil for none
		name   string
		path   string
		body   string
		status int
	}{
		{name: "normal request", path: "/normal", status: 200, body: "example.com"},
		{
			name: "follow redirect with retry config", path: "/redirect", status: 200, body: "example.com",
			tune: func(c *core) {
				c.client.SetRetryConfig(&RetryConfig{MaxRetryCount: 1})
				c.req.SetMaxRedirects(1)
				c.req.RawRequest.Header.SetMethod(fiber.MethodGet)
			},
		},
		{name: "the request return an error", path: "/return-error", status: 500, body: "the request is error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			core := coreFor(context.Background(), tc.path)
			if tc.tune != nil {
				tc.tune(core)
			}

			resp, err := core.execFunc()
			require.NoError(t, err)
			require.Equal(t, tc.status, resp.RawResponse.StatusCode())
			require.Equal(t, tc.body, string(resp.RawResponse.Body()))
		})
	}

	t.Run("the request timeout", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		_, err := coreFor(ctx, "/hang-up").execFunc()

		require.Equal(t, ErrTimeoutOrCancel, err)
	})

	t.Run("cancel drains errChan", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		core := clientTestCCore(New())
		core.ctx = ctx
		core.req.RawRequest.SetRequestURI("http://example.com/drain-err")

		blockingTransport := newBlockingErrTransport(errors.New("upstream failure"))
		core.client.transport = blockingTransport
		defer blockingTransport.release()

		type execResult struct {
			resp *Response
			err  error
		}

		resultCh := make(chan execResult, 1)
		go func() {
			resp, err := core.execFunc()
			resultCh <- execResult{resp: resp, err: err}
		}()

		apptest.Recv(t, blockingTransport.called, time.Second, "transport Do was not invoked")

		cancel()

		result := apptest.Recv(t, resultCh, time.Second, "execFunc did not return")
		require.Nil(t, result.resp)
		require.ErrorIs(t, result.err, ErrTimeoutOrCancel)

		blockingTransport.release()

		apptest.Recv(t, blockingTransport.finished, time.Second, "transport Do did not finish")
	})

	t.Run("timeout snapshots ownership before transport", func(t *testing.T) {
		t.Parallel()
		core, client, req := newCore(), New(), AcquireRequest()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()

		// A helper-owned request: execute releases it as soon as the caller
		// times out, while the transport goroutine is still completing on its
		// own clock. Nothing orders that release before the goroutine builds
		// its response, so any read of req there is a data race.
		req.clientOwned = true
		req.SetURL("http://example.com/owned-timeout")

		transport := newDelayedTransport(150 * time.Millisecond)
		client.transport = transport

		// Once execute returns, req is back in the pool, so the test must not
		// touch it again either: another test may already own the same object.
		resp, err := core.execute(ctx, client, req)
		require.Nil(t, resp)
		require.ErrorIs(t, err, ErrTimeoutOrCancel)

		apptest.Recv(t, transport.finished, time.Second, "transport Do did not finish")

		// Let the goroutine build and hand off its response after Do returned.
		time.Sleep(50 * time.Millisecond)
	})

	t.Run("panic in transport returns error", func(t *testing.T) {
		t.Parallel()
		core := clientTestCCore(New())
		core.ctx = context.Background()

		core.req.RawRequest.SetRequestURI("http://example.com/panic")
		core.client.transport = &clientTestCStubTransport{
			do: func(*fasthttp.Request, *fasthttp.Response) error { panic("boom") },
		}

		resp, err := core.execFunc()

		require.Nil(t, resp)
		require.EqualError(t, err, "client panic: boom")
	})
}

func Test_Execute(t *testing.T) {
	t.Parallel()
	ln := clientTestCExecServer(t, nil)

	// newClient returns a client that dials the server above.
	newClient := func() *Client { return New().SetDial(func(_ string) (net.Conn, error) { return ln.Dial() }) }

	t.Run("add user request hooks", func(t *testing.T) {
		t.Parallel()
		core, client, req := newCore(), newClient(), AcquireRequest()
		client.AddRequestHook(func(_ *Client, _ *Request) error {
			require.Equal(t, "http://example.com", req.URL())
			return nil
		})
		req.SetURL("http://example.com")

		resp, err := core.execute(context.Background(), client, req)
		require.NoError(t, err)
		require.Equal(t, "Not Found", string(resp.RawResponse.Body()))
	})

	t.Run("add user response hooks", func(t *testing.T) {
		t.Parallel()
		core, client, req := newCore(), newClient(), AcquireRequest()
		client.AddResponseHook(func(_ *Client, _ *Response, req *Request) error {
			require.Equal(t, "http://example.com", req.URL())
			return nil
		})
		req.SetURL("http://example.com")

		resp, err := core.execute(context.Background(), client, req)
		require.NoError(t, err)
		require.Equal(t, "Not Found", string(resp.RawResponse.Body()))
	})

	// The slow route answers after a second, so the timeouts set below decide
	// whether the request gets its answer.
	for _, tc := range []struct {
		wantErr        error
		name           string
		body           string
		clientTimeout  time.Duration
		requestTimeout time.Duration
	}{
		{name: "no timeout", body: "example.com hang up"},
		{name: "client timeout", clientTimeout: 500 * time.Millisecond, wantErr: ErrTimeoutOrCancel},
		{name: "request timeout", requestTimeout: 300 * time.Millisecond, wantErr: ErrTimeoutOrCancel},
		{
			name:          "request timeout has higher level",
			clientTimeout: 30 * time.Millisecond, requestTimeout: 3000 * time.Millisecond,
			body: "example.com hang up",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			core, client, req := newCore(), newClient(), AcquireRequest()
			client.SetTimeout(tc.clientTimeout)
			req.SetURL("http://example.com/hang-up").SetTimeout(tc.requestTimeout)

			resp, err := core.execute(context.Background(), client, req)
			if tc.wantErr != nil {
				require.Equal(t, tc.wantErr, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.body, string(resp.RawResponse.Body()))
		})
	}
}

// clientTestCConcurrentHooks runs the hook phase of two requests that share a
// client and requires both hooks to be inside it at the same time: a hook run
// under the client lock would hold the second request back. kind names the hook
// in the failure messages. addHook registers hook, which blocks until released,
// as a hook of that kind; invoke runs the phase for one core.
func clientTestCConcurrentHooks(t *testing.T, kind string, addHook func(client *Client, hook func()), invoke func(core *core) error) {
	t.Helper()

	client := New()
	entered := make(chan struct{}, 2)
	release := make(chan struct{})

	addHook(client, func() {
		entered <- struct{}{}
		<-release
	})

	run := func() <-chan error {
		done := make(chan error, 1)

		go func() {
			core := clientTestCCore(client)
			defer ReleaseRequest(core.req)
			core.req.SetURL("http://example.com")

			done <- invoke(core)
		}()

		return done
	}

	firstDone := run()

	apptest.Recv(t, entered, time.Second, "first "+kind+" hook was not invoked")

	secondDone := run()

	apptest.Recv(t, entered, time.Second, "second "+kind+" hook execution was serialized by the client lock")

	close(release)

	require.NoError(t, <-firstDone)
	require.NoError(t, <-secondDone)
}

func Test_PreHooks_DoesNotSerializeConcurrentRequests(t *testing.T) {
	t.Parallel()
	clientTestCConcurrentHooks(t, "request",
		func(client *Client, hook func()) {
			client.AddRequestHook(func(_ *Client, _ *Request) error {
				hook()
				return nil
			})
		},
		func(core *core) error { return core.preHooks() })
}

func Test_AfterHooks_DoesNotSerializeConcurrentRequests(t *testing.T) {
	t.Parallel()
	clientTestCConcurrentHooks(t, "response",
		func(client *Client, hook func()) {
			client.AddResponseHook(func(_ *Client, _ *Response, _ *Request) error {
				hook()
				return nil
			})
		},
		func(core *core) error {
			resp := AcquireResponse()
			defer ReleaseResponse(resp)

			return core.afterHooks(resp)
		})
}

// Test_PreHooks_ReturnsHookError verifies a failing request hook aborts
// preHooks and its error is propagated (and the client lock is released).
func Test_PreHooks_ReturnsHookError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		add  func(client *Client, hooks ...RequestHook) *Client
		name string
		msg  string
	}{
		{name: "user request hook", add: (*Client).AddRequestHook, msg: "request hook failed"},
		{name: "final request hook", add: (*Client).AddFinalRequestHook, msg: "final request hook failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := New()
			wantErr := errors.New(tc.msg)
			tc.add(client, func(_ *Client, _ *Request) error { return wantErr })

			core := clientBCore(t, client)
			core.req.SetURL("http://example.com")

			require.ErrorIs(t, core.preHooks(), wantErr)
		})
	}
}

func Test_PreHooks_AllowsUserHookClientConfigMutation(t *testing.T) {
	t.Parallel()
	client := New()
	client.AddRequestHook(func(c *Client, _ *Request) error {
		c.SetBaseURL("http://example.com")
		c.SetPathParam("resource", "users")
		c.SetParam("page", "1")
		c.SetHeader("x-fiber-test", "ok")
		c.SetCookie("session", "abc")
		return nil
	})

	core := clientBCore(t, client)
	core.req.SetURL("/:resource")

	require.NoError(t, core.preHooks())
	require.Equal(t, "http://example.com/users?page=1", core.req.RawRequest.URI().String())
	require.Equal(t, "ok", string(core.req.RawRequest.Header.Peek("x-fiber-test")))
	require.Contains(t, string(core.req.RawRequest.Header.Cookie("session")), "abc")
}

func Test_PreHooks_FinalRequestHookSeesSerializedRequest(t *testing.T) {
	t.Parallel()
	client := New()
	client.AddFinalRequestHook(func(_ *Client, req *Request) error {
		require.Equal(t, "POST", string(req.RawRequest.Header.Method()))
		require.Equal(t, "application/json", string(req.RawRequest.Header.ContentType()))
		require.JSONEq(t, `{"name":"fiber"}`, string(req.RawRequest.Body()))
		req.RawRequest.Header.Set("X-Signature", "signed")
		return nil
	})

	core := clientBCore(t, client)
	core.req.SetMethod("POST").SetURL("http://example.com").SetJSON(map[string]string{"name": "fiber"})

	require.NoError(t, core.preHooks())
	require.Equal(t, "signed", string(core.req.RawRequest.Header.Peek("X-Signature")))
}

func Test_PreHooks_ReleasesClientLockWhenBuiltinHookPanics(t *testing.T) {
	t.Parallel()
	client := New()
	callCount := 0
	client.builtinRequestHooks = []RequestHook{func(_ *Client, _ *Request) error {
		callCount++
		if callCount == 1 {
			panic("built-in request hook panic")
		}
		return nil
	}}

	firstCore := clientBCore(t, client)

	require.PanicsWithValue(t, "built-in request hook panic", func() { require.NoError(t, firstCore.preHooks()) })

	secondCore := clientBCore(t, client)

	require.NoError(t, secondCore.preHooks())
	require.Equal(t, 2, callCount)
}

func Test_PreHooks_RunsFinalRequestHooksOutsideClientLock(t *testing.T) {
	t.Parallel()
	client := New()
	hookCount := 0
	client.AddFinalRequestHook(func(c *Client, _ *Request) error {
		hookCount = len(c.FinalRequestHook())
		return nil
	})

	core := clientBCore(t, client)
	core.req.SetURL("http://example.com")

	require.NoError(t, core.preHooks())
	require.Equal(t, 1, hookCount)
}

// Test_AfterHooks_ReturnsUserHookError verifies a failing user response hook
// aborts afterHooks and its error is propagated.
func Test_AfterHooks_ReturnsUserHookError(t *testing.T) {
	t.Parallel()
	client := New()
	wantErr := errors.New("response hook failed")
	client.AddResponseHook(func(_ *Client, _ *Response, _ *Request) error { return wantErr })

	core := clientBCore(t, client)

	resp := clientTestBResponse(t)

	require.ErrorIs(t, core.afterHooks(resp), wantErr)
}

// clientTestCStubTransport is an httpClientTransport that hands every send, by
// whichever method, to do, and does nothing for the rest of the interface. The
// stub transports below are built on it.
type clientTestCStubTransport struct {
	do func(req *fasthttp.Request, resp *fasthttp.Response) error
}

func (s *clientTestCStubTransport) Do(req *fasthttp.Request, resp *fasthttp.Response) error {
	return s.do(req, resp)
}

func (s *clientTestCStubTransport) DoTimeout(req *fasthttp.Request, resp *fasthttp.Response, _ time.Duration) error {
	return s.do(req, resp)
}

func (s *clientTestCStubTransport) DoDeadline(req *fasthttp.Request, resp *fasthttp.Response, _ time.Time) error {
	return s.do(req, resp)
}

func (s *clientTestCStubTransport) DoRedirects(req *fasthttp.Request, resp *fasthttp.Response, _ int) error {
	return s.do(req, resp)
}

func (*clientTestCStubTransport) CloseIdleConnections()      {}
func (*clientTestCStubTransport) TLSConfig() *tls.Config     { return nil }
func (*clientTestCStubTransport) SetTLSConfig(*tls.Config)   {}
func (*clientTestCStubTransport) SetDial(fasthttp.DialFunc)  {}
func (*clientTestCStubTransport) Client() any                { return nil }
func (*clientTestCStubTransport) StreamResponseBody() bool   { return false }
func (*clientTestCStubTransport) SetStreamResponseBody(bool) {}

// delayedTransport completes successfully after a fixed delay, without any
// synchronization with the caller, mirroring a slow upstream that answers after
// the client has already given up.
type delayedTransport struct {
	clientTestCStubTransport
	finished chan struct{}
}

func newDelayedTransport(delay time.Duration) *delayedTransport {
	d := &delayedTransport{finished: make(chan struct{})}
	d.do = func(_ *fasthttp.Request, resp *fasthttp.Response) error {
		time.Sleep(delay)
		resp.SetStatusCode(fasthttp.StatusOK)
		close(d.finished)
		return nil
	}
	return d
}

// blockingErrTransport fails every send with its error, once release lets it.
type blockingErrTransport struct {
	clientTestCStubTransport

	called   chan struct{}
	unblock  chan struct{}
	finished chan struct{}

	calledOnce   sync.Once
	releaseOnce  sync.Once
	finishedOnce sync.Once
}

func newBlockingErrTransport(err error) *blockingErrTransport {
	b := &blockingErrTransport{called: make(chan struct{}), unblock: make(chan struct{}), finished: make(chan struct{})}
	b.do = func(_ *fasthttp.Request, _ *fasthttp.Response) error {
		b.calledOnce.Do(func() { close(b.called) })
		<-b.unblock
		b.finishedOnce.Do(func() { close(b.finished) })
		return err
	}
	return b
}

func (b *blockingErrTransport) release() {
	b.releaseOnce.Do(func() { close(b.unblock) })
}

func Test_Core_RequestBodyStream(t *testing.T) {
	t.Parallel()

	// received is what the server saw of a request body.
	type received struct {
		body   string
		length int
	}

	for _, tc := range []struct{ name, path, content string }{
		{name: "request with body stream is properly copied", path: "/echo", content: "this is streamed body content"},
		{name: "request body stream with content length", path: "/check-length", content: "body with known length"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resultCh := make(chan received, 1)
			client := clientTestAClient(t, func(app *fiber.App) {
				app.Post(tc.path, func(c fiber.Ctx) error {
					resultCh <- received{body: string(c.Body()), length: c.Request().Header.ContentLength()}
					return c.Send(c.Body())
				})
			})

			req := AcquireRequest().SetClient(client).SetURL("http://example.com" + tc.path).
				SetMethod(fiber.MethodPost).SetRawBody([]byte(tc.content))

			resp, err := req.Send()
			require.NoError(t, err)
			defer resp.Close()

			result := <-resultCh
			require.Equal(t, tc.content, string(resp.Body()))
			require.Equal(t, tc.content, result.body)
			require.Equal(t, len(tc.content), result.length)
		})
	}

	t.Run("raw body stream survives CopyTo", func(t *testing.T) {
		t.Parallel()
		const streamContent = "streaming raw request body"

		resultCh := make(chan received, 1)
		client := clientTestAClient(t, func(app *fiber.App) {
			app.Post("/copy-to-body-stream", func(c fiber.Ctx) error {
				body := string(c.Body())
				resultCh <- received{body: body, length: c.Request().Header.ContentLength()}
				return c.SendString(body)
			})
		})

		core := clientTestCCore(client)
		core.ctx = context.Background()

		core.req.RawRequest.SetRequestURI("http://example.com/copy-to-body-stream")
		core.req.RawRequest.Header.SetMethod(fiber.MethodPost)
		core.req.RawRequest.SetBodyStream(bytes.NewBufferString(streamContent), len(streamContent))

		resp, err := core.execFunc()
		require.NoError(t, err)
		defer resp.Close()

		result := <-resultCh
		require.Equal(t, streamContent, string(resp.Body()))
		require.Equal(t, streamContent, result.body)
		require.Equal(t, len(streamContent), result.length)
	})
}

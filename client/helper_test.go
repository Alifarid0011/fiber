package client

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"
)

type testServer struct {
	app *fiber.App
	ch  chan struct{}
	ln  *fasthttputil.InmemoryListener
	tb  testing.TB
}

func startTestServer(tb testing.TB, beforeStarting func(app *fiber.App)) *testServer {
	tb.Helper()

	ln := fasthttputil.NewInmemoryListener()
	app := fiber.New(fiber.Config{
		CBOREncoder: cbor.Marshal,
		CBORDecoder: cbor.Unmarshal,
	})

	if beforeStarting != nil {
		beforeStarting(app)
	}

	ch := make(chan struct{})
	go func() {
		err := app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true})
		assert.NoError(tb, err)

		close(ch)
	}()

	return &testServer{
		app: app,
		ch:  ch,
		ln:  ln,
		tb:  tb,
	}
}

func (ts *testServer) stop() {
	ts.tb.Helper()

	if err := ts.app.Shutdown(); err != nil {
		ts.tb.Fatal(err)
	}

	select {
	case <-ts.ch:
	case <-time.After(time.Second):
		ts.tb.Fatalf("timeout when waiting for server close")
	}
}

func (ts *testServer) dial() func(addr string) (net.Conn, error) {
	ts.tb.Helper()

	return func(_ string) (net.Conn, error) {
		return ts.ln.Dial()
	}
}

func createHelperServer(tb testing.TB) (app *fiber.App, dial func(addr string) (net.Conn, error), start func()) { //nolint:nonamedreturns // gocritic unnamedResult requires explicit result identifiers for helper components
	tb.Helper()

	ln := fasthttputil.NewInmemoryListener()

	app = fiber.New()

	dial = func(_ string) (net.Conn, error) {
		return ln.Dial()
	}
	start = func() {
		require.NoError(tb, app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true}))
	}

	return app, dial, start
}

// clientTestCServeGet serves handler for GET / from a fresh in-memory server and
// returns the dial function that reaches it.
func clientTestCServeGet(tb testing.TB, handler fiber.Handler) func(addr string) (net.Conn, error) {
	tb.Helper()

	app, dial, start := createHelperServer(tb)
	app.Get("/", handler)
	go start()

	return dial
}

// acceptSignalListener closes accepting the first time it is asked to accept,
// which is when the server has registered the listener.
type acceptSignalListener struct {
	net.Listener
	accepting chan struct{}
	once      sync.Once
}

func (l *acceptSignalListener) Accept() (net.Conn, error) {
	l.once.Do(func() { close(l.accepting) })
	return l.Listener.Accept() //nolint:wrapcheck // a transparent wrapper
}

// clientTestCServe serves handler from a fasthttp server on an in-memory
// listener and returns the dial function that reaches it. The server is shut
// down when the test ends, once Serve has returned.
func clientTestCServe(t *testing.T, handler fasthttp.RequestHandler) fasthttp.DialFunc {
	t.Helper()

	mem := fasthttputil.NewInmemoryListener()
	ln := &acceptSignalListener{Listener: mem, accepting: make(chan struct{})}
	server := &fasthttp.Server{Handler: handler}

	done := make(chan struct{})
	go func() {
		defer close(done)
		assert.NoError(t, server.Serve(ln))
	}()
	// A test that expects the request to fail never reaches the server, so
	// without this Shutdown can run before Serve has registered its listener,
	// find nothing to close, and leave Serve blocked in Accept for good.
	<-ln.accepting
	t.Cleanup(func() {
		require.NoError(t, server.Shutdown())
		<-done
	})

	return func(string) (net.Conn, error) { return mem.Dial() }
}

// clientTestCReqResp returns a pooled fasthttp request and response that are
// released when the test ends.
func clientTestCReqResp(t *testing.T) (*fasthttp.Request, *fasthttp.Response) {
	t.Helper()

	req, resp := fasthttp.AcquireRequest(), fasthttp.AcquireResponse()
	t.Cleanup(func() {
		fasthttp.ReleaseRequest(req)
		fasthttp.ReleaseResponse(resp)
	})

	return req, resp
}

// clientTestCRepeat is how many times the testRequest helpers send their
// request: the first of count, or once.
func clientTestCRepeat(count []int) int {
	if len(count) > 0 {
		return count[0]
	}

	return 1
}

func testRequest(t *testing.T, handler fiber.Handler, wrapAgent func(agent *Request), excepted string, count ...int) {
	t.Helper()

	ln := clientTestCServeGet(t, handler)
	client := New().SetDial(ln)

	for range clientTestCRepeat(count) {
		req := AcquireRequest().SetClient(client)
		wrapAgent(req)

		resp, err := req.Get("http://example.com")

		clientTestARequireOK(t, resp, err, excepted)
		resp.Close()
	}
}

func testRequestFail(t *testing.T, handler fiber.Handler, wrapAgent func(agent *Request), excepted error, count ...int) {
	t.Helper()

	ln := clientTestCServeGet(t, handler)
	client := New().SetDial(ln)

	for range clientTestCRepeat(count) {
		req := AcquireRequest().SetClient(client)
		wrapAgent(req)

		_, err := req.Get("http://example.com")

		require.Equal(t, excepted.Error(), err.Error())
	}
}

func testClient(t *testing.T, handler fiber.Handler, wrapAgent func(agent *Client), excepted string, count ...int) { //nolint:unparam // maybe needed
	t.Helper()

	ln := clientTestCServeGet(t, handler)

	for range clientTestCRepeat(count) {
		client := New().SetDial(ln)
		wrapAgent(client)

		resp, err := client.Get("http://example.com")

		clientTestARequireOK(t, resp, err, excepted)
		resp.Close()
	}
}

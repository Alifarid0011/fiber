package timeout

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/internal/apptest"
)

var (
	// Custom error that we treat like a timeout when returned by the handler.
	errCustomTimeout = errors.New("custom timeout error")

	// Some unrelated error that should NOT trigger a request timeout.
	errUnrelated = errors.New("unmatched error")
)

// sleepWithContext simulates a task that takes `d` time, but returns `te` if the context is canceled.
func sleepWithContext(ctx context.Context, d time.Duration, te error) error {
	timer := time.NewTimer(d)
	defer timer.Stop() // Clean up the timer

	select {
	case <-ctx.Done():
		return te
	case <-timer.C:
		return nil
	}
}

// TestTimeout_Statuses tests the status a handler behind the middleware is
// answered with when it completes within the allotted timeout, fails with an
// error or a panic, or has no deadline to exceed.
func TestTimeout_Statuses(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		handler fiber.Handler
		name    string
		cfg     []Config // the arguments of New after the handler
		want    int
	}{
		{
			// The handler sleeps for 10ms, well under the 50ms limit.
			name: "completes within the timeout",
			handler: func(c fiber.Ctx) error {
				// Simulate some work
				if err := sleepWithContext(c.Context(), 10*time.Millisecond, context.DeadlineExceeded); err != nil {
					return err
				}
				return c.SendString("OK")
			},
			cfg:  []Config{{Timeout: 50 * time.Millisecond}},
			want: fiber.StatusOK,
		},
		{
			// Returning a user-defined error is also treated as a timeout.
			name: "custom timeout error",
			handler: func(c fiber.Ctx) error {
				// Sleep might time out, or might return early. If the context is canceled,
				// we treat errCustomTimeout as a 'timeout-like' condition.
				if err := sleepWithContext(c.Context(), 200*time.Millisecond, errCustomTimeout); err != nil {
					return fmt.Errorf("wrapped: %w", err)
				}
				return c.SendString("Should never get here")
			},
			cfg:  []Config{{Timeout: 50 * time.Millisecond, Errors: []error{errCustomTimeout}}},
			want: fiber.StatusServiceUnavailable,
		},
		{
			// An error that is neither a deadline exceeded nor a custom 'timeout'
			// error is propagated as a regular 500 (internal server error).
			name: "unmatched error",
			handler: func(_ fiber.Ctx) error {
				return errUnrelated // Not in the custom error list
			},
			cfg:  []Config{{Timeout: 100 * time.Millisecond, Errors: []error{errCustomTimeout}}},
			want: fiber.StatusInternalServerError,
		},
		{
			// The edge case where the timeout is zero, here by leaving the Config out.
			// Usually this means the request can never exceed a 'deadline' – effectively no timeout.
			name: "zero duration",
			handler: func(c fiber.Ctx) error {
				// Sleep 50ms, but there's no real 'deadline' since zero-timeout.
				time.Sleep(50 * time.Millisecond)
				return c.SendString("No timeout used")
			},
			want: fiber.StatusOK,
		},
		{
			// Negative timeout values fall back to zero.
			name: "negative duration",
			handler: func(c fiber.Ctx) error {
				time.Sleep(50 * time.Millisecond)
				return c.SendString("No timeout used")
			},
			cfg:  []Config{{Timeout: -100 * time.Millisecond}},
			want: fiber.StatusOK,
		},
		{
			// A panic in the handler results in 500 Internal Server Error.
			name:    "panic in the handler",
			handler: func(_ fiber.Ctx) error { panic("test panic") },
			cfg:     []Config{{Timeout: 100 * time.Millisecond}},
			want:    fiber.StatusInternalServerError,
		},
		{
			// When the handler returns a timeout error and no OnTimeout is configured,
			// invokeOnTimeout falls back to fiber.ErrServiceUnavailable. The handler
			// returns immediately so the result is processed via the handler-returned
			// branch deterministically.
			name:    "deadline error without OnTimeout",
			handler: func(_ fiber.Ctx) error { return context.DeadlineExceeded },
			cfg:     []Config{{Timeout: time.Second}},
			want:    fiber.StatusServiceUnavailable,
		},
		{
			// The Next function skips the middleware.
			name: "Next skips the middleware",
			handler: func(c fiber.Ctx) error {
				time.Sleep(100 * time.Millisecond)
				return c.SendString("OK")
			},
			cfg: []Config{{
				Timeout: 10 * time.Millisecond,
				Next: func(_ fiber.Ctx) bool {
					return true // Always skip
				},
			}},
			want: fiber.StatusOK,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := fiber.New()

			app.Get("/", New(tc.handler, tc.cfg...))

			apptest.Expect(t, app, "/", tc.want)
		})
	}
}

// TestTimeout_Exceeded tests a handler that exceeds the provided timeout.
func TestTimeout_Exceeded(t *testing.T) {
	t.Parallel()
	app := fiber.New()

	// The handler sleep and the bound below are spread far apart on purpose: a
	// tight bound measures runner load, not early return.
	const handlerSleep = 2 * time.Second

	// This handler listens for context cancelation and returns early when timeout occurs.
	app.Get("/slow", New(func(c fiber.Ctx) error {
		if err := sleepWithContext(c.Context(), handlerSleep, context.DeadlineExceeded); err != nil {
			return err
		}
		return c.SendString("Should never get here")
	}, Config{Timeout: 50 * time.Millisecond}))

	req := httptest.NewRequest(fiber.MethodGet, "/slow", http.NoBody)
	start := time.Now()
	resp, err := app.Test(req)
	elapsed := time.Since(start)
	require.NoError(t, err, "app.Test(req) should not fail")
	require.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode, "Expected 503 Service Unavailable")
	// Handler should return on cancelation, well before its own sleep elapses.
	require.Less(t, elapsed, handlerSleep/2, "handler should return early on context cancelation")
}

// TestTimeout_DefaultStatus pins the default 503 (RFC 9110 §15.6.4): 408 would
// invite a retry while the timed-out handler may still be running.
func TestTimeout_DefaultStatus(t *testing.T) {
	t.Parallel()
	app := fiber.New()

	var sideEffects atomic.Int32
	app.Post("/charge", New(func(c fiber.Ctx) error {
		// Work that ignores the context, so it finishes after the response went out.
		time.Sleep(150 * time.Millisecond)
		sideEffects.Add(1)
		return c.SendString("charged")
	}, Config{Timeout: 20 * time.Millisecond}))

	resp := apptest.Expect(t, app, apptest.Req(fiber.MethodPost, "/charge"), fiber.StatusServiceUnavailable)
	require.NotEqual(t, fiber.StatusRequestTimeout, resp.StatusCode)

	require.Equal(t, fiber.ErrServiceUnavailable.Message, apptest.Body(t, resp))

	// The premise: the timed-out handler runs to completion after the response.
	require.Zero(t, sideEffects.Load())
	require.Eventually(t, func() bool { return sideEffects.Load() == 1 }, 5*time.Second, 10*time.Millisecond)
}

// TestTimeout_OnTimeoutChoosesStatus shows how to keep or choose another
// status: return the error from OnTimeout. The error shapes the response, body
// included.
func TestTimeout_OnTimeoutChoosesStatus(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		want *fiber.Error
		name string
	}{
		{name: "request timeout", want: fiber.ErrRequestTimeout},
		{name: "gateway timeout", want: fiber.ErrGatewayTimeout},
		{name: "custom message", want: fiber.NewError(fiber.StatusGatewayTimeout, "upstream too slow")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := fiber.New()

			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			app.Get("/slow", New(func(c fiber.Ctx) error {
				<-c.Context().Done()
				<-release
				return nil
			}, Config{Timeout: 20 * time.Millisecond, OnTimeout: func(_ fiber.Ctx) error { return tc.want }}))

			resp := apptest.Expect(t, app, "/slow", tc.want.Code)
			require.Equal(t, tc.want.Message, apptest.Body(t, resp))
		})
	}
}

// TestTimeout_ContextPropagation verifies that the timeout context is properly
// passed to the handler so it can detect cancelation (Issue #3671).
func TestTimeout_ContextPropagation(t *testing.T) {
	t.Parallel()
	app := fiber.New()

	errCh := make(chan error, 1)

	app.Get("/context-aware", New(func(c fiber.Ctx) error {
		timer := time.NewTimer(500 * time.Millisecond)
		defer timer.Stop()

		select {
		case <-timer.C:
			errCh <- nil
			return c.SendString("completed")

		case <-c.Context().Done():
			errCh <- c.Context().Err()
			return c.Context().Err()
		}
	}, Config{Timeout: 50 * time.Millisecond}))

	apptest.Expect(t, app, "/context-aware", fiber.StatusServiceUnavailable)

	safety := time.NewTimer(1 * time.Second)
	defer safety.Stop()

	select {
	case handlerErr := <-errCh:
		require.ErrorIs(t, handlerErr, context.DeadlineExceeded, "handler should report DeadlineExceeded")

	case <-safety.C:
		t.Fatal("timed out waiting for handler to report context state")
	}
}

// TestTimeout_HandlerReturnsEarlyOnCancel verifies that handlers checking context
// can return early, making the overall request faster than the handler's work time.
func TestTimeout_HandlerReturnsEarlyOnCancel(t *testing.T) {
	t.Parallel()
	app := fiber.New()

	app.Get("/early-return", New(func(c fiber.Ctx) error {
		// Handler that would take 500ms but checks context
		for range 50 {
			select {
			case <-c.Context().Done():
				return c.Context().Err()
			case <-time.After(10 * time.Millisecond):
				// Continue work
			}
		}
		return c.SendString("completed")
	}, Config{Timeout: 30 * time.Millisecond}))

	req := httptest.NewRequest(fiber.MethodGet, "/early-return", http.NoBody)
	start := time.Now()
	resp, err := app.Test(req)
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)
	// Should complete much faster than 500ms because handler checks context
	require.Less(t, elapsed, 100*time.Millisecond)
}

// TestTimeout_DefaultResponseClearsBufferedBody verifies that the default timeout
// response does not disclose data buffered by a handler that timed out before
// completing successfully.
func TestTimeout_DefaultResponseClearsBufferedBody(t *testing.T) {
	t.Parallel()
	app := fiber.New()

	app.Get("/partial", New(func(c fiber.Ctx) error {
		require.NoError(t, c.SendString("LEAKED-PARTIAL-SECRET"))
		<-c.Context().Done()
		return c.Context().Err()
	}, Config{Timeout: 20 * time.Millisecond}))

	apptest.ExpectBody(t, app, "/partial", fiber.StatusServiceUnavailable, fiber.ErrServiceUnavailable.Message)
}

// TestTimeout_CustomHandler ensures that a custom handler runs on timeout.
func TestTimeout_CustomHandler(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	var called atomic.Int32

	app.Get("/custom-handler", New(func(c fiber.Ctx) error {
		if err := sleepWithContext(c.Context(), 100*time.Millisecond, context.DeadlineExceeded); err != nil {
			return err
		}
		return c.SendString("should not reach")
	}, Config{
		Timeout: 20 * time.Millisecond,
		OnTimeout: func(c fiber.Ctx) error {
			called.Add(1)
			return c.Status(408).JSON(fiber.Map{"error": "timeout"})
		},
	}))

	resp := apptest.Expect(t, app, "/custom-handler", fiber.StatusRequestTimeout)
	require.Equal(t, int32(1), called.Load())

	body := apptest.Body(t, resp)
	require.JSONEq(t, `{"error":"timeout"}`, body)
}

// TestTimeout_OnTimeoutWritesResponse exercises the timeout path (tCtx.Done())
// while a custom OnTimeout handler shapes the response body. The handler blocks
// past the deadline so the middleware takes the timeout branch rather than the
// handler-returned branch.
func TestTimeout_OnTimeoutWritesResponse(t *testing.T) {
	t.Parallel()
	app := fiber.New()

	var called atomic.Int32
	app.Get("/on-timeout-body", New(func(c fiber.Ctx) error {
		// Wait until the deadline fires, then linger so the middleware has
		// already selected the timeout case before we return.
		<-c.Context().Done()
		time.Sleep(40 * time.Millisecond)
		return nil
	}, Config{
		Timeout: 20 * time.Millisecond,
		OnTimeout: func(c fiber.Ctx) error {
			called.Add(1)
			return c.Status(fiber.StatusServiceUnavailable).SendString("custom timeout body")
		},
	}))

	resp := apptest.Expect(t, app, "/on-timeout-body", fiber.StatusServiceUnavailable)
	require.Equal(t, int32(1), called.Load())

	require.Equal(t, "custom timeout body", apptest.Body(t, resp))
}

// TestTimeout_OnTimeoutEmptyResponse exercises the timeout path with an
// OnTimeout handler that leaves the response untouched (still default 200/empty),
// so the middleware falls back to writing the default 503 timeout response.
func TestTimeout_OnTimeoutEmptyResponse(t *testing.T) {
	t.Parallel()
	app := fiber.New()

	var called atomic.Int32
	app.Get("/on-timeout-empty", New(func(c fiber.Ctx) error {
		<-c.Context().Done()
		time.Sleep(40 * time.Millisecond)
		return nil
	}, Config{
		Timeout: 20 * time.Millisecond,
		OnTimeout: func(_ fiber.Ctx) error {
			called.Add(1)
			// Intentionally do not write any response.
			return nil
		},
	}))

	resp := apptest.Expect(t, app, "/on-timeout-empty", fiber.StatusServiceUnavailable)
	require.Equal(t, int32(1), called.Load())

	require.Equal(t, fiber.ErrServiceUnavailable.Message, apptest.Body(t, resp))
}

// TestIsTimeoutError_DeadlineExceeded ensures context.DeadlineExceeded triggers timeout.
func TestIsTimeoutError_DeadlineExceeded(t *testing.T) {
	t.Parallel()

	require.True(t, isTimeoutError(context.DeadlineExceeded, nil))
	require.True(t, isTimeoutError(fmt.Errorf("wrap: %w", context.DeadlineExceeded), nil))
}

// TestIsTimeoutError_CustomErrors verifies custom errors are detected.
func TestIsTimeoutError_CustomErrors(t *testing.T) {
	t.Parallel()

	customErr := errors.New("custom timeout")
	require.True(t, isTimeoutError(customErr, []error{customErr}))
	require.True(t, isTimeoutError(fmt.Errorf("wrap: %w", customErr), []error{customErr}))
	require.False(t, isTimeoutError(errUnrelated, []error{customErr}))
}

// TestIsTimeoutError_WithOnTimeout verifies that custom OnTimeout is called for custom errors.
func TestIsTimeoutError_WithOnTimeout(t *testing.T) {
	t.Parallel()
	ctx := apptest.Ctx(t, fiber.New())

	called := false
	cfg := Config{
		Timeout: 100 * time.Millisecond,
		Errors:  []error{errCustomTimeout},
		OnTimeout: func(_ fiber.Ctx) error {
			called = true
			return errors.New("handled")
		},
	}

	// Test via full middleware to ensure OnTimeout is called
	handler := New(func(_ fiber.Ctx) error { return fmt.Errorf("wrap: %w", errCustomTimeout) }, cfg)

	err := handler(ctx)
	require.True(t, called)
	require.EqualError(t, err, "handled")
}

// TestTimeout_ImmediateReturn verifies that the middleware returns immediately on timeout
// without waiting for the handler to finish (using Abandon mechanism).
func TestTimeout_ImmediateReturn(t *testing.T) {
	t.Parallel()
	app := fiber.New()

	handlerStarted := make(chan struct{})
	handlerDone := make(chan struct{})

	app.Get("/immediate", New(func(_ fiber.Ctx) error {
		close(handlerStarted)
		// Handler takes 500ms but middleware should return after 20ms
		time.Sleep(500 * time.Millisecond)
		close(handlerDone)
		return nil
	}, Config{Timeout: 20 * time.Millisecond}))

	req := httptest.NewRequest(fiber.MethodGet, "/immediate", http.NoBody)
	start := time.Now()
	resp, err := app.Test(req)
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)
	// Middleware should return immediately after timeout, not wait 500ms
	require.Less(t, elapsed, 100*time.Millisecond, "middleware should return immediately on timeout")

	// Wait for handler to verify it was abandoned properly
	<-handlerStarted
	select {
	case <-handlerDone:
		// Handler finished - cleanup goroutine should have released context
	case <-time.After(1 * time.Second):
		t.Log("Handler still running (expected for abandoned context)")
	}
}

// TestTimeout_PanicAfterTimeout ensures panics after a timeout are handled.
func TestTimeout_PanicAfterTimeout(t *testing.T) {
	t.Parallel()
	app := fiber.New()

	panicDone := make(chan struct{})
	app.Get("/panic-after-timeout", New(func(c fiber.Ctx) error {
		<-c.Context().Done()
		defer close(panicDone)
		panic("panic after timeout")
	}, Config{Timeout: 20 * time.Millisecond}))

	// With immediate return, we get 503 (not 500) because panic happens after middleware returned
	apptest.Expect(t, app, "/panic-after-timeout", fiber.StatusServiceUnavailable)

	// Wait for panic to occur and be handled by cleanup goroutine
	select {
	case <-panicDone:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("panic did not occur")
	}
}

// TestTimeout_ContextCleanup verifies that the context is properly released
// after the handler finishes (even after timeout).
func TestTimeout_ContextCleanup(t *testing.T) {
	t.Parallel()
	app := fiber.New()

	handlerDone := make(chan struct{})
	app.Get("/cleanup", New(func(c fiber.Ctx) error {
		defer close(handlerDone)
		<-c.Context().Done()
		// Small delay to simulate cleanup
		time.Sleep(50 * time.Millisecond)
		return nil
	}, Config{Timeout: 20 * time.Millisecond}))

	apptest.Expect(t, app, "/cleanup", fiber.StatusServiceUnavailable)

	// Wait for handler to finish - cleanup goroutine should release context
	select {
	case <-handlerDone:
		// Give cleanup goroutine time to run
		time.Sleep(20 * time.Millisecond)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("handler did not finish")
	}
}

// TestTimeout_AbandonMechanism verifies the Abandon mechanism works correctly.
func TestTimeout_AbandonMechanism(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	ctx := app.AcquireCtx(&fasthttp.RequestCtx{})
	t.Cleanup(ctx.ForceRelease)

	// Initially not abandoned
	require.False(t, ctx.IsAbandoned())

	// Abandon it
	ctx.Abandon()
	require.True(t, ctx.IsAbandoned())

	// ReleaseCtx should be a no-op when abandoned
	app.ReleaseCtx(ctx)
	require.True(t, ctx.IsAbandoned(), "ReleaseCtx should not release abandoned context")

	// Note: We intentionally do NOT test ForceRelease here.
	// In the timeout middleware, abandoned contexts are NOT released back to the pool
	// to avoid race conditions with requestHandler. This is the same approach
	// fasthttp uses for timed-out RequestCtx objects.
}

// TestTimeout_AbandonedCtxReclaimed verifies the fix for #4359: a timed-out
// request's context is NOT reclaimed while its handler is still running, and IS
// returned to the pool once the handler finishes (proven by IsAbandoned flipping
// back to false, which only ForceRelease does).
func TestTimeout_AbandonedCtxReclaimed(t *testing.T) {
	t.Parallel()
	app := fiber.New()

	ctxCh := make(chan fiber.Ctx, 1)
	release := make(chan struct{})
	app.Get("/reclaim", New(func(c fiber.Ctx) error {
		ctxCh <- c
		// Keep "running" past the timeout until the test releases us, simulating
		// a slow upstream.
		<-release
		return nil
	}, Config{Timeout: 20 * time.Millisecond}))

	apptest.Expect(t, app, "/reclaim", fiber.StatusServiceUnavailable)

	var c fiber.Ctx
	select {
	case c = <-ctxCh:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}

	// Handler is still running -> the abandoned context must NOT be reclaimed yet.
	require.True(t, c.IsAbandoned(), "context reclaimed while handler still running")
	time.Sleep(30 * time.Millisecond)
	require.True(t, c.IsAbandoned(), "context reclaimed while handler still running")

	// Let the handler finish; the context must now be reclaimed (pooled).
	close(release)
	require.Eventually(t, func() bool {
		return !c.IsAbandoned()
	}, time.Second, 5*time.Millisecond, "abandoned context was not reclaimed after handler finished")
}

// TestTimeout_PanicAfterTimeoutReclaimed verifies that the panic-after-timeout
// path also reclaims the abandoned context once the handler goroutine unwinds.
func TestTimeout_PanicAfterTimeoutReclaimed(t *testing.T) {
	t.Parallel()
	app := fiber.New()

	ctxCh := make(chan fiber.Ctx, 1)
	release := make(chan struct{})
	app.Get("/panic-reclaim", New(func(c fiber.Ctx) error {
		ctxCh <- c
		<-release
		panic("panic after timeout")
	}, Config{Timeout: 20 * time.Millisecond}))

	apptest.Expect(t, app, "/panic-reclaim", fiber.StatusServiceUnavailable)

	var c fiber.Ctx
	select {
	case c = <-ctxCh:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}

	close(release)
	require.Eventually(t, func() bool {
		return !c.IsAbandoned()
	}, time.Second, 5*time.Millisecond, "abandoned context was not reclaimed after handler panicked")
}

// TestTimeout_AbandonWithoutReclaimNotPooled guards the SSE-style use of
// Abandon(): a context that is abandoned but never armed for reclamation stays
// out of the pool when ReleaseCtx is called.
func TestTimeout_AbandonWithoutReclaimNotPooled(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	ctx := app.AcquireCtx(&fasthttp.RequestCtx{})
	t.Cleanup(ctx.ForceRelease)

	ctx.Abandon()
	app.ReleaseCtx(ctx)
	require.True(t, ctx.IsAbandoned(), "abandoned, un-armed context must not be auto-reclaimed")
}

type neverReturningReader struct{}

func (neverReturningReader) Read(_ []byte) (int, error) { select {} }

// Test_Timeout_ResponseUnwritten covers the OnTimeout fallback's decision on its
// own. Driving it through a served request would mean reading the response while
// the timed-out handler still writes to it, which is a race this middleware has
// either way: the handler is abandoned by design and keeps running.
func Test_Timeout_ResponseUnwritten(t *testing.T) {
	t.Parallel()

	tests := []struct {
		setup func(resp *fasthttp.Response)
		name  string
		want  bool
	}{
		{name: "nothing written", setup: func(_ *fasthttp.Response) {}, want: true},
		{
			name:  "stream with a payload",
			setup: func(resp *fasthttp.Response) { resp.SetBodyStream(strings.NewReader("streamed"), -1) },
			want:  true,
		},
		{
			name:  "stream that yields nothing",
			setup: func(resp *fasthttp.Response) { resp.SetBodyStream(strings.NewReader(""), -1) },
			want:  true,
		},
		{
			name:  "reader that never returns",
			setup: func(resp *fasthttp.Response) { resp.SetBodyStream(neverReturningReader{}, -1) },
			want:  true,
		},
		{
			name:  "buffered body the handler already wrote",
			setup: func(resp *fasthttp.Response) { resp.SetBodyString("handler-body") },
			want:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			resp := &fasthttp.Response{}
			tc.setup(resp)
			require.Equal(t, tc.want, timeoutResponseUnwritten(resp))
		})
	}
}

func Test_Timeout_OnTimeout_ResetsBodyStream(t *testing.T) {
	t.Parallel()

	resp := &fasthttp.Response{}
	resp.SetBodyStream(neverReturningReader{}, -1)

	require.True(t, timeoutResponseUnwritten(resp))

	// What handleTimeout does once the check passes. ResetBody closes the stream
	// instead of draining it, so this returns rather than blocking on the reader.
	resp.ResetBody()
	resp.SetStatusCode(fiber.StatusServiceUnavailable)
	resp.SetBodyString(fiber.ErrServiceUnavailable.Message)

	require.False(t, resp.IsBodyStream())
	require.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode())
	require.Equal(t, fiber.ErrServiceUnavailable.Message, string(resp.Body()))
}

func Test_Timeout_DefaultReturnsErrServiceUnavailable(t *testing.T) {
	t.Parallel()
	app := fiber.New()

	seen := make(chan error, 1)
	app.Use(func(c fiber.Ctx) error {
		err := c.Next()
		seen <- err
		return err
	})
	// Hold the handler past the deadline, so the middleware always selects the
	// timeout rather than racing it against the handler returning.
	release := make(chan struct{})
	app.Get("/slow", New(func(c fiber.Ctx) error {
		<-c.Context().Done()
		<-release
		return nil
	}, Config{Timeout: 20 * time.Millisecond}))

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/slow", http.NoBody))
	close(release)
	require.NoError(t, err)
	require.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)

	select {
	case err := <-seen:
		require.ErrorIs(t, err, fiber.ErrServiceUnavailable)
	case <-time.After(time.Second):
		t.Fatal("outer middleware never observed the result")
	}
}

func Test_Timeout_ErrorHandlerSkippedAfterTimeout(t *testing.T) {
	t.Parallel()

	var errorHandlerCalls atomic.Int32
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c fiber.Ctx, err error) error {
			errorHandlerCalls.Add(1)
			return c.Status(fiber.StatusTeapot).SendString(err.Error())
		},
	})

	seen := make(chan error, 1)
	app.Use(func(c fiber.Ctx) error {
		err := c.Next()
		seen <- err
		return err
	})
	release := make(chan struct{})
	app.Get("/partial", New(func(c fiber.Ctx) error {
		if err := c.SendString("partial output"); err != nil {
			return err
		}
		<-c.Context().Done()
		<-release
		return c.Context().Err()
	}, Config{Timeout: 20 * time.Millisecond}))

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/partial", http.NoBody))
	close(release)
	require.NoError(t, err)
	require.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)
	require.Equal(t, fiber.ErrServiceUnavailable.Message, apptest.Body(t, resp))

	select {
	case err := <-seen:
		require.ErrorIs(t, err, fiber.ErrServiceUnavailable)
	case <-time.After(time.Second):
		t.Fatal("outer middleware never observed the result")
	}
	require.Equal(t, int32(0), errorHandlerCalls.Load())
}

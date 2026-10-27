package sse

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/internal/apptest"
	"github.com/stretchr/testify/require"
)

type panicStringer struct{}

func (*panicStringer) String() string {
	panic("String must not be called for SSE data")
}

// sseTestFrame writes event with writeEvent and returns what reached the writer.
func sseTestFrame(t *testing.T, event Event) string {
	t.Helper()

	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	require.NoError(t, writeEvent(w, event))
	require.NoError(t, w.Flush())
	return buf.String()
}

// sseTestStream returns a stream that writes to the returned buffer and is
// closed when the test ends.
func sseTestStream(t *testing.T) (*Stream, *bytes.Buffer) {
	t.Helper()

	var buf bytes.Buffer
	stream := newStream(context.Background(), bufio.NewWriter(&buf), "")
	t.Cleanup(stream.closeStream)
	return stream, &buf
}

// sseTestOnClose serves handler behind an OnClose hook and returns the error
// the hook received. The test fails if the hook is not called.
func sseTestOnClose(t *testing.T, handler func(fiber.Ctx, *Stream) error) error {
	t.Helper()

	closed := make(chan error, 1)

	app := fiber.New()
	app.Get("/events", New(Config{
		DisableHeartbeat: true,
		Handler:          handler,
		OnClose:          func(_ fiber.Ctx, err error) { closed <- err },
	}))

	apptest.Expect(t, app, "/events", fiber.StatusOK)
	select {
	case err := <-closed:
		return err
	case <-time.After(time.Second):
		t.Fatal("OnClose was not called")
		return nil
	}
}

// Test_SSE_EventFrames covers the frame writeEvent writes for an event.
func Test_SSE_EventFrames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		want  string
		event Event
	}{
		{
			name:  "writes frame",
			event: Event{ID: " 42 ", Name: "update", Data: "one\r\ntwo", Retry: 2500 * time.Millisecond},
			want:  "id: 42\nevent: update\nretry: 2500\ndata: one\ndata: two\n\n",
		},
		{name: "omits whitespace-only fields", event: Event{ID: "   ", Name: "   ", Data: "ok"}, want: "data: ok\n\n"},
		// panicStringer.String panics: a typed nil must be JSON encoded, never stringified.
		{name: "JSON encodes typed nil stringer", event: Event{Data: (*panicStringer)(nil)}, want: "data: null\n\n"},
		{name: "omits data for untyped nil", event: Event{ID: "42"}, want: "id: 42\n\n"},
		{name: "writes raw JSON data", event: Event{Data: json.RawMessage(`{"hello":"world"}`)}, want: "data: {\"hello\":\"world\"}\n\n"},
		{name: "writes byte slice data", event: Event{Data: []byte("hello\nworld")}, want: "data: hello\ndata: world\n\n"},
		{name: "preserves trailing data newline", event: Event{Data: "hello\n"}, want: "data: hello\ndata: \n\n"},
		{name: "preserves intentional blank data line", event: Event{Data: "hello\n\n"}, want: "data: hello\ndata: \ndata: \n\n"},
		{
			name:  "JSON encodes data",
			event: Event{Name: "message", Data: map[string]string{"hello": "world"}},
			want:  "event: message\ndata: {\"hello\":\"world\"}\n\n",
		},
		{
			name: "sub-millisecond retry rounds up", event: Event{Data: "x", Retry: 500 * time.Microsecond},
			want: "retry: 1\ndata: x\n\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, sseTestFrame(t, tc.event))
		})
	}
}

func Test_SSE_EventRejectsFieldInjection(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)

	require.ErrorIs(t, writeEvent(w, Event{ID: "42\nretry: 1", Data: "ignored"}), errInvalidField)
}

func Test_SSE_NewUsesAppJSONEncoder(t *testing.T) {
	t.Parallel()
	app := fiber.New(fiber.Config{
		JSONEncoder: func(any) ([]byte, error) { return []byte(`{"encoded":"custom"}`), nil },
	})
	app.Get("/events", New(Config{
		DisableHeartbeat: true,
		Handler: func(_ fiber.Ctx, stream *Stream) error {
			return stream.Event(Event{Data: map[string]string{"hello": "world"}})
		},
	}))

	apptest.ExpectBody(t, app, "/events", fiber.StatusOK, "data: {\"encoded\":\"custom\"}\n\n")
}

func Test_SSE_EventDoesNotWritePartialFrameWhenDataMarshalFails(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)

	require.Error(t, writeEvent(w, Event{ID: "42", Name: "broken", Data: func() {}}))
	require.NoError(t, w.Flush())

	require.Empty(t, buf.String())
}

func Test_SSE_EventReturnsWriterError(t *testing.T) {
	t.Parallel()
	writeErr := errors.New("write failed")
	w := bufio.NewWriterSize(errWriter{err: writeErr}, 1)

	require.ErrorIs(t, writeEvent(w, Event{Data: "hello"}), writeErr)
}

func Test_SSE_CommentSanitizesLines(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)

	require.NoError(t, writeComment(w, " first\r\nsecond "))
	require.NoError(t, w.Flush())

	require.Equal(t, ": first\n: second\n\n", buf.String())
}

func Test_SSE_CommentReturnsWriterError(t *testing.T) {
	t.Parallel()
	writeErr := errors.New("write failed")
	w := bufio.NewWriterSize(errWriter{err: writeErr}, 1)

	require.ErrorIs(t, writeComment(w, ""), writeErr)
}

func Test_SSE_NewWritesHeadersAndEvents(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	var capturedLastEventID string
	app.Get("/events", New(Config{
		Retry:            time.Second,
		DisableHeartbeat: true,
		Handler: func(_ fiber.Ctx, stream *Stream) error {
			capturedLastEventID = stream.LastEventID()
			return stream.Event(Event{Name: "ready", Data: "ok"})
		},
	}))

	req := httptest.NewRequest(fiber.MethodGet, "/events", http.NoBody)
	req.Header.Set("Last-Event-ID", "last-1")
	resp := apptest.Do(t, app, req)
	require.Equal(t, "last-1", capturedLastEventID)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	require.Equal(t, fiber.MIMETextEventStream, resp.Header.Get(fiber.HeaderContentType))
	require.Equal(t, "no-cache", resp.Header.Get(fiber.HeaderCacheControl))
	require.Equal(t, "keep-alive", resp.Header.Get(fiber.HeaderConnection))
	require.Equal(t, "no", resp.Header.Get("X-Accel-Buffering"))

	require.Equal(t, "retry: 1000\n\nevent: ready\ndata: ok\n\n", apptest.Body(t, resp))
}

func Test_SSE_NewWritesHeartbeat(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	app.Get("/events", New(Config{
		HeartbeatInterval: 5 * time.Millisecond,
		Handler: func(_ fiber.Ctx, stream *Stream) error {
			select {
			case <-time.After(150 * time.Millisecond):
				return nil
			case <-stream.Done():
				return stream.Err()
			}
		},
	}))

	resp := apptest.Do(t, app, "/events")
	body := apptest.Body(t, resp)
	require.Contains(t, body, ":\n\n")
}

func Test_SSE_OnCloseReceivesNilAfterNormalClose(t *testing.T) {
	t.Parallel()
	require.NoError(t, sseTestOnClose(t, func(_ fiber.Ctx, stream *Stream) error { return stream.Event(Event{Data: "ok"}) }))
}

func Test_SSE_StreamComment(t *testing.T) {
	t.Parallel()
	stream, buf := sseTestStream(t)

	require.NoError(t, stream.Comment("hello"))
	require.Equal(t, ": hello\n\n", buf.String())
}

func Test_SSE_StreamCommentReturnsWriterError(t *testing.T) {
	t.Parallel()
	writeErr := errors.New("write failed")
	stream := newStream(context.Background(), bufio.NewWriterSize(errWriter{err: writeErr}, 1), "")

	require.ErrorIs(t, stream.Comment("hello"), writeErr)
	require.ErrorIs(t, stream.Err(), writeErr)
}

func Test_SSE_StreamRetryIgnoresNonPositiveDuration(t *testing.T) {
	t.Parallel()
	stream, buf := sseTestStream(t)

	require.NoError(t, stream.Retry(0))
	require.NoError(t, stream.Retry(-time.Second))
	require.Empty(t, buf.String())
}

func Test_SSE_StreamRetryReturnsWriterError(t *testing.T) {
	t.Parallel()
	writeErr := errors.New("write failed")
	stream := newStream(context.Background(), bufio.NewWriterSize(errWriter{err: writeErr}, 1), "")

	require.ErrorIs(t, stream.Retry(time.Second), writeErr)
	require.ErrorIs(t, stream.Err(), writeErr)
}

func Test_SSE_StreamConcurrentWrites(t *testing.T) {
	t.Parallel()
	stream, buf := sseTestStream(t)

	const writers = 16
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() { errs <- stream.Event(Event{Name: "message", Data: i}) })
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	require.Equal(t, writers, strings.Count(buf.String(), "event: message\n"))
	require.Equal(t, writers, strings.Count(buf.String(), "data: "))
}

func Test_SSE_StreamContextCanceledOnClose(t *testing.T) {
	t.Parallel()
	stream, _ := sseTestStream(t)
	stream.closeStream()

	select {
	case <-stream.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("stream context was not canceled")
	}
}

func Test_SSE_NewStreamUsesBackgroundContextWhenNil(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	stream := newStream(nil, bufio.NewWriter(&buf), "") //nolint:staticcheck // Covers the nil fallback branch in newStream.
	defer stream.closeStream()

	require.NotNil(t, stream.Context())
	require.NoError(t, stream.Context().Err())
}

func Test_SSE_StreamErrAfterNormalClose(t *testing.T) {
	t.Parallel()
	stream, _ := sseTestStream(t)
	stream.closeStream()

	require.NoError(t, stream.Err())
	require.ErrorIs(t, stream.Event(Event{Data: "late"}), errStreamClosed)
}

func Test_SSE_StreamReturnsLatchedError(t *testing.T) {
	t.Parallel()
	writeErr := errors.New("write failed")
	stream := newStream(context.Background(), bufio.NewWriter(errWriter{err: writeErr}), "")

	require.ErrorIs(t, stream.Event(Event{Data: "hello"}), writeErr)
	require.ErrorIs(t, stream.Event(Event{Data: "again"}), writeErr)
}

func Test_SSE_StreamWriteError(t *testing.T) {
	t.Parallel()
	writeErr := errors.New("write failed")
	stream := newStream(context.Background(), bufio.NewWriter(errWriter{err: writeErr}), "")

	require.ErrorIs(t, stream.Event(Event{Data: "hello"}), writeErr)
	require.ErrorIs(t, stream.Err(), writeErr)
	select {
	case <-stream.Done():
	case <-time.After(time.Second):
		t.Fatal("stream was not closed after write error")
	}
}

func Test_SSE_InterruptedClientClosesStream(t *testing.T) {
	t.Parallel()
	const (
		testTimeout                  = time.Second
		disconnectObservationTimeout = 2 * time.Second
	)

	app := fiber.New()
	handlerDone := make(chan error, 1)
	onCloseDone := make(chan error, 1)

	app.Get("/events", New(Config{
		HeartbeatInterval: 100 * time.Millisecond,
		Handler: func(_ fiber.Ctx, stream *Stream) error {
			if err := stream.Event(Event{Data: "ready"}); err != nil {
				handlerDone <- err
				return err
			}

			<-stream.Done()
			err := stream.Err()
			handlerDone <- err
			return err
		},
		OnClose: func(_ fiber.Ctx, err error) { onCloseDone <- err },
	}))

	resp := apptest.Do(t, app, "/events", fiber.TestConfig{FailOnTimeout: false, Timeout: testTimeout})
	require.Equal(t, fiber.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.True(t, err == nil || errors.Is(err, io.ErrUnexpectedEOF))
	require.Contains(t, string(body), "data: ready\n\n")

	select {
	case err := <-handlerDone:
		require.Error(t, err)
	case <-time.After(disconnectObservationTimeout):
		t.Fatal("handler did not observe the interrupted client")
	}

	select {
	case err := <-onCloseDone:
		require.Error(t, err)
	case <-time.After(disconnectObservationTimeout):
		t.Fatal("OnClose was not called after the interrupted client")
	}
}

func Test_SSE_HandlerErrorCallsOnClose(t *testing.T) {
	t.Parallel()
	handlerErr := errors.New("boom")
	require.ErrorIs(t, sseTestOnClose(t, func(fiber.Ctx, *Stream) error { return handlerErr }), handlerErr)
}

func Test_SSE_HandlerPanicCallsOnClose(t *testing.T) {
	t.Parallel()
	require.EqualError(t, sseTestOnClose(t, func(fiber.Ctx, *Stream) error { panic("boom") }), "sse: handler panic: boom")
}

func Test_SSE_NewPanicsWithoutHandler(t *testing.T) {
	t.Parallel()
	require.PanicsWithValue(t, "sse: Handler must not be nil", func() { New() })
}

func Test_SSE_StopHeartbeatIsIdempotent(t *testing.T) {
	t.Parallel()
	stream, _ := sseTestStream(t)

	stop := stream.startHeartbeat(time.Hour)
	require.NotNil(t, stop)
	require.NotPanics(t, stop)
	require.NotPanics(t, stop)
}

func Test_SSE_StartHeartbeatReturnsNilForNonPositiveInterval(t *testing.T) {
	t.Parallel()
	stream, _ := sseTestStream(t)

	require.Nil(t, stream.startHeartbeat(0))
	require.Nil(t, stream.startHeartbeat(-time.Second))
}

type errWriter struct {
	err error
}

func (w errWriter) Write([]byte) (int, error) {
	return 0, fmt.Errorf("test writer: %w", w.err)
}

func Benchmark_sanitizeField(b *testing.B) {
	inputs := []string{
		"tick",                                 // short: scalar path
		"6ba7b810-9dad-11d1-80b4-00c04fd430c8", // typical event ID
		"a longer event name for dashboards",
	}
	var s string
	var err error
	b.ReportAllocs()
	for b.Loop() {
		for _, in := range inputs {
			s, err = sanitizeField(in)
		}
	}
	_, _ = s, err
}

func Test_SSE_RetryMillisecondsStaysPositive(t *testing.T) {
	t.Parallel()
	// Rounding must not overflow into a negative "retry:" value.
	require.Positive(t, retryMilliseconds(time.Duration(math.MaxInt64)))
	require.Positive(t, retryMilliseconds(time.Duration(math.MaxInt64)-1))
	require.Equal(t, int64(1), retryMilliseconds(time.Nanosecond))
	require.Equal(t, int64(2), retryMilliseconds(time.Millisecond+time.Nanosecond))
}

//nolint:depguard // Because we test logging :D
package logger

import (
	"bufio"
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/valyala/bytebufferpool"
	"github.com/valyala/fasthttp"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/internal/apptest"
	fiberlog "github.com/gofiber/fiber/v3/log"
)

const (
	pathFooBar = "/?foo=bar"
	httpProto  = "HTTP/1.1"
)

func benchmarkSetup(b *testing.B, app *fiber.App, uri string) {
	b.Helper()

	h := app.Handler()

	fctx := &fasthttp.RequestCtx{}
	fctx.Request.Header.SetMethod(fiber.MethodGet)
	fctx.Request.SetRequestURI(uri)

	b.ReportAllocs()

	for b.Loop() {
		h(fctx)
	}
}

func benchmarkSetupParallel(b *testing.B, app *fiber.App, path string) {
	b.Helper()

	handler := app.Handler()

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		fctx := &fasthttp.RequestCtx{}
		fctx.Request.Header.SetMethod(fiber.MethodGet)
		fctx.Request.SetRequestURI(path)

		for pb.Next() {
			handler(fctx)
		}
	})
}

// loggerTestBuf returns a pooled buffer that goes back to the pool once the
// test and all of its subtests have finished.
func loggerTestBuf(tb testing.TB) *bytebufferpool.ByteBuffer {
	tb.Helper()

	buf := bytebufferpool.Get()
	tb.Cleanup(func() { bytebufferpool.Put(buf) })
	return buf
}

// loggerTestApp returns an app whose first middleware is the logger configured
// by cfg, plus the pooled buffer it writes to: the copy of cfg the logger gets
// has its Stream replaced by that buffer. appCfg, when given, configures the
// app itself.
func loggerTestApp(t *testing.T, cfg *Config, appCfg ...fiber.Config) (*fiber.App, *bytebufferpool.ByteBuffer) {
	t.Helper()

	buf := loggerTestBuf(t)
	logCfg := *cfg
	logCfg.Stream = buf
	app := fiber.New(appCfg...)
	app.Use(New(logCfg))
	return app, buf
}

// loggerTestTag returns a LogFunc that writes s.
func loggerTestTag(s string) LogFunc {
	return func(output Buffer, _ fiber.Ctx, _ *Data, _ string) (int, error) {
		return output.WriteString(s)
	}
}

// loggerTestLog sends one GET / that its handler answers with 200 through a
// logger configured by cfg and returns what the logger wrote.
func loggerTestLog(t *testing.T, cfg *Config) string {
	t.Helper()

	app, buf := loggerTestApp(t, cfg)
	app.Get("/", func(c fiber.Ctx) error { return c.SendString("ok") })

	apptest.Expect(t, app, "/", fiber.StatusOK)
	return buf.String()
}

// Test_Logger_Output pins the exact line the logger writes for one request,
// one scenario per row. A row without a handler registers no route, so its
// request is a 404.
func Test_Logger_Output(t *testing.T) {
	t.Parallel()

	const customTag = "it is a custom tag"
	templateErr := errors.New("template failure")
	colors := fiber.DefaultColors
	hello := func(c fiber.Ctx) error { return c.SendString("Hello fiber!") }
	testHeader := apptest.WithHeader("test", "Hello fiber!")

	for _, tc := range []struct {
		handler fiber.Handler
		name    string
		target  string // defaults to "/"
		want    string
		opts    []apptest.Option
		cfg     Config
		status  int
	}{
		{
			name:    "error tag",
			cfg:     Config{Format: "${error}"},
			handler: func(_ fiber.Ctx) error { return errors.New("some random error") },
			status:  fiber.StatusInternalServerError,
			want:    "some random error",
		},
		{
			name:   "queryParams tag",
			cfg:    Config{Format: "${queryParams}"},
			target: "/?foo=bar&baz=moz",
			status: fiber.StatusNotFound,
			want:   "foo=bar&baz=moz",
		},
		{
			name:    "reqHeader tag",
			cfg:     Config{Format: "${reqHeader:test}"},
			handler: hello,
			opts:    []apptest.Option{testHeader},
			status:  fiber.StatusOK,
			want:    "Hello fiber!",
		},
		{
			// Formerly Test_ReqHeader_Header: identical to the row above, and
			// kept as a scenario of its own.
			name:    "reqHeader tag again",
			cfg:     Config{Format: "${reqHeader:test}"},
			handler: hello,
			opts:    []apptest.Option{testHeader},
			status:  fiber.StatusOK,
			want:    "Hello fiber!",
		},
		{
			name: "custom tag",
			cfg: Config{
				Format:     "${custom_tag}",
				CustomTags: map[string]LogFunc{"custom_tag": loggerTestTag(customTag)},
			},
			handler: hello,
			opts:    []apptest.Option{testHeader},
			status:  fiber.StatusOK,
			want:    customTag,
		},
		{
			name: "failing tag leaves its error in the line",
			cfg: Config{
				Format: "${fail}",
				CustomTags: map[string]LogFunc{
					"fail": func(_ Buffer, _ fiber.Ctx, _ *Data, _ string) (int, error) {
						return 0, templateErr
					},
				},
			},
			status: fiber.StatusNotFound,
			want:   templateErr.Error(),
		},
		{
			name:   "every tag",
			cfg:    Config{Format: "${pid}${reqHeaders}${referer}${scheme}${protocol}${ip}${ips}${host}${url}${ua}${body}${route}${black}${red}${green}${yellow}${blue}${magenta}${cyan}${white}${reset}${error}${reqHeader:test}${query:test}${form:test}${cookie:test}"},
			target: pathFooBar,
			status: fiber.StatusNotFound,
			want:   fmt.Sprintf("%dHost=example.comhttpHTTP/1.10.0.0.0example.com/?foo=bar/%s%s%s%s%s%s%s%s%sNot Found", os.Getpid(), colors.Black, colors.Red, colors.Green, colors.Yellow, colors.Blue, colors.Magenta, colors.Cyan, colors.White, colors.Reset),
		},
		{
			name:   "forced colors",
			cfg:    Config{Format: "${ip}${status}${method}${path}${error}\n", DisableColors: true, ForceColors: true},
			status: fiber.StatusNotFound,
			want:   fmt.Sprintf("0.0.0.0%s404%s%sGET%s/%sNot Found%s\n", colors.Yellow, colors.Reset, colors.Cyan, colors.Reset, colors.Red, colors.Reset),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app, buf := loggerTestApp(t, &tc.cfg)
			if tc.handler != nil {
				app.Get("/", tc.handler)
			}

			apptest.Expect(t, app, apptest.Req(fiber.MethodGet, cmp.Or(tc.target, "/"), tc.opts...), tc.status)
			require.Equal(t, tc.want, buf.String())
		})
	}
}

func Test_Logger_locals(t *testing.T) {
	t.Parallel()
	app, buf := loggerTestApp(t, &Config{Format: "${locals:demo}"})

	app.Get("/", func(c fiber.Ctx) error {
		c.Locals("demo", "johndoe")
		return c.SendStatus(fiber.StatusOK)
	})

	app.Get("/int", func(c fiber.Ctx) error {
		c.Locals("demo", 55)
		return c.SendStatus(fiber.StatusOK)
	})

	app.Get("/empty", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	apptest.Expect(t, app, "/", fiber.StatusOK)
	require.Equal(t, "johndoe", buf.String())

	buf.Reset()

	apptest.Expect(t, app, "/int", fiber.StatusOK)
	require.Equal(t, "55", buf.String())

	buf.Reset()

	apptest.Expect(t, app, "/empty", fiber.StatusOK)
	require.Empty(t, buf.String())
}

func Test_Logger_Next(t *testing.T) {
	t.Parallel()
	app := fiber.New()

	app.Use(New(Config{
		Next: func(_ fiber.Ctx) bool {
			return true
		},
	}))

	apptest.Expect(t, app, "/", fiber.StatusNotFound)
}

func Test_Logger_Done(t *testing.T) {
	t.Parallel()
	buf := bytes.NewBuffer(nil)
	app := fiber.New()

	app.Use(New(Config{
		Done: func(c fiber.Ctx, logString []byte) {
			if c.Response().StatusCode() == fiber.StatusOK {
				_, err := buf.Write(logString)
				require.NoError(t, err)
			}
		},
	})).Get("/logging", func(ctx fiber.Ctx) error {
		return ctx.SendStatus(fiber.StatusOK)
	})

	apptest.Expect(t, app, "/logging", fiber.StatusOK)
	require.Positive(t, buf.Len(), 0)
}

func Test_Logger_TimeUpdaterStopsOnDone(t *testing.T) {
	t.Parallel()

	var timestamp atomic.Value
	timestamp.Store(time.Now().Format(time.RFC3339Nano))

	done := make(chan struct{})
	cfg := Config{
		Format:           "${time}",
		TimeFormat:       time.RFC3339Nano,
		TimeInterval:     5 * time.Millisecond,
		timeZoneLocation: time.Local,
		TimeDone:         done,
	}

	stoppedCh := startTimestampUpdaterWithStop(&timestamp, &cfg)

	initial, ok := timestamp.Load().(string)
	require.True(t, ok)
	// Wait for a tick rather than assume one lands inside a fixed sleep: the
	// updater is a goroutine on a 5ms ticker, and a loaded machine ran the whole
	// sleep before scheduling it.
	require.Eventually(t, func() bool {
		updated, isString := timestamp.Load().(string)
		return isString && updated != initial
	}, time.Second, time.Millisecond, "timestamp was never updated")

	close(done)
	select {
	case <-stoppedCh:
	case <-time.After(time.Second):
		t.Fatal("timestamp updater did not stop")
	}
	stopped, ok := timestamp.Load().(string)
	require.True(t, ok)
	time.Sleep(20 * time.Millisecond)
	finalValue, ok := timestamp.Load().(string)
	require.True(t, ok)
	require.Equal(t, stopped, finalValue)
}

func Test_Logger_TimestampUpdater_StopsImmediatelyWithoutTimeTag(t *testing.T) {
	t.Parallel()

	var timestamp atomic.Value
	timestamp.Store(time.Now().Format(time.RFC3339Nano))

	stoppedCh := startTimestampUpdaterWithStop(&timestamp, &Config{
		Format: "${pid}",
	})

	select {
	case <-stoppedCh:
	case <-time.After(time.Second):
		t.Fatal("timestamp updater did not stop immediately")
	}
}

// Test_Logger_Filter tests the Filter functionality of the logger middleware.
// It verifies that logs are written or skipped based on the filter condition.
func Test_Logger_Filter(t *testing.T) {
	t.Parallel()

	t.Run("Test Not Found", func(t *testing.T) {
		t.Parallel()

		// Return true to skip logging for all requests != 404
		app, logOutput := loggerTestApp(t, &Config{
			Skip: func(c fiber.Ctx) bool {
				return c.Response().StatusCode() != fiber.StatusNotFound
			},
		})

		apptest.Expect(t, app, "/nonexistent", fiber.StatusNotFound)

		// Expect logs for the 404 request
		require.Contains(t, logOutput.String(), "404")
	})

	t.Run("Test OK", func(t *testing.T) {
		t.Parallel()

		// Return true to skip logging for all requests == 200
		app, logOutput := loggerTestApp(t, &Config{
			Skip: func(c fiber.Ctx) bool {
				return c.Response().StatusCode() == fiber.StatusOK
			},
		})

		app.Get("/", func(c fiber.Ctx) error {
			return c.SendStatus(fiber.StatusOK)
		})

		apptest.Expect(t, app, "/", fiber.StatusOK)

		// We skip logging for status == 200, so "200" should not appear
		require.NotContains(t, logOutput.String(), "200")
	})

	t.Run("Always Skip", func(t *testing.T) {
		t.Parallel()

		// Filter always returns true => skip all logs
		app, logOutput := loggerTestApp(t, &Config{
			Skip: func(_ fiber.Ctx) bool {
				return true // always skip
			},
		})

		app.Get("/something", func(c fiber.Ctx) error {
			return c.Status(fiber.StatusTeapot).SendString("I'm a teapot")
		})

		apptest.Do(t, app, "/something")

		// Expect NO logs
		require.Empty(t, logOutput.String())
	})

	t.Run("Never Skip", func(t *testing.T) {
		t.Parallel()

		// Filter always returns false => never skip logs
		app, logOutput := loggerTestApp(t, &Config{
			Skip: func(_ fiber.Ctx) bool {
				return false // never skip
			},
		})

		app.Get("/always", func(c fiber.Ctx) error {
			return c.Status(fiber.StatusTeapot).SendString("Teapot again")
		})

		apptest.Do(t, app, "/always")

		// Expect some logging - check any substring
		require.Contains(t, logOutput.String(), strconv.Itoa(fiber.StatusTeapot))
	})

	t.Run("Skip /healthz", func(t *testing.T) {
		t.Parallel()

		// Filter returns true (skip logs) if the request path is /healthz
		app, logOutput := loggerTestApp(t, &Config{
			Skip: func(c fiber.Ctx) bool {
				return c.Path() == "/healthz"
			},
		})

		// Normal route
		app.Get("/", func(c fiber.Ctx) error {
			return c.SendString("Hello World!")
		})
		// Health route
		app.Get("/healthz", func(c fiber.Ctx) error {
			return c.SendString("OK")
		})

		// Request to "/" -> should be logged
		apptest.Do(t, app, "/")
		require.Contains(t, logOutput.String(), "200")

		// Reset output buffer
		logOutput.Reset()

		// Request to "/healthz" -> should be skipped
		apptest.Do(t, app, "/healthz")
		require.Empty(t, logOutput.String())
	})
}

func Test_Logger_ErrorTimeZone(t *testing.T) {
	t.Parallel()
	app := fiber.New()

	app.Use(New(Config{
		TimeZone: "invalid",
	}))

	apptest.Expect(t, app, "/", fiber.StatusNotFound)
}

func Test_Logger_LoggerToWriter(t *testing.T) {
	app := fiber.New()

	buf := loggerTestBuf(t)

	logger := fiberlog.DefaultLogger[*log.Logger]()
	stdlogger := logger.Logger()
	stdlogger.SetFlags(0)
	logger.SetOutput(buf)

	testCases := []struct {
		levelStr string
		level    fiberlog.Level
	}{
		{
			level:    fiberlog.LevelTrace,
			levelStr: "Trace",
		},
		{
			level:    fiberlog.LevelDebug,
			levelStr: "Debug",
		},
		{
			level:    fiberlog.LevelInfo,
			levelStr: "Info",
		},
		{
			level:    fiberlog.LevelWarn,
			levelStr: "Warn",
		},
		{
			level:    fiberlog.LevelError,
			levelStr: "Error",
		},
	}

	for _, tc := range testCases {
		level := strconv.Itoa(int(tc.level))
		t.Run(level, func(t *testing.T) {
			buf.Reset()

			app.Use("/"+level, New(Config{
				Format: "${error}",
				Stream: LoggerToWriter(logger, tc.
					level),
			}))

			app.Get("/"+level, func(_ fiber.Ctx) error {
				return errors.New("some random error")
			})

			apptest.Expect(t, app, "/"+level, fiber.StatusInternalServerError)
			require.Equal(t, "["+tc.levelStr+"] some random error\n", buf.String())
		})

		require.Panics(t, func() {
			LoggerToWriter(logger, fiberlog.LevelPanic)
		})

		require.Panics(t, func() {
			LoggerToWriter(logger, fiberlog.LevelFatal)
		})

		require.Panics(t, func() {
			LoggerToWriter[any](nil, fiberlog.LevelFatal)
		})
	}
}

// loggerTestOutput is a Config.Stream that counts the writes it receives and,
// when err is set, rejects every one of them with it.
type loggerTestOutput struct {
	err    error
	writes int
}

func (o *loggerTestOutput) Write(b []byte) (int, error) {
	o.writes++
	if o.err != nil {
		return 0, o.err
	}
	return len(b), nil
}

// Test_Logger_StreamWrites counts the writes made to Config.Stream for one
// request. A stream that rejects the line is written the error text as well;
// one that takes it is written the line only.
func Test_Logger_StreamWrites(t *testing.T) {
	t.Parallel()

	errFakeOutput := errors.New("fake output")

	for _, tc := range []struct {
		err           error
		name          string
		writes        int
		disableColors bool
	}{
		{name: "error output without color", disableColors: true, err: errFakeOutput, writes: 2},
		{name: "error output", err: errFakeOutput, writes: 2},
		{name: "enable colors", writes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			out := &loggerTestOutput{err: tc.err}
			app := fiber.New()
			app.Use(New(Config{Stream: out, DisableColors: tc.disableColors}))

			apptest.Expect(t, app, "/", fiber.StatusNotFound)
			require.Equal(t, tc.writes, out.writes)
		})
	}
}

// Test_AppendIntPadded pins the right-align pad loop directly: statuses are
// almost always exactly width 3, so the end-to-end test below cannot
// exercise the padding (and net/http rejects 2-digit status lines).
func Test_AppendIntPadded(t *testing.T) {
	t.Parallel()
	buf := loggerTestBuf(t)

	testCases := []struct {
		expected string
		v        int
		width    int
	}{
		{"  5", 5, 3},
		{" 99", 99, 3},
		{"404", 404, 3},
		{"1000", 1000, 3},
		{" -1", -1, 3},
	}
	for _, tc := range testCases {
		buf.Reset()
		appendIntPadded(buf, tc.v, tc.width)
		require.Equal(t, tc.expected, buf.String(), "appendIntPadded(%d, %d)", tc.v, tc.width)
	}
}

// Test_Logger_DefaultFormat_WithoutColor pins the byte layout of the
// non-color default line, in particular the width-3 right-aligned status
// field that is appended digit-wise without an intermediate string.
func Test_Logger_DefaultFormat_WithoutColor(t *testing.T) {
	t.Parallel()
	app, buf := loggerTestApp(t, &Config{DisableColors: true})
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusNotFound)
	})

	apptest.Expect(t, app, "/", fiber.StatusNotFound)
	require.Contains(t, buf.String(), " | 404 | ", "status field must be width-3 with separators")
	require.Contains(t, buf.String(), " | GET     | ", "method field must be width-7, left aligned")
}

func Test_Logger_UnknownTagPanicsWithTypedError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		format   string
		wantTag  string
		wantPara string
	}{
		{name: "parametric tag", format: "${missing:value}", wantTag: "missing:value", wantPara: "value"},
		{name: "bare tag", format: "${missing}", wantTag: "missing"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			defer func() {
				r := recover()
				require.NotNil(t, r, "New must panic for unknown tag")

				err, ok := r.(error)
				require.True(t, ok, "panic value must be an error, got %T", r)
				require.ErrorIs(t, err, ErrUnknownTag)

				var typed *UnknownTagError
				require.ErrorAs(t, err, &typed)
				require.Equal(t, tt.wantTag, typed.Tag)
				require.Equal(t, tt.wantPara, typed.Param)
				require.EqualError(t, err, `logger: unknown template tag: "`+tt.wantTag+`"`)
			}()

			New(Config{Format: tt.format})
		})
	}
}

// Test_Logger_NamedFormats pins the layout of the exported format constants.
// The timestamp is matched, not compared, because it moves with the clock.
func Test_Logger_NamedFormats(t *testing.T) {
	t.Parallel()

	const (
		method    = fiber.MethodGet
		status    = fiber.StatusNotFound
		ip        = "0.0.0.0"
		bytesSent = 0
		referer   = "http://example.com"
		ua        = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/74.0.3729.169 Safari/537.36"
	)
	msg := fmt.Sprintf("%s %s responded with %d", method, pathFooBar, status)

	for _, tc := range []struct {
		name    string
		format  string
		pattern string
		opts    []apptest.Option
	}{
		{
			name:    "CLF",
			format:  CommonFormat,
			pattern: fmt.Sprintf(`0\.0\.0\.0 - - \[\d{2}:\d{2}:\d{2}\] "%s %s %s" %d %d`, method, regexp.QuoteMeta(pathFooBar), httpProto, status, bytesSent),
		},
		{
			name:    "combined CLF",
			format:  CombinedFormat,
			opts:    []apptest.Option{apptest.WithHeader("Referer", referer), apptest.WithHeader("User-Agent", ua)},
			pattern: fmt.Sprintf(`0\.0\.0\.0 - - \[\d{2}:\d{2}:\d{2}\] "%s %s %s" %d %d "%s" "%s"`, method, regexp.QuoteMeta(pathFooBar), httpProto, status, bytesSent, regexp.QuoteMeta(referer), regexp.QuoteMeta(ua)), //nolint:gocritic // double quoting for regex and string is not needed
		},
		{
			name:    "JSON",
			format:  JSONFormat,
			pattern: fmt.Sprintf(`\{"time":"\d{2}:\d{2}:\d{2}","ip":"%s","method":%q,"url":"%s","status":%d,"bytesSent":%d\}`, regexp.QuoteMeta(ip), method, regexp.QuoteMeta(pathFooBar), status, bytesSent), //nolint:gocritic // double quoting for regex and string is not needed
		},
		{
			name:    "ECS",
			format:  ECSFormat,
			pattern: fmt.Sprintf(`\{"@timestamp":"\d{2}:\d{2}:\d{2}","ecs":\{"version":"1.6.0"\},"client":\{"ip":"%s"\},"http":\{"request":\{"method":%q,"url":"%s","protocol":%q\},"response":\{"status_code":%d,"body":\{"bytes":%d\}\}\},"log":\{"level":"INFO","logger":"fiber"\},"message":"%s"\}`, regexp.QuoteMeta(ip), method, regexp.QuoteMeta(pathFooBar), httpProto, status, bytesSent, regexp.QuoteMeta(msg)), //nolint:gocritic // double quoting for regex and string is not needed
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app, buf := loggerTestApp(t, &Config{Format: tc.format})

			apptest.Expect(t, app, apptest.Req(method, pathFooBar, tc.opts...), status)
			require.Regexp(t, tc.pattern, buf.String())
		})
	}
}

func getLatencyTimeUnits() []struct {
	unit string
	div  time.Duration
} {
	// windows does not support µs sleep precision
	// https://github.com/golang/go/issues/29485
	if runtime.GOOS == "windows" {
		return []struct {
			unit string
			div  time.Duration
		}{
			{unit: "ms", div: time.Millisecond},
			{unit: "s", div: time.Second},
		}
	}
	return []struct {
		unit string
		div  time.Duration
	}{
		{unit: "µs", div: time.Microsecond},
		{unit: "ms", div: time.Millisecond},
		{unit: "s", div: time.Second},
	}
}

// Test_Logger_WithLatency checks that the latency is rendered in the unit that
// fits its magnitude, both through the ${latency} tag and in the default format.
// The latency is injected through LoggerFunc, so no request has to be slow.
func Test_Logger_WithLatency(t *testing.T) {
	for _, tc := range []struct {
		// latency extracts the rendered latency from what was logged.
		latency func(logged []byte) []byte
		name    string
		format  string
		timeout time.Duration
	}{
		{
			name:    "latency tag",
			format:  "${latency}",
			timeout: 3 * time.Second,
			latency: func(logged []byte) []byte { return logged },
		},
		{
			// The latency is the third column of the default line.
			name:    "default format",
			timeout: 2 * time.Second,
			latency: func(logged []byte) []byte { return bytes.Split(logged, []byte(" | "))[2] },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var latencyDuration time.Duration
			fixedStart := time.Unix(0, 0)
			app, buff := loggerTestApp(t, &Config{
				Format: tc.format,
				LoggerFunc: func(c fiber.Ctx, data *Data, cfg *Config) error {
					data.Start = fixedStart
					data.Stop = fixedStart.Add(latencyDuration)
					return defaultLoggerInstance(c, data, cfg)
				},
			})

			// Define a test route
			app.Get("/test", func(c fiber.Ctx) error {
				return c.SendStatus(fiber.StatusOK)
			})

			// Loop through each time unit and assert that the log output contains the expected latency value
			for _, tu := range getLatencyTimeUnits() {
				// Update the latency duration for the next iteration
				latencyDuration = tu.div

				// Create a new HTTP request to the test route
				resp := apptest.Do(t, app, "/test", fiber.TestConfig{
					Timeout:       tc.timeout,
					FailOnTimeout: true,
				})
				require.Equal(t, fiber.StatusOK, resp.StatusCode)

				// Assert that the latency value is in the current time unit
				latency := tc.latency(buff.Bytes())
				require.True(t, bytes.HasSuffix(latency, []byte(tu.unit)), "Expected latency to be in %s, got %s", tu.unit, latency)

				// Reset the buffer
				buff.Reset()
			}
		})
	}
}

func Test_Response_Body(t *testing.T) {
	t.Parallel()
	app, buf := loggerTestApp(t, &Config{Format: "${resBody}"})

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("Sample response body")
	})

	app.Post("/test", func(c fiber.Ctx) error {
		return c.Send([]byte("Post in test"))
	})

	apptest.Do(t, app, "/")

	expectedGetResponse := "Sample response body"
	require.Equal(t, expectedGetResponse, buf.String())

	buf.Reset() // Reset buffer to test POST
	apptest.Do(t, app, apptest.Req(fiber.MethodPost, "/test"))

	expectedPostResponse := "Post in test"
	require.Equal(t, expectedPostResponse, buf.String())
}

func Test_Request_Body(t *testing.T) {
	t.Parallel()
	app, buf := loggerTestApp(t, &Config{Format: "${bytesReceived} ${bytesSent} ${status}"})

	app.Post("/", func(c fiber.Ctx) error {
		c.Response().Header.SetContentLength(5)
		return c.SendString("World")
	})

	// Create a POST request with a body
	body := []byte("Hello")
	req := httptest.NewRequest(fiber.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")

	apptest.Do(t, app, req)
	require.Equal(t, "5 5 200", buf.String())
}

func Test_Logger_AppendUint(t *testing.T) {
	t.Parallel()
	app, buf := loggerTestApp(t, &Config{Format: "${bytesReceived} ${bytesSent} ${status}"})

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("hello")
	})

	app.Get("/content", func(c fiber.Ctx) error {
		c.Response().Header.SetContentLength(5)
		return c.SendString("hello")
	})

	apptest.Expect(t, app, "/", fiber.StatusOK)
	require.Equal(t, "-2 0 200", buf.String())

	buf.Reset()
	apptest.Expect(t, app, "/content", fiber.StatusOK)
	require.Equal(t, "-2 5 200", buf.String())
}

func Test_Logger_Data_Race(t *testing.T) {
	t.Parallel()
	app := fiber.New()

	app.Use(New(ConfigDefault))
	app.Use(New(Config{
		Format: "${time} | ${pid} | ${locals:requestid} | ${status} | ${latency} | ${method} | ${path}\n",
	}))

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("hello")
	})

	var (
		resp1, resp2 *http.Response
		err1, err2   error
	)
	wg := &sync.WaitGroup{}
	wg.Go(func() {
		resp1, err1 = app.Test(httptest.NewRequest(fiber.MethodGet, "/", http.NoBody))
	})
	resp2, err2 = app.Test(httptest.NewRequest(fiber.MethodGet, "/", http.NoBody))
	wg.Wait()

	require.NoError(t, err1)
	require.Equal(t, fiber.StatusOK, resp1.StatusCode)
	require.NoError(t, err2)
	require.Equal(t, fiber.StatusOK, resp2.StatusCode)
}

func Test_Logger_TimeUpdatesAfterInterval(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	app := fiber.New()
	app.Use(New(Config{
		Format:       "${time}",
		TimeFormat:   time.RFC3339Nano,
		TimeInterval: 10 * time.Millisecond,
		Stream:       &buf,
	}))
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusNoContent)
	})

	apptest.Expect(t, app, "/", fiber.StatusNoContent)
	first := buf.String()
	require.NotEmpty(t, first)

	var second string
	require.Eventually(t, func() bool {
		buf.Reset()

		apptest.Expect(t, app, "/", fiber.StatusNoContent)

		second = buf.String()
		return second != "" && second != first
	}, 200*time.Millisecond, 5*time.Millisecond)
}

func Test_Logger_SharedTimestampState(t *testing.T) {
	t.Parallel()

	loc := time.FixedZone("test/zone", 3600)
	first := sharedTimestamp(time.RFC3339, loc, 10*time.Millisecond)
	second := sharedTimestamp(time.RFC3339, loc, 10*time.Millisecond)
	third := sharedTimestamp(time.RFC3339Nano, loc, 10*time.Millisecond)

	require.Same(t, first, second)
	require.NotSame(t, first, third)
	loaded, ok := first.Load().(string)
	require.True(t, ok)
	require.NotEmpty(t, loaded)
}

func Test_Response_Header(t *testing.T) {
	t.Parallel()
	buf := loggerTestBuf(t)

	app := fiber.New()

	app.Use(func(c fiber.Ctx) error {
		c.Response().Header.Set(fiber.HeaderXRequestID, "Hello fiber!")
		return c.Next()
	})
	app.Use(New(Config{
		Format: "${respHeader:X-Request-ID}",
		Stream: buf,
	}))
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("Hello fiber!")
	})

	apptest.Expect(t, app, "/", fiber.StatusOK)
	require.Equal(t, "Hello fiber!", buf.String())
}

func Test_Logger_RegisteredTag(t *testing.T) {
	t.Parallel()

	const tag = "registered-test-tag"

	require.NoError(t, RegisterTag(tag, loggerTestTag("registered")))

	app, buf := loggerTestApp(t, &Config{Format: "${" + tag + "}"})
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	apptest.Expect(t, app, "/", fiber.StatusOK)
	require.Equal(t, "registered", buf.String())
}

func Test_Logger_CustomTagOverridesRegisteredTag(t *testing.T) {
	t.Parallel()

	const tag = "registered-override-test-tag"

	require.NoError(t, RegisterTag(tag, loggerTestTag("registered")))

	app, buf := loggerTestApp(t, &Config{
		Format:     "${" + tag + "}",
		CustomTags: map[string]LogFunc{tag: loggerTestTag("override")},
	})
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	apptest.Expect(t, app, "/", fiber.StatusOK)
	require.Equal(t, "override", buf.String())
}

func Test_Logger_RegisterTagRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	require.ErrorIs(t, RegisterTag("", loggerTestTag("ignored")), ErrTagInvalid)
	require.ErrorIs(t, RegisterTag("missing", nil), ErrTagInvalid)
}

func Test_Logger_MustRegisterTagPanicsOnInvalidInput(t *testing.T) {
	t.Parallel()

	require.PanicsWithError(t, ErrTagInvalid.Error(), func() {
		MustRegisterTag("", loggerTestTag("ignored"))
	})
}

func Test_Logger_DataLegacyTemplateChains(t *testing.T) {
	t.Parallel()

	buf := loggerTestBuf(t)

	app := fiber.New()
	app.Use(New(Config{
		DisableColors: true,
		Format:        "${method} ${path}",
		LoggerFunc: func(c fiber.Ctx, data *Data, _ *Config) error {
			if len(data.TemplateChain) != len(data.LogFuncChain) {
				return fmt.Errorf("template/log func chain length mismatch: template=%d logfunc=%d", len(data.TemplateChain), len(data.LogFuncChain))
			}
			for i, logFunc := range data.LogFuncChain {
				switch {
				case logFunc == nil:
					if _, err := buf.Write(data.TemplateChain[i]); err != nil {
						return fmt.Errorf("write template chain: %w", err)
					}
				case data.TemplateChain[i] == nil:
					if _, err := logFunc(buf, c, data, ""); err != nil {
						return err
					}
				default:
					if _, err := logFunc(buf, c, data, string(data.TemplateChain[i])); err != nil {
						return err
					}
				}
			}
			return nil
		},
	}))
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("Hello fiber!")
	})

	apptest.Expect(t, app, "/", fiber.StatusOK)
	require.Equal(t, "GET /", buf.String())
}

func Test_Logger_ByteSent_Streaming(t *testing.T) {
	t.Parallel()
	app, buf := loggerTestApp(t, &Config{Format: "${bytesReceived} ${bytesSent} ${status}"})

	app.Get("/", func(c fiber.Ctx) error {
		c.Set("Connection", "keep-alive")
		c.Set("Transfer-Encoding", "chunked")
		c.RequestCtx().SetBodyStreamWriter(func(w *bufio.Writer) {
			var i int
			for {
				i++
				msg := fmt.Sprintf("%d - the time is %v", i, time.Now())
				fmt.Fprintf(w, "data: Message: %s\n\n", msg)
				err := w.Flush()
				if err != nil {
					break
				}
				if i == 10 {
					break
				}
			}
		})
		return nil
	})

	apptest.Expect(t, app, "/", fiber.StatusOK)

	// -2 means identity, -1 means chunked, 200 status
	require.Equal(t, "-2 -1 200", buf.String())
}

func Benchmark_Logger(b *testing.B) {
	b.Run("NoMiddleware", func(bb *testing.B) {
		app := fiber.New()
		app.Get("/", func(c fiber.Ctx) error {
			return c.SendString("Hello, World!")
		})
		benchmarkSetup(bb, app, "/")
	})

	b.Run("WithBytesAndStatus", func(bb *testing.B) {
		app := fiber.New()
		app.Use(New(Config{
			Format: "${bytesReceived} ${bytesSent} ${status}",
			Stream: io.Discard,
		}))
		app.Get("/", func(c fiber.Ctx) error {
			c.Set("test", "test")
			return c.SendString("Hello, World!")
		})
		benchmarkSetup(bb, app, "/")
	})

	b.Run("DefaultFormat", func(bb *testing.B) {
		app := fiber.New()
		app.Use(New(Config{
			Stream: io.Discard,
		}))
		app.Get("/", func(c fiber.Ctx) error {
			return c.SendString("Hello, World!")
		})
		benchmarkSetup(bb, app, "/")
	})

	b.Run("DefaultFormatDisableColors", func(bb *testing.B) {
		app := fiber.New()
		app.Use(New(Config{
			Stream:        io.Discard,
			DisableColors: true,
		}))
		app.Get("/", func(c fiber.Ctx) error {
			return c.SendString("Hello, World!")
		})
		benchmarkSetup(bb, app, "/")
	})

	b.Run("DefaultFormatForceColors", func(bb *testing.B) {
		app := fiber.New()
		app.Use(New(Config{
			Stream:      io.Discard,
			ForceColors: true,
		}))
		app.Get("/", func(c fiber.Ctx) error {
			return c.SendString("Hello, World!")
		})
		benchmarkSetup(bb, app, "/")
	})

	b.Run("DefaultFormatWithFiberLog", func(bb *testing.B) {
		app := fiber.New()
		logger := fiberlog.DefaultLogger[*log.Logger]()
		logger.SetOutput(io.Discard)
		app.Use(New(Config{
			Stream: LoggerToWriter(logger, fiberlog.LevelDebug),
		}))
		app.Get("/", func(c fiber.Ctx) error {
			return c.SendString("Hello, World!")
		})
		benchmarkSetup(bb, app, "/")
	})

	b.Run("WithTagParameter", func(bb *testing.B) {
		app := fiber.New()
		app.Use(New(Config{
			Format: "${bytesReceived} ${bytesSent} ${status} ${reqHeader:test}",
			Stream: io.Discard,
		}))
		app.Get("/", func(c fiber.Ctx) error {
			c.Set("test", "test")
			return c.SendString("Hello, World!")
		})
		benchmarkSetup(bb, app, "/")
	})

	b.Run("WithLocals", func(bb *testing.B) {
		app := fiber.New()
		app.Use(New(Config{
			Format: "${locals:demo}",
			Stream: io.Discard,
		}))
		app.Get("/", func(c fiber.Ctx) error {
			c.Locals("demo", "johndoe")
			return c.SendStatus(fiber.StatusOK)
		})
		benchmarkSetup(bb, app, "/")
	})

	b.Run("WithLocalsInt", func(bb *testing.B) {
		app := fiber.New()
		app.Use(New(Config{
			Format: "${locals:demo}",
			Stream: io.Discard,
		}))
		app.Get("/int", func(c fiber.Ctx) error {
			c.Locals("demo", 55)
			return c.SendStatus(fiber.StatusOK)
		})
		benchmarkSetup(bb, app, "/int")
	})

	b.Run("WithCustomDone", func(bb *testing.B) {
		app := fiber.New()
		app.Use(New(Config{
			Done: func(c fiber.Ctx, logString []byte) {
				if c.Response().StatusCode() == fiber.StatusOK {
					io.Discard.Write(logString) //nolint:errcheck // ignore error
				}
			},
			Stream: io.Discard,
		}))
		app.Get("/logging", func(ctx fiber.Ctx) error {
			return ctx.SendStatus(fiber.StatusOK)
		})
		benchmarkSetup(bb, app, "/logging")
	})

	b.Run("WithAllTags", func(bb *testing.B) {
		app := fiber.New()
		app.Use(New(Config{
			Format: "${pid}${reqHeaders}${referer}${scheme}${protocol}${ip}${ips}${host}${url}${ua}${body}${route}${black}${red}${green}${yellow}${blue}${magenta}${cyan}${white}${reset}${error}${reqHeader:test}${query:test}${form:test}${cookie:test}",
			Stream: io.Discard,
		}))
		app.Get("/", func(c fiber.Ctx) error {
			return c.SendString("Hello, World!")
		})
		benchmarkSetup(bb, app, "/")
	})

	b.Run("Streaming", func(bb *testing.B) {
		app := fiber.New()
		app.Use(New(Config{
			Format: "${bytesReceived} ${bytesSent} ${status}",
			Stream: io.Discard,
		}))
		app.Get("/", func(c fiber.Ctx) error {
			c.Set("Connection", "keep-alive")
			c.Set("Transfer-Encoding", "chunked")
			c.RequestCtx().SetBodyStreamWriter(func(w *bufio.Writer) {
				var i int
				for {
					i++
					msg := fmt.Sprintf("%d - the time is %v", i, time.Now())
					fmt.Fprintf(w, "data: Message: %s\n\n", msg)
					err := w.Flush()
					if err != nil {
						break
					}
					if i == 10 {
						break
					}
				}
			})
			return nil
		})
		benchmarkSetup(bb, app, "/")
	})

	b.Run("WithBody", func(bb *testing.B) {
		app := fiber.New()
		app.Use(New(Config{
			Format: "${resBody}",
			Stream: io.Discard,
		}))
		app.Get("/", func(c fiber.Ctx) error {
			return c.SendString("Sample response body")
		})
		benchmarkSetup(bb, app, "/")
	})
}

func Benchmark_Logger_Parallel(b *testing.B) {
	b.Run("NoMiddleware", func(bb *testing.B) {
		app := fiber.New()
		app.Get("/", func(c fiber.Ctx) error {
			return c.SendString("Hello, World!")
		})
		benchmarkSetupParallel(bb, app, "/")
	})

	b.Run("WithBytesAndStatus", func(bb *testing.B) {
		app := fiber.New()
		app.Use(New(Config{
			Format: "${bytesReceived} ${bytesSent} ${status}",
			Stream: io.Discard,
		}))
		app.Get("/", func(c fiber.Ctx) error {
			c.Set("test", "test")
			return c.SendString("Hello, World!")
		})
		benchmarkSetupParallel(bb, app, "/")
	})

	b.Run("DefaultFormat", func(bb *testing.B) {
		app := fiber.New()
		app.Use(New(Config{
			Stream: io.Discard,
		}))
		app.Get("/", func(c fiber.Ctx) error {
			return c.SendString("Hello, World!")
		})
		benchmarkSetupParallel(bb, app, "/")
	})

	b.Run("DefaultFormatWithFiberLog", func(bb *testing.B) {
		app := fiber.New()
		logger := fiberlog.DefaultLogger[*log.Logger]()
		logger.SetOutput(io.Discard)
		app.Use(New(Config{
			Stream: LoggerToWriter(logger, fiberlog.LevelDebug),
		}))
		app.Get("/", func(c fiber.Ctx) error {
			return c.SendString("Hello, World!")
		})
		benchmarkSetupParallel(bb, app, "/")
	})

	b.Run("DefaultFormatDisableColors", func(bb *testing.B) {
		app := fiber.New()
		app.Use(New(Config{
			Stream:        io.Discard,
			DisableColors: true,
		}))
		app.Get("/", func(c fiber.Ctx) error {
			return c.SendString("Hello, World!")
		})
		benchmarkSetupParallel(bb, app, "/")
	})

	b.Run("DefaultFormatForceColors", func(bb *testing.B) {
		app := fiber.New()
		app.Use(New(Config{
			Stream:      io.Discard,
			ForceColors: true,
		}))
		app.Get("/", func(c fiber.Ctx) error {
			return c.SendString("Hello, World!")
		})
		benchmarkSetupParallel(bb, app, "/")
	})

	b.Run("WithTagParameter", func(bb *testing.B) {
		app := fiber.New()
		app.Use(New(Config{
			Format: "${bytesReceived} ${bytesSent} ${status} ${reqHeader:test}",
			Stream: io.Discard,
		}))
		app.Get("/", func(c fiber.Ctx) error {
			c.Set("test", "test")
			return c.SendString("Hello, World!")
		})
		benchmarkSetupParallel(bb, app, "/")
	})

	b.Run("WithLocals", func(bb *testing.B) {
		app := fiber.New()
		app.Use(New(Config{
			Format: "${locals:demo}",
			Stream: io.Discard,
		}))
		app.Get("/", func(c fiber.Ctx) error {
			c.Locals("demo", "johndoe")
			return c.SendStatus(fiber.StatusOK)
		})
		benchmarkSetupParallel(bb, app, "/")
	})

	b.Run("WithLocalsInt", func(bb *testing.B) {
		app := fiber.New()
		app.Use(New(Config{
			Format: "${locals:demo}",
			Stream: io.Discard,
		}))
		app.Get("/int", func(c fiber.Ctx) error {
			c.Locals("demo", 55)
			return c.SendStatus(fiber.StatusOK)
		})
		benchmarkSetupParallel(bb, app, "/int")
	})

	b.Run("WithCustomDone", func(bb *testing.B) {
		app := fiber.New()
		app.Use(New(Config{
			Done: func(c fiber.Ctx, logString []byte) {
				if c.Response().StatusCode() == fiber.StatusOK {
					io.Discard.Write(logString) //nolint:errcheck // ignore error
				}
			},
			Stream: io.Discard,
		}))
		app.Get("/logging", func(ctx fiber.Ctx) error {
			return ctx.SendStatus(fiber.StatusOK)
		})
		benchmarkSetupParallel(bb, app, "/logging")
	})

	b.Run("WithAllTags", func(bb *testing.B) {
		app := fiber.New()
		app.Use(New(Config{
			Format: "${pid}${reqHeaders}${referer}${scheme}${protocol}${ip}${ips}${host}${url}${ua}${body}${route}${black}${red}${green}${yellow}${blue}${magenta}${cyan}${white}${reset}${error}${reqHeader:test}${query:test}${form:test}${cookie:test}",
			Stream: io.Discard,
		}))
		app.Get("/", func(c fiber.Ctx) error {
			return c.SendString("Hello, World!")
		})
		benchmarkSetupParallel(bb, app, "/")
	})

	b.Run("Streaming", func(bb *testing.B) {
		app := fiber.New()
		app.Use(New(Config{
			Format: "${bytesReceived} ${bytesSent} ${status}",
			Stream: io.Discard,
		}))
		app.Get("/", func(c fiber.Ctx) error {
			c.Set("Connection", "keep-alive")
			c.Set("Transfer-Encoding", "chunked")
			c.RequestCtx().SetBodyStreamWriter(func(w *bufio.Writer) {
				var i int
				for {
					i++
					msg := fmt.Sprintf("%d - the time is %v", i, time.Now())
					fmt.Fprintf(w, "data: Message: %s\n\n", msg)
					err := w.Flush()
					if err != nil {
						break
					}
					if i == 10 {
						break
					}
				}
			})
			return nil
		})
		benchmarkSetupParallel(bb, app, "/")
	})

	b.Run("WithBody", func(bb *testing.B) {
		app := fiber.New()
		app.Use(New(Config{
			Format: "${resBody}",
			Stream: io.Discard,
		}))
		app.Get("/", func(c fiber.Ctx) error {
			return c.SendString("Sample response body")
		})
		benchmarkSetupParallel(bb, app, "/")
	})
}

// failingBuffer is a Buffer whose WriteString starts failing after failAfter
// successful calls, so the error paths of writeSanitizedColored can be reached
// without a real failing sink.
type failingBuffer struct {
	*bytebufferpool.ByteBuffer
	failAfter int
	calls     int
}

var errWriteFailed = errors.New("write failed")

func (b *failingBuffer) WriteString(s string) (int, error) {
	if b.calls >= b.failAfter {
		return 0, errWriteFailed
	}
	b.calls++
	n, err := b.ByteBuffer.WriteString(s)
	if err != nil {
		return n, fmt.Errorf("failingBuffer write string: %w", err)
	}
	return n, nil
}

// WriteByte fails after failAfter successful writes, counting against the
// same budget as WriteString, so the byte-at-a-time scratch writer can be
// made to fail mid-column.
func (b *failingBuffer) WriteByte(c byte) error {
	if b.calls >= b.failAfter {
		return errWriteFailed
	}
	b.calls++
	if err := b.ByteBuffer.WriteByte(c); err != nil {
		return fmt.Errorf("failingBuffer write byte: %w", err)
	}
	return nil
}

func (b *failingBuffer) Write(p []byte) (int, error) {
	if b.calls >= b.failAfter {
		return 0, errWriteFailed
	}
	b.calls++
	n, err := b.ByteBuffer.Write(p)
	if err != nil {
		return n, fmt.Errorf("failingBuffer write: %w", err)
	}
	return n, nil
}

// loggerTestAllWrites is a failAfter that no scenario comes close to using up,
// for the cases where every write is meant to succeed.
const loggerTestAllWrites = 99

// loggerTestWriteCase is one scenario for loggerTestWriteErrors: the sink takes
// failAfter writes and rejects every later one, and the function under test must
// then report wantN bytes written and leave exactly wantOut in the sink. Unless
// succeeds is set it must also return errWriteFailed. note says why those are
// the right values and is part of the failure message.
type loggerTestWriteCase struct {
	name      string
	wantOut   string
	note      string
	failAfter int
	wantN     int
	succeeds  bool
}

// loggerTestWriteErrors runs call against a failingBuffer once per case, in
// parallel subtests named after the cases. The contract it pins is the one a
// sink that fails partway through a column needs: the sequence stops at the
// failure, the error surfaces, and the returned byte count covers only what
// actually reached the sink. call receives the subtest's t for scenarios that
// need a fixture of their own.
func loggerTestWriteErrors(t *testing.T, call func(t *testing.T, sink Buffer) (int, error), cases []loggerTestWriteCase) {
	t.Helper()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			buf := &failingBuffer{ByteBuffer: loggerTestBuf(t), failAfter: tc.failAfter}

			n, err := call(t, buf)
			if tc.succeeds {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, errWriteFailed)
			}
			require.Equal(t, tc.wantN, n, tc.note)
			require.Equal(t, tc.wantOut, buf.String(), tc.note)
		})
	}
}

// Test_writeSanitizedColored_PropagatesWriteErrors pins the short-circuits in
// writeSanitizedColored. A sink that fails partway must stop the sequence and
// surface the error rather than writing a reset escape with no matching color
// (or silently dropping the failure), and the byte count returned must reflect
// only what actually reached the sink.
func Test_writeSanitizedColored_PropagatesWriteErrors(t *testing.T) {
	t.Parallel()

	const color, value, reset = "<c>", "va\nlue", "<r>"

	loggerTestWriteErrors(t, func(_ *testing.T, sink Buffer) (int, error) {
		return writeSanitizedColored(sink, color, value, reset)
	}, []loggerTestWriteCase{
		{name: "color write fails", failAfter: 0, note: "nothing may reach the sink once the color fails"},
		{name: "value write fails", failAfter: 1, wantN: len(color), wantOut: color, note: "only the color made it out, and the reset must not be written after a failure"},
		{name: "reset write fails", failAfter: 2, wantN: len(color) + len(value), wantOut: "<c>va lue", note: "the color and value made it out, the reset did not"},
		{
			name: "all writes succeed", failAfter: loggerTestAllWrites, succeeds: true,
			wantN: len(color) + len(value) + len(reset), wantOut: "<c>va lue<r>",
			note: "the value is scrubbed but the color escapes pass through verbatim",
		},
	})
}

// Buffer is a public type — Config.LoggerFunc is handed one — so a sink that
// fails partway through a column is reachable by anything a user plugs in. The
// tests below pin the same contract Test_writeSanitizedColored_PropagatesWriteErrors
// pins for the sanitized writer: the column stops at the failure, the error
// surfaces, and the returned byte count covers only what actually reached the
// sink.

// Test_appendIntTag_PropagatesWriteErrors covers the padded integer column the
// colored ${status} tag writes through. 7 in a width of 3 needs two padding
// spaces, so the padding and the digit can fail independently.
func Test_appendIntTag_PropagatesWriteErrors(t *testing.T) {
	t.Parallel()

	loggerTestWriteErrors(t, func(_ *testing.T, sink Buffer) (int, error) {
		return appendIntTag(sink, 7, 3)
	}, []loggerTestWriteCase{
		{name: "padding write fails", failAfter: 0},
		{name: "digit write fails", failAfter: 2, wantN: 2, wantOut: "  ", note: "both padding spaces made it out, the digit did not"},
		{name: "all writes succeed", failAfter: loggerTestAllWrites, succeeds: true, wantN: 3, wantOut: "  7"},
	})
}

// Test_appendInt_PropagatesWriteErrors covers the unpadded integer the
// uncolored ${status}, ${bytesSent} and ${bytesReceived} tags write.
func Test_appendInt_PropagatesWriteErrors(t *testing.T) {
	t.Parallel()

	loggerTestWriteErrors(t, func(_ *testing.T, sink Buffer) (int, error) {
		return appendInt(sink, 200)
	}, []loggerTestWriteCase{
		{name: "second digit write fails", failAfter: 1, wantN: 1, wantOut: "2", note: "only the first digit made it out"},
	})
}

// Test_appendDurationTag_PropagatesWriteErrors covers the ${latency} column.
// Its padding is counted in runes, so a duration rendering "µs" still has to
// stop and report in bytes.
func Test_appendDurationTag_PropagatesWriteErrors(t *testing.T) {
	t.Parallel()

	const (
		width  = 13
		padded = "        145µs"
	)
	d := 145 * time.Microsecond // "145µs": 5 runes, 6 bytes

	loggerTestWriteErrors(t, func(_ *testing.T, sink Buffer) (int, error) {
		return appendDurationTag(sink, d, width)
	}, []loggerTestWriteCase{
		{name: "padding write fails", failAfter: 0},
		// Eight spaces pad "145µs" to 13 columns; the ninth write is the
		// first byte of the duration itself.
		{name: "duration write fails", failAfter: 8, wantN: 8, wantOut: "        ", note: "the padding made it out, the duration did not"},
		{
			name: "all writes succeed", failAfter: loggerTestAllWrites, succeeds: true,
			wantN: len(padded), wantOut: padded,
			note: "padded to 13 columns, which is 14 bytes because of the two-byte micro sign",
		},
	})
	loggerTestWriteErrors(t, func(_ *testing.T, sink Buffer) (int, error) {
		return appendDurationTag(sink, d, 2)
	}, []loggerTestWriteCase{
		{
			name: "value wider than the column", failAfter: loggerTestAllWrites, succeeds: true,
			wantN: len("145µs"), wantOut: "145µs",
			note: "a latency past the column width is written whole, with no padding",
		},
	})
}

// Test_writeColored_PropagatesWriteErrors covers the colored ${method} tag.
func Test_writeColored_PropagatesWriteErrors(t *testing.T) {
	t.Parallel()

	const color, value, reset = "<c>", "GET", "<r>"

	loggerTestWriteErrors(t, func(_ *testing.T, sink Buffer) (int, error) {
		return writeColored(sink, color, value, reset)
	}, []loggerTestWriteCase{
		{name: "color write fails", failAfter: 0},
		{name: "value write fails", failAfter: 1, wantN: len(color), wantOut: color, note: "the reset must not follow a failed value"},
		{name: "reset write fails", failAfter: 2, wantN: len(color) + len(value), wantOut: color + value},
		{
			name: "all writes succeed", failAfter: loggerTestAllWrites, succeeds: true,
			wantN: len(color) + len(value) + len(reset), wantOut: color + value + reset,
			note: "the method is written verbatim, as the uncolored branch writes it",
		},
	})
}

// Test_writeColoredInt_PropagatesWriteErrors covers the colored ${status} tag,
// which wraps the padded integer column in the two color escapes.
func Test_writeColoredInt_PropagatesWriteErrors(t *testing.T) {
	t.Parallel()

	const color, reset = "<c>", "<r>"

	loggerTestWriteErrors(t, func(_ *testing.T, sink Buffer) (int, error) {
		return writeColoredInt(sink, color, 200, 3, reset)
	}, []loggerTestWriteCase{
		{name: "color write fails", failAfter: 0},
		{name: "status write fails", failAfter: 1, wantN: len(color), wantOut: color, note: "the reset must not follow a failed status"},
		// One WriteString for the color, then three WriteByte calls for the
		// digits; the fifth write is the reset.
		{name: "reset write fails", failAfter: 4, wantN: len(color) + 3, wantOut: color + "200"},
		{
			name: "all writes succeed", failAfter: loggerTestAllWrites, succeeds: true,
			wantN: len(color) + 3 + len(reset), wantOut: color + "200" + reset,
		},
	})
	loggerTestWriteErrors(t, func(_ *testing.T, sink Buffer) (int, error) {
		return writeColoredInt(sink, color, 200, 1, reset)
	}, []loggerTestWriteCase{
		{
			name: "value wider than the column", failAfter: loggerTestAllWrites, succeeds: true,
			wantN: len(color) + 3 + len(reset), wantOut: color + "200" + reset,
			note: "a status past the column width is written whole, with no padding",
		},
	})
}

// Test_Logger_SanitizesLocalsByType covers each arm of ${locals:}'s type
// switch. The []byte arm has its own scrubbing call, so a []byte local carrying
// CR/LF must be scrubbed just like the string one — otherwise a handler
// stashing raw request bytes in Locals would reopen the injection this scrubs.
func Test_Logger_SanitizesLocalsByType(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		value    any
		name     string
		expected string
	}{
		{name: "bytes", value: []byte("a\r\nb"), expected: "a  b"},
		{name: "string", value: "a\r\nb", expected: "a  b"},
		{name: "nil", value: nil, expected: ""},
		{name: "other", value: struct{ V string }{"a\r\nb"}, expected: "{a  b}"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app, buf := loggerTestApp(t, &Config{Format: "${locals:demo}"})
			app.Get("/", func(c fiber.Ctx) error {
				if tc.value != nil {
					c.Locals("demo", tc.value)
				}
				return c.SendStatus(fiber.StatusOK)
			})

			apptest.Expect(t, app, "/", fiber.StatusOK)
			require.Equal(t, tc.expected, buf.String())
		})
	}
}

// Test_Logger_SanitizesControlBytes ensures user-controlled values cannot
// inject CR/LF (or other C0/DEL bytes) into a log line and forge log entries.
// See https://github.com/gofiber/fiber/issues/4341.
func Test_Logger_SanitizesControlBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		build        func() *http.Request
		name         string
		format       string
		expected     string
		unescapePath bool
	}{
		{
			name:   "query",
			format: "${query:q}",
			build: func() *http.Request {
				return httptest.NewRequest(fiber.MethodGet, "/?q=a%0d%0a200+GET+/legit", http.NoBody)
			},
			expected: "a  200 GET /legit",
		},
		{
			name:   "body",
			format: "${body}",
			build: func() *http.Request {
				req := httptest.NewRequest(fiber.MethodPost, "/", strings.NewReader("a\r\nb\x00c"))
				req.Header.Set(fiber.HeaderContentType, fiber.MIMETextPlain)
				return req
			},
			expected: "a  b c",
		},
		{
			name:   "tab is preserved",
			format: "${query:q}",
			build: func() *http.Request {
				return httptest.NewRequest(fiber.MethodGet, "/?q=a%09b", http.NoBody)
			},
			expected: "a\tb",
		},
		{
			// The decoded-path case: with UnescapePath enabled c.Path() carries
			// the percent-decoded bytes, so ${path} would otherwise emit raw CRLF.
			name:         "decoded path",
			format:       "${path}",
			unescapePath: true,
			build: func() *http.Request {
				return httptest.NewRequest(fiber.MethodGet, "/admin%0d%0a200", http.NoBody)
			},
			expected: "/admin  200",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			app, buf := loggerTestApp(t, &Config{Format: tt.format}, fiber.Config{UnescapePath: tt.unescapePath})
			app.Add([]string{fiber.MethodGet, fiber.MethodPost}, "/", func(c fiber.Ctx) error {
				return c.SendString("ok")
			})

			apptest.Do(t, app, tt.build())
			require.Equal(t, tt.expected, buf.String())
		})
	}
}

// Test_Logger_DefaultFormat_SanitizesControlBytes covers the default logger
// configuration, which takes defaultLoggerInstance's hand-written fast path
// instead of the tag map. Without scrubbing there, plain logger.New() — by far
// the most common setup — still lets a request forge extra log lines (#4341).
func Test_Logger_DefaultFormat_SanitizesControlBytes(t *testing.T) {
	t.Parallel()

	for _, colors := range []bool{false, true} {
		t.Run(fmt.Sprintf("colors=%v", colors), func(t *testing.T) {
			t.Parallel()

			app, buf := loggerTestApp(t, &Config{ForceColors: colors, DisableColors: !colors}, fiber.Config{UnescapePath: true})
			app.Get("/*", func(_ fiber.Ctx) error {
				return errors.New("boom\r\nFORGED-ERROR-LINE")
			})

			apptest.Do(t, app, "/admin%0d%0aFORGED-PATH-LINE")

			out := buf.String()
			require.NotContains(t, out, "\r")
			require.Equal(t, 1, strings.Count(out, "\n"), "log entry must stay on one line: %q", out)
			require.Contains(t, out, "FORGED-PATH-LINE")
			require.Contains(t, out, "FORGED-ERROR-LINE")
		})
	}
}

// Test_SanitizeValue covers the exported helper the docs point custom tags,
// context tags and LoggerFunc implementations at. It has to apply exactly what
// the built-in tags apply, or the guidance is wrong.
func Test_SanitizeValue(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		in   string
		want string
	}{
		{name: "clean passes through", in: "plain-value", want: "plain-value"},
		{name: "empty", in: "", want: ""},
		{name: "CRLF is neutralized", in: "a\r\nb", want: "a  b"},
		{name: "tab is preserved", in: "a\tb", want: "a\tb"},
		{name: "NUL and DEL", in: "a\x00b\x7fc", want: "a b c"},
		{name: "escape sequence", in: "a\x1b[31mb", want: "a [31mb"},
		// Documented limitation: only ASCII controls are replaced, so C1
		// controls (including NEL U+0085) survive.
		{name: "C1 NEL passes through", in: "a\u0085b", want: "a\u0085b"},
		{name: "multibyte is untouched", in: "h\u00e9llo", want: "h\u00e9llo"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, SanitizeValue(tc.in))
			// The exported helper must not diverge from the internal one the
			// built-in tags use.
			require.Equal(t, sanitizeLogValue(tc.in), SanitizeValue(tc.in))
		})
	}
}

// Test_Logger_IPs asserts ${ips} logs the chain the framework acted on: every
// X-Forwarded-For field line, whatever case the field name arrived in.
func Test_Logger_IPs(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		want    string
		lines   []string
		disable bool
	}{
		// ${ips} logs every X-Forwarded-For field line, not just the first.
		//
		// A recipient may combine repeated field lines into the comma-joined form
		// (RFC 9110 §5.2), and Fiber's proxy-header accessors do exactly that — so
		// reading only the first here logged a shorter chain than the one c.IP() and
		// c.IPs() parsed, and the access log did not explain what was actually
		// enforced.
		{
			name:  "normalized, two lines, three addresses",
			lines: []string{"X-Forwarded-For: 1.1.1.1", "X-Forwarded-For: 2.2.2.2, 3.3.3.3"},
			want:  "1.1.1.1,2.2.2.2,3.3.3.3",
		},
		// The tag logs the chain the framework acted on, whatever case the field
		// name arrived in.
		//
		// Reading the header here rather than asking c.IPs() meant reading it a
		// second way, and PeekAll compares stored names byte for byte — so under
		// DisableHeaderNormalizing a lower-case "x-forwarded-for:" matched nothing
		// and the tag logged an empty chain, while c.IPs() went on parsing it and the
		// trust decisions went on using it.
		{
			name:  "normalized, one line",
			lines: []string{"X-Forwarded-For: 1.1.1.1, 2.2.2.2"},
			want:  "1.1.1.1,2.2.2.2",
		},
		{
			name:    "raw, lower-case name",
			lines:   []string{"x-forwarded-for: 1.1.1.1, 2.2.2.2"},
			disable: true,
			want:    "1.1.1.1,2.2.2.2",
		},
		{
			name:  "normalized, two lines",
			lines: []string{"X-Forwarded-For: 1.1.1.1", "X-Forwarded-For: 2.2.2.2"},
			want:  "1.1.1.1,2.2.2.2",
		},
		{
			name:    "raw, mixed-case names",
			lines:   []string{"x-forwarded-for: 1.1.1.1", "X-Forwarded-For: 2.2.2.2"},
			disable: true,
			want:    "1.1.1.1,2.2.2.2",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app, buf := loggerTestApp(t, &Config{Format: "${ips}"}, fiber.Config{
				TrustProxy:               true,
				DisableHeaderNormalizing: tc.disable,
			})
			app.Get("/", func(c fiber.Ctx) error { return c.SendString("hi") })

			parts := []string{"GET / HTTP/1.1\r\nHost: example.com\r\n"}
			for _, l := range tc.lines {
				parts = append(parts, l+"\r\n")
			}
			raw := strings.Join(append(parts, "\r\n"), "")

			req := fasthttp.AcquireRequest()
			defer fasthttp.ReleaseRequest(req)
			if tc.disable {
				req.Header.DisableNormalizing()
			}
			require.NoError(t, req.Read(bufio.NewReader(strings.NewReader(raw))))

			fctx := &fasthttp.RequestCtx{}
			fctx.Init(req, nil, nil)
			app.Handler()(fctx)

			require.Equal(t, tc.want, buf.String(),
				"every field line must be logged, as the chain the framework parsed and the trust decisions used")
		})
	}
}

// Test_Logger_RegisterContextTag_Sanitizes pins that a value rendered by a
// registered context tag cannot close the log line it is written on.
//
// Every built-in tag passes through the sanitizer; this path did not, so a CR
// or LF in whatever the application pulled out of the context — in practice
// request data — started a second entry the reader has no way to tell from a
// real one.
func Test_Logger_RegisterContextTag_Sanitizes(t *testing.T) {
	t.Parallel()

	// Both registries are mutex-guarded and re-registering replaces rather
	// than fails, so this runs in parallel like every other registry test in
	// the package. The name has to be its own, though: registration is global
	// and outlives the test whether or not it is parallel.
	RegisterContextTag("sanitize-probe", func(_ any) string {
		return "before\r\nGET /forged HTTP/1.1\nafter"
	})

	line := loggerTestLog(t, &Config{Format: "${sanitize-probe}\n"})
	require.NotContains(t, line, "\r", "a CR must not reach the log")
	require.Equal(t, 1, strings.Count(line, "\n"), "the entry must occupy one line: %q", line)
	require.Contains(t, line, "before")
	require.Contains(t, line, "after")
}

// Test_TagIPs_PropagatesWriteErrors pins the short-circuits in the ${ips} tag.
// A sink that fails partway must stop and surface the error rather than
// carrying on, and the count returned must reflect only what reached the sink.
func Test_TagIPs_PropagatesWriteErrors(t *testing.T) {
	t.Parallel()

	// A trusted proxy, so c.IPs() returns the forwarded chain rather than
	// nothing at all — the tag needs more than one entry to reach the
	// separator between them.
	app := fiber.New(fiber.Config{
		TrustProxy:       true,
		TrustProxyConfig: fiber.TrustProxyConfig{Loopback: true},
	})
	tag := createTagMap(&ConfigDefault)[TagIPs]

	loggerTestWriteErrors(t, func(t *testing.T, sink Buffer) (int, error) {
		t.Helper()

		fctx := &fasthttp.RequestCtx{}
		fctx.Request.Header.SetMethod(fiber.MethodGet)
		fctx.Request.SetRequestURI("/")
		fctx.Request.Header.Set(fiber.HeaderXForwardedFor, "1.1.1.1, 2.2.2.2")
		return tag(sink, apptest.CtxFor(t, app, fctx), nil, "")
	}, []loggerTestWriteCase{
		{name: "the whole chain is written", failAfter: loggerTestAllWrites, succeeds: true, wantN: len("1.1.1.1,2.2.2.2"), wantOut: "1.1.1.1,2.2.2.2"},
		{name: "the first entry fails", failAfter: 0},
		// One successful write for the first address, then the "," fails.
		{name: "the separator fails", failAfter: 1, wantN: len("1.1.1.1"), wantOut: "1.1.1.1", note: "only the first address reached the sink"},
		// The first address and the separator succeed; the second fails.
		{name: "the second entry fails", failAfter: 2, wantN: len("1.1.1.1,"), wantOut: "1.1.1.1,"},
	})
}

func Test_Logger_ResBody_DoesNotDrainStream(t *testing.T) {
	t.Parallel()

	var logged bytes.Buffer
	app := fiber.New()
	app.Use(New(Config{
		Format: "[${resBody}]",
		Stream: &logged,
	}))
	app.Get("/events", func(c fiber.Ctx) error {
		return c.SendStreamWriter(func(w *bufio.Writer) {
			w.WriteString("data: one\n\n") //nolint:errcheck // nothing to do with a stream-writer error here
			w.Flush()                      //nolint:errcheck // same
		})
	})

	fctx := &fasthttp.RequestCtx{}
	fctx.Request.SetRequestURI("/events")
	fctx.Request.Header.SetMethod(fiber.MethodGet)
	app.Handler()(fctx)

	require.True(t, fctx.Response.IsBodyStream(), "logging must leave a streamed response streamed")
	require.Equal(t, "[]", logged.String(), "and it logs nothing rather than the buffered stream")

	logged.Reset()
	buffered := fiber.New()
	buffered.Use(New(Config{Format: "[${resBody}]", Stream: &logged}))
	buffered.Get("/", func(c fiber.Ctx) error {
		return c.SendString("hello")
	})

	bctx := &fasthttp.RequestCtx{}
	bctx.Request.SetRequestURI("/")
	bctx.Request.Header.SetMethod(fiber.MethodGet)
	buffered.Handler()(bctx)

	require.Equal(t, "[hello]", logged.String())
}

// Test_LatencyColumns_MatchFmt pins the two latency renderers against the
// fmt-based forms they replaced, over random durations spread across every
// magnitude Duration.String distinguishes. appendDurationTag, behind
// ${latency}, must match "%13v": fmt pads by runes, so a sub-millisecond
// value's two-byte 'µ' takes one column. appendDurationPadded, behind the
// default format, must match the byte-counted fixedWidth(d.String(), 13,
// true) it replaced, so that column is unchanged byte for byte.
func Test_LatencyColumns_MatchFmt(t *testing.T) {
	t.Parallel()

	r := rand.New(rand.NewPCG(0x1a7e, 0xc0de))
	durations := []time.Duration{
		0, 1, -1, 999, 1000, 1001, 999999, 1000000, 1000001,
		time.Second - 1, time.Second, time.Second + 1, time.Minute, time.Hour, 61 * time.Minute,
		145 * time.Microsecond, -145 * time.Microsecond, 1500 * time.Microsecond, 999 * time.Millisecond,
		math.MaxInt64, math.MinInt64, math.MinInt64 + 1,
	}
	for range 20000 {
		// A random bit width first, so nanoseconds and hours are equally likely.
		bits := 1 + r.IntN(63)
		d := time.Duration(r.Uint64() & (1<<uint(bits) - 1))
		if r.IntN(8) == 0 {
			d = -d
		}
		durations = append(durations, d)
	}

	tagBuf := loggerTestBuf(t)
	padBuf := loggerTestBuf(t)

	for _, d := range durations {
		tagBuf.Reset()
		n, err := appendDurationTag(tagBuf, d, 13)
		require.NoError(t, err)
		want := fmt.Sprintf("%13v", d)
		require.Equal(t, want, tagBuf.String(), "appendDurationTag(%d)", int64(d))
		require.Equal(t, len(want), n, "appendDurationTag(%d) byte count", int64(d))

		padBuf.Reset()
		appendDurationPadded(padBuf, d, 13)
		s := d.String()
		want = strings.Repeat(" ", max(0, 13-len(s))) + s
		require.Equal(t, want, padBuf.String(), "appendDurationPadded(%d)", int64(d))

		// The colored default format's column counts runes like "%13v" does.
		padBuf.Reset()
		appendDurationColumns(padBuf, d, 13)
		require.Equal(t, fmt.Sprintf("%13v", d), padBuf.String(), "appendDurationColumns(%d)", int64(d))
	}
}

// Test_Logger_DefaultFormat_WithColor_MatchesFmt pins the colored default
// line, byte for byte, to the Fprintf call it was written with until that call
// was replaced by in-place writes. The old format string is kept here as the
// oracle, so every detail of fmt's output — rune-counted padding for the
// latency and path columns, the colors wrapping " NNN " and " GET     " with
// their spaces inside, the error padding parsed from the format — stays what
// it was. The cases cover the two places bytes and columns differ: a
// sub-millisecond latency ("µs") and a non-ASCII path.
func Test_Logger_DefaultFormat_WithColor_MatchesFmt(t *testing.T) {
	t.Parallel()

	fixedStart := time.Unix(0, 0)
	testCases := []struct {
		err     error
		name    string
		target  string
		latency time.Duration
		status  int
	}{
		{name: "ascii path, ms latency", target: "/api/v1/users", latency: 1500 * time.Microsecond, status: fiber.StatusOK},
		{name: "micro latency is one column wide", target: "/x", latency: 145 * time.Microsecond, status: fiber.StatusNotFound},
		{name: "non-ascii path pads by runes", target: "/caf%C3%A9/%C3%A9t%C3%A9", latency: 3 * time.Millisecond, status: fiber.StatusOK},
		{name: "chain error", target: "/boom", latency: 2 * time.Second, err: errors.New("kaboom")},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var want string
			app, buf := loggerTestApp(t, &Config{
				ForceColors: true,
				LoggerFunc: func(c fiber.Ctx, data *Data, cfg *Config) error {
					data.Start = fixedStart
					data.Stop = fixedStart.Add(tc.latency)

					colors := c.App().Config().ColorScheme
					formatErr := ""
					if data.ChainErr != nil {
						formatErr = colors.Red + " | " + sanitizeLogValue(data.ChainErr.Error()) + colors.Reset
					}
					want = fmt.Sprintf(
						"%s |%s %3d %s| %13v | %15s |%s %-7s %s| %-"+data.ErrPaddingStr+"s %s\n",
						data.Timestamp,
						statusColor(c.Res().StatusCode(), &colors), c.Res().StatusCode(), colors.Reset,
						data.Stop.Sub(data.Start),
						sanitizeLogValue(c.IP()),
						methodColor(c.Method(), &colors), c.Method(), colors.Reset,
						sanitizeLogValue(c.Path()),
						formatErr,
					)
					return defaultLoggerInstance(c, data, cfg)
				},
			}, fiber.Config{UnescapePath: true})
			app.Get("/*", func(c fiber.Ctx) error {
				if tc.err != nil {
					return tc.err
				}
				return c.SendStatus(tc.status)
			})

			apptest.Do(t, app, tc.target)
			require.NotEmpty(t, want, "the logger must have run")
			require.Equal(t, want, buf.String())
		})
	}
}

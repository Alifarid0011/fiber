package log

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const work = "work"

func initDefaultLogger() {
	logger = &defaultLogger{
		stdlog: log.New(os.Stderr, "", 0),
		depth:  4,
	}
	MustSetContextTemplate(ContextConfig{})
}

type byteSliceWriter struct {
	b []byte
}

func (w *byteSliceWriter) Write(p []byte) (int, error) {
	w.b = append(w.b, p...)
	return len(p), nil
}

// callSiteLine returns the line number of the call that invoked it. Pair it
// with a call on the next line to capture an absolute line number that stays
// in sync when surrounding code shifts.
func callSiteLine(t *testing.T) int {
	t.Helper()
	_, _, line, ok := runtime.Caller(1)
	require.True(t, ok)
	return line
}

// expectCallerOutput asserts that w contains exactly two log lines, the first
// emitted from withContextLine and the second from infoLine, both annotated
// with default_test.go via the standard library's Lshortfile flag.
func expectCallerOutput(t *testing.T, w *byteSliceWriter, withContextLine, infoLine int) {
	t.Helper()
	want := fmt.Sprintf("default_test.go:%d: [Info] \ndefault_test.go:%d: [Info] \n", withContextLine, infoLine)
	require.Equal(t, want, string(w.b), "log output should attribute the WithContext call site (line %d) and the bare Info call site (line %d)", withContextLine, infoLine)
}

// Test_WithContextCaller runs serially because it mutates the package-global
// logger and output to verify caller attribution.
func Test_WithContextCaller(t *testing.T) {
	tests := []struct {
		ctx  any
		name string
	}{
		{name: "context", ctx: context.TODO()},
		{name: "nil context", ctx: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Cleanup(initDefaultLogger)

			logger = &defaultLogger{
				stdlog: log.New(os.Stderr, "", log.Lshortfile),
				depth:  4,
			}

			var w byteSliceWriter
			SetOutput(&w)

			withContextLine := callSiteLine(t) + 1
			WithContext(tt.ctx).Info("")
			infoLine := callSiteLine(t) + 1
			Info("")

			expectCallerOutput(t, &w, withContextLine, infoLine)
		})
	}
}

// Test_WithContextRenderError locks in M8: a misconfigured context tag must
// not silently drop context — the failure should leave a visible marker in
// the log line so operators notice. The error message is also sanitized for
// control bytes so a tag that wraps attacker-controlled data in its error
// cannot inject CR/LF into the log stream. Calls initDefaultLogger up front
// because under -shuffle=on this test may run before any other log test, in
// which case the package-global logger.stdlog could still be nil.
func Test_WithContextRenderError(t *testing.T) {
	initDefaultLogger()
	t.Cleanup(initDefaultLogger)

	// Embed CR/LF in the error to exercise the sanitiser.
	templateErr := errors.New("tag\r\nboom")
	require.NoError(t, SetContextTemplate(ContextConfig{
		Format: "[${broken}] ",
		CustomTags: map[string]ContextTagFunc{
			"broken": func(_ Buffer, _ any, _ *ContextData, _ string) (int, error) {
				return 0, templateErr
			},
		},
	}))
	t.Cleanup(func() { MustSetContextTemplate(ContextConfig{}) })

	var w byteSliceWriter
	SetOutput(&w)

	WithContext(context.Background()).Info("payload")

	out := string(w.b)
	require.Contains(t, out, "ctx-render-error", "expected render-error marker, got %q", out)
	require.Contains(t, out, "payload", "expected payload to still be emitted, got %q", out)
	require.NotContains(t, out, "\r", "CR in error message must be sanitized")
	// One trailing newline from log.Output is expected; reject any embedded ones.
	require.Equal(t, 1, strings.Count(out, "\n"), "embedded LF in error message must be sanitized; got %q", out)
}

// Test_NewRetainedContext_TypedNil locks in the reflect-based nil detection
// in newRetainedContext: a typed-nil pointer wrapped in `any` must be treated
// as "no context" so tag renderers don't fault on a nil receiver.
func Test_NewRetainedContext_TypedNil(t *testing.T) {
	t.Parallel()

	type fakeCtx struct{}
	var typedNil *fakeCtx

	tests := []struct {
		value any
		name  string
		want  bool
	}{
		{name: "untyped nil", value: nil, want: false},
		{name: "typed nil pointer", value: typedNil, want: false},
		{name: "typed nil map", value: map[string]string(nil), want: false},
		{name: "typed nil slice", value: []byte(nil), want: false},
		{name: "typed nil chan", value: chan int(nil), want: false},
		{name: "non-nil pointer", value: &fakeCtx{}, want: true},
		{name: "string", value: "context", want: true},
		{name: "non-nil map", value: map[string]string{"k": "v"}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, newRetainedContext(tt.value).isOk)
		})
	}
}

func Test_DefaultLogger(t *testing.T) {
	initDefaultLogger()

	var w byteSliceWriter
	SetOutput(&w)

	Trace("trace work")
	Debug("received work order")
	Info("starting work")
	Warn("work may fail")
	Error("work failed")

	require.Panics(t, func() {
		Panic("work panic")
	})

	require.Equal(t, "[Trace] trace work\n"+
		"[Debug] received work order\n"+
		"[Info] starting work\n"+
		"[Warn] work may fail\n"+
		"[Error] work failed\n"+
		"[Panic] work panic\n", string(w.b))
}

func Test_DefaultFormatLogger(t *testing.T) {
	initDefaultLogger()

	var w byteSliceWriter
	SetOutput(&w)

	Tracef("trace %s", work)
	Debugf("received %s order", work)
	Infof("starting %s", work)
	Warnf("%s may fail", work)
	Errorf("%s failed", work)

	require.Panics(t, func() {
		Panicf("%s panic", work)
	})

	require.Equal(t, "[Trace] trace work\n"+
		"[Debug] received work order\n"+
		"[Info] starting work\n"+
		"[Warn] work may fail\n"+
		"[Error] work failed\n"+
		"[Panic] work panic\n", string(w.b))
}

func Test_CtxLogger(t *testing.T) {
	initDefaultLogger()

	var w byteSliceWriter
	SetOutput(&w)

	ctx := context.Background()

	WithContext(ctx).Tracef("trace %s", work)
	WithContext(ctx).Debugf("received %s order", work)
	WithContext(ctx).Infof("starting %s", work)
	WithContext(ctx).Warnf("%s may fail", work)
	WithContext(ctx).Errorf("%s failed %d", work, 50)

	require.Panics(t, func() {
		WithContext(ctx).Panicf("%s panic", work)
	})

	require.Equal(t, "[Trace] trace work\n"+
		"[Debug] received work order\n"+
		"[Info] starting work\n"+
		"[Warn] work may fail\n"+
		"[Error] work failed 50\n"+
		"[Panic] work panic\n", string(w.b))
}

// Test_WithContextTemplate runs serially because initDefaultLogger,
// SetContextTemplate, MustSetContextTemplate, and SetOutput mutate package
// globals shared with other log tests.
func Test_WithContextTemplate(t *testing.T) {
	initDefaultLogger()

	type requestIDKey struct{}
	ctx := context.WithValue(context.Background(), requestIDKey{}, "req-42")

	require.NoError(t, SetContextTemplate(ContextConfig{
		Format: "[${requestid}] ",
		CustomTags: map[string]ContextTagFunc{
			"requestid": func(output Buffer, ctx any, _ *ContextData, _ string) (int, error) {
				ctxTyped, ok := ctx.(context.Context)
				if !ok {
					return 0, nil
				}
				id, ok := ctxTyped.Value(requestIDKey{}).(string)
				if !ok {
					return 0, nil
				}
				return output.WriteString(id)
			},
		},
	}))
	t.Cleanup(func() { MustSetContextTemplate(ContextConfig{}) })

	var w byteSliceWriter
	SetOutput(&w)

	WithContext(ctx).Info("start")

	require.Equal(t, "[Info] [req-42] start\n", string(w.b))
}

// Test_WithContextTemplateFailureOmitsPartialContext locks in the scratch-buffer
// guarantee from M8: a tag that writes some bytes before erroring must not
// leak its prefix into the real log line. Instead, the render-error marker
// stands in for the entire context block.
func Test_WithContextTemplateFailureOmitsPartialContext(t *testing.T) {
	initDefaultLogger()

	templateErr := errors.New("template failure")
	require.NoError(t, SetContextTemplate(ContextConfig{
		Format: "[${broken}] ",
		CustomTags: map[string]ContextTagFunc{
			"broken": func(output Buffer, _ any, _ *ContextData, _ string) (int, error) {
				if _, err := output.WriteString("partial"); err != nil {
					return 0, err
				}
				return 0, templateErr
			},
		},
	}))
	t.Cleanup(func() { MustSetContextTemplate(ContextConfig{}) })

	var w byteSliceWriter
	SetOutput(&w)

	WithContext(context.Background()).Info("start")

	out := string(w.b)
	require.NotContains(t, out, "partial", "scratch buffer must isolate partial writes from the live log line")
	require.Contains(t, out, "ctx-render-error", "render error must be surfaced rather than silently dropped")
	require.Contains(t, out, "start", "log payload must still reach the writer")
}

func Test_LogfKeyAndValues(t *testing.T) {
	tests := []struct {
		name          string
		format        string
		wantOutput    string
		fmtArgs       []any
		keysAndValues []any
		level         Level
	}{
		{
			name:          "test logf with debug level and key-values",
			level:         LevelDebug,
			format:        "",
			fmtArgs:       nil,
			keysAndValues: []any{"name", "Bob", "age", 30},
			wantOutput:    "[Debug] name=Bob age=30\n",
		},
		{
			name:          "test logf with info level and key-values",
			level:         LevelInfo,
			format:        "",
			fmtArgs:       nil,
			keysAndValues: []any{"status", "ok", "code", 200},
			wantOutput:    "[Info] status=ok code=200\n",
		},
		{
			name:          "test logf with warn level and key-values",
			level:         LevelWarn,
			format:        "",
			fmtArgs:       nil,
			keysAndValues: []any{"error", "not found", "id", 123},
			wantOutput:    "[Warn] error=not found id=123\n",
		},
		{
			name:          "test logf with format and key-values",
			level:         LevelWarn,
			format:        "test",
			fmtArgs:       nil,
			keysAndValues: []any{"error", "not found", "id", 123},
			wantOutput:    "[Warn] test error=not found id=123\n",
		},
		{
			name:          "test logf with one key",
			level:         LevelWarn,
			format:        "",
			fmtArgs:       nil,
			keysAndValues: []any{"error"},
			wantOutput:    "[Warn] error=KEYVALS UNPAIRED\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			l := &defaultLogger{
				stdlog: log.New(&buf, "", 0),
				level:  tt.level,
				depth:  4,
			}
			l.privateLogw(tt.level, tt.format, tt.keysAndValues)
			require.Equal(t, tt.wantOutput, buf.String())
		})
	}
}

func Test_SetLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		want  string
		level Level
	}{
		{name: "trace", level: LevelTrace, want: "[Trace] "},
		{name: "debug", level: LevelDebug, want: "[Debug] "},
		{name: "info", level: LevelInfo, want: "[Info] "},
		{name: "warn", level: LevelWarn, want: "[Warn] "},
		{name: "error", level: LevelError, want: "[Error] "},
		{name: "fatal", level: LevelFatal, want: "[Fatal] "},
		{name: "panic", level: LevelPanic, want: "[Panic] "},
		{name: "unknown level", level: 8, want: "[?8] "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			setLogger := &defaultLogger{
				stdlog: log.New(os.Stderr, "", log.LstdFlags|log.Lshortfile|log.Lmicroseconds),
				depth:  4,
			}

			setLogger.SetLevel(tt.level)
			require.Equal(t, tt.level, setLogger.level)
			require.Equal(t, tt.want, setLogger.level.toString())
		})
	}
}

func Test_Logger(t *testing.T) {
	underlyingLogger := log.New(os.Stderr, "", log.LstdFlags|log.Lshortfile|log.Lmicroseconds)
	setLogger := &defaultLogger{
		stdlog: underlyingLogger,
		depth:  4,
	}

	require.Equal(t, underlyingLogger, setLogger.Logger())

	logger := setLogger.Logger()
	logger.SetFlags(log.LstdFlags | log.Lshortfile | log.Lmicroseconds)
	require.Equal(t, log.LstdFlags|log.Lshortfile|log.Lmicroseconds, setLogger.stdlog.Flags())
}

// Test_Logw runs serially because initDefaultLogger and SetOutput mutate the
// package-global logger shared with other log tests.
func Test_Logw(t *testing.T) {
	keysAndValues := []any{"key1", "value1", "key2", "value2"}

	tests := []struct {
		log    func(msg string, keysAndValues ...any)
		name   string
		msg    string
		want   string
		panics bool
	}{
		{name: "Tracew", log: Tracew, msg: "trace work", want: "[Trace] trace work key1=value1 key2=value2\n"},
		{name: "Debugw", log: Debugw, msg: "debug work", want: "[Debug] debug work key1=value1 key2=value2\n"},
		{name: "Infow", log: Infow, msg: "info work", want: "[Info] info work key1=value1 key2=value2\n"},
		{name: "Warnw", log: Warnw, msg: "warning work", want: "[Warn] warning work key1=value1 key2=value2\n"},
		{name: "Errorw", log: Errorw, msg: "error work", want: "[Error] error work key1=value1 key2=value2\n"},
		{name: "Panicw", log: Panicw, msg: "panic work", want: "[Panic] panic work key1=value1 key2=value2\n", panics: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			initDefaultLogger()

			var w byteSliceWriter
			SetOutput(&w)

			if tt.panics {
				require.Panics(t, func() {
					tt.log(tt.msg, keysAndValues...)
				})
			} else {
				tt.log(tt.msg, keysAndValues...)
			}

			require.Equal(t, tt.want, string(w.b))
		})
	}
}

type stringKey struct {
	value string
}

func (k stringKey) String() string {
	return "key:" + k.value
}

func Test_DefaultLoggerNonStringKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		log  func(l *defaultLogger)
		name string
		want string
	}{
		{
			name: "Tracew with non-string keys",
			log:  func(l *defaultLogger) { l.Tracew("trace", 123, "value", stringKey{value: "alpha"}, 42) },
			want: "[Trace] trace 123=value key:alpha=42\n",
		},
		{
			name: "Infow with non-string keys",
			log:  func(l *defaultLogger) { l.Infow("info", 456, "value", stringKey{value: "beta"}, 7) },
			want: "[Info] info 456=value key:beta=7\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			l := &defaultLogger{
				stdlog: log.New(&buf, "", 0),
				level:  LevelTrace,
				depth:  4,
			}

			require.NotPanics(t, func() {
				tt.log(l)
			})

			require.Equal(t, tt.want, buf.String())
		})
	}
}

func Benchmark_LogfKeyAndValues(b *testing.B) {
	tests := []struct {
		name          string
		format        string
		keysAndValues []any
		level         Level
	}{
		{
			name:          "test logf with debug level and key-values",
			level:         LevelDebug,
			format:        "",
			keysAndValues: []any{"name", "Bob", "age", 30},
		},
		{
			name:          "test logf with info level and key-values",
			level:         LevelInfo,
			format:        "",
			keysAndValues: []any{"status", "ok", "code", 200},
		},
		{
			name:          "test logf with warn level and key-values",
			level:         LevelWarn,
			format:        "",
			keysAndValues: []any{"error", "not found", "id", 123},
		},
		{
			name:          "test logf with format and key-values",
			level:         LevelWarn,
			format:        "test",
			keysAndValues: []any{"error", "not found", "id", 123},
		},
		{
			name:          "test logf with one key",
			level:         LevelWarn,
			format:        "",
			keysAndValues: []any{"error"},
		},
	}

	for _, tt := range tests {
		b.Run(tt.name, func(bb *testing.B) {
			var buf bytes.Buffer
			l := &defaultLogger{
				stdlog: log.New(&buf, "", 0),
				level:  tt.level,
				depth:  4,
			}

			bb.ReportAllocs()

			for bb.Loop() {
				l.privateLogw(tt.level, tt.format, tt.keysAndValues)
			}
		})
	}
}

func Benchmark_LogfKeyAndValues_Parallel(b *testing.B) {
	tests := []struct {
		name          string
		format        string
		keysAndValues []any
		level         Level
	}{
		{
			name:          "debug level with key-values",
			level:         LevelDebug,
			format:        "",
			keysAndValues: []any{"name", "Bob", "age", 30},
		},
		{
			name:          "info level with key-values",
			level:         LevelInfo,
			format:        "",
			keysAndValues: []any{"status", "ok", "code", 200},
		},
		{
			name:          "warn level with key-values",
			level:         LevelWarn,
			format:        "",
			keysAndValues: []any{"error", "not found", "id", 123},
		},
		{
			name:          "warn level with format and key-values",
			level:         LevelWarn,
			format:        "test",
			keysAndValues: []any{"error", "not found", "id", 123},
		},
		{
			name:          "warn level with one key",
			level:         LevelWarn,
			format:        "",
			keysAndValues: []any{"error"},
		},
	}

	for _, tt := range tests {
		b.Run(tt.name, func(bb *testing.B) {
			bb.ReportAllocs()
			bb.ResetTimer()
			bb.RunParallel(func(pb *testing.PB) {
				var buf bytes.Buffer
				l := &defaultLogger{
					stdlog: log.New(&buf, "", 0),
					level:  tt.level,
					depth:  4,
				}
				for pb.Next() {
					l.privateLogw(tt.level, tt.format, tt.keysAndValues)
				}
			})
		})
	}
}

func Test_DefaultLogger_FatalExitsAboveLevel(t *testing.T) {
	t.Parallel()

	var exited []int
	logger := &defaultLogger{
		stdlog: log.New(io.Discard, "", 0),
		exit:   func(code int) { exited = append(exited, code) },
		level:  LevelPanic,
		depth:  4,
	}

	logger.Fatal("fatal")
	logger.Fatalf("fatal %d", 1)
	logger.Fatalw("fatal", "k", "v")
	require.Equal(t, []int{1, 1, 1}, exited)

	// A derived logger keeps the hook, or it would exit the test process.
	derived, ok := logger.WithContext(t.Context()).(*defaultLogger)
	require.True(t, ok)
	derived.Fatal("fatal")
	require.Equal(t, []int{1, 1, 1, 1}, exited)
}

func Test_DefaultLogger_FatalExitsAfterWrite(t *testing.T) {
	t.Parallel()

	var exited []int
	var buf bytes.Buffer
	logger := &defaultLogger{
		stdlog: log.New(&buf, "", 0),
		exit:   func(code int) { exited = append(exited, code) },
		level:  LevelTrace,
		depth:  4,
	}

	logger.Fatal("fatal")
	logger.Fatalf("fatal %d", 1)
	logger.Fatalw("fatal", "k", "v")
	require.Equal(t, []int{1, 1, 1}, exited)
	require.Contains(t, buf.String(), "[Fatal] fatal")
}

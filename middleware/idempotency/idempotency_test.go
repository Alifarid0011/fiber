package idempotency

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/internal/apptest"
	"github.com/valyala/fasthttp"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validKey = "00000000-0000-0000-0000-000000000000"

func Test_Idempotency(t *testing.T) {
	t.Parallel()
	app := fiber.New()

	app.Use(func(c fiber.Ctx) error {
		if err := c.Next(); err != nil {
			return err
		}

		isMethodSafe := fiber.IsMethodSafe(c.Method())
		isIdempotent := IsFromCache(c) || WasPutToCache(c)
		hasReqHeader := c.Get("X-Idempotency-Key") != ""

		if isMethodSafe {
			if isIdempotent {
				return errors.New("request with safe HTTP method should not be idempotent")
			}
		} else {
			// Unsafe
			if hasReqHeader {
				if !isIdempotent {
					return errors.New("request with unsafe HTTP method should be idempotent if X-Idempotency-Key request header is set")
				}
			} else if isIdempotent {
				return errors.New("request with unsafe HTTP method should not be idempotent if X-Idempotency-Key request header is not set")
			}
		}

		return nil
	})

	// The memory storage expires entries on a coarse 1-second clock
	// (exp = uint32(ttl.Seconds()) + Timestamp()), so a freshly stored entry has
	// up to a full second less real headroom than its nominal lifetime. Keep the
	// lifetime comfortably above that 1-second granularity so the back-to-back
	// "store then replay" assertions below don't flake when a slow CI runner
	// pauses between two consecutive requests. It must also stay small enough
	// that the /slow handler's 3*lifetime sleep finishes within the 15s request
	// timeout used in doReq.
	const lifetime = 3 * time.Second

	app.Use(New(Config{Lifetime: lifetime}))

	nextCount := func() func() int {
		var count atomic.Int32
		return func() int { return int(count.Add(1)) }
	}()

	app.Add([]string{
		fiber.MethodGet,
		fiber.MethodPost,
		fiber.MethodQuery,
	}, "/", func(c fiber.Ctx) error { return c.SendString(strconv.Itoa(nextCount())) })

	app.Post("/slow", func(c fiber.Ctx) error {
		time.Sleep(3 * lifetime)

		return c.SendString(strconv.Itoa(nextCount()))
	})

	doReq := func(method, route, idempotencyKey string) string {
		req := httptest.NewRequest(method, route, http.NoBody)
		if idempotencyKey != "" {
			req.Header.Set("X-Idempotency-Key", idempotencyKey)
		}
		resp := apptest.Do(t, app, req, fiber.TestConfig{Timeout: 15 * time.Second, FailOnTimeout: true})
		body := apptest.Body(t, resp)
		require.Equal(t, fiber.StatusOK, resp.StatusCode, body)
		return body
	}

	require.Equal(t, "1", doReq(fiber.MethodGet, "/", ""))
	require.Equal(t, "2", doReq(fiber.MethodGet, "/", ""))

	require.Equal(t, "3", doReq(fiber.MethodPost, "/", ""))
	require.Equal(t, "4", doReq(fiber.MethodPost, "/", ""))

	require.Equal(t, "5", doReq(fiber.MethodGet, "/", validKey))
	require.Equal(t, "6", doReq(fiber.MethodGet, "/", validKey))

	require.Equal(t, "7", doReq(fiber.MethodPost, "/", validKey))
	require.Equal(t, "7", doReq(fiber.MethodPost, "/", validKey))
	require.Equal(t, "8", doReq(fiber.MethodPost, "/", ""))
	require.Equal(t, "9", doReq(fiber.MethodPost, "/", "11111111-1111-1111-1111-111111111111"))

	require.Equal(t, "7", doReq(fiber.MethodPost, "/", validKey))
	time.Sleep(4 * lifetime)
	require.Equal(t, "10", doReq(fiber.MethodPost, "/", validKey))
	require.Equal(t, "10", doReq(fiber.MethodPost, "/", validKey))

	// Test raciness
	{
		var wg sync.WaitGroup
		for range 100 {
			wg.Go(func() {
				assert.Equal(t, "11", doReq(fiber.MethodPost, "/slow", "22222222-2222-2222-2222-222222222222"))
			})
		}
		wg.Wait()
		require.Equal(t, "11", doReq(fiber.MethodPost, "/slow", "22222222-2222-2222-2222-222222222222"))
	}
	time.Sleep(3 * lifetime)
	require.Equal(t, "12", doReq(fiber.MethodPost, "/slow", "22222222-2222-2222-2222-222222222222"))

	// QUERY is safe (RFC 10008), so idempotency is skipped: each request runs the
	// handler and increments the counter even with an idempotency key set.
	require.Equal(t, "13", doReq(fiber.MethodQuery, "/", "33333333-3333-3333-3333-333333333333"))
	require.Equal(t, "14", doReq(fiber.MethodQuery, "/", "33333333-3333-3333-3333-333333333333"))
}

func Benchmark_Idempotency(b *testing.B) {
	app := fiber.New()

	// Needs to be at least a second as the memory storage doesn't support shorter durations.
	const lifetime = 1 * time.Second

	app.Use(New(Config{Lifetime: lifetime}))

	app.Post("/", func(_ fiber.Ctx) error { return nil })

	h := app.Handler()

	// post returns a context holding a POST / with the header pairs kv.
	post := func(kv ...string) *fasthttp.RequestCtx {
		c := &fasthttp.RequestCtx{}
		c.Request.Header.SetMethod(fiber.MethodPost)
		c.Request.SetRequestURI("/")
		for i := 0; i+1 < len(kv); i += 2 {
			c.Request.Header.Set(kv[i], kv[i+1])
		}
		return c
	}

	b.Run("hit", func(b *testing.B) {
		c := post("X-Idempotency-Key", validKey)

		b.ReportAllocs()
		for b.Loop() {
			h(c)
		}
	})

	b.Run("skip", func(b *testing.B) {
		c := post()

		b.ReportAllocs()
		for b.Loop() {
			h(c)
		}
	})

	scoped := fiber.New()
	scoped.Use(New(Config{Lifetime: lifetime, KeyScope: func(c fiber.Ctx) string { return c.Get("X-User") }}))
	scoped.Post("/", func(_ fiber.Ctx) error { return nil })
	hs := scoped.Handler()

	b.Run("scoped hit", func(b *testing.B) {
		c := post("X-Idempotency-Key", validKey, "X-User", "alice")

		b.ReportAllocs()
		for b.Loop() {
			hs(c)
		}
	})
}

func Benchmark_scopedKey(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		scopedKey("alice", validKey)
	}
}

func Test_configDefault_defaults(t *testing.T) {
	t.Parallel()
	cfg := configDefault()
	require.NotNil(t, cfg.Lock)
	require.NotNil(t, cfg.Storage)
	require.Equal(t, ConfigDefault.Lifetime, cfg.Lifetime)
	require.Equal(t, ConfigDefault.KeyHeader, cfg.KeyHeader)
	require.Nil(t, cfg.KeepResponseHeaders)
	require.Nil(t, cfg.KeyScope)

	app := fiber.New()
	for method, skipped := range map[string]bool{fiber.MethodGet: true, fiber.MethodPost: false} {
		fctx := &fasthttp.RequestCtx{}
		fctx.Request.Header.SetMethod(method)
		ctx := app.AcquireCtx(fctx)
		require.Equal(t, skipped, cfg.Next(ctx), method)
		app.ReleaseCtx(ctx)
	}

	require.NoError(t, cfg.KeyHeaderValidate(validKey))
	require.Error(t, cfg.KeyHeaderValidate("short"))
}

func Test_configDefault_override(t *testing.T) {
	t.Parallel()
	l := &stubLock{}
	s := &stubStorage{}

	cfg := configDefault(Config{
		Lifetime:            42 * time.Second,
		KeyHeader:           "Foo",
		KeepResponseHeaders: []string{},
		Lock:                l,
		Storage:             s,
		KeyScope:            func(fiber.Ctx) string { return "scope" },
	})

	require.Equal(t, 42*time.Second, cfg.Lifetime)
	require.Equal(t, "Foo", cfg.KeyHeader)
	require.NotNil(t, cfg.KeyScope)
	require.Nil(t, cfg.KeepResponseHeaders)
	require.Equal(t, l, cfg.Lock)
	require.Equal(t, s, cfg.Storage)
	require.NotNil(t, cfg.Next)
	require.NotNil(t, cfg.KeyHeaderValidate)
}

// idempotencyTestPost sends a POST request to route through app, carrying key
// as the idempotency key unless it is empty, and returns the response with its
// body. opts are applied to the request as well.
func idempotencyTestPost(t *testing.T, app *fiber.App, route, key string, opts ...apptest.Option) (resp *http.Response, body string) { //nolint:nonamedreturns // gocritic unnamedResult prefers naming returned response and body payload for clarity
	t.Helper()

	if key != "" {
		opts = append(opts, apptest.WithHeader(ConfigDefault.KeyHeader, key))
	}
	resp = apptest.Do(t, app, apptest.Req(fiber.MethodPost, route, opts...), fiber.TestConfig{Timeout: 5 * time.Second})
	return resp, apptest.Body(t, resp)
}

func Test_New_NextSkip(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	var count int

	app.Use(New(Config{Next: func(_ fiber.Ctx) bool { return true }}))

	app.Post("/", func(c fiber.Ctx) error {
		count++
		return c.SendString(strconv.Itoa(count))
	})

	_, body1 := idempotencyTestPost(t, app, "/", validKey)
	_, body2 := idempotencyTestPost(t, app, "/", validKey)

	require.Equal(t, "1", body1)
	require.Equal(t, "2", body2)
}

// Test_New_Errors covers the failures of the key, the storage and the lock: each
// ends the request with a 500 whose body names the step that failed.
func Test_New_Errors(t *testing.T) {
	t.Parallel()
	secondPassStorage := &stubStorage{}

	for _, tc := range []struct {
		name     string
		key      string
		contains string
		cfg      []Config // none: the default storage and lock
	}{
		{name: "invalid key", key: "bad", contains: "invalid length"},
		{
			name:     "storage get error",
			key:      validKey,
			cfg:      []Config{{Storage: &stubStorage{getErr: errors.New("boom")}, Lock: &stubLock{}}},
			contains: "failed to write cached response at fastpath",
		},
		{
			name:     "unmarshal error",
			key:      validKey,
			cfg:      []Config{{Storage: &stubStorage{data: map[string][]byte{validKey: []byte("bad")}}, Lock: &stubLock{}}},
			contains: "failed to write cached response at fastpath",
		},
		{
			name:     "lock error",
			key:      validKey,
			cfg:      []Config{{Lock: &stubLock{lockErr: errors.New("fail")}, Storage: &stubStorage{}}},
			contains: "failed to lock",
		},
		{
			name:     "storage set error",
			key:      validKey,
			cfg:      []Config{{Storage: &stubStorage{setErr: errors.New("nope")}, Lock: &stubLock{}}},
			contains: "failed to save response",
		},
		{
			// The entry is missing on the fast path and the read fails once the lock is held.
			name: "second pass read error",
			key:  validKey,
			cfg: []Config{{
				Lock:    &stubLock{afterLock: func() { secondPassStorage.getErr = errors.New("g") }},
				Storage: secondPassStorage,
			}},
			contains: "failed to write cached response while locked",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := fiber.New()
			app.Use(New(tc.cfg...))
			app.Post("/", func(c fiber.Ctx) error { return c.SendString("ok") })

			resp, body := idempotencyTestPost(t, app, "/", tc.key)
			require.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)
			require.Contains(t, body, tc.contains)
		})
	}
}

// Test_New_StoreRetrieve_FilterHeaders pins the keep-header filter: a replay carries only the headers
// KeepResponseHeaders names. Configured names match response header names ASCII case-insensitively
// (fasthttp reports canonical case like "Foo"), and the name list is snapshotted at New() time, so
// mutating the caller's slice afterwards cannot change an already-built handler.
func Test_New_StoreRetrieve_FilterHeaders(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		keep   []string
		mutate bool // the slice names "Bar" instead once New has returned
	}{
		{name: "exact name", keep: []string{"Foo"}},
		{name: "case insensitive snapshot", keep: []string{"fOO"}, mutate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := fiber.New()
			s := &stubStorage{}
			app.Use(New(Config{Storage: s, Lock: &stubLock{}, KeepResponseHeaders: tc.keep}))
			if tc.mutate {
				tc.keep[0] = "Bar"
			}

			var count int
			app.Post("/", func(c fiber.Ctx) error {
				count++
				c.Set("Foo", "foo")
				c.Set("Bar", "bar")
				return c.SendString(fmt.Sprintf("resp%d", count))
			})

			resp, body := idempotencyTestPost(t, app, "/", validKey)
			require.Equal(t, "resp1", body)
			require.Equal(t, "foo", resp.Header.Get("Foo"))
			require.Equal(t, "bar", resp.Header.Get("Bar"))

			resp2, body2 := idempotencyTestPost(t, app, "/", validKey)
			require.Equal(t, "resp1", body2)
			require.Equal(t, "foo", resp2.Header.Get("Foo"), "the configured name must match whatever its case")
			require.Empty(t, resp2.Header.Get("Bar"), "mutating the config slice after New must not change filtering")
			require.Equal(t, 1, count)
			require.Equal(t, 1, s.setCount)
		})
	}
}

// Test_New_StoreRetrieve_HeadersWithCommas pins that a replayed response
// carries each header value exactly as the handler set it, even when the app
// enables EnableSplittingOnParsers, which only concerns request parsing. The
// cookie is kept because the config names it.
func Test_New_StoreRetrieve_HeadersWithCommas(t *testing.T) {
	t.Parallel()
	app := fiber.New(fiber.Config{EnableSplittingOnParsers: true})
	app.Use(New(Config{
		Storage:             &stubStorage{},
		Lock:                &stubLock{},
		KeepResponseHeaders: []string{fiber.HeaderContentDisposition, fiber.HeaderSetCookie},
	}))

	var count int
	app.Post("/", func(c fiber.Ctx) error {
		count++
		c.Set(fiber.HeaderContentDisposition, `attachment; filename="a,b.txt"`)
		c.Cookie(&fiber.Cookie{
			Name:    "session",
			Value:   "abc",
			Expires: time.Date(2030, time.October, 21, 7, 28, 0, 0, time.UTC),
		})
		return c.SendString(fmt.Sprintf("resp%d", count))
	})

	resp, body := idempotencyTestPost(t, app, "/", validKey)
	require.Equal(t, "resp1", body)

	resp2, body2 := idempotencyTestPost(t, app, "/", validKey)
	require.Equal(t, "resp1", body2)
	require.Equal(t, 1, count)
	require.NotEmpty(t, resp2.Header.Values(fiber.HeaderSetCookie))
	require.Equal(t, resp.Header.Values(fiber.HeaderContentDisposition), resp2.Header.Values(fiber.HeaderContentDisposition))
	require.Equal(t, resp.Header.Values(fiber.HeaderSetCookie), resp2.Header.Values(fiber.HeaderSetCookie))
}

// Test_New_DoesNotReplayCookies pins that a cookie is neither stored nor
// replayed by default: the key alone identifies a request, so a replayed
// Set-Cookie hands the session of whoever sent the key first to whoever sends
// it next, on any route.
func Test_New_DoesNotReplayCookies(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	s := &stubStorage{}
	app.Use(New(Config{Storage: s, Lock: &stubLock{}}))

	var count int
	app.Post("/login", func(c fiber.Ctx) error {
		count++
		c.Cookie(&fiber.Cookie{Name: "sid", Value: "ALICE-SESSION"})
		c.Set(fiber.HeaderSetCookie2, "sid=ALICE-SESSION; Version=1")
		c.Set("X-Request", "alice")
		return c.SendString("welcome alice")
	})
	app.Post("/other", func(c fiber.Ctx) error {
		count++
		return c.SendString("other")
	})

	// the client that caused the miss gets its cookies
	resp, body := idempotencyTestPost(t, app, "/login", validKey)
	require.Equal(t, "welcome alice", body)
	require.Equal(t, []string{"sid=ALICE-SESSION; path=/; SameSite=Lax"}, resp.Header.Values(fiber.HeaderSetCookie))
	require.Equal(t, []string{"sid=ALICE-SESSION; Version=1"}, resp.Header.Values(fiber.HeaderSetCookie2))

	// whoever sends the key next gets the rest of the response, and the
	// handler is not run for them, on the route that stored it or another
	for _, route := range []string{"/login", "/other"} {
		resp, body = idempotencyTestPost(t, app, route, validKey)
		require.Equal(t, "welcome alice", body, route)
		require.Equal(t, "alice", resp.Header.Get("X-Request"), route)
		require.Empty(t, resp.Header.Values(fiber.HeaderSetCookie), route)
		require.Empty(t, resp.Header.Values(fiber.HeaderSetCookie2), route)
	}
	require.Equal(t, 1, count)

	// and nothing in the storage holds them
	for _, val := range s.data {
		require.NotContains(t, string(val), "ALICE-SESSION")
	}
}

// Test_New_KeepResponseHeaders_NamedCookies pins that naming a cookie header in
// KeepResponseHeaders stores and replays it, matched case-insensitively, and
// that naming one of the two leaves the other out.
func Test_New_KeepResponseHeaders_NamedCookies(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	app.Use(New(Config{
		Storage:             &stubStorage{},
		Lock:                &stubLock{},
		KeepResponseHeaders: []string{"set-cookie", "X-Request"},
	}))

	app.Post("/", func(c fiber.Ctx) error {
		c.Cookie(&fiber.Cookie{Name: "sid", Value: "abc"})
		c.Set(fiber.HeaderSetCookie2, "sid=abc; Version=1")
		c.Set("X-Request", "first")
		c.Set("X-Other", "dropped")
		return c.SendString("ok")
	})

	first, _ := idempotencyTestPost(t, app, "/", validKey)
	replayed, _ := idempotencyTestPost(t, app, "/", validKey)
	require.Equal(t, []string{"sid=abc; path=/; SameSite=Lax"}, first.Header.Values(fiber.HeaderSetCookie))
	require.Equal(t, first.Header.Values(fiber.HeaderSetCookie), replayed.Header.Values(fiber.HeaderSetCookie))
	require.Equal(t, "first", replayed.Header.Get("X-Request"))
	require.Empty(t, replayed.Header.Values(fiber.HeaderSetCookie2))
	require.Empty(t, replayed.Header.Values("X-Other"))
}

// Test_New_ReplayDropsStoredCookies pins that a cookie in an entry the storage
// already holds, written by a version that kept every header or under a
// configuration that named the cookie, is not replayed unless the
// configuration names it now.
func Test_New_ReplayDropsStoredCookies(t *testing.T) {
	t.Parallel()
	stored := func() *stubStorage {
		res := response{
			Headers: map[string][]string{
				fiber.HeaderSetCookie:  {"sid=ALICE-SESSION; path=/"},
				fiber.HeaderSetCookie2: {"sid=ALICE-SESSION; Version=1"},
				"X-Request":            {"alice"},
			},
			Body:       []byte("welcome alice"),
			StatusCode: fiber.StatusOK,
		}
		bs, err := res.MarshalMsg(nil)
		require.NoError(t, err)
		return &stubStorage{data: map[string][]byte{validKey: bs}}
	}
	replay := func(keep []string) *http.Response {
		app := fiber.New()
		app.Use(New(Config{Storage: stored(), Lock: &stubLock{}, KeepResponseHeaders: keep}))
		app.Post("/", func(_ fiber.Ctx) error { return errors.New("the handler must not run") })

		resp, body := idempotencyTestPost(t, app, "/", validKey)
		require.Equal(t, "welcome alice", body)
		require.Equal(t, "alice", resp.Header.Get("X-Request"))
		return resp
	}

	for _, keep := range [][]string{nil, {"X-Request"}} {
		resp := replay(keep)
		require.Empty(t, resp.Header.Values(fiber.HeaderSetCookie), keep)
		require.Empty(t, resp.Header.Values(fiber.HeaderSetCookie2), keep)
	}

	resp := replay([]string{"X-Request", fiber.HeaderSetCookie})
	require.Equal(t, []string{"sid=ALICE-SESSION; path=/"}, resp.Header.Values(fiber.HeaderSetCookie))
	require.Empty(t, resp.Header.Values(fiber.HeaderSetCookie2))
}

// Test_New_KeyScope pins that a key is stored, replayed and locked per scope: a
// user who sends the key another user sent runs the handler for their own
// request and gets their own response, cookie included, and the storage holds
// neither the key nor the scope.
func Test_New_KeyScope(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	s := &stubStorage{}
	l := &stubLock{}
	app.Use(New(Config{
		Storage:             s,
		Lock:                l,
		KeepResponseHeaders: []string{fiber.HeaderSetCookie},
		KeyScope:            func(c fiber.Ctx) string { return c.Get("X-User") },
	}))

	var count int
	app.Post("/", func(c fiber.Ctx) error {
		count++
		c.Cookie(&fiber.Cookie{Name: "sid", Value: c.Get("X-User") + "-session"})
		return c.SendString(fmt.Sprintf("%s:%d", c.Get("X-User"), count))
	})

	post := func(user string) (*http.Response, string) {
		var opts []apptest.Option
		if user != "" {
			opts = append(opts, apptest.WithHeader("X-User", user))
		}
		return idempotencyTestPost(t, app, "/", validKey, opts...)
	}
	requireReply := func(user, wantBody string, wantCalls int) {
		t.Helper()
		resp, body := post(user)
		require.Equal(t, wantBody, body)
		require.Equal(t, "sid="+user+"-session; path=/; SameSite=Lax", resp.Header.Get(fiber.HeaderSetCookie))
		require.Equal(t, wantCalls, count)
	}

	requireReply("alice", "alice:1", 1)
	requireReply("bob", "bob:2", 2) // the key is Alice's too, the response is not
	requireReply("alice", "alice:1", 2)
	requireReply("bob", "bob:2", 2)
	requireReply("", ":3", 3) // a request without a user is a scope of its own
	requireReply("", ":3", 3)

	require.Equal(t, 3, s.setCount)
	require.Len(t, s.data, 3)
	// Each entry holds its own user's cookie and nobody else's, under the key
	// the scope and the idempotency key digest to.
	for _, user := range []string{"alice", "bob", ""} {
		val := string(s.data[scopedKey(user, validKey)])
		require.Contains(t, val, "sid="+user+"-session")
		for _, other := range []string{"alice", "bob"} {
			if other != user {
				require.NotContains(t, val, other+"-session")
			}
		}
	}
	// A replay is answered before the lock, so only the three misses took it,
	// under the same keys, and a key two users share makes neither wait for
	// the other.
	require.Len(t, l.locked, 3)
	for _, key := range l.locked {
		require.Contains(t, s.data, key)
	}
}

// Test_New_KeyScope_NotConsulted pins that the scope is asked for only when a
// request carries a valid key.
func Test_New_KeyScope_NotConsulted(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	var asked int
	app.Use(New(Config{
		Storage:  &stubStorage{},
		Lock:     &stubLock{},
		KeyScope: func(fiber.Ctx) string { asked++; return "user" },
	}))
	app.Post("/", func(c fiber.Ctx) error { return c.SendString("ok") })

	idempotencyTestPost(t, app, "/", "")
	require.Zero(t, asked, "no key")

	resp, _ := idempotencyTestPost(t, app, "/", "bad")
	require.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)
	require.Zero(t, asked, "invalid key")

	idempotencyTestPost(t, app, "/", validKey)
	require.Equal(t, 1, asked)
}

// Test_scopedKey pins that a scope and a key give one storage key, of one
// length, that no other pair gives, however their bytes run together.
func Test_scopedKey(t *testing.T) {
	t.Parallel()
	first := scopedKey("alice", validKey)
	require.Equal(t, first, scopedKey("alice", validKey))
	require.Len(t, scopedKey("", ""), 64)
	require.Len(t, scopedKey(strings.Repeat("s", 1000), strings.Repeat("k", 1000)), 64)
	require.NotEqual(t, validKey, scopedKey("", validKey))

	seen := make(map[string][2]string)
	for _, pair := range [][2]string{
		{"", ""},
		{"", "abc"},
		{"a", "bc"},
		{"ab", "c"},
		{"abc", ""},
		{"1", ":ab"},
		{"1:a", "b"},
		{"3", ":abc"},
		{"3:abc", ""},
		{"0", ":abc"},
		{"10", ":"},
		{"1", "0:"},
		{"alice", validKey},
		{"bob", validKey},
		{"alice" + validKey[:1], validKey[1:]},
	} {
		key := scopedKey(pair[0], pair[1])
		prev, dup := seen[key]
		require.False(t, dup, "%q and %q share a key", pair, prev)
		seen[key] = pair
	}
}

func Test_New_Cache_WhenBodyTooLarge(t *testing.T) {
	t.Parallel()
	bodyLimit := 8
	app := fiber.New(fiber.Config{BodyLimit: bodyLimit})
	s := &stubStorage{}
	var wasPut []bool

	app.Use(func(c fiber.Ctx) error {
		if err := c.Next(); err != nil {
			return err
		}
		wasPut = append(wasPut, WasPutToCache(c))
		return nil
	})
	app.Use(New(Config{Storage: s, Lock: &stubLock{}}))

	var count int
	oversized := strings.Repeat("a", bodyLimit+1)
	app.Post("/", func(c fiber.Ctx) error {
		count++
		return c.SendString(oversized)
	})

	for range 2 {
		resp, body := idempotencyTestPost(t, app, "/", validKey)
		require.Equal(t, fiber.StatusOK, resp.StatusCode)
		require.Equal(t, oversized, body)
	}

	require.Equal(t, 1, count)
	require.Equal(t, 1, s.setCount)
	require.Len(t, wasPut, 2)
	require.True(t, wasPut[0])
	require.False(t, wasPut[1])
}

func Test_New_HandlerError(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	s := &stubStorage{}
	app.Use(New(Config{Storage: s, Lock: &stubLock{}}))
	app.Post("/", func(_ fiber.Ctx) error { return errors.New("boom") })

	for range 2 {
		resp, body := idempotencyTestPost(t, app, "/", validKey)
		require.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)
		require.Equal(t, "boom", body)
		require.Equal(t, 0, s.setCount)
	}
}

func Test_New_UnlockError(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	l := &stubLock{unlockErr: errors.New("u")}
	app.Use(New(Config{Lock: l, Storage: &stubStorage{}}))
	app.Post("/", func(c fiber.Ctx) error { return c.SendString("ok") })

	resp, body := idempotencyTestPost(t, app, "/", validKey)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	require.Equal(t, "ok", body)
}

// The response is stored after the handler returns, so a handler's own deferred
// cancel must not reach storage through Ctx.Err().
func Test_Idempotency_Store_After_Handler_Cancel(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	app.Use(New())

	var calls int
	app.Post("/", func(c fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(c.Context(), time.Hour)
		defer cancel()
		c.SetContext(ctx)

		calls++
		return c.SendString("response")
	})

	_, body := idempotencyTestPost(t, app, "/", validKey)
	require.Equal(t, "response", body)
	_, body = idempotencyTestPost(t, app, "/", validKey)
	require.Equal(t, "response", body)
	require.Equal(t, 1, calls, "second request was not served from the idempotency cache")
}

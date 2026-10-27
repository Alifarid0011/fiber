package session

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/extractors"
	"github.com/gofiber/fiber/v3/internal/apptest"
	"github.com/gofiber/fiber/v3/internal/clocktest"
	"github.com/gofiber/fiber/v3/internal/storage/memory"
	"github.com/gofiber/utils/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func Test_Session(t *testing.T) {
	t.Parallel()

	// session store
	store := NewStore()

	// fiber instance
	app := fiber.New()

	// fiber context
	ctx := app.AcquireCtx(&fasthttp.RequestCtx{})

	// Get a new session
	sess, err := store.Get(ctx)
	require.NoError(t, err)
	require.True(t, sess.Fresh())
	token := sess.ID()
	require.NoError(t, sess.Save())

	sess.Release()
	app.ReleaseCtx(ctx)
	ctx = app.AcquireCtx(&fasthttp.RequestCtx{})

	// set session using default cookie extractor
	ctx.Request().Header.SetCookie("session_id", token)

	// get session
	sess, err = store.Get(ctx)
	require.NoError(t, err)
	require.False(t, sess.Fresh())

	// get keys
	keys := sess.Keys()
	require.Equal(t, []any{}, keys)

	// get value
	name := sess.Get("name")
	require.Nil(t, name)

	// set value
	sess.Set("name", "john")

	// get value
	name = sess.Get("name")
	require.Equal(t, "john", name)

	keys = sess.Keys()
	require.Equal(t, []any{"name"}, keys)

	// delete key
	sess.Delete("name")

	// get value
	name = sess.Get("name")
	require.Nil(t, name)

	// get keys
	keys = sess.Keys()
	require.Equal(t, []any{}, keys)

	// get id
	id := sess.ID()
	require.Equal(t, token, id)

	// save the old session first
	err = sess.Save()
	require.NoError(t, err)

	// release the session
	sess.Release()
	// release the context
	app.ReleaseCtx(ctx)

	// requesting entirely new context to prevent falsy tests
	ctx = app.AcquireCtx(&fasthttp.RequestCtx{})

	sess, err = store.Get(ctx)
	require.NoError(t, err)
	require.True(t, sess.Fresh())

	// this id should be randomly generated as session key was deleted
	require.Len(t, sess.ID(), 43)

	sess.Release()

	// when we use the original session for the second time
	// the session be should be same if the session is not expired
	app.ReleaseCtx(ctx)
	ctx = app.AcquireCtx(&fasthttp.RequestCtx{})
	defer app.ReleaseCtx(ctx)

	// request the server with the old session
	ctx.Request().Header.SetCookie("session_id", id)
	sess, err = store.Get(ctx)
	defer sess.Release()
	require.NoError(t, err)
	require.False(t, sess.Fresh())
	require.Equal(t, sess.id, id)
}

// sessionTestBasicValues is one value of each basic type that a session stores
// without registering it, named after its type.
var sessionTestBasicValues = []struct {
	value any
	name  string
}{
	{name: "bool", value: true},
	{name: "string", value: "str"},
	{name: "int", value: 13},
	{name: "int8", value: int8(13)},
	{name: "int16", value: int16(13)},
	{name: "int32", value: int32(13)},
	{name: "int64", value: int64(13)},
	{name: "uint", value: uint(13)},
	{name: "uint8", value: uint8(13)},
	{name: "uint16", value: uint16(13)},
	{name: "uint32", value: uint32(13)},
	{name: "uint64", value: uint64(13)},
	{name: "uintptr", value: uintptr(13)},
	{name: "byte", value: byte('k')},
	{name: "rune", value: 'k'},
	{name: "float32", value: float32(13)},
	{name: "float64", value: float64(13)},
	{name: "complex64", value: complex64(13)},
	{name: "complex128", value: complex128(13)},
}

func Test_Session_Types(t *testing.T) {
	t.Parallel()

	// session store
	store := NewStore()

	// fiber instance
	app := fiber.New()

	// fiber context
	ctx := apptest.Ctx(t, app)

	// set cookie
	ctx.Request().Header.SetCookie("session_id", "123")

	// get session
	sess, err := store.Get(ctx)
	require.NoError(t, err)
	require.True(t, sess.Fresh())

	// the session string is no longer be 123
	newSessionIDString := sess.ID()

	type User struct {
		Name string
	}
	store.RegisterType(User{})
	vuser := User{
		Name: "John",
	}
	// set value
	sess.Set("vuser", vuser)
	for _, v := range sessionTestBasicValues {
		sess.Set("v"+v.name, v.value)
	}

	// save session
	err = sess.Save()
	require.NoError(t, err)

	sess.Release()
	sess = loadSession(t, app, store, newSessionIDString)
	require.False(t, sess.Fresh())

	// get value, which must come back with the type it went in with
	require.Equal(t, vuser, sess.Get("vuser"))
	for _, v := range sessionTestBasicValues {
		require.Equal(t, v.value, sess.Get("v"+v.name), v.name)
	}
}

func Test_Session_Store_Reset(t *testing.T) {
	t.Parallel()
	// session store
	store := NewStore()
	// fiber instance
	app := fiber.New()
	// fiber context
	ctx := apptest.Ctx(t, app)

	// get session
	sess, err := store.Get(ctx)
	require.NoError(t, err)
	// make sure its new
	require.True(t, sess.Fresh())
	// set value & save
	sess.Set("hello", "world")
	ctx.Request().Header.SetCookie("session_id", sess.ID())
	require.NoError(t, sess.Save())

	// reset store
	require.NoError(t, store.Reset(ctx))
	id := sess.ID()

	sess.Release()

	// make sure the session is recreated
	sess = loadSession(t, app, store, id)
	require.True(t, sess.Fresh())
	require.Nil(t, sess.Get("hello"))
}

func Test_Session_KeyTypes(t *testing.T) {
	// Note: This test cannot run in parallel because it registers types
	// in the global gob registry via store.RegisterType(), which would
	// cause race conditions with other parallel tests.

	// session store
	store := NewStore()
	// fiber instance
	app := fiber.New()
	// fiber context
	ctx := apptest.Ctx(t, app)

	// get session
	sess, err := store.Get(ctx)
	require.NoError(t, err)
	require.True(t, sess.Fresh())

	type Person struct {
		Name string
	}

	type unexportedKey int

	// register non-default types
	store.RegisterType(Person{})
	store.RegisterType(unexportedKey(0))

	type unregisteredKeyType int
	type unregisteredValueType int

	// verify unregistered keys types are not allowed
	var (
		unregisteredKey   unregisteredKeyType
		unregisteredValue unregisteredValueType
	)
	sess.Set(unregisteredKey, "test")
	err = sess.Save()
	require.Error(t, err)
	sess.Delete(unregisteredKey)
	err = sess.Save()
	require.NoError(t, err)
	sess.Set("abc", unregisteredValue)
	err = sess.Save()
	require.Error(t, err)
	sess.Delete("abc")
	err = sess.Save()
	require.NoError(t, err)

	require.NoError(t, sess.Reset())

	// Release session before continuing
	sess.Release()

	// Get a new session after reset
	sess, err = store.Get(ctx)
	require.NoError(t, err)
	require.True(t, sess.Fresh())

	// every basic type and the registered ones, each stored as a key holding a value equal to it
	keys := make([]any, 0, len(sessionTestBasicValues)+2)
	for _, v := range sessionTestBasicValues {
		keys = append(keys, v.value)
	}
	keys = append(keys, Person{Name: "John"}, unexportedKey(13))

	// loop test all key value pairs
	for _, key := range keys {
		sess.Set(key, key)
	}

	id := sess.ID()
	ctx.Request().Header.SetCookie("session_id", id)
	// save session
	err = sess.Save()
	require.NoError(t, err)

	sess.Release()

	// get session
	sess = loadSession(t, app, store, id)
	require.False(t, sess.Fresh())

	// loop test all key value pairs
	for _, key := range keys {
		// get value
		require.Equal(t, key, sess.Get(key))
	}
}

// Saving writes the session ID to every sink of the store's extractor that can
// be written back to: the cookie of a cookie extractor and the response header
// of a header one, whatever the chain around them. A source that is read-only
// (query, form) is never written.
func Test_Session_Save(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		header    string // the response header that has to carry the ID; empty if none does
		name      string
		extractor extractors.Extractor // the zero value is the default, the cookie "session_id"
		cookie    bool                 // whether the "session_id" cookie has to carry the ID
	}{
		{name: "save to cookie", cookie: true},
		{name: "save to header", extractor: extractors.FromHeader("session_id"), header: "session_id"},
		// Chained extractors: both the cookie and the header are set when both are present.
		{
			name:      "cookie and header chain",
			extractor: extractors.Chain(extractors.FromCookie("session_id"), extractors.FromHeader("x-session-id")),
			cookie:    true,
			header:    "x-session-id",
		},
		{
			name:      "header and cookie chain",
			extractor: extractors.Chain(extractors.FromHeader("x-session-id"), extractors.FromCookie("session_id")),
			cookie:    true,
			header:    "x-session-id",
		},
		// Only query/form extractors: nothing is set on the response.
		{
			name:      "only SourceOther extractors - no response setting",
			extractor: extractors.Chain(extractors.FromQuery("session_id"), extractors.FromForm("session_id")),
		},
		// The query is ignored for the response, the cookie and the header are not.
		{
			name:      "mixed chain with SourceOther",
			extractor: extractors.Chain(extractors.FromCookie("session_id"), extractors.FromQuery("session_id"), extractors.FromHeader("x-session-id")),
			cookie:    true,
			header:    "x-session-id",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sess, ctx := sessionTestFresh(t, NewStore(Config{Extractor: tc.extractor}))
			// set value
			sess.Set("name", "john")

			// save session
			err := sess.Save()
			require.NoError(t, err)

			cookie := ctx.Response().Header.PeekCookie("session_id")
			if tc.cookie {
				require.NotNil(t, cookie)
				require.Contains(t, string(cookie), sess.ID())
			} else {
				require.Nil(t, cookie)
			}

			if tc.header != "" {
				require.Equal(t, sess.ID(), string(ctx.Response().Header.Peek(tc.header)))
			} else {
				require.Empty(t, string(ctx.Response().Header.Peek("session_id")))
			}
		})
	}
}

// Test_Session_ExtractorPreservation verifies that the extractor which actually
// supplied the incoming session ID drives the write-back decision, preventing
// a session ID read from a read-only source (query/form/param) from being
// promoted into cookies/headers for an existing session (session fixation).
func Test_Session_ExtractorPreservation(t *testing.T) {
	t.Parallel()

	t.Run("read-only winner is not promoted for existing session", func(t *testing.T) {
		t.Parallel()
		// Chain prefers the cookie, but also accepts a read-only query source.
		store := NewStore(Config{
			Extractor: extractors.Chain(extractors.FromCookie("session_id"), extractors.FromQuery("session_id")),
		})
		app := fiber.New()

		// First request: create and persist a session (written to the cookie).
		ctx1 := apptest.Ctx(t, app)
		sess, err := store.Get(ctx1)
		require.NoError(t, err)
		sess.Set("name", "john")
		require.NoError(t, sess.Save())
		id := sess.ID()
		require.NotNil(t, ctx1.Response().Header.PeekCookie("session_id"))
		sess.Release()

		// Second request: the existing ID is provided only via the query string
		// (an attacker-controllable, read-only source).
		ctx2 := apptest.Ctx(t, app)
		ctx2.Request().SetRequestURI("/path?session_id=" + id)

		sess2, err := store.Get(ctx2)
		require.NoError(t, err)
		require.False(t, sess2.Fresh(), "existing session must not be fresh")
		require.Equal(t, id, sess2.ID())
		require.NoError(t, sess2.Save())

		// The read-only ID must NOT be promoted into a cookie/header.
		require.Nil(t, ctx2.Response().Header.PeekCookie("session_id"))
		require.Empty(t, string(ctx2.Response().Header.Peek("session_id")))
		sess2.Release()
	})

	t.Run("fresh session still persists to writable sink", func(t *testing.T) {
		t.Parallel()
		store := NewStore(Config{
			Extractor: extractors.Chain(extractors.FromCookie("session_id"), extractors.FromQuery("session_id")),
		})
		app := fiber.New()

		// The query supplies an ID that does not exist in storage, so a fresh
		// session with a freshly generated ID is created.
		ctx := apptest.Ctx(t, app)
		ctx.Request().SetRequestURI("/path?session_id=does-not-exist")

		sess, err := store.Get(ctx)
		require.NoError(t, err)
		require.True(t, sess.Fresh(), "missing data must yield a fresh session")
		require.NotEqual(t, "does-not-exist", sess.ID(), "fresh session must use a generated ID")
		require.NoError(t, sess.Save())

		// A freshly generated ID is safe to persist to the configured cookie sink.
		cookie := ctx.Response().Header.PeekCookie("session_id")
		require.NotNil(t, cookie)
		require.Contains(t, string(cookie), sess.ID())
		require.NotContains(t, string(cookie), "does-not-exist")
		sess.Release()
	})

	t.Run("cookie winner writes back to the same cookie", func(t *testing.T) {
		t.Parallel()
		store := NewStore(Config{
			Extractor: extractors.Chain(extractors.FromCookie("session_id"), extractors.FromHeader("x-session-id")),
		})
		app := fiber.New()

		// Persist a session first.
		id := seedSession(t, app, store, nil)

		// Provide the existing ID via the cookie source.
		ctx2 := apptest.Ctx(t, app)
		ctx2.Request().Header.SetCookie("session_id", id)

		sess2, err := store.Get(ctx2)
		require.NoError(t, err)
		require.False(t, sess2.Fresh())
		require.NoError(t, sess2.Save())

		cookie := ctx2.Response().Header.PeekCookie("session_id")
		require.NotNil(t, cookie)
		require.Contains(t, string(cookie), id)
		sess2.Release()
	})
}

// Test_Session_getExtractorInfo exercises the extractor-resolution branches
// directly, including the nil-config safe default and single (non-chained)
// configured extractors.
func Test_Session_getExtractorInfo(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		sess *Session
		name string
		want []extractors.Extractor // the writable sinks, of which only Source and Key are compared
	}{
		{
			name: "nil config returns safe cookie default",
			sess: &Session{},
			want: []extractors.Extractor{{Source: extractors.SourceCookie, Key: "session_id"}},
		},
		{
			name: "single cookie extractor",
			sess: &Session{config: NewStore(Config{Extractor: extractors.FromCookie("session_id")})},
			want: []extractors.Extractor{{Source: extractors.SourceCookie, Key: "session_id"}},
		},
		{
			name: "single header extractor",
			sess: &Session{config: NewStore(Config{Extractor: extractors.FromHeader("x-session-id")})},
			want: []extractors.Extractor{{Source: extractors.SourceHeader, Key: "x-session-id"}},
		},
		{
			name: "single read-only extractor yields no writable sink",
			sess: &Session{config: NewStore(Config{Extractor: extractors.FromQuery("session_id")})},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.sess.getExtractorInfo()
			require.Len(t, got, len(tc.want))
			for i, want := range tc.want {
				require.Equal(t, want.Source, got[i].Source)
				require.Equal(t, want.Key, got[i].Key)
			}
		})
	}
}

func Test_Session_Save_IdleTimeout(t *testing.T) {
	t.Parallel()

	t.Run("save to cookie", func(t *testing.T) {
		t.Parallel()

		const sessionDuration = 5 * time.Second
		// session store
		store := NewStore()
		// fiber instance
		app := fiber.New()
		// fiber context
		ctx := app.AcquireCtx(&fasthttp.RequestCtx{})
		defer app.ReleaseCtx(ctx)

		// get session
		sess, err := store.Get(ctx)
		require.NoError(t, err)

		// set value
		sess.Set("name", "john")

		token := sess.ID()

		// expire this session in 5 seconds
		sess.SetIdleTimeout(sessionDuration)

		// save session
		err = sess.Save()
		require.NoError(t, err)

		sess.Release()
		app.ReleaseCtx(ctx)
		ctx = app.AcquireCtx(&fasthttp.RequestCtx{})

		// here you need to get the old session yet
		ctx.Request().Header.SetCookie("session_id", token)
		sess, err = store.Get(ctx)
		require.NoError(t, err)
		require.Equal(t, token, sess.ID(), "session ID should match before expiration")
		name := sess.Get("name")
		require.Equal(t, "john", name, "session should contain the saved value before expiration")

		// Wait on the cached clock the storage compares against, not just the
		// wall clock, or a late timestamp updater keeps the session alive.
		clocktest.SleepPast(t, sessionDuration)

		sess.Release()

		app.ReleaseCtx(ctx)
		ctx = app.AcquireCtx(&fasthttp.RequestCtx{})
		defer app.ReleaseCtx(ctx)

		// here you should get a new session
		ctx.Request().Header.SetCookie("session_id", token)
		sess, err = store.Get(ctx)
		defer sess.Release()
		require.NoError(t, err)
		require.Nil(t, sess.Get("name"))
		require.NotEqual(t, sess.ID(), token)
	})
}

func Test_Session_Save_AbsoluteTimeout(t *testing.T) {
	t.Parallel()

	t.Run("save to cookie", func(t *testing.T) {
		t.Parallel()

		const absoluteTimeout = 2 * time.Second // extra headroom to avoid flakiness under -race
		// session store
		store := NewStore(Config{
			IdleTimeout:     absoluteTimeout,
			AbsoluteTimeout: absoluteTimeout,
		})

		// force change to IdleTimeout
		store.IdleTimeout = 10 * time.Second

		// fiber instance
		app := fiber.New()
		// fiber context
		ctx := app.AcquireCtx(&fasthttp.RequestCtx{})
		defer app.ReleaseCtx(ctx)

		// get session
		sess, err := store.Get(ctx)
		require.NoError(t, err)

		// set value
		sess.Set("name", "john")

		token := sess.ID()

		// save session
		err = sess.Save()
		require.NoError(t, err)

		sess.Release()
		app.ReleaseCtx(ctx)
		ctx = app.AcquireCtx(&fasthttp.RequestCtx{})

		// here you need to get the old session yet
		ctx.Request().Header.SetCookie("session_id", token)
		sess, err = store.Get(ctx)
		require.NoError(t, err)
		require.Equal(t, "john", sess.Get("name"))

		// just to make sure the session has been expired
		time.Sleep(absoluteTimeout + (200 * time.Millisecond))

		sess.Release()

		app.ReleaseCtx(ctx)
		ctx = app.AcquireCtx(&fasthttp.RequestCtx{})

		// here you should get a new session
		ctx.Request().Header.SetCookie("session_id", token)
		sess, err = store.Get(ctx)
		require.NoError(t, err)
		require.Nil(t, sess.Get("name"))
		require.NotEqual(t, sess.ID(), token)
		require.True(t, sess.Fresh())
		require.IsType(t, time.Time{}, sess.Get(absExpirationKey))

		token = sess.ID()

		sess.Set("name", "john")

		// save session
		err = sess.Save()
		require.NoError(t, err)

		sess.Release()
		app.ReleaseCtx(ctx)

		// just to make sure the session has been expired
		time.Sleep(absoluteTimeout + (200 * time.Millisecond))

		// try to get expired session by id
		sess, err = store.GetByID(ctx, token)
		require.Error(t, err)
		require.ErrorIs(t, err, ErrSessionIDNotFoundInStore)
		require.Nil(t, sess)
	})
}

func Test_Session_Destroy(t *testing.T) {
	t.Parallel()

	t.Run("destroy from cookie", func(t *testing.T) {
		t.Parallel()
		// get session
		sess, _ := sessionTestFresh(t, NewStore())

		sess.Set("name", "fenny")
		require.NoError(t, sess.Destroy())
		name := sess.Get("name")
		require.Nil(t, name)
	})

	t.Run("destroy from header", func(t *testing.T) {
		t.Parallel()
		// session store
		store := NewStore(Config{
			Extractor: extractors.FromHeader("session_id"),
		})
		// fiber instance
		app := fiber.New()

		// set value & save
		id := seedSession(t, app, store, func(sess *Session) { sess.Set("name", "fenny") })

		// get session
		ctx := apptest.Ctx(t, app)
		ctx.Request().Header.Set("session_id", id)
		sess, err := store.Get(ctx)
		require.NoError(t, err)
		defer sess.Release()

		err = sess.Destroy()
		require.NoError(t, err)
		require.Empty(t, string(ctx.Response().Header.Peek("session_id")))
	})
}

func Test_Session_Custom_Config(t *testing.T) {
	t.Parallel()

	store := NewStore(Config{IdleTimeout: time.Hour, KeyGenerator: func() string { return "very random" }})
	require.Equal(t, time.Hour, store.IdleTimeout)
	require.Equal(t, "very random", store.KeyGenerator())

	store = NewStore(Config{IdleTimeout: 0})
	require.Equal(t, ConfigDefault.IdleTimeout, store.IdleTimeout)
}

func Test_Session_Cookie(t *testing.T) {
	t.Parallel()
	// get session
	sess, ctx := sessionTestFresh(t, NewStore())
	require.NoError(t, sess.Save())

	// cookie should be set on Save ( even if empty data )
	cookie := ctx.Response().Header.PeekCookie("session_id")
	require.NotNil(t, cookie)
	require.Regexp(t, `^session_id=[A-Za-z0-9\-_]{43}; max-age=\d+; path=/; SameSite=Lax$`, string(cookie))
}

// Test_Session_Cookie_SameSite verifies every supported mode's emitted
// attribute and interaction with the Secure flag.
func Test_Session_Cookie_SameSite(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		sameSite         string
		expectedSameSite string
		initialSecure    bool
		expectSecure     bool
	}{
		{
			name:             "Lax should not force secure",
			sameSite:         "Lax",
			expectedSameSite: "SameSite=Lax",
		},
		{
			name:             "Lax with secure should stay secure",
			sameSite:         "Lax",
			expectedSameSite: "SameSite=Lax",
			initialSecure:    true,
			expectSecure:     true,
		},
		{
			name:             "Strict should not force secure",
			sameSite:         "Strict",
			expectedSameSite: "SameSite=Strict",
		},
		{
			name:             "Strict with secure should stay secure",
			sameSite:         "Strict",
			expectedSameSite: "SameSite=Strict",
			initialSecure:    true,
			expectSecure:     true,
		},
		{
			name:             "None should force secure",
			sameSite:         "None",
			expectedSameSite: "SameSite=None",
			expectSecure:     true,
		},
		{
			name:             "None with secure should stay secure",
			sameSite:         "None",
			expectedSameSite: "SameSite=None",
			initialSecure:    true,
			expectSecure:     true,
		},
		{
			name:             "Case-insensitive none should force secure",
			sameSite:         "none",
			expectedSameSite: "SameSite=None",
			expectSecure:     true,
		},
		{
			name:             "Case-insensitive strict should not force secure",
			sameSite:         "strict",
			expectedSameSite: "SameSite=Strict",
		},
		{
			name:     "Disabled should omit SameSite",
			sameSite: fiber.CookieSameSiteDisabled,
		},
		{
			name:          "Case-insensitive disabled should preserve secure",
			sameSite:      "DISABLED",
			initialSecure: true,
			expectSecure:  true,
		},
		{
			name:             "Empty should default to Lax",
			expectedSameSite: "SameSite=Lax",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// session store
			store := NewStore(Config{
				CookieSameSite: tc.sameSite,
				CookieSecure:   tc.initialSecure,
			})

			// get session
			sess, ctx := sessionTestFresh(t, store)

			// save session to trigger cookie setting
			err := sess.Save()
			require.NoError(t, err)

			// check cookie
			cookie := string(ctx.Response().Header.PeekCookie("session_id"))
			if tc.expectedSameSite == "" {
				require.NotContains(t, cookie, "SameSite")
			} else {
				require.Contains(t, cookie, tc.expectedSameSite)
			}

			if tc.expectSecure {
				require.Contains(t, cookie, "secure")
			} else {
				require.NotContains(t, cookie, "secure")
			}
		})
	}
}

// Regression: https://github.com/gofiber/fiber/pull/1191
func Test_Session_Cookie_In_Middleware_Chain(t *testing.T) {
	store := NewStore()

	// fiber context
	ctx := apptest.Ctx(t, fiber.New())

	// get session
	sess, err := store.Get(ctx)
	require.NoError(t, err)
	sess.Set("id", "1")
	require.True(t, sess.Fresh())
	id := sess.ID()
	require.NoError(t, sess.Save())

	sess.Release()

	sess, err = store.Get(ctx)
	require.NoError(t, err)
	defer sess.Release()
	sess.Set("name", "john")
	require.False(t, sess.Fresh())  // Session should not be fresh - it reuses the same ID from context locals
	require.Equal(t, id, sess.ID()) // session id should be the same

	require.Equal(t, "1", sess.Get("id"))
	require.Equal(t, "john", sess.Get("name"))
}

// Regression: https://github.com/gofiber/fiber/issues/1365
func Test_Session_Deletes_Single_Key(t *testing.T) {
	t.Parallel()
	store := NewStore()
	app := fiber.New()

	id := seedSession(t, app, store, func(sess *Session) { sess.Set("id", "1") })

	sess := loadSession(t, app, store, id)
	sess.Delete("id")
	require.NoError(t, sess.Save())

	sess = loadSession(t, app, store, id)
	require.False(t, sess.Fresh())
	require.Nil(t, sess.Get("id"))
}

func Test_Session_Reset(t *testing.T) {
	t.Parallel()
	// fiber instance
	app := fiber.New()

	// session store
	store := NewStore()

	t.Run("reset session data and id, and set fresh to be true", func(t *testing.T) {
		t.Parallel()

		// now the session is in the storage, with a value set
		originalSessionUUIDString := seedSession(t, app, store, func(sess *Session) {
			sess.Set("name", "fenny")
			sess.Set("email", "fenny@example.com")
		})

		// as the session is in the storage, session.fresh should be false
		acquiredSession := loadSession(t, app, store, originalSessionUUIDString)
		require.False(t, acquiredSession.Fresh())

		err := acquiredSession.Reset()
		require.NoError(t, err)

		require.NotEqual(t, originalSessionUUIDString, acquiredSession.ID())

		// acquiredSession.fresh should be true after resetting
		require.True(t, acquiredSession.Fresh())

		// Check that the session data has been reset
		keys := acquiredSession.Keys()
		require.Equal(t, []any{}, keys)

		// Set a new value for 'name' and check that it's updated
		acquiredSession.Set("name", "john")
		require.Equal(t, "john", acquiredSession.Get("name"))
		require.Nil(t, acquiredSession.Get("email"))

		// Save after resetting
		err = acquiredSession.Save()
		require.NoError(t, err)

		// Check that the session id is not in the header or cookie anymore
		ctx := acquiredSession.ctx
		require.Empty(t, string(ctx.Response().Header.Peek("session_id")))
		require.Empty(t, string(ctx.Request().Header.Peek("session_id")))
	})
}

func Test_Session_Reset_KeepsAbsoluteTimeout(t *testing.T) {
	t.Parallel()

	const absoluteTimeout = time.Second

	// newSessionStore keeps the storage TTL out of the way; only the absolute timeout expires the session.
	newSessionStore := func() *Store {
		store := NewStore(Config{
			IdleTimeout:     absoluteTimeout,
			AbsoluteTimeout: absoluteTimeout,
		})
		store.IdleTimeout = 10 * time.Second
		return store
	}

	testCases := []struct {
		rotate func(*Session) error
		name   string
	}{
		{name: "Reset arms a new absolute expiration", rotate: (*Session).Reset},
		{name: "Regenerate keeps the absolute expiration", rotate: (*Session).Regenerate},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := newSessionStore()
			app := fiber.New()

			ctx := app.AcquireCtx(&fasthttp.RequestCtx{})
			sess, err := store.Get(ctx)
			require.NoError(t, err)
			require.True(t, sess.Fresh())
			sess.Set("name", "john")
			require.NoError(t, sess.Save())
			token := sess.ID()
			sess.Release()
			app.ReleaseCtx(ctx)

			ctx = app.AcquireCtx(&fasthttp.RequestCtx{})
			ctx.Request().Header.SetCookie("session_id", token)
			sess, err = store.Get(ctx)
			require.NoError(t, err)
			require.False(t, sess.Fresh())
			require.NoError(t, tc.rotate(sess))
			require.NotEqual(t, token, sess.ID())
			require.IsType(t, time.Time{}, sess.Get(absExpirationKey), "the rotated session must carry an absolute expiration")
			sess.Set("name", "jane")
			require.NoError(t, sess.Save())
			token = sess.ID()
			sess.Release()
			app.ReleaseCtx(ctx)

			time.Sleep(absoluteTimeout + 200*time.Millisecond)

			ctx = app.AcquireCtx(&fasthttp.RequestCtx{})
			defer app.ReleaseCtx(ctx)
			ctx.Request().Header.SetCookie("session_id", token)
			sess, err = store.Get(ctx)
			require.NoError(t, err)
			defer sess.Release()
			require.True(t, sess.Fresh(), "the rotated session must expire absolutely")
			require.Nil(t, sess.Get("name"))
			require.NotEqual(t, token, sess.ID())
		})
	}
}

// Regression: https://github.com/gofiber/fiber/issues/1395
func Test_Session_Regenerate(t *testing.T) {
	t.Parallel()
	// fiber instance
	app := fiber.New()
	t.Run("set fresh to be true when regenerating a session", func(t *testing.T) {
		t.Parallel()
		// session store
		store := NewStore()

		// now the session is in the storage
		originalSessionUUIDString := seedSession(t, app, store, nil)

		// as the session is in the storage, session.fresh should be false
		acquiredSession := loadSession(t, app, store, originalSessionUUIDString)
		require.False(t, acquiredSession.Fresh())

		err := acquiredSession.Regenerate()
		require.NoError(t, err)

		require.NotEqual(t, originalSessionUUIDString, acquiredSession.ID())

		// acquiredSession.fresh should be true after regenerating
		require.True(t, acquiredSession.Fresh())
	})
}

func Benchmark_Session(b *testing.B) {
	b.Run("default", func(b *testing.B) {
		app, store := fiber.New(), NewStore()
		c := apptest.Ctx(b, app)
		c.Request().Header.SetCookie("session_id", "12356789")

		b.ReportAllocs()
		for b.Loop() {
			sess, _ := store.Get(c) //nolint:errcheck // We're inside a benchmark
			sess.Set("john", "doe")
			_ = sess.Save() //nolint:errcheck // We're inside a benchmark

			sess.Release()
		}
	})

	b.Run("storage", func(b *testing.B) {
		app := fiber.New()
		store := NewStore(Config{
			Storage: memory.New(),
		})
		c := apptest.Ctx(b, app)
		c.Request().Header.SetCookie("session_id", "12356789")

		b.ReportAllocs()
		for b.Loop() {
			sess, _ := store.Get(c) //nolint:errcheck // We're inside a benchmark
			sess.Set("john", "doe")
			_ = sess.Save() //nolint:errcheck // We're inside a benchmark

			sess.Release()
		}
	})
}

// Benchmark_Session_ChainExtractor measures the lookup a chain-configured store
// does per request. The user values are reset every iteration because a reused
// context answers from the cached session-ID local and never reaches the
// extractor.
func Benchmark_Session_ChainExtractor(b *testing.B) {
	app := fiber.New()
	store := NewStore(Config{
		Extractor: extractors.Chain(
			extractors.FromHeader("X-Session"),
			extractors.FromCookie("session_id"),
		),
	})

	fctx := &fasthttp.RequestCtx{}
	c := apptest.CtxFor(b, app, fctx)
	c.Request().Header.SetCookie("session_id", "12356789")

	b.ReportAllocs()
	for b.Loop() {
		fctx.ResetUserValues()

		sess, _ := store.Get(c) //nolint:errcheck // We're inside a benchmark
		sess.Set("john", "doe")
		_ = sess.Save() //nolint:errcheck // We're inside a benchmark

		sess.Release()
	}
}

func Benchmark_Session_Parallel(b *testing.B) {
	b.Run("default", func(b *testing.B) {
		app, store := fiber.New(), NewStore()
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				c := app.AcquireCtx(&fasthttp.RequestCtx{})
				c.Request().Header.SetCookie("session_id", "12356789")

				sess, _ := store.Get(c) //nolint:errcheck // We're inside a benchmark
				sess.Set("john", "doe")
				_ = sess.Save() //nolint:errcheck // We're inside a benchmark

				sess.Release()

				app.ReleaseCtx(c)
			}
		})
	})

	b.Run("storage", func(b *testing.B) {
		app := fiber.New()
		store := NewStore(Config{
			Storage: memory.New(),
		})
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				c := app.AcquireCtx(&fasthttp.RequestCtx{})
				c.Request().Header.SetCookie("session_id", "12356789")

				sess, _ := store.Get(c) //nolint:errcheck // We're inside a benchmark
				sess.Set("john", "doe")
				_ = sess.Save() //nolint:errcheck // We're inside a benchmark

				sess.Release()

				app.ReleaseCtx(c)
			}
		})
	})
}

func Benchmark_Session_Asserted(b *testing.B) {
	b.Run("default", func(b *testing.B) {
		app, store := fiber.New(), NewStore()
		c := apptest.Ctx(b, app)
		c.Request().Header.SetCookie("session_id", "12356789")

		b.ReportAllocs()
		for b.Loop() {
			sess, err := store.Get(c)
			require.NoError(b, err)
			sess.Set("john", "doe")
			err = sess.Save()
			require.NoError(b, err)
			sess.Release()
		}
	})

	b.Run("storage", func(b *testing.B) {
		app := fiber.New()
		store := NewStore(Config{
			Storage: memory.New(),
		})
		c := apptest.Ctx(b, app)
		c.Request().Header.SetCookie("session_id", "12356789")

		b.ReportAllocs()
		for b.Loop() {
			sess, err := store.Get(c)
			require.NoError(b, err)
			sess.Set("john", "doe")
			err = sess.Save()
			require.NoError(b, err)
			sess.Release()
		}
	})
}

func Benchmark_Session_Asserted_Parallel(b *testing.B) {
	b.Run("default", func(b *testing.B) {
		app, store := fiber.New(), NewStore()
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				c := app.AcquireCtx(&fasthttp.RequestCtx{})
				c.Request().Header.SetCookie("session_id", "12356789")

				sess, err := store.Get(c)
				require.NoError(b, err)
				sess.Set("john", "doe")
				require.NoError(b, sess.Save())
				sess.Release()
				app.ReleaseCtx(c)
			}
		})
	})

	b.Run("storage", func(b *testing.B) {
		app := fiber.New()
		store := NewStore(Config{
			Storage: memory.New(),
		})
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				c := app.AcquireCtx(&fasthttp.RequestCtx{})
				c.Request().Header.SetCookie("session_id", "12356789")

				sess, err := store.Get(c)
				require.NoError(b, err)
				sess.Set("john", "doe")
				require.NoError(b, sess.Save())
				sess.Release()
				app.ReleaseCtx(c)
			}
		})
	})
}

func Test_Session_Concurrency(t *testing.T) {
	app := fiber.New()
	store := NewStore()

	var wg sync.WaitGroup
	errChan := make(chan error, 10) // Buffered channel to collect errors
	const numGoroutines = 10        // Number of concurrent goroutines to test

	// Start numGoroutines goroutines
	for range numGoroutines {
		wg.Go(func() {
			localCtx := app.AcquireCtx(&fasthttp.RequestCtx{})

			sess, err := store.getSession(localCtx)
			if err != nil {
				errChan <- err
				return
			}

			// Set a value
			sess.Set("name", "john")

			// get the session id
			id := sess.ID()

			// Check if the session is fresh
			if !sess.Fresh() {
				errChan <- errors.New("session should be fresh")
				return
			}

			// Save the session
			if saveErr := sess.Save(); saveErr != nil {
				errChan <- saveErr
				return
			}

			// release the session
			sess.Release()

			// Release the context
			app.ReleaseCtx(localCtx)

			// Acquire a new context
			localCtx = app.AcquireCtx(&fasthttp.RequestCtx{})
			defer app.ReleaseCtx(localCtx)

			// Set the session id in the header
			localCtx.Request().Header.SetCookie("session_id", id)

			// Get the session
			sess, err = store.Get(localCtx)
			if err != nil {
				errChan <- err
				return
			}
			defer sess.Release()

			// Get the value
			name := sess.Get("name")
			if name != "john" {
				errChan <- errors.New("name should be john")
				return
			}

			// Get ID from the session
			if sess.ID() != id {
				errChan <- errors.New("id should be the same")
				return
			}

			// Check if the session is fresh
			if sess.Fresh() {
				errChan <- errors.New("session should not be fresh")
				return
			}

			// Delete the key
			sess.Delete("name")

			// Get the value
			name = sess.Get("name")
			if name != nil {
				errChan <- errors.New("name should be nil")
				return
			}

			// Destroy the session
			if err := sess.Destroy(); err != nil {
				errChan <- err
				return
			}
		})
	}

	wg.Wait()      // Wait for all goroutines to finish
	close(errChan) // Close the channel to signal no more errors will be sent

	// Check for errors sent to errChan
	for err := range errChan {
		require.NoError(t, err)
	}
}

func Test_Session_StoreGetDecodeSessionDataError(t *testing.T) {
	// Initialize a new store with default config
	store := NewStore()

	// Create a new Fiber app
	app := fiber.New()

	// Generate a fake session ID
	sessionID := uuid.New().String()

	// Store invalid session data to simulate decode error
	err := store.Storage.Set(sessionID, []byte("invalid data"), 0)
	require.NoError(t, err, "Failed to set invalid session data")

	// Create a new request context
	c := apptest.Ctx(t, app)

	// Set the session ID in cookies
	c.Request().Header.SetCookie("session_id", sessionID)

	// Attempt to get the session
	_, err = store.Get(c)
	require.Error(t, err, "Expected error due to invalid session data, but got nil")

	// Check that the error message is as expected
	require.Contains(t, err.Error(), "failed to decode session data", "Unexpected error message")

	// Check that the error is as expected
	require.ErrorContains(t, err, "failed to decode session data", "Unexpected error")

	// Attempt to get the session by ID
	_, err = store.GetByID(c, sessionID)
	require.Error(t, err, "Expected error due to invalid session data, but got nil")

	// Check that the error message is as expected
	require.ErrorContains(t, err, "failed to decode session data", "Unexpected error")
}

// This test verifies the fix for the fresh flag bug where calling getSession()
// multiple times in the same request would incorrectly mark the session as fresh
// when the ID was found in context locals.
func Test_Session_Fresh_Flag_Bug(t *testing.T) {
	t.Parallel()

	store := NewStore()
	app := fiber.New()

	// Test Case 1: First call with no session cookie - should be fresh
	sess1, _ := sessionTestFresh(t, store)
	require.True(t, sess1.Fresh(), "First session should be fresh (no cookie provided)")
	sessionID := sess1.ID()
	require.NoError(t, sess1.Save())

	// Test Case 2: Second call with session cookie - should NOT be fresh
	ctx2 := apptest.Ctx(t, app)
	ctx2.Request().Header.SetCookie("session_id", sessionID)
	sess2, err := store.Get(ctx2)
	require.NoError(t, err)
	require.False(t, sess2.Fresh(), "Existing session should not be fresh")
	require.Equal(t, sessionID, sess2.ID())

	// Test Case 3: Call getSession() again in the same request
	// This simulates what happens when CSRF middleware calls store operations
	// The session ID is now in context locals from the first getSession() call
	sess3, err := store.getSession(ctx2)
	require.NoError(t, err)
	require.False(t, sess3.Fresh(), "Session should still not be fresh on second getSession() call in same request")
	require.Equal(t, sessionID, sess3.ID())

	sess2.Release()
	sess3.Release()

	// Test Case 4: Expired session - should generate new ID and be fresh
	ctx3 := apptest.Ctx(t, app)
	ctx3.Request().Header.SetCookie("session_id", "expired-or-nonexistent-id")
	sess4, err := store.Get(ctx3)
	require.NoError(t, err)
	require.True(t, sess4.Fresh(), "New session (after expired/missing data) should be fresh")
	require.NotEqual(t, "expired-or-nonexistent-id", sess4.ID(), "Should have generated a new session ID")

	sess4.Release()
}

// This test simulates the user-reported issue with CSRF + session middleware
// where a POST without CSRF token would result in a new session_id cookie
func Test_Session_CSRF_Scenario(t *testing.T) {
	t.Parallel()

	const idleTimeout = 2 * time.Second // Longer timeout to ensure session persists

	store := NewStore(Config{
		IdleTimeout: idleTimeout,
	})
	app := fiber.New()

	// Simulate: First GET request creates session
	ctx1 := app.AcquireCtx(&fasthttp.RequestCtx{})
	sess1, err := store.Get(ctx1)
	require.NoError(t, err)
	require.True(t, sess1.Fresh())
	firstSessionID := sess1.ID()

	// Store some data (simulating CSRF token storage)
	sess1.Set("csrf_token", "token-123")
	require.NoError(t, sess1.Save())
	sess1.Release()
	app.ReleaseCtx(ctx1)

	// Small delay to ensure save completes
	time.Sleep(10 * time.Millisecond)

	// Simulate: POST request with valid session (before expiration)
	ctx2 := app.AcquireCtx(&fasthttp.RequestCtx{})
	ctx2.Request().Header.SetCookie("session_id", firstSessionID)
	sess2, err := store.Get(ctx2)
	require.NoError(t, err)
	require.False(t, sess2.Fresh(), "Session should not be fresh - it exists")
	require.Equal(t, firstSessionID, sess2.ID(), "Session ID should remain the same")
	require.Equal(t, "token-123", sess2.Get("csrf_token"))

	// Simulate CSRF validation failure (session is accessed but request fails)
	// Session should still maintain the same ID
	require.Equal(t, firstSessionID, sess2.ID())
	sess2.Release()
	app.ReleaseCtx(ctx2)

	// Wait for session to expire on the cached clock the storage compares against
	clocktest.SleepPast(t, idleTimeout)

	// Simulate: POST request with expired session
	// This is the scenario the user reported - session data is gone
	ctx3 := apptest.Ctx(t, app)
	ctx3.Request().Header.SetCookie("session_id", firstSessionID)
	sess3, err := store.Get(ctx3)
	require.NoError(t, err)
	require.True(t, sess3.Fresh(), "Session should be fresh - old data expired")
	require.NotEqual(t, firstSessionID, sess3.ID(), "Should have generated new session ID (expected behavior)")
	require.Nil(t, sess3.Get("csrf_token"), "Old session data should be gone")

	sess3.Release()
}

// This test ensures that calling getSession() multiple times within the same
// request context doesn't incorrectly mark the session as fresh due to the
// session ID being stored in context locals
func Test_Session_Multiple_GetSession_Calls(t *testing.T) {
	t.Parallel()

	store := NewStore()
	app := fiber.New()

	// Create initial session
	sess1, _ := sessionTestFresh(t, store)
	require.True(t, sess1.Fresh())
	sessionID := sess1.ID()
	sess1.Set("test_key", "test_value")
	require.NoError(t, sess1.Save())

	// New request with existing session
	ctx2 := apptest.Ctx(t, app)
	ctx2.Request().Header.SetCookie("session_id", sessionID)

	// First getSession() call - loads from storage
	sess2, err := store.getSession(ctx2)
	require.NoError(t, err)
	require.False(t, sess2.Fresh(), "First call: existing session should not be fresh")
	require.Equal(t, sessionID, sess2.ID())
	require.Equal(t, "test_value", sess2.Get("test_key"))

	// Second getSession() call - ID now in context locals
	// This is where the bug would manifest before the fix
	sess3, err := store.getSession(ctx2)
	require.NoError(t, err)
	require.False(t, sess3.Fresh(), "Second call: session should STILL not be fresh (bug fix verification)")
	require.Equal(t, sessionID, sess3.ID())
	require.Equal(t, "test_value", sess3.Get("test_key"))

	// Third call to ensure consistency
	sess4, err := store.getSession(ctx2)
	require.NoError(t, err)
	require.False(t, sess4.Fresh(), "Third call: session should remain not fresh")
	require.Equal(t, sessionID, sess4.ID())

	sess2.Release()
	sess3.Release()
	sess4.Release()
}

func Test_Session_DestroyWithContext(t *testing.T) {
	t.Parallel()

	t.Run("destroy with context from cookie", func(t *testing.T) {
		t.Parallel()
		sess, _ := sessionTestFresh(t, NewStore())

		sess.Set("name", "fenny")
		require.NoError(t, sess.DestroyWithContext(t.Context()))
		name := sess.Get("name")
		require.Nil(t, name)
	})

	t.Run("destroy with nil data", func(t *testing.T) {
		t.Parallel()
		sess := &Session{data: nil}
		require.NoError(t, sess.DestroyWithContext(t.Context()))
	})
}

func Test_Session_RegenerateWithContext(t *testing.T) {
	t.Parallel()

	t.Run("regenerate with context preserves data pattern", func(t *testing.T) {
		t.Parallel()
		store := NewStore()
		app := fiber.New()

		originalID := seedSession(t, app, store, func(sess *Session) { sess.Set("name", "fenny") })

		sess := loadSession(t, app, store, originalID)

		err := sess.RegenerateWithContext(t.Context())
		require.NoError(t, err)
		require.NotEqual(t, originalID, sess.ID())
		require.True(t, sess.Fresh())
		// Regenerate must preserve session data (unlike Reset, which clears it).
		require.Equal(t, "fenny", sess.Get("name"))
	})
}

func Test_Session_ResetWithContext(t *testing.T) {
	t.Parallel()

	t.Run("reset with context clears data and changes id", func(t *testing.T) {
		t.Parallel()
		sess, _ := sessionTestFresh(t, NewStore())

		originalID := sess.ID()
		sess.Set("name", "fenny")
		sess.Set("email", "fenny@example.com")

		err := sess.Save()
		require.NoError(t, err)

		err = sess.ResetWithContext(t.Context())
		require.NoError(t, err)

		require.NotEqual(t, originalID, sess.ID())
		require.True(t, sess.Fresh())
		require.Nil(t, sess.Get("name"))
		require.Nil(t, sess.Get("email"))
	})
}

func Test_Session_SaveWithContext(t *testing.T) {
	t.Parallel()

	t.Run("save with context persists data", func(t *testing.T) {
		t.Parallel()
		store := NewStore()
		sess, _ := sessionTestFresh(t, store)

		sess.Set("name", "fenny")
		err := sess.SaveWithContext(t.Context())
		require.NoError(t, err)

		// Verify data persisted by getting a new session with same ID
		sess2 := loadSession(t, fiber.New(), store, sess.ID())

		require.Equal(t, "fenny", sess2.Get("name"))
	})

	t.Run("save with context when nil data", func(t *testing.T) {
		t.Parallel()
		sess := &Session{data: nil}
		require.NoError(t, sess.SaveWithContext(t.Context()))
	})

	t.Run("save with context is a no-op when in middleware handler", func(t *testing.T) {
		t.Parallel()
		// Back the session with a storage that errors on any write: a real save
		// would surface that error, so a nil return proves no write happened.
		sess := newSessionWithStorage(t, &failingStorage{err: errors.New("Set must not be called")})
		sess.Set("name", "fenny")

		// Simulate the session being in use by the middleware handler.
		m := &Middleware{Session: sess}
		sess.ctx.Locals(middlewareContextKey, m)

		// SaveWithContext must be a no-op when the session is in use by middleware.
		require.NoError(t, sess.SaveWithContext(t.Context()))
	})
}

func Test_Session_ResolveContext(t *testing.T) {
	t.Parallel()

	t.Run("resolve context returns background when ctx is nil", func(t *testing.T) {
		t.Parallel()
		sess := &Session{ctx: nil}
		ctx := sess.resolveContext()
		require.NotNil(t, ctx)
	})

	t.Run("resolve context returns fiber context when ctx is not nil", func(t *testing.T) {
		t.Parallel()
		fctx := apptest.Ctx(t, fiber.New())

		sess := &Session{ctx: fctx}
		ctx := sess.resolveContext()
		require.Equal(t, fctx, ctx)
	})
}

// sessionTestFresh starts a session with store on a blank request and returns
// it with the request context. Both are released when the test ends, so the
// test must not release either itself.
func sessionTestFresh(t *testing.T, store *Store) (*Session, fiber.Ctx) {
	t.Helper()

	ctx := apptest.Ctx(t, fiber.New())
	sess, err := store.Get(ctx)
	require.NoError(t, err)
	t.Cleanup(sess.Release)
	return sess, ctx
}

// newSessionWithStorage returns a fresh session backed by the given storage.
// Because the request has no session cookie, Get does not read from storage, so
// it succeeds even when the storage backend is unavailable. Pooled resources
// (the session and fiber ctx) are released automatically via t.Cleanup.
func newSessionWithStorage(t *testing.T, storage fiber.Storage) *Session {
	t.Helper()

	sess, _ := sessionTestFresh(t, NewStore(Config{Storage: storage}))
	return sess
}

// Test_Session_WithContext runs every *WithContext operation against each way
// the context variants have to cope with a storage backend: a storage that
// fails, a context that is already canceled or past its deadline, and a nil
// context.
func Test_Session_WithContext(t *testing.T) {
	t.Parallel()

	storageErr := errors.New("storage unavailable")

	operations := []struct {
		call          func(*Session, context.Context) error
		name          string
		hasData       bool // the session holds a value when the call is made
		failKeepsData bool // a call that fails must not wipe that value from the in-memory session
	}{
		{name: "DestroyWithContext", call: (*Session).DestroyWithContext, hasData: true, failKeepsData: true},
		{name: "RegenerateWithContext", call: (*Session).RegenerateWithContext},
		{name: "ResetWithContext", call: (*Session).ResetWithContext, hasData: true, failKeepsData: true},
		{name: "SaveWithContext", call: (*Session).SaveWithContext, hasData: true},
	}

	conditions := []struct {
		storage func() fiber.Storage
		context func(t *testing.T) context.Context
		err     error // what every call must return, or nil when it has to succeed without panicking
		name    string
	}{
		{
			name:    "propagates storage error",
			storage: func() fiber.Storage { return &failingStorage{err: storageErr} },
			context: func(t *testing.T) context.Context {
				t.Helper()
				return t.Context()
			},
			err: storageErr,
		},
		{
			// contextStorage honors cancellation by returning ctx.Err().
			name:    "honors canceled context",
			storage: func() fiber.Storage { return &contextStorage{} },
			context: func(*testing.T) context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			err: context.Canceled,
		},
		{
			// contextStorage honors a deadline by returning ctx.Err(). An already-expired
			// deadline yields context.DeadlineExceeded, the primary motivation for the
			// *WithContext variants (bounding slow or unresponsive backends).
			name:    "honors expired deadline",
			storage: func() fiber.Storage { return &contextStorage{} },
			context: func(t *testing.T) context.Context {
				t.Helper()
				ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
				t.Cleanup(cancel)
				return ctx
			},
			err: context.DeadlineExceeded,
		},
		{
			// A nil context must be normalized to context.Background() instead of
			// panicking when forwarded to storage (see PR #4393 review feedback).
			name:    "with nil context does not panic",
			storage: func() fiber.Storage { return memory.New() },
			context: func(*testing.T) context.Context { return nil },
		},
	}

	for _, condition := range conditions {
		for _, op := range operations {
			t.Run(op.name+" "+condition.name, func(t *testing.T) {
				t.Parallel()
				sess := newSessionWithStorage(t, condition.storage())
				if op.hasData {
					sess.Set("name", "fenny")
				}

				var err error
				require.NotPanics(t, func() { err = op.call(sess, condition.context(t)) })
				if condition.err == nil {
					require.NoError(t, err)
					return
				}
				require.ErrorIs(t, err, condition.err)
				if op.failKeepsData {
					// A failed, canceled or timed-out delete must not wipe the in-memory session.
					require.Equal(t, "fenny", sess.Get("name"))
				}
			})
		}
	}
}

// recordingStorage logs every Set so a test can assert what a save actually
// wrote, then delegates to an in-memory backend.
type recordingStorage struct {
	fiber.Storage
	setLog []storageWrite
	mu     sync.Mutex
}

type storageWrite struct {
	key   string
	value []byte
	exp   time.Duration
}

func newRecordingStorage() *recordingStorage {
	return &recordingStorage{Storage: memory.New()}
}

func (s *recordingStorage) SetWithContext(ctx context.Context, key string, val []byte, exp time.Duration) error {
	s.mu.Lock()
	s.setLog = append(s.setLog, storageWrite{key: key, value: utils.CopyBytes(val), exp: exp})
	s.mu.Unlock()
	return s.Storage.SetWithContext(ctx, key, val, exp)
}

func (s *recordingStorage) Set(key string, val []byte, exp time.Duration) error {
	return s.SetWithContext(context.Background(), key, val, exp)
}

func (s *recordingStorage) writes() []storageWrite {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.setLog)
}

func (s *recordingStorage) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setLog = nil
}

// seedSession stores a session through the given store and returns its ID.
// fill may be nil to store a session without data.
func seedSession(t *testing.T, app *fiber.App, store *Store, fill func(*Session)) string {
	t.Helper()
	ctx := apptest.Ctx(t, app)

	sess, err := store.Get(ctx)
	require.NoError(t, err)
	if fill != nil {
		fill(sess)
	}
	require.NoError(t, sess.Save())
	id := sess.ID()
	sess.Release()
	return id
}

// loadSession returns the stored session for id through a request carrying its
// cookie, which is the path that retains the decoded snapshot. The session and
// the request context are released when the test ends.
func loadSession(t *testing.T, app *fiber.App, store *Store, id string) *Session {
	t.Helper()
	ctx := apptest.Ctx(t, app)
	ctx.Request().Header.SetCookie("session_id", id)

	sess, err := store.Get(ctx)
	require.NoError(t, err)
	t.Cleanup(sess.Release)
	return sess
}

// sessionTestStored returns the stored session for id through Store.GetByID,
// which leaves it without a request, and releases it when the test ends.
func sessionTestStored(t *testing.T, store *Store, id string) *Session {
	t.Helper()

	sess, err := store.GetByID(t.Context(), id)
	require.NoError(t, err)
	t.Cleanup(sess.Release)
	return sess
}

// A save that changed nothing still has to refresh storage: the sliding idle
// timeout is the entire safety argument for reusing the decoded snapshot.
func Test_Session_CleanSave_WritesSnapshotToStorage(t *testing.T) {
	t.Parallel()

	const idleTimeout = 2 * time.Hour

	app := fiber.New()
	rec := newRecordingStorage()
	store := NewStore(Config{Storage: rec, IdleTimeout: idleTimeout})

	id := seedSession(t, app, store, func(sess *Session) {
		sess.Set("user", "fenny")
		sess.Set("admin", true)
	})

	seeded := rec.writes()
	require.Len(t, seeded, 1)
	rec.reset()

	sess := loadSession(t, app, store, id)
	require.Equal(t, "fenny", sess.Get("user"))
	require.False(t, sess.data.dirty.Load(), "a scalar read must leave the session clean")
	sess.data.Data["admin"] = false // bypasses Set, so only a re-encode would persist it
	require.NoError(t, sess.Save())

	written := rec.writes()
	require.Len(t, written, 1, "a clean save must still write to storage")
	require.Equal(t, id, written[0].key)
	require.Equal(t, seeded[0].value, written[0].value, "the retained snapshot must be written back verbatim")
	require.Equal(t, idleTimeout, written[0].exp, "a clean save must slide the idle timeout")

	reloaded := sessionTestStored(t, store, id)
	require.Equal(t, "fenny", reloaded.Get("user"))
	require.Equal(t, true, reloaded.Get("admin"))
}

// A value the caller still holds can be mutated in place without Set, so any
// save after it was handed out must re-encode rather than reuse the snapshot.
func Test_Session_AliasedValueMutatedBetweenSaves(t *testing.T) {
	t.Parallel()

	newStore := func() (*fiber.App, *Store) {
		app := fiber.New()
		store := NewStore(Config{Storage: memory.New()})
		store.RegisterType(map[string]string{})
		return app, store
	}

	t.Run("mutated through Get", func(t *testing.T) {
		t.Parallel()

		app, store := newStore()
		id := seedSession(t, app, store, func(sess *Session) { sess.Set("prefs", map[string]string{"theme": "dark"}) })

		sess := loadSession(t, app, store, id)
		prefs, ok := sess.Get("prefs").(map[string]string)
		require.True(t, ok)
		prefs["theme"] = "light"
		require.NoError(t, sess.Save())

		reloaded := sessionTestStored(t, store, id)
		require.Equal(t, map[string]string{"theme": "light"}, reloaded.Get("prefs"))
	})

	t.Run("mutated after Set on a decoded session", func(t *testing.T) {
		t.Parallel()

		app, store := newStore()
		id := seedSession(t, app, store, func(sess *Session) { sess.Set("user", "fenny") })

		sess := loadSession(t, app, store, id)
		prefs := map[string]string{"theme": "dark"}
		sess.Set("prefs", prefs)
		require.NoError(t, sess.Save())

		// The caller still holds prefs, so the second save must encode again.
		prefs["theme"] = "light"
		require.NoError(t, sess.Save())

		reloaded := sessionTestStored(t, store, id)
		require.Equal(t, map[string]string{"theme": "light"}, reloaded.Get("prefs"))
		require.Equal(t, "fenny", reloaded.Get("user"))
	})
}

// Benchmark_Session_ReadOnly measures the request shape the clean-save path
// targets: load a stored session, read a scalar, save it unmodified.
func Benchmark_Session_ReadOnly(b *testing.B) {
	app := fiber.New()
	store := NewStore(Config{Storage: memory.New()})

	seed := app.AcquireCtx(&fasthttp.RequestCtx{})
	sess, err := store.Get(seed)
	require.NoError(b, err)
	sess.Set("user_id", "3d2ab0a4b1f94d4c9a1b7c0e5f6a8b9c")
	sess.Set("role", "admin")
	require.NoError(b, sess.Save())
	id := sess.ID()
	sess.Release()
	app.ReleaseCtx(seed)

	c := apptest.Ctx(b, app)
	c.Request().Header.SetCookie("session_id", id)

	b.ReportAllocs()
	for b.Loop() {
		s, _ := store.Get(c) //nolint:errcheck // We're inside a benchmark
		_ = s.Get("user_id")
		_ = s.Save() //nolint:errcheck // We're inside a benchmark

		s.Release()
	}
}

// scribbleStorage overwrites the buffer it handed out on the previous Get, the
// way a backend reusing a read buffer would.
type scribbleStorage struct {
	fiber.Storage
	last []byte
}

func (s *scribbleStorage) GetWithContext(ctx context.Context, key string) ([]byte, error) {
	val, err := s.Storage.GetWithContext(ctx, key)
	s.last = val
	return val, err
}

func (s *scribbleStorage) Get(key string) ([]byte, error) {
	return s.GetWithContext(context.Background(), key)
}

func (s *scribbleStorage) scribble() {
	for i := range s.last {
		s.last[i] = 0xFF
	}
}

// The retained snapshot must be a copy: fiber.Storage promises nothing about
// the lifetime of the slice its Get returns.
func Test_Session_RetainedSnapshotSurvivesStorageBufferReuse(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	scribbler := &scribbleStorage{Storage: memory.New()}
	store := NewStore(Config{Storage: scribbler})

	id := seedSession(t, app, store, func(sess *Session) { sess.Set("user", "fenny") })

	sess := loadSession(t, app, store, id)
	scribbler.scribble()
	require.NoError(t, sess.Save())

	reloaded := sessionTestStored(t, store, id)
	require.Equal(t, "fenny", reloaded.Get("user"))
}

// The snapshot outlives operations that do not touch the data, and must not
// outlive the ones that do.
func Test_Session_CleanSave_Lifecycle(t *testing.T) {
	t.Parallel()

	t.Run("Regenerate writes the snapshot under the new ID", func(t *testing.T) {
		t.Parallel()

		app := fiber.New()
		store := NewStore(Config{Storage: memory.New()})
		id := seedSession(t, app, store, func(sess *Session) { sess.Set("user", "fenny") })

		sess := loadSession(t, app, store, id)
		require.NoError(t, sess.Regenerate())
		newID := sess.ID()
		require.NotEqual(t, id, newID)
		sess.data.Data["user"] = "bypassed" // only a re-encode would persist it
		require.NoError(t, sess.Save())

		_, err := store.GetByID(t.Context(), id)
		require.ErrorIs(t, err, ErrSessionIDNotFoundInStore)

		reloaded := sessionTestStored(t, store, newID)
		require.Equal(t, "fenny", reloaded.Get("user"))
	})

	t.Run("Reset drops the snapshot", func(t *testing.T) {
		t.Parallel()

		app := fiber.New()
		store := NewStore(Config{Storage: memory.New()})
		id := seedSession(t, app, store, func(sess *Session) { sess.Set("user", "fenny") })

		sess := loadSession(t, app, store, id)
		require.NoError(t, sess.Reset())
		require.Nil(t, sess.rawData)
		require.NoError(t, sess.Save())

		reloaded := sessionTestStored(t, store, sess.ID())
		require.Nil(t, reloaded.Get("user"))
	})

	t.Run("Destroy drops the snapshot", func(t *testing.T) {
		t.Parallel()

		app := fiber.New()
		store := NewStore(Config{Storage: memory.New()})
		id := seedSession(t, app, store, func(sess *Session) { sess.Set("user", "fenny") })

		sess := loadSession(t, app, store, id)
		require.NoError(t, sess.Destroy())
		require.Nil(t, sess.rawData)
		require.NoError(t, sess.Save())

		reloaded := sessionTestStored(t, store, id)
		require.Nil(t, reloaded.Get("user"))
	})

	t.Run("a session loaded by ID can take the fast path", func(t *testing.T) {
		t.Parallel()

		app := fiber.New()
		rec := newRecordingStorage()
		store := NewStore(Config{Storage: rec})
		id := seedSession(t, app, store, func(sess *Session) { sess.Set("user", "fenny") })
		rec.reset()

		// GetByID leaves ctx nil, so this also covers saving without a request.
		sess := sessionTestStored(t, store, id)
		require.False(t, sess.data.dirty.Load())
		sess.data.Data["user"] = "bypassed" // only a re-encode would persist it
		require.NoError(t, sess.Save())

		written := rec.writes()
		require.Len(t, written, 1)
		require.Equal(t, id, written[0].key)
		require.NotContains(t, string(written[0].value), "bypassed")
	})
}

// With AbsoluteTimeout every load reads the deadline back as a time.Time, and that
// read must neither dirty the session nor move the deadline.
func Test_Session_CleanSave_KeepsAbsoluteExpiration(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	store := NewStore(Config{Storage: memory.New(), AbsoluteTimeout: time.Hour})

	id := seedSession(t, app, store, func(sess *Session) { sess.Set("user", "fenny") })

	first := loadSession(t, app, store, id)
	deadline := first.absExpiration()
	require.False(t, deadline.IsZero())
	// The absolute-timeout probe reads a time.Time through Get on every load,
	// so the session has to stay clean for the fast path to exist at all.
	require.False(t, first.data.dirty.Load())
	require.NoError(t, first.Save())

	second := loadSession(t, app, store, id)
	require.Equal(t, deadline, second.absExpiration())
}

// Get writes the dirty flag while holding only RLock, so a shared session must
// stay race-free when a save runs next to a read.
func Test_Session_ConcurrentAliasedReadAndSave(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	store := NewStore(Config{Storage: memory.New()})
	store.RegisterType(map[string]string{})

	id := seedSession(t, app, store, func(sess *Session) {
		sess.Set("prefs", map[string]string{"theme": "dark"})
		sess.Set("user", "fenny")
	})

	sess := loadSession(t, app, store, id)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				_ = sess.Get("prefs")
				_ = sess.Get("user")
				_ = sess.Keys()
			}
		})
	}

	saveErrs := make(chan error, 50)
	wg.Go(func() {
		for range 50 {
			if err := sess.Save(); err != nil {
				saveErrs <- err
			}
		}
	})

	wg.Wait()
	close(saveErrs)
	for err := range saveErrs {
		require.NoError(t, err)
	}

	reloaded := sessionTestStored(t, store, id)
	require.Equal(t, "fenny", reloaded.Get("user"))
}

// Special thanks to @codemicro for moving this to fiber core
// Original middleware: github.com/codemicro/fiber-cache
package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/internal/apptest"
	"github.com/gofiber/fiber/v3/internal/storage/memory"
	"github.com/gofiber/fiber/v3/middleware/compress"
	"github.com/gofiber/fiber/v3/middleware/etag"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/gofiber/utils/v2"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// testClock is a manually advanced clock that makes time-dependent freshness
// tests deterministic: instead of sleeping past a TTL boundary (which is racy
// under -race -count -shuffle), tests advance the clock explicitly. It is safe
// for the concurrent reads performed by the cache handler.
type testClock struct {
	now time.Time
	mu  sync.Mutex
}

func newTestClock(start time.Time) *testClock {
	return &testClock{now: start}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// tickingClock advances a second on every read, so any clock read the store
// phase performs after the Date header was stamped surfaces as phantom age.
type tickingClock struct {
	now time.Time
	mu  sync.Mutex
}

func (c *tickingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(time.Second)
	return c.now
}

type failingCacheStorage struct {
	data map[string][]byte
	errs map[string]error
	mu   sync.RWMutex
}

type mutatingStorage struct {
	data   map[string][]byte
	mutate func(key string, value []byte) []byte
}

func newFailingCacheStorage() *failingCacheStorage {
	return &failingCacheStorage{
		data: make(map[string][]byte),
		errs: make(map[string]error),
	}
}

var storageKinds = []string{"memory", "external"}

func storageOf(kind string) fiber.Storage {
	if kind == "external" {
		return newFailingCacheStorage()
	}
	return nil
}

type reply struct {
	header http.Header
	cache  string
	body   string
	status int
}

// doRequest runs req and returns the reply, its body read and closed.
func doRequest(t *testing.T, app *fiber.App, req *http.Request) reply {
	t.Helper()

	resp := apptest.Do(t, app, req)
	body := apptest.Body(t, resp)
	require.NoError(t, resp.Body.Close())
	return reply{status: resp.StatusCode, header: resp.Header, cache: resp.Header.Get("X-Cache"), body: body}
}

// getRoot sends GET / with the headers.
func getRoot(t *testing.T, app *fiber.App, headers map[string]string) reply {
	t.Helper()

	req := httptest.NewRequest(fiber.MethodGet, "/", http.NoBody)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	return doRequest(t, app, req)
}

func newMutatingStorage(mutate func(key string, value []byte) []byte) *mutatingStorage {
	return &mutatingStorage{
		data:   make(map[string][]byte),
		mutate: mutate,
	}
}

func (s *mutatingStorage) GetWithContext(_ context.Context, key string) ([]byte, error) {
	return s.Get(key)
}

func (s *mutatingStorage) Get(key string) ([]byte, error) {
	if value, ok := s.data[key]; ok {
		return value, nil
	}

	return nil, nil
}

func (s *mutatingStorage) SetWithContext(_ context.Context, key string, val []byte, _ time.Duration) error {
	return s.Set(key, val, 0)
}

func (s *mutatingStorage) Set(key string, val []byte, _ time.Duration) error {
	if key == "" || len(val) == 0 {
		return nil
	}

	if s.mutate != nil {
		val = s.mutate(key, val)
	}

	s.data[key] = val
	return nil
}

func (s *mutatingStorage) DeleteWithContext(_ context.Context, key string) error {
	return s.Delete(key)
}

func (s *mutatingStorage) Delete(key string) error {
	delete(s.data, key)
	return nil
}

func (s *mutatingStorage) ResetWithContext(_ context.Context) error {
	return s.Reset()
}

func (s *mutatingStorage) Reset() error {
	s.data = make(map[string][]byte)
	return nil
}

func (s *mutatingStorage) Close() error {
	s.data = nil
	return nil
}

func (s *failingCacheStorage) GetWithContext(_ context.Context, key string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err, ok := s.errs["get|"+key]; ok && err != nil {
		return nil, err
	}
	if val, ok := s.data[key]; ok {
		return append([]byte(nil), val...), nil
	}
	return nil, nil
}

func (s *failingCacheStorage) Get(key string) ([]byte, error) {
	return s.GetWithContext(context.Background(), key)
}

func (s *failingCacheStorage) SetWithContext(_ context.Context, key string, val []byte, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err, ok := s.errs["set|"+key]; ok && err != nil {
		return err
	}
	s.data[key] = append([]byte(nil), val...)
	return nil
}

func (s *failingCacheStorage) Set(key string, val []byte, exp time.Duration) error {
	return s.SetWithContext(context.Background(), key, val, exp)
}

func (s *failingCacheStorage) DeleteWithContext(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err, ok := s.errs["del|"+key]; ok && err != nil {
		return err
	}
	delete(s.data, key)
	return nil
}

func (s *failingCacheStorage) Delete(key string) error {
	return s.DeleteWithContext(context.Background(), key)
}

func (s *failingCacheStorage) ResetWithContext(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = make(map[string][]byte)
	s.errs = make(map[string]error)
	return nil
}

func (s *failingCacheStorage) Reset() error {
	return s.ResetWithContext(context.Background())
}

func (*failingCacheStorage) Close() error { return nil }

type contextRecord struct {
	key      string
	value    string
	canceled bool
}

type contextRecorderStorage struct {
	*failingCacheStorage
	deletes []contextRecord
	gets    []contextRecord
	sets    []contextRecord
}

// blockingDeleteStorage pauses the first stale metadata deletion while another
// request replaces that entry and reuses its heap index.
type blockingDeleteStorage struct {
	*failingCacheStorage
	deleteStarted  chan struct{}
	continueDelete chan struct{}
	key            string
	blockOnce      atomic.Bool
}

func (s *blockingDeleteStorage) DeleteWithContext(ctx context.Context, key string) error {
	if key == s.key && s.blockOnce.CompareAndSwap(false, true) {
		close(s.deleteStarted)
		<-s.continueDelete
	}
	return s.failingCacheStorage.DeleteWithContext(ctx, key)
}

// restoreMutationStorage models a backend change while eviction has dropped
// the old heap node and its storage deletion fails.
type restoreMutationStorage struct {
	*failingCacheStorage
	key      string
	mutation string
	armed    atomic.Bool
}

type blockingFailedEvictionStorage struct {
	*failingCacheStorage
	started        chan struct{}
	continueDelete chan struct{}
	key            string
	armed          atomic.Bool
}

func (s *blockingFailedEvictionStorage) DeleteWithContext(ctx context.Context, key string) error {
	if key == s.key && s.armed.CompareAndSwap(true, false) {
		close(s.started)
		<-s.continueDelete
		return errors.New("eviction delete failed")
	}
	return s.failingCacheStorage.DeleteWithContext(ctx, key)
}

func (s *restoreMutationStorage) DeleteWithContext(ctx context.Context, key string) error {
	if key != s.key || !s.armed.CompareAndSwap(true, false) {
		return s.failingCacheStorage.DeleteWithContext(ctx, key)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mutation == "missing" {
		delete(s.data, key)
		delete(s.data, key+"_body")
	} else {
		var entry item
		if _, err := entry.UnmarshalMsg(s.data[key]); err != nil {
			return err
		}
		entry.heapgen += 1000
		raw, err := entry.MarshalMsg(nil)
		if err != nil {
			return err
		}
		s.data[key] = raw
	}
	return errors.New("eviction delete failed")
}

func newContextRecorderStorage() *contextRecorderStorage {
	return &contextRecorderStorage{failingCacheStorage: newFailingCacheStorage()}
}

func contextRecordFrom(ctx context.Context, key string) contextRecord {
	record := contextRecord{
		key:      key,
		canceled: errors.Is(ctx.Err(), context.Canceled),
	}
	if value, ok := ctx.Value(markerKey).(string); ok {
		record.value = value
	}
	return record
}

func (s *contextRecorderStorage) GetWithContext(ctx context.Context, key string) ([]byte, error) {
	s.gets = append(s.gets, contextRecordFrom(ctx, key))
	return s.failingCacheStorage.GetWithContext(ctx, key)
}

func (s *contextRecorderStorage) SetWithContext(ctx context.Context, key string, val []byte, exp time.Duration) error {
	s.sets = append(s.sets, contextRecordFrom(ctx, key))
	return s.failingCacheStorage.SetWithContext(ctx, key, val, exp)
}

func (s *contextRecorderStorage) DeleteWithContext(ctx context.Context, key string) error {
	s.deletes = append(s.deletes, contextRecordFrom(ctx, key))
	return s.failingCacheStorage.DeleteWithContext(ctx, key)
}

func (s *contextRecorderStorage) recordedGets() []contextRecord {
	out := make([]contextRecord, len(s.gets))
	copy(out, s.gets)
	return out
}

func (s *contextRecorderStorage) recordedSets() []contextRecord {
	out := make([]contextRecord, len(s.sets))
	copy(out, s.sets)
	return out
}

func (s *contextRecorderStorage) recordedDeletes() []contextRecord {
	out := make([]contextRecord, len(s.deletes))
	copy(out, s.deletes)
	return out
}

// cacheTestAKey is the key under which the default key generator stores the
// entry of a plain GET for path, as httptest sends it to example.com.
func cacheTestAKey(path string) string {
	return cacheKeyVersion + "|GET|scheme=http|host=example.com|" + path + "|q=|h=accept:0|accept-encoding:0|accept-language:0"
}

func TestCacheStorageErrors(t *testing.T) {
	t.Parallel()

	key := cacheTestAKey("/")

	for _, tc := range []struct {
		name    string
		errKey  string // the failing storage operation, spelled as failingCacheStorage keys it
		wantErr string
		expired bool // the storage already holds an expired entry for the request
	}{
		{name: "get", errKey: "get|" + key, wantErr: "cache: failed to get key"},
		{name: "set", errKey: "set|" + key + "_body", wantErr: "cache: failed to store raw key"},
		{name: "delete", errKey: "del|" + key, wantErr: "cache: failed to delete expired key", expired: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			storage := newFailingCacheStorage()
			storage.errs[tc.errKey] = errors.New("boom")

			if tc.expired {
				// Use an obviously expired timestamp without relying on time-based conversions
				raw, err := (&item{exp: 1}).MarshalMsg(nil)
				require.NoError(t, err)
				storage.data[key] = raw
			}

			var captured error
			app := fiber.New(fiber.Config{
				ErrorHandler: func(c fiber.Ctx, err error) error {
					captured = err
					return c.Status(fiber.StatusInternalServerError).SendString("storage failure")
				},
			})

			app.Use(New(Config{Storage: storage, Expiration: time.Second}))
			app.Get("/", func(c fiber.Ctx) error {
				return c.SendString("ok")
			})

			apptest.Expect(t, app, "/", fiber.StatusInternalServerError)
			require.Error(t, captured)
			require.ErrorContains(t, captured, tc.wantErr)
		})
	}
}

type contextKey string

const markerKey contextKey = "marker"

func contextWithMarker(label string) context.Context {
	return context.WithValue(context.Background(), markerKey, label)
}

func canceledContextWithMarker(label string) context.Context {
	ctx, cancel := context.WithCancel(contextWithMarker(label))
	cancel()
	return ctx
}

func TestCacheEvictionPropagatesRequestContextToDelete(t *testing.T) {
	t.Parallel()

	storage := newContextRecorderStorage()
	app := fiber.New()

	app.Use(func(c fiber.Ctx) error {
		path := c.Path()
		if path == "/first" {
			c.SetContext(contextWithMarker("first"))
		}
		if path == "/second" {
			c.SetContext(canceledContextWithMarker("evict"))
		}
		return c.Next()
	})

	app.Use(New(Config{Storage: storage, Expiration: time.Minute, MaxBytes: 5}))

	app.Get("/first", func(c fiber.Ctx) error {
		return c.SendString("aaa")
	})

	app.Get("/second", func(c fiber.Ctx) error {
		return c.SendString("bbbb")
	})

	apptest.Expect(t, app, "/first", fiber.StatusOK)

	apptest.Expect(t, app, "/second", fiber.StatusOK)

	records := storage.recordedDeletes()
	require.Len(t, records, 2)

	var keys []string
	for _, rec := range records {
		keys = append(keys, rec.key)
		require.Equal(t, "evict", rec.value)
		require.True(t, rec.canceled)
	}

	require.ElementsMatch(t, []string{cacheTestAKey("/first"), cacheTestAKey("/first") + "_body"}, keys)
}

func TestCacheCleanupPropagatesRequestContextToDelete(t *testing.T) {
	t.Parallel()

	storage := newContextRecorderStorage()
	storage.errs["set|"+cacheTestAKey("/")] = errors.New("boom")

	var captured error
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c fiber.Ctx, err error) error {
			captured = err
			return c.Status(fiber.StatusInternalServerError).SendString("storage failure")
		},
	})

	app.Use(func(c fiber.Ctx) error {
		c.SetContext(canceledContextWithMarker("cleanup"))
		return c.Next()
	})

	app.Use(New(Config{Storage: storage, Expiration: time.Minute}))

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("payload")
	})

	apptest.Expect(t, app, "/", fiber.StatusInternalServerError)
	require.Error(t, captured)
	require.ErrorContains(t, captured, "cache: failed to store key")

	records := storage.recordedDeletes()
	require.Len(t, records, 1)
	require.Equal(t, cacheTestAKey("/")+"_body", records[0].key)
	require.Equal(t, "cleanup", records[0].value)
	require.True(t, records[0].canceled)
}

func TestCacheStorageOperationsObserveRequestContext(t *testing.T) {
	t.Parallel()

	storage := newContextRecorderStorage()
	app := fiber.New()

	app.Use(func(c fiber.Ctx) error {
		ctxLabel := string(c.Request().Header.Peek("X-Context"))
		if ctxLabel == "" {
			return c.Next()
		}

		canceled := string(c.Request().Header.Peek("X-Cancel")) == "true"
		if canceled {
			c.SetContext(canceledContextWithMarker(ctxLabel))
		} else {
			c.SetContext(contextWithMarker(ctxLabel))
		}
		return c.Next()
	})

	app.Use(New(Config{Storage: storage, Expiration: time.Minute}))

	app.Get("/cache", func(c fiber.Ctx) error {
		return c.SendString("payload")
	})

	for _, label := range []string{"store", "fetch"} {
		apptest.Expect(t, app, apptest.Req(fiber.MethodGet, "/cache",
			apptest.WithHeader("X-Context", label), apptest.WithHeader("X-Cancel", "true")), fiber.StatusOK)
	}

	entryKey := cacheTestAKey("/cache")

	setRecords := storage.recordedSets()
	require.Len(t, setRecords, 2)
	for _, rec := range setRecords {
		require.Contains(t, []string{entryKey, entryKey + "_body"}, rec.key)
		require.Equal(t, "store", rec.value)
		require.True(t, rec.canceled)
	}

	getRecords := storage.recordedGets()
	require.NotEmpty(t, getRecords)

	var fetchEntry, fetchBody bool
	for _, rec := range getRecords {
		if rec.value != "fetch" {
			continue
		}

		if rec.key == entryKey {
			require.True(t, rec.canceled)
			fetchEntry = true
		}
		if rec.key == entryKey+"_body" {
			require.True(t, rec.canceled)
			fetchBody = true
		}
	}

	require.True(t, fetchEntry, "expected cached entry retrieval to observe request context")
	require.True(t, fetchBody, "expected cached body retrieval to observe request context")
}

func Test_Cache_CacheControl(t *testing.T) {
	t.Parallel()

	app := fiber.New()

	app.Use(New(Config{Expiration: 10 * time.Second}))

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("Hello, World!")
	})

	apptest.Do(t, app, "/")

	resp := apptest.Do(t, app, "/")
	// max-age can be 10 or 11 depending on sub-second timing between store and read
	cc := resp.Header.Get(fiber.HeaderCacheControl)
	require.True(t, cc == "public, max-age=10" || cc == "public, max-age=11",
		"unexpected Cache-Control: %s", cc)
}

func Test_Cache_CacheControl_Disabled(t *testing.T) {
	t.Parallel()

	app := fiber.New()

	app.Use(New(Config{
		Expiration:          10 * time.Second,
		DisableCacheControl: true,
	}))

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("Hello, World!")
	})

	apptest.Do(t, app, "/")

	resp := apptest.Do(t, app, "/")
	require.Empty(t, resp.Header.Get(fiber.HeaderCacheControl))
}

// cacheTestAOrigin returns an app that serves / for every method through mw, the
// cache middleware under test, from an origin that counts its calls: the nth
// call answers prefix followed by n and sets the response headers given as name
// and value pairs. The counter tells how many requests reached the origin.
func cacheTestAOrigin(mw fiber.Handler, prefix string, headers ...string) (*fiber.App, *atomic.Int32) {
	app := fiber.New()
	app.Use(mw)

	calls := new(atomic.Int32)
	app.All("/", func(c fiber.Ctx) error {
		n := calls.Add(1)
		for i := 0; i+1 < len(headers); i += 2 {
			c.Set(headers[i], headers[i+1])
		}
		return c.SendString(prefix + strconv.Itoa(int(n)))
	})
	return app, calls
}

func Test_Cache_Expired(t *testing.T) {
	t.Parallel()
	app, _ := cacheTestAOrigin(New(Config{Expiration: 2 * time.Second}), "")

	resp := apptest.Do(t, app, "/")
	body := apptest.BodyBytes(t, resp)

	// Sleep until the cache is expired
	time.Sleep(3 * time.Second)

	respCached := apptest.Do(t, app, "/")
	bodyCached := apptest.BodyBytes(t, respCached)

	if bytes.Equal(body, bodyCached) {
		t.Errorf("Cache should have expired: %s, %s", body, bodyCached)
	}

	// Next response should be also cached
	respCachedNextRound := apptest.Do(t, app, "/")
	bodyCachedNextRound := apptest.BodyBytes(t, respCachedNextRound)

	if !bytes.Equal(bodyCachedNextRound, bodyCached) {
		t.Errorf("Cache should not have expired: %s, %s", bodyCached, bodyCachedNextRound)
	}
}

func Test_Cache(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		mw   fiber.Handler
		name string
	}{
		{name: "query keys disabled", mw: New(Config{DisableQueryKeys: true})},
		{name: "invalid expiration", mw: New(Config{Expiration: 0 * time.Second})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app, _ := cacheTestAOrigin(tc.mw, "")

			resp := apptest.Do(t, app, "/")

			cachedResp := apptest.Do(t, app, "/")

			body := apptest.BodyBytes(t, resp)
			cachedBody := apptest.BodyBytes(t, cachedResp)

			require.Equal(t, cachedBody, body)
		})
	}
}

// cacheTestAExpect sends target through app and requires the X-Cache header of
// the response to be want. It returns the response for further assertions.
func cacheTestAExpect[T apptest.Target](t *testing.T, app *fiber.App, target T, want string) *http.Response {
	t.Helper()

	resp := apptest.Do(t, app, target)
	require.Equal(t, want, resp.Header.Get("X-Cache"), "%s %s: X-Cache", resp.Request.Method, resp.Request.URL)
	return resp
}

// cacheTestAExpectBody is cacheTestAExpect that also requires the response body
// to be body.
func cacheTestAExpectBody[T apptest.Target](t *testing.T, app *fiber.App, target T, want, body string) *http.Response {
	t.Helper()

	resp := cacheTestAExpect(t, app, target, want)
	require.Equal(t, body, apptest.Body(t, resp), "%s %s: body", resp.Request.Method, resp.Request.URL)
	return resp
}

// cacheTestAExpectStatus is cacheTestAExpect that also requires the response
// status to be status.
func cacheTestAExpectStatus[T apptest.Target](t *testing.T, app *fiber.App, target T, want string, status int) *http.Response {
	t.Helper()

	resp := apptest.Expect(t, app, target, status)
	require.Equal(t, want, resp.Header.Get("X-Cache"), "%s %s: X-Cache", resp.Request.Method, resp.Request.URL)
	return resp
}

// cacheTestAGetCC returns a GET request for target that carries the
// Cache-Control request header cacheControl.
func cacheTestAGetCC(target, cacheControl string) *http.Request {
	return apptest.Req(fiber.MethodGet, target, apptest.WithHeader(fiber.HeaderCacheControl, cacheControl))
}

// cacheTestAIDApp returns an app that serves GET / through the middleware mw and
// answers it with the id query parameter (default 1), so that a body says which
// request produced it.
func cacheTestAIDApp(mw ...fiber.Handler) *fiber.App {
	app := fiber.New()
	for _, h := range mw {
		app.Use(h)
	}
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString(fiber.Query(c, "id", "1"))
	})
	return app
}

// go test -run Test_Cache_WithNoCacheRequestDirective
func Test_Cache_WithNoCacheRequestDirective(t *testing.T) {
	t.Parallel()

	app := cacheTestAIDApp(New(Config{DisableQueryKeys: true}))

	// Request id = 1
	cacheTestAExpectBody(t, app, "/", cacheMiss, "1")
	// Response cached, entry id = 1

	// Request id = 2 without Cache-Control: no-cache
	cacheTestAExpectBody(t, app, "/?id=2", cacheHit, "1")
	// Response not cached, returns cached response, entry id = 1

	// Request id = 2 with Cache-Control: no-cache
	cacheTestAExpectBody(t, app, cacheTestAGetCC("/?id=2", noCache), cacheMiss, "2")
	// Response cached, returns updated response, entry = 2

	/* Check Test_Cache_WithETagAndNoCacheRequestDirective */
	// Request id = 2 with Cache-Control: no-cache again
	cacheTestAExpectBody(t, app, cacheTestAGetCC("/?id=2", noCache), cacheMiss, "2")
	// Response cached, returns updated response, entry = 2

	// Request id = 3 with Cache-Control: NO-CACHE
	cacheTestAExpectBody(t, app, cacheTestAGetCC("/?id=3", "NO-CACHE"), cacheMiss, "3")
	// Response cached, returns updated response, entry = 3

	// Request id = 4 with Cache-Control: my-no-cache
	cacheTestAExpectBody(t, app, cacheTestAGetCC("/?id=4", "my-no-cache"), cacheHit, "3")
	// Response served from cache, existing entry = 3

	// Request id = 4 again without Cache-Control: no-cache
	cacheTestAExpectBody(t, app, "/?id=4", cacheHit, "3")
	// Response cached, returns cached response, entry id = 3

	// Request id = 1 without Cache-Control: no-cache
	cacheTestAExpectBody(t, app, "/", cacheHit, "3")
	// Response not cached, returns cached response, entry id = 3
}

// go test -run Test_Cache_WithETagAndNoCacheRequestDirective
func Test_Cache_WithETagAndNoCacheRequestDirective(t *testing.T) {
	t.Parallel()

	app := cacheTestAIDApp(etag.New(), New(Config{DisableQueryKeys: true}))

	// withETag is a GET for target that carries the Cache-Control request header
	// and the entity tag the client holds.
	withETag := func(target, cacheControl, etagToken string) *http.Request {
		return apptest.Req(fiber.MethodGet, target,
			apptest.WithHeader(fiber.HeaderCacheControl, cacheControl),
			apptest.WithHeader(fiber.HeaderIfNoneMatch, etagToken))
	}

	// Request id = 1
	resp := cacheTestAExpectStatus(t, app, "/", cacheMiss, fiber.StatusOK)
	// Response cached, entry id = 1

	// If response status 200
	etagToken := resp.Header.Get("Etag")

	// Request id = 2 with ETag but without Cache-Control: no-cache
	cacheTestAExpectStatus(t, app, apptest.Req(fiber.MethodGet, "/?id=2", apptest.WithHeader(fiber.HeaderIfNoneMatch, etagToken)),
		cacheHit, fiber.StatusNotModified)
	// Response not cached, returns cached response, entry id = 1, status not modified

	// Request id = 2 with ETag and Cache-Control: no-cache
	resp = cacheTestAExpectStatus(t, app, withETag("/?id=2", noCache, etagToken), cacheMiss, fiber.StatusOK)
	// Response cached, returns updated response, entry id = 2

	// If response status 200
	etagToken = resp.Header.Get("Etag")

	// Request id = 3 with ETag and Cache-Control: NO-CACHE
	cacheTestAExpectStatus(t, app, withETag("/?id=3", "NO-CACHE", etagToken), cacheMiss, fiber.StatusOK)
	// Response cached, returns updated response, entry id = 3

	// Request id = 2 with ETag and Cache-Control: no-cache again
	cacheTestAExpectStatus(t, app, withETag("/?id=2", noCache, etagToken), cacheMiss, fiber.StatusNotModified)
	// Response cached, returns updated response, entry id = 2, status not modified

	// Request id = 1 without ETag and Cache-Control: no-cache
	cacheTestAExpectStatus(t, app, "/", cacheHit, fiber.StatusOK)
	// Response not cached, returns cached response, entry id = 2
}

// go test -run Test_Cache_WithNoStoreRequestDirective
func Test_Cache_WithNoStoreRequestDirective(t *testing.T) {
	t.Parallel()

	app := cacheTestAIDApp(New(Config{DisableQueryKeys: true}))

	// Request id = 2
	apptest.ExpectBody(t, app, cacheTestAGetCC("/?id=2", noStore), fiber.StatusOK, "2")
	// Response not cached, returns updated response

	// Request id = 3 with Cache-Control: NO-STORE
	apptest.ExpectBody(t, app, cacheTestAGetCC("/?id=3", "NO-STORE"), fiber.StatusOK, "3")
	// Response not cached, returns updated response

	// Request id = 4 with Cache-Control: my-no-store
	cacheTestAExpectBody(t, app, cacheTestAGetCC("/?id=4", "my-no-store"), cacheMiss, "4")
	// Response cached, returns updated response, entry = 4

	// Request id = 4 again without Cache-Control
	cacheTestAExpectBody(t, app, "/?id=4", cacheHit, "4")
	// Response cached previously, served from cache
}

func Test_Cache_WithSeveralRequests(t *testing.T) {
	t.Parallel()

	app := fiber.New()

	app.Use(New(Config{
		Expiration: 10 * time.Second,
	}))

	app.Get("/:id", func(c fiber.Ctx) error {
		return c.SendString(c.Params("id"))
	})

	for range 10 {
		for i := range 10 {
			func(id int) {
				rsp := apptest.Do(t, app, fmt.Sprintf("/%d", id))

				defer func(body io.ReadCloser) {
					closeErr := body.Close()
					require.NoError(t, closeErr)
				}(rsp.Body)

				idFromServ := apptest.Body(t, rsp)

				a, err := strconv.Atoi(idFromServ)
				require.NoError(t, err)

				// Sometimes, the id is not equal to a
				require.Equal(t, id, a)
			}(i)
		}
	}
}

func Test_Cache_Get(t *testing.T) {
	t.Parallel()

	app := fiber.New()

	app.Use(New())

	app.Post("/", func(c fiber.Ctx) error {
		return c.SendString(fiber.Query[string](c, "cache"))
	})

	app.Get("/get", func(c fiber.Ctx) error {
		return c.SendString(fiber.Query[string](c, "cache"))
	})

	apptest.ExpectBody(t, app, apptest.Req(fiber.MethodPost, "/?cache=123"), fiber.StatusOK, "123")
	apptest.ExpectBody(t, app, apptest.Req(fiber.MethodPost, "/?cache=12345"), fiber.StatusOK, "12345")
	apptest.ExpectBody(t, app, "/get?cache=123", fiber.StatusOK, "123")
	apptest.ExpectBody(t, app, "/get?cache=12345", fiber.StatusOK, "12345")
}

func Test_Cache_Post(t *testing.T) {
	t.Parallel()

	app := fiber.New()

	app.Use(New())

	var count atomic.Int32
	app.Post("/", func(c fiber.Ctx) error {
		current := count.Add(1)
		return c.SendString(fmt.Sprintf("%d:%s", current, fiber.Query[string](c, "cache")))
	})

	cacheTestAExpectBody(t, app, apptest.Req(fiber.MethodPost, "/?cache=123"), cacheUnreachable, "1:123")
	cacheTestAExpectBody(t, app, apptest.Req(fiber.MethodPost, "/?cache=123"), cacheUnreachable, "2:123")
	cacheTestAExpectBody(t, app, apptest.Req(fiber.MethodPost, "/?cache=12345"), cacheUnreachable, "3:12345")
}

func Test_Cache_CustomMethods(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		wantCache [2]string // X-Cache of the first and of the second request
		name      string
		method    string
		methods   []string
		wantCalls int32 // requests that reach the handler
	}{
		{
			name:      "POST cached when in Methods",
			method:    fiber.MethodPost,
			methods:   []string{fiber.MethodGet, fiber.MethodHead, fiber.MethodPost},
			wantCache: [2]string{cacheMiss, cacheHit},
			wantCalls: 1,
		},
		{
			// PUT not in Methods: always bypasses the cache
			name:      "unconfigured method bypasses cache",
			method:    fiber.MethodPut,
			methods:   []string{fiber.MethodGet},
			wantCache: [2]string{cacheUnreachable, cacheUnreachable},
			wantCalls: 2,
		},
		{
			// Even GET bypasses the cache when Methods is explicitly empty
			name:      "empty Methods slice disables caching",
			method:    fiber.MethodGet,
			methods:   []string{},
			wantCache: [2]string{cacheUnreachable, cacheUnreachable},
			wantCalls: 2,
		},
		{
			// "get" is normalized to "GET" and matches
			name:      "lowercase method names are normalized",
			method:    fiber.MethodGet,
			methods:   []string{"get", "post"},
			wantCache: [2]string{cacheMiss, cacheHit},
			wantCalls: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app, calls := cacheTestAOrigin(New(Config{Methods: tc.methods}), "")

			// The origin answers with its call count, so a cached second answer repeats the first.
			cacheTestAExpectBody(t, app, apptest.Req(tc.method, "/"), tc.wantCache[0], "1")
			cacheTestAExpectBody(t, app, apptest.Req(tc.method, "/"), tc.wantCache[1], strconv.Itoa(int(tc.wantCalls)))
			require.Equal(t, tc.wantCalls, calls.Load(), "the handler must run once per request the cache lets through")
		})
	}
}

func Test_Cache_QueryMethod(t *testing.T) {
	t.Parallel()

	// query is a QUERY request for / that carries body.
	query := func(body string) *http.Request {
		return httptest.NewRequest(fiber.MethodQuery, "/", strings.NewReader(body))
	}

	t.Run("same body hits cache", func(t *testing.T) {
		t.Parallel()
		app, _ := cacheTestAOrigin(New(Config{Methods: []string{fiber.MethodQuery}}), "")

		body := `{"filter":"active"}`

		cacheTestAExpectBody(t, app, query(body), cacheMiss, "1")
		cacheTestAExpectBody(t, app, query(body), cacheHit, "1")
	})

	t.Run("different body produces different cache key", func(t *testing.T) {
		t.Parallel()
		app, calls := cacheTestAOrigin(New(Config{Methods: []string{fiber.MethodQuery}}), "")

		bodyA := `{"filter":"active"}`
		bodyB := `{"filter":"archived"}`

		cacheTestAExpect(t, app, query(bodyA), cacheMiss)
		cacheTestAExpect(t, app, query(bodyB), cacheMiss)
		require.Equal(t, int32(2), calls.Load())

		cacheTestAExpect(t, app, query(bodyA), cacheHit)
	})

	t.Run("body hash namespace does not collide with raw body", func(t *testing.T) {
		t.Parallel()
		app, calls := cacheTestAOrigin(New(Config{Methods: []string{fiber.MethodQuery}}), "")

		longBody := strings.Repeat("x", maxKeyDimensionSegmentLength+1)
		longBodyHash := sha256.Sum256([]byte(longBody))
		shortBody := "sha256:" + hex.EncodeToString(longBodyHash[:])

		cacheTestAExpect(t, app, query(longBody), cacheMiss)
		cacheTestAExpect(t, app, query(shortBody), cacheMiss)
		require.Equal(t, int32(2), calls.Load())

		cacheTestAExpect(t, app, query(longBody), cacheHit)
	})

	t.Run("empty body is cacheable", func(t *testing.T) {
		t.Parallel()
		app, calls := cacheTestAOrigin(New(Config{Methods: []string{fiber.MethodQuery}}), "")

		cacheTestAExpect(t, app, apptest.Req(fiber.MethodQuery, "/"), cacheMiss)
		cacheTestAExpect(t, app, apptest.Req(fiber.MethodQuery, "/"), cacheHit)
		require.Equal(t, int32(1), calls.Load())
	})

	t.Run("not cached when QUERY not in Methods", func(t *testing.T) {
		t.Parallel()
		app, calls := cacheTestAOrigin(New(), "") // default Methods: GET, HEAD

		cacheTestAExpect(t, app, query(`{}`), cacheUnreachable)
		cacheTestAExpect(t, app, query(`{}`), cacheUnreachable)
		require.Equal(t, int32(2), calls.Load())
	})
}

// cacheTestAAcceptLanguage returns a GET request for / that asks for language.
func cacheTestAAcceptLanguage(language string) *http.Request {
	return apptest.Req(fiber.MethodGet, "/", apptest.WithHeader(fiber.HeaderAcceptLanguage, language))
}

func Test_Cache_DefaultKeyDimensions(t *testing.T) {
	t.Parallel()

	// echoApp returns an app that serves method / through mw and answers with the
	// number of requests it has served and the value echo takes from the request,
	// such as "2:en-US".
	echoApp := func(mw fiber.Handler, method string, echo func(c fiber.Ctx) string) *fiber.App {
		app := fiber.New()
		app.Use(mw)

		var count atomic.Int32
		app.Add([]string{method}, "/", func(c fiber.Ctx) error {
			return c.SendString(fmt.Sprintf("%d:%s", count.Add(1), echo(c)))
		})
		return app
	}
	acceptLanguage := func(c fiber.Ctx) string { return c.Get(fiber.HeaderAcceptLanguage) }

	t.Run("distinct query values do not collide", func(t *testing.T) {
		t.Parallel()

		app := echoApp(New(), fiber.MethodGet, func(c fiber.Ctx) string { return string(c.Request().URI().QueryString()) })

		apptest.ExpectBody(t, app, "/?id=1", fiber.StatusOK, "1:id=1")
		apptest.ExpectBody(t, app, "/?id=2", fiber.StatusOK, "2:id=2")
	})

	t.Run("representation headers partition cache", func(t *testing.T) {
		t.Parallel()

		app := echoApp(New(), fiber.MethodGet, acceptLanguage)

		apptest.ExpectBody(t, app, cacheTestAAcceptLanguage("en-US"), fiber.StatusOK, "1:en-US")
		apptest.ExpectBody(t, app, cacheTestAAcceptLanguage("fr-FR"), fiber.StatusOK, "2:fr-FR")
	})

	t.Run("keyed cookies preserve case-sensitive names", func(t *testing.T) {
		t.Parallel()

		app := echoApp(New(Config{KeyCookies: []string{"SessionID"}}), fiber.MethodGet,
			func(c fiber.Ctx) string { return c.Cookies("SessionID") })

		session := func(value string) *http.Request {
			req := httptest.NewRequest(fiber.MethodGet, "/", http.NoBody)
			req.AddCookie(&http.Cookie{Name: "SessionID", Value: value})
			return req
		}

		cacheTestAExpectBody(t, app, session("alpha"), cacheMiss, "1:alpha")
		cacheTestAExpectBody(t, app, session("beta"), cacheMiss, "2:beta")
		cacheTestAExpectBody(t, app, session("beta"), cacheHit, "2:beta")
	})

	t.Run("long paths are bounded in storage keys", func(t *testing.T) {
		t.Parallel()

		storage := newMutatingStorage(nil)
		oversizedPath := "/" + strings.Repeat("a", maxKeyDimensionSegmentLength+1)
		app := fiber.New()
		app.Use(New(Config{
			Expiration: 1 * time.Hour,
			Storage:    storage,
		}))
		app.Get(oversizedPath, func(c fiber.Ctx) error {
			return c.SendString("ok")
		})

		cacheTestAExpect(t, app, oversizedPath, cacheMiss)

		hash := sha256.Sum256([]byte(oversizedPath))
		expectedBoundedPath := "sha256:" + hex.EncodeToString(hash[:])
		require.Len(t, expectedBoundedPath, len("sha256:")+sha256.Size*2)

		expectedPrefix := cacheKeyVersion + "|" + fiber.MethodGet + "|scheme=http|host=example.com|" + expectedBoundedPath
		foundBoundedKey := false
		for key := range storage.data {
			require.NotContains(t, key, oversizedPath)
			if strings.HasPrefix(key, expectedPrefix) {
				foundBoundedKey = true
			}
		}
		require.True(t, foundBoundedKey)
	})

	t.Run("empty keyed headers disable default header partitioning", func(t *testing.T) {
		t.Parallel()

		app := echoApp(New(Config{KeyHeaders: []string{}}), fiber.MethodGet, acceptLanguage)

		apptest.ExpectBody(t, app, cacheTestAAcceptLanguage("en-US"), fiber.StatusOK, "1:en-US")
		cacheTestAExpectBody(t, app, cacheTestAAcceptLanguage("fr-FR"), cacheHit, "1:en-US")
	})

	t.Run("non-keyed headers do not fragment cache", func(t *testing.T) {
		t.Parallel()

		app := echoApp(New(), fiber.MethodGet, func(c fiber.Ctx) string { return c.Get("X-Request-ID") })

		apptest.ExpectBody(t, app, apptest.Req(fiber.MethodGet, "/", apptest.WithHeader("X-Request-ID", "a")), fiber.StatusOK, "1:a")
		apptest.ExpectBody(t, app, apptest.Req(fiber.MethodGet, "/", apptest.WithHeader("X-Request-ID", "b")), fiber.StatusOK, "1:a")
	})

	t.Run("body-dependent requests are bypassed by default", func(t *testing.T) {
		t.Parallel()

		app := echoApp(New(), fiber.MethodPost, func(c fiber.Ctx) string { return string(c.Body()) })

		apptest.ExpectBody(t, app, httptest.NewRequest(fiber.MethodPost, "/", strings.NewReader("a")), fiber.StatusOK, "1:a")
		apptest.ExpectBody(t, app, httptest.NewRequest(fiber.MethodPost, "/", strings.NewReader("b")), fiber.StatusOK, "2:b")
	})
}

func Test_Cache_NothingToCache(t *testing.T) {
	t.Parallel()
	app, _ := cacheTestAOrigin(New(Config{Expiration: -(time.Second * 1)}), "")

	resp := apptest.Do(t, app, "/")
	body := apptest.BodyBytes(t, resp)

	time.Sleep(500 * time.Millisecond)

	respCached := apptest.Do(t, app, "/")
	bodyCached := apptest.BodyBytes(t, respCached)

	if bytes.Equal(body, bodyCached) {
		t.Errorf("Cache should have expired: %s, %s", body, bodyCached)
	}
}

// cacheTestAExpectRepeat requests target twice and requires the second response
// to repeat the first one's body and to carry a Cache-Control header, as a
// cached answer does. It returns the body.
func cacheTestAExpectRepeat(t *testing.T, app *fiber.App, target string) []byte {
	t.Helper()

	body := apptest.BodyBytes(t, apptest.Do(t, app, target))

	respCached := apptest.Do(t, app, target)
	bodyCached := apptest.BodyBytes(t, respCached)
	require.True(t, bytes.Equal(body, bodyCached))
	require.NotEmpty(t, respCached.Header.Get(fiber.HeaderCacheControl))
	return body
}

func Test_Cache_CustomNext(t *testing.T) {
	t.Parallel()

	app, _ := cacheTestAOrigin(New(Config{
		Next: func(c fiber.Ctx) bool {
			return c.Response().StatusCode() != fiber.StatusOK
		},
	}), "")

	errorCount := 0
	app.Get("/error", func(c fiber.Ctx) error {
		errorCount++
		return c.Status(fiber.StatusInternalServerError).SendString(strconv.Itoa(errorCount))
	})

	cacheTestAExpectRepeat(t, app, "/")

	apptest.Do(t, app, "/error")

	errRespCached := apptest.Do(t, app, "/error")
	require.Empty(t, errRespCached.Header.Get(fiber.HeaderCacheControl))
}

func Test_CustomKey(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	var called bool
	app.Use(New(Config{KeyGenerator: func(c fiber.Ctx) string {
		called = true
		return utils.CopyString(c.Path())
	}}))

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("hi")
	})

	apptest.Do(t, app, "/")
	require.True(t, called)
}

func Test_CustomExpiration(t *testing.T) {
	t.Parallel()
	var called bool
	var newCacheTime int
	app, _ := cacheTestAOrigin(New(Config{ExpirationGenerator: func(c fiber.Ctx, _ *Config) time.Duration {
		called = true
		var err error
		newCacheTime, err = strconv.Atoi(c.GetRespHeader("Cache-Time", "600"))
		require.NoError(t, err)
		return time.Second * time.Duration(newCacheTime)
	}}), "", "Cache-Time", "1")

	resp := apptest.Do(t, app, "/")
	require.True(t, called)
	require.Equal(t, 1, newCacheTime)

	// Wait until the cache expires (timestamp tick can delay expiry detection slightly).
	expireDeadline := time.Now().Add(3 * time.Second)
	var cachedResp *http.Response
	for {
		cachedResp = apptest.Do(t, app, "/")
		if cachedResp.Header.Get("X-Cache") != cacheHit {
			break
		}
		require.True(t, time.Now().Before(expireDeadline), "response remained cached beyond expected expiration")
		time.Sleep(50 * time.Millisecond)
	}

	body := apptest.BodyBytes(t, resp)
	cachedBody := apptest.BodyBytes(t, cachedResp)

	if bytes.Equal(body, cachedBody) {
		t.Errorf("Cache should have expired: %s, %s", body, cachedBody)
	}

	// Next response should be cached
	cachedRespNextRound := apptest.Do(t, app, "/")
	cachedBodyNextRound := apptest.BodyBytes(t, cachedRespNextRound)

	if !bytes.Equal(cachedBodyNextRound, cachedBody) {
		t.Errorf("Cache should not have expired: %s, %s", cachedBodyNextRound, cachedBody)
	}
}

func Test_AdditionalE2EResponseHeaders(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(New(Config{
		StoreResponseHeaders: true,
	}))

	app.Get("/", func(c fiber.Ctx) error {
		c.Response().Header.Add("X-Foobar", "foobar")
		return c.SendString("hi")
	})

	resp := apptest.Do(t, app, "/")
	require.Equal(t, "foobar", resp.Header.Get("X-Foobar"))

	resp = apptest.Do(t, app, "/")
	require.Equal(t, "foobar", resp.Header.Get("X-Foobar"))
}

// Test_SetCookieResponseIsNotStored asserts that a response handing the client
// a cookie is not stored at all.
//
// The entry would be served to every client whose request matches its key, and
// a response that sets a cookie has personalized itself for the one client that
// caused the miss. Keeping Set-Cookie out of the stored headers is not enough —
// the body is the payload, so the second client below would read the first
// one's page.
func Test_SetCookieResponseIsNotStored(t *testing.T) {
	t.Parallel()

	var calls int
	app := fiber.New()
	app.Use(New(Config{StoreResponseHeaders: true}))
	app.Get("/", func(c fiber.Ctx) error {
		calls++
		c.Cookie(&fiber.Cookie{Name: "session", Value: fmt.Sprintf("secret-%d", calls)})
		return c.SendString(fmt.Sprintf("page-for-client-%d", calls))
	})

	resp := cacheTestAExpectBody(t, app, "/", cacheUnreachable, "page-for-client-1")
	require.Contains(t, resp.Header.Get("Set-Cookie"), "session=secret-1")

	resp = cacheTestAExpect(t, app, "/", cacheUnreachable)
	require.Contains(t, resp.Header.Get("Set-Cookie"), "session=secret-2")
	require.Equal(t, "page-for-client-2", apptest.Body(t, resp), "the second client must not read the first one's page")
	require.Equal(t, 2, calls, "the handler must run for both clients")
}

// Test_SetCookieResponseRevalidateDirectivesAreNotConsent asserts that
// must-revalidate and proxy-revalidate do not buy a cookie-setting response a
// place in the store.
//
// RFC 9111 §3.5 lets must-revalidate carry a response to an authorized request,
// because a cache honoring it goes back to the origin once the entry is stale
// and the origin re-checks the credential. Neither half of that holds here: the
// directive says when a stale entry may be reused, not that the body is
// impersonal, and this middleware never revalidates — it serves the stored body
// for the whole configured expiration. Reusing the authorization test for the
// cookie gate therefore handed the first client's page to the second.
func Test_SetCookieResponseRevalidateDirectivesAreNotConsent(t *testing.T) {
	t.Parallel()

	for _, directive := range []string{"must-revalidate", "proxy-revalidate"} {
		t.Run(directive, func(t *testing.T) {
			t.Parallel()

			var calls int
			app := fiber.New()
			app.Use(New(Config{StoreResponseHeaders: true}))
			app.Get("/", func(c fiber.Ctx) error {
				calls++
				c.Set(fiber.HeaderCacheControl, directive)
				c.Cookie(&fiber.Cookie{Name: "session", Value: fmt.Sprintf("secret-%d", calls)})
				return c.SendString(fmt.Sprintf("page-for-client-%d", calls))
			})

			cacheTestAExpect(t, app, "/", cacheUnreachable)

			resp := cacheTestAExpect(t, app, "/", cacheUnreachable)
			require.Equal(t, "page-for-client-2", apptest.Body(t, resp), "the second client must not read the first one's page")
			require.Equal(t, 2, calls, "the handler must run for both clients")
		})
	}
}

// Test_SetCookieResponseSharedCacheOptIn asserts the escape hatch still works:
// a response that says outright a shared cache may hold it is taken at its
// word, so the gate above is about the revalidate directives specifically and
// not a blanket refusal of every cookie-setting response.
func Test_SetCookieResponseSharedCacheOptIn(t *testing.T) {
	t.Parallel()

	for _, directive := range []string{"public", "s-maxage=60"} {
		t.Run(directive, func(t *testing.T) {
			t.Parallel()

			app := fiber.New()
			app.Use(New(Config{StoreResponseHeaders: true}))
			app.Get("/", func(c fiber.Ctx) error {
				c.Set(fiber.HeaderCacheControl, directive)
				c.Cookie(&fiber.Cookie{Name: "session", Value: "shared"})
				return c.SendString("hi")
			})

			resp := cacheTestAExpect(t, app, "/", cacheMiss)
			// Without this the test passes vacuously: a response carrying no
			// cookie is stored anyway, so the assertions below would not
			// distinguish "the opt-in beat the cookie gate" from "there was no
			// cookie to gate".
			require.Contains(t, resp.Header.Get("Set-Cookie"), "session=shared")

			resp = cacheTestAExpect(t, app, "/", cacheHit)
			require.Empty(t, resp.Header.Values("Set-Cookie"), "the stored entry must not replay the cookie")
		})
	}
}

// Test_SetCookieFromInnerMiddlewareIsSeen pins the ordering the cookie gate can
// actually enforce: a middleware that writes its cookie on the way out is seen
// when it sits inside the cache, because its post-Next work has already run by
// the time the store decision is made.
//
// The reverse order is a hazard the gate cannot see. With the cookie-writing
// middleware outside, its post-Next work runs after the cache has stored, so
// the entry is kept and the next client reads the first one's body. Nothing
// here can detect that — the response the cache inspects genuinely has no
// cookie on it yet — so it is documented as an ordering requirement in
// docs/middleware/cache.md rather than asserted as behavior.
func Test_SetCookieFromInnerMiddlewareIsSeen(t *testing.T) {
	t.Parallel()

	var calls int
	app := fiber.New()
	app.Use(New())
	app.Use(func(c fiber.Ctx) error {
		err := c.Next()
		c.Cookie(&fiber.Cookie{Name: "session", Value: fmt.Sprintf("secret-%d", calls)})
		return err
	})
	app.Get("/", func(c fiber.Ctx) error {
		calls++
		return c.SendString(fmt.Sprintf("page-for-client-%d", calls))
	})

	for i := 1; i <= 2; i++ {
		cacheTestAExpectBody(t, app, "/", cacheUnreachable, fmt.Sprintf("page-for-client-%d", i))
	}
	require.Equal(t, 2, calls, "the handler must run for both clients")
}

// Test_StoreResponseHeaders_DropsSetCookie covers the opt-in path: a route that
// says a shared cache may store the response is taken at its word, and then the
// stored copy still leaves Set-Cookie out so the entry cannot hand the first
// client's session to the rest.
func Test_StoreResponseHeaders_DropsSetCookie(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(New(Config{
		StoreResponseHeaders: true,
	}))

	app.Get("/", func(c fiber.Ctx) error {
		c.Set(fiber.HeaderCacheControl, "public, max-age=60")
		c.Cookie(&fiber.Cookie{Name: "session", Value: "first-client-secret"})
		c.Response().Header.Add("X-Foobar", "foobar")
		return c.SendString("hi")
	})

	resp := cacheTestAExpect(t, app, "/", cacheMiss)
	require.Contains(t, resp.Header.Get("Set-Cookie"), "session=first-client-secret")

	resp = cacheTestAExpect(t, app, "/", cacheHit)
	require.Empty(t, resp.Header.Values("Set-Cookie"))
	// Other stored headers still come back, so the assertion above is about
	// Set-Cookie specifically and not about header storage being off.
	require.Equal(t, "foobar", resp.Header.Get("X-Foobar"))
}

// Test_StoreResponseHeaders_KeepsRepeatedFieldLines asserts that a header sent
// on more than one field line comes back off a cache hit intact.
//
// The restore loop used to replay every stored entry with Set, which overwrites
// the first matching line and leaves the rest — so a name sent twice collapsed
// to its last value. Two of the headers below are why that matters: a Vary that
// loses "Cookie" lets a downstream shared cache serve one user's response to
// another, and a Content-Security-Policy that loses a line drops from the
// intersection of both policies (what a browser actually enforces) to whichever
// one is weaker on its own.
func Test_StoreResponseHeaders_KeepsRepeatedFieldLines(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(New(Config{
		StoreResponseHeaders: true,
	}))

	app.Get("/", func(c fiber.Ctx) error {
		c.Response().Header.Add("Vary", "Cookie")
		c.Response().Header.Add("Vary", "Accept-Encoding")
		c.Response().Header.Add("Content-Security-Policy", "default-src 'none'")
		c.Response().Header.Add("Content-Security-Policy", "script-src 'self'")
		c.Response().Header.Add("X-Single", "only")
		return c.SendString("hi")
	})

	resp := cacheTestAExpect(t, app, "/", cacheMiss)
	require.Equal(t, []string{"Cookie", "Accept-Encoding"}, resp.Header.Values("Vary"))

	resp = cacheTestAExpect(t, app, "/", cacheHit)
	// Vary is carried by the entry itself rather than by the stored header
	// list, and comes back on one line with every name (RFC 9110 §5.3).
	require.Equal(t, []string{"Cookie,Accept-Encoding"}, resp.Header.Values("Vary"))
	require.Equal(t,
		[]string{"default-src 'none'", "script-src 'self'"},
		resp.Header.Values("Content-Security-Policy"),
	)
	// A name sent once is still restored once, so the fix did not turn Set into
	// an unconditional Add.
	require.Equal(t, []string{"only"}, resp.Header.Values("X-Single"))
}

func Test_CacheHeader(t *testing.T) {
	t.Parallel()

	app := fiber.New()

	app.Use(New(Config{
		Expiration: 10 * time.Second,
		Next: func(c fiber.Ctx) bool {
			return c.Response().StatusCode() != fiber.StatusOK
		},
	}))

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("Hello, World!")
	})

	app.Post("/", func(c fiber.Ctx) error {
		return c.SendString(fiber.Query[string](c, "cache"))
	})

	count := 0
	app.Get("/error", func(c fiber.Ctx) error {
		count++
		c.Response().Header.Add("Cache-Time", "1")
		return c.Status(fiber.StatusInternalServerError).SendString(strconv.Itoa(count))
	})

	cacheTestAExpect(t, app, "/", cacheMiss)
	cacheTestAExpect(t, app, "/", cacheHit)
	cacheTestAExpect(t, app, apptest.Req(fiber.MethodPost, "/?cache=12345"), cacheUnreachable)
	cacheTestAExpect(t, app, "/error", cacheUnreachable)
}

func Test_Cache_WithHead(t *testing.T) {
	t.Parallel()

	app, _ := cacheTestAOrigin(New(), "", "Cache-Time", "1")

	resp := cacheTestAExpect(t, app, apptest.Req(fiber.MethodHead, "/"), cacheMiss)

	cachedResp := cacheTestAExpect(t, app, apptest.Req(fiber.MethodHead, "/"), cacheHit)

	body := apptest.BodyBytes(t, resp)
	cachedBody := apptest.BodyBytes(t, cachedResp)

	require.Equal(t, cachedBody, body)
}

func Test_Cache_WithHeadThenGet(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(New())

	handler := func(c fiber.Ctx) error {
		return c.SendString(fiber.Query[string](c, "cache"))
	}
	app.RouteChain("/").Get(handler).Head(handler)

	cacheTestAExpectBody(t, app, apptest.Req(fiber.MethodHead, "/?cache=123"), cacheMiss, "")
	cacheTestAExpectBody(t, app, apptest.Req(fiber.MethodHead, "/?cache=123"), cacheHit, "")
	cacheTestAExpectBody(t, app, "/?cache=123", cacheMiss, "123")
	cacheTestAExpectBody(t, app, "/?cache=123", cacheHit, "123")
}

func Test_CustomCacheHeader(t *testing.T) {
	t.Parallel()

	app := fiber.New()

	app.Use(New(Config{
		CacheHeader: "Cache-Status",
	}))

	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("Hello, World!")
	})

	resp := apptest.Do(t, app, "/")
	require.Equal(t, cacheMiss, resp.Header.Get("Cache-Status"))
}

func Test_CacheInvalidation(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		maxBytes uint
	}{
		{name: "invalidate"},
		{name: "invalidate and remove from the heap", maxBytes: 10 * 1024 * 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app, _ := cacheTestAOrigin(New(Config{
				CacheInvalidator: func(c fiber.Ctx) bool {
					return fiber.Query[bool](c, "invalidate")
				},
				MaxBytes: tc.maxBytes,
			}), "")

			body := cacheTestAExpectRepeat(t, app, "/")

			respInvalidate := apptest.Do(t, app, "/?invalidate=true")
			bodyInvalidate := apptest.BodyBytes(t, respInvalidate)
			require.NotEqual(t, body, bodyInvalidate)
		})
	}
}

func Test_CacheInvalidation_noCacheEntry(t *testing.T) {
	t.Parallel()
	t.Run("Cache Invalidator should not be called if no cache entry exist ", func(t *testing.T) {
		t.Parallel()
		app := fiber.New()
		cacheInvalidatorExecuted := false
		app.Use(New(Config{
			CacheInvalidator: func(c fiber.Ctx) bool {
				cacheInvalidatorExecuted = true
				return fiber.Query[bool](c, "invalidate")
			},
			MaxBytes: 10 * 1024 * 1024,
		}))
		apptest.Do(t, app, "/?invalidate=true")
		require.False(t, cacheInvalidatorExecuted)
	})
}

func Test_CacheInvalidator_PanicReleasesLock(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(recover.New())
	var panicked atomic.Bool
	app.Use(New(Config{
		CacheInvalidator: func(fiber.Ctx) bool {
			if panicked.CompareAndSwap(false, true) {
				panic("invalidator")
			}
			return false
		},
	}))
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	cfg := fiber.TestConfig{Timeout: 2 * time.Second, FailOnTimeout: true}
	resp := apptest.Do(t, app, "/", cfg)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)

	resp = apptest.Do(t, app, "/", cfg)
	require.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)

	// A lock held across that panic hangs this request.
	resp = apptest.Do(t, app, "/", cfg)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func Test_CacheInvalidation_IgnoresMaxStale(t *testing.T) {
	t.Parallel()

	for _, storage := range storageKinds {
		for _, maxStale := range []string{"max-stale", "max-stale=3600"} {
			t.Run(storage+"/"+maxStale, func(t *testing.T) {
				t.Parallel()

				cfg := Config{
					MaxBytes: 1 << 20,
					CacheInvalidator: func(c fiber.Ctx) bool {
						return c.Get("X-Session-Ended") == "1"
					},
				}
				cfg.Storage = storageOf(storage)
				var calls atomic.Int32
				app := fiber.New()
				app.Use(New(cfg))
				app.Get("/", func(c fiber.Ctx) error {
					return c.SendString("response " + strconv.Itoa(int(calls.Add(1))))
				})

				got := getRoot(t, app, nil)
				require.Equal(t, "response 1", got.body)
				require.Equal(t, cacheMiss, got.cache)
				got = getRoot(t, app, nil)
				require.Equal(t, "response 1", got.body)
				require.Equal(t, cacheHit, got.cache)

				got = getRoot(t, app, map[string]string{"X-Session-Ended": "1", fiber.HeaderCacheControl: maxStale})
				require.Equal(t, "response 2", got.body, "an invalidated entry was served to a request that accepts stale ones")
				require.NotEqual(t, cacheHit, got.cache)
				require.Equal(t, int32(2), calls.Load())

				got = getRoot(t, app, nil)
				require.Equal(t, "response 2", got.body)
				require.Equal(t, cacheHit, got.cache)
			})
		}
	}
}

// CacheInvalidator must not run under the middleware's lock: a slow call would stall every request, a panic would leave it held.
func Test_CacheInvalidation_RunsWithoutTheLock(t *testing.T) {
	t.Parallel()

	for _, storage := range storageKinds {
		t.Run("a panic does not wedge the cache/"+storage, func(t *testing.T) {
			t.Parallel()

			cfg := Config{
				CacheInvalidator: func(c fiber.Ctx) bool {
					if c.Get("X-Panic") == "1" {
						panic("session store unreachable")
					}
					return false
				},
			}
			cfg.Storage = storageOf(storage)
			app := fiber.New()
			app.Use(recover.New())
			app.Use(New(cfg))
			app.Get("/", func(c fiber.Ctx) error { return c.SendString("ok") })

			require.Equal(t, cacheMiss, getRoot(t, app, nil).cache)
			require.Equal(t, fiber.StatusInternalServerError, getRoot(t, app, map[string]string{"X-Panic": "1"}).status)
			// The next request must not wait on a lock left held, and the panicking one's entry is back in the pool.
			got := getRoot(t, app, nil)
			require.Equal(t, cacheHit, got.cache)
			require.Equal(t, "ok", got.body)
		})
	}

	t.Run("a slow one does not stall other requests", func(t *testing.T) {
		t.Parallel()

		entered := make(chan struct{})
		release := make(chan struct{})
		var releaseOnce sync.Once
		releaseSlow := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(releaseSlow)

		app := fiber.New()
		app.Use(New(Config{
			CacheInvalidator: func(c fiber.Ctx) bool {
				if c.Get("X-Slow") == "1" {
					close(entered)
					<-release
				}
				return false
			},
		}))
		app.Get("/", func(c fiber.Ctx) error { return c.SendString("ok") })
		require.Equal(t, cacheMiss, getRoot(t, app, nil).cache)

		slowErr := make(chan error, 1)
		go func() {
			req := httptest.NewRequest(fiber.MethodGet, "/", http.NoBody)
			req.Header.Set("X-Slow", "1")
			resp, err := app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
			if err == nil {
				err = resp.Body.Close()
			}
			slowErr <- err
		}()
		<-entered

		// Served while the lookup is still running.
		require.Equal(t, cacheHit, getRoot(t, app, nil).cache)

		releaseSlow()
		require.NoError(t, <-slowErr)
	})

	// Requests overlap in the invalidator while others store, expire and evict entries; nothing may race or unbalance the accounting.
	for _, storage := range storageKinds {
		t.Run("overlapping with stores, expiry and eviction/"+storage, func(t *testing.T) {
			t.Parallel()

			// The limit exceeds what every request in flight can reserve at once, and is below the number of paths.
			const (
				workers  = 8
				requests = 120
				paths    = 24
				maxBytes = 120
			)
			payload := strings.Repeat("x", 12)

			probe := &accountingProbe{}
			clock := newTestClock(time.Unix(1_700_000_000, 0))
			var invalidations atomic.Int64
			cfg := Config{
				MaxBytes:   maxBytes,
				Expiration: 2 * time.Second,
				clock:      clock.Now,
				accounting: probe.record,
				// App code that overlaps with everything else: it yields, waits and ends entries.
				CacheInvalidator: func(fiber.Ctx) bool {
					switch invalidations.Add(1) % 5 {
					case 0:
						return true
					case 1:
						time.Sleep(200 * time.Microsecond)
					case 2:
						runtime.Gosched()
					}
					return false
				},
			}
			cfg.Storage = storageOf(storage)
			app := fiber.New()
			app.Use(New(cfg))
			app.Get("/:name", func(c fiber.Ctx) error { return c.SendString(payload) })

			request := func(name, cacheControl string) error {
				req := httptest.NewRequest(fiber.MethodGet, "/"+name, http.NoBody)
				if cacheControl != "" {
					req.Header.Set(fiber.HeaderCacheControl, cacheControl)
				}
				resp, err := app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
				if err != nil {
					return err
				}
				body, err := io.ReadAll(resp.Body)
				if closeErr := resp.Body.Close(); err == nil {
					err = closeErr
				}
				if err != nil {
					return err
				}
				if resp.StatusCode != fiber.StatusOK || string(body) != payload {
					return fmt.Errorf("/%s answered %d %q", name, resp.StatusCode, body)
				}
				return nil
			}

			// Entries run out of time while the requests are in flight.
			stop := make(chan struct{})
			var ticker sync.WaitGroup
			ticker.Go(func() {
				for {
					select {
					case <-stop:
						return
					default:
						clock.Add(300 * time.Millisecond)
						runtime.Gosched()
					}
				}
			})

			var wg sync.WaitGroup
			for worker := range workers {
				wg.Go(func() {
					rng := rand.New(rand.NewPCG(uint64(worker), 7))
					for range requests {
						name := "p" + strconv.Itoa(rng.IntN(paths))
						cacheControl := ""
						switch rng.IntN(6) {
						case 0:
							cacheControl = "max-age=0"
						case 1:
							cacheControl = "no-cache"
						}
						if err := request(name, cacheControl); err != nil {
							t.Errorf("worker %d: %v", worker, err)
							return
						}
					}
				})
			}
			wg.Wait()
			close(stop)
			ticker.Wait()

			require.NoError(t, request("p0", ""))
			require.LessOrEqual(t, probe.counted(), uint(maxBytes), "the accounting went out of balance")
		})
	}
}

func Test_CacheStorage_CustomHeaders(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	app.Use(New(Config{
		Storage:  memory.New(),
		MaxBytes: 10 * 1024 * 1024,
	}))

	app.Get("/", func(c fiber.Ctx) error {
		c.Response().Header.Set("Content-Type", "text/xml")
		c.Response().Header.Set("Content-Encoding", "utf8")
		return c.Send([]byte("<xml><value>Test</value></xml>"))
	})

	cacheTestAExpectRepeat(t, app, "/")
}

// Because time points are updated once every X milliseconds, entries in tests can often have
// equal expiration times and thus be in a random order. This closure hands out increasing
// time intervals to maintain strong ascending order of expiration
func stableAscendingExpiration() func(c1 fiber.Ctx, c2 *Config) time.Duration {
	i := 0
	return func(_ fiber.Ctx, _ *Config) time.Duration {
		i++
		return time.Hour * time.Duration(i)
	}
}

func Test_Cache_StorageMissingBodyIsMiss(t *testing.T) {
	t.Parallel()

	storage := newFailingCacheStorage()
	app := fiber.New()
	app.Use(New(Config{Storage: storage, Expiration: time.Hour}))

	var calls atomic.Int32
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("response-" + strconv.Itoa(int(calls.Add(1))))
	})

	cacheTestAExpect(t, app, "/", cacheMiss)

	// Drop the body but keep the metadata, as a backend evicting under pressure would.
	storage.mu.Lock()
	dropped := 0
	for key := range storage.data {
		if strings.HasSuffix(key, "_body") {
			delete(storage.data, key)
			dropped++
		}
	}
	storage.mu.Unlock()
	require.Equal(t, 1, dropped)

	resp := cacheTestAExpectStatus(t, app, "/", cacheMiss, fiber.StatusOK)
	require.Equal(t, "response-2", apptest.Body(t, resp))

	// The miss stored the fresh response again.
	cacheTestAExpectBody(t, app, "/", cacheHit, "response-2")
}

// accountingProbe records the MaxBytes bookkeeping after each request.
type accountingProbe struct {
	mu          sync.Mutex
	storedBytes uint
	heapLen     int
}

func (p *accountingProbe) record(storedBytes uint, heapLen int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.storedBytes, p.heapLen = storedBytes, heapLen
}

func (p *accountingProbe) counted() uint {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.storedBytes
}

func (p *accountingProbe) nodes() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.heapLen
}

func Test_Cache_NoCacheRefresh_KeepsAccounting(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name   string
		header string
	}{
		{name: "Cache-Control", header: fiber.HeaderCacheControl},
		{name: "Pragma", header: fiber.HeaderPragma},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			probe := &accountingProbe{}
			app := fiber.New()
			app.Use(New(Config{
				MaxBytes:   1500,
				Expiration: time.Hour,
				accounting: probe.record,
			}))
			app.Get("/*", func(c fiber.Ctx) error {
				return c.Send(make([]byte, 600))
			})

			cacheTestAExpect(t, app, "/x", cacheMiss)
			cacheTestAExpect(t, app, apptest.Req(fiber.MethodGet, "/x", apptest.WithHeader(tc.header, noCache)), cacheMiss)

			require.Equal(t, uint(600), probe.counted(), "the refreshed entry must be counted once")
			require.Equal(t, 1, probe.nodes(), "the refreshed entry must own a single heap node")

			// 600 + 600 fits within 1500, so /y must not evict anything.
			cacheTestAExpect(t, app, "/y", cacheMiss)
			cacheTestAExpect(t, app, "/x", cacheHit)
			cacheTestAExpect(t, app, "/y", cacheHit)

			require.Equal(t, uint(1200), probe.counted())
			require.Equal(t, 2, probe.nodes())
		})
	}
}

func Test_Cache_FailedRefresh(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		failedKey string // the storage write that fails
		keepsOld  bool   // whether the entry the refresh superseded stays counted and tracked
	}{
		{
			// Refresh the entry, failing the replacement's body store: the entry it
			// superseded stays in the backend, so it must stay counted and tracked
			// rather than leaking its bytes and its expiry.
			name:      "failed body store keeps the old entry tracked",
			failedKey: "set|" + cacheTestAKey("/") + "_body",
			keepsOld:  true,
		},
		{
			// Refresh the entry, letting the replacement body land but failing its
			// metadata: the cleanup deletes that body, so the entry the refresh
			// superseded can no longer be served and must not keep its accounting.
			name:      "failed metadata store after the body write drops the old entry",
			failedKey: "set|" + cacheTestAKey("/"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			storage := newFailingCacheStorage()
			probe := &accountingProbe{}
			app := fiber.New()
			app.Use(New(Config{
				Storage:    storage,
				MaxBytes:   1500,
				Expiration: time.Hour,
				accounting: probe.record,
			}))
			app.Get("/", func(c fiber.Ctx) error {
				return c.Send(make([]byte, 600))
			})

			apptest.Expect(t, app, "/", fiber.StatusOK)
			counted, nodes := probe.counted(), probe.nodes()
			require.NotZero(t, counted)
			require.Equal(t, 1, nodes)

			storage.mu.Lock()
			storage.errs[tc.failedKey] = errors.New("boom")
			storage.mu.Unlock()

			apptest.Expect(t, app, cacheTestAGetCC("/", noCache), fiber.StatusInternalServerError)

			if !tc.keepsOld {
				counted, nodes = 0, 0
			}
			require.Equal(t, counted, probe.counted())
			require.Equal(t, nodes, probe.nodes())
		})
	}
}

func Test_Cache_UncacheableResponseDoesNotEvict(t *testing.T) {
	t.Parallel()

	probe := &accountingProbe{}
	app := fiber.New()
	app.Use(New(Config{
		MaxBytes:   10,
		Expiration: time.Hour,
		accounting: probe.record,
	}))
	app.Get("/a", func(c fiber.Ctx) error {
		return c.SendString("aaaaa")
	})
	app.Get("/b", func(c fiber.Ctx) error {
		// max-age=0 is never stored, and the body only fits if /a is evicted.
		c.Set(fiber.HeaderCacheControl, "max-age=0")
		return c.SendString("bbbbbbbbbb")
	})

	cacheTestAExpect(t, app, "/a", cacheMiss)
	cacheTestAExpect(t, app, "/b", cacheUnreachable)

	require.Equal(t, uint(5), probe.counted(), "an uncacheable response must not displace stored entries")
	require.Equal(t, 1, probe.nodes())

	cacheTestAExpect(t, app, "/a", cacheHit)
}

func Test_Cache_MaxBytesOrder(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	app.Use(New(Config{
		MaxBytes:            2,
		ExpirationGenerator: stableAscendingExpiration(),
	}))

	app.Get("/*", func(c fiber.Ctx) error {
		return c.SendString("1")
	})

	cases := [][]string{
		// Insert a, b into cache of size 2 bytes (responses are 1 byte)
		{"/a", cacheMiss},
		{"/b", cacheMiss},
		{"/a", cacheHit},
		{"/b", cacheHit},
		// Add c -> a evicted
		{"/c", cacheMiss},
		{"/b", cacheHit},
		// Add a again -> b evicted
		{"/a", cacheMiss},
		{"/c", cacheHit},
		// Add b -> c evicted
		{"/b", cacheMiss},
		{"/c", cacheMiss},
	}

	for idx, tcase := range cases {
		rsp := apptest.Do(t, app, tcase[0])
		require.Equal(t, tcase[1], rsp.Header.Get("X-Cache"), "Case %v", idx)
	}
}

func Test_Cache_MaxBytesSizes(t *testing.T) {
	t.Parallel()
	app := fiber.New()

	app.Use(New(Config{
		MaxBytes:            7,
		ExpirationGenerator: stableAscendingExpiration(),
	}))

	app.Get("/*", func(c fiber.Ctx) error {
		path := c.RequestCtx().URI().LastPathSegment()
		size, err := strconv.Atoi(string(path))
		require.NoError(t, err)
		return c.Send(make([]byte, size))
	})

	cases := [][]string{
		{"/1", cacheMiss},
		{"/2", cacheMiss},
		{"/3", cacheMiss},
		{"/4", cacheMiss}, // 1+2+3+4 > 7 => 1,2 are evicted now
		{"/3", cacheHit},
		{"/1", cacheMiss},
		{"/2", cacheMiss},
		{"/8", cacheUnreachable}, // too big to cache -> unreachable
	}

	for idx, tcase := range cases {
		rsp := apptest.Do(t, app, tcase[0])
		require.Equal(t, tcase[1], rsp.Header.Get("X-Cache"), "Case %v", idx)
	}
}

func Test_Cache_UncacheableStatusCodes(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	app.Use(New())

	app.Get("/:statusCode", func(c fiber.Ctx) error {
		statusCode, err := strconv.Atoi(c.Params("statusCode"))
		require.NoError(t, err)
		return c.Status(statusCode).SendString("foo")
	})

	uncacheableStatusCodes := []int{
		// Informational responses
		fiber.StatusContinue,
		fiber.StatusSwitchingProtocols,
		fiber.StatusProcessing,
		fiber.StatusEarlyHints,

		// Successful responses
		fiber.StatusPartialContent, // never stored: this cache does not handle ranges (RFC 9111 §3.3)
		fiber.StatusCreated,
		fiber.StatusAccepted,
		fiber.StatusResetContent,
		fiber.StatusMultiStatus,
		fiber.StatusAlreadyReported,
		fiber.StatusIMUsed,

		// Redirection responses
		fiber.StatusFound,
		fiber.StatusSeeOther,
		fiber.StatusNotModified,
		fiber.StatusUseProxy,
		fiber.StatusSwitchProxy,
		fiber.StatusTemporaryRedirect,

		// Client error responses
		fiber.StatusBadRequest,
		fiber.StatusUnauthorized,
		fiber.StatusPaymentRequired,
		fiber.StatusForbidden,
		fiber.StatusNotAcceptable,
		fiber.StatusProxyAuthRequired,
		fiber.StatusRequestTimeout,
		fiber.StatusConflict,
		fiber.StatusLengthRequired,
		fiber.StatusPreconditionFailed,
		fiber.StatusRequestEntityTooLarge,
		fiber.StatusUnsupportedMediaType,
		fiber.StatusRequestedRangeNotSatisfiable,
		fiber.StatusExpectationFailed,
		fiber.StatusMisdirectedRequest,
		fiber.StatusUnprocessableEntity,
		fiber.StatusLocked,
		fiber.StatusFailedDependency,
		fiber.StatusTooEarly,
		fiber.StatusUpgradeRequired,
		fiber.StatusPreconditionRequired,
		fiber.StatusTooManyRequests,
		fiber.StatusRequestHeaderFieldsTooLarge,
		fiber.StatusTeapot,
		fiber.StatusUnavailableForLegalReasons,

		// Server error responses
		fiber.StatusInternalServerError,
		fiber.StatusBadGateway,
		fiber.StatusServiceUnavailable,
		fiber.StatusGatewayTimeout,
		fiber.StatusHTTPVersionNotSupported,
		fiber.StatusVariantAlsoNegotiates,
		fiber.StatusInsufficientStorage,
		fiber.StatusLoopDetected,
		fiber.StatusNotExtended,
		fiber.StatusNetworkAuthenticationRequired,
	}
	for _, v := range uncacheableStatusCodes {
		resp := cacheTestAExpect(t, app, fmt.Sprintf("/%d", v), cacheUnreachable)
		require.Equal(t, v, resp.StatusCode)
	}
}

func TestCacheAgeHeader(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	app.Use(New(Config{Expiration: 10 * time.Second}))
	app.Get("/", func(c fiber.Ctx) error { return c.SendString("ok") })

	resp := apptest.Do(t, app, "/")
	require.Equal(t, "0", resp.Header.Get(fiber.HeaderAge))

	time.Sleep(4 * time.Second)

	resp = cacheTestAExpect(t, app, "/", cacheHit)
	age, err := strconv.Atoi(resp.Header.Get(fiber.HeaderAge))
	require.NoError(t, err)
	require.Positive(t, age)
}

func TestCacheUpstreamAge(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	app.Use(New(Config{Expiration: 3 * time.Second}))
	app.Get("/", func(c fiber.Ctx) error {
		c.Set(fiber.HeaderAge, "5")
		return c.SendString("hi")
	})

	resp := apptest.Do(t, app, "/")
	require.Equal(t, "5", resp.Header.Get(fiber.HeaderAge))

	time.Sleep(1500 * time.Millisecond)

	resp = cacheTestAExpect(t, app, "/", cacheUnreachable)
	require.Equal(t, "5", resp.Header.Get(fiber.HeaderAge))
}

func Test_Cache_PartialContentNotStored(t *testing.T) {
	t.Parallel()

	const full = "0123456789"
	app := fiber.New()
	app.Use(New())
	app.Get("/", func(c fiber.Ctx) error {
		if c.Get(fiber.HeaderRange) == "bytes=0-4" {
			c.Set(fiber.HeaderContentRange, "bytes 0-4/10")
			return c.Status(fiber.StatusPartialContent).SendString(full[:5])
		}
		return c.SendString(full)
	})

	resp := cacheTestAExpectStatus(t, app, apptest.Req(fiber.MethodGet, "/", apptest.WithHeader(fiber.HeaderRange, "bytes=0-4")),
		cacheUnreachable, fiber.StatusPartialContent)
	require.Equal(t, full[:5], apptest.Body(t, resp))

	resp = cacheTestAExpectStatus(t, app, "/", cacheMiss, fiber.StatusOK)
	require.Empty(t, resp.Header.Get(fiber.HeaderContentRange))
	require.Equal(t, full, apptest.Body(t, resp))
}

func Test_CacheRequestMaxAgeRevalidates(t *testing.T) {
	t.Parallel()

	app, _ := cacheTestAOrigin(New(Config{
		Expiration: 30 * time.Second,
		KeyGenerator: func(c fiber.Ctx) string {
			return c.Path() + "|req-max-age-zero"
		},
	}), "", fiber.HeaderCacheControl, "public, max-age=30")

	cacheTestAExpectBody(t, app, "/", cacheMiss, "1")
	cacheTestAExpectBody(t, app, cacheTestAGetCC("/", "max-age=0"), cacheMiss, "2")
	cacheTestAExpectBody(t, app, "/", cacheHit, "2")
}

func Test_CacheExpiresFutureAllowsCaching(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		layout string
	}{
		{name: "IMF-fixdate", layout: http.TimeFormat},
		// Pins the RFC 9110 §5.6.7 acceptance set: a future Expires in the obsolete
		// RFC 850 format is a valid HTTP-date and must enable caching rather than
		// hit the parse-error (force-revalidate) path, consistently with how the
		// Date header is parsed.
		{name: "obsolete RFC 850 format", layout: "Monday, 02-Jan-06 15:04:05 GMT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app, _ := cacheTestAOrigin(New(Config{StoreResponseHeaders: true}), "expires",
				fiber.HeaderExpires, time.Now().Add(30*time.Second).UTC().Format(tc.layout))

			cacheTestAExpectBody(t, app, "/", cacheMiss, "expires1")
			cacheTestAExpectBody(t, app, "/", cacheHit, "expires1")
		})
	}
}

func Test_CacheExpiresPastPreventsCaching(t *testing.T) {
	t.Parallel()

	app, _ := cacheTestAOrigin(New(), "expires",
		fiber.HeaderExpires, time.Now().Add(-1*time.Minute).UTC().Format(http.TimeFormat))

	cacheTestAExpectBody(t, app, "/", cacheUnreachable, "expires1")
	cacheTestAExpectBody(t, app, "/", cacheUnreachable, "expires2")
}

func Test_CacheAllowsSharedCacheRevalidateWithAuthorization(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name         string
		cacheControl string
		keySuffix    string
		prefix       string // of the origin's answers
	}{
		{name: "must-revalidate", cacheControl: "must-revalidate, max-age=60", keySuffix: "|must-revalidate-auth", prefix: "auth"},
		{name: "proxy-revalidate", cacheControl: "proxy-revalidate, max-age=60", keySuffix: "|proxy-revalidate-auth", prefix: "proxy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app, _ := cacheTestAOrigin(New(Config{
				Expiration: 30 * time.Second,
				KeyGenerator: func(c fiber.Ctx) string {
					return c.Path() + tc.keySuffix
				},
			}), tc.prefix, fiber.HeaderCacheControl, tc.cacheControl)

			authorized := func() *http.Request {
				return apptest.Req(fiber.MethodGet, "/", apptest.WithHeader(fiber.HeaderAuthorization, "Bearer token"))
			}

			cacheTestAExpectBody(t, app, authorized(), cacheMiss, tc.prefix+"1")
			cacheTestAExpectBody(t, app, authorized(), cacheHit, tc.prefix+"1")
		})
	}
}

func Test_CacheInvalidExpiresStoredAsStale(t *testing.T) {
	t.Parallel()

	storage := newFailingCacheStorage()

	app, _ := cacheTestAOrigin(New(Config{
		Expiration: 30 * time.Second,
		KeyGenerator: func(c fiber.Ctx) string {
			return c.Path() + "|invalid-expires"
		},
		Storage: storage,
	}), "body", fiber.HeaderCacheControl, "public", fiber.HeaderExpires, "invalid-date")

	expectedKey := cacheKeyVersion + "|GET|/|invalid-expires"

	for i := 1; i <= 3; i++ {
		cacheTestAExpectBody(t, app, "/", cacheMiss, "body"+strconv.Itoa(i))
		require.Contains(t, storage.data, expectedKey)
		require.Contains(t, storage.data, expectedKey+"_body")
	}
}

func Test_CacheStoreDoesNotAgeItsOwnResponse(t *testing.T) {
	t.Parallel()

	// A response fiber just generated has age 0, so a one-second lifetime has to
	// survive the store phase no matter how much time passes inside it.
	clock := &tickingClock{now: time.Now().Truncate(time.Second)}
	app := fiber.New()
	app.Use(New(Config{clock: clock.Now}))

	app.Get("/", func(c fiber.Ctx) error {
		c.Set(fiber.HeaderCacheControl, "max-age=1")
		return c.SendString("cached")
	})

	cacheTestAExpect(t, app, "/", cacheMiss)
}

func Test_CacheStoreExpiresAnchoredToDate(t *testing.T) {
	t.Parallel()

	// An Expires lifetime must be measured from the instant the Date header is
	// anchored to, or clock reads inside the store phase consume it.
	clock := &tickingClock{now: time.Now().Truncate(time.Second)}
	app := fiber.New()
	app.Use(New(Config{clock: clock.Now}))

	app.Get("/", func(c fiber.Ctx) error {
		c.Set(fiber.HeaderExpires, clock.Now().Add(2*time.Second).UTC().Format(http.TimeFormat))
		return c.SendString("cached")
	})

	cacheTestAExpect(t, app, "/", cacheMiss)
}

func Test_CacheSMaxAgeOverridesMaxAgeWhenShorter(t *testing.T) {
	t.Parallel()

	// Drive freshness from a manually advanced clock so the immediate cache hit
	// can never straddle a whole-second boundary (the previous time.Sleep based
	// version flaked under -race -count -shuffle).
	clock := newTestClock(time.Now().Truncate(time.Second))
	app, _ := cacheTestAOrigin(New(Config{clock: clock.Now}), "", fiber.HeaderCacheControl, "public, max-age=10, s-maxage=1")

	cacheTestAExpect(t, app, "/", cacheMiss)
	cacheTestAExpectBody(t, app, "/", cacheHit, "1")

	// Advance past the 1s s-maxage window; max-age=10 is longer, so the shorter
	// s-maxage must win and the entry must be treated as stale.
	clock.Add(1100 * time.Millisecond)

	cacheTestAExpectBody(t, app, "/", cacheMiss, "2")
}

func Test_CacheSMaxAgeOverridesMaxAgeWhenLonger(t *testing.T) {
	t.Parallel()

	// Drive freshness from a manually advanced clock so the checks never straddle
	// a whole-second boundary (the previous time.Sleep based version flaked under
	// -race -count -shuffle).
	clock := newTestClock(time.Now().Truncate(time.Second))
	app, _ := cacheTestAOrigin(New(Config{clock: clock.Now}), "", fiber.HeaderCacheControl, "public, max-age=1, s-maxage=2")

	cacheTestAExpect(t, app, "/", cacheMiss)

	// Past max-age=1 but within the longer s-maxage=2 window: the longer
	// s-maxage must win, so the entry is still fresh.
	clock.Add(1200 * time.Millisecond)

	cacheTestAExpectBody(t, app, "/", cacheHit, "1")

	// Advance past the 2s s-maxage window; the entry must now be stale.
	clock.Add(1700 * time.Millisecond)

	cacheTestAExpectBody(t, app, "/", cacheMiss, "2")
}

func Test_CacheOnlyIfCachedMiss(t *testing.T) {
	t.Parallel()

	app, calls := cacheTestAOrigin(New(), "")

	cacheTestAExpectStatus(t, app, cacheTestAGetCC("/", "only-if-cached"), cacheUnreachable, fiber.StatusGatewayTimeout)
	require.Zero(t, calls.Load())
}

func Test_CacheOnlyIfCachedStaleNotServed(t *testing.T) {
	t.Parallel()

	app, calls := cacheTestAOrigin(New(), "", fiber.HeaderCacheControl, "public, max-age=1")

	cacheTestAExpect(t, app, "/", cacheMiss)

	time.Sleep(1500 * time.Millisecond)

	cacheTestAExpectStatus(t, app, cacheTestAGetCC("/", "only-if-cached"), cacheUnreachable, fiber.StatusGatewayTimeout)
	require.Equal(t, int32(1), calls.Load())
}

// Test_CacheOnlyIfCachedWithNoCache covers a request that asks for a stored
// response and refuses to have it served unvalidated. RFC 9111 Section 5.2.1.7
// has a cache that cannot satisfy only-if-cached without contacting the origin
// answer 504, and validating a stored response is contacting the origin. The
// fresh entry used to fall through to the handler instead.
func Test_CacheOnlyIfCachedWithNoCache(t *testing.T) {
	t.Parallel()

	requests := []struct {
		header map[string]string
		name   string
	}{
		{name: "no-cache beside only-if-cached", header: map[string]string{fiber.HeaderCacheControl: "no-cache, only-if-cached"}},
		{name: "only-if-cached before no-cache", header: map[string]string{fiber.HeaderCacheControl: "only-if-cached, no-cache"}},
		{name: "Pragma no-cache", header: map[string]string{fiber.HeaderCacheControl: "only-if-cached", fiber.HeaderPragma: "no-cache"}},
		{name: "max-age=0 behaves the same way", header: map[string]string{fiber.HeaderCacheControl: "max-age=0, only-if-cached"}},
	}

	for _, storageName := range []string{"in-memory", "storage"} {
		for _, tc := range requests {
			t.Run(storageName+"/"+tc.name, func(t *testing.T) {
				t.Parallel()

				cfg := Config{}
				if storageName == "storage" {
					cfg.Storage = memory.New()
				}

				app, calls := cacheTestAOrigin(New(cfg), "origin-", fiber.HeaderCacheControl, "public, max-age=60")

				// A fresh entry, stored by an ordinary request.
				cacheTestAExpect(t, app, "/", cacheMiss)
				require.Equal(t, int32(1), calls.Load())

				req := apptest.Req(fiber.MethodGet, "/")
				for k, v := range tc.header {
					req.Header.Set(k, v)
				}
				cacheTestAExpectStatus(t, app, req, cacheUnreachable, fiber.StatusGatewayTimeout)
				require.Equal(t, int32(1), calls.Load(), "only-if-cached must not reach the handler")

				// The refusal leaves the stored response alone.
				resp := cacheTestAExpectStatus(t, app, "/", cacheHit, fiber.StatusOK)
				require.Equal(t, "origin-1", apptest.Body(t, resp))
				require.Equal(t, int32(1), calls.Load())
			})
		}
	}
}

func Test_CacheMaxStaleServesStaleResponse(t *testing.T) {
	t.Parallel()

	app, calls := cacheTestAOrigin(New(), "", fiber.HeaderCacheControl, "public, max-age=2")

	cacheTestAExpect(t, app, "/", cacheMiss)
	cacheTestAExpect(t, app, "/", cacheHit)

	time.Sleep(2500 * time.Millisecond)

	resp := apptest.Do(t, app, cacheTestAGetCC("/", "max-stale=5"))
	require.Equalf(t, cacheHit, resp.Header.Get("X-Cache"), "dirs=%+v Age=%s count=%d", parseRequestCacheControl([]byte("max-stale=5")), resp.Header.Get(fiber.HeaderAge), calls.Load())
	require.Equal(t, "1", apptest.Body(t, resp))
	require.Equal(t, int32(1), calls.Load())
}

func Test_CacheMaxStaleRespectsMustRevalidate(t *testing.T) {
	t.Parallel()

	app, calls := cacheTestAOrigin(New(), "", fiber.HeaderCacheControl, "public, max-age=1, must-revalidate")

	cacheTestAExpect(t, app, "/", cacheMiss)

	time.Sleep(1500 * time.Millisecond)

	cacheTestAExpectBody(t, app, cacheTestAGetCC("/", "max-stale=30"), cacheMiss, "2")
	require.Equal(t, int32(2), calls.Load())
}

func Test_CacheMaxStaleRespectsProxyRevalidateSharedAuth(t *testing.T) {
	t.Parallel()

	// s-maxage=2, not 1: the store phase reads cfg.now() twice and charges the
	// whole second in between as apparent age, so a one-second lifetime can be
	// consumed entirely and the entry reported unreachable instead of cached.
	app, calls := cacheTestAOrigin(New(), "", fiber.HeaderCacheControl, "s-maxage=2, proxy-revalidate")

	cacheTestAExpect(t, app, apptest.Req(fiber.MethodGet, "/", apptest.WithHeader(fiber.HeaderAuthorization, "Bearer abc")), cacheMiss)

	time.Sleep(2500 * time.Millisecond)

	req := apptest.Req(fiber.MethodGet, "/",
		apptest.WithHeader(fiber.HeaderAuthorization, "Bearer abc"),
		apptest.WithHeader(fiber.HeaderCacheControl, "max-stale=30"))

	cacheTestAExpectBody(t, app, req, cacheMiss, "2")
	require.Equal(t, int32(2), calls.Load())
}

func Test_CachePreservesCacheControlHeaders(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(New())

	expires := time.Now().Add(10 * time.Second).UTC().Format(http.TimeFormat)
	app.Get("/", func(c fiber.Ctx) error {
		c.Set(fiber.HeaderCacheControl, "public, max-age=5, immutable")
		c.Set(fiber.HeaderExpires, expires)
		c.Set(fiber.HeaderETag, `W/"abc"`)
		return c.SendString("ok")
	})

	for _, want := range []string{cacheMiss, cacheHit} {
		resp := cacheTestAExpect(t, app, "/", want)
		require.Equal(t, "public, max-age=5, immutable", resp.Header.Get(fiber.HeaderCacheControl))
		require.Equal(t, expires, resp.Header.Get(fiber.HeaderExpires))
		require.Equal(t, `W/"abc"`, resp.Header.Get(fiber.HeaderETag))
	}
}

func setResponseDate(date time.Time) fiber.Handler {
	return func(c fiber.Ctx) error {
		if err := c.Next(); err != nil {
			return err
		}
		c.Response().Header.Set(fiber.HeaderDate, date.UTC().Format(http.TimeFormat))
		return nil
	}
}

func Test_CacheDateAndAgeHandling(t *testing.T) {
	t.Parallel()

	type testCase struct {
		name             string
		cacheControl     string
		cacheHeader      string
		dateOffset       time.Duration
		expiration       time.Duration
		expectAgeAtLeast int
		expectCount      int
		originAge        int
	}

	cases := []testCase{
		{
			name:             "age derived from past date without Age header",
			dateOffset:       -1 * time.Minute,
			cacheControl:     "public, max-age=120",
			cacheHeader:      cacheHit,
			expiration:       5 * time.Minute,
			expectAgeAtLeast: 1,
			expectCount:      1,
		},
		{
			name:         "stale due to past date despite max-age",
			dateOffset:   -90 * time.Second,
			cacheControl: "public, max-age=30",
			cacheHeader:  cacheUnreachable,
			expiration:   5 * time.Minute,
			expectCount:  2,
			originAge:    90,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app := fiber.New()
			app.Use(New(Config{Expiration: tc.expiration}))
			app.Use(setResponseDate(time.Now().Add(tc.dateOffset).UTC()))

			var count int
			app.Get("/", func(c fiber.Ctx) error {
				count++
				if tc.originAge > 0 {
					c.Response().Header.Set(fiber.HeaderAge, strconv.Itoa(tc.originAge))
				}
				c.Set(fiber.HeaderCacheControl, tc.cacheControl)
				return c.SendString(strconv.Itoa(count))
			})

			apptest.Do(t, app, "/")

			if tc.cacheHeader == cacheHit {
				time.Sleep(2 * time.Second)
			}

			resp := apptest.Do(t, app, "/")
			require.Equal(t, tc.cacheHeader, resp.Header.Get("X-Cache"))
			if tc.cacheHeader == cacheHit {
				ageVal, err := strconv.Atoi(resp.Header.Get(fiber.HeaderAge))
				require.NoError(t, err)
				require.GreaterOrEqual(t, ageVal, tc.expectAgeAtLeast)
				require.Equal(t, 1, count)
			} else {
				require.Equal(t, strconv.Itoa(tc.expectCount), apptest.Body(t, resp))
				require.Equal(t, tc.expectCount, count)
			}
		})
	}
}

// cacheTestAMutatedHit stores a response for expiration behind a storage that
// lets mutate rewrite the metadata of the entry on its way in, so a test can
// plant values the cache never writes itself, and returns the cache hit that
// serves it. The origin sets the response headers given as name and value pairs.
func cacheTestAMutatedHit(t *testing.T, expiration time.Duration, mutate func(it *item), headers ...string) *http.Response {
	t.Helper()

	storage := newMutatingStorage(func(key string, val []byte) []byte {
		if strings.HasSuffix(key, "_body") {
			return val
		}

		var it item
		if _, err := it.UnmarshalMsg(val); err != nil {
			return val
		}

		mutate(&it)
		updated, err := it.MarshalMsg(nil)
		if err != nil {
			return val
		}

		return updated
	})
	app, _ := cacheTestAOrigin(New(Config{Expiration: expiration, Storage: storage}), "", headers...)

	cacheTestAExpect(t, app, "/", cacheMiss)
	return cacheTestAExpect(t, app, "/", cacheHit)
}

func Test_CacheClampsInvalidStoredDate(t *testing.T) {
	t.Parallel()

	resp := cacheTestAMutatedHit(t, time.Minute, func(it *item) { it.date = uint64(math.MaxInt64) + 1024 },
		fiber.HeaderCacheControl, "public, max-age=60")

	parsedDate, err := http.ParseTime(resp.Header.Get(fiber.HeaderDate))
	require.NoError(t, err)
	require.WithinDuration(t, time.Now(), parsedDate, time.Minute)

	ageVal, err := strconv.Atoi(resp.Header.Get(fiber.HeaderAge))
	require.NoError(t, err)
	require.Less(t, ageVal, 60)
	require.GreaterOrEqual(t, ageVal, 0)
}

func Test_CacheClampsFutureStoredDate(t *testing.T) {
	t.Parallel()

	resp := cacheTestAMutatedHit(t, time.Minute, func(it *item) {
		future := time.Now().Add(2 * time.Second).UTC()
		it.date = uint64(max(future.Unix(), 0))
	}, fiber.HeaderCacheControl, "public, max-age=60")

	parsedDate, err := http.ParseTime(resp.Header.Get(fiber.HeaderDate))
	require.NoError(t, err)
	require.False(t, parsedDate.After(time.Now()))

	ageVal, err := strconv.Atoi(resp.Header.Get(fiber.HeaderAge))
	require.NoError(t, err)
	require.GreaterOrEqual(t, ageVal, 0)
}

func Test_CacheAgeClockStepsBackwards(t *testing.T) {
	t.Parallel()

	clock := newTestClock(time.Now().Truncate(time.Second))
	app := fiber.New()
	app.Use(New(Config{clock: clock.Now}))
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("cached")
	})

	cacheTestAExpect(t, app, "/", cacheMiss)

	clock.Add(-time.Hour)

	resp := cacheTestAExpect(t, app, "/", cacheHit)
	require.Equal(t, "0", resp.Header.Get(fiber.HeaderAge))
	require.Empty(t, resp.Header.Values(fiber.HeaderWarning))
}

func Test_RequestPragmaNoCacheTriggersMiss(t *testing.T) {
	t.Parallel()

	app, _ := cacheTestAOrigin(New(Config{
		Expiration: time.Minute,
	}), "body", fiber.HeaderCacheControl, "public, max-age=60")

	cacheTestAExpectBody(t, app, "/", cacheMiss, "body1")
	cacheTestAExpectBody(t, app, "/", cacheHit, "body1")
	cacheTestAExpectBody(t, app, apptest.Req(fiber.MethodGet, "/", apptest.WithHeader(fiber.HeaderPragma, "no-cache")), cacheMiss, "body2")
	cacheTestAExpectBody(t, app, "/", cacheHit, "body2")
}

func Test_CacheStaleResponseAddsWarning110(t *testing.T) {
	t.Parallel()

	app, _ := cacheTestAOrigin(New(Config{
		Expiration: 2 * time.Second,
	}), "body", fiber.HeaderCacheControl, "public, max-age=1")

	cacheTestAExpect(t, app, "/", cacheMiss)

	req := cacheTestAGetCC("/", "max-stale=5")

	// Wait for the cached response to become stale (max-age=1)
	// Add extra time to ensure the entry has expired
	time.Sleep(1200 * time.Millisecond)

	var resp *http.Response
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp = apptest.Do(t, app, req)
		if resp.Header.Get("X-Cache") == cacheHit {
			ageVal, err := strconv.Atoi(resp.Header.Get(fiber.HeaderAge))
			require.NoError(t, err)
			if ageVal >= 1 {
				// Check that Warning header is present before breaking
				warnings := resp.Header.Values(fiber.HeaderWarning)
				if len(warnings) > 0 {
					break
				}
			}
		}
		require.True(t, time.Now().Before(deadline), "response did not become stale before deadline")
		time.Sleep(50 * time.Millisecond)
	}

	warnings := resp.Header.Values(fiber.HeaderWarning)
	require.NotEmpty(t, warnings, "Warning header should be present when serving stale response")
	require.Contains(t, strings.Join(warnings, "\n"), "110", "warning 110 not found in %v", warnings)
}

func Test_CacheHeuristicFreshnessAddsWarning113(t *testing.T) {
	t.Parallel()

	app, _ := cacheTestAOrigin(New(Config{
		Expiration: 2 * time.Second,
	}), "body", fiber.HeaderCacheControl, "public, max-age=60")

	cacheTestAExpect(t, app, "/", cacheMiss)
	resp := cacheTestAExpect(t, app, "/", cacheHit)

	for _, w := range resp.Header.Values(fiber.HeaderWarning) {
		require.NotContains(t, w, "113", "warning 113 should not be present for explicitly fresh responses")
	}
}

func Test_CacheHeuristicFreshnessAddsWarning113AfterThreshold(t *testing.T) {
	t.Parallel()

	resp := cacheTestAMutatedHit(t, 2*time.Second, func(it *item) {
		oldDate := time.Now().Add(-25 * time.Hour).UTC()
		it.date = uint64(max(oldDate.Unix(), 0))

		future := time.Now().Add(48 * time.Hour).UTC()
		it.exp = uint64(max(future.Unix(), 0))
		it.ttl = uint64((48 * time.Hour) / time.Second)
	})

	warnings := resp.Header.Values(fiber.HeaderWarning)
	require.NotEmpty(t, warnings)
	require.Contains(t, strings.Join(warnings, "\n"), "113", "warning 113 not found in %v", warnings)
}

func Test_CacheAgeHeaderIsCappedAtMaxDeltaSeconds(t *testing.T) {
	t.Parallel()

	const veryLargeAge = uint64(math.MaxInt32) + 1000
	resp := cacheTestAMutatedHit(t, time.Minute, func(it *item) { it.age = veryLargeAge },
		fiber.HeaderCacheControl, "public, max-age=60")

	ageVal, err := strconv.Atoi(resp.Header.Get(fiber.HeaderAge))
	require.NoError(t, err)
	require.Equal(t, math.MaxInt32, ageVal)
}

func Test_CacheMinFreshForcesRevalidation(t *testing.T) {
	t.Parallel()

	app, calls := cacheTestAOrigin(New(), "", fiber.HeaderCacheControl, "public, max-age=5")

	cacheTestAExpectBody(t, app, "/", cacheMiss, "1")

	resp := apptest.Do(t, app, cacheTestAGetCC("/", "min-fresh=10"))
	require.Equalf(t, cacheMiss, resp.Header.Get("X-Cache"), "dirs=%+v Age=%s count=%d", parseRequestCacheControl([]byte("min-fresh=10")), resp.Header.Get(fiber.HeaderAge), calls.Load())
	require.Equal(t, "2", apptest.Body(t, resp))

	cacheTestAExpectBody(t, app, "/", cacheHit, "2")
}

func Test_CachePermanentRedirectCached(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(New(Config{
		Expiration:           30 * time.Second,
		StoreResponseHeaders: true,
		KeyGenerator: func(c fiber.Ctx) string {
			return c.Path() + "|status-308"
		},
	}))

	var count int
	app.Get("/", func(c fiber.Ctx) error {
		count++
		c.Set(fiber.HeaderCacheControl, "public, max-age=30")
		c.Set(fiber.HeaderLocation, "/dest")
		return c.Status(fiber.StatusPermanentRedirect).SendString("redir" + strconv.Itoa(count))
	})

	for _, want := range []string{cacheMiss, cacheHit} {
		resp := cacheTestAExpectStatus(t, app, "/", want, fiber.StatusPermanentRedirect)
		require.Equal(t, "redir1", apptest.Body(t, resp))
		require.Equal(t, "/dest", resp.Header.Get(fiber.HeaderLocation))
	}
}

func Test_CacheNoStoreAndNoCacheDirectives(t *testing.T) {
	t.Parallel()

	for _, directive := range []string{"no-store", "no-cache"} {
		t.Run(directive, func(t *testing.T) {
			t.Parallel()

			app, _ := cacheTestAOrigin(New(), "", fiber.HeaderCacheControl, directive)

			cacheTestAExpectBody(t, app, "/", cacheUnreachable, "1")
			cacheTestAExpectBody(t, app, "/", cacheUnreachable, "2")
		})
	}
}

func Test_CacheNoCacheDirectiveOverridesExistingEntry(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(New())

	var noCacheMode atomic.Bool
	app.Get("/", func(c fiber.Ctx) error {
		if noCacheMode.Load() {
			c.Set(fiber.HeaderCacheControl, "no-cache")
			return c.SendString("no-cache")
		}

		c.Set(fiber.HeaderCacheControl, "public, max-age=60")
		return c.SendString("cacheable")
	})

	cacheTestAExpectBody(t, app, "/", cacheMiss, "cacheable")

	noCacheMode.Store(true)
	cacheTestAExpectBody(t, app, cacheTestAGetCC("/", "no-cache"), cacheUnreachable, "no-cache")
	cacheTestAExpectBody(t, app, "/", cacheUnreachable, "no-cache")
}

func Test_CacheRespectsUpstreamAgeForFreshness(t *testing.T) {
	t.Parallel()

	t.Run("skipsCachingWhenAgeExhaustsFreshness", func(t *testing.T) {
		t.Parallel()

		app, _ := cacheTestAOrigin(New(Config{
			KeyGenerator: func(c fiber.Ctx) string {
				return c.Path() + "|age-exhausted"
			},
		}), "", fiber.HeaderCacheControl, "public, max-age=2", fiber.HeaderAge, "2")

		cacheTestAExpectBody(t, app, "/", cacheUnreachable, "1")
		cacheTestAExpectBody(t, app, "/", cacheUnreachable, "2")
	})

	t.Run("expiresAfterRemainingLifetime", func(t *testing.T) {
		t.Parallel()

		// Manually advanced like the s-maxage tests: with one second of lifetime
		// left, a tick of the real clock would expire the entry before the hit.
		clock := newTestClock(time.Now().Truncate(time.Second))
		app, _ := cacheTestAOrigin(New(Config{
			clock: clock.Now,
			KeyGenerator: func(c fiber.Ctx) string {
				return c.Path() + "|age-remaining"
			},
		}), "", fiber.HeaderCacheControl, "public, max-age=2", fiber.HeaderAge, "1")

		cacheTestAExpectBody(t, app, "/", cacheMiss, "1")
		cacheTestAExpectBody(t, app, "/", cacheHit, "1")

		clock.Add(1500 * time.Millisecond)

		cacheTestAExpectBody(t, app, "/", cacheMiss, "2")
	})
}

func Test_CacheVarySeparatesVariants(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(New(Config{
		KeyGenerator: func(c fiber.Ctx) string {
			return c.Path() + "|vary-separated"
		},
	}))

	var count int
	app.Get("/", func(c fiber.Ctx) error {
		count++
		c.Set(fiber.HeaderVary, fiber.HeaderAcceptLanguage)
		return c.SendString(c.Get(fiber.HeaderAcceptLanguage) + strconv.Itoa(count))
	})

	cacheTestAExpectBody(t, app, cacheTestAAcceptLanguage("en"), cacheMiss, "en1")
	cacheTestAExpectBody(t, app, cacheTestAAcceptLanguage("fr"), cacheMiss, "fr2")
	cacheTestAExpectBody(t, app, cacheTestAAcceptLanguage("en"), cacheHit, "en1")
	cacheTestAExpectBody(t, app, cacheTestAAcceptLanguage("fr"), cacheHit, "fr2")
}

func Test_CacheVaryStarUncacheable(t *testing.T) {
	t.Parallel()

	for _, disableVaryHeaders := range []bool{false, true} {
		t.Run(fmt.Sprintf("DisableVaryHeaders=%t", disableVaryHeaders), func(t *testing.T) {
			t.Parallel()

			app := fiber.New()
			app.Use(New(Config{
				DisableVaryHeaders: disableVaryHeaders,
				KeyGenerator: func(c fiber.Ctx) string {
					return c.Path() + "|vary-star"
				},
			}))

			var count int
			app.Get("/", func(c fiber.Ctx) error {
				count++
				c.Set(fiber.HeaderVary, "*")
				return c.SendString(strconv.Itoa(count))
			})

			resp := apptest.Do(t, app, "/")
			require.Equal(t, cacheUnreachable, resp.Header.Get("X-Cache"))
			body := apptest.BodyBytes(t, resp)
			require.Equal(t, "1", string(body))

			resp = apptest.Do(t, app, "/")
			require.Equal(t, cacheUnreachable, resp.Header.Get("X-Cache"))
			body = apptest.BodyBytes(t, resp)
			require.Equal(t, "2", string(body))
		})
	}
}

func Test_CachePrivateDirective(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(New())

	var count int
	app.Get("/", func(c fiber.Ctx) error {
		count++
		c.Set(fiber.HeaderCacheControl, "private")
		return c.SendString(strconv.Itoa(count))
	})

	cacheTestBExpectUncacheable(t, app)
}

// cacheTestBExpect sends target through app and requires the X-Cache status
// want. The body is read and closed, and stays readable from the response.
func cacheTestBExpect[T apptest.Target](t *testing.T, app *fiber.App, target T, want string) *http.Response {
	t.Helper()

	resp := apptest.Do(t, app, target)
	require.Equal(t, want, resp.Header.Get("X-Cache"))
	apptest.BodyBytes(t, resp)
	return resp
}

// cacheTestBExpectBody is cacheTestBExpect that also requires the body.
func cacheTestBExpectBody[T apptest.Target](t *testing.T, app *fiber.App, target T, want, body string) {
	t.Helper()

	resp := cacheTestBExpect(t, app, target, want)
	require.Equal(t, body, apptest.Body(t, resp))
}

// cacheTestBExpectUncacheable sends GET / with opts through app twice and
// requires both answers to come straight from the handler, which numbers its
// calls: X-Cache is unreachable and the bodies are "1" and "2".
func cacheTestBExpectUncacheable(t *testing.T, app *fiber.App, opts ...apptest.Option) {
	t.Helper()

	for _, body := range []string{"1", "2"} {
		cacheTestBExpectBody(t, app, apptest.Req(fiber.MethodGet, "/", opts...), cacheUnreachable, body)
	}
}

// Test_CacheUncacheableWithAuthorization asserts that no answer to a request
// carrying Authorization is stored: with the response saying private, and with
// the default config alone, which skips such requests.
func Test_CacheUncacheableWithAuthorization(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name         string
		cacheControl string
	}{
		{name: "private directive", cacheControl: "private"},
		{name: "skipped by default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app := fiber.New()
			app.Use(New())

			var count int
			app.Get("/", func(c fiber.Ctx) error {
				count++
				if tc.cacheControl != "" {
					c.Set(fiber.HeaderCacheControl, tc.cacheControl)
				}
				return c.SendString(strconv.Itoa(count))
			})

			cacheTestBExpectUncacheable(t, app, apptest.WithHeader(fiber.HeaderAuthorization, "Bearer token"))
		})
	}
}

func Test_CachePrivateDirectiveInvalidatesExistingEntry(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(New())

	var privateMode atomic.Bool
	app.Get("/", func(c fiber.Ctx) error {
		if privateMode.Load() {
			c.Set(fiber.HeaderCacheControl, "private")
			return c.SendString("private")
		}

		c.Set(fiber.HeaderCacheControl, "public, max-age=60")
		return c.SendString("public")
	})

	cacheTestBExpectBody(t, app, "/", cacheMiss, "public")

	privateMode.Store(true)
	noCache := apptest.Req(fiber.MethodGet, "/", apptest.WithHeader(fiber.HeaderCacheControl, "no-cache"))
	cacheTestBExpectBody(t, app, noCache, cacheUnreachable, "private")

	privateMode.Store(false)
	cacheTestBExpectBody(t, app, "/", cacheMiss, "public")
}

// cacheTestBPathKeyedApp returns an app running the cache middleware with cfg,
// except that entries live an hour and are keyed by the request path alone.
func cacheTestBPathKeyedApp(cfg *Config) *fiber.App {
	keyed := *cfg
	keyed.Expiration = time.Hour
	keyed.KeyGenerator = pathKey

	app := fiber.New()
	app.Use(New(keyed))
	return app
}

// cacheTestBInvalidateByQuery is a CacheInvalidator that fires on ?invalidate=true.
func cacheTestBInvalidateByQuery(c fiber.Ctx) bool {
	return fiber.Query[bool](c, "invalidate")
}

func Test_CacheStorage_UncacheableRevalidationDeletesStaleEntry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		setUncacheableHeaders func(fiber.Ctx)
		name                  string
	}{
		{
			name: "private",
			setUncacheableHeaders: func(c fiber.Ctx) {
				c.Set(fiber.HeaderCacheControl, "private")
			},
		},
		{
			name: "no-cache",
			setUncacheableHeaders: func(c fiber.Ctx) {
				c.Set(fiber.HeaderCacheControl, "no-cache")
			},
		},
		{
			name: "no-store",
			setUncacheableHeaders: func(c fiber.Ctx) {
				c.Set(fiber.HeaderCacheControl, "no-store")
			},
		},
		{
			name: "vary-star",
			setUncacheableHeaders: func(c fiber.Ctx) {
				c.Set(fiber.HeaderVary, "*")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			storage := newContextRecorderStorage()
			var returnUncacheable atomic.Bool

			app := cacheTestBPathKeyedApp(&Config{
				CacheInvalidator: cacheTestBInvalidateByQuery,
				MaxBytes:         5,
				Storage:          storage,
			})
			app.Get("/cached", func(c fiber.Ctx) error {
				if returnUncacheable.Load() {
					tt.setUncacheableHeaders(c)
					return c.SendString("newer")
				}

				c.Set(fiber.HeaderCacheControl, "public, max-age=60, must-revalidate")
				return c.SendString("stale")
			})
			app.Get("/next", func(c fiber.Ctx) error {
				c.Set(fiber.HeaderCacheControl, "public, max-age=60")
				return c.SendString("fresh")
			})

			cacheTestBExpect(t, app, "/cached", cacheMiss)

			key := cacheKeyVersion + "|GET|/cached"
			bodyKey := key + "_body"
			storage.mu.RLock()
			require.Contains(t, storage.data, key)
			require.Contains(t, storage.data, bodyKey)
			storage.mu.RUnlock()

			returnUncacheable.Store(true)
			resp := apptest.Expect(t, app, "/cached?invalidate=true", fiber.StatusOK)
			require.Equal(t, cacheUnreachable, resp.Header.Get("X-Cache"))
			require.Equal(t, "newer", apptest.Body(t, resp))
			require.NoError(t, resp.Body.Close())

			storage.mu.RLock()
			require.NotContains(t, storage.data, key)
			require.NotContains(t, storage.data, bodyKey)
			storage.mu.RUnlock()

			deletes := storage.recordedDeletes()
			require.Equal(t, []contextRecord{{key: key}, {key: bodyKey}}, deletes)
			for _, get := range storage.recordedGets() {
				require.NotEqual(t, bodyKey, get.key, "revalidation must not fetch the stale body")
			}

			// If heap accounting retained the deleted entry, caching /next would
			// try to evict it and encounter these sentinel errors.
			storage.mu.Lock()
			storage.errs["del|"+key] = errors.New("stale metadata remained in heap")
			storage.errs["del|"+bodyKey] = errors.New("stale body remained in heap")
			storage.mu.Unlock()

			cacheTestBExpect(t, app, "/next", cacheMiss)
			cacheTestBExpect(t, app, "/next", cacheHit)

			resp = apptest.Do(t, app, "/cached")
			require.NotEqual(t, cacheHit, resp.Header.Get("X-Cache"))
			require.Equal(t, "newer", apptest.Body(t, resp))
			require.NoError(t, resp.Body.Close())
		})
	}
}

// cacheTestBFlipping returns a handler that answers "stale", cacheable under
// staleCC and with the staleExtra header name and value pairs, until flipped is
// set, and "newer" under newerCC from then on.
func cacheTestBFlipping(flipped *atomic.Bool, staleCC, newerCC string, staleExtra ...string) fiber.Handler {
	return func(c fiber.Ctx) error {
		if flipped.Load() {
			c.Set(fiber.HeaderCacheControl, newerCC)
			return c.SendString("newer")
		}

		c.Set(fiber.HeaderCacheControl, staleCC)
		for i := 0; i < len(staleExtra); i += 2 {
			c.Set(staleExtra[i], staleExtra[i+1])
		}
		return c.SendString("stale")
	}
}

func Test_CacheStorage_UncacheableRevalidationDeletionErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		failedKeySuffix   string
		wantDeletes       []string
		metadataRemaining bool
	}{
		{
			name:              "metadata",
			metadataRemaining: true,
			wantDeletes:       []string{cacheKeyVersion + "|GET|/cached"},
		},
		{
			name:            "body",
			failedKeySuffix: "_body",
			wantDeletes:     []string{cacheKeyVersion + "|GET|/cached", cacheKeyVersion + "|GET|/cached_body"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			storage := newContextRecorderStorage()
			var returnNoCache atomic.Bool

			app := cacheTestBPathKeyedApp(&Config{
				CacheInvalidator: cacheTestBInvalidateByQuery,
				MaxBytes:         5,
				Storage:          storage,
			})
			app.Get("/cached", cacheTestBFlipping(&returnNoCache, "public, max-age=60, must-revalidate", "no-cache"))
			app.Get("/next", func(c fiber.Ctx) error {
				c.Set(fiber.HeaderCacheControl, "public, max-age=60")
				return c.SendString("fresh")
			})

			cacheTestBExpect(t, app, "/cached", cacheMiss)

			key := cacheKeyVersion + "|GET|/cached"
			bodyKey := key + "_body"
			storage.mu.Lock()
			storage.errs["del|"+key+tt.failedKeySuffix] = errors.New("delete failed")
			storage.mu.Unlock()
			returnNoCache.Store(true)

			resp := apptest.Expect(t, app, "/cached?invalidate=true", fiber.StatusOK)
			require.Equal(t, cacheUnreachable, resp.Header.Get("X-Cache"))
			require.Equal(t, "newer", apptest.Body(t, resp))
			require.NoError(t, resp.Body.Close())

			storage.mu.RLock()
			_, metadataRemaining := storage.data[key]
			_, bodyRemaining := storage.data[bodyKey]
			storage.mu.RUnlock()
			require.Equal(t, tt.metadataRemaining, metadataRemaining)
			require.True(t, bodyRemaining)

			deletes := storage.recordedDeletes()
			deleteKeys := make([]string, 0, len(deletes))
			for _, deleteRecord := range deletes {
				deleteKeys = append(deleteKeys, deleteRecord.key)
			}
			require.Equal(t, tt.wantDeletes, deleteKeys)
			for _, get := range storage.recordedGets() {
				require.NotEqual(t, bodyKey, get.key, "revalidation must not fetch the stale body")
			}

			// The old heap entry remains tracked after a deletion error so a later
			// eviction can retry cleanup and restore byte accounting.
			storage.mu.Lock()
			delete(storage.errs, "del|"+key+tt.failedKeySuffix)
			storage.mu.Unlock()
			cacheTestBExpect(t, app, "/next", cacheMiss)

			storage.mu.RLock()
			require.NotContains(t, storage.data, key)
			require.NotContains(t, storage.data, bodyKey)
			storage.mu.RUnlock()
		})
	}
}

// cacheTestBRequireAccounting requires the MaxBytes bookkeeping the probe saw
// last: the bytes counted as stored and the nodes in the eviction heap.
func cacheTestBRequireAccounting(t *testing.T, probe *accountingProbe, counted uint, nodes int) {
	t.Helper()

	require.Equal(t, counted, probe.counted(), "bytes counted as stored")
	require.Equal(t, nodes, probe.nodes(), "nodes in the eviction heap")
}

func Test_CacheStorage_UncacheableRevalidationPrecheckFailuresReturnOrigin(t *testing.T) {
	t.Parallel()

	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("metadataMissing=%t", missing), func(t *testing.T) {
			t.Parallel()

			storage := newFailingCacheStorage()
			probe := &accountingProbe{}
			key := cacheKeyVersion + "|GET|/cached"
			var revalidate atomic.Bool
			app := cacheTestBPathKeyedApp(&Config{MaxBytes: 5, Storage: storage, accounting: probe.record})
			app.Get("/cached", func(c fiber.Ctx) error {
				if revalidate.Load() {
					storage.mu.Lock()
					if missing {
						delete(storage.data, key)
						delete(storage.data, key+"_body")
					} else {
						storage.errs["get|"+key] = errors.New("metadata lookup failed")
					}
					storage.mu.Unlock()
					c.Set(fiber.HeaderCacheControl, "no-cache")
					return c.SendString("newer")
				}
				c.Set(fiber.HeaderCacheControl, "public, max-age=60")
				return c.SendString("stale")
			})

			cacheTestBExpect(t, app, "/cached", cacheMiss)

			revalidate.Store(true)
			request := apptest.Req(fiber.MethodGet, "/cached", apptest.WithHeader(fiber.HeaderCacheControl, "max-age=0"))
			resp := apptest.Expect(t, app, request, fiber.StatusOK)
			require.Equal(t, cacheUnreachable, resp.Header.Get("X-Cache"))
			require.Equal(t, "newer", apptest.Body(t, resp))
			require.NoError(t, resp.Body.Close())

			if missing {
				cacheTestBRequireAccounting(t, probe, 0, 0)
			} else {
				cacheTestBRequireAccounting(t, probe, 5, 1)
			}
		})
	}
}

func Test_CacheStorage_EvictionRestoreDropsAbsentOrReplacedEntry(t *testing.T) {
	t.Parallel()

	for _, mutation := range []string{"missing", "replaced"} {
		t.Run(mutation, func(t *testing.T) {
			t.Parallel()

			storage := &restoreMutationStorage{
				failingCacheStorage: newFailingCacheStorage(),
				key:                 cacheKeyVersion + "|GET|/old",
				mutation:            mutation,
			}
			probe := &accountingProbe{}
			app := cacheTestBPathKeyedApp(&Config{MaxBytes: 5, Storage: storage, accounting: probe.record})
			app.Get("/:name", func(c fiber.Ctx) error {
				if c.Path() == "/old" {
					return c.SendString("stale")
				}
				return c.SendString("fresh")
			})

			cacheTestBExpect(t, app, "/old", cacheMiss)
			cacheTestBRequireAccounting(t, probe, 5, 1)

			storage.armed.Store(true)
			resp := apptest.Expect(t, app, "/new", fiber.StatusInternalServerError)
			require.NoError(t, resp.Body.Close())
			cacheTestBRequireAccounting(t, probe, 0, 0)
		})
	}
}

// cacheTestBParked is a request sent in the background that stops on a hook of
// the test, in the origin or in the storage, until the test lets it go on.
type cacheTestBParked struct {
	resp    *http.Response
	err     error
	release chan struct{}
	done    chan struct{}
	once    sync.Once
}

// cacheTestBPark sends req through app in the background and returns once
// parked is closed, which the request does as it stops on release. finish lets
// it go on, and so does the end of the test should an assertion fail before.
func cacheTestBPark(t *testing.T, app *fiber.App, req *http.Request, parked <-chan struct{}, release chan struct{}) *cacheTestBParked {
	t.Helper()

	p := &cacheTestBParked{release: release, done: make(chan struct{})}
	t.Cleanup(p.open)
	go func() {
		defer close(p.done)
		p.resp, p.err = app.Test(req)
	}()
	<-parked
	return p
}

// open lets the parked request go on.
func (p *cacheTestBParked) open() {
	p.once.Do(func() { close(p.release) })
}

// finish lets the parked request go on, waits for it and returns its response,
// the body already read and closed.
func (p *cacheTestBParked) finish(t *testing.T) *http.Response {
	t.Helper()

	p.open()
	<-p.done
	require.NoError(t, p.err)
	apptest.BodyBytes(t, p.resp)
	return p.resp
}

func Test_CacheStorage_EvictionRestoreDropsOldNodeAfterConcurrentReplacement(t *testing.T) {
	t.Parallel()

	storage := &blockingFailedEvictionStorage{
		failingCacheStorage: newFailingCacheStorage(),
		key:                 cacheKeyVersion + "|GET|/old",
		started:             make(chan struct{}),
		continueDelete:      make(chan struct{}),
	}
	probe := &accountingProbe{}
	app := cacheTestBPathKeyedApp(&Config{MaxBytes: 10, Storage: storage, accounting: probe.record})
	app.Get("/:name", func(c fiber.Ctx) error {
		c.Set(fiber.HeaderCacheControl, "public, max-age=60")
		if c.Path() == "/new" {
			return c.SendString("0123456789")
		}
		if fiber.Query[string](c, "replacement") == "true" {
			return c.SendString("")
		}
		return c.SendString("stale")
	})

	cacheTestBExpect(t, app, "/old", cacheMiss)

	storage.armed.Store(true)
	evicting := cacheTestBPark(t, app, apptest.Req(fiber.MethodGet, "/new"), storage.started, storage.continueDelete)

	replacementReq := apptest.Req(fiber.MethodGet, "/old?replacement=true", apptest.WithHeader(fiber.HeaderCacheControl, "max-age=0"))
	resp := apptest.Expect(t, app, replacementReq, fiber.StatusOK)
	require.NoError(t, resp.Body.Close())

	oldResp := evicting.finish(t)
	require.Equal(t, fiber.StatusInternalServerError, oldResp.StatusCode)
	cacheTestBRequireAccounting(t, probe, 0, 1)
	storage.mu.Lock()
	_, replacementStored := storage.data[storage.key]
	storage.mu.Unlock()
	require.True(t, replacementStored)
}

func Test_CacheStorage_MinFreshUncacheableRevalidationCleansEntry(t *testing.T) {
	t.Parallel()

	storage := newFailingCacheStorage()
	probe := &accountingProbe{}
	var uncacheable atomic.Bool
	app := cacheTestBPathKeyedApp(&Config{MaxBytes: 5, Storage: storage, accounting: probe.record})
	app.Get("/cached", cacheTestBFlipping(&uncacheable, "public, max-age=5", "no-store"))

	cacheTestBExpect(t, app, "/cached", cacheMiss)

	uncacheable.Store(true)
	req := apptest.Req(fiber.MethodGet, "/cached", apptest.WithHeader(fiber.HeaderCacheControl, "min-fresh=10"))
	cacheTestBExpectBody(t, app, req, cacheUnreachable, "newer")
	cacheTestBRequireAccounting(t, probe, 0, 0)
	storage.mu.Lock()
	_, metadataExists := storage.data[cacheKeyVersion+"|GET|/cached"]
	storage.mu.Unlock()
	require.False(t, metadataExists)
}

func Test_Cache_InMemoryUncacheableRevalidationRemovesStaleEntry(t *testing.T) {
	t.Parallel()

	probe := &accountingProbe{}
	var uncacheable atomic.Bool
	app := cacheTestBPathKeyedApp(&Config{
		CacheInvalidator: cacheTestBInvalidateByQuery,
		MaxBytes:         10,
		accounting:       probe.record,
	})
	app.Get("/cached", cacheTestBFlipping(&uncacheable, "public, max-age=60, must-revalidate", "no-cache"))

	cacheTestBExpect(t, app, "/cached", cacheMiss)
	cacheTestBRequireAccounting(t, probe, 5, 1)

	uncacheable.Store(true)
	resp := apptest.Expect(t, app, "/cached?invalidate=true", fiber.StatusOK)
	require.Equal(t, cacheUnreachable, resp.Header.Get("X-Cache"))
	require.Equal(t, "newer", apptest.Body(t, resp))
	require.NoError(t, resp.Body.Close())
	cacheTestBRequireAccounting(t, probe, 0, 0)

	cacheTestBExpectBody(t, app, "/cached", cacheUnreachable, "newer")
}

func Test_Cache_DefaultMemoryRevalidationDoesNotReplayUncacheableEntry(t *testing.T) {
	t.Parallel()

	var uncacheable atomic.Bool
	app := fiber.New()
	app.Use(New(Config{Expiration: time.Hour}))
	app.Get("/cached", cacheTestBFlipping(&uncacheable, "public, max-age=60", "no-cache"))

	cacheTestBExpect(t, app, "/cached", cacheMiss)

	uncacheable.Store(true)
	request := apptest.Req(fiber.MethodGet, "/cached", apptest.WithHeader(fiber.HeaderCacheControl, "max-age=0"))
	cacheTestBExpect(t, app, request, cacheUnreachable)

	resp := apptest.Do(t, app, "/cached")
	require.NotEqual(t, cacheHit, resp.Header.Get("X-Cache"))
	require.Equal(t, "newer", apptest.Body(t, resp))
	require.NoError(t, resp.Body.Close())
}

func Test_Cache_InvalidExpiresMemoryRevalidationCleansEntry(t *testing.T) {
	t.Parallel()

	var noCache atomic.Bool
	app := fiber.New()
	app.Use(New(Config{Expiration: 30 * time.Second, MaxBytes: 5}))
	app.Get("/cached", cacheTestBFlipping(&noCache, "public", "no-cache", fiber.HeaderExpires, "invalid-date"))

	cacheTestBExpect(t, app, "/cached", cacheMiss)

	noCache.Store(true)
	for range 2 {
		cacheTestBExpectBody(t, app, "/cached", cacheUnreachable, "newer")
	}
}

func Test_CacheStorage_ExpiredMetadataRemovesMatchingHeapEntry(t *testing.T) {
	t.Parallel()

	storage := newFailingCacheStorage()
	probe := &accountingProbe{}
	var originCalls atomic.Int32
	app := cacheTestBPathKeyedApp(&Config{MaxBytes: 5, Storage: storage, accounting: probe.record})
	app.Get("/cached", func(c fiber.Ctx) error {
		c.Set(fiber.HeaderCacheControl, "public, max-age=60")
		return c.SendString(fmt.Sprintf("%05d", originCalls.Add(1)))
	})

	cacheTestBExpect(t, app, "/cached", cacheMiss)

	key := cacheKeyVersion + "|GET|/cached"
	storage.mu.Lock()
	var entry item
	_, err := entry.UnmarshalMsg(storage.data[key])
	if err == nil {
		entry.ttl = 0
		entry.exp = 1
		storage.data[key], err = entry.MarshalMsg(nil)
	}
	storage.mu.Unlock()
	require.NoError(t, err)

	cacheTestBExpectBody(t, app, "/cached", cacheMiss, "00002")
	cacheTestBRequireAccounting(t, probe, 5, 1)
}

// cacheTestBVersionedOrigin answers /cached by its version query: the
// "uncacheable" version with a no-cache "old origin", the "replacement" version
// with a cacheable "newer!", and any other with a cacheable "stale". When
// started is not nil, the uncacheable version closes it and then waits for
// release before it answers, which holds that request in the origin.
func cacheTestBVersionedOrigin(started chan<- struct{}, release <-chan struct{}) fiber.Handler {
	return func(c fiber.Ctx) error {
		switch fiber.Query[string](c, "version") {
		case "uncacheable":
			if started != nil {
				close(started)
				<-release
			}
			c.Set(fiber.HeaderCacheControl, "no-cache")
			return c.SendString("old origin")
		case "replacement":
			c.Set(fiber.HeaderCacheControl, "public, max-age=60")
			return c.SendString("newer!")
		default:
			c.Set(fiber.HeaderCacheControl, "public, max-age=60")
			return c.SendString("stale")
		}
	}
}

// cacheTestBRace is the interleaving the concurrent uncacheable-revalidation
// tests share: a revalidation of /cached whose answer turns out uncacheable is
// parked, and while it is, another revalidation replaces the entry.
type cacheTestBRace struct {
	app    *fiber.App
	parked *cacheTestBParked
}

// cacheTestBStartRace caches "stale" at /cached, then parks on release the
// revalidation of it that the origin answers uncacheable, which is parked once
// parked is closed.
func cacheTestBStartRace(t *testing.T, app *fiber.App, parked <-chan struct{}, release chan struct{}) *cacheTestBRace {
	t.Helper()

	cacheTestBExpect(t, app, "/cached", cacheMiss)
	uncacheable := apptest.Req(fiber.MethodGet, "/cached?version=uncacheable", apptest.WithHeader(fiber.HeaderCacheControl, "max-age=0"))
	return &cacheTestBRace{app: app, parked: cacheTestBPark(t, app, uncacheable, parked, release)}
}

// replace completes, with the first revalidation parked, a second one that the
// origin answers cacheable: it replaces the entry, and is a miss.
func (r *cacheTestBRace) replace(t *testing.T) {
	t.Helper()

	replacement := apptest.Req(fiber.MethodGet, "/cached?version=replacement", apptest.WithHeader(fiber.HeaderCacheControl, "max-age=0"))
	resp := apptest.Expect(t, r.app, replacement, fiber.StatusOK)
	require.Equal(t, cacheMiss, resp.Header.Get("X-Cache"))
	apptest.BodyBytes(t, resp)
}

// finish lets the parked revalidation go on and requires the origin's
// uncacheable answer to reach its client.
func (r *cacheTestBRace) finish(t *testing.T) {
	t.Helper()

	resp := r.parked.finish(t)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	require.Equal(t, cacheUnreachable, resp.Header.Get("X-Cache"))
	require.Equal(t, "old origin", apptest.Body(t, resp))
}

func Test_Cache_ConcurrentUncacheableCleanupPreservesReplacementAccounting(t *testing.T) {
	t.Parallel()

	storage := &blockingDeleteStorage{
		failingCacheStorage: newFailingCacheStorage(),
		deleteStarted:       make(chan struct{}),
		continueDelete:      make(chan struct{}),
		key:                 cacheKeyVersion + "|GET|/cached",
	}
	probe := &accountingProbe{}
	app := cacheTestBPathKeyedApp(&Config{MaxBytes: 20, Storage: storage, accounting: probe.record})
	app.Get("/cached", cacheTestBVersionedOrigin(nil, nil))

	race := cacheTestBStartRace(t, app, storage.deleteStarted, storage.continueDelete)
	race.replace(t)
	cacheTestBRequireAccounting(t, probe, 6, 1)

	race.finish(t)
	require.Equal(t, uint(6), probe.counted(), "cleanup must not subtract a replacement's bytes")
	require.Equal(t, 1, probe.nodes(), "cleanup must not remove a replacement's heap node")
}

func Test_Cache_InMemoryUncacheableRevalidationPreservesConcurrentReplacement(t *testing.T) {
	t.Parallel()

	originStarted := make(chan struct{})
	continueOrigin := make(chan struct{})
	probe := &accountingProbe{}
	app := cacheTestBPathKeyedApp(&Config{MaxBytes: 20, accounting: probe.record})
	app.Get("/cached", cacheTestBVersionedOrigin(originStarted, continueOrigin))

	race := cacheTestBStartRace(t, app, originStarted, continueOrigin)
	race.replace(t)
	cacheTestBRequireAccounting(t, probe, 6, 1)

	race.finish(t)
	cacheTestBRequireAccounting(t, probe, 6, 1)

	cacheTestBExpectBody(t, app, "/cached", cacheHit, "newer!")
}

func Test_Cache_ExternalUncacheableRevalidationPreservesCompletedReplacement(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name     string
		maxBytes uint
	}{
		{name: "with MaxBytes", maxBytes: 20},
		{name: "without MaxBytes"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			originStarted := make(chan struct{})
			continueOrigin := make(chan struct{})
			storage := newFailingCacheStorage()
			probe := &accountingProbe{}
			app := cacheTestBPathKeyedApp(&Config{MaxBytes: tt.maxBytes, Storage: storage, accounting: probe.record})
			app.Get("/cached", cacheTestBVersionedOrigin(originStarted, continueOrigin))

			race := cacheTestBStartRace(t, app, originStarted, continueOrigin)
			race.replace(t)
			if tt.maxBytes > 0 {
				cacheTestBRequireAccounting(t, probe, 6, 1)
			}

			race.finish(t)
			if tt.maxBytes > 0 {
				cacheTestBRequireAccounting(t, probe, 6, 1)
			}

			key := cacheKeyVersion + "|GET|/cached"
			storage.mu.RLock()
			require.Contains(t, storage.data, key)
			require.Equal(t, []byte("newer!"), storage.data[key+"_body"])
			storage.mu.RUnlock()

			cacheTestBExpectBody(t, app, "/cached", cacheHit, "newer!")
		})
	}
}

func Test_CacheStorage_UncacheableRevalidationPreservesCompletedReplacement(t *testing.T) {
	t.Parallel()

	for _, maxBytes := range []uint{0, 20} {
		t.Run(fmt.Sprintf("MaxBytes=%d", maxBytes), func(t *testing.T) {
			t.Parallel()

			originStarted := make(chan struct{})
			continueOrigin := make(chan struct{})
			probe := &accountingProbe{}
			app := cacheTestBPathKeyedApp(&Config{MaxBytes: maxBytes, Storage: newFailingCacheStorage(), accounting: probe.record})
			app.Get("/cached", cacheTestBVersionedOrigin(originStarted, continueOrigin))

			race := cacheTestBStartRace(t, app, originStarted, continueOrigin)
			race.replace(t)
			race.finish(t)

			if maxBytes > 0 {
				cacheTestBRequireAccounting(t, probe, 6, 1)
			}
			cacheTestBExpectBody(t, app, "/cached", cacheHit, "newer!")
		})
	}
}

func Test_CacheStorage_NoStoreRevalidationCleansVaryManifest(t *testing.T) {
	t.Parallel()

	for _, failDelete := range []bool{false, true} {
		t.Run(fmt.Sprintf("deleteFailure=%t", failDelete), func(t *testing.T) {
			t.Parallel()

			storage := newFailingCacheStorage()
			var noStore atomic.Bool
			app := cacheTestBPathKeyedApp(&Config{Storage: storage})
			app.Get("/cached", cacheTestBFlipping(&noStore, "public, max-age=60", "no-store", fiber.HeaderVary, "Accept"))

			accepting := apptest.WithHeader(fiber.HeaderAccept, "application/json")
			cacheTestBExpect(t, app, apptest.Req(fiber.MethodGet, "/cached", accepting), cacheMiss)

			manifestKey := cacheKeyVersion + "|GET|/cached|vary"
			storage.mu.Lock()
			_, manifestStored := storage.data[manifestKey]
			if failDelete {
				storage.errs["del|"+manifestKey] = errors.New("manifest delete failed")
			}
			storage.mu.Unlock()
			require.True(t, manifestStored)

			noStore.Store(true)
			request := apptest.Req(fiber.MethodGet, "/cached", accepting, apptest.WithHeader(fiber.HeaderCacheControl, "max-age=0"))
			resp := apptest.Expect(t, app, request, fiber.StatusOK)
			require.Equal(t, cacheUnreachable, resp.Header.Get("X-Cache"))
			require.Equal(t, "newer", apptest.Body(t, resp))
			require.NoError(t, resp.Body.Close())

			storage.mu.RLock()
			_, manifestRemains := storage.data[manifestKey]
			storage.mu.RUnlock()
			require.Equal(t, failDelete, manifestRemains)
		})
	}
}

func Test_CacheControlNotOverwritten(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	app.Use(New(Config{Expiration: 10 * time.Second, StoreResponseHeaders: true}))
	app.Get("/", func(c fiber.Ctx) error {
		c.Set(fiber.HeaderCacheControl, "private")
		return c.SendString("ok")
	})

	apptest.Do(t, app, "/")

	resp := apptest.Do(t, app, "/")
	require.Equal(t, "private", resp.Header.Get(fiber.HeaderCacheControl))
}

func Test_CacheMaxAgeDirective(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	app.Use(New(Config{Expiration: 10 * time.Second}))
	app.Get("/", func(c fiber.Ctx) error {
		c.Set(fiber.HeaderCacheControl, "max-age=1")
		return c.SendString("1")
	})

	apptest.Do(t, app, "/")

	time.Sleep(1500 * time.Millisecond)

	cacheTestBExpect(t, app, "/", cacheMiss)
}

func TestCacheBypassesExistingEntryForAuthorization(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(New())

	var count int
	app.Get("/", func(c fiber.Ctx) error {
		count++
		return c.SendString(strconv.Itoa(count))
	})

	cacheTestBExpectBody(t, app, "/", cacheMiss, "1")

	authReq := apptest.Req(fiber.MethodGet, "/", apptest.WithHeader(fiber.HeaderAuthorization, "Bearer token"))
	cacheTestBExpectBody(t, app, authReq, cacheUnreachable, "2")

	cacheTestBExpectBody(t, app, "/", cacheHit, "1")
}

func TestCacheOnlyIfCachedRejectsNonShareableAuthorizationEntry(t *testing.T) {
	t.Parallel()

	const cacheHeader = "X-Test-Cache"

	storage := newMutatingStorage(nil)
	app := fiber.New()
	app.Use(New(Config{
		CacheHeader: cacheHeader,
		Expiration:  time.Hour,
		Storage:     storage,
	}))

	var originCalls atomic.Int32
	app.Get("/", func(c fiber.Ctx) error {
		call := originCalls.Add(1)
		if call == 1 {
			c.Set(fiber.HeaderCacheControl, "public, max-age=3600")
		}
		return c.SendString(fmt.Sprintf("origin-%d", call))
	})

	newAuthorizationRequest := func(cacheControl, pragma string) *http.Request {
		req := httptest.NewRequest(fiber.MethodGet, "/", http.NoBody)
		req.Header.Set(fiber.HeaderAuthorization, "Bearer token")
		if cacheControl != "" {
			req.Header.Set(fiber.HeaderCacheControl, cacheControl)
		}
		if pragma != "" {
			req.Header.Set(fiber.HeaderPragma, pragma)
		}
		return req
	}

	resp := apptest.Expect(t, app, newAuthorizationRequest("", ""), fiber.StatusOK)
	require.Equal(t, cacheMiss, resp.Header.Get(cacheHeader))
	require.Equal(t, int32(1), originCalls.Load())

	metadataKey := ""
	for key, value := range storage.data {
		if strings.HasSuffix(key, "_body") {
			continue
		}

		var cached item
		_, err := cached.UnmarshalMsg(value)
		require.NoError(t, err)
		cached.shareable = false
		storage.data[key], err = cached.MarshalMsg(nil)
		require.NoError(t, err)
		metadataKey = key
	}
	require.NotEmpty(t, metadataKey)

	// The entry is not shareable, so a request that only takes a stored answer
	// is refused, and the entry stays.
	expectRefused := func(cacheControl, pragma string) {
		t.Helper()

		refused := apptest.Expect(t, app, newAuthorizationRequest(cacheControl, pragma), fiber.StatusGatewayTimeout)
		require.Equal(t, cacheUnreachable, refused.Header.Get(cacheHeader))
		require.Equal(t, int32(1), originCalls.Load())
		require.Contains(t, storage.data, metadataKey)
		require.Contains(t, storage.data, metadataKey+"_body")
	}
	expectRefused("only-if-cached", "")
	expectRefused("no-cache, only-if-cached", "")
	expectRefused("only-if-cached", "no-cache")

	resp = apptest.Expect(t, app, newAuthorizationRequest("no-cache", ""), fiber.StatusOK)
	require.Equal(t, cacheUnreachable, resp.Header.Get(cacheHeader))
	require.Equal(t, "origin-2", apptest.Body(t, resp))
	require.Equal(t, int32(2), originCalls.Load())
	require.Contains(t, storage.data, metadataKey)
	require.Contains(t, storage.data, metadataKey+"_body")
}

// TestCacheAuthorizationWithSharedCacheDirectives asserts that the answer to a
// request carrying Authorization is stored only when the response carries a
// directive that lets a shared cache keep it.
func TestCacheAuthorizationWithSharedCacheDirectives(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		cacheControl  string
		expires       string
		expectedBody  string
		expectedBody2 string
		expectFirst   string
		expectSecond  string
	}{
		{
			name:          "public",
			cacheControl:  "public, max-age=60",
			expectedBody:  "ok-1",
			expectedBody2: "ok-1",
			expectFirst:   cacheMiss,
			expectSecond:  cacheHit,
		},
		{
			name:          "must-revalidate",
			cacheControl:  "must-revalidate, max-age=60",
			expectedBody:  "ok-1",
			expectedBody2: "ok-1",
			expectFirst:   cacheMiss,
			expectSecond:  cacheHit,
		},
		{
			name:          "proxy-revalidate",
			cacheControl:  "proxy-revalidate, max-age=60",
			expectedBody:  "ok-1",
			expectedBody2: "ok-1",
			expectFirst:   cacheMiss,
			expectSecond:  cacheHit,
		},
		{
			name:          "expires header",
			cacheControl:  "",
			expires:       time.Now().Add(1 * time.Minute).UTC().Format(http.TimeFormat),
			expectedBody:  "ok-1",
			expectedBody2: "ok-2",
			expectFirst:   cacheUnreachable,
			expectSecond:  cacheUnreachable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			app := fiber.New()
			app.Use(New(Config{Expiration: 10 * time.Second}))

			var count int
			app.Get("/", func(c fiber.Ctx) error {
				count++
				c.Set(fiber.HeaderCacheControl, tt.cacheControl)
				if tt.expires != "" {
					c.Set(fiber.HeaderExpires, tt.expires)
				}
				return c.SendString(fmt.Sprintf("ok-%d", count))
			})

			req := apptest.Req(fiber.MethodGet, "/", apptest.WithHeader(fiber.HeaderAuthorization, "Bearer token"))
			cacheTestBExpectBody(t, app, req, tt.expectFirst, tt.expectedBody)
			cacheTestBExpectBody(t, app, req, tt.expectSecond, tt.expectedBody2)

			if tt.expectSecond == cacheHit {
				require.Equal(t, 1, count)
			} else {
				require.Equal(t, 2, count)
			}
		})
	}
}

func TestCacheSeparatesAuthorizationValues(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(New(Config{Expiration: 10 * time.Second}))

	var count int
	app.Get("/", func(c fiber.Ctx) error {
		count++
		c.Set(fiber.HeaderCacheControl, "public, max-age=60")
		return c.SendString(fmt.Sprintf("body-%d-%s", count, c.Get(fiber.HeaderAuthorization)))
	})

	newRequest := func(token string) *http.Request {
		return apptest.Req(fiber.MethodGet, "/", apptest.WithHeader(fiber.HeaderAuthorization, "Bearer "+token))
	}

	authTokenA := "token-a"
	authTokenB := "token-b"

	cacheTestBExpectBody(t, app, newRequest(authTokenA), cacheMiss, "body-1-Bearer "+authTokenA)

	cacheTestBExpectBody(t, app, newRequest(authTokenA), cacheHit, "body-1-Bearer "+authTokenA)
	require.Equal(t, 1, count)

	cacheTestBExpectBody(t, app, newRequest(authTokenB), cacheMiss, "body-2-Bearer "+authTokenB)
	require.Equal(t, 2, count)

	cacheTestBExpectBody(t, app, newRequest(authTokenB), cacheHit, "body-2-Bearer "+authTokenB)

	cacheTestBExpectBody(t, app, newRequest(authTokenA), cacheHit, "body-1-Bearer "+authTokenA)
	require.Equal(t, 2, count)
}

// Benchmark_Cache measures the hit path: the first request stores the response
// and every one after it is served from the entry.
//
// The status has to be one this middleware treats as cacheable, which is the
// set RFC 9110 Section 15.1 marks heuristically cacheable — RFC 9111 defines
// caching but has no Section 15, and defers the status codes to that one. This
// was 418, which is not in the set, so the middleware answered "unreachable"
// every time and the benchmark measured the path that stores nothing while the
// handler re-read the file on every iteration.
//
// go test -v -run=^$ -bench=Benchmark_Cache -benchmem -count=4
func Benchmark_Cache(b *testing.B) {
	app := fiber.New()

	app.Use(New())

	app.Get("/demo", func(c fiber.Ctx) error {
		data, _ := os.ReadFile("../../README.md") //nolint:errcheck // We're inside a benchmark
		return c.Status(fiber.StatusOK).Send(data)
	})

	h := app.Handler()

	fctx := &fasthttp.RequestCtx{}
	fctx.Request.Header.SetMethod(fiber.MethodGet)
	fctx.Request.SetRequestURI("/demo")

	// One untimed request to populate the entry, so every measured iteration is
	// a hit rather than the first being the miss that fills the cache. b.Loop
	// resets the timer when it first returns, so this stays out of the numbers.
	// Without it the benchmark also fails outright at -benchtime=1x, where the
	// single iteration is that miss.
	h(fctx)

	b.ReportAllocs()

	for b.Loop() {
		h(fctx)
	}

	require.Equal(b, fiber.StatusOK, fctx.Response.Header.StatusCode())
	require.Equal(b, cacheHit, string(fctx.Response.Header.Peek("X-Cache")))
	require.Greater(b, len(fctx.Response.Body()), 30000)
}

// Benchmark_Cache_HitConfigs measures a hit under the settings that add work to it: a Vary, an invalidator,
// credentials as long as a real token, and a refusal recorded in the URL's group.
func Benchmark_Cache_HitConfigs(b *testing.B) {
	cases := []struct {
		name    string
		vary    string
		cfg     Config
		refused bool // another session's refusal is recorded before the hits are measured
	}{
		{name: "default"},
		{name: "vary", vary: "Accept-Encoding, Accept-Language"},
		{name: "vary Cookie", vary: "Cookie"},
		{name: "invalidator", cfg: Config{CacheInvalidator: func(fiber.Ctx) bool { return false }}},
		{name: "KeyCookies", cfg: Config{KeyCookies: []string{"session"}}},
		{name: "KeyCookies, refusal recorded", cfg: Config{KeyCookies: []string{"session"}}, refused: true},
		{name: "KeyHeaders X-API-Key", cfg: Config{KeyHeaders: []string{"X-API-Key"}}},
	}

	for i := range cases {
		bc := &cases[i]
		b.Run(bc.name, func(b *testing.B) {
			var ended atomic.Bool
			app := fiber.New()
			app.Use(New(bc.cfg))
			app.Get("/demo", func(c fiber.Ctx) error {
				if ended.Load() && c.Cookies("session") == "ended" {
					return c.SendStatus(fiber.StatusUnauthorized)
				}
				if bc.vary != "" {
					c.Set(fiber.HeaderVary, bc.vary)
				}
				return c.SendString("body")
			})

			h := app.Handler()
			fctx := &fasthttp.RequestCtx{}
			fctx.Request.Header.SetMethod(fiber.MethodGet)
			fctx.Request.SetRequestURI("/demo")
			fctx.Request.Header.Set("X-API-Key", "tok_0123456789abcdefghijklmnopqrstuvwxyz")
			fctx.Request.Header.SetCookie("session", "abc")

			// One untimed request to populate the entry, so every measured iteration is a hit.
			h(fctx)

			if bc.refused {
				// Another session's entry, then its refusal on a revalidation, leave a record in the group.
				other := &fasthttp.RequestCtx{}
				other.Request.Header.SetMethod(fiber.MethodGet)
				other.Request.SetRequestURI("/demo")
				other.Request.Header.SetCookie("session", "ended")
				h(other)
				ended.Store(true)
				other.Request.Header.Set(fiber.HeaderCacheControl, "no-cache")
				h(other)
				require.Equal(b, fiber.StatusUnauthorized, other.Response.StatusCode())
			}

			b.ReportAllocs()

			for b.Loop() {
				h(fctx)
			}

			require.Equal(b, cacheHit, string(fctx.Response.Header.Peek("X-Cache")))
		})
	}
}

func Benchmark_Cache_Miss(b *testing.B) {
	app := fiber.New()

	app.Use(New())

	app.Get("/*", func(c fiber.Ctx) error {
		data, _ := os.ReadFile("../../README.md") //nolint:errcheck // We're inside a benchmark
		return c.Status(fiber.StatusOK).Send(data)
	})

	h := app.Handler()

	fctx := &fasthttp.RequestCtx{}
	fctx.Request.Header.SetMethod(fiber.MethodGet)

	b.ReportAllocs()
	b.ResetTimer()

	var n int
	for b.Loop() {
		n++
		fctx.Request.SetRequestURI("/demo/" + strconv.Itoa(n))
		h(fctx)
	}

	require.Equal(b, fiber.StatusOK, fctx.Response.Header.StatusCode())
	require.Greater(b, len(fctx.Response.Body()), 30000)
}

// go test -v -run=^$ -bench=Benchmark_Cache_Storage -benchmem -count=4
func Benchmark_Cache_Storage(b *testing.B) {
	app := fiber.New()

	app.Use(New(Config{
		Storage: memory.New(),
	}))

	app.Get("/demo", func(c fiber.Ctx) error {
		data, _ := os.ReadFile("../../README.md") //nolint:errcheck // We're inside a benchmark
		return c.Status(fiber.StatusOK).Send(data)
	})

	h := app.Handler()

	fctx := &fasthttp.RequestCtx{}
	fctx.Request.Header.SetMethod(fiber.MethodGet)
	fctx.Request.SetRequestURI("/demo")

	// One untimed request to populate the entry, so every measured iteration is
	// a hit rather than the first being the miss that fills the cache. b.Loop
	// resets the timer when it first returns, so this stays out of the numbers.
	// Without it the benchmark also fails outright at -benchtime=1x, where the
	// single iteration is that miss.
	h(fctx)

	b.ReportAllocs()

	for b.Loop() {
		h(fctx)
	}

	require.Equal(b, fiber.StatusOK, fctx.Response.Header.StatusCode())
	require.Equal(b, cacheHit, string(fctx.Response.Header.Peek("X-Cache")))
	require.Greater(b, len(fctx.Response.Body()), 30000)
}

func Benchmark_Cache_AdditionalHeaders(b *testing.B) {
	app := fiber.New()
	app.Use(New(Config{
		StoreResponseHeaders: true,
	}))

	app.Get("/demo", func(c fiber.Ctx) error {
		c.Response().Header.Add("X-Foobar", "foobar")
		return c.SendStatus(fiber.StatusOK)
	})

	h := app.Handler()

	fctx := &fasthttp.RequestCtx{}
	fctx.Request.Header.SetMethod(fiber.MethodGet)
	fctx.Request.SetRequestURI("/demo")

	// One untimed request to populate the entry, so every measured iteration is
	// a hit rather than the first being the miss that fills the cache. b.Loop
	// resets the timer when it first returns, so this stays out of the numbers.
	// Without it the benchmark also fails outright at -benchtime=1x, where the
	// single iteration is that miss.
	h(fctx)

	b.ReportAllocs()

	for b.Loop() {
		h(fctx)
	}

	require.Equal(b, fiber.StatusOK, fctx.Response.Header.StatusCode())
	require.Equal(b, cacheHit, string(fctx.Response.Header.Peek("X-Cache")))
	// Exactly one line, not one per iteration. The handler adds it on the miss
	// and the hit path replaces what it stored; while the response was never
	// cached the handler ran every time and its Add piled up on the reused
	// context, so the benchmark grew its own work as it went.
	require.Equal(b, [][]byte{[]byte("foobar")}, fctx.Response.Header.PeekAll("X-Foobar"))
}

func Benchmark_Cache_QueryMethod(b *testing.B) {
	app := fiber.New()
	app.Use(New(Config{
		Methods: []string{fiber.MethodQuery},
	}))

	app.Query("/demo", func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	h := app.Handler()

	fctx := &fasthttp.RequestCtx{}
	fctx.Request.Header.SetMethod(fiber.MethodQuery)
	fctx.Request.SetRequestURI("/demo")
	fctx.Request.SetBody([]byte(`{"filter":"active","page":1}`))

	// One untimed request to populate the entry, so every measured iteration is
	// a hit rather than the first being the miss that fills the cache. b.Loop
	// resets the timer when it first returns, so this stays out of the numbers.
	// Without it the benchmark also fails outright at -benchtime=1x, where the
	// single iteration is that miss.
	h(fctx)

	b.ReportAllocs()

	for b.Loop() {
		h(fctx)
	}

	require.Equal(b, fiber.StatusOK, fctx.Response.Header.StatusCode())
	require.Equal(b, cacheHit, string(fctx.Response.Header.Peek("X-Cache")))
}

func Benchmark_Cache_MaxSize(b *testing.B) {
	// The benchmark is run with three different MaxSize parameters
	// 1) 0:        Tracking is disabled = no overhead
	// 2) MaxInt32: Enough to store all entries = no removals
	// 3) 100:      Small size = constant insertions and removals
	cases := []uint{0, math.MaxUint32, 100}
	names := []string{"Disabled", "Unlim", "LowBounded"}
	for i, size := range cases {
		b.Run(names[i], func(b *testing.B) {
			app := fiber.New()
			app.Use(New(Config{MaxBytes: size}))

			app.Get("/*", func(c fiber.Ctx) error {
				return c.Status(fiber.StatusOK).SendString("1")
			})

			h := app.Handler()
			fctx := &fasthttp.RequestCtx{}
			fctx.Request.Header.SetMethod(fiber.MethodGet)

			b.ReportAllocs()

			// strconv, not fmt.Sprintf: the formatting is the benchmark's own
			// setup and its allocation was landing in the middleware's numbers.
			n := 0
			uri := make([]byte, 0, 24)
			for b.Loop() {
				n++
				uri = strconv.AppendInt(append(uri[:0], '/'), int64(n), 10)
				fctx.Request.SetRequestURIBytes(uri)
				h(fctx)
			}

			// Stored, so the bounded case actually reaches the eviction loop this
			// benchmark exists to measure.
			require.Equal(b, fiber.StatusOK, fctx.Response.Header.StatusCode())
			require.Equal(b, cacheMiss, string(fctx.Response.Header.Peek("X-Cache")))
		})
	}
}

func Test_Cache_RevalidationWithMaxBytes(t *testing.T) {
	t.Parallel()

	// A request the cached entry cannot answer revalidates it, and the next
	// request is served the new entry.
	for _, tc := range []struct {
		name string
		// Cache-Control of the origin's response.
		cacheControl string
		// Cache-Control of the revalidating request.
		revalidate string
		// How long the entry ages before it is revalidated.
		wait time.Duration
	}{
		{name: "max-age=0 revalidation removes old entry on storage success", cacheControl: "max-age=60", revalidate: "max-age=0"},
		{name: "min-fresh revalidation with MaxBytes", cacheControl: "max-age=2", revalidate: "min-fresh=5", wait: time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app := fiber.New()

			app.Use(New(Config{
				MaxBytes: 100,
			}))

			requestCount := 0
			app.Get("/test", func(c fiber.Ctx) error {
				requestCount++
				c.Set(fiber.HeaderCacheControl, tc.cacheControl)
				return c.SendString(fmt.Sprintf("response-%d", requestCount))
			})

			// First request - cache the response
			cacheTestBExpect(t, app, "/test", cacheMiss)

			// Wait a bit so the entry has aged
			if tc.wait > 0 {
				time.Sleep(tc.wait)
			}

			// Request that revalidates the entry
			req2 := apptest.Req(fiber.MethodGet, "/test", apptest.WithHeader(fiber.HeaderCacheControl, tc.revalidate))
			resp2 := apptest.Do(t, app, req2)
			require.Equal(t, "response-2", apptest.Body(t, resp2))

			// Next request should serve the NEW cached entry
			resp3 := cacheTestBExpect(t, app, "/test", cacheHit)
			require.Equal(t, "response-2", apptest.Body(t, resp3), "New entry should be cached")
		})
	}

	t.Run("revalidation respects MaxBytes eviction", func(t *testing.T) {
		t.Parallel()

		app := fiber.New()

		app.Use(New(Config{
			MaxBytes:            20, // Only room for 2 responses of 10 bytes each
			ExpirationGenerator: stableAscendingExpiration(),
		}))

		app.Get("/*", func(c fiber.Ctx) error {
			c.Set(fiber.HeaderCacheControl, "max-age=60")
			return c.SendString("1234567890") // 10 bytes
		})

		// Cache /a and /b
		cacheTestBExpect(t, app, "/a", cacheMiss)
		cacheTestBExpect(t, app, "/b", cacheMiss)

		// Both should be cached
		cacheTestBExpect(t, app, "/a", cacheHit)
		cacheTestBExpect(t, app, "/b", cacheHit)

		// Revalidate /a with max-age=0
		apptest.Do(t, app, apptest.Req(fiber.MethodGet, "/a", apptest.WithHeader(fiber.HeaderCacheControl, "max-age=0")))

		// /a should be revalidated and cached again
		cacheTestBExpect(t, app, "/a", cacheHit)

		// /b should still be cached (heap accounting should be correct)
		cacheTestBExpect(t, app, "/b", cacheHit)
	})

	t.Run("no-store revalidation removes old in-memory entry", func(t *testing.T) {
		t.Parallel()

		app := fiber.New()

		app.Use(New(Config{
			MaxBytes: 100,
		}))

		requestCount := 0
		app.Get("/test", func(c fiber.Ctx) error {
			requestCount++
			if requestCount == 1 {
				c.Set(fiber.HeaderCacheControl, "max-age=60")
				return c.SendString("cacheable")
			}
			// Second request returns no-store
			c.Set(fiber.HeaderCacheControl, "no-store")
			return c.SendString("not-cacheable")
		})

		// First request - cache the response
		cacheTestBExpectBody(t, app, "/test", cacheMiss, "cacheable")

		// Request with max-age=0 to force revalidation
		// The new response will be no-store (not cacheable)
		req2 := apptest.Req(fiber.MethodGet, "/test", apptest.WithHeader(fiber.HeaderCacheControl, "max-age=0"))
		cacheTestBExpectBody(t, app, req2, cacheUnreachable, "not-cacheable")

		// A later request must reach the origin instead of serving the stale body.
		cacheTestBExpectBody(t, app, "/test", cacheUnreachable, "not-cacheable")
		require.Equal(t, 3, requestCount)
	})
}

// Test_parseCacheControlDirectives_QuotedStrings tests RFC 9111 Section 5.2 compliance
// for quoted-string values in Cache-Control directives
func Test_parseCacheControlDirectives_QuotedStrings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		expected map[string]string
		input    string
	}{
		{
			name:  "simple quoted value",
			input: `community="UCI"`,
			expected: map[string]string{
				"community": "UCI",
			},
		},
		{
			name:  "multiple directives with quoted values",
			input: `max-age=3600, community="UCI", custom="value"`,
			expected: map[string]string{
				"max-age":   "3600",
				"community": "UCI",
				"custom":    "value",
			},
		},
		{
			name:  "quoted value with spaces",
			input: `custom="value with spaces"`,
			expected: map[string]string{
				"custom": "value with spaces",
			},
		},
		{
			name:  "quoted value with escaped quote",
			input: `custom="value with \"quotes\""`,
			expected: map[string]string{
				"custom": `value with "quotes"`,
			},
		},
		{
			name:  "quoted value with escaped backslash",
			input: `custom="value with \\ backslash"`,
			expected: map[string]string{
				"custom": `value with \ backslash`,
			},
		},
		{
			name:  "mixed quoted and unquoted values",
			input: `max-age=3600, community="UCI", no-cache, custom="test"`,
			expected: map[string]string{
				"max-age":   "3600",
				"community": "UCI",
				"no-cache":  "",
				"custom":    "test",
			},
		},
		{
			name:  "quoted empty value",
			input: `custom=""`,
			expected: map[string]string{
				"custom": "",
			},
		},
		{
			name:  "spaces around quoted value",
			input: `custom = "value" , another="test"`,
			expected: map[string]string{
				"custom":  "value",
				"another": "test",
			},
		},
		{
			name:  "tabs around directives and values",
			input: "\tmax-age\t=\t3600\t,\tcommunity\t=\t\"UCI\"\t",
			expected: map[string]string{
				"max-age":   "3600",
				"community": "UCI",
			},
		},
		{
			name:  "whitespace other than SP and HTAB is kept",
			input: "max-age=\v3600",
			expected: map[string]string{
				"max-age": "\v3600",
			},
		},
		{
			name:  "unquoted token value",
			input: `max-age=3600`,
			expected: map[string]string{
				"max-age": "3600",
			},
		},
		{
			name:  "complex mixed case",
			input: `max-age=3600, s-maxage=7200, community="UCI", no-store, custom="value with \"escaped\" quotes"`,
			expected: map[string]string{
				"max-age":   "3600",
				"s-maxage":  "7200",
				"community": "UCI",
				"no-store":  "",
				"custom":    `value with "escaped" quotes`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := make(map[string]string)
			parseCacheControlDirectives([]byte(tt.input), func(key, value []byte) {
				result[string(key)] = string(value)
			})
			require.Equal(t, tt.expected, result)
		})
	}
}

// Test_Cache_MaxBytes_InsufficientSpace tests the "insufficient space" error path
// when an entry is larger than MaxBytes, ensuring such entries are treated as unreachable
func Test_Cache_MaxBytes_InsufficientSpace(t *testing.T) {
	t.Parallel()

	t.Run("entry larger than MaxBytes with empty cache", func(t *testing.T) {
		t.Parallel()
		app := fiber.New()

		app.Use(New(Config{
			MaxBytes:   10, // Very small cache
			Expiration: 1 * time.Hour,
		}))

		app.Get("/large", func(c fiber.Ctx) error {
			// Return data larger than MaxBytes
			return c.Send(make([]byte, 20))
		})

		// Should be unreachable because entry is too large
		cacheTestBExpect(t, app, "/large", cacheUnreachable)
	})

	t.Run("entry larger than MaxBytes after eviction", func(t *testing.T) {
		t.Parallel()
		app := fiber.New()

		app.Use(New(Config{
			MaxBytes:            15,
			ExpirationGenerator: stableAscendingExpiration(),
		}))

		app.Get("/*", func(c fiber.Ctx) error {
			path := c.Path()
			if path == "/small" {
				return c.Send(make([]byte, 5))
			}
			return c.Send(make([]byte, 20))
		})

		// Cache a small entry first
		cacheTestBExpect(t, app, "/small", cacheMiss)

		// Try to cache a large entry - should return unreachable since it won't fit even after eviction
		cacheTestBExpect(t, app, "/large", cacheUnreachable)
	})
}

func Test_Cache_MaxBytes_DeletionFailureRestoresTracking(t *testing.T) {
	t.Parallel()

	storage := newFailingCacheStorage()

	app := fiber.New()
	app.Use(New(Config{
		MaxBytes:   4,
		Expiration: 1 * time.Hour,
		Storage:    storage,
	}))

	app.Get("/:name", func(c fiber.Ctx) error {
		return c.SendString("data")
	})

	// Seed the cache with a single entry
	cacheTestBExpect(t, app, "/first", cacheMiss)

	var storedKeys []string
	storage.mu.Lock()
	for key := range storage.data {
		storedKeys = append(storedKeys, key)
		if strings.Contains(key, "/first") {
			storage.errs["del|"+key] = errors.New("delete failed")
		}
	}
	storage.mu.Unlock()
	t.Logf("stored keys after first cache: %v", storedKeys)

	// Next request triggers eviction; deletion failure should surface an error
	rsp := apptest.Do(t, app, "/second")
	body := apptest.Body(t, rsp)
	require.Equal(t, fiber.StatusInternalServerError, rsp.StatusCode)
	require.Contains(t, body, "failed to delete key")
	require.NoError(t, rsp.Body.Close())
	var remainingKeys []string
	storage.mu.RLock()
	for key := range storage.data {
		remainingKeys = append(remainingKeys, key)
	}
	storage.mu.RUnlock()
	t.Logf("stored keys after deletion failure: %v", remainingKeys)
	storage.mu.Lock()
	storage.errs = make(map[string]error)
	storage.mu.Unlock()

	// Another request should succeed and be cacheable after restoring heap tracking
	cacheTestBExpect(t, app, "/third", cacheMiss)
}

// Test_Cache_MaxBytes_ConcurrencyAndRaceConditions tests that the race condition fix works correctly
// under concurrent load, verifying that storedBytes never exceeds MaxBytes even with multiple
// goroutines making simultaneous requests
func Test_Cache_MaxBytes_ConcurrencyAndRaceConditions(t *testing.T) {
	t.Parallel()

	t.Run("concurrent requests with MaxBytes limit", func(t *testing.T) {
		t.Parallel()
		app := fiber.New()

		const maxBytes = uint(1000)
		const numGoroutines = 20
		const requestsPerGoroutine = 5

		app.Use(New(Config{
			MaxBytes:   maxBytes,
			Expiration: 10 * time.Second,
		}))

		app.Get("/*", func(c fiber.Ctx) error {
			// Return data that will fill up the cache
			return c.Send(make([]byte, 50))
		})

		// Launch multiple goroutines making concurrent requests
		var wg sync.WaitGroup
		errChan := make(chan error, numGoroutines*requestsPerGoroutine)

		for id := range numGoroutines {
			wg.Go(func() {
				for j := range requestsPerGoroutine {
					path := fmt.Sprintf("/test-%d-%d", id, j)
					req := httptest.NewRequest(fiber.MethodGet, path, http.NoBody)
					_, err := app.Test(req)
					if err != nil {
						errChan <- err
					}
				}
			})
		}

		wg.Wait()
		close(errChan)

		// Check for errors
		for err := range errChan {
			require.NoError(t, err, "concurrent request failed")
		}

		// The test passes if no errors occurred and no race conditions were detected by -race flag
	})

	t.Run("concurrent requests near capacity triggers eviction", func(t *testing.T) {
		t.Parallel()
		app := fiber.New()

		const maxBytes = uint(200)
		const numRequests = 10

		app.Use(New(Config{
			MaxBytes:   maxBytes,
			Expiration: 10 * time.Second,
		}))

		app.Get("/*", func(c fiber.Ctx) error {
			// Each response is about 50 bytes, so we'll exceed capacity
			return c.Send(make([]byte, 50))
		})

		// Make concurrent requests that will trigger evictions
		var wg sync.WaitGroup
		for id := range numRequests {
			wg.Go(func() {
				path := fmt.Sprintf("/item-%d", id)
				req := httptest.NewRequest(fiber.MethodGet, path, http.NoBody)
				_, err := app.Test(req)
				if err != nil {
					t.Logf("request error: %v", err)
				}
			})
		}

		wg.Wait()

		// Test passes if no race conditions or panics occurred
		// The -race flag will detect any remaining race conditions
	})
}

// Test_Cache_HelperFunctions tests various helper functions for better coverage
func Test_Cache_HelperFunctions(t *testing.T) {
	t.Parallel()

	t.Run("parseHTTPDate empty", func(t *testing.T) {
		t.Parallel()
		result, ok := parseHTTPDate([]byte{})
		require.False(t, ok)
		require.Equal(t, uint64(0), result)
	})

	t.Run("parseHTTPDate invalid", func(t *testing.T) {
		t.Parallel()
		result, ok := parseHTTPDate([]byte("invalid"))
		require.False(t, ok)
		require.Equal(t, uint64(0), result)
	})

	t.Run("parseHTTPDate valid", func(t *testing.T) {
		t.Parallel()
		result, ok := parseHTTPDate([]byte("Mon, 02 Jan 2006 15:04:05 GMT"))
		require.True(t, ok)
		require.Positive(t, result)
	})

	t.Run("safeUnixSeconds negative", func(t *testing.T) {
		t.Parallel()
		result := safeUnixSeconds(time.Unix(-1, 0))
		require.Equal(t, uint64(0), result)
	})

	t.Run("safeUnixSeconds positive", func(t *testing.T) {
		t.Parallel()
		result := safeUnixSeconds(time.Unix(1234567890, 0))
		require.Equal(t, uint64(1234567890), result)
	})

	t.Run("remainingFreshness nil", func(t *testing.T) {
		t.Parallel()
		result := remainingFreshness(nil, 100)
		require.Equal(t, uint64(0), result)
	})

	t.Run("remainingFreshness zero exp", func(t *testing.T) {
		t.Parallel()
		e := &item{exp: 0}
		result := remainingFreshness(e, 100)
		require.Equal(t, uint64(0), result)
	})

	t.Run("remainingFreshness expired", func(t *testing.T) {
		t.Parallel()
		e := &item{exp: 100}
		result := remainingFreshness(e, 200)
		require.Equal(t, uint64(0), result)
	})

	t.Run("remainingFreshness valid", func(t *testing.T) {
		t.Parallel()
		e := &item{exp: 200}
		result := remainingFreshness(e, 100)
		require.Equal(t, uint64(100), result)
	})

	t.Run("lookupCachedHeader not found", func(t *testing.T) {
		t.Parallel()
		headers := []cachedHeader{{key: []byte("Content-Type"), value: []byte("text/html")}}
		value, found := lookupCachedHeader(headers, "Authorization")
		require.False(t, found)
		require.Nil(t, value)
	})

	t.Run("lookupCachedHeader case insensitive", func(t *testing.T) {
		t.Parallel()
		headers := []cachedHeader{{key: []byte("Authorization"), value: []byte("Bearer token")}}
		value, found := lookupCachedHeader(headers, "authorization")
		require.True(t, found)
		require.Equal(t, []byte("Bearer token"), value)
	})

	t.Run("secondsToDuration zero", func(t *testing.T) {
		t.Parallel()
		result := secondsToDuration(0)
		require.Equal(t, time.Duration(0), result)
	})

	t.Run("secondsToDuration large", func(t *testing.T) {
		t.Parallel()
		result := secondsToDuration(9223372036)
		require.Greater(t, result, time.Duration(0))
	})

	t.Run("secondsToTime zero", func(t *testing.T) {
		t.Parallel()
		result := secondsToTime(0)
		require.Equal(t, time.Unix(0, 0).UTC(), result)
	})

	t.Run("secondsToTime value", func(t *testing.T) {
		t.Parallel()
		result := secondsToTime(1234567890)
		require.Equal(t, time.Unix(1234567890, 0).UTC(), result)
	})

	t.Run("isHeuristicFreshness short age", func(t *testing.T) {
		t.Parallel()
		cfg := &Config{Expiration: 1 * time.Hour}
		e := &item{cacheControl: []byte("public")}
		result := isHeuristicFreshness(e, cfg, 3600)
		require.False(t, result)
	})

	t.Run("isHeuristicFreshness with expires", func(t *testing.T) {
		t.Parallel()
		cfg := &Config{Expiration: 1 * time.Hour}
		e := &item{cacheControl: []byte("public"), expires: []byte("Wed, 21 Oct 2015 07:28:00 GMT")}
		result := isHeuristicFreshness(e, cfg, uint64(25*time.Hour/time.Second))
		require.False(t, result)
	})

	t.Run("isHeuristicFreshness true", func(t *testing.T) {
		t.Parallel()
		cfg := &Config{Expiration: 1 * time.Hour}
		e := &item{cacheControl: []byte("public")}
		result := isHeuristicFreshness(e, cfg, uint64(25*time.Hour/time.Second))
		require.True(t, result)
	})

	t.Run("cacheBodyFetchError miss", func(t *testing.T) {
		t.Parallel()
		mask := func(_ string) string { return "***" }
		err := cacheBodyFetchError(mask, "key", errCacheMiss)
		require.Error(t, err)
		require.Contains(t, err.Error(), "no cached body")
	})

	t.Run("cacheBodyFetchError other", func(t *testing.T) {
		t.Parallel()
		mask := func(_ string) string { return "***" }
		originalErr := errors.New("storage error")
		err := cacheBodyFetchError(mask, "key", originalErr)
		require.Equal(t, originalErr, err)
	})
}

// cacheTestBNewApp returns an app that caches under cfg and whose GET /test
// answers "test" with cacheControl, unless empty, as its Cache-Control and with
// respHeaders.
func cacheTestBNewApp(cfg *Config, cacheControl string, respHeaders map[string]string) *fiber.App {
	app := fiber.New()
	app.Use(New(*cfg))
	app.Get("/test", func(c fiber.Ctx) error {
		if cacheControl != "" {
			c.Response().Header.Set(fiber.HeaderCacheControl, cacheControl)
		}
		for name, value := range respHeaders {
			c.Response().Header.Set(name, value)
		}
		return c.SendString("test")
	})
	return app
}

// cacheTestBCase is a scenario for cacheTestBRun: GET /test is sent to an app
// from cacheTestBNewApp, and the X-Cache status of the responses is checked.
type cacheTestBCase struct {
	// Response headers of the origin besides cacheControl.
	respHeaders map[string]string
	// Request headers, sent with every request.
	reqHeaders map[string]string
	// Request headers added to the second request.
	secondHeaders map[string]string
	// Headers the last response must carry.
	wantHeaders map[string]string
	name        string
	// Cache-Control of the origin's response, unless empty.
	cacheControl string
	// X-Cache of the first response.
	first string
	// X-Cache of the response to a second request; none is sent when it is empty.
	second string
	// Expiration is an hour unless the case sets another.
	cfg Config
}

// cacheTestBRun runs the cases as parallel subtests of t.
func cacheTestBRun(t *testing.T, cases []cacheTestBCase) {
	t.Helper()

	for i := range cases {
		tc := &cases[i]
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := tc.cfg
			if cfg.Expiration == 0 {
				cfg.Expiration = time.Hour
			}
			app := cacheTestBNewApp(&cfg, tc.cacheControl, tc.respHeaders)

			newRequest := func(headers ...map[string]string) *http.Request {
				var opts []apptest.Option
				for _, h := range headers {
					for name, value := range h {
						opts = append(opts, apptest.WithHeader(name, value))
					}
				}
				return apptest.Req(fiber.MethodGet, "/test", opts...)
			}
			resp := apptest.Do(t, app, newRequest(tc.reqHeaders))
			require.Equal(t, tc.first, resp.Header.Get("X-Cache"), "first request")
			if tc.second != "" {
				resp = apptest.Do(t, app, newRequest(tc.reqHeaders, tc.secondHeaders))
				require.Equal(t, tc.second, resp.Header.Get("X-Cache"), "second request")
			}
			for name, value := range tc.wantHeaders {
				require.Equal(t, value, resp.Header.Get(name), name)
			}
		})
	}
}

// Test_Cache_VaryAndAuth tests vary and auth functionality
func Test_Cache_VaryAndAuth(t *testing.T) {
	t.Parallel()

	t.Run("storeVaryManifest failure", func(t *testing.T) {
		t.Parallel()
		storage := newFailingCacheStorage()
		storage.errs["set|manifest"] = errors.New("storage fail")
		manager := &manager{storage: storage}
		err := storeVaryManifest(context.Background(), manager, "manifest", []string{"Accept"}, 3600*time.Second)
		require.Error(t, err)
	})

	t.Run("loadVaryManifest not found", func(t *testing.T) {
		t.Parallel()
		storage := newFailingCacheStorage()
		manager := &manager{storage: storage}
		varyNames, found, err := loadVaryManifest(context.Background(), manager, "nonexistent")
		require.NoError(t, err)
		require.False(t, found)
		require.Nil(t, varyNames)
	})

	cacheTestBRun(t, []cacheTestBCase{
		{
			name:         "vary with multiple headers",
			cacheControl: "max-age=3600",
			respHeaders:  map[string]string{"Vary": "Accept, Accept-Encoding"},
			reqHeaders:   map[string]string{"Accept": "application/json", "Accept-Encoding": "gzip"},
			first:        cacheMiss,
			second:       cacheHit,
		},
		{
			name:         "auth with must-revalidate",
			cacheControl: "must-revalidate, max-age=3600",
			reqHeaders:   map[string]string{"Authorization": "Bearer token1"},
			first:        cacheMiss,
			second:       cacheHit,
		},
	})
}

// Test_Cache_DateAndCacheControl tests date parsing and cache control
func Test_Cache_DateAndCacheControl(t *testing.T) {
	t.Parallel()

	cacheTestBRun(t, []cacheTestBCase{
		{
			name:         "date header parsing",
			cacheControl: "max-age=3600",
			respHeaders:  map[string]string{"Date": "Mon, 02 Jan 2006 15:04:05 GMT"},
			first:        cacheMiss,
		},
		{
			name:         "invalid date header",
			cacheControl: "max-age=3600",
			respHeaders:  map[string]string{"Date": "invalid"},
			first:        cacheMiss,
		},
		{name: "cache control with quoted values", cacheControl: `max-age=3600, ext="value, with, commas"`, first: cacheMiss},
		{name: "cache control with spaces", cacheControl: "max-age=3600  ,  public  ,  must-revalidate", first: cacheMiss},
	})
}

// Test_Cache_CacheControlCombinations tests common cache control directive combinations
func Test_Cache_CacheControlCombinations(t *testing.T) {
	t.Parallel()

	cacheTestBRun(t, []cacheTestBCase{
		{name: "max-age with public", cacheControl: "public, max-age=3600", first: cacheMiss, second: cacheHit},
		{name: "max-age with private", cacheControl: "private, max-age=3600", first: cacheUnreachable},
		{name: "s-maxage overrides max-age", cacheControl: "public, max-age=60, s-maxage=3600", first: cacheMiss, second: cacheHit},
		{name: "no-store prevents caching", cacheControl: "no-store", first: cacheUnreachable, second: cacheUnreachable},
		{name: "no-cache with etag", cacheControl: "no-cache", respHeaders: map[string]string{"ETag": `"123456"`}, first: cacheUnreachable},
		{name: "must-revalidate with max-age", cacheControl: "must-revalidate, max-age=3600", first: cacheMiss, second: cacheHit},
		{name: "proxy-revalidate with max-age", cacheControl: "public, proxy-revalidate, max-age=3600", first: cacheMiss, second: cacheHit},
		{name: "immutable with max-age", cacheControl: "public, max-age=31536000, immutable", first: cacheMiss, second: cacheHit},
		{name: "max-age=0 with must-revalidate", cacheControl: "max-age=0, must-revalidate", first: cacheUnreachable},
		{name: "public with no explicit max-age", cacheControl: "public", first: cacheMiss, second: cacheHit},
		{name: "multiple cache directives with extensions", cacheControl: `public, max-age=3600, custom="value"`, first: cacheMiss, second: cacheHit},
		{name: "private overrides public", cacheControl: "public, private, max-age=3600", first: cacheUnreachable},
		{name: "stale-while-revalidate with max-age", cacheControl: "max-age=60, stale-while-revalidate=120", first: cacheMiss, second: cacheHit},
		{name: "stale-if-error with max-age", cacheControl: "max-age=60, stale-if-error=3600", first: cacheMiss, second: cacheHit},
	})
}

// Test_Cache_RequestResponseDirectives tests caching behavior with various request/response cache-control directives
func Test_Cache_RequestResponseDirectives(t *testing.T) {
	t.Parallel()

	cacheTestBRun(t, []cacheTestBCase{
		{
			name:         "request with pragma no-cache",
			cacheControl: "max-age=3600",
			reqHeaders:   map[string]string{"Pragma": "no-cache"},
			first:        cacheMiss,
		},
		{
			name:          "request with max-age=0 directive",
			cacheControl:  "max-age=3600",
			secondHeaders: map[string]string{"Cache-Control": "max-age=0"},
			first:         cacheMiss,
			second:        cacheMiss,
		},
		{
			name:        "response with expires header",
			respHeaders: map[string]string{"Expires": time.Now().Add(1 * time.Hour).UTC().Format(http.TimeFormat)},
			first:       cacheMiss,
			second:      cacheHit,
		},
		{
			name:         "response with age header",
			cacheControl: "max-age=3600",
			respHeaders:  map[string]string{"Age": "30"},
			first:        cacheMiss,
			second:       cacheHit,
		},
		{
			name: "custom key generator",
			cfg: Config{
				KeyGenerator: func(c fiber.Ctx) string {
					return "custom-" + c.Path()
				},
			},
			first:  cacheMiss,
			second: cacheHit,
		},
		{
			name:          "only-if-cached with cache hit",
			cacheControl:  "max-age=3600",
			secondHeaders: map[string]string{"Cache-Control": "only-if-cached"},
			first:         cacheMiss,
			second:        cacheHit,
		},
		{name: "cache control with uppercase directives", cacheControl: "PUBLIC, MAX-AGE=3600", first: cacheMiss, second: cacheHit},
	})

	t.Run("negative expiration skips caching", func(t *testing.T) {
		t.Parallel()
		app := cacheTestBNewApp(&Config{Expiration: -1 * time.Second}, "", nil)

		rsp := apptest.Do(t, app, "/test")
		require.NotEqual(t, cacheMiss, rsp.Header.Get("X-Cache"))
		require.NotEqual(t, cacheHit, rsp.Header.Get("X-Cache"))
	})

	t.Run("request with no-store directive", func(t *testing.T) {
		t.Parallel()
		app := cacheTestBNewApp(&Config{Expiration: 1 * time.Hour}, "", nil)

		req := apptest.Req(fiber.MethodGet, "/test", apptest.WithHeader("Cache-Control", "no-store"))
		rsp := apptest.Do(t, app, req)
		require.NotEqual(t, cacheMiss, rsp.Header.Get("X-Cache"))
	})

	t.Run("non-get-head method bypasses cache", func(t *testing.T) {
		t.Parallel()
		app := fiber.New()
		app.Use(New(Config{Expiration: 1 * time.Hour}))
		app.Post("/test", func(c fiber.Ctx) error {
			return c.SendString("test")
		})

		cacheTestBExpect(t, app, apptest.Req(fiber.MethodPost, "/test"), cacheUnreachable)
	})

	t.Run("request with min-fresh directive", func(t *testing.T) {
		t.Parallel()
		app := cacheTestBNewApp(&Config{Expiration: 1 * time.Hour}, "max-age=60", nil)

		// First request to cache
		cacheTestBExpect(t, app, "/test", cacheMiss)

		// Second request with min-fresh that's too high
		req := apptest.Req(fiber.MethodGet, "/test", apptest.WithHeader("Cache-Control", "min-fresh=120"))
		rsp := apptest.Do(t, app, req)
		// Should be a miss because min-fresh requirement not met
		cacheStatus := rsp.Header.Get("X-Cache")
		require.Contains(t, []string{cacheMiss, cacheUnreachable}, cacheStatus, "min-fresh requirement should prevent cache hit")
	})

	t.Run("request with max-stale directive", func(t *testing.T) {
		t.Parallel()
		app := cacheTestBNewApp(&Config{Expiration: 1 * time.Second}, "max-age=1", nil)

		// First request to cache
		cacheTestBExpect(t, app, "/test", cacheMiss)

		// Wait for it to become stale
		time.Sleep(2 * time.Second)

		// Request with max-stale to accept stale content
		req := apptest.Req(fiber.MethodGet, "/test", apptest.WithHeader("Cache-Control", "max-stale=60"))
		rsp := apptest.Do(t, app, req)
		// max-stale should allow serving stale content
		cacheStatus := rsp.Header.Get("X-Cache")
		// Should be either a hit (if stale is served) or miss (if revalidated)
		require.Contains(t, []string{cacheHit, cacheMiss, "stale"}, cacheStatus, "max-stale should allow stale content or revalidate")
	})

	t.Run("response with warning header", func(t *testing.T) {
		t.Parallel()
		app := cacheTestBNewApp(&Config{Expiration: 1 * time.Second}, "max-age=1", nil)

		// Cache the response
		cacheTestBExpect(t, app, "/test", cacheMiss)

		// Wait for it to become stale
		time.Sleep(2 * time.Second)

		// Request again - should get stale warning or revalidate
		rsp := apptest.Do(t, app, "/test")
		// Check that either cache miss (revalidation) or warning header is present
		cacheStatus := rsp.Header.Get("X-Cache")
		warningHeader := rsp.Header.Get("Warning")
		require.True(t, cacheStatus == cacheMiss || warningHeader != "", "stale response should either revalidate or have warning header")
	})

	t.Run("external storage with body key", func(t *testing.T) {
		t.Parallel()
		storage := newFailingCacheStorage()
		app := fiber.New()
		app.Use(New(Config{
			Expiration: 1 * time.Hour,
			Storage:    storage,
		}))
		app.Get("/test", func(c fiber.Ctx) error {
			return c.SendString("test content")
		})

		cacheTestBExpect(t, app, "/test", cacheMiss)

		// Verify body key is stored
		hasBodyKey := false
		storage.mu.RLock()
		for k := range storage.data {
			if strings.Contains(k, "_body") {
				hasBodyKey = true
				break
			}
		}
		storage.mu.RUnlock()
		require.True(t, hasBodyKey)
	})

	t.Run("only-if-cached with cache miss", func(t *testing.T) {
		t.Parallel()
		app := cacheTestBNewApp(&Config{Expiration: 1 * time.Hour}, "", nil)

		req := apptest.Req(fiber.MethodGet, "/test", apptest.WithHeader("Cache-Control", "only-if-cached"))
		apptest.Expect(t, app, req, fiber.StatusGatewayTimeout)
	})
}

// Test_Cache_ConfigurationAndResponseHandling tests cache behavior for specific configuration and response edge cases.
func Test_Cache_ConfigurationAndResponseHandling(t *testing.T) {
	t.Parallel()

	cacheTestBRun(t, []cacheTestBCase{
		{
			name:         "response with Vary star prevents caching",
			cacheControl: "max-age=3600",
			respHeaders:  map[string]string{"Vary": "*"},
			first:        cacheUnreachable,
		},
		{
			name:         "authorization without shared cache directives",
			cacheControl: "max-age=3600",
			reqHeaders:   map[string]string{"Authorization": "******"},
			first:        cacheUnreachable,
		},
		{
			name:         "disable cache control header generation",
			cfg:          Config{DisableCacheControl: true},
			cacheControl: "max-age=3600",
			first:        cacheMiss,
			second:       cacheHit,
		},
		{
			name:         "disable value redaction",
			cfg:          Config{DisableValueRedaction: true},
			cacheControl: "max-age=3600",
			first:        cacheMiss,
		},
		{
			name:         "response with ETag header",
			cacheControl: "max-age=3600",
			respHeaders:  map[string]string{"ETag": `"abc123"`},
			first:        cacheMiss,
			second:       cacheHit,
			wantHeaders:  map[string]string{"ETag": `"abc123"`},
		},
		{
			name:         "response with Content-Encoding header",
			cacheControl: "max-age=3600",
			respHeaders:  map[string]string{"Content-Encoding": "gzip"},
			first:        cacheMiss,
			second:       cacheHit,
			wantHeaders:  map[string]string{"Content-Encoding": "gzip"},
		},
		{
			name:         "response with custom headers preserved",
			cfg:          Config{StoreResponseHeaders: true},
			cacheControl: "max-age=3600",
			respHeaders:  map[string]string{"X-Custom-Header": "custom-value"},
			first:        cacheMiss,
			second:       cacheHit,
			wantHeaders:  map[string]string{"X-Custom-Header": "custom-value"},
		},
		{
			// A request with no-cache forces revalidation
			name:         "revalidation scenario with cache miss",
			cacheControl: "max-age=3600",
			reqHeaders:   map[string]string{"Cache-Control": "no-cache"},
			first:        cacheMiss,
		},
	})

	t.Run("next function prevents caching", func(t *testing.T) {
		t.Parallel()
		app := fiber.New()
		app.Use(New(Config{
			Expiration: 1 * time.Hour,
			Next: func(c fiber.Ctx) bool {
				return c.Path() == "/skip"
			},
		}))
		app.Get("/skip", func(c fiber.Ctx) error {
			return c.SendString("test")
		})

		cacheTestBExpect(t, app, "/skip", cacheUnreachable)
	})

	t.Run("non-cacheable status code", func(t *testing.T) {
		t.Parallel()
		app := fiber.New()
		app.Use(New(Config{Expiration: 1 * time.Hour}))
		app.Get("/test", func(c fiber.Ctx) error {
			return c.Status(fiber.StatusCreated).SendString("created")
		})

		cacheTestBExpect(t, app, "/test", cacheUnreachable)
	})

	t.Run("body larger than MaxBytes", func(t *testing.T) {
		t.Parallel()
		app := fiber.New()
		app.Use(New(Config{
			Expiration: 1 * time.Hour,
			MaxBytes:   10,
		}))
		app.Get("/test", func(c fiber.Ctx) error {
			return c.Send(make([]byte, 100))
		})

		cacheTestBExpect(t, app, "/test", cacheUnreachable)
	})

	t.Run("delete vary manifest on no-cache response", func(t *testing.T) {
		t.Parallel()
		app := fiber.New()
		app.Use(New(Config{Expiration: 1 * time.Hour}))
		app.Get("/test", func(c fiber.Ctx) error {
			// First request creates vary manifest
			if c.Query("first") != "" {
				c.Response().Header.Set("Vary", "Accept")
				c.Response().Header.Set("Cache-Control", "max-age=3600")
			} else {
				// Second request returns no-cache to delete manifest
				c.Response().Header.Set("Cache-Control", "no-cache")
			}
			return c.SendString("test")
		})

		req := apptest.Req(fiber.MethodGet, "/test?first=true", apptest.WithHeader("Accept", "application/json"))
		cacheTestBExpect(t, app, req, cacheMiss)

		// Second request without Vary should delete manifest
		cacheTestBExpect(t, app, "/test", cacheUnreachable)
	})

	t.Run("vary manifest deletion on different vary response", func(t *testing.T) {
		t.Parallel()
		app := fiber.New()
		var counter atomic.Int32
		app.Use(New(Config{Expiration: 1 * time.Hour}))
		app.Get("/test", func(c fiber.Ctx) error {
			if counter.Add(1) == 1 {
				c.Response().Header.Set("Vary", "Accept")
			}
			// Second response has no Vary header - should delete manifest
			c.Response().Header.Set("Cache-Control", "max-age=3600")
			return c.SendString("test")
		})

		req := apptest.Req(fiber.MethodGet, "/test", apptest.WithHeader("Accept", "application/json"))
		cacheTestBExpect(t, app, req, cacheMiss)

		// Second request - different vary behavior
		cacheTestBExpect(t, app, "/test", cacheMiss)
	})
}

// Test_hasDirective covers RFC 9111 §5.2 directive parsing including
// space, tab, and '=' as valid terminators (fixes #4143).
func Test_hasDirective(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		cc        string
		directive string
		want      bool
	}{
		// Basic matches
		{"exact match", "no-cache", "no-cache", true},
		{"comma separated", "public, no-cache, max-age=0", "no-cache", true},
		{"at start", "no-cache, max-age=0", "no-cache", true},
		{"at end", "public, no-cache", "no-cache", true},
		{"not present", "public, max-age=0", "no-cache", false},
		{"shorter token does not match", "no-catch", "no-cache", false},
		{"substring of longer token", "no-cache-extended", "no-cache", false},

		// Trailing whitespace (#4143)
		{"trailing space", "no-cache ", "no-cache", true},
		{"trailing tab", "no-cache\t", "no-cache", true},
		{"private trailing space", "private ", "private", true},

		// Directive with value (#4143)
		{"directive with equals", `no-cache="Set-Cookie"`, "no-cache", true},
		{"max-age with value", "max-age=3600", "max-age", true},
		{"s-maxage with value in list", "public, s-maxage=600, max-age=3600", "s-maxage", true},

		// Tab as separator before directive
		{"tab before directive", "public,\tno-cache", "no-cache", true},

		// Case insensitive
		{"case insensitive", "No-Cache", "no-cache", true},

		// Empty / edge cases
		{"empty header", "", "no-cache", false},
		{"empty directive never matches", "no-cache", "", false},
		{"empty directive empty header", "", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := hasDirective(tc.cc, tc.directive)
			require.Equal(t, tc.want, got)
		})
	}
}

// Test_CacheInvalidator_NoDataRace exercises the path where one request
// triggers CacheInvalidator (writing e.exp under mux.Lock) while concurrent
// requests evaluate freshness (reading e.exp). The fields must be read under
// the lock; -race will flag a regression. The test also asserts the
// invalidator path actually drives fresh handler executions, so a regression
// that silently disabled invalidation would also fail.
func Test_CacheInvalidator_NoDataRace(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		invalidator  func(fiber.Ctx) bool
		invalidating func() *http.Request
		name         string
	}{
		{
			name:         "RaceWithExactTimestamp",
			invalidator:  cacheTestBInvalidateByQuery,
			invalidating: func() *http.Request { return apptest.Req(fiber.MethodGet, "/?invalidate=true") },
		},
		{
			name: "SharedEntryNoDataRace",
			// Keyed on a header outside the cache key, so every request shares one entry.
			invalidator: func(c fiber.Ctx) bool {
				return c.Get("X-Invalidate") == "1"
			},
			invalidating: func() *http.Request {
				return apptest.Req(fiber.MethodGet, "/", apptest.WithHeader("X-Invalidate", "1"))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var handlerCalls atomic.Uint64
			app := fiber.New()
			app.Use(New(Config{
				CacheInvalidator: tc.invalidator,
				// Long expiration so the only source of fresh handler executions
				// after priming is the CacheInvalidator branch.
				Expiration: time.Hour,
			}))

			app.Get("/", func(c fiber.Ctx) error {
				return c.SendString(strconv.FormatUint(handlerCalls.Add(1), 10))
			})

			// Prime the cache so the served-from-cache branch is reachable.
			resp := apptest.Do(t, app, "/")
			require.NoError(t, resp.Body.Close())
			primed := handlerCalls.Load()

			var (
				wg            sync.WaitGroup
				invalidations atomic.Uint64
				errCount      atomic.Uint64
			)
			const workers = 16
			const iterations = 50
			for i := range workers {
				wg.Go(func() {
					for j := range iterations {
						req := apptest.Req(fiber.MethodGet, "/")
						if (i+j)%4 == 0 {
							req = tc.invalidating()
							invalidations.Add(1)
						}
						resp, err := app.Test(req)
						if err != nil {
							t.Errorf("app.Test: %v", err)
							errCount.Add(1)
							return
						}
						if err := resp.Body.Close(); err != nil {
							t.Errorf("Body.Close: %v", err)
							errCount.Add(1)
						}
					}
				})
			}
			wg.Wait()

			require.Zero(t, errCount.Load(), "worker goroutines reported errors")
			require.Positive(t, invalidations.Load(), "test never issued an invalidating request")
			// Each invalidation must drive at least one fresh handler execution.
			// Long Expiration guarantees natural expiry cannot account for the bump.
			require.Greater(t, handlerCalls.Load(), primed, "invalidator never bypassed the cache")
		})
	}
}

// go test -v -run=^$ -bench=Benchmark_parseCacheControlDirectives -benchmem -count=4
func Benchmark_parseCacheControlDirectives(b *testing.B) {
	inputs := [][]byte{
		[]byte("no-cache"),
		[]byte("public, max-age=3600"),
		[]byte("private, no-store, must-revalidate"),
		[]byte(`max-age=30, s-maxage=90, community="UCI", no-cache`),
		[]byte("max-age = 60 ,\tno-transform"),
	}
	var n int
	b.ReportAllocs()
	for b.Loop() {
		for _, in := range inputs {
			parseCacheControlDirectives(in, func(key, value []byte) {
				n += len(key) + len(value)
			})
		}
	}
	_ = n
}

// go test -v -run=^$ -bench=Benchmark_hasDirective -benchmem -count=4
func Benchmark_hasDirective(b *testing.B) {
	inputs := []string{
		"no-cache",
		"public, max-age=3600",
		"private, no-store, must-revalidate",
		"max-age=30, s-maxage=90, no-cache",
	}
	var got bool
	b.ReportAllocs()
	for b.Loop() {
		for _, in := range inputs {
			got = hasDirective(in, "no-cache")
		}
	}
	_ = got
}

func Test_Cache_OrphanedMetadataWithoutBody(t *testing.T) {
	t.Parallel()

	for _, maxBytes := range []uint{0, 4096} {
		t.Run(fmt.Sprintf("maxbytes-%d", maxBytes), func(t *testing.T) {
			t.Parallel()

			storage := newFailingCacheStorage()
			app := fiber.New()
			app.Use(New(Config{Storage: storage, Expiration: time.Hour, MaxBytes: maxBytes}))

			served := 0
			app.Get("/", func(c fiber.Ctx) error {
				served++
				return c.SendString(fmt.Sprintf("body %d", served))
			})

			cacheTestBExpect(t, app, "/", cacheMiss)

			var metadataKey string
			storage.mu.Lock()
			for key := range storage.data {
				if trimmed, ok := strings.CutSuffix(key, "_body"); ok {
					metadataKey = trimmed
					delete(storage.data, key)
					break
				}
			}
			storage.mu.Unlock()
			require.NotEmpty(t, metadataKey)

			cacheTestBExpectBody(t, app, "/", cacheMiss, "body 2")
			cacheTestBExpectBody(t, app, "/", cacheHit, "body 2")
		})
	}
}

func Test_Cache_BodyFetchFailure(t *testing.T) {
	t.Parallel()

	newApp := func(storage fiber.Storage) *fiber.App {
		app := fiber.New()
		app.Use(New(Config{Storage: storage, Expiration: time.Hour}))
		app.Get("/", func(c fiber.Ctx) error { return c.SendString("cached") })
		return app
	}

	prime := func(t *testing.T, app *fiber.App, storage *failingCacheStorage) string {
		t.Helper()

		cacheTestBExpect(t, app, "/", cacheMiss)

		var metadataKey string
		storage.mu.RLock()
		for key := range storage.data {
			if trimmed, ok := strings.CutSuffix(key, "_body"); ok {
				metadataKey = trimmed
				break
			}
		}
		storage.mu.RUnlock()
		require.NotEmpty(t, metadataKey)
		return metadataKey
	}

	t.Run("body read fails", func(t *testing.T) {
		t.Parallel()

		storage := newFailingCacheStorage()
		app := newApp(storage)
		key := prime(t, app, storage)

		storage.mu.Lock()
		storage.errs["get|"+key+"_body"] = errors.New("boom")
		storage.mu.Unlock()

		apptest.Expect(t, app, "/", fiber.StatusInternalServerError)
	})

	t.Run("orphaned metadata delete fails", func(t *testing.T) {
		t.Parallel()

		storage := newFailingCacheStorage()
		app := newApp(storage)
		key := prime(t, app, storage)

		storage.mu.Lock()
		delete(storage.data, key+"_body")
		storage.errs["del|"+key] = errors.New("boom")
		storage.mu.Unlock()

		apptest.Expect(t, app, "/", fiber.StatusInternalServerError)
	})
}

func Test_Cache_VaryManifestStoreFailureUnreservesSpace(t *testing.T) {
	t.Parallel()

	const body = "0123456789abcdef"
	storage := newFailingCacheStorage()
	app := fiber.New()
	app.Use(New(Config{Storage: storage, Expiration: time.Hour, MaxBytes: 2 * uint(len(body))}))
	app.Get("/:name", func(c fiber.Ctx) error {
		c.Response().Header.Set(fiber.HeaderVary, "Accept-Language")
		return c.SendString(body)
	})

	manifestKey := cacheKeyVersion + "|GET|scheme=http|host=example.com|/a|q=|h=accept:0|accept-encoding:0|accept-language:0|vary"
	storage.mu.Lock()
	storage.errs["set|"+manifestKey] = errors.New("boom")
	storage.mu.Unlock()

	apptest.Expect(t, app, "/a", fiber.StatusInternalServerError)

	storage.mu.Lock()
	delete(storage.errs, "set|"+manifestKey)
	storage.mu.Unlock()

	// The abandoned reservation must be gone, or the second entry evicts the first.
	for _, path := range []string{"/a", "/b"} {
		resp := apptest.Do(t, app, path)
		require.Equal(t, cacheMiss, resp.Header.Get("X-Cache"), "path=%q", path)
	}
	for _, path := range []string{"/a", "/b"} {
		resp := apptest.Do(t, app, path)
		require.Equal(t, cacheHit, resp.Header.Get("X-Cache"), "path=%q", path)
	}
}

// Test_Cache_VaryIsReplayedOnHit asserts a hit carries the stored response's
// Vary whether or not response headers are stored. Vary used to live only in
// the StoreResponseHeaders list, so by default a hit went out without it, and a
// cache in front of the app — keying on the URL alone, as it then must — could
// serve one variant to every client. Vary now joins the fields the entry always
// carries, on one line, and the stored header list no longer holds a copy.
func Test_Cache_VaryIsReplayedOnHit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// A prefix of the Cache-Control the hit must carry, or empty when none
		// may go out.
		wantCacheControl string
		cfg              Config
	}{
		{name: "default", wantCacheControl: "public, max-age="},
		{name: "StoreResponseHeaders", cfg: Config{StoreResponseHeaders: true}, wantCacheControl: "public, max-age="},
		{name: "DisableVaryHeaders", cfg: Config{DisableVaryHeaders: true}, wantCacheControl: "public, max-age="},
		{name: "DisableCacheControl", cfg: Config{DisableCacheControl: true}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app := fiber.New()
			app.Use(New(tc.cfg))
			app.Get("/", func(c fiber.Ctx) error {
				c.Set(fiber.HeaderVary, fiber.HeaderAcceptLanguage)
				return c.SendString("page-in:" + c.Get(fiber.HeaderAcceptLanguage))
			})

			get := func(wantStatus string) *http.Response {
				t.Helper()
				req := httptest.NewRequest(fiber.MethodGet, "/", http.NoBody)
				req.Header.Set(fiber.HeaderAcceptLanguage, "en")
				resp := apptest.Do(t, app, req)
				require.Equal(t, wantStatus, resp.Header.Get("X-Cache"))

				require.Equal(t, "page-in:en", apptest.Body(t, resp))
				// Once, not once per place it is kept: under StoreResponseHeaders
				// the stored header list must not carry a second copy.
				require.Equal(t, []string{fiber.HeaderAcceptLanguage}, resp.Header.Values(fiber.HeaderVary), "(%s)", wantStatus)
				return resp
			}

			get(cacheMiss)
			resp := get(cacheHit)

			// A Vary naming no credential keeps the public form.
			cacheControl := resp.Header.Get(fiber.HeaderCacheControl)
			if tc.wantCacheControl == "" {
				require.Empty(t, cacheControl)
			} else {
				require.True(t, strings.HasPrefix(cacheControl, tc.wantCacheControl), "Cache-Control %q", cacheControl)
			}
		})
	}
}

// Test_Cache_CredentialKeyedHitIsPrivate asserts a hit on an entry holding one
// user's response says so. Such an entry has no Cache-Control of its own in the
// common case, and the "public, max-age" this middleware then wrote told a
// shared cache in front of the app it could store the hit and serve it to the
// next client asking for the URL: keyed on the URL alone, that cache never sees
// the cookie the entry was selected by. An entry is one user's whenever the
// response's Vary lists a credential header, or the key is partitioned by
// KeyCookies, KeyCredentialHeaders, or a credential header in KeyHeaders.
func Test_Cache_CredentialKeyedHitIsPrivate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		appCfg fiber.Config
		name   string
		// A Cache-Control of the handler's own.
		cacheControl string
		// What the hit must carry: Vary with its field lines joined, and the
		// exact Cache-Control, which defaults to the private form.
		wantVary         string
		wantCacheControl string
		// The Vary field lines the handler sends, and the spelling of the field
		// name it writes them under, "Vary" when empty.
		vary     []string
		varyName string
		cfg      Config
		// Whether the request carries the session cookie.
		noCookie bool
	}{
		{name: "Vary: Cookie", vary: []string{"Cookie"}, wantVary: "Cookie"},
		{name: "Vary: cookie in lower case", vary: []string{"cookie"}, wantVary: "cookie"},
		{name: "Vary: Cookie on a second field line", vary: []string{"Accept-Encoding", "Cookie"}, wantVary: "Accept-Encoding,Cookie"},
		{name: "Vary: Authorization", vary: []string{"Authorization"}, wantVary: "Authorization"},
		{name: "Vary: Proxy-Authorization", vary: []string{"Proxy-Authorization"}, wantVary: "Proxy-Authorization"},
		{name: "Vary: X-API-Key", vary: []string{"X-API-Key"}, wantVary: "X-API-Key"},
		{name: "KeyCookies", cfg: Config{KeyCookies: []string{"session"}}},
		{name: "KeyCookies without the cookie on the request", cfg: Config{KeyCookies: []string{"session"}}, noCookie: true},
		{name: "KeyHeaders naming Cookie", cfg: Config{KeyHeaders: []string{"Cookie"}}},
		{name: "KeyHeaders naming Authorization", cfg: Config{KeyHeaders: []string{"Authorization"}}},
		{name: "KeyCredentialHeaders", cfg: Config{KeyCredentialHeaders: []string{"X-Company-Token"}}},
		{name: "KeyHeaders naming Proxy-Authorization", cfg: Config{KeyHeaders: []string{"Proxy-Authorization"}}},
		{name: "KeyHeaders naming X-API-Key", cfg: Config{KeyHeaders: []string{"X-API-Key"}}},
		{name: "KeyHeaders naming X-Auth-Token", cfg: Config{KeyHeaders: []string{"X-Auth-Token"}}},
		{name: "KeyHeaders naming X-Access-Token", cfg: Config{KeyHeaders: []string{"X-Access-Token"}}},
		// KeyHeaders naming a credential header makes every entry one client's, whatever the generator.
		{name: "custom KeyGenerator, KeyHeaders naming Cookie, without the cookie on the request", cfg: Config{KeyGenerator: pathKey, KeyHeaders: []string{"Cookie"}}, noCookie: true},
		{name: "custom KeyGenerator, KeyHeaders naming Authorization", cfg: Config{KeyGenerator: pathKey, KeyHeaders: []string{"Authorization"}}},
		{name: "DisableVaryHeaders", cfg: Config{DisableVaryHeaders: true}, vary: []string{"Cookie"}, wantVary: "Cookie"},
		{name: "DisableHeaderNormalizing", appCfg: fiber.Config{DisableHeaderNormalizing: true}, vary: []string{"cookie"}, wantVary: "cookie"},
		{name: "DisableHeaderNormalizing with the field name in lower case", appCfg: fiber.Config{DisableHeaderNormalizing: true}, vary: []string{"Cookie"}, varyName: "vary", wantVary: "Cookie"},
		{
			name:             "the handler's own Cache-Control is kept",
			vary:             []string{"Cookie"},
			cacheControl:     "max-age=60",
			wantVary:         "Cookie",
			wantCacheControl: "max-age=60",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// A fixed clock, so the delta-seconds written on the hit is the whole
			// lifetime rather than whatever the wall clock left of it.
			clock := newTestClock(time.Unix(1_700_000_000, 0))
			cfg := tc.cfg
			cfg.Expiration = time.Minute
			cfg.clock = clock.Now
			wantCacheControl := tc.wantCacheControl
			if wantCacheControl == "" {
				wantCacheControl = "private, max-age=60"
			}

			app := fiber.New(tc.appCfg)
			app.Use(New(cfg))
			varyName := tc.varyName
			if varyName == "" {
				varyName = fiber.HeaderVary
			}
			app.Get("/me", func(c fiber.Ctx) error {
				for _, v := range tc.vary {
					c.Response().Header.Add(varyName, v)
				}
				if tc.cacheControl != "" {
					c.Set(fiber.HeaderCacheControl, tc.cacheControl)
				}
				return c.SendString("page-for:" + c.Cookies("session"))
			})

			wantBody := "page-for:alice"
			if tc.noCookie {
				wantBody = "page-for:"
			}
			get := func(wantStatus string) *http.Response {
				t.Helper()
				req := httptest.NewRequest(fiber.MethodGet, "/me", http.NoBody)
				if !tc.noCookie {
					req.Header.Set(fiber.HeaderCookie, "session=alice")
				}
				resp := apptest.Do(t, app, req)
				require.Equal(t, wantStatus, resp.Header.Get("X-Cache"))

				require.Equal(t, wantBody, apptest.Body(t, resp))
				require.Equal(t, tc.wantVary, strings.Join(resp.Header.Values(fiber.HeaderVary), ","), "(%s)", wantStatus)
				return resp
			}

			get(cacheMiss)
			resp := get(cacheHit)
			require.Equal(t, wantCacheControl, resp.Header.Get(fiber.HeaderCacheControl))
		})
	}
}

// Test_Cache_EntryStoredWithoutVaryIsNotRead covers an entry a previous version
// stored. It carried no Vary, so it would be replayed without one and, holding
// no Cache-Control of its own, under "public, max-age" — whatever cookie it was
// selected by. The key namespace is versioned so no such entry is ever read.
func Test_Cache_EntryStoredWithoutVaryIsNotRead(t *testing.T) {
	t.Parallel()

	storage := memory.New()
	app := fiber.New()
	app.Use(New(Config{
		Storage:    storage,
		Expiration: time.Minute,
		// A generator of the caller's own, as an application preserving its
		// key format across the upgrade would have.
		KeyGenerator: func(c fiber.Ctx) string { return c.Path() },
	}))
	app.Get("/me", func(c fiber.Ctx) error { return c.SendString("handler ran") })

	// Seed what the previous version wrote for a "Vary: Cookie" response to
	// alice: the manifest naming the cookie, and her entry — metadata and body —
	// under the variant key that manifest produces for her cookie.
	const legacyBase = "v3|GET|/me"
	var hdr fasthttp.RequestHeader
	hdr.Set(fiber.HeaderCookie, "session=alice")
	legacyKey := varyKey(legacyBase, []string{"cookie"}, &hdr, true)

	legacy := &item{
		body:   []byte("alice's page"),
		ctype:  []byte(fiber.MIMETextPlainCharsetUTF8),
		status: fiber.StatusOK,
		exp:    uint64(time.Now().Add(time.Minute).Unix()),
		ttl:    uint64(time.Minute / time.Second),
	}
	raw, err := legacy.MarshalMsg(nil)
	require.NoError(t, err)
	require.NoError(t, storage.Set(legacyBase+"|vary", []byte("cookie"), time.Minute))
	require.NoError(t, storage.Set(legacyKey, raw, time.Minute))
	require.NoError(t, storage.Set(legacyKey+"_body", legacy.body, time.Minute))

	req := httptest.NewRequest(fiber.MethodGet, "/me", http.NoBody)
	req.Header.Set(fiber.HeaderCookie, "session=alice")
	resp := apptest.Expect(t, app, req, fiber.StatusOK)

	body := apptest.Body(t, resp)
	require.Equal(t, "handler ran", body, "an entry stored without its Vary must not be served")
	require.Equal(t, cacheMiss, resp.Header.Get("X-Cache"))
}

// Test_Cache_VaryReplayedWithCompress asserts the pair compress writes comes
// back together: a hit that replays Content-Encoding without the
// Vary: Accept-Encoding beside it tells a cache in front of the app the encoded
// body suits every client.
func Test_Cache_VaryReplayedWithCompress(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(New())
	app.Use(compress.New())
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString(strings.Repeat("compressible ", 256))
	})

	for _, want := range []string{cacheMiss, cacheHit} {
		req := httptest.NewRequest(fiber.MethodGet, "/", http.NoBody)
		req.Header.Set(fiber.HeaderAcceptEncoding, "gzip")
		resp := apptest.Do(t, app, req)
		require.Equal(t, want, resp.Header.Get("X-Cache"))
		require.Equal(t, "gzip", resp.Header.Get(fiber.HeaderContentEncoding), "(%s)", want)
		require.Equal(t, []string{fiber.HeaderAcceptEncoding}, resp.Header.Values(fiber.HeaderVary), "(%s)", want)
	}
}

// Test_Cache_VaryLongerThanEntryLimitIsUncacheable pins the cap on a stored
// Vary to the limit the entry decodes it under. The encoder does not enforce
// that limit, so a longer value reached an external Storage and every later
// read of the entry failed on it, and the route answered with that error until
// the entry expired. Such a response is not stored at all, and a value exactly
// at the limit still round-trips.
func Test_Cache_VaryLongerThanEntryLimitIsUncacheable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		wantSecond string
		varyLen    int
	}{
		{name: "at the limit", varyLen: maxVaryLen, wantSecond: cacheHit},
		{name: "past the limit", varyLen: maxVaryLen + 1, wantSecond: cacheUnreachable},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// One field name of the given length: fewer names than
			// maxVaryHeaders, so only the byte length is in play.
			vary := "X-" + strings.Repeat("a", tc.varyLen-2)

			app := fiber.New()
			app.Use(New(Config{Storage: memory.New(), Expiration: time.Minute}))
			app.Get("/", func(c fiber.Ctx) error {
				c.Set(fiber.HeaderVary, vary)
				return c.SendString("body")
			})

			for i, want := range []string{"", tc.wantSecond} {
				resp := apptest.Do(t, app, "/")
				require.Equal(t, fiber.StatusOK, resp.StatusCode, "request %d", i+1)
				if want != "" {
					require.Equal(t, want, resp.Header.Get("X-Cache"), "request %d", i+1)
				}
				body := apptest.Body(t, resp)
				require.Equal(t, "body", body, "request %d", i+1)
				require.Equal(t, []string{vary}, resp.Header.Values(fiber.HeaderVary), "request %d", i+1)
			}
		})
	}
}

func Test_Cache_CredentialHeadersAreHashedInTheKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		header string
		cfg    Config
	}{
		{name: "declared in KeyCredentialHeaders", header: "X-Company-Token", cfg: Config{KeyCredentialHeaders: []string{"X-Company-Token"}}},
		{name: "recognized in KeyHeaders", header: "X-API-Key", cfg: Config{KeyHeaders: []string{"X-API-Key"}}},
	}

	const token = "first-company-token"
	const otherToken = "second-company-token"

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			storage := newFailingCacheStorage()
			cfg := tc.cfg
			cfg.Storage = storage
			app := fiber.New()
			app.Use(New(cfg))
			app.Get("/", func(c fiber.Ctx) error { return c.SendString("for " + c.Get(tc.header)) })

			get := func(value string) string {
				t.Helper()
				return getRoot(t, app, map[string]string{tc.header: value}).cache
			}

			// The credential still tells entries apart.
			require.Equal(t, cacheMiss, get(token))
			require.Equal(t, cacheHit, get(token))
			require.Equal(t, cacheMiss, get(otherToken))

			storage.mu.RLock()
			defer storage.mu.RUnlock()
			require.NotEmpty(t, storage.data)
			for key := range storage.data {
				require.NotContains(t, key, token)
				require.NotContains(t, key, otherToken)
			}
		})
	}
}

// With a custom KeyGenerator, KeyCredentialHeaders takes no part in the key, but a hit for a request
// that sends one of the headers is private and not served stale. One that sends none finds the shared entry.
func Test_Cache_KeyCredentialHeadersWithCustomKeyGenerator(t *testing.T) {
	t.Parallel()

	token := map[string]string{"X-Company-Token": "t"}
	tests := []struct {
		headers    map[string]string
		name       string
		keyHeaders []string
		declared   bool
		private    bool
	}{
		{name: "declared, request sends it", declared: true, headers: token, private: true},
		{name: "declared, anonymous request", declared: true},
		{name: "not declared, so the entry is shared", headers: token},
		// Named in KeyHeaders, the header is taken to be in the key, as before the option existed.
		{name: "named in KeyHeaders, anonymous request", keyHeaders: []string{"X-API-Key"}, private: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			clock := newTestClock(time.Unix(1_700_000_000, 0))
			cfg := Config{
				Expiration:   time.Minute,
				clock:        clock.Now,
				KeyGenerator: pathKey,
			}
			if tc.declared {
				cfg.KeyCredentialHeaders = []string{"X-Company-Token"}
			}
			cfg.KeyHeaders = tc.keyHeaders
			var calls atomic.Int32
			app := fiber.New()
			app.Use(New(cfg))
			app.Get("/", func(c fiber.Ctx) error {
				calls.Add(1)
				return c.SendString("body")
			})

			require.Equal(t, cacheMiss, getRoot(t, app, tc.headers).cache)

			hit := getRoot(t, app, tc.headers)
			require.Equal(t, cacheHit, hit.cache)
			wantCacheControl := "public, max-age=60"
			if tc.private {
				wantCacheControl = "private, max-age=60"
			}
			require.Equal(t, wantCacheControl, hit.header.Get(fiber.HeaderCacheControl))

			// Past the entry's lifetime, well inside what the storage keeps.
			clock.Add(2 * time.Minute)
			headers := map[string]string{fiber.HeaderCacheControl: "max-stale"}
			maps.Copy(headers, tc.headers)
			got := getRoot(t, app, headers)
			if tc.private {
				require.Equal(t, int32(2), calls.Load(), "an entry holding a credential's response was served stale")
				return
			}
			require.Equal(t, cacheHit, got.cache)
			require.Equal(t, int32(1), calls.Load())
		})
	}
}

func Test_configDefault_KeyCredentialHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		wantKeyHeaders  []string
		wantCredentials []string
		wantCarrying    []string // the KeyHeaders that carry a credential
		cfg             Config
	}{
		{
			name:           "unset",
			wantKeyHeaders: []string{"accept", "accept-encoding", "accept-language"},
		},
		{
			name:            "joins the default key headers",
			cfg:             Config{KeyCredentialHeaders: []string{"X-Company-Token"}},
			wantKeyHeaders:  []string{"accept", "accept-encoding", "accept-language", "x-company-token"},
			wantCredentials: []string{"x-company-token"},
			wantCarrying:    []string{"x-company-token"},
		},
		{
			name: "normalized, trimmed, deduplicated and merged with KeyHeaders",
			cfg: Config{
				KeyHeaders:           []string{"X-Company-Token", "Accept"},
				KeyCredentialHeaders: []string{" X-Company-Token ", "x-company-token", "", "X-Other"},
			},
			wantKeyHeaders:  []string{"accept", "x-company-token", "x-other"},
			wantCredentials: []string{"x-company-token", "x-other"},
			wantCarrying:    []string{"x-company-token", "x-other"},
		},
		{
			name: "with header partitioning otherwise disabled",
			cfg: Config{
				KeyHeaders:           []string{},
				KeyCredentialHeaders: []string{"X-Company-Token"},
			},
			wantKeyHeaders:  []string{"x-company-token"},
			wantCredentials: []string{"x-company-token"},
			wantCarrying:    []string{"x-company-token"},
		},
		{
			name:           "recognized headers named in KeyHeaders carry a credential, others do not",
			cfg:            Config{KeyHeaders: []string{"X-Tenant", "x-api-key", "Accept", "Authorization"}},
			wantKeyHeaders: []string{"accept", "authorization", "x-api-key", "x-tenant"},
			wantCarrying:   []string{"authorization", "x-api-key"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// The caller's slices are theirs: normalizing must not rewrite them.
			keyHeaders := slices.Clone(tc.cfg.KeyHeaders)
			credentials := slices.Clone(tc.cfg.KeyCredentialHeaders)

			cfg := configDefault(tc.cfg)
			require.Equal(t, tc.wantKeyHeaders, cfg.KeyHeaders)
			require.Equal(t, tc.wantCredentials, cfg.KeyCredentialHeaders)
			require.Equal(t, tc.wantCarrying, cfg.credentialKeyHeaders)
			require.Equal(t, keyHeaders, tc.cfg.KeyHeaders)
			require.Equal(t, credentials, tc.cfg.KeyCredentialHeaders)
		})
	}
}

package cache

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/internal/apptest"
	"github.com/stretchr/testify/require"
)

// cacheTestCWait receives from ch, failing the test with msg unless a value
// arrives within two seconds.
func cacheTestCWait[T any](t *testing.T, ch <-chan T, msg string) T {
	t.Helper()

	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		t.Fatal(msg)
	}
	var unreachable T // Fatal has stopped the test
	return unreachable
}

// cacheTestCEvict sends GET /new through app in the background, a request the
// test arranged to fail with a 500 because the eviction it starts cannot delete
// the entry it picked. The channel yields what went wrong with the request or
// with its answer, nil when nothing did.
func cacheTestCEvict(app *fiber.App) <-chan error {
	done := make(chan error, 1)
	go func() {
		resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/new", http.NoBody))
		if err == nil {
			if resp.StatusCode != fiber.StatusInternalServerError {
				err = errors.New("eviction did not fail")
			}
			if closeErr := resp.Body.Close(); closeErr != nil {
				err = errors.Join(err, closeErr)
			}
		}
		done <- err
	}()
	return done
}

func Test_CacheStorage_SlowRestoreDoesNotBlockUnrelatedHit(t *testing.T) {
	t.Parallel()
	const oldKey = cacheKeyVersion + "|GET|/old"
	started := make(chan struct{})
	resume := make(chan struct{})
	var armed atomic.Bool
	storage := &hookedStorage{failingCacheStorage: newFailingCacheStorage(), onGet: func(key string) {
		// The read that restores the entry eviction picked parks until the test lets it go on.
		if key == oldKey && armed.CompareAndSwap(true, false) {
			close(started)
			<-resume
		}
	}}
	probe := &accountingProbe{}
	app := cacheTestBPathKeyedApp(&Config{MaxBytes: 10, Storage: storage, accounting: probe.record})
	app.Get("/:name", func(c fiber.Ctx) error {
		switch c.Path() {
		case "/old":
			c.Set(fiber.HeaderCacheControl, "public, max-age=30")
			return c.SendString("stale")
		case "/hot":
			c.Set(fiber.HeaderCacheControl, "public, max-age=60")
			return c.SendString("hot")
		default:
			c.Set(fiber.HeaderCacheControl, "public, max-age=60")
			return c.SendString("fresh")
		}
	})

	for _, path := range []string{"/old", "/hot"} {
		cacheTestAExpect(t, app, path, cacheMiss)
	}
	cacheTestBRequireAccounting(t, probe, 8, 2)

	storage.setErr("del", oldKey, errors.New("eviction delete failed"))
	armed.Store(true)

	evictionDone := cacheTestCEvict(app)

	var resumeOnce sync.Once
	release := func() { resumeOnce.Do(func() { close(resume) }) }
	defer release()

	cacheTestCWait(t, started, "eviction did not reach restore read")

	hitDone := make(chan error, 1)
	go func() {
		resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/hot", http.NoBody))
		if err == nil {
			if resp.Header.Get("X-Cache") != cacheHit {
				err = errors.New("unrelated cache entry was not a hit")
			}
			if closeErr := resp.Body.Close(); closeErr != nil {
				err = errors.Join(err, closeErr)
			}
		}
		hitDone <- err
	}()

	require.NoError(t, cacheTestCWait(t, hitDone, "unrelated cache hit blocked on restore storage I/O"))
	require.Equal(t, uint(8), probe.counted(), "pending restore must retain its byte reservation")
	require.Equal(t, 2, probe.nodes())

	release()
	require.NoError(t, cacheTestCWait(t, evictionDone, "eviction did not finish after storage resumed"))
	cacheTestBRequireAccounting(t, probe, 8, 2)
}

func Test_CacheStorage_RestoredGenerationSurvivesIndexReuse(t *testing.T) {
	t.Parallel()
	storage := &blockingFailedEvictionStorage{
		failingCacheStorage: newFailingCacheStorage(),
		key:                 cacheKeyVersion + "|GET|/old",
		started:             make(chan struct{}),
		continueDelete:      make(chan struct{}),
	}
	probe := &accountingProbe{}
	var uncacheable atomic.Bool
	app := cacheTestBPathKeyedApp(&Config{MaxBytes: 10, Storage: storage, accounting: probe.record})
	app.Get("/:name", func(c fiber.Ctx) error {
		switch c.Path() {
		case "/old":
			if uncacheable.Load() {
				c.Set(fiber.HeaderCacheControl, "no-store")
				return c.SendString("newer")
			}
			c.Set(fiber.HeaderCacheControl, "public, max-age=30")
			return c.SendString("stale")
		case "/slot":
			c.Set(fiber.HeaderCacheControl, "public, max-age=60")
			return c.SendString("")
		default:
			c.Set(fiber.HeaderCacheControl, "public, max-age=60")
			return c.SendString("0123456789")
		}
	})

	apptest.BodyBytes(t, apptest.Do(t, app, "/old"))

	storage.armed.Store(true)
	evictionDone := cacheTestCEvict(app)

	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(storage.continueDelete) }) }
	defer release()
	cacheTestCWait(t, storage.started, "eviction did not reach blocked deletion")

	cacheTestAExpect(t, app, "/slot", cacheMiss)
	release()
	require.NoError(t, cacheTestCWait(t, evictionDone, "eviction did not finish"))
	cacheTestBRequireAccounting(t, probe, 5, 2)

	uncacheable.Store(true)
	apptest.BodyBytes(t, apptest.Expect(t, app, cacheTestAGetCC("/old", "max-age=0"), fiber.StatusOK))
	cacheTestBRequireAccounting(t, probe, 0, 1)
}

package cache

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/internal/apptest"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// Variants share a manifest while their bodies occupy distinct storage keys.
// Mutations happen during the origin call, after the stale entry was read.
func Test_Cache_UncacheableVaryCleanup(t *testing.T) {
	t.Parallel()

	for _, kind := range storageKinds {
		external := kind == "external"
		for _, directive := range []string{"private", "no-cache", "no-store", "VaryStar"} {
			for _, scenario := range []string{"success", "absent", "readError", "metadataDeleteError", "bodyDeleteError", "replaced", "plainNoStoreMiss"} {
				if scenario == "plainNoStoreMiss" && directive != "no-store" {
					continue
				}
				if !external && (scenario == "readError" || scenario == "metadataDeleteError" || scenario == "bodyDeleteError") {
					continue
				}
				t.Run(kind+"/"+directive+"/"+scenario, func(t *testing.T) {
					t.Parallel()

					storage := newFailingCacheStorage()
					clock := newTestClock(time.Now())
					cfg := Config{MaxBytes: 10, clock: clock.Now}
					if external {
						cfg.Storage = storage
					}
					baseKey := cacheKeyVersion + "|GET|/cached"
					manifestKey := baseKey + "|vary"
					var header fasthttp.RequestHeader
					header.Set("X-Variant", "a")
					key := varyKey(baseKey, []string{"x-variant"}, &header, true)
					app := cacheTestBPathKeyedApp(&cfg)
					app.Get("/cached", func(c fiber.Ctx) error {
						c.Set(fiber.HeaderVary, "X-Variant")
						c.Set(fiber.HeaderCacheControl, "public, max-age=60")
						if c.Get("X-Action") == "cleanup" {
							if scenario == "replaced" || (!external && scenario == "absent") {
								variant := "a"
								if scenario == "absent" {
									variant = "c" // Evicts a, whose expiry precedes b's.
								}
								request := apptest.Req(fiber.MethodGet, "/cached",
									apptest.WithHeader("X-Variant", variant), apptest.WithHeader(fiber.HeaderCacheControl, "max-age=0"))
								response, err := app.Test(request)
								if err != nil {
									return err
								}
								if err := response.Body.Close(); err != nil {
									return fmt.Errorf("close nested response: %w", err)
								}
								if response.StatusCode != fiber.StatusOK {
									return errors.New("nested store failed")
								}
							} else if external {
								storage.mu.Lock()
								switch scenario {
								case "absent":
									delete(storage.data, key)
									delete(storage.data, key+"_body")
								case "readError":
									storage.errs["get|"+key] = errors.New("verification failed")
								case "metadataDeleteError":
									storage.errs["del|"+key] = errors.New("metadata delete failed")
								case "bodyDeleteError":
									storage.errs["del|"+key+"_body"] = errors.New("body delete failed")
								}
								storage.mu.Unlock()
							}
							if directive == "VaryStar" {
								c.Set(fiber.HeaderVary, "*")
							} else {
								c.Set(fiber.HeaderCacheControl, directive)
							}
							return c.SendString("newer")
						}
						return c.SendString("fresh")
					})

					request := func(variant, action string) reply {
						t.Helper()
						req := apptest.Req(fiber.MethodGet, "/cached", apptest.WithHeader("X-Variant", variant))
						if action != "" {
							req.Header.Set("X-Action", action)
							req.Header.Set(fiber.HeaderCacheControl, "max-age=0")
						}
						return doRequest(t, app, req)
					}
					for _, variant := range []string{"a", "b"} {
						require.Equal(t, cacheMiss, request(variant, "").cache)
						clock.Add(time.Second)
					}
					variant := "a"
					if scenario == "plainNoStoreMiss" {
						variant = "c"
					}
					got := request(variant, "cleanup")
					require.Equal(t, fiber.StatusOK, got.status)
					require.Equal(t, "newer", got.body)
					require.Equal(t, cacheUnreachable, got.cache)
					preserveManifest := scenario == "replaced" || scenario == "plainNoStoreMiss"
					if external {
						storage.mu.Lock()
						_, manifestPresent := storage.data[manifestKey]
						storage.errs = make(map[string]error)
						storage.mu.Unlock()
						require.Equal(t, preserveManifest, manifestPresent)
					}
					got = request("b", "")
					require.Equal(t, fiber.StatusOK, got.status)
					require.Equal(t, "fresh", got.body)
					wantStatus := cacheMiss
					if preserveManifest {
						wantStatus = cacheHit
					}
					require.Equal(t, wantStatus, got.cache)
					if scenario == "replaced" {
						require.Equal(t, cacheHit, request("a", "").cache)
					}
				})
			}
		}
	}
}

// A reload that lands on an error page must not purge the entry or the
// manifest its sibling variants hang on; a cacheable status still does. A 401, 403 or 407 is
// no verdict on an entry no credential selected, whatever it says about keeping the response.
func Test_Cache_UncacheableStatusKeepsEntryAndManifest(t *testing.T) {
	t.Parallel()

	answers := []struct {
		name      string
		directive string
		status    int
		purged    bool // the entry and the manifest go, so no variant is a hit afterwards
	}{
		{name: "503", status: fiber.StatusServiceUnavailable},
		{name: "503 no-store", status: fiber.StatusServiceUnavailable, directive: "no-store"},
		{name: "503 private", status: fiber.StatusServiceUnavailable, directive: "private"},
		{name: "503 no-cache", status: fiber.StatusServiceUnavailable, directive: "no-cache"},
		{name: "503 VaryStar", status: fiber.StatusServiceUnavailable, directive: "VaryStar"},
		{name: "429 no-store", status: fiber.StatusTooManyRequests, directive: "no-store"},
		{name: "401", status: fiber.StatusUnauthorized},
		{name: "403", status: fiber.StatusForbidden},
		{name: "407", status: fiber.StatusProxyAuthRequired},
		{name: "401 private", status: fiber.StatusUnauthorized, directive: "private"},
		{name: "403 no-store", status: fiber.StatusForbidden, directive: "no-store"},
		{name: "407 VaryStar", status: fiber.StatusProxyAuthRequired, directive: "VaryStar"},
		{name: "404 no-store", status: fiber.StatusNotFound, directive: "no-store", purged: true},
	}

	for _, kind := range storageKinds {
		for _, reload := range []string{"max-age=0", "no-cache"} {
			for _, answer := range answers {
				t.Run(kind+"/"+reload+"/"+answer.name, func(t *testing.T) {
					t.Parallel()

					app := fiber.New()
					app.Use(New(Config{Storage: storageOf(kind)}))
					app.Get("/cached", func(c fiber.Ctx) error {
						if c.Get(fiber.HeaderCacheControl) == "" {
							c.Set(fiber.HeaderVary, "X-Variant")
							c.Set(fiber.HeaderCacheControl, "public, max-age=60")
							return c.SendString("fresh")
						}
						switch answer.directive {
						case "":
						case "VaryStar":
							c.Set(fiber.HeaderVary, "*")
						default:
							c.Set(fiber.HeaderCacheControl, answer.directive)
						}
						return c.SendStatus(answer.status)
					})

					request := func(variant, cacheControl string) reply {
						t.Helper()
						return cacheTestCGet(t, app, "/cached", "X-Variant", variant, fiber.HeaderCacheControl, cacheControl)
					}

					for _, variant := range []string{"a", "b"} {
						require.Equal(t, cacheMiss, request(variant, "").cache)
					}
					got := request("a", reload)
					require.Equal(t, answer.status, got.status)
					require.Equal(t, cacheUnreachable, got.cache)

					want := cacheHit
					if answer.purged {
						want = cacheMiss
					}
					// Sibling first: a miss on a would store the manifest again.
					for _, variant := range []string{"b", "a"} {
						require.Equal(t, want, request(variant, "").cache)
					}
				})
			}
		}
	}
}

func Test_CacheStorage_PartialEvictionRetainsBodyRetry(t *testing.T) {
	t.Parallel()

	for _, failBodyRead := range []bool{false, true} {
		name := "bodyPresent"
		if failBodyRead {
			name = "bodyReadError"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			storage := newFailingCacheStorage()
			probe := &accountingProbe{}
			key := cacheKeyVersion + "|GET|/a"
			app := cacheTestBPathKeyedApp(&Config{Storage: storage, MaxBytes: 15, accounting: probe.record})
			app.Get("/:name", func(c fiber.Ctx) error {
				return c.SendString("0123456789")
			})
			request := func(path string, status int) {
				t.Helper()
				apptest.BodyBytes(t, apptest.Expect(t, app, path, status))
			}
			request("/a", fiber.StatusOK)
			storage.mu.Lock()
			storage.errs["del|"+key+"_body"] = errors.New("body deletion failed")
			if failBodyRead {
				storage.errs["get|"+key+"_body"] = errors.New("body verification failed")
			}
			storage.mu.Unlock()
			request("/b", fiber.StatusInternalServerError)
			cacheTestBRequireAccounting(t, probe, 10, 1)
			storage.mu.Lock()
			require.NotContains(t, storage.data, key)
			require.Contains(t, storage.data, key+"_body")
			storage.errs = make(map[string]error)
			storage.mu.Unlock()
			request("/c", fiber.StatusOK)
			storage.mu.RLock()
			require.NotContains(t, storage.data, key+"_body")
			storage.mu.RUnlock()
			cacheTestBRequireAccounting(t, probe, 10, 1)
		})
	}
}

func Test_manager_delIf(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"absent", "deleted", "replaced"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			manager := newManager(nil, true)
			expected := &item{body: []byte("stale")}
			replacement := &item{body: []byte("fresh")}
			want := memoryEntryAbsent
			switch scenario {
			case "deleted":
				want = memoryEntryDeleted
				manager.memory.Set("key", expected, time.Hour)
			case "replaced":
				want = memoryEntryReplaced
				manager.memory.Set("key", replacement, time.Hour)
			}
			require.Equal(t, want, manager.delIf("key", expected))
			if scenario == "replaced" {
				require.Same(t, replacement, manager.memory.Get("key"))
			} else {
				_, err := manager.get(context.Background(), "key")
				require.ErrorIs(t, err, errCacheMiss)
			}
		})
	}
}

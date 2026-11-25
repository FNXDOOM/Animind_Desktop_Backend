package cache

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"animind-backend-go/internal/config"
)

// These run against a real Redis: the Lua window and group sets cannot be proven against the
// in-process store. Point REDIS_TEST_URL at a disposable database; they skip otherwise.

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func redisConfig(url string) config.CacheConfig {
	return config.CacheConfig{
		URL:            url,
		Namespace:      "animind-test",
		SessionTTL:     time.Minute,
		MetadataTTL:    time.Hour,
		DialTimeout:    time.Second,
		CommandTimeout: 2 * time.Second,
		MaxConnections: 8,
	}
}

func testURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("REDIS_TEST_URL")
	if url == "" {
		t.Skip("set REDIS_TEST_URL to a disposable Redis database to run cache integration tests")
	}
	return url
}

// openRedis connects without touching existing keys, which is what modelling a second
// replica needs.
func openRedis(t *testing.T) Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	store, err := Open(ctx, redisConfig(testURL(t)), discardLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// newRedisStore connects to a namespace it has just emptied, so a run cannot inherit keys
// from the last one.
func newRedisStore(t *testing.T) Store {
	t.Helper()
	store := openRedis(t)

	cfg := redisConfig(testURL(t))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := redis.NewClient(&redis.Options{Addr: func() string {
		parsed, err := redis.ParseURL(cfg.URL)
		if err != nil {
			t.Fatalf("REDIS_TEST_URL: %v", err)
		}
		return parsed.Addr
	}()})
	defer client.Close()

	// Only this namespace: a shared Redis may hold another stack's keys.
	cursor := uint64(0)
	for {
		keys, next, err := client.Scan(ctx, cursor, cfg.Namespace+":*", 500).Result()
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if len(keys) > 0 {
			if err := client.Del(ctx, keys...).Err(); err != nil {
				t.Fatalf("del: %v", err)
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	return store
}

func TestRedisRoundTripAndTTL(t *testing.T) {
	store := newRedisStore(t)
	ctx := context.Background()

	if err := store.Set(ctx, "credential:web:hash1", []byte("principal"), 2*time.Second); err != nil {
		t.Fatalf("Set: %v", err)
	}
	value, found, err := store.Get(ctx, "credential:web:hash1")
	if err != nil || !found || string(value) != "principal" {
		t.Fatalf("Get = %q found=%v err=%v", value, found, err)
	}
	if _, found, err := store.Get(ctx, "credential:web:missing"); err != nil || found {
		t.Fatalf("a missing key must read as a miss: found=%v err=%v", found, err)
	}

	// Redis owns this expiry. A credential entry that outlived its TTL would outlive the
	// window the TTL exists to bound.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if _, found, _ := store.Get(ctx, "credential:web:hash1"); !found {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("the entry was still readable past its TTL")
}

func TestRedisGroupEvictionRemovesEveryMember(t *testing.T) {
	store := newRedisStore(t)
	ctx := context.Background()

	group := UserGroup("user-a")
	members := []string{"credential:web:a", "credential:desktop:b", "credential:web:c"}
	for _, key := range members {
		if err := store.Set(ctx, key, []byte(key), time.Minute, group); err != nil {
			t.Fatalf("Set %s: %v", key, err)
		}
	}
	if err := store.Set(ctx, "credential:web:d", []byte("d"), time.Minute, UserGroup("user-b")); err != nil {
		t.Fatalf("Set: %v", err)
	}

	if err := store.DeleteGroup(ctx, group); err != nil {
		t.Fatalf("DeleteGroup: %v", err)
	}
	for _, key := range members {
		if _, found, err := store.Get(ctx, key); err != nil || found {
			t.Fatalf("%s survived a group eviction: found=%v err=%v", key, found, err)
		}
	}
	if _, found, err := store.Get(ctx, "credential:web:d"); err != nil || !found {
		t.Fatalf("another account's entry was evicted: found=%v err=%v", found, err)
	}
	// The set itself must go, or a revoked account would accumulate members forever.
	if _, found, err := store.Get(ctx, "group:"+group); err != nil || found {
		t.Fatalf("the group set survived its own eviction: found=%v err=%v", found, err)
	}
}

func TestRedisWindowIsSharedBetweenProcesses(t *testing.T) {
	first := newRedisStore(t)
	// A second connection stands for a second API replica. Per-process counting would let
	// each admit three of six, so the deployment would silently serve double the limit.
	second := openRedis(t)
	ctx := context.Background()

	key := RateKey("rescan", "203.0.113.7")
	admitted := 0
	for attempt := 0; attempt < 6; attempt++ {
		store := first
		if attempt%2 == 1 {
			store = second
		}
		decision, err := store.Allow(ctx, key, 3, time.Minute)
		if err != nil {
			t.Fatalf("Allow: %v", err)
		}
		if decision.Allowed {
			admitted++
		}
	}
	if admitted != 3 {
		t.Fatalf("two processes sharing one Redis were admitted %d of 6, want the budget of 3", admitted)
	}

	remaining, err := ttlOf(first, key)
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	if remaining <= 0 || remaining > time.Minute+time.Second {
		t.Fatalf("window TTL = %v, want about a minute", remaining)
	}
}

func TestRedisWindowReopensAfterTheWindow(t *testing.T) {
	store := newRedisStore(t)
	ctx := context.Background()
	key := RateKey("webhook", "198.51.100.9")

	for attempt := 0; attempt < 2; attempt++ {
		decision, err := store.Allow(ctx, key, 1, 2*time.Second)
		if err != nil {
			t.Fatalf("Allow: %v", err)
		}
		if attempt == 0 && !decision.Allowed {
			t.Fatal("the first hit was denied")
		}
		if attempt == 1 && decision.Allowed {
			t.Fatal("the second hit inside the window was allowed")
		}
		if attempt == 1 && decision.RetryAfter <= 0 {
			t.Fatal("a denial must say when the window resets")
		}
	}
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if decision, err := store.Allow(ctx, key, 1, 2*time.Second); err == nil && decision.Allowed {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("the window never reopened")
}

// TestRedisScriptsAreAtomicUnderConcurrency pushes one window from many goroutines and
// expects exactly the budget to be admitted. Count-and-set without a script over-admits here.
func TestRedisScriptsAreAtomicUnderConcurrency(t *testing.T) {
	store := newRedisStore(t)
	ctx := context.Background()
	key := RateKey("login", "192.0.2.44")

	const limit = 25
	const attempts = 80
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted, failures := 0, 0
	for worker := 0; worker < attempts; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			decision, err := store.Allow(ctx, key, limit, time.Minute)
			if err != nil {
				mu.Lock()
				failures++
				mu.Unlock()
				return
			}
			if decision.Allowed {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if failures > 0 {
		t.Fatalf("%d concurrent Allow calls errored", failures)
	}
	if admitted != limit {
		t.Fatalf("concurrent counting admitted %d, want exactly %d", admitted, limit)
	}
}

func TestOpenAgainstADeadPortFailsFast(t *testing.T) {
	testURL(t) // the suite is opt-in on the same variable
	cfg := redisConfig("redis://127.0.0.1:1/9")
	cfg.DialTimeout = 500 * time.Millisecond
	cfg.CommandTimeout = 500 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	store, err := Open(ctx, cfg, discardLogger())
	if err == nil {
		_ = store.Close()
		t.Fatal("Open succeeded against a port nothing listens on")
	}
	if !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("Open error = %v, want a connection failure", err)
	}
}

func TestRedisStoreReportsItsBackendAndCounters(t *testing.T) {
	store := newRedisStore(t)
	ctx := context.Background()

	if err := store.Set(ctx, "anilist:doc", []byte("{}"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, _, err := store.Get(ctx, "anilist:doc"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	stats := store.Stats()
	if stats.Writes == 0 || stats.Hits == 0 {
		t.Fatalf("counters stayed empty after real traffic: %+v", stats)
	}
	if !store.Shared() || store.Backend() != "redis" {
		t.Fatalf("Shared=%v Backend=%q", store.Shared(), store.Backend())
	}
}

// ttlOf reads how long a namespaced key has left.
func ttlOf(store Store, key string) (time.Duration, error) {
	client, ok := store.(*redisStore)
	if !ok {
		return 0, errors.New("not a redis store")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return client.client.TTL(ctx, client.key(key)).Result()
}

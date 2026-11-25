package cache

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The cache module is tested against its in-process store. Redis-specific behaviour — the Lua
// window, group sets — is covered in redis_test.go against a real server.

// clock is a hand the tests can turn: cache correctness is about what has expired, and
// waiting for real seconds would make the suite slow and flaky.
type clock struct{ at time.Time }

func (c *clock) now() time.Time { return c.at }

func (c *clock) advance(by time.Duration) { c.at = c.at.Add(by) }

func newTestLocal(t *testing.T) (*localStore, *clock) {
	t.Helper()
	timer := &clock{at: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}
	store := newLocal()
	store.now = timer.now
	store.windows = NewWindowLimiter(timer.now)
	return store, timer
}

func TestLocalStoreExpiresEntries(t *testing.T) {
	store, clock := newTestLocal(t)
	ctx := context.Background()

	if err := store.Set(ctx, "credential:web:abc", []byte("hello"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	value, found, err := store.Get(ctx, "credential:web:abc")
	if err != nil || !found || string(value) != "hello" {
		t.Fatalf("Get inside the TTL = %q, found=%v, err=%v", value, found, err)
	}

	clock.advance(time.Minute + time.Second)
	if _, found, err := store.Get(ctx, "credential:web:abc"); err != nil || found {
		t.Fatalf("an expired entry must read as a miss: found=%v err=%v", found, err)
	}
	stats := store.Stats()
	if stats.Hits != 1 || stats.Misses != 1 {
		t.Fatalf("counters = hits %d misses %d, want 1 and 1", stats.Hits, stats.Misses)
	}
}

func TestLocalStoreGroupEvictionFindsKeysTheCallerHasNot(t *testing.T) {
	store, _ := newTestLocal(t)
	ctx := context.Background()

	// Two credentials for one account, plus one for another.
	if err := store.Set(ctx, "credential:web:one", []byte("1"), time.Minute, UserGroup("user-a")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.Set(ctx, "credential:desktop:two", []byte("2"), time.Minute, UserGroup("user-a")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.Set(ctx, "credential:web:three", []byte("3"), time.Minute, UserGroup("user-b")); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Revocation knows the account, not the token hash: this is the only handle it has.
	if err := store.DeleteGroup(ctx, UserGroup("user-a")); err != nil {
		t.Fatalf("DeleteGroup: %v", err)
	}

	for _, key := range []string{"credential:web:one", "credential:desktop:two"} {
		if _, found, err := store.Get(ctx, key); err != nil || found {
			t.Fatalf("%s survived a group delete: found=%v err=%v", key, found, err)
		}
	}
	if _, found, err := store.Get(ctx, "credential:web:three"); err != nil || !found {
		t.Fatalf("another account's entry was evicted: found=%v err=%v", found, err)
	}
}

func TestLocalStoreEntryCanBelongToSeveralGroups(t *testing.T) {
	store, _ := newTestLocal(t)
	ctx := context.Background()

	if err := store.Set(ctx, "credential:web:x", []byte("v"), time.Minute, UserGroup("user-a"), "episode:1"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.DeleteGroup(ctx, "episode:1"); err != nil {
		t.Fatalf("DeleteGroup: %v", err)
	}
	if _, found, err := store.Get(ctx, "credential:web:x"); err != nil || found {
		t.Fatalf("a second group did not own the entry: found=%v err=%v", found, err)
	}
}

func TestWindowLimiterCountsOneFixedWindow(t *testing.T) {
	clock := &clock{at: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}
	limiter := NewWindowLimiter(clock.now)
	ctx := context.Background()

	for hit := 1; hit <= 3; hit++ {
		decision, err := limiter.Allow(ctx, "rl:rescan:1.2.3.4", 3, time.Minute)
		if err != nil {
			t.Fatalf("Allow: %v", err)
		}
		if !decision.Allowed {
			t.Fatalf("hit %d denied inside the budget", hit)
		}
	}
	decision, err := limiter.Allow(ctx, "rl:rescan:1.2.3.4", 3, time.Minute)
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if decision.Allowed {
		t.Fatal("the fourth hit was allowed by a limit of three")
	}
	if decision.RetryAfter <= 0 || decision.RetryAfter > time.Minute {
		t.Fatalf("Retry-After = %v, want the rest of the window", decision.RetryAfter)
	}

	clock.advance(time.Minute + time.Second)
	if decision, err := limiter.Allow(ctx, "rl:rescan:1.2.3.4", 3, time.Minute); err != nil || !decision.Allowed {
		t.Fatalf("after the window the request should be allowed: allowed=%v err=%v", decision.Allowed, err)
	}
}

func TestWindowLimiterKeepsBucketsApart(t *testing.T) {
	limiter := NewWindowLimiter(nil)
	ctx := context.Background()

	for hit := 0; hit < 5; hit++ {
		if _, err := limiter.Allow(ctx, RateKey("login", "1.2.3.4"), 3, time.Minute); err != nil {
			t.Fatalf("Allow: %v", err)
		}
	}
	// Exhausting logins must not block playback.
	decision, err := limiter.Allow(ctx, RateKey("stream", "1.2.3.4"), 600, time.Minute)
	if err != nil || !decision.Allowed {
		t.Fatalf("a different bucket was affected: allowed=%v err=%v", decision.Allowed, err)
	}
	// Nor one client's budget for another's.
	if decision, err := limiter.Allow(ctx, RateKey("login", "5.6.7.8"), 3, time.Minute); err != nil || !decision.Allowed {
		t.Fatalf("another client shared the exhausted budget: allowed=%v err=%v", decision.Allowed, err)
	}
}

// unavailableStore answers nothing, which is how a Redis that stopped answering looks.
type unavailableStore struct{ err error }

func (u unavailableStore) Get(context.Context, string) ([]byte, bool, error) {
	return nil, false, u.err
}
func (u unavailableStore) Set(context.Context, string, []byte, time.Duration, ...string) error {
	return u.err
}
func (u unavailableStore) Delete(context.Context, ...string) error      { return u.err }
func (u unavailableStore) DeleteGroup(context.Context, ...string) error { return u.err }
func (u unavailableStore) Allow(context.Context, string, int, time.Duration) (Decision, error) {
	return Decision{}, u.err
}

type failingLimiter struct{ err error }

func (f failingLimiter) Allow(context.Context, string, int, time.Duration) (Decision, error) {
	return Decision{}, f.err
}

func TestResilientLimiterFallsBackOnlyWhenTheStoreIsUnreachable(t *testing.T) {
	local := NewWindowLimiter(nil)

	resilient := NewResilientLimiter(failingLimiter{err: ErrUnavailable}, local)
	for hit := 1; hit <= 2; hit++ {
		decision, err := resilient.Allow(context.Background(), "rl:login:9.9.9.9", 3, time.Minute)
		if err != nil {
			t.Fatalf("a dead store must not error the caller: %v", err)
		}
		if !decision.Allowed {
			t.Fatalf("hit %d denied while inside the local budget", hit)
		}
	}
	if resilient.Fallbacks() != 2 {
		t.Fatalf("Fallbacks = %d, want 2", resilient.Fallbacks())
	}

	// A cancelled request is the caller's own outcome, not an outage to work around.
	sealed := NewResilientLimiter(failingLimiter{err: context.Canceled}, local)
	if _, err := sealed.Allow(context.Background(), "rl:login:8.8.8.8", 3, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was swallowed: %v", err)
	}
}

func TestLocalStoreGetHandsOutACopy(t *testing.T) {
	// Redis returns a fresh slice per read; the in-process store must not be the one
	// implementation a caller can corrupt by writing into what it was given.
	store, _ := newTestLocal(t)
	ctx := context.Background()

	if err := store.Set(ctx, "anilist:doc", []byte(`{"data":{"id":1}}`), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	first, found, err := store.Get(ctx, "anilist:doc")
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	first[0] = 'X'

	second, _, err := store.Get(ctx, "anilist:doc")
	if err != nil {
		t.Fatalf("Get after mutation: %v", err)
	}
	if string(second) != `{"data":{"id":1}}` {
		t.Fatalf("a caller mutated the stored entry: %q", second)
	}
}

func TestAniListKeyIgnoresFormattingButNotContent(t *testing.T) {
	spaced := `{"query":"query { Media{id} }","variables":{"search":"Frieren"}}`
	compact := `{"query":"query { Media{id} }","variables":{"search":"Frieren"}}`
	if AniListKey(strings.ReplaceAll(compact, "  ", " ")) != AniListKey(spaced) {
		t.Fatal("whitespace changed the identity of the same document")
	}

	// Key order and whitespace inside JSON do not change what is being asked for.
	first := `{"query":"q","variables":{"a":1,"b":2}}`
	second := `{"variables": { "b": 2, "a": 1 }, "query":"q"}`
	if AniListKey(first) != AniListKey(second) {
		t.Fatalf("equivalent documents produced different keys:\n  %s\n  %s", AniListKey(first), AniListKey(second))
	}
	if AniListKey(first) == AniListKey(`{"query":"q","variables":{"a":1,"b":3}}`) {
		t.Fatal("a different variable produced the same key")
	}
	if !strings.HasPrefix(AniListKey(first), "anilist:") {
		t.Fatalf("key = %q, want the anilist scope", AniListKey(first))
	}
}

func TestCredentialKeySeparatesTokenFamilies(t *testing.T) {
	web := CredentialKey("web", "hash")
	desktop := CredentialKey("desktop", "hash")
	if web == desktop {
		t.Fatal("a desktop token could satisfy a web session lookup by key collision")
	}
	if !strings.HasPrefix(web, "credential:web:") {
		t.Fatalf("key = %q", web)
	}
}

type corruptStore struct{}

func (corruptStore) Get(context.Context, string) ([]byte, bool, error) {
	return []byte("{not json"), true, nil
}
func (corruptStore) Set(context.Context, string, []byte, time.Duration, ...string) error { return nil }
func (corruptStore) Delete(context.Context, ...string) error                             { return nil }
func (corruptStore) DeleteGroup(context.Context, ...string) error                        { return nil }

type recordingStore struct {
	corruptStore
	deleted []string
}

func (r *recordingStore) Delete(_ context.Context, keys ...string) error {
	r.deleted = append(r.deleted, keys...)
	return nil
}

func TestGetJSONTreatsAnUndecodableEntryAsAMiss(t *testing.T) {
	// A cache written by an older build must degrade to a database read, never to a 500.
	store := &recordingStore{}
	type payload struct {
		Name string `json:"name"`
	}
	value, found, err := GetJSON[payload](context.Background(), store, "credential:web:a")
	if err != nil || found {
		t.Fatalf("GetJSON = (%+v, found=%v, err=%v), want a miss with no error", value, found, err)
	}
	if len(store.deleted) != 1 || store.deleted[0] != "credential:web:a" {
		t.Fatalf("the unreadable entry was left in place: %v", store.deleted)
	}
}

func TestSetJSONSkipsANonPositiveTTL(t *testing.T) {
	store, _ := newTestLocal(t)
	ctx := context.Background()

	if err := SetJSON(ctx, store, "anilist:x", map[string]string{"a": "b"}, 0); err != nil {
		t.Fatalf("SetJSON: %v", err)
	}
	if _, found, err := store.Get(ctx, "anilist:x"); err != nil || found {
		t.Fatalf("a zero TTL must not store anything: found=%v err=%v", found, err)
	}
}

func TestSanitizeNamespaceCannotForgeASeparator(t *testing.T) {
	cases := map[string]string{
		"animind":         "animind",
		"stack-two":       "stack-two",
		`a":credential`:   "acredential",
		"":                "animind",
		"  spaced out  ":  "spacedout",
		"emoji🙂namespace": "emojinamespace",
	}
	for input, want := range cases {
		if got := sanitizeNamespace(input); got != want {
			t.Fatalf("sanitizeNamespace(%q) = %q, want %q", input, got, want)
		}
	}
}

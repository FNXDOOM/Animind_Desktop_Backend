// Package cache is the shared state layer: credentials, rate-limit counters and upstream
// metadata that outlives one process. Redis and the in-process store both satisfy Store.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"animind-backend-go/internal/config"
)

// ErrUnavailable marks a store that cannot be reached right now. Callers fall back to their
// authoritative path rather than fail the request: a cache outage costs latency, never data.
var ErrUnavailable = errors.New("cache: unavailable")

// KV is the entry half of the store; keys are namespaced by the implementation. A missing
// entry is found=false with a nil error; any other error means the store could not be asked.
type KV interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	// Set stores value for ttl. Groups name sets the entry belongs to, so a delete by group
	// can find entries whose keys no caller still holds.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration, groups ...string) error
	Delete(ctx context.Context, keys ...string) error
	DeleteGroup(ctx context.Context, groups ...string) error
}

// OneTimeStore atomically reads and removes an entry, for credentials that must not be
// replayed such as OAuth state. Callers must not emulate it with Get+Delete.
type OneTimeStore interface {
	Take(ctx context.Context, key string) ([]byte, bool, error)
}

// Take consumes one entry from a store that supports atomic one-time reads.
func Take(ctx context.Context, store KV, key string) ([]byte, bool, error) {
	oneTime, ok := store.(OneTimeStore)
	if !ok {
		return nil, false, errors.New("cache: store does not support atomic take")
	}
	return oneTime.Take(ctx, key)
}

// Decision is one fixed-window verdict.
type Decision struct {
	Allowed   bool
	Remaining int
	// RetryAfter is how long until the window resets; zero when allowed.
	RetryAfter time.Duration
}

// Limiter counts hits against a fixed window shared by every process using the same store.
type Limiter interface {
	Allow(ctx context.Context, key string, limit int, window time.Duration) (Decision, error)
}

// Store is everything the backend needs from shared state.
type Store interface {
	KV
	Limiter

	// Shared reports whether state crosses processes; rate limiting is only correct when it does.
	Shared() bool
	// Backend is the configured store name, for logs and metrics.
	Backend() string
	Stats() Snapshot
	Ping(ctx context.Context) error
	Close() error
}

// Snapshot is the observable outcome counters, read by the metrics endpoint.
type Snapshot struct {
	Hits, Misses, Writes, Evictions, Errors uint64
	Allowed, Denied, Fallbacks              uint64
}

// Stats counts what one response cannot show: whether a hit came from the store and how
// often it was unreachable. Tests assert on these; the metrics endpoint exports them.
type Stats struct {
	hits, misses, writes, evictions, errors atomic.Uint64
	allowed, denied, fallbacks              atomic.Uint64
}

func (s *Stats) record(op string) {
	switch op {
	case "hit":
		s.hits.Add(1)
	case "miss":
		s.misses.Add(1)
	case "set":
		s.writes.Add(1)
	case "delete":
		s.evictions.Add(1)
	case "error":
		s.errors.Add(1)
	case "allow":
		s.allowed.Add(1)
	case "deny":
		s.denied.Add(1)
	case "fallback":
		s.fallbacks.Add(1)
	}
}

func (s *Stats) snapshot() Snapshot {
	return Snapshot{
		Hits: s.hits.Load(), Misses: s.misses.Load(), Writes: s.writes.Load(),
		Evictions: s.evictions.Load(), Errors: s.errors.Load(),
		Allowed: s.allowed.Load(), Denied: s.denied.Load(),
		Fallbacks: s.fallbacks.Load(),
	}
}

// ── Key naming ───────────────────────────────────────────────────────────────

// Key scopes keep entries of different kinds apart in one namespace, so a credential
// lookup cannot collide with a metadata lookup.
const (
	scopeCredential = "credential"
	scopeRate       = "rl"
	scopeAniList    = "anilist"
	scopeMetadata   = "metadata"
	scopeGroup      = "group"
)

// CredentialKey is the cache entry for one resolved token; the hash never carries a secret.
func CredentialKey(kind, tokenHash string) string {
	return scopeCredential + ":" + kind + ":" + tokenHash
}

// RateKey is one client's window in one bucket. Buckets are per route group so exhausting
// logins cannot block playback.
func RateKey(bucket, client string) string {
	return scopeRate + ":" + bucket + ":" + client
}

// AniListKey hashes a request document: whitespace and key order must not split a cache
// entry, and a hash keeps keys a fixed size whatever the client sent.
func AniListKey(document string) string {
	sum := sha256.Sum256([]byte(normalizeJSON(document)))
	return scopeAniList + ":" + hex.EncodeToString(sum[:])
}

// MetadataKey is the shared half of the scanner's title cache.
func MetadataKey(titleKey string) string {
	return scopeMetadata + ":" + strings.ReplaceAll(strings.ToLower(titleKey), " ", "_")
}

// UserGroup is the invalidation set for everything derived from one account.
func UserGroup(userID string) string { return "user:" + userID }

// groupTTL outlives every entry written into a group, so losing a set costs only stale-key
// hygiene, never a missed eviction.
const groupTTL = 24 * time.Hour

// normalizeJSON makes an equivalent payload produce one key.
func normalizeJSON(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	var value any
	if err := json.Unmarshal([]byte(trimmed), &value); err != nil {
		// Not JSON: hash it as written rather than guessing.
		return trimmed
	}
	// json.Marshal sorts map keys, so key order stops mattering.
	canonical, err := json.Marshal(value)
	if err != nil {
		return trimmed
	}
	return string(canonical)
}

// ── JSON helpers ─────────────────────────────────────────────────────────────

// GetJSON reads and decodes one entry. A value that will not decode is a miss: a cache
// written by an older build must never become an error at request time.
func GetJSON[T any](ctx context.Context, kv KV, key string) (T, bool, error) {
	var zero T
	raw, found, err := kv.Get(ctx, key)
	if err != nil || !found {
		return zero, false, err
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		_ = kv.Delete(ctx, key)
		return zero, false, nil
	}
	return out, true, nil
}

// SetJSON encodes and stores one entry.
func SetJSON(ctx context.Context, kv KV, key string, value any, ttl time.Duration, groups ...string) error {
	if ttl <= 0 {
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("cache: encode %s: %w", key, err)
	}
	return kv.Set(ctx, key, encoded, ttl, groups...)
}

// ── Construction ─────────────────────────────────────────────────────────────

// Open connects to the configured store. Without REDIS_URL the in-process store is returned, so
// the same paths run locally. A URL that does not answer fails startup; a dead Redis degrades.
func Open(ctx context.Context, cfg config.CacheConfig, logger *slog.Logger) (Store, error) {
	if strings.TrimSpace(cfg.URL) == "" {
		logger.Info("cache: no REDIS_URL, keeping shared state in this process")
		return newLocal(), nil
	}

	client, err := newRedis(ctx, cfg, logger)
	if err != nil {
		return nil, err
	}
	logger.Info("cache: redis connected",
		slog.String("backend", "redis"),
		slog.String("database", redactedTarget(cfg.URL)),
		slog.String("namespace", cfg.Namespace))
	return client, nil
}

// redactedTarget keeps a password out of the log line while still naming the host.
func redactedTarget(url string) string {
	parsed, err := redis.ParseURL(url)
	if err != nil {
		return "unparseable"
	}
	return parsed.Addr
}

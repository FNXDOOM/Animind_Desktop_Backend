package cache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"animind-backend-go/internal/config"
)

// redisStore is the cross-process implementation. Every method bounds itself: this sits in
// front of authentication, so a Redis that stops answering must cost one short timeout and
// then be ignored, not stall the request.
type redisStore struct {
	client  *redis.Client
	ns      string
	stats   *Stats
	timeout time.Duration
	logger  *slog.Logger

	// windowScript atomically counts and sets the reset deadline. Two commands without this
	// can leave an INCR'ed key with no TTL after a crash between them, which is a counter
	// that never resets.
	windowScript *redis.Script
	// groupScript empties an invalidation set in one round trip.
	groupScript *redis.Script
	// takeScript consumes a one-time value atomically.
	takeScript *redis.Script
}

var _ Store = (*redisStore)(nil)

func newRedis(ctx context.Context, cfg config.CacheConfig, logger *slog.Logger) (*redisStore, error) {
	options, err := redis.ParseURL(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("cache: REDIS_URL: %w", err)
	}
	options.DialTimeout = cfg.DialTimeout
	options.ReadTimeout = cfg.CommandTimeout
	options.WriteTimeout = cfg.CommandTimeout
	options.PoolSize = cfg.MaxConnections
	options.MinIdleConns = 1
	// One retry, then give up and let the caller use its authoritative path.
	options.MaxRetries = 1
	options.MinRetryBackoff = 2 * time.Millisecond
	options.MaxRetryBackoff = 20 * time.Millisecond

	client := redis.NewClient(options)
	store := &redisStore{
		client:  client,
		ns:      sanitizeNamespace(cfg.Namespace),
		stats:   &Stats{},
		timeout: cfg.CommandTimeout,
		logger:  logger,
		// The script counts and expires in one atomic step; the ceiling is compared here because
		// a number Redis never needs to see does not have to cross the wire.
		windowScript: redis.NewScript(`
			local hits = redis.call('INCR', KEYS[1])
			local window = tonumber(ARGV[1])
			if hits == 1 then
				redis.call('PEXPIRE', KEYS[1], window)
			end
			local ttl = redis.call('PTTL', KEYS[1])
			if ttl < 0 then
				redis.call('PEXPIRE', KEYS[1], window)
				ttl = window
			end
			return {hits, ttl}
		`),
		groupScript: redis.NewScript(`
			local members = redis.call('SMEMBERS', KEYS[1])
			local removed = 0
			for i = 1, #members do
				redis.call('DEL', members[i])
				removed = removed + 1
			end
			redis.call('DEL', KEYS[1])
			return removed
		`),
		takeScript: redis.NewScript(`
			local value = redis.call('GET', KEYS[1])
			if value then
				redis.call('DEL', KEYS[1])
			end
			return value
		`),
	}

	pingCtx, cancel := context.WithTimeout(ctx, cfg.DialTimeout+cfg.CommandTimeout)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("cache: redis at %s did not answer: %w", options.Addr, err)
	}
	return store, nil
}

// sanitizeNamespace keeps a configurable prefix from becoming a key separator.
func sanitizeNamespace(raw string) string {
	trimmed := ""
	for _, r := range raw {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			trimmed += string(r)
		}
	}
	if trimmed == "" {
		return "animind"
	}
	return trimmed
}

func (r *redisStore) key(name string) string { return r.ns + ":" + name }

// timed gives an unbounded caller context a ceiling this store will not exceed.
func (r *redisStore) timed(ctx context.Context) (context.Context, context.CancelFunc) {
	if r.timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, r.timeout)
}

func (r *redisStore) Shared() bool    { return true }
func (r *redisStore) Backend() string { return "redis" }
func (r *redisStore) Stats() Snapshot { return r.stats.snapshot() }

func (r *redisStore) Get(ctx context.Context, name string) ([]byte, bool, error) {
	ctx, cancel := r.timed(ctx)
	defer cancel()

	value, err := r.client.Get(ctx, r.key(name)).Bytes()
	switch {
	case errors.Is(err, redis.Nil):
		r.stats.record("miss")
		return nil, false, nil
	case err != nil:
		r.stats.record("error")
		return nil, false, r.unavailable("get", err)
	}
	r.stats.record("hit")
	return value, true, nil
}

// Take atomically reads and removes a one-time entry.
func (r *redisStore) Take(ctx context.Context, name string) ([]byte, bool, error) {
	ctx, cancel := r.timed(ctx)
	defer cancel()

	raw, err := r.takeScript.Run(ctx, r.client, []string{r.key(name)}).Result()
	if errors.Is(err, redis.Nil) {
		r.stats.record("miss")
		return nil, false, nil
	}
	if err != nil {
		r.stats.record("error")
		return nil, false, r.unavailable("take", err)
	}
	if raw == nil {
		r.stats.record("miss")
		return nil, false, nil
	}
	var value []byte
	switch typed := raw.(type) {
	case string:
		value = []byte(typed)
	case []byte:
		value = typed
	default:
		r.stats.record("error")
		return nil, false, fmt.Errorf("cache: one-time value returned %T", raw)
	}
	r.stats.record("hit")
	r.stats.record("delete")
	return value, true, nil
}

// Set stores the entry and records it in each group so a group delete can find it later.
func (r *redisStore) Set(ctx context.Context, name string, value []byte, ttl time.Duration, groups ...string) error {
	if ttl <= 0 {
		return nil
	}
	ctx, cancel := r.timed(ctx)
	defer cancel()

	key := r.key(name)
	transaction := r.client.TxPipeline()
	transaction.Set(ctx, key, value, ttl)
	for _, group := range groups {
		if group == "" {
			continue
		}
		members := r.key(scopeGroup + ":" + group)
		transaction.SAdd(ctx, members, key)
		transaction.Expire(ctx, members, groupTTL)
	}
	if _, err := transaction.Exec(ctx); err != nil {
		r.stats.record("error")
		return r.unavailable("set", err)
	}
	r.stats.record("set")
	return nil
}

func (r *redisStore) Delete(ctx context.Context, names ...string) error {
	if len(names) == 0 {
		return nil
	}
	ctx, cancel := r.timed(ctx)
	defer cancel()

	keys := make([]string, 0, len(names))
	for _, name := range names {
		keys = append(keys, r.key(name))
	}
	if err := r.client.Del(ctx, keys...).Err(); err != nil {
		r.stats.record("error")
		return r.unavailable("delete", err)
	}
	r.stats.record("delete")
	return nil
}

// DeleteGroup removes every entry written under the given groups.
func (r *redisStore) DeleteGroup(ctx context.Context, groups ...string) error {
	if len(groups) == 0 {
		return nil
	}
	ctx, cancel := r.timed(ctx)
	defer cancel()

	for _, group := range groups {
		if group == "" {
			continue
		}
		if err := r.groupScript.Run(ctx, r.client, []string{r.key(scopeGroup + ":" + group)}).Err(); err != nil {
			r.stats.record("error")
			return r.unavailable("delete group", err)
		}
	}
	r.stats.record("delete")
	return nil
}

// Allow counts one hit in a fixed window shared by every process.
func (r *redisStore) Allow(ctx context.Context, name string, limit int, window time.Duration) (Decision, error) {
	ctx, cancel := r.timed(ctx)
	defer cancel()

	result, err := r.windowScript.Run(ctx, r.client, []string{r.key(name)}, window.Milliseconds()).Slice()
	if err != nil {
		r.stats.record("error")
		return Decision{}, r.unavailable("allow", err)
	}
	if len(result) != 2 {
		r.stats.record("error")
		return Decision{}, fmt.Errorf("cache: rate window returned %d fields", len(result))
	}
	hits, err := toInt64(result[0])
	if err != nil {
		r.stats.record("error")
		return Decision{}, err
	}
	ttlMillis, err := toInt64(result[1])
	if err != nil {
		r.stats.record("error")
		return Decision{}, err
	}

	decision := Decision{
		Allowed:    hits <= int64(limit),
		Remaining:  int(max(int64(limit)-hits, 0)),
		RetryAfter: time.Duration(max(ttlMillis, 0)) * time.Millisecond,
	}
	if decision.Allowed {
		r.stats.record("allow")
	} else {
		r.stats.record("deny")
	}
	return decision, nil
}

func (r *redisStore) Ping(ctx context.Context) error {
	ctx, cancel := r.timed(ctx)
	defer cancel()
	if err := r.client.Ping(ctx).Err(); err != nil {
		return r.unavailable("ping", err)
	}
	return nil
}

func (r *redisStore) Close() error { return r.client.Close() }

// unavailable wraps a Redis failure so callers can both detect it and keep going. The error
// text is logged here; the caller only needs to know the store did not answer.
func (r *redisStore) unavailable(op string, err error) error {
	// A cancelled caller is not an outage; reporting it as one would turn every client
	// disconnect into a warning line.
	if errors.Is(err, context.Canceled) {
		return err
	}
	r.logger.Warn("cache: redis "+op+" failed", slog.String("error", err.Error()))
	return fmt.Errorf("%w: %s: %v", ErrUnavailable, op, err)
}

func toInt64(value any) (int64, error) {
	switch v := value.(type) {
	case int64:
		return v, nil
	case int:
		return int64(v), nil
	}
	return 0, fmt.Errorf("cache: rate window returned %T", value)
}

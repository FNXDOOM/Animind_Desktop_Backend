package cache

import (
	"bytes"
	"context"
	"sort"
	"sync"
	"time"
)

// localStore keeps shared state in this process.
//
// It is what runs when no REDIS_URL is configured, so the caching code paths are exercised
// identically with or without Redis. Everything is bounded: an entry map that grows with the
// library would eventually cost more than the database it was meant to avoid.
type localStore struct {
	mu      sync.Mutex
	entries map[string]*localEntry
	groups  map[string]map[string]struct{}
	windows *WindowLimiter
	stats   *Stats
	now     func() time.Time
}

type localEntry struct {
	value     []byte
	expiresAt time.Time
}

// maxLocalEntries caps the in-process cache. Overflow drops the entries that expire soonest,
// which are the ones a fresh lookup would answer anyway.
const maxLocalEntries = 50_000

func newLocal() *localStore {
	return &localStore{
		entries: make(map[string]*localEntry),
		groups:  make(map[string]map[string]struct{}),
		windows: NewWindowLimiter(nil),
		stats:   &Stats{},
		now:     time.Now,
	}
}

var _ Store = (*localStore)(nil)

func (l *localStore) Shared() bool               { return false }
func (l *localStore) Backend() string            { return "local" }
func (l *localStore) Stats() Snapshot            { return l.stats.snapshot() }
func (l *localStore) Ping(context.Context) error { return nil }
func (l *localStore) Close() error               { return nil }

func (l *localStore) Allow(ctx context.Context, key string, limit int, duration time.Duration) (Decision, error) {
	decision, err := l.windows.Allow(ctx, key, limit, duration)
	if decision.Allowed {
		l.stats.record("allow")
	} else {
		l.stats.record("deny")
	}
	return decision, err
}

func (l *localStore) Get(_ context.Context, key string) ([]byte, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	stored, exists := l.entries[key]
	if !exists {
		l.stats.record("miss")
		return nil, false, nil
	}
	if l.now().After(stored.expiresAt) {
		l.removeLocked(key)
		l.stats.record("miss")
		return nil, false, nil
	}
	l.stats.record("hit")
	// Copy on read. Redis hands back a fresh slice per Get, so a caller that mutated what it
	// got would corrupt the in-process store and nothing else — the two implementations have to
	// differ in speed, not in who can damage the cache.
	return bytes.Clone(stored.value), true, nil
}

// Take reads and removes an entry while holding the same lock, making one-time
// credentials single-use even when callbacks arrive concurrently.
func (l *localStore) Take(_ context.Context, key string) ([]byte, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	stored, exists := l.entries[key]
	if !exists {
		l.stats.record("miss")
		return nil, false, nil
	}
	if l.now().After(stored.expiresAt) {
		l.removeLocked(key)
		l.stats.record("miss")
		return nil, false, nil
	}
	value := bytes.Clone(stored.value)
	l.removeLocked(key)
	l.stats.record("hit")
	l.stats.record("delete")
	return value, true, nil
}

func (l *localStore) Set(_ context.Context, key string, value []byte, ttl time.Duration, groups ...string) error {
	if ttl <= 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	l.entries[key] = &localEntry{value: value, expiresAt: l.now().Add(ttl)}
	for _, group := range groups {
		if group == "" {
			continue
		}
		members, exists := l.groups[group]
		if !exists {
			members = make(map[string]struct{})
			l.groups[group] = members
		}
		members[key] = struct{}{}
	}
	l.stats.record("set")
	if len(l.entries) > maxLocalEntries {
		l.evictLocked()
	}
	return nil
}

func (l *localStore) Delete(_ context.Context, keys ...string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, key := range keys {
		l.removeLocked(key)
	}
	l.stats.record("delete")
	return nil
}

func (l *localStore) DeleteGroup(_ context.Context, groups ...string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, group := range groups {
		for key := range l.groups[group] {
			delete(l.entries, key)
		}
		delete(l.groups, group)
	}
	l.stats.record("delete")
	return nil
}

func (l *localStore) removeLocked(key string) {
	delete(l.entries, key)
	for group, members := range l.groups {
		delete(members, key)
		if len(members) == 0 {
			delete(l.groups, group)
		}
	}
}

// evictLocked reclaims space by expiring what is already stale, then by dropping the entries
// closest to expiry. Only runs on overflow, so the sort is off the hot path.
func (l *localStore) evictLocked() {
	now := l.now()
	for key, entry := range l.entries {
		if now.After(entry.expiresAt) {
			l.removeLocked(key)
		}
	}
	if len(l.entries) <= maxLocalEntries {
		return
	}
	expiring := make([]string, 0, len(l.entries))
	for key := range l.entries {
		expiring = append(expiring, key)
	}
	sort.Slice(expiring, func(i, j int) bool {
		return l.entries[expiring[i]].expiresAt.Before(l.entries[expiring[j]].expiresAt)
	})
	for _, key := range expiring[:len(expiring)-maxLocalEntries/2] {
		l.removeLocked(key)
	}
}

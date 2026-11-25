package cache

import (
	"context"
	"sync"
	"time"
)

// WindowLimiter is the in-process fixed window: one counter per key, resetting a window after
// the first hit. It is both the no-Redis store and the fallback a shared store degrades to.
type WindowLimiter struct {
	mu      sync.Mutex
	windows map[string]*window
	now     func() time.Time
	// pruneAfter bounds memory as client keys rotate.
	pruneAfter time.Duration
	lastPrune  time.Time
}

type window struct {
	count   int
	resetAt time.Time
}

// maxEntries caps the map so a spoofed-header flood cannot turn the limiter into the outage.
const maxEntries = 100_000

func NewWindowLimiter(now func() time.Time) *WindowLimiter {
	if now == nil {
		now = time.Now
	}
	return &WindowLimiter{windows: make(map[string]*window), now: now, pruneAfter: 5 * time.Minute}
}

// Allow records one hit and reports whether it fits the window. Over-limit requests are
// counted too, so a hammering client keeps its window pinned at the ceiling.
func (l *WindowLimiter) Allow(_ context.Context, key string, limit int, duration time.Duration) (Decision, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.pruneLocked(now)

	w, exists := l.windows[key]
	if !exists || now.After(w.resetAt) {
		l.windows[key] = &window{count: 1, resetAt: now.Add(duration)}
		return Decision{Allowed: true, Remaining: max(limit-1, 0)}, nil
	}
	w.count++
	if w.count > limit {
		return Decision{Allowed: false, Remaining: 0, RetryAfter: w.resetAt.Sub(now)}, nil
	}
	return Decision{Allowed: true, Remaining: max(limit-w.count, 0)}, nil
}

func (l *WindowLimiter) pruneLocked(now time.Time) {
	if now.Sub(l.lastPrune) < l.pruneAfter && len(l.windows) < maxEntries {
		return
	}
	l.lastPrune = now
	for key, w := range l.windows {
		if now.After(w.resetAt) {
			delete(l.windows, key)
		}
	}
}

// Hits reports how many keys hold a live window. It has no callers.
func (l *WindowLimiter) Hits() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.windows)
}

package cache

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

// FallbackCounter is what an exporter reads to see how often a shared limiter had to answer
// locally.
type FallbackCounter interface {
	Fallbacks() uint64
}

// ResilientLimiter keeps rate limiting alive when the shared store cannot be reached.
//
// The trade is deliberate: a Redis outage degrades to per-process counting, which over-admits
// rather than locking every client out of an API that is otherwise healthy. Callers that got
// a verdict from the shared store are unaffected.
type ResilientLimiter struct {
	shared   Limiter
	local    Limiter
	fallback atomic.Uint64
}

// NewResilientLimiter wraps a shared limiter with an in-process one.
func NewResilientLimiter(shared, local Limiter) *ResilientLimiter {
	return &ResilientLimiter{shared: shared, local: local}
}

var _ Limiter = (*ResilientLimiter)(nil)

func (r *ResilientLimiter) Allow(ctx context.Context, key string, limit int, duration time.Duration) (Decision, error) {
	decision, err := r.shared.Allow(ctx, key, limit, duration)
	if err == nil {
		return decision, nil
	}
	// Only an unreachable store justifies local counting; a cancelled request is the
	// caller's own outcome and must not be answered with a verdict from a stale window.
	if !errors.Is(err, ErrUnavailable) {
		return Decision{}, err
	}
	r.fallback.Add(1)
	return r.local.Allow(ctx, key, limit, duration)
}

// Fallbacks counts verdicts taken from the in-process window instead of the shared one.
// A number that climbs while the API is healthy is a Redis worth looking at.
func (r *ResilientLimiter) Fallbacks() uint64 { return r.fallback.Load() }

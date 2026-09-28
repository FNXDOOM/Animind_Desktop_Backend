package httpx

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Fixed-window rate limiter keyed by client address.
//
// The limits mirror the original backend's per-route counters so an existing client's
// behaviour stays inside budget: 3 rescans a minute, 20 logins, 1200 HLS segments, and so
// on. Buckets are per route, so exhausting the login budget cannot block playback.
//
// State is process-local. With more than one instance behind a load balancer each
// instance enforces its own budget, which multiplies the effective limit; run the single
// instance this backend is designed for, or move counting to a shared store.

// RateLimiter keeps one fixed window per key.
type RateLimiter struct {
	mu       sync.Mutex
	windows  map[string]*window
	limit    int
	interval time.Duration
	now      func() time.Time
	// lastSweep prevents the map from growing without bound as keys rotate.
	lastSweep time.Time
}

type window struct {
	count   int
	resetAt time.Time
}

// NewRateLimiter returns a limiter allowing limit requests per interval.
func NewRateLimiter(limit int, interval time.Duration) *RateLimiter {
	return &RateLimiter{
		windows:  make(map[string]*window),
		limit:    limit,
		interval: interval,
		now:      time.Now,
	}
}

// Allow records a hit for key and reports whether the request may proceed. When denied it
// returns the seconds until the window resets.
func (l *RateLimiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.sweepLocked(now)

	w, exists := l.windows[key]
	if !exists || now.After(w.resetAt) {
		l.windows[key] = &window{count: 1, resetAt: now.Add(l.interval)}
		return true, 0
	}
	w.count++
	if w.count > l.limit {
		return false, time.Until(w.resetAt)
	}
	return true, 0
}

// sweepLocked drops expired windows periodically. Callers hold l.mu.
func (l *RateLimiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < 5*time.Minute {
		return
	}
	l.lastSweep = now
	for key, w := range l.windows {
		if now.After(w.resetAt) {
			delete(l.windows, key)
		}
	}
}

// Middleware enforces the limiter using the caller-supplied key function.
func (l *RateLimiter) Middleware(key func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if key == nil {
				next.ServeHTTP(w, r)
				return
			}
			ok, retryAfter := l.Allow(key(r))
			if !ok {
				seconds := int(mathCeilSeconds(retryAfter))
				w.Header().Set("Retry-After", strconv.Itoa(seconds))
				w.Header().Set("X-RateLimit-Limit", strconv.Itoa(l.limit))
				w.Header().Set("X-RateLimit-Remaining", "0")
				WriteErrorCode(w, http.StatusTooManyRequests, "rate_limited",
					"Too many requests. Please try again later.")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func mathCeilSeconds(d time.Duration) float64 {
	if d <= 0 {
		return 1
	}
	seconds := d.Seconds()
	whole := int64(seconds)
	if float64(whole) < seconds {
		whole++
	}
	if whole < 1 {
		return 1
	}
	return float64(whole)
}

// RateLimitPresets bundles the per-route limiters the API uses.
type RateLimitPresets struct {
	PublicRead   *RateLimiter
	StreamTicket *RateLimiter
	MediaStream  *RateLimiter
	HLSSession   *RateLimiter
	HLSSegment   *RateLimiter
	Rescan       *RateLimiter
	Webhook      *RateLimiter
	Signup       *RateLimiter
	Login        *RateLimiter
	Account      *RateLimiter
	Admin        *RateLimiter
	UserData     *RateLimiter
}

// NewRateLimitPresets builds every limiter with the same per-IP keying.
//
// trustProxy decides whether the forwarded header forms the key; it is passed in rather
// than read from the environment here so the wiring stays explicit in one place.
func NewRateLimitPresets(trustProxy bool) *RateLimitPresets {
	newOne := func(limit int, interval time.Duration) *RateLimiter {
		return NewRateLimiter(limit, interval)
	}
	return &RateLimitPresets{
		PublicRead:   newOne(120, time.Minute),
		StreamTicket: newOne(120, time.Minute),
		MediaStream:  newOne(600, time.Minute),
		HLSSession:   newOne(60, time.Minute),
		HLSSegment:   newOne(1200, time.Minute),
		Rescan:       newOne(3, time.Minute),
		Webhook:      newOne(30, time.Minute),
		Signup:       newOne(10, time.Minute),
		Login:        newOne(20, time.Minute),
		Account:      newOne(5, time.Minute),
		Admin:        newOne(60, time.Minute),
		UserData:     newOne(240, time.Minute),
	}
}

// Middleware wraps a limiter with per-IP keying.
func (l *RateLimiter) MiddlewareByIP(trustProxy bool) func(http.Handler) http.Handler {
	return l.Middleware(func(r *http.Request) string { return ClientIP(r, trustProxy) })
}

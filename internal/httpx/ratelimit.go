package httpx

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"animind-backend-go/internal/cache"
)

// Fixed-window rate limiting for the public API.
//
// The limits mirror the original backend's per-route counters so an existing client's
// behaviour stays inside budget: 3 rescans a minute, 20 logins, 1200 HLS segments. Buckets
// are per route, so exhausting the login budget cannot block playback.
//
// Counting lives behind cache.Limiter. With Redis that window is shared, which is what makes
// the limit real once more than one process answers traffic: a per-process map multiplies the
// effective budget by the replica count, and behind nginx the client cannot tell which
// instance it reached. Without Redis the same interface is served in-process.

// RateLimiter enforces one fixed window per key inside one named bucket.
type RateLimiter struct {
	name     string
	limit    int
	interval time.Duration
	window   cache.Limiter
}

// NewRateLimiter returns a limiter allowing limit requests per interval, counted by window.
func NewRateLimiter(name string, limit int, interval time.Duration, window cache.Limiter) *RateLimiter {
	return &RateLimiter{name: name, limit: limit, interval: interval, window: window}
}

// Allow records a hit for key and reports whether the request may proceed. When denied it
// returns the seconds until the window resets.
//
// A limiter that cannot count lets the request through. Rate limiting protects the service
// from load; failing it closed would turn a cache outage into an outage of the API.
func (l *RateLimiter) Allow(ctx context.Context, key string) (ok bool, retryAfter time.Duration) {
	decision, err := l.window.Allow(ctx, cache.RateKey(l.name, key), l.limit, l.interval)
	if err != nil {
		slog.Warn("rate limit window unavailable, allowing the request",
			slog.String("bucket", l.name), slog.String("error", err.Error()))
		return true, 0
	}
	return decision.Allowed, decision.RetryAfter
}

// Middleware enforces the limiter using the caller-supplied key function.
func (l *RateLimiter) Middleware(key func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if key == nil {
				next.ServeHTTP(w, r)
				return
			}
			client := key(r)
			ok, retryAfter := l.Allow(r.Context(), client)
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
	PublicRead     *RateLimiter
	StreamTicket   *RateLimiter
	MediaStream    *RateLimiter
	HLSSession     *RateLimiter
	HLSSegment     *RateLimiter
	Rescan         *RateLimiter
	Webhook        *RateLimiter
	Signup         *RateLimiter
	Login          *RateLimiter
	Account        *RateLimiter
	Admin          *RateLimiter
	UserData       *RateLimiter
	TorrentSearch  *RateLimiter
	DownloadCreate *RateLimiter
	DownloadRead   *RateLimiter
	DownloadWrite  *RateLimiter
	DownloadEvents *RateLimiter
	Trending       *RateLimiter

	// fallback reports local-window verdicts across every bucket, for the metrics endpoint.
	fallback cache.FallbackCounter
}

// NewRateLimitPresets builds every limiter on one shared counting window.
//
// The store decides whether counting crosses processes. When it does, a Redis that stops
// answering degrades to a local window instead of either stalling or admitting everyone:
// over-admitting during an outage is a worse outcome than a temporarily per-process budget,
// and failing closed would turn a cache outage into an outage of the API.
func NewRateLimitPresets(store cache.Store, logger *slog.Logger) *RateLimitPresets {
	window := cache.Limiter(store)
	if store != nil && store.Shared() {
		window = cache.NewResilientLimiter(store, cache.NewWindowLimiter(nil))
	}
	if window == nil {
		// Tests and embedders that pass no store still need counting, and the in-process
		// window is what the single-instance deployment used before Redis existed.
		window = cache.NewWindowLimiter(nil)
		logger.Debug("rate limiting counts inside this process")
	}

	newOne := func(name string, limit int, interval time.Duration) *RateLimiter {
		return NewRateLimiter(name, limit, interval, window)
	}
	presets := &RateLimitPresets{
		PublicRead:     newOne("public", 120, time.Minute),
		StreamTicket:   newOne("ticket", 120, time.Minute),
		MediaStream:    newOne("stream", 600, time.Minute),
		HLSSession:     newOne("hls-session", 60, time.Minute),
		HLSSegment:     newOne("hls-segment", 1200, time.Minute),
		Rescan:         newOne("rescan", 3, time.Minute),
		Webhook:        newOne("webhook", 30, time.Minute),
		Signup:         newOne("signup", 10, time.Minute),
		Login:          newOne("login", 20, time.Minute),
		Account:        newOne("account", 5, time.Minute),
		Admin:          newOne("admin", 60, time.Minute),
		UserData:       newOne("user-data", 240, time.Minute),
		TorrentSearch:  newOne("torrent-search", 30, time.Minute),
		DownloadCreate: newOne("download-create", 10, time.Minute),
		DownloadRead:   newOne("download-read", 120, time.Minute),
		DownloadWrite:  newOne("download-write", 60, time.Minute),
		DownloadEvents: newOne("download-events", 10, time.Minute),
		// Trending is a cheap in-memory read but gets its own 60/min bucket
		// rather than UserData's shared 240/min, so a poller cannot spend the
		// profile-write budget.
		Trending: newOne("trending", 60, time.Minute),
	}
	if resilient, ok := window.(*cache.ResilientLimiter); ok {
		presets.fallback = resilient
	}
	return presets
}

// Middleware wraps a limiter with per-client keying.
//
// trustProxy decides whether the forwarded header forms the key. It is passed in rather than
// read from the environment here so the wiring stays explicit in one place.
func (l *RateLimiter) MiddlewareByIP(trustProxy bool) func(http.Handler) http.Handler {
	return l.Middleware(func(r *http.Request) string { return ClientIP(r, trustProxy) })
}

package httpx

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"animind-backend-go/internal/auth"
)

// Middleware: request identity, CORS, authentication, and failure containment.

type principalKey struct{}

// WithPrincipal stores an authenticated caller on the context.
func WithPrincipal(ctx context.Context, p *auth.Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFromContext returns the authenticated caller, or nil.
func PrincipalFromContext(ctx context.Context) *auth.Principal {
	p, _ := ctx.Value(principalKey{}).(*auth.Principal)
	return p
}

// RequireAuth resolves the bearer credential and stops the request when it is missing or
// invalid. Responses deliberately echo the original backend's wording: the desktop player
// treats 401 as a hard stop with no retry, so the message is the only diagnostic a user
// ever sees.
func (s *Server) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := credential(r)
		if token == "" {
			WriteError(w, http.StatusUnauthorized, "Missing or invalid authorization token.")
			return
		}
		ctx := auth.WithClient(r.Context(), auth.Client{
			UserAgent: r.UserAgent(),
			IPAddress: ClientIP(r, s.cfg.TrustProxy),
		})
		principal, err := s.auth.Authenticate(ctx, token)
		if err != nil {
			WriteError(w, http.StatusUnauthorized, "Invalid or expired token.")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(ctx, principal)))
	})
}

// RequireAdmin must run after RequireAuth.
func (s *Server) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal := PrincipalFromContext(r.Context())
		if principal == nil {
			WriteError(w, http.StatusUnauthorized, "Missing or invalid authorization token.")
			return
		}
		if !principal.IsAdmin {
			WriteError(w, http.StatusForbidden, "Admin access required.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// CORS answers the site and the desktop player.
//
// The player sends no Origin at all on its Qt network calls, and a missing Origin must
// stay allowed, otherwise every desktop request would fail preflight-free but be rejected
// by an over-equal check here.
func (s *Server) CORS(next http.Handler) http.Handler {
	allowedHeaders := "Content-Type, Authorization, x-webhook-secret, Accept, Range"
	methods := "GET, POST, PATCH, PUT, DELETE, OPTIONS"

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && s.cfg.OriginAllowed(strings.TrimRight(origin, "/")) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Set("Access-Control-Allow-Methods", methods)
			h.Set("Access-Control-Allow-Headers", allowedHeaders)
			h.Set("Access-Control-Expose-Headers", "Content-Range, Accept-Ranges, Content-Length, Retry-After, X-Session-Expires-At")
			h.Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequestLogger records one line per request with enough identity to follow a playback
// problem from the media endpoint back to the ticket that authorised it.
func RequestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		slog.Info("request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Int("bytes", rec.bytes),
			slog.String("ip", r.RemoteAddr),
			slog.String("user_agent", truncateForLog(r.UserAgent(), 120)),
			slog.Duration("duration", time.Since(start)),
		)
	})
}

func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
	mu     sync.Mutex
}

func (r *statusRecorder) WriteHeader(status int) {
	r.mu.Lock()
	r.status = status
	r.mu.Unlock()
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	n, err := r.ResponseWriter.Write(p)
	r.mu.Lock()
	r.bytes += n
	r.mu.Unlock()
	return n, err
}

// Flush passes streaming through to the underlying writer, which the media and HLS
// handlers rely on.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Recover turns a panic into a 500 without leaking the stack to the client. A nil request
// or a closed client connection is not a defect, so those paths stay quiet.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				slog.Error("panic recovered",
					slog.Any("panic", v),
					slog.String("path", r.URL.Path),
					slog.String("stack", string(debug.Stack())))
				WriteError(w, http.StatusInternalServerError, "Internal server error.")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// MaxBody caps the request body size for JSON endpoints.
func MaxBody(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// securityHeadersForMedia relaxes no-store for cacheable media segments while keeping
// sniffing off.
func securityHeadersForMedia(w http.ResponseWriter, cacheable bool) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	if cacheable {
		h.Set("Cache-Control", "private, max-age=300")
	} else {
		h.Set("Cache-Control", "no-cache, no-store")
	}
}

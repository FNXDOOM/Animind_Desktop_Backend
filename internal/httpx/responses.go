// Package httpx is the HTTP edge: routing, middleware, and handlers. It owns every status
// code and JSON shape the native clients and the admin console parse.
package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

// ErrorPayload is the standard error body.
type ErrorPayload struct {
	Error string `json:"error"`
	// Details carries a developer-facing reason when one is safe to expose.
	Details string `json:"details,omitempty"`
	// Code is a stable identifier for clients that branch on failures.
	Code string `json:"code,omitempty"`
}

// WriteError emits a JSON error body.
func WriteError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, ErrorPayload{Error: message})
}

// WriteErrorCode emits a JSON error body with a stable code.
func WriteErrorCode(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorPayload{Error: message, Code: code})
}

// WriteErrorDetails emits an error plus a safe developer detail.
func WriteErrorDetails(w http.ResponseWriter, status int, message, details string) {
	writeJSON(w, status, ErrorPayload{Error: message, Details: details})
}

// WriteJSON serialises v with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) { writeJSON(w, status, v) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("failed to encode response", slog.String("error", err.Error()))
	}
}

// maxRequestBytes bounds every JSON body; media never travels through this endpoint.
const maxRequestBytes = 1 << 20

// DecodeJSON reads a request body with a hard size cap and rejects unknown fields, so a
// client typo such as "refesh_token" fails loudly.
func DecodeJSON(r *http.Request, dst any) error {
	defer func() {
		// Drain a small remainder so keep-alive connections stay reusable.
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<10))
	}()
	dec := json.NewDecoder(io.LimitReader(r.Body, maxRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("request body is required")
		}
		return err
	}
	return nil
}

// QueryInt reads an integer query parameter with clamping.
func QueryInt(r *http.Request, key string, fallback, min, max int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}

// Param returns a path segment, or "" when absent.
func Param(r *http.Request, key string) string { return chi.URLParam(r, key) }

// BearerToken extracts the credential from an Authorization header, accepting only the Bearer
// scheme both clients send.
func BearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if header == "" {
		return ""
	}
	scheme, value, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(value)
}

// ClientHint reports whether the caller is the native desktop player. The clientType query
// parameter wins because the player's media calls carry other user agents than its API calls.
func ClientHint(r *http.Request) string {
	switch value := strings.ToLower(r.URL.Query().Get("clientType")); value {
	case "native", "desktop":
		return "native"
	case "web", "browser":
		return "web"
	}
	if value := strings.ToLower(r.URL.Query().Get("client_type")); value == "desktop" || value == "native" {
		return "native"
	}
	ua := r.Header.Get("User-Agent")
	switch {
	case strings.Contains(ua, "Animind-Desktop"), strings.Contains(ua, "Animind-Qt"),
		strings.Contains(ua, "Electron"), strings.Contains(ua, "okhttp"),
		strings.Contains(ua, "Animind-Kotlin"):
		return "native"
	}
	return "web"
}

// IsNativeClient reports whether the caller is the desktop player.
func IsNativeClient(r *http.Request) bool { return ClientHint(r) == "native" }

// ClientIP resolves the requesting address for rate limiting. X-Forwarded-For is trusted only
// when the deployment declares a proxy, or a client could forge it and buy an unlimited bucket.
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
			first := strings.TrimSpace(strings.Split(xf, ",")[0])
			if net.ParseIP(first) != nil {
				return first
			}
		}
		if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); net.ParseIP(real) != nil {
			return real
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// SecureHeaders sets the headers a JSON API should always send; media responses override some.
func SecureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

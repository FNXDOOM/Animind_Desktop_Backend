package httpx

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestClientIPHandlesIPv6 pins the transport-address parsing. Cutting at the first colon
// returned "[2001" for an IPv6 peer, collapsing unrelated clients into one rate-limit key.
func TestClientIPHandlesIPv6(t *testing.T) {
	cases := []struct{ remote, want string }{
		{"192.0.2.10:5555", "192.0.2.10"},
		{"[2001:db8::1]:5555", "2001:db8::1"},
		{"[::1]:5555", "::1"},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = tc.remote
		if got := ClientIP(r, false); got != tc.want {
			t.Errorf("ClientIP(%q) = %q, want %q", tc.remote, got, tc.want)
		}
	}

	// Forwarded values are only trusted when they parse as an IP.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "192.0.2.10:5555"
	r.Header.Set("X-Forwarded-For", "not-an-ip")
	if got := ClientIP(r, true); got != "192.0.2.10" {
		t.Errorf("ClientIP with a bad forwarded value = %q, want the peer address", got)
	}
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	if got := ClientIP(r, true); got != "203.0.113.7" {
		t.Errorf("ClientIP forwarded = %q, want 203.0.113.7", got)
	}
}

// TestCORSWithWildcardNeverAllowsCredentials pins the browser boundary: a wildcard is a
// public API, never a credentialed one.
func TestCORSWithWildcardNeverAllowsCredentials(t *testing.T) {
	server := testServerWith(t, func(d *Deps) {
		d.Config.FrontendOrigins = []string{"*"}
	})
	rec := do(server, http.MethodGet, "/not-a-route", map[string]string{"Origin": "https://evil.example"})

	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got == "true" {
		t.Fatal("wildcard origin was combined with allow-credentials")
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" && got != "" {
		t.Fatalf("unexpected allow-origin %q", got)
	}
}

// TestSocketHandshakeIsRateLimited proves the socket route is behind the same per-route
// limiter as the REST surface. Before the fix, every handshake created a peer.
func TestSocketHandshakeIsRateLimited(t *testing.T) {
	server := socketServer(t)
	base := server.URL + "/api/socket.io?EIO=4&transport=polling"

	limited := false
	for i := 0; i < 140; i++ {
		resp, err := http.Get(base)
		if err != nil {
			t.Fatalf("handshake %d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("140 polling handshakes produced no 429; the socket route is unthrottled")
	}
}

// TestSessionCookieUsesHostPrefixWhenSecure pins the cookie hygiene: a Secure deployment
// uses the __Host- name, which no sibling subdomain can overwrite.
func TestSessionCookieUsesHostPrefixWhenSecure(t *testing.T) {
	server := testServerWith(t, func(d *Deps) {
		d.Config.Auth.SecureCookies = true
	})
	rec := httptest.NewRecorder()
	server.setSessionCookie(rec, "as_test", time.Now().Add(time.Hour), false)

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies, want 1", len(cookies))
	}
	if cookies[0].Name != SecureSessionCookieName {
		t.Fatalf("cookie name = %q, want %q", cookies[0].Name, SecureSessionCookieName)
	}
	if !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].Domain != "" || cookies[0].Path != "/" {
		t.Fatalf("cookie attributes are not host-only: %+v", cookies[0])
	}
}
func TestDebugErrorDetailsAreOptIn(t *testing.T) {
	if safeDetail(io.ErrUnexpectedEOF, false) != "" {
		t.Fatal("safeDetail exposed an error without DEBUG_ERRORS")
	}
	if got := safeDetail(io.ErrUnexpectedEOF, true); !strings.Contains(got, "unexpected EOF") {
		t.Fatalf("safeDetail with DEBUG_ERRORS = %q", got)
	}
}

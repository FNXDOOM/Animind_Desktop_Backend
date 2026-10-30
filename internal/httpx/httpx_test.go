package httpx

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"animind-backend-go/internal/config"
)

// testServer builds a server with no database or media services. Route registration and the
// middleware chain do not need them, so this exercises the HTTP surface: status codes,
// headers, error shape, and the fact that protected routes refuse unauthenticated callers
// before touching any dependency.
func testServer(t *testing.T) *Server {
	t.Helper()
	cfg := testConfig(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	server := New(Deps{Config: cfg, Logger: logger})
	server.httpServer = &http.Server{}
	return server
}

// testConfig loads the smallest configuration every HTTP test needs. Callers that want a service
// (auth, storage) still construct only what their routes touch.
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/db")
	t.Setenv("DESKTOP_TOKEN_SIGNING_KEY", "0123456789012345678901234567890123456789")
	t.Setenv("STORAGE_MODE", "local")
	t.Setenv("LOCAL_STORAGE_PATH", t.TempDir())
	t.Setenv("FRONTEND_URL", "https://fnxdoom.in,http://localhost:5173")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Request logging goes through the default logger; silencing it keeps test output readable.
	slog.SetDefault(logger)
	return cfg
}

func do(server *Server, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, nil)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	server.Router().ServeHTTP(recorder, request)
	return recorder
}

// TestRoutesAreRegistered checks the full inventory both clients depend on, so a typo in a
// path is a test failure rather than a broken player.
//
// Each route is probed with an empty request. A registered path answers 401, 400 or 500
// (this build has no database, so handlers that reach it fail); an unregistered path answers
// 404. Only the 404 is evidence of a missing route.
func TestRoutesAreRegistered(t *testing.T) {
	server := testServer(t)
	router := server.Router()

	const uuid = "00000000-0000-0000-0000-000000000000"
	const session = "00000000000000000000000000000000"
	required := map[string]string{
		http.MethodGet: `/health /api/health /api/shows /api/shows/` + uuid +
			` /api/episodes/` + uuid + `/stream-ticket /api/episodes/` + uuid + `/stream ` +
			`/api/episodes/` + uuid + `/subtitles /api/episodes/` + uuid + `/audio-tracks ` +
			`/api/hls/` + session + `/playlist.m3u8 /api/hls/` + session + `/seg00000.ts ` +
			`/api/auth/me /api/auth/sessions /api/auth/google/url /api/auth/google/callback ` +
			`/api/me/watchlist /api/me/progress /api/me/progress/recent /api/me/history /api/me/preferences ` +
			`/api/admin/users /api/admin/metrics /api/syncplay/rooms/ABCDE`,
		http.MethodPost: `/api/anilist /api/webhooks/storage /api/rescan /api/admin/scan /api/admin/library/cleanup ` +
			`/api/episodes/` + uuid + `/hls-session /api/hls/` + session + `/seek ` +
			`/api/hls/` + session + `/heartbeat ` +
			`/api/auth/signup /api/auth/login /api/auth/logout /api/auth/consume-code ` +
			`/api/auth/password/change /api/auth/password/forgot /api/auth/password/reset ` +
			`/api/auth/desktop/exchange /api/auth/desktop/refresh /api/auth/desktop/revoke /api/auth/desktop/bridge-token`,
		http.MethodPut:    `/api/me/watchlist/1 /api/me/progress /api/me/history /api/me/preferences`,
		http.MethodPatch:  `/api/me /api/me/watchlist/1 /api/admin/users/` + uuid,
		http.MethodDelete: `/api/me /api/me/watchlist/1 /api/hls/` + session + ` /api/admin/shows/` + uuid + ` /api/auth/sessions/` + uuid,
	}

	for method, list := range required {
		for _, path := range strings.Fields(list) {
			request := httptest.NewRequest(method, path, nil)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code == http.StatusNotFound {
				t.Errorf("%s %s is not registered (404)", method, path)
			}
			if recorder.Code == http.StatusMethodNotAllowed {
				t.Errorf("%s %s answered 405, so the method is not wired", method, path)
			}
		}
	}
}

// TestProtectedRoutesRejectAnonymous covers every route that must not answer without a
// credential, which is the single most important security property of this API.
func TestProtectedRoutesRejectAnonymous(t *testing.T) {
	server := testServer(t)

	cases := []struct {
		method, path string
		body         bool
	}{
		{http.MethodGet, "/api/me/watchlist", false},
		{http.MethodPut, "/api/me/watchlist/123", true},
		{http.MethodPatch, "/api/me/watchlist/123", true},
		{http.MethodDelete, "/api/me/watchlist/123", false},
		{http.MethodGet, "/api/me/progress?anime_id=1&episode_index=0", false},
		{http.MethodPut, "/api/me/progress", true},
		{http.MethodGet, "/api/me/history", false},
		{http.MethodPut, "/api/me/history", true},
		{http.MethodGet, "/api/me/preferences", false},
		{http.MethodPut, "/api/me/preferences", true},
		{http.MethodPatch, "/api/me", true},
		{http.MethodDelete, "/api/me", false},
		{http.MethodGet, "/api/auth/me", false},
		{http.MethodGet, "/api/auth/sessions", false},
		{http.MethodPost, "/api/auth/desktop/bridge-token", false},
		{http.MethodPost, "/api/auth/password/change", true},
		{http.MethodGet, "/api/episodes/00000000-0000-0000-0000-000000000000/stream-ticket", false},
		{http.MethodGet, "/api/episodes/00000000-0000-0000-0000-000000000000/subtitles", false},
		{http.MethodGet, "/api/episodes/00000000-0000-0000-0000-000000000000/audio-tracks", false},
		{http.MethodPost, "/api/episodes/00000000-0000-0000-0000-000000000000/hls-session", true},
		{http.MethodPost, "/api/hls/00000000000000000000000000000000/seek", true},
		{http.MethodPost, "/api/hls/00000000000000000000000000000000/heartbeat", false},
		{http.MethodDelete, "/api/hls/00000000000000000000000000000000", false},
		{http.MethodPost, "/api/rescan", false},
		{http.MethodGet, "/api/admin/users", false},
		{http.MethodPost, "/api/admin/scan", false},
		{http.MethodGet, "/api/admin/metrics", false},
		{http.MethodGet, "/api/syncplay/rooms/ABCDE", false},
	}

	for _, tc := range cases {
		var recorder *httptest.ResponseRecorder
		if tc.body {
			recorder = requestJSON(t, server, tc.method, tc.path, map[string]any{})
		} else {
			recorder = do(server, tc.method, tc.path, nil)
		}
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401 (body %q)", tc.method, tc.path, recorder.Code, recorder.Body.String())
			continue
		}
		assertErrorShape(t, recorder.Body.Bytes())
	}
}

// requestJSON sends a body with an explicit method, so a protected PUT is not mistaken for a
// 405 on the way to being tested.
func requestJSON(t *testing.T, server *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, path, strings.NewReader(string(encoded)))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Router().ServeHTTP(recorder, request)
	return recorder
}

// TestPublicRoutesRemainOpen documents the deliberate choice to serve the catalog without a
// session: both clients browse before sign-in.
func TestPublicRoutesRemainOpen(t *testing.T) {
	server := testServer(t)

	// The AniList proxy validates its body before any network call.
	recorder := requestJSON(t, server, http.MethodPost, "/api/anilist", map[string]any{"query": "mutation { junk }"})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("mutation proxy = %d, want 400", recorder.Code)
	}

	recorder = do(server, http.MethodPost, "/api/webhooks/storage", map[string]string{"x-webhook-secret": "wrong"})
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("webhook with a wrong secret = %d, want 401", recorder.Code)
	}

	// An unguessable HLS session id must not be treated as valid.
	recorder = do(server, http.MethodGet, "/api/hls/deadbeefdeadbeefdeadbeefdeadbeef/playlist.m3u8", nil)
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("HLS playlist without a session manager = %d, want 400", recorder.Code)
	}
}

func TestUnknownRouteAndMethod(t *testing.T) {
	server := testServer(t)

	recorder := do(server, http.MethodGet, "/api/does-not-exist", nil)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown path = %d, want 404", recorder.Code)
	}
	assertErrorShape(t, recorder.Body.Bytes())

	recorder = do(server, http.MethodPut, "/api/shows", nil)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Errorf("wrong method = %d, want 405", recorder.Code)
	}
}

func TestCORSPreflightAndAllowedOrigin(t *testing.T) {
	server := testServer(t)

	recorder := do(server, http.MethodOptions, "/api/me/watchlist", map[string]string{
		"Origin": "https://fnxdoom.in", "Access-Control-Request-Method": http.MethodPut,
	})
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("preflight = %d, want 204", recorder.Code)
	}
	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "https://fnxdoom.in" {
		t.Errorf("allow-origin = %q", got)
	}
	if !strings.Contains(recorder.Header().Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Error("preflight must allow the Authorization header the clients send")
	}

	recorder = do(server, http.MethodOptions, "/api/me/watchlist", map[string]string{
		"Origin": "https://evil.example", "Access-Control-Request-Method": http.MethodPut,
	})
	if recorder.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("an origin outside FRONTEND_URL must not receive CORS headers")
	}
}

func TestSecurityHeadersAreSet(t *testing.T) {
	server := testServer(t)
	recorder := do(server, http.MethodGet, "/api/does-not-exist", nil)

	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := recorder.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

func TestRateLimitReturns429WithRetryAfter(t *testing.T) {
	server := testServer(t)
	// The rescan limiter allows three requests a minute; the fourth must be refused before
	// any scan work happens. Anonymous requests still consume budget, which is the point: the
	// limiter runs ahead of authentication so a credential-stuffing loop cannot flood the
	// auth path.
	for attempt := 1; attempt <= 3; attempt++ {
		recorder := do(server, http.MethodPost, "/api/rescan", nil)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401 while unauthenticated", attempt, recorder.Code)
		}
	}
	recorder := do(server, http.MethodPost, "/api/rescan", nil)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("fourth attempt = %d, want 429", recorder.Code)
	}
	if recorder.Header().Get("Retry-After") == "" {
		t.Error("a 429 must tell the client when to retry")
	}
	assertErrorShape(t, recorder.Body.Bytes())
}

func TestRateLimitersAreIndependentPerRoute(t *testing.T) {
	server := testServer(t)
	// Exhausting the rescan budget must not lock the catalog out of the login budget.
	for attempt := 0; attempt < 4; attempt++ {
		do(server, http.MethodPost, "/api/rescan", nil)
	}
	recorder := do(server, http.MethodGet, "/api/admin/users", nil)
	if recorder.Code == http.StatusTooManyRequests {
		t.Error("route limiters are shared when they should be independent")
	}
}

func TestParseRange(t *testing.T) {
	const size = 1000
	cases := []struct {
		header    string
		chunk     int64
		wantStart int64
		wantEnd   int64
		wantError bool
	}{
		{header: "bytes=0-499", wantStart: 0, wantEnd: 499},
		{header: "bytes=500-", wantStart: 500, wantEnd: 999},
		{header: "bytes=-200", wantStart: 800, wantEnd: 999},
		{header: "bytes=0-", chunk: 100, wantStart: 0, wantEnd: 99},
		{header: "bytes=900-", chunk: 100, wantStart: 900, wantEnd: 999},
		{header: "bytes=0-1999", wantStart: 0, wantEnd: 999},
		{header: "chars=0-10", wantError: true},
		{header: "bytes=0-10,20-30", wantError: true},
		{header: "bytes=1500-", wantError: true},
		{header: "bytes=abc-def", wantError: true},
		{header: "bytes=30-10", wantError: true},
	}

	for _, tc := range cases {
		start, end, err := parseRange(tc.header, size, tc.chunk)
		if tc.wantError {
			if err == nil {
				t.Errorf("parseRange(%q) accepted an invalid header (%d-%d)", tc.header, start, end)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseRange(%q) = %v, want %d-%d", tc.header, err, tc.wantStart, tc.wantEnd)
			continue
		}
		if start != tc.wantStart || end != tc.wantEnd {
			t.Errorf("parseRange(%q) = %d-%d, want %d-%d", tc.header, start, end, tc.wantStart, tc.wantEnd)
		}
	}
}

func TestServeLocalFileHonoursRange(t *testing.T) {
	payload := strings.Repeat("A", 100) + strings.Repeat("B", 100)

	request := httptest.NewRequest(http.MethodGet, "/api/episodes/x/stream", nil)
	request.Header.Set("Range", "bytes=100-149")
	recorder := httptest.NewRecorder()
	serveLocalFile(recorder, request, strings.NewReader(payload), int64(len(payload)), "video/mp4", 1<<20, false)

	if recorder.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", recorder.Code)
	}
	if got := recorder.Body.String(); got != strings.Repeat("B", 50) {
		t.Errorf("body length = %d, want 50", len(got))
	}
	if got := recorder.Header().Get("Content-Range"); got != "bytes 100-149/200" {
		t.Errorf("content-range = %q", got)
	}
	if got := recorder.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("accept-ranges = %q, want bytes", got)
	}
}

func TestServeLocalFileRejectsImpossibleRange(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/episodes/x/stream", nil)
	request.Header.Set("Range", "bytes=5000-6000")
	recorder := httptest.NewRecorder()
	serveLocalFile(recorder, request, strings.NewReader("data"), 4, "video/mp4", 1<<20, false)

	if recorder.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want 416", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Range"); got != "bytes */4" {
		t.Errorf("content-range = %q, want the total size", got)
	}
}

func TestClientTypeDetection(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		userAgent string
		want      string
	}{
		{"query override", "/x?clientType=desktop", "Mozilla/5.0", "native"},
		{"qt user agent", "/x", "Animind-Qt/1.0", "native"},
		{"okhttp user agent", "/x", "okhttp/4.9", "native"},
		{"browser", "/x", "Mozilla/5.0", "web"},
	}
	for _, tc := range cases {
		request := httptest.NewRequest(http.MethodGet, tc.path, nil)
		request.Header.Set("User-Agent", tc.userAgent)
		if got := ClientHint(request); got != tc.want {
			t.Errorf("%s: ClientHint = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestBearerTokenParsing(t *testing.T) {
	cases := map[string]string{
		"Bearer abc":                 "abc",
		"bearer abc":                 "abc",
		"Bearer   abc ":              "abc",
		"Token abc":                  "",
		"":                           "",
		"Bearer":                     "",
		"Basic dXNlcjpwYXNzd29yZA==": "",
	}
	for header, want := range cases {
		request := httptest.NewRequest(http.MethodGet, "/x", nil)
		if header != "" {
			request.Header.Set("Authorization", header)
		}
		if got := BearerToken(request); got != want {
			t.Errorf("Authorization %q -> %q, want %q", header, got, want)
		}
	}
}

func TestSanitizeReturnTargetRejectsForeignHosts(t *testing.T) {
	allowed := []string{"https://fnxdoom.in", "http://localhost:5173"}
	if got := sanitizeReturnTarget("https://fnxdoom.in/watch", allowed); got == "" {
		t.Error("a configured origin must be accepted")
	}
	for _, target := range []string{
		"https://evil.example/redirect", "javascript:alert(1)", "//evil.example", "",
		"https://fnxdoom.in.evil.example/x",
	} {
		if got := sanitizeReturnTarget(target, allowed); got != "" {
			t.Errorf("sanitizeReturnTarget(%q) = %q, must be rejected", target, got)
		}
	}
}

func TestIsReadOnlyGraphQL(t *testing.T) {
	if !isReadOnlyGraphQL("query { Media(id:1) { title } }") {
		t.Error("a plain query must be allowed")
	}
	if isReadOnlyGraphQL("mutation { SaveMedia }") {
		t.Error("mutations must be refused")
	}
	if isReadOnlyGraphQL("query { x } # then mutation { y }") {
		t.Error("a mutation hidden in a comment must be refused")
	}
	if isReadOnlyGraphQL("subscription { Live }") {
		t.Error("subscriptions must be refused")
	}
}

// assertErrorShape checks the contract every client parses: {"error": "..."}.
func assertErrorShape(t *testing.T, body []byte) {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("error body is not JSON: %q", string(body))
	}
	value, ok := payload["error"].(string)
	if !ok || strings.TrimSpace(value) == "" {
		t.Errorf("error body must carry a non-empty \"error\" string, got %v", payload)
	}
}

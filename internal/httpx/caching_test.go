package httpx

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"animind-backend-go/internal/cache"
	"animind-backend-go/internal/config"
)

// Shared-state behaviour: what one store does for more than one server, what it exposes, and
// what the AniList relay stops doing once it has answered a document.

func loadTestConfig(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/db")
	t.Setenv("DESKTOP_TOKEN_SIGNING_KEY", "0123456789012345678901234567890123456789")
	t.Setenv("STORAGE_MODE", "local")
	t.Setenv("LOCAL_STORAGE_PATH", t.TempDir())
	t.Setenv("FRONTEND_URL", "http://localhost:5173")
	t.Setenv("METRICS_ENABLED", "true")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

func newStore(t *testing.T, cfg *config.Config) cache.Store {
	t.Helper()
	store, err := cache.Open(context.Background(), cfg.Cache, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestRateLimitBudgetIsSharedAcrossServers is the reason counting moved out of the server:
// two instances behind one proxy each enforcing their own window would serve twice the limit,
// and the client has no way to tell which instance it reached.
func TestRateLimitBudgetIsSharedAcrossServers(t *testing.T) {
	cfg := loadTestConfig(t)
	store := newStore(t, cfg)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	first := New(Deps{Config: cfg, Cache: store, Logger: logger})
	second := New(Deps{Config: cfg, Cache: store, Logger: logger})

	// The rescan budget is three a minute. Auth fails on every attempt, which is irrelevant:
	// the limiter runs first, so a 401 is a counted hit and a 429 is the budget closing.
	servers := []*Server{first, second}
	for attempt := 0; attempt < 3; attempt++ {
		recorder := do(servers[attempt%2], http.MethodPost, "/api/rescan", nil)
		if recorder.Code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d was rate limited while the budget was still open", attempt+1)
		}
	}
	recorder := do(servers[1], http.MethodPost, "/api/rescan", nil)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("the fourth hit crossed two servers and was %d, want 429: the window is per-server", recorder.Code)
	}

	// A different bucket stays independent even though both share the store.
	if other := do(second, http.MethodGet, "/api/admin/users", nil); other.Code == http.StatusTooManyRequests {
		t.Fatal("exhausting rescan blocked the admin bucket")
	}
}

// TestMetricsEndpointExportsRequestsAndCacheOutcomes proves the scrape target Grafana reads
// has content: an empty registry is a dashboard of "no data".
func TestMetricsEndpointExportsRequestsAndCacheOutcomes(t *testing.T) {
	cfg := loadTestConfig(t)
	store := newStore(t, cfg)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := New(Deps{Config: cfg, Cache: store, Logger: logger})

	for attempt := 0; attempt < 4; attempt++ {
		do(server, http.MethodPost, "/api/rescan", nil)
	}
	// A parameterised path: the label must be the route pattern, not whatever the client
	// asked for, or a path scan would mint unbounded series.
	do(server, http.MethodGet, "/api/hls/00000000000000000000000000000000/seg00000.ts", nil)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	server.metricsHandler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("metrics endpoint = %d, want 200", recorder.Code)
	}
	body := recorder.Body.String()
	for _, expected := range []string{
		`animind_http_requests_total{method="POST",route="/api/rescan",status="429"}`,
		`animind_http_requests_total{method="POST",route="/api/rescan",status="401"}`,
		`animind_http_request_duration_seconds`,
		`route="/api/hls/{sessionId}/{segment}"`,
		`animind_cache_operations_total{backend="local",result="deny"}`,
		`animind_cache_operations_total{backend="local",result="allow"}`,
		`animind_cache_shared`,
		`go_goroutines`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metrics are missing %q\n---\n%s", expected, snippet(body))
		}
	}
	if strings.Contains(body, `route="/api/hls/00000000000000000000000000000000/seg00000.ts"`) {
		t.Fatal("route labels carry raw paths instead of patterns")
	}
}

func TestMetricsDisabledServesNothing(t *testing.T) {
	cfg := loadTestConfig(t)
	cfg.Metrics.Enabled = false
	server := New(Deps{Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if server.metrics != nil {
		t.Fatal("metrics were built while disabled")
	}
	recorder := httptest.NewRecorder()
	server.metricsHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("a disabled metrics listener answered %d, want 404", recorder.Code)
	}
}

// TestAniListProxyServesADocumentOnce is the relay's whole point: the site re-issues the same
// search as a user browses, and each one used to leave the process.
func TestAniListProxyServesADocumentOnce(t *testing.T) {
	var upstream atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream.Add(1)
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "Page") {
			http.Error(w, `{"errors":[{"message":"unexpected document"}]}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"Media":{"id":1,"title":{"romaji":"Frieren"}}}}`))
	}))
	defer server.Close()

	cfg := loadTestConfig(t)
	cfg.Metadata.AniListEndpoint = server.URL
	store := newStore(t, cfg)
	api := New(Deps{Config: cfg, Cache: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})

	document := mustBody(t, map[string]any{
		"query":     "query($s:String){Page(perPage:1){media(type:ANIME,search:$s){id title{romaji}}}}",
		"variables": map[string]any{"s": "Frieren"},
	})

	first := doRaw(api, http.MethodPost, "/api/anilist", document, "application/json")
	if first.Code != http.StatusOK {
		t.Fatalf("first proxied call = %d: %s", first.Code, first.Body.String())
	}
	if got := first.Header().Get("X-Cache"); got != "MISS" {
		t.Fatalf("first response X-Cache = %q, want MISS", got)
	}

	second := doRaw(api, http.MethodPost, "/api/anilist", document, "application/json")
	if got := second.Header().Get("X-Cache"); got != "HIT" {
		t.Fatalf("second response X-Cache = %q, want HIT (upstream calls: %d)", got, upstream.Load())
	}
	if calls := upstream.Load(); calls != 1 {
		t.Fatalf("the upstream was called %d times for one document", calls)
	}
	if !strings.Contains(second.Body.String(), "Frieren") {
		t.Fatalf("the cached document came back altered: %s", second.Body.String())
	}

	// A different variable is a different document, and must still reach the upstream.
	other := mustBody(t, map[string]any{
		"query":     "query($s:String){Page(perPage:1){media(type:ANIME,search:$s){id title{romaji}}}}",
		"variables": map[string]any{"s": "Bocchi"},
	})
	if resp := doRaw(api, http.MethodPost, "/api/anilist", other, "application/json"); resp.Header().Get("X-Cache") != "MISS" {
		t.Fatal("a different search reused the first document from cache")
	}
	if calls := upstream.Load(); calls != 2 {
		t.Fatalf("upstream calls = %d, want one per distinct document", calls)
	}
}

// TestAniListProxyDoesNotCacheABadResponse keeps a provider hiccup from becoming a half-day
// of empty results. AniList answers some rejections with 200 and an errors array, and a
// truncated body is not a document at all.
func TestAniListProxyDoesNotCacheABadResponse(t *testing.T) {
	var upstream atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch upstream.Load() {
		case 1:
			_, _ = w.Write([]byte(`{"errors":[{"message":"rate limited"}]}`))
		case 2:
			// Valid JSON, no data: a search that matched nothing on a broken document.
			_, _ = w.Write([]byte(`{"data":null,"errors":[{"message":"nope"}]}`))
		default:
			// Truncated: must not be remembered just because the status said 200.
			_, _ = w.Write([]byte(`{"data":{"Media":{"id":`))
		}
	}))
	defer server.Close()

	cfg := loadTestConfig(t)
	cfg.Metadata.AniListEndpoint = server.URL
	api := New(Deps{Config: cfg, Cache: newStore(t, cfg), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})

	document := mustBody(t, map[string]any{"query": "query{Page{media{id}}}"})
	for attempt := 1; attempt <= 3; attempt++ {
		response := doRaw(api, http.MethodPost, "/api/anilist", document, "application/json")
		if response.Header().Get("X-Cache") == "HIT" {
			t.Fatalf("attempt %d replayed a response that was not usable: %s", attempt, response.Body.String())
		}
	}
	if calls := upstream.Load(); calls != 3 {
		t.Fatalf("upstream calls = %d, want every bad response retried", calls)
	}
}

func mustBody(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return encoded
}

// doRaw sends a body, which do() cannot: the existing helper always sends an empty request.
func doRaw(server *Server, method, path string, body []byte, contentType string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, strings.NewReader(string(body)))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	server.Router().ServeHTTP(recorder, request)
	return recorder
}

func snippet(body string) string {
	lines := strings.Split(body, "\n")
	var kept []string
	for _, line := range lines {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, "animind_") {
			kept = append(kept, line)
		}
	}
	if len(kept) > 30 {
		kept = kept[:30]
	}
	return strings.Join(kept, "\n")
}

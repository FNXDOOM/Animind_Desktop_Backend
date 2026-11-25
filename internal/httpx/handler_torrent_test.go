package httpx

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"animind-backend-go/internal/scanner"
	"animind-backend-go/internal/torrent"
)

// Handler tests for /api/torrents/search and /api/search/trending; the torrent service sits
// behind torrentServiceAPI, so every case runs on a fake without a database.

func searchFixture() torrent.SearchResponse {
	title := "[SubsPlease] Sousou no Frieren - 01 (1080p) [10bit].mkv"
	return torrent.SearchResponse{
		Data: []torrent.Result{{
			ID: "opaque", InfoHash: "aa11", Title: title,
			SizeBytes: 1473283210, Seeders: 412, Leechers: 9, Grabs: 2300,
			PublishedAt: time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC),
			Indexer:     "prowlarr", Category: "anime",
			Parsed: scanner.ParseTorrentTitle(title),
			Health: "excellent", Trusted: true,
			Magnet: "magnet:?xt=urn:btih:aa11", // must never reach the wire
		}},
		Total: 1, Cached: false, QueryUsed: []string{"Sousou no Frieren"},
	}
}

func TestTorrentSearchHandler(t *testing.T) {
	srv := testServerWith(t, func(d *Deps) {
		d.Torrent = &fakeTorrentService{resp: searchFixture()}
	})
	rec := callAs(t, user("user-1"), srv.handleTorrentSearch,
		http.MethodGet, "/api/torrents/search?q=frieren&limit=50", nil, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data      []map[string]any `json:"data"`
		Total     int              `json:"total"`
		Cached    bool             `json:"cached"`
		QueryUsed []string         `json:"queryUsed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Data) != 1 || body.Total != 1 || body.Cached {
		t.Fatalf("body: %s", rec.Body.String())
	}
	if len(body.QueryUsed) != 1 || body.QueryUsed[0] != "Sousou no Frieren" {
		t.Errorf("queryUsed = %v", body.QueryUsed)
	}
	row := body.Data[0]
	for _, key := range []string{"id", "infoHash", "title", "sizeBytes", "seeders", "leechers", "grabs",
		"publishedAt", "indexer", "category", "parsed", "health", "trusted"} {
		if _, has := row[key]; !has {
			t.Errorf("search row is missing %q", key)
		}
	}
	if _, has := row["magnet"]; has {
		t.Fatal("magnet must never be serialized")
	}
	parsed, _ := row["parsed"].(map[string]any)
	if parsed == nil || parsed["show"] == "" || parsed["isBatch"] == nil {
		t.Errorf("parsed block incomplete: %v", parsed)
	}
}

func TestTorrentSearchNotConfiguredAndDisabled(t *testing.T) {
	// No TORZNAB_URL: the server has no torrent service at all.
	srv := testServerWith(t, nil)
	rec := callAs(t, user("user-1"), srv.handleTorrentSearch,
		http.MethodGet, "/api/torrents/search?q=frieren", nil, nil)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("no indexer = %d, want 501", rec.Code)
	}
	assertCode(t, rec.Body.Bytes(), "indexer_not_configured")

	// s3 mode: local downloads are off, so search is refused even with an indexer.
	srv = testServerWith(t, func(d *Deps) {
		d.Config.Downloads.Enabled = false
		d.Torrent = &fakeTorrentService{resp: searchFixture()}
	})
	rec = callAs(t, user("user-1"), srv.handleTorrentSearch,
		http.MethodGet, "/api/torrents/search?q=frieren", nil, nil)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("s3 mode = %d, want 501", rec.Code)
	}
	assertCode(t, rec.Body.Bytes(), "downloads_unavailable")
}

func TestTorrentSearchRejectsBadQueries(t *testing.T) {
	srv := testServerWith(t, func(d *Deps) {
		d.Torrent = &fakeTorrentService{resp: searchFixture()}
	})
	fake := srv.torrent.(*fakeTorrentService)

	cases := []struct {
		name, target string
	}{
		{"missing q", "/api/torrents/search"},
		{"blank q", "/api/torrents/search?q="},
		{"one rune", "/api/torrents/search?q=f"},
		// 130 runes, no spaces: the URL must stay one legal request line.
		{"over 120 runes", "/api/torrents/search?q=" + strings.Repeat("a", 130)},
		{"bad sort", "/api/torrents/search?q=frieren&sort=votes"},
		{"bad batch", "/api/torrents/search?q=frieren&batch=sometimes"},
		{"bad season", "/api/torrents/search?q=frieren&season=one"},
	}
	for _, tc := range cases {
		rec := callAs(t, user("user-1"), srv.handleTorrentSearch, http.MethodGet, tc.target, nil, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400 (body %s)", tc.name, rec.Code, rec.Body.String())
		}
	}
	if len(fake.queries) != 0 {
		t.Errorf("bad queries reached the service: %v", fake.queries)
	}

	// A well-formed request carries the parsed filters and a clamped limit.
	rec := callAs(t, user("user-1"), srv.handleTorrentSearch,
		http.MethodGet, "/api/torrents/search?q=frieren&season=1&episode=5&resolution=1080&minSeeders=50&batch=only&sort=seeders&limit=5000",
		nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid request = %d, want 200", rec.Code)
	}
	q := fake.queries[0]
	if q.Q != "frieren" || q.Season == nil || *q.Season != 1 || q.Episode == nil || *q.Episode != 5 {
		t.Errorf("query parsed wrong: %+v", q)
	}
	if q.Resolution == nil || *q.Resolution != 1080 || q.MinSeeders == nil || *q.MinSeeders != 50 {
		t.Errorf("filters parsed wrong: %+v", q)
	}
	if q.Batch != "only" || q.Sort != "seeders" || q.Limit != 100 {
		t.Errorf("batch/sort/limit parsed wrong: %+v", q)
	}
}

func TestSearchTrendingHandler(t *testing.T) {
	// The endpoint reads the shared counter; only normalized query text is counted (spec §7.3).
	srv := testServerWith(t, func(d *Deps) { d.Trending = torrent.NewTrending(time.Now) })
	rec := callAs(t, user("user-1"), srv.handleSearchTrending,
		http.MethodGet, "/api/search/trending", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []string `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Data) != 0 {
		t.Fatalf("empty counter returned %v", body.Data)
	}

	srv.trending.Record("  Sousou no Frieren!!  ")
	srv.trending.Record("sousou no frieren")
	srv.trending.Record("frieren")
	for range 11 {
		srv.trending.Record("oshibanake")
	}

	rec = callAs(t, user("user-1"), srv.handleSearchTrending,
		http.MethodGet, "/api/search/trending", nil, nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Data) != 3 {
		t.Fatalf("data = %v, want 3 normalized queries", body.Data)
	}
	if body.Data[0] != "oshibanake" {
		t.Errorf("top query = %q, want the most-hit one", body.Data[0])
	}
	if body.Data[1] != "sousou no frieren" {
		t.Errorf("normalization lost: %q", body.Data[1])
	}
}

func TestDownloadRouteLimitsThroughRouter(t *testing.T) {
	// Limits attach ahead of authentication, so anonymous probes prove the presets are on the
	// routes: TorrentSearch 30/min, DownloadEvents 10/min.
	srv := testServerWith(t, nil)

	for attempt := 1; attempt <= 30; attempt++ {
		rec := do(srv, http.MethodGet, "/api/torrents/search", nil)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("torrents/search hit the limit early on attempt %d", attempt)
		}
	}
	if rec := do(srv, http.MethodGet, "/api/torrents/search", nil); rec.Code != http.StatusTooManyRequests {
		t.Errorf("31st torrents/search = %d, want 429", rec.Code)
	}

	events := testServerWith(t, nil)
	for attempt := 1; attempt <= 10; attempt++ {
		rec := do(events, http.MethodGet, "/api/downloads/events", nil)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("downloads/events hit the limit early on attempt %d", attempt)
		}
	}
	if rec := do(events, http.MethodGet, "/api/downloads/events", nil); rec.Code != http.StatusTooManyRequests {
		t.Errorf("11th downloads/events = %d, want 429", rec.Code)
	}
	// The events budget must not spend the read budget of the other download routes.
	if rec := do(events, http.MethodGet, "/api/downloads", nil); rec.Code == http.StatusTooManyRequests {
		t.Error("download-events limiter leaked into the download-read bucket")
	}
}

// assertCode checks the stable code field of an error body.
func assertCode(t *testing.T, body []byte, code string) {
	t.Helper()
	var payload struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("error body is not JSON: %q", string(body))
	}
	if payload.Code != code {
		t.Fatalf("code = %q, want %q (body %s)", payload.Code, code, string(body))
	}
}

// compile-time guard: the fake must keep satisfying the handler seam.
var _ torrentServiceAPI = (*fakeTorrentService)(nil)

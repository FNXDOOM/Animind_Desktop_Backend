package torrent

// Test doubles for the search pipeline. The cache fake is a mutex-guarded map with no
// expiry, on purpose: the search tests assert hit/miss and re-registration behaviour
// directly, and TTL arithmetic belongs to the real stores in internal/cache. The indexer
// and metadata fakes are the only seams Search consumes.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"animind-backend-go/internal/metadata"
	"animind-backend-go/internal/scanner"
)

type fakeIndexer struct {
	mu      sync.Mutex
	queries []Query
	results map[string][]Result // keyed by the query string actually sent
	err     error               // when set, every query fails with it
}

func (f *fakeIndexer) Search(_ context.Context, q Query) ([]Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, q)
	if f.err != nil {
		return nil, f.err
	}
	return f.results[q.Q], nil
}

func (f *fakeIndexer) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queries)
}

type fakeMetadata struct {
	meta *metadata.Metadata
	err  error
}

func (f *fakeMetadata) Fetch(_ context.Context, _ string) (*metadata.Metadata, error) {
	return f.meta, f.err
}

type mapKV struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newMapKV() *mapKV { return &mapKV{data: make(map[string][]byte)} }

func (m *mapKV) Get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, ok := m.data[key]
	if !ok {
		return nil, false, nil
	}
	return append([]byte(nil), raw...), true, nil
}

func (m *mapKV) Set(_ context.Context, key string, value []byte, _ time.Duration, _ ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = append([]byte(nil), value...)
	return nil
}

func (m *mapKV) Delete(_ context.Context, keys ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, key := range keys {
		delete(m.data, key)
	}
	return nil
}

// DeleteGroup stays a no-op: the search service never tags its entries with a group.
func (m *mapKV) DeleteGroup(_ context.Context, _ ...string) error { return nil }

func (m *mapKV) has(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.data[key]
	return ok
}

// newTestServiceMeta wires the standard fakes around idx; meta nil means the metadata
// lookup finds nothing, so the search runs q alone.
func newTestServiceMeta(t *testing.T, idx Indexer, meta *metadata.Metadata) *Service {
	t.Helper()
	return NewService(idx, &fakeMetadata{meta: meta}, newMapKV(), time.Minute, nil)
}

// newTestService is the helper the search tests use: the fake metadata resolves every title
// to Sousou no Frieren with the English alternate, so variant queries fire.
func newTestService(t *testing.T, idx Indexer) *Service {
	t.Helper()
	return newTestServiceMeta(t, idx, &metadata.Metadata{
		Title:           "Sousou no Frieren",
		AlternateTitles: []string{"Frieren"},
	})
}

func ptrInt(v int) *int { return &v }

func ptrStr(v string) *string { return &v }

func TestSearchMergesByInfoHashAndRanks(t *testing.T) {
	idx := &fakeIndexer{results: map[string][]Result{
		"Sousou no Frieren": {
			{InfoHash: "aa", Title: "[SubsPlease] Sousou no Frieren - 01 (1080p)", Seeders: 400,
				SizeBytes: 1000, Magnet: "magnet:?xt=urn:btih:aa&dn=frieren",
				Parsed: scanner.ParseTorrentTitle("[SubsPlease] Sousou no Frieren - 01 (1080p)")},
			{InfoHash: "bb", Title: "frieren 01 720p", Seeders: 4, SizeBytes: 500,
				Magnet: "magnet:?xt=urn:btih:bb&dn=frieren"},
		},
		"Frieren": {
			{InfoHash: "aa", Title: "[SubsPlease] Sousou no Frieren - 01 (1080p)", Seeders: 412,
				SizeBytes: 1000, Magnet: "magnet:?xt=urn:btih:aa&dn=frieren"},
		},
	}}
	svc := newTestService(t, idx) // fake metadata returning AlternateTitles ["Frieren"]
	resp, err := svc.Search(context.Background(), Query{Q: "frieren", Sort: "seeders", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.QueryUsed) < 2 || resp.Total != 2 {
		t.Fatalf("queries/total: %+v", resp)
	}
	if resp.Cached {
		t.Fatal("first search must not be cached")
	}
	if resp.Data[0].InfoHash != "aa" || resp.Data[0].Seeders != 412 {
		t.Fatalf("merge must keep the richest row: %+v", resp.Data[0])
	}
	if resp.Data[1].InfoHash != "bb" {
		t.Fatalf("rank order: %+v", resp.Data)
	}
	if resp.Data[0].ID == "" || len(resp.Data[0].ID) != 22 {
		t.Fatalf("opaque id: %q", resp.Data[0].ID)
	}
	if got, ok := svc.Result(resp.Data[0].ID); !ok || got.InfoHash != "aa" {
		t.Fatalf("id lookup: %v %v", got, ok)
	}
	if got, ok := svc.Result(resp.Data[0].ID); ok && got.Magnet == "" {
		t.Fatal("id lookup must hand the magnet back to the download flow")
	}
	if _, ok := svc.Result("expired-id"); ok {
		t.Fatal("unknown id must miss")
	}
	// result structs must not carry magnets over the wire: assert a JSON marshal of SearchResponse
	// contains no "magnet" key and no magnet: URL.
	blob, _ := json.Marshal(resp)
	if strings.Contains(string(blob), "magnet") {
		t.Fatal("magnet leaked into the response")
	}
	// The fake rows above carry magnets and btih hashes, so the two assertions are real: the
	// plain infoHash field is the one allowed hash on the wire.
	if strings.Contains(string(blob), "urn:btih") {
		t.Fatal("btih leaked into the response")
	}
	if !strings.Contains(string(blob), `"infoHash":"aa"`) {
		t.Fatalf("infoHash missing from the wire shape: %s", blob)
	}
	// health assigned when the client left it unset
	if resp.Data[0].Health != "excellent" || resp.Data[1].Health != "poor" {
		t.Fatalf("health: %q %q", resp.Data[0].Health, resp.Data[1].Health)
	}
	// The run is exactly the two deduped variants: the raw "frieren" never runs beside its
	// provider-cased variant, and both variant queries actually hit the indexer.
	if !slices.Equal(resp.QueryUsed, []string{"Sousou no Frieren", "Frieren"}) {
		t.Fatalf("queryUsed must be the deduped variant run: %v", resp.QueryUsed)
	}
	if idx.calls() != 2 {
		t.Fatalf("indexer calls: %d", idx.calls())
	}
}

func newFilterFixture(t *testing.T) (*Service, *fakeIndexer, *mapKV) {
	t.Helper()
	// The titles carry the semantics: the service re-parses every title, so "batch",
	// "720p x264" and "1080p hevc" produce the parsed fields the filters read.
	idx := &fakeIndexer{results: map[string][]Result{
		"op": {
			{InfoHash: "h0", Title: "op zero", Seeders: 0, SizeBytes: 100},
			{InfoHash: "h1", Title: "op batch", Seeders: 3, SizeBytes: 200},
			{InfoHash: "h2", Title: "op 720p x264", Seeders: 5, SizeBytes: 300},
			{InfoHash: "h3", Title: "op 1080p hevc", Seeders: 7, SizeBytes: 400},
		},
	}}
	kv := newMapKV()
	svc := NewService(idx, &fakeMetadata{}, kv, time.Minute, nil)
	return svc, idx, kv
}

func hashes(rows []Result) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.InfoHash
	}
	return out
}

func TestSearchFiltersAndCache(t *testing.T) {
	ctx := context.Background()

	t.Run("zero seeder rows hidden unless minSeeders is zero", func(t *testing.T) {
		svc, _, _ := newFilterFixture(t)
		resp, err := svc.Search(ctx, Query{Q: "op"})
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.Data) != 3 || slices.Contains(hashes(resp.Data), "h0") {
			t.Fatalf("default run hides dead rows (relevance order): %v", hashes(resp.Data))
		}
		dead, err := svc.Search(ctx, Query{Q: "op", MinSeeders: ptrInt(0), Sort: "seeders"})
		if err != nil {
			t.Fatal(err)
		}
		idx := slices.Index(hashes(dead.Data), "h0")
		if idx < 0 || dead.Data[idx].Health != "dead" {
			t.Fatalf("minSeeders=0 must keep dead rows with their health: %v", hashes(dead.Data))
		}
		seeded, err := svc.Search(ctx, Query{Q: "op", Sort: "seeders"})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(hashes(seeded.Data), []string{"h3", "h2", "h1"}) {
			t.Fatalf("seeders order: %v", hashes(seeded.Data))
		}
	})

	t.Run("minSeeders floor", func(t *testing.T) {
		svc, _, _ := newFilterFixture(t)
		resp, err := svc.Search(ctx, Query{Q: "op", MinSeeders: ptrInt(5)})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(hashes(resp.Data), []string{"h3", "h2"}) {
			t.Fatalf("minSeeders=5: %v", hashes(resp.Data))
		}
	})

	t.Run("batch only and exclude", func(t *testing.T) {
		svc, _, _ := newFilterFixture(t)
		only, err := svc.Search(ctx, Query{Q: "op", Batch: "only", Sort: "seeders"})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(hashes(only.Data), []string{"h1"}) {
			t.Fatalf("batch=only: %v", hashes(only.Data))
		}
		excl, err := svc.Search(ctx, Query{Q: "op", Batch: "exclude", Sort: "seeders"})
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(hashes(excl.Data), "h1") || !slices.Equal(hashes(excl.Data), []string{"h3", "h2"}) {
			t.Fatalf("batch=exclude: %v", hashes(excl.Data))
		}
	})

	t.Run("resolution and codec filter the parsed spec", func(t *testing.T) {
		svc, _, _ := newFilterFixture(t)
		res, err := svc.Search(ctx, Query{Q: "op", Resolution: ptrInt(1080), Sort: "seeders"})
		if err != nil {
			t.Fatal(err)
		}
		// h1 declares no resolution, so it cannot contradict the request and stays.
		if !slices.Equal(hashes(res.Data), []string{"h3", "h1"}) {
			t.Fatalf("resolution=1080: %v", hashes(res.Data))
		}
		codec, err := svc.Search(ctx, Query{Q: "op", Codec: ptrStr("x264"), Sort: "seeders"})
		if err != nil {
			t.Fatal(err)
		}
		// "x264" must normalize to the parsed AVC; the unparsed h1 stays.
		if !slices.Equal(hashes(codec.Data), []string{"h2", "h1"}) {
			t.Fatalf("codec=x264: %v", hashes(codec.Data))
		}
	})

	t.Run("second identical call is cached and skips the indexer", func(t *testing.T) {
		svc, idx, _ := newFilterFixture(t)
		first, err := svc.Search(ctx, Query{Q: "op"})
		if err != nil {
			t.Fatal(err)
		}
		if first.Cached || idx.calls() != 1 {
			t.Fatalf("first call: cached=%v calls=%d", first.Cached, idx.calls())
		}
		second, err := svc.Search(ctx, Query{Q: "op"})
		if err != nil {
			t.Fatal(err)
		}
		if !second.Cached || idx.calls() != 1 {
			t.Fatalf("second call: cached=%v calls=%d", second.Cached, idx.calls())
		}
		if second.Total != first.Total || second.Data[0].ID != first.Data[0].ID {
			t.Fatalf("cached response drifted: %+v vs %+v", second, first)
		}
	})

	t.Run("cache hit re-registers expired ids", func(t *testing.T) {
		svc, idx, kv := newFilterFixture(t)
		first, err := svc.Search(ctx, Query{Q: "op"})
		if err != nil {
			t.Fatal(err)
		}
		id := first.Data[0].ID
		if err := kv.Delete(ctx, resultCacheKey(id)); err != nil {
			t.Fatal(err)
		}
		if _, ok := svc.Result(id); ok {
			t.Fatal("deleted id must miss before the next search")
		}
		again, err := svc.Search(ctx, Query{Q: "op"})
		if err != nil {
			t.Fatal(err)
		}
		if !again.Cached || idx.calls() != 1 {
			t.Fatalf("expected a cache hit, got cached=%v calls=%d", again.Cached, idx.calls())
		}
		got, ok := svc.Result(id)
		if !ok || got.InfoHash != first.Data[0].InfoHash {
			t.Fatalf("cache hit must re-register ids: %v %v", got, ok)
		}
	})

	t.Run("query length errors before any query", func(t *testing.T) {
		svc, idx, _ := newFilterFixture(t)
		if _, err := svc.Search(ctx, Query{Q: "f"}); err == nil {
			t.Fatal("a 1-rune query must fail")
		}
		if _, err := svc.Search(ctx, Query{Q: strings.Repeat("あ", 121)}); err == nil {
			t.Fatal("a 121-rune query must fail")
		}
		if idx.calls() != 0 {
			t.Fatalf("rejected queries must not reach the indexer: %d", idx.calls())
		}
		// 120 runes is inside the bound, so this one runs.
		if _, err := svc.Search(ctx, Query{Q: strings.Repeat("あ", 120)}); err != nil {
			t.Fatalf("120 runes must be accepted: %v", err)
		}
	})
}

func TestSearchRecordsTrending(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	tr := NewTrending(func() time.Time { return now })
	idx := &fakeIndexer{results: map[string][]Result{
		"frieren": {{InfoHash: "aa", Title: "frieren 01", Seeders: 3}},
	}}
	svc := NewService(idx, &fakeMetadata{}, newMapKV(), time.Minute, tr)
	if _, err := svc.Search(context.Background(), Query{Q: " frieren "}); err != nil {
		t.Fatal(err)
	}
	// A rejected query is not a successful search and records nothing.
	if _, err := svc.Search(context.Background(), Query{Q: "x"}); err == nil {
		t.Fatal("a too-short query must fail")
	}
	if top := tr.Top(10); len(top) != 1 || top[0] != "frieren" {
		t.Fatalf("trending after one successful search: %v", top)
	}
}

func TestSearchPropagatesIndexerError(t *testing.T) {
	idx := &fakeIndexer{err: ErrNotConfigured}
	svc := newTestServiceMeta(t, idx, nil)
	_, err := svc.Search(context.Background(), Query{Q: "frieren"})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("indexer errors must propagate unwrapped enough for errors.Is: %v", err)
	}
}

func TestNewResultID(t *testing.T) {
	first := NewResultID()
	if len(first) != 22 {
		t.Fatalf("16 base64url bytes are 22 chars, got %q", first)
	}
	if strings.ContainsAny(first, "+/=") {
		t.Fatalf("id must be base64url without padding: %q", first)
	}
	if second := NewResultID(); second == first {
		t.Fatalf("ids must not repeat: %q", first)
	}
}

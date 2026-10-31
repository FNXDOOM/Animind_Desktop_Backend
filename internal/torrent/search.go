package torrent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sync/errgroup"

	"animind-backend-go/internal/cache"
	"animind-backend-go/internal/metadata"
	"animind-backend-go/internal/scanner"
)

// MetadataFetcher is the narrow seam Search needs from metadata.Service: the catalog names
// a query is worth retrying with. *metadata.Service satisfies it as-is; tests fake it.
type MetadataFetcher interface {
	Fetch(ctx context.Context, title string) (*metadata.Metadata, error)
}

const (
	searchKeyPrefix = "torrent:search:"
	resultKeyPrefix = "torrent:res:"
	// One user search costs at most three indexer calls: q plus up to two variants.
	maxQueries    = 3
	minQueryRunes = 2
	maxQueryRunes = 120
)

// Service runs the search pipeline: variants, concurrent indexer fan-out, merge, filters,
// ranking, opaque ids and the search cache. The clock the brief allots it lives inside the
// Trending counter, the only time-dependent part.
type Service struct {
	indexer  Indexer
	metadata MetadataFetcher
	shared   cache.KV
	ttl      time.Duration
	trending *Trending
}

// NewService wires the pipeline. shared is the same store instance the metadata service
// uses (a cache.Store satisfies cache.KV); a nil or zero-TTL store simply disables caching.
// A nil trending counter is replaced by a process-local one, so Record always runs.
func NewService(indexer Indexer, meta MetadataFetcher, shared cache.KV, cacheTTL time.Duration, trending *Trending) *Service {
	if trending == nil {
		trending = NewTrending(time.Now)
	}
	return &Service{indexer: indexer, metadata: meta, shared: shared, ttl: cacheTTL, trending: trending}
}

// SearchResponse is the wire shape of one search page. Result's own marshal is the wire
// view and cannot carry a magnet.
type SearchResponse struct {
	Data      []Result `json:"data"`
	Total     int      `json:"total"`
	Cached    bool     `json:"cached"`
	QueryUsed []string `json:"queryUsed"`
}

// Search answers one user query. q is normalized, enriched with up to two title variants
// from the metadata catalog and fanned out to the indexer concurrently; the merged results
// are filtered, ranked and cached under the normalized query plus filters.
func (s *Service) Search(ctx context.Context, q Query) (SearchResponse, error) {
	norm := normalizeQuery(q.Q)
	if n := utf8.RuneCountInString(norm); n < minQueryRunes || n > maxQueryRunes {
		return SearchResponse{}, fmt.Errorf("torrent: query must be %d-%d characters", minQueryRunes, maxQueryRunes)
	}

	if resp, ok := s.cachedSearch(ctx, norm, q); ok {
		s.trending.Record(norm)
		return resp, nil
	}

	queries := s.planQueries(ctx, norm)
	rows, err := s.runQueries(ctx, queries, q)
	if err != nil {
		return SearchResponse{}, err
	}
	merged := mergeResults(rows)
	assignHealth(merged)
	filtered := filterRows(merged, q)
	ranked := rankRows(filtered, norm, q.Sort)

	total := len(ranked)
	if limit := clampLimit(q.Limit); len(ranked) > limit {
		ranked = ranked[:limit]
	}
	resp := SearchResponse{Data: ranked, Total: total, QueryUsed: queries}
	for i := range resp.Data {
		resp.Data[i].ID = NewResultID()
		s.storeResult(ctx, toCachedResult(resp.Data[i]))
	}
	s.storeSearch(ctx, norm, q, resp)
	s.trending.Record(norm)
	return resp, nil
}

// Result resolves one opaque id back into its full row, magnet included, for the download
// flow. A miss is the handler's 410 result_expired.
func (s *Service) Result(id string) (Result, bool) {
	if s.shared == nil || id == "" {
		return Result{}, false
	}
	// No request context is available at this signature, so the store's own timeouts apply.
	cr, found, err := cache.GetJSON[cachedResult](context.Background(), s.shared, resultCacheKey(id))
	if err != nil || !found {
		return Result{}, false
	}
	return cr.result(), true
}

// NewResultID mints one opaque result id: 16 random bytes, base64url, no padding — 22 chars.
func NewResultID() string {
	var b [16]byte
	rand.Read(b[:]) // crypto/rand.Read is documented to never fail (Go 1.24+)
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// normalizeQuery trims and collapses whitespace; case is kept, because indexers match the
// provider spellings the variants carry.
func normalizeQuery(q string) string {
	return strings.Join(strings.Fields(q), " ")
}

// planQueries builds the concurrent run. Variants come from the metadata catalog (primary
// title, then alternates); they are deduped case-insensitively, and a variant that is q in
// provider casing stands in for q instead of running beside it — sending both would spend
// one of the three calls on the same query. The stand-in is moved to the front of the run
// so the three-query cap can never truncate it away: a long variant list must not leave the
// user's query unexecuted. A metadata miss never fails the search.
func (s *Service) planQueries(ctx context.Context, q string) []string {
	variants := s.titleVariants(ctx, q)
	seen := make(map[string]bool, len(variants))
	unique := make([]string, 0, len(variants))
	for _, v := range variants {
		low := strings.ToLower(v)
		if low == "" || seen[low] {
			continue
		}
		seen[low] = true
		unique = append(unique, v)
	}
	if seen[strings.ToLower(q)] {
		// The q-matching variant is the stand-in for q; it must survive the cap, so it
		// leads the run before truncation.
		for i, v := range unique {
			if strings.ToLower(v) == strings.ToLower(q) {
				unique[0], unique[i] = unique[i], unique[0]
				break
			}
		}
		// The q slot is freed, so a third variant may use it; the cap stays three.
		if len(unique) > maxQueries {
			unique = unique[:maxQueries]
		}
		return unique
	}
	if len(unique) > maxQueries-1 {
		unique = unique[:maxQueries-1]
	}
	return append([]string{q}, unique...)
}

func (s *Service) titleVariants(ctx context.Context, q string) []string {
	if s.metadata == nil {
		return nil
	}
	meta, err := s.metadata.Fetch(ctx, q)
	if err != nil || meta == nil {
		return nil
	}
	out := make([]string, 0, 1+len(meta.AlternateTitles))
	if t := normalizeQuery(meta.Title); t != "" {
		out = append(out, t)
	}
	for _, alt := range meta.AlternateTitles {
		if t := normalizeQuery(alt); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// runQueries fans the query run out concurrently. Every title is (re-)parsed here: variant
// rows can arrive unparsed, and the parse is deterministic, so overwriting is safe. The
// first indexer error fails the whole search.
func (s *Service) runQueries(ctx context.Context, queries []string, q Query) ([]Result, error) {
	perQuery := make([][]Result, len(queries))
	g, gctx := errgroup.WithContext(ctx)
	for i, query := range queries {
		g.Go(func() error {
			// Only Q, Season, Episode and Limit travel to Torznab; the rest of the
			// filters are client-side.
			sub := q
			sub.Q = query
			sub.Resolution, sub.Codec, sub.MinSeeders, sub.Batch, sub.Sort = nil, nil, nil, "", ""
			rows, err := s.indexer.Search(gctx, sub)
			if err != nil {
				return fmt.Errorf("torrent: query %q: %w", query, err)
			}
			for j := range rows {
				rows[j].Parsed = scanner.ParseTorrentTitle(rows[j].Title)
			}
			perQuery[i] = rows // distinct index per goroutine: no race
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	total := 0
	for _, rows := range perQuery {
		total += len(rows)
	}
	out := make([]Result, 0, total)
	for _, rows := range perQuery {
		out = append(out, rows...)
	}
	return out, nil
}

// mergeResults dedupes by info hash, keeping the row with the richer parse and, on a tie,
// the higher seeder count. Rows without a hash are dropped: dedupe and the download
// pipeline both key on it.
func mergeResults(rows []Result) []Result {
	byHash := make(map[string]*Result, len(rows))
	order := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.InfoHash == "" {
			continue
		}
		existing, ok := byHash[r.InfoHash]
		if !ok {
			row := r
			byHash[r.InfoHash] = &row
			order = append(order, r.InfoHash)
			continue
		}
		if parsedRichness(r.Parsed) > parsedRichness(existing.Parsed) ||
			(parsedRichness(r.Parsed) == parsedRichness(existing.Parsed) && r.Seeders > existing.Seeders) {
			*existing = r
		}
	}
	out := make([]Result, 0, len(order))
	for _, h := range order {
		out = append(out, *byHash[h])
	}
	return out
}

// assignHealth fills the bucket the indexer client left empty; fakes and other backends do
// not always set it.
func assignHealth(rows []Result) {
	for i := range rows {
		if rows[i].Health == "" {
			rows[i].Health = healthFor(rows[i].Seeders)
		}
	}
}

// ── Cache ────────────────────────────────────────────────────────────────────────

// cachedSearch is the full-fidelity cache form of a search page. Result's marshal is the
// wire view and strips magnets, so the cache keeps its own encoding; a hit re-registers the
// ids from rows that still carry theirs.
type cachedSearch struct {
	Rows      []cachedResult `json:"rows"`
	Total     int            `json:"total"`
	QueryUsed []string       `json:"queryUsed"`
}

// cachedResult mirrors Result with the magnet included: Result(id) must hand the magnet
// back to the download flow even when it is answered from the cache.
type cachedResult struct {
	ID          string               `json:"id"`
	InfoHash    string               `json:"infoHash"`
	Title       string               `json:"title"`
	SizeBytes   int64                `json:"sizeBytes"`
	Seeders     int                  `json:"seeders"`
	Leechers    int                  `json:"leechers"`
	Grabs       int                  `json:"grabs"`
	PublishedAt time.Time            `json:"publishedAt"`
	Indexer     string               `json:"indexer"`
	Category    string               `json:"category"`
	Parsed      scanner.TorrentTitle `json:"parsed"`
	Health      string               `json:"health"`
	Trusted     bool                 `json:"trusted"`
	Magnet      string               `json:"magnet"`
}

func toCachedResult(r Result) cachedResult {
	return cachedResult{
		ID: r.ID, InfoHash: r.InfoHash, Title: r.Title, SizeBytes: r.SizeBytes,
		Seeders: r.Seeders, Leechers: r.Leechers, Grabs: r.Grabs,
		PublishedAt: r.PublishedAt, Indexer: r.Indexer, Category: r.Category,
		Parsed: r.Parsed, Health: r.Health, Trusted: r.Trusted, Magnet: r.Magnet,
	}
}

func (c cachedResult) result() Result {
	return Result{
		ID: c.ID, InfoHash: c.InfoHash, Title: c.Title, SizeBytes: c.SizeBytes,
		Seeders: c.Seeders, Leechers: c.Leechers, Grabs: c.Grabs,
		PublishedAt: c.PublishedAt, Indexer: c.Indexer, Category: c.Category,
		Parsed: c.Parsed, Health: c.Health, Trusted: c.Trusted, Magnet: c.Magnet,
	}
}

func (s *Service) cachedSearch(ctx context.Context, norm string, q Query) (SearchResponse, bool) {
	if s.shared == nil {
		return SearchResponse{}, false
	}
	entry, found, err := cache.GetJSON[cachedSearch](ctx, s.shared, searchCacheKey(norm, q))
	if err != nil || !found {
		return SearchResponse{}, false
	}
	resp := SearchResponse{
		Data:      make([]Result, len(entry.Rows)),
		Total:     entry.Total,
		Cached:    true,
		QueryUsed: entry.QueryUsed,
	}
	for i, cr := range entry.Rows {
		resp.Data[i] = cr.result()
	}
	// The search entry and the per-id entries share a TTL but stores may evict
	// independently; re-register whatever fell out so Result(id) keeps working across hits.
	for _, cr := range entry.Rows {
		if _, found, _ := cache.GetJSON[cachedResult](ctx, s.shared, resultCacheKey(cr.ID)); !found {
			s.storeResult(ctx, cr)
		}
	}
	return resp, true
}

func (s *Service) storeSearch(ctx context.Context, norm string, q Query, resp SearchResponse) {
	if s.shared == nil || s.ttl <= 0 {
		return
	}
	entry := cachedSearch{Rows: make([]cachedResult, len(resp.Data)), Total: resp.Total, QueryUsed: resp.QueryUsed}
	for i, r := range resp.Data {
		entry.Rows[i] = toCachedResult(r)
	}
	// A failed cache write degrades to a cold cache; the indexer is still the authority.
	_ = cache.SetJSON(ctx, s.shared, searchCacheKey(norm, q), entry, s.ttl)
}

func (s *Service) storeResult(ctx context.Context, cr cachedResult) {
	if s.shared == nil || s.ttl <= 0 {
		return
	}
	_ = cache.SetJSON(ctx, s.shared, resultCacheKey(cr.ID), cr, s.ttl)
}

// searchCacheKey is the namespaced key for one normalized query plus its filter set. The
// fingerprint is hashed so odd query bytes cannot forge keys, mirroring cache.AniListKey.
func searchCacheKey(norm string, q Query) string {
	h := sha256.New()
	fmt.Fprintf(h, "q\x00%s\x00season\x00%s\x00episode\x00%s\x00resolution\x00%s\x00codec\x00%s\x00minSeeders\x00%s\x00batch\x00%s\x00sort\x00%s\x00limit\x00%d",
		norm, optInt(q.Season), optInt(q.Episode), optInt(q.Resolution), optString(q.Codec),
		optInt(q.MinSeeders), q.Batch, q.Sort, q.Limit)
	return searchKeyPrefix + hex.EncodeToString(h.Sum(nil))
}

// resultCacheKey is the namespaced key for one opaque result id. Ids are base64url, so the
// key needs no escaping.
func resultCacheKey(id string) string {
	return resultKeyPrefix + id
}

func optInt(v *int) string {
	if v == nil {
		return ""
	}
	return strconv.Itoa(*v)
}

func optString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// ── Wire shape ───────────────────────────────────────────────────────────────────

// wireResult is the HTTP shape of one search hit. Result's marshal goes through it so a
// magnet can never reach the wire — the client re-attaches via the opaque id and the
// download flow reads the magnet back from the cache. Field names are camelCase, matching
// the brief's "queryUsed".
type wireResult struct {
	ID          string     `json:"id"`
	InfoHash    string     `json:"infoHash"`
	Title       string     `json:"title"`
	SizeBytes   int64      `json:"sizeBytes"`
	Seeders     int        `json:"seeders"`
	Leechers    int        `json:"leechers"`
	Grabs       int        `json:"grabs"`
	PublishedAt time.Time  `json:"publishedAt"`
	Indexer     string     `json:"indexer"`
	Category    string     `json:"category"`
	Parsed      wireParsed `json:"parsed"`
	Health      string     `json:"health"`
	Trusted     bool       `json:"trusted"`
}

type wireParsed struct {
	Show         string   `json:"show"`
	Season       *int     `json:"season"`
	EpisodeStart *int     `json:"episodeStart"`
	EpisodeEnd   *int     `json:"episodeEnd"`
	IsBatch      bool     `json:"isBatch"`
	Spec         wireSpec `json:"spec"`
}

type wireSpec struct {
	Resolution   *int     `json:"resolution"`
	Source       *string  `json:"source"`
	VideoCodec   *string  `json:"videoCodec"`
	BitDepth     *int     `json:"bitDepth"`
	AudioCodecs  []string `json:"audioCodecs"`
	DualAudio    bool     `json:"dualAudio"`
	Subs         *string  `json:"subs"`
	HDR          bool     `json:"hdr"`
	ReleaseGroup *string  `json:"releaseGroup"`
	IsBatch      bool     `json:"isBatch"`
}

// MarshalJSON strips the magnet: the invariant lives on the type, so no handler can leak it.
func (r Result) MarshalJSON() ([]byte, error) {
	return json.Marshal(wireResult{
		ID: r.ID, InfoHash: r.InfoHash, Title: r.Title, SizeBytes: r.SizeBytes,
		Seeders: r.Seeders, Leechers: r.Leechers, Grabs: r.Grabs,
		PublishedAt: r.PublishedAt, Indexer: r.Indexer, Category: r.Category,
		Parsed: wireParsed{
			Show:         r.Parsed.Show,
			Season:       r.Parsed.Season,
			EpisodeStart: r.Parsed.EpisodeStart,
			EpisodeEnd:   r.Parsed.EpisodeEnd,
			IsBatch:      r.Parsed.IsBatch,
			Spec: wireSpec{
				Resolution:   r.Parsed.Spec.Resolution,
				Source:       r.Parsed.Spec.Source,
				VideoCodec:   r.Parsed.Spec.VideoCodec,
				BitDepth:     r.Parsed.Spec.BitDepth,
				AudioCodecs:  r.Parsed.Spec.AudioCodecs,
				DualAudio:    r.Parsed.Spec.DualAudio,
				Subs:         r.Parsed.Spec.Subs,
				HDR:          r.Parsed.Spec.HDR,
				ReleaseGroup: r.Parsed.Spec.ReleaseGroup,
				IsBatch:      r.Parsed.Spec.IsBatch,
			},
		},
		Health:  r.Health,
		Trusted: r.Trusted,
	})
}

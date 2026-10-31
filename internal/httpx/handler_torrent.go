package httpx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"animind-backend-go/internal/torrent"
)

// Handlers for the torrent search surface: /api/torrents/search and
// /api/search/trending. Both are session routes; search additionally refuses
// when the deployment has no Torznab endpoint or runs in s3 mode, where
// nothing torrent-related operates.

// torrentServiceAPI is the narrow seam the HTTP layer needs from the torrent
// search service; *torrent.Service satisfies it as-is.
type torrentServiceAPI interface {
	Search(ctx context.Context, q torrent.Query) (torrent.SearchResponse, error)
	Result(id string) (torrent.Result, bool)
}

var _ torrentServiceAPI = (*torrent.Service)(nil)

// Search parameter bounds, matching the service's own validation so a bad
// request is refused here before it spends an indexer call.
const (
	minQueryRunes = 2
	maxQueryRunes = 120
)

// Query validation errors, each rendered as a plain 400.
var (
	errBadQuery = errors.New("q must be between 2 and 120 characters.")
	errBadBatch = errors.New("batch must be any, only or exclude.")
	errBadSort  = errors.New("sort must be relevance, seeders, size or date.")
)

func errBadFilter(key string) error {
	return fmt.Errorf("%s must be an integer.", key)
}

// handleTorrentSearch runs one user query through the search pipeline. The
// response is the service's own wire shape: Result's marshal strips magnets,
// so none of the rows can carry one.
func (s *Server) handleTorrentSearch(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Downloads.Enabled {
		WriteErrorCode(w, http.StatusNotImplemented, "downloads_unavailable",
			"Torrent search is only available in local storage mode.")
		return
	}
	// The torrent service is constructed only when TORZNAB_URL is set, so a nil
	// seam is the deployment's "not configured" state; the service's own
	// ErrNotConfigured covers anything else.
	if s.torrent == nil {
		WriteErrorCode(w, http.StatusNotImplemented, "indexer_not_configured",
			"No Torznab indexer is configured on this server.")
		return
	}

	query, err := parseTorrentQuery(r)
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := s.torrent.Search(r.Context(), query)
	if err != nil {
		if errors.Is(err, torrent.ErrNotConfigured) {
			WriteErrorCode(w, http.StatusNotImplemented, "indexer_not_configured",
				"No Torznab indexer is configured on this server.")
			return
		}
		s.serverError(w, "Search failed.", err)
		return
	}
	WriteJSON(w, http.StatusOK, resp)
}

// parseTorrentQuery lifts the query string into the service's Query. Optional
// filters that are present but malformed are client errors rather than
// silently dropped values.
func parseTorrentQuery(r *http.Request) (torrent.Query, error) {
	params := r.URL.Query()
	q := strings.Join(strings.Fields(params.Get("q")), " ")
	if n := utf8.RuneCountInString(q); n < minQueryRunes || n > maxQueryRunes {
		return torrent.Query{}, errBadQuery
	}
	query := torrent.Query{Q: q, Batch: params.Get("batch"), Sort: params.Get("sort")}
	switch query.Batch {
	case "", "any", "only", "exclude":
	default:
		return torrent.Query{}, errBadBatch
	}
	switch query.Sort {
	case "", "relevance", "seeders", "size", "date":
	default:
		return torrent.Query{}, errBadSort
	}
	var err error
	if query.Season, err = optionalIntParam(params, "season"); err != nil {
		return torrent.Query{}, err
	}
	if query.Episode, err = optionalIntParam(params, "episode"); err != nil {
		return torrent.Query{}, err
	}
	if query.Resolution, err = optionalIntParam(params, "resolution"); err != nil {
		return torrent.Query{}, err
	}
	if query.MinSeeders, err = optionalIntParam(params, "minSeeders"); err != nil {
		return torrent.Query{}, err
	}
	if raw := strings.TrimSpace(params.Get("codec")); raw != "" {
		query.Codec = &raw
	}
	// The service applies its own default (50) when limit is unset or zero.
	query.Limit = QueryInt(r, "limit", 0, 0, 100)
	return query, nil
}

func optionalIntParam(params url.Values, key string) (*int, error) {
	raw := strings.TrimSpace(params.Get(key))
	if raw == "" {
		return nil, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return nil, errBadFilter(key)
	}
	return &value, nil
}

// ── Trending ─────────────────────────────────────────────────────────────────

// handleSearchTrending returns the most-recorded normalized queries of the
// last seven days. The counter stores query text only — no user identity is
// recorded anywhere in the search pipeline (spec §7.3) — so there is nothing
// per-user to filter on here.
func (s *Server) handleSearchTrending(w http.ResponseWriter, r *http.Request) {
	top := []string{}
	if s.trending != nil {
		top = s.trending.Top(10)
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": top})
}

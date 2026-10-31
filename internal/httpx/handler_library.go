package httpx

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"animind-backend-go/internal/cache"
	"animind-backend-go/internal/metadata"
	"animind-backend-go/internal/repository"
	"animind-backend-go/internal/scanner"
	"animind-backend-go/internal/syncplay"
)

// Catalog, administration and diagnostics.
//
// The catalog is intentionally public: both clients render the library before sign-in. It
// exposes only what a card or detail page shows, never a file path or bucket name, which is
// why Episode rows drop those fields at the JSON edge.

// showListItem is one catalog card.
type showListItem struct {
	ID               string    `json:"id"`
	Title            string    `json:"title"`
	Synopsis         *string   `json:"synopsis"`
	CoverImageURL    *string   `json:"cover_image_url"`
	Genres           []string  `json:"genres"`
	Rating           *float64  `json:"rating"`
	EpisodeCount     *int      `json:"episode_count"`
	Studio           *string   `json:"studio"`
	Status           *string   `json:"status"`
	Year             *string   `json:"year"`
	AnilistID        *int      `json:"anilist_id"`
	TrailerID        *string   `json:"trailer_id"`
	TrailerSite      *string   `json:"trailer_site"`
	TrailerThumbnail *string   `json:"trailer_thumbnail"`
	CreatedAt        time.Time `json:"created_at"`
}

func showToItem(show repository.Show) showListItem {
	return showListItem{
		ID: show.ID, Title: show.Title, Synopsis: show.Synopsis, CoverImageURL: show.CoverImageURL,
		Genres: show.Genres, Rating: show.Rating, EpisodeCount: show.EpisodeCount, Studio: show.Studio,
		Status: show.Status, Year: show.Year, AnilistID: show.AnilistID, TrailerID: show.TrailerID,
		TrailerSite: show.TrailerSite, TrailerThumbnail: show.TrailerThumbnail, CreatedAt: show.CreatedAt,
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	response := map[string]any{
		"status":    "ok",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"uptime":    time.Since(s.startedAt).Round(time.Second).String(),
		"storage":   string(s.cfg.Storage.Mode),
	}
	// A database that stops answering makes the API useless; report it as degraded rather
	// than claiming health.
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		response["status"] = "degraded"
		response["database"] = "unreachable"
		WriteJSON(w, http.StatusServiceUnavailable, response)
		return
	}
	response["database"] = "ok"
	response["watch_parties"] = s.hub != nil && s.hub.RoomCount() > 0
	WriteJSON(w, http.StatusOK, response)
}

// handleListShows returns a page of the catalog.
//
// Duplicate identities are collapsed the way the original backend did, on AniList id when
// present and normalized title otherwise, keeping the richer row. That is a display
// safeguard: the scanner is the place a duplicate should never have been created.
func (s *Server) handleListShows(w http.ResponseWriter, r *http.Request) {
	limit := QueryInt(r, "limit", 50, 1, 200)
	offset := QueryInt(r, "offset", 0, 0, 1_000_000)
	search := r.URL.Query().Get("q")
	genre := r.URL.Query().Get("genre")

	shows, total, err := s.store.ListShows(r.Context(), limit, offset, search, genre)
	if err != nil {
		s.serverError(w, "Failed to fetch shows.", err)
		return
	}

	deduped := dedupeShows(shows)
	items := make([]showListItem, 0, len(deduped))
	for _, show := range deduped {
		items = append(items, showToItem(show))
	}

	WriteJSON(w, http.StatusOK, map[string]any{
		"data":     items,
		"total":    len(items),
		"rawTotal": total,
		"limit":    limit,
		"offset":   offset,
	})
}

// dedupeShows collapses rows that describe the same series, keeping the richer one and
// preserving the incoming (title-ordered) sequence.
//
// Two passes, because a library accumulates duplicates two ways: the same external entry
// imported under two spellings of its title (caught by the AniList id), and the same title
// indexed twice with only one of them enriched (caught by the normalized title). The scanner
// is where a duplicate should never have been created; this is the display safeguard that keeps
// the grid from showing the same card twice regardless of how the rows got that way.
func dedupeShows(shows []repository.Show) []repository.Show {
	type entry struct {
		show repository.Show
		rank int
	}

	merge := func(rows []repository.Show, keyOf func(repository.Show) string) []repository.Show {
		order := make([]string, 0, len(rows))
		best := make(map[string]entry, len(rows))
		for _, show := range rows {
			key := keyOf(show)
			previous, seen := best[key]
			if !seen {
				order = append(order, key)
				best[key] = entry{show: show, rank: showRank(show)}
				continue
			}
			if rank := showRank(show); rank > previous.rank {
				best[key] = entry{show: show, rank: rank}
			}
		}
		out := make([]repository.Show, 0, len(order))
		for _, key := range order {
			out = append(out, best[key].show)
		}
		return out
	}

	byTitle := func(show repository.Show) string { return "title:" + metadata.NormalizeTitleKey(show.Title) }
	byAnilist := func(show repository.Show) string {
		if show.AnilistID == nil {
			// Rows with no external link are not identifiable this way, so each keeps its own
			// slot and the title pass below decides.
			return fmt.Sprintf("unlinked:%s", show.ID)
		}
		return fmt.Sprintf("anilist:%d", *show.AnilistID)
	}

	return merge(merge(shows, byAnilist), byTitle)
}

// showRank scores a row's richness. The dedupe pass keeps whichever row could render the
// better card.
func showRank(show repository.Show) int {
	score := 0
	if show.AnilistID != nil {
		score += 8
	}
	if show.CoverImageURL != nil && *show.CoverImageURL != "" {
		score += 4
	}
	if show.Synopsis != nil && *show.Synopsis != "" {
		score += 2
	}
	if len(show.Genres) > 0 {
		score++
	}
	if show.Rating != nil {
		score++
	}
	return score
}

// episodeListItem hides storage details from clients.
type episodeListItem struct {
	ID            string    `json:"id"`
	SeasonNumber  int       `json:"season_number"`
	ContentType   string    `json:"content_type"`
	EpisodeNumber *float64  `json:"episode_number"`
	Title         *string   `json:"title"`
	Thumbnail     *string   `json:"thumbnail"`
	Duration      *int      `json:"duration"`
	CreatedAt     time.Time `json:"created_at"`
}

// handleShowByID returns one show with its episodes.
//
// Episodes are de-duplicated on (season, content type, episode number) and ordered, and a
// fractional episode number keeps its fraction: a special labelled 5.5 must not merge into
// episode 5.
func (s *Server) handleShowByID(w http.ResponseWriter, r *http.Request) {
	id := Param(r, "id")
	show, err := s.store.ShowByIDText(r.Context(), id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			WriteError(w, http.StatusNotFound, "Show not found.")
			return
		}
		s.serverError(w, "Failed to fetch the show.", err)
		return
	}

	episodes, err := s.store.EpisodesForShow(r.Context(), show.ID)
	if err != nil {
		s.serverError(w, "Failed to fetch episodes.", err)
		return
	}

	seen := map[string]bool{}
	items := make([]episodeListItem, 0, len(episodes))
	for _, episode := range episodes {
		if episode.EpisodeNumber == nil {
			// A row with no number cannot be placed in a list; merging it at 0 would hide a
			// real episode.
			continue
		}
		number := normalizeEpisodeNumber(*episode.EpisodeNumber)
		season := normalizeSeason(episode.SeasonNumber)
		contentType := episode.ContentType
		if contentType == "" {
			contentType = "tv"
		}
		key := fmt.Sprintf("%d:%s:%v", season, contentType, number)
		if seen[key] {
			continue
		}
		seen[key] = true

		thumbnail := episode.Thumbnail
		if thumbnail == nil || *thumbnail == "" {
			// Neither client has per-episode artwork, so the show cover is served rather
			// than leaving the card blank.
			thumbnail = show.CoverImageURL
		}
		items = append(items, episodeListItem{
			ID: episode.ID, SeasonNumber: season, ContentType: contentType,
			EpisodeNumber: &number, Title: episode.Title, Thumbnail: thumbnail,
			Duration: episode.Duration, CreatedAt: episode.CreatedAt,
		})
	}

	response := showToItem(*show)
	WriteJSON(w, http.StatusOK, struct {
		showListItem
		Episodes []episodeListItem `json:"episodes"`
	}{response, items})
}

func normalizeEpisodeNumber(value float64) float64 {
	if value == float64(int64(value)) {
		return float64(int64(value))
	}
	return value
}

func normalizeSeason(value int) int {
	if value < 0 {
		return 1
	}
	return value
}

// handleAniListProxy forwards a read-only GraphQL document to AniList.
//
// The site needs AniList's search and detail operations, which a browser cannot call
// directly because AniList sends no CORS headers. Only queries are accepted: a mutation or
// subscription in a proxied document would let a visitor write to someone else's AniList
// account using a token this server never holds.
func (s *Server) handleAniListProxy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Query     string          `json:"query"`
		Variables json.RawMessage `json:"variables"`
	}
	if err := DecodeJSON(r, &body); err != nil {
		WriteError(w, http.StatusBadRequest, `Missing or invalid "query" field.`)
		return
	}
	query := strings.TrimSpace(body.Query)
	if query == "" || len(query) > 10000 {
		WriteError(w, http.StatusBadRequest, `Missing or invalid "query" field.`)
		return
	}
	if !isReadOnlyGraphQL(query) {
		WriteError(w, http.StatusBadRequest, "Only read-only AniList GraphQL queries are allowed.")
		return
	}
	if len(body.Variables) > 0 {
		trimmed := strings.TrimSpace(string(body.Variables))
		if trimmed != "" && !strings.HasPrefix(trimmed, "{") {
			WriteError(w, http.StatusBadRequest, `Invalid "variables" field.`)
			return
		}
	}

	payload := map[string]any{"query": query}
	if len(body.Variables) > 0 {
		var variables map[string]any
		if err := json.Unmarshal(body.Variables, &variables); err != nil {
			WriteError(w, http.StatusBadRequest, `Invalid "variables" field.`)
			return
		}
		payload["variables"] = variables
	} else {
		payload["variables"] = map[string]any{}
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		s.serverError(w, "Could not build the upstream request.", err)
		return
	}

	// AniList answers the same document identically for everybody, and the site re-requests
	// the same search and detail queries as a user browses. Relaying every one of them cost an
	// outbound call per click and left AniList's rate limit as the site's capacity limit.
	cacheKey := cache.AniListKey(string(encoded))
	if document, ok := s.cachedAniList(r.Context(), cacheKey); ok {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-Cache", "HIT")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(document)
		return
	}

	requestCtx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, s.cfg.Metadata.AniListEndpoint, strings.NewReader(string(encoded)))
	if err != nil {
		s.serverError(w, "Could not reach AniList.", err)
		return
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "AniMind/1.0 ("+s.cfg.PublicIssuer()+")")

	response, err := s.aniListHTTP.Do(request)
	if err != nil {
		s.logger.Warn("AniList proxy upstream failed", slog.String("error", err.Error()))
		WriteErrorCode(w, http.StatusBadGateway, "anilist_unreachable", "AniList could not be reached.")
		return
	}
	defer response.Body.Close()

	upstream, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		s.serverError(w, "Could not read the AniList response.", err)
		return
	}
	if response.StatusCode != http.StatusOK {
		s.logger.Warn("AniList upstream responded badly", slog.Int("status", response.StatusCode))
	} else if isUsableGraphQLDocument(upstream) {
		// Only a good document is stored: caching an empty match would hide the title from
		// every later search for half a day.
		s.rememberAniList(r.Context(), cacheKey, upstream)
	}
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(upstream)
}

// cachedAniList reads one proxied document. A store that cannot be reached is a miss.
func (s *Server) cachedAniList(ctx context.Context, key string) ([]byte, bool) {
	if s.cache == nil {
		return nil, false
	}
	document, found, err := s.cache.Get(ctx, key)
	if err != nil {
		return nil, false
	}
	return document, found
}

func (s *Server) rememberAniList(ctx context.Context, key string, document []byte) {
	if s.cache == nil {
		return
	}
	if err := s.cache.Set(ctx, key, document, s.cfg.Cache.MetadataTTL); err != nil {
		s.logger.Warn("could not store the AniList response", slog.String("error", err.Error()))
	}
}

// isUsableGraphQLDocument reports a response worth remembering: JSON carrying data and no
// top-level errors. AniList answers some failures with 200 and an errors array, which must
// not become a half-day of silence.
func isUsableGraphQLDocument(body []byte) bool {
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return false
	}
	if len(envelope.Errors) > 0 {
		return false
	}
	return len(envelope.Data) > 0 && string(envelope.Data) != "null"
}

// isReadOnlyGraphQL rejects documents that mutate, including one hidden inside a comment
// or a fragment, which is why comments are stripped before the check.
func isReadOnlyGraphQL(query string) bool {
	var lines []string
	for _, line := range strings.Split(query, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "#") {
			continue
		}
		lines = append(lines, line)
	}
	stripped := strings.Join(lines, "\n")
	lowered := strings.ToLower(stripped)
	return !strings.Contains(lowered, "mutation") && !strings.Contains(lowered, "subscription")
}

// handleStorageWebhook triggers a scan when the object store reports a new file.
//
// It answers before scanning: a bucket that waits on a scan would retry the notification,
// and scans are serialized anyway.
func (s *Server) handleStorageWebhook(w http.ResponseWriter, r *http.Request) {
	secret := r.Header.Get("x-webhook-secret")
	// Constant-time comparison: this is a shared secret, and a byte-at-a-time mismatch would
	// turn the endpoint into an oracle for recovering it.
	if subtle.ConstantTimeCompare([]byte(secret), []byte(s.cfg.WebhookSecret)) != 1 {
		WriteError(w, http.StatusUnauthorized, "Invalid webhook secret.")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]bool{"received": true})

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if _, err := s.scanner.Run(ctx); err != nil {
			s.logger.Error("background scan from webhook failed", slog.String("error", err.Error()))
		}
	}()
}

// rescanResponse is the library list a client refreshes from after a scan.
type rescanResponse struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Synopsis string   `json:"synopsis"`
	ImageURL string   `json:"imageUrl"`
	Rating   float64  `json:"rating"`
	Genres   []string `json:"genres"`
	Episodes int      `json:"episodes"`
	Studio   string   `json:"studio"`
	Status   string   `json:"status"`
	Year     string   `json:"year"`
	Reason   string   `json:"reason"`
	Trailer  *trailer `json:"trailer,omitempty"`
}

type trailer struct {
	ID        string  `json:"id"`
	Site      string  `json:"site"`
	Thumbnail *string `json:"thumbnail"`
}

// handleRescan runs a scan and returns the refreshed library in the camelCase shape the
// site's Anime type expects.
func (s *Server) handleRescan(w http.ResponseWriter, r *http.Request) {
	result, err := s.runScan(w, r)
	if err != nil {
		return
	}
	_ = result

	summaries, err := s.store.ListShowSummaries(r.Context())
	if err != nil {
		s.serverError(w, "Scan finished but the library list could not be loaded.", err)
		return
	}

	items := make([]rescanResponse, 0, len(summaries))
	for _, show := range summaries {
		item := rescanResponse{
			ID: show.ID, Title: show.Title, Synopsis: orEmpty(show.Synopsis),
			ImageURL: orEmpty(show.CoverImageURL), Rating: orZero(show.Rating),
			Genres: show.Genres, Episodes: orZeroInt(show.EpisodeCount),
			Studio: orEmpty(show.Studio), Status: orEmpty(show.Status), Year: orEmpty(show.Year),
			Reason: "From your cloud library",
		}
		if item.Genres == nil {
			item.Genres = []string{}
		}
		if show.TrailerID != nil && *show.TrailerID != "" {
			site := "youtube"
			if show.TrailerSite != nil && *show.TrailerSite != "" {
				site = *show.TrailerSite
			}
			item.Trailer = &trailer{ID: *show.TrailerID, Site: site, Thumbnail: show.TrailerThumbnail}
		}
		items = append(items, item)
	}
	WriteJSON(w, http.StatusOK, items)
}

// runScan performs a scan and maps its errors to responses. It returns the error when the
// response has already been written.
func (s *Server) runScan(w http.ResponseWriter, r *http.Request) (*scanner.Result, error) {
	result, err := s.scanner.Run(r.Context())
	if err != nil {
		switch {
		case errors.Is(err, scanner.ErrAlreadyRunning):
			WriteErrorCode(w, http.StatusConflict, "scan_in_progress", "A library scan is already running.")
		default:
			WriteErrorDetails(w, http.StatusInternalServerError, "Scan failed.", safeDetail(err, s.cfg.IsProduction))
		}
		return nil, err
	}

	// Prewarming is best-effort and runs after the response: first playback of a new episode
	// would otherwise wait on an ffmpeg transcode.
	if s.cfg.IsLocal() && s.cfg.AudioCache.PrewarmEnabled {
		targets := s.scanner.PrewarmTargets()
		if len(targets) > 0 {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
				defer cancel()
				built := s.audio.PrewarmLibrary(ctx, s.storage, targets)
				s.logger.Info("audio prewarm finished", slog.Int("variants", built))
			}()
		}
	}
	return result, nil
}

func (s *Server) handleAdminScan(w http.ResponseWriter, r *http.Request) {
	result, err := s.scanner.Run(r.Context())
	if err != nil {
		if errors.Is(err, scanner.ErrAlreadyRunning) {
			WriteErrorCode(w, http.StatusConflict, "scan_in_progress", "A library scan is already running.")
			return
		}
		s.serverError(w, "Scan failed.", err)
		return
	}
	WriteJSON(w, http.StatusOK, result)
}

// handleAdminUsers lists accounts.
//
// This replaces the old profile-row list: every account appears, including ones that never
// opened the site, because the user table is now the only source of truth for admins.
func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	limit := QueryInt(r, "limit", 100, 1, 200)
	offset := QueryInt(r, "offset", 0, 0, 100_000)
	users, err := s.store.ListUsers(r.Context(), limit, offset)
	if err != nil {
		s.serverError(w, "Failed to fetch users.", err)
		return
	}
	WriteJSON(w, http.StatusOK, users)
}

type setAdminRequest struct {
	IsAdmin bool `json:"is_admin"`
	// CanDownload is optional: nil leaves the entitlement untouched, so the
	// legacy {"is_admin": ...} payload keeps its exact old behaviour.
	CanDownload *bool `json:"can_download"`
}

func (s *Server) handleAdminSetUserAdmin(w http.ResponseWriter, r *http.Request) {
	var req setAdminRequest
	if err := DecodeJSON(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, "is_admin must be a boolean.")
		return
	}
	target := Param(r, "id")
	principal := PrincipalFromContext(r.Context())
	if principal != nil && target == principal.UserID && !req.IsAdmin {
		WriteError(w, http.StatusBadRequest, "You cannot remove your own admin access.")
		return
	}
	if err := s.store.SetAdmin(r.Context(), target, req.IsAdmin); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			WriteError(w, http.StatusNotFound, "User not found.")
			return
		}
		s.serverError(w, "Failed to update user.", err)
		return
	}
	if req.CanDownload != nil {
		if err := s.store.SetCanDownload(r.Context(), target, *req.CanDownload); err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				WriteError(w, http.StatusNotFound, "User not found.")
				return
			}
			s.serverError(w, "Failed to update user.", err)
			return
		}
	}
	// IsAdmin and can_download are part of the cached identity, so a change must
	// evict it or the change lands a minute late.
	s.auth.InvalidateUser(r.Context(), target)
	WriteJSON(w, http.StatusOK, map[string]bool{"success": true})
}

func (s *Server) handleAdminDeleteShow(w http.ResponseWriter, r *http.Request) {
	id := Param(r, "id")
	if err := s.store.DeleteShow(r.Context(), id); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			WriteError(w, http.StatusNotFound, "Show not found.")
			return
		}
		s.serverError(w, "Failed to delete show.", err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]bool{"success": true})
}

// handleAdminMetrics reports real operational numbers, replacing the hard-coded dashboard
// values the site showed.
func (s *Server) handleAdminMetrics(w http.ResponseWriter, r *http.Request) {
	shows, episodes, err := s.store.LibraryCounts(r.Context())
	if err != nil {
		s.serverError(w, "Could not load library counts.", err)
		return
	}
	users, err := s.store.CountUsers(r.Context())
	if err != nil {
		s.serverError(w, "Could not load account counts.", err)
		return
	}
	parties, err := s.store.CountActiveParties(r.Context())
	if err != nil {
		s.logger.Warn("could not count watch parties", slog.String("error", err.Error()))
	}

	metrics := map[string]any{
		"shows":          shows,
		"episodes":       episodes,
		"users":          users,
		"watch_parties":  parties,
		"storage_mode":   string(s.cfg.Storage.Mode),
		"uptime_seconds": int64(time.Since(s.startedAt).Seconds()),
		"server_time":    time.Now().UTC().Format(time.RFC3339),
	}
	if s.hls != nil {
		metrics["hls"] = s.hls.Stats()
	}
	if s.hub != nil {
		metrics["socket_peers"] = s.hub.PeerCount()
		metrics["socket_rooms"] = s.hub.RoomCount()
	}
	WriteJSON(w, http.StatusOK, metrics)
}

// handleAdminCleanupCaches runs the audio-cache TTL sweep on demand.
func (s *Server) handleAdminCleanupCaches(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.IsLocal() {
		WriteError(w, http.StatusBadRequest, "The audio variant cache is only used in local storage mode.")
		return
	}
	result, err := s.audio.CleanupByTTL(s.cfg.AudioCache.VariantTTL)
	if err != nil {
		s.serverError(w, "Cache cleanup failed.", err)
		return
	}
	WriteJSON(w, http.StatusOK, result)
}

// handleRoomLookup answers whether a room code is live and which episode it is on, so a
// client can validate a code before asking the user to open a show.
func (s *Server) handleRoomLookup(w http.ResponseWriter, r *http.Request) {
	code := strings.ToUpper(strings.TrimSpace(Param(r, "code")))
	if code == "" {
		WriteError(w, http.StatusBadRequest, "Room code is required.")
		return
	}
	if s.hub != nil {
		if snapshot, ok := s.hub.Snapshot(code); ok {
			WriteJSON(w, http.StatusOK, snapshot)
			return
		}
	}
	// A code that is not live may still have been a room, which distinguishes "expired" from
	// "never existed".
	if s.hub != nil {
		if stored, err := s.hub.PersistedRoom(code); err == nil && stored != nil {
			WriteErrorCode(w, http.StatusGone, "room_ended", "That watch party has ended.")
			return
		}
	}
	WriteError(w, http.StatusNotFound, "Room not found.")
}

// SyncPlayPath re-exports the socket path for documentation output.
const SyncPlayPath = syncplay.SocketPath

func orEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func orZero(value *float64) float64 {
	if value == nil {
		return 0
	}
	return *value
}

func orZeroInt(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

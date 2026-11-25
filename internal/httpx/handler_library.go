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

// Catalog, administration and diagnostics. The catalog is public: both clients render it
// before sign-in, and cards never carry a file path or bucket name.

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
	// A database that stops answering makes the API useless, so report degraded.
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

// handleListShows returns a page of the catalog with duplicate series collapsed.
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

// dedupeShows collapses rows describing the same series, keeping the richer one and incoming
// title order; duplicates arrive as one AniList id under two spellings, or one title twice.
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
			// Rows with no external link keep their own slot; the title pass decides.
			return fmt.Sprintf("unlinked:%s", show.ID)
		}
		return fmt.Sprintf("anilist:%d", *show.AnilistID)
	}

	return merge(merge(shows, byAnilist), byTitle)
}

// showRank scores a row's richness so dedupe keeps the row that renders the better card.
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

// handleShowByID returns one show with its episodes de-duplicated on (season, content type,
// episode number); a fractional number keeps its fraction, so 5.5 never merges into 5.
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
			// A row with no number cannot be placed in a list; merging it at 0 hides a real episode.
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
			// Neither client has per-episode artwork, so the show cover fills the card.
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

// handleAniListProxy forwards a read-only GraphQL document to AniList, which sends no CORS
// headers. Only queries pass: this server holds no AniList token for a mutation.
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

	// AniList answers the same document identically for everybody, so caching every search
	// and detail query avoids an outbound call per click and AniList's rate limit.
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
	// Cache only documents small enough to bound memory: an oversized response could evict
	// the rest of the metadata working set.
	const maxCachedAniListBytes = 256 << 10
	if response.StatusCode != http.StatusOK {
		s.logger.Warn("AniList upstream responded badly", slog.Int("status", response.StatusCode))
	} else if len(upstream) <= maxCachedAniListBytes && isUsableGraphQLDocument(upstream) {
		// Only a good document is stored; an empty match would hide the title for half a day.
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

// isUsableGraphQLDocument reports a response worth remembering: JSON with data and no
// top-level errors, since AniList answers some failures with 200 and an errors array.
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

// isReadOnlyGraphQL rejects mutating documents, so comments are stripped before the check.
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

// handleStorageWebhook triggers a scan on a new object; it answers before scanning because a
// bucket waiting on a scan would retry the notification.
func (s *Server) handleStorageWebhook(w http.ResponseWriter, r *http.Request) {
	secret := r.Header.Get("x-webhook-secret")
	// Constant-time comparison: a byte-at-a-time mismatch would turn the endpoint into an
	// oracle for recovering this shared secret.
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

// runLibraryScan performs a scan, then schedules the audio prewarm that keeps a new episode's
// first play from waiting on ffmpeg; the scanner serializes overlapping runs.
func (s *Server) runLibraryScan(ctx context.Context) (*scanner.Result, error) {
	if s.scanner == nil {
		return nil, errors.New("scanner is not configured")
	}
	result, err := s.scanner.Run(ctx)
	if err != nil {
		return nil, err
	}

	// Prewarming is best-effort; without it a new episode's first play waits on ffmpeg.
	if s.cfg.IsLocal() && s.cfg.AudioCache.PrewarmEnabled && s.audio != nil && s.storage != nil {
		targets := s.scanner.PrewarmTargets()
		if len(targets) > 0 {
			go func() {
				prewarmCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
				defer cancel()
				built := s.audio.PrewarmLibrary(prewarmCtx, s.storage, targets)
				s.logger.Info("audio prewarm finished", slog.Int("variants", built))
			}()
		}
	}
	return result, nil
}

func (s *Server) handleAdminScan(w http.ResponseWriter, r *http.Request) {
	result, err := s.runLibraryScan(r.Context())
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

// handleAdminUsers lists accounts from the user table, so an account that never opened the
// site appears too.
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
	// IsAdmin is optional: nil leaves the flag untouched, so a can_download-only PATCH
	// cannot demote an admin target.
	IsAdmin *bool `json:"is_admin"`
	// CanDownload is optional: nil leaves the entitlement untouched.
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
	// The self-demote guard fires only when this request would actually change is_admin;
	// toggling can_download on self is legitimate.
	if principal != nil && target == principal.UserID && req.IsAdmin != nil && !*req.IsAdmin {
		WriteError(w, http.StatusBadRequest, "You cannot remove your own admin access.")
		return
	}
	if req.IsAdmin != nil {
		if err := s.store.SetAdmin(r.Context(), target, *req.IsAdmin); err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				WriteError(w, http.StatusNotFound, "User not found.")
				return
			}
			s.serverError(w, "Failed to update user.", err)
			return
		}
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
	// IsAdmin and can_download live in the cached identity, so a change must evict it.
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

// handleAdminMetrics reports real operational numbers for the admin dashboard.
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

// handleRoomLookup answers whether a room code is live and which episode it is on.
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
	// A dead code may still have been a room, which distinguishes expired from never existed.
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

package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"animind-backend-go/internal/auth"
	"animind-backend-go/internal/repository"
)

// User-owned data endpoints.
//
// The site used to write its watchlist and progress rows straight to the database with an
// externally issued JWT, and the desktop player used to talk to a REST gateway. Both now
// come through here, which is where the ownership check lives: every statement is filtered
// by the session's user id, never by an id the client supplied.

// watchlistItemResponse is a saved show. anime_data is the exact object the client stored,
// which is what its cards are built from.
type watchlistItemResponse struct {
	ID        string          `json:"id"`
	UserID    string          `json:"user_id"`
	AnimeID   string          `json:"anime_id"`
	AnimeData json.RawMessage `json:"anime_data"`
	Status    string          `json:"status"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// watchlistStatuses are the values the site's filter chips match.
var watchlistStatuses = map[string]bool{
	"Watching": true, "Completed": true, "Plan to Watch": true, "Dropped": true,
}

// normalizeWatchlistStatus validates a status and applies the default.
//
// It is a pure function so the whitelist and the default can be tested without a database or
// a session: this is the rule that decides whether a show shows up under "Watching".
func normalizeWatchlistStatus(raw string) (string, error) {
	status := strings.TrimSpace(raw)
	if status == "" {
		return "Plan to Watch", nil
	}
	if !watchlistStatuses[status] {
		return "", errors.New("status must be Watching, Completed, Plan to Watch or Dropped")
	}
	return status, nil
}

// normalizeProgress validates a resume write and clamps a negative position, which a seeking
// client can produce.
func normalizeProgress(animeID string, episodeIndex int, seconds float64) (string, int, float64, error) {
	trimmed := strings.TrimSpace(animeID)
	if trimmed == "" {
		return "", 0, 0, errors.New("anime_id is required")
	}
	if episodeIndex < 0 || episodeIndex > 100000 {
		return "", 0, 0, errors.New("episode_index is out of range")
	}
	if seconds < 0 {
		seconds = 0
	}
	return trimmed, episodeIndex, seconds, nil
}

func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.requirePrincipal(w, r)
	if !ok {
		return
	}
	// Deleting a user cascades their watchlist, progress, history, preferences, sessions and
	// party rows, so no orphan can keep a claim on the account's identifiers.
	if err := s.auth.DeleteAccount(r.Context(), principal.UserID); err != nil {
		s.serverError(w, "Failed to delete account.", err)
		return
	}
	s.clearSessionCookie(w)
	s.logger.Info("account deleted", slog.String("user_id", principal.UserID))
	WriteJSON(w, http.StatusOK, map[string]bool{"success": true})
}

type updateProfileRequest struct {
	Username        *string `json:"username"`
	AvatarURL       *string `json:"avatar_url"`
	Email           *string `json:"email"`
	CurrentPassword string  `json:"current_password"`
}

func (s *Server) handleUpdateProfile(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req updateProfileRequest
	if err := DecodeJSON(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, "username, avatar_url and email must be strings.")
		return
	}
	if req.Email != nil {
		if strings.TrimSpace(req.CurrentPassword) == "" {
			WriteError(w, http.StatusBadRequest, "current_password is required to change your email.")
			return
		}
		user, err := s.auth.ChangeEmail(r.Context(), principal.UserID, principal.WebSessionRowID, *req.Email, req.CurrentPassword)
		if err != nil {
			switch {
			case errors.Is(err, auth.ErrPasswordNotSet):
				// A Google-only account has no password to re-authenticate with.
				WriteErrorCode(w, http.StatusBadRequest, "password_not_set", "This account signs in with Google.")
			case errors.Is(err, auth.ErrEmailTaken):
				WriteError(w, http.StatusConflict, "An account with that email already exists.")
			case errors.Is(err, auth.ErrPasswordIncorrect):
				WriteError(w, http.StatusBadRequest, "The current password is incorrect.")
			case errors.Is(err, auth.ErrInvalidEmail):
				WriteError(w, http.StatusBadRequest, "Enter a valid email address.")
			default:
				s.serverError(w, "Could not update your profile.", err)
			}
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"user": user})
		return
	}
	if req.Username == nil && req.AvatarURL == nil {
		WriteError(w, http.StatusBadRequest, "Provide username, avatar_url or email.")
		return
	}
	user, err := s.auth.UpdateProfile(r.Context(), principal.UserID, req.Username, req.AvatarURL)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidUsername) || err != nil && strings.Contains(err.Error(), "avatar_url") {
			WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.serverError(w, "Could not update your profile.", err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"user": user})
}

func (s *Server) handleListWatchlist(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.requirePrincipal(w, r)
	if !ok {
		return
	}
	items, err := s.store.ListWatchlist(r.Context(), principal.UserID)
	if err != nil {
		s.serverError(w, "Could not load your watchlist.", err)
		return
	}
	out := make([]watchlistItemResponse, 0, len(items))
	for _, item := range items {
		out = append(out, watchlistItemResponse{
			ID: item.ID, UserID: item.UserID, AnimeID: item.AnimeID,
			AnimeData: json.RawMessage(item.AnimeData), Status: item.Status,
			CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
		})
	}
	// The desktop client requires a top-level array, so the list is returned bare.
	WriteJSON(w, http.StatusOK, out)
}

type putWatchlistRequest struct {
	AnimeID   *string         `json:"anime_id"`
	AnimeData json.RawMessage `json:"anime_data"`
	Status    string          `json:"status"`
}

func (s *Server) handlePutWatchlist(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.requirePrincipal(w, r)
	if !ok {
		return
	}
	animeID := strings.TrimSpace(Param(r, "animeId"))
	if animeID == "" {
		WriteError(w, http.StatusBadRequest, "anime_id is required.")
		return
	}

	var req putWatchlistRequest
	if err := DecodeJSON(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, "anime_data must be a JSON object.")
		return
	}
	if req.AnimeData == nil || len(req.AnimeData) == 0 {
		WriteError(w, http.StatusBadRequest, "anime_data is required.")
		return
	}
	if !json.Valid(req.AnimeData) {
		WriteError(w, http.StatusBadRequest, "anime_data is not valid JSON.")
		return
	}
	status, err := normalizeWatchlistStatus(req.Status)
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.AnimeID != nil && strings.TrimSpace(*req.AnimeID) != "" && strings.TrimSpace(*req.AnimeID) != animeID {
		// A mismatched body id would let one request write two identities; the path wins.
		WriteError(w, http.StatusConflict, "anime_id in the body does not match the path.")
		return
	}

	item, err := s.store.UpsertWatchlist(r.Context(), principal.UserID, animeID, req.AnimeData, status)
	if err != nil {
		s.serverError(w, "Could not save that show.", err)
		return
	}
	WriteJSON(w, http.StatusOK, watchlistItemResponse{
		ID: item.ID, UserID: item.UserID, AnimeID: item.AnimeID,
		AnimeData: json.RawMessage(item.AnimeData), Status: item.Status,
		CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	})
}

type patchWatchlistRequest struct {
	Status string `json:"status"`
}

func (s *Server) handlePatchWatchlist(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req patchWatchlistRequest
	if err := DecodeJSON(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, "status is required.")
		return
	}
	status, err := normalizeWatchlistStatus(req.Status)
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.UpdateWatchlistStatus(r.Context(), principal.UserID, Param(r, "animeId"), status); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			WriteError(w, http.StatusNotFound, "That show is not on your watchlist.")
			return
		}
		s.serverError(w, "Could not update that entry.", err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]bool{"success": true})
}

func (s *Server) handleDeleteWatchlist(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteWatchlistItem(r.Context(), principal.UserID, Param(r, "animeId")); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			WriteError(w, http.StatusNotFound, "That show is not on your watchlist.")
			return
		}
		s.serverError(w, "Could not remove that entry.", err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]bool{"success": true})
}

// handleGetProgress returns one resume position.
//
// episode_index is the client's positional index into its episode list. It is stored as-is
// for compatibility with rows the site already has; prefer the per-episode history endpoint
// for new clients, which keys on a show title instead.
func (s *Server) handleGetProgress(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.requirePrincipal(w, r)
	if !ok {
		return
	}
	animeID := strings.TrimSpace(r.URL.Query().Get("anime_id"))
	if animeID == "" {
		WriteError(w, http.StatusBadRequest, "anime_id is required.")
		return
	}
	index := QueryInt(r, "episode_index", 0, 0, 100000)
	seconds, err := s.store.ProgressSeconds(r.Context(), principal.UserID, animeID, index)
	if err != nil {
		s.serverError(w, "Could not load your progress.", err)
		return
	}
	// A bare number is what the player's resume logic expects; a missing row is 0, not 404.
	WriteJSON(w, http.StatusOK, map[string]any{"timestamp": seconds, "anime_id": animeID, "episode_index": index})
}

type putProgressRequest struct {
	AnimeID      string  `json:"anime_id"`
	EpisodeIndex int     `json:"episode_index"`
	Timestamp    float64 `json:"timestamp"`
}

func (s *Server) handlePutProgress(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req putProgressRequest
	if err := DecodeJSON(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, "anime_id, episode_index and timestamp are required.")
		return
	}
	animeID, episodeIndex, seconds, err := normalizeProgress(req.AnimeID, req.EpisodeIndex, req.Timestamp)
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Progress is written on a client timer; the rate limiter covers the volume, so this
	// handler stays as cheap as a single upsert.
	if err := s.store.SaveProgress(r.Context(), principal.UserID, animeID, episodeIndex, seconds); err != nil {
		s.serverError(w, "Could not save your progress.", err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]bool{"success": true})
}

func (s *Server) handleRecentProgress(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.requirePrincipal(w, r)
	if !ok {
		return
	}
	limit := QueryInt(r, "limit", 20, 1, 100)
	items, err := s.store.RecentProgress(r.Context(), principal.UserID, limit)
	if err != nil {
		s.serverError(w, "Could not load your history.", err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleListHistory(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.requirePrincipal(w, r)
	if !ok {
		return
	}
	limit := QueryInt(r, "limit", 50, 1, 200)
	items, err := s.store.ListHistory(r.Context(), principal.UserID, limit)
	if err != nil {
		s.serverError(w, "Could not load your continue-watching list.", err)
		return
	}
	if items == nil {
		items = []repository.HistoryItem{}
	}
	// The desktop player parses a top-level array exactly like the REST gateway it replaced.
	WriteJSON(w, http.StatusOK, items)
}

type putHistoryRequest struct {
	AnilistID    *string `json:"anilist_id"`
	ShowTitle    string  `json:"show_title"`
	EpisodeLabel string  `json:"episode_label"`
	ThumbnailURL *string `json:"thumbnail_url"`
	ProgressPct  int     `json:"progress_pct"`
}

func (s *Server) handlePutHistory(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req putHistoryRequest
	if err := DecodeJSON(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, "show_title is required.")
		return
	}
	req.ShowTitle = strings.TrimSpace(req.ShowTitle)
	if req.ShowTitle == "" {
		WriteError(w, http.StatusBadRequest, "show_title is required.")
		return
	}
	req.ProgressPct = clampPercent(req.ProgressPct)

	item := repository.HistoryItem{
		UserID: principal.UserID, AnilistID: req.AnilistID, ShowTitle: req.ShowTitle,
		EpisodeLabel: strings.TrimSpace(req.EpisodeLabel), ThumbnailURL: req.ThumbnailURL,
		ProgressPct: req.ProgressPct, LastWatched: time.Now().UTC(),
	}
	if err := s.store.UpsertHistory(r.Context(), item); err != nil {
		s.serverError(w, "Could not save your history.", err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]bool{"success": true})
}

func clampPercent(value int) int {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func (s *Server) handleGetPreferences(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.requirePrincipal(w, r)
	if !ok {
		return
	}
	doc, err := s.store.Preferences(r.Context(), principal.UserID)
	if err != nil {
		s.serverError(w, "Could not load your preferences.", err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(doc)
}

// handlePutPreferences stores player settings so volume, speed, subtitle and audio choices
// follow the account instead of one browser profile.
func (s *Server) handlePutPreferences(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.requirePrincipal(w, r)
	if !ok {
		return
	}
	body, err := readJSONBody(r, 64*1024)
	if err != nil {
		WriteError(w, http.StatusBadRequest, "preferences must be a JSON object.")
		return
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(body, &decoded); err != nil {
		WriteError(w, http.StatusBadRequest, "preferences must be a JSON object.")
		return
	}
	stored, err := s.store.SavePreferences(r.Context(), principal.UserID, body)
	if err != nil {
		s.serverError(w, "Could not save your preferences.", err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"preferences": json.RawMessage(stored)})
}

// readJSONBody reads at most limit bytes of a request body.
func readJSONBody(r *http.Request, limit int64) ([]byte, error) {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errors.New("preferences document is too large")
	}
	if len(body) == 0 {
		return nil, errors.New("request body is required")
	}
	return body, nil
}

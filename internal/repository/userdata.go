package repository

import (
	"context"
	"encoding/json"
)

// User-owned viewing data: watchlist, resume progress, continue-watching history and
// player preferences.
//
// Every statement here is filtered by user_id with no exception: ownership is enforced
// in SQL rather than by a check in the handler, so a forgotten guard cannot expose
// another account's rows.

// ListWatchlist returns a user's saved shows, newest first, with the stored anime
// payload intact.
func (s *Store) ListWatchlist(ctx context.Context, userID string) ([]WatchlistItem, error) {
	rows, err := s.pool.Query(ctx, `
		select id::text, user_id::text, anime_id, anime_data, status, created_at, updated_at
		from watchlist
		where user_id = $1::uuid
		order by created_at desc
		limit 500`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]WatchlistItem, 0, 32)
	for rows.Next() {
		var w WatchlistItem
		if err := rows.Scan(&w.ID, &w.UserID, &w.AnimeID, &w.AnimeData, &w.Status, &w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// UpsertWatchlist saves or replaces a watchlist entry. animeData is the client's own
// show object, stored verbatim so a later read can rebuild the card without a catalog
// lookup. The payload is re-serialized by Postgres, which also rejects non-JSON.
func (s *Store) UpsertWatchlist(ctx context.Context, userID, animeID string, animeData json.RawMessage, status string) (*WatchlistItem, error) {
	if status == "" {
		status = "Plan to Watch"
	}
	var w WatchlistItem
	err := s.pool.QueryRow(ctx, `
		insert into watchlist (user_id, anime_id, anime_data, status, updated_at)
		values ($1::uuid, $2, $3::jsonb, $4, now())
		on conflict (user_id, anime_id) do update
		set anime_data = excluded.anime_data, status = excluded.status, updated_at = now()
		returning id::text, user_id::text, anime_id, anime_data, status, created_at, updated_at`,
		userID, animeID, []byte(animeData), status).
		Scan(&w.ID, &w.UserID, &w.AnimeID, &w.AnimeData, &w.Status, &w.CreatedAt, &w.UpdatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return &w, nil
}

// UpdateWatchlistStatus changes only the user's watch status.
func (s *Store) UpdateWatchlistStatus(ctx context.Context, userID, animeID, status string) error {
	tag, err := s.pool.Exec(ctx, `
		update watchlist set status = $3, updated_at = now()
		where user_id = $1::uuid and anime_id = $2`, userID, animeID, status)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteWatchlistItem removes an entry.
func (s *Store) DeleteWatchlistItem(ctx context.Context, userID, animeID string) error {
	tag, err := s.pool.Exec(ctx, `
		delete from watchlist where user_id = $1::uuid and anime_id = $2`, userID, animeID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ProgressSeconds returns the stored resume position for one episode, or 0 when the
// user has never watched it. Callers must not treat a missing row as an error: the
// player asks before playback and expects a number.
func (s *Store) ProgressSeconds(ctx context.Context, userID, animeID string, episodeIndex int) (float64, error) {
	var ts float64
	err := s.pool.QueryRow(ctx, `
		select timestamp from progress
		where user_id = $1::uuid and anime_id = $2 and episode_index = $3`,
		userID, animeID, episodeIndex).Scan(&ts)
	if err != nil {
		if isNoRows(err) {
			return 0, nil
		}
		return 0, err
	}
	return ts, nil
}

// SaveProgress writes a resume position. This runs on a client timer roughly every
// five seconds during playback, so it is an upsert with no read-first step.
func (s *Store) SaveProgress(ctx context.Context, userID, animeID string, episodeIndex int, seconds float64) error {
	_, err := s.pool.Exec(ctx, `
		insert into progress (user_id, anime_id, episode_index, timestamp, updated_at)
		values ($1::uuid, $2, $3, $4, now())
		on conflict (user_id, anime_id, episode_index) do update
		set timestamp = excluded.timestamp, updated_at = now()`,
		userID, animeID, episodeIndex, seconds)
	return err
}

// RecentProgress lists the shows a user has been watching, newest first.
//
// The site previously had no resume rail because no such query existed; this backs
// GET /api/me/progress/recent.
func (s *Store) RecentProgress(ctx context.Context, userID string, limit int) ([]ProgressItem, error) {
	rows, err := s.pool.Query(ctx, `
		select anime_id, episode_index, timestamp, updated_at
		from progress
		where user_id = $1::uuid and timestamp > 0
		order by updated_at desc
		limit $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ProgressItem, 0, 16)
	for rows.Next() {
		var p ProgressItem
		if err := rows.Scan(&p.AnimeID, &p.EpisodeIndex, &p.Timestamp, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpsertHistory records a continue-watching entry written by the desktop player.
// Rows are unique per show and episode label, matching how that client reports state.
func (s *Store) UpsertHistory(ctx context.Context, in HistoryItem) error {
	_, err := s.pool.Exec(ctx, `
		insert into watch_history (user_id, anilist_id, show_title, episode_label, thumbnail_url, progress_pct, last_watched)
		values ($1::uuid, $2, $3, $4, $5, $6, $7)
		on conflict (user_id, show_title, episode_label) do update
		set anilist_id = excluded.anilist_id,
		    thumbnail_url = excluded.thumbnail_url,
		    progress_pct = excluded.progress_pct,
		    last_watched = excluded.last_watched`,
		in.UserID, in.AnilistID, in.ShowTitle, in.EpisodeLabel, in.ThumbnailURL, in.ProgressPct, in.LastWatched)
	return err
}

// ListHistory returns the continue-watching rail, newest first.
func (s *Store) ListHistory(ctx context.Context, userID string, limit int) ([]HistoryItem, error) {
	rows, err := s.pool.Query(ctx, `
		select user_id::text, anilist_id, show_title, episode_label, thumbnail_url, progress_pct, last_watched
		from watch_history
		where user_id = $1::uuid
		order by last_watched desc
		limit $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]HistoryItem, 0, 32)
	for rows.Next() {
		var h HistoryItem
		if err := rows.Scan(&h.UserID, &h.AnilistID, &h.ShowTitle, &h.EpisodeLabel, &h.ThumbnailURL, &h.ProgressPct, &h.LastWatched); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Preferences returns a user's stored player settings, or an empty object when none
// have been saved yet.
func (s *Store) Preferences(ctx context.Context, userID string) (json.RawMessage, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `
		select coalesce(preferences, '{}'::jsonb) from user_preferences where user_id = $1::uuid`, userID).Scan(&raw)
	if err != nil {
		if isNoRows(err) {
			return json.RawMessage("{}"), nil
		}
		return nil, err
	}
	return json.RawMessage(raw), nil
}

// SavePreferences replaces a user's settings document.
func (s *Store) SavePreferences(ctx context.Context, userID string, doc json.RawMessage) (json.RawMessage, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `
		insert into user_preferences (user_id, preferences, updated_at)
		values ($1::uuid, $2::jsonb, now())
		on conflict (user_id) do update set preferences = excluded.preferences, updated_at = now()
		returning preferences`, userID, []byte(doc)).Scan(&raw)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

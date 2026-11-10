package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// The library queries are shared by the catalog endpoints, the scanner, and the
// streaming handlers, so they live together and are the only place episode file
// paths are read.

const showColumns = `
	id::text, title, synopsis, cover_image_url, genres, rating::float8, episode_count,
	studio, status, year, anilist_id, trailer_id, trailer_site, trailer_thumbnail, created_at`

const episodeColumns = `
	id::text, show_id::text, season_number, content_type, episode_number::float8,
	title, thumbnail, duration, file_path, bucket_name, original_title,
	scanner_source, scanner_confidence::float8, created_at`

func scanShow(row scanner) (*Show, error) {
	var sh Show
	err := row.Scan(&sh.ID, &sh.Title, &sh.Synopsis, &sh.CoverImageURL, &sh.Genres, &sh.Rating,
		&sh.EpisodeCount, &sh.Studio, &sh.Status, &sh.Year, &sh.AnilistID,
		&sh.TrailerID, &sh.TrailerSite, &sh.TrailerThumbnail, &sh.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgxErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("repository: scan show: %w", err)
	}
	return &sh, nil
}

func scanEpisode(row scanner) (*Episode, error) {
	var e Episode
	err := row.Scan(&e.ID, &e.ShowID, &e.SeasonNumber, &e.ContentType, &e.EpisodeNumber,
		&e.Title, &e.Thumbnail, &e.Duration, &e.FilePath, &e.BucketName, &e.OriginalTitle,
		&e.ScannerSource, &e.ScannerConfidence, &e.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgxErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("repository: scan episode: %w", err)
	}
	return &e, nil
}

// ListShows returns the catalog page plus the unpaginated total. search matches title, studio
// or synopsis and genre filters the genres array; both are optional. The dedupe-by-identity
// pass belongs to the HTTP layer, not here, so the raw total stays honest.
func (s *Store) ListShows(ctx context.Context, limit, offset int, search, genre string) ([]Show, int64, error) {
	var total int64
	if err := s.pool.QueryRow(ctx, `select count(*) from shows`).Scan(&total); err != nil {
		return nil, 0, err
	}

	var clauses []string
	args := make([]any, 0, 4)
	if trimmed := strings.TrimSpace(search); trimmed != "" {
		args = append(args, "%"+escapeLike(trimmed)+"%")
		clauses = append(clauses, fmt.Sprintf(`(title ilike $%d or studio ilike $%d or synopsis ilike $%d)`,
			len(args), len(args), len(args)))
	}
	if trimmed := strings.TrimSpace(genre); trimmed != "" {
		args = append(args, escapeLike(trimmed))
		clauses = append(clauses, fmt.Sprintf(`$%d = any(genres)`, len(args)))
	}

	where := ""
	if len(clauses) > 0 {
		where = " where " + strings.Join(clauses, " and ")
	}

	args = append(args, limit, offset)
	query := fmt.Sprintf(`select %s from shows%s order by title asc limit $%d offset $%d`,
		showColumns, where, len(args)-1, len(args))

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := make([]Show, 0, 64)
	for rows.Next() {
		var sh Show
		if err := rows.Scan(&sh.ID, &sh.Title, &sh.Synopsis, &sh.CoverImageURL, &sh.Genres, &sh.Rating,
			&sh.EpisodeCount, &sh.Studio, &sh.Status, &sh.Year, &sh.AnilistID,
			&sh.TrailerID, &sh.TrailerSite, &sh.TrailerThumbnail, &sh.CreatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, sh)
	}
	return out, total, rows.Err()
}

// ShowByID loads one show.
func (s *Store) ShowByID(ctx context.Context, id string) (*Show, error) {
	return scanShow(s.pool.QueryRow(ctx, `select `+showColumns+` from shows where id = $1::uuid`, id))
}

// ShowByIDText is ShowByID but tolerant of a non-uuid id, so a malformed path
// parameter reports "not found" instead of a database error.
func (s *Store) ShowByIDText(ctx context.Context, id string) (*Show, error) {
	if !isUUID(id) {
		return nil, ErrNotFound
	}
	return s.ShowByID(ctx, id)
}

// EpisodesForShow returns every episode of a show ordered for playback. The 10000 limit is
// deliberate: a long-running series must not be silently truncated the way the original
// backend's 1000-row default page did.
func (s *Store) EpisodesForShow(ctx context.Context, showID string) ([]Episode, error) {
	rows, err := s.pool.Query(ctx, `
		select `+episodeColumns+` from episodes
		where show_id = $1::uuid
		order by season_number asc, episode_number asc nulls last
		limit 10000`, showID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Episode, 0, 128)
	for rows.Next() {
		var e Episode
		if err := rows.Scan(&e.ID, &e.ShowID, &e.SeasonNumber, &e.ContentType, &e.EpisodeNumber,
			&e.Title, &e.Thumbnail, &e.Duration, &e.FilePath, &e.BucketName, &e.OriginalTitle,
			&e.ScannerSource, &e.ScannerConfidence, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EpisodeByID loads an episode including its storage path, which only the server sees.
func (s *Store) EpisodeByID(ctx context.Context, id string) (*Episode, error) {
	if !isUUID(id) {
		return nil, ErrNotFound
	}
	return scanEpisode(s.pool.QueryRow(ctx, `select `+episodeColumns+` from episodes where id = $1::uuid`, id))
}

// EpisodeExists powers watch-party validation without loading the row.
func (s *Store) EpisodeExists(ctx context.Context, id string) bool {
	if !isUUID(id) {
		return false
	}
	var exists bool
	err := s.pool.QueryRow(ctx, `select exists(select 1 from episodes where id = $1::uuid)`, id).Scan(&exists)
	return err == nil && exists
}

// ShowSummary is the trimmed show projection returned by a rescan.
type ShowSummary struct {
	ID               string   `json:"id"`
	Title            string   `json:"title"`
	Synopsis         *string  `json:"synopsis"`
	CoverImageURL    *string  `json:"cover_image_url"`
	Genres           []string `json:"genres"`
	Rating           *float64 `json:"rating"`
	EpisodeCount     *int     `json:"episode_count"`
	Studio           *string  `json:"studio"`
	Status           *string  `json:"status"`
	Year             *string  `json:"year"`
	TrailerID        *string  `json:"trailer_id"`
	TrailerSite      *string  `json:"trailer_site"`
	TrailerThumbnail *string  `json:"trailer_thumbnail"`
}

// ListShowSummaries returns the whole catalog in title order, which is what a rescan
// response needs so a client can refresh its library in one round trip.
func (s *Store) ListShowSummaries(ctx context.Context) ([]ShowSummary, error) {
	rows, err := s.pool.Query(ctx, `
		select id::text, title, synopsis, cover_image_url, genres, rating::float8, episode_count,
		       studio, status, year, trailer_id, trailer_site, trailer_thumbnail
		from shows order by title asc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ShowSummary, 0, 64)
	for rows.Next() {
		var v ShowSummary
		if err := rows.Scan(&v.ID, &v.Title, &v.Synopsis, &v.CoverImageURL, &v.Genres, &v.Rating,
			&v.EpisodeCount, &v.Studio, &v.Status, &v.Year, &v.TrailerID, &v.TrailerSite, &v.TrailerThumbnail); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// DeleteShow removes a show; its episodes cascade, and any watch party pointing at it
// keeps its row but loses the reference.
func (s *Store) DeleteShow(ctx context.Context, id string) error {
	if !isUUID(id) {
		return ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `delete from shows where id = $1::uuid`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ── Scanner queries ──────────────────────────────────────────────────────────

// ShowByTitle finds a show by case-insensitive exact title.
func (s *Store) ShowByTitle(ctx context.Context, title string) (*Show, error) {
	return scanShow(s.pool.QueryRow(ctx, `select `+showColumns+` from shows where lower(trim(title)) = lower(trim($1)) limit 1`, title))
}

// ShowByAnilistID finds a show already linked to an external entry, which is how the
// scanner avoids creating a duplicate when a release name differs from the official one.
func (s *Store) ShowByAnilistID(ctx context.Context, anilistID int) (*Show, error) {
	return scanShow(s.pool.QueryRow(ctx,
		`select `+showColumns+` from shows where anilist_id = $1 limit 1`, anilistID))
}

// ShowCandidates returns loose title matches for scoring. A release folder rarely
// equals the official title, so the scanner ranks these rather than trusting SQL.
func (s *Store) ShowCandidates(ctx context.Context, title string, limit int) ([]Show, error) {
	// Collapse the title into a per-word wildcard pattern, e.g. "Frieren S1" -> "%frieren%s1%".
	words := strings.Fields(strings.ToLower(title))
	if len(words) == 0 {
		return nil, nil
	}
	pattern := "%" + strings.Join(words, "%") + "%"

	rows, err := s.pool.Query(ctx, `select `+showColumns+` from shows where lower(title) like $1 limit $2`, pattern, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Show, 0, 8)
	for rows.Next() {
		var sh Show
		if err := rows.Scan(&sh.ID, &sh.Title, &sh.Synopsis, &sh.CoverImageURL, &sh.Genres, &sh.Rating,
			&sh.EpisodeCount, &sh.Studio, &sh.Status, &sh.Year, &sh.AnilistID,
			&sh.TrailerID, &sh.TrailerSite, &sh.TrailerThumbnail, &sh.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, sh)
	}
	return out, rows.Err()
}

// CreateShow inserts a show. A unique-violation on title is returned as-is so the
// scanner can re-read the winning row from a concurrent scan.
func (s *Store) CreateShow(ctx context.Context, in Show) (*Show, error) {
	return scanShow(s.pool.QueryRow(ctx, `
		insert into shows (title, synopsis, cover_image_url, genres, rating, episode_count,
		                   studio, status, year, anilist_id, trailer_id, trailer_site, trailer_thumbnail)
		values ($1, $2, $3, coalesce($4::text[], '{}'::text[]), $5, $6, $7, $8, $9, $10, $11, $12, $13)
		returning `+showColumns,
		in.Title, in.Synopsis, in.CoverImageURL, in.Genres, in.Rating, in.EpisodeCount,
		in.Studio, in.Status, in.Year, in.AnilistID, in.TrailerID, in.TrailerSite, in.TrailerThumbnail))
}

// FillShowMetadata writes only the metadata fields that are still empty, so a scan
// never overwrites good data with a weaker source.
func (s *Store) FillShowMetadata(ctx context.Context, id string, in Show) error {
	_, err := s.pool.Exec(ctx, `
		update shows set
			synopsis        = coalesce(synopsis, $2),
			cover_image_url = coalesce(cover_image_url, $3),
			genres          = case when cardinality(genres) = 0 then coalesce($4::text[], '{}'::text[]) else genres end,
			rating          = coalesce(rating, $5),
			episode_count   = coalesce(episode_count, $6),
			studio          = coalesce(studio, $7),
			status          = coalesce(status, $8),
			year            = coalesce(year, $9),
			anilist_id      = coalesce(anilist_id, $10),
			trailer_id        = coalesce(trailer_id, $11),
			trailer_site      = coalesce(trailer_site, $12),
			trailer_thumbnail = coalesce(trailer_thumbnail, $13)
		where id = $1::uuid`,
		id, in.Synopsis, in.CoverImageURL, in.Genres, in.Rating, in.EpisodeCount,
		in.Studio, in.Status, in.Year, in.AnilistID, in.TrailerID, in.TrailerSite, in.TrailerThumbnail)
	return err
}

// UpsertEpisode inserts or replaces one episode keyed by show, season, content type
// and episode number. Re-scanning a library must be idempotent, and a moved file must
// update the row rather than duplicate it.
func (s *Store) UpsertEpisode(ctx context.Context, in Episode) (*Episode, error) {
	return scanEpisode(s.pool.QueryRow(ctx, `
		insert into episodes (show_id, season_number, content_type, episode_number, file_path,
		                      bucket_name, original_title, scanner_source, scanner_confidence)
		values ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9)
		on conflict (show_id, season_number, content_type, episode_number) do update
		set file_path = excluded.file_path,
		    bucket_name = excluded.bucket_name,
		    original_title = coalesce(excluded.original_title, episodes.original_title),
		    scanner_source = coalesce(excluded.scanner_source, episodes.scanner_source),
		    scanner_confidence = coalesce(excluded.scanner_confidence, episodes.scanner_confidence)
		returning `+episodeColumns,
		in.ShowID, in.SeasonNumber, in.ContentType, in.EpisodeNumber, in.FilePath,
		in.BucketName, in.OriginalTitle, in.ScannerSource, in.ScannerConfidence))
}

// EpisodePath is the (id, file_path) pair the scanner needs to detect deletions.
type EpisodePath struct {
	ID       string
	FilePath string
	ShowID   string
}

// ListEpisodePaths returns every indexed file path.
func (s *Store) ListEpisodePaths(ctx context.Context) ([]EpisodePath, error) {
	rows, err := s.pool.Query(ctx, `select id::text, file_path, show_id::text from episodes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]EpisodePath, 0, 256)
	for rows.Next() {
		var p EpisodePath
		if err := rows.Scan(&p.ID, &p.FilePath, &p.ShowID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeleteEpisodesByID removes episodes whose file disappeared.
func (s *Store) DeleteEpisodesByID(ctx context.Context, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	uuids := make([]string, 0, len(ids))
	for _, id := range ids {
		if isUUID(id) {
			uuids = append(uuids, id)
		}
	}
	if len(uuids) == 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, `
		delete from episodes where id::text = any($1)`, uuids)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ShowIDsWithEpisodes returns every show id that still owns at least one episode.
func (s *Store) ShowIDsWithEpisodes(ctx context.Context) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `select distinct show_id::text from episodes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]bool, 256)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// ListShowIDs returns every show id.
func (s *Store) ListShowIDs(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `select id::text from shows`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]string, 0, 128)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// DeleteShowsByID removes shows left with no episodes.
func (s *Store) DeleteShowsByID(ctx context.Context, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, `delete from shows where id::text = any($1)`, ids)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// escapeLike neutralises the wildcards inside a user-supplied search term. Values are always
// passed as parameters, so this is not injection: it stops a literal "%" from turning the
// search into a full table scan.
func escapeLike(value string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
}

// LibraryStats is the size of what this API can serve.
type LibraryStats struct {
	Shows    int64
	Episodes int64
	Users    int64
}

// CountLibrary answers the catalog and account totals in one round trip, which is what makes
// it safe to call from a scrape.
func (s *Store) CountLibrary(ctx context.Context) (LibraryStats, error) {
	var counts LibraryStats
	err := s.pool.QueryRow(ctx, `
		select (select count(*) from shows),
		       (select count(*) from episodes),
		       (select count(*) from animind_users)`).
		Scan(&counts.Shows, &counts.Episodes, &counts.Users)
	if err != nil {
		return LibraryStats{}, fmt.Errorf("repository: count library: %w", err)
	}
	return counts, nil
}

// LibraryCounts reports catalog size for the admin metrics screen.
func (s *Store) LibraryCounts(ctx context.Context) (shows, episodes int64, err error) {
	if err = s.pool.QueryRow(ctx, `select count(*) from shows`).Scan(&shows); err != nil {
		return
	}
	err = s.pool.QueryRow(ctx, `select count(*) from episodes`).Scan(&episodes)
	return
}

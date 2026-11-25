package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Torrent download jobs, one row per user download, scoped by user_id unless an admin helper
// passes all=true. activeStates matches uq_download_jobs_active_hash, so terminal rows retry.
const activeStates = `'queued','fetching_metadata','awaiting_selection','downloading','verifying','importing','ready','paused'`

// DownloadJob is one download_jobs row. IDs are strings, cast with ::text/::uuid.
type DownloadJob struct {
	ID            string     `json:"id"`
	UserID        string     `json:"user_id"`
	ResultTitle   string     `json:"result_title"`
	InfoHash      string     `json:"info_hash"`
	SizeBytes     int64      `json:"size_bytes"`
	State         string     `json:"state"`
	Error         *string    `json:"error"`
	ProgressBytes int64      `json:"progress_bytes"`
	SelectedFiles []int      `json:"selected_files"` // nil when unset
	SavePath      *string    `json:"save_path"`
	ShowID        *string    `json:"show_id"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	CompletedAt   *time.Time `json:"completed_at"`
}

const downloadJobColumns = `
	id::text, user_id::text, result_title, info_hash, size_bytes, state, error,
	progress_bytes, selected_files, save_path, show_id::text, created_at, updated_at, completed_at`

func scanDownloadJob(row pgx.Row) (DownloadJob, error) {
	var j DownloadJob
	// selected_files is nullable jsonb, decoded below like watchlist's anime_data.
	var rawFiles []byte
	err := row.Scan(&j.ID, &j.UserID, &j.ResultTitle, &j.InfoHash, &j.SizeBytes, &j.State, &j.Error,
		&j.ProgressBytes, &rawFiles, &j.SavePath, &j.ShowID, &j.CreatedAt, &j.UpdatedAt, &j.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return DownloadJob{}, ErrNotFound
	}
	if err != nil {
		return DownloadJob{}, fmt.Errorf("repository: scan download job: %w", err)
	}
	if rawFiles != nil {
		if err := json.Unmarshal(rawFiles, &j.SelectedFiles); err != nil {
			return DownloadJob{}, fmt.Errorf("repository: decode selected_files: %w", err)
		}
	}
	return j, nil
}

// selectedFilesValue renders a file selection as jsonb: nil stays NULL, a slice becomes an array.
func selectedFilesValue(files []int) any {
	if files == nil {
		return nil
	}
	// json.Marshal on []int cannot fail; the error is unreachable.
	raw, _ := json.Marshal(files)
	return raw
}

// ── Downloads ────────────────────────────────────────────────────────────────

// CreateDownloadJob inserts a new job; an empty state means "queued". A second live row
// for the same (user, info hash) violates uq_download_jobs_active_hash.
func (s *Store) CreateDownloadJob(ctx context.Context, job DownloadJob) (DownloadJob, error) {
	if job.State == "" {
		job.State = "queued"
	}
	return scanDownloadJob(s.pool.QueryRow(ctx, `
		insert into download_jobs (user_id, result_title, info_hash, size_bytes, state, error,
		                           progress_bytes, selected_files, save_path, show_id)
		values ($1::uuid, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10::uuid)
		returning `+downloadJobColumns,
		job.UserID, job.ResultTitle, job.InfoHash, job.SizeBytes, job.State, job.Error,
		job.ProgressBytes, selectedFilesValue(job.SelectedFiles), job.SavePath, job.ShowID))
}

// GetDownloadJob loads one job by primary key.
func (s *Store) GetDownloadJob(ctx context.Context, id string) (DownloadJob, error) {
	if !isUUID(id) {
		return DownloadJob{}, ErrNotFound
	}
	return scanDownloadJob(s.pool.QueryRow(ctx,
		`select `+downloadJobColumns+` from download_jobs where id = $1::uuid`, id))
}

// FindDownloadJobByHash reports whether the user already has a non-terminal job for this
// torrent — the dedupe check before a new download; terminal states never match.
func (s *Store) FindDownloadJobByHash(ctx context.Context, userID, infoHash string) (DownloadJob, bool, error) {
	job, err := scanDownloadJob(s.pool.QueryRow(ctx, `
		select `+downloadJobColumns+` from download_jobs
		where user_id = $1::uuid and info_hash = $2 and state in (`+activeStates+`)`,
		userID, infoHash))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return DownloadJob{}, false, nil
		}
		return DownloadJob{}, false, err
	}
	return job, true, nil
}

// ListDownloadJobs returns jobs newest first. A non-nil userID scopes to that account;
// with a nil userID only all=true lists across accounts, and an unscoped list returns none.
func (s *Store) ListDownloadJobs(ctx context.Context, userID *string, all bool, limit int) ([]DownloadJob, error) {
	if limit <= 0 {
		limit = 100
	}
	var (
		query string
		args  []any
	)
	switch {
	case userID != nil:
		query = `select ` + downloadJobColumns + ` from download_jobs
			where user_id = $1::uuid
			order by created_at desc
			limit $2`
		args = []any{*userID, limit}
	case all:
		query = `select ` + downloadJobColumns + ` from download_jobs
			order by created_at desc
			limit $1`
		args = []any{limit}
	default:
		return []DownloadJob{}, nil
	}

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]DownloadJob, 0, 16)
	for rows.Next() {
		job, err := scanDownloadJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

// TransitionDownloadJob is a compare-and-set: if the row already left from, nothing
// matches and ErrConflict is returned instead of clobbering the newer state.
func (s *Store) TransitionDownloadJob(ctx context.Context, id, from, to string) (DownloadJob, error) {
	rows, err := s.pool.Query(ctx, `
		update download_jobs set state = $3, updated_at = now()
		where id = $1::uuid and state = $2
		returning `+downloadJobColumns,
		id, from, to)
	if err != nil {
		return DownloadJob{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		// A real database failure must not masquerade as a state conflict.
		if err := rows.Err(); err != nil {
			return DownloadJob{}, err
		}
		return DownloadJob{}, ErrConflict
	}
	return scanDownloadJob(rows)
}

// SetDownloadJobError records the failure code without changing state: the manager
// transitions to failed itself, so error text and state reach watchers separately.
func (s *Store) SetDownloadJobError(ctx context.Context, id, code string) (DownloadJob, error) {
	if !isUUID(id) {
		return DownloadJob{}, ErrNotFound
	}
	return scanDownloadJob(s.pool.QueryRow(ctx, `
		update download_jobs set error = $2, updated_at = now()
		where id = $1::uuid
		returning `+downloadJobColumns,
		id, code))
}

// SetDownloadJobProgress stores the bytes-on-disk counter the progress ticker writes.
func (s *Store) SetDownloadJobProgress(ctx context.Context, id string, progressBytes int64) error {
	if !isUUID(id) {
		return ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `
		update download_jobs set progress_bytes = $2, updated_at = now() where id = $1::uuid`,
		id, progressBytes)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetDownloadJobSelection stores the picked file indexes; a nil slice clears the selection.
func (s *Store) SetDownloadJobSelection(ctx context.Context, id string, files []int) (DownloadJob, error) {
	if !isUUID(id) {
		return DownloadJob{}, ErrNotFound
	}
	return scanDownloadJob(s.pool.QueryRow(ctx, `
		update download_jobs set selected_files = $2::jsonb, updated_at = now()
		where id = $1::uuid
		returning `+downloadJobColumns,
		id, selectedFilesValue(files)))
}

// SetDownloadJobImportResult records the imported path and resolved show, stamping completed_at.
func (s *Store) SetDownloadJobImportResult(ctx context.Context, id, savePath string, showID *string) error {
	if !isUUID(id) {
		return ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `
		update download_jobs
		set save_path = $2, show_id = $3::uuid, completed_at = now(), updated_at = now()
		where id = $1::uuid`, id, savePath, showID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CountActiveDownloadJobs counts jobs in non-terminal states, either one user's or —
// with a nil userID — globally.
func (s *Store) CountActiveDownloadJobs(ctx context.Context, userID *string) (int, error) {
	query := `select count(*) from download_jobs where state in (` + activeStates + `)`
	var args []any
	if userID != nil {
		query += ` and user_id = $1::uuid`
		args = append(args, *userID)
	}
	var n int
	err := s.pool.QueryRow(ctx, query, args...).Scan(&n)
	return n, err
}

// ListResumableDownloadJobs returns every non-terminal job, newest first, so the manager
// can resume after a restart.
func (s *Store) ListResumableDownloadJobs(ctx context.Context) ([]DownloadJob, error) {
	rows, err := s.pool.Query(ctx, `
		select `+downloadJobColumns+` from download_jobs
		where state in (`+activeStates+`)
		order by created_at desc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]DownloadJob, 0, 16)
	for rows.Next() {
		job, err := scanDownloadJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

// DeleteDownloadJob removes a job row; imported media files are handled separately.
func (s *Store) DeleteDownloadJob(ctx context.Context, id string) error {
	if !isUUID(id) {
		return ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `delete from download_jobs where id = $1::uuid`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

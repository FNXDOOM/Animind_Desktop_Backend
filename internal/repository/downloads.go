package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Torrent download jobs: one row per user-initiated download, driven through its
// state machine by internal/downloads.
//
// Every read and write is scoped by user_id except the admin/global helpers, which
// take an explicit nil-user "all" decision from the caller; a list without an owner
// and without all=true returns nothing rather than everything.
//
// The non-terminal state set below is also the WHERE clause of the partial unique
// index uq_download_jobs_active_hash: failed and cancelled rows leave the index, so
// a torrent can be retried after a terminal state.
const activeStates = `'queued','fetching_metadata','awaiting_selection','downloading','verifying','importing','ready','paused'`

// DownloadJob is one download_jobs row. IDs are strings cast with ::text/::uuid,
// matching the rest of the package.
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
	// selected_files is nullable jsonb; it comes back as raw bytes and is decoded
	// below, the same shape watchlist's anime_data uses.
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

// selectedFilesValue renders a file selection as jsonb input: nil stays NULL, and a
// non-nil slice (including an empty one) is stored as a JSON array.
func selectedFilesValue(files []int) any {
	if files == nil {
		return nil
	}
	// json.Marshal on []int cannot fail; the error is unreachable.
	raw, _ := json.Marshal(files)
	return raw
}

// ── Downloads ────────────────────────────────────────────────────────────────

// CreateDownloadJob inserts a new job. An empty state means "queued". A second live
// row for the same (user, info hash) violates uq_download_jobs_active_hash and
// surfaces as a unique violation.
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

// FindDownloadJobByHash reports whether the user already has a job for this torrent in a
// non-terminal state — the dedupe check before a new download starts. Terminal states
// (failed, cancelled) never match, so a torrent can be retried.
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

// ListDownloadJobs returns jobs newest first. A non-nil userID scopes the list to that
// account; with a nil userID only all=true lists across accounts. Neither an owner nor
// all=true returns no rows: there is no legitimate caller for an unscoped user list.
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

// TransitionDownloadJob moves a job from one state to another as a compare-and-set:
// when the row is no longer in the expected state (a user paused or cancelled while the
// worker worked) it matches nothing and ErrConflict is returned instead of clobbering
// the newer state.
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

// SetDownloadJobError records the failure code on a job. It deliberately does not
// change the state: the manager performs the transition to failed itself, so the
// error text and the state change reach watchers as separate steps.
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

// SetDownloadJobSelection stores the file indexes the user picked out of a multi-file
// torrent. A nil slice clears the selection.
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

// SetDownloadJobImportResult records where the imported files landed and which show
// they resolved to, stamping completed_at for the ready transition that follows.
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
// with a nil userID — globally, which is what the quota checks and metrics read.
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

// ListResumableDownloadJobs returns every job in a non-terminal state, newest first.
// After a restart the manager re-enters the loop for each of these rows.
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

// DeleteDownloadJob removes a job row. Cancelled and finished jobs are deleted by the
// manager; imported media files are handled separately by the importer.
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

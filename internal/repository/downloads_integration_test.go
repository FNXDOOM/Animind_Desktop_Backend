package repository

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Download job rows reference animind_users and shows, so the cascade truncate in
// newTestStore clears them between tests; each test seeds its own accounts.

func TestDownloadJobsCRUDAndTransitions(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	user := mustUser(t, store, "downloads@example.com")
	hash := strings.Repeat("a", 40)

	job, err := store.CreateDownloadJob(ctx, DownloadJob{
		UserID: user.ID, ResultTitle: "SubsPlease Frieren 01", InfoHash: hash,
		SizeBytes: 1_000, State: "queued",
	})
	if err != nil || job.State != "queued" {
		t.Fatalf("create: %v %+v", err, job)
	}
	if job.ProgressBytes != 0 || job.SelectedFiles != nil {
		t.Errorf("a new job must start with no progress and no selection: %+v", job)
	}

	// The partial unique index admits exactly one live row per user and torrent.
	if _, err := store.CreateDownloadJob(ctx, DownloadJob{
		UserID: user.ID, ResultTitle: "again", InfoHash: hash,
		SizeBytes: 1, State: "queued",
	}); err == nil {
		t.Fatal("duplicate active hash must violate the partial unique index")
	} else if !IsUniqueViolation(err) {
		t.Errorf("expected a unique violation, got %v", err)
	}

	// The index is scoped to (user_id, info_hash): another account downloading the
	// same torrent gets its own row.
	other := mustUser(t, store, "downloads2@example.com")
	otherJob, err := store.CreateDownloadJob(ctx, DownloadJob{
		UserID: other.ID, ResultTitle: "same torrent, other account", InfoHash: hash,
		SizeBytes: 2_000, State: "queued",
	})
	if err != nil {
		t.Fatalf("same hash for another user must be allowed: %v", err)
	}

	got, err := store.TransitionDownloadJob(ctx, job.ID, "queued", "fetching_metadata")
	if err != nil || got.State != "fetching_metadata" {
		t.Fatalf("transition: %v", err)
	}
	if _, err := store.TransitionDownloadJob(ctx, job.ID, "queued", "downloading"); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong expected state must conflict, got %v", err)
	}
	if err := store.SetDownloadJobProgress(ctx, job.ID, 512); err != nil {
		t.Fatal(err)
	}
	if reloaded, err := store.GetDownloadJob(ctx, job.ID); err != nil || reloaded.ProgressBytes != 512 {
		t.Fatalf("progress: %v %+v", err, reloaded)
	}
	sel, err := store.SetDownloadJobSelection(ctx, job.ID, []int{0, 2})
	if err != nil || len(sel.SelectedFiles) != 2 {
		t.Fatalf("selection: %v", err)
	}

	// Dedupe finds the live row and only the owner's row; terminal states never match.
	found, ok, err := store.FindDownloadJobByHash(ctx, user.ID, hash)
	if err != nil || !ok || found.ID != job.ID {
		t.Fatalf("by hash: ok=%v job=%+v err=%v", ok, found, err)
	}
	otherFound, ok, err := store.FindDownloadJobByHash(ctx, other.ID, hash)
	if err != nil || !ok || otherFound.ID != otherJob.ID {
		t.Fatalf("the other account must find its own row: ok=%v err=%v", ok, err)
	}
	if _, ok, err := store.FindDownloadJobByHash(ctx, user.ID, strings.Repeat("b", 40)); err != nil || ok {
		t.Fatalf("an unused hash must miss, ok=%v err=%v", ok, err)
	}

	listed, err := store.ListDownloadJobs(ctx, &user.ID, false, 10)
	if err != nil || len(listed) != 1 {
		t.Fatalf("list: %v", err)
	}
	everything, err := store.ListDownloadJobs(ctx, nil, true, 10)
	if err != nil || len(everything) != 2 {
		t.Fatalf("admin list: %v (%d rows)", err, len(everything))
	}
	if limited, err := store.ListDownloadJobs(ctx, nil, true, 1); err != nil || len(limited) != 1 {
		t.Fatalf("limit: %v (%d rows)", err, len(limited))
	}
	if scoped, err := store.ListDownloadJobs(ctx, nil, false, 10); err != nil || len(scoped) != 0 {
		t.Fatalf("a list without an owner or all=true must fail closed, got %d rows: %v", len(scoped), err)
	}
	active, err := store.CountActiveDownloadJobs(ctx, &user.ID)
	if err != nil || active != 1 {
		t.Fatalf("count: %v", err)
	}
	if global, err := store.CountActiveDownloadJobs(ctx, nil); err != nil || global != 2 {
		t.Fatalf("global count: %v", err)
	}

	// The importer records where files landed and which show they resolved to,
	// plus the completion stamp.
	show, err := store.CreateShow(ctx, Show{Title: "Frieren", Genres: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetDownloadJobImportResult(ctx, job.ID, "Frieren/Season 1/Frieren - 01.mkv", &show.ID); err != nil {
		t.Fatal(err)
	}
	imported, err := store.GetDownloadJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if imported.SavePath == nil || *imported.SavePath != "Frieren/Season 1/Frieren - 01.mkv" {
		t.Errorf("save_path = %v", imported.SavePath)
	}
	if imported.ShowID == nil || *imported.ShowID != show.ID {
		t.Errorf("show_id = %v", imported.ShowID)
	}
	if imported.CompletedAt == nil {
		t.Error("the import result must record completed_at")
	}
	if len(imported.SelectedFiles) != 2 {
		t.Errorf("selected_files lost on update: %v", imported.SelectedFiles)
	}

	// Recording an error is separate from the failed transition the manager performs.
	if _, err := store.SetDownloadJobError(ctx, job.ID, "disk_full"); err != nil {
		t.Fatal(err)
	}
	errored, err := store.GetDownloadJob(ctx, job.ID)
	if err != nil || errored.Error == nil || *errored.Error != "disk_full" {
		t.Fatalf("error code: %v %+v", err, errored)
	}
	if errored.State != "fetching_metadata" {
		t.Errorf("SetDownloadJobError must not change state, got %q", errored.State)
	}

	resumable, err := store.ListResumableDownloadJobs(ctx)
	if err != nil || len(resumable) != 2 {
		t.Fatalf("resumable: %v (%d rows)", err, len(resumable))
	}
	if _, err := store.TransitionDownloadJob(ctx, job.ID, "fetching_metadata", "failed"); err != nil {
		t.Fatalf("transition to failed: %v", err)
	}
	if active, err := store.CountActiveDownloadJobs(ctx, &user.ID); err != nil || active != 0 {
		t.Errorf("a failed job is not active, got %d/%v", active, err)
	}
	if _, ok, err := store.FindDownloadJobByHash(ctx, user.ID, hash); err != nil || ok {
		t.Errorf("a failed job must not satisfy dedupe, ok=%v err=%v", ok, err)
	}
	if remaining, err := store.ListResumableDownloadJobs(ctx); err != nil || len(remaining) != 1 {
		t.Errorf("only the other account's queued job is resumable: %v (%d rows)", err, len(remaining))
	}

	if err := store.DeleteDownloadJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetDownloadJob(ctx, job.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted job must not be found")
	}
}

func TestCanDownload(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	user := mustUser(t, store, "can-download@example.com")

	if err := store.SetCanDownload(ctx, user.ID, true); err != nil {
		t.Fatal(err)
	}
	// Assert through the admin listing, the same fetch that surfaces the flag.
	assertCanDownload(t, store, user.ID, true)

	if err := store.SetCanDownload(ctx, user.ID, false); err != nil {
		t.Fatal(err)
	}
	assertCanDownload(t, store, user.ID, false)

	// An unknown account must report not found rather than silently succeed.
	if err := store.SetCanDownload(ctx, "00000000-0000-0000-0000-000000000000", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown account must be not found, got %v", err)
	}
}

func assertCanDownload(t *testing.T, store *Store, userID string, want bool) {
	t.Helper()
	users, err := store.ListUsers(context.Background(), 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		if u.ID == userID {
			if u.CanDownload != want {
				t.Errorf("can_download = %v, want %v", u.CanDownload, want)
			}
			return
		}
	}
	t.Fatalf("account %s missing from the admin listing", userID)
}

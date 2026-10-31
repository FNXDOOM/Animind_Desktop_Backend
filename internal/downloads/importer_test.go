package downloads

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"animind-backend-go/internal/config"
	"animind-backend-go/internal/repository"
	"animind-backend-go/internal/scanner"
)

// ── fake scanner ─────────────────────────────────────────────────────────────

// fakeScanner plays scanner.Service: plan[i] is the outcome of call i, nil
// meaning a successful scan; beyond the plan every call succeeds.
type fakeScanner struct {
	mu    sync.Mutex
	plan  []error
	calls int
}

func (s *fakeScanner) Run(context.Context) (*scanner.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.calls
	s.calls++
	if i < len(s.plan) && s.plan[i] != nil {
		return nil, s.plan[i]
	}
	return &scanner.Result{}, nil
}

func (s *fakeScanner) runCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// ── helpers ──────────────────────────────────────────────────────────────────

// seedSource writes a file where the engine would have left a downloaded one:
// <cfg.Dir>/<jobID>/<torrent-relative path>.
func seedSource(t *testing.T, dir, jobID, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, jobID, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newImportManager builds a manager for importer tests with a short scan
// settle window so the retry rule is observable without sleeping. libRoot is
// the real library root, threaded exactly as production wiring must.
func newImportManager(t *testing.T, st store, cfg config.DownloadsConfig, libRoot string, scan scanRunner) *Manager {
	t.Helper()
	m := newManager(st, NewNoopEngine(), cfg, libRoot, scan, nil, diskOK)
	m.scanSettle = 2 * time.Millisecond
	t.Cleanup(m.Close)
	return m
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestImportMovesAndNeverOverwrites(t *testing.T) {
	lib := t.TempDir()
	dir := filepath.Join(lib, ".incoming")
	user := uuid.New()
	id := uuid.New()
	f := newFakeEngine(map[string][]EngineFile{
		id.String(): {
			{Index: 0, Path: "Show - 01.mkv", SizeBytes: 100},
			{Index: 1, Path: "Show - 02.mkv", SizeBytes: 100},
		},
	})
	seedSource(t, dir, id.String(), "Show - 01.mkv", "ep1")
	seedSource(t, dir, id.String(), "Show - 02.mkv", "ep2")

	// an earlier import already owns episode 2's destination
	season := filepath.Join(lib, "Show", "Season 00")
	if err := os.MkdirAll(season, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(season, "Show - 02.mkv"), []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatal(err)
	}

	st := newFakeStore()
	st.queueID(id.String())
	m := newTestManager(t, f, st, testConfig(dir), lib, diskOK, nil)

	if _, err := m.Create(context.Background(), Request{
		UserID: user, Magnet: "magnet:?xt=urn:btih:abc", Title: "Show - 01",
		InfoHash: "abc", SizeBytes: 200,
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "awaiting_selection", func() bool { return st.get(id.String()).State == StateAwaitingSelection })
	if _, err := m.SelectFiles(context.Background(), id.String(), user.String(), false, []int{0, 1}); err != nil {
		t.Fatal(err)
	}
	close(f.handle(id.String()).done)
	waitFor(t, "failed on conflict", func() bool { return st.get(id.String()).State == StateFailed })

	job := st.get(id.String())
	if job.Error == nil || *job.Error != "import_conflict" {
		t.Fatalf("error = %v, want import_conflict", job.Error)
	}
	if job.CompletedAt != nil {
		t.Fatal("completed_at stamped on a conflicted import, want nil")
	}
	moved, err := os.ReadFile(filepath.Join(season, "Show - 01.mkv"))
	if err != nil {
		t.Fatalf("first import did not land: %v", err)
	}
	if string(moved) != "ep1" {
		t.Fatalf("first import content = %q", moved)
	}
	if _, err := os.Stat(filepath.Join(dir, id.String(), "Show - 01.mkv")); !os.IsNotExist(err) {
		t.Fatalf("imported file was copied, not moved: %v", err)
	}
	kept, err := os.ReadFile(filepath.Join(season, "Show - 02.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	if string(kept) != "ORIGINAL" {
		t.Fatalf("existing library file was overwritten: %q", kept)
	}
}

func TestImportResolvesShowAndRescans(t *testing.T) {
	id := uuid.NewString()
	files := []EngineFile{{Index: 0, Path: "Show - 01.mkv", SizeBytes: 4}}

	// run drives one importJob in its own library: ResultTitle "Show - 01"
	// parses to show "Show" with no season, so the destination is Show/Season 00.
	run := func(t *testing.T, plan []error, seed func(st *fakeStore, dest string)) (Imported, error, *fakeStore, *fakeScanner, string) {
		t.Helper()
		lib := t.TempDir()
		cfg := testConfig(filepath.Join(lib, ".incoming"))
		dest := filepath.Join(lib, "Show", "Season 00")
		st := newFakeStore()
		st.seed(repository.DownloadJob{ID: id, State: StateImporting, ResultTitle: "Show - 01", SelectedFiles: []int{0}})
		seedSource(t, cfg.Dir, id, "Show - 01.mkv", "ep1")
		if seed != nil {
			seed(st, dest)
		}
		scan := &fakeScanner{plan: plan}
		m := newImportManager(t, st, cfg, lib, scan)
		job, err := st.GetDownloadJob(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		imported, err := m.importJob(context.Background(), job, files, []int{0})
		return imported, err, st, scan, dest
	}

	t.Run("already running scan is retried exactly once", func(t *testing.T) {
		imported, err, st, scan, dest := run(t, []error{scanner.ErrAlreadyRunning, nil}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if n := scan.runCalls(); n != 2 {
			t.Fatalf("scan called %d times, want exactly the one retry", n)
		}
		if imported.Destination != dest {
			t.Fatalf("destination = %q, want %q", imported.Destination, dest)
		}
		if _, err := os.Stat(filepath.Join(dest, "Show - 01.mkv")); err != nil {
			t.Fatalf("imported file missing: %v", err)
		}
		got := st.get(id)
		if got.SavePath == nil || *got.SavePath != dest {
			t.Fatalf("save_path = %v, want %q", got.SavePath, dest)
		}
		if got.CompletedAt == nil {
			t.Fatal("completed_at not stamped")
		}
		if got.ShowID != nil {
			t.Fatalf("show_id = %v with nothing seeded, want nil", got.ShowID)
		}
	})

	t.Run("title match resolves the show when no path matches", func(t *testing.T) {
		_, err, st, _, _ := run(t, nil, func(st *fakeStore, _ string) {
			st.seedShow("show", "title-row")
		})
		if err != nil {
			t.Fatal(err)
		}
		got := st.get(id)
		if got.ShowID == nil || *got.ShowID != "title-row" {
			t.Fatalf("show_id = %v, want title-row", got.ShowID)
		}
	})

	t.Run("imported path match wins over the title", func(t *testing.T) {
		_, err, st, _, _ := run(t, nil, func(st *fakeStore, dest string) {
			st.seedShow("show", "title-row")
			st.seedEpisodePath(filepath.Join(dest, "Show - 01.mkv"), "path-row")
		})
		if err != nil {
			t.Fatal(err)
		}
		got := st.get(id)
		if got.ShowID == nil || *got.ShowID != "path-row" {
			t.Fatalf("show_id = %v, want path-row", got.ShowID)
		}
	})

	t.Run("a failed scan fails the import", func(t *testing.T) {
		_, err, _, scan, _ := run(t, []error{errors.New("scan boom")}, nil)
		if err == nil {
			t.Fatal("scan failure must fail the import")
		}
		if n := scan.runCalls(); n != 1 {
			t.Fatalf("scan called %d times, want 1 (no retry for other errors)", n)
		}
	})

	t.Run("a missing source file fails the move", func(t *testing.T) {
		lib := t.TempDir()
		cfg := testConfig(filepath.Join(lib, ".incoming"))
		st := newFakeStore()
		job := st.seed(repository.DownloadJob{ID: id, State: StateImporting, ResultTitle: "Show - 01", SelectedFiles: []int{0}})
		scan := &fakeScanner{}
		m := newImportManager(t, st, cfg, lib, scan)
		if _, err := m.importJob(context.Background(), job, files, []int{0}); err == nil {
			t.Fatal("missing source must fail the import")
		}
	})
}

func TestRemoveImportedIsContained(t *testing.T) {
	lib := t.TempDir()
	cfg := testConfig(filepath.Join(lib, ".incoming"))
	scan := &fakeScanner{}
	m := newImportManager(t, newFakeStore(), cfg, lib, scan)
	ctx := context.Background()

	t.Run("contained file is removed and one scan runs", func(t *testing.T) {
		p := filepath.Join(lib, "Show", "Season 00", "Show - 01.mkv")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := m.removeImported(ctx, []string{p}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("file survived removal: %v", err)
		}
		if n := scan.runCalls(); n != 1 {
			t.Fatalf("scan calls = %d, want 1", n)
		}
	})

	t.Run("path outside the library errors without scanning", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "outside.mkv")
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := m.removeImported(ctx, []string{p}); err == nil {
			t.Fatal("a path outside the library must be refused")
		}
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("outside file was touched: %v", err)
		}
		if n := scan.runCalls(); n != 1 {
			t.Fatalf("scan ran on a refusal: %d calls", n)
		}
	})

	t.Run("symlink escaping the library is refused", func(t *testing.T) {
		outside := filepath.Join(t.TempDir(), "real.mkv")
		if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(lib, "link.mkv")
		if err := os.Symlink(outside, link); err != nil {
			t.Skipf("symlinks unavailable on this host: %v", err)
		}
		if err := m.removeImported(ctx, []string{link}); err == nil {
			t.Fatal("an escaping symlink must be refused")
		}
		if _, err := os.Stat(outside); err != nil {
			t.Fatalf("symlink target was touched: %v", err)
		}
		if n := scan.runCalls(); n != 1 {
			t.Fatalf("scan ran on a refusal: %d calls", n)
		}
	})

	t.Run("missing path is a no-op", func(t *testing.T) {
		p := filepath.Join(lib, "Show", "Season 00", "gone.mkv")
		if err := m.removeImported(ctx, []string{p}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestImportRefusesSymlinkedSource(t *testing.T) {
	// The import-time symlink defense: a manifest entry whose on-disk source
	// resolves outside the job directory would move the link or copy its
	// target's content into the library, so the import fails closed — before
	// anything moves and before any destination folder exists.
	t.Run("symlinked file entry", func(t *testing.T) {
		lib := t.TempDir()
		cfg := testConfig(filepath.Join(lib, ".incoming"))
		id := uuid.NewString()
		seedSource(t, cfg.Dir, id, "Show - 01.mkv", "ep1")
		seedSource(t, cfg.Dir, id, "Show - 02.mkv", "ep2")
		outside := filepath.Join(t.TempDir(), "outside.mkv")
		if err := os.WriteFile(outside, []byte("hostile"), 0o644); err != nil {
			t.Fatal(err)
		}
		// episode 1's on-disk entry is a link to a file outside the job dir
		link := filepath.Join(cfg.Dir, id, "Show - 01.mkv")
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, link); err != nil {
			t.Skipf("symlinks unavailable on this host: %v", err)
		}
		st := newFakeStore()
		job := st.seed(repository.DownloadJob{ID: id, State: StateImporting, ResultTitle: "Show - 01", SelectedFiles: []int{0, 1}})
		m := newImportManager(t, st, cfg, lib, &fakeScanner{})
		files := []EngineFile{
			{Index: 0, Path: "Show - 01.mkv", SizeBytes: 100},
			{Index: 1, Path: "Show - 02.mkv", SizeBytes: 100},
		}
		if _, err := m.importJob(context.Background(), job, files, []int{0, 1}); !errors.Is(err, errUnsafePath) {
			t.Fatalf("want errUnsafePath, got %v", err)
		}
		// fail-closed: the clean episode never left the job dir...
		if _, serr := os.Stat(filepath.Join(cfg.Dir, id, "Show - 02.mkv")); serr != nil {
			t.Fatalf("clean source moved despite the refusal: %v", serr)
		}
		// ...and no destination folder was created
		if _, serr := os.Stat(filepath.Join(lib, "Show")); !os.IsNotExist(serr) {
			t.Fatalf("library destination created despite the refusal: %v", serr)
		}
	})

	t.Run("symlinked directory component", func(t *testing.T) {
		lib := t.TempDir()
		cfg := testConfig(filepath.Join(lib, ".incoming"))
		id := uuid.NewString()
		// the manifest path runs through a job-dir folder that is itself a
		// link to a directory outside the job dir
		outsideDir := filepath.Join(t.TempDir(), "real")
		if err := os.MkdirAll(outsideDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outsideDir, "Show - 01.mkv"), []byte("hostile"), 0o644); err != nil {
			t.Fatal(err)
		}
		seedSource(t, cfg.Dir, id, "Show - 02.mkv", "ep2")
		if err := os.Symlink(outsideDir, filepath.Join(cfg.Dir, id, "pack")); err != nil {
			t.Skipf("symlinks unavailable on this host: %v", err)
		}
		st := newFakeStore()
		job := st.seed(repository.DownloadJob{ID: id, State: StateImporting, ResultTitle: "Show - 01", SelectedFiles: []int{0, 1}})
		m := newImportManager(t, st, cfg, lib, &fakeScanner{})
		files := []EngineFile{
			{Index: 0, Path: "pack/Show - 01.mkv", SizeBytes: 100},
			{Index: 1, Path: "Show - 02.mkv", SizeBytes: 100},
		}
		if _, err := m.importJob(context.Background(), job, files, []int{0, 1}); !errors.Is(err, errUnsafePath) {
			t.Fatalf("want errUnsafePath, got %v", err)
		}
		if _, serr := os.Stat(filepath.Join(cfg.Dir, id, "Show - 02.mkv")); serr != nil {
			t.Fatalf("clean source moved despite the refusal: %v", serr)
		}
	})
}

func TestImportResumesIdempotentlyAfterCrash(t *testing.T) {
	// A crash between the final move and SetDownloadJobImportResult leaves the
	// file in the library, the source gone, and the row still importing. The
	// re-driven import must treat that shape as already-imported and reach
	// ready — not mis-fail with import_conflict and block retry forever.
	lib := t.TempDir()
	dir := filepath.Join(lib, ".incoming")
	user := uuid.New()
	id := uuid.New()
	f := newFakeEngine(map[string][]EngineFile{
		id.String(): {{Index: 0, Path: "Show - 01.mkv", SizeBytes: 100}},
	})
	// the destination is occupied by the crashed import and the source is gone
	season := filepath.Join(lib, "Show", "Season 00")
	if err := os.MkdirAll(season, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(season, "Show - 01.mkv"), []byte("ep1"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := newFakeStore()
	st.seed(repository.DownloadJob{
		ID: id.String(), UserID: user.String(), InfoHash: "h1",
		State: StateImporting, ResultTitle: "Show - 01", SelectedFiles: []int{0},
	})
	m := newTestManager(t, f, st, testConfig(dir), lib, diskOK, nil)
	if err := m.ResumeFromStore(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "ready", func() bool { return st.get(id.String()).State == StateReady })
	job := st.get(id.String())
	if job.Error != nil {
		t.Fatalf("resumed import errored: %s", *job.Error)
	}
	if job.SavePath == nil || *job.SavePath != season {
		t.Fatalf("save_path = %v, want %q", job.SavePath, season)
	}
	if job.CompletedAt == nil {
		t.Fatal("completed_at not stamped on the resumed import")
	}
	kept, err := os.ReadFile(filepath.Join(season, "Show - 01.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	if string(kept) != "ep1" {
		t.Fatalf("already-imported file was disturbed: %q", kept)
	}
}

package downloads

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"animind-backend-go/internal/repository"
	"animind-backend-go/internal/scanner"
)

// Imported reports where a job's files landed and which show they resolved to.
// ShowID is a string because download_jobs.show_id is carried as text through
// the repository seam.
type Imported struct {
	ShowTitle   string
	Destination string
	ShowID      *string
	Paths       []string
}

var (
	// errImportConflict answers the spec's import_conflict code: the
	// destination is occupied and imports never overwrite.
	errImportConflict = errors.New("downloads: import conflict")
	// errNothingSelected marks an importing row with no files picked; moving
	// nothing would stamp an empty season folder onto the library.
	errNothingSelected = errors.New("downloads: no files selected for import")
	// errNoLibraryRoot marks a manager wired without the real library root:
	// guessing one from DOWNLOADS_DIR is unsound, so the import refuses.
	errNoLibraryRoot = errors.New("downloads: library root not configured")
)

// showResolver answers the two lookups behind show_id resolution, in order:
// an episode that already indexes an imported path wins, then a
// case-insensitive title match.
type showResolver interface {
	ShowByEpisodePath(ctx context.Context, path string) (string, bool, error)
	ShowByTitle(ctx context.Context, title string) (*repository.Show, error)
}

// ShowByEpisodePath has no store query yet — Task 10 adds the episode-by-path
// lookup, and until then the seam reports unresolved rather than inventing SQL
// outside the repository package.
func (dbStore) ShowByEpisodePath(context.Context, string) (string, bool, error) {
	return "", false, nil
}

// dbStore answers both resolver lookups: ShowByTitle promoted from the store,
// ShowByEpisodePath stubbed above.
var _ showResolver = dbStore{}

// seasonDir renders the spec §7.5 destination folder; "Season 00" collects
// specials and everything the release name never pinned to a season.
func seasonDir(season *int) string {
	n := 0
	if season != nil {
		n = *season
	}
	return fmt.Sprintf("Season %02d", n)
}

// sanitizeTitle makes a show title safe as a library folder name: the
// characters Windows reserves in names, control runes and the edges are
// stripped (trailing dots included — "Show." is an invalid Windows name).
func sanitizeTitle(title string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return -1
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, title)
	return strings.TrimRight(strings.TrimSpace(cleaned), ".")
}

// defaultScanSettle bounds the wait for an in-flight scan before the single
// retry; cron and webhook scans are exactly the ErrAlreadyRunning case.
const defaultScanSettle = 30 * time.Second

// runScan triggers one library scan: when a scan is already running, wait one
// settle window and retry exactly once, a second refusal failing the import.
// The scanner exposes no completion signal, so the settle window is the wait.
func (m *Manager) runScan(ctx context.Context) error {
	_, err := m.scanner.Run(ctx)
	if !errors.Is(err, scanner.ErrAlreadyRunning) {
		return err
	}
	settle := m.scanSettle
	if settle <= 0 {
		settle = defaultScanSettle
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(settle):
	}
	_, err = m.scanner.Run(ctx)
	return err
}

// importJob moves the selected files into <library>/<Show>/Season NN, rescans
// and records the result. The library root comes from the constructor, never
// guessed from DOWNLOADS_DIR — Task 1 only guarantees it lies under the root.
func (m *Manager) importJob(ctx context.Context, job repository.DownloadJob, files []EngineFile, selected []int) (Imported, error) {
	if m.libraryRoot == "" {
		return Imported{}, errNoLibraryRoot
	}
	if len(selected) == 0 {
		return Imported{}, errNothingSelected
	}
	// the selection rules run again over the full manifest: a hostile name
	// fails the job before anything moves, and junk rides out of the pick
	pickable, err := SelectableFiles(files, m.cfg)
	if err != nil {
		return Imported{}, err
	}
	wanted := make(map[int]bool, len(selected))
	for _, i := range selected {
		wanted[i] = true
	}
	byIndex := make(map[int]EngineFile, len(files))
	for _, f := range files {
		byIndex[f.Index] = f
	}
	var moving []EngineFile
	for _, i := range pickable {
		if wanted[i] {
			moving = append(moving, byIndex[i])
		}
	}

	// Fail-closed symlink defense, run before anything moves and before any
	// destination directory is created: every selected source must resolve
	// inside the job directory, and one escape fails the whole import.
	jobDir := filepath.Join(m.cfg.Dir, job.ID)
	for _, f := range moving {
		if err := ensureSourceInsideJobDir(jobDir, filepath.Join(jobDir, f.Path)); err != nil {
			return Imported{}, err
		}
	}

	parsed := scanner.ParseTorrentTitle(job.ResultTitle)
	show := parsed.Show
	if show == "" {
		// a torrent name without an episode parses to nothing; the raw name is
		// still the best folder hint
		show = job.ResultTitle
	}
	showDir := sanitizeTitle(show)
	if showDir == "" || windowsDeviceName(showDir) {
		return Imported{}, fmt.Errorf("downloads: unusable show title %q", job.ResultTitle)
	}
	dest := filepath.Join(m.libraryRoot, showDir, seasonDir(parsed.Season))
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return Imported{}, fmt.Errorf("downloads: create destination: %w", err)
	}

	paths := make([]string, 0, len(moving))
	for _, f := range moving {
		src := filepath.Join(jobDir, f.Path)
		dst, err := moveInto(dest, src)
		if err != nil {
			return Imported{}, err
		}
		paths = append(paths, dst)
	}

	if m.scanner != nil {
		if err := m.runScan(ctx); err != nil {
			return Imported{}, fmt.Errorf("downloads: library scan: %w", err)
		}
	}
	showID := m.resolveShow(ctx, paths, show)
	if err := m.store.SetDownloadJobImportResult(ctx, job.ID, dest, showID); err != nil {
		return Imported{}, fmt.Errorf("downloads: record import result: %w", err)
	}
	return Imported{ShowTitle: showDir, Destination: dest, ShowID: showID, Paths: paths}, nil
}

// resolveShow prefers the show that already indexes an imported path, then a
// case-insensitive title match on the parsed show; nil leaves the column null
// for the next scan to fill.
func (m *Manager) resolveShow(ctx context.Context, paths []string, show string) *string {
	if m.shows == nil {
		return nil
	}
	for _, p := range paths {
		if id, ok, err := m.shows.ShowByEpisodePath(ctx, p); err == nil && ok {
			return &id
		}
	}
	if show == "" {
		return nil
	}
	row, err := m.shows.ShowByTitle(ctx, show)
	if err != nil || row == nil {
		return nil
	}
	id := row.ID
	return &id
}

// ensureSourceInsideJobDir is the import-time symlink defense: every src
// component resolves, via EvalSymlinks, inside the resolved job dir — else a
// link would move, or have its target copied, into the library.
func ensureSourceInsideJobDir(jobDir, src string) error {
	resJobDir, err := filepath.EvalSymlinks(jobDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // nothing was downloaded under this job; moveInto reports it
		}
		return fmt.Errorf("downloads: resolve job dir: %w", err)
	}
	resSrc, err := filepath.EvalSymlinks(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // gone: an already-moved resume or a missing file; moveInto decides
		}
		return fmt.Errorf("downloads: resolve %s: %w", src, err)
	}
	if !withinDir(resJobDir, resSrc) {
		return fmt.Errorf("%w: %s resolves outside the job directory", errUnsafePath, src)
	}
	return nil
}

// moveInto lands one file without ever overwriting. A present dst with src
// still on disk is import_conflict; dst alone is the shape a crash between the
// last move and SetDownloadJobImportResult leaves, so it reads as
// already-imported. Rename first — the roots share a volume by config.
func moveInto(dstDir, src string) (string, error) {
	dst := filepath.Join(dstDir, filepath.Base(src))
	if _, err := os.Lstat(dst); err == nil {
		if _, serr := os.Lstat(src); os.IsNotExist(serr) {
			return dst, nil // already imported by the attempt the crash interrupted
		}
		return "", errImportConflict
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("downloads: stat destination: %w", err)
	}
	if err := os.Rename(src, dst); err == nil {
		return dst, nil
	}
	// rename can refuse across devices: stage a copy as a temp sibling, then
	// swap it into place; the deferred remove is a no-op once it lands
	tmp, err := os.CreateTemp(dstDir, ".import-*")
	if err != nil {
		return "", fmt.Errorf("downloads: staging file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := copyFileContents(tmp, src); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("downloads: staging file: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return "", fmt.Errorf("downloads: stage permissions: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return "", fmt.Errorf("downloads: move into library: %w", err)
	}
	if err := os.Remove(src); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("downloads: remove copied source: %w", err)
	}
	return dst, nil
}

func copyFileContents(dst *os.File, srcPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("downloads: open source: %w", err)
	}
	defer src.Close()
	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("downloads: copy: %w", err)
	}
	return nil
}

// removeImported deletes exactly the files a job imported — every path must
// still resolve inside the library root the constructor was given — and
// rescans once so the pruned episodes leave the catalog (spec §7.5).
func (m *Manager) removeImported(ctx context.Context, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	if m.libraryRoot == "" {
		return errNoLibraryRoot
	}
	root := m.libraryRoot
	for _, p := range paths {
		inside, err := containedInLibrary(root, p)
		if err != nil {
			return fmt.Errorf("downloads: resolve %s: %w", p, err)
		}
		if !inside {
			return fmt.Errorf("downloads: %s is outside the library root %s", p, root)
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("downloads: remove %s: %w", p, err)
		}
	}
	if m.scanner == nil {
		return nil
	}
	return m.runScan(ctx)
}

// containedInLibrary reports whether path sits under root and still resolves
// there after symlinks are followed; a link out is rejected, not followed. A
// missing path counts as contained — removing it is a no-op, not an escape.
func containedInLibrary(root, path string) (bool, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false, err
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}
	if !withinDir(absRoot, absPath) {
		return false, nil
	}
	resRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return false, err
	}
	resPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	return withinDir(resRoot, resPath), nil
}

// withinDir is the strict child-of test: a file is never the root itself.
func withinDir(root, path string) bool {
	return path != root && strings.HasPrefix(path, root+string(os.PathSeparator))
}

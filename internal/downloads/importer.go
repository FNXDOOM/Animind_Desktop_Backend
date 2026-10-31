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
)

// showResolver answers the two lookups behind show_id resolution: an episode
// that already indexes an imported path wins, then a case-insensitive title
// match. *repository.Store supplies the title lookup through dbStore; the
// in-memory test fake provides both.
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

// runScan triggers one library scan, honoring the spec's retry rule: when a
// scan is already running, wait one settle window and retry exactly once — a
// second refusal fails the import. The scanner exposes no completion signal,
// so the settle window is the wait.
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

// importJob moves the selected files into the library under
// <library>/<Show>/Season NN, rescans, and records the import result.
//
// The library root is DOWNLOADS_DIR's parent: Task 1's containment rule pins
// DOWNLOADS_DIR inside the library root (config refuses anything else), so the
// parent directory is the library root by construction.
func (m *Manager) importJob(ctx context.Context, job repository.DownloadJob, files []EngineFile, selected []int) (Imported, error) {
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
	dest := filepath.Join(filepath.Dir(m.cfg.Dir), showDir, seasonDir(parsed.Season))
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return Imported{}, fmt.Errorf("downloads: create destination: %w", err)
	}

	paths := make([]string, 0, len(moving))
	for _, f := range moving {
		src := filepath.Join(m.cfg.Dir, job.ID, f.Path)
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

// moveInto lands one file inside dstDir without ever overwriting: an existing
// destination is the spec's import_conflict. Rename first — the download root
// and the library share a volume by config — falling back to copy+delete for
// the day they don't share one.
func moveInto(dstDir, src string) (string, error) {
	dst := filepath.Join(dstDir, filepath.Base(src))
	if _, err := os.Lstat(dst); err == nil {
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
// still resolve inside the library root — and rescans once so the pruned
// episodes leave the catalog (spec §7.5).
func (m *Manager) removeImported(ctx context.Context, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	root := filepath.Dir(m.cfg.Dir)
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
// there once symlinks are followed; a link pointing outside is rejected rather
// than followed. A missing path counts as contained: removal of it is a no-op,
// not an escape.
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

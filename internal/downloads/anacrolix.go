package downloads

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/storage"
	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"animind-backend-go/internal/config"
)

// metadataTimeout bounds how long Add waits for the swarm to deliver the
// metainfo before the job is dropped and ErrMetadataTimeout returned.
const metadataTimeout = 120 * time.Second

// donePollInterval is how often the Done() watcher samples file completion.
const donePollInterval = 500 * time.Millisecond

// magnetPattern accepts only v1 btih magnet URIs with xt as the first
// parameter; anything else is rejected before the client sees it.
var magnetPattern = regexp.MustCompile(`^magnet:\?xt=urn:btih:[0-9A-Za-z]{32,40}(&[^\s]*)*$`)

// The adapter is the contract: these fail the build if upstream drifts.
var (
	_ Engine        = (*AnacrolixEngine)(nil)
	_ TorrentHandle = (*anacrolixHandle)(nil)
)

// AnacrolixEngine adapts github.com/anacrolix/torrent to Engine: one client
// for every job, each torrent storing under <Dir>/<jobID> via its own
// AddTorrentOpts.Storage, never the client DataDir. Upstream keys torrents by
// infohash, so a second live job for the same hash is refused, not shared.
type AnacrolixEngine struct {
	cfg config.DownloadsConfig
	cli *torrent.Client
	mu  sync.Mutex
	// jobs maps a job to its live handle; guarded by mu.
	jobs map[uuid.UUID]*anacrolixHandle
}

// NewAnacrolixEngine builds the client from cfg's DHT/PEX/UTP knobs. UPnP/NAT
// port mapping is hard-disabled — the engine must never punch holes in the
// operator's network — though local TCP/UDP ports are still bound.
func NewAnacrolixEngine(cfg config.DownloadsConfig) (*AnacrolixEngine, error) {
	cli, err := torrent.NewClient(newClientConfig(cfg))
	if err != nil {
		return nil, fmt.Errorf("downloads: new client: %w", err)
	}
	return &AnacrolixEngine{cfg: cfg, cli: cli, jobs: map[uuid.UUID]*anacrolixHandle{}}, nil
}

// newClientConfig builds the client configuration, split out so tests can
// assert the config contract without constructing a client, which binds ports.
func newClientConfig(cfg config.DownloadsConfig) *torrent.ClientConfig {
	cliConf := torrent.NewDefaultClientConfig()
	cliConf.DataDir = cfg.Dir
	cliConf.DisableTrackers = false
	cliConf.NoDHT = !cfg.DHT
	cliConf.DisablePEX = !cfg.PEX
	cliConf.DisableUTP = !cfg.UTP
	// No port mapping: without this upstream launches go cl.forwardPort() and
	// may add a UPnP/NAT-PMP mapping on the operator's router.
	cliConf.NoDefaultPortForwarding = true
	// Empty host = wildcard per network: "0.0.0.0" fails upstream's tcp6 listen
	// on dual-stack hosts. Ephemeral ports can land in a Windows excluded range
	// and kill startup (measured: udp4 :64437), hence DOWNLOADS_LISTEN_ADDR.
	addr := cfg.ListenAddr
	if addr == "" {
		addr = ":0"
	}
	cliConf.SetListenAddr(addr)
	// Seeding continues after completion; the manager stops it at the
	// configured ratio/minutes.
	cliConf.Seed = true
	// Upstream: DownloadRateLimiter nil means no limiting; UploadRateLimiter is
	// dereferenced unconditionally, so an unlimited upload keeps the default
	// Inf limiter instead of nil.
	if cfg.DownKbps != 0 {
		cliConf.DownloadRateLimiter = limiter(cfg.DownKbps)
	}
	if cfg.UpKbps != 0 {
		cliConf.UploadRateLimiter = limiter(cfg.UpKbps)
	}
	return cliConf
}

// Add resolves the magnet and blocks until metadata is available, the 120s
// engine timeout expires, or ctx is done. Files are never downloaded here;
// that starts when the manager calls Select.
func (e *AnacrolixEngine) Add(ctx context.Context, jobID uuid.UUID, magnet string) (TorrentHandle, error) {
	if !magnetPattern.MatchString(magnet) {
		return nil, fmt.Errorf("downloads: not a magnet")
	}
	jobDir := filepath.Join(e.cfg.Dir, jobID.String())
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		return nil, fmt.Errorf("downloads: create job dir: %w", err)
	}
	// Every failure before the job is registered must remove the placeholder
	// dir again: the job never started, so nothing may be left on disk under
	// <Dir>/<jobID>. (The timeout/ctx paths below use abandon for the same.)
	spec, err := torrent.TorrentSpecFromMagnetUri(magnet)
	if err != nil {
		_ = os.Remove(jobDir)
		return nil, fmt.Errorf("downloads: parse magnet: %w", err)
	}
	// Per-job storage root: everything this job writes stays under jobDir.
	spec.Storage = storage.NewFile(jobDir)
	t, isNew, err := e.cli.AddTorrentSpec(spec)
	if err != nil {
		// Upstream already dropped its own newly-created torrent on this error.
		_ = os.Remove(jobDir)
		return nil, fmt.Errorf("downloads: add torrent: %w", err)
	}
	if !isNew {
		// t belongs to the other live job; only the placeholder dir is ours.
		_ = os.Remove(jobDir)
		return nil, fmt.Errorf("downloads: infohash %s already active in another job", t.InfoHash().HexString())
	}
	select {
	case <-t.GotInfo():
	case <-time.After(metadataTimeout):
		e.abandon(t, jobDir)
		return nil, ErrMetadataTimeout
	case <-ctx.Done():
		e.abandon(t, jobDir)
		return nil, ctx.Err()
	}
	// Nothing may download before Select: enforce the contract by construction
	// instead of relying on upstream's initial priorities happening to be none.
	for _, f := range t.Files() {
		f.SetPriority(torrent.PiecePriorityNone)
	}
	h := newAnacrolixHandle(e, jobID, jobDir, t)
	e.mu.Lock()
	e.jobs[jobID] = h
	e.mu.Unlock()
	return h, nil
}

// Close shuts the client down; upstream closes every torrent and its per-job
// storage as part of this.
func (e *AnacrolixEngine) Close() error {
	errs := e.cli.Close() // upstream returns []error, not error
	e.mu.Lock()
	e.jobs = map[uuid.UUID]*anacrolixHandle{}
	e.mu.Unlock()
	return errors.Join(errs...)
}

// abandon drops a torrent whose metadata never arrived. os.Remove only
// succeeds on an empty dir, so it cleans up the placeholder without ever
// deleting data pieces that made it to disk.
func (e *AnacrolixEngine) abandon(t *torrent.Torrent, jobDir string) {
	t.Drop()
	_ = os.Remove(jobDir)
}

// limiter converts a kbps knob into upstream's *rate.Limiter fields. A nil
// return is safe for DownloadRateLimiter (upstream documents nil as unlimited)
// but not for UploadRateLimiter, which callers must never assign.
func limiter(kbps int64) *rate.Limiter {
	if kbps == 0 {
		return nil
	}
	return rate.NewLimiter(rate.Limit(float64(kbps)*1024), 64<<10)
}

// anacrolixHandle drives one torrent. File indexes are stable: the file list
// is captured (sorted by path) once at metadata time and reused everywhere.
type anacrolixHandle struct {
	e     *AnacrolixEngine
	jobID uuid.UUID
	dir   string
	t     *torrent.Torrent

	// files is sorted by path so Index means the same thing across
	// Files/Select/manager restarts. Read-only after construction.
	files []*torrent.File

	selMu    sync.Mutex
	selected map[int]bool // engine indexes chosen for download; nil until Select

	statsMu    sync.Mutex
	statsAt    time.Time
	statsRead  int64 // last BytesReadData sample
	statsWrote int64 // last BytesWrittenData sample
	downKbps   int64
	upKbps     int64

	doneOnce sync.Once
	done     chan struct{}
}

func newAnacrolixHandle(e *AnacrolixEngine, jobID uuid.UUID, dir string, t *torrent.Torrent) *anacrolixHandle {
	h := &anacrolixHandle{
		e:     e,
		jobID: jobID,
		dir:   dir,
		t:     t,
		files: t.Files(),
	}
	// SliceStable: a malformed metainfo could contain duplicate paths, and ties
	// must not reshuffle indexes. (Contract pinned in anacrolix_paths_test.go.)
	sortFilesByPath(h.files)
	return h
}

// engineFileElement is the minimal upstream-file view the EngineFile mapping
// needs: torrent.File has unexported fields, no constructor, and its
// BytesCompleted needs a live client, so tests use synthetic files here.
type engineFileElement interface {
	Path() string
	Length() int64
	BytesCompleted() int64
}

// sortFilesByPath orders the file list by path with SliceStable so Index means
// the same thing across Files/Select/manager restarts, and a malformed
// metainfo's duplicate paths cannot reshuffle indexes.
func sortFilesByPath[T engineFileElement](files []T) {
	sort.SliceStable(files, func(i, j int) bool {
		return files[i].Path() < files[j].Path()
	})
}

// engineFiles maps upstream files, in the handle's sorted order, to
// EngineFiles. Path(), not DisplayPath(): DisplayPath drops the torrent-name
// directory upstream stores files under, so import paths would not resolve on
// disk. Clean matches the platform separator and guards malformed metainfo.
func engineFiles[T engineFileElement](files []T) []EngineFile {
	out := make([]EngineFile, 0, len(files))
	for i, f := range files {
		out = append(out, EngineFile{
			Index:     i,
			Path:      filepath.Clean(f.Path()),
			SizeBytes: f.Length(),
			Progress:  f.BytesCompleted(),
		})
	}
	return out
}

// Files reports every file in the torrent, indexed in the handle's stable
// sorted-by-path order; see engineFiles for the Path contract.
func (h *anacrolixHandle) Files() ([]EngineFile, error) {
	if h.t.Info() == nil {
		return nil, errors.New("downloads: torrent metadata not available")
	}
	return engineFiles(h.files), nil
}

// Select downloads only the given indexes: selected files are raised to normal
// priority, everything else is set to never-download. It validates the whole
// selection before touching any priority.
func (h *anacrolixHandle) Select(indexes []int) error {
	if h.t.Info() == nil {
		return errors.New("downloads: torrent metadata not available")
	}
	if len(indexes) == 0 {
		return errors.New("downloads: empty file selection")
	}
	sel := make(map[int]bool, len(indexes))
	for _, i := range indexes {
		if i < 0 || i >= len(h.files) {
			return fmt.Errorf("downloads: file index %d out of range", i)
		}
		sel[i] = true
	}
	for i, f := range h.files {
		if sel[i] {
			f.SetPriority(torrent.PiecePriorityNormal)
		} else {
			f.SetPriority(torrent.PiecePriorityNone)
		}
	}
	h.selMu.Lock()
	h.selected = sel
	h.selMu.Unlock()
	return nil
}

// Stats samples the torrent. Rates are derived from upstream's wire counters
// between consecutive calls, so the first call reports zero rates.
func (h *anacrolixHandle) Stats() Stats {
	ts := h.t.Stats()
	s := Stats{
		BytesDone:  h.t.BytesCompleted(),
		BytesTotal: h.t.Length(),
		Peers:      ts.TotalPeers,
	}
	now := time.Now()
	h.statsMu.Lock()
	defer h.statsMu.Unlock()
	if dt := now.Sub(h.statsAt); !h.statsAt.IsZero() && dt > 0 {
		read := ts.BytesReadData.Int64() - h.statsRead
		wrote := ts.BytesWrittenData.Int64() - h.statsWrote
		if read > 0 {
			h.downKbps = int64(float64(read) / dt.Seconds() / 1024)
		} else {
			h.downKbps = 0
		}
		if wrote > 0 {
			h.upKbps = int64(float64(wrote) / dt.Seconds() / 1024)
		} else {
			h.upKbps = 0
		}
	}
	h.statsAt = now
	h.statsRead = ts.BytesReadData.Int64()
	h.statsWrote = ts.BytesWrittenData.Int64()
	s.DownKbps, s.UpKbps = h.downKbps, h.upKbps
	if s.DownKbps > 0 {
		if remaining := s.BytesTotal - s.BytesDone; remaining > 0 {
			s.ETA = time.Duration(float64(remaining) / (float64(s.DownKbps) * 1024) * float64(time.Second))
		}
	}
	return s
}

// Pause stops both directions; Resume re-enables download and seed upload.
func (h *anacrolixHandle) Pause() error {
	h.t.DisallowDataDownload()
	h.t.DisallowDataUpload()
	return nil
}

func (h *anacrolixHandle) Resume() error {
	h.t.AllowDataDownload()
	h.t.AllowDataUpload()
	return nil
}

// Verify blocks until every piece of the torrent has been hash-checked.
func (h *anacrolixHandle) Verify(ctx context.Context) error {
	return h.t.VerifyDataContext(ctx)
}

// Drop removes the torrent from the client and unregisters the job. Upstream
// closes the per-job storage during Drop, so the deferred RemoveAll is safe.
func (h *anacrolixHandle) Drop(deleteFiles bool) error {
	h.t.Drop()
	h.e.mu.Lock()
	if h.e.jobs[h.jobID] == h {
		delete(h.e.jobs, h.jobID)
	}
	h.e.mu.Unlock()
	if deleteFiles {
		return os.RemoveAll(h.dir)
	}
	return nil
}

// Done closes its channel once every selected byte is downloaded; with no
// selection it never fires. The watcher is one-shot and stops silently if the
// torrent closes. BytesMissing is unusable — upstream counts unselected files.
func (h *anacrolixHandle) Done() <-chan struct{} {
	h.doneOnce.Do(func() {
		h.done = make(chan struct{})
		go h.watchDone()
	})
	return h.done
}

func (h *anacrolixHandle) watchDone() {
	ticker := time.NewTicker(donePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-h.t.Closed():
			return
		case <-ticker.C:
		}
		if h.selectedRemaining() == 0 {
			close(h.done)
			return
		}
	}
}

// selectedRemaining sums the bytes still missing from the selected files; -1
// when nothing has been selected yet.
func (h *anacrolixHandle) selectedRemaining() int64 {
	h.selMu.Lock()
	defer h.selMu.Unlock()
	if len(h.selected) == 0 {
		return -1
	}
	var remaining int64
	for i := range h.selected {
		f := h.files[i]
		if n := f.Length() - f.BytesCompleted(); n > 0 {
			remaining += n
		}
	}
	return remaining
}

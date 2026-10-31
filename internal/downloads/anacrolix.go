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

// AnacrolixEngine adapts github.com/anacrolix/torrent to Engine.
//
// One client serves every job: upstream (v1.61.0) takes per-torrent storage via
// AddTorrentOpts.Storage, so each torrent gets a storage root under
// <Dir>/<jobID> and the client-level DataDir is never used for job data. A
// torrent is keyed upstream by infohash, so a second live job for the same
// infohash (e.g. two users, same torrent) is refused rather than silently
// sharing another job's storage root.
type AnacrolixEngine struct {
	cfg config.DownloadsConfig
	cli *torrent.Client
	mu  sync.Mutex
	// jobs maps a job to its live handle; guarded by mu.
	jobs map[uuid.UUID]*anacrolixHandle
}

// NewAnacrolixEngine builds the client. DHT/PEX/UTP come from cfg; port
// mapping is left off because the operator owns the network posture. The
// client still binds local TCP/UDP ports even with discovery disabled.
func NewAnacrolixEngine(cfg config.DownloadsConfig) (*AnacrolixEngine, error) {
	cliConf := torrent.NewDefaultClientConfig()
	cliConf.DataDir = cfg.Dir
	cliConf.DisableTrackers = false
	cliConf.NoDHT = !cfg.DHT
	cliConf.DisablePEX = !cfg.PEX
	cliConf.DisableUTP = !cfg.UTP
	// Empty host = wildcard per socket network: upstream also opens tcp6, and a
	// literal "0.0.0.0" fails that listen on dual-stack hosts.
	cliConf.SetListenAddr(":0")
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
	cli, err := torrent.NewClient(cliConf)
	if err != nil {
		return nil, fmt.Errorf("downloads: new client: %w", err)
	}
	return &AnacrolixEngine{cfg: cfg, cli: cli, jobs: map[uuid.UUID]*anacrolixHandle{}}, nil
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
	spec, err := torrent.TorrentSpecFromMagnetUri(magnet)
	if err != nil {
		return nil, fmt.Errorf("downloads: parse magnet: %w", err)
	}
	// Per-job storage root: everything this job writes stays under jobDir.
	spec.Storage = storage.NewFile(jobDir)
	t, isNew, err := e.cli.AddTorrentSpec(spec)
	if err != nil {
		return nil, fmt.Errorf("downloads: add torrent: %w", err)
	}
	if !isNew {
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

	// files is sorted by display path so Index means the same thing across
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
	// must not reshuffle indexes.
	sort.SliceStable(h.files, func(i, j int) bool {
		return h.files[i].DisplayPath() < h.files[j].DisplayPath()
	})
	return h
}

// Files reports every file in the torrent, indexed in the handle's stable
// sorted-by-path order.
func (h *anacrolixHandle) Files() ([]EngineFile, error) {
	if h.t.Info() == nil {
		return nil, errors.New("downloads: torrent metadata not available")
	}
	out := make([]EngineFile, 0, len(h.files))
	for i, f := range h.files {
		out = append(out, EngineFile{
			Index:     i,
			Path:      filepath.Clean(f.DisplayPath()),
			SizeBytes: f.Length(),
			Progress:  f.BytesCompleted(),
		})
	}
	return out, nil
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

// Done closes its channel once every selected byte has been downloaded. The
// watcher is one-shot and stops silently if the torrent (or client) is closed.
// With no selection yet it never fires; BytesMissing alone cannot be used
// because upstream counts unselected files as missing too.
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

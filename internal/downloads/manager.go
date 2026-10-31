package downloads

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"animind-backend-go/internal/config"
	"animind-backend-go/internal/repository"
	"animind-backend-go/internal/scanner"
)

// Failure codes recorded on download_jobs.error. SetDownloadJobError is
// state-blind, so the code is written just before the transition to failed.
const (
	codeMetadataTimeout = "metadata_timeout"
	codeEngineDisabled  = "engine_disabled"
	codeNoFiles         = "no_files"
	codeDownloadFailed  = "download_failed"
	codeImportConflict  = "import_conflict"
)

var (
	// ErrForbidden answers the 403 class: the actor is neither the job's owner
	// nor an admin. Enforced by the user-action methods right after the row is
	// loaded, before any state check.
	ErrForbidden = errors.New("downloads: forbidden")
	// ErrDiskFull answers the 507 class: the download root lacks headroom.
	ErrDiskFull = errors.New("downloads: insufficient_disk")
	// ErrTooLarge answers the 400 job_too_large class.
	ErrTooLarge = errors.New("downloads: job_too_large")
	// ErrDuplicate answers the 409 class; its text carries the existing job id.
	ErrDuplicate = errors.New("downloads: duplicate")
	// ErrTooManyActive answers the 403 quota class, global or per user.
	ErrTooManyActive = errors.New("downloads: too_many_active")
)

// Request is one validated download request; httpx resolves ResultID into the
// magnet and title before calling.
type Request struct {
	UserID    uuid.UUID
	ResultID  string
	Magnet    string
	Title     string
	InfoHash  string
	SizeBytes int64
}

// store narrows *repository.Store to the download methods the manager drives.
// The manager tests swap in an in-memory fake; the production wrapper is dbStore.
type store interface {
	CreateDownloadJob(ctx context.Context, job repository.DownloadJob) (repository.DownloadJob, error)
	GetDownloadJob(ctx context.Context, id string) (repository.DownloadJob, error)
	FindDownloadJobByHash(ctx context.Context, userID, infoHash string) (repository.DownloadJob, bool, error)
	TransitionDownloadJob(ctx context.Context, id, from, to string) (repository.DownloadJob, error)
	SetDownloadJobError(ctx context.Context, id, code string) (repository.DownloadJob, error)
	SetDownloadJobProgress(ctx context.Context, id string, progressBytes int64) error
	SetDownloadJobSelection(ctx context.Context, id string, files []int) (repository.DownloadJob, error)
	CountActiveDownloadJobs(ctx context.Context, userID *string) (int, error)
	ListResumableDownloadJobs(ctx context.Context) ([]repository.DownloadJob, error)
	DeleteDownloadJob(ctx context.Context, id string) error
	SetDownloadJobImportResult(ctx context.Context, id, savePath string, showID *string) error
}

// dbStore adapts the concrete repository to the narrow store seam.
type dbStore struct{ *repository.Store }

var _ store = dbStore{}

// scanRunner is the import seam: one library scan after files land, matching
// scanner.Service.Run exactly so the real service slots in unwrapped.
type scanRunner interface {
	Run(ctx context.Context) (*scanner.Result, error)
}

// The real service must keep satisfying the seam; a signature drift breaks the
// build here instead of at wiring time.
var _ scanRunner = (*scanner.Service)(nil)

// Manager owns the download_jobs state machine: quotas and dedupe on Create, one
// drive goroutine per live job, startup resume, and hub publications. It never
// writes download_jobs outside the repository methods on the store seam.
type Manager struct {
	store  store
	engine Engine
	cfg    config.DownloadsConfig
	// libraryRoot is the real library root the importer builds destinations in
	// and removeImported checks containment against. Task 1 only pins
	// DOWNLOADS_DIR somewhere under it, so it is threaded, never derived from
	// cfg.Dir; an empty root fails imports closed.
	libraryRoot string
	scanner     scanRunner
	shows       showResolver // show_id lookups; nil when the store can't answer them
	hub         eventPublisher
	diskFree    func(path string, min int64) (bool, error)
	tick        time.Duration // progress poll interval; tests shorten it
	scanSettle  time.Duration // ErrAlreadyRunning wait before the one scan retry

	base    context.Context
	stopAll context.CancelFunc

	mu   sync.Mutex
	jobs map[string]*jobRun
}

// NewManager wires the production manager. libraryRoot must be the configured
// library root (cfg.Storage.LocalPath at wiring time), not anything derived
// from cfg.Dir: DOWNLOADS_DIR is only guaranteed to live under it.
func NewManager(store *repository.Store, engine Engine, cfg config.DownloadsConfig, libraryRoot string, scanner scanRunner, hub eventPublisher, diskFree func(path string, min int64) (bool, error)) *Manager {
	return newManager(dbStore{store}, engine, cfg, libraryRoot, scanner, hub, diskFree)
}

func newManager(st store, engine Engine, cfg config.DownloadsConfig, libraryRoot string, scanner scanRunner, hub eventPublisher, diskFree func(path string, min int64) (bool, error)) *Manager {
	if engine == nil {
		engine = NewNoopEngine()
	}
	if diskFree == nil {
		diskFree = statfsFree
	}
	base, stopAll := context.WithCancel(context.Background())
	var shows showResolver
	if r, ok := st.(showResolver); ok {
		shows = r // dbStore carries *repository.Store.ShowByTitle; fakes may too
	}
	return &Manager{
		store: st, engine: engine, cfg: cfg, libraryRoot: libraryRoot, scanner: scanner, shows: shows, hub: hub, diskFree: diskFree,
		tick: 500 * time.Millisecond,
		base: base, stopAll: stopAll,
		jobs: map[string]*jobRun{},
	}
}

// Close stops every drive goroutine; the wiring calls it on shutdown next to
// engine.Close. Rows in non-terminal states survive and are picked up by
// ResumeFromStore on the next start.
func (m *Manager) Close() { m.stopAll() }

// jobRun is the live side of one job: the engine handle plus the drive
// generation. Guarded fields are touched by HTTP goroutines while the drive
// goroutine runs.
type jobRun struct {
	id     uuid.UUID
	userID string
	magnet string

	mu     sync.Mutex
	handle TorrentHandle
	cancel context.CancelFunc
}

func (j *jobRun) currentHandle() TorrentHandle {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.handle
}

func (j *jobRun) setHandle(h TorrentHandle) {
	j.mu.Lock()
	j.handle = h
	j.mu.Unlock()
}

// stop ends the current drive generation; the goroutine leaves silently.
func (j *jobRun) stop() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.cancel != nil {
		j.cancel()
	}
}

func (m *Manager) register(j *jobRun) *jobRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs[j.id.String()] = j
	return j
}

func (m *Manager) lookup(id string) *jobRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.jobs[id]
}

func (m *Manager) take(id string) *jobRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.jobs[id]
	delete(m.jobs, id)
	return j
}

// launch (re)starts the drive loop; any previous generation is cancelled so at
// most one loop per job is alive.
func (m *Manager) launch(j *jobRun) {
	j.mu.Lock()
	if j.cancel != nil {
		j.cancel()
	}
	ctx, cancel := context.WithCancel(m.base)
	j.cancel = cancel
	j.mu.Unlock()
	go m.drive(ctx, j)
}

// ── Create ───────────────────────────────────────────────────────────────────

// Create validates in the spec §7.5 order — disk, size, per-user, global,
// duplicate — all before the row is written, then enqueues the drive loop.
// Rejected requests never leave rows behind.
func (m *Manager) Create(ctx context.Context, req Request) (repository.DownloadJob, error) {
	if !m.cfg.Enabled {
		return repository.DownloadJob{}, ErrNotEnabled
	}
	need := m.cfg.MinFreeBytes
	if req.SizeBytes > 0 {
		need += req.SizeBytes
	}
	ok, err := m.diskFree(m.cfg.Dir, need)
	if err != nil {
		return repository.DownloadJob{}, fmt.Errorf("downloads: disk check: %w", err)
	}
	if !ok {
		return repository.DownloadJob{}, ErrDiskFull
	}
	if m.cfg.MaxBytesPerJob > 0 && req.SizeBytes > m.cfg.MaxBytesPerJob {
		return repository.DownloadJob{}, ErrTooLarge
	}
	uid := req.UserID.String()
	n, err := m.store.CountActiveDownloadJobs(ctx, &uid)
	if err != nil {
		return repository.DownloadJob{}, fmt.Errorf("downloads: quota check: %w", err)
	}
	if m.cfg.MaxActivePerUser > 0 && n >= m.cfg.MaxActivePerUser {
		return repository.DownloadJob{}, ErrTooManyActive
	}
	n, err = m.store.CountActiveDownloadJobs(ctx, nil)
	if err != nil {
		return repository.DownloadJob{}, fmt.Errorf("downloads: quota check: %w", err)
	}
	if m.cfg.MaxActive > 0 && n >= m.cfg.MaxActive {
		return repository.DownloadJob{}, ErrTooManyActive
	}
	existing, found, err := m.store.FindDownloadJobByHash(ctx, uid, req.InfoHash)
	if err != nil {
		return repository.DownloadJob{}, fmt.Errorf("downloads: dedupe check: %w", err)
	}
	if found {
		return repository.DownloadJob{}, fmt.Errorf("%w: existing job %s", ErrDuplicate, existing.ID)
	}

	job, err := m.store.CreateDownloadJob(ctx, repository.DownloadJob{
		UserID: uid, ResultTitle: req.Title, InfoHash: req.InfoHash, SizeBytes: req.SizeBytes,
	})
	if err != nil {
		return repository.DownloadJob{}, fmt.Errorf("downloads: create job: %w", err)
	}
	m.publish(job)
	id, err := uuid.Parse(job.ID)
	if err != nil {
		return job, fmt.Errorf("downloads: job id %q: %w", job.ID, err)
	}
	m.launch(m.register(&jobRun{id: id, userID: job.UserID, magnet: magnetFor(req.Magnet, job)}))
	return job, nil
}

// magnetFor prefers the httpx-resolved magnet; after a restart the original is
// gone and one is rebuilt from the stored info hash for DHT discovery.
func magnetFor(magnet string, job repository.DownloadJob) string {
	if magnet != "" {
		return magnet
	}
	return "magnet:?xt=urn:btih:" + job.InfoHash + "&dn=" + url.QueryEscape(job.ResultTitle)
}

// ── user actions ─────────────────────────────────────────────────────────────

// authorized loads the row and enforces ownership before any state work: the
// actor must own the job or hold the admin bypass. A mismatch answers
// ErrForbidden without leaking anything else about the row.
func (m *Manager) authorized(ctx context.Context, jobID, actorUserID string, isAdmin bool) (repository.DownloadJob, error) {
	job, err := m.store.GetDownloadJob(ctx, jobID)
	if err != nil {
		return repository.DownloadJob{}, err
	}
	if !isAdmin && job.UserID != actorUserID {
		return repository.DownloadJob{}, ErrForbidden
	}
	return job, nil
}

func (m *Manager) Pause(ctx context.Context, jobID, actorUserID string, isAdmin bool) (repository.DownloadJob, error) {
	job, err := m.authorized(ctx, jobID, actorUserID, isAdmin)
	if err != nil {
		return repository.DownloadJob{}, err
	}
	if !canTransition(job.State, StatePaused) {
		return repository.DownloadJob{}, fmt.Errorf("downloads: cannot pause from %s: %w", job.State, repository.ErrConflict)
	}
	updated, err := m.store.TransitionDownloadJob(ctx, job.ID, job.State, StatePaused)
	if err != nil {
		return repository.DownloadJob{}, err // the row moved underneath us
	}
	if j := m.lookup(jobID); j != nil {
		j.stop()
		if h := j.currentHandle(); h != nil {
			_ = h.Pause() // best effort: the row is paused either way
		}
	}
	m.publish(updated)
	return updated, nil
}

func (m *Manager) Resume(ctx context.Context, jobID, actorUserID string, isAdmin bool) (repository.DownloadJob, error) {
	job, err := m.authorized(ctx, jobID, actorUserID, isAdmin)
	if err != nil {
		return repository.DownloadJob{}, err
	}
	if job.State != StatePaused {
		return repository.DownloadJob{}, fmt.Errorf("downloads: cannot resume from %s: %w", job.State, repository.ErrConflict)
	}
	updated, err := m.store.TransitionDownloadJob(ctx, job.ID, StatePaused, resumeTarget(job))
	if err != nil {
		return repository.DownloadJob{}, err
	}
	j, err := m.runFor(updated)
	if err != nil {
		return updated, err
	}
	if h := j.currentHandle(); h != nil {
		_ = h.Resume()
	}
	m.launch(j)
	m.publish(updated)
	return updated, nil
}

// runFor returns the live run for a stored row, registering a fresh one when
// the job is not being driven in this session (post-restart resume).
func (m *Manager) runFor(job repository.DownloadJob) (*jobRun, error) {
	if j := m.lookup(job.ID); j != nil {
		return j, nil
	}
	id, err := uuid.Parse(job.ID)
	if err != nil {
		return nil, fmt.Errorf("downloads: job id %q: %w", job.ID, err)
	}
	return m.register(&jobRun{id: id, userID: job.UserID, magnet: magnetFor("", job)}), nil
}

// resumeTarget picks the state a paused row returns to. Rows with a stored
// selection replay it into downloading; selection-less rows (packs paused
// while awaiting_selection) go back to awaiting_selection — resuming them
// into downloading would strand them there, since Done() only fires for
// selected bytes and the row could never leave.
func resumeTarget(job repository.DownloadJob) string {
	if len(job.SelectedFiles) > 0 {
		return StateDownloading
	}
	return StateAwaitingSelection
}

// Cancel stops a live job from any non-terminal or paused state, or removes a
// terminal row outright. deleteFiles additionally drops the engine torrent's
// data and the job directory.
func (m *Manager) Cancel(ctx context.Context, jobID, actorUserID string, isAdmin, deleteFiles bool) error {
	job, err := m.authorized(ctx, jobID, actorUserID, isAdmin)
	if err != nil {
		return err
	}
	if !isTerminal(job.State) {
		updated, err := m.store.TransitionDownloadJob(ctx, job.ID, job.State, StateCancelled)
		if err != nil {
			return err
		}
		m.publish(updated)
	}
	var handle TorrentHandle
	if j := m.take(jobID); j != nil {
		j.stop()
		handle = j.currentHandle()
	}
	if handle != nil {
		_ = handle.Drop(deleteFiles)
	}
	if isTerminal(job.State) {
		if err := m.store.DeleteDownloadJob(ctx, job.ID); err != nil {
			return err
		}
	}
	if deleteFiles {
		return removeJobDir(m.cfg.Dir, jobID)
	}
	return nil
}

// SelectFiles records the user's pick for a pack waiting in awaiting_selection
// and pushes it into the engine; the row then walks on to downloading.
func (m *Manager) SelectFiles(ctx context.Context, jobID, actorUserID string, isAdmin bool, indexes []int) (repository.DownloadJob, error) {
	job, err := m.authorized(ctx, jobID, actorUserID, isAdmin)
	if err != nil {
		return repository.DownloadJob{}, err
	}
	if job.State != StateAwaitingSelection {
		return repository.DownloadJob{}, fmt.Errorf("downloads: job %s is %s, not awaiting_selection: %w", job.ID, job.State, repository.ErrConflict)
	}
	if _, err := m.store.SetDownloadJobSelection(ctx, job.ID, indexes); err != nil {
		return repository.DownloadJob{}, err
	}
	if j := m.lookup(jobID); j != nil {
		if h := j.currentHandle(); h != nil {
			if err := h.Select(indexes); err != nil {
				return repository.DownloadJob{}, fmt.Errorf("downloads: select files: %w", err)
			}
		}
	}
	updated, err := m.store.TransitionDownloadJob(ctx, job.ID, StateAwaitingSelection, StateDownloading)
	if err != nil {
		return repository.DownloadJob{}, err
	}
	j, err := m.runFor(updated)
	if err != nil {
		return updated, err
	}
	m.launch(j)
	m.publish(updated)
	return updated, nil
}

// ── startup ──────────────────────────────────────────────────────────────────

// ResumeFromStore re-drives every non-terminal row after a restart: paused
// rows are CASed to their selection-aware target first (selection ->
// downloading, none -> awaiting_selection), everything else re-enters at its
// own state. Ready rows are left alone. The CAS lives here, not in the drive
// loop, so no drive generation can ever un-pause a row on its own.
func (m *Manager) ResumeFromStore(ctx context.Context) error {
	jobs, err := m.store.ListResumableDownloadJobs(ctx)
	if err != nil {
		return fmt.Errorf("downloads: list resumable: %w", err)
	}
	for _, job := range jobs {
		if isTerminal(job.State) {
			continue
		}
		if job.State == StatePaused {
			job, err = m.store.TransitionDownloadJob(ctx, job.ID, StatePaused, resumeTarget(job))
			if err != nil {
				continue // paused->cancelled or failed raced underneath us
			}
		}
		id, err := uuid.Parse(job.ID)
		if err != nil {
			continue
		}
		m.launch(m.register(&jobRun{id: id, userID: job.UserID, magnet: magnetFor("", job)}))
	}
	return nil
}

// ── drive loop ───────────────────────────────────────────────────────────────

func (m *Manager) drive(ctx context.Context, j *jobRun) {
	job, err := m.store.GetDownloadJob(ctx, j.id.String())
	if err != nil {
		return // the row is gone; nothing to drive
	}
	for ctx.Err() == nil {
		switch job.State {
		case StateQueued:
			next, ok := m.hop(ctx, j, StateQueued, StateFetchingMetadata)
			if !ok {
				return
			}
			job = next

		case StateFetchingMetadata:
			// The metadata-wait budget lives inside engine.Add; a timeout or any
			// other Add failure fails the job from here.
			if _, ok := m.ensureHandle(ctx, j, job); !ok {
				return
			}
			next, ok := m.hop(ctx, j, StateFetchingMetadata, StateAwaitingSelection)
			if !ok {
				return
			}
			job = next

		case StateAwaitingSelection:
			h, ok := m.ensureHandle(ctx, j, job) // also rebuilds the handle after a restart
			if !ok {
				return
			}
			files, err := h.Files()
			if err != nil {
				m.failJob(ctx, j, job.State, codeFor(err))
				return
			}
			if len(files) == 0 {
				m.failJob(ctx, j, job.State, codeNoFiles)
				return
			}
			if countVideoFiles(files) > 1 {
				return // pack: waits for the user's SelectFiles
			}
			// single-episode torrent: pick everything and roll on
			indexes := allIndexes(files)
			if err := h.Select(indexes); err != nil {
				m.failJob(ctx, j, job.State, codeFor(err))
				return
			}
			if _, err := m.store.SetDownloadJobSelection(ctx, job.ID, indexes); err != nil && !errors.Is(err, repository.ErrNotFound) {
				m.failJob(ctx, j, job.State, codeFor(err))
				return
			}
			next, ok := m.hop(ctx, j, StateAwaitingSelection, StateDownloading)
			if !ok {
				return
			}
			job = next

		case StateDownloading:
			if !m.watch(ctx, j, job) {
				return
			}
			next, ok := m.hop(ctx, j, StateDownloading, StateVerifying)
			if !ok {
				return
			}
			job = next

		case StateVerifying:
			h, ok := m.ensureHandle(ctx, j, job)
			if !ok {
				return
			}
			if err := h.Verify(ctx); err != nil {
				m.failJob(ctx, j, job.State, codeFor(err))
				return
			}
			next, ok := m.hop(ctx, j, StateVerifying, StateImporting)
			if !ok {
				return
			}
			job = next

		case StateImporting:
			h, ok := m.ensureHandle(ctx, j, job) // also rebuilds the handle after a restart
			if !ok {
				return
			}
			files, err := h.Files()
			if err != nil {
				m.failJob(ctx, j, job.State, codeFor(err))
				return
			}
			if _, err := m.importJob(ctx, job, files, job.SelectedFiles); err != nil {
				m.failJob(ctx, j, job.State, codeFor(err))
				return
			}
			if _, ok := m.hop(ctx, j, StateImporting, StateReady); !ok {
				return
			}
			return // terminal: the drive loop's work is done

		default:
			// Terminal, paused, or unknown. A paused row leaves paused only
			// through the explicit resume paths (Resume, ResumeFromStore): a
			// drive generation that merely observes paused — e.g. one
			// scheduled after the user's Pause CAS but before its stop — must
			// not un-pause the job underneath the user.
			return
		}
	}
}

// hop performs one state-machine edge as a compare-and-set and publishes the
// new state. A conflict means the user paused or cancelled meanwhile: the loop
// must stop silently, the user's action wins.
func (m *Manager) hop(ctx context.Context, j *jobRun, from, to string) (repository.DownloadJob, bool) {
	job, err := m.store.TransitionDownloadJob(ctx, j.id.String(), from, to)
	if err != nil {
		if !errors.Is(err, repository.ErrConflict) {
			m.failJob(ctx, j, from, codeDownloadFailed)
		}
		return repository.DownloadJob{}, false
	}
	m.publish(job)
	return job, true
}

// watch polls the engine until the torrent completes or the job is superseded.
// False means stop driving without failing: pause, cancel, or a vanished row.
func (m *Manager) watch(ctx context.Context, j *jobRun, job repository.DownloadJob) bool {
	h, ok := m.ensureHandle(ctx, j, job)
	if !ok {
		return false
	}
	ticker := time.NewTicker(m.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-h.Done():
			return true
		case <-ticker.C:
			if ctx.Err() != nil {
				// Superseded generation: a pause (or pause+resume) launched a
				// new drive loop for this row; never let this stale one write
				// a progress tick into a row it no longer owns.
				return false
			}
			st := h.Stats()
			if err := m.store.SetDownloadJobProgress(ctx, j.id.String(), st.BytesDone); err != nil {
				if errors.Is(err, repository.ErrNotFound) {
					return false // the row was deleted under us
				}
				continue // transient store hiccup: keep the loop alive
			}
			m.publishProgress(job, st)
		}
	}
}

// ensureHandle returns the torrent handle, re-Adding when the engine no longer
// knows the torrent (post-restart). A stored selection is replayed so Done()
// can fire again — the engine only completes selected bytes.
func (m *Manager) ensureHandle(ctx context.Context, j *jobRun, job repository.DownloadJob) (TorrentHandle, bool) {
	if h := j.currentHandle(); h != nil {
		return h, true
	}
	h, err := m.engine.Add(ctx, j.id, j.magnet)
	if err != nil {
		m.failJob(ctx, j, job.State, codeFor(err))
		return nil, false
	}
	j.setHandle(h)
	if len(job.SelectedFiles) > 0 {
		if err := h.Select(job.SelectedFiles); err != nil {
			m.failJob(ctx, j, job.State, codeFor(err))
			return nil, false
		}
	}
	return h, true
}

// failJob records the code, then flips the row to failed via CAS. Both steps
// are best effort: once a concurrent pause or cancel wins the CAS, it owns the
// row and the failure is dropped.
func (m *Manager) failJob(ctx context.Context, j *jobRun, from, code string) {
	if ctx.Err() != nil {
		return // superseded: the user's action decides the outcome
	}
	_, _ = m.store.SetDownloadJobError(ctx, j.id.String(), code)
	job, err := m.store.TransitionDownloadJob(ctx, j.id.String(), from, StateFailed)
	if err != nil {
		return
	}
	m.publish(job)
}

func codeFor(err error) string {
	switch {
	case errors.Is(err, ErrMetadataTimeout):
		return codeMetadataTimeout
	case errors.Is(err, ErrNotEnabled):
		return codeEngineDisabled
	case errors.Is(err, errImportConflict):
		return codeImportConflict
	case errors.Is(err, errNothingSelected):
		return codeNoFiles
	default:
		return codeDownloadFailed
	}
}

// ── events ───────────────────────────────────────────────────────────────────

func (m *Manager) publish(job repository.DownloadJob) {
	if m.hub == nil {
		return
	}
	m.hub.Publish(job.UserID, eventFor(job, nil))
}

func (m *Manager) publishProgress(job repository.DownloadJob, st Stats) {
	if m.hub == nil {
		return
	}
	m.hub.Publish(job.UserID, eventFor(job, &st))
}

func eventFor(job repository.DownloadJob, st *Stats) JobEvent {
	size, bytes := job.SizeBytes, job.ProgressBytes
	down, up, peers, eta := int64(0), int64(0), 0, time.Duration(0)
	if st != nil {
		bytes = st.BytesDone
		down, up, peers, eta = st.DownKbps, st.UpKbps, st.Peers, st.ETA
		if size == 0 {
			size = st.BytesTotal
		}
	}
	ev := JobEvent{
		ID: job.ID, State: job.State, SizeBytes: size, BytesDone: bytes,
		DownKbps: down, UpKbps: up, Peers: peers, ETASec: int64(eta / time.Second),
		ShowID: job.ShowID, Error: job.Error,
	}
	if size > 0 {
		ev.Progress = float64(bytes) / float64(size) * 100
	}
	return ev
}

// ── helpers ──────────────────────────────────────────────────────────────────

// defaultVideoExts is the video-only half of files.go's selection allowlist:
// the auto-select heuristic must tell one episode from a pack, so subtitles
// stay out of the count.
var defaultVideoExts = videoExtensionSet

func countVideoFiles(files []EngineFile) int {
	n := 0
	for _, f := range files {
		if defaultVideoExts[strings.TrimPrefix(strings.ToLower(filepath.Ext(f.Path)), ".")] {
			n++
		}
	}
	return n
}

func allIndexes(files []EngineFile) []int {
	out := make([]int, 0, len(files))
	for _, f := range files {
		out = append(out, f.Index)
	}
	return out
}

// removeJobDir deletes <root>/<jobID> after verifying the joined path still
// lives under the download root; the id flows in from user-facing routes.
func removeJobDir(root, jobID string) error {
	dir := filepath.Join(root, jobID)
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("downloads: resolve root: %w", err)
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("downloads: resolve job dir: %w", err)
	}
	if absDir != absRoot && !strings.HasPrefix(absDir, absRoot+string(os.PathSeparator)) {
		return fmt.Errorf("downloads: job dir %s escapes %s", dir, root)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("downloads: remove job dir: %w", err)
	}
	return nil
}

// freeDiskBytesOf is the free-space probe behind statfsFree, swappable so
// tests can pin the boundary without touching a real filesystem.
var freeDiskBytesOf = freeDiskBytes

// statfsFree is the production diskFree: the root must keep min bytes of
// headroom beyond whatever the job itself will occupy. The boundary is
// inclusive per spec — free >= max(jobSize, 0) + MinFreeBytes passes.
func statfsFree(path string, min int64) (bool, error) {
	free, err := freeDiskBytesOf(path)
	if err != nil {
		return false, err
	}
	return free >= min, nil
}

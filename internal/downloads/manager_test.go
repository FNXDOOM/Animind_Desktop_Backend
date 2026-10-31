package downloads

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"animind-backend-go/internal/config"
	"animind-backend-go/internal/repository"
)

// ── fake store ───────────────────────────────────────────────────────────────

// fakeStore mirrors internal/repository/downloads.go semantics in memory: the
// CAS transition, the active-set counting (ready and paused count), and the
// state-blind progress/error writes.
type fakeStore struct {
	mu   sync.Mutex
	jobs map[string]repository.DownloadJob
	ids  []string // queued ids handed out by CreateDownloadJob, for tests that must know the id up front
}

func newFakeStore() *fakeStore { return &fakeStore{jobs: map[string]repository.DownloadJob{}} }

var _ store = (*fakeStore)(nil)

// queueID pins the id the next CreateDownloadJob returns.
func (s *fakeStore) queueID(id string) { s.mu.Lock(); s.ids = append(s.ids, id); s.mu.Unlock() }

func (s *fakeStore) seed(job repository.DownloadJob) repository.DownloadJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	if job.ID == "" {
		job.ID = uuid.NewString()
	}
	now := time.Now()
	job.CreatedAt, job.UpdatedAt = now, now
	s.jobs[job.ID] = job
	return job
}

func (s *fakeStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.jobs)
}

func (s *fakeStore) get(id string) repository.DownloadJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobs[id]
}

func (s *fakeStore) CreateDownloadJob(_ context.Context, job repository.DownloadJob) (repository.DownloadJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if job.State == "" {
		job.State = StateQueued
	}
	if len(s.ids) > 0 {
		job.ID, s.ids = s.ids[0], s.ids[1:]
	} else {
		job.ID = uuid.NewString()
	}
	now := time.Now()
	job.CreatedAt, job.UpdatedAt = now, now
	s.jobs[job.ID] = job
	return job, nil
}

func (s *fakeStore) GetDownloadJob(_ context.Context, id string) (repository.DownloadJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok {
		return repository.DownloadJob{}, repository.ErrNotFound
	}
	return job, nil
}

func (s *fakeStore) FindDownloadJobByHash(_ context.Context, userID, infoHash string) (repository.DownloadJob, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, job := range s.jobs {
		if job.UserID == userID && job.InfoHash == infoHash && isActive(job.State) {
			return job, true, nil
		}
	}
	return repository.DownloadJob{}, false, nil
}

func (s *fakeStore) TransitionDownloadJob(_ context.Context, id, from, to string) (repository.DownloadJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok || job.State != from {
		return repository.DownloadJob{}, repository.ErrConflict
	}
	job.State = to
	job.UpdatedAt = time.Now()
	s.jobs[id] = job
	return job, nil
}

func (s *fakeStore) SetDownloadJobError(_ context.Context, id, code string) (repository.DownloadJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok {
		return repository.DownloadJob{}, repository.ErrNotFound
	}
	job.Error = &code
	job.UpdatedAt = time.Now()
	s.jobs[id] = job
	return job, nil
}

func (s *fakeStore) SetDownloadJobProgress(_ context.Context, id string, progressBytes int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok {
		return repository.ErrNotFound
	}
	job.ProgressBytes = progressBytes
	job.UpdatedAt = time.Now()
	s.jobs[id] = job
	return nil
}

func (s *fakeStore) SetDownloadJobSelection(_ context.Context, id string, files []int) (repository.DownloadJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok {
		return repository.DownloadJob{}, repository.ErrNotFound
	}
	job.SelectedFiles = files
	job.UpdatedAt = time.Now()
	s.jobs[id] = job
	return job, nil
}

func (s *fakeStore) CountActiveDownloadJobs(_ context.Context, userID *string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, job := range s.jobs {
		if !isActive(job.State) {
			continue
		}
		if userID != nil && job.UserID != *userID {
			continue
		}
		n++
	}
	return n, nil
}

func (s *fakeStore) ListResumableDownloadJobs(_ context.Context) ([]repository.DownloadJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []repository.DownloadJob{}
	for _, job := range s.jobs {
		if isActive(job.State) {
			out = append(out, job)
		}
	}
	return out, nil
}

func (s *fakeStore) DeleteDownloadJob(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[id]; !ok {
		return repository.ErrNotFound
	}
	delete(s.jobs, id)
	return nil
}

// ── fake engine ──────────────────────────────────────────────────────────────

type fakeAdd struct {
	jobID  uuid.UUID
	magnet string
}

type fakeEngine struct {
	mu        sync.Mutex
	added     []fakeAdd
	files     map[string][]EngineFile // keyed by job id string
	handles   map[string]*fakeHandle
	addErr    map[string]error // keyed by job id string
	pauseOps  map[string]int
	resumeOps map[string]int
	selectOps map[string][]int
	verifyOps map[string]int
	drops     map[string]bool
}

func newFakeEngine(files map[string][]EngineFile) *fakeEngine {
	return &fakeEngine{
		files:     files,
		handles:   map[string]*fakeHandle{},
		addErr:    map[string]error{},
		pauseOps:  map[string]int{},
		resumeOps: map[string]int{},
		selectOps: map[string][]int{},
		verifyOps: map[string]int{},
		drops:     map[string]bool{},
	}
}

func (f *fakeEngine) Add(_ context.Context, jobID uuid.UUID, magnet string) (TorrentHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added = append(f.added, fakeAdd{jobID: jobID, magnet: magnet})
	if err := f.addErr[jobID.String()]; err != nil {
		return nil, err
	}
	h := &fakeHandle{eng: f, jobID: jobID.String(), files: f.files[jobID.String()], done: make(chan struct{})}
	f.handles[jobID.String()] = h
	return h, nil
}

func (f *fakeEngine) Close() error { return nil }

func (f *fakeEngine) handle(jobID string) *fakeHandle {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.handles[jobID]
}

func (f *fakeEngine) addedFor(jobID uuid.UUID) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, a := range f.added {
		if a.jobID == jobID {
			n++
		}
	}
	return n
}

type fakeHandle struct {
	eng       *fakeEngine
	jobID     string
	files     []EngineFile
	done      chan struct{}
	bytesDone atomic.Int64
}

func (h *fakeHandle) Files() ([]EngineFile, error) { return h.files, nil }

func (h *fakeHandle) Select(indexes []int) error {
	h.eng.mu.Lock()
	defer h.eng.mu.Unlock()
	h.eng.selectOps[h.jobID] = indexes
	return nil
}

func (h *fakeHandle) Stats() Stats {
	total := int64(0)
	for _, f := range h.files {
		total += f.SizeBytes
	}
	return Stats{
		BytesDone: h.bytesDone.Load(), BytesTotal: total,
		DownKbps: 2400, UpKbps: 120, Peers: 18, ETA: 400 * time.Second,
	}
}

func (h *fakeHandle) Pause() error {
	h.eng.mu.Lock()
	h.eng.pauseOps[h.jobID]++
	h.eng.mu.Unlock()
	return nil
}

func (h *fakeHandle) Resume() error {
	h.eng.mu.Lock()
	h.eng.resumeOps[h.jobID]++
	h.eng.mu.Unlock()
	return nil
}

func (h *fakeHandle) Verify(context.Context) error {
	h.eng.mu.Lock()
	h.eng.verifyOps[h.jobID]++
	h.eng.mu.Unlock()
	return nil
}

func (h *fakeHandle) Drop(deleteFiles bool) error {
	h.eng.mu.Lock()
	defer h.eng.mu.Unlock()
	h.eng.drops[h.jobID] = deleteFiles
	return nil
}

func (h *fakeHandle) Done() <-chan struct{} { return h.done }

// ── recording hub ────────────────────────────────────────────────────────────

type recordingHub struct {
	mu     sync.Mutex
	events []JobEvent
}

func (h *recordingHub) Publish(_ string, ev JobEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, ev)
}

func (h *recordingHub) statesFor(jobID string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, ev := range h.events {
		if ev.ID != jobID {
			continue
		}
		// progress ticks repeat the current state; keep the state-change walk only
		if len(out) > 0 && out[len(out)-1] == ev.State {
			continue
		}
		out = append(out, ev.State)
	}
	return out
}

func (h *recordingHub) sawProgress(jobID string, bytesDone int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ev := range h.events {
		if ev.ID == jobID && ev.BytesDone == bytesDone && ev.DownKbps == 2400 {
			return true
		}
	}
	return false
}

// ── helpers ──────────────────────────────────────────────────────────────────

func testConfig(dir string) config.DownloadsConfig {
	return config.DownloadsConfig{
		Dir: dir, Enabled: true,
		MaxActive: 2, MaxActivePerUser: 1,
		MaxBytesPerJob: 100 << 20, MinFreeBytes: 1 << 20,
	}
}

func diskOK(string, int64) (bool, error) { return true, nil }

func newTestManager(t *testing.T, eng Engine, st store, cfg config.DownloadsConfig, disk func(string, int64) (bool, error), hub eventPublisher) *Manager {
	t.Helper()
	m := newManager(st, eng, cfg, nil, hub, disk)
	m.tick = 5 * time.Millisecond
	t.Cleanup(m.Close)
	return m
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func stateOf(t *testing.T, st *fakeStore, id string) string {
	t.Helper()
	job, err := st.GetDownloadJob(context.Background(), id)
	if err != nil {
		t.Fatalf("get job %s: %v", id, err)
	}
	return job.State
}

func reqFor(user uuid.UUID, hash string, size int64) Request {
	return Request{
		UserID: user, ResultID: "opaque-result", Magnet: "magnet:?xt=urn:btih:" + hash,
		Title: "Show", InfoHash: hash, SizeBytes: size,
	}
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestTransitionsMatchSpecTable(t *testing.T) {
	for from, tos := range transitions {
		for _, to := range tos {
			if !canTransition(from, to) {
				t.Errorf("canTransition(%s, %s) = false, want true", from, to)
			}
		}
	}
	illegal := [][2]string{
		{StateQueued, StateDownloading},
		{StateQueued, StateReady},
		{StateFetchingMetadata, StateDownloading},
		{StateAwaitingSelection, StateVerifying},
		{StateDownloading, StateQueued},
		{StateVerifying, StateReady},
		{StateImporting, StatePaused},
		{StateReady, StateCancelled},
		{StateFailed, StateQueued},
		{StateCancelled, StateDownloading},
	}
	for _, pair := range illegal {
		if canTransition(pair[0], pair[1]) {
			t.Errorf("canTransition(%s, %s) = true, want false", pair[0], pair[1])
		}
	}
	for _, s := range []string{StateReady, StateFailed, StateCancelled} {
		if !isTerminal(s) {
			t.Errorf("isTerminal(%s) = false, want true", s)
		}
	}
	for _, s := range []string{StateQueued, StateDownloading, StatePaused, StateVerifying} {
		if isTerminal(s) {
			t.Errorf("isTerminal(%s) = true, want false", s)
		}
	}
	// isActive is the unique-index set from migration 0003: ready and paused stay
	// in it, failed and cancelled leave it.
	for _, s := range []string{StateQueued, StateFetchingMetadata, StateAwaitingSelection, StateDownloading,
		StateVerifying, StateImporting, StateReady, StatePaused} {
		if !isActive(s) {
			t.Errorf("isActive(%s) = false, want true", s)
		}
	}
	for _, s := range []string{StateFailed, StateCancelled, "", "bogus"} {
		if isActive(s) {
			t.Errorf("isActive(%q) = true, want false", s)
		}
	}
}

func TestCreateQuotaChecksRunInOrder(t *testing.T) {
	user := uuid.New()
	ctx := context.Background()

	t.Run("disk check runs before the size cap", func(t *testing.T) {
		st := newFakeStore()
		m := newTestManager(t, newFakeEngine(nil), st, testConfig(t.TempDir()),
			func(string, int64) (bool, error) { return false, nil }, nil)
		req := reqFor(user, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 1<<40) // over the size cap too
		if _, err := m.Create(ctx, req); !errors.Is(err, ErrDiskFull) {
			t.Fatalf("want ErrDiskFull, got %v", err)
		}
		if st.count() != 0 {
			t.Fatalf("rejected Create wrote %d rows, want 0", st.count())
		}
	})

	t.Run("size cap runs before the active counts", func(t *testing.T) {
		st := newFakeStore()
		cfg := testConfig(t.TempDir())
		st.seed(repository.DownloadJob{UserID: user.String(), InfoHash: "bbbb", State: StateDownloading})
		m := newTestManager(t, newFakeEngine(nil), st, cfg, diskOK, nil)
		req := reqFor(user, "bbbb", 1<<40) // over cap AND at the per-user cap and a duplicate
		if _, err := m.Create(ctx, req); !errors.Is(err, ErrTooLarge) {
			t.Fatalf("want ErrTooLarge, got %v", err)
		}
		if st.count() != 1 {
			t.Fatalf("rejected Create wrote rows, want only the seeded one")
		}
	})

	t.Run("per-user cap runs before the duplicate check", func(t *testing.T) {
		st := newFakeStore()
		existing := st.seed(repository.DownloadJob{UserID: user.String(), InfoHash: "cccc", State: StateDownloading})
		m := newTestManager(t, newFakeEngine(nil), st, testConfig(t.TempDir()), diskOK, nil)
		_, err := m.Create(ctx, reqFor(user, "cccc", 10)) // duplicate too
		if !errors.Is(err, ErrTooManyActive) {
			t.Fatalf("want ErrTooManyActive, got %v", err)
		}
		if existing.State != StateDownloading {
			t.Fatalf("seeded job changed state: %s", existing.State)
		}
	})

	t.Run("global cap", func(t *testing.T) {
		st := newFakeStore()
		cfg := testConfig(t.TempDir())
		cfg.MaxActive, cfg.MaxActivePerUser = 1, 5
		other := uuid.New()
		st.seed(repository.DownloadJob{UserID: other.String(), InfoHash: "dddd", State: StateDownloading})
		m := newTestManager(t, newFakeEngine(nil), st, cfg, diskOK, nil)
		if _, err := m.Create(ctx, reqFor(user, "eeee", 10)); !errors.Is(err, ErrTooManyActive) {
			t.Fatalf("want ErrTooManyActive, got %v", err)
		}
	})

	t.Run("duplicate carries the existing job id", func(t *testing.T) {
		st := newFakeStore()
		cfg := testConfig(t.TempDir())
		cfg.MaxActive, cfg.MaxActivePerUser = 10, 10
		existing := st.seed(repository.DownloadJob{UserID: user.String(), InfoHash: "ffff", State: StateAwaitingSelection})
		m := newTestManager(t, newFakeEngine(nil), st, cfg, diskOK, nil)
		_, err := m.Create(ctx, reqFor(user, "ffff", 10))
		if !errors.Is(err, ErrDuplicate) {
			t.Fatalf("want ErrDuplicate, got %v", err)
		}
		if !strings.Contains(err.Error(), existing.ID) {
			t.Fatalf("duplicate error %q must carry the existing job id %s", err.Error(), existing.ID)
		}
	})

	t.Run("disabled downloads", func(t *testing.T) {
		st := newFakeStore()
		cfg := testConfig(t.TempDir())
		cfg.Enabled = false
		m := newTestManager(t, newFakeEngine(nil), st, cfg, diskOK, nil)
		if _, err := m.Create(ctx, reqFor(user, "aaaa", 10)); !errors.Is(err, ErrNotEnabled) {
			t.Fatalf("want ErrNotEnabled, got %v", err)
		}
	})
}

func TestCreateHappyPathWalksTheStates(t *testing.T) {
	dir := t.TempDir()
	user := uuid.New()
	id := uuid.New()

	files := map[string][]EngineFile{
		id.String(): {
			{Index: 0, Path: "Show - 01.mkv", SizeBytes: 500},
			{Index: 1, Path: "Show - 02.mkv", SizeBytes: 600},
		},
	}
	f := newFakeEngine(files)
	st := newFakeStore()
	st.queueID(id.String())
	hub := &recordingHub{}
	m := newTestManager(t, f, st, testConfig(dir), diskOK, hub)

	job, err := m.Create(context.Background(), reqFor(user, "abc123", 1100))
	if err != nil {
		t.Fatal(err)
	}
	if job.ID != id.String() || job.State != StateQueued || job.UserID != user.String() {
		t.Fatalf("created row = %+v", job)
	}

	// pack (two videos): metadata, then wait for the user's pick
	waitFor(t, "awaiting_selection", func() bool { return st.get(id.String()).State == StateAwaitingSelection })
	if f.addedFor(id) != 1 {
		t.Fatalf("engine Add called %d times, want 1", f.addedFor(id))
	}
	if got := f.added[0].magnet; got != reqFor(user, "abc123", 1100).Magnet {
		t.Fatalf("Add magnet = %q", got)
	}

	got, err := m.SelectFiles(context.Background(), id, user, []int{0, 1})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateDownloading {
		t.Fatalf("after SelectFiles state = %s, want downloading", got.State)
	}
	if sel := st.get(id.String()).SelectedFiles; len(sel) != 2 || sel[0] != 0 || sel[1] != 1 {
		t.Fatalf("stored selection = %v", sel)
	}
	waitFor(t, "engine select", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.selectOps[id.String()]) == 2
	})

	// progress ticks reach the store and the hub
	f.handle(id.String()).bytesDone.Store(300)
	waitFor(t, "progress persisted", func() bool { return st.get(id.String()).ProgressBytes == 300 })
	waitFor(t, "progress event", func() bool { return hub.sawProgress(id.String(), 300) })

	// Done fires only after Select: the walk continues to ready
	close(f.handle(id.String()).done)
	waitFor(t, "ready", func() bool { return st.get(id.String()).State == StateReady })
	if f.verifyOps[id.String()] != 1 {
		t.Fatalf("Verify called %d times, want 1", f.verifyOps[id.String()])
	}
	if c := st.get(id.String()).CompletedAt; c != nil {
		t.Fatalf("completed_at stamped by the manager, want nil (only the importer stamps it)")
	}

	states := hub.statesFor(id.String())
	wantWalk := []string{StateQueued, StateFetchingMetadata, StateAwaitingSelection, StateDownloading,
		StateVerifying, StateImporting, StateReady}
	if len(states) < len(wantWalk) {
		t.Fatalf("hub events = %v, want at least %v", states, wantWalk)
	}
	for i, s := range wantWalk {
		if states[i] != s {
			t.Fatalf("hub walk = %v, want prefix %v", states, wantWalk)
		}
	}
}

func TestCreateSingleEpisodeAutoSelects(t *testing.T) {
	user := uuid.New()
	id := uuid.New()
	files := map[string][]EngineFile{
		id.String(): {
			{Index: 0, Path: "Show - 01.mkv", SizeBytes: 500},
			{Index: 1, Path: "Show - 01.ass", SizeBytes: 50},
		},
	}
	f := newFakeEngine(files)
	st := newFakeStore()
	st.queueID(id.String())
	m := newTestManager(t, f, st, testConfig(t.TempDir()), diskOK, nil)

	if _, err := m.Create(context.Background(), reqFor(user, "abc123", 550)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "downloading without SelectFiles", func() bool { return st.get(id.String()).State == StateDownloading })
	if sel := st.get(id.String()).SelectedFiles; len(sel) != 2 {
		t.Fatalf("auto-select persisted %v, want both indexes", sel)
	}
	close(f.handle(id.String()).done)
	waitFor(t, "ready", func() bool { return st.get(id.String()).State == StateReady })
}

func TestPauseResumeAndCancel(t *testing.T) {
	dir := t.TempDir()
	user := uuid.New()
	ctx := context.Background()
	cfg := testConfig(dir)
	cfg.MaxActivePerUser = 3 // ready jobs count as active, so the second job needs headroom

	driveToDownloading := func(t *testing.T, m *Manager, f *fakeEngine, st *fakeStore) uuid.UUID {
		t.Helper()
		id := uuid.New()
		f.files[id.String()] = []EngineFile{{Index: 0, Path: "Show - 01.mkv", SizeBytes: 500}}
		st.queueID(id.String())
		if _, err := m.Create(ctx, reqFor(user, uuid.NewString(), 500)); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "downloading", func() bool { return st.get(id.String()).State == StateDownloading })
		return id
	}

	st := newFakeStore()
	f := newFakeEngine(map[string][]EngineFile{})
	m := newTestManager(t, f, st, cfg, diskOK, nil)

	id := driveToDownloading(t, m, f, st)

	paused, err := m.Pause(ctx, id, user)
	if err != nil {
		t.Fatal(err)
	}
	if paused.State != StatePaused {
		t.Fatalf("paused state = %s", paused.State)
	}
	waitFor(t, "engine paused", func() bool { return f.pauseOps[id.String()] == 1 })

	resumed, err := m.Resume(ctx, id, user)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.State != StateDownloading {
		t.Fatalf("resumed state = %s, want downloading", resumed.State)
	}
	waitFor(t, "engine resumed", func() bool { return f.resumeOps[id.String()] == 1 })

	close(f.handle(id.String()).done)
	waitFor(t, "ready after resume", func() bool { return st.get(id.String()).State == StateReady })

	// cancel a ready job with deleteFiles: row deleted, torrent dropped, dir removed
	jobDir := filepath.Join(dir, id.String())
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobDir, "Show - 01.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.Cancel(ctx, id, user, true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetDownloadJob(ctx, id.String()); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("ready row survived cancel: %v", err)
	}
	if !f.drops[id.String()] {
		t.Fatalf("engine Drop(true) not recorded: %v", f.drops)
	}
	if _, err := os.Stat(jobDir); !os.IsNotExist(err) {
		t.Fatalf("job dir survived cancel: %v", err)
	}

	// cancel a downloading job without deleteFiles: row stays as cancelled, files kept
	id2 := driveToDownloading(t, m, f, st)
	if err := m.Cancel(ctx, id2, user, false); err != nil {
		t.Fatal(err)
	}
	if got := st.get(id2.String()).State; got != StateCancelled {
		t.Fatalf("cancelled state = %s, want cancelled", got)
	}
	if dropped, ok := f.drops[id2.String()]; !ok || dropped {
		t.Fatalf("engine drop for id2 = %v, %v; want false", dropped, ok)
	}

	// cancel a queued row that never reached the engine (post-restart shape)
	queued := st.seed(repository.DownloadJob{UserID: user.String(), InfoHash: "zzz", State: StateQueued})
	if err := m.Cancel(ctx, uuid.MustParse(queued.ID), user, false); err != nil {
		t.Fatal(err)
	}
	if got := st.get(queued.ID).State; got != StateCancelled {
		t.Fatalf("queued cancel state = %s, want cancelled", got)
	}
}

func TestPauseAndResumeGuardTheTable(t *testing.T) {
	user := uuid.New()
	ctx := context.Background()
	st := newFakeStore()
	m := newTestManager(t, newFakeEngine(nil), st, testConfig(t.TempDir()), diskOK, nil)

	// importing must not pause: the table forbids importing -> paused
	importing := st.seed(repository.DownloadJob{UserID: user.String(), InfoHash: "aaa", State: StateImporting})
	if _, err := m.Pause(ctx, uuid.MustParse(importing.ID), user); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("pause from importing: want ErrConflict, got %v", err)
	}
	// resume requires paused
	downloading := st.seed(repository.DownloadJob{UserID: user.String(), InfoHash: "bbb", State: StateDownloading})
	if _, err := m.Resume(ctx, uuid.MustParse(downloading.ID), user); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("resume from downloading: want ErrConflict, got %v", err)
	}
	// SelectFiles requires awaiting_selection
	pack := st.seed(repository.DownloadJob{UserID: user.String(), InfoHash: "ccc", State: StateDownloading})
	if _, err := m.SelectFiles(ctx, uuid.MustParse(pack.ID), user, []int{0}); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("select on downloading: want ErrConflict, got %v", err)
	}
	// unknown rows 404
	if _, err := m.Pause(ctx, uuid.New(), user); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("pause missing job: want ErrNotFound, got %v", err)
	}
}

func TestStartupResume(t *testing.T) {
	user := uuid.New()
	fmID, pID, rID := uuid.New(), uuid.New(), uuid.New()
	f := newFakeEngine(map[string][]EngineFile{
		fmID.String(): {{Index: 0, Path: "A - 01.mkv", SizeBytes: 100}},
		pID.String():  {{Index: 0, Path: "B - 01.mkv", SizeBytes: 100}},
	})
	st := newFakeStore()
	st.seed(repository.DownloadJob{ID: fmID.String(), UserID: user.String(), InfoHash: "h1", State: StateFetchingMetadata, ResultTitle: "A"})
	st.seed(repository.DownloadJob{ID: pID.String(), UserID: user.String(), InfoHash: "h2", State: StatePaused, ResultTitle: "B", SelectedFiles: []int{0}})
	st.seed(repository.DownloadJob{ID: rID.String(), UserID: user.String(), InfoHash: "h3", State: StateReady, ResultTitle: "C"})

	m := newTestManager(t, f, st, testConfig(t.TempDir()), diskOK, nil)
	if err := m.ResumeFromStore(context.Background()); err != nil {
		t.Fatal(err)
	}

	// fetching_metadata: re-Added and walked on
	waitFor(t, "fetching job re-added", func() bool { return f.addedFor(fmID) == 1 })
	waitFor(t, "fetching job downloading", func() bool { return st.get(fmID.String()).State == StateDownloading })

	// paused: resumed through paused->downloading, stored selection replayed
	waitFor(t, "paused job resumed", func() bool { return st.get(pID.String()).State == StateDownloading })
	waitFor(t, "paused job re-added", func() bool { return f.addedFor(pID) == 1 })
	waitFor(t, "selection replayed", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.selectOps[pID.String()]) == 1
	})
	close(f.handle(pID.String()).done)
	waitFor(t, "resumed job ready", func() bool { return st.get(pID.String()).State == StateReady })

	// ready: untouched
	if got := st.get(rID.String()).State; got != StateReady {
		t.Fatalf("ready job moved to %s", got)
	}
	if f.addedFor(rID) != 0 {
		t.Fatalf("ready job re-added %d times", f.addedFor(rID))
	}
}

func TestMetadataTimeoutFailsTheJob(t *testing.T) {
	user := uuid.New()
	id := uuid.New()
	f := newFakeEngine(nil)
	f.addErr[id.String()] = ErrMetadataTimeout
	st := newFakeStore()
	st.queueID(id.String())
	m := newTestManager(t, f, st, testConfig(t.TempDir()), diskOK, nil)

	// the failure is asynchronous: Create itself answers 201-shaped success
	if _, err := m.Create(context.Background(), reqFor(user, "abc123", 100)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "failed", func() bool { return st.get(id.String()).State == StateFailed })
	job := st.get(id.String())
	if job.Error == nil || *job.Error != "metadata_timeout" {
		t.Fatalf("error = %v, want metadata_timeout", job.Error)
	}
	if job.CompletedAt != nil {
		t.Fatal("completed_at stamped on a failed job, want nil")
	}
	if f.addedFor(id) != 1 {
		t.Fatalf("engine Add called %d times, want exactly 1 (no retry)", f.addedFor(id))
	}
}

func TestSynchronousAddFailureFailsTheJob(t *testing.T) {
	// Task 6 ruling: a duplicate infohash at Add fails synchronously and becomes a
	// job failure, not a 409 — the 409 dedupe lives in Create's database check.
	user := uuid.New()
	id := uuid.New()
	f := newFakeEngine(nil)
	f.addErr[id.String()] = errors.New("downloads: duplicate infohash for an existing torrent")
	st := newFakeStore()
	st.queueID(id.String())
	m := newTestManager(t, f, st, testConfig(t.TempDir()), diskOK, nil)

	if _, err := m.Create(context.Background(), reqFor(user, "abc123", 100)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "failed", func() bool { return st.get(id.String()).State == StateFailed })
	job := st.get(id.String())
	if job.Error == nil || *job.Error != codeDownloadFailed {
		t.Fatalf("error = %v, want %s", job.Error, codeDownloadFailed)
	}
}

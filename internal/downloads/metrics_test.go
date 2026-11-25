package downloads

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Metric smoke: the manager's three hooks (active gauge, bytes and failed counters) fire where
// spec §7.10 names; this file owns that they are called, and that an absent observer is safe.

// recordingObserver captures every hook call without a Prometheus registry.
type recordingObserver struct {
	mu       sync.Mutex
	active   []int
	bytes    []int64
	failures int
}

func (o *recordingObserver) ActiveJobs(n int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.active = append(o.active, n)
}

func (o *recordingObserver) BytesDownloaded(delta int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.bytes = append(o.bytes, delta)
}

func (o *recordingObserver) JobFailed() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.failures++
}

func (o *recordingObserver) recordedBytes() []int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]int64(nil), o.bytes...)
}

func (o *recordingObserver) recordedFailures() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.failures
}

func (o *recordingObserver) sawActive(n int) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, v := range o.active {
		if v == n {
			return true
		}
	}
	return false
}

// singleEpisodeFiles is the minimal manifest that auto-selects: exactly one
// video file, so the drive loop walks awaiting_selection on its own.
func singleEpisodeFiles() []EngineFile {
	return []EngineFile{
		{Index: 0, Path: "Smoke Show S01E01.mkv", SizeBytes: 4 << 20},
	}
}

// waitForState polls the store until the job reaches want, or fails the test.
func waitForState(t *testing.T, st *fakeStore, id string, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if job := st.get(id); job.State == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	job := st.get(id)
	t.Fatalf("job %s never reached %s (state %q, error %v)", id, want, job.State, job.Error)
}

// TestCreateObservesActiveGauge covers the free gauge update: Create's own
// global quota snapshot reaches the observer before any ticker runs.
func TestCreateObservesActiveGauge(t *testing.T) {
	st := newFakeStore()
	obs := &recordingObserver{}
	m := newTestManager(t, newFakeEngine(map[string][]EngineFile{}), st, testConfig(t.TempDir()), t.TempDir(), diskOK, nil)
	m.SetObserver(obs)

	ctx := context.Background()
	_, err := m.Create(ctx, Request{UserID: uuid.New(), Magnet: "magnet:?xt=urn:btih:" + uuid.NewString(), InfoHash: "abc", Title: "x"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !obs.sawActive(1) {
		t.Errorf("observer never saw one active job after create; saw %v", obs.active)
	}
}

// TestProgressTicksFeedBytesCounter drives one job into downloading and asserts the bytes
// counter receives positive deltas, while regressions never count.
func TestProgressTicksFeedBytesCounter(t *testing.T) {
	st := newFakeStore()
	jobID := uuid.NewString()
	st.queueID(jobID)
	files := singleEpisodeFiles()
	eng := newFakeEngine(map[string][]EngineFile{jobID: files})
	obs := &recordingObserver{}
	m := newTestManager(t, eng, st, testConfig(t.TempDir()), t.TempDir(), diskOK, nil)
	m.SetObserver(obs)

	_, err := m.Create(context.Background(), Request{
		UserID: uuid.New(), Magnet: "magnet:?xt=urn:btih:" + uuid.NewString(),
		InfoHash: "abc", Title: "Smoke Show S01E01", SizeBytes: 4 << 20,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	waitForState(t, st, jobID, StateDownloading)

	// Let the ticker seed its baseline while the engine reports zero: after
	// this window every recorded delta must equal the reported growth.
	eng.handle(jobID).bytesDone.Store(0)
	time.Sleep(100 * time.Millisecond)

	eng.handle(jobID).bytesDone.Store(1 << 20)
	waitForBytesObservations(t, obs, 1)

	// A regression in reported progress must not count.
	eng.handle(jobID).bytesDone.Store(512 << 10)
	time.Sleep(100 * time.Millisecond)

	// Growth again: only the new bytes are counted.
	eng.handle(jobID).bytesDone.Store(3 << 20)
	waitForBytesObservations(t, obs, 2)
	m.Close()

	var counted int64
	for _, d := range obs.recordedBytes() {
		if d <= 0 {
			t.Fatalf("non-positive delta recorded: %v", obs.recordedBytes())
		}
		counted += d
	}
	// A regression is not counted, but it does re-base the next delta: the
	// counter total is (0->1MiB) + (512KiB->3MiB), never the regressed span.
	if want := int64(1<<20) + int64(3<<20) - int64(512<<10); counted != want {
		t.Errorf("counter total = %d, want %d", counted, want)
	}
}

func waitForBytesObservations(t *testing.T, obs *recordingObserver, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(obs.recordedBytes()) >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("observer only recorded %d byte observations, wanted %d", len(obs.recordedBytes()), n)
}

// TestFailedTransitionCountsJobFailed fails a job at metadata time and asserts
// exactly one failed_total increment.
func TestFailedTransitionCountsJobFailed(t *testing.T) {
	st := newFakeStore()
	jobID := uuid.NewString()
	st.queueID(jobID)
	eng := newFakeEngine(map[string][]EngineFile{jobID: singleEpisodeFiles()})
	eng.addErr[jobID] = ErrMetadataTimeout
	obs := &recordingObserver{}
	m := newTestManager(t, eng, st, testConfig(t.TempDir()), t.TempDir(), diskOK, nil)
	m.SetObserver(obs)

	_, err := m.Create(context.Background(), Request{
		UserID: uuid.New(), Magnet: "magnet:?xt=urn:btih:" + uuid.NewString(),
		InfoHash: "abc", Title: "Smoke Show S01E01",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if job := st.get(jobID); job.State == StateFailed {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if job := st.get(jobID); job.State != StateFailed {
		t.Fatalf("job never failed (state %q)", job.State)
	}
	if n := obs.recordedFailures(); n != 1 {
		t.Errorf("failed_total hook fired %d times, want 1", n)
	}
}

// TestManagerWithoutObserverStillDrives: every hook must be nil-safe, because
// the wiring only installs an observer when metrics are enabled.
func TestManagerWithoutObserverStillDrives(t *testing.T) {
	st := newFakeStore()
	jobID := uuid.NewString()
	st.queueID(jobID)
	eng := newFakeEngine(map[string][]EngineFile{jobID: singleEpisodeFiles()})
	m := newTestManager(t, eng, st, testConfig(t.TempDir()), t.TempDir(), diskOK, nil)

	job, err := m.Create(context.Background(), Request{
		UserID: uuid.New(), Magnet: "magnet:?xt=urn:btih:" + uuid.NewString(),
		InfoHash: "abc", Title: "Smoke Show S01E01",
	})
	if err != nil {
		t.Fatalf("create without observer: %v", err)
	}
	waitForState(t, st, job.ID, StateDownloading)
	eng.handle(job.ID).bytesDone.Store(1 << 20)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && st.get(job.ID).ProgressBytes == 0 {
		time.Sleep(2 * time.Millisecond)
	}
	if got := st.get(job.ID); got.ProgressBytes != 1<<20 {
		t.Errorf("progress not recorded without an observer: %d", got.ProgressBytes)
	}
}

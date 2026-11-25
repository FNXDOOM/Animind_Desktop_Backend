package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"animind-backend-go/internal/auth"
	"animind-backend-go/internal/downloads"
	"animind-backend-go/internal/repository"
	"animind-backend-go/internal/torrent"
)

// Handler tests for the download surface: create, list, get, patch, delete and the SSE
// stream, all on fakes; the admin PATCH persistence test uses the gated M4 harness.

// Account ids are uuids because the manager's request carries uuid.UUID.
const (
	userOneID    = "00000000-0000-0000-0000-000000000001"
	userTwoID    = "00000000-0000-0000-0000-000000000002"
	userThreeID  = "00000000-0000-0000-0000-000000000003"
	adminUserID  = "00000000-0000-0000-0000-00000000000a"
	jobUUID      = "00000000-0000-0000-0000-0000000000aa"
	otherJobUUID = "00000000-0000-0000-0000-0000000000bb"
)

// ── Fakes over the handler seams ─────────────────────────────────────────────

type fakeTorrentService struct {
	resp    torrent.SearchResponse
	err     error
	results map[string]torrent.Result
	queries []torrent.Query
}

func (f *fakeTorrentService) Search(_ context.Context, q torrent.Query) (torrent.SearchResponse, error) {
	f.queries = append(f.queries, q)
	return f.resp, f.err
}

func (f *fakeTorrentService) Result(id string) (torrent.Result, bool) {
	r, ok := f.results[id]
	return r, ok
}

type fakeDownloadsManager struct {
	createJob repository.DownloadJob
	createErr error

	pauseJob  repository.DownloadJob
	pauseErr  error
	resumeJob repository.DownloadJob
	resumeErr error
	selectJob repository.DownloadJob
	selectErr error
	cancelErr error

	lastCreate      downloads.Request
	lastPauseActor  string
	lastPauseAdmin  bool
	lastResumeActor string
	lastSelectActor string
	lastSelectIdxs  []int
	lastCancelID    string
	lastCancelAdmin bool
	lastDeleteFiles bool

	live  bool
	stats downloads.Stats
	files []downloads.EngineFile
}

func (f *fakeDownloadsManager) Create(_ context.Context, req downloads.Request) (repository.DownloadJob, error) {
	f.lastCreate = req
	return f.createJob, f.createErr
}

func (f *fakeDownloadsManager) Pause(_ context.Context, _, actorUserID string, isAdmin bool) (repository.DownloadJob, error) {
	f.lastPauseActor, f.lastPauseAdmin = actorUserID, isAdmin
	return f.pauseJob, f.pauseErr
}

func (f *fakeDownloadsManager) Resume(_ context.Context, _, actorUserID string, _ bool) (repository.DownloadJob, error) {
	f.lastResumeActor = actorUserID
	return f.resumeJob, f.resumeErr
}

func (f *fakeDownloadsManager) Cancel(_ context.Context, jobID, _ string, isAdmin, deleteFiles bool) error {
	f.lastCancelID, f.lastCancelAdmin, f.lastDeleteFiles = jobID, isAdmin, deleteFiles
	return f.cancelErr
}

func (f *fakeDownloadsManager) SelectFiles(_ context.Context, _, actorUserID string, _ bool, indexes []int) (repository.DownloadJob, error) {
	f.lastSelectActor, f.lastSelectIdxs = actorUserID, indexes
	return f.selectJob, f.selectErr
}

func (f *fakeDownloadsManager) Snapshot(string) (downloads.Stats, []downloads.EngineFile, bool) {
	return f.stats, f.files, f.live
}

type fakeJobStore struct {
	jobs []repository.DownloadJob
	byID map[string]repository.DownloadJob

	lastListUser *string
	lastListAll  bool
	lastLimit    int
}

func (f *fakeJobStore) ListDownloadJobs(_ context.Context, userID *string, all bool, limit int) ([]repository.DownloadJob, error) {
	f.lastListUser, f.lastListAll, f.lastLimit = userID, all, limit
	return f.jobs, nil
}

func (f *fakeJobStore) GetDownloadJob(_ context.Context, id string) (repository.DownloadJob, error) {
	if job, ok := f.byID[id]; ok {
		return job, nil
	}
	return repository.DownloadJob{}, repository.ErrNotFound
}

type fakeEntitlements struct{ allowed map[string]bool }

func (f fakeEntitlements) CanDownload(_ context.Context, userID string) (bool, error) {
	return f.allowed[userID], nil
}

var _ downloadsManagerAPI = (*fakeDownloadsManager)(nil)
var _ downloadsJobStore = (*fakeJobStore)(nil)
var _ downloadEntitlement = fakeEntitlements{}
var _ torrentServiceAPI = (*fakeTorrentService)(nil)

// ── Fixtures ─────────────────────────────────────────────────────────────────

func storedJob(id, userID string) repository.DownloadJob {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	return repository.DownloadJob{
		ID: id, UserID: userID, ResultTitle: "Show - 01 (1080p)", InfoHash: "aa11",
		SizeBytes: 1000, State: "downloading", ProgressBytes: 250,
		SelectedFiles: []int{0, 2},
		CreatedAt:     now, UpdatedAt: now,
	}
}

func installJobStore(srv *Server, jobs ...repository.DownloadJob) *fakeJobStore {
	store := &fakeJobStore{jobs: jobs, byID: make(map[string]repository.DownloadJob, len(jobs))}
	for _, job := range jobs {
		store.byID[job.ID] = job
	}
	srv.jobStore = store
	return store
}

func installDefaults(srv *Server, u sessionUser, mutate func(*fakeDownloadsManager)) *fakeDownloadsManager {
	fake := &fakeDownloadsManager{}
	if mutate != nil {
		mutate(fake)
	}
	srv.downloads = fake
	srv.entitlements = u.entitlements()
	return fake
}

func searchResultFor(id string) torrent.Result {
	return torrent.Result{ID: id, InfoHash: "aa11", Title: "Show - 01", SizeBytes: 1000,
		Magnet: "magnet:?xt=urn:btih:aa11"}
}

// ── Create ───────────────────────────────────────────────────────────────────

func TestDownloadCreateAndGuards(t *testing.T) {
	body := func(resultID string) map[string]any { return map[string]any{"resultId": resultID} }

	t.Run("happy path resolves the opaque id and returns the job", func(t *testing.T) {
		srv := testServerWith(t, func(d *Deps) {
			d.Torrent = &fakeTorrentService{results: map[string]torrent.Result{"opaque": searchResultFor("opaque")}}
		})
		caller := user(userOneID).withCanDownload()
		fake := installDefaults(srv, caller, func(m *fakeDownloadsManager) {
			m.createJob = storedJob(jobUUID, userOneID)
			m.live = true
			m.stats = downloads.Stats{BytesDone: 250, BytesTotal: 1000, DownKbps: 2400, Peers: 18, ETA: 400 * time.Second}
			m.files = []downloads.EngineFile{{Index: 0, Path: "Show - 01.mkv", SizeBytes: 1000, Progress: 250}}
		})

		rec := callAs(t, caller, srv.handleDownloadCreate, http.MethodPost, "/api/downloads", nil, body("opaque"))
		if rec.Code != http.StatusCreated {
			t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
		}
		req := fake.lastCreate
		if req.UserID.String() != userOneID {
			t.Errorf("manager got user %q", req.UserID)
		}
		if req.Magnet != "magnet:?xt=urn:btih:aa11" || req.InfoHash != "aa11" || req.Title != "Show - 01" || req.SizeBytes != 1000 {
			t.Errorf("manager got %+v", req)
		}

		var job map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, key := range []string{"id", "resultTitle", "infoHash", "state", "progressBytes", "sizeBytes",
			"progressPct", "downKbps", "upKbps", "peers", "etaSec", "error", "showId",
			"createdAt", "updatedAt", "completedAt", "files"} {
			if _, has := job[key]; !has {
				t.Errorf("job JSON is missing %q", key)
			}
		}
		if job["state"] != "downloading" || job["progressPct"] != 25.0 || job["downKbps"] != 2400.0 {
			t.Errorf("live fields wrong: %v", job)
		}
		files, _ := job["files"].([]any)
		if len(files) != 1 {
			t.Fatalf("files = %v", job["files"])
		}
		file := files[0].(map[string]any)
		if file["path"] != "Show - 01.mkv" || file["sizeBytes"] != 1000.0 || file["progressBytes"] != 250.0 || file["selected"] != true {
			t.Errorf("file entry wrong: %v", file)
		}
	})

	t.Run("unknown or expired result id answers 410", func(t *testing.T) {
		srv := testServerWith(t, func(d *Deps) {
			d.Torrent = &fakeTorrentService{results: map[string]torrent.Result{}}
		})
		caller := user(userOneID).withCanDownload()
		installDefaults(srv, caller, nil)

		rec := callAs(t, caller, srv.handleDownloadCreate, http.MethodPost, "/api/downloads", nil, body("stale"))
		if rec.Code != http.StatusGone {
			t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
		}
		assertCode(t, rec.Body.Bytes(), "result_expired")
	})

	t.Run("a user without can_download is refused", func(t *testing.T) {
		srv := testServerWith(t, func(d *Deps) {
			d.Torrent = &fakeTorrentService{results: map[string]torrent.Result{"opaque": searchResultFor("opaque")}}
		})
		caller := user(userOneID) // canDownload: false
		installDefaults(srv, caller, nil)

		rec := callAs(t, caller, srv.handleDownloadCreate, http.MethodPost, "/api/downloads", nil, body("opaque"))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
		}
		assertCode(t, rec.Body.Bytes(), "download_forbidden")
	})

	t.Run("an admin bypasses the entitlement", func(t *testing.T) {
		srv := testServerWith(t, func(d *Deps) {
			d.Torrent = &fakeTorrentService{results: map[string]torrent.Result{"opaque": searchResultFor("opaque")}}
		})
		caller := user(adminUserID).withAdmin() // no can_download
		installDefaults(srv, caller, func(m *fakeDownloadsManager) { m.createJob = storedJob(jobUUID, adminUserID) })

		rec := callAs(t, caller, srv.handleDownloadCreate, http.MethodPost, "/api/downloads", nil, body("opaque"))
		if rec.Code != http.StatusCreated {
			t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("manager sentinels map to their codes", func(t *testing.T) {
		cases := []struct {
			name     string
			err      error
			status   int
			code     string
			existing string
		}{
			{"disk full", downloads.ErrDiskFull, http.StatusInsufficientStorage, "insufficient_disk", ""},
			{"too large", downloads.ErrTooLarge, http.StatusBadRequest, "job_too_large", ""},
			{"quota", downloads.ErrTooManyActive, http.StatusForbidden, "download_forbidden", ""},
			{"disabled", downloads.ErrNotEnabled, http.StatusNotImplemented, "downloads_unavailable", ""},
			{"duplicate", &downloads.DuplicateJobError{JobID: otherJobUUID}, http.StatusConflict, "", otherJobUUID},
			// A wrapped sentinel without the typed id still answers 409, with no job id.
			{"duplicate without a typed id", fmt.Errorf("wrapped: %w", downloads.ErrDuplicate), http.StatusConflict, "", ""},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				srv := testServerWith(t, func(d *Deps) {
					d.Torrent = &fakeTorrentService{results: map[string]torrent.Result{"opaque": searchResultFor("opaque")}}
				})
				caller := user(userOneID).withCanDownload()
				installDefaults(srv, caller, func(m *fakeDownloadsManager) { m.createErr = tc.err })

				rec := callAs(t, caller, srv.handleDownloadCreate, http.MethodPost, "/api/downloads", nil, body("opaque"))
				if rec.Code != tc.status {
					t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
				}
				if tc.code != "" {
					assertCode(t, rec.Body.Bytes(), tc.code)
				}
				if tc.existing != "" {
					var payload struct {
						ExistingJobID string `json:"existingJobId"`
					}
					if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
						t.Fatalf("decode: %v", err)
					}
					if payload.ExistingJobID != tc.existing {
						t.Errorf("existingJobId = %q, want %q", payload.ExistingJobID, tc.existing)
					}
				}
			})
		}
	})

	t.Run("s3 mode short-circuits before anything else", func(t *testing.T) {
		srv := testServerWith(t, func(d *Deps) { d.Config.Downloads.Enabled = false })
		caller := user(userOneID).withCanDownload()
		installDefaults(srv, caller, nil)

		rec := callAs(t, caller, srv.handleDownloadCreate, http.MethodPost, "/api/downloads", nil, body("opaque"))
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
		}
		assertCode(t, rec.Body.Bytes(), "downloads_unavailable")
	})

	t.Run("a missing or empty result id is a 400", func(t *testing.T) {
		srv := testServerWith(t, func(d *Deps) { d.Torrent = &fakeTorrentService{} })
		caller := user(userOneID).withCanDownload()
		installDefaults(srv, caller, nil)

		rec := callAs(t, caller, srv.handleDownloadCreate, http.MethodPost, "/api/downloads", nil, body(""))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("empty id = %d, want 400", rec.Code)
		}
		rec = callAs(t, caller, srv.handleDownloadCreate, http.MethodPost, "/api/downloads", nil, map[string]any{"junk": true})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("unknown field = %d, want 400 (strict decoding)", rec.Code)
		}
	})
}

// ── List / get / patch / delete ───────────────────────────────────────────────

func TestDownloadListGetPatchDeleteAndSSE(t *testing.T) {
	t.Run("list returns newest first and admin all lists everyone", func(t *testing.T) {
		srv := testServerWith(t, nil)
		mine := storedJob(jobUUID, userOneID)
		theirs := storedJob(otherJobUUID, userTwoID)
		theirs.CreatedAt = mine.CreatedAt.Add(-time.Minute) // older row, listed second

		caller := user(userOneID)
		installDefaults(srv, caller, nil)
		store := installJobStore(srv, mine, theirs)

		rec := callAs(t, caller, srv.handleDownloadList, http.MethodGet, "/api/downloads?limit=50", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
		}
		if store.lastListUser == nil || *store.lastListUser != userOneID || store.lastListAll || store.lastLimit != 50 {
			t.Errorf("list scope wrong: user=%v all=%v limit=%d", store.lastListUser, store.lastListAll, store.lastLimit)
		}
		var list struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(list.Data) != 2 || list.Data[0]["id"] != jobUUID || list.Data[1]["id"] != otherJobUUID {
			t.Errorf("list order wrong: %v", list.Data)
		}

		admin := user(adminUserID).withAdmin()
		rec = callAs(t, admin, srv.handleDownloadList, http.MethodGet, "/api/downloads?all=true", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("admin status: %d", rec.Code)
		}
		if store.lastListUser != nil || !store.lastListAll {
			t.Errorf("admin all=true scope wrong: user=%v all=%v", store.lastListUser, store.lastListAll)
		}
	})

	t.Run("get returns the job and 404s foreign and missing ids", func(t *testing.T) {
		srv := testServerWith(t, nil)
		installDefaults(srv, user(userOneID), func(m *fakeDownloadsManager) { m.live = true })
		installJobStore(srv, storedJob(jobUUID, userOneID), storedJob(otherJobUUID, userTwoID))

		rec := callAs(t, user(userOneID), srv.handleDownloadGet, http.MethodGet,
			"/api/downloads/"+jobUUID, map[string]string{"id": jobUUID}, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
		}

		// Another user's job is a 404, not a 403: the id space must not be probeable.
		rec = callAs(t, user(userOneID), srv.handleDownloadGet, http.MethodGet,
			"/api/downloads/"+otherJobUUID, map[string]string{"id": otherJobUUID}, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("foreign job = %d, want 404", rec.Code)
		}
		rec = callAs(t, user(userOneID), srv.handleDownloadGet, http.MethodGet,
			"/api/downloads/missing", map[string]string{"id": "missing"}, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("missing job = %d, want 404", rec.Code)
		}

		// An admin may read anyone's job.
		rec = callAs(t, user(adminUserID).withAdmin(), srv.handleDownloadGet, http.MethodGet,
			"/api/downloads/"+otherJobUUID, map[string]string{"id": otherJobUUID}, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("admin get = %d, want 200", rec.Code)
		}
	})

	t.Run("patch pauses, resumes and selects files", func(t *testing.T) {
		srv := testServerWith(t, nil)
		caller := user(userOneID)
		fake := installDefaults(srv, caller, func(m *fakeDownloadsManager) {
			m.pauseJob = storedJob(jobUUID, userOneID)
			m.selectJob = storedJob(jobUUID, userOneID)
		})
		installJobStore(srv, storedJob(jobUUID, userOneID))

		rec := callAs(t, caller, srv.handleDownloadPatch, http.MethodPatch,
			"/api/downloads/"+jobUUID, map[string]string{"id": jobUUID}, map[string]any{"action": "pause"})
		if rec.Code != http.StatusOK {
			t.Fatalf("pause: %d %s", rec.Code, rec.Body.String())
		}
		if fake.lastPauseActor != userOneID || fake.lastPauseAdmin {
			t.Errorf("pause actor wrong: %+v", fake)
		}

		rec = callAs(t, caller, srv.handleDownloadPatch, http.MethodPatch,
			"/api/downloads/"+jobUUID, map[string]string{"id": jobUUID}, map[string]any{"action": "resume"})
		if rec.Code != http.StatusOK {
			t.Fatalf("resume: %d %s", rec.Code, rec.Body.String())
		}

		rec = callAs(t, caller, srv.handleDownloadPatch, http.MethodPatch,
			"/api/downloads/"+jobUUID, map[string]string{"id": jobUUID}, map[string]any{"fileIndexes": []int{0, 2}})
		if rec.Code != http.StatusOK {
			t.Fatalf("select: %d %s", rec.Code, rec.Body.String())
		}
		if len(fake.lastSelectIdxs) != 2 || fake.lastSelectIdxs[1] != 2 {
			t.Errorf("selection forwarded wrong: %v", fake.lastSelectIdxs)
		}

		// A state conflict (e.g. selecting while downloading) is a 409.
		fake.selectErr = fmt.Errorf("downloads: job %s is downloading, not awaiting_selection: %w", jobUUID, repository.ErrConflict)
		rec = callAs(t, caller, srv.handleDownloadPatch, http.MethodPatch,
			"/api/downloads/"+jobUUID, map[string]string{"id": jobUUID}, map[string]any{"fileIndexes": []int{0}})
		fake.selectErr = nil
		if rec.Code != http.StatusConflict {
			t.Fatalf("select conflict = %d, want 409", rec.Code)
		}

		// A selection the torrent cannot honour is the only user-fixable failure: 400.
		fake.selectErr = downloads.ErrInvalidSelection
		rec = callAs(t, caller, srv.handleDownloadPatch, http.MethodPatch,
			"/api/downloads/"+jobUUID, map[string]string{"id": jobUUID}, map[string]any{"fileIndexes": []int{0}})
		fake.selectErr = nil
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid selection = %d, want 400", rec.Code)
		}
		assertCode(t, rec.Body.Bytes(), "invalid_file_selection")

		// A server fault must never wear the 400 selection code; it keeps its 500 class.
		fake.selectErr = errors.New("downloads: store unavailable")
		rec = callAs(t, caller, srv.handleDownloadPatch, http.MethodPatch,
			"/api/downloads/"+jobUUID, map[string]string{"id": jobUUID}, map[string]any{"fileIndexes": []int{0}})
		fake.selectErr = nil
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("server fault on select = %d, want 500", rec.Code)
		}

		// An empty selection is a client error with its own code.
		rec = callAs(t, caller, srv.handleDownloadPatch, http.MethodPatch,
			"/api/downloads/"+jobUUID, map[string]string{"id": jobUUID}, map[string]any{"fileIndexes": []int{}})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("empty selection = %d, want 400", rec.Code)
		}
		assertCode(t, rec.Body.Bytes(), "invalid_file_selection")

		rec = callAs(t, caller, srv.handleDownloadPatch, http.MethodPatch,
			"/api/downloads/"+jobUUID, map[string]string{"id": jobUUID}, map[string]any{})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("empty body = %d, want 400", rec.Code)
		}
	})

	t.Run("patch and delete refuse foreign jobs with 404", func(t *testing.T) {
		srv := testServerWith(t, nil)
		caller := user(userOneID)
		installDefaults(srv, caller, nil)
		installJobStore(srv, storedJob(otherJobUUID, userTwoID))

		rec := callAs(t, caller, srv.handleDownloadPatch, http.MethodPatch,
			"/api/downloads/"+otherJobUUID, map[string]string{"id": otherJobUUID}, map[string]any{"action": "pause"})
		if rec.Code != http.StatusNotFound {
			t.Fatalf("foreign patch = %d, want 404", rec.Code)
		}
		rec = callAs(t, caller, srv.handleDownloadDelete, http.MethodDelete,
			"/api/downloads/"+otherJobUUID, map[string]string{"id": otherJobUUID}, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("foreign delete = %d, want 404", rec.Code)
		}
	})

	t.Run("delete forwards deleteFiles and honours the entitlement", func(t *testing.T) {
		srv := testServerWith(t, nil)
		caller := user(userOneID).withCanDownload()
		installDefaults(srv, caller, nil)
		installJobStore(srv, storedJob(jobUUID, userOneID))

		rec := callAs(t, caller, srv.handleDownloadDelete, http.MethodDelete,
			"/api/downloads/"+jobUUID+"?deleteFiles=true", map[string]string{"id": jobUUID}, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
		}
		fake := srv.downloads.(*fakeDownloadsManager)
		if fake.lastCancelID != jobUUID || !fake.lastDeleteFiles || fake.lastCancelAdmin {
			t.Errorf("cancel forwarded wrong: %+v", fake)
		}

		// A user without the entitlement may not cancel either; here they own the job, so the
		// refusal is the entitlement check, not ownership.
		blocked := user(userThreeID)
		srv.entitlements = blocked.entitlements()
		installJobStore(srv, storedJob(jobUUID, userThreeID))
		rec = callAs(t, blocked, srv.handleDownloadDelete, http.MethodDelete,
			"/api/downloads/"+jobUUID, map[string]string{"id": jobUUID}, nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("blocked delete = %d, want 403", rec.Code)
		}
		assertCode(t, rec.Body.Bytes(), "download_forbidden")
	})

	t.Run("sse streams a snapshot then live events and cleans up", func(t *testing.T) {
		hub := downloads.NewEventHub()
		srv := testServerWith(t, func(d *Deps) { d.EventHub = hub })

		// A pre-existing job state becomes the snapshot on connect.
		hub.Publish(userOneID, downloads.JobEvent{ID: "job-a", State: "downloading", Progress: 42.5, SizeBytes: 1000, BytesDone: 425})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		request := httptest.NewRequest(http.MethodGet, "/api/downloads/events", nil)
		request = request.WithContext(WithPrincipal(ctx, user(userOneID).principal()))
		recorder := newSyncRecorder()
		done := make(chan struct{})
		go func() {
			srv.handleDownloadEvents(recorder, request)
			close(done)
		}()

		waitUntil(t, func() bool { return hub.Len() == 1 }, "the stream never subscribed")

		// A different job id dodges the hub's one-second coalesce window.
		hub.Publish(userOneID, downloads.JobEvent{ID: "job-b", State: "ready", Progress: 100})
		waitUntil(t, func() bool { return strings.Contains(recorder.body(), "job-b") }, "the live event never arrived")
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("the handler did not return on disconnect")
		}
		waitUntil(t, func() bool { return hub.Len() == 0 }, "the subscription was not removed on disconnect")

		frame := recorder.body()
		if !strings.Contains(recorder.header().Get("Content-Type"), "text/event-stream") {
			t.Errorf("content type = %q", recorder.header().Get("Content-Type"))
		}
		if !strings.Contains(frame, "event: snapshot") || !strings.Contains(frame, `"job-a"`) {
			t.Errorf("snapshot frame missing: %q", frame)
		}
		if !strings.Contains(frame, "event: job") || !strings.Contains(frame, `"job-b"`) {
			t.Errorf("live frame missing: %q", frame)
		}
	})

	t.Run("sse refuses the third concurrent stream for a user", func(t *testing.T) {
		hub := downloads.NewEventHub()
		srv := testServerWith(t, func(d *Deps) { d.EventHub = hub })
		// The hub caps one user at two live streams.
		for range 2 {
			if _, _, cancel, ok := hub.Subscribe(userOneID); ok {
				defer cancel()
			}
		}

		rec := callAs(t, user(userOneID), srv.handleDownloadEvents, http.MethodGet, "/api/downloads/events", nil, nil)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("capped stream = %d, want 503", rec.Code)
		}
	})

	t.Run("sse is unavailable in s3 mode", func(t *testing.T) {
		srv := testServerWith(t, func(d *Deps) {
			d.Config.Downloads.Enabled = false
			d.EventHub = downloads.NewEventHub()
		})
		rec := callAs(t, user(userOneID), srv.handleDownloadEvents, http.MethodGet, "/api/downloads/events", nil, nil)
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("s3 sse = %d, want 501", rec.Code)
		}
		assertCode(t, rec.Body.Bytes(), "downloads_unavailable")
	})
}

// waitUntil spins until cond holds, failing after two seconds; SSE handlers run on their own
// goroutine, so a plain assertion would race them.
func waitUntil(t *testing.T, cond func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(message)
}

// syncRecorder wraps a ResponseRecorder in a mutex: the SSE handler writes from its own goroutine.
type syncRecorder struct {
	mu  sync.Mutex
	rec *httptest.ResponseRecorder
}

func newSyncRecorder() *syncRecorder { return &syncRecorder{rec: httptest.NewRecorder()} }

func (s *syncRecorder) Header() http.Header { return s.rec.Header() }

func (s *syncRecorder) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec.Write(p)
}

func (s *syncRecorder) WriteHeader(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.WriteHeader(status)
}

// Flush satisfies http.Flusher; the recorder's own Flush is a no-op.
func (s *syncRecorder) Flush() {}

func (s *syncRecorder) body() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec.Body.String()
}

func (s *syncRecorder) header() http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec.Header()
}

// ── Admin PATCH (persistence is DB-gated) ────────────────────────────────────

// promoteAdmin writes the flag and evicts the cached identity: IsAdmin is part of the cached
// principal, so any writer outside handleAdminSetUserAdmin must reproduce the eviction.
func promoteAdmin(t *testing.T, store *repository.Store, service *auth.Service, userID string) {
	t.Helper()
	if err := store.SetAdmin(context.Background(), userID, true); err != nil {
		t.Fatalf("promote admin: %v", err)
	}
	service.InvalidateUser(context.Background(), userID)
}

// TestAdminPatchCanDownloadPersists runs the admin entitlement toggle through the real store,
// skipping without POSTGRES_TEST_DATABASE_URL like every other database suite.
func TestAdminPatchCanDownloadPersists(t *testing.T) {
	srv, store, service := sharedHarness(t)
	admin := signUpAccount(t, service)
	adminID := meUserID(t, srv, admin)
	promoteAdmin(t, store, service, adminID)
	target := signUpAccount(t, service)
	targetID := meUserID(t, srv, target)

	rec := admin.do(t, srv, http.MethodPatch, "/api/admin/users/"+targetID, map[string]any{"can_download": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d %s", rec.Code, rec.Body.String())
	}
	if !canDownloadOf(t, store, targetID) {
		t.Error("can_download did not persist as true")
	}

	rec = admin.do(t, srv, http.MethodPatch, "/api/admin/users/"+targetID, map[string]any{"can_download": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch off = %d %s", rec.Code, rec.Body.String())
	}
	if canDownloadOf(t, store, targetID) {
		t.Error("can_download did not persist as false")
	}

	// A non-admin caller must be refused by the route's RequireAdmin.
	plain := signUpAccount(t, service)
	rec = plain.do(t, srv, http.MethodPatch, "/api/admin/users/"+targetID, map[string]any{"can_download": true})
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-admin patch = %d, want 403", rec.Code)
	}
}

// meUserID resolves an account's user id through one authenticated call.
func meUserID(t *testing.T, srv *Server, acc account) string {
	t.Helper()
	rec := acc.do(t, srv, http.MethodGet, "/api/auth/me", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("auth/me = %d %s", rec.Code, rec.Body.String())
	}
	var me struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatalf("decode me: %v", err)
	}
	if me.User.ID == "" {
		t.Fatal("auth/me returned no user id")
	}
	return me.User.ID
}

func canDownloadOf(t *testing.T, store *repository.Store, userID string) bool {
	t.Helper()
	users, err := store.ListUsers(context.Background(), 200, 0)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	for _, u := range users {
		if u.ID == userID {
			return u.CanDownload
		}
	}
	t.Fatalf("user %s not listed", userID)
	return false
}

func isAdminOf(t *testing.T, store *repository.Store, userID string) bool {
	t.Helper()
	users, err := store.ListUsers(context.Background(), 200, 0)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	for _, u := range users {
		if u.ID == userID {
			return u.IsAdmin
		}
	}
	t.Fatalf("user %s not listed", userID)
	return false
}

// TestSetAdminRequestShape pins the payload contract: is_admin is optional, so a
// can_download-only PATCH decodes with no implied false.
func TestSetAdminRequestShape(t *testing.T) {
	var req setAdminRequest
	if err := json.Unmarshal([]byte(`{"can_download":true}`), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.IsAdmin != nil {
		t.Fatal("a can_download-only payload must leave IsAdmin nil, not an implied false")
	}
	if req.CanDownload == nil || !*req.CanDownload {
		t.Fatal("can_download must decode")
	}

	var legacy setAdminRequest
	if err := json.Unmarshal([]byte(`{"is_admin":false}`), &legacy); err != nil {
		t.Fatalf("decode legacy: %v", err)
	}
	if legacy.IsAdmin == nil || *legacy.IsAdmin {
		t.Fatal("a legacy is_admin payload must decode as present-and-false")
	}
}

// TestAdminPatchIsAdminOptional runs the optional is_admin semantics through the real store:
// a can_download-only PATCH must not demote, and the legacy flow keeps the self-demote guard.
func TestAdminPatchIsAdminOptional(t *testing.T) {
	srv, store, service := sharedHarness(t)

	admin := signUpAccount(t, service)
	adminID := meUserID(t, srv, admin)
	promoteAdmin(t, store, service, adminID)

	t.Run("can_download-only patch leaves is_admin untouched", func(t *testing.T) {
		target := signUpAccount(t, service)
		targetID := meUserID(t, srv, target)
		promoteAdmin(t, store, service, targetID)

		rec := admin.do(t, srv, http.MethodPatch, "/api/admin/users/"+targetID, map[string]any{"can_download": true})
		if rec.Code != http.StatusOK {
			t.Fatalf("patch = %d %s", rec.Code, rec.Body.String())
		}
		if !isAdminOf(t, store, targetID) {
			t.Error("a can_download-only PATCH demoted the admin target")
		}
		if !canDownloadOf(t, store, targetID) {
			t.Error("can_download did not persist as true")
		}
	})

	t.Run("an admin can toggle their own can_download", func(t *testing.T) {
		rec := admin.do(t, srv, http.MethodPatch, "/api/admin/users/"+adminID, map[string]any{"can_download": false})
		if rec.Code != http.StatusOK {
			t.Fatalf("self patch = %d %s", rec.Code, rec.Body.String())
		}
		if canDownloadOf(t, store, adminID) {
			t.Error("own can_download did not persist as false")
		}
		if !isAdminOf(t, store, adminID) {
			t.Error("toggling own can_download demoted the admin")
		}
	})

	t.Run("legacy is_admin flow is unchanged", func(t *testing.T) {
		target := signUpAccount(t, service)
		targetID := meUserID(t, srv, target)

		rec := admin.do(t, srv, http.MethodPatch, "/api/admin/users/"+targetID, map[string]any{"is_admin": true})
		if rec.Code != http.StatusOK {
			t.Fatalf("promote patch = %d %s", rec.Code, rec.Body.String())
		}
		if !isAdminOf(t, store, targetID) {
			t.Error("legacy is_admin promotion did not persist")
		}

		// The self-demote guard still fires when the request really changes is_admin on self.
		rec = admin.do(t, srv, http.MethodPatch, "/api/admin/users/"+adminID, map[string]any{"is_admin": false})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("self demote = %d, want 400 (body %s)", rec.Code, rec.Body.String())
		}
		if !isAdminOf(t, store, adminID) {
			t.Error("the self-demote guard did not hold the admin flag")
		}
	})
}

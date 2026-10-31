package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/google/uuid"

	"animind-backend-go/internal/auth"
	"animind-backend-go/internal/downloads"
	"animind-backend-go/internal/repository"
)

// Handlers for the download surface: create, list, get, patch, delete and the
// SSE stream. Every route requires a session; create and delete additionally
// require the can_download entitlement (admins exempt). Job ids that belong to
// another user answer 404 like a missing row, so the id space cannot be
// probed; the manager enforces ownership again behind the handlers.

// downloadsManagerAPI is the narrow seam the download handlers need from the
// job manager; *downloads.Manager satisfies it as-is.
type downloadsManagerAPI interface {
	Create(ctx context.Context, req downloads.Request) (repository.DownloadJob, error)
	Pause(ctx context.Context, jobID, actorUserID string, isAdmin bool) (repository.DownloadJob, error)
	Resume(ctx context.Context, jobID, actorUserID string, isAdmin bool) (repository.DownloadJob, error)
	Cancel(ctx context.Context, jobID, actorUserID string, isAdmin, deleteFiles bool) error
	SelectFiles(ctx context.Context, jobID, actorUserID string, isAdmin bool, indexes []int) (repository.DownloadJob, error)
	Snapshot(jobID string) (downloads.Stats, []downloads.EngineFile, bool)
}

var _ downloadsManagerAPI = (*downloads.Manager)(nil)

// downloadsJobStore is the read seam the download handlers need from the
// repository; *repository.Store satisfies it as-is.
type downloadsJobStore interface {
	ListDownloadJobs(ctx context.Context, userID *string, all bool, limit int) ([]repository.DownloadJob, error)
	GetDownloadJob(ctx context.Context, id string) (repository.DownloadJob, error)
}

var _ downloadsJobStore = (*repository.Store)(nil)

// downloadEntitlement answers whether one account may start (and delete)
// torrent downloads. The store exposes can_download only through the admin
// listing today, so the production adapter pages it; a dedicated one-row read
// on the store can replace the adapter without the handlers noticing.
type downloadEntitlement interface {
	CanDownload(ctx context.Context, userID string) (bool, error)
}

// storeEntitlements reads the entitlement from the repository.
type storeEntitlements struct{ store *repository.Store }

// entitlementPage is the listing page size; instances larger than one page pay
// one extra query per page until the account is found.
const entitlementPage = 200

func (s storeEntitlements) CanDownload(ctx context.Context, userID string) (bool, error) {
	if s.store == nil {
		return false, nil
	}
	for offset := 0; ; offset += entitlementPage {
		users, err := s.store.ListUsers(ctx, entitlementPage, offset)
		if err != nil {
			return false, err
		}
		for _, u := range users {
			if u.ID == userID {
				return u.CanDownload, nil
			}
		}
		if len(users) < entitlementPage {
			return false, nil
		}
	}
}

// canDownload consults the entitlement seam, failing closed when it is absent.
func (s *Server) canDownload(ctx context.Context, userID string) (bool, error) {
	if s.entitlements == nil {
		return false, nil
	}
	return s.entitlements.CanDownload(ctx, userID)
}

// downloadsUnavailable short-circuits every download route in s3 mode, where
// there is no local disk for the engine to write to.
func (s *Server) downloadsUnavailable(w http.ResponseWriter) bool {
	if s.cfg.Downloads.Enabled {
		return false
	}
	WriteErrorCode(w, http.StatusNotImplemented, "downloads_unavailable",
		"Downloads are only available in local storage mode.")
	return true
}

// ── Create ───────────────────────────────────────────────────────────────────

type createDownloadRequest struct {
	ResultID string `json:"resultId"`
}

// handleDownloadCreate resolves the opaque result id back into the magnet and
// hands the request to the manager. The magnet exists only inside this call —
// the client sees the job, never the link.
func (s *Server) handleDownloadCreate(w http.ResponseWriter, r *http.Request) {
	if s.downloadsUnavailable(w) {
		return
	}
	principal, ok := s.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !principal.IsAdmin {
		allowed, err := s.canDownload(r.Context(), principal.UserID)
		if err != nil {
			s.serverError(w, "Could not verify download permission.", err)
			return
		}
		if !allowed {
			WriteErrorCode(w, http.StatusForbidden, "download_forbidden",
				"Your account does not have download access.")
			return
		}
	}

	var req createDownloadRequest
	if err := DecodeJSON(r, &req); err != nil || req.ResultID == "" {
		WriteError(w, http.StatusBadRequest, "resultId is required.")
		return
	}
	result, found := s.torrent.Result(req.ResultID)
	if !found {
		WriteErrorCode(w, http.StatusGone, "result_expired",
			"That search result has expired; search again.")
		return
	}
	userID, err := uuid.Parse(principal.UserID)
	if err != nil {
		// The principal's id comes from the account row, so this is unreachable
		// in practice; refusing beats writing a malformed user_id.
		WriteError(w, http.StatusBadRequest, "Invalid account.")
		return
	}

	job, err := s.downloads.Create(r.Context(), downloads.Request{
		UserID: userID, ResultID: req.ResultID, Magnet: result.Magnet,
		Title: result.Title, InfoHash: result.InfoHash, SizeBytes: result.SizeBytes,
	})
	if err != nil {
		s.writeCreateError(w, err)
		return
	}
	WriteJSON(w, http.StatusCreated, s.jobResponse(job))
}

// writeCreateError maps the manager's validation order onto status codes. The
// duplicate case carries the existing job's id so the client can open it
// instead of re-adding the same torrent.
func (s *Server) writeCreateError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, downloads.ErrNotEnabled):
		WriteErrorCode(w, http.StatusNotImplemented, "downloads_unavailable",
			"Downloads are only available in local storage mode.")
	case errors.Is(err, downloads.ErrDiskFull):
		WriteErrorCode(w, http.StatusInsufficientStorage, "insufficient_disk",
			"Not enough free space on the download disk.")
	case errors.Is(err, downloads.ErrTooLarge):
		WriteErrorCode(w, http.StatusBadRequest, "job_too_large",
			"That torrent is larger than the server's per-job limit.")
	case errors.Is(err, downloads.ErrTooManyActive):
		// The spec has no separate quota code; the 403 class is download_forbidden.
		WriteErrorCode(w, http.StatusForbidden, "download_forbidden",
			"The download quota is already in use. Cancel a job and try again.")
	case errors.Is(err, downloads.ErrDuplicate):
		WriteJSON(w, http.StatusConflict, map[string]any{
			"error":         "You already have this torrent queued or downloading.",
			"existingJobId": existingJobIDFromDuplicate(err),
		})
	default:
		s.serverError(w, "Could not start the download.", err)
	}
}

// existingJobIDFromDuplicate pulls the job id the manager embeds in
// ErrDuplicate's text ("downloads: duplicate: existing job <uuid>").
var duplicateJobIDPattern = regexp.MustCompile(`existing job ([0-9a-fA-F-]{36})`)

func existingJobIDFromDuplicate(err error) string {
	m := duplicateJobIDPattern.FindStringSubmatch(err.Error())
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// ── List / get ───────────────────────────────────────────────────────────────

// handleDownloadList returns the caller's jobs newest first. An admin may pass
// all=true to list every account's jobs.
func (s *Server) handleDownloadList(w http.ResponseWriter, r *http.Request) {
	if s.downloadsUnavailable(w) {
		return
	}
	principal, ok := s.requirePrincipal(w, r)
	if !ok {
		return
	}
	all := principal.IsAdmin && r.URL.Query().Get("all") == "true"
	limit := QueryInt(r, "limit", 100, 1, 200)
	var userID *string
	if !all {
		userID = &principal.UserID
	}
	jobs, err := s.jobStore.ListDownloadJobs(r.Context(), userID, all, limit)
	if err != nil {
		s.serverError(w, "Could not load downloads.", err)
		return
	}
	data := make([]jobResponse, 0, len(jobs))
	for _, job := range jobs {
		data = append(data, s.jobResponse(job))
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": data})
}

// handleDownloadGet returns one job with its per-file progress.
func (s *Server) handleDownloadGet(w http.ResponseWriter, r *http.Request) {
	if s.downloadsUnavailable(w) {
		return
	}
	job, _, ok := s.loadOwnedJob(w, r)
	if !ok {
		return
	}
	WriteJSON(w, http.StatusOK, s.jobResponse(job))
}

// loadOwnedJob fetches the row and enforces ownership: a foreign job answers
// 404 exactly like a missing one, so probing ids learns nothing.
func (s *Server) loadOwnedJob(w http.ResponseWriter, r *http.Request) (repository.DownloadJob, *auth.Principal, bool) {
	principal, ok := s.requirePrincipal(w, r)
	if !ok {
		return repository.DownloadJob{}, nil, false
	}
	job, err := s.jobStore.GetDownloadJob(r.Context(), Param(r, "id"))
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			WriteError(w, http.StatusNotFound, "Job not found.")
			return repository.DownloadJob{}, nil, false
		}
		s.serverError(w, "Could not load the download.", err)
		return repository.DownloadJob{}, nil, false
	}
	if job.UserID != principal.UserID && !principal.IsAdmin {
		WriteError(w, http.StatusNotFound, "Job not found.")
		return repository.DownloadJob{}, nil, false
	}
	return job, principal, true
}

// ── Patch ────────────────────────────────────────────────────────────────────

type patchDownloadRequest struct {
	Action      string `json:"action"`
	FileIndexes []int  `json:"fileIndexes"`
}

// handleDownloadPatch applies pause/resume, or records a file selection for a
// pack waiting in awaiting_selection. The manager re-checks ownership and the
// state machine behind this handler.
func (s *Server) handleDownloadPatch(w http.ResponseWriter, r *http.Request) {
	if s.downloadsUnavailable(w) {
		return
	}
	job, principal, ok := s.loadOwnedJob(w, r)
	if !ok {
		return
	}
	var req patchDownloadRequest
	if err := DecodeJSON(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, "Provide action \"pause\"/\"resume\" or fileIndexes.")
		return
	}

	isAdmin := principal.IsAdmin
	switch {
	case req.Action == "pause":
		updated, err := s.downloads.Pause(r.Context(), job.ID, principal.UserID, isAdmin)
		if err != nil {
			s.writeJobActionError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, s.jobResponse(updated))
	case req.Action == "resume":
		updated, err := s.downloads.Resume(r.Context(), job.ID, principal.UserID, isAdmin)
		if err != nil {
			s.writeJobActionError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, s.jobResponse(updated))
	case len(req.FileIndexes) > 0:
		if req.Action != "" {
			WriteError(w, http.StatusBadRequest, "Send either an action or fileIndexes, not both.")
			return
		}
		for _, index := range req.FileIndexes {
			if index < 0 {
				WriteErrorCode(w, http.StatusBadRequest, "invalid_file_selection",
					"fileIndexes must be non-negative torrent file positions.")
				return
			}
		}
		updated, err := s.downloads.SelectFiles(r.Context(), job.ID, principal.UserID, isAdmin, req.FileIndexes)
		if err != nil {
			// The only user-fixable failure here is a selection the torrent
			// cannot honour; everything else maps through the shared path.
			if !errors.Is(err, repository.ErrNotFound) && !errors.Is(err, repository.ErrConflict) &&
				!errors.Is(err, downloads.ErrForbidden) {
				WriteErrorCode(w, http.StatusBadRequest, "invalid_file_selection",
					"The server could not select those files for this torrent.")
				return
			}
			s.writeJobActionError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, s.jobResponse(updated))
	case req.FileIndexes != nil:
		// An explicit empty array names a selection the engine would reject.
		WriteErrorCode(w, http.StatusBadRequest, "invalid_file_selection",
			"fileIndexes cannot be empty.")
	default:
		WriteError(w, http.StatusBadRequest, "Provide action \"pause\"/\"resume\" or fileIndexes.")
	}
}

// writeJobActionError maps manager action failures onto responses. ErrForbidden
// keeps its 403 class even though ownership was already checked: the manager is
// the authority, and a 403 here can only mean its check saw something the row
// read did not.
func (s *Server) writeJobActionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, repository.ErrNotFound):
		WriteError(w, http.StatusNotFound, "Job not found.")
	case errors.Is(err, downloads.ErrForbidden):
		WriteErrorCode(w, http.StatusForbidden, "download_forbidden",
			"You do not have access to that download.")
	case errors.Is(err, repository.ErrConflict):
		WriteError(w, http.StatusConflict, "That action is not allowed in the job's current state.")
	default:
		s.serverError(w, "Could not update the download.", err)
	}
}

// ── Delete ───────────────────────────────────────────────────────────────────

// handleDownloadDelete cancels a live job or removes a finished one. With
// deleteFiles=true the engine's data and the job directory go with it, and a
// library scan runs afterwards so pruned files leave the catalog.
func (s *Server) handleDownloadDelete(w http.ResponseWriter, r *http.Request) {
	if s.downloadsUnavailable(w) {
		return
	}
	job, principal, ok := s.loadOwnedJob(w, r)
	if !ok {
		return
	}
	// Cancelling is a mutation of the download surface, so it honours the same
	// entitlement as creating: without it only an admin may remove jobs.
	if !principal.IsAdmin {
		allowed, err := s.canDownload(r.Context(), principal.UserID)
		if err != nil {
			s.serverError(w, "Could not verify download permission.", err)
			return
		}
		if !allowed {
			WriteErrorCode(w, http.StatusForbidden, "download_forbidden",
				"Your account does not have download access.")
			return
		}
	}

	deleteFiles := parseBoolParam(r, "deleteFiles")
	if err := s.downloads.Cancel(r.Context(), job.ID, principal.UserID, principal.IsAdmin, deleteFiles); err != nil {
		switch {
		case errors.Is(err, repository.ErrNotFound):
			WriteError(w, http.StatusNotFound, "Job not found.")
		case errors.Is(err, downloads.ErrForbidden):
			WriteErrorCode(w, http.StatusForbidden, "download_forbidden",
				"You do not have access to that download.")
		default:
			s.serverError(w, "Could not cancel the download.", err)
		}
		return
	}

	if deleteFiles {
		s.rescanAfterDelete()
	}
	WriteJSON(w, http.StatusOK, map[string]bool{"success": true})
}

// rescanAfterDelete prunes catalog rows for files the cancellation removed. It
// is best-effort and background: the delete itself already succeeded.
func (s *Server) rescanAfterDelete() {
	if s.scanner == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if _, err := s.scanner.Run(ctx); err != nil {
			s.logger.Warn("post-delete rescan failed", slog.String("error", err.Error()))
		}
	}()
}

func parseBoolParam(r *http.Request, key string) bool {
	switch r.URL.Query().Get(key) {
	case "true", "1", "yes":
		return true
	default:
		return false
	}
}

// ── SSE ──────────────────────────────────────────────────────────────────────

// handleDownloadEvents streams the user's job events: a snapshot of the last
// known state per job on connect, then live frames, with a comment heartbeat
// every 15 s so intermediaries do not close the stream. The subscription is
// removed on disconnect through cancel, which also closes the channel and ends
// the receive loop.
func (s *Server) handleDownloadEvents(w http.ResponseWriter, r *http.Request) {
	if s.downloadsUnavailable(w) {
		return
	}
	principal, ok := s.requirePrincipal(w, r)
	if !ok {
		return
	}
	if s.eventsHub == nil {
		WriteError(w, http.StatusServiceUnavailable, "Event streaming is not available.")
		return
	}
	events, snapshot, cancel, ok := s.eventsHub.Subscribe(principal.UserID)
	if !ok {
		WriteError(w, http.StatusServiceUnavailable, "Too many event streams; try again later.")
		return
	}
	defer cancel()

	flusher, canFlush := w.(http.Flusher)
	if !canFlush {
		WriteError(w, http.StatusInternalServerError, "Streaming is not supported on this connection.")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	for _, ev := range snapshot {
		writeSSEEvent(w, "snapshot", ev)
	}
	flusher.Flush()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, open := <-events:
			if !open {
				return // the hub closed the channel during cleanup
			}
			writeSSEEvent(w, "job", ev)
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// writeSSEEvent writes one frame. Events carry state, progress and rates —
// never magnets or filesystem paths.
func writeSSEEvent(w http.ResponseWriter, event string, ev downloads.JobEvent) {
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
}

// ── Wire shape ───────────────────────────────────────────────────────────────

// jobFile is one entry of the job's files array.
type jobFile struct {
	Index         int    `json:"index"`
	Path          string `json:"path"`
	SizeBytes     int64  `json:"sizeBytes"`
	Selected      bool   `json:"selected"`
	ProgressBytes int64  `json:"progressBytes"`
}

// jobResponse is the §7.7 wire shape of one download job: the stored row merged
// with the live engine view when the manager is driving the job in this
// session.
type jobResponse struct {
	ID            string     `json:"id"`
	ResultTitle   string     `json:"resultTitle"`
	InfoHash      string     `json:"infoHash"`
	State         string     `json:"state"`
	ProgressBytes int64      `json:"progressBytes"`
	SizeBytes     int64      `json:"sizeBytes"`
	ProgressPct   float64    `json:"progressPct"`
	DownKbps      int64      `json:"downKbps"`
	UpKbps        int64      `json:"upKbps"`
	Peers         int        `json:"peers"`
	EtaSec        int64      `json:"etaSec"`
	Error         *string    `json:"error"`
	ShowID        *string    `json:"showId"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
	CompletedAt   *time.Time `json:"completedAt"`
	Files         []jobFile  `json:"files"`
}

// jobResponse merges the row with the live engine snapshot: rates and per-file
// progress only exist in memory, while progress_bytes and size_bytes are the
// durable values the store holds.
func (s *Server) jobResponse(job repository.DownloadJob) jobResponse {
	resp := jobResponse{
		ID: job.ID, ResultTitle: job.ResultTitle, InfoHash: job.InfoHash, State: job.State,
		ProgressBytes: job.ProgressBytes, SizeBytes: job.SizeBytes,
		Error: job.Error, ShowID: job.ShowID,
		CreatedAt: job.CreatedAt, UpdatedAt: job.UpdatedAt, CompletedAt: job.CompletedAt,
		Files: []jobFile{},
	}
	var files []downloads.EngineFile
	if s.downloads != nil {
		stats, liveFiles, live := s.downloads.Snapshot(job.ID)
		files = liveFiles
		if live {
			resp.ProgressBytes = stats.BytesDone
			resp.DownKbps, resp.UpKbps, resp.Peers = stats.DownKbps, stats.UpKbps, stats.Peers
			resp.EtaSec = int64(stats.ETA / time.Second)
			if resp.SizeBytes == 0 {
				resp.SizeBytes = stats.BytesTotal
			}
		}
	}
	if resp.SizeBytes > 0 {
		pct := float64(resp.ProgressBytes) / float64(resp.SizeBytes) * 100
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
		resp.ProgressPct = pct
	}
	selected := make(map[int]bool, len(job.SelectedFiles))
	for _, index := range job.SelectedFiles {
		selected[index] = true
	}
	for _, f := range files {
		resp.Files = append(resp.Files, jobFile{
			Index: f.Index, Path: f.Path, SizeBytes: f.SizeBytes,
			Selected: selected[f.Index], ProgressBytes: f.Progress,
		})
	}
	return resp
}

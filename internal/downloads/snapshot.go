package downloads

// Snapshot is the read-only view the HTTP layer uses to render live transfer
// state and the per-file listing on GET /api/downloads/{id}. The manager owns
// the handles, so the accessor lives here rather than letting handlers reach
// into the engine.
//
// Reads are the same ones the drive loop makes on its own ticker, so running
// them concurrently with it is safe.

// Snapshot returns the live engine view of one job: transfer stats plus the
// per-file listing with progress. ok is false when the job is not being driven
// in this session (post-restart, before metadata, or terminal) or has no
// handle yet; callers then answer from the stored row alone. Files are nil
// when metadata is not available.
func (m *Manager) Snapshot(jobID string) (Stats, []EngineFile, bool) {
	j := m.lookup(jobID)
	if j == nil {
		return Stats{}, nil, false
	}
	h := j.currentHandle()
	if h == nil {
		return Stats{}, nil, false
	}
	st := h.Stats()
	files, err := h.Files()
	if err != nil {
		return st, nil, true
	}
	return st, files, true
}

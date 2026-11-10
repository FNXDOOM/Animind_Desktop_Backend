package downloads

// Snapshot is the HTTP layer's read-only view for GET /api/downloads/{id}.
// The manager owns the handles, so handlers never reach into the engine, and
// these are the drive loop's own reads, so running concurrently is safe.

// Snapshot returns the live engine view of one job: transfer stats plus the
// per-file listing. ok is false when the job is not being driven in this
// session (post-restart, before metadata, or terminal) or has no handle yet;
// callers then answer from the stored row alone. Files are nil without metadata.
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

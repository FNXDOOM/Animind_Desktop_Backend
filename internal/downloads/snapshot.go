package downloads

// Snapshot returns the live engine view of one job: stats plus files. ok is false when the job
// is not driven in this session or has no handle; Files are nil without metadata.
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

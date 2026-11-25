package downloads

// Download job state names, matching the download_jobs states exactly.
const (
	StateQueued            = "queued"
	StateFetchingMetadata  = "fetching_metadata"
	StateAwaitingSelection = "awaiting_selection"
	StateDownloading       = "downloading"
	StateVerifying         = "verifying"
	StateImporting         = "importing"
	StateReady             = "ready"
	StatePaused            = "paused"
	StateFailed            = "failed"
	StateCancelled         = "cancelled"
)

// transitions is the spec §7.5 state machine plus paused -> awaiting_selection, so a paused
// pack resumes into the wait, not a stranded row. The store's CAS is state-blind.
var transitions = map[string][]string{
	StateQueued:            {StateFetchingMetadata, StateFailed, StateCancelled, StatePaused},
	StateFetchingMetadata:  {StateAwaitingSelection, StateFailed, StateCancelled, StatePaused},
	StateAwaitingSelection: {StateDownloading, StateFailed, StateCancelled, StatePaused},
	StateDownloading:       {StateVerifying, StateFailed, StateCancelled, StatePaused},
	StateVerifying:         {StateImporting, StateFailed, StateCancelled, StatePaused},
	StateImporting:         {StateReady, StateFailed, StateCancelled},
	StatePaused:            {StateDownloading, StateAwaitingSelection, StateCancelled},
	StateReady:             {},
	StateFailed:            {},
	StateCancelled:         {},
}

func canTransition(from, to string) bool {
	for _, next := range transitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

func isTerminal(state string) bool {
	switch state {
	case StateReady, StateFailed, StateCancelled:
		return true
	}
	return false
}

// isActive is the set of uq_download_jobs_active_hash: every non-terminal state plus ready
// and paused. Ready rows still hold their torrent slot, so quotas count them.
func isActive(state string) bool {
	switch state {
	case StateQueued, StateFetchingMetadata, StateAwaitingSelection, StateDownloading,
		StateVerifying, StateImporting, StateReady, StatePaused:
		return true
	}
	return false
}

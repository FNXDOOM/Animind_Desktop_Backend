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

// transitions is spec §7.5's state machine plus one controller-ruled addition:
// paused -> awaiting_selection, so a pack paused before selecting resumes into
// the selection wait instead of a downloading row whose Done() never fires.
// The store's CAS is state-blind, so this table alone guards illegal hops.
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

// isActive is the partial unique index uq_download_jobs_active_hash's set from
// migration 0003: every non-terminal state plus ready and paused. Ready rows
// still hold their torrent slot, which is why the quota counts include them.
func isActive(state string) bool {
	switch state {
	case StateQueued, StateFetchingMetadata, StateAwaitingSelection, StateDownloading,
		StateVerifying, StateImporting, StateReady, StatePaused:
		return true
	}
	return false
}

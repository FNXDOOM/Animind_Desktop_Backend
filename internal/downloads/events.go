package downloads

// JobEvent is one SSE payload about a download job (spec §7.7). The struct is
// the event surface's entirety: it never carries magnets or filesystem paths.
// The concrete hub lands in Task 9; it lives here because the manager already
// publishes it at every state change and progress tick.
type JobEvent struct {
	ID        string  `json:"id"`
	State     string  `json:"state"`
	Progress  float64 `json:"progress"` // percent 0..100
	BytesDone int64   `json:"bytesDone"`
	SizeBytes int64   `json:"sizeBytes"`
	DownKbps  int64   `json:"downKbps"`
	UpKbps    int64   `json:"upKbps"`
	Peers     int     `json:"peers"`
	ETASec    int64   `json:"etaSec"`
	ShowID    *string `json:"showId"`
	Error     *string `json:"error,omitempty"`
}

// eventPublisher is the hub seam; Task 9's *Hub satisfies it. Nil means no
// streaming is wired and publishes are skipped.
type eventPublisher interface {
	Publish(userID string, ev JobEvent)
}

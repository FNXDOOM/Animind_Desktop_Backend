package downloads

import "sync"
import "time"

// JobEvent is one SSE payload about a download job (spec §7.7). The struct is
// the event surface's entirety: it never carries magnets or filesystem paths.
// The manager publishes it at every state change and progress tick.
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

// eventPublisher is the hub seam; *Hub satisfies it. Nil means no streaming
// is wired and publishes are skipped.
type eventPublisher interface {
	Publish(userID string, ev JobEvent)
}

const (
	streamBuffer   = 64
	maxPerUser     = 2
	maxTotal       = 100
	coalesceWindow = time.Second
)

// Hub fans events out to a user's SSE streams: buffered per-stream channels, a
// reconnect snapshot, and per-job coalescing so a chatty manager cannot flood
// a browser. All methods are safe for concurrent use.
type Hub struct {
	mu    sync.Mutex
	subs  map[string]map[*subscriber]struct{}
	nsubs int
	// closed marks shutdown: Publish becomes a no-op and Subscribe refuses, so
	// nothing can re-arm a stream the process is trying to drain.
	closed bool
	// last survives unsubscribes: it is the reconnect snapshot.
	last map[string]map[string]JobEvent
	// sent and pend only matter while a user has live streams.
	sent map[string]map[string]time.Time
	pend map[string]map[string]*pendingFlush
}

type subscriber struct {
	ch chan JobEvent
}

type pendingFlush struct {
	ev    JobEvent
	timer *time.Timer
}

var _ eventPublisher = (*Hub)(nil)

func NewEventHub() *Hub {
	return &Hub{
		subs: map[string]map[*subscriber]struct{}{},
		last: map[string]map[string]JobEvent{},
		sent: map[string]map[string]time.Time{},
		pend: map[string]map[string]*pendingFlush{},
	}
}

// Len reports live streams across all users.
func (h *Hub) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.nsubs
}

func (h *Hub) Publish(userID string, ev JobEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return // shutdown: nothing streams and no snapshot will be read again
	}

	lastByJob := h.last[userID]
	if lastByJob == nil {
		lastByJob = map[string]JobEvent{}
		h.last[userID] = lastByJob
	}
	lastByJob[ev.ID] = ev

	if len(h.subs[userID]) == 0 {
		return
	}
	if p, ok := h.pend[userID][ev.ID]; ok {
		p.ev = ev // the pending flush must carry the newest state
		return
	}
	now := time.Now()
	if lt, ok := h.sent[userID][ev.ID]; ok && now.Sub(lt) < coalesceWindow {
		h.pendAt(userID, ev.ID, ev, coalesceWindow-now.Sub(lt))
		return
	}
	h.deliver(userID, ev, now)
}

func (h *Hub) pendAt(userID, jobID string, ev JobEvent, delay time.Duration) {
	byJob := h.pend[userID]
	if byJob == nil {
		byJob = map[string]*pendingFlush{}
		h.pend[userID] = byJob
	}
	p := &pendingFlush{ev: ev}
	p.timer = time.AfterFunc(delay, func() { h.flush(userID, jobID) })
	byJob[jobID] = p
}

func (h *Hub) flush(userID, jobID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	byJob := h.pend[userID]
	p, ok := byJob[jobID]
	if !ok {
		return // canceled while the fired timer waited on the lock
	}
	delete(byJob, jobID)
	if len(byJob) == 0 {
		delete(h.pend, userID)
	}
	h.deliver(userID, p.ev, time.Now())
}

// deliver drops the oldest buffered event for slow streams so the manager
// never blocks on a publish.
func (h *Hub) deliver(userID string, ev JobEvent, now time.Time) {
	if len(h.subs[userID]) == 0 {
		return
	}
	byJob := h.sent[userID]
	if byJob == nil {
		byJob = map[string]time.Time{}
		h.sent[userID] = byJob
	}
	byJob[ev.ID] = now
	for s := range h.subs[userID] {
		select {
		case s.ch <- ev:
		default:
			select {
			case <-s.ch:
			default:
			}
			select {
			case s.ch <- ev:
			default:
			}
		}
	}
}

// Subscribe registers a stream for the user: ch carries live events, snapshot
// holds the last known event per job, cancel removes the stream. ok is false
// when the hub is closed (shutdown) or the per-user (2) or total (100) cap is
// hit.
func (h *Hub) Subscribe(userID string) (ch <-chan JobEvent, snapshot []JobEvent, cancel func(), ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.nsubs >= maxTotal || len(h.subs[userID]) >= maxPerUser {
		return nil, nil, nil, false
	}
	s := &subscriber{ch: make(chan JobEvent, streamBuffer)}
	if h.subs[userID] == nil {
		h.subs[userID] = map[*subscriber]struct{}{}
	}
	h.subs[userID][s] = struct{}{}
	h.nsubs++

	if lastByJob := h.last[userID]; len(lastByJob) > 0 {
		snapshot = make([]JobEvent, 0, len(lastByJob))
		for _, ev := range lastByJob {
			snapshot = append(snapshot, ev)
		}
	}

	cancel = func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, live := h.subs[userID][s]; !live {
			return
		}
		delete(h.subs[userID], s)
		if len(h.subs[userID]) == 0 {
			delete(h.subs, userID)
			h.dropCoalesceState(userID)
		}
		h.nsubs--
		close(s.ch) // lets the SSE handler's receive loop terminate
	}
	return s.ch, snapshot, cancel, true
}

// Close ends every subscriber stream at once: each channel is closed so its
// SSE handler's receive loop returns and the HTTP connections drain instead of
// holding server.Shutdown open for its whole timeout window. It is idempotent;
// afterwards Publish is a no-op and Subscribe returns ok=false. Pre-existing
// cancel funcs stay no-ops, so no channel can be closed twice.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for _, bySub := range h.subs {
		for s := range bySub {
			close(s.ch)
		}
	}
	// The caps are free again — moot, since Subscribe now refuses — and the
	// per-user coalescing state dies with its streams.
	h.subs = map[string]map[*subscriber]struct{}{}
	h.nsubs = 0
	for _, byJob := range h.pend {
		for _, p := range byJob {
			p.timer.Stop()
		}
	}
	h.pend = map[string]map[string]*pendingFlush{}
	h.sent = map[string]map[string]time.Time{}
}

func (h *Hub) dropCoalesceState(userID string) {
	for _, p := range h.pend[userID] {
		p.timer.Stop()
	}
	delete(h.pend, userID)
	delete(h.sent, userID)
}

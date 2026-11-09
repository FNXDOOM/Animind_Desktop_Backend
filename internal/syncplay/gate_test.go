package syncplay

import (
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"animind-backend-go/internal/config"
)

// Unit tests for the scheduling layer, driving the real handlers over in-memory channels. The
// gate never touches the store, so these run without the database the functional suite needs.

func defaultSyncPlay() config.SyncPlayConfig {
	return config.SyncPlayConfig{
		Enabled:                true,
		ReadyTimeout:           40 * time.Millisecond,
		BufferGoal:             120 * time.Second,
		SoftSeekThreshold:      200 * time.Millisecond,
		SpeedSyncMaxDrift:      10 * time.Second,
		EscapeBuffer:           30 * time.Second,
		EscapeWait:             10 * time.Second,
		StallDetectWindow:      2 * time.Second,
		StallGrace:             5 * time.Second,
		WaitDeadline:           30 * time.Second,
		StartLeadFloor:         500 * time.Millisecond,
		StartLeadCeiling:       4 * time.Second,
		MaxReportedPing:        10 * time.Second,
		MaxParticipants:        12,
		SlowdownRate:           0.80,
		SpeedupRate:            1.50,
		CorrectionEngageDrift:  1500 * time.Millisecond,
		CorrectionReleaseDrift: 100 * time.Millisecond,
		FastForwardDrift:       5 * time.Second,
		MaxPlaybackOffset:      500 * time.Millisecond,
		MaxCatchUpOffset:       20 * time.Second,
		BufferGoalFraction:     0.10,
		TimeSyncWindow:         8,
		TimeSyncDeadband:       120 * time.Millisecond,
		TimeSyncMaxAge:         150 * time.Second,
		TimeSyncGreedyInterval: time.Second,
		TimeSyncIdleInterval:   15 * time.Second,
		TimeSyncPausedInterval: 60 * time.Second,
		ProtocolTimeout:        12500 * time.Millisecond,
	}
}

func newMemHub(t *testing.T, tune func(*config.SyncPlayConfig)) *Hub {
	t.Helper()
	s := defaultSyncPlay()
	if tune != nil {
		tune(&s)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &Hub{
		cfg:          &config.Config{SyncPlay: s},
		logger:       logger,
		peers:        map[string]*peer{},
		pendingPeers: map[string]*peer{},
		rooms:        map[string]*Room{},
	}
}

func (h *Hub) withPeer(sid, userID, name string) *peer {
	p := newPeer(sid, "websocket", h, h.logger)
	p.userID, p.name = userID, name
	h.mu.Lock()
	h.peers[sid] = p
	h.mu.Unlock()
	return p
}

func (h *Hub) room(code string) *Room {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.rooms[code]
}

// roomWith returns a room whose first named member is the host, plus each member's peer.
func roomWith(h *Hub, code string, names ...string) (*Room, []*peer) {
	peers := make([]*peer, 0, len(names))
	for _, name := range names {
		peers = append(peers, h.withPeer("sock-"+name, "user-"+name, name))
	}
	room := newRoom(code, "ep-1", "show-1", peers[0].sid, peers[0].userID, peers[0].name, 1400)
	h.mu.Lock()
	h.rooms[code] = room
	h.mu.Unlock()
	peers[0].roomCode = code
	for _, p := range peers[1:] {
		room.mu.Lock()
		room.participants[p.sid] = &Participant{
			SocketID: p.sid, UserID: p.userID, DisplayName: p.name,
			ReadyState: readyStateBuffering, PlaybackRate: 1,
			LastHeartbeatAt: time.Now(), joinedAt: time.Now(),
		}
		room.mu.Unlock()
		p.roomCode = code
	}
	return room, peers
}

// ── Outbound frames ──────────────────────────────────────────────────────────

type received struct {
	event string
	arg   json.RawMessage
}

// recorder buffers a peer's outbound frames so an assertion reads a queue, not a live socket.
// waitFor advances a cursor, keeping history so ordering can be asserted.
type recorder struct {
	peer   *peer
	frames []received
	cursor int
}

func record(t *testing.T, p *peer) *recorder {
	t.Helper()
	return &recorder{peer: p}
}

func (r *recorder) drain() {
	for {
		select {
		case frame := <-r.peer.out:
			if name, arg, ok := decodeFrame(frame); ok {
				r.frames = append(r.frames, received{event: name, arg: arg})
			}
		default:
			return
		}
	}
}

// waitFor collects until one named event appears and returns its payload.
func (r *recorder) waitFor(event string, within time.Duration) (json.RawMessage, bool) {
	deadline := time.Now().Add(within)
	for {
		r.drain()
		for i := r.cursor; i < len(r.frames); i++ {
			if r.frames[i].event == event {
				arg := r.frames[i].arg
				r.cursor = i + 1
				return arg, true
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, false
		}
		select {
		case frame := <-r.peer.out:
			if name, arg, ok := decodeFrame(frame); ok {
				r.frames = append(r.frames, received{event: name, arg: arg})
			}
		case <-time.After(remaining):
		}
	}
}

// seen counts how often an event was delivered over the whole recorded history.
func (r *recorder) seen(event string) int {
	r.drain()
	count := 0
	for _, f := range r.frames {
		if f.event == event {
			count++
		}
	}
	return count
}

// notSeen waits, then asserts that an event never arrived. A negative that had nothing to wait
// for is not evidence, so the window is explicit.
func (r *recorder) notSeen(event string, forHowLong time.Duration) bool {
	time.Sleep(forHowLong)
	r.drain()
	return r.seen(event) == 0
}

func (r *recorder) names() []string {
	r.drain()
	out := make([]string, 0, len(r.frames))
	for _, f := range r.frames {
		out = append(out, f.event)
	}
	return out
}

// decodeFrame turns an Engine.IO packet into an event name and its argument.
func decodeFrame(frame string) (string, json.RawMessage, bool) {
	body, ok := strings.CutPrefix(frame, "42")
	if !ok {
		return "", nil, false
	}
	var args []json.RawMessage
	if err := json.Unmarshal([]byte(body), &args); err != nil || len(args) < 2 {
		return "", nil, false
	}
	var name string
	if err := json.Unmarshal(args[0], &name); err != nil {
		return "", nil, false
	}
	return name, args[1], true
}

func decode[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", string(raw), err)
	}
	return out
}

func emit(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal event body: %v", err)
	}
	return raw
}

// readyAll moves every member through the gate by reporting a full buffer, as a client would.
func readyAll(t *testing.T, h *Hub, code string, peers []*peer) {
	t.Helper()
	room := h.room(code)
	for _, p := range peers {
		h.handleBufferingProgress(p, inbound{Args: emit(t,
			bufferingRequest{BufferedSeconds: 240, Percent: 100})}, room)
	}
}

// ── Pure policies ────────────────────────────────────────────────────────────

func TestLeadMillis(t *testing.T) {
	cases := []struct {
		name                       string
		ping, floor, ceiling, want int64
	}{
		{"a local room still schedules", 1, 500, 4000, 500},
		{"twice the worst ping", 300, 500, 4000, 600},
		{"the floor wins below it", 120, 500, 4000, 500},
		{"the ceiling caps a satellite link", 9000, 500, 4000, 4000},
		{"zero ceiling means no cap", 9000, 500, 0, 18000},
	}
	for _, tc := range cases {
		if got := leadMillis(tc.ping, tc.floor, tc.ceiling); got != tc.want {
			t.Errorf("%s: leadMillis(%d,%d,%d) = %d, want %d",
				tc.name, tc.ping, tc.floor, tc.ceiling, got, tc.want)
		}
	}
}

func TestClampPing(t *testing.T) {
	if got := clampPing(-5, 10000); got != 0 {
		t.Errorf("a negative report must read as no latency, got %d", got)
	}
	if got := clampPing(50000, 10000); got != 10000 {
		t.Errorf("clampPing(50000) = %d, want 10000", got)
	}
	if got := clampPing(120, 10000); got != 120 {
		t.Errorf("an honest report must survive the clamp, got %d", got)
	}
}

func TestScaledGoalSeconds(t *testing.T) {
	cfg := &config.Config{SyncPlay: defaultSyncPlay()}
	if got := scaledGoalSeconds(cfg, 0); got != 120 {
		t.Errorf("unknown remaining should keep the configured goal, got %v", got)
	}
	if got := scaledGoalSeconds(cfg, 1400); got != 120 {
		t.Errorf("a full episode should keep the configured goal, got %v", got)
	}
	if got := scaledGoalSeconds(cfg, 200); got != 20 {
		t.Errorf("three minutes left should ask for a tenth of it, got %v", got)
	}
	if got := scaledGoalSeconds(cfg, 60); got != minGoalSeconds {
		t.Errorf("a minute left is below the floor, want %v, got %v", minGoalSeconds, got)
	}
	if got := scaledGoalSeconds(cfg, -5); got != 120 {
		t.Errorf("a position past the end is no evidence about the goal, got %v", got)
	}
}

func TestGroupStateOf(t *testing.T) {
	if got := groupStateOf(true, true, true); got != groupStateWaiting {
		t.Errorf("the gate outranks playback, got %q", got)
	}
	if got := groupStateOf(true, false, true); got != groupStatePlaying {
		t.Errorf("got %q", got)
	}
	if got := groupStateOf(false, false, true); got != groupStatePaused {
		t.Errorf("got %q", got)
	}
	if got := groupStateOf(false, false, false); got != groupStateIdle {
		t.Errorf("a room nobody has started must not read as paused, got %q", got)
	}
}

func TestPercentOf(t *testing.T) {
	if got := percentOf(30, 120); got != 25 {
		t.Errorf("got %v", got)
	}
	if got := percentOf(200, 120); got != 100 {
		t.Errorf("a full buffer should read full, not %v", got)
	}
	if got := percentOf(10, 0); got != 0 {
		t.Errorf("no goal must not divide, got %v", got)
	}
}

func TestSyncConfigReachesAClient(t *testing.T) {
	h := newMemHub(t, nil)
	cfg := h.syncConfig()
	if cfg.Version != protocolVersion {
		t.Errorf("version %d, want %d", cfg.Version, protocolVersion)
	}
	if cfg.Gate.WaitDeadlineMs != 30000 {
		t.Errorf("the wait deadline did not reach the client: %d", cfg.Gate.WaitDeadlineMs)
	}
	if cfg.Correction.SlowdownRate != 0.80 || cfg.Start.LeadMinMs != 500 {
		t.Errorf("thresholds are missing from the tuning block: %+v %+v", cfg.Correction, cfg.Start)
	}
	found := false
	for _, feature := range cfg.Features {
		if feature == "scheduledStart" {
			found = true
		}
	}
	if !found {
		t.Error("a client cannot negotiate a capability the server does not name")
	}
}

// ── Sequence numbers ─────────────────────────────────────────────────────────

func TestBroadcastStampsMonotonicSeq(t *testing.T) {
	h := newMemHub(t, nil)
	room, peers := roomWith(h, "SEQ01", "host", "member")
	member := record(t, peers[1])

	room.broadcast(h, "peerStalling", &peerNamePayload{UserID: "u", DisplayName: "x", SentAt: nowMillis()}, "")
	room.broadcast(h, "peerStallRecovered", &peerNamePayload{UserID: "u", DisplayName: "x", SentAt: nowMillis()}, "")

	first, ok := member.waitFor("peerStalling", time.Second)
	if !ok {
		t.Fatal("no peerStalling delivered")
	}
	second, ok := member.waitFor("peerStallRecovered", time.Second)
	if !ok {
		t.Fatal("no peerStallRecovered delivered")
	}
	a := decode[map[string]any](t, first)
	b := decode[map[string]any](t, second)
	seqA, seqB := int64(a["seq"].(float64)), int64(b["seq"].(float64))
	if seqA == 0 || seqB <= seqA {
		t.Fatalf("seq must increase per broadcast, got %d then %d", seqA, seqB)
	}
	if int64(b["serverTime"].(float64)) == 0 {
		t.Fatal("broadcasts must carry the hub's clock for ordering")
	}
}

func TestPrivateCorrectionAndDenialCarryRoomStamps(t *testing.T) {
	h := newMemHub(t, nil)
	room, peers := roomWith(h, "PRIV1", "host", "member")
	member := record(t, peers[1])
	room.mu.Lock()
	room.isPlaying = true
	room.currentTime = 100
	room.mu.Unlock()

	h.checkDrift(peers[1], room, 100.6)
	correctionRaw, ok := member.waitFor("speedSeek", time.Second)
	if !ok {
		t.Fatal("no private speedSeek delivered")
	}
	correction := decode[map[string]any](t, correctionRaw)
	if correction["seq"].(float64) <= 0 {
		t.Fatal("private speedSeek must carry a room sequence")
	}
	if correction["serverTime"].(float64) != correction["sentAt"].(float64) {
		t.Fatalf("private correction clocks differ: serverTime=%v sentAt=%v", correction["serverTime"], correction["sentAt"])
	}

	h.handleTransferHost(peers[1], inbound{Args: emit(t, transferHostRequest{TargetUserID: peers[0].userID})}, room)
	deniedRaw, ok := member.waitFor("syncDenied", time.Second)
	if !ok {
		t.Fatal("no private syncDenied delivered")
	}
	denied := decode[map[string]any](t, deniedRaw)
	if denied["seq"].(float64) <= correction["seq"].(float64) {
		t.Fatalf("private denial seq %v must follow correction seq %v", denied["seq"], correction["seq"])
	}
	if denied["serverTime"].(float64) != denied["sentAt"].(float64) {
		t.Fatalf("private denial clocks differ: serverTime=%v sentAt=%v", denied["serverTime"], denied["sentAt"])
	}
}

// ── The gate and its scheduled release ───────────────────────────────────────

func TestGateReleasesOntoAScheduledStart(t *testing.T) {
	h := newMemHub(t, nil)
	room, peers := roomWith(h, "GATE1", "host", "member")
	memberRec := record(t, peers[1])

	h.handlePlay(peers[0], inbound{Args: emit(t, timeRequest{CurrentTime: 42})}, room)

	gateRaw, ok := memberRec.waitFor("waitForBufferGoal", time.Second)
	if !ok {
		t.Fatal("a group play must open the buffer gate")
	}
	gate := decode[map[string]any](t, gateRaw)
	if gate["waitDeadlineMs"] == nil {
		t.Error("the gate must say how long it can take, or a client can only show a spinner")
	}
	if goal, _ := gate["bufferGoalSeconds"].(float64); goal != 120 {
		t.Errorf("the gate should publish its goal, got %v", goal)
	}
	if state, _ := gate["groupState"].(string); state != string(groupStateWaiting) {
		t.Errorf("the gate should declare waiting, got %q", state)
	}

	readyAll(t, h, "GATE1", peers)

	allReadyRaw, ok := memberRec.waitFor("allReady", 2*time.Second)
	if !ok {
		t.Fatalf("the gate never announced allReady; member saw %v", memberRec.names())
	}
	allReady := decode[map[string]any](t, allReadyRaw)
	scheduled := int64(allReady["scheduledPlayAt"].(float64))
	floor := h.cfg.SyncPlay.StartLeadFloor.Milliseconds()
	if lead := scheduled - nowMillis(); lead < floor-50 {
		t.Errorf("allReady scheduled %d ms out, want at least the %d ms floor", lead, floor)
	}
	if intent, _ := allReady["playIntent"].(bool); !intent {
		t.Error("a gate opened by play must close with the intent to play")
	}
	if state, _ := allReady["groupState"].(string); state != string(groupStatePlaying) {
		t.Errorf("the room should declare playing, got %q", state)
	}

	syncRaw, ok := memberRec.waitFor("syncPlay", 2*time.Second)
	if !ok {
		t.Fatal("allReady was not followed by syncPlay")
	}
	sync := decode[map[string]any](t, syncRaw)
	if got := int64(sync["scheduledPlayAt"].(float64)); got != scheduled {
		t.Errorf("syncPlay scheduled %d but allReady scheduled %d: two instants for one start",
			got, scheduled)
	}
	// Ordering is what a client's stale-packet guard compares, so it has to be on the wire.
	if int64(sync["seq"].(float64)) <= int64(allReady["seq"].(float64)) {
		t.Error("syncPlay must carry a later seq than the allReady that precedes it")
	}
}

func TestScheduledStartUsesTheSlowestMember(t *testing.T) {
	h := newMemHub(t, nil)
	room, peers := roomWith(h, "PING1", "host", "member")
	ping := 900.0
	h.handleHeartbeat(peers[1], inbound{Args: emit(t, heartbeatRequest{CurrentTime: 10, PingMS: &ping})}, room)

	memberRec := record(t, peers[1])
	h.handlePlay(peers[0], inbound{Args: emit(t, timeRequest{CurrentTime: 10})}, room)
	readyAll(t, h, "PING1", peers)

	raw, ok := memberRec.waitFor("allReady", 2*time.Second)
	if !ok {
		t.Fatal("gate never closed")
	}
	scheduled := decode[map[string]any](t, raw)["scheduledPlayAt"].(float64)
	if lead := int64(scheduled) - nowMillis(); lead < 1700 || lead > 1800 {
		t.Errorf("a 900 ms member should push the start ~1800 ms out, got %d", lead)
	}
}

func TestPingReportIsClampedBeforeItMovesTheStart(t *testing.T) {
	h := newMemHub(t, nil)
	room, peers := roomWith(h, "CLMP1", "host", "member")
	ping := 500000.0
	h.handleHeartbeat(peers[1], inbound{Args: emit(t, heartbeatRequest{CurrentTime: 3, PingMS: &ping})}, room)

	room.mu.Lock()
	stored := room.participants[peers[1].sid].PingMs
	room.mu.Unlock()

	want := h.cfg.SyncPlay.MaxReportedPing.Milliseconds()
	if stored != want {
		t.Errorf("a client must not schedule the room further than the cap: stored %d, want %d", stored, want)
	}
	memberRec := record(t, peers[1])
	h.handlePlay(peers[0], inbound{Args: emit(t, timeRequest{CurrentTime: 3})}, room)
	readyAll(t, h, "CLMP1", peers)
	raw, ok := memberRec.waitFor("allReady", 2*time.Second)
	if !ok {
		t.Fatal("gate never closed")
	}
	scheduled := decode[map[string]any](t, raw)["scheduledPlayAt"].(float64)
	if lead := int64(scheduled) - nowMillis(); lead > want*2+50 {
		t.Errorf("the scheduled lead escaped the clamp: %d ms", lead)
	}
}

func TestIgnoreWaitReleasesOnlyItsOwnMember(t *testing.T) {
	// All other gate exits are disabled: with the 40 ms timeout left running, a forceReady would
	// close the gate and the test would pass without the opt-out doing anything.
	h := newMemHub(t, func(s *config.SyncPlayConfig) {
		s.ReadyTimeout = 10 * time.Second
		s.EscapeWait = 10 * time.Second
		s.WaitDeadline = 10 * time.Second
	})
	room, peers := roomWith(h, "IGN01", "host", "member")
	memberRec, hostRec := record(t, peers[1]), record(t, peers[0])

	h.handlePlay(peers[0], inbound{Args: emit(t, timeRequest{CurrentTime: 77})}, room)
	if _, ok := memberRec.waitFor("waitForBufferGoal", time.Second); !ok {
		t.Fatal("expected the gate to open")
	}
	// Drain the host's first gate now, else the later read answers with the wrong gate.
	if _, ok := hostRec.waitFor("waitForBufferGoal", time.Second); !ok {
		t.Fatal("the host never saw the gate open")
	}

	ignore := true
	h.handleIgnoreWait(peers[1], inbound{Args: emit(t, ignoreWaitRequest{Ignore: &ignore})}, room)

	released, ok := memberRec.waitFor("peerReady", time.Second)
	if !ok {
		t.Fatalf("the opt-out was never announced; saw %v", memberRec.names())
	}
	body := decode[map[string]any](t, released)
	if ignoring, _ := body["ignoring"].(bool); !ignoring {
		t.Error("peerReady should say the member left the wait rather than arrived ready")
	}
	if count, _ := body["readyCount"].(float64); count != 1 {
		t.Errorf("only the opting-out member should be released, readyCount=%v", count)
	}
	// Releasing one member must not start the room; the host has not buffered yet.
	if !memberRec.notSeen("allReady", 200*time.Millisecond) {
		t.Error("the gate closed while the host was still being waited for")
	}
	if memberRec.seen("peerIgnoreWait") == 0 {
		t.Error("the rest of the room was never told who stopped being waited for")
	}

	h.handleReady(peers[0], room)
	if _, ok := memberRec.waitFor("allReady", time.Second); !ok {
		t.Fatalf("the gate stayed open after every remaining member was ready; saw %v", memberRec.names())
	}

	room.mu.Lock()
	participant := room.participants[peers[1].sid]
	stored := participant.IgnoreWait
	room.mu.Unlock()
	if !stored {
		t.Fatal("IgnoreWait should persist past the gate it released")
	}

	// A standing preference: the next gate counts this member ready from the start.
	h.handlePlay(peers[0], inbound{Args: emit(t, timeRequest{CurrentTime: 80})}, room)
	gateRaw, ok := hostRec.waitFor("waitForBufferGoal", 2*time.Second)
	if !ok {
		t.Fatalf("the second play did not open a gate; saw %v", hostRec.names())
	}
	gate := decode[map[string]any](t, gateRaw)
	if count, _ := gate["readyCount"].(float64); count != 1 {
		t.Errorf("the gate should open with the opted-out member already counted, readyCount=%v", count)
	}
	h.handleReady(peers[0], room)
	if _, ok := hostRec.waitFor("allReady", time.Second); !ok {
		t.Fatalf("the second gate waited on a member that had opted out; saw %v", hostRec.names())
	}
}

func TestIgnoreWaitDoesNotCorrectTheMember(t *testing.T) {
	h := newMemHub(t, nil)
	room, peers := roomWith(h, "IGN02", "host", "member")
	ignore := true
	h.handleIgnoreWait(peers[1], inbound{Args: emit(t, ignoreWaitRequest{Ignore: &ignore})}, room)

	room.mu.Lock()
	room.isPlaying = true
	room.currentTime = 100
	room.mu.Unlock()

	memberRec := record(t, peers[1])
	// 96 is four seconds behind, past the re-gate threshold, the strongest correction available.
	h.handleHeartbeat(peers[1], inbound{Args: emit(t, heartbeatRequest{CurrentTime: 96})}, room)
	if !memberRec.notSeen("softCorrect", 150*time.Millisecond) {
		t.Error("a member that opted out of the wait must not be steered back into it")
	}
}

func TestWaitDeadlineClosesAGateNobodyAnswered(t *testing.T) {
	h := newMemHub(t, func(s *config.SyncPlayConfig) {
		s.WaitDeadline = 60 * time.Millisecond
		// timeout + escape pushed past the window, so only the deadline can close this gate.
		s.ReadyTimeout = 10 * time.Second
		s.EscapeWait = 10 * time.Second
	})
	room, peers := roomWith(h, "DDL01", "host", "member")
	memberRec := record(t, peers[1])

	h.handlePlay(peers[0], inbound{Args: emit(t, timeRequest{CurrentTime: 5})}, room)
	if _, ok := memberRec.waitFor("waitForBufferGoal", time.Second); !ok {
		t.Fatal("expected the gate to open")
	}
	if _, ok := memberRec.waitFor("allReady", 2*time.Second); !ok {
		t.Fatalf("the deadline never closed the gate; saw %v", memberRec.names())
	}
	room.mu.Lock()
	waiting := room.waitingForReady
	room.mu.Unlock()
	if waiting {
		t.Error("the deadline must close the gate, not merely start playback")
	}
}

func TestStaleForceReadyCannotCrossIntoANewGate(t *testing.T) {
	h := newMemHub(t, nil)
	room, peers := roomWith(h, "EPOCH", "host", "member")
	h.handlePlay(peers[0], inbound{Args: emit(t, timeRequest{CurrentTime: 5})}, room)

	room.mu.Lock()
	epoch := room.gateEpoch
	room.mu.Unlock()

	// A stale epoch must be a no-op, not a free pass into the new gate.
	h.forceReady(peers[1].sid, room.code, epoch-1)
	room.mu.Lock()
	ready := len(room.readyPeers)
	room.mu.Unlock()
	if ready != 0 {
		t.Fatalf("a stale timer granted readiness to %d members", ready)
	}

	h.forceReady(peers[1].sid, room.code, epoch)
	room.mu.Lock()
	ready = len(room.readyPeers)
	room.mu.Unlock()
	if ready != 1 {
		t.Fatal("the live timer must still work")
	}
}

func TestPauseCancelsAnOpenGate(t *testing.T) {
	h := newMemHub(t, nil)
	room, peers := roomWith(h, "PAUSE", "host", "member")
	memberRec := record(t, peers[1])

	h.handlePlay(peers[0], inbound{Args: emit(t, timeRequest{CurrentTime: 5})}, room)
	if _, ok := memberRec.waitFor("waitForBufferGoal", time.Second); !ok {
		t.Fatal("expected the gate to open")
	}
	h.handlePause(peers[0], inbound{Args: emit(t, timeRequest{CurrentTime: 5})}, room)

	raw, ok := memberRec.waitFor("pause", time.Second)
	if !ok {
		t.Fatal("no pause delivered")
	}
	if state, _ := decode[map[string]any](t, raw)["groupState"].(string); state != string(groupStatePaused) {
		t.Errorf("pause should report paused, got %q", state)
	}
	room.mu.Lock()
	waiting, playIntent := room.waitingForReady, room.playIntentAfterReady
	room.mu.Unlock()
	if waiting || playIntent {
		t.Error("a pause must cancel both the gate and the play intent it carried")
	}

	// A cancelled gate must not start the room when the stragglers finally answer.
	readyAll(t, h, "PAUSE", peers)
	if !memberRec.notSeen("syncPlay", 150*time.Millisecond) {
		t.Error("the room started playing after an explicit pause")
	}
}

// ── Buffer visibility ────────────────────────────────────────────────────────

func TestPeerBufferingIsAnnouncedOncePerFall(t *testing.T) {
	h := newMemHub(t, nil)
	room, peers := roomWith(h, "BUF01", "host", "member")
	room.mu.Lock()
	room.isPlaying = true
	room.currentTime = 50
	room.participants[peers[1].sid].CurrentTime = 50
	room.mu.Unlock()

	memberRec := record(t, peers[1])
	h.handleBufferingProgress(peers[1], inbound{Args: emit(t, bufferingRequest{BufferedSeconds: 4, Percent: 3})}, room)
	h.handleBufferingProgress(peers[1], inbound{Args: emit(t, bufferingRequest{BufferedSeconds: 3, Percent: 2})}, room)
	if got := memberRec.seen("peerBuffering"); got != 1 {
		t.Errorf("a member that stays short should announce once, got %d", got)
	}

	// Recovering must re-arm the announcement: the second time it falls behind is news again.
	h.handleBufferingProgress(peers[1], inbound{Args: emit(t, bufferingRequest{BufferedSeconds: 240, Percent: 100})}, room)
	h.handleBufferingProgress(peers[1], inbound{Args: emit(t, bufferingRequest{BufferedSeconds: 2, Percent: 1})}, room)
	if got := memberRec.seen("peerBuffering"); got != 2 {
		t.Errorf("a second fall should announce again, got %d", got)
	}
}

func TestBufferingUpdateCarriesTheScaledGoal(t *testing.T) {
	h := newMemHub(t, nil)
	room, peers := roomWith(h, "GOAL1", "host", "member")
	room.mu.Lock()
	room.currentTime = 1380 // twenty seconds from the end credits
	room.mu.Unlock()

	memberRec := record(t, peers[1])
	h.handleBufferingProgress(peers[1], inbound{Args: emit(t, bufferingRequest{BufferedSeconds: 11, Percent: 91})}, room)

	raw, ok := memberRec.waitFor("bufferingUpdate", time.Second)
	if !ok {
		t.Fatal("no bufferingUpdate delivered")
	}
	update := decode[map[string]any](t, raw)
	if goal, _ := update["bufferGoalSeconds"].(float64); goal > 30 {
		t.Errorf("near the end of an episode the goal should shrink, still asking for %v s", goal)
	}
	if percent, _ := update["percent"].(float64); percent != 91 {
		t.Errorf("the client's own reading should pass through, got %v", percent)
	}
}

func TestDriftIsPublishedForEveryMember(t *testing.T) {
	h := newMemHub(t, nil)
	room, peers := roomWith(h, "DRIFT", "host", "member")
	room.mu.Lock()
	room.isPlaying = true
	room.currentTime = 60
	room.mu.Unlock()

	memberRec := record(t, peers[1])
	h.handleHeartbeat(peers[1], inbound{Args: emit(t, heartbeatRequest{CurrentTime: 59.5})}, room)

	raw, ok := memberRec.waitFor("participantStates", time.Second)
	if !ok {
		t.Fatal("no participant states")
	}
	states := decode[struct {
		Peers      []map[string]any `json:"peers"`
		Canonical  float64          `json:"canonicalTime"`
		GroupState string           `json:"groupState"`
		Seq        int64            `json:"seq"`
	}](t, raw)
	if len(states.Peers) != 2 {
		t.Fatalf("both members should be listed, got %d", len(states.Peers))
	}
	var member map[string]any
	for _, p := range states.Peers {
		if p["userId"] == "user-member" {
			member = p
		}
	}
	if member == nil {
		t.Fatal("the reporting member is missing from its own state broadcast")
	}
	if drift, _ := member["driftMs"].(float64); drift < -501 || drift > -499 {
		t.Errorf("half a second behind should publish about -500 ms, got %v", drift)
	}
	if states.GroupState != string(groupStatePlaying) {
		t.Errorf("states should carry the phase, got %q", states.GroupState)
	}
	if states.Seq == 0 {
		t.Error("state broadcasts need a seq so a client can drop a superseded one")
	}
}

// ── Correction attribution ───────────────────────────────────────────────────

func TestCorrectionsNameTheClockTheyWereMeasuredAgainst(t *testing.T) {
	cases := []struct {
		name     string
		peerTime float64
		event    string
	}{
		// The ladder's two single-member bands, either of which a viewer can notice.
		{"a quiet seek for 400 ms", 59.6, "softCorrect"},
		{"a rate nudge for 800 ms", 59.2, "speedSeek"},
	}
	for _, tc := range cases {
		h := newMemHub(t, nil)
		room, peers := roomWith(h, "ATTR1", "host", "member")
		room.mu.Lock()
		room.isPlaying = true
		room.currentTime = 60
		room.mu.Unlock()

		memberRec := record(t, peers[1])
		h.handleHeartbeat(peers[1], inbound{Args: emit(t, heartbeatRequest{CurrentTime: tc.peerTime})}, room)

		raw, ok := memberRec.waitFor(tc.event, time.Second)
		if !ok {
			t.Fatalf("%s: expected %s, saw %v", tc.name, tc.event, memberRec.names())
		}
		correction := decode[map[string]any](t, raw)
		if from, _ := correction["fromUserId"].(string); from != "user-host" {
			t.Errorf("%s: a correction must name the clock it was measured against, got %q", tc.name, from)
		}
		want := (tc.peerTime - 60) * 1000
		if drift, _ := correction["driftMs"].(float64); drift < want-1 || drift > want+1 {
			t.Errorf("%s: drift should report %v ms, got %v", tc.name, want, drift)
		}
	}
}

// ── Clock service ────────────────────────────────────────────────────────────

func TestTimeSyncPongCarriesFourTimestamps(t *testing.T) {
	h := newMemHub(t, nil)
	clockPeer := h.withPeer("sock-clock", "user-clock", "clock")
	rec := record(t, clockPeer)

	before := nowMillis()
	h.handleTimeSync(clockPeer, inbound{Args: json.RawMessage(`{"clientSendTime":1234}`)})
	after := nowMillis()

	raw, ok := rec.waitFor("timesync_pong", time.Second)
	if !ok {
		t.Fatal("no pong")
	}
	pong := decode[struct {
		ClientSendTime json.RawMessage `json:"clientSendTime"`
		ServerTime     int64           `json:"serverTime"`
		ServerReceive  int64           `json:"serverReceiveTime"`
	}](t, raw)
	if pong.ClientSendTime.String() != "1234" {
		t.Errorf("the client's own stamp must come back unchanged, got %s", pong.ClientSendTime)
	}
	if pong.ServerReceive < before || pong.ServerReceive > after {
		t.Errorf("serverReceiveTime %d is outside the handler's own window [%d,%d]",
			pong.ServerReceive, before, after)
	}
	if pong.ServerTime < pong.ServerReceive {
		t.Error("the respond time must not precede the receive time")
	}
}

func TestOffsetAndPingFromAClientAreRecorded(t *testing.T) {
	h := newMemHub(t, nil)
	room, peers := roomWith(h, "OFFS1", "host", "member")

	offset, ping := 1432.0, 88.0
	h.handleTimeSyncOffset(peers[1], inbound{Args: emit(t,
		timeSyncOffsetRequest{OffsetMS: &offset, PingMS: &ping})}, room)

	room.mu.Lock()
	participant := room.participants[peers[1].sid]
	gotOffset, gotPing := participant.ClockOffsetMS, participant.PingMs
	room.mu.Unlock()

	if gotOffset != 1432 || gotPing != 88 {
		t.Errorf("stored offset=%d ping=%d, want 1432/88", gotOffset, gotPing)
	}
}

func TestHostHandoverHoldsCorrectionsBriefly(t *testing.T) {
	h := newMemHub(t, nil)
	room, peers := roomWith(h, "HOST1", "host", "member")
	memberRec := record(t, peers[1])

	h.handleTransferHost(peers[0], inbound{Args: emit(t,
		transferHostRequest{TargetUserID: "user-member"})}, room)

	raw, ok := memberRec.waitFor("hostChanged", time.Second)
	if !ok {
		t.Fatal("the room was never told who the host is now")
	}
	changed := decode[map[string]any](t, raw)
	resume := int64(changed["correctionsResumeAt"].(float64))
	if resume <= nowMillis() {
		t.Error("a handover must give the incoming host time to report a position")
	}
	if id, _ := changed["newHostUserId"].(string); id != "user-member" {
		t.Errorf("new host %q", id)
	}
}

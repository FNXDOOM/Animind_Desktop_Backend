package syncplay

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// Room state machine.
//
// One room has one authoritative clock. The host's heartbeat is what moves it, every other
// peer is corrected towards it, and any state change that would make peers disagree (a
// play, a seek, a new joiner) opens a buffer gate so nobody starts from a different
// position.
//
// Control is deliberately not host-only: play, pause and seek are accepted from any
// participant, matching how the feature was shipped. Host-only actions are transferHost and
// changeEpisode.

// readyState values reported in the peer list.
const (
	readyStateBuffering = "buffering"
	readyStateReady     = "ready"
)

// Participant is one socket inside a room. It doubles as the wire shape of a peer entry in
// every broadcast, so socketId and displayName are read by the deployed site's roster UI.
type Participant struct {
	SocketID        string    `json:"socketId"`
	UserID          string    `json:"userId"`
	DisplayName     string    `json:"displayName"`
	CurrentTime     float64   `json:"currentTime"`
	PlaybackRate    float64   `json:"playbackRate"`
	ReadyState      string    `json:"readyState"`
	BufferedAhead   float64   `json:"bufferedAhead"`
	Ready           bool      `json:"ready"`
	Stalling        bool      `json:"stalling"`
	LastHeartbeatAt time.Time `json:"-"`
	StallingSinceMs int64     `json:"-"`
	joinedAt        time.Time `json:"-"`
}

// Room is one watch party.
type Room struct {
	code      string
	episodeID string

	hostSocketID string
	hostUserID   string
	hostName     string

	currentTime float64
	isPlaying   bool

	participants map[string]*Participant

	waitingForReady      bool
	playIntentAfterReady bool
	readyPeers           map[string]bool
	readyTimers          map[string]*time.Timer
	escapeTimer          *time.Timer

	stallingPeers    map[string]bool
	stallGraceTimers map[string]*time.Timer

	createdAt time.Time
	mu        sync.Mutex
}

func newRoom(code, episodeID, hostSocketID, hostUserID, hostName string) *Room {
	now := time.Now()
	room := &Room{
		code:             code,
		episodeID:        episodeID,
		hostSocketID:     hostSocketID,
		hostUserID:       hostUserID,
		hostName:         hostName,
		participants:     map[string]*Participant{},
		readyPeers:       map[string]bool{},
		readyTimers:      map[string]*time.Timer{},
		stallingPeers:    map[string]bool{},
		stallGraceTimers: map[string]*time.Timer{},
		createdAt:        now,
	}
	room.participants[hostSocketID] = &Participant{
		SocketID: hostSocketID, UserID: hostUserID, DisplayName: hostName,
		ReadyState: readyStateReady, LastHeartbeatAt: now, joinedAt: now, PlaybackRate: 1,
	}
	return room
}

// payload shapes. Field names are the wire contract with socket.io-client.

type gatePayload struct {
	CurrentTime       float64        `json:"currentTime"`
	BufferGoalSeconds float64        `json:"bufferGoalSeconds,omitempty"`
	Peers             []*Participant `json:"peers"`
	TotalPeers        int            `json:"totalPeers"`
	ReadyCount        int            `json:"readyCount"`
	SentAt            int64          `json:"sentAt"`
}

type syncPayload struct {
	CurrentTime float64 `json:"currentTime"`
	SentAt      int64   `json:"sentAt"`
}

type pausePayload struct {
	CurrentTime float64 `json:"currentTime"`
	FromUserID  string  `json:"fromUserId"`
	SentAt      int64   `json:"sentAt"`
}

type seekPayload struct {
	Time       float64 `json:"time"`
	FromUserID string  `json:"fromUserId"`
	SentAt     int64   `json:"sentAt"`
}

type peerReadyPayload struct {
	UserID      string `json:"userId"`
	DisplayName string `json:"displayName"`
	ReadyCount  int    `json:"readyCount"`
	TotalPeers  int    `json:"totalPeers"`
	TimedOut    *bool  `json:"timedOut,omitempty"`
}

type peerPresencePayload struct {
	UserID           string `json:"userId"`
	DisplayName      string `json:"displayName"`
	ParticipantCount int    `json:"participantCount"`
}

type peerNamePayload struct {
	UserID      string `json:"userId"`
	DisplayName string `json:"displayName"`
}

type statesPayload struct {
	Peers         []*Participant `json:"peers"`
	CanonicalTime float64        `json:"canonicalTime"`
	IsPlaying     bool           `json:"isPlaying"`
}

type softCorrectPayload struct {
	CurrentTime float64 `json:"currentTime"`
}

type speedSeekPayload struct {
	Rate       float64 `json:"rate"`
	DurationMs int64   `json:"duration"`
	TargetTime float64 `json:"targetTime"`
}

type bufferingUpdatePayload struct {
	UserID            string  `json:"userId"`
	DisplayName       string  `json:"displayName"`
	BufferedSeconds   float64 `json:"bufferedSeconds"`
	Percent           float64 `json:"percent"`
	BufferGoalSeconds float64 `json:"bufferGoalSeconds"`
}

type deniedPayload struct {
	Reason string `json:"reason"`
}

type episodeChangedPayload struct {
	EpisodeID string `json:"episodeId"`
	SentAt    int64  `json:"sentAt"`
}

type hostChangedPayload struct {
	NewHostUserID      string `json:"newHostUserId"`
	NewHostDisplayName string `json:"newHostDisplayName"`
}

type timeSyncPongPayload struct {
	ClientSendTime json.RawMessage `json:"clientSendTime"`
	ServerTime     int64           `json:"serverTime"`
}

type roomSyncPayload struct {
	CurrentTime     float64 `json:"currentTime"`
	IsPlaying       bool    `json:"isPlaying"`
	WaitingForReady bool    `json:"waitingForReady"`
	SentAt          int64   `json:"sentAt"`
}

type createRoomRequest struct {
	EpisodeID string `json:"episodeId"`
}

type joinRoomRequest struct {
	RoomCode string `json:"roomCode"`
}

type timeRequest struct {
	CurrentTime float64 `json:"currentTime"`
}

type seekRequest struct {
	Time float64 `json:"time"`
}

type heartbeatRequest struct {
	CurrentTime   float64  `json:"currentTime"`
	PlaybackRate  *float64 `json:"playbackRate"`
	BufferedAhead *float64 `json:"bufferedAhead"`
}

type bufferingRequest struct {
	BufferedSeconds float64 `json:"bufferedSeconds"`
	Percent         float64 `json:"percent"`
}

type transferHostRequest struct {
	TargetUserID string `json:"targetUserId"`
}

type changeEpisodeRequest struct {
	EpisodeID string `json:"episodeId"`
}

type timeSyncRequest struct {
	ClientSendTime json.RawMessage `json:"clientSendTime"`
}

// joinAckPayload is the answer to joinRoom. isPlaying is always false because the buffer
// gate re-coordinates every peer before playback resumes.
type joinAckPayload struct {
	Success          bool                 `json:"success"`
	Error            string               `json:"error,omitempty"`
	EpisodeID        string               `json:"episodeId,omitempty"`
	CurrentTime      float64              `json:"currentTime"`
	IsPlaying        bool                 `json:"isPlaying"`
	HostUserID       string               `json:"hostUserId,omitempty"`
	ParticipantCount int                  `json:"participantCount"`
	Participants     []participantSummary `json:"participants,omitempty"`
	SentAt           int64                `json:"sentAt"`
}

type participantSummary struct {
	UserID      string `json:"userId"`
	DisplayName string `json:"displayName"`
}

type createRoomAckPayload struct {
	Success  bool   `json:"success"`
	Error    string `json:"error,omitempty"`
	RoomCode string `json:"roomCode,omitempty"`
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func (r *Room) peerStates() []*Participant {
	out := make([]*Participant, 0, len(r.participants))
	// Sort by socket id so a peer list does not reshuffle between broadcasts, which the
	// site renders as a roster and would otherwise flicker.
	ids := make([]string, 0, len(r.participants))
	for id := range r.participants {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		participant := r.participants[id]
		view := *participant
		view.Ready = r.readyPeers[id]
		view.Stalling = r.stallingPeers[id]
		out = append(out, &view)
	}
	return out
}

func (r *Room) broadcast(h *Hub, event string, payload any, exceptSocketID string) {
	h.mu.RLock()
	peers := make([]*peer, 0, len(h.peers))
	for _, p := range h.peers {
		if p.roomCode == r.code && p.sid != exceptSocketID {
			peers = append(peers, p)
		}
	}
	h.mu.RUnlock()
	for _, p := range peers {
		p.Emit(event, payload)
	}
}

// snapshot builds the HTTP view of a room.
func (r *Room) snapshot() RoomSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return RoomSnapshot{
		Code: r.code, EpisodeID: r.episodeID, HostUserID: r.hostUserID, HostDisplayName: r.hostName,
		Status: "active", CurrentTime: r.currentTime, IsPlaying: r.isPlaying,
		Participants: len(r.participants),
	}
}

func (r *Room) close(h *Hub) {
	r.mu.Lock()
	h.stopTimers(r)
	r.mu.Unlock()

	r.broadcast(h, "syncPaused", syncPayload{CurrentTime: r.currentTime, SentAt: nowMillis()}, "")
	h.mu.Lock()
	delete(h.rooms, r.code)
	h.mu.Unlock()

	if err := h.repo.EndParty(context.Background(), r.code); err != nil {
		h.logger.Warn("could not mark watch party ended", slog.String("room", r.code), slog.String("error", err.Error()))
	}
}

func nowMillis() int64 { return time.Now().UnixMilli() }

// ── Event handlers ───────────────────────────────────────────────────────────

func (h *Hub) handleCreateRoom(p *peer, in inbound, existing *Room) {
	var req createRoomRequest
	if err := json.Unmarshal(in.Args, &req); err != nil {
		h.reply(p, in, createRoomAckPayload{Success: false, Error: "Episode is required."})
		return
	}
	episodeID := strings.TrimSpace(req.EpisodeID)
	if episodeID == "" {
		h.reply(p, in, createRoomAckPayload{Success: false, Error: "Episode is required."})
		return
	}
	// An unknown episode would create a room nobody can join correctly.
	if !h.repo.EpisodeExists(context.Background(), episodeID) {
		h.reply(p, in, createRoomAckPayload{Success: false, Error: "Episode is not in the library."})
		return
	}
	if existing != nil {
		// Leaving the previous room keeps its roster honest; a stale entry there would
		// block its buffer gate forever.
		existing.handlePeerLeft(p, h)
	}

	code, err := h.newRoomCode()
	if err != nil {
		h.reply(p, in, createRoomAckPayload{Success: false, Error: "Could not create the room."})
		return
	}

	room := newRoom(code, episodeID, p.sid, p.userID, p.name)
	h.mu.Lock()
	h.rooms[code] = room
	h.mu.Unlock()
	p.roomCode = code

	if err := h.repo.CreateParty(context.Background(), code, p.userID, p.name, &episodeID); err != nil {
		h.logger.Warn("watch party could not be recorded", slog.String("room", code), slog.String("error", err.Error()))
	}
	h.reply(p, in, createRoomAckPayload{Success: true, RoomCode: code})
	h.logger.Info("watch party created", slog.String("room", code), slog.String("host", p.userID))
}

func (h *Hub) handleJoinRoom(p *peer, in inbound, existing *Room) {
	var req joinRoomRequest
	if err := json.Unmarshal(in.Args, &req); err != nil {
		h.reply(p, in, joinAckPayload{Success: false, Error: "Room code is required."})
		return
	}
	code := strings.ToUpper(strings.TrimSpace(req.RoomCode))
	if code == "" {
		h.reply(p, in, joinAckPayload{Success: false, Error: "Room code is required."})
		return
	}
	if existing != nil && existing.code != code {
		existing.handlePeerLeft(p, h)
	}

	h.mu.RLock()
	room := h.rooms[code]
	h.mu.RUnlock()
	if room == nil {
		h.reply(p, in, joinAckPayload{Success: false, Error: "Room not found."})
		return
	}

	room.mu.Lock()
	if len(room.participants) >= h.cfg.SyncPlay.MaxParticipants {
		room.mu.Unlock()
		h.reply(p, in, joinAckPayload{Success: false, Error: "Room is full."})
		return
	}
	now := time.Now()
	room.participants[p.sid] = &Participant{
		SocketID: p.sid, UserID: p.userID, DisplayName: p.name,
		CurrentTime: room.currentTime, PlaybackRate: 1, ReadyState: readyStateBuffering,
		LastHeartbeatAt: now, joinedAt: now,
	}
	p.roomCode = code
	wasPlaying := room.isPlaying
	room.isPlaying = false
	count := len(room.participants)
	summaries := room.uniqueParticipantsLocked()
	currentTime := room.currentTime
	episodeID := room.episodeID
	hostUserID := room.hostUserID
	room.mu.Unlock()

	if err := h.repo.RecordPartyParticipant(context.Background(), code, p.userID); err != nil {
		h.logger.Warn("watch party participant could not be recorded",
			slog.String("room", code), slog.String("error", err.Error()))
	}

	room.broadcast(h, "peerJoined", peerPresencePayload{
		UserID: p.userID, DisplayName: p.name, ParticipantCount: count,
	}, p.sid)

	h.reply(p, in, joinAckPayload{
		Success: true, EpisodeID: episodeID, CurrentTime: currentTime, IsPlaying: false,
		HostUserID: hostUserID, ParticipantCount: count, Participants: summaries, SentAt: nowMillis(),
	})

	// Every join re-gates the room: the newcomer is behind, and starting it late is the
	// only way to keep the group together.
	room.initiateBufferGate(h, wasPlaying)
}

func (r *Room) uniqueParticipantsLocked() []participantSummary {
	seen := map[string]bool{}
	out := make([]participantSummary, 0, len(r.participants))
	for _, participant := range r.participants {
		if seen[participant.UserID] {
			continue
		}
		seen[participant.UserID] = true
		out = append(out, participantSummary{UserID: participant.UserID, DisplayName: participant.DisplayName})
	}
	return out
}

func (h *Hub) handlePlay(p *peer, in inbound, room *Room) {
	var req timeRequest
	if err := json.Unmarshal(in.Args, &req); err != nil {
		return
	}
	if !h.ensureParticipant(p, room) {
		return
	}

	room.mu.Lock()
	room.currentTime = req.CurrentTime
	solo := len(room.participants) == 1
	if solo {
		room.isPlaying = true
	}
	currentTime := room.currentTime
	room.mu.Unlock()

	if solo {
		// With one viewer there is nothing to coordinate.
		p.Emit("syncPlay", syncPayload{CurrentTime: currentTime, SentAt: nowMillis()})
		return
	}
	room.initiateBufferGate(h, true)
}

func (h *Hub) handlePause(p *peer, in inbound, room *Room) {
	var req timeRequest
	if err := json.Unmarshal(in.Args, &req); err != nil {
		return
	}
	if !h.ensureParticipant(p, room) {
		return
	}

	room.mu.Lock()
	room.isPlaying = false
	room.currentTime = req.CurrentTime
	currentTime := room.currentTime
	// A pause cancels an open gate; otherwise the room would resume playing after the
	// stragglers finished buffering.
	if room.waitingForReady {
		h.cancelGateLocked(room)
	}
	room.mu.Unlock()

	room.broadcast(h, "pause", pausePayload{
		CurrentTime: req.CurrentTime, FromUserID: p.userID, SentAt: nowMillis(),
	}, p.sid)
	_ = currentTime
}

func (h *Hub) handleSeek(p *peer, in inbound, room *Room) {
	var req seekRequest
	if err := json.Unmarshal(in.Args, &req); err != nil {
		return
	}
	if !h.ensureParticipant(p, room) {
		return
	}

	room.mu.Lock()
	wasPlaying := room.isPlaying
	room.currentTime = req.Time
	room.mu.Unlock()

	if len(room.participants) == 1 {
		p.Emit("seek", seekPayload{Time: req.Time, FromUserID: p.userID, SentAt: nowMillis()})
		return
	}
	// In a group a seek is a re-gate whose target the gate announces, so nobody plays the
	// old position while buffering catches up.
	room.initiateBufferGate(h, wasPlaying)
}

func (h *Hub) handleReady(p *peer, room *Room) {
	if room == nil {
		return
	}
	room.mu.Lock()
	if !room.waitingForReady || room.readyPeers[p.sid] {
		room.mu.Unlock()
		return
	}
	room.readyPeers[p.sid] = true
	if participant := room.participants[p.sid]; participant != nil {
		participant.ReadyState = readyStateReady
	}
	if timer := room.readyTimers[p.sid]; timer != nil {
		timer.Stop()
		delete(room.readyTimers, p.sid)
	}
	readyCount := len(room.readyPeers)
	total := len(room.participants)
	name := p.name
	room.mu.Unlock()

	room.broadcast(h, "peerReady", peerReadyPayload{
		UserID: p.userID, DisplayName: name, ReadyCount: readyCount, TotalPeers: total,
	}, "")
	room.tryResumeAfterReady(h)
}

func (h *Hub) handleBufferingProgress(p *peer, in inbound, room *Room) {
	var req bufferingRequest
	if err := json.Unmarshal(in.Args, &req); err != nil {
		return
	}
	if room == nil {
		return
	}

	room.mu.Lock()
	participant := room.participants[p.sid]
	if participant == nil {
		room.mu.Unlock()
		return
	}
	participant.BufferedAhead = req.BufferedSeconds
	shouldReady := req.BufferedSeconds >= h.cfg.SyncPlay.BufferGoal.Seconds() &&
		room.waitingForReady && !room.readyPeers[p.sid]
	if shouldReady {
		room.readyPeers[p.sid] = true
		participant.ReadyState = readyStateReady
		if timer := room.readyTimers[p.sid]; timer != nil {
			timer.Stop()
			delete(room.readyTimers, p.sid)
		}
	}
	readyCount := len(room.readyPeers)
	total := len(room.participants)
	room.mu.Unlock()

	room.broadcast(h, "bufferingUpdate", bufferingUpdatePayload{
		UserID: p.userID, DisplayName: participant.DisplayName,
		BufferedSeconds: req.BufferedSeconds, Percent: req.Percent,
		BufferGoalSeconds: h.cfg.SyncPlay.BufferGoal.Seconds(),
	}, "")

	if shouldReady {
		room.broadcast(h, "peerReady", peerReadyPayload{
			UserID: p.userID, DisplayName: participant.DisplayName,
			ReadyCount: readyCount, TotalPeers: total,
		}, "")
		room.tryResumeAfterReady(h)
	}
}

func (h *Hub) handleBuffering(p *peer, room *Room) {
	if room == nil {
		return
	}
	room.mu.Lock()
	skip := room.waitingForReady || !room.isPlaying
	room.mu.Unlock()
	if skip {
		return
	}
	h.handlePeerStall(p, room)
}

func (h *Hub) handleStallRecovered(p *peer, room *Room) {
	if room == nil {
		return
	}
	h.handlePeerStallRecovered(p, room)
}

func (h *Hub) handleHeartbeat(p *peer, in inbound, room *Room) {
	var req heartbeatRequest
	if err := json.Unmarshal(in.Args, &req); err != nil {
		return
	}
	if room == nil {
		return
	}

	now := time.Now()
	room.mu.Lock()
	participant := room.participants[p.sid]
	if participant == nil {
		room.mu.Unlock()
		return
	}
	previous := participant.CurrentTime
	participant.CurrentTime = req.CurrentTime
	participant.LastHeartbeatAt = now
	if req.PlaybackRate != nil {
		participant.PlaybackRate = *req.PlaybackRate
	}
	if req.BufferedAhead != nil {
		participant.BufferedAhead = *req.BufferedAhead
	}

	// Only the host moves the canonical clock while playing; a paused room keeps the
	// position its last pause set.
	if p.sid == room.hostSocketID && room.isPlaying {
		room.currentTime = req.CurrentTime
	}

	// Heartbeat-based stall detection: a peer whose position does not advance for the
	// detect window is declared stalling.
	stallSignal := false
	recovered := false
	delta := absFloat(req.CurrentTime - previous)
	if room.isPlaying && !room.waitingForReady {
		if delta < 0.1 && participant.StallingSinceMs == 0 {
			participant.StallingSinceMs = now.UnixMilli()
		} else if delta >= 0.1 && participant.StallingSinceMs > 0 {
			participant.StallingSinceMs = 0
			recovered = true
		}
		if participant.StallingSinceMs > 0 &&
			now.UnixMilli()-participant.StallingSinceMs >= h.cfg.SyncPlay.StallDetectWindow.Milliseconds() {
			stallSignal = true
		}
	}
	playing := room.isPlaying
	waiting := room.waitingForReady
	room.mu.Unlock()

	if recovered {
		h.handlePeerStallRecovered(p, room)
	}
	if stallSignal {
		h.handlePeerStall(p, room)
	}

	room.broadcast(h, "participantStates", statesPayload{
		Peers: room.peerStates(), CanonicalTime: room.currentTime, IsPlaying: playing,
	}, "")

	if playing && !waiting {
		h.checkDrift(p, room, req.CurrentTime)
	}
}

func (h *Hub) handleTransferHost(p *peer, in inbound, room *Room) {
	var req transferHostRequest
	if err := json.Unmarshal(in.Args, &req); err != nil {
		return
	}
	if room == nil {
		return
	}
	room.mu.Lock()
	if p.sid != room.hostSocketID {
		room.mu.Unlock()
		p.Emit("syncDenied", deniedPayload{Reason: "Only the current host can transfer control."})
		return
	}
	targetSocketID, targetName := "", ""
	for _, participant := range sortedParticipants(room) {
		if participant.UserID == strings.TrimSpace(req.TargetUserID) {
			targetSocketID, targetName = participant.SocketID, participant.DisplayName
			break
		}
	}
	if targetSocketID == "" {
		room.mu.Unlock()
		p.Emit("syncDenied", deniedPayload{Reason: "Target user is not in the room."})
		return
	}
	room.hostSocketID = targetSocketID
	room.hostUserID = strings.TrimSpace(req.TargetUserID)
	room.hostName = targetName
	room.mu.Unlock()

	room.broadcast(h, "hostChanged", hostChangedPayload{
		NewHostUserID: room.hostUserID, NewHostDisplayName: targetName,
	}, "")
	h.logger.Info("watch party host transferred",
		slog.String("room", room.code), slog.String("new_host", room.hostUserID))
}

func (h *Hub) handleChangeEpisode(p *peer, in inbound, room *Room) {
	var req changeEpisodeRequest
	if err := json.Unmarshal(in.Args, &req); err != nil {
		return
	}
	if room == nil {
		return
	}
	room.mu.Lock()
	if p.sid != room.hostSocketID {
		room.mu.Unlock()
		p.Emit("syncDenied", deniedPayload{Reason: "Only the host can change the episode."})
		return
	}
	room.episodeID = strings.TrimSpace(req.EpisodeID)
	room.currentTime = 0
	room.isPlaying = false
	h.cancelGateLocked(room)
	// Stale buffer reports from the previous episode must not satisfy the next gate.
	for _, participant := range room.participants {
		participant.BufferedAhead = 0
		participant.ReadyState = readyStateBuffering
		participant.CurrentTime = 0
		participant.StallingSinceMs = 0
	}
	episodeID := room.episodeID
	room.mu.Unlock()

	room.broadcast(h, "episodeChanged", episodeChangedPayload{EpisodeID: episodeID, SentAt: nowMillis()}, p.sid)
	h.logger.Info("watch party episode changed", slog.String("room", room.code), slog.String("episode", episodeID))
}

func (h *Hub) handleTimeSync(p *peer, in inbound) {
	var req timeSyncRequest
	if err := json.Unmarshal(in.Args, &req); err != nil {
		req.ClientSendTime = json.RawMessage("null")
	}
	// The pong echoes the client's own timestamp so it can compute round-trip offset.
	p.Emit("timesync_pong", timeSyncPongPayload{ClientSendTime: req.ClientSendTime, ServerTime: nowMillis()})
}

func (h *Hub) handleRequestSync(p *peer, room *Room) {
	if room == nil {
		return
	}
	room.mu.Lock()
	payload := roomSyncPayload{
		CurrentTime: room.currentTime, IsPlaying: room.isPlaying,
		WaitingForReady: room.waitingForReady, SentAt: nowMillis(),
	}
	room.mu.Unlock()
	p.Emit("sync", payload)
}

// ── Gate logic ───────────────────────────────────────────────────────────────

// initiateBufferGate opens the ready gate for every participant and pauses playback until
// all of them report enough buffer, the per-peer timeout fires, or the escape hatch decides
// waiting is no longer productive.
func (r *Room) initiateBufferGate(h *Hub, playAfterReady bool) {
	r.mu.Lock()
	h.stopTimers(r)
	r.readyPeers = map[string]bool{}
	r.waitingForReady = true
	r.playIntentAfterReady = playAfterReady
	r.isPlaying = false
	for _, participant := range r.participants {
		participant.ReadyState = readyStateBuffering
	}
	states := r.peerStates()
	goalSeconds := h.cfg.SyncPlay.BufferGoal.Seconds()
	gate := gatePayload{
		CurrentTime: r.currentTime, BufferGoalSeconds: goalSeconds, Peers: states,
		TotalPeers: len(r.participants), ReadyCount: 0, SentAt: nowMillis(),
	}
	legacy := gatePayload{
		CurrentTime: r.currentTime, Peers: states,
		TotalPeers: len(r.participants), ReadyCount: 0, SentAt: nowMillis(),
	}
	timeout := h.cfg.SyncPlay.ReadyTimeout
	escapeWait := h.cfg.SyncPlay.EscapeWait
	escapeBuffer := h.cfg.SyncPlay.EscapeBuffer.Seconds()
	code := r.code
	peers := make([]string, 0, len(r.participants))
	for id := range r.participants {
		peers = append(peers, id)
	}
	r.mu.Unlock()

	// Both events are sent: newer clients gate on buffer, older ones on readiness alone.
	r.broadcast(h, "waitForBufferGoal", gate, "")
	r.broadcast(h, "waitForReady", legacy, "")

	for _, socketID := range peers {
		id := socketID
		r.mu.Lock()
		r.readyTimers[id] = time.AfterFunc(timeout, func() {
			h.forceReady(id, code)
		})
		r.mu.Unlock()
	}

	r.mu.Lock()
	r.escapeTimer = time.AfterFunc(escapeWait, func() { h.runEscapeHatch(code, escapeBuffer) })
	r.mu.Unlock()
}

// forceReady satisfies one straggler's gate when its timer fires, so a stalled or offline
// peer cannot hold the party hostage.
func (h *Hub) forceReady(socketID, code string) {
	h.mu.RLock()
	room := h.rooms[code]
	h.mu.RUnlock()
	if room == nil {
		return
	}
	room.mu.Lock()
	if !room.waitingForReady || room.readyPeers[socketID] {
		room.mu.Unlock()
		return
	}
	room.readyPeers[socketID] = true
	userID, displayName := socketID, socketID
	if participant := room.participants[socketID]; participant != nil {
		userID, displayName = participant.UserID, participant.DisplayName
		participant.ReadyState = readyStateReady
	}
	if timer := room.readyTimers[socketID]; timer != nil {
		timer.Stop()
		delete(room.readyTimers, socketID)
	}
	readyCount := len(room.readyPeers)
	total := len(room.participants)
	room.mu.Unlock()

	timedOut := true
	room.broadcast(h, "peerReady", peerReadyPayload{
		UserID: userID, DisplayName: displayName, ReadyCount: readyCount, TotalPeers: total, TimedOut: &timedOut,
	}, "")
	room.tryResumeAfterReady(h)
}

// runEscapeHatch ends a gate early when at least one peer has useful buffer or somebody is
// already ready, rather than letting a single slow client stall the party indefinitely.
func (h *Hub) runEscapeHatch(code string, bufferGoal float64) {
	h.mu.RLock()
	room := h.rooms[code]
	h.mu.RUnlock()
	if room == nil {
		return
	}
	room.mu.Lock()
	if !room.waitingForReady {
		room.mu.Unlock()
		return
	}
	anyBuffered := false
	for _, participant := range room.participants {
		if participant.BufferedAhead >= bufferGoal {
			anyBuffered = true
			break
		}
	}
	if !anyBuffered && len(room.readyPeers) == 0 {
		room.mu.Unlock()
		return
	}
	for id := range room.participants {
		room.readyPeers[id] = true
		if participant := room.participants[id]; participant != nil {
			participant.ReadyState = readyStateReady
		}
	}
	room.mu.Unlock()
	room.tryResumeAfterReady(h)
}

// tryResumeAfterReady closes the gate once every participant is ready.
func (r *Room) tryResumeAfterReady(h *Hub) {
	r.mu.Lock()
	if !r.waitingForReady {
		r.mu.Unlock()
		return
	}
	if len(r.readyPeers) < len(r.participants) {
		r.mu.Unlock()
		return
	}
	r.waitingForReady = false
	h.stopTimers(r)
	shouldPlay := r.playIntentAfterReady
	r.playIntentAfterReady = false
	if shouldPlay {
		r.isPlaying = true
	}
	payload := syncPayload{CurrentTime: r.currentTime, SentAt: nowMillis()}
	r.mu.Unlock()

	if shouldPlay {
		r.broadcast(h, "syncPlay", payload, "")
		return
	}
	r.broadcast(h, "syncPaused", payload, "")
}

// cancelGateLocked aborts an open gate. Callers hold r.mu.
func (h *Hub) cancelGateLocked(room *Room) {
	room.waitingForReady = false
	room.playIntentAfterReady = false
	h.stopTimers(room)
	room.readyPeers = map[string]bool{}
}

// stopTimers stops and clears every pending gate and grace timer. Callers hold room.mu.
func (h *Hub) stopTimers(room *Room) {
	for id, timer := range room.readyTimers {
		if timer != nil {
			timer.Stop()
		}
		delete(room.readyTimers, id)
	}
	for id, timer := range room.stallGraceTimers {
		if timer != nil {
			timer.Stop()
		}
		delete(room.stallGraceTimers, id)
	}
	if room.escapeTimer != nil {
		room.escapeTimer.Stop()
		room.escapeTimer = nil
	}
	room.stallingPeers = map[string]bool{}
}

// ── Drift and stall ──────────────────────────────────────────────────────────

// checkDrift corrects a peer that has fallen out of step. The decision is made by decideDrift
// so the ladder can be tested without a socket, a room, or a clock.
func (h *Hub) checkDrift(p *peer, room *Room, peerTime float64) {
	room.mu.Lock()
	if !room.isPlaying || room.waitingForReady {
		room.mu.Unlock()
		return
	}
	canonical := room.currentTime
	room.mu.Unlock()

	action, rate, durationMS := decideDrift(peerTime, canonical,
		h.cfg.SyncPlay.SoftSeekThreshold.Seconds()*1000,
		h.cfg.SyncPlay.SpeedSyncMaxDrift.Seconds()*1000)

	switch action {
	case driftNone:
		return
	case driftSoftCorrect:
		p.Emit("softCorrect", softCorrectPayload{CurrentTime: canonical})
	case driftSpeedSeek:
		p.Emit("speedSeek", speedSeekPayload{Rate: rate, DurationMs: durationMS, TargetTime: canonical})
	case driftRegate:
		room.initiateBufferGate(h, true)
	}
}

// driftAction is the correction one peer needs.
type driftAction int

const (
	// driftNone is used inside the tolerated window: no packet, no visible stutter.
	driftNone driftAction = iota
	// driftSoftCorrect tells the client to seek quietly.
	driftSoftCorrect
	// driftSpeedSeek nudges the playback rate instead of seeking, so a small gap closes
	// without costing anyone a buffer refill.
	driftSpeedSeek
	// driftRegate re-gates the whole room, because the gap is too large to catch up politely.
	driftRegate
)

// softCorrectCeilingMS separates a quiet seek from a speed change. It is a fixed value rather
// than a setting: it marks the point where a seek would cost more buffer than it saves.
const softCorrectCeilingMS = 500.0

// maxSpeedSeekMS caps how long a rate nudge lasts, so a wild drift becomes a re-gate rather
// than minutes of fast playback.
const maxSpeedSeekMS = int64(8000)

// decideDrift classifies one peer against the canonical time.
//
// Peer times are seconds, so the drift is converted before it is compared with the
// millisecond thresholds. A peer behind the group speeds up at 1.5x; a peer ahead of it slows
// to 0.8x; the duration is twice the drift because halving the error at the corrected rate is
// what "catching up" means.
func decideDrift(peerTime, canonicalTime, softThresholdMS, speedThresholdMS float64) (action driftAction, rate float64, durationMS int64) {
	driftMS := absFloat(peerTime-canonicalTime) * 1000
	switch {
	case driftMS < softThresholdMS:
		return driftNone, 0, 0
	case driftMS < softCorrectCeilingMS:
		return driftSoftCorrect, 0, 0
	case driftMS < speedThresholdMS:
		rate = 0.8
		if peerTime < canonicalTime {
			rate = 1.5
		}
		// Rounded rather than truncated: a drift of 0.6s is 1200ms of correction, not 1199.
		durationMS = int64(math.Round(driftMS * 2))
		if durationMS > maxSpeedSeekMS {
			durationMS = maxSpeedSeekMS
		}
		return driftSpeedSeek, rate, durationMS
	default:
		return driftRegate, 0, 0
	}
}

// handlePeerStall announces a buffering peer and re-gates the room if it does not recover
// within the grace period.
func (h *Hub) handlePeerStall(p *peer, room *Room) {
	room.mu.Lock()
	if room.waitingForReady || !room.isPlaying || room.stallingPeers[p.sid] {
		room.mu.Unlock()
		return
	}
	room.stallingPeers[p.sid] = true
	displayName := p.name
	if participant := room.participants[p.sid]; participant != nil {
		displayName = participant.DisplayName
	}
	code := room.code
	grace := h.cfg.SyncPlay.StallGrace
	room.mu.Unlock()

	room.broadcast(h, "peerStalling", peerNamePayload{UserID: p.userID, DisplayName: displayName}, "")

	room.mu.Lock()
	room.stallGraceTimers[p.sid] = time.AfterFunc(grace, func() {
		h.stallGraceExpired(p.sid, code)
	})
	room.mu.Unlock()
}

// stallGraceExpired re-gates the room when a stalling peer never recovered.
func (h *Hub) stallGraceExpired(socketID, code string) {
	h.mu.RLock()
	room := h.rooms[code]
	h.mu.RUnlock()
	if room == nil {
		return
	}
	room.mu.Lock()
	delete(room.stallGraceTimers, socketID)
	if !room.stallingPeers[socketID] || room.waitingForReady {
		room.mu.Unlock()
		return
	}
	delete(room.stallingPeers, socketID)
	room.mu.Unlock()

	// The stalled peer is behind everyone else, so the whole room re-gates around it.
	room.initiateBufferGate(h, true)
}

func (h *Hub) handlePeerStallRecovered(p *peer, room *Room) {
	room.mu.Lock()
	if !room.stallingPeers[p.sid] {
		room.mu.Unlock()
		return
	}
	delete(room.stallingPeers, p.sid)
	if timer := room.stallGraceTimers[p.sid]; timer != nil {
		timer.Stop()
		delete(room.stallGraceTimers, p.sid)
	}
	if participant := room.participants[p.sid]; participant != nil {
		participant.StallingSinceMs = 0
	}
	displayName := p.name
	room.mu.Unlock()

	room.broadcast(h, "peerStallRecovered", peerNamePayload{UserID: p.userID, DisplayName: displayName}, "")
}

// ── Membership ───────────────────────────────────────────────────────────────

// ensureParticipant answers whether a peer may act in a room. A peer that left without
// disconnecting, or one that never joined, is refused silently: the clients drive their own
// optimistic UI and re-sync on the next state broadcast.
func (h *Hub) ensureParticipant(p *peer, room *Room) bool {
	if room == nil {
		return false
	}
	room.mu.Lock()
	defer room.mu.Unlock()
	participant := room.participants[p.sid]
	if participant == nil {
		return false
	}
	participant.DisplayName = p.name
	return true
}

// handlePeerLeft removes a peer, promotes a new host when needed, and closes an empty room.
func (r *Room) handlePeerLeft(p *peer, h *Hub) {
	r.mu.Lock()
	if _, ok := r.participants[p.sid]; !ok {
		r.mu.Unlock()
		p.roomCode = ""
		return
	}
	displayName := p.name
	userID := p.userID
	delete(r.participants, p.sid)
	delete(r.readyPeers, p.sid)
	delete(r.stallingPeers, p.sid)
	if timer := r.readyTimers[p.sid]; timer != nil {
		timer.Stop()
		delete(r.readyTimers, p.sid)
	}
	if timer := r.stallGraceTimers[p.sid]; timer != nil {
		timer.Stop()
		delete(r.stallGraceTimers, p.sid)
	}
	remaining := len(r.participants)
	emptiedHost := p.sid == r.hostSocketID && remaining > 0
	waiting := r.waitingForReady

	var successor *Participant
	if emptiedHost {
		// Promotion follows join order: the earliest surviving peer is the one most likely
		// to have watched the whole episode so far.
		successor = sortedParticipants(r)[0]
		r.hostSocketID = successor.SocketID
		r.hostUserID = successor.UserID
		r.hostName = successor.DisplayName
	}
	// A departure lowers the bar for the gate: without this the room would wait forever for
	// a peer that is gone.
	if waiting {
		r.mu.Unlock()
		r.tryResumeAfterReady(h)
		r.mu.Lock()
	}
	code := r.code
	r.mu.Unlock()
	p.roomCode = ""

	if emptiedHost {
		r.broadcast(h, "hostChanged", hostChangedPayload{
			NewHostUserID: successor.UserID, NewHostDisplayName: successor.DisplayName,
		}, "")
		h.logger.Info("watch party host promoted", slog.String("room", code), slog.String("new_host", successor.UserID))
	}

	if remaining == 0 {
		h.mu.Lock()
		delete(h.rooms, code)
		h.mu.Unlock()
		r.mu.Lock()
		h.stopTimers(r)
		r.mu.Unlock()
		if err := h.repo.EndParty(context.Background(), code); err != nil {
			h.logger.Warn("could not end watch party", slog.String("room", code), slog.String("error", err.Error()))
		}
		h.logger.Info("watch party closed", slog.String("room", code))
		return
	}

	r.broadcast(h, "peerLeft", peerPresencePayload{
		UserID: userID, DisplayName: displayName, ParticipantCount: remaining,
	}, "")
}

// sortedParticipants returns peers in socket-id order for deterministic host promotion and
// listing. Callers hold room.mu.
func sortedParticipants(room *Room) []*Participant {
	out := make([]*Participant, 0, len(room.participants))
	for _, participant := range room.participants {
		out = append(out, participant)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].joinedAt.Equal(out[j].joinedAt) {
			return out[i].SocketID < out[j].SocketID
		}
		return out[i].joinedAt.Before(out[j].joinedAt)
	})
	return out
}

// reply answers an event that requested an acknowledgement, or emits it as a plain event
// when the client sent a bare event.
func (h *Hub) reply(p *peer, in inbound, payload any) {
	if in.ACKID != nil {
		p.EmitAck(*in.ACKID, payload)
		return
	}
	// socket.io-client also accepts an event-shaped reply for the ACK-less code path.
	switch body := payload.(type) {
	case createRoomAckPayload:
		if !body.Success {
			p.Emit("syncDenied", deniedPayload{Reason: body.Error})
			return
		}
		p.Emit("roomCreated", map[string]any{"roomCode": body.RoomCode, "success": true})
	case joinAckPayload:
		if !body.Success {
			p.Emit("syncDenied", deniedPayload{Reason: body.Error})
			return
		}
		p.Emit("roomJoined", body)
	default:
		p.Emit("syncDenied", deniedPayload{Reason: fmt.Sprintf("unhandled reply %T", body)})
	}
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

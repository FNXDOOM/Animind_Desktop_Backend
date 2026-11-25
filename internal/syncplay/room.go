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

	"animind-backend-go/internal/repository"
)

// readyState values reported in the peer list.
const (
	readyStateBuffering = "buffering"
	readyStateReady     = "ready"
)

// Participant is one socket inside a room; it doubles as the wire shape of a peer entry in every
// broadcast, so its json names are a deployed-client contract.
type Participant struct {
	SocketID      string  `json:"socketId"`
	UserID        string  `json:"userId"`
	DisplayName   string  `json:"displayName"`
	CurrentTime   float64 `json:"currentTime"`
	PlaybackRate  float64 `json:"playbackRate"`
	ReadyState    string  `json:"readyState"`
	BufferedAhead float64 `json:"bufferedAhead"`
	Ready         bool    `json:"ready"`
	Stalling      bool    `json:"stalling"`

	// PingMs is this member's round trip; the start is scheduled off the room's highest so the
	// peer that hears last still keeps up.
	PingMs int64 `json:"pingMs"`

	// IgnoreWait marks a member the gate does not hold for; shown in the roster.
	IgnoreWait bool `json:"ignoreWait"`

	// DriftMS is position minus the canonical clock at the last heartbeat; published for humans,
	// nothing decides from it.
	DriftMS float64 `json:"driftMs"`

	// ClockOffsetMS is the member's own clock-error belief, published so it is legible outside it.

	ClockOffsetMS   int64     `json:"clockOffsetMs"`
	LastHeartbeatAt time.Time `json:"-"`
	StallingSinceMs int64     `json:"-"`

	// bufferingAnnounced latches the peerBuffering broadcast so a member that reports its
	// buffer once a second does not notify the room once a second.
	bufferingAnnounced bool
	joinedAt           time.Time `json:"-"`
}

// Room is one watch party.
type Room struct {
	code      string
	episodeID string
	showID    string

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
	waitDeadlineTimer    *time.Timer
	gateOpenedAt         time.Time

	// gateEpoch tags one gate opening: a callback already running cannot be stopped, so this is
	// what keeps a closed gate's forceReady from granting readiness to a later gate.
	gateEpoch int64

	stallingPeers    map[string]bool
	stallGraceTimers map[string]*time.Timer

	// seq counts broadcasts so clients can order packets that overtook each other; it only
	// ever increases.
	seq int64
	// everActed separates "never played" from "paused", which the wire cannot tell apart.
	everActed bool
	// durationSeconds is the episode length, used to shrink the buffer goal near the end; zero
	// means unknown and leaves the goal at its configured value.

	durationSeconds float64

	createdAt time.Time
	mu        sync.Mutex
}

func newRoom(code, episodeID, showID, hostSocketID, hostUserID, hostName string, durationSeconds float64) *Room {
	now := time.Now()
	room := &Room{
		code:             code,
		episodeID:        episodeID,
		showID:           showID,
		durationSeconds:  durationSeconds,
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

// payload shapes; field names are the wire contract with socket.io-client, and every one embeds
// stamped, which the hub fills with the sequence and its clock at emit time.

type gatePayload struct {
	stamped
	CurrentTime       float64        `json:"currentTime"`
	BufferGoalSeconds float64        `json:"bufferGoalSeconds,omitempty"`
	Peers             []*Participant `json:"peers"`
	TotalPeers        int            `json:"totalPeers"`
	ReadyCount        int            `json:"readyCount"`
	GroupState        groupState     `json:"groupState"`
	SentAt            int64          `json:"sentAt"`
	// WaitDeadlineMs is when the gate closes by itself, so a client shows time remaining, not
	// a spinner.
	WaitDeadlineMs int64 `json:"waitDeadlineMs,omitempty"`
}

type syncPayload struct {
	stamped
	CurrentTime float64    `json:"currentTime"`
	IsPlaying   bool       `json:"isPlaying"`
	GroupState  groupState `json:"groupState"`
	SentAt      int64      `json:"sentAt"`
	// ScheduledPlayAt is the instant to start at. Zero means "nothing was scheduled", which a
	// client must read as act-now rather than act at the epoch.
	ScheduledPlayAt int64 `json:"scheduledPlayAt,omitempty"`
}

// allReadyPayload closes a buffer gate so the web client, which listens by name, ends it with
// one scheduled start rather than a resume per member.

type allReadyPayload struct {
	stamped
	CurrentTime     float64    `json:"currentTime"`
	ScheduledPlayAt int64      `json:"scheduledPlayAt,omitempty"`
	ReadyCount      int        `json:"readyCount"`
	TotalPeers      int        `json:"totalPeers"`
	PlayIntent      bool       `json:"playIntent"`
	GroupState      groupState `json:"groupState"`
	SentAt          int64      `json:"sentAt"`
}

type pausePayload struct {
	stamped
	CurrentTime float64    `json:"currentTime"`
	FromUserID  string     `json:"fromUserId"`
	GroupState  groupState `json:"groupState"`
	SentAt      int64      `json:"sentAt"`
}

type seekPayload struct {
	stamped
	Time            float64    `json:"time"`
	FromUserID      string     `json:"fromUserId"`
	GroupState      groupState `json:"groupState"`
	SentAt          int64      `json:"sentAt"`
	ScheduledPlayAt int64      `json:"scheduledPlayAt,omitempty"`
}

type peerReadyPayload struct {
	stamped
	UserID      string     `json:"userId"`
	DisplayName string     `json:"displayName"`
	ReadyCount  int        `json:"readyCount"`
	TotalPeers  int        `json:"totalPeers"`
	GroupState  groupState `json:"groupState"`
	TimedOut    *bool      `json:"timedOut,omitempty"`
	// Ignoring reports that a member stepped out of the wait rather than arriving ready.
	Ignoring bool  `json:"ignoring,omitempty"`
	SentAt   int64 `json:"sentAt"`
}

type peerPresencePayload struct {
	stamped
	UserID           string     `json:"userId"`
	DisplayName      string     `json:"displayName"`
	ParticipantCount int        `json:"participantCount"`
	GroupState       groupState `json:"groupState"`
	SentAt           int64      `json:"sentAt"`
}

// peerIgnoreWaitPayload reports a member opting in or out of being waited for at gates.
type peerIgnoreWaitPayload struct {
	stamped
	UserID      string `json:"userId"`
	DisplayName string `json:"displayName"`
	Ignore      bool   `json:"ignore"`
	SentAt      int64  `json:"sentAt"`
}

type peerNamePayload struct {
	stamped
	UserID      string `json:"userId"`
	DisplayName string `json:"displayName"`
	SentAt      int64  `json:"sentAt"`
}

// peerBufferingPayload is the mid-play counterpart of a gate: a member that ran short on
// buffer without the room having to stop. The deployed web client listens for this name.
type peerBufferingPayload struct {
	stamped
	UserID          string  `json:"userId"`
	DisplayName     string  `json:"displayName"`
	BufferedSeconds float64 `json:"bufferedSeconds"`
	Percent         float64 `json:"percent"`
	SentAt          int64   `json:"sentAt"`
}

type statesPayload struct {
	stamped
	Peers         []*Participant `json:"peers"`
	CanonicalTime float64        `json:"canonicalTime"`
	IsPlaying     bool           `json:"isPlaying"`
	GroupState    groupState     `json:"groupState"`
	SentAt        int64          `json:"sentAt"`
}

type softCorrectPayload struct {
	stamped
	CurrentTime float64 `json:"currentTime"`
	// FromUserID and DriftMS attribute the correction, so it reads as the group working, not a
	// glitch.
	FromUserID string  `json:"fromUserId,omitempty"`
	DriftMS    float64 `json:"driftMs,omitempty"`
	SentAt     int64   `json:"sentAt"`
}

type speedSeekPayload struct {
	stamped
	Rate       float64 `json:"rate"`
	DurationMs int64   `json:"duration"`
	TargetTime float64 `json:"targetTime"`
	FromUserID string  `json:"fromUserId,omitempty"`
	DriftMS    float64 `json:"driftMs,omitempty"`
	SentAt     int64   `json:"sentAt"`
}

type bufferingUpdatePayload struct {
	stamped
	UserID            string  `json:"userId"`
	DisplayName       string  `json:"displayName"`
	BufferedSeconds   float64 `json:"bufferedSeconds"`
	Percent           float64 `json:"percent"`
	BufferGoalSeconds float64 `json:"bufferGoalSeconds"`
	SentAt            int64   `json:"sentAt"`
}

type deniedPayload struct {
	stamped
	Reason string `json:"reason"`
	SentAt int64  `json:"sentAt"`
}

type episodeChangedPayload struct {
	stamped
	EpisodeID   string     `json:"episodeId"`
	CurrentTime float64    `json:"currentTime"`
	GroupState  groupState `json:"groupState"`
	SentAt      int64      `json:"sentAt"`
	// ScheduledPlayAt is the instant the new episode starts: everyone loads a different file, so
	// they have to arrive together.
	ScheduledPlayAt int64 `json:"scheduledPlayAt,omitempty"`
}

type hostChangedPayload struct {
	stamped
	NewHostUserID      string     `json:"newHostUserId"`
	NewHostDisplayName string     `json:"newHostDisplayName"`
	GroupState         groupState `json:"groupState"`
	SentAt             int64      `json:"sentAt"`
	// CorrectionsResumeAt tells clients how long to hold off steering: the incoming host has not
	// reported a position yet, so its clock is not the room's yet either.
	CorrectionsResumeAt int64 `json:"correctionsResumeAt,omitempty"`
}

type timeSyncPongPayload struct {
	ClientSendTime json.RawMessage `json:"clientSendTime"`
	ServerTime     int64           `json:"serverTime"`
	// ServerReceiveTime is the hub's clock the instant the ping arrived, read before any work: a
	// send time and a respond time alone collapse round trip into clock error.
	ServerReceiveTime int64 `json:"serverReceiveTime"`
}

type roomSyncPayload struct {
	stamped
	CurrentTime     float64    `json:"currentTime"`
	IsPlaying       bool       `json:"isPlaying"`
	WaitingForReady bool       `json:"waitingForReady"`
	GroupState      groupState `json:"groupState"`
	SentAt          int64      `json:"sentAt"`
	// StartLeadMs and BufferGoalSeconds let a client that rejoined mid-gate rebuild its view of
	// the room from one packet.
	StartLeadMs       float64            `json:"startLeadMs,omitempty"`
	BufferGoalSeconds float64            `json:"bufferGoalSeconds,omitempty"`
	Peers             []*Participant     `json:"peers,omitempty"`
	Config            *syncConfigPayload `json:"syncConfig,omitempty"`
}

type createRoomRequest struct {
	EpisodeID string `json:"episodeId"`
}

type joinRoomRequest struct {
	RoomCode string `json:"roomCode"`
}

type timeRequest struct {
	CurrentTime float64  `json:"currentTime"`
	PingMS      *float64 `json:"pingMs"`
}

type seekRequest struct {
	Time   float64  `json:"time"`
	PingMS *float64 `json:"pingMs"`
}

type heartbeatRequest struct {
	CurrentTime   float64  `json:"currentTime"`
	PlaybackRate  *float64 `json:"playbackRate"`
	BufferedAhead *float64 `json:"bufferedAhead"`
	// PingMS rides the heartbeat rather than a dedicated event because the scheduled start needs
	// it on a packet that already flows every couple of seconds.
	PingMS *float64 `json:"pingMs"`
	// DurationSeconds lets the gate scale its buffer goal to the time left rather than demand a
	// fixed goal near the end credits.
	DurationSeconds *float64 `json:"durationSeconds"`
}

// pingRequest is a stand-alone latency report, used when heartbeats are sparse (paused) and the
// room still has to schedule a start.
type pingRequest struct {
	PingMS float64 `json:"pingMs"`
}

// ignoreWaitRequest lets a member out of the buffer gate so it does not hold the room.
type ignoreWaitRequest struct {
	Ignore *bool `json:"ignore"`
}

// timeSyncOffsetRequest records a client's own clock estimate, echoed into the roster.
type timeSyncOffsetRequest struct {
	OffsetMS *float64 `json:"offsetMs"`
	PingMS   *float64 `json:"pingMs"`
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

// joinAckPayload answers joinRoom; isPlaying is always false because a join opens a gate first.
type joinAckPayload struct {
	stamped
	Success           bool                 `json:"success"`
	Error             string               `json:"error,omitempty"`
	EpisodeID         string               `json:"episodeId,omitempty"`
	CurrentTime       float64              `json:"currentTime"`
	IsPlaying         bool                 `json:"isPlaying"`
	GroupState        groupState           `json:"groupState"`
	HostUserID        string               `json:"hostUserId,omitempty"`
	ParticipantCount  int                  `json:"participantCount"`
	Participants      []participantSummary `json:"participants,omitempty"`
	Peers             []*Participant       `json:"peers,omitempty"`
	SentAt            int64                `json:"sentAt"`
	BufferGoalSeconds float64              `json:"bufferGoalSeconds,omitempty"`
	// Config is the tuning block, so a client's thresholds are redeployed rather than shipped.
	Config *syncConfigPayload `json:"syncConfig,omitempty"`
}

type participantSummary struct {
	UserID      string `json:"userId"`
	DisplayName string `json:"displayName"`
}

type createRoomAckPayload struct {
	stamped
	Success    bool               `json:"success"`
	Error      string             `json:"error,omitempty"`
	RoomCode   string             `json:"roomCode,omitempty"`
	EpisodeID  string             `json:"episodeId,omitempty"`
	GroupState groupState         `json:"groupState"`
	SentAt     int64              `json:"sentAt"`
	Config     *syncConfigPayload `json:"syncConfig,omitempty"`
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// peerStatesLocked renders the roster as the wire sees it. Callers hold r.mu.
func (r *Room) peerStatesLocked() []*Participant {
	out := make([]*Participant, 0, len(r.participants))
	// Sorted by socket id so the roster does not reshuffle and flicker between broadcasts.
	ids := make([]string, 0, len(r.participants))
	for id := range r.participants {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		participant := r.participants[id]
		view := *participant
		view.Ready = r.readyPeers[id] || participant.IgnoreWait
		view.Stalling = r.stallingPeers[id]
		// Drift is published, never acted on here; it is what a human uses to read corrections.
		view.DriftMS = (participant.CurrentTime - r.currentTime) * 1000
		out = append(out, &view)
	}
	return out
}

// stateLocked renders the room's phase explicitly. Callers hold r.mu.
func (r *Room) stateLocked() groupState {
	return groupStateOf(r.isPlaying, r.waitingForReady, r.everActed)
}

// highestPingLocked is the slowest round trip currently reported in the room, already clamped.
// Callers hold r.mu.
func (h *Hub) highestPingLocked(room *Room) int64 {
	maxMS := h.cfg.SyncPlay.MaxReportedPing.Milliseconds()
	highest := int64(0)
	for _, participant := range room.participants {
		if ping := clampPing(participant.PingMs, maxMS); ping > highest {
			highest = ping
		}
	}
	return highest
}

// leadMillisLocked is how far ahead of "now" a group action is scheduled. Callers hold r.mu.
func (h *Hub) leadMillisLocked(room *Room) int64 {
	return leadMillis(h.highestPingLocked(room),
		h.cfg.SyncPlay.StartLeadFloor.Milliseconds(),
		h.cfg.SyncPlay.StartLeadCeiling.Milliseconds())
}

// reportPingLocked records a member's self-reported round trip, which arrives on several events
// because the measurable moments differ. Callers hold r.mu.
func (h *Hub) reportPingLocked(room *Room, p *peer, reportMS *float64) {
	if reportMS == nil {
		return
	}
	participant := room.participants[p.sid]
	if participant == nil {
		return
	}
	ping := int64(*reportMS + 0.5)
	participant.PingMs = clampPing(ping, h.cfg.SyncPlay.MaxReportedPing.Milliseconds())
}

// scheduledStartLocked is the wall-clock instant every member acts at, computed once so agreement
// needs a shared number, not a shared clock. Callers hold r.mu.
func (h *Hub) scheduledStartLocked(room *Room) int64 {
	return nowMillis() + h.leadMillisLocked(room)
}

// goalSecondsLocked is the buffer a member must hold to clear the gate, scaled to how much
// episode is left according to whoever is watching the most of it. Callers hold r.mu.
func (h *Hub) goalSecondsLocked(room *Room) float64 {
	remaining := room.durationSeconds - room.currentTime
	if remaining <= 0 {
		// The host's position is the only clock the room has, so a peer ahead of it is the
		// better estimate of how much is left.
		for _, participant := range room.participants {
			if ahead := room.durationSeconds - participant.CurrentTime; ahead > remaining {
				remaining = ahead
			}
		}
	}
	return scaledGoalSeconds(h.cfg, remaining)
}

// stamp gives a payload the room's next sequence number and the hub's clock, taken at emit time
// so a packet delayed in a channel is not certified as the newer one.
func (room *Room) nextStamp() (int64, int64) {
	room.mu.Lock()
	defer room.mu.Unlock()
	room.seq++
	return room.seq, nowMillis()
}

func (h *Hub) stamp(room *Room, payload stampable) {
	seq, sent := room.nextStamp()
	payload.stamp(seq, sent)
}

func (room *Room) denied(reason string) *deniedPayload {
	seq, sent := room.nextStamp()
	payload := &deniedPayload{Reason: reason, SentAt: sent}
	payload.stamp(seq, sent)
	return payload
}

func (r *Room) broadcast(h *Hub, event string, payload any, exceptSocketID string) {
	if stampable, ok := payload.(stampable); ok {
		h.stamp(r, stampable)
	}
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
		Code: r.code, ShowID: r.showID, EpisodeID: r.episodeID, HostUserID: r.hostUserID, HostDisplayName: r.hostName,
		Status: "active", GroupState: r.stateLocked(), CurrentTime: r.currentTime, IsPlaying: r.isPlaying,
		Participants: len(r.participants),
	}
}

func (r *Room) close(h *Hub) {
	r.mu.Lock()
	h.stopTimers(r)
	r.isPlaying = false
	r.waitingForReady = false
	payload := syncPayload{CurrentTime: r.currentTime, GroupState: groupStateIdle, SentAt: nowMillis()}
	r.mu.Unlock()

	r.broadcast(h, "syncPaused", &payload, "")
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
	// An unknown episode would create a room nobody can join correctly. The same lookup gives the
	// room its parent show, which the HTTP snapshot publishes to a joiner before it opens a player.
	episode, err := h.repo.EpisodeByID(context.Background(), episodeID)
	if err != nil || episode == nil {
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

	room := newRoom(code, episodeID, episode.ShowID, p.sid, p.userID, p.name, episodeSeconds(episode))
	h.mu.Lock()
	h.rooms[code] = room
	h.mu.Unlock()
	p.roomCode = code

	if err := h.repo.CreateParty(context.Background(), code, p.userID, p.name, &episodeID); err != nil {
		h.logger.Warn("watch party could not be recorded", slog.String("room", code), slog.String("error", err.Error()))
	}
	h.reply(p, in, createRoomAckPayload{
		Success: true, RoomCode: code, EpisodeID: episodeID,
		GroupState: groupStateIdle, SentAt: nowMillis(), Config: h.syncConfigPointer(),
	})
	h.logger.Info("watch party created", slog.String("room", code), slog.String("host", p.userID))
}

// episodeSeconds reads a fetched episode's length for the scaled buffer goal. A miss is not an
// error: an unknown length simply leaves the goal at its configured cap.
func episodeSeconds(episode *repository.Episode) float64 {
	if episode == nil || episode.Duration == nil || *episode.Duration <= 0 {
		return 0
	}
	return float64(*episode.Duration)
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
	if !h.joinAllowed(p.userID) {
		h.reply(p, in, joinAckPayload{Success: false, Error: "Too many join attempts. Try again in a minute."})
		return
	}
	if existing != nil && existing.code != code {
		existing.handlePeerLeft(p, h)
	}

	h.mu.RLock()
	room := h.rooms[code]
	h.mu.RUnlock()
	if room == nil {
		h.noteJoinFailure(p.userID)
		h.reply(p, in, joinAckPayload{Success: false, Error: "Room not found."})
		return
	}

	room.mu.Lock()
	if len(room.participants) >= h.cfg.SyncPlay.MaxParticipants {
		room.mu.Unlock()
		h.noteJoinFailure(p.userID)
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
	h.clearJoinFailures(p.userID)
	summaries := room.uniqueParticipantsLocked()
	states := room.peerStatesLocked()
	goal := h.goalSecondsLocked(room)
	currentTime := room.currentTime
	episodeID := room.episodeID
	hostUserID := room.hostUserID
	room.mu.Unlock()

	if err := h.repo.RecordPartyParticipant(context.Background(), code, p.userID); err != nil {
		h.logger.Warn("watch party participant could not be recorded",
			slog.String("room", code), slog.String("error", err.Error()))
	}

	room.broadcast(h, "peerJoined", &peerPresencePayload{
		UserID: p.userID, DisplayName: p.name, ParticipantCount: count,
		GroupState: groupStateWaiting, SentAt: nowMillis(),
	}, p.sid)

	ack := joinAckPayload{
		Success: true, EpisodeID: episodeID, CurrentTime: currentTime, IsPlaying: false,
		// The ack states the next phase directly; a joiner need not infer it from event order.
		GroupState: groupStateWaiting, HostUserID: hostUserID,
		ParticipantCount: count, Participants: summaries, Peers: states,
		SentAt: nowMillis(), BufferGoalSeconds: goal,
		Config: h.syncConfigPointer(),
	}
	h.stamp(room, &ack)
	h.reply(p, in, ack)

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
	room.everActed = true
	h.reportPingLocked(room, p, req.PingMS)
	solo := len(room.participants) == 1
	if solo {
		room.isPlaying = true
	}
	// A solo viewer is scheduled too: one client path, no acting-on-receipt special case.
	startAt := h.scheduledStartLocked(room)
	currentTime := room.currentTime
	state := room.stateLocked()
	room.mu.Unlock()

	if solo {
		// Nothing to coordinate solo, but the start still arrives as a future instant.
		payload := &syncPayload{
			CurrentTime: currentTime, IsPlaying: true, GroupState: state,
			SentAt: nowMillis(), ScheduledPlayAt: startAt,
		}
		h.stamp(room, payload)
		p.Emit("syncPlay", payload)
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
	h.reportPingLocked(room, p, req.PingMS)
	state := room.stateLocked()
	// A pause cancels an open gate; otherwise the room would resume playing after the
	// stragglers finished buffering.
	if room.waitingForReady {
		h.cancelGateLocked(room)
		state = room.stateLocked()
	}
	room.mu.Unlock()

	// Stopping is not scheduled: a member that pauses 200 ms late has not desynchronised
	// anyone, whereas a member that starts 200 ms early has.
	room.broadcast(h, "pause", &pausePayload{
		CurrentTime: req.CurrentTime, FromUserID: p.userID, GroupState: state, SentAt: nowMillis(),
	}, p.sid)
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
	h.reportPingLocked(room, p, req.PingMS)
	participants := len(room.participants)
	startAt := int64(0)
	if wasPlaying {
		// A seek mid-playback moves everyone's target; without a scheduled instant each member
		// lands at the new position as its own packet arrives.
		startAt = h.scheduledStartLocked(room)
	}
	state := room.stateLocked()
	room.mu.Unlock()

	if participants == 1 {
		payload := &seekPayload{
			Time: req.Time, FromUserID: p.userID, GroupState: state,
			SentAt: nowMillis(), ScheduledPlayAt: startAt,
		}
		h.stamp(room, payload)
		p.Emit("seek", payload)
		return
	}
	// In a group a seek is a re-gate whose target the gate announces, so nobody plays the
	// old position while buffering catches up.
	room.broadcast(h, "seek", &seekPayload{
		Time: req.Time, FromUserID: p.userID, GroupState: state,
		SentAt: nowMillis(), ScheduledPlayAt: startAt,
	}, p.sid)
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

	room.broadcast(h, "peerReady", &peerReadyPayload{
		UserID: p.userID, DisplayName: name, ReadyCount: readyCount, TotalPeers: total,
		SentAt: nowMillis(),
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
	goal := h.goalSecondsLocked(room)
	gating := room.waitingForReady
	shouldReady := req.BufferedSeconds >= goal && gating && !room.readyPeers[p.sid]
	if shouldReady {
		room.readyPeers[p.sid] = true
		participant.ReadyState = readyStateReady
		participant.bufferingAnnounced = false
		if timer := room.readyTimers[p.sid]; timer != nil {
			timer.Stop()
			delete(room.readyTimers, p.sid)
		}
	}
	// A member that falls short of the goal mid-playback is announced once, on the way down;
	// the web client already listens for peerBuffering by name.
	announceBuffering := !gating && room.isPlaying && req.BufferedSeconds < goal &&
		!participant.bufferingAnnounced
	if announceBuffering {
		participant.bufferingAnnounced = true
	} else if req.BufferedSeconds >= goal {
		// Recovering has to re-arm the announcement: falling behind twice is news twice, and a
		// latch only the gate clears would stay shut for the rest of the episode.
		participant.bufferingAnnounced = false
	}
	readyCount := len(room.readyPeers)
	total := len(room.participants)
	displayName := participant.DisplayName
	userID := participant.UserID
	state := room.stateLocked()
	room.mu.Unlock()

	if announceBuffering {
		room.broadcast(h, "peerBuffering", &peerBufferingPayload{
			UserID: userID, DisplayName: displayName,
			BufferedSeconds: req.BufferedSeconds, Percent: req.Percent, SentAt: nowMillis(),
		}, "")
	}

	room.broadcast(h, "bufferingUpdate", &bufferingUpdatePayload{
		UserID: userID, DisplayName: displayName,
		BufferedSeconds: req.BufferedSeconds, Percent: req.Percent,
		BufferGoalSeconds: goal, SentAt: nowMillis(),
	}, "")

	if shouldReady {
		room.broadcast(h, "peerReady", &peerReadyPayload{
			UserID: userID, DisplayName: displayName,
			ReadyCount: readyCount, TotalPeers: total, SentAt: nowMillis(), GroupState: state,
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
		if *req.BufferedAhead >= h.goalSecondsLocked(room) {
			participant.bufferingAnnounced = false
		}
	}
	h.reportPingLocked(room, p, req.PingMS)
	if req.DurationSeconds != nil && *req.DurationSeconds > 0 && p.sid == room.hostSocketID {
		// The host owns the room's view of the media, so a peer cannot overwrite another's episode.
		room.durationSeconds = *req.DurationSeconds
	}

	// Only the host moves the canonical clock while playing; a paused room keeps the position
	// its last pause set.
	if p.sid == room.hostSocketID && room.isPlaying {
		room.currentTime = req.CurrentTime
	}

	// Stall detection: a position that does not advance for the detect window is stalling.
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
	states := &statesPayload{
		Peers: room.peerStatesLocked(), CanonicalTime: room.currentTime,
		IsPlaying: playing, GroupState: room.stateLocked(), SentAt: nowMillis(),
	}
	room.mu.Unlock()

	if recovered {
		h.handlePeerStallRecovered(p, room)
	}
	if stallSignal {
		h.handlePeerStall(p, room)
	}

	room.broadcast(h, "participantStates", states, "")

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
		p.Emit("syncDenied", room.denied("Only the current host can transfer control."))
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
		p.Emit("syncDenied", room.denied("Target user is not in the room."))
		return
	}
	room.hostSocketID = targetSocketID
	room.hostUserID = strings.TrimSpace(req.TargetUserID)
	room.hostName = targetName
	// The new host has not reported a position, so corrections are held off rather than aimed
	// at the outgoing host's last reading.
	resumeAt := nowMillis() + h.cfg.SyncPlay.StallDetectWindow.Milliseconds()
	state := room.stateLocked()
	newHost, newName := room.hostUserID, targetName
	room.mu.Unlock()

	room.broadcast(h, "hostChanged", &hostChangedPayload{
		NewHostUserID: newHost, NewHostDisplayName: newName, GroupState: state,
		SentAt: nowMillis(), CorrectionsResumeAt: resumeAt,
	}, "")
	h.logger.Info("watch party host transferred",
		slog.String("room", room.code), slog.String("new_host", newHost))
}

func (h *Hub) handleChangeEpisode(p *peer, in inbound, room *Room) {
	var req changeEpisodeRequest
	if err := json.Unmarshal(in.Args, &req); err != nil {
		return
	}
	if room == nil {
		return
	}
	episodeID := strings.TrimSpace(req.EpisodeID)
	if episodeID == "" {
		p.Emit("syncDenied", room.denied("Episode is not in the library."))
		return
	}
	// Fetched before the lock: an episode lookup must not hold a room's mutex. The row also carries
	// the new parent show, so the room's snapshot keeps naming the right one after a change.
	episode, err := h.repo.EpisodeByID(context.Background(), episodeID)
	if err != nil || episode == nil {
		p.Emit("syncDenied", room.denied("Episode is not in the library."))
		return
	}
	duration := episodeSeconds(episode)

	room.mu.Lock()
	if p.sid != room.hostSocketID {
		room.mu.Unlock()
		p.Emit("syncDenied", room.denied("Only the host can change the episode."))
		return
	}
	room.episodeID = episodeID
	room.showID = episode.ShowID
	room.durationSeconds = duration
	room.currentTime = 0
	room.isPlaying = false
	room.everActed = false
	h.cancelGateLocked(room)
	// Stale buffer reports from the previous episode must not satisfy the next gate.
	for _, participant := range room.participants {
		participant.BufferedAhead = 0
		participant.ReadyState = readyStateBuffering
		participant.CurrentTime = 0
		participant.StallingSinceMs = 0
		participant.bufferingAnnounced = false
	}
	payload := &episodeChangedPayload{
		EpisodeID: episodeID, CurrentTime: 0, GroupState: room.stateLocked(), SentAt: nowMillis(),
	}
	room.mu.Unlock()

	room.broadcast(h, "episodeChanged", payload, p.sid)
	// Everyone loads a different file, so an episode change re-gates like any scheduled start.
	room.initiateBufferGate(h, false)
	h.logger.Info("watch party episode changed", slog.String("room", room.code), slog.String("episode", episodeID))
}

// handleTimeSync answers a clock probe with four timestamps. The receive time is read before the
// payload is parsed, so no handler work is charged to the caller as clock error.
func (h *Hub) handleTimeSync(p *peer, in inbound) {
	received := nowMillis()
	var req timeSyncRequest
	if err := json.Unmarshal(in.Args, &req); err != nil {
		req.ClientSendTime = json.RawMessage("null")
	}
	// The pong echoes the client's own timestamp so it can separate round trip from offset.
	p.Emit("timesync_pong", timeSyncPongPayload{
		ClientSendTime: req.ClientSendTime, ServerTime: nowMillis(), ServerReceiveTime: received,
	})
}

// handlePingReport records a round trip measured outside a heartbeat, which is the common case
// for a paused room where the client syncs its clock but has no position to report.
func (h *Hub) handlePingReport(p *peer, in inbound, room *Room) {
	var req pingRequest
	if err := json.Unmarshal(in.Args, &req); err != nil || room == nil {
		return
	}
	report := req.PingMS
	room.mu.Lock()
	h.reportPingLocked(room, p, &report)
	room.mu.Unlock()
}

// handleTimeSyncOffset stores a client's own clock belief. The hub does not act on it, but
// publishing it makes one member's clock error legible from outside.
func (h *Hub) handleTimeSyncOffset(p *peer, in inbound, room *Room) {
	var req timeSyncOffsetRequest
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
	if req.OffsetMS != nil {
		participant.ClockOffsetMS = int64(*req.OffsetMS + 0.5)
	}
	h.reportPingLocked(room, p, req.PingMS)
	room.mu.Unlock()
}

// handleIgnoreWait steps a member out of, or back into, the buffer gate. Opting out takes effect
// on the open gate, not only the next one.
func (h *Hub) handleIgnoreWait(p *peer, in inbound, room *Room) {
	var req ignoreWaitRequest
	if err := json.Unmarshal(in.Args, &req); err != nil || req.Ignore == nil {
		return
	}
	if !h.ensureParticipant(p, room) {
		return
	}
	ignore := *req.Ignore

	room.mu.Lock()
	participant := room.participants[p.sid]
	participant.IgnoreWait = ignore
	released := false
	if ignore && room.waitingForReady && !room.readyPeers[p.sid] {
		room.readyPeers[p.sid] = true
		participant.ReadyState = readyStateReady
		if timer := room.readyTimers[p.sid]; timer != nil {
			timer.Stop()
			delete(room.readyTimers, p.sid)
		}
		released = true
	}
	if !ignore && room.waitingForReady && participant.BufferedAhead < h.goalSecondsLocked(room) {
		// Rejoining the wait mid-gate only makes sense if there is still something to wait for;
		// otherwise the room would reopen a gate it had already satisfied.
		delete(room.readyPeers, p.sid)
		participant.ReadyState = readyStateBuffering
		code, sid := room.code, p.sid
		timeout := h.cfg.SyncPlay.ReadyTimeout
		epoch := room.gateEpoch
		room.readyTimers[sid] = time.AfterFunc(timeout, func() { h.forceReady(sid, code, epoch) })
	}
	readyCount := len(room.readyPeers)
	total := len(room.participants)
	displayName := participant.DisplayName
	userID := participant.UserID
	room.mu.Unlock()

	room.broadcast(h, "peerIgnoreWait", &peerIgnoreWaitPayload{
		UserID: userID, DisplayName: displayName, Ignore: ignore, SentAt: nowMillis(),
	}, "")
	room.broadcast(h, "peerReady", &peerReadyPayload{
		UserID: userID, DisplayName: displayName,
		ReadyCount: readyCount, TotalPeers: total, Ignoring: ignore, SentAt: nowMillis(),
	}, "")
	if released {
		room.tryResumeAfterReady(h)
	}
	h.logger.Info("watch party wait preference changed",
		slog.String("room", room.code), slog.String("user", userID), slog.Bool("ignore", ignore))
}

func (h *Hub) handleRequestSync(p *peer, room *Room) {
	if room == nil {
		return
	}
	room.mu.Lock()
	payload := roomSyncPayload{
		CurrentTime: room.currentTime, IsPlaying: room.isPlaying,
		WaitingForReady: room.waitingForReady, GroupState: room.stateLocked(),
		SentAt: nowMillis(), StartLeadMs: float64(h.leadMillisLocked(room)),
		BufferGoalSeconds: h.goalSecondsLocked(room), Peers: room.peerStatesLocked(),
		Config: h.syncConfigPointer(),
	}
	room.mu.Unlock()
	h.stamp(room, &payload)
	p.Emit("sync", payload)
}

// ── Gate logic

// initiateBufferGate opens the ready gate for every participant and holds playback until all are
// ready, their timeouts fire, the escape hatch or the hard deadline closes it.
func (r *Room) initiateBufferGate(h *Hub, playAfterReady bool) {
	r.mu.Lock()
	h.stopTimers(r)
	r.readyPeers = map[string]bool{}
	r.waitingForReady = true
	r.playIntentAfterReady = playAfterReady
	r.isPlaying = false
	r.gateOpenedAt = time.Now()
	r.gateEpoch++
	epoch := r.gateEpoch
	goal := h.goalSecondsLocked(r)
	// A member that opted out is ready the moment the gate opens, so the others start now rather
	// than after its timeout.
	for id, participant := range r.participants {
		if participant.IgnoreWait {
			r.readyPeers[id] = true
			participant.ReadyState = readyStateReady
			continue
		}
		participant.ReadyState = readyStateBuffering
	}
	states := r.peerStatesLocked()
	deadline := nowMillis() + h.cfg.SyncPlay.WaitDeadline.Milliseconds()
	gate := &gatePayload{
		CurrentTime: r.currentTime, BufferGoalSeconds: goal, Peers: states,
		TotalPeers: len(r.participants), ReadyCount: len(r.readyPeers),
		GroupState: r.stateLocked(), SentAt: nowMillis(), WaitDeadlineMs: deadline,
	}
	legacy := &gatePayload{
		CurrentTime: r.currentTime, Peers: states,
		TotalPeers: len(r.participants), ReadyCount: len(r.readyPeers),
		GroupState: r.stateLocked(), SentAt: nowMillis(), WaitDeadlineMs: deadline,
	}
	timeout := h.cfg.SyncPlay.ReadyTimeout
	escapeWait := h.cfg.SyncPlay.EscapeWait
	waitDeadline := h.cfg.SyncPlay.WaitDeadline
	code := r.code
	peers := make([]string, 0, len(r.participants))
	for id := range r.participants {
		peers = append(peers, id)
	}
	if len(r.readyPeers) == len(r.participants) {
		// Nobody left to wait for: announce the gate so clients cancel what they scheduled, then
		// close it immediately rather than holding a room with nothing to buffer.
		r.mu.Unlock()
		r.broadcast(h, "waitForBufferGoal", gate, "")
		r.broadcast(h, "waitForReady", legacy, "")
		r.tryResumeAfterReady(h)
		return
	}
	escapeBuffer := h.cfg.SyncPlay.EscapeBuffer.Seconds()
	r.mu.Unlock()

	// Both events are sent newer clients gate on buffer, older ones on readiness alone.
	r.broadcast(h, "waitForBufferGoal", gate, "")
	r.broadcast(h, "waitForReady", legacy, "")

	for _, socketID := range peers {
		id := socketID
		r.mu.Lock()
		if r.waitingForReady && !r.readyPeers[id] {
			r.readyTimers[id] = time.AfterFunc(timeout, func() {
				h.forceReady(id, code, epoch)
			})
		}
		r.mu.Unlock()
	}

	r.mu.Lock()
	r.escapeTimer = time.AfterFunc(escapeWait, func() { h.runEscapeHatch(code, escapeBuffer, epoch) })
	// The wait deadline is the last resort: it closes the gate whatever the buffer looks like.
	r.waitDeadlineTimer = time.AfterFunc(waitDeadline, func() { h.waitDeadlineExpired(code, epoch) })
	r.mu.Unlock()
}

// forceReady satisfies one straggler when its timer fires, so a stalled peer cannot hold the
// party; the epoch check stops a late timer granting readiness to a reopened gate.
func (h *Hub) forceReady(socketID, code string, epoch int64) {
	h.mu.RLock()
	room := h.rooms[code]
	h.mu.RUnlock()
	if room == nil {
		return
	}
	room.mu.Lock()
	if !room.waitingForReady || room.gateEpoch != epoch || room.readyPeers[socketID] {
		room.mu.Unlock()
		return
	}
	room.readyPeers[socketID] = true
	userID, displayName := socketID, socketID
	ignore := false
	if participant := room.participants[socketID]; participant != nil {
		userID, displayName = participant.UserID, participant.DisplayName
		participant.ReadyState = readyStateReady
		ignore = participant.IgnoreWait
	}
	if timer := room.readyTimers[socketID]; timer != nil {
		timer.Stop()
		delete(room.readyTimers, socketID)
	}
	readyCount := len(room.readyPeers)
	total := len(room.participants)
	state := room.stateLocked()
	room.mu.Unlock()

	timedOut := true
	room.broadcast(h, "peerReady", &peerReadyPayload{
		UserID: userID, DisplayName: displayName, ReadyCount: readyCount, TotalPeers: total,
		TimedOut: &timedOut, GroupState: state, Ignoring: ignore, SentAt: nowMillis(),
	}, "")
	room.tryResumeAfterReady(h)
}

// runEscapeHatch ends a gate early when at least one peer has useful buffer or somebody is
// already ready, rather than letting a single slow client stall the party indefinitely.
func (h *Hub) runEscapeHatch(code string, bufferGoal float64, epoch int64) {
	h.mu.RLock()
	room := h.rooms[code]
	h.mu.RUnlock()
	if room == nil {
		return
	}
	room.mu.Lock()
	if !room.waitingForReady || room.gateEpoch != epoch {
		room.mu.Unlock()
		return
	}
	goal := h.goalSecondsLocked(room)
	if goal < bufferGoal {
		bufferGoal = goal
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
	h.readyEveryoneLocked(room)
	room.mu.Unlock()
	room.tryResumeAfterReady(h)
}

// waitDeadlineExpired closes a gate that has taken too long, unconditionally; unlike the escape
// hatch it does not wait for evidence that waiting is productive.
func (h *Hub) waitDeadlineExpired(code string, epoch int64) {
	h.mu.RLock()
	room := h.rooms[code]
	h.mu.RUnlock()
	if room == nil {
		return
	}
	room.mu.Lock()
	if !room.waitingForReady || room.gateEpoch != epoch {
		room.mu.Unlock()
		return
	}
	waited := time.Since(room.gateOpenedAt).Milliseconds()
	h.readyEveryoneLocked(room)
	room.mu.Unlock()

	h.logger.Info("watch party gate hit its wait deadline",
		slog.String("room", code), slog.Int64("waited_ms", waited))
	room.tryResumeAfterReady(h)
}

// readyEveryoneLocked releases the whole gate at once. Callers hold r.mu.
func (h *Hub) readyEveryoneLocked(room *Room) {
	for id, participant := range room.participants {
		room.readyPeers[id] = true
		if participant != nil {
			participant.ReadyState = readyStateReady
		}
	}
}

// tryResumeAfterReady closes the gate once every participant is ready, and starts the room on a
// scheduled instant rather than on the arrival of a packet.
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
	startAt := int64(0)
	if shouldPlay {
		r.isPlaying = true
		r.everActed = true
		// "Ready" is not "together": the lead converts one into the other so the last receiver
		// does not start a round trip after the first.
		startAt = h.scheduledStartLocked(r)
	}
	state := r.stateLocked()
	readyCount, total := len(r.readyPeers), len(r.participants)
	allReady := &allReadyPayload{
		CurrentTime: r.currentTime, ScheduledPlayAt: startAt, ReadyCount: readyCount,
		TotalPeers: total, PlayIntent: shouldPlay, GroupState: state, SentAt: nowMillis(),
	}
	payload := &syncPayload{
		CurrentTime: r.currentTime, IsPlaying: shouldPlay, GroupState: state,
		SentAt: nowMillis(), ScheduledPlayAt: startAt,
	}
	r.mu.Unlock()

	r.broadcast(h, "allReady", allReady, "")
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
	// Skipping an epoch invalidates every timer armed for the gate that just closed, including
	// ones whose callback is already on another goroutine.
	room.gateEpoch++
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
	if room.waitDeadlineTimer != nil {
		room.waitDeadlineTimer.Stop()
		room.waitDeadlineTimer = nil
	}
	room.stallingPeers = map[string]bool{}
}

// ── Drift and stall ──────────────────────────────────────────────────────────

// checkDrift corrects a peer that has fallen out of step, using decideDrift's classification.
func (h *Hub) checkDrift(p *peer, room *Room, peerTime float64) {
	room.mu.Lock()
	if !room.isPlaying || room.waitingForReady {
		room.mu.Unlock()
		return
	}
	canonical := room.currentTime
	hostUserID := room.hostUserID
	ignore := false
	if participant := room.participants[p.sid]; participant != nil {
		ignore = participant.IgnoreWait
	}
	room.mu.Unlock()

	// A member that opted out is told its drift but not corrected, which is what the opt-out means.
	if ignore {
		return
	}

	action, rate, durationMS := decideDrift(peerTime, canonical,
		h.cfg.SyncPlay.SoftSeekThreshold.Seconds()*1000,
		h.cfg.SyncPlay.SpeedSyncMaxDrift.Seconds()*1000,
		h.cfg.SyncPlay.SlowdownRate, h.cfg.SyncPlay.SpeedupRate)
	driftMS := (peerTime - canonical) * 1000
	switch action {
	case driftNone:
		return
	case driftSoftCorrect:
		seq, sent := room.nextStamp()
		payload := &softCorrectPayload{
			CurrentTime: canonical, FromUserID: hostUserID, DriftMS: driftMS, SentAt: sent,
		}
		payload.stamp(seq, sent)
		p.Emit("softCorrect", payload)
	case driftSpeedSeek:
		seq, sent := room.nextStamp()
		payload := &speedSeekPayload{
			Rate: rate, DurationMs: durationMS, TargetTime: canonical,
			FromUserID: hostUserID, DriftMS: driftMS, SentAt: sent,
		}
		payload.stamp(seq, sent)
		p.Emit("speedSeek", payload)
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

// decideDrift classifies one peer against the canonical time. Times are seconds, so drift is
// converted before the ms thresholds; duration is twice the drift to halve the error at the rate.
func decideDrift(peerTime, canonicalTime, softThresholdMS, speedThresholdMS, slowdownRate, speedupRate float64) (action driftAction, rate float64, durationMS int64) {
	driftMS := absFloat(peerTime-canonicalTime) * 1000
	switch {
	case driftMS < softThresholdMS:
		return driftNone, 0, 0
	case driftMS < softCorrectCeilingMS:
		return driftSoftCorrect, 0, 0
	case driftMS < speedThresholdMS:
		rate = slowdownRate
		if peerTime < canonicalTime {
			rate = speedupRate
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
	buffered, goal := 0.0, h.goalSecondsLocked(room)
	if participant := room.participants[p.sid]; participant != nil {
		displayName = participant.DisplayName
		buffered = participant.BufferedAhead
		participant.bufferingAnnounced = true
	}
	userID := p.userID
	code := room.code
	grace := h.cfg.SyncPlay.StallGrace
	room.mu.Unlock()

	// A stall is a short buffer from the other side, so both names are sent: the web client
	// listens for peerBuffering, the site's banner for peerStalling.
	room.broadcast(h, "peerStalling", &peerNamePayload{
		UserID: userID, DisplayName: displayName, SentAt: nowMillis(),
	}, "")
	room.broadcast(h, "peerBuffering", &peerBufferingPayload{
		UserID: userID, DisplayName: displayName, BufferedSeconds: buffered,
		Percent: percentOf(buffered, goal), SentAt: nowMillis(),
	}, "")

	room.mu.Lock()
	room.stallGraceTimers[p.sid] = time.AfterFunc(grace, func() {
		h.stallGraceExpired(p.sid, code)
	})
	room.mu.Unlock()
}

// percentOf expresses a buffer reading against the goal the room is currently enforcing, which
// is the only form a progress bar can use. A zero goal is a room with no policy, not a divide.
func percentOf(value, goal float64) float64 {
	if goal <= 0 {
		return 0
	}
	percent := value / goal * 100
	if percent > 100 {
		percent = 100
	}
	return percent
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
		participant.bufferingAnnounced = false
	}
	displayName := p.name
	room.mu.Unlock()

	room.broadcast(h, "peerStallRecovered", &peerNamePayload{
		UserID: p.userID, DisplayName: displayName, SentAt: nowMillis(),
	}, "")
}

// ── Membership ───────────────────────────────────────────────────────────────

// ensureParticipant answers whether a peer may act in a room. A departed or never-joined peer is
// refused silently; clients drive their own optimistic UI and re-sync on the next broadcast.
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
	successorID, successorName := "", ""
	if emptiedHost {
		// Promotion follows join order: the earliest surviving peer is the one most likely
		// to have watched the whole episode so far.
		successor = sortedParticipants(r)[0]
		r.hostSocketID = successor.SocketID
		r.hostUserID = successor.UserID
		r.hostName = successor.DisplayName
		successorID, successorName = successor.UserID, successor.DisplayName
	}
	// A departure lowers the bar for the gate: without this the room would wait forever for
	// a peer that is gone.
	if waiting {
		r.mu.Unlock()
		r.tryResumeAfterReady(h)
		r.mu.Lock()
	}
	state := r.stateLocked()
	roster := r.peerStatesLocked()
	canonical := r.currentTime
	playing := r.isPlaying
	resumeCorrectionsAt := nowMillis() + h.cfg.SyncPlay.StallDetectWindow.Milliseconds()
	code := r.code
	r.mu.Unlock()
	p.roomCode = ""

	if emptiedHost {
		r.broadcast(h, "hostChanged", &hostChangedPayload{
			NewHostUserID: successorID, NewHostDisplayName: successorName, GroupState: state,
			SentAt: nowMillis(), CorrectionsResumeAt: resumeCorrectionsAt,
		}, "")
		h.logger.Info("watch party host promoted", slog.String("room", code), slog.String("new_host", successorID))
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

	r.broadcast(h, "peerLeft", &peerPresencePayload{
		UserID: userID, DisplayName: displayName, ParticipantCount: remaining,
		GroupState: state, SentAt: nowMillis(),
	}, "")
	r.broadcast(h, "participantStates", &statesPayload{
		Peers: roster, CanonicalTime: canonical, IsPlaying: playing,
		GroupState: state, SentAt: nowMillis(),
	}, "")
}

// sortedParticipants returns peers ordered by join time, breaking ties on socket id, so
// host promotion and the roster are deterministic. Callers hold room.mu.
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
			if room := h.roomFor(p); room != nil {
				p.Emit("syncDenied", room.denied(body.Error))
			} else {
				p.Emit("syncDenied", &deniedPayload{Reason: body.Error, SentAt: nowMillis()})
			}
			return
		}
		// The whole ack travels, not just the code: a client that never asks for an
		// acknowledgement still has to be told which tuning it is playing under.
		p.Emit("roomCreated", body)
	case joinAckPayload:
		if !body.Success {
			if room := h.roomFor(p); room != nil {
				p.Emit("syncDenied", room.denied(body.Error))
			} else {
				p.Emit("syncDenied", &deniedPayload{Reason: body.Error, SentAt: nowMillis()})
			}
			return
		}
		p.Emit("roomJoined", body)
	default:
		if room := h.roomFor(p); room != nil {
			p.Emit("syncDenied", room.denied(fmt.Sprintf("unhandled reply %T", body)))
		} else {
			p.Emit("syncDenied", &deniedPayload{
				Reason: fmt.Sprintf("unhandled reply %T", body), SentAt: nowMillis(),
			})
		}
	}
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

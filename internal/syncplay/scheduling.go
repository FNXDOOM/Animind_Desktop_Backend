package syncplay

import (
	"animind-backend-go/internal/config"
)

// Scheduling: what the hub attaches to a broadcast so a group acts on one instant rather than
// on the accident of who received a packet first. All of it is pure, so none of it needs a socket.

// Bumped when a field clients must understand is added, so a client can tell an old server from
// its own broken parse.
const protocolVersion = 2

// stamped is embedded in every broadcast payload and filled in at emit time. Ordering recorded at
// build time would certify a stale packet as the newer one.
type stamped struct {
	Seq          int64 `json:"seq"`
	ServerTimeMs int64 `json:"serverTime"`
}

func (s *stamped) stamp(seq, serverTime int64) { s.Seq, s.ServerTimeMs = seq, serverTime }

// stampable is what broadcast looks for. Embedding stamped satisfies it for free.
type stampable interface {
	stamp(seq, serverTime int64)
}

// clampPing bounds a self-reported round trip, so one client cannot push a group's start out.
func clampPing(reportMS, maxMS int64) int64 {
	if reportMS < 0 {
		return 0
	}
	if reportMS > maxMS {
		return maxMS
	}
	return reportMS
}

// leadMillis is the delay before a group start: twice the slowest round trip (Jellyfin's rule),
// floored because a local room's near-zero ping would otherwise schedule nothing and fall back to
// acting on receipt.
func leadMillis(highestPingMS, floorMS, ceilingMS int64) int64 {
	lead := highestPingMS * 2
	if lead < floorMS {
		lead = floorMS
	}
	if ceilingMS > 0 && lead > ceilingMS {
		lead = ceilingMS
	}
	return lead
}

// minGoalSeconds is the floor for a shrunken goal: a member still needs media to survive a seek.
const minGoalSeconds = 10.0

// scaledGoalSeconds is the buffer a member must hold to clear the gate. The configured goal is a
// cap, not a target: 120 s is the entire remaining file near the end, which would turn every
// final scene into a permanent gate.
func scaledGoalSeconds(cfg *config.Config, remainingSeconds float64) float64 {
	goal := cfg.SyncPlay.BufferGoal.Seconds()
	if remainingSeconds <= 0 {
		return goal
	}
	scaled := remainingSeconds * cfg.SyncPlay.BufferGoalFraction
	if scaled >= goal {
		return goal
	}
	if scaled < minGoalSeconds {
		return minGoalSeconds
	}
	return scaled
}

// groupState is the room's phase, stated rather than inferred from which event arrived last.
type groupState string

const (
	groupStateIdle    groupState = "idle"   // host, no playback intent yet
	groupStatePaused  groupState = "paused" // an agreed position that is not advancing
	groupStatePlaying groupState = "playing"
	groupStateWaiting groupState = "waiting" // the buffer gate
)

func groupStateOf(playing, waiting, everActed bool) groupState {
	switch {
	case waiting:
		return groupStateWaiting
	case playing:
		return groupStatePlaying
	case everActed:
		return groupStatePaused
	default:
		return groupStateIdle
	}
}

// ── Client configuration ─────────────────────────────────────────────────────

// syncConfigPayload is the tuning block clients are configured from: a threshold here is
// redeployed, one baked into a client is released.
type syncConfigPayload struct {
	Version    int              `json:"version"`
	Features   []string         `json:"features"`
	Gate       gateConfig       `json:"gate"`
	Start      startConfig      `json:"start"`
	Correction correctionConfig `json:"correction"`
	Drift      driftConfig      `json:"drift"`
	TimeSync   timeSyncConfig   `json:"timeSync"`
	Limits     limitConfig      `json:"limits"`
}

type gateConfig struct {
	BufferGoalSeconds   float64 `json:"bufferGoalSeconds"`
	BufferGoalFraction  float64 `json:"bufferGoalFraction"`
	ReadyTimeoutMs      int64   `json:"readyTimeoutMs"`
	EscapeWaitMs        int64   `json:"escapeWaitMs"`
	EscapeBufferSeconds float64 `json:"escapeBufferSeconds"`
	WaitDeadlineMs      int64   `json:"waitDeadlineMs"`
	StallDetectMs       int64   `json:"stallDetectMs"`
	StallGraceMs        int64   `json:"stallGraceMs"`
}

type startConfig struct {
	LeadMinMs         int64 `json:"leadMinMs"`
	LeadMaxMs         int64 `json:"leadMaxMs"`
	MaxReportedPingMs int64 `json:"maxReportedPingMs"`
}

type correctionConfig struct {
	SlowdownRate       float64 `json:"slowdownRate"`
	SpeedupRate        float64 `json:"speedupRate"`
	EngageDriftMs      int64   `json:"engageDriftMs"`
	ReleaseDriftMs     int64   `json:"releaseDriftMs"`
	FastForwardDriftMs int64   `json:"fastForwardDriftMs"`
}

type driftConfig struct {
	SoftSeekThresholdMs  int64 `json:"softSeekThresholdMs"`
	SpeedSyncMaxDriftMs  int64 `json:"speedSyncMaxDriftMs"`
	MaxPlaybackOffsetMs  int64 `json:"maxPlaybackOffsetMs"`
	MaxCatchUpOffsetMs   int64 `json:"maxCatchUpOffsetMs"`
	SoftCorrectCeilingMs int64 `json:"softCorrectCeilingMs"`
	MaxSpeedSeekMs       int64 `json:"maxSpeedSeekMs"`
}

type timeSyncConfig struct {
	Window           int   `json:"window"`
	DeadbandMs       int64 `json:"deadbandMs"`
	MaxAgeMs         int64 `json:"maxAgeMs"`
	GreedyIntervalMs int64 `json:"greedyIntervalMs"`
	IdlePlayingMs    int64 `json:"idlePlayingMs"`
	IdlePausedMs     int64 `json:"idlePausedMs"`
}

type limitConfig struct {
	HeartbeatIntervalMs int64 `json:"heartbeatIntervalMs"`
	ProtocolTimeoutMs   int64 `json:"protocolTimeoutMs"`
	MaxParticipants     int   `json:"maxParticipants"`
}

// featureList lets a client enable a path only when this hub actually emits it.
var featureList = []string{
	"scheduledStart",
	"broadcastSeq",
	"timeSyncReceive",
	"allReady",
	"peerBuffering",
	"ignoreWait",
	"pingReport",
	"groupState",
	"waitDeadline",
	"activityFeed",
}

func (h *Hub) syncConfig() syncConfigPayload {
	s := h.cfg.SyncPlay
	return syncConfigPayload{
		Version:  protocolVersion,
		Features: featureList,
		Gate: gateConfig{
			BufferGoalSeconds:   s.BufferGoal.Seconds(),
			BufferGoalFraction:  s.BufferGoalFraction,
			ReadyTimeoutMs:      s.ReadyTimeout.Milliseconds(),
			EscapeWaitMs:        s.EscapeWait.Milliseconds(),
			EscapeBufferSeconds: s.EscapeBuffer.Seconds(),
			WaitDeadlineMs:      s.WaitDeadline.Milliseconds(),
			StallDetectMs:       s.StallDetectWindow.Milliseconds(),
			StallGraceMs:        s.StallGrace.Milliseconds(),
		},
		Start: startConfig{
			LeadMinMs:         s.StartLeadFloor.Milliseconds(),
			LeadMaxMs:         s.StartLeadCeiling.Milliseconds(),
			MaxReportedPingMs: s.MaxReportedPing.Milliseconds(),
		},
		Correction: correctionConfig{
			SlowdownRate:       s.SlowdownRate,
			SpeedupRate:        s.SpeedupRate,
			EngageDriftMs:      s.CorrectionEngageDrift.Milliseconds(),
			ReleaseDriftMs:     s.CorrectionReleaseDrift.Milliseconds(),
			FastForwardDriftMs: s.FastForwardDrift.Milliseconds(),
		},
		Drift: driftConfig{
			SoftSeekThresholdMs:  s.SoftSeekThreshold.Milliseconds(),
			SpeedSyncMaxDriftMs:  s.SpeedSyncMaxDrift.Milliseconds(),
			MaxPlaybackOffsetMs:  s.MaxPlaybackOffset.Milliseconds(),
			MaxCatchUpOffsetMs:   s.MaxCatchUpOffset.Milliseconds(),
			SoftCorrectCeilingMs: int64(softCorrectCeilingMS),
			MaxSpeedSeekMs:       maxSpeedSeekMS,
		},
		TimeSync: timeSyncConfig{
			Window:           s.TimeSyncWindow,
			DeadbandMs:       s.TimeSyncDeadband.Milliseconds(),
			MaxAgeMs:         s.TimeSyncMaxAge.Milliseconds(),
			GreedyIntervalMs: s.TimeSyncGreedyInterval.Milliseconds(),
			IdlePlayingMs:    s.TimeSyncIdleInterval.Milliseconds(),
			IdlePausedMs:     s.TimeSyncPausedInterval.Milliseconds(),
		},
		Limits: limitConfig{
			HeartbeatIntervalMs: pingIntervalMillis(s),
			ProtocolTimeoutMs:   s.ProtocolTimeout.Milliseconds(),
			MaxParticipants:     s.MaxParticipants,
		},
	}
}

// syncConfigPointer is syncConfig as an owning pointer, the shape an ack payload embeds.
func (h *Hub) syncConfigPointer() *syncConfigPayload {
	cfg := h.syncConfig()
	return &cfg
}

// pingIntervalMillis keeps heartbeats well inside the stall-detect window, so a frozen peer is
// caught by position rather than by a missing packet.
func pingIntervalMillis(s config.SyncPlayConfig) int64 {
	const wanted = int64(2000)
	if window := s.StallDetectWindow.Milliseconds(); window > 0 && window/2 < wanted {
		return window / 2
	}
	return wanted
}

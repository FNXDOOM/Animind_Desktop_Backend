package syncplay

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"animind-backend-go/internal/auth"
	"animind-backend-go/internal/config"
)

// newTestHub builds a hub with no database and an authenticator that refuses everything,
// which is enough to exercise framing and configuration defaults.
func newTestHub(t *testing.T) *Hub {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/db")
	t.Setenv("DESKTOP_TOKEN_SIGNING_KEY", "0123456789012345678901234567890123456789")
	t.Setenv("STORAGE_MODE", "local")
	t.Setenv("LOCAL_STORAGE_PATH", t.TempDir())

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return NewHub(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, refusingAuthenticator{})
}

type refusingAuthenticator struct{}

func (refusingAuthenticator) Authenticate(_ context.Context, _ string) (*auth.Principal, error) {
	return nil, auth.ErrTokenInvalid
}

func TestEncodeEventFrame(t *testing.T) {
	frame, err := encodeEvent("heartbeat", map[string]any{"currentTime": 12.5})
	if err != nil {
		t.Fatal(err)
	}
	// A socket.io EVENT packet is "2" followed by a JSON array whose first element is the
	// event name.
	if frame[0] != sioEvent {
		t.Fatalf("frame must start with the EVENT type byte, got %q", frame[:2])
	}
	var args []json.RawMessage
	if err := json.Unmarshal([]byte(frame[1:]), &args); err != nil {
		t.Fatalf("frame body is not a JSON array: %q err=%v", frame, err)
	}
	var name string
	if err := json.Unmarshal(args[0], &name); err != nil || name != "heartbeat" {
		t.Errorf("event name = %q err=%v", name, err)
	}
}

func TestDecodeMessageEventAndAckID(t *testing.T) {
	events, err := decodeMessage(`2["joinRoom",{"roomCode":"abcde"}]`)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected one event, got %d", len(events))
	}
	if events[0].Event != "joinRoom" {
		t.Errorf("event = %q", events[0].Event)
	}
	if events[0].ACKID != nil {
		t.Errorf("a bare event must have no ack id, got %v", *events[0].ACKID)
	}
	var payload struct {
		RoomCode string `json:"roomCode"`
	}
	if err := json.Unmarshal(events[0].Args, &payload); err != nil || payload.RoomCode != "abcde" {
		t.Errorf("payload = %q", string(events[0].Args))
	}

	// With an ack id the digits sit between the type byte and the JSON array.
	events, err = decodeMessage(`217["createRoom",{"episodeId":"ep-1"}]`)
	if err != nil {
		t.Fatalf("decode ack request: %v", err)
	}
	if len(events) != 1 || events[0].ACKID == nil || *events[0].ACKID != 17 {
		t.Fatalf("ack id was not parsed: %+v", events)
	}
	if events[0].Event != "createRoom" {
		t.Errorf("event = %q, want createRoom", events[0].Event)
	}
}

func TestDecodeMessageBatchedPackets(t *testing.T) {
	// socket.io-client batches several events into one Engine.IO message.
	batch := `2["play",{"currentTime":1}]2["heartbeat",{"currentTime":2}]`
	events, err := decodeMessage(batch)
	if err != nil {
		t.Fatalf("decode batch: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected two events, got %d: %+v", len(events), events)
	}
	if events[0].Event != "play" || events[1].Event != "heartbeat" {
		t.Errorf("events = %q, %q", events[0].Event, events[1].Event)
	}
}

func TestSplitPacketKeepsBracketsInsideStrings(t *testing.T) {
	// A title containing brackets must not be mistaken for a packet boundary.
	input := `2["changeEpisode",{"episodeId":"a]b[\"c"}]` + `2["ready",{}]`
	first, rest, err := splitPacket(input)
	if err != nil {
		t.Fatal(err)
	}
	if rest != `2["ready",{}]` {
		t.Errorf("rest = %q, want the second packet intact", rest)
	}
	events, err := decodeMessage(first)
	if err != nil {
		t.Fatalf("decode first packet: %v", err)
	}
	if len(events) != 1 || events[0].Event != "changeEpisode" {
		t.Errorf("events = %+v", events)
	}
}

func TestDecodeMessageRejectsGarbage(t *testing.T) {
	if _, err := decodeMessage(`2[not json]`); err == nil {
		t.Error("a malformed event body must be rejected")
	}
	if _, err := decodeMessage(`2[]`); err == nil {
		t.Error("an event with no name must be rejected")
	}
}

func TestEncodeAckFrame(t *testing.T) {
	frame, err := encodeAck(3, map[string]bool{"success": true})
	if err != nil {
		t.Fatal(err)
	}
	if frame[0] != sioEventAck {
		t.Fatalf("ack must use the EVENT ACK type byte, got %q", frame[:1])
	}
	if frame[1] != '3' {
		t.Fatalf("ack id must follow the type byte: %q", frame)
	}
}

func TestRoomCodeAlphabetIsAmbiguityFree(t *testing.T) {
	for _, banned := range []rune{'I', 'O', '0', '1'} {
		if strings.ContainsRune(roomCodeAlphabet, banned) {
			t.Errorf("room code alphabet must exclude %q, which users cannot reliably type", string(banned))
		}
	}
	if len(roomCodeAlphabet) != 32 {
		t.Errorf("alphabet length = %d, want 32", len(roomCodeAlphabet))
	}

	code, err := randomRoomCode(roomCodeLength)
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != roomCodeLength {
		t.Errorf("code %q has the wrong length", code)
	}
	seen := map[string]bool{code: true}
	for i := 0; i < 200; i++ {
		other, err := randomRoomCode(roomCodeLength)
		if err != nil {
			t.Fatal(err)
		}
		if seen[other] {
			t.Fatalf("two draws produced the same code %q", other)
		}
		seen[other] = true
	}
}

func TestOpenPayloadMatchesEngineIODefaults(t *testing.T) {
	hub := newTestHub(t)
	payload := hub.openPayload("sid-1")
	if payload.SID != "sid-1" {
		t.Errorf("sid = %q", payload.SID)
	}
	// pingInterval is milliseconds on the wire; a value in seconds would make clients
	// disconnect themselves.
	if payload.PingInterval < 1000 {
		t.Errorf("pingInterval = %d, which looks like seconds rather than milliseconds", payload.PingInterval)
	}
	if len(payload.Upgrades) == 0 || payload.Upgrades[0] != "websocket" {
		t.Errorf("upgrades = %v, must offer websocket", payload.Upgrades)
	}
}

func TestAuthenticateRejectsUnknownToken(t *testing.T) {
	hub := newTestHub(t)
	if _, err := hub.authenticate(""); err == nil {
		t.Error("an empty token must be refused")
	}
	if _, err := hub.authenticate("as_notarealtoken"); err == nil {
		t.Error("a token the auth service does not recognise must be refused")
	}
}

// TestRoomSnapshotCarriesTheParentShow pins the field a cross-show joiner needs: the room lookup
// must name the show, not only the episode, or the client has to guess from a cache it may not have.
func TestRoomSnapshotCarriesTheParentShow(t *testing.T) {
	h := newMemHub(t, nil)
	room, _ := roomWith(h, "ABCDE", "host")
	room.mu.Lock()
	room.showID = "show-9"
	room.mu.Unlock()

	snapshot := room.snapshot()
	if snapshot.ShowID != "show-9" {
		t.Fatalf("snapshot showId = %q, want show-9", snapshot.ShowID)
	}
	if snapshot.EpisodeID != "ep-1" {
		t.Fatalf("snapshot episodeId = %q, want ep-1", snapshot.EpisodeID)
	}
}

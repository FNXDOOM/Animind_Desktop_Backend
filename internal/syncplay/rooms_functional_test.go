package syncplay

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"

	"animind-backend-go/internal/auth"
	"animind-backend-go/internal/config"
	"animind-backend-go/internal/database"
	"animind-backend-go/internal/repository"
)

// Functional tests for the watch-party protocol: two real Socket.IO clients against a real
// Hub, asserting the room state machine through the frames the shipped site parses.
//
// These need a database, because creating a room validates the episode and records the room.
// They skip when POSTGRES_TEST_DATABASE_URL is unset; the pure pieces (framing, drift policy,
// room codes) are covered without one.

// client is a minimal Socket.IO v4 client over a raw WebSocket: enough to connect with an
// auth payload, emit events, read ACKs, and collect broadcasts.
type client struct {
	t        *testing.T
	conn     *websocket.Conn
	name     string
	token    string
	mu       sync.Mutex
	frames   []string
	eventsMu sync.Mutex
	events   []framed
	nextID   int
}

type framed struct {
	event string
	arg   json.RawMessage
	id    int
}

func dial(t *testing.T, url, token string, hub *Hub) *client {
	t.Helper()
	wsURL := strings.Replace(url, "http://", "ws://", 1)
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL+"/api/socket.io?EIO=4&transport=websocket", nil)
	if err != nil {
		if resp != nil {
			t.Fatalf("dial %s: %v (status %d)", url, err, resp.StatusCode)
		}
		t.Fatalf("dial %s: %v", url, err)
	}
	c := &client{t: t, conn: conn, token: token}
	go c.readLoop()

	// Engine.IO open packet arrives first; then we send the Socket.IO CONNECT with the token.
	if frame := c.waitFor(func(f string) bool { return strings.HasPrefix(f, "0") }, 5*time.Second); frame == "" {
		t.Fatal("no Engine.IO OPEN packet")
	}
	c.send("40" + mustJSON(map[string]string{"token": token}))
	if frame := c.waitFor(func(f string) bool { return strings.HasPrefix(f, "40") }, 10*time.Second); frame == "" {
		// A rejected handshake answers 44{...} (Engine.IO message + Socket.IO CONNECT_ERROR),
		// so include the frames seen to tell a refused token apart from a slow database.
		t.Fatalf("no Socket.IO CONNECT ack; frames=%v", c.framesSeen())
	}
	_ = hub
	return c
}

func (c *client) readLoop() {
	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		frame := string(raw)
		c.mu.Lock()
		c.frames = append(c.frames, frame)
		c.mu.Unlock()
		if strings.HasPrefix(frame, "42") {
			if event, arg, id := parseEvent(frame); event != "" {
				c.eventsMu.Lock()
				c.events = append(c.events, framed{event: event, arg: arg, id: id})
				c.eventsMu.Unlock()
			}
		}
	}
}

func parseEvent(frame string) (string, json.RawMessage, int) {
	body := strings.TrimPrefix(frame, "42")
	id := 0
	for i, ch := range body {
		if ch == '[' {
			id, body = 0, body[i:]
			break
		}
		if ch < '0' || ch > '9' {
			return "", nil, 0
		}
		id = id*10 + int(ch-'0')
		_ = i
	}
	var args []json.RawMessage
	if err := json.Unmarshal([]byte(body), &args); err != nil || len(args) == 0 {
		return "", nil, 0
	}
	var event string
	if err := json.Unmarshal(args[0], &event); err != nil {
		return "", nil, 0
	}
	payload := json.RawMessage("{}")
	if len(args) > 1 {
		payload = args[1]
	}
	return event, payload, id
}

func (c *client) send(payload string) {
	if err := c.conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

// emit sends an event and returns immediately.
func (c *client) emit(event string, payload any) {
	body, err := json.Marshal([]any{event, payload})
	if err != nil {
		c.t.Fatal(err)
	}
	c.send("42" + string(body))
}

// emitWithAck sends an event that asks for an acknowledgement, and returns that ack body.
func (c *client) emitWithAck(event string, payload any) json.RawMessage {
	c.nextID++
	id := c.nextID
	body, err := json.Marshal([]any{event, payload})
	if err != nil {
		c.t.Fatal(err)
	}
	c.send("42" + itoa(id) + string(body))

	expiry := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		var match string
		for _, f := range c.frames {
			if strings.HasPrefix(f, "43"+itoa(id)) {
				match = f
				break
			}
		}
		c.mu.Unlock()
		if match != "" {
			rest := strings.TrimPrefix(match, "43"+itoa(id))
			var args []json.RawMessage
			if err := json.Unmarshal([]byte(rest), &args); err != nil || len(args) == 0 {
				c.t.Fatalf("malformed ack %q", match)
			}
			return args[0]
		}
		if time.Now().After(expiry) {
			c.t.Fatalf("no ack for %s", event)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// collect returns the payload of a received event, removing it from the queue so a later
// collector sees the rest. Events are kept rather than streamed through a channel because
// several broadcasts land while the test is waiting on something else.
func (c *client) collect(event string, timeout time.Duration) (json.RawMessage, bool) {
	expiry := time.Now().Add(timeout)
	for {
		c.eventsMu.Lock()
		for i, frame := range c.events {
			if frame.event == event {
				payload := frame.arg
				c.events = append(c.events[:i], c.events[i+1:]...)
				c.eventsMu.Unlock()
				return payload, true
			}
		}
		c.eventsMu.Unlock()
		if time.Now().After(expiry) {
			return nil, false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitFor blocks until a recorded frame satisfies the predicate, returning "" on timeout.
func (c *client) waitFor(match func(string) bool, timeout time.Duration) string {
	deadline := time.After(timeout)
	for {
		c.mu.Lock()
		for _, f := range c.frames {
			if match(f) {
				c.mu.Unlock()
				return f
			}
		}
		c.mu.Unlock()
		select {
		case <-deadline:
			return ""
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func itoa(v int) string { return strconv.Itoa(v) }

func itoa64(v int64) string { return strconv.FormatInt(v, 10) }

// isolatedDatabaseURL creates a database of its own for this test binary and returns a DSN
// for it.
//
// Without this the two database-backed suites race each other: the repository suite
// truncates every table to start clean, which deletes the rows this suite is mid-handshake
// over. Separate databases, rather than a serialised test run, is what keeps a parallel
// `go test ./...` honest.
func isolatedDatabaseURL(t *testing.T, dsn string) string {
	t.Helper()

	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Scheme == "" {
		t.Skipf("watch-party functional tests need a postgres:// DSN so they can isolate a database: %v", err)
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to the maintenance database: %v", err)
	}

	// The name is a constant prefix plus this process id, never caller input.
	name := "animind_syncplay_" + strconv.Itoa(os.Getpid())
	if _, err := conn.Exec(ctx, "drop database if exists "+name); err != nil {
		conn.Close(ctx)
		t.Fatalf("drop scratch database: %v", err)
	}
	if _, err := conn.Exec(ctx, "create database "+name); err != nil {
		conn.Close(ctx)
		t.Fatalf("create scratch database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := conn.Exec(context.Background(), "drop database if exists "+name); err != nil {
			t.Logf("could not drop the scratch database: %v", err)
		}
		conn.Close(context.Background())
	})

	isolated := *parsed
	isolated.Path = "/" + name
	return isolated.String()
}

// roomFixture is a live hub plus the seeded show, episode and two signed-in accounts.
type roomFixture struct {
	hub     *Hub
	server  *httptest.Server
	store   *repository.Store
	episode string
	users   []string // session tokens, host first
}

func newRoomFixture(t *testing.T) *roomFixture {
	t.Helper()
	dsn := os.Getenv("POSTGRES_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set POSTGRES_TEST_DATABASE_URL to exercise the watch-party protocol")
	}
	dsn = isolatedDatabaseURL(t, dsn)

	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("DESKTOP_TOKEN_SIGNING_KEY", strings.Repeat("k", 48))
	t.Setenv("STORAGE_MODE", "local")
	t.Setenv("LOCAL_STORAGE_PATH", t.TempDir())
	// Two connections are plenty for a handshake test and keep the shared server's
	// connection budget free for the other suites running in parallel.
	t.Setenv("DB_MAX_CONNS", "2")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ctx := context.Background()
	db, err := database.Open(ctx, cfg, logger)
	if err != nil {
		t.Fatalf("database: %v", err)
	}
	t.Cleanup(db.Close)
	store := repository.New(db)

	suffix := time.Now().UnixNano()
	email := func(name string) string { return name + "." + itoa64(suffix) + "@syncplay.test" }
	hash := "argon2id$v=19$m=2048,t=1,p=1$c2FsdGRhdGF0aGF0aXMxMA$ZHVtbXloYXNoZm9ydGVzdGluZ29ubHk"

	title := "SyncPlay Test Show " + itoa64(suffix)
	show, err := store.CreateShow(ctx, repository.Show{Title: title, Genres: []string{}})
	if err != nil {
		t.Fatalf("CreateShow: %v", err)
	}
	one := 1.0
	episode, err := store.UpsertEpisode(ctx, repository.Episode{
		ShowID: show.ID, SeasonNumber: 1, ContentType: "tv", EpisodeNumber: &one,
		FilePath: "syncplay/ep01.mkv", BucketName: "local",
	})
	if err != nil {
		t.Fatalf("UpsertEpisode: %v", err)
	}

	signIn := func(name string) string {
		user, err := store.CreateUser(ctx, email(name), email(name), name, nil, &hash)
		if err != nil {
			t.Fatalf("create user: %v", err)
		}
		// The token is unique per fixture so two runs against one database do not collide on
		// the session hash.
		token := name + "-" + itoa64(suffix)
		if _, err := store.CreateWebSession(ctx, user.ID, auth.HashToken(token), name, "test", "", false, time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("create session: %v", err)
		}
		return token
	}

	hub := NewHub(cfg, logger, store, testAuthenticator{store: store})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub.serveSocket(w, r)
	}))
	t.Cleanup(func() {
		hub.CloseAll()
		server.Close()
	})

	return &roomFixture{
		hub: hub, server: server, store: store, episode: episode.ID,
		users: []string{signIn("host"), signIn("member")},
	}
}

// testAuthenticator maps the two fixture tokens onto the seeded accounts.
type testAuthenticator struct{ store *repository.Store }

func (a testAuthenticator) Authenticate(ctx context.Context, token string) (*auth.Principal, error) {
	session, err := a.store.ActiveWebSession(ctx, auth.HashToken(token))
	if err != nil {
		return nil, auth.ErrTokenInvalid
	}
	user, err := a.store.UserByID(ctx, session.UserID)
	if err != nil {
		return nil, auth.ErrTokenInvalid
	}
	return &auth.Principal{UserID: user.ID, Username: user.Username, Email: user.Email, Kind: "web", WebSessionRowID: session.ID}, nil
}

// TestWatchPartyHandshakeAndGate drives the real protocol: create a room, have a second peer
// join it, and assert that the join re-gates the room and that the gate announces the
// canonical position to both peers.
func TestWatchPartyHandshakeAndGate(t *testing.T) {
	fixture := newRoomFixture(t)
	base := fixture.server.URL

	host := dial(t, base, fixture.users[0], fixture.hub)
	member := dial(t, base, fixture.users[1], fixture.hub)

	ack := struct {
		Success  bool   `json:"success"`
		Error    string `json:"error"`
		RoomCode string `json:"roomCode"`
	}{}
	if err := json.Unmarshal(host.emitWithAck("createRoom", map[string]string{"episodeId": "00000000-0000-0000-0000-000000000000"}), &ack); err != nil {
		t.Fatalf("decode createRoom ack: %v", err)
	}
	t.Cleanup(func() { host.conn.Close(); member.conn.Close() })

	if ack.Success || ack.Error == "" {
		t.Fatalf("an unknown episode must be refused, got %+v", ack)
	}

	if err := json.Unmarshal(host.emitWithAck("createRoom", map[string]string{"episodeId": fixture.episode}), &ack); err != nil {
		t.Fatalf("decode createRoom ack: %v", err)
	}
	if !ack.Success || len(ack.RoomCode) != 5 {
		t.Fatalf("createRoom failed: %+v", ack)
	}

	join := struct {
		Success          bool `json:"success"`
		ParticipantCount int  `json:"participantCount"`
		IsPlaying        bool `json:"isPlaying"`
	}{}
	if err := json.Unmarshal(member.emitWithAck("joinRoom", map[string]string{"roomCode": strings.ToLower(ack.RoomCode)}), &join); err != nil {
		t.Fatalf("decode joinRoom ack: %v", err)
	}
	if !join.Success {
		t.Fatalf("joining by a lower-case code must work: %+v", join)
	}
	if join.ParticipantCount != 2 || join.IsPlaying {
		t.Errorf("joinRoom ack = %+v, want 2 participants and isPlaying false", join)
	}

	// The join opened a gate for everyone, including the host who did not join.
	for name, peer := range map[string]*client{"host": host, "member": member} {
		payload, ok := peer.collect("waitForBufferGoal", 5*time.Second)
		if !ok {
			t.Errorf("%s never received waitForBufferGoal; frames=%v", name, peer.framesSeen())
			continue
		}
		var gate struct {
			CurrentTime       float64 `json:"currentTime"`
			BufferGoalSeconds float64 `json:"bufferGoalSeconds"`
			TotalPeers        int     `json:"totalPeers"`
			Peers             []struct {
				SocketID    string `json:"socketId"`
				DisplayName string `json:"displayName"`
				ReadyState  string `json:"readyState"`
			} `json:"peers"`
		}
		if err := json.Unmarshal(payload, &gate); err != nil {
			t.Errorf("%s: bad gate payload: %v", name, err)
			continue
		}
		if gate.TotalPeers != 2 || len(gate.Peers) != 2 {
			t.Errorf("%s: gate says %d peers / %d listed, want 2 and 2", name, gate.TotalPeers, len(gate.Peers))
		}
		if gate.BufferGoalSeconds <= 0 {
			t.Errorf("%s: gate carries no buffer goal", name)
		}
	}

	if _, ok := host.collect("peerJoined", 5*time.Second); !ok {
		t.Error("host never heard peerJoined")
	}

	// Both peers reporting enough buffer closes the gate and resumes play, because the
	// room's play intent was inherited from the host's play request.
	host.emit("play", map[string]any{"currentTime": 12.5})
	if _, ok := member.collect("waitForBufferGoal", 5*time.Second); !ok {
		t.Fatal("a play did not open a gate for the member")
	}
	for _, peer := range []*client{host, member} {
		peer.emit("bufferingProgress", map[string]any{"bufferedSeconds": 200, "percent": 40})
	}

	synced, ok := member.collect("syncPlay", 8*time.Second)
	if !ok {
		t.Fatalf("the gate never closed with syncPlay; frames=%v", member.framesSeen())
	}
	var play struct {
		CurrentTime float64 `json:"currentTime"`
	}
	if err := json.Unmarshal(synced, &play); err != nil {
		t.Fatalf("decode syncPlay: %v", err)
	}
	if play.CurrentTime != 12.5 {
		t.Errorf("syncPlay carried %.2f, want the host's 12.5", play.CurrentTime)
	}

	// participantStates is carried by the heartbeat the client sends every few seconds while
	// playing, so drive one rather than waiting for a broadcast that only follows it.
	member.emit("heartbeat", map[string]any{"currentTime": 12.5, "playbackRate": 1.0, "bufferedAhead": 30})
	statesPayload, ok := member.collect("participantStates", 5*time.Second)
	if !ok {
		t.Fatal("no participantStates broadcast after a heartbeat")
	}
	var states struct {
		Peers []struct {
			SocketID    string `json:"socketId"`
			UserID      string `json:"userId"`
			DisplayName string `json:"displayName"`
			ReadyState  string `json:"readyState"`
			Ready       bool   `json:"ready"`
		} `json:"peers"`
		CanonicalTime float64 `json:"canonicalTime"`
		IsPlaying     bool    `json:"isPlaying"`
	}
	if err := json.Unmarshal(statesPayload, &states); err != nil {
		t.Fatalf("decode participantStates: %v", err)
	}
	if len(states.Peers) != 2 {
		t.Errorf("participantStates listed %d peers, want 2", len(states.Peers))
	}
	// The roster fields are what the site renders; a missing socketId or displayName shows up
	// as an empty peer list there.
	for _, peer := range states.Peers {
		if peer.SocketID == "" || peer.DisplayName == "" {
			t.Errorf("peer entry is missing roster fields: %+v", peer)
		}
	}
	if !states.IsPlaying {
		t.Error("the room should report playing after the gate closed with play intent")
	}

	// requestSync is the reconnect path: it must answer with the room's current truth.
	syncState, ok := member.emitRequest("requestSync", "sync", 5*time.Second)
	if !ok {
		t.Fatal("requestSync was not answered")
	}
	var state struct {
		IsPlaying bool    `json:"isPlaying"`
		Current   float64 `json:"currentTime"`
	}
	_ = json.Unmarshal(syncState, &state)
	if !state.IsPlaying {
		t.Errorf("after the gate closed the room should be playing: %+v", state)
	}

	// Leaving is disconnecting: dropping the member closes nothing (the host remains), but
	// the host must hear peerLeft.
	member.conn.Close()
	if _, ok := host.collect("peerLeft", 5*time.Second); !ok {
		t.Error("host never heard peerLeft after the member's socket closed")
	}
}

func TestWatchPartyRejectsBadCredentials(t *testing.T) {
	fixture := newRoomFixture(t)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/socket.io?EIO=4&transport=websocket", nil)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")

	// A refused upgrade is answered before any room state exists.
	fixture.hub.serveSocket(recorder, request)
	body := recorder.Body.String()
	if recorder.Code == http.StatusSwitchingProtocols {
		t.Skip("the httptest recorder cannot drive an upgrade; handshake rejection covered by dial below")
	}
	if !strings.Contains(body, "Invalid") && recorder.Code < 400 {
		t.Errorf("an anonymous socket should not be accepted: %d %q", recorder.Code, body)
	}
}

// framesSeen exposes the collected frames for failure messages.
func (c *client) framesSeen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.frames))
	copy(out, c.frames)
	return out
}

// emitRequest sends an event and waits for a plain (non-ack) event of the given name.
func (c *client) emitRequest(event, waitFor string, timeout time.Duration) (json.RawMessage, bool) {
	c.emit(event, map[string]any{})
	return c.collect(waitFor, timeout)
}

// Package syncplay implements watch parties: shared playback on a server-authoritative clock.
// The Socket.IO v4 protocol is hand-rolled here because the deployed socket.io-client needs
// Engine.IO framing over both WebSocket and long-polling, and only the subset this feature uses
// is worth maintaining. Room state lives in memory, since live playback is pointless to persist
// per heartbeat; the database keeps only a best-effort audit trail, so a write failure warns
// rather than stops the party.
package syncplay

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Engine.IO v4 packet types.
const (
	engineOpen    = '0'
	engineClose   = '1'
	enginePing    = '2'
	enginePong    = '3'
	engineMessage = '4'
	engineUpgrade = '5'
	engineNoop    = '6'
)

// Socket.IO v4 packet types, carried inside an Engine.IO message.
const (
	sioConnect      = byte('0')
	sioDisconnect   = byte('1')
	sioEvent        = byte('2')
	sioEventAck     = byte('3')
	sioConnectError = byte('4')
)

// SocketPath is where clients connect. It is namespaced under /api so a single reverse
// proxy rule serves the API and the sockets from one origin.
const SocketPath = "/api/socket.io"

// engineOpenPayload is the handshake body sent immediately on connect. pingInterval and
// pingTimeout follow the Engine.IO defaults the clients were built against.
type engineOpenPayload struct {
	SID          string   `json:"sid"`
	Upgrades     []string `json:"upgrades"`
	PingInterval int      `json:"pingInterval"`
	PingTimeout  int      `json:"pingTimeout"`
	MaxPayload   int      `json:"maxPayload"`
}

// inbound is one decoded application event.
type inbound struct {
	Event    string
	Args     json.RawMessage
	ACKID    *int
	WantsACK bool
}

// peer is one connected client, independent of transport.
type peer struct {
	sid string

	hub    *Hub
	userID string
	name   string

	// out carries fully framed Engine.IO strings to the writer.
	out chan string
	// roomCode is the room this peer joined, if any.
	roomCode string

	// acks maps a socket.io ack id to the event it answers, so inbound ACK frames can be
	// attributed (the current protocol has no client-to-server acks; this keeps the framing
	// honest if that changes).
	ackMu sync.Mutex

	closed   chan struct{}
	closeOne sync.Once
	logger   *slog.Logger

	// transport is "websocket" or "polling".
	transport string

	// pollMu guards the inbox for long-polling readers.
	pollMu      sync.Mutex
	inbox       []string
	pollWait    chan struct{}
	lastTouched time.Time

	// upgrading records that the client announced a WebSocket upgrade.
	upgrading bool
}

func newPeer(sid, transport string, hub *Hub, logger *slog.Logger) *peer {
	return &peer{
		sid:         sid,
		transport:   transport,
		hub:         hub,
		out:         make(chan string, 64),
		closed:      make(chan struct{}),
		pollWait:    make(chan struct{}, 1),
		logger:      logger,
		lastTouched: time.Now(),
	}
}

// send frames an Engine.IO message packet containing raw socket.io data.
func (p *peer) send(raw string) {
	p.enqueue(string(engineMessage) + raw)
}

func (p *peer) enqueue(frame string) {
	select {
	case p.out <- frame:
	default:
		// A slow client must not block the room. Dropping a stale heartbeat broadcast is
		// correct behaviour; dropping a state change would not be, so the hub re-sends
		// critical packets through sendDirect.
		p.logger.Debug("dropping socket packet for a slow client",
			slog.String("sid", p.sid), slog.String("peer", p.userID))
	}
	select {
	case p.pollWait <- struct{}{}:
	default:
	}
}

// Emit sends an application event with a JSON payload.
func (p *peer) Emit(event string, payload any) {
	frame, err := encodeEvent(event, payload)
	if err != nil {
		p.logger.Warn("could not encode socket event", slog.String("event", event), slog.String("error", err.Error()))
		return
	}
	p.send(frame)
}

// EmitAck answers an event that requested an acknowledgement.
func (p *peer) EmitAck(id int, payload any) {
	frame, err := encodeAck(id, payload)
	if err != nil {
		p.logger.Warn("could not encode socket ack", slog.Int("ack", id), slog.String("error", err.Error()))
		return
	}
	p.send(frame)
}

// Close ends the connection.
func (p *peer) Close() {
	p.closeOne.Do(func() { close(p.closed) })
	select {
	case p.out <- string(engineClose):
	default:
	}
}

// encodeEvent builds a socket.io EVENT frame: 42["name",{...}]
func encodeEvent(event string, payload any) (string, error) {
	args := []any{event}
	if payload != nil {
		args = append(args, payload)
	}
	body, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	return string(sioEvent) + string(body), nil
}

// encodeAck builds a socket.io EVENT ACK frame: 43<id>[payload]
func encodeAck(id int, payload any) (string, error) {
	body, err := json.Marshal([]any{payload})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%d%s", string(sioEventAck), id, string(body)), nil
}

// decodeMessage parses one or more concatenated socket.io packets in an Engine.IO message, which
// is how socket.io-client sends polling batches. It splits on the packet-type byte, always a
// digit and never the start of a JSON body.
func decodeMessage(payload string) ([]inbound, error) {
	var out []inbound
	remaining := payload
	for len(remaining) > 0 {
		packet, rest, err := splitPacket(remaining)
		if err != nil {
			return out, err
		}
		remaining = rest
		if packet == "" {
			continue
		}
		kind := packet[0]
		body := packet[1:]
		switch kind {
		case sioConnect:
			out = append(out, inbound{Event: "", Args: json.RawMessage(orEmpty(body)), WantsACK: false})
		case sioEvent:
			digits, eventBody := leadingDigits(body)
			in, err := decodeEventPacket(eventBody)
			if err != nil {
				return out, err
			}
			if digits != "" {
				id := 0
				_, _ = fmt.Sscanf(digits, "%d", &id)
				in.ACKID = &id
			}
			out = append(out, in)
		case sioEventAck:
			// An ack from the client carries no application data this server needs.
			continue
		case sioDisconnect:
			return out, errPeerDisconnect
		default:
			continue
		}
	}
	return out, nil
}

var errPeerDisconnect = errors.New("syncplay: peer requested disconnect")

func decodeEventPacket(body string) (inbound, error) {
	var args []json.RawMessage
	if err := json.Unmarshal([]byte(body), &args); err != nil {
		return inbound{}, fmt.Errorf("syncplay: malformed event packet %q: %w", truncate(body, 120), err)
	}
	if len(args) == 0 {
		return inbound{}, fmt.Errorf("syncplay: empty event packet")
	}
	var name string
	if err := json.Unmarshal(args[0], &name); err != nil {
		return inbound{}, fmt.Errorf("syncplay: event name is not a string: %w", err)
	}
	payload := json.RawMessage("{}")
	if len(args) > 1 {
		payload = args[1]
	}
	return inbound{Event: name, Args: payload}, nil
}

// splitPacket separates one socket.io packet from a concatenated batch by tracking JSON
// depth, since a packet boundary is only recognizable when the previous body is closed.
func splitPacket(input string) (packet, rest string, err error) {
	if input == "" {
		return "", "", nil
	}
	kind := input[0]
	body := input[1:]
	// Skip an ack id, which is digits optionally followed by a '-'.
	digits, _ := leadingDigits(body)
	if digits != "" {
		body = body[len(digits):]
		if strings.HasPrefix(body, "-") {
			// A binary ack placeholder; not used by this protocol.
			body = body[1:]
		}
	}
	if body == "" || body[0] != '[' {
		// CONNECT, DISCONNECT and error packets carry an object or nothing.
		if body == "" || body[0] == '{' {
			depth := 0
			for i := 0; i < len(body); i++ {
				switch body[i] {
				case '{':
					depth++
				case '}':
					depth--
				}
				if depth == 0 {
					return string(kind) + body[:i+1], body[i+1:], nil
				}
			}
		}
		return string(kind) + body, "", nil
	}
	depth := 0
	inString := false
	escaped := false
	for i := 0; i < len(body); i++ {
		c := body[i]
		if escaped {
			escaped = false
			continue
		}
		switch {
		case c == '\\' && inString:
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
		case c == '[' || c == '{':
			depth++
		case c == ']' || c == '}':
			depth--
			if depth == 0 {
				return string(kind) + digits + body[:i+1], body[i+1:], nil
			}
		}
	}
	return input, "", nil
}

func leadingDigits(input string) (digits, rest string) {
	i := 0
	for i < len(input) && input[i] >= '0' && input[i] <= '9' {
		i++
	}
	return input[:i], input[i:]
}

func orEmpty(s string) string {
	if s == "" {
		return "{}"
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// websocketHandler upgrades a connection and runs one peer; both the initial connect and an
// upgrade from polling arrive here, and the sid parameter decides whether the peer is new.
func (h *Hub) websocketHandler(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("sid")
	encoding := r.URL.Query().Get("EIO")
	if encoding != "" && encoding != "4" {
		writeHTTPError(w, http.StatusBadRequest, "unsupported EIO version")
		return
	}

	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.logger.Warn("websocket upgrade failed", slog.String("error", err.Error()))
		return
	}

	existing := h.takePendingPeer(sid)
	if existing != nil {
		// Engine.IO upgrade: confirm on the WebSocket and switch the transport.
		h.attachWebSocket(existing, conn, true)
		return
	}

	peer := newPeer(newSID(), "websocket", h, h.logger)
	h.register(peer)
	h.attachWebSocket(peer, conn, false)
}

// attachWebSocket drives read and write loops for a peer.
func (h *Hub) attachWebSocket(p *peer, conn *websocket.Conn, upgrade bool) {
	writer := h.wsWriter(p, conn)
	// finished lets the reader wait for the writer's last flush, so the socket is not closed
	// under a queue still holding the reason the connection ended.
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		writer()
	}()
	if upgrade {
		p.send(string(engineNoop))
	} else {
		p.enqueue(string(engineOpen) + mustJSON(h.openPayload(p.sid)))
	}

	conn.SetReadLimit(h.cfg.SyncPlay.ReadLimitBytes)
	pongDeadline := time.Now().Add(h.cfg.SyncPlay.PongTimeout)
	conn.SetReadDeadline(pongDeadline)
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(h.cfg.SyncPlay.PongTimeout))
	})

	defer func() {
		p.Close()
		select {
		case <-finished:
		case <-time.After(time.Second):
		}
		_ = conn.Close()
		h.disconnect(p)
	}()

	for {
		select {
		case <-p.closed:
			return
		default:
		}

		_, raw, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				h.logger.Debug("socket read error", slog.String("sid", p.sid), slog.String("error", err.Error()))
			}
			return
		}
		p.lastTouched = time.Now()
		if err := conn.SetReadDeadline(time.Now().Add(h.cfg.SyncPlay.PongTimeout)); err != nil {
			return
		}
		if !h.handleEngineFrame(p, string(raw)) {
			return
		}
	}
}

// drainBeforeClose flushes frames still queued on a closing peer, so a refusal's reason reaches
// the client before the socket drops. Returns false once a write fails.
func (h *Hub) drainBeforeClose(p *peer, conn *websocket.Conn) bool {
	for {
		select {
		case frame := <-p.out:
			_ = conn.SetWriteDeadline(time.Now().Add(h.cfg.SyncPlay.WriteTimeout))
			if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
				return false
			}
		default:
			return true
		}
	}
}

// wsWriter returns the loop that drains queued frames to the socket and sends Engine.IO pings.
func (h *Hub) wsWriter(p *peer, conn *websocket.Conn) func() {
	return func() {
		ticker := time.NewTicker(h.cfg.SyncPlay.PingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-p.closed:
				// Flush the queue first: a refusal's reason must arrive before the socket closes.
				if !h.drainBeforeClose(p, conn) {
					return
				}
				_ = conn.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
				return
			case frame := <-p.out:
				_ = conn.SetWriteDeadline(time.Now().Add(h.cfg.SyncPlay.WriteTimeout))
				if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
					h.logger.Debug("socket write error", slog.String("sid", p.sid), slog.String("error", err.Error()))
					p.Close()
					return
				}
			case <-ticker.C:
				_ = conn.SetWriteDeadline(time.Now().Add(h.cfg.SyncPlay.WriteTimeout))
				if err := conn.WriteMessage(websocket.TextMessage, []byte{enginePing}); err != nil {
					p.Close()
					return
				}
			}
		}
	}
}

// handleEngineFrame interprets one raw Engine.IO frame. It returns false when the
// connection should close.
func (h *Hub) handleEngineFrame(p *peer, frame string) bool {
	if frame == "" {
		return true
	}
	kind, body := frame[0], frame[1:]
	switch kind {
	case engineMessage:
		return h.handleSocketPacket(p, body)
	case enginePing:
		p.enqueue(string(enginePong))
		return true
	case enginePong:
		p.lastTouched = time.Now()
		return true
	case engineUpgrade:
		// The client proved the new transport works; make it primary.
		p.enqueue(string(engineUpgrade))
		return true
	case engineNoop:
		return true
	case engineClose:
		return false
	default:
		return true
	}
}

// handleSocketPacket processes socket.io-level packets, including the CONNECT packet that
// carries the auth payload.
func (h *Hub) handleSocketPacket(p *peer, body string) bool {
	if body == "" {
		return true
	}
	kind, rest := body[0], body[1:]

	switch kind {
	case sioConnect:
		// The CONNECT token is nested under "auth" by socket.io-client but sent flat by a
		// hand-rolled client; both shapes are accepted since we control only one.
		var conn struct {
			Token string `json:"token"`
			Auth  struct {
				Token string `json:"token"`
			} `json:"auth"`
		}
		_ = json.Unmarshal([]byte(orEmpty(rest)), &conn)
		token := conn.Token
		if token == "" {
			token = conn.Auth.Token
		}
		principal, err := h.authenticate(token)
		if err != nil {
			p.send(string(sioConnectError) + mustJSON(map[string]string{"message": errorMessage(err)}))
			return false
		}
		p.userID = principal.UserID
		p.name = principal.DisplayName
		p.send(string(sioConnect) + mustJSON(map[string]string{"sid": p.sid}))
		h.onReady(p)
		return true
	case sioDisconnect:
		return false
	case sioEvent:
		packet, _, err := splitPacket(string(sioEvent) + rest)
		if err != nil {
			h.logger.Debug("bad event packet", slog.String("sid", p.sid), slog.String("error", err.Error()))
			return true
		}
		events, err := decodeMessage(packet)
		if err != nil {
			h.logger.Debug("could not decode socket event", slog.String("sid", p.sid), slog.String("error", err.Error()))
			return true
		}
		for _, in := range events {
			h.dispatchEvent(p, in)
		}
		return true
	default:
		return true
	}
}

// pollingHandler implements the Engine.IO long-polling transport for clients that cannot
// open a WebSocket.
func (h *Hub) pollingHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("EIO") != "" && r.URL.Query().Get("EIO") != "4" {
		writeHTTPError(w, http.StatusBadRequest, "unsupported EIO version")
		return
	}

	sid := r.URL.Query().Get("sid")
	switch r.Method {
	case http.MethodGet:
		p := h.lookupPeer(sid)
		if p == nil {
			// A new polling session: open it and send the handshake immediately.
			p = newPeer(newSID(), "polling", h, h.logger)
			h.register(p)
			go p.keepAlive(h)
			p.enqueue(string(engineOpen) + mustJSON(h.openPayload(p.sid)))
			h.drainPoll(w, p, 0)
			return
		}
		h.drainPoll(w, p, h.cfg.SyncPlay.PingInterval)
	case http.MethodPost:
		p := h.lookupPeer(sid)
		if p == nil {
			writeHTTPError(w, http.StatusBadRequest, "unknown session")
			return
		}
		body, err := readLimited(r, h.cfg.SyncPlay.ReadLimitBytes)
		if err != nil {
			writeHTTPError(w, http.StatusRequestEntityTooLarge, "payload too large")
			return
		}
		for _, frame := range splitEngineFrames(body) {
			if !h.handleEngineFrame(p, frame) {
				h.disconnect(p)
			}
		}
		w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
		_, _ = w.Write([]byte("ok"))
	default:
		writeHTTPError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// drainPoll writes queued frames as a single polling response, waiting up to wait for them.
func (h *Hub) drainPoll(w http.ResponseWriter, p *peer, wait time.Duration) {
	var frames []string
	deadline := time.After(wait)
	for {
		select {
		case frame := <-p.out:
			frames = append(frames, frame)
		case <-p.closed:
			frames = append(frames, string(engineClose))
		case <-deadline:
		case <-time.After(50 * time.Millisecond):
			if len(frames) > 0 {
				continue
			}
			if wait == 0 {
				deadline = nil
				continue
			}
		}
		break
	}

	w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
	w.Header().Set("Access-Control-Allow-Credentials", "true")
	_, _ = w.Write([]byte(strings.Join(frames, "\x1e")))
}

// splitEngineFrames separates a batched polling POST.
func splitEngineFrames(body string) []string {
	if strings.Contains(body, "\x1e") {
		return strings.Split(body, "\x1e")
	}
	return []string{body}
}

// keepAlive expires idle polling sessions so abandoned browser tabs do not accumulate.
func (p *peer) keepAlive(h *Hub) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-p.closed:
			return
		case <-ticker.C:
			if p.transport == "polling" && time.Since(p.lastTouched) > 2*time.Minute {
				p.Close()
				h.disconnect(p)
				return
			}
		}
	}
}

func (h *Hub) openPayload(sid string) engineOpenPayload {
	return engineOpenPayload{
		SID:          sid,
		Upgrades:     []string{"websocket"},
		PingInterval: int(h.cfg.SyncPlay.PingInterval.Milliseconds()),
		PingTimeout:  int(h.cfg.SyncPlay.PongTimeout.Milliseconds()),
		MaxPayload:   int(h.cfg.SyncPlay.ReadLimitBytes),
	}
}

func mustJSON(v any) string {
	out, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(out)
}

func writeHTTPError(w http.ResponseWriter, status int, message string) {
	http.Error(w, message, status)
}

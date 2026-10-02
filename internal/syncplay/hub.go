package syncplay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"

	"animind-backend-go/internal/auth"
	"animind-backend-go/internal/config"
	"animind-backend-go/internal/repository"
)

// Hub owns every live peer and room. Each room serializes its own changes, so a handler never has to reason about interleaving from another goroutine.
// Authenticator resolves a socket's auth payload to an account. auth.Service satisfies it.

type Authenticator interface {
	Authenticate(ctx context.Context, token string) (*auth.Principal, error)
}

// Identity is a verified socket user.
type Identity struct {
	UserID      string
	DisplayName string
}

// Hub tracks peers and rooms.
type Hub struct {
	cfg    *config.Config
	logger *slog.Logger
	repo   *repository.Store
	auth   Authenticator

	upgrader websocket.Upgrader

	mu           sync.RWMutex
	peers        map[string]*peer
	pendingPeers map[string]*peer
	rooms        map[string]*Room
}

// NewHub builds the hub. CORS is enforced at the HTTP layer, so the socket handler only needs to refuse an upgrade from a disallowed origin.

func NewHub(cfg *config.Config, logger *slog.Logger, repo *repository.Store, authenticator Authenticator) *Hub {
	return &Hub{
		cfg:          cfg,
		logger:       logger,
		repo:         repo,
		auth:         authenticator,
		peers:        map[string]*peer{},
		pendingPeers: map[string]*peer{},
		rooms:        map[string]*Room{},
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				if origin == "" {
					// The desktop player and server-to-server clients send no Origin.
					return true
				}
				return cfg.OriginAllowed(strings.TrimRight(origin, "/"))
			},
		},
	}
}

// Register mounts the socket endpoints on a router.
func (h *Hub) Register(r chi.Router) {
	if !h.cfg.SyncPlay.Enabled {
		return
	}
	r.Get(SocketPath, h.serveSocket)
	r.Head(SocketPath, h.serveSocket)
	r.Post(SocketPath, h.serveSocket)
}

// serveSocket routes an Engine.IO request to the transport the client asked for.
func (h *Hub) serveSocket(w http.ResponseWriter, r *http.Request) {
	if isWebSocketUpgrade(r) {
		h.websocketHandler(w, r)
		return
	}
	if r.Method == http.MethodGet && r.URL.Query().Get("transport") == "polling" {
		h.pollingHandler(w, r)
		return
	}
	if r.Method == http.MethodPost {
		h.pollingHandler(w, r)
		return
	}
	writeHTTPError(w, http.StatusBadRequest, `{"message":"Invalid transport, must be websocket."}`)
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

func (h *Hub) register(p *peer) {
	h.mu.Lock()
	h.peers[p.sid] = p
	h.mu.Unlock()
}

func (h *Hub) lookupPeer(sid string) *peer {
	if sid == "" {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.peers[sid]
}

// takePendingPeer hands a resuming WebSocket its polling session, which is how an
// Engine.IO upgrade transfers state without duplicating the peer.
func (h *Hub) takePendingPeer(sid string) *peer {
	if sid == "" {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.peers[sid]
	if p == nil || p.transport != "polling" {
		return nil
	}
	p.transport = "websocket"
	p.upgrading = true
	return p
}

func (h *Hub) forgetPeer(p *peer) {
	h.mu.Lock()
	delete(h.peers, p.sid)
	h.mu.Unlock()
}

// authenticate resolves the socket token, rejecting anything this backend did not issue.
func (h *Hub) authenticate(token string) (*Identity, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("Authentication required.")
	}
	principal, err := h.auth.Authenticate(context.Background(), token)
	if err != nil {
		return nil, errors.New("Invalid token.")
	}
	name := principal.Username
	if strings.TrimSpace(name) == "" {
		name = firstEight(principal.UserID)
	}
	return &Identity{UserID: principal.UserID, DisplayName: name}, nil
}

// onReady runs after a peer has connected and authenticated.
func (h *Hub) onReady(p *peer) {
	h.logger.Debug("watch party peer connected",
		slog.String("sid", p.sid), slog.String("user", p.userID), slog.String("transport", p.transport))
}

// dispatchEvent routes one application event to its handler.
func (h *Hub) dispatchEvent(p *peer, in inbound) {
	// ACK-only frames (an event the client asked us to answer) carry their id; the room
	// handlers reply with the same id when they produce a result.
	switch in.Event {
	case "":
		return
	}

	if !h.cfg.SyncPlay.Enabled {
		p.Emit("syncDenied", map[string]string{"reason": "Watch parties are disabled on this server."})
		return
	}

	room := h.roomFor(p)
	switch in.Event {
	case "createRoom":
		h.handleCreateRoom(p, in, room)
	case "joinRoom":
		h.handleJoinRoom(p, in, room)
	case "play":
		h.handlePlay(p, in, room)
	case "pause":
		h.handlePause(p, in, room)
	case "seek":
		h.handleSeek(p, in, room)
	case "ready":
		h.handleReady(p, room)
	case "bufferingProgress":
		h.handleBufferingProgress(p, in, room)
	case "buffering":
		h.handleBuffering(p, room)
	case "stallRecovered":
		h.handleStallRecovered(p, room)
	case "heartbeat":
		h.handleHeartbeat(p, in, room)
	case "ping":
		h.handlePingReport(p, in, room)
	case "timesync_offset":
		h.handleTimeSyncOffset(p, in, room)
	case "ignoreWait":
		h.handleIgnoreWait(p, in, room)
	case "transferHost":
		h.handleTransferHost(p, in, room)
	case "changeEpisode":
		h.handleChangeEpisode(p, in, room)
	case "timesync_ping":
		h.handleTimeSync(p, in)
	case "requestSync":
		h.handleRequestSync(p, room)
	default:
		h.logger.Debug("unknown socket event", slog.String("event", in.Event), slog.String("sid", p.sid))
	}
}

// roomFor returns the room a peer belongs to.
func (h *Hub) roomFor(p *peer) *Room {
	if p.roomCode == "" {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.rooms[p.roomCode]
}

// disconnect removes a peer from its room and closes the room when it empties.
func (h *Hub) disconnect(p *peer) {
	h.forgetPeer(p)
	code := p.roomCode
	if code == "" {
		return
	}
	room := h.roomFor(p)
	if room == nil {
		return
	}
	room.handlePeerLeft(p, h)
}

// RoomCount reports active rooms for the health and admin endpoints.
func (h *Hub) RoomCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.rooms)
}

// PeerCount reports connected sockets.
func (h *Hub) PeerCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.peers)
}

// RoomSnapshot describes a room for the HTTP lookup endpoint, which lets a client validate
// a code before it asks the user to open the matching episode.
type RoomSnapshot struct {
	Code            string     `json:"code"`
	EpisodeID       string     `json:"episodeId"`
	HostUserID      string     `json:"hostUserId"`
	HostDisplayName string     `json:"hostDisplayName"`
	Status          string     `json:"status"`
	GroupState      groupState `json:"groupState"`
	CurrentTime     float64    `json:"currentTime"`
	IsPlaying       bool       `json:"isPlaying"`
	Participants    int        `json:"participantCount"`
}

// Snapshot returns a room's current state, or false when the code is unknown.
func (h *Hub) Snapshot(code string) (RoomSnapshot, bool) {
	h.mu.RLock()
	room := h.rooms[strings.ToUpper(strings.TrimSpace(code))]
	h.mu.RUnlock()
	if room == nil {
		return RoomSnapshot{}, false
	}
	return room.snapshot(), true
}

// PersistedRoom looks a room up in the database, so an expired code answers "not found"
// rather than "unknown" when it was never a room at all.
func (h *Hub) PersistedRoom(code string) (*repository.PartyRow, error) {
	return h.repo.PartyByCode(context.Background(), strings.ToUpper(strings.TrimSpace(code)))
}

// CloseAll ends every room, used on shutdown so participants are told before the process
// exits.
func (h *Hub) CloseAll() {
	h.mu.RLock()
	rooms := make([]*Room, 0, len(h.rooms))
	for _, room := range h.rooms {
		rooms = append(rooms, room)
	}
	peers := make([]*peer, 0, len(h.peers))
	for _, p := range h.peers {
		peers = append(peers, p)
	}
	h.mu.RUnlock()

	for _, room := range rooms {
		room.close(h)
	}
	for _, p := range peers {
		p.Close()
	}
}

// EndStaleRooms marks rooms from a previous process as ended. Live socket state does not
// survive a restart, so any row still active at boot is a stale record.
func (h *Hub) EndStaleRooms() {
	ended, err := h.repo.EndAllActiveParties(context.Background())
	if err != nil {
		h.logger.Warn("could not close stale watch parties", slog.String("error", err.Error()))
		return
	}
	if ended > 0 {
		h.logger.Info("closed watch parties left active by a previous run", slog.Int("count", int(ended)))
	}
}

// CleanupEnded deletes rooms that ended long ago along with their participant rows.
func (h *Hub) CleanupEnded() (rooms, participants int64) {
	cutoff := time.Now().Add(-h.cfg.SyncPlay.EndedTTL)
	deleted, count, err := h.repo.DeleteEndedParties(context.Background(), cutoff)
	if err != nil {
		h.logger.Warn("watch party cleanup failed", slog.String("error", err.Error()))
		return 0, 0
	}
	if deleted > 0 || count > 0 {
		h.logger.Info("watch party cleanup finished",
			slog.Int64("rooms", deleted), slog.Int64("participants", count))
	}
	return deleted, count
}

// roomCodeAlphabet excludes the characters that are easy to mistype or misread: I, O, 0, 1.
const roomCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

const roomCodeLength = 5

// newRoomCode mints a code and retries on collision with a live room.
func (h *Hub) newRoomCode() (string, error) {
	for attempt := 0; attempt < 32; attempt++ {
		code, err := randomRoomCode(roomCodeLength)
		if err != nil {
			return "", err
		}
		h.mu.RLock()
		_, taken := h.rooms[code]
		h.mu.RUnlock()
		if taken {
			continue
		}
		return code, nil
	}
	return "", errors.New("syncplay: could not allocate a room code")
}

func randomRoomCode(length int) (string, error) {
	out := make([]byte, length)
	limit := big.NewInt(int64(len(roomCodeAlphabet)))
	for i := range out {
		n, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", err
		}
		out[i] = roomCodeAlphabet[n.Int64()]
	}
	return string(out), nil
}

func newSID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return hex.EncodeToString([]byte(time.Now().String()))
	}
	return hex.EncodeToString(buf)
}

func firstEight(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// errorMessage renders a handshake failure; the connect-error payload carries only a message,
// so this is the entire diagnostic a client receives.
func errorMessage(err error) string {
	if err == nil {
		return "Invalid token."
	}
	return err.Error()
}

// readLimited reads a request body up to limit bytes, so a hostile client cannot over-allocate
// the socket layer.
func readLimited(r *http.Request, limit int64) (string, error) {
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(data)) > limit {
		return "", errors.New("syncplay: payload too large")
	}
	return string(data), nil
}

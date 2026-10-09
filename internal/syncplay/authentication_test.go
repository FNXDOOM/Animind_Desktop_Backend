package syncplay

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"animind-backend-go/internal/auth"
	"animind-backend-go/internal/config"
)

type acceptingSocketAuthenticator struct{}

func (acceptingSocketAuthenticator) Authenticate(_ context.Context, token string) (*auth.Principal, error) {
	if token != "socket-token" {
		return nil, auth.ErrTokenInvalid
	}
	return &auth.Principal{UserID: "user-1", Username: "socket-user", Kind: "web"}, nil
}

func newAuthenticationTestServer(t *testing.T) (*Hub, *httptest.Server) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/db")
	t.Setenv("DESKTOP_TOKEN_SIGNING_KEY", "0123456789012345678901234567890123456789")
	t.Setenv("STORAGE_MODE", "local")
	t.Setenv("LOCAL_STORAGE_PATH", t.TempDir())

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := NewHub(cfg, logger, nil, acceptingSocketAuthenticator{})
	server := httptest.NewServer(http.HandlerFunc(hub.serveSocket))
	t.Cleanup(func() {
		hub.CloseAll()
		server.Close()
	})
	return hub, server
}

func TestWebSocketApplicationEventsRequireAuthentication(t *testing.T) {
	_, server := newAuthenticationTestServer(t)
	url := "ws" + strings.TrimPrefix(server.URL, "http") + SocketPath + "?EIO=4&transport=websocket"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	if frame := readAuthenticationTestFrame(t, conn, time.Second); !strings.HasPrefix(frame, "0") {
		t.Fatalf("expected Engine.IO OPEN, got %q", frame)
	}

	// This event must not reach handleTimeSync before the CONNECT succeeds.
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`42["timesync_ping",{}]`)); err != nil {
		t.Fatalf("send unauthenticated event: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("an unauthenticated application event produced a socket response")
	}
	_ = conn.Close()

	// Authenticate a fresh connection after proving the pre-auth event was ignored. A client
	// never sends application events before CONNECT, so this also keeps the regression focused
	// on the normal authenticated wire protocol.
	conn, _, err = websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial authenticated websocket: %v", err)
	}
	defer conn.Close()
	if frame := readAuthenticationTestFrame(t, conn, time.Second); !strings.HasPrefix(frame, "0") {
		t.Fatalf("expected Engine.IO OPEN, got %q", frame)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`40{"token":"socket-token"}`)); err != nil {
		t.Fatalf("send CONNECT: %v", err)
	}
	if frame := readAuthenticationTestFrame(t, conn, time.Second); !strings.HasPrefix(frame, "40") {
		t.Fatalf("expected Socket.IO CONNECT, got %q", frame)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`42["timesync_ping",{}]`)); err != nil {
		t.Fatalf("send authenticated event: %v", err)
	}
	if frame := readAuthenticationTestFrame(t, conn, time.Second); !strings.Contains(frame, `timesync_pong`) {
		t.Fatalf("authenticated event was not processed, got %q", frame)
	}
}

func TestPollingApplicationEventsRequireAuthentication(t *testing.T) {
	hub, server := newAuthenticationTestServer(t)
	base := server.URL + SocketPath + "?EIO=4&transport=polling"
	var open string
	for attempt := 0; attempt < 10 && !strings.HasPrefix(open, "0"); attempt++ {
		// The server queues the initial OPEN immediately; retry the rare case where a zero-wait
		// polling drain observes its deadline before the queue receive.
		open = pollingAuthenticationRequest(t, http.MethodGet, base, "")
	}
	if !strings.HasPrefix(open, "0") {
		t.Fatalf("expected Engine.IO OPEN, got %q", open)
	}
	openBody := strings.TrimPrefix(open, "0")
	var payload struct {
		SID string `json:"sid"`
	}
	if err := json.Unmarshal([]byte(openBody), &payload); err != nil {
		t.Fatalf("decode OPEN: %v", err)
	}
	if payload.SID == "" {
		t.Fatal("OPEN did not include a polling session id")
	}
	pollURL := base + "&sid=" + payload.SID

	// Polling POSTs use the same Engine.IO message framing as WebSocket messages.
	pollingAuthenticationRequest(t, http.MethodPost, pollURL, `42["timesync_ping",{}]`)
	peer := hub.lookupPeer(payload.SID)
	if peer == nil {
		t.Fatal("polling session disappeared after an unauthenticated event")
	}
	select {
	case frame := <-peer.out:
		t.Fatalf("an unauthenticated polling event produced %q", frame)
	default:
	}

	pollingAuthenticationRequest(t, http.MethodPost, pollURL, `40{"token":"socket-token"}`)
	if frame := pollingAuthenticationRequest(t, http.MethodGet, pollURL, ""); !strings.HasPrefix(frame, "40") {
		t.Fatalf("expected Socket.IO CONNECT, got %q", frame)
	}
	pollingAuthenticationRequest(t, http.MethodPost, pollURL, `42["timesync_ping",{}]`)
	if frame := pollingAuthenticationRequest(t, http.MethodGet, pollURL, ""); !strings.Contains(frame, `timesync_pong`) {
		t.Fatalf("authenticated polling event was not processed, got %q", frame)
	}
}

func readAuthenticationTestFrame(t *testing.T, conn *websocket.Conn, timeout time.Duration) string {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read socket frame: %v", err)
	}
	return string(raw)
}

func pollingAuthenticationRequest(t *testing.T, method, url, body string) string {
	t.Helper()
	request, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new polling request: %v", err)
	}
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "text/plain; charset=UTF-8")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("polling %s: %v", method, err)
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read polling %s: %v", method, err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("polling %s status = %d, body = %q", method, response.StatusCode, contents)
	}
	return string(contents)
}

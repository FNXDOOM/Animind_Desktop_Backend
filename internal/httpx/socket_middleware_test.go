package httpx

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
	"animind-backend-go/internal/syncplay"
)

// The socket has to survive the whole middleware chain, so these dial through Router(): a bare
// handler test, as the protocol suite uses, misses a middleware that breaks the upgrade.

// stubAuthenticator accepts one token; auth is the hub's concern, not what these tests.
type stubAuthenticator struct{}

func (stubAuthenticator) Authenticate(_ context.Context, token string) (*auth.Principal, error) {
	if token != "socket-token" {
		return nil, auth.ErrTokenInvalid
	}
	return &auth.Principal{UserID: "user-1", Username: "middleware", Kind: "web"}, nil
}

func socketServer(t *testing.T) *httptest.Server {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/db")
	t.Setenv("DESKTOP_TOKEN_SIGNING_KEY", "0123456789012345678901234567890123456789")
	t.Setenv("STORAGE_MODE", "local")
	t.Setenv("LOCAL_STORAGE_PATH", t.TempDir())
	t.Setenv("FRONTEND_URL", "https://fnxdoom.in,http://localhost:5173")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	slog.SetDefault(logger)

	hub := syncplay.NewHub(cfg, logger, nil, stubAuthenticator{})
	server := New(Deps{Config: cfg, Logger: logger, Hub: hub})
	recorder := httptest.NewServer(server.Router())
	t.Cleanup(func() {
		hub.CloseAll()
		recorder.Close()
	})
	return recorder
}

// dialSocket upgrades and consumes the Engine.IO OPEN packet, which every Socket.IO handshake
// must read before it can send; it returns the connection and that OPEN body.
func dialSocket(t *testing.T, target *httptest.Server) (*websocket.Conn, string) {
	t.Helper()
	url := "ws" + strings.TrimPrefix(target.URL, "http") + syncplay.SocketPath + "?EIO=4&transport=websocket"
	request := http.Header{}
	request.Set("Origin", "https://fnxdoom.in")
	conn, response, err := websocket.DefaultDialer.Dial(url, request)
	if err != nil {
		if response != nil {
			t.Fatalf("upgrade through the middleware chain: %v (status %d %s)",
				err, response.StatusCode, response.Status)
		}
		t.Fatalf("upgrade through the middleware chain: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	frame := readFrame(t, conn, 3*time.Second)
	if !strings.HasPrefix(frame, "0") {
		t.Fatalf("first packet should be Engine.IO OPEN, got %q", frame)
	}
	return conn, strings.TrimPrefix(frame, "0")
}

func readFrame(t *testing.T, conn *websocket.Conn, within time.Duration) string {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(within))
	_, frame, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read a frame within %v: %v", within, err)
	}
	return string(frame)
}

func TestSocketUpgradeSurvivesTheMiddlewareChain(t *testing.T) {
	server := socketServer(t)
	conn, openBody := dialSocket(t, server)

	// A middleware that rewrites bodies would corrupt the OPEN payload asserted below.
	var open struct {
		SID          string `json:"sid"`
		PingInterval int    `json:"pingInterval"`
		PingTimeout  int    `json:"pingTimeout"`
	}
	if err := json.Unmarshal([]byte(openBody), &open); err != nil {
		t.Fatalf("OPEN is not the documented payload: %v (%s)", err, openBody)
	}
	if open.SID == "" {
		t.Error("OPEN must carry a session id, or a client cannot resume after a drop")
	}
	if open.PingInterval <= 0 || open.PingTimeout <= 0 {
		t.Errorf("OPEN must state the liveness window, got %+v", open)
	}

	// A CONNECT with a good token must be answered with CONNECT, not a silent close.
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`40{"token":"socket-token"}`)); err != nil {
		t.Fatalf("send CONNECT: %v", err)
	}
	if frame := readFrame(t, conn, 3*time.Second); !strings.HasPrefix(frame, "40") {
		t.Errorf("expected a Socket.IO CONNECT ack, got %q", frame)
	}
}

func TestSocketRefusesABadTokenWithoutKillingTheConnection(t *testing.T) {
	server := socketServer(t)
	conn, _ := dialSocket(t, server)

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`40{"token":"not-a-token"}`)); err != nil {
		t.Fatalf("send CONNECT: %v", err)
	}
	frame := readFrame(t, conn, 3*time.Second)
	if !strings.HasPrefix(frame, "44") {
		t.Fatalf("a refused handshake should answer CONNECT_ERROR (44...), got %q", frame)
	}
	if !strings.Contains(frame, "message") {
		t.Errorf("a client shows this string to a user; it should say something. got %s", frame)
	}
	var envelope struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(frame, "44")), &envelope); err != nil {
		t.Fatalf("CONNECT_ERROR body is not the documented shape: %v (%s)", err, frame)
	}
	if envelope.Message == "" {
		t.Error("the refusal must carry a reason")
	}
}

func TestSocketRejectsADisallowedOrigin(t *testing.T) {
	target := socketServer(t)

	url := "ws" + strings.TrimPrefix(target.URL, "http") + syncplay.SocketPath + "?EIO=4&transport=websocket"
	request := http.Header{}
	request.Set("Origin", "https://evil.example")
	conn, response, err := websocket.DefaultDialer.Dial(url, request)
	if err == nil {
		conn.Close()
		t.Fatal("a disallowed origin was upgraded anyway")
	}
	if response == nil {
		t.Fatal("the refusal should carry an HTTP status")
	}
	if response.StatusCode == http.StatusInternalServerError {
		t.Fatalf("a refused origin must answer as a refusal, not a crash: %s", response.Status)
	}
}

package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Token prefixes are public wire format: the player distinguishes its credentials and Kind()
// routes verification by them, so they cannot change without breaking installed clients.
const (
	// DesktopAccessPrefix marks a desktop access token (HMAC-signed, self-contained).
	DesktopAccessPrefix = "adk_"
	// DesktopRefreshPrefix marks a desktop refresh token (opaque, rotated on use).
	DesktopRefreshPrefix = "adr_"
	// WebSessionPrefix marks a site session token (opaque, hashed at rest).
	WebSessionPrefix = "as_"
	// accessTokenVersion is the payload layout version, currently "v1".
	accessTokenVersion = "v1"
)

// TokenKind identifies which verifier accepts a bearer token.
type TokenKind int

const (
	// TokenUnknown is returned when no verifier recognizes the credential.
	TokenUnknown TokenKind = iota
	// TokenDesktopAccess is an adk_ desktop access token.
	TokenDesktopAccess
	// TokenDesktopRefresh is an adr_ desktop refresh token.
	TokenDesktopRefresh
	// TokenWebSession is an as_ site session token.
	TokenWebSession
)

// Errors returned by token verification. All of them map to 401 at the HTTP edge.
var (
	ErrTokenMalformed  = errors.New("token is malformed")
	ErrTokenSignature  = errors.New("token signature does not match")
	ErrTokenExpired    = errors.New("token has expired")
	ErrTicketMalformed = errors.New("stream ticket is malformed")
	ErrTicketExpired   = errors.New("stream ticket has expired")
)

// Kind reports which verifier accepts a raw bearer token.
func Kind(token string) TokenKind {
	switch {
	case strings.HasPrefix(token, DesktopAccessPrefix):
		return TokenDesktopAccess
	case strings.HasPrefix(token, DesktopRefreshPrefix):
		return TokenDesktopRefresh
	case strings.HasPrefix(token, WebSessionPrefix):
		return TokenWebSession
	default:
		return TokenUnknown
	}
}

// IsDesktopAccessToken reports whether a bearer token is an adk_ desktop token.
func IsDesktopAccessToken(token string) bool {
	return strings.HasPrefix(token, DesktopAccessPrefix)
}

// Signer issues and validates the backend's own tokens — desktop access tokens and playback
// tickets — with the single DESKTOP_TOKEN_SIGNING_KEY secret. Payloads are tamper-evident, not
// encrypted.
type Signer struct {
	key []byte
}

// NewSigner builds a Signer from a configured secret.
func NewSigner(secret string) (*Signer, error) {
	if len(secret) < 32 {
		return nil, errors.New("auth: signing key must be at least 32 characters")
	}
	return &Signer{key: []byte(secret)}, nil
}

// DesktopClaims is the decoded body of a desktop access token.
type DesktopClaims struct {
	UserID    string
	SessionID string
	ExpiresAt int64 // unix seconds
}

// IssueDesktopAccessToken signs a self-contained desktop access token. Installed clients parse
// the exact layout, so it cannot change:
//
//	payload = "v1." + userID + "." + sessionID + "." + expUnixSeconds
//	token   = "adk_" + base64url(payload) + "." + base64url(HMAC-SHA256(key, payload))
//
// The expiry authorizes a request without a database read; the pinned session row still allows
// immediate revocation.
func (s *Signer) IssueDesktopAccessToken(userID, sessionID string, expiresAt time.Time) string {
	payload := strings.Join([]string{
		accessTokenVersion,
		userID,
		sessionID,
		strconv.FormatInt(expiresAt.Unix(), 10),
	}, ".")
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return DesktopAccessPrefix + encoded + "." + s.sign(payload)
}

// VerifyDesktopAccessToken checks the signature and expiry of a desktop access token.
// It performs no database lookup; use the session repository to confirm the token has
// not been revoked.
func (s *Signer) VerifyDesktopAccessToken(token string) (*DesktopClaims, error) {
	if !IsDesktopAccessToken(token) {
		return nil, ErrTokenMalformed
	}
	body := strings.TrimPrefix(token, DesktopAccessPrefix)
	// Split at the LAST dot: base64url never emits one, so it is the signature separator even for
	// a token carrying a dotted prefix.
	lastDot := strings.LastIndex(body, ".")
	if lastDot <= 0 || lastDot == len(body)-1 {
		return nil, ErrTokenMalformed
	}
	encoded, signature := body[:lastDot], body[lastDot+1:]
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, ErrTokenMalformed
	}
	payload := string(raw)
	if !s.verify(payload, signature) {
		return nil, ErrTokenSignature
	}

	parts := strings.Split(payload, ".")
	if len(parts) != 4 || parts[0] != accessTokenVersion {
		return nil, ErrTokenMalformed
	}
	exp, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return nil, ErrTokenMalformed
	}
	if exp <= time.Now().Unix() {
		return nil, ErrTokenExpired
	}
	return &DesktopClaims{UserID: parts[1], SessionID: parts[2], ExpiresAt: exp}, nil
}

// HashToken returns the lowercase hex SHA-256 of a token. Tokens are stored only as this hash,
// so a database leak yields no usable credential.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NewDesktopRefreshToken returns adr_ plus 48 random bytes, base64url encoded.
func NewDesktopRefreshToken() (string, error) {
	buf := make([]byte, 48)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: refresh token: %w", err)
	}
	return DesktopRefreshPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// NewWebSessionToken returns as_ plus 32 random bytes, base64url encoded.
func NewWebSessionToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: session token: %w", err)
	}
	return WebSessionPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// NewOpaqueToken returns a base64url token of n random bytes, without a prefix.
func NewOpaqueToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: opaque token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// StreamTicketPayload is the signed body of a playback ticket. The JSON key names are
// load-bearing: the shipped desktop parser decodes exactly these after base64url.
type StreamTicketPayload struct {
	EpisodeID string `json:"episodeId"`
	// At is the requested audio track index, omitted when none was requested.
	At *int `json:"at,omitempty"`
	// CM is the client mode: "n" for the native player, "b" for a browser.
	CM string `json:"cm"`
	// Exp is the expiry as epoch milliseconds.
	Exp int64 `json:"exp"`
}

// ClientNative and ClientBrowser are the two playback ticket modes.
const (
	ClientNative  = "n"
	ClientBrowser = "b"
)

// IssueStreamTicket creates a ticket granting stream access to one episode for a bounded time,
// so the long-lived access token never lands in a media player's request log.
func (s *Signer) IssueStreamTicket(episodeID string, audioTrackIndex *int, clientMode string, expiresAt time.Time) (string, error) {
	payload := StreamTicketPayload{
		EpisodeID: episodeID,
		At:        audioTrackIndex,
		CM:        clientMode,
		Exp:       expiresAt.UnixMilli(),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("auth: marshal ticket: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	return encoded + "." + s.sign(encoded), nil
}

// VerifyStreamTicket validates a playback ticket for a specific episode. Tickets carry no
// identity and scope one episode, so a leaked one cannot browse the library.
func (s *Signer) VerifyStreamTicket(ticket, episodeID string) (*StreamTicketPayload, error) {
	idx := strings.LastIndex(ticket, ".")
	if idx <= 0 || idx == len(ticket)-1 {
		return nil, ErrTicketMalformed
	}
	encoded, signature := ticket[:idx], ticket[idx+1:]
	if !s.verify(encoded, signature) {
		return nil, ErrTicketMalformed
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, ErrTicketMalformed
	}
	var payload StreamTicketPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, ErrTicketMalformed
	}
	if payload.EpisodeID != episodeID {
		return nil, ErrTicketMalformed
	}
	if payload.Exp <= time.Now().UnixMilli() {
		return nil, ErrTicketExpired
	}
	return &payload, nil
}

func (s *Signer) sign(payload string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Signer) verify(payload, signature string) bool {
	want, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(payload))
	return hmac.Equal(mac.Sum(nil), want)
}

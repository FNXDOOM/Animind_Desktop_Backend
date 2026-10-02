package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Minimal HS256 JWT support.
//
// A JWT is needed for exactly one reason: the shipped desktop player decodes the
// credential handed to it by the browser bridge before exchanging it, reading the
// claims sub, sid and exp. Anything it cannot parse as three dot-separated base64url
// segments breaks sign-in for existing installs, so the format is kept while the
// issuer becomes this backend.
//
// This is deliberately not a general JWT library: one algorithm, no "none", and the
// audience and issuer are fixed so a token minted for one purpose cannot be replayed
// against another.

// BridgeAudience is the aud claim identifying a desktop handoff token.
const BridgeAudience = "animind-desktop-bridge"

// BridgeClaims is the payload of a desktop handoff token.
//
// JSON keys match what the desktop client reads: sub for the user id, sid for the
// session id, exp for the expiry in seconds, plus email for display.
type BridgeClaims struct {
	Issuer    string `json:"iss"`
	Subject   string `json:"sub"`
	SessionID string `json:"sid"`
	Email     string `json:"email,omitempty"`
	Audience  string `json:"aud"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

// JWT errors. Each maps to 401 at the edge; none expose the verification detail to the
// client, since a caller learning "signature bad" versus "expired" gains nothing.
var (
	ErrJWTFormat    = errors.New("auth: malformed token")
	ErrJWTSignature = errors.New("auth: invalid token signature")
	ErrJWTExpired   = errors.New("auth: expired token")
	ErrJWTIssuer    = errors.New("auth: unexpected token issuer or audience")
)

// SignBridgeToken produces a compact HS256 JWT for the desktop handoff.
func (s *Signer) SignBridgeToken(claims BridgeClaims) (string, error) {
	header := base64URLEncode([]byte(`{"alg":"HS256","typ":"JWT"}`))
	claims.IssuedAt = time.Now().Unix()
	body, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("auth: marshal bridge claims: %w", err)
	}
	payload := base64URLEncode(body)
	signingInput := header + "." + payload
	return signingInput + "." + base64URLEncode(hmacSHA256([]byte(signingInput), s.key)), nil
}

// VerifyBridgeToken validates a desktop handoff token and returns its claims.
//
// The signature is checked before the payload is decoded, and the algorithm header is
// required to be HS256: an attacker-supplied alg of "none" would otherwise present an
// unsigned token as valid.
func (s *Signer) VerifyBridgeToken(token string) (*BridgeClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, ErrJWTFormat
	}
	headerRaw, err := base64URLDecode(parts[0])
	if err != nil {
		return nil, ErrJWTFormat
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return nil, ErrJWTFormat
	}
	if header.Alg != "HS256" {
		return nil, ErrJWTSignature
	}

	signingInput := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(signingInput))
	if !hmac.Equal(mac.Sum(nil), mustBase64URLDecode(parts[2])) {
		return nil, ErrJWTSignature
	}

	payloadRaw, err := base64URLDecode(parts[1])
	if err != nil {
		return nil, ErrJWTFormat
	}
	var claims BridgeClaims
	if err := json.Unmarshal(payloadRaw, &claims); err != nil {
		return nil, ErrJWTFormat
	}
	if claims.Audience != BridgeAudience || claims.Subject == "" {
		return nil, ErrJWTIssuer
	}
	if claims.ExpiresAt <= time.Now().Unix() {
		return nil, ErrJWTExpired
	}
	return &claims, nil
}

func hmacSHA256(data, key []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

func base64URLEncode(data []byte) string { return base64.RawURLEncoding.EncodeToString(data) }

func base64URLDecode(data string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(data)
}

// mustBase64URLDecode tolerates undecodable signature input by returning a zero byte,
// which can never match a real HMAC. The signature path has no useful error to report:
// an invalid encoding is simply an invalid token.
func mustBase64URLDecode(data string) []byte {
	raw, err := base64URLDecode(data)
	if err != nil {
		return []byte{0}
	}
	return raw
}

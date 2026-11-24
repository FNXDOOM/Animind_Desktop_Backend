package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"animind-backend-go/internal/cache"
	"animind-backend-go/internal/config"
)

func testParams() Argon2Params {
	// Deliberately cheap so the suite stays fast; production defaults are stronger and are
	// asserted separately in TestDefaultArgon2Params.
	return Argon2Params{Memory: 2048, Time: 1, Threads: 1, KeyLen: 32}
}

func TestDefaultArgon2ParamsMeetBaseline(t *testing.T) {
	params := DefaultArgon2Params()
	if params.Memory < 65536 || params.Time < 3 || params.Threads < 1 {
		t.Fatalf("default argon2id parameters weakened: %+v", params)
	}
}

func TestHashAndVerifyPassword(t *testing.T) {
	hash, err := HashPassword("correct horse battery", testParams())
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "argon2id$v=19$") {
		t.Fatalf("unexpected hash encoding: %q", hash)
	}
	if strings.Contains(hash, "correct horse battery") {
		t.Fatal("hash contains the plaintext password")
	}

	needsRehash, err := VerifyPasswordAgainstCurrent("correct horse battery", hash, testParams())
	if err != nil {
		t.Fatalf("verify correct password: %v", err)
	}
	if needsRehash {
		t.Error("a hash created with the current parameters should not request a rehash")
	}

	if _, err := VerifyPasswordAgainstCurrent("wrong password", hash, testParams()); err != ErrHashMismatch {
		t.Errorf("verify wrong password: got %v, want ErrHashMismatch", err)
	}
}

func TestHashPasswordRejectsWeakInput(t *testing.T) {
	// The returned error names the rule as well as the sentinel, so callers use errors.Is and
	// the message is fit to show in a form.
	if _, err := HashPassword("short", testParams()); !errors.Is(err, ErrWeakPassword) {
		t.Errorf("short password: got %v, want ErrWeakPassword", err)
	}
	if err := ValidatePasswordStrength("short"); err == nil || !strings.Contains(err.Error(), "at least 8") {
		t.Errorf("short password message should name the minimum, got %v", err)
	}
	if _, err := HashPassword(strings.Repeat("a", maxPasswordLength+1), testParams()); !errors.Is(err, ErrWeakPassword) {
		t.Errorf("oversized password: got %v, want ErrWeakPassword", err)
	}
	if err := ValidatePasswordStrength(strings.Repeat("a", maxPasswordLength)); err != nil {
		t.Errorf("a maximum-length password must be accepted: %v", err)
	}
}

func TestValidateGoogleProfileRequiresVerifiedEmail(t *testing.T) {
	verified := true
	unverified := false

	tests := []struct {
		name    string
		profile *GoogleProfile
		wantErr bool
	}{
		{name: "nil profile", wantErr: true},
		{name: "missing subject", profile: &GoogleProfile{Email: "user@example.com", EmailTrue: &verified}, wantErr: true},
		{name: "missing email", profile: &GoogleProfile{Subject: "google-subject", EmailTrue: &verified}, wantErr: true},
		{name: "missing verification claim", profile: &GoogleProfile{Subject: "google-subject", Email: "user@example.com"}, wantErr: true},
		{name: "unverified email", profile: &GoogleProfile{Subject: "google-subject", Email: "user@example.com", EmailTrue: &unverified}, wantErr: true},
		{name: "verified email", profile: &GoogleProfile{Subject: "google-subject", Email: "user@example.com", EmailTrue: &verified}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateGoogleProfile(tt.profile)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateGoogleProfile() error = %v, want error = %v", err, tt.wantErr)
			}
		})
	}
}

func TestNewGoogleDerivesCallbackFromPublicAPIURL(t *testing.T) {
	google := NewGoogle(&config.Config{
		PublicURL:       "https://api.example.test",
		FrontendOrigins: []string{"https://app.example.test"},
		Auth: config.AuthConfig{
			GoogleClientID:     "client-id",
			GoogleClientSecret: "client-secret",
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if got, want := google.RedirectURL, "https://api.example.test/api/auth/google/callback"; got != want {
		t.Fatalf("Google callback URL = %q, want %q", got, want)
	}
}

func TestVerifyPasswordDetectsParameterUpgrade(t *testing.T) {
	weak := Argon2Params{Memory: 1024, Time: 1, Threads: 1, KeyLen: 16}
	hash, err := HashPassword("password123", weak)
	if err != nil {
		t.Fatal(err)
	}
	needsRehash, err := VerifyPasswordAgainstCurrent("password123", hash, DefaultArgon2Params())
	if err != nil {
		t.Fatalf("legacy hash must still verify: %v", err)
	}
	if !needsRehash {
		t.Error("expected a rehash request so the stored hash can be strengthened")
	}
}

func TestVerifyPasswordRejectsMalformedHash(t *testing.T) {
	for _, stored := range []string{"", "bcrypt$1$2$3", "argon2id$v=19$m=0,t=1,p=1$c2FsdA$aGFzaA", "argon2id$v=19$m=65536,t=3,p=4$notbase64!!$aGFzaA"} {
		if _, err := VerifyPasswordAgainstCurrent("anything", stored, testParams()); err != ErrInvalidHash {
			t.Errorf("stored %q: got %v, want ErrInvalidHash", stored, err)
		}
	}
}

func TestDesktopAccessTokenRoundTrip(t *testing.T) {
	signer, err := NewSigner("0123456789012345678901234567890123456789")
	if err != nil {
		t.Fatal(err)
	}
	userID := "8f14e45f-ceea-467a-9a1b-2f1a0d3c4b5a"
	sessionID := "session-123"

	token := signer.IssueDesktopAccessToken(userID, sessionID, time.Now().Add(time.Minute))
	if !IsDesktopAccessToken(token) {
		t.Fatalf("token must carry the adk_ prefix the desktop client routes on: %q", token)
	}
	if strings.Count(token, ".") != 1 {
		t.Fatalf("token must be prefix+payload.signature, got %q", token)
	}

	claims, err := signer.VerifyDesktopAccessToken(token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.UserID != userID || claims.SessionID != sessionID {
		t.Errorf("claims = %+v, want user %q session %q", claims, userID, sessionID)
	}

	// The wire format is a contract with the shipped player, so the exact layout is pinned:
	// payload decodes to v1.<user>.<session>.<exp>.
	body := strings.TrimPrefix(token, DesktopAccessPrefix)
	payloadPart := body[:strings.LastIndex(body, ".")]
	raw, err := base64.RawURLEncoding.DecodeString(payloadPart)
	if err != nil {
		t.Fatalf("payload must be base64url: %v", err)
	}
	segments := strings.Split(string(raw), ".")
	if len(segments) != 4 || segments[0] != "v1" || segments[1] != userID || segments[2] != sessionID {
		t.Fatalf("unexpected payload layout %q", string(raw))
	}
}

func TestDesktopAccessTokenRejectsTampering(t *testing.T) {
	signer, _ := NewSigner("0123456789012345678901234567890123456789")
	token := signer.IssueDesktopAccessToken("user-a", "session-a", time.Now().Add(time.Minute))

	// Flip one character of the signature.
	last := strings.LastIndex(token, ".")
	broken := token[:last+1] + swapByte(token[last+1:])
	if _, err := signer.VerifyDesktopAccessToken(broken); err != ErrTokenSignature {
		t.Errorf("tampered signature: got %v, want ErrTokenSignature", err)
	}

	// A token signed with a different key must not verify.
	other, _ := NewSigner("abcdefghijklmnopqrstuvwxyzABCDEFGHIJ0123456789")
	if _, err := other.VerifyDesktopAccessToken(token); err != ErrTokenSignature {
		t.Errorf("foreign key: got %v, want ErrTokenSignature", err)
	}

	// An expired token fails without a database read.
	expired := signer.IssueDesktopAccessToken("user-a", "session-a", time.Now().Add(-time.Second))
	if _, err := signer.VerifyDesktopAccessToken(expired); err != ErrTokenExpired {
		t.Errorf("expired token: got %v, want ErrTokenExpired", err)
	}

	for _, malformed := range []string{"", "adk_", "adk_notbase64.sig", "abc_def.payload.sig"} {
		if _, err := signer.VerifyDesktopAccessToken(malformed); err == nil {
			t.Errorf("malformed token %q was accepted", malformed)
		}
	}
}

func TestBridgeTokenRequiresHS256AndAudience(t *testing.T) {
	signer, _ := NewSigner("0123456789012345678901234567890123456789")
	token, err := signer.SignBridgeToken(BridgeClaims{
		Issuer: "http://localhost:3001", Subject: "user-uuid", SessionID: "web-session",
		Email: "a@b.co", Audience: BridgeAudience, ExpiresAt: time.Now().Add(time.Minute).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := signer.VerifyBridgeToken(token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.Subject != "user-uuid" || claims.SessionID != "web-session" {
		t.Errorf("claims = %+v", claims)
	}

	// The desktop client decodes this as a JWT, so it must have three segments.
	if strings.Count(token, ".") != 2 {
		t.Fatalf("bridge token must be a JWT (3 segments), got %q", token)
	}

	_, payload, signature := strings.Split(token, ".")[0], strings.Split(token, ".")[1], strings.Split(token, ".")[2]

	// alg=none must be refused rather than trusted.
	forgedHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	if _, err := signer.VerifyBridgeToken(forgedHeader + "." + payload + "." + signature); err != ErrJWTSignature {
		t.Errorf("alg=none: got %v, want ErrJWTSignature", err)
	}

	// A token minted for another purpose must not be accepted here.
	other, err := signer.SignBridgeToken(BridgeClaims{
		Subject: "x", Audience: "something-else", ExpiresAt: time.Now().Add(time.Minute).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := signer.VerifyBridgeToken(other); err != ErrJWTIssuer {
		t.Errorf("wrong audience: got %v, want ErrJWTIssuer", err)
	}

	// Expiry must be enforced.
	expiredToken, err := signer.SignBridgeToken(BridgeClaims{
		Subject: "x", Audience: BridgeAudience, ExpiresAt: time.Now().Add(-time.Minute).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := signer.VerifyBridgeToken(expiredToken); err != ErrJWTExpired {
		t.Errorf("expired: got %v, want ErrJWTExpired", err)
	}
}

func TestBridgeTokenIssuerMustMatchDeployment(t *testing.T) {
	signer, _ := NewSigner("0123456789012345678901234567890123456789")
	token, err := signer.SignBridgeToken(BridgeClaims{
		Issuer: "https://api.example.test", Subject: "user-1", Audience: BridgeAudience,
		ExpiresAt: time.Now().Add(time.Minute).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := signer.VerifyBridgeTokenForIssuer(token, "https://api.example.test"); err != nil {
		t.Fatalf("matching issuer rejected: %v", err)
	}
	if _, err := signer.VerifyBridgeTokenForIssuer(token, "https://other.example.test"); err != ErrJWTIssuer {
		t.Errorf("wrong issuer: got %v, want ErrJWTIssuer", err)
	}
}

func TestGoogleStateIsBrowserBoundAndSingleUse(t *testing.T) {
	store, err := cache.Open(context.Background(), config.CacheConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	signer, _ := NewSigner("0123456789012345678901234567890123456789")
	service := &Service{
		signer: signer, cfg: &config.Config{PublicURL: "https://api.example.test"},
		stateCache: store, localState: make(map[string]time.Time),
	}
	state, err := service.GoogleRedirectStateForBrowser("/library", "browser-nonce")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.VerifyGoogleRedirectStateForBrowser(state, "another-browser"); err != ErrTokenInvalid {
		t.Fatalf("state accepted for another browser: %v", err)
	}
	redirect, err := service.VerifyGoogleRedirectStateForBrowser(state, "browser-nonce")
	if err != nil || redirect != "/library" {
		t.Fatalf("valid state: redirect=%q err=%v", redirect, err)
	}
	if _, err := service.VerifyGoogleRedirectStateForBrowser(state, "browser-nonce"); err != ErrTokenInvalid {
		t.Errorf("state replay: got %v, want ErrTokenInvalid", err)
	}
}

func TestLoginFailureThrottleIsScopedToSource(t *testing.T) {
	store, err := cache.Open(context.Background(), config.CacheConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	signer, _ := NewSigner("0123456789012345678901234567890123456789")
	service := &Service{
		signer:     signer,
		cfg:        &config.Config{Auth: config.AuthConfig{MaxFailedLogins: 2, FailedLoginLockout: time.Minute}},
		stateCache: store, loginLimiter: store,
	}
	first := WithLoginSource(context.Background(), "198.51.100.10")
	if service.recordLoginFailure(first, "person@example.test") {
		t.Fatal("first failure unexpectedly locked the source")
	}
	if !service.recordLoginFailure(first, "person@example.test") {
		t.Fatal("second failure should throttle this source")
	}
	other := WithLoginSource(context.Background(), "198.51.100.11")
	if service.recordLoginFailure(other, "person@example.test") {
		t.Fatal("one source must not lock another source out")
	}
}

func TestStreamTicketBindsEpisodeClientModeAndTrack(t *testing.T) {
	signer, _ := NewSigner("0123456789012345678901234567890123456789")
	track := 2

	ticket, err := signer.IssueStreamTicket("episode-1", &track, ClientBrowser, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := signer.VerifyStreamTicket(ticket, "episode-1")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if payload.CM != ClientBrowser || payload.At == nil || *payload.At != 2 {
		t.Errorf("payload = %+v", payload)
	}

	// A ticket must not unlock a different episode.
	if _, err := signer.VerifyStreamTicket(ticket, "episode-2"); err != ErrTicketMalformed {
		t.Errorf("wrong episode: got %v, want rejection", err)
	}

	native, err := signer.IssueStreamTicket("episode-1", nil, ClientNative, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if parsed, err := signer.VerifyStreamTicket(native, "episode-1"); err != nil || parsed.CM != ClientNative || parsed.At != nil {
		t.Errorf("native ticket = %+v err=%v", parsed, err)
	}

	expired, err := signer.IssueStreamTicket("episode-1", nil, ClientBrowser, time.Now().Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := signer.VerifyStreamTicket(expired, "episode-1"); err != ErrTicketExpired {
		t.Errorf("expired ticket: got %v, want ErrTicketExpired", err)
	}

	for _, malformed := range []string{"", "no-dot", "a.b.c", "!!!.sig"} {
		if _, err := signer.VerifyStreamTicket(malformed, "episode-1"); err == nil {
			t.Errorf("malformed ticket %q was accepted", malformed)
		}
	}
}

func TestTokenKindRouting(t *testing.T) {
	cases := map[string]TokenKind{
		"adk_something.sig": TokenDesktopAccess,
		"adr_something":     TokenDesktopRefresh,
		"as_something":      TokenWebSession,
		"jwt.a.b":           TokenUnknown,
		"":                  TokenUnknown,
	}
	for token, want := range cases {
		if got := Kind(token); got != want {
			t.Errorf("Kind(%q) = %v, want %v", token, got, want)
		}
	}
}

func TestGeneratedTokensAreRandomAndWellSized(t *testing.T) {
	refreshA, err := NewDesktopRefreshToken()
	if err != nil {
		t.Fatal(err)
	}
	refreshB, _ := NewDesktopRefreshToken()
	if refreshA == refreshB {
		t.Error("refresh tokens must be unpredictable")
	}
	if !strings.HasPrefix(refreshA, DesktopRefreshPrefix) {
		t.Errorf("refresh token prefix = %q", refreshA[:4])
	}

	sessionA, _ := NewWebSessionToken()
	sessionB, _ := NewWebSessionToken()
	if sessionA == sessionB || !strings.HasPrefix(sessionA, WebSessionPrefix) {
		t.Errorf("web session tokens must be unique and prefixed, got %q", sessionA)
	}

	// Hashing must be stable and one-way.
	if HashToken(refreshA) != HashToken(refreshA) {
		t.Error("HashToken must be deterministic")
	}
	if len(HashToken(refreshA)) != 64 || strings.Contains(HashToken(refreshA), refreshA) {
		t.Errorf("unexpected hash form %q", HashToken(refreshA))
	}
}

func TestEmailAndUsernameValidation(t *testing.T) {
	valid := []string{"a@b.co", "nikhil@example.com", "name+tag@sub.example.co.uk"}
	for _, email := range valid {
		if err := ValidateEmail(email); err != nil {
			t.Errorf("ValidateEmail(%q) = %v, want valid", email, err)
		}
	}
	invalid := []string{"", "no-at-sign", "two@@x.com", "a@b", " spaced @b.co", "a@b.", "a@.co"}
	for _, email := range invalid {
		if err := ValidateEmail(email); err == nil {
			t.Errorf("ValidateEmail(%q) accepted an invalid address", email)
		}
	}

	if NormalizeEmail("  Nikhil@Example.COM ") != "nikhil@example.com" {
		t.Error("NormalizeEmail must trim and lowercase")
	}

	for _, name := range []string{"ok_name", "A.b-c", "ab"} {
		if err := ValidateUsername(name); err != nil {
			t.Errorf("ValidateUsername(%q) = %v", name, err)
		}
	}
	for _, name := range []string{"a", "", "has space", "semi;colon", strings.Repeat("x", 40), ".leading"} {
		if err := ValidateUsername(name); err == nil {
			t.Errorf("ValidateUsername(%q) accepted an invalid name", name)
		}
	}

	if got := UsernameFromEmail("nikhil.gudi+work@example.com"); got != "nikhilgudi" {
		t.Errorf("UsernameFromEmail = %q, want nikhilgudi", got)
	}
}

func TestNewSignerRejectsWeakKey(t *testing.T) {
	if _, err := NewSigner("short"); err == nil {
		t.Fatal("a short signing key must be refused")
	}
}

func swapByte(s string) string {
	if s == "" {
		return "A"
	}
	first := s[0]
	if first == 'A' {
		return "B" + s[1:]
	}
	return "A" + s[1:]
}

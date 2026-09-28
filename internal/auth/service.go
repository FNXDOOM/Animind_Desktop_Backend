package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"animind-backend-go/internal/config"
	"animind-backend-go/internal/repository"
)

// Service is the whole authentication surface: accounts, credentials and sessions.
//
// Nothing here knows about HTTP. Handlers translate these results into status codes and
// JSON, which keeps the security decisions in one place and unit-testable without a
// server.
type Service struct {
	store  *repository.Store
	signer *Signer
	cfg    *config.Config
	google *Google
	logger *slog.Logger
}

// NewService wires the service to its dependencies.
func NewService(store *repository.Store, signer *Signer, cfg *config.Config, google *Google, logger *slog.Logger) *Service {
	return &Service{store: store, signer: signer, cfg: cfg, google: google, logger: logger}
}

// Principal is an authenticated caller.
type Principal struct {
	UserID   string `json:"-"`
	Email    string `json:"-"`
	Username string `json:"-"`
	IsAdmin  bool   `json:"-"`
	// Kind is how the caller proved identity: "web" or "desktop".
	Kind string `json:"-"`
	// WebSessionRowID identifies the site session row, used for logout and sliding expiry.
	WebSessionRowID string `json:"-"`
	// DesktopSessionID identifies the pinned desktop session, echoed to the player.
	DesktopSessionID string `json:"-"`
}

// IsWeb reports whether the caller is a browser session.
func (p *Principal) IsWeb() bool { return p.Kind == "web" }

// LoginResult is the outcome of a successful credential check.
type LoginResult struct {
	User         *repository.PublicUser
	SessionToken string
	ExpiresAt    time.Time
}

// DesktopTokens is the desktop credential pair handed to the player.
type DesktopTokens struct {
	UserID                string
	SessionID             string
	AccessToken           string
	RefreshToken          string
	ExpiresIn             int
	AccessTokenExpiresAt  time.Time
	RefreshTokenExpiresAt time.Time
}

// Credential and lifecycle errors.
var (
	ErrInvalidCredentials = errors.New("email or password is incorrect")
	ErrAccountLocked      = errors.New("too many failed attempts, try again later")
	ErrEmailTaken         = errors.New("an account with that email already exists")
	ErrSignupDisabled     = errors.New("account creation is disabled on this server")
	ErrNotAuthenticated   = errors.New("authentication required")
	ErrPasswordIncorrect  = errors.New("current password is incorrect")
	ErrTokenInvalid       = errors.New("token is not valid")
	// ErrInvalidAvatarURL is returned when a profile update carries a URL that could execute
	// in a browser, such as a javascript: link.
	ErrInvalidAvatarURL = errors.New("avatar_url must be an http or https URL")
)

// touchInterval throttles session last-used writes. A browser hits the API many times
// per second; rewriting the session row each time would make authentication the most
// expensive part of every request.
const touchInterval = time.Minute

// ── Password accounts ────────────────────────────────────────────────────────

// SignUp creates a password account and signs it in.
//
// Email addresses are unique after normalization, and the password is hashed with
// argon2id before anything else happens, so a rejected insert cannot leave a plaintext
// credential behind.
func (s *Service) SignUp(ctx context.Context, rawEmail, password, rawUsername string) (*LoginResult, error) {
	if !s.cfg.Auth.AllowSignup {
		return nil, ErrSignupDisabled
	}
	email := NormalizeEmail(rawEmail)
	if err := ValidateEmail(email); err != nil {
		return nil, err
	}
	if err := ValidatePasswordStrength(password); err != nil {
		return nil, err
	}
	username := strings.TrimSpace(rawUsername)
	if username == "" {
		username = UsernameFromEmail(email)
	}
	if err := ValidateUsername(username); err != nil {
		return nil, err
	}

	avatar := DefaultAvatar(username)
	hash, err := HashPassword(password, s.argon2Params())
	if err != nil {
		return nil, err
	}

	user, err := s.store.CreateUser(ctx, email, email, username, &avatar, &hash)
	if err != nil {
		if repository.IsUniqueViolation(err) {
			return nil, ErrEmailTaken
		}
		return nil, fmt.Errorf("auth: create user: %w", err)
	}
	s.logger.Info("account created", slog.String("user_id", user.ID), slog.String("method", "password"))
	return s.SignInUser(ctx, user.ID, "web", false)
}

// Login verifies a password and starts a web session.
//
// The failure path still runs argon2id (via the stored hash of a placeholder) when the
// account does not exist, so response time does not disclose whether an email is
// registered.
func (s *Service) Login(ctx context.Context, rawEmail, password string) (*LoginResult, error) {
	email := NormalizeEmail(rawEmail)
	if err := ValidateEmail(email); err != nil || password == "" {
		s.burnTime(ctx)
		return nil, ErrInvalidCredentials
	}

	user, err := s.store.UserByEmailNormalized(ctx, email)
	if errors.Is(err, repository.ErrNotFound) {
		s.burnTime(ctx)
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}

	if user.LockedUntil != nil && user.LockedUntil.After(time.Now()) {
		return nil, ErrAccountLocked
	}
	if user.PasswordHash == nil || *user.PasswordHash == "" {
		// Identity-only account: it must sign in through its provider.
		return nil, ErrInvalidCredentials
	}

	needsRehash, err := VerifyPasswordAgainstCurrent(password, *user.PasswordHash, s.argon2Params())
	if err != nil {
		if errors.Is(err, ErrHashMismatch) {
			if _, lockErr := s.store.RegisterFailedLogin(ctx, user.ID, s.cfg.Auth.MaxFailedLogins, s.cfg.Auth.FailedLoginLockout); lockErr != nil {
				s.logger.Error("failed to record login failure", slog.String("error", lockErr.Error()))
			}
			return nil, ErrInvalidCredentials
		}
		// A corrupt stored hash is a server-side defect, not a user error.
		s.logger.Error("stored password hash unreadable", slog.String("user_id", user.ID), slog.String("error", err.Error()))
		return nil, ErrInvalidCredentials
	}

	if err := s.store.ClearFailedLogins(ctx, user.ID); err != nil {
		s.logger.Warn("could not clear login failures", slog.String("error", err.Error()))
	}
	if needsRehash {
		// Parameters were raised since the hash was written; upgrade it now that the
		// plaintext is available.
		if fresh, hashErr := HashPassword(password, s.argon2Params()); hashErr == nil {
			if updateErr := s.store.UpdatePassword(ctx, user.ID, fresh); updateErr != nil {
				s.logger.Warn("could not upgrade password hash", slog.String("error", updateErr.Error()))
			}
		}
	}

	return s.SignInUser(ctx, user.ID, "web", false)
}

// SignInUser opens a web session for an existing account and returns its token.
//
// deviceName labels the session in the security screen, and rememberMe extends its
// lifetime to the configured "stay signed in" window.
func (s *Service) SignInUser(ctx context.Context, userID, deviceName string, rememberMe bool) (*LoginResult, error) {
	user, err := s.store.UserByID(ctx, userID)
	if err != nil {
		return nil, err
	}

	token, err := NewWebSessionToken()
	if err != nil {
		return nil, err
	}
	ttl := s.cfg.Auth.WebSessionTTL
	if rememberMe {
		ttl = s.cfg.Auth.RememberMeTTL
	}
	expiresAt := time.Now().Add(ttl)

	if _, err := s.store.CreateWebSession(ctx, userID, HashToken(token), deviceName, userAgent(ctx), clientIP(ctx), rememberMe, expiresAt); err != nil {
		return nil, fmt.Errorf("auth: create web session: %w", err)
	}
	return &LoginResult{User: user.Public(), SessionToken: token, ExpiresAt: expiresAt}, nil
}

// ── Credential verification ──────────────────────────────────────────────────

// Authenticate resolves any bearer credential to a principal.
//
// Desktop tokens are verified by signature first and then against their pinned row, so
// a forged token is rejected without a database query and a revoked one is rejected with
// a query. Site sessions are always a single indexed lookup.
func (s *Service) Authenticate(ctx context.Context, token string) (*Principal, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, ErrNotAuthenticated
	}

	switch Kind(token) {
	case TokenDesktopAccess:
		return s.authenticateDesktop(ctx, token)
	case TokenWebSession:
		return s.authenticateWeb(ctx, token)
	default:
		return nil, ErrTokenInvalid
	}
}

func (s *Service) authenticateDesktop(ctx context.Context, token string) (*Principal, error) {
	claims, err := s.signer.VerifyDesktopAccessToken(token)
	if err != nil {
		return nil, ErrTokenInvalid
	}
	session, err := s.store.DesktopSessionByAccessHash(ctx, HashToken(token))
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, ErrTokenInvalid
		}
		return nil, err
	}
	// The row must describe the same identity the signature claims; otherwise a token
	// re-issued against a different session would be accepted.
	if session.UserID != claims.UserID || session.SessionID != claims.SessionID {
		return nil, ErrTokenInvalid
	}
	user, err := s.store.UserByID(ctx, session.UserID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, ErrTokenInvalid
		}
		return nil, err
	}
	return &Principal{
		UserID: user.ID, Email: user.Email, Username: user.Username, IsAdmin: user.IsAdmin,
		Kind: "desktop", DesktopSessionID: session.SessionID,
	}, nil
}

func (s *Service) authenticateWeb(ctx context.Context, token string) (*Principal, error) {
	session, err := s.store.ActiveWebSession(ctx, HashToken(token))
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, ErrTokenInvalid
		}
		return nil, err
	}
	if time.Since(session.LastUsedAt) > touchInterval {
		ttl := s.cfg.Auth.WebSessionTTL
		if session.RememberMe {
			ttl = s.cfg.Auth.RememberMeTTL
		}
		if err := s.store.TouchWebSession(ctx, session.ID, time.Now().Add(ttl)); err != nil {
			s.logger.Warn("could not refresh session expiry", slog.String("error", err.Error()))
		}
	}
	user, err := s.store.UserByID(ctx, session.UserID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, ErrTokenInvalid
		}
		return nil, err
	}
	return &Principal{
		UserID: user.ID, Email: user.Email, Username: user.Username, IsAdmin: user.IsAdmin,
		Kind: "web", WebSessionRowID: session.ID,
	}, nil
}

// Logout revokes the calling session. Desktop callers use the revoke endpoint instead.
func (s *Service) Logout(ctx context.Context, p *Principal) error {
	if !p.IsWeb() || p.WebSessionRowID == "" {
		return nil
	}
	return s.store.RevokeWebSession(ctx, p.UserID, p.WebSessionRowID)
}

// RevokeSession revokes one credential by id, whichever family it belongs to.
//
// Both ids are primary keys of rows owned by the caller, and the user filter is applied in
// SQL by both repository methods, so an id belonging to another account revokes nothing.
func (s *Service) RevokeSession(ctx context.Context, userID, id string) (bool, error) {
	desktop, err := s.store.RevokeDesktopSessionByID(ctx, id, userID)
	if err != nil {
		return false, err
	}
	if desktop {
		return true, nil
	}
	if err := s.store.RevokeWebSession(ctx, userID, id); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// LogoutEverywhere revokes all other web sessions for the account.
func (s *Service) LogoutEverywhere(ctx context.Context, userID, exceptRowID string) error {
	return s.store.RevokeAllWebSessions(ctx, userID, exceptRowID)
}

// ── Desktop sessions ─────────────────────────────────────────────────────────

// DesktopExchange trades a web credential for the desktop pair.
//
// Accepted inputs are the caller's site session token or a bridge JWT minted by
// BridgeToken; both resolve to an account, and the resulting session is labeled with
// the device the desktop player reports.
func (s *Service) DesktopExchange(ctx context.Context, bearer, deviceID, deviceName string) (*DesktopTokens, error) {
	principal, err := s.Authenticate(ctx, bearer)
	if err != nil {
		// Fall back to a bridge JWT, which is what the browser handoff delivers.
		claims, jwtErr := s.signer.VerifyBridgeToken(strings.TrimSpace(bearer))
		if jwtErr != nil {
			return nil, ErrNotAuthenticated
		}
		user, userErr := s.store.UserByID(ctx, claims.Subject)
		if userErr != nil {
			return nil, ErrTokenInvalid
		}
		principal = &Principal{UserID: user.ID, Email: user.Email, Username: user.Username, IsAdmin: user.IsAdmin, Kind: "web"}
	}
	return s.issueDesktopSession(ctx, principal.UserID, deviceID, deviceName)
}

// issueDesktopSession creates the token pair and pins it.
func (s *Service) issueDesktopSession(ctx context.Context, userID, deviceID, deviceName string) (*DesktopTokens, error) {
	sessionID, err := NewOpaqueToken(16)
	if err != nil {
		return nil, err
	}
	refreshToken, err := NewDesktopRefreshToken()
	if err != nil {
		return nil, err
	}
	refreshExpires := time.Now().Add(s.cfg.Auth.RefreshTokenTTL)
	accessExpires := time.Now().Add(s.cfg.Auth.AccessTokenTTL)
	accessToken := s.signer.IssueDesktopAccessToken(userID, sessionID, accessExpires)

	if _, err := s.store.CreateDesktopSession(ctx, userID, sessionID,
		TruncateLabel(deviceID, 128), TruncateLabel(deviceName, 128),
		HashToken(refreshToken), HashToken(accessToken), accessExpires, refreshExpires); err != nil {
		return nil, fmt.Errorf("auth: pin desktop session: %w", err)
	}

	return &DesktopTokens{
		UserID: userID, SessionID: sessionID,
		AccessToken: accessToken, RefreshToken: refreshToken,
		ExpiresIn:             int(s.cfg.Auth.AccessTokenTTL.Seconds()),
		AccessTokenExpiresAt:  accessExpires,
		RefreshTokenExpiresAt: refreshExpires,
	}, nil
}

// DesktopRefresh rotates a session's credentials.
//
// The presented refresh token stops working as part of the same statement, so a stolen
// token can only be used once.
func (s *Service) DesktopRefresh(ctx context.Context, refreshToken string) (*DesktopTokens, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if !strings.HasPrefix(refreshToken, DesktopRefreshPrefix) {
		return nil, ErrTokenInvalid
	}
	session, err := s.store.DesktopSessionByRefreshHash(ctx, HashToken(refreshToken))
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, ErrTokenInvalid
		}
		return nil, err
	}

	// Keep the session id stable: clients correlate devices on it.
	newRefresh, err := NewDesktopRefreshToken()
	if err != nil {
		return nil, err
	}
	accessExpires := time.Now().Add(s.cfg.Auth.AccessTokenTTL)
	refreshExpires := time.Now().Add(s.cfg.Auth.RefreshTokenTTL)
	newAccess := s.signer.IssueDesktopAccessToken(session.UserID, session.SessionID, accessExpires)

	if err := s.store.RotateDesktopSession(ctx, session.ID, HashToken(newRefresh), HashToken(newAccess), accessExpires, refreshExpires); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, ErrTokenInvalid
		}
		return nil, fmt.Errorf("auth: rotate desktop session: %w", err)
	}

	return &DesktopTokens{
		UserID: session.UserID, SessionID: session.SessionID,
		AccessToken: newAccess, RefreshToken: newRefresh,
		ExpiresIn:             int(s.cfg.Auth.AccessTokenTTL.Seconds()),
		AccessTokenExpiresAt:  accessExpires,
		RefreshTokenExpiresAt: refreshExpires,
	}, nil
}

// DesktopRevoke signs a device out using either half of its credential pair. It reports
// whether any row matched, so the handler can answer 404 rather than pretending.
func (s *Service) DesktopRevoke(ctx context.Context, refreshToken, accessToken string) (bool, error) {
	revoked := false
	if refreshToken = strings.TrimSpace(refreshToken); refreshToken != "" {
		ok, err := s.store.RevokeDesktopSessionByRefreshHash(ctx, HashToken(refreshToken))
		if err != nil {
			return false, err
		}
		revoked = revoked || ok
	}
	if accessToken = strings.TrimSpace(accessToken); accessToken != "" && IsDesktopAccessToken(accessToken) {
		ok, err := s.store.RevokeDesktopSessionByAccessToken(ctx, HashToken(accessToken))
		if err != nil {
			return false, err
		}
		revoked = revoked || ok
	}
	return revoked, nil
}

// BridgeToken mints the short-lived JWT the site hands to a desktop player over the
// loopback callback.
//
// It is signed by this backend, carries the session id the desktop expects, and lives
// for well under a minute because it is exchanged immediately.
func (s *Service) BridgeToken(ctx context.Context, p *Principal) (string, error) {
	token, err := s.signer.SignBridgeToken(BridgeClaims{
		Issuer:    s.cfg.PublicIssuer(),
		Subject:   p.UserID,
		SessionID: p.DesktopSessionID,
		Email:     p.Email,
		Audience:  BridgeAudience,
		ExpiresAt: time.Now().Add(60 * time.Second).Unix(),
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

// ── Profile and password maintenance ─────────────────────────────────────────

// UpdateProfile changes the display name and avatar for the signed-in user.
func (s *Service) UpdateProfile(ctx context.Context, userID string, username *string, avatarURL *string) (*repository.PublicUser, error) {
	if username != nil {
		if err := ValidateUsername(*username); err != nil {
			return nil, err
		}
		trimmed := strings.TrimSpace(*username)
		username = &trimmed
	}
	if avatarURL != nil {
		trimmed := strings.TrimSpace(*avatarURL)
		if trimmed != "" && !IsSafeImageURL(trimmed) {
			return nil, ErrInvalidAvatarURL
		}
		if trimmed == "" {
			avatarURL = nil
		} else {
			avatarURL = &trimmed
		}
	}
	user, err := s.store.UpdateProfile(ctx, userID, username, avatarURL)
	if err != nil {
		return nil, err
	}
	return user.Public(), nil
}

// IsSafeImageURL rejects javascript: and other executable schemes in a stored avatar URL.
func IsSafeImageURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return parsed.Scheme == "http" || parsed.Scheme == "https"
}

// ChangePassword replaces a password after confirming the current one. Every other web
// session is revoked so a stolen cookie cannot outlive a password change.
func (s *Service) ChangePassword(ctx context.Context, userID, currentPassword, newPassword string) error {
	user, err := s.store.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	if user.PasswordHash == nil {
		return ErrPasswordIncorrect
	}
	if _, err := VerifyPasswordAgainstCurrent(currentPassword, *user.PasswordHash, s.argon2Params()); err != nil {
		if errors.Is(err, ErrHashMismatch) {
			return ErrPasswordIncorrect
		}
		return err
	}
	hash, err := HashPassword(newPassword, s.argon2Params())
	if err != nil {
		return err
	}
	if err := s.store.UpdatePassword(ctx, userID, hash); err != nil {
		return err
	}
	return s.store.RevokeAllWebSessions(ctx, userID, "")
}

// RequestPasswordReset pins a single-use token for an existing account.
//
// It reports whether an account matched so the caller can log that, but the HTTP layer
// must answer identically either way to avoid confirming addresses.
func (s *Service) RequestPasswordReset(ctx context.Context, rawEmail string) (token string, found bool, err error) {
	email := NormalizeEmail(rawEmail)
	user, lookupErr := s.store.UserByEmailNormalized(ctx, email)
	if errors.Is(lookupErr, repository.ErrNotFound) {
		return "", false, nil
	}
	if lookupErr != nil {
		return "", false, lookupErr
	}
	token, err = NewOpaqueToken(32)
	if err != nil {
		return "", true, err
	}
	err = s.store.CreateAuthToken(ctx, user.ID, "password_reset", HashToken(token), time.Now().Add(time.Hour))
	return token, true, err
}

// ResetPassword consumes a reset token and installs a new password, signing out every
// existing session on success.
func (s *Service) ResetPassword(ctx context.Context, token, newPassword string) error {
	userID, err := s.store.ConsumeAuthToken(ctx, "password_reset", HashToken(strings.TrimSpace(token)))
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return ErrTokenInvalid
		}
		return err
	}
	hash, err := HashPassword(newPassword, s.argon2Params())
	if err != nil {
		return err
	}
	if err := s.store.UpdatePassword(ctx, userID, hash); err != nil {
		return err
	}
	return s.store.RevokeAllWebSessions(ctx, userID, "")
}

// DeleteAccount removes the user and cascades every owned row and session.
func (s *Service) DeleteAccount(ctx context.Context, userID string) error {
	return s.store.DeleteUser(ctx, userID)
}

// ── Provider sign-in ─────────────────────────────────────────────────────────

// GoogleEnabled reports whether the provider is configured, so the HTTP layer can
// answer 501 instead of redirecting to a broken URL.
func (s *Service) GoogleEnabled() bool { return s.google != nil && s.google.Enabled }

// GoogleAuthURL builds the provider redirect.
func (s *Service) GoogleAuthURL(state string) string { return s.google.AuthURL(state) }

// GoogleExchange trades an authorization code for a verified profile.
func (s *Service) GoogleExchange(ctx context.Context, code string) (*GoogleProfile, error) {
	if !s.GoogleEnabled() {
		return nil, errors.New("auth: Google sign-in is not configured")
	}
	return s.google.Exchange(ctx, code)
}

// GoogleRedirectState is the signed, single-use value carried through the provider round
// trip. It binds the callback to the browser that started it and records where to return.
func (s *Service) GoogleRedirectState(redirectAfter string) (string, error) {
	return s.signer.SignBridgeToken(BridgeClaims{
		Issuer:    s.cfg.PublicIssuer(),
		Subject:   "oauth-state",
		SessionID: TruncateLabel(redirectAfter, 512),
		Audience:  "animind-oauth-state",
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
	})
}

// VerifyGoogleRedirectState validates the state parameter and returns the requested
// post-login redirect target.
func (s *Service) VerifyGoogleRedirectState(state string) (redirectAfter string, err error) {
	claims, err := s.signer.VerifyBridgeToken(state)
	if err != nil {
		return "", ErrTokenInvalid
	}
	if claims.Audience != "animind-oauth-state" || claims.Subject != "oauth-state" {
		return "", ErrTokenInvalid
	}
	return claims.SessionID, nil
}

// SignInWithGoogle finds or creates the account behind a verified Google profile and
// starts a web session.
func (s *Service) SignInWithGoogle(ctx context.Context, profile *GoogleProfile) (*LoginResult, error) {
	user, err := s.store.UserByProviderUserID(ctx, "google", profile.Subject)
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return nil, err
	}

	if errors.Is(err, repository.ErrNotFound) {
		email := NormalizeEmail(profile.Email)
		if email == "" {
			return nil, errors.New("auth: Google account supplied no email address")
		}
		if existing, lookupErr := s.store.UserByEmailNormalized(ctx, email); lookupErr == nil {
			// Password account first, Google later: attach the identity to it.
			user = existing
		} else if errors.Is(lookupErr, repository.ErrNotFound) {
			username := strings.TrimSpace(profile.Name)
			if usernameErr := ValidateUsername(username); usernameErr != nil {
				username = UsernameFromEmail(email)
			}
			avatar := profile.Picture
			if avatar == "" {
				avatar = DefaultAvatar(username)
			}
			created, createErr := s.store.CreateUser(ctx, email, email, username, &avatar, nil)
			if createErr != nil {
				if repository.IsUniqueViolation(createErr) {
					// Lost a race with a concurrent signup on the same address.
					existing, retryErr := s.store.UserByEmailNormalized(ctx, email)
					if retryErr != nil {
						return nil, retryErr
					}
					created = existing
				} else {
					return nil, fmt.Errorf("auth: create oauth user: %w", createErr)
				}
			}
			user = created
		} else {
			return nil, lookupErr
		}
		if _, linkErr := s.store.LinkIdentity(ctx, user.ID, "google", profile.Subject, NormalizeEmail(profile.Email), profile.Name, profile.Picture); linkErr != nil {
			return nil, fmt.Errorf("auth: link google identity: %w", linkErr)
		}
		s.logger.Info("google identity linked", slog.String("user_id", user.ID))
	}

	return s.SignInUser(ctx, user.ID, "google", false)
}

// ── One-time codes for the site handoff ──────────────────────────────────────

// CreateAuthCode mints a short-lived code the site trades for a session after an
// OAuth redirect, so a bearer token never appears in browser history or a Referer.
func (s *Service) CreateAuthCode(ctx context.Context, userID string) (string, error) {
	code, err := NewOpaqueToken(32)
	if err != nil {
		return "", err
	}
	if err := s.store.CreateAuthToken(ctx, userID, "auth_code", HashToken(code), time.Now().Add(2*time.Minute)); err != nil {
		return "", err
	}
	return code, nil
}

// ConsumeAuthCode redeems a one-time code for a full web session.
func (s *Service) ConsumeAuthCode(ctx context.Context, code string) (*LoginResult, error) {
	userID, err := s.store.ConsumeAuthToken(ctx, "auth_code", HashToken(strings.TrimSpace(code)))
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, ErrTokenInvalid
		}
		return nil, err
	}
	return s.SignInUser(ctx, userID, "oauth-code", true)
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func (s *Service) argon2Params() Argon2Params {
	a := s.cfg.Auth
	params := Argon2Params{Memory: a.Argon2Memory, Time: a.Argon2Time, Threads: a.Argon2Threads, KeyLen: 32}
	if params.Memory == 0 || params.Time == 0 || params.Threads == 0 {
		return DefaultArgon2Params()
	}
	return params
}

// burnTime performs a throw-away hash so a missing account costs the same as a wrong
// password.
func (s *Service) burnTime(ctx context.Context) {
	placeholder := "$argon2id$v=19$m=65536,t=3,p=4$c29tZXNhbHRzb21lc2FsdA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	_, _ = VerifyPasswordAgainstCurrent("not-a-real-password", placeholder, s.argon2Params())
}

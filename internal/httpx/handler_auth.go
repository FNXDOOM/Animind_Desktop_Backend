package httpx

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"animind-backend-go/internal/auth"
	"animind-backend-go/internal/repository"
)

const googleStateCookieName = "animind_google_state"

// Authentication endpoints. Web clients use a session cookie or bearer token; the desktop
// player a HMAC access token. Passwords never appear in a URL, a log line, or a response body.

// sessionResponse is the credential a browser receives.
type sessionResponse struct {
	Token     string                 `json:"access_token"`
	TokenType string                 `json:"token_type"`
	ExpiresAt string                 `json:"expires_at"`
	User      *repository.PublicUser `json:"user"`
}

// signupRequest is the POST /api/auth/signup body.
type signupRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Username string `json:"username"`
}

// loginRequest is the POST /api/auth/login body.
type loginRequest struct {
	Email      string `json:"email"`
	Password   string `json:"password"`
	RememberMe bool   `json:"remember_me"`
}

func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	var req signupRequest
	if err := DecodeJSON(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, "Email, password and username are required.")
		return
	}

	result, err := s.auth.SignUp(s.withClient(r), req.Email, req.Password, req.Username)
	if err != nil {
		s.writeAuthError(w, err, "Signup failed.")
		return
	}

	s.setSessionCookie(w, result.SessionToken, result.ExpiresAt, false)
	WriteJSON(w, http.StatusCreated, sessionResponse{
		Token: result.SessionToken, TokenType: "Bearer",
		ExpiresAt: result.ExpiresAt.UTC().Format(time.RFC3339),
		User:      result.User,
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := DecodeJSON(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, "Email and password are required.")
		return
	}
	if strings.TrimSpace(req.Email) == "" || req.Password == "" {
		WriteError(w, http.StatusBadRequest, "Email and password are required.")
		return
	}

	loginCtx := auth.WithLoginSource(s.withClient(r), ClientIP(r, s.cfg.TrustProxy))
	result, err := s.auth.Login(loginCtx, req.Email, req.Password)
	if err != nil {
		s.writeAuthError(w, err, "Login failed.")
		return
	}

	s.setSessionCookie(w, result.SessionToken, result.ExpiresAt, req.RememberMe)
	WriteJSON(w, http.StatusOK, sessionResponse{
		Token: result.SessionToken, TokenType: "Bearer",
		ExpiresAt: result.ExpiresAt.UTC().Format(time.RFC3339),
		User:      result.User,
	})
}

// handleLogout revokes the caller's session, then clears the cookie. It sits outside
// RequireAuth so logout still succeeds with an expired or missing credential.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	principal := PrincipalFromContext(r.Context())
	if principal == nil {
		if token := s.credential(r); token != "" {
			if resolved, err := s.auth.Authenticate(r.Context(), token); err == nil {
				principal = resolved
			}
		}
	}
	if principal != nil && principal.IsWeb() {
		if err := s.auth.Logout(r.Context(), principal); err != nil {
			s.logger.Warn("logout could not revoke the session", slog.String("error", err.Error()))
		}
	}
	s.clearSessionCookie(w)
	WriteJSON(w, http.StatusOK, map[string]bool{"success": true})
}

func (s *Server) handleCurrentUser(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.requirePrincipal(w, r)
	if !ok {
		return
	}
	user, err := s.store.UserByID(r.Context(), principal.UserID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			WriteError(w, http.StatusUnauthorized, "Invalid or expired token.")
			return
		}
		s.serverError(w, "Could not load your account.", err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"user": user.Public(), "session_kind": principal.Kind})
}

// ── Password maintenance ─────────────────────────────────────────────────────

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req changePasswordRequest
	if err := DecodeJSON(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, "current_password and new_password are required.")
		return
	}
	if err := s.auth.ChangePassword(r.Context(), principal.UserID, req.CurrentPassword, req.NewPassword); err != nil {
		s.writeAuthError(w, err, "Could not change your password.")
		return
	} // Every session was revoked, so this cookie is dead.
	s.clearSessionCookie(w)
	WriteJSON(w, http.StatusOK, map[string]bool{"success": true})
}

type forgotPasswordRequest struct {
	Email string `json:"email"`
}

// handleForgotPassword answers identically whether or not the address exists, so it cannot
// enumerate accounts. Only a token hash is logged; delivery belongs to the recovery integration.
func (s *Server) handleForgotPassword(w http.ResponseWriter, r *http.Request) {
	var req forgotPasswordRequest
	if err := DecodeJSON(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, "email is required.")
		return
	}
	_, found, err := s.auth.RequestPasswordReset(r.Context(), req.Email)
	if err != nil {
		s.serverError(w, "Could not start password recovery.", err)
		return
	}
	if found {
		// The reset token is never logged, so a log sink cannot become a credential oracle.
		s.logger.Warn("password reset token issued",
			slog.String("note", "deliver through the configured recovery channel; it expires in one hour"))
	}
	WriteJSON(w, http.StatusAccepted, map[string]any{
		"success": true,
		"message": "If that address has an account, a reset link has been generated.",
	})
}

type resetPasswordRequest struct {
	Token       string `json:"token"`
	NewPassword string `json:"new_password"`
}

func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	var req resetPasswordRequest
	if err := DecodeJSON(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, "token and new_password are required.")
		return
	}
	if err := s.auth.ResetPassword(r.Context(), req.Token, req.NewPassword); err != nil {
		s.writeAuthError(w, err, "Could not reset the password.")
		return
	}
	s.clearSessionCookie(w)
	WriteJSON(w, http.StatusOK, map[string]bool{"success": true})
}

// ── Google sign-in ───────────────────────────────────────────────────────────

// handleGoogleURL returns the provider redirect. The state is signed and single-use, binding
// this browser to the callback so a forged callback cannot sign someone into another account.
func (s *Server) handleGoogleURL(w http.ResponseWriter, r *http.Request) {
	if !s.auth.GoogleEnabled() {
		WriteErrorCode(w, http.StatusNotImplemented, "oauth_not_configured",
			"Google sign-in is not configured on this server.")
		return
	}
	redirectAfter := sanitizeReturnTarget(r.URL.Query().Get("redirect"), s.returnTargets())
	browserNonce, err := auth.NewOpaqueToken(32)
	if err != nil {
		s.serverError(w, "Could not start Google sign-in.", err)
		return
	}
	state, err := s.auth.GoogleRedirectStateForBrowser(redirectAfter, browserNonce)
	if err != nil {
		s.serverError(w, "Could not start Google sign-in.", err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: googleStateCookieName, Value: browserNonce, Path: "/api/auth/google",
		MaxAge: 10 * 60, HttpOnly: true, Secure: s.cfg.Auth.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	})
	WriteJSON(w, http.StatusOK, map[string]string{"url": s.auth.GoogleAuthURL(state), "state": state})
}

// handleGoogleStart is the browser-facing entry point: it sets the nonce cookie and redirects
// to the provider. google/url cannot work from an app, whose cookie never reaches the callback
// browser's jar.
func (s *Server) handleGoogleStart(w http.ResponseWriter, r *http.Request) {
	if !s.auth.GoogleEnabled() {
		WriteErrorCode(w, http.StatusNotImplemented, "oauth_not_configured",
			"Google sign-in is not configured on this server.")
		return
	}
	redirectAfter := sanitizeReturnTarget(r.URL.Query().Get("redirect"), s.returnTargets())
	browserNonce, err := auth.NewOpaqueToken(32)
	if err != nil {
		s.serverError(w, "Could not start Google sign-in.", err)
		return
	}
	state, err := s.auth.GoogleRedirectStateForBrowser(redirectAfter, browserNonce)
	if err != nil {
		s.serverError(w, "Could not start Google sign-in.", err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: googleStateCookieName, Value: browserNonce, Path: "/api/auth/google",
		MaxAge: 10 * 60, HttpOnly: true, Secure: s.cfg.Auth.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, s.auth.GoogleAuthURL(state), http.StatusFound)
}

// returnTargets is the only place AppLinkOrigins joins FrontendOrigins, so CORS and every other
// origin check stay limited to the web client's own origins. PUBLIC_URL is always a safe return
// target.
func (s *Server) returnTargets() []string {
	merged := make([]string, 0, len(s.cfg.FrontendOrigins)+len(s.cfg.AppLinkOrigins)+1)
	if s.cfg.PublicURL != "" {
		merged = append(merged, s.cfg.PublicURL)
	}
	merged = append(merged, s.cfg.FrontendOrigins...)
	merged = append(merged, s.cfg.AppLinkOrigins...)
	return merged
}

func (s *Server) handleGoogleCallback(w http.ResponseWriter, r *http.Request) {
	if !s.auth.GoogleEnabled() {
		WriteError(w, http.StatusNotImplemented, "Google sign-in is not configured on this server.")
		return
	}
	browserNonce := ""
	if cookie, err := r.Cookie(googleStateCookieName); err == nil {
		browserNonce = strings.TrimSpace(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: googleStateCookieName, Path: "/api/auth/google", MaxAge: -1, HttpOnly: true, Secure: s.cfg.Auth.SecureCookies, SameSite: http.SameSiteLaxMode})
	if providerError := r.URL.Query().Get("error"); providerError != "" {
		s.logger.Info("Google sign-in declined by the user", slog.String("error", providerError))
		http.Redirect(w, r, s.frontendReturn("")+"?auth=cancelled", http.StatusFound)
		return
	}

	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	if code == "" || state == "" {
		WriteError(w, http.StatusBadRequest, "Missing code or state.")
		return
	}
	redirectAfter, err := s.auth.VerifyGoogleRedirectStateForBrowser(state, browserNonce)
	if err != nil {
		WriteError(w, http.StatusUnauthorized, "The sign-in attempt could not be verified. Start again.")
		return
	}

	profile, err := s.auth.GoogleExchange(r.Context(), code)
	if err != nil {
		s.logger.Warn("Google sign-in failed", slog.String("error", err.Error()))
		http.Redirect(w, r, s.frontendReturn(redirectAfter)+"?auth=failed", http.StatusFound)
		return
	}

	result, err := s.auth.SignInWithGoogle(s.withClient(r), profile)
	if err != nil {
		s.logger.Warn("Google account could not be provisioned", slog.String("error", err.Error()))
		http.Redirect(w, r, s.frontendReturn(redirectAfter)+"?auth=failed", http.StatusFound)
		return
	}

	// A same-origin deployment gets the cookie immediately; the SPA is redirected with a
	// one-time code so the session token never appears in a URL it could be cached in.
	s.setSessionCookie(w, result.SessionToken, result.ExpiresAt, true)
	authCode, err := s.auth.CreateAuthCode(r.Context(), result.User.ID)
	if err != nil {
		s.serverError(w, "Could not complete Google sign-in.", err)
		return
	}
	target := s.frontendReturn(redirectAfter)
	if target == "" {
		WriteJSON(w, http.StatusOK, sessionResponse{
			Token: result.SessionToken, TokenType: "Bearer",
			ExpiresAt: result.ExpiresAt.UTC().Format(time.RFC3339), User: result.User,
		})
		return
	}
	separator := "?"
	if strings.Contains(target, "?") {
		separator = "&"
	}
	http.Redirect(w, r, target+separator+"auth_code="+url.QueryEscape(authCode), http.StatusFound)
}

// consumeCodeRequest is the POST /api/auth/consume-code body.
type consumeCodeRequest struct {
	Code string `json:"code"`
}

func (s *Server) handleConsumeCode(w http.ResponseWriter, r *http.Request) {
	var req consumeCodeRequest
	if err := DecodeJSON(r, &req); err != nil || strings.TrimSpace(req.Code) == "" {
		WriteError(w, http.StatusBadRequest, "code is required.")
		return
	}
	result, err := s.auth.ConsumeAuthCode(s.withClient(r), req.Code)
	if err != nil {
		s.writeAuthError(w, err, "That sign-in link is no longer valid.")
		return
	}
	s.setSessionCookie(w, result.SessionToken, result.ExpiresAt, true)
	WriteJSON(w, http.StatusOK, sessionResponse{
		Token: result.SessionToken, TokenType: "Bearer",
		ExpiresAt: result.ExpiresAt.UTC().Format(time.RFC3339), User: result.User,
	})
}

func (s *Server) frontendReturn(target string) string {
	if target != "" {
		return target
	}
	if len(s.cfg.FrontendOrigins) > 0 && s.cfg.FrontendOrigins[0] != "*" {
		return s.cfg.FrontendOrigins[0]
	}
	return ""
}

// sanitizeReturnTarget accepts a post-login redirect only when it names a configured origin,
// so the callback cannot be turned into an open redirect.
func sanitizeReturnTarget(raw string, allowed []string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme != "https" && parsed.Scheme != "http" {
		return ""
	}
	origin := parsed.Scheme + "://" + parsed.Host
	for _, candidate := range allowed {
		if candidate == "*" {
			continue
		}
		if strings.EqualFold(strings.TrimRight(candidate, "/"), origin) {
			return trimmed
		}
	}
	return ""
}

// ── Desktop pairing ──────────────────────────────────────────────────────────

// desktopTokenResponse is the exact wire shape the Qt player parses: it reads access_token,
// refresh_token and session_id, and prefers the ISO-8601 timestamps over expires_in, so both
// are sent.
type desktopTokenResponse struct {
	TokenType             string `json:"token_type"`
	AccessToken           string `json:"access_token"`
	RefreshToken          string `json:"refresh_token"`
	SessionID             string `json:"session_id"`
	ExpiresIn             int    `json:"expires_in"`
	AccessTokenExpiresAt  string `json:"access_token_expires_at"`
	RefreshTokenExpiresAt string `json:"refresh_token_expires_at"`
}

func desktopResponse(tokens *auth.DesktopTokens) desktopTokenResponse {
	return desktopTokenResponse{
		TokenType:             "Bearer",
		AccessToken:           tokens.AccessToken,
		RefreshToken:          tokens.RefreshToken,
		SessionID:             tokens.SessionID,
		ExpiresIn:             tokens.ExpiresIn,
		AccessTokenExpiresAt:  tokens.AccessTokenExpiresAt.UTC().Format(time.RFC3339),
		RefreshTokenExpiresAt: tokens.RefreshTokenExpiresAt.UTC().Format(time.RFC3339),
	}
}

type desktopExchangeRequest struct {
	DeviceID   string `json:"deviceId"`
	DeviceName string `json:"deviceName"`
}

// handleDesktopExchange pairs a desktop player with a signed-in account. The credential is a
// browser session token or the bridge JWT; both are issued here, so the exchange proves a
// logged-in browser.
func (s *Server) handleDesktopExchange(w http.ResponseWriter, r *http.Request) {
	bearer := s.credential(r)
	if bearer == "" {
		WriteError(w, http.StatusUnauthorized, "Missing authorization token.")
		return
	}
	var req desktopExchangeRequest
	if r.ContentLength > 0 {
		if err := DecodeJSON(r, &req); err != nil {
			WriteError(w, http.StatusBadRequest, "deviceId and deviceName must be strings.")
			return
		}
	}

	tokens, err := s.auth.DesktopExchange(r.Context(), bearer, req.DeviceID, req.DeviceName)
	if err != nil {
		s.writeAuthError(w, err, "Could not pair this device.")
		return
	}
	s.logger.Info("desktop device paired", slog.String("user_id", tokens.UserID), slog.String("device", req.DeviceName))
	WriteJSON(w, http.StatusOK, desktopResponse(tokens))
}

type refreshTokenRequest struct {
	RefreshToken string `json:"refresh_token"`
}

func (s *Server) handleDesktopRefresh(w http.ResponseWriter, r *http.Request) {
	var req refreshTokenRequest
	if err := DecodeJSON(r, &req); err != nil || strings.TrimSpace(req.RefreshToken) == "" {
		WriteError(w, http.StatusBadRequest, "refresh_token is required.")
		return
	}
	tokens, err := s.auth.DesktopRefresh(r.Context(), req.RefreshToken)
	if err != nil {
		WriteError(w, http.StatusUnauthorized, "Invalid or expired refresh token.")
		return
	}
	WriteJSON(w, http.StatusOK, desktopResponse(tokens))
}

// handleDesktopRevoke signs a desktop device out. Either half of the credential pair works,
// because the player may already hold an expired access token.
func (s *Server) handleDesktopRevoke(w http.ResponseWriter, r *http.Request) {
	var req refreshTokenRequest
	if r.ContentLength > 0 {
		if err := DecodeJSON(r, &req); err != nil {
			WriteError(w, http.StatusBadRequest, "Provide refresh_token or Authorization bearer token.")
			return
		}
	}
	bearer := BearerToken(r)
	if strings.TrimSpace(req.RefreshToken) == "" && bearer == "" {
		WriteError(w, http.StatusBadRequest, "Provide refresh_token or Authorization bearer token.")
		return
	}

	revoked, err := s.auth.DesktopRevoke(r.Context(), req.RefreshToken, bearer)
	if err != nil {
		s.serverError(w, "Failed to revoke desktop session.", err)
		return
	}
	if !revoked {
		WriteError(w, http.StatusNotFound, "Desktop session not found.")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]bool{"success": true})
}

// handleBridgeToken mints the credential a signed-in browser hands to a desktop player over
// its loopback callback; the desktop posts it to /api/auth/desktop/exchange.
func (s *Server) handleBridgeToken(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !principal.IsWeb() {
		// A desktop token is already device-bound; handing it on would pair two machines
		// to one session.
		WriteError(w, http.StatusConflict, "Sign in in a browser first to pair a desktop device.")
		return
	}
	token, err := s.auth.BridgeToken(r.Context(), principal)
	if err != nil {
		s.serverError(w, "Could not create the pairing token.", err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"token":      token,
		"session_id": principal.WebSessionRowID,
		"user_id":    principal.UserID,
		"email":      principal.Email,
		"expires_in": 60,
		// callback lets the client build the loopback URL the desktop is listening on.
		"callback_example": "http://127.0.0.1:27182/auth?token=<token>&sessionId=<session_id>",
	})
}

// ── Session inventory ────────────────────────────────────────────────────────

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.requirePrincipal(w, r)
	if !ok {
		return
	}
	web, err := s.store.ListWebSessions(r.Context(), principal.UserID)
	if err != nil {
		s.serverError(w, "Could not list your sessions.", err)
		return
	}
	desktop, err := s.store.ListDesktopSessions(r.Context(), principal.UserID)
	if err != nil {
		s.serverError(w, "Could not list your devices.", err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"web_sessions":    web,
		"desktop_devices": desktop,
		"current_session": principal.WebSessionRowID,
		"current_device":  principal.DesktopSessionID,
	})
}

func (s *Server) handleRevokeSession(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.requirePrincipal(w, r)
	if !ok {
		return
	}
	id := Param(r, "id")
	if id == "" {
		WriteError(w, http.StatusBadRequest, "Session id is required.")
		return
	}

	revoked, err := s.auth.RevokeSession(r.Context(), principal.UserID, id)
	if err != nil {
		s.serverError(w, "Could not revoke that session.", err)
		return
	}
	if !revoked {
		WriteError(w, http.StatusNotFound, "No such session for this account.")
		return
	}
	if principal.WebSessionRowID == id {
		s.clearSessionCookie(w)
	}
	WriteJSON(w, http.StatusOK, map[string]bool{"success": true})
}

// writeAuthError translates a service failure into a status code.
func (s *Server) writeAuthError(w http.ResponseWriter, err error, fallback string) {
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials),
		errors.Is(err, auth.ErrTokenInvalid),
		errors.Is(err, auth.ErrNotAuthenticated),
		errors.Is(err, auth.ErrPasswordIncorrect):
		WriteError(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, auth.ErrAccountLocked):
		WriteError(w, http.StatusLocked, err.Error())
	case errors.Is(err, auth.ErrEmailTaken):
		WriteError(w, http.StatusConflict, err.Error())
	case errors.Is(err, auth.ErrAccountLinkRequired):
		WriteError(w, http.StatusConflict, err.Error())
	case errors.Is(err, auth.ErrSignupDisabled):
		WriteError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, auth.ErrWeakPassword),
		errors.Is(err, auth.ErrInvalidEmail),
		errors.Is(err, auth.ErrInvalidUsername),
		errors.Is(err, auth.ErrInvalidAvatarURL):
		WriteError(w, http.StatusBadRequest, err.Error())
	default:
		s.serverError(w, fallback, err)
	}
}

// serverError logs the real cause and answers with a generic message, so internals such as a
// connection string never reach a client.
func (s *Server) serverError(w http.ResponseWriter, message string, err error) {
	s.logger.Error("request failed", slog.String("error", err.Error()), slog.String("message", message))
	WriteErrorDetails(w, http.StatusInternalServerError, message, safeDetail(err, s.cfg.DebugErrors))
}

// safeDetail exposes the underlying error only when the operator explicitly enabled
// DEBUG_ERRORS; the environment name alone is not consent to leak internals.
func safeDetail(err error, expose bool) string {
	if !expose || err == nil {
		return ""
	}
	return err.Error()
}

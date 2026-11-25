package httpx

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"animind-backend-go/internal/auth"
)

// desktopAuthLoopbackPort is the fixed port the Qt player listens on (kBridgePort in
// auth_manager.cpp). The page never accepts a caller-supplied host or port, so the token can
// only travel to the local player.
const desktopAuthLoopbackPort = 27182

// desktopAuthPage is the browser-side pairing form. Any signed-in account may pair a desktop
// player; this page replaces the old website's handoff.
type desktopAuthPage struct {
	CSRF           string
	Error          string
	Email          string
	SignedIn       bool
	GoogleEnabled  bool
	GoogleStartURL string
}

// googleStartURL builds the browser Google entry point for a return path on this API, or ""
// when Google is not configured. One helper keeps a dead button off the pairing page.
func (s *Server) googleStartURL(returnPath string) string {
	if s.auth == nil || !s.auth.GoogleEnabled() {
		return ""
	}
	issuer := strings.TrimRight(s.cfg.PublicIssuer(), "/")
	if !strings.HasPrefix(issuer, "http://") && !strings.HasPrefix(issuer, "https://") {
		return ""
	}
	return "/api/auth/google/start?redirect=" + url.QueryEscape(issuer+returnPath)
}

// handleDesktopAuthPage renders the sign-in form, or a confirmation step when a session
// already exists. The confirmation matters: a GET that auto-redirected would let a malicious
// page drive a signed-in browser into a local listener without the user acting.
func (s *Server) handleDesktopAuthPage(w http.ResponseWriter, r *http.Request) {
	principal := s.currentWebPrincipal(r)
	data := desktopAuthPage{CSRF: s.adminCSRFToken(w, r)}
	if principal != nil {
		if !principal.IsWeb() {
			s.renderAdminMessage(w, http.StatusBadRequest, "Use a browser session",
				"Open this page in a browser and sign in there, then try pairing again.", "/desktop-auth", "Back")
			return
		}
		if r.URL.Query().Get("auth_code") != "" {
			// The Google callback already set the session cookie; strip the one-time code
			// from the address bar rather than leaving it in history.
			s.redirectAdmin(w, r, "/desktop-auth")
			return
		}
		data.SignedIn = true
		data.Email = principal.Email
	} else {
		data.GoogleStartURL = s.googleStartURL("/desktop-auth")
		data.GoogleEnabled = data.GoogleStartURL != ""
	}
	s.renderAdmin(w, r, http.StatusOK, "desktop_auth", data)
}

// handleDesktopAuthConfirm mints the bridge token after the user explicitly confirms. A POST
// with the CSRF token keeps the handoff out of reach of a cross-site GET.
func (s *Server) handleDesktopAuthConfirm(w http.ResponseWriter, r *http.Request) {
	principal := s.currentWebPrincipal(r)
	if principal == nil {
		s.redirectAdmin(w, r, "/desktop-auth")
		return
	}
	if !principal.IsWeb() {
		s.renderAdminMessage(w, http.StatusBadRequest, "Use a browser session",
			"Open this page in a browser and sign in there, then try pairing again.", "/desktop-auth", "Back")
		return
	}
	token, err := s.auth.BridgeToken(r.Context(), principal)
	if err != nil {
		s.logger.Error("could not create a desktop bridge token", slog.String("error", err.Error()))
		s.renderAdminMessage(w, http.StatusInternalServerError, "Pairing failed",
			"The pairing token could not be created. Check the server log.", "/desktop-auth", "Retry")
		return
	}
	target := fmt.Sprintf("http://127.0.0.1:%d/auth?token=%s&sessionId=%s",
		desktopAuthLoopbackPort, url.QueryEscape(token), url.QueryEscape(principal.WebSessionRowID))
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// handleDesktopAuthLogin signs a browser in for the handoff. It accepts any account, unlike
// the admin login, and sends the browser back to /desktop-auth for confirmation.
func (s *Server) handleDesktopAuthLogin(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		s.renderAdminMessage(w, http.StatusServiceUnavailable, "Authentication unavailable",
			"The authentication service is not configured.", "/desktop-auth", "Back")
		return
	}
	email := strings.TrimSpace(r.PostFormValue("email"))
	password := r.PostFormValue("password")
	if email == "" || password == "" {
		s.renderAdmin(w, r, http.StatusBadRequest, "desktop_auth", desktopAuthPage{
			CSRF: s.adminCSRFToken(w, r), Error: "Email and password are required.", Email: email,
		})
		return
	}

	loginCtx := auth.WithLoginSource(s.withClient(r), ClientIP(r, s.cfg.TrustProxy))
	result, err := s.auth.Login(loginCtx, email, password)
	if err != nil {
		status := http.StatusUnauthorized
		message := "Invalid email or password."
		if errors.Is(err, auth.ErrAccountLocked) {
			status = http.StatusLocked
			message = err.Error()
		} else if !errors.Is(err, auth.ErrInvalidCredentials) {
			s.logger.Warn("desktop pairing login failed", slog.String("error", err.Error()))
		}
		s.renderAdmin(w, r, status, "desktop_auth", desktopAuthPage{
			CSRF: s.adminCSRFToken(w, r), Error: message, Email: email,
		})
		return
	}

	s.setSessionCookie(w, result.SessionToken, result.ExpiresAt, false)
	s.redirectAdmin(w, r, "/desktop-auth")
}

// currentWebPrincipal resolves the browser cookie without requiring an admin flag.
func (s *Server) currentWebPrincipal(r *http.Request) *auth.Principal {
	if s.auth == nil {
		return nil
	}
	token := s.credential(r)
	if token == "" {
		return nil
	}
	principal, err := s.auth.Authenticate(auth.WithClient(r.Context(), auth.Client{
		UserAgent: r.UserAgent(), IPAddress: ClientIP(r, s.cfg.TrustProxy),
	}), token)
	if err != nil {
		return nil
	}
	return principal
}

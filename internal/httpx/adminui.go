package httpx

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"animind-backend-go/internal/auth"
	"animind-backend-go/internal/repository"
	"animind-backend-go/internal/scanner"
)

// The admin console is server-rendered and embedded in the binary, so the deployment stays a
// single process with no Node build step. Every page requires an administrator session, every
// mutating form a double-submit CSRF token, and the CSP allows only the embedded stylesheet.
//
//go:embed adminui/templates/*.html adminui/static/*.css
var adminAssets embed.FS

const (
	adminCSRFCookie         = "animind_admin_csrf"
	secureAdminCSRFCookie   = "__Host-" + adminCSRFCookie
	adminUsersPerPage       = 50
	adminShowsPerPage       = 50
	adminScanTimeoutMinutes = 30
	adminScanLaunchCooldown = 5 * time.Second
)

var adminTemplates = map[string]*template.Template{}

func init() {
	for _, page := range []string{"dashboard", "users", "user", "library", "confirm"} {
		adminTemplates[page] = template.Must(template.New(page).Funcs(adminTemplateFuncs()).ParseFS(
			adminAssets, "adminui/templates/layout.html", "adminui/templates/"+page+".html"))
	}
	adminTemplates["login"] = template.Must(template.New("login").Funcs(adminTemplateFuncs()).ParseFS(
		adminAssets, "adminui/templates/login.html"))
	adminTemplates["desktop_auth"] = template.Must(template.New("desktop_auth").Funcs(adminTemplateFuncs()).ParseFS(
		adminAssets, "adminui/templates/desktop_auth.html"))
	adminTemplates["message"] = template.Must(template.New("message").Funcs(adminTemplateFuncs()).ParseFS(
		adminAssets, "adminui/templates/message.html"))
}

func adminTemplateFuncs() template.FuncMap {
	return template.FuncMap{
		"initial": func(value string) string {
			value = strings.TrimSpace(value)
			if value == "" {
				return "?"
			}
			runes := []rune(value)
			return strings.ToUpper(string(runes[0]))
		},
		"fmtTime": func(value time.Time) string {
			if value.IsZero() {
				return "—"
			}
			return value.Local().Format("2 Jan 2006, 15:04")
		},
		"deref": func(value *string) string {
			if value == nil {
				return ""
			}
			return *value
		},
		"derefInt": func(value *int) int {
			if value == nil {
				return 0
			}
			return *value
		},
		"clientLabel": func(userAgent *string) string {
			if userAgent == nil {
				return "Unknown"
			}
			return adminClientLabel(*userAgent)
		},
		"durationMS": func(ms int64) string {
			if ms < 1000 {
				return fmt.Sprintf("%d ms", ms)
			}
			return (time.Duration(ms) * time.Millisecond).Round(time.Second).String()
		},
	}
}

// adminClientLabel turns a stored user agent into a short platform label. A session created
// before client metadata was recorded has no user agent and stays "Unknown", never guessed.
func adminClientLabel(userAgent string) string {
	lowered := strings.ToLower(userAgent)
	switch {
	case strings.Contains(lowered, "okhttp"), strings.Contains(lowered, "animind-kotlin"):
		return "Android app"
	case strings.Contains(lowered, "iphone"), strings.Contains(lowered, "ipad"), strings.Contains(lowered, "ios"):
		return "iOS app"
	case strings.Contains(lowered, "android"):
		return "Android"
	case strings.Contains(lowered, "animind-qt"), strings.Contains(lowered, "animind-desktop"):
		return "Desktop player"
	case lowered == "":
		return "Unknown"
	default:
		return "Browser"
	}
}

// ── View models ──────────────────────────────────────────────────────────────

type adminChrome struct {
	Title          string
	Subtitle       string
	Active         string
	AdminEmail     string
	CSRF           string
	Flash          string
	Error          string
	RefreshSeconds int
}

type adminDashboard struct {
	adminChrome
	Stats          repository.UserStats
	WebSessions    int64
	DesktopDevices int64
	Shows          int64
	Episodes       int64
	ActiveParties  int64
	Peers          int
	Rooms          int
	Scan           scanner.Status
	RecentUsers    []repository.PublicUser
	StorageMode    string
	Env            string
	PublicURL      string
	Uptime         string
}

type adminUsersPage struct {
	adminChrome
	Users    []repository.AdminUserRow
	Search   string
	Total    int64
	Page     int
	Pages    int
	PrevPage int
	NextPage int
	HasPrev  bool
	HasNext  bool
}

type adminUserPage struct {
	adminChrome
	User           *repository.User
	IsSelf         bool
	WebSessions    []repository.WebSession
	DesktopDevices []repository.DesktopSessionDevice
}

type adminLibraryPage struct {
	adminChrome
	Shows      []repository.Show
	Search     string
	Total      int64
	Page       int
	Pages      int
	PrevPage   int
	NextPage   int
	HasPrev    bool
	HasNext    bool
	Scan       scanner.Status
	CanCleanup bool
}

type adminConfirmPage struct {
	adminChrome
	Heading      string
	Body         string
	Entity       string
	Action       string
	ConfirmLabel string
	CancelHref   string
}

type adminMessagePage struct {
	Title     string
	Heading   string
	Body      string
	BackHref  string
	BackLabel string
}

type adminLoginPage struct {
	Title  string
	CSRF   string
	Error  string
	Email  string
	Notice string
}

// ── Routes and middleware ────────────────────────────────────────────────────

// adminSecurityHeaders apply to the console and the desktop pairing page, including the
// stylesheet. The CSP has no script and no inline style source, so a compromised page cannot
// exfiltrate a session through an injected script.
func adminSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy",
			"default-src 'none'; style-src 'self'; img-src 'self' data:; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) redirectAdminLogin(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

func (s *Server) redirectAdmin(w http.ResponseWriter, r *http.Request, path string) {
	// Keep the helper same-origin even if a future caller passes a tainted value: only
	// /admin and /desktop-auth paths are accepted, and //host tricks are refused.
	parsed, err := url.Parse(path)
	if err != nil || parsed.IsAbs() || parsed.Scheme != "" || parsed.Host != "" ||
		(!strings.HasPrefix(parsed.Path, "/admin") && !strings.HasPrefix(parsed.Path, "/desktop-auth")) ||
		strings.Contains(parsed.Path, "..") || strings.Contains(parsed.Path, "//") {
		path = "/admin"
	}
	// #nosec G710 -- path is an internal route validated immediately above.
	http.Redirect(w, r, path, http.StatusSeeOther)
}

// requireAdminPage resolves the browser session, redirects an anonymous visitor to the login
// page, and refuses a signed-in account that is not an administrator.
func (s *Server) requireAdminPage(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.auth == nil {
			s.redirectAdminLogin(w, r)
			return
		}
		token := s.credential(r)
		if token == "" {
			s.redirectAdminLogin(w, r)
			return
		}
		ctx := auth.WithClient(r.Context(), auth.Client{
			UserAgent: r.UserAgent(),
			IPAddress: ClientIP(r, s.cfg.TrustProxy),
		})
		principal, err := s.auth.Authenticate(ctx, token)
		if err != nil {
			s.clearSessionCookie(w)
			s.redirectAdminLogin(w, r)
			return
		}
		if !principal.IsAdmin {
			s.renderAdminMessage(w, http.StatusForbidden, "Administrator access required",
				"This account cannot open the admin console. Sign in with an administrator account.",
				"/admin/login", "Switch account")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(ctx, principal)))
	})
}

// adminCSRF implements double-submit CSRF protection: the token is rendered into the form and
// stored in an HttpOnly cookie, and a Secure deployment uses the __Host- prefix.
func (s *Server) adminCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil {
				s.renderAdminMessage(w, http.StatusBadRequest, "Invalid form",
					"The form could not be read. Reload the page and try again.", "/admin", "Back")
				return
			}
			if !s.validAdminCSRF(r) {
				s.renderAdminMessage(w, http.StatusForbidden, "Security check failed",
					"Reload the page and submit the form again.", "/admin", "Back")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) adminCSRFCookieName() string {
	if s.cfg.Auth.SecureCookies {
		return secureAdminCSRFCookie
	}
	return adminCSRFCookie
}

// adminCSRFToken returns the token for this browser, creating it on first use.
func (s *Server) adminCSRFToken(w http.ResponseWriter, r *http.Request) string {
	name := s.adminCSRFCookieName()
	if cookie, err := r.Cookie(name); err == nil && len(cookie.Value) >= 32 {
		return cookie.Value
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		// A failure here is a broken platform RNG; the request cannot safely proceed.
		s.logger.Error("could not create an admin CSRF token", slog.String("error", err.Error()))
		return ""
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	// Secure follows the deployment config; production startup rejects SecureCookies=false.
	// #nosec G124 -- dynamic Secure is intentional and enforced in production.
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: token, Path: "/", HttpOnly: true,
		Secure: s.cfg.Auth.SecureCookies, SameSite: http.SameSiteStrictMode,
	})
	return token
}

func (s *Server) validAdminCSRF(r *http.Request) bool {
	cookie, err := r.Cookie(s.adminCSRFCookieName())
	if err != nil || len(cookie.Value) < 32 {
		return false
	}
	form := r.PostFormValue("csrf")
	if len(form) < 32 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(form)) == 1
}

// ── Rendering ────────────────────────────────────────────────────────────────

func (s *Server) chrome(w http.ResponseWriter, r *http.Request, title, subtitle, active string) adminChrome {
	email := ""
	if principal := PrincipalFromContext(r.Context()); principal != nil {
		email = principal.Email
	}
	flash, failure := adminFlash(r)
	return adminChrome{
		Title: title, Subtitle: subtitle, Active: active,
		AdminEmail: email, CSRF: s.adminCSRFToken(w, r), Flash: flash, Error: failure,
	}
}

func (s *Server) renderAdmin(w http.ResponseWriter, r *http.Request, status int, page string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	name := "layout"
	switch page {
	case "login", "desktop_auth", "message":
		name = page
	}
	if err := adminTemplates[page].ExecuteTemplate(w, name, data); err != nil {
		s.logger.Error("could not render the admin page",
			slog.String("page", page), slog.String("error", err.Error()))
	}
}

func (s *Server) renderAdminMessage(w http.ResponseWriter, status int, heading, body, backHref, backLabel string) {
	s.renderAdmin(w, nil, status, "message", adminMessagePage{
		Title: heading, Heading: heading, Body: body, BackHref: backHref, BackLabel: backLabel,
	})
}

func (s *Server) renderAdminLogin(w http.ResponseWriter, r *http.Request, status int, message, email string) {
	notice, _ := adminFlash(r)
	s.renderAdmin(w, r, status, "login", adminLoginPage{
		Title: "Sign in", CSRF: s.adminCSRFToken(w, r), Error: message, Email: email, Notice: notice,
	})
}

func adminFlash(r *http.Request) (notice, failure string) {
	notices := map[string]string{
		"saved":           "Changes saved.",
		"session_revoked": "Session revoked.",
		"device_revoked":  "Desktop device revoked.",
		"all_revoked":     "Every session for this account was revoked.",
		"user_deleted":    "Account deleted.",
		"show_deleted":    "Show deleted.",
		"scan_started":    "Library scan started. This page refreshes while it runs.",
		"cache_cleaned":   "Audio cache cleanup finished.",
		"signed_out":      "Signed out.",
	}
	failures := map[string]string{
		"self_demote":         "You cannot remove your own administrator access.",
		"self_delete":         "You cannot delete your own account from this panel.",
		"user_not_found":      "That account no longer exists.",
		"session_not_found":   "That session was already gone.",
		"device_not_found":    "That device was already gone.",
		"revoke_failed":       "The session could not be revoked.",
		"update_failed":       "The account could not be updated.",
		"delete_failed":       "The account could not be deleted.",
		"show_delete_failed":  "The show could not be deleted.",
		"show_not_found":      "That show no longer exists.",
		"scan_unavailable":    "The scanner is not configured.",
		"scan_running":        "A library scan is already running.",
		"scan_cooldown":       "A scan was started moments ago. Try again in a few seconds.",
		"cleanup_unavailable": "The audio cache is only available in local storage mode.",
		"cleanup_failed":      "The audio cache cleanup failed.",
		"flags_missing":       "The access form was incomplete.",
	}
	if r == nil {
		return "", ""
	}
	query := r.URL.Query()
	return notices[query.Get("notice")], failures[query.Get("error")]
}

func adminPagination(page, perPage int, total int64) (pages, prev, next int, hasPrev, hasNext bool) {
	pages = int((total + int64(perPage) - 1) / int64(perPage))
	if pages < 1 {
		pages = 1
	}
	if page > pages {
		page = pages
	}
	if page < 1 {
		page = 1
	}
	prev, next = page-1, page+1
	return pages, prev, next, page > 1, page < pages
}

// adminUserParam validates the account id before it reaches a query or a redirect. A malformed
// id is an ordinary 303 back to the list, not a database error or a raw Location.
func (s *Server) adminUserParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := Param(r, "id")
	if !repository.IsUUID(id) {
		s.redirectAdmin(w, r, "/admin/users?error=user_not_found")
		return "", false
	}
	return id, true
}

// ── Login and logout ─────────────────────────────────────────────────────────

func (s *Server) handleAdminLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.auth != nil {
		if token := s.credential(r); token != "" {
			if principal, err := s.auth.Authenticate(auth.WithClient(r.Context(), auth.Client{
				UserAgent: r.UserAgent(), IPAddress: ClientIP(r, s.cfg.TrustProxy),
			}), token); err == nil && principal.IsAdmin {
				s.redirectAdmin(w, r, "/admin")
				return
			}
		}
	}
	s.renderAdminLogin(w, r, http.StatusOK, "", "")
}

func (s *Server) handleAdminLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		s.renderAdminLogin(w, r, http.StatusServiceUnavailable, "Authentication is not configured.", "")
		return
	}
	email := strings.TrimSpace(r.PostFormValue("email"))
	password := r.PostFormValue("password")
	if email == "" || password == "" {
		s.renderAdminLogin(w, r, http.StatusBadRequest, "Email and password are required.", email)
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
			s.logger.Warn("admin login failed", slog.String("error", err.Error()))
		}
		s.renderAdminLogin(w, r, status, message, email)
		return
	}

	if !result.User.IsAdmin {
		// The password was correct but this is not an operator: revoke the newly minted
		// session rather than leave a live credential.
		if principal, authErr := s.auth.Authenticate(loginCtx, result.SessionToken); authErr == nil {
			_ = s.auth.Logout(r.Context(), principal)
		}
		s.clearSessionCookie(w)
		s.renderAdminLogin(w, r, http.StatusForbidden,
			"This account does not have administrator access.", email)
		return
	}

	s.setSessionCookie(w, result.SessionToken, result.ExpiresAt, false)
	s.redirectAdmin(w, r, "/admin")
}

func (s *Server) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	if principal := PrincipalFromContext(r.Context()); principal != nil && principal.IsWeb() && s.auth != nil {
		if err := s.auth.Logout(r.Context(), principal); err != nil {
			s.logger.Warn("admin logout could not revoke the session", slog.String("error", err.Error()))
		}
	}
	s.clearSessionCookie(w)
	s.redirectAdmin(w, r, "/admin/login?notice=signed_out")
}

// ── Pages ────────────────────────────────────────────────────────────────────

func (s *Server) adminStoreReady(w http.ResponseWriter) bool {
	if s.store == nil {
		s.renderAdminMessage(w, http.StatusServiceUnavailable, "Database not configured",
			"The admin console cannot load data without a database.", "/health", "API health")
		return false
	}
	return true
}

func (s *Server) handleAdminUIDashboard(w http.ResponseWriter, r *http.Request) {
	if !s.adminStoreReady(w) {
		return
	}
	if r.URL.Query().Get("auth_code") != "" {
		// The Google callback set the session cookie; strip the one-time code from the URL.
		s.redirectAdmin(w, r, "/admin")
		return
	}
	ctx := r.Context()
	stats, err := s.store.UserStats(ctx)
	if err != nil {
		s.renderAdminMessage(w, http.StatusInternalServerError, "Could not load accounts",
			"The account summary failed. Check the server log.", "/admin", "Retry")
		return
	}
	web, desktop, err := s.store.SessionCounts(ctx)
	if err != nil {
		s.renderAdminMessage(w, http.StatusInternalServerError, "Could not load sessions",
			"The session summary failed. Check the server log.", "/admin", "Retry")
		return
	}
	shows, episodes, err := s.store.LibraryCounts(ctx)
	if err != nil {
		s.renderAdminMessage(w, http.StatusInternalServerError, "Could not load the library",
			"The library summary failed. Check the server log.", "/admin", "Retry")
		return
	}
	parties, err := s.store.CountActiveParties(ctx)
	if err != nil {
		s.logger.Warn("could not count watch parties", slog.String("error", err.Error()))
	}
	recent, err := s.store.ListUsers(ctx, 5, 0)
	if err != nil {
		s.logger.Warn("could not list recent accounts", slog.String("error", err.Error()))
	}

	scan := scanner.Status{}
	if s.scanner != nil {
		scan = s.scanner.Status()
	}
	peers, rooms := 0, 0
	if s.hub != nil {
		peers, rooms = s.hub.PeerCount(), s.hub.RoomCount()
	}

	chrome := s.chrome(w, r, "Overview", "Accounts, sessions and library at a glance.", "dashboard")
	s.renderAdmin(w, r, http.StatusOK, "dashboard", adminDashboard{
		adminChrome: chrome,
		Stats:       stats, WebSessions: web, DesktopDevices: desktop,
		Shows: shows, Episodes: episodes, ActiveParties: parties,
		Peers: peers, Rooms: rooms, Scan: scan, RecentUsers: recent,
		StorageMode: string(s.cfg.Storage.Mode), Env: s.cfg.Env,
		PublicURL: s.cfg.PublicIssuer(), Uptime: time.Since(s.startedAt).Round(time.Second).String(),
	})
}

func (s *Server) handleAdminUIUsers(w http.ResponseWriter, r *http.Request) {
	if !s.adminStoreReady(w) {
		return
	}
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	page := QueryInt(r, "page", 1, 1, 100_000)
	offset := (page - 1) * adminUsersPerPage

	users, total, err := s.store.ListUsersWithSessions(r.Context(), search, adminUsersPerPage, offset)
	if err != nil {
		s.renderAdminMessage(w, http.StatusInternalServerError, "Could not load accounts",
			"The account list failed. Check the server log.", "/admin", "Back to overview")
		return
	}
	pages, prev, next, hasPrev, hasNext := adminPagination(page, adminUsersPerPage, total)
	if page > pages {
		page = pages
		offset = (page - 1) * adminUsersPerPage
		users, total, err = s.store.ListUsersWithSessions(r.Context(), search, adminUsersPerPage, offset)
		if err != nil {
			s.renderAdminMessage(w, http.StatusInternalServerError, "Could not load accounts",
				"The account list failed. Check the server log.", "/admin", "Back to overview")
			return
		}
		pages, prev, next, hasPrev, hasNext = adminPagination(page, adminUsersPerPage, total)
	}

	s.renderAdmin(w, r, http.StatusOK, "users", adminUsersPage{
		adminChrome: s.chrome(w, r, "Accounts", "Everyone signed in through the mobile app, the desktop player or the web.", "users"),
		Users:       users, Search: search, Total: total,
		Page: page, Pages: pages, PrevPage: prev, NextPage: next, HasPrev: hasPrev, HasNext: hasNext,
	})
}

func (s *Server) handleAdminUIUser(w http.ResponseWriter, r *http.Request) {
	if !s.adminStoreReady(w) {
		return
	}
	user, err := s.store.UserByID(r.Context(), Param(r, "id"))
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			s.renderAdminMessage(w, http.StatusNotFound, "Account not found",
				"That account no longer exists.", "/admin/users", "Back to accounts")
			return
		}
		s.renderAdminMessage(w, http.StatusInternalServerError, "Could not load the account",
			"Check the server log for the database error.", "/admin/users", "Back to accounts")
		return
	}
	web, err := s.store.ListWebSessions(r.Context(), user.ID)
	if err != nil {
		s.renderAdminMessage(w, http.StatusInternalServerError, "Could not load sessions",
			"Check the server log for the database error.", "/admin/users/"+user.ID, "Retry")
		return
	}
	desktop, err := s.store.ListDesktopSessions(r.Context(), user.ID)
	if err != nil {
		s.renderAdminMessage(w, http.StatusInternalServerError, "Could not load devices",
			"Check the server log for the database error.", "/admin/users/"+user.ID, "Retry")
		return
	}
	principal := PrincipalFromContext(r.Context())
	s.renderAdmin(w, r, http.StatusOK, "user", adminUserPage{
		adminChrome:    s.chrome(w, r, user.Username, user.Email, "users"),
		User:           user,
		IsSelf:         principal != nil && principal.UserID == user.ID,
		WebSessions:    web,
		DesktopDevices: desktop,
	})
}

func (s *Server) handleAdminUILibrary(w http.ResponseWriter, r *http.Request) {
	if !s.adminStoreReady(w) {
		return
	}
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	page := QueryInt(r, "page", 1, 1, 100_000)
	offset := (page - 1) * adminShowsPerPage

	shows, _, err := s.store.ListShows(r.Context(), adminShowsPerPage, offset, search, "")
	if err != nil {
		s.renderAdminMessage(w, http.StatusInternalServerError, "Could not load the library",
			"Check the server log for the database error.", "/admin", "Back to overview")
		return
	}
	total, err := s.store.CountShows(r.Context(), search, "")
	if err != nil {
		s.renderAdminMessage(w, http.StatusInternalServerError, "Could not count the library",
			"Check the server log for the database error.", "/admin", "Back to overview")
		return
	}
	pages, prev, next, hasPrev, hasNext := adminPagination(page, adminShowsPerPage, total)
	if page > pages {
		page = pages
		offset = (page - 1) * adminShowsPerPage
		shows, _, err = s.store.ListShows(r.Context(), adminShowsPerPage, offset, search, "")
		if err != nil {
			s.renderAdminMessage(w, http.StatusInternalServerError, "Could not load the library",
				"Check the server log for the database error.", "/admin", "Back to overview")
			return
		}
		total, err = s.store.CountShows(r.Context(), search, "")
		if err != nil {
			s.renderAdminMessage(w, http.StatusInternalServerError, "Could not count the library",
				"Check the server log for the database error.", "/admin", "Back to overview")
			return
		}
		pages, prev, next, hasPrev, hasNext = adminPagination(page, adminShowsPerPage, total)
	}

	scan := scanner.Status{}
	if s.scanner != nil {
		scan = s.scanner.Status()
	}
	chrome := s.chrome(w, r, "Library", "Scan the catalog, inspect indexed shows and clean derived files.", "library")
	if scan.Running {
		chrome.RefreshSeconds = 5
	}
	s.renderAdmin(w, r, http.StatusOK, "library", adminLibraryPage{
		adminChrome: chrome,
		Shows:       shows, Search: search, Total: total,
		Page: page, Pages: pages, PrevPage: prev, NextPage: next, HasPrev: hasPrev, HasNext: hasNext,
		Scan: scan, CanCleanup: s.cfg.IsLocal() && s.audio != nil,
	})
}

// ── Account actions ──────────────────────────────────────────────────────────

func (s *Server) handleAdminUIUserFlags(w http.ResponseWriter, r *http.Request) {
	if !s.adminStoreReady(w) {
		return
	}
	target, ok := s.adminUserParam(w, r)
	if !ok {
		return
	}
	if r.PostFormValue("flags") != "1" {
		s.redirectAdmin(w, r, "/admin/users/"+target+"?error=flags_missing")
		return
	}
	principal := PrincipalFromContext(r.Context())
	isAdmin := r.PostFormValue("is_admin") != ""
	canDownload := r.PostFormValue("can_download") != ""

	// The form disables the admin checkbox for the signed-in account; forcing the flag here
	// keeps the server authoritative even if a client re-enables it.
	if principal != nil && principal.UserID == target {
		isAdmin = true
	}
	if err := s.store.SetUserAccess(r.Context(), target, isAdmin, canDownload); err != nil {
		code := "update_failed"
		if errors.Is(err, repository.ErrNotFound) {
			code = "user_not_found"
		}
		s.redirectAdmin(w, r, "/admin/users/"+target+"?error="+code)
		return
	}
	if s.auth != nil {
		s.auth.InvalidateUser(r.Context(), target)
	}
	if principal != nil {
		s.logger.Info("admin updated account access",
			slog.String("admin_id", principal.UserID), slog.String("target_id", target),
			slog.Bool("is_admin", isAdmin), slog.Bool("can_download", canDownload))
	}
	s.redirectAdmin(w, r, "/admin/users/"+target+"?notice=saved")
}

func (s *Server) handleAdminRevokeWebSession(w http.ResponseWriter, r *http.Request) {
	if !s.adminStoreReady(w) {
		return
	}
	target, ok := s.adminUserParam(w, r)
	if !ok {
		return
	}
	sessionID := Param(r, "sessionId")
	if err := s.store.RevokeWebSession(r.Context(), target, sessionID); err != nil {
		code := "revoke_failed"
		if errors.Is(err, repository.ErrNotFound) {
			code = "session_not_found"
		}
		s.redirectAdmin(w, r, "/admin/users/"+target+"?error="+code)
		return
	}
	if s.auth != nil {
		// Drop the cached identity for the revoked token; otherwise a request inside the
		// cache window could still authenticate it.
		s.auth.InvalidateUser(r.Context(), target)
	}
	principal := PrincipalFromContext(r.Context())
	if principal != nil && principal.UserID == target && principal.WebSessionRowID == sessionID {
		s.clearSessionCookie(w)
		s.redirectAdmin(w, r, "/admin/login?notice=signed_out")
		return
	}
	s.redirectAdmin(w, r, "/admin/users/"+target+"?notice=session_revoked")
}

func (s *Server) handleAdminRevokeDesktop(w http.ResponseWriter, r *http.Request) {
	if !s.adminStoreReady(w) {
		return
	}
	target, ok := s.adminUserParam(w, r)
	if !ok {
		return
	}
	sessionID := Param(r, "sessionId")
	revoked, err := s.store.RevokeDesktopSessionByID(r.Context(), sessionID, target)
	if err != nil || !revoked {
		code := "revoke_failed"
		if err == nil {
			code = "device_not_found"
		}
		s.redirectAdmin(w, r, "/admin/users/"+target+"?error="+code)
		return
	}
	if s.auth != nil {
		s.auth.InvalidateUser(r.Context(), target)
	}
	s.redirectAdmin(w, r, "/admin/users/"+target+"?notice=device_revoked")
}

func (s *Server) handleAdminRevokeAllSessions(w http.ResponseWriter, r *http.Request) {
	if !s.adminStoreReady(w) {
		return
	}
	target, ok := s.adminUserParam(w, r)
	if !ok {
		return
	}
	principal := PrincipalFromContext(r.Context())
	if err := s.store.RevokeAllWebSessions(r.Context(), target, ""); err != nil {
		s.redirectAdmin(w, r, "/admin/users/"+target+"?error=revoke_failed")
		return
	}
	if err := s.store.RevokeAllDesktopSessions(r.Context(), target); err != nil {
		s.redirectAdmin(w, r, "/admin/users/"+target+"?error=revoke_failed")
		return
	}
	if s.auth != nil {
		s.auth.InvalidateUser(r.Context(), target)
	}
	if principal != nil {
		s.logger.Info("admin revoked every session",
			slog.String("admin_id", principal.UserID), slog.String("target_id", target))
	}
	if principal != nil && principal.UserID == target {
		s.clearSessionCookie(w)
		s.redirectAdmin(w, r, "/admin/login?notice=signed_out")
		return
	}
	s.redirectAdmin(w, r, "/admin/users/"+target+"?notice=all_revoked")
}

func (s *Server) handleAdminDeleteUser(w http.ResponseWriter, r *http.Request) {
	if !s.adminStoreReady(w) {
		return
	}
	target, ok := s.adminUserParam(w, r)
	if !ok {
		return
	}
	principal := PrincipalFromContext(r.Context())
	if principal != nil && principal.UserID == target {
		s.redirectAdmin(w, r, "/admin/users/"+target+"?error=self_delete")
		return
	}
	if err := s.store.DeleteUser(r.Context(), target); err != nil {
		code := "delete_failed"
		if errors.Is(err, repository.ErrNotFound) {
			code = "user_not_found"
		}
		s.redirectAdmin(w, r, "/admin/users/"+target+"?error="+code)
		return
	}
	if s.auth != nil {
		s.auth.InvalidateUser(r.Context(), target)
	}
	if principal != nil {
		s.logger.Info("admin deleted account",
			slog.String("admin_id", principal.UserID), slog.String("target_id", target))
	}
	s.redirectAdmin(w, r, "/admin/users?notice=user_deleted")
}

// ── Library actions ──────────────────────────────────────────────────────────

func (s *Server) handleAdminUIScan(w http.ResponseWriter, r *http.Request) {
	if s.scanner == nil {
		s.redirectAdmin(w, r, "/admin/library?error=scan_unavailable")
		return
	}
	status := s.scanner.Status()
	if status.Running {
		s.redirectAdmin(w, r, "/admin/library?error=scan_running")
		return
	}
	if !status.StartedAt.IsZero() && time.Since(status.StartedAt) < adminScanLaunchCooldown {
		s.redirectAdmin(w, r, "/admin/library?error=scan_cooldown")
		return
	}

	if principal := PrincipalFromContext(r.Context()); principal != nil {
		s.logger.Info("admin started library scan", slog.String("admin_id", principal.UserID))
	}
	// The scan must outlive the request; the timeout bounds the detached work.
	// #nosec G118 -- background work deliberately uses a detached context.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(adminScanTimeoutMinutes)*time.Minute)
		defer cancel()
		if _, err := s.runLibraryScan(ctx); err != nil {
			s.logger.Error("admin library scan failed", slog.String("error", err.Error()))
		}
	}()
	s.redirectAdmin(w, r, "/admin/library?notice=scan_started")
}

func (s *Server) handleAdminUICleanup(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.IsLocal() || s.audio == nil {
		s.redirectAdmin(w, r, "/admin/library?error=cleanup_unavailable")
		return
	}
	if _, err := s.audio.CleanupByTTL(s.cfg.AudioCache.VariantTTL); err != nil {
		s.logger.Error("admin audio cache cleanup failed", slog.String("error", err.Error()))
		s.redirectAdmin(w, r, "/admin/library?error=cleanup_failed")
		return
	}
	if principal := PrincipalFromContext(r.Context()); principal != nil {
		s.logger.Info("admin cleaned the audio cache", slog.String("admin_id", principal.UserID))
	}
	s.redirectAdmin(w, r, "/admin/library?notice=cache_cleaned")
}

func (s *Server) handleAdminUIDeleteShow(w http.ResponseWriter, r *http.Request) {
	if !s.adminStoreReady(w) {
		return
	}
	id := Param(r, "id")
	if err := s.store.DeleteShow(r.Context(), id); err != nil {
		code := "show_delete_failed"
		if errors.Is(err, repository.ErrNotFound) {
			code = "show_not_found"
		}
		s.redirectAdmin(w, r, "/admin/library?error="+code)
		return
	}
	if principal := PrincipalFromContext(r.Context()); principal != nil {
		s.logger.Info("admin deleted show",
			slog.String("admin_id", principal.UserID), slog.String("show_id", id))
	}
	s.redirectAdmin(w, r, "/admin/library?notice=show_deleted")
}

// ── Confirmation pages ───────────────────────────────────────────────────────

func (s *Server) handleAdminConfirmRevokeAll(w http.ResponseWriter, r *http.Request) {
	if !s.adminStoreReady(w) {
		return
	}
	user, err := s.store.UserByID(r.Context(), Param(r, "id"))
	if err != nil {
		s.renderAdminMessage(w, http.StatusNotFound, "Account not found",
			"That account no longer exists.", "/admin/users", "Back to accounts")
		return
	}
	web, _ := s.store.ListWebSessions(r.Context(), user.ID)
	desktop, _ := s.store.ListDesktopSessions(r.Context(), user.ID)
	s.renderAdmin(w, r, http.StatusOK, "confirm", adminConfirmPage{
		adminChrome:  s.chrome(w, r, "Revoke every session", "", "users"),
		Heading:      "Revoke every session?",
		Body:         fmt.Sprintf("This signs %s out of every browser, app and desktop player. They can sign in again with their password.", user.Email),
		Entity:       fmt.Sprintf("%d web/app session(s) and %d desktop device(s)", len(web), len(desktop)),
		Action:       "/admin/users/" + user.ID + "/sessions/revoke-all",
		ConfirmLabel: "Revoke all sessions",
		CancelHref:   "/admin/users/" + user.ID,
	})
}

func (s *Server) handleAdminConfirmDeleteUser(w http.ResponseWriter, r *http.Request) {
	if !s.adminStoreReady(w) {
		return
	}
	user, err := s.store.UserByID(r.Context(), Param(r, "id"))
	if err != nil {
		s.renderAdminMessage(w, http.StatusNotFound, "Account not found",
			"That account no longer exists.", "/admin/users", "Back to accounts")
		return
	}
	principal := PrincipalFromContext(r.Context())
	if principal != nil && principal.UserID == user.ID {
		s.redirectAdmin(w, r, "/admin/users/"+user.ID+"?error=self_delete")
		return
	}
	s.renderAdmin(w, r, http.StatusOK, "confirm", adminConfirmPage{
		adminChrome:  s.chrome(w, r, "Delete account", "", "users"),
		Heading:      "Delete this account?",
		Body:         "This permanently removes the account and every watchlist entry, resume position, history row, session and device that belongs to it. This cannot be undone.",
		Entity:       user.Email,
		Action:       "/admin/users/" + user.ID + "/delete",
		ConfirmLabel: "Delete account",
		CancelHref:   "/admin/users/" + user.ID,
	})
}

func (s *Server) handleAdminConfirmDeleteShow(w http.ResponseWriter, r *http.Request) {
	if !s.adminStoreReady(w) {
		return
	}
	show, err := s.store.ShowByIDText(r.Context(), Param(r, "id"))
	if err != nil {
		s.renderAdminMessage(w, http.StatusNotFound, "Show not found",
			"That show no longer exists.", "/admin/library", "Back to library")
		return
	}
	s.renderAdmin(w, r, http.StatusOK, "confirm", adminConfirmPage{
		adminChrome:  s.chrome(w, r, "Delete show", "", "library"),
		Heading:      "Delete this show?",
		Body:         "The show and all of its indexed episodes are removed. The media files are not touched; a later scan can index them again.",
		Entity:       show.Title,
		Action:       "/admin/shows/" + show.ID + "/delete",
		ConfirmLabel: "Delete show",
		CancelHref:   "/admin/library",
	})
}

// ── Static assets ────────────────────────────────────────────────────────────

func (s *Server) handleAdminCSS(w http.ResponseWriter, r *http.Request) {
	data, err := adminAssets.ReadFile("adminui/static/admin.css")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

package httpx

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func adminFormRequest(t *testing.T, server *Server, method, path string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	request := httptest.NewRequest(method, path, body)
	if form != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	server.Router().ServeHTTP(recorder, request)
	return recorder
}

func adminCookie(recorder *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}

func TestAdminRedirectsAnonymousToLogin(t *testing.T) {
	server := testServer(t)
	for _, path := range []string{"/admin", "/admin/"} {
		recorder := adminFormRequest(t, server, http.MethodGet, path, nil)
		if recorder.Code != http.StatusSeeOther {
			t.Fatalf("GET %s = %d, want 303", path, recorder.Code)
		}
		if location := recorder.Header().Get("Location"); location != "/admin/login" {
			t.Fatalf("GET %s Location = %q, want /admin/login", path, location)
		}
	}
}

func TestAdminRootRedirectsToConsole(t *testing.T) {
	server := testServer(t)
	recorder := adminFormRequest(t, server, http.MethodGet, "/", nil)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("GET / = %d, want 303", recorder.Code)
	}
	if location := recorder.Header().Get("Location"); location != "/admin" {
		t.Fatalf("Location = %q, want /admin", location)
	}
}

func TestAdminLoginPageSetsCSRFCookieAndCSP(t *testing.T) {
	server := testServer(t)
	recorder := adminFormRequest(t, server, http.MethodGet, "/admin/login", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /admin/login = %d, want 200", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "Sign in") || !strings.Contains(body, `name="csrf"`) {
		t.Fatalf("login page did not render the form:\n%s", body)
	}
	if cookie := adminCookie(recorder, adminCSRFCookie); cookie == nil || len(cookie.Value) < 32 {
		t.Fatal("login page did not set a CSRF cookie")
	}
	csp := recorder.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("unexpected CSP: %q", csp)
	}
}

func TestAdminStylesheetIsServed(t *testing.T) {
	server := testServer(t)
	recorder := adminFormRequest(t, server, http.MethodGet, "/admin/static/admin.css", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /admin/static/admin.css = %d, want 200", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/css") {
		t.Fatalf("Content-Type = %q, want text/css", got)
	}
	if !strings.Contains(recorder.Body.String(), ".sidebar") {
		t.Fatal("stylesheet body is empty or unexpected")
	}
}

func TestAdminRejectsNonAdminSession(t *testing.T) {
	server, _, service := sharedHarness(t)
	acct := signUpAccount(t, service)

	recorder := adminFormRequest(t, server, http.MethodGet, "/admin", nil,
		&http.Cookie{Name: SessionCookieName, Value: acct.token})
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("GET /admin as a non-admin = %d, want 403", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "Administrator access required") {
		t.Fatalf("unexpected body: %s", recorder.Body.String())
	}
}

func TestAdminDashboardLoadsForAdmin(t *testing.T) {
	server, store, service := sharedHarness(t)
	acct := signUpAccount(t, service)
	userID := meUserID(t, server, acct)
	promoteAdmin(t, store, service, userID)

	recorder := adminFormRequest(t, server, http.MethodGet, "/admin", nil,
		&http.Cookie{Name: SessionCookieName, Value: acct.token})
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /admin = %d, body %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "Overview") || !strings.Contains(body, acct.email) {
		t.Fatalf("dashboard did not render the account: %s", body)
	}
	if !strings.Contains(body, "Library scan") {
		t.Fatal("dashboard is missing the scanner card")
	}
}

func TestAdminLoginFormEstablishesAnAdminSession(t *testing.T) {
	server, store, service := sharedHarness(t)
	acct := signUpAccount(t, service)
	userID := meUserID(t, server, acct)
	promoteAdmin(t, store, service, userID)

	page := adminFormRequest(t, server, http.MethodGet, "/admin/login", nil)
	csrf := adminCookie(page, adminCSRFCookie)
	if csrf == nil {
		t.Fatal("login page did not set a CSRF cookie")
	}

	recorder := adminFormRequest(t, server, http.MethodPost, "/admin/login", url.Values{
		"csrf":     {csrf.Value},
		"email":    {acct.email},
		"password": {testAccountPassword},
	}, csrf)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("POST /admin/login = %d, body %s", recorder.Code, recorder.Body.String())
	}
	session := adminCookie(recorder, SessionCookieName)
	if session == nil {
		t.Fatal("login did not set a session cookie")
	}

	dashboard := adminFormRequest(t, server, http.MethodGet, "/admin", nil, session)
	if dashboard.Code != http.StatusOK {
		t.Fatalf("dashboard after login = %d, body %s", dashboard.Code, dashboard.Body.String())
	}
}

func TestAdminUsersPageAndDetail(t *testing.T) {
	server, store, service := sharedHarness(t)
	admin := signUpAccount(t, service)
	adminID := meUserID(t, server, admin)
	promoteAdmin(t, store, service, adminID)
	target := signUpAccount(t, service)
	targetID := meUserID(t, server, target)
	cookie := &http.Cookie{Name: SessionCookieName, Value: admin.token}

	list := adminFormRequest(t, server, http.MethodGet, "/admin/users", nil, cookie)
	if list.Code != http.StatusOK {
		t.Fatalf("GET /admin/users = %d, body %s", list.Code, list.Body.String())
	}
	if !strings.Contains(list.Body.String(), target.email) {
		t.Fatal("the account list is missing a live account")
	}

	detail := adminFormRequest(t, server, http.MethodGet, "/admin/users/"+targetID, nil, cookie)
	if detail.Code != http.StatusOK {
		t.Fatalf("GET /admin/users/{id} = %d, body %s", detail.Code, detail.Body.String())
	}
	body := detail.Body.String()
	if !strings.Contains(body, "App and browser sessions") || !strings.Contains(body, "Desktop devices") {
		t.Fatal("the account page is missing the session and device sections")
	}
}

func TestAdminFormsRequireCSRF(t *testing.T) {
	server, store, service := sharedHarness(t)
	admin := signUpAccount(t, service)
	adminID := meUserID(t, server, admin)
	promoteAdmin(t, store, service, adminID)
	target := signUpAccount(t, service)
	targetID := meUserID(t, server, target)

	dashboard := adminFormRequest(t, server, http.MethodGet, "/admin", nil,
		&http.Cookie{Name: SessionCookieName, Value: admin.token})
	csrf := adminCookie(dashboard, adminCSRFCookie)
	if csrf == nil {
		t.Fatal("dashboard did not set a CSRF cookie")
	}
	session := &http.Cookie{Name: SessionCookieName, Value: admin.token}
	path := "/admin/users/" + targetID + "/flags"

	missing := adminFormRequest(t, server, http.MethodPost, path, url.Values{
		"flags": {"1"}, "can_download": {"on"},
	}, session)
	if missing.Code != http.StatusForbidden {
		t.Fatalf("POST without a CSRF token = %d, want 403", missing.Code)
	}

	ok := adminFormRequest(t, server, http.MethodPost, path, url.Values{
		"csrf": {csrf.Value}, "flags": {"1"}, "can_download": {"on"},
	}, session, csrf)
	if ok.Code != http.StatusSeeOther {
		t.Fatalf("POST with a CSRF token = %d, body %s", ok.Code, ok.Body.String())
	}
	allowed, err := store.CanDownload(context.Background(), targetID)
	if err != nil {
		t.Fatalf("CanDownload: %v", err)
	}
	if !allowed {
		t.Fatal("the entitlement change did not persist")
	}
}

func TestAdminSessionRevocation(t *testing.T) {
	server, store, service := sharedHarness(t)
	admin := signUpAccount(t, service)
	adminID := meUserID(t, server, admin)
	promoteAdmin(t, store, service, adminID)
	target := signUpAccount(t, service)
	targetID := meUserID(t, server, target)

	sessions, err := store.ListWebSessions(context.Background(), targetID)
	if err != nil || len(sessions) == 0 {
		t.Fatalf("ListWebSessions: %v (n=%d)", err, len(sessions))
	}
	targetSession := sessions[0]

	dashboard := adminFormRequest(t, server, http.MethodGet, "/admin", nil,
		&http.Cookie{Name: SessionCookieName, Value: admin.token})
	csrf := adminCookie(dashboard, adminCSRFCookie)
	if csrf == nil {
		t.Fatal("dashboard did not set a CSRF cookie")
	}
	recorder := adminFormRequest(t, server, http.MethodPost,
		fmt.Sprintf("/admin/users/%s/sessions/%s/revoke", targetID, targetSession.ID),
		url.Values{"csrf": {csrf.Value}},
		&http.Cookie{Name: SessionCookieName, Value: admin.token}, csrf)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("revoke = %d, body %s", recorder.Code, recorder.Body.String())
	}
	remaining, err := store.ListWebSessions(context.Background(), targetID)
	if err != nil {
		t.Fatalf("ListWebSessions: %v", err)
	}
	for _, session := range remaining {
		if session.ID == targetSession.ID {
			t.Fatal("the revoked session is still live")
		}
	}
}

func TestDesktopAuthPageRendersForAnonymous(t *testing.T) {
	server := testServer(t)
	recorder := adminFormRequest(t, server, http.MethodGet, "/desktop-auth", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /desktop-auth = %d, want 200", recorder.Code)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "Pair this desktop") || !strings.Contains(body, `name="csrf"`) {
		t.Fatalf("pairing page did not render the form:\n%s", body)
	}
	if cookie := adminCookie(recorder, adminCSRFCookie); cookie == nil {
		t.Fatal("pairing page did not set a CSRF cookie")
	}
}

func TestDesktopAuthPairsAnyAccount(t *testing.T) {
	server, _, service := sharedHarness(t)
	acct := signUpAccount(t, service) // not an administrator
	page := adminFormRequest(t, server, http.MethodGet, "/desktop-auth", nil)
	csrf := adminCookie(page, adminCSRFCookie)
	if csrf == nil {
		t.Fatal("pairing page did not set a CSRF cookie")
	}
	login := adminFormRequest(t, server, http.MethodPost, "/desktop-auth/login", url.Values{
		"csrf":     {csrf.Value},
		"email":    {acct.email},
		"password": {testAccountPassword},
	}, csrf)
	if login.Code != http.StatusSeeOther {
		t.Fatalf("POST /desktop-auth/login = %d, body %s", login.Code, login.Body.String())
	}
	session := adminCookie(login, SessionCookieName)
	if session == nil {
		t.Fatal("pairing login did not set a session cookie")
	}

	pairPage := adminFormRequest(t, server, http.MethodGet, "/desktop-auth", nil, session)
	if pairPage.Code != http.StatusOK {
		t.Fatalf("GET /desktop-auth after login = %d, body %s", pairPage.Code, pairPage.Body.String())
	}
	if !strings.Contains(pairPage.Body.String(), "Pair this desktop") {
		t.Fatal("the confirmation page did not render")
	}

	pair := adminFormRequest(t, server, http.MethodPost, "/desktop-auth/confirm",
		url.Values{"csrf": {csrf.Value}}, session, csrf)
	if pair.Code != http.StatusSeeOther {
		t.Fatalf("POST /desktop-auth/confirm = %d, body %s", pair.Code, pair.Body.String())
	}
	location := pair.Header().Get("Location")
	if !strings.HasPrefix(location, "http://127.0.0.1:27182/auth?token=") || !strings.Contains(location, "&sessionId=") {
		t.Fatalf("pairing redirect = %q", location)
	}
}

// TestLegacyRescanRouteIsGone pins the removal of the old website's scan endpoint.
func TestLegacyRescanRouteIsGone(t *testing.T) {
	server := testServer(t)
	recorder := adminFormRequest(t, server, http.MethodPost, "/api/rescan", nil)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("POST /api/rescan = %d, want 404", recorder.Code)
	}
}

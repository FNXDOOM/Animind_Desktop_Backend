package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"animind-backend-go/internal/config"
)

// Google sign-in, implemented directly against Google's OAuth 2.0 endpoints.
//
// The original backend delegated this to a hosted auth service. Speaking OAuth
// ourselves keeps the "no third-party identity provider" rule while still letting site
// users click "continue with Google".
//
// Accounts are matched by Google subject first and email second, so a user who signed
// up with a password and later clicks Google gets one account rather than two.

const (
	googleAuthEndpoint     = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenEndpoint    = "https://oauth2.googleapis.com/token"
	googleUserinfoEndpoint = "https://openidconnect.googleapis.com/v1/userinfo"
	googleScopes           = "openid email profile"
)

// GoogleProfile is the verified identity returned by the userinfo endpoint.
type GoogleProfile struct {
	Subject   string `json:"sub"`
	Email     string `json:"email"`
	EmailTrue *bool  `json:"email_verified"`
	Name      string `json:"name"`
	Picture   string `json:"picture"`
}

// Google is an OAuth client for the authorization-code flow.
type Google struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
	HTTP         *http.Client
	Enabled      bool
	Logger       *slog.Logger
}

// NewGoogle builds the provider from configuration. Enabled is false when the client
// credentials are absent, which lets the rest of the backend offer the button or not.
func NewGoogle(cfg *config.Config, logger *slog.Logger) *Google {
	a := cfg.Auth
	enabled := a.GoogleClientID != "" && a.GoogleClientSecret != ""
	redirect := a.GoogleRedirectURL
	if redirect == "" {
		// Derive the callback from the first configured site origin so a deployment
		// only needs to register one URL with Google.
		if len(cfg.FrontendOrigins) > 0 && cfg.FrontendOrigins[0] != "*" {
			redirect = strings.TrimSuffix(cfg.FrontendOrigins[0], "/") + "/api/auth/google/callback"
		}
	}
	return &Google{
		ClientID:     a.GoogleClientID,
		ClientSecret: a.GoogleClientSecret,
		RedirectURL:  redirect,
		Enabled:      enabled,
		HTTP:         &http.Client{Timeout: 10 * time.Second},
		Logger:       logger,
	}
}

// AuthURL builds the redirect target for the browser.
//
// prompt=select_account matches the behaviour users expect from a "continue with Google"
// button: an already-signed-in browser still offers the account picker.
func (g *Google) AuthURL(state string) string {
	params := url.Values{}
	params.Set("client_id", g.ClientID)
	params.Set("redirect_uri", g.RedirectURL)
	params.Set("response_type", "code")
	params.Set("scope", googleScopes)
	params.Set("state", state)
	params.Set("prompt", "select_account")
	return googleAuthEndpoint + "?" + params.Encode()
}

// ErrGoogleRejected reports a provider-side failure; the detail goes to the log, never
// to the browser, because the response may contain account information.
var ErrGoogleRejected = errors.New("auth: Google sign-in was rejected")

// Exchange trades an authorization code for the caller's profile.
func (g *Google) Exchange(ctx context.Context, code string) (*GoogleProfile, error) {
	if !g.Enabled {
		return nil, errors.New("auth: Google sign-in is not configured")
	}
	form := url.Values{}
	form.Set("code", code)
	form.Set("client_id", g.ClientID)
	form.Set("client_secret", g.ClientSecret)
	form.Set("redirect_uri", g.RedirectURL)
	form.Set("grant_type", "authorization_code")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, googleTokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := g.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth: google token request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		g.Logger.Warn("google token exchange failed",
			slog.Int("status", resp.StatusCode),
			slog.String("detail", truncate(string(body), 300)))
		return nil, ErrGoogleRejected
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("auth: decode google token response: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return nil, ErrGoogleRejected
	}

	profileReq, err := http.NewRequestWithContext(ctx, http.MethodGet, googleUserinfoEndpoint, nil)
	if err != nil {
		return nil, err
	}
	profileReq.Header.Set("Authorization", "Bearer "+tokenResp.AccessToken)

	profileResp, err := g.HTTP.Do(profileReq)
	if err != nil {
		return nil, fmt.Errorf("auth: google userinfo request: %w", err)
	}
	defer profileResp.Body.Close()

	profileBody, err := io.ReadAll(io.LimitReader(profileResp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if profileResp.StatusCode != http.StatusOK {
		return nil, ErrGoogleRejected
	}

	var profile GoogleProfile
	if err := json.Unmarshal(profileBody, &profile); err != nil {
		return nil, fmt.Errorf("auth: decode google profile: %w", err)
	}
	// sub is the only stable identifier. Without it the account cannot be matched on a
	// later login, so the sign-in is refused rather than creating an unlinked user.
	if profile.Subject == "" {
		return nil, ErrGoogleRejected
	}
	if profile.Email != "" {
		profile.Email = NormalizeEmail(profile.Email)
	}
	return &profile, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

package auth

import (
	"context"
	"log/slog"
	"time"

	"animind-backend-go/internal/cache"
	"animind-backend-go/internal/config"
)

// credentialCache resolves a token from shared state instead of the database: every
// authenticated request used to cost two indexed SELECTs, session row then user row.
//
// Entries are keyed by the SHA-256 hash the database stores, so a key reveals nothing
// about its token, and sit in the owner's invalidation group so revocation does not
// need to know the key it is revoking.
type credentialCache struct {
	store  cache.Store
	ttl    time.Duration
	logger *slog.Logger
}

// cachedPrincipal is the stored half of Principal. Email and IsAdmin travel with it because
// authorization decisions read them; anything else would go stale in a way no caller checks.
type cachedPrincipal struct {
	UserID           string `json:"userId"`
	Email            string `json:"email"`
	Username         string `json:"username"`
	IsAdmin          bool   `json:"isAdmin"`
	Kind             string `json:"kind"`
	WebSessionRowID  string `json:"webSessionRowId,omitempty"`
	DesktopSessionID string `json:"desktopSessionId,omitempty"`
}

func (c cachedPrincipal) principal() *Principal {
	return &Principal{
		UserID: c.UserID, Email: c.Email, Username: c.Username, IsAdmin: c.IsAdmin,
		Kind: c.Kind, WebSessionRowID: c.WebSessionRowID, DesktopSessionID: c.DesktopSessionID,
	}
}

func newCredentialCache(store cache.Store, cfg *config.Config, logger *slog.Logger) *credentialCache {
	if store == nil {
		return nil
	}
	// The entry must not outlive the interval the database uses to stamp a session as used.
	// Otherwise a long-lived cache would stop sliding expiry from happening at all.
	ttl := cfg.Cache.SessionTTL
	if ttl > touchInterval {
		ttl = touchInterval
	}
	return &credentialCache{store: store, ttl: ttl, logger: logger}
}

// lookup returns a cached resolution. A store that cannot be reached is a miss: losing the
// cache must never turn into a 401.
func (c *credentialCache) lookup(ctx context.Context, key string) (*Principal, bool) {
	cached, found, err := cache.GetJSON[cachedPrincipal](ctx, c.store, key)
	if err != nil {
		c.logger.Warn("auth: credential cache lookup failed", slog.String("error", err.Error()))
		return nil, false
	}
	if !found {
		return nil, false
	}
	return cached.principal(), true
}

// remember stores a resolution for at most the shorter of the configured TTL and the time the
// underlying row itself stops being valid. Caching a credential past its own expiry would be
// a way to extend a session by not looking at the database.
func (c *credentialCache) remember(ctx context.Context, key string, p *Principal, expiresAt time.Time) {
	ttl := c.ttl
	if until := time.Until(expiresAt); until < ttl {
		ttl = until
	}
	if ttl <= 0 {
		return
	}
	value := cachedPrincipal{
		UserID: p.UserID, Email: p.Email, Username: p.Username, IsAdmin: p.IsAdmin,
		Kind: p.Kind, WebSessionRowID: p.WebSessionRowID, DesktopSessionID: p.DesktopSessionID,
	}
	if err := cache.SetJSON(ctx, c.store, key, value, ttl, cache.UserGroup(p.UserID)); err != nil {
		c.logger.Warn("auth: credential cache write failed", slog.String("error", err.Error()))
	}
}

// invalidateUser drops every credential resolution for one account. Revocations,
// password changes and privilege writes all go through here because the group is the only
// handle those paths have: a session revoked by row id says nothing about the token hash
// its cache entry was keyed by.
//
// It cannot erase an entry written by a resolution already in flight when the revocation
// landed. That race costs at most the cache TTL, which is why the TTL is short.
func (c *credentialCache) invalidateUser(ctx context.Context, userID string) {
	if userID == "" {
		return
	}
	if err := c.store.DeleteGroup(ctx, cache.UserGroup(userID)); err != nil {
		c.logger.Warn("auth: credential cache eviction failed", slog.String("error", err.Error()))
	}
}

// credentialKey names one token's cache entry. kind separates the two token families so a
// desktop access token can never satisfy a web session lookup by key collision.
func credentialKey(kind, token string) string {
	return cache.CredentialKey(kind, HashToken(token))
}

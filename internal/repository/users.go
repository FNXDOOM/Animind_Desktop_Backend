// Package repository contains every SQL statement the backend runs. Scoping a method by the
// authenticated user id is the authorization mechanism: there is no client-side key and no
// row-level-security policy to rely on.
//
// IDs are read with an explicit ::text cast and written with ::uuid, keeping Go strings on
// both ends and avoiding the driver's UUID codec.
package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"animind-backend-go/internal/database"
)

// Store is the single entry point to SQL. It is safe for concurrent use.
type Store struct {
	pool *pgxpool.Pool
}

// New builds a Store over an open database.
func New(db *database.DB) *Store { return &Store{pool: db.Pool} }

// Ping reports whether the database is answering, which is what /health checks.
func (s *Store) Ping(ctx context.Context) error {
	var one int
	return s.pool.QueryRow(ctx, "select 1").Scan(&one)
}

// PoolStats is a snapshot of the connection pool. The metrics endpoint exports it because a
// saturated pool looks identical to a slow query from the outside.
type PoolStats struct {
	Total  int32
	Active int32
	Idle   int32
	Max    int32
}

func (s *Store) PoolStats() PoolStats {
	stat := s.pool.Stat()
	if stat == nil {
		return PoolStats{}
	}
	return PoolStats{
		Total:  stat.TotalConns(),
		Active: stat.AcquiredConns(),
		Idle:   stat.IdleConns(),
		Max:    stat.MaxConns(),
	}
}

const userColumns = `
	id::text, email, email_normalized, username, avatar_url, password_hash,
	is_admin, failed_login_count, locked_until, created_at, updated_at`

func scanUser(row pgx.Row) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.EmailNormalized, &u.Username, &u.AvatarURL, &u.PasswordHash,
		&u.IsAdmin, &u.FailedLogins, &u.LockedUntil, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("repository: scan user: %w", err)
	}
	return &u, nil
}

// ── Users ────────────────────────────────────────────────────────────────────

// CreateUser inserts an account. avatarURL may be nil; plaintextPassword must already
// be hashed by the caller.
func (s *Store) CreateUser(ctx context.Context, email, emailNormalized, username string, avatarURL, passwordHash *string) (*User, error) {
	return scanUser(s.pool.QueryRow(ctx, `
		insert into animind_users (email, email_normalized, username, avatar_url, password_hash)
		values ($1, $2, $3, $4, $5)
		returning `+userColumns,
		email, emailNormalized, username, avatarURL, passwordHash))
}

// UserByEmailNormalized looks up an account by its canonical email.
func (s *Store) UserByEmailNormalized(ctx context.Context, emailNormalized string) (*User, error) {
	return scanUser(s.pool.QueryRow(ctx, `select `+userColumns+` from animind_users where email_normalized = $1`, emailNormalized))
}

// UserByID loads an account by primary key.
func (s *Store) UserByID(ctx context.Context, id string) (*User, error) {
	return scanUser(s.pool.QueryRow(ctx, `select `+userColumns+` from animind_users where id = $1::uuid`, id))
}

// EmailTaken reports whether a normalized email already belongs to an account.
// It lets signup answer 409 without leaking the row.
func (s *Store) EmailTaken(ctx context.Context, emailNormalized string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `select exists(select 1 from animind_users where email_normalized = $1)`, emailNormalized).Scan(&exists)
	return exists, err
}

// EmailTakenExcept ignores the caller's own row, so re-submitting the same address is not a
// conflict for an account changing its email.
func (s *Store) EmailTakenExcept(ctx context.Context, emailNormalized, exceptUserID string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `
		select exists(select 1 from animind_users where email_normalized = $1 and id <> $2::uuid)`,
		emailNormalized, exceptUserID).Scan(&exists)
	return exists, err
}

// UpdateEmail writes both the display address and its normalized form so lookups stay exact.
func (s *Store) UpdateEmail(ctx context.Context, userID, email, emailNormalized string) (*User, error) {
	return scanUser(s.pool.QueryRow(ctx, `
		update animind_users
		set email = $2, email_normalized = $3, updated_at = now()
		where id = $1::uuid
		returning `+userColumns,
		userID, email, emailNormalized))
}

// UpdatePassword stores a new argon2id hash and clears any login lockout.
func (s *Store) UpdatePassword(ctx context.Context, userID string, passwordHash string) error {
	tag, err := s.pool.Exec(ctx, `
		update animind_users
		set password_hash = $2, failed_login_count = 0, locked_until = null, updated_at = now()
		where id = $1::uuid`, userID, passwordHash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateProfile changes the display name and avatar. Both are optional: a nil field
// keeps its current value.
func (s *Store) UpdateProfile(ctx context.Context, userID string, username, avatarURL *string) (*User, error) {
	return scanUser(s.pool.QueryRow(ctx, `
		update animind_users
		set username = coalesce($2, username),
		    avatar_url = coalesce($3, avatar_url),
		    updated_at = now()
		where id = $1::uuid
		returning `+userColumns,
		userID, username, avatarURL))
}

// SetAdmin grants or revokes administrator rights.
func (s *Store) SetAdmin(ctx context.Context, userID string, isAdmin bool) error {
	tag, err := s.pool.Exec(ctx, `
		update animind_users set is_admin = $2, updated_at = now() where id = $1::uuid`, userID, isAdmin)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetCanDownload grants or revokes the torrent-download entitlement.
func (s *Store) SetCanDownload(ctx context.Context, userID string, allowed bool) error {
	tag, err := s.pool.Exec(ctx, `
		update animind_users set can_download = $2, updated_at = now() where id = $1::uuid`, userID, allowed)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RegisterFailedLogin increments the failure counter and returns whether the account
// is now locked, so the caller can distinguish "wrong password" from "try later"
// without a second query.
func (s *Store) RegisterFailedLogin(ctx context.Context, userID string, maxFailures int, lockout time.Duration) (locked bool, err error) {
	var count int
	err = s.pool.QueryRow(ctx, `
		update animind_users
		set failed_login_count = failed_login_count + 1,
		    locked_until = case when failed_login_count + 1 >= $2
		                        then now() + make_interval(secs => $3)
		                        else locked_until end
		where id = $1::uuid
		returning failed_login_count`,
		userID, maxFailures, lockout.Seconds()).Scan(&count)
	if err != nil {
		return false, err
	}
	return count >= maxFailures, nil
}

// ClearFailedLogins resets the lockout counters after a successful password check.
func (s *Store) ClearFailedLogins(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `
		update animind_users set failed_login_count = 0, locked_until = null where id = $1::uuid`, userID)
	return err
}

// DeleteUser removes the account. Watchlist, progress, history, sessions and party
// rows all cascade.
func (s *Store) DeleteUser(ctx context.Context, userID string) error {
	tag, err := s.pool.Exec(ctx, `delete from animind_users where id = $1::uuid`, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListUsers returns accounts for the admin panel, newest first.
func (s *Store) ListUsers(ctx context.Context, limit, offset int) ([]PublicUser, error) {
	rows, err := s.pool.Query(ctx, `
		select id::text, email, username, avatar_url, is_admin, can_download, created_at, updated_at
		from animind_users
		order by created_at desc
		limit $1 offset $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]PublicUser, 0, 32)
	for rows.Next() {
		var u PublicUser
		if err := rows.Scan(&u.ID, &u.Email, &u.Username, &u.AvatarURL, &u.IsAdmin, &u.CanDownload, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// CountUsers returns the total account count for admin metrics.
func (s *Store) CountUsers(ctx context.Context) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `select count(*) from animind_users`).Scan(&n)
	return n, err
}

// ── Linked provider identities ───────────────────────────────────────────────

const identityColumns = `id::text, user_id::text, provider, provider_user_id, coalesce(email,''), coalesce(display_name,''), coalesce(avatar_url,'')`

// LinkIdentity attaches an external account to a user, refreshing its profile fields.
func (s *Store) LinkIdentity(ctx context.Context, userID, provider, providerUserID, email, displayName, avatarURL string) (*LinkedIdentity, error) {
	var i LinkedIdentity
	err := s.pool.QueryRow(ctx, `
		insert into animind_user_identities (user_id, provider, provider_user_id, email, display_name, avatar_url)
		values ($1::uuid, $2, $3, $4, $5, $6)
		on conflict (provider, provider_user_id) do update
		set email = excluded.email, display_name = excluded.display_name, avatar_url = excluded.avatar_url
		returning `+identityColumns,
		userID, provider, providerUserID, email, displayName, avatarURL).
		Scan(&i.ID, &i.UserID, &i.Provider, &i.ProviderUserID, &i.Email, &i.DisplayName, &i.AvatarURL)
	return &i, err
}

// UserByProviderUserID finds the account bound to an external identity.
func (s *Store) UserByProviderUserID(ctx context.Context, provider, providerUserID string) (*User, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		select user_id::text from animind_user_identities where provider = $1 and provider_user_id = $2`,
		provider, providerUserID).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return s.UserByID(ctx, id)
}

// ── One-time auth tokens (password reset) ────────────────────────────────────

// CreateAuthToken pins a hashed, single-use token such as a reset link.
func (s *Store) CreateAuthToken(ctx context.Context, userID, kind, tokenHash string, expiresAt time.Time) error {
	_, err := s.pool.Exec(ctx, `
		insert into animind_auth_tokens (user_id, kind, token_hash, expires_at)
		values ($1::uuid, $2, $3, $4)`, userID, kind, tokenHash, expiresAt)
	return err
}

// ConsumeAuthToken validates and invalidates a single-use token atomically.
// It returns the owning user id, or ErrNotFound for unknown, used or expired tokens.
func (s *Store) ConsumeAuthToken(ctx context.Context, kind, tokenHash string) (string, error) {
	var userID string
	err := s.pool.QueryRow(ctx, `
		delete from animind_auth_tokens
		where kind = $1 and token_hash = $2 and used_at is null and expires_at > now()
		returning user_id::text`, kind, tokenHash).Scan(&userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	return userID, nil
}

// ── Web sessions ─────────────────────────────────────────────────────────────

const webSessionColumns = `
	id::text, user_id::text, session_hash, device_name, user_agent, host(ip_address),
	remember_me, expires_at, last_used_at, revoked_at, created_at`

func scanWebSession(row pgx.Row) (*WebSession, error) {
	var s WebSession
	err := row.Scan(&s.ID, &s.UserID, &s.TokenHash, &s.DeviceName, &s.UserAgent, &s.IPAddress,
		&s.RememberMe, &s.ExpiresAt, &s.LastUsedAt, &s.RevokedAt, &s.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("repository: scan web session: %w", err)
	}
	return &s, nil
}

// CreateWebSession stores a new session row. The caller keeps the plaintext token.
func (s *Store) CreateWebSession(ctx context.Context, userID, tokenHash, deviceName, userAgent, ip string, rememberMe bool, expiresAt time.Time) (*WebSession, error) {
	return scanWebSession(s.pool.QueryRow(ctx, `
		insert into animind_web_sessions (user_id, session_hash, device_name, user_agent, ip_address, remember_me, expires_at)
		values ($1::uuid, $2, $3, $4, nullif($5,'')::inet, $6, $7)
		returning `+webSessionColumns,
		userID, tokenHash, deviceName, userAgent, ip, rememberMe, expiresAt))
}

// ActiveWebSession loads a live session by token hash. The WHERE clause is the whole
// authorization decision for a session token: revoked or expired rows are not found.
func (s *Store) ActiveWebSession(ctx context.Context, tokenHash string) (*WebSession, error) {
	return scanWebSession(s.pool.QueryRow(ctx, `
		select `+webSessionColumns+` from animind_web_sessions
		where session_hash = $1 and revoked_at is null and expires_at > now()`, tokenHash))
}

// TouchWebSession advances sliding expiry and the last-used stamp.
func (s *Store) TouchWebSession(ctx context.Context, id string, expiresAt time.Time) error {
	_, err := s.pool.Exec(ctx, `
		update animind_web_sessions set last_used_at = now(), expires_at = $2 where id = $1::uuid`, id, expiresAt)
	return err
}

// RevokeWebSession signs one session out. The user id is part of the predicate: without it,
// presenting another account's session id would be enough to revoke it.
func (s *Store) RevokeWebSession(ctx context.Context, userID, id string) error {
	if !isUUID(id) {
		return ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `
		update animind_web_sessions set revoked_at = now()
		where id = $1::uuid and user_id = $2::uuid and revoked_at is null`, id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeAllWebSessions signs every other session out, leaving the current one usable.
func (s *Store) RevokeAllWebSessions(ctx context.Context, userID, exceptID string) error {
	_, err := s.pool.Exec(ctx, `
		update animind_web_sessions set revoked_at = now()
		where user_id = $1::uuid and revoked_at is null and ($2 = '' or id::text <> $2)`, userID, exceptID)
	return err
}

// ListWebSessions returns a user's live sessions for the security screen.
func (s *Store) ListWebSessions(ctx context.Context, userID string) ([]WebSession, error) {
	rows, err := s.pool.Query(ctx, `
		select id::text, user_id::text, device_name, user_agent, host(ip_address),
		       remember_me, expires_at, last_used_at, created_at
		from animind_web_sessions
		where user_id = $1::uuid and revoked_at is null and expires_at > now()
		order by created_at desc
		limit 50`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]WebSession, 0, 8)
	for rows.Next() {
		var ws WebSession
		if err := rows.Scan(&ws.ID, &ws.UserID, &ws.DeviceName, &ws.UserAgent, &ws.IPAddress,
			&ws.RememberMe, &ws.ExpiresAt, &ws.LastUsedAt, &ws.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, ws)
	}
	return out, rows.Err()
}

// DeleteExpiredWebSessions prunes rows that can no longer be used.
func (s *Store) DeleteExpiredWebSessions(ctx context.Context, olderThan time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		delete from animind_web_sessions where expires_at < $1 or revoked_at < $1`, olderThan)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

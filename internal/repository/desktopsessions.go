package repository

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Desktop session storage. One row pins the whole credential pair so an access token can be
// checked against revocation without decoding it twice, a refresh token rotates in place and
// invalidates its predecessor, and the device labels the desktop app reports travel with it.

const desktopSessionColumns = `
	id::text, user_id::text, session_id, device_id, device_name,
	refresh_token_hash, refresh_token_expires_at, access_token_hash,
	access_token_expires_at, revoked_at, created_at, updated_at`

func scanDesktopSession(row scanner) (*DesktopSession, error) {
	var d DesktopSession
	err := row.Scan(&d.ID, &d.UserID, &d.SessionID, &d.DeviceID, &d.DeviceName,
		&d.RefreshTokenHash, &d.RefreshTokenExpires, &d.AccessTokenHash,
		&d.AccessTokenExpires, &d.RevokedAt, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		if isNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("repository: scan desktop session: %w", err)
	}
	return &d, nil
}

func isNoRows(err error) bool { return errors.Is(err, pgxErrNoRows) }

// CreateDesktopSession pins a freshly issued token pair.
func (s *Store) CreateDesktopSession(ctx context.Context, userID, sessionID, deviceID, deviceName, refreshTokenHash, accessTokenHash string, accessExpiresAt, refreshExpiresAt time.Time) (*DesktopSession, error) {
	return scanDesktopSession(s.pool.QueryRow(ctx, `
		insert into desktop_auth_sessions
			(user_id, session_id, device_id, device_name,
			 refresh_token_hash, refresh_token_expires_at,
			 access_token_hash, access_token_expires_at)
		values ($1::uuid, $2, nullif($3,''), nullif($4,''), $5, $6, $7, $8)
		returning `+desktopSessionColumns,
		userID, sessionID, deviceID, deviceName,
		refreshTokenHash, refreshExpiresAt, accessTokenHash, accessExpiresAt))
}

// DesktopSessionByAccessHash returns a live session matching an access token hash.
// Expiry and revocation are part of the predicate, so a stale row is not returned.
func (s *Store) DesktopSessionByAccessHash(ctx context.Context, hash string) (*DesktopSession, error) {
	return scanDesktopSession(s.pool.QueryRow(ctx, `
		select `+desktopSessionColumns+` from desktop_auth_sessions
		where access_token_hash = $1 and revoked_at is null and access_token_expires_at > now()`, hash))
}

// DesktopSessionByRefreshHash returns the session owning an unexpired refresh token.
func (s *Store) DesktopSessionByRefreshHash(ctx context.Context, hash string) (*DesktopSession, error) {
	return scanDesktopSession(s.pool.QueryRow(ctx, `
		select `+desktopSessionColumns+` from desktop_auth_sessions
		where refresh_token_hash = $1 and revoked_at is null
		  and refresh_token_expires_at > now() and absolute_expires_at > now()`, hash))
}

// RevokeDesktopFamilyByReusedHash revokes every session in the family that once owned the
// presented refresh hash. Called when a rotated-out token is replayed: the caller cannot
// tell which device is legitimate, so the whole family must re-authenticate.
func (s *Store) RevokeDesktopFamilyByReusedHash(ctx context.Context, hash string) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		with hit as (
			select family_id from desktop_auth_sessions
			where rotated_from_hash = $1 limit 1
		)
		update desktop_auth_sessions s
		set revoked_at = now(), updated_at = now()
		from hit
		where s.family_id = hit.family_id and s.revoked_at is null`, hash)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// RotateDesktopSession replaces both token hashes, invalidating the presented refresh
// token. If this write fails the caller must not return the new tokens, or the device
// would hold a credential the server cannot verify.
func (s *Store) RotateDesktopSession(ctx context.Context, id, refreshTokenHash, accessTokenHash string, accessExpiresAt, refreshExpiresAt time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		update desktop_auth_sessions
		set rotated_from_hash = refresh_token_hash,
		    refresh_token_hash = $2, refresh_token_expires_at = $3,
		    access_token_hash = $4, access_token_expires_at = $5,
		    updated_at = now()
		where id = $1::uuid and revoked_at is null and absolute_expires_at > now()`,
		id, refreshTokenHash, refreshExpiresAt, accessTokenHash, accessExpiresAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeDesktopSessionByRefreshHash marks a session revoked; reports whether a row matched.
func (s *Store) RevokeDesktopSessionByRefreshHash(ctx context.Context, hash string) (bool, error) {
	return s.revokeDesktop(ctx, `refresh_token_hash = $1`, hash)
}

// RevokeDesktopSessionByAccessToken marks a session revoked from its access token.
func (s *Store) RevokeDesktopSessionByAccessToken(ctx context.Context, hash string) (bool, error) {
	return s.revokeDesktop(ctx, `access_token_hash = $1`, hash)
}

// RevokeDesktopSessionByID revokes a session from its primary key (security screen).
// The user filter is part of the predicate, so another account's id revokes nothing.
func (s *Store) RevokeDesktopSessionByID(ctx context.Context, id, userID string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		update desktop_auth_sessions set revoked_at = now(), updated_at = now()
		where id = $1::uuid and user_id = $2::uuid and revoked_at is null`, id, userID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (s *Store) revokeDesktop(ctx context.Context, predicate, hash string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		update desktop_auth_sessions set revoked_at = now(), updated_at = now()
		where `+predicate+` and revoked_at is null`, hash)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// DesktopSessionDevice describes one signed-in device for the security screen.
type DesktopSessionDevice struct {
	ID         string    `json:"id"`
	SessionID  string    `json:"session_id"`
	DeviceID   *string   `json:"device_id"`
	DeviceName *string   `json:"device_name"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// ListDesktopSessions returns a user's live desktop devices. A session past its absolute
// expiry is not live even when its last rotated refresh token has not expired yet.
func (s *Store) ListDesktopSessions(ctx context.Context, userID string) ([]DesktopSessionDevice, error) {
	rows, err := s.pool.Query(ctx, `
		select id::text, session_id, device_id, device_name, created_at, updated_at, refresh_token_expires_at
		from desktop_auth_sessions
		where user_id = $1::uuid and revoked_at is null
		  and refresh_token_expires_at > now() and absolute_expires_at > now()
		order by created_at desc
		limit 50`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]DesktopSessionDevice, 0, 8)
	for rows.Next() {
		var d DesktopSessionDevice
		if err := rows.Scan(&d.ID, &d.SessionID, &d.DeviceID, &d.DeviceName, &d.CreatedAt, &d.LastSeenAt, &d.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RevokeAllDesktopSessions revokes every live desktop session for an account. It is
// called when the account's credential changes (password reset, password change, email
// change), because a desktop refresh token otherwise outlives the remediation and keeps
// an attacker signed in for up to the refresh lifetime.
func (s *Store) RevokeAllDesktopSessions(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `
		update desktop_auth_sessions
		set revoked_at = now(), updated_at = now()
		where user_id = $1::uuid and revoked_at is null`, userID)
	return err
}

// CleanupDesktopSessions deletes every revoked row and every refresh token expired before
// the cutoff. Revoked rows carry no future value, so age does not gate them; the retention
// window only protects a recently expired session from an in-flight refresh.
func (s *Store) CleanupDesktopSessions(ctx context.Context, expiredBefore time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		delete from desktop_auth_sessions
		where revoked_at is not null or refresh_token_expires_at < $1`, expiredBefore)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

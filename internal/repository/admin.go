package repository

import (
	"context"
	"strings"
)

// AdminUserRow is one account on the admin users page. The session counts are live rows, so
// the operator can see at a glance which accounts still have a phone, browser or desktop
// player signed in.
type AdminUserRow struct {
	PublicUser
	WebSessions    int64 `json:"web_sessions"`
	DesktopDevices int64 `json:"desktop_devices"`
}

// ListUsersWithSessions returns accounts for the admin console, newest first, with the number
// of live sessions of each kind. A search matches email or username. The public API keeps its
// own ListUsers shape; this projection is only for the server-rendered panel.
func (s *Store) ListUsersWithSessions(ctx context.Context, search string, limit, offset int) ([]AdminUserRow, int64, error) {
	search = strings.TrimSpace(search)
	pattern := "%"
	if search != "" {
		pattern = "%" + escapeLike(search) + "%"
	}
	where := `where ($1 = '' or u.email ilike $2 or u.username ilike $2)`

	var total int64
	if err := s.pool.QueryRow(ctx, `select count(*) from animind_users u `+where, search, pattern).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := s.pool.Query(ctx, `
		select u.id::text, u.email, u.username, u.avatar_url, u.is_admin, u.can_download,
		       u.created_at, u.updated_at,
		       (select count(*) from animind_web_sessions w
		         where w.user_id = u.id and w.revoked_at is null and w.expires_at > now()),
		       (select count(*) from desktop_auth_sessions d
		         where d.user_id = u.id and d.revoked_at is null
		           and d.refresh_token_expires_at > now() and d.absolute_expires_at > now())
		from animind_users u `+where+`
		order by u.created_at desc
		limit $3 offset $4`, search, pattern, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := make([]AdminUserRow, 0, limit)
	for rows.Next() {
		var u AdminUserRow
		if err := rows.Scan(&u.ID, &u.Email, &u.Username, &u.AvatarURL, &u.IsAdmin, &u.CanDownload,
			&u.CreatedAt, &u.UpdatedAt, &u.WebSessions, &u.DesktopDevices); err != nil {
			return nil, 0, err
		}
		out = append(out, u)
	}
	return out, total, rows.Err()
}

// UserStats is the account summary on the admin overview.
type UserStats struct {
	Total       int64
	Admins      int64
	Downloaders int64
}

// UserStats counts accounts and the two entitlements in one round trip.
func (s *Store) UserStats(ctx context.Context) (UserStats, error) {
	var stats UserStats
	err := s.pool.QueryRow(ctx, `
		select count(*),
		       count(*) filter (where is_admin),
		       count(*) filter (where can_download)
		from animind_users`).Scan(&stats.Total, &stats.Admins, &stats.Downloaders)
	return stats, err
}

// SessionCounts counts live browser/app sessions and live desktop devices across every
// account, for the overview cards.
func (s *Store) SessionCounts(ctx context.Context) (web, desktop int64, err error) {
	err = s.pool.QueryRow(ctx, `
		select
		  (select count(*) from animind_web_sessions
		    where revoked_at is null and expires_at > now()),
		  (select count(*) from desktop_auth_sessions
		    where revoked_at is null and refresh_token_expires_at > now()
		      and absolute_expires_at > now())`).Scan(&web, &desktop)
	return web, desktop, err
}

package repository

import (
	"context"
	"time"
)

// Watch party persistence.
//
// The socket layer holds the authoritative live state; these rows exist so a room can
// be looked up by code over HTTP, so a restart does not leave "active" rooms forever,
// and so the cleanup job has something to delete. A write failure here must never fail
// a join, which is why the syncplay handlers treat these errors as warnings.

const partyColumns = `
	id, host_user_id::text, host_name, episode_id::text, status, created_at, updated_at, ended_at`

func scanParty(row scanner) (*PartyRow, error) {
	var p PartyRow
	err := row.Scan(&p.ID, &p.HostUserID, &p.HostName, &p.EpisodeID, &p.Status, &p.CreatedAt, &p.UpdatedAt, &p.EndedAt)
	if err != nil {
		if isNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &p, nil
}

// CreateParty registers a room. Re-using a code that is still active is rejected by the
// primary key, which is the desired behaviour: codes are minted in memory and a collision
// must surface rather than silently merge two rooms.
func (s *Store) CreateParty(ctx context.Context, code, hostUserID, hostName string, episodeID *string) error {
	_, err := s.pool.Exec(ctx, `
		insert into watch_parties (id, host_user_id, host_name, episode_id, status)
		values ($1, $2::uuid, $3, $4::uuid, 'active')`, code, hostUserID, hostName, episodeID)
	return err
}

// PartyByCode loads a room for the HTTP room-lookup endpoint.
func (s *Store) PartyByCode(ctx context.Context, code string) (*PartyRow, error) {
	return scanParty(s.pool.QueryRow(ctx, `select `+partyColumns+` from watch_parties where id = $1`, code))
}

// PartyParticipant is one join record for a room.
type PartyParticipant struct {
	PartyID  string    `json:"party_id"`
	UserID   string    `json:"user_id"`
	JoinedAt time.Time `json:"joined_at"`
}

// RecordPartyParticipant notes that a user joined a room.
func (s *Store) RecordPartyParticipant(ctx context.Context, partyID, userID string) error {
	_, err := s.pool.Exec(ctx, `
		insert into watch_party_participants (party_id, user_id, joined_at)
		values ($1, $2::uuid, now())`, partyID, userID)
	return err
}

// EndParty marks a room ended. An empty code means the room never persisted, so the
// call is a no-op rather than an error.
func (s *Store) EndParty(ctx context.Context, code string) error {
	if code == "" {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		update watch_parties set status = 'ended', ended_at = now(), updated_at = now()
		where id = $1 and status <> 'ended'`, code)
	return err
}

// EndAllActiveParties closes rooms left active by a previous process. Live socket state
// does not survive a restart, so any row still marked active at boot is a stale record.
func (s *Store) EndAllActiveParties(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		update watch_parties set status = 'ended', ended_at = now(), updated_at = now()
		where status = 'active'`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DeleteEndedParties removes rooms that ended before the cutoff along with their
// participant rows, in one transaction so participants never outlive their room.
func (s *Store) DeleteEndedParties(ctx context.Context, olderThan time.Time) (rooms, participants int64, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var codes []string
	rows, err := tx.Query(ctx, `
		select id from watch_parties
		where status = 'ended' and ended_at is not null and ended_at <= $1`, olderThan)
	if err != nil {
		return 0, 0, err
	}
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			rows.Close()
			return 0, 0, err
		}
		codes = append(codes, code)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if len(codes) == 0 {
		if err := tx.Commit(ctx); err != nil {
			return 0, 0, err
		}
		return 0, 0, nil
	}

	tag, err := tx.Exec(ctx, `delete from watch_party_participants where party_id = any($1)`, codes)
	if err != nil {
		return 0, 0, err
	}
	participants = tag.RowsAffected()

	tag, err = tx.Exec(ctx, `delete from watch_parties where id = any($1)`, codes)
	if err != nil {
		return 0, 0, err
	}
	rooms = tag.RowsAffected()

	if err := tx.Commit(ctx); err != nil {
		return 0, 0, err
	}
	return rooms, participants, nil
}

// CountActiveParties reports room load for the admin metrics screen.
func (s *Store) CountActiveParties(ctx context.Context) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `select count(*) from watch_parties where status = 'active'`).Scan(&n)
	return n, err
}

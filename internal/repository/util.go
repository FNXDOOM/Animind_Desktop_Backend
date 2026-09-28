package repository

import (
	"errors"
	"regexp"

	"github.com/jackc/pgx/v5"
)

// pgxErrNoRows is aliased so domain files can translate a missing row without
// importing the driver.
var pgxErrNoRows = pgx.ErrNoRows

// scanner is the minimal interface satisfied by pgx.Row and by a row fetched from a
// transaction, letting one scan helper serve both.
type scanner interface {
	Scan(dest ...any) error
}

// uuidPattern matches the canonical 8-4-4-4-12 hex form.
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// isUUID guards queries that cast a path parameter with ::uuid. Postgres raises a
// syntax error for a malformed literal, which would surface as a 500 for what is
// really a bad identifier; checking first lets the caller answer 404.
func isUUID(value string) bool { return uuidPattern.MatchString(value) }

// IsUUID reports whether a client-supplied identifier can be used as a primary key.
func IsUUID(value string) bool { return isUUID(value) }

// notFound maps a driver "no rows" error onto ErrNotFound.
func notFound(err error) error {
	if errors.Is(err, pgxErrNoRows) {
		return ErrNotFound
	}
	return err
}

// uniqueViolation detects a Postgres unique constraint failure (SQLSTATE 23505). The
// scanner treats it as "another writer won the race" and re-reads the existing row.
func uniqueViolation(err error) bool {
	var e interface{ SQLState() string }
	if !errors.As(err, &e) {
		return false
	}
	return e.SQLState() == "23505"
}

// IsUniqueViolation exposes uniqueViolation for callers that branch on it.
func IsUniqueViolation(err error) bool { return uniqueViolation(err) }

package repository

import (
	"errors"
	"regexp"

	"github.com/jackc/pgx/v5"
)

// pgxErrNoRows lets domain files translate a missing row without importing the driver.
var pgxErrNoRows = pgx.ErrNoRows

// scanner is the minimal interface shared by pgx.Row and transaction rows.
type scanner interface {
	Scan(dest ...any) error
}

// uuidPattern matches the canonical 8-4-4-4-12 hex form.
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// isUUID guards ::uuid casts: a malformed identifier must answer 404, not raise a 500.
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

// uniqueViolation detects SQLSTATE 23505, which the scanner treats as a lost insert race.
func uniqueViolation(err error) bool {
	var e interface{ SQLState() string }
	if !errors.As(err, &e) {
		return false
	}
	return e.SQLState() == "23505"
}

// IsUniqueViolation exposes uniqueViolation for callers that branch on it.
func IsUniqueViolation(err error) bool { return uniqueViolation(err) }

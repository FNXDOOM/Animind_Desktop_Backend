// Package migrations exposes the SQL migration files embedded in the binary, so a single
// binary can migrate a fresh database on boot (DB_AUTO_MIGRATE=true).
//
// Files live at the repository root so operators can also apply them with psql directly.
package migrations

import "embed"

// FS holds every *.sql file in this directory.
//
//go:embed *.sql
var FS embed.FS

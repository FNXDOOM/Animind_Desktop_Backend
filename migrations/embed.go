// Package migrations exposes the SQL migration files embedded in the binary.
//
// Files live at the repository root so operators can also apply them with psql
// directly, and are embedded here so a single binary can migrate a fresh database
// on boot (DB_AUTO_MIGRATE=true).
package migrations

import "embed"

// FS holds every *.sql file in this directory.
//
//go:embed *.sql
var FS embed.FS

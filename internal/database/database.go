// Package database owns the Postgres connection pool and schema migrations. All data access
// goes through this pool, and every user-owned read and write is scoped to the authenticated
// user id in the query layer.
package database

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"animind-backend-go/internal/config"
	"animind-backend-go/migrations"
)

// DB wraps a pgx pool with the helpers the rest of the backend uses.
type DB struct {
	Pool   *pgxpool.Pool
	logger *slog.Logger
}

// Open builds the pool from cfg and verifies connectivity within DB_PING_TIMEOUT_SECONDS.
func Open(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*DB, error) {
	parsed, err := pgxpool.ParseConfig(cfg.DB.URL)
	if err != nil {
		return nil, fmt.Errorf("database: parse DATABASE_URL: %w", err)
	}
	if cfg.DB.MaxConns > 0 {
		parsed.MaxConns = cfg.DB.MaxConns
	}
	if cfg.DB.MinConns > 0 && cfg.DB.MinConns < parsed.MaxConns {
		parsed.MinConns = cfg.DB.MinConns
	}
	parsed.HealthCheckPeriod = 1 * time.Minute
	parsed.ConnConfig.RuntimeParams["application_name"] = "animind-backend-go"

	pool, err := pgxpool.NewWithConfig(ctx, parsed)
	if err != nil {
		return nil, fmt.Errorf("database: create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, cfg.DB.PingTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database: connect: %w", err)
	}

	db := &DB{Pool: pool, logger: logger}
	if cfg.DB.AutoMigrate {
		if err := db.Migrate(ctx); err != nil {
			pool.Close()
			return nil, err
		}
	}
	return db, nil
}

// Close releases every connection in the pool.
func (db *DB) Close() { db.Pool.Close() }

// Migrate applies embedded SQL files that have not run yet, in filename order. Each file runs
// in its own transaction with its schema_migrations row, so a partial apply rolls back and is
// retried on the next boot instead of leaving the schema unknown.
func (db *DB) Migrate(ctx context.Context) error {
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		return fmt.Errorf("database: read embedded migrations: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	if _, err := db.Pool.Exec(ctx, `
		create table if not exists schema_migrations (
			name text primary key,
			applied_at timestamptz not null default now()
		)`); err != nil {
		return fmt.Errorf("database: create schema_migrations: %w", err)
	}

	for _, name := range names {
		var applied bool
		err := db.Pool.QueryRow(ctx, `select exists(select 1 from schema_migrations where name = $1)`, name).Scan(&applied)
		if err != nil {
			return fmt.Errorf("database: check migration %s: %w", name, err)
		}
		if applied {
			continue
		}

		sql, err := migrations.FS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("database: read migration %s: %w", name, err)
		}

		err = db.withTx(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return fmt.Errorf("exec: %w", err)
			}
			_, err := tx.Exec(ctx, `insert into schema_migrations(name) values ($1)`, name)
			return err
		})
		if err != nil {
			return fmt.Errorf("database: apply %s: %w", name, err)
		}
		db.logger.Info("migration applied", slog.String("migration", name))
	}
	return nil
}

// withTx runs fn inside a transaction, committing on success and rolling back on error.
func (db *DB) withTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		// Rollback after commit is a no-op that only reports an already-closed tx.
		_ = tx.Rollback(ctx)
	}()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

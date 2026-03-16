package store

import (
	"context"
	"embed"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.up.sql
var migrationFS embed.FS

func Migrate(ctx context.Context, db *DB) error {
	// Create the schema_migrations tracking table.
	_, err := db.Pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`)
	if err != nil {
		return fmt.Errorf("creating schema_migrations table: %w", err)
	}

	// Acquire an advisory lock to prevent concurrent migration runs
	// (e.g. two instances starting simultaneously in Azure Container Apps).
	_, err = db.Pool.Exec(ctx, `SELECT pg_advisory_lock(1)`)
	if err != nil {
		return fmt.Errorf("acquiring migration lock: %w", err)
	}
	defer db.Pool.Exec(ctx, `SELECT pg_advisory_unlock(1)`) //nolint:errcheck // best-effort unlock

	// Find the current version.
	var current int
	err = db.Pool.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current)
	if err != nil {
		return fmt.Errorf("reading current migration version: %w", err)
	}

	// Read available migration files.
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("reading migration files: %w", err)
	}

	type migration struct {
		version int
		name    string
	}

	var pending []migration
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		v, err := parseVersion(e.Name())
		if err != nil {
			return fmt.Errorf("parsing migration filename %s: %w", e.Name(), err)
		}
		if v > current {
			pending = append(pending, migration{version: v, name: e.Name()})
		}
	}

	if len(pending) == 0 {
		slog.Info("schema is up to date", "version", current)
		return nil
	}

	sort.Slice(pending, func(i, j int) bool {
		return pending[i].version < pending[j].version
	})

	for _, m := range pending {
		sql, err := migrationFS.ReadFile("migrations/" + m.name)
		if err != nil {
			return fmt.Errorf("reading migration %s: %w", m.name, err)
		}

		tx, err := db.Pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("beginning transaction for migration %d: %w", m.version, err)
		}

		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("applying migration %s: %w", m.name, err)
		}

		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, m.version); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("recording migration %d: %w", m.version, err)
		}

		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("committing migration %d: %w", m.version, err)
		}

		slog.Info("applied migration", "version", m.version, "file", m.name)
	}

	return nil
}

func parseVersion(filename string) (int, error) {
	parts := strings.SplitN(filename, "_", 2)
	if len(parts) < 2 {
		return 0, fmt.Errorf("invalid migration filename: %s", filename)
	}
	return strconv.Atoi(parts[0])
}

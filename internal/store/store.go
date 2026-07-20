// Package store owns the SQLite database: schema, the ingest diff engine,
// and the read queries the web UI and runbook generator consume.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaV1 string

// Migrations apply in order; PRAGMA user_version tracks the last applied.
var migrations = []string{
	schemaV1,
	// v2: collector instance configuration. `config` is non-secret JSON
	// (URL, options); `secret` is the API token sealed by internal/secrets.
	`CREATE TABLE collector_configs (
		id      INTEGER PRIMARY KEY,
		type    TEXT NOT NULL,
		name    TEXT NOT NULL,
		config  TEXT NOT NULL DEFAULT '{}',
		secret  BLOB,
		enabled INTEGER NOT NULL DEFAULT 1,
		UNIQUE (type, name)
	);`,
	// v3: runbook generation. Hand-authored pages (START HERE, contacts),
	// export history, and key/value settings for the export pipeline.
	`CREATE TABLE pages (
		slug       TEXT PRIMARY KEY,
		body_md    TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);
	CREATE TABLE exports (
		id         INTEGER PRIMARY KEY,
		created_at TEXT NOT NULL,
		format     TEXT NOT NULL,
		path       TEXT NOT NULL,
		status     TEXT NOT NULL CHECK (status IN ('ok', 'error')),
		detail     TEXT
	);
	CREATE TABLE settings (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);`,
	// v4: backup restore-test tracking. One row per backup-job resource;
	// "last verified: never" in the runbook is the nag that matters most.
	`CREATE TABLE backup_checks (
		resource_id INTEGER PRIMARY KEY REFERENCES resources(id),
		verified_at TEXT NOT NULL,
		note        TEXT NOT NULL DEFAULT ''
	);`,
	// v5: household-facing annotation fields. These live alongside the
	// administrator fields in the same table so they inherit everything
	// annotations already get — identity keying that survives re-collection,
	// orphan reattach, and config backup/restore. SQLite can't alter a CHECK
	// constraint in place, so the table is rebuilt.
	`CREATE TABLE annotations_v5 (
		id          INTEGER PRIMARY KEY,
		resource_id INTEGER NOT NULL REFERENCES resources(id),
		field       TEXT NOT NULL CHECK (field IN (
		              'purpose', 'recovery', 'credential_pointer', 'note',
		              'plain_english', 'household_importance', 'safe_to_off', 'monthly_cost')),
		body_md     TEXT NOT NULL,
		updated_at  TEXT NOT NULL,
		UNIQUE (resource_id, field)
	);
	INSERT INTO annotations_v5 (id, resource_id, field, body_md, updated_at)
	  SELECT id, resource_id, field, body_md, updated_at FROM annotations;
	DROP TABLE annotations;
	ALTER TABLE annotations_v5 RENAME TO annotations;`,
}

// Store wraps the SQLite database. Safe for concurrent use; SQLite-level
// write serialization is handled by the driver plus WAL mode.
type Store struct {
	db *sql.DB
}

// Open opens (creating and migrating if needed) the database at path.
// Use ":memory:" for tests.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// modernc.org/sqlite serializes writes per connection; a single
	// connection avoids SQLITE_BUSY races and is ample for homelab scale.
	db.SetMaxOpenConns(1)

	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	for v := version; v < len(migrations); v++ {
		if _, err := s.db.Exec(migrations[v]); err != nil {
			return fmt.Errorf("apply schema v%d: %w", v+1, err)
		}
		if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", v+1)); err != nil {
			return fmt.Errorf("set schema version: %w", err)
		}
	}
	return nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// latestOKRun returns the id of the collector's most recent successful run,
// or 0 if it has never run. Must be called inside the given querier's scope.
func latestOKRun(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, collector string) (int64, error) {
	var id sql.NullInt64
	err := q.QueryRowContext(ctx,
		`SELECT MAX(id) FROM runs WHERE collector = ? AND status = 'ok'`, collector).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id.Int64, nil
}

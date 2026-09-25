package taskstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Schema migration.
//
// Steps are append-only and each runs in its own transaction, so a failure
// leaves the database at the last version that applied completely. The
// migration list is a parameter of open (via config), not a package global, so
// tests can exercise rollback with a deliberately broken step without
// mutating shared state.
type migration struct {
	version    int
	name       string
	statements []string
}

// schemaMigrations is the schema history of this package. Never edit or
// reorder a released step: databases in the field have already recorded its
// version.
var schemaMigrations = []migration{
	{
		version: 1,
		name:    "task state machine",
		statements: []string{
			// seq is the rowid and gives insertion order for "earliest queued
			// task". AUTOINCREMENT (rather than a plain INTEGER PRIMARY KEY)
			// guarantees the sequence never reuses a value, so ordering stays
			// meaningful even if rows are ever removed.
			`CREATE TABLE tasks (
				seq                INTEGER PRIMARY KEY AUTOINCREMENT,
				id                 TEXT NOT NULL UNIQUE,
				node_id            TEXT NOT NULL,
				capability         TEXT NOT NULL,
				capability_version INTEGER NOT NULL,
				input              TEXT NOT NULL,
				timeout_seconds    INTEGER NOT NULL,
				state              TEXT NOT NULL CHECK (state IN
					('queued','leased','running','cancel_requested','succeeded','failed','cancelled','unknown')),
				attempt_id         TEXT,
				lease_expires_at   TEXT,
				result_state       TEXT,
				result_text        TEXT,
				result_exit_code   INTEGER,
				result_error_code  TEXT,
				result_truncated   INTEGER,
				created_at         TEXT NOT NULL,
				updated_at         TEXT NOT NULL
			)`,
			// Serves the claim query: earliest queued task for one node.
			`CREATE INDEX tasks_claim ON tasks (node_id, state, seq)`,
			// One row per attempt. The token hash lives here and nowhere else,
			// so a task row can never leak a credential even by accident.
			`CREATE TABLE attempts (
				seq              INTEGER PRIMARY KEY AUTOINCREMENT,
				id               TEXT NOT NULL UNIQUE,
				task_id          TEXT NOT NULL REFERENCES tasks (id),
				node_id          TEXT NOT NULL,
				token_hash       TEXT NOT NULL,
				state            TEXT NOT NULL CHECK (state IN
					('leased','running','succeeded','failed','cancelled','expired')),
				lease_expires_at TEXT NOT NULL,
				created_at       TEXT NOT NULL,
				updated_at       TEXT NOT NULL
			)`,
			`CREATE INDEX attempts_task ON attempts (task_id)`,
			// Idempotent submission, scoped to a node. The primary key is the
			// dedupe index; payload_hash decides "same payload" versus
			// "conflict".
			`CREATE TABLE idempotency (
				node_id      TEXT NOT NULL,
				idem_key     TEXT NOT NULL,
				task_id      TEXT NOT NULL REFERENCES tasks (id),
				payload_hash TEXT NOT NULL,
				created_at   TEXT NOT NULL,
				PRIMARY KEY (node_id, idem_key)
			)`,
		},
	},
}

const createMigrationsTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
	version    INTEGER PRIMARY KEY,
	name       TEXT NOT NULL,
	applied_at TEXT NOT NULL
)`

// migrate applies every step newer than the recorded version, one transaction
// per step.
func migrate(ctx context.Context, db *sql.DB, steps []migration) error {
	if err := checkMigrations(steps); err != nil {
		return err
	}
	if err := execLocked(ctx, db, "create migration table", createMigrationsTable); err != nil {
		return err
	}
	current, err := schemaVersion(ctx, db)
	if err != nil {
		return err
	}
	latest := steps[len(steps)-1].version
	if current > latest {
		// A newer database is not something this build may "fix": it may be
		// missing columns and states that the running code cannot understand.
		return fmt.Errorf("taskstore: database schema version %d is newer than this build supports (%d)", current, latest)
	}
	for _, step := range steps {
		if step.version <= current {
			continue
		}
		if err := applyMigration(ctx, db, step); err != nil {
			return err
		}
	}
	return nil
}

// applyMigration runs one step and records its version in the same
// transaction, so a partially applied step cannot be recorded. It takes the
// write lock the same way a mutation does, so a second process opening the same
// database waits under its context instead of failing on a busy handler.
func applyMigration(ctx context.Context, db *sql.DB, step migration) error {
	tx, err := beginWriteTx(ctx, db)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range step.statements {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("taskstore: migration %d (%s) failed: %w", step.version, step.name, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
		step.version, step.name, formatTime(time.Now().UTC())); err != nil {
		return dbError("record migration", err)
	}
	if err := tx.Commit(); err != nil {
		return dbError("commit migration", err)
	}
	return nil
}

// checkMigrations rejects a migration list that could not be applied in order.
func checkMigrations(steps []migration) error {
	if len(steps) == 0 {
		return fmt.Errorf("taskstore: no migrations to apply")
	}
	prev := 0
	for _, step := range steps {
		if step.version < 1 || step.version <= prev {
			return fmt.Errorf("taskstore: migration versions must be positive and strictly increasing, got %d after %d", step.version, prev)
		}
		prev = step.version
	}
	return nil
}

func schemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var version int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return 0, dbError("read schema version", err)
	}
	return version, nil
}

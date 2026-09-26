package policy

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
// tests can exercise rollback with a deliberately broken step without mutating
// shared state.
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
		name:    "principals and scopes",
		statements: []string{
			// One row per issued credential. token_hash is UNIQUE because a
			// token hash identifies exactly one principal: Authenticate looks
			// a credential up by hash, so a duplicate would make the answer
			// ambiguous. revoked_at is NULL while the credential is live and
			// holds the instant of revocation afterwards; a revoked row is
			// kept rather than deleted, so "this credential was revoked" stays
			// answerable after the fact.
			`CREATE TABLE principals (
				id         TEXT PRIMARY KEY,
				role       TEXT NOT NULL CHECK (role IN ('admin','operator','viewer')),
				token_hash TEXT NOT NULL UNIQUE,
				issued_at  TEXT NOT NULL,
				expires_at TEXT NOT NULL,
				revoked_at TEXT
			)`,
			// The node scope of a principal, one row per node. The primary key
			// makes the set a set: a duplicate node in a scope cannot exist
			// even if a caller asks for one. An admin has no rows here, which
			// is the representation of "global": a scope that is empty because
			// it was never granted cannot be confused with a scope that was
			// granted and then narrowed.
			`CREATE TABLE principal_scopes (
				principal_id TEXT NOT NULL REFERENCES principals (id),
				node_id      TEXT NOT NULL,
				PRIMARY KEY (principal_id, node_id)
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
		// Refusing to start is the safe outcome for a permission store.
		return fmt.Errorf("policy: database schema version %d is newer than this build supports (%d)", current, latest)
	}
	for _, step := range steps {
		if step.version <= current {
			continue
		}
		if err := applyMigration(ctx, db, step, latest); err != nil {
			return err
		}
	}
	return nil
}

// applyMigration runs one step and records its version in the same
// transaction, so a partially applied step cannot be recorded. It takes the
// write lock the same way a mutation does, so a second process opening the same
// database waits under its context instead of failing on a busy handler.
func applyMigration(ctx context.Context, db *sql.DB, step migration, latest int) error {
	tx, err := beginWriteTx(ctx, db)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Another opener may have migrated after our initial version read. Make
	// the decision again while holding the write lock, before executing DDL.
	current, err := schemaVersion(ctx, tx)
	if err != nil {
		return err
	}
	if current > latest {
		return fmt.Errorf("policy: database schema version %d is newer than this build supports (%d)", current, latest)
	}
	if current >= step.version {
		return nil
	}
	for _, stmt := range step.statements {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("policy: migration %d (%s) failed: %w", step.version, step.name, err)
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
		return fmt.Errorf("policy: no migrations to apply")
	}
	prev := 0
	for _, step := range steps {
		if step.version < 1 || step.version <= prev {
			return fmt.Errorf("policy: migration versions must be positive and strictly increasing, got %d after %d", step.version, prev)
		}
		prev = step.version
	}
	return nil
}

func schemaVersion(ctx context.Context, db querier) (int, error) {
	var version int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return 0, dbError("read schema version", err)
	}
	return version, nil
}

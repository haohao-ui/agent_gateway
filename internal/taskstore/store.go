// Package taskstore is the M1 task state machine over a local SQLite database.
//
// It implements the internal/taskstore contract in docs/CONTRACTS.md: durable
// task state (queued, leased, running, cancel_requested, succeeded, failed,
// cancelled, unknown), one secret lease credential per attempt stored only as a
// hash, submission that is idempotent per (node, idempotency key), and lease
// expiry that parks an execution in unknown instead of re-running it.
//
// The package is a narrow domain+storage facade. It has no transport, HTTP,
// MCP or process dependencies, and it starts no goroutines: the caller drives
// lease expiry through Expire, so the store never needs a background owner.
// Every mutation runs inside one BEGIN IMMEDIATE transaction, so the
// read-modify-write sequences below (claim, cancel, complete, expire) cannot
// interleave with each other, not even across processes sharing the file.
package taskstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Database tuning. These are explicit rather than left to driver defaults
// because the values decide how the store behaves under contention.
const (
	// maxOpenConns bounds the pool for a file database. WAL lets readers run
	// while one writer holds the write lock, so a small pool is enough;
	// additional connections only add page-cache memory and lock contention.
	maxOpenConns = 4

	// busyTimeoutMS is the SQLite-level wait for a lock inside a single
	// statement. It is deliberately short: SQLite's busy handler cannot be
	// interrupted, so sqlite3_interrupt() does not end a busy wait and every
	// millisecond spent there is a millisecond in which a caller's context
	// cannot stop the operation. Waiting for the write lock therefore happens
	// in Go, in beginWriteTx, where the context is checked between attempts.
	// This value only has to absorb the brief collisions that can still happen
	// inside a transaction that already holds the write lock, such as a
	// concurrent checkpoint, and it bounds how long a caller's deadline can be
	// overshot by one attempt.
	busyTimeoutMS = 200

	// lockWaitTimeout is how long a write transaction waits for the write lock
	// before failing. The wait is the Go loop in beginWriteTx, so a caller's
	// context can end it sooner.
	lockWaitTimeout = 5 * time.Second

	// lockRetryInterval is the pause between attempts at the write lock. An
	// attempt already spends up to busyTimeoutMS inside SQLite, whose busy
	// handler backs off exponentially, so this only prevents a tight spin.
	lockRetryInterval = 10 * time.Millisecond

	// memoryDSN is the one non-file database this package accepts. It is
	// pinned to a single pooled connection because SQLite gives every
	// connection its own private in-memory database.
	memoryDSN = ":memory:"
)

// Store is a handle to one SQLite-backed task database.
//
// A Store is safe for concurrent use by multiple goroutines. It owns the
// database handle until Close. Lease tokens never leave the store except
// through Claim: Get and every other read return protocol.Task, which has no
// token field.
type Store struct {
	db    *sql.DB
	clock func() time.Time
}

// Open opens the SQLite database at path, creating the file when it does not
// exist, and applies pending schema migrations.
//
// path must be a plain filesystem path or ":memory:". A file created here has
// mode 0600, because a task database holds task payloads and agent output that
// other local accounts should not read; an existing file keeps the mode it
// already had. The parent directory is never created for the caller.
//
// If opening or migrating fails, the connection pool is closed before Open
// returns, so a failed call leaves no file lock, WAL or pooled connection
// behind.
func Open(path string) (*Store, error) {
	return open(context.Background(), path, defaultConfig())
}

// config holds the injection points Open uses. Production callers get the real
// SQLite connector and the wall clock; tests substitute both.
type config struct {
	steps   []migration
	connect func(dsn string) (driver.Connector, error)
	clock   func() time.Time
}

func defaultConfig() config {
	return config{
		steps:   schemaMigrations,
		connect: sqlite.NewConnector,
		clock:   time.Now,
	}
}

// open is Open with its dependencies supplied. It keeps the deferred Close so
// that no failure path can leak the pool.
func open(ctx context.Context, path string, cfg config) (*Store, error) {
	if cfg.clock == nil {
		cfg.clock = time.Now
	}
	if err := checkPath(path); err != nil {
		return nil, err
	}
	if err := ensureFile(path); err != nil {
		return nil, err
	}
	connector, err := cfg.connect(dsnFor(path))
	if err != nil {
		return nil, fmt.Errorf("taskstore: open %s: %w", path, err)
	}
	db := sql.OpenDB(connector)
	if path == memoryDSN {
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
	} else {
		db.SetMaxOpenConns(maxOpenConns)
		db.SetMaxIdleConns(maxOpenConns)
	}
	ok := false
	defer func() {
		if !ok {
			// A pool that stays open holds the SQLite file lock and the WAL
			// for the lifetime of the process.
			_ = db.Close()
		}
	}()
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("taskstore: connect %s: %w", path, err)
	}
	if err := migrate(ctx, db, cfg.steps); err != nil {
		return nil, err
	}
	ok = true
	return &Store{db: db, clock: cfg.clock}, nil
}

// Close releases the database handle. It is idempotent and never removes the
// database file. Calls made after Close fail with a "database is closed" error
// rather than panicking.
func (s *Store) Close() error {
	return s.db.Close()
}

// checkPath rejects paths whose meaning the driver would reinterpret. Only
// plain filesystem paths and ":memory:" are accepted so that a caller cannot
// smuggle in a URI, a query string or an overriding DSN parameter.
func checkPath(path string) error {
	switch {
	case path == "":
		return invalidf("database path is empty")
	case path == memoryDSN:
		return nil
	case strings.HasPrefix(path, "file:"):
		return invalidf("database path must be a plain filesystem path, not a file: URI")
	case strings.ContainsAny(path, "?#"):
		return invalidf("database path must not contain '?' or '#'")
	}
	return nil
}

// ensureFile creates the database file with mode 0600 before SQLite opens it.
// SQLite would otherwise create it with the process umask, typically 0644.
func ensureFile(path string) error {
	if path == memoryDSN {
		return nil
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("taskstore: create database file: %w", err)
	}
	return f.Close()
}

// dsnFor builds the driver DSN. Every option is deliberate:
//
//   - _txlock=immediate: BeginTx issues BEGIN IMMEDIATE, so a write
//     transaction takes the write lock up front instead of discovering a
//     conflict halfway through and failing to upgrade.
//   - _journal_mode=WAL: readers do not block the writer.
//   - _synchronous=FULL: WAL plus FULL fsyncs each commit. Task rows are the
//     durable record of what the gateway asked a node to run, so the extra
//     fsync is worth more here than write throughput.
//   - _busy_timeout: a short backstop, not the way the write lock is waited
//     for; see busyTimeoutMS and beginWriteTx.
//   - _foreign_keys=1: the attempts and idempotency tables reference tasks;
//     enforcing that keeps a half-written claim impossible.
//   - _dqs=0 and _defensive=1: reject double-quoted string literals and
//     SQL-level schema writes, turning two classes of silent corruption into
//     loud errors.
func dsnFor(path string) string {
	return path + "?" + strings.Join([]string{
		"_txlock=immediate",
		"_journal_mode=WAL",
		"_synchronous=FULL",
		"_busy_timeout=" + strconv.Itoa(busyTimeoutMS),
		"_foreign_keys=1",
		"_dqs=0",
		"_defensive=1",
	}, "&")
}

// querier is the subset of *sql.DB and *sql.Tx used by the statement helpers,
// so the same code reads through the pool and inside a transaction. Helpers
// take a querier rather than reaching for s.db so that a statement meant to be
// transactional cannot silently run outside the transaction.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// writeTx runs fn inside a single write transaction and commits it.
//
// The DSN sets _txlock=immediate, so the write lock is held from BEGIN to
// COMMIT: statements inside fn see a stable database, and no other writer can
// slip between the read and the write of a read-modify-write sequence. fn must
// use q for every statement, never the pool.
//
// now is read after the write lock is held, and is the only time a mutation may
// use. A caller that waited for the lock must not be judged by the instant it
// arrived: a lease stamped with the arrival time would be issued with part of
// its lifetime already spent, and an expiry check would compare against a time
// that has already passed by the time the check runs.
func (s *Store) writeTx(ctx context.Context, fn func(q querier, now time.Time) error) error {
	tx, err := beginWriteTx(ctx, s.db)
	if err != nil {
		return err
	}
	// Rollback after a successful Commit is a no-op, and the driver's Rollback
	// ignores the caller's context, so a cancelled context still cleans up the
	// transaction instead of stranding it on a pooled connection.
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx, s.now()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return dbError("commit transaction", err)
	}
	return nil
}

// beginWriteTx starts a write transaction, waiting for the write lock in Go
// rather than inside SQLite.
//
// SQLite's busy handler cannot be interrupted: sqlite3_interrupt() does not
// end a busy wait. Leaving the wait to the busy timeout would therefore make a
// caller's context unable to stop a contended write, and a cancelled or
// timed-out request would still hold its goroutine for the whole timeout. Here
// the SQLite-level timeout is short (busyTimeoutMS) and this loop owns the
// waiting: it checks the context before every attempt, ends the wait as soon as
// the context is done, and still gives a writer that merely has to wait its
// turn up to lockWaitTimeout.
//
// The transaction body itself is never retried: once BEGIN IMMEDIATE succeeds
// the write lock is held, so the body runs exactly once and a failure inside it
// is reported rather than replayed.
func beginWriteTx(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	var tx *sql.Tx
	err := lockWait(ctx, "begin transaction", func() error {
		var err error
		tx, err = db.BeginTx(ctx, nil)
		return err
	})
	if err != nil {
		return nil, err
	}
	return tx, nil
}

// execLocked runs one statement that needs the write lock, waiting for it the
// same way beginWriteTx does. It is for the statements that cannot be part of a
// transaction, such as creating the migration bookkeeping table.
func execLocked(ctx context.Context, db *sql.DB, op, query string) error {
	return lockWait(ctx, op, func() error {
		_, err := db.ExecContext(ctx, query)
		return err
	})
}

// lockWait runs attempt until it stops being refused by a lock, the context
// ends, or the lock budget runs out.
//
// attempt must be safe to repeat: either a BEGIN IMMEDIATE, which either takes
// the lock or leaves the connection untouched, or a single statement that is
// idempotent by construction. Nothing that has already changed data is retried
// here.
func lockWait(ctx context.Context, op string, attempt func() error) error {
	deadline := time.Now().Add(lockWaitTimeout)
	for {
		if err := ctx.Err(); err != nil {
			return lockWaitError(err)
		}
		err := attempt()
		if err == nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			// The context ended while this attempt was running: report that,
			// not the lock error the attempt happened to return.
			return lockWaitError(ctxErr)
		}
		if !isLockError(err) {
			return dbError(op, err)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("taskstore: write lock not acquired within %s: %w", lockWaitTimeout, err)
		}
		if err := sleepCtx(ctx, lockRetryInterval); err != nil {
			return lockWaitError(err)
		}
	}
}

// lockWaitError reports that a caller's context ended the wait for the write
// lock. The context error stays attached, so a caller can tell a deadline from
// a cancellation with errors.Is.
func lockWaitError(err error) error {
	return fmt.Errorf("taskstore: waiting for the write lock: %w", err)
}

// isLockError reports whether err is SQLite refusing a lock. Extended result
// codes share their low byte with the primary code (SQLITE_BUSY_SNAPSHOT and
// friends), so the primary code is what gets compared.
func isLockError(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	switch sqliteErr.Code() & 0xff {
	case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
		return true
	}
	return false
}

// sleepCtx waits for d unless the context ends first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// now returns the current time in UTC. UTC is part of the storage format, not
// a display choice: persisted times are compared as fixed-width UTC text.
func (s *Store) now() time.Time {
	return s.clock().UTC()
}

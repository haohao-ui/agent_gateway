// Package policy is the M2 operator-permission store: roles, node scopes and
// revocable bearer credentials for the humans and services that drive the
// gateway.
//
// It owns one SQLite database of its own, so permission data is never mixed
// with the task database and a schema change here cannot touch task state.
// The package is a narrow domain+storage facade: it has no transport, HTTP,
// MCP or process dependencies, starts no goroutines, and does no logging, so a
// credential cannot leak through a log line by accident.
//
// The two halves of the contract are deliberately separate:
//
//   - Store.Issue/Authenticate/Revoke are the credential lifecycle. They are
//     the only functions that touch the database, and they are the barrier
//     against revoked or expired authority: a token is accepted only if a
//     non-revoked, unexpired row exists for its hash at the moment of the call.
//   - Authorize is a pure decision over a Principal value. It reads no
//     database, takes no clock and no context, so it cannot be fooled by a
//     stale cache; the flip side is that it trusts the Principal it is given.
//     A Principal carries authority only because Authenticate produced it, and
//     callers must not build one from request data.
//
// Tokens are 256 bits from crypto/rand. The cleartext token is returned once,
// by Issue, and never stored: the database holds only its SHA-256 hash, so a
// stolen database file or a backup cannot be replayed as a credential. Issue
// and Revoke are administrative operations meant for trusted local callers
// (a CLI or a bootstrap path) and are not authentication endpoints; wiring
// them to the network is an integration decision for the coordinator.
package policy

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

// Database tuning. The same shape as the task store's, because both databases
// are files shared by a gateway process plus whatever local tooling opens the
// same path; the values are repeated here rather than shared because
// internal/taskstore is a frozen M1 interface owned by another worker and
// exposes none of these helpers.
const (
	// maxOpenConns bounds the pool for a file database. WAL lets readers run
	// while one writer holds the write lock, so a small pool is enough.
	maxOpenConns = 4

	// busyTimeoutMS is the SQLite-level wait for a lock inside a single
	// statement. It is deliberately short: SQLite's busy handler cannot be
	// interrupted, so sqlite3_interrupt() does not end a busy wait and every
	// millisecond spent there is a millisecond in which a caller's context
	// cannot stop the operation. Waiting for the write lock therefore happens
	// in Go, in beginWriteTx, where the context is checked between attempts.
	busyTimeoutMS = 200

	// lockWaitTimeout is how long a write transaction waits for the write lock
	// before failing. The wait is the Go loop in lockWait, so a caller's
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

// Store is a handle to one SQLite-backed permission database.
//
// A Store is safe for concurrent use by multiple goroutines, and several
// processes may open the same file: every mutation runs in one write
// transaction and every read of a credential happens on a fresh snapshot, so
// a revocation issued through one handle is visible to the others at once.
// The store keeps no cache of principals, which is what makes that true.
type Store struct {
	db    *sql.DB
	clock func() time.Time
}

// Open opens the SQLite database at path, creating the file when it does not
// exist, and applies pending schema migrations.
//
// path must be a plain filesystem path or ":memory:". A file created here has
// mode 0600, because the database holds the hashes of live operator
// credentials and the scopes they carry; an existing file keeps the mode it
// already had. The parent directory is never created for the caller.
//
// If opening or migrating fails, the connection pool is closed before Open
// returns, so a failed call leaves no file lock, WAL or pooled connection
// behind.
func Open(path string) (*Store, error) {
	return open(context.Background(), path, defaultConfig())
}

// config holds the injection points Open uses. Production callers get the real
// SQLite connector and the wall clock; tests substitute both, so a test can
// pin the clock that decides whether a credential has expired and can drive a
// migration failure without touching a shared package global.
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
		return nil, fmt.Errorf("policy: open %s: %w", path, err)
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
	if err := connectWithLockWait(ctx, db, path); err != nil {
		return nil, err
	}
	if err := migrate(ctx, db, cfg.steps); err != nil {
		return nil, err
	}
	ok = true
	return &Store{db: db, clock: cfg.clock}, nil
}

// connectWithLockWait establishes the first connection, waiting for the write
// lock the same way a mutation does.
//
// The driver applies the DSN's PRAGMAs while it opens a connection, and
// switching the journal mode to WAL takes the database lock. When several
// processes open the same new database at the same moment — the gateway
// starting beside a local tool, or a test opening the same path from many
// goroutines — the losers of that race get SQLITE_BUSY from the connect itself,
// before any statement of ours runs, so a retry inside a transaction cannot help
// them. Waiting here is safe because establishing a connection changes nothing
// but those connection settings, and a failed attempt leaves nothing behind:
// database/sql drops a connection whose Connect or first statement failed and
// the next attempt opens a fresh one.
func connectWithLockWait(ctx context.Context, db *sql.DB, path string) error {
	return lockWait(ctx, "connect "+path, func() error {
		return db.PingContext(ctx)
	})
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
		return fmt.Errorf("policy: create database file: %w", err)
	}
	return f.Close()
}

// dsnFor builds the driver DSN. Every option is deliberate:
//
//   - _txlock=immediate: BeginTx issues BEGIN IMMEDIATE, so a write
//     transaction takes the write lock up front instead of discovering a
//     conflict halfway through and failing to upgrade. Reads do not pay for
//     this: readTx asks for a read-only transaction, which the driver begins
//     with a plain (deferred) BEGIN.
//   - _journal_mode=WAL: readers do not block the writer, and a reader sees a
//     consistent snapshot for the length of its transaction. That is what lets
//     a credential be read and its scope be read in one atomic step while
//     another process revokes it.
//   - _synchronous=FULL: WAL plus FULL fsyncs each commit. A committed
//     revocation is a security decision, so it must survive a power loss
//     rather than living only in a WAL buffer.
//   - _busy_timeout: a short backstop, not the way the write lock is waited
//     for; see busyTimeoutMS and lockWait.
//   - _foreign_keys=1: the scope table references principals; enforcing that
//     keeps a scope row from outliving the principal it belongs to.
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
// slip between the read and the write of a read-modify-write sequence such as
// "revoke unless it is already revoked".
//
// now is read after the write lock is held, and is the only time a mutation may
// use. A caller that waited for the lock must not be judged by the instant it
// arrived: a credential issued around a contested lock would otherwise be
// stamped against a time that has already passed by the time the check runs.
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

// readTx runs fn inside one read-only transaction, so every statement in fn
// sees the same snapshot of the database.
//
// This is what makes Authenticate atomic: the principal row and its scope rows
// are two queries, and without a snapshot a revocation committed between them
// would leave the caller holding authority assembled from two different
// versions of the database. The transaction is read-only, so the driver begins
// it deferred and it takes no write lock — in WAL mode it never blocks the
// writer. Only the BEGIN is retried on a lock error; the statements inside use
// the driver's short busy timeout, because a read that is waiting on a
// checkpoint cannot do anything more useful than wait.
//
// fn must use q for every statement, never the pool.
func (s *Store) readTx(ctx context.Context, fn func(q querier) error) error {
	var tx *sql.Tx
	err := lockWait(ctx, "begin read transaction", func() error {
		var err error
		tx, err = s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		return err
	})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	return fn(tx)
}

// beginWriteTx starts a write transaction, waiting for the write lock in Go
// rather than inside SQLite.
//
// SQLite's busy handler cannot be interrupted: sqlite3_interrupt() does not
// end a busy wait. Leaving the wait to the busy timeout would therefore make a
// caller's context unable to stop a contended write, and a cancelled request
// would still hold its goroutine for the whole timeout. Here the SQLite-level
// timeout is short (busyTimeoutMS) and this loop owns the waiting: it checks
// the context before every attempt, ends the wait as soon as the context is
// done, and still gives a writer that merely has to wait its turn up to
// lockWaitTimeout.
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
// same way beginWriteTx does. It is for statements that cannot be part of a
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
// attempt must be safe to repeat: either a BEGIN, which either takes its lock
// or leaves the connection untouched, or a single statement that is idempotent
// by construction. Nothing that has already changed data is retried here.
//
// A non-lock failure is returned as the caller's error, wrapped only to name
// the operation; errors.Is still finds whatever sentinel or driver error is
// inside it.
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
			return fmt.Errorf("policy: write lock not acquired within %s: %w", lockWaitTimeout, err)
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
	return fmt.Errorf("policy: waiting for the write lock: %w", err)
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
// a display choice: persisted times are compared as fixed-width UTC text, and
// an expiry decision must not depend on the timezone of the host that opened
// the database.
func (s *Store) now() time.Time {
	return s.clock().UTC()
}

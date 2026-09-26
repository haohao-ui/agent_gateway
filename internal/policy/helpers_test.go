package policy

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sqlite "modernc.org/sqlite"
)

// testNode is the node most tests scope a principal to; otherNode is a node
// the same principal must not reach.
const (
	testNode  = "node-1"
	otherNode = "node-2"
)

// testTime is the instant every frozen clock starts at. Fixing it keeps the
// expiry arithmetic exact: a test can add a duration and know whether the
// result is before or after "now" to the nanosecond.
var testTime = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

// newStore opens a store on a fresh temporary database file.
func newStore(t *testing.T) *Store {
	t.Helper()
	return openStoreAt(t, filepath.Join(t.TempDir(), "policy.db"))
}

// newFrozenStore opens a store whose clock is pinned to testTime.
func newFrozenStore(t *testing.T) *Store {
	t.Helper()
	st := newStore(t)
	freezeClock(st, testTime)
	return st
}

// openStoreAt opens a store at an explicit path so a test can close it and
// reopen the same database. The cleanup Close is a no-op for a store the test
// already closed.
func openStoreAt(t *testing.T, path string) *Store {
	t.Helper()
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return st
}

// freezeClock pins a store's clock. Expiry is then driven by the test, so no
// test has to sleep.
func freezeClock(st *Store, at time.Time) {
	st.clock = func() time.Time { return at }
}

// mustIssue issues a principal and fails the test if it cannot be issued.
func mustIssue(t *testing.T, st *Store, role Role, nodeIDs []string, expiresAt time.Time) (Principal, string) {
	t.Helper()
	p, token, err := st.Issue(context.Background(), role, nodeIDs, expiresAt)
	if err != nil {
		t.Fatalf("Issue(%s, %v): %v", role, nodeIDs, err)
	}
	return p, token
}

// mustAuthenticate authenticates a token and fails the test if it is refused.
func mustAuthenticate(t *testing.T, st *Store, token string) Principal {
	t.Helper()
	p, err := st.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	return p
}

// mustRevoke revokes a principal and fails the test if the call fails.
func mustRevoke(t *testing.T, st *Store, principalID string) {
	t.Helper()
	if err := st.Revoke(context.Background(), principalID); err != nil {
		t.Fatalf("Revoke(%s): %v", principalID, err)
	}
}

// rawOpen opens the same database file outside the store, for assertions about
// what is physically stored.
func rawOpen(t *testing.T, path string) *sql.DB {
	t.Helper()
	connector, err := sqlite.NewConnector(dsnFor(path))
	if err != nil {
		t.Fatalf("NewConnector: %v", err)
	}
	db := sql.OpenDB(connector)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("ping raw database: %v", err)
	}
	return db
}

func countPrincipals(t *testing.T, st *Store) int {
	t.Helper()
	var n int
	if err := st.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM principals`).Scan(&n); err != nil {
		t.Fatalf("count principals: %v", err)
	}
	return n
}

func countScopes(t *testing.T, st *Store, principalID string) int {
	t.Helper()
	var n int
	if err := st.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM principal_scopes WHERE principal_id = ?`, principalID).Scan(&n); err != nil {
		t.Fatalf("count scopes: %v", err)
	}
	return n
}

// storedRevokedAt reads the revoked_at column directly, which is how a test
// tells "never revoked" from "revoked twice, timestamp kept from the first".
func storedRevokedAt(t *testing.T, st *Store, principalID string) sql.NullString {
	t.Helper()
	var revokedAt sql.NullString
	if err := st.db.QueryRowContext(context.Background(),
		`SELECT revoked_at FROM principals WHERE id = ?`, principalID).Scan(&revokedAt); err != nil {
		t.Fatalf("read revoked_at: %v", err)
	}
	return revokedAt
}

// storedHashCount counts rows whose token_hash equals hash. It is the positive
// control for the "no plaintext token" scans: it proves the token's hash is
// what the database holds.
func storedHashCount(t *testing.T, st *Store, hash string) int {
	t.Helper()
	var n int
	if err := st.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM principals WHERE token_hash = ?`, hash).Scan(&n); err != nil {
		t.Fatalf("count token hashes: %v", err)
	}
	return n
}

func hasTable(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil {
		t.Fatalf("look up table %s: %v", name, err)
	}
	return n > 0
}

func tableVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var version int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	return version
}

// migrationCount counts recorded migrations, which is how a test proves that
// reopening an up-to-date database applied nothing.
func migrationCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	return n
}

// takeWriteLock reserves a pooled connection and holds the write lock on it.
// The returned function releases both; it is also registered as a cleanup, so a
// failing test cannot leave the lock held.
func takeWriteLock(t *testing.T, st *Store) func() {
	t.Helper()
	ctx := context.Background()
	conn, err := st.db.Conn(ctx)
	if err != nil {
		t.Fatalf("reserve a connection: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		_ = conn.Close()
		t.Fatalf("take the write lock: %v", err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			if _, err := conn.ExecContext(context.Background(), `ROLLBACK`); err != nil {
				t.Errorf("release the write lock: %v", err)
			}
			if err := conn.Close(); err != nil {
				t.Errorf("close the lock holder: %v", err)
			}
		})
	}
	t.Cleanup(release)
	return release
}

// waitForConnectionsInUse blocks until the pool reports n connections in use.
// A test uses it to know that a call has taken a connection of its own and is
// on its way into the write lock, rather than sleeping and hoping.
func waitForConnectionsInUse(t *testing.T, st *Store, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if inUse := st.db.Stats().InUse; inUse >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d connections in use, have %d", n, st.db.Stats().InUse)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitUntil blocks until cond reports true. It makes a concurrency test wait
// for the event it wants to happen next instead of sleeping and hoping: a test
// that revokes while its readers are still starting up would otherwise prove
// nothing and pass anyway.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// runWhileLockHeld runs fn while another connection holds the write lock, and
// moves the store's clock from before to after at the moment the lock is
// released.
//
// That ordering is what makes a stale timestamp observable: a call that reads
// the clock before waiting for the lock sees before, while one that reads it
// after acquiring the lock sees after. The clock only moves once the lock is
// released, so the result does not depend on how long the wait takes.
func runWhileLockHeld(t *testing.T, st *Store, before, after time.Time, fn func()) {
	t.Helper()
	release := takeWriteLock(t, st)
	var released int32
	st.clock = func() time.Time {
		if atomic.LoadInt32(&released) == 1 {
			return after
		}
		return before
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	// Wait until the call holds a connection, then let it proceed.
	waitForConnectionsInUse(t, st, 2)
	atomic.StoreInt32(&released, 1)
	release()
	<-done
}

// databaseFiles lists every file SQLite wrote for a database path, including
// the WAL and shared-memory sidecars, so a scan for a secret covers all of
// them.
func databaseFiles(t *testing.T, path string) []string {
	t.Helper()
	files, err := filepath.Glob(path + "*")
	if err != nil {
		t.Fatalf("glob database files: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("no database files were written for %s", path)
	}
	return files
}

// readDatabaseFiles reads every file belonging to a database path.
func readDatabaseFiles(t *testing.T, path string) map[string][]byte {
	t.Helper()
	out := make(map[string][]byte)
	for _, name := range databaseFiles(t, path) {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", filepath.Base(name), err)
		}
		out[filepath.Base(name)] = data
	}
	return out
}

// countingConnector wraps the real SQLite connector so a test can assert that
// a failed Open released the pool it created.
type countingConnector struct {
	inner  driver.Connector
	opened *int32
	closed *int32
}

func (c countingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	atomic.AddInt32(c.opened, 1)
	return &countingConn{Conn: conn, closed: c.closed}, nil
}

func (c countingConnector) Driver() driver.Driver { return c.inner.Driver() }

// countingConn forwards the optional driver interfaces to the wrapped
// connection, so wrapping does not change how database/sql drives it. An
// interface the wrapped connection does not implement reports driver.ErrSkip,
// which makes database/sql fall back to the prepared-statement path instead of
// failing.
type countingConn struct {
	driver.Conn
	closed *int32
}

func (c *countingConn) Close() error {
	err := c.Conn.Close()
	atomic.AddInt32(c.closed, 1)
	return err
}

func (c *countingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if p, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return p.PrepareContext(ctx, query)
	}
	return c.Conn.Prepare(query)
}

func (c *countingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if e, ok := c.Conn.(driver.ExecerContext); ok {
		return e.ExecContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

func (c *countingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if q, ok := c.Conn.(driver.QueryerContext); ok {
		return q.QueryContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

func (c *countingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if b, ok := c.Conn.(driver.ConnBeginTx); ok {
		return b.BeginTx(ctx, opts)
	}
	return c.Conn.Begin()
}

func (c *countingConn) Ping(ctx context.Context) error {
	if p, ok := c.Conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c *countingConn) ResetSession(ctx context.Context) error {
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (c *countingConn) IsValid() bool {
	if v, ok := c.Conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

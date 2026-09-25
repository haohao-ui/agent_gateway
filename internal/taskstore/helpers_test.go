package taskstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sqlite "modernc.org/sqlite"

	"agent-gateway/internal/protocol"
)

// testNode is the node every test submits and claims under unless it is
// deliberately testing a second node.
const testNode = "node-1"

// newStore opens a store on a fresh temporary database file.
func newStore(t *testing.T) *Store {
	t.Helper()
	return openStoreAt(t, filepath.Join(t.TempDir(), "tasks.db"))
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

// freezeClock pins a store's clock. Lease expiry is then driven by advance and
// by Expire's explicit now, so no test has to sleep.
func freezeClock(st *Store, at time.Time) {
	st.clock = func() time.Time { return at }
}

// advance moves a frozen clock forward from wherever it currently points.
func advance(st *Store, d time.Duration) {
	base := st.clock()
	st.clock = func() time.Time { return base.Add(d) }
}

// submitInput is a valid submission; tests change the fields they care about.
func submitInput() protocol.SubmitRequest {
	return protocol.SubmitRequest{
		NodeID:            testNode,
		Capability:        "agent.run",
		CapabilityVersion: 1,
		Input:             json.RawMessage(`{"prompt":"hello"}`),
		TimeoutSeconds:    60,
	}
}

func baseResult() protocol.Result {
	return protocol.Result{State: protocol.Succeeded, Text: "done", ExitCode: 0}
}

func mustSubmit(t *testing.T, st *Store, in protocol.SubmitRequest) protocol.Task {
	t.Helper()
	task, err := st.Submit(context.Background(), in)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	return task
}

func mustClaim(t *testing.T, st *Store, nodeID string, leaseFor time.Duration) *protocol.Lease {
	t.Helper()
	lease, err := st.Claim(context.Background(), nodeID, leaseFor)
	if err != nil {
		t.Fatalf("Claim(%s): %v", nodeID, err)
	}
	if lease == nil {
		t.Fatalf("Claim(%s) returned no task", nodeID)
	}
	return lease
}

func mustGet(t *testing.T, st *Store, taskID string) protocol.Task {
	t.Helper()
	task, err := st.Get(context.Background(), taskID)
	if err != nil {
		t.Fatalf("Get(%s): %v", taskID, err)
	}
	return task
}

// claimStart submits one task, claims it and starts it, which is the state
// every completion and cancellation test begins from.
func claimStart(t *testing.T, st *Store) (protocol.Task, *protocol.Lease) {
	t.Helper()
	task := mustSubmit(t, st, submitInput())
	lease := mustClaim(t, st, testNode, time.Minute)
	if lease.Task.ID != task.ID {
		t.Fatalf("claimed task %s, want %s", lease.Task.ID, task.ID)
	}
	if err := st.Start(context.Background(), task.ID, lease.Task.AttemptID, lease.Token); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return task, lease
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

func countTasks(t *testing.T, st *Store) int {
	t.Helper()
	var n int
	if err := st.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM tasks`).Scan(&n); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	return n
}

func countState(t *testing.T, st *Store, state protocol.State) int {
	t.Helper()
	var n int
	if err := st.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM tasks WHERE state = ?`, string(state)).Scan(&n); err != nil {
		t.Fatalf("count tasks in state %s: %v", state, err)
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
// A test uses it to know that a mutation has taken a connection of its own and
// is on its way into the write lock, rather than sleeping and hoping.
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

// runWhileLockHeld runs fn while another connection holds the write lock, and
// moves the store's clock from before to after the moment the lock is released.
//
// That ordering is what makes a stale timestamp observable: a mutation that
// reads the clock before waiting for the lock sees before, while one that reads
// it after acquiring the lock sees after. The clock only moves once the lock is
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
	// Wait until the mutation holds a connection, then let it proceed. Under
	// the old behaviour the clock had already been read by this point, which is
	// exactly what the assertions below detect.
	waitForConnectionsInUse(t, st, 2)
	atomic.StoreInt32(&released, 1)
	release()
	<-done
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

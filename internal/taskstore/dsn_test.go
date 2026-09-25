package taskstore

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
)

// TestConnectionSettingsTakeEffect checks the DSN options against the pinned
// driver (modernc.org/sqlite v1.59.0) instead of trusting them. Every option is
// read back from the connection it is supposed to affect, on each physical
// connection the pool can hand out: a DSN shorthand that the driver ignored
// would otherwise only show up as a durability or locking surprise later.
//
// The values asserted here are the ones dsnFor asks for. If the driver ever
// stops honouring a shorthand, this test fails with the observed value.
func TestConnectionSettingsTakeEffect(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.db")
	st := openStoreAt(t, path)

	// Hold one connection per pool slot: database/sql cannot hand the same
	// connection to two holders, so this forces every slot to be a distinct
	// physical connection, and each one gets checked.
	conns := make([]*sql.Conn, 0, maxOpenConns)
	for i := 0; i < maxOpenConns; i++ {
		conn, err := st.db.Conn(ctx)
		if err != nil {
			t.Fatalf("reserve connection %d: %v", i, err)
		}
		conns = append(conns, conn)
	}
	settings := []struct{ pragma, want, why string }{
		{"journal_mode", "wal", "WAL keeps readers from blocking the writer"},
		{"foreign_keys", "1", "attempts and idempotency reference tasks"},
		{"synchronous", "2", "2 is FULL: task state is the durable record"},
		{"busy_timeout", strconv.Itoa(busyTimeoutMS), "the short backstop behind the Go lock wait"},
	}
	for i, conn := range conns {
		for _, setting := range settings {
			if got := pragmaValue(t, ctx, conn, setting.pragma); got != setting.want {
				t.Errorf("connection %d: PRAGMA %s = %q, want %q (%s)", i, setting.pragma, got, setting.want, setting.why)
			}
		}
		// _dqs=0: an unresolved double-quoted identifier is an error instead of
		// silently becoming a string literal.
		var ignored string
		if err := conn.QueryRowContext(ctx, `SELECT "not_a_column"`).Scan(&ignored); err == nil {
			t.Errorf("connection %d accepted a double-quoted unknown identifier; _dqs=0 is not in effect", i)
		}
		// _defensive=1: SQL-level schema writes are no-ops.
		if _, err := conn.ExecContext(ctx, `PRAGMA writable_schema = ON`); err != nil {
			t.Errorf("connection %d: PRAGMA writable_schema = ON: %v", i, err)
		}
		if got := pragmaValue(t, ctx, conn, "writable_schema"); got != "0" {
			t.Errorf("connection %d: writable_schema = %q, want 0 while _defensive=1", i, got)
		}
		if err := conn.Close(); err != nil {
			t.Errorf("close connection %d: %v", i, err)
		}
	}

	// _txlock=immediate: a transaction started through the pool takes the write
	// lock at BEGIN, so a second connection cannot take it while the
	// transaction is open. With a deferred BEGIN the probe below would succeed.
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	probe := rawOpen(t, path)
	if _, err := probe.ExecContext(ctx, `PRAGMA busy_timeout = 0`); err != nil {
		t.Fatalf("clear the probe's busy timeout: %v", err)
	}
	if _, err := probe.ExecContext(ctx, `BEGIN IMMEDIATE`); !isLockError(err) {
		t.Fatalf("BEGIN IMMEDIATE on a second connection = %v, want a lock error: the store's transaction did not take the write lock at BEGIN", err)
	}
}

// pragmaValue reads one PRAGMA from a connection and normalises the driver's
// representation to text, so a numeric setting and a text setting are compared
// the same way.
func pragmaValue(t *testing.T, ctx context.Context, conn *sql.Conn, name string) string {
	t.Helper()
	var raw any
	if err := conn.QueryRowContext(ctx, `PRAGMA `+name).Scan(&raw); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	switch value := raw.(type) {
	case string:
		return value
	case []byte:
		return string(value)
	default:
		return fmt.Sprint(value)
	}
}

package policy

import (
	"context"
	"database/sql/driver"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sqlite "modernc.org/sqlite"

	"agent-gateway/internal/protocol"
)

func TestOpenCreatesFileWithPrivateMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.db")
	st := openStoreAt(t, path)
	if _, _, err := st.Issue(context.Background(), Operator, []string{testNode}, testTime.Add(time.Hour)); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat database: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("database mode is %#o, want no group or other access", perm)
	}
}

func TestOpenRejectsUnusablePaths(t *testing.T) {
	for _, path := range []string{"", "file:policy.db", "policy.db?mode=memory", "policy#1.db"} {
		if _, err := Open(path); !errors.Is(err, protocol.ErrInvalid) {
			t.Errorf("Open(%q) = %v, want protocol.ErrInvalid", path, err)
		}
	}
}

func TestOpenFailsWhenDirectoryIsMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "policy.db")
	st, err := Open(path)
	if err == nil {
		_ = st.Close()
		t.Fatal("Open created a database in a directory that does not exist")
	}
}

// failingMigrations is a list whose second step cannot apply. The failure is a
// duplicate CREATE TABLE rather than a syntax error so the first statement of
// the step has already run when the second one fails.
func failingMigrations() []migration {
	return []migration{
		{version: 1, name: "base", statements: []string{`CREATE TABLE probe (id TEXT PRIMARY KEY)`}},
		{version: 2, name: "broken", statements: []string{
			`CREATE TABLE partial (id TEXT PRIMARY KEY)`,
			`CREATE TABLE partial (id TEXT PRIMARY KEY)`,
		}},
	}
}

// workingMigrations is failingMigrations with the broken statement corrected.
func workingMigrations() []migration {
	return []migration{
		{version: 1, name: "base", statements: []string{`CREATE TABLE probe (id TEXT PRIMARY KEY)`}},
		{version: 2, name: "fixed", statements: []string{
			`CREATE TABLE partial (id TEXT PRIMARY KEY)`,
			`CREATE TABLE second (id TEXT PRIMARY KEY)`,
		}},
	}
}

func TestOpenReleasesPoolWhenMigrationFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.db")
	var opened, closed int32
	cfg := config{
		steps: failingMigrations(),
		connect: func(dsn string) (driver.Connector, error) {
			inner, err := sqlite.NewConnector(dsn)
			if err != nil {
				return nil, err
			}
			return countingConnector{inner: inner, opened: &opened, closed: &closed}, nil
		},
		clock: time.Now,
	}
	st, err := open(context.Background(), path, cfg)
	if err == nil {
		_ = st.Close()
		t.Fatal("open succeeded with a broken migration")
	}
	if got := atomic.LoadInt32(&opened); got == 0 {
		t.Fatal("the pool never opened a connection, so the test proves nothing")
	}
	if got, want := atomic.LoadInt32(&closed), atomic.LoadInt32(&opened); got != want {
		t.Fatalf("closed %d of %d opened connections, want every connection released", got, want)
	}
}

func TestMigrationRollsBackFailedStep(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "policy.db")
	if st, err := open(ctx, path, config{steps: failingMigrations(), connect: sqlite.NewConnector, clock: time.Now}); err == nil {
		_ = st.Close()
		t.Fatal("open succeeded with a broken migration")
	}
	db := rawOpen(t, path)
	// The step that failed must have been rolled back entirely, and its
	// version must not have been recorded.
	if hasTable(t, db, "partial") {
		t.Fatal("the failed migration left a table behind")
	}
	if got := tableVersion(t, db); got != 1 {
		t.Fatalf("recorded schema version = %d, want 1", got)
	}
	// A corrected list must apply on top of the surviving step.
	st, err := open(ctx, path, config{steps: workingMigrations(), connect: sqlite.NewConnector, clock: time.Now})
	if err != nil {
		t.Fatalf("open with corrected migrations: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db = rawOpen(t, path)
	if !hasTable(t, db, "partial") {
		t.Fatal("the corrected migration did not apply")
	}
	if got := tableVersion(t, db); got != 2 {
		t.Fatalf("recorded schema version = %d, want 2", got)
	}
}

func TestMigrateRejectsInvalidLists(t *testing.T) {
	db := rawOpen(t, filepath.Join(t.TempDir(), "policy.db"))
	cases := map[string][]migration{
		"empty":        nil,
		"zero version": {{version: 0, name: "zero"}},
		"duplicate":    {{version: 1, name: "a"}, {version: 1, name: "b"}},
		"out of order": {{version: 2, name: "a"}, {version: 1, name: "b"}},
	}
	for name, steps := range cases {
		if err := migrate(context.Background(), db, steps); err == nil {
			t.Errorf("migrate(%s) succeeded, want an error", name)
		}
	}
}

// TestOpenRejectsNewerSchemaVersion: a database written by a newer build may
// hold columns and constraints this build cannot reason about, and guessing is
// not an option for a permission store.
func TestOpenRejectsNewerSchemaVersion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "policy.db")
	steps := append(append([]migration{}, schemaMigrations...),
		migration{version: 99, name: "future", statements: []string{`CREATE TABLE future (id TEXT PRIMARY KEY)`}})
	st, err := open(ctx, path, config{steps: steps, connect: sqlite.NewConnector, clock: time.Now})
	if err != nil {
		t.Fatalf("open with a future schema: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if st, err := Open(path); err == nil {
		_ = st.Close()
		t.Fatal("Open accepted a database written by a newer schema")
	}
}

// TestReopenAppliesEachMigrationOnce: opening an up-to-date database must
// record nothing new, or a restart would slowly fill the bookkeeping table.
func TestReopenAppliesEachMigrationOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.db")
	st := openStoreAt(t, path)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened := openStoreAt(t, path)
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db := rawOpen(t, path)
	if got := migrationCount(t, db); got != len(schemaMigrations) {
		t.Fatalf("recorded migrations = %d, want %d", got, len(schemaMigrations))
	}
	if got := tableVersion(t, db); got != schemaMigrations[len(schemaMigrations)-1].version {
		t.Fatalf("schema version = %d, want %d", got, schemaMigrations[len(schemaMigrations)-1].version)
	}
	if !hasTable(t, db, "principals") || !hasTable(t, db, "principal_scopes") {
		t.Fatal("the schema does not contain both tables")
	}
}

// TestSchemaEnforcesItsInvariants checks the constraints that back the Go
// rules: role values are restricted, a token hash identifies one principal,
// and a scope row cannot exist without its principal. A hand-edited or
// corrupted database must be refused by the schema rather than accepted.
func TestSchemaEnforcesItsInvariants(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "policy.db")
	st := openStoreAt(t, path)
	freezeClock(st, testTime)
	p, token := mustIssue(t, st, Viewer, []string{testNode}, testTime.Add(time.Hour))

	insert := `INSERT INTO principals (id, role, token_hash, issued_at, expires_at, revoked_at)
	           VALUES (?, ?, ?, ?, ?, NULL)`
	stamp := formatTime(testTime)
	if _, err := st.db.ExecContext(ctx, insert, "prn_x", "superuser", "hash-x", stamp, stamp); err == nil {
		t.Error("the schema accepted an unknown role")
	}
	if _, err := st.db.ExecContext(ctx, insert, "prn_y", "viewer", tokenHash(token), stamp, stamp); err == nil {
		t.Error("the schema accepted a duplicate token hash")
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO principal_scopes (principal_id, node_id) VALUES (?, ?)`,
		"prn_missing", testNode); err == nil {
		t.Error("the schema accepted a scope row for a principal that does not exist")
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO principal_scopes (principal_id, node_id) VALUES (?, ?)`,
		p.ID, p.NodeIDs[0]); err == nil {
		t.Error("the schema accepted a duplicate scope entry")
	}
	// The Go path still works after all those refusals.
	if mustAuthenticate(t, st, token).ID != p.ID {
		t.Fatal("the principal stopped authenticating")
	}
}

func TestMemoryDatabaseSupportsTheLifecycle(t *testing.T) {
	ctx := context.Background()
	st, err := Open(memoryDSN)
	if err != nil {
		t.Fatalf("Open(%s): %v", memoryDSN, err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	freezeClock(st, testTime)
	p, token := mustIssue(t, st, Operator, []string{testNode}, testTime.Add(time.Hour))
	if got := mustAuthenticate(t, st, token); got.ID != p.ID {
		t.Fatalf("authenticated %s, want %s", got.ID, p.ID)
	}
	if err := Authorize(Principal{ID: p.ID, Role: p.Role, NodeIDs: p.NodeIDs}, actionTaskRead, testNode); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	mustRevoke(t, st, p.ID)
	if _, err := st.Authenticate(ctx, token); !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("the revoked token = %v, want protocol.ErrUnauthorized", err)
	}
}

func TestOperationsAfterCloseFail(t *testing.T) {
	ctx := context.Background()
	st := newFrozenStore(t)
	p, token := mustIssue(t, st, Operator, []string{testNode}, testTime.Add(time.Hour))
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, _, err := st.Issue(ctx, Operator, []string{testNode}, testTime.Add(time.Hour)); err == nil {
		t.Error("Issue after Close succeeded")
	}
	if _, err := st.Authenticate(ctx, token); err == nil {
		t.Error("Authenticate after Close succeeded")
	}
	if err := st.Revoke(ctx, p.ID); err == nil {
		t.Error("Revoke after Close succeeded")
	}
	// Authorize is a pure decision and keeps working: it never had a
	// connection to lose.
	if err := Authorize(Principal{ID: p.ID, Role: p.Role, NodeIDs: p.NodeIDs}, actionTaskRead, testNode); err != nil {
		t.Errorf("Authorize after Close: %v", err)
	}
}

// TestDSNKeepsDatabaseOptionsOutOfThePath: the path a caller passes is used as
// a path, never as a DSN that could override journaling or foreign keys.
func TestDSNKeepsDatabaseOptionsOutOfThePath(t *testing.T) {
	for _, path := range []string{"file:x?_journal_mode=OFF", "x.db?_foreign_keys=0", "x.db#y"} {
		if _, err := Open(path); !errors.Is(err, protocol.ErrInvalid) {
			t.Errorf("Open(%q) = %v, want protocol.ErrInvalid", path, err)
		}
	}
	dsn := dsnFor("policy.db")
	for _, want := range []string{"_journal_mode=WAL", "_foreign_keys=1", "_txlock=immediate", "_synchronous=FULL"} {
		if !strings.Contains(dsn, want) {
			t.Errorf("the DSN is missing %s: %s", want, dsn)
		}
	}
}

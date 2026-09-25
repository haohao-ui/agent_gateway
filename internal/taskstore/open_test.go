package taskstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	sqlite "modernc.org/sqlite"

	"agent-gateway/internal/protocol"
)

func TestOpenCreatesFileWithPrivateMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.db")
	st := openStoreAt(t, path)
	if _, err := st.Submit(context.Background(), submitInput()); err != nil {
		t.Fatalf("Submit: %v", err)
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
	for _, path := range []string{"", "file:tasks.db", "tasks.db?mode=memory", "tasks#1.db"} {
		if _, err := Open(path); !errors.Is(err, protocol.ErrInvalid) {
			t.Errorf("Open(%q) = %v, want protocol.ErrInvalid", path, err)
		}
	}
}

func TestOpenFailsWhenDirectoryIsMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "tasks.db")
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
	path := filepath.Join(t.TempDir(), "tasks.db")
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
	path := filepath.Join(t.TempDir(), "tasks.db")
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
	db := rawOpen(t, filepath.Join(t.TempDir(), "tasks.db"))
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

func TestOpenRejectsNewerSchemaVersion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.db")
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

func TestReopenPreservesTaskAndResult(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.db")
	st := openStoreAt(t, path)
	task, lease := claimStart(t, st)
	result := protocol.Result{State: protocol.Succeeded, Text: "结果：中文输出", ExitCode: 0, Truncated: true}
	if err := st.Complete(ctx, task.ID, lease.Task.AttemptID, lease.Token, result); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	before, err := st.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := openStoreAt(t, path)
	after, err := reopened.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	assertSameTask(t, before, after)

	// The recorded attempt still authenticates an identical result ...
	if err := reopened.Complete(ctx, task.ID, lease.Task.AttemptID, lease.Token, result); err != nil {
		t.Fatalf("retransmit after reopen: %v", err)
	}
	// ... and still refuses a different one.
	changed := result
	changed.Text = "different"
	if err := reopened.Complete(ctx, task.ID, lease.Task.AttemptID, lease.Token, changed); !errors.Is(err, protocol.ErrConflict) {
		t.Fatalf("retransmit with a different result = %v, want protocol.ErrConflict", err)
	}
}

func TestReopenKeepsLeaseUsable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.db")
	st := openStoreAt(t, path)
	task := mustSubmit(t, st, submitInput())
	lease := mustClaim(t, st, testNode, time.Minute)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := openStoreAt(t, path)
	got, err := reopened.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got.State != protocol.Leased || got.AttemptID != lease.Task.AttemptID || got.LeaseExpiresAt == nil {
		t.Fatalf("lease did not survive the reopen: %+v", got)
	}
	if err := reopened.Start(ctx, task.ID, lease.Task.AttemptID, lease.Token); err != nil {
		t.Fatalf("Start after reopen: %v", err)
	}
	if err := reopened.Complete(ctx, task.ID, lease.Task.AttemptID, lease.Token, baseResult()); err != nil {
		t.Fatalf("Complete after reopen: %v", err)
	}
}

func TestOperationsAfterCloseFail(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	task := mustSubmit(t, st, submitInput())
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := st.Get(ctx, task.ID); err == nil {
		t.Fatal("Get after Close succeeded")
	}
	if _, err := st.Submit(ctx, submitInput()); err == nil {
		t.Fatal("Submit after Close succeeded")
	}
	if _, err := st.Claim(ctx, testNode, time.Minute); err == nil {
		t.Fatal("Claim after Close succeeded")
	}
}

func TestMemoryDatabaseSupportsTaskLifecycle(t *testing.T) {
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
	task := mustSubmit(t, st, submitInput())
	lease := mustClaim(t, st, testNode, time.Minute)
	if lease.Task.ID != task.ID {
		t.Fatalf("claimed %s, want %s", lease.Task.ID, task.ID)
	}
	if _, err := st.Get(ctx, task.ID); err != nil {
		t.Fatalf("Get: %v", err)
	}
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

// assertSameTask compares every field of two task views, using time.Equal so
// that two readings of the same instant compare equal.
func assertSameTask(t *testing.T, want, got protocol.Task) {
	t.Helper()
	if got.ID != want.ID || got.NodeID != want.NodeID || got.Capability != want.Capability ||
		got.CapabilityVersion != want.CapabilityVersion || got.TimeoutSeconds != want.TimeoutSeconds ||
		got.State != want.State || got.AttemptID != want.AttemptID || string(got.Input) != string(want.Input) {
		t.Fatalf("task changed:\nwant %+v\ngot  %+v", want, got)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) || !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Fatalf("timestamps changed:\nwant %s / %s\ngot  %s / %s",
			want.CreatedAt, want.UpdatedAt, got.CreatedAt, got.UpdatedAt)
	}
	switch {
	case want.LeaseExpiresAt == nil && got.LeaseExpiresAt != nil,
		want.LeaseExpiresAt != nil && got.LeaseExpiresAt == nil:
		t.Fatalf("lease expiry presence changed: want %v, got %v", want.LeaseExpiresAt, got.LeaseExpiresAt)
	case want.LeaseExpiresAt != nil && !got.LeaseExpiresAt.Equal(*want.LeaseExpiresAt):
		t.Fatalf("lease expiry changed: want %s, got %s", want.LeaseExpiresAt, got.LeaseExpiresAt)
	}
	switch {
	case want.Result == nil && got.Result != nil,
		want.Result != nil && got.Result == nil:
		t.Fatalf("result presence changed: want %v, got %v", want.Result, got.Result)
	case want.Result != nil && !sameResult(*want.Result, *got.Result):
		t.Fatalf("result changed: want %+v, got %+v", *want.Result, *got.Result)
	}
}

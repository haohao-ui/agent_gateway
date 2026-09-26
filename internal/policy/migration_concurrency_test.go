package policy

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

// Simulate an opener whose pre-lock schema read predates another opener's
// commit. The duplicate DDL must be skipped based on the locked version.
func TestMigrationRechecksVersionAfterTakingLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.db")
	st := openStoreAt(t, path)
	step := schemaMigrations[0]
	if err := applyMigration(context.Background(), st.db, step, step.version); err != nil {
		t.Fatalf("stale migration decision: %v", err)
	}
	if got := migrationCount(t, st.db); got != len(schemaMigrations) {
		t.Fatalf("migration count = %d", got)
	}
	// A stale old build must not quietly accept a newer schema either.
	if err := applyMigration(context.Background(), st.db, step, 0); err == nil {
		t.Fatal("stale opener accepted unsupported schema")
	}
}

func TestConcurrentInitialOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.db")
	const count = 32
	start := make(chan struct{})
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			st, err := Open(path)
			if err == nil {
				err = st.Close()
			}
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Open: %v", err)
		}
	}
	db := rawOpen(t, path)
	if got := migrationCount(t, db); got != len(schemaMigrations) {
		t.Fatalf("migration count = %d", got)
	}
}

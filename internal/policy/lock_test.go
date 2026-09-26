package policy

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestAWriteWaitsForTheLockInsteadOfFailing holds the write lock from another
// connection and requires a mutation issued while it is held to wait, then
// succeed. A store that gave up on the first lock conflict would turn ordinary
// contention — a second process, or a pool of concurrent callers — into failed
// requests.
func TestAWriteWaitsForTheLockInsteadOfFailing(t *testing.T) {
	st := newFrozenStore(t)
	release := takeWriteLock(t, st)

	var (
		wg    sync.WaitGroup
		p     Principal
		token string
		err   error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		p, token, err = st.Issue(context.Background(), Operator, []string{testNode}, testTime.Add(time.Hour))
	}()
	// The call must be inside its write attempt before the lock is released,
	// or this would be an ordinary uncontended write. The hold lasts longer
	// than the statement-level busy timeout, so the wait cannot be a single
	// attempt inside the driver: it has to be retried.
	waitForConnectionsInUse(t, st, 2)
	time.Sleep(time.Duration(busyTimeoutMS)*time.Millisecond*3/2 + 50*time.Millisecond)
	release()
	wg.Wait()

	if err != nil {
		t.Fatalf("Issue while another connection held the write lock: %v", err)
	}
	if got := mustAuthenticate(t, st, token); got.ID != p.ID {
		t.Fatalf("authenticated %s, want %s", got.ID, p.ID)
	}
	if n := countPrincipals(t, st); n != 1 {
		t.Fatalf("stored principals = %d, want 1", n)
	}
}

// TestAWriteWaitingForTheLockHonoursItsDeadline is the other side of the same
// coin: a caller that gives up must get its context error back rather than being
// held for the whole lock budget. That is why the wait is a loop in Go with the
// context checked between attempts, and not SQLite's busy handler, which cannot
// be interrupted.
func TestAWriteWaitingForTheLockHonoursItsDeadline(t *testing.T) {
	st := newFrozenStore(t)
	release := takeWriteLock(t, st)
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := st.Issue(ctx, Operator, []string{testNode}, testTime.Add(time.Hour))
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Issue with an expiring deadline = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > lockWaitTimeout/2 {
		t.Fatalf("the caller waited %s, want the 100ms deadline to end the wait well before %s", elapsed, lockWaitTimeout)
	}
	if n := countPrincipals(t, st); n != 0 {
		t.Fatalf("the abandoned Issue stored %d principals", n)
	}
}

// TestDriverLockErrorsAreRecognisedAsRetryable pins the classification the
// retry loop depends on: the retry happens only when a failure can be told
// apart from a real one. If the driver's lock code were not recognised, a
// contended write would fail with "database is locked" instead of waiting;
// if every driver failure looked like a lock, a genuine constraint violation
// would be retried until the lock budget ran out and then reported as a
// timeout.
func TestDriverLockErrorsAreRecognisedAsRetryable(t *testing.T) {
	ctx := context.Background()
	st := newFrozenStore(t)
	release := takeWriteLock(t, st)

	// A write from the connection that already holds the lock is refused with
	// the driver's lock code. (A second *connection* in this process blocks
	// inside the driver until the lock is free instead, so this is the shape
	// that reaches the classifier; another process reaches it the same way.)
	const probe = `INSERT INTO schema_migrations (version, name, applied_at) VALUES (999, 'probe', 'far')`
	if _, err := st.db.ExecContext(ctx, probe); err == nil {
		t.Fatal("a write succeeded while this connection held the write lock")
	} else if !isLockError(err) {
		t.Fatalf("the driver's lock error is not classified as one: %v (%T)", err, err)
	}
	release()

	// The same statement now succeeds, and repeating it fails with a
	// constraint violation: a real driver error that must not be read as a
	// lock.
	if _, err := st.db.ExecContext(ctx, probe); err != nil {
		t.Fatalf("the probe insert after releasing the lock: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, probe); err == nil {
		t.Fatal("the duplicate insert succeeded")
	} else if isLockError(err) {
		t.Fatalf("a constraint violation is classified as a lock error: %v", err)
	}

	// Errors that are not driver errors at all are not locks either.
	for _, err := range []error{errors.New("boom"), sql.ErrNoRows, context.Canceled} {
		if isLockError(err) {
			t.Fatalf("%v is classified as a lock error", err)
		}
	}
}

// TestSleepCtxStopsWithTheContext covers the helper the retry loop waits in.
func TestSleepCtxStopsWithTheContext(t *testing.T) {
	if err := sleepCtx(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("sleepCtx with a live context: %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := sleepCtx(cancelled, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("sleepCtx with a cancelled context = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("sleepCtx with a cancelled context slept for %s, want it to return at once", elapsed)
	}
}

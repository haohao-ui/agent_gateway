package taskstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"agent-gateway/internal/protocol"
)

func TestClaimReturnsOldestQueuedTaskForNode(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	first := mustSubmit(t, st, submitInput())
	second := mustSubmit(t, st, submitInput())
	other := submitInput()
	other.NodeID = "node-2"
	third := mustSubmit(t, st, other)

	lease := mustClaim(t, st, testNode, time.Minute)
	if lease.Task.ID != first.ID {
		t.Fatalf("claimed %s, want the oldest queued task %s", lease.Task.ID, first.ID)
	}
	if lease.Task.State != protocol.Leased {
		t.Fatalf("claimed task state = %s, want leased", lease.Task.State)
	}
	if lease.Task.AttemptID == "" {
		t.Fatal("claim did not create an attempt")
	}
	if lease.Task.LeaseExpiresAt == nil {
		t.Fatal("claim did not set a lease expiry")
	}
	if lease.Token == "" {
		t.Fatal("claim returned an empty token")
	}

	next := mustClaim(t, st, testNode, time.Minute)
	if next.Task.ID != second.ID {
		t.Fatalf("second claim got %s, want %s", next.Task.ID, second.ID)
	}

	empty, err := st.Claim(ctx, testNode, time.Minute)
	if err != nil {
		t.Fatalf("Claim on an empty queue: %v", err)
	}
	if empty != nil {
		t.Fatalf("Claim on an empty queue returned %+v, want nil", empty)
	}

	// Another node's queue is untouched by the claims above.
	otherLease := mustClaim(t, st, "node-2", time.Minute)
	if otherLease.Task.ID != third.ID {
		t.Fatalf("node-2 claimed %s, want %s", otherLease.Task.ID, third.ID)
	}
}

func TestClaimValidatesArguments(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	for _, tc := range []struct {
		name     string
		nodeID   string
		leaseFor time.Duration
	}{
		{"empty node", "", time.Minute},
		{"control character in node", "node\n1", time.Minute},
		{"zero lease", testNode, 0},
		{"negative lease", testNode, -time.Second},
		{"lease above the maximum", testNode, maxLease + time.Nanosecond},
	} {
		if _, err := st.Claim(ctx, tc.nodeID, tc.leaseFor); !errors.Is(err, protocol.ErrInvalid) {
			t.Errorf("Claim(%s) = %v, want protocol.ErrInvalid", tc.name, err)
		}
	}
	mustSubmit(t, st, submitInput())
	if _, err := st.Claim(ctx, testNode, maxLease); err != nil {
		t.Fatalf("Claim with the maximum lease: %v", err)
	}
}

func TestClaimStoresOnlyTokenHash(t *testing.T) {
	st := newStore(t)
	mustSubmit(t, st, submitInput())
	lease := mustClaim(t, st, testNode, time.Minute)

	var stored string
	if err := st.db.QueryRowContext(context.Background(),
		`SELECT token_hash FROM attempts WHERE id = ?`, lease.Task.AttemptID).Scan(&stored); err != nil {
		t.Fatalf("read the stored hash: %v", err)
	}
	if stored == lease.Token {
		t.Fatal("the lease token was stored in plaintext")
	}
	if want := tokenHash(lease.Token); stored != want {
		t.Fatalf("stored hash = %s, want the sha256 of the token %s", stored, want)
	}
	if len(stored) != 64 {
		t.Fatalf("stored hash is %d characters, want 64 hex characters", len(stored))
	}
}

func TestStartRejectsBadCredentials(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	mustSubmit(t, st, submitInput())
	mustSubmit(t, st, submitInput())
	first := mustClaim(t, st, testNode, time.Minute)
	second := mustClaim(t, st, testNode, time.Minute)

	if err := st.Start(ctx, "task_missing", first.Task.AttemptID, first.Token); !errors.Is(err, protocol.ErrNotFound) {
		t.Errorf("Start on an unknown task = %v, want protocol.ErrNotFound", err)
	}
	if err := st.Start(ctx, first.Task.ID, "att_missing", first.Token); !errors.Is(err, protocol.ErrUnauthorized) {
		t.Errorf("Start with an unknown attempt = %v, want protocol.ErrUnauthorized", err)
	}
	if err := st.Start(ctx, first.Task.ID, first.Task.AttemptID, ""); !errors.Is(err, protocol.ErrInvalid) {
		t.Errorf("Start with an empty token = %v, want protocol.ErrInvalid", err)
	}
	if err := st.Start(ctx, first.Task.ID, first.Task.AttemptID, second.Token); !errors.Is(err, protocol.ErrUnauthorized) {
		t.Errorf("Start with another task's token = %v, want protocol.ErrUnauthorized", err)
	}
	if err := st.Start(ctx, first.Task.ID, second.Task.AttemptID, second.Token); !errors.Is(err, protocol.ErrUnauthorized) {
		t.Errorf("Start with another task's attempt = %v, want protocol.ErrUnauthorized", err)
	}

	// No rejected call may have moved the task.
	task, err := st.Get(ctx, first.Task.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if task.State != protocol.Leased {
		t.Fatalf("state = %s after rejected Starts, want leased", task.State)
	}
	// The real credentials still work.
	if err := st.Start(ctx, first.Task.ID, first.Task.AttemptID, first.Token); err != nil {
		t.Fatalf("Start with the right credentials: %v", err)
	}
}

func TestStartOnlyFromLeased(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	mustSubmit(t, st, submitInput())
	lease := mustClaim(t, st, testNode, time.Minute)
	if err := st.Start(ctx, lease.Task.ID, lease.Task.AttemptID, lease.Token); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := st.Start(ctx, lease.Task.ID, lease.Task.AttemptID, lease.Token); !errors.Is(err, protocol.ErrConflict) {
		t.Fatalf("second Start = %v, want protocol.ErrConflict", err)
	}
}

// TestStartRejectsSupersededAttempt builds the state a later milestone
// produces when a task is re-leased after an unknown outcome: the task points
// at a newer attempt, and the older attempt's genuine credentials must be
// refused instead of writing to the task again.
func TestStartRejectsSupersededAttempt(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	mustSubmit(t, st, submitInput())
	old := mustClaim(t, st, testNode, time.Minute)

	superseded := "att_superseded"
	now := time.Now().UTC()
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO attempts (id, task_id, node_id, token_hash, state, lease_expires_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, 'leased', ?, ?, ?)`,
		superseded, old.Task.ID, testNode, tokenHash("newer-token"),
		formatTime(now.Add(time.Minute)), formatTime(now), formatTime(now)); err != nil {
		t.Fatalf("insert the newer attempt: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE tasks SET attempt_id = ? WHERE id = ?`, superseded, old.Task.ID); err != nil {
		t.Fatalf("point the task at the newer attempt: %v", err)
	}

	if err := st.Start(ctx, old.Task.ID, old.Task.AttemptID, old.Token); !errors.Is(err, protocol.ErrConflict) {
		t.Fatalf("Start with superseded credentials = %v, want protocol.ErrConflict", err)
	}
}

func TestStartRejectsExpiredLease(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	freezeClock(st, time.Now().UTC())
	mustSubmit(t, st, submitInput())
	lease := mustClaim(t, st, testNode, time.Minute)

	advance(st, 2*time.Minute)
	if err := st.Start(ctx, lease.Task.ID, lease.Task.AttemptID, lease.Token); !errors.Is(err, protocol.ErrLeaseExpired) {
		t.Fatalf("Start after the lease expired = %v, want protocol.ErrLeaseExpired", err)
	}
}

func TestStartRejectedAfterCancelRequested(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	mustSubmit(t, st, submitInput())
	lease := mustClaim(t, st, testNode, time.Minute)
	if _, err := st.Cancel(ctx, lease.Task.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if err := st.Start(ctx, lease.Task.ID, lease.Task.AttemptID, lease.Token); !errors.Is(err, protocol.ErrConflict) {
		t.Fatalf("Start on a cancel_requested task = %v, want protocol.ErrConflict", err)
	}
	// The node can still report the cancellation.
	cancelled := protocol.Result{State: protocol.Cancelled, ExitCode: -1, ErrorCode: "cancelled"}
	if err := st.Complete(ctx, lease.Task.ID, lease.Task.AttemptID, lease.Token, cancelled); err != nil {
		t.Fatalf("Complete(cancelled) after a cancellation request: %v", err)
	}
}

func TestRenewExtendsLease(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	freezeClock(st, time.Now().UTC())
	task, lease := claimStart(t, st)

	before, err := st.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	advance(st, 30*time.Second)
	if err := st.Renew(ctx, task.ID, lease.Task.AttemptID, lease.Token, 2*time.Minute); err != nil {
		t.Fatalf("Renew: %v", err)
	}
	after, err := st.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("Get after Renew: %v", err)
	}
	if after.State != protocol.Running {
		t.Fatalf("state = %s after Renew, want running", after.State)
	}
	if before.LeaseExpiresAt == nil || after.LeaseExpiresAt == nil {
		t.Fatal("lease expiry missing before or after Renew")
	}
	if !after.LeaseExpiresAt.After(*before.LeaseExpiresAt) {
		t.Fatalf("lease expiry did not move forward: %s -> %s", before.LeaseExpiresAt, after.LeaseExpiresAt)
	}
	if !after.UpdatedAt.After(before.UpdatedAt) {
		t.Fatalf("updated_at did not move forward: %s -> %s", before.UpdatedAt, after.UpdatedAt)
	}
}

func TestRenewRejectedAfterExpiry(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	freezeClock(st, time.Now().UTC())
	task, lease := claimStart(t, st)

	advance(st, 90*time.Second)
	if err := st.Renew(ctx, task.ID, lease.Task.AttemptID, lease.Token, time.Minute); !errors.Is(err, protocol.ErrLeaseExpired) {
		t.Fatalf("Renew after the lease expired = %v, want protocol.ErrLeaseExpired", err)
	}
}

func TestRenewKeepsCancelRequestedReportable(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	freezeClock(st, time.Now().UTC())
	task, lease := claimStart(t, st)
	if _, err := st.Cancel(ctx, task.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	advance(st, 30*time.Second)
	if err := st.Renew(ctx, task.ID, lease.Task.AttemptID, lease.Token, time.Minute); err != nil {
		t.Fatalf("Renew on a cancel_requested task: %v", err)
	}
	got, err := st.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != protocol.CancelRequested {
		t.Fatalf("state = %s, want cancel_requested", got.State)
	}
}

func TestRenewRejectsTerminalTask(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	task, lease := claimStart(t, st)
	if err := st.Complete(ctx, task.ID, lease.Task.AttemptID, lease.Token, baseResult()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if err := st.Renew(ctx, task.ID, lease.Task.AttemptID, lease.Token, time.Minute); !errors.Is(err, protocol.ErrConflict) {
		t.Fatalf("Renew on a finished task = %v, want protocol.ErrConflict", err)
	}
	if err := st.Renew(ctx, task.ID, lease.Task.AttemptID, "wrong-token", time.Minute); !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("Renew with a wrong token = %v, want protocol.ErrUnauthorized", err)
	}
}

func TestRenewValidatesLeaseDuration(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	task, lease := claimStart(t, st)
	for _, leaseFor := range []time.Duration{0, -time.Second, maxLease + time.Nanosecond} {
		if err := st.Renew(ctx, task.ID, lease.Task.AttemptID, lease.Token, leaseFor); !errors.Is(err, protocol.ErrInvalid) {
			t.Errorf("Renew(lease %s) = %v, want protocol.ErrInvalid", leaseFor, err)
		}
	}
}

// TestClaimFailsFastWhileWriteLockHeld is the regression test for the wait on
// the write lock: another connection holds it, so the claim must end when the
// caller's context ends. SQLite's busy handler cannot be interrupted, so a
// claim that waited inside SQLite would sit for the whole lock budget and then
// report SQLITE_BUSY instead of the caller's deadline.
func TestClaimFailsFastWhileWriteLockHeld(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	mustSubmit(t, st, submitInput())

	holder, err := st.db.Conn(ctx)
	if err != nil {
		t.Fatalf("reserve a connection: %v", err)
	}
	defer func() { _ = holder.Close() }()
	if _, err := holder.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("take the write lock: %v", err)
	}
	held := true
	defer func() {
		if held {
			_, _ = holder.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()

	claimCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	lease, err := st.Claim(claimCtx, testNode, time.Minute)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("Claim returned %+v while another writer held the lock", lease)
	}
	t.Logf("Claim failed after %s: %v", elapsed, err)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Claim = %v, want the caller's context deadline", err)
	}
	// The wait ends at the 300ms deadline plus at most one SQLite busy
	// interval; a claim that waited inside SQLite instead would sit here for
	// the whole lock budget and report SQLITE_BUSY.
	if elapsed > time.Second {
		t.Fatalf("Claim waited %s: the context must end the wait, not the lock budget of %s", elapsed, lockWaitTimeout)
	}

	if _, err := holder.ExecContext(context.Background(), `ROLLBACK`); err != nil {
		t.Fatalf("release the write lock: %v", err)
	}
	held = false
	// The refused attempt must not poison the pool.
	lease = mustClaim(t, st, testNode, time.Minute)
	if lease.Task.ID == "" {
		t.Fatal("claim after the lock was released returned no task")
	}
}

// TestClaimWaitsForWriteLockAndSucceeds is the other half of the same
// behaviour: a writer that only has to wait its turn must get the lock once it
// is released, rather than failing the moment it is contended.
func TestClaimWaitsForWriteLockAndSucceeds(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	task := mustSubmit(t, st, submitInput())

	holder, err := st.db.Conn(ctx)
	if err != nil {
		t.Fatalf("reserve a connection: %v", err)
	}
	defer func() { _ = holder.Close() }()
	if _, err := holder.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("take the write lock: %v", err)
	}

	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(100 * time.Millisecond)
		if _, err := holder.ExecContext(context.Background(), `ROLLBACK`); err != nil {
			t.Errorf("release the write lock: %v", err)
		}
	}()

	start := time.Now()
	lease, err := st.Claim(ctx, testNode, time.Minute)
	elapsed := time.Since(start)
	<-released
	if err != nil {
		t.Fatalf("Claim while the lock was held briefly: %v", err)
	}
	if lease == nil || lease.Task.ID != task.ID {
		t.Fatalf("Claim returned %+v, want task %s", lease, task.ID)
	}
	if elapsed < 50*time.Millisecond {
		t.Fatalf("Claim returned after %s, before the lock could have been released", elapsed)
	}
}

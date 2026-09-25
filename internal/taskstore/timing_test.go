package taskstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"agent-gateway/internal/protocol"
)

// The tests here cover one rule: a mutation judges and stamps time at the
// instant it holds the write lock, never at the instant the caller arrived.
// runWhileLockHeld makes the difference observable by moving the clock only
// once the lock is released, so a timestamp read before the wait is a different
// value from one read after it.

func TestClaimLeaseIsMeasuredFromLockAcquisition(t *testing.T) {
	st := newStore(t)
	base := time.Now().UTC()
	late := base.Add(2 * time.Minute)
	freezeClock(st, base)
	mustSubmit(t, st, submitInput())

	var lease *protocol.Lease
	var claimErr error
	runWhileLockHeld(t, st, base, late, func() {
		lease, claimErr = st.Claim(context.Background(), testNode, time.Minute)
	})
	if claimErr != nil {
		t.Fatalf("Claim: %v", claimErr)
	}
	if lease == nil {
		t.Fatal("Claim returned no task")
	}
	if lease.Task.LeaseExpiresAt == nil {
		t.Fatal("claim did not set a lease expiry")
	}
	// The full lease runs from acquisition, not from the moment the caller
	// asked: a lease stamped with the arrival time would arrive partly spent.
	if want := late.Add(time.Minute); !lease.Task.LeaseExpiresAt.Equal(want) {
		t.Fatalf("lease expires at %s, want %s (acquisition + lease duration)", lease.Task.LeaseExpiresAt, want)
	}
	if !lease.Task.UpdatedAt.Equal(late) {
		t.Fatalf("claim stamped updated_at %s, want %s", lease.Task.UpdatedAt, late)
	}
	if !lease.Task.LeaseExpiresAt.After(late) {
		t.Fatal("the issued lease is already expired at the moment it was granted")
	}
}

func TestMutationsRejectedWhenLeaseExpiresDuringLockWait(t *testing.T) {
	base := time.Now().UTC()
	late := base.Add(2 * time.Minute)

	leased := func(t *testing.T, st *Store) (protocol.Task, *protocol.Lease) {
		t.Helper()
		task := mustSubmit(t, st, submitInput())
		lease := mustClaim(t, st, testNode, time.Minute)
		return task, lease
	}
	running := func(t *testing.T, st *Store) (protocol.Task, *protocol.Lease) {
		t.Helper()
		return claimStart(t, st)
	}

	cases := []struct {
		name    string
		prepare func(*testing.T, *Store) (protocol.Task, *protocol.Lease)
		mutate  func(*Store, protocol.Task, *protocol.Lease) error
	}{
		{
			name:    "Start",
			prepare: leased,
			mutate: func(st *Store, task protocol.Task, lease *protocol.Lease) error {
				return st.Start(context.Background(), task.ID, lease.Task.AttemptID, lease.Token)
			},
		},
		{
			name:    "Renew",
			prepare: running,
			mutate: func(st *Store, task protocol.Task, lease *protocol.Lease) error {
				return st.Renew(context.Background(), task.ID, lease.Task.AttemptID, lease.Token, time.Minute)
			},
		},
		{
			name:    "Complete",
			prepare: running,
			mutate: func(st *Store, task protocol.Task, lease *protocol.Lease) error {
				return st.Complete(context.Background(), task.ID, lease.Task.AttemptID, lease.Token, baseResult())
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t)
			freezeClock(st, base)
			task, lease := tc.prepare(t, st)
			before := mustGet(t, st, task.ID)

			var err error
			runWhileLockHeld(t, st, base, late, func() {
				err = tc.mutate(st, task, lease)
			})
			if !errors.Is(err, protocol.ErrLeaseExpired) {
				t.Fatalf("%s = %v, want protocol.ErrLeaseExpired: the lease lapsed while the caller waited for the write lock", tc.name, err)
			}
			// The refused call must not have changed the task.
			if got := mustGet(t, st, task.ID); got.State != before.State {
				t.Fatalf("state = %s after a refused %s, want %s", got.State, tc.name, before.State)
			}
		})
	}
}

// TestCompleteRetransmissionSurvivesExpiryDuringLockWait pins the exception to
// the rule above: a terminal result may be retransmitted after the lease has
// lapsed, including while a lock wait crossed the expiry. Losing an
// acknowledgement must not cost a node its result.
func TestCompleteRetransmissionSurvivesExpiryDuringLockWait(t *testing.T) {
	st := newStore(t)
	base := time.Now().UTC()
	freezeClock(st, base)
	task, lease := claimStart(t, st)
	result := baseResult()
	if err := st.Complete(context.Background(), task.ID, lease.Task.AttemptID, lease.Token, result); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	var replayErr error
	runWhileLockHeld(t, st, base, base.Add(2*time.Minute), func() {
		replayErr = st.Complete(context.Background(), task.ID, lease.Task.AttemptID, lease.Token, result)
	})
	if replayErr != nil {
		t.Fatalf("retransmitted Complete = %v, want nil", replayErr)
	}

	// A different result is still a conflict, not a way to amend an outcome.
	changed := result
	changed.Text = "different"
	var conflictErr error
	runWhileLockHeld(t, st, base, base.Add(2*time.Minute), func() {
		conflictErr = st.Complete(context.Background(), task.ID, lease.Task.AttemptID, lease.Token, changed)
	})
	if !errors.Is(conflictErr, protocol.ErrConflict) {
		t.Fatalf("Complete with a changed result = %v, want protocol.ErrConflict", conflictErr)
	}
}

// TestSubmitAndCancelTimestampsFollowLockAcquisition audits the two mutations
// that only stamp rows: their timestamps come from the write lock as well.
func TestSubmitAndCancelTimestampsFollowLockAcquisition(t *testing.T) {
	st := newStore(t)
	base := time.Now().UTC()
	late := base.Add(2 * time.Minute)
	freezeClock(st, base)

	var submitted protocol.Task
	var submitErr error
	runWhileLockHeld(t, st, base, late, func() {
		submitted, submitErr = st.Submit(context.Background(), submitInput())
	})
	if submitErr != nil {
		t.Fatalf("Submit: %v", submitErr)
	}
	if !submitted.CreatedAt.Equal(late) || !submitted.UpdatedAt.Equal(late) {
		t.Fatalf("Submit stamped %s / %s, want %s", submitted.CreatedAt, submitted.UpdatedAt, late)
	}

	var cancelled protocol.Task
	var cancelErr error
	runWhileLockHeld(t, st, base, late, func() {
		cancelled, cancelErr = st.Cancel(context.Background(), submitted.ID)
	})
	if cancelErr != nil {
		t.Fatalf("Cancel: %v", cancelErr)
	}
	if cancelled.State != protocol.Cancelled {
		t.Fatalf("state = %s, want cancelled", cancelled.State)
	}
	if !cancelled.UpdatedAt.Equal(late) {
		t.Fatalf("Cancel stamped updated_at %s, want %s", cancelled.UpdatedAt, late)
	}
}

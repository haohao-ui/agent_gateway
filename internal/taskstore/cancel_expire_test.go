package taskstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"agent-gateway/internal/protocol"
)

func TestCancelQueuedTaskIsImmediate(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	task := mustSubmit(t, st, submitInput())

	got, err := st.Cancel(ctx, task.ID)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if got.State != protocol.Cancelled {
		t.Fatalf("state = %s, want cancelled", got.State)
	}
	if got.Result != nil {
		t.Fatalf("a task cancelled before dispatch recorded a result: %+v", got.Result)
	}
	if got.AttemptID != "" {
		t.Fatalf("a task cancelled before dispatch has an attempt: %q", got.AttemptID)
	}

	// Cancelling twice is not an error and does not move the task.
	again, err := st.Cancel(ctx, task.ID)
	if err != nil {
		t.Fatalf("second Cancel: %v", err)
	}
	if again.State != protocol.Cancelled {
		t.Fatalf("state after a repeated Cancel = %s, want cancelled", again.State)
	}

	// A cancelled task is never claimed.
	lease, err := st.Claim(ctx, testNode, time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if lease != nil {
		t.Fatalf("Claim returned the cancelled task %s", lease.Task.ID)
	}
}

func TestCancelRequestsCancellationOfLiveTask(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	task, lease := claimStart(t, st)

	running, err := st.Cancel(ctx, task.ID)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if running.State != protocol.CancelRequested {
		t.Fatalf("state = %s, want cancel_requested", running.State)
	}
	if running.AttemptID != lease.Task.AttemptID {
		t.Fatal("a cancellation request dropped the attempt reference")
	}

	// A repeat call is idempotent, and the node can still renew the lease so
	// that it can report the cancellation it performed.
	if again, err := st.Cancel(ctx, task.ID); err != nil {
		t.Fatalf("second Cancel: %v", err)
	} else if again.State != protocol.CancelRequested {
		t.Fatalf("state = %s, want cancel_requested", again.State)
	}
	if err := st.Renew(ctx, task.ID, lease.Task.AttemptID, lease.Token, time.Minute); err != nil {
		t.Fatalf("Renew while a cancellation is pending: %v", err)
	}
}

func TestCancelTerminalTaskReportsCurrentState(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	task, lease := claimStart(t, st)
	failed := protocol.Result{State: protocol.Failed, ExitCode: 1, ErrorCode: "exit-status"}
	if err := st.Complete(ctx, task.ID, lease.Task.AttemptID, lease.Token, failed); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	got, err := st.Cancel(ctx, task.ID)
	if err != nil {
		t.Fatalf("Cancel on a finished task: %v", err)
	}
	if got.State != protocol.Failed {
		t.Fatalf("state = %s, want failed", got.State)
	}
	if got.Result == nil || !sameResult(*got.Result, failed) {
		t.Fatalf("Cancel changed the recorded result: %+v", got.Result)
	}
}

func TestCancelUnknownTaskIsNotFound(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	if _, err := st.Cancel(ctx, "task_missing"); !errors.Is(err, protocol.ErrNotFound) {
		t.Fatalf("Cancel(unknown) = %v, want protocol.ErrNotFound", err)
	}
	if _, err := st.Cancel(ctx, ""); !errors.Is(err, protocol.ErrInvalid) {
		t.Fatalf("Cancel(empty) = %v, want protocol.ErrInvalid", err)
	}
}

func TestExpireMovesLapsedLeasesToUnknown(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	base := time.Now().UTC()
	freezeClock(st, base)

	leasedTask := mustSubmit(t, st, submitInput())
	leased := mustClaim(t, st, testNode, time.Minute)

	runningTask := mustSubmit(t, st, submitInput())
	running := mustClaim(t, st, testNode, time.Minute)
	if err := st.Start(ctx, runningTask.ID, running.Task.AttemptID, running.Token); err != nil {
		t.Fatalf("Start: %v", err)
	}

	pendingTask := mustSubmit(t, st, submitInput())
	pending := mustClaim(t, st, testNode, time.Minute)
	if err := st.Start(ctx, pendingTask.ID, pending.Task.AttemptID, pending.Token); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := st.Cancel(ctx, pendingTask.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	finishedTask := mustSubmit(t, st, submitInput())
	finished := mustClaim(t, st, testNode, time.Minute)
	if err := st.Start(ctx, finishedTask.ID, finished.Task.AttemptID, finished.Token); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := st.Complete(ctx, finishedTask.ID, finished.Task.AttemptID, finished.Token, baseResult()); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Submitted last, so it is still queued when the leases lapse.
	queuedTask := mustSubmit(t, st, submitInput())

	moved, err := st.Expire(ctx, base.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if moved != 3 {
		t.Fatalf("Expire moved %d tasks, want 3", moved)
	}

	for _, tc := range []struct {
		state string
		task  protocol.Task
		lease *protocol.Lease
	}{
		{"leased", leasedTask, leased},
		{"running", runningTask, running},
		{"cancel_requested", pendingTask, pending},
	} {
		got := mustGet(t, st, tc.task.ID)
		if got.State != protocol.Unknown {
			t.Errorf("task that was %s is %s after Expire, want unknown", tc.state, got.State)
		}
		if got.LeaseExpiresAt != nil {
			t.Errorf("task that was %s kept a lease expiry after Expire", tc.state)
		}
		if got.AttemptID != tc.lease.Task.AttemptID {
			t.Errorf("task that was %s lost its attempt reference: %q", tc.state, got.AttemptID)
		}
	}
	if got := mustGet(t, st, queuedTask.ID); got.State != protocol.Queued {
		t.Errorf("queued task state = %s after Expire, want queued", got.State)
	}
	if got := mustGet(t, st, finishedTask.ID); got.State != protocol.Succeeded || got.Result == nil {
		t.Errorf("finished task changed: %+v", got)
	}

	// Expire is idempotent, and an unknown task is never handed out again.
	again, err := st.Expire(ctx, base.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("second Expire: %v", err)
	}
	if again != 0 {
		t.Fatalf("second Expire moved %d tasks, want 0", again)
	}
	lease, err := st.Claim(ctx, testNode, time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if lease == nil || lease.Task.ID != queuedTask.ID {
		t.Fatalf("Claim returned %+v, want the queued task %s", lease, queuedTask.ID)
	}
}

func TestExpireKeepsLiveLeases(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	base := time.Now().UTC()
	freezeClock(st, base)

	shortTask := mustSubmit(t, st, submitInput())
	mustClaim(t, st, testNode, 30*time.Second)
	longTask := mustSubmit(t, st, submitInput())
	mustClaim(t, st, testNode, 5*time.Minute)

	moved, err := st.Expire(ctx, base.Add(time.Minute))
	if err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if moved != 1 {
		t.Fatalf("Expire moved %d tasks, want 1", moved)
	}
	if got := mustGet(t, st, shortTask.ID); got.State != protocol.Unknown {
		t.Errorf("task with the lapsed lease is %s, want unknown", got.State)
	}
	if got := mustGet(t, st, longTask.ID); got.State != protocol.Leased {
		t.Errorf("task with the live lease is %s, want leased", got.State)
	}
}

// TestExpireOrdersSubSecondLeases guards the stored time format: lease expiry
// is compared as text, so a layout whose text order disagreed with the time
// order would expire the wrong task here.
func TestExpireOrdersSubSecondLeases(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	base := time.Now().UTC()
	freezeClock(st, base)

	earlyTask := mustSubmit(t, st, submitInput())
	mustClaim(t, st, testNode, time.Second)
	advance(st, time.Microsecond)
	lateTask := mustSubmit(t, st, submitInput())
	mustClaim(t, st, testNode, time.Second)

	// The first lease ends at base+1s, the second one microsecond later.
	moved, err := st.Expire(ctx, base.Add(time.Second))
	if err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if moved != 1 {
		t.Fatalf("Expire moved %d tasks, want exactly the lease that ended at the cutoff", moved)
	}
	if got := mustGet(t, st, earlyTask.ID); got.State != protocol.Unknown {
		t.Errorf("task whose lease ended at the cutoff is %s, want unknown", got.State)
	}
	if got := mustGet(t, st, lateTask.ID); got.State != protocol.Leased {
		t.Errorf("task whose lease ends after the cutoff is %s, want leased", got.State)
	}
}

func TestExpireRejectsZeroTime(t *testing.T) {
	st := newStore(t)
	if _, err := st.Expire(context.Background(), time.Time{}); !errors.Is(err, protocol.ErrInvalid) {
		t.Fatalf("Expire(zero time) = %v, want protocol.ErrInvalid", err)
	}
}

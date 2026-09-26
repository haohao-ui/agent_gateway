package taskstore

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-gateway/internal/protocol"
)

func TestCompleteUnknownTaskReconciliation(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	base := time.Now().UTC()
	freezeClock(st, base)
	task, lease := claimStart(t, st)

	// Lease expires and moves to unknown
	moved, err := st.Expire(ctx, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if moved != 1 {
		t.Fatalf("Expire moved %d tasks, want 1", moved)
	}

	// Verify task is now in unknown state
	got, err := st.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != protocol.Unknown {
		t.Fatalf("task state = %s, want unknown", got.State)
	}

	// Node re-establishes connectivity and reports completion with its attempt credentials
	expectedResult := protocol.Result{
		State:    protocol.Succeeded,
		Text:     "finished successfully after network recovery",
		ExitCode: 0,
	}
	if err := st.Complete(ctx, task.ID, lease.Task.AttemptID, lease.Token, expectedResult); err != nil {
		t.Fatalf("Complete on unknown task should succeed via reconciliation, got: %v", err)
	}

	// Check final state
	finalTask, err := st.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("Get after complete: %v", err)
	}
	if finalTask.State != protocol.Succeeded {
		t.Fatalf("final task state = %s, want succeeded", finalTask.State)
	}
	if finalTask.Result == nil || finalTask.Result.Text != expectedResult.Text {
		t.Fatalf("final task result mismatch: %+v", finalTask.Result)
	}

	// Retransmission of identical result is idempotent
	if err := st.Complete(ctx, task.ID, lease.Task.AttemptID, lease.Token, expectedResult); err != nil {
		t.Fatalf("idempotent retransmission failed: %v", err)
	}

	// Different result fails with conflict
	differentResult := protocol.Result{State: protocol.Failed, Text: "different"}
	if err := st.Complete(ctx, task.ID, lease.Task.AttemptID, lease.Token, differentResult); !errors.Is(err, protocol.ErrConflict) {
		t.Fatalf("different result should return ErrConflict, got: %v", err)
	}
}

func TestRequeueUnknownTask(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	base := time.Now().UTC()
	freezeClock(st, base)
	task, lease := claimStart(t, st)

	// Move to unknown
	if _, err := st.Expire(ctx, base.Add(time.Hour)); err != nil {
		t.Fatalf("Expire: %v", err)
	}

	// Operator requeues the task
	requeued, err := st.Requeue(ctx, task.ID)
	if err != nil {
		t.Fatalf("Requeue failed: %v", err)
	}
	if requeued.State != protocol.Queued {
		t.Fatalf("requeued state = %s, want queued", requeued.State)
	}
	if requeued.AttemptID != "" {
		t.Fatalf("requeued attempt_id should be empty, got: %q", requeued.AttemptID)
	}

	// Old node tries to complete with stale attempt credentials -> must be rejected
	oldResult := protocol.Result{State: protocol.Succeeded, Text: "stale node result"}
	err = st.Complete(ctx, task.ID, lease.Task.AttemptID, lease.Token, oldResult)
	if !errors.Is(err, protocol.ErrConflict) {
		t.Fatalf("stale node completion after requeue must return ErrConflict, got: %v", err)
	}

	// Healthy node can now claim the task again
	newLease, err := st.Claim(ctx, task.NodeID, 5*time.Minute)
	if err != nil {
		t.Fatalf("Claim after requeue failed: %v", err)
	}
	if newLease == nil {
		t.Fatalf("Claim returned nil lease after requeue")
	}
	if newLease.Task.AttemptID == lease.Task.AttemptID {
		t.Fatalf("new attempt ID must differ from old attempt ID")
	}
}

func TestResolveUnknownTask(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	base := time.Now().UTC()
	freezeClock(st, base)
	task, lease := claimStart(t, st)

	// Move to unknown
	if _, err := st.Expire(ctx, base.Add(time.Hour)); err != nil {
		t.Fatalf("Expire: %v", err)
	}

	// Operator resolves task as failed
	operatorResult := protocol.Result{
		State:     protocol.Failed,
		Text:      "operator determined node was lost permanently",
		ErrorCode: "operator_abort",
		ExitCode:  -1,
	}
	resolved, err := st.Resolve(ctx, task.ID, operatorResult)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if resolved.State != protocol.Failed {
		t.Fatalf("resolved state = %s, want failed", resolved.State)
	}
	if resolved.Result == nil || resolved.Result.ErrorCode != "operator_abort" {
		t.Fatalf("resolved result mismatch: %+v", resolved.Result)
	}

	// Old node tries to complete afterwards -> must be rejected
	err = st.Complete(ctx, task.ID, lease.Task.AttemptID, lease.Token, protocol.Result{State: protocol.Succeeded})
	if !errors.Is(err, protocol.ErrConflict) {
		t.Fatalf("completion after resolve must return ErrConflict, got: %v", err)
	}
}

func TestRequeueAndResolveRefuseNonUnknownTasks(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	task := mustSubmit(t, st, submitInput())

	// Queued task cannot be requeued or resolved
	if _, err := st.Requeue(ctx, task.ID); !errors.Is(err, protocol.ErrConflict) {
		t.Fatalf("Requeue on queued task should return ErrConflict, got: %v", err)
	}
	if _, err := st.Resolve(ctx, task.ID, protocol.Result{State: protocol.Failed}); !errors.Is(err, protocol.ErrConflict) {
		t.Fatalf("Resolve on queued task should return ErrConflict, got: %v", err)
	}
}

func TestListUnknown(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	base := time.Now().UTC()
	freezeClock(st, base)

	// Submit and claim two tasks
	task1, _ := claimStart(t, st)
	task2, _ := claimStart(t, st)

	// Expire both
	moved, err := st.Expire(ctx, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if moved != 2 {
		t.Fatalf("Expire moved %d, want 2", moved)
	}

	unknowns, err := st.ListUnknown(ctx, 10)
	if err != nil {
		t.Fatalf("ListUnknown: %v", err)
	}
	if len(unknowns) != 2 {
		t.Fatalf("ListUnknown returned %d, want 2", len(unknowns))
	}
	found := make(map[string]bool)
	for _, u := range unknowns {
		found[u.ID] = true
	}
	if !found[task1.ID] || !found[task2.ID] {
		t.Fatalf("missing tasks in ListUnknown: %v", found)
	}
}

func TestConcurrent_ReconciliationVsRequeue(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	base := time.Now().UTC()
	freezeClock(st, base)
	task, lease := claimStart(t, st)

	if _, err := st.Expire(ctx, base.Add(time.Hour)); err != nil {
		t.Fatalf("Expire: %v", err)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	var completeSuccess, requeueSuccess int32

	wg.Add(2)
	// Goroutine 1: Node tries to complete
	go func() {
		defer wg.Done()
		<-start
		res := protocol.Result{State: protocol.Succeeded, Text: "node done"}
		if err := st.Complete(ctx, task.ID, lease.Task.AttemptID, lease.Token, res); err == nil {
			atomic.AddInt32(&completeSuccess, 1)
		}
	}()

	// Goroutine 2: Operator tries to requeue
	go func() {
		defer wg.Done()
		<-start
		if _, err := st.Requeue(ctx, task.ID); err == nil {
			atomic.AddInt32(&requeueSuccess, 1)
		}
	}()

	close(start)
	wg.Wait()

	// Exactly one of the two actions can succeed; the other must fail with conflict
	cWin := atomic.LoadInt32(&completeSuccess)
	rWin := atomic.LoadInt32(&requeueSuccess)
	if cWin+rWin != 1 {
		t.Fatalf("expected exactly one winner between Complete and Requeue, got complete=%d requeue=%d", cWin, rWin)
	}
}

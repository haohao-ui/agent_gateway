package taskstore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"agent-gateway/internal/protocol"
)

func TestCompleteRunningTaskRecordsResult(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	task, lease := claimStart(t, st)
	result := protocol.Result{State: protocol.Succeeded, Text: "结果：中文输出", ExitCode: 0, Truncated: true}
	if err := st.Complete(ctx, task.ID, lease.Task.AttemptID, lease.Token, result); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	got, err := st.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != protocol.Succeeded {
		t.Fatalf("state = %s, want succeeded", got.State)
	}
	if got.Result == nil {
		t.Fatal("a finished task has no result")
	}
	if !sameResult(*got.Result, result) {
		t.Fatalf("stored result %+v, want %+v", *got.Result, result)
	}
	if got.AttemptID != lease.Task.AttemptID {
		t.Fatalf("attempt_id = %q, want %q", got.AttemptID, lease.Task.AttemptID)
	}
	if got.LeaseExpiresAt != nil {
		t.Fatalf("a finished task still carries a lease expiry: %s", got.LeaseExpiresAt)
	}
}

func TestCompleteRequiresStart(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	mustSubmit(t, st, submitInput())
	lease := mustClaim(t, st, testNode, time.Minute)

	if err := st.Complete(ctx, lease.Task.ID, lease.Task.AttemptID, lease.Token, baseResult()); !errors.Is(err, protocol.ErrConflict) {
		t.Fatalf("Complete on a leased task = %v, want protocol.ErrConflict", err)
	}
	got, err := st.Get(ctx, lease.Task.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != protocol.Leased || got.Result != nil {
		t.Fatalf("a refused completion changed the task: %+v", got)
	}
}

func TestCompleteRejectsNonTerminalResultState(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	task, lease := claimStart(t, st)
	for _, state := range []protocol.State{
		protocol.Queued, protocol.Leased, protocol.Running,
		protocol.CancelRequested, protocol.Unknown, protocol.State("finished"), "",
	} {
		result := baseResult()
		result.State = state
		if err := st.Complete(ctx, task.ID, lease.Task.AttemptID, lease.Token, result); !errors.Is(err, protocol.ErrInvalid) {
			t.Errorf("Complete with result state %q = %v, want protocol.ErrInvalid", state, err)
		}
	}
	got, err := st.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != protocol.Running || got.Result != nil {
		t.Fatalf("a refused completion changed the task: %+v", got)
	}
}

func TestCompleteValidatesResultLimits(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	task, lease := claimStart(t, st)

	atLimit := baseResult()
	atLimit.Text = strings.Repeat("x", maxResultBytes)
	if err := st.Complete(ctx, task.ID, lease.Task.AttemptID, lease.Token, atLimit); err != nil {
		t.Fatalf("Complete with a result of exactly the limit: %v", err)
	}

	// The task is finished now, so the remaining cases use a fresh one.
	task, lease = claimStart(t, st)
	cases := []struct {
		name   string
		mutate func(*protocol.Result)
	}{
		{"text above the limit", func(r *protocol.Result) { r.Text = strings.Repeat("x", maxResultBytes+1) }},
		{"invalid utf-8 text", func(r *protocol.Result) { r.Text = "\xff\xfe" }},
		{"uppercase error code", func(r *protocol.Result) { r.ErrorCode = "Timeout" }},
		{"error code with a space", func(r *protocol.Result) { r.ErrorCode = "timed out" }},
		{"overlong error code", func(r *protocol.Result) { r.ErrorCode = strings.Repeat("a", maxErrorCodeLen+1) }},
	}
	for _, tc := range cases {
		result := baseResult()
		tc.mutate(&result)
		if err := st.Complete(ctx, task.ID, lease.Task.AttemptID, lease.Token, result); !errors.Is(err, protocol.ErrInvalid) {
			t.Errorf("Complete with %s = %v, want protocol.ErrInvalid", tc.name, err)
		}
	}
	// A valid multibyte result fits within the byte limit.
	chinese := baseResult()
	chinese.Text = strings.Repeat("中", maxResultBytes/3)
	if err := st.Complete(ctx, task.ID, lease.Task.AttemptID, lease.Token, chinese); err != nil {
		t.Fatalf("Complete with a multibyte result: %v", err)
	}
	got, err := st.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Result == nil || got.Result.Text != chinese.Text {
		t.Fatalf("multibyte result did not round-trip: %+v", got.Result)
	}
}

func TestCompleteRetransmissionIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	freezeClock(st, time.Now().UTC())
	task, lease := claimStart(t, st)
	result := protocol.Result{State: protocol.Failed, Text: "boom", ExitCode: 2, ErrorCode: "exit-status"}
	if err := st.Complete(ctx, task.ID, lease.Task.AttemptID, lease.Token, result); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// The acknowledgement may be lost long after the lease lapsed; an
	// identical retransmission with the recorded credentials is still valid.
	advance(st, 10*time.Minute)
	if err := st.Complete(ctx, task.ID, lease.Task.AttemptID, lease.Token, result); err != nil {
		t.Fatalf("retransmitted Complete: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*protocol.Result)
	}{
		{"text", func(r *protocol.Result) { r.Text = "different" }},
		{"state", func(r *protocol.Result) { r.State = protocol.Succeeded }},
		{"exit code", func(r *protocol.Result) { r.ExitCode = 0 }},
		{"error code", func(r *protocol.Result) { r.ErrorCode = "timeout" }},
		{"truncated", func(r *protocol.Result) { r.Truncated = true }},
	}
	for _, tc := range cases {
		changed := result
		tc.mutate(&changed)
		if err := st.Complete(ctx, task.ID, lease.Task.AttemptID, lease.Token, changed); !errors.Is(err, protocol.ErrConflict) {
			t.Errorf("Complete with a changed %s = %v, want protocol.ErrConflict", tc.name, err)
		}
	}
	got, err := st.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Result == nil || !sameResult(*got.Result, result) {
		t.Fatalf("a refused retransmission changed the recorded result: %+v", got.Result)
	}
}

func TestCompleteWithWrongTokenOnFinishedTask(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	task, lease := claimStart(t, st)
	if err := st.Complete(ctx, task.ID, lease.Task.AttemptID, lease.Token, baseResult()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// A terminal task is not a way around the credential check.
	if err := st.Complete(ctx, task.ID, lease.Task.AttemptID, "wrong-token", baseResult()); !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("Complete with a wrong token = %v, want protocol.ErrUnauthorized", err)
	}
	if err := st.Complete(ctx, task.ID, "att_missing", lease.Token, baseResult()); !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("Complete with an unknown attempt = %v, want protocol.ErrUnauthorized", err)
	}
}

func TestCompleteAfterCancelRequested(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	task, lease := claimStart(t, st)
	if _, err := st.Cancel(ctx, task.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	// The cancellation arrived first, so a success report is a conflict ...
	if err := st.Complete(ctx, task.ID, lease.Task.AttemptID, lease.Token, baseResult()); !errors.Is(err, protocol.ErrConflict) {
		t.Fatalf("Complete(succeeded) after a cancellation request = %v, want protocol.ErrConflict", err)
	}
	// ... and the node reports the cancellation it performed.
	cancelled := protocol.Result{State: protocol.Cancelled, ExitCode: -1, ErrorCode: "cancelled"}
	if err := st.Complete(ctx, task.ID, lease.Task.AttemptID, lease.Token, cancelled); err != nil {
		t.Fatalf("Complete(cancelled): %v", err)
	}
	got, err := st.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != protocol.Cancelled || got.Result == nil || !sameResult(*got.Result, cancelled) {
		t.Fatalf("task after cancellation = %+v", got)
	}
}

func TestCompleteCancelledFromRunning(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	task, lease := claimStart(t, st)
	// A node that gave up on its own reports cancelled without a Cancel call.
	cancelled := protocol.Result{State: protocol.Cancelled, ExitCode: -1, ErrorCode: "cancelled"}
	if err := st.Complete(ctx, task.ID, lease.Task.AttemptID, lease.Token, cancelled); err != nil {
		t.Fatalf("Complete(cancelled) from running: %v", err)
	}
	got, err := st.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != protocol.Cancelled {
		t.Fatalf("state = %s, want cancelled", got.State)
	}
}

func TestCompleteRejectsExpiredLease(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	freezeClock(st, time.Now().UTC())
	task, lease := claimStart(t, st)
	advance(st, 2*time.Minute)
	if err := st.Complete(ctx, task.ID, lease.Task.AttemptID, lease.Token, baseResult()); !errors.Is(err, protocol.ErrLeaseExpired) {
		t.Fatalf("Complete after the lease expired = %v, want protocol.ErrLeaseExpired", err)
	}
	got, err := st.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != protocol.Running || got.Result != nil {
		t.Fatalf("a refused completion changed the task: %+v", got)
	}
}

func TestCompleteUnknownTaskWithMismatchedCredentialIsRejected(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	base := time.Now().UTC()
	freezeClock(st, base)
	task, lease := claimStart(t, st)
	moved, err := st.Expire(ctx, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if moved != 1 {
		t.Fatalf("Expire moved %d tasks, want 1", moved)
	}
	// Wrong token is unauthorized
	if err := st.Complete(ctx, task.ID, lease.Task.AttemptID, "wrong-token-abc", baseResult()); !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("Complete with wrong token = %v, want protocol.ErrUnauthorized", err)
	}
	// Correct token reconciles successfully
	if err := st.Complete(ctx, task.ID, lease.Task.AttemptID, lease.Token, baseResult()); err != nil {
		t.Fatalf("Complete on unknown task with valid credentials failed: %v", err)
	}
	got, err := st.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != protocol.Succeeded || got.Result == nil {
		t.Fatalf("task after reconciliation = %+v", got)
	}
}

func TestCompleteOnQueuedTaskIsUnauthorized(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	task := mustSubmit(t, st, submitInput())
	if err := st.Complete(ctx, task.ID, "att_missing", "some-token", baseResult()); !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("Complete on a queued task = %v, want protocol.ErrUnauthorized", err)
	}
}

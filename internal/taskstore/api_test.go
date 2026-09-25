// Package taskstore_test exercises the exported API exactly as another package
// would: it may only use the contract in docs/CONTRACTS.md, so it fails to
// build if a signature drifts.
package taskstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"agent-gateway/internal/protocol"
	"agent-gateway/internal/taskstore"
)

func TestPublicAPILifecycle(t *testing.T) {
	ctx := context.Background()
	st, err := taskstore.Open(filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	request := protocol.SubmitRequest{
		NodeID:            "node-a",
		Capability:        "agent.run",
		CapabilityVersion: 1,
		Input:             json.RawMessage(`{"prompt":"hi"}`),
		TimeoutSeconds:    30,
		IdempotencyKey:    "k1",
	}
	submitted, err := st.Submit(ctx, request)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if submitted.State != protocol.Queued {
		t.Fatalf("state = %s, want queued", submitted.State)
	}
	replay, err := st.Submit(ctx, request)
	if err != nil {
		t.Fatalf("replayed Submit: %v", err)
	}
	if replay.ID != submitted.ID {
		t.Fatalf("replayed Submit created %s, want %s", replay.ID, submitted.ID)
	}

	// A short lease plus Expire with an explicit time keeps the expiry test
	// free of sleeps.
	lease, err := st.Claim(ctx, "node-a", 50*time.Millisecond)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if lease == nil {
		t.Fatal("Claim returned no task")
	}
	if lease.Task.ID != submitted.ID {
		t.Fatalf("claimed %s, want %s", lease.Task.ID, submitted.ID)
	}
	if lease.Token == "" {
		t.Fatal("Claim returned an empty token")
	}
	if err := st.Start(ctx, lease.Task.ID, lease.Task.AttemptID, lease.Token); err != nil {
		t.Fatalf("Start: %v", err)
	}
	result := protocol.Result{State: protocol.Succeeded, Text: "ok"}
	if err := st.Complete(ctx, lease.Task.ID, lease.Task.AttemptID, lease.Token, result); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if err := st.Complete(ctx, lease.Task.ID, lease.Task.AttemptID, lease.Token, result); err != nil {
		t.Fatalf("retransmitted Complete: %v", err)
	}
	task, err := st.Get(ctx, lease.Task.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if task.State != protocol.Succeeded || task.Result == nil || task.Result.Text != result.Text {
		t.Fatalf("finished task = %+v", task)
	}

	// A finished task is not affected by an expiry sweep, and the sweep needs
	// no sleep because the caller supplies the instant.
	moved, err := st.Expire(ctx, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if moved != 0 {
		t.Fatalf("Expire moved %d finished tasks, want 0", moved)
	}

	// Failures are classified by sentinel, never by message.
	if _, err := st.Get(ctx, "task_missing"); !errors.Is(err, protocol.ErrNotFound) {
		t.Fatalf("Get(unknown) = %v, want protocol.ErrNotFound", err)
	}
	if err := st.Start(ctx, lease.Task.ID, lease.Task.AttemptID, "wrong-token"); !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("Start with a wrong token = %v, want protocol.ErrUnauthorized", err)
	}
	if _, err := st.Claim(ctx, "node-a", 0); !errors.Is(err, protocol.ErrInvalid) {
		t.Fatalf("Claim with a zero lease = %v, want protocol.ErrInvalid", err)
	}
}

func TestPublicAPIExpiresLapsedLeaseIntoUnknown(t *testing.T) {
	ctx := context.Background()
	st, err := taskstore.Open(filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	if _, err := st.Submit(ctx, protocol.SubmitRequest{
		NodeID:            "node-a",
		Capability:        "agent.run",
		CapabilityVersion: 1,
		Input:             json.RawMessage(`{"prompt":"hi"}`),
		TimeoutSeconds:    30,
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	lease, err := st.Claim(ctx, "node-a", 50*time.Millisecond)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if lease == nil {
		t.Fatal("Claim returned no task")
	}
	if err := st.Start(ctx, lease.Task.ID, lease.Task.AttemptID, lease.Token); err != nil {
		t.Fatalf("Start: %v", err)
	}

	moved, err := st.Expire(ctx, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if moved != 1 {
		t.Fatalf("Expire moved %d tasks, want 1", moved)
	}
	task, err := st.Get(ctx, lease.Task.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if task.State != protocol.Unknown {
		t.Fatalf("state = %s after the lease lapsed, want unknown", task.State)
	}
	if task.AttemptID != lease.Task.AttemptID {
		t.Fatalf("attempt_id = %q, want the attempt that has to be reconciled %q", task.AttemptID, lease.Task.AttemptID)
	}
	if err := st.Complete(ctx, lease.Task.ID, lease.Task.AttemptID, lease.Token, protocol.Result{State: protocol.Succeeded}); !errors.Is(err, protocol.ErrConflict) {
		t.Fatalf("Complete on an unknown task = %v, want protocol.ErrConflict", err)
	}
}

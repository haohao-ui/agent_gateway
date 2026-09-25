package taskstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-gateway/internal/protocol"
)

func TestSubmitStoresCompactedInput(t *testing.T) {
	st := newStore(t)
	in := submitInput()
	in.Input = json.RawMessage("{ \"prompt\" : \"hello\" }")
	task := mustSubmit(t, st, in)

	if got, want := string(task.Input), `{"prompt":"hello"}`; got != want {
		t.Fatalf("stored input = %s, want %s", got, want)
	}
	if task.State != protocol.Queued {
		t.Fatalf("state = %s, want queued", task.State)
	}
	if task.AttemptID != "" || task.LeaseExpiresAt != nil || task.Result != nil {
		t.Fatalf("a fresh task carries an attempt, lease or result: %+v", task)
	}
	if task.CreatedAt.IsZero() || !task.CreatedAt.Equal(task.UpdatedAt) {
		t.Fatalf("created_at %s and updated_at %s, want one non-zero instant", task.CreatedAt, task.UpdatedAt)
	}
	if task.CreatedAt.Location() != time.UTC {
		t.Fatalf("created_at is stored in %s, want UTC", task.CreatedAt.Location())
	}
	if task.NodeID != in.NodeID || task.Capability != in.Capability ||
		task.CapabilityVersion != in.CapabilityVersion || task.TimeoutSeconds != in.TimeoutSeconds {
		t.Fatalf("stored task does not match the submission: %+v", task)
	}
	if count := countTasks(t, st); count != 1 {
		t.Fatalf("stored %d tasks, want 1", count)
	}
}

func TestSubmitDeduplicatesSamePayload(t *testing.T) {
	st := newStore(t)
	in := submitInput()
	in.IdempotencyKey = "key-1"
	first := mustSubmit(t, st, in)

	// Same payload, different whitespace: whitespace is the one difference that
	// compaction removes, so this is the same payload.
	retry := in
	retry.Input = json.RawMessage(`{ "prompt" : "hello" }`)
	second := mustSubmit(t, st, retry)
	if second.ID != first.ID {
		t.Fatalf("retry created task %s, want %s", second.ID, first.ID)
	}
	if count := countTasks(t, st); count != 1 {
		t.Fatalf("stored %d tasks, want 1", count)
	}

	// The same key on another node is a different submission.
	other := in
	other.NodeID = "node-2"
	third := mustSubmit(t, st, other)
	if third.ID == first.ID {
		t.Fatal("a second node was given the first node's task")
	}
	if count := countTasks(t, st); count != 2 {
		t.Fatalf("stored %d tasks, want 2", count)
	}
}

func TestSubmitConflictsOnDifferentPayload(t *testing.T) {
	st := newStore(t)
	in := submitInput()
	in.IdempotencyKey = "key-1"
	mustSubmit(t, st, in)

	cases := []struct {
		name   string
		mutate func(*protocol.SubmitRequest)
	}{
		{"input", func(r *protocol.SubmitRequest) { r.Input = json.RawMessage(`{"prompt":"other"}`) }},
		{"timeout", func(r *protocol.SubmitRequest) { r.TimeoutSeconds = 30 }},
		{"capability", func(r *protocol.SubmitRequest) { r.Capability = "other.run" }},
		{"capability version", func(r *protocol.SubmitRequest) { r.CapabilityVersion = 2 }},
	}
	for _, tc := range cases {
		changed := in
		tc.mutate(&changed)
		if _, err := st.Submit(context.Background(), changed); !errors.Is(err, protocol.ErrConflict) {
			t.Errorf("Submit with a changed %s = %v, want protocol.ErrConflict", tc.name, err)
		}
	}
	if count := countTasks(t, st); count != 1 {
		t.Fatalf("stored %d tasks, want 1", count)
	}
}

func TestSubmitWithoutKeyAlwaysCreatesTask(t *testing.T) {
	st := newStore(t)
	first := mustSubmit(t, st, submitInput())
	second := mustSubmit(t, st, submitInput())
	if first.ID == second.ID {
		t.Fatal("submissions without an idempotency key were deduplicated")
	}
	if count := countTasks(t, st); count != 2 {
		t.Fatalf("stored %d tasks, want 2", count)
	}
}

// TestSubmitPayloadIdentityIsLexical pins the documented limit of idempotent
// submission: payloads are compared as bytes after whitespace compaction, so
// two spellings of the same JSON document are different payloads and the retry
// is refused instead of being merged. Each case below is the same JSON value as
// the original, written differently. This is the M1 decision; a semantic
// comparison is deliberately not attempted.
func TestSubmitPayloadIdentityIsLexical(t *testing.T) {
	st := newStore(t)
	in := submitInput()
	in.IdempotencyKey = "lexical-key"
	in.Input = json.RawMessage(`{"path":"/tmp/中","n":2}`)
	mustSubmit(t, st, in)

	cases := []struct {
		name  string
		input json.RawMessage
	}{
		{"reordered keys", json.RawMessage(`{"n":2,"path":"/tmp/中"}`)},
		// Same JSON value as the original, written with an escape sequence.
		{"escaped multibyte", json.RawMessage("{\"path\":\"/tmp/\\u4e2d\",\"n\":2}")},
		{"escaped slash", json.RawMessage(`{"path":"\/tmp\/中","n":2}`)},
	}
	for _, tc := range cases {
		changed := in
		changed.Input = tc.input
		if _, err := st.Submit(context.Background(), changed); !errors.Is(err, protocol.ErrConflict) {
			t.Errorf("Submit with %s = %v, want protocol.ErrConflict (payload identity is lexical)", tc.name, err)
		}
	}
	if count := countTasks(t, st); count != 1 {
		t.Fatalf("stored %d tasks, want 1", count)
	}
}

func TestSubmitValidatesRequest(t *testing.T) {
	st := newStore(t)
	oversized := json.RawMessage(fmt.Sprintf(`{"a":%q}`, strings.Repeat("x", maxInputBytes)))

	cases := []struct {
		name   string
		mutate func(*protocol.SubmitRequest)
	}{
		{"empty node", func(r *protocol.SubmitRequest) { r.NodeID = "" }},
		{"control character in node", func(r *protocol.SubmitRequest) { r.NodeID = "node\n1" }},
		{"overlong node", func(r *protocol.SubmitRequest) { r.NodeID = strings.Repeat("n", maxLabelBytes+1) }},
		{"empty capability", func(r *protocol.SubmitRequest) { r.Capability = "" }},
		{"zero capability version", func(r *protocol.SubmitRequest) { r.CapabilityVersion = 0 }},
		{"negative capability version", func(r *protocol.SubmitRequest) { r.CapabilityVersion = -1 }},
		{"empty input", func(r *protocol.SubmitRequest) { r.Input = nil }},
		{"invalid json", func(r *protocol.SubmitRequest) { r.Input = json.RawMessage(`{"prompt":`) }},
		{"invalid utf-8", func(r *protocol.SubmitRequest) { r.Input = json.RawMessage([]byte{'"', 0xff, '"'}) }},
		{"oversized input", func(r *protocol.SubmitRequest) { r.Input = oversized }},
		{"timeout zero", func(r *protocol.SubmitRequest) { r.TimeoutSeconds = 0 }},
		{"timeout too large", func(r *protocol.SubmitRequest) { r.TimeoutSeconds = maxTimeoutSecond + 1 }},
		{"overlong idempotency key", func(r *protocol.SubmitRequest) {
			r.IdempotencyKey = strings.Repeat("k", maxIdemKeyBytes+1)
		}},
	}
	for _, tc := range cases {
		in := submitInput()
		tc.mutate(&in)
		if _, err := st.Submit(context.Background(), in); !errors.Is(err, protocol.ErrInvalid) {
			t.Errorf("Submit with %s = %v, want protocol.ErrInvalid", tc.name, err)
		}
	}
	if count := countTasks(t, st); count != 0 {
		t.Fatalf("stored %d tasks after invalid submissions, want 0", count)
	}
}

func TestGetUnknownTaskIsNotFound(t *testing.T) {
	st := newStore(t)
	if _, err := st.Get(context.Background(), "task_missing"); !errors.Is(err, protocol.ErrNotFound) {
		t.Fatalf("Get(unknown) = %v, want protocol.ErrNotFound", err)
	}
	if _, err := st.Get(context.Background(), ""); !errors.Is(err, protocol.ErrInvalid) {
		t.Fatalf("Get(empty) = %v, want protocol.ErrInvalid", err)
	}
}

func TestSubmitRejectsCancelledContext(t *testing.T) {
	st := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := st.Submit(ctx, submitInput()); err == nil {
		t.Fatal("Submit with a cancelled context succeeded")
	}
	if count := countTasks(t, st); count != 0 {
		t.Fatalf("stored %d tasks after a cancelled Submit, want 0", count)
	}
}

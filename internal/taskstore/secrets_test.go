package taskstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-gateway/internal/protocol"
)

// TestLeaseTokenIsNeverStoredInPlaintext scans every byte SQLite wrote for the
// token. A hash in the attempts table is not enough on its own: the token must
// not appear in the task row, the WAL, or a leftover page either.
func TestLeaseTokenIsNeverStoredInPlaintext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.db")
	st := openStoreAt(t, path)
	task, lease := claimStart(t, st)
	if err := st.Complete(context.Background(), task.ID, lease.Task.AttemptID, lease.Token, baseResult()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	files, err := filepath.Glob(filepath.Join(dir, "tasks.db*"))
	if err != nil {
		t.Fatalf("glob database files: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no database files were written")
	}
	t.Logf("scanning %d database files for the lease token", len(files))
	for _, name := range files {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", filepath.Base(name), err)
		}
		if bytes.Contains(data, []byte(lease.Token)) {
			t.Fatalf("%s contains the lease token in plaintext", filepath.Base(name))
		}
	}
}

// TestLeaseTokenIsAbsentFromTaskViewsAndErrors checks the other half of the
// contract: the secret is returned once and never appears in a task view, in
// JSON, or in an error message.
func TestLeaseTokenIsAbsentFromTaskViewsAndErrors(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	task, lease := claimStart(t, st)

	encoded, err := json.Marshal(lease.Task)
	if err != nil {
		t.Fatalf("marshal the claimed task: %v", err)
	}
	if bytes.Contains(encoded, []byte(lease.Token)) {
		t.Fatal("the claimed task JSON contains the lease token")
	}

	wrong := "not-the-real-token"
	err = st.Complete(ctx, task.ID, lease.Task.AttemptID, wrong, baseResult())
	if err == nil {
		t.Fatal("Complete with a wrong token succeeded")
	}
	if strings.Contains(err.Error(), lease.Token) || strings.Contains(err.Error(), wrong) {
		t.Fatalf("the error message leaks a token: %v", err)
	}

	if err := st.Complete(ctx, task.ID, lease.Task.AttemptID, lease.Token, baseResult()); err != nil {
		t.Fatalf("Complete with the right token: %v", err)
	}
	finished := mustGet(t, st, task.ID)
	encoded, err = json.Marshal(finished)
	if err != nil {
		t.Fatalf("marshal the finished task: %v", err)
	}
	if bytes.Contains(encoded, []byte(lease.Token)) {
		t.Fatal("the finished task JSON contains the lease token")
	}

	// A token from another task must not authenticate, and the refusal must
	// not echo it.
	mustSubmit(t, st, submitInput())
	other := mustClaim(t, st, testNode, time.Minute)
	err = st.Start(ctx, other.Task.ID, other.Task.AttemptID, lease.Token)
	if !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("cross-task token = %v, want protocol.ErrUnauthorized", err)
	}
	if strings.Contains(err.Error(), lease.Token) {
		t.Fatalf("the error message leaks the token: %v", err)
	}
}

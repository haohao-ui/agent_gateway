// Package integration exercises the frozen M1 contracts across the real store
// and executor. Helpers are this Go test binary, never a user's Agent CLI.
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-gateway/internal/protocol"
	"agent-gateway/internal/runner"
	"agent-gateway/internal/taskstore"
)

func TestExecutorHelper(t *testing.T) {
	if os.Getenv("AGW_INTEGRATION_HELPER") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) {
			fmt.Print(os.Args[i+1])
			if strings.HasPrefix(os.Args[i+1], "fail:") {
				os.Exit(9)
			}
			os.Exit(0)
		}
	}
	os.Exit(2)
}

func TestTaskExecutionAndLostResultACK(t *testing.T) {
	for _, tc := range []struct {
		name, instruction string
		state             protocol.State
		code              int
	}{
		{"success", "中文结果; literal $(not-a-command)", protocol.Succeeded, 0},
		{"failure", "fail: controlled fixture", protocol.Failed, 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			root := t.TempDir()
			dbPath := filepath.Join(root, "tasks.sqlite")
			store, err := taskstore.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if store != nil {
					_ = store.Close()
				}
			}()
			input, err := json.Marshal(map[string]string{"prompt": tc.instruction})
			if err != nil {
				t.Fatal(err)
			}
			req := protocol.SubmitRequest{NodeID: "fixture-node", Capability: "agent.run", CapabilityVersion: 1, Input: input, TimeoutSeconds: 10, IdempotencyKey: "fixture-submit"}
			task, err := store.Submit(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			repeated, err := store.Submit(ctx, req)
			if err != nil || repeated.ID != task.ID {
				t.Fatalf("submission retry: task=%+v err=%v", repeated, err)
			}
			lease, err := store.Claim(ctx, req.NodeID, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if lease == nil || lease.Task.ID != task.ID {
				t.Fatal("expected submitted task to be claimed")
			}
			if err := store.Start(ctx, task.ID, lease.Task.AttemptID, lease.Token); err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			result, err := runner.Run(ctx, runner.Config{
				Executable: executable, Args: []string{"-test.run=^TestExecutorHelper$", "--", "{instruction}"},
				WorkDir: root, Env: map[string]string{"AGW_INTEGRATION_HELPER": "1"},
			}, tc.instruction)
			if err != nil {
				t.Fatal(err)
			}
			if result.State != tc.state || result.ExitCode != tc.code || result.Text != tc.instruction {
				t.Fatalf("unexpected execution result: %+v", result)
			}
			if err := store.Complete(ctx, task.ID, lease.Task.AttemptID, lease.Token, result); err != nil {
				t.Fatal(err)
			}
			// Simulate server restart after storing the result but before the node
			// receives its acknowledgement. The retry must not execute again.
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = taskstore.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Complete(ctx, task.ID, lease.Task.AttemptID, lease.Token, result); err != nil {
				t.Fatal(err)
			}
			persisted, err := store.Get(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if persisted.State != tc.state || persisted.Result == nil || *persisted.Result != result {
				t.Fatalf("persisted result mismatch: %+v", persisted)
			}
			next, err := store.Claim(ctx, req.NodeID, time.Minute)
			if err != nil || next != nil {
				t.Fatalf("completed work was redelivered: %v", err)
			}
			changed := result
			changed.Text = "different result"
			if err := store.Complete(ctx, task.ID, lease.Task.AttemptID, lease.Token, changed); !errors.Is(err, protocol.ErrConflict) {
				t.Fatalf("conflicting replay: %v", err)
			}
		})
	}
}

func TestLostClaimResponseBecomesUnknownWithoutRedelivery(t *testing.T) {
	ctx := context.Background()
	store, err := taskstore.Open(filepath.Join(t.TempDir(), "tasks.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	task, err := store.Submit(ctx, protocol.SubmitRequest{NodeID: "fixture-node", Capability: "agent.run", CapabilityVersion: 1, Input: json.RawMessage(`{"prompt":"must not silently retry"}`), TimeoutSeconds: 10})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.Claim(ctx, "fixture-node", time.Second)
	if err != nil || lease == nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := store.Expire(ctx, lease.Task.LeaseExpiresAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	persisted, err := store.Get(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != protocol.Unknown {
		t.Fatalf("lost claim must be unknown, got %s", persisted.State)
	}
	next, err := store.Claim(ctx, "fixture-node", time.Minute)
	if err != nil || next != nil {
		t.Fatalf("uncertain execution must not be redelivered: %v", err)
	}
}

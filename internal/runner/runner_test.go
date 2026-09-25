package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"agent-gateway/internal/protocol"
)

// TestRunArgvVerbatim verifies that instruction arguments are passed verbatim without
// shell interpretation, word splitting, or placeholder escaping issues.
func TestRunArgvVerbatim(t *testing.T) {
	cfg := newConfig(t, "helper:echo-argv", "prefix", InstructionPlaceholder, "suffix")

	instruction := "hello world; rm -rf /; 'quoted' \"double\" \n $PATH `id` --flag"
	res, err := Run(context.Background(), cfg, instruction)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if res.State != protocol.Succeeded || res.ExitCode != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}

	want := fmt.Sprintf("ARG[0]=prefix\nARG[1]=%s\nARG[2]=suffix\n", instruction)
	if res.Text != want {
		t.Fatalf("argv mismatch:\nwant: %q\ngot:  %q", want, res.Text)
	}
}

// TestRunStdin verifies instruction delivery over stdin when Stdin=true.
func TestRunStdin(t *testing.T) {
	cfg := newConfig(t, "helper:stdin-copy")
	cfg.Stdin = true

	instruction := "first line\nsecond line with special chars: <>&'\"\nthird line"
	res, err := Run(context.Background(), cfg, instruction)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if res.State != protocol.Succeeded || res.ExitCode != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.Text != instruction {
		t.Fatalf("stdin text mismatch:\nwant: %q\ngot:  %q", instruction, res.Text)
	}
}

// TestRunWorkDir verifies that the child actually runs in Config.WorkDir.
func TestRunWorkDir(t *testing.T) {
	targetDir := t.TempDir()
	realTarget, err := filepath.EvalSymlinks(targetDir)
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}

	cfg := newConfig(t, "helper:pwd", InstructionPlaceholder)
	cfg.WorkDir = targetDir

	res, err := Run(context.Background(), cfg, "ignored")
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if res.State != protocol.Succeeded {
		t.Fatalf("expected succeeded, got: %+v", res)
	}
	realGot, err := filepath.EvalSymlinks(res.Text)
	if err != nil {
		t.Fatalf("eval symlinks for child pwd: %v", err)
	}
	if realGot != realTarget {
		t.Fatalf("pwd mismatch: got %q, want %q", realGot, realTarget)
	}
}

// TestRunEnvIsolation ensures the child environment only receives the whitelist in
// Config.Env and does not inherit the parent process's environment.
func TestRunEnvIsolation(t *testing.T) {
	t.Setenv("SECRET_PARENT_TOKEN", "super-secret-1234")

	cfg := newConfig(t, "helper:env-dump", "ALLOWED_KEY", InstructionPlaceholder)
	cfg.Env = map[string]string{
		"ALLOWED_KEY": "whitelist-value",
	}

	res, err := Run(context.Background(), cfg, "SECRET_PARENT_TOKEN")
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if res.State != protocol.Succeeded {
		t.Fatalf("expected succeeded, got: %+v", res)
	}

	want := "ALLOWED_KEY=whitelist-value\nSECRET_PARENT_TOKEN=<unset>\n"
	if res.Text != want {
		t.Fatalf("env mismatch:\nwant: %q\ngot:  %q", want, res.Text)
	}
}

// TestRunExitStatuses verifies success and failure exit code mappings.
func TestRunExitStatuses(t *testing.T) {
	t.Run("exit 0 is succeeded", func(t *testing.T) {
		cfg := newConfig(t, "helper:exit", "0", InstructionPlaceholder)
		res, err := Run(context.Background(), cfg, "ignored")
		if err != nil {
			t.Fatalf("Run error: %v", err)
		}
		if res.State != protocol.Succeeded || res.ExitCode != 0 || res.ErrorCode != "" {
			t.Fatalf("unexpected result: %+v", res)
		}
	})

	t.Run("exit 42 is failed", func(t *testing.T) {
		cfg := newConfig(t, "helper:exit", "42", InstructionPlaceholder)
		res, err := Run(context.Background(), cfg, "ignored")
		if err != nil {
			t.Fatalf("Run error: %v", err)
		}
		if res.State != protocol.Failed || res.ExitCode != 42 || res.ErrorCode != "" {
			t.Fatalf("unexpected result: %+v", res)
		}
	})
}

// TestRunOutputStderrInterleaved verifies stderr and stdout are merged into the
// same bounded tail.
func TestRunOutputStderrInterleaved(t *testing.T) {
	cfg := newConfig(t, "helper:stderr-write", InstructionPlaceholder)
	res, err := Run(context.Background(), cfg, "error message to stderr")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if res.State != protocol.Succeeded {
		t.Fatalf("expected succeeded, got %+v", res)
	}
	if res.Text != "error message to stderr" {
		t.Fatalf("unexpected stderr output: %q", res.Text)
	}
}

// TestRunLargeOutputBoundedAndDrained verifies that output larger than the buffer
// is continuously drained without stalling the child, retaining only the tail.
func TestRunLargeOutputBoundedAndDrained(t *testing.T) {
	// Produce 256 KiB of output (128k runes of 2 bytes each)
	totalRunes := 128 * 1024
	cfg := newConfig(t, "helper:repeat-rune", "a", fmt.Sprintf("%d", totalRunes), InstructionPlaceholder)
	cfg.MaxOutputBytes = 16 * 1024 // Retain 16 KiB

	res, err := Run(context.Background(), cfg, "ignored")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if res.State != protocol.Succeeded {
		t.Fatalf("expected succeeded, got %+v", res)
	}
	if !res.Truncated {
		t.Fatal("expected Truncated to be true")
	}
	if len(res.Text) != 16*1024 {
		t.Fatalf("expected 16 KiB output, got %d bytes", len(res.Text))
	}
}

// TestRunUTF8Sanitization verifies that invalid UTF-8 bytes are stripped and the
// returned text is guaranteed valid UTF-8.
func TestRunUTF8Sanitization(t *testing.T) {
	cfg := newConfig(t, "helper:raw-bytes", "255", "100", InstructionPlaceholder)
	res, err := Run(context.Background(), cfg, "ignored")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if !utf8.ValidString(res.Text) {
		t.Fatalf("output text is not valid UTF-8: %q", res.Text)
	}
	// Raw 0xFF bytes cannot form valid UTF-8 and should be dropped.
	if res.Text != "" {
		t.Fatalf("expected empty string after dropping invalid bytes, got %q", res.Text)
	}
}

// TestRunPreCancelledContext verifies that a pre-cancelled context immediately
// returns protocol.ErrInvalid and never spawns a child.
func TestRunPreCancelledContext(t *testing.T) {
	touchPath := filepath.Join(t.TempDir(), "touched.txt")
	cfg := newConfig(t, "helper:touch-after", touchPath, "0", InstructionPlaceholder)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel before Run

	res, err := Run(ctx, cfg, "ignored")
	if err == nil {
		t.Fatal("expected error for pre-cancelled context, got nil")
	}
	if !errors.Is(err, protocol.ErrInvalid) {
		t.Fatalf("expected ErrInvalid wrapping, got: %v", err)
	}
	if res != (protocol.Result{}) {
		t.Fatalf("expected zero Result, got %+v", res)
	}

	time.Sleep(50 * time.Millisecond)
	if _, statErr := os.Stat(touchPath); !os.IsNotExist(statErr) {
		t.Fatal("child was spawned despite pre-cancelled context")
	}
}

// TestRunContextCancellation verifies that cancelling a running command returns
// Result with State=Cancelled, ErrorCode=ErrorCodeCancelled, ExitCode=-1.
func TestRunContextCancellation(t *testing.T) {
	cfg := newConfig(t, "helper:sleep", "10000", InstructionPlaceholder)
	cfg.GracePeriod = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	start := time.Now()
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	res, err := Run(ctx, cfg, "ignored")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("expected nil error once process started, got: %v", err)
	}
	if res.State != protocol.Cancelled {
		t.Fatalf("state = %q, want cancelled", res.State)
	}
	if res.ErrorCode != ErrorCodeCancelled {
		t.Fatalf("error_code = %q, want %q", res.ErrorCode, ErrorCodeCancelled)
	}
	if res.ExitCode != -1 {
		t.Fatalf("exit_code = %d, want -1", res.ExitCode)
	}
	if elapsed > 4*time.Second {
		t.Fatalf("cancellation took too long: %v", elapsed)
	}
}

// TestRunContextDeadline verifies that context deadline expiry returns Result
// with State=Failed, ErrorCode=ErrorCodeTimeout, ExitCode=-1.
func TestRunContextDeadline(t *testing.T) {
	cfg := newConfig(t, "helper:sleep", "10000", InstructionPlaceholder)
	cfg.GracePeriod = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	res, err := Run(ctx, cfg, "ignored")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("expected nil error once process started, got: %v", err)
	}
	if res.State != protocol.Failed {
		t.Fatalf("state = %q, want failed", res.State)
	}
	if res.ErrorCode != ErrorCodeTimeout {
		t.Fatalf("error_code = %q, want %q", res.ErrorCode, ErrorCodeTimeout)
	}
	if res.ExitCode != -1 {
		t.Fatalf("exit_code = %d, want -1", res.ExitCode)
	}
	if elapsed > 4*time.Second {
		t.Fatalf("deadline termination took too long: %v", elapsed)
	}
}

// mockFailingController tests postStart error handling.
type mockFailingController struct {
	processController
	postStartErr error
}

func (m *mockFailingController) postStart(cmd *exec.Cmd) error {
	return m.postStartErr
}

// TestRunPostStartFailure exercises the fatal confinement handshake failure path.
func TestRunPostStartFailure(t *testing.T) {
	cfg := newConfig(t, "helper:sleep", "5000", InstructionPlaceholder)
	mock := &mockFailingController{
		processController: newProcessController(),
		postStartErr:      errors.New("mock job assignment failure"),
	}

	res, err := runWith(context.Background(), cfg, "ignored", mock)
	if err != nil {
		t.Fatalf("expected nil error on postStart failure, got %v", err)
	}
	if res.State != protocol.Failed {
		t.Fatalf("state = %q, want failed", res.State)
	}
	if res.ErrorCode != ErrorCodeProcessSetup {
		t.Fatalf("error_code = %q, want %q", res.ErrorCode, ErrorCodeProcessSetup)
	}
}

// TestRunHugeStdinIgnoredChild verifies that writing a huge instruction to a child
// that does not read stdin will time out and force-close the stdin pipe without leaking
// the writer goroutine.
func TestRunHugeStdinIgnoredChild(t *testing.T) {
	cfg := newConfig(t, "helper:stdin-ignore", "200")
	cfg.Stdin = true

	huge := strings.Repeat("A", 1024*1024) // 1 MiB exceeds OS pipe buffers
	res, err := Run(context.Background(), cfg, huge)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if res.State != protocol.Succeeded {
		t.Fatalf("expected succeeded once child exits, got: %+v", res)
	}
}

// TestRunLeakedOutputPipeDoesNotHang verifies that an escaped descendant holding the
// stdout pipe open past the direct child's exit is bounded by PipeDrainTimeout.
func TestRunLeakedOutputPipeDoesNotHang(t *testing.T) {
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "descendant.pid")
	defer killPIDFromFile(t, pidfile)

	// Direct child exits immediately after spawning a grandchild holding stdout for 10s.
	cfg := newConfig(t, "helper:spawn-and-exit", pidfile, "10000", "inherit", InstructionPlaceholder)

	start := time.Now()
	res, err := Run(context.Background(), cfg, "ignored")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if res.State != protocol.Succeeded {
		t.Fatalf("expected succeeded, got: %+v", res)
	}
	// Drain timeout is 500ms; Run must not wait the full 10s.
	if elapsed > 4*time.Second {
		t.Fatalf("Run hung on leaked stdout pipe: took %v", elapsed)
	}
}

// TestRunConcurrentRace executes Run across multiple goroutines to verify race safety.
func TestRunConcurrentRace(t *testing.T) {
	cfg := newConfig(t, "helper:echo-argv", InstructionPlaceholder)

	var wg sync.WaitGroup
	workers := 8
	errCh := make(chan error, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			instr := fmt.Sprintf("race-task-%d", id)
			res, err := Run(context.Background(), cfg, instr)
			if err != nil {
				errCh <- fmt.Errorf("worker %d: %w", id, err)
				return
			}
			want := fmt.Sprintf("ARG[0]=%s\n", instr)
			if res.State != protocol.Succeeded || res.Text != want {
				errCh <- fmt.Errorf("worker %d: unexpected result: %+v", id, res)
				return
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Error(err)
	}
}

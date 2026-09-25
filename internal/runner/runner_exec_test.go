package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"agent-gateway/internal/protocol"
)

// This file holds the takeover's execution tests. runner_test.go already covers the
// happy paths (argv, stdin, workdir, environment, exit statuses, stderr merging,
// large output, pre-cancelled context, cancellation, deadline, a confinement
// failure, huge stdin and a leaked pipe), so only the cases it does not assert are
// kept here: extra argv with stdin, the exact UTF-8 window boundary, the stdin
// writer goroutine join, the sentinel control case, the start-failure path, and the
// two controller-failure teardown paths.

// TestRunStdinKeepsExtraArgs proves Stdin=true only removes the placeholder.
func TestRunStdinKeepsExtraArgs(t *testing.T) {
	cfg := newConfig(t, "helper:echo-argv", "--flag", "value with space")
	cfg.Stdin = true

	res, err := Run(context.Background(), cfg, "unused-instruction")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	want := "ARG[0]=--flag\nARG[1]=value with space\n"
	if res.Text != want {
		t.Fatalf("text = %q, want %q", res.Text, want)
	}
}

// TestRunExactUTF8TailBoundary checks the byte window cut precisely: 4096 bytes of
// 3-byte runes hold 1365 whole runes plus one orphaned continuation byte.
func TestRunExactUTF8TailBoundary(t *testing.T) {
	cfg := newConfig(t, "helper:repeat-rune", "中", "20000", "{instruction}")
	cfg.MaxOutputBytes = 4096

	res, err := Run(context.Background(), cfg, "ignored")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !utf8.ValidString(res.Text) {
		t.Fatalf("text is not valid UTF-8: %q", res.Text)
	}
	if !res.Truncated {
		t.Fatal("truncated = false, want true")
	}
	if len(res.Text) != 4095 {
		t.Fatalf("retained %d bytes, want 4095 (one orphaned continuation byte dropped)", len(res.Text))
	}
	for _, r := range res.Text {
		if r != '中' {
			t.Fatalf("unexpected rune %q in the truncated tail", r)
		}
	}
}

// TestRunStdinWriterDoesNotLeakWhenChildIgnoresInput sends an instruction far
// larger than the pipe buffer to a child that never reads it, and additionally
// requires the writer goroutine to be joined: the count must come back down.
func TestRunStdinWriterDoesNotLeakWhenChildIgnoresInput(t *testing.T) {
	cfg := newConfig(t, "helper:stdin-ignore", "300")
	cfg.Stdin = true
	instruction := strings.Repeat("x", 1<<20)

	before := runtime.NumGoroutine()
	done := make(chan struct{})
	var (
		res protocol.Result
		err error
	)
	go func() {
		defer close(done)
		res, err = Run(context.Background(), cfg, instruction)
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return: the stdin writer is stuck")
	}

	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if res.State != protocol.Succeeded {
		t.Fatalf("state = %q, want succeeded", res.State)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("goroutines leaked: before=%d after=%d", before, runtime.NumGoroutine())
}

// TestRunJoinsStdinWriterWhenDescendantHoldsStdin keeps the read end of the stdin
// pipe open in a descendant that never reads, while the direct child exits at once.
// The writer goroutine is blocked on a full pipe at that moment, so this is the
// case that would leak if the writer were abandoned. Note that os/exec's Wait also
// closes the stdin pipe, which unblocks the write; the join is what makes the
// guarantee independent of that behaviour, and the assertions below hold either way.
func TestRunJoinsStdinWriterWhenDescendantHoldsStdin(t *testing.T) {
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "grandchild.pid")
	cfg := newConfig(t, "helper:spawn-and-exit", pidfile, "60000", "null", "stdin")
	cfg.Stdin = true
	instruction := strings.Repeat("x", 1<<20)

	before := runtime.NumGoroutine()
	start := time.Now()
	res, err := Run(context.Background(), cfg, instruction)
	elapsed := time.Since(start)
	defer killPIDFromFile(t, pidfile)

	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if res.State != protocol.Succeeded {
		t.Fatalf("state = %q, want succeeded", res.State)
	}
	if elapsed > 4*StdinWriteTimeout {
		t.Fatalf("Run waited %s for the stdin writer", elapsed)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("stdin writer goroutine leaked: before=%d after=%d", before, runtime.NumGoroutine())
}

// TestRunLiveContextStartsChild is the control case for the pre-cancelled sentinel:
// without it, that test could pass for the wrong reason.
func TestRunLiveContextStartsChild(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "child-ran")
	cfg := newConfig(t, "helper:touch-after", "{instruction}", "0")

	res, err := Run(context.Background(), cfg, sentinel)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if res.State != protocol.Succeeded {
		t.Fatalf("state = %q, want succeeded", res.State)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("sentinel missing for a live context, the fail-closed test would be vacuous: %v", err)
	}
}

// TestRunStartFailureReturnsError covers a validated executable the OS still refuses
// to start: a regular file without execute permission.
func TestRunStartFailureReturnsError(t *testing.T) {
	dir := t.TempDir()
	notExecutable := filepath.Join(dir, "not-executable")
	if err := os.WriteFile(notExecutable, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cfg := Config{
		Executable: notExecutable,
		Args:       []string{InstructionPlaceholder},
		WorkDir:    dir,
		Env:        map[string]string{},
	}
	res, err := Run(context.Background(), cfg, "ignored")
	if err == nil {
		t.Fatalf("Run returned nil error, result %+v", res)
	}
	if !errors.Is(err, protocol.ErrInvalid) {
		t.Fatalf("error = %v, want one wrapping protocol.ErrInvalid", err)
	}
}

// fakeController is a processController with triggerable failures. It deliberately
// does not kill the child on a postStart failure, so Run's own fail-closed handling
// is what must save the day.
type fakeController struct {
	prepareErr error
	postErr    error

	mu       sync.Mutex
	cleaned  int
	reaped   bool
	postCall int
}

func (f *fakeController) prepareCmd(*exec.Cmd) error { return f.prepareErr }

func (f *fakeController) postStart(*exec.Cmd) error {
	f.mu.Lock()
	f.postCall++
	f.mu.Unlock()
	return f.postErr
}

func (f *fakeController) terminate(*exec.Cmd, time.Duration) {}

func (f *fakeController) markReaped() {
	f.mu.Lock()
	f.reaped = true
	f.mu.Unlock()
}

func (f *fakeController) wasReaped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reaped
}

func (f *fakeController) cleanup(*exec.Cmd) {
	f.mu.Lock()
	f.cleaned++
	f.mu.Unlock()
}

func (f *fakeController) cleanupCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cleaned
}

// TestRunFailsClosedWhenConfinementFails goes beyond asserting the result fields:
// the already-started child must be dead before Run returns, which the sentinel
// file written by a surviving child would reveal, and cleanup must run exactly once.
func TestRunFailsClosedWhenConfinementFails(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "child-finished")
	cfg := newConfig(t, "helper:touch-after", "{instruction}", "700")
	controller := &fakeController{postErr: errors.New("simulated job assignment failure")}

	res, err := runWith(context.Background(), cfg, sentinel, controller)
	if err != nil {
		t.Fatalf("Run returned error after the child started: %v", err)
	}
	if res.State != protocol.Failed {
		t.Fatalf("state = %q, want failed", res.State)
	}
	if res.ErrorCode != ErrorCodeProcessSetup {
		t.Fatalf("error code = %q, want %q", res.ErrorCode, ErrorCodeProcessSetup)
	}

	// Wait past the child's own deadline: a survivor would have written the sentinel.
	time.Sleep(1200 * time.Millisecond)
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("the child survived a failed confinement handshake")
	}
	if got := controller.cleanupCount(); got != 1 {
		t.Fatalf("cleanup called %d times, want exactly 1", got)
	}
}

// TestRunPrepareFailureReturnsError covers a controller that cannot prepare the
// process at all: nothing may be spawned and the failure is an error, not a Result.
func TestRunPrepareFailureReturnsError(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "child-finished")
	cfg := newConfig(t, "helper:touch-after", "{instruction}", "0")
	controller := &fakeController{prepareErr: errors.New("simulated job creation failure")}

	res, err := runWith(context.Background(), cfg, sentinel, controller)
	if err == nil {
		t.Fatalf("Run returned nil error, result %+v", res)
	}
	if !errors.Is(err, protocol.ErrInvalid) {
		t.Fatalf("error = %v, want one wrapping protocol.ErrInvalid", err)
	}
	if res != (protocol.Result{}) {
		t.Fatalf("result = %+v, want the zero value", res)
	}
	if controller.postCall != 0 {
		t.Fatal("postStart was called even though prepare failed")
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("the child started even though the controller could not prepare it")
	}
}

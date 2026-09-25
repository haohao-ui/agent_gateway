//go:build !windows

package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"agent-gateway/internal/protocol"
)

// TestRunKillsDescendantsThatIgnoreTerm is the process-tree test. The direct child
// spawns a grandchild that ignores SIGTERM and dies on its own from the graceful
// signal; the grandchild can only be stopped by the SIGKILL escalation, which must
// still happen after the direct child has already been reaped.
func TestRunKillsDescendantsThatIgnoreTerm(t *testing.T) {
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "grandchild.pid")
	cfg := newConfig(t, "helper:spawn", "{instruction}", "60000", "null")
	cfg.GracePeriod = 200 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type outcome struct {
		res protocol.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := Run(ctx, cfg, pidfile)
		done <- outcome{res, err}
	}()

	// Wait until the grandchild exists and the parent is still running, so the
	// cancellation arrives while the whole tree is alive.
	pidText := waitForFile(t, pidfile, 10*time.Second)
	grandchildPID, err := strconv.Atoi(strings.TrimSpace(pidText))
	if err != nil {
		t.Fatalf("grandchild pid file %q is not a pid: %v", pidText, err)
	}
	if !processAlive(grandchildPID) {
		t.Fatal("grandchild is not alive before cancellation, the test would be vacuous")
	}

	cancel()

	var got outcome
	select {
	case got = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if got.err != nil {
		t.Fatalf("Run returned error: %v", got.err)
	}
	if got.res.State != protocol.Cancelled {
		t.Fatalf("state = %q, want cancelled", got.res.State)
	}
	if got.res.ErrorCode != ErrorCodeCancelled {
		t.Fatalf("error code = %q, want %q", got.res.ErrorCode, ErrorCodeCancelled)
	}

	waitForProcessExit(t, grandchildPID, 5*time.Second)
}

// TestRunDoesNotSignalGroupAfterNormalExit characterizes the deliberate limit of
// the Unix cleanup: once Wait has reaped the direct child, its PGID may already
// have been recycled by the kernel, so the group is no longer a safe kill target
// and a descendant that outlives a normal exit is left alone. This test pins that
// behaviour; if cleanup is ever extended, the PGID reuse hazard must be solved
// first (a pidfd or a cgroup), not just the assertion relaxed.
func TestRunDoesNotSignalGroupAfterNormalExit(t *testing.T) {
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "grandchild.pid")
	cfg := newConfig(t, "helper:spawn-and-exit", "{instruction}", "60000", "null")

	res, err := Run(context.Background(), cfg, pidfile)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if res.State != protocol.Succeeded {
		t.Fatalf("state = %q, want succeeded for the immediate parent exit", res.State)
	}

	pidText := waitForFile(t, pidfile, 10*time.Second)
	grandchildPID, err := strconv.Atoi(strings.TrimSpace(pidText))
	if err != nil {
		t.Fatalf("grandchild pid file %q is not a pid: %v", pidText, err)
	}
	defer func() {
		if p, err := os.FindProcess(grandchildPID); err == nil {
			_ = p.Kill()
		}
	}()

	// Give a hypothetical late group kill time to land before asserting survival.
	time.Sleep(300 * time.Millisecond)
	if !processAlive(grandchildPID) {
		t.Fatal("descendant of a normally exited child was signalled; the PGID reuse hazard is back")
	}
}

// TestRunChildGetsOwnProcessGroup checks the isolation primitive the cancellation
// path depends on: the child must lead its own group, not the runner's.
func TestRunChildGetsOwnProcessGroup(t *testing.T) {
	cfg := newConfig(t, "helper:pgid", "{instruction}")
	res, err := Run(context.Background(), cfg, "ignored")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	var pid, pgid int
	if _, err := fmt.Sscanf(res.Text, "pid=%d pgid=%d", &pid, &pgid); err != nil {
		t.Fatalf("cannot parse %q: %v", res.Text, err)
	}
	if pgid != pid {
		t.Fatalf("child pgid = %d, want its own pid %d (it stayed in the runner's group)", pgid, pid)
	}
	if pgid == syscall.Getpgrp() {
		t.Fatal("child shares the runner's process group")
	}
}

// TestRunRejectsNonRegularExecutable covers the type check that keeps Start from
// blocking on a FIFO or opening a device node.
func TestRunRejectsNonRegularExecutable(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}

	cfg := Config{
		Executable: fifo,
		Args:       []string{InstructionPlaceholder},
		WorkDir:    dir,
		Env:        map[string]string{},
	}
	res, err := Run(context.Background(), cfg, "ignored")
	if err == nil {
		t.Fatalf("Run accepted a FIFO executable, result %+v", res)
	}
	if !errors.Is(err, protocol.ErrInvalid) {
		t.Fatalf("error = %v, want one wrapping protocol.ErrInvalid", err)
	}
}

// processAlive reports whether a pid still exists. A zombie still counts as alive;
// the caller polls until the reaper has collected it.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// waitForProcessExit polls until the pid is gone.
func waitForProcessExit(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %d is still alive after %s", pid, timeout)
}

// helperPrintPgid is the Unix implementation of the "pgid" helper mode.
func helperPrintPgid() int {
	fmt.Printf("pid=%d pgid=%d", os.Getpid(), syscall.Getpgrp())
	return 0
}

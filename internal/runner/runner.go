// Package runner executes one local CLI under explicit limits: an argv or stdin
// instruction that cannot be reshaped by the caller, a whitelisted environment, a
// bounded output tail, and a whole-process-tree teardown on cancellation.
//
// It is deliberately not a sandbox. See Config for the exact isolation limits.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"agent-gateway/internal/protocol"
)

const (
	// ErrorCodeTimeout is returned when execution hits the context deadline.
	ErrorCodeTimeout = "timeout"
	// ErrorCodeCancelled is returned when execution is cancelled by the context.
	ErrorCodeCancelled = "cancelled"
	// ErrorCodeProcessSetup is returned when the child started but could not be
	// confined or resumed; the child is killed before Run returns.
	ErrorCodeProcessSetup = "process_setup"
	// PipeDrainTimeout bounds how long Run waits for the output pipe to reach EOF
	// once the direct child is gone; a descendant that escaped the process group
	// can hold it open forever.
	PipeDrainTimeout = 500 * time.Millisecond
	// StdinWriteTimeout bounds how long Run waits for the stdin writer goroutine
	// before force-closing the pipe.
	StdinWriteTimeout = 2 * time.Second
)

// Run executes the instruction once with the given configuration.
//
// Return contract:
//   - Validation or spawn failure returns a zero Result and a non-nil error
//     wrapping protocol.ErrInvalid. A cancelled or already-expired ctx also fails
//     here: no subprocess is started.
//   - Once the child has started, every outcome is reported as a Result with a nil
//     error: exit status 0 is succeeded, a non-zero exit status is failed,
//     ctx deadline is failed with ErrorCodeTimeout, ctx cancellation is cancelled
//     with ErrorCodeCancelled, and a failed confinement handshake is failed with
//     ErrorCodeProcessSetup.
//   - Result.ExitCode is -1 whenever no valid exit status exists (the child was
//     signalled, or Wait itself failed).
//
// Run returns only after every goroutine it created has exited and the process tree
// has been torn down, so a caller may safely reuse the same working directory.
func Run(ctx context.Context, cfg Config, instruction string) (protocol.Result, error) {
	return runWith(ctx, cfg, instruction, newProcessController())
}

// runWith carries the implementation of Run with the platform controller injected,
// so the confinement-failure path can be exercised on any platform.
func runWith(ctx context.Context, cfg Config, instruction string, controller processController) (protocol.Result, error) {
	finalArgs, err := cfg.validate(instruction)
	if err != nil {
		return protocol.Result{}, err
	}

	// Fail closed before spawning: a subprocess started under an already-cancelled
	// context would run with nobody left to report or clean it up.
	if err := ctx.Err(); err != nil {
		return protocol.Result{}, fmt.Errorf("%w: context is already %v", protocol.ErrInvalid, err)
	}

	cmd := exec.Command(cfg.Executable, finalArgs...)
	cmd.Dir = cfg.WorkDir
	// Whitelist only; os.Environ is never copied.
	cmd.Env = environment(cfg.Env)

	// One pipe for both streams keeps interleaving cheap and gives a single EOF to
	// wait for.
	pr, pw, err := os.Pipe()
	if err != nil {
		return protocol.Result{}, fmt.Errorf("%w: create output pipe: %v", protocol.ErrInvalid, err)
	}
	cmd.Stdout = pw
	cmd.Stderr = pw

	var (
		stdinPipe io.WriteCloser
		nullStdin *os.File
	)
	if cfg.Stdin {
		stdinPipe, err = cmd.StdinPipe()
		if err != nil {
			closeFiles(pr, pw)
			return protocol.Result{}, fmt.Errorf("%w: create stdin pipe: %v", protocol.ErrInvalid, err)
		}
	} else {
		// Without this the child would inherit the gateway's own standard input.
		nullStdin, err = os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			closeFiles(pr, pw)
			return protocol.Result{}, fmt.Errorf("%w: open %s for stdin: %v", protocol.ErrInvalid, os.DevNull, err)
		}
		cmd.Stdin = nullStdin
	}

	if err = controller.prepareCmd(cmd); err != nil {
		closeFiles(pr, pw)
		closeStdin(stdinPipe, nullStdin)
		return protocol.Result{}, fmt.Errorf("%w: prepare process: %v", protocol.ErrInvalid, err)
	}

	if err = cmd.Start(); err != nil {
		closeFiles(pr, pw)
		closeStdin(stdinPipe, nullStdin)
		controller.cleanup(cmd)
		return protocol.Result{}, fmt.Errorf("%w: start %q: %v", protocol.ErrInvalid, cfg.Executable, err)
	}

	// ---- the child exists from here on: failures become Results, never errors ----

	// The parent must drop its copies of the write end and the child's stdin: the
	// child owns its own duplicates now, and a lingering write end would keep the
	// drain from ever seeing EOF.
	_ = pw.Close()
	if nullStdin != nil {
		_ = nullStdin.Close()
	}

	buf := newTailBuffer(cfg.MaxOutputBytes)
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		// A copy error is expected whenever the read end is force-closed below.
		_, _ = io.Copy(buf, pr)
		_ = pr.Close()
	}()

	// reaped is closed after Wait returns; waitErr carries its result. Both are
	// needed because the cancellation watcher and the main path both observe exit.
	reaped := make(chan struct{})
	waitErrCh := make(chan error, 1)
	go func() {
		waitErr := cmd.Wait()
		controller.markReaped()
		waitErrCh <- waitErr
		close(reaped)
	}()

	var stdinWriter *stdinWriter
	if stdinPipe != nil {
		stdinWriter = startStdinWriter(stdinPipe, instruction)
	}

	// A failed confinement handshake (Windows suspend/assign/resume) is fatal: the
	// child may not run unconfined, so it is killed and reported as a failed run.
	if err := controller.postStart(cmd); err != nil {
		// postStart already kills a child it could not confine; killing again makes
		// the guarantee independent of the controller's failure handling.
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		waitErr := <-waitErrCh
		if stdinWriter != nil {
			stdinWriter.join(StdinWriteTimeout)
		}
		controller.cleanup(cmd)
		drainOutput(pr, drainDone)
		return failedResult(buf, exitCodeOf(waitErr), ErrorCodeProcessSetup), nil
	}

	// Watch for cancellation on a separate goroutine so terminate() may block for
	// the grace period without stalling the exit path.
	var (
		cancelErr error
		watchDone chan struct{}
	)
	if ctx.Done() != nil {
		watchDone = make(chan struct{})
		go func() {
			defer close(watchDone)
			select {
			case <-ctx.Done():
				if controller.wasReaped() {
					// The child exited on its own; its exit status is authoritative.
					return
				}
				cancelErr = ctx.Err()
				controller.terminate(cmd, cfg.GracePeriod)
			case <-reaped:
			}
		}()
	}

	// Always wait for the direct child: Run must not return while a process it
	// started is still alive.
	<-reaped
	if watchDone != nil {
		// Join the watcher before reading cancelErr; it may still be terminating the
		// process tree, and cleanup must not race that teardown.
		<-watchDone
	}
	waitErr := <-waitErrCh

	controller.cleanup(cmd)
	if stdinWriter != nil {
		// Joined after the child is gone: a child that ignored its stdin can no
		// longer keep the writer blocked.
		stdinWriter.join(StdinWriteTimeout)
	}
	drainOutput(pr, drainDone)

	exitCode := exitCodeOf(waitErr)
	text, truncated := buf.Result()

	var state protocol.State
	var errorCode string
	switch {
	case errors.Is(cancelErr, context.DeadlineExceeded):
		state, errorCode = protocol.Failed, ErrorCodeTimeout
	case cancelErr != nil:
		state, errorCode = protocol.Cancelled, ErrorCodeCancelled
	case exitCode == 0:
		state = protocol.Succeeded
	default:
		state = protocol.Failed
	}

	return protocol.Result{
		State:     state,
		Text:      text,
		ExitCode:  exitCode,
		ErrorCode: errorCode,
		Truncated: truncated,
	}, nil
}

// failedResult builds the Result for a run that started but could not proceed.
func failedResult(buf *tailBuffer, exitCode int, errorCode string) protocol.Result {
	text, truncated := buf.Result()
	return protocol.Result{
		State:     protocol.Failed,
		Text:      text,
		ExitCode:  exitCode,
		ErrorCode: errorCode,
		Truncated: truncated,
	}
}

// exitCodeOf extracts a valid exit status, or -1 when none exists.
func exitCodeOf(waitErr error) int {
	if waitErr == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return code
		}
	}
	return -1
}

// drainOutput waits for the copy goroutine and force-closes the read end if a
// lingering writer keeps the pipe open past the drain timeout.
func drainOutput(pr *os.File, done <-chan struct{}) {
	timer := time.NewTimer(PipeDrainTimeout)
	defer timer.Stop()

	select {
	case <-done:
	case <-timer.C:
		_ = pr.Close()
		<-done
	}
}

// stdinWriter feeds the instruction to the child and guarantees that the pipe is
// closed and the goroutine joined even when the child never reads it.
type stdinWriter struct {
	pipe io.WriteCloser
	done chan struct{}
}

func startStdinWriter(pipe io.WriteCloser, instruction string) *stdinWriter {
	w := &stdinWriter{pipe: pipe, done: make(chan struct{})}
	go func() {
		defer close(w.done)
		// A child that exits without reading fails this write; the pipe is closed
		// below either way, and the write error is not an invocation failure.
		_, _ = io.WriteString(pipe, instruction)
		_ = pipe.Close()
	}()
	return w
}

// join blocks until the writer goroutine has exited, force-closing the pipe after
// timeout so an instruction larger than the pipe buffer cannot leak the goroutine.
func (w *stdinWriter) join(timeout time.Duration) {
	select {
	case <-w.done:
		return
	case <-time.After(timeout):
		// Closing an *os.File is safe while another goroutine is blocked writing:
		// the runtime poller unblocks it with ErrClosed.
		_ = w.pipe.Close()
		<-w.done
	}
}

// closeFiles closes every non-nil file, ignoring errors.
func closeFiles(files ...*os.File) {
	for _, f := range files {
		if f != nil {
			_ = f.Close()
		}
	}
}

// closeStdin releases the descriptors owned by Run before Start.
func closeStdin(pipe io.WriteCloser, null *os.File) {
	if pipe != nil {
		_ = pipe.Close()
	}
	if null != nil {
		_ = null.Close()
	}
}

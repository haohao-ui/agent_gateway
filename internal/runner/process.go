package runner

import (
	"os/exec"
	"time"
)

// processController owns the platform-specific lifecycle of one controlled child:
// how it is prepared before Start, confined after Start, terminated on
// cancellation, and how the whole process tree is torn down before Run returns.
//
// All methods are called from a single goroutine and must be safe for nil
// command/process fields, because a failure before Start leaves them unset.
type processController interface {
	// prepareCmd configures cmd before Start, e.g. a new process group or the
	// Windows Job Object that the child is assigned to immediately after Start.
	prepareCmd(cmd *exec.Cmd) error
	// postStart runs right after Start succeeded. On Windows it performs the
	// suspend/assign/resume handshake; an error there is fatal for the invocation.
	postStart(cmd *exec.Cmd) error
	// terminate asks the whole process tree to exit and escalates to a forced kill
	// after gracePeriod. It blocks until escalation has been attempted.
	terminate(cmd *exec.Cmd, gracePeriod time.Duration)
	// markReaped is called by the Wait goroutine as soon as the direct child has
	// been collected, so termination stops treating its PGID/job as live.
	markReaped()
	// wasReaped reports whether Wait has already collected the direct child. It lets
	// the cancellation watcher defer to a real exit status instead of reporting a
	// cancellation for a process that finished on its own.
	wasReaped() bool
	// cleanup releases OS resources and guarantees that no process of this
	// invocation survives. It must be idempotent and must not kill a process
	// group whose process has already been reaped (the PGID may be recycled).
	cleanup(cmd *exec.Cmd)
}

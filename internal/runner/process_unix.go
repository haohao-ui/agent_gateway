//go:build !windows

package runner

import (
	"errors"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const (
	// pgidSettleTimeout bounds how long cancellation waits for killed descendants to
	// disappear from the process group.
	pgidSettleTimeout = 2 * time.Second
	// pgidPollInterval is the polling step used while waiting for that group to drain.
	pgidPollInterval = 5 * time.Millisecond
)

// unixProcessController confines one child to its own process group so that the
// whole group can be signalled at once.
//
// Termination protocol on cancellation:
//  1. SIGTERM the group.
//  2. Wait for the grace period, or until the direct child is reaped.
//  3. If the group still has live members (a descendant that ignored SIGTERM, or the
//     direct child itself), SIGKILL the group and wait briefly for it to drain.
//
// The group is only ever signalled by PGID while the direct child has not been
// reaped. Once Wait has reaped it, the kernel may recycle that PGID for an
// unrelated process group, so cleanup deliberately does not kill it.
type unixProcessController struct {
	mu      sync.Mutex
	pgid    int
	started bool
	reaped  bool
	// waitDone is closed once the direct child has been reaped by Wait.
	waitDone chan struct{}
	// reapOnce guards the single close of waitDone.
	reapOnce sync.Once
	// cleanOnce guards the single cleanup pass.
	cleanOnce sync.Once
}

func newProcessController() processController {
	return &unixProcessController{
		waitDone: make(chan struct{}),
	}
}

func (c *unixProcessController) prepareCmd(cmd *exec.Cmd) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// A new group with pgid == pid; the child must not be able to join ours.
	cmd.SysProcAttr.Setpgid = true
	return nil
}

func (c *unixProcessController) postStart(cmd *exec.Cmd) error {
	if cmd.Process == nil || cmd.Process.Pid <= 0 {
		return errors.New("process group unavailable: process did not start")
	}
	c.mu.Lock()
	c.pgid = cmd.Process.Pid
	c.started = true
	c.mu.Unlock()
	return nil
}

// markReaped records that Wait has collected the direct child.
func (c *unixProcessController) markReaped() {
	c.mu.Lock()
	c.reaped = true
	c.mu.Unlock()
	c.reapOnce.Do(func() { close(c.waitDone) })
}

func (c *unixProcessController) wasReaped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reaped
}

func (c *unixProcessController) terminate(cmd *exec.Cmd, gracePeriod time.Duration) {
	pgid, ok := c.livePGID()
	if !ok {
		return
	}

	_ = syscall.Kill(-pgid, syscall.SIGTERM)

	timer := time.NewTimer(gracePeriod)
	defer timer.Stop()

	select {
	case <-c.waitDone:
		// The direct child exited. Descendants may still ignore SIGTERM, so the
		// group is only considered done when no member is left.
		if !pgidAlive(pgid) {
			return
		}
	case <-timer.C:
	}

	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	c.awaitGroupExit(pgid)
}

func (c *unixProcessController) cleanup(cmd *exec.Cmd) {
	c.cleanOnce.Do(func() {
		pgid, started := c.livePGID()
		if started {
			// The direct child is still unreaped here, so the PGID cannot have been
			// recycled yet and killing the group is safe.
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	})
}

// livePGID returns the process group id while the direct child is started but not
// yet reaped. After the reaper ran, the PGID is no longer a safe kill target.
func (c *unixProcessController) livePGID() (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started || c.reaped || c.pgid <= 0 {
		return 0, false
	}
	return c.pgid, true
}

// awaitGroupExit polls until the process group is empty or the settle timeout
// expires, so Run does not return while a freshly killed descendant is still dying.
func (c *unixProcessController) awaitGroupExit(pgid int) {
	deadline := time.NewTimer(pgidSettleTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(pgidPollInterval)
	defer ticker.Stop()

	for {
		if !pgidAlive(pgid) {
			return
		}
		select {
		case <-deadline.C:
			return
		case <-ticker.C:
		}
	}
}

// pgidAlive reports whether any process remains in the group. A nil error or EPERM
// both mean "at least one member exists"; ESRCH means the group is empty.
func pgidAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

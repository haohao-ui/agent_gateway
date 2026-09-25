//go:build windows

package runner

import (
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// windowsProcessController confines one child (and every descendant it creates) to
// a Job Object.
//
// # Why the child is created suspended
//
// os/exec starts children with CreateProcess and, because it closes the main
// thread handle immediately (syscall.StartProcess defers CloseHandle(pi.Thread)),
// it cannot resume a suspended child afterwards. Two alternatives were considered
// and rejected:
//
//   - Create the process already assigned to the job: CreateProcess has no job
//     parameter; assignment is always a separate call, so a race would remain.
//   - Use PROC_THREAD_ATTRIBUTE_JOB_LIST: not reachable through os/exec's
//     SysProcAttr, and starting the process ourselves would fork the whole
//     Start/Wait machinery of the standard library.
//
// The implemented handshake is therefore:
//
//	prepareCmd : create the job and add CREATE_SUSPENDED to CreationFlags.
//	Start      : CreateProcess returns with the child created but not running.
//	postStart  : re-open the main thread by TID (the original handle is already
//	             closed), assign the process to the job, resume the thread.
//
// Until ResumeThread, the child has executed no user code, so it cannot have
// created a descendant outside the job. A failure at any step before the resume
// kills the still-suspended child and fails the invocation, so the child is never
// left running unconfined.
//
// # Cancellation and cleanup
//
// The direct child is only one member of the job. JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
// plus TerminateJobObject terminate the entire job in the kernel, which is what
// makes process-tree termination possible on Windows at all: killing the parent
// handle alone would leave descendants running.
//
// Verification status: this file is cross-compiled for GOOS=windows but has never
// been executed on a Windows kernel in this environment. See docs/reports/agy-M1.md.
type windowsProcessController struct {
	mu      sync.Mutex
	job     windows.Handle
	pid     uint32
	started bool
	reaped  bool
	// waitDone is closed once the direct child has been reaped by Wait.
	waitDone chan struct{}
	reapOnce sync.Once
	// cleanOnce guards the single job-handle close.
	cleanOnce sync.Once
}

func newProcessController() processController {
	return &windowsProcessController{waitDone: make(chan struct{})}
}

func (c *windowsProcessController) prepareCmd(cmd *exec.Cmd) error {
	if cmd.SysProcAttr == nil {
		// Without SysProcAttr there is no way to request CREATE_SUSPENDED, and
		// assigning the child to the job after it started running would leave a
		// window in which it could escape. Fail closed instead.
		return errors.New("windows process control requires SysProcAttr for CREATE_SUSPENDED; refusing to start an unconfined child")
	}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("create job object: %w", err)
	}

	// KILL_ON_JOB_CLOSE is the backstop: even if Run fails or a later step forgets
	// to terminate explicitly, closing the handle kills every job member.
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return fmt.Errorf("set job object limits: %w", err)
	}

	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP

	c.mu.Lock()
	c.job = job
	c.mu.Unlock()
	return nil
}

func (c *windowsProcessController) postStart(cmd *exec.Cmd) error {
	if cmd.Process == nil || cmd.Process.Pid <= 0 {
		return errors.New("job assignment failed: process did not start")
	}
	pid := uint32(cmd.Process.Pid)

	c.mu.Lock()
	job := c.job
	c.pid = pid
	c.mu.Unlock()
	if job == 0 {
		return errors.New("job assignment failed: job object is not available")
	}

	// The child is suspended, so it still owns exactly one thread: the main one.
	tid, err := mainThreadID(pid)
	if err != nil {
		return c.abortSuspended(cmd, fmt.Errorf("job assignment failed: %w", err))
	}

	hThread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, tid)
	if err != nil {
		return c.abortSuspended(cmd, fmt.Errorf("job assignment failed: open main thread: %w", err))
	}

	hProc, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false, pid)
	if err != nil {
		_ = windows.CloseHandle(hThread)
		return c.abortSuspended(cmd, fmt.Errorf("job assignment failed: open process: %w", err))
	}
	assignErr := windows.AssignProcessToJobObject(job, hProc)
	_ = windows.CloseHandle(hProc)
	if assignErr != nil {
		_ = windows.CloseHandle(hThread)
		return c.abortSuspended(cmd, fmt.Errorf("job assignment failed: %w", assignErr))
	}

	// The child is confined to the job now, so it is safe to let it run. It must
	// never be left suspended: an unresumed child would hang the invocation.
	if _, err := windows.ResumeThread(hThread); err != nil {
		_ = windows.CloseHandle(hThread)
		return c.abortSuspended(cmd, fmt.Errorf("resume suspended child: %w", err))
	}
	_ = windows.CloseHandle(hThread)

	c.mu.Lock()
	c.started = true
	c.mu.Unlock()
	return nil
}

// markReaped records that Wait has collected the direct child.
func (c *windowsProcessController) markReaped() {
	c.mu.Lock()
	c.reaped = true
	c.mu.Unlock()
	c.reapOnce.Do(func() { close(c.waitDone) })
}

func (c *windowsProcessController) terminate(cmd *exec.Cmd, gracePeriod time.Duration) {
	c.mu.Lock()
	job, pid, started := c.job, c.pid, c.started
	c.mu.Unlock()
	if job == 0 || !started {
		return
	}

	// Graceful phase: CTRL_BREAK reaches the child's own console process group
	// (CREATE_NEW_PROCESS_GROUP), not ours. Best effort: a child without a console
	// simply never sees it and the grace period expires.
	_ = windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, pid)

	timer := time.NewTimer(gracePeriod)
	defer timer.Stop()
	select {
	case <-c.waitDone:
		return
	case <-timer.C:
	}

	// Forced phase: the kernel terminates every process still in the job.
	_ = windows.TerminateJobObject(job, 1)
}

func (c *windowsProcessController) wasReaped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reaped
}

func (c *windowsProcessController) cleanup(cmd *exec.Cmd) {
	c.cleanOnce.Do(func() {
		c.mu.Lock()
		job := c.job
		c.job = 0
		c.mu.Unlock()
		if job != 0 {
			// Closing the handle is the last kill: any descendant that outlived the
			// direct child is still a job member and dies here.
			_ = windows.CloseHandle(job)
		}
	})
}

// abortSuspended kills a child that is still suspended because job assignment or
// the resume failed. Killing by process handle works whether or not the child was
// already assigned to the job; terminating the job covers the assigned case.
func (c *windowsProcessController) abortSuspended(cmd *exec.Cmd, cause error) error {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	c.mu.Lock()
	job := c.job
	c.mu.Unlock()
	if job != 0 {
		_ = windows.TerminateJobObject(job, 1)
	}
	return cause
}

// mainThreadID resolves the thread id of a freshly created suspended process. The
// handle returned by CreateProcess is closed by syscall.StartProcess, so the thread
// has to be found again before it can be resumed.
func mainThreadID(pid uint32) (uint32, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return 0, fmt.Errorf("snapshot threads: %w", err)
	}
	defer windows.CloseHandle(snapshot)

	var entry windows.ThreadEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID == pid {
			return entry.ThreadID, nil
		}
	}
	return 0, fmt.Errorf("no thread found for pid %d: %w", pid, err)
}

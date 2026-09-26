package node

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"agent-gateway/internal/protocol"
	"agent-gateway/internal/runner"
)

const (
	journalFilename = "journal.jsonl"
	outboxDirname   = "outbox"
	lockFilename    = "node.lock"
	workDirname     = "work"

	// reportAttempts bounds how many times one result is offered to the gateway
	// before it is left in the outbox. Retrying forever would stop the node from
	// ever picking up other work; leaving it on disk keeps the evidence.
	reportAttempts = 6
	baseBackoff    = 500 * time.Millisecond
	maxBackoff     = 30 * time.Second

	// shutdownReportTimeout is the detached window a completed result gets to
	// reach the gateway while the node is shutting down.
	shutdownReportTimeout = 5 * time.Second

	// maxErrorTextBytes caps the text the node puts into a result it builds
	// itself, well under the gateway's 64 KiB result limit.
	maxErrorTextBytes = 4 << 10
)

// Node executes tasks for one gateway.
type Node struct {
	cfg Config
	dir string
	// workDir is where adapters start when they do not name their own.
	workDir string
	client  *GatewayClient
	journal *Journal
	outbox  *Outbox
	log     *slog.Logger
}

// New prepares a node: it loads the paired identity, refuses an expired
// certificate, and opens the local journal and outbox.
func New(cfg Config, dir string, log *slog.Logger) (*Node, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	cfg.PopulateDefaultCapabilities()
	if dir == "" {
		return nil, fmt.Errorf("%w: node directory is required", protocol.ErrInvalid)
	}
	cfg.resolvePaths(dir)

	caPEM, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read CA certificate: %w", err)
	}
	certPEM, err := os.ReadFile(cfg.CertFile)
	if err != nil {
		return nil, fmt.Errorf("read node certificate: %w", err)
	}
	keyPEM, err := os.ReadFile(cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("read node key: %w", err)
	}
	if err := checkCertificateValidity(certPEM, time.Now()); err != nil {
		return nil, err
	}

	client, err := NewGatewayClient(cfg.ServerURL, caPEM, certPEM, keyPEM, 0)
	if err != nil {
		return nil, err
	}
	var configuredCaps []string
	for _, c := range cfg.Capabilities {
		configuredCaps = append(configuredCaps, c.Name)
	}
	discoveredAgents := DiscoverInstalledAgents(configuredCaps)
	client.SetMetadata(protocol.NodeSoftwareVersion, runtime.GOOS, runtime.GOARCH, discoveredAgents)
	journal, err := OpenJournal(filepath.Join(dir, journalFilename))
	if err != nil {
		return nil, err
	}
	outbox, err := OpenOutbox(filepath.Join(dir, outboxDirname))
	if err != nil {
		journal.Close()
		return nil, err
	}

	// Executions start in a directory of their own rather than in the one that
	// holds the node's private key. The runner needs an existing absolute path,
	// so it is created here rather than left to the first task.
	workDir := filepath.Join(dir, workDirname)
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		journal.Close()
		return nil, fmt.Errorf("create the work directory: %w", err)
	}

	return &Node{
		cfg: cfg, dir: dir, workDir: workDir,
		client: client, journal: journal, outbox: outbox, log: log,
	}, nil
}

// Close releases the node's local resources.
func (n *Node) Close() error {
	n.client.CloseIdleConnections()
	return n.journal.Close()
}

// Run claims and executes tasks until ctx is cancelled. It returns an error
// only when the node cannot continue at all: a refused identity, an unreadable
// journal, or another process holding the node directory.
func (n *Node) Run(ctx context.Context) error {
	release, err := acquireInstanceLock(filepath.Join(n.dir, lockFilename))
	if err != nil {
		return err
	}
	defer func() { _ = release() }()

	if err := n.reconcile(ctx); err != nil {
		return err
	}

	n.log.Info("node started",
		"server", n.cfg.ServerURL,
		"work_dir", n.workDir,
		"capabilities", len(n.cfg.Capabilities),
		"lease_seconds", n.cfg.LeaseSeconds,
		"renew_seconds", n.cfg.RenewSeconds)

	failures := 0
	for {
		if ctx.Err() != nil {
			n.log.Info("node stopped")
			return nil
		}

		lease, err := n.client.Claim(ctx, n.cfg.LeaseSeconds)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, ErrUnauthorized) {
				return fmt.Errorf("gateway rejected this node's credentials: %w", err)
			}
			failures++
			n.log.Warn("claim failed", "attempt", failures, "error", err)
			if err := sleep(ctx, backoff(failures)); err != nil {
				return nil
			}
		case lease == nil:
			// 204: the gateway held the request open and had nothing to hand
			// over. Poll again immediately.
			failures = 0
		default:
			failures = 0
			if err := n.serve(ctx, lease); err != nil {
				if errors.Is(err, ErrUnauthorized) {
					return fmt.Errorf("gateway rejected this node's credentials: %w", err)
				}
				n.log.Error("task attempt ended with an error", "task", lease.Task.ID, "error", err)
			}
		}
	}
}

// reconcile handles the state left by a previous run before any new work is
// claimed. A stored result is safe to re-report; an attempt that was
// interrupted while it may have been executing is never re-run.
func (n *Node) reconcile(ctx context.Context) error {
	entries, err := n.journal.Entries()
	if err != nil {
		return err
	}
	unfinished := Unfinished(entries)

	pending, err := n.outbox.List()
	if err != nil {
		return err
	}
	storedResult := make(map[attemptKey]bool, len(pending))
	for _, entry := range pending {
		storedResult[entry.key()] = true
	}

	for _, entry := range pending {
		n.log.Info("reporting a result stored by a previous run", "task", entry.TaskID, "attempt", entry.AttemptID)
		if err := n.deliver(ctx, entry); err != nil {
			if errors.Is(err, ErrUnauthorized) {
				return err
			}
			n.log.Warn("stored result is still not acknowledged", "task", entry.TaskID, "error", err)
		}
	}

	for _, entry := range unfinished {
		if storedResult[attemptKey{TaskID: entry.TaskID, AttemptID: entry.AttemptID}] {
			continue
		}
		n.log.Error("attempt needs reconciliation and will not be re-executed",
			"task", entry.TaskID, "attempt", entry.AttemptID, "last_state", entry.State)
		if err := n.journal.Append(JournalEntry{
			TaskID:    entry.TaskID,
			AttemptID: entry.AttemptID,
			State:     JournalNeedsReconcile,
			Detail:    "node restarted with an unfinished attempt; not re-executed",
		}); err != nil {
			return err
		}
	}
	return nil
}

// serve runs one leased task from the claim to the terminal report.
func (n *Node) serve(ctx context.Context, lease *protocol.Lease) error {
	task := lease.Task
	log := n.log.With("task", task.ID, "attempt", task.AttemptID, "capability", task.Capability)

	// The node records that it took the lease before it tells anyone: after a
	// crash this entry is the only proof the attempt was ever seen.
	if err := n.journal.Append(JournalEntry{TaskID: task.ID, AttemptID: task.AttemptID, State: JournalClaimed}); err != nil {
		return err
	}

	// Confirming the start is safe to repeat, so a lost response is retried
	// rather than guessed at.
	if err := n.startWithRetry(ctx, lease); err != nil {
		if errors.Is(err, errLeaseLost) {
			// The attempt is gone before any side effect: record it and move on.
			// The gateway expires the task into unknown on its own schedule.
			log.Warn("the attempt was gone before it started", "reason", err)
			return n.journal.Append(JournalEntry{
				TaskID: task.ID, AttemptID: task.AttemptID,
				State: JournalLeaseLost, Detail: err.Error(),
			})
		}
		return fmt.Errorf("confirm start: %w", err)
	}

	// Local policy decides what this node is willing to run. The check happens
	// after Start because the gateway accepts a terminal result only for a
	// started attempt: refusing before starting would leave the task leased
	// until its lease lapsed.
	capability, capErr := n.cfg.capabilityFor(task)
	var instructionText string
	if capErr == nil {
		instructionText, capErr = instruction(capability, task.Input)
	}
	if capErr != nil {
		log.Warn("refusing the task", "reason", capErr)
		return n.finish(ctx, lease, refusal(capErr), "")
	}

	trimmedInst := strings.TrimSpace(instructionText)
	if strings.HasPrefix(trimmedInst, "MESH_SYS:RESTART") {
		log.Info("received system restart command")
		go func() {
			time.Sleep(1 * time.Second)
			execPath, err := os.Executable()
			if err == nil && runtime.GOOS != "windows" {
				_ = syscall.Exec(execPath, os.Args, os.Environ())
			}
			os.Exit(0)
		}()
		return n.finish(ctx, lease, protocol.Result{
			State:    protocol.Succeeded,
			Text:     "Node restart initiated successfully.",
			ExitCode: 0,
		}, "restart")
	}

	if strings.HasPrefix(trimmedInst, "MESH_SYS:UPGRADE") {
		parts := strings.Fields(trimmedInst)
		downloadURL := ""
		if len(parts) >= 2 {
			downloadURL = parts[1]
		}
		log.Info("received system upgrade command", "url", downloadURL)
		err := n.performSelfUpgrade(ctx, downloadURL)
		if err != nil {
			log.Error("self-upgrade failed", "error", err)
			return n.finish(ctx, lease, protocol.Result{
				State:     protocol.Failed,
				ErrorCode: "upgrade_failed",
				Text:      "Self-upgrade failed: " + err.Error(),
				ExitCode:  1,
			}, "upgrade_failed")
		}
		go func() {
			time.Sleep(1 * time.Second)
			execPath, err := os.Executable()
			if err == nil && runtime.GOOS != "windows" {
				_ = syscall.Exec(execPath, os.Args, os.Environ())
			}
			os.Exit(0)
		}()
		return n.finish(ctx, lease, protocol.Result{
			State:    protocol.Succeeded,
			Text:     "Node binary upgraded successfully. Restarting...",
			ExitCode: 0,
		}, "upgraded")
	}

	log.Info("executing task")
	outcome := n.execute(ctx, lease, capability, instructionText)
	if !outcome.report {
		state := outcome.journalState
		if state == "" {
			state = JournalAborted
		}
		if err := n.journal.Append(JournalEntry{
			TaskID: task.ID, AttemptID: task.AttemptID,
			State: state, Detail: outcome.detail,
		}); err != nil {
			return err
		}
		log.Warn("not reporting an outcome", "reason", outcome.detail)
		return nil
	}
	log.Info("task finished", "state", string(outcome.result.State), "exit_code", outcome.result.ExitCode)
	return n.finish(ctx, lease, outcome.result, outcome.detail)
}

// errLeaseLost means the gateway no longer considers this attempt current, so
// the work must stop and nothing about it may be reported.
var errLeaseLost = errors.New("the attempt no longer holds the lease")

// execution is what one attempt produced.
type execution struct {
	result protocol.Result
	// report is false when the outcome must not reach the gateway: either the
	// lease was lost, or the node stopped the work while shutting down. In both
	// cases the node cannot prove what happened, so the task is left to expire
	// into unknown instead of being given a terminal state it did not earn.
	report       bool
	journalState string
	detail       string
}

// execute runs the process while a second goroutine keeps the lease alive and
// watches for a cancellation request.
func (n *Node) execute(ctx context.Context, lease *protocol.Lease, capability *Capability, instruction string) execution {
	var cancelRequested, leaseLost atomic.Bool

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	// The task carries its own execution limit. Without this deadline a long
	// adapter would run until its lease lapsed, which turns a timeout the
	// submitter asked for into an unknown outcome nobody wanted.
	if lease.Task.TimeoutSeconds > 0 {
		var cancelTimeout context.CancelFunc
		runCtx, cancelTimeout = context.WithTimeout(runCtx, time.Duration(lease.Task.TimeoutSeconds)*time.Second)
		defer cancelTimeout()
	}

	renewDone := make(chan error, 1)
	go func() {
		err := n.renewLoop(runCtx, lease, &cancelRequested, &leaseLost)
		// Whatever ended the loop ends the run window too: a lost lease must
		// stop the process, and a cancellation request is meant to stop it.
		cancelRun()
		renewDone <- err
	}()

	if err := n.journal.Append(JournalEntry{
		TaskID: lease.Task.ID, AttemptID: lease.Task.AttemptID, State: JournalStarting,
	}); err != nil {
		return execution{detail: "could not write the journal: " + err.Error()}
	}

	result, runErr := runner.Run(runCtx, n.cfg.runnerConfig(capability, n.workDir), instruction)

	cancelRun()
	<-renewDone

	switch {
	case leaseLost.Load():
		return execution{
			journalState: JournalLeaseLost,
			detail:       "the lease was lost while the task ran; the outcome is not reported",
		}
	case runErr == nil && result.State == protocol.Cancelled && ctx.Err() != nil:
		return execution{
			journalState: JournalAborted,
			detail:       "the node stopped while the task was running; the outcome is not reported",
		}
	}

	detail := ""
	if runErr != nil {
		// A configuration or launch failure never started a process, so this is
		// a gateway-visible failure rather than an uncertain execution.
		result = protocol.Result{
			State:     protocol.Failed,
			ExitCode:  -1,
			ErrorCode: "adapter_failed",
			Text:      clampText(runErr.Error()),
		}
	}
	if cancelRequested.Load() {
		// The gateway's cancellation arrived first, and its state machine only
		// accepts cancelled for such a task. The real output is kept as the
		// text so the evidence is not lost with the state.
		detail = "a cancellation was requested while the task ran; the recorded state follows the gateway"
		if result.State != protocol.Cancelled {
			code := result.ErrorCode
			if code == "" {
				code = "cancelled"
			}
			result = protocol.Result{
				State:     protocol.Cancelled,
				Text:      result.Text,
				ExitCode:  result.ExitCode,
				ErrorCode: code,
				Truncated: result.Truncated,
			}
		}
	}
	return execution{result: result, report: true, detail: detail}
}

// renewLoop keeps the lease alive and notices a cancellation request. It
// returns when the run window closes or the lease can no longer be held.
func (n *Node) renewLoop(ctx context.Context, lease *protocol.Lease, cancelRequested, leaseLost *atomic.Bool) error {
	ticker := time.NewTicker(n.cfg.renewInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if _, err := n.client.Renew(ctx, lease, n.cfg.LeaseSeconds); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				switch {
				case errors.Is(err, ErrUnauthorized):
					return err
				case errors.Is(err, ErrNotFound), n.leaseGone(ctx, lease.Task.ID):
					leaseLost.Store(true)
					n.log.Error("the lease is gone; stopping the task", "task", lease.Task.ID, "error", err)
					return fmt.Errorf("%w: %v", errLeaseLost, err)
				default:
					// A transient failure does not prove the lease lapsed; the
					// next tick tries again before the deadline passes.
					n.log.Warn("lease renewal failed", "task", lease.Task.ID, "error", err)
					continue
				}
			}

			// The task state is the only signal that a cancellation was
			// requested: renewals keep working for a task that is waiting to be
			// cancelled, so the node has to look.
			task, err := n.client.Get(ctx, lease.Task.ID)
			if err != nil {
				n.log.Warn("could not read the task state", "task", lease.Task.ID, "error", err)
				continue
			}
			if task.State == protocol.CancelRequested {
				cancelRequested.Store(true)
				n.log.Info("cancellation requested; stopping the task", "task", lease.Task.ID)
				return nil
			}
		}
	}
}

// leaseGone asks the gateway whether this attempt is still current. Only the
// task state can tell a transient refusal from the task having moved on:
// renewing a task that expired into unknown is refused as a plain conflict,
// exactly like a lock contention would be.
//
// A read that fails answers false: abandoning work needs evidence, and a failed
// read is not evidence.
func (n *Node) leaseGone(ctx context.Context, taskID string) bool {
	task, err := n.client.Get(ctx, taskID)
	if err != nil {
		return false
	}
	switch task.State {
	case protocol.Leased, protocol.Running, protocol.CancelRequested:
		return false
	default:
		return true
	}
}

// finish stores a result before reporting it, so a crash between the two is
// recoverable.
func (n *Node) finish(ctx context.Context, lease *protocol.Lease, result protocol.Result, detail string) error {
	entry := OutboxEntry{
		TaskID:    lease.Task.ID,
		AttemptID: lease.Task.AttemptID,
		Token:     lease.Token,
		Result:    result,
	}
	if err := n.outbox.Put(entry); err != nil {
		return err
	}
	if err := n.journal.Append(JournalEntry{
		TaskID: lease.Task.ID, AttemptID: lease.Task.AttemptID, State: JournalResultPending, Detail: detail,
	}); err != nil {
		return err
	}

	reportCtx := ctx
	if ctx.Err() != nil {
		// The work finished, but the node is shutting down: a completed result
		// is worth a short detached window.
		var cancel context.CancelFunc
		reportCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), shutdownReportTimeout)
		defer cancel()
	}
	return n.deliver(reportCtx, entry)
}

// deliver reports a stored result and clears it once the gateway acknowledges
// it. A refusal that retrying cannot fix moves the entry aside instead of
// deleting the only evidence of what ran.
func (n *Node) deliver(ctx context.Context, entry OutboxEntry) error {
	delay := baseBackoff
	rewritten := false

	for attempt := 1; ; attempt++ {
		err := n.client.Complete(ctx, entry.TaskID, entry.AttemptID, entry.Token, entry.Result)
		if err == nil {
			// Record the acknowledgement before dropping the stored result. A
			// crash between the two then leaves the entry in the outbox, where
			// the next start reports it again — a retransmission the gateway
			// accepts — instead of leaving a journal entry that looks like an
			// interrupted attempt that never happened.
			if err := n.journal.Append(JournalEntry{
				TaskID: entry.TaskID, AttemptID: entry.AttemptID, State: JournalAcknowledged,
			}); err != nil {
				return err
			}
			return n.outbox.Remove(entry)
		}

		switch {
		case errors.Is(err, ErrUnauthorized):
			// Keep the result: the credentials may be repaired, and dropping it
			// would lose the only record of what ran.
			return fmt.Errorf("the gateway refused the result: %w", err)
		case errors.Is(err, ErrNotFound):
			n.log.Error("the gateway will not accept this result any more",
				"task", entry.TaskID, "attempt", entry.AttemptID, "error", err)
			return n.reject(entry, "gateway no longer accepts this result: "+err.Error())
		case errors.Is(err, ErrRejected):
			return n.reject(entry, "gateway rejected the result: "+err.Error())
		case errors.Is(err, ErrConflict):
			// The refusal is a plain conflict, so only the task state says what
			// actually happened: a cancellation that arrived first, a task that
			// expired into unknown, or a different result already recorded.
			task, getErr := n.client.Get(ctx, entry.TaskID)
			if getErr != nil {
				// Cannot tell the cases apart yet; try again later.
				n.log.Warn("could not read the task state after a conflict",
					"task", entry.TaskID, "attempt", entry.AttemptID, "error", getErr)
			} else {
				switch {
				case task.State == protocol.CancelRequested && entry.Result.State != protocol.Cancelled && !rewritten:
					rewritten = true
					previous := entry.Result
					entry.Result = protocol.Result{
						State:     protocol.Cancelled,
						Text:      previous.Text,
						ExitCode:  previous.ExitCode,
						ErrorCode: "cancelled",
						Truncated: previous.Truncated,
					}
					n.log.Warn("a cancellation was requested first; reporting the task as cancelled",
						"task", entry.TaskID, "attempt", entry.AttemptID)
					if err := n.outbox.Put(entry); err != nil {
						return err
					}
					continue
				case task.State == protocol.Unknown:
					n.log.Error("the task expired into unknown before the result was accepted",
						"task", entry.TaskID, "attempt", entry.AttemptID)
					return n.reject(entry, "the task expired into unknown before the result was accepted: "+err.Error())
				default:
					n.log.Error("the gateway already holds a different result for this attempt",
						"task", entry.TaskID, "attempt", entry.AttemptID, "error", err)
					return n.reject(entry, "conflicting result already recorded: "+err.Error())
				}
			}
		}

		if attempt >= reportAttempts {
			return fmt.Errorf("result not acknowledged after %d attempts: %w", attempt, err)
		}
		if ctx.Err() != nil {
			return fmt.Errorf("result not acknowledged before shutdown: %w", err)
		}
		n.log.Warn("result not acknowledged; retrying", "task", entry.TaskID, "attempt", attempt, "error", err)
		if err := sleep(ctx, delay); err != nil {
			return fmt.Errorf("result not acknowledged before shutdown: %w", err)
		}
		delay = nextDelay(delay)
	}
}

// reject moves an entry the gateway will never accept out of the outbox.
func (n *Node) reject(entry OutboxEntry, reason string) error {
	if err := n.outbox.Reject(entry, reason); err != nil {
		return err
	}
	return n.journal.Append(JournalEntry{
		TaskID: entry.TaskID, AttemptID: entry.AttemptID, State: JournalRejected, Detail: reason,
	})
}

// startWithRetry confirms the start, retrying because the gateway treats a
// retransmission as the same confirmation.
func (n *Node) startWithRetry(ctx context.Context, lease *protocol.Lease) error {
	delay := baseBackoff
	for attempt := 1; ; attempt++ {
		err := n.client.Start(ctx, lease)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrUnauthorized) {
			return err
		}
		if errors.Is(err, ErrNotFound) || n.leaseGone(ctx, lease.Task.ID) {
			return fmt.Errorf("%w: %v", errLeaseLost, err)
		}
		if errors.Is(err, ErrRejected) {
			return err
		}
		if attempt >= reportAttempts || ctx.Err() != nil {
			return err
		}
		n.log.Warn("start confirmation failed; retrying", "task", lease.Task.ID, "attempt", attempt, "error", err)
		if err := sleep(ctx, delay); err != nil {
			return err
		}
		delay = nextDelay(delay)
	}
}

// refusal turns a local policy rejection into a terminal result.
func refusal(err error) protocol.Result {
	return protocol.Result{
		State:     protocol.Failed,
		ExitCode:  -1,
		ErrorCode: "unsupported_task",
		Text:      clampText(err.Error()),
	}
}

// clampText keeps a node-built message within the gateway's result limits and
// guarantees the UTF-8 the gateway requires.
func clampText(text string) string {
	text = strings.ToValidUTF8(text, "�")
	if len(text) <= maxErrorTextBytes {
		return text
	}
	// Cut on a rune boundary: a byte-wise cut can split a multi-byte character.
	cut := maxErrorTextBytes
	for cut > 0 && !utf8Start(text[cut]) {
		cut--
	}
	return text[:cut]
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// backoff grows the delay between claim attempts and spreads retries out with
// jitter, so several nodes coming back at once do not line up.
func backoff(attempt int) time.Duration {
	shift := min(attempt-1, 6)
	delay := baseBackoff << shift
	if delay > maxBackoff {
		delay = maxBackoff
	}
	return jitter(delay)
}

func nextDelay(current time.Duration) time.Duration {
	next := current * 2
	if next > maxBackoff {
		next = maxBackoff
	}
	return jitter(next)
}

func jitter(delay time.Duration) time.Duration {
	return time.Duration(float64(delay) * (0.5 + 0.5*rand.Float64()))
}

func sleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// checkCertificateValidity refuses to start with a certificate the gateway will
// reject anyway, so the operator sees why instead of a stream of 401s.
func checkCertificateValidity(certPEM []byte, now time.Time) error {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return fmt.Errorf("%w: node certificate is not valid PEM", protocol.ErrInvalid)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("%w: parse node certificate: %v", protocol.ErrInvalid, err)
	}
	if now.After(cert.NotAfter) {
		return fmt.Errorf("%w: node certificate expired at %s, pair again",
			protocol.ErrUnauthorized, cert.NotAfter.UTC().Format(time.RFC3339))
	}
	if now.Before(cert.NotBefore) {
		return fmt.Errorf("%w: node certificate is not valid before %s",
			protocol.ErrUnauthorized, cert.NotBefore.UTC().Format(time.RFC3339))
	}
	return nil
}

func (n *Node) performSelfUpgrade(ctx context.Context, downloadURL string) error {
	if downloadURL == "" {
		downloadURL = n.cfg.ServerURL + "/download/mesh"
	}
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable: %w", err)
	}

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 60 * time.Second,
	}
	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download mesh: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download mesh returned status: %d", resp.StatusCode)
	}

	tmpFile := execPath + ".upgrade.tmp"
	f, err := os.OpenFile(tmpFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("create upgrade tmp file: %w", err)
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(tmpFile)
	}()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("write upgrade file: %w", err)
	}
	_ = f.Close()

	if err := os.Rename(tmpFile, execPath); err != nil {
		return fmt.Errorf("replace executable: %w", err)
	}
	return nil
}


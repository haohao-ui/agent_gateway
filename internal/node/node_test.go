package node

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"agent-gateway/internal/identity"
	"agent-gateway/internal/protocol"
	"agent-gateway/internal/runner"
)

// syncBuffer collects log output written by the node goroutine so a test can
// wait for a lifecycle message without racing the writer.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestNodeExecutesTaskAndReportsResult(t *testing.T) {
	gw := startTestGateway(t)
	nodeDir := t.TempDir()
	state := pairTestNode(t, gw, nodeDir)

	cfg := testNodeConfig(t, gw.ts.URL, nodeDir, helperCapability(t, "echo-argv"))
	n, err := New(cfg, nodeDir, testLogger())
	if err != nil {
		t.Fatalf("new node: %v", err)
	}
	t.Cleanup(func() { _ = n.Close() })
	runNode(t, n)

	client := gatewayClient(t, nodeDir, gw.ts.URL)
	task := submitTask(t, client, state.NodeID, `{"prompt":"hello world"}`)

	final := waitForTask(t, client, task.ID, protocol.Succeeded, 20*time.Second)
	if final.Result == nil {
		t.Fatal("succeeded task has no result")
	}
	// The adapter receives the prompt as one argv element, verbatim.
	if !strings.Contains(final.Result.Text, "ARG[0]=hello world") {
		t.Fatalf("result text %q does not show the instruction in argv", final.Result.Text)
	}
	if final.Result.ExitCode != 0 {
		t.Fatalf("exit code is %d, want 0", final.Result.ExitCode)
	}

	// Server completion precedes receiving the HTTP ACK and durable local
	// cleanup. Wait for both local effects instead of racing that handoff.
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, err := n.journal.Entries()
		if err != nil {
			t.Fatalf("read journal: %v", err)
		}
		pending, err := n.outbox.List()
		if err != nil {
			t.Fatalf("list outbox: %v", err)
		}
		state := lastState(entries, task.ID, final.AttemptID)
		if state == JournalAcknowledged && len(pending) == 0 {
			break
		}
		if time.Now().After(deadline) {
			// Do not print outbox entries: they contain lease credentials.
			t.Fatalf("local ACK cleanup incomplete: journal=%q pending=%d", state, len(pending))
		}
		time.Sleep(10 * time.Millisecond)
	}

}

func TestNodeKeepsLeaseAliveWhileRunning(t *testing.T) {
	gw := startTestGateway(t)
	nodeDir := t.TempDir()
	state := pairTestNode(t, gw, nodeDir)

	cfg := testNodeConfig(t, gw.ts.URL, nodeDir, helperCapability(t, "sleep", "2500"))
	cfg.LeaseSeconds = 6
	cfg.RenewSeconds = 1

	n, err := New(cfg, nodeDir, testLogger())
	if err != nil {
		t.Fatalf("new node: %v", err)
	}
	t.Cleanup(func() { _ = n.Close() })
	runNode(t, n)

	client := gatewayClient(t, nodeDir, gw.ts.URL)
	task := submitTask(t, client, state.NodeID, `{"prompt":"ignore"}`)

	// Wait until the task is running, then watch the deadline move: a lease that
	// is never extended would keep the same expiry for the whole run.
	running := waitForTask(t, client, task.ID, protocol.Running, 20*time.Second)
	if running.LeaseExpiresAt == nil {
		t.Fatal("running task has no lease deadline")
	}
	first := *running.LeaseExpiresAt

	renewed := false
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		current := getTask(t, client, task.ID)
		if current.LeaseExpiresAt != nil && current.LeaseExpiresAt.After(first) {
			renewed = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !renewed {
		t.Fatal("the lease deadline never moved while the task was running")
	}

	final := waitForTask(t, client, task.ID, protocol.Succeeded, 20*time.Second)
	if final.Result == nil || final.Result.ExitCode != 0 {
		t.Fatalf("unexpected result: %+v", final.Result)
	}
}

func TestNodeLeaseLossStopsTheTaskAndDoesNotReport(t *testing.T) {
	gw := startTestGateway(t)
	nodeDir := t.TempDir()
	state := pairTestNode(t, gw, nodeDir)

	// A long adapter: the test needs the task to still be running when the
	// lease is taken away.
	cfg := testNodeConfig(t, gw.ts.URL, nodeDir, helperCapability(t, "sleep", "60000"))
	cfg.LeaseSeconds = 4
	cfg.RenewSeconds = 1

	n, err := New(cfg, nodeDir, testLogger())
	if err != nil {
		t.Fatalf("new node: %v", err)
	}
	t.Cleanup(func() { _ = n.Close() })
	runNode(t, n)

	client := gatewayClient(t, nodeDir, gw.ts.URL)
	task := submitTask(t, client, state.NodeID, `{"prompt":"ignore"}`)
	waitForTask(t, client, task.ID, protocol.Running, 20*time.Second)

	// Take the lease away as a stalled node would experience it, then run the
	// sweep the gateway performs: the attempt must become unknown, not requeued.
	if _, err := gw.store.Expire(context.Background(), time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("expire leases: %v", err)
	}
	unknown := waitForTask(t, client, task.ID, protocol.Unknown, 20*time.Second)
	if unknown.Result != nil {
		t.Fatalf("an unknown task must not carry a result, got %+v", unknown.Result)
	}

	// The node notices the loss on its next renewal, kills the process and
	// records why, so the assertion waits for that rather than racing it.
	waitFor(t, "the node to record the lost lease", 30*time.Second, func() bool {
		entries, err := n.journal.Entries()
		return err == nil && lastState(entries, task.ID, unknown.AttemptID) == JournalLeaseLost
	})

	entries, err := n.journal.Entries()
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	// The attempt must not be executed twice: one starting entry, and nothing
	// left waiting to be reported.
	started := 0
	for _, entry := range entries {
		if entry.State == JournalStarting {
			started++
		}
	}
	if started != 1 {
		t.Fatalf("the adapter was started %d times, want exactly 1", started)
	}
	pending, err := n.outbox.List()
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("a lost lease must not produce a reportable result, got %+v", pending)
	}
}

func TestNodeStopsOnCancellationRequest(t *testing.T) {
	gw := startTestGateway(t)
	nodeDir := t.TempDir()
	state := pairTestNode(t, gw, nodeDir)

	cfg := testNodeConfig(t, gw.ts.URL, nodeDir, helperCapability(t, "sleep", "60000"))
	cfg.LeaseSeconds = 6
	cfg.RenewSeconds = 1

	n, err := New(cfg, nodeDir, testLogger())
	if err != nil {
		t.Fatalf("new node: %v", err)
	}
	t.Cleanup(func() { _ = n.Close() })
	runNode(t, n)

	client := gatewayClient(t, nodeDir, gw.ts.URL)
	task := submitTask(t, client, state.NodeID, `{"prompt":"ignore"}`)
	waitForTask(t, client, task.ID, protocol.Running, 20*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.Cancel(ctx, task.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// The node learns about the cancellation by reading the task state, stops
	// the process, and reports cancelled.
	final := waitForTask(t, client, task.ID, protocol.Cancelled, 30*time.Second)
	if final.Result == nil || final.Result.State != protocol.Cancelled {
		t.Fatalf("unexpected result: %+v", final.Result)
	}
}

func TestNodeRefusesCapabilityItDoesNotServe(t *testing.T) {
	gw := startTestGateway(t)
	nodeDir := t.TempDir()
	state := pairTestNode(t, gw, nodeDir)

	capability := helperCapability(t, "echo-argv")
	cfg := testNodeConfig(t, gw.ts.URL, nodeDir, capability)
	n, err := New(cfg, nodeDir, testLogger())
	if err != nil {
		t.Fatalf("new node: %v", err)
	}
	t.Cleanup(func() { _ = n.Close() })
	runNode(t, n)

	client := gatewayClient(t, nodeDir, gw.ts.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task, err := client.Submit(ctx, protocol.SubmitRequest{
		NodeID:            state.NodeID,
		Capability:        "something.else",
		CapabilityVersion: 1,
		Input:             json.RawMessage(`{"prompt":"x"}`),
		TimeoutSeconds:    30,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	final := waitForTask(t, client, task.ID, protocol.Failed, 20*time.Second)
	if final.Result == nil || final.Result.ErrorCode != "unsupported_task" {
		t.Fatalf("unexpected result: %+v", final.Result)
	}
	// Nothing may have been launched for a task this node does not serve.
	entries, err := n.journal.Entries()
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	for _, entry := range entries {
		if entry.State == JournalStarting {
			t.Fatal("a refused task reached the starting state")
		}
	}
}

func TestNodeReportsAdapterFailure(t *testing.T) {
	gw := startTestGateway(t)
	nodeDir := t.TempDir()
	state := pairTestNode(t, gw, nodeDir)

	cfg := testNodeConfig(t, gw.ts.URL, nodeDir, helperCapability(t, "exit", "3"))
	n, err := New(cfg, nodeDir, testLogger())
	if err != nil {
		t.Fatalf("new node: %v", err)
	}
	t.Cleanup(func() { _ = n.Close() })
	runNode(t, n)

	client := gatewayClient(t, nodeDir, gw.ts.URL)
	task := submitTask(t, client, state.NodeID, `{"prompt":"ignore"}`)

	final := waitForTask(t, client, task.ID, protocol.Failed, 20*time.Second)
	if final.Result == nil || final.Result.ExitCode != 3 {
		t.Fatalf("unexpected result: %+v", final.Result)
	}
}

func TestNodeRefusesToRunTwiceInOneDirectory(t *testing.T) {
	gw := startTestGateway(t)
	nodeDir := t.TempDir()
	pairTestNode(t, gw, nodeDir)

	cfg := testNodeConfig(t, gw.ts.URL, nodeDir, helperCapability(t, "echo-argv"))
	// The readiness signal must not contend for the instance lock: probing the
	// lock steals it from the node's own first acquisition, the first loop then
	// exits with a conflict and nobody holds it. Run logs "node started" only
	// after it took the lock and reconciled, which is safe on a loaded machine.
	logs := &syncBuffer{}
	first, err := New(cfg, nodeDir, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatalf("new node: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	runNode(t, first)

	waitFor(t, "the first node to start and take the instance lock", 10*time.Second, func() bool {
		return strings.Contains(logs.String(), "node started")
	})

	second, err := New(cfg, nodeDir, testLogger())
	if err != nil {
		t.Fatalf("new node: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	err = second.Run(context.Background())
	if !errors.Is(err, protocol.ErrConflict) {
		t.Fatalf("second instance returned %v, want a conflict on the instance lock", err)
	}
}

func TestNodeRejectsExpiredCertificate(t *testing.T) {
	gw := startTestGateway(t)
	nodeDir := t.TempDir()
	pairTestNode(t, gw, nodeDir)

	certPEM, err := os.ReadFile(filepath.Join(nodeDir, NodeCertFile))
	if err != nil {
		t.Fatalf("read certificate: %v", err)
	}
	if err := checkCertificateValidity(certPEM, time.Now().AddDate(1, 0, 0)); !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("expired certificate returned %v, want an unauthorized error", err)
	}
	if err := checkCertificateValidity(certPEM, time.Now()); err != nil {
		t.Fatalf("fresh certificate rejected: %v", err)
	}
}

func TestPairRefusesToReplaceAnExistingIdentity(t *testing.T) {
	gw := startTestGateway(t)
	nodeDir := t.TempDir()
	pairTestNode(t, gw, nodeDir)

	invitation, err := gw.ca.GenerateInvitation(5 * time.Minute)
	if err != nil {
		t.Fatalf("generate invitation: %v", err)
	}
	_, err = Pair(context.Background(), PairConfig{
		ServerURL:       gw.ts.URL,
		InvitationToken: invitation.Token,
		CACertPEM:       gw.ca.CACertPEM(),
		Dir:             nodeDir,
	})
	if !errors.Is(err, protocol.ErrConflict) {
		t.Fatalf("second pairing returned %v, want a conflict", err)
	}
}

func TestPairRejectsAGatewayWithAnotherCA(t *testing.T) {
	gw := startTestGateway(t)

	// A server the node trusts for TLS, but which answers with a foreign CA:
	// this is what a downgrade of the trust anchor would look like.
	otherCA, err := identity.LoadOrGenerateCA(t.TempDir())
	if err != nil {
		t.Fatalf("create other CA: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/pair", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protocol.PairResponse{
			NodeID:        "node-foreign",
			CertPEM:       "-----BEGIN CERTIFICATE-----\n",
			CACertPEM:     string(otherCA.CACertPEM()),
			ServerVersion: protocol.CurrentProtocolVersion,
		})
	})

	serverCert, err := gw.ca.GenerateServerCertificate([]string{"127.0.0.1", "localhost"})
	if err != nil {
		t.Fatalf("generate server certificate: %v", err)
	}
	ts := httptest.NewUnstartedServer(mux)
	ts.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		NextProtos:   []string{"h2", "http/1.1"},
		MinVersion:   tls.VersionTLS13,
	}
	ts.EnableHTTP2 = true
	ts.StartTLS()
	t.Cleanup(ts.Close)

	_, err = Pair(context.Background(), PairConfig{
		ServerURL:       ts.URL,
		InvitationToken: "invitation",
		CACertPEM:       gw.ca.CACertPEM(),
		Dir:             t.TempDir(),
	})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("pairing against a mismatched CA returned %v, want an unauthorized error", err)
	}
}

func lastState(entries []JournalEntry, taskID, attemptID string) string {
	state := ""
	for _, entry := range entries {
		if entry.TaskID == taskID && entry.AttemptID == attemptID {
			state = entry.State
		}
	}
	return state
}

func TestReconcileRefusesToRerunAnInterruptedAttempt(t *testing.T) {
	gw := startTestGateway(t)
	nodeDir := t.TempDir()
	pairTestNode(t, gw, nodeDir)

	cfg := testNodeConfig(t, gw.ts.URL, nodeDir, helperCapability(t, "echo-argv"))
	n, err := New(cfg, nodeDir, testLogger())
	if err != nil {
		t.Fatalf("new node: %v", err)
	}
	t.Cleanup(func() { _ = n.Close() })

	// This is exactly what a crash in the middle of an attempt leaves behind:
	// the node had taken the lease and was about to launch, and no result was
	// ever stored.
	for _, state := range []string{JournalClaimed, JournalStarting} {
		if err := n.journal.Append(JournalEntry{TaskID: "task-crashed", AttemptID: "attempt-crashed", State: state}); err != nil {
			t.Fatalf("seed journal: %v", err)
		}
	}

	if err := n.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	entries, err := n.journal.Entries()
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	if got := lastState(entries, "task-crashed", "attempt-crashed"); got != JournalNeedsReconcile {
		t.Fatalf("journal ended in %q, want %q", got, JournalNeedsReconcile)
	}
	// Nothing may have been launched again, and no result may have been invented.
	started := 0
	for _, entry := range entries {
		if entry.State == JournalStarting {
			started++
		}
	}
	if started != 1 {
		t.Fatalf("the interrupted attempt was started %d times, want exactly 1", started)
	}
	if pending, err := n.outbox.List(); err != nil || len(pending) != 0 {
		t.Fatalf("reconciliation invented a result: %+v (err %v)", pending, err)
	}
}

func TestReconcileReportsAStoredResult(t *testing.T) {
	gw := startTestGateway(t)
	nodeDir := t.TempDir()
	state := pairTestNode(t, gw, nodeDir)

	cfg := testNodeConfig(t, gw.ts.URL, nodeDir, helperCapability(t, "echo-argv"))
	n, err := New(cfg, nodeDir, testLogger())
	if err != nil {
		t.Fatalf("new node: %v", err)
	}
	t.Cleanup(func() { _ = n.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	client := gatewayClient(t, nodeDir, gw.ts.URL)
	task := submitTask(t, client, state.NodeID, `{"prompt":"ignore"}`)

	// Take a real lease, then act like a node that finished the work and died
	// before the acknowledgement arrived: the result is on disk, the gateway
	// has never seen it.
	lease, err := client.Claim(ctx, 60)
	if err != nil || lease == nil {
		t.Fatalf("claim: %v (lease %+v)", err, lease)
	}
	if err := client.Start(ctx, lease); err != nil {
		t.Fatalf("start: %v", err)
	}
	stored := OutboxEntry{
		TaskID:    lease.Task.ID,
		AttemptID: lease.Task.AttemptID,
		Token:     lease.Token,
		Result:    protocol.Result{State: protocol.Succeeded, Text: "recovered result", ExitCode: 0},
	}
	if err := n.outbox.Put(stored); err != nil {
		t.Fatalf("store the result: %v", err)
	}
	if err := n.journal.Append(JournalEntry{TaskID: stored.TaskID, AttemptID: stored.AttemptID, State: JournalResultPending}); err != nil {
		t.Fatalf("journal: %v", err)
	}

	if err := n.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	final := waitForTask(t, client, task.ID, protocol.Succeeded, 20*time.Second)
	if final.Result == nil || final.Result.Text != "recovered result" {
		t.Fatalf("unexpected result: %+v", final.Result)
	}
	if pending, err := n.outbox.List(); err != nil || len(pending) != 0 {
		t.Fatalf("the reported entry is still pending: %+v (err %v)", pending, err)
	}
	entries, err := n.journal.Entries()
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	if got := lastState(entries, stored.TaskID, stored.AttemptID); got != JournalAcknowledged {
		t.Fatalf("journal ended in %q, want %q", got, JournalAcknowledged)
	}
}

func TestNodeStopsTaskAtItsOwnDeadline(t *testing.T) {
	gw := startTestGateway(t)
	nodeDir := t.TempDir()
	state := pairTestNode(t, gw, nodeDir)

	// The adapter would run for a minute; the task asks for two seconds. The
	// lease is long enough that only the task's own deadline can end this.
	cfg := testNodeConfig(t, gw.ts.URL, nodeDir, helperCapability(t, "sleep", "60000"))
	cfg.LeaseSeconds = 30
	cfg.RenewSeconds = 3

	n, err := New(cfg, nodeDir, testLogger())
	if err != nil {
		t.Fatalf("new node: %v", err)
	}
	t.Cleanup(func() { _ = n.Close() })
	runNode(t, n)

	client := gatewayClient(t, nodeDir, gw.ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	submitted := time.Now()
	task, err := client.Submit(ctx, protocol.SubmitRequest{
		NodeID:            state.NodeID,
		Capability:        "agent.run",
		CapabilityVersion: 1,
		Input:             json.RawMessage(`{"prompt":"ignore"}`),
		TimeoutSeconds:    2,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	final := waitForTask(t, client, task.ID, protocol.Failed, 20*time.Second)
	if final.Result == nil {
		t.Fatal("a failed task carries no result")
	}
	if final.Result.ErrorCode != runner.ErrorCodeTimeout {
		t.Fatalf("error code is %q, want %q", final.Result.ErrorCode, runner.ErrorCodeTimeout)
	}
	// The adapter was stopped because the deadline passed, not because the lease
	// happened to lapse much later.
	if elapsed := time.Since(submitted); elapsed > 15*time.Second {
		t.Fatalf("the task ran for %s instead of stopping at its deadline", elapsed)
	}
}

func TestNodeHandlesLargeUnicodeOutput(t *testing.T) {
	gw := startTestGateway(t)
	nodeDir := t.TempDir()
	state := pairTestNode(t, gw, nodeDir)

	// The adapter writes about 240 KB of Chinese text: more than the 64 KiB a
	// result may carry, and every character multi-byte.
	cfg := testNodeConfig(t, gw.ts.URL, nodeDir,
		helperCapability(t, "repeat", "中文输出", "20000"))
	n, err := New(cfg, nodeDir, testLogger())
	if err != nil {
		t.Fatalf("new node: %v", err)
	}
	t.Cleanup(func() { _ = n.Close() })
	runNode(t, n)

	client := gatewayClient(t, nodeDir, gw.ts.URL)
	task := submitTask(t, client, state.NodeID, `{"prompt":"ignore"}`)

	final := waitForTask(t, client, task.ID, protocol.Succeeded, 30*time.Second)
	if final.Result == nil {
		t.Fatal("succeeded task carries no result")
	}
	if !final.Result.Truncated {
		t.Fatal("a result larger than the retained tail must be marked truncated")
	}
	if len(final.Result.Text) > 64<<10 {
		t.Fatalf("result text is %d bytes, over the gateway limit", len(final.Result.Text))
	}
	if !utf8.ValidString(final.Result.Text) {
		t.Fatal("the truncation split a multi-byte character")
	}
	// What survived must be the tail of the output, not an empty or garbled run.
	if !strings.Contains(final.Result.Text, "中文输出") {
		t.Fatalf("unexpected truncated text: %q", firstBytes(final.Result.Text, 80))
	}
}

func firstBytes(text string, n int) string {
	if len(text) <= n {
		return text
	}
	return text[:n]
}

// stubTransport answers every request with a fixed status and body, so a client
// path can be exercised without a gateway.
type stubTransport struct {
	status int
	body   string
}

func (t stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: t.status,
		Body:       io.NopCloser(strings.NewReader(t.body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func TestClaimTreatsNoContentAsNoWork(t *testing.T) {
	gw := startTestGateway(t)
	nodeDir := t.TempDir()
	pairTestNode(t, gw, nodeDir)

	client := gatewayClient(t, nodeDir, gw.ts.URL)
	// In-package test: the idle answer is the one that must not be decoded.
	client.http = &http.Client{Transport: stubTransport{status: http.StatusNoContent}}

	lease, err := client.Claim(context.Background(), 30)
	if err != nil {
		t.Fatalf("a 204 from the claim poll was reported as an error: %v", err)
	}
	if lease != nil {
		t.Fatalf("expected no lease, got %+v", lease)
	}
}

func TestNodePicksUpWorkAfterIdling(t *testing.T) {
	gw := startTestGateway(t)
	nodeDir := t.TempDir()
	state := pairTestNode(t, gw, nodeDir)

	cfg := testNodeConfig(t, gw.ts.URL, nodeDir, helperCapability(t, "echo-argv"))
	cfg.LeaseSeconds = 30
	cfg.RenewSeconds = 5

	n, err := New(cfg, nodeDir, testLogger())
	if err != nil {
		t.Fatalf("new node: %v", err)
	}
	t.Cleanup(func() { _ = n.Close() })
	runNode(t, n)

	// Let the loop park in a poll against an empty queue first, so the task
	// below has to arrive through the wake-up path rather than the first claim.
	// (The 204 half of the idle path is guarded by TestClaimTreatsNoContentAsNoWork.)
	time.Sleep(300 * time.Millisecond)

	client := gatewayClient(t, nodeDir, gw.ts.URL)
	submitted := time.Now()
	task := submitTask(t, client, state.NodeID, `{"prompt":"after idling"}`)

	final := waitForTask(t, client, task.ID, protocol.Succeeded, 20*time.Second)
	if final.Result == nil || !strings.Contains(final.Result.Text, "after idling") {
		t.Fatalf("unexpected result: %+v", final.Result)
	}
	// The point of the long poll: work is taken as soon as it is queued, not
	// after some backoff expires.
	if elapsed := time.Since(submitted); elapsed > 5*time.Second {
		t.Fatalf("a queued task waited %s to be claimed", elapsed)
	}
}

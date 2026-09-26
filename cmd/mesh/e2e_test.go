package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"agent-gateway/internal/identity"
	"agent-gateway/internal/node"
	"agent-gateway/internal/protocol"
)

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// echoExecutable finds a standalone echo. The smoke test runs a real process, so
// it needs an executable that exists on the host; Windows has no such binary,
// and the test says so instead of pretending to pass.
func echoExecutable(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no standalone echo executable on Windows; the adapter path is covered by internal/node tests")
	}
	for _, candidate := range []string{"/bin/echo", "/usr/bin/echo"} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	t.Skip("no echo executable on this host")
	return ""
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func waitForTask(t *testing.T, client *node.GatewayClient, taskID string, want protocol.State, timeout time.Duration) protocol.Task {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var last protocol.Task
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		task, err := client.Get(ctx, taskID)
		cancel()
		if err != nil {
			t.Fatalf("get task: %v", err)
		}
		last = task
		if task.State == want {
			return task
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("task %s is %s after %s, want %s", taskID, last.State, timeout, want)
	return last
}

// TestGatewayEndToEnd runs the whole loop in one process: a real gateway over
// HTTP/2 mTLS, a real paired node, and a real subprocess executed through the
// runner. It is the same code path the CLI runs.
func TestGatewayEndToEnd(t *testing.T) {
	echo := echoExecutable(t)

	gatewayDir := t.TempDir()
	nodeDir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ready := make(chan gatewayInfo, 1)
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- serveGateway(ctx, gatewayOptions{
			Addr:        "127.0.0.1:0",
			DataDir:     gatewayDir,
			Invitations: 1,
			InviteTTL:   5 * time.Minute,
			ExpireEvery: 200 * time.Millisecond,
			Log:         testLogger(),
		}, func(info gatewayInfo) { ready <- info })
	}()

	info := <-ready
	if !strings.HasPrefix(info.Addr, "https://") {
		t.Fatalf("gateway address is not https: %q", info.Addr)
	}
	if len(info.Invitations) != 1 {
		t.Fatalf("expected one invitation, got %d", len(info.Invitations))
	}

	// 1. Pair, trusting only the CA file the gateway printed.
	state, err := node.Pair(ctx, node.PairConfig{
		ServerURL:       info.Addr,
		InvitationToken: info.Invitations[0].Token,
		CACertPEM:       readFile(t, info.CACertPath),
		Dir:             nodeDir,
	})
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	if state.NodeID == "" {
		t.Fatal("pairing returned no node id")
	}

	// 2. Start the node loop with a real adapter.
	worker, err := node.New(node.Config{
		ServerURL:    info.Addr,
		CertFile:     node.NodeCertFile,
		KeyFile:      node.NodeKeyFile,
		CAFile:       node.NodeCAFile,
		LeaseSeconds: 10,
		RenewSeconds: 3,
		Capabilities: []node.Capability{{
			Name:    "agent.run",
			Version: 1,
			Adapter: node.Adapter{Executable: echo, Args: []string{"{instruction}"}, WorkDir: t.TempDir()},
		}},
	}, nodeDir, testLogger())
	if err != nil {
		t.Fatalf("new node: %v", err)
	}
	defer worker.Close()

	nodeCtx, stopNode := context.WithCancel(ctx)
	defer stopNode()
	nodeErr := make(chan error, 1)
	go func() { nodeErr <- worker.Run(nodeCtx) }()

	// 3. Queue work as the node's own identity and watch it complete.
	client, err := node.NewGatewayClient(info.Addr,
		readFile(t, filepath.Join(nodeDir, node.NodeCAFile)),
		readFile(t, filepath.Join(nodeDir, node.NodeCertFile)),
		readFile(t, filepath.Join(nodeDir, node.NodeKeyFile)), 5*time.Second)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer client.CloseIdleConnections()

	const prompt = "hello from the end-to-end test"
	task, err := client.Submit(ctx, protocol.SubmitRequest{
		NodeID:            state.NodeID,
		Capability:        "agent.run",
		CapabilityVersion: 1,
		Input:             json.RawMessage(fmt.Sprintf(`{"prompt":%q}`, prompt)),
		TimeoutSeconds:    60,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	final := waitForTask(t, client, task.ID, protocol.Succeeded, 30*time.Second)
	if final.Result == nil {
		t.Fatal("succeeded task carries no result")
	}
	if !strings.Contains(final.Result.Text, prompt) {
		t.Fatalf("result text %q does not contain the prompt", final.Result.Text)
	}

	// 4. Shut down cleanly: the node stops, the gateway closes its listener.
	stopNode()
	select {
	case err := <-nodeErr:
		if err != nil {
			t.Fatalf("node loop returned an error: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the node loop did not stop")
	}

	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("gateway returned an error: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the gateway did not shut down")
	}
}

// TestStarterConfigIsLoadable guards the documented workflow: the file written
// by 'mesh pair' has to be accepted by the loader 'mesh node' uses.
func TestStarterConfigIsLoadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.json")
	if err := writeStarterConfig(path, "https://127.0.0.1:8443"); err != nil {
		t.Fatalf("write starter configuration: %v", err)
	}

	cfg, err := node.LoadConfig(path)
	if err != nil {
		t.Fatalf("the configuration written by 'mesh pair' does not load: %v", err)
	}
	if cfg.ServerURL != "https://127.0.0.1:8443" {
		t.Fatalf("server url is %q", cfg.ServerURL)
	}
	if len(cfg.Capabilities) != 1 || cfg.Capabilities[0].Name != "agent.run" {
		t.Fatalf("unexpected capabilities: %+v", cfg.Capabilities)
	}
	if cfg.Capabilities[0].Adapter.Executable == "" {
		t.Fatal("the starter capability has no adapter executable")
	}
}

// TestInvitationMintedWhileTheGatewayRuns is the operator story the API could
// not express before: add a machine to a gateway that is already up.
func TestInvitationMintedWhileTheGatewayRuns(t *testing.T) {
	gatewayDir := t.TempDir()
	nodeDir := t.TempDir()
	secondNodeDir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ready := make(chan gatewayInfo, 1)
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- serveGateway(ctx, gatewayOptions{
			Addr: "127.0.0.1:0",
			// No invitation at startup: the only token in this test is the one
			// minted while the gateway is serving.
			Invitations: 0,
			DataDir:     gatewayDir,
			ExpireEvery: 200 * time.Millisecond,
			Log:         testLogger(),
		}, func(info gatewayInfo) { ready <- info })
	}()
	info := <-ready

	invitation, err := identity.IssueInvitation(gatewayDir, 5*time.Minute, nil)
	if err != nil {
		t.Fatalf("mint an invitation for a running gateway: %v", err)
	}

	_, err = node.Pair(ctx, node.PairConfig{
		ServerURL:       info.Addr,
		InvitationToken: invitation.Token,
		CACertPEM:       readFile(t, info.CACertPath),
		Dir:             nodeDir,
	})
	if err != nil {
		t.Fatalf("pair with an invitation minted after startup: %v", err)
	}

	// The token is one-shot: a second machine cannot reuse it.
	_, err = node.Pair(ctx, node.PairConfig{
		ServerURL:       info.Addr,
		InvitationToken: invitation.Token,
		CACertPEM:       readFile(t, info.CACertPath),
		Dir:             secondNodeDir,
	})
	if !errors.Is(err, node.ErrUnauthorized) {
		t.Fatalf("reused invitation returned %v, want unauthorized", err)
	}

	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("gateway returned an error: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the gateway did not shut down")
	}
}

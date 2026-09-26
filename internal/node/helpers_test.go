package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-gateway/internal/httpapi"
	"agent-gateway/internal/identity"
	"agent-gateway/internal/protocol"
	"agent-gateway/internal/taskstore"
)

// Helper process protocol
//
// Every execution test re-runs this test binary as the controlled child, the
// same way internal/runner does. The mode travels in argv because the runner's
// environment whitelist would erase an environment variable.
const helperPrefix = "helper:"

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], helperPrefix) {
		os.Exit(runHelper(strings.TrimPrefix(os.Args[1], helperPrefix), os.Args[2:]))
	}
	os.Exit(m.Run())
}

// runHelper implements the child behaviour. An unknown mode must fail loudly:
// a silent success would turn a typo into a false pass.
func runHelper(mode string, args []string) int {
	switch mode {
	case "echo-argv":
		for i, arg := range args {
			fmt.Printf("ARG[%d]=%s\n", i, arg)
		}
		return 0

	case "stdin-copy":
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return 2
		}
		if _, err := os.Stdout.Write(b); err != nil {
			return 2
		}
		return 0

	case "repeat":
		// args: text, count. Writes far more than any retained tail so the
		// truncation path can be exercised with multi-byte characters.
		chunk := strings.Repeat(args[0], atoi(args, 1))
		if _, err := io.WriteString(os.Stdout, chunk); err != nil {
			return 2
		}
		return 0

	case "exit":
		return atoi(args, 0)

	case "sleep":
		time.Sleep(millis(args, 0))
		return 0
	}

	fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
	return 9
}

func atoi(args []string, i int) int {
	if i >= len(args) {
		return 0
	}
	n, err := strconv.Atoi(args[i])
	if err != nil {
		return 0
	}
	return n
}

func millis(args []string, i int) time.Duration {
	return time.Duration(atoi(args, i)) * time.Millisecond
}

// testBinary returns the absolute path of this test binary, which acts as the
// controlled adapter executable.
func testBinary(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	if !filepath.IsAbs(exe) {
		t.Fatalf("test binary path is not absolute: %q", exe)
	}
	return exe
}

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// testGateway is a real gateway: real SQLite, real CA, real HTTP/2 mTLS listener.
type testGateway struct {
	ts    *httptest.Server
	store *taskstore.Store
	ca    *identity.CA
}

func startTestGateway(t *testing.T) *testGateway {
	t.Helper()

	dir := t.TempDir()
	store, err := taskstore.Open(filepath.Join(dir, "tasks.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ca, err := identity.LoadOrGenerateCA(dir)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}

	apiServer := httpapi.NewServer(store, ca)
	tlsConfig, err := apiServer.BuildTLSConfig([]string{"localhost", "127.0.0.1"})
	if err != nil {
		t.Fatalf("build tls config: %v", err)
	}

	ts := httptest.NewUnstartedServer(apiServer.Handler())
	ts.TLS = tlsConfig
	ts.EnableHTTP2 = true
	ts.StartTLS()
	t.Cleanup(ts.Close)

	return &testGateway{ts: ts, store: store, ca: ca}
}

// pairTestNode enrolls a node into nodeDir and returns the gateway's record.
func pairTestNode(t *testing.T, gw *testGateway, nodeDir string) PairState {
	t.Helper()

	invitation, err := gw.ca.GenerateInvitation(5 * time.Minute)
	if err != nil {
		t.Fatalf("generate invitation: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	state, err := Pair(ctx, PairConfig{
		ServerURL:       gw.ts.URL,
		InvitationToken: invitation.Token,
		CACertPEM:       gw.ca.CACertPEM(),
		Dir:             nodeDir,
	})
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	return state
}

// gatewayClient builds a client that speaks as the node in nodeDir.
func gatewayClient(t *testing.T, nodeDir, serverURL string) *GatewayClient {
	t.Helper()

	caPEM, err := os.ReadFile(filepath.Join(nodeDir, NodeCAFile))
	if err != nil {
		t.Fatalf("read CA: %v", err)
	}
	certPEM, err := os.ReadFile(filepath.Join(nodeDir, NodeCertFile))
	if err != nil {
		t.Fatalf("read certificate: %v", err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(nodeDir, NodeKeyFile))
	if err != nil {
		t.Fatalf("read key: %v", err)
	}

	client, err := NewGatewayClient(serverURL, caPEM, certPEM, keyPEM, 5*time.Second)
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func submitTask(t *testing.T, client *GatewayClient, nodeID, input string) protocol.Task {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	task, err := client.Submit(ctx, protocol.SubmitRequest{
		NodeID:            nodeID,
		Capability:        "agent.run",
		CapabilityVersion: 1,
		Input:             json.RawMessage(input),
		TimeoutSeconds:    60,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return task
}

func getTask(t *testing.T, client *GatewayClient, taskID string) protocol.Task {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	task, err := client.Get(ctx, taskID)
	if err != nil {
		t.Fatalf("get task %s: %v", taskID, err)
	}
	return task
}

// waitForTask polls until the task reaches state, returning the task it saw.
func waitForTask(t *testing.T, client *GatewayClient, taskID string, state protocol.State, timeout time.Duration) protocol.Task {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var last protocol.Task
	for time.Now().Before(deadline) {
		last = getTask(t, client, taskID)
		if last.State == state {
			return last
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("task %s is %s after %s, want %s (result %+v)", taskID, last.State, timeout, state, last.Result)
	return last
}

// waitFor polls a condition so a test never depends on a fixed sleep.
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

// runNode starts the loop in the background and returns a stop function.
func runNode(t *testing.T, n *Node) func() {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("node loop returned an error: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("node loop did not stop")
		}
	})
	return cancel
}

// adapter capability that runs this test binary in the given helper mode.
func helperCapability(t *testing.T, mode string, extraArgs ...string) Capability {
	t.Helper()
	args := append([]string{helperPrefix + mode}, extraArgs...)
	return Capability{
		Name:    "agent.run",
		Version: 1,
		Adapter: Adapter{
			Executable: testBinary(t),
			Args:       append(args, "{instruction}"),
			WorkDir:    t.TempDir(),
			Env:        map[string]string{"NODE_TEST_HELPER": "1"},
		},
	}
}

func testNodeConfig(t *testing.T, serverURL, nodeDir string, capability Capability) Config {
	t.Helper()
	return Config{
		ServerURL:    serverURL,
		CertFile:     NodeCertFile,
		KeyFile:      NodeKeyFile,
		CAFile:       NodeCAFile,
		LeaseSeconds: 30,
		RenewSeconds: 5,
		Capabilities: []Capability{capability},
	}
}

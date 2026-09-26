package httpapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-gateway/internal/identity"
	"agent-gateway/internal/protocol"
	"agent-gateway/internal/taskstore"
)

func setupTestServer(t *testing.T) (*Server, *httptest.Server, *identity.CA) {
	t.Helper()
	dataDir := t.TempDir()

	store, err := taskstore.Open(filepath.Join(dataDir, "tasks.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ca, err := identity.LoadOrGenerateCA(dataDir)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}

	apiServer := NewServer(store, ca)
	tlsConfig, err := apiServer.BuildTLSConfig([]string{"localhost", "127.0.0.1"})
	if err != nil {
		t.Fatalf("build tls config: %v", err)
	}

	ts := httptest.NewUnstartedServer(apiServer.Handler())
	ts.TLS = tlsConfig
	ts.EnableHTTP2 = true
	ts.StartTLS()
	t.Cleanup(ts.Close)

	return apiServer, ts, ca
}

// testNode is one paired device: its server-assigned identity and the mTLS
// client that carries its certificate.
type testNode struct {
	nodeID string
	client *http.Client
}

// unauthenticatedClient speaks TLS but presents no client certificate, which is
// what an unpaired caller can do.
func unauthenticatedClient(t *testing.T, ca *identity.CA) *http.Client {
	t.Helper()
	tlsConfig, err := identity.BuildBootstrapTLSConfig(ca.CACertPEM())
	if err != nil {
		t.Fatalf("build bootstrap tls config: %v", err)
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig, ForceAttemptHTTP2: true}}
}

// pairNode enrolls a fresh node through POST /v1/pair and returns its client.
func pairNode(t *testing.T, ts *httptest.Server, ca *identity.CA) testNode {
	t.Helper()

	inv, err := ca.GenerateInvitation(5 * time.Minute)
	if err != nil {
		t.Fatalf("generate invitation: %v", err)
	}
	keyPEM, csrPEM, err := identity.GenerateNodeKeyAndCSR()
	if err != nil {
		t.Fatalf("generate node key and CSR: %v", err)
	}

	status, body := call(t, unauthenticatedClient(t, ca), http.MethodPost, ts.URL+"/v1/pair",
		protocol.PairRequest{InvitationToken: inv.Token, CSRPEM: string(csrPEM)})
	if status != http.StatusOK {
		t.Fatalf("pair: got status %d, body %s", status, body)
	}

	var pairResp protocol.PairResponse
	if err := json.Unmarshal(body, &pairResp); err != nil {
		t.Fatalf("decode pair response: %v", err)
	}
	if pairResp.NodeID == "" || pairResp.CertPEM == "" {
		t.Fatalf("incomplete pair response: %+v", pairResp)
	}

	tlsConfig, err := identity.BuildClientTLSConfig(ca.CACertPEM(), []byte(pairResp.CertPEM), keyPEM)
	if err != nil {
		t.Fatalf("build client tls config: %v", err)
	}

	return testNode{
		nodeID: pairResp.NodeID,
		client: &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig, ForceAttemptHTTP2: true}},
	}
}

// call sends one JSON request and returns the status with the raw response body.
// The body is always read and closed, so no test leaks a connection.
func call(t *testing.T, client *http.Client, method, url string, payload any) (int, []byte) {
	t.Helper()

	var body *bytes.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		body = bytes.NewReader(encoded)
	} else {
		body = bytes.NewReader(nil)
	}

	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()

	raw := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		raw = append(raw, buf[:n]...)
		if err != nil {
			break
		}
	}
	return resp.StatusCode, raw
}

func mustSubmit(t *testing.T, ts *httptest.Server, node testNode, key string) protocol.Task {
	t.Helper()

	status, body := call(t, node.client, http.MethodPost, ts.URL+"/v1/tasks/submit", protocol.SubmitRequest{
		NodeID:            node.nodeID,
		Capability:        "agent.run",
		CapabilityVersion: 1,
		Input:             json.RawMessage(`{"prompt":"echo hello"}`),
		TimeoutSeconds:    30,
		IdempotencyKey:    key,
	})
	if status != http.StatusCreated {
		t.Fatalf("submit: got status %d, body %s", status, body)
	}

	var task protocol.Task
	if err := json.Unmarshal(body, &task); err != nil {
		t.Fatalf("decode submitted task: %v", err)
	}
	return task
}

func mustClaim(t *testing.T, ts *httptest.Server, node testNode) protocol.Lease {
	t.Helper()

	status, body := call(t, node.client, http.MethodPost, ts.URL+"/v1/tasks/claim",
		protocol.ClaimRequest{LeaseDurationSeconds: 30})
	if status != http.StatusOK {
		t.Fatalf("claim: got status %d, body %s", status, body)
	}

	var lease protocol.Lease
	if err := json.Unmarshal(body, &lease); err != nil {
		t.Fatalf("decode lease: %v", err)
	}
	if lease.Token == "" || lease.Task.ID == "" {
		t.Fatalf("incomplete lease: %+v", lease)
	}
	return lease
}

func mustStart(t *testing.T, ts *httptest.Server, node testNode, lease protocol.Lease) {
	t.Helper()

	status, body := call(t, node.client, http.MethodPost, ts.URL+"/v1/tasks/start", StartRequest{
		TaskID:    lease.Task.ID,
		AttemptID: lease.Task.AttemptID,
		Token:     lease.Token,
	})
	if status != http.StatusOK {
		t.Fatalf("start: got status %d, body %s", status, body)
	}
}

// waitFor polls cond until it holds, so a test never depends on a fixed sleep.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (s *Server) waiterCount(nodeID string) int {
	s.waitersMu.Lock()
	defer s.waitersMu.Unlock()
	return len(s.waiters[nodeID])
}

func TestHTTPAPI_PairAndUnauthenticatedAccess(t *testing.T) {
	_, ts, ca := setupTestServer(t)

	// 1. A client without a certificate cannot reach a task route.
	unauthClient := unauthenticatedClient(t, ca)
	status, _ := call(t, unauthClient, http.MethodPost, ts.URL+"/v1/tasks/claim",
		protocol.ClaimRequest{LeaseDurationSeconds: 10})
	if status != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing client cert, got %d", status)
	}

	// 2. Pairing with an unknown invitation is rejected.
	keyPEM, csrPEM, err := identity.GenerateNodeKeyAndCSR()
	if err != nil {
		t.Fatalf("generate node key and CSR: %v", err)
	}
	status, _ = call(t, unauthClient, http.MethodPost, ts.URL+"/v1/pair",
		protocol.PairRequest{InvitationToken: "invalid-token", CSRPEM: string(csrPEM)})
	if status != http.StatusUnauthorized {
		t.Fatalf("expected 401 for bad invitation token, got %d", status)
	}

	// 3. A valid invitation issues a certificate.
	inv, err := ca.GenerateInvitation(5 * time.Minute)
	if err != nil {
		t.Fatalf("generate invitation: %v", err)
	}
	resp, err := unauthClient.Post(ts.URL+"/v1/pair", "application/json",
		bytes.NewReader(mustMarshal(t, protocol.PairRequest{InvitationToken: inv.Token, CSRPEM: string(csrPEM)})))
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for pair, got %d", resp.StatusCode)
	}

	// The frozen M2 contract requires ALPN h2 and TLS 1.3 minimum.
	if resp.ProtoMajor != 2 || resp.TLS.NegotiatedProtocol != "h2" {
		t.Fatalf("expected HTTP/2 with ALPN h2, got proto=%q alpn=%q", resp.Proto, resp.TLS.NegotiatedProtocol)
	}
	if resp.TLS.Version != tls.VersionTLS13 {
		t.Fatalf("expected TLS 1.3, got version %#x", resp.TLS.Version)
	}

	var pairResp protocol.PairResponse
	if err := json.NewDecoder(resp.Body).Decode(&pairResp); err != nil {
		t.Fatalf("decode pair response: %v", err)
	}
	if pairResp.NodeID == "" || pairResp.CertPEM == "" || pairResp.CACertPEM == "" {
		t.Fatalf("incomplete pair response: %+v", pairResp)
	}

	// 4. The issued certificate is accepted on a task route.
	tlsConfig, err := identity.BuildClientTLSConfig(ca.CACertPEM(), []byte(pairResp.CertPEM), keyPEM)
	if err != nil {
		t.Fatalf("build client tls config: %v", err)
	}
	authClient := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig, ForceAttemptHTTP2: true}}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/tasks/claim",
		bytes.NewReader(mustMarshal(t, protocol.ClaimRequest{LeaseDurationSeconds: 10})))
	if err != nil {
		t.Fatalf("build claim request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// The queue is empty, so the long poll is cancelled by the client. Either
	// outcome is fine as long as the gateway does not answer 401.
	if resp, err := authClient.Do(req); err == nil {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
			t.Fatalf("unexpected claim status: %d", resp.StatusCode)
		}
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return encoded
}

func TestHTTPAPI_UnauthenticatedTaskRoutesAreRejected(t *testing.T) {
	_, ts, ca := setupTestServer(t)
	client := unauthenticatedClient(t, ca)

	cases := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPost, "/v1/tasks/submit", protocol.SubmitRequest{NodeID: "node-x", Capability: "agent.run", CapabilityVersion: 1, Input: json.RawMessage(`{}`), TimeoutSeconds: 30}},
		{http.MethodGet, "/v1/tasks/task-1", nil},
		{http.MethodPost, "/v1/tasks/task-1/cancel", nil},
		{http.MethodPost, "/v1/tasks/claim", protocol.ClaimRequest{LeaseDurationSeconds: 10}},
		{http.MethodPost, "/v1/tasks/start", StartRequest{TaskID: "task-1", AttemptID: "attempt-1", Token: "token"}},
		{http.MethodPost, "/v1/tasks/renew", protocol.RenewRequest{TaskID: "task-1", AttemptID: "attempt-1", Token: "token", LeaseDurationSeconds: 30}},
		{http.MethodPost, "/v1/tasks/complete", protocol.CompleteRequest{TaskID: "task-1", AttemptID: "attempt-1", Token: "token"}},
		{http.MethodPost, "/v1/tasks/events", protocol.TaskEvent{TaskID: "task-1", AttemptID: "attempt-1", Sequence: 1}},
	}

	for _, tc := range cases {
		status, body := call(t, client, tc.method, ts.URL+tc.path, tc.body)
		if status != http.StatusUnauthorized {
			t.Fatalf("%s %s: expected 401 without a client certificate, got %d (%s)", tc.method, tc.path, status, body)
		}
	}
}

func TestHTTPAPI_FullTaskLifecycleOverMTLS(t *testing.T) {
	_, ts, ca := setupTestServer(t)
	node := pairNode(t, ts, ca)

	// 1. Submit a task for this node.
	created := mustSubmit(t, ts, node, "lifecycle-1")

	// 2. Claim it.
	lease := mustClaim(t, ts, node)
	if lease.Task.ID != created.ID {
		t.Fatalf("claimed task %s, want %s", lease.Task.ID, created.ID)
	}

	// 3. Confirm the start before running the work.
	mustStart(t, ts, node, lease)

	// 4. Renew the lease and check the deadline the gateway reports back.
	status, body := call(t, node.client, http.MethodPost, ts.URL+"/v1/tasks/renew", protocol.RenewRequest{
		TaskID:               lease.Task.ID,
		AttemptID:            lease.Task.AttemptID,
		Token:                lease.Token,
		LeaseDurationSeconds: 120,
	})
	if status != http.StatusOK {
		t.Fatalf("renew: got status %d, body %s", status, body)
	}
	var renewResp protocol.RenewResponse
	if err := json.Unmarshal(body, &renewResp); err != nil {
		t.Fatalf("decode renew response: %v", err)
	}
	if time.Until(renewResp.LeaseExpiresAt) < 60*time.Second {
		t.Fatalf("renewed lease expires too soon: %s", renewResp.LeaseExpiresAt)
	}

	// 5. Stream one execution event.
	status, body = call(t, node.client, http.MethodPost, ts.URL+"/v1/tasks/events", protocol.TaskEvent{
		TaskID:    lease.Task.ID,
		AttemptID: lease.Task.AttemptID,
		Sequence:  1,
		Timestamp: time.Now().UTC(),
		Type:      protocol.EventTypeStdout,
		Data:      json.RawMessage(`{"chunk":"running command..."}`),
	})
	if status != http.StatusOK {
		t.Fatalf("events: got status %d, body %s", status, body)
	}

	// 6. Report the terminal result.
	status, body = call(t, node.client, http.MethodPost, ts.URL+"/v1/tasks/complete", protocol.CompleteRequest{
		TaskID:    lease.Task.ID,
		AttemptID: lease.Task.AttemptID,
		Token:     lease.Token,
		Result:    protocol.Result{State: protocol.Succeeded, Text: "execution succeeded", ExitCode: 0},
	})
	if status != http.StatusOK {
		t.Fatalf("complete: got status %d, body %s", status, body)
	}

	// 7. Read the task back.
	status, body = call(t, node.client, http.MethodGet, ts.URL+"/v1/tasks/"+created.ID, nil)
	if status != http.StatusOK {
		t.Fatalf("get task: got status %d, body %s", status, body)
	}
	var finalTask protocol.Task
	if err := json.Unmarshal(body, &finalTask); err != nil {
		t.Fatalf("decode final task: %v", err)
	}
	if finalTask.State != protocol.Succeeded || finalTask.Result == nil || finalTask.Result.Text != "execution succeeded" {
		t.Fatalf("unexpected final task: %+v", finalTask)
	}

	// A listing must never carry the attempt credential.
	if strings.Contains(string(body), lease.Token) {
		t.Fatal("task response leaked the lease token")
	}
}

func TestHTTPAPI_StartRetransmissionIsIdempotent(t *testing.T) {
	_, ts, ca := setupTestServer(t)
	node := pairNode(t, ts, ca)
	mustSubmit(t, ts, node, "start-retry")
	lease := mustClaim(t, ts, node)

	mustStart(t, ts, node, lease)
	// The node lost the first acknowledgement and retries: the gateway must
	// confirm instead of treating the retry as a second launch.
	mustStart(t, ts, node, lease)

	// Wrong credentials are still refused after a successful start.
	status, _ := call(t, node.client, http.MethodPost, ts.URL+"/v1/tasks/start", StartRequest{
		TaskID:    lease.Task.ID,
		AttemptID: lease.Task.AttemptID,
		Token:     "not-the-token",
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a bad token, got %d", status)
	}
}

func TestHTTPAPI_CompleteRetransmissionAndConflict(t *testing.T) {
	_, ts, ca := setupTestServer(t)
	node := pairNode(t, ts, ca)
	mustSubmit(t, ts, node, "complete-retry")
	lease := mustClaim(t, ts, node)
	mustStart(t, ts, node, lease)

	result := protocol.Result{State: protocol.Succeeded, Text: "execution succeeded", ExitCode: 0}
	complete := func(res protocol.Result) int {
		status, body := call(t, node.client, http.MethodPost, ts.URL+"/v1/tasks/complete", protocol.CompleteRequest{
			TaskID:    lease.Task.ID,
			AttemptID: lease.Task.AttemptID,
			Token:     lease.Token,
			Result:    res,
		})
		if status != http.StatusOK && status != http.StatusConflict {
			t.Fatalf("complete: got status %d, body %s", status, body)
		}
		return status
	}

	if status := complete(result); status != http.StatusOK {
		t.Fatalf("first complete: got %d, want 200", status)
	}
	// The acknowledgement was lost, so the node retransmits the same result.
	if status := complete(result); status != http.StatusOK {
		t.Fatalf("retransmitted complete: got %d, want 200", status)
	}
	// A different result for the same attempt must not overwrite the record.
	other := protocol.Result{State: protocol.Succeeded, Text: "different", ExitCode: 0}
	if status := complete(other); status != http.StatusConflict {
		t.Fatalf("conflicting complete: got %d, want 409", status)
	}
}

func TestHTTPAPI_CrossNodeAccessIsRejected(t *testing.T) {
	_, ts, ca := setupTestServer(t)
	owner := pairNode(t, ts, ca)
	intruder := pairNode(t, ts, ca)

	if owner.nodeID == intruder.nodeID {
		t.Fatal("two pairings produced the same node ID")
	}

	task := mustSubmit(t, ts, owner, "cross-node")
	lease := mustClaim(t, ts, owner)
	mustStart(t, ts, owner, lease)

	// Every route the intruder can name the task on must answer as if the task
	// did not exist, without revealing that it does.
	attempts := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/v1/tasks/" + task.ID, nil},
		{http.MethodPost, "/v1/tasks/" + task.ID + "/cancel", nil},
		{http.MethodPost, "/v1/tasks/start", StartRequest{TaskID: task.ID, AttemptID: lease.Task.AttemptID, Token: lease.Token}},
		{http.MethodPost, "/v1/tasks/renew", protocol.RenewRequest{TaskID: task.ID, AttemptID: lease.Task.AttemptID, Token: lease.Token, LeaseDurationSeconds: 60}},
		{http.MethodPost, "/v1/tasks/complete", protocol.CompleteRequest{TaskID: task.ID, AttemptID: lease.Task.AttemptID, Token: lease.Token, Result: protocol.Result{State: protocol.Succeeded}}},
		{http.MethodPost, "/v1/tasks/events", protocol.TaskEvent{TaskID: task.ID, AttemptID: lease.Task.AttemptID, Sequence: 1, Type: protocol.EventTypeStdout}},
	}

	for _, a := range attempts {
		status, body := call(t, intruder.client, a.method, ts.URL+a.path, a.body)
		if status != http.StatusNotFound {
			t.Fatalf("%s %s from another node: got %d (%s), want 404", a.method, a.path, status, body)
		}
	}

	// The intruder's requests must not have disturbed the owner's task.
	status, body := call(t, owner.client, http.MethodGet, ts.URL+"/v1/tasks/"+task.ID, nil)
	if status != http.StatusOK {
		t.Fatalf("owner get: got %d, body %s", status, body)
	}
	var current protocol.Task
	if err := json.Unmarshal(body, &current); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	if current.State != protocol.Running {
		t.Fatalf("owner task state is %s, want running", current.State)
	}
}

func TestHTTPAPI_SubmitForAnotherNodeIsForbidden(t *testing.T) {
	_, ts, ca := setupTestServer(t)
	node := pairNode(t, ts, ca)

	status, body := call(t, node.client, http.MethodPost, ts.URL+"/v1/tasks/submit", protocol.SubmitRequest{
		NodeID:            "node-someone-else",
		Capability:        "agent.run",
		CapabilityVersion: 1,
		Input:             json.RawMessage(`{"prompt":"x"}`),
		TimeoutSeconds:    30,
	})
	if status != http.StatusForbidden {
		t.Fatalf("expected 403 when naming another node, got %d (%s)", status, body)
	}
}

func TestHTTPAPI_ClaimWakesOnSubmit(t *testing.T) {
	srv, ts, ca := setupTestServer(t)
	node := pairNode(t, ts, ca)

	// Park a long poll while the queue is empty.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/tasks/claim",
		bytes.NewReader(mustMarshal(t, protocol.ClaimRequest{LeaseDurationSeconds: 30})))
	if err != nil {
		t.Fatalf("build claim request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	type claimResult struct {
		status int
		lease  protocol.Lease
		err    error
	}
	results := make(chan claimResult, 1)
	go func() {
		resp, err := node.client.Do(req)
		if err != nil {
			results <- claimResult{err: err}
			return
		}
		defer resp.Body.Close()
		var lease protocol.Lease
		if resp.StatusCode == http.StatusOK {
			if err := json.NewDecoder(resp.Body).Decode(&lease); err != nil {
				results <- claimResult{err: err}
				return
			}
		}
		results <- claimResult{status: resp.StatusCode, lease: lease}
	}()

	waitFor(t, "the parked claim to register", func() bool { return srv.waiterCount(node.nodeID) == 1 })

	task := mustSubmit(t, ts, node, "wakeup")

	select {
	case res := <-results:
		if res.err != nil {
			t.Fatalf("parked claim failed: %v", res.err)
		}
		if res.status != http.StatusOK {
			t.Fatalf("parked claim: got status %d, want 200", res.status)
		}
		if res.lease.Task.ID != task.ID {
			t.Fatalf("parked claim returned task %s, want %s", res.lease.Task.ID, task.ID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("parked claim was not woken by the submission")
	}
}

func TestHTTPAPI_ClaimDisconnectKeepsTaskQueued(t *testing.T) {
	srv, ts, ca := setupTestServer(t)
	node := pairNode(t, ts, ca)

	// Park a claim with an empty queue, then drop the connection.
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/tasks/claim",
		bytes.NewReader(mustMarshal(t, protocol.ClaimRequest{LeaseDurationSeconds: 30})))
	if err != nil {
		t.Fatalf("build claim request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := node.client.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()

	waitFor(t, "the parked claim to register", func() bool { return srv.waiterCount(node.nodeID) == 1 })
	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("claim did not return after the client disconnected")
	}

	// The disconnected poll must not leave a waiter behind.
	waitFor(t, "the waiter to be removed", func() bool { return srv.waiterCount(node.nodeID) == 0 })

	// Work queued afterwards is still claimable: nothing was consumed or lost.
	task := mustSubmit(t, ts, node, "after-disconnect")
	lease := mustClaim(t, ts, node)
	if lease.Task.ID != task.ID {
		t.Fatalf("claimed %s, want %s", lease.Task.ID, task.ID)
	}
}

func TestHTTPAPI_OversizedBodyIsRejected(t *testing.T) {
	_, ts, ca := setupTestServer(t)
	node := pairNode(t, ts, ca)

	// One byte past the submit limit, sent as a raw body so the gateway has to
	// bound the read rather than trusting the payload.
	huge := strings.Repeat("a", submitBodyLimit+1)
	payload := `{"node_id":"` + node.nodeID + `","capability":"agent.run","capability_version":1,` +
		`"input":{"prompt":"` + huge + `"},"timeout_seconds":30}`

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/tasks/submit", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := node.client.Do(req)
	if err != nil {
		t.Fatalf("submit oversized: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for an oversized body, got %d", resp.StatusCode)
	}
}

func TestHTTPAPI_PairRateLimit(t *testing.T) {
	srv, ts, ca := setupTestServer(t)

	// A frozen clock makes the bucket deterministic: burst 2, no refill.
	frozen := time.Now()
	srv.pairLimiter = newPairLimiter(1, 2, func() time.Time { return frozen })

	client := unauthenticatedClient(t, ca)
	_, csrPEM, err := identity.GenerateNodeKeyAndCSR()
	if err != nil {
		t.Fatalf("generate CSR: %v", err)
	}
	body := protocol.PairRequest{InvitationToken: "invalid-token", CSRPEM: string(csrPEM)}

	// The limiter counts failed attempts too, so a caller cannot hammer the
	// endpoint for free just because every attempt is rejected.
	for i := 0; i < 2; i++ {
		if status, _ := call(t, client, http.MethodPost, ts.URL+"/v1/pair", body); status != http.StatusUnauthorized {
			t.Fatalf("attempt %d: got %d, want 401", i+1, status)
		}
	}
	if status, _ := call(t, client, http.MethodPost, ts.URL+"/v1/pair", body); status != http.StatusTooManyRequests {
		t.Fatalf("expected 429 once the burst is spent, got %d", status)
	}
}

func TestHTTPAPI_ForeignCACertificateIsRejected(t *testing.T) {
	_, ts, ca := setupTestServer(t)

	// A node enrolled with a different CA presents a chain the gateway does not
	// trust; the handshake must fail rather than fall back to no verification.
	foreignCA, err := identity.LoadOrGenerateCA(t.TempDir())
	if err != nil {
		t.Fatalf("create foreign CA: %v", err)
	}
	inv, err := foreignCA.GenerateInvitation(5 * time.Minute)
	if err != nil {
		t.Fatalf("generate foreign invitation: %v", err)
	}
	keyPEM, csrPEM, err := identity.GenerateNodeKeyAndCSR()
	if err != nil {
		t.Fatalf("generate node key and CSR: %v", err)
	}
	_, foreignCertPEM, err := foreignCA.SignNodeCSR(inv.Token, csrPEM)
	if err != nil {
		t.Fatalf("sign foreign CSR: %v", err)
	}

	tlsConfig, err := identity.BuildClientTLSConfig(ca.CACertPEM(), foreignCertPEM, keyPEM)
	if err != nil {
		t.Fatalf("build client tls config: %v", err)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig, ForceAttemptHTTP2: true}}

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/tasks/claim",
		bytes.NewReader(mustMarshal(t, protocol.ClaimRequest{LeaseDurationSeconds: 10})))
	if err != nil {
		t.Fatalf("build claim request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	if resp, err := client.Do(req); err == nil {
		defer resp.Body.Close()
		t.Fatalf("expected the TLS handshake to fail, got status %d", resp.StatusCode)
	}
}

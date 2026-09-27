package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"path/filepath"
	"testing"
	"time"

	"agent-gateway/internal/devicestore"
	"agent-gateway/internal/identity"
	"agent-gateway/internal/policy"
	"agent-gateway/internal/protocol"
	"agent-gateway/internal/taskstore"
)

func secureFixture(t *testing.T) (*Server, *httptest.Server, *identity.CA) {
	t.Helper()
	dir := t.TempDir()
	tasks, err := taskstore.Open(filepath.Join(dir, "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tasks.Close() })
	devices, err := devicestore.Open(filepath.Join(dir, "devices.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { devices.Close() })
	policies, err := policy.Open(filepath.Join(dir, "policy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { policies.Close() })
	ca, err := identity.LoadOrGenerateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSecureServer(tasks, ca, devices, policies)
	if err != nil {
		t.Fatal(err)
	}
	config, err := s.BuildTLSConfig([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(s.Handler())
	ts.TLS = config
	ts.EnableHTTP2 = true
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return s, ts, ca
}
func issueRole(t *testing.T, s *Server, role policy.Role, nodes ...string) string {
	t.Helper()
	_, token, err := s.policies.Issue(context.Background(), role, nodes, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return token
}
func operatorCall(t *testing.T, c *http.Client, method, url, token string, in any) (int, []byte) {
	t.Helper()
	var b bytes.Buffer
	if in != nil {
		if err := json.NewEncoder(&b).Encode(in); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, url, &b)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, body
}
func TestSecureOperatorRolesAndRevocation(t *testing.T) {
	s, ts, ca := secureFixture(t)
	a := pairNode(t, ts, ca)
	b := pairNode(t, ts, ca)
	c := unauthenticatedClient(t, ca)
	defer c.CloseIdleConnections()
	defer a.client.CloseIdleConnections()
	defer b.client.CloseIdleConnections()
	admin := issueRole(t, s, policy.Admin)
	op := issueRole(t, s, policy.Operator, a.nodeID)
	viewer := issueRole(t, s, policy.Viewer, a.nodeID)
	other := issueRole(t, s, policy.Operator, b.nodeID)
	in := protocol.SubmitRequest{NodeID: a.nodeID, Capability: "agent.run", CapabilityVersion: 1, Input: json.RawMessage(`{"prompt":"test"}`), TimeoutSeconds: 30}
	path := ts.URL + "/v1/operator/tasks/submit"
	for _, tc := range []struct {
		name, token string
		want        int
	}{{"viewer", viewer, 403}, {"outside scope", other, 403}, {"missing", "", 401}, {"operator", op, 201}} {
		t.Run(tc.name, func(t *testing.T) {
			status, _ := operatorCall(t, c, "POST", path, tc.token, in)
			if status != tc.want {
				t.Fatalf("got %d want %d", status, tc.want)
			}
		})
	}
	status, _ := operatorCall(t, a.client, "POST", path, "", in)
	if status != 401 {
		t.Fatalf("device became operator: %d", status)
	}
	status, body := operatorCall(t, c, "POST", path, admin, in)
	if status != 201 {
		t.Fatalf("admin submit: %d", status)
	}
	var task protocol.Task
	if err := json.Unmarshal(body, &task); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		token, method, suffix string
		want                  int
	}{{viewer, "GET", "", 200}, {other, "GET", "", 404}, {viewer, "POST", "/cancel", 404}, {other, "POST", "/cancel", 404}, {op, "POST", "/cancel", 200}} {
		status, _ := operatorCall(t, c, tc.method, ts.URL+"/v1/operator/tasks/"+task.ID+tc.suffix, tc.token, nil)
		if status != tc.want {
			t.Fatalf("%s%s got %d want %d", tc.method, tc.suffix, status, tc.want)
		}
	}
	// Node can still execute a task created by an independent operator.
	lease := mustClaim(t, ts, a)
	mustStart(t, ts, a, lease)
	status, _ = call(t, a.client, "POST", ts.URL+"/v1/tasks/complete", protocol.CompleteRequest{TaskID: lease.Task.ID, AttemptID: lease.Task.AttemptID, Token: lease.Token, Result: protocol.Result{State: protocol.Succeeded, Text: "ok", ExitCode: 0}})
	if status != 200 {
		t.Fatalf("complete: %d", status)
	}
	revoke := ts.URL + "/v1/operator/devices/" + a.nodeID + "/revoke"
	status, _ = operatorCall(t, c, "POST", revoke, op, map[string]string{"reason": "retired"})
	if status != 403 {
		t.Fatalf("operator revoke: %d", status)
	}
	status, _ = operatorCall(t, c, "POST", revoke, admin, map[string]string{"reason": "retired"})
	if status != 200 {
		t.Fatalf("admin revoke: %d", status)
	}
	// Reuse the already-open TLS connection; handshake-only revocation would fail this test.
	reused := false
	req, _ := http.NewRequest("GET", ts.URL+"/v1/tasks/"+lease.Task.ID, nil)
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }}))
	res, err := a.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if !reused {
		t.Fatal("test did not reuse TLS connection")
	}
	if res.StatusCode != 401 {
		t.Fatalf("revoked node got %d", res.StatusCode)
	}
	principal, err := s.policies.Authenticate(context.Background(), op)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.policies.Revoke(context.Background(), principal.ID); err != nil {
		t.Fatal(err)
	}
	status, _ = operatorCall(t, c, "GET", ts.URL+"/v1/operator/tasks/"+lease.Task.ID, op, nil)
	if status != 401 {
		t.Fatalf("revoked operator: %d", status)
	}
}
func TestSecureUnknownCertificateAndPlainHTTP(t *testing.T) {
	s, ts, ca := secureFixture(t)
	key, csr, err := identity.GenerateNodeKeyAndCSR()
	if err != nil {
		t.Fatal(err)
	}
	inv, err := ca.GenerateInvitation(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, cert, err := ca.SignNodeCSR(inv.Token, csr)
	if err != nil {
		t.Fatal(err)
	}
	config, err := identity.BuildClientTLSConfig(ca.CACertPEM(), cert, key)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: config, ForceAttemptHTTP2: true}}
	defer c.CloseIdleConnections()
	status, _ := call(t, c, "POST", ts.URL+"/v1/tasks/claim", protocol.ClaimRequest{})
	if status != 401 {
		t.Fatalf("unregistered cert accepted: %d", status)
	}
	req := httptest.NewRequest("POST", "http://gateway/v1/operator/tasks/submit", nil)
	req.Header.Set("Authorization", "Bearer "+issueRole(t, s, policy.Admin))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 426 {
		t.Fatalf("plain HTTP accepted: %d", rec.Code)
	}
	if _, err := NewSecureServer(s.store, ca, nil, s.policies); err == nil {
		t.Fatal("missing store accepted")
	}
}

func TestSecureOperatorReconcileRequeueAndResolve(t *testing.T) {
	s, ts, ca := secureFixture(t)
	node := pairNode(t, ts, ca)
	c := unauthenticatedClient(t, ca)
	defer c.CloseIdleConnections()
	defer node.client.CloseIdleConnections()

	admin := issueRole(t, s, policy.Admin)
	op := issueRole(t, s, policy.Operator, node.nodeID)
	viewer := issueRole(t, s, policy.Viewer, node.nodeID)
	otherOp := issueRole(t, s, policy.Operator, "other-node-xyz")

	// Submit and claim a task, then let it expire into unknown
	submitReq := protocol.SubmitRequest{
		NodeID:            node.nodeID,
		Capability:        "agent.run",
		CapabilityVersion: 1,
		Input:             json.RawMessage(`{"prompt":"reconcile-test"}`),
		TimeoutSeconds:    30,
	}
	status, body := operatorCall(t, c, "POST", ts.URL+"/v1/operator/tasks/submit", op, submitReq)
	if status != 201 {
		t.Fatalf("submit: %d", status)
	}
	var task protocol.Task
	if err := json.Unmarshal(body, &task); err != nil {
		t.Fatal(err)
	}
	lease := mustClaim(t, ts, node)
	mustStart(t, ts, node, lease)

	// Expire to unknown
	if _, err := s.store.Expire(context.Background(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	// 1. List unknown tasks
	status, body = operatorCall(t, c, "GET", ts.URL+"/v1/operator/tasks?state=unknown", op, nil)
	if status != 200 {
		t.Fatalf("list unknown: %d", status)
	}
	var list []protocol.Task
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != task.ID {
		t.Fatalf("list mismatch: %+v", list)
	}

	// Other operator out of scope cannot see the task in list
	status, body = operatorCall(t, c, "GET", ts.URL+"/v1/operator/tasks?state=unknown", otherOp, nil)
	if status != 200 {
		t.Fatalf("other list: %d", status)
	}
	var otherList []protocol.Task
	json.Unmarshal(body, &otherList)
	if len(otherList) != 0 {
		t.Fatalf("other operator must see 0 tasks, got %d", len(otherList))
	}

	// 2. Viewer cannot requeue
	status, _ = operatorCall(t, c, "POST", ts.URL+"/v1/operator/tasks/"+task.ID+"/requeue", viewer, nil)
	if status != 404 {
		t.Fatalf("viewer requeue want 404, got %d", status)
	}

	// 3. Operator in scope can requeue
	status, body = operatorCall(t, c, "POST", ts.URL+"/v1/operator/tasks/"+task.ID+"/requeue", op, nil)
	if status != 200 {
		t.Fatalf("op requeue want 200, got %d", status)
	}
	var requeued protocol.Task
	json.Unmarshal(body, &requeued)
	if requeued.State != protocol.Queued {
		t.Fatalf("requeued state want queued, got %s", requeued.State)
	}

	// Node claims again and gets new attempt
	newLease := mustClaim(t, ts, node)
	mustStart(t, ts, node, newLease)

	// Expire again
	s.store.Expire(context.Background(), time.Now().Add(time.Hour))

	// 4. Resolve task as failed by operator
	resolveBody := map[string]any{
		"state":      "failed",
		"text":       "manual abort",
		"error_code": "manual_abort",
		"exit_code":  -1,
	}
	status, body = operatorCall(t, c, "POST", ts.URL+"/v1/operator/tasks/"+task.ID+"/resolve", admin, resolveBody)
	if status != 200 {
		t.Fatalf("admin resolve want 200, got %d", status)
	}
	var resolved protocol.Task
	json.Unmarshal(body, &resolved)
	if resolved.State != protocol.Failed {
		t.Fatalf("resolved state want failed, got %s", resolved.State)
	}
}

func TestSecureOperatorListDevices(t *testing.T) {
	s, ts, ca := secureFixture(t)
	nodeA := pairNode(t, ts, ca)
	nodeB := pairNode(t, ts, ca)
	c := unauthenticatedClient(t, ca)
	defer c.CloseIdleConnections()
	defer nodeA.client.CloseIdleConnections()
	defer nodeB.client.CloseIdleConnections()

	admin := issueRole(t, s, policy.Admin)
	opA := issueRole(t, s, policy.Operator, nodeA.nodeID)

	// Admin sees both nodes
	status, body := operatorCall(t, c, "GET", ts.URL+"/v1/operator/devices", admin, nil)
	if status != 200 {
		t.Fatalf("admin list devices: %d", status)
	}
	var allDevices []devicestore.Device
	if err := json.Unmarshal(body, &allDevices); err != nil {
		t.Fatal(err)
	}
	if len(allDevices) != 2 {
		t.Fatalf("admin expected 2 devices, got %d", len(allDevices))
	}

	// Operator A only sees nodeA
	status, body = operatorCall(t, c, "GET", ts.URL+"/v1/operator/devices", opA, nil)
	if status != 200 {
		t.Fatalf("opA list devices: %d", status)
	}
	var opDevices []devicestore.Device
	if err := json.Unmarshal(body, &opDevices); err != nil {
		t.Fatal(err)
	}
	if len(opDevices) != 1 || opDevices[0].NodeID != nodeA.nodeID {
		t.Fatalf("opA expected only nodeA, got %+v", opDevices)
	}

	// Unauthenticated request rejected
	status, _ = operatorCall(t, c, "GET", ts.URL+"/v1/operator/devices", "", nil)
	if status != 401 {
		t.Fatalf("unauthenticated list devices want 401, got %d", status)
	}
}

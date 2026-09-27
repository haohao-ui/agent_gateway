package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"agent-gateway/internal/policy"
	"agent-gateway/internal/protocol"
)

func TestPublicStatus(t *testing.T) {
	_, ts, _ := secureFixture(t)
	client := ts.Client()

	resp, err := client.Get(ts.URL + "/api/public/status")
	if err != nil {
		t.Fatalf("get public status: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var res map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if _, ok := res["gateway_url"]; !ok {
		t.Errorf("missing gateway_url in response")
	}
	if _, ok := res["registered_devices"]; !ok {
		t.Errorf("missing registered_devices in response")
	}
}

func TestOnboardingMD(t *testing.T) {
	_, ts, _ := secureFixture(t)
	client := ts.Client()

	resp, err := client.Get(ts.URL + "/onboarding.md")
	if err != nil {
		t.Fatalf("get onboarding.md: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	if !strings.Contains(bodyStr, ts.URL) {
		t.Errorf("expected onboarding.md to contain gateway URL %s, got: %s", ts.URL, bodyStr)
	}
	if !strings.Contains(bodyStr, "agent-gateway pair") {
		t.Errorf("expected onboarding.md to contain pair instructions")
	}
}

func TestOperatorWaitTask(t *testing.T) {
	s, ts, ca := secureFixture(t)
	node := pairNode(t, ts, ca)
	defer node.client.CloseIdleConnections()

	client := ts.Client()
	adminToken := issueRole(t, s, policy.Admin)

	// 1. Submit a task via operator endpoint
	submitReq := protocol.SubmitRequest{
		NodeID:            node.nodeID,
		Capability:        "agent.run",
		CapabilityVersion: 1,
		Input:             []byte(`{"instruction":"echo test"}`),
		TimeoutSeconds:    30,
	}
	code, respBytes := operatorCall(t, client, "POST", ts.URL+"/v1/operator/tasks/submit", adminToken, submitReq)
	if code != http.StatusCreated {
		t.Fatalf("expected 201 created, got %d: %s", code, string(respBytes))
	}

	var task protocol.Task
	if err := json.Unmarshal(respBytes, &task); err != nil {
		t.Fatalf("decode submit task: %v", err)
	}

	// 2. Call wait endpoint with short timeout (1s) on queued task
	code, waitBytes := operatorCall(t, client, "GET", ts.URL+"/v1/operator/tasks/"+task.ID+"/wait?timeout=1", adminToken, nil)
	if code != http.StatusOK {
		t.Fatalf("expected 200 from wait, got %d: %s", code, string(waitBytes))
	}

	var waited protocol.Task
	if err := json.Unmarshal(waitBytes, &waited); err != nil {
		t.Fatalf("decode wait task: %v", err)
	}
	if waited.ID != task.ID || waited.State != protocol.Queued {
		t.Errorf("unexpected waited task: %+v", waited)
	}
}

func TestMCPMountAndAuth(t *testing.T) {
	s, ts, _ := secureFixture(t)
	client := ts.Client()
	token := issueRole(t, s, policy.Admin)

	// Attach dummy MCP handler
	called := false
	s.SetMCPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("mcp ok"))
	}))

	// Without token -> 401
	respNoAuth, err := client.Post(ts.URL+"/mcp", "application/json", nil)
	if err != nil {
		t.Fatalf("post /mcp without token: %v", err)
	}
	respNoAuth.Body.Close()
	if respNoAuth.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", respNoAuth.StatusCode)
	}

	// With token -> 200
	code, body := operatorCall(t, client, "POST", ts.URL+"/mcp", token, nil)
	if code != http.StatusOK || !called {
		t.Errorf("expected 200 and called=true with valid token, got code=%d, body=%s, called=%v", code, string(body), called)
	}
}

func TestMCPSession_DynamicRevalidation(t *testing.T) {
	s, ts, _ := secureFixture(t)
	client := ts.Client()
	token := issueRole(t, s, policy.Admin)

	s.SetMCPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("mcp session ok"))
	}))

	sessionID := "test-session-dynamic-1"

	// 1. Initial request with token and sessionid -> registers session
	code, _ := operatorCall(t, client, "POST", ts.URL+"/mcp?sessionid="+sessionID, token, nil)
	if code != http.StatusOK {
		t.Fatalf("expected 200 on initial session registration, got %d", code)
	}

	// 2. Subsequent request without token, using sessionid -> passes dynamic check
	resp, err := client.Post(ts.URL+"/mcp?sessionid="+sessionID, "application/json", nil)
	if err != nil {
		t.Fatalf("post with sessionid: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for active session request, got %d", resp.StatusCode)
	}

	// 3. Invalidate or corrupt the underlying session token in session store
	s.mcpSessions.Store(sessionID, &mcpSessionEntry{
		token:       "invalidated-or-revoked-token",
		expiresAt:   time.Now().Add(10 * time.Minute),
		lastChecked: time.Now(),
	})

	// 4. Request with sessionid should now fail dynamic re-validation -> 401
	respRevoked, err := client.Post(ts.URL+"/mcp?sessionid="+sessionID, "application/json", nil)
	if err != nil {
		t.Fatalf("post with revoked session: %v", err)
	}
	respRevoked.Body.Close()
	if respRevoked.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 when underlying session token is invalid, got %d", respRevoked.StatusCode)
	}
}

func TestDownloadMeshSHA256(t *testing.T) {
	_, ts, _ := secureFixture(t)
	client := ts.Client()

	resp, err := client.Get(ts.URL + "/download/mesh.sha256")
	if err != nil {
		t.Fatalf("get mesh.sha256: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for mesh.sha256, got %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	parts := strings.Fields(string(body))
	if len(parts) == 0 || len(parts[0]) != 64 {
		t.Errorf("expected 64-char sha256 hex string, got: %s", string(body))
	}
}

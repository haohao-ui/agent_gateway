package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"agent-gateway/internal/policy"
)

func TestDeviceUniqueness_ReEnrollmentSupersedesOldNode(t *testing.T) {
	s, ts, _ := secureFixture(t)
	adminToken := issueRole(t, s, policy.Admin)

	machID := "mach-test-device-uuid"
	hostname := "workdemac-mini.local"

	// 1. First enrollment of machine
	nodeID1 := "node-1111111111111111"
	err := s.devices.RegisterWithDevice(context.Background(), nodeID1, strings.Repeat("a", 64), machID, hostname, time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("first registration failed: %v", err)
	}
	s.nodeRuntime.Store(nodeID1, NodeRuntimeInfo{
		NodeID:    nodeID1,
		Hostname:  hostname,
		MachineID: machID,
		Status:    "online",
		LastSeen:  time.Now().Add(-5 * time.Minute),
		Online:    true,
	})

	// 2. Second enrollment of the SAME machine
	nodeID2 := "node-2222222222222222"
	err = s.devices.RegisterWithDevice(context.Background(), nodeID2, strings.Repeat("b", 64), machID, hostname, time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("second registration failed: %v", err)
	}
	s.nodeRuntime.Store(nodeID2, NodeRuntimeInfo{
		NodeID:    nodeID2,
		Hostname:  hostname,
		MachineID: machID,
		Status:    "online",
		LastSeen:  time.Now(),
		Online:    true,
	})

	// Check device store state: nodeID1 must be revoked/superseded
	devs, err := s.devices.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range devs {
		if d.NodeID == nodeID1 && !d.Revoked {
			t.Errorf("expected node 1 to be superseded (revoked), but it is not")
		}
		if d.NodeID == nodeID2 && d.Revoked {
			t.Errorf("expected node 2 to be active, but it is revoked")
		}
	}

	// 3. Query operatorListDevices: only 1 unique device should be presented
	req, _ := http.NewRequest("GET", ts.URL+"/v1/operator/devices", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("operatorListDevices status: %d", resp.StatusCode)
	}
	var views []OperatorDeviceView
	if err := json.NewDecoder(resp.Body).Decode(&views); err != nil {
		t.Fatal(err)
	}

	if len(views) != 1 {
		t.Fatalf("expected exactly 1 deduplicated device view, got %d", len(views))
	}
	if views[0].NodeID != nodeID2 {
		t.Errorf("expected active view to be node 2 (%s), got %s", nodeID2, views[0].NodeID)
	}
	if views[0].Hostname != hostname {
		t.Errorf("expected hostname %s, got %s", hostname, views[0].Hostname)
	}
}

func TestOperatorCredential_IssueAndRevokeAPI(t *testing.T) {
	s, ts, _ := secureFixture(t)
	adminToken := issueRole(t, s, policy.Admin)

	// 1. Issue new credential via API
	body, _ := json.Marshal(map[string]any{
		"role":      "admin",
		"ttl_hours": 72,
	})
	req, _ := http.NewRequest("POST", ts.URL+"/v1/operator/credentials", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("issue credential status: %d, body: %s", resp.StatusCode, string(raw))
	}

	var issueResult struct {
		PrincipalID string `json:"principal_id"`
		Role        string `json:"role"`
		Token       string `json:"token"`
		ExpiresAt   string `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&issueResult); err != nil {
		t.Fatal(err)
	}

	if issueResult.PrincipalID == "" || issueResult.Token == "" {
		t.Fatalf("invalid issue response: %+v", issueResult)
	}

	// 2. Test authenticating with the new token
	reqList, _ := http.NewRequest("GET", ts.URL+"/v1/operator/credentials", nil)
	reqList.Header.Set("Authorization", "Bearer "+issueResult.Token)
	respList, err := ts.Client().Do(reqList)
	if err != nil {
		t.Fatal(err)
	}
	respList.Body.Close()
	if respList.StatusCode != http.StatusOK {
		t.Fatalf("expected new token to work, got status %d", respList.StatusCode)
	}

	// 3. Revoke the token
	reqRevoke, _ := http.NewRequest("POST", ts.URL+"/v1/operator/credentials/"+issueResult.PrincipalID+"/revoke", nil)
	reqRevoke.Header.Set("Authorization", "Bearer "+adminToken)
	respRevoke, err := ts.Client().Do(reqRevoke)
	if err != nil {
		t.Fatal(err)
	}
	respRevoke.Body.Close()
	if respRevoke.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke status: %d", respRevoke.StatusCode)
	}

	// 4. Token should no longer work
	reqList2, _ := http.NewRequest("GET", ts.URL+"/v1/operator/credentials", nil)
	reqList2.Header.Set("Authorization", "Bearer "+issueResult.Token)
	respList2, err := ts.Client().Do(reqList2)
	if err != nil {
		t.Fatal(err)
	}
	respList2.Body.Close()
	if respList2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected revoked token to fail with 401, got %d", respList2.StatusCode)
	}
}

func TestDeviceAllowedTools_AuthorizeAndBlock(t *testing.T) {
	s, ts, _ := secureFixture(t)
	adminToken := issueRole(t, s, policy.Admin)

	nodeID := "node-tool-guard-01"
	err := s.devices.Register(context.Background(), nodeID, strings.Repeat("c", 64), time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// 1. Initially configure allowed tools: ["bash", "docker"]
	setBody, _ := json.Marshal(map[string]any{"tools": []string{"bash", "docker"}})
	reqSet, _ := http.NewRequest("POST", ts.URL+"/v1/operator/devices/"+nodeID+"/tools", bytes.NewReader(setBody))
	reqSet.Header.Set("Authorization", "Bearer "+adminToken)
	reqSet.Header.Set("Content-Type", "application/json")
	respSet, err := ts.Client().Do(reqSet)
	if err != nil {
		t.Fatalf("set tools: %v", err)
	}
	defer respSet.Body.Close()
	if respSet.StatusCode != http.StatusOK {
		t.Fatalf("set tools status: %d", respSet.StatusCode)
	}

	// 2. Submit task with authorized capability "bash": should succeed
	submitBash, _ := json.Marshal(map[string]any{
		"node_id":            nodeID,
		"capability":         "bash",
		"capability_version": 1,
		"input":              map[string]any{"command": "echo ok"},
		"timeout_seconds":    30,
	})
	reqSubmitBash, _ := http.NewRequest("POST", ts.URL+"/v1/operator/tasks", bytes.NewReader(submitBash))
	reqSubmitBash.Header.Set("Authorization", "Bearer "+adminToken)
	reqSubmitBash.Header.Set("Content-Type", "application/json")
	respSubmitBash, err := ts.Client().Do(reqSubmitBash)
	if err != nil {
		t.Fatal(err)
	}
	defer respSubmitBash.Body.Close()
	if respSubmitBash.StatusCode != http.StatusCreated {
		t.Fatalf("expected bash task to be created (201), got %d", respSubmitBash.StatusCode)
	}

	// 3. Submit task with unauthorized capability "python3": should be blocked with 403
	submitPython, _ := json.Marshal(map[string]any{
		"node_id":            nodeID,
		"capability":         "python3",
		"capability_version": 1,
		"input":              map[string]any{"script": "print(1)"},
		"timeout_seconds":    30,
	})
	reqSubmitPython, _ := http.NewRequest("POST", ts.URL+"/v1/operator/tasks", bytes.NewReader(submitPython))
	reqSubmitPython.Header.Set("Authorization", "Bearer "+adminToken)
	reqSubmitPython.Header.Set("Content-Type", "application/json")
	respSubmitPython, err := ts.Client().Do(reqSubmitPython)
	if err != nil {
		t.Fatal(err)
	}
	defer respSubmitPython.Body.Close()
	if respSubmitPython.StatusCode != http.StatusForbidden {
		t.Fatalf("expected python3 task to be blocked (403), got %d", respSubmitPython.StatusCode)
	}

	// 4. Update tools to allow python3 as well
	updateBody, _ := json.Marshal(map[string]any{"tools": []string{"bash", "docker", "python3"}})
	reqUpdate, _ := http.NewRequest("POST", ts.URL+"/v1/operator/devices/"+nodeID+"/tools", bytes.NewReader(updateBody))
	reqUpdate.Header.Set("Authorization", "Bearer "+adminToken)
	reqUpdate.Header.Set("Content-Type", "application/json")
	respUpdate, err := ts.Client().Do(reqUpdate)
	if err != nil {
		t.Fatal(err)
	}
	defer respUpdate.Body.Close()

	// 5. Submit python3 task again: now should succeed!
	submitPythonAllowed, _ := json.Marshal(map[string]any{
		"node_id":            nodeID,
		"capability":         "python3",
		"capability_version": 1,
		"input":              map[string]any{"script": "print(1)"},
		"timeout_seconds":    30,
	})
	reqSubmitPython2, _ := http.NewRequest("POST", ts.URL+"/v1/operator/tasks", bytes.NewReader(submitPythonAllowed))
	reqSubmitPython2.Header.Set("Authorization", "Bearer "+adminToken)
	reqSubmitPython2.Header.Set("Content-Type", "application/json")
	respSubmitPython2, err := ts.Client().Do(reqSubmitPython2)
	if err != nil {
		t.Fatal(err)
	}
	defer respSubmitPython2.Body.Close()
	if respSubmitPython2.StatusCode != http.StatusCreated {
		t.Fatalf("expected python3 task to succeed after authorization (201), got %d", respSubmitPython2.StatusCode)
	}
}

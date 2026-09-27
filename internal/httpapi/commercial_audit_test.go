package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-gateway/internal/events"
	"agent-gateway/internal/policy"
	"agent-gateway/internal/protocol"
)

func TestCommercialDownloadTraversal(t *testing.T) {
	s, ts, _ := secureFixture(t)
	dir := t.TempDir()
	s.SetDataDir(dir)
	if err := os.MkdirAll(filepath.Join(dir, "dist"), 0700); err != nil {
		t.Fatal(err)
	}
	marker := []byte("AUDIT_SYNTHETIC_PRIVATE_FILE")
	if err := os.WriteFile(filepath.Join(dir, "audit-secret"), marker, 0600); err != nil {
		t.Fatal(err)
	}
	resp, err := ts.Client().Get(ts.URL + "/download/mesh?arch=" + url.QueryEscape("../../../audit-secret"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if bytes.Equal(body, marker) {
		t.Fatalf("unauthenticated download escaped dist and returned private fixture: HTTP %d", resp.StatusCode)
	}
}

func TestCommercialViewerManagement(t *testing.T) {
	s, ts, _ := secureFixture(t)
	viewer := issueRole(t, s, policy.Viewer, "node-viewer")
	t.Run("invitation", func(t *testing.T) {
		code, _ := operatorCall(t, ts.Client(), "POST", ts.URL+"/api/operator/invitations", viewer, map[string]any{"max_uses": 1})
		if code != 403 {
			t.Fatalf("viewer issued enrollment invitation: HTTP %d, want 403", code)
		}
	})
	t.Run("tls_reset", func(t *testing.T) {
		s.SetDataDir(t.TempDir())
		if err := os.MkdirAll(s.customTLSDir(), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(s.customCertPath(), []byte("synthetic"), 0600); err != nil {
			t.Fatal(err)
		}
		code, _ := operatorCall(t, ts.Client(), "POST", ts.URL+"/api/system/tls/reset", viewer, nil)
		if code != 403 {
			t.Fatalf("viewer reset TLS configuration: HTTP %d, want 403", code)
		}
	})
}

func auditReadEvent(t *testing.T, s *Server, client *http.Client, endpoint, token string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "DENIED"
	}
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), "connected") {
			break
		}
	}
	s.publishEvent(events.Event{Type: events.TypeTaskOutput, NodeID: "node-outside", TaskID: "audit-task", Data: "AUDIT_OUTSIDE_SCOPE_OUTPUT"})
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), "AUDIT_OUTSIDE_SCOPE_OUTPUT") {
			return "LEAK"
		}
	}
	return "NO_LEAK"
}

func TestCommercialSSEAuthorization(t *testing.T) {
	t.Run("plaintext_unauthenticated", func(t *testing.T) {
		s, _, _ := secureFixture(t)
		ts := httptest.NewServer(s.Handler())
		defer ts.Close()
		if auditReadEvent(t, s, ts.Client(), ts.URL+"/v1/events/stream", "") == "LEAK" {
			t.Fatal("unauthenticated HTTP subscriber received task output")
		}
	})
	t.Run("tls_scope", func(t *testing.T) {
		s, ts, _ := secureFixture(t)
		token := issueRole(t, s, policy.Viewer, "node-allowed")
		if auditReadEvent(t, s, ts.Client(), ts.URL+"/v1/events/stream", token) == "LEAK" {
			t.Fatal("scoped TLS viewer received another node's task output")
		}
	})
}

func TestCommercialLoginTransportAndLogout(t *testing.T) {
	s, _, _ := secureFixture(t)
	token := issueRole(t, s, policy.Admin)
	s.SetAdminCredentials("audit-password", token)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := ts.Client().Post(ts.URL+"/api/login", "application/json", strings.NewReader(`{"username":"admin","password":"audit-password"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Error("login accepted plaintext HTTP even with allowPlainHTTP=false")
	}
	for _, c := range resp.Cookies() {
		if c.Name == "gateway_token" && !c.Secure {
			t.Error("plaintext login returned reusable credential cookie without Secure")
		}
	}
}

func TestCommercialCrossOriginMutation(t *testing.T) {
	s, ts, _ := secureFixture(t)
	token := issueRole(t, s, policy.Admin)
	req, _ := http.NewRequest("POST", ts.URL+"/api/operator/invitations", strings.NewReader(`{"max_uses":1}`))
	req.Header.Set("Origin", "https://untrusted.example")
	req.Header.Set("Content-Type", "text/plain")
	req.AddCookie(&http.Cookie{Name: "gateway_token", Value: token})
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatal("cross-origin text/plain mutation with ambient cookie accepted; no Origin/CSRF enforcement")
	}
}

func TestCommercialPowerShellInterpolation(t *testing.T) {
	s := NewServer(nil, nil)
	req := httptest.NewRequest("GET", "http://gateway/download/install.ps1?token="+url.QueryEscape(`$(Write-Output AUDIT_MARKER)`), nil)
	rec := httptest.NewRecorder()
	s.handleDownloadInstallPowerShell(rec, req)
	if strings.Contains(rec.Body.String(), `[string]$Token = "$(Write-Output AUDIT_MARKER)"`) {
		t.Fatal("request token inserted into expandable PowerShell string without escaping")
	}
}

func TestCommercialMCPSessionRace(t *testing.T) {
	s, _, _ := secureFixture(t)
	token := issueRole(t, s, policy.Admin)
	s.SetMCPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	s.mcpSessions.Store("audit-shared-session", &mcpSessionEntry{token: token, expiresAt: time.Now().Add(time.Minute)})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest("POST", "https://gateway/mcp?sessionid=audit-shared-session", nil)
			s.handleMCP(httptest.NewRecorder(), r)
		}()
	}
	wg.Wait()
}

func TestCommercialPublicStatusOffline(t *testing.T) {
	_, ts, ca := secureFixture(t)
	n := pairNode(t, ts, ca)
	defer n.client.CloseIdleConnections()
	resp, err := ts.Client().Get(ts.URL + "/api/public/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Online int `json:"online_devices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Online != 0 {
		t.Fatalf("paired node with no worker heartbeat counted online: %d", out.Online)
	}
}

func TestCommercialUnknownTaskProbe(t *testing.T) {
	s, ts, _ := secureFixture(t)
	v := issueRole(t, s, policy.Viewer, "node-a")
	_, err := s.store.Submit(context.Background(), protocol.SubmitRequest{NodeID: "node-b", Capability: "agent.run", CapabilityVersion: 1, Input: []byte(`{"prompt":"fixture"}`), TimeoutSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	code, _ := operatorCall(t, ts.Client(), "GET", ts.URL+"/v1/operator/tasks/not-a-task", v, nil)
	if code != 404 {
		t.Fatalf("expected hidden task 404, got %d", code)
	}
}

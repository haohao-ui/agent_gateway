package httpapi

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-gateway/internal/events"
	"agent-gateway/internal/policy"
)

func TestH4PlainHTTPFlagNeverPermitsCredentials(t *testing.T) {
	s, _, _ := secureFixture(t)
	s.SetAllowPlainHTTP(true)
	admin := issueRole(t, s, policy.Admin)
	s.SetAdminCredentials("test-password", admin)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	for _, path := range []string{"/api/login", "/api/operator/invitations", "/v1/events/stream", "/mcp", "/v1/operator/credentials"} {
		req, _ := http.NewRequest("POST", ts.URL+path, strings.NewReader(`{"username":"admin","password":"test-password"}`))
		req.AddCookie(&http.Cookie{Name: "gateway_token", Value: admin})
		req.Header.Set("Authorization", "Bearer "+admin)
		res, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 426 || len(res.Cookies()) != 0 {
			t.Fatalf("%s: status %d, cookies %d", path, res.StatusCode, len(res.Cookies()))
		}
	}
}
func TestH7SessionsAreIndependentSecureAndRevokedOnLogout(t *testing.T) {
	s, ts, _ := secureFixture(t)
	admin := issueRole(t, s, policy.Admin)
	s.SetAdminCredentials("test-password", admin)
	var cookies []*http.Cookie
	for range 2 {
		res, err := ts.Client().Post(ts.URL+"/api/login", "application/json", strings.NewReader(`{"username":"admin","password":"test-password"}`))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 200 || len(res.Cookies()) != 1 {
			t.Fatalf("login status %d", res.StatusCode)
		}
		c := res.Cookies()[0]
		if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.MaxAge != 28800 || c.Value == admin {
			t.Fatal("insecure or shared session")
		}
		cookies = append(cookies, c)
	}
	if cookies[0].Value == cookies[1].Value {
		t.Fatal("login reused session")
	}
	req, _ := http.NewRequest("POST", ts.URL+"/api/logout", nil)
	req.AddCookie(cookies[0])
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	if _, err := s.policies.Authenticate(t.Context(), cookies[0].Value); err == nil {
		t.Fatal("logout did not revoke")
	}
	if _, err := s.policies.Authenticate(t.Context(), cookies[1].Value); err != nil {
		t.Fatal("logout revoked other session")
	}
}
func TestH7LoginRateLimitAndRecovery(t *testing.T) {
	s, ts, _ := secureFixture(t)
	s.SetAdminCredentials("correct", issueRole(t, s, policy.Admin))
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	s.loginLimiter.now = func() time.Time { return time.Unix(0, clock.Load()) }
	s.loginGlobal.now = s.loginLimiter.now
	limited := 0
	for i := 0; i < 80; i++ {
		status, _ := operatorCall(t, ts.Client(), "POST", ts.URL+"/api/login", "", map[string]string{"username": "admin", "password": "wrong"})
		if status == 429 {
			limited++
		} else if status != 401 {
			t.Fatal(status)
		}
	}
	if limited < 75 {
		t.Fatalf("only %d attempts limited", limited)
	}
	code, _ := operatorCall(t, ts.Client(), "POST", ts.URL+"/api/login", "", map[string]string{"username": "admin", "password": "correct"})
	if code != 429 {
		t.Fatalf("bucket bypass: %d", code)
	}
	// Set clock only after requests have completed; tests do not mutate shared clock concurrently.
	clock.Add(int64(5 * time.Minute))
	code, _ = operatorCall(t, ts.Client(), "POST", ts.URL+"/api/login", "", map[string]string{"username": "admin", "password": "correct"})
	if code != 200 {
		t.Fatalf("did not recover: %d", code)
	}
}
func TestH7QueryCredentialsRejectedAndAdminRevocation(t *testing.T) {
	s, ts, _ := secureFixture(t)
	admin := issueRole(t, s, policy.Admin)
	viewer := issueRole(t, s, policy.Viewer, "node-a")
	for _, path := range []string{"/v1/operator/tasks", "/v1/events/stream", "/mcp"} {
		res, err := ts.Client().Get(ts.URL + path + "?token=" + admin)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 400 {
			t.Fatalf("query token: %s %d", path, res.StatusCode)
		}
	}
	p, err := s.policies.Authenticate(t.Context(), viewer)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := ts.URL + "/v1/operator/credentials/" + p.ID + "/revoke"
	for _, path := range []string{ts.URL + "/v1/operator/credentials", endpoint} {
		method := "GET"
		if path == endpoint {
			method = "POST"
		}
		if code, _ := operatorCall(t, ts.Client(), method, path, viewer, nil); code != 403 {
			t.Fatalf("viewer management status %d", code)
		}
	}
	code, body := operatorCall(t, ts.Client(), "GET", ts.URL+"/v1/operator/credentials", admin, nil)
	if code != 200 || strings.Contains(string(body), admin) || strings.Contains(string(body), "token_hash") || !strings.Contains(string(body), p.ID) {
		t.Fatal("invalid credential metadata response")
	}
	if code, _ := operatorCall(t, ts.Client(), "POST", endpoint, admin, nil); code != 204 {
		t.Fatal(code)
	}
	if _, err := s.policies.Authenticate(t.Context(), viewer); err == nil {
		t.Fatal("API revocation ineffective")
	}
}

func openTestStream(t *testing.T, ts *httptest.Server, token string) (*http.Response, *bufio.Scanner) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/v1/events/stream", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	if res.StatusCode != 200 {
		return res, nil
	}
	scan := bufio.NewScanner(res.Body)
	for scan.Scan() {
		if strings.Contains(scan.Text(), "connected") {
			return res, scan
		}
	}
	t.Fatal("missing connected event")
	return nil, nil
}
func TestH5RevocationAndExpiryTerminateExistingStreams(t *testing.T) {
	for _, kind := range []string{"event_after_revoke", "idle_revoke", "idle_expiry", "external_store"} {
		t.Run(kind, func(t *testing.T) {
			s, ts, _ := secureFixture(t)
			var external *policy.Store
			if kind == "external_store" {
				path := filepath.Join(t.TempDir(), "policy.sqlite")
				st, err := policy.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				defer st.Close()
				s.policies = st
				external, err = policy.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				defer external.Close()
			}
			expiry := time.Now().Add(time.Hour)
			if kind == "idle_expiry" {
				expiry = time.Now().Add(150 * time.Millisecond)
			}
			p, token, err := s.policies.Issue(t.Context(), policy.Viewer, []string{"node-a"}, expiry)
			if err != nil {
				t.Fatal(err)
			}
			_, scan := openTestStream(t, ts, token)
			if kind != "idle_expiry" {
				revokeStore := s.policies
				if external != nil {
					revokeStore = external
				}
				if err := revokeStore.Revoke(t.Context(), p.ID); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "event_after_revoke" {
				s.hub.Publish(events.Event{NodeID: "node-a", Type: events.TypeTaskOutput, Data: "REVOKED_OUTPUT"})
			}
			start := time.Now()
			for scan.Scan() {
				if strings.Contains(scan.Text(), "REVOKED_OUTPUT") {
					t.Fatal("revoked output leaked")
				}
			}
			if scan.Err() != nil || time.Since(start) > 2*time.Second {
				t.Fatalf("stream did not close promptly: %v", scan.Err())
			}
		})
	}
}
func TestH5ScopeAndSubscriptionBounds(t *testing.T) {
	s, ts, _ := secureFixture(t)
	token := issueRole(t, s, policy.Viewer, "node-a")
	_, scan := openTestStream(t, ts, token)
	s.hub.Publish(events.Event{Type: events.TypeTaskOutput, Data: "NO_NODE_LEAK"})
	s.hub.Publish(events.Event{NodeID: "node-b", Type: events.TypeTaskOutput, Data: "OTHER_NODE_LEAK"})
	s.hub.Publish(events.Event{NodeID: "node-a", Type: events.TypeTaskOutput, Data: "ALLOWED"})
	for scan.Scan() {
		line := scan.Text()
		if strings.Contains(line, "LEAK") {
			t.Fatal("scope leak")
		}
		if strings.Contains(line, "ALLOWED") {
			break
		}
	}
	for i := 1; i < 40; i++ {
		res, _ := openTestStream(t, ts, token)
		want := 429
		if i < maxPrincipalStreams {
			want = 200
		}
		if res.StatusCode != want {
			t.Fatalf("subscription %d: %d", i, res.StatusCode)
		}
	}
	if s.hub.SubscriberCount() != maxPrincipalStreams {
		t.Fatal("unexpected subscriber count")
	}
	for i := maxPrincipalStreams; i < maxTotalStreams; i++ {
		tok := issueRole(t, s, policy.Admin)
		res, _ := openTestStream(t, ts, tok)
		if res.StatusCode != 200 {
			t.Fatal(res.StatusCode)
		}
	}
	res, _ := openTestStream(t, ts, issueRole(t, s, policy.Admin))
	if res.StatusCode != 429 {
		t.Fatal("global bound missing")
	}
}
func TestH5StreamSlotReleased(t *testing.T) {
	s, ts, _ := secureFixture(t)
	tok := issueRole(t, s, policy.Admin)
	res, _ := openTestStream(t, ts, tok)
	res.Body.Close()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s.streamsMu.Lock()
		n := s.streamCount
		s.streamsMu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("stream slot leaked")
}
func TestH7MCPDoesNotEmitCredential(t *testing.T) {
	s, ts, _ := secureFixture(t)
	tok := issueRole(t, s, policy.Admin)
	s.SetMCPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "event: endpoint\ndata: /mcp?sessionid=test\n\n")
	}))
	code, body := operatorCall(t, ts.Client(), "GET", ts.URL+"/mcp", tok, nil)
	if code != 200 || strings.Contains(string(body), tok) {
		t.Fatal("MCP emitted secret")
	}
}

func TestH6ReleaseEndpointConfinedToDistributionFiles(t *testing.T) {
	s, ts, _ := secureFixture(t)
	dir := t.TempDir()
	s.SetDataDir(dir)
	if err := os.Mkdir(filepath.Join(dir, "dist"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dist", "manifest.json"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"manifest.json", "release.key", "../admin.token", "unknown"} {
		res, err := ts.Client().Get(ts.URL + "/download/release/" + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		want := 404
		if path == "manifest.json" {
			want = 200
		}
		if res.StatusCode != want {
			t.Fatalf("%s status %d", path, res.StatusCode)
		}
	}
	secret := filepath.Join(dir, "outside")
	if err := os.WriteFile(secret, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "dist", "NOTICE")); err != nil {
		t.Skip("symlink unavailable")
	}
	res, err := ts.Client().Get(ts.URL + "/download/release/NOTICE")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatal("symlink escaped distribution root")
	}
}

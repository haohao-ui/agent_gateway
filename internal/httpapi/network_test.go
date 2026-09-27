package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"agent-gateway/internal/policy"
)

func TestNetworkSettingsGetAndUpdate(t *testing.T) {
	s, ts, _ := secureFixture(t)
	s.SetDataDir(t.TempDir())
	admin := issueRole(t, s, policy.Admin)
	s.SetAdminCredentials("test-pass", admin)

	// 1. Initial GET should show enable_http: false
	req, _ := http.NewRequest("GET", ts.URL+"/api/system/network", nil)
	req.Header.Set("Authorization", "Bearer "+admin)
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", res.StatusCode)
	}
	var data map[string]any
	if err := json.NewDecoder(res.Body).Decode(&data); err != nil {
		t.Fatal(err)
	}
	if data["enable_http"] != false {
		t.Fatalf("expected enable_http: false, got %v", data["enable_http"])
	}

	// 2. Update to enable_http: true
	body, _ := json.Marshal(map[string]any{
		"enable_http": true,
		"http_port":   8888,
	})
	updateReq, _ := http.NewRequest("POST", ts.URL+"/api/system/network", bytes.NewReader(body))
	updateReq.Header.Set("Authorization", "Bearer "+admin)
	updateReq.Header.Set("Content-Type", "application/json")
	updateRes, err := ts.Client().Do(updateReq)
	if err != nil {
		t.Fatal(err)
	}
	defer updateRes.Body.Close()
	if updateRes.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", updateRes.StatusCode)
	}

	// 3. Verify persistence
	cfg := s.LoadNetworkConfig()
	if !cfg.EnableHTTP || cfg.HTTPPort != 8888 {
		t.Fatalf("expected config enable_http=true and port=8888, got %+v", cfg)
	}
	if !s.IsHTTPAllowed() {
		t.Fatalf("expected server IsHTTPAllowed to be true after update")
	}

	// 4. Unauthorized user should be rejected
	viewer := issueRole(t, s, policy.Viewer, "node-1")
	unauthReq, _ := http.NewRequest("POST", ts.URL+"/api/system/network", bytes.NewReader(body))
	unauthReq.Header.Set("Authorization", "Bearer "+viewer)
	unauthReq.Header.Set("Content-Type", "application/json")
	unauthRes, err := ts.Client().Do(unauthReq)
	if err != nil {
		t.Fatal(err)
	}
	defer unauthRes.Body.Close()
	if unauthRes.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for non-admin, got %d", unauthRes.StatusCode)
	}
}

func TestInstallScriptWithTokenAllowed(t *testing.T) {
	_, ts, _ := secureFixture(t)
	validToken := "0123456789abcdef0123456789abcdef"

	for _, path := range []string{"/install.sh", "/install.ps1", "/download/install.sh", "/download/install.ps1"} {
		res, err := ts.Client().Get(ts.URL + path + "?token=" + validToken)
		if err != nil {
			t.Fatalf("GET %s failed: %v", path, err)
		}
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s?token=%s expected status 200, got %d", path, validToken, res.StatusCode)
		}
		buf := new(bytes.Buffer)
		buf.ReadFrom(res.Body)
		res.Body.Close()
		bodyStr := buf.String()
		if !bytes.Contains(buf.Bytes(), []byte(validToken)) {
			t.Fatalf("GET %s response should contain injected token, got: %s", path, bodyStr)
		}
	}
}

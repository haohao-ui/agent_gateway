package node

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCommercialUpgradeTransport(t *testing.T) {
	if os.Getenv("AUDIT_UPGRADE_CHILD") == "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		n := &Node{cfg: Config{ServerURL: os.Getenv("AUDIT_UPGRADE_URL")}}
		_, err := n.performSelfUpgrade(ctx, os.Getenv("AUDIT_UPGRADE_URL"))
		if err != nil {
			t.Log("untrusted update rejected")
			return
		}
		t.Log("untrusted update accepted")
		return
	}
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "AUDIT_NON_EXECUTABLE_UNSIGNED_UPDATE")
	}))
	defer ts.Close()
	original, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(original)
	if err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(t.TempDir(), "audit-node-test")
	if err := os.WriteFile(child, raw, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, child, "-test.run=^TestCommercialUpgradeTransport$", "-test.v")
	cmd.Env = append(os.Environ(), "AUDIT_UPGRADE_CHILD=1", "AUDIT_UPGRADE_URL="+ts.URL)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated update probe failed: %v (%s)", err, out)
	}
	updated, err := os.ReadFile(child)
	if err != nil {
		t.Fatal(err)
	}
	if string(updated) == "AUDIT_NON_EXECUTABLE_UNSIGNED_UPDATE" {
		t.Fatal("untrusted self-signed TLS server replaced isolated node executable with unsigned non-executable bytes")
	}
}

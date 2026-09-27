package node

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"agent-gateway/internal/release"
)

func TestSignedUpgradeUsesPinnedKeyAndKeepsBackup(t *testing.T) {
	if os.Getenv("SIGNED_UPGRADE_CHILD") == "1" {
		dir := os.Getenv("SIGNED_UPGRADE_DIR")
		n := &Node{dir: dir, cfg: Config{ServerURL: os.Getenv("SIGNED_UPGRADE_SERVER"), CAFile: filepath.Join(dir, "ca.crt")}}
		if _, err := n.performSelfUpgrade(t.Context(), ""); err != nil {
			t.Fatal(err)
		}
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(exe + ".previous"); err != nil {
			t.Fatal("missing backup")
		}
		raw, err := os.ReadFile(filepath.Join(dir, "release.sequence"))
		if err != nil || strings.TrimSpace(string(raw)) != "3" {
			t.Fatal("missing sequence state")
		}
		return
	}
	// Native Windows executable replacement is deliberately not claimed by this test.
	if runtime.GOOS == "windows" {
		t.Skip("running executable replacement requires native Windows installer validation")
	}
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(dir, "node-upgrade-test")
	if err := os.WriteFile(child, raw, 0700); err != nil {
		t.Fatal(err)
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "release.pub"), pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: public}), 0600); err != nil {
		t.Fatal(err)
	}
	target := runtime.GOOS + "-" + runtime.GOARCH
	files := map[string][]byte{release.Targets[target]: raw, "LICENSE": []byte("fixture license"), "NOTICE": []byte("fixture notice"), "THIRD_PARTY_NOTICES": []byte("fixture notices")}
	m := release.Manifest{Schema: 1, Version: "test", Sequence: 3, ExpiresAt: time.Now().Add(time.Hour), Files: map[string]release.File{}}
	for name, data := range files {
		id := name
		if name == release.Targets[target] {
			id = target
		}
		sum := sha256.Sum256(data)
		m.Files[id] = release.File{Name: name, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
	}
	manifest, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	files["manifest.json"] = manifest
	files["manifest.sig"] = ed25519.Sign(key, manifest)
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/download/release/")
		data, ok := files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(data)
	}))
	defer ts.Close()
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw})
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), cert, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, child, "-test.run=^TestSignedUpgradeUsesPinnedKeyAndKeepsBackup$", "-test.v")
	cmd.Env = append(os.Environ(), "SIGNED_UPGRADE_CHILD=1", "SIGNED_UPGRADE_DIR="+dir, "SIGNED_UPGRADE_SERVER="+ts.URL)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("isolated signed upgrade: %v %s", err, out)
	}
	current, err := os.ReadFile(child)
	if err != nil || string(current) != string(raw) {
		t.Fatal("verified executable mismatch")
	}
}
func TestUpgradeRejectsHTTPForeignOriginAndMissingKey(t *testing.T) {
	for _, url := range []string{"http://gateway.example/download/mesh", "https://other.example/download/mesh", "https://gateway.example/download/mesh?arch=other", "https://user@gateway.example/download/mesh", "https://gateway.example/download/mesh"} {
		n := &Node{dir: t.TempDir(), cfg: Config{ServerURL: "https://gateway.example", CAFile: "ca.crt"}}
		if _, err := n.performSelfUpgrade(t.Context(), url); err == nil {
			t.Fatal("unsafe or unpinned upgrade accepted")
		}
	}
}

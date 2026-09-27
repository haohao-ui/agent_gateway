package release

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func signedFixture(t *testing.T) (Manifest, map[string][]byte, ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	target := runtime.GOOS + "-" + runtime.GOARCH
	files := map[string][]byte{Targets[target]: binary, "LICENSE": []byte("license fixture"), "NOTICE": []byte("notice fixture"), "THIRD_PARTY_NOTICES": []byte("third party fixture")}
	m := Manifest{Schema: 1, Version: "test-1", Sequence: 2, ExpiresAt: time.Now().Add(time.Hour), Files: map[string]File{}}
	for name, data := range files {
		id := name
		if name == Targets[target] {
			id = target
		}
		sum := sha256.Sum256(data)
		m.Files[id] = File{Name: name, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	files["manifest.json"] = raw
	files["manifest.sig"] = ed25519.Sign(key, raw)
	return m, files, pub, key
}
func TestManifestRejectsUntrustedExpiredDowngradedAndMalformed(t *testing.T) {
	original, files, pub, key := signedFixture(t)
	for _, kind := range []string{"wrong-key", "tampered", "missing-signature", "expired", "downgrade", "path", "oversize", "missing-notice", "unknown-field"} {
		t.Run(kind, func(t *testing.T) {
			raw := append([]byte{}, files["manifest.json"]...)
			sig := files["manifest.sig"]
			min := uint64(0)
			verifyKey := pub
			var m Manifest
			json.Unmarshal(raw, &m)
			switch kind {
			case "wrong-key":
				verifyKey, _, _ = ed25519.GenerateKey(rand.Reader)
			case "tampered":
				raw = bytes.Replace(raw, []byte("test-1"), []byte("evil-1"), 1)
			case "missing-signature":
				sig = nil
			case "expired":
				m.ExpiresAt = time.Now().Add(-time.Second)
			case "downgrade":
				min = original.Sequence + 1
			case "path":
				f := m.Files["LICENSE"]
				f.Name = "../LICENSE"
				m.Files["LICENSE"] = f
			case "oversize":
				f := m.Files["LICENSE"]
				f.Size = MaxBinaryBytes + 1
				m.Files["LICENSE"] = f
			case "missing-notice":
				delete(m.Files, "NOTICE")
			case "unknown-field":
				raw = append([]byte(`{"injected":true,`), raw[1:]...)
				sig = ed25519.Sign(key, raw)
			}
			if kind == "expired" || kind == "path" || kind == "oversize" || kind == "missing-notice" {
				raw, _ = json.Marshal(m)
				sig = ed25519.Sign(key, raw)
			}
			if _, err := VerifyManifest(raw, sig, verifyKey, time.Now(), min); err == nil {
				t.Fatal("unsafe manifest accepted")
			}
		})
	}
}
func TestFetchAuthenticatesBeforePublishAndFailsClosed(t *testing.T) {
	_, files, pub, _ := signedFixture(t)
	target := runtime.GOOS + "-" + runtime.GOARCH
	for _, kind := range []string{"valid", "tampered-binary", "missing-signature", "redirect", "untrusted-tls", "plain-http", "oversize-manifest", "wrong-platform"} {
		t.Run(kind, func(t *testing.T) {
			var binaryRequests atomic.Int32
			ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				name := strings.TrimPrefix(r.URL.Path, "/download/release/")
				if kind == "redirect" {
					http.Redirect(w, r, "http://127.0.0.1:1/mesh", 302)
					return
				}
				if kind == "missing-signature" && name == "manifest.sig" {
					http.NotFound(w, r)
					return
				}
				data, ok := files[name]
				if !ok {
					http.NotFound(w, r)
					return
				}
				if name == Targets[target] {
					binaryRequests.Add(1)
					if kind == "tampered-binary" {
						data = append([]byte{}, data...)
						data[0] ^= 255
					}
				}
				if kind == "oversize-manifest" && name == "manifest.json" {
					data = bytes.Repeat([]byte("x"), MaxManifestBytes+1)
				}
				w.Write(data)
			}))
			defer ts.Close()
			client := ts.Client()
			base := ts.URL
			requested := target
			if kind == "untrusted-tls" {
				client = &http.Client{}
			}
			if kind == "plain-http" {
				base = strings.Replace(base, "https:", "http:", 1)
			}
			if kind == "wrong-platform" {
				requested = "invalid-platform"
			}
			destination := filepath.Join(t.TempDir(), "mesh")
			if err := os.WriteFile(destination, []byte("existing executable"), 0600); err != nil {
				t.Fatal(err)
			}
			staged, _, err := Fetch(t.Context(), client, base, pub, requested, 0, destination)
			if kind == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				defer os.Remove(staged)
				data, _ := os.ReadFile(staged)
				if !bytes.Equal(data, files[Targets[target]]) {
					t.Fatal("wrong binary")
				}
			} else if err == nil {
				os.Remove(staged)
				t.Fatal("unsafe download accepted")
			}
			old, _ := os.ReadFile(destination)
			if string(old) != "existing executable" {
				t.Fatal("destination modified before verification")
			}
			if kind == "missing-signature" && binaryRequests.Load() != 0 {
				t.Fatal("downloaded binary before verifying signature")
			}
			if kind != "valid" {
				paths, _ := filepath.Glob(filepath.Join(filepath.Dir(destination), ".mesh-release-*"))
				if len(paths) != 0 {
					t.Fatal("failed download left staging files")
				}
			}
		})
	}
}
func TestPlatformRejectsFakeMZAndWrongArchitecture(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fake.exe")
	os.WriteFile(p, []byte("MZ"+strings.Repeat("AUDIT", 500)), 0600)
	if err := CheckPlatform(p, "windows-amd64"); err == nil {
		t.Fatal("fake executable header accepted")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	arch := "amd64"
	if runtime.GOARCH == "amd64" {
		arch = "arm64"
	}
	if err := CheckPlatform(exe, runtime.GOOS+"-"+arch); err == nil {
		t.Fatal("wrong architecture accepted")
	}
}

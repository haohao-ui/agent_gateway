// Package release authenticates offline-signed distribution manifests against
// an independently provisioned public key. The download server is not a trust root.
package release

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const MaxBinaryBytes = 256 << 20
const MaxManifestBytes = 64 << 10

var Targets = map[string]string{
	"darwin-arm64": "mesh-darwin-arm64", "darwin-amd64": "mesh-darwin-amd64",
	"linux-arm64": "mesh-linux-arm64", "linux-amd64": "mesh-linux-amd64",
	"windows-arm64": "mesh-windows-arm64.exe", "windows-amd64": "mesh-windows-amd64.exe",
}
var NoticeFiles = []string{"LICENSE", "NOTICE", "THIRD_PARTY_NOTICES"}

type File struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type Manifest struct {
	Schema    int             `json:"schema"`
	Version   string          `json:"version"`
	Sequence  uint64          `json:"sequence"`
	ExpiresAt time.Time       `json:"expires_at"`
	Files     map[string]File `json:"files"`
}

func ReadPublicKey(path string) (ed25519.PublicKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	b, _ := pem.Decode(raw)
	if b == nil {
		return nil, errors.New("invalid release public key PEM")
	}
	key, err := x509.ParsePKIXPublicKey(b.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := key.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("release key must be Ed25519")
	}
	return pub, nil
}
func VerifyManifest(raw, sig []byte, key ed25519.PublicKey, now time.Time, minSequence uint64) (Manifest, error) {
	var m Manifest
	if len(raw) > MaxManifestBytes || len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, raw, sig) {
		return m, errors.New("invalid release signature")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return m, errors.New("trailing manifest data")
	}
	if m.Schema != 1 || strings.TrimSpace(m.Version) == "" || len(m.Version) > 128 || m.Sequence == 0 || m.Sequence < minSequence || !m.ExpiresAt.After(now) {
		return m, errors.New("invalid, expired or downgraded release manifest")
	}
	if len(m.Files) < 4 || len(m.Files) > len(Targets)+len(NoticeFiles) {
		return m, errors.New("invalid release file count")
	}
	for name, f := range m.Files {
		expected, ok := Targets[name]
		if !ok {
			for _, notice := range NoticeFiles {
				if name == notice {
					expected = notice
					ok = true
				}
			}
		}
		digest, err := hex.DecodeString(f.SHA256)
		if !ok || f.Name != expected || f.Size <= 0 || f.Size > MaxBinaryBytes || err != nil || len(digest) != sha256.Size {
			return m, errors.New("invalid release file entry")
		}
	}
	for _, name := range NoticeFiles {
		if _, ok := m.Files[name]; !ok {
			return m, errors.New("release notices missing")
		}
	}
	return m, nil
}

func Digest(path string) (File, error) {
	f, err := os.Open(path)
	if err != nil {
		return File{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, MaxBinaryBytes+1))
	if err != nil {
		return File{}, err
	}
	if n == 0 || n > MaxBinaryBytes {
		return File{}, errors.New("invalid release file size")
	}
	return File{Name: filepath.Base(path), Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}
func VerifyFile(path string, expected File) error {
	actual, err := Digest(path)
	if err != nil {
		return err
	}
	if actual.Size != expected.Size || actual.SHA256 != expected.SHA256 {
		return errors.New("release file digest or size mismatch")
	}
	return nil
}

// CheckPlatform parses the executable format and validates the declared target.
func CheckPlatform(path, target string) error {
	parts := strings.Split(target, "-")
	if len(parts) != 2 {
		return errors.New("invalid platform")
	}
	switch parts[0] {
	case "linux":
		f, err := elf.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		if (parts[1] == "amd64" && f.Machine == elf.EM_X86_64) || (parts[1] == "arm64" && f.Machine == elf.EM_AARCH64) {
			return nil
		}
	case "darwin":
		f, err := macho.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		if (parts[1] == "amd64" && f.Cpu == macho.CpuAmd64) || (parts[1] == "arm64" && f.Cpu == macho.CpuArm64) {
			return nil
		}
	case "windows":
		f, err := pe.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		if (parts[1] == "amd64" && f.Machine == pe.IMAGE_FILE_MACHINE_AMD64) || (parts[1] == "arm64" && f.Machine == pe.IMAGE_FILE_MACHINE_ARM64) {
			return nil
		}
	}
	return errors.New("release executable platform mismatch")
}

func BaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("release server must be an HTTPS origin without credentials, path or query")
	}
	u.Path = ""
	return u, nil
}

func download(ctx context.Context, client *http.Client, base *url.URL, name string, limit int64) ([]byte, error) {
	u := *base
	u.Path = "/download/release/" + name
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, errors.New("release download failed")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release download status %d", res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("release response exceeds limit")
	}
	return b, nil
}

// Fetch stages a verified file beside destination. Caller owns rename/removal.
// Client TLS roots come from local configuration. Redirects are always rejected.
func Fetch(ctx context.Context, client *http.Client, baseURL string, key ed25519.PublicKey, target string, minSequence uint64, destination string) (string, Manifest, error) {
	var m Manifest
	base, err := BaseURL(baseURL)
	if err != nil {
		return "", m, err
	}
	if len(key) != ed25519.PublicKeySize {
		return "", m, errors.New("release public key required")
	}
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("release redirects forbidden") }
	c.Timeout = 90 * time.Second
	raw, err := download(ctx, &c, base, "manifest.json", MaxManifestBytes)
	if err != nil {
		return "", m, err
	}
	sig, err := download(ctx, &c, base, "manifest.sig", ed25519.SignatureSize)
	if err != nil {
		return "", m, err
	}
	m, err = VerifyManifest(raw, sig, key, time.Now(), minSequence)
	if err != nil {
		return "", m, err
	}
	entry, ok := m.Files[target]
	if !ok {
		return "", m, errors.New("requested platform/file absent from signed release")
	}
	// Stream binary bytes to disk under a signed size bound; never allocate the binary in memory.
	u := *base
	u.Path = "/download/release/" + entry.Name
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", m, err
	}
	res, err := c.Do(req)
	if err != nil {
		return "", m, errors.New("release binary download failed")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", m, fmt.Errorf("release binary status %d", res.StatusCode)
	}
	f, err := os.CreateTemp(filepath.Dir(destination), ".mesh-release-*")
	if err != nil {
		return "", m, err
	}
	path := f.Name()
	ok = false
	defer func() {
		f.Close()
		if !ok {
			os.Remove(path)
		}
	}()
	n, err := io.Copy(f, io.LimitReader(res.Body, entry.Size+1))
	if err != nil {
		return "", m, err
	}
	if n != entry.Size {
		return "", m, errors.New("release binary size mismatch")
	}
	if err = f.Sync(); err != nil {
		return "", m, err
	}
	if err = f.Close(); err != nil {
		return "", m, err
	}
	if err = VerifyFile(path, entry); err != nil {
		return "", m, err
	}
	if _, binary := Targets[target]; binary {
		if err = CheckPlatform(path, target); err != nil {
			return "", m, err
		}
		if err = os.Chmod(path, 0755); err != nil {
			return "", m, err
		}
	}
	ok = true
	return path, m, nil
}

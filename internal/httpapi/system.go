package httpapi

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"agent-gateway/internal/policy"
)

type TLSStatus struct {
	CustomEnabled bool     `json:"custom_enabled"`
	Subject       string   `json:"subject,omitempty"`
	Issuer        string   `json:"issuer,omitempty"`
	DNSNames      []string `json:"dns_names,omitempty"`
	ExpiresAt     string   `json:"expires_at,omitempty"`
}

// customTLSDir returns the path to the custom TLS storage directory.
func (s *Server) customTLSDir() string {
	if s.dataDir == "" {
		return "./gateway-data/tls"
	}
	return filepath.Join(s.dataDir, "tls")
}

func (s *Server) customCertPath() string {
	return filepath.Join(s.customTLSDir(), "server.crt")
}

func (s *Server) customKeyPath() string {
	return filepath.Join(s.customTLSDir(), "server.key")
}

// handleGetTLSStatus returns current TLS configuration info (custom or built-in).
func (s *Server) handleGetTLSStatus(w http.ResponseWriter, r *http.Request) {
	certPath := s.customCertPath()
	keyPath := s.customKeyPath()

	status := TLSStatus{CustomEnabled: false}
	if certBytes, err := os.ReadFile(certPath); err == nil {
		if _, err := os.ReadFile(keyPath); err == nil {
			// Parse certificate to show details
			block, _ := pem.Decode(certBytes)
			if block != nil {
				if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
					status.CustomEnabled = true
					status.Subject = cert.Subject.CommonName
					status.Issuer = cert.Issuer.CommonName
					status.DNSNames = cert.DNSNames
					status.ExpiresAt = cert.NotAfter.UTC().Format(time.RFC3339)
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, status)
}

// requireAdminPrincipal authenticates the operator token, checks CSRF, and ensures the principal holds Admin role.
func (s *Server) requireAdminPrincipal(w http.ResponseWriter, r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" {
		if u, err := url.Parse(origin); err == nil {
			if !strings.EqualFold(u.Host, r.Host) {
				writeError(w, http.StatusForbidden, "forbidden", "cross-origin request disallowed")
				return false
			}
		} else {
			writeError(w, http.StatusForbidden, "forbidden", "invalid origin header")
			return false
		}
	}
	if s.policies == nil {
		return true
	}
	token := extractOperatorToken(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "operator token required")
		return false
	}
	p, err := s.policies.Authenticate(r.Context(), token)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid operator token")
		return false
	}
	if p.Role != policy.Admin {
		writeError(w, http.StatusForbidden, "forbidden", "admin role required for this operation")
		return false
	}
	return true
}

// handleUploadTLS receives multipart/form-data or JSON with certificate and private key.
func (s *Server) handleUploadTLS(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPrincipal(w, r) {
		return
	}

	err := r.ParseMultipartForm(10 << 20) // 10 MB limit
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid multipart form: "+err.Error())
		return
	}

	certFile, _, err := r.FormFile("cert")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "missing 'cert' file")
		return
	}
	defer certFile.Close()

	keyFile, _, err := r.FormFile("key")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "missing 'key' file")
		return
	}
	defer keyFile.Close()

	certBytes, err := io.ReadAll(certFile)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "failed to read cert file")
		return
	}

	keyBytes, err := io.ReadAll(keyFile)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "failed to read key file")
		return
	}

	// Validate the keypair before writing to disk
	_, err = tls.X509KeyPair(certBytes, keyBytes)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_tls", "certificate and key do not match or are invalid: "+err.Error())
		return
	}

	tlsDir := s.customTLSDir()
	if err := os.MkdirAll(tlsDir, 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to create tls dir: "+err.Error())
		return
	}

	if err := os.WriteFile(s.customCertPath(), certBytes, 0o600); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to save server.crt: "+err.Error())
		return
	}
	if err := os.WriteFile(s.customKeyPath(), keyBytes, 0o600); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to save server.key: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Custom TLS certificate installed successfully. Please restart gateway for changes to take effect.",
	})
}

// handleResetTLS restores the built-in self-signed CA certificates.
func (s *Server) handleResetTLS(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPrincipal(w, r) {
		return
	}

	_ = os.Remove(s.customCertPath())
	_ = os.Remove(s.customKeyPath())

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Custom certificate removed. Default built-in CA will be used upon restart.",
	})
}

// handleSystemRestart restarts the gateway process gracefully.
func (s *Server) handleSystemRestart(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPrincipal(w, r) {
		return
	}

	execPath, err := os.Executable()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to locate executable: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Gateway is restarting now. Service will reconnect shortly...",
	})

	go func() {
		time.Sleep(600 * time.Millisecond)
		if runtime.GOOS != "windows" {
			_ = syscall.Exec(execPath, os.Args, os.Environ())
		}
		os.Exit(0)
	}()
}

var knownPlatformBinaries = map[string]string{
	"darwin-amd64":  "mesh-darwin-amd64",
	"darwin-arm64":  "mesh-darwin-arm64",
	"linux-amd64":   "mesh-linux-amd64",
	"linux-arm64":   "mesh-linux-arm64",
	"windows-amd64": "mesh-windows-amd64.exe",
	"windows-arm64": "mesh-windows-arm64.exe",
}

func (s *Server) resolveMeshBinaryPath(arch, ua string) (string, string, error) {
	arch = strings.ToLower(strings.TrimSpace(arch))
	ua = strings.ToLower(ua)
	isWindows := strings.Contains(arch, "windows") || strings.Contains(ua, "windows")

	var targetFileNames []string

	if arch != "" {
		// Strict validation: reject any directory traversal attempts or illegal characters
		if strings.ContainsAny(arch, "/\\.%") || len(arch) > 32 {
			return "", "", fmt.Errorf("invalid arch parameter: path traversal disallowed")
		}
		if exactName, ok := knownPlatformBinaries[arch]; ok {
			targetFileNames = append(targetFileNames, exactName)
		} else {
			targetFileNames = append(targetFileNames, fmt.Sprintf("mesh-%s.exe", arch), fmt.Sprintf("mesh-%s", arch))
		}
	} else if isWindows {
		targetFileNames = append(targetFileNames, "mesh-windows-amd64.exe", "mesh-windows-arm64.exe")
	} else if strings.Contains(ua, "linux") {
		targetFileNames = append(targetFileNames, "mesh-linux-amd64", "mesh-linux-arm64")
	} else {
		targetFileNames = append(targetFileNames, "mesh-darwin-arm64", "mesh-darwin-amd64", "mesh")
	}

	searchDirs := []string{}
	if s.dataDir != "" {
		searchDirs = append(searchDirs, filepath.Join(s.dataDir, "dist"))
	}
	searchDirs = append(searchDirs, "dist")
	if execPath, err := os.Executable(); err == nil {
		searchDirs = append(searchDirs, filepath.Join(filepath.Dir(execPath), "dist"))
	}

	for _, d := range searchDirs {
		absDir, err := filepath.Abs(d)
		if err != nil {
			continue
		}
		for _, name := range targetFileNames {
			candidate := filepath.Join(absDir, name)
			cleanCandidate := filepath.Clean(candidate)
			// Enforce strictly that candidate file resides inside the allowed distribution directory
			if !strings.HasPrefix(cleanCandidate, absDir+string(filepath.Separator)) {
				continue
			}
			if fi, err := os.Stat(cleanCandidate); err == nil && !fi.IsDir() {
				outName := "mesh"
				if isWindows || strings.HasSuffix(cleanCandidate, ".exe") {
					outName = "mesh.exe"
				}
				return cleanCandidate, outName, nil
			}
		}
	}

	// If a specific arch was requested, do NOT fall back to the host executable
	if arch != "" {
		return "", "", os.ErrNotExist
	}

	// For default download (no arch specified), fallback to current running executable
	execPath, err := os.Executable()
	if err != nil {
		return "", "", os.ErrNotExist
	}
	outName := "mesh"
	if isWindows {
		outName = "mesh.exe"
	}
	return execPath, outName, nil
}

func fileSHA256(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// handleDownloadMesh serves the gateway/node binary for remote installations.
func (s *Server) handleDownloadMesh(w http.ResponseWriter, r *http.Request) {
	arch := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("arch")))
	ua := strings.ToLower(r.UserAgent())
	candidate, outName, err := s.resolveMeshBinaryPath(arch, ua)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "mesh binary not found for requested architecture")
		return
	}

	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", outName))
	http.ServeFile(w, r, candidate)
}

// handleDownloadMeshSHA256 serves the SHA-256 checksum of the target node binary for integrity verification.
func (s *Server) handleDownloadMeshSHA256(w http.ResponseWriter, r *http.Request) {
	arch := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("arch")))
	ua := strings.ToLower(r.UserAgent())
	candidate, outName, err := s.resolveMeshBinaryPath(arch, ua)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "binary not found: "+err.Error())
		return
	}

	hash, err := fileSHA256(candidate)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to compute sha256: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(fmt.Sprintf("%s  %s\n", hash, outName)))
}

// handleDownloadInstallScript serves the one-line bash installer for remote worker nodes.
func (s *Server) handleDownloadInstallScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	_, _ = w.Write([]byte(signedInstallerSH))
}
func (s *Server) handleDownloadInstallPowerShell(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(signedInstallerPS))
}

func extractOperatorToken(r *http.Request) string {
	headers := r.Header.Values("Authorization")
	if len(headers) == 1 {
		fields := strings.Fields(headers[0])
		if len(fields) == 2 && strings.EqualFold(fields[0], "Bearer") && len(fields[1]) <= 1024 {
			return fields[1]
		}
	}
	if c, err := r.Cookie("gateway_token"); err == nil && c.Value != "" && len(c.Value) <= 1024 {
		return c.Value
	}
	return ""
}

func (s *Server) handleDownloadSkill(w http.ResponseWriter, r *http.Request) {
	candidatePaths := []string{
		"skills/agent-mesh/SKILL.md",
		filepath.Join(s.dataDir, "skills", "agent-mesh", "SKILL.md"),
	}

	for _, p := range candidatePaths {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			w.Header().Set("Content-Disposition", "attachment; filename=\"SKILL.md\"")
			http.ServeFile(w, r, p)
			return
		}
	}

	writeError(w, http.StatusNotFound, "not_found", "SKILL.md not found on gateway")
}

// handleCreateInvitation mints an invitation token (single-use or multi-use) for node enrollment.
func (s *Server) handleCreateInvitation(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPrincipal(w, r) {
		return
	}

	var in struct {
		TTLMinutes int `json:"ttl_minutes"`
		MaxUses    int `json:"max_uses"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)

	if in.TTLMinutes <= 0 {
		in.TTLMinutes = 120 // 默认 2 小时
	}
	if in.TTLMinutes > 1440 {
		in.TTLMinutes = 1440 // 最大 24 小时
	}
	if in.MaxUses == 0 {
		in.MaxUses = 1
	}

	ttl := time.Duration(in.TTLMinutes) * time.Minute
	invitation, err := s.ca.GenerateInvitationWithUses(ttl, in.MaxUses)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to issue invitation: "+err.Error())
		return
	}

	installCmd := "bash ./install.sh https://<gateway> ./invitation.txt ./node"
	installCmdPS1 := "./install.ps1 -Server https://<gateway> -TokenFile ./invitation.txt -Dir ./node"

	isMulti := in.MaxUses > 1 || in.MaxUses == -1
	writeJSON(w, http.StatusOK, map[string]any{
		"token":              invitation.Token,
		"expires_at":         invitation.ExpiresAt.Format(time.RFC3339),
		"max_uses":           invitation.MaxUses,
		"multi_use":          isMulti,
		"server_fingerprint": invitation.ServerFingerprint,
		"install_cmd":        installCmd,
		"install_cmd_bash":   installCmd,
		"install_cmd_ps1":    installCmdPS1,
	})
}

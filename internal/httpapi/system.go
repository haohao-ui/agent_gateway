package httpapi

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
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

// handleUploadTLS receives multipart/form-data or JSON with certificate and private key.
func (s *Server) handleUploadTLS(w http.ResponseWriter, r *http.Request) {
	// Require operator authorization
	if s.policies != nil {
		token := extractOperatorToken(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "operator token required")
			return
		}
		if _, err := s.policies.Authenticate(r.Context(), token); err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid operator token")
			return
		}
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
	if s.policies != nil {
		token := extractOperatorToken(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "operator token required")
			return
		}
		if _, err := s.policies.Authenticate(r.Context(), token); err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid operator token")
			return
		}
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
	if s.policies != nil {
		token := extractOperatorToken(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "operator token required")
			return
		}
		if _, err := s.policies.Authenticate(r.Context(), token); err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid operator token")
			return
		}
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

// handleDownloadMesh serves the gateway/node binary for remote installations.
func (s *Server) handleDownloadMesh(w http.ResponseWriter, r *http.Request) {
	arch := r.URL.Query().Get("arch")
	// If a specific architecture is requested and exists in dist directory, serve that
	if s.dataDir != "" {
		if arch != "" {
			candidate := filepath.Join(s.dataDir, "dist", fmt.Sprintf("mesh-%s", arch))
			if _, err := os.Stat(candidate); err == nil {
				w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"mesh-%s\"", arch))
				http.ServeFile(w, r, candidate)
				return
			}
		}
		// If client User-Agent indicates Linux, prefer linux-amd64 if available
		ua := strings.ToLower(r.UserAgent())
		if strings.Contains(ua, "linux") {
			candidate := filepath.Join(s.dataDir, "dist", "mesh-linux-amd64")
			if _, err := os.Stat(candidate); err == nil {
				w.Header().Set("Content-Disposition", "attachment; filename=\"mesh\"")
				http.ServeFile(w, r, candidate)
				return
			}
		}
	}

	// Default: serve currently running executable
	execPath, err := os.Executable()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "binary not available: "+err.Error())
		return
	}

	w.Header().Set("Content-Disposition", "attachment; filename=\"mesh\"")
	http.ServeFile(w, r, execPath)
}

// handleDownloadInstallScript serves the one-line bash installer for remote worker nodes.
func (s *Server) handleDownloadInstallScript(w http.ResponseWriter, r *http.Request) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	downloadURL := fmt.Sprintf("%s://%s", scheme, r.Host)

	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil && h != "" {
		host = h
	}
	tlsURL := fmt.Sprintf("https://%s:8443", host)

	script := fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail

# Agent Mesh Node One-Line Auto Installer
# Assets Download URL: %[1]s
# Gateway TLS Server:  %[2]s

DOWNLOAD_URL="%[1]s"
GATEWAY_TLS_URL="%[2]s"
TOKEN="${1:-}"
DIR="${2:-$HOME/.agent-mesh-node}"

if [ -z "$TOKEN" ]; then
    echo "=========================================================="
    echo "❌ 缺少配对邀请码 (Invitation Token)"
    echo "用法: curl -fsSL $DOWNLOAD_URL/download/install.sh | bash -s -- <INVITATION_TOKEN> [INSTALL_DIR]"
    echo "请在网关控制台获取一个有效的配对邀请码后重试。"
    echo "=========================================================="
    exit 1
fi

echo "=========================================================="
echo "🚀 开始安装 Agent Mesh 节点工作进程..."
echo "下载端点: $DOWNLOAD_URL"
echo "网关服务: $GATEWAY_TLS_URL"
echo "安装目录: $DIR"
echo "=========================================================="

mkdir -p "$DIR"
cd "$DIR"

echo "1. 探测主机操作系统与架构..."
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
    x86_64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
esac
echo "   Detected: ${OS}-${ARCH}"

echo "2. 下载网关 CA 证书..."
curl -fsSL "$DOWNLOAD_URL/download/ca.crt" -o ca.crt

echo "3. 下载 mesh 节点二进制程序..."
curl -fsSL "$DOWNLOAD_URL/download/mesh?arch=${OS}-${ARCH}" -o mesh
chmod +x mesh

echo "4. 执行节点安全配对..."
./mesh pair --server "$GATEWAY_TLS_URL" --ca ca.crt --token "$TOKEN" --dir "$DIR"

echo "=========================================================="
echo "✅ 节点已成功与网关完成证书配对！"
echo ""
echo "正在后台启动 mesh node 工作循环..."
nohup "$DIR/mesh" node --dir "$DIR" --config "$DIR/node.json" > "$DIR/node.log" 2>&1 &
echo "启动成功！后台进程 PID: $!"
echo "查看运行日志: tail -f $DIR/node.log"
echo "=========================================================="
`, downloadURL, tlsURL)

	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(script))
}

func extractOperatorToken(r *http.Request) string {
	headers := r.Header.Values("Authorization")
	if len(headers) == 1 {
		fields := strings.Fields(headers[0])
		if len(fields) == 2 && strings.EqualFold(fields[0], "Bearer") && len(fields[1]) <= 1024 {
			return fields[1]
		}
	}
	if q := r.URL.Query().Get("token"); q != "" && len(q) <= 1024 {
		return q
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

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
	"net"
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

echo "4. 校验 mesh 程序完整性 (SHA-256 防篡改)..."
EXPECTED_HASH=$(curl -fsSL "$DOWNLOAD_URL/download/mesh.sha256?arch=${OS}-${ARCH}" 2>/dev/null | awk '{print $1}' || true)
if [ -n "$EXPECTED_HASH" ]; then
    ACTUAL_HASH=""
    if command -v sha256sum >/dev/null 2>&1; then
        ACTUAL_HASH=$(sha256sum mesh | awk '{print $1}')
    elif command -v shasum >/dev/null 2>&1; then
        ACTUAL_HASH=$(shasum -a 256 mesh | awk '{print $1}')
    fi

    if [ -n "$ACTUAL_HASH" ]; then
        if [ "$ACTUAL_HASH" != "$EXPECTED_HASH" ]; then
            echo "❌ 二进制完整性校验失败！期望值: $EXPECTED_HASH，计算值: $ACTUAL_HASH" >&2
            echo "疑似遭遇网络中间人篡改或文件损坏，终止安装并立即清除可执行程序！" >&2
            rm -f mesh
            exit 1
        fi
        echo "   ✅ SHA-256 完整性校验通过: ${ACTUAL_HASH}"
    else
        echo "   ⚠️ 未找到本地 sha256 工具，跳过本地计算"
    fi
else
    echo "   ⚠️ 未能从网关获取校验值"
fi

echo "5. 执行节点安全配对..."
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

// handleDownloadInstallPowerShell serves the PowerShell one-line installer for Windows worker nodes.
func (s *Server) handleDownloadInstallPowerShell(w http.ResponseWriter, r *http.Request) {
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
	token := r.URL.Query().Get("token")

	safeToken := strings.ReplaceAll(token, "'", "''")

	script := fmt.Sprintf(`param(
    [Parameter(Position=0)]
    [string]$Token = '%[3]s',

    [Parameter(Position=1)]
    [string]$Dir = "$HOME\.agent-mesh-node"
)

$ErrorActionPreference = "Stop"

# Agent Mesh Node One-Line Auto Installer (PowerShell for Windows)
# Assets Download URL: %[1]s
# Gateway TLS Server:  %[2]s

$DOWNLOAD_URL = "%[1]s"
$GATEWAY_TLS_URL = "%[2]s"

if (-not $Token -or $Token -eq "{{TOKEN}}") {
    Write-Host "==========================================================" -ForegroundColor Red
    Write-Host "❌ 缺少配对邀请码 (Invitation Token)" -ForegroundColor Red
    Write-Host "用法:" -ForegroundColor Yellow
    Write-Host "  irm ""$DOWNLOAD_URL/download/install.ps1?token=<邀请码>"" | iex" -ForegroundColor White
    Write-Host "或:" -ForegroundColor Yellow
    Write-Host "  & ([scriptblock]::Create((irm ""$DOWNLOAD_URL/download/install.ps1""))) -Token ""<邀请码>""" -ForegroundColor White
    Write-Host "请在网关控制台获取一个有效的配对邀请码后重试。" -ForegroundColor Yellow
    Write-Host "==========================================================" -ForegroundColor Red
    Exit 1
}

Write-Host "==========================================================" -ForegroundColor Cyan
Write-Host "🚀 开始安装 Agent Mesh Windows 节点工作进程..." -ForegroundColor Green
Write-Host "下载端点: $DOWNLOAD_URL"
Write-Host "网关服务: $GATEWAY_TLS_URL"
Write-Host "安装目录: $Dir"
Write-Host "==========================================================" -ForegroundColor Cyan

# 1. 创建安装目录
if (-not (Test-Path -Path $Dir)) {
    New-Item -ItemType Directory -Path $Dir -Force | Out-Null
}
$resolvedDir = (Resolve-Path -Path $Dir).Path
Set-Location -Path $resolvedDir

# 2. 探测系统架构
$arch = $env:PROCESSOR_ARCHITECTURE.ToLower()
$meshArch = "windows-amd64"
if ($arch -eq "arm64") {
    $meshArch = "windows-arm64"
}
Write-Host "1. 探测主机操作系统与架构: Windows (${meshArch})" -ForegroundColor Cyan

# 3. 下载网关 CA 证书
Write-Host "2. 正在下载网关 CA 证书..." -ForegroundColor Cyan
$caFile = Join-Path $resolvedDir "ca.crt"
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12 -bor [Net.SecurityProtocolType]::Tls13
Invoke-RestMethod -Uri "$DOWNLOAD_URL/download/ca.crt" -OutFile $caFile

# 4. 停止可能运行中的旧进程并下载 mesh.exe
Write-Host "3. 正在下载 mesh.exe 节点程序 (${meshArch})..." -ForegroundColor Cyan
Get-Process -Name "mesh" -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
Start-Sleep -Milliseconds 500

$exeFile = Join-Path $resolvedDir "mesh.exe"
Invoke-RestMethod -Uri "$DOWNLOAD_URL/download/mesh?arch=$meshArch" -OutFile $exeFile

# 5. 校验 SHA-256 完整性 (防篡改)
Write-Host "4. 正在校验 mesh.exe 完整性 (SHA-256)..." -ForegroundColor Cyan
try {
    $rawHashResp = (Invoke-RestMethod -Uri "$DOWNLOAD_URL/download/mesh.sha256?arch=$meshArch").Trim()
    $expectedHash = ($rawHashResp -split '\s+')[0].ToLower()
    $actualHash = (Get-FileHash -Path $exeFile -Algorithm SHA256).Hash.ToLower()

    if ($expectedHash -and $actualHash -ne $expectedHash) {
        Write-Host "❌ 二进制完整性校验失败！期望: $expectedHash，实际: $actualHash" -ForegroundColor Red
        Write-Host "疑似遭遇中间人篡改或下载损坏，已紧急终止安装并移除文件。" -ForegroundColor Red
        Remove-Item -Path $exeFile -Force -ErrorAction SilentlyContinue
        Exit 1
    }
    Write-Host "   ✅ SHA-256 签名校验通过: $actualHash" -ForegroundColor Green
} catch {
    Write-Host "⚠️ 获取或比对签名校验值异常: $_ ，继续执行" -ForegroundColor Yellow
}

# 6. 执行节点安全证书配对
Write-Host "5. 正在执行节点安全证书配对..." -ForegroundColor Cyan
& $exeFile pair --server $GATEWAY_TLS_URL --ca $caFile --token $Token --dir $resolvedDir
if ($LASTEXITCODE -ne 0) {
    Write-Host "❌ 节点证书配对失败，请检查邀请码或网关端口与证书连接。" -ForegroundColor Red
    Exit $LASTEXITCODE
}

Write-Host "==========================================================" -ForegroundColor Green
Write-Host "✅ 节点已成功与网关完成安全证书配对！" -ForegroundColor Green
Write-Host ""
Write-Host "正在后台启动 mesh node 工作循环..." -ForegroundColor Cyan

# 7. 后台拉起节点工作进程 (静默无黑框运行)
$logFile = Join-Path $resolvedDir "node.log"
$errFile = Join-Path $resolvedDir "node.err.log"
$cfgFile = Join-Path $resolvedDir "node.json"

$startParams = @{
    FilePath = $exeFile
    ArgumentList = @("node", "--dir", $resolvedDir, "--config", $cfgFile)
    WorkingDirectory = $resolvedDir
    RedirectStandardOutput = $logFile
    RedirectStandardError = $errFile
    WindowStyle = "Hidden"
    PassThru = $true
}
$proc = Start-Process @startParams

Write-Host "🎉 节点启动成功！后台进程 PID: $($proc.Id)" -ForegroundColor Green
Write-Host "运行日志文件: $logFile" -ForegroundColor Yellow
Write-Host "查看实时日志命令 (PowerShell):" -ForegroundColor Gray
Write-Host "  Get-Content -Path '$logFile' -Wait" -ForegroundColor White
Write-Host "==========================================================" -ForegroundColor Green
`, downloadURL, tlsURL, safeToken)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
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

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	installCmd := fmt.Sprintf("curl -fsSL %s://%s/download/install.sh | bash -s -- %s", scheme, r.Host, invitation.Token)
	installCmdPS1 := fmt.Sprintf("irm \"%s://%s/download/install.ps1?token=%s\" | iex", scheme, r.Host, invitation.Token)

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

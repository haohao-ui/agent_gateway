package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agent-gateway/internal/policy"
	"agent-gateway/internal/protocol"
)

const onboardingTemplate = `# Agent Gateway 新机器自动对接说明（适用于当前电脑 Agent）

> 目标：把本机注册到 Agent Gateway，并安装随系统启动的本地服务。网关投递任务后，Hook 会调用当前 Agent 的非交互式命令执行，结果自动回传网关。
> 网关地址：{{GATEWAY_URL}}

## 1. 检查网关连通性
先检查健康探针端点：
` + "```bash" + `
curl -fsS {{GATEWAY_URL}}/api/public/status
` + "```" + `

## 2. 节点配对与证书生成
使用网关操作员发放的配对邀请码（Invitation Token）进行安全配对：
` + "```bash" + `
agent-gateway pair --server {{GATEWAY_URL}} --token <INVITATION_TOKEN> --dir ~/.agent-gateway
` + "```" + `

## 3. 配置与启动守护服务
配置您的 Agent 适配器后，一键注册为系统用户级后台守护服务（开机自启）：
` + "```bash" + `
agent-gateway service install --role node
` + "```" + `
`

func parseTimeoutParam(r *http.Request) time.Duration {
	val := r.URL.Query().Get("timeout")
	if val == "" {
		return 30 * time.Second
	}
	if d, err := time.ParseDuration(val); err == nil {
		return d
	}
	if n, err := strconv.Atoi(val); err == nil {
		return time.Duration(n) * time.Second
	}
	return 30 * time.Second
}

func (s *Server) waitTaskTerminal(ctx context.Context, taskID string, timeout time.Duration) (protocol.Task, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if timeout > 120*time.Second {
		timeout = 120 * time.Second
	}

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		task, err := s.store.Get(ctx, taskID)
		if err != nil {
			return protocol.Task{}, err
		}
		switch task.State {
		case protocol.Succeeded, protocol.Failed, protocol.Cancelled, protocol.Unknown:
			return task, nil
		}
		if time.Now().After(deadline) {
			return task, nil
		}
		select {
		case <-ctx.Done():
			return task, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Server) handlePublicStatus(w http.ResponseWriter, r *http.Request) {
	scheme := "https"
	if r.TLS == nil {
		scheme = "http"
	}
	gatewayURL := fmt.Sprintf("%s://%s", scheme, r.Host)

	registered := 0
	online := 0
	if s.devices != nil {
		if list, err := s.devices.List(r.Context()); err == nil {
			registered = len(list)
			for _, d := range list {
				if !d.Revoked && time.Now().Before(d.ExpiresAt) {
					online++
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"gateway_url":        gatewayURL,
		"registered_devices": registered,
		"online_devices":     online,
		"updated_at":         time.Now().Unix(),
	})
}

func (s *Server) handleOnboardingMD(w http.ResponseWriter, r *http.Request) {
	scheme := "https"
	if r.TLS == nil {
		scheme = "http"
	}
	gatewayURL := fmt.Sprintf("%s://%s", scheme, r.Host)

	doc := strings.ReplaceAll(onboardingTemplate, "{{GATEWAY_URL}}", gatewayURL)
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(doc))
}

func (s *Server) operatorWait(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	task, err := s.store.Get(r.Context(), taskID)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if policy.Authorize(operatorPrincipal(r), "task.read", task.NodeID) != nil {
		writeError(w, http.StatusNotFound, "not_found", "task not found")
		return
	}

	timeout := parseTimeoutParam(r)
	result, err := s.waitTaskTerminal(r.Context(), taskID, timeout)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleWaitTask(w http.ResponseWriter, r *http.Request) {
	nodeID, _ := r.Context().Value(nodeIDContextKey).(string)
	taskID := r.PathValue("id")
	task, err := s.store.Get(r.Context(), taskID)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if task.NodeID != nodeID {
		writeError(w, http.StatusNotFound, "not_found", "task not found")
		return
	}

	timeout := parseTimeoutParam(r)
	result, err := s.waitTaskTerminal(r.Context(), taskID, timeout)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type sseEndpointRewriter struct {
	http.ResponseWriter
	token   string
	server  *Server
	rewrote bool
}

func (rw *sseEndpointRewriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (rw *sseEndpointRewriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := rw.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

func (rw *sseEndpointRewriter) Write(b []byte) (int, error) {
	if !rw.rewrote && bytes.Contains(b, []byte("event: endpoint")) {
		rw.rewrote = true
		lines := strings.Split(string(b), "\n")
		var modifiedLines []string
		for _, line := range lines {
			if strings.HasPrefix(line, "data:") {
				ep := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				// Extract sessionid if present and record it in mcpSessions
				if idx := strings.Index(ep, "sessionid="); idx != -1 {
					sid := ep[idx+len("sessionid="):]
					if ampIdx := strings.Index(sid, "&"); ampIdx != -1 {
						sid = sid[:ampIdx]
					}
					if sid != "" && rw.server != nil {
						rw.server.mcpSessions.Store(sid, time.Now().Add(24*time.Hour))
					}
				}
				// Append token to endpoint so standard MCP clients automatically send token in subsequent POSTs
				if rw.token != "" && !strings.Contains(ep, "token=") {
					sep := "&"
					if !strings.Contains(ep, "?") {
						sep = "?"
					}
					ep = ep + sep + "token=" + rw.token
				}
				modifiedLines = append(modifiedLines, "data: "+ep)
			} else {
				modifiedLines = append(modifiedLines, line)
			}
		}
		newBytes := []byte(strings.Join(modifiedLines, "\n"))
		_, err := rw.ResponseWriter.Write(newBytes)
		return len(b), err
	}
	return rw.ResponseWriter.Write(b)
}

// SetMCPHandler attaches an HTTP handler to serve MCP protocol requests on /mcp.
func (s *Server) SetMCPHandler(h http.Handler) {
	s.mcpHandler = h
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if s.mcpHandler == nil {
		writeError(w, http.StatusNotFound, "not_found", "MCP service is not mounted on this gateway")
		return
	}

	// 1. If request carries a valid sessionid that was already authenticated, allow it directly
	sessionID := r.URL.Query().Get("sessionid")
	if sessionID != "" {
		if expVal, ok := s.mcpSessions.Load(sessionID); ok {
			if exp, ok := expVal.(time.Time); ok && time.Now().Before(exp) {
				s.mcpHandler.ServeHTTP(w, r)
				return
			}
		}
	}

	// 2. Authenticate token for new session establishment or non-session requests
	token := ""
	if s.policies != nil {
		headers := r.Header.Values("Authorization")
		if len(headers) == 1 {
			fields := strings.Fields(headers[0])
			if len(fields) == 2 && strings.EqualFold(fields[0], "Bearer") && len(fields[1]) <= 1024 {
				token = fields[1]
			}
		}
		if token == "" {
			qToken := r.URL.Query().Get("token")
			if qToken != "" && len(qToken) <= 1024 {
				token = qToken
			}
		}
		if token == "" {
			if c, err := r.Cookie("gateway_token"); err == nil && c.Value != "" && len(c.Value) <= 1024 {
				token = c.Value
			}
		}
		if token == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "operator credential required for MCP (Bearer header, ?token= or cookie)")
			return
		}
		if _, err := s.policies.Authenticate(r.Context(), token); err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid operator credential for MCP")
			return
		}
	}

	// 3. For GET requests (SSE stream initialization), wrap ResponseWriter to inject token and cache sessionid
	if r.Method == http.MethodGet {
		rw := &sseEndpointRewriter{
			ResponseWriter: w,
			token:          token,
			server:         s,
		}
		s.mcpHandler.ServeHTTP(rw, r)
		return
	}

	s.mcpHandler.ServeHTTP(w, r)
}

func (s *Server) handleDownloadCA(w http.ResponseWriter, r *http.Request) {
	if s.ca == nil {
		writeError(w, http.StatusNotFound, "not_found", "CA certificate not found")
		return
	}
	w.Header().Set("Content-Type", "application/x-x509-ca-cert")
	w.Header().Set("Content-Disposition", "attachment; filename=\"ca.crt\"")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(s.ca.CACertPEM())
}


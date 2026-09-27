package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-gateway/internal/devicestore"
	"agent-gateway/internal/httpapi"
	"agent-gateway/internal/identity"
	gatewaymcp "agent-gateway/internal/mcp"
	"agent-gateway/internal/policy"
	"agent-gateway/internal/taskstore"
	official "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCommercialMCPWireAuthorization(t *testing.T) {
	for _, kind := range []string{"sse", "streamable"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			dir := t.TempDir()
			tasks, err := taskstore.Open(filepath.Join(dir, "tasks.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer tasks.Close()
			devices, err := devicestore.Open(filepath.Join(dir, "devices.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer devices.Close()
			policies, err := policy.Open(filepath.Join(dir, "policy.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer policies.Close()
			ca, err := identity.LoadOrGenerateCA(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := devices.Register(ctx, "node-allowed", strings.Repeat("a", 64), time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if err := devices.Register(ctx, "node-outside", strings.Repeat("b", 64), time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			principal, token, err := policies.Issue(ctx, policy.Viewer, []string{"node-allowed"}, time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			s, err := httpapi.NewSecureServer(tasks, ca, devices, policies)
			if err != nil {
				t.Fatal(err)
			}
			backend := &gatewaymcp.LocalBackend{Tasks: tasks, Devices: devices}
			if kind == "sse" {
				s.SetMCPHandler(gatewaymcp.NewSSEHandler(backend))
			} else {
				s.SetMCPHandler(gatewaymcp.NewStreamableHTTPHandler(backend))
			}
			cfg, err := s.BuildTLSConfig([]string{"127.0.0.1"})
			if err != nil {
				t.Fatal(err)
			}
			ts := httptest.NewUnstartedServer(s.Handler())
			ts.TLS = cfg
			ts.StartTLS()
			defer ts.Close()
			var transport official.Transport
			if kind == "sse" {
				transport = &official.SSEClientTransport{Endpoint: ts.URL + "/mcp?token=" + token, HTTPClient: ts.Client()}
			} else {
				transport = &official.StreamableClientTransport{Endpoint: ts.URL + "/mcp?token=" + token, HTTPClient: ts.Client()}
			}
			client := official.NewClient(&official.Implementation{Name: "commercial-audit", Version: "1"}, nil)
			session, err := client.Connect(ctx, transport, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			listed, err := session.ListTools(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("real %s initialize/list_tools succeeded; tools=%d", kind, len(listed.Tools))
			res, err := session.CallTool(ctx, &official.CallToolParams{Name: "task_submit", Arguments: map[string]any{"node_id": "node-outside", "instruction": "AUDIT_NO_EXECUTION"}})
			if err == nil && !res.IsError {
				t.Error("real MCP viewer submitted out-of-scope task")
			}
			devicesResult, err := session.CallTool(ctx, &official.CallToolParams{Name: "device_list", Arguments: map[string]any{}})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(devicesResult)
			if strings.Contains(string(raw), "node-outside") {
				t.Error("real MCP leaked out-of-scope device")
			}
			if err := policies.Revoke(ctx, principal.ID); err != nil {
				t.Fatal(err)
			}
			after, err := session.CallTool(ctx, &official.CallToolParams{Name: "device_list", Arguments: map[string]any{}})
			if err == nil && !after.IsError {
				t.Error("real MCP continued tools after credential revocation")
			}
		})
	}
}

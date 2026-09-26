package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"agent-gateway/internal/devicestore"
	"agent-gateway/internal/taskstore"
	official "github.com/modelcontextprotocol/go-sdk/mcp"
)

func setupTestMCP(t *testing.T) (*official.ClientSession, func()) {
	t.Helper()

	dataDir := t.TempDir()
	tasks, err := taskstore.Open(filepath.Join(dataDir, "tasks.sqlite"))
	if err != nil {
		t.Fatalf("open tasks store: %v", err)
	}
	devs, err := devicestore.Open(filepath.Join(dataDir, "devices.sqlite"))
	if err != nil {
		t.Fatalf("open devices store: %v", err)
	}

	backend := &LocalBackend{
		Tasks:   tasks,
		Devices: devs,
		DataDir: dataDir,
	}

	server := NewServer(backend)
	serverTransport, clientTransport := official.NewInMemoryTransports()

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		_ = server.Run(ctx, serverTransport)
	}()

	client := official.NewClient(&official.Implementation{
		Name:    "mcp-test-runner",
		Version: "1.0.0",
	}, nil)

	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		cancel()
		t.Fatalf("client connect: %v", err)
	}

	cleanup := func() {
		cancel()
		tasks.Close()
		devs.Close()
	}

	return session, cleanup
}

func TestMCPServer_ListTools(t *testing.T) {
	session, cleanup := setupTestMCP(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	toolsResult, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	expectedTools := map[string]bool{
		"task_submit":               false,
		"task_get":                  false,
		"task_cancel":               false,
		"device_list":               false,
		"doctor_diagnose":           false,
		"wait_task_result":          false,
		"handoff_to_computer_agent": false,
	}

	for _, tool := range toolsResult.Tools {
		if _, ok := expectedTools[tool.Name]; ok {
			expectedTools[tool.Name] = true
		}
	}

	for name, found := range expectedTools {
		if !found {
			t.Errorf("tool %s not registered on MCP server", name)
		}
	}
}

func TestMCPServer_TaskWorkflow(t *testing.T) {
	session, cleanup := setupTestMCP(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 1. Submit task
	submitArgs := SubmitTaskInput{
		NodeID:      "node-mcp-test",
		Instruction: "echo Hello MCP",
	}
	res, err := session.CallTool(ctx, &official.CallToolParams{
		Name:      "task_submit",
		Arguments: submitArgs,
	})
	if err != nil {
		t.Fatalf("call task_submit: %v", err)
	}
	if res.IsError {
		t.Fatalf("task_submit returned error: %+v", res.Content)
	}

	var submitOut SubmitTaskOutput
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*official.TextContent); ok {
			_ = json.Unmarshal([]byte(tc.Text), &submitOut)
		}
	}
	if submitOut.TaskID == "" {
		t.Fatalf("expected task ID in submit output, got %+v", submitOut)
	}

	// 2. Wait task (timeout 1s on queued task should return current state)
	waitArgs := WaitTaskInput{
		TaskID:         submitOut.TaskID,
		TimeoutSeconds: 1,
	}
	waitRes, err := session.CallTool(ctx, &official.CallToolParams{
		Name:      "wait_task_result",
		Arguments: waitArgs,
	})
	if err != nil {
		t.Fatalf("call wait_task_result: %v", err)
	}
	if waitRes.IsError {
		t.Fatalf("wait_task_result returned error: %+v", waitRes.Content)
	}

	// 3. Inspect task
	getArgs := GetTaskInput{
		TaskID: submitOut.TaskID,
	}
	getRes, err := session.CallTool(ctx, &official.CallToolParams{
		Name:      "task_get",
		Arguments: getArgs,
	})
	if err != nil {
		t.Fatalf("call task_get: %v", err)
	}
	if getRes.IsError {
		t.Fatalf("task_get returned error: %+v", getRes.Content)
	}

	// 4. Cancel task
	cancelArgs := CancelTaskInput{
		TaskID: submitOut.TaskID,
	}
	cancelRes, err := session.CallTool(ctx, &official.CallToolParams{
		Name:      "task_cancel",
		Arguments: cancelArgs,
	})
	if err != nil {
		t.Fatalf("call task_cancel: %v", err)
	}
	if cancelRes.IsError {
		t.Fatalf("task_cancel returned error: %+v", cancelRes.Content)
	}
}

func TestMCPServer_HandoffWorkflow(t *testing.T) {
	session, cleanup := setupTestMCP(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Call handoff with explicit node
	handoffArgs := HandoffInput{
		Instruction: "Check system memory",
		TargetNode:  "node-handoff-1",
		Context:     "User requested resource status on mobile",
	}
	res, err := session.CallTool(ctx, &official.CallToolParams{
		Name:      "handoff_to_computer_agent",
		Arguments: handoffArgs,
	})
	if err != nil {
		t.Fatalf("call handoff_to_computer_agent: %v", err)
	}
	if res.IsError {
		t.Fatalf("handoff returned error: %+v", res.Content)
	}

	var out SubmitTaskOutput
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*official.TextContent); ok {
			_ = json.Unmarshal([]byte(tc.Text), &out)
		}
	}
	if out.TaskID == "" || out.NodeID != "node-handoff-1" {
		t.Errorf("unexpected handoff result: %+v", out)
	}
}

func TestMCPServer_DeviceListAndDiagnose(t *testing.T) {
	session, cleanup := setupTestMCP(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Call device_list
	dRes, err := session.CallTool(ctx, &official.CallToolParams{
		Name:      "device_list",
		Arguments: ListDevicesInput{},
	})
	if err != nil {
		t.Fatalf("call device_list: %v", err)
	}
	if dRes.IsError {
		t.Fatalf("device_list returned error: %+v", dRes.Content)
	}

	// Call doctor_diagnose
	diagRes, err := session.CallTool(ctx, &official.CallToolParams{
		Name:      "doctor_diagnose",
		Arguments: DiagnoseInput{},
	})
	if err != nil {
		t.Fatalf("call doctor_diagnose: %v", err)
	}
	if diagRes.IsError {
		t.Fatalf("doctor_diagnose returned error: %+v", diagRes.Content)
	}
}

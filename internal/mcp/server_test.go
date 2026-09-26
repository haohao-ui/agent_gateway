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
		"task_submit":     false,
		"task_get":        false,
		"task_cancel":     false,
		"device_list":     false,
		"doctor_diagnose": false,
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

	// Unmarshal structured content
	var submitOut SubmitTaskOutput
	rawJSON, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	if err := json.Unmarshal(rawJSON, &submitOut); err != nil {
		t.Fatalf("unmarshal submit output: %v", err)
	}
	if submitOut.TaskID == "" || submitOut.State != "queued" {
		t.Fatalf("unexpected submit out: %+v", submitOut)
	}

	taskID := submitOut.TaskID

	// 2. Get task
	getArgs := GetTaskInput{TaskID: taskID}
	getRes, err := session.CallTool(ctx, &official.CallToolParams{
		Name:      "task_get",
		Arguments: getArgs,
	})
	if err != nil {
		t.Fatalf("call task_get: %v", err)
	}
	var getOut GetTaskOutput
	rawJSON, _ = json.Marshal(getRes.StructuredContent)
	_ = json.Unmarshal(rawJSON, &getOut)

	if getOut.TaskID != taskID || getOut.State != "queued" {
		t.Fatalf("unexpected get output: %+v", getOut)
	}

	// 3. Cancel task
	cancelArgs := CancelTaskInput{TaskID: taskID}
	cancelRes, err := session.CallTool(ctx, &official.CallToolParams{
		Name:      "task_cancel",
		Arguments: cancelArgs,
	})
	if err != nil {
		t.Fatalf("call task_cancel: %v", err)
	}
	var cancelOut CancelTaskOutput
	rawJSON, _ = json.Marshal(cancelRes.StructuredContent)
	_ = json.Unmarshal(rawJSON, &cancelOut)

	if cancelOut.TaskID != taskID || cancelOut.State != "cancelled" {
		t.Fatalf("unexpected cancel output: %+v", cancelOut)
	}

	// 4. Device list
	devRes, err := session.CallTool(ctx, &official.CallToolParams{
		Name:      "device_list",
		Arguments: ListDevicesInput{},
	})
	if err != nil {
		t.Fatalf("call device_list: %v", err)
	}
	var devOut ListDevicesOutput
	rawJSON, _ = json.Marshal(devRes.StructuredContent)
	_ = json.Unmarshal(rawJSON, &devOut)
	// Empty is fine as no devices enrolled yet

	// 5. Doctor diagnose
	docRes, err := session.CallTool(ctx, &official.CallToolParams{
		Name:      "doctor_diagnose",
		Arguments: DiagnoseInput{},
	})
	if err != nil {
		t.Fatalf("call doctor_diagnose: %v", err)
	}
	var docOut DiagnoseOutput
	rawJSON, _ = json.Marshal(docRes.StructuredContent)
	_ = json.Unmarshal(rawJSON, &docOut)
	if len(docOut.Checks) == 0 {
		t.Errorf("expected doctor checks to be non-empty")
	}
}

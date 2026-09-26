package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	official "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	ServerName    = "agent-gateway"
	ServerVersion = "0.1.0"
)

// NewServer builds an official MCP server configured with agent-gateway tools.
func NewServer(backend GatewayBackend) *official.Server {
	s := official.NewServer(&official.Implementation{
		Name:    ServerName,
		Version: ServerVersion,
	}, nil)

	registerTools(s, backend)
	return s
}

// RunStdio starts an MCP server loop over standard input/output.
func RunStdio(ctx context.Context, backend GatewayBackend) error {
	server := NewServer(backend)
	transport := &official.StdioTransport{}
	return server.Run(ctx, transport)
}

func registerTools(s *official.Server, backend GatewayBackend) {
	// 1. task_submit
	official.AddTool(s, &official.Tool{
		Name:        "task_submit",
		Description: "Submit a new execution task to a specific worker node on the mesh network.",
	}, func(ctx context.Context, req *official.CallToolRequest, in SubmitTaskInput) (*official.CallToolResult, SubmitTaskOutput, error) {
		capability := in.Capability
		if capability == "" {
			capability = "agent.run"
		}
		timeout := in.TimeoutSeconds
		if timeout <= 0 {
			timeout = 120
		}

		var rawInput []byte
		var jsonTest any
		if err := json.Unmarshal([]byte(in.Instruction), &jsonTest); err == nil {
			rawInput = []byte(in.Instruction)
		} else {
			rawInput, _ = json.Marshal(map[string]string{"instruction": in.Instruction})
		}

		task, err := backend.SubmitTask(ctx, in.NodeID, capability, rawInput, timeout)
		if err != nil {
			return nil, SubmitTaskOutput{}, fmt.Errorf("submit task failed: %w", err)
		}

		out := SubmitTaskOutput{
			TaskID:    task.ID,
			NodeID:    task.NodeID,
			State:     string(task.State),
			CreatedAt: task.CreatedAt.UTC().Format(time.RFC3339),
			Message:   fmt.Sprintf("Task %s successfully enqueued for node %s", task.ID, task.NodeID),
		}
		return nil, out, nil
	})

	// 2. task_get
	official.AddTool(s, &official.Tool{
		Name:        "task_get",
		Description: "Inspect the current status, attempt progress, and execution output of a task.",
	}, func(ctx context.Context, req *official.CallToolRequest, in GetTaskInput) (*official.CallToolResult, GetTaskOutput, error) {
		task, err := backend.GetTask(ctx, in.TaskID)
		if err != nil {
			return nil, GetTaskOutput{}, fmt.Errorf("get task failed: %w", err)
		}

		out := GetTaskOutput{
			TaskID:    task.ID,
			NodeID:    task.NodeID,
			State:     string(task.State),
			AttemptID: task.AttemptID,
			CreatedAt: task.CreatedAt.UTC().Format(time.RFC3339),
			UpdatedAt: task.UpdatedAt.UTC().Format(time.RFC3339),
		}
		if task.Result != nil {
			out.Output = task.Result.Text
			out.ExitCode = task.Result.ExitCode
			out.Error = task.Result.ErrorCode
		}
		return nil, out, nil
	})

	// 3. task_cancel
	official.AddTool(s, &official.Tool{
		Name:        "task_cancel",
		Description: "Request cancellation of a queued, leased or running task.",
	}, func(ctx context.Context, req *official.CallToolRequest, in CancelTaskInput) (*official.CallToolResult, CancelTaskOutput, error) {
		task, err := backend.CancelTask(ctx, in.TaskID)
		if err != nil {
			return nil, CancelTaskOutput{}, fmt.Errorf("cancel task failed: %w", err)
		}

		out := CancelTaskOutput{
			TaskID:  task.ID,
			State:   string(task.State),
			Message: fmt.Sprintf("Task %s cancellation requested (state: %s)", task.ID, task.State),
		}
		return nil, out, nil
	})

	// 4. device_list
	official.AddTool(s, &official.Tool{
		Name:        "device_list",
		Description: "List all enrolled worker machines/nodes and their certificate authorization status.",
	}, func(ctx context.Context, req *official.CallToolRequest, in ListDevicesInput) (*official.CallToolResult, ListDevicesOutput, error) {
		devices, err := backend.ListDevices(ctx)
		if err != nil {
			return nil, ListDevicesOutput{}, fmt.Errorf("list devices failed: %w", err)
		}

		var items []DeviceItem
		for _, d := range devices {
			items = append(items, DeviceItem{
				NodeID:      d.NodeID,
				Fingerprint: d.Fingerprint,
				Revoked:     d.Revoked,
				ExpiresAt:   d.ExpiresAt.UTC().Format(time.RFC3339),
			})
		}
		return nil, ListDevicesOutput{Devices: items}, nil
	})

	// 5. doctor_diagnose
	official.AddTool(s, &official.Tool{
		Name:        "doctor_diagnose",
		Description: "Run environmental and database health checks for the gateway and worker environment.",
	}, func(ctx context.Context, req *official.CallToolRequest, in DiagnoseInput) (*official.CallToolResult, DiagnoseOutput, error) {
		rep, err := backend.Diagnose(ctx)
		if err != nil {
			return nil, DiagnoseOutput{}, fmt.Errorf("diagnostics failed: %w", err)
		}

		var checks []CheckItem
		for _, c := range rep.Checks {
			checks = append(checks, CheckItem{
				Name:        c.Name,
				Status:      string(c.Status),
				Message:     c.Message,
				Remediation: c.Remediation,
			})
		}

		return nil, DiagnoseOutput{
			Healthy: rep.Healthy,
			Checks:  checks,
		}, nil
	})
}

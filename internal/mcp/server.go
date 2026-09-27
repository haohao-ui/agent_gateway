package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"agent-gateway/internal/policy"
	official "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	ServerName    = "agent-gateway"
	ServerVersion = "0.1.0"
)

func authorizeNodeAction(ctx context.Context, action string, nodeID string) error {
	p, ok := policy.PrincipalFromContext(ctx)
	if !ok {
		return nil
	}
	return policy.Authorize(p, action, nodeID)
}

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

// NewStreamableHTTPHandler returns an http.Handler serving streamable MCP over HTTP.
func NewStreamableHTTPHandler(backend GatewayBackend) http.Handler {
	return official.NewStreamableHTTPHandler(func(req *http.Request) *official.Server {
		return NewServer(backend)
	}, nil)
}

// NewSSEHandler returns an http.Handler serving SSE MCP sessions.
func NewSSEHandler(backend GatewayBackend) http.Handler {
	return official.NewSSEHandler(func(req *http.Request) *official.Server {
		return NewServer(backend)
	}, &official.SSEOptions{
		DisableLocalhostProtection: true,
	})
}

func registerTools(s *official.Server, backend GatewayBackend) {
	// 1. task_submit / submit_task handler
	submitHandler := func(ctx context.Context, req *official.CallToolRequest, in SubmitTaskInput) (*official.CallToolResult, SubmitTaskOutput, error) {
		if err := authorizeNodeAction(ctx, policy.ActionTaskSubmit, in.NodeID); err != nil {
			return nil, SubmitTaskOutput{}, fmt.Errorf("authorization denied: %w", err)
		}

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
	}
	official.AddTool(s, &official.Tool{
		Name:        "task_submit",
		Description: "Submit a new execution task to a specific worker node on the mesh network.",
	}, submitHandler)
	official.AddTool(s, &official.Tool{
		Name:        "submit_task",
		Description: "Alias for task_submit. Queue work on a registered computer/agent and return task ID.",
	}, submitHandler)

	// 2. task_get / get_task_result handler
	getHandler := func(ctx context.Context, req *official.CallToolRequest, in GetTaskInput) (*official.CallToolResult, GetTaskOutput, error) {
		task, err := backend.GetTask(ctx, in.TaskID)
		if err != nil {
			return nil, GetTaskOutput{}, fmt.Errorf("get task failed: %w", err)
		}
		if err := authorizeNodeAction(ctx, policy.ActionTaskRead, task.NodeID); err != nil {
			return nil, GetTaskOutput{}, fmt.Errorf("authorization denied: %w", err)
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
	}
	official.AddTool(s, &official.Tool{
		Name:        "task_get",
		Description: "Inspect the current status, attempt progress, and execution output of a task.",
	}, getHandler)
	official.AddTool(s, &official.Tool{
		Name:        "get_task_result",
		Description: "Alias for task_get. Fetch status and eventual execution result using the task ID.",
	}, getHandler)

	// 3. wait_task_result
	official.AddTool(s, &official.Tool{
		Name:        "wait_task_result",
		Description: "Synchronously wait up to timeout seconds for a task to reach terminal state, returning execution result or current progress.",
	}, func(ctx context.Context, req *official.CallToolRequest, in WaitTaskInput) (*official.CallToolResult, GetTaskOutput, error) {
		timeout := time.Duration(in.TimeoutSeconds) * time.Second
		if in.TimeoutSeconds <= 0 {
			timeout = 30 * time.Second
		}
		initialTask, err := backend.GetTask(ctx, in.TaskID)
		if err == nil {
			if err := authorizeNodeAction(ctx, policy.ActionTaskRead, initialTask.NodeID); err != nil {
				return nil, GetTaskOutput{}, fmt.Errorf("authorization denied: %w", err)
			}
		}
		task, err := backend.WaitTask(ctx, in.TaskID, timeout)
		if err != nil {
			return nil, GetTaskOutput{}, fmt.Errorf("wait task failed: %w", err)
		}
		if err := authorizeNodeAction(ctx, policy.ActionTaskRead, task.NodeID); err != nil {
			return nil, GetTaskOutput{}, fmt.Errorf("authorization denied: %w", err)
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

	// 3b. node_execute / execute_on_node (all-in-one command execution)
	executeHandler := func(ctx context.Context, req *official.CallToolRequest, in NodeExecuteInput) (*official.CallToolResult, NodeExecuteOutput, error) {
		if err := authorizeNodeAction(ctx, policy.ActionTaskSubmit, in.NodeID); err != nil {
			return nil, NodeExecuteOutput{}, fmt.Errorf("authorization denied: %w", err)
		}

		capability := in.Capability
		if capability == "" {
			capability = "bash"
		}
		timeout := in.TimeoutSeconds
		if timeout <= 0 {
			timeout = 60
		}
		if timeout > 300 {
			timeout = 300
		}

		rawInput, _ := json.Marshal(map[string]string{
			"instruction": in.Command,
			"prompt":      in.Command,
		})

		task, err := backend.SubmitTask(ctx, in.NodeID, capability, rawInput, timeout)
		if err != nil {
			if capability == "bash" {
				task, err = backend.SubmitTask(ctx, in.NodeID, "agent.run", rawInput, timeout)
			}
			if err != nil {
				return nil, NodeExecuteOutput{}, fmt.Errorf("submit task to node %s failed: %w", in.NodeID, err)
			}
		}

		waitTimeout := time.Duration(timeout) * time.Second
		finishedTask, err := backend.WaitTask(ctx, task.ID, waitTimeout)
		if err != nil {
			return nil, NodeExecuteOutput{
				TaskID: task.ID,
				NodeID: task.NodeID,
				State:  string(task.State),
				Error:  fmt.Sprintf("wait task failed: %v", err),
			}, nil
		}

		out := NodeExecuteOutput{
			TaskID: finishedTask.ID,
			NodeID: finishedTask.NodeID,
			State:  string(finishedTask.State),
		}
		if finishedTask.Result != nil {
			out.Output = finishedTask.Result.Text
			out.ExitCode = finishedTask.Result.ExitCode
			out.Error = finishedTask.Result.ErrorCode
		}
		return nil, out, nil
	}
	official.AddTool(s, &official.Tool{
		Name:        "node_execute",
		Description: "All-in-one tool: submit a command to a remote worker node, wait for completion, and directly return output and exit code.",
	}, executeHandler)
	official.AddTool(s, &official.Tool{
		Name:        "execute_on_node",
		Description: "Alias for node_execute. Execute a command on a remote worker node and synchronously return result.",
	}, executeHandler)

	// 4. handoff_to_computer_agent
	official.AddTool(s, &official.Tool{
		Name:        "handoff_to_computer_agent",
		Description: "Hand off a mobile/desktop conversation task to a computer agent. If target_node is omitted, the gateway automatically selects an online node.",
	}, func(ctx context.Context, req *official.CallToolRequest, in HandoffInput) (*official.CallToolResult, SubmitTaskOutput, error) {
		target := in.TargetNode
		if target == "" {
			devices, err := backend.ListDevices(ctx)
			if err != nil {
				return nil, SubmitTaskOutput{}, fmt.Errorf("list candidate devices: %w", err)
			}
			for _, d := range devices {
				if !d.Revoked && time.Now().Before(d.ExpiresAt) {
					if authorizeNodeAction(ctx, policy.ActionTaskSubmit, d.NodeID) == nil {
						target = d.NodeID
						break
					}
				}
			}
			if target == "" {
				return nil, SubmitTaskOutput{}, errors.New("no authorized active worker node found on the mesh for this principal")
			}
		}

		if err := authorizeNodeAction(ctx, policy.ActionTaskSubmit, target); err != nil {
			return nil, SubmitTaskOutput{}, fmt.Errorf("authorization denied: %w", err)
		}

		capability := in.Agent
		if capability == "" {
			capability = "agent.run"
		}
		timeout := in.TimeoutSeconds
		if timeout <= 0 {
			timeout = 120
		}

		instruction := in.Instruction
		if in.Context != "" {
			instruction = fmt.Sprintf("上下文：\n%s\n\n任务：\n%s", in.Context, instruction)
		}

		rawInput, _ := json.Marshal(map[string]string{
			"instruction": instruction,
		})

		task, err := backend.SubmitTask(ctx, target, capability, rawInput, timeout)
		if err != nil {
			return nil, SubmitTaskOutput{}, fmt.Errorf("handoff task failed: %w", err)
		}

		out := SubmitTaskOutput{
			TaskID:    task.ID,
			NodeID:    task.NodeID,
			State:     string(task.State),
			CreatedAt: task.CreatedAt.UTC().Format(time.RFC3339),
			Message:   fmt.Sprintf("Task %s successfully handed off to node %s", task.ID, task.NodeID),
		}
		return nil, out, nil
	})

	// 5. task_cancel / cancel_task handler
	cancelHandler := func(ctx context.Context, req *official.CallToolRequest, in CancelTaskInput) (*official.CallToolResult, CancelTaskOutput, error) {
		task, err := backend.GetTask(ctx, in.TaskID)
		if err != nil {
			return nil, CancelTaskOutput{}, fmt.Errorf("cancel task failed: %w", err)
		}
		if err := authorizeNodeAction(ctx, policy.ActionTaskCancel, task.NodeID); err != nil {
			return nil, CancelTaskOutput{}, fmt.Errorf("authorization denied: %w", err)
		}

		task, err = backend.CancelTask(ctx, in.TaskID)
		if err != nil {
			return nil, CancelTaskOutput{}, fmt.Errorf("cancel task failed: %w", err)
		}

		out := CancelTaskOutput{
			TaskID:  task.ID,
			State:   string(task.State),
			Message: fmt.Sprintf("Task %s cancellation requested (state: %s)", task.ID, task.State),
		}
		return nil, out, nil
	}
	official.AddTool(s, &official.Tool{
		Name:        "task_cancel",
		Description: "Request cancellation of a queued, leased or running task.",
	}, cancelHandler)
	official.AddTool(s, &official.Tool{
		Name:        "cancel_task",
		Description: "Alias for task_cancel. Request cancellation of an existing task.",
	}, cancelHandler)

	// 6. device_list / list_devices handler
	deviceListHandler := func(ctx context.Context, req *official.CallToolRequest, in ListDevicesInput) (*official.CallToolResult, ListDevicesOutput, error) {
		p, hasPrincipal := policy.PrincipalFromContext(ctx)

		if detailed, ok := backend.(DetailedDeviceProvider); ok {
			items, err := detailed.ListDetailedDevices(ctx)
			if err == nil {
				if hasPrincipal && p.Role != policy.Admin {
					var filtered []DeviceItem
					for _, item := range items {
						if policy.Authorize(p, policy.ActionTaskRead, item.NodeID) == nil {
							filtered = append(filtered, item)
						}
					}
					return nil, ListDevicesOutput{Devices: filtered}, nil
				}
				return nil, ListDevicesOutput{Devices: items}, nil
			}
		}
		devices, err := backend.ListDevices(ctx)
		if err != nil {
			return nil, ListDevicesOutput{}, fmt.Errorf("list devices failed: %w", err)
		}

		var items []DeviceItem
		for _, d := range devices {
			if hasPrincipal && p.Role != policy.Admin {
				if err := policy.Authorize(p, policy.ActionTaskRead, d.NodeID); err != nil {
					continue
				}
			}
			items = append(items, DeviceItem{
				NodeID:      d.NodeID,
				Fingerprint: d.Fingerprint,
				Revoked:     d.Revoked,
				ExpiresAt:   d.ExpiresAt.UTC().Format(time.RFC3339),
			})
		}
		return nil, ListDevicesOutput{Devices: items}, nil
	}
	official.AddTool(s, &official.Tool{
		Name:        "device_list",
		Description: "List all enrolled worker machines/nodes, online status, and detected tool capabilities.",
	}, deviceListHandler)
	official.AddTool(s, &official.Tool{
		Name:        "list_devices",
		Description: "Alias for device_list. List computers, detected installed software, and runnable agent adapters.",
	}, deviceListHandler)

	// 7. doctor_diagnose
	official.AddTool(s, &official.Tool{
		Name:        "doctor_diagnose",
		Description: "Run environmental and database health checks for the gateway and worker environment.",
	}, func(ctx context.Context, req *official.CallToolRequest, in DiagnoseInput) (*official.CallToolResult, DiagnoseOutput, error) {
		if p, ok := policy.PrincipalFromContext(ctx); ok && p.Role != policy.Admin {
			return nil, DiagnoseOutput{}, errors.New("authorization denied: diagnostic tools require admin role")
		}

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

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"agent-gateway/internal/node"
	"agent-gateway/internal/protocol"
)

const taskHelp = `usage:
  mesh task submit --node-dir ./node --capability agent.run --input '{"prompt":"..."}' [flags]
  mesh task get --node-dir ./node <task-id>
  mesh task cancel --node-dir ./node <task-id>

Operator mode: add --token-file FILE --server https://HOST --ca FILE; submit also requires --node-id ID. Do not combine with --node-dir.
  mesh task requeue --server URL --ca FILE --token-file FILE <task-id>
  mesh task resolve --server URL --ca FILE --token-file FILE [--state failed|cancelled] [--reason TEXT] <task-id>
  mesh task list --server URL --ca FILE --token-file FILE [--state unknown]

The legacy node task API is authenticated with the machine's certificate, so these commands
speak as the node in --node-dir: a task is queued for that machine and only that
machine can read or cancel it.
`

// runTask dispatches the task verbs.
func runTask(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, taskHelp)
		return errors.New("a task verb is required")
	}

	switch args[0] {
	case "submit":
		return runTaskSubmit(ctx, args[1:])
	case "get":
		return runTaskGet(ctx, args[1:])
	case "cancel":
		return runTaskCancel(ctx, args[1:])
	case "requeue":
		return runTaskRequeue(ctx, args[1:])
	case "resolve":
		return runTaskResolve(ctx, args[1:])
	case "list":
		return runTaskList(ctx, args[1:])
	case "help", "-h", "--help":
		fmt.Print(taskHelp)
		return nil
	default:
		return fmt.Errorf("unknown task verb %q", args[0])
	}
}

// taskClient builds a client from the identity in nodeDir.
func taskClient(ctx context.Context, nodeDir, serverOverride string) (*node.GatewayClient, node.PairState, error) {
	if nodeDir == "" {
		nodeDir = defaultNodeDir()
	}
	state, err := node.LoadState(nodeDir)
	if err != nil {
		return nil, node.PairState{}, fmt.Errorf("%w (did you run 'mesh pair'?)", err)
	}
	serverURL := state.ServerURL
	if serverOverride != "" {
		serverURL = serverOverride
	}

	caPEM, err := os.ReadFile(filepath.Join(nodeDir, node.NodeCAFile))
	if err != nil {
		return nil, node.PairState{}, fmt.Errorf("read the CA certificate: %w", err)
	}
	certPEM, err := os.ReadFile(filepath.Join(nodeDir, node.NodeCertFile))
	if err != nil {
		return nil, node.PairState{}, fmt.Errorf("read the node certificate: %w", err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(nodeDir, node.NodeKeyFile))
	if err != nil {
		return nil, node.PairState{}, fmt.Errorf("read the node key: %w", err)
	}

	client, err := node.NewGatewayClient(serverURL, caPEM, certPEM, keyPEM, 0)
	if err != nil {
		return nil, node.PairState{}, err
	}
	return client, state, nil
}

func runTaskSubmit(ctx context.Context, args []string) error {
	cmd := newCommand("task submit", "Queue a task for the machine in --node-dir.")
	nodeDir := cmd.flags.String("node-dir", defaultNodeDir(), "directory holding the machine identity (default: ~/.agent-mesh-node)")
	server := cmd.flags.String("server", envOrDefault([]string{"MESH_SERVER_URL", "MESH_SERVER"}, ""), "override the gateway URL recorded at pairing time")
	tokenFile := cmd.flags.String("token-file", envString("MESH_TOKEN_FILE", ""), "operator credential file; requires --server and --ca")
	caFile := cmd.flags.String("ca", envOrDefault([]string{"MESH_CA_FILE", "MESH_CA"}, ""), "trusted gateway CA file")
	nodeID := cmd.flags.String("node-id", envString("MESH_NODE_ID", ""), "target node for operator submission")
	capability := cmd.flags.String("capability", "agent.run", "capability name")
	version := cmd.flags.Int("capability-version", 1, "capability version")
	input := cmd.flags.String("input", "", "capability input as JSON, for example '{\"prompt\":\"hi\"}'")
	timeout := cmd.flags.Int("timeout", 300, "task timeout in seconds")
	idempotencyKey := cmd.flags.String("key", "", "idempotency key: resubmitting the same key and input returns the first task")
	if err := cmd.flags.Parse(args); err != nil {
		return err
	}

	if *input == "" {
		return errors.New("--input is required")
	}
	if !json.Valid([]byte(*input)) {
		return errors.New("--input must be valid JSON")
	}
	if _, err := cmd.logger(); err != nil {
		return err
	}

	if *tokenFile != "" {
		if err := rejectNodeIdentity(cmd); err != nil {
			return err
		}
		return operatorTaskCommand(ctx, "submit", *server, *caFile, *tokenFile, *nodeID, "", protocol.SubmitRequest{Capability: *capability, CapabilityVersion: *version, Input: json.RawMessage(*input), TimeoutSeconds: *timeout, IdempotencyKey: *idempotencyKey})
	}
	client, state, err := taskClient(ctx, *nodeDir, *server)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()

	task, err := client.Submit(ctx, protocol.SubmitRequest{
		NodeID:            state.NodeID,
		Capability:        *capability,
		CapabilityVersion: *version,
		Input:             json.RawMessage(*input),
		TimeoutSeconds:    *timeout,
		IdempotencyKey:    *idempotencyKey,
	})
	if err != nil {
		return err
	}
	return printTask(task)
}

func runTaskGet(ctx context.Context, args []string) error {
	cmd := newCommand("task get", "Show one task owned by the machine in --node-dir.")
	nodeDir := cmd.flags.String("node-dir", defaultNodeDir(), "directory holding the machine identity (default: ~/.agent-mesh-node)")
	server := cmd.flags.String("server", envOrDefault([]string{"MESH_SERVER_URL", "MESH_SERVER"}, ""), "override the gateway URL recorded at pairing time")
	tokenFile := cmd.flags.String("token-file", envString("MESH_TOKEN_FILE", ""), "operator credential file; requires --server and --ca")
	caFile := cmd.flags.String("ca", envOrDefault([]string{"MESH_CA_FILE", "MESH_CA"}, ""), "trusted gateway CA file")
	if err := cmd.flags.Parse(args); err != nil {
		return err
	}
	if cmd.flags.NArg() != 1 {
		return errors.New("exactly one task id is required")
	}

	if *tokenFile != "" {
		if err := rejectNodeIdentity(cmd); err != nil {
			return err
		}
		return operatorTaskCommand(ctx, "get", *server, *caFile, *tokenFile, "", cmd.flags.Arg(0), protocol.SubmitRequest{})
	}
	client, _, err := taskClient(ctx, *nodeDir, *server)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()

	task, err := client.Get(ctx, cmd.flags.Arg(0))
	if err != nil {
		return err
	}
	return printTask(task)
}

func runTaskCancel(ctx context.Context, args []string) error {
	cmd := newCommand("task cancel", "Ask for a task owned by the machine in --node-dir to stop.")
	nodeDir := cmd.flags.String("node-dir", defaultNodeDir(), "directory holding the machine identity (default: ~/.agent-mesh-node)")
	server := cmd.flags.String("server", envOrDefault([]string{"MESH_SERVER_URL", "MESH_SERVER"}, ""), "override the gateway URL recorded at pairing time")
	tokenFile := cmd.flags.String("token-file", envString("MESH_TOKEN_FILE", ""), "operator credential file; requires --server and --ca")
	caFile := cmd.flags.String("ca", envOrDefault([]string{"MESH_CA_FILE", "MESH_CA"}, ""), "trusted gateway CA file")
	if err := cmd.flags.Parse(args); err != nil {
		return err
	}
	if cmd.flags.NArg() != 1 {
		return errors.New("exactly one task id is required")
	}

	if *tokenFile != "" {
		if err := rejectNodeIdentity(cmd); err != nil {
			return err
		}
		return operatorTaskCommand(ctx, "cancel", *server, *caFile, *tokenFile, "", cmd.flags.Arg(0), protocol.SubmitRequest{})
	}
	client, _, err := taskClient(ctx, *nodeDir, *server)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()

	task, err := client.Cancel(ctx, cmd.flags.Arg(0))
	if err != nil {
		return err
	}
	fmt.Printf("cancellation recorded; the task is now %s\n", task.State)
	return printTask(task)
}

// printTask writes the gateway's view of a task. The lease token is not part of
// protocol.Task, so a listing can never leak the attempt credential.
func printTask(task protocol.Task) error {
	encoded, err := json.MarshalIndent(task, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the task: %w", err)
	}
	fmt.Printf("%s\n", encoded)
	return nil
}

func rejectNodeIdentity(cmd *command) error {
	var err error
	cmd.flags.Visit(func(f *flag.Flag) {
		if f.Name == "node-dir" {
			err = errors.New("--node-dir and --token-file cannot be combined")
		}
	})
	return err
}

func runTaskRequeue(ctx context.Context, args []string) error {
	cmd := newCommand("task requeue", "Requeue a task parked in unknown state back to queued (operator only).")
	server := cmd.flags.String("server", envOrDefault([]string{"MESH_SERVER_URL", "MESH_SERVER"}, ""), "gateway HTTPS URL")
	tokenFile := cmd.flags.String("token-file", envString("MESH_TOKEN_FILE", ""), "operator credential file")
	caFile := cmd.flags.String("ca", envOrDefault([]string{"MESH_CA_FILE", "MESH_CA"}, ""), "trusted gateway CA file")
	if err := cmd.flags.Parse(args); err != nil {
		return err
	}
	if cmd.flags.NArg() != 1 {
		return errors.New("exactly one task id is required")
	}
	if *tokenFile == "" || *server == "" || *caFile == "" {
		return errors.New("requeue requires --server, --ca, and --token-file")
	}
	return operatorTaskCommand(ctx, "requeue", *server, *caFile, *tokenFile, "", cmd.flags.Arg(0), nil)
}

func runTaskResolve(ctx context.Context, args []string) error {
	cmd := newCommand("task resolve", "Resolve a task parked in unknown state as failed or cancelled (operator only).")
	server := cmd.flags.String("server", envOrDefault([]string{"MESH_SERVER_URL", "MESH_SERVER"}, ""), "gateway HTTPS URL")
	tokenFile := cmd.flags.String("token-file", envString("MESH_TOKEN_FILE", ""), "operator credential file")
	caFile := cmd.flags.String("ca", envOrDefault([]string{"MESH_CA_FILE", "MESH_CA"}, ""), "trusted gateway CA file")
	state := cmd.flags.String("state", "failed", "terminal state: failed or cancelled")
	reason := cmd.flags.String("reason", "operator resolved unknown task", "rationale for terminal resolution")
	if err := cmd.flags.Parse(args); err != nil {
		return err
	}
	if cmd.flags.NArg() != 1 {
		return errors.New("exactly one task id is required")
	}
	if *tokenFile == "" || *server == "" || *caFile == "" {
		return errors.New("resolve requires --server, --ca, and --token-file")
	}
	body := map[string]any{
		"state":      *state,
		"text":       *reason,
		"error_code": "operator_resolved",
		"exit_code":  -1,
	}
	return operatorTaskCommand(ctx, "resolve", *server, *caFile, *tokenFile, "", cmd.flags.Arg(0), body)
}

func runTaskList(ctx context.Context, args []string) error {
	cmd := newCommand("task list", "List tasks (operator only).")
	server := cmd.flags.String("server", envOrDefault([]string{"MESH_SERVER_URL", "MESH_SERVER"}, ""), "gateway HTTPS URL")
	tokenFile := cmd.flags.String("token-file", envString("MESH_TOKEN_FILE", ""), "operator credential file")
	caFile := cmd.flags.String("ca", envOrDefault([]string{"MESH_CA_FILE", "MESH_CA"}, ""), "trusted gateway CA file")
	state := cmd.flags.String("state", "unknown", "filter tasks by state (default unknown)")
	if err := cmd.flags.Parse(args); err != nil {
		return err
	}
	if *tokenFile == "" || *server == "" || *caFile == "" {
		return errors.New("list requires --server, --ca, and --token-file")
	}
	return operatorTaskCommand(ctx, "list", *server, *caFile, *tokenFile, "", *state, nil)
}

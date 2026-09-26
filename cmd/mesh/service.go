package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"agent-gateway/internal/service"
)

const serviceUsage = `usage: mesh service <action> [flags]

Actions:
  install       install background service for server or node
  uninstall     uninstall background service
  start         start the background service
  stop          stop the background service
  status        display service status and process information

Common flags:
  --role server|node      role to manage (default: server)

Install flags:
  --bin <path>            executable binary path (default: current binary)
  --working-dir <path>    working directory (default: user home)
  --log-dir <path>        directory for service stdout/stderr logs
  --addr <address>        listen address (server role, default: 127.0.0.1:8443)
  --data-dir <path>       gateway data directory (server role)
  --config <path>         node config path (node role, default: ./node/node.json)
  --node-dir <path>       node journal directory (node role)
  --args <args>           additional command-line arguments to pass

Status flags:
  --json                  output status in JSON format
`

func runService(ctx context.Context, args []string) error {
	if len(args) < 1 {
		fmt.Fprint(os.Stderr, serviceUsage)
		return errors.New("subcommand required: install, uninstall, start, stop or status")
	}

	action := args[0]
	subArgs := args[1:]
	mgr := service.NewManager()

	switch action {
	case "install":
		return runServiceInstall(ctx, mgr, subArgs)
	case "uninstall":
		return runServiceUninstall(ctx, mgr, subArgs)
	case "start":
		return runServiceStart(ctx, mgr, subArgs)
	case "stop":
		return runServiceStop(ctx, mgr, subArgs)
	case "status":
		return runServiceStatus(ctx, mgr, subArgs)
	case "help", "-h", "--help":
		fmt.Print(serviceUsage)
		return nil
	default:
		return fmt.Errorf("unknown service action %q\n\n%s", action, serviceUsage)
	}
}

func runServiceInstall(ctx context.Context, mgr service.Manager, args []string) error {
	fs := flag.NewFlagSet("service install", flag.ExitOnError)
	role := fs.String("role", "server", "role to install: server or node")
	bin := fs.String("bin", "", "executable binary path (default: current executable)")
	workingDir := fs.String("working-dir", "", "working directory")
	logDir := fs.String("log-dir", "", "directory for stdout/stderr logs")
	addr := fs.String("addr", "", "listen address (server role)")
	dataDir := fs.String("data-dir", "", "data directory (server role)")
	configPath := fs.String("config", "", "configuration file (node role)")
	nodeDir := fs.String("node-dir", "", "node journal/data directory (node role)")
	extraArgs := fs.String("args", "", "extra arguments to pass")

	if err := fs.Parse(args); err != nil {
		return err
	}

	roleName := strings.ToLower(strings.TrimSpace(*role))
	if err := service.ValidateRole(roleName); err != nil {
		return err
	}

	var serviceArgs []string
	switch roleName {
	case service.RoleServer:
		serviceArgs = append(serviceArgs, "server")
		if *addr != "" {
			serviceArgs = append(serviceArgs, "--addr", *addr)
		}
		if *dataDir != "" {
			serviceArgs = append(serviceArgs, "--data-dir", *dataDir)
		}
	case service.RoleNode:
		serviceArgs = append(serviceArgs, "node")
		if *configPath != "" {
			serviceArgs = append(serviceArgs, "--config", *configPath)
		}
		if *nodeDir != "" {
			serviceArgs = append(serviceArgs, "--dir", *nodeDir)
		}
	}

	if *extraArgs != "" {
		for _, arg := range strings.Fields(*extraArgs) {
			if trimmed := strings.TrimSpace(arg); trimmed != "" {
				serviceArgs = append(serviceArgs, trimmed)
			}
		}
	}

	cfg := service.Config{
		Role:       roleName,
		BinaryPath: *bin,
		Args:       serviceArgs,
		WorkingDir: *workingDir,
		LogDir:     *logDir,
	}

	if err := mgr.Install(ctx, cfg); err != nil {
		return fmt.Errorf("install service: %w", err)
	}

	status, err := mgr.Status(ctx, roleName)
	if err != nil {
		fmt.Printf("Service %s installed successfully.\n", roleName)
		return nil
	}

	fmt.Printf("Service %q installed successfully:\n", roleName)
	fmt.Printf("  platform:     %s\n", status.Platform)
	if status.ServiceFile != "" {
		fmt.Printf("  service file: %s\n", status.ServiceFile)
	}
	if status.LogFile != "" {
		fmt.Printf("  log file:     %s\n", status.LogFile)
	}
	if status.Running {
		fmt.Printf("  status:       running (PID: %d)\n", status.PID)
	} else {
		fmt.Printf("  status:       installed (not running)\n")
	}

	return nil
}

func runServiceUninstall(ctx context.Context, mgr service.Manager, args []string) error {
	fs := flag.NewFlagSet("service uninstall", flag.ExitOnError)
	role := fs.String("role", "server", "role to uninstall: server or node")
	if err := fs.Parse(args); err != nil {
		return err
	}

	roleName := strings.ToLower(strings.TrimSpace(*role))
	if err := service.ValidateRole(roleName); err != nil {
		return err
	}

	if err := mgr.Uninstall(ctx, roleName); err != nil {
		return fmt.Errorf("uninstall service: %w", err)
	}

	fmt.Printf("Service %q uninstalled successfully.\n", roleName)
	return nil
}

func runServiceStart(ctx context.Context, mgr service.Manager, args []string) error {
	fs := flag.NewFlagSet("service start", flag.ExitOnError)
	role := fs.String("role", "server", "role to start: server or node")
	if err := fs.Parse(args); err != nil {
		return err
	}

	roleName := strings.ToLower(strings.TrimSpace(*role))
	if err := service.ValidateRole(roleName); err != nil {
		return err
	}

	if err := mgr.Start(ctx, roleName); err != nil {
		return fmt.Errorf("start service: %w", err)
	}

	fmt.Printf("Service %q started.\n", roleName)
	return nil
}

func runServiceStop(ctx context.Context, mgr service.Manager, args []string) error {
	fs := flag.NewFlagSet("service stop", flag.ExitOnError)
	role := fs.String("role", "server", "role to stop: server or node")
	if err := fs.Parse(args); err != nil {
		return err
	}

	roleName := strings.ToLower(strings.TrimSpace(*role))
	if err := service.ValidateRole(roleName); err != nil {
		return err
	}

	if err := mgr.Stop(ctx, roleName); err != nil {
		return fmt.Errorf("stop service: %w", err)
	}

	fmt.Printf("Service %q stopped.\n", roleName)
	return nil
}

func runServiceStatus(ctx context.Context, mgr service.Manager, args []string) error {
	fs := flag.NewFlagSet("service status", flag.ExitOnError)
	role := fs.String("role", "server", "role to check: server or node")
	asJSON := fs.Bool("json", false, "output in JSON format")
	if err := fs.Parse(args); err != nil {
		return err
	}

	roleName := strings.ToLower(strings.TrimSpace(*role))
	if err := service.ValidateRole(roleName); err != nil {
		return err
	}

	status, err := mgr.Status(ctx, roleName)
	if err != nil {
		return fmt.Errorf("check service status: %w", err)
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	}

	fmt.Printf("Service Status (%s):\n", roleName)
	fmt.Printf("  platform:     %s\n", status.Platform)
	fmt.Printf("  installed:    %t\n", status.Installed)
	if status.Running {
		fmt.Printf("  running:      true (PID: %d)\n", status.PID)
	} else {
		fmt.Printf("  running:      false\n")
	}
	if status.ServiceFile != "" {
		fmt.Printf("  service file: %s\n", status.ServiceFile)
	}
	if status.LogFile != "" {
		fmt.Printf("  log file:     %s\n", status.LogFile)
	}
	if status.Details != "" {
		fmt.Printf("  details:      %s\n", status.Details)
	}

	return nil
}

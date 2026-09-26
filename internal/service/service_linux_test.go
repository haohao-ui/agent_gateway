//go:build linux

package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxManagerLifecycle(t *testing.T) {
	tmpDir := t.TempDir()
	logDir := filepath.Join(tmpDir, "logs")

	var executedCmds []string
	mockRunner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		cmdStr := name + " " + strings.Join(args, " ")
		executedCmds = append(executedCmds, cmdStr)

		if name == "systemctl" && len(args) >= 3 && args[1] == "show" {
			if strings.Contains(args[2], "server") {
				return []byte("MainPID=4242\nActiveState=active\nSubState=running\n"), nil
			}
			return []byte("MainPID=0\nActiveState=inactive\nSubState=dead\n"), nil
		}
		return []byte(""), nil
	}

	mgr := NewLinuxManager(tmpDir, mockRunner)

	cfg := Config{
		Role:       "server",
		BinaryPath: "/tmp/mesh",
		Args:       []string{"server"},
		ServiceDir: tmpDir,
		LogDir:     logDir,
	}

	// 1. Install
	if err := mgr.Install(context.Background(), cfg); err != nil {
		t.Fatalf("Install failed: %v", err)
	}

	expectedService := filepath.Join(tmpDir, "agent-gateway-server.service")
	if _, err := os.Stat(expectedService); os.IsNotExist(err) {
		t.Fatalf("expected service unit file %s to be created", expectedService)
	}

	// 2. Status
	status, err := mgr.Status(context.Background(), "server")
	if err != nil {
		t.Fatalf("Status failed: %v", err)
	}
	if !status.Installed {
		t.Errorf("expected Installed to be true")
	}
	if !status.Running || status.PID != 4242 {
		t.Errorf("expected Running=true and PID=4242, got running=%v, pid=%d", status.Running, status.PID)
	}

	// 3. Start & Stop
	if err := mgr.Start(context.Background(), "server"); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if err := mgr.Stop(context.Background(), "server"); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	// 4. Uninstall
	if err := mgr.Uninstall(context.Background(), "server"); err != nil {
		t.Fatalf("Uninstall failed: %v", err)
	}
	if _, err := os.Stat(expectedService); !os.IsNotExist(err) {
		t.Errorf("expected service unit file to be removed after uninstall")
	}

	if err := mgr.Uninstall(context.Background(), "server"); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("expected ErrNotInstalled on second uninstall, got %v", err)
	}
}

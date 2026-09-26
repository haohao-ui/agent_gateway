//go:build darwin

package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDarwinManagerLifecycle(t *testing.T) {
	tmpDir := t.TempDir()
	logDir := filepath.Join(tmpDir, "logs")

	var executedCmds []string
	mockRunner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		cmdStr := name + " " + strings.Join(args, " ")
		executedCmds = append(executedCmds, cmdStr)

		if name == "launchctl" && len(args) >= 2 && args[0] == "list" {
			if args[1] == "com.agent-gateway.server" {
				return []byte("{\n\t\"PID\" = 4242;\n\t\"LastExitStatus\" = 0;\n};\n"), nil
			}
			return []byte("Could not find service"), errors.New("exit status 1")
		}
		return []byte("ok"), nil
	}

	mgr := NewDarwinManager(tmpDir, mockRunner)

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

	expectedPlist := filepath.Join(tmpDir, "com.agent-gateway.server.plist")
	if _, err := os.Stat(expectedPlist); os.IsNotExist(err) {
		t.Fatalf("expected plist file %s to be created", expectedPlist)
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

	// Status for uninstalled role
	nodeStatus, err := mgr.Status(context.Background(), "node")
	if err != nil {
		t.Fatalf("Status node failed: %v", err)
	}
	if nodeStatus.Installed {
		t.Errorf("expected node to not be installed")
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
	if _, err := os.Stat(expectedPlist); !os.IsNotExist(err) {
		t.Errorf("expected plist file to be removed after uninstall")
	}

	// Verify uninstalled error
	if err := mgr.Uninstall(context.Background(), "server"); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("expected ErrNotInstalled on second uninstall, got %v", err)
	}
}

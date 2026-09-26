//go:build linux

package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type linuxManager struct {
	execCommand func(ctx context.Context, name string, args ...string) ([]byte, error)
	customDir   string
}

// NewManager creates a platform-native service manager for Linux.
func NewManager() Manager {
	return &linuxManager{
		execCommand: defaultExecCommand,
	}
}

// NewLinuxManager creates a Linux service manager with custom command runner and optional service directory.
func NewLinuxManager(dir string, cmdRunner func(ctx context.Context, name string, args ...string) ([]byte, error)) Manager {
	if cmdRunner == nil {
		cmdRunner = defaultExecCommand
	}
	return &linuxManager{
		execCommand: cmdRunner,
		customDir:   dir,
	}
}

func (m *linuxManager) getServiceDir(cfgDir string) (string, error) {
	if cfgDir != "" {
		return cfgDir, nil
	}
	if m.customDir != "" {
		return m.customDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("lookup home directory: %w", err)
	}
	return filepath.Join(home, ".config", "systemd", "user"), nil
}

func (m *linuxManager) getServicePath(role string, cfgDir string) (string, error) {
	dir, err := m.getServiceDir(cfgDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, fmt.Sprintf("agent-gateway-%s.service", role)), nil
}

func (m *linuxManager) unitName(role string) string {
	return fmt.Sprintf("agent-gateway-%s.service", role)
}

func (m *linuxManager) Install(ctx context.Context, cfg Config) error {
	if err := NormalizeConfig(&cfg); err != nil {
		return err
	}

	serviceDir, err := m.getServiceDir(cfg.ServiceDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(serviceDir, 0755); err != nil {
		return fmt.Errorf("create systemd user directory: %w", err)
	}
	if err := os.MkdirAll(cfg.LogDir, 0755); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}

	servicePath, err := m.getServicePath(cfg.Role, cfg.ServiceDir)
	if err != nil {
		return err
	}

	data, err := GenerateLinuxServiceUnit(cfg)
	if err != nil {
		return fmt.Errorf("generate service unit: %w", err)
	}

	if err := os.WriteFile(servicePath, data, 0644); err != nil {
		return fmt.Errorf("write service unit file: %w", err)
	}

	unit := m.unitName(cfg.Role)
	_, _ = m.execCommand(ctx, "systemctl", "--user", "daemon-reload")
	if out, err := m.execCommand(ctx, "systemctl", "--user", "enable", unit); err != nil {
		return fmt.Errorf("systemctl enable failed: %w (output: %s)", err, string(out))
	}
	if out, err := m.execCommand(ctx, "systemctl", "--user", "start", unit); err != nil {
		return fmt.Errorf("systemctl start failed: %w (output: %s)", err, string(out))
	}

	return nil
}

func (m *linuxManager) Uninstall(ctx context.Context, role string) error {
	if err := ValidateRole(role); err != nil {
		return err
	}
	servicePath, err := m.getServicePath(role, "")
	if err != nil {
		return err
	}

	if _, err := os.Stat(servicePath); os.IsNotExist(err) {
		return fmt.Errorf("%w: %s (%s)", ErrNotInstalled, role, servicePath)
	}

	unit := m.unitName(role)
	_, _ = m.execCommand(ctx, "systemctl", "--user", "stop", unit)
	_, _ = m.execCommand(ctx, "systemctl", "--user", "disable", unit)

	if err := os.Remove(servicePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove service unit file: %w", err)
	}

	_, _ = m.execCommand(ctx, "systemctl", "--user", "daemon-reload")
	return nil
}

func (m *linuxManager) Start(ctx context.Context, role string) error {
	if err := ValidateRole(role); err != nil {
		return err
	}
	unit := m.unitName(role)
	out, err := m.execCommand(ctx, "systemctl", "--user", "start", unit)
	if err != nil {
		return fmt.Errorf("systemctl start %s failed: %w (output: %s)", unit, err, string(out))
	}
	return nil
}

func (m *linuxManager) Stop(ctx context.Context, role string) error {
	if err := ValidateRole(role); err != nil {
		return err
	}
	unit := m.unitName(role)
	out, err := m.execCommand(ctx, "systemctl", "--user", "stop", unit)
	if err != nil {
		return fmt.Errorf("systemctl stop %s failed: %w (output: %s)", unit, err, string(out))
	}
	return nil
}

func (m *linuxManager) Status(ctx context.Context, role string) (Status, error) {
	if err := ValidateRole(role); err != nil {
		return Status{}, err
	}
	servicePath, err := m.getServicePath(role, "")
	if err != nil {
		return Status{}, err
	}

	home, _ := os.UserHomeDir()
	logFile := filepath.Join(home, ".agent-gateway", "logs", role+".log")

	stat := Status{
		Role:        role,
		Platform:    "linux (systemd --user)",
		ServiceFile: servicePath,
		LogFile:     logFile,
	}

	if _, err := os.Stat(servicePath); err == nil {
		stat.Installed = true
	} else {
		stat.Installed = false
		return stat, nil
	}

	unit := m.unitName(role)
	out, err := m.execCommand(ctx, "systemctl", "--user", "show", unit, "--property=MainPID,ActiveState,SubState")
	if err != nil {
		stat.Details = fmt.Sprintf("systemctl show failed: %s", string(out))
		return stat, nil
	}

	lines := strings.Split(string(out), "\n")
	var activeState, subState string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "MainPID=") {
			pidVal := strings.TrimPrefix(line, "MainPID=")
			if pid, err := strconv.Atoi(pidVal); err == nil && pid > 0 {
				stat.PID = pid
			}
		} else if strings.HasPrefix(line, "ActiveState=") {
			activeState = strings.TrimPrefix(line, "ActiveState=")
		} else if strings.HasPrefix(line, "SubState=") {
			subState = strings.TrimPrefix(line, "SubState=")
		}
	}

	if activeState == "active" {
		stat.Running = true
	}
	stat.Details = fmt.Sprintf("ActiveState=%s, SubState=%s", activeState, subState)
	return stat, nil
}

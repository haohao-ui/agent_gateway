//go:build darwin

package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var (
	darwinPIDRegex  = regexp.MustCompile(`"PID"\s*=\s*(\d+);`)
	darwinExitRegex = regexp.MustCompile(`"LastExitStatus"\s*=\s*(-?\d+);`)
)

type darwinManager struct {
	execCommand func(ctx context.Context, name string, args ...string) ([]byte, error)
	customDir   string
}

// NewManager creates a platform-native service manager for Darwin.
func NewManager() Manager {
	return &darwinManager{
		execCommand: defaultExecCommand,
	}
}

// NewDarwinManager creates a Darwin service manager with custom command runner and optional service directory.
func NewDarwinManager(dir string, cmdRunner func(ctx context.Context, name string, args ...string) ([]byte, error)) Manager {
	if cmdRunner == nil {
		cmdRunner = defaultExecCommand
	}
	return &darwinManager{
		execCommand: cmdRunner,
		customDir:   dir,
	}
}

func (m *darwinManager) getPlistDir(cfgDir string) (string, error) {
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
	return filepath.Join(home, "Library", "LaunchAgents"), nil
}

func (m *darwinManager) getPlistPath(role string, cfgDir string) (string, error) {
	dir, err := m.getPlistDir(cfgDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, fmt.Sprintf("com.agent-gateway.%s.plist", role)), nil
}

func (m *darwinManager) Install(ctx context.Context, cfg Config) error {
	if err := NormalizeConfig(&cfg); err != nil {
		return err
	}

	plistDir, err := m.getPlistDir(cfg.ServiceDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(plistDir, 0755); err != nil {
		return fmt.Errorf("create LaunchAgents directory: %w", err)
	}
	if err := os.MkdirAll(cfg.LogDir, 0755); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}

	plistPath, err := m.getPlistPath(cfg.Role, cfg.ServiceDir)
	if err != nil {
		return err
	}

	data, err := GenerateDarwinPlist(cfg)
	if err != nil {
		return fmt.Errorf("generate plist: %w", err)
	}

	// Write plist file
	if err := os.WriteFile(plistPath, data, 0644); err != nil {
		return fmt.Errorf("write plist file: %w", err)
	}

	// If using system LaunchAgents, register with launchctl
	// Ignore unload error if not loaded
	_, _ = m.execCommand(ctx, "launchctl", "unload", "-w", plistPath)
	if out, err := m.execCommand(ctx, "launchctl", "load", "-w", plistPath); err != nil {
		return fmt.Errorf("launchctl load failed: %w (output: %s)", err, string(out))
	}

	return nil
}

func (m *darwinManager) Uninstall(ctx context.Context, role string) error {
	if err := ValidateRole(role); err != nil {
		return err
	}
	plistPath, err := m.getPlistPath(role, "")
	if err != nil {
		return err
	}

	if _, err := os.Stat(plistPath); os.IsNotExist(err) {
		return fmt.Errorf("%w: %s (%s)", ErrNotInstalled, role, plistPath)
	}

	// Unload from launchctl
	_, _ = m.execCommand(ctx, "launchctl", "unload", "-w", plistPath)

	// Remove plist file
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove plist file: %w", err)
	}

	return nil
}

func (m *darwinManager) Start(ctx context.Context, role string) error {
	if err := ValidateRole(role); err != nil {
		return err
	}
	label := fmt.Sprintf("com.agent-gateway.%s", role)
	out, err := m.execCommand(ctx, "launchctl", "start", label)
	if err != nil {
		return fmt.Errorf("launchctl start %s failed: %w (output: %s)", label, err, string(out))
	}
	return nil
}

func (m *darwinManager) Stop(ctx context.Context, role string) error {
	if err := ValidateRole(role); err != nil {
		return err
	}
	label := fmt.Sprintf("com.agent-gateway.%s", role)
	out, err := m.execCommand(ctx, "launchctl", "stop", label)
	if err != nil {
		return fmt.Errorf("launchctl stop %s failed: %w (output: %s)", label, err, string(out))
	}
	return nil
}

func (m *darwinManager) Status(ctx context.Context, role string) (Status, error) {
	if err := ValidateRole(role); err != nil {
		return Status{}, err
	}
	plistPath, err := m.getPlistPath(role, "")
	if err != nil {
		return Status{}, err
	}

	home, _ := os.UserHomeDir()
	logFile := filepath.Join(home, ".agent-gateway", "logs", role+".log")

	stat := Status{
		Role:        role,
		Platform:    "darwin (launchd)",
		ServiceFile: plistPath,
		LogFile:     logFile,
	}

	if _, err := os.Stat(plistPath); err == nil {
		stat.Installed = true
	} else {
		stat.Installed = false
		return stat, nil
	}

	label := fmt.Sprintf("com.agent-gateway.%s", role)
	out, err := m.execCommand(ctx, "launchctl", "list", label)
	outStr := string(out)
	if err != nil {
		if strings.Contains(outStr, "Could not find service") {
			stat.Running = false
			stat.Details = "service registered in launchd but inactive"
			return stat, nil
		}
		stat.Details = fmt.Sprintf("launchctl list returned: %s", outStr)
		return stat, nil
	}

	// Parse PID
	if match := darwinPIDRegex.FindStringSubmatch(outStr); len(match) > 1 {
		if pid, err := strconv.Atoi(match[1]); err == nil && pid > 0 {
			stat.Running = true
			stat.PID = pid
		}
	}

	// Parse LastExitStatus
	if match := darwinExitRegex.FindStringSubmatch(outStr); len(match) > 1 {
		stat.Details = fmt.Sprintf("last exit status: %s", match[1])
	}

	return stat, nil
}

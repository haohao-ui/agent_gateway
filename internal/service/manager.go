package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrNotInstalled        = errors.New("service is not installed")
	ErrAlreadyInstalled    = errors.New("service is already installed")
	ErrPlatformUnsupported = errors.New("system service is not supported on this platform")
	ErrInvalidRole         = errors.New("invalid role: must be 'server' or 'node'")
)

// Role constants
const (
	RoleServer = "server"
	RoleNode   = "node"
)

// Config defines the configuration needed to install and register a service.
type Config struct {
	Role       string            `json:"role"`
	BinaryPath string            `json:"binary_path"`
	Args       []string          `json:"args"`
	WorkingDir string            `json:"working_dir"`
	LogDir     string            `json:"log_dir"`
	Env        map[string]string `json:"env,omitempty"`
	ServiceDir string            `json:"service_dir,omitempty"` // Override default directory for unit/plist (used for testing)
}

// Status contains operational status information of a managed service.
type Status struct {
	Role        string `json:"role"`
	Platform    string `json:"platform"`
	Installed   bool   `json:"installed"`
	Running     bool   `json:"running"`
	PID         int    `json:"pid,omitempty"`
	ServiceFile string `json:"service_file,omitempty"`
	LogFile     string `json:"log_file,omitempty"`
	Details     string `json:"details,omitempty"`
}

// Manager manages user-level services on the host system.
type Manager interface {
	Install(ctx context.Context, cfg Config) error
	Uninstall(ctx context.Context, role string) error
	Start(ctx context.Context, role string) error
	Stop(ctx context.Context, role string) error
	Status(ctx context.Context, role string) (Status, error)
}

// ValidateRole ensures role is either 'server' or 'node'.
func ValidateRole(role string) error {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case RoleServer, RoleNode:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrInvalidRole, role)
	}
}

// NormalizeConfig validates and sets defaults for Config.
func NormalizeConfig(cfg *Config) error {
	if err := ValidateRole(cfg.Role); err != nil {
		return err
	}
	cfg.Role = strings.ToLower(strings.TrimSpace(cfg.Role))

	if cfg.BinaryPath == "" {
		execPath, err := os.Executable()
		if err != nil {
			return fmt.Errorf("resolve current executable: %w", err)
		}
		cfg.BinaryPath, err = filepath.Abs(execPath)
		if err != nil {
			return fmt.Errorf("resolve executable absolute path: %w", err)
		}
	} else {
		abs, err := filepath.Abs(cfg.BinaryPath)
		if err != nil {
			return fmt.Errorf("resolve binary path: %w", err)
		}
		cfg.BinaryPath = abs
	}

	if cfg.WorkingDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		cfg.WorkingDir = home
	} else {
		abs, err := filepath.Abs(cfg.WorkingDir)
		if err == nil {
			cfg.WorkingDir = abs
		}
	}

	if cfg.LogDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		cfg.LogDir = filepath.Join(home, ".agent-gateway", "logs")
	} else {
		abs, err := filepath.Abs(cfg.LogDir)
		if err == nil {
			cfg.LogDir = abs
		}
	}

	return nil
}

//go:build windows

package service

import (
	"context"
	"fmt"
)

type windowsManager struct{}

// NewManager creates a platform-native service manager for Windows.
func NewManager() Manager {
	return &windowsManager{}
}

func (m *windowsManager) Install(ctx context.Context, cfg Config) error {
	return fmt.Errorf("%w: user-level background services require Windows Task Scheduler or administrative Windows Service (sc.exe)", ErrPlatformUnsupported)
}

func (m *windowsManager) Uninstall(ctx context.Context, role string) error {
	return fmt.Errorf("%w: user-level background services on Windows must be removed via Task Scheduler", ErrPlatformUnsupported)
}

func (m *windowsManager) Start(ctx context.Context, role string) error {
	return fmt.Errorf("%w: user-level background services on Windows must be managed via Task Scheduler", ErrPlatformUnsupported)
}

func (m *windowsManager) Stop(ctx context.Context, role string) error {
	return fmt.Errorf("%w: user-level background services on Windows must be managed via Task Scheduler", ErrPlatformUnsupported)
}

func (m *windowsManager) Status(ctx context.Context, role string) (Status, error) {
	return Status{
		Role:      role,
		Platform:  "windows",
		Installed: false,
		Running:   false,
		Details:   "Windows user-level services require Task Scheduler configuration",
	}, nil
}

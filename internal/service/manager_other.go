//go:build !darwin && !linux && !windows

package service

import (
	"context"
	"fmt"
	"runtime"
)

type otherManager struct{}

// NewManager creates a platform-native service manager for unsupported platforms.
func NewManager() Manager {
	return &otherManager{}
}

func (m *otherManager) Install(ctx context.Context, cfg Config) error {
	return fmt.Errorf("%w: %s", ErrPlatformUnsupported, runtime.GOOS)
}

func (m *otherManager) Uninstall(ctx context.Context, role string) error {
	return fmt.Errorf("%w: %s", ErrPlatformUnsupported, runtime.GOOS)
}

func (m *otherManager) Start(ctx context.Context, role string) error {
	return fmt.Errorf("%w: %s", ErrPlatformUnsupported, runtime.GOOS)
}

func (m *otherManager) Stop(ctx context.Context, role string) error {
	return fmt.Errorf("%w: %s", ErrPlatformUnsupported, runtime.GOOS)
}

func (m *otherManager) Status(ctx context.Context, role string) (Status, error) {
	return Status{
		Role:      role,
		Platform:  runtime.GOOS,
		Installed: false,
		Running:   false,
		Details:   fmt.Sprintf("platform %s is not supported for background services", runtime.GOOS),
	}, nil
}

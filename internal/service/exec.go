package service

import (
	"bytes"
	"context"
	"os/exec"
)

func defaultExecCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return bytes.TrimSpace(out), err
}

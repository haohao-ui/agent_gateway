package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"agent-gateway/internal/devicestore"
	"agent-gateway/internal/taskstore"
)

func TestRunMCP_LocalModeStartup(t *testing.T) {
	dataDir := t.TempDir()

	// Initialize stores
	tasks, err := taskstore.Open(filepath.Join(dataDir, "tasks.sqlite"))
	if err != nil {
		t.Fatalf("open tasks: %v", err)
	}
	tasks.Close()

	devs, err := devicestore.Open(filepath.Join(dataDir, "devices.sqlite"))
	if err != nil {
		t.Fatalf("open devs: %v", err)
	}
	devs.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// In local mode with empty stdin, StdioTransport will exit when context cancels or stdin closes
	_ = runMCP(ctx, []string{"--data-dir", dataDir})
}

func TestRunMCP_RemoteMissingToken(t *testing.T) {
	err := runMCP(context.Background(), []string{"--server", "https://127.0.0.1:8443"})
	if err == nil {
		t.Fatal("expected error when --token is missing in remote mode")
	}
}

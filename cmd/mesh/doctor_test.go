package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"agent-gateway/internal/identity"
	"agent-gateway/internal/taskstore"
)

func TestRunDoctor_ServerMode(t *testing.T) {
	dataDir := t.TempDir()

	// 1. Should fail on empty directory
	err := runDoctor(context.Background(), []string{"--server-mode", "--data-dir", dataDir})
	if err == nil {
		t.Fatal("expected doctor to fail on uninitialized server data dir")
	}

	// 2. Initialize CA and task store
	ca, err := identity.LoadOrGenerateCA(dataDir)
	if err != nil {
		t.Fatalf("generate CA: %v", err)
	}
	if ca == nil {
		t.Fatal("nil CA")
	}

	store, err := taskstore.Open(filepath.Join(dataDir, "tasks.sqlite"))
	if err != nil {
		t.Fatalf("taskstore open: %v", err)
	}
	store.Close()

	// 3. Should pass (with warnings about other DBs not existing yet)
	err = runDoctor(context.Background(), []string{"--server-mode", "--data-dir", dataDir, "--json"})
	if err != nil {
		t.Fatalf("expected doctor to pass with CA and tasks DB, got: %v", err)
	}
}

func TestRunDoctor_NodeMode(t *testing.T) {
	nodeDir := t.TempDir()

	// 1. Should fail on un-paired directory
	err := runDoctor(context.Background(), []string{"--node-dir", nodeDir})
	if err == nil {
		t.Fatal("expected doctor to fail on un-paired node directory")
	}

	// 2. Run with short timeout context
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	err = runDoctor(ctx, []string{"--node-dir", nodeDir, "--json"})
	if err == nil {
		t.Fatal("expected error")
	}
}

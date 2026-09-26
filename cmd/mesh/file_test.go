package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRunFile_LocalWorkflow(t *testing.T) {
	dataDir := t.TempDir()
	taskID := "task-file-cli-test"

	// Create test file to upload
	srcFile := filepath.Join(dataDir, "input.txt")
	testContent := []byte("CLI file upload test payload")
	if err := os.WriteFile(srcFile, testContent, 0o600); err != nil {
		t.Fatalf("write src file: %v", err)
	}

	ctx := context.Background()

	// 1. Upload
	err := runFile(ctx, []string{"upload", "--data-dir", dataDir, taskID, srcFile})
	if err != nil {
		t.Fatalf("runFile upload: %v", err)
	}

	// 2. List
	err = runFile(ctx, []string{"list", "--data-dir", dataDir, taskID, "--json"})
	if err != nil {
		t.Fatalf("runFile list: %v", err)
	}

	// 3. Download
	destFile := filepath.Join(dataDir, "downloaded.txt")
	err = runFile(ctx, []string{"download", "--data-dir", dataDir, "--output", destFile, taskID, "input.txt"})
	if err != nil {
		t.Fatalf("runFile download: %v", err)
	}

	downloaded, err := os.ReadFile(destFile)
	if err != nil {
		t.Fatalf("read downloaded: %v", err)
	}
	if string(downloaded) != string(testContent) {
		t.Errorf("content mismatch: got %q, want %q", downloaded, testContent)
	}

	// 4. Delete
	err = runFile(ctx, []string{"delete", "--data-dir", dataDir, taskID, "input.txt"})
	if err != nil {
		t.Fatalf("runFile delete: %v", err)
	}

	// Download after delete should fail
	err = runFile(ctx, []string{"download", "--data-dir", dataDir, "--output", destFile, taskID, "input.txt"})
	if err == nil {
		t.Fatal("expected download to fail after deletion")
	}
}

func TestRunFile_MissingArgs(t *testing.T) {
	ctx := context.Background()
	if err := runFile(ctx, []string{}); err == nil {
		t.Fatal("expected error for empty args")
	}
	if err := runFile(ctx, []string{"unknown_action"}); err == nil {
		t.Fatal("expected error for unknown action")
	}
	if err := runFile(ctx, []string{"upload"}); err == nil {
		t.Fatal("expected error for missing upload args")
	}
}

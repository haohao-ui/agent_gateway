package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNodeLifecycleMissingConfig(t *testing.T) {
	tempDir := t.TempDir()
	err := runNodeStart([]string{"--dir", tempDir})
	if err == nil || !strings.Contains(err.Error(), "未找到节点配置文件") {
		t.Fatalf("expected missing config error with user guidance, got: %v", err)
	}
}

func TestNodeLifecycleStatusStopped(t *testing.T) {
	tempDir := t.TempDir()
	err := runNodeStatus([]string{"--dir", tempDir})
	if err != nil {
		t.Fatalf("expected status on stopped node to succeed, got: %v", err)
	}
}

func TestNodeLifecycleStopWhenNotRunning(t *testing.T) {
	tempDir := t.TempDir()
	err := runNodeStop([]string{"--dir", tempDir})
	if err != nil {
		t.Fatalf("expected stop on non-running node to succeed cleanly, got: %v", err)
	}
}

func TestNodeLifecycleCleanPIDFile(t *testing.T) {
	tempDir := t.TempDir()
	pidFile := filepath.Join(tempDir, "node.pid")
	// Write a dead PID
	_ = os.WriteFile(pidFile, []byte("99999999"), 0o644)
	err := runNodeStop([]string{"--dir", tempDir})
	if err != nil {
		t.Fatalf("expected runNodeStop to succeed, got: %v", err)
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("expected dead pid file to be cleaned up, but it still exists")
	}
}

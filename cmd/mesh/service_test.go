package main

import (
	"context"
	"strings"
	"testing"
)

func TestRunServiceHelp(t *testing.T) {
	// help should return nil
	if err := runService(context.Background(), []string{"help"}); err != nil {
		t.Errorf("runService help returned error: %v", err)
	}
	if err := runService(context.Background(), []string{"-h"}); err != nil {
		t.Errorf("runService -h returned error: %v", err)
	}
	if err := runService(context.Background(), []string{"--help"}); err != nil {
		t.Errorf("runService --help returned error: %v", err)
	}
}

func TestRunServiceMissingSubcommand(t *testing.T) {
	err := runService(context.Background(), []string{})
	if err == nil || !strings.Contains(err.Error(), "subcommand required") {
		t.Errorf("expected subcommand required error, got %v", err)
	}
}

func TestRunServiceUnknownAction(t *testing.T) {
	err := runService(context.Background(), []string{"unknown"})
	if err == nil || !strings.Contains(err.Error(), "unknown service action") {
		t.Errorf("expected unknown service action error, got %v", err)
	}
}

func TestRunServiceStatusInvalidRole(t *testing.T) {
	err := runService(context.Background(), []string{"status", "--role", "invalid_role"})
	if err == nil || !strings.Contains(err.Error(), "invalid role") {
		t.Errorf("expected invalid role error, got %v", err)
	}
}

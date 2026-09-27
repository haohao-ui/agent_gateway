package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseDotEnv(t *testing.T) {
	input := `
# This is a comment
MESH_SERVER_ADDR=127.0.0.1:18443
export MESH_HTTP_ADDR="127.0.0.1:18080"
MESH_DATA_DIR='./custom-data'
MESH_HOSTS=127.0.0.1,localhost
EMPTY_VAL=
# Another comment
SPACED_KEY = value with spaces  
QUOTED_NEWLINE="line1\nline2"
`
	vars, err := parseDotEnv(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parseDotEnv failed: %v", err)
	}

	if vars["MESH_SERVER_ADDR"] != "127.0.0.1:18443" {
		t.Errorf("expected 127.0.0.1:18443, got %q", vars["MESH_SERVER_ADDR"])
	}
	if vars["MESH_HTTP_ADDR"] != "127.0.0.1:18080" {
		t.Errorf("expected 127.0.0.1:18080, got %q", vars["MESH_HTTP_ADDR"])
	}
	if vars["MESH_DATA_DIR"] != "./custom-data" {
		t.Errorf("expected ./custom-data, got %q", vars["MESH_DATA_DIR"])
	}
	if vars["MESH_HOSTS"] != "127.0.0.1,localhost" {
		t.Errorf("expected 127.0.0.1,localhost, got %q", vars["MESH_HOSTS"])
	}
	if vars["EMPTY_VAL"] != "" {
		t.Errorf("expected empty string, got %q", vars["EMPTY_VAL"])
	}
	if vars["SPACED_KEY"] != "value with spaces" {
		t.Errorf("expected 'value with spaces', got %q", vars["SPACED_KEY"])
	}
	if vars["QUOTED_NEWLINE"] != "line1\nline2" {
		t.Errorf("expected newline, got %q", vars["QUOTED_NEWLINE"])
	}
}

func TestLoadDotEnv(t *testing.T) {
	tmpDir := t.TempDir()
	envFile := filepath.Join(tmpDir, ".env")
	content := "TEST_ENV_GATEWAY_PORT=9999\nTEST_ENV_OVERWRITE=from_file\n"
	if err := os.WriteFile(envFile, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	// Pre-set TEST_ENV_OVERWRITE to ensure it is not overwritten
	t.Setenv("TEST_ENV_OVERWRITE", "from_process")
	t.Setenv("MESH_ENV_FILE", envFile)

	loadDotEnv()

	if os.Getenv("TEST_ENV_GATEWAY_PORT") != "9999" {
		t.Errorf("expected 9999, got %q", os.Getenv("TEST_ENV_GATEWAY_PORT"))
	}
	if os.Getenv("TEST_ENV_OVERWRITE") != "from_process" {
		t.Errorf("expected from_process, got %q", os.Getenv("TEST_ENV_OVERWRITE"))
	}
}

func TestEnvHelpers(t *testing.T) {
	t.Setenv("KEY_A", "val_a")
	t.Setenv("INT_KEY", "42")
	t.Setenv("DUR_KEY", "10m")

	if got := envOrDefault([]string{"NON_EXIST", "KEY_A"}, "def"); got != "val_a" {
		t.Errorf("expected val_a, got %q", got)
	}
	if got := envOrDefault([]string{"NON_EXIST1", "NON_EXIST2"}, "def"); got != "def" {
		t.Errorf("expected def, got %q", got)
	}
	if got := envInt("INT_KEY", 1); got != 42 {
		t.Errorf("expected 42, got %d", got)
	}
	if got := envInt("NON_EXIST", 1); got != 1 {
		t.Errorf("expected 1, got %d", got)
	}
	if got := envDuration("DUR_KEY", time.Second); got != 10*time.Minute {
		t.Errorf("expected 10m, got %v", got)
	}
}

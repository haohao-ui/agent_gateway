package service

import (
	"encoding/xml"
	"errors"
	"strings"
	"testing"
)

func TestValidateRole(t *testing.T) {
	cases := []struct {
		role    string
		wantErr bool
	}{
		{"server", false},
		{"node", false},
		{"Server", false},
		{" NODE ", false},
		{"other", true},
		{"", true},
	}

	for _, c := range cases {
		err := ValidateRole(c.role)
		if (err != nil) != c.wantErr {
			t.Errorf("ValidateRole(%q) err = %v, wantErr = %v", c.role, err, c.wantErr)
		}
	}
}

func TestNormalizeConfig(t *testing.T) {
	cfg := Config{
		Role: "server",
	}
	if err := NormalizeConfig(&cfg); err != nil {
		t.Fatalf("NormalizeConfig failed: %v", err)
	}

	if cfg.BinaryPath == "" {
		t.Errorf("expected BinaryPath to be populated, got empty")
	}
	if cfg.WorkingDir == "" {
		t.Errorf("expected WorkingDir to be populated, got empty")
	}
	if cfg.LogDir == "" {
		t.Errorf("expected LogDir to be populated, got empty")
	}

	// Test invalid role
	badCfg := Config{Role: "invalid"}
	if err := NormalizeConfig(&badCfg); !errors.Is(err, ErrInvalidRole) {
		t.Errorf("NormalizeConfig with invalid role want ErrInvalidRole, got: %v", err)
	}
}

func TestGenerateDarwinPlist(t *testing.T) {
	cfg := Config{
		Role:       "server",
		BinaryPath: "/usr/local/bin/mesh",
		Args:       []string{"server", "--addr", "0.0.0.0:8443", "--data-dir", "/var/lib/gateway"},
		WorkingDir: "/var/lib/gateway",
		LogDir:     "/var/log/gateway",
		Env: map[string]string{
			"MESH_ENV": "test",
		},
	}

	data, err := GenerateDarwinPlist(cfg)
	if err != nil {
		t.Fatalf("GenerateDarwinPlist failed: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "<string>com.agent-gateway.server</string>") {
		t.Errorf("plist missing label: %s", content)
	}
	if !strings.Contains(content, "<string>/usr/local/bin/mesh</string>") {
		t.Errorf("plist missing binary path: %s", content)
	}
	if !strings.Contains(content, "<string>--addr</string>") {
		t.Errorf("plist missing args: %s", content)
	}
	if !strings.Contains(content, "<string>/var/log/gateway/server.log</string>") {
		t.Errorf("plist missing log path: %s", content)
	}
	if !strings.Contains(content, "<key>MESH_ENV</key>") {
		t.Errorf("plist missing environment variable: %s", content)
	}

	// Verify XML structure is well-formed
	var parsed struct {
		XMLName xml.Name `xml:"plist"`
	}
	if err := xml.Unmarshal(data, &parsed); err != nil {
		t.Errorf("generated plist is not well-formed XML: %v", err)
	}
}

func TestGenerateLinuxServiceUnit(t *testing.T) {
	cfg := Config{
		Role:       "node",
		BinaryPath: "/usr/local/bin/mesh",
		Args:       []string{"node", "--config", "/etc/mesh/node.json"},
		WorkingDir: "/etc/mesh",
		LogDir:     "/var/log/gateway",
		Env: map[string]string{
			"NODE_ENV": "production",
		},
	}

	data, err := GenerateLinuxServiceUnit(cfg)
	if err != nil {
		t.Fatalf("GenerateLinuxServiceUnit failed: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "Description=Agent Gateway (node)") {
		t.Errorf("service unit missing description: %s", content)
	}
	if !strings.Contains(content, "ExecStart=/usr/local/bin/mesh node --config /etc/mesh/node.json") {
		t.Errorf("service unit missing ExecStart: %s", content)
	}
	if !strings.Contains(content, "StandardOutput=append:/var/log/gateway/node.log") {
		t.Errorf("service unit missing StandardOutput: %s", content)
	}
	if !strings.Contains(content, "Restart=always") {
		t.Errorf("service unit missing Restart directive: %s", content)
	}
	if !strings.Contains(content, "Environment=\"NODE_ENV=production\"") {
		t.Errorf("service unit missing Environment directive: %s", content)
	}
}

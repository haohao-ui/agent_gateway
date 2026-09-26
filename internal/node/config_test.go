package node

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-gateway/internal/protocol"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "node.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

const minimalConfig = `{
  "server_url": "https://127.0.0.1:8443",
  "cert_file": "node.crt",
  "key_file": "node.key",
  "ca_file": "ca.crt",
  "capabilities": [{"name": "agent.run", "version": 1, "adapter": {"executable": "/bin/echo", "args": ["{instruction}"]}}]
}`

func TestLoadConfigAppliesDefaults(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, minimalConfig))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.LeaseSeconds != DefaultLeaseSeconds || cfg.RenewSeconds != DefaultRenewSeconds {
		t.Fatalf("unexpected lease defaults: %d/%d", cfg.LeaseSeconds, cfg.RenewSeconds)
	}
	if cfg.MaxOutputBytes != DefaultMaxOutputBytes || cfg.GraceSeconds != DefaultGraceSeconds {
		t.Fatalf("unexpected limit defaults: %d/%d", cfg.MaxOutputBytes, cfg.GraceSeconds)
	}
	if cfg.Capabilities[0].InstructionKey != defaultInstructionKey {
		t.Fatalf("instruction key defaulted to %q", cfg.Capabilities[0].InstructionKey)
	}
}

func TestLoadConfigRejectsUnknownFields(t *testing.T) {
	// A typo in a config file must fail loudly rather than silently disabling a
	// limit the operator believed they had set.
	body := strings.Replace(minimalConfig, `"ca_file": "ca.crt",`, `"ca_file": "ca.crt", "renew_second": 5,`, 1)
	if _, err := LoadConfig(writeConfig(t, body)); err == nil {
		t.Fatal("a misspelled field was accepted")
	}
}

func TestLoadConfigRejectsLeaseShorterThanRenewal(t *testing.T) {
	body := strings.Replace(minimalConfig, `"server_url"`, `"lease_seconds": 5, "renew_seconds": 5, "server_url"`, 1)
	_, err := LoadConfig(writeConfig(t, body))
	if !errors.Is(err, protocol.ErrInvalid) {
		t.Fatalf("got %v, want an invalid configuration error", err)
	}
}

func TestCapabilityForRejectsUnknownWork(t *testing.T) {
	cfg := Config{Capabilities: []Capability{{Name: "agent.run", Version: 2}}}

	if _, err := cfg.capabilityFor(protocol.Task{Capability: "agent.run", CapabilityVersion: 1}); err == nil {
		t.Fatal("a version this node does not serve was accepted")
	}
	if _, err := cfg.capabilityFor(protocol.Task{Capability: "file.get", CapabilityVersion: 1}); err == nil {
		t.Fatal("a capability this node does not serve was accepted")
	}
	if _, err := cfg.capabilityFor(protocol.Task{Capability: "agent.run", CapabilityVersion: 2}); err != nil {
		t.Fatalf("the configured capability was refused: %v", err)
	}
}

func TestInstructionExtraction(t *testing.T) {
	cap := &Capability{Name: "agent.run", Version: 1, InstructionKey: "prompt"}

	cases := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "plain", input: `{"prompt":"hello"}`, want: "hello"},
		{name: "unicode", input: `{"prompt":"中文 提示"}`, want: "中文 提示"},
		{name: "empty", input: `{"prompt":""}`, want: ""},
		{name: "missing field", input: `{"other":"x"}`, wantErr: true},
		{name: "wrong type", input: `{"prompt":42}`, wantErr: true},
		{name: "not an object", input: `["prompt"]`, wantErr: true},
		{name: "invalid json", input: `{"prompt":`, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := instruction(cap, []byte(tc.input))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("input %s was accepted as %q", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("instruction: %v", err)
			}
			if got != tc.want {
				t.Fatalf("instruction is %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRunnerConfigCarriesAdapterFields(t *testing.T) {
	cfg := Config{MaxOutputBytes: 4096, GraceSeconds: 3}
	cap := &Capability{
		Name:    "agent.run",
		Version: 1,
		Adapter: Adapter{
			Executable: "/bin/echo",
			Args:       []string{"{instruction}", "--flag"},
			WorkDir:    "/tmp",
			Env:        map[string]string{"ONLY": "this"},
			Stdin:      true,
		},
	}

	got := cfg.runnerConfig(cap, "/data/node/work")
	if got.Executable != "/bin/echo" || got.WorkDir != "/tmp" || !got.Stdin {
		t.Fatalf("unexpected runner config: %+v", got)
	}
	if len(got.Args) != 2 || got.Args[0] != "{instruction}" {
		t.Fatalf("args were not carried through: %+v", got.Args)
	}
	if got.Env["ONLY"] != "this" || len(got.Env) != 1 {
		t.Fatalf("environment whitelist was not carried through: %+v", got.Env)
	}
	if got.MaxOutputBytes != 4096 || got.GracePeriod.Seconds() != 3 {
		t.Fatalf("limits were not carried through: %+v", got)
	}
}

func TestRunnerConfigFallsBackToTheNodeWorkDirectory(t *testing.T) {
	cfg := Config{}
	cap := &Capability{Name: "agent.run", Version: 1, Adapter: Adapter{Executable: "/bin/echo", Args: []string{"{instruction}"}}}

	got := cfg.runnerConfig(cap, "/data/node/work")
	if got.WorkDir != "/data/node/work" {
		t.Fatalf("work dir is %q, want the node work directory", got.WorkDir)
	}
}

func TestResolvePaths(t *testing.T) {
	cfg := Config{CertFile: "node.crt", KeyFile: "node.key", CAFile: "/absolute/ca.crt"}
	cfg.resolvePaths("/data/node")

	if cfg.CertFile != "/data/node/node.crt" || cfg.KeyFile != "/data/node/node.key" {
		t.Fatalf("relative paths were not resolved: %+v", cfg)
	}
	if cfg.CAFile != "/absolute/ca.crt" {
		t.Fatalf("an absolute path was rewritten: %q", cfg.CAFile)
	}
}

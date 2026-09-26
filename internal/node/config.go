// Package node runs the outbound half of the gateway protocol: it pairs with a
// gateway, long-polls for work, executes one task at a time through
// internal/runner, and reports the result from a local outbox.
//
// The loop is deliberately conservative about uncertainty. A lease that lapses
// while the work is running is not reported and not restarted: the gateway
// parks the task in unknown and a human decides, because the node cannot prove
// whether a partial side effect happened (docs/ARCHITECTURE.md ADR-009).
//
// Not implemented here, and therefore not to be reported as working:
//
//   - Reconciliation of an interrupted attempt. A node that crashed while
//     starting or running writes a journal entry saying so and refuses to
//     re-execute, but the operator-side audit and recovery paths that
//     docs/M2-DESIGN.md requires do not exist yet.
//   - Task concurrency. One lease is processed at a time.
//   - Capability negotiation and workspace policy beyond a name/version check
//     against the locally configured capability list.
//   - Artifact transfer. Results carry bounded text only.
package node

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"agent-gateway/internal/protocol"
	"agent-gateway/internal/runner"
)

// Defaults applied when a configuration file leaves a field unset.
const (
	DefaultLeaseSeconds   = 60
	DefaultRenewSeconds   = 15
	DefaultMaxOutputBytes = 64 << 10
	DefaultGraceSeconds   = 2
	defaultInstructionKey = "prompt"
)

// Config is the node's on-disk configuration (node.json).
type Config struct {
	// ServerURL is the gateway base URL, for example https://gateway.local:8443.
	ServerURL string `json:"server_url"`
	// CertFile, KeyFile and CAFile are resolved against the node directory when
	// they are not absolute.
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
	CAFile   string `json:"ca_file"`

	// LeaseSeconds is the lease the node asks for; RenewSeconds is how often it
	// extends the lease while the work runs.
	LeaseSeconds   int `json:"lease_seconds"`
	RenewSeconds   int `json:"renew_seconds"`
	MaxOutputBytes int `json:"max_output_bytes"`
	GraceSeconds   int `json:"grace_seconds"`

	// Capabilities is the closed list of work this node accepts. A task naming
	// anything else is refused locally rather than executed.
	Capabilities []Capability `json:"capabilities"`
}

// Capability binds one advertised capability to the local command that serves it.
type Capability struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
	// InstructionKey names the JSON field of the task input that becomes the
	// runner's instruction. Defaults to "prompt".
	InstructionKey string  `json:"instruction_key"`
	Adapter        Adapter `json:"adapter"`
}

// Adapter is the local command configuration, converted to a runner.Config when
// a task arrives. It is operator configuration, never remote input: a task
// cannot add argv, change the executable or widen the environment.
type Adapter struct {
	Executable string   `json:"executable"`
	Args       []string `json:"args,omitempty"`
	Stdin      bool     `json:"stdin,omitempty"`
	// WorkDir is where the command starts. Empty means the node's own work
	// directory, which keeps executions out of the directory that holds the
	// node's private key. It is a startup directory, not a sandbox.
	WorkDir string            `json:"work_dir,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// LoadConfig reads and validates a node configuration file.
func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read node config: %w", err)
	}

	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse node config %s: %w", path, err)
	}
	if err := cfg.normalize(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// normalize fills in defaults and rejects a configuration the node cannot run.
func (c *Config) normalize() error {
	if c.ServerURL == "" {
		return fmt.Errorf("%w: server_url is required", protocol.ErrInvalid)
	}
	if c.CertFile == "" || c.KeyFile == "" || c.CAFile == "" {
		return fmt.Errorf("%w: cert_file, key_file and ca_file are required", protocol.ErrInvalid)
	}
	if len(c.Capabilities) == 0 {
		return fmt.Errorf("%w: at least one capability is required", protocol.ErrInvalid)
	}
	if c.LeaseSeconds == 0 {
		c.LeaseSeconds = DefaultLeaseSeconds
	}
	if c.RenewSeconds == 0 {
		c.RenewSeconds = DefaultRenewSeconds
	}
	if c.MaxOutputBytes == 0 {
		c.MaxOutputBytes = DefaultMaxOutputBytes
	}
	if c.GraceSeconds == 0 {
		c.GraceSeconds = DefaultGraceSeconds
	}
	if c.LeaseSeconds < 1 || c.RenewSeconds < 1 {
		return fmt.Errorf("%w: lease_seconds and renew_seconds must be positive", protocol.ErrInvalid)
	}
	// A lease that expires before the first renewal would hand the task to
	// unknown while the node is still working on it.
	if c.RenewSeconds >= c.LeaseSeconds {
		return fmt.Errorf("%w: renew_seconds (%d) must be shorter than lease_seconds (%d)",
			protocol.ErrInvalid, c.RenewSeconds, c.LeaseSeconds)
	}

	seen := make(map[string]bool, len(c.Capabilities))
	for i := range c.Capabilities {
		cap := &c.Capabilities[i]
		if cap.Name == "" || cap.Version < 1 {
			return fmt.Errorf("%w: capability %d needs a name and a version >= 1", protocol.ErrInvalid, i)
		}
		if cap.InstructionKey == "" {
			cap.InstructionKey = defaultInstructionKey
		}
		if cap.Adapter.Executable == "" {
			return fmt.Errorf("%w: capability %s has no adapter executable", protocol.ErrInvalid, cap.Name)
		}
		if seen[cap.Name] {
			return fmt.Errorf("%w: capability %s is listed twice", protocol.ErrInvalid, cap.Name)
		}
		seen[cap.Name] = true
	}
	return nil
}

// capabilityFor finds the locally configured capability a task addresses.
func (c *Config) capabilityFor(task protocol.Task) (*Capability, error) {
	for i := range c.Capabilities {
		if c.Capabilities[i].Name != task.Capability {
			continue
		}
		if c.Capabilities[i].Version != task.CapabilityVersion {
			return nil, fmt.Errorf("%w: capability %s version %d is not served by this node",
				protocol.ErrInvalid, task.Capability, task.CapabilityVersion)
		}
		return &c.Capabilities[i], nil
	}
	return nil, fmt.Errorf("%w: capability %s is not served by this node", protocol.ErrInvalid, task.Capability)
}

// renewInterval returns how often the lease is extended during execution.
func (c *Config) renewInterval() time.Duration {
	return time.Duration(c.RenewSeconds) * time.Second
}

// runnerConfig converts the adapter to the runner's configuration.
// defaultWorkDir is used when the adapter does not name one.
func (c *Config) runnerConfig(cap *Capability, defaultWorkDir string) runner.Config {
	workDir := cap.Adapter.WorkDir
	if workDir == "" {
		workDir = defaultWorkDir
	}
	return runner.Config{
		Executable:     cap.Adapter.Executable,
		Args:           append([]string(nil), cap.Adapter.Args...),
		Stdin:          cap.Adapter.Stdin,
		WorkDir:        workDir,
		Env:            cap.Adapter.Env,
		MaxOutputBytes: c.MaxOutputBytes,
		GracePeriod:    time.Duration(c.GraceSeconds) * time.Second,
	}
}

// instruction extracts the instruction a capability runs from the task input.
// The value is passed to the runner as data: it becomes one argv element or the
// child's standard input, never a shell fragment.
func instruction(cap *Capability, input []byte) (string, error) {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(input, &fields); err != nil {
		return "", fmt.Errorf("%w: task input is not a JSON object: %v", protocol.ErrInvalid, err)
	}
	raw, ok := fields[cap.InstructionKey]
	if !ok {
		return "", fmt.Errorf("%w: task input has no %q field", protocol.ErrInvalid, cap.InstructionKey)
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return "", fmt.Errorf("%w: task input %q must be a string", protocol.ErrInvalid, cap.InstructionKey)
	}
	return text, nil
}

// resolvePaths makes the credential paths absolute against the node directory.
func (c *Config) resolvePaths(dir string) {
	for _, field := range []*string{&c.CertFile, &c.KeyFile, &c.CAFile} {
		if !filepath.IsAbs(*field) {
			*field = filepath.Join(dir, *field)
		}
	}
}

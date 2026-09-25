package runner

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-gateway/internal/protocol"
)

// validConfig returns a configuration that passes every check, so each test can
// break exactly one field.
func validConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		Executable: testBinary(t),
		Args:       []string{InstructionPlaceholder},
		WorkDir:    t.TempDir(),
		Env:        map[string]string{"K": "V"},
	}
}

func TestConfigRejectsInvalidExecutable(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "file")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	for _, tc := range []struct {
		name string
		exe  string
	}{
		{"empty", ""},
		{"relative", "bin/agent"},
		{"missing", filepath.Join(dir, "nope")},
		{"directory", dir},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig(t)
			cfg.Executable = tc.exe
			if _, err := cfg.validate("instr"); !errors.Is(err, protocol.ErrInvalid) {
				t.Fatalf("error = %v, want one wrapping protocol.ErrInvalid", err)
			}
		})
	}
}

func TestConfigRejectsInvalidWorkDir(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	for _, tc := range []struct {
		name string
		wd   string
	}{
		{"empty", ""},
		{"relative", "sub/dir"},
		{"missing", filepath.Join(dir, "nope")},
		{"not a directory", file},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig(t)
			cfg.WorkDir = tc.wd
			if _, err := cfg.validate("instr"); !errors.Is(err, protocol.ErrInvalid) {
				t.Fatalf("error = %v, want one wrapping protocol.ErrInvalid", err)
			}
		})
	}
}

func TestConfigOutputLimitBounds(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.MaxOutputBytes = 0
		if _, err := cfg.validate("instr"); err != nil {
			t.Fatalf("validate: %v", err)
		}
		if cfg.MaxOutputBytes != DefaultMaxOutputBytes {
			t.Fatalf("limit = %d, want the default %d", cfg.MaxOutputBytes, DefaultMaxOutputBytes)
		}
	})

	t.Run("maximum accepted", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.MaxOutputBytes = MaxAllowedOutputBytes
		if _, err := cfg.validate("instr"); err != nil {
			t.Fatalf("validate: %v", err)
		}
	})

	for _, tc := range []struct {
		name  string
		limit int
	}{
		{"negative", -1},
		{"above maximum", MaxAllowedOutputBytes + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig(t)
			cfg.MaxOutputBytes = tc.limit
			if _, err := cfg.validate("instr"); !errors.Is(err, protocol.ErrInvalid) {
				t.Fatalf("error = %v, want one wrapping protocol.ErrInvalid", err)
			}
		})
	}
}

func TestConfigGracePeriodDefaultsWhenNotPositive(t *testing.T) {
	for _, tc := range []struct {
		name  string
		grace time.Duration
	}{
		{"zero", 0},
		{"negative", -time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig(t)
			cfg.GracePeriod = tc.grace
			if _, err := cfg.validate("instr"); err != nil {
				t.Fatalf("validate: %v", err)
			}
			if cfg.GracePeriod != DefaultGracePeriod {
				t.Fatalf("grace = %s, want the default %s", cfg.GracePeriod, DefaultGracePeriod)
			}
		})
	}
}

// TestConfigPlaceholderRules pins the argv template contract.
func TestConfigPlaceholderRules(t *testing.T) {
	t.Run("substitutes exactly one standalone placeholder", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.Args = []string{"--prompt", InstructionPlaceholder, "--end"}
		args, err := cfg.validate("hello world")
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		want := []string{"--prompt", "hello world", "--end"}
		if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("args = %q, want %q", args, want)
		}
	})

	t.Run("does not mutate the caller's slice", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.Args = []string{InstructionPlaceholder}
		if _, err := cfg.validate("hello"); err != nil {
			t.Fatalf("validate: %v", err)
		}
		if cfg.Args[0] != InstructionPlaceholder {
			t.Fatalf("caller slice was rewritten: %q", cfg.Args)
		}
	})

	for _, tc := range []struct {
		name  string
		args  []string
		stdin bool
	}{
		{"no placeholder", []string{"--prompt"}, false},
		{"two placeholders", []string{InstructionPlaceholder, InstructionPlaceholder}, false},
		{"embedded placeholder", []string{"--prompt=" + InstructionPlaceholder}, false},
		{"placeholder with stdin", []string{InstructionPlaceholder}, true},
		{"empty argv without stdin", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig(t)
			cfg.Args = tc.args
			cfg.Stdin = tc.stdin
			if _, err := cfg.validate("hello"); !errors.Is(err, protocol.ErrInvalid) {
				t.Fatalf("error = %v, want one wrapping protocol.ErrInvalid", err)
			}
		})
	}

	t.Run("stdin without placeholder is accepted", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.Args = []string{"--flag"}
		cfg.Stdin = true
		args, err := cfg.validate("hello")
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		if len(args) != 1 || args[0] != "--flag" {
			t.Fatalf("args = %q, want [--flag]", args)
		}
	})
}

func TestConfigRejectsInvalidEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
	}{
		{"empty name", map[string]string{"": "v"}},
		{"name with equals", map[string]string{"A=B": "v"}},
		{"name with nul", map[string]string{"A\x00": "v"}},
		{"value with nul", map[string]string{"A": "v\x00"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig(t)
			cfg.Env = tc.env
			if _, err := cfg.validate("instr"); !errors.Is(err, protocol.ErrInvalid) {
				t.Fatalf("error = %v, want one wrapping protocol.ErrInvalid", err)
			}
		})
	}

	t.Run("nil environment is allowed", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.Env = nil
		if _, err := cfg.validate("instr"); err != nil {
			t.Fatalf("validate: %v", err)
		}
	})
}

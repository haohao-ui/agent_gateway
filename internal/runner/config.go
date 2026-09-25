package runner

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-gateway/internal/protocol"
)

const (
	// DefaultMaxOutputBytes is the default retained output size (64 KiB).
	DefaultMaxOutputBytes = 64 * 1024
	// MaxAllowedOutputBytes is the hard upper bound for retained output (64 KiB).
	MaxAllowedOutputBytes = 64 * 1024
	// DefaultGracePeriod is the default wait between graceful termination and forced kill.
	DefaultGracePeriod = 2 * time.Second
	// InstructionPlaceholder is the standalone argv placeholder replaced by the instruction.
	InstructionPlaceholder = "{instruction}"
)

// Config controls one controlled execution of an external CLI.
//
// Isolation limits (portable, deliberately not a sandbox): WorkDir only selects the
// startup directory, Env is a whitelist that never inherits the parent environment,
// and the instruction is passed as one argv element or on stdin, never through a shell.
// Nothing here restricts filesystem, network, credential or kernel access of the child,
// so callers must treat the executable and its arguments as trusted local configuration.
type Config struct {
	// Executable is an absolute path to a regular file.
	Executable string
	// Args carries exactly one standalone {instruction} element, or no placeholder at
	// all when Stdin is true. Elements are passed verbatim; no shell parsing happens.
	Args []string
	// Stdin sends the instruction on the child's standard input instead of argv.
	Stdin bool
	// WorkDir is the absolute, existing startup directory of the child.
	WorkDir string
	// Env is the only environment the child receives; os.Environ is never copied.
	Env map[string]string
	// MaxOutputBytes is the retained tail size (default 64 KiB, maximum 64 KiB).
	MaxOutputBytes int
	// GracePeriod is the delay between graceful and forced termination (default 2s).
	GracePeriod time.Duration
}

// validate checks the configuration and replaces it with its normalized copy.
// It returns a copy of the argv with the placeholder substituted; the caller's
// Args slice is never mutated.
func (c *Config) validate(instruction string) ([]string, error) {
	// Reject a nil receiver style mistake early: every field below is mandatory
	// except the optional limits, so an empty Config can never be valid.
	if c == nil {
		return nil, fmt.Errorf("%w: config must not be nil", protocol.ErrInvalid)
	}

	if c.Executable == "" {
		return nil, fmt.Errorf("%w: executable must not be empty", protocol.ErrInvalid)
	}
	if !filepath.IsAbs(c.Executable) {
		return nil, fmt.Errorf("%w: executable must be an absolute path: %q", protocol.ErrInvalid, c.Executable)
	}
	// Reject a non-regular executable before Start, which would otherwise fail with
	// a less specific error or, for a FIFO/device, block.
	fi, err := os.Stat(c.Executable)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: executable does not exist: %q", protocol.ErrInvalid, c.Executable)
		}
		return nil, fmt.Errorf("%w: executable is not usable: %v", protocol.ErrInvalid, err)
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("%w: executable must not be a directory: %q", protocol.ErrInvalid, c.Executable)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: executable must be a regular file, got mode %v: %q", protocol.ErrInvalid, fi.Mode(), c.Executable)
	}

	if c.WorkDir == "" {
		return nil, fmt.Errorf("%w: workdir must not be empty", protocol.ErrInvalid)
	}
	if !filepath.IsAbs(c.WorkDir) {
		return nil, fmt.Errorf("%w: workdir must be an absolute path: %q", protocol.ErrInvalid, c.WorkDir)
	}
	wfi, err := os.Stat(c.WorkDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: workdir does not exist: %q", protocol.ErrInvalid, c.WorkDir)
		}
		return nil, fmt.Errorf("%w: workdir is not usable: %v", protocol.ErrInvalid, err)
	}
	if !wfi.IsDir() {
		return nil, fmt.Errorf("%w: workdir must be a directory: %q", protocol.ErrInvalid, c.WorkDir)
	}

	switch {
	case c.MaxOutputBytes == 0:
		c.MaxOutputBytes = DefaultMaxOutputBytes
	case c.MaxOutputBytes < 0:
		return nil, fmt.Errorf("%w: max output bytes must not be negative, got %d", protocol.ErrInvalid, c.MaxOutputBytes)
	case c.MaxOutputBytes > MaxAllowedOutputBytes:
		return nil, fmt.Errorf("%w: max output bytes must not exceed %d, got %d", protocol.ErrInvalid, MaxAllowedOutputBytes, c.MaxOutputBytes)
	}

	if c.GracePeriod <= 0 {
		c.GracePeriod = DefaultGracePeriod
	}

	finalArgs, err := substituteInstruction(c.Args, c.Stdin, instruction)
	if err != nil {
		return nil, err
	}

	for k, v := range c.Env {
		if k == "" {
			return nil, fmt.Errorf("%w: environment variable name must not be empty", protocol.ErrInvalid)
		}
		if strings.ContainsRune(k, '=') || strings.ContainsRune(k, 0) {
			return nil, fmt.Errorf("%w: invalid environment variable name %q", protocol.ErrInvalid, k)
		}
		if strings.ContainsRune(v, 0) {
			return nil, fmt.Errorf("%w: invalid environment variable value for %q", protocol.ErrInvalid, k)
		}
	}

	return finalArgs, nil
}

// substituteInstruction returns the argv that the child receives.
//
// With Stdin=false the argv must contain exactly one standalone placeholder element,
// so a caller-controlled instruction can never reshape the argument vector: it always
// occupies exactly one element. With Stdin=true the placeholder is forbidden outright.
func substituteInstruction(args []string, stdin bool, instruction string) ([]string, error) {
	out := make([]string, len(args))
	placeholders := 0

	for i, arg := range args {
		if arg == InstructionPlaceholder {
			placeholders++
			if stdin {
				return nil, fmt.Errorf("%w: placeholder %s is forbidden when Stdin is true", protocol.ErrInvalid, InstructionPlaceholder)
			}
			out[i] = instruction
			continue
		}
		if strings.Contains(arg, InstructionPlaceholder) {
			return nil, fmt.Errorf("%w: placeholder %s must be a standalone argument, found in %q", protocol.ErrInvalid, InstructionPlaceholder, arg)
		}
		out[i] = arg
	}

	if !stdin && placeholders != 1 {
		return nil, fmt.Errorf("%w: exactly one standalone %s argument is required when Stdin is false, found %d", protocol.ErrInvalid, InstructionPlaceholder, placeholders)
	}
	return out, nil
}

// environment converts the whitelist into the KEY=VALUE form expected by os/exec.
// It never calls os.Environ, so the child sees no inherited variables.
func environment(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

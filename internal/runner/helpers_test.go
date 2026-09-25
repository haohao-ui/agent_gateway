package runner

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Helper process protocol
//
// Every execution test re-runs this test binary as the controlled child. The mode
// travels in argv ("helper:<mode>") rather than in the environment, because the
// environment is exactly what Config.Env is meant to replace: a mode delivered
// through an environment variable would be erased by the whitelist under test.
const helperPrefix = "helper:"

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], helperPrefix) {
		os.Exit(runHelper(strings.TrimPrefix(os.Args[1], helperPrefix), os.Args[2:]))
	}
	os.Exit(m.Run())
}

// runHelper implements the child-side behaviour. It must never return for an
// unknown mode without a non-zero status: a silent success would turn a typo in a
// test into a false pass.
func runHelper(mode string, args []string) int {
	switch mode {
	case "echo-argv":
		// One line per element so the test can assert element boundaries exactly.
		for i, arg := range args {
			fmt.Printf("ARG[%d]=%s\n", i, arg)
		}
		return 0

	case "stdin-copy":
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintln(os.Stderr, "read stdin:", err)
			return 2
		}
		if _, err := os.Stdout.Write(b); err != nil {
			return 2
		}
		return 0

	case "pgid":
		return helperPrintPgid()

	case "stderr-write":
		if _, err := fmt.Fprint(os.Stderr, args[0]); err != nil {
			return 2
		}
		return 0

	case "stdin-ignore":
		// Never reads stdin, so a writer blocked on a full pipe can only be
		// released by the runner's join/force-close path.
		time.Sleep(millis(args, 0))
		return 0

	case "env-dump":
		for _, key := range args {
			if v, ok := os.LookupEnv(key); ok {
				fmt.Printf("%s=%s\n", key, v)
			} else {
				fmt.Printf("%s=<unset>\n", key)
			}
		}
		return 0

	case "pwd":
		wd, err := os.Getwd()
		if err != nil {
			return 2
		}
		fmt.Print(wd)
		return 0

	case "repeat-rune":
		// args: rune, count. Used for UTF-8 truncation boundaries.
		r := []rune(args[0])[0]
		count := atoi(args, 1)
		chunk := strings.Repeat(string(r), count)
		_, err := io.WriteString(os.Stdout, chunk)
		if err != nil {
			return 2
		}
		return 0

	case "raw-bytes":
		// args: byte value, count. Emits invalid UTF-8 on purpose.
		b := byte(atoi(args, 0))
		count := atoi(args, 1)
		chunk := make([]byte, count)
		for i := range chunk {
			chunk[i] = b
		}
		if _, err := os.Stdout.Write(chunk); err != nil {
			return 2
		}
		return 0

	case "exit":
		return atoi(args, 0)

	case "sleep":
		time.Sleep(millis(args, 0))
		return 0

	case "touch-after":
		// args: path, delay. Proves whether the child was still alive to finish.
		time.Sleep(millis(args, 1))
		if err := os.WriteFile(args[0], []byte("done"), 0o600); err != nil {
			return 2
		}
		return 0

	case "ignore-term":
		// args: pidfile ("-" to skip), duration. Survives SIGTERM so only the
		// forced escalation can stop it.
		signal.Ignore(syscall.SIGTERM)
		if args[0] != "-" {
			if err := os.WriteFile(args[0], []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
				return 2
			}
		}
		time.Sleep(millis(args, 1))
		return 0

	case "spawn":
		// args: pidfile, duration, stdout-mode ("inherit" or "null").
		// Starts a grandchild that ignores SIGTERM and records its pid, then sleeps
		// without ignoring SIGTERM so the parent dies on the graceful signal.
		self, err := os.Executable()
		if err != nil {
			return 2
		}
		child := exec.Command(self, helperPrefix+"ignore-term", args[0], args[1])
		if len(args) > 2 && args[2] == "inherit" {
			// Deliberately leaks the output pipe to the grandchild.
			child.Stdout = os.Stdout
			child.Stderr = os.Stderr
		} else {
			devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
			if err != nil {
				return 2
			}
			defer devNull.Close()
			child.Stdout = devNull
			child.Stderr = devNull
		}
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "spawn grandchild:", err)
			return 2
		}
		time.Sleep(millis(args, 1))
		return 0

	case "spawn-and-exit":
		// args: pidfile, grandchild duration, stdout-mode, stdin-mode.
		// Parent exits at once, leaving the grandchild behind.
		self, err := os.Executable()
		if err != nil {
			return 2
		}
		child := exec.Command(self, helperPrefix+"ignore-term", args[0], args[1])
		if len(args) > 3 && args[3] == "stdin" {
			// os/exec redirects a nil Stdin to the null device, so sharing the
			// inherited pipe has to be explicit.
			child.Stdin = os.Stdin
		}
		if len(args) > 2 && args[2] == "inherit" {
			child.Stdout = os.Stdout
			child.Stderr = os.Stderr
		} else {
			devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
			if err != nil {
				return 2
			}
			defer devNull.Close()
			child.Stdout = devNull
			child.Stderr = devNull
		}
		if err := child.Start(); err != nil {
			return 2
		}
		return 0

	case "hold-stdout":
		// args: duration. Holds the inherited stdout pipe open for the given time.
		time.Sleep(millis(args, 0))
		return 0
	}

	fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
	return 9
}

func atoi(args []string, i int) int {
	if i >= len(args) {
		return 0
	}
	n, err := strconv.Atoi(args[i])
	if err != nil {
		return 0
	}
	return n
}

func millis(args []string, i int) time.Duration {
	return time.Duration(atoi(args, i)) * time.Millisecond
}

// testBinary returns the absolute path of this test binary, which is used as the
// controlled executable.
func testBinary(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	if !filepath.IsAbs(exe) {
		t.Fatalf("test binary path is not absolute: %q", exe)
	}
	return exe
}

// newConfig builds a valid configuration for the helper binary.
func newConfig(t *testing.T, args ...string) Config {
	t.Helper()
	return Config{
		Executable:  testBinary(t),
		Args:        args,
		WorkDir:     t.TempDir(),
		Env:         map[string]string{"HELPER_MARKER": "1"},
		GracePeriod: 200 * time.Millisecond,
	}
}

// killPIDFromFile stops a helper process that a test started indirectly, so a
// stray grandchild does not outlive the test run.
func killPIDFromFile(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

// waitForFile polls until path exists, returning its content.
func waitForFile(t *testing.T, path string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return string(b)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, path)
	return ""
}

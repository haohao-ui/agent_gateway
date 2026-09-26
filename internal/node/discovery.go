package node

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"agent-gateway/internal/protocol"
)

// AgentSoftware alias for protocol.AgentSoftware.
type AgentSoftware = protocol.AgentSoftware

var knownCLIs = map[string]string{
	"claude":   "Claude Code",
	"codex":    "Codex CLI",
	"gemini":   "Gemini CLI",
	"hermes":   "Hermes",
	"aider":    "Aider",
	"opencode": "OpenCode",
	"cursor":   "Cursor CLI",
	"python3":  "Python 3",
	"node":     "Node.js",
	"docker":   "Docker",
	"git":      "Git",
	"bash":     "Bash Shell",
	"go":       "Go Runtime",
}

var knownDarwinApps = map[string]struct{ ID, Name string }{
	"Claude.app":          {"claude-desktop", "Claude Desktop"},
	"Cursor.app":          {"cursor-desktop", "Cursor Desktop"},
	"ChatGPT.app":         {"chatgpt", "ChatGPT"},
	"ChatGPT Classic.app": {"chatgpt-classic", "ChatGPT Classic"},
	"Doubao.app":          {"doubao", "Doubao"},
	"Windsurf.app":        {"windsurf", "Windsurf"},
}

// DiscoverInstalledAgents scans the host machine conservatively for known AI tools.
func DiscoverInstalledAgents(configuredAdapters []string) []AgentSoftware {
	found := make(map[string]AgentSoftware)

	// 1. Scan CLIs via LookPath and common directories
	for cmd, label := range knownCLIs {
		if path, err := exec.LookPath(cmd); err == nil && path != "" {
			found[cmd] = AgentSoftware{
				ID:       cmd,
				Name:     label,
				Kind:     "cli",
				Runnable: true,
			}
			continue
		}
		home, _ := os.UserHomeDir()
		commonDirs := []string{
			"/usr/local/bin",
			"/opt/homebrew/bin",
			filepath.Join(home, ".local", "bin"),
		}
		for _, dir := range commonDirs {
			candidate := filepath.Join(dir, cmd)
			if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
				found[cmd] = AgentSoftware{
					ID:       cmd,
					Name:     label,
					Kind:     "cli",
					Runnable: true,
				}
				break
			}
		}
	}

	// 2. Scan macOS GUI applications
	if runtime.GOOS == "darwin" {
		home, _ := os.UserHomeDir()
		appDirs := []string{
			"/Applications",
			filepath.Join(home, "Applications"),
		}
		for _, dir := range appDirs {
			for appName, meta := range knownDarwinApps {
				appPath := filepath.Join(dir, appName)
				if fi, err := os.Stat(appPath); err == nil && fi.IsDir() {
					if _, exists := found[meta.ID]; !exists {
						found[meta.ID] = AgentSoftware{
							ID:       meta.ID,
							Name:     meta.Name,
							Kind:     "gui",
							Runnable: false,
						}
					}
				}
			}
		}
	}

	// 3. Mark explicitly configured runnable adapters
	for _, name := range configuredAdapters {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		item, ok := found[name]
		if !ok {
			item = AgentSoftware{
				ID:   name,
				Name: name,
				Kind: "cli",
			}
		}
		item.Runnable = true
		found[name] = item
	}

	var result []AgentSoftware
	for _, item := range found {
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].ID < result[j].ID
	})
	return result
}

// DefaultShellAdapter detects a suitable local shell (bash, sh or cmd.exe) for running instructions.
func DefaultShellAdapter() (string, []string) {
	if runtime.GOOS == "windows" {
		if comspec := os.Getenv("COMSPEC"); comspec != "" {
			return comspec, []string{"/c", "{instruction}"}
		}
		return "cmd.exe", []string{"/c", "{instruction}"}
	}
	candidates := []string{"/bin/bash", "/usr/bin/bash", "/bin/sh", "/usr/bin/sh"}
	for _, candidate := range candidates {
		if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
			return candidate, []string{"-c", "{instruction}"}
		}
	}
	if path, err := exec.LookPath("bash"); err == nil {
		if abs, err := filepath.Abs(path); err == nil {
			return abs, []string{"-c", "{instruction}"}
		}
	}
	if path, err := exec.LookPath("sh"); err == nil {
		if abs, err := filepath.Abs(path); err == nil {
			return abs, []string{"-c", "{instruction}"}
		}
	}
	return "/bin/echo", []string{"{instruction}"}
}

// PopulateDefaultCapabilities registers runnable execution adapters for detected tools.
func (c *Config) PopulateDefaultCapabilities() {
	existing := make(map[string]bool)
	for i := range c.Capabilities {
		existing[c.Capabilities[i].Name] = true
	}
	if !existing["agent.run"] {
		shell, args := DefaultShellAdapter()
		if shell != "" {
			c.Capabilities = append(c.Capabilities, Capability{
				Name:           "agent.run",
				Version:        1,
				InstructionKey: defaultInstructionKey,
				Adapter: Adapter{
					Executable: shell,
					Args:       args,
				},
			})
			existing["agent.run"] = true
		}
	}

	// 2. Discover tool paths and populate capabilities
	tryAdd := func(name string, findCmd string, args []string) {
		if existing[name] {
			return
		}
		var binPath string
		if p, err := exec.LookPath(findCmd); err == nil && p != "" {
			binPath = p
		} else {
			home, _ := os.UserHomeDir()
			commonDirs := []string{
				"/usr/local/bin",
				"/opt/homebrew/bin",
				"/usr/bin",
				"/bin",
				filepath.Join(home, ".local", "bin"),
			}
			for _, dir := range commonDirs {
				candidate := filepath.Join(dir, findCmd)
				if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
					binPath = candidate
					break
				}
			}
		}
		if binPath != "" {
			if abs, err := filepath.Abs(binPath); err == nil {
				c.Capabilities = append(c.Capabilities, Capability{
					Name:           name,
					Version:        1,
					InstructionKey: defaultInstructionKey,
					Adapter: Adapter{
						Executable: abs,
						Args:       args,
					},
				})
				existing[name] = true
			}
		}
	}

	tryAdd("bash", "bash", []string{"-c", "{instruction}"})
	tryAdd("sh", "sh", []string{"-c", "{instruction}"})
	tryAdd("python3", "python3", []string{"-c", "{instruction}"})
	tryAdd("python", "python", []string{"-c", "{instruction}"})
	tryAdd("codex", "codex", []string{"exec", "--skip-git-repo-check", "--", "{instruction}"})
	tryAdd("hermes", "hermes", []string{"chat", "-m", "{instruction}"})
	tryAdd("claude", "claude", []string{"-p", "{instruction}"})
}

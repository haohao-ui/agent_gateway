package node

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// AgentSoftware describes a detected AI application or CLI tool on the node host.
type AgentSoftware struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"` // "cli" or "gui"
	Runnable bool   `json:"runnable"`
}

var knownCLIs = map[string]string{
	"claude":   "Claude Code",
	"codex":    "Codex CLI",
	"gemini":   "Gemini CLI",
	"hermes":   "Hermes",
	"aider":    "Aider",
	"opencode": "OpenCode",
	"cursor":   "Cursor CLI",
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
				Runnable: false,
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
					Runnable: false,
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

package protocol

import (
	"os/exec"
	"strings"
)

var (
	// Version represents the baseline semantic release version.
	Version = "0.1.1"

	// GitCommit is set via linker flags: -X agent-gateway/internal/protocol.GitCommit=<hash>
	GitCommit = ""

	// BuildTime is set via linker flags: -X agent-gateway/internal/protocol.BuildTime=<time>
	BuildTime = ""
)

func init() {
	if GitCommit == "" {
		if out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output(); err == nil {
			GitCommit = strings.TrimSpace(string(out))
		}
	}
}

// FullVersion returns version tagged with commit short hash, e.g. "0.1.0-cecf1b5".
func FullVersion() string {
	commit := strings.TrimSpace(GitCommit)
	if commit != "" {
		if len(commit) > 7 {
			commit = commit[:7]
		}
		return Version + "-" + commit
	}
	return Version
}

package node

import (
	"testing"
)

func TestDiscoverInstalledAgents(t *testing.T) {
	configured := []string{"custom-agent", "codex"}
	agents := DiscoverInstalledAgents(configured)

	if len(agents) == 0 {
		t.Fatalf("expected at least configured agents in discovered list")
	}

	foundCustom := false
	foundCodex := false
	for _, a := range agents {
		if a.ID == "custom-agent" {
			foundCustom = true
			if !a.Runnable {
				t.Errorf("custom-agent should be marked as runnable")
			}
		}
		if a.ID == "codex" {
			foundCodex = true
			if !a.Runnable {
				t.Errorf("codex should be marked as runnable since it was configured")
			}
		}
	}

	if !foundCustom {
		t.Errorf("configured custom-agent not found in discovered list")
	}
	if !foundCodex {
		t.Errorf("configured codex not found in discovered list")
	}
}

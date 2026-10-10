package agentkickoff

import (
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
)

// TestAClaudeCodeFirstTurnOutlastsItsRequestBudget: a first turn is bounded by
// a context; for claude-code that context must outlast the 35m request budget
// (agents.ClaudeCodeTurnTimeout), or the kickoff would cancel a turn its own
// HTTP client is still willing to wait for. The gateway kind keeps the
// configured turn exactly.
func TestAClaudeCodeFirstTurnOutlastsItsRequestBudget(t *testing.T) {
	d := &Deliverer{turn: 15 * time.Minute}
	if got := d.turnFor(agents.Agent{}); got != 15*time.Minute {
		t.Fatalf("gateway (unset kind) turn = %s, want the configured 15m", got)
	}
	if got := d.turnFor(agents.Agent{Kind: agents.KindGateway}); got != 15*time.Minute {
		t.Fatalf("gateway turn = %s, want 15m", got)
	}
	if got := d.turnFor(agents.Agent{Kind: agents.KindClaudeCode}); got != 37*time.Minute {
		t.Fatalf("claude-code turn = %s, want 37m (35m request + 2m slack)", got)
	}
	// A configured turn already longer than that wins.
	long := &Deliverer{turn: 90 * time.Minute}
	if got := long.turnFor(agents.Agent{Kind: agents.KindClaudeCode}); got != 90*time.Minute {
		t.Fatalf("claude-code turn with a longer configured turn = %s, want 90m", got)
	}
}

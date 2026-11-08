package ui

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// TestAPoolWorkersCardAndPageShowItsPool: the detail page carries the pool
// chip; an agent in no pool carries none. (The Agents list's card, which
// carried it too, is retired.)
func TestAPoolWorkersCardAndPageShowItsPool(t *testing.T) {
	// The card every served page has: the pool is in its ⋯ menu (PR #527).
	page := AgentDetailView{ID: 3, Name: "keen-wren", Kind: agents.KindClaudeCode, CCAccount: "alpha",
		Card: AgentCardView{ID: 3, Name: "keen-wren", Kind: agents.KindClaudeCode}}
	if html := renderString(t, AgentDetailPage(page, Features{})); strings.Contains(html, "data-worker-pool") {
		t.Fatalf("a detail page in no pool shows a pool chip")
	}
	page.WorkerPool = "reviewers"
	if html := renderString(t, AgentDetailPage(page, Features{})); !strings.Contains(html, `data-worker-pool="reviewers"`) {
		t.Fatalf("a pool worker's detail page has no pool chip")
	}
}

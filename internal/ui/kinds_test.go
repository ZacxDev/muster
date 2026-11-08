package ui

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// TestAClaudeCodeAgentsDetailShowsItsAccountNotAModelPicker: a claude-code agent
// has no per-agent model (POST /agents/{id}/model refuses it), so its detail
// page shows its account instead of a picker that can only fail — in its ONE
// header row's ⋯ menu (foldedHeader, PR #527), where the menu's Terminal item
// beside it is what says the kind. The gateway agent is the control.
func TestAClaudeCodeAgentsDetailShowsItsAccountNotAModelPicker(t *testing.T) {
	render := func(kind, account string) string {
		v := sampleAgentDetailView()
		v.Kind, v.CCAccount = kind, account
		v.Card = AgentCardView{ID: v.ID, Name: v.Name, Status: "running", Kind: kind}
		var b bytes.Buffer
		if err := RenderAgentDetail(&b, v, featuresAllOn); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	modelPost := `/model"`
	if gw := render(agents.KindGateway, ""); !strings.Contains(gw, modelPost) {
		t.Fatal("control: the gateway agent's detail has no model picker")
	}
	cc := render(agents.KindClaudeCode, "work")
	if strings.Contains(cc, modelPost) {
		t.Fatal("a claude-code agent's detail renders the model picker")
	}
	// The account, as plain text in the header's ⋯ menu, beside the menu's
	// Terminal item (no "kind Claude Code" chip — it wrapped into a tall pill on
	// a phone); and the header is the folded one.
	if !strings.Contains(cc, `data-folded-header`) {
		t.Fatal("a claude-code agent's detail does not draw the folded header")
	}
	i := strings.Index(cc, `id="`+AgentMenuID+`"`)
	if i < 0 {
		t.Fatal("a claude-code agent's detail has no ⋯ menu")
	}
	menu := cc[i:]
	if !strings.Contains(menu, `data-cc-account="work"`) || !strings.Contains(menu, `>work</span>`) || !strings.Contains(menu, `id="view-terminal"`) {
		t.Fatal("a claude-code agent's ⋯ menu does not name its account beside Terminal")
	}
	if strings.Count(cc, `data-cc-account=`) != 1 {
		t.Error("the account is drawn outside the ⋯ menu too")
	}
	if strings.Contains(cc, "Claude Code · work") {
		t.Error("the header still draws the kind chip")
	}
}

package ui

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// TestAClaudeCodeAgentsDetailShowsItsAccountNotAModelPicker: a claude-code agent
// has no per-agent model (POST /agents/{id}/model refuses it), so its detail
// page shows the kind and account instead of a picker that can only fail. The
// gateway agent is the control.
func TestAClaudeCodeAgentsDetailShowsItsAccountNotAModelPicker(t *testing.T) {
	render := func(kind, account string) string {
		v := sampleAgentDetailView()
		v.Kind, v.CCAccount = kind, account
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
	if !strings.Contains(cc, "Claude Code · work") {
		t.Fatal("a claude-code agent's detail does not name its kind and account")
	}
}

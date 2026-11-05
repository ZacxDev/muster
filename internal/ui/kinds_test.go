package ui

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// TestAClaudeCodeAgentsDetailShowsItsAccountNotAModelPicker: a claude-code agent
// has no per-agent model (POST /agents/{id}/model refuses it), so its detail
// page shows its account instead of a picker that can only fail. The
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
	// The account, as plain text in the header's second row; the Terminal
	// switch beside it is what says the kind (no "kind Claude Code" chip — it
	// wrapped into a tall pill on a phone).
	if !strings.Contains(cc, `data-cc-account="work"`) || !strings.Contains(cc, `>work</span>`) {
		t.Fatal("a claude-code agent's detail does not name its account")
	}
	if strings.Contains(cc, "Claude Code · work") {
		t.Error("the header still draws the kind chip")
	}
}

// TestTheFailoverIsAOneLineMarkerWhoseDetailOpensOnATap: a claude-code agent
// with a failover on record draws a "↻ switched" button in the header's second
// row that opens the whole line in a popover — a TAP target, because a title=
// tooltip never shows on a touch screen — and draws neither without one. The
// row is one line: no chip that wraps (the old "failed over" pill grew it to
// ~5 lines at 412px; e2e chief-mobile.spec.ts measures that).
func TestTheFailoverIsAOneLineMarkerWhoseDetailOpensOnATap(t *testing.T) {
	const line = "from north · rate_limited · resets Nov 6 22:00 UTC"
	v := sampleAgentDetailView()
	v.Kind, v.CCAccount = agents.KindClaudeCode, "work"
	if html := renderString(t, agentDetailHeader(v)); strings.Contains(html, "data-failover") {
		t.Fatal("control: an agent with no failover draws the marker")
	}
	v.LastFailover = line
	html := renderString(t, agentDetailHeader(v))
	i := strings.Index(html, `<button type="button" data-failover`)
	if i < 0 {
		t.Fatalf("no failover marker:\n%s", html)
	}
	btn := html[i:]
	btn = btn[:strings.Index(btn, "</button>")]
	for _, want := range []string{`popovertarget="` + agentFailoverPopoverID + `"`, "h-11", "whitespace-nowrap", "switched"} {
		if !strings.Contains(btn, want) {
			t.Errorf("the marker lacks %q: %s", want, btn)
		}
	}
	pop := html[strings.Index(html, `id="`+agentFailoverPopoverID+`"`):]
	for _, want := range []string{`popover="auto"`, "data-floating-menu", "data-failover-detail", line} {
		if !strings.Contains(pop, want) {
			t.Errorf("the popover lacks %q", want)
		}
	}
	if strings.Contains(html, "failed over</span>") {
		t.Error("the header still draws the failover chip")
	}
}

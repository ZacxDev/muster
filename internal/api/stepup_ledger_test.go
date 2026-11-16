package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// stepUpChannel is how a person whose request a step-up gate (stepup.Fresh)
// refuses as stale CONFIRMS — or why that gate refuses nobody.
type stepUpChannel struct {
	// how is one of:
	//   form:     a server-drawn form marked data-step-up-form on a page that
	//             draws the dialog and ui.StepUpFormScript (stepUpFormsHTML);
	//   script:   the page's script reads 403 + X-Muster-Step-Up and asks
	//             window.musterStepUp, then retries;
	//   terminal: the terminal page's attach (data-step-up);
	//   none:     not a refusal of a person's request (it decides something
	//             else, or records the freshness).
	how string
	// proof NAMES the test meant to show the page offers the confirm: a Test
	// in this package, or a browser spec (e2e/tests/…). ⚠ The ledger checks
	// only that it exists (and, for a spec, that it drives #term-confirm) —
	// NOT that it exercises this gate; that is the reader's to check.
	proof string
	// gap, for a channel with no proof yet, says so — in the ledger, where it
	// is seen, rather than nowhere. A form channel may not have one.
	gap string
}

// stepUpChannels is EVERY function in internal/api that calls stepup.Fresh,
// as <file>:<func>, and how its refusal is confirmed. 🔴 IT FAILS WHEN THE SET
// GROWS OR SHRINKS: a new step-up gate must say how its person confirms, and
// name a proof (or a gap) — the gap behind /teams/{id}/plugins, /hosts and
// /connectors, a team's cards and a team agent's terminal, each of which
// refused with "Confirm it's you" on a page that offered no confirm (#717).
var stepUpChannels = map[string]stepUpChannel{
	"attention_opencode.go:handleAttentionOpenCodeAnswer": {how: "form", proof: "TestTheOpenCodeRelayFormConfirmsInPlace"},
	"end_session.go:teamEndCardRefusal":                   {how: "form", proof: "TestATeamsCardsConfirmInPlaceForAnOperator"},
	"team_dispatch.go:teamCardRefusal":                    {how: "form", proof: "TestATeamsCardsConfirmInPlaceForAnOperator"},
	"outreach_batch.go:handleOutreachBatchDecide":         {how: "form", proof: "TestATeamsCardsConfirmInPlaceForAnOperator"}, // the Asks page's dialog, drawn with plugins off too
	"plugin_drafts.go:decideDraft":                        {how: "form", proof: "e2e/tests/plugins.spec.ts"},
	"plugin_pairing.go:decidePairForm":                    {how: "form", proof: "TestThePluginsPagePairingApproveConfirmsInPlace"},
	"team_admin.go:handleAgentRotateCredentials":          {how: "form", proof: "TestTheAgentPageOffersRotateCredentialsToThoseWhoMayRotate"},
	"team_connectors.go:decideTeamConnector":              {how: "form", proof: "TestTheTeamConnectorsPageConfirmsInPlace"},
	"team_connectors.go:handleTeamStandingRevoke":         {how: "form", proof: "TestTheTeamConnectorsPageConfirmsInPlace"},
	"team_hosts_page.go:teamHostsFresh":                   {how: "form", proof: "TestTheTeamHostsPageConfirmsInPlace"},
	"team_plugins_page.go:teamPluginsFresh":               {how: "form", proof: "TestTheTeamPluginsPageConfirmsInPlace"},
	"team_agents.go:teamAgentGate":                        {how: "terminal", proof: "TestATeamAgentsPagesConfirmInPlaceForAnOperator"},
	"mirror.go:handleMirrorTerminalWS":                    {how: "terminal", proof: "e2e/tests/mirror-stepup.spec.ts"},
	"mirror_view.go:mirrorSend":                           {how: "script", proof: "e2e/tests/mirror-stepup.spec.ts"},
	"mirror_ask.go:handleMirrorAskAnswer":                 {how: "script", gap: "no test drives a stale answer on a question card to the dialog (mirror_ask.go's script asks; unproven)"},
	"send_queue.go:sendQueueGate":                         {how: "script", gap: "no browser test drives a stale queue write to the dialog (send_queue.go's script asks; unproven)"},
	"send_queue_edit.go:sendQueueEdit":                    {how: "script", gap: "as sendQueueGate"},
	"send_queue_edit.go:sendQueueEditSave":                {how: "script", gap: "as sendQueueGate"},
	"send_queue_now.go:sendQueueSendNow":                  {how: "script", gap: "as sendQueueGate"},
	"send_queue.go:sendQueueDeliver":                      {how: "none"}, // the dispatcher's delivery of a queued row: the row's sign-in, not a person's request
	"chief_owner_release.go:typerSpeaksForOwner":          {how: "none"}, // whether a typed chat turn speaks for the owner
	"plugin_handoffs.go:handlePluginHandoffMint":          {how: "none"}, // records stepped_up on the hand-off; refuses nobody
	"mirror.go:freshForMillis":                            {how: "none"}, // how long the sign-in stays fresh, for the page's dialog
}

// stepUpGateSites parses internal/api's non-test sources and returns every
// function that calls stepup.Fresh, as <file>:<func>.
func stepUpGateSites(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var out []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			found := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Fresh" {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "stepup" {
						found = true
					}
				}
				return !found
			})
			if found {
				out = append(out, f+":"+fn.Name.Name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// testFuncs is every Test function declared in this package's _test.go files.
func testFuncs(t *testing.T) map[string]bool {
	t.Helper()
	files, _ := filepath.Glob("*_test.go")
	fset := token.NewFileSet()
	out := map[string]bool{}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") {
				out[fn.Name.Name] = true
			}
		}
	}
	return out
}

// TestEveryStepUpGateSaysHowItsPersonConfirms is the ledger: every step-up
// gate in internal/api is in stepUpChannels and every entry is still a gate
// (the set cannot grow or shrink unnoticed), each says how its person
// confirms, and each names a proof that EXISTS (or, outside form, an open
// gap). Whether the named proof exercises that gate is not checked here.
func TestEveryStepUpGateSaysHowItsPersonConfirms(t *testing.T) {
	sites := stepUpGateSites(t)
	if len(sites) < 10 {
		t.Fatalf("CONTROL: the parser found %d step-up gate(s) (%v); want the dozens there are", len(sites), sites)
	}
	seen := map[string]bool{}
	for _, s := range sites {
		seen[s] = true
		if _, ok := stepUpChannels[s]; !ok {
			t.Errorf("%s calls stepup.Fresh and is not in stepUpChannels: say how a person it refuses confirms (a form marked data-step-up-form on a page that draws stepUpFormsHTML, the page script, the terminal) and name the test that proves it", s)
		}
	}
	tests := testFuncs(t)
	for s, c := range stepUpChannels {
		if !seen[s] {
			t.Errorf("stepUpChannels lists %s, which no longer calls stepup.Fresh: remove it", s)
		}
		switch c.how {
		case "form", "script", "terminal":
		case "none":
			if c.proof != "" || c.gap != "" {
				t.Errorf("%s: a none entry names a proof or a gap", s)
			}
			continue
		default:
			t.Errorf("%s: unknown channel %q", s, c.how)
			continue
		}
		switch {
		case c.proof == "" && (c.gap == "" || c.how == "form"):
			t.Errorf("%s (%s): no proof that its page offers the confirm", s, c.how)
		case strings.HasPrefix(c.proof, "e2e/"):
			src, err := os.ReadFile(filepath.Join("..", "..", c.proof))
			if err != nil || !strings.Contains(string(src), "term-confirm") {
				t.Errorf("%s (%s): its proof %s is no browser spec that drives the dialog (#term-confirm): %v", s, c.how, c.proof, err)
			}
		case c.proof != "" && !tests[c.proof]:
			t.Errorf("%s (%s): its proof %s is no test in this package", s, c.how, c.proof)
		}
	}
}

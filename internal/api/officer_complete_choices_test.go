package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// officer_complete's `choices` (1-4 {value, label}: the decision buttons of an
// escalation's DM) reach the HQ API's complete AS SENT, and a choices argument
// the HQ API would refuse is refused by muster first, nothing sent, the item
// still held. Before this the tool did not offer the key (and closes its
// properties), and its dispatch had no field for it.
// ---------------------------------------------------------------------------

// choicesArg is what the forwarding test sends: two choices with distinct
// values and labels, compact (the HQ body is encoded by encoding/json, which
// compacts a raw value's whitespace and changes nothing else).
const choicesArg = `[{"value":"rollback","label":"Roll back the release"},{"value":"hold-2h","label":"Hold for two hours"}]`

// TestOfficerCompleteOffersChoices: every HQ officer pool's officer_complete
// offers `choices`, an array of at most 4 {value, label}; a builtin officer
// is not offered it.
func TestOfficerCompleteOffersChoices(t *testing.T) {
	r := newKubeOfficerRig(t)
	ctx := context.Background()
	for _, a := range []agents.Agent{
		r.addNamed(offKJudge+"1", 2401),
		r.addNamed(officers.InstanceName(offA, 1), 2402),
		r.addNamed(offKRout+"1", 2403),
	} {
		raw, ok := completePropsOf(t, r.s.MCPToolDefsFor(ctx, a))["choices"]
		if !ok {
			t.Errorf("%s: officer_complete offers no choices", a.Name)
			continue
		}
		var ch struct {
			Type     string `json:"type"`
			MaxItems int    `json:"maxItems"`
			Items    struct {
				Properties map[string]json.RawMessage `json:"properties"`
				Required   []string                   `json:"required"`
			} `json:"items"`
		}
		if err := json.Unmarshal(raw, &ch); err != nil {
			t.Fatalf("%s: choices %s: %v", a.Name, raw, err)
		}
		if ch.Type != "array" || ch.MaxItems != 4 {
			t.Errorf("%s: choices is %s, want an array of at most 4", a.Name, raw)
		}
		if len(ch.Items.Properties) != 2 || ch.Items.Properties["value"] == nil || ch.Items.Properties["label"] == nil || len(ch.Items.Required) != 2 {
			t.Errorf("%s: a choice is %s, want exactly value and label, both required", a.Name, raw)
		}
	}
	b := completePropsOf(t, []agents.ToolDef{completeToolDef(officers.Officer{ID: "marketing:example", Backend: officers.BackendBuiltin})})
	if _, has := b["choices"]; has {
		t.Errorf("a builtin officer's officer_complete offers choices")
	}
}

// TestOfficerCompleteForwardsChoicesToHQ: an escalation carrying choices
// reaches the HQ API's complete body with exactly those bytes; the same
// complete without choices sends no choices key.
func TestOfficerCompleteForwardsChoicesToHQ(t *testing.T) {
	r := newKubeOfficerRig(t)
	ctx := context.Background()
	judge := r.addNamed(offKJudge+"1", 2411)

	item := r.hq.AddTieredItem(offK, "followup.check", "choices-sent", officers.TierJudgement)
	r.hq.SetClaim(item, judge.Name, time.Now().Add(10*time.Minute))
	r.hq.ResetRequests()
	args := `{"item_id":"` + item + `","outcome":"escalated","summary":"the release regressed","choices":` + choicesArg + `}`
	if out := r.s.dispatchAgentTool(ctx, judge, completeTool, args); isToolErr(out) {
		t.Fatalf("a complete with choices: %s", out)
	}
	body := rawCompleteBody(t, r)
	got, sent := body["choices"]
	if !sent {
		t.Fatalf("choices not forwarded: the complete body has no choices key (%s)", completeRequests(r.hq)[0].Raw)
	}
	if string(got) != choicesArg {
		t.Errorf("the HQ API received choices %s, want exactly %s", got, choicesArg)
	}
	if it := r.hq.Item(item); it.Status != "escalated" {
		t.Errorf("the item is %s, want escalated", it.Status)
	}

	item = r.hq.AddTieredItem(offK, "followup.check", "choices-absent", officers.TierJudgement)
	r.hq.SetClaim(item, judge.Name, time.Now().Add(10*time.Minute))
	r.hq.ResetRequests()
	args = `{"item_id":"` + item + `","outcome":"escalated","summary":"the release regressed"}`
	if out := r.s.dispatchAgentTool(ctx, judge, completeTool, args); isToolErr(out) {
		t.Fatalf("a complete without choices: %s", out)
	}
	body = rawCompleteBody(t, r)
	if v, sent := body["choices"]; sent {
		t.Errorf("the HQ API received choices %s on a complete that sent none", v)
	}
	if _, ok := body["summary"]; !ok {
		t.Fatalf("the body read is wrong: no summary in %v", body)
	}
}

// TestOfficerCompleteRefusesBadChoicesLocally: choices the HQ API would
// refuse — five of them, a reserved value, choices on outcome done — are
// refused by muster with a message naming the problem, no complete reaches
// the HQ API, and the item stays claimed. The positive control (the same
// item, valid choices) is sent.
func TestOfficerCompleteRefusesBadChoicesLocally(t *testing.T) {
	r := newKubeOfficerRig(t)
	ctx := context.Background()
	judge := r.addNamed(offKJudge+"1", 2421)
	item := r.hq.AddTieredItem(offK, "followup.check", "choices-bad", officers.TierJudgement)
	r.hq.SetClaim(item, judge.Name, time.Now().Add(10*time.Minute))
	five := `[{"value":"a","label":"A"},{"value":"b","label":"B"},{"value":"c","label":"C"},{"value":"d","label":"D"},{"value":"e","label":"E"}]`
	for _, c := range []struct{ name, outcome, choices, want string }{
		{"five choices", "escalated", five, "at most 4 choices"},
		{"reserved value", "escalated", `[{"value":"resolve","label":"Resolve it"}]`, "own buttons"},
		{"outcome done", "done", `[{"value":"keep","label":"Keep"}]`, "only with outcome escalated"},
	} {
		r.hq.ResetRequests()
		args := `{"item_id":"` + item + `","outcome":"` + c.outcome + `","summary":"s","choices":` + c.choices + `}`
		out := r.s.dispatchAgentTool(ctx, judge, completeTool, args)
		if !isToolErr(out) || !strings.Contains(out, c.want) {
			t.Errorf("%s: %s, want muster's refusal containing %q", c.name, out, c.want)
		}
		if n := len(completeRequests(r.hq)); n != 0 {
			t.Errorf("%s: %d complete request(s) reached the HQ API, want none", c.name, n)
		}
		if it := r.hq.Item(item); it.Status != "claimed" {
			t.Errorf("%s: the item is %s, want still claimed", c.name, it.Status)
		}
	}
	r.hq.ResetRequests()
	args := `{"item_id":"` + item + `","outcome":"escalated","summary":"s","choices":[{"value":"keep","label":"Keep"}]}`
	if out := r.s.dispatchAgentTool(ctx, judge, completeTool, args); isToolErr(out) {
		t.Fatalf("the positive control (valid choices) was refused: %s", out)
	}
	if n := len(completeRequests(r.hq)); n != 1 {
		t.Fatalf("the positive control sent %d complete request(s), want 1", n)
	}
}

// choicesSentence is the instruction about an escalation's options, pinned
// as a literal (never read from the constant it checks).
const choicesSentence = "When your escalation offers the person options, send them in the same call as choices " +
	"(at most 4 {value, label}: a short label, and a value that stays the same whenever you offer that option), never as prose. " +
	"Each becomes a button; a tap comes back to you as a directive item: carry it out within your charter."

// TestAnHQOfficerInstanceIsToldToSendChoices: an HQ officer instance's MCP
// instructions and system prompt end their escalation paragraph with the
// choices sentence, and its kickoff carries it in step 3; an ordinary worker
// is not told it.
func TestAnHQOfficerInstanceIsToldToSendChoices(t *testing.T) {
	r := newOfficerRig(t)
	r.s.ext.Attention = stubAttention{}
	inst := r.addInstance(r.a, 1, 803)
	ctx := context.Background()
	want := `Use attention_ask only if your charter explicitly says to. ` + choicesSentence
	for name, in := range map[string]string{"MCP instructions": r.s.mcpInstructionsFor(ctx, inst), "system prompt": r.s.AgentInstructions(inst)} {
		if !strings.Contains(in, want) {
			t.Errorf("an HQ officer instance's %s lack the choices sentence:\n%s", name, in)
		}
	}
	if k := officers.KickoffText(r.a, 1); !strings.Contains(k, want+"\n") {
		t.Errorf("an HQ officer's kickoff lacks the choices sentence in step 3:\n%s", k)
	}
	worker := r.agent(t, officerOrdinaryID)
	for name, in := range map[string]string{"MCP instructions": r.s.mcpInstructionsFor(ctx, worker), "system prompt": r.s.AgentInstructions(worker)} {
		if strings.Contains(in, choicesSentence) {
			t.Errorf("an ordinary worker's %s carry the choices sentence", name)
		}
	}
}

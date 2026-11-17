package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
)

// ---------------------------------------------------------------------------
// officer_complete's `actions` and `action_reports` reach HQ's complete AS
// SENT (civ-hq docs/OFFICERS.md §5d "HQ resolves an escalation whose actions
// are reported done"). Before this, officer_complete's schema closed its
// properties (additionalProperties: false) without them, the dispatch decoded
// neither and officers.Completion had no field for them: a release officer
// could not send either, so HQ's structured-actions resolve could never run.
//
// The fixtures follow officer_complete_attribution_test.go: compact JSON,
// keys and items deliberately out of sorted order (a value re-encoded through
// a Go map would come back sorted), each distinct from the other.
// ---------------------------------------------------------------------------

const (
	actionsArg       = `[{"target":"20271112120000_article_system_category","kind":"migration_applied"},{"kind":"manual","target":"Ops: run the backfill"}]`
	actionReportsArg = `[{"status":"done","item_id":4182,"index":0,"evidence":"pg_class has ArticleCategory"}]`
)

var actionFields = []struct{ key, arg string }{
	{"actions", actionsArg},
	{"action_reports", actionReportsArg},
}

// TestOfficerCompleteForwardsTheActionFieldsAsSent: each field, sent alone,
// reaches HQ byte for byte and the other is no key at all; both together
// reach HQ with every attribution field absent.
func TestOfficerCompleteForwardsTheActionFieldsAsSent(t *testing.T) {
	r := newKubeOfficerRig(t)
	ctx := context.Background()
	judge := r.addNamed(offKJudge+"1", 2301)
	for i, f := range actionFields {
		item := r.hq.AddTieredItem(offK, "release.evaluate", "civitai-web:img-"+f.key, officers.TierJudgement)
		r.hq.SetClaim(item, judge.Name, time.Now().Add(10*time.Minute))
		r.hq.ResetRequests()
		args := `{"item_id":"` + item + `","outcome":"escalated","summary":"RATING: medium — DB owner: apply it ` + string(rune('a'+i)) + `","` + f.key + `":` + f.arg + `}`
		if out := r.s.dispatchAgentTool(ctx, judge, completeTool, args); isToolErr(out) {
			t.Fatalf("%s: %s", f.key, out)
		}
		body := rawCompleteBody(t, r)
		if got := string(body[f.key]); got != f.arg {
			t.Errorf("%s: HQ received %q, want %q", f.key, got, f.arg)
		}
		for _, other := range append(append([]struct{ key, arg string }{}, actionFields...), attributionFields...) {
			if other.key == f.key {
				continue
			}
			if v, sent := body[other.key]; sent {
				t.Errorf("%s: HQ received %s %s, which the instance did not send", f.key, other.key, v)
			}
		}
	}
	item := r.hq.AddTieredItem(offK, "release.evaluate", "civitai-web:img-both", officers.TierJudgement)
	r.hq.SetClaim(item, judge.Name, time.Now().Add(10*time.Minute))
	r.hq.ResetRequests()
	args := `{"item_id":"` + item + `","outcome":"escalated","summary":"RATING: medium — DB owner: apply it","actions":` + actionsArg + `,"action_reports":` + actionReportsArg + `}`
	if out := r.s.dispatchAgentTool(ctx, judge, completeTool, args); isToolErr(out) {
		t.Fatalf("both: %s", out)
	}
	body := rawCompleteBody(t, r)
	for _, f := range actionFields {
		if got := string(body[f.key]); got != f.arg {
			t.Errorf("both: HQ received %s %q, want %q", f.key, got, f.arg)
		}
	}
	if it := r.hq.Item(item); it.Status != "escalated" {
		t.Fatalf("the item is %s, want escalated", it.Status)
	}
}

// TestOfficerCompleteSendsNoActionKeyWhenNoneIsGiven: a plain complete sends
// neither key. ⚠ An INVARIANT guard at the base (which sent neither ever);
// the positive control is the test above.
func TestOfficerCompleteSendsNoActionKeyWhenNoneIsGiven(t *testing.T) {
	r := newKubeOfficerRig(t)
	ctx := context.Background()
	judge := r.addNamed(offKJudge+"1", 2311)
	item := r.hq.AddTieredItem(offK, "release.evaluate", "civitai-web:img-none", officers.TierJudgement)
	r.hq.SetClaim(item, judge.Name, time.Now().Add(10*time.Minute))
	r.hq.ResetRequests()
	if out := r.s.dispatchAgentTool(ctx, judge, completeTool, `{"item_id":"`+item+`","outcome":"done","summary":"RATING: low — no action"}`); isToolErr(out) {
		t.Fatalf("a plain complete: %s", out)
	}
	body := rawCompleteBody(t, r)
	for _, f := range actionFields {
		if v, sent := body[f.key]; sent {
			t.Errorf("HQ received %s %s on a complete that sent none", f.key, v)
		}
	}
}

// TestOfficerCompleteOffersTheActionFieldsToAnHQOfficerOnly: every HQ
// officer's officer_complete schema has both properties (the schema is
// closed, so without them an instance cannot send them); a builtin officer's
// has neither (its backend would drop them).
func TestOfficerCompleteOffersTheActionFieldsToAnHQOfficerOnly(t *testing.T) {
	for _, c := range []struct {
		name string
		o    officers.Officer
		want bool
	}{
		{"builtin", officers.Officer{ID: "marketing:example", Backend: officers.BackendBuiltin}, false},
		{"hq", officers.Officer{ID: "release:civitai-web"}, true},
		{"hq routine", officers.Officer{ID: "release:civitai-web", Backend: officers.BackendHQ, Tier: officers.TierRoutine}, true},
	} {
		props := completePropsOf(t, []agents.ToolDef{completeToolDef(c.o)})
		for _, f := range actionFields {
			if _, has := props[f.key]; has != c.want {
				t.Errorf("%s: officer_complete offers %s: %v, want %v", c.name, f.key, has, c.want)
			}
		}
	}
	r := newKubeOfficerRig(t)
	props := completePropsOf(t, r.s.MCPToolDefsFor(context.Background(), r.addNamed(offKRout+"1", 2321)))
	for _, f := range actionFields {
		if _, ok := props[f.key]; !ok {
			t.Errorf("a routine HQ instance's officer_complete offers no %s", f.key)
		}
	}
}

// TestAnHQWithoutActionsRefusesThemAndTheRetryWithoutSucceeds: against an HQ
// that predates the fields (its decoder disallows unknown fields), a complete
// carrying `actions` gets HQ's generic 400 and the item stays held; the same
// complete without it is served.
func TestAnHQWithoutActionsRefusesThemAndTheRetryWithoutSucceeds(t *testing.T) {
	r := newKubeOfficerRig(t)
	ctx := context.Background()
	r.hq.CompleteFields = hqtest.CompleteFieldsWithout("actions", "action_reports")
	judge := r.addNamed(offKJudge+"1", 2331)
	item := r.hq.AddTieredItem(offK, "release.evaluate", "civitai-web:img-old-hq", officers.TierJudgement)
	r.hq.SetClaim(item, judge.Name, time.Now().Add(10*time.Minute))
	r.hq.ResetRequests()
	out := r.s.dispatchAgentTool(ctx, judge, completeTool, `{"item_id":"`+item+`","outcome":"escalated","summary":"RATING: medium — DB owner: apply it","actions":`+actionsArg+`}`)
	if !isToolErr(out) || !strings.Contains(out, `"status":400`) || !strings.Contains(out, "body must be one JSON object") {
		t.Fatalf("actions against an HQ without them: %s, want HQ's generic 400", out)
	}
	if it := r.hq.Item(item); it.Status != "claimed" || it.ClaimedBy != judge.Name {
		t.Fatalf("a refused complete left the item %s held by %q, want it still held", it.Status, it.ClaimedBy)
	}
	if out := r.s.dispatchAgentTool(ctx, judge, completeTool, `{"item_id":"`+item+`","outcome":"escalated","summary":"RATING: medium — DB owner: apply it"}`); isToolErr(out) {
		t.Fatalf("the same complete without actions: %s, want it served", out)
	}
}

// TestOfficerClaimToolNamesTheOpenActions: officer_claim's description (what
// an instance reads of the claim's answer) names memory.open_actions and
// what to do with it, as its whole sentence.
func TestOfficerClaimToolNamesTheOpenActions(t *testing.T) {
	const want = "The answer's memory is what HQ recorded of your earlier work: history, open_threads (your escalations still open) and, when you have any, " +
		"open_actions (the structured actions of those escalations still to confirm, least recently checked first: re-check them as your charter says). "
	for _, d := range queueToolDefs() {
		if d.Name != officers.ToolClaim {
			continue
		}
		if !strings.Contains(d.Description, want) {
			t.Fatalf("officer_claim's description %q does not carry %q", d.Description, want)
		}
		return
	}
	t.Fatalf("%s is not offered", officers.ToolClaim)
}

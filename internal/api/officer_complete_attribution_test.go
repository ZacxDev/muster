package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
)

// ---------------------------------------------------------------------------
// officer_complete's `authors`, `attributed_prs`, `attribution` and `tickets`
// reach HQ's complete AS SENT (civ-hq docs/OFFICERS.md §5i "Author mail",
// §5b "Attributed PRs", and the release-ticket kind). Before this, muster's
// dispatch decoded none of them and officers.Completion had no field for
// them, so every one was dropped silently and no attributed author was ever
// told.
//
// The fixtures are compact JSON with no <, > or & (the client's json.Marshal
// compacts and HTML-escapes a raw value; neither changes what HQ decodes) and
// with object keys and list items deliberately OUT of sorted order: a value
// decoded into a Go map or struct and re-encoded would come back sorted, so
// byte equality at HQ proves the value went through unchanged. Each fixture
// is distinct from every other, so a field forwarded under the wrong key
// cannot pass.
// ---------------------------------------------------------------------------

const (
	authorsArg       = `{"zeta-author":"Your #7259 broke comment.toggleReaction","alpha-author":"Your #7251 slowed image.getInfinite"}`
	attributedPRsArg = `[7259,7251]`
	attributionArg   = `"confirmed"`
	ticketsArg       = `[{"ticket_id":48213,"pr":7259,"attribution":"likely","gist":"users see a blank gallery after the release"}]`
)

// attributionFields are the four keys, each with its fixture.
var attributionFields = []struct{ key, arg string }{
	{"authors", authorsArg},
	{"attributed_prs", attributedPRsArg},
	{"attribution", attributionArg},
	{"tickets", ticketsArg},
}

// rawCompleteBody is the one complete request HQ saw, as its top-level keys
// mapped to their raw bytes on the wire.
func rawCompleteBody(t *testing.T, r *officerRig) map[string]json.RawMessage {
	t.Helper()
	reqs := completeRequests(r.hq)
	if len(reqs) != 1 {
		t.Fatalf("HQ saw %d complete requests, want 1", len(reqs))
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(reqs[0].Raw, &body); err != nil {
		t.Fatalf("the complete body %q: %v", reqs[0].Raw, err)
	}
	return body
}

// TestOfficerCompleteForwardsEachAttributionFieldAsSent: each of the four
// fields, sent alone, reaches HQ byte for byte, and the other three are no
// key at all (an HQ that predates a field refuses it, even as null). Every
// pool sends them: a release-ticket item is a routine item.
func TestOfficerCompleteForwardsEachAttributionFieldAsSent(t *testing.T) {
	r := newKubeOfficerRig(t)
	ctx := context.Background()
	judge := r.addNamed(offKJudge+"1", 2201)
	routine := r.addNamed(offKRout+"1", 2202)
	for i, f := range attributionFields {
		for _, c := range []struct {
			a    agents.Agent
			tier string
		}{{judge, officers.TierJudgement}, {routine, officers.TierRoutine}} {
			item := r.hq.AddTieredItem(offK, "followup.check", "rel-"+f.key+"-"+c.tier, c.tier)
			r.hq.SetClaim(item, c.a.Name, time.Now().Add(10*time.Minute))
			r.hq.ResetRequests()
			args := `{"item_id":"` + item + `","outcome":"escalated","summary":"release ` + string(rune('a'+i)) + ` regressed","` + f.key + `":` + f.arg + `}`
			if out := r.s.dispatchAgentTool(ctx, c.a, completeTool, args); isToolErr(out) {
				t.Fatalf("%s from %s: %s", f.key, c.a.Name, out)
			}
			body := rawCompleteBody(t, r)
			if got := string(body[f.key]); got != f.arg {
				t.Errorf("%s from %s: HQ received %s %q, want %q", f.key, c.a.Name, f.key, got, f.arg)
			}
			for _, other := range attributionFields {
				if other.key == f.key {
					continue
				}
				if v, sent := body[other.key]; sent {
					t.Errorf("%s from %s: HQ received %s %s, which the instance did not send", f.key, c.a.Name, other.key, v)
				}
			}
		}
	}
}

// TestOfficerCompleteSendsNoAttributionKeyWhenNoneIsGiven: a complete with
// none of the four sends none of them — not null, not an empty list. The
// positive control is the test above: the same body read sees each key when
// it is sent. ⚠ An INVARIANT guard at the base (which sent none of the four
// ever); it pins the omission against a forwarder that sends every key
// unconditionally.
func TestOfficerCompleteSendsNoAttributionKeyWhenNoneIsGiven(t *testing.T) {
	r := newKubeOfficerRig(t)
	ctx := context.Background()
	judge := r.addNamed(offKJudge+"1", 2211)
	item := r.hq.AddTieredItem(offK, "followup.check", "rel-none", officers.TierJudgement)
	r.hq.SetClaim(item, judge.Name, time.Now().Add(10*time.Minute))
	r.hq.ResetRequests()
	if out := r.s.dispatchAgentTool(ctx, judge, completeTool, `{"item_id":"`+item+`","outcome":"done","summary":"the release is clean"}`); isToolErr(out) {
		t.Fatalf("a plain complete: %s", out)
	}
	body := rawCompleteBody(t, r)
	for _, f := range attributionFields {
		if v, sent := body[f.key]; sent {
			t.Errorf("HQ received %s %s on a complete that sent none", f.key, v)
		}
	}
	if _, ok := body["summary"]; !ok {
		t.Fatalf("the body read is wrong: no summary in %v", body)
	}
}

// TestOfficerCompleteForwardsAnEmptyListTheInstanceSent: a value the instance
// did send is forwarded unchanged, an empty list and null included — HQ, not
// muster, decides whether it takes one.
func TestOfficerCompleteForwardsAnEmptyListTheInstanceSent(t *testing.T) {
	r := newKubeOfficerRig(t)
	ctx := context.Background()
	routine := r.addNamed(offKRout+"1", 2221)
	item := r.hq.AddTieredItem(offK, "followup.tickets", "sweep-1", officers.TierRoutine)
	r.hq.SetClaim(item, routine.Name, time.Now().Add(10*time.Minute))
	r.hq.ResetRequests()
	if out := r.s.dispatchAgentTool(ctx, routine, completeTool, `{"item_id":"`+item+`","outcome":"done","summary":"no ticket is related","tickets":[],"attribution":null}`); isToolErr(out) {
		t.Fatalf("an empty tickets list: %s", out)
	}
	body := rawCompleteBody(t, r)
	if got := string(body["tickets"]); got != `[]` {
		t.Errorf("HQ received tickets %q, want []", got)
	}
	if got, sent := body["attribution"]; !sent || string(got) != `null` {
		t.Errorf("HQ received attribution %q (sent %v), want the null the instance sent", got, sent)
	}
}

// TestOfficerCompleteForwardsTheInvestigationWithTheAttributionFields: an
// investigating officer's escalation carrying its investigation block AND all
// four fields reaches HQ with every one, each byte for byte, and HQ stores
// the investigation.
func TestOfficerCompleteForwardsTheInvestigationWithTheAttributionFields(t *testing.T) {
	r := newKubeOfficerRig(t)
	ctx := context.Background()
	r.hq.Investigating = map[string]bool{offK: true}
	judge := r.addNamed(offKJudge+"1", 2231)
	item := r.hq.AddTieredItem(offK, "followup.check", "rel-all", officers.TierJudgement)
	r.hq.SetClaim(item, judge.Name, time.Now().Add(10*time.Minute))
	r.hq.ResetRequests()
	args := `{"item_id":"` + item + `","outcome":"escalated","summary":"PR 7259 broke reactions","investigation":` + investigationArg
	for _, f := range attributionFields {
		args += `,"` + f.key + `":` + f.arg
	}
	args += `}`
	if out := r.s.dispatchAgentTool(ctx, judge, completeTool, args); isToolErr(out) {
		t.Fatalf("the escalation was refused: %s", out)
	}
	body := rawCompleteBody(t, r)
	if got := string(body["investigation"]); got != investigationArg {
		t.Errorf("HQ received investigation %q, want %q", got, investigationArg)
	}
	for _, f := range attributionFields {
		if got := string(body[f.key]); got != f.arg {
			t.Errorf("HQ received %s %q, want %q", f.key, got, f.arg)
		}
	}
	if it := r.hq.Item(item); it.Status != "escalated" || it.Investigation == nil {
		t.Fatalf("the item is %s with investigation %v, want escalated with the block", it.Status, it.Investigation)
	}
}

// TestOfficerCompleteOffersTheAttributionFieldsToEveryPool: every pool's
// officer_complete schema has the four properties, so an instance knows to
// send them.
func TestOfficerCompleteOffersTheAttributionFieldsToEveryPool(t *testing.T) {
	r := newKubeOfficerRig(t)
	ctx := context.Background()
	for _, a := range []agents.Agent{
		r.addNamed(offKJudge+"1", 2241),
		r.addNamed(officers.InstanceName(offA, 1), 2242),
		r.addNamed(offKRout+"1", 2243),
	} {
		props := completePropsOf(t, r.s.MCPToolDefsFor(ctx, a))
		for _, f := range attributionFields {
			if _, ok := props[f.key]; !ok {
				t.Errorf("%s: officer_complete offers no %s", a.Name, f.key)
			}
		}
	}
}

// completePropsOf is officer_complete's schema properties as defs offer it.
func completePropsOf(t *testing.T, defs []agents.ToolDef) map[string]json.RawMessage {
	t.Helper()
	for _, d := range defs {
		if d.Name != completeTool {
			continue
		}
		b, err := json.Marshal(d.Parameters)
		if err != nil {
			t.Fatal(err)
		}
		var p struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(b, &p); err != nil {
			t.Fatal(err)
		}
		return p.Properties
	}
	t.Fatalf("%s is not offered", completeTool)
	return nil
}

// TestAnHQWithoutTicketsRefusesThemAndTheRetryWithoutSucceeds: against an HQ
// that predates `tickets` (its decoder disallows unknown fields), a complete
// carrying it gets HQ's generic 400 — naming no field — and nothing is
// written: the item stays held. The same complete without `tickets` is then
// served. A fake that accepted every field could not see this.
func TestAnHQWithoutTicketsRefusesThemAndTheRetryWithoutSucceeds(t *testing.T) {
	r := newKubeOfficerRig(t)
	ctx := context.Background()
	r.hq.CompleteFields = hqtest.CompleteFieldsWithout("tickets")
	routine := r.addNamed(offKRout+"1", 2251)
	item := r.hq.AddTieredItem(offK, "followup.tickets", "sweep-old-hq", officers.TierRoutine)
	r.hq.SetClaim(item, routine.Name, time.Now().Add(10*time.Minute))
	r.hq.ResetRequests()
	out := r.s.dispatchAgentTool(ctx, routine, completeTool, `{"item_id":"`+item+`","outcome":"escalated","summary":"two tickets match PR 7259","tickets":`+ticketsArg+`}`)
	if !isToolErr(out) || !strings.Contains(out, `"status":400`) || !strings.Contains(out, "body must be one JSON object") {
		t.Fatalf("tickets against an HQ without them: %s, want HQ's generic 400", out)
	}
	if strings.Contains(out, "tickets") {
		t.Errorf("HQ's refusal of an unknown field named it: %s (real HQ's decoder names none)", out)
	}
	if it := r.hq.Item(item); it.Status != "claimed" || it.ClaimedBy != routine.Name {
		t.Fatalf("a refused complete left the item %s held by %q, want it still held", it.Status, it.ClaimedBy)
	}
	out = r.s.dispatchAgentTool(ctx, routine, completeTool, `{"item_id":"`+item+`","outcome":"escalated","summary":"two tickets match PR 7259","attributed_prs":`+attributedPRsArg+`}`)
	if isToolErr(out) {
		t.Fatalf("the same complete without tickets: %s, want it served", out)
	}
	if it := r.hq.Item(item); it.Status != "escalated" {
		t.Fatalf("the retry left the item %s, want escalated", it.Status)
	}
}

// TestOfficerCompleteOffersTheHQFieldsToAnHQOfficerOnly: a builtin officer's
// officer_complete has none of the four (its backend would drop them), an HQ
// officer's has all four — judgement, routine and untiered alike.
func TestOfficerCompleteOffersTheHQFieldsToAnHQOfficerOnly(t *testing.T) {
	for _, c := range []struct {
		name string
		o    officers.Officer
		want bool
	}{
		{"builtin", officers.Officer{ID: "marketing:example", Backend: officers.BackendBuiltin}, false},
		{"builtin routine", officers.Officer{ID: "marketing:example", Backend: officers.BackendBuiltin, Tier: officers.TierRoutine}, false},
		{"hq", officers.Officer{ID: "triage:example"}, true},
		{"hq explicit", officers.Officer{ID: "triage:example", Backend: officers.BackendHQ, Tier: officers.TierRoutine}, true},
	} {
		props := completePropsOf(t, []agents.ToolDef{completeToolDef(c.o)})
		for _, f := range attributionFields {
			if _, has := props[f.key]; has != c.want {
				t.Errorf("%s: officer_complete offers %s: %v, want %v", c.name, f.key, has, c.want)
			}
		}
	}
}

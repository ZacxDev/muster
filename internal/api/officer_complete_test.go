package api

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
)

// ---------------------------------------------------------------------------
// officer_complete's two per-pool arguments, end to end through the HQ fake:
//
//   - `investigation` (civ-hq docs/OFFICERS.md "Investigation"): HQ REQUIRES
//     it to escalate an item of a type whose policy sets investigation: true.
//     A judgement (or untiered) instance is offered it and muster forwards it
//     to HQ AS SENT — HQ owns its validation, and HQ's 400 comes back to the
//     instance with HQ's own words. A routine instance is not offered it: its
//     hand-off is needs_judgement.
//   - outcome `needs_judgement`: HQ takes it for a ROUTINE item only (it
//     promotes the item to judgement in place). A routine instance is
//     offered and served it; every other instance is refused it by muster.
//
// The tool name and outcomes are literals on purpose: the charters name them.
// ---------------------------------------------------------------------------

const completeTool = "officer_complete"

// investigationArg is a valid investigation block. fix.pr_urls is an EMPTY
// list on purpose: a block re-encoded through a Go struct with omitempty
// drops it, so seeing it at HQ proves the block went through as sent.
const investigationArg = `{"hypotheses":["the web pods ran out of memory","a bad deploy"],` +
	`"eliminated":[{"hypothesis":"a bad deploy","evidence":"no rollout in the window","audit_ids":[9001,9002]}],` +
	`"root_cause":"the cache sidecar leaks memory under load","confidence":"medium",` +
	`"fix":{"pr_urls":[],"operator_steps":["raise the sidecar memory limit","restart the web pods"]},` +
	`"not_checked":["node kernel logs"]}`

// completeRequests are the complete requests HQ saw since the last reset.
func completeRequests(hq *hqtest.Server) []hqtest.Request {
	var out []hqtest.Request
	for _, q := range hq.Requests() {
		if strings.HasSuffix(q.Path, "/complete") {
			out = append(out, q)
		}
	}
	return out
}

// completeSchemaOf is officer_complete's schema as defs offer it: its outcome
// enum (sorted) and whether it has an investigation property. ok is false
// when the tool is not offered.
func completeSchemaOf(t *testing.T, defs []agents.ToolDef) (outcomes []string, investigation, ok bool) {
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
		var outcome struct {
			Enum []string `json:"enum"`
		}
		_ = json.Unmarshal(p.Properties["outcome"], &outcome)
		sort.Strings(outcome.Enum)
		_, investigation = p.Properties["investigation"]
		return outcome.Enum, investigation, true
	}
	return nil, false, false
}

// TestOfficerCompleteOffersEachPoolItsArguments: the judgement instance and
// an untiered officer's instance are offered investigation and the three
// closing outcomes; the routine instance is offered needs_judgement as well,
// and no investigation.
func TestOfficerCompleteOffersEachPoolItsArguments(t *testing.T) {
	r := newKubeOfficerRig(t)
	ctx := context.Background()
	for _, c := range []struct {
		name          string
		a             agents.Agent
		outcomes      string
		investigation bool
	}{
		{"judgement", r.addNamed(offKJudge+"1", 2101), "done,escalated,no_action", true},
		{"untiered", r.addNamed(officers.InstanceName(offA, 1), 2102), "done,escalated,no_action", true},
		{"routine", r.addNamed(offKRout+"1", 2103), "done,escalated,needs_judgement,no_action", false},
	} {
		outcomes, inv, ok := completeSchemaOf(t, r.s.MCPToolDefsFor(ctx, c.a))
		if !ok {
			t.Fatalf("%s: %s is not offered", c.name, completeTool)
		}
		if strings.Join(outcomes, ",") != c.outcomes || inv != c.investigation {
			t.Errorf("%s: outcomes %v, investigation %v; want %s, %v", c.name, outcomes, inv, c.outcomes, c.investigation)
		}
	}
}

// TestOfficerCompleteForwardsTheInvestigationAsSent: a judgement instance of
// an investigating officer escalates with an investigation block; HQ receives
// exactly that block (the empty pr_urls included) and stores it on the item.
func TestOfficerCompleteForwardsTheInvestigationAsSent(t *testing.T) {
	r := newKubeOfficerRig(t)
	ctx := context.Background()
	r.hq.Investigating = map[string]bool{offK: true}
	judge := r.addNamed(offKJudge+"1", 2111)
	item := r.hq.AddTieredItem(offK, "anomaly.alert", "node-a", officers.TierJudgement)
	r.hq.SetClaim(item, judge.Name, time.Now().Add(10*time.Minute))
	r.hq.ResetRequests()

	args := `{"item_id":"` + item + `","outcome":"escalated","summary":"the cache sidecar leaks; an operator must raise its limit","investigation":` + investigationArg + `}`
	out := r.s.dispatchAgentTool(ctx, judge, completeTool, args)
	if isToolErr(out) {
		t.Fatalf("the escalation was refused: %s", out)
	}
	reqs := completeRequests(r.hq)
	if len(reqs) != 1 {
		t.Fatalf("HQ saw %d complete requests, want 1", len(reqs))
	}
	var want map[string]any
	if err := json.Unmarshal([]byte(investigationArg), &want); err != nil {
		t.Fatal(err)
	}
	wb, _ := json.Marshal(want)
	gb, _ := json.Marshal(reqs[0].Body["investigation"])
	if string(gb) != string(wb) {
		t.Fatalf("HQ received investigation %s, want %s", gb, wb)
	}
	if it := r.hq.Item(item); it.Status != "escalated" || it.Investigation == nil {
		t.Fatalf("the item is %s with investigation %v, want escalated with the block", it.Status, it.Investigation)
	}
}

// TestOfficerCompleteReturnsHQsInvestigationRefusalVerbatim: a block HQ
// refuses comes back to the instance with HQ's status and HQ's own message,
// so it can fix the block; nothing is completed. muster checks none of the
// block's fields itself.
func TestOfficerCompleteReturnsHQsInvestigationRefusalVerbatim(t *testing.T) {
	r := newKubeOfficerRig(t)
	ctx := context.Background()
	r.hq.Investigating = map[string]bool{offK: true}
	judge := r.addNamed(offKJudge+"1", 2121)
	item := r.hq.AddTieredItem(offK, "anomaly.alert", "node-a", officers.TierJudgement)
	r.hq.SetClaim(item, judge.Name, time.Now().Add(10*time.Minute))

	bad := strings.Replace(investigationArg, `"confidence":"medium"`, `"confidence":"certain"`, 1)
	out := r.s.dispatchAgentTool(ctx, judge, completeTool, `{"item_id":"`+item+`","outcome":"escalated","summary":"s","investigation":`+bad+`}`)
	if !isToolErr(out) || !strings.Contains(out, `"status":400`) || !strings.Contains(out, "investigation.confidence must be one of low, medium, high") {
		t.Fatalf("HQ's refusal of the block: %s, want HQ's 400 with its own message", out)
	}
	if it := r.hq.Item(item); it.Status != "claimed" {
		t.Fatalf("a refused complete left the item %s", it.Status)
	}
}

// TestOfficerCompleteWithoutTheRequiredInvestigationGetsHQsRefusal: an
// escalation of an investigating officer's item with no block is HQ's 400,
// naming the field. ⚠ An INVARIANT guard: HQ's refusal came back before this
// change too.
func TestOfficerCompleteWithoutTheRequiredInvestigationGetsHQsRefusal(t *testing.T) {
	r := newKubeOfficerRig(t)
	ctx := context.Background()
	r.hq.Investigating = map[string]bool{offK: true}
	judge := r.addNamed(offKJudge+"1", 2131)
	item := r.hq.AddTieredItem(offK, "anomaly.alert", "node-a", officers.TierJudgement)
	r.hq.SetClaim(item, judge.Name, time.Now().Add(10*time.Minute))
	out := r.s.dispatchAgentTool(ctx, judge, completeTool, `{"item_id":"`+item+`","outcome":"escalated","summary":"s"}`)
	if !isToolErr(out) || !strings.Contains(out, `"status":400`) || !strings.Contains(out, "investigation is required") {
		t.Fatalf("no block: %s, want HQ's 400 naming investigation", out)
	}
}

// TestARoutineInstanceIsRefusedAnInvestigation: a routine instance sending
// investigation is refused by muster, pointed at needs_judgement, and
// nothing reaches HQ — the block is never silently dropped.
func TestARoutineInstanceIsRefusedAnInvestigation(t *testing.T) {
	r := newKubeOfficerRig(t)
	ctx := context.Background()
	r.hq.Investigating = map[string]bool{offK: true}
	routine := r.addNamed(offKRout+"1", 2141)
	item := r.hq.AddTieredItem(offK, "anomaly.alert", "node-r", officers.TierRoutine)
	r.hq.SetClaim(item, routine.Name, time.Now().Add(10*time.Minute))
	r.hq.ResetRequests()
	out := r.s.dispatchAgentTool(ctx, routine, completeTool, `{"item_id":"`+item+`","outcome":"escalated","summary":"s","investigation":`+investigationArg+`}`)
	if !isToolErr(out) || !strings.Contains(out, "needs_judgement") {
		t.Fatalf("a routine instance's investigation: %s, want muster's refusal naming needs_judgement", out)
	}
	if n := len(completeRequests(r.hq)); n != 0 {
		t.Fatalf("%d complete requests reached HQ", n)
	}
	// the positive control: JSON null is no block, and the same complete is sent
	out = r.s.dispatchAgentTool(ctx, routine, completeTool, `{"item_id":"`+item+`","outcome":"no_action","summary":"s","investigation":null}`)
	if isToolErr(out) || len(completeRequests(r.hq)) != 1 {
		t.Fatalf("investigation null: %s (%d complete requests), want it served", out, len(completeRequests(r.hq)))
	}
	if _, sent := completeRequests(r.hq)[0].Body["investigation"]; sent {
		t.Fatal("investigation null was sent to HQ")
	}
}

// TestARoutineInstanceHandsAnItemToJudgement: a routine instance completes
// its routine item with outcome needs_judgement; HQ receives that outcome
// and promotes the item to judgement in place, keeping the summary. A
// judgement instance and an untiered officer's instance are refused the
// outcome by muster, before HQ.
func TestARoutineInstanceHandsAnItemToJudgement(t *testing.T) {
	r := newKubeOfficerRig(t)
	ctx := context.Background()
	routine := r.addNamed(offKRout+"1", 2151)
	item := r.hq.AddTieredItem(offK, "anomaly.alert", "node-r", officers.TierRoutine)
	r.hq.SetClaim(item, routine.Name, time.Now().Add(10*time.Minute))
	r.hq.ResetRequests()
	const summary = "three nodes disagree on the alert; this needs a judgement read"
	out := r.s.dispatchAgentTool(ctx, routine, completeTool, `{"item_id":"`+item+`","outcome":"needs_judgement","summary":"`+summary+`"}`)
	if isToolErr(out) {
		t.Fatalf("needs_judgement from a routine instance: %s", out)
	}
	reqs := completeRequests(r.hq)
	if len(reqs) != 1 || reqs[0].Body["outcome"] != "needs_judgement" {
		t.Fatalf("HQ saw %+v, want one complete with outcome needs_judgement", reqs)
	}
	if it := r.hq.Item(item); it.Tier != "judgement" || it.Status != "open" || it.ClaimedBy != "" || it.RoutineSummary != summary {
		t.Fatalf("the item is tier %q status %q held by %q with routine summary %q; want judgement, open, unheld, the summary",
			it.Tier, it.Status, it.ClaimedBy, it.RoutineSummary)
	}

	judge := r.addNamed(offKJudge+"1", 2152)
	plain := r.addNamed(officers.InstanceName(offA, 1), 2153)
	jItem := r.hq.AddTieredItem(offK, "anomaly.alert", "node-j", officers.TierJudgement)
	r.hq.SetClaim(jItem, judge.Name, time.Now().Add(10*time.Minute))
	pItem := r.hq.AddItem(offA, "pr.review", "pr-9")
	r.hq.SetClaim(pItem, plain.Name, time.Now().Add(10*time.Minute))
	r.hq.ResetRequests()
	for a, it := range map[*agents.Agent]string{&judge: jItem, &plain: pItem} {
		out := r.s.dispatchAgentTool(ctx, *a, completeTool, `{"item_id":"`+it+`","outcome":"needs_judgement","summary":"s"}`)
		if !isToolErr(out) || !strings.Contains(out, "outcome must be one of done, escalated, no_action") {
			t.Errorf("%s sending needs_judgement: %s, want muster's refusal", a.Name, out)
		}
	}
	if n := len(completeRequests(r.hq)); n != 0 {
		t.Fatalf("%d refused needs_judgement completes reached HQ", n)
	}
}

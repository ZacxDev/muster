package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officership"
)

// The shepherd's loop through HQ, end to end through muster's tool dispatch
// with the shepherd's own token: the merge-ask tool, shadow, and the shadow
// block a completion carries.

// shepMergeAsk is a valid merge ask's arguments for #5 under item id.
func shepMergeAsk(id string) map[string]any {
	return map[string]any{"item_id": id, "repo": shepRepoAPI, "pr": 5, "head_sha": shepHeadAPI, "base_ref": "trunk",
		"base_sha": "9999999999999999999999999999999999999999", "summary": "Raise the limit", "purpose": "The worker OOMs",
		"how": "Two audit rounds; CI green",
		"evidence": map[string]any{"officer_item": map[string]any{"officer": offShepAuth, "item": 3901},
			"open_yellows":       []any{map[string]any{"where": "deploy.yaml:4", "text": "naming"}},
			"protected_identity": map[string]any{"result": "clean"}, "settled_at": "2026-11-12T10:00:00Z", "rollback": "revert"}}
}

func (r *shepAPIRig) mergeAsk(t *testing.T, args map[string]any) string {
	t.Helper()
	b, _ := json.Marshal(args)
	return r.s.dispatchAgentTool(context.Background(), r.inst, officers.ToolShepherdMergeAsk, string(b))
}

func TestTheMergeAskIsTheShepherdsOnly(t *testing.T) {
	r := newShepAPIRig(t)
	ctx := context.Background()
	sess, _ := r.s.OfficerClaudeCodeSession(r.inst)
	if !officerToolNameSet(r.s.MCPToolDefsFor(ctx, r.inst))[officers.ToolShepherdMergeAsk] ||
		!strings.Contains(","+strings.Join(sess.AllowMCPTools, ",")+",", ","+officers.ToolShepherdMergeAsk+",") {
		t.Fatal("the shepherd is not offered and pre-allowed officer_shepherd_merge_ask")
	}
	if officerToolNameSet(r.s.MCPToolDefsFor(ctx, r.authInst))[officers.ToolShepherdMergeAsk] {
		t.Fatal("the authoring draft officer is offered the merge-ask tool")
	}
	b, _ := json.Marshal(shepMergeAsk("1"))
	if out := r.s.dispatchAgentTool(ctx, r.authInst, officers.ToolShepherdMergeAsk, string(b)); !strings.Contains(out, "is not one of officer") {
		t.Fatalf("the draft officer called the merge-ask tool: %s", out)
	}
	// pr-opened is muster's own call: no tool of any officer reaches it.
	for _, a := range []string{r.inst.Name, r.authInst.Name} {
		inst := r.inst
		if a == r.authInst.Name {
			inst = r.authInst
		}
		for name := range officerToolNameSet(r.s.MCPToolDefsFor(ctx, inst)) {
			if strings.Contains(name, "opened") || strings.Contains(name, "pr_open") {
				t.Errorf("%s is offered %s", a, name)
			}
		}
	}
}

func TestTheMergeAskIsPostedWithTheCallerAsInstance(t *testing.T) {
	r := newShepAPIRig(t)
	id := r.hold(t)
	out := r.mergeAsk(t, shepMergeAsk(id))
	if isToolErr(out) || !strings.Contains(out, `"merge"`) {
		t.Fatalf("a valid merge ask: %s", out)
	}
	calls := r.hq.MergeAskRequests()
	if len(calls) != 1 || calls[0].Path != "/api/v1/officers/"+offShep+"/items/"+id+"/merge-asks" || calls[0].Auth != "Bearer "+offShepTok {
		t.Fatalf("merge-ask calls %+v, want one to item %s with the shepherd's token", calls, id)
	}
	if calls[0].Body["instance"] != r.inst.Name || calls[0].Body["head_sha"] != shepHeadAPI {
		t.Fatalf("body instance %v head %v, want the caller %s", calls[0].Body["instance"], calls[0].Body["head_sha"], r.inst.Name)
	}
	// An `instance` argument is refused, not overridden: HQ never asked.
	args := shepMergeAsk(id)
	args["instance"] = "officer-shepherd-delta-infra-9"
	if out := r.mergeAsk(t, args); !strings.Contains(out, "instance is not an argument") || len(r.hq.MergeAskRequests()) != 1 {
		t.Fatalf("an instance argument: %s (calls %d)", out, len(r.hq.MergeAskRequests()))
	}
}

func TestTheMergeAskNeedsTheLiveHoldOfThatItemForThatPullRequest(t *testing.T) {
	r := newShepAPIRig(t)
	id := r.hold(t)
	other := r.hq.AddItem(offShep, "pr-shepherd.opened", officers.ShepherdSubject(shepRepoAPI, 6))
	r.hq.SetClaim(other, r.inst.Name, time.Now().Add(time.Hour))
	if out := r.mergeAsk(t, shepMergeAsk(other)); !strings.Contains(out, `"gate":"item"`) || !strings.Contains(out, "is about") {
		t.Fatalf("an item for another pull request: %s", out)
	}
	r.hq.SetClaim(id, "officer-shepherd-delta-infra-2", time.Now().Add(time.Hour))
	if out := r.mergeAsk(t, shepMergeAsk(id)); !strings.Contains(out, `"gate":"item"`) || !strings.Contains(out, "is claimed by officer-shepherd-delta-infra-2") {
		t.Fatalf("another holder: %s", out)
	}
	r.hq.SetClaim(id, r.inst.Name, time.Now().Add(-time.Minute))
	if out := r.mergeAsk(t, shepMergeAsk(id)); !strings.Contains(out, `"gate":"item"`) || !strings.Contains(out, "has expired") {
		t.Fatalf("a lapsed lease: %s", out)
	}
	if n := len(r.hq.MergeAskRequests()); n != 0 {
		t.Fatalf("HQ was asked %d time(s) without a live hold", n)
	}
}

// TestTheMergeAsksBoundsAreHQs: muster checks no bound of its own — an
// out-of-bounds summary reaches HQ, and HQ's 400 comes back verbatim.
func TestTheMergeAsksBoundsAreHQs(t *testing.T) {
	r := newShepAPIRig(t)
	id := r.hold(t)
	r.hq.MergeAskStatus, r.hq.MergeAskCode, r.hq.MergeAskMessage = http.StatusBadRequest, "invalid", "summary is required: what the pull request does, in at most 300 characters"
	args := shepMergeAsk(id)
	args["summary"] = strings.Repeat("x", 301)
	out := r.mergeAsk(t, args)
	if !strings.Contains(out, `"status":400`) || !strings.Contains(out, "in at most 300 characters") {
		t.Fatalf("a long summary: %s, want HQ's 400 verbatim", out)
	}
	if n := len(r.hq.MergeAskRequests()); n != 1 {
		t.Fatalf("HQ saw %d merge ask(s), want the one", n)
	}
}

// TestTheFakeDecodesTheMergeAskAsHQDoes: the fake's merge-asks route decodes
// strictly into the copy of HQ's types, so a body with a string item id is
// its 400 — the control that the route CAN refuse what HQ refuses.
func TestTheFakeDecodesTheMergeAskAsHQDoes(t *testing.T) {
	r := newShepAPIRig(t)
	id := r.hold(t)
	c, err := officers.Serves[officers.ShipBackend](r.s.officerRT.hq.Backend(r.s.officerRT.reg.All()[1]), officers.CapShip)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.MergeAsk(context.Background(), officers.ItemID(id), []byte(`{"instance":"x","evidence":{"officer_item":{"item":"3901"}}}`))
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("a string item id at the fake: %v, want HQ's 400", err)
	}
	_, err = c.MergeAsk(context.Background(), officers.ItemID(id), []byte(`{"instance":"x","evidence":{"ladder":{"rounds":[]}}}`))
	if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("an unknown field at the fake: %v, want HQ's 400", err)
	}
	if out := r.mergeAsk(t, shepMergeAsk(id)); isToolErr(out) {
		t.Fatalf("muster's own body at the fake: %s", out)
	}
}

func TestHQsMergeAskRefusalIsPassedBackVerbatim(t *testing.T) {
	r := newShepAPIRig(t)
	id := r.hold(t)
	r.hq.MergeAskStatus, r.hq.MergeAskCode, r.hq.MergeAskMessage = http.StatusUnprocessableEntity, "audit_not_stopped", "the audit ladder of #5 has not stopped at 888888888888"
	out := r.mergeAsk(t, shepMergeAsk(id))
	for _, want := range []string{`"status":422`, `"code":"audit_not_stopped"`, `"error":"the audit ladder of #5 has not stopped at 888888888888"`} {
		if !strings.Contains(out, want) {
			t.Errorf("HQ's 422 came back as %s; missing %s", out, want)
		}
	}
	r.hq.MergeAskStatus, r.hq.MergeAskCode, r.hq.MergeAskMessage = http.StatusBadGateway, "github_unavailable", "GitHub did not answer"
	out = r.mergeAsk(t, shepMergeAsk(id))
	if !strings.Contains(out, `"status":502`) || !strings.Contains(out, `"code":"github_unavailable"`) {
		t.Errorf("HQ's 502 github_unavailable came back as %s", out)
	}
}

// TestShadowIsReadFromHQAndTheCompletionCarriesTheBlock: an item HQ marks
// shadow refuses the shepherd's mark ready and merge ask with nothing sent;
// with no item shadow, the officer row decides (and an unreadable one is
// shadow); the instance's officer_complete then carries the block of what
// muster refused, once.
func TestShadowIsReadFromHQAndTheCompletionCarriesTheBlock(t *testing.T) {
	r := newShepAPIRig(t)
	id := r.hold(t)
	yes, no := true, false
	r.hq.SetItemShadow(id, &yes)
	if out := r.ready(t); !strings.Contains(out, `"gate":"shadow"`) || !strings.Contains(out, "shadow: would have marked "+shepRepoAPI+"#5 ready") {
		t.Fatalf("item shadow, mark ready: %s", out)
	}
	if out := r.mergeAsk(t, shepMergeAsk(id)); !strings.Contains(out, "shadow: would have asked HQ for a merge ask of "+shepRepoAPI+"#5 at 888888888888") {
		t.Fatalf("item shadow, merge ask: %s", out)
	}
	if len(r.shepGH.readiedPRs()) != 0 || len(r.hq.MergeAskRequests()) != 0 {
		t.Fatalf("a shadowed write was sent: readied %v, merge asks %d", r.shepGH.readiedPRs(), len(r.hq.MergeAskRequests()))
	}
	// Absent on the item: the officer row decides.
	r.hq.SetItemShadow(id, nil)
	r.hq.SetOfficerShadow(offShep, true)
	if out := r.mergeAsk(t, shepMergeAsk(id)); !strings.Contains(out, "HQ runs officer "+offShep+" in shadow") {
		t.Fatalf("officer shadow: %s", out)
	}
	r.hq.SetOfficerShadow(offShep, false)
	r.hq.NoShadowField = true
	if out := r.mergeAsk(t, shepMergeAsk(id)); !strings.Contains(out, "could not be read for its shadow") {
		t.Fatalf("an officer row without shadow: %s", out)
	}
	r.hq.NoShadowField = false
	r.hq.SetItemShadow(id, &no)
	// The instance completes: its text, then the block, once.
	text := "Settled #5 over one round."
	complete := func() string {
		b, _ := json.Marshal(map[string]any{"item_id": id, "outcome": officers.CompleteOutcomes(r.s.officerRT.reg.All()[1])[0], "summary": text})
		return r.s.dispatchAgentTool(context.Background(), r.inst, officers.ToolComplete, string(b))
	}
	if out := complete(); isToolErr(out) {
		t.Fatalf("complete: %s", out)
	}
	got := r.hq.Item(id).Summary
	want := text + "\n\n" + shadowHeader + "\n- would have marked " + shepRepoAPI + "#5 ready" +
		"\n- would have asked HQ for a merge ask of " + shepRepoAPI + "#5 at 888888888888"
	if got != want {
		t.Fatalf("the completion summary is\n%q\nwant\n%q", got, want)
	}
	if strings.Count(got, shadowHeader) != 1 {
		t.Fatalf("the block appears %d times", strings.Count(got, shadowHeader))
	}
	if r.s.officerRT.shadowSummary(offShep, officers.ItemID(id), "x", officers.MaxSummaryChars) != "x" {
		t.Fatal("the lines outlived the completion that carried them")
	}
}

// TestTheDraftOfficersGateTellsHQWithItsOwnToken: the authoring draft
// officer's push gate is wired to pr-opened, which posts with THAT officer's
// token (the seam the officership tests stub).
func TestTheDraftOfficersGateTellsHQWithItsOwnToken(t *testing.T) {
	r := newShepAPIRig(t)
	pg, ok := r.s.callerOf(r.authInst).Gate.(*officership.PushGate)
	if !ok || pg.PROpened == nil {
		t.Fatalf("the draft officer's gate has no pr-opened: %#v", r.s.callerOf(r.authInst).Gate)
	}
	if _, err := pg.PROpened(context.Background(), officers.PROpened{Repo: shepRepoAPI, PR: 5, HeadSHA: shepHeadAPI, HeadRef: "officer/x", Base: "trunk",
		Author: "delta-investigator[bot]"}); err != nil {
		t.Fatal(err)
	}
	calls := r.hq.PROpenedRequests()
	if len(calls) != 1 || calls[0].Auth != "Bearer "+offShepAuthT || calls[0].Path != "/api/v1/officers/"+offShepAuth+"/pr-opened" {
		t.Fatalf("pr-opened calls %+v, want one with the authoring officer's token", calls)
	}
}

// TestTheShadowBlockIsCutNeverTheInstancesText: at HQ's bound the block loses
// whole lines (then says how many), never a character of the instance's own
// summary; with no room for the header, no block at all.
func TestTheShadowBlockIsCutNeverTheInstancesText(t *testing.T) {
	lines := []string{"would have pushed aaaaaaaaaaaa..bbbbbbbbbbbb to officer/x", "would have commented on o/r#5 (12 chars)", "would have marked o/r#5 ready"}
	text := strings.Repeat("s", 100)
	full := withShadowBlock(text, lines, 0, 1000)
	if !strings.HasPrefix(full, text+"\n\n"+shadowHeader) || strings.Count(full, "\n- would have") != 3 {
		t.Fatalf("room for all: %q", full)
	}
	head := len("\n\n"+shadowHeader) + 100
	cut := withShadowBlock(text, lines, 0, head+len("\n- "+lines[0])+len("\n- … and 2 more"))
	if !strings.HasPrefix(cut, text) || !strings.Contains(cut, lines[0]) || !strings.HasSuffix(cut, "\n- … and 2 more") || strings.Contains(cut, lines[1]) {
		t.Fatalf("room for one line: %q", cut)
	}
	if got := withShadowBlock(text, lines, 0, 110); got != text {
		t.Fatalf("no room for the header: %q, want the instance's text alone", got)
	}
	big := strings.Repeat("é", officers.MaxSummaryChars)
	if got := withShadowBlock(big, lines, 0, officers.MaxSummaryChars); got != big || utf8.RuneCountInString(got) != officers.MaxSummaryChars {
		t.Fatal("a summary already at the bound was changed")
	}
}

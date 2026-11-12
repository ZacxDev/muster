package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/officers"
)

// officer_complete's `handoff`, and the AUTHORING officer's pull-request fix
// item (kind *.pr_fix), end to end through muster with HQ faked.

const fixHead = "3c5e7a9b1d2f4a6c8e0b2d4f6a8c0e1f3a5b7c9d"

// completeAs completes item id as agent a with outcome and the extra args.
func (r *shepAPIRig) completeAs(a any, id, outcome string, extra map[string]any) string {
	args := map[string]any{"item_id": id, "outcome": outcome, "summary": "Fixed the naming the audit flagged."}
	for k, v := range extra {
		args[k] = v
	}
	b, _ := json.Marshal(args)
	if a == r.authInst.Name {
		return r.s.dispatchAgentTool(context.Background(), r.authInst, officers.ToolComplete, string(b))
	}
	return r.s.dispatchAgentTool(context.Background(), r.inst, officers.ToolComplete, string(b))
}

// completes are the raw bodies of every complete HQ saw for item id.
func (r *shepAPIRig) completes(id string) [][]byte {
	var out [][]byte
	for _, q := range r.hq.Requests() {
		if q.Method == http.MethodPost && strings.HasSuffix(q.Path, "/items/"+id+"/complete") {
			out = append(out, q.Raw)
		}
	}
	return out
}

func TestOfficerCompleteSendsTheHandoffOnlyWhenGiven(t *testing.T) {
	r := newShepAPIRig(t)
	id := r.hold(t)
	if out := r.completeAs(r.inst.Name, id, "done", map[string]any{"handoff": map[string]any{"head_sha": fixHead}}); isToolErr(out) {
		t.Fatalf("complete with a handoff: %s", out)
	}
	got := r.completes(id)
	if len(got) != 1 {
		t.Fatalf("%d completes sent", len(got))
	}
	if !strings.Contains(string(got[0]), `"handoff":{"head_sha":"`+fixHead+`"}`) {
		t.Fatalf("the body %s does not carry the handoff byte for byte", got[0])
	}
	id2 := r.hold(t)
	if out := r.completeAs(r.inst.Name, id2, "done", nil); isToolErr(out) {
		t.Fatalf("complete without a handoff: %s", out)
	}
	if b := r.completes(id2); len(b) != 1 || strings.Contains(string(b[0]), "handoff") {
		t.Fatalf("a complete without a handoff sent %s", b)
	}
}

func TestABadHandoffIsRefusedBeforeHQ(t *testing.T) {
	r := newShepAPIRig(t)
	id := r.hold(t)
	for name, c := range map[string]struct {
		outcome string
		handoff any
		want    string
	}{
		"a short sha":       {"done", map[string]any{"head_sha": fixHead[:12]}, "40 lower-case hex"},
		"an upper sha":      {"done", map[string]any{"head_sha": strings.ToUpper(fixHead)}, "40 lower-case hex"},
		"no head_sha":       {"done", map[string]any{}, "40 lower-case hex"},
		"an extra key":      {"done", map[string]any{"head_sha": fixHead, "round": 3}, "handoff must be exactly"},
		"not an object":     {"done", fixHead, "handoff must be exactly"},
		"outcome escalated": {"escalated", map[string]any{"head_sha": fixHead}, "handoff goes with outcome done only (HQ refuses it on escalated)"},
	} {
		t.Run(name, func(t *testing.T) {
			before := len(r.completes(id))
			out := r.completeAs(r.inst.Name, id, c.outcome, map[string]any{"handoff": c.handoff})
			if !isToolErr(out) || !strings.Contains(out, c.want) {
				t.Fatalf("got %s, want a refusal containing %q", out, c.want)
			}
			if len(r.completes(id)) != before {
				t.Fatal("HQ was sent the complete")
			}
		})
	}
}

func TestHQsHandoffRefusalIsPassedBackAndTheItemStaysHeld(t *testing.T) {
	r := newShepAPIRig(t)
	id := r.hold(t)
	r.hq.RefuseCompletes(http.StatusConflict, "round_cap", "the loop of example-org/delta-infra#5 is at its round cap")
	out := r.completeAs(r.inst.Name, id, "done", map[string]any{"handoff": map[string]any{"head_sha": fixHead}})
	for _, want := range []string{`"status":409`, `"code":"round_cap"`, `"error":"the loop of example-org/delta-infra#5 is at its round cap"`} {
		if !strings.Contains(out, want) {
			t.Errorf("HQ's 409 came back as %s; missing %s", out, want)
		}
	}
	if it := r.hq.Item(id); it.Status != "claimed" || it.ClaimedBy != r.inst.Name {
		t.Fatalf("after HQ refused the complete the item is %s by %s, want still claimed by %s", it.Status, it.ClaimedBy, r.inst.Name)
	}
}

// authPush is a push by the AUTHORING officer's instance to its #5 branch,
// through its gate as the proxy would hand it.
func (r *shepAPIRig) authPush(t *testing.T) (*http.Request, error) {
	t.Helper()
	g := r.s.callerOf(r.authInst).Gate
	body := wirePkt("1111111111111111111111111111111111111111 "+fixHead+" refs/heads/officer/investigate-delta-infra/5-fix\x00report-status\n") + "0000"
	rq := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
	rq = rq.WithContext(connectors.WithCaller(rq.Context(), connectors.Caller{AgentName: r.authInst.Name}))
	return g.Admit(rq, "github.com", shepRepoAPI+".git/git-receive-pack")
}

// wirePkt is s as one pkt-line of the push protocol.
func wirePkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

// fixFindings is HQ's `findings` on a fix item: the auditor's report, a
// STRING, verbatim. muster never parses it.
const fixFindings = "VERDICT: no-red · round 2 · head 888888888888\n🟡 a log line could be clearer\n🟢 nit: naming"

// TestTheAuthorsFixItemThroughMuster: the authoring draft officer holds a
// pr_fix item for its own #5 in HQ (findings a string, as HQ writes it); in
// shadow its push is refused before GitHub with the would-have line; its
// complete WITH a handoff is then refused (the head was never pushed) and
// nothing reaches HQ; its complete escalated goes through and carries the
// shadow block.
func TestTheAuthorsFixItemThroughMuster(t *testing.T) {
	r := newShepAPIRig(t)
	r.authGH.mu.Lock()
	r.authGH.author = "delta-investigator[bot]"
	r.authGH.mu.Unlock()
	fix := r.hq.AddItem(offShepAuth, "investigate.pr_fix", officers.ShepherdSubject(shepRepoAPI, 5))
	r.hq.SetPayload(fix, `{"pr_loop":{"repo":"`+shepRepoAPI+`","pr":5,"head_sha":"`+shepHeadAPI+`"},"findings":`+strconv.Quote(fixFindings)+`,"from":{"officer_id":"audit:delta-infra"}}`)
	r.hq.SetClaim(fix, r.authInst.Name, time.Now().Add(time.Hour))
	yes := true
	r.hq.SetItemShadow(fix, &yes)
	out, err := r.authPush(t)
	want := "shadow: would have pushed 111111111111.." + fixHead[:12] + " to officer/investigate-delta-infra/5-fix"
	if out != nil || !errors.Is(err, connectors.ErrGateRefused) || !strings.Contains(err.Error(), want) {
		t.Fatalf("a shadowed fix push: err = %v (forwarded %v), want %q", err, out != nil, want)
	}
	// A hand-off now is HQ's to refuse (409 nothing_pushed): muster keeps
	// no memory of pushes to decide it, and passes HQ's answer back.
	r.hq.RefuseCompletes(http.StatusConflict, "nothing_pushed", "no push reached #5 since the fix item was claimed")
	res := r.completeAs(r.authInst.Name, fix, "done", map[string]any{"handoff": map[string]any{"head_sha": fixHead}})
	if !strings.Contains(res, `"code":"nothing_pushed"`) || !strings.Contains(res, `"status":409`) {
		t.Fatalf("a handoff after a shadowed push: %s, want HQ's 409 verbatim", res)
	}
	r.hq.RefuseCompletes(0, "", "")
	r.hq.ResetRequests()
	if res := r.completeAs(r.authInst.Name, fix, "escalated", nil); isToolErr(res) {
		t.Fatalf("complete escalated: %s", res)
	}
	sent := r.completes(fix)
	if len(sent) != 1 {
		t.Fatalf("%d completes sent", len(sent))
	}
	var body struct {
		Summary string          `json:"summary"`
		Handoff json.RawMessage `json:"handoff"`
	}
	if err := json.Unmarshal(sent[0], &body); err != nil {
		t.Fatal(err)
	}
	wantSummary := "Fixed the naming the audit flagged.\n\n" + shadowHeader + "\n- would have pushed 111111111111.." + fixHead[:12] + " to officer/investigate-delta-infra/5-fix"
	if body.Summary != wantSummary || body.Handoff != nil {
		t.Fatalf("complete summary %q handoff %s, want %q and no handoff", body.Summary, body.Handoff, wantSummary)
	}
	// Not in shadow: the push is admitted (the existing draft rule, now with
	// the fix item's checks passed).
	no := false
	fix2 := r.hq.AddItem(offShepAuth, "investigate.pr_fix", officers.ShepherdSubject(shepRepoAPI, 5))
	// The owner's Feedback route: `feedback` instead of `findings`.
	r.hq.SetPayload(fix2, `{"pr_loop":{"repo":"`+shepRepoAPI+`","pr":5,"head_sha":"`+shepHeadAPI+`"},"feedback":{"text":"rename the flag","by":"owner","at":"2026-11-12T10:00:00Z"},"from":{"officer_id":"`+offShep+`"}}`)
	r.hq.SetItemShadow(fix2, &no)
	r.hq.SetClaim(fix2, r.authInst.Name, time.Now().Add(time.Hour))
	// Two items the rule must NOT read, each in shadow: an ordinary (not
	// *.pr_fix) item for #5 this instance holds, and a fix item for #5 held
	// by ANOTHER instance.
	plain := r.hq.AddItem(offShepAuth, "investigate.opened", officers.ShepherdSubject(shepRepoAPI, 5))
	r.hq.SetItemShadow(plain, &yes)
	r.hq.SetClaim(plain, r.authInst.Name, time.Now().Add(time.Hour))
	other := r.hq.AddItem(offShepAuth, "investigate.pr_fix", officers.ShepherdSubject(shepRepoAPI, 5))
	r.hq.SetItemShadow(other, &yes)
	r.hq.SetClaim(other, "officer-investigate-delta-infra-2", time.Now().Add(time.Hour))
	if out, err := r.authPush(t); err != nil || out == nil {
		t.Fatalf("a fix push out of shadow (an ordinary item and another instance's fix item in shadow beside it): %v", err)
	}
}

// heldFix puts a fix item for #5 in the authoring instance's live hold, out
// of shadow, findings a string, and returns its id.
func (r *shepAPIRig) heldFix(t *testing.T) string {
	t.Helper()
	r.authGH.mu.Lock()
	r.authGH.author = "delta-investigator[bot]"
	r.authGH.mu.Unlock()
	fix := r.hq.AddItem(offShepAuth, "investigate.pr_fix", officers.ShepherdSubject(shepRepoAPI, 5))
	r.hq.SetPayload(fix, `{"pr_loop":{"repo":"`+shepRepoAPI+`","pr":5,"head_sha":"`+shepHeadAPI+`"},"findings":"any text at all, not JSON {[","from":{}}`)
	no := false
	r.hq.SetItemShadow(fix, &no)
	r.hq.SetClaim(fix, r.authInst.Name, time.Now().Add(time.Hour))
	return fix
}

// TestTheFixHandoffGoesThroughOutOfShadow: a fix pushed out of shadow and
// landed is handed on — the complete carries the handoff byte for byte.
func TestTheFixHandoffGoesThroughOutOfShadow(t *testing.T) {
	r := newShepAPIRig(t)
	r.authGH.mu.Lock()
	r.authGH.author = "delta-investigator[bot]"
	r.authGH.mu.Unlock()
	fix := r.hq.AddItem(offShepAuth, "investigate.pr_fix", officers.ShepherdSubject(shepRepoAPI, 5))
	r.hq.SetPayload(fix, `{"pr_loop":{"repo":"`+shepRepoAPI+`","pr":5,"head_sha":"`+shepHeadAPI+`"},"findings":"any text at all, not JSON {[","from":{}}`)
	no := false
	r.hq.SetItemShadow(fix, &no)
	r.hq.SetClaim(fix, r.authInst.Name, time.Now().Add(time.Hour))
	out, err := r.authPush(t)
	if err != nil || out == nil {
		t.Fatalf("the fix push: %v", err)
	}
	if res := r.completeAs(r.authInst.Name, fix, "done", map[string]any{"handoff": map[string]any{"head_sha": fixHead}}); isToolErr(res) {
		t.Fatalf("complete: %s", res)
	}
	if b := r.completes(fix); len(b) != 1 || !strings.Contains(string(b[0]), `"handoff":{"head_sha":"`+fixHead+`"}`) {
		t.Fatalf("the complete sent %s", b)
	}
}

// TestHandoffToTheAuthorIsTheShepherdsOnly: `to: "author"` rides a shepherd's
// complete byte for byte; any other value, or `to` on another officer, is
// refused before HQ; HQ's 409s come back verbatim.
func TestHandoffToTheAuthorIsTheShepherdsOnly(t *testing.T) {
	r := newShepAPIRig(t)
	id := r.hold(t)
	if out := r.completeAs(r.inst.Name, id, "done", map[string]any{"handoff": map[string]any{"head_sha": fixHead, "to": "author"}}); isToolErr(out) {
		t.Fatalf("a shepherd's hand-off to the author: %s", out)
	}
	if b := r.completes(id); len(b) != 1 || !strings.Contains(string(b[0]), `"handoff":{"head_sha":"`+fixHead+`","to":"author"}`) {
		t.Fatalf("the complete sent %s", b)
	}
	id2 := r.hold(t)
	if out := r.completeAs(r.inst.Name, id2, "done", map[string]any{"handoff": map[string]any{"head_sha": fixHead, "to": "auditor"}}); !strings.Contains(out, `handoff.to must be \"author\"`) {
		t.Fatalf("to: auditor: %s", out)
	}
	if n := len(r.completes(id2)); n != 0 {
		t.Fatalf("to: auditor reached HQ %d time(s)", n)
	}
	fix := r.hq.AddItem(offShepAuth, "investigate.pr_fix", officers.ShepherdSubject(shepRepoAPI, 5))
	r.hq.SetClaim(fix, r.authInst.Name, time.Now().Add(time.Hour))
	if out := r.completeAs(r.authInst.Name, fix, "done", map[string]any{"handoff": map[string]any{"head_sha": fixHead, "to": "author"}}); !strings.Contains(out, "handoff.to is a shepherd's only") {
		t.Fatalf("to on a draft officer: %s", out)
	}
	if n := len(r.completes(fix)); n != 0 {
		t.Fatalf("to on a draft officer reached HQ %d time(s)", n)
	}
	r.hq.RefuseCompletes(http.StatusConflict, "audit_not_stopped", "the ladder of #5 has not stopped at "+fixHead[:12])
	out := r.completeAs(r.inst.Name, id2, "done", map[string]any{"handoff": map[string]any{"head_sha": fixHead, "to": "author"}})
	if !strings.Contains(out, `"status":409`) || !strings.Contains(out, `"code":"audit_not_stopped"`) {
		t.Fatalf("HQ's 409 came back as %s", out)
	}
}

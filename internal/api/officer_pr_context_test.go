package api

import (
	"context"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
)

// THE PR CONTEXT READ TOOL (officer_pr_context.go): a web-performance
// officer's JUDGEMENT instance reads one PR's context through HQ's officer PR
// context route (civ-hq web-performance author attribution P3) with the
// officer's own token; no other pool or officer is offered it. Names, paths
// and codes are spelled as literals on purpose: they are the contract.

const prContextTool = "officer_pr_context"

func TestThePRContextToolIsTheWebPerformanceJudgementPoolsOnly(t *testing.T) {
	r := newItemAskRig(t)
	ctx := context.Background()
	offers := func(a agents.Agent) (listed, allowed bool) {
		listed = officerToolNameSet(r.s.MCPToolDefsFor(ctx, a))[prContextTool]
		sess, ok := r.s.OfficerClaudeCodeSession(a)
		if !ok {
			t.Fatalf("%s has no officer session", a.Name)
		}
		for _, m := range sess.AllowMCPTools {
			allowed = allowed || m == prContextTool
		}
		return
	}
	wpJudge := r.addNamed(r.b.Pool(officers.TierJudgement).InstanceName(1), 1201)
	wpRoutine := r.addNamed(r.b.Pool(officers.TierRoutine).InstanceName(1), 1202)
	trJudge := r.addNamed(r.a.Pool(officers.TierJudgement).InstanceName(1), 1203)
	if l, a := offers(wpJudge); !l || !a {
		t.Errorf("the web-performance judgement instance: listed %v, allowed %v; want both", l, a)
	}
	for _, inst := range []agents.Agent{wpRoutine, trJudge} {
		if l, a := offers(inst); l || a {
			t.Errorf("%s: listed %v, allowed %v; want neither", inst.Name, l, a)
		}
	}
	// called anyway: refused at dispatch, and HQ never sees it
	for _, c := range []struct {
		who string
		a   agents.Agent
	}{{"the web-performance routine instance", wpRoutine}, {"the task reconciler", trJudge},
		{"an ordinary agent", r.agent(t, officerOrdinaryID)}, {"the chief", r.agent(t, officerChiefID)}} {
		r.hq.ResetRequests()
		out := r.s.dispatchAgentTool(ctx, c.a, prContextTool, `{"item_id":"1","pr":7251}`)
		if !isToolErr(out) {
			t.Errorf("%s calling %s: %s, want a refusal", c.who, prContextTool, out)
		}
		if n := len(r.hq.Requests()); n != 0 {
			t.Errorf("%s's refused read reached HQ (%d requests)", c.who, n)
		}
	}
}

// TestThePRContextToolReadsHQWithTheOfficersOwnToken: exactly one GET to
// …/prs/{pr}/context?item={item}, authorised with the OFFICER's token, HQ's
// answer passed back byte for byte.
func TestThePRContextToolReadsHQWithTheOfficersOwnToken(t *testing.T) {
	r := newItemAskRig(t)
	ctx := context.Background()
	inst := r.addInstance(r.b, 1, 1211)
	r.hq.ResetRequests()
	out := r.s.dispatchAgentTool(ctx, inst, prContextTool, `{"item_id":"88","pr":7251}`)
	if want := `{"item":"88","pr":7251,"pr_key":"acme/widgets#7251","use":"attribution evidence only"}` + "\n"; out != want {
		t.Fatalf("read:\n got %q\nwant %q", out, want)
	}
	reqs := r.hq.Requests()
	if len(reqs) != 1 {
		t.Fatalf("HQ saw %d requests, want 1: %+v", len(reqs), reqs)
	}
	if q := reqs[0]; q.Method != "GET" || q.Path != "/api/v1/officers/"+offWP+"/prs/7251/context" || q.Query != "item=88" || q.Auth != "Bearer "+offWPTok {
		t.Errorf("read went %s %s ?%s with %q", q.Method, q.Path, q.Query, q.Auth)
	}
}

func TestThePRContextToolRefusesLocallyWhatMusterOwns(t *testing.T) {
	r := newItemAskRig(t)
	ctx := context.Background()
	inst := r.addInstance(r.b, 1, 1221)
	for _, args := range []string{
		`{"item_id":"88"}`,
		`{"item_id":"88","pr":"7251"}`,
		`{"item_id":"88","pr":0}`,
		`{"pr":7251}`,
		`{"item_id":"../x","pr":7251}`,
		`{"item_id":"88","pr":7251,"repo":"acme/widgets"}`,
		`{"item_id":"88","pr":7251,"officer_id":"release:alpha-web"}`,
	} {
		r.hq.ResetRequests()
		out := r.s.dispatchAgentTool(ctx, inst, prContextTool, args)
		if !isToolErr(out) {
			t.Errorf("%s: %s, want a refusal", args, out)
		}
		if n := len(r.hq.Requests()); n != 0 {
			t.Errorf("%s reached HQ (%d requests)", args, n)
		}
	}
}

// TestThePRContextToolPassesHQsFinalRefusalsBack: a PR outside the range is
// HQ's 404 pr_not_visible; a type without the policy its 403
// pr_context_read_off; an HQ without the route is pr_context_route_unavailable
// — each final, never retryable, and the route's absence never reads as
// "not in range".
func TestThePRContextToolPassesHQsFinalRefusalsBack(t *testing.T) {
	r := newItemAskRig(t)
	ctx := context.Background()
	inst := r.addInstance(r.b, 1, 1231)
	out := r.s.dispatchAgentTool(ctx, inst, prContextTool, `{"item_id":"88","pr":9999}`)
	if st, code := obsRefusal(out); st != 404 || code != "pr_not_visible" || strings.Contains(out, "retryable") {
		t.Errorf("a PR outside the range: %s, want a final refused 404 pr_not_visible", out)
	}
	r.hq.PRContextReadOff = true
	out = r.s.dispatchAgentTool(ctx, inst, prContextTool, `{"item_id":"88","pr":7251}`)
	if st, code := obsRefusal(out); st != 403 || code != "pr_context_read_off" || strings.Contains(out, "retryable") {
		t.Errorf("pr_context_read off: %s, want a final refused 403 pr_context_read_off", out)
	}
	r.hq.PRContextReadOff, r.hq.PRContextAbsent = false, true
	out = r.s.dispatchAgentTool(ctx, inst, prContextTool, `{"item_id":"88","pr":7251}`)
	st, code := refusedCode(out)
	if !isToolErr(out) || st != 404 || code != "pr_context_route_unavailable" || strings.Contains(out, "retryable") || strings.Contains(out, "pr_not_visible") {
		t.Errorf("an HQ without the route: %s, want a final refused 404 pr_context_route_unavailable", out)
	}
}

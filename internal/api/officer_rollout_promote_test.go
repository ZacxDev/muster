package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// THE ROLLOUT PROPOSAL TOOL (officer_item_asks.go): officer_rollout_promote_propose
// (civ-hq #887, docs/OFFICERS.md §5r), a rollout officer's — every pool. ONE
// POST …/items/{item}/rollout-promotions with the officer's own token, which
// muster holds and the instance never sees; HQ re-reads the canary, asks the
// approvers and — on an approval, never the officer — dispatches the
// promotion with its own credentials. Names, paths and codes are spelled as
// literals on purpose: they are the contract.
// ---------------------------------------------------------------------------

const (
	rolloutTool     = "officer_rollout_promote_propose"
	offRO           = "rollout:example"
	offROTok        = "fixture-officer-token-rollout-3b9e"
	kindSweep       = "rollout.sweep"
	wantRolloutNote = "NOTHING has been promoted, tagged or deployed"
	rolloutEvidence = `{"summary":"24.5 h on the canary, every check clean","checks":[` +
		`{"name":"restart control","query":"count(kube_pod_container_status_restarts_total)","result":"52"},` +
		`{"name":"restarts","query":"sum(increase(kube_pod_container_status_restarts_total[24h]))","result":"0"}]}`
)

// rolloutArgs is a well-formed proposal on item.
func rolloutArgs(item string) string {
	return `{"item_id":"` + item + `","repo":"example-org/fleet-controller","preview_tag":"v2.40.0-preview.2","evidence":` + rolloutEvidence + `}`
}

// newRolloutRig: a TIERED rollout officer (r.a) beside a tiered
// web-performance officer (r.b), both on the fake HQ. The web-performance
// officer's token is known to the fake too, so a request carrying it would
// be ACCEPTED as that officer — which the token assertion would catch.
func newRolloutRig(t *testing.T) *officerRig {
	t.Helper()
	return newOfficerRigCfg(t, map[string]string{offROTok: offRO, offWPTok: offWP}, func(hqURL string) []officers.Officer {
		tiers := `"tiers":{"routine":{"model":"haiku","max_instances":1},"judgement":{"model":"opus","max_instances":1}},"transcript":{"enabled":true}`
		raw := `[{"id":"` + offRO + `","hq_url":"` + hqURL + `",` + tiers + `},{"id":"` + offWP + `","hq_url":"` + hqURL + `",` + tiers + `}]`
		list, err := officers.Parse(raw, func(k string) string {
			return map[string]string{
				officers.TokenEnvPrefix + officers.TokenEnvSuffix(offRO): offROTok,
				officers.TokenEnvPrefix + officers.TokenEnvSuffix(offWP): offWPTok,
			}[k]
		}, "muster-agent-")
		if err != nil {
			t.Fatalf("parse the rollout rig's officers: %v", err)
		}
		if len(list) != 2 || list[0].ID != offRO || list[1].ID != offWP {
			t.Fatalf("the rollout rig's officers: %+v", list)
		}
		return list
	}, func(c *OfficersConfig) {
		c.TranscriptSource = emptyTranscripts{}
		c.NoTranscriptRun = true
	})
}

// heldSweep adds a ROUTINE rollout.sweep item (HQ's rollout policy tiers the
// sweep routine) and has the routine instance inst claim it.
func (r *officerRig) heldSweep(t *testing.T, inst agents.Agent) string {
	t.Helper()
	item := r.hq.AddTieredItem(offRO, kindSweep, "sched:rollout-sweep:fixture", officers.TierRoutine)
	if out := r.s.dispatchAgentTool(context.Background(), inst, officers.ToolClaim, `{"item_id":"`+item+`"}`); isToolErr(out) {
		t.Fatalf("claim: %s", out)
	}
	r.hq.ResetRequests()
	return item
}

// TestTheRolloutToolIsTheRolloutOfficersEveryPool: both pools of the rollout
// officer list and pre-allow officer_rollout_promote_propose (HQ tiers the
// sweep that proposes routine); the web-performance officer's pools do not.
// Called anyway — by the web-performance officer, an ordinary agent or the
// chief — it is refused at dispatch and HQ never sees it.
func TestTheRolloutToolIsTheRolloutOfficersEveryPool(t *testing.T) {
	r := newRolloutRig(t)
	ctx := context.Background()
	offers := func(a agents.Agent) (listed, allowed bool) {
		listed = officerToolNameSet(r.s.MCPToolDefsFor(ctx, a))[rolloutTool]
		sess, ok := r.s.OfficerClaudeCodeSession(a)
		if !ok {
			t.Fatalf("%s has no officer session", a.Name)
		}
		for _, m := range sess.AllowMCPTools {
			allowed = allowed || m == rolloutTool
		}
		return
	}
	roRoutine := r.addNamed(r.a.Pool(officers.TierRoutine).InstanceName(1), 1401)
	roJudge := r.addNamed(r.a.Pool(officers.TierJudgement).InstanceName(1), 1402)
	wpRoutine := r.addNamed(r.b.Pool(officers.TierRoutine).InstanceName(1), 1403)
	wpJudge := r.addNamed(r.b.Pool(officers.TierJudgement).InstanceName(1), 1404)
	if o, _, ok := r.s.officerOf(roRoutine); !ok || o.Tier != officers.TierRoutine {
		t.Fatalf("%s does not resolve to the routine pool", roRoutine.Name)
	}
	for _, inst := range []agents.Agent{roRoutine, roJudge} {
		if l, a := offers(inst); !l || !a {
			t.Errorf("%s: %s listed %v, allowed %v; want both", inst.Name, rolloutTool, l, a)
		}
	}
	for _, inst := range []agents.Agent{wpRoutine, wpJudge} {
		if l, a := offers(inst); l || a {
			t.Errorf("%s: %s listed %v, allowed %v; want neither", inst.Name, rolloutTool, l, a)
		}
	}
	for _, c := range []struct {
		who string
		a   agents.Agent
	}{
		{"the web-performance judgement instance", wpJudge},
		{"an ordinary agent", r.agent(t, officerOrdinaryID)},
		{"the chief", r.agent(t, officerChiefID)},
	} {
		r.hq.ResetRequests()
		out := r.s.dispatchAgentTool(ctx, c.a, rolloutTool, rolloutArgs("1"))
		if !isToolErr(out) {
			t.Errorf("%s calling %s: %s, want a refusal", c.who, rolloutTool, out)
		}
		if n := len(r.hq.Requests()); n != 0 {
			t.Errorf("%s's refused %s reached HQ (%d requests)", c.who, rolloutTool, n)
		}
	}
}

// TestARolloutProposalSendsTheContractBodyWithTheOfficersToken: exactly one
// POST to …/items/{item}/rollout-promotions, authorised with the OFFICER's
// token, its body exactly {instance, repo, preview_tag, evidence} — instance
// the calling ROUTINE instance's own name, the evidence object as given —
// and HQ's answer passed back with the note that nothing is promoted yet and
// the lease has ended; muster stops tracking the item.
func TestARolloutProposalSendsTheContractBodyWithTheOfficersToken(t *testing.T) {
	r := newRolloutRig(t)
	ctx := context.Background()
	inst := r.addNamed(r.a.Pool(officers.TierRoutine).InstanceName(1), 1411)
	item := r.heldSweep(t, inst)
	if !r.tracked(inst, item) {
		t.Fatal("control: muster does not track the claimed item, so the lease-ended assertion below could not see a change")
	}
	out := r.s.dispatchAgentTool(ctx, inst, rolloutTool, rolloutArgs(item))
	if isToolErr(out) {
		t.Fatalf("proposal: %s", out)
	}
	reqs := r.hq.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d HQ requests, want 1: %+v", len(reqs), reqs)
	}
	q := reqs[0]
	if q.Method != http.MethodPost || q.Path != "/api/v1/officers/rollout:example/items/"+item+"/rollout-promotions" || q.Auth != "Bearer "+offROTok {
		t.Fatalf("went %s %s with %q", q.Method, q.Path, q.Auth)
	}
	var ev map[string]any
	if err := json.Unmarshal([]byte(rolloutEvidence), &ev); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"instance": inst.Name, "repo": "example-org/fleet-controller", "preview_tag": "v2.40.0-preview.2", "evidence": ev}
	if got, w := normJSON(t, q.Body), normJSON(t, want); got != w {
		t.Fatalf("body:\n%s\nwant\n%s", got, w)
	}
	var res struct {
		Promotion map[string]any   `json:"rollout_promotion"`
		Asks      []map[string]any `json:"asks"`
		Item      map[string]any   `json:"item"`
		Note      string           `json:"note"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("not JSON: %s", out)
	}
	if res.Promotion["state"] != "pending" || res.Promotion["preview_tag"] != "v2.40.0-preview.2" || len(res.Asks) != 1 || res.Item["status"] != "waiting" {
		t.Errorf("result does not carry HQ's answer: %s", out)
	}
	for _, s := range []string{wantRolloutNote, "ENDED", "do not heartbeat, complete or release", "never say the tag was promoted"} {
		if !strings.Contains(res.Note, s) {
			t.Errorf("note does not say %q: %s", s, res.Note)
		}
	}
	if r.tracked(inst, item) {
		t.Error("muster still tracks the item after a successful proposal (the lease ended)")
	}
}

// TestTheRolloutToolRefusesLocallyWhatMusterOwns: muster builds the body, so
// an argument it does not know (a workflow, a stable tag, a token, another
// officer) or a missing or wrongly typed one is refused before HQ is called —
// never dropped.
func TestTheRolloutToolRefusesLocallyWhatMusterOwns(t *testing.T) {
	r := newRolloutRig(t)
	ctx := context.Background()
	inst := r.addNamed(r.a.Pool(officers.TierRoutine).InstanceName(1), 1421)
	ok := `"repo":"example-org/fleet-controller","preview_tag":"v2.40.0-preview.2","evidence":` + rolloutEvidence
	for _, args := range []string{
		`{}`,
		`{` + ok + `}`,
		`{"item_id":"",` + ok + `}`,
		`{"item_id":"1/../2",` + ok + `}`,
		`{"item_id":"1","preview_tag":"v2.40.0-preview.2","evidence":` + rolloutEvidence + `}`,
		`{"item_id":"1","repo":"","preview_tag":"v2.40.0-preview.2","evidence":` + rolloutEvidence + `}`,
		`{"item_id":"1","repo":7,"preview_tag":"v2.40.0-preview.2","evidence":` + rolloutEvidence + `}`,
		`{"item_id":"1","repo":"example-org/fleet-controller","evidence":` + rolloutEvidence + `}`,
		`{"item_id":"1","repo":"example-org/fleet-controller","preview_tag":["v2.40.0-preview.2"],"evidence":` + rolloutEvidence + `}`,
		`{"item_id":"1","repo":"example-org/fleet-controller","preview_tag":"v2.40.0-preview.2"}`,
		`{"item_id":"1","repo":"example-org/fleet-controller","preview_tag":"v2.40.0-preview.2","evidence":"clean"}`,
		`{"item_id":"1","repo":"example-org/fleet-controller","preview_tag":"v2.40.0-preview.2","evidence":[{"summary":"x"}]}`,
		`{"item_id":"1","repo":"example-org/fleet-controller","preview_tag":"v2.40.0-preview.2","evidence":null}`,
		`{"item_id":"1",` + ok + `,"workflow":"promote.yml"}`,
		`{"item_id":"1",` + ok + `,"stable_tag":"v2.40.0"}`,
		`{"item_id":"1",` + ok + `,"token":"x"}`,
		`{"item_id":"1",` + ok + `,"officer_id":"web-performance:alpha-web"}`,
	} {
		r.hq.ResetRequests()
		out := r.s.dispatchAgentTool(ctx, inst, rolloutTool, args)
		if !isToolErr(out) {
			t.Errorf("%s %s: %s, want a refusal", rolloutTool, args, out)
		}
		if n := len(r.hq.Requests()); n != 0 {
			t.Errorf("%s %s reached HQ (%d requests)", rolloutTool, args, n)
		}
	}
	// Positive control: the same instance with well-formed arguments does
	// reach HQ, so the zeros above are the parser's, not a dead rig's.
	item := r.heldSweep(t, inst)
	if out := r.s.dispatchAgentTool(ctx, inst, rolloutTool, rolloutArgs(item)); isToolErr(out) || len(r.hq.Requests()) != 1 {
		t.Fatalf("control: a well-formed proposal: %s (%d HQ requests)", out, len(r.hq.Requests()))
	}
}

// TestEveryRolloutRefusalReachesTheModelVerbatim: a representative HQ 4xx of
// each family (§5r) and the route's 503 rollout_actuator_not_configured (a
// 5xx that is a decision) each come back as a FINAL refusal with HQ's status,
// its code and HQ's body under refused.hq; the route's 502
// github_unavailable comes back as RETRYABLE (HQ wrote nothing); an outage
// as "unavailable". In none of them does muster end its own tracking of the
// item.
func TestEveryRolloutRefusalReachesTheModelVerbatim(t *testing.T) {
	r := newRolloutRig(t)
	ctx := context.Background()
	inst := r.addNamed(r.a.Pool(officers.TierRoutine).InstanceName(1), 1431)
	item := r.heldSweep(t, inst)
	for _, c := range []struct {
		status int
		code   string
	}{
		{400, "invalid"},
		{403, "not_allowed"},
		{409, "promotion_pending"},
		{409, "officer_shadow"},
		{422, "soak_too_short"},
		{503, "rollout_actuator_not_configured"},
	} {
		msg := "fixture refusal " + c.code + " — exact words"
		r.hq.RefuseVerb("rollout-promotions", c.status, c.code, msg)
		out := r.s.dispatchAgentTool(ctx, inst, rolloutTool, rolloutArgs(item))
		r.hq.RefuseVerb("rollout-promotions", 0, "", "")
		st, code, hq, text := refusedHQ(t, out)
		if !isToolErr(out) || st != c.status || code != c.code || strings.Contains(out, "retryable") || strings.Contains(out, "unavailable") {
			t.Errorf("%d %s: %s, want a final refusal %d %s", c.status, c.code, out, c.status, c.code)
			continue
		}
		if hq["error"] != msg || hq["code"] != c.code {
			t.Errorf("%s: HQ's body not passed verbatim: %v", c.code, hq)
		}
		if want := "HQ refused " + rolloutTool + " (" + strconv.Itoa(c.status) + " " + c.code + "): " + msg; text != want {
			t.Errorf("%s: error %q, want %q", c.code, text, want)
		}
		if !r.tracked(inst, item) {
			t.Errorf("%s: muster stopped tracking the item on a refusal", c.code)
		}
	}

	r.hq.RefuseVerb("rollout-promotions", http.StatusBadGateway, "github_unavailable", "HQ could not read GitHub")
	out := r.s.dispatchAgentTool(ctx, inst, rolloutTool, rolloutArgs(item))
	r.hq.RefuseVerb("rollout-promotions", 0, "", "")
	var again struct {
		Error     string `json:"error"`
		Retryable struct {
			Status int    `json:"status"`
			Code   string `json:"code"`
		} `json:"retryable"`
	}
	if err := json.Unmarshal([]byte(out), &again); err != nil || !isToolErr(out) || again.Retryable.Status != 502 || again.Retryable.Code != "github_unavailable" || strings.Contains(out, `"refused"`) {
		t.Errorf("502 github_unavailable: %s, want a retryable answer carrying HQ's code", out)
	}
	if !r.tracked(inst, item) {
		t.Error("502 github_unavailable: muster stopped tracking the item")
	}

	r.hq.SetDown(true)
	out = r.s.dispatchAgentTool(ctx, inst, rolloutTool, rolloutArgs(item))
	r.hq.SetDown(false)
	if !isToolErr(out) || !strings.Contains(out, "unavailable") || strings.Contains(out, "refused") {
		t.Errorf("HQ down: %s, want unavailable", out)
	}
	if !r.tracked(inst, item) {
		t.Error("HQ down: muster stopped tracking the item")
	}
}

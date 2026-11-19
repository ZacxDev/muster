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
// THE ROLLOUT EVIDENCE TOOL (officer_rollout_evidence.go):
// officer_rollout_evidence_attach (civ-hq docs/OFFICERS.md §5r, "The
// evidence"), a rollout officer's ROUTINE pool (or an untiered one). ONE
// POST …/items/{item}/rollout-evidence with the officer's own token, which
// muster holds and the instance never sees; HQ shows the evidence to the
// person who starts a rollout. It does not end the lease and starts nothing.
// It replaces officer_rollout_promote_propose, whose HQ route is retired
// (410 officer_propose_retired). Names, paths and codes are spelled as
// literals on purpose: they are the contract.
// ---------------------------------------------------------------------------

const (
	evidenceTool     = "officer_rollout_evidence_attach"
	retiredRollout   = "officer_rollout_promote_propose"
	offRO            = "rollout:example"
	offROTok         = "fixture-officer-token-rollout-3b9e"
	kindSweep        = "rollout.sweep"
	wantEvidenceNote = "NOTHING has been started, promoted, tagged or deployed"
	rolloutEvidence  = `{"summary":"clean: 24.5 h on the canary, every check clean","checks":[` +
		`{"name":"restart control","query":"count(kube_pod_container_status_restarts_total)","result":"52"},` +
		`{"name":"restarts","query":"sum(increase(kube_pod_container_status_restarts_total[24h]))","result":"0"}]}`
)

// evidenceArgs is a well-formed attach on item.
func evidenceArgs(item string) string {
	return `{"item_id":"` + item + `","repo":"example-org/fleet-controller","tag":"v2.40.0-preview.2","evidence":` + rolloutEvidence + `}`
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

// rolloutOffers reports whether instance a lists tool over MCP and its
// session pre-allows it.
func (r *officerRig) rolloutOffers(t *testing.T, a agents.Agent, tool string) (listed, allowed bool) {
	t.Helper()
	listed = officerToolNameSet(r.s.MCPToolDefsFor(context.Background(), a))[tool]
	sess, ok := r.s.OfficerClaudeCodeSession(a)
	if !ok {
		t.Fatalf("%s has no officer session", a.Name)
	}
	for _, m := range sess.AllowMCPTools {
		allowed = allowed || m == tool
	}
	return
}

// TestTheEvidenceToolIsTheRolloutOfficersRoutinePoolOnly: the rollout
// officer's ROUTINE pool lists and pre-allows officer_rollout_evidence_attach
// (HQ tiers the sweep that attaches routine); its JUDGEMENT pool does not,
// nor do the web-performance officer's pools. Called anyway — by the rollout
// judgement instance, the web-performance officer, an ordinary agent or the
// chief — it is refused at dispatch and HQ never sees it.
func TestTheEvidenceToolIsTheRolloutOfficersRoutinePoolOnly(t *testing.T) {
	r := newRolloutRig(t)
	ctx := context.Background()
	roRoutine := r.addNamed(r.a.Pool(officers.TierRoutine).InstanceName(1), 1401)
	roJudge := r.addNamed(r.a.Pool(officers.TierJudgement).InstanceName(1), 1402)
	wpRoutine := r.addNamed(r.b.Pool(officers.TierRoutine).InstanceName(1), 1403)
	wpJudge := r.addNamed(r.b.Pool(officers.TierJudgement).InstanceName(1), 1404)
	if o, _, ok := r.s.officerOf(roRoutine); !ok || o.Tier != officers.TierRoutine {
		t.Fatalf("%s does not resolve to the routine pool", roRoutine.Name)
	}
	if l, a := r.rolloutOffers(t, roRoutine, evidenceTool); !l || !a {
		t.Errorf("%s: %s listed %v, allowed %v; want both", roRoutine.Name, evidenceTool, l, a)
	}
	for _, inst := range []agents.Agent{roJudge, wpRoutine, wpJudge} {
		if l, a := r.rolloutOffers(t, inst, evidenceTool); l || a {
			t.Errorf("%s: %s listed %v, allowed %v; want neither", inst.Name, evidenceTool, l, a)
		}
	}
	for _, c := range []struct {
		who string
		a   agents.Agent
	}{
		{"the rollout judgement instance", roJudge},
		{"the web-performance judgement instance", wpJudge},
		{"an ordinary agent", r.agent(t, officerOrdinaryID)},
		{"the chief", r.agent(t, officerChiefID)},
	} {
		r.hq.ResetRequests()
		out := r.s.dispatchAgentTool(ctx, c.a, evidenceTool, evidenceArgs("1"))
		if !isToolErr(out) {
			t.Errorf("%s calling %s: %s, want a refusal", c.who, evidenceTool, out)
		}
		if n := len(r.hq.Requests()); n != 0 {
			t.Errorf("%s's refused %s reached HQ (%d requests)", c.who, evidenceTool, n)
		}
	}
}

// TestTheRetiredProposalIsOfferedToNoRolloutPool: officer_rollout_promote_propose
// is gone — neither rollout pool lists or pre-allows it, and the routine
// instance calling it (with the old, well-formed arguments, on an item it
// holds) is refused at dispatch without reaching HQ, whose route would
// answer 410 officer_propose_retired.
func TestTheRetiredProposalIsOfferedToNoRolloutPool(t *testing.T) {
	r := newRolloutRig(t)
	ctx := context.Background()
	roRoutine := r.addNamed(r.a.Pool(officers.TierRoutine).InstanceName(1), 1441)
	roJudge := r.addNamed(r.a.Pool(officers.TierJudgement).InstanceName(1), 1442)
	for _, inst := range []agents.Agent{roRoutine, roJudge} {
		if l, a := r.rolloutOffers(t, inst, retiredRollout); l || a {
			t.Errorf("%s: %s listed %v, allowed %v; want neither", inst.Name, retiredRollout, l, a)
		}
	}
	item := r.heldSweep(t, roRoutine)
	old := `{"item_id":"` + item + `","repo":"example-org/fleet-controller","preview_tag":"v2.40.0-preview.2","evidence":` + rolloutEvidence + `}`
	out := r.s.dispatchAgentTool(ctx, roRoutine, retiredRollout, old)
	if !isToolErr(out) {
		t.Errorf("the routine instance calling %s: %s, want a refusal", retiredRollout, out)
	}
	if n := len(r.hq.Requests()); n != 0 {
		t.Errorf("the retired %s reached HQ (%d requests)", retiredRollout, n)
	}
	// Positive control: the same instance on the same item does reach HQ
	// with the new tool, so the zero above is the retirement's.
	if out := r.s.dispatchAgentTool(ctx, roRoutine, evidenceTool, evidenceArgs(item)); isToolErr(out) || len(r.hq.Requests()) != 1 {
		t.Fatalf("control: a well-formed attach: %s (%d HQ requests)", out, len(r.hq.Requests()))
	}
}

// TestAnEvidenceAttachSendsTheContractBodyWithTheOfficersToken: exactly one
// POST to …/items/{item}/rollout-evidence, authorised with the OFFICER's
// token, its body exactly {instance, repo, tag, evidence} — instance the
// calling ROUTINE instance's own name, the evidence object as given — and
// HQ's answer passed back with the note that nothing started and the lease
// goes on; muster keeps tracking the item, HQ still has it claimed by the
// instance, and a second attach on it is accepted too.
func TestAnEvidenceAttachSendsTheContractBodyWithTheOfficersToken(t *testing.T) {
	r := newRolloutRig(t)
	ctx := context.Background()
	inst := r.addNamed(r.a.Pool(officers.TierRoutine).InstanceName(1), 1411)
	item := r.heldSweep(t, inst)
	out := r.s.dispatchAgentTool(ctx, inst, evidenceTool, evidenceArgs(item))
	if isToolErr(out) {
		t.Fatalf("attach: %s", out)
	}
	reqs := r.hq.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d HQ requests, want 1: %+v", len(reqs), reqs)
	}
	q := reqs[0]
	if q.Method != http.MethodPost || q.Path != "/api/v1/officers/rollout:example/items/"+item+"/rollout-evidence" || q.Auth != "Bearer "+offROTok {
		t.Fatalf("went %s %s with %q", q.Method, q.Path, q.Auth)
	}
	var ev map[string]any
	if err := json.Unmarshal([]byte(rolloutEvidence), &ev); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"instance": inst.Name, "repo": "example-org/fleet-controller", "tag": "v2.40.0-preview.2", "evidence": ev}
	if got, w := normJSON(t, q.Body), normJSON(t, want); got != w {
		t.Fatalf("body:\n%s\nwant\n%s", got, w)
	}
	if got := r.hq.RolloutEvidenceAttached(); len(got) != 1 || got[0].Officer != offRO || got[0].Item != item {
		t.Fatalf("HQ recorded %+v, want one attach by %s on %s", got, offRO, item)
	}
	var res struct {
		Evidence map[string]any `json:"evidence"`
		Note     string         `json:"note"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("not JSON: %s", out)
	}
	if res.Evidence["tag"] != "v2.40.0-preview.2" || res.Evidence["instance"] != inst.Name || res.Evidence["item_id"] != item {
		t.Errorf("result does not carry HQ's answer: %s", out)
	}
	for _, s := range []string{wantEvidenceNote, "lease on this item is UNCHANGED", "complete it as usual", "never say a rollout started"} {
		if !strings.Contains(res.Note, s) {
			t.Errorf("note does not say %q: %s", s, res.Note)
		}
	}
	if !r.tracked(inst, item) {
		t.Error("muster stopped tracking the item after an attach (an attach does not end the lease)")
	}
	if it := r.hq.Item(item); it.Status != "claimed" || it.ClaimedBy != inst.Name {
		t.Errorf("HQ's item after the attach: %+v, want still claimed by %s", it, inst.Name)
	}
	if out := r.s.dispatchAgentTool(ctx, inst, evidenceTool, evidenceArgs(item)); isToolErr(out) || len(r.hq.RolloutEvidenceAttached()) != 2 {
		t.Errorf("a second attach on the same held item: %s (%d attached)", out, len(r.hq.RolloutEvidenceAttached()))
	}
}

// TestTheEvidenceToolRefusesLocallyWhatMusterOwns: muster builds the body,
// so an argument it does not know (the retired preview_tag, a workflow, a
// token, another officer) or a missing or wrongly typed one is refused
// before HQ is called — never dropped.
func TestTheEvidenceToolRefusesLocallyWhatMusterOwns(t *testing.T) {
	r := newRolloutRig(t)
	ctx := context.Background()
	inst := r.addNamed(r.a.Pool(officers.TierRoutine).InstanceName(1), 1421)
	ok := `"repo":"example-org/fleet-controller","tag":"v2.40.0-preview.2","evidence":` + rolloutEvidence
	for _, args := range []string{
		`{}`,
		`{` + ok + `}`,
		`{"item_id":"",` + ok + `}`,
		`{"item_id":"1/../2",` + ok + `}`,
		`{"item_id":"1","tag":"v2.40.0-preview.2","evidence":` + rolloutEvidence + `}`,
		`{"item_id":"1","repo":"","tag":"v2.40.0-preview.2","evidence":` + rolloutEvidence + `}`,
		`{"item_id":"1","repo":7,"tag":"v2.40.0-preview.2","evidence":` + rolloutEvidence + `}`,
		`{"item_id":"1","repo":"example-org/fleet-controller","evidence":` + rolloutEvidence + `}`,
		`{"item_id":"1","repo":"example-org/fleet-controller","tag":"","evidence":` + rolloutEvidence + `}`,
		`{"item_id":"1","repo":"example-org/fleet-controller","tag":["v2.40.0-preview.2"],"evidence":` + rolloutEvidence + `}`,
		`{"item_id":"1","repo":"example-org/fleet-controller","preview_tag":"v2.40.0-preview.2","evidence":` + rolloutEvidence + `}`,
		`{"item_id":"1","repo":"example-org/fleet-controller","tag":"v2.40.0-preview.2"}`,
		`{"item_id":"1","repo":"example-org/fleet-controller","tag":"v2.40.0-preview.2","evidence":"clean"}`,
		`{"item_id":"1","repo":"example-org/fleet-controller","tag":"v2.40.0-preview.2","evidence":[{"summary":"x"}]}`,
		`{"item_id":"1","repo":"example-org/fleet-controller","tag":"v2.40.0-preview.2","evidence":null}`,
		`{"item_id":"1",` + ok + `,"preview_tag":"v2.40.0-preview.2"}`,
		`{"item_id":"1",` + ok + `,"workflow":"promote.yml"}`,
		`{"item_id":"1",` + ok + `,"token":"x"}`,
		`{"item_id":"1",` + ok + `,"officer_id":"web-performance:alpha-web"}`,
	} {
		r.hq.ResetRequests()
		out := r.s.dispatchAgentTool(ctx, inst, evidenceTool, args)
		if !isToolErr(out) {
			t.Errorf("%s %s: %s, want a refusal", evidenceTool, args, out)
		}
		if n := len(r.hq.Requests()); n != 0 {
			t.Errorf("%s %s reached HQ (%d requests)", evidenceTool, args, n)
		}
	}
	// Positive control: the same instance with well-formed arguments does
	// reach HQ, so the zeros above are the parser's, not a dead rig's.
	item := r.heldSweep(t, inst)
	if out := r.s.dispatchAgentTool(ctx, inst, evidenceTool, evidenceArgs(item)); isToolErr(out) || len(r.hq.Requests()) != 1 {
		t.Fatalf("control: a well-formed attach: %s (%d HQ requests)", out, len(r.hq.Requests()))
	}
}

// TestEveryEvidenceRefusalReachesTheModelVerbatim: each HQ refusal the route
// documents (§5r: 400 invalid, 403 not_allowed, 404, 409 lease_lost /
// officer_disabled / rollout_actuator_off / rollout_policy_invalid) comes
// back as a FINAL refusal with HQ's status, its code, its message and HQ's
// body under refused.hq; an outage as "unavailable". A real lease_lost (the
// instance does not hold the item) passes through the same way. In none of
// them does muster end its own tracking of the item.
func TestEveryEvidenceRefusalReachesTheModelVerbatim(t *testing.T) {
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
		{404, "not_found"},
		{409, "lease_lost"},
		{409, "officer_disabled"},
		{409, "rollout_actuator_off"},
		{409, "rollout_policy_invalid"},
	} {
		msg := "fixture refusal " + c.code + " — exact words"
		r.hq.RefuseVerb("rollout-evidence", c.status, c.code, msg)
		out := r.s.dispatchAgentTool(ctx, inst, evidenceTool, evidenceArgs(item))
		r.hq.RefuseVerb("rollout-evidence", 0, "", "")
		st, code, hq, text := refusedHQ(t, out)
		if !isToolErr(out) || st != c.status || code != c.code || strings.Contains(out, "retryable") || strings.Contains(out, "unavailable") {
			t.Errorf("%d %s: %s, want a final refusal %d %s", c.status, c.code, out, c.status, c.code)
			continue
		}
		if hq["error"] != msg || hq["code"] != c.code {
			t.Errorf("%s: HQ's body not passed verbatim: %v", c.code, hq)
		}
		if want := "HQ refused " + evidenceTool + " (" + strconv.Itoa(c.status) + " " + c.code + "): " + msg; text != want {
			t.Errorf("%s: error %q, want %q", c.code, text, want)
		}
		if !r.tracked(inst, item) {
			t.Errorf("%s: muster stopped tracking the item on a refusal", c.code)
		}
	}
	if n := len(r.hq.RolloutEvidenceAttached()); n != 0 {
		t.Errorf("HQ recorded %d attaches while every call was refused", n)
	}

	// The fake's own lease check: an item this instance does not hold.
	other := r.hq.AddTieredItem(offRO, kindSweep, "sched:rollout-sweep:other", officers.TierRoutine)
	out := r.s.dispatchAgentTool(ctx, inst, evidenceTool, evidenceArgs(other))
	if st, code, _, _ := refusedHQ(t, out); st != http.StatusConflict || code != "lease_lost" {
		t.Errorf("attach on an item not held: %s, want 409 lease_lost", out)
	}

	r.hq.SetDown(true)
	out = r.s.dispatchAgentTool(ctx, inst, evidenceTool, evidenceArgs(item))
	r.hq.SetDown(false)
	if !isToolErr(out) || !strings.Contains(out, "unavailable") || strings.Contains(out, "refused") {
		t.Errorf("HQ down: %s, want unavailable", out)
	}
	if !r.tracked(inst, item) {
		t.Error("HQ down: muster stopped tracking the item")
	}
}

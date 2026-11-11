package api

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
)

// THE GAP TASK TOOL (officer_gap_tasks.go): a datastore officer's instance —
// either pool — has HQ file ONE task per (store, kind) through HQ's officer
// gap-tasks route (civ-hq docs/OFFICERS.md §5o) with the officer's own token;
// no other officer is offered it. Names, paths and codes are literals on
// purpose: they are the contract.

const (
	gapTaskTool = "officer_gap_task"
	offDS       = "datastore:dp-9"
	offDSTok    = "fixture-officer-token-datastore-7c2e"
	kindAudit   = "datastore.audit"
)

// newGapTaskRig: a tiered datastore officer (r.a) beside the task
// reconciler (r.b), both on the fake HQ.
func newGapTaskRig(t *testing.T) *officerRig {
	t.Helper()
	return newOfficerRigCfg(t, map[string]string{offDSTok: offDS, offTRTok: offTR}, func(hqURL string) []officers.Officer {
		tiers := `"tiers":{"routine":{"model":"haiku","max_instances":1},"judgement":{"model":"opus","max_instances":1}}`
		raw := `[{"id":"` + offDS + `","hq_url":"` + hqURL + `",` + tiers + `},{"id":"` + offTR + `","hq_url":"` + hqURL + `",` + tiers + `}]`
		list, err := officers.Parse(raw, func(k string) string {
			return map[string]string{
				officers.TokenEnvPrefix + officers.TokenEnvSuffix(offDS): offDSTok,
				officers.TokenEnvPrefix + officers.TokenEnvSuffix(offTR): offTRTok,
			}[k]
		}, "muster-agent-")
		if err != nil {
			t.Fatalf("parse the gap-task rig's officers: %v", err)
		}
		return list
	}, nil)
}

func TestTheGapTaskToolIsTheDatastoreOfficersBothPools(t *testing.T) {
	r := newGapTaskRig(t)
	ctx := context.Background()
	offers := func(a agents.Agent) (listed, allowed bool) {
		listed = officerToolNameSet(r.s.MCPToolDefsFor(ctx, a))[gapTaskTool]
		sess, ok := r.s.OfficerClaudeCodeSession(a)
		if !ok {
			t.Fatalf("%s has no officer session", a.Name)
		}
		for _, m := range sess.AllowMCPTools {
			allowed = allowed || m == gapTaskTool
		}
		return
	}
	dsRoutine := r.addNamed(r.a.Pool(officers.TierRoutine).InstanceName(1), 1301)
	dsJudge := r.addNamed(r.a.Pool(officers.TierJudgement).InstanceName(1), 1302)
	trRoutine := r.addNamed(r.b.Pool(officers.TierRoutine).InstanceName(1), 1303)
	for _, inst := range []agents.Agent{dsRoutine, dsJudge} {
		if l, a := offers(inst); !l || !a {
			t.Errorf("%s: listed %v, allowed %v; want both", inst.Name, l, a)
		}
	}
	if l, a := offers(trRoutine); l || a {
		t.Errorf("the task reconciler: listed %v, allowed %v; want neither", l, a)
	}
	for _, c := range []struct {
		who string
		a   agents.Agent
	}{{"the task reconciler", trRoutine}, {"an ordinary agent", r.agent(t, officerOrdinaryID)}, {"the chief", r.agent(t, officerChiefID)}} {
		r.hq.ResetRequests()
		out := r.s.dispatchAgentTool(ctx, c.a, gapTaskTool, `{"item_id":"1","store":"qdrant","kind":"no_backup"}`)
		if !isToolErr(out) {
			t.Errorf("%s calling %s: %s, want a refusal", c.who, gapTaskTool, out)
		}
		if n := len(r.hq.Requests()); n != 0 {
			t.Errorf("%s's refused filing reached HQ (%d requests)", c.who, n)
		}
	}
}

// TestAGapTaskSendsTheContractBodyWithTheOfficersToken: exactly one POST to
// …/items/{item}/gap-tasks, authorised with the OFFICER's token, its body
// exactly {instance, store, kind, …} — instance the caller's own name — and
// HQ's answer passed back; a repeat comes back filed false with the same task,
// and the item is still the instance's (a filing does not end the lease).
func TestAGapTaskSendsTheContractBodyWithTheOfficersToken(t *testing.T) {
	r := newGapTaskRig(t)
	ctx := context.Background()
	inst := r.addNamed(r.a.Pool(officers.TierRoutine).InstanceName(1), 1311)
	item := r.hq.AddTieredItem(offDS, kindAudit, "sched:daily-audit:x", officers.TierRoutine)
	if out := r.s.dispatchAgentTool(ctx, inst, officers.ToolClaim, `{"item_id":"`+item+`"}`); isToolErr(out) {
		t.Fatalf("claim: %s", out)
	}
	r.hq.ResetRequests()
	out := r.s.dispatchAgentTool(ctx, inst, gapTaskTool, `{"item_id":"`+item+`","store":"qdrant","kind":"no_backup","namespace":"qdrant","tier":"secondary"}`)
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["filed"] != true {
		t.Fatalf("first filing: %s, want HQ's answer with filed true", out)
	}
	reqs := r.hq.Requests()
	if len(reqs) != 1 {
		t.Fatalf("HQ saw %d requests, want 1: %+v", len(reqs), reqs)
	}
	if q := reqs[0]; q.Method != "POST" || q.Path != "/api/v1/officers/"+offDS+"/items/"+item+"/gap-tasks" || q.Auth != "Bearer "+offDSTok {
		t.Errorf("filing went %s %s with %q", q.Method, q.Path, q.Auth)
	}
	gts := r.hq.GapTasks()
	wantBody := map[string]any{"instance": inst.Name, "store": "qdrant", "kind": "no_backup", "namespace": "qdrant", "tier": "secondary"}
	if len(gts) != 1 || !reflect.DeepEqual(gts[0].Body, wantBody) {
		t.Fatalf("HQ received %+v, want exactly %v", gts, wantBody)
	}
	again := r.s.dispatchAgentTool(ctx, inst, gapTaskTool, `{"item_id":"`+item+`","store":"qdrant","kind":"no_backup"}`)
	if !strings.Contains(again, `"filed":false`) || !strings.Contains(again, gts[0].TaskID) {
		t.Fatalf("repeat: %s, want filed false with task %s", again, gts[0].TaskID)
	}
	if it := r.hq.Item(item); it.Status != "claimed" || it.ClaimedBy != inst.Name {
		t.Fatalf("after two filings the item is %s held by %q, want still claimed by %s", it.Status, it.ClaimedBy, inst.Name)
	}
}

func TestTheGapTaskToolRefusesLocallyWhatMusterOwns(t *testing.T) {
	r := newGapTaskRig(t)
	ctx := context.Background()
	inst := r.addNamed(r.a.Pool(officers.TierRoutine).InstanceName(1), 1321)
	for _, args := range []string{
		`{"item_id":"88","kind":"no_backup"}`,
		`{"item_id":"88","store":"qdrant"}`,
		`{"store":"qdrant","kind":"no_backup"}`,
		`{"item_id":"../x","store":"qdrant","kind":"no_backup"}`,
		`{"item_id":"88","store":"qdrant","kind":"no_backup","title":"my own words"}`,
		`{"item_id":"88","store":"qdrant","kind":"no_backup","officer_id":"datastore:other"}`,
	} {
		r.hq.ResetRequests()
		out := r.s.dispatchAgentTool(ctx, inst, gapTaskTool, args)
		if !isToolErr(out) {
			t.Errorf("%s: %s, want a refusal", args, out)
		}
		if n := len(r.hq.Requests()); n != 0 {
			t.Errorf("%s reached HQ (%d requests)", args, n)
		}
	}
}

// TestTheGapTaskToolPassesHQsRefusalsBack: HQ's cap and lost-lease answers
// come back with their status and code, so the charter's "list the rest as
// unfiled" and "do not retry" can act on them.
func TestTheGapTaskToolPassesHQsRefusalsBack(t *testing.T) {
	r := newGapTaskRig(t)
	ctx := context.Background()
	inst := r.addNamed(r.a.Pool(officers.TierRoutine).InstanceName(1), 1331)
	item := r.hq.AddTieredItem(offDS, kindAudit, "sched:daily-audit:y", officers.TierRoutine)
	// not claimed by this instance: HQ's lease_lost
	out := r.s.dispatchAgentTool(ctx, inst, gapTaskTool, `{"item_id":"`+item+`","store":"qdrant","kind":"no_backup"}`)
	if st, code := obsRefusal(out); st != 409 || code != "lease_lost" {
		t.Errorf("unheld item: %s, want refused 409 lease_lost", out)
	}
	if out := r.s.dispatchAgentTool(ctx, inst, officers.ToolClaim, `{"item_id":"`+item+`"}`); isToolErr(out) {
		t.Fatalf("claim: %s", out)
	}
	r.hq.RefuseVerb("gap-tasks", 429, "gap_task_cap", "this item already filed 3 gap tasks")
	out = r.s.dispatchAgentTool(ctx, inst, gapTaskTool, `{"item_id":"`+item+`","store":"ntfy","kind":"no_backup"}`)
	if st, code := obsRefusal(out); st != 429 || code != "gap_task_cap" {
		t.Errorf("capped: %s, want refused 429 gap_task_cap", out)
	}
}

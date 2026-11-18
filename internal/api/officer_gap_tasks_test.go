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
// either pool — has HQ file ONE open task per (store, kind) through HQ's officer
// gap-tasks route (civ-hq docs/OFFICERS.md §5o) with the officer's own token;
// no other officer is offered it. Names, paths and codes are literals on
// purpose: they are the contract.

const (
	gapTaskTool = "officer_gap_task"
	offDS       = "datastore:test-9"
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
		out := r.s.dispatchAgentTool(ctx, c.a, gapTaskTool, `{"item_id":"1","store":"vector-store","kind":"no_backup"}`)
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
	out := r.s.dispatchAgentTool(ctx, inst, gapTaskTool, `{"item_id":"`+item+`","store":"vector-store","kind":"no_backup","namespace":"vector-store","tier":"secondary"}`)
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
	wantBody := map[string]any{"instance": inst.Name, "store": "vector-store", "kind": "no_backup", "namespace": "vector-store", "tier": "secondary"}
	if len(gts) != 1 || !reflect.DeepEqual(gts[0].Body, wantBody) {
		t.Fatalf("HQ received %+v, want exactly %v", gts, wantBody)
	}
	again := r.s.dispatchAgentTool(ctx, inst, gapTaskTool, `{"item_id":"`+item+`","store":"vector-store","kind":"no_backup"}`)
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
		`{"item_id":"88","store":"vector-store"}`,
		`{"store":"vector-store","kind":"no_backup"}`,
		`{"item_id":"../x","store":"vector-store","kind":"no_backup"}`,
		`{"item_id":"88","store":"vector-store","kind":"no_backup","title":"my own words"}`,
		`{"item_id":"88","store":"vector-store","kind":"no_backup","officer_id":"datastore:other"}`,
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
	out := r.s.dispatchAgentTool(ctx, inst, gapTaskTool, `{"item_id":"`+item+`","store":"vector-store","kind":"no_backup"}`)
	if st, code := obsRefusal(out); st != 409 || code != "lease_lost" {
		t.Errorf("unheld item: %s, want refused 409 lease_lost", out)
	}
	if out := r.s.dispatchAgentTool(ctx, inst, officers.ToolClaim, `{"item_id":"`+item+`"}`); isToolErr(out) {
		t.Fatalf("claim: %s", out)
	}
	r.hq.RefuseVerb("gap-tasks", 429, "gap_task_cap", "this item already filed 3 gap tasks")
	out = r.s.dispatchAgentTool(ctx, inst, gapTaskTool, `{"item_id":"`+item+`","store":"push-svc","kind":"no_backup"}`)
	if st, code := obsRefusal(out); st != 429 || code != "gap_task_cap" {
		t.Errorf("capped: %s, want refused 429 gap_task_cap", out)
	}
}

// TestTheGapTaskToolSaysAClosedGapIsFiledAgain: HQ keeps ONE OPEN task per
// (officer, store, kind) and files a new one once that task is closed (civ-hq
// §5o). The tool text is what the model acts on, so the whole description is
// pinned: the first version told it a gap is filed once "EVER", which would
// have it skip a gap still present after a person closed its task.
func TestTheGapTaskToolSaysAClosedGapIsFiledAgain(t *testing.T) {
	want := "File ONE HQ task for a backup/DR gap you found (POST /officers/{id}/items/{item_id}/gap-tasks), for an item you hold. " +
		"Name the store and the gap kind; HQ writes the task's title and text from its own template and assigns it to your owner — you cannot write the text or pick a person. " +
		"Kinds: no_backup, backup_unobservable, no_offsite, no_stale_alert, no_failed_alert, no_wal_alert, no_metrics, no_size_metric, mirror_unverified, routing_gap, unregistered_store. " +
		"Filed in shadow too. HQ keeps ONE OPEN task per store and kind: filed:false means that task is still open (its task_id is in the answer) — that is not an error, report the link. " +
		"Once a person closes it, filing a gap you still find files a NEW task (filed:true): file it as usual and say it was refiled. " +
		"A 429 gap_task_cap means list the rest in your summary as unfiled, do not retry. A 403 gap_tasks_off means HQ does not let this officer file: do not retry. " +
		"It does NOT end your lease: keep working the item and complete it as usual, naming each task you filed."
	if got := gapTaskToolDef().Description; got != want {
		t.Fatalf("officer_gap_task description:\n got %q\nwant %q", got, want)
	}
}

// TestAGapTaskIsFiledAgainAfterItsTaskIsClosed: through the tool, against
// the fake HQ serving civ-hq's rule — a repeat while the task is open answers
// it (filed false), and once the task is closed the next filing comes back
// filed true with a NEW task, which a repeat then answers.
func TestAGapTaskIsFiledAgainAfterItsTaskIsClosed(t *testing.T) {
	r := newGapTaskRig(t)
	ctx := context.Background()
	inst := r.addNamed(r.a.Pool(officers.TierRoutine).InstanceName(1), 1341)
	item := r.hq.AddTieredItem(offDS, kindAudit, "sched:daily-audit:z", officers.TierRoutine)
	if out := r.s.dispatchAgentTool(ctx, inst, officers.ToolClaim, `{"item_id":"`+item+`"}`); isToolErr(out) {
		t.Fatalf("claim: %s", out)
	}
	file := func() (bool, string) {
		t.Helper()
		out := r.s.dispatchAgentTool(ctx, inst, gapTaskTool, `{"item_id":"`+item+`","store":"git-host","kind":"no_offsite"}`)
		var got struct {
			Filed   bool `json:"filed"`
			GapTask struct {
				TaskID string `json:"task_id"`
			} `json:"gap_task"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil || got.GapTask.TaskID == "" {
			t.Fatalf("filing: %s", out)
		}
		return got.Filed, got.GapTask.TaskID
	}
	filed, first := file()
	if !filed {
		t.Fatalf("first filing not filed")
	}
	if filed, id := file(); filed || id != first {
		t.Fatalf("repeat while open: filed %v task %s, want filed false with %s", filed, id, first)
	}
	r.hq.CloseGapTask(first)
	filed, second := file()
	if !filed || second == first {
		t.Fatalf("after close: filed %v task %s, want a new task (not %s)", filed, second, first)
	}
	if filed, id := file(); filed || id != second {
		t.Fatalf("repeat after the refile: filed %v task %s, want filed false with %s", filed, id, second)
	}
}

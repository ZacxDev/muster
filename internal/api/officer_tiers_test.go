package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// OFFICER TIERS, BOUND TO THE SERVER: a tiered officer's routine instances
// (`officer-<slug>-r<n>`) are its instances for every rule the name decides —
// the officer tools, the claim's tier, the session's model, delete protection
// — and an untiered officer beside it behaves exactly as before.
// ---------------------------------------------------------------------------

const (
	offT    = "gpu-health:gamma"
	offTTok = "fixture-officer-token-gamma-4e0b"
	// The tiered officer's two pools' instance-name stems.
	offTJudge   = "officer-gpu-health-gamma-"
	offTRoutine = "officer-gpu-health-gamma-r"
)

// newTieredOfficerRig is the rig over tiered officer T (r.a: judgement opus
// ×1, routine haiku ×2) and untiered officer A (r.b, max 2) — Parse's id order.
func newTieredOfficerRig(t *testing.T) *officerRig {
	t.Helper()
	return newOfficerRigWith(t, map[string]string{offATok: offA, offTTok: offT}, func(hqURL string) []officers.Officer {
		raw := `[{"id":"` + offA + `","hq_url":"` + hqURL + `","max_instances":2},` +
			`{"id":"` + offT + `","hq_url":"` + hqURL + `","tiers":{"routine":{"model":"haiku","max_instances":2},"judgement":{"model":"opus","max_instances":1}}}]`
		list, err := officers.Parse(raw, func(k string) string {
			return map[string]string{
				officers.TokenEnvPrefix + officers.TokenEnvSuffix(offA): offATok,
				officers.TokenEnvPrefix + officers.TokenEnvSuffix(offT): offTTok,
			}[k]
		}, "muster-agent-")
		if err != nil {
			t.Fatalf("parse the tiered rig's officers: %v", err)
		}
		return list
	})
}

// addNamed stores a running claude-code agent row named name.
func (r *officerRig) addNamed(name string, id int64) agents.Agent {
	a := agents.Agent{ID: id, Name: name, DisplayName: "officer", Kind: agents.KindClaudeCode, Status: agents.StatusRunning, OwnerID: 1, Namespace: "ns"}
	r.store.mu.Lock()
	r.store.rows = append(r.store.rows, a)
	r.store.mu.Unlock()
	r.prov.mu.Lock()
	r.prov.phase[a.Name] = provision.PhaseRunning
	r.prov.mu.Unlock()
	return a
}

// TestATieredInstanceClaimsWithItsTier: officer_claim from a routine instance
// sends tier routine and from a judgement instance tier judgement, so HQ's
// wrong_tier refusal reaches the instance and the item is untouched; an
// UNTIERED officer's claim sends no tier at all, as before. officer_items
// lists only the instance's own tier; an untiered officer's list is unfiltered.
func TestATieredInstanceClaimsWithItsTier(t *testing.T) {
	r := newTieredOfficerRig(t)
	ctx := context.Background()
	routine := r.addNamed(offTRoutine+"1", 941)
	judge := r.addNamed(offTJudge+"1", 942)
	flat := r.addNamed(officers.InstanceName(offA, 1), 943)
	rItem := r.hq.AddTieredItem(offT, "gpu.check", "node-a", officers.TierRoutine)
	jItem := r.hq.AddTieredItem(offT, "gpu.alert", "node-b", officers.TierJudgement)
	aItem := r.hq.AddItem(offA, "release.evaluate", "svc:v1")

	claim := func(a agents.Agent, item string) (string, map[string]any) {
		t.Helper()
		r.hq.ResetRequests()
		out := r.s.dispatchAgentTool(ctx, a, officers.ToolClaim, `{"item_id":"`+item+`"}`)
		reqs := r.hq.Requests()
		if len(reqs) == 0 {
			t.Fatalf("%s claiming %s reached HQ with nothing: %s", a.Name, item, out)
		}
		return out, reqs[0].Body
	}

	out, body := claim(routine, jItem)
	if body["tier"] != officers.TierRoutine {
		t.Errorf("a routine instance's claim body = %v, want tier routine", body)
	}
	if !strings.Contains(out, "wrong_tier") {
		t.Errorf("a routine instance claiming a judgement item: %s, want HQ's wrong_tier refusal passed through", out)
	}
	if it := r.hq.Item(jItem); it.ClaimedBy != "" {
		t.Errorf("the refused claim took the item (claimed by %q)", it.ClaimedBy)
	}

	out, body = claim(routine, rItem)
	if body["tier"] != officers.TierRoutine || isToolErr(out) || r.hq.Item(rItem).ClaimedBy != routine.Name {
		t.Errorf("a routine instance claiming a routine item: body %v, result %s, holder %q", body, out, r.hq.Item(rItem).ClaimedBy)
	}

	out, body = claim(judge, jItem)
	if body["tier"] != officers.TierJudgement || isToolErr(out) || r.hq.Item(jItem).ClaimedBy != judge.Name {
		t.Errorf("a judgement instance claiming a judgement item: body %v, result %s, holder %q", body, out, r.hq.Item(jItem).ClaimedBy)
	}

	out, body = claim(flat, aItem)
	if _, named := body["tier"]; named || isToolErr(out) {
		t.Errorf("an untiered officer's claim: body %v (must name no tier), result %s", body, out)
	}

	for _, tc := range []struct {
		a    agents.Agent
		want string // "" = no tier filter at all
	}{{routine, "tier=routine"}, {judge, "tier=judgement"}, {flat, ""}} {
		r.hq.ResetRequests()
		if out := r.s.dispatchAgentTool(ctx, tc.a, officers.ToolItems, `{}`); isToolErr(out) {
			t.Fatalf("%s officer_items: %s", tc.a.Name, out)
		}
		reqs := r.hq.Requests()
		if len(reqs) != 1 {
			t.Fatalf("%s officer_items: %d requests", tc.a.Name, len(reqs))
		}
		q := reqs[0].Query
		if tc.want == "" && strings.Contains(q, "tier") || tc.want != "" && !strings.Contains(q, tc.want) {
			t.Errorf("%s officer_items query = %q, want %q", tc.a.Name, q, tc.want)
		}
	}
}

// TestTieredInstancesAreTheOfficersForEveryNameRule: a routine instance is
// delete-protected and runs its tier's model, like a judgement instance; a
// `-r<n>` past the routine max, and a `-r<n>` beside an UNTIERED officer, are
// ordinary agents.
func TestTieredInstancesAreTheOfficersForEveryNameRule(t *testing.T) {
	r := newTieredOfficerRig(t)
	r1 := r.addNamed(offTRoutine+"1", 951)
	r2 := r.addNamed(offTRoutine+"2", 952)
	r3 := r.addNamed(offTRoutine+"3", 953) // past the routine max of 2
	j1 := r.addNamed(offTJudge+"1", 954)
	j2 := r.addNamed(offTJudge+"2", 955)                     // past the judgement max of 1 (routine's 2 must not leak)
	flatR := r.addNamed("officer-release-alpha-web-r1", 956) // A is untiered: no routine pool

	list, _ := r.store.List(context.Background())
	protect := r.s.deleteProtection(list)
	for _, tc := range []struct {
		a    agents.Agent
		want bool
	}{{r1, true}, {r2, true}, {r3, false}, {j1, true}, {j2, false}, {flatR, false}} {
		if got := protect(tc.a); got != tc.want {
			t.Errorf("deleteProtection(%s) = %v, want %v", tc.a.Name, got, tc.want)
		}
	}
	for _, tc := range []struct {
		a       agents.Agent
		protect bool
	}{{r2, true}, {r3, false}} {
		rec := doReq(t, r.s, r.h, http.MethodDelete, "/agents/"+strconv.FormatInt(tc.a.ID, 10))
		got := waitDestroyed(r.prov, tc.a.ID)
		if tc.protect && (rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "instance of officer "+offT) || got) {
			t.Errorf("DELETE %s = %d %q (destroyed %v), want 409 naming the officer and no Destroy", tc.a.Name, rec.Code, rec.Body.String(), got)
		}
		if !tc.protect && (rec.Code != http.StatusOK || !got) {
			t.Errorf("DELETE %s = %d (destroyed %v), want 200 and a Destroy", tc.a.Name, rec.Code, got)
		}
	}
	for _, tc := range []struct {
		a     agents.Agent
		model string
	}{{r1, "haiku"}, {j1, "opus"}} {
		sess, ok := r.installed(tc.a)
		if !ok || sess.Model != tc.model || !sess.BypassPermissions {
			t.Errorf("session for %s = %+v (ok %v), want model %q", tc.a.Name, sess, ok, tc.model)
		}
	}
	if _, ok := r.installed(r3); ok {
		t.Error("an agent past the routine max got an officer session")
	}
}

// TestTheWakeLoopCreatesRoutineInstanceAgents: over the real fleet binding, a
// routine item creates routine instance 1 (`-r1`, shown as `#r1`) with a
// kickoff naming its tier — and no judgement instance.
func TestTheWakeLoopCreatesRoutineInstanceAgents(t *testing.T) {
	r := newTieredOfficerRig(t)
	ctx := context.Background()
	r.hq.AddTieredItem(offT, "gpu.check", "node-a", officers.TierRoutine)
	if res := r.rec.Check(ctx, offT, true); res.Err != nil || len(res.ActErrs) != 0 {
		t.Fatalf("pass: %+v", res)
	}
	if len(r.store.created) != 1 {
		t.Fatalf("created %d agents, want 1: %+v", len(r.store.created), r.store.created)
	}
	c := r.store.created[0]
	if c.Name != offTRoutine+"1" || c.DisplayName != "officer "+offT+" #r1" || c.Kind != agents.KindClaudeCode {
		t.Fatalf("created %q (%q), want %q (%q)", c.Name, c.DisplayName, offTRoutine+"1", "officer "+offT+" #r1")
	}
	if !strings.Contains(c.PendingNote, offTRoutine+"1") || !strings.Contains(c.PendingNote, "routine") || strings.Contains(c.PendingNote, offTTok) {
		t.Fatalf("first turn: %q (must name the instance and its tier, and never carry the token)", c.PendingNote)
	}
	officerWaitFor(t, func() bool { d, _, _ := r.prov.counts(); return d == 1 })
}

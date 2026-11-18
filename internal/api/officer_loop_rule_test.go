package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officership"
)

// A pull request in its review loop, through muster with HQ faked: the
// authoring officer's push needs the *.pr_fix item for it HELD under a live
// lease, read from HQ — a lapsed lease and a released item are no hold.

func (r *shepAPIRig) putInLoop(t *testing.T) {
	t.Helper()
	pg, ok := r.s.callerOf(r.authInst).Gate.(*officership.PushGate)
	if !ok || pg.Loops == nil {
		t.Fatal("the draft officer's gate has no review-loop record")
	}
	if err := pg.Loops.Settle(context.Background(), shepRepoAPI, 5, officers.PRLoopInLoop); err != nil {
		t.Fatal(err)
	}
}

func TestAnInLoopPullRequestNeedsTheFixItemHeldThroughHQ(t *testing.T) {
	r := newShepAPIRig(t)
	r.authGH.mu.Lock()
	r.authGH.author = "delta-investigator[bot]"
	r.authGH.mu.Unlock()
	// Out of the loop: the draft stage's push, as before.
	if out, err := r.authPush(t); err != nil || out == nil {
		t.Fatalf("control: a push out of the loop: %v", err)
	}
	// Announced, but HQ named no loop item (no_taker), or still pending: not
	// in the loop.
	pg := r.s.callerOf(r.authInst).Gate.(*officership.PushGate)
	// Another pull request in a loop, so the push's branch lookup runs and
	// #5's own state is what decides.
	if err := pg.Loops.Settle(context.Background(), shepRepoAPI, 99, officers.PRLoopInLoop); err != nil {
		t.Fatal(err)
	}
	for _, st := range []string{officers.PRLoopPending, officers.PRLoopNoTaker, officers.PRLoopClosed} {
		if err := pg.Loops.Settle(context.Background(), shepRepoAPI, 5, st); err != nil {
			t.Fatal(err)
		}
		if out, err := r.authPush(t); err != nil || out == nil {
			t.Fatalf("a push with the announcement %s: %v, want it admitted (not in the loop)", st, err)
		}
	}
	r.putInLoop(t)
	refusedForLoop := func(what string) {
		t.Helper()
		out, err := r.authPush(t)
		if out != nil || !errors.Is(err, connectors.ErrGateRefused) || !strings.Contains(err.Error(), "#5 is in its review loop") {
			t.Fatalf("%s: err = %v (forwarded %v), want the loop refusal", what, err, out != nil)
		}
	}
	refusedForLoop("no fix item at all")
	// An ORDINARY item for #5 held live is not a fix item; a fix item for #5
	// held live by ANOTHER instance is not this instance's hold.
	plain := r.hq.AddItem(offShepAuth, "investigate.opened", officers.ShepherdSubject(shepRepoAPI, 5))
	r.hq.SetClaim(plain, r.authInst.Name, time.Now().Add(time.Hour))
	refusedForLoop("only an ordinary item held")
	theirs := r.hq.AddItem(offShepAuth, "investigate.pr_fix", officers.ShepherdSubject(shepRepoAPI, 5))
	r.hq.SetClaim(theirs, "officer-investigate-delta-infra-2", time.Now().Add(time.Hour))
	refusedForLoop("the fix item held by another instance")
	r.hq.Complete(theirs)
	fix := r.hq.AddItem(offShepAuth, "investigate.pr_fix", officers.ShepherdSubject(shepRepoAPI, 5))
	no := false
	r.hq.SetItemShadow(fix, &no)
	refusedForLoop("a fix item open, not claimed (released)")
	r.hq.SetClaim(fix, r.authInst.Name, time.Now().Add(-time.Minute))
	refusedForLoop("a lapsed lease")
	r.hq.SetClaim(fix, r.authInst.Name, time.Now().Add(time.Hour))
	if out, err := r.authPush(t); err != nil || out == nil {
		t.Fatalf("the fix item held live: %v", err)
	}
}

// TestTheHandoffToSchemaIsTheShepherdsOnly: officer_complete's schema offers
// handoff.to (enum ["author"]) to a shepherd and to no other officer.
func TestTheHandoffToSchemaIsTheShepherdsOnly(t *testing.T) {
	r := newShepAPIRig(t)
	toOf := func(o officers.Officer) any {
		b, _ := json.Marshal(completeToolDef(o).Parameters)
		var p struct {
			Properties map[string]struct {
				Properties map[string]map[string]any `json:"properties"`
			} `json:"properties"`
		}
		_ = json.Unmarshal(b, &p)
		return p.Properties["handoff"].Properties["to"]["enum"]
	}
	all := r.s.officerRT.reg.All()
	var shep, draft officers.Officer
	for _, o := range all {
		if o.Ship.IsShepherd() {
			shep = o
		} else {
			draft = o
		}
	}
	if got, _ := json.Marshal(toOf(shep)); string(got) != `["author"]` {
		t.Fatalf("the shepherd's handoff.to enum = %s, want [\"author\"]", got)
	}
	if got := toOf(draft); got != nil {
		t.Fatalf("the draft officer's schema offers handoff.to (%v)", got)
	}
	params := func(o officers.Officer) string { b, _ := json.Marshal(completeToolDef(o).Parameters); return string(b) }
	if !strings.Contains(params(shep), `Add to: \"author\"`) || strings.Contains(params(draft), `Add to: \"author\"`) {
		t.Fatal("the handoff description names to: \"author\" for the wrong officer")
	}
}

// TestShadowLinesEndWithTheirHold: a release forgets the item's lines; a
// lapsed lease, an item another instance now holds or one gone is pruned;
// a live hold keeps them.
func TestShadowLinesEndWithTheirHold(t *testing.T) {
	r := newShepAPIRig(t)
	shep := r.s.officerRT.reg.All()[1]
	ids := map[string]string{}
	for _, name := range []string{"released", "lapsed", "live", "gone"} {
		id := r.hq.AddItem(offShep, "pr-shepherd.opened", officers.ShepherdSubject(shepRepoAPI, 5))
		r.hq.SetClaim(id, r.inst.Name, time.Now().Add(time.Hour))
		r.s.officerRT.recordShadow(offShep, officers.ItemID(id), "would have commented on x#5 ("+name+")")
		ids[name] = id
	}
	b, _ := json.Marshal(map[string]any{"item_id": ids["released"]})
	if out := r.s.dispatchAgentTool(context.Background(), r.inst, officers.ToolRelease, string(b)); isToolErr(out) {
		t.Fatalf("release: %s", out)
	}
	r.hq.SetClaim(ids["lapsed"], r.inst.Name, time.Now().Add(-time.Minute))
	r.hq.Complete(ids["gone"])
	kept := func(name string) bool {
		return r.s.officerRT.shadowSummary(offShep, officers.ItemID(ids[name]), "x", officers.MaxSummaryChars) != "x"
	}
	if kept("released") {
		t.Fatal("a release kept the item's shadow lines")
	}
	if err := r.s.pruneShadow(context.Background(), shep, time.Now()); err != nil {
		t.Fatal(err)
	}
	if kept("lapsed") || kept("gone") || !kept("live") {
		t.Fatalf("after the prune: lapsed %v gone %v live %v; want only live kept", kept("lapsed"), kept("gone"), kept("live"))
	}
}

// TestThePruneKeepsALineRecordedAfterItsRead: the prune's now is taken before
// its HQ read; a line recorded after it (an item HQ's answer did not show
// held yet) survives, one recorded before it does not.
func TestThePruneKeepsALineRecordedAfterItsRead(t *testing.T) {
	r := newShepAPIRig(t)
	shep := r.s.officerRT.reg.All()[1]
	id := r.hq.AddItem(offShep, "pr-shepherd.opened", officers.ShepherdSubject(shepRepoAPI, 5)) // open: not held
	r.s.officerRT.recordShadow(offShep, officers.ItemID(id), "would have commented on x#5 (old)")
	readAt := time.Now()
	time.Sleep(2 * time.Millisecond)
	r.s.officerRT.recordShadow(offShep, officers.ItemID(id), "would have commented on x#5 (new)")
	if err := r.s.pruneShadow(context.Background(), shep, readAt); err != nil {
		t.Fatal(err)
	}
	got := r.s.officerRT.shadowSummary(offShep, officers.ItemID(id), "x", officers.MaxSummaryChars)
	if !strings.Contains(got, "(new)") || strings.Contains(got, "(old)") {
		t.Fatalf("after a prune at the read: %q, want the line recorded after it kept and the older one dropped", got)
	}
}

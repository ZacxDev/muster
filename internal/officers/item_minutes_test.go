package officers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
)

// ---------------------------------------------------------------------------
// THE JUDGEMENT WALL-TIME CAP: `tiers.judgement.max_item_minutes`.
//
// A judgement instance that has held one item for the cap is stopped by the
// wake loop, and the item is handed back to a human: completed `escalated`
// with a summary naming the cap. Not released — a released judgement item is
// claimed again by the same pool (re-claiming an item already counted today
// is free under max_sessions_per_day), so a release is a loop, not a stop.
// Not `needs_judgement` — HQ refuses that outcome on a judgement item (civ-hq
// docs/OFFICERS.md, "Tiers and due dates").
//
// Fixture values: the cap is 30 minutes and the passes land at 29 and 31, so
// neither sits on the boundary; the routine pool has no cap and holds its
// item for the same 31 minutes, so a cap applied to the wrong pool fails.
// ---------------------------------------------------------------------------

const cappedTiers = `"tiers":{"routine":{"model":"haiku","max_instances":3},"judgement":{"model":"opus","max_instances":2,"max_item_minutes":30}}`

type capRig struct {
	hq    *hqtest.Server
	clk   *clock
	fleet *poolFleet
	rec   *officers.Reconciler
	logs  *bytes.Buffer
	list  []officers.Officer
}

func newCapRig(t *testing.T, tiers string) *capRig {
	t.Helper()
	hq := hqtest.New(map[string]string{tierTok: tierID})
	t.Cleanup(hq.Close)
	clk := &clock{t: time.Date(2001, 1, 1, 9, 0, 0, 0, time.UTC)}
	hq.Now = clk.Now
	list, err := officers.Parse(`[{"id":"`+tierID+`","hq_url":"`+hq.BaseURL()+`","idle_scale_to_zero_after":"10m",`+tiers+`}]`,
		env(map[string]string{officers.TokenEnvPrefix + officers.TokenEnvSuffix(tierID): tierTok}), "muster-agent-")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	r := &capRig{hq: hq, clk: clk, fleet: &poolFleet{up: map[string]bool{}, kickoff: map[string]string{}}, logs: &bytes.Buffer{}, list: list}
	r.restart(t)
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(r.logs.String())
		}
	})
	return r
}

// restart replaces the reconciler with a new one over the same HQ and fleet:
// a restarted muster, which remembers nothing.
func (r *capRig) restart(t *testing.T) {
	t.Helper()
	rec, err := officers.NewReconciler(officers.Config{Registry: officers.NewRegistry(r.list), HQ: officers.NewClientHQ(nil), Fleet: r.fleet,
		Logger: log.New(r.logs, "", 0), Now: r.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	r.rec = rec
}

func (r *capRig) check(t *testing.T) officers.PassResult {
	t.Helper()
	res := r.rec.Check(context.Background(), tierID, true)
	if res.Err != nil {
		t.Fatalf("pass: %v", res.Err)
	}
	return res
}

// hold puts item id in holder's live claim for ten more minutes, as the
// instance's heartbeat would (a claim, if holder has no live lease on it).
func (r *capRig) hold(id, holder string) { r.hq.SetClaim(id, holder, r.clk.Now().Add(10*time.Minute)) }

// heldFor advances the clock by d with each (item, holder) pair of pairs
// renewing its 10-minute lease every 5 minutes, as a working instance's
// heartbeats do: the lease never lapses, so HQ keeps claimed_at. No pass runs.
func (r *capRig) heldFor(d time.Duration, pairs ...string) {
	for d > 0 {
		step := min(5*time.Minute, d)
		r.clk.Advance(step)
		d -= step
		for i := 0; i+1 < len(pairs); i += 2 {
			r.hold(pairs[i], pairs[i+1])
		}
	}
}

// completes is the complete requests HQ received, as "item:instance:outcome".
func (r *capRig) completes() []string {
	var out []string
	for _, q := range r.hq.Requests() {
		if q.Method == "POST" && strings.HasSuffix(q.Path, "/complete") {
			parts := strings.Split(q.Path, "/")
			inst, _ := q.Body["instance"].(string)
			oc, _ := q.Body["outcome"].(string)
			out = append(out, parts[len(parts)-2]+":"+inst+":"+oc)
		}
	}
	return out
}

// TestAJudgementItemIsStoppedAndEscalatedAtItsWallTimeCap: at 29 minutes
// nothing happens; past 30 the judgement instance holding the item is
// stopped and the item is completed `escalated` as that instance, with a
// summary naming the cap. The routine pool, which has no cap, holds its own
// item for the same time untouched.
func TestAJudgementItemIsStoppedAndEscalatedAtItsWallTimeCap(t *testing.T) {
	r := newCapRig(t, cappedTiers)
	j := r.hq.AddTieredItem(tierID, "gpu.alert", "node-c", officers.TierJudgement)
	ro := r.hq.AddTieredItem(tierID, "gpu.check", "node-a", officers.TierRoutine)
	r.check(t)
	if starts, _ := r.fleet.take(); fmt.Sprint(starts) != fmt.Sprint([]string{judgeStem + "1", routineStem + "1"}) {
		t.Fatalf("started %v, want judgement 1 and routine r1", starts)
	}
	if k := r.fleet.kickoff[judgeStem+"1"]; !strings.Contains(k, "wall-time cap of 30 minutes") {
		t.Errorf("the judgement kickoff does not tell the instance its 30-minute cap:\n%s", k)
	}
	if k := r.fleet.kickoff[routineStem+"1"]; strings.Contains(k, "wall-time cap") {
		t.Errorf("the routine kickoff names a cap its pool does not have:\n%s", k)
	}
	r.hold(j, judgeStem+"1")
	r.hold(ro, routineStem+"1")
	r.check(t) // both claims first seen now

	r.heldFor(29*time.Minute, j, judgeStem+"1", ro, routineStem+"1")
	r.check(t)
	if _, stops := r.fleet.take(); len(stops) != 0 {
		t.Fatalf("29 minutes into a 30-minute cap: stopped %v, want nothing", stops)
	}
	if got := r.hq.Item(j).Status; got != "claimed" {
		t.Fatalf("29 minutes into a 30-minute cap: item is %q, want still claimed", got)
	}

	r.heldFor(2*time.Minute, j, judgeStem+"1", ro, routineStem+"1")
	r.check(t)
	if _, stops := r.fleet.take(); fmt.Sprint(stops) != fmt.Sprint([]string{judgeStem + "1"}) {
		t.Fatalf("31 minutes into a 30-minute cap: stopped %v, want judgement instance 1 only", stops)
	}
	it := r.hq.Item(j)
	if it.Status != "escalated" || it.Outcome != "escalated" {
		t.Fatalf("the capped item is status %q outcome %q, want escalated: a human picks it up", it.Status, it.Outcome)
	}
	for _, want := range []string{"max_item_minutes", "30"} {
		if !strings.Contains(it.Summary, want) {
			t.Errorf("the escalation summary does not name %q:\n%s", want, it.Summary)
		}
	}
	if got := r.completes(); fmt.Sprint(got) != fmt.Sprint([]string{j + ":" + judgeStem + "1:escalated"}) {
		t.Errorf("complete requests %v, want exactly one, as the holder, escalated", got)
	}
	if got := r.hq.Item(ro).Status; got != "claimed" {
		t.Errorf("the routine item (no cap on its pool) is %q, want still claimed", got)
	}
}

// TestACappedItemWhoseHolderIsDownIsEscalatedNotReleased: the wake loop
// releases a live claim whose holder is not up so another instance can take
// it. A claim past the cap is escalated instead — released, it would be
// claimed again and run for another cap.
func TestACappedItemWhoseHolderIsDownIsEscalatedNotReleased(t *testing.T) {
	r := newCapRig(t, cappedTiers)
	j := r.hq.AddTieredItem(tierID, "gpu.alert", "node-c", officers.TierJudgement)
	r.check(t)
	r.hold(j, judgeStem+"1")
	r.check(t)
	// The instance heartbeats for 31 minutes, then dies with its lease live.
	r.heldFor(31*time.Minute, j, judgeStem+"1")
	r.fleet.mu.Lock()
	r.fleet.up[judgeStem+"1"] = false // the instance died holding the item
	r.fleet.mu.Unlock()
	r.check(t)
	if it := r.hq.Item(j); it.Status != "escalated" {
		t.Fatalf("a capped item whose holder is down is %q, want escalated (released, it would run another cap)", it.Status)
	}
	for _, q := range r.hq.Requests() {
		if strings.HasSuffix(q.Path, "/release") {
			t.Fatalf("the capped item was released: %s", q.Path)
		}
	}
}

// TestNoCapLeavesALongItemAlone is an INVARIANT GUARD (green on the base too):
// without max_item_minutes a judgement item may run as long as its holder
// heartbeats.
func TestNoCapLeavesALongItemAlone(t *testing.T) {
	r := newCapRig(t, tiersBlock)
	j := r.hq.AddTieredItem(tierID, "gpu.alert", "node-c", officers.TierJudgement)
	r.check(t)
	r.hold(j, judgeStem+"1")
	r.check(t)
	r.fleet.take()
	r.heldFor(90*time.Minute, j, judgeStem+"1")
	r.check(t)
	if _, stops := r.fleet.take(); len(stops) != 0 {
		t.Fatalf("no cap: stopped %v after 90 minutes", stops)
	}
	if got := r.hq.Item(j).Status; got != "claimed" {
		t.Fatalf("no cap: item is %q, want claimed", got)
	}
}

// TestMaxItemMinutesIsJudgementOnlyAndBounded: the cap is read on the
// judgement pool (asserted on the parsed pool's JSON, so this compiles on a
// tree without the field), refused on the routine pool and out of range, each
// refusal naming the field.
func TestMaxItemMinutesIsJudgementOnlyAndBounded(t *testing.T) {
	list, err := parseOfficer(t, ","+cappedTiers)
	if err != nil {
		t.Fatalf("a judgement cap of 30 minutes: %v", err)
	}
	b, _ := json.Marshal(list[0].Tiers)
	var got struct {
		Routine   map[string]any `json:"routine"`
		Judgement map[string]any `json:"judgement"`
	}
	_ = json.Unmarshal(b, &got)
	if v, _ := got.Judgement["max_item_minutes"].(float64); v != 30 {
		t.Fatalf("the judgement pool reads %s, want max_item_minutes 30", b)
	}
	if v, ok := got.Routine["max_item_minutes"].(float64); ok && v != 0 {
		t.Fatalf("the routine pool took the judgement cap: %s", b)
	}
	for _, tc := range []struct{ name, judgement, routine, want string }{
		{"on the routine pool", `{"model":"opus"}`, `{"model":"haiku","max_item_minutes":30}`, "tiers.routine: max_item_minutes"},
		{"over a day", `{"model":"opus","max_item_minutes":1441}`, `{"model":"haiku"}`, "tiers.judgement: max_item_minutes"},
		{"negative", `{"model":"opus","max_item_minutes":-1}`, `{"model":"haiku"}`, "tiers.judgement: max_item_minutes"},
	} {
		_, err := parseOfficer(t, `,"tiers":{"routine":`+tc.routine+`,"judgement":`+tc.judgement+`}`)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want a refusal containing %q", tc.name, err, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// THE CAP'S CLOCK IS HQ'S `claimed_at`, NOT MUSTER'S MEMORY.
//
// HQ sets claimed_at on every claim and keeps it across heartbeats, and the
// items list returns it. Measured from it, the cap survives a restart of
// muster: a muster restarted more often than the cap would otherwise never
// cap anything.
// ---------------------------------------------------------------------------

// TestARestartedMusterStillCapsAnItemClaimedBeforeIt: the item is claimed at
// 09:00 and watched by one muster for 20 minutes; a NEW muster (no memory)
// first sees it at 09:31 and must stop and escalate it on that first pass.
func TestARestartedMusterStillCapsAnItemClaimedBeforeIt(t *testing.T) {
	r := newCapRig(t, cappedTiers)
	j := r.hq.AddTieredItem(tierID, "gpu.alert", "node-c", officers.TierJudgement)
	r.check(t)
	r.hold(j, judgeStem+"1")
	r.check(t)
	r.heldFor(20*time.Minute, j, judgeStem+"1")
	r.check(t)
	r.fleet.take()

	r.heldFor(11*time.Minute, j, judgeStem+"1")
	r.restart(t)
	r.check(t)
	if _, stops := r.fleet.take(); fmt.Sprint(stops) != fmt.Sprint([]string{judgeStem + "1"}) {
		t.Fatalf("a restarted muster, 31 minutes after HQ's claimed_at: stopped %v, want judgement instance 1", stops)
	}
	if it := r.hq.Item(j); it.Status != "escalated" {
		t.Fatalf("a restarted muster, 31 minutes after HQ's claimed_at: item is %q, want escalated", it.Status)
	}
}

// TestAnItemWithoutClaimedAtIsNotCapped: an HQ that does not return
// claimed_at gives the cap no clock. The item is NOT capped (no guessing a
// start), and that is logged once, not on every pass.
func TestAnItemWithoutClaimedAtIsNotCapped(t *testing.T) {
	r := newCapRig(t, cappedTiers)
	j := r.hq.AddTieredItem(tierID, "gpu.alert", "node-c", officers.TierJudgement)
	r.check(t)
	r.hold(j, judgeStem+"1")
	r.hq.ClearClaimedAt(j)
	r.check(t)
	r.fleet.take()
	for i := 0; i < 2; i++ {
		r.clk.Advance(31 * time.Minute)
		r.hold(j, judgeStem+"1")
		r.hq.ClearClaimedAt(j)
		r.check(t)
	}
	if _, stops := r.fleet.take(); len(stops) != 0 {
		t.Fatalf("no claimed_at: stopped %v, want nothing (no clock, no cap)", stops)
	}
	if got := r.hq.Item(j).Status; got != "claimed" {
		t.Fatalf("no claimed_at: item is %q, want still claimed", got)
	}
	if n := strings.Count(r.logs.String(), "claimed_at"); n != 1 {
		t.Errorf("the missing claimed_at is logged %d times over three passes, want once:\n%s", n, r.logs.String())
	}
}

// ---------------------------------------------------------------------------
// THE CAP ESCALATION AND HQ's INVESTIGATION BLOCK.
//
// An officer type whose policy sets `investigation: true` must complete an
// `escalated` item with an `investigation` block, else HQ answers 400 and
// writes nothing (civ-hq docs/OFFICERS.md "Investigation"). HQ takes the
// block on `escalated` for any type, so the cap escalation always carries a
// minimal honest one: confidence low (which gives operator steps only), no
// conclusion, step: re-investigate.
// ---------------------------------------------------------------------------

// capped brings judgement item j to 31 minutes into a 30-minute cap, held by
// judgement instance 1, without running the pass that sees it.
func (r *capRig) capped(t *testing.T) string {
	t.Helper()
	j := r.hq.AddTieredItem(tierID, "gpu.alert", "node-c", officers.TierJudgement)
	r.check(t)
	r.hold(j, judgeStem+"1")
	r.check(t)
	r.fleet.take()
	r.heldFor(31*time.Minute, j, judgeStem+"1")
	return j
}

// TestTheCapEscalationCarriesALowConfidenceInvestigation: on an investigating
// type the cap's escalation is accepted, and its block says what happened —
// low confidence, the cap named, nothing concluded, re-investigate.
func TestTheCapEscalationCarriesALowConfidenceInvestigation(t *testing.T) {
	r := newCapRig(t, cappedTiers)
	r.hq.Investigating = map[string]bool{tierID: true}
	j := r.capped(t)
	r.check(t)
	it := r.hq.Item(j)
	if it.Status != "escalated" {
		t.Fatalf("on an investigating type the capped item is %q, want escalated (HQ refuses an escalation without an investigation block)", it.Status)
	}
	inv := it.Investigation
	if inv["confidence"] != "low" {
		t.Errorf("investigation.confidence = %v, want low: nothing was concluded", inv["confidence"])
	}
	fix, _ := inv["fix"].(map[string]any)
	if prs, ok := fix["pr_urls"]; ok {
		t.Errorf("investigation.fix.pr_urls = %v, want none: low confidence gives operator steps only", prs)
	}
	steps, _ := fix["operator_steps"].([]any)
	if len(steps) == 0 || !strings.Contains(strings.ToLower(fmt.Sprint(steps)), "re-investigate") {
		t.Errorf("investigation.fix.operator_steps = %v, want a re-investigate step", steps)
	}
	for _, want := range []string{"30 minutes", "wall-time cap"} {
		if rc, _ := inv["root_cause"].(string); !strings.Contains(rc, want) {
			t.Errorf("investigation.root_cause %q does not name %q", rc, want)
		}
	}
}

// TestARefusedCapEscalationKeepsTheItemHeldNotReclaimed: HQ refuses the cap's
// escalation (400). Muster logs HQ's error and keeps the item HELD by the
// stopped holder (heartbeating it), so its lease never lapses and no instance
// claims it for another full session. Once HQ takes the escalation, it lands.
func TestARefusedCapEscalationKeepsTheItemHeldNotReclaimed(t *testing.T) {
	r := newCapRig(t, cappedTiers)
	const refusal = "investigation.root_cause must be at most 300 characters"
	r.hq.RefuseCompletes(400, "invalid", refusal)
	j := r.capped(t)
	r.check(t)
	if _, stops := r.fleet.take(); fmt.Sprint(stops) != fmt.Sprint([]string{judgeStem + "1"}) {
		t.Fatalf("at the cap, escalation refused: stopped %v, want judgement instance 1", stops)
	}
	if !strings.Contains(r.logs.String(), refusal) {
		t.Errorf("the refused escalation is not logged with HQ's error %q:\n%s", refusal, r.logs.String())
	}
	r.hq.ResetRequests()
	// 20 minutes of passes: twice the 10-minute lease the holder last took.
	for i := 0; i < 4; i++ {
		r.clk.Advance(5 * time.Minute)
		r.check(t)
		it := r.hq.Item(j)
		if it.Status != "claimed" || it.ClaimedBy != judgeStem+"1" || it.LeaseExpiresAt == nil || !it.LeaseExpiresAt.After(r.clk.Now()) {
			t.Fatalf("%d minutes after a refused cap escalation the item is %q by %q (lease %v): want still held by the stopped holder, so nothing re-claims it",
				5*(i+1), it.Status, it.ClaimedBy, it.LeaseExpiresAt)
		}
		if starts, _ := r.fleet.take(); len(starts) != 0 {
			t.Fatalf("%d minutes after a refused cap escalation: started %v, want nothing (a start would re-run a full session)", 5*(i+1), starts)
		}
	}
	for _, q := range r.hq.Requests() {
		if strings.HasSuffix(q.Path, "/release") || strings.HasSuffix(q.Path, "/claim") {
			t.Fatalf("a refused cap escalation led to %s %s", q.Method, q.Path)
		}
	}
	n := 0
	for _, line := range strings.Split(r.logs.String(), "\n") {
		if strings.Contains(line, refusal) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the same refusal is logged on %d lines over five passes, want once:\n%s", n, r.logs.String())
	}
	r.hq.RefuseCompletes(0, "", "")
	r.clk.Advance(time.Minute)
	r.check(t)
	if it := r.hq.Item(j); it.Status != "escalated" {
		t.Fatalf("once HQ takes the escalation the item is %q, want escalated", it.Status)
	}
}

// TestAnHQBeforeTheInvestigationBlockStillTakesTheCapEscalation: an HQ that
// predates the block refuses it as an unknown field (400), and the cap's
// escalation still lands, without it. Regression coverage, not an invariant
// guard: red on the base (which refuses max_item_minutes at Parse), and the
// only test that fails when stopOverrun's 400 retry is removed.
func TestAnHQBeforeTheInvestigationBlockStillTakesTheCapEscalation(t *testing.T) {
	r := newCapRig(t, cappedTiers)
	r.hq.PreInvestigation = true
	j := r.capped(t)
	r.check(t)
	if it := r.hq.Item(j); it.Status != "escalated" {
		t.Fatalf("on an HQ before the investigation block the capped item is %q, want escalated", it.Status)
	}
}

// TestACappedHolderIsNeverStartedForOtherWork: HQ keeps refusing the cap's
// escalation, so the capped item A stays held in judgement instance 1's NAME
// (muster heartbeats it) while another judgement item B is open. Instance 1
// must never be started again: a new session under that name claims B, and
// the next pass stops it for A's cap, killing B's work, every few polls and
// without bound (a re-claim is free under max_sessions_per_day). With a
// second instance number free B goes to it; at max_instances 1 B waits.
// Nothing is stopped after the cap's own stop.
func TestACappedHolderIsNeverStartedForOtherWork(t *testing.T) {
	for _, tc := range []struct{ name, tiers, holderOfB string }{
		{"a second instance number free", cappedTiers, judgeStem + "2"},
		{"max_instances 1", `"tiers":{"routine":{"model":"haiku","max_instances":3},"judgement":{"model":"opus","max_instances":1,"max_item_minutes":30}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCapRig(t, tc.tiers)
			r.hq.RefuseCompletes(400, "invalid", "investigation.root_cause must be at most 300 characters")
			a := r.capped(t)
			b := r.hq.AddTieredItem(tierID, "gpu.alert", "node-d", officers.TierJudgement)
			r.check(t) // the cap: instance 1 stopped, the escalation refused, A kept held
			starts, stops := r.fleet.take()
			if fmt.Sprint(stops) != fmt.Sprint([]string{judgeStem + "1"}) {
				t.Fatalf("at the cap: stopped %v, want judgement instance 1", stops)
			}
			up := func(name string) bool {
				r.fleet.mu.Lock()
				defer r.fleet.mu.Unlock()
				return r.fleet.up[name]
			}
			started := append([]string(nil), starts...)
			// 30 minutes of passes: three times the 10-minute lease A's holder
			// last took, so A lives only by muster's hold.
			for i := 0; i < 6; i++ {
				r.clk.Advance(5 * time.Minute)
				if it := r.hq.Item(b); it.Status == "claimed" && up(it.ClaimedBy) {
					r.hold(b, it.ClaimedBy) // B's holder heartbeats it
				}
				r.check(t)
				starts, stops := r.fleet.take()
				if len(stops) != 0 {
					t.Fatalf("pass %d after a refused cap escalation: stopped %v, want nothing (B, which is nowhere near its cap, was being worked)", i+1, stops)
				}
				for _, s := range starts {
					started = append(started, s)
					if r.hq.Item(b).Status == "open" {
						r.hold(b, s) // a started instance claims B at once
					}
				}
			}
			for _, s := range started {
				if s == judgeStem+"1" {
					t.Fatalf("started %v after a refused cap escalation: instance 1 holds capped item %s and must never be started", started, a)
				}
			}
			if it := r.hq.Item(a); it.Status != "claimed" || it.ClaimedBy != judgeStem+"1" {
				t.Errorf("capped item A is %q by %q, want still held by instance 1 (its escalation is refused)", it.Status, it.ClaimedBy)
			}
			it := r.hq.Item(b)
			if tc.holderOfB == "" {
				if it.Status != "open" || len(started) != 0 {
					t.Errorf("max_instances 1: B is %q by %q and %v started, want B open and nothing started (it waits for instance 1's slot)", it.Status, it.ClaimedBy, started)
				}
			} else if it.Status != "claimed" || it.ClaimedBy != tc.holderOfB {
				t.Errorf("B is %q by %q, want claimed by %s", it.Status, it.ClaimedBy, tc.holderOfB)
			}
		})
	}
}

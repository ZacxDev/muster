package officers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
)

// ---------------------------------------------------------------------------
// OFFICER TIERS: a `tiers` block splits an officer's instances into a routine
// pool (`officer-<slug>-r<n>`) and a judgement pool (`officer-<slug>-<n>`),
// each with its own model and max instances, and each woken only for items of
// its tier as HQ reads them.
//
// Fixture values are pairwise distinct — routine max 3, judgement max 2,
// models "haiku" and "opus" — so a pool that read the other pool's value
// cannot pass.
// ---------------------------------------------------------------------------

const (
	tierID  = "gpu-health:fleet"
	tierTok = "tok-tiered-5b2a91" // a fixture, nobody's token
	// The two pools' instance-name stems.
	judgeStem   = "officer-gpu-health-fleet-"
	routineStem = "officer-gpu-health-fleet-r"
)

const tiersBlock = `"tiers":{"routine":{"model":"haiku","max_instances":3},"judgement":{"model":"opus","max_instances":2}}`

func parseOfficer(t *testing.T, fields string) ([]officers.Officer, error) {
	t.Helper()
	raw := `[{"id":"` + tierID + `","hq_url":"https://hq.example.com/api/v1"` + fields + `}]`
	return officers.Parse(raw, env(map[string]string{officers.TokenEnvPrefix + officers.TokenEnvSuffix(tierID): tierTok}), "muster-agent-")
}

func mustTiered(t *testing.T) officers.Officer {
	t.Helper()
	list, err := parseOfficer(t, ","+tiersBlock)
	if err != nil {
		t.Fatalf("a valid tiers block was refused: %v", err)
	}
	return list[0]
}

// pools returns o's judgement and routine pools, failing unless there are
// exactly those two.
func pools(t *testing.T, o officers.Officer) (judge, routine officers.Officer) {
	t.Helper()
	ps := o.Pools()
	if len(ps) != 2 {
		t.Fatalf("a tiered officer has %d pools, want 2 (judgement, routine)", len(ps))
	}
	for _, p := range ps {
		switch p.Tier {
		case officers.TierJudgement:
			judge = p
		case officers.TierRoutine:
			routine = p
		}
	}
	if judge.Tier == "" || routine.Tier == "" {
		t.Fatalf("pools are %q and %q, want judgement and routine", ps[0].Tier, ps[1].Tier)
	}
	return judge, routine
}

// TestATiersBlockDeclaresTwoPools: each pool has its tier's model and max
// instances, and its own instance names; the binding rule resolves a name to
// the pool it belongs to, and to nothing past that pool's max.
func TestATiersBlockDeclaresTwoPools(t *testing.T) {
	o := mustTiered(t)
	judge, routine := pools(t, o)
	if judge.Model != "opus" || judge.MaxInstances != 2 || routine.Model != "haiku" || routine.MaxInstances != 3 {
		t.Fatalf("judgement = %q/%d, routine = %q/%d; want opus/2 and haiku/3", judge.Model, judge.MaxInstances, routine.Model, routine.MaxInstances)
	}
	if got := routine.InstanceName(2); got != routineStem+"2" {
		t.Errorf("routine instance 2 = %q, want %q", got, routineStem+"2")
	}
	if got := judge.InstanceName(2); got != judgeStem+"2" {
		t.Errorf("judgement instance 2 = %q, want %q", got, judgeStem+"2")
	}
	reg := officers.NewRegistry([]officers.Officer{o})
	for name, want := range map[string]string{
		judgeStem + "1":    "judgement#1 opus/2",
		judgeStem + "2":    "judgement#2 opus/2",
		judgeStem + "3":    "", // past judgement's max of 2 (routine's 3 must not leak)
		routineStem + "1":  "routine#1 haiku/3",
		routineStem + "3":  "routine#3 haiku/3",
		routineStem + "4":  "", // past routine's max
		routineStem + "0":  "",
		routineStem + "01": "",
		routineStem:        "",
		routineStem + "1x": "",
		judgeStem + "rr1":  "",
		judgeStem + "R1":   "",
		judgeStem + "r-1":  "",
	} {
		p, n, ok := reg.ForAgentName(name)
		got := ""
		if ok {
			got = fmt.Sprintf("%s#%d %s/%d", p.Tier, n, p.Model, p.MaxInstances)
		}
		if got != want {
			t.Errorf("ForAgentName(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestNoTiersBlockIsTodaysOfficer — an INVARIANT GUARD (it holds at the base
// too): an officer without `tiers` is one untiered pool, named and bound as
// before, and a `-r<n>` name is not its instance.
func TestNoTiersBlockIsTodaysOfficer(t *testing.T) {
	list, err := parseOfficer(t, `,"max_instances":2,"model":"opus"`)
	if err != nil {
		t.Fatal(err)
	}
	o := list[0]
	if o.Tiers != nil || o.Tier != "" {
		t.Fatalf("untiered officer: Tiers %v, Tier %q; want nil and \"\"", o.Tiers, o.Tier)
	}
	ps := o.Pools()
	if len(ps) != 1 || ps[0].Tier != "" || ps[0].MaxInstances != 2 || ps[0].Model != "opus" {
		t.Fatalf("untiered officer's pools = %+v, want just itself", ps)
	}
	if got := o.InstanceName(2); got != officers.InstanceName(tierID, 2) || got != judgeStem+"2" {
		t.Errorf("InstanceName(2) = %q", got)
	}
	reg := officers.NewRegistry(list)
	if _, _, ok := reg.ForAgentName(routineStem + "1"); ok {
		t.Error("an untiered officer claims a -r1 agent as its instance")
	}
	if _, n, ok := reg.ForAgentName(judgeStem + "2"); !ok || n != 2 {
		t.Error("an untiered officer lost its instance 2")
	}
	if k := officers.KickoffText(o, 1); strings.Contains(k, officers.TierRoutine) || strings.Contains(k, officers.TierJudgement) {
		t.Error("an untiered officer's kickoff mentions tiers")
	}
}

// TestTiersRefusals: a tiers block muster could not run is refused at boot,
// naming what is wrong.
func TestTiersRefusals(t *testing.T) {
	pool := func(model string, max int) string { return fmt.Sprintf(`{"model":%q,"max_instances":%d}`, model, max) }
	for _, tc := range []struct{ name, fields, want string }{
		{"unknown tier key", `,"tiers":{"routine":` + pool("haiku", 1) + `,"judgement":` + pool("opus", 1) + `,"cheap":` + pool("haiku", 1) + `}`, `"cheap"`},
		{"routine model missing", `,"tiers":{"routine":{"max_instances":1},"judgement":` + pool("opus", 1) + `}`, "tiers.routine: model is required"},
		{"judgement model missing", `,"tiers":{"routine":` + pool("haiku", 1) + `,"judgement":{"max_instances":1}}`, "tiers.judgement: model is required"},
		{"routine model blank", `,"tiers":{"routine":` + pool("  ", 1) + `,"judgement":` + pool("opus", 1) + `}`, "tiers.routine: model is required"},
		{"routine missing", `,"tiers":{"judgement":` + pool("opus", 1) + `}`, "tiers.routine"},
		{"judgement missing", `,"tiers":{"routine":` + pool("haiku", 1) + `}`, "tiers.judgement"},
		{"unknown pool key", `,"tiers":{"routine":{"model":"haiku","max_instances":1,"daily_budget_usd":5},"judgement":` + pool("opus", 1) + `}`, "daily_budget_usd"},
		{"routine max over cap", `,"tiers":{"routine":` + pool("haiku", officers.MaxInstancesCap+1) + `,"judgement":` + pool("opus", 1) + `}`, "tiers.routine: max_instances"},
		{"judgement max negative", `,"tiers":{"routine":` + pool("haiku", 1) + `,"judgement":` + pool("opus", -1) + `}`, "tiers.judgement: max_instances"},
		{"bad model", `,"tiers":{"routine":` + pool("has space", 1) + `,"judgement":` + pool("opus", 1) + `}`, "tiers.routine"},
		{"top-level model too", `,"model":"sonnet",` + tiersBlock, "tiers.judgement.model"},
		{"top-level max too", `,"max_instances":2,` + tiersBlock, "tiers.judgement.max_instances"},
		{"tiers not an object", `,"tiers":["routine"]`, "tiers"},
	} {
		_, err := parseOfficer(t, tc.fields)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want a refusal containing %q", tc.name, err, tc.want)
		}
	}
	// Control: the same block, well formed, parses — so each refusal above is
	// the one thing that case changed.
	if _, err := parseOfficer(t, ","+tiersBlock); err != nil {
		t.Fatalf("control: %v", err)
	}
	// A pool's max_instances defaults to 1, as the officer's does.
	list, err := parseOfficer(t, `,"tiers":{"routine":{"model":"haiku"},"judgement":{"model":"opus"}}`)
	if err != nil {
		t.Fatal(err)
	}
	j, r := pools(t, list[0])
	if j.MaxInstances != 1 || r.MaxInstances != 1 {
		t.Errorf("default max_instances: judgement %d, routine %d; want 1 and 1", j.MaxInstances, r.MaxInstances)
	}
}

// TestTheRoutineNameMustFitTheNamespaceBound: `-r<n>` is one character longer
// than `-<n>`, so the bound is checked against the routine pool's highest
// name too — a prefix that leaves room for the judgement name exactly is
// refused for the routine one.
func TestTheRoutineNameMustFitTheNamespaceBound(t *testing.T) {
	tok := env(map[string]string{officers.TokenEnvPrefix + officers.TokenEnvSuffix(tierID): tierTok})
	judgeName := officers.InstanceName(tierID, 1)
	prefix := strings.Repeat("p", officers.MaxNamespaceLen-len(judgeName)) // judgement fits exactly
	untiered := `[{"id":"` + tierID + `","hq_url":"https://hq.example.com/api/v1"}]`
	if _, err := officers.Parse(untiered, tok, prefix); err != nil {
		t.Fatalf("control: the untiered officer fits exactly: %v", err)
	}
	tiered := `[{"id":"` + tierID + `","hq_url":"https://hq.example.com/api/v1","tiers":{"routine":{"model":"haiku"},"judgement":{"model":"opus"}}}]`
	_, err := officers.Parse(tiered, tok, prefix)
	if err == nil || !strings.Contains(err.Error(), routineStem+"1") {
		t.Fatalf("a routine name one over the bound: err = %v, want a refusal naming %q", err, routineStem+"1")
	}
}

// tieredItem is an HQ item as HQ's JSON spells it — tier included when given.
func tieredItem(t *testing.T, id, tier, status, holder string, lease *time.Time, blocked bool) officers.Item {
	t.Helper()
	m := map[string]any{"id": id, "officer_id": tierID, "kind": "gpu.check", "subject_key": "s" + id, "status": status, "blocked": blocked}
	if tier != "-" {
		m["tier"] = tier
	}
	if holder != "" {
		m["claimed_by"] = holder
	}
	if lease != nil {
		m["lease_expires_at"] = lease
	}
	b, _ := json.Marshal(m)
	var it officers.Item
	if err := json.Unmarshal(b, &it); err != nil {
		t.Fatal(err)
	}
	return it
}

// poolInstances is p's instances 1..max, the named ones up.
func poolInstances(p officers.Officer, up ...string) []officers.InstanceState {
	isUp := map[string]bool{}
	for _, n := range up {
		isUp[n] = true
	}
	var out []officers.InstanceState
	for n := 1; n <= p.MaxInstances; n++ {
		name := p.InstanceName(n)
		out = append(out, officers.InstanceState{N: n, Name: name, Exists: isUp[name], Up: isUp[name]})
	}
	return out
}

// TestDecidePerTier: each pool's Decide sees only the claimable items of its
// own tier as HQ reads them (an item without a tier, or with one muster does
// not know, is judgement: fail toward Claude), and only the claims its own
// instances hold.
func TestDecidePerTier(t *testing.T) {
	o := mustTiered(t)
	judge, routine := pools(t, o)
	now := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	live, lapsed := now.Add(time.Minute), now.Add(-time.Minute)
	type want struct {
		claimable, held int
		start           []int
		release         []string
		idle            bool
	}
	for _, tc := range []struct {
		name          string
		open, claimed []officers.Item
		judgeUp       []string
		routineUp     []string
		judge, rout   want
	}{
		{name: "nothing", judge: want{idle: true}, rout: want{idle: true}},
		{name: "one routine item wakes the routine pool only",
			open:  []officers.Item{tieredItem(t, "1", "routine", "open", "", nil, false)},
			judge: want{idle: true}, rout: want{claimable: 1, start: []int{1}}},
		{name: "one judgement item wakes the judgement pool only",
			open:  []officers.Item{tieredItem(t, "1", "judgement", "open", "", nil, false)},
			judge: want{claimable: 1, start: []int{1}}, rout: want{idle: true}},
		{name: "an item without a tier is judgement",
			open:  []officers.Item{tieredItem(t, "1", "-", "open", "", nil, false)},
			judge: want{claimable: 1, start: []int{1}}, rout: want{idle: true}},
		{name: "an unknown tier is judgement",
			open:  []officers.Item{tieredItem(t, "1", "fast", "open", "", nil, false)},
			judge: want{claimable: 1, start: []int{1}}, rout: want{idle: true}},
		{name: "each pool scales to its own items and max",
			open: []officers.Item{
				tieredItem(t, "1", "routine", "open", "", nil, false), tieredItem(t, "2", "routine", "open", "", nil, false),
				tieredItem(t, "3", "routine", "open", "", nil, false), tieredItem(t, "4", "routine", "open", "", nil, false),
				tieredItem(t, "5", "judgement", "open", "", nil, false)},
			judge: want{claimable: 1, start: []int{1}}, rout: want{claimable: 4, start: []int{1, 2, 3}}},
		{name: "a lapsed claim HQ now reads as judgement (promoted) is judgement's",
			claimed: []officers.Item{tieredItem(t, "1", "judgement", "claimed", routineStem+"1", &lapsed, false)},
			judge:   want{claimable: 1, start: []int{1}}, rout: want{idle: true}},
		{name: "a lapsed routine claim is routine's to take again",
			claimed: []officers.Item{tieredItem(t, "1", "routine", "claimed", routineStem+"2", &lapsed, false)},
			judge:   want{idle: true}, rout: want{claimable: 1, start: []int{1}}},
		{name: "a live claim held by an up routine instance is the routine pool's",
			claimed:   []officers.Item{tieredItem(t, "1", "routine", "claimed", routineStem+"1", &live, false)},
			routineUp: []string{routineStem + "1"},
			judge:     want{idle: true}, rout: want{held: 1}},
		{name: "a live claim held by a DOWN routine instance is released by the routine pool alone",
			claimed: []officers.Item{tieredItem(t, "1", "routine", "claimed", routineStem+"2", &live, false)},
			judge:   want{idle: true}, rout: want{held: 1, release: []string{"1"}}},
		{name: "a claim is the holder's pool's, whatever the item's tier",
			claimed: []officers.Item{tieredItem(t, "1", "routine", "claimed", judgeStem+"1", &live, false)},
			judgeUp: []string{judgeStem + "1"},
			judge:   want{held: 1}, rout: want{idle: true}},
		{name: "a blocked routine item wakes nobody",
			open:  []officers.Item{tieredItem(t, "1", "routine", "open", "", nil, true)},
			judge: want{idle: true}, rout: want{idle: true}},
	} {
		for _, side := range []struct {
			pool officers.Officer
			up   []string
			want want
		}{{judge, tc.judgeUp, tc.judge}, {routine, tc.routineUp, tc.rout}} {
			obs := officers.Observation{Open: tc.open, Claimed: tc.claimed, Instances: poolInstances(side.pool, side.up...)}
			d := officers.Decide(side.pool, obs, time.Time{}, now, nil)
			var released []string
			for _, it := range d.Release {
				released = append(released, string(it.ID))
			}
			if d.Claimable != side.want.claimable || d.Held != side.want.held || d.Idle != side.want.idle ||
				fmt.Sprint(d.Start) != fmt.Sprint(side.want.start) || fmt.Sprint(released) != fmt.Sprint(side.want.release) {
				t.Errorf("%s — %s pool: claimable %d held %d start %v release %v idle %v; want claimable %d held %d start %v release %v idle %v",
					tc.name, side.pool.Tier, d.Claimable, d.Held, d.Start, released, d.Idle,
					side.want.claimable, side.want.held, side.want.start, side.want.release, side.want.idle)
			}
		}
	}

	// Control — an UNTIERED officer counts every tier, as today.
	list, err := parseOfficer(t, `,"max_instances":3`)
	if err != nil {
		t.Fatal(err)
	}
	flat := list[0]
	obs := officers.Observation{
		Open: []officers.Item{tieredItem(t, "1", "routine", "open", "", nil, false), tieredItem(t, "2", "judgement", "open", "", nil, false),
			tieredItem(t, "3", "-", "open", "", nil, false)},
		Instances: poolInstances(flat),
	}
	if d := officers.Decide(flat, obs, time.Time{}, now, nil); d.Claimable != 3 || fmt.Sprint(d.Start) != "[1 2 3]" {
		t.Errorf("untiered officer: claimable %d start %v, want 3 and [1 2 3]", d.Claimable, d.Start)
	}
}

// poolFleet is a Fleet keyed by instance NAME, so the two pools of one officer
// are distinct instances.
type poolFleet struct {
	mu      sync.Mutex
	up      map[string]bool
	starts  []string
	stops   []string
	kickoff map[string]string
	// failStop: a Stop of one of these instance names is recorded and fails,
	// leaving the instance up.
	failStop map[string]bool
}

func (f *poolFleet) Instances(_ context.Context, o officers.Officer) ([]officers.InstanceState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []officers.InstanceState
	for n := 1; n <= o.MaxInstances; n++ {
		name := o.InstanceName(n)
		out = append(out, officers.InstanceState{N: n, Name: name, Exists: f.up[name], Up: f.up[name]})
	}
	return out, nil
}

func (f *poolFleet) Start(_ context.Context, o officers.Officer, n int, kickoff string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := o.InstanceName(n)
	f.up[name] = true
	f.starts = append(f.starts, name)
	f.kickoff[name] = kickoff
	return nil
}

func (f *poolFleet) Stop(_ context.Context, o officers.Officer, n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := o.InstanceName(n)
	f.stops = append(f.stops, name)
	if f.failStop[name] {
		return errors.New("fixture: stop failed")
	}
	f.up[name] = false
	return nil
}

func (f *poolFleet) Reprompt(context.Context, officers.Officer, int, string) error { return nil }

func (f *poolFleet) take() (starts, stops []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	starts, stops = f.starts, f.stops
	f.starts, f.stops = nil, nil
	sort.Strings(starts)
	sort.Strings(stops)
	return starts, stops
}

// TestTheWakeLoopRunsEachTierPool: one pass reads HQ once and moves each pool
// for its own tier's items — starting the routine pool's `-r<n>` instances
// with a kickoff that names the tier — and each pool idles out on its own
// clock: the routine pool scales to zero while the judgement pool works on,
// and the judgement pool while the routine pool works on.
func TestTheWakeLoopRunsEachTierPool(t *testing.T) {
	hq := hqtest.New(map[string]string{tierTok: tierID})
	t.Cleanup(hq.Close)
	clk := &clock{t: time.Date(2001, 1, 1, 9, 0, 0, 0, time.UTC)}
	hq.Now = clk.Now
	list, err := officers.Parse(`[{"id":"`+tierID+`","hq_url":"`+hq.BaseURL()+`","idle_scale_to_zero_after":"10m",`+tiersBlock+`}]`,
		env(map[string]string{officers.TokenEnvPrefix + officers.TokenEnvSuffix(tierID): tierTok}), "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	fleet := &poolFleet{up: map[string]bool{}, kickoff: map[string]string{}}
	logs := &bytes.Buffer{}
	rec, err := officers.NewReconciler(officers.Config{Registry: officers.NewRegistry(list), HQ: officers.NewBackends(nil), Fleet: fleet, Logger: log.New(logs, "", 0), Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	check := func() officers.PassResult {
		t.Helper()
		res := rec.Check(context.Background(), tierID, true)
		if res.Err != nil {
			t.Fatalf("pass: %v", res.Err)
		}
		return res
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	})

	r1 := hq.AddTieredItem(tierID, "gpu.check", "node-a", officers.TierRoutine)
	r2 := hq.AddTieredItem(tierID, "gpu.check", "node-b", officers.TierRoutine)
	res := check()
	if starts, _ := fleet.take(); fmt.Sprint(starts) != fmt.Sprint([]string{routineStem + "1", routineStem + "2"}) {
		t.Fatalf("two routine items: started %v, want the routine pool's r1 and r2 only", starts)
	}
	if res.Routine == nil || res.Routine.Claimable != 2 || !res.Decision.Idle {
		t.Fatalf("pass result: judgement %+v, routine %+v; want judgement idle and routine claimable 2", res.Decision, res.Routine)
	}
	if k := fleet.kickoff[routineStem+"1"]; !strings.Contains(k, routineStem+"1") || !strings.Contains(k, "routine") {
		t.Errorf("the routine instance's kickoff does not name it or its tier:\n%s", k)
	}

	j := hq.AddTieredItem(tierID, "gpu.alert", "node-c", officers.TierJudgement)
	check()
	if starts, _ := fleet.take(); fmt.Sprint(starts) != fmt.Sprint([]string{judgeStem + "1"}) {
		t.Fatalf("a judgement item: started %v, want the judgement pool's instance 1 only", starts)
	}
	if k := fleet.kickoff[judgeStem+"1"]; !strings.Contains(k, "judgement") {
		t.Errorf("the judgement instance's kickoff does not name its tier:\n%s", k)
	}

	// The routine items finish; the judgement item stays open. The routine
	// pool idles out after its window; the judgement pool does not.
	hq.Complete(r1)
	hq.Complete(r2)
	check() // routine idle starts now
	clk.Advance(11 * time.Minute)
	check()
	if _, stops := fleet.take(); fmt.Sprint(stops) != fmt.Sprint([]string{routineStem + "1", routineStem + "2"}) {
		t.Fatalf("routine idle for its window while judgement works: stopped %v, want r1 and r2 only", stops)
	}
	// The mirror image: the judgement item finishes while a new routine item
	// keeps the routine pool busy. The judgement pool idles out on its own
	// clock; the routine pool does not. (Both orders are asserted: a shared
	// idle clock passes whichever order the loop writes last — measured: the
	// routine-idles-first half alone let a one-clock mutant through.)
	hq.Complete(j)
	r3 := hq.AddTieredItem(tierID, "gpu.check", "node-d", officers.TierRoutine)
	check() // judgement idle starts now; the routine pool wakes for r3
	if starts, _ := fleet.take(); fmt.Sprint(starts) != fmt.Sprint([]string{routineStem + "1"}) {
		t.Fatalf("a new routine item: started %v, want routine r1 again", starts)
	}
	clk.Advance(11 * time.Minute)
	check()
	if _, stops := fleet.take(); fmt.Sprint(stops) != fmt.Sprint([]string{judgeStem + "1"}) {
		t.Fatalf("judgement idle for its window while routine works: stopped %v, want judgement instance 1 only", stops)
	}
	hq.Complete(r3)
	check()
	clk.Advance(11 * time.Minute)
	check()
	if _, stops := fleet.take(); fmt.Sprint(stops) != fmt.Sprint([]string{routineStem + "1"}) {
		t.Fatalf("routine idle for its window: stopped %v, want r1", stops)
	}
}

// TestTheClaimSendsTheTier: Client.Claim names the tier when given one — HQ
// then refuses another tier's item (409 wrong_tier) and changes nothing — and
// sends NO tier key when given "" (an untiered officer's claim, as today).
func TestTheClaimSendsTheTier(t *testing.T) {
	hq := hqtest.New(map[string]string{tierTok: tierID})
	t.Cleanup(hq.Close)
	o := mustTiered(t)
	o.HQURL = hq.BaseURL()
	c := officers.NewClient(o, nil)
	ctx := context.Background()
	jItem := hq.AddTieredItem(tierID, "gpu.alert", "a", officers.TierJudgement)
	rItem := hq.AddTieredItem(tierID, "gpu.check", "b", officers.TierRoutine)
	anyItem := hq.AddTieredItem(tierID, "gpu.check", "c", officers.TierRoutine)

	hq.ResetRequests()
	_, err := c.Claim(ctx, officers.ItemID(jItem), routineStem+"1", 60, officers.TierRoutine)
	var hqErr *officers.HQError
	if !errors.As(err, &hqErr) || hqErr.Status != 409 || !strings.Contains(string(hqErr.Body), `"wrong_tier"`) {
		t.Fatalf("a routine claim of a judgement item: err = %v, want HQ's 409 wrong_tier", err)
	}
	if got := hq.Item(jItem); got.Status != "open" || got.ClaimedBy != "" {
		t.Fatalf("the refused claim changed the item: %+v", got)
	}
	if reqs := hq.Requests(); len(reqs) != 1 || reqs[0].Body["tier"] != officers.TierRoutine {
		t.Fatalf("the claim's body = %v, want tier %q", reqs, officers.TierRoutine)
	}
	if _, err := c.Claim(ctx, officers.ItemID(rItem), routineStem+"1", 60, officers.TierRoutine); err != nil {
		t.Fatalf("a routine claim of a routine item: %v", err)
	}
	hq.ResetRequests()
	if _, err := c.Claim(ctx, officers.ItemID(anyItem), judgeStem+"1", 60, ""); err != nil {
		t.Fatalf("a claim naming no tier: %v", err)
	}
	if reqs := hq.Requests(); len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	} else if _, named := reqs[0].Body["tier"]; named {
		t.Fatalf("a claim given no tier sent one: %v", reqs[0].Body)
	}

	// The items list takes the tier filter the same way.
	hq.ResetRequests()
	page, err := c.Items(ctx, officers.ItemsQuery{Status: officers.StatusOpen, Tier: officers.TierJudgement})
	if err != nil {
		t.Fatal(err)
	}
	if reqs := hq.Requests(); len(reqs) != 1 || !strings.Contains(reqs[0].Query, "tier=judgement") {
		t.Fatalf("items query = %v, want tier=judgement", reqs)
	}
	if len(page.Items) != 1 || string(page.Items[0].ID) != jItem || page.Items[0].Tier != officers.TierJudgement {
		t.Fatalf("judgement items = %+v, want only %s", page.Items, jItem)
	}
}

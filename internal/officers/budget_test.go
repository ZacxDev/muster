package officers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// A builtin officer's daily budget (daily_budget_usd) and routine batching
// (routine_kinds + routine_wake_at): budget.go.
// ---------------------------------------------------------------------------

const budgetID = "marketing:demo"

// budgetDecl is the declaration every test here parses: a $10 budget and
// plugin.note / plugin.snapshot batched into one wake at 09:00 UTC.
const budgetDecl = `[{"id": "marketing:demo", "backend": "builtin", "max_instances": 1, "idle_scale_to_zero_after": "30m",
  "daily_budget_usd": 10, "routine_kinds": ["plugin.snapshot", "plugin.note"], "routine_wake_at": "09:00"}]`

func parseBudgetOfficer(t *testing.T, raw string) officers.Officer {
	t.Helper()
	list, err := officers.Parse(raw, func(string) string { return "" }, "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("parsed %d officers, want 1", len(list))
	}
	return list[0]
}

func TestTheBudgetSettingsParse(t *testing.T) {
	o := parseBudgetOfficer(t, budgetDecl)
	if o.Budget.DailyUSD != 10 {
		t.Fatalf("DailyUSD = %v, want 10", o.Budget.DailyUSD)
	}
	if got := strings.Join(o.Budget.RoutineKinds, ","); got != "plugin.note,plugin.snapshot" {
		t.Fatalf("RoutineKinds = %q, want sorted plugin.note,plugin.snapshot", got)
	}
	if o.Budget.WakeAt != 9*time.Hour {
		t.Fatalf("WakeAt = %v, want 9h", o.Budget.WakeAt)
	}
	// No budget fields: the zero Budget, which caps and batches nothing.
	plain := parseBudgetOfficer(t, `[{"id": "marketing:demo", "backend": "builtin"}]`)
	if plain.Budget.DailyUSD != 0 || len(plain.Budget.RoutineKinds) != 0 || plain.OverBudget(1e6) || plain.IsRoutineKind("plugin.note") {
		t.Fatalf("an officer with no budget settings caps or batches: %+v", plain.Budget)
	}
}

func TestBadBudgetSettingsAreRefusedAtBoot(t *testing.T) {
	for name, tc := range map[string]struct{ decl, want string }{
		"not builtin": {`[{"id": "release:x", "hq_url": "https://hq.example/api/v1", "daily_budget_usd": 10}]`, `need "backend": "builtin"`},
		"negative":    {`[{"id": "marketing:demo", "backend": "builtin", "daily_budget_usd": -1}]`, "daily_budget_usd -1 is not in"},
		"too big":     {`[{"id": "marketing:demo", "backend": "builtin", "daily_budget_usd": 1000.5}]`, "daily_budget_usd 1000.5 is not in"},
		"kinds no wake": {`[{"id": "marketing:demo", "backend": "builtin", "routine_kinds": ["plugin.note"]}]`,
			"routine_kinds needs routine_wake_at"},
		"wake no kinds": {`[{"id": "marketing:demo", "backend": "builtin", "routine_wake_at": "09:00"}]`,
			"routine_wake_at without routine_kinds"},
		"bad wake": {`[{"id": "marketing:demo", "backend": "builtin", "routine_kinds": ["plugin.note"], "routine_wake_at": "25:00"}]`,
			`routine_wake_at "25:00" is not`},
		"spaced kind": {`[{"id": "marketing:demo", "backend": "builtin", "routine_kinds": ["plugin note"], "routine_wake_at": "09:00"}]`,
			`routine_kinds entry "plugin note" is not an item kind`},
		"host officer": {`[{"id": "marketing:demo", "backend": "builtin", "runtime": "host", "host": "host-alpha", "repo": "example-org/example-site", "daily_budget_usd": 10}]`,
			`daily_budget_usd is refused on a "runtime": "host" officer`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := officers.Parse(tc.decl, func(string) string { return "" }, "")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to say %q", err, tc.want)
			}
		})
	}
}

// TestRoutineItemsOfOneDayComeDueTogether: every routine item enqueued
// between two wakes gets the same not_before (the next 09:00 UTC); one
// enqueued at or after the wake waits for tomorrow's; a kind not listed, or
// one that merely starts like a listed one, is not deferred at all.
// TestAHostOfficerWithoutABudgetStillParses is the host refusal's control:
// the same host declaration, no daily_budget_usd, parses — so the refusal
// above is the budget's, not something else about the declaration.
func TestAHostOfficerWithoutABudgetStillParses(t *testing.T) {
	o := parseBudgetOfficer(t, `[{"id": "marketing:demo", "backend": "builtin", "runtime": "host", "host": "host-alpha", "repo": "example-org/example-site"}]`)
	if !o.OnHost() || o.Budget.DailyUSD != 0 {
		t.Fatalf("host officer without a budget: OnHost %v, budget %v", o.OnHost(), o.Budget.DailyUSD)
	}
}

func TestRoutineItemsOfOneDayComeDueTogether(t *testing.T) {
	o := parseBudgetOfficer(t, budgetDecl)
	day := time.Date(2000, 3, 4, 0, 0, 0, 0, time.UTC)
	today9, tomorrow9 := day.Add(9*time.Hour), day.Add(33*time.Hour)
	for _, tc := range []struct {
		at   time.Duration
		kind string
		want *time.Time
	}{
		{0, "plugin.note", &today9},
		{8*time.Hour + 59*time.Minute, "plugin.snapshot", &today9},
		{9 * time.Hour, "plugin.note", &tomorrow9},
		{9*time.Hour + time.Second, "plugin.note", &tomorrow9},
		{23 * time.Hour, "plugin.snapshot", &tomorrow9},
		{3 * time.Hour, "plugin.reply", nil},
		{3 * time.Hour, "plugin.no", nil},
		{3 * time.Hour, "draft.refused", nil},
	} {
		got := o.RoutineDue(tc.kind, day.Add(tc.at))
		switch {
		case tc.want == nil && got != nil:
			t.Errorf("%s at +%s: deferred to %s, want not deferred", tc.kind, tc.at, got)
		case tc.want != nil && (got == nil || !got.Equal(*tc.want)):
			t.Errorf("%s at +%s: not_before %v, want %s", tc.kind, tc.at, got, tc.want)
		}
	}
	// A wake time from a zone ahead of UTC is still read as UTC.
	east := time.FixedZone("east", 5*3600)
	if got := o.RoutineDue("plugin.note", time.Date(2000, 3, 4, 13, 0, 0, 0, east)); got == nil || !got.Equal(today9) {
		t.Fatalf("08:00 UTC given in +05:00: not_before %v, want %s", got, today9)
	}
}

func TestTheBudgetBoundary(t *testing.T) {
	o := parseBudgetOfficer(t, budgetDecl)
	for _, tc := range []struct {
		spent float64
		want  bool
	}{{0, false}, {9.99, false}, {10, true}, {10.49, true}} {
		if got := o.OverBudget(tc.spent); got != tc.want {
			t.Errorf("OverBudget(%v) with a $10 budget = %v, want %v", tc.spent, got, tc.want)
		}
	}
	// A budget that is not $10, so a cap read from anywhere but the
	// officer's own daily_budget_usd shows: $10.49 is under $25.50.
	o25 := parseBudgetOfficer(t, `[{"id": "marketing:demo", "backend": "builtin", "daily_budget_usd": 25.5}]`)
	for _, tc := range []struct {
		spent float64
		want  bool
	}{{10.49, false}, {25.49, false}, {25.99, true}} {
		if got := o25.OverBudget(tc.spent); got != tc.want {
			t.Errorf("OverBudget(%v) with a $25.50 budget = %v, want %v", tc.spent, got, tc.want)
		}
	}
}

func TestOnlyTheOwnersNonRoutineItemsBypassTheBudget(t *testing.T) {
	o := parseBudgetOfficer(t, budgetDecl)
	for _, tc := range []struct {
		it   officers.Item
		want bool
	}{
		{officers.Item{Kind: "draft.refused", Origin: officers.OriginOwner}, true},
		{officers.Item{Kind: "manual.run", Origin: officers.OriginOwner}, true},
		{officers.Item{Kind: "draft.refused", Origin: ""}, false},
		{officers.Item{Kind: "plugin.reply", Origin: ""}, false},
		{officers.Item{Kind: "plugin.note", Origin: officers.OriginOwner}, false},
		{officers.Item{Kind: "plugin.snapshot", Origin: officers.OriginOwner}, false},
		{officers.Item{Kind: "draft.refused", Origin: "Owner"}, false},
	} {
		if got := o.BypassesBudget(tc.it); got != tc.want {
			t.Errorf("BypassesBudget(%s, origin %q) = %v, want %v", tc.it.Kind, tc.it.Origin, got, tc.want)
		}
	}
}

// TestAnItemNotYetDueDoesNotWakeTheOfficer: an item whose not_before is
// ahead (a batched routine item, or any item enqueued with --not-before) is
// refused by the claim (409 not_due), so it must not wake an instance that
// would claim nothing; once due, it wakes one. The items are read from the
// JSON the built-in backend writes, as the wake loop reads them.
func TestAnItemNotYetDueDoesNotWakeTheOfficer(t *testing.T) {
	now := time.Date(2000, 3, 4, 12, 0, 0, 0, time.UTC)
	o := officers.Officer{ID: budgetID, MaxInstances: 1, IdleAfter: 30 * time.Minute, PollInterval: time.Minute, Owner: 1}
	var items []officers.Item
	if err := json.Unmarshal([]byte(`[{"id": "7", "kind": "plugin.note", "status": "open", "not_before": "2000-03-05T09:00:00Z"}]`), &items); err != nil {
		t.Fatal(err)
	}
	insts := []officers.InstanceState{{N: 1, Name: officers.InstanceName(budgetID, 1)}}
	d := officers.Decide(o, officers.Observation{Open: items, Instances: insts}, time.Time{}, now, nil)
	if len(d.Start) != 0 || d.Claimable != 0 || !d.Idle {
		t.Fatalf("an item due tomorrow woke the officer: %+v", d)
	}
	d = officers.Decide(o, officers.Observation{Open: items, Instances: insts}, time.Time{}, now.Add(21*time.Hour), nil)
	if len(d.Start) != 1 || d.Claimable != 1 {
		t.Fatalf("the item at its not_before did not wake the officer: %+v", d)
	}
	// Over budget as well, an item not yet due is still not work at all:
	// it is not counted as held back by the budget either (it would not be
	// claimable with the budget fresh).
	d = officers.Decide(o, officers.Observation{Open: items, Instances: insts, OverBudget: true}, time.Time{}, now, nil)
	if d.BudgetWaiting != 0 || d.Claimable != 0 || len(d.Start) != 0 {
		t.Fatalf("an item due tomorrow, over budget: %+v, want neither claimable nor held back by the budget", d)
	}
}

// budgetHQ is the loop's HQ for a builtin officer with a budget: open items
// in memory, and the day's spend (spendReader).
type budgetHQ struct {
	mu       sync.Mutex
	open     []officers.Item
	spent    map[string]float64 // by UTC day
	spendErr error
	reads    int
}

func (h *budgetHQ) AllItems(_ context.Context, _ officers.Officer, status string) ([]officers.Item, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if status != officers.StatusOpen {
		return nil, nil
	}
	return append([]officers.Item(nil), h.open...), nil
}
func (h *budgetHQ) Release(context.Context, officers.Officer, officers.ItemID, string) error {
	return nil
}
func (h *budgetHQ) Complete(context.Context, officers.Officer, officers.ItemID, string, officers.Completion) error {
	return nil
}
func (h *budgetHQ) Heartbeat(context.Context, officers.Officer, officers.ItemID, string, int) error {
	return nil
}
func (h *budgetHQ) SpentToday(_ context.Context, _ officers.Officer, now time.Time) (float64, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reads++
	return h.spent[now.UTC().Format("2006-01-02")], true, h.spendErr
}

func (h *budgetHQ) add(id, kind, origin string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.open = append(h.open, officers.Item{ID: officers.ItemID(id), Kind: kind, Status: officers.StatusOpen, Origin: origin})
}

func (h *budgetHQ) setOpen(items ...officers.Item) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.open = items
}

func (h *budgetHQ) setSpent(day time.Time, usd float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.spent[day.UTC().Format("2006-01-02")] = usd
}

type budgetRig struct {
	hq     *budgetHQ
	fleet  *fakeFleet
	clk    *clock
	rec    *officers.Reconciler
	alerts []officers.BudgetAlert
	logs   *bytes.Buffer
	mu     sync.Mutex
}

func newBudgetRig(t *testing.T) *budgetRig {
	t.Helper()
	o := parseBudgetOfficer(t, budgetDecl)
	r := &budgetRig{hq: &budgetHQ{spent: map[string]float64{}}, fleet: newFleet(),
		clk: &clock{t: time.Date(2000, 3, 4, 12, 0, 0, 0, time.UTC)}, logs: &bytes.Buffer{}}
	rec, err := officers.NewReconciler(officers.Config{
		Registry: officers.NewRegistry([]officers.Officer{o}),
		HQ:       r.hq,
		Fleet:    r.fleet,
		Logger:   log.New(r.logs, "", 0),
		Now:      r.clk.Now,
		BudgetAlert: func(_ officers.Officer, a officers.BudgetAlert) {
			r.mu.Lock()
			r.alerts = append(r.alerts, a)
			r.mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.rec = rec
	return r
}

func (r *budgetRig) check(t *testing.T) officers.PassResult {
	t.Helper()
	res := r.rec.Check(context.Background(), budgetID, true)
	if res.Err != nil {
		t.Fatalf("pass: %v", res.Err)
	}
	return res
}

func (r *budgetRig) alertCount() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.alerts) }

// TestASpentBudgetStopsWakesAlertsOnceAndLetsOnlyTheOwnersItemsThrough walks
// one officer with a $10 budget through a day: at $9.99 an item wakes it; a
// $0.50 run takes it to $10.49 — over the cap without landing on it — and
// from then a plugin's item and the owner's ROUTINE item hold back (counted),
// the owner is told exactly once however many passes follow, and the owner's
// own non-routine item still wakes it. The next UTC day the budget is fresh:
// the held items wake it, and only a new crossing alerts again.
func TestASpentBudgetStopsWakesAlertsOnceAndLetsOnlyTheOwnersItemsThrough(t *testing.T) {
	r := newBudgetRig(t)
	day1 := r.clk.Now()
	r.hq.setSpent(day1, 9.99)
	r.hq.add("1", "plugin.reply", "")
	if res := r.check(t); len(res.Decision.Start) != 1 || res.Decision.Claimable != 1 || res.Decision.BudgetWaiting != 0 {
		t.Fatalf("at $9.99 of $10 a plugin item did not wake the officer: %+v", res.Decision)
	}
	if n := r.alertCount(); n != 0 {
		t.Fatalf("alerted %d time(s) under the budget", n)
	}

	// The instance worked item 1 (a $0.50 run) and scaled to zero.
	r.fleet.Stop(context.Background(), officers.Officer{}, 1)
	r.hq.setOpen()
	r.hq.setSpent(day1, 10.49)
	r.hq.add("2", "plugin.reply", "")
	r.clk.Advance(time.Minute)
	res := r.check(t)
	if len(res.Decision.Start) != 0 || res.Decision.Claimable != 0 || res.Decision.BudgetWaiting != 1 {
		t.Fatalf("at $10.49 of $10 a plugin item woke the officer (or was not counted held back): %+v", res.Decision)
	}
	if n := r.alertCount(); n != 1 {
		t.Fatalf("crossing the budget alerted %d time(s), want 1", n)
	}
	if a := r.alerts[0]; a.Spent != 10.49 || a.Budget != 10 || a.Waiting != 1 || !a.ResetAt.Equal(time.Date(2000, 3, 5, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("alert = %+v, want spent 10.49, budget 10, 1 waiting, reset 2000-03-05T00:00Z", a)
	}
	if !strings.Contains(r.logs.String(), "daily budget reached") {
		t.Fatalf("no log line for the crossing:\n%s", r.logs)
	}

	// More passes the same day: still held, never a second alert.
	r.hq.add("3", "plugin.note", officers.OriginOwner) // routine: never bypasses
	for i := 0; i < 3; i++ {
		r.clk.Advance(time.Minute)
		res = r.check(t)
		if len(res.Decision.Start) != 0 || res.Decision.BudgetWaiting != 2 {
			t.Fatalf("pass %d over budget: the owner's ROUTINE item or a plugin item woke it: %+v", i, res.Decision)
		}
	}
	if n := r.alertCount(); n != 1 {
		t.Fatalf("passes after the crossing alerted again: %d alerts, want 1", n)
	}

	// The owner's own non-routine item wakes it, the rest still held.
	r.hq.add("4", "draft.refused", officers.OriginOwner)
	r.clk.Advance(time.Minute)
	res = r.check(t)
	if len(res.Decision.Start) != 1 || res.Decision.Claimable != 1 || res.Decision.BudgetWaiting != 2 {
		t.Fatalf("over budget, the owner's draft.refused item did not wake the officer alone: %+v", res.Decision)
	}

	// The next UTC day: nothing spent yet, so the held items are work again,
	// and no alert until the new day's spend crosses.
	r.fleet.Stop(context.Background(), officers.Officer{}, 1)
	r.hq.setOpen(officers.Item{ID: "2", Kind: "plugin.reply", Status: officers.StatusOpen},
		officers.Item{ID: "3", Kind: "plugin.note", Status: officers.StatusOpen, Origin: officers.OriginOwner})
	r.clk.Advance(13 * time.Hour)
	res = r.check(t)
	if len(res.Decision.Start) != 1 || res.Decision.Claimable != 2 || res.Decision.BudgetWaiting != 0 {
		t.Fatalf("the next UTC day, the held items did not wake the officer: %+v", res.Decision)
	}
	if n := r.alertCount(); n != 1 {
		t.Fatalf("a fresh day alerted with nothing spent: %d alerts", n)
	}
	r.hq.setSpent(r.clk.Now(), 10.49)
	r.clk.Advance(time.Minute)
	r.check(t)
	if n := r.alertCount(); n != 2 {
		t.Fatalf("the second day's crossing: %d alerts, want 2", n)
	}
}

// TestAnUnreadableSpendHoldsWorkBackWithoutAlerting: the spend cannot be read
// — fail closed for the pass (a plugin item does not wake it), but nobody is
// told "the budget is reached", which nobody measured. The owner's item still
// wakes it.
func TestAnUnreadableSpendHoldsWorkBackAndTellsTheOwnerOnceADay(t *testing.T) {
	r := newBudgetRig(t)
	r.hq.spendErr = errors.New("usage table unreachable")
	r.hq.add("1", "plugin.reply", "")
	res := r.check(t)
	if len(res.Decision.Start) != 0 || res.Decision.BudgetWaiting != 1 {
		t.Fatalf("an unreadable spend let a plugin item wake the officer: %+v", res.Decision)
	}
	for i := 0; i < 3; i++ {
		r.clk.Advance(time.Minute)
		r.check(t)
	}
	if n := r.alertCount(); n != 1 {
		t.Fatalf("an unreadable spend over 4 passes alerted %d time(s), want 1", n)
	}
	if a := r.alerts[0]; !a.Unreadable || a.Budget != 10 || a.Waiting != 1 || a.Spent != 0 {
		t.Fatalf("unreadable alert = %+v, want Unreadable, budget 10, 1 waiting, nothing spent", a)
	}
	r.hq.add("2", "draft.refused", officers.OriginOwner)
	if res := r.check(t); len(res.Decision.Start) != 1 {
		t.Fatalf("an unreadable spend held back the owner's own item: %+v", res.Decision)
	}
	// The next UTC day, still unreadable: told once more.
	r.clk.Advance(24 * time.Hour)
	r.check(t)
	if n := r.alertCount(); n != 2 {
		t.Fatalf("an unreadable spend the next day: %d alerts, want 2", n)
	}
	if !strings.Contains(r.logs.String(), "usage table unreachable") {
		t.Fatalf("the unreadable spend was not logged:\n%s", r.logs)
	}
}

// TestAnOfficerWithNoBudgetNeverReadsItsSpend: the read costs a query every
// pass; an officer that declares no budget must not pay it, and is never held.
func TestAnOfficerWithNoBudgetNeverReadsItsSpend(t *testing.T) {
	hq := &budgetHQ{spent: map[string]float64{"2000-03-04": 1e6}}
	o := parseBudgetOfficer(t, `[{"id": "marketing:demo", "backend": "builtin", "max_instances": 1}]`)
	fleet := newFleet()
	rec, err := officers.NewReconciler(officers.Config{Registry: officers.NewRegistry([]officers.Officer{o}), HQ: hq, Fleet: fleet,
		Logger: log.New(&bytes.Buffer{}, "", 0), Now: func() time.Time { return time.Date(2000, 3, 4, 12, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	hq.add("1", "plugin.reply", "")
	if res := rec.Check(context.Background(), budgetID, true); len(res.Decision.Start) != 1 {
		t.Fatalf("an officer with no budget was held: %+v", res.Decision)
	}
	if hq.reads != 0 {
		t.Fatalf("an officer with no budget read its spend %d time(s)", hq.reads)
	}
}

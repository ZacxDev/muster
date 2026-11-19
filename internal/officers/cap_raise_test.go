package officers_test

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
)

// THE OWNER'S RAISE OF A DAILY BUDGET FOR ONE UTC DAY, as the wake loop reads
// it (Officer.WithRaisedCap; Reconciler.observe through Backends'
// RaisedCapToday).

// raisingHQ is budgetHQ that also answers today's raise (raisedCapReader).
type raisingHQ struct {
	*budgetHQ
	mu       sync.Mutex
	raised   map[string]float64 // by UTC day
	raiseErr error
}

func (h *raisingHQ) RaisedCapToday(_ context.Context, _ officers.Officer, now time.Time) (float64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.raised[now.UTC().Format("2006-01-02")], h.raiseErr
}

func (h *raisingHQ) set(day time.Time, usd float64, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.raised[day.UTC().Format("2006-01-02")] = usd
	h.raiseErr = err
}

// TestARaiseIsOnlyEverARaise pins WithRaisedCap's rule with values the
// declared budget cannot equal: a raise above it is the day's budget; one at
// or below it changes nothing; an officer with no budget is never given one.
func TestARaiseIsOnlyEverARaise(t *testing.T) {
	o := parseBudgetOfficer(t, budgetDecl) // $10
	for _, c := range []struct {
		usd, want float64
	}{{23.5, 23.5}, {10, 10}, {7, 10}, {0, 10}} {
		if got := o.WithRaisedCap(c.usd).Budget.DailyUSD; got != c.want {
			t.Errorf("$10 budget raised to $%v: $%v, want $%v", c.usd, got, c.want)
		}
	}
	none := o
	none.Budget.DailyUSD = 0
	if got := none.WithRaisedCap(23.5).Budget.DailyUSD; got != 0 {
		t.Errorf("an officer with no budget, raised to $23.5: $%v, want 0 (no budget given)", got)
	}
}

// TestTheWakeLoopWakesARaisedOfficerOnItsDayOnly: $12 of $10 holds a plugin's
// item back; with the owner's raise to $20 for that day the next pass wakes
// the officer for it; an unreadable raise holds it to the declared $10 (fail
// closed); the next UTC day, $12 holds it again — the raise was the day
// before's. The alert names the budget the pass held it to.
func TestTheWakeLoopWakesARaisedOfficerOnItsDayOnly(t *testing.T) {
	o := parseBudgetOfficer(t, budgetDecl)
	hq := &raisingHQ{budgetHQ: &budgetHQ{spent: map[string]float64{}}, raised: map[string]float64{}}
	clk := &clock{t: time.Date(2000, 3, 4, 12, 0, 0, 0, time.UTC)}
	fleet := newFleet()
	logs := &bytes.Buffer{}
	var alerts []officers.BudgetAlert
	rec, err := officers.NewReconciler(officers.Config{Registry: officers.NewRegistry([]officers.Officer{o}), HQ: hq, Fleet: fleet,
		Logger: log.New(logs, "", 0), Now: clk.Now, BudgetAlert: func(_ officers.Officer, a officers.BudgetAlert) { alerts = append(alerts, a) }})
	if err != nil {
		t.Fatal(err)
	}
	check := func() officers.Decision {
		t.Helper()
		res := rec.Check(context.Background(), budgetID, true)
		if res.Err != nil {
			t.Fatalf("pass: %v", res.Err)
		}
		return res.Decision
	}
	day1 := clk.Now()
	hq.setSpent(day1, 12)
	hq.add("1", "plugin.reply", "")
	if d := check(); len(d.Start) != 0 || d.BudgetWaiting != 1 {
		t.Fatalf("control: $12 of $10 woke it: %+v", d)
	}
	if len(alerts) != 1 || alerts[0].Budget != 10 {
		t.Fatalf("the first alert: %+v, want one naming the $10 budget", alerts)
	}

	hq.set(day1, 20, errors.New("fixture: the raise read failed"))
	clk.Advance(time.Minute)
	if d := check(); len(d.Start) != 0 || d.BudgetWaiting != 1 {
		t.Fatalf("an unreadable raise woke it: %+v (want held to the declared budget)", d)
	}
	if !strings.Contains(logs.String(), "reading today's raised cap") {
		t.Fatalf("an unreadable raise was not logged:\n%s", logs)
	}

	hq.set(day1, 20, nil)
	clk.Advance(time.Minute)
	if d := check(); len(d.Start) != 1 || d.Claimable != 1 || d.BudgetWaiting != 0 {
		t.Fatalf("raised to $20 at $12, the pass did not wake it for the item: %+v", d)
	}

	fleet.Stop(context.Background(), officers.Officer{}, 1)
	clk.Advance(24 * time.Hour) // the next UTC day; the raise was day1's
	hq.setSpent(clk.Now(), 12)
	if d := check(); len(d.Start) != 0 || d.BudgetWaiting != 1 {
		t.Fatalf("the next UTC day at $12, it was woken: %+v (the $10 budget is back)", d)
	}
	if n := len(alerts); n != 2 || alerts[1].Budget != 10 {
		t.Fatalf("the next day's alert: %+v, want a second naming $10", alerts)
	}
}

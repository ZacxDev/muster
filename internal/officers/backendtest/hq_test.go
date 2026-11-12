package backendtest_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/backendtest"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
)

// hqWorld is the suite's World over HQ's client (*officers.Client) and the
// hqtest fake: the HQ implementation of the contract.
type hqWorld struct {
	hq  *hqtest.Server
	b   map[string]officers.Backend
	mu  sync.Mutex
	now time.Time
}

func newHQWorld(t *testing.T, fault bool) *hqWorld {
	t.Helper()
	toks := map[string]string{"fixture-conf-a": backendtest.OfficerA, "fixture-conf-b": backendtest.OfficerB}
	hq := hqtest.New(toks)
	t.Cleanup(hq.Close)
	hq.ClaimNext = true
	hq.AllowDoubleClaim = fault
	w := &hqWorld{hq: hq, b: map[string]officers.Backend{}, now: time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)}
	hq.Now = func() time.Time { w.mu.Lock(); defer w.mu.Unlock(); return w.now }
	for tok, id := range toks {
		w.b[id] = officers.NewClient(officers.Officer{ID: id, HQURL: hq.BaseURL(), Owner: 1}.WithToken(tok), nil)
	}
	return w
}

func (w *hqWorld) Backend(officer string) officers.Backend { return w.b[officer] }

func (w *hqWorld) AddItem(officer, kind, subject, tier, priority string) officers.ItemID {
	id := w.hq.AddTieredItem(officer, kind, subject, tier)
	if priority != "" {
		w.hq.SetPriority(id, priority)
	}
	return officers.ItemID(id)
}

func (w *hqWorld) SetBlocked(id officers.ItemID) { w.hq.SetBlocked(string(id), "fixture-task-1") }

func (w *hqWorld) SetEnabled(officer string, on bool) { w.hq.SetDisabled(officer, !on) }

func (w *hqWorld) SetCharter(officer, charter string) { w.hq.Charters[officer] = charter }

func (w *hqWorld) Advance(d time.Duration) { w.mu.Lock(); w.now = w.now.Add(d); w.mu.Unlock() }

// TestHQConforms: HQ's client over the fake keeps every rule of the contract.
func TestHQConforms(t *testing.T) {
	backendtest.Run(t, func(t *testing.T) backendtest.World { return newHQWorld(t, false) })
}

// TestTheSuiteCatchesADoubleClaimOnHQ: with the fault on — the fake lets a
// second instance claim a live lease — the double-claim check FAILS, naming
// the claim it let through; with it off the same check passes. The suite can
// go red against this backend, so its green above means something.
func TestTheSuiteCatchesADoubleClaimOnHQ(t *testing.T) {
	c := backendtest.Find(backendtest.CheckDoubleClaim)
	if err := c.Run(context.Background(), newHQWorld(t, false)); err != nil {
		t.Fatalf("control: the check fails with no fault: %v", err)
	}
	err := c.Run(context.Background(), newHQWorld(t, true))
	if err == nil || !strings.Contains(err.Error(), "a second instance's claim of a held item") {
		t.Fatalf("with a double claim allowed the check answered %v, want it to fail on the second instance's claim", err)
	}
}

package llmproxy

import (
	"io"
	"net/http"
	"reflect"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// A HOLD'S COST (civ-hq docs/OFFICERS.md "Per-hold cost"): every claim of an
// item is one hold, and whatever ends a hold — Ended (complete, release, an
// ask), a claim of another item, HQ's new claim of the same item (its lease
// lapsed), or a wake-loop Sync that no longer shows it — hands that hold's
// cost back ONCE, with HQ's claim number, for HQ to add up. Unlike the
// item's cost (ItemCost), a later claim's hold has a known cost of its own.
// ---------------------------------------------------------------------------

// oneHold is the single HoldCost of ended, or a failure naming what.
func oneHold(t *testing.T, what string, ended []HoldCost) HoldCost {
	t.Helper()
	if len(ended) != 1 {
		t.Fatalf("%s: handed back %d holds %+v, want exactly 1", what, len(ended), ended)
	}
	return ended[0]
}

// wantHold fails unless got is the hold want, its USD to 1e-9.
func wantHold(t *testing.T, what string, got, want HoldCost) {
	t.Helper()
	gotUSD, wantUSD := got.USD, want.USD
	got.USD, want.USD = 0, 0
	if !reflect.DeepEqual(got, want) || !near(gotUSD, wantUSD) {
		got.USD, want.USD = gotUSD, wantUSD
		t.Errorf("%s: hold %+v, want %+v", what, got, want)
	}
}

// TestEachHoldsCostIsHandedBackWhenItEnds: every way a hold ends hands back
// its own cost and HQ's claim number, once.
//   - instance 1, item-a, claim 1: two requests (0.0137, 0.0291); Ended
//     answers 0.0428 for attempt 1.
//   - instance 2, item-b, claim 1: one request (0.0523); HQ's NEW claim of
//     the same item (attempts 3: its lease lapsed) answers the first hold,
//     0.0523 for attempt 1; the next request (0.0419) is the new hold's
//     alone — Ended answers 0.0419 for attempt 3, not 0.0942 — and that
//     hold is not the item's whole cost (not_first_claim).
//   - instance 3, item-c, claim 2 (the judgement claim after a hand-on): one
//     request (0.0613); claiming item-d answers item-c's hold, 0.0613 for
//     attempt 2.
//   - instance 4, item-e, claim 1: one request (0.0271); a Sync showing it
//     holding nothing (and instance 3 still holding item-d) answers 0.0271
//     for attempt 1 alone; a second Sync, nothing.
//   - muster's already-yours answer (attempts 0) for the item instance 1
//     holds ends nothing; nor does Ended of an item not held.
func TestEachHoldsCostIsHandedBackWhenItEnds(t *testing.T) {
	const officer = "items:hold-cost"
	r := newRig(t, officer, costsInOrder(0.0137, 0.0291, 0.0523, 0.0419, 0.0613, 0.0271, 0.0389))
	post := func(n int) {
		t.Helper()
		r.set(func(c *Caller) { c.Instance, c.InstanceLabel = n, "r"+string(rune('0'+n)) })
		if resp, body := r.post(t); resp.StatusCode != http.StatusOK {
			t.Fatalf("request by instance %d: %d %s", n, resp.StatusCode, body)
		}
	}
	const pool = "routine"

	if got := r.p.ClaimedAttempt(officer, pool, 1, "item-a", 1); len(got) != 0 {
		t.Fatalf("instance 1's first claim ended %+v, want none", got)
	}
	post(1) // 0.0137
	if got := r.p.ClaimedAttempt(officer, pool, 1, "item-a", 0); len(got) != 0 {
		t.Fatalf("already-yours (attempts 0) of the held item ended %+v, want none", got)
	}
	post(1) // 0.0291
	if got := r.p.Ended(officer, pool, 1, "item-other"); len(got) != 0 {
		t.Fatalf("Ended of an item not held ended %+v, want none", got)
	}
	wantHold(t, "item-a ended", oneHold(t, "item-a ended", r.p.Ended(officer, pool, 1, "item-a")),
		HoldCost{Pool: pool, Instance: 1, Item: "item-a", Attempt: 1, USD: 0.0428})
	if got := r.p.Ended(officer, pool, 1, "item-a"); len(got) != 0 {
		t.Fatalf("item-a ended twice: the second handed back %+v, want none", got)
	}

	r.p.ClaimedAttempt(officer, pool, 2, "item-b", 1)
	post(2) // 0.0523
	wantHold(t, "item-b re-claimed by HQ (attempts 3)", oneHold(t, "item-b re-claimed", r.p.ClaimedAttempt(officer, pool, 2, "item-b", 3)),
		HoldCost{Pool: pool, Instance: 2, Item: "item-b", Attempt: 1, USD: 0.0523})
	post(2) // 0.0419
	if _, why := r.p.ItemCostWhy(officer, pool, 2, "item-b"); why != CostNotFirstClaim {
		t.Errorf("item-b's third claim as the item's cost: reason %q, want %q", why, CostNotFirstClaim)
	}
	wantHold(t, "item-b's second hold ended", oneHold(t, "item-b ended", r.p.Ended(officer, pool, 2, "item-b")),
		HoldCost{Pool: pool, Instance: 2, Item: "item-b", Attempt: 3, USD: 0.0419})

	r.p.ClaimedAttempt(officer, pool, 3, "item-c", 2)
	post(3) // 0.0613
	wantHold(t, "item-c ended by claiming item-d", oneHold(t, "item-d claimed", r.p.ClaimedAttempt(officer, pool, 3, "item-d", 1)),
		HoldCost{Pool: pool, Instance: 3, Item: "item-c", Attempt: 2, USD: 0.0613})

	r.p.ClaimedAttempt(officer, pool, 4, "item-e", 1)
	post(4) // 0.0271
	wantHold(t, "item-e gone from HQ's claimed list", oneHold(t, "Sync", r.p.Sync(officer, pool, map[int][]string{3: {"item-d"}, 4: nil}, time.Now())),
		HoldCost{Pool: pool, Instance: 4, Item: "item-e", Attempt: 1, USD: 0.0271})
	if got := r.p.Sync(officer, pool, map[int][]string{3: {"item-d"}, 4: nil}, time.Now()); len(got) != 0 {
		t.Fatalf("a second Sync handed back %+v, want none (each hold once)", got)
	}
}

// TestAHoldsCostIsUnknownUnlessThisHoldMeteredAllOfIt: an ended hold whose
// spend the meter did not see whole hands back a reason, never a figure:
// a claim with no attempts, a hold adopted from HQ's claimed list (a
// restart), a hold with no request booked, one with a request booked from
// the proxy's estimate, and one ended while a request admitted under it is
// still streaming. Each reason is the one its case names.
func TestAHoldsCostIsUnknownUnlessThisHoldMeteredAllOfIt(t *testing.T) {
	const officer = "items:hold-unknown"
	const pool = "routine"
	r := newRig(t, officer, streamOK(0.0367))
	post := func(n int) {
		t.Helper()
		r.set(func(c *Caller) { c.Instance, c.InstanceLabel = n, "r"+string(rune('0'+n)) })
		if resp, body := r.post(t); resp.StatusCode != http.StatusOK {
			t.Fatalf("request by instance %d: %d %s", n, resp.StatusCode, body)
		}
	}
	why := func(what string, ended []HoldCost, want string) {
		t.Helper()
		if h := oneHold(t, what, ended); h.Why != want {
			t.Errorf("%s: hold %+v, want reason %q", what, h, want)
		}
	}

	r.p.ClaimedAttempt(officer, pool, 1, "item-noattempts", 0)
	post(1)
	why("a claim with no attempts", r.p.Ended(officer, pool, 1, "item-noattempts"), CostAttemptsUnknown)

	r.p.Sync(officer, pool, map[int][]string{2: {"item-adopted"}}, time.Now())
	post(2)
	why("an adopted hold", r.p.Ended(officer, pool, 2, "item-adopted"), CostAdopted)

	r.p.ClaimedAttempt(officer, pool, 3, "item-none", 1)
	why("a hold with no request", r.p.Ended(officer, pool, 3, "item-none"), CostNoRequests)

	est := newRig(t, officer, streamOK(nil))
	est.p.ClaimedAttempt(officer, pool, 1, "item-estimated", 1)
	if resp, body := est.post(t); resp.StatusCode != http.StatusOK {
		t.Fatalf("the estimated request: %d %s", resp.StatusCode, body)
	}
	why("a hold with an estimated request", est.p.Ended(officer, pool, 1, "item-estimated"), CostEstimated)

	// request 1 (0.0107) booked; request 2 still streaming when the hold ends
	midStream, release := make(chan struct{}), make(chan struct{})
	fl := newRig(t, officer, func(w http.ResponseWriter, _ *http.Request, n int) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i, e := range streamEvents(0.0107) {
			_, _ = io.WriteString(w, e)
			w.(http.Flusher).Flush()
			if n == 2 && i == 0 {
				close(midStream)
				<-release
			}
		}
	})
	fl.p.ClaimedAttempt(officer, pool, 1, "item-inflight", 1)
	if resp, body := fl.post(t); resp.StatusCode != http.StatusOK {
		t.Fatalf("request 1: %d %s", resp.StatusCode, body)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fl.post(t)
	}()
	<-midStream
	ended := fl.p.Ended(officer, pool, 1, "item-inflight")
	close(release)
	<-done
	why("a hold ended with a request in flight", ended, CostInFlight)

	// the positive control: the same rig's first claim, metered whole
	r.p.ClaimedAttempt(officer, pool, 4, "item-known", 1)
	post(4)
	if h := oneHold(t, "the positive control", r.p.Ended(officer, pool, 4, "item-known")); h.Why != "" || !near(h.USD, 0.0367) || h.Attempt != 1 {
		t.Errorf("the positive control: hold %+v, want 0.0367 known, attempt 1", h)
	}
}

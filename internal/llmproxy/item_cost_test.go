package llmproxy

import (
	"io"
	"math"
	"net/http"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// AN ITEM'S COST (civ-hq docs/OFFICERS.md "Item cost"): what the routine
// instance's hold on the item booked through the proxy, request by request,
// as the provider reported it. officer_complete sends it to HQ as cost_usd
// with cost_from muster:llm-proxy. It is KNOWN only when this proxy metered
// the whole of the item's proxy work: HQ's first claim of it (attempts 1),
// claimed through muster, and at least one request booked. Anything else is
// unknown, and an unknown cost is never a 0.
// ---------------------------------------------------------------------------

// costsInOrder answers request n (1-based) with the stream whose provider
// cost is costs[n-1].
func costsInOrder(costs ...float64) func(http.ResponseWriter, *http.Request, int) {
	return func(w http.ResponseWriter, _ *http.Request, n int) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range streamEvents(costs[n-1]) {
			_, _ = io.WriteString(w, e)
			w.(http.Flusher).Flush()
		}
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestAnItemsCostIsTheSumOfItsOwnRequestsOnly: instance 1 holds item-a and
// makes two requests (0.0137, 0.0291); instance 2, holding nothing, makes one
// (0.0523), then claims item-b and makes one (0.0419). item-a's cost is 0.0428
// — its own two requests, not instance 2's — and item-b's is 0.0419, not the
// request instance 2 made before it held it. Neither instance knows a cost for
// the item the other holds.
func TestAnItemsCostIsTheSumOfItsOwnRequestsOnly(t *testing.T) {
	const officer = "items:cost-sum"
	r := newRig(t, officer, costsInOrder(0.0137, 0.0523, 0.0291, 0.0419))
	as := func(n int) { r.set(func(c *Caller) { c.Instance, c.InstanceLabel = n, "r"+string(rune('0'+n)) }) }
	post := func() {
		t.Helper()
		if resp, body := r.post(t); resp.StatusCode != http.StatusOK {
			t.Fatalf("request: %d %s", resp.StatusCode, body)
		}
	}

	r.p.ClaimedAttempt(officer, 1, "item-a", 1)
	as(1)
	post() // 0.0137 → item-a
	as(2)
	post() // 0.0523 → no item (instance 2 holds none)
	as(1)
	post() // 0.0291 → item-a
	r.p.ClaimedAttempt(officer, 2, "item-b", 1)
	as(2)
	post() // 0.0419 → item-b

	if got, ok := r.p.ItemCost(officer, 1, "item-a"); !ok || !near(got, 0.0428) {
		t.Errorf("item-a's cost = %v (known %v), want 0.0428: its own two requests", got, ok)
	}
	if got, ok := r.p.ItemCost(officer, 2, "item-b"); !ok || !near(got, 0.0419) {
		t.Errorf("item-b's cost = %v (known %v), want 0.0419: its own request, not the one made holding nothing", got, ok)
	}
	if got, ok := r.p.ItemCost(officer, 1, "item-b"); ok {
		t.Errorf("instance 1 (holding item-a) reports a cost %v for item-b", got)
	}
	if got, ok := r.p.ItemCost(officer, 2, "item-a"); ok {
		t.Errorf("instance 2 (holding item-b) reports a cost %v for item-a", got)
	}
	if got, ok := r.p.ItemCost("items:another-officer", 1, "item-a"); ok {
		t.Errorf("another officer's instance 1 reports a cost %v for item-a", got)
	}
}

// TestAnItemsCostIsNotChargedARequestAdmittedUnderAnotherHold: a request
// admitted while instance 1 held item-a (0.0613) is still streaming when the
// hold changes — item-a ends and item-b is claimed. Its cost is charged to
// neither: not to item-b, whose cost is its own one request (0.0419).
func TestAnItemsCostIsNotChargedARequestAdmittedUnderAnotherHold(t *testing.T) {
	const officer = "items:cost-handover"
	var r *rig
	costs := []float64{0.0613, 0.0419}
	r = newRig(t, officer, func(w http.ResponseWriter, req *http.Request, n int) {
		if n == 1 { // mid-request: the hold changes under it
			r.p.Ended(officer, 1, "item-a")
			r.p.ClaimedAttempt(officer, 1, "item-b", 1)
		}
		costsInOrder(costs...)(w, req, n)
	})
	r.p.ClaimedAttempt(officer, 1, "item-a", 1)
	for i := 0; i < 2; i++ {
		if resp, body := r.post(t); resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: %d %s", i+1, resp.StatusCode, body)
		}
	}
	if got, ok := r.p.ItemCost(officer, 1, "item-b"); !ok || !near(got, 0.0419) {
		t.Errorf("item-b's cost = %v (known %v), want 0.0419: not the request admitted under item-a", got, ok)
	}
}

// TestAnItemsCostIsUnknownWhileARequestUnderItIsInFlight: instance 1 holds
// item-a; request 1 (0.0107) is booked, and request 2 (0.4993), admitted
// under item-a, is still streaming. Read now, the cost is UNKNOWN — not the
// 0.0107 booked so far, which officer_complete would send as the item's
// whole cost and HQ would store as known. Once request 2 is booked, it is
// known: 0.51.
func TestAnItemsCostIsUnknownWhileARequestUnderItIsInFlight(t *testing.T) {
	const officer = "items:cost-inflight"
	midStream, release := make(chan struct{}), make(chan struct{})
	r := newRig(t, officer, func(w http.ResponseWriter, _ *http.Request, n int) {
		w.Header().Set("Content-Type", "text/event-stream")
		cost := 0.0107
		if n == 2 {
			cost = 0.4993
		}
		events := streamEvents(cost)
		for i, e := range events {
			_, _ = io.WriteString(w, e)
			w.(http.Flusher).Flush()
			if n == 2 && i == 0 { // message_start sent: the request is in flight
				close(midStream)
				<-release
			}
		}
	})
	r.p.ClaimedAttempt(officer, 1, "item-a", 1)
	if resp, body := r.post(t); resp.StatusCode != http.StatusOK {
		t.Fatalf("request 1: %d %s", resp.StatusCode, body)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.post(t)
	}()
	<-midStream
	got, ok := r.p.ItemCost(officer, 1, "item-a")
	close(release)
	<-done
	if ok {
		t.Errorf("with request 2 in flight: cost %v reported known, want unknown (request 2 is not booked yet)", got)
	}
	if got, ok := r.p.ItemCost(officer, 1, "item-a"); !ok || !near(got, 0.51) {
		t.Errorf("after request 2 is booked: cost %v (known %v), want 0.51", got, ok)
	}
}

// TestAnItemsCostIsUnknownUnlessThisHoldMeteredAllOfIt: each case is a hold
// whose booked requests are not provably the item's whole proxy spend — so
// its cost is unknown, never 0 and never the partial sum.
func TestAnItemsCostIsUnknownUnlessThisHoldMeteredAllOfIt(t *testing.T) {
	const officer = "items:cost-unknown"
	r := newRig(t, officer, streamOK(0.0367))
	as := func(n int) { r.set(func(c *Caller) { c.Instance, c.InstanceLabel = n, "r"+string(rune('0'+n)) }) }
	post := func(n int) {
		t.Helper()
		as(n)
		if resp, body := r.post(t); resp.StatusCode != http.StatusOK {
			t.Fatalf("request by instance %d: %d %s", n, resp.StatusCode, body)
		}
	}
	unknown := func(why string, n int, item string) {
		t.Helper()
		if got, ok := r.p.ItemCost(officer, n, item); ok {
			t.Errorf("%s: cost %v reported known, want unknown", why, got)
		}
	}

	// Held, first claim, but no request booked yet: no evidence, not a 0.
	r.p.ClaimedAttempt(officer, 1, "item-none", 1)
	unknown("a first claim with no request booked", 1, "item-none")

	// HQ's second claim of the item: an earlier hold may have spent on it.
	r.p.ClaimedAttempt(officer, 2, "item-second", 2)
	post(2)
	unknown("HQ's second claim (attempts 2)", 2, "item-second")

	// No attempts from HQ (an HQ without the field, or muster's already-yours
	// answer for an item the meter did not hold).
	r.p.ClaimedAttempt(officer, 3, "item-noattempts", 0)
	post(3)
	unknown("a claim with no attempts", 3, "item-noattempts")

	// Adopted from a wake-loop read (a claim muster did not see: a restart).
	r.p.Sync(officer, map[int][]string{4: {"item-adopted"}}, time.Now())
	post(4)
	unknown("an item adopted from HQ's claimed list", 4, "item-adopted")

	// A first claim, then HQ's re-claim of it (its lease lapsed): unknown.
	r.p.ClaimedAttempt(officer, 5, "item-reclaimed", 1)
	post(5)
	r.p.ClaimedAttempt(officer, 5, "item-reclaimed", 2)
	unknown("a first claim re-claimed after a lapse (attempts 2)", 5, "item-reclaimed")

	// Ended: the meter no longer holds it.
	r.p.ClaimedAttempt(officer, 6, "item-ended", 1)
	post(6)
	r.p.Ended(officer, 6, "item-ended")
	unknown("an ended hold", 6, "item-ended")

	// A request the provider reported no cost for is booked from the proxy's
	// pessimistic estimate: not a measured cost, so the item's is unknown.
	est := newRig(t, officer, streamOK(nil))
	est.p.ClaimedAttempt(officer, 1, "item-estimated", 1)
	if resp, body := est.post(t); resp.StatusCode != http.StatusOK {
		t.Fatalf("the estimated request: %d %s", resp.StatusCode, body)
	}
	if got, ok := est.p.ItemCost(officer, 1, "item-estimated"); ok {
		t.Errorf("a hold with a request booked from the estimate: cost %v reported known, want unknown", got)
	}
	// The positive control, with the same requests: a first claim, metered
	// whole, is known — and stays known through muster's already-yours
	// re-claim of the item it holds (no new HQ claim, attempts unknown).
	r.p.ClaimedAttempt(officer, 7, "item-known", 1)
	post(7)
	post(7)
	r.p.ClaimedAttempt(officer, 7, "item-known", 0)
	if got, ok := r.p.ItemCost(officer, 7, "item-known"); !ok || !near(got, 2*0.0367) {
		t.Errorf("the positive control: cost %v (known %v), want 0.0734", got, ok)
	}
}

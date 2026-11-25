package api

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/push"
)

// A PAIRING REQUEST'S NOTIFICATION GOES WHEN THE REQUEST STOPS WAITING
// (plugin_pairing.go pairSettled, withdrawExpiredPairs; docs/push.md "When a
// notification is withdrawn"): approved or denied on /plugins, or expired
// undecided — noticed by the Attention sweep, not only when a page reads it,
// and withdrawn once. Names only identifiers that exist before the change.

// pairNotes is user 1's pushes under request id's tag: shown, withdrawn.
func (w *outboundWorld) pairNotes(id int64) (shown, withdrawn int) {
	w.srv.WaitForPushes()
	for _, p := range w.pusher.to(retryEP) {
		if p.Tag != "pair-"+strconv.FormatInt(id, 10) {
			continue
		}
		if p.Type == push.TypeResolved {
			withdrawn++
		} else {
			shown++
		}
	}
	return shown, withdrawn
}

// sweepUntil runs the Attention sweep (its first pass; the next is an hour
// off) until done reports true or 5 s pass, then stops it.
func (w *outboundWorld) sweepUntil(done func() bool) {
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		w.srv.RunAttentionSweep(ctx, time.Hour)
	}()
	for deadline := time.Now().Add(5 * time.Second); !done() && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-stopped
	w.srv.WaitForPushes()
}

// TestADecidedOrExpiredPairingRequestWithdrawsItsNotification: approved,
// denied, or expired undecided — each request's push is withdrawn exactly
// once, and a request still pending keeps it through the sweep.
//
// Red at origin/main: no withdrawal for a pairing request; an expired one was
// noticed only by a page's read, which sends nothing.
func TestADecidedOrExpiredPairingRequestWithdrawsItsNotification(t *testing.T) {
	w := newOutboundWorldWith(t, true, outboundFixture{pushes: true})
	approved, _ := w.pair(pairBody)
	denied, _ := w.pair(strings.Replace(pairBody, "example-paired", "example-other-pair", 1))
	lapsed, _ := w.pair(strings.Replace(pairBody, "example-paired", "example-late", 1))
	pending, _ := w.pair(strings.Replace(pairBody, "example-paired", "example-mint", 1))
	ids := map[string]int64{"approved": w.pairID(approved.RequestID), "denied": w.pairID(denied.RequestID),
		"expired": w.pairID(lapsed.RequestID), "pending": w.pairID(pending.RequestID)}
	for name, id := range ids {
		if shown, gone := w.pairNotes(id); shown != 1 || gone != 0 {
			t.Fatalf("CONTROL: the %s request's pushes: %d shown, %d withdrawn; want 1, 0", name, shown, gone)
		}
	}
	if rec := w.approvePair(ids["approved"], approved.Code); rec.Code != 303 {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	if rec := w.do("POST", "/plugins/pair/"+strconv.FormatInt(ids["denied"], 10)+"/deny", url.Values{}.Encode(), w.operatorForm); rec.Code != 303 {
		t.Fatalf("deny: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := w.f.pool.Exec(authz.System(w.ctx), `UPDATE plugin_pair_requests SET expires_at = now() - interval '1 second' WHERE id = $1`, ids["expired"]); err != nil {
		t.Fatal(err)
	}
	expired := func() bool { _, gone := w.pairNotes(ids["expired"]); return gone > 0 }
	// Two sweeps: the second must not withdraw it again.
	w.sweepUntil(expired)
	w.sweepUntil(func() bool { return false })
	for name, want := range map[string]int{"approved": 1, "denied": 1, "expired": 1, "pending": 0} {
		if _, gone := w.pairNotes(ids[name]); gone != want {
			t.Errorf("the %s request: %d withdrawals, want %d", name, gone, want)
		}
	}
}

package builtin_test

import (
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers/backendtest"
	"github.com/ZacxDev/muster/internal/officers/builtin"
)

// TestAFutureTimeIsNotANewerEngagement: an engagement the plugin dates in
// the future is recorded at muster's time, so it can never stand for an
// engagement "since" an outreach made after it arrived.
func TestAFutureTimeIsNotANewerEngagement(t *testing.T) {
	w := newPGWorld(t, false)
	tx, err := w.store.Pool().Begin(ctxOp())
	if err != nil {
		t.Fatal(err)
	}
	future := w.clock().Add(365 * 24 * time.Hour)
	res, err := w.store.RecordEngagementTx(ctxOp(), tx, 1, backendtest.OfficerA, cVenue, builtin.Person{Handle: "wes"},
		builtin.Engagement{Kind: builtin.EventReply, Ref: "r1", OurPostRef: "post-1", At: &future, Text: "q?"}, builtin.EngagementSource{PluginID: 1}, w.clock())
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctxOp()); err != nil {
		t.Fatal(err)
	}
	w.advance(time.Hour)
	w.sent(res.Contact.ID, 9301, builtin.TargetDM)
	w.advance(time.Hour)
	_, err = w.guard(builtin.Outreach{Kind: builtin.TargetFollow, Handle: "wes", BecauseRef: "r1"})
	refusedWith(t, "a future-dated engagement after an outreach", err, builtin.RefuseTwiceNoEngage)
}

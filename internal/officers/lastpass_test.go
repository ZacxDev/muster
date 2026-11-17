package officers_test

import (
	"context"
	"testing"
	"time"
)

// TestLastPassIsTheLastPassThisProcessRan (officers O-C: the officer page's
// "last pass"): nothing before the first pass; then that pass's time, what it
// saw and started; a failed pass's failure and its count; and a pass skipped
// in backoff (it read nothing) changes nothing. The fixture's clock is not
// the zero time and the counts are distinct, so a recorder that kept a
// constant could not pass.
func TestLastPassIsTheLastPassThisProcessRan(t *testing.T) {
	r := newRig(t, 2, 30*time.Minute)
	if _, ok := r.rec.LastPass(relID); ok {
		t.Fatal("a last pass before any pass ran")
	}
	r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	r.hq.AddItem(relID, "release.evaluate", "svc:v2")
	r.hq.AddItem(relID, "release.evaluate", "svc:v3")
	r.clk.Advance(7 * time.Minute)
	first := r.clk.Now()
	if res := r.check(t); res.Err != nil {
		t.Fatal(res.Err)
	}
	lp, ok := r.rec.LastPass(relID)
	if !ok || !lp.At.Equal(first) || lp.Err != "" || lp.Failures != 0 || lp.Claimable != 3 || lp.Started != 2 {
		t.Fatalf("after a pass: %+v, %v; want at %s, no error, 3 claimable, 2 started", lp, ok, first)
	}
	r.fleet.blind = true
	r.clk.Advance(time.Minute)
	failed := r.clk.Now()
	if res := r.check(t); res.Err == nil {
		t.Fatal("control: a blind fleet's pass did not fail")
	}
	lp, _ = r.rec.LastPass(relID)
	if !lp.At.Equal(failed) || lp.Err == "" || lp.Failures != 1 || lp.Claimable != 0 || lp.Started != 0 {
		t.Fatalf("after a failed pass: %+v; want at %s, its error, 1 failure, nothing seen or started", lp, failed)
	}
	// In backoff a pass that is not forced reads nothing: not a pass.
	r.clk.Advance(time.Second)
	if res := r.rec.Check(context.Background(), relID, false); !res.Skipped {
		t.Fatalf("control: the pass in backoff was not skipped: %+v", res)
	}
	if again, _ := r.rec.LastPass(relID); !again.At.Equal(failed) {
		t.Fatalf("a skipped pass moved the last pass to %s", again.At)
	}
}

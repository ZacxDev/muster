package sessionstate

import (
	"testing"
	"time"
)

// TestStateOfPrecedence: needs_you > stalled > working, and an answerable
// wait is needs_you with NO episode (the ask is the owner's item).
func TestStateOfPrecedence(t *testing.T) {
	for _, c := range []struct {
		name    string
		f       Facts
		state   string
		episode bool
	}{
		{"a waiting window", Facts{Waiting: true}, StateNeedsYou, true},
		{"an answerable wait", Facts{Waiting: true, Answerable: true}, StateNeedsYou, false},
		{"a stall the classifier says asked", Facts{Stalled: true, AskStall: true}, StateNeedsYou, true},
		{"a stall", Facts{Stalled: true}, StateStalled, true},
		{"waiting beats a stall", Facts{Waiting: true, Stalled: true}, StateNeedsYou, true},
		{"an ask score alone is not a stall", Facts{AskStall: true}, StateWorking, false},
		{"nothing", Facts{}, StateWorking, false},
	} {
		if s, e := StateOf(c.f); s != c.state || e != c.episode {
			t.Errorf("%s: %s %v, want %s %v", c.name, s, e, c.state, c.episode)
		}
	}
}

// TestAskedNeverQuietsAFailure: a classifier that did not run or failed
// counts as asked; only a clean "did not ask" does not.
func TestAskedNeverQuietsAFailure(t *testing.T) {
	if !Asked(false, "", false) {
		t.Error("not classified must count as asked")
	}
	if !Asked(true, "http_500", false) {
		t.Error("a failed classification must count as asked")
	}
	if !Asked(true, "", true) {
		t.Error("an ask stall must count as asked")
	}
	if Asked(true, "", false) {
		t.Error("a clean 'did not ask' counted as asked")
	}
}

// TestAdvanceEndsOnlyOnSustainedWork: a blip shorter than EndAfter leaves
// the episode open and is forgotten; work seen for EndAfter ends it, while it
// still works or when it stops; the run is measured between sightings, never
// to now. Fixture steps overshoot and undershoot EndAfter by 7 s and 11 s,
// never landing on it.
func TestAdvanceEndsOnlyOnSustainedWork(t *testing.T) {
	t0 := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	var zero time.Time
	st := Advance(true, zero, zero, t0)
	if st.End || !st.BusySince.Equal(t0) || !st.BusySeen.Equal(t0) {
		t.Fatalf("first sighting: %+v", st)
	}
	short := t0.Add(EndAfter - 11*time.Second)
	st = Advance(true, t0, t0, short)
	if st.End || !st.BusySince.Equal(t0) || !st.BusySeen.Equal(short) {
		t.Fatalf("still working, under EndAfter: %+v", st)
	}
	// It stopped after the short run, and is seen idle long after: the idle
	// time is not work.
	st = Advance(false, t0, short, t0.Add(time.Hour))
	if st.End || !st.BusySince.IsZero() || !st.BusySeen.IsZero() {
		t.Fatalf("a short run then idle: %+v", st)
	}
	long := t0.Add(EndAfter + 7*time.Second)
	if st = Advance(true, t0, short, long); !st.End || st.Reason != EndWorked {
		t.Fatalf("working past EndAfter: %+v", st)
	}
	if st = Advance(false, t0, long, long.Add(time.Hour)); !st.End || st.Reason != EndWorked {
		t.Fatalf("stopped after a run past EndAfter: %+v", st)
	}
	if st = Advance(false, zero, zero, long); st.End || !st.BusySince.IsZero() {
		t.Fatalf("idle and never seen working: %+v", st)
	}
}

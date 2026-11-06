package sessionstate

import (
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/chiefnudge"
)

// TestOwnerActedEndsOnTheOwnersOwnNewerWord: the owner acting in the session
// after the episode opened ends it — their own message strictly after the
// opening, or a waiting episode's prompt answered — and nothing else does: an
// older message (the one that preceded the stop), a muster nudge or paste, an
// untimed message, a session waiting now, an unknown status, or no read. The
// owner's message lands 1 s after the opening and 1 s before it, never on it,
// and the "at the opening" case is pinned on its own.
func TestOwnerActedEndsOnTheOwnersOwnNewerWord(t *testing.T) {
	opened := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	nudge := chiefnudge.NudgeLabel + " keep going until merged"
	const pasted = "please rebase onto main and push"
	assistant := chiefnudge.Msg{Role: "assistant", Text: "CI is green. Shall I merge it?"}
	owner := func(text string, at time.Time, id string) chiefnudge.Msg {
		return chiefnudge.Msg{Role: "user", Text: text, ID: id, At: at}
	}
	before := owner("merge it when green", opened.Add(-time.Second), "line:100")
	after := owner("yes, merge it", opened.Add(time.Second), "line:900")
	base := OwnerFacts{OpenedAt: opened, Known: true, Prefixes: []string{chiefnudge.NudgeLabel}, Injected: []string{pasted}}
	with := func(mod func(*OwnerFacts)) OwnerFacts {
		f := base
		mod(&f)
		return f
	}
	for _, c := range []struct {
		name string
		f    OwnerFacts
		want bool
	}{
		{"an owner message after the opening", with(func(f *OwnerFacts) {
			f.Msgs = []chiefnudge.Msg{before, assistant, after}
		}), true},
		{"an owner message after the opening, the session already replied", with(func(f *OwnerFacts) {
			f.Msgs = []chiefnudge.Msg{before, assistant, after, assistant}
		}), true},
		{"the owner's message before the opening (owner_last)", with(func(f *OwnerFacts) {
			f.Msgs = []chiefnudge.Msg{assistant, before}
		}), false},
		{"the owner's message exactly at the opening", with(func(f *OwnerFacts) {
			f.Msgs = []chiefnudge.Msg{assistant, owner("now", opened, "line:500")}
		}), false},
		{"a chief nudge after the opening", with(func(f *OwnerFacts) {
			f.Msgs = []chiefnudge.Msg{before, assistant, owner(nudge, opened.Add(time.Minute), "line:900")}
		}), false},
		{"a message muster pasted after the opening", with(func(f *OwnerFacts) {
			f.Msgs = []chiefnudge.Msg{before, assistant, owner(pasted, opened.Add(time.Minute), "line:900")}
		}), false},
		{"an untimed owner message", with(func(f *OwnerFacts) {
			f.Msgs = []chiefnudge.Msg{before, assistant, owner("yes", time.Time{}, "line:900")}
		}), false},
		{"an owner message after the opening, the session waiting now", with(func(f *OwnerFacts) {
			f.Msgs, f.WaitingNow = []chiefnudge.Msg{after}, true
		}), false},
		{"an owner message after the opening, the status unknown", with(func(f *OwnerFacts) {
			f.Msgs, f.Known = []chiefnudge.Msg{after}, false
		}), false},
		{"no read", base, false},
		{"a waiting episode whose prompt was answered", with(func(f *OwnerFacts) { f.WasWaiting = true }), true},
		{"a waiting episode still waiting", with(func(f *OwnerFacts) { f.WasWaiting, f.WaitingNow = true, true }), false},
		{"a waiting episode, the status unknown", with(func(f *OwnerFacts) { f.WasWaiting, f.Known = true, false }), false},
	} {
		if got := OwnerActed(c.f); got != c.want {
			t.Errorf("%s: OwnerActed = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestAShortBlipWithNoOwnerActionStillLeavesTheEpisodeOpen: the rule the
// owner-acted end sits beside is unchanged — a blip under EndAfter that the
// owner did not start keeps the episode open, and 120 s of work still ends it
// "worked".
func TestAShortBlipWithNoOwnerActionStillLeavesTheEpisodeOpen(t *testing.T) {
	t0 := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	blip := t0.Add(40 * time.Second)
	if OwnerActed(OwnerFacts{OpenedAt: t0.Add(-time.Hour), Known: true}) {
		t.Fatal("no owner action ended it")
	}
	if st := Advance(false, t0, blip, t0.Add(10*time.Minute)); st.End {
		t.Fatalf("a 40 s blip ended it: %+v", st)
	}
	if st := Advance(true, t0, t0, t0.Add(EndAfter+13*time.Second)); !st.End || st.Reason != EndWorked {
		t.Fatalf("133 s of work: %+v, want ended %q", st, EndWorked)
	}
}

// TestStateOfPrecedence: needs_you > stalled > working; every wait is an
// episode, an answerable ask's too (its push waits the grace like any other).
func TestStateOfPrecedence(t *testing.T) {
	for _, c := range []struct {
		name    string
		f       Facts
		state   string
		episode bool
	}{
		{"a waiting window", Facts{Waiting: true}, StateNeedsYou, true},
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

// TestHandledIsViewedAndResetOnly pins Handled's set, spelled as the column
// holds it: 'owner' is NOT handled (the streak an 'owner' end carries can be
// the next stop's — round-1 F1), and neither is an outgrown stop.
func TestHandledIsViewedAndResetOnly(t *testing.T) {
	for reason, want := range map[string]bool{"": false, "worked": false, "gone": false, "owner": false, "viewed": true, "reset": true, "other": false} {
		if got := Handled(reason); got != want {
			t.Errorf("Handled(%q) = %v, want %v", reason, got, want)
		}
	}
}

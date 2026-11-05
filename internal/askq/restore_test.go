package askq

import (
	"errors"
	"testing"
	"time"
)

// A QUESTION READ OPEN IN ITS TRANSCRIPT (Restore): registered whatever the
// store remembers — idempotently, never reopening one the CLI ended,
// reopening one that expired.

// TestRestoreRegistersWhatAFreshStoreLost: a restarted muster's store is
// empty; reading the call open restores the question, Pending and answerable.
func TestRestoreRegistersWhatAFreshStoreLost(t *testing.T) {
	s, _ := testStore(t)
	e, changed, err := s.Restore(key(1), twoQs)
	if err != nil || !changed || e.State != Pending || !ValidID(e.ID) {
		t.Fatalf("restore into an empty store: %+v %v %v", e, changed, err)
	}
	if _, again, err := s.BeginTyping(key(1), someAnswers); err != nil || again {
		t.Fatalf("a restored question is not answerable: %v %v", again, err)
	}
}

// TestRestoreIsIdempotent: restored again, and registered by the hook before
// or after, it is ONE entry — the same id — and nothing fires for a repeat.
func TestRestoreIsIdempotent(t *testing.T) {
	s, _ := testStore(t)
	fired := 0
	s.OnChange(func(Entry) { fired++ })
	hook, err := s.Open(key(1), twoQs, NewID())
	if err != nil {
		t.Fatal(err)
	}
	fired = 0
	for range 3 {
		e, changed, err := s.Restore(key(1), twoQs)
		if err != nil || changed || e.ID != hook.ID {
			t.Fatalf("restoring a hook's open question: %+v %v %v", e, changed, err)
		}
	}
	if fired != 0 {
		t.Fatalf("a repeated restore fired %d changes", fired)
	}
	// Restored first, then the hook's registration (a hook slower than
	// muster's scan): the hook's Open returns the same entry.
	k2 := key(1)
	k2.ToolUseID = "toolu_2"
	a, _, _ := s.Restore(k2, twoQs)
	if h, err := s.Open(k2, twoQs, NewID()); err != nil || h.ID != a.ID {
		t.Fatalf("the hook after a restore: %+v %v (restored %s)", h, err, a.ID)
	}
	if got := s.ForSession(1, "mirror", "laptop", "s-1", time.Hour); len(got) != 2 {
		t.Fatalf("%d entries for two calls: %+v", len(got), got)
	}
}

// TestRestoreLeavesAnAnswerBeingTyped: typing is not disturbed.
func TestRestoreLeavesAnAnswerBeingTyped(t *testing.T) {
	s, _ := testStore(t)
	_, _ = s.Open(key(1), twoQs, "")
	if _, _, err := s.BeginTyping(key(1), someAnswers); err != nil {
		t.Fatal(err)
	}
	if e, changed, _ := s.Restore(key(1), twoQs); changed || e.State != Typing {
		t.Fatalf("a restore disturbed the typing: %+v %v", e, changed)
	}
}

// TestRestoreNeverReopensWhatTheCLIEnded: answered, dismissed, or replaced by
// a newer question — a late or stale restore leaves it as it is.
func TestRestoreNeverReopensWhatTheCLIEnded(t *testing.T) {
	for _, c := range []struct {
		name string
		end  func(s *Store)
		want State
	}{
		{"answered", func(s *Store) { resolve(s, 1, someAnswers) }, Answered},
		{"dismissed", func(s *Store) { resolve(s, 1, nil) }, Declined},
		{"superseded", func(s *Store) {
			k := key(1)
			k.ToolUseID = "toolu_newer"
			_, _ = s.Open(k, twoQs, "")
		}, Superseded},
	} {
		s, _ := testStore(t)
		first, _ := s.Open(key(1), twoQs, "")
		c.end(s)
		e, changed, err := s.Restore(key(1), twoQs)
		if err != nil || changed || e.ID != first.ID || e.State != c.want {
			t.Errorf("%s: restored again: %+v %v %v", c.name, e, changed, err)
		}
		if got, _ := s.Find(key(1)); got.State != c.want {
			t.Errorf("%s: the stored state became %s", c.name, got.State)
		}
	}
}

// TestRestoreReopensAnExpiredQuestion: its session looked gone (Gone) — the
// transcript says the picker is there: Pending again under the same id, its note
// gone, and the CLI's record still resolves it.
func TestRestoreReopensAnExpiredQuestion(t *testing.T) {
	s, _ := testStore(t)
	first, _ := s.Open(key(1), twoQs, "")
	if n := s.Gone(1, "mirror", "laptop", "s-1", "gone"); n != 1 {
		t.Fatalf("gone expired %d", n)
	}
	e, changed, err := s.Restore(key(1), twoQs)
	if err != nil || !changed || e.ID != first.ID || e.State != Pending || e.Note != "" || !e.Resolved.IsZero() {
		t.Fatalf("an expired question restored open: %+v %v %v", e, changed, err)
	}
	if got := s.Pending(1); len(got) != 1 || got[0].ID != first.ID {
		t.Fatalf("pending after the reopen: %+v", got)
	}
	// Reopened, it still resolves from the CLI's record.
	if got, ok := resolve(s, 1, someAnswers); !ok || got.State != Answered {
		t.Fatalf("the reopened question did not resolve: %+v %v", got, ok)
	}
}

// TestRestoreKeepsThePerOwnerCap: the cap on open questions holds for a
// restore as for the hook.
func TestRestoreKeepsThePerOwnerCap(t *testing.T) {
	s, _ := testStore(t)
	for i := range MaxPendingPerOwner {
		k := key(1)
		k.Session, k.ToolUseID = "s-"+string(rune('a'+i%26))+string(rune('a'+i/26)), "toolu_1"
		if _, _, err := s.Restore(k, twoQs); err != nil {
			t.Fatalf("restore %d: %v", i, err)
		}
	}
	if _, _, err := s.Restore(key(1), twoQs); !errors.Is(err, ErrFull) {
		t.Fatalf("past the cap: %v", err)
	}
}

// TestASeenSessionsQuestionNeverAgesOut: MaxOpen counts only time the session
// was NOT seen listed. Marked seen, a question stays open far past MaxOpen;
// the control — the same question, never seen — expires past it.
func TestASeenSessionsQuestionNeverAgesOut(t *testing.T) {
	s, c := testStore(t)
	_, _ = s.Open(key(1), twoQs, "")
	other := key(1)
	other.Session = "s-unseen"
	_, _ = s.Open(other, twoQs, "")
	for range 4 { // 4 × (MaxOpen - 1 min): far past MaxOpen in all
		c.add(MaxOpen - time.Minute)
		s.Seen(1, "mirror", "laptop", "s-1")
	}
	s.Sweep()
	if got, _ := s.Find(key(1)); got.State != Pending {
		t.Fatalf("a seen session's question aged out: %s", got.State)
	}
	if got, _ := s.Find(other); got.State != Expired {
		t.Fatalf("the never-seen control is %s, want expired", got.State)
	}
	// No longer seen: it expires MaxOpen after it last was.
	c.add(MaxOpen + time.Minute)
	s.Sweep()
	if got, _ := s.Find(key(1)); got.State != Expired {
		t.Fatalf("unseen for MaxOpen: %s", got.State)
	}
}

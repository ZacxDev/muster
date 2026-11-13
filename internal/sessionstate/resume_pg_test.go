package sessionstate

import (
	"slices"
	"testing"
	"time"
)

// TestAStopBackFromGoneOnItsStreakKeepsItsPush (regression): PG.Open's resume
// rule (resumedReasons). When a session's LATEST episode ended 'gone' on the streak
// being opened, the new episode is written carrying that episode's pushed_at —
// so the stop's one push claim refuses it (ClaimPush false) and it is never
// pushed a second time. Every other shape carries nothing: a stop that ended
// gone unpushed (the hold delays, it never silences), a new streak (the
// session ran), another end reason, an empty streak, another session key,
// another user, and a gone stop that is not the latest.
func TestAStopBackFromGoneOnItsStreakKeepsItsPush(t *testing.T) {
	p := pgStore(t)
	// Distinct from every other time in the test, and from now: a carried
	// pushed_at can only be this one.
	pushedAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	ep := func(key, streak string) Episode {
		e := stalledEp(key)
		e.StreakKey = streak
		return e
	}
	// stop opens key on streak as user uid, pushes it when pushed, and ends it
	// for reason.
	stop := func(t *testing.T, uid int64, key, streak string, pushed bool, reason string) Episode {
		t.Helper()
		ctx := asUser(uid)
		e, claimed, err := p.Open(ctx, ep(key, streak))
		if err != nil || !claimed {
			t.Fatalf("open %s: claimed=%v err=%v", key, claimed, err)
		}
		if pushed {
			if ok, err := p.ClaimPush(ctx, e.ID, pushedAt); err != nil || !ok {
				t.Fatalf("claim %s: %v %v", key, ok, err)
			}
		}
		if ended, err := p.End(ctx, e.ID, reason, pushedAt.Add(time.Hour)); err != nil || !ended {
			t.Fatalf("end %s: %v %v", key, ended, err)
		}
		return e
	}
	reopen := func(t *testing.T, uid int64, key, streak string) Episode {
		t.Helper()
		e, claimed, err := p.Open(asUser(uid), ep(key, streak))
		if err != nil || !claimed {
			t.Fatalf("reopen %s: claimed=%v err=%v", key, claimed, err)
		}
		return e
	}

	t.Run("pushed, gone, back on its streak: carried, and never claimed again", func(t *testing.T) {
		first := stop(t, 2, "k-carry", "since:111", true, EndGone)
		back := reopen(t, 2, "k-carry", "since:111")
		if back.Seq != first.Seq+1 || !back.Open() || !back.PushedAt.Equal(pushedAt) || back.Quiet != "" || back.ResumedFrom != first.ID {
			t.Fatalf("the stop back from gone: %+v, want seq %d carrying pushed_at %v, resumed from %d", back, first.Seq+1, pushedAt, first.ID)
		}
		if ok, err := p.ClaimPush(asUser(2), back.ID, pushedAt.Add(2*time.Hour)); err != nil || ok {
			t.Fatalf("the stop back from gone was claimed for a second push: %v %v", ok, err)
		}
		// Gone again and back again: still the first push.
		if ended, err := p.End(asUser(2), back.ID, EndGone, pushedAt.Add(3*time.Hour)); err != nil || !ended {
			t.Fatalf("end: %v %v", ended, err)
		}
		// The stop's FIRST episode stays the link (not the one in between).
		if again := reopen(t, 2, "k-carry", "since:111"); !again.PushedAt.Equal(pushedAt) || again.ResumedFrom != first.ID {
			t.Fatalf("gone twice: %+v, want pushed_at %v, resumed from %d", again, pushedAt, first.ID)
		}
	})

	t.Run("unpushed, gone, back on its streak: no push to carry, but the same stop", func(t *testing.T) {
		first := stop(t, 2, "k-unpushed", "since:222", false, EndGone)
		if back := reopen(t, 2, "k-unpushed", "since:222"); !back.PushedAt.IsZero() || back.ResumedFrom != first.ID {
			t.Fatalf("the unpushed stop back: %+v, want no push, resumed from %d", back, first.ID)
		}
	})

	for _, c := range []struct {
		name            string
		pushed          bool
		reason          string
		streak, streak2 string
	}{
		{"pushed, gone, back on a NEW streak", true, EndGone, "since:333", "since:334"},
		{"pushed, ended owner, same streak", true, EndOwner, "since:444", "since:444"},
		{"pushed, ended worked, same streak", true, EndWorked, "since:555", "since:555"},
		{"pushed, gone, no streak", true, EndGone, "", ""},
	} {
		t.Run(c.name+": carries nothing", func(t *testing.T) {
			key := "k-" + c.name
			stop(t, 2, key, c.streak, c.pushed, c.reason)
			if back := reopen(t, 2, key, c.streak2); !back.PushedAt.IsZero() || back.ResumedFrom != 0 {
				t.Fatalf("carried the stop: %+v", back)
			}
		})
	}

	t.Run("another session key, another user: carry nothing", func(t *testing.T) {
		stop(t, 2, "k-mine", "since:666", true, EndGone)
		// The other key at the SAME seq as k-mine's gone episode, so only the
		// key tells them apart.
		stop(t, 2, "k-other", "since:665", false, EndWorked)
		if other := reopen(t, 2, "k-other", "since:666"); !other.PushedAt.IsZero() || other.ResumedFrom != 0 {
			t.Fatalf("another key carried the push: %+v", other)
		}
		if theirs := reopen(t, 3, "k-mine", "since:666"); !theirs.PushedAt.IsZero() || theirs.ResumedFrom != 0 || theirs.Owner != 3 {
			t.Fatalf("another user's same key carried the push: %+v", theirs)
		}
	})

	t.Run("only the LATEST episode is resumed", func(t *testing.T) {
		stop(t, 2, "k-latest", "since:777", true, EndGone)
		stop(t, 2, "k-latest", "since:778", false, EndGone)
		if back := reopen(t, 2, "k-latest", "since:777"); !back.PushedAt.IsZero() || back.ResumedFrom != 0 {
			t.Fatalf("an older gone episode's push was carried past a later stop: %+v", back)
		}
	})
}

// TestResumedReasonsIsGoneAlone pins PG.Open's carry set: only 'gone' is the
// same stop back. A widened set would carry the push of a stop the owner
// answered.
func TestResumedReasonsIsGoneAlone(t *testing.T) {
	if !slices.Equal(resumedReasons, []string{EndGone}) {
		t.Fatalf("resumedReasons = %q, want [gone]", resumedReasons)
	}
}

// TestMissedPassesCountAndReset: Missed counts consecutive missing passes on
// an open episode (0 on an ended one); Present and Observe reset the count;
// the count is read back on the episode.
func TestMissedPassesCountAndReset(t *testing.T) {
	p := pgStore(t)
	ctx := asUser(2)
	e, _, err := p.Open(ctx, stalledEp("k-miss"))
	if err != nil {
		t.Fatal(err)
	}
	read := func() int {
		t.Helper()
		got, ok, err := p.OpenFor(ctx, KindMirror, "k-miss")
		if err != nil || !ok {
			t.Fatalf("read: %v %v", ok, err)
		}
		return got.MissedPasses
	}
	for want := 1; want <= 2; want++ {
		if n, err := p.Missed(ctx, e.ID); err != nil || n != want {
			t.Fatalf("missed #%d: %d %v", want, n, err)
		}
	}
	if n := read(); n != 2 {
		t.Fatalf("read back: %d", n)
	}
	if err := p.Present(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	if n := read(); n != 0 {
		t.Fatalf("after Present: %d", n)
	}
	if n, _ := p.Missed(ctx, e.ID); n != 1 {
		t.Fatalf("missed after Present: %d", n)
	}
	if err := p.Observe(ctx, e.ID, Observation{State: StateStalled, StreakKey: "since:1", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if n := read(); n != 0 {
		t.Fatalf("after Observe: %d", n)
	}
	if _, err := p.End(ctx, e.ID, EndGone, time.Now()); err != nil {
		t.Fatal(err)
	}
	if n, err := p.Missed(ctx, e.ID); err != nil || n != 0 {
		t.Fatalf("missed on an ended episode: %d %v", n, err)
	}
}

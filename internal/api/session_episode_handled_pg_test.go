package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/mirror"
)

// WHICH ENDS ARE "HANDLED" (sessionstate.Handled), end to end on a real
// Postgres. A handled stop is not opened again on the streak it ended on, so
// an end may be handled only when that streak is one the owner really dealt
// with: 'viewed' (the stop the owner was shown) and 'reset' (the backlog, once)
// — never 'owner', whose streak can be the NEXT stop's. And a handled stop an
// older image opened again mid-rollout is ended again (PG.EndRepeat).
//
// 🔴 These tests name only what exists at the commit before this change too
// (end reasons as the column holds them, no helper of the change), so each
// REGRESSION test is shown red there by running it there.

// endReasonsMigration is the migration that adds 'owner', 'viewed' and
// 'reset' (by its slug, so its number does not matter), as SQL.
func endReasonsMigration(t *testing.T) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "db", "migrations", "[0-9][0-9][0-9][0-9]_session_episode_end_*.sql"))
	if err != nil || len(files) != 1 {
		t.Fatalf("the end-reasons migration: %v %v", files, err)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// applyReset applies the end-reasons migration as the migrator would, scoped
// to the operator's rows so the package's shared database keeps other tests'
// episodes.
func (f *stateFixture) applyReset(t *testing.T) {
	t.Helper()
	dbtest.OwnerExec(t, dbtest.DSN(t), `BEGIN; SELECT set_config('muster.uid', '`+strconv.FormatInt(authz.OperatorUserID, 10)+
		`', true), set_config('muster.scope', '', true); `+endReasonsMigration(t)+` COMMIT;`)
}

// since is the fixture window's streak at host time ms, as the pass keys it.
func since(ms int64) string { return "since:" + strconv.FormatInt(ms, 10) }

// storedStreak is episode id's streak_key as the database holds it.
func (f *stateFixture) storedStreak(t *testing.T, id int64) string {
	t.Helper()
	var s string
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT streak_key FROM session_episodes WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// pushedFor reports whether a push (not its withdrawal) went out for episode id.
func (f *stateFixture) pushedFor(id int64) bool {
	for _, p := range f.episodePushes() {
		if p == "attention attention-episode-"+strconv.FormatInt(id, 10) {
			return true
		}
	}
	return false
}

// TestAStopAfterTheOwnerAnsweredIsPushedWhenTheOwnerReadFailedFirst
// (regression, round-1 F1): a stall on streak S1 is pushed; the owner answers
// it; the session works briefly and stops AGAIN, on S2, before the next pass.
// That pass's owner-acted read FAILS, so the episode stays open and the pass
// writes S2 onto it. The next pass reads OK and ends it 'owner' — carrying S2.
// S2 is a new stop the owner has not seen: it must open a new episode and push.
// Red before: 'owner' was handled, so the reopen guard refused S2 for as long
// as the session sat on it (the auditor: 50 passes, one episode, no push).
func TestAStopAfterTheOwnerAnsweredIsPushedWhenTheOwnerReadFailedFirst(t *testing.T) {
	f := newStateFixture(t, false)
	id, tag := f.pushedStall(t)
	f.say(chiefnudge.Msg{Role: "user", Text: "yes, merge it", ID: "line:9000", At: f.openedAt(t, id).Add(3 * time.Second)})
	f.advance(30 * time.Second)
	f.statusSince(mirror.StatusIdle, nudgeSince+600_000, 20*time.Minute)
	f.readErr = errors.New("the host did not answer")
	f.statePass(t)
	if e := f.oneEpisode(t); e.ID != id || !e.Open() || f.storedStreak(t, id) != since(nudgeSince+600_000) {
		t.Fatalf("the scenario: after the failed read the episode should be open on S2: %+v (streak %q)", e, f.storedStreak(t, id))
	}
	if f.readCount() == 0 {
		t.Fatal("the owner-acted read never ran: this test checked nothing")
	}
	f.readErr = nil
	f.advance(30 * time.Second)
	f.statePass(t)
	if e := f.episodeByID(t, id); e.EndReason != endOwner {
		t.Fatalf("after the read succeeded: reason %q, want %q", e.EndReason, endOwner)
	}
	// Past the repush floor: S2's stop pushes.
	f.advance(3 * time.Minute)
	f.statePass(t)
	list := f.episodes(t)
	if len(list) != 2 || list[1].ID != id || !list[0].Open() {
		t.Fatalf("S2's stop opened no new episode: %+v", list)
	}
	if got := f.episodePushes(); len(got) != 3 || got[0] != "attention "+tag || got[1] != "resolved "+tag || !f.pushedFor(list[0].ID) {
		t.Fatalf("pushes: %v, want S1's push, its withdrawal, then S2's push", got)
	}
}

// TestAViewAfterAFailedReadEndsOnTheStopTheOwnerSaw — an INVARIANT GUARD for
// 'viewed' in the F1 shape: the owner answers, the session stops again on S2,
// the owner-acted read fails, and THEN the owner opens the session. The view
// ends the episode on S2 — the stop the page showed — so S2 is not pushed
// again; the session's next stop, S3, is.
func TestAViewAfterAFailedReadEndsOnTheStopTheOwnerSaw(t *testing.T) {
	f := newStateFixture(t, false)
	id, _ := f.pushedStall(t)
	f.say(chiefnudge.Msg{Role: "user", Text: "yes, merge it", ID: "line:9000", At: f.openedAt(t, id).Add(3 * time.Second)})
	f.advance(30 * time.Second)
	f.statusSince(mirror.StatusIdle, nudgeSince+600_000, 20*time.Minute)
	f.readErr = errors.New("the host did not answer")
	f.statePass(t)
	f.readErr = nil
	if code := f.get(t, viewPath(), nil); code != 200 {
		t.Fatalf("GET view: %d", code)
	}
	if e := f.episodeByID(t, id); e.EndReason != endViewed || f.storedStreak(t, id) != since(nudgeSince+600_000) {
		t.Fatalf("the view: reason %q streak %q, want %q on S2", e.EndReason, f.storedStreak(t, id), endViewed)
	}
	f.advance(3 * time.Minute)
	f.statePass(t)
	if list := f.episodes(t); len(list) != 1 {
		t.Fatalf("the stop the owner was shown opened again: %+v", list)
	}
	f.advance(30 * time.Second)
	f.statusSince(mirror.StatusIdle, nudgeSince+900_000, 20*time.Minute)
	f.statePass(t)
	if list := f.episodes(t); len(list) != 2 || !list[0].Open() || !f.pushedFor(list[0].ID) {
		t.Fatalf("the next stop (S3) opened or pushed nothing: %+v, pushes %v", list, f.episodePushes())
	}
}

// TestAViewEndsOnTheStreakItShowedNotTheStoredOne (regression, round-1 N1): a
// stall on S1 is pushed; the session stops again on S2 by itself before the
// next pass, so the episode still holds S1. The owner opens the session — the
// page shows S2's stop. The episode ends 'viewed' on S2, and the next passes
// do not open and push S2 as a new stop. Red before: the view ended it on the
// stored S1, and the next pass pushed S2 again.
func TestAViewEndsOnTheStreakItShowedNotTheStoredOne(t *testing.T) {
	f := newStateFixture(t, false)
	id, _ := f.pushedStall(t)
	f.advance(10 * time.Second)
	f.statusSince(mirror.StatusIdle, nudgeSince+600_000, 20*time.Minute)
	if code := f.get(t, viewPath(), nil); code != 200 {
		t.Fatalf("GET view: %d", code)
	}
	if e := f.episodeByID(t, id); e.EndReason != endViewed {
		t.Fatalf("the view: reason %q", e.EndReason)
	}
	f.advance(3 * time.Minute)
	f.statePass(t)
	f.advance(30 * time.Second)
	f.statePass(t)
	if list := f.episodes(t); len(list) != 1 {
		t.Fatalf("the stop the owner was shown opened again: %+v (pushes %v)", list, f.episodePushes())
	}
	if got := f.episodePushes(); len(got) != 2 {
		t.Fatalf("pushes: %v, want the push and its withdrawal only", got)
	}
}

// TestAResetOnALaggingStreakStillPushesTheNewStop — an INVARIANT GUARD for
// 'reset': the reset ends an episode on the streak last observed. When the
// session has stopped again since (S2, not yet observed), the reset's S1 does
// not suppress S2: it opens a new episode and pushes.
func TestAResetOnALaggingStreakStillPushesTheNewStop(t *testing.T) {
	f := newStateFixture(t, false)
	id, _ := f.pushedStall(t)
	f.advance(10 * time.Second)
	f.statusSince(mirror.StatusIdle, nudgeSince+600_000, 20*time.Minute)
	f.applyReset(t)
	if e := f.episodeByID(t, id); e.EndReason != endReset {
		t.Fatalf("after the reset: reason %q", e.EndReason)
	}
	f.advance(3 * time.Minute)
	f.statePass(t)
	if list := f.episodes(t); len(list) != 2 || !list[0].Open() || !f.pushedFor(list[0].ID) {
		t.Fatalf("the stop after the reset's streak opened or pushed nothing: %+v, pushes %v", list, f.episodePushes())
	}
}

// oldImageOpen opens an episode for the fixture window as an image from before
// the reset does: no reopen guard, on streak (the old waiting hook names none:
// ""). It returns the new episode's id.
func (f *stateFixture) oldImageOpen(t *testing.T, streak string) int64 {
	t.Helper()
	var id int64
	if err := f.pool.QueryRow(authz.System(context.Background()), `INSERT INTO session_episodes (owner_id, session_kind, session_key, seq,
			state, waiting, asked, runtime, mirror_host, mirror_session, mirror_window, window_index, streak_key)
		SELECT $1, 'mirror', $2, coalesce(max(seq), 0) + 1, 'stalled', false, true, '', 'ws', 'main', '@1', 1, $3
		  FROM session_episodes WHERE owner_id = $1 AND session_kind = 'mirror' AND session_key = $2
		RETURNING id`, authz.OperatorUserID, nudgeSID, streak).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestAnOlderImageReopeningAResetStopIsEndedAndWithdrawn (regression, round-1
// F2): during the rolling update the older image keeps running after the reset
// applied, and opens and pushes a session still stopped on the streak its
// reset episode ended on — from its pass (the streak named) or its waiting
// hook (no streak). The new image's next pass ends that repeat 'reset' and
// withdraws its push, and does not open the stop again; the session moving on
// is still a new stop (the control). Red before: the repeat stayed open and
// pushed.
func TestAnOlderImageReopeningAResetStopIsEndedAndWithdrawn(t *testing.T) {
	for _, c := range []struct{ name, streak string }{
		{"its pass", since(nudgeSince)},
		{"its waiting hook", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newStateFixture(t, false)
			id, _ := f.pushedStall(t)
			f.applyReset(t)
			f.advance(30 * time.Second)
			f.statePass(t)
			if e := f.episodeByID(t, id); e.EndReason != endReset || !f.cleared(t, id) {
				t.Fatalf("the reset: %+v (cleared %v)", e, f.cleared(t, id))
			}
			// The older image, past the repush floor: opens the same stop and pushes it.
			f.advance(3 * time.Minute)
			rep := f.oldImageOpen(t, c.streak)
			ep, ok, err := f.eps.Get(asUser(authz.OperatorUserID), rep)
			if err != nil || !ok {
				t.Fatalf("the repeat: %v %v", ok, err)
			}
			f.srv.pushEpisode(asUser(authz.OperatorUserID), ep, 20*time.Minute)
			if !f.pushedFor(rep) {
				t.Fatalf("the scenario: the older image's push did not go out: %v", f.episodePushes())
			}
			// The new image's pass.
			f.advance(30 * time.Second)
			f.statePass(t)
			if e := f.episodeByID(t, rep); e.EndedAt.IsZero() || e.EndReason != endReset {
				t.Fatalf("the older image's repeat: ended %v reason %q, want %q", e.EndedAt, e.EndReason, endReset)
			}
			if !f.cleared(t, rep) {
				t.Fatal("the repeat's push was not withdrawn")
			}
			repTag := "resolved attention-episode-" + strconv.FormatInt(rep, 10)
			if got := f.episodePushes(); got[len(got)-1] != repTag {
				t.Fatalf("pushes: %v, want the repeat's withdrawal last", got)
			}
			n := len(f.episodePushes())
			f.advance(3 * time.Minute)
			f.statePass(t)
			if list := f.episodes(t); len(list) != 2 || list[0].Open() {
				t.Fatalf("the stop opened again after the repeat ended: %+v", list)
			}
			if got := f.episodePushes(); len(got) != n {
				t.Fatalf("pushes after the repeat ended: %v", got)
			}
			// The control: the session moves on; its next stop is a new one,
			// pushed, and not ended on the next pass.
			f.statusSince(mirror.StatusIdle, nudgeSince+900_000, 20*time.Minute)
			f.statePass(t)
			f.advance(30 * time.Second)
			f.statePass(t)
			list := f.episodes(t)
			if len(list) != 3 || !list[0].Open() || !f.pushedFor(list[0].ID) {
				t.Fatalf("the session's next stop: %+v, pushes %v", list, f.episodePushes())
			}
		})
	}
}

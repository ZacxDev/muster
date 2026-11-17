package api

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
	"github.com/ZacxDev/muster/internal/sessionstate"
)

// THE SESSION MINDER end to end on a real Postgres (session_minder.go): an
// owner with an active Session minder gets no chief nudge — the chief is not
// even asked — and paused or deleted, the nudges come back; another owner's
// minder changes nothing for this one; with session state off it suppresses
// nothing; and each stop the nudges would have judged becomes ONE item in its
// queue, once for good.

// minderRig is the state fixture with the built-in officer store on.
type minderRig struct {
	*stateFixture
	store *builtin.Store
}

func newMinderRig(t *testing.T, nudges bool) *minderRig {
	t.Helper()
	f := newStateFixture(t, nudges)
	r := &minderRig{stateFixture: f, store: builtin.NewStore(f.pool)}
	f.srv.EnableOfficerStore(r.store)
	return r
}

// declareMinder declares owner's officer id in the database the way the
// wizard's Session minder template does — its type granting session_minder —
// switched on. Removed at cleanup with everything that is only its.
func declareMinder(t *testing.T, store *builtin.Store, owner int64, id string, caps ...string) {
	t.Helper()
	if caps == nil {
		caps = []string{builtin.CapSessionMinder}
	}
	pol, err := json.Marshal(map[string]any{"capabilities": caps})
	if err != nil {
		t.Fatal(err)
	}
	slug := "minder" + strconv.FormatInt(time.Now().UnixNano()%1_000_000_000, 36)
	if _, err := store.CreateDeclared(asUser(owner), owner, builtin.DeclaredWrite{TypeName: "Session minder", Charter: "Mind my sessions.",
		Policy: pol, Enabled: true, Mode: "shadow", Fields: officers.Declaration{ID: id, Owner: owner, Runtime: officers.RuntimeAuto}},
		[]string{slug}); err != nil {
		t.Fatalf("declare %s for %d: %v", id, owner, err)
	}
	t.Cleanup(func() { _, _ = store.DeleteDeclared(asUser(owner), owner, id) })
}

// minderItems is owner's officer's items, oldest first, read straight from
// the table.
type minderRow struct {
	ID       int64
	Kind     string
	Subject  string
	Priority string
	Status   string
	Payload  minderPayload
}

func minderItems(t *testing.T, f *stateFixture, owner int64, officer string) []minderRow {
	t.Helper()
	rows, err := f.pool.Query(authz.System(context.Background()), `SELECT id, kind, subject_key, priority, status, payload
		FROM officer_items WHERE owner_id = $1 AND officer = $2 ORDER BY id`, owner, officer)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []minderRow
	for rows.Next() {
		var m minderRow
		var raw []byte
		if err := rows.Scan(&m.ID, &m.Kind, &m.Subject, &m.Priority, &m.Status, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &m.Payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

const minderID = "session-minder:main"

// TestASessionMinderTurnsTheChiefsNudgesOffForItsOwner: with the operator's
// Session minder switched on, a stop the chief would have judged is decided
// NOT AT ALL — no decision recorded, no chief call — and is the minder's item
// instead. Paused, the very next sweep decides it (the nudges are back).
// CONTROL: TestAnotherOwnersSessionMinderLeavesTheNudgesAlone decides the same
// stop with no minder of the operator's.
func TestASessionMinderTurnsTheChiefsNudgesOffForItsOwner(t *testing.T) {
	r := newMinderRig(t, true)
	declareMinder(t, r.store, authz.OperatorUserID, minderID)
	r.statePass(t)
	if n := r.pass(t); n != 0 {
		t.Fatalf("SUPPRESSION: an owner with an active Session minder was nudged: %d decision(s) %+v", n, r.decisions(t))
	}
	if checks, _ := r.gw.seen(); len(checks) != 0 {
		t.Fatalf("SUPPRESSION: the chief was asked about a minded owner's stop: %v", checks)
	}
	if got := minderItems(t, r.stateFixture, authz.OperatorUserID, minderID); len(got) != 1 {
		t.Fatalf("the minder's items: %+v; want the one stop", got)
	}

	if _, err := r.store.SetDeclaredEnabled(asUser(authz.OperatorUserID), authz.OperatorUserID, minderID, false); err != nil {
		t.Fatal(err)
	}
	if n := r.pass(t); n != 1 {
		t.Fatalf("a PAUSED Session minder still kept the chief's nudges off: decided %d, want 1", n)
	}
	if d := r.one(t); d.Kind != "mirror" || d.Key != nudgeSID {
		t.Fatalf("decided %+v; want the window %s", d, nudgeSID)
	}
}

// TestADeletedSessionMinderGivesTheNudgesBack: suppressed while it exists;
// deleted (the page's Delete: DeleteDeclared), the next sweep decides the stop.
func TestADeletedSessionMinderGivesTheNudgesBack(t *testing.T) {
	r := newMinderRig(t, true)
	declareMinder(t, r.store, authz.OperatorUserID, minderID)
	r.statePass(t)
	if n := r.pass(t); n != 0 {
		t.Fatalf("SUPPRESSION: an owner with an active Session minder was nudged: %d decision(s)", n)
	}
	if _, err := r.store.DeleteDeclared(asUser(authz.OperatorUserID), authz.OperatorUserID, minderID); err != nil {
		t.Fatal(err)
	}
	if n := r.pass(t); n != 1 {
		t.Fatalf("a DELETED Session minder still kept the chief's nudges off: decided %d, want 1", n)
	}
}

// TestAnotherOwnersSessionMinderLeavesTheNudgesAlone: another person's active
// minder — and an officer of the operator's own whose type grants something
// else — leave the operator's nudges exactly as they were: the stop is
// decided, by the chief.
func TestAnotherOwnersSessionMinderLeavesTheNudgesAlone(t *testing.T) {
	r := newMinderRig(t, true)
	declareMinder(t, r.store, r.member, minderID)
	declareMinder(t, r.store, authz.OperatorUserID, "drafter:main", builtin.CapOutboundDrafts)
	r.statePass(t)
	if n := r.pass(t); n != 1 {
		t.Fatalf("decided %d, want 1: another owner's minder (or a non-minder officer) took the operator's nudges away", n)
	}
	if checks, _ := r.gw.seen(); len(checks) != 1 {
		t.Fatalf("chief calls %v; want one", checks)
	}
	if got := minderItems(t, r.stateFixture, authz.OperatorUserID, "drafter:main"); len(got) != 0 {
		t.Fatalf("a non-minder officer was fed stops: %+v", got)
	}
}

// TestASessionMinderWithoutSessionStateSuppressesNothing: with session state
// off nothing can feed the minder, so it takes nothing away — the chief still
// decides the stop (a minder that cannot mind never silences).
func TestASessionMinderWithoutSessionStateSuppressesNothing(t *testing.T) {
	f := newNudgeFixture(t, false)
	store := builtin.NewStore(f.pool)
	f.srv.EnableOfficerStore(store)
	declareMinder(t, store, authz.OperatorUserID, minderID)
	if n := f.pass(t); n != 1 {
		t.Fatalf("decided %d with session state off, want 1", n)
	}
}

// TestAMindedOwnersStopPushesWithoutWaitingOnTheChief: a LIVE owner's idle
// stop is held for the chief's decision (episodeChiefWait) — but with a
// Session minder the chief decides nothing, so the stop is pushed on the
// first pass, as with no chief. CONTROL: the same live owner with no minder,
// first pass, no push.
func TestAMindedOwnersStopPushesWithoutWaitingOnTheChief(t *testing.T) {
	ctl := newMinderRig(t, true)
	ctl.live()
	ctl.statePass(t)
	if got := ctl.episodePushes(); len(got) != 0 {
		t.Fatalf("control: a live owner's stop pushed before the chief decided: %v", got)
	}

	r := newMinderRig(t, true)
	r.live()
	declareMinder(t, r.store, authz.OperatorUserID, minderID)
	r.statePass(t)
	if got := r.episodePushes(); len(got) != 1 {
		t.Fatalf("a minded live owner's stop: pushes %v; want one at once (the chief judges nothing for it)", got)
	}
}

// TestASessionMinderIsFedEachStopOnce is the feed: the stop the nudges would
// have judged is one item — kind session.stalled, subject its episode, where
// the session is and its state, no session text — and stays one item through
// further passes AND after the item is done while the stop stays open. The
// next stop is a second item. It runs with the chief's nudges OFF: the feed
// is session state's, not the chief's.
func TestASessionMinderIsFedEachStopOnce(t *testing.T) {
	r := newMinderRig(t, false)
	declareMinder(t, r.store, authz.OperatorUserID, minderID)
	r.statePass(t)
	e := r.oneEpisode(t)
	got := minderItems(t, r.stateFixture, authz.OperatorUserID, minderID)
	if len(got) != 1 {
		t.Fatalf("items after the first pass: %+v; want 1", got)
	}
	it := got[0]
	wantSubject := "episode:" + strconv.FormatInt(e.ID, 10)
	if it.Kind != "session.stalled" || it.Subject != wantSubject || it.Status != "open" {
		t.Fatalf("item %+v; want kind session.stalled, subject %s, open", it, wantSubject)
	}
	p := it.Payload
	if p.EpisodeID != e.ID || p.State != sessionstate.StateStalled || !p.Asked || p.Session.Kind != "mirror" ||
		p.Session.Label != "Gold:1" || p.Session.TmuxSession != "main" || p.Session.WindowIndex != 1 || p.IdleSeconds != 20*60 {
		t.Fatalf("payload %+v", p)
	}
	// With no classifier the stop is stalled and counts as asked
	// (sessionstate.Asked): a normal-priority item (high is for needs_you).
	if it.Priority != "normal" {
		t.Fatalf("priority %q; want normal for a stalled stop", it.Priority)
	}

	r.advance(30 * time.Second)
	r.statePass(t)
	if got := minderItems(t, r.stateFixture, authz.OperatorUserID, minderID); len(got) != 1 {
		t.Fatalf("a second pass over the same stop: %d items, want 1", len(got))
	}
	if _, err := r.store.ResolveItem(asUser(authz.OperatorUserID), authz.OperatorUserID, minderID, it.ID, "no_action", "finished its work", r.srv.now()); err != nil {
		t.Fatal(err)
	}
	r.advance(30 * time.Second)
	r.statePass(t)
	if got := minderItems(t, r.stateFixture, authz.OperatorUserID, minderID); len(got) != 1 {
		t.Fatalf("the same open stop after its item was done: %d items, want 1 (fed once for good)", len(got))
	}

	// The next stop: sustained work ends the episode, then it stops again.
	r.status(mirror.StatusBusy, 0)
	r.statePass(t)
	r.advance(sessionstate.EndAfter + 9*time.Second)
	r.statePass(t)
	r.advance(time.Minute)
	r.status(mirror.StatusIdle, 20*time.Minute)
	r.statePass(t)
	got = minderItems(t, r.stateFixture, authz.OperatorUserID, minderID)
	if len(got) != 2 || got[1].Subject == got[0].Subject {
		t.Fatalf("after the next stop: %+v; want a second item for its own episode", got)
	}
}

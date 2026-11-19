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
	"github.com/ZacxDev/muster/internal/provision"
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

// newMinderRig is the minder fixture. Its officer runtime can run a minder IN PODS (a provisioner, the claude-code
// kind, the session settings taken), so a declared minder resolves and is in
// the live roster (minderRuns). newMinderRigNowhere is the same rig with
// nowhere to run one.
func newMinderRig(t *testing.T, nudges bool) *minderRig {
	t.Helper()
	return newMinderRigWith(t, nudges, true)
}

// newMinderRigNowhere is newMinderRig whose officer runtime can run nothing:
// no provisioner (pods refused) and no host spawn store (hosts refused).
func newMinderRigNowhere(t *testing.T, nudges bool) *minderRig {
	t.Helper()
	return newMinderRigWith(t, nudges, false)
}

func newMinderRigWith(t *testing.T, nudges, pods bool) *minderRig {
	t.Helper()
	f := newStateFixture(t, nudges)
	r := &minderRig{stateFixture: f, store: builtin.NewStore(f.pool)}
	f.srv.EnableOfficerStore(r.store)
	cfg := OfficersConfig{Builtin: r.store}
	if pods {
		// The fixture's chief reads as RUNNING on the provisioner (live status
		// is the provisioner's once there is one), so the nudges still reach it.
		f.srv.ext.Provisioner = &officerProv{store: &officerAgents{}, phase: map[string]provision.Phase{f.boss.Name: provision.PhaseRunning}}
		f.srv.ext.Kinds = stubKinds{pick: "acct-one"}
		cfg.InstallSession = discardSession
	}
	if _, err := f.srv.EnableOfficers(cfg); err != nil {
		t.Fatal(err)
	}
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

// declare is declareMinder on the rig, then the roster load every write runs
// (reloadDeclaredOfficers), so the live roster has it when it can run.
func (r *minderRig) declare(t *testing.T, owner int64, id string, caps ...string) {
	t.Helper()
	declareMinder(t, r.store, owner, id, caps...)
	if err := r.srv.reloadDeclaredOfficers(context.Background()); err != nil {
		t.Fatal(err)
	}
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
	r.declare(t, authz.OperatorUserID, minderID)
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
	r.declare(t, authz.OperatorUserID, minderID)
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
	r.declare(t, r.member, minderID)
	r.declare(t, authz.OperatorUserID, "drafter:main", builtin.CapOutboundDrafts)
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

// TestASessionMinderThatCannotRunSuppressesNothing: switched on, but this
// muster has nowhere to run it (no pods, no host) — the roster load resolves
// it to nothing, so it would mind nothing, and the chief still decides the
// stop. CONTROL: TestASessionMinderTurnsTheChiefsNudgesOffForItsOwner, the
// same minder on a rig that runs it in pods, decides nothing.
func TestASessionMinderThatCannotRunSuppressesNothing(t *testing.T) {
	r := newMinderRigNowhere(t, true)
	r.declare(t, authz.OperatorUserID, minderID)
	st, ok := r.srv.officerRT.declaredView(authz.OperatorUserID, minderID)
	if !ok || st.officer != nil || st.reason == "" {
		t.Fatalf("fixture: the minder should be declared and resolve to nowhere; got declared=%v %+v", ok, st)
	}
	r.statePass(t)
	if n := r.pass(t); n != 1 {
		t.Fatalf("UNRUNNABLE MINDER: a Session minder with nowhere to run (%s) kept the chief's nudges off: decided %d, want 1", st.reason, n)
	}
	if got := minderItems(t, r.stateFixture, authz.OperatorUserID, minderID); len(got) != 0 {
		t.Fatalf("a minder that cannot run was fed stops: %+v", got)
	}
}

// minderHost is the named host a pinned minder runs on in the tests below:
// not the fixture's "ws" (where the stopped session is), so dropping it
// leaves the stop itself in view.
const minderHost = "minderbox"

// newMinderRigOnHost is the minder fixture whose officer runtime runs host
// officers only (no pods: a provisioner would let the chief's pod placement
// stand in), with a second host of the operator's, minderHost, listed by the
// mirror hub — connected when online.
func newMinderRigOnHost(t *testing.T, online bool) *minderRig {
	t.Helper()
	f := newStateFixture(t, true)
	r := &minderRig{stateFixture: f, store: builtin.NewStore(f.pool)}
	f.srv.EnableOfficerStore(r.store)
	if _, err := f.srv.EnableOfficers(OfficersConfig{Builtin: r.store, HostSpawns: officers.NewMemHostSpawnStore()}); err != nil {
		t.Fatal(err)
	}
	r.setMinderHost(online)
	return r
}

// setMinderHost lists minderHost on the hub, connected or not (an
// interactive host when connected).
func (r *minderRig) setMinderHost(online bool) { r.setMinderHostView(online, online) }

// setMinderHostView lists minderHost with Online and Interactive set apart, so
// a test can tell which of the two the minder's rule reads.
func (r *minderRig) setMinderHostView(online, interactive bool) {
	r.mirror.mu.Lock()
	defer r.mirror.mu.Unlock()
	r.mirror.others = []mirror.HostView{{Host: minderHost, Online: online, Interactive: interactive}}
}

// declareOnHost declares owner's Session minder pinned to host ("runtime":
// "host"), switched on, then runs the roster load.
func (r *minderRig) declareOnHost(t *testing.T, owner int64, id, host string) {
	t.Helper()
	pol, err := json.Marshal(map[string]any{"capabilities": []string{builtin.CapSessionMinder}})
	if err != nil {
		t.Fatal(err)
	}
	slug := "minder" + strconv.FormatInt(time.Now().UnixNano()%1_000_000_000, 36)
	if _, err := r.store.CreateDeclared(asUser(owner), owner, builtin.DeclaredWrite{TypeName: "Session minder", Charter: "Mind my sessions.",
		Policy: pol, Enabled: true, Mode: "shadow", Fields: officers.Declaration{ID: id, Owner: owner, Runtime: officers.RuntimeHost, Host: host}},
		[]string{slug}); err != nil {
		t.Fatalf("declare %s on %s for %d: %v", id, host, owner, err)
	}
	t.Cleanup(func() { _, _ = r.store.DeleteDeclared(asUser(owner), owner, id) })
	if err := r.srv.reloadDeclaredOfficers(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestASessionMinderOnAnOfflineHostSuppressesNothing: a Session minder pinned
// to a named host is IN THE ROSTER whether or not that host is connected (the
// runtime rule only asks whether this muster runs host officers). While the
// host is connected it minds — the chief decides nothing and the minder gets
// the stop. Once the host drops, nothing is minding: the very next sweep
// decides the stop (the nudges are back), and the minder is fed nothing more.
// CONTROL for the pod path: TestASessionMinderTurnsTheChiefsNudgesOffForItsOwner
// (a minder in pods, no host involved) still suppresses.
func TestASessionMinderOnAnOfflineHostSuppressesNothing(t *testing.T) {
	r := newMinderRigOnHost(t, true)
	r.declareOnHost(t, authz.OperatorUserID, minderID, minderHost)
	o, ok := r.srv.officerRT.reg.ForOwner(authz.OperatorUserID, minderID)
	if !ok || !o.OnHost() || o.Host != minderHost {
		st, _ := r.srv.officerRT.declaredView(authz.OperatorUserID, minderID)
		t.Fatalf("fixture: the minder should be in the roster on %s; got in=%v %+v (reason %q)", minderHost, ok, o, st.reason)
	}
	r.statePass(t)
	if n := r.pass(t); n != 0 {
		t.Fatalf("SUPPRESSION: a minder on a CONNECTED host let the chief decide %d stop(s)", n)
	}
	if got := minderItems(t, r.stateFixture, authz.OperatorUserID, minderID); len(got) != 1 {
		t.Fatalf("the minder's items on a connected host: %+v; want the one stop", got)
	}

	r.setMinderHost(false)
	if _, ok := r.srv.officerRT.reg.ForOwner(authz.OperatorUserID, minderID); !ok {
		t.Fatal("fixture: the minder left the roster when its host dropped; the case under test is one still IN it")
	}
	if n := r.pass(t); n != 1 {
		t.Fatalf("OFFLINE HOST: a Session minder whose host %s is not connected kept the chief's nudges off: decided %d, want 1", minderHost, n)
	}
}

// TestASessionMinderOnAnOfflineHostIsNotFed: pinned to a host that is not
// connected from the start, the minder takes nothing — the chief decides the
// stop and the minder's queue stays empty.
func TestASessionMinderOnAnOfflineHostIsNotFed(t *testing.T) {
	r := newMinderRigOnHost(t, false)
	r.declareOnHost(t, authz.OperatorUserID, minderID, minderHost)
	if _, ok := r.srv.officerRT.reg.ForOwner(authz.OperatorUserID, minderID); !ok {
		t.Fatal("fixture: the minder should be in the roster with its host offline")
	}
	r.statePass(t)
	if got := minderItems(t, r.stateFixture, authz.OperatorUserID, minderID); len(got) != 0 {
		t.Fatalf("a minder on an offline host was fed stops: %+v", got)
	}
	if n := r.pass(t); n != 1 {
		t.Fatalf("OFFLINE HOST: decided %d, want 1", n)
	}
}

// TestTheMindersHostIsReadByOnlineNotInteractive: "connected" is the hub's
// Online, not Interactive — a connected host that takes no input still runs
// the minder (it suppresses); ...Offline: a host listed Interactive but not
// Online does not (the chief decides).
func TestTheMindersHostIsReadByOnlineNotInteractive(t *testing.T) {
	r := newMinderRigOnHost(t, true)
	r.setMinderHostView(true, false)
	r.declareOnHost(t, authz.OperatorUserID, minderID, minderHost)
	r.statePass(t)
	if n := r.pass(t); n != 0 {
		t.Fatalf("ONLINE, NOT INTERACTIVE: a minder on a connected host let the chief decide %d stop(s)", n)
	}
}

// TestTheMindersHostIsReadByOnlineNotInteractiveOffline is its other half.
func TestTheMindersHostIsReadByOnlineNotInteractiveOffline(t *testing.T) {
	off := newMinderRigOnHost(t, false)
	off.setMinderHostView(false, true)
	off.declareOnHost(t, authz.OperatorUserID, minderID, minderHost)
	off.statePass(t)
	if n := off.pass(t); n != 1 {
		t.Fatalf("INTERACTIVE, NOT ONLINE: a minder on a disconnected host kept the chief's nudges off: decided %d, want 1", n)
	}
}

// minderItemStatus puts the operator's minder item id in status — the minder
// having acted on it: done through the store's ResolveItem; waiting or
// claimed (with a lease d past the fixture's clock) straight in the table.
func (r *minderRig) minderItemStatus(t *testing.T, id int64, status string, d time.Duration) {
	t.Helper()
	if status == "done" {
		if _, err := r.store.ResolveItem(asUser(authz.OperatorUserID), authz.OperatorUserID, minderID, id, "done", "asked its owner", r.srv.now()); err != nil {
			t.Fatal(err)
		}
		return
	}
	var lease *time.Time
	if status == "claimed" {
		at := r.srv.now().Add(d)
		lease = &at
	}
	if _, err := r.pool.Exec(authz.System(context.Background()), `UPDATE officer_items SET status = $2, claimed_by = CASE WHEN $2 = 'claimed' THEN 'officer-instance-1' END,
		claimed_at = CASE WHEN $2 = 'claimed' THEN now() END, lease_expires_at = $3 WHERE id = $1`, id, status, lease); err != nil {
		t.Fatal(err)
	}
}

// TestAStopFedToAMinderIsClosedWhenTheChiefTakesIt: a stop fed to the minder
// while its host was connected, still OPEN in its queue when the host drops,
// is judged by the chief — and the minder's item for it is closed first
// (no_action), so the minder, back, never acts on the stop the chief took.
func TestAStopFedToAMinderIsClosedWhenTheChiefTakesIt(t *testing.T) {
	r := newMinderRigOnHost(t, true)
	r.declareOnHost(t, authz.OperatorUserID, minderID, minderHost)
	r.statePass(t)
	if got := minderItems(t, r.stateFixture, authz.OperatorUserID, minderID); len(got) != 1 || got[0].Status != "open" {
		t.Fatalf("fixture: the minder's items %+v; want the one stop, open", got)
	}
	r.setMinderHost(false)
	if n := r.pass(t); n != 1 {
		t.Fatalf("the chief decided %d, want 1 (the minder is offline)", n)
	}
	got := minderItems(t, r.stateFixture, authz.OperatorUserID, minderID)
	if len(got) != 1 || got[0].Status != "done" {
		t.Fatalf("TWO JUDGES: the chief judged the stop and the minder's item for it is %+v; want it closed (done)", got)
	}
}

// TestAStopTheMinderActedOnIsNotJudgedByTheChief: a stop the minder already
// acted on — finished, waiting on its owner, or claimed under a live lease —
// stays the minder's when its host drops: the chief does not judge it.
// CONTROL: TestAStopFedToAMinderIsClosedWhenTheChiefTakesIt, the same stop
// still open, is judged.
func TestAStopTheMinderActedOnIsNotJudgedByTheChief(t *testing.T) {
	for _, status := range []string{"done", "waiting", "claimed"} {
		t.Run(status, func(t *testing.T) {
			r := newMinderRigOnHost(t, true)
			r.declareOnHost(t, authz.OperatorUserID, minderID, minderHost)
			r.statePass(t)
			got := minderItems(t, r.stateFixture, authz.OperatorUserID, minderID)
			if len(got) != 1 {
				t.Fatalf("fixture: the minder's items %+v; want the one stop", got)
			}
			r.minderItemStatus(t, got[0].ID, status, 10*time.Minute)
			r.setMinderHost(false)
			if n := r.pass(t); n != 0 {
				t.Fatalf("TWO JUDGES: the minder's item for the stop is %s, and the chief judged it too: decided %d, want 0", status, n)
			}
			if checks, _ := r.gw.seen(); len(checks) != 0 {
				t.Fatalf("TWO JUDGES: the chief was asked about a stop the minder holds (%s): %v", status, checks)
			}
		})
	}
}

// TestAMinderIsFedAnUnjudgedStopWhenItsHostReturns is the CONTROL for
// TestAMinderIsNotFedAStopTheChiefJudged: the same stop, NOT judged while the
// host was offline, is fed to the minder once it is back.
func TestAMinderIsFedAnUnjudgedStopWhenItsHostReturns(t *testing.T) {
	ctl := newMinderRigOnHost(t, false)
	ctl.declareOnHost(t, authz.OperatorUserID, minderID, minderHost)
	ctl.statePass(t)
	ctl.setMinderHost(true)
	ctl.advance(30 * time.Second)
	ctl.statePass(t)
	if got := minderItems(t, ctl.stateFixture, authz.OperatorUserID, minderID); len(got) != 1 {
		t.Fatalf("control: an unjudged stop after the host came back: items %+v; want 1", got)
	}
}

// TestAMinderIsNotFedAStopTheChiefJudged: the chief judged a stop while the
// minder's host was offline; when the host comes back the state pass does not
// feed that stop to the minder as well.
func TestAMinderIsNotFedAStopTheChiefJudged(t *testing.T) {
	r := newMinderRigOnHost(t, false)
	r.declareOnHost(t, authz.OperatorUserID, minderID, minderHost)
	r.statePass(t)
	if n := r.pass(t); n != 1 {
		t.Fatalf("fixture: the chief decided %d, want 1 (the minder is offline)", n)
	}
	r.setMinderHost(true)
	r.advance(30 * time.Second)
	r.statePass(t)
	if got := minderItems(t, r.stateFixture, authz.OperatorUserID, minderID); len(got) != 0 {
		t.Fatalf("TWO JUDGES: the reconnected minder was fed a stop the chief already judged: %+v", got)
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
	r.declare(t, authz.OperatorUserID, minderID)
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
	r.declare(t, authz.OperatorUserID, minderID)
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

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/fleet"
	"github.com/ZacxDev/muster/internal/githubapp"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/sessionstate"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/ui"
)

// FLEET PROPOSALS (fleet_actions.go, fleet_cards.go) on a real Postgres: the
// chief's tools file cards; the owner's tap sends exactly the stored text
// through the send queue; every refusal and re-read; the flag off. Every id,
// key, repo and text is invented and pairwise distinct, and distinct from the
// constants the assertions name.

// proposeFixture is the fleet fixture with fleet proposals on, its clock
// pinned to a minute so the attribution line is a literal.
type proposeFixture struct {
	*fleetFixture
	at time.Time
}

func newProposeFixture(t *testing.T) *proposeFixture {
	t.Helper()
	f := &proposeFixture{fleetFixture: newFleetFixture(t), at: time.Now().UTC().Truncate(time.Minute)}
	f.srv.ext.FleetPropose, f.srv.ext.FleetActions = true, fleet.NewPG(f.pool)
	f.srv.ext.Mirror = observedMirror{f.mirror}
	f.srv.now = func() time.Time { return f.at }
	return f
}

// observedMirror is the digest's mirror whose window states carry the time
// the host observed them (what the stall facts key a streak by).
type observedMirror struct{ *digestMirror }

func (m observedMirror) AgentState(ctx context.Context, host, session, window string) mirror.AgentState {
	st := m.digestMirror.AgentState(ctx, host, session, window)
	if st.Since > 0 {
		st.Observed = time.UnixMilli(st.Since)
	}
	return st
}

// stopped lists a stopped Claude Code window for owner (an open stalled stop
// on its idle streak), returning its fleet id, key and window.
func (f *proposeFixture) stopped(t *testing.T, owner int64) (id, key, window string) {
	t.Helper()
	key, window = f.window(owner, mirror.StatusIdle)
	f.stop(t, owner, key, window, sessionstate.StateStalled, false, fleetT0)
	return "mirror:" + key, key, window
}

// tool calls a fleet tool as the owner and decodes its answer.
func (f *proposeFixture) tool(t *testing.T, ctx context.Context, name, args string) map[string]any {
	t.Helper()
	out, ok := f.srv.dispatchFleetTool(ctx, name, args)
	if !ok {
		t.Fatalf("%s is not a fleet tool", name)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("%s: %v: %s", name, err, out)
	}
	return m
}

// propose files class on subject as the owner, returning the action id.
func (f *proposeFixture) propose(t *testing.T, subject, class string) int64 {
	t.Helper()
	m := f.tool(t, f.asOwner, sessionInstructTool, `{"subject":`+strconv.Quote(subject)+`,"class":"`+class+`","reason":"it offered a next step"}`)
	if m["state"] != fleet.StateProposed {
		t.Fatalf("propose %s on %s: %v", class, subject, m)
	}
	return int64(m["action_id"].(float64))
}

// fresh is ctx with a sign-in from just now (the step-up window's).
func fresh(ctx context.Context) context.Context { return stepup.WithAuthTime(ctx, time.Now()) }

// stale is ctx with a sign-in older than the step-up window.
func stale(ctx context.Context) context.Context {
	return stepup.WithAuthTime(ctx, time.Now().Add(-stepup.Window()-time.Hour))
}

// card posts verb on proposal id as ctx, with form.
func (f *proposeFixture) card(t *testing.T, ctx context.Context, id int64, verb string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, ui.FleetActionPath(id, verb), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	req.SetPathValue("verb", verb)
	rec := httptest.NewRecorder()
	f.srv.handleFleetCardAction(rec, req.WithContext(ctx))
	return rec
}

// rows is the owner's send-queue rows: body and fleet origin, oldest first.
func (f *proposeFixture) rows(t *testing.T) [][2]string {
	t.Helper()
	rs, err := f.pool.Query(authz.System(context.Background()), `SELECT body, coalesce(fleet_action_id, 0) FROM send_queue WHERE owner_id = $1 ORDER BY id`, f.owner)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	var out [][2]string
	for rs.Next() {
		var body string
		var origin int64
		if err := rs.Scan(&body, &origin); err != nil {
			t.Fatal(err)
		}
		out = append(out, [2]string{body, strconv.FormatInt(origin, 10)})
	}
	return out
}

// action is the owner's action id now.
func (f *proposeFixture) action(t *testing.T, id int64) fleet.Action {
	t.Helper()
	a, err := f.srv.ext.FleetActions.GetAction(f.asOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// header is the attribution line the fixture's owner's approval writes.
func (f *proposeFixture) header() string {
	return "[Chief, approved by user " + strconv.FormatInt(f.owner, 10) + " at " + f.at.Format("15:04") + " UTC]"
}

// TestAnApprovedCardQueuesExactlyOneRowWithTheFixedText: session_instruct
// continue files a proposal (nothing queued); the owner's Approve queues ONE
// send-queue row, its origin the action, its body muster's attribution line
// and the continue text byte for byte; the action is queued on that row. A
// second tap finds it decided and queues nothing more.
func TestAnApprovedCardQueuesExactlyOneRowWithTheFixedText(t *testing.T) {
	f := newProposeFixture(t)
	subject, _, _ := f.stopped(t, f.owner)
	id := f.propose(t, subject, fleet.ClassContinue)
	if got := f.rows(t); len(got) != 0 {
		t.Fatalf("a proposal queued %v before any tap", got)
	}
	rec := f.card(t, fresh(f.asOwner), id, "approve", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), FleetQueued) {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	want := f.header() + "\nYes, continue with what you proposed. Ask with attention_ask before anything destructive, outward or outside this task."
	got := f.rows(t)
	if len(got) != 1 || got[0][0] != want || got[0][1] != strconv.FormatInt(id, 10) {
		t.Fatalf("queued rows %q; want exactly one, origin %d:\n%q", got, id, want)
	}
	if a := f.action(t, id); a.State != fleet.StateQueued || a.SendQueueID == 0 || a.DecidedVia != fleet.ViaWeb || a.DecidedBy != f.owner {
		t.Fatalf("the action after approve: %+v", a)
	}
	if rec := f.card(t, fresh(f.asOwner), id, "approve", nil); !strings.Contains(rec.Body.String(), FleetDecided) || len(f.rows(t)) != 1 {
		t.Fatalf("a second approve: %s; rows %v", rec.Body.String(), f.rows(t))
	}
}

// TestACardWhoseEpisodeEndedIsRefusedAtTap: the stop the proposal was filed in
// ended before the tap: nothing is queued, the action is suppressed:moved_on
// and the card says so. Positive control: the same tap on a live stop queues.
func TestACardWhoseEpisodeEndedIsRefusedAtTap(t *testing.T) {
	f := newProposeFixture(t)
	gone, goneKey, _ := f.stopped(t, f.owner)
	live, _, _ := f.stopped(t, f.owner)
	goneID, liveID := f.propose(t, gone, fleet.ClassStopNote), f.propose(t, live, fleet.ClassStopNote)
	ep, ok, err := f.eps.OpenFor(f.asOwner, sessionstate.KindMirror, goneKey)
	if err != nil || !ok {
		t.Fatalf("the stop: %v %v", ok, err)
	}
	if _, err := f.eps.End(f.asOwner, ep.ID, "owner", time.Now()); err != nil {
		t.Fatal(err)
	}
	rec := f.card(t, fresh(f.asOwner), goneID, "approve", nil)
	if !strings.Contains(rec.Body.String(), FleetMovedOn) {
		t.Fatalf("the ended stop's tap: %s", rec.Body.String())
	}
	if a := f.action(t, goneID); a.State != fleet.Suppressed(fleet.GuardMovedOn) || a.FinishedAt.IsZero() {
		t.Fatalf("the ended stop's action: %+v", a)
	}
	if got := f.rows(t); len(got) != 0 {
		t.Fatalf("the ended stop queued %v", got)
	}
	f.card(t, fresh(f.asOwner), liveID, "approve", nil)
	if got := f.rows(t); len(got) != 1 || got[0][1] != strconv.FormatInt(liveID, 10) {
		t.Fatalf("control: the live stop's tap queued %v", got)
	}
}

// TestAnEditedTextRequiresAFreshSignIn: an Edit on a stale sign-in is 403 with
// X-Muster-Step-Up and sends nothing (the proposal stays open); on a fresh one
// the owner's own text is queued as THEIR row (no chief line, no fleet origin)
// and the action records the edit. An Approve on a stale sign-in is refused
// the same way.
func TestAnEditedTextRequiresAFreshSignIn(t *testing.T) {
	f := newProposeFixture(t)
	subject, _, _ := f.stopped(t, f.owner)
	id := f.propose(t, subject, fleet.ClassContinue)
	const mine = "Go on, but run the integration suite before you push."
	for _, verb := range []string{"edit", "approve"} {
		rec := f.card(t, stale(f.asOwner), id, verb, url.Values{"text": {mine}})
		if rec.Code != http.StatusForbidden || rec.Header().Get("X-Muster-Step-Up") != "required" {
			t.Fatalf("%s on a stale sign-in: %d %q", verb, rec.Code, rec.Header().Get("X-Muster-Step-Up"))
		}
	}
	if a := f.action(t, id); a.State != fleet.StateProposed || len(f.rows(t)) != 0 {
		t.Fatalf("a stale tap moved it: %+v rows %v", a, f.rows(t))
	}
	rec := f.card(t, fresh(f.asOwner), id, "edit", url.Values{"text": {mine}})
	if rec.Code != http.StatusOK {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body.String())
	}
	if got := f.rows(t); len(got) != 1 || got[0][0] != mine || got[0][1] != "0" {
		t.Fatalf("the edited text: %q; want the owner's own row %q with no fleet origin", got, mine)
	}
	if a := f.action(t, id); a.State != fleet.StateQueued || a.DecidedVia != fleet.ViaWebEdit || !strings.HasPrefix(a.Outcome, "edited") {
		t.Fatalf("the edited action: %+v", a)
	}
}

// TestAPhoneNotificationActionCannotApprove: a paired phone's signed request
// (authz.ViaDevice), even with a fresh sign-in, is refused before anything is
// read: the proposal stays open and nothing is queued. The batched push
// carries no action, and its tap opens the chief panel.
func TestAPhoneNotificationActionCannotApprove(t *testing.T) {
	f := newProposeFixture(t)
	subject, _, _ := f.stopped(t, f.owner)
	id := f.propose(t, subject, fleet.ClassContinue)
	phone := fresh(authz.WithPrincipal(context.Background(), authz.Principal{UserID: f.owner, Role: authz.RoleAdmin, Via: authz.ViaDevice}))
	for _, verb := range []string{"approve", "edit", "reject"} {
		rec := f.card(t, phone, id, verb, url.Values{"text": {"from the lock screen"}})
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), FleetNotFromPhone) {
			t.Fatalf("%s from a phone: %d %s", verb, rec.Code, rec.Body.String())
		}
	}
	if a := f.action(t, id); a.State != fleet.StateProposed || len(f.rows(t)) != 0 {
		t.Fatalf("a phone's tap moved it: %+v rows %v", a, f.rows(t))
	}
	n := fleetPushNotification(3)
	if n.AskActions || len(n.Data) != 1 || n.Data["url"] != "/?chief=open" || n.Title != "3 proposals waiting" {
		t.Fatalf("the batched push: %+v", n)
	}
}

// TestBulkApproveSendsTheRestAndReportsTheMovedOnRow: three proposals; one
// session moved on (its window went busy) before the bulk tap: two rows
// queued, one skipped, "2 sent, 1 skipped (moved on)."
func TestBulkApproveSendsTheRestAndReportsTheMovedOnRow(t *testing.T) {
	f := newProposeFixture(t)
	var ids []int64
	var movedWindow string
	for i := 0; i < 3; i++ {
		subject, _, w := f.stopped(t, f.owner)
		ids = append(ids, f.propose(t, subject, fleet.ClassContinue))
		if i == 1 {
			movedWindow = w
		}
	}
	f.mirror.mu.Lock()
	for i := range f.mirror.own[f.owner] {
		if f.mirror.own[f.owner][i].ID == movedWindow {
			f.mirror.own[f.owner][i].Status, f.mirror.own[f.owner][i].StatusSince = mirror.StatusBusy, 1700000099000
		}
	}
	f.mirror.mu.Unlock()
	form := url.Values{"op": {"approve"}}
	for _, id := range ids {
		form.Add("id", strconv.FormatInt(id, 10))
	}
	req := httptest.NewRequest(http.MethodPost, ui.FleetBulkPath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	f.srv.handleFleetBulk(rec, req.WithContext(fresh(f.asOwner)))
	if !strings.Contains(rec.Body.String(), "2 sent, 1 skipped (moved on).") {
		t.Fatalf("bulk: %d %s", rec.Code, rec.Body.String())
	}
	got := f.rows(t)
	if len(got) != 2 || got[0][1] != strconv.FormatInt(ids[0], 10) || got[1][1] != strconv.FormatInt(ids[2], 10) {
		t.Fatalf("bulk queued %v; want the first and third", got)
	}
	if a := f.action(t, ids[1]); a.State != fleet.Suppressed(fleet.GuardMovedOn) {
		t.Fatalf("the moved-on row: %+v", a)
	}
}

// TestAnotherOwnersSessionATeamWindowTheChiefAndAWorkingSessionAreRefused:
// every subject that is not a stopped session of the owner's own fleet is
// refused before anything is written — another owner's stopped window, a
// team's window the owner can see, the chief itself: the SAME not_found; a
// working session of the owner's: not_stopped. A second proposal of the same
// class: already_proposed. Positive control: the owner's own stop is filed.
func TestAnotherOwnersSessionATeamWindowTheChiefAndAWorkingSessionAreRefused(t *testing.T) {
	f := newProposeFixture(t)
	stranger := f.newMember(t)
	theirs, _, _ := f.stopped(t, stranger)
	teamKey := "7ea7ea70-0000-4000-8000-00000000ab12"
	f.mirror.teamOwner = stranger + 1_000
	f.mirror.team = []mirror.Window{{Session: "shared", ID: "@91", Index: 1, Claude: true, ClaudeSession: teamKey, Status: mirror.StatusIdle, StatusSince: 1}}
	working, _ := f.window(f.owner, mirror.StatusBusy)
	mine, _, _ := f.stopped(t, f.owner)
	for subject, want := range map[string]string{
		theirs:              fleetRefusedNotFound,
		"mirror:" + teamKey: fleetRefusedNotFound,
		"agent:" + strconv.FormatInt(f.chief.ID, 10): fleetRefusedNotFound,
		"mirror:" + working:                          fleetRefusedNotStopped,
	} {
		for _, name := range []string{sessionInstructTool, sessionWrapTool} {
			args := `{"subject":` + strconv.Quote(subject) + `,"reason":"x"}`
			if name == sessionInstructTool {
				args = `{"subject":` + strconv.Quote(subject) + `,"class":"continue","reason":"x"}`
			}
			if m := f.tool(t, f.asOwner, name, args); m["refused"] != want || m["action_id"] != nil {
				t.Errorf("%s on %s: %v; want refused %s", name, subject, m, want)
			}
		}
	}
	f.propose(t, mine, fleet.ClassContinue)
	if m := f.tool(t, f.asOwner, sessionInstructTool, `{"subject":`+strconv.Quote(mine)+`,"class":"continue","reason":"again"}`); m["refused"] != fleetRefusedAlready {
		t.Fatalf("a second continue: %v", m)
	}
	var n int
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM fleet_actions WHERE owner_id IN ($1, $2)`, f.owner, stranger).Scan(&n); err != nil || n != 1 {
		t.Fatalf("fleet_actions rows: %d (%v); want only the owner's own one", n, err)
	}
}

// TestFlagOffListsNoToolDrawsNoCardWritesNothing: with fleet proposals off
// (the default), the chief is offered none of the four tools, a call is
// refused `off`, the digest has no Proposed group, the card routes answer 404,
// and the pass proposes nothing for a finished session. Positive control: the
// same fleet with the flag on lists the tools, draws the group and the pass
// proposes the handoff.
func TestFlagOffListsNoToolDrawsNoCardWritesNothing(t *testing.T) {
	f := newProposeFixture(t)
	subject, key, _ := f.stopped(t, f.owner)
	f.pr(t, f.owner, key, "example-org/gearbox", 552, &githubapp.PRState{State: "closed", Merged: true, HeadSHA: "abcdef0123456789abcdef0123456789abcdef01", ClosedAt: fleetT0})
	names := map[string]bool{sessionInstructTool: true, sessionWrapTool: true, workReassignTool: true, fleetActionsListTool: true}
	listed := func() int {
		n := 0
		for _, d := range f.srv.MCPToolDefsFor(f.asOwner, f.chief) {
			if names[d.Name] {
				n++
			}
		}
		return n
	}
	count := func() int {
		var n int
		if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM fleet_actions WHERE owner_id = $1`, f.owner).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	f.srv.ext.FleetPropose = false
	if n := listed(); n != 0 {
		t.Errorf("flag off: %d fleet tools listed", n)
	}
	if m := f.tool(t, f.asOwner, sessionInstructTool, `{"subject":`+strconv.Quote(subject)+`,"class":"continue","reason":"x"}`); m["refused"] != fleetRefusedOff {
		t.Errorf("flag off: a call answered %v", m)
	}
	if html := f.digest(t, f.owner, false); strings.Contains(html, `data-fleet-group="proposed"`) || strings.Contains(html, "data-fleet-proposal") {
		t.Errorf("flag off: the digest draws the Proposed group")
	}
	if rec := f.card(t, fresh(f.asOwner), 1, "approve", nil); rec.Code != http.StatusNotFound {
		t.Errorf("flag off: a card route answered %d", rec.Code)
	}
	f.srv.fleetPass(f.asOwner, f.owner, f.srv.stallInputs(f.asOwner, nil))
	if n := count(); n != 0 {
		t.Fatalf("flag off: the pass wrote %d actions", n)
	}

	f.srv.ext.FleetPropose = true
	if n := listed(); n != 4 {
		t.Errorf("control: flag on lists %d fleet tools, want 4", n)
	}
	f.srv.fleetPass(f.asOwner, f.owner, f.srv.stallInputs(f.asOwner, nil))
	if n := count(); n != 1 {
		t.Fatalf("control: flag on, the pass proposed %d actions for the finished session, want 1", n)
	}
	list, _ := f.srv.ext.FleetActions.OpenActions(f.asOwner)
	if len(list) != 1 || list[0].Class != fleet.ClassHandoff || list[0].Proposer != fleet.ProposerSweep ||
		list[0].Text != "Your PR #552 is merged and nothing is in flight. Write your handoff (what is done, what is left, where), then stop." {
		t.Fatalf("control: the pass's proposal: %+v", list)
	}
	if html := f.digest(t, f.owner, false); !strings.Contains(html, `data-fleet-group="proposed"`) ||
		!strings.Contains(html, `data-fleet-proposal="`+strconv.FormatInt(list[0].ID, 10)+`"`) {
		t.Errorf("control: flag on, the digest lacks the proposal's card")
	}
	// Once per stop: the next pass (past its interval) proposes nothing more,
	// even after the owner rejected the first.
	f.srv.fleetReject(f.asOwner, list[0].ID, fleet.ViaWeb)
	f.srv.fleetPassAt.Delete(f.owner)
	f.srv.fleetPass(f.asOwner, f.owner, f.srv.stallInputs(f.asOwner, nil))
	if n := count(); n != 1 {
		t.Fatalf("a second pass on the same stop proposed again: %d actions", n)
	}
}

// TestTheDeliveryGateRereadsTheStop: a queued fleet row is pasted only while
// its action is queued on it and the session is still in its stop and idle
// streak; the window going busy (a new turn) refuses it and suppresses the
// action. Positive control: before the change the gate passes.
func TestTheDeliveryGateRereadsTheStop(t *testing.T) {
	f := newProposeFixture(t)
	subject, _, window := f.stopped(t, f.owner)
	id := f.propose(t, subject, fleet.ClassContinue)
	f.card(t, fresh(f.asOwner), id, "approve", nil)
	a := f.action(t, id)
	it, err := f.queue.Get(f.asOwner, a.SendQueueID)
	if err != nil || it.FleetActionID != id {
		t.Fatalf("the queued row: %+v %v", it, err)
	}
	if why := f.srv.sendQueueFleetGate(f.asOwner, it); why != "" {
		t.Fatalf("control: the gate refused a live stop: %s", why)
	}
	f.mirror.mu.Lock()
	for i := range f.mirror.own[f.owner] {
		if f.mirror.own[f.owner][i].ID == window {
			f.mirror.own[f.owner][i].Status, f.mirror.own[f.owner][i].StatusSince = mirror.StatusBusy, 1700000123000
		}
	}
	f.mirror.mu.Unlock()
	if why := f.srv.sendQueueFleetGate(f.asOwner, it); why != SendQueueFleetMoved {
		t.Fatalf("the window went busy: %q, want %q", why, SendQueueFleetMoved)
	}
	if a := f.action(t, id); a.State != fleet.Suppressed(fleet.GuardMovedOn) {
		t.Fatalf("the action after the gate: %+v", a)
	}
	// A fleet row is never edited or retried (sendqueue's own refusals).
	if _, err := f.queue.Hold(f.asOwner, it.ID); err != sendqueue.ErrNudgeNotEditable {
		t.Errorf("a fleet row held for edit: %v", err)
	}
}

// TestTheChiefTextIsNeverTheOwnersWords: muster's attribution line is one of
// its own pastes (nudgeInjectedPrefixes), so a pasted fleet text that was
// never in the send queue's record (a row since pruned) is still not the
// owner's last message: the owner's own line before it is, and it counts as
// one of muster's texts after it. Positive control: the owner's line alone.
func TestTheChiefTextIsNeverTheOwnersWords(t *testing.T) {
	t0 := time.Date(2001, 2, 3, 4, 5, 0, 0, time.UTC)
	own := chiefnudge.Msg{Role: "user", Text: "please tidy the release notes", ID: "11", At: t0}
	pasted := chiefnudge.Msg{Role: "user", Text: fleet.Body("Rowan Vale", t0, fleet.ContinueText), ID: "12", At: t0.Add(time.Minute)}
	last, ok, after := chiefnudge.SinceOwner([]chiefnudge.Msg{own, pasted}, nil, nudgeInjectedPrefixes)
	if !ok || last.ID != "11" || after != 1 {
		t.Fatalf("with the fleet text after the owner's: last %+v ok %v after %d; want the owner's line, 1 after", last, ok, after)
	}
	last, ok, after = chiefnudge.SinceOwner([]chiefnudge.Msg{own}, nil, nudgeInjectedPrefixes)
	if !ok || last.ID != "11" || after != 0 {
		t.Fatalf("control: %+v %v %d", last, ok, after)
	}
}

// closingMirror is the proposal fixture's mirror whose hosts advertise
// agent-close, recording each close it is asked for.
type closingMirror struct {
	observedMirror
	closes *[]mirror.AgentCloseReq
}

func (m closingMirror) Hosts(ctx context.Context) []mirror.HostView {
	hs := m.observedMirror.Hosts(ctx)
	for i := range hs {
		hs[i].AgentClose = true
	}
	return hs
}

func (m closingMirror) CloseAgent(_ context.Context, _ string, req mirror.AgentCloseReq) (mirror.AgentCloseResult, error) {
	*m.closes = append(*m.closes, req)
	return mirror.AgentCloseResult{Graceful: true, Reason: "the agent exited"}, nil
}

// setStatus sets owner's window w's status and its time.
func (f *proposeFixture) setStatus(owner int64, w, status string, since int64) {
	f.mirror.mu.Lock()
	defer f.mirror.mu.Unlock()
	for i := range f.mirror.own[owner] {
		if f.mirror.own[owner][i].ID == w {
			f.mirror.own[owner][i].Status, f.mirror.own[owner][i].StatusSince = status, since
		}
	}
}

// pass runs the owner's fleet step now (past its interval).
func (f *proposeFixture) pass() {
	f.srv.fleetPassAt.Delete(f.owner)
	f.srv.fleetPass(f.asOwner, f.owner, f.srv.stallInputs(f.asOwner, nil))
}

// TestAWrapClosesTheSessionOnlyAfterItsHandoffTurn: "Write handoff, then
// close" is one card. Approved, its handoff text is queued; once the row is
// sent the action waits (sent, open); while the session stays on the streak
// the handoff was sent on, nothing is closed; once it worked and stopped again
// (a new streak), its window is closed — the window the card named, holding
// its conversation — and the action finishes "closed". A second wrap, on a
// session with an open pull request, is left open and says why.
func TestAWrapClosesTheSessionOnlyAfterItsHandoffTurn(t *testing.T) {
	f := newProposeFixture(t)
	var closes []mirror.AgentCloseReq
	f.srv.ext.Mirror = closingMirror{observedMirror{f.mirror}, &closes}
	subject, key, window := f.stopped(t, f.owner)
	m := f.tool(t, f.asOwner, sessionWrapTool, `{"subject":`+strconv.Quote(subject)+`,"reason":"its work is merged"}`)
	id := int64(m["action_id"].(float64))
	f.card(t, fresh(f.asOwner), id, "approve", nil)
	got := f.rows(t)
	if len(got) != 1 || got[0][0] != f.header()+"\nWrite your handoff (what is done, what is left, where), then stop." {
		t.Fatalf("the wrap's handoff row: %q", got)
	}
	a := f.action(t, id)
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE send_queue SET state = 'sent', claimed_at = now(), finished_at = now() WHERE id = $1`, a.SendQueueID); err != nil {
		t.Fatal(err)
	}
	f.pass()
	if a = f.action(t, id); a.State != fleet.StateSent || !a.FinishedAt.IsZero() || a.CloseAfter != "since:1700000000000" {
		t.Fatalf("after the paste: %+v; want sent, open, waiting on its streak", a)
	}
	f.pass()
	if len(closes) != 0 {
		t.Fatalf("closed on the streak the handoff was sent on: %v", closes)
	}
	f.setStatus(f.owner, window, mirror.StatusIdle, 1700000777000)
	f.pass()
	if len(closes) != 1 || closes[0].Key != key || closes[0].Window != window || closes[0].Session != "work" {
		t.Fatalf("closes %+v; want exactly the card's window", closes)
	}
	if a = f.action(t, id); a.FinishedAt.IsZero() || !strings.HasPrefix(a.Outcome, "closed") {
		t.Fatalf("after the close: %+v", a)
	}

	// The refusal: an open pull request leaves the session open.
	subject2, key2, window2 := f.stopped(t, f.owner)
	f.pr(t, f.owner, key2, "example-org/sprocket", 707, &githubapp.PRState{State: "open", HeadSHA: "1234567890abcdef1234567890abcdef12345678"})
	m = f.tool(t, f.asOwner, sessionWrapTool, `{"subject":`+strconv.Quote(subject2)+`,"reason":"wrap it"}`)
	id2 := int64(m["action_id"].(float64))
	f.card(t, fresh(f.asOwner), id2, "approve", nil)
	a2 := f.action(t, id2)
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE send_queue SET state = 'sent', claimed_at = now(), finished_at = now() WHERE id = $1`, a2.SendQueueID); err != nil {
		t.Fatal(err)
	}
	f.pass()
	f.setStatus(f.owner, window2, mirror.StatusIdle, 1700000888000)
	f.pass()
	if a2 = f.action(t, id2); a2.FinishedAt.IsZero() || a2.Outcome != "not closed: it has an open pull request" || len(closes) != 1 {
		t.Fatalf("the open pull request's wrap: %+v, closes %d", a2, len(closes))
	}
}

// TestAReassignRoutesTheTaskThenAsksTheOldSessionForItsHandoff: one card, two
// steps on the tap: the task routed to `to` (a route row, its message queued
// to that window), then `from` told the task moved — a fleet row with muster's
// line and the reassign text. Before the tap, nothing.
func TestAReassignRoutesTheTaskThenAsksTheOldSessionForItsHandoff(t *testing.T) {
	f := newProposeFixture(t)
	f.srv.ext.Routes, f.srv.ext.Routing = f.plugins, &Routing{}
	from, _, _ := f.stopped(t, f.owner)
	to, _, _ := f.stopped(t, f.owner)
	n, err := f.notes.Create(f.asOwner, notes.Note{Title: "migrate the billing cron", Body: "move it to the new scheduler", Status: notes.StatusOpen})
	if err != nil {
		t.Fatal(err)
	}
	m := f.tool(t, f.asOwner, workReassignTool, `{"task_id":`+strconv.FormatInt(n.ID, 10)+`,"from":`+strconv.Quote(from)+`,"to":`+strconv.Quote(to)+`,"reason":"the other session owns the scheduler"}`)
	if m["state"] != fleet.StateProposed {
		t.Fatalf("work_reassign: %v", m)
	}
	id := int64(m["action_id"].(float64))
	if got := f.rows(t); len(got) != 0 {
		t.Fatalf("before the tap: %v", got)
	}
	if m := f.tool(t, f.asOwner, workReassignTool, `{"task_id":999999999,"from":`+strconv.Quote(from)+`,"to":`+strconv.Quote(to)+`,"reason":"x"}`); m["refused"] != fleetRefusedTask {
		t.Fatalf("another task: %v", m)
	}
	f.card(t, fresh(f.asOwner), id, "approve", nil)
	a := f.action(t, id)
	if a.RouteID == "" || a.State != fleet.StateQueued {
		t.Fatalf("after the tap: %+v", a)
	}
	var routes int
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM session_routes WHERE route_id = $1 AND task_id = $2`, a.RouteID, n.ID).Scan(&routes); err != nil || routes != 1 {
		t.Fatalf("the task's route: %d %v", routes, err)
	}
	want := f.header() + "\nTask #" + strconv.FormatInt(n.ID, 10) + " has moved to another session (" + a.ToSubject + "). Stop work on it. Write your handoff (what is done, what is left, where), then stop."
	var fleetRows int
	for _, r := range f.rows(t) {
		if r[1] == strconv.FormatInt(id, 10) {
			fleetRows++
			if r[0] != want && !strings.HasSuffix(r[0], "). Stop work on it. Write your handoff (what is done, what is left, where), then stop.") {
				t.Errorf("the from session's row: %q", r[0])
			}
		}
	}
	if fleetRows != 1 {
		t.Fatalf("fleet rows %d (all %v); want exactly the handoff to from", fleetRows, f.rows(t))
	}
}

// TestAProposalExpiresAfterTwelveHours: a card drawn 11 hours after its
// proposal is still there; 13 hours after, it is expired ("12 h passed") and
// not drawn. Its stop never ended (the re-read alone would keep it).
func TestAProposalExpiresAfterTwelveHours(t *testing.T) {
	f := newProposeFixture(t)
	subject, _, _ := f.stopped(t, f.owner)
	id := f.propose(t, subject, fleet.ClassStopNote)
	card := `data-fleet-proposal="` + strconv.FormatInt(id, 10) + `"`
	f.at = time.Now().Add(11 * time.Hour)
	if html := f.digest(t, f.owner, false); !strings.Contains(html, card) {
		t.Fatalf("11 h on, the card is gone")
	}
	f.at = time.Now().Add(13 * time.Hour)
	if html := f.digest(t, f.owner, false); strings.Contains(html, card) {
		t.Fatalf("13 h on, the card is still drawn")
	}
	if a := f.action(t, id); a.State != fleet.StateExpired || a.Outcome != "12 h passed" {
		t.Fatalf("the expired action: %+v", a)
	}
}

// TestAnotherOwnersProposalCannotBeDecided: another owner's approve, edit,
// reject and bulk naming this owner's proposal id decide nothing and send
// nothing — the proposal stays open. Positive control: the owner's own tap
// queues.
func TestAnotherOwnersProposalCannotBeDecided(t *testing.T) {
	f := newProposeFixture(t)
	stranger := f.newMember(t)
	subject, _, _ := f.stopped(t, f.owner)
	id := f.propose(t, subject, fleet.ClassContinue)
	theirs := fresh(f.as(stranger))
	for _, verb := range []string{"approve", "edit", "reject"} {
		f.card(t, theirs, id, verb, url.Values{"text": {"not mine to send"}})
	}
	req := httptest.NewRequest(http.MethodPost, ui.FleetBulkPath, strings.NewReader(url.Values{"op": {"approve"}, "id": {strconv.FormatInt(id, 10)}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	f.srv.handleFleetBulk(httptest.NewRecorder(), req.WithContext(theirs))
	if a := f.action(t, id); a.State != fleet.StateProposed || len(f.rows(t)) != 0 {
		t.Fatalf("another owner's taps moved it: %+v rows %v", a, f.rows(t))
	}
	f.card(t, fresh(f.asOwner), id, "approve", nil)
	if a := f.action(t, id); a.State != fleet.StateQueued || len(f.rows(t)) != 1 {
		t.Fatalf("control: the owner's own tap: %+v rows %v", a, f.rows(t))
	}
}

// TestConcurrentTapsOnOneCardQueueOneRowOnASmallPool: eight taps at once on
// one card (a double tap, two devices, Approve and Approve all together), on
// a 4-connection pool: exactly ONE row is queued, every tap answers, and none
// waits out a statement timeout (the claim is one conditional UPDATE: no
// connection is held across another's).
func TestConcurrentTapsOnOneCardQueueOneRowOnASmallPool(t *testing.T) {
	withPoolMaxConns(t, 4)
	f := newProposeFixture(t)
	subject, _, _ := f.stopped(t, f.owner)
	id := f.propose(t, subject, fleet.ClassContinue)
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = f.card(t, fresh(f.asOwner), id, "approve", nil).Code
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("tap %d answered %d", i, c)
		}
	}
	if got := f.rows(t); len(got) != 1 || got[0][1] != strconv.FormatInt(id, 10) {
		t.Fatalf("eight concurrent taps queued %v; want exactly one row", got)
	}
}

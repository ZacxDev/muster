package api

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/fleet"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/sessionstate"
)

// #796's ROUND 1: a queued close is never an agent turn and is repaired when
// stuck; a close card is never edited; a wrap's close runs off the pass; an
// officer — a host-spawned window or a dispatched instance — is not_found;
// the paste gate matches its row; a team principal decides no card; turning
// proposals off withdraws their notice. Every value is invented.

// fileClose files a close card on a stopped session the way the pass's
// fallback does (fleetProposeClose's proposal), as the owner, and returns it.
func (f *proposeFixture) fileClose(t *testing.T, subject string) fleet.Action {
	t.Helper()
	sub, refused, why := f.srv.fleetStoppedSubject(f.asOwner, subject)
	if refused != "" {
		t.Fatalf("%s: %s (%s)", subject, refused, why)
	}
	p := f.srv.fleetProposalFor(sub, fleet.ClassClose, fleetCloseText(""), "no handoff turn came")
	p.Proposer = fleet.ProposerSweep
	a, err := f.srv.ext.FleetActions.Propose(f.asOwner, p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// closing moves close card a as the tap does, to queued "closing" (the state
// fleetDeliverClose's background close holds it in).
func (f *proposeFixture) closing(t *testing.T, a fleet.Action) fleet.Action {
	t.Helper()
	st := f.srv.ext.FleetActions
	a, ok, err := st.MoveAction(f.asOwner, a.ID, []string{fleet.StateProposed}, fleet.Move{State: fleet.StateApproved, DecidedVia: fleet.ViaWeb, DecidedBy: f.owner})
	if err != nil || !ok {
		t.Fatalf("approve: %v %v", ok, err)
	}
	a, ok, err = st.MoveAction(f.asOwner, a.ID, []string{fleet.StateApproved}, fleet.Move{State: fleet.StateQueued, Outcome: "closing"})
	if err != nil || !ok {
		t.Fatalf("queue: %v %v", ok, err)
	}
	return a
}

// TestAQueuedCloseIsNeverATurnAndIsRepairedWhenStuck: a close card queued
// while its background close runs ("closing", no queue row) is left alone by
// the pass — an AGENT's is never sent as a turn (the agent branch keyed on
// "no queue row" took it), a window's never settled by a queue row it does
// not have — until it has been queued past fleetCloseRepairAfter (a restart
// mid-close): then it fails, saying the close was interrupted.
func TestAQueuedCloseIsNeverATurnAndIsRepairedWhenStuck(t *testing.T) {
	f := newProposeFixture(t)
	subject, _, _ := f.stopped(t, f.owner)
	win := f.closing(t, f.fileClose(t, subject))
	// An agent's close, as the store holds one: kind agent, no queue row.
	sub, _, _ := f.srv.fleetStoppedSubject(f.asOwner, subject)
	p := f.srv.fleetProposalFor(sub, fleet.ClassClose, fleetCloseText(""), "an agent's close")
	p.SubjectKind, p.SubjectKey, p.Host, p.TmuxSession, p.Window, p.AgentID = "agent", "9137:5512", "", "", "", 0
	ag, err := f.srv.ext.FleetActions.Propose(f.asOwner, p)
	if err != nil {
		t.Fatal(err)
	}
	ag = f.closing(t, ag)

	f.pass()
	for _, id := range []int64{win.ID, ag.ID} {
		if a := f.action(t, id); a.State != fleet.StateQueued || !a.FinishedAt.IsZero() || a.Outcome != "closing" {
			t.Fatalf("a close mid-close after a pass: %+v; want left queued, closing", a)
		}
	}
	f.at = f.at.Add(fleetCloseRepairAfter + time.Minute)
	f.pass()
	for _, id := range []int64{win.ID, ag.ID} {
		if a := f.action(t, id); a.State != fleet.StateFailed || a.FinishedAt.IsZero() || a.Outcome != "its close was interrupted: the session may still be open" {
			t.Fatalf("a close stuck past the repair: %+v; want failed, interrupted", a)
		}
	}
}

// TestACloseCardIsNeverEdited: a close card draws no Edit form, and an edit
// posted to it anyway is refused — nothing closed, the card still proposed.
// Control: a stop_note card on another session draws its Edit form.
func TestACloseCardIsNeverEdited(t *testing.T) {
	f := newProposeFixture(t)
	var closes []mirror.AgentCloseReq
	f.srv.ext.Mirror = closingMirror{observedMirror{f.mirror}, &closes}
	s1, _, _ := f.stopped(t, f.owner)
	s2, _, _ := f.stopped(t, f.owner)
	c := f.fileClose(t, s1)
	note := f.propose(t, s2, fleet.ClassStopNote)
	page := f.digest(t, f.owner, true)
	if strings.Contains(page, `data-fleet-edit="`+strconv.FormatInt(c.ID, 10)+`"`) {
		t.Fatalf("the close card draws an Edit button")
	}
	if !strings.Contains(page, `data-fleet-edit="`+strconv.FormatInt(note, 10)+`"`) {
		t.Fatalf("control: the stop_note card draws no Edit button")
	}
	rec := f.card(t, fresh(f.asOwner), c.ID, "edit", url.Values{"text": {"Please wrap up and push first."}})
	if !strings.Contains(rec.Body.String(), FleetNotEditable) {
		t.Fatalf("an edit of the close card: %s", rec.Body.String())
	}
	time.Sleep(100 * time.Millisecond) // a close would run in the background
	if a := f.action(t, c.ID); a.State != fleet.StateProposed || len(closesOf(&closes)) != 0 || len(f.rows(t)) != 0 {
		t.Fatalf("after the refused edit: %+v closes %d rows %v", a, len(closesOf(&closes)), f.rows(t))
	}
}

// blockingCloser is a closing mirror whose closes wait for release.
type blockingCloser struct {
	observedMirror
	mu      sync.Mutex
	n       int
	started chan struct{}
	release chan struct{}
}

func (m *blockingCloser) Hosts(ctx context.Context) []mirror.HostView {
	hs := m.observedMirror.Hosts(ctx)
	for i := range hs {
		hs[i].AgentClose = true
	}
	return hs
}

func (m *blockingCloser) CloseAgent(context.Context, string, mirror.AgentCloseReq) (mirror.AgentCloseResult, error) {
	m.mu.Lock()
	m.n++
	m.mu.Unlock()
	m.started <- struct{}{}
	<-m.release
	return mirror.AgentCloseResult{Graceful: true, Reason: "the agent exited"}, nil
}

func (m *blockingCloser) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.n
}

// TestAWrapsCloseRunsOffThePass: the wrap's close step runs in the
// background, so the session-state pass (every owner's pushes and nudges)
// returns while the close is still running; a second pass meanwhile starts
// no second close; released, the wrap finishes "closed".
func TestAWrapsCloseRunsOffThePass(t *testing.T) {
	f := newProposeFixture(t)
	bc := &blockingCloser{observedMirror: observedMirror{f.mirror}, started: make(chan struct{}, 4), release: make(chan struct{})}
	f.srv.ext.Mirror = bc
	subject, _, window := f.stopped(t, f.owner)
	m := f.tool(t, f.asOwner, sessionWrapTool, `{"subject":`+strconv.Quote(subject)+`,"reason":"its work is merged"}`)
	id := int64(m["action_id"].(float64))
	f.card(t, fresh(f.asOwner), id, "approve", nil)
	a := f.action(t, id)
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE send_queue SET state = 'sent', claimed_at = now(), finished_at = now() WHERE id = $1`, a.SendQueueID); err != nil {
		t.Fatal(err)
	}
	f.pass()
	f.setStatus(f.owner, window, mirror.StatusIdle, 1700000999000)
	done := make(chan struct{})
	go func() { f.pass(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		close(bc.release)
		t.Fatal("the pass waited on the wrap's close")
	}
	select {
	case <-bc.started:
	case <-time.After(5 * time.Second):
		t.Fatal("no close started")
	}
	f.pass() // the close is still running
	if n := bc.count(); n != 1 {
		t.Fatalf("closes started while one runs: %d; want 1", n)
	}
	close(bc.release)
	if a = f.waitFinished(t, id); !strings.HasPrefix(a.Outcome, "closed") {
		t.Fatalf("after the close: %+v", a)
	}
}

// TestAnOfficerIsNotFound: an officer's instance is never a fleet subject —
// a HOST-SPAWNED one, seen as a mirrored window the spawn store recorded,
// and a dispatched instance row the delete protection does not hold (a pod
// row of a host officer: officerOf, not officerOfRow) — each refused
// not_found, as another owner's session is. Controls: an ordinary window and
// an ordinary agent, stopped the same way, are not refused not_found.
func TestAnOfficerIsNotFound(t *testing.T) {
	f := newProposeFixture(t)
	host := "bench-" + strconv.FormatInt(f.owner, 10)
	declareHostOfficerOn(t, f.srv, f.owner, host)
	const officerID = "marketing:stepup"
	instruct := func(subject string) map[string]any {
		return f.tool(t, f.asOwner, sessionInstructTool, `{"subject":`+strconv.Quote(subject)+`,"class":"stop_note","reason":"it should pause"}`)
	}

	offSubject, offKey, offWindow := f.stopped(t, f.owner)
	mem := f.srv.officerHostSpawns().(*officers.MemHostSpawnStore)
	sp, err := mem.InsertHostSpawn(f.asOwner, officerID, officers.InstanceName(officerID, 1), host)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := mem.RecordHostSpawnWindow(f.asOwner, sp.ID, host, offWindow, offKey); err != nil || !ok {
		t.Fatalf("record the spawn's window: %v %v", ok, err)
	}
	if m := instruct(offSubject); m["refused"] != fleetRefusedNotFound {
		t.Fatalf("a host officer's window: %v; want not_found", m)
	}
	plain, _, _ := f.stopped(t, f.owner)
	if m := instruct(plain); m["state"] != fleet.StateProposed {
		t.Fatalf("control: an ordinary window: %v", m)
	}

	stoppedAgent := func(name string) string {
		a, err := agents.NewPG(f.pool).Create(f.as(f.owner), agents.Agent{Name: name, Namespace: "devpod-" + name, DisplayName: name,
			Status: agents.StatusRunning, HooksToken: "fleet-r1-" + name})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM agents WHERE id = $1`, a.ID)
		})
		if _, ok, err := f.eps.Open(f.as(f.owner), sessionstate.Episode{Kind: sessionstate.KindAgent, Key: strconv.FormatInt(a.ID, 10) + ":1",
			State: sessionstate.StateStalled, Asked: true, AgentID: a.ID, StreakKey: "msg:4471"}); err != nil || !ok {
			t.Fatalf("open the agent's stop: %v %v", ok, err)
		}
		return "agent:" + strconv.FormatInt(a.ID, 10)
	}
	pod := stoppedAgent(officers.InstanceName(officerID, 1))
	if m := instruct(pod); m["refused"] != fleetRefusedNotFound {
		t.Fatalf("a pod row of a host officer: %v; want not_found", m)
	}
	if m := instruct(stoppedAgent("fleet-r1-plain")); m["refused"] == fleetRefusedNotFound {
		t.Fatalf("control: an ordinary agent is not_found: %v", m)
	}
}

// TestThePasteGateMatchesItsOwnRow: a queue row naming a queued action is
// pasted only if it IS the action's row (send_queue_id): another row naming
// the same action is refused "no longer waiting". Control: its own row passes.
func TestThePasteGateMatchesItsOwnRow(t *testing.T) {
	f := newProposeFixture(t)
	subject, _, _ := f.stopped(t, f.owner)
	id := f.propose(t, subject, fleet.ClassStopNote)
	f.card(t, fresh(f.asOwner), id, "approve", nil)
	a := f.action(t, id)
	if a.State != fleet.StateQueued || a.SendQueueID == 0 {
		t.Fatalf("fixture: %+v", a)
	}
	if got := f.srv.sendQueueFleetGate(f.asOwner, sendqueue.Item{ID: a.SendQueueID + 7919, FleetActionID: id}); !strings.HasPrefix(got, SendQueueFleetClosed) {
		t.Fatalf("another row naming the action: %q; want refused", got)
	}
	if got := f.srv.sendQueueFleetGate(f.asOwner, sendqueue.Item{ID: a.SendQueueID, FleetActionID: id}); got != "" {
		t.Fatalf("control: its own row: %q; want pasted", got)
	}
}

// TestATeamPrincipalDecidesNoCard: a team's acting principal (a team acts
// only through its own doors, here an agent's) is refused as "not your card"
// — the team refusal itself, not the door refusal a person at that door gets
// (the control that tells the two apart). A person in a browser may act.
func TestATeamPrincipalDecidesNoCard(t *testing.T) {
	team := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 4242, Role: authz.RoleMember, Team: true, Via: authz.ViaAgent})
	if got := fleetCardRefusal(team); got != "not your card" {
		t.Fatalf("a team principal: %q", got)
	}
	personAtTheDoor := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 4243, Role: authz.RoleMember, Via: authz.ViaAgent})
	if got := fleetCardRefusal(personAtTheDoor); got != "a fleet proposal is decided only by a person signed in to muster in a browser" {
		t.Fatalf("control: a person at an agent's door: %q", got)
	}
	person := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 4243, Role: authz.RoleMember, Via: authz.ViaSession})
	if got := fleetCardRefusal(person); got != "" {
		t.Fatalf("control: a person in a browser: %q", got)
	}
}

// TestTurningProposalsOffWithdrawsTheirNotice: the "N proposals waiting"
// notice filed while proposals were on is withdrawn by the first pass after
// they are turned off — while the proposal itself still waits (so it is the
// switch, not "nothing waits", that withdrew it).
func TestTurningProposalsOffWithdrawsTheirNotice(t *testing.T) {
	f := newProposeFixture(t)
	notices := func() int {
		open, err := f.srv.ext.Attention.Open(f.asOwner)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, a := range open {
			if a.Kind == attention.KindNotice && a.Asker.Label == fleetNoticeLabel {
				n++
			}
		}
		return n
	}
	subject, _, _ := f.stopped(t, f.owner)
	id := f.propose(t, subject, fleet.ClassContinue)
	f.pass()
	if n := notices(); n != 1 {
		t.Fatalf("fixture: notices %d", n)
	}
	f.srv.ext.FleetPropose = false
	f.pass()
	if n := notices(); n != 0 {
		t.Fatalf("after turning proposals off: %d notice(s) open", n)
	}
	if a := f.action(t, id); a.State != fleet.StateProposed {
		t.Fatalf("the proposal: %+v; want still proposed", a)
	}
}

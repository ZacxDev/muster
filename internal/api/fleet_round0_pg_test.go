package api

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/fleet"
	"github.com/ZacxDev/muster/internal/githubapp"
	"github.com/ZacxDev/muster/internal/mirror"
)

// #796's ROUND 0, as the operator answered it: the chief stays the stop judge
// (the fleet runs alongside); one "Write handoff, then close" card with A8's
// close fallback; one "N proposals waiting" Attention item beside the push.

// TestTheFleetDoesNotJudgeStopsInThisPhase: with fleet proposals on, session
// state on and a chief, the stop judge is still the chief — and the one seam
// where phase 3 lets the fleet judge, fleetJudgesStops, answers false
// unconditionally (its body is `return false`, read from the source).
func TestTheFleetDoesNotJudgeStopsInThisPhase(t *testing.T) {
	f := newProposeFixture(t)
	if !f.srv.fleetProposeOn() {
		t.Fatal("fixture: fleet proposals are off")
	}
	if got := f.srv.stopJudgeOf(f.asOwner, f.owner); got != judgeChief {
		t.Fatalf("stopJudgeOf with fleet proposals on = %q; want %q (the chief judges until phase 3)", got, judgeChief)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "stop_judge.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "fleetJudgesStops" {
			continue
		}
		found = true
		ret, ok := fn.Body.List[0].(*ast.ReturnStmt)
		if len(fn.Body.List) != 1 || !ok || len(ret.Results) != 1 {
			t.Fatalf("fleetJudgesStops is not a single return")
		}
		if id, ok := ret.Results[0].(*ast.Ident); !ok || id.Name != "false" {
			t.Fatalf("fleetJudgesStops does not return false: the fleet would judge stops before phase 3")
		}
	}
	if !found {
		t.Fatal("instrument: no fleetJudgesStops in stop_judge.go")
	}
}

// TestTheChiefJudgesLiveStopsWithTheFleetOnOrOff: the chief stays the judge
// BEHAVIOURALLY, at both of the judge's readers — not only at stopJudgeOf —
// over fleet proposals off and on (session state on, the owner's nudges live):
// episodeChiefJudges answers true (a stop's push waits for the chief), and the
// chief's sweep records a decision for the stalled window. A gate on the flag
// at either reader (`s.fleetProposeOn() || …`) turns the "on" row red.
func TestTheChiefJudgesLiveStopsWithTheFleetOnOrOff(t *testing.T) {
	for _, on := range []bool{false, true} {
		// A subtest each: the fixture's rows are cleaned up between the rows.
		t.Run(map[bool]string{false: "fleet off", true: "fleet on"}[on], func(t *testing.T) {
			f := newStateFixture(t, true)
			f.live()
			if on {
				f.srv.ext.FleetPropose, f.srv.ext.FleetDigest, f.srv.ext.FleetActions = true, true, fleet.NewPG(f.pool)
			}
			if f.srv.fleetProposeOn() != on {
				t.Fatalf("fixture: fleet proposals on = %v; want %v", f.srv.fleetProposeOn(), on)
			}
			octx := f.srv.attentionPrincipal(context.Background(), authz.OperatorUserID)
			if !f.srv.episodeChiefJudges(octx, authz.OperatorUserID) {
				t.Fatalf("fleet proposals on=%v: episodeChiefJudges is false; the chief judges live stops", on)
			}
			f.statePass(t) // opens the window's stop
			if n := f.pass(t); n != 1 {
				t.Fatalf("fleet proposals on=%v: the chief's sweep decided %d; want 1", on, n)
			}
		})
	}
}

// TestAnActionInFlightSettlesWhateverTheJudge: an approved card's queued row
// that was sent settles the action on the next pass while the CHIEF judges
// the owner's stops (round 0's lead: it settled only while the fleet judged).
func TestAnActionInFlightSettlesWhateverTheJudge(t *testing.T) {
	f := newProposeFixture(t)
	if got := f.srv.stopJudgeOf(f.asOwner, f.owner); got != judgeChief {
		t.Fatalf("fixture: the judge is %q", got)
	}
	subject, _, _ := f.stopped(t, f.owner)
	id := f.propose(t, subject, fleet.ClassStopNote)
	f.card(t, fresh(f.asOwner), id, "approve", nil)
	a := f.action(t, id)
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE send_queue SET state = 'sent', claimed_at = now(), finished_at = now() WHERE id = $1`, a.SendQueueID); err != nil {
		t.Fatal(err)
	}
	f.pass()
	if a = f.action(t, id); a.State != fleet.StateSent || a.FinishedAt.IsZero() {
		t.Fatalf("after the pass: %+v; want sent and finished", a)
	}
}

// TestTheFinishedSessionsCardIsWriteHandoffThenClose: the pass proposes ONE
// "Write handoff, then close" card (A8), never a separate handoff card; and
// when no handoff turn comes within 24 h of its tap, the close is offered as a
// card of its own (the fallback), which closes the window when tapped.
func TestTheFinishedSessionsCardIsWriteHandoffThenClose(t *testing.T) {
	f := newProposeFixture(t)
	var closes []mirror.AgentCloseReq
	f.srv.ext.Mirror = closingMirror{observedMirror{f.mirror}, &closes}
	_, key, window := f.stopped(t, f.owner)
	f.pr(t, f.owner, key, "example-org/turbine", 818, &githubapp.PRState{State: "closed", Merged: true,
		HeadSHA: "9876543210abcdef9876543210abcdef98765432", ClosedAt: fleetT0})
	f.pass()
	list, _ := f.srv.ext.FleetActions.OpenActions(f.asOwner)
	if len(list) != 1 || list[0].Class != fleet.ClassWrap || list[0].Proposer != fleet.ProposerSweep {
		t.Fatalf("the pass's proposals %+v; want exactly one wrap", list)
	}
	id := list[0].ID
	f.card(t, fresh(f.asOwner), id, "approve", nil)
	a := f.action(t, id)
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE send_queue SET state = 'sent', claimed_at = now(), finished_at = now() WHERE id = $1`, a.SendQueueID); err != nil {
		t.Fatal(err)
	}
	f.pass()
	// No handoff turn: 23 h on, still waiting; 25 h on, the close is offered.
	f.at = time.Now().Add(23 * time.Hour)
	f.pass()
	if a = f.action(t, id); !a.FinishedAt.IsZero() {
		t.Fatalf("23 h on: %+v; want still waiting", a)
	}
	f.at = time.Now().Add(25 * time.Hour)
	f.pass()
	if a = f.action(t, id); a.FinishedAt.IsZero() || !strings.Contains(a.Outcome, "a close card is offered") {
		t.Fatalf("25 h on: %+v", a)
	}
	list, _ = f.srv.ext.FleetActions.OpenActions(f.asOwner)
	if len(list) != 1 || list[0].Class != fleet.ClassClose || list[0].SubjectKey != key {
		t.Fatalf("the fallback: %+v; want one close card on the session", list)
	}
	if len(closesOf(&closes)) != 0 {
		t.Fatalf("closed before any tap: %v", closes)
	}
	f.at = time.Now()
	rec := f.card(t, fresh(f.asOwner), list[0].ID, "approve", nil)
	if !strings.Contains(rec.Body.String(), FleetClosing) {
		t.Fatalf("approving the close: %s", rec.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for f.action(t, list[0].ID).FinishedAt.IsZero() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if c := f.action(t, list[0].ID); !strings.HasPrefix(c.Outcome, "closed") || len(closesOf(&closes)) != 1 || closesOf(&closes)[0].Window != window {
		t.Fatalf("the close card: %+v, closes %+v", c, closes)
	}
}

// TestOneProposalsNoticeUpdatedNotStacked: with the batched push, ONE
// "N proposals waiting" Attention item (a notice): a later push replaces it
// with the new count (one open, never two); once nothing waits it is
// withdrawn.
func TestOneProposalsNoticeUpdatedNotStacked(t *testing.T) {
	f := newProposeFixture(t)
	notices := func() []attention.Ask {
		open, err := f.srv.ext.Attention.Open(f.asOwner)
		if err != nil {
			t.Fatal(err)
		}
		var out []attention.Ask
		for _, a := range open {
			if a.Kind == attention.KindNotice && a.Asker.Label == fleetNoticeLabel {
				out = append(out, a)
			}
		}
		return out
	}
	s1, _, _ := f.stopped(t, f.owner)
	first := f.propose(t, s1, fleet.ClassContinue)
	f.pass()
	if n := notices(); len(n) != 1 || n[0].Question != fleetNoticeText(1) {
		t.Fatalf("after the first push: %+v", n)
	}
	s2, _, _ := f.stopped(t, f.owner)
	second := f.propose(t, s2, fleet.ClassStopNote)
	dateBackPush(t, f, 31*time.Minute)
	f.pass()
	if n := notices(); len(n) != 1 || n[0].Question != fleetNoticeText(2) {
		t.Fatalf("after the second push: %+v; want ONE notice saying 2", n)
	}
	f.srv.fleetReject(f.asOwner, first, fleet.ViaWeb)
	f.srv.fleetReject(f.asOwner, second, fleet.ViaWeb)
	f.pass()
	if n := notices(); len(n) != 0 {
		t.Fatalf("nothing waits, yet %d notice(s) are open", len(n))
	}
}

// dateBackPush moves the owner's last batched push back by d.
func dateBackPush(t *testing.T, f *proposeFixture, d time.Duration) {
	t.Helper()
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE fleet_owner_marks SET proposals_pushed_at = proposals_pushed_at - $2 * interval '1 second'
		WHERE owner_id = $1`, f.owner, int(d.Seconds())); err != nil {
		t.Fatal(err)
	}
}

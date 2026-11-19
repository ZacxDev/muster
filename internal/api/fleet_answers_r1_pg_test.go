package api

import (
	"context"
	"html"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/asklog"
	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/cichecks"
	"github.com/ZacxDev/muster/internal/fleet"
	"github.com/ZacxDev/muster/internal/githubapp"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/sessionstate"
)

// #804's ROUND 1: a pick card is never edited; an officer's window gets no
// ci_result and no send; the team refusals and the owner checks reached; an
// outside-turn card's text capped at 4 KiB. Every value is invented.

// teamCtx is owner's account acting as a TEAM (its acting principal: a team
// acts only through its own doors, here an agent's).
func teamCtx(owner int64) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: owner, Role: authz.RoleMember, Team: true, Via: authz.ViaAgent})
}

// personAtAgentDoor is the same account as a person at the same door: the
// control that differs from teamCtx in Team alone.
func personAtAgentDoor(owner int64) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: owner, Role: authz.RoleAdmin, Via: authz.ViaAgent})
}

// heldPicker opens a picker on owner's window key, returning its entry.
func (f *fleetAnswerFixture) heldPicker(t *testing.T, owner int64, key, tool string) askq.Entry {
	t.Helper()
	qs := []askq.Question{{Question: "Delete the stale branch?", Options: []askq.Option{{Label: "Delete it"}, {Label: "Keep it"}}}}
	e, err := f.srv.asks.Open(askq.Key{Owner: owner, Source: askSourceMirror, Scope: "bench-" + strconv.FormatInt(owner, 10), Session: key, ToolUseID: tool}, qs, "")
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// TestAPickCardIsNeverEdited (🔴-1): a pick card draws no Edit form, and an
// edit posted to it is refused FleetNotEditable — nothing typed, the card
// still proposed (before, the edit typed the chief's ORIGINAL option and
// recorded it sent). Control: a plain approve of the same card types it.
func TestAPickCardIsNeverEdited(t *testing.T) {
	f := newFleetAnswerFixture(t)
	key, window := f.window(f.owner, mirror.StatusWaiting)
	f.stop(t, f.owner, key, window, sessionstate.StateNeedsYou, true, fleetT0)
	f.heldPicker(t, f.owner, key, "toolu_01EditPickR1aaaa")
	m := f.tool(t, f.asOwner, sessionAnswerTool, `{"subject":"mirror:`+key+`","answer_kind":"pick","option":"Delete it","reason":"it is merged"}`)
	id := int64(m["action_id"].(float64))
	if page := f.digest(t, f.owner, true); strings.Contains(page, `data-fleet-edit="`+strconv.FormatInt(id, 10)+`"`) {
		t.Fatalf("the pick card draws an Edit button")
	}
	rec := f.card(t, fresh(f.asOwner), id, "edit", url.Values{"text": {"Keep it -- do not delete"}})
	if !strings.Contains(rec.Body.String(), FleetNotEditable) || f.asking.count() != 0 {
		t.Fatalf("an edit of the pick: typed %d; %s", f.asking.count(), rec.Body.String())
	}
	if a := f.action(t, id); a.State != fleet.StateProposed {
		t.Fatalf("after the refused edit: %+v", a)
	}
	f.card(t, fresh(f.asOwner), id, "approve", nil)
	if f.asking.count() != 1 || f.asking.typed[0][0].Picks[0] != 0 {
		t.Fatalf("control: the approve typed %+v; want option 1", f.asking.typed)
	}
}

// TestAnOfficersWindowGetsNoCIResultAndNoSend (🟡-2): a host-spawned
// officer's window whose CI finished after its stop gets no ci_result card
// (control: an ordinary window in the same state gets one); and a card filed
// on a window that then became an officer's is not sent at the tap.
func TestAnOfficersWindowGetsNoCIResultAndNoSend(t *testing.T) {
	f := newFleetAnswerFixture(t)
	host := "bench-" + strconv.FormatInt(f.owner, 10)
	declareHostOfficerOn(t, f.srv, f.owner, host)
	const officerID = "marketing:stepup"
	mem := f.srv.officerHostSpawns().(*officers.MemHostSpawnStore)
	spawn := func(key, window string) {
		sp, err := mem.InsertHostSpawn(f.asOwner, officerID, officers.InstanceName(officerID, 1), host)
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := mem.RecordHostSpawnWindow(f.asOwner, sp.ID, host, window, key); err != nil || !ok {
			t.Fatalf("record the spawn's window: %v %v", ok, err)
		}
	}
	finished := cichecks.Checks{Runs: []cichecks.Run{{ID: 31, Name: "unit", Status: "completed", Conclusion: "success", CompletedAt: fleetT0.Add(4 * 60e9)}}}
	_, offKey, offWindow := f.stopped(t, f.owner)
	f.pr(t, f.owner, offKey, "example-org/bobbin", 431, &githubapp.PRState{State: "open", HeadSHA: ansHead})
	f.ci.set("example-org/bobbin", ansHead, finished)
	spawn(offKey, offWindow)
	_, plainKey, _ := f.stopped(t, f.owner)
	f.pr(t, f.owner, plainKey, "example-org/spindle", 432, &githubapp.PRState{State: "open", HeadSHA: newHead})
	f.ci.set("example-org/spindle", newHead, finished)
	f.pass()
	list, _ := f.srv.ext.FleetActions.OpenActions(f.asOwner)
	var on []string
	for _, a := range list {
		if a.Class == fleet.ClassCIResult {
			on = append(on, a.SubjectKey)
		}
	}
	if len(on) != 1 || on[0] != plainKey {
		t.Fatalf("ci_result cards on %v; want only the ordinary window %s", on, plainKey)
	}

	// Filed before the window was an officer's; tapped after: not sent.
	later, laterKey, laterWindow := f.stopped(t, f.owner)
	id := f.propose(t, later, fleet.ClassStopNote)
	spawn(laterKey, laterWindow)
	if rec := f.card(t, fresh(f.asOwner), id, "approve", nil); !strings.Contains(rec.Body.String(), FleetMovedOn) || len(f.rows(t)) != 0 {
		t.Fatalf("a card on a window that became an officer's: %s rows %v", rec.Body.String(), f.rows(t))
	}
}

// TestATeamPrincipalIsRefusedAtTheCallAndFilesNoOutsideTurnCard (🟡-3): the
// fleet tools called as a TEAM are refused `off` (the call-time refusal: the
// listing is a different layer, and a gateway runtime can call a name it was
// not offered); and route_to_session outside a turn files no card for a
// team's chief (fleetCardsFor). Controls: the same account as a person, at
// the same door, files both.
func TestATeamPrincipalIsRefusedAtTheCallAndFilesNoOutsideTurnCard(t *testing.T) {
	f := newFleetAnswerFixture(t)
	f.srv.ext.Routes, f.srv.ext.Routing = f.plugins, &Routing{}
	s1, _, _ := f.stopped(t, f.owner)
	s2, _, _ := f.stopped(t, f.owner)
	args := `{"subject":` + strconv.Quote(s1) + `,"class":"stop_note","reason":"it should pause"}`
	if m := f.tool(t, teamCtx(f.owner), sessionInstructTool, args); m["refused"] != fleetRefusedOff {
		t.Fatalf("a team's call: %v; want off", m)
	}
	if m := f.tool(t, personAtAgentDoor(f.owner), sessionInstructTool, args); m["state"] != fleet.StateProposed {
		t.Fatalf("control: the person's call: %v", m)
	}
	route := `{"subject":` + strconv.Quote(s2) + `,"text":"Post the benchmark numbers.","owner_asked":true}`
	if out := f.srv.toolRouteToSession(teamCtx(f.owner), f.chief, route); strings.Contains(out, `"state":"proposed"`) {
		t.Fatalf("a team's chief outside a turn filed a card: %s", out)
	}
	if out := f.srv.toolRouteToSession(personAtAgentDoor(f.owner), f.chief, route); !strings.Contains(out, `"state":"proposed"`) {
		t.Fatalf("control: the person's chief outside a turn: %s", out)
	}
}

// TestAHeldQuestionIsTheOwnersOwn (🟡-3, fleetAskOn's owner check): an ask-log
// row of another owner — invisible under row-level security, but NOT when an
// operator has turned it off (db/rollback/rls_off.sql) — is never the held
// question, all else equal. Control: the owner's own row is.
func TestAHeldQuestionIsTheOwnersOwn(t *testing.T) {
	ep := &sessionstate.Episode{ID: 7701, Kind: sessionstate.KindMirror, Key: "5e55a1d0-own-q", Host: "rig-east", Window: "@44"}
	sub := fleetSubject{kind: "mirror", key: ep.Key, episode: ep}
	q := asklog.Ask{ID: 9, Owner: 31, Kind: asklog.KindPlainText, Host: "rig-east", Window: "@44", ClaudeSession: ep.Key, EpisodeID: ep.ID}
	if !fleetAskOn(q, sub, 31) {
		t.Fatalf("control: the owner's own question is not held")
	}
	if fleetAskOn(q, sub, 32) {
		t.Fatalf("another owner's question is held")
	}
}

// TestAPicksTapReReadsThePickersOwner (🟡-3, fleetPickStillOn's owner check):
// a pick action whose picker is ANOTHER owner's entry (same tool id, same
// stop) is moved on at the tap. Control: the owner's own entry holds.
func TestAPicksTapReReadsThePickersOwner(t *testing.T) {
	f := newFleetAnswerFixture(t)
	key, window := f.window(f.owner, mirror.StatusWaiting)
	f.stop(t, f.owner, key, window, sessionstate.StateNeedsYou, true, fleetT0)
	const tool = "toolu_01OwnerPickR1bbbb"
	own := f.heldPicker(t, f.owner, key, tool)
	m := f.tool(t, f.asOwner, sessionAnswerTool, `{"subject":"mirror:`+key+`","answer_kind":"pick","option":"Keep it","reason":"x"}`)
	a := f.action(t, int64(m["action_id"].(float64)))
	if a.Evidence.AskID != own.ID {
		t.Fatalf("fixture: %+v", a.Evidence)
	}
	if st := f.srv.fleetPickStillOn(f.asOwner, a); st != nudgeStalled {
		t.Fatalf("control: the owner's own picker: %v", st)
	}
	other := f.heldPicker(t, f.owner+90001, key, tool)
	a.Evidence.AskID = other.ID
	if st := f.srv.fleetPickStillOn(f.asOwner, a); st != nudgeMovedOn {
		t.Fatalf("another owner's picker: %v; want moved on", st)
	}
}

// TestAnOutsideTurnCardsTextIsAtMost4KiB (🟢-4): route_to_session and
// route_many outside a turn file a card's text, at most 4 KiB like custom's:
// one byte over is refused too_long (route_many: each subject, by
// fleetRouteCard's one cap) and files nothing; exactly 4 KiB files.
func TestAnOutsideTurnCardsTextIsAtMost4KiB(t *testing.T) {
	f := newFleetAnswerFixture(t)
	f.srv.ext.Routes, f.srv.ext.Routing = f.plugins, &Routing{}
	s1, _, _ := f.stopped(t, f.owner)
	s2, _, _ := f.stopped(t, f.owner)
	over, exact := strings.Repeat("q", fleet.MaxCustomBytes+1), strings.Repeat("r", fleet.MaxCustomBytes)
	if out := f.srv.toolRouteToSession(f.asOwner, f.chief, `{"subject":`+strconv.Quote(s1)+`,"text":"`+over+`","owner_asked":true}`); !strings.Contains(out, fleetRefusedTooLong) {
		t.Fatalf("route_to_session, 4 KiB + 1: %.200s", out)
	}
	if out := f.srv.toolRouteMany(f.asOwner, f.chief, `{"subjects":[`+strconv.Quote(s1)+`,`+strconv.Quote(s2)+`],"text":"`+over+`","owner_asked":true}`); !strings.Contains(out, fleetRefusedTooLong) {
		t.Fatalf("route_many, 4 KiB + 1: %.200s", out)
	}
	if list, _ := f.srv.ext.FleetActions.OpenActions(f.asOwner); len(list) != 0 {
		t.Fatalf("an over-long text filed %d cards", len(list))
	}
	if out := f.srv.toolRouteToSession(f.asOwner, f.chief, `{"subject":`+strconv.Quote(s1)+`,"text":"`+exact+`","owner_asked":true}`); !strings.Contains(out, `"state":"proposed"`) {
		t.Fatalf("control: exactly 4 KiB: %.200s", out)
	}
}

// TestAnUnreadableSpawnStoreSendsNothing (round 2): when the officers' spawn
// store cannot be read, whether a window is an officer's is UNKNOWN — the tap
// queues nothing (the card stays proposed, "could not read"), and a row
// already queued is held at the paste (SendQueueNudgeWait), never pasted.
// Controls: before the store failed, the same tap queued its row.
func TestAnUnreadableSpawnStoreSendsNothing(t *testing.T) {
	f := newFleetAnswerFixture(t)
	declareHostOfficerOn(t, f.srv, f.owner, "bench-"+strconv.FormatInt(f.owner, 10))
	s1, _, _ := f.stopped(t, f.owner)
	s2, _, _ := f.stopped(t, f.owner)
	id1 := f.propose(t, s1, fleet.ClassStopNote)
	id2 := f.propose(t, s2, fleet.ClassContinue)
	f.card(t, fresh(f.asOwner), id1, "approve", nil)
	a1 := f.action(t, id1)
	if a1.State != fleet.StateQueued || a1.SendQueueID == 0 || len(f.rows(t)) != 1 {
		t.Fatalf("control: the tap before the store failed: %+v rows %v", a1, f.rows(t))
	}
	f.srv.officerRT.hostSpawns = failingSpawns{f.srv.officerRT.hostSpawns}
	if rec := f.card(t, fresh(f.asOwner), id2, "approve", nil); !strings.Contains(rec.Body.String(), html.EscapeString(FleetUnreadable)) || len(f.rows(t)) != 1 {
		t.Fatalf("a tap with the spawn store down: %s rows %v", rec.Body.String(), f.rows(t))
	}
	if a := f.action(t, id2); a.State != fleet.StateProposed {
		t.Fatalf("the card after the unreadable tap: %+v; want still proposed", a)
	}
	if got := f.srv.sendQueueFleetGate(f.asOwner, sendqueue.Item{ID: a1.SendQueueID, FleetActionID: id1}); got != SendQueueNudgeWait {
		t.Fatalf("the paste with the spawn store down: %q; want %q", got, SendQueueNudgeWait)
	}
}

package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/cichecks"
	"github.com/ZacxDev/muster/internal/fleet"
	"github.com/ZacxDev/muster/internal/githubapp"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/sessionstate"
)

// FLEET PROPOSALS, PART 2b (fleet_answers.go) on a real Postgres:
// session_answer (continue, merge_when_green, composed, pick), muster's
// ci_result, route / route_many / end_session outside an owner turn, and the
// dispatch reassign. Every value is invented and pairwise distinct.

// askingMirror is the proposal fixture's mirror whose hosts answer held
// questions (AskAnswer), recording each typing it is asked for.
type askingMirror struct {
	observedMirror
	mu     sync.Mutex
	typed  [][]askq.Choice
	failIt bool
}

func (m *askingMirror) Hosts(ctx context.Context) []mirror.HostView {
	hs := m.observedMirror.Hosts(ctx)
	for i := range hs {
		hs[i].AskAnswer = true
	}
	return hs
}

func (m *askingMirror) AuthenticateHost(http.ResponseWriter, *http.Request) (int64, string, bool) {
	return 0, "", false
}

func (m *askingMirror) AnswerWindow(_ context.Context, _, _, _ string, _ []askq.Question, choices []askq.Choice, _ *askq.Progress) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failIt {
		return errors.New("the picker moved")
	}
	m.typed = append(m.typed, choices)
	return nil
}

func (m *askingMirror) Nudge(int64) {}

func (m *askingMirror) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.typed)
}

// fakeCI is GitHub's CI read: the checks by "repo@sha".
type fakeCI struct {
	mu sync.Mutex
	by map[string]cichecks.Checks
	n  int
}

func (f *fakeCI) CommitChecks(_ context.Context, repo, sha string) (cichecks.Checks, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	c, ok := f.by[repo+"@"+sha]
	if !ok {
		return cichecks.Checks{}, errors.New("not found")
	}
	return c, nil
}

func (f *fakeCI) set(repo, sha string, c cichecks.Checks) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.by[repo+"@"+sha] = c
}

// fleetAnswerFixture is the proposal fixture with a host that answers pickers and
// a CI read, the CI cache forgotten between reads.
type fleetAnswerFixture struct {
	*proposeFixture
	asking *askingMirror
	ci     *fakeCI
}

func newFleetAnswerFixture(t *testing.T) *fleetAnswerFixture {
	t.Helper()
	f := &fleetAnswerFixture{proposeFixture: newProposeFixture(t), ci: &fakeCI{by: map[string]cichecks.Checks{}}}
	f.asking = &askingMirror{observedMirror: observedMirror{f.mirror}}
	f.srv.ext.Mirror, f.srv.ext.FleetChecks = f.asking, f.ci
	return f
}

// forgetCI drops the fleet's CI cache (a later read is fresh).
func (f *fleetAnswerFixture) forgetCI() {
	f.srv.fleetCI.mu.Lock()
	f.srv.fleetCI.m = nil
	f.srv.fleetCI.mu.Unlock()
}

const (
	ansHead = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"
	newHead = "f0e1d2c3b4a5968778695a4b3c2d1e0f12345678"
)

func green(at time.Time) cichecks.Checks {
	return cichecks.Checks{Statuses: map[string]cichecks.Status{"lint": {State: "success", UpdatedAt: at}},
		Runs: []cichecks.Run{{ID: 5, Name: "unit", Status: "completed", Conclusion: "success", CompletedAt: at}}}
}

// TestAPickMustBeOneOfTheHeldQuestionsOptions: a session holding a picker;
// a pick of an option not on it (another word, another case) is refused
// not_an_option and files nothing; one of its own options files a card
// naming option and question; the tap types exactly that option into the
// picker through the one typing an owner's card makes, and the action is
// sent. A second card on a picker that was answered meanwhile moves on.
func TestAPickMustBeOneOfTheHeldQuestionsOptions(t *testing.T) {
	f := newFleetAnswerFixture(t)
	key, window := f.window(f.owner, mirror.StatusWaiting)
	f.stop(t, f.owner, key, window, sessionstate.StateNeedsYou, true, fleetT0)
	qs := []askq.Question{{Question: "Which store backs the cache?", Options: []askq.Option{{Label: "Postgres"}, {Label: "SQLite"}}}}
	entry, err := f.srv.asks.Open(askq.Key{Owner: f.owner, Source: askSourceMirror, Scope: "bench-" + strconv.FormatInt(f.owner, 10), Session: key,
		ToolUseID: "toolu_01PickFixture1234"}, qs, "")
	if err != nil {
		t.Fatal(err)
	}
	subject := "mirror:" + key
	for _, bad := range []string{"MySQL", "sqlite", "SQLite "} {
		m := f.tool(t, f.asOwner, sessionAnswerTool, `{"subject":`+strconv.Quote(subject)+`,"answer_kind":"pick","option":`+strconv.Quote(bad)+`,"reason":"x"}`)
		if m["refused"] != fleetRefusedNotAnOption {
			t.Fatalf("pick %q: %v; want not_an_option", bad, m)
		}
	}
	m := f.tool(t, f.asOwner, sessionAnswerTool, `{"subject":`+strconv.Quote(subject)+`,"answer_kind":"pick","option":"SQLite","reason":"it is the dev default"}`)
	if m["state"] != fleet.StateProposed {
		t.Fatalf("pick SQLite: %v", m)
	}
	id := int64(m["action_id"].(float64))
	a := f.action(t, id)
	if a.Class != fleet.ClassAnswerPick || a.Text != "Pick “SQLite” in: Which store backs the cache?" || a.Evidence.AskID != entry.ID {
		t.Fatalf("the pick card: %+v", a)
	}
	rec := f.card(t, fresh(f.asOwner), id, "approve", nil)
	if !strings.Contains(rec.Body.String(), FleetSent) {
		t.Fatalf("approve: %s", rec.Body.String())
	}
	if f.asking.count() != 1 || len(f.asking.typed[0]) != 1 || len(f.asking.typed[0][0].Picks) != 1 || f.asking.typed[0][0].Picks[0] != 1 {
		t.Fatalf("typed %+v; want option 2 (SQLite) of the one question", f.asking.typed)
	}
	if a = f.action(t, id); a.State != fleet.StateSent || len(f.rows(t)) != 0 {
		t.Fatalf("after the pick: %+v rows %v; want sent, nothing pasted", a, f.rows(t))
	}

	// A picker answered in the terminal before the tap: moved on, nothing typed.
	key2, window2 := f.window(f.owner, mirror.StatusWaiting)
	f.stop(t, f.owner, key2, window2, sessionstate.StateNeedsYou, true, fleetT0)
	if _, err := f.srv.asks.Open(askq.Key{Owner: f.owner, Source: askSourceMirror, Scope: "bench-" + strconv.FormatInt(f.owner, 10), Session: key2,
		ToolUseID: "toolu_01PickFixture5678"}, qs, ""); err != nil {
		t.Fatal(err)
	}
	m = f.tool(t, f.asOwner, sessionAnswerTool, `{"subject":"mirror:`+key2+`","answer_kind":"pick","option":"Postgres","reason":"y"}`)
	id2 := int64(m["action_id"].(float64))
	f.srv.asks.Resolve(f.owner, askSourceMirror, "bench-"+strconv.FormatInt(f.owner, 10), key2, "toolu_01PickFixture5678", map[string]string{"Which store backs the cache?": "SQLite"})
	if rec := f.card(t, fresh(f.asOwner), id2, "approve", nil); !strings.Contains(rec.Body.String(), FleetMovedOn) || f.asking.count() != 1 {
		t.Fatalf("an answered picker's tap: %s; typed %d", rec.Body.String(), f.asking.count())
	}
}

// TestEightTapsOnOnePickTypeItOnce: concurrent approves of one pick card on a
// 4-connection pool type the option ONCE.
func TestEightTapsOnOnePickTypeItOnce(t *testing.T) {
	withPoolMaxConns(t, 4)
	f := newFleetAnswerFixture(t)
	key, window := f.window(f.owner, mirror.StatusWaiting)
	f.stop(t, f.owner, key, window, sessionstate.StateNeedsYou, true, fleetT0)
	if _, err := f.srv.asks.Open(askq.Key{Owner: f.owner, Source: askSourceMirror, Scope: "bench-" + strconv.FormatInt(f.owner, 10), Session: key,
		ToolUseID: "toolu_01PickConcurrent99"}, []askq.Question{{Question: "Ship it?", Options: []askq.Option{{Label: "Yes"}, {Label: "Not yet"}}}}, ""); err != nil {
		t.Fatal(err)
	}
	m := f.tool(t, f.asOwner, sessionAnswerTool, `{"subject":"mirror:`+key+`","answer_kind":"pick","option":"Not yet","reason":"z"}`)
	id := int64(m["action_id"].(float64))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); f.card(t, fresh(f.asOwner), id, "approve", nil) }()
	}
	wg.Wait()
	if n := f.asking.count(); n != 1 {
		t.Fatalf("eight taps typed %d times; want once", n)
	}
}

// TestTheAnswerTextsByteForByte: continue and composed queue muster's line
// and the text, byte for byte.
func TestTheAnswerTextsByteForByte(t *testing.T) {
	f := newFleetAnswerFixture(t)
	s1, _, _ := f.stopped(t, f.owner)
	s2, _, _ := f.stopped(t, f.owner)
	m1 := f.tool(t, f.asOwner, sessionAnswerTool, `{"subject":`+strconv.Quote(s1)+`,"answer_kind":"continue","reason":"it asked to go on"}`)
	m2 := f.tool(t, f.asOwner, sessionAnswerTool, `{"subject":`+strconv.Quote(s2)+`,"answer_kind":"composed","text":"Use the staging bucket, not prod.","reason":"it asked which"}`)
	f.card(t, fresh(f.asOwner), int64(m1["action_id"].(float64)), "approve", nil)
	f.card(t, fresh(f.asOwner), int64(m2["action_id"].(float64)), "approve", nil)
	got := f.rows(t)
	if len(got) != 2 || got[0][0] != f.header()+"\nYes, continue." || got[1][0] != f.header()+"\nUse the staging bucket, not prod." {
		t.Fatalf("rows %q", got)
	}
	if m := f.tool(t, f.asOwner, sessionAnswerTool, `{"subject":`+strconv.Quote(s1)+`,"answer_kind":"pick","reason":"x"}`); m["refused"] != fleetRefusedBadArgs {
		t.Fatalf("pick with no option: %v", m)
	}
}

// TestMergeWhenGreenNeedsGreenCIOnTheHeadAndReReadsIt: the session's own open
// pull request, CI read green on its head: the card's text pins the commit
// (literal). CI pending: refused merge_check. Green at filing, red at the tap:
// moved on, nothing sent. An owner the App does not admit: refused (the CI
// read is App-gated), and GitHub is not asked.
func TestMergeWhenGreenNeedsGreenCIOnTheHeadAndReReadsIt(t *testing.T) {
	f := newFleetAnswerFixture(t)
	subject, key, _ := f.stopped(t, f.owner)
	f.pr(t, f.owner, key, "example-org/gizmo", 640, &githubapp.PRState{State: "open", HeadSHA: ansHead})
	args := `{"subject":` + strconv.Quote(subject) + `,"answer_kind":"merge_when_green","reason":"it asked to merge"}`
	f.ci.set("example-org/gizmo", ansHead, cichecks.Checks{Runs: []cichecks.Run{{ID: 3, Name: "unit", Status: "in_progress"}}})
	if m := f.tool(t, f.asOwner, sessionAnswerTool, args); m["refused"] != fleetRefusedMergeCheck {
		t.Fatalf("CI pending: %v", m)
	}
	f.forgetCI()
	f.ci.set("example-org/gizmo", ansHead, green(time.Now()))
	before := f.ci.n
	if m := f.tool(t, f.as(f.owner), sessionAnswerTool, args); m["refused"] != fleetRefusedMergeCheck || f.ci.n != before {
		t.Fatalf("a member the App does not admit: %v (%d reads)", m, f.ci.n-before)
	}
	m := f.tool(t, f.asOwner, sessionAnswerTool, args)
	if m["state"] != fleet.StateProposed {
		t.Fatalf("CI green: %v", m)
	}
	id := int64(m["action_id"].(float64))
	if a := f.action(t, id); a.Text != "Yes, merge #640. CI is green on a1b2c3d4e5f6. Merge only that commit: gh pr merge 640 --match-head-commit "+ansHead+"." {
		t.Fatalf("the merge text: %q", a.Text)
	}
	f.forgetCI()
	f.ci.set("example-org/gizmo", ansHead, cichecks.Checks{Runs: []cichecks.Run{{ID: 9, Name: "unit", Status: "completed", Conclusion: "failure", CompletedAt: time.Now()}}})
	if rec := f.card(t, fresh(f.asOwner), id, "approve", nil); !strings.Contains(rec.Body.String(), FleetMovedOn) || len(f.rows(t)) != 0 {
		t.Fatalf("CI red at the tap: %s rows %v", rec.Body.String(), f.rows(t))
	}
}

// TestCIResultIsProposedWhenTheChecksRunningAtTheStopFinish: a stalled
// session with its pull request open; its head's checks finished BEFORE it
// stopped: nothing; still running: nothing; all finished, the last AFTER the
// stop: one ci_result with §4.2's text (literal), once per stop.
func TestCIResultIsProposedWhenTheChecksRunningAtTheStopFinish(t *testing.T) {
	f := newFleetAnswerFixture(t)
	_, key, _ := f.stopped(t, f.owner) // opened at fleetT0
	f.pr(t, f.owner, key, "example-org/sprocket", 512, &githubapp.PRState{State: "open", HeadSHA: newHead})
	count := func() int {
		list, _ := f.srv.ext.FleetActions.OpenActions(f.asOwner)
		n := 0
		for _, a := range list {
			if a.Class == fleet.ClassCIResult {
				n++
			}
		}
		return n
	}
	f.ci.set("example-org/sprocket", newHead, green(fleetT0.Add(-time.Minute)))
	f.pass()
	if n := count(); n != 0 {
		t.Fatalf("checks finished before the stop: %d proposals", n)
	}
	f.forgetCI()
	f.ci.set("example-org/sprocket", newHead, cichecks.Checks{Runs: []cichecks.Run{{ID: 4, Name: "unit", Status: "queued"}}})
	f.pass()
	if n := count(); n != 0 {
		t.Fatalf("checks still running: %d proposals", n)
	}
	f.forgetCI()
	f.ci.set("example-org/sprocket", newHead, cichecks.Checks{
		Statuses: map[string]cichecks.Status{"lint": {State: "success", UpdatedAt: fleetT0.Add(time.Minute)}},
		Runs: []cichecks.Run{{ID: 6, Name: "unit", Status: "completed", Conclusion: "failure", CompletedAt: fleetT0.Add(9 * time.Minute)},
			{ID: 7, Name: "e2e", Status: "completed", Conclusion: "timed_out", CompletedAt: fleetT0.Add(8 * time.Minute)}}})
	f.pass()
	list, _ := f.srv.ext.FleetActions.OpenActions(f.asOwner)
	if len(list) != 1 || list[0].Class != fleet.ClassCIResult || list[0].Proposer != fleet.ProposerSweep ||
		list[0].Text != "CI finished on #512 at f0e1d2c3b4a5: ok=1 bad=2 (e2e, unit). Continue." {
		t.Fatalf("the ci_result: %+v", list)
	}
	f.srv.fleetReject(f.asOwner, list[0].ID, fleet.ViaWeb)
	f.pass()
	if n := count(); n != 0 {
		t.Fatalf("proposed again on the same stop: %d", n)
	}
}

// TestRoutesAndEndsOutsideAnOwnersTurnFileCards: route_to_session {text},
// route_many and end_session_propose called with no owner turn file cards
// (route, route ×2, close) and route nothing; with fleet proposals off each
// refuses as before (the positive control for the switch).
func TestRoutesAndEndsOutsideAnOwnersTurnFileCards(t *testing.T) {
	f := newFleetAnswerFixture(t)
	f.srv.ext.Routes, f.srv.ext.Routing = f.plugins, &Routing{}
	f.srv.ext.ChiefDispatch = f.plugins // end_session_propose's proposal store
	s1, k1, _ := f.stopped(t, f.owner)
	s2, _, _ := f.stopped(t, f.owner)
	s3, _, _ := f.stopped(t, f.owner)
	out := f.srv.toolRouteToSession(f.asOwner, f.chief, `{"subject":`+strconv.Quote(s1)+`,"text":"Rebase onto the release branch.","owner_asked":true}`)
	if !strings.Contains(out, `"state":"proposed"`) {
		t.Fatalf("route_to_session outside a turn: %s", out)
	}
	out = f.srv.toolRouteMany(f.asOwner, f.chief, `{"subjects":[`+strconv.Quote(s2)+`,`+strconv.Quote(s3)+`],"text":"Post your status.","owner_asked":true}`)
	if !strings.Contains(out, `"proposed":[{`) || strings.Count(out, `"action_id"`) != 2 {
		t.Fatalf("route_many outside a turn: %s", out)
	}
	out = f.srv.toolEndSessionPropose(f.asOwner, f.chief, `{"session":`+strconv.Quote(k1)+`}`)
	if !strings.Contains(out, `"state":"proposed"`) {
		t.Fatalf("end_session_propose outside a turn: %s", out)
	}
	list, _ := f.srv.ext.FleetActions.OpenActions(f.asOwner)
	classes := map[string]int{}
	for _, a := range list {
		classes[a.Class]++
	}
	if classes[fleet.ClassRoute] != 3 || classes[fleet.ClassClose] != 1 || len(list) != 4 {
		t.Fatalf("cards %v", classes)
	}
	var routes int
	_ = f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM session_routes WHERE owner_id = $1`, f.owner).Scan(&routes)
	if routes != 0 || len(f.rows(t)) != 0 {
		t.Fatalf("something was routed (%d routes, rows %v)", routes, f.rows(t))
	}
	f.srv.ext.FleetPropose = false
	if out := f.srv.toolRouteToSession(f.asOwner, f.chief, `{"subject":`+strconv.Quote(s2)+`,"text":"x","owner_asked":true}`); strings.Contains(out, "proposed") {
		t.Fatalf("flag off: %s", out)
	}
}

// TestADispatchReassignStartsTheNewSessionOnThePassThenHandsOff: work_reassign
// with dispatch in place of to: the tap starts nothing itself; the next pass
// starts the new session exactly as agent_dispatch would be asked (the task,
// the target, a request key of the action's), then queues the handoff to the
// old session.
func TestADispatchReassignStartsTheNewSessionOnThePassThenHandsOff(t *testing.T) {
	f := newFleetAnswerFixture(t)
	f.srv.ext.TaskRouting = &TaskRouting{DispatchOwners: map[int64]bool{f.owner: true}}
	f.srv.ext.ChiefDispatch = stubChiefDispatch{}
	var asked []agentDispatchIn
	f.srv.fleetDispatch = func(_ context.Context, a agents.Agent, in agentDispatchIn) string {
		asked = append(asked, in)
		return toolJSON(map[string]any{"started": true, "agent": "fresh-otter"})
	}
	from, _, _ := f.stopped(t, f.owner)
	n, err := f.notes.Create(f.asOwner, notes.Note{Title: "port the importer", Body: "to the new queue", Status: notes.StatusOpen})
	if err != nil {
		t.Fatal(err)
	}
	m := f.tool(t, f.asOwner, workReassignTool, `{"task_id":`+strconv.FormatInt(n.ID, 10)+`,"from":`+strconv.Quote(from)+`,"dispatch":{"target":"pod"},"reason":"a fresh session"}`)
	if m["state"] != fleet.StateProposed {
		t.Fatalf("work_reassign dispatch: %v", m)
	}
	id := int64(m["action_id"].(float64))
	f.card(t, fresh(f.asOwner), id, "approve", nil)
	if len(asked) != 0 || len(f.rows(t)) != 0 {
		t.Fatalf("the tap started something: %v rows %v", asked, f.rows(t))
	}
	f.pass()
	if len(asked) != 1 || asked[0].TaskID != n.ID || asked[0].Target != "pod" || asked[0].RequestKey != "fleet-"+strconv.FormatInt(id, 10) {
		t.Fatalf("dispatch asked %+v", asked)
	}
	got := f.rows(t)
	if len(got) != 1 || !strings.HasSuffix(got[0][0], "Task #"+strconv.FormatInt(n.ID, 10)+" has moved to another session (a new session on pod). Stop work on it. Write your handoff (what is done, what is left, where), then stop.") {
		t.Fatalf("the handoff to from: %q", got)
	}
	if a := f.action(t, id); a.State != fleet.StateQueued {
		t.Fatalf("after the pass: %+v", a)
	}
}

// TestTheCIReadIsGatedByTheApp: the CI read (and the head read it pins) asks
// GitHub only for an owner the App admits (githubAppForOwner): a member is
// unknown and GitHub is not asked; the admin's read is made. (The merge
// check's own refusal for a member stops earlier, at the pull request's
// state, so this reaches the CI read itself.)
func TestTheCIReadIsGatedByTheApp(t *testing.T) {
	f := newFleetAnswerFixture(t)
	f.ci.set("example-org/flange", ansHead, green(time.Now()))
	f.prs.mu.Lock()
	f.prs.states["example-org/flange#77"] = githubapp.PRState{State: "open", HeadSHA: ansHead}
	f.prs.mu.Unlock()
	if _, ok := f.srv.fleetCIRead(f.as(f.owner), "example-org/flange", ansHead); ok || f.ci.n != 0 {
		t.Fatalf("a member's CI read: ok %v, %d GitHub reads; want unknown and none", ok, f.ci.n)
	}
	if _, ok := f.srv.fleetPRHead(f.as(f.owner), "example-org/flange", 77); ok {
		t.Fatalf("a member's head read: known")
	}
	if sum, ok := f.srv.fleetCIRead(f.asOwner, "example-org/flange", ansHead); !ok || !sum.AllGreen() || f.ci.n != 1 {
		t.Fatalf("control: the admin's read: ok %v green %v reads %d", ok, sum.AllGreen(), f.ci.n)
	}
	if head, ok := f.srv.fleetPRHead(f.asOwner, "example-org/flange", 77); !ok || head != ansHead {
		t.Fatalf("control: the admin's head read: %q %v", head, ok)
	}
}

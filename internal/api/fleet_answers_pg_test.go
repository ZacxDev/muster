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

	"github.com/ZacxDev/muster/internal/asklog"
	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/cichecks"
	"github.com/ZacxDev/muster/internal/fleet"
	"github.com/ZacxDev/muster/internal/githubapp"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sessionstate"
)

// FLEET PROPOSALS, PART 2b (fleet_answers.go) on a real Postgres:
// session_answer (continue, composed — each to a held plain-text question —
// and pick), muster's ci_result, and route / route_many / end_session outside
// an owner turn. Every value is invented and pairwise distinct.

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
	f.srv.ext.AskLog = &AskLog{Store: asklog.NewPG(f.pool)}
	return f
}

// held opens a plain-text question in the ask log, as the capture writes one,
// for owner's window (key, window) in its open stop (episode 0: none named),
// returning its id.
func (f *fleetAnswerFixture) held(t *testing.T, owner int64, key, window string, episode int64, question string) int64 {
	t.Helper()
	id, ok, err := asklog.NewPG(f.pool).Open(f.as(owner), asklog.Ask{Kind: asklog.KindPlainText, Runtime: asklog.RuntimeClaudeCode,
		Host: "bench-" + strconv.FormatInt(owner, 10), TmuxSession: "work", Window: window, ClaudeSession: key, EpisodeID: episode,
		Question: question, AskedAt: fleetT0.Add(time.Minute)})
	if err != nil || !ok {
		t.Fatalf("open ask: %v %v", ok, err)
	}
	return id
}

// openEpisode is key's open stop.
func (f *fleetAnswerFixture) openEpisode(t *testing.T, owner int64, key string) int64 {
	t.Helper()
	var id int64
	if err := f.pool.QueryRow(f.as(owner), `SELECT id FROM session_episodes WHERE session_key = $1 AND ended_at IS NULL`, key).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
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

// TestTheAnswerTextsByteForByte: continue and composed, each to the session's
// held question, queue muster's line and the text, byte for byte.
func TestTheAnswerTextsByteForByte(t *testing.T) {
	f := newFleetAnswerFixture(t)
	s1, k1, w1 := f.stopped(t, f.owner)
	s2, k2, w2 := f.stopped(t, f.owner)
	f.held(t, f.owner, k1, w1, f.openEpisode(t, f.owner, k1), "Shall I go on with the migration?")
	f.held(t, f.owner, k2, w2, 0, "Which bucket do the fixtures go in?")
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

// TestAnAnswerNeedsAnOpenHeldQuestion: continue and composed answer an open
// plain-text question of THAT session's stop, the owner's. Refused
// no_question, nothing filed: the ask log off; no question; a question of
// another session; one already answered; one of an earlier stop; two open and
// no ask_id. Filed (positive controls): the one open question, or ask_id
// naming one of two, the card carrying its ask-log id. A question answered
// in the terminal after the card was filed: the tap moves on, nothing pasted.
func TestAnAnswerNeedsAnOpenHeldQuestion(t *testing.T) {
	f := newFleetAnswerFixture(t)
	answer := func(subject, extra string) map[string]any {
		return f.tool(t, f.asOwner, sessionAnswerTool, `{"subject":`+strconv.Quote(subject)+`,"answer_kind":"continue",`+extra+`"reason":"it asked"}`)
	}
	refused := func(what string, m map[string]any) {
		t.Helper()
		if m["refused"] != fleetRefusedNoQuestion {
			t.Fatalf("%s: %v; want no_question", what, m)
		}
	}
	s1, k1, w1 := f.stopped(t, f.owner)
	_, k2, w2 := f.stopped(t, f.owner)

	f.srv.ext.AskLog = nil
	refused("the ask log off", answer(s1, ""))
	f.srv.ext.AskLog = &AskLog{Store: asklog.NewPG(f.pool)}
	refused("no question", answer(s1, ""))
	other := f.held(t, f.owner, k2, w2, 0, "Rename the flag to strict?")
	refused("another session's question", answer(s1, ""))
	refused("another session's question by id", answer(s1, `"ask_id":`+strconv.FormatInt(other, 10)+`,`))
	done := f.held(t, f.owner, k1, w1, 0, "Drop the old index too?")
	if _, _, err := asklog.NewPG(f.pool).Resolve(f.asOwner, done, asklog.Resolution{Outcome: asklog.OutcomeAnswered, Answer: "yes", AnsweredBy: asklog.ByOwner}); err != nil {
		t.Fatal(err)
	}
	refused("an answered question", answer(s1, ""))
	ep := f.openEpisode(t, f.owner, k1)
	f.held(t, f.owner, k1, w1, f.openEpisode(t, f.owner, k2), "Keep the retry loop?") // a stop not k1's open one
	refused("a question of another stop", answer(s1, ""))
	if list, _ := f.srv.ext.FleetActions.OpenActions(f.asOwner); len(list) != 0 {
		t.Fatalf("a refusal filed %+v", list)
	}

	q1 := f.held(t, f.owner, k1, w1, ep, "Push the branch now?")
	m := answer(s1, "")
	if m["state"] != fleet.StateProposed {
		t.Fatalf("the one open question: %v", m)
	}
	id := int64(m["action_id"].(float64))
	if a := f.action(t, id); a.Evidence.AskLogID != q1 {
		t.Fatalf("the card's question: %d; want %d", a.Evidence.AskLogID, q1)
	}
	f.srv.fleetReject(f.asOwner, id, fleet.ViaWeb)
	q2 := f.held(t, f.owner, k1, w1, 0, "Tag the release as well?")
	refused("two open, no ask_id", answer(s1, ""))
	m = answer(s1, `"ask_id":`+strconv.FormatInt(q2, 10)+`,`)
	if m["state"] != fleet.StateProposed {
		t.Fatalf("ask_id naming one of two: %v", m)
	}
	id = int64(m["action_id"].(float64))
	if a := f.action(t, id); a.Evidence.AskLogID != q2 {
		t.Fatalf("the card's question: %d; want %d", a.Evidence.AskLogID, q2)
	}
	// Answered in the terminal before the tap: moved on, nothing pasted.
	if _, _, err := asklog.NewPG(f.pool).Resolve(f.asOwner, q2, asklog.Resolution{Outcome: asklog.OutcomeAnswered, Answer: "no", AnsweredBy: asklog.ByOwner}); err != nil {
		t.Fatal(err)
	}
	if rec := f.card(t, fresh(f.asOwner), id, "approve", nil); !strings.Contains(rec.Body.String(), FleetMovedOn) || len(f.rows(t)) != 0 {
		t.Fatalf("a question answered before the tap: %s rows %v", rec.Body.String(), f.rows(t))
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
// (custom, custom ×2, close) and route nothing; with fleet proposals off each
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
	if classes[fleet.ClassCustom] != 3 || classes[fleet.ClassClose] != 1 || len(list) != 4 {
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

// TestTheCIReadIsGatedByTheApp: the CI read (and the head read it pins) asks
// GitHub only for an owner the App admits (githubAppForOwner): a member is
// unknown and GitHub is not asked; the admin's read is made.
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

// TestATeamsChiefIsNotShownTheFleetTools: a team's chief is not even SHOWN
// the fleet tools (the call refuses them too, dispatchFleetTool): listed as
// a team's acting principal, none of the five; the same principal as a
// person (the positive control — only Team differs) lists all five.
func TestATeamsChiefIsNotShownTheFleetTools(t *testing.T) {
	f := newFleetAnswerFixture(t)
	names := map[string]bool{sessionInstructTool: true, sessionWrapTool: true, workReassignTool: true, fleetActionsListTool: true, sessionAnswerTool: true}
	listed := func(team bool) []string {
		ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: f.owner, Role: authz.RoleMember, Team: team, Via: authz.ViaAgent})
		var got []string
		for _, d := range f.srv.MCPToolDefsFor(ctx, f.chief) {
			if names[d.Name] {
				got = append(got, d.Name)
			}
		}
		return got
	}
	if got := listed(true); len(got) != 0 {
		t.Fatalf("a team's chief is shown %v", got)
	}
	if got := listed(false); len(got) != 5 {
		t.Fatalf("control: the same principal as a person is shown %v; want all five", got)
	}
}

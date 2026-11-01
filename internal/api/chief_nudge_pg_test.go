package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// THE CHIEF'S STALL NUDGES end to end on a real Postgres (chief_nudge.go):
// candidate selection at the server, the quote guard, the caps, shadow mode
// sending nothing, the live ask → answer → verbatim relay, the live nudge
// through the send queue, and coverage (officers, the chief and other users'
// agents left out).

const (
	nudgeSID   = "0b5d7a52-6f1e-4c39-9a3e-2d4c8b1f0e77"
	nudgeSince = int64(1_760_000_500_000)
	// nudgeOwnerText is the owner's message every default transcript carries;
	// nudgeOwnerID its id there.
	nudgeOwnerText = "keep going until merged, then report back"
	nudgeOwnerID   = "line:4096"
)

// nudgeGateway plays the chiefs' gateway: a stall check (Chat) answers reply;
// an agent turn (ChatWithTools) is recorded and answered.
type nudgeGateway struct {
	mu     sync.Mutex
	reply  string
	fails  int      // the next fails stall checks answer an error
	checks []string // "<agent> <key>"
	prompt []string
	turns  []string // "<agent> <text>"
	// during, when set, runs inside each stall check before it answers: what
	// changes while the chief is thinking (chief_nudge_unknown_pg_test.go).
	during func()
	// answers, when set, answer the next stall checks in order, ahead of reply
	// (chief_nudge_reask_pg_test.go).
	answers []nudgeAnswer
}

// nudgeAnswer is one scripted stall-check answer: reply, or err when set.
type nudgeAnswer struct {
	reply string
	err   error
}

func (g *nudgeGateway) Chat(_ context.Context, a agents.Agent, key, msg string, _ func(string)) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.checks = append(g.checks, a.Name+" "+key)
	g.prompt = append(g.prompt, msg)
	if g.during != nil {
		g.during()
	}
	if g.fails > 0 {
		g.fails--
		return "", errors.New("nudgeGateway: the provider is down")
	}
	if len(g.answers) > 0 {
		a := g.answers[0]
		g.answers = g.answers[1:]
		return a.reply, a.err
	}
	return g.reply, nil
}

func (g *nudgeGateway) ChatWithTools(_ context.Context, a agents.Agent, _, _, text string, _ []agents.ToolDef, _ agents.ToolDispatch, _ agents.StreamEmit) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.turns = append(g.turns, a.Name+" "+text)
	return "on it", nil
}

func (g *nudgeGateway) set(reply string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reply = reply
}

func (g *nudgeGateway) seen() (checks, turns []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.checks...), append([]string(nil), g.turns...)
}

type nudgeFixture struct {
	*attnFixture
	gw      *nudgeGateway
	nudges  *chiefnudge.PG
	queue   *sendqueue.PGStore
	cfg     *ChiefNudges
	boss    agents.Agent // user 1's chief
	mu      sync.Mutex
	script  []chiefnudge.Msg
	partial bool // the scripted read reports itself incomplete
}

// nudgeReply is the chief's JSON for a nudge quoting quote from message id.
func nudgeReply(id, quote, covers string) string {
	return `{"decision":"nudge","reason":"CI is green and the owner said to merge","quote_id":"` + id +
		`","quote":"` + quote + `","covered_action":"` + covers + `"}`
}

const nudgeAskReply = `{"decision":"ask","reason":"it asked which branch","question":"Merge into main or into release?","options":["main","release"]}`

func newNudgeFixture(t *testing.T, live bool) *nudgeFixture {
	t.Helper()
	f := &nudgeFixture{gw: &nudgeGateway{}, cfg: &ChiefNudges{Idle: 10 * time.Minute, HourlyCap: 6, Live: map[int64]bool{}}}
	if live {
		f.cfg.Live[authz.OperatorUserID] = true
	}
	store := &nudgeAgents{}
	f.attnFixture = newAttnFixture(t, func(ext *Extensions, pool *pgxpool.Pool) {
		f.nudges, f.queue = chiefnudge.NewPG(pool), sendqueue.NewPG(pool)
		ext.ChiefNudges, ext.ChiefNudge, ext.SendQueue, ext.Gateway = f.nudges, f.cfg, f.queue, f.gw
		store.Store = ext.Agents
		ext.Agents = store
	})
	f.gw.set(nudgeReply(nudgeOwnerID, "keep going until merged", "merge the pull request"))
	f.mirror.windows = []mirror.Window{{Session: "main", ID: "@1", Index: 1, Active: true, Label: "Gold:1", Claude: true,
		ClaudeSession: nudgeSID, Status: mirror.StatusIdle, StatusSince: nudgeSince}}
	f.idle(20 * time.Minute)
	store.hideChiefsOf(t, authz.OperatorUserID)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	f.boss = f.agentFor(t, authz.OperatorUserID, "nudgechief-"+suffix, "chief", "nudge-chief-tok-"+suffix)
	f.script = []chiefnudge.Msg{
		{Role: "user", Text: nudgeOwnerText, ID: nudgeOwnerID, At: time.Now().Add(-10 * time.Minute)},
		{Role: "assistant", Text: "CI is green on the PR. Shall I merge it?"},
	}
	f.srv.nudgeReadHook = func(context.Context, chiefnudge.Session) (nudgeRead, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return nudgeRead{Msgs: append([]chiefnudge.Msg(nil), f.script...), Partial: f.partial}, nil
	}
	t.Cleanup(func() {
		sys := authz.System(context.Background())
		for _, q := range []string{`DELETE FROM send_queue WHERE owner_id = $1`, `DELETE FROM chief_nudges WHERE owner_id IN ($1, $2)`,
			`DELETE FROM attention_asks WHERE owner_id = $1 AND (system_asker OR mirror_session IS NOT NULL)`} {
			args := []any{authz.OperatorUserID}
			if strings.Contains(q, "$2") {
				args = append(args, f.member)
			}
			if _, err := f.pool.Exec(sys, q, args...); err != nil {
				t.Errorf("cleanup %q: %v", q, err)
			}
		}
	})
	return f
}

// nudgeAgents is the nudge fixture's agent store: the real one, with every
// agent the owner already had labelled chief left out of List.
//
// 🔴 THE FIXTURE'S BOSS MUST BE THE OWNER'S ONLY CHIEF, and the package's
// shared database need not agree: a run cut short leaves its rows behind
// (agent_destroy_pg_test.go reuses the reserved slug `chief` rather than
// removing it), and chiefAgent resolves the LOWEST-id agent labelled chief, so
// any such row outranks the boss made here — the boss turns into a nudge
// candidate and every expectation breaks. Those rows are another test's (or a
// live reproduction), and the tests relabel and delete the boss, so the
// fixture neither adopts nor alters them: it stops the server SEEING them.
// Only List is filtered, which is the one read the chief is resolved from
// (chiefAgent, chiefPredicate, chiefOn).
type nudgeAgents struct {
	agents.Store
	mu     sync.Mutex
	hidden map[int64]bool
}

func (s *nudgeAgents) List(ctx context.Context) ([]agents.Agent, error) {
	list, err := s.Store.List(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	out := list[:0:0]
	for _, a := range list {
		if !s.hidden[a.ID] {
			out = append(out, a)
		}
	}
	return out, err
}

// hideChiefsOf hides every agent owner has now that chiefAgent's own rule
// (resolveChief) would pick, one after another until it picks none. Called
// before the boss is made, so the boss is never among them.
func (s *nudgeAgents) hideChiefsOf(t *testing.T, owner int64) {
	t.Helper()
	list, err := s.Store.List(asUser(owner))
	if err != nil {
		t.Fatal(err)
	}
	list = ownedBy(list, owner)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hidden = map[int64]bool{}
	for {
		c, ok := resolveChief(list)
		if !ok {
			return
		}
		s.hidden[c.ID] = true
		kept := list[:0]
		for _, a := range list {
			if a.ID != c.ID {
				kept = append(kept, a)
			}
		}
		list = kept
	}
}

// idle puts the window idle for d on muster's clock, in streak nudgeSince.
func (f *nudgeFixture) idle(d time.Duration) {
	f.mirror.mu.Lock()
	defer f.mirror.mu.Unlock()
	f.mirror.agentState = &mirror.AgentState{Runtime: mirror.RuntimeClaude, Status: mirror.StatusIdle, Since: nudgeSince,
		Observed: time.Now().Add(-d), Session: nudgeSID}
}

func (f *nudgeFixture) state(mut func(*mirror.AgentState)) {
	f.mirror.mu.Lock()
	defer f.mirror.mu.Unlock()
	mut(f.mirror.agentState)
}

func (f *nudgeFixture) pass(t *testing.T) int {
	t.Helper()
	n, err := f.srv.ChiefNudgePass(context.Background())
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	return n
}

func (f *nudgeFixture) decisions(t *testing.T) []chiefnudge.Decision {
	t.Helper()
	list, err := f.nudges.List(asUser(authz.OperatorUserID), 100)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func (f *nudgeFixture) one(t *testing.T) chiefnudge.Decision {
	t.Helper()
	list := f.decisions(t)
	if len(list) != 1 {
		t.Fatalf("decisions: %d, want 1: %+v", len(list), list)
	}
	return list[0]
}

// count is a system-scope count of rows of the operator's in table.
func (f *nudgeFixture) count(t *testing.T, q string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(authz.System(context.Background()), q, authz.OperatorUserID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *nudgeFixture) queuedRows(t *testing.T) int {
	return f.count(t, `SELECT count(*) FROM send_queue WHERE owner_id = $1`)
}

func (f *nudgeFixture) askRows(t *testing.T) int {
	return f.count(t, `SELECT count(*) FROM attention_asks WHERE owner_id = $1`)
}

func (f *nudgeFixture) pastes() []pasteCall {
	f.mirror.v2.mu.Lock()
	defer f.mirror.v2.mu.Unlock()
	return append([]pasteCall(nil), f.mirror.v2.pastes...)
}

func (f *nudgeFixture) audited(t *testing.T, id int64) []string {
	t.Helper()
	rows, err := f.pool.Query(authz.System(context.Background()), `SELECT action FROM audit_log WHERE object = $1 ORDER BY id`, chiefnudge.Object(id))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		out = append(out, a)
	}
	return out
}

func (f *nudgeFixture) ownerName(t *testing.T) string {
	t.Helper()
	var n string
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT display_name FROM users WHERE id = $1`, authz.OperatorUserID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(n) == "" {
		return "Your owner"
	}
	return n
}

// tick runs one send queue pass with the idle wait already met.
func (f *nudgeFixture) tick(t *testing.T) {
	t.Helper()
	f.srv.sendQueueClock = func() time.Time { return time.Now().Add(time.Hour) }
	if _, err := f.srv.newSendQueueDispatcher().Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestNudgeFixtureBossOutranksAnOlderChiefInTheDatabase is the fixture's own
// guard: an agent the operator already has labelled chief — what a run cut
// short leaves in the package's shared database — has a LOWER id than the
// fixture's boss, so chiefAgent would resolve it. The fixture's boss is still
// the chief that is asked, it is not itself a candidate (one decision, for the
// window), and the older row is left exactly as it was.
func TestNudgeFixtureBossOutranksAnOlderChiefInTheDatabase(t *testing.T) {
	dsn := dbtest.DSN(t)
	pool, err := db.Connect(dbtest.AsOperator(context.Background()), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	name := "nudgeleftover-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	old, err := agents.NewPG(pool).Create(asUser(authz.OperatorUserID), agents.Agent{Name: name, Namespace: "devpod-" + name,
		DisplayName: "chief", Status: agents.StatusRunning, HooksToken: "nudge-leftover-tok-" + name})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(authz.System(context.Background()), `DELETE FROM agents WHERE id = $1`, old.ID)
	})

	f := newNudgeFixture(t, false)
	if old.ID >= f.boss.ID {
		t.Fatalf("the planted chief %d does not outrank the boss %d: the case is not built", old.ID, f.boss.ID)
	}
	if n := f.pass(t); n != 1 {
		t.Fatalf("decided %d, want 1 (the window): %+v", n, f.decisions(t))
	}
	if d := f.one(t); d.Kind != chiefnudge.KindMirror || d.Key != nudgeSID {
		t.Fatalf("decided %+v, want the window %s", d, nudgeSID)
	}
	checks, _ := f.gw.seen()
	if len(checks) != 1 || !strings.HasPrefix(checks[0], f.boss.Name+" ") {
		t.Fatalf("chief calls %v, want one by the boss %s", checks, f.boss.Name)
	}
	got, err := agents.NewPG(pool).Get(asUser(authz.OperatorUserID), old.ID)
	if err != nil || got.DisplayName != "chief" || got.Name != name {
		t.Fatalf("the older chief was altered: %+v, %v", got, err)
	}
}

// TestChiefNudgeShadowRecordsTheDecisionAndSendsNothing is the shadow guard:
// the whole pipeline runs — the chief is asked, the quote checked, the nudge
// composed and recorded with its audit row — and NOTHING reaches the session
// or the owner's queue: no send_queue row, no attention ask, no paste. A
// second pass over the same idle streak asks the chief nothing.
func TestChiefNudgeShadowRecordsTheDecisionAndSendsNothing(t *testing.T) {
	f := newNudgeFixture(t, false)
	queued0, asks0 := f.queuedRows(t), f.askRows(t)
	if n := f.pass(t); n != 1 {
		t.Fatalf("decided %d, want 1", n)
	}
	d := f.one(t)
	want := `[Chief nudge] ` + f.ownerName(t) + ` said at ` + f.script[0].At.In(time.Local).Format("15:04") + `: "keep going until merged". Continue.`
	if d.Decision != chiefnudge.DecisionNudge || d.Outcome != chiefnudge.OutcomeWouldSend || d.Mode != chiefnudge.ModeShadow ||
		d.Message != want || d.QuoteID != nudgeOwnerID || d.QuoteText != "keep going until merged" ||
		d.Covered != "merge the pull request" || d.AnchorKey != nudgeOwnerID || d.Key != nudgeSID ||
		d.StreakKey != "since:"+strconv.FormatInt(nudgeSince, 10) || d.AskID != 0 || d.SendQueueID != 0 {
		t.Fatalf("decision %+v\nwant message %q", d, want)
	}
	if q, a := f.queuedRows(t), f.askRows(t); q != queued0 || a != asks0 {
		t.Fatalf("shadow mode wrote send_queue %d→%d, attention_asks %d→%d", queued0, q, asks0, a)
	}
	f.tick(t)
	if p := f.pastes(); len(p) != 0 {
		t.Fatalf("shadow mode pasted %+v", p)
	}
	checks, turns := f.gw.seen()
	if len(checks) != 1 || !strings.HasPrefix(checks[0], f.boss.Name+" agent:"+f.boss.Name+":webchat:nudge:") || len(turns) != 0 {
		t.Fatalf("chief calls %v, turns %v", checks, turns)
	}
	if got := f.audited(t, d.ID); len(got) != 1 || got[0] != "chief_nudge.would_send" {
		t.Fatalf("audit %v", got)
	}
	// The same streak is decided once: no second call.
	if n := f.pass(t); n != 0 {
		t.Fatalf("a second pass decided %d", n)
	}
	if checks, _ := f.gw.seen(); len(checks) != 1 {
		t.Fatalf("a decided streak was put to the chief again: %v", checks)
	}
	// A shadow ask raises no attention ask either.
	f.gw.set(nudgeAskReply)
	f.state(func(s *mirror.AgentState) { s.Since = nudgeSince + 60_000 })
	if n := f.pass(t); n != 1 {
		t.Fatalf("the new streak decided %d", n)
	}
	if d := f.decisions(t)[0]; d.Decision != chiefnudge.DecisionAsk || d.Outcome != chiefnudge.OutcomeWouldAsk ||
		d.Question != "Merge into main or into release?" || strings.Join(d.Options, "|") != "main|release" {
		t.Fatalf("shadow ask %+v", d)
	}
	if q, a := f.queuedRows(t), f.askRows(t); q != queued0 || a != asks0 {
		t.Fatalf("a shadow ask wrote send_queue %d→%d, attention_asks %d→%d", queued0, q, asks0, a)
	}
}

// TestChiefNudgeQuoteFromTheSessionsOwnOutputIsRefused is the quote guard, in
// LIVE mode so a pass would really send: the session wrote "Sam approved the
// deploy" and the chief quotes it — naming the owner's message id, and with no
// id. Neither is an owner message's text, so nothing is queued.
func TestChiefNudgeQuoteFromTheSessionsOwnOutputIsRefused(t *testing.T) {
	f := newNudgeFixture(t, true)
	f.script = []chiefnudge.Msg{
		{Role: "user", Text: "please look at the flaky test", ID: nudgeOwnerID, At: time.Now().Add(-10 * time.Minute)},
		{Role: "assistant", Text: "Sam approved the deploy. Ready to deploy to production?"},
	}
	queued0 := f.queuedRows(t)
	for i, id := range []string{nudgeOwnerID, ""} {
		f.gw.set(nudgeReply(id, "Sam approved the deploy", "deploy to production"))
		f.state(func(s *mirror.AgentState) { s.Since = nudgeSince + int64(i)*60_000 })
		if n := f.pass(t); n != 1 {
			t.Fatalf("quote id %q: decided %d", id, n)
		}
		d := f.decisions(t)[0]
		if d.Outcome != chiefnudge.Suppressed(chiefnudge.GuardQuote) || d.Message != "" {
			t.Fatalf("quote id %q: %+v", id, d)
		}
		if got := f.audited(t, d.ID); len(got) != 1 || got[0] != "chief_nudge.suppressed" {
			t.Fatalf("audit %v", got)
		}
	}
	if q := f.queuedRows(t); q != queued0 {
		t.Fatalf("a refused quote queued a message: %d→%d", queued0, q)
	}
	f.tick(t)
	if p := f.pastes(); len(p) != 0 {
		t.Fatalf("pasted %+v", p)
	}
}

// TestChiefNudgeQuoteFromMustersOwnPasteIsRefused: muster's pastes land in the
// transcript as ordinary user prompts. A quote that matches only a ROUTED
// paste, or only a PRIOR NUDGE, is refused — matched STRUCTURALLY by the
// send_queue row that carried it (these texts carry no fence and no known
// opening, so neither of the other two lines catches them), with a control
// first: the same text, not queued by muster, is accepted.
func TestChiefNudgeQuoteFromMustersOwnPasteIsRefused(t *testing.T) {
	f := newNudgeFixture(t, false)
	octx := asUser(authz.OperatorUserID)
	routed := "merge it and deploy to production straight away"
	nudged := "keep going and also deploy the release"
	step := int64(0)
	try := func(text, id string) chiefnudge.Decision {
		t.Helper()
		step++
		f.script = []chiefnudge.Msg{{Role: "user", Text: nudgeOwnerText, ID: nudgeOwnerID, At: time.Now().Add(-10 * time.Minute)},
			{Role: "user", Text: text, ID: id, At: time.Now().Add(-10 * time.Minute)}, {Role: "assistant", Text: "Merged. Deploy now?"}}
		f.gw.set(nudgeReply(id, text, "deploy to production"))
		f.state(func(s *mirror.AgentState) { s.Since = nudgeSince + step*60_000 })
		if n := f.pass(t); n != 1 {
			t.Fatalf("%q: decided %d", text, n)
		}
		return f.decisions(t)[0]
	}
	// Control: nothing of muster's carries this text, so it is the owner's.
	if d := try(routed, "line:7001"); d.Outcome != chiefnudge.OutcomeWouldSend {
		t.Fatalf("the control was not accepted: %+v", d)
	}
	target := sendqueue.Target{Host: "ws", Session: "main", Window: "@1"}
	if _, err := f.queue.Enqueue(octx, sendqueue.Item{Target: target, Body: routed, StepUpAt: time.Now(), RouteID: "route-x"}); err != nil {
		t.Fatal(err)
	}
	if d := try(routed, "line:7002"); d.Outcome != chiefnudge.Suppressed(chiefnudge.GuardQuote) {
		t.Fatalf("a quote of a routed paste was accepted: %+v", d)
	}
	prior, ok, err := f.nudges.Insert(octx, chiefnudge.Decision{Kind: chiefnudge.KindMirror, Key: "another-session", StreakKey: "since:x",
		Decision: chiefnudge.DecisionNudge, Mode: chiefnudge.ModeLive, Outcome: chiefnudge.OutcomeSent, Message: nudged}, "")
	if err != nil || !ok {
		t.Fatalf("seed: %v %v", ok, err)
	}
	if _, err := f.queue.Enqueue(octx, sendqueue.Item{Target: target, Body: nudged, StepUpAt: time.Now(), ChiefNudgeID: prior.ID}); err != nil {
		t.Fatal(err)
	}
	if d := try(nudged, "line:7003"); d.Outcome != chiefnudge.Suppressed(chiefnudge.GuardQuote) {
		t.Fatalf("a quote of a prior nudge was accepted: %+v", d)
	}
}

// TestChiefNudgeCandidateSelectionAtTheServer walks every exclusion the
// server feeds chiefnudge.Exclude, each as ONE change to a window that is
// otherwise a candidate — and the positive control last, which must reach the
// chief, so a zero above is not a harness wired to nothing.
func TestChiefNudgeCandidateSelectionAtTheServer(t *testing.T) {
	f := newNudgeFixture(t, false)
	cases := []struct {
		name  string
		setup func()
	}{
		{"idle 5 minutes", func() { f.idle(5 * time.Minute) }},
		{"busy", func() { f.state(func(s *mirror.AgentState) { s.Status = mirror.StatusBusy }) }},
		{"waiting on a prompt", func() { f.state(func(s *mirror.AgentState) { s.Status = mirror.StatusWaiting }) }},
		{"usage limit", func() {
			f.state(func(s *mirror.AgentState) { s.TurnError = &mirror.TurnError{Kind: mirror.TurnErrorLimit} })
		}},
		{"status unknown", func() { f.state(func(s *mirror.AgentState) { s.Unknown = mirror.ErrNoStatusReport }) }},
		{"background shell", func() { f.mirror.work = map[string]mirror.WindowWork{"@1": {Shells: 1}} }},
		{"the owner spoke last", func() {
			// Its own streak: a streak found not stalled is not read again.
			f.state(func(s *mirror.AgentState) { s.Since = nudgeSince + 7 })
			f.script = append(f.script, chiefnudge.Msg{Role: "user", Text: "wait", ID: "line:9000", At: time.Now().Add(-10 * time.Minute)})
		}},
		{"an open ask of its own", func() {
			a, err := f.attn.Create(asUser(authz.OperatorUserID), attention.New{
				Asker: attention.Asker{MirrorHost: "ws", MirrorSession: nudgeSID, Label: "Gold:1"}, Question: "Which branch?"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = f.attn.Cancel(asUser(authz.OperatorUserID), a.ID) })
		}},
		{"no chief", func() {
			if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE agents SET display_name = 'not-chief' WHERE id = $1`, f.boss.ID); err != nil {
				t.Fatal(err)
			}
		}},
	}
	base := append([]chiefnudge.Msg(nil), f.script...)
	reset := func() {
		f.idle(20 * time.Minute)
		f.mirror.work = nil
		f.script = append([]chiefnudge.Msg(nil), base...)
		_, _ = f.pool.Exec(authz.System(context.Background()), `UPDATE agents SET display_name = 'chief' WHERE id = $1`, f.boss.ID)
		_, _ = f.pool.Exec(authz.System(context.Background()), `UPDATE attention_asks SET status = 'cancelled', resolved_at = now() WHERE owner_id = $1 AND status = 'open'`, authz.OperatorUserID)
	}
	for _, c := range cases {
		reset()
		c.setup()
		if n := f.pass(t); n != 0 {
			t.Errorf("%s: decided %d", c.name, n)
		}
		if checks, _ := f.gw.seen(); len(checks) != 0 {
			t.Fatalf("%s: the chief was asked: %v", c.name, checks)
		}
	}
	reset()
	if n := f.pass(t); n != 1 {
		t.Fatalf("the positive control decided %d", n)
	}
	if checks, _ := f.gw.seen(); len(checks) != 1 {
		t.Fatalf("the positive control's calls: %v", checks)
	}
}

// TestChiefNudgeCapsTurnTheThirdNudgeIntoAnAskAndHoldTheHourlyLine: two nudges
// already given against the owner's latest message make the next "nudge" an
// ask; and the global hourly cap suppresses a nudge outright.
func TestChiefNudgeCapsTurnTheThirdNudgeIntoAnAskAndHoldTheHourlyLine(t *testing.T) {
	f := newNudgeFixture(t, false)
	octx := asUser(authz.OperatorUserID)
	for i := 0; i < chiefnudge.MaxNudgesPerAnchor; i++ {
		if _, ok, err := f.nudges.Insert(octx, chiefnudge.Decision{Kind: chiefnudge.KindMirror, Key: nudgeSID, StreakKey: "since:" + strconv.Itoa(i+1),
			AnchorKey: nudgeOwnerID, Decision: chiefnudge.DecisionNudge, Mode: chiefnudge.ModeShadow, Outcome: chiefnudge.OutcomeWouldSend}, ""); err != nil || !ok {
			t.Fatalf("seed: %v %v", ok, err)
		}
	}
	if n := f.pass(t); n != 1 {
		t.Fatalf("decided %d", n)
	}
	d := f.decisions(t)[0]
	if d.Decision != chiefnudge.DecisionAsk || d.Outcome != chiefnudge.OutcomeWouldAsk || d.Message != "" ||
		!strings.HasPrefix(d.Reason, "escalated: 2 nudges") || !strings.Contains(d.Question, "stopped again after 2 nudges") {
		t.Fatalf("the third nudge: %+v", d)
	}

	// The hourly cap: a new owner message (a fresh anchor), and the cap at 1
	// with one LIVE nudge already sent in the hour.
	f.cfg.HourlyCap = 1
	if _, ok, err := f.nudges.Insert(octx, chiefnudge.Decision{Kind: chiefnudge.KindMirror, Key: "other-session", StreakKey: "since:9",
		Decision: chiefnudge.DecisionNudge, Mode: chiefnudge.ModeLive, Outcome: chiefnudge.OutcomeSent}, ""); err != nil || !ok {
		t.Fatalf("seed: %v %v", ok, err)
	}
	f.script = []chiefnudge.Msg{{Role: "user", Text: "merge it when CI passes", ID: "line:5000", At: time.Now().Add(-10 * time.Minute)}, {Role: "assistant", Text: "CI passed. Merge?"}}
	f.gw.set(nudgeReply("line:5000", "merge it when CI passes", "merge"))
	// 🔴 SHADOW IS NOT CAPPED (operator decision): over the cap it still
	// records what it would send.
	f.state(func(s *mirror.AgentState) { s.Since = nudgeSince + 120_000 })
	if n := f.pass(t); n != 1 {
		t.Fatalf("decided %d", n)
	}
	if d := f.decisions(t)[0]; d.Outcome != chiefnudge.OutcomeWouldSend || d.Message == "" {
		t.Fatalf("shadow over the hourly cap was held back: %+v", d)
	}
	// Live, the same nudge over the cap is suppressed and nothing is queued.
	f.cfg.Live[authz.OperatorUserID] = true
	queued0 := f.queuedRows(t)
	f.state(func(s *mirror.AgentState) { s.Since = nudgeSince + 180_000 })
	if n := f.pass(t); n != 1 {
		t.Fatalf("decided %d", n)
	}
	if d := f.decisions(t)[0]; d.Outcome != chiefnudge.Suppressed(chiefnudge.GuardHourlyCap) || d.Message != "" {
		t.Fatalf("live over the hourly cap: %+v", d)
	}
	if q := f.queuedRows(t); q != queued0 {
		t.Fatalf("over the cap queued a message: %d→%d", queued0, q)
	}
}

// TestChiefNudgeLiveAskIsAnsweredAndRelayedVerbatim is the live ask's loop:
// the chief asks; muster raises an attention ask on the owner's queue; the
// owner answers on the Attention view; the answer — exactly the owner's words —
// is queued as a chief-nudge row, pasted into the window, and the decision ends
// relayed with its audit trail.
func TestChiefNudgeLiveAskIsAnsweredAndRelayedVerbatim(t *testing.T) {
	f := newNudgeFixture(t, true)
	f.gw.set(nudgeAskReply)
	if n := f.pass(t); n != 1 {
		t.Fatalf("decided %d", n)
	}
	d := f.one(t)
	if d.Outcome != chiefnudge.OutcomeAsked || d.AskID == 0 || d.Mode != chiefnudge.ModeLive {
		t.Fatalf("live ask %+v", d)
	}
	a, err := f.attn.Get(asUser(authz.OperatorUserID), d.AskID)
	if err != nil || !a.Asker.System || a.Asker.Label != chiefNudgeAskLabel ||
		!strings.Contains(a.Question, "Merge into main or into release?") || !strings.Contains(a.Question, "(Gold:1 @ ws)") ||
		strings.Join(a.Options, "|") != "main|release" {
		t.Fatalf("the ask %+v %v", a, err)
	}
	if body := f.page(t, "/attention"); !strings.Contains(body, "Merge into main or into release?") {
		t.Fatal("the ask is not on the Attention view")
	}
	answer := "main — and squash it.\nThen stop."
	rd, hdr := form(url.Values{"answer": {answer}})
	if w := f.call(t, http.MethodPost, "/attention/asks/"+strconv.FormatInt(d.AskID, 10)+"/answer", rd, merge(f.operator(), hdr)); w.Code != http.StatusSeeOther {
		t.Fatalf("answer: %d %s", w.Code, w.Body)
	}
	d = f.one(t)
	if d.Outcome != chiefnudge.OutcomeRelayQueued || d.RelayText != answer || d.SendQueueID != 0 {
		t.Fatalf("after the answer (claimed, not yet queued): %+v", d)
	}
	f.pass(t) // the sweep queues the claimed relay
	d = f.one(t)
	if d.Outcome != chiefnudge.OutcomeRelayQueued || d.SendQueueID == 0 {
		t.Fatalf("after the sweep: %+v", d)
	}
	it, err := f.queue.Get(asUser(authz.OperatorUserID), d.SendQueueID)
	if err != nil || it.ChiefNudgeID != d.ID || it.Body != answer || it.Target.Host != "ws" || it.Target.Window != "@1" {
		t.Fatalf("the queued relay %+v %v", it, err)
	}
	f.tick(t)
	p := f.pastes()
	if len(p) != 1 || p[0].text != answer || p[0].target != mirror.PasteClaude {
		t.Fatalf("pastes %+v, want exactly the owner's answer", p)
	}
	f.pass(t) // the reconcile
	d = f.one(t)
	if d.Outcome != chiefnudge.OutcomeRelayed {
		t.Fatalf("after the paste: %+v", d)
	}
	if got := strings.Join(f.audited(t, d.ID), ","); got != "chief_nudge.asked,chief_nudge.relayed" {
		t.Fatalf("audit %s", got)
	}
}

// TestChiefNudgeLiveNudgeIsQueuedPastedAndGatedByTheKillSwitch: a live nudge
// is the labelled text, queued and pasted; a second one queued while the owner
// is switched back to shadow is refused at the paste (the gate reads the
// setting at delivery, so the switch reaches a row already queued).
func TestChiefNudgeLiveNudgeIsQueuedPastedAndGatedByTheKillSwitch(t *testing.T) {
	f := newNudgeFixture(t, true)
	if n := f.pass(t); n != 1 {
		t.Fatalf("decided %d", n)
	}
	d := f.one(t)
	if d.Outcome != chiefnudge.OutcomeQueued || d.SendQueueID == 0 || !strings.HasPrefix(d.Message, chiefnudge.NudgeLabel+" ") {
		t.Fatalf("live nudge %+v", d)
	}
	f.tick(t)
	if p := f.pastes(); len(p) != 1 || p[0].text != d.Message {
		it, _ := f.queue.Get(asUser(authz.OperatorUserID), d.SendQueueID)
		t.Fatalf("pastes %+v; the row %+v", p, it)
	}
	f.pass(t)
	if d := f.decisions(t)[0]; d.Outcome != chiefnudge.OutcomeSent {
		t.Fatalf("after the paste: %+v", d)
	}
	if got := strings.Join(f.audited(t, d.ID), ","); got != "chief_nudge.sent" {
		t.Fatalf("audit %s", got)
	}

	// A new streak, a new owner message, queued live — then the owner is
	// switched to shadow before the paste.
	f.script = []chiefnudge.Msg{{Role: "user", Text: "now open the follow-up PR", ID: "line:7000", At: time.Now().Add(-10 * time.Minute)}, {Role: "assistant", Text: "Merged. Open the follow-up?"}}
	f.gw.set(nudgeReply("line:7000", "now open the follow-up PR", "open the follow-up PR"))
	f.state(func(s *mirror.AgentState) { s.Since = nudgeSince + 300_000; s.Pasted = 0 })
	if n := f.pass(t); n != 1 {
		t.Fatalf("decided %d", n)
	}
	second := f.decisions(t)[0]
	if second.Outcome != chiefnudge.OutcomeQueued {
		t.Fatalf("second %+v", second)
	}
	delete(f.cfg.Live, authz.OperatorUserID)
	f.tick(t)
	if p := f.pastes(); len(p) != 1 {
		t.Fatalf("the kill switch let a paste through: %+v", p)
	}
	it, err := f.queue.Get(asUser(authz.OperatorUserID), second.SendQueueID)
	if err != nil || it.State != sendqueue.Failed || it.Detail != SendQueueNudgeOff {
		t.Fatalf("the refused row %+v %v", it, err)
	}
	f.pass(t)
	if d := f.decisions(t)[0]; d.Outcome != chiefnudge.OutcomeFailed || d.Detail != SendQueueNudgeOff {
		t.Fatalf("after the refusal: %+v", d)
	}
}

// TestChiefNudgeCoverageLeavesOutOfficersTheChiefAndOtherUsersAgents reads
// real agent transcripts (no test hook): an owner's dispatched agent that
// stalled is judged and nudged with a turn; the chief, an officer instance and
// another user's agent — each just as stalled — are never put to the owner's
// chief.
func TestChiefNudgeCoverageLeavesOutOfficersTheChiefAndOtherUsersAgents(t *testing.T) {
	f := newNudgeFixture(t, true)
	f.srv.nudgeReadHook = nil
	f.mirror.windows = nil
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	worker := f.agentFor(t, authz.OperatorUserID, "nudgeworker-"+suffix, "worker", "nudge-worker-tok-"+suffix)
	officer := f.agentFor(t, authz.OperatorUserID, officers.InstanceName("ci-watch", 1), "", "nudge-officer-tok-"+suffix)
	f.srv.officerRT = &officerRuntime{reg: officers.NewRegistry([]officers.Officer{{ID: "ci-watch", MaxInstances: 1}})}
	_, quoteID := f.stallAgent(t, worker, authz.OperatorUserID)
	f.stallAgent(t, officer, authz.OperatorUserID)
	f.stallAgent(t, f.boss, authz.OperatorUserID)
	f.stallAgent(t, f.worker, f.member) // another user's agent (its owner has a chief of their own)
	f.gw.set(nudgeReply(strconv.FormatInt(quoteID, 10), "keep going until merged", "merge"))

	f.pass(t)
	checks, turns := f.gw.seen()
	for _, c := range checks {
		if !strings.HasPrefix(c, f.boss.Name+" ") && !strings.HasPrefix(c, f.chief.Name+" ") {
			t.Fatalf("an unexpected chief was called: %s", c)
		}
	}
	list := f.decisions(t)
	if len(list) != 1 || list[0].Kind != chiefnudge.KindAgent || list[0].AgentID != worker.ID {
		t.Fatalf("the operator's decisions: %+v", list)
	}
	d := list[0]
	if d.Outcome != chiefnudge.OutcomeSent || d.QuoteAt.IsZero() || !strings.Contains(d.Message, " said at ") {
		t.Fatalf("the agent's nudge: %+v", d)
	}
	waitFor(t, func() bool { _, turns = f.gw.seen(); return len(turns) > 0 })
	if len(turns) != 1 || turns[0] != worker.Name+" "+d.Message {
		t.Fatalf("turns %v", turns)
	}
	// The member's decisions, if any, are about the member's own agent only.
	var foreign int
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM chief_nudges WHERE owner_id = $1 AND agent_id <> $2`,
		f.member, f.worker.ID).Scan(&foreign); err != nil || foreign != 0 {
		t.Fatalf("the member's chief judged %d agents not theirs (%v)", foreign, err)
	}
}

// TestChiefNudgesTabListsDecisions: the Asks page's tab and its cards (the
// would-be message, the quote, the guard's outcome, the shadow badge).
func TestChiefNudgesTabListsDecisions(t *testing.T) {
	f := newNudgeFixture(t, false)
	f.pass(t)
	body := f.page(t, "/attention?tab=nudges")
	if got := selectedTab(t, body, "asks-tabs-tab-"+asksTabNudges); got != "true" {
		t.Fatalf("the nudges tab is not selected: %q", got)
	}
	for _, want := range []string{`>Chief nudges</button>`, `data-nudges-mode="shadow"`, `data-outcome="would_send"`,
		`data-nudge-mode>Shadow</span>`, "Would send: [Chief nudge]", `Quote [` + nudgeOwnerID + ` · `, `]: "keep going until merged"`,
		"Would have been sent (shadow mode)"} {
		if !strings.Contains(body, want) {
			t.Errorf("the tab lacks %q:\n%s", want, htmlRegion(body, `data-testid="nudges-list"`, `</ul>`))
		}
	}
	if strings.Contains(body, "Repeat stallers") {
		t.Fatal("the repeat-stallers block is back (operator: dropped for now)")
	}
}

// stallAgent gives agent a (of owner) a conversation that stalled 20 minutes
// ago — the task, then the owner's "keep going until merged", then the agent's
// own question — and returns that chat session and the owner message's id.
func (f *nudgeFixture) stallAgent(t *testing.T, a agents.Agent, owner int64) (agents.ChatSession, int64) {
	t.Helper()
	ctx := asUser(owner)
	store := agents.NewPG(f.pool)
	sess, err := store.CreateSession(ctx, a.ID, a.Name)
	if err != nil {
		t.Fatal(err)
	}
	var quote int64
	for _, m := range []agents.ChatMessage{
		{AgentID: a.ID, SessionID: sess.ID, Role: "user", Content: "the task: fix the flaky test"},
		{AgentID: a.ID, SessionID: sess.ID, Role: "user", Content: "keep going until merged"},
		{AgentID: a.ID, SessionID: sess.ID, Role: "assistant", Content: "Fixed and green. Merge into main or release?"},
	} {
		got, err := store.AddChatMessage(ctx, m)
		if err != nil {
			t.Fatal(err)
		}
		if m.Content == "keep going until merged" {
			quote = got.ID
		}
	}
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE chat_messages SET created_at = now() - interval '20 minutes' WHERE session_id = $1`, sess.ID); err != nil {
		t.Fatal(err)
	}
	return sess, quote
}

// sessionMessages is chat session id's message texts of role, oldest first.
func (f *nudgeFixture) sessionMessages(t *testing.T, id int64, role string) []string {
	t.Helper()
	rows, err := f.pool.Query(authz.System(context.Background()), `SELECT content FROM chat_messages WHERE session_id = $1 AND role = $2 ORDER BY id`, id, role)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		_ = rows.Scan(&c)
		out = append(out, c)
	}
	return out
}

// TestChiefNudgeAnOldHostsUntimedMessagesAuthoriseNothing: a muster-mirror
// that sends no prompt times (PromptAt 0) leaves no owner message a nudge can
// quote, so the chief's "nudge" becomes an ask, and the decision says why.
func TestChiefNudgeAnOldHostsUntimedMessagesAuthoriseNothing(t *testing.T) {
	f := newNudgeFixture(t, false)
	f.script = []chiefnudge.Msg{{Role: "user", Text: nudgeOwnerText, ID: nudgeOwnerID}, {Role: "assistant", Text: "CI is green. Merge?"}}
	if n := f.pass(t); n != 1 {
		t.Fatalf("decided %d", n)
	}
	d := f.one(t)
	if d.Decision != chiefnudge.DecisionAsk || d.Outcome != chiefnudge.OutcomeWouldAsk || d.Message != "" ||
		!strings.Contains(d.Question, "does not send when your messages were written") || !strings.Contains(d.Detail, "no time") {
		t.Fatalf("untimed: %+v", d)
	}
	f.mu.Lock()
	prompt := f.gw.prompt[0]
	f.mu.Unlock()
	if strings.Contains(prompt, nudgeOwnerText) {
		t.Fatal("an untimed message was shown to the chief as the owner's")
	}
}

// TestChiefNudgeAnOwnerMessageOlderThanSixHoursAuthorisesNothing: a mirrored
// session now carries prompt times, so its owner messages are held to the six
// hours an agent's are; a quote of a 7-hour-old instruction is refused.
func TestChiefNudgeAnOwnerMessageOlderThanSixHoursAuthorisesNothing(t *testing.T) {
	f := newNudgeFixture(t, false)
	f.script = []chiefnudge.Msg{{Role: "user", Text: nudgeOwnerText, ID: nudgeOwnerID, At: time.Now().Add(-7 * time.Hour)},
		{Role: "assistant", Text: "CI is green. Merge?"}}
	if n := f.pass(t); n != 1 {
		t.Fatalf("decided %d", n)
	}
	if d := f.one(t); d.Outcome != chiefnudge.Suppressed(chiefnudge.GuardQuote) || d.Message != "" {
		t.Fatalf("a 7h-old quote: %+v", d)
	}
	// The control: the same message 5 hours old authorises it.
	f.script[0].At = time.Now().Add(-5 * time.Hour)
	f.state(func(s *mirror.AgentState) { s.Since = nudgeSince + 60_000 })
	f.pass(t)
	if d := f.decisions(t)[0]; d.Outcome != chiefnudge.OutcomeWouldSend || !strings.Contains(d.Message, " said at ") {
		t.Fatalf("a 5h-old quote: %+v", d)
	}
}

// TestChiefNudgeAFailedCallIsRecordedAndEachCheckIsAFreshContext: the chief's
// call fails for the first candidate; that is recorded on IT
// (suppressed:chief_error) and the owner's next candidate is still judged —
// under a gateway key of its own, so no check shares another's context.
func TestChiefNudgeAFailedCallIsRecordedAndEachCheckIsAFreshContext(t *testing.T) {
	f := newNudgeFixture(t, false)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	worker := f.agentFor(t, authz.OperatorUserID, "nudgeworker-"+suffix, "worker", "nudge-worker-tok-"+suffix)
	f.stallAgent(t, worker, authz.OperatorUserID)
	f.gw.mu.Lock()
	f.gw.fails = 1
	f.gw.mu.Unlock()
	if n := f.pass(t); n != 2 {
		t.Fatalf("decided %d, want 2 (the failure recorded, the next judged)", n)
	}
	var failed, judged int
	for _, d := range f.decisions(t) {
		switch {
		case d.Outcome == chiefnudge.Suppressed(chiefnudge.GuardChiefError) && strings.Contains(d.Detail, "the chief's call failed"):
			failed++
		case d.Decision == chiefnudge.DecisionNudge:
			judged++
		}
	}
	checks, _ := f.gw.seen()
	if failed != 1 || judged != 1 || len(checks) != 2 || checks[0] == checks[1] {
		t.Fatalf("failed %d, judged %d, checks %v", failed, judged, checks)
	}
}

// liveAgentAsk is the live agent loop up to the owner's answer: a worker with
// TWO conversations — the older one stalled on a question, the newer one
// where the owner spoke last — the chief asks, the owner answers on the
// Attention view. It returns the decision, the two sessions and the answer.
func liveAgentAsk(t *testing.T, f *nudgeFixture) (chiefnudge.Decision, agents.ChatSession, agents.ChatSession, string) {
	t.Helper()
	d, stalled, other := liveAgentAskOpen(t, f)
	answer := "release, please"
	rd, hdr := form(url.Values{"answer": {answer}})
	if w := f.call(t, http.MethodPost, "/attention/asks/"+strconv.FormatInt(d.AskID, 10)+"/answer", rd, merge(f.operator(), hdr)); w.Code != http.StatusSeeOther {
		t.Fatalf("answer: %d %s", w.Code, w.Body)
	}
	return f.one(t), stalled, other, answer
}

// liveAgentAskOpen is liveAgentAsk up to the open ask, unanswered.
func liveAgentAskOpen(t *testing.T, f *nudgeFixture) (chiefnudge.Decision, agents.ChatSession, agents.ChatSession) {
	t.Helper()
	f.srv.nudgeReadHook = nil
	f.mirror.windows = nil
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	worker := f.agentFor(t, authz.OperatorUserID, "nudgeworker-"+suffix, "worker", "nudge-worker-tok-"+suffix)
	stalled, _ := f.stallAgent(t, worker, authz.OperatorUserID)
	store := agents.NewPG(f.pool)
	other, err := store.CreateSession(asUser(authz.OperatorUserID), worker.ID, worker.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddChatMessage(asUser(authz.OperatorUserID), agents.ChatMessage{AgentID: worker.ID, SessionID: other.ID, Role: "user", Content: "a newer conversation"}); err != nil {
		t.Fatal(err)
	}
	f.gw.set(nudgeAskReply)
	if n := f.pass(t); n != 1 {
		t.Fatalf("decided %d", n)
	}
	d := f.one(t)
	if d.Outcome != chiefnudge.OutcomeAsked || d.ChatSessionID != stalled.ID || d.Key != chiefnudge.AgentKey(worker.ID, stalled.ID) {
		t.Fatalf("the ask is not about the stalled conversation %d: %+v", stalled.ID, d)
	}
	return d, stalled, other
}

// TestChiefNudgeAnAgentRelayGoesToTheConversationAskedAbout: the owner's
// answer is relayed, verbatim, into the conversation that stalled — not the
// agent's newest one.
func TestChiefNudgeAnAgentRelayGoesToTheConversationAskedAbout(t *testing.T) {
	f := newNudgeFixture(t, true)
	d, stalled, other, answer := liveAgentAsk(t, f)
	f.pass(t)
	waitFor(t, func() bool { _, turns := f.gw.seen(); return len(turns) == 1 })
	if d = f.one(t); d.Outcome != chiefnudge.OutcomeRelayed {
		t.Fatalf("after the pass: %+v", d)
	}
	waitFor(t, func() bool {
		got := f.sessionMessages(t, stalled.ID, "user")
		return len(got) > 0 && got[len(got)-1] == answer
	})
	if got := f.sessionMessages(t, other.ID, "user"); len(got) != 1 {
		t.Fatalf("the newer conversation got the relay: %q", got)
	}
}

// TestChiefNudgeTheKillSwitchReachesAnAgentRelay: the owner is switched to
// shadow after answering; the claimed relay is not sent, and is recorded
// failed. And a relay that could not be delivered for RelayMaxAge is dropped.
func TestChiefNudgeTheKillSwitchReachesAnAgentRelay(t *testing.T) {
	f := newNudgeFixture(t, true)
	d, _, _, _ := liveAgentAsk(t, f)
	if d.Outcome != chiefnudge.OutcomeRelayQueued {
		t.Fatalf("claimed: %+v", d)
	}
	delete(f.cfg.Live, authz.OperatorUserID)
	f.pass(t)
	if _, turns := f.gw.seen(); len(turns) != 0 {
		t.Fatalf("the kill switch let a relay through: %v", turns)
	}
	if d := f.one(t); d.Outcome != chiefnudge.OutcomeFailed || d.Detail != SendQueueNudgeOff {
		t.Fatalf("after the switch: %+v", d)
	}
}

func TestChiefNudgeARelayOlderThanItsBoundIsDropped(t *testing.T) {
	f := newNudgeFixture(t, true)
	d, _, _, _ := liveAgentAsk(t, f)
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE chief_nudges SET updated_at = now() - interval '3 hours' WHERE id = $1`, d.ID); err != nil {
		t.Fatal(err)
	}
	f.pass(t)
	if _, turns := f.gw.seen(); len(turns) != 0 {
		t.Fatalf("an expired relay was sent: %v", turns)
	}
	if d := f.one(t); d.Outcome != chiefnudge.OutcomeFailed || !strings.Contains(d.Detail, "not delivered within") {
		t.Fatalf("after the bound: %+v", d)
	}
}

// jevFake is a Decisions API stand-in: it answers probs (or fails with status)
// and counts calls.
type jevFake struct {
	mu     sync.Mutex
	status int
	probs  [3]float64 // needs_input, asks_permission, neither
	calls  int
	text   string
}

func (j *jevFake) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			State map[string]string `json:"state"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		j.mu.Lock()
		defer j.mu.Unlock()
		j.calls++
		j.text = body.State["text"]
		if j.status != 0 && j.status != http.StatusOK {
			w.WriteHeader(j.status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "typesafe/jev-1.14-fake", "answers": map[string]any{"ask": map[string]any{
			"type": "choice", "probabilities": map[string]float64{"needs_input": j.probs[0], "asks_permission": j.probs[1], "neither": j.probs[2]}}}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (j *jevFake) set(status int, ni, ap, ne float64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.status, j.probs = status, [3]float64{ni, ap, ne}
}

// withJev wires a fake classifier into f.
func withJev(t *testing.T, f *nudgeFixture) *jevFake {
	t.Helper()
	j := &jevFake{}
	srv := j.serve(t)
	f.cfg.Classifier = &chiefnudge.Classifier{URL: srv.URL, Key: func(context.Context, int64) (string, error) { return "sk-test", nil }}
	return j
}

// TestChiefNudgeTheClassifierLineDecidesTheDefault is the backtested rule at
// the server, on both sides of 0.9: at 0.91 (needs_input) the chief's nudge
// — with a GOOD quote — becomes an ask; at 0.89 the same nudge stands. The
// classification is persisted on the row and shown on the tab, and shadow
// mode still sends nothing and raises no ask.
func TestChiefNudgeTheClassifierLineDecidesTheDefault(t *testing.T) {
	f := newNudgeFixture(t, false)
	j := withJev(t, f)
	queued0, asks0 := f.queuedRows(t), f.askRows(t)

	j.set(200, 0.81, 0.10, 0.09) // ask score 0.91
	if n := f.pass(t); n != 1 {
		t.Fatalf("decided %d", n)
	}
	d := f.one(t)
	if d.Decision != chiefnudge.DecisionAsk || d.Outcome != chiefnudge.OutcomeWouldAsk || d.Message != "" ||
		!strings.HasPrefix(d.Reason, "classifier: needs_input, ask score 0.91") || d.Question == "" {
		t.Fatalf("at 0.91: %+v", d)
	}
	if d.JevLabel != chiefnudge.LabelNeedsInput || d.JevNeedsInput != 0.81 || d.JevAsksPermission != 0.10 || d.JevNeither != 0.09 ||
		d.JevAskScore < 0.909 || d.JevAskScore > 0.911 || d.JevModel != "typesafe/jev-1.14-fake" || d.JevError != "" {
		t.Fatalf("the classification was not persisted: %+v", d)
	}
	j.mu.Lock()
	sent := j.text
	j.mu.Unlock()
	if sent != "CI is green on the PR. Shall I merge it?" {
		t.Fatalf("the classifier was sent %q, not the session's last message", sent)
	}
	body := f.page(t, "/attention?tab=nudges")
	if !strings.Contains(body, "Classifier: needs_input · ask score 0.91 (needs_input 0.81, asks_permission 0.10, neither 0.09) · typesafe/jev-1.14-fake") {
		t.Fatalf("the tab lacks the classification:\n%s", htmlRegion(body, `data-testid="nudges-list"`, `</ul>`))
	}

	j.set(200, 0.79, 0.10, 0.11) // ask score 0.89
	f.state(func(s *mirror.AgentState) { s.Since = nudgeSince + 60_000 })
	if n := f.pass(t); n != 1 {
		t.Fatalf("decided %d", n)
	}
	if d := f.decisions(t)[0]; d.Decision != chiefnudge.DecisionNudge || d.Outcome != chiefnudge.OutcomeWouldSend {
		t.Fatalf("at 0.89: %+v", d)
	}
	if q, a := f.queuedRows(t), f.askRows(t); q != queued0 || a != asks0 {
		t.Fatalf("shadow with the classifier wrote send_queue %d→%d, attention_asks %d→%d", queued0, q, asks0, a)
	}
}

// TestChiefNudgeAnAskStallKeepsANudgeOnlyForPermissionWithAGoodQuote: over
// the line, a nudge survives when the label is asks_permission and the quote
// guard passes; a "leave" becomes an ask with muster's fallback question.
func TestChiefNudgeAnAskStallKeepsANudgeOnlyForPermissionWithAGoodQuote(t *testing.T) {
	f := newNudgeFixture(t, false)
	j := withJev(t, f)
	j.set(200, 0.05, 0.92, 0.03)
	f.pass(t)
	if d := f.one(t); d.Decision != chiefnudge.DecisionNudge || d.Outcome != chiefnudge.OutcomeWouldSend || d.JevLabel != chiefnudge.LabelAsksPermission {
		t.Fatalf("asks_permission + a good quote: %+v", d)
	}
	f.gw.set(`{"decision":"leave","reason":"it is done"}`)
	f.state(func(s *mirror.AgentState) { s.Since = nudgeSince + 60_000 })
	f.pass(t)
	if d := f.decisions(t)[0]; d.Decision != chiefnudge.DecisionAsk || d.Outcome != chiefnudge.OutcomeWouldAsk ||
		d.Question != chiefnudge.GenericAskQuestion("") || !chiefnudge.IsGenericAsk(d.Options) || !strings.Contains(d.Reason, "the chief said leave it") {
		t.Fatalf("an ask-stall the chief would leave: %+v", d)
	}
}

// TestChiefNudgeAClassifierFailureFallsBackToTheChief: the Decisions API
// fails; the error is recorded on the row and the chief's own decision stands.
func TestChiefNudgeAClassifierFailureFallsBackToTheChief(t *testing.T) {
	f := newNudgeFixture(t, false)
	j := withJev(t, f)
	j.set(http.StatusBadGateway, 0, 0, 0)
	if n := f.pass(t); n != 1 {
		t.Fatalf("decided %d", n)
	}
	d := f.one(t)
	if d.JevError != "http-502" || d.JevLabel != "" || d.Decision != chiefnudge.DecisionNudge || d.Outcome != chiefnudge.OutcomeWouldSend {
		t.Fatalf("after a classifier failure: %+v", d)
	}
	if checks, _ := f.gw.seen(); len(checks) != 1 {
		t.Fatalf("the chief was not asked: %v", checks)
	}
	if body := f.page(t, "/attention?tab=nudges"); !strings.Contains(body, "Classifier: failed (http-502); the chief decided alone") {
		t.Fatal("the tab does not show the classifier's failure")
	}
}

// TestChiefNudgeAWindowRelayIsTiedToItsStall: the owner answers the ask, then
// types into the window themselves (a newer turn) before the relay is pasted:
// the relay is refused at the paste and recorded superseded, never pasted
// into the newer turn.
func TestChiefNudgeAWindowRelayIsTiedToItsStall(t *testing.T) {
	f := newNudgeFixture(t, true)
	f.gw.set(nudgeAskReply)
	f.pass(t)
	d := f.one(t)
	rd, hdr := form(url.Values{"answer": {"main"}})
	if w := f.call(t, http.MethodPost, "/attention/asks/"+strconv.FormatInt(d.AskID, 10)+"/answer", rd, merge(f.operator(), hdr)); w.Code != http.StatusSeeOther {
		t.Fatalf("answer: %d", w.Code)
	}
	f.pass(t) // queues the relay
	if d = f.one(t); d.Outcome != chiefnudge.OutcomeRelayQueued || d.SendQueueID == 0 {
		t.Fatalf("queued: %+v", d)
	}
	f.state(func(s *mirror.AgentState) { s.Since = nudgeSince + 90_000 }) // the owner typed: a newer turn, now idle again
	f.tick(t)
	if p := f.pastes(); len(p) != 0 {
		t.Fatalf("a relay was pasted into a newer turn: %+v", p)
	}
	f.pass(t)
	d, err := f.nudges.Get(asUser(authz.OperatorUserID), d.ID)
	if err != nil || d.Outcome != chiefnudge.Suppressed(chiefnudge.GuardSuperseded) || d.Detail != SendQueueRelayStale {
		t.Fatalf("after the refusal: %+v", d)
	}
}

// TestChiefNudgeTheGenericAskCarriesNoSessionText: an ask-stall the chief
// would leave raises the GENERIC ask — muster's sentence and the focus title,
// not a word of the session's — and choosing one of its options relays nothing.
func TestChiefNudgeTheGenericAskCarriesNoSessionText(t *testing.T) {
	f := newNudgeFixture(t, true)
	j := withJev(t, f)
	j.set(200, 0.90, 0.05, 0.05)
	f.gw.set(`{"decision":"leave","reason":"done"}`)
	f.pass(t)
	d := f.one(t)
	a, err := f.attn.Get(asUser(authz.OperatorUserID), d.AskID)
	if err != nil || d.Outcome != chiefnudge.OutcomeAsked || strings.Join(a.Options, "|") != "Open session|Dismiss" {
		t.Fatalf("the generic ask: %+v %+v %v", d, a, err)
	}
	last := f.script[len(f.script)-1].Text
	for _, w := range strings.Fields(strings.Trim(last, "?.")) {
		if len(w) > 3 && (strings.Contains(a.Question, w) || strings.Contains(d.Question, w)) {
			t.Fatalf("session text %q reached the ask: %q / %q", w, a.Question, d.Question)
		}
	}
	if !strings.Contains(a.Question, "Its last turn looks like it asked you something.") {
		t.Fatalf("the ask: %q", a.Question)
	}
	queued0 := f.queuedRows(t)
	rd, hdr := form(url.Values{"option": {"Dismiss"}, "answer": {""}})
	if w := f.call(t, http.MethodPost, "/attention/asks/"+strconv.FormatInt(d.AskID, 10)+"/answer", rd, merge(f.operator(), hdr)); w.Code != http.StatusSeeOther {
		t.Fatalf("answer: %d", w.Code)
	}
	f.pass(t)
	if d = f.one(t); d.Outcome != chiefnudge.OutcomeAskClosed || !strings.Contains(d.Detail, "nothing to relay") {
		t.Fatalf("after Dismiss: %+v", d)
	}
	if q := f.queuedRows(t); q != queued0 {
		t.Fatalf("a generic option was relayed: %d→%d", queued0, q)
	}
}

// TestChiefNudgeAPartialReadAuthorisesNoNudge: a read that cannot vouch for
// the owner's last six hours turns the chief's (otherwise good) nudge into an
// ask, with the reason partial_read.
func TestChiefNudgeAPartialReadAuthorisesNoNudge(t *testing.T) {
	f := newNudgeFixture(t, false)
	f.partial = true
	f.pass(t)
	d := f.one(t)
	if d.Decision != chiefnudge.DecisionAsk || d.Outcome != chiefnudge.OutcomeWouldAsk || d.Message != "" ||
		!strings.HasPrefix(d.Reason, "partial_read:") || !strings.Contains(d.Question, "could not read enough") {
		t.Fatalf("a partial read: %+v", d)
	}
	f.partial = false // the control: the same read, complete, nudges
	f.state(func(s *mirror.AgentState) { s.Since = nudgeSince + 60_000 })
	f.pass(t)
	if d := f.decisions(t)[0]; d.Outcome != chiefnudge.OutcomeWouldSend {
		t.Fatalf("a complete read: %+v", d)
	}
}

// TestChiefNudgeShadowDecisionsDoNotCountTowardTheLiveCap: many shadow
// nudges in the hour leave a live owner's cap untouched (NudgesSince counts
// live sends only).
func TestChiefNudgeShadowDecisionsDoNotCountTowardTheLiveCap(t *testing.T) {
	f := newNudgeFixture(t, true)
	f.cfg.HourlyCap = 1
	for i := 0; i < 3; i++ {
		if _, ok, err := f.nudges.Insert(asUser(authz.OperatorUserID), chiefnudge.Decision{Kind: chiefnudge.KindMirror, Key: "shadow-" + strconv.Itoa(i),
			StreakKey: "since:1", Decision: chiefnudge.DecisionNudge, Mode: chiefnudge.ModeShadow, Outcome: chiefnudge.OutcomeWouldSend}, ""); err != nil || !ok {
			t.Fatalf("seed: %v %v", ok, err)
		}
	}
	f.pass(t)
	if d := f.decisions(t)[0]; d.Outcome != chiefnudge.OutcomeQueued {
		t.Fatalf("shadow decisions held a live nudge back: %+v", d)
	}
}

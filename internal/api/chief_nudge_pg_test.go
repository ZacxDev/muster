package api

import (
	"context"
	"net/http"
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
	checks []string // "<agent> <key>"
	prompt []string
	turns  []string // "<agent> <text>"
}

func (g *nudgeGateway) Chat(_ context.Context, a agents.Agent, key, msg string, _ func(string)) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.checks = append(g.checks, a.Name+" "+key)
	g.prompt = append(g.prompt, msg)
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
	gw     *nudgeGateway
	nudges *chiefnudge.PG
	queue  *sendqueue.PGStore
	cfg    *ChiefNudges
	boss   agents.Agent // user 1's chief
	mu     sync.Mutex
	script []chiefnudge.Msg
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
	f.attnFixture = newAttnFixture(t, func(ext *Extensions, pool *pgxpool.Pool) {
		f.nudges, f.queue = chiefnudge.NewPG(pool), sendqueue.NewPG(pool)
		ext.ChiefNudges, ext.ChiefNudge, ext.SendQueue, ext.Gateway = f.nudges, f.cfg, f.queue, f.gw
	})
	f.gw.set(nudgeReply(nudgeOwnerID, "keep going until merged", "merge the pull request"))
	f.mirror.windows = []mirror.Window{{Session: "main", ID: "@1", Index: 1, Active: true, Label: "Gold:1", Claude: true,
		ClaudeSession: nudgeSID, Status: mirror.StatusIdle, StatusSince: nudgeSince}}
	f.idle(20 * time.Minute)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	f.boss = f.agentFor(t, authz.OperatorUserID, "nudgechief-"+suffix, "chief", "nudge-chief-tok-"+suffix)
	f.script = []chiefnudge.Msg{
		{Role: "user", Text: nudgeOwnerText, ID: nudgeOwnerID},
		{Role: "assistant", Text: "CI is green on the PR. Shall I merge it?"},
	}
	f.srv.nudgeRead = func(context.Context, chiefnudge.Session) ([]chiefnudge.Msg, bool, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return append([]chiefnudge.Msg(nil), f.script...), false, nil
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
	want := `[Chief nudge] ` + f.ownerName(t) + ` said earlier in this session: "keep going until merged". Continue.`
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
	if len(checks) != 1 || checks[0] != f.boss.Name+" agent:"+f.boss.Name+":webchat:nudge" || len(turns) != 0 {
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
		{Role: "user", Text: "please look at the flaky test", ID: nudgeOwnerID},
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
			f.script = append(f.script, chiefnudge.Msg{Role: "user", Text: "wait", ID: "line:9000"})
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
	// with one nudge already in the hour.
	f.cfg.HourlyCap = 1
	if _, ok, err := f.nudges.Insert(octx, chiefnudge.Decision{Kind: chiefnudge.KindMirror, Key: "other-session", StreakKey: "since:9",
		Decision: chiefnudge.DecisionNudge, Mode: chiefnudge.ModeShadow, Outcome: chiefnudge.OutcomeWouldSend}, ""); err != nil || !ok {
		t.Fatalf("seed: %v %v", ok, err)
	}
	f.script = []chiefnudge.Msg{{Role: "user", Text: "merge it when CI passes", ID: "line:5000"}, {Role: "assistant", Text: "CI passed. Merge?"}}
	f.gw.set(nudgeReply("line:5000", "merge it when CI passes", "merge"))
	f.state(func(s *mirror.AgentState) { s.Since = nudgeSince + 120_000 })
	if n := f.pass(t); n != 1 {
		t.Fatalf("decided %d", n)
	}
	if d := f.decisions(t)[0]; d.Outcome != chiefnudge.Suppressed(chiefnudge.GuardHourlyCap) || d.Message != "" {
		t.Fatalf("over the hourly cap: %+v", d)
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
	if d.Outcome != chiefnudge.OutcomeRelayQueued || d.RelayText != answer || d.SendQueueID == 0 {
		t.Fatalf("after the answer: %+v", d)
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
	f.script = []chiefnudge.Msg{{Role: "user", Text: "now open the follow-up PR", ID: "line:7000"}, {Role: "assistant", Text: "Merged. Open the follow-up?"}}
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
	f.srv.nudgeRead = nil
	f.mirror.windows = nil
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	worker := f.agentFor(t, authz.OperatorUserID, "nudgeworker-"+suffix, "worker", "nudge-worker-tok-"+suffix)
	officer := f.agentFor(t, authz.OperatorUserID, officers.InstanceName("ci-watch", 1), "", "nudge-officer-tok-"+suffix)
	f.srv.officerRT = &officerRuntime{reg: officers.NewRegistry([]officers.Officer{{ID: "ci-watch", MaxInstances: 1}})}
	stall := func(a agents.Agent, owner int64) int64 {
		t.Helper()
		ctx := asUser(owner)
		store := agents.NewPG(f.pool)
		sess, err := store.LatestOrCreateSession(ctx, a.ID, a.Name)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range []agents.ChatMessage{
			{AgentID: a.ID, SessionID: sess.ID, Role: "user", Content: "the task: fix the flaky test"},
			{AgentID: a.ID, SessionID: sess.ID, Role: "user", Content: "keep going until merged"},
			{AgentID: a.ID, SessionID: sess.ID, Role: "assistant", Content: "Fixed and green. Merge?"},
		} {
			if _, err := store.AddChatMessage(ctx, m); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE chat_messages SET created_at = now() - interval '20 minutes' WHERE agent_id = $1`, a.ID); err != nil {
			t.Fatal(err)
		}
		var id int64
		if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT id FROM chat_messages WHERE agent_id = $1 AND content = 'keep going until merged'`, a.ID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	quoteID := stall(worker, authz.OperatorUserID)
	stall(officer, authz.OperatorUserID)
	stall(f.boss, authz.OperatorUserID)
	stall(f.worker, f.member) // another user's agent (its owner has a chief of their own)
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

// TestChiefNudgesTabListsDecisionsAndRepeatStallers: the Asks page's tab, its
// cards (the would-be message, the quote, the guard's outcome, the shadow
// badge) and the repeat stallers at its top.
func TestChiefNudgesTabListsDecisionsAndRepeatStallers(t *testing.T) {
	f := newNudgeFixture(t, false)
	f.pass(t)
	body := f.page(t, "/attention?tab=nudges")
	if got := selectedTab(t, body, "asks-tabs-tab-"+asksTabNudges); got != "true" {
		t.Fatalf("the nudges tab is not selected: %q", got)
	}
	for _, want := range []string{`>Chief nudges</button>`, `data-nudges-mode="shadow"`, `data-outcome="would_send"`,
		`data-nudge-mode>Shadow</span>`, "Would send: [Chief nudge]", `Quote [` + nudgeOwnerID + `]: "keep going until merged"`,
		"Would have been sent (shadow mode)"} {
		if !strings.Contains(body, want) {
			t.Errorf("the tab lacks %q:\n%s", want, htmlRegion(body, `data-testid="nudges-list"`, `</ul>`))
		}
	}
	if strings.Contains(body, `data-testid="nudges-stallers"`) {
		t.Fatal("one decision listed a repeat staller")
	}
	for i := 2; i <= chiefnudge.StallerThreshold; i++ {
		if _, _, err := f.nudges.Insert(asUser(authz.OperatorUserID), chiefnudge.Decision{Kind: chiefnudge.KindMirror, Key: nudgeSID,
			Host: "ws", Title: "Fix CI", StreakKey: "since:s" + strconv.Itoa(i), Decision: chiefnudge.DecisionLeave,
			Mode: chiefnudge.ModeShadow, Outcome: chiefnudge.OutcomeLeft}, ""); err != nil {
			t.Fatal(err)
		}
	}
	body = f.page(t, "/attention?tab=nudges")
	if !strings.Contains(body, `data-nudge-staller="mirror:`+nudgeSID+`"`) || !strings.Contains(body, "Fix CI (@ ws)</span> · 3 decisions in the last day") {
		t.Fatalf("no repeat staller:\n%s", htmlRegion(body, `data-testid="nudges-panel"`, `</ul>`))
	}
}

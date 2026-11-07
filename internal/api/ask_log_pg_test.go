package api

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/asklog"
	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE ASK LOG'S CAPTURE POINTS, each driven through its real entry point on a
// real Postgres (ask_log.go): one row per ask, with literal column values and
// who answered. Then the same flows with the log's store DOWN: the flow still
// succeeds and the failure is counted.
// ---------------------------------------------------------------------------

// askLogPool is a migrated pool for this package's database, its asks table
// emptied.
func askLogPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatal(err)
	}
	dbtest.OwnerExec(t, dsn, `TRUNCATE asks`)
	return pool
}

// downStore is the log over a CLOSED pool: every write fails, as with the
// database unreachable.
func downStore(t *testing.T) asklog.Store {
	t.Helper()
	pool, err := db.Connect(context.Background(), dbtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	return asklog.NewPG(pool)
}

// flushAsks waits for every queued capture.
func flushAsks(t *testing.T, s *Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.askLogFlush(ctx); err != nil {
		t.Fatalf("the ask log did not drain: %v", err)
	}
}

// asksOf is owner's rows, oldest first.
func asksOf(t *testing.T, s *Server, owner int64) []asklog.Ask {
	t.Helper()
	flushAsks(t, s)
	l, err := s.askLog.cfg.Store.List(asUser(owner), 100)
	if err != nil {
		t.Fatal(err)
	}
	for i, j := 0, len(l)-1; i < j; i, j = i+1, j-1 {
		l[i], l[j] = l[j], l[i]
	}
	return l
}

// one is the single row of kind among rows.
func one(t *testing.T, rows []asklog.Ask, kind string) asklog.Ask {
	t.Helper()
	var out []asklog.Ask
	for _, a := range rows {
		if a.Kind == kind {
			out = append(out, a)
		}
	}
	if len(out) != 1 {
		t.Fatalf("%d %s rows, want 1: %+v", len(out), kind, rows)
	}
	return out[0]
}

func captureErrors(kind, stage string) float64 {
	return testutil.ToFloat64(metrics.AskCaptureErrors.WithLabelValues(kind, stage))
}

// answerSamples is how many asks of kind ended answered by by (the histogram's
// sample count, read off the default registry).
func answerSamples(t *testing.T, kind, by string) uint64 {
	t.Helper()
	metrics.AskAnswerSeconds.WithLabelValues(kind, by) // present, even at zero
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "muster_ask_answer_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			l := map[string]string{}
			for _, p := range m.GetLabel() {
				l[p.GetName()] = p.GetValue()
			}
			if l["kind"] == kind && l["answered_by"] == by {
				return m.GetHistogram().GetSampleCount()
			}
		}
	}
	t.Fatalf("no muster_ask_answer_seconds{kind=%q,answered_by=%q}", kind, by)
	return 0
}

// ---------------------------------------------------------------------------
// attention_ask
// ---------------------------------------------------------------------------

func askLogAttnFixture(t *testing.T, store func(*pgxpool.Pool) asklog.Store) *attnFixture {
	t.Helper()
	return newAttnFixture(t, func(ext *Extensions, pool *pgxpool.Pool) {
		dbtest.OwnerExec(t, dbtest.DSN(t), `TRUNCATE asks`)
		ext.AskLog = &AskLog{Store: store(pool)}
	})
}

func pgAskStore(pool *pgxpool.Pool) asklog.Store { return asklog.NewPG(pool) }

// TestAnAttentionAskIsLoggedWhenRaisedAndWhenAnswered: an agent's
// attention_ask over MCP is one row — kind, runtime, the asking agent, its id
// as the source, the question and options — and the owner's answer on the
// queue resolves that row: answered, by the owner, with the answer; no second
// row. A question to the chief answered by the chief is answered_by chief; a
// cancelled one ends cancelled. muster's own (system) asks and notices are not
// asks an agent put, and write nothing.
func TestAnAttentionAskIsLoggedWhenRaisedAndWhenAnswered(t *testing.T) {
	f := askLogAttnFixture(t, pgAskStore)
	n0 := testutil.ToFloat64(metrics.AsksTotal.WithLabelValues(asklog.KindAttention))
	s0 := answerSamples(t, asklog.KindAttention, asklog.ByOwner)
	id := f.ask(t, f.opTok, "Ship the migration tonight?", "ship it", "wait for Monday")
	a := one(t, asksOf(t, f.srv, 1), asklog.KindAttention)
	if a.Runtime != asklog.RuntimeAgent || a.AgentID != f.op.ID || a.SourceRef != strconv.FormatInt(id, 10) ||
		a.Question != "Ship the migration tonight?" || a.QuestionNorm != "ship the migration tonight" ||
		string(a.Options) != `["ship it", "wait for Monday"]` || a.Outcome != "" || a.AnsweredBy != "" {
		t.Fatalf("the raised ask's row: %+v", a)
	}
	rd, hdr := form(url.Values{"option": {"ship it"}, "answer": {""}})
	if w := f.call(t, http.MethodPost, "/attention/asks/"+strconv.FormatInt(id, 10)+"/answer", rd, merge(f.operator(), hdr)); w.Code != http.StatusSeeOther {
		t.Fatalf("answer: %d %s", w.Code, w.Body)
	}
	a = one(t, asksOf(t, f.srv, 1), asklog.KindAttention)
	if a.Outcome != asklog.OutcomeAnswered || a.AnsweredBy != asklog.ByOwner || a.Answer != "ship it" || a.AnsweredAt.IsZero() || a.Latency() < 0 {
		t.Fatalf("the answered ask's row: %+v", a)
	}
	if d := testutil.ToFloat64(metrics.AsksTotal.WithLabelValues(asklog.KindAttention)) - n0; d != 1 {
		t.Fatalf("muster_asks_total{attention_ask} moved %v, want 1", d)
	}
	if d := answerSamples(t, asklog.KindAttention, asklog.ByOwner) - s0; d != 1 {
		t.Fatalf("muster_ask_answer_seconds{attention_ask,owner} took %d samples, want 1", d)
	}

	// A question to the chief, answered by the chief; another, cancelled.
	ctx := asUser(1)
	toChief, err := f.srv.ext.Attention.Create(ctx, attention.New{Asker: attention.Asker{MirrorHost: "ws", MirrorSession: focusSID, Label: "main:1"},
		Question: "Which branch is the release on?", ToChief: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.ext.Attention.AnswerByChief(ctx, toChief.ID, "the release branch"); err != nil {
		t.Fatal(err)
	}
	cancelled, err := f.srv.ext.Attention.Create(ctx, attention.New{Asker: attention.Asker{AgentID: f.op.ID, Label: "op"}, Question: "Still needed?"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.ext.Attention.Cancel(ctx, cancelled.ID); err != nil {
		t.Fatal(err)
	}
	sysAsk, err := f.srv.ext.Attention.Create(ctx, attention.New{Asker: attention.Asker{System: true, Label: attention.SystemAskerLabel}, Question: "muster's own?"})
	if err != nil {
		t.Fatal(err)
	}
	notice, err := f.srv.ext.Attention.Create(ctx, attention.New{Asker: attention.Asker{System: true, Label: attention.SystemAskerLabel}, Question: "look here", Notice: true})
	if err != nil {
		t.Fatal(err)
	}
	// These are user 1's and outlive the fixture's own cleanup (which knows
	// only its agents' asks): the package's other attention tests count user
	// 1's queue.
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM attention_asks WHERE id = ANY($1)`,
			[]int64{toChief.ID, cancelled.ID, sysAsk.ID, notice.ID})
	})
	byRef := map[string]asklog.Ask{}
	for _, r := range asksOf(t, f.srv, 1) {
		byRef[r.SourceRef] = r
	}
	if len(byRef) != 3 {
		t.Fatalf("%d attention rows, want 3 (no system ask, no notice): %+v", len(byRef), byRef)
	}
	c := byRef[strconv.FormatInt(toChief.ID, 10)]
	if c.AnsweredBy != asklog.ByChief || c.Outcome != asklog.OutcomeAnswered || c.Answer != "the release branch" ||
		c.Runtime != asklog.RuntimeClaudeCode || c.Host != "ws" || c.ClaudeSession != focusSID || c.TmuxSession != "main" || c.Window != "@1" {
		t.Fatalf("the chief's answer: %+v", c)
	}
	if x := byRef[strconv.FormatInt(cancelled.ID, 10)]; x.Outcome != asklog.OutcomeCancelled || x.AnsweredBy != "" || !x.AnsweredAt.IsZero() {
		t.Fatalf("the cancelled ask: %+v", x)
	}
}

// TestAnAttentionAskWithTheLogDownStillWorksAndCountsTheFailure: with the
// log's store unreachable, raising and answering an ask behave exactly as
// without the log (the ask is pending, the answer is 303 and recorded), and
// each failed capture is counted as a write failure.
func TestAnAttentionAskWithTheLogDownStillWorksAndCountsTheFailure(t *testing.T) {
	f := askLogAttnFixture(t, func(*pgxpool.Pool) asklog.Store { return downStore(t) })
	e0 := captureErrors(asklog.KindAttention, asklog.StageWrite)
	id := f.ask(t, f.opTok, "Rotate the staging key?", "yes", "no")
	rd, hdr := form(url.Values{"option": {"no"}, "answer": {""}})
	if w := f.call(t, http.MethodPost, "/attention/asks/"+strconv.FormatInt(id, 10)+"/answer", rd, merge(f.operator(), hdr)); w.Code != http.StatusSeeOther {
		t.Fatalf("answer with the log down: %d %s", w.Code, w.Body)
	}
	flushAsks(t, f.srv)
	got, err := f.attn.Get(asUser(1), id)
	if err != nil || got.Status != attention.Answered || got.Answer != "no" {
		t.Fatalf("the ask itself: %+v %v", got, err)
	}
	if d := captureErrors(asklog.KindAttention, asklog.StageWrite) - e0; d != 2 {
		t.Fatalf("capture failures counted %v, want 2 (the open and the answer)", d)
	}
}

// ---------------------------------------------------------------------------
// ask_user_question
// ---------------------------------------------------------------------------

// askLogAskServer is askServer with the ask log on store.
func askLogAskServer(t *testing.T, store asklog.Store) (*askMirror, *Server, *httptest.Server) {
	t.Helper()
	am := newAskMirror(true)
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Notes: stubNotesStore{}, Agents: mirrorAgents{}, SessionLiveness: stubLiveness{}, Provisioner: newFakeTerminal(),
		Mirror: am, AskLog: &AskLog{Store: store}})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	s.askWatch.mu.Lock()
	s.askWatch.retry = 20 * time.Millisecond
	s.askWatch.mu.Unlock()
	t.Cleanup(s.EndStreams)
	return am, s, ts
}

// TestAnAskUserQuestionIsLoggedWhenOpenedAndWhenAnswered: the hook's
// registration (handleMirrorAskOpen) is one row — the questions, each one's
// header, options and multi-select flag, the host, session and window, the
// question store's id as the source. The question expiring and being
// restored (the open-question scan reopens it under the same id) captures it
// again and writes no second row (nor a second count); the owner's answer typed
// from the card (AnswerWindow) and confirmed by the CLI's record then replaces
// the expiry: answered, by the owner, with the chosen labels. A second
// question dismissed in the terminal ends declined.
func TestAnAskUserQuestionIsLoggedWhenOpenedAndWhenAnswered(t *testing.T) {
	pool := askLogPool(t)
	am, s, ts := askLogAskServer(t, asklog.NewPG(pool))
	n0 := testutil.ToFloat64(metrics.AsksTotal.WithLabelValues(asklog.KindAskUserQuestion))
	register(t, ts)
	region(t, s, ts, `data-ask-state="pending"`)
	open := s.Questions().Pending(1)
	if len(open) != 1 {
		t.Fatalf("%d open questions", len(open))
	}
	a := one(t, asksOf(t, s, 1), asklog.KindAskUserQuestion)
	if a.Runtime != asklog.RuntimeClaudeCode || a.Host != "ws" || a.ClaudeSession != "sess-1" || a.TmuxSession != "main" || a.Window != "@1" ||
		a.SourceRef != open[0].ID || a.Question != "Which color do you prefer?\n\nWhich pets do you have?" ||
		// The options hold each question's header, labels and flag — never its
		// text, which is the question column's and is purged with it.
		string(a.Options) != `[{"header": "Color", "options": ["Red", "Blue", "Green"], "multiSelect": false}, `+
			`{"header": "Pets", "options": ["Cat", "Dog", "Fish"], "multiSelect": true}]` ||
		a.Outcome != "" {
		t.Fatalf("the opened question's row: %+v\n%s", a, a.Options)
	}
	// The session looked gone (the question expired), then its transcript
	// showed the call open again (the open-question scan's Restore reopens it
	// under the same id): the capture runs again for the same source, and
	// writes no second row; the row reads expired until the answer replaces it.
	if n := s.Questions().Gone(1, askSourceMirror, "ws", "sess-1", "gone for a moment"); n != 1 {
		t.Fatalf("expired %d", n)
	}
	if a := one(t, asksOf(t, s, 1), asklog.KindAskUserQuestion); a.Outcome != asklog.OutcomeExpired {
		t.Fatalf("the expired question's row: %+v", a)
	}
	if _, changed, err := s.Questions().Restore(open[0].Key, gateQs(t)); err != nil || !changed {
		t.Fatalf("restore: changed=%v err=%v", changed, err)
	}
	region(t, s, ts, `data-ask-state="pending"`)
	resp, body := answerReq(t, s, ts, answerForm(gateTool, "a0", "Red", "a1", "Fish", "a1", "Dog"), "", "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Ask-State") != ui.AskStateTyping {
		t.Fatalf("answer: %d %s\n%s", resp.StatusCode, resp.Header.Get("X-Ask-State"), body)
	}
	if got := am.typedSoFar(); len(got) != 1 {
		t.Fatalf("typed %q", got)
	}
	am.feed(gateTool, gateQs(t), map[string]string{"Which color do you prefer?": "Red", "Which pets do you have?": "Dog, Fish"})
	region(t, s, ts, `data-ask-state="answered"`)
	a = one(t, asksOf(t, s, 1), asklog.KindAskUserQuestion)
	if a.Outcome != asklog.OutcomeAnswered || a.AnsweredBy != asklog.ByOwner || a.AnsweredAt.IsZero() ||
		a.Answer != "Which color do you prefer? → Red\nWhich pets do you have? → Dog, Fish" {
		t.Fatalf("the answered question's row: %+v", a)
	}
	if d := testutil.ToFloat64(metrics.AsksTotal.WithLabelValues(asklog.KindAskUserQuestion)) - n0; d != 1 {
		t.Fatalf("muster_asks_total{ask_user_question} moved %v, want 1 (the Restore is not a new ask)", d)
	}
	// A second call in the session, dismissed in the terminal.
	if code := openAsk(t, ts, "mmt_ws", "sess-1", "toolu_second", gateInput(t)); code != http.StatusCreated {
		t.Fatalf("second registration: %d", code)
	}
	am.feed("toolu_second", gateQs(t), nil)
	region(t, s, ts, `data-ask-state="declined"`)
	rows := asksOf(t, s, 1)
	if len(rows) != 2 || rows[1].Outcome != asklog.OutcomeDeclined || rows[1].AnsweredBy != asklog.ByOwner || rows[1].Answer != "" {
		t.Fatalf("the dismissed question: %+v", rows)
	}
}

// TestAnAskUserQuestionWithTheLogDownStillWorksAndCountsTheFailure: the hook
// still registers at once and the card still resolves; the failed captures
// are counted.
func TestAnAskUserQuestionWithTheLogDownStillWorksAndCountsTheFailure(t *testing.T) {
	am, s, ts := askLogAskServer(t, downStore(t))
	e0 := captureErrors(asklog.KindAskUserQuestion, asklog.StageWrite)
	register(t, ts)
	region(t, s, ts, `data-ask-state="pending"`)
	am.feed(gateTool, gateQs(t), map[string]string{"Which color do you prefer?": "Blue", "Which pets do you have?": "Cat"})
	region(t, s, ts, `data-ask-state="answered-terminal"`)
	flushAsks(t, s)
	if d := captureErrors(asklog.KindAskUserQuestion, asklog.StageWrite) - e0; d != 2 {
		t.Fatalf("capture failures counted %v, want 2", d)
	}
}

// ---------------------------------------------------------------------------
// opencode questions and permission requests
// ---------------------------------------------------------------------------

func askLogOpenCodeServer(t *testing.T, store asklog.Store) (*openCodeAskMirror, *Server, *httptest.Server) {
	t.Helper()
	m := &openCodeAskMirror{fakeMirror: &fakeMirror{online: true, interactive: true, statusReport: true, windows: openCodeAnswerable()}, answers: true}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Notes: stubNotesStore{}, Agents: mirrorAgents{}, SessionLiveness: stubLiveness{}, Provisioner: newFakeTerminal(),
		Mirror: m, Attention: emptyAttention{}, AskLog: &AskLog{Store: store}})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return m, s, ts
}

// TestAnOpenCodeAskIsLoggedWhenItArrivesAndWhenAnswered: the status hook sees
// two named asks — a form (opencode_question, its options) and a permission
// request (opencode_permission, its description and choices) — and logs each
// once, keyed by its prompt id (a second sight, or a restart's re-capture,
// writes nothing and counts nothing); a form with no
// prompt id names no ask and writes nothing. The owner's answer relayed from
// the queue resolves the permission: answered, by the owner, "reject". The
// form, left in the terminal (the window stops waiting), ends moved_on.
func TestAnOpenCodeAskIsLoggedWhenItArrivesAndWhenAnswered(t *testing.T) {
	pool := askLogPool(t)
	m, s, ts := askLogOpenCodeServer(t, asklog.NewPG(pool))
	nq := testutil.ToFloat64(metrics.AsksTotal.WithLabelValues(asklog.KindOpenCodeQuestion))
	s.AskLogMirrorChanged(1)
	s.AskLogMirrorChanged(1)
	rows := asksOf(t, s, 1)
	if len(rows) != 2 {
		t.Fatalf("%d rows, want 2: %+v", len(rows), rows)
	}
	q, p := one(t, rows, asklog.KindOpenCodeQuestion), one(t, rows, asklog.KindOpenCodePermission)
	if q.Runtime != asklog.RuntimeOpenCode || q.Host != "ws" || q.TmuxSession != "main" || q.Window != "@1" ||
		q.ClaudeSession != "opencode:"+askOpenCodeSID || q.SourceRef != "frm_1a2aaa3d1001lEte0g1lyG1pFi" || q.Question != "Proceed?" ||
		string(q.Options) != `["Yes", "No"]` || q.Outcome != "" {
		t.Fatalf("the form's row: %+v", q)
	}
	if p.Window != "@2" || p.SourceRef != "per_1a2aa6345001HPvxtQP9FD941m" || p.Question != "bash: rm -rf build" ||
		string(p.Options) != `["once", "reject"]` {
		t.Fatalf("the permission's row: %+v", p)
	}
	// A restart: nothing seen, so both asks are captured again — keyed by
	// their prompt ids, they are the same two rows, and counted once.
	s.askLog.mu.Lock()
	s.askLog.seen = map[askWinKey]askSeen{}
	s.askLog.mu.Unlock()
	s.AskLogMirrorChanged(1)
	if rows := asksOf(t, s, 1); len(rows) != 2 {
		t.Fatalf("after a restart's re-capture: %d rows, want the same 2: %+v", len(rows), rows)
	}
	if d := testutil.ToFloat64(metrics.AsksTotal.WithLabelValues(asklog.KindOpenCodeQuestion)) - nq; d != 1 {
		t.Fatalf("muster_asks_total{opencode_question} moved %v, want 1", d)
	}
	form := url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@2"}, "prompt": {"per_1a2aa6345001HPvxtQP9FD941m"}, "choice": {"reject"}}
	if resp, body := postOpenCode(t, ts, sessionCookieFor(s), ts.URL, form); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("answer: %d %s", resp.StatusCode, body)
	}
	if len(m.answered()) != 1 {
		t.Fatalf("relayed %v", m.answered())
	}
	// The form is answered in the terminal: its window stops waiting.
	m.fakeMirror.mu.Lock()
	m.fakeMirror.windows[0].Status, m.fakeMirror.windows[0].Pending = mirror.StatusBusy, nil
	m.fakeMirror.mu.Unlock()
	s.AskLogMirrorChanged(1)
	rows = asksOf(t, s, 1)
	p, q = one(t, rows, asklog.KindOpenCodePermission), one(t, rows, asklog.KindOpenCodeQuestion)
	if p.Outcome != asklog.OutcomeAnswered || p.AnsweredBy != asklog.ByOwner || p.Answer != "reject" || p.AnsweredAt.IsZero() {
		t.Fatalf("the answered permission: %+v", p)
	}
	if q.Outcome != asklog.OutcomeMovedOn || q.AnsweredBy != "" || !q.AnsweredAt.IsZero() {
		t.Fatalf("the form left in the terminal: %+v", q)
	}
}

// TestAnOpenCodeAnswerWithTheLogDownStillRelaysAndCountsTheFailure.
func TestAnOpenCodeAnswerWithTheLogDownStillRelaysAndCountsTheFailure(t *testing.T) {
	m, s, ts := askLogOpenCodeServer(t, downStore(t))
	e0 := captureErrors(asklog.KindOpenCodeQuestion, asklog.StageWrite)
	e1 := captureErrors(asklog.KindOpenCodePermission, asklog.StageWrite)
	s.AskLogMirrorChanged(1)
	form := url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@2"}, "prompt": {"per_1a2aa6345001HPvxtQP9FD941m"}, "choice": {"once"}}
	if resp, body := postOpenCode(t, ts, sessionCookieFor(s), ts.URL, form); resp.StatusCode != http.StatusSeeOther || len(m.answered()) != 1 {
		t.Fatalf("answer with the log down: %d %s, relayed %v", resp.StatusCode, body, m.answered())
	}
	flushAsks(t, s)
	// The form's open and the answer under the question kind's label, the
	// permission's open under its own.
	if d := captureErrors(asklog.KindOpenCodeQuestion, asklog.StageWrite) - e0; d != 2 {
		t.Fatalf("opencode_question capture failures counted %v, want 2", d)
	}
	if d := captureErrors(asklog.KindOpenCodePermission, asklog.StageWrite) - e1; d != 1 {
		t.Fatalf("opencode_permission capture failures counted %v, want 1", d)
	}
}

// ---------------------------------------------------------------------------
// plain_text: mirrored Claude Code windows
// ---------------------------------------------------------------------------

// plainFixture is a server whose host "ws" lists one Claude window, @1 of
// session main, whose transcript is msgs (read through the capture's hook).
type plainFixture struct {
	s    *Server
	m    *fakeMirror
	pool *pgxpool.Pool
	msgs []chiefnudge.Msg
	err  error
	cwd  string
}

func newPlainFixture(t *testing.T, store func(*pgxpool.Pool) asklog.Store, more ...func(*Extensions, *pgxpool.Pool)) *plainFixture {
	t.Helper()
	pool := askLogPool(t)
	f := &plainFixture{pool: pool, cwd: "/work/widgets",
		m: &fakeMirror{online: true, interactive: true, statusReport: true, v2: &fakeMirrorV2{},
			windows: []mirror.Window{{Session: "main", ID: "@1", Index: 1, Active: true, Claude: true, ClaudeSession: focusSID,
				Status: mirror.StatusBusy, StatusSince: 1}}}}
	f.s = New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	ext := Extensions{Notes: stubNotesStore{}, Agents: mirrorAgents{}, SessionLiveness: stubLiveness{}, Provisioner: newFakeTerminal(),
		Mirror: f.m, Attention: emptyAttention{}, AskLog: &AskLog{Store: store(pool)}}
	for _, fn := range more {
		fn(&ext, pool)
	}
	f.s.UseExtensions(ext)
	f.s.askLog.readHook = func(ctx context.Context, c chiefnudge.Session) (askRead, error) {
		if c.Host != "ws" || c.Key != focusSID || c.TmuxSession != "main" || c.Window != "@1" {
			t.Errorf("the capture read %+v, not the window's transcript", c)
		}
		f.m.mu.Lock()
		defer f.m.mu.Unlock()
		return askRead{Msgs: append([]chiefnudge.Msg(nil), f.msgs...), Cwd: f.cwd}, f.err
	}
	f.s.AskLogMirrorChanged(1) // the first sight: recorded, nothing captured
	return f
}

// status moves the window to st (its streak to since), fires the hook, and
// lets the captures it queued run — as they do in production, within
// milliseconds, before the next thing happens in the session.
func (f *plainFixture) status(t *testing.T, st string, since int64) {
	t.Helper()
	f.m.mu.Lock()
	f.m.windows[0].Status, f.m.windows[0].StatusSince = st, since
	f.m.mu.Unlock()
	f.s.AskLogMirrorChanged(1)
	flushAsks(t, f.s)
}

func (f *plainFixture) say(role, text string, at time.Time) {
	f.m.mu.Lock()
	f.msgs = append(f.msgs, chiefnudge.Msg{Role: role, Text: text, At: at})
	f.m.mu.Unlock()
}

// TestAPlainTextQuestionIsLoggedAtTurnEndAndItsReplyLinked: a Claude window
// going busy → idle with a question as its last message is one row — where,
// the working directory, the question and its normal form; the owner's reply
// (the first user message after it), read when the window turns busy again,
// resolves it: answered, by the owner, with the reply, provenance pane (no
// queued row carried it) and the reply's own time as answered_at. A turn
// ending on a statement writes nothing (the pre-filter, counted dropped); a
// question the session went past with no reply ends moved_on; one whose window
// leaves an online host ends gone. A window seen for the first time idle
// captures nothing.
func TestAPlainTextQuestionIsLoggedAtTurnEndAndItsReplyLinked(t *testing.T) {
	f := newPlainFixture(t, pgAskStore)
	kept0 := testutil.ToFloat64(metrics.AskPlainTextPrefilter.WithLabelValues("kept"))
	dropped0 := testutil.ToFloat64(metrics.AskPlainTextPrefilter.WithLabelValues("dropped"))
	none0 := answerSamples(t, asklog.KindPlainText, "none")
	base := time.Now().UTC().Truncate(time.Millisecond)
	f.say("user", "fix the flaky test", base)
	f.say("assistant", "Fixed it; the suite is green.\n\nShall I merge #12?", time.Time{})
	f.status(t, mirror.StatusIdle, 2)
	a := one(t, asksOf(t, f.s, 1), asklog.KindPlainText)
	if a.Runtime != asklog.RuntimeClaudeCode || a.Host != "ws" || a.TmuxSession != "main" || a.Window != "@1" || a.ClaudeSession != focusSID ||
		a.Cwd != "/work/widgets" || a.SourceRef != "" || a.Question != "Fixed it; the suite is green.\n\nShall I merge #12?" ||
		a.QuestionNorm != "shall i merge {pr}" || a.Outcome != "" || string(a.Options) != `[]` || a.Jev != (asklog.Jev{}) {
		t.Fatalf("the question's row: %+v", a)
	}
	reply := base.Add(30 * time.Second)
	f.say("user", "yes, merge it", reply)
	f.status(t, mirror.StatusBusy, 3)
	a = one(t, asksOf(t, f.s, 1), asklog.KindPlainText)
	if a.Outcome != asklog.OutcomeAnswered || a.AnsweredBy != asklog.ByOwner || a.Answer != "yes, merge it" ||
		a.Provenance != asklog.ProvPane || !a.AnsweredAt.Equal(reply) {
		t.Fatalf("the answered question: %+v", a)
	}

	// A statement: nothing.
	f.say("assistant", "Merged and deployed.", time.Time{})
	f.status(t, mirror.StatusIdle, 4)
	if rows := asksOf(t, f.s, 1); len(rows) != 1 {
		t.Fatalf("a turn ending on a statement wrote a row: %+v", rows)
	}
	// A question the session went past.
	f.status(t, mirror.StatusBusy, 5)
	f.say("user", "now the docs", base.Add(time.Minute))
	f.say("assistant", "Want me to update the changelog too?", time.Time{})
	f.status(t, mirror.StatusIdle, 6)
	f.say("assistant", "I went ahead and updated it.", time.Time{})
	f.status(t, mirror.StatusBusy, 7)
	rows := asksOf(t, f.s, 1)
	if len(rows) != 2 || rows[1].Outcome != asklog.OutcomeMovedOn || rows[1].Answer != "" || rows[1].AnsweredBy != "" {
		t.Fatalf("the question it went past: %+v", rows)
	}
	// A question whose window leaves the (online) host.
	f.say("assistant", "Should I close the window?", time.Time{})
	f.status(t, mirror.StatusIdle, 8)
	f.m.mu.Lock()
	f.m.windows = []mirror.Window{{Session: "other", ID: "@9", Claude: true, ClaudeSession: "sess-other", Status: mirror.StatusBusy, StatusSince: 1}}
	f.m.mu.Unlock()
	f.s.AskLogMirrorChanged(1)
	rows = asksOf(t, f.s, 1)
	if len(rows) != 3 || rows[2].Outcome != asklog.OutcomeGone {
		t.Fatalf("the question whose window is gone: %+v", rows)
	}
	if d := testutil.ToFloat64(metrics.AskPlainTextPrefilter.WithLabelValues("kept")) - kept0; d != 3 {
		t.Errorf("prefilter kept moved %v, want 3", d)
	}
	if d := testutil.ToFloat64(metrics.AskPlainTextPrefilter.WithLabelValues("dropped")) - dropped0; d != 1 {
		t.Errorf("prefilter dropped moved %v, want 1", d)
	}
	if d := answerSamples(t, asklog.KindPlainText, "none") - none0; d != 2 { // moved_on and gone
		t.Errorf("muster_ask_answer_seconds{plain_text,none} took %d samples, want 2", d)
	}
	// The window that appeared (first seen busy, then idle with a question)
	// captures on its first turn end; one first seen IDLE does not.
	f2 := newPlainFixture(t, pgAskStore)
	f2.say("assistant", "Shall I continue?", time.Time{})
	f2.m.mu.Lock()
	f2.m.windows[0].Status = mirror.StatusIdle
	f2.m.mu.Unlock()
	f2.s.askLog.seen = map[askWinKey]askSeen{} // a restart: nothing seen
	f2.s.AskLogMirrorChanged(1)
	if rows := asksOf(t, f2.s, 1); len(rows) != 0 {
		t.Fatalf("a window first seen idle was captured: %+v", rows)
	}
}

// TestAQuicklyAnsweredPlainTextQuestionIsStillLogged (the design's recon: a
// plain-text question answered before the idle threshold opens no session
// episode, which is why the log captures at every turn end): the owner's reply
// is already in the transcript when the turn end's read runs — the question
// is still logged, and answered at once from that same read. The busy that
// follows writes nothing more.
func TestAQuicklyAnsweredPlainTextQuestionIsStillLogged(t *testing.T) {
	f := newPlainFixture(t, pgAskStore)
	asked := time.Now().UTC().Truncate(time.Millisecond)
	f.say("assistant", "Shall I continue?", time.Time{})
	f.say("user", "yes", asked.Add(2*time.Second)) // replied before the read
	f.status(t, mirror.StatusIdle, 2)
	f.status(t, mirror.StatusBusy, 3)
	rows := asksOf(t, f.s, 1)
	if len(rows) != 1 {
		t.Fatalf("%d rows, want 1: %+v", len(rows), rows)
	}
	if a := rows[0]; a.Question != "Shall I continue?" || a.Outcome != asklog.OutcomeAnswered || a.Answer != "yes" ||
		a.AnsweredBy != asklog.ByOwner || a.Provenance != asklog.ProvPane {
		t.Fatalf("the quickly answered question: %+v", a)
	}
	// The same turn read again (a second busy → idle with nothing new) is not
	// a second question.
	f.status(t, mirror.StatusIdle, 4)
	if rows := asksOf(t, f.s, 1); len(rows) != 1 {
		t.Fatalf("the same turn was captured twice: %+v", rows)
	}
}

// TestAPlainTextReplyFromTheSendQueueIsMusterSend: a reply that is the body of
// a message the owner's send queue pasted into the window is provenance
// muster_send — matched by the row, not the words.
func TestAPlainTextReplyFromTheSendQueueIsMusterSend(t *testing.T) {
	f := newPlainFixture(t, pgAskStore, func(ext *Extensions, pool *pgxpool.Pool) { ext.SendQueue = sendqueue.NewPG(pool) })
	ctx := asUser(1)
	var qid int64
	if err := f.pool.QueryRow(ctx, `INSERT INTO send_queue (mirror_host, mirror_session, mirror_window, body, stepup_at) VALUES ('ws', 'main', '@1', 'go ahead', now()) RETURNING id`).Scan(&qid); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE send_queue SET state = 'sent', claimed_at = now(), finished_at = now() WHERE id = $1`, qid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM send_queue WHERE id = $1`, qid)
	})
	f.say("assistant", "Ready to push?", time.Time{})
	f.status(t, mirror.StatusIdle, 2)
	f.say("user", "go ahead", time.Now())
	f.status(t, mirror.StatusBusy, 3)
	if a := one(t, asksOf(t, f.s, 1), asklog.KindPlainText); a.Provenance != asklog.ProvMusterSend || a.Answer != "go ahead" {
		t.Fatalf("a reply the send queue pasted: %+v", a)
	}
}

// TestAPlainTextCaptureWithTheLogDownOrTheReadFailingIsCounted: the hook
// returns at once whatever the capture meets; a transcript that cannot be read
// is counted at stage read, a store that is down at stage write.
func TestAPlainTextCaptureWithTheLogDownOrTheReadFailingIsCounted(t *testing.T) {
	f := newPlainFixture(t, func(*pgxpool.Pool) asklog.Store { return downStore(t) })
	w0, r0 := captureErrors(asklog.KindPlainText, asklog.StageWrite), captureErrors(asklog.KindPlainText, asklog.StageRead)
	f.say("assistant", "Shall I proceed?", time.Time{})
	start := time.Now()
	f.status(t, mirror.StatusIdle, 2)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("the status hook took %s with the log down", d)
	}
	flushAsks(t, f.s)
	if d := captureErrors(asklog.KindPlainText, asklog.StageWrite) - w0; d != 1 {
		t.Fatalf("write failures counted %v, want 1", d)
	}
	f.err = context.DeadlineExceeded
	f.status(t, mirror.StatusBusy, 3)
	f.status(t, mirror.StatusIdle, 4)
	flushAsks(t, f.s)
	if d := captureErrors(asklog.KindPlainText, asklog.StageRead) - r0; d != 1 {
		t.Fatalf("read failures counted %v, want 1", d)
	}
}

// TestAPlainTextQuestionIsClassifiedAtCaptureOnlyWhenAsked: with
// MUSTER_ASK_LOG_CLASSIFY on, a kept question carries the streak's stall
// classification when the sweep already has one (no new call); off, the jev
// columns stay empty even then.
func TestAPlainTextQuestionIsClassifiedAtCaptureOnlyWhenAsked(t *testing.T) {
	for _, on := range []bool{true, false} {
		f := newPlainFixture(t, pgAskStore, func(ext *Extensions, _ *pgxpool.Pool) { ext.AskLog.JevAtCapture = on })
		w := f.m.windows[0]
		w.Status, w.StatusSince = mirror.StatusIdle, 2
		f.s.nudgeRetry.keepJev(nudgeRetryKey(1, askLogSession("ws", w)), chiefnudge.Result{Label: "needs_input",
			Scores: map[string]float64{"needs_input": 0.8, "asks_permission": 0.15, "neither": 0.05}, AskScore: 0.95, Model: "jev-test"}, f.s.now())
		f.say("assistant", "Shall I continue?", time.Time{})
		f.status(t, mirror.StatusIdle, 2)
		a := one(t, asksOf(t, f.s, 1), asklog.KindPlainText)
		want := asklog.Jev{}
		if on {
			want = asklog.Jev{Label: "needs_input", NeedsInput: 0.8, AsksPermission: 0.15, Neither: 0.05, AskScore: 0.95, Model: "jev-test"}
		}
		if a.Jev != want {
			t.Fatalf("classify=%v: the row's jev is %+v, want %+v", on, a.Jev, want)
		}
	}
}

// ---------------------------------------------------------------------------
// plain_text: dispatched agents
// ---------------------------------------------------------------------------

// TestAnAgentsQuestionIsLoggedAtTurnEndAndTheOwnersNextMessageLinked: a turn
// persisted (persistTurn, every turn's one door) whose last text asks is one
// row — the agent, its conversation, the question; the next turn's end finds
// the owner's typed message after it and resolves the row: answered, by the
// owner, muster_send (typed into muster's chat). A question followed by
// another turn with no user message ends moved_on. The turn's messages are
// stored whatever the log does.
func TestAnAgentsQuestionIsLoggedAtTurnEndAndTheOwnersNextMessageLinked(t *testing.T) {
	f := askLogAttnFixture(t, pgAskStore)
	ctx := asUser(1)
	store := agents.NewPG(f.pool)
	sess, err := store.LatestOrCreateSession(ctx, f.op.ID, f.op.Name)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.persistTurn(ctx, f.op, sess, []agents.TurnPart{{Kind: "tool_call", Content: "{}"}, {Kind: "text", Content: "Tests pass.\n\nShould I open the PR?"}})
	a := one(t, asksOf(t, f.srv, 1), asklog.KindPlainText)
	if a.Runtime != asklog.RuntimeAgent || a.AgentID != f.op.ID || a.ChatSessionID != sess.ID || a.Host != "" ||
		a.Question != "Tests pass.\n\nShould I open the PR?" || a.QuestionNorm != "should i open the pr" || a.Outcome != "" {
		t.Fatalf("the agent's question: %+v", a)
	}
	if _, err := store.AddChatMessage(ctx, agents.ChatMessage{AgentID: f.op.ID, SessionID: sess.ID, Role: "user", Content: "yes, open it", TypedBy: 1}); err != nil {
		t.Fatal(err)
	}
	f.srv.persistTurn(ctx, f.op, sess, []agents.TurnPart{{Kind: "text", Content: "Opened."}})
	a = one(t, asksOf(t, f.srv, 1), asklog.KindPlainText)
	if a.Outcome != asklog.OutcomeAnswered || a.AnsweredBy != asklog.ByOwner || a.Answer != "yes, open it" || a.Provenance != asklog.ProvMusterSend || a.AnsweredAt.IsZero() {
		t.Fatalf("the answered agent question: %+v", a)
	}
	f.srv.persistTurn(ctx, f.op, sess, []agents.TurnPart{{Kind: "text", Content: "Want me to tag a release?"}})
	f.srv.persistTurn(ctx, f.op, sess, []agents.TurnPart{{Kind: "text", Content: "Tagged it anyway."}})
	rows := asksOf(t, f.srv, 1)
	if len(rows) != 2 || rows[1].Outcome != asklog.OutcomeMovedOn {
		t.Fatalf("the question the agent went past: %+v", rows)
	}
	msgs, err := store.ListRecentChatMessages(ctx, sess.ID, 0)
	if err != nil || len(msgs) != 6 {
		t.Fatalf("the conversation holds %d messages (%v), want 6", len(msgs), err)
	}
}

// TestAnAgentTurnWithTheLogDownIsStillStored: the turn's messages are stored
// and the failed capture counted.
func TestAnAgentTurnWithTheLogDownIsStillStored(t *testing.T) {
	f := askLogAttnFixture(t, func(*pgxpool.Pool) asklog.Store { return downStore(t) })
	ctx := asUser(1)
	store := agents.NewPG(f.pool)
	sess, err := store.LatestOrCreateSession(ctx, f.op.ID, f.op.Name)
	if err != nil {
		t.Fatal(err)
	}
	w0 := captureErrors(asklog.KindPlainText, asklog.StageWrite)
	f.srv.persistTurn(ctx, f.op, sess, []agents.TurnPart{{Kind: "text", Content: "Shall I keep going?"}})
	flushAsks(t, f.srv)
	if d := captureErrors(asklog.KindPlainText, asklog.StageWrite) - w0; d != 1 {
		t.Fatalf("write failures counted %v, want 1", d)
	}
	if msgs, err := store.ListRecentChatMessages(ctx, sess.ID, 0); err != nil || len(msgs) != 1 || msgs[0].Content != "Shall I keep going?" {
		t.Fatalf("the turn was not stored: %+v %v", msgs, err)
	}
}

// ---------------------------------------------------------------------------
// The retention sweep, through the server.
// ---------------------------------------------------------------------------

// TestTheServersSweepPurgesWithTheConfiguredTTL: the server's sweep clears text
// older than ITS text TTL (here 48 h), not the default 30 days.
func TestTheServersSweepPurgesWithTheConfiguredTTL(t *testing.T) {
	pool := askLogPool(t)
	st := asklog.NewPG(pool)
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{AskLog: &AskLog{Store: st, TextTTL: 48 * time.Hour}})
	ctx := asUser(1)
	old, _, err := st.Open(ctx, asklog.Ask{Kind: asklog.KindPlainText, Runtime: asklog.RuntimeAgent, Question: "Old?", AskedAt: time.Now().Add(-49 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	young, _, err := st.Open(ctx, asklog.Ask{Kind: asklog.KindPlainText, Runtime: asklog.RuntimeAgent, Question: "Young?", AskedAt: time.Now().Add(-47 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	s.askLogPurge(context.Background()) // the sweep's one pass (RunAskLogSweep runs it on its ticker)
	if a, _, _ := st.Get(ctx, old); a.Question != "" || a.TextPurgedAt.IsZero() {
		t.Fatalf("the 49-hour-old ask kept its text: %+v", a)
	}
	if a, _, _ := st.Get(ctx, young); a.Question != "Young?" {
		t.Fatalf("the 47-hour-old ask lost its text: %+v", a)
	}
}

var _ = askq.Pending

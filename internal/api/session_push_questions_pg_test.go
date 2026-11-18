package api

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
)

// EVERY PROMPT'S PUSH WAITS ITS GRACE — AN ANSWERABLE ASK'S TOO (operator
// decision after #463; session_push.go, insideGrace). Measured on the live
// fleet after #463: every push the operator felt
// for a waiting window came from an AskUserQuestion, sent the instant the
// question registered and answered at the terminal 13–30 s later. The card
// shows in muster at once; only the phone waits, and it buzzes only if the
// question is still open once the grace has passed.
//
// 🔴 Each regression test below uses only names that exist at the base too
// (the fixture, the question store, the hub hook, the pass, the clock, a
// restarted server, the Asks page) and spells the grace as a literal, so it
// is shown RED against the base by running it there: the base pushes a held
// question from the waiting diff at once.

// pastWaitingGrace moves s's clock past a waiting window's grace (60 s,
// insideGrace) and runs the attention sweep's re-diff — what pushes a wait the
// waiting diff held — then waits for the pushes. For a test of the diff's
// OTHER rules (its seeding, its re-push window, a push's text or tap route):
// since the follow-up to #463 a new wait's push is held for the grace, so such a test
// lets the grace pass where it used to read the push at once.
func pastWaitingGrace(s *Server) {
	s.WaitForPushes()
	prev := s.now
	s.now = func() time.Time { return prev().Add(EpisodeWaitingGrace + time.Second) }
	s.recheckOfflineHosts()
	s.WaitForPushes()
}

// toolQ is the fixture's AskUserQuestion's tool_use id.
const toolQ = "toolu_grace_q1"

// questionKey is the fixture window's AskUserQuestion's key in s's store.
func questionKey() askq.Key {
	return askq.Key{Owner: authz.OperatorUserID, Source: askSourceMirror, Scope: "ws", Session: nudgeSID, ToolUseID: toolQ}
}

var graceQuestion = []askq.Question{{Question: "Which database?", Options: []askq.Option{{Label: "postgres"}, {Label: "sqlite"}}}}

// asks has the hub see the fixture's host first with its window idle (as a
// running muster has, so nothing is seeded as a host's first sight), then the
// window wait on a held AskUserQuestion — registered by the hook, as Claude
// Code's PreToolUse does — and nudges the hub.
func (f *stateFixture) asks(t *testing.T) {
	t.Helper()
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	f.status(mirror.StatusWaiting, 2*time.Second)
	if _, err := f.srv.Questions().Open(questionKey(), graceQuestion, askq.NewID()); err != nil {
		t.Fatal(err)
	}
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
}

// rebuilt is a restarted process's first sight of the fixture's window: the
// hub reports it waiting (a nudge), then the question is read back open from
// its transcript (askq Restore — the scan's own call) and the hub nudges again.
func (f *stateFixture) rebuilt(t *testing.T) {
	t.Helper()
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	if _, _, err := f.srv.Questions().Restore(questionKey(), graceQuestion); err != nil {
		t.Fatal(err)
	}
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
}

// answered is the owner answering the question at the terminal: the hook
// resolves it, and Claude Code works on.
func (f *stateFixture) answered(t *testing.T) {
	t.Helper()
	if _, ok := f.srv.Questions().Resolve(authz.OperatorUserID, askSourceMirror, "ws", nudgeSID, toolQ, map[string]string{"Which database?": "postgres"}); !ok {
		t.Fatal("the question was not open to resolve")
	}
	f.statusSince(mirror.StatusBusy, nudgeSince+600_000, 0)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
}

// TestAQuestionAnsweredInsideItsGraceNeverPushes (regression): the window
// asks an AskUserQuestion. Its card is on the Asks list at once; no push from
// the hook, nor from a pass 30 s in; the owner answers it at the terminal
// 40 s in. The grace passing, and more passes and nudges, push nothing.
func TestAQuestionAnsweredInsideItsGraceNeverPushes(t *testing.T) {
	f := newStateFixture(t, false)
	f.asks(t)
	if got := f.buzzes(); len(got) != 0 {
		t.Fatalf("pushed when the question registered: %v", got)
	}
	body := f.page(t, "/attention")
	if !strings.Contains(body, "Which database?") || !strings.Contains(body, `data-kind="question"`) {
		t.Fatalf("the question's card is not on the Asks list at once:\n%s", body)
	}
	if len(f.episodes(t)) != 1 || strings.Contains(body, `data-testid="attention-episodes"`) {
		t.Fatalf("want its stop held as one episode with NO second \"Stalled / waiting\" row beside its card:\n%s", body)
	}
	f.advance(30 * time.Second)
	f.statePass(t)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	if got := f.buzzes(); len(got) != 0 {
		t.Fatalf("pushed inside the grace: %v", got)
	}
	f.advance(10 * time.Second)
	f.answered(t)
	for i := 0; i < 4; i++ {
		f.advance(30 * time.Second)
		f.statePass(t)
		f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	}
	if got := f.buzzes(); len(got) != 0 {
		t.Fatalf("pushes: %v, want none — the question was answered inside its grace", got)
	}
}

// TestAQuestionStillOpenAfterItsGracePushesOnceWithItsText (regression): the
// window asks and nobody answers. Nothing is pushed inside the grace; the first
// pass past 60 s pushes it ONCE, under the stop's episode tag, as the ASK'S OWN
// notification — its question and options in full (the operator's "Full text
// always" for asks) — and later passes, nudges and a restart add nothing.
func TestAQuestionStillOpenAfterItsGracePushesOnceWithItsText(t *testing.T) {
	f := newStateFixture(t, false)
	f.asks(t)
	f.advance(30 * time.Second)
	f.statePass(t)
	if got := f.buzzes(); len(got) != 0 {
		t.Fatalf("pushed inside the grace: %v", got)
	}
	f.advance(31 * time.Second)
	f.statePass(t)
	e := f.oneEpisode(t)
	want := "attention attention-episode-" + strconv.FormatInt(e.ID, 10)
	if got := f.buzzes(); len(got) != 1 || got[0] != want {
		t.Fatalf("pushes past the grace: %v, want [%s]", got, want)
	}
	p := f.payloads()[0]
	if p.Title != "Gold:1 · tmux main:1 @ ws is asking" || p.Body != "Which database?\nOptions: postgres · sqlite" {
		t.Fatalf("payload: title %q body %q, want the ask's own", p.Title, p.Body)
	}
	if p.Data["url"] != "/attention/open/episode-"+strconv.FormatInt(e.ID, 10) {
		t.Fatalf("url %q", p.Data["url"])
	}
	for i := 0; i < 3; i++ {
		f.advance(30 * time.Second)
		f.statePass(t)
		f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	}
	f.restart()
	f.rebuilt(t)
	f.statePass(t)
	if got := f.buzzes(); len(got) != 1 {
		t.Fatalf("pushes after more passes and a restart: %v", got)
	}
}

// TestARestartNeverRePushesARebuiltQuestionAlreadyPushed (regression, the
// live re-push measured after #463): a question waited out its grace and was pushed;
// muster restarts and reads it back open from the transcript. The fresh
// process pushes nothing for it — not on the first nudge, not once the
// question is rebuilt, not on later passes. Red at the base: its first nudge
// marked the host seen, so the rebuilt question was pushed as new.
func TestARestartNeverRePushesARebuiltQuestionAlreadyPushed(t *testing.T) {
	f := newStateFixture(t, false)
	f.asks(t)
	f.advance(61 * time.Second)
	f.statePass(t)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	if got := f.buzzes(); len(got) != 1 {
		t.Fatalf("pushes before the restart: %v, want the question's one", got)
	}
	f.restart()
	f.advance(5 * time.Second)
	f.rebuilt(t)
	for i := 0; i < 3; i++ {
		f.statePass(t)
		f.advance(30 * time.Second)
		f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	}
	if got := f.buzzes(); len(got) != 1 {
		t.Fatalf("pushes after the restart: %v, want still the one from before it", got)
	}
}

// TestARestartPushesARebuiltQuestionNeverPushedOncePastItsGrace (regression):
// muster restarts 20 s into a question's grace — it was never pushed. The
// fresh process rebuilds it from the transcript, and pushes it exactly once
// once the grace (timed from the stop's opening, before the restart) has
// passed — never twice, across the hook and two passes. Red at the base: the
// question was pushed the moment it registered, before the restart.
func TestARestartPushesARebuiltQuestionNeverPushedOncePastItsGrace(t *testing.T) {
	f := newStateFixture(t, false)
	f.asks(t)
	f.advance(20 * time.Second)
	f.statePass(t)
	if got := f.buzzes(); len(got) != 0 {
		t.Fatalf("pushes before the restart: %v, want none (inside its grace)", got)
	}
	f.restart()
	f.advance(45 * time.Second)
	f.rebuilt(t)
	f.statePass(t)
	f.advance(30 * time.Second)
	f.statePass(t)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	e := f.oneEpisode(t)
	if got := f.buzzes(); len(got) != 1 || got[0] != "attention attention-episode-"+strconv.FormatInt(e.ID, 10) {
		t.Fatalf("pushes after the restart: %v, want the question's one push", got)
	}
}

// openCodeQuestionWindow puts the fixture's window on an opencode form muster
// cannot relay: a question that names no prompt id (openCodeItem: why =
// openCodeTerminalText).
func (f *stateFixture) openCodeQuestionWindow() {
	f.mirror.mu.Lock()
	defer f.mirror.mu.Unlock()
	f.mirror.windows[0] = mirror.Window{Session: "main", ID: "@1", Index: 1, Label: "Gold:1", Active: true, Runtime: mirror.RuntimeOpenCode,
		AgentSession: askOpenCodeSID, Status: mirror.StatusWaiting, StatusSince: nudgeSince,
		Pending: &mirror.Pending{Kind: "question", Tool: "question", Summary: "Which database? (and 1 more question)"}}
	f.mirror.agentState = &mirror.AgentState{Runtime: mirror.RuntimeOpenCode, Status: mirror.StatusWaiting, Since: nudgeSince,
		Observed: f.clock.Add(-2 * time.Second)}
}

// TestAnOpenCodeQuestionMusterCannotRelayIsAPlainWait (regression, finding
// 2): an opencode question muster cannot answer from here is NOT an
// answerable ask (answerableItem) — until the follow-up to #463 its question Kind alone
// made it one, so it was pushed at once by the waiting diff with no episode.
// It is a plain wait: held for its grace, then pushed once as its episode's
// text-free "needs you".
func TestAnOpenCodeQuestionMusterCannotRelayIsAPlainWait(t *testing.T) {
	f := newStateFixture(t, false)
	f.openCodeQuestionWindow()
	octx := f.srv.attentionPrincipal(t.Context(), authz.OperatorUserID)
	items := f.srv.waitingSessionsIn(octx, f.mirror.Hosts(octx))
	if len(items) != 1 || items[0].Kind != attentionKindQuestion || items[0].openCode == nil || items[0].openCode.why == "" {
		t.Fatalf("fixture: want one unrelayable opencode question, got %+v", items)
	}
	if answerableItem(items[0]) {
		t.Fatal("an opencode question muster cannot relay counts as answerable")
	}
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	f.advance(30 * time.Second)
	f.statePass(t)
	if got := f.buzzes(); len(got) != 0 {
		t.Fatalf("pushed inside the grace: %v", got)
	}
	f.advance(31 * time.Second)
	f.statePass(t)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	e := f.oneEpisode(t)
	if got := f.buzzes(); len(got) != 1 || got[0] != "attention attention-episode-"+strconv.FormatInt(e.ID, 10) {
		t.Fatalf("pushes past the grace: %v, want its episode's one", got)
	}
	if p := f.payloads()[0]; !strings.HasSuffix(p.Title, " needs you") || !strings.HasPrefix(p.Body, "Needs you") {
		t.Fatalf("payload: title %q body %q, want the plain wait's text-free one", p.Title, p.Body)
	}
}

// keyless makes the fixture's window one session state cannot key (a host
// that sends no Claude session id): it stays in the waiting diff.
func (f *stateFixture) keyless() {
	f.mirror.mu.Lock()
	defer f.mirror.mu.Unlock()
	f.mirror.windows[0].ClaudeSession = ""
}

// TestAKeylessWaitingWindowsPushWaitsItsGraceToo (regression, the first
// brief): a window session state cannot key keeps the waiting diff's
// summary-only push — under the SAME grace (insideGrace). Answered inside it:
// never pushed. Still waiting past it: pushed once, by the attention sweep's
// re-diff, with no further nudge. Red at the base: pushed the moment it
// started waiting.
func TestAKeylessWaitingWindowsPushWaitsItsGraceToo(t *testing.T) {
	t.Run("answered inside its grace", func(t *testing.T) {
		f := newStateFixture(t, false)
		f.keyless()
		f.seenIdleThenWaiting()
		f.advance(20 * time.Second)
		f.srv.AttentionMirrorChanged(authz.OperatorUserID)
		f.status(mirror.StatusBusy, 0)
		f.srv.AttentionMirrorChanged(authz.OperatorUserID)
		f.advance(2 * time.Minute)
		f.srv.recheckOfflineHosts()
		f.srv.AttentionMirrorChanged(authz.OperatorUserID)
		if got := f.buzzes(); len(got) != 0 {
			t.Fatalf("pushes: %v, want none", got)
		}
	})
	t.Run("still waiting past its grace", func(t *testing.T) {
		f := newStateFixture(t, false)
		f.keyless()
		f.seenIdleThenWaiting()
		if got := f.buzzes(); len(got) != 0 {
			t.Fatalf("pushed when it started waiting: %v", got)
		}
		f.advance(30 * time.Second)
		f.srv.recheckOfflineHosts()
		if got := f.buzzes(); len(got) != 0 {
			t.Fatalf("pushed inside the grace: %v", got)
		}
		f.advance(31 * time.Second)
		f.srv.recheckOfflineHosts() // the attention sweep: no hub nudge
		got := f.buzzes()
		if len(got) != 1 || !strings.HasPrefix(got[0], "attention attention-session-") {
			t.Fatalf("pushes past the grace: %v, want the diff's one", got)
		}
		for i := 0; i < 3; i++ {
			f.advance(30 * time.Second)
			f.srv.recheckOfflineHosts()
			f.srv.AttentionMirrorChanged(authz.OperatorUserID)
		}
		if got := f.buzzes(); len(got) != 1 {
			t.Fatalf("pushes after more sweeps: %v", got)
		}
		if list := f.episodes(t); len(list) != 0 {
			t.Fatalf("episodes: %+v", list)
		}
	})
}

// TestAHostsFirstSightHoldsOnlyAWaitStillInsideItsGrace (regression): a
// restart or a reconnect — the diff's first sight of a host — seeds a wait
// that is older than its grace (by its host's own Since: it was due, and
// pushed, before) and never pushes it; one that started 20 s before is still
// being held, so it is held for the rest of its grace and pushed once then.
// Red at the base: neither was ever pushed (every first-sight wait was
// seeded), so a wait caught by a restart mid-hold was silent.
func TestAHostsFirstSightHoldsOnlyAWaitStillInsideItsGrace(t *testing.T) {
	for _, c := range []struct {
		name   string
		age    time.Duration
		pushes int
	}{{"waiting 20 s", 20 * time.Second, 1}, {"waiting 5 min", 5 * time.Minute, 0}} {
		t.Run(c.name, func(t *testing.T) {
			f := newStateFixture(t, false)
			f.keyless()
			f.statusSince(mirror.StatusWaiting, f.srv.now().Add(-c.age).UnixMilli(), 0)
			f.srv.AttentionMirrorChanged(authz.OperatorUserID) // the host's first sight
			if got := f.buzzes(); len(got) != 0 {
				t.Fatalf("pushed on the host's first sight: %v", got)
			}
			f.advance(30 * time.Second)
			f.srv.recheckOfflineHosts()
			f.advance(30 * time.Second)
			f.srv.recheckOfflineHosts()
			f.srv.AttentionMirrorChanged(authz.OperatorUserID)
			if got := f.buzzes(); len(got) != c.pushes {
				t.Fatalf("pushes: %v, want %d", got, c.pushes)
			}
		})
	}
}

// TestSessionStateOffHoldsAQuestionsPushToo: with MUSTER_SESSION_STATE off
// there are no episodes, and every waiting push is the diff's — a held
// AskUserQuestion's included, under the same grace: none inside it, one past
// it while the question is still open.
func TestSessionStateOffHoldsAQuestionsPushToo(t *testing.T) {
	f := newStateFixture(t, false)
	f.srv.ext.Episodes = nil
	f.asks(t)
	f.advance(30 * time.Second)
	f.srv.recheckOfflineHosts()
	if got := f.buzzes(); len(got) != 0 {
		t.Fatalf("pushed inside the grace: %v", got)
	}
	f.advance(31 * time.Second)
	f.srv.recheckOfflineHosts()
	got := f.buzzes()
	if len(got) != 1 || !strings.HasPrefix(got[0], "attention attention-session-") {
		t.Fatalf("pushes past the grace: %v, want the question's one", got)
	}
	if p := f.payloads()[0]; p.Body != "Which database?\nOptions: postgres · sqlite" {
		t.Fatalf("body %q", p.Body)
	}
}

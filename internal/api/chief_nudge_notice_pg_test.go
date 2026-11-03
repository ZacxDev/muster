package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/mirror"
)

// A LIVE CHIEF NUDGE IS A NOTICE (docs/chief-nudges.md#live-mode): a pointer at
// the session, with no answer, no relay, not counted toward the open-ask cap,
// and opencode panes judged like Claude Code ones.
//
// 🔴 THIS FILE USES ONLY WHAT EXISTED BEFORE NOTICES (no attention.KindNotice,
// no noticeItem), so it compiles at the commit before them and each test can be
// shown RED there and GREEN here — the regression matrix.

// noticeCard is the Asks page's card for ask id, "" when the page has none.
func noticeCard(page string, id int64) string {
	i := strings.Index(page, `<li id="ask-`+strconv.FormatInt(id, 10)+`"`)
	if i < 0 {
		return ""
	}
	li := page[i:]
	return li[:strings.Index(li, "</li>")]
}

// TestChiefNudgeAFullAskCapDoesNotSuppressALiveNudge: the owner already has
// MaxOpenPerOwner open asks. A live ask decision still reaches the owner (its
// notice is not counted toward the cap, nor refused at it), and the cap still
// holds for a question. RED before notices: the decision was recorded
// suppressed:ask_cap (6 such in 3 days of live data).
func TestChiefNudgeAFullAskCapDoesNotSuppressALiveNudge(t *testing.T) {
	f := newNudgeFixture(t, true)
	op := asUser(authz.OperatorUserID)
	for i := range attention.MaxOpenPerOwner {
		if _, err := f.attn.Create(op, attention.New{Asker: attention.Asker{System: true, Label: "muster"},
			Question: "an open question " + strconv.Itoa(i)}); err != nil {
			t.Fatalf("filling the cap: %v", err)
		}
	}
	f.gw.set(nudgeAskReply)
	if n := f.pass(t); n != 1 {
		t.Fatalf("decided %d", n)
	}
	d := f.one(t)
	if d.Outcome != chiefnudge.OutcomeAsked || d.AskID == 0 {
		t.Fatalf("at a full ask cap the nudge was %q (%s), want raised", d.Outcome, d.Detail)
	}
	if _, err := f.attn.Create(op, attention.New{Asker: attention.Asker{System: true, Label: "muster"}, Question: "one too many"}); err == nil {
		t.Fatal("the cap no longer holds for a question (the control)")
	}
}

// TestChiefNudgeALiveNudgeIsNeverAnsweredNorRelayed: the live decision's item
// on the Asks page names the window's host and exact tmux target and has no
// answer box; an answer posted to it anyway is refused (409), and nothing is
// queued, pasted or relayed into the session. RED before notices: the item was
// an ask with an answer box, the answer was accepted (303) and relayed.
func TestChiefNudgeALiveNudgeIsNeverAnsweredNorRelayed(t *testing.T) {
	f := newNudgeFixture(t, true)
	f.gw.set(nudgeAskReply)
	if n := f.pass(t); n != 1 {
		t.Fatalf("decided %d", n)
	}
	d := f.one(t)
	rd, hdr := form(url.Values{"answer": {"main"}})
	if w := f.call(t, http.MethodPost, "/attention/asks/"+strconv.FormatInt(d.AskID, 10)+"/answer", rd, merge(f.operator(), hdr)); w.Code != http.StatusConflict {
		t.Fatalf("an answer to the nudge: %d, want 409", w.Code)
	}
	card := noticeCard(f.page(t, "/attention"), d.AskID)
	if card == "" || !strings.Contains(card, "ws · main:1") || strings.Contains(card, "data-attention-answer") || strings.Contains(card, "<textarea") {
		t.Fatalf("the nudge's card (want the host and tmux target, no answer box):\n%s", card)
	}
	f.pass(t)
	f.tick(t)
	f.pass(t)
	if got := f.one(t); got.Outcome != chiefnudge.OutcomeAsked || got.RelayText != "" || got.SendQueueID != 0 {
		t.Fatalf("after the answer: %+v", got)
	}
	if q, p := f.queuedRows(t), f.pastes(); q != 0 || len(p) != 0 {
		t.Fatalf("the nudge put something into the session: %d queued, pastes %+v", q, p)
	}
}

// TestTheSummaryCarriesANudgeNoticesTmuxTarget: the attention summary's item
// for a live nudge carries "target": "<tmux session>:<window index>" and its
// host — what `muster nudge next` jumps to. RED before notices: no target.
func TestTheSummaryCarriesANudgeNoticesTmuxTarget(t *testing.T) {
	f := newNudgeFixture(t, true)
	f.gw.set(nudgeAskReply)
	f.pass(t)
	d := f.one(t)
	items, err := f.srv.attentionItems(f.srv.attentionPrincipal(context.Background(), authz.OperatorUserID))
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Key != "ask-"+strconv.FormatInt(d.AskID, 10) {
			continue
		}
		raw, _ := json.Marshal(attentionSummaryItemOf(it, "", time.Now()))
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		if m["target"] != "main:1" || m["host"] != "ws" {
			t.Fatalf("the summary item %s, want target main:1 on host ws", raw)
		}
		return
	}
	t.Fatalf("the nudge is not on the queue: %+v", items)
}

// ocSID is an opencode session id, as opencode names them; ocLine its owner
// prompt's place (opencode's renderer uses the message's creation time).
const (
	ocSID  = "ses_0fixture1a2b3c4d5e6f7a8b9c"
	ocLine = int64(1_793_000_000_000)
)

// opencodeFixture is the nudge fixture whose host shows ONE opencode pane
// (tmux oc:2, window @2) idle in streak nudgeSince, with its transcript served
// through the rendered view exactly as a muster-mirror's opencode viewer sends
// it: the stream named by the BARE opencode id, the owner's prompt with no time
// (opencode's renderer sets none), the session's question last, then the
// prompt index. AgentState names no conversation for it, as the hub's does for
// any runtime but Claude Code.
func opencodeFixture(t *testing.T) *nudgeFixture {
	t.Helper()
	return opencodeFixtureAt(t, 0)
}

// opencodeFixtureAt is opencodeFixture whose owner prompt carries promptAt
// (Unix ms; 0 = no time, as opencode's renderer sends today), and returns
// it with the prompt's line.
func opencodeFixtureAt(t *testing.T, promptAt int64) *nudgeFixture {
	t.Helper()
	f := newNudgeFixture(t, true)
	f.srv.nudgeReadHook = nil
	f.mirror.windows = []mirror.Window{{Session: "oc", ID: "@2", Index: 2, Active: true, Runtime: mirror.RuntimeOpenCode,
		AgentSession: ocSID, Status: mirror.StatusIdle, StatusSince: nudgeSince}}
	f.mirror.mu.Lock()
	f.mirror.agentState = &mirror.AgentState{Runtime: mirror.RuntimeOpenCode, Status: mirror.StatusIdle, Since: nudgeSince,
		Observed: time.Now().Add(-20 * time.Minute)}
	f.mirror.mu.Unlock()
	line := ocLine
	f.mirror.v2.mu.Lock()
	f.mirror.v2.batches = []mirror.Msg{
		{Gen: ocSID, Reset: true, Events: []cctranscript.Event{
			{Kind: cctranscript.KindPrompt, Text: "push the branch when the tests pass", Line: line, PromptAt: promptAt},
			{Kind: cctranscript.KindText, Text: "The tests pass. Shall I push the opencode branch?"}}},
		{Gen: ocSID, PromptIndex: true, Prompts: []cctranscript.PromptRef{{Line: line}}},
	}
	f.mirror.v2.mu.Unlock()
	return f
}

// TestChiefNudgeAnOpencodePaneIsANudgeCandidate: an idle opencode pane is put
// to the chief like a Claude Code window — under its runtime-qualified key,
// with its transcript read through the rendered view (the chief is shown the
// pane's own last message) — and, live, its decision raises the notice for
// tmux oc:2 rather than a nudge: muster cannot type into an opencode pane.
// RED before: the sweep listed only Claude Code windows, so nothing was
// decided.
func TestChiefNudgeAnOpencodePaneIsANudgeCandidate(t *testing.T) {
	f := opencodeFixture(t)
	if n := f.pass(t); n != 1 {
		t.Fatalf("decided %d, want the opencode pane", n)
	}
	d := f.one(t)
	if d.Kind != chiefnudge.KindMirror || d.Key != "opencode:"+ocSID || d.Host != "ws" || d.TmuxSession != "oc" || d.Window != "@2" ||
		d.StreakKey != "since:"+strconv.FormatInt(nudgeSince, 10) {
		t.Fatalf("decision %+v", d)
	}
	f.gw.mu.Lock()
	prompt := strings.Join(f.gw.prompt, "\n")
	f.gw.mu.Unlock()
	if !strings.Contains(prompt, "Shall I push the opencode branch?") {
		t.Fatalf("the chief was not shown the pane's last message:\n%s", prompt)
	}
	if d.Decision != chiefnudge.DecisionAsk || d.Outcome != chiefnudge.OutcomeAsked || d.AskID == 0 || d.SendQueueID != 0 {
		t.Fatalf("live, an opencode pane gets the notice, never a queued nudge: %+v", d)
	}
	if q := f.queuedRows(t); q != 0 {
		t.Fatalf("%d message(s) queued for a view-only pane", q)
	}
	if card := noticeCard(f.page(t, "/attention"), d.AskID); !strings.Contains(card, "ws · oc:2") {
		t.Fatalf("the notice does not name tmux oc:2 on ws:\n%s", card)
	}
}

// TestChiefNudgeAnOpencodePaneIsNeverNudgedEvenWithTimedPrompts: were the
// host to send its prompts' times — so the owner's message could authorise a
// nudge, and the chief proposes one quoting it exactly — the pane still gets
// the notice, never a queued nudge: muster cannot type into it (the view_only
// guard). Its prompt time is what makes the guard reachable: with untimed
// prompts the no-times rule turns the nudge into a notice first.
func TestChiefNudgeAnOpencodePaneIsNeverNudgedEvenWithTimedPrompts(t *testing.T) {
	f := opencodeFixtureAt(t, time.Now().Add(-15*time.Minute).UnixMilli())
	f.gw.set(nudgeReply("line:"+strconv.FormatInt(ocLine, 10), "push the branch when the tests pass", "push the branch"))
	if n := f.pass(t); n != 1 {
		t.Fatalf("decided %d", n)
	}
	d := f.one(t)
	if d.Decision != chiefnudge.DecisionAsk || d.Outcome != chiefnudge.OutcomeAsked || d.SendQueueID != 0 || !strings.HasPrefix(d.Reason, "view_only:") {
		t.Fatalf("an opencode pane with a quotable owner message: %+v", d)
	}
	if q := f.queuedRows(t); q != 0 {
		t.Fatalf("%d message(s) queued for a view-only pane", q)
	}
}

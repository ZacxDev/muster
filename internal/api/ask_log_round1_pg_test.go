package api

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/asklog"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/mirror"
)

// TestAReplyMusterQueuedIsNotTheOwnersAnswer (round 1, Y1): a plain-text
// question answered by a chief nudge is answered_by chief, by a routed message
// answered_by other — never owner, so a nudge never counts as the owner's
// approval. The same for a dispatched agent's nudge.
func TestAReplyMusterQueuedIsNotTheOwnersAnswer(t *testing.T) {
	for _, c := range []struct{ reply, by, prov string }{
		{chiefnudge.NudgeLabel + " you asked whether to continue: yes", asklog.ByChief, asklog.ProvNudge},
		{RoutedPrefacePrefix + "the review bot: go ahead", asklog.ByOther, asklog.ProvRoute},
	} {
		f := newPlainFixture(t, pgAskStore)
		f.say("assistant", "Shall I continue?", time.Time{})
		f.status(t, mirror.StatusIdle, 2)
		f.say("user", c.reply, time.Now())
		f.status(t, mirror.StatusBusy, 3)
		if a := one(t, asksOf(t, f.s, 1), asklog.KindPlainText); a.AnsweredBy != c.by || a.Provenance != c.prov || a.Outcome != asklog.OutcomeAnswered {
			t.Fatalf("a reply %q: %+v, want answered_by %s", c.reply, a, c.by)
		}
	}
	f := askLogAttnFixture(t, pgAskStore)
	ctx := asUser(1)
	store := agents.NewPG(f.pool)
	sess, err := store.LatestOrCreateSession(ctx, f.op.ID, f.op.Name)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.persistTurn(ctx, f.op, sess, []agents.TurnPart{{Kind: "text", Content: "Shall I keep going?"}})
	if _, err := store.AddChatMessage(ctx, agents.ChatMessage{AgentID: f.op.ID, SessionID: sess.ID, Role: "user", Content: chiefnudge.NudgeLabel + " keep going"}); err != nil {
		t.Fatal(err)
	}
	f.srv.persistTurn(ctx, f.op, sess, []agents.TurnPart{{Kind: "text", Content: "Kept going."}})
	if a := one(t, asksOf(t, f.srv, 1), asklog.KindPlainText); a.AnsweredBy != asklog.ByChief || a.Provenance != asklog.ProvNudge {
		t.Fatalf("an agent's question answered by a nudge: %+v", a)
	}
}

// TestAnAgentsQuickReplyBeforeTheCaptureRunsIsStillTheAnswer (round 1, Y2):
// the owner's reply is stored before the turn's queued capture runs (the
// recorder is held); the reply is still linked as the answer at the next
// turn's end, not read as already past (moved_on).
func TestAnAgentsQuickReplyBeforeTheCaptureRunsIsStillTheAnswer(t *testing.T) {
	f := askLogAttnFixture(t, pgAskStore)
	ctx := asUser(1)
	store := agents.NewPG(f.pool)
	sess, err := store.LatestOrCreateSession(ctx, f.op.ID, f.op.Name)
	if err != nil {
		t.Fatal(err)
	}
	hold := make(chan struct{})
	f.srv.askLogDo(1, asklog.KindPlainText, func(context.Context) error { <-hold; return nil })
	f.srv.persistTurn(ctx, f.op, sess, []agents.TurnPart{{Kind: "text", Content: "Should I open the PR?"}})
	if _, err := store.AddChatMessage(ctx, agents.ChatMessage{AgentID: f.op.ID, SessionID: sess.ID, Role: "user", Content: "yes", TypedBy: 1}); err != nil {
		t.Fatal(err)
	}
	close(hold) // the capture runs only now, the reply already stored
	flushAsks(t, f.srv)
	f.srv.persistTurn(ctx, f.op, sess, []agents.TurnPart{{Kind: "text", Content: "Opened."}})
	a := one(t, asksOf(t, f.srv, 1), asklog.KindPlainText)
	if a.Outcome != asklog.OutcomeAnswered || a.Answer != "yes" || a.AnsweredBy != asklog.ByOwner {
		t.Fatalf("a reply stored before the capture ran: %+v", a)
	}
}

// TestANewSessionInTheWindowEndsTheOldOnesQuestion (round 1, green a): the
// window now shows another Claude session, so the old one's open question
// ends gone rather than staying open for ever.
func TestANewSessionInTheWindowEndsTheOldOnesQuestion(t *testing.T) {
	f := newPlainFixture(t, pgAskStore)
	f.say("assistant", "Shall I continue?", time.Time{})
	f.status(t, mirror.StatusIdle, 2)
	f.m.mu.Lock()
	f.m.windows[0].ClaudeSession, f.m.windows[0].Status = "1c2d3e4f-5a6b-4c7d-8e9f-0a1b2c3d4e5f", mirror.StatusBusy
	f.m.mu.Unlock()
	f.s.AskLogMirrorChanged(1)
	rows := asksOf(t, f.s, 1)
	if len(rows) != 1 || rows[0].Outcome != asklog.OutcomeGone {
		t.Fatalf("the old session's question after a new session took the window: %+v", rows)
	}
}

// TestTheSweepClosesWhatARestartLeftOpen (round 1, green a): the server's
// sweep, at its start, closes the open plain-text row a previous process
// opened (outcome unknown) and leaves an open AskUserQuestion row.
func TestTheSweepClosesWhatARestartLeftOpen(t *testing.T) {
	pool := askLogPool(t)
	st := asklog.NewPG(pool)
	ctx := asUser(1)
	before := time.Now().Add(-time.Hour)
	plain, _, err := st.Open(ctx, asklog.Ask{Kind: asklog.KindPlainText, Runtime: asklog.RuntimeClaudeCode, Question: "Shall I?", AskedAt: before})
	if err != nil {
		t.Fatal(err)
	}
	auq, _, err := st.Open(ctx, asklog.Ask{Kind: asklog.KindAskUserQuestion, Runtime: asklog.RuntimeClaudeCode, SourceRef: "q-restart", Question: "Which?", AskedAt: before})
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{AskLog: &AskLog{Store: st}})
	sctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.RunAskLogSweep(sctx, time.Hour); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if a, _, _ := st.Get(ctx, plain); a.Outcome == asklog.OutcomeUnknown {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("the sweep did not close the plain-text row a restart left open")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if a, _, _ := st.Get(ctx, auq); a.Outcome != "" {
		t.Fatalf("the open AskUserQuestion row was closed: %+v", a)
	}
}

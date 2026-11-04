package api

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/plugins"
)

// SESSION STATE STEP 3b-1 end to end on a real Postgres
// (docs/design/session-state.md, "Covering words"; chief_chat_quotes.go): a
// nudge's quote may come from a Chief-chat turn the owner TYPED (web or phone,
// through the owner's signed-in chat doors), never from a relayed answer, a
// session question, a plugin message, a task event or an agent's or muster's
// own turn; only within the window; a partial read gives nothing; a quote that
// does not verify is a notify; and the nudge is live only for an owner on
// MUSTER_CHIEF_NUDGE_CHIEF_CHAT_QUOTES_LIVE_OWNERS too, read again at the
// paste and at an agent's turn. A same-session quote and session state off
// are unchanged.
//
// 🔴 These tests use only names that exist at the base too, except through
// chief_chat_quotes_gate_test.go's helpers — so each REGRESSION test is
// shown red against the base by running this file there with those stubbed.
// The quote source and the notify's reason are literals for the same reason.

const (
	// chiefChatTyped is what the owner types into the Chief chat; the chief
	// quotes chiefChatQuote from it. Neither is in the session's own script.
	chiefChatTyped = "ship the release branch once CI is green, no need to ask"
	chiefChatQuote = "ship the release branch once CI is green"
	chiefChatCover = "merge the pull request"
)

// chiefChatReply is the chief's JSON for a nudge quoting quote from Chief-chat
// row id, naming source.
func chiefChatReply(id int64, source, quote string) string {
	return `{"decision":"nudge","reason":"the owner told the chief to ship it","quote_source":"` + source +
		`","quote_id":"chief:` + strconv.FormatInt(id, 10) + `","quote":"` + quote + `","covered_action":"` + chiefChatCover + `"}`
}

// chiefChatRow is the id of the newest user row on the fixture's chief whose
// text is text (system scope: whoever wrote it).
func (f *nudgeFixture) chiefChatRow(t *testing.T, text string) int64 {
	t.Helper()
	var id int64
	if err := f.pool.QueryRow(authz.System(context.Background()),
		`SELECT id FROM chat_messages WHERE agent_id = $1 AND role = 'user' AND content LIKE '%' || $2 || '%' ORDER BY id DESC LIMIT 1`,
		f.boss.ID, text).Scan(&id); err != nil {
		t.Fatalf("the chief's row holding %q: %v", text, err)
	}
	return id
}

// chiefSession is the chief's current conversation.
func (f *nudgeFixture) chiefSession(t *testing.T) agents.ChatSession {
	t.Helper()
	s, err := agents.NewPG(f.pool).LatestOrCreateSession(asUser(authz.OperatorUserID), f.boss.ID, f.boss.Name)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// chiefIdle waits for every turn on the chief to end: the stall check needs
// the chief's turn gate.
func (f *nudgeFixture) chiefIdle(t *testing.T) {
	t.Helper()
	waitFor(t, func() bool { return !f.srv.turns.busy(f.boss.ID) })
}

// typeToChief is the owner TYPING text into the Chief chat's composer: the
// signed-in browser door (POST /agents/{name}/send), the turn waited for.
func (f *nudgeFixture) typeToChief(t *testing.T, text string) int64 {
	t.Helper()
	rd, hdr := form(url.Values{"text": {text}, "nonce": {"chief-chat-typed-" + strconv.FormatInt(time.Now().UnixNano(), 36)}})
	if w := f.call(t, http.MethodPost, "/agents/"+f.boss.Name+"/send", rd, merge(f.operator(), hdr)); w.Code != http.StatusNoContent {
		t.Fatalf("the owner's send to the chief: %d %s", w.Code, w.Body)
	}
	f.chiefIdle(t)
	return f.chiefChatRow(t, text)
}

// chiefChatFixture is the state fixture with the classifier saying the stop
// asked NOTHING (so only the old list and the Chief-chat list gate a nudge),
// the owner live on the old list when live.
func chiefChatFixture(t *testing.T, live bool) *stateFixture {
	t.Helper()
	f, _ := newNeedsYouFixture(t, jevNothing, live)
	return f
}

// lastPrompt is the newest stall check's prompt.
func (f *nudgeFixture) lastPrompt(t *testing.T) string {
	t.Helper()
	f.gw.mu.Lock()
	defer f.gw.mu.Unlock()
	if len(f.gw.prompt) == 0 {
		t.Fatal("no stall check was made")
	}
	return f.gw.prompt[len(f.gw.prompt)-1]
}

// TestAnOwnerTypedChiefChatTurnIsACoveringQuote (regression): a line the owner
// TYPED into the Chief chat's composer is shown to the chief with its
// "chief:<id>" and, quoted with quote_source chief_chat, passes the quote
// guard: a nudge recorded with its source. Shadow, because the owner is on the
// old list only: would_send, nothing queued or pasted, the stop pushed. RED at
// the base: the Chief chat was never read, so the quote failed
// (suppressed:quote_unverified).
func TestAnOwnerTypedChiefChatTurnIsACoveringQuote(t *testing.T) {
	f := chiefChatFixture(t, true)
	id := f.typeToChief(t, chiefChatTyped)
	f.gw.set(chiefChatReply(id, "chief_chat", chiefChatQuote))
	queued0 := f.queuedRows(t)
	f.judge(t)
	d := f.one(t)
	if d.Decision != chiefnudge.DecisionNudge || d.Outcome != chiefnudge.OutcomeWouldSend || d.Mode != chiefnudge.ModeShadow ||
		d.QuoteText != chiefChatQuote || d.QuoteID != "chief:"+strconv.FormatInt(id, 10) || quoteSourceOf(d) != "chief_chat" {
		t.Fatalf("decision %+v\nwant a shadow would_send quoting chief:%d from chief_chat", d, id)
	}
	if !strings.HasPrefix(d.Message, chiefnudge.NudgeLabel) || !strings.Contains(d.Message, `told the chief`) ||
		!strings.Contains(d.Message, chiefChatQuote) {
		t.Fatalf("the nudge's text %q", d.Message)
	}
	if p := f.lastPrompt(t); !strings.Contains(p, "[id chief:"+strconv.FormatInt(id, 10)) || !strings.Contains(p, chiefChatTyped) {
		t.Fatalf("the prompt does not show the typed Chief-chat turn:\n%s", p)
	}
	if q := f.queuedRows(t) - queued0; q != 0 {
		t.Fatalf("send_queue rows written: %d", q)
	}
	if got := f.episodePushes(); len(got) != 1 {
		t.Fatalf("pushes: %v, want 1 (shadow never quiets a stop)", got)
	}
	f.tick(t)
	if p := f.pastes(); len(p) != 0 {
		t.Fatalf("a shadow Chief-chat nudge was pasted: %+v", p)
	}
}

// TestANonOwnerChiefChatTurnNeverCounts (decision 13): a turn on the chief
// that the owner did not TYPE into muster's chat — a session question over the
// machine door, a relayed answer, a plugin message, a task event, an agent's
// or muster's own turn (a nudge-style turn, a route hand-back) — is never
// shown to the chief as the owner's, and a nudge quoting it meets the quote
// guard: suppressed:quote_unverified with its source (chief_chat) on the row,
// nothing queued, nothing pasted, the stop pushed. The owner is on EVERY list,
// so a row that wrongly counted would be queued live. Each row is written by
// its real writer.
//
// ⚠ The "never counts" half is an INVARIANT the base keeps too (the base
// counts no Chief-chat turn at all, and records the same outcome); what is new
// — and red at the base — is only the source on the row. Its teeth are the
// mutants (a writer made to set a typer turns its case red).
func TestANonOwnerChiefChatTurnNeverCounts(t *testing.T) {
	ctx := asUser(authz.OperatorUserID)
	for _, c := range []struct {
		kind  string
		write func(t *testing.T, f *stateFixture, text string)
	}{
		{"a session question over the machine door", func(t *testing.T, f *stateFixture, text string) {
			w := f.call(t, http.MethodPost, "/api/agents/"+f.boss.Name+"/messages", strings.NewReader(`{"message":"`+text+`"}`),
				map[string]string{"Authorization": "Bearer " + connTestHookToken, "Content-Type": "application/json"})
			if w.Code != http.StatusOK {
				t.Fatalf("the machine door: %d %s", w.Code, w.Body)
			}
		}},
		{"a relayed answer", func(t *testing.T, f *stateFixture, text string) {
			if _, err := f.srv.runDeliveredTurn(ctx, f.boss, f.chiefSession(t), text, nil); err != nil {
				t.Fatal(err)
			}
		}},
		{"a plugin message", func(t *testing.T, f *stateFixture, text string) {
			_, p, err := f.plugins.Create(ctx, "chief-chat-inbox-"+strconv.FormatInt(time.Now().UnixNano(), 36), "Synthetic inbox", "synthetic-inbox")
			if err != nil {
				t.Fatal(err)
			}
			d, err := plugins.NewDeliverer(plugins.DelivererConfig{Store: f.plugins, Agents: agents.NewPG(f.pool), Gateway: &threadTurns{},
				Turn: (&threadTurns{}).turn, Logger: log.New(io.Discard, "", 0)})
			if err != nil {
				t.Fatal(err)
			}
			if err := d.Handed(ctx, p, f.boss, plugins.Message{Sender: "Ada <ada@example.invalid>", Subject: "release",
				Body: text}, "routed by thread"); err != nil {
				t.Fatal(err)
			}
		}},
		{"a task event", func(t *testing.T, f *stateFixture, text string) {
			f.srv.taskForwardTurn = nil // stored in the chief's conversation, no turn
			boss := f.boss
			boss.KickedOff = true
			if _, err := f.srv.forwardToTaskAgent(ctx, boss, text); err != nil {
				t.Fatal(err)
			}
		}},
		{"an agent's or muster's own turn", func(t *testing.T, f *stateFixture, text string) {
			if !f.srv.turns.enter(f.boss.ID) {
				t.Fatal("the chief's turn gate is held")
			}
			f.srv.startAgentTurn(ctx, f.boss, f.chiefSession(t), text, notTyped)
			waitFor(t, func() bool { _, turns := f.gw.seen(); return len(turns) > 0 })
			f.chiefIdle(t)
		}},
		{"a route hand-back", func(t *testing.T, f *stateFixture, text string) {
			if _, err := f.srv.runUserTurn(ctx, f.boss, f.chiefSession(t), text, nil); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.kind, func(t *testing.T) {
			f := chiefChatFixture(t, true)
			needsYouLive(f.nudgeFixture, authz.OperatorUserID)
			chiefChatQuotesLive(f.nudgeFixture, authz.OperatorUserID)
			c.write(t, f, chiefChatTyped)
			id := f.chiefChatRow(t, chiefChatTyped)
			f.gw.set(chiefChatReply(id, "chief_chat", chiefChatQuote))
			queued0 := f.queuedRows(t)
			f.judge(t)
			d := f.one(t)
			if d.Decision != chiefnudge.DecisionNudge || d.Outcome != chiefnudge.Suppressed(chiefnudge.GuardQuote) ||
				quoteSourceOf(d) != "chief_chat" {
				t.Fatalf("decision %+v\nwant suppressed:quote_unverified from chief_chat (the quote is not the owner's typing)", d)
			}
			if p := f.lastPrompt(t); strings.Contains(p, "chief:"+strconv.FormatInt(id, 10)) {
				t.Fatalf("the prompt shows a %s as the owner's:\n%s", c.kind, p)
			}
			if q := f.queuedRows(t) - queued0; q != 0 {
				t.Fatalf("send_queue rows written: %d", q)
			}
			if got := f.episodePushes(); len(got) != 1 {
				t.Fatalf("pushes: %v, want 1", got)
			}
		})
	}
}

// TestAChiefChatTurnOutsideTheWindowDoesNotCount (regression): a typed turn
// older than OwnerWindow (6h) is not shown and its quote is suppressed; the
// control, the same turn 5 hours old, is a nudge. RED at the base: the control
// was suppressed (the Chief chat was never read). The 7-hour case alone is an
// INVARIANT GUARD (the base records the same suppression).
func TestAChiefChatTurnOutsideTheWindowDoesNotCount(t *testing.T) {
	for _, c := range []struct {
		name    string
		age     string
		outcome string
	}{
		{"5 hours old: counts", "5 hours", chiefnudge.OutcomeWouldSend},
		{"7 hours old: does not", "7 hours", chiefnudge.Suppressed(chiefnudge.GuardQuote)},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := chiefChatFixture(t, true)
			id := f.typeToChief(t, chiefChatTyped)
			if _, err := f.pool.Exec(authz.System(context.Background()),
				`UPDATE chat_messages SET created_at = now() - $2::interval WHERE id = $1`, id, c.age); err != nil {
				t.Fatal(err)
			}
			f.gw.set(chiefChatReply(id, "chief_chat", chiefChatQuote))
			f.judge(t)
			if d := f.one(t); d.Outcome != c.outcome {
				t.Fatalf("decision %+v, want outcome %s", d, c.outcome)
			}
		})
	}
}

// TestAFailedChiefChatReadGivesNothing (regression): when the Chief-chat read
// fails, the chief is shown none of it and a quote of a typed turn meets the
// quote guard (suppressed:quote_unverified, source chief_chat, the stop
// pushed). The control: the same quote with the read working is a nudge. RED
// at the base: the control was suppressed (the Chief chat was never read).
func TestAFailedChiefChatReadGivesNothing(t *testing.T) {
	for _, c := range []struct {
		name    string
		fail    bool
		outcome string
		pushes  int
	}{
		{"the read works: counts", false, chiefnudge.OutcomeWouldSend, 1},
		{"the read fails: nothing counts", true, chiefnudge.Suppressed(chiefnudge.GuardQuote), 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := chiefChatFixture(t, true)
			id := f.typeToChief(t, chiefChatTyped)
			if c.fail {
				failChiefChatRead(t, f.nudgeFixture)
			}
			f.gw.set(chiefChatReply(id, "chief_chat", chiefChatQuote))
			f.judge(t)
			d := f.one(t)
			if d.Outcome != c.outcome || quoteSourceOf(d) != "chief_chat" {
				t.Fatalf("decision %+v, want outcome %s from chief_chat", d, c.outcome)
			}
			if shown := strings.Contains(f.lastPrompt(t), chiefChatTyped); shown == c.fail {
				t.Fatalf("the typed turn shown: %v", shown)
			}
			// A failed read is no Chief chat at all, not an empty one: the
			// guard says no Chief-chat quote could pass here.
			if c.fail && d.Detail != "the quote names the Chief chat, which no nudge here may quote" {
				t.Fatalf("detail %q", d.Detail)
			}
			if got := f.episodePushes(); len(got) != c.pushes {
				t.Fatalf("pushes: %v, want %d", got, c.pushes)
			}
		})
	}
}

// TestOnlyTheNewestTypedChiefChatTurnsAreReadAndShown (new in 3b-1, round 0
// D1; its SQL names typed_by, so it cannot run at the base): the Chief chat is
// read newest first, MaxOwnerMessages (20) turns, which are all the chief is
// shown and all a quote is checked against. With 20 typed turns after it, the
// quoted turn is the 21st newest: not shown, and its quote is suppressed; with
// 19 after it, it is the 20th: shown, and the nudge passes.
func TestOnlyTheNewestTypedChiefChatTurnsAreReadAndShown(t *testing.T) {
	for _, c := range []struct {
		name    string
		after   int
		outcome string
	}{
		{"the 20th newest: read", 19, chiefnudge.OutcomeWouldSend},
		{"the 21st newest: not read", 20, chiefnudge.Suppressed(chiefnudge.GuardQuote)},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := chiefChatFixture(t, true)
			id := f.typeToChief(t, chiefChatTyped)
			if _, err := f.pool.Exec(authz.System(context.Background()), `INSERT INTO chat_messages (agent_id, owner_id, session_id, role, content, typed_by)
				SELECT $1, $2, $3, 'user', 'a later typed line ' || g, $2 FROM generate_series(1, $4) g`,
				f.boss.ID, authz.OperatorUserID, f.chiefSession(t).ID, c.after); err != nil {
				t.Fatal(err)
			}
			f.gw.set(chiefChatReply(id, "chief_chat", chiefChatQuote))
			f.judge(t)
			if d := f.one(t); d.Outcome != c.outcome {
				t.Fatalf("decision %+v, want outcome %s", d, c.outcome)
			}
			if shown := strings.Contains(f.lastPrompt(t), chiefChatTyped); shown != (c.outcome == chiefnudge.OutcomeWouldSend) {
				t.Fatalf("the quoted turn shown: %v", shown)
			}
		})
	}
}

// TestAnUnverifiableChiefChatQuoteIsSuppressedAndPushes: a Chief-chat quote
// that is not, verbatim, in the typed turn it names — or that names a source
// muster does not know — meets the one quote guard: suppressed:quote_unverified
// with the source it named on the row ("" for one muster does not know),
// nothing queued, nothing pasted, and the STOP PUSHED (read off the push sink
// after the state pass), with the owner on every list.
//
// Regression for the first case (RED at the base: no source recorded). The
// second is an INVARIANT GUARD: the base records the same row.
func TestAnUnverifiableChiefChatQuoteIsSuppressedAndPushes(t *testing.T) {
	for _, c := range []struct {
		name, source, quote, recorded string
	}{
		{"words the owner did not type", "chief_chat", "ship the release branch right now", "chief_chat"},
		{"a source muster does not know", "another_pane", chiefChatQuote, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := chiefChatFixture(t, true)
			chiefChatQuotesLive(f.nudgeFixture, authz.OperatorUserID)
			id := f.typeToChief(t, chiefChatTyped)
			f.gw.set(chiefChatReply(id, c.source, c.quote))
			queued0 := f.queuedRows(t)
			f.judge(t)
			d := f.one(t)
			if d.Decision != chiefnudge.DecisionNudge || d.Outcome != chiefnudge.Suppressed(chiefnudge.GuardQuote) ||
				quoteSourceOf(d) != c.recorded {
				t.Fatalf("decision %+v\nwant suppressed:quote_unverified, source %q", d, c.recorded)
			}
			if q := f.queuedRows(t) - queued0; q != 0 {
				t.Fatalf("send_queue rows written: %d", q)
			}
			if got := f.episodePushes(); len(got) != 1 {
				t.Fatalf("pushes: %v, want 1 (a suppressed nudge never quiets a stop)", got)
			}
			f.tick(t)
			if p := f.pastes(); len(p) != 0 {
				t.Fatalf("pasted: %+v", p)
			}
		})
	}
}

// TestAnUnverifiedChiefChatQuoteIsNeverTheUntimedEscalation (round 0 D2): when
// the session's owner messages arrived with no time (an older muster-mirror),
// a SESSION quote that fails is still the untimed escalation (a notify, as
// before 3b-1 — the control), but a Chief-chat quote that fails meets the
// quote guard: suppressed:quote_unverified with its source, the stop pushed.
// New in 3b-1 (no base equivalent for the Chief-chat half).
func TestAnUnverifiedChiefChatQuoteIsNeverTheUntimedEscalation(t *testing.T) {
	for _, c := range []struct {
		name, decision, outcome, source string
		chief                           bool
	}{
		{"a session quote: the untimed escalation", "notify", "notified", "", false},
		{"a Chief-chat quote: the quote guard", chiefnudge.DecisionNudge, chiefnudge.Suppressed(chiefnudge.GuardQuote), "chief_chat", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := chiefChatFixture(t, true)
			f.mu.Lock()
			f.script[0].At = time.Time{} // the owner's session message carries no time
			f.mu.Unlock()
			id := f.typeToChief(t, chiefChatTyped)
			if c.chief {
				f.gw.set(chiefChatReply(id, "chief_chat", "ship the release branch right now"))
			}
			f.judge(t)
			d := f.one(t)
			if d.Decision != c.decision || d.Outcome != c.outcome || (c.chief && quoteSourceOf(d) != c.source) {
				t.Fatalf("decision %+v\nwant %s / %s", d, c.decision, c.outcome)
			}
			if got := f.episodePushes(); len(got) != 1 {
				t.Fatalf("pushes: %v, want 1", got)
			}
		})
	}
}

// TestTheChiefChatQuoteGate (regression; shadow first): a nudge quoting the
// owner's typed Chief chat is queued for real only for an owner ALSO on
// MUSTER_CHIEF_NUDGE_CHIEF_CHAT_QUOTES_LIVE_OWNERS, on top of the gates it was
// under; then its paste is sent and the stop is quiet. Off that list it is
// shadow — would_send, never pasted, the stop pushed — and so it is for an
// owner on that list but not the old one. For a NEEDS-YOU stop it needs the
// needs-you list too. RED at the base: no Chief-chat quote passed at all.
func TestTheChiefChatQuoteGate(t *testing.T) {
	for _, c := range []struct {
		name                      string
		p                         [3]float64
		live, needsYou, chiefChat bool
		mode, outcome             string
		pushes, pastes            int
	}{
		{"old list and Chief-chat list", jevNothing, true, false, true, chiefnudge.ModeLive, chiefnudge.OutcomeQueued, 0, 1},
		{"old list alone", jevNothing, true, false, false, chiefnudge.ModeShadow, chiefnudge.OutcomeWouldSend, 1, 0},
		{"Chief-chat list alone", jevNothing, false, false, true, chiefnudge.ModeShadow, chiefnudge.OutcomeWouldSend, 1, 0},
		{"needs-you: all three lists", jevAsked, true, true, true, chiefnudge.ModeLive, chiefnudge.OutcomeQueued, 0, 1},
		{"needs-you: no needs-you list", jevAsked, true, false, true, chiefnudge.ModeShadow, chiefnudge.OutcomeWouldSend, 1, 0},
		{"needs-you: no Chief-chat list", jevAsked, true, true, false, chiefnudge.ModeShadow, chiefnudge.OutcomeWouldSend, 1, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, _ := newNeedsYouFixture(t, c.p, c.live)
			if c.needsYou {
				needsYouLive(f.nudgeFixture, authz.OperatorUserID)
			}
			if c.chiefChat {
				chiefChatQuotesLive(f.nudgeFixture, authz.OperatorUserID)
			}
			id := f.typeToChief(t, chiefChatTyped)
			f.gw.set(chiefChatReply(id, "chief_chat", chiefChatQuote))
			f.judge(t)
			d := f.one(t)
			if d.Decision != chiefnudge.DecisionNudge || d.Mode != c.mode || d.Outcome != c.outcome || quoteSourceOf(d) != "chief_chat" {
				t.Fatalf("decision %+v\nwant mode %s, outcome %s, from chief_chat", d, c.mode, c.outcome)
			}
			f.tick(t)
			if p := f.pastes(); len(p) != c.pastes {
				t.Fatalf("pastes: %+v, want %d", p, c.pastes)
			}
			f.pass(t) // the reconcile: a pasted row is sent
			f.statePass(t)
			if got := f.episodePushes(); len(got) != c.pushes {
				t.Fatalf("pushes: %v, want %d", got, c.pushes)
			}
		})
	}
}

// TestTheChiefChatGateIsReadAgainAtThePaste (regression): a live Chief-chat
// nudge queued while its owner was on the list is NOT pasted once the owner is
// taken off it (still on the old list): the paste is refused, the decision
// fails with the kill switch's words, and the stop pushes. RED at the base:
// nothing was queued.
func TestTheChiefChatGateIsReadAgainAtThePaste(t *testing.T) {
	f := chiefChatFixture(t, true)
	chiefChatQuotesLive(f.nudgeFixture, authz.OperatorUserID)
	id := f.typeToChief(t, chiefChatTyped)
	f.gw.set(chiefChatReply(id, "chief_chat", chiefChatQuote))
	f.judge(t)
	if d := f.one(t); d.Outcome != chiefnudge.OutcomeQueued {
		t.Fatalf("precondition: %+v", d)
	}
	chiefChatQuotesLive(f.nudgeFixture) // off the Chief-chat list, on the old one still
	f.tick(t)
	if p := f.pastes(); len(p) != 0 {
		t.Fatalf("pasted after the owner left the Chief-chat list: %+v", p)
	}
	f.pass(t)
	if d := f.one(t); d.Outcome != chiefnudge.OutcomeFailed || d.Detail != SendQueueNudgeOff {
		t.Fatalf("decision: %+v", d)
	}
	f.statePass(t)
	if got := f.episodePushes(); len(got) != 1 {
		t.Fatalf("pushes: %v", got)
	}
}

// TestTheChiefChatGateHoldsAtAnAgentsTurn (regression): a live Chief-chat nudge
// to an agent whose turn could not start is retried by the reconcile. With the
// owner on the Chief-chat list the retry SENDS it (the positive control: the
// path is reached and ends in a turn); taken off the list before the retry, it
// fails with the kill switch's words and no turn starts. RED at the base: the
// retry sent it (no such gate).
func TestTheChiefChatGateHoldsAtAnAgentsTurn(t *testing.T) {
	seed := func(t *testing.T, f *nudgeFixture) chiefnudge.Decision {
		t.Helper()
		d, _ := queuedAgentNudge(t, f, jevNothing, 0)
		// Re-record it as a Chief-chat nudge (the seed knows no source).
		if _, err := f.pool.Exec(authz.System(context.Background()), `DELETE FROM chief_nudges WHERE id = $1`, d.ID); err != nil {
			t.Fatal(err)
		}
		d.ID = 0
		d, ok, err := f.nudges.Insert(asUser(authz.OperatorUserID), withQuoteSource(d, "chief_chat"), "")
		if err != nil || !ok {
			t.Fatalf("re-seed: %v %v", ok, err)
		}
		return d
	}
	t.Run("on the Chief-chat list: sent", func(t *testing.T) {
		f := newNudgeFixture(t, true)
		chiefChatQuotesLive(f, authz.OperatorUserID)
		d := seed(t, f)
		f.pass(t)
		if got := f.nudgeByID(t, d.ID); got.Outcome != chiefnudge.OutcomeSent {
			t.Fatalf("decision %+v", got)
		}
		waitFor(t, func() bool { _, turns := f.gw.seen(); return len(turns) > 0 })
	})
	t.Run("taken off the Chief-chat list: not sent", func(t *testing.T) {
		f := newNudgeFixture(t, true)
		chiefChatQuotesLive(f, authz.OperatorUserID)
		d := seed(t, f)
		chiefChatQuotesLive(f) // off before the retry; still on the old list
		f.pass(t)
		if got := f.nudgeByID(t, d.ID); got.Outcome != chiefnudge.OutcomeFailed || got.Detail != SendQueueNudgeOff {
			t.Fatalf("a Chief-chat agent nudge off its list: %+v", got)
		}
		time.Sleep(200 * time.Millisecond)
		if _, turns := f.gw.seen(); len(turns) != 0 {
			t.Fatalf("turns %v", turns)
		}
	})
}

// TestASameSessionQuoteIsUnaffectedByTheChiefChatGate — an INVARIANT GUARD
// (green at the base too): with a typed Chief-chat turn present and the owner
// on the old list but NOT the Chief-chat list, a nudge quoting the session's
// own owner message is live exactly as before: queued, pasted, the stop quiet.
func TestASameSessionQuoteIsUnaffectedByTheChiefChatGate(t *testing.T) {
	f := chiefChatFixture(t, true)
	f.typeToChief(t, chiefChatTyped)
	queued0 := f.queuedRows(t)
	f.judge(t) // the fixture's reply quotes the session's own message
	d := f.one(t)
	if d.Decision != chiefnudge.DecisionNudge || d.Mode != chiefnudge.ModeLive || d.Outcome != chiefnudge.OutcomeQueued ||
		d.QuoteID != nudgeOwnerID {
		t.Fatalf("decision %+v", d)
	}
	if q := f.queuedRows(t) - queued0; q != 1 {
		t.Fatalf("send_queue rows written: %d, want 1", q)
	}
	f.tick(t)
	if p := f.pastes(); len(p) != 1 {
		t.Fatalf("pastes: %+v", p)
	}
	if got := f.episodePushes(); len(got) != 0 {
		t.Fatalf("pushes: %v", got)
	}
}

// TestSessionStateOffNeverReadsTheChiefChat — an INVARIANT GUARD (green at the
// base too): with MUSTER_SESSION_STATE unset, a typed Chief-chat turn is never
// shown to the chief, and a nudge quoting it is refused by the quote guard
// exactly as before (suppressed:quote_unverified), for an owner on every list.
func TestSessionStateOffNeverReadsTheChiefChat(t *testing.T) {
	f := newNudgeFixture(t, true)
	needsYouLive(f, authz.OperatorUserID)
	chiefChatQuotesLive(f, authz.OperatorUserID)
	id := f.typeToChief(t, chiefChatTyped)
	f.gw.set(chiefChatReply(id, "chief_chat", chiefChatQuote))
	queued0 := f.queuedRows(t)
	f.pass(t)
	d := f.one(t)
	if d.Decision != chiefnudge.DecisionNudge || d.Outcome != chiefnudge.Suppressed(chiefnudge.GuardQuote) || d.EpisodeID != 0 {
		t.Fatalf("decision %+v", d)
	}
	if p := f.lastPrompt(t); strings.Contains(p, chiefChatTyped) || strings.Contains(p, "chief_chat") {
		t.Fatalf("the state-off prompt shows the Chief chat:\n%s", p)
	}
	if q := f.queuedRows(t) - queued0; q != 0 {
		t.Fatalf("send_queue rows written: %d", q)
	}
}

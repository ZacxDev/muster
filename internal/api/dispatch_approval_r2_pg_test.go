package api

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefusage"
	"github.com/ZacxDev/muster/internal/plugins"
)

// #602 round 2: the approval card names the RUNNING turn's trigger, read from
// the turn itself — in the production order, where a plugin delivery and an
// attention answer store their row only AFTER the turn — and the caps count
// turns by the turn's own serial.

// approvalTurnGateway is the chief's runtime: each turn runs during (the chief's
// CLI calling muster's tools mid-turn) and answers "ok". CLIActivity is the
// fixture's (ownerTurnFor reads it).
type approvalTurnGateway struct {
	*podLanes
	mu     sync.Mutex
	during func()
}

func (g *approvalTurnGateway) ChatWithTools(ctx context.Context, a agents.Agent, sessionKey, instructions, message string, tools []agents.ToolDef,
	dispatch agents.ToolDispatch, emit agents.StreamEmit) (string, error) {
	g.mu.Lock()
	fn := g.during
	g.mu.Unlock()
	if fn != nil {
		fn()
	}
	return "ok", nil
}

func (g *approvalTurnGateway) Chat(ctx context.Context, a agents.Agent, sessionKey, message string, emit func(string)) (string, error) {
	return "ok", nil
}

// withTurns wires the fixture's chief to a approvalTurnGateway and returns it.
func (f *hostFixture) withTurns(t *testing.T) *approvalTurnGateway {
	g := &approvalTurnGateway{podLanes: f.pod}
	f.srv.ext.Gateway = g
	// A turn leaves rows naming the owner (its usage and outcome records) that
	// keep the fixture's cleanup from deleting them, so they are made a member
	// again: the database's other tests count its admins.
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `UPDATE users SET role = 'member' WHERE id = $1`, f.owner)
	})
	return g
}

// turn runs one chief turn started by trigger through chatTurn — the one
// function every door's turn passes — with during as the chief's work in it.
// Nothing is stored for it first, as for a delivery.
func (f *hostFixture) turn(t *testing.T, g *approvalTurnGateway, trigger chiefusage.Trigger, during func()) {
	t.Helper()
	sess, err := agents.NewPG(f.pool).LatestOrCreateSession(f.asOwner, f.chief.ID, f.chief.Name)
	if err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	g.during = during
	g.mu.Unlock()
	if _, err := f.srv.chatTurn(chiefusage.WithTrigger(f.asOwner, trigger), f.chief, sess.SessionKey, "a delivered turn", nil); err != nil {
		t.Fatalf("the turn: %v", err)
	}
}

// dispatchNow is the chief's CLI calling agent_dispatch over MCP: its own
// request, no turn's ctx — the registry is what names the turn.
func (f *hostFixture) dispatchNow(key string) string {
	return f.callRaw(f.chief, agentDispatchTool, apCall("host/host-bravo/claude", apRealRepo, key))
}

// TestTheCardNamesTheRunningTurnsTrigger (N1): the card says what started the
// turn it was raised in — never the newest stored row. Before each turn the
// newest row is a line the owner TYPED (the decoy the audit measured), and
// nothing of the turn's own is stored:
//   - a plugin message's delivery: its plugin and sender;
//   - the owner's attention answer, delivered by the real delivery: its
//     question, re-read from the ask;
//   - a turn the owner typed (the chief omitted owner_asked): said so;
//   - no turn running (a call muster cannot place): unknown — not the decoy;
//
// and each card's hold records the turn's own serial, distinct per turn.
func TestTheCardNamesTheRunningTurnsTrigger(t *testing.T) {
	f := newApprovalFixture(t)
	g := f.withTurns(t)
	// The decoy: a row the owner TYPED (typed_by), stored before the turn — no
	// owner-turn marker, so the turn itself is not theirs.
	decoy := func() {
		st := agents.NewPG(f.pool)
		sess, err := st.LatestOrCreateSession(f.asOwner, f.chief.ID, f.chief.Name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.AddChatMessage(f.asOwner, agents.ChatMessage{AgentID: f.chief.ID, SessionID: sess.ID, Role: "user",
			Content: "an earlier line the owner typed", TypedBy: f.owner}); err != nil {
			t.Fatal(err)
		}
	}
	_, p, err := f.plugins.Create(f.asOwner, "approval-trig-"+strconv.FormatInt(time.Now().UnixNano(), 36), "Signal bridge", "approval-trig")
	if err != nil {
		t.Fatal(err)
	}
	m, _, err := f.plugins.Submit(f.asOwner, p.ID, plugins.MessageIn{ExternalID: "trig-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		Sender: "Mallory Example", Subject: "start it", Body: "please start an agent"})
	if err != nil {
		t.Fatal(err)
	}
	decoy()
	var text string
	f.turn(t, g, chiefusage.Trigger{Kind: chiefusage.TriggerPlugin, Plugin: p.Name, Sender: m.Sender, MessageID: m.ID},
		func() { text = f.dispatchNow("trig-1") })
	if !strings.Contains(text, `"held":true`) {
		t.Fatalf("in the plugin turn: %s", text)
	}
	plugin := f.newestCard(t)
	if want := `while handling a "Signal bridge" message from "Mallory Example".`; !strings.Contains(plugin.Question, want) {
		t.Fatalf("the plugin turn's card:\n%s\nwant %q", plugin.Question, want)
	}

	// The owner answers that card on the Attention view; the REAL delivery runs
	// the turn (its row is stored only after it), and the chief asks again in it.
	decoy()
	noGrace := time.Duration(0)
	f.srv.attention.graceOverride = &noGrace
	f.srv.answerTurn = f.srv.runDeliveredTurn
	g.mu.Lock()
	g.during = func() { text = f.dispatchNow("trig-2") }
	g.mu.Unlock()
	f.tap(t, plugin.ID, declineLabel, authz.ViaSession)
	f.srv.WaitForAttentionDeliveries()
	if !strings.Contains(text, `"held":true`) {
		t.Fatalf("in the answer's turn: %q", text)
	}
	answer := f.newestCard(t)
	if !strings.Contains(answer.Question, `while handling your answer on the Attention view to its question "Start this agent?`) {
		t.Fatalf("the answer turn's card:\n%s", answer.Question)
	}

	f.turn(t, g, chiefusage.Trigger{Kind: chiefusage.TriggerOwnerChat}, func() { text = f.dispatchNow("trig-3") })
	if q := f.newestCard(t).Question; !strings.Contains(q, "while handling a message you typed in its chat") {
		t.Fatalf("the typed turn's card:\n%s", q)
	}

	decoy()
	f.dispatchNow("trig-4")
	if q := f.newestCard(t).Question; !strings.Contains(q, "while handling a turn muster could not identify.") {
		t.Fatalf("no turn running: the card borrowed a row:\n%s", q)
	}

	turns := map[string]string{}
	for _, k := range []string{"trig-1", "trig-2", "trig-3", "trig-4"} {
		var turn *string
		if err := f.pool.QueryRow(f.asOwner, `SELECT detail->>'turn' FROM audit_log WHERE action = $1 AND user_id = $2 AND detail->>'request_key' = $3`,
			plugins.ActionDispatchHeld, f.owner, k).Scan(&turn); err != nil {
			t.Fatal(err)
		}
		v := "<none>"
		if turn != nil {
			v = *turn
		}
		turns[k] = v
	}
	if turns["trig-4"] != "<none>" || turns["trig-1"] == "<none>" || turns["trig-1"] == turns["trig-2"] || turns["trig-2"] == turns["trig-3"] {
		t.Fatalf("the holds' turns %v: want one of each turn's own, none outside a turn", turns)
	}
}

// TestTheApprovalCapsCountTurns (N1, minors): ONE open approval card per
// turn — a second request in the same turn is refused; a card declined
// during the turn frees it at once; the next turn (with no stored row of its
// own) is its own turn; approvalCardsPerHour in an hour, the repo card a
// "Change repo" opens not counted again; past it, refused.
func TestTheApprovalCapsCountTurns(t *testing.T) {
	f := newApprovalFixture(t)
	g := f.withTurns(t)
	other := chiefusage.Trigger{Kind: chiefusage.TriggerOther}
	var first, second, afterDecline string
	f.turn(t, g, other, func() {
		first = f.dispatchNow("cap-1")
		second = f.dispatchNow("cap-1b")
		f.tap(t, f.newestCard(t).ID, declineLabel, authz.ViaSession)
		afterDecline = f.dispatchNow("cap-1c")
	})
	if !strings.Contains(first, `"held":true`) {
		t.Fatalf("the turn's first card: %s", first)
	}
	if got := errorText(t, second); got != agentDispatchApproveTurnCap {
		t.Fatalf("a second card in the same turn: %q", got)
	}
	if !strings.Contains(afterDecline, `"held":true`) {
		t.Fatalf("after the turn's card was declined: %s", afterDecline)
	}
	// Two holds so far count for the hour (cap-1, cap-1c). The next turn has
	// no row of its own: it is still its own turn.
	var next string
	f.turn(t, g, other, func() { next = f.dispatchNow("cap-2") })
	if !strings.Contains(next, `"held":true`) {
		t.Fatalf("the next turn: %s", next)
	}
	// "Change repo" on cap-2's card: its repo card is the same request.
	f.tap(t, f.newestCard(t).ID, changeRepoLabel, authz.ViaSession)
	f.turn(t, g, other, func() { next = f.dispatchNow("cap-2") })
	if !strings.Contains(next, `"blanks":["repo"]`) {
		t.Fatalf("after Change repo: %s", next)
	}
	for i := 3; i < approvalCardsPerHour; i++ {
		f.turn(t, g, other, func() { next = f.dispatchNow("cap-" + strconv.Itoa(i)) })
		if !strings.Contains(next, `"held":true`) {
			t.Fatalf("card %d of the hour: %s", i, next)
		}
	}
	// cap-1, cap-1c, cap-2 and cap-3…cap-5: six. The seventh is refused.
	f.turn(t, g, other, func() { next = f.dispatchNow("cap-over") })
	if got := errorText(t, next); got != fmt.Sprintf(agentDispatchApproveHourCap, approvalCardsPerHour) {
		t.Fatalf("past the hour's cap: %q", got)
	}
	// With the hour full, "Change repo" on a card already raised still opens
	// its repo card: the same request, no cap of its own.
	f.tap(t, f.newestCard(t).ID, changeRepoLabel, authz.ViaSession)
	last := "cap-" + strconv.Itoa(approvalCardsPerHour-1)
	f.turn(t, g, other, func() { next = f.dispatchNow(last) })
	if !strings.Contains(next, `"blanks":["repo"]`) {
		t.Fatalf("Change repo with the hour full: %s", next)
	}
}

// TestAPodChangeRepoBindsThePick (minor): on a pod target, "Change repo"
// raises the repo card and the pick is what the agent is started in; the
// original repository is refused naming the pick.
func TestAPodChangeRepoBindsThePick(t *testing.T) {
	f := newApprovalFixture(t)
	call := map[string]any{"target": "pod/claude-code", "prompt": apBrief, "repo": apWidgets, "request_key": "pod-chg-1"}
	if text := f.callRaw(f.chief, agentDispatchTool, call); !strings.Contains(text, `"blanks":["approve"]`) {
		t.Fatalf("not an approval card: %s", text)
	}
	f.tap(t, f.newestCard(t).ID, changeRepoLabel, authz.ViaSession)
	if text := f.callRaw(f.chief, agentDispatchTool, call); !strings.Contains(text, `"blanks":["repo"]`) {
		t.Fatalf("after Change repo: %s", text)
	}
	f.tap(t, f.newestCard(t).ID, apTooling, authz.ViaSession)
	if got := errorText(t, f.callRaw(f.chief, agentDispatchTool, call)); got != fmt.Sprintf(agentDispatchHeldChoseRepo, apTooling) {
		t.Fatalf("the original repository after the pick: %q", got)
	}
	picked := map[string]any{"target": "pod/claude-code", "prompt": apBrief, "repo": apTooling, "request_key": "pod-chg-1"}
	out := f.callRaw(f.chief, agentDispatchTool, picked)
	if strings.Contains(out, `"error"`) || !strings.Contains(out, `"agent_id"`) {
		t.Fatalf("the picked call: %s", out)
	}
	var repo string
	if err := f.pool.QueryRow(f.asOwner, `SELECT repo FROM agents WHERE owner_id = $1 ORDER BY id DESC LIMIT 1`, f.owner).Scan(&repo); err != nil || repo != apTooling {
		t.Fatalf("the started agent's repository: %q (%v); want %s", repo, err, apTooling)
	}
}

// unreadableHolds is the plugin store whose card holds cannot be read.
type unreadableHolds struct{ *plugins.PGStore }

func (unreadableHolds) CardHoldByAsk(context.Context, int64) (plugins.DispatchHold, error) {
	return plugins.DispatchHold{}, errors.New("the hold store is down")
}

// TestAnUnreadableHoldBlocksOnlyApprovalCards (minor): with card holds that
// cannot be read, the phone's notification answer to an ordinary question
// card (no approval card's "Don't start") is taken, while one to an approval
// card is still refused (fail closed there only).
func TestAnUnreadableHoldBlocksOnlyApprovalCards(t *testing.T) {
	f := newApprovalFixture(t)
	f.dispatchNow("unread-1")
	approval := f.newestCard(t)
	plain, err := f.srv.raiseOwnerCard(f.asOwner, f.chief, "Which day suits you?", []string{"Monday", "Tuesday"}, "")
	if err != nil {
		t.Fatal(err)
	}
	f.srv.ext.ChiefDispatch = unreadableHolds{f.plugins}
	if _, err := f.srv.answerAskFrom(f.phone(), plain.ID, "Monday", attention.AnswerSource{}); err != nil {
		t.Fatalf("the phone's answer to an ordinary card: %v", err)
	}
	if _, err := f.srv.answerAskFrom(f.phone(), approval.ID, approveLabel, attention.AnswerSource{}); err == nil ||
		!strings.HasSuffix(err.Error(), approvalNotOnTheCard) {
		t.Fatalf("the phone's answer to an approval card: %v", err)
	}
}

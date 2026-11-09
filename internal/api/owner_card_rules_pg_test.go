package api

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/ui"
)

// The stored card's rules that kept #496's behaviour (audit round 1, #513):
// an expired card is still answered in the chat by a typed reply (Y1); one
// answer settles one card (Y2); a refused raise still asks (Y3); the answer is
// delivered into the session the chief asked in (Y4); and only the owner's own
// doors answer a card (Y5).

const secondCardArgs = `{"question":"Which model?","options":[{"label":"qwen/qwen3-coder"},{"label":"Claude (Claude Code)"}]}`

// seedTranscript stores msgs on the chief's default session, in order.
func (f *deliveryFixture) seedTranscript(t *testing.T, msgs ...agents.ChatMessage) {
	t.Helper()
	for _, m := range msgs {
		m.AgentID, m.SessionID = f.chief.ID, f.def.ID
		if _, err := f.agentsStore.AddChatMessage(asUser(1), m); err != nil {
			t.Fatal(err)
		}
	}
}

func cardCall(toolID, args string, askID int64) []agents.ChatMessage {
	return []agents.ChatMessage{
		{Role: "assistant", Kind: "tool_call", ToolID: toolID, ToolName: askOwnerTool, Content: args},
		{Role: "assistant", Kind: "tool_result", ToolID: toolID, ToolName: askOwnerTool, ToolOK: true,
			Content: `{"asked":true,"ask_id":` + strconv.FormatInt(askID, 10) + `}`},
	}
}

// TestAnExpiredStoredCardIsStillAnsweredByATypedReply (Y1): a stored card whose
// ask lapsed is drawn from the transcript, as #496 drew every card — waiting
// while nothing was typed after it, then answered by the owner's typed reply.
func TestAnExpiredStoredCardIsStillAnsweredByATypedReply(t *testing.T) {
	f := newDeliveryFixture(t)
	id := f.askOwnerAs(t, ownerCardArgs)
	f.seedCardTranscript(t, id)
	if _, err := f.pool.Exec(authz.System(context.Background()),
		`UPDATE attention_asks SET created_at = now() - interval '2 days', expires_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if st, _ := f.cardState(t); st != ui.OwnerAskWaiting {
		t.Fatalf("an expired card with no reply after it is %q; want waiting (the transcript's)", st)
	}
	f.seedTranscript(t, agents.ChatMessage{Role: "user", Content: "host-charlie", TypedBy: 1})
	st, text := f.cardState(t)
	if st != ui.OwnerAskAnswered || !strings.Contains(text, "Your answer: host-charlie") {
		t.Fatalf("an expired card after a typed reply is %q: %s", st, text)
	}
}

// TestOneTypedReplySettlesOneCard (Y2): two cards asked since the owner last
// typed. The chat draws ONE card (the second is its plain row), and the
// owner's typed reply settles only the oldest ask; the other stays open for
// its own answer on the Attention view.
func TestOneTypedReplySettlesOneCard(t *testing.T) {
	f := newDeliveryFixture(t)
	// Another asker's older open card: the rule is per asker, so it is neither
	// settled nor counted as this chief's oldest.
	theirs, err := f.srv.raiseOwnerCard(asUser(1), f.op, "Their question?", []string{"a", "b"}, "")
	if err != nil {
		t.Fatal(err)
	}
	first := f.askOwnerAs(t, ownerCardArgs)
	second := f.askOwnerAs(t, secondCardArgs)
	msgs := []agents.ChatMessage{{Role: "user", Content: "start something mirrored", TypedBy: 1}}
	msgs = append(msgs, cardCall("call_one", ownerCardArgs, first)...)
	msgs = append(msgs, cardCall("call_two", secondCardArgs, second)...)
	f.seedTranscript(t, msgs...)
	if st, _ := f.cardState(t); st != ui.OwnerAskWaiting { // cardState fails on two cards
		t.Fatalf("the one card is %q", st)
	}
	f.srv.askerAddressed(asUser(1), attention.Asker{AgentID: f.chief.ID}, "host-bravo")
	a1, _ := f.attn.Get(asUser(1), first)
	a2, _ := f.attn.Get(asUser(1), second)
	if a1.Status != attention.AnsweredInSession || a1.Answer != "host-bravo" {
		t.Fatalf("the first card: %+v", a1.Status)
	}
	if a2.Status != attention.Open {
		t.Fatalf("one typed reply also settled the second card: %+v", a2.Status)
	}
	if a, _ := f.attn.Get(asUser(1), theirs.ID); a.Status != attention.Open {
		t.Fatalf("a reply to the chief settled another asker's card: %+v", a.Status)
	}
	// The next reply answers the next card.
	f.srv.askerAddressed(asUser(1), attention.Asker{AgentID: f.chief.ID}, "qwen/qwen3-coder")
	if a2, _ = f.attn.Get(asUser(1), second); a2.Status != attention.AnsweredInSession || a2.Answer != "qwen/qwen3-coder" {
		t.Fatalf("the second card after a second reply: %+v", a2)
	}
}

// TestATypedReplyStillSettlesEveryPlainAsk (Y2's control): the one-card rule is
// the owner cards' only — two attention_ask questions by the same chief are
// both settled by one message, as before.
func TestATypedReplyStillSettlesEveryPlainAsk(t *testing.T) {
	f := newDeliveryFixture(t)
	var ids []int64
	for _, q := range []string{"Plain one?", "Plain two?"} {
		a, err := f.attn.Create(asUser(1), attention.New{Asker: callerOfAgent(f.chief).asker, Question: q, Options: []string{"yes", "no"}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, a.ID)
	}
	f.srv.askerAddressed(asUser(1), attention.Asker{AgentID: f.chief.ID}, "yes")
	for _, id := range ids {
		if a, _ := f.attn.Get(asUser(1), id); a.Status != attention.AnsweredInSession {
			t.Fatalf("plain ask %d: %+v", id, a.Status)
		}
	}
}

// refusingAttention is an attention store whose Create fails with err.
type refusingAttention struct {
	attention.Store
	err error
}

func (r refusingAttention) Create(context.Context, attention.New) (attention.Ask, error) {
	return attention.Ask{}, r.err
}

// TestARefusedRaiseStillAsksInTheChat (Y3): when the store refuses the card —
// the per-owner cap or a database error — ask_owner still asks and ends the
// turn, naming no ask_id, so the chat draws the card from the call.
func TestARefusedRaiseStillAsksInTheChat(t *testing.T) {
	f := newDeliveryFixture(t)
	for _, why := range []error{attention.ErrTooMany, errors.New("connection reset by peer")} {
		f.srv.ext.Attention = refusingAttention{Store: f.attn, err: why}
		ctx := agents.WithTurnEnd(asUser(1))
		out := f.srv.toolAskOwner(ctx, f.chief, ownerCardArgs)
		if !strings.Contains(out, `"asked":true`) || !agents.TurnEnded(ctx) {
			t.Fatalf("%v: %s (turn ended %v)", why, out, agents.TurnEnded(ctx))
		}
		if _, ok := ui.ReadOwnerCardResult(out); ok {
			t.Fatalf("%v: a refused raise names an ask: %s", why, out)
		}
	}
}

// TestAnAnswerIsDeliveredIntoTheSessionTheChiefAskedIn (Y4): a card raised
// during the chief's turn in its NEWER session (not the default) has its
// Attention view answer delivered there. Control: a card raised outside any
// turn goes to the default session, as every answer did.
func TestAnAnswerIsDeliveredIntoTheSessionTheChiefAskedIn(t *testing.T) {
	f := newDeliveryFixture(t)
	if f.newer.ID == f.def.ID {
		t.Fatal("control: the fixture's two sessions are one")
	}
	end := f.srv.turnSessions.begin(f.chief.ID, f.newer.SessionKey)
	id := f.askOwnerAs(t, ownerCardArgs)
	end()
	if a, _ := f.attn.Get(asUser(1), id); a.AskedSession != f.newer.SessionKey {
		t.Fatalf("the card records session %q; want %q", a.AskedSession, f.newer.SessionKey)
	}
	f.answerForm(t, id, "host-charlie")
	if a := f.delivery(t, id); a.Delivery != attention.DeliveryDelivered || a.DeliveredSession != f.newer.ID {
		t.Fatalf("delivered %q into %d; want the asked-in session %d (default %d)", a.Delivery, a.DeliveredSession, f.newer.ID, f.def.ID)
	}
	other := f.askOwnerAs(t, ownerCardArgs)
	f.answerForm(t, other, "host-bravo")
	if a := f.delivery(t, other); a.DeliveredSession != f.def.ID {
		t.Fatalf("a card raised outside a turn was delivered into %d; want the default %d", a.DeliveredSession, f.def.ID)
	}
}

// TestAnOwnerCardIsNotAnsweredOverMCP (Y5): attention_answer from a mirrored
// session cannot answer an owner card (ErrNotOwnerDoor; it stays open). The
// control: the same chief's plain attention_ask IS answerable there.
func TestAnOwnerCardIsNotAnsweredOverMCP(t *testing.T) {
	f := newDeliveryFixture(t)
	const host, sess = "host-bravo", "0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b"
	id := f.askOwnerAs(t, ownerCardArgs)
	if _, err := f.attn.AnswerFromSession(asUser(1), id, "host-bravo", host, sess); !errors.Is(err, attention.ErrNotOwnerDoor) {
		t.Fatalf("an MCP answer to an owner card: %v; want ErrNotOwnerDoor", err)
	}
	if a, _ := f.attn.Get(asUser(1), id); a.Status != attention.Open {
		t.Fatalf("the refused MCP answer moved the card: %+v", a.Status)
	}
	plain, err := f.attn.Create(asUser(1), attention.New{Asker: callerOfAgent(f.chief).asker, Question: "Plain?", Options: []string{"yes", "no"}})
	if err != nil {
		t.Fatal(err)
	}
	if a, err := f.attn.AnswerFromSession(asUser(1), plain.ID, "yes", host, sess); err != nil || a.Status != attention.Answered {
		t.Fatalf("control: an MCP answer to a plain ask: %+v, %v", a.Status, err)
	}
}

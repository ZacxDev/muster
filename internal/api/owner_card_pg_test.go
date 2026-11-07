package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE STORED ask_owner CARD (docs/design/chief-dispatch.md 2.8, the four
// properties mirrored dispatch needs from it), over a real Postgres:
//
//  1. the card is an attention ask: stored options, an id, and the door its
//     answer came through (0092: browser, device, mcp);
//  2. muster's own code raises one for the chief (raiseOwnerCard), not only
//     the chief's tool call;
//  3. its answer reaches the chief as a turn — delivered from the Attention
//     view or phone, or the owner's own typed message in the chat;
//  4. (dispatch_card) the live agent card is keyed by a host dispatch too.
//
// Fixtures pick option labels that are pairwise distinct, so a mutant that
// binds "the first option" or "the only option" is seen.

const ownerCardArgs = `{"question":"Which host?","options":[{"label":"host-bravo","description":"online"},{"label":"host-charlie"}]}`

// askOwnerAs runs ask_owner for the delivery fixture's chief and returns the
// stored card's ask id. It calls the tool's function directly, as the owner's
// principal: which of user 1's agents is THE chief is decided per call from the
// agent list, which other fixtures of this package change concurrently, and the
// chief-only gate has its own tests (TestAskOwnerIsTheChiefsOverMCPAndOffersItsSchema).
func (f *deliveryFixture) askOwnerAs(t *testing.T, args string) int64 {
	t.Helper()
	text := f.srv.toolAskOwner(asUser(1), f.chief, args)
	id, ok := ui.ReadOwnerCardResult(text)
	if !ok || !strings.Contains(text, "END YOUR TURN NOW") {
		t.Fatalf("ask_owner's result names no stored card: %s", text)
	}
	return id
}

// seedCardTranscript stores the chief's ask_owner call and its result (as the
// turn would) on the chief's default session, so its chat log draws the card.
func (f *deliveryFixture) seedCardTranscript(t *testing.T, id int64) {
	t.Helper()
	ctx := asUser(1)
	for _, m := range []agents.ChatMessage{
		{Role: "user", Content: "start something mirrored", TypedBy: 1},
		{Role: "assistant", Kind: "tool_call", ToolID: "call_card", ToolName: askOwnerTool, Content: ownerCardArgs},
		{Role: "assistant", Kind: "tool_result", ToolID: "call_card", ToolName: askOwnerTool, ToolOK: true,
			Content: `{"asked":true,"ask_id":` + strconv.FormatInt(id, 10) + `}`},
	} {
		m.AgentID, m.SessionID = f.chief.ID, f.def.ID
		if _, err := f.agentsStore.AddChatMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
}

// cardState is the chief's chat log's one question card: its state and node.
func (f *deliveryFixture) cardState(t *testing.T) (string, string) {
	t.Helper()
	w := f.call(t, http.MethodGet, "/ui/agents/"+f.chief.Name+"/chat-log?session="+strconv.FormatInt(f.def.ID, 10), nil, f.operator())
	if w.Code != http.StatusOK {
		t.Fatalf("chat-log: %d %s", w.Code, w.Body)
	}
	cards := nodesWith(parseHTML(t, w.Body.String()), "data-owner-ask", "")
	if len(cards) != 1 {
		t.Fatalf("%d question cards in the chief's chat", len(cards))
	}
	st, _ := nodeAttr(cards[0], "data-owner-ask-state")
	return st, nodeText(cards[0])
}

// TestAskOwnerFilesAStoredCardOnTheAttentionViewAndPhone: property 1. The
// chief's ask_owner is an attention ask raised BY THE CHIEF, holding the
// option labels as its stored options, open, pushed to the owner's phone and
// listed on their Attention view; the result names it.
func TestAskOwnerFilesAStoredCardOnTheAttentionViewAndPhone(t *testing.T) {
	f := newDeliveryFixture(t)
	id := f.askOwnerAs(t, ownerCardArgs)
	a, err := f.attn.Get(asUser(1), id)
	if err != nil {
		t.Fatal(err)
	}
	if a.Asker.AgentID != f.chief.ID || a.Status != attention.Open || a.Kind != attention.KindAsk ||
		strings.Join(a.Options, "|") != "host-bravo|host-charlie" || a.Question != "Which host?" {
		t.Fatalf("the stored card is %+v; want an open ask by the chief with the two labels", a)
	}
	if want := "attention-ask-" + strconv.FormatInt(id, 10); !strings.Contains(strings.Join(f.pushes(), "\n"), want) {
		t.Errorf("no push %s reached the owner's phone: %v", want, f.pushes())
	}
	if page := f.page(t, "/attention"); !strings.Contains(page, "Which host?") {
		t.Error("the card is not on the owner's Attention view")
	}
}

// TestAStoredCardAnsweredOnTheViewReachesTheChiefWithItsDoor: properties 1 and
// 3, and the chat reading the STORED state. Before the answer the chief's chat
// draws the card waiting, naming its ask; the owner taps "host-charlie" on the
// Attention view; the row is answered with door 'browser' and binds exactly the
// second option (ChosenOption = 1); the answer is delivered to the chief's
// default session as one turn; and the chat now draws the card answered — with
// no typed line after it, so only the stored row can have said so.
func TestAStoredCardAnsweredOnTheViewReachesTheChiefWithItsDoor(t *testing.T) {
	f := newDeliveryFixture(t)
	id := f.askOwnerAs(t, ownerCardArgs)
	f.seedCardTranscript(t, id)
	if st, _ := f.cardState(t); st != ui.OwnerAskWaiting {
		t.Fatalf("before any answer the card is %q", st)
	}
	f.answerForm(t, id, "host-charlie")
	a := f.delivery(t, id)
	if a.Status != attention.Answered || a.AnsweredVia.Via != attention.ViaBrowser || !a.AnsweredVia.OwnerDoor() {
		t.Fatalf("answered %+v (door %q); want answered through the owner's browser", a.Status, a.AnsweredVia.Via)
	}
	if i, ok := a.ChosenOption(); !ok || i != 1 {
		t.Fatalf("ChosenOption = %d, %v; want 1 (host-charlie)", i, ok)
	}
	if a.Delivery != attention.DeliveryDelivered || a.DeliveredSession != f.def.ID {
		t.Fatalf("delivery %q into %d; want delivered into the default session %d", a.Delivery, a.DeliveredSession, f.def.ID)
	}
	turns := f.turnsInto(f.def)
	if len(turns) != 1 || !strings.Contains(turns[0], "your owner answered attention #"+strconv.FormatInt(id, 10)) || !strings.Contains(turns[0], "host-charlie") {
		t.Fatalf("the chief's turns: %q", turns)
	}
	st, text := f.cardState(t)
	if st != ui.OwnerAskAnswered || !strings.Contains(text, "Your answer: host-charlie") {
		t.Fatalf("after the view's answer the chat card is %q: %s", st, text)
	}
}

// TestAStoredCardTakesAFreeTextAnswerThatBindsNoOption: the "Other" answer is
// kept — a typed answer on the view is recorded and delivered — and binds NO
// option, even one that differs from an option only by case.
func TestAStoredCardTakesAFreeTextAnswerThatBindsNoOption(t *testing.T) {
	f := newDeliveryFixture(t)
	for _, answer := range []string{"neither, wait for tomorrow", "HOST-BRAVO"} {
		id := f.askOwnerAs(t, ownerCardArgs)
		f.answerForm(t, id, answer)
		a := f.delivery(t, id)
		if a.Status != attention.Answered || a.Answer != answer {
			t.Fatalf("free text %q: %+v", answer, a)
		}
		if i, ok := a.ChosenOption(); ok {
			t.Fatalf("free text %q bound option %d", answer, i)
		}
	}
}

// TestTheOwnersTypedMessageAnswersAStoredCardInTheChat: a tap in the chat is
// the owner's next typed message (#496's behaviour, kept), and that message
// settles the stored card in session, so the Attention view and the chat agree.
func TestTheOwnersTypedMessageAnswersAStoredCardInTheChat(t *testing.T) {
	f := newDeliveryFixture(t)
	id := f.askOwnerAs(t, ownerCardArgs)
	f.seedCardTranscript(t, id)
	f.srv.askerAddressed(asUser(1), attention.Asker{AgentID: f.chief.ID}, "host-bravo")
	a, err := f.attn.Get(asUser(1), id)
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != attention.AnsweredInSession || a.Answer != "host-bravo" {
		t.Fatalf("after the owner's typed message the card is %+v", a)
	}
	if i, ok := a.ChosenOption(); !ok || i != 0 {
		t.Fatalf("ChosenOption = %d, %v; want 0", i, ok)
	}
	if a.AnsweredVia.OwnerDoor() {
		t.Error("an in-session answer claims an owner's door: its turn is the owner's typed turn, not a recorded tap")
	}
	if st, _ := f.cardState(t); st != ui.OwnerAskAnswered {
		t.Fatalf("the chat card is %q", st)
	}
}

// TestMusterRaisesACardForTheChiefAndThePhoneAnswerIsRecordedAsTheDevice:
// property 2. Server code (no tool call) raises a card for the chief: an ask by
// the chief, pushed; answered from the owner's PHONE (a ViaDevice principal,
// as the signed device action runs) it records door 'device' and is delivered
// to the chief. The CONTROL: the same answer under an API token's principal is
// REFUSED (ErrNotOwnerDoor) and the card stays open — the door is the
// principal's, never a constant, and an owner card takes only the owner's own.
func TestMusterRaisesACardForTheChiefAndThePhoneAnswerIsRecordedAsTheDevice(t *testing.T) {
	f := newDeliveryFixture(t)
	for _, c := range []struct {
		via      authz.Via
		wantDoor string
	}{{authz.ViaDevice, attention.ViaDevice}, {authz.ViaToken, ""}} {
		a, err := f.srv.raiseOwnerCard(asUser(1), f.chief, "Which model?", []string{"Claude (Claude Code)", "qwen/qwen3-coder"})
		if err != nil {
			t.Fatal(err)
		}
		if a.Asker.AgentID != f.chief.ID || a.Status != attention.Open {
			t.Fatalf("raised %+v", a)
		}
		if want := "attention-ask-" + strconv.FormatInt(a.ID, 10); !strings.Contains(strings.Join(f.pushes(), "\n"), want) {
			t.Fatalf("the raised card was not pushed: %v", f.pushes())
		}
		ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: c.via})
		_, err = f.srv.answerAsk(ctx, a.ID, "Claude (Claude Code)")
		if c.wantDoor == "" {
			if !errors.Is(err, attention.ErrNotOwnerDoor) {
				t.Fatalf("answered under %s: %v; want ErrNotOwnerDoor", c.via, err)
			}
			if got, _ := f.attn.Get(asUser(1), a.ID); got.Status != attention.Open {
				t.Fatalf("a refused door moved the card: %+v", got.Status)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		got := f.delivery(t, a.ID)
		if got.AnsweredVia.Via != c.wantDoor || got.AnsweredVia.OwnerDoor() != (c.wantDoor != "") {
			t.Fatalf("answered under %s: door %q (owner door %v); want %q", c.via, got.AnsweredVia.Via, got.AnsweredVia.OwnerDoor(), c.wantDoor)
		}
		if got.Delivery != attention.DeliveryDelivered {
			t.Fatalf("the raised card's answer was not delivered to the chief: %q", got.Delivery)
		}
	}
}

// TestADispatchCardIsKeyedByAHostDispatch: property 4. An agent_dispatch result
// naming a host dispatch draws a card keyed "host-<id>" (never an agent id),
// served by ?host_dispatch=, from the host-dispatch source; with no source it
// says it could not be read and stays refreshable.
func TestADispatchCardIsKeyedByAHostDispatch(t *testing.T) {
	f := newDeliveryFixture(t)
	get := func() string {
		w := f.call(t, http.MethodGet, ui.DispatchCardHostSrc(7), nil, f.operator())
		if w.Code != http.StatusOK {
			t.Fatalf("host card: %d %s", w.Code, w.Body)
		}
		return w.Body.String()
	}
	body := get()
	if !strings.Contains(body, `data-dispatch-agent="host-7"`) || !strings.Contains(body, `data-dispatch-state="unreadable"`) {
		t.Fatalf("with no source: %s", body)
	}
	f.srv.hostDispatchCardSource = func(context.Context, int64) (ui.DispatchCard, error) {
		return ui.DispatchCard{Host: "host-bravo", Window: "agents:@4", Runtime: "Claude Code", State: ui.DispatchWorking,
			HostNote: "close the window on host-bravo to stop it"}, nil
	}
	body = get()
	for _, want := range []string{`data-dispatch-agent="host-7"`, `data-dispatch-host-id="7"`, `data-dispatch-state="working"`,
		"host-bravo · agents:@4", "close the window on host-bravo to stop it", `hx-get="/ui/dispatch-card?host_dispatch=7"`} {
		if !strings.Contains(body, want) {
			t.Errorf("host card lacks %q:\n%s", want, body)
		}
	}
	if r, ok := ui.ReadDispatchResult(`{"host_dispatch_id":7,"host":"host-bravo"}`); !ok || r.HostDispatchID != 7 || r.AgentID != 0 {
		t.Errorf("ReadDispatchResult = %+v, %v", r, ok)
	}
}

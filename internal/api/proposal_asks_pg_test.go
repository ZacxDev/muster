package api

import (
	"encoding/json"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/ui"
)

// A QUESTION THE PROPOSAL ANSWERED (chief_proposal.go withdrawAnsweredAsks):
// the owner asks from a session's view, the chief asks them a question about
// it, the owner answers by typing a NEW line on the view (which settles none
// of the chief's asks: view_chief.go), and the chief files the proposal. The
// question is then withdrawn — and only that one. Every value is invented
// fixture data.

// askedID is the ask id in an ask_owner or attention_ask result.
func askedID(t *testing.T, text string) int64 {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("not a tool result: %s", text)
	}
	for _, k := range []string{ui.OwnerCardAskIDKey, "id"} {
		if v, ok := out[k].(float64); ok && v > 0 {
			return int64(v)
		}
	}
	t.Fatalf("no ask id in %s", text)
	return 0
}

func (f *succFixture) askOwner(t *testing.T, question string) int64 {
	t.Helper()
	return askedID(t, f.callRaw(f.chief, askOwnerTool, map[string]any{"question": question,
		"options": []map[string]any{{"label": "acme/widgets"}, {"label": "acme/gadgets"}}}))
}

func (f *succFixture) attentionAsk(t *testing.T, question string) int64 {
	t.Helper()
	return askedID(t, f.callRaw(f.chief, attentionAskTool, map[string]any{"question": question}))
}

func (f *succFixture) askStatus(t *testing.T, id int64) attention.Ask {
	t.Helper()
	a, err := f.srv.ext.Attention.Get(f.asOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestAProposalWithdrawsTheQuestionsItAnswered: the chief's ask_owner card
// and attention_ask raised in the turn of the owner's view line about the
// session are withdrawn (cancelled, with muster's reason) once the chief
// files a proposal about it. Untouched: the chief's question in the turn of a
// view line about ANOTHER session; its question raised while a plugin message
// was the newest input (linked to no session); and another agent's question
// about the same session; and the proposal's own card.
func TestAProposalWithdrawsTheQuestionsItAnswered(t *testing.T) {
	f := newSuccFixture(t)
	// Another session first: the chief's question about it.
	f.viewLine(t, sxClaudeKey, "what is this session doing?")
	otherSession := f.askOwner(t, "Do you want its summary or its diff?")

	// The owner asks about the viewed session; the chief asks back, twice.
	f.viewLine(t, sxOpenCodeKey, "copy the kickoff and start a spin-off")
	card := f.askOwner(t, "Which repository should the spin-off use?")
	asked := f.attentionAsk(t, "Which base branch should it start from?")

	// A plugin message arrives; a question raised now is not linked.
	f.plainLine(t, "Plugin Example mail: the weekly newsletter", 0)
	afterMail := f.askOwner(t, "Shall I archive the newsletter?")

	// Another agent of the owner's asks about the same session.
	worker, err := agents.NewPG(f.pool).Create(f.asOwner, agents.Agent{Name: "askworker-" + sxOpenCodeSID[4:12], Namespace: "devpod-askworker",
		Status: agents.StatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := f.srv.ext.Attention.Create(f.asOwner, attention.New{Asker: callerOfAgent(worker).asker, Question: "Is this session mine?",
		AboutKey: sxOpenCodeKey})
	if err != nil {
		t.Fatal(err)
	}

	// The owner answers on the VIEW, not on the card; the chief proposes.
	f.viewLine(t, sxOpenCodeKey, "acme/widgets, from main")
	text, id := f.propose(t, sxCall(sxOpenCodeKey, "spinoff", sxRefFenc, intp(0)))
	if id <= 0 {
		t.Fatalf("the proposal: %s", text)
	}
	x, err := f.plugins.ProposalByID(f.asOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	reason := "superseded: your chief proposed a succession of this session (proposal " + itoa(x.ID) + "), so this question about it was withdrawn"
	for name, aid := range map[string]int64{"the ask_owner card": card, "the attention_ask": asked} {
		a := f.askStatus(t, aid)
		if a.Status != attention.Cancelled || a.SettledReason != reason {
			t.Errorf("%s about the session: %s, reason %q; want cancelled, %q", name, a.Status, a.SettledReason, reason)
		}
	}
	for name, aid := range map[string]int64{"another session's question": otherSession, "a question after a plugin message": afterMail,
		"another agent's question": theirs.ID, "the proposal's own card": x.AskID} {
		if a := f.askStatus(t, aid); a.Status != attention.Open || a.SettledReason != "" {
			t.Errorf("%s: %s, reason %q; want it open", name, a.Status, a.SettledReason)
		}
	}
	// attention_wait tells the chief why (a read: wait_seconds 0).
	var wait map[string]any
	_ = json.Unmarshal([]byte(f.callRaw(f.chief, attentionWaitTool, map[string]any{"id": asked, "wait_seconds": 0})), &wait)
	if wait["status"] != string(attention.Cancelled) || wait["reason"] != reason {
		t.Errorf("attention_wait on the withdrawn ask: %v", wait)
	}
}

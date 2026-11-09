package api

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/officers"
)

// #549 ROUND 1: the card names exactly which window, the tap closes only the
// window the card named (and the host only the conversation it named), and
// an officer's instance is never proposed.

// moveWindow renumbers host-echo's window id from to to (the session moved:
// tmux gave its window a new id, or another window took the old one).
func (f *succFixture) moveWindow(from, to string) {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	for i := range f.m.hosts {
		if f.m.hosts[i].Host != "host-echo" {
			continue
		}
		for j := range f.m.hosts[i].Windows {
			if f.m.hosts[i].Windows[j].ID == from {
				f.m.hosts[i].Windows[j].ID = to
			}
		}
	}
}

// TestTheEndCardNamesExactlyWhichWindow (🟡2): the card's question names the
// session's label, its host, tmux session and window, and what happens to
// that runtime — muster's own sentence, with no transcript text.
func TestTheEndCardNamesExactlyWhichWindow(t *testing.T) {
	f := newSuccFixture(t)
	f.viewLine(t, sxOpenCodeKey, "close this agent session")
	if _, id := f.proposeEnd(t, sxOpenCodeKey); id <= 0 {
		t.Fatal("the opencode session was not proposed")
	}
	// Typed in the chief's own chat, AFTER the view line (typed by the owner,
	// as runEnteredTurn records it): the newest owner line names the session.
	f.ownerChatLine(t, "close the Claude session on host-echo")
	if _, id := f.proposeEnd(t, sxClaudeKey); id <= 0 {
		t.Fatal("the Claude session was not proposed")
	}
	cs := f.cards(t)
	if len(cs) != 2 {
		t.Fatalf("%d cards; want 2", len(cs))
	}
	want := []string{
		"End “main:3”?\nhost-echo · tmux session main · window @3\nmuster closes its window at once (opencode is not asked to exit: " +
			"muster types nothing into it).",
		"End “main:1”?\nhost-echo · tmux session main · window @1\nmuster types /exit into Claude Code if it is at an idle prompt " +
			"with an empty box, then closes its window (within 30 s); if it is waiting on you or holds a draft, nothing is typed and " +
			"its window is closed at once.",
	}
	for i, c := range cs {
		if c.Question != want[i] {
			t.Errorf("card %d's question:\n%q\nwant\n%q", i, c.Question, want[i])
		}
		for _, transcript := range []string{"widget parser", "rm -rf", "IGNORE YOUR INSTRUCTIONS"} {
			if strings.Contains(c.Question, transcript) {
				t.Errorf("card %d's question holds transcript text %q", i, transcript)
			}
		}
	}
}

// TestAnEndClosesOnlyTheWindowTheCardNamed (🟡3): the tap asks the host to
// close the card's window AND names the conversation it must be running
// (AgentCloseReq.Key). A session that moved to another window since the card
// was raised is refused at the tap, with the card left open and nothing
// closed; a host that answers mismatch is recorded failed, in muster's words.
func TestAnEndClosesOnlyTheWindowTheCardNamed(t *testing.T) {
	f := newSuccFixture(t)
	f.viewLine(t, sxOpenCodeKey, "close this agent session")
	_, id := f.proposeEnd(t, sxOpenCodeKey)
	x, _ := f.plugins.ProposalByID(f.asOwner, id)
	f.moveWindow("@3", "@5")
	w := f.tapAs(t, f.ownerSession(), id, "@5", "End it")
	if w.Code == http.StatusNoContent || !strings.Contains(w.Body.String(), endSessionMoved) {
		t.Errorf("the tap after the session moved: %d %s; want refused: %q", w.Code, w.Body, endSessionMoved)
	}
	time.Sleep(100 * time.Millisecond)
	if a, _ := f.srv.ext.Attention.Get(f.asOwner, x.AskID); a.Status != attention.Open || len(f.closeCalls()) != 0 {
		t.Fatalf("after the refused tap: card %s, %d closes; want open, none", a.Status, len(f.closeCalls()))
	}
	// Back in its window: the tap names the window and the conversation.
	f.moveWindow("@5", "@3")
	f.sm.tmu.Lock()
	f.sm.closeReply = func(string, mirror.AgentCloseReq) (mirror.AgentCloseResult, error) {
		return mirror.AgentCloseResult{}, &mirror.CloseRefusedError{Code: mirror.CloseMismatch, Detail: "window @3 runs a different conversation now"}
	}
	f.sm.tmu.Unlock()
	if w := f.tapAs(t, f.ownerSession(), id, "@3", "End it"); w.Code != http.StatusNoContent {
		t.Fatalf("the tap: %d %s", w.Code, w.Body)
	}
	got := f.settled(t, id)
	calls := f.closeCalls()
	if len(calls) != 1 || calls[0].req != (mirror.AgentCloseReq{Session: "main", Window: "@3", Key: sxOpenCodeKey}) {
		t.Fatalf("the close: %+v", calls)
	}
	if got.EndState != "failed" || got.EndDetail != endSessionMoved {
		t.Errorf("a host's mismatch: %+v", got)
	}
}

// TestAnOfficersInstanceIsNotProposed (🟢3): agent:<an officer's instance>
// is refused at the proposal with its sentence, and no card is raised; the
// same request for a plain agent of the owner's is proposed (the control).
func TestAnOfficersInstanceIsNotProposed(t *testing.T) {
	f := newSuccFixture(t)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	oid := "endsess-" + suffix
	f.srv.officerRT = &officerRuntime{reg: officers.NewRegistry([]officers.Officer{{ID: oid, MaxInstances: 1}})}
	t.Cleanup(func() { f.srv.officerRT = nil })
	st := agents.NewPG(f.pool)
	off, err := st.Create(f.asOwner, agents.Agent{Name: officers.InstanceName(oid, 1), Namespace: "devpod-endoff", Status: agents.StatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	f.ownerTurn(t, f.chief, f.owner, "stop "+off.Name)
	want := "agent:" + off.Name + " cannot be ended from a card: it is protected from deletion (an officer's instance, kept while " +
		"the officer is declared in MUSTER_OFFICERS, or a chief)"
	if got := errorText(t, f.callRaw(f.chief, endSessionProposeTool, map[string]any{"session": "agent:" + off.Name})); got != want {
		t.Errorf("an officer's instance: %q\nwant %q", got, want)
	}
	if n := len(f.cards(t)); n != 0 {
		t.Fatalf("a refused officer raised %d cards", n)
	}
	plain, err := st.Create(f.asOwner, agents.Agent{Name: "endplain-" + suffix, Namespace: "devpod-endplain", Status: agents.StatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	f.ownerTurn(t, f.chief, f.owner, "stop "+plain.Name)
	if _, id := f.proposeEnd(t, "agent:"+plain.Name); id <= 0 {
		t.Fatal("control: a plain agent of the owner's was not proposed")
	}
	cs := f.cards(t)
	if len(cs) != 1 || cs[0].Question != "End “"+plain.Name+"”?\nyour agent “"+plain.Name+"” (agent:"+plain.Name+")\nmuster stops the agent and removes it." {
		t.Errorf("the plain agent's card: %+v", cs)
	}
}

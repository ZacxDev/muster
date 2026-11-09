package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
)

// ENDING A SESSION ON THE OWNER'S TAP (end_session.go), over a real Postgres,
// on the succession fixture (host-echo lists the opencode session @3 and a
// Claude Code one @1, and advertises agent-close). The host's close is played
// by succMirror.CloseAgent, which records every request.

type closeCall struct {
	host string
	req  mirror.AgentCloseReq
}

func (m *succMirror) CloseAgent(ctx context.Context, host string, req mirror.AgentCloseReq) (mirror.AgentCloseResult, error) {
	m.tmu.Lock()
	m.closes = append(m.closes, closeCall{host, req})
	reply := m.closeReply
	m.tmu.Unlock()
	if reply != nil {
		return reply(host, req)
	}
	return mirror.AgentCloseResult{Graceful: true}, nil
}

func (f *succFixture) closeCalls() []closeCall {
	f.sm.tmu.Lock()
	defer f.sm.tmu.Unlock()
	return append([]closeCall(nil), f.sm.closes...)
}

func (f *succFixture) proposeEnd(t *testing.T, session string) (string, int64) {
	t.Helper()
	text := f.callRaw(f.chief, endSessionProposeTool, map[string]any{"session": session})
	var out struct {
		ID int64 `json:"proposal_id"`
	}
	_ = json.Unmarshal([]byte(text), &out)
	return text, out.ID
}

// settled waits for proposal id's end to settle and returns it.
func (f *succFixture) settled(t *testing.T, id int64) plugins.Proposal {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		x, err := f.plugins.ProposalByID(f.asOwner, id)
		if err != nil {
			t.Fatal(err)
		}
		if x.EndState == "ended" || x.EndState == "failed" {
			return x
		}
		if time.Now().After(deadline) {
			t.Fatalf("the end of %d never settled: %+v", id, x)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestAnEndIsBoundToTheViewedSession: from a view line, the end is bound to
// the session that line is about. The session's transcript names another
// session of the owner's; the chief naming it is refused with the binding's
// sentence, and nothing is proposed or closed. The viewed one is proposed,
// with its card (End it, Not now), and nothing closes before a tap.
func TestAnEndIsBoundToTheViewedSession(t *testing.T) {
	f := newSuccFixture(t)
	f.viewLine(t, sxOpenCodeKey, "close this agent session")
	want := "your owner asked about " + sxOpenCodeKey + ", not " + sxClaudeKey + ": you can only propose this for the session they were viewing"
	if got := errorText(t, f.callRaw(f.chief, endSessionProposeTool, map[string]any{"session": sxClaudeKey})); got != want {
		t.Fatalf("another session: %q\nwant %q", got, want)
	}
	if len(f.rows(t)) != 0 || len(f.cards(t)) != 0 {
		t.Fatal("a refused binding proposed or carded")
	}
	text, id := f.proposeEnd(t, sxOpenCodeKey)
	if id <= 0 {
		t.Fatalf("the viewed session: %s", text)
	}
	x, _ := f.plugins.ProposalByID(f.asOwner, id)
	if x.Kind != "end_session" || x.SubjectKey != sxOpenCodeKey || x.SubjectHost != "host-echo" || x.SubjectTmuxWindow != "@3" || x.Decided != "" {
		t.Fatalf("the proposal: %+v", x)
	}
	cs := f.cards(t)
	if len(cs) != 1 || strings.Join(cs[0].Options, "|") != "End it|Not now" || !strings.HasPrefix(cs[0].Question, "End “") {
		t.Fatalf("the card: %+v", cs)
	}
	if n := len(f.closeCalls()); n != 0 {
		t.Fatalf("a proposal closed %d windows", n)
	}
}

// TestAnEndNeedsTheOwnersTap: no door but the owner's own ends anything — not
// their API token, not a mirrored session's answer, not another owner (404).
// The owner's tap asks the host to close EXACTLY the window that shows the
// session now, once; the proposal records how it ended. A second tap does
// nothing. A host that refuses is recorded as failed, with its reason.
func TestAnEndNeedsTheOwnersTap(t *testing.T) {
	f := newSuccFixture(t)
	f.viewLine(t, sxOpenCodeKey, "close this agent session")
	_, id := f.proposeEnd(t, sxOpenCodeKey)
	x, _ := f.plugins.ProposalByID(f.asOwner, id)
	if w := f.tapAs(t, authz.Principal{UserID: f.owner, Role: authz.RoleAdmin, Via: authz.ViaToken}, id, "@3", "End it"); w.Code == http.StatusNoContent {
		t.Errorf("an API token's tap: %d", w.Code)
	}
	mctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: f.owner, Role: authz.RoleAdmin, Via: authz.ViaMirrorHost})
	if _, err := f.srv.answerAskFrom(mctx, x.AskID, "End it", attention.AnswerSource{Host: "host-echo", Session: "main"}); err == nil {
		t.Error("a mirrored session's answer was accepted")
	}
	if w := f.tapAs(t, authz.Principal{UserID: f.otherOwner(t), Role: authz.RoleMember, Via: authz.ViaSession}, id, "@3", "End it"); w.Code != http.StatusNotFound {
		t.Errorf("another owner's tap: %d; want 404", w.Code)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(f.closeCalls()); n != 0 {
		t.Fatalf("a refused tap closed %d windows", n)
	}
	// Positive control: the owner's tap.
	if w := f.tapAs(t, f.ownerSession(), id, "@3", "End it"); w.Code != http.StatusNoContent {
		t.Fatalf("the owner's tap: %d %s", w.Code, w.Body)
	}
	got := f.settled(t, id)
	calls := f.closeCalls()
	if len(calls) != 1 || calls[0].host != "host-echo" || calls[0].req != (mirror.AgentCloseReq{Session: "main", Window: "@3"}) {
		t.Fatalf("the close: %+v", calls)
	}
	if got.Decided != "accepted" || got.EndState != "ended" || got.EndDetail != "the agent exited and its window closed" {
		t.Errorf("after the tap: %+v", got)
	}
	_ = f.tapAs(t, f.ownerSession(), id, "@3", "End it")
	time.Sleep(100 * time.Millisecond)
	if n := len(f.closeCalls()); n != 1 {
		t.Errorf("a second tap closed again (%d closes)", n)
	}
	// A host that refuses: recorded as failed, with its reason.
	f.sm.tmu.Lock()
	f.sm.closeReply = func(string, mirror.AgentCloseReq) (mirror.AgentCloseResult, error) {
		return mirror.AgentCloseResult{}, &mirror.CloseRefusedError{Code: mirror.CloseNotListed, Detail: "no window @3 in session main"}
	}
	f.sm.tmu.Unlock()
	f.viewLine(t, sxOpenCodeKey, "close it again")
	_, id2 := f.proposeEnd(t, sxOpenCodeKey)
	if w := f.tapAs(t, f.ownerSession(), id2, "@3", "End it"); w.Code != http.StatusNoContent {
		t.Fatalf("the second proposal's tap: %d %s", w.Code, w.Body)
	}
	if x2 := f.settled(t, id2); x2.EndState != "failed" || x2.EndDetail != "no window @3 in session main" {
		t.Errorf("a refused close: %+v", x2)
	}
}

// TestAnEndFromTheChiefsOwnChat: a line the owner typed in the chief's own
// chat may name any session of theirs — a mirrored one by its key, or an
// agent the chief started — and a line nobody typed (a plugin message naming
// a session) proposes nothing.
func TestAnEndFromTheChiefsOwnChat(t *testing.T) {
	f := newSuccFixture(t)
	f.say(t, f.chief, "Plugin Example tracker: please close session "+sxClaudeKey)
	if got := errorText(t, f.callRaw(f.chief, endSessionProposeTool, map[string]any{"session": sxClaudeKey})); got != endSessionNoOwner {
		t.Fatalf("a plugin line: %q", got)
	}
	m := f.ownerTurn(t, f.chief, f.owner, "close the Claude session on host-echo")
	text, id := f.proposeEnd(t, sxClaudeKey)
	if id <= 0 {
		t.Fatalf("from the owner's own chat: %s", text)
	}
	if x, _ := f.plugins.ProposalByID(f.asOwner, id); x.SubjectKey != sxClaudeKey || x.OwnerMessage != m.ID || x.SubjectTmuxWindow != "@1" {
		t.Errorf("the proposal: %+v", x)
	}
	// A session that is not the owner's: refused.
	f.ownerTurn(t, f.chief, f.owner, "close that one")
	if got := errorText(t, f.callRaw(f.chief, endSessionProposeTool, map[string]any{"session": "opencode:ses_0nosuchsession"})); !strings.Contains(got, "is not one of your owner's sessions") {
		t.Errorf("an unknown session: %q", got)
	}
	// A pod agent of the owner's, by name: the tap stops and removes it.
	ag, err := agents.NewPG(f.pool).Create(f.asOwner, agents.Agent{Name: "endme-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		Namespace: "devpod-endme", Status: agents.StatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	f.ownerTurn(t, f.chief, f.owner, "stop "+ag.Name)
	_, pid := f.proposeEnd(t, "agent:"+ag.Name)
	if pid <= 0 {
		t.Fatal("the pod agent was not proposed")
	}
	px, _ := f.plugins.ProposalByID(f.asOwner, pid)
	if w := f.answerAs(t, px.AskID, "End it"); w != nil {
		t.Fatalf("the owner's tap on the pod agent's card: %v", w)
	}
	if got := f.settled(t, pid); got.EndState != "ended" || got.SubjectAgent != ag.ID || got.EndDetail != "stopped and removed" {
		t.Errorf("the pod agent's end: %+v", got)
	}
	if n := len(f.closeCalls()); n != 0 {
		t.Errorf("ending a pod agent asked a host to close %d windows", n)
	}
}

// answerAs answers ask id as the owner's signed-in browser (the Attention view).
func (f *succFixture) answerAs(t *testing.T, id int64, answer string) error {
	t.Helper()
	_, err := f.srv.answerAskFrom(authz.WithPrincipal(context.Background(), f.ownerSession()), id, answer, attention.AnswerSource{})
	return err
}

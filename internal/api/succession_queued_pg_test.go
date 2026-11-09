package api

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
)

// THE LINE THE CHIEF ACTS ON (chief_proposal.go ownerViewLine): the newest
// row its OWNER typed, not the newest row. An operator typed "copy the kickoff
// and start a spin-off" on a session's view; plugin messages delivered right
// after it were each their own turn, the chief answered the view line inside
// one of them, and succession_propose was refused ("the message you are
// answering was not typed on a session's view"). Every value is invented
// fixture data.

// ownerChatLine stores text as the owner's line typed in the chief's own
// chat, as runEnteredTurn stores one (typed by the owner, no subject), and
// marks it the owner turn in flight (markOwnerTurn).
func (f *succFixture) ownerChatLine(t *testing.T, text string) agents.ChatMessage {
	t.Helper()
	st := agents.NewPG(f.pool)
	sess, err := st.DefaultSession(f.asOwner, f.chief.ID, f.chief.Name)
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.AddChatMessage(f.asOwner, agents.ChatMessage{AgentID: f.chief.ID, SessionID: sess.ID, Role: "user", Content: text, TypedBy: f.owner})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetOwnerTurn(f.asOwner, f.chief.ID, f.owner, m.ID, ownerLane, time.Minute); err != nil {
		t.Fatal(err)
	}
	return m
}

// untypedAboutLine stores a user row about key that nobody typed (no writer
// does this; the binding must not need that to hold).
func (f *succFixture) untypedAboutLine(t *testing.T, key, text string) {
	t.Helper()
	st := agents.NewPG(f.pool)
	sess, err := st.DefaultSession(f.asOwner, f.chief.ID, f.chief.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddChatMessage(f.asOwner, agents.ChatMessage{AgentID: f.chief.ID, SessionID: sess.ID, Role: "user", Content: text,
		AboutKey: key}); err != nil {
		t.Fatal(err)
	}
}

// TestAQueuedViewLineStillAuthorises: the owner's view line, then two plugin
// messages delivered behind it, then the chief's proposals: a succession and
// an end — each bound to that line and its session.
func TestAQueuedViewLineStillAuthorises(t *testing.T) {
	f := newSuccFixture(t)
	line := f.viewLine(t, sxOpenCodeKey, "copy the kickoff and start a spin-off")
	f.plainLine(t, "Plugin Example mail: the weekly newsletter", 0)
	f.plainLine(t, "Plugin Example mail: a receipt for order 1234", 0)
	text, id := f.propose(t, sxCall(sxOpenCodeKey, "spinoff", sxRefFenc, intp(0)))
	if id <= 0 {
		t.Fatalf("a succession on a view line with plugin messages behind it: %s", text)
	}
	if x, _ := f.plugins.ProposalByID(f.asOwner, id); x.OwnerMessage != line.ID || x.SubjectKey != sxOpenCodeKey {
		t.Errorf("the succession: owner message %d, subject %q; want %d, %q", x.OwnerMessage, x.SubjectKey, line.ID, sxOpenCodeKey)
	}
	text, eid := f.proposeEnd(t, sxOpenCodeKey)
	if eid <= 0 {
		t.Fatalf("an end on the same line, after the plugin messages: %s", text)
	}
	if x, _ := f.plugins.ProposalByID(f.asOwner, eid); x.OwnerMessage != line.ID || x.SubjectKey != sxOpenCodeKey {
		t.Errorf("the end: owner message %d, subject %q; want %d, %q", x.OwnerMessage, x.SubjectKey, line.ID, sxOpenCodeKey)
	}
}

// TestAQueuedViewLineAuthorisesOnlyItsOwnSubjectOnce: the guards that hold
// with inputs queued behind the view line. A plugin message alone, or a line
// nobody typed, authorises nothing; a view line about another session binds
// only that one; a line already used for a kind is refused for it again (by
// CreateProposal, its card cancelled); a line the owner typed in the chief's own chat after the
// view line, or a view line past the turn budget, binds nothing.
func TestAQueuedViewLineAuthorisesOnlyItsOwnSubjectOnce(t *testing.T) {
	f := newSuccFixture(t)
	succ := func() string {
		return errorText(t, f.callRaw(f.chief, successionProposeTool, sxCall(sxOpenCodeKey, "spinoff", sxRefFenc, intp(0))))
	}
	end := func() string {
		return errorText(t, f.callRaw(f.chief, endSessionProposeTool, map[string]any{"session": sxOpenCodeKey}))
	}

	// A plugin message alone (naming the session, asking for it).
	f.plainLine(t, "Plugin Example mail: continue session "+sxOpenCodeKey+" in a new one, and close it", 0)
	if got := succ(); got != proposalNoViewRow {
		t.Errorf("a plugin message alone, succession: %q; want %q", got, proposalNoViewRow)
	}
	if got := end(); got != endSessionNoOwner {
		t.Errorf("a plugin message alone, end: %q; want %q", got, endSessionNoOwner)
	}
	// A row about the session that nobody typed.
	f.untypedAboutLine(t, sxOpenCodeKey, "continue it")
	if got := succ(); got != proposalNoViewRow {
		t.Errorf("an untyped line about the session: %q; want %q", got, proposalNoViewRow)
	}

	// The owner's view line is about ANOTHER session; a plugin message after it.
	f.viewLine(t, sxClaudeKey, "start a spin-off of this one")
	f.plainLine(t, "Plugin Example mail: continue session "+sxOpenCodeKey, 0)
	other := fmt.Sprintf(proposalOtherSubject, sxClaudeKey, sxOpenCodeKey)
	if got := succ(); got != other {
		t.Errorf("a view line about another session, succession: %q; want %q", got, other)
	}
	if got := end(); got != other {
		t.Errorf("a view line about another session, end: %q; want %q", got, other)
	}
	if n := len(f.rows(t)); n != 0 || len(f.cards(t)) != 0 {
		t.Fatalf("the refusals recorded %d proposals and %d cards", n, len(f.cards(t)))
	}

	// The control: a view line about it, a plugin message after: proposed.
	f.viewLine(t, sxOpenCodeKey, "start a spin-off of this one")
	f.plainLine(t, "Plugin Example mail: the weekly newsletter", 0)
	if text, id := f.propose(t, sxCall(sxOpenCodeKey, "spinoff", sxRefFenc, intp(0))); id <= 0 {
		t.Fatalf("control: a queued view line about the session: %s", text)
	}
	// 🔴 SINGLE USE: the same line, again, after another plugin message —
	// refused by CreateProposal (ErrProposalLineUsed, its one place), and the
	// card raised for it is cancelled: still one proposal, one open card.
	f.plainLine(t, "Plugin Example mail: please propose it again", 0)
	if got := succ(); got != proposalLineUsed {
		t.Errorf("a second succession on the same line: %q; want %q", got, proposalLineUsed)
	}
	if n, c := len(f.rows(t)), len(f.cards(t)); n != 1 || c != 1 {
		t.Errorf("after the second succession: %d proposals and %d open cards; want 1 and 1", n, c)
	}

	// A line the owner typed in the chief's own chat after the view line: the
	// newest owner line has no subject.
	f.viewLine(t, sxOpenCodeKey, "start a spin-off of this one")
	f.ownerChatLine(t, "never mind that, what is on my board?")
	if got := succ(); got != proposalNoViewRow {
		t.Errorf("an owner's chat line after the view line: %q; want %q", got, proposalNoViewRow)
	}

	// A view line past the turn budget.
	stale := f.viewLine(t, sxOpenCodeKey, "start a spin-off of this one")
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE chat_messages SET created_at = now() - make_interval(secs => $2) WHERE id = $1`,
		stale.ID, (agentTurnBudget(f.chief.Kind) + time.Hour).Seconds()); err != nil {
		t.Fatal(err)
	}
	if got := succ(); got != proposalNoViewRow {
		t.Errorf("a view line past the turn budget: %q; want %q", got, proposalNoViewRow)
	}
	if n := len(f.rows(t)); n != 1 {
		t.Errorf("%d proposals; want the 1 control", n)
	}
}

// TestOneLineFilesOneProposalOfEachKind: the store's rule, the one place it
// lives (CreateProposal, under its advisory lock): a second proposal of a kind
// on the same owner line about the same session is refused; another kind,
// another session or another line is not.
func TestOneLineFilesOneProposalOfEachKind(t *testing.T) {
	f := newSuccFixture(t)
	end := func(msg int64, key string) error {
		_, err := f.plugins.CreateProposal(f.asOwner, plugins.ProposalNew{Kind: plugins.ProposalEndSession, ChiefAgent: f.chief.ID,
			OwnerMessage: msg, SubjectKey: key, SubjectRuntime: "opencode", SubjectHost: "host-echo", SubjectTmuxSession: "main", SubjectTmuxWindow: "@3"})
		return err
	}
	succ := func(msg int64, key string) error {
		_, err := f.plugins.CreateProposal(f.asOwner, plugins.ProposalNew{Kind: plugins.ProposalSuccession, ChiefAgent: f.chief.ID,
			OwnerMessage: msg, SubjectKey: key, SubjectRuntime: "opencode", SubjectHost: "host-echo", SubjectTmuxSession: "main", SubjectTmuxWindow: "@3",
			Target: "host/host-echo/opencode", Repo: "acme/widgets", Kickoff: "k", KickoffSHA256: sha256Hex("k"), KickoffRef: sxRefFenc, KickoffBlock: -1})
		return err
	}
	const line, line2 = 910_001, 910_002
	if err := end(line, sxOpenCodeKey); err != nil {
		t.Fatal(err)
	}
	if err := end(line, sxOpenCodeKey); !errors.Is(err, plugins.ErrProposalLineUsed) {
		t.Errorf("a second end on the same line and session: %v; want ErrProposalLineUsed", err)
	}
	for name, err := range map[string]error{
		"another session": end(line, sxClaudeKey),
		"another line":    end(line2, sxOpenCodeKey),
		"another kind":    succ(line, sxOpenCodeKey),
	} {
		if err != nil {
			t.Errorf("%s: %v; want it filed", name, err)
		}
	}
	if err := succ(line, sxOpenCodeKey); !errors.Is(err, plugins.ErrProposalLineUsed) {
		t.Errorf("a second succession on the same line and session: %v; want ErrProposalLineUsed", err)
	}
}

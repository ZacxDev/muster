package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE CHIEF'S PROPOSALS (docs/design/chief-in-agent-view.md, phase 2): ONE
// primitive for an action the chief may PREPARE about one of the owner's
// sessions but never take — a succession now (succession.go), ending a
// session next. Every kind shares, by construction:
//
//   - ONE BINDING RULE (bindViewSubject): the proposal is about the session the
//     owner's view line is about (chat_messages.about of the newest user row
//     the chief is answering), and the session the chief names must be that
//     one. Transcript text names nothing here.
//   - ONE RECORD AND CARD (fileProposal): a chief_proposals row of its kind,
//     and an owner card (raiseOwnerCard: the view, the Attention view, the
//     phone) whose buttons are the kind's options and "Not now".
//   - ONE TAP (proposalForAnswer, answerAskFrom's hook, BEFORE the answer is
//     recorded): only the owner's own door (their signed-in browser or paired
//     phone), only one of the card's buttons, only an undecided proposal. Not
//     now declines; any other button is the kind's accept (acceptProposal). An
//     accept refused before it did anything leaves the card OPEN, with its
//     sentence, as a refused gate release does (answer_release.go).
//   - ONE TAP ROUTE on the view (POST /mirror/view/proposal), which takes only
//     the card of the session that view shows and goes through that path.
// ---------------------------------------------------------------------------

// The binding's refusals (the chief reads them), each its own sentence.
const (
	proposalNoViewRow = "this needs your owner to have asked from that session's view: the message you are answering was not " +
		"typed on a session's view (or is too old), so there is no session to propose it for"
	proposalOtherSubject = "your owner asked about %s, not %s: you can only propose this for the session they were viewing"
	proposalNotVisible   = "session %s is not visible now: the session's transcript can no longer be read"
	proposalNoCard       = "the card could not be put in front of your owner (%s): nothing was proposed"
)

// The tap's refusals, as the owner reads them (the view's card, the Attention
// view, the phone's "Not sent: …").
const (
	proposalNotOwnersDoor  = "only you can accept your chief's proposal, tapping its card in your own signed-in browser or on your paired phone"
	proposalNotATap        = "tap one of the card's buttons: nothing happens on a typed answer, and Not now declines it"
	proposalDecidedAlready = "this proposal was already accepted or declined"
	proposalAcceptFailed   = "it could not be done: %s"
)

// proposalDeclineLabel is every card's last button.
const proposalDeclineLabel = "Not now"

// EventLineageChanged names a proposal's change on the owner's stream: filed,
// declined, accepted, and a succession's successor starting, running or
// ending. The id is proposalEventID; the event carries no text.
const EventLineageChanged = "lineage.changed"

// proposalEventID is "<kind>-<id>" ("succession-12").
func proposalEventID(x plugins.Proposal) string { return x.Kind + "-" + strconv.FormatInt(x.ID, 10) }

// proposalStore is the plugin store's proposal half (found by assertion, as
// hostDispatchStore is).
type proposalStore interface {
	CreateProposal(ctx context.Context, n plugins.ProposalNew) (plugins.Proposal, error)
	ProposalByID(ctx context.Context, id int64) (plugins.Proposal, error)
	ProposalByAsk(ctx context.Context, askID int64) (plugins.Proposal, error)
	LatestProposalFor(ctx context.Context, kind, key string) (plugins.Proposal, error)
	SuccessionForDispatch(ctx context.Context, hostDispatchID int64) (plugins.Proposal, error)
	SupersededKeys(ctx context.Context) (map[string]bool, error)
	DeclineProposal(ctx context.Context, id int64) error
	MarkSuperseded(ctx context.Context, id int64) (bool, error)
	SetSuccessionNotified(ctx context.Context, id int64) (bool, error)
	SetSuccessorSeen(ctx context.Context, id int64) error
}

var _ proposalStore = (*plugins.PGStore)(nil)

// proposals is the store's proposal half, nil when it has none.
func (s *Server) proposals() proposalStore {
	if s.ext.ChiefDispatch == nil {
		return nil
	}
	st, _ := s.ext.ChiefDispatch.(proposalStore)
	return st
}

// viewLineFor is the owner's view line chief is answering: the NEWEST user row
// of its default session, when the owner typed it on a session's view
// (AboutKey, TypedBy the owner) within the turn budget; else the refusal.
//
// 🔴 THE NEWEST ROW, NOT THE NEWEST VIEW ROW: a plugin delivery, a relayed
// answer or a line typed in the chief's own chat after the view line is the
// turn being answered now, and it carries no subject.
func (s *Server) viewLineFor(ctx context.Context, chief agents.Agent) (agents.ChatMessage, string) {
	sess, err := s.defaultSession(ctx, chief)
	if err != nil {
		s.logger.Printf("proposal: %s's default session: %v", chief.Name, err)
		return agents.ChatMessage{}, proposalNoViewRow
	}
	msgs, err := s.ext.Agents.ListRecentChatMessages(ctx, sess.ID, chatHistoryCap)
	if err != nil {
		s.logger.Printf("proposal: read %s's chat: %v", chief.Name, err)
		return agents.ChatMessage{}, proposalNoViewRow
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role != "user" {
			continue
		}
		owner := agentOwnerID(chief)
		if m.AboutKey == "" || owner <= 0 || m.TypedBy != owner || s.now().Sub(m.CreatedAt) > agentTurnBudget(chief.Kind) {
			return agents.ChatMessage{}, proposalNoViewRow
		}
		return m, ""
	}
	return agents.ChatMessage{}, proposalNoViewRow
}

// bindViewSubject is THE BINDING RULE: the view line chief is answering, the
// window that shows its subject now and that subject — when claimed, the
// session key the chief named (a "mirror:" prefix allowed), is exactly the
// line's subject. Anything else is the refusal.
//
// 🔴 EVERYTHING AFTER IS THE LINE'S, NEVER THE CLAIM'S: the window is found by
// the line's key, through the owner-scoped hub.
func (s *Server) bindViewSubject(ctx context.Context, chief agents.Agent, claimed string) (agents.ChatMessage, mirror.SessionView, chatSubject, string) {
	line, why := s.viewLineFor(ctx, chief)
	if why != "" {
		return agents.ChatMessage{}, mirror.SessionView{}, chatSubject{}, why
	}
	key := strings.TrimPrefix(strings.TrimSpace(claimed), string(focus.KindMirror)+":")
	if key != line.AboutKey {
		return agents.ChatMessage{}, mirror.SessionView{}, chatSubject{}, fmt.Sprintf(proposalOtherSubject, line.AboutKey, key)
	}
	e, ok := s.mirrorFocusEntryFor(ctx, line.AboutKey)
	if !ok {
		return agents.ChatMessage{}, mirror.SessionView{}, chatSubject{}, fmt.Sprintf(proposalNotVisible, line.AboutKey)
	}
	return line, e, s.sessionSubject(ctx, e), ""
}

// fileProposal records proposal n (its kind's fields set by the caller) about
// subject about, answering view line line, and raises its card with question
// and the kind's options followed by Not now: the ONE way a proposal is made.
func (s *Server) fileProposal(ctx context.Context, chief agents.Agent, line agents.ChatMessage, about chatSubject, n plugins.ProposalNew,
	question string, options []string) (plugins.Proposal, string) {
	st := s.proposals()
	if st == nil || s.ext.Attention == nil {
		return plugins.Proposal{}, "proposals are not available on this muster"
	}
	ask, err := s.raiseOwnerCard(ctx, chief, question, append(append([]string(nil), options...), proposalDeclineLabel))
	if err != nil {
		return plugins.Proposal{}, fmt.Sprintf(proposalNoCard, ownerCardRefusal(err))
	}
	n.ChiefAgent, n.OwnerMessage, n.AskID = chief.ID, line.ID, ask.ID
	n.SubjectKey, n.SubjectRuntime, n.SubjectHost = line.AboutKey, about.Runtime, about.Host
	n.SubjectTmuxSession, n.SubjectTmuxWindow, n.SubjectTitle, n.SubjectDispatch = about.TmuxSession, about.TmuxWindow, cutRunes(about.Title, 300), about.Dispatch
	x, err := st.CreateProposal(ctx, n)
	if err != nil {
		s.logger.Printf("proposal: record %s's %s for %s: %v", chief.Name, n.Kind, line.AboutKey, err)
		_, _ = s.cancelAsk(ctx, ask.ID)
		return plugins.Proposal{}, "the proposal could not be recorded; try again"
	}
	s.logger.Printf("proposal %d: %s proposed a %s of %s (card %d)", x.ID, chief.Name, x.Kind, x.SubjectKey, ask.ID)
	s.broadcastTo(agentOwnerID(chief), EventLineageChanged, proposalEventID(x))
	return x, ""
}

// proposalForAnswer is THE TAP (answerAskFrom, BEFORE the answer is
// recorded); a nil error when ask id is no proposal's card. On a card the
// answer must come through the owner's own door, be one of its buttons and
// find the proposal undecided; Not now declines it, any other button is the
// kind's accept. An accept refused before it did anything leaves the card
// OPEN, with its sentence.
func (s *Server) proposalForAnswer(ctx context.Context, id int64, answer string, src attention.AnswerSource) error {
	st := s.proposals()
	if st == nil || s.ext.Attention == nil {
		return nil
	}
	a, err := s.ext.Attention.Get(ctx, id)
	if err != nil || a.Status != attention.Open {
		return nil // the store's Answer says what is wrong with the ask itself
	}
	x, err := st.ProposalByAsk(ctx, id)
	if err != nil {
		return nil
	}
	// 🔴 THE OWNER'S OWN DOOR: a signed-in browser or the paired phone, as the
	// card's owner. A mirrored session's attention_answer, a token, a plugin:
	// refused before anything is recorded or done.
	if src.ViaSession() || !attention.IsOwnerDoor(ctx, a.Owner) {
		return errReleaseRefused(proposalNotOwnersDoor)
	}
	if x.Decided != plugins.ProposalOpen {
		return errReleaseRefused(proposalDecidedAlready)
	}
	if !proposalOptionOf(a, answer) {
		return errReleaseRefused(proposalNotATap)
	}
	if sameWords(answer) == proposalDeclineLabel {
		if err := st.DeclineProposal(ctx, x.ID); err != nil {
			return errReleaseRefused(proposalDecidedAlready)
		}
		s.logger.Printf("proposal %d (%s): declined by its owner", x.ID, x.Kind)
		s.broadcastTo(a.Owner, EventLineageChanged, proposalEventID(x))
		return nil
	}
	if why := s.acceptProposal(ctx, x, sameWords(answer)); why != "" {
		return errReleaseRefused(fmt.Sprintf(proposalAcceptFailed, why))
	}
	return nil
}

// acceptProposal is the kind's accept on the owner's tap of button choice
// (one of the card's own options, not Not now): "" when it was done.
func (s *Server) acceptProposal(ctx context.Context, x plugins.Proposal, choice string) string {
	switch x.Kind {
	case plugins.ProposalSuccession:
		return s.acceptSuccession(ctx, x, choice)
	}
	return "muster cannot do a " + x.Kind + " yet"
}

// proposalOptionOf reports whether answer is one of ask a's own options.
func proposalOptionOf(a attention.Ask, answer string) bool {
	for _, o := range a.Options {
		if sameWords(o) == sameWords(answer) {
			return true
		}
	}
	return false
}

// handleProposalTap is POST /mirror/view/proposal (id, choice, host, session,
// window): the owner's tap on a proposal card of the view at that address.
// 🔴 THE CARD MUST BE THAT VIEW'S: the proposal is about the session the
// window shows now (its subject_key), read as the principal; anything else is
// not found. The tap then goes through the ONE answer path (answerAskFrom),
// whose proposal hook does what was proposed.
func (s *Server) handleProposalTap(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "the request is too large", http.StatusRequestEntityTooLarge)
		return
	}
	st := s.proposals()
	_, e, _, _, ok := s.mirrorEntry(r, r.PostFormValue("host"), r.PostFormValue("session"), r.PostFormValue("window"))
	id, _ := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	if !ok || st == nil || s.ext.Attention == nil || id <= 0 || e.SessionKey() == "" {
		http.NotFound(w, r)
		return
	}
	x, err := st.ProposalByID(ctx, id)
	if err != nil || x.SubjectKey != e.SessionKey() || x.AskID <= 0 {
		http.NotFound(w, r)
		return
	}
	nav := isDocumentNavigation(r)
	back := mirrorRef(e).ViewPath()
	a, err := s.answerAskFrom(ctx, x.AskID, r.PostFormValue("choice"), attention.AnswerSource{})
	if err != nil {
		if attentionErrCode(err) == http.StatusInternalServerError {
			s.logger.Printf("proposal %d: the tap: %v", x.ID, err)
		}
		if nav {
			http.Redirect(w, r, withNotice(back, noticeFailed), http.StatusSeeOther)
			return
		}
		http.Error(w, attentionErrText(err, a), attentionErrCode(err))
		return
	}
	if nav {
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// proposalCardView is proposal x's card as a view draws it: the kind's
// heading, settings and body, and its buttons.
func proposalCardView(x plugins.Proposal, heading string, settings [][2]string, bodyLabel, body string, options []string) ui.ProposalView {
	return ui.ProposalView{ID: x.ID, Kind: x.Kind, Heading: heading, Settings: settings, BodyLabel: bodyLabel, Body: body,
		Options: append(append([]string(nil), options...), proposalDeclineLabel)}
}

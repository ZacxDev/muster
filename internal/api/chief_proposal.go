package api

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/fence"
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
	LatestProposalAbout(ctx context.Context, key string) (plugins.Proposal, error)
	StartEnd(ctx context.Context, id int64) error
	RecordEnd(ctx context.Context, id int64, ended bool, detail string) error
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

// proposalBody is what an owner's proposal card draws under its question on
// the Attention view and the ask page, read from the proposal ROW (never from
// the ask): for a succession, its kickoff.
type proposalBody struct {
	label, text string
	// cut: text is cut, and the whole is on the session's view.
	cut bool
}

// proposalKickoffBody is ask a's proposal body for ctx's principal, nil when
// a is no proposal's card (or the principal is not its owner: the store reads
// the principal's own proposals only). 🔴 THE ONE PLACE OFF THE SESSION'S VIEW
// THAT THE KICKOFF IS READ FOR DRAWING, and only for the owner's own pages
// (withAskBoxes, renderAskPage): the ask's question never holds it
// (successionCardQuestion), so attention_list, attention_wait, the push and
// an answer's delivery cannot hand it to anyone.
func (s *Server) proposalKickoffBody(ctx context.Context, a attention.Ask) *proposalBody {
	st := s.proposals()
	if st == nil || !a.OwnerCard || a.Asker.AgentID <= 0 || a.Owner != requestOwner(ctx) {
		return nil
	}
	x, err := st.ProposalByAsk(ctx, a.ID)
	if err != nil || x.Kind != plugins.ProposalSuccession {
		return nil
	}
	b := &proposalBody{label: "Kickoff · " + strconv.Itoa(len(x.Kickoff)) + " bytes, copied exactly from the session", text: x.Kickoff}
	if !successionKickoffFitsCard(x) {
		b.text, b.cut = cutRunes(x.Kickoff, successionCardKickoffRoom(x)), true
	}
	return b
}

// isProposalCard reports whether ask a is one of ctx's principal's chief
// proposal cards.
func (s *Server) isProposalCard(ctx context.Context, a attention.Ask) bool {
	st := s.proposals()
	if st == nil || !a.OwnerCard || a.Asker.AgentID <= 0 {
		return false
	}
	_, err := st.ProposalByAsk(ctx, a.ID)
	return err == nil
}

// proposalAnsweredText is a proposal card's answer as the Answered list and
// the ask page say it (#546 round 2, G1): recorded, and never delivered to
// the chief, which muster told in its own words.
const proposalAnsweredText = "muster recorded your answer. It is not sent to your chief: muster told it what you decided in its own words."

// proposalBodyHTML is b as the Attention view's card and the ask page draw it
// ("" for nil).
func proposalBodyHTML(b *proposalBody) string {
	if b == nil {
		return ""
	}
	esc := html.EscapeString
	// The classes are ui's (the stylesheet scans internal/ui, not this file).
	out := `<div class="` + ui.ProposalBodyClass + `" data-attention-proposal>
<p class="` + ui.ProposalBodyNoteClass + `">` + esc(b.label) + `</p>
<pre class="` + ui.ProposalKickoffClass + `" data-attention-proposal-kickoff>` + esc(b.text) + `</pre>
`
	if b.cut {
		out += `<p class="` + ui.ProposalBodyNoteClass + `" data-attention-proposal-cut>` + esc(successionCutNote) + `</p>
`
	}
	return out + "</div>\n"
}

// successionCutNote is a cut kickoff's line under it off the view.
const successionCutNote = "Cut here: the whole kickoff is on the session's view, and it starts from there."

// fileProposal records proposal n (its kind's fields set by the caller) about
// the session keyed key (subject about), answering the owner's message
// ownerMsg, and raises its card with question and askOptions (the buttons the
// Attention view and the phone offer, Not now included when offered): the ONE
// way a proposal is made. The caller has bound key (bindViewSubject, or the
// owner's own typed turn).
func (s *Server) fileProposal(ctx context.Context, chief agents.Agent, ownerMsg int64, key string, about chatSubject, n plugins.ProposalNew,
	question string, askOptions []string) (plugins.Proposal, string) {
	st := s.proposals()
	if st == nil || s.ext.Attention == nil {
		return plugins.Proposal{}, "proposals are not available on this muster"
	}
	ask, err := s.raiseProposalCard(ctx, chief, question, askOptions)
	if err != nil {
		return plugins.Proposal{}, fmt.Sprintf(proposalNoCard, ownerCardRefusal(err))
	}
	n.ChiefAgent, n.OwnerMessage, n.AskID = chief.ID, ownerMsg, ask.ID
	n.SubjectKey, n.SubjectRuntime, n.SubjectHost = key, about.Runtime, about.Host
	n.SubjectTmuxSession, n.SubjectTmuxWindow, n.SubjectTitle, n.SubjectDispatch = about.TmuxSession, about.TmuxWindow, cutRunes(about.Title, 300), about.Dispatch
	x, err := st.CreateProposal(ctx, n)
	if err != nil {
		s.logger.Printf("proposal: record %s's %s for %s: %v", chief.Name, n.Kind, key, err)
		_, _ = s.cancelAsk(ctx, ask.ID)
		return plugins.Proposal{}, "the proposal could not be recorded; try again"
	}
	s.logger.Printf("proposal %d: %s proposed a %s of %s (card %d)", x.ID, chief.Name, x.Kind, x.SubjectKey, ask.ID)
	s.broadcastTo(agentOwnerID(chief), EventLineageChanged, proposalEventID(x))
	return x, ""
}

// proposalNote is what muster tells the chief once its proposal was decided:
// its own sentence, never the card's question or answer (which carry the
// copied kickoff, raw session text).
type proposalNote struct {
	chief int64
	text  string
}

// proposalForAnswer is THE TAP (answerAskFrom, BEFORE the answer is
// recorded); nil, nil when ask id is no proposal's card. On a card the answer
// must come through the owner's own door, be one of its buttons TAPPED as a
// button (optionTapped: never free text that spells one), come from a surface
// that showed the proposal whole (proposalSurfaceRefusal) and find the
// proposal undecided; Not now declines it, any other button is the kind's
// accept. An accept refused before it did anything leaves the card OPEN,
// with its sentence. The note is muster's sentence for the chief
// (noteProposalToChief): the answer itself is never delivered.
func (s *Server) proposalForAnswer(ctx context.Context, id int64, answer string, src attention.AnswerSource) (*proposalNote, error) {
	st := s.proposals()
	if st == nil || s.ext.Attention == nil {
		return nil, nil
	}
	a, err := s.ext.Attention.Get(ctx, id)
	if err != nil || a.Status != attention.Open {
		return nil, nil // the store's Answer says what is wrong with the ask itself
	}
	x, err := st.ProposalByAsk(ctx, id)
	if err != nil {
		return nil, nil
	}
	// 🔴 THE OWNER'S OWN DOOR: a signed-in browser or the paired phone, as the
	// card's owner. A mirrored session's attention_answer, a token, a plugin:
	// refused before anything is recorded or done.
	if src.ViaSession() || !attention.IsOwnerDoor(ctx, a.Owner) {
		return nil, errReleaseRefused(proposalNotOwnersDoor)
	}
	if x.Decided != plugins.ProposalOpen {
		return nil, errReleaseRefused(proposalDecidedAlready)
	}
	// 🔴 A BUTTON, TAPPED: free text that spells a button accepts nothing.
	if !optionTapped(ctx) {
		return nil, errReleaseRefused(proposalNotATap)
	}
	// What the surface the answer came from may do: a card it could not show
	// whole is not accepted there.
	if why := proposalSurfaceRefusal(ctx, x, answer); why != "" {
		return nil, errReleaseRefused(why)
	}
	// One of the card's buttons: the ask's own, or — tapped on the session's
	// view (handleProposalTap) — one of the view card's.
	if !proposalOptionOf(a, answer) && !(fromProposalView(ctx) && slices.Contains(proposalViewOptions(x), strings.TrimSpace(answer))) {
		return nil, errReleaseRefused(proposalNotATap)
	}
	choice := strings.TrimSpace(answer)
	if choice == proposalDeclineLabel {
		if err := st.DeclineProposal(ctx, x.ID); err != nil {
			return nil, errReleaseRefused(proposalDecidedAlready)
		}
		s.logger.Printf("proposal %d (%s): declined by its owner", x.ID, x.Kind)
		s.broadcastTo(a.Owner, EventLineageChanged, proposalEventID(x))
		return &proposalNote{chief: x.ChiefAgent, text: proposalDecisionNote(x, false)}, nil
	}
	if why := s.acceptProposal(ctx, x, choice); why != "" {
		return nil, errReleaseRefused(fmt.Sprintf(proposalAcceptFailed, why))
	}
	return &proposalNote{chief: x.ChiefAgent, text: proposalDecisionNote(x, true)}, nil
}

// proposalDecisionNote is muster's sentence to the chief about proposal x's
// decision: its kind, its id and the session key — no kickoff, no card text.
func proposalDecisionNote(x plugins.Proposal, accepted bool) string {
	what := "your proposal " + strconv.FormatInt(x.ID, 10) + " (" + x.Kind + " of " + fence.Sanitize(x.SubjectKey) + ")"
	if !accepted {
		return "[muster] Your owner declined " + what + ". Nothing was done. Do not propose it again unless they ask."
	}
	if x.Kind == plugins.ProposalSuccession {
		return "[muster] Your owner tapped Start on " + what + ": muster is starting the successor and their view follows it. " +
			"Nothing more is needed from you."
	}
	return "[muster] Your owner accepted " + what + ": muster is doing it. Nothing more is needed from you."
}

// noteProposalToChief writes n as a note in the chief's chat (writeChiefNote:
// a user row, no turn), read as the card's owner.
func (s *Server) noteProposalToChief(ctx context.Context, n proposalNote) {
	if s.ext.Agents == nil || n.chief <= 0 {
		return
	}
	chief, err := s.ext.Agents.Get(ctx, n.chief)
	if err != nil {
		s.logger.Printf("proposal: the chief %d to tell: %v", n.chief, err)
		return
	}
	if err := s.writeChiefNote(context.WithoutCancel(ctx), chief, n.text); err != nil {
		s.logger.Printf("proposal: note to the chief: %v", err)
	}
}

// optionTapKey marks an answer given by TAPPING one of a card's buttons (the
// Attention view's option button, the view's proposal card), never free text.
type optionTapKey struct{}

func withOptionTap(ctx context.Context) context.Context {
	return context.WithValue(ctx, optionTapKey{}, true)
}

func optionTapped(ctx context.Context) bool {
	v, _ := ctx.Value(optionTapKey{}).(bool)
	return v
}

// proposalViewKey marks an answer given by a tap on the proposal card of the
// session's own view (handleProposalTap: the owner's signed-in session, the
// card of the session that view shows).
type proposalViewKey struct{}

func withProposalView(ctx context.Context) context.Context {
	return context.WithValue(ctx, proposalViewKey{}, true)
}

func fromProposalView(ctx context.Context) bool {
	v, _ := ctx.Value(proposalViewKey{}).(bool)
	return v
}

// proposalViewOptions is the buttons proposal x's card has on the session's
// view (which shows it whole), Not now included.
func proposalViewOptions(x plugins.Proposal) []string {
	switch x.Kind {
	case plugins.ProposalSuccession:
		return append(successionCardOptions(x.Mode), proposalDeclineLabel)
	}
	return nil
}

// proposalSurfaceRefusal is why answer cannot be taken from the surface it
// came from, "" when it can. 🔴 A SUCCESSION WHOSE KICKOFF DOES NOT FIT ITS
// CARD WHOLE starts only from the session's view (operator decision, #546
// round 0 Q1): off the view, a start — and the review button itself — is
// refused, and the card stays open.
func proposalSurfaceRefusal(ctx context.Context, x plugins.Proposal, answer string) string {
	if x.Kind != plugins.ProposalSuccession || fromProposalView(ctx) {
		return ""
	}
	switch strings.TrimSpace(answer) {
	case successionReviewLabel:
		return successionReviewOnView
	case successionStartLabel, successionReplaceLabel, successionSpinoffLabel:
		// 🔴 NEVER FROM THE PHONE (round 1, 🟡2): its notification shows the
		// card's question, not the kickoff the start would run. DEFENCE IN
		// DEPTH, unreachable in production today (round 2, G2): the only
		// doors that mark an answer a tapped button (withOptionTap) are the
		// Attention view's form and the view's tap route, both behind a
		// signed-in browser session (requireSession), so a phone's answer
		// is refused by optionTapped above — and a proposal card's push
		// carries no chips or signed actions anyway (proposalCardFiled).
		// This keeps a start off the phone should either of those change.
		if p, ok := authz.From(ctx); ok && p.Via == authz.ViaDevice {
			return successionReviewOnView
		}
		if !successionKickoffFitsCard(x) {
			return successionReviewOnView
		}
	}
	return ""
}

// acceptProposal is the kind's accept on the owner's tap of button choice
// (one of the card's own options, not Not now): "" when it was done.
func (s *Server) acceptProposal(ctx context.Context, x plugins.Proposal, choice string) string {
	switch x.Kind {
	case plugins.ProposalSuccession:
		return s.acceptSuccession(ctx, x, choice)
	case plugins.ProposalEndSession:
		return s.acceptEndSession(ctx, x, choice)
	}
	return "muster cannot do a " + x.Kind + " yet"
}

// proposalOptionOf reports whether answer is exactly one of ask a's own
// options (a button's value, as its button sends it).
func proposalOptionOf(a attention.Ask, answer string) bool {
	return slices.Contains(a.Options, strings.TrimSpace(answer))
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
	a, err := s.answerAskFrom(withOptionTap(withProposalView(ctx)), x.AskID, r.PostFormValue("choice"), attention.AnswerSource{})
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

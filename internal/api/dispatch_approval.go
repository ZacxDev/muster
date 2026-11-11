package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/fence"
	"github.com/ZacxDev/muster/internal/github"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
)

// ---------------------------------------------------------------------------
// APPROVE BY TAP: agent_dispatch outside a turn the owner typed.
//
// 🔴 WHY IT EXISTS (measured in production). The owner asked the chief, in its
// chat, to start an agent on a host; the chief asked them back with cards and
// they answered on the Attention view. Those answers arrive as turns muster
// delivers (typed_by NULL), so ownerTurnFor refused every call after them and
// the start could never pass — the one tap path (a held card) held a blank
// host or model, never "go ahead". Now a dispatch that needs the owner and is
// NOT in their typed turn files an APPROVAL card instead of being refused: the
// held-dispatch mechanism (fileHeldDispatch / completeHeld, request_key) with
// one more blank, the last one: "approve" — or "repo", when the repository is
// blank or "?" — shown with EVERYTHING that would start.
//
// 🔴 WHAT MAKES A TAP THE OWNER'S, unchanged from the host and model cards:
//   - it is decided from the STORED rows only (the hold, the card's attention
//     ask), never from what the chief says about the answer;
//   - the answer must come through an owner's door — their browser or phone
//     (AnswerSource.OwnerDoor) — never a mirrored session's attention_answer
//     (agentDispatchHeldDoor);
//   - every other argument must be the hold's, byte for byte (held_hash,
//     agentDispatchHeldChanged): the target, the task, the repository unless
//     it was the blank, the base, the model and THE BRIEF;
//   - it is single use: the reservation that starts names the hold, under the
//     owner's reservation lock (plugins.DispatchHold);
//   - a repository the owner picks is checked as connected on the call that
//     completes it, like any other (connectedRepoName).
//
// 🔴 THE BRIEF THE AGENT GETS IS THE BRIEF THE OWNER APPROVED. ownerAnswer
// (task_routing.go) exists so that, without the owner's typed turn, the text
// an agent is told is the OWNER's — the chief writes its brief while reading
// outside text (a plugin message, a task), and that text must not become an
// agent's instructions on the chief's word alone. Its rule made the chief
// replace a detailed brief with the owner's one-line answer. The card keeps
// the rule's intent another way: the chief's brief is shown on the card IN
// FULL (a brief that does not fit the card is refused, never cut), the hash
// binds it, and the agent is told it is a brief its owner approved on a card
// (renderApprovedBrief) — not one they typed.
//
// ⚠ AN UNTYPED TURN CAN NOW PUT A CARD IN FRONT OF THE OWNER (a plugin
// message's turn included). It always could with ask_owner; this card says it
// was not typed in the chat, shows the whole request, and starts nothing
// without the tap. The Attention store's per-owner cap bounds how many are
// open.
// ---------------------------------------------------------------------------

// The approval card's blanks (heldDetail.Blanks): always the LAST card, so
// everything it shows is settled.
const (
	blankApprove = "approve"
	blankRepo    = "repo"
)

// The approval card's options and their values.
const (
	approveLabel    = "Approve and start"
	changeRepoLabel = "Change repo"
	declineLabel    = "Don't start"
	approveValue    = "approve"
	changeRepoValue = "change-repo"
	declineValue    = "decline"
	repoValuePref   = "repo:"
	// repoCardMax bounds the repositories a repo card offers (the owner can
	// type any other: owner/name).
	repoCardMax = 8
)

// The approval refusals and notes.
const (
	agentDispatchApproveKey = "a dispatch outside a message your owner typed in your chat needs a request_key: mint one for this " +
		"request and muster asks your owner to approve it on a card; pass the same key when you call again"
	agentDispatchApproveGated = "task %d is gated: a card does not release a gate. Ask your owner to release it, or to tell you " +
		"to start it, typing in your chat"
	agentDispatchApproveTooLong = "your prompt is too long to show your owner whole on an approval card (the card holds %d characters, " +
		"this one needs %d): shorten it, or ask your owner to type the request in your chat"
	agentDispatchHeldDeclined = "your owner declined this dispatch on its card: do not start it"
	agentDispatchHeldInChat   = "your owner answered that card in your chat: their message is a turn they typed, so call " +
		"agent_dispatch in your answer to it with owner_asked: true"
	agentDispatchHeldChoseRepo = "your owner chose the repository %s for this dispatch: call agent_dispatch with repo set to exactly that"
	// agentDispatchHeldPushAction: the stored answer came through the phone's
	// notification action, not a tap on the whole card.
	agentDispatchHeldPushAction = "your owner's answer to that approval card came from a notification, which cannot show the whole " +
		"request, so muster cannot take it as their approval; it starts only from a tap on the card itself"
	// approvalNotOnTheCard: the answer path's refusal of the same (the card
	// stays open): what the owner's phone reads.
	approvalNotOnTheCard = "open this card to approve it: an agent is started only from the whole card, never from a notification"
	// The approval caps (approvalCapRefusal).
	agentDispatchApproveTurnCap = "you already put an approval card in front of your owner in this turn: one per turn. Wait for " +
		"their answer before asking for another start"
	agentDispatchApproveHourCap = "you have put %d approval cards in front of your owner in the last hour, the most allowed: " +
		"ask them in your chat instead, or wait"
	// approvalCardsPerHour bounds the approval cards one owner gets in an hour.
	approvalCardsPerHour = 6
)

// errApprovalNeeded is a reservation check's answer for a request the owner
// must approve on a card: every other check passed, nothing is reserved, and
// the caller files the card. 🔴 THE CARD COMES AFTER THE RESERVATION'S OWN
// FIRST ANSWERS: a request_key that already started replays that start, and a
// task already started answers with it — the reservation decides those before
// its check runs — so a retry of a start that exists never raises a card.
var errApprovalNeeded = errors.New("the owner's approval is needed")

// approvalBlanks is the blanks a dispatch without the owner's typed turn
// holds: t's (nil for a pod target), then the repository's card when it is
// blank, else the approval card.
func approvalBlanks(t *hostTarget, repoBlank bool) []string {
	var out []string
	if t != nil {
		out = t.blanks()
	}
	if repoBlank {
		return append(out, blankRepo)
	}
	return append(out, blankApprove)
}

// isApprovalBlank reports whether blank is an approval card's (the owner's
// say-so for the whole request, not one blank of it).
func isApprovalBlank(blank string) bool { return blank == blankApprove || blank == blankRepo }

// repoBlankOf reports whether repo is a blank an approval card can ask for.
func repoBlankOf(repo string) bool {
	r := strings.TrimSpace(repo)
	return r == "" || r == "?"
}

// approvalCard is an approval card's question and options for blank, or the
// refusal.
type approvalCard struct {
	question string
	labels   []string
	values   map[string]string
}

// approvalRequest is what an approval card shows: the request exactly as it
// will start.
type approvalRequest struct {
	target, model, repo, base string
	task                      notes.Note
	onTask                    bool
	prompt                    string
	// trigger is what started the chief's turn (approvalTrigger), said on
	// the card; set by fileHeldDispatch.
	trigger string
}

// repoChangeable reports whether the request's repository is the chief's to
// pass — so the owner may change it — not one its task names.
func (r approvalRequest) repoChangeable() bool {
	return !r.onTask || strings.TrimSpace(r.task.Repo) == ""
}

// approvalQuestion is the card's text: the request in full, the brief last
// and whole. ok false when it does not fit attention.MaxQuestionRunes (the
// refusal then says by how much): a brief is never cut on the card, because
// the brief is what the agent is told.
func approvalQuestion(r approvalRequest, repoBlank bool) (string, string) {
	one := func(v string) string { return fence.Sanitize(strings.TrimSpace(v)) }
	var b strings.Builder
	b.WriteString("Start this agent? Your chief asked for it in a turn you did not type in its chat, so nothing starts unless " +
		"you approve it here.\n")
	if r.trigger != "" {
		b.WriteString("Your chief asked for this while handling " + r.trigger + ".\n")
	}
	where := one(r.target)
	if l := dispatchTargetLabel(r.target); l != r.target {
		where = one(l) + " (" + where + ")"
	}
	b.WriteString("On: " + where)
	if r.model != "" {
		b.WriteString(", model " + one(r.model))
	}
	b.WriteString("\n")
	switch {
	case repoBlank:
		b.WriteString("Repository: choose it below (or type its owner/name)\n")
	case r.repo != "":
		b.WriteString("Repository: " + one(r.repo) + "\n")
	default:
		b.WriteString("Repository: none\n")
	}
	if r.base != "" {
		b.WriteString("Base branch: " + one(r.base) + "\n")
	}
	if r.onTask {
		title := r.task.Title
		if title == "" {
			title, _, _ = strings.Cut(strings.TrimSpace(r.task.Body), "\n")
		}
		b.WriteString("Task: #" + strconv.FormatInt(r.task.ID, 10) + " " + one(fence.Cut(title, 120)) + "\n")
	} else {
		b.WriteString("Task: none\n")
	}
	if strings.TrimSpace(r.prompt) == "" {
		b.WriteString("\nBrief: none (the agent is given the task's text).")
	} else {
		b.WriteString("\nThe chief's brief — what the agent is told, exactly as shown:\n" + r.prompt)
	}
	q := b.String()
	if n := utf8.RuneCountInString(q); n > attention.MaxQuestionRunes {
		return "", fmt.Sprintf(agentDispatchApproveTooLong, attention.MaxQuestionRunes, n)
	}
	return q, ""
}

// approvalCardFor is the card for blank (blankApprove or blankRepo) of request
// r, for chief a starting on host ("" for a pod target).
func (s *Server) approvalCardFor(ctx context.Context, a agents.Agent, blank, host string, r approvalRequest) (approvalCard, string) {
	q, why := approvalQuestion(r, blank == blankRepo)
	if why != "" {
		return approvalCard{}, why
	}
	c := approvalCard{question: q, values: map[string]string{}}
	if blank == blankApprove {
		c.labels = []string{approveLabel}
		c.values[approveLabel] = approveValue
		if r.repoChangeable() {
			// The repo card in this one's place: the connected repositories,
			// ranked, every guard of a picked repository (completeHeld).
			c.labels = append(c.labels, changeRepoLabel)
			c.values[changeRepoLabel] = changeRepoValue
		}
		c.labels = append(c.labels, declineLabel)
		c.values[declineLabel] = declineValue
		return c, ""
	}
	list, err := s.githubRepos(ctx)
	if errors.Is(err, errNoGitHub) {
		return approvalCard{}, chiefDispatchNoGitHub
	}
	if err != nil {
		s.logger.Printf("agent_dispatch: list the owner's repositories for a repo card: %v", err)
		return approvalCard{}, "your owner's repositories could not be listed; try again later"
	}
	ranked := s.rankedRepos(ctx, a, host, list)
	if len(ranked) == 0 {
		return approvalCard{}, chiefDispatchNoGitHub
	}
	for _, full := range ranked {
		c.labels = append(c.labels, full)
		c.values[full] = repoValuePref + strings.ToLower(full)
	}
	c.labels = append(c.labels, declineLabel)
	c.values[declineLabel] = declineValue
	return c, ""
}

// rankedRepos is up to repoCardMax of list's repositories (canonical FullName)
// for a repo card: first the GitHub repository of the session the owner is
// viewing in the chief's chat (chiefAskSubject), then those of the online
// windows on host (the start's host, "" for none) in the list's order, then
// the rest of list in its order. Only connected repositories (list's) appear;
// a window's GitHub repository is its host's word, so it only ORDERS them.
func (s *Server) rankedRepos(ctx context.Context, a agents.Agent, host string, list []github.Repo) []string {
	canonical := map[string]string{}
	for _, r := range list {
		canonical[strings.ToLower(r.FullName)] = r.FullName
	}
	var out []string
	add := func(full string) {
		if full != "" && !slices.Contains(out, full) && len(out) < repoCardMax {
			out = append(out, full)
		}
	}
	if key := s.chiefAskSubject(ctx, a); key != "" {
		if e, ok := s.mirrorFocusEntryFor(ctx, key); ok {
			add(canonical[strings.ToLower(e.GitHubRepo)])
		}
	}
	if host != "" && s.ext.Mirror != nil {
		for _, e := range mirror.Ordered(s.ext.Mirror.Hosts(ctx)) {
			if e.Online && e.Host == host && e.GitHubRepo != "" {
				add(canonical[strings.ToLower(e.GitHubRepo)])
			}
		}
	}
	for _, r := range list {
		add(r.FullName)
	}
	return out
}

// --- where the request came from, and how often -----------------------------

// approvalTriggerRunes cuts an outside name (a sender, a question) on the card.
const approvalTriggerRunes = 80

// attentionAnswerTurnPrefix starts muster's delivery of the owner's answer to
// an attention ask (renderAttentionAnswer); the ask it names is re-read before
// the card says so.
const attentionAnswerTurnPrefix = "muster: your owner answered attention #"

// chiefTurnRow is the user row the chief's running turn answers: the turn's
// own input (turnInputOf), else — a delivered turn muster did not store first
// — the newest user row of its default session within its turn budget. ok
// false when there is none.
func (s *Server) chiefTurnRow(ctx context.Context, chief agents.Agent) (agents.ChatMessage, bool) {
	st, ok := s.ext.Agents.(ownerTurnStore)
	if !ok {
		return agents.ChatMessage{}, false
	}
	if id := s.turnInputOf(ctx, chief.ID); id > 0 {
		if m, err := st.ChatMessage(ctx, id); err == nil && m.AgentID == chief.ID && m.Role == "user" {
			return m, true
		}
	}
	sess, err := s.defaultSession(ctx, chief)
	if err != nil {
		return agents.ChatMessage{}, false
	}
	msgs, err := s.ext.Agents.ListRecentChatMessages(ctx, sess.ID, chatHistoryCap)
	if err != nil {
		return agents.ChatMessage{}, false
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if m := msgs[i]; m.Role == "user" {
			if s.now().Sub(m.CreatedAt) > agentTurnBudget(chief.Kind) {
				break
			}
			return m, true
		}
	}
	return agents.ChatMessage{}, false
}

// approvalTrigger is the chief's running turn (its row id, 0 unknown) and
// what started it, as the approval card says it ("Your chief asked for this
// while handling <trigger>"). 🔴 DECIDED FROM STRUCTURE, NOT THE ROW'S WORDS:
// a plugin delivery is its plugin_message_id (its plugin's label, the
// message's sender — outside words, quoted and cut — and whether it is a task
// event); muster's delivery of an attention answer names its ask, which is
// re-read and must be an ANSWERED ask of THIS chief's; a row the owner typed
// is TypedBy. Anything else is said to be unknown, never guessed.
func (s *Server) approvalTrigger(ctx context.Context, chief agents.Agent) (int64, string) {
	m, ok := s.chiefTurnRow(ctx, chief)
	if !ok {
		return 0, "a turn muster could not identify"
	}
	quote := func(v string) string {
		return strconv.Quote(fence.Cut(fence.Sanitize(strings.TrimSpace(v)), approvalTriggerRunes))
	}
	switch {
	case m.TypedBy > 0 && m.TypedBy == agentOwnerID(chief):
		return m.ID, "a message you typed in its chat (which it could not show was yours)"
	case m.PluginMessageID > 0 && s.ext.Plugins != nil:
		pm, err := s.ext.Plugins.Message(ctx, m.PluginMessageID)
		if err != nil {
			return m.ID, "a plugin message"
		}
		name := "a plugin"
		if p, err := s.ext.Plugins.Plugin(ctx, pm.PluginID); err == nil {
			name = firstNonBlank(p.Label, p.Name)
		}
		if plugins.IsTaskEvent(pm) {
			return m.ID, "a task event from " + quote(name) + " about task #" + strconv.FormatInt(*pm.TaskID, 10)
		}
		if strings.TrimSpace(pm.Sender) == "" {
			return m.ID, "a " + quote(name) + " message"
		}
		return m.ID, "a " + quote(name) + " message from " + quote(pm.Sender)
	case strings.HasPrefix(m.Content, attentionAnswerTurnPrefix) && s.ext.Attention != nil:
		digits := m.Content[len(attentionAnswerTurnPrefix):]
		if i := strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
			digits = digits[:i]
		}
		if id, err := strconv.ParseInt(digits, 10, 64); err == nil {
			if a, err := s.ext.Attention.Get(ctx, id); err == nil && a.Asker.AgentID == chief.ID && a.Status == attention.Answered {
				where := "on the Attention view"
				if a.AnsweredVia.Via == attention.ViaDevice {
					where = "from your phone's notification"
				}
				return m.ID, "your answer " + where + " to its question " + quote(a.Question)
			}
		}
	}
	return m.ID, "a turn you did not type"
}

// approvalCapRefusal is "" when the chief may put one more approval card in
// front of its owner in turn (for request key), else why not: at most ONE
// open approval card per chief turn (another request_key's), and at most
// approvalCardsPerHour filed in an hour. A count that cannot be read refuses
// (fail closed).
func (s *Server) approvalCapRefusal(ctx context.Context, turn int64, key string) string {
	st := s.hostDispatches()
	if st == nil {
		return "the approval card could not be raised; try again"
	}
	hour, inTurn, err := st.ApprovalHoldCounts(ctx, time.Hour, turn, key)
	if err != nil {
		s.logger.Printf("agent_dispatch: count approval cards: %v", err)
		return "the approval card could not be raised; try again"
	}
	if inTurn > 0 {
		return agentDispatchApproveTurnCap
	}
	if hour >= approvalCardsPerHour {
		return fmt.Sprintf(agentDispatchApproveHourCap, hour)
	}
	return ""
}

// approvalSurfaceRefusal is why answer to ask id cannot be taken from the
// door it came through, nil when it can (proposalSurfaceRefusal's model,
// #546): an APPROVAL card is answered only on the whole card — the Attention
// view, or the phone's signed-in page — never from the phone's signed
// notification action (the device door), whose lock screen showed a cut
// question. Refused BEFORE anything is recorded: the card stays open.
func (s *Server) approvalSurfaceRefusal(ctx context.Context, id int64) error {
	st := s.hostDispatches()
	if st == nil {
		return nil
	}
	p, ok := authz.From(ctx)
	if !ok || p.Via != authz.ViaDevice {
		return nil
	}
	h, err := st.CardHoldByAsk(ctx, id)
	if errors.Is(err, plugins.ErrHoldNotFound) {
		return nil // not a dispatch card
	}
	if err != nil {
		// Unreadable: whether this is an approval card is unknown, and the
		// phone's action is refused (fail closed; the whole card still works).
		s.logger.Printf("agent_dispatch: read the hold of card %d: %v", id, err)
		return errReleaseRefused(approvalNotOnTheCard)
	}
	var d heldDetail
	if json.Unmarshal(h.Detail, &d) != nil || !d.Approval {
		return nil
	}
	return errReleaseRefused(approvalNotOnTheCard)
}

// approvalNext is a held answer's "next" for an approval card.
func approvalNext(blank string) string {
	if blank == blankRepo {
		return "muster asked your owner to approve this start and choose its repository, on a card showing the whole request. " +
			"When their answer arrives, call agent_dispatch again with the same request_key, the same arguments and repo set " +
			"to the repository they chose; do not choose one yourself. If they declined, do not start it."
	}
	return "muster asked your owner to approve this start, on a card showing the whole request. When their answer arrives, " +
		"call agent_dispatch again with the same request_key and exactly the same arguments. If they declined, do not start it."
}

// renderApprovedBrief is the block the chief's brief becomes at the top of a
// started agent's first message when the owner APPROVED it on a card
// (renderChiefRelay's sibling): muster's preface — what the owner did, and did
// not, do — then the text fenced line by line. muster's rules close the
// message and win.
func renderApprovedBrief(text string, onTask bool) string {
	pre := "muster: your owner's chief wrote the instructions below, and your owner approved them on a card that showed them " +
		"in full (they did not type them). Follow them as your owner's wishes for this work, unless they conflict with " +
		"muster's rules at the end of this message, which win."
	if onTask {
		pre += " The task itself follows the block."
	}
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = fence.Sanitize(l)
	}
	return fence.Block{Preface: pre, Marker: chiefInstructionsMarker, Count: 1, Lines: lines}.Render(fence.NewNonce())
}

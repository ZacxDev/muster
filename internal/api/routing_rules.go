package api

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefusage"
	"github.com/ZacxDev/muster/internal/fence"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/plugins"
)

// ---------------------------------------------------------------------------
// ROUTING RULES (internal/plugins rules.go; docs/plugins.md#routing-rules): a
// message muster can place by a deterministic match is placed WITHOUT a chief
// turn.
//
//   - THE CHIEF PROPOSES (routing_rule_propose): about a message delivered to
//     it, naming which of that message's fields to match on and the action —
//     route to one mirrored session, or settle with a reason. muster copies the
//     values from the stored message (plugins.ProposeRule) and raises a card.
//   - THE OWNER TAPS (ruleForAnswer, answerAskFrom's hook, BEFORE the answer is
//     recorded): only through the owner's own door, only a tapped button, only
//     a proposed rule. Approve rule makes it active; Not now declines it.
//   - THE RULE RUNS BEFORE THE CHIEF (applyRoutingRules, in
//     dispatchPluginMessage after muster's other deterministic paths): the
//     first active rule that matches routes the message (routeByMuster — the
//     chief's own route, made by muster) or settles it (SettleQueuedAsMuster),
//     and the message is logged in the chief's conversation with muster's note
//     (Deliverer.Handed) — no turn. A route whose session cannot take the
//     message falls back to the chief, counted on the rule.
//
// 🔴 PLACEMENT: AFTER routeByThread, settleClosedPRReview, routeByPR and
// startMailResume. Each of those keys on a STRUCTURAL fact about this very
// message — the thread a session started, the pull request's state, the
// session that authored the pull request at this round — which is always as
// specific and as current as a learnt rule, and usually more: a rule keyed on a
// review sender would send every review to the session the first one went to,
// while routeByPR sends each to the session that authored that pull request.
// A rule fills the gap those leave, and never overrides them.
// ---------------------------------------------------------------------------

const (
	routingRulesPath     = "/rules"
	routingRuleProposeTo = chiefusage.RuleProposeTool
	ruleApproveLabel     = "Approve rule"
	// ruleSettleVia is the audit's "via" for a settle by rule.
	ruleSettleVia = "rule"
)

// The tap's refusals, as the owner reads them.
const (
	ruleNotOwnersDoor  = "only you can approve your chief's routing rule, tapping its card in your own signed-in browser or on your paired phone"
	ruleNotATap        = "tap one of the card's buttons: nothing happens on a typed answer, and Not now declines it"
	ruleDecidedAlready = "this routing rule was already approved or declined"
)

// ruleStore is the plugin store's routing-rule half (found by assertion, as
// proposalStore is), with the settle muster makes for a settle rule.
type ruleStore interface {
	CreateRuleProposal(ctx context.Context, n plugins.RuleNew) (plugins.Rule, error)
	RuleByID(ctx context.Context, id int64) (plugins.Rule, error)
	RuleByAsk(ctx context.Context, askID int64) (plugins.Rule, error)
	Rules(ctx context.Context) ([]plugins.Rule, error)
	ActiveRules(ctx context.Context, pluginID int64) ([]plugins.Rule, error)
	ApproveRule(ctx context.Context, id int64) (plugins.Rule, error)
	DeclineRule(ctx context.Context, id int64) (plugins.Rule, error)
	SetRuleEnabled(ctx context.Context, id int64, on bool) error
	DeleteRule(ctx context.Context, id int64) (plugins.Rule, error)
	RecordRuleHit(ctx context.Context, id int64) error
	RecordRuleFallback(ctx context.Context, id int64) error
	SettleQueuedAsMuster(ctx context.Context, chiefID, ownerID, messageID int64, reason, via string) error
}

var _ ruleStore = (*plugins.PGStore)(nil)

// rules is the plugin store's rule half, nil when plugins are off (or the
// store has none: a test's fake).
func (s *Server) rules() ruleStore {
	if s.ext.Plugins == nil {
		return nil
	}
	st, _ := s.ext.Plugins.(ruleStore)
	return st
}

// rulesOffered reports whether the chief is offered routing_rule_propose.
func (s *Server) rulesOffered() bool { return s.rules() != nil && s.ext.Attention != nil }

// --- the dispatch -----------------------------------------------------------

// applyRoutingRules handles NEW message m of plugin p by the first active rule
// that matches it (plugins.FirstRuleMatch): handled true with the rule's id
// when the rule routed or settled it; handled false with fallback set to the
// rule's id when a rule matched and could not (its session cannot take the
// message now, routing is off, no chief) — the caller hands m to the chief;
// and false with no fallback when no rule matched. ctx acts as m's owner.
func (s *Server) applyRoutingRules(ctx context.Context, p plugins.Plugin, m plugins.Message) (handled bool, ruleID, fallback int64) {
	st := s.rules()
	if st == nil || s.pluginDeliverer == nil {
		return false, 0, 0
	}
	rules, err := st.ActiveRules(ctx, p.ID)
	if err != nil {
		s.logger.Printf("rules: message %d: read the rules: %v; to the chief", m.ID, err)
		return false, 0, 0
	}
	r, ok := plugins.FirstRuleMatch(rules, m)
	if !ok {
		return false, 0, 0
	}
	if why := s.runRule(ctx, st, p, m, r); why != "" {
		// 🔴 NEVER DROPPED: a rule that cannot act hands the message to the
		// chief, as if there were no rule, and the miss is counted on the rule.
		s.logger.Printf("rules: message %d matched rule %d, which could not act (%s); to the chief", m.ID, r.ID, why)
		if err := st.RecordRuleFallback(ctx, r.ID); err != nil {
			s.logger.Printf("rules: rule %d: count the fallback: %v", r.ID, err)
		}
		return false, 0, r.ID
	}
	if err := st.RecordRuleHit(ctx, r.ID); err != nil {
		s.logger.Printf("rules: rule %d: count the hit: %v", r.ID, err)
	}
	s.logger.Printf("rules: message %d handled by rule %d (%s)", m.ID, r.ID, r.Action)
	return true, r.ID, 0
}

// ruleName is "rule #N".
func ruleName(id int64) string { return "rule #" + strconv.FormatInt(id, 10) }

// runRule does rule r's action on m: "" when done, else why it could not.
func (s *Server) runRule(ctx context.Context, st ruleStore, p plugins.Plugin, m plugins.Message, r plugins.Rule) string {
	switch r.Action {
	case plugins.RuleRoute:
		if !messageRoutingOn(s.ext) {
			return "session routing is off"
		}
		origin := plugins.RouteOrigin{Via: plugins.RouteViaRule, Session: r.RouteSession, Rule: r.ID}
		// The note in the chief's chat names the rule as its settle reason
		// would: "muster: rule #N".
		target := "the session named by " + plugins.SettledByMuster + ruleName(r.ID) + " (your owner's routing rule)"
		what := "a message matching your owner's routing " + ruleName(r.ID)
		if taken, refused := s.routeByMusterWhy(ctx, p, m, origin, target, what); !taken {
			return "the route was refused: " + refused
		}
		return ""
	case plugins.RuleSettle:
		chief, err := s.chiefAgent(ctx)
		if err != nil {
			return "there is no chief to settle it under: " + err.Error()
		}
		reason := plugins.RuleSettledReasonStart + strconv.FormatInt(r.ID, 10) + " — " + r.SettleReason
		if err := st.SettleQueuedAsMuster(ctx, chief.ID, agentOwnerID(chief), m.ID, reason, ruleSettleVia); err != nil {
			return "the settle failed: " + err.Error()
		}
		cur, err := s.ext.Plugins.Message(ctx, m.ID)
		if err != nil {
			cur = m
		}
		// The note names the rule as the message's settle reason does.
		note := "[muster] plugin message_id " + strconv.FormatInt(m.ID, 10) + " was settled by " + fence.Sanitize(reason) +
			" (your owner's routing rule: when " + plugins.RuleSummary(r.Match) + "). No turn was spent on it; nothing to do."
		if err := s.pluginDeliverer.Handed(ctx, p, chief, cur, note); err != nil {
			s.logger.Printf("rules: message %d: record it in the chief's conversation: %v", m.ID, err)
		}
		return ""
	}
	return "unknown action " + r.Action
}

// --- the chief's tool ---------------------------------------------------------

func routingRuleProposeToolDef() agents.ToolDef {
	return agents.ToolDef{Type: "function", Name: routingRuleProposeTo,
		Description: "Propose a routing rule from a plugin message delivered to you, so later messages like it skip you. " +
			"muster copies the fields you pick from that message (its sender or thread, optionally narrowed by a prefix) and, " +
			"for each later match, routes it to one mirrored " +
			"session (verified senders only) or settles it with your reason. Your owner approves it with one tap; until " +
			"then nothing changes. Propose one only when you would place that kind of message the same way every time.",
		Parameters: objSchema(map[string]any{
			"message_id":         map[string]any{"type": "integer", "description": "The plugin message (its message_id) the rule is copied from."},
			"action":             map[string]any{"type": "string", "enum": []string{plugins.RuleRoute, plugins.RuleSettle}},
			"subject":            strProp(`route: the session, "mirror:<claude session id>".`),
			"reason":             strProp("settle: why such messages need nothing (at most 300 characters)."),
			"match_sender":       map[string]any{"type": "boolean", "description": "Match the message's sender exactly."},
			"match_thread":       map[string]any{"type": "boolean", "description": "Match the message's thread exactly."},
			"external_id_prefix": strProp("Narrow it to external ids starting with this (8+ bytes, a prefix of the message's own)."),
			"subject_prefix":     strProp("Narrow it to subjects starting with this (a prefix of the message's own)."),
		}, "message_id", "action")}
}

// toolRoutingRulePropose is routing_rule_propose: the rule the chief asks for,
// copied from a message delivered to it (plugins.ProposeRule), with its card.
func (s *Server) toolRoutingRulePropose(ctx context.Context, a agents.Agent, arguments string) string {
	st := s.rules()
	if st == nil || s.ext.Attention == nil {
		return toolErr("%s is not available on this muster", routingRuleProposeTo)
	}
	var in struct {
		MessageID        int64  `json:"message_id"`
		Action           string `json:"action"`
		Subject          string `json:"subject"`
		Reason           string `json:"reason"`
		MatchSender      bool   `json:"match_sender"`
		MatchThread      bool   `json:"match_thread"`
		ExternalIDPrefix string `json:"external_id_prefix"`
		SubjectPrefix    string `json:"subject_prefix"`
	}
	if err := json.Unmarshal([]byte(orEmptyJSON(arguments)), &in); err != nil || in.MessageID <= 0 {
		return toolErr("arguments are {message_id, action, subject | reason, and the fields to match on}")
	}
	m, err := s.ext.Plugins.Message(ctx, in.MessageID)
	if err != nil || !plugins.DeliveredTo(m, a.ID, agentOwnerID(a)) {
		return toolErr("message_id %d is not a plugin message delivered to you", in.MessageID)
	}
	p, err := s.ext.Plugins.Plugin(ctx, m.PluginID)
	if err != nil {
		return toolErr("the message's plugin is gone")
	}
	req := plugins.RuleRequest{MatchSender: in.MatchSender, MatchThread: in.MatchThread,
		ExternalPrefix: in.ExternalIDPrefix, SubjectPrefix: in.SubjectPrefix, Action: in.Action, Reason: in.Reason}
	target := ""
	if in.Action == plugins.RuleRoute {
		if !messageRoutingOn(s.ext) {
			return toolErr("session routing is off on this muster, so a rule cannot route: propose a settle rule, or none")
		}
		sub, err := focus.ParseSubject(in.Subject)
		if err != nil || sub.Kind != focus.KindMirror {
			return toolErr(`a route rule's subject is "mirror:<claude session id>", as focus_search returns it`)
		}
		// The session must be able to take a message NOW: a rule is never
		// proposed for a session that is already gone.
		t, refused := s.resolveRouteTarget(ctx, sub)
		if refused != "" {
			return refused
		}
		req.Session, target = sub.Key, t.label
	}
	n, err := plugins.ProposeRule(p, m, req)
	if err != nil {
		return toolErr("%s", strings.TrimPrefix(err.Error(), plugins.ErrInvalid.Error()+": "))
	}
	ask, err := s.raiseProposalCard(ctx, a, ruleCardQuestion(p, n, target), []string{ruleApproveLabel, proposalDeclineLabel})
	if err != nil {
		return toolErr(proposalNoCard, ownerCardRefusal(err))
	}
	n.ChiefAgent, n.AskID = a.ID, ask.ID
	r, err := st.CreateRuleProposal(chiefWriteContext(ctx, a), n)
	if err != nil {
		_, _ = s.cancelAsk(ctx, ask.ID)
		switch {
		case errors.Is(err, plugins.ErrRuleExists):
			return toolErr("a rule with that match already exists (proposed, active or disabled): nothing was proposed")
		case errors.Is(err, plugins.ErrRuleProposalsFull):
			return toolErr("%d routing rules already wait for your owner's decision: nothing was proposed", plugins.MaxOpenRuleProposals)
		}
		s.logger.Printf("rules: record %s's proposal: %v", a.Name, err)
		return toolErr("the rule could not be recorded; try again")
	}
	s.logger.Printf("rules: %s proposed rule %d (%s) for plugin %s (card %d)", a.Name, r.ID, r.Action, p.Name, ask.ID)
	s.proposalCardFiled(ask)
	return toolJSON(map[string]any{"rule_id": r.ID, "state": r.State, "match": plugins.RuleSummary(r.Match), "action": r.Action,
		"next": "Your owner approves or declines it with one tap. Until then nothing changes; handle this message as you would anyway."})
}

// chiefWriteContext is ctx with its principal's door set to the agent's, for
// the write chief a makes through its own tool. 🔴 A DELIVERED MESSAGE'S TURN
// RUNS ON THE PLUGIN'S PRINCIPAL (it acts as the owner), and the rule store
// refuses every write from a plugin's principal (plugins.ErrRulePlugin) — so
// without this the chief could never propose from the very turn the message
// reached it on (measured: the browser suite's first run). The write is the
// chief's: dispatchAgentTool has already refused any agent but the owner's
// chief, and no plugin token reaches an agent tool. ctx is returned as it is
// when its principal is not chief's owner (the store then refuses it).
func chiefWriteContext(ctx context.Context, a agents.Agent) context.Context {
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 || p.UserID != agentOwnerID(a) {
		return ctx
	}
	p.Via = authz.ViaAgent
	return authz.WithPrincipal(ctx, p)
}

// ruleCardQuestion is a rule proposal's card: what it matches and what it does.
func ruleCardQuestion(p plugins.Plugin, n plugins.RuleNew, target string) string {
	do := ""
	switch n.Action {
	case plugins.RuleRoute:
		do = "route it to the session " + strconv.Quote(fence.Sanitize(target)) + " (as route_to_session would)"
	default:
		do = "settle it: " + strconv.Quote(fence.Sanitize(n.SettleReason))
	}
	return "Your chief proposes a routing rule for " + fence.Sanitize(fence.Cut(p.Label, plugins.MaxLabelRunes)) + " messages: when " +
		plugins.RuleSummary(n.Match) + ", " + do + ". Approve it and muster does this for later matching messages without a chief " +
		"turn, logging each in the chief's chat. You can disable or delete it on the Rules page."
}

// --- the owner's tap ------------------------------------------------------------

// ruleForAnswer is THE TAP on a rule card (answerAskFrom, BEFORE the answer is
// recorded); nil, nil when ask id is no rule's card. The answer must come
// through the owner's own door, be one of the card's buttons TAPPED as a
// button, and find the rule still proposed; Not now declines it, Approve rule
// makes it active — once. The note is muster's sentence for the chief.
func (s *Server) ruleForAnswer(ctx context.Context, id int64, answer string, src attention.AnswerSource) (*proposalNote, error) {
	st := s.rules()
	if st == nil || s.ext.Attention == nil {
		return nil, nil
	}
	a, err := s.ext.Attention.Get(ctx, id)
	if err != nil || a.Status != attention.Open {
		return nil, nil
	}
	r, err := st.RuleByAsk(ctx, id)
	if err != nil {
		return nil, nil
	}
	// 🔴 THE OWNER'S OWN DOOR: a signed-in browser or the paired phone, as the
	// card's owner. A mirrored session's attention_answer, a token, a plugin:
	// refused before anything is recorded or done.
	if src.ViaSession() || !attention.IsOwnerDoor(ctx, a.Owner) {
		return nil, errReleaseRefused(ruleNotOwnersDoor)
	}
	if r.State != plugins.RuleProposed {
		return nil, errReleaseRefused(ruleDecidedAlready)
	}
	choice := strings.TrimSpace(answer)
	if !optionTapped(ctx) || !proposalOptionOf(a, choice) {
		return nil, errReleaseRefused(ruleNotATap)
	}
	what := "your routing " + ruleName(r.ID) + " (" + plugins.RuleSummary(r.Match) + ")"
	if choice == proposalDeclineLabel {
		if _, err := st.DeclineRule(ctx, r.ID); err != nil {
			return nil, errReleaseRefused(ruleDecidedAlready)
		}
		s.logger.Printf("rules: rule %d declined by its owner", r.ID)
		return &proposalNote{chief: r.ChiefAgent, text: "[muster] Your owner declined " + what +
			". Nothing changes. Do not propose it again unless they ask."}, nil
	}
	if _, err := st.ApproveRule(ctx, r.ID); err != nil {
		return nil, errReleaseRefused(ruleDecidedAlready)
	}
	s.logger.Printf("rules: rule %d approved by its owner", r.ID)
	return &proposalNote{chief: r.ChiefAgent, text: "[muster] Your owner approved " + what +
		". From now on muster handles later messages that match it without a turn, and logs each here."}, nil
}

// isRuleCard reports whether ask a is one of ctx's principal's rule cards.
func (s *Server) isRuleCard(ctx context.Context, a attention.Ask) bool {
	st := s.rules()
	if st == nil || !a.OwnerCard || a.Asker.AgentID <= 0 {
		return false
	}
	_, err := st.RuleByAsk(ctx, a.ID)
	return err == nil
}

// --- the Rules page ---------------------------------------------------------------

func (s *Server) registerRoutingRuleRoutes(mux Mux) {
	mux.HandleFunc("GET "+routingRulesPath, s.requirePluginsOn(s.requireSession(s.handleRulesPage)))
	mux.HandleFunc("POST "+routingRulesPath+"/{id}/disable", s.requirePluginsOn(s.requireOperatorWrite(s.handleRuleDisable)))
	mux.HandleFunc("POST "+routingRulesPath+"/{id}/enable", s.requirePluginsOn(s.requireOperatorWrite(s.handleRuleEnable)))
	mux.HandleFunc("POST "+routingRulesPath+"/{id}/delete", s.requirePluginsOn(s.requireOperatorWrite(s.handleRuleDelete)))
}

func (s *Server) handleRulesPage(w http.ResponseWriter, r *http.Request) {
	s.renderRulesPage(w, r, http.StatusOK, "")
}

func (s *Server) handleRuleDisable(w http.ResponseWriter, r *http.Request) {
	s.ruleWrite(w, r, "disable")
}
func (s *Server) handleRuleEnable(w http.ResponseWriter, r *http.Request) {
	s.ruleWrite(w, r, "enable")
}
func (s *Server) handleRuleDelete(w http.ResponseWriter, r *http.Request) {
	s.ruleWrite(w, r, "delete")
}

// ruleWrite is the Rules page's three buttons.
func (s *Server) ruleWrite(w http.ResponseWriter, r *http.Request, op string) {
	st := s.rules()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if st == nil || err != nil || id <= 0 {
		s.renderRulesPage(w, r, http.StatusNotFound, "No such rule.")
		return
	}
	switch op {
	case "disable":
		err = st.SetRuleEnabled(r.Context(), id, false)
	case "enable":
		err = st.SetRuleEnabled(r.Context(), id, true)
	default:
		var gone plugins.Rule
		gone, err = st.DeleteRule(r.Context(), id)
		// 🔴 A DELETED PROPOSAL'S CARD GOES WITH IT: left open, a later tap
		// would find no rule and reach the chief as a plain answer.
		if err == nil && gone.State == plugins.RuleProposed && gone.AskID > 0 && s.ext.Attention != nil {
			if _, cerr := s.cancelAsk(r.Context(), gone.AskID); cerr != nil && !errors.Is(cerr, attention.ErrNotOpen) {
				s.logger.Printf("rules: rule %d deleted; cancel its card %d: %v", id, gone.AskID, cerr)
			}
		}
	}
	switch {
	case errors.Is(err, plugins.ErrRuleNotFound):
		s.renderRulesPage(w, r, http.StatusNotFound, "No such rule (or it cannot be "+op+"d now).")
		return
	case err != nil:
		s.logger.Printf("rules: %s %d: %v", op, id, err)
		s.renderRulesPage(w, r, http.StatusInternalServerError, "The rule could not be changed.")
		return
	}
	http.Redirect(w, r, routingRulesPath, http.StatusSeeOther)
}

// ruleTime is t as the page shows it ("never" for zero).
func ruleTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format("2006-01-02 15:04") + " UTC"
}

// ruleStateText is a rule's state in words.
func ruleStateText(r plugins.Rule) string {
	switch r.State {
	case plugins.RuleProposed:
		return "waiting for your tap on its card"
	case plugins.RuleActive:
		return "active"
	case plugins.RuleDisabled:
		return "disabled"
	}
	return "declined"
}

// ruleActionText is a rule's action in words.
func ruleActionText(r plugins.Rule) string {
	if r.Action == plugins.RuleRoute {
		return "route to session " + r.RouteSession
	}
	return "settle: " + r.SettleReason
}

// renderRulesPage writes /rules: every rule of the owner's, newest first.
func (s *Server) renderRulesPage(w http.ResponseWriter, r *http.Request, code int, problem string) {
	var list []plugins.Rule
	if st := s.rules(); st != nil {
		var err error
		if list, err = st.Rules(r.Context()); err != nil {
			s.logger.Printf("rules: page list: %v", err)
			problem = "The rules could not be read."
		}
	}
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString(`<section class="flex flex-col gap-4 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line" data-testid="rules-page">
<h1 class="text-balance text-base font-semibold text-fg">Routing rules</h1>
<p class="text-sm text-fg2">A routing rule places a plugin message without your chief: it routes the message to one session, or settles it, and logs it in your chief's chat. Your chief proposes a rule; it runs only once you approve its card, and only on messages that arrive after that.</p>
`)
	b.WriteString(problemNode(problem))
	b.WriteString(`<ul class="flex flex-col gap-2" data-testid="rules">
`)
	if len(list) == 0 {
		b.WriteString(`<li class="text-sm text-fg2">No rules yet.</li>`)
	}
	for _, x := range list {
		id := strconv.FormatInt(x.ID, 10)
		b.WriteString(`<li class="` + flowsCardClass + `" data-rule-id="` + id + `" data-rule-state="` + esc(x.State) + `">
<span class="break-all text-sm font-semibold text-fg">Rule #` + id + ` <span class="font-mono text-xs text-fg2">` + esc(x.PluginName) + `</span></span>
<span class="break-all text-sm text-fg" data-rule-match>When ` + esc(plugins.RuleSummary(x.Match)) + `</span>
<span class="break-all text-sm text-fg" data-rule-action>` + esc(ruleActionText(x)) + `</span>
<span class="text-xs text-fg2" data-rule-status>` + esc(ruleStateText(x)) + ` · created ` + esc(ruleTime(x.CreatedAt)) + `</span>
<span class="text-xs text-fg2" data-rule-hits="` + strconv.FormatInt(x.Hits, 10) + `">` + strconv.FormatInt(x.Hits, 10) + ` hit(s) · last hit ` +
			esc(ruleTime(x.LastHitAt)) + ` · ` + strconv.FormatInt(x.Fallbacks, 10) + ` handed to your chief (session unavailable)</span>
`)
		switch x.State {
		case plugins.RuleActive:
			b.WriteString(`<form method="POST" action="` + routingRulesPath + `/` + id + `/disable"><button type="submit" class="` + flowsQuietButton + `" data-rule-disable>Disable</button></form>
`)
		case plugins.RuleDisabled:
			b.WriteString(`<form method="POST" action="` + routingRulesPath + `/` + id + `/enable"><button type="submit" class="` + flowsQuietButton + `" data-rule-enable>Enable</button></form>
`)
		}
		// Every rule can be deleted — a proposed one's card is withdrawn with it.
		b.WriteString(`<form method="POST" action="` + routingRulesPath + `/` + id + `/delete"><button type="submit" class="` + flowsQuietButton + `" data-rule-delete>Delete</button></form>
`)
		b.WriteString("</li>\n")
	}
	b.WriteString(`</ul>
<p class="text-sm text-fg2">How much your chief costs, and what the rules save: <a href="` + usagePath + `" class="text-accent underline">Usage</a>.</p>
<a href="` + pluginsPath + `" class="inline-flex min-h-[44px] items-center text-sm text-accent underline">Back to plugins</a>
</section>`)
	s.writeNavDocument(w, r, code, "Routing rules", "", false, b.String())
}

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// THE STEP-UP GATE ON A HOST OFFICER'S MACHINE (operator decision, 2026-11-13;
// docs/design/officers-on-hosts.md, "The step-up gate").
//
// The mirror door names its caller by a header the caller sets: the host's
// loopback MCP endpoint maps X-Muster-Pane to a session, and any process of
// the machine's user can send any pane. On a machine that runs a host
// officer, that process may be the officer itself — so an owner-authority
// tool reached through that machine's door (approving a draft, deciding a
// connector request, answering an ask …) proves nothing about WHO decided.
//
// 🔴 SO ON SUCH A MACHINE EVERY OWNER-AUTHORITY TOOL IS A REQUEST, NOT A
// DECISION. hostStepUpGate files a CONFIRMATION CARD to the owner — one card
// per decision, naming the tool and its exact arguments — and runs nothing.
// The decision runs only when the owner answers that card "Confirm" from
// their paired phone: a SIGNED ACTION (device_actions.go), whose challenge is
// single use, lives two minutes and is bound to that card's id and the sha256
// of the exact answer. A browser answer, an MCP answer (attention_answer from
// any machine) and an answer after a restart lost the pending decision are
// refused, and the card stays open (or is withdrawn). The decision runs
// once: it is taken out of the pending set before it runs.
//
// What is NOT gated: the owner deciding in muster's own web UI (the Reply
// drafts tab, Connectors, Attention), which is the owner's signed-in browser;
// any machine with no host officer declared on it; the read tools.
// ---------------------------------------------------------------------------

// hostStepUpTools is THE LEDGER of the mirror door's owner-authority tools, by
// name, with what a confirmation card says it asks to do. Every other mirror
// door tool is classified in hostStepUpExempt (TestEveryMirrorToolIsClassified
// fails when a new one is neither).
var hostStepUpTools = map[string]string{
	attentionAnswerTool:        "answer one of your open asks as you",
	connectorRequestDecideTool: "decide an agent's connector request as you",
	pluginDraftDecideTool:      "approve or reject a reply or outbound draft as you",
	agentDestroyTool:           "destroy one of your agents",
	chiefRestartTool:           "restart your chief",
	dispatchApproveTool:        "approve a held agent start as you",
	routeReplyToolName:         "answer a routed message (which may be relayed to its sender) as you",
	pluginMessageRouteTool:     "route a plugin message as you",
	pluginRequestDecideTool:    "decide a plugin's pairing request (which mints it a token) as you",
}

// hostStepUpExempt is every other mirror door tool, with why it is not an
// owner-authority tool.
var hostStepUpExempt = map[string]string{
	attentionAskTool:          "the session's own question to the owner",
	attentionWaitTool:         "waits on the session's own ask",
	attentionCancelTool:       "withdraws the session's own ask",
	tasksListTool:             "the tasks this session created",
	taskGetTool:               "the tasks this session created",
	taskCommentTool:           "the tasks this session created",
	taskSetStatusTool:         "the tasks this session created",
	chiefAskTool:              "a question to the chief, never the owner's turn",
	agentsListTool:            "a read",
	connectorRequestsListTool: "a read",
	pluginDraftsListTool:      "a read",
	pluginMessagesListTool:    "a read",
	dispatchRequestTool:       "files a held start; dispatch_approve is the decision",
	pageCreateTool:            "the session's own page; a link needs the owner's tap",
	pageUpdateTool:            "the session's own page",
	pageGetTool:               "a read",
	pageListTool:              "a read",
	agentReportDoneTool:       "the session's own dispatch",
	pageShareRequestTool:      "asks the owner for a link (their tap decides)",
	pageResponsesTool:         "a read of the session's own page",
	pageLinkRevokeTool:        "revokes a link of the session's own page",
	"connectors_list":         "a host instance's own (hostOfficerMirrorTool)",
	"connector_request":       "a host instance's own (hostOfficerMirrorTool)",
	"connector_status":        "a host instance's own (hostOfficerMirrorTool)",
	postgresQueryToolName:     "a host instance's own (hostOfficerMirrorTool)",
	pluginRequestsListTool:    "a read",
	officers.ToolBrief:        "a host instance's own (hostOfficerMirrorTool)",
	officers.ToolItems:        "a host instance's own (hostOfficerMirrorTool)",
	officers.ToolClaim:        "a host instance's own (hostOfficerMirrorTool)",
	officers.ToolHeartbeat:    "a host instance's own (hostOfficerMirrorTool)",
	officers.ToolComplete:     "a host instance's own (hostOfficerMirrorTool)",
	officers.ToolRelease:      "a host instance's own (hostOfficerMirrorTool)",
	officers.ToolAskPerson:    "a host instance's own (hostOfficerMirrorTool)",
	officers.ToolPostDraft:    "a host instance's own (hostOfficerMirrorTool); the draft itself waits for the owner",
}

// hostDecisionLabel is the confirmation card's asker label: how a card whose
// pending decision a restart lost is still recognised (and refused).
const hostDecisionLabel = "step-up"

// hostDecision is one pending owner-authority call, waiting for its card.
type hostDecision struct {
	owner      int64
	host       string
	caller     toolCaller
	tool, args string
}

// hostDecisions is the pending set, by card (ask) id. In memory: muster runs
// one replica, and a restart can only LOSE a pending decision (its card is
// then refused and withdrawn), never run one.
type hostDecisions struct {
	mu      sync.Mutex
	pending map[int64]hostDecision
}

func (h *hostDecisions) put(id int64, d hostDecision) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pending == nil {
		h.pending = map[int64]hostDecision{}
	}
	h.pending[id] = d
}

// take removes and returns card id's decision: single use.
func (h *hostDecisions) take(id int64) (hostDecision, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	d, ok := h.pending[id]
	delete(h.pending, id)
	return d, ok
}

func (h *hostDecisions) peek(id int64) (hostDecision, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	d, ok := h.pending[id]
	return d, ok
}

// hostOfficersOnMachine is the host officers of owner declared on host: a
// non-empty answer is what gates that machine's door.
func (s *Server) hostOfficersOnMachine(owner int64, host string) []string {
	if s.officerRT == nil {
		return nil
	}
	var out []string
	for _, o := range s.officerRT.reg.All() {
		if o.OnHost() && o.Host == host && o.Owner == owner {
			out = append(out, o.ID)
		}
	}
	return out
}

// The card's two answers.
const (
	hostDecisionConfirm = "Confirm"
	hostDecisionRefuse  = "Refuse"
)

// hostStepUpGate is the mirror door's gate, run on every call after the host
// instance rule: an owner-authority tool from a machine that runs a host
// officer files a confirmation card and runs nothing (ok true, the tool's
// answer is the card); any other call is not the gate's (ok false).
func (s *Server) hostStepUpGate(ctx context.Context, host string, c toolCaller, name, args string) (string, bool) {
	what, gated := hostStepUpTools[name]
	if !gated {
		return "", false
	}
	p, _ := authz.From(ctx)
	ids := s.hostOfficersOnMachine(p.UserID, host)
	if len(ids) == 0 {
		return "", false
	}
	if s.ext.Attention == nil {
		return toolErr("%s from %s needs your owner's confirmation on their phone (this machine runs host officer %s), and this server has no "+
			"attention store to ask through; decide it in muster's web UI", name, host, strings.Join(ids, ", ")), true
	}
	sum := sha256.Sum256([]byte(name + "\x00" + args))
	question := fmt.Sprintf("A session on %s (%s) asks to %s: %s %s\n\nThis machine runs host officer %s, and its sessions are named by a header "+
		"any process there can set — so this needs you. Confirm on your paired phone to let it happen (a signed action; a browser answer is refused), "+
		"or decide it in muster's web UI instead. [%s]",
		host, firstNonBlank(c.asker.Label, c.asker.MirrorSession), what, name, mirror.OneLine(strings.TrimSpace(args), 600), strings.Join(ids, ", "), hex.EncodeToString(sum[:6]))
	ask, err := s.ext.Attention.Create(s.attentionPrincipal(ctx, p.UserID), attention.New{
		Asker:    attention.Asker{System: true, Label: hostDecisionLabel},
		Question: question, Options: []string{hostDecisionConfirm, hostDecisionRefuse},
	})
	if err != nil {
		s.logger.Printf("step-up: file a card for %s from %s/%s: %v", name, host, c.asker.MirrorSession, err)
		return toolErr("%s needs your owner's confirmation on their phone, and the confirmation could not be filed; try again", name), true
	}
	s.hostDecisions.put(ask.ID, hostDecision{owner: p.UserID, host: host, caller: c, tool: name, args: args})
	s.askFiled(ask)
	s.logger.Printf("step-up: %s from %s/%s waits for card %d (host officer %s on the machine)", name, host, c.asker.MirrorSession, ask.ID, strings.Join(ids, ", "))
	return toolJSON(map[string]any{"status": "awaiting_owner_confirmation", "card": ask.ID,
		"next": "Nothing was done yet. Your owner confirms or refuses this on their phone; it then runs once, exactly as asked. " +
			"Do not call it again; read the outcome with the list tools later."}), true
}

// errHostDecisionNeedsPhone refuses a confirmation card's answer from any door
// but the owner's paired phone.
var errHostDecisionNeedsPhone = fmt.Errorf("%w: this confirmation is answered only from your paired phone (a signed action); "+
	"or decide the request itself in muster's web UI", attention.ErrInvalid)

// errHostDecisionLost refuses a card whose pending decision is gone (a restart).
var errHostDecisionLost = fmt.Errorf("%w: this confirmation's request was lost when muster restarted; the session must ask again", attention.ErrInvalid)

// hostDecisionForAnswer is answerAskFrom's hook for a confirmation card:
// handled false for any other ask. For a card: a door that is not the
// owner's paired phone is refused (the card stays open); an answer that is
// neither option is refused; "Refuse" runs nothing; "Confirm" runs the
// decision once, as the session that asked, before the answer is recorded.
func (s *Server) hostDecisionForAnswer(ctx context.Context, id int64, answer string) (bool, error) {
	pending, ok := s.hostDecisions.peek(id)
	if !ok {
		if s.ext.Attention == nil {
			return false, nil
		}
		a, err := s.ext.Attention.Get(ctx, id)
		if err != nil || !a.Asker.System || a.Asker.Label != hostDecisionLabel {
			return false, nil
		}
		if _, err := s.cancelAsk(ctx, id); err != nil && !errors.Is(err, attention.ErrNotOpen) {
			s.logger.Printf("step-up: withdraw lost card %d: %v", id, err)
		}
		return true, errHostDecisionLost
	}
	// Every check BEFORE the decision is taken: a refused answer leaves it
	// pending for the owner's phone.
	p, ok := authz.From(ctx)
	if !ok || p.UserID != pending.owner {
		return true, errHostDecisionLost
	}
	if p.Via != authz.ViaDevice {
		return true, errHostDecisionNeedsPhone
	}
	if answer != hostDecisionConfirm && answer != hostDecisionRefuse {
		return true, fmt.Errorf("%w: answer %q or %q", attention.ErrInvalid, hostDecisionConfirm, hostDecisionRefuse)
	}
	d, ok := s.hostDecisions.take(id)
	if !ok {
		return true, errHostDecisionLost
	}
	if answer == hostDecisionRefuse {
		s.logger.Printf("step-up: card %d refused: %s from %s/%s not run", id, d.tool, d.host, d.caller.asker.MirrorSession)
		return true, nil
	}
	mctx := authz.WithPrincipal(context.WithoutCancel(ctx), s.principalFor(ctx, d.owner, authz.ViaMirrorHost))
	out := s.runStepUpTool(mctx, d.host, d.caller, d.tool, d.args)
	outcome := "ran"
	if isToolErr(out) {
		outcome = "ran and was refused: " + mirror.OneLine(out, 200)
	}
	s.logger.Printf("step-up: card %d confirmed on the owner's phone: %s from %s/%s %s", id, d.tool, d.host, d.caller.asker.MirrorSession, outcome)
	return true, nil
}

// runStepUpTool runs a confirmed owner-authority tool exactly as the mirror
// door would have, for caller c of host. ONLY hostDecisionForAnswer calls it
// (TestTheStepUpGateIsALedger).
func (s *Server) runStepUpTool(ctx context.Context, host string, c toolCaller, name, args string) string {
	switch name {
	case chiefRestartTool:
		return s.toolChiefRestart(ctx, c, args)
	case routeReplyToolName:
		return s.toolRouteReply(ctx, c, args)
	}
	if text, ok := s.dispatchPluginMessageRouteTool(ctx, c, name, args); ok {
		return text
	}
	if text, ok := s.dispatchHoldTool(ctx, c, name, args); ok {
		return text
	}
	if text, ok := s.dispatchCallerTool(ctx, c, name, args); ok {
		return text
	}
	return toolErr("unknown tool %q", name)
}

// hostStepUpToolNames is the gated set, sorted (for the docs and the tests).
func hostStepUpToolNames() []string {
	var out []string
	for n := range hostStepUpTools {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

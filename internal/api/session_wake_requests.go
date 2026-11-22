package api

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/sessionwake"
)

// ---------------------------------------------------------------------------
// A DECISION ON WORK A MIRRORED SESSION ASKED FOR WAKES THAT SESSION
// (migration 0178; docs/attention.md#delivery). session_wake.go wakes a
// session about what it RAISED or WROTE itself — an ask, a reply draft, a
// page. This file adds two things a session ASKS muster for, whose decision
// the owner makes later, elsewhere:
//
//   - AN OFFICER'S DRAFT. A session enqueues an item for one of the owner's
//     officers (officer_item_enqueue, from its own MCP door), the officer files
//     an outbound draft for that item, and the owner approves or rejects it —
//     and later it is sent, fails, or may have been sent, on whichever attempt
//     of its retry chain (0175) it ends. The session that enqueued the item
//     (officer_items.requested_host / requested_session) is told each.
//   - A STEP-UP CARD. A session's owner-authority MCP call on a guarded
//     machine files a confirmation card instead of running
//     (hostStepUpGate); the owner confirms or refuses it on their phone,
//     dismisses it, or decides the card's draft on another door. The session
//     whose call raised it (StepUpCard.RaisedByHost / RaisedBySession) is told
//     which, once.
//
// 🔴 THE SESSION IS THE ONE MUSTER BOUND, NEVER ONE A CALLER NAMED. Both
// records are written from the bound caller only: officer_item_enqueue's
// toolCaller (the mirror token's host plus the window that host lists running
// the session now) and hostStepUpGate's. No payload field, argument or header
// reaches either, and an item enqueued any other way (the admin API, the
// owner's pages, a plugin, a schedule) names no session and wakes none.
//
// 🔴 EVERYTHING ELSE IS session_wake.go's, UNCHANGED: one renderer (renderWake,
// whose cases for these kinds carry ids, a decision word, an attempt count and
// a tool name from muster's own ledger — and only a rejection's reason the
// OWNER gave, fenced), the send queue and its gate, the session pin, a fresh
// sign-in for the owner's words, "Not delivered" after a day, Retry delivery.
// ---------------------------------------------------------------------------

// wakeOfficerItemRequester is the mirrored session that enqueued the item
// officer draft d was filed for, ok false when no session did (or d is not an
// officer's outbound draft).
func (s *Server) wakeOfficerItemRequester(ctx context.Context, d plugins.Draft) (host, sess string, item int64, ok bool) {
	if s.officerStore == nil || !d.IsOutbound() || d.OfficerItemID == nil || *d.OfficerItemID <= 0 || d.OwnerID <= 0 {
		return "", "", 0, false
	}
	h, sid, err := s.officerStore.ItemRequester(ctx, d.OwnerID, *d.OfficerItemID)
	if err != nil {
		return "", "", 0, false
	}
	host, sess, ok = wakeClaudeTarget(h, sid)
	return host, sess, *d.OfficerItemID, ok
}

// officerRejectionToRequester reports whether the owner's rejection of
// officer draft d goes ONLY to the session that enqueued its item (operator
// decision): a session is recorded on the item and a wake to it would be filed
// (wakeReaches, fileWake's own rule). Then the officer files no draft.refused
// follow-up and no escalation for it — the item is finished quietly
// (outboundSettled) — and the session's line says the officer will not
// redraft (renderWake). false — no session recorded, or none a wake can reach
// — and the rejection goes back to the officer, as it always has: a rejection
// is never dropped on the floor.
func (s *Server) officerRejectionToRequester(ctx context.Context, d plugins.Draft) bool {
	if d.Status != plugins.DraftRejected {
		return false
	}
	octx := s.queuedPrincipal(context.WithoutCancel(ctx), d.OwnerID)
	host, _, _, ok := s.wakeOfficerItemRequester(octx, d)
	return ok && s.wakeReaches(ctx, d.OwnerID, host)
}

// wakeForOfficerDraftDecision is the wake the owner's decision on officer
// draft d owes the session that enqueued its item (host, sess): approved or
// rejected, and — with a rejection — the reason the OWNER gave (0123), fenced.
func wakeForOfficerDraftDecision(d plugins.Draft, item int64, host, sess string) (sessionwake.New, bool) {
	n := sessionwake.New{Kind: sessionwake.KindOfficerDraftDecided, DraftID: d.ID, OfficerItemID: item, Host: host, Session: sess}
	switch d.Status {
	case plugins.DraftApproved:
		n.Decision = sessionwake.DecisionApproved
	case plugins.DraftRejected:
		n.Decision = sessionwake.DecisionRejected
		if d.DecisionReason != nil {
			n.OperatorText = *d.DecisionReason
		}
	default:
		return sessionwake.New{}, false
	}
	return n, true
}

// wakeOfficerDraftDecided files the wake the owner's decision on officer draft
// d owes the session that enqueued its item (decideDraftAs, through
// wakeDraftDecided: every door).
func (s *Server) wakeOfficerDraftDecided(ctx context.Context, d plugins.Draft, via plugins.DraftDecisionSource) {
	octx := s.queuedPrincipal(context.WithoutCancel(ctx), d.OwnerID)
	host, sess, item, ok := s.wakeOfficerItemRequester(octx, d)
	if !ok {
		return
	}
	n, ok := wakeForOfficerDraftDecision(d, item, host, sess)
	if !ok {
		return
	}
	if via.Via == plugins.DraftViaMCP {
		n.ViaHost, n.ViaSession = via.Host, via.Session
	}
	s.fileWake(ctx, d.OwnerID, n)
}

// officerOutcomeDecision is the outcome word of an attempt that ENDED its
// chain in status st, "" for one that did not end it.
func officerOutcomeDecision(st string) string {
	switch plugins.DraftStatus(st) {
	case plugins.DraftSent:
		return sessionwake.DecisionSent
	case plugins.DraftFailed:
		return sessionwake.DecisionFailed
	case plugins.DraftUnconfirmed:
		return sessionwake.DecisionUnconfirmed
	}
	return ""
}

// wakeOfficerOutcomesDue files the outcome wake every approved officer draft
// whose retry chain has since ended owes the session that enqueued its item:
// the ONE writer of an officer outcome line, whichever attempt the chain ended
// on and however it settled (the plugin's result, a lease that lapsed, its
// retries spent). The session and the item are the decision wake's, and so is
// where the approval was made.
func (s *Server) wakeOfficerOutcomesDue(ctx context.Context) {
	if !s.wakesOn() {
		return
	}
	owed, err := s.ext.SessionWakes.OfficerOutcomesOwed(ctx, wakeBatch)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.logger.Printf("session wakes: officer draft outcomes owed: %v", err)
		}
		return
	}
	for _, o := range owed {
		dec := officerOutcomeDecision(o.Status)
		if dec == "" {
			continue
		}
		octx := s.queuedPrincipal(ctx, o.Owner)
		decided, err := s.ext.SessionWakes.Get(octx, o.Decision)
		if err != nil || decided.Kind != sessionwake.KindOfficerDraftDecided || decided.Decision != sessionwake.DecisionApproved {
			continue
		}
		s.fileWake(octx, o.Owner, sessionwake.New{Kind: sessionwake.KindOfficerDraftOutcome, DraftID: o.DraftID,
			OfficerItemID: decided.OfficerItemID, Decision: dec, Count: max(o.Attempt, 1), Host: decided.Host, Session: decided.Session,
			ViaHost: decided.ViaHost, ViaSession: decided.ViaSession})
	}
}

// ---------------------------------------------------------------------------
// Step-up cards.
// ---------------------------------------------------------------------------

// isStepUpCard reports whether a is a step-up card hostStepUpGate filed.
func isStepUpCard(a attention.Ask) bool {
	return a.StepUp != nil && a.Asker.System && a.Asker.Label == hostDecisionLabel
}

// wakeForStepUp is the wake step-up card a owes the session whose call raised
// it, settled as decision: the card and the tool's name only — the tool from
// muster's own ledger (hostStepUpTools), never the call's arguments or its
// output. ok false for any other ask, or a card no session raised.
func wakeForStepUp(a attention.Ask, decision string) (sessionwake.New, bool) {
	if !isStepUpCard(a) {
		return sessionwake.New{}, false
	}
	if _, listed := hostStepUpTools[a.StepUp.Tool]; !listed {
		return sessionwake.New{}, false
	}
	host, sess, ok := wakeClaudeTarget(a.StepUp.RaisedByHost, a.StepUp.RaisedBySession)
	if !ok {
		return sessionwake.New{}, false
	}
	return sessionwake.New{Kind: sessionwake.KindStepUpDecided, AskID: a.ID, Component: a.StepUp.Tool, Decision: decision,
		Host: host, Session: sess}, true
}

// wakeStepUpDecided files the wake step-up card a, just settled as decision,
// owes the session whose call raised it (hostDecisionForAnswer: the phone's
// Confirm or Refuse; wakeDismissed: the owner's Dismiss).
func (s *Server) wakeStepUpDecided(ctx context.Context, a attention.Ask, decision string) {
	if n, ok := wakeForStepUp(a, decision); ok {
		s.fileWake(ctx, a.Owner, n)
	}
}

// wakeStepUpsSettledBy files, for every step-up card about draft d that its
// decision on another door just settled (settleDraftStepUps, which answers
// each with stepUpDraftAnswer), the wake it owes the session whose call raised
// it. A card the phone confirmed or refused is hostDecisionForAnswer's (its
// answer is the option), never this one's.
func (s *Server) wakeStepUpsSettledBy(ctx context.Context, d plugins.Draft, via string) {
	if !s.wakesOn() || s.ext.Attention == nil || d.ID <= 0 {
		return
	}
	dec := sessionwake.DecisionSettledRejected
	if d.Status == plugins.DraftApproved {
		dec = sessionwake.DecisionSettledApproved
	}
	answer := stepUpDraftAnswer(d, via)
	asks, err := s.ext.Attention.Search(ctx, attention.Search{DraftID: d.ID, Limit: attention.MaxSearchResults})
	if err != nil {
		return
	}
	for _, a := range asks {
		if !isStepUpCard(a) || a.StepUp.DraftID != d.ID || a.Status != attention.Answered ||
			a.Answer == hostDecisionConfirm || a.Answer == hostDecisionRefuse || strings.TrimSpace(a.Answer) != answer {
			continue
		}
		s.wakeStepUpDecided(ctx, a, dec)
	}
}

// stepUpCardRef is a step-up wake's card as its line names it.
func stepUpCardRef(w sessionwake.Wake) string {
	return "step-up card #" + strconv.FormatInt(w.AskID, 10) + ", which this session's " + w.Component + " call raised"
}

// officerDraftRef is an officer wake's draft and item as its line names them.
func officerDraftRef(w sessionwake.Wake) string {
	d := "draft #" + strconv.FormatInt(w.DraftID, 10)
	if w.Kind == sessionwake.KindOfficerDraftOutcome && w.Count > 1 {
		d += " (attempt " + strconv.Itoa(w.Count) + " of its retry chain)"
	}
	return d + ", which an officer filed for officer item #" + strconv.FormatInt(w.OfficerItemID, 10) + " that this session enqueued"
}

package api

import (
	"context"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/ui"
)

// AN OFFICER PUSHES ONLY FOR A DECISION (operator decision, 2026-11-18):
// a push from an officer — or from a plugin draft or outreach batch filed on
// an officer's behalf — is sent ONLY when something needs the operator to
// decide, and every one of those opens the exact card the decision is made
// on — an officer HELD (its daily budget reached, its judgement pool held by a
// refused cap escalation) is one: it stops working until the operator raises
// the cap or lifts the hold. Something the operator need not act on (a page
// it published, a reply its plugin refused, its spend unreadable, a host
// instance slow to claim) causes NO push: the operator reads it on the Asks
// page, the officer's page or the sidebar when they look.
//
// 🔴 ONE TABLE, ONE PLACE: officerPushes classifies every push path an
// officer can reach, and each of those call sites asks officerPushSends —
// never a per-site "is it worth a push" judgement. A new officer push path is
// a new officerPush row: TestEveryOfficerPushIsClassified fails until it has
// one.
//
// 🔴 ONE CARD FUNCTION: officerDecisionCard is where each decision's card
// lives. The officer page's "Needs you" section (a later change) retargets a
// decision by editing it, and every officer decision push follows.
//
// ⚠ NOT IN THIS TABLE, ON PURPOSE (the inventory in the change's description
// has the reasons): the paths an officer shares with every agent and that are
// decisions already — a generic attention_ask or page_share_request from an
// officer's pod instance (an ask: its card is the ask, attentionOpenURL), a
// task it moved to review (the task), a host step-up confirmation, a host
// officer's window waiting at a prompt. Those push as any agent's do.

// officerPush is one push path an officer can reach.
type officerPush string

const (
	// officerPushAsk: a builtin officer's officer_ask_person (officerAsker).
	officerPushAsk officerPush = "ask"
	// officerPushEscalation: an item an officer escalated to a person
	// (raiseOfficerNotice): answer it or leave it escalated.
	officerPushEscalation officerPush = "escalation"
	// officerPushDraft: an officer's outbound draft its plugin passed, awaiting
	// approval (notifyDraftAwaitingNoting).
	officerPushDraft officerPush = "draft"
	// officerPushOutreachBatch: an officer's day of follows and likes, awaiting
	// ticks (notifyBatchAwaiting).
	officerPushOutreachBatch officerPush = "outreach_batch"
	// officerPushConnectorRequest: an officer's instance asks for a connector
	// (onConnectorRequested).
	officerPushConnectorRequest officerPush = "connector_request"
	// officerPushDraftRefused: the plugin refused an officer's draft; it went
	// to the session that asked for its item when one did (its wake was filed,
	// wakeOfficerDraftRefused), back to the officer otherwise
	// (notifyDraftRefused).
	officerPushDraftRefused officerPush = "draft_refused"
	// officerPushDraftRetrying: an officer's approved draft failed for the
	// plugins.RetryOwnerPushAttempt-th time with nothing sent and keeps
	// retrying on its own (draftRetryScheduled): once per chain.
	officerPushDraftRetrying officerPush = "draft_retrying"
	// officerPushDraftRetryEnded: an officer's retrying draft ended unsent
	// at its expiry, or stopped retrying on its own after
	// plugins.MaxAutoAttempts (notifyRetryChainEnded): once per budget.
	officerPushDraftRetryEnded officerPush = "draft_retry_ended"
	// officerPushBudget: the officer's daily budget was reached
	// (officerBudgetNotice): it is held for the day unless the operator
	// raises the cap or lifts the hold, on the officer's page.
	officerPushBudget officerPush = "budget"
	// officerPushBudgetUnreadable: the officer's spend could not be read
	// (officerBudgetNotice): muster's own failure, logged; nothing to decide.
	officerPushBudgetUnreadable officerPush = "budget_unreadable"
	// officerPushCapHold: the officer's judgement pool is held, its wall-time
	// cap's escalation refused (officerCapHoldNotice): the officer is stopped
	// until the operator acts on its page.
	officerPushCapHold officerPush = "cap_hold"
	// officerPushAutoRestart: the wake loop restarted an instance from error
	// (officerAutoRestartNotice): it is working again; logged.
	officerPushAutoRestart officerPush = "auto_restart"
	// officerPushRestartHeld: an instance in error with work waiting that
	// the wake loop will not restart — its error class is not one a restart
	// fixes, or the officer's daily auto-restarts are spent
	// (officerAutoRestartNotice): it holds its slot until the operator stops
	// and starts it.
	officerPushRestartHeld officerPush = "restart_held"
	// officerPushUnclaimed: a host instance has not claimed its item
	// (raiseUnclaimedNotice): a dismiss-only notice.
	officerPushUnclaimed officerPush = "unclaimed"
	// officerPushPagePublished: an officer's instance published a page
	// (pagePublished).
	officerPushPagePublished officerPush = "page_published"
	// officerPushPageShared: an officer's instance shared a page under the
	// owner's opt-in to unasked shares (pageShareAuto).
	officerPushPageShared officerPush = "page_shared"
	// officerPushPageUpdated: an officer's instance published a new version
	// of a page (pageUpdated).
	officerPushPageUpdated officerPush = "page_updated"
	// officerPushNotifyOwner: an officer's instance called notify_owner
	// (toolNotifyOwner).
	officerPushNotifyOwner officerPush = "notify_owner"
)

// officerPushes is THE classification: true for a path that asks the operator
// to decide (it pushes, to its card), false for one that only informs (it
// never pushes).
var officerPushes = map[officerPush]bool{
	officerPushAsk:              true,  // answer the officer's question
	officerPushEscalation:       true,  // answer it back to the officer, or leave it escalated
	officerPushDraft:            true,  // approve or reject what goes out
	officerPushOutreachBatch:    true,  // tick the follows and likes that go out
	officerPushConnectorRequest: true,  // approve or deny the access
	officerPushDraftRefused:     false, // it went to the asking session or back to the officer, which handles it
	// The owner approved it and it is still not out: one push at the third
	// failure, one at the end, never per attempt (from the retry contract
	// derived from the operator's ask to approve once and have it retry on
	// its own, with a manual retry via MCP): clear what blocks the plugin,
	// or stop or retry it on its card.
	officerPushDraftRetrying:    true,
	officerPushDraftRetryEnded:  true,
	officerPushBudget:           true,  // raise the cap for today, or lift the hold (the officer's page)
	officerPushBudgetUnreadable: false, // muster's failure, in its log; nothing the operator decides
	officerPushCapHold:          true,  // the officer is held: act on its page
	officerPushAutoRestart:      false, // it is working again; the log says why it was restarted
	officerPushRestartHeld:      true,  // stop and start the instance on the officer's page
	officerPushUnclaimed:        false, // dismiss-only: its card stays on the Asks page
	officerPushPagePublished:    false, // the page is in the officer's chat
	officerPushPageShared:       false, // the owner opted in to unasked shares
	officerPushPageUpdated:      false, // the page is in the officer's chat
	officerPushNotifyOwner:      false, // a decision goes through officer_ask_person instead
}

// officerPushSends reports whether path k pushes: its row in officerPushes.
// A path with no row does not push and is logged — a test pins that every
// constant has one (TestEveryOfficerPushIsClassified), so this is a fallback,
// never a route.
func (s *Server) officerPushSends(k officerPush, owner int64, tag string) bool {
	decides, ok := officerPushes[k]
	switch {
	case !ok:
		s.logger.Printf("push: officer push %q has no classification: dropped (owner %d, tag %s)", k, owner, tag)
	case !decides:
		s.logger.Printf("push: officer %s is information only: not pushed (owner %d, tag %s)", k, owner, tag)
	}
	return ok && decides
}

// officerCardRef names what a decision is made on: ID is the ask's attention
// key, the draft's id, the batch's id, the connector grant's id or the
// officer's id; Team is a team's id when the card is on that team's page (a
// team's connector requests), 0 for the viewer's own; Owner is the officer's
// owner (a person or a team), for its page.
type officerCardRef struct {
	ID    string
	Team  int64
	Owner int64
}

// officerDecisionCard is where decision k's card lives — the address its push
// opens. "" for a path that is not a decision (it never pushes).
func officerDecisionCard(k officerPush, r officerCardRef) string {
	switch k {
	case officerPushAsk, officerPushEscalation:
		// The tap route, resolved when tapped: the open ask's own page (its
		// card, its options and its answer box), else its item on the queue.
		return attentionOpenURL(r.ID)
	case officerPushDraft, officerPushDraftRetrying, officerPushDraftRetryEnded:
		id, err := strconv.ParseInt(r.ID, 10, 64)
		if err != nil {
			return ""
		}
		return draftLink(id)
	case officerPushOutreachBatch:
		return batchLink(r.ID)
	case officerPushConnectorRequest:
		id, err := strconv.ParseInt(r.ID, 10, 64)
		if err != nil {
			return ""
		}
		return connectorRequestLink(r.Team, id)
	case officerPushBudget, officerPushCapHold, officerPushRestartHeld:
		// The officer's page: where a hold is lifted or the day's cap raised.
		// It has no budget section to anchor yet; a later change adds one
		// and retargets this. owner= is always named: the page reads it for
		// a team's officer and accepts the person's own id.
		return officerHref(officerScope{Owner: r.Owner, Team: r.Owner > 0}, r.ID, "")
	}
	return ""
}

// connectorRequestLink is a connector request's card: on the Connectors page,
// or a team's own (/teams/{id}/connectors — a member's /connectors lists only
// their own grants), at its card's anchor (ui.ConnectorRequestAnchor).
func connectorRequestLink(team int64, grantID int64) string {
	page := "/connectors"
	if team > 0 {
		page = "/teams/" + strconv.FormatInt(team, 10) + "/connectors"
	}
	return page + "#" + ui.ConnectorRequestAnchor(grantID)
}

// officerSystemAsk reports whether ask a is one muster raised for a builtin
// officer (its asker is muster, labelled officerAskLabel): the asks
// officerAsker.Raise, raiseOfficerNotice and raiseUnclaimedNotice file.
func officerSystemAsk(a attention.Ask) bool {
	return a.Asker.System && strings.HasPrefix(a.Asker.Label, officerAskLabel(""))
}

// officerAskFiled is askFiled for an ask or notice muster raised for an
// officer on path k: filed as any ask is when k is a decision; otherwise the
// views move (its card is on the Asks page) and nothing is pushed.
func (s *Server) officerAskFiled(k officerPush, a attention.Ask) {
	if s.officerPushSends(k, a.Owner, attentionTag(attentionAskKey(a.ID))) {
		s.askFiled(a)
		return
	}
	s.attentionChanged(a.Owner, attentionAskKey(a.ID))
}

// connectorRequestByOfficer reports whether grant g, requested under ctx, was
// requested by an officer's instance of the request's owner (agentIsOfficer).
func (s *Server) connectorRequestByOfficer(ctx context.Context, g connectors.Grant) bool {
	return s.agentIsOfficer(ctx, g.AgentID, requestOwner(ctx))
}

// agentIsOfficer reports whether agent id is an instance of one of OWNER's
// officers (officerOf, the one binding rule), read from its row: owner is the
// one the push it decides goes to. No agent (0: a mirrored session), no
// officers, a row that cannot be read, or a row that is not owner's: false.
//
// 🔴 THE ROW MUST BE owner's (agentReadCallers "own"). Since 0131 Agents.Get
// can return a member's TEAM agent; this read only decides whether owner's
// push is routed through officerPushes, and it never acts on the row — but a
// row of anyone else's is answered "not an officer's" outright, so a team
// officer's row can never change how a member's own push is sent.
func (s *Server) agentIsOfficer(ctx context.Context, id, owner int64) bool {
	if id <= 0 || owner <= 0 || s.officerRT == nil || s.ext.Agents == nil {
		return false
	}
	a, err := s.ext.Agents.Get(ctx, id)
	if err != nil || a.OwnerID != owner {
		return false
	}
	_, _, ok := s.officerOf(a)
	return ok
}

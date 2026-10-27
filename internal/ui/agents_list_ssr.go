package ui

import (
	"time"

	g "maragu.dev/gomponents"
)

// THE AGENTS LIST IS IN THE AGENTS TAB'S FIRST PAINT.
//
// The tab's document used to draw #agents-list EMPTY and fill it from its own
// `load` request (GET /ui/agents): every visit painted a filter bar over
// nothing, then the cards. The document now carries the cards (ShellData.WithList,
// built by internal/api from the SAME agentsListCards the partial is built from),
// and the live wiring is unchanged — every SSE nudge, resync and local action
// still morphs the list in place.
//
// 🔴 ONE RENDERER. The document's list body and the partial's are both
// agentsListBody of the same views, so they cannot drift apart:
// TestTheAgentsDocumentDrawsTheListPartial (internal/api) compares the two for
// one state, byte for byte.
//
// ⚠ THE FIRST LIVE REFRESH STILL RE-SENDS THE LIST. The SSE stream's open fires
// muster:resync (resyncScript), which refreshes the list right after every load
// and morphs it over the identical server-rendered one. A version/204 protocol
// that answered it with no content was built, MEASURED and removed: layout
// shift 0 with it and 0 / 0.00006 without (64-window list, phone and desktop);
// what it saved was invisible DOM churn (data-list-i restamps, a class rewrite)
// and relative-time labels one second fresher. It cost a full list build before
// the hash anyway, and a header every refresh spec had to work around. The
// morph's shift is what e2e/tests/cls.spec.ts measures.

// shellList is the list a document draws on its first paint, and its session
// switcher (switcher.go), resolved against the same cards.
type shellList struct {
	cards     []AgentCardView
	mirrors   []MirrorCardView
	idleAfter time.Duration
	switcher  *Switcher
}

// WithList is the list's own cards, drawn into #agents-list on the first
// paint, and the session switcher docked below it, resolved against those
// cards and the viewer's recently opened refs (newest first) — as
// RenderAgentsList resolves both, so the first paint and every refresh agree.
// idleAfter is the Idle section's threshold, for its header's hint.
func (d ShellData) WithList(list []AgentCardView, mirrors []MirrorCardView, refs []RecentRef, idleAfter time.Duration) ShellData {
	d.list = &shellList{cards: list, mirrors: mirrors, idleAfter: idleAfter, switcher: agentsSwitcher(list, mirrors, refs)}
	return d
}

// agentsSwitcher is the Agents list's switcher: the page has no session of its
// own, so nothing is current and nothing is left out.
func agentsSwitcher(list []AgentCardView, mirrors []MirrorCardView, refs []RecentRef) *Switcher {
	return NewSwitcher(list, mirrors, refs, SwitcherCurrent{})
}

// agentsListBody is what #agents-list holds: the one list, by activity, pins
// first, the Idle section last (agentsListNodes), or the empty state.
func agentsListBody(list []AgentCardView, mirrors []MirrorCardView, idleAfter time.Duration) g.Node {
	return agentsListNodes(list, mirrors, idleAfter)
}

// drawnList is what a document's #agents-list carries of its list: its cards;
// nothing for a list the document does not draw (nil).
func drawnList(l *shellList) g.Node {
	if l == nil {
		return nil
	}
	return agentsListBody(l.cards, l.mirrors, l.idleAfter)
}

// listTrigger is the list's hx-trigger: no first load for a list the document
// drew (see agentsListTrigger).
func listTrigger(l *shellList) string {
	if l != nil {
		return agentsListLiveTrigger
	}
	return agentsListTrigger
}

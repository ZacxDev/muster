package ui

import (
	"slices"
	"sort"
	"strings"
	"time"

	g "maragu.dev/gomponents"
)

// THE AGENTS LIST IS ONE FLAT LIST, MOST RECENTLY ACTIVE FIRST (operator ask:
// "sort agents by most recently active, flat, not separated by host"). Before
// it the list drew every dispatched agent ahead of every mirrored window, so an
// agent idle for a week sat above a Claude Code window that answered a minute
// ago. The host stays visible on each card through its host icon (hostMark /
// clusterMark); it no longer decides the order.
//
// listEntry is one card of that list: exactly one of agent and mirror is set.
type listEntry struct {
	agent  *AgentCardView
	mirror *MirrorCardView
}

// pinned reports whether the viewer pinned the entry (internal/pins).
func (e listEntry) pinned() bool {
	if e.agent != nil {
		return e.agent.Pinned
	}
	return e.mirror.Pinned
}

// idle reports whether the entry is drawn in the Idle section (MarkIdle).
func (e listEntry) idle() bool {
	if e.agent != nil {
		return e.agent.Idle
	}
	return e.mirror.Idle
}

// activeAt is the entry's LAST ACTIVITY — the time its card prints as "active
// … ago" (cardActivity), so the order and the label are one number:
//
//   - a dispatched agent: agents.LastActivity (the newer of its row's
//     updated_at and its newest persisted transcript message: a turn, a status
//     write, a rename), composed into LastActiveAt by internal/api;
//   - a mirrored window: the time its host last saw the window active
//     (mirror.SessionView.Activity, floored to the host's minute bucket). Zero
//     when the host did not say (an older muster-mirror).
func (e listEntry) activeAt() time.Time {
	if e.agent != nil {
		return e.agent.lastActive()
	}
	return e.mirror.ActiveAt
}

// orderList is the Agents list's order — 🔴 ONE RULE, ONE PLACE:
//
//  1. the viewer's PINNED entries first, then the rest (pins are per viewer,
//     which is why this happens here and not in agents.SortByLastActivity or
//     mirror.Ordered, whose orders are viewer-free);
//  2. within each group, most recently active first (activeAt), agents and
//     mirrored windows interleaved;
//  3. an entry with NO known activity (a mirrored window whose host did not say)
//     after every entry that has one;
//  4. ties keep the order the entries arrive in — agents first, each kind in
//     its own already-decided order (agents.SortByLastActivity breaks its own
//     ties by id; mirror.Ordered by host, then the host's order) — so two cards
//     active in the same instant never swap places between two renders.
func orderList(agents []AgentCardView, mirrors []MirrorCardView) []listEntry {
	out := make([]listEntry, 0, len(agents)+len(mirrors))
	for i := range agents {
		out = append(out, listEntry{agent: &agents[i]})
	}
	for i := range mirrors {
		out = append(out, listEntry{mirror: &mirrors[i]})
	}
	sort.SliceStable(out, func(i, j int) bool { return listBefore(out[i], out[j]) })
	return out
}

// listBefore is orderList's comparison: whether a is drawn above b.
func listBefore(a, b listEntry) bool {
	if pa, pb := a.pinned(), b.pinned(); pa != pb {
		return pa
	}
	// An unknown (zero) time is before every known one, so After alone puts
	// it last (rule 3); a separate branch for it was an equivalent mutant.
	return a.activeAt().After(b.activeAt())
}

// waiting reports whether the entry waits on the viewer: whether the Agents
// list's "Waiting on you" chip (listChips' "waiting") shows its card — the
// chip's token among the card's data-wait words (waitTokens): an attention
// item that files under "you", or a dispatched agent blocked at a permission
// prompt. 🔴 THE ONE PREDICATE the session switcher's Waiting view and waiting
// dot (NewSwitcher, switcherItems), the sidebar's order (navBefore) and — by
// the chip's own token — the list's chip all read, so none of them can
// disagree about who is waiting.
func (e listEntry) waiting() bool {
	var words string
	if e.agent != nil {
		words = waitTokens(e.agent.Attention, e.agent.AwaitingApproval)
	} else {
		words = waitTokens(e.mirror.Attention, false)
	}
	return slices.Contains(strings.Fields(words), waitingChip().Token)
}

// waitingChip is the list's "Waiting on you" chip.
func waitingChip() listChip {
	c, _ := chipByID("waiting")
	return c
}

// chief reports whether the entry is the viewer's chief agent
// (AgentCardView.IsChief, internal/api's one chief predicate). A mirrored
// window is never the chief.
func (e listEntry) chief() bool { return e.agent != nil && e.agent.IsChief }

// primaryChief reports whether the entry is THE chief — where `/` opens
// (AgentCardView.PrimaryChief, internal/api's primaryChief).
func (e listEntry) primaryChief() bool { return e.agent != nil && e.agent.PrimaryChief }

// navOrder is the SIDEBAR's order of the Agents list's entries (nav_agents.go)
// — 🔴 ONE RULE, ONE PLACE, beside orderList, whose output it re-sorts:
//
//  1. the chief agent first (the operator's main conversation, pinned) —
//     THE chief, where `/` opens (primaryChief), above any other agent that
//     is also a chief (a relabelled reserved slug beside a labelled chief);
//  2. then every entry waiting on the operator (listEntry.waiting);
//  3. then the rest, most recently active first (activeAt), an entry with no
//     known activity last;
//  4. ties keep orderList's order (a stable sort over its output), so two
//     entries active in the same instant never swap places between renders.
//
// Unlike orderList it does not lift the viewer's pins: the sidebar is a list
// of conversations ordered by who needs the operator, and the Agents list,
// its Pins view and the switcher keep the pins.
func navOrder(agents []AgentCardView, mirrors []MirrorCardView) []listEntry {
	out := orderList(agents, mirrors)
	sort.SliceStable(out, func(i, j int) bool { return navBefore(out[i], out[j]) })
	return out
}

// navBefore is navOrder's comparison: whether a is drawn above b.
func navBefore(a, b listEntry) bool {
	if pa, pb := a.primaryChief(), b.primaryChief(); pa != pb {
		return pa
	}
	if ca, cb := a.chief(), b.chief(); ca != cb {
		return ca
	}
	if wa, wb := a.waiting(), b.waiting(); wa != wb {
		return wa
	}
	return a.activeAt().After(b.activeAt())
}

// listCard renders one entry as its card.
func listCard(e listEntry) g.Node {
	if e.agent != nil {
		return agentCard(*e.agent)
	}
	return mirrorCard(*e.mirror)
}

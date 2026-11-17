package api

import (
	"context"
	"slices"
	"strconv"

	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
)

// THE ONE OPEN COUNT. Four surfaces say how much waits on the reader — the ☰
// menu badge (handleNavWaiting, and a session switcher page's first count:
// switcher), the sidebar's More badge (handleAttentionBadge), the Asks page's
// "N open" (renderAsksPage, then the feed's script: ui asks_card.go count) and
// the Log tab's "Open" pill (askLogPanelHTML). They used to count four
// different things (the sidebar's waiting rows; the queue; the Open feed's
// cards without the drafts; the log's rows with no outcome yet), so one
// screen could say 4, 6, 6 and 3 at once. Every one of them now draws
// asksPendingOf, and TestEverySurfaceShowsTheSameOpenCount pins that they
// agree over one state.

// asksPendingOf is how many things wait on ctx's reader, over ONE read: the
// Asks page's items (pageAttentionItems — a waiting window session state
// covers is one of its stalled rows instead), reply drafts included, without
// the asks the reader only watches (waitingOnReader), plus session state's
// stalled rows (episodes: episodeRowsHTML's count).
func asksPendingOf(ctx context.Context, pageItems []attentionItem, episodes int) int {
	return len(waitingOnReader(ctx, pageItems)) + episodes
}

// asksPendingCount is asksPendingOf over a fresh read of ctx's principal's
// queue and hub (readAsks: one hub read), and the read's error, if any (the
// count is then of what could be read).
func (s *Server) asksPendingCount(ctx context.Context) (int, error) {
	rd := s.readAsks(ctx)
	items, err := s.attentionItemsWith(ctx, rd.hosts, rd.waiting)
	_, n := s.episodeRowsHTML(ctx, rd, s.now())
	return asksPendingOf(ctx, s.asksPageItems(ctx, items, rd.hosts), n), err
}

// asksPageItems is the Asks page's items — what the Open feed and the drafts
// draw and THE ONE OPEN COUNT counts: pageAttentionItems, and an agent of the
// reader's blocked on a permission prompt (approvalItems). One function for
// the feed (askFeedNow) and the count (asksPendingCount), so they cannot
// disagree about what is open.
func (s *Server) asksPageItems(ctx context.Context, items []attentionItem, hs []mirror.HostView) []attentionItem {
	page := s.pageAttentionItems(items, hs)
	return append(page, s.approvalItems(ctx, items)...)
}

// approvalKey is an approval-blocked agent's item key (approvalItems).
func approvalKey(agentID int64) string { return "approval-" + strconv.FormatInt(agentID, 10) }

// approvalItems is every provisioned agent of ctx's principal blocked at a
// permission prompt (AwaitingApproval: the "awaiting approval" badge, the
// sidebar's waiting mark) with no ask of its own already in items, as a
// waiting item (attentionKindSession) whose View in session is the agent's
// page. 🔴 IT IS NOT A QUEUE ITEM: it is not pushed and not in attention_list
// (the agent's own prompt is its push) — it is drawn and counted on the Asks
// page, so the one open count keeps every session the sidebar marks waiting
// (the audit of #787: the ☰ used to count it, then lost it).
func (s *Server) approvalItems(ctx context.Context, items []attentionItem) []attentionItem {
	if s.ext.Agents == nil {
		return nil
	}
	asked := map[int64]bool{}
	for _, it := range items {
		if it.Kind == attentionKindAsk && it.card.agentID > 0 {
			asked[it.card.agentID] = true
		}
	}
	var out []attentionItem
	// Only the agents the approval ledger holds as blocked (a handful at
	// most), each read as ctx's principal: one that is not the reader's is
	// not found and is skipped. No list of every agent on each count.
	for _, id := range s.awaitingApprovalIDs() {
		if asked[id] {
			continue
		}
		a, err := s.ext.Agents.Get(ctx, id)
		if err != nil || !a.Provisioned() {
			continue
		}
		it := attentionItem{Key: approvalKey(a.ID), Kind: attentionKindSession, Source: firstNonBlank(a.DisplayName, a.Name),
			Text: sessionWaitingText, Link: s.agentPageHref(ctx, a), subject: focus.AgentSubject(a.ID)}
		it.card.agentID = a.ID
		out = append(out, it)
	}
	s.withNames(ctx, out)
	return out
}

// menuCount is the ☰ badge's count where the Asks queue is served — THE ONE
// OPEN COUNT — and false where it is not (the badge then counts the sidebar's
// waiting rows, ui.NavWaitingCount). A failed read is logged and counts what
// could be read.
func (s *Server) menuCount(ctx context.Context) (int, bool) {
	if s.ext.Attention == nil {
		return 0, false
	}
	n, err := s.asksPendingCount(ctx)
	if err != nil {
		s.logger.Printf("nav waiting: asks: %v", err)
	}
	return n, true
}

// awaitingApprovalIDs is every agent id AwaitingApproval holds true for now
// (the same ledger, pruned the same way), in id order.
func (s *Server) awaitingApprovalIDs() []int64 {
	l := &s.approvals
	l.mu.Lock()
	defer l.mu.Unlock()
	l.init()
	l.pruneLocked(s.now())
	seen := map[int64]bool{}
	for id := range l.blocked {
		seen[id] = true
	}
	for _, o := range l.owners {
		seen[o.agentID] = true
	}
	ids := make([]int64, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

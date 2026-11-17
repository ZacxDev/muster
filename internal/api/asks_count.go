package api

import (
	"context"
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
	return asksPendingOf(ctx, s.pageAttentionItems(items, rd.hosts), n), err
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

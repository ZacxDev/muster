package api

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE CHIEF'S CHAT DRAWS A ROUTE'S ANSWER AS A CARD — FROM THE ROUTE'S RECORD,
// NOT FROM THE ROW'S TEXT. A session's answer to a request routed from this
// chat reaches the chief as an untyped user row: muster's head (routeNote), a
// preface for the model and the answer behind a fence (routeAnswerText). The
// person sees a "From <session>" card with the answer (ui.RouteAnswerCard);
// the row's exact text is in its ⓘ.
//
// 🔴 THE TEXT ONLY POINTS; THE STORE DECIDES. An untyped user row can be
// written by more than muster (`muster chief ask` over a token), so a row is
// drawn as answers only when EVERY route id its heads name is a route of THIS
// chat's chief that has an answer, and the row carries the head muster itself
// writes for that route (routeNote + routeAnswerNoteFor, recomputed here from
// the record — answerOnRecord's test). The card's name and body come from the
// record, so a forged row can at most redraw a real answer. Anything less is
// drawn as before.

// routeAnswerHeadRE finds the route id of each head (routeNote) opening a
// paragraph of a row.
var routeAnswerHeadRE = regexp.MustCompile(`(?m)^\[muster\] Route (rt_[0-9a-f]+) \(`)

// routeAnswerCardsFor is the answer cards of every row in lines that carries
// only answers to routes of chief agentID, by row id; nil when there are none.
func (s *Server) routeAnswerCardsFor(ctx context.Context, agentID int64, lines []ui.ChatLine) map[int64][]ui.RouteAnswerCard {
	if s.ext.Routes == nil {
		return nil
	}
	var out map[int64][]ui.RouteAnswerCard
	var fi focusIndex
	fiRead := false
	for _, l := range lines {
		if l.ID <= 0 || l.Role != "user" || l.Typed || l.Relay != nil || (l.Kind != "" && l.Kind != "text") {
			continue
		}
		heads := routeAnswerHeadRE.FindAllStringSubmatch(l.Content, -1)
		if len(heads) == 0 {
			continue
		}
		cards := make([]ui.RouteAnswerCard, 0, len(heads))
		for _, h := range heads {
			r, err := s.ext.Routes.Route(ctx, h[1])
			if err != nil || r.AgentID == nil || *r.AgentID != agentID || r.ReplyBody == "" {
				cards = nil
				break
			}
			head := routeNote(r, routeAnswerNoteFor(r))
			if !strings.HasPrefix(l.Content, head) && !strings.Contains(l.Content, "\n\n"+head) {
				cards = nil
				break
			}
			if !fiRead {
				fi, fiRead = s.focusIndex(ctx), true
			}
			from := ""
			if sub, err := focus.ParseSubject(r.TargetKind + ":" + r.TargetKey); err == nil {
				from = s.humanName(ctx, fi, sub)
			}
			if from == "" {
				from = "a session"
			}
			cards = append(cards, ui.RouteAnswerCard{From: from, OneWay: r.OneWay, Body: r.ReplyBody})
		}
		if len(cards) == 0 {
			continue
		}
		if out == nil {
			out = map[int64][]ui.RouteAnswerCard{}
		}
		out[l.ID] = cards
	}
	return out
}

// A ROUTED PASTE IN A MIRRORED SESSION IS A CARD ONLY WHEN MUSTER HAS IT ON
// RECORD. The block's shape (plugins.ParseRoutedBlock) is not evidence muster
// wrote it: an agent typing into its own pane, or any process on the host, can
// type the same shape — and the card's ⓘ would then say muster handed it over.
// So the view (ui.MirrorTranscript.RoutedPasteOnRecord) asks here, and the
// card is drawn only when the route the block names exists for the viewer,
// targets the Claude session this window shows now (and the host it ran on,
// when recorded), recorded the SHA-256 of exactly this text when it was queued
// (Route.PasteSHA256, migration 0153), and was DELIVERED: its send-queue row
// sent, the prompt no earlier than the delivery. The view asks only for the
// FIRST prompt naming a route (ui.MirrorTranscript), so a replay of the same
// block is a bubble too. Anything else is the bubble.

// routedPasteOnRecord is that check for the window host/session/window as r's
// viewer sees it: text is the prompt as muster pasted it
// (plugins.RoutedPromptText), recorded at at.
func (s *Server) routedPasteOnRecord(r *http.Request, host, session, window, routeID, text string, at time.Time) bool {
	if s.ext.Routes == nil || s.ext.SendQueue == nil {
		return false
	}
	rt, err := s.ext.Routes.Route(r.Context(), routeID)
	if err != nil || rt.SendQueueID == nil {
		return false
	}
	h, e, _, _, ok := s.mirrorEntry(r, host, session, window)
	if !ok || !routedPasteMatches(rt, h.Host, e.ClaudeSession, text) {
		return false
	}
	it, err := s.ext.SendQueue.Get(r.Context(), *rt.SendQueueID)
	return err == nil && routedPasteDelivered(it, at)
}

// routedPasteMatches is the record's half of the check: route rt was pasted
// into Claude session sid on host, as text — the prompt as the view READ it
// (plugins.RoutedPromptText), hashed with RoutedReadSHA256.
func routedPasteMatches(rt plugins.Route, host, sid, text string) bool {
	return rt.TargetKind == string(focus.KindMirror) && sid != "" && rt.TargetKey == sid &&
		(rt.TargetHost == "" || rt.TargetHost == host) &&
		rt.PasteSHA256 != "" && rt.PasteSHA256 == plugins.RoutedReadSHA256(text)
}

// routedPasteClockSkew is how far a prompt's time (the host's clock, as the
// CLI wrote it) may fall before its send-queue row's claim (Postgres's clock)
// and still be that delivery: the paste follows the claim, so only the two
// clocks' difference can put it earlier.
const routedPasteClockSkew = time.Minute

// routedPasteDelivered is the delivery's half: the route's send-queue row
//   - was SENT, and the prompt was recorded no earlier than the row's delivery
//     began (its claim), within routedPasteClockSkew; or
//   - is still SENDING, claimed no later than the prompt: a LIVE view decides
//     when the prompt arrives, and the host confirms the paste after the CLI
//     has recorded it (measured on 404 real pastes: 275 rows flipped to sent
//     after the prompt's time, p50 12 ms, max 32 s). The hash still binds the
//     card to the exact text.
//
// Queued, cancelled or failed is no delivery (a failed one fails closed: the
// paste may have landed, but muster does not say so). A prompt with no time is
// not taken for a delivery.
func routedPasteDelivered(it sendqueue.Item, at time.Time) bool {
	if at.IsZero() || it.ClaimedAt.IsZero() {
		return false
	}
	switch it.State {
	case sendqueue.Sent:
		return !at.Before(it.ClaimedAt.Add(-routedPasteClockSkew))
	case sendqueue.Sending:
		return !at.Before(it.ClaimedAt)
	}
	return false
}

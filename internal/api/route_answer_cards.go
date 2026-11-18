package api

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/plugins"
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
// when recorded), and recorded the SHA-256 of exactly this text when it was
// queued (Route.PasteSHA256, migration 0153). Anything else is the bubble.

// routedPasteOnRecord is that check for the window host/session/window as r's
// viewer sees it.
func (s *Server) routedPasteOnRecord(r *http.Request, host, session, window, routeID, text string) bool {
	if s.ext.Routes == nil {
		return false
	}
	rt, err := s.ext.Routes.Route(r.Context(), routeID)
	if err != nil {
		return false
	}
	h, e, _, _, ok := s.mirrorEntry(r, host, session, window)
	if !ok {
		return false
	}
	return routedPasteMatches(rt, h.Host, e.ClaudeSession, text)
}

// routedPasteMatches is the record's half of the check: route rt was pasted
// into Claude session sid on host, as text.
func routedPasteMatches(rt plugins.Route, host, sid, text string) bool {
	return rt.TargetKind == string(focus.KindMirror) && sid != "" && rt.TargetKey == sid &&
		(rt.TargetHost == "" || rt.TargetHost == host) &&
		rt.PasteSHA256 != "" && rt.PasteSHA256 == plugins.RoutedPasteSHA256(text)
}

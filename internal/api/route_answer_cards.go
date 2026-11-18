package api

import (
	"context"
	"regexp"
	"strings"

	"github.com/ZacxDev/muster/internal/focus"
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

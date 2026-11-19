package api

import (
	"context"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE SESSION CARDS A CHIEF'S REPLY CAN DRAW (ui/session_card.go): the
// viewer's OWN sessions, keyed by every key that may name one —
//
//   - a mirrored window's conversation id as the chief's tools spell it
//     ("mirror:<key>", focus.Subject), and its raw tmux target
//     (mirror.WhereTarget: "tmux <session>:<index> @ <host>");
//   - an agent's id ("agent:<n>").
//
// 🔴 OWN, NEVER VISIBLE. The hub lists a member's TEAMS' hosts too, and the
// agents list holds a team's agents since 0131: both are filtered to the
// viewer's own (Owner), so a marker naming a team's or another owner's session
// resolves to nothing and draws "a session" — no card, no title.
//
// A key two of them share (one tmux target on two hosts cannot happen; a
// conversation shown in two windows can) is kept with no link: no card.
//
// Every read runs under ctx's principal; the whole index is read once, lazily,
// on the first reference a reply makes (ui.MentionIndex).

// sessionCardToolNote ends the description of every chief tool that hands out
// a session's id (fleet_list, focus_search, sessions_overview): how to name
// one to the owner. The system prompt says the same (agentspec chiefLiveReads).
const sessionCardToolNote = " To name a session to your owner, write [[session:<its id>]]: muster draws it as a card " +
	"they tap to open it. Never write its where, tmux address or id bare."

// chatMentionIndex is mentionIndex for an agent's chat: the chief's replies
// draw session cards; other agents' replies link mentions only.
func (s *Server) chatMentionIndex(ctx context.Context, chief bool) *ui.MentionIndex {
	mx := s.mentionIndex(ctx)
	if chief {
		mx.WithSessionCards(func() map[string]ui.SessionCard { return s.sessionCards(ctx) })
	}
	return mx
}

// sessionCards is the viewer's own sessions' cards by key (see above).
func (s *Server) sessionCards(ctx context.Context) map[string]ui.SessionCard {
	owner := requestOwner(ctx)
	out := map[string]ui.SessionCard{}
	add := func(key string, c ui.SessionCard) {
		if prev, ok := out[key]; ok && prev.Href != c.Href {
			out[key] = ui.SessionCard{} // shared: no card
			return
		}
		out[key] = c
	}
	if owner <= 0 {
		return out
	}
	fi := s.focusIndex(ctx)
	if s.ext.Mirror != nil {
		for _, e := range mirror.Ordered(s.ext.Mirror.Hosts(ctx)) {
			// The hub's own entries carry their owner; 0 is a fake's "the
			// viewer's", never another owner's.
			if e.Owner != 0 && e.Owner != owner {
				continue
			}
			n := fi.mirrorName(e)
			title := e.PlainTitle()
			if n.Human() {
				title = n.Text
			}
			c := ui.SessionCard{Title: title, Computer: e.Host, Status: sessionCardStatus(e.Status),
				Href: ui.MirrorCardHref(ui.MirrorCardView{Host: e.Host, Session: e.Session, Window: e.Window,
					Claude: e.Claude, AgentView: e.AgentView()}),
				Address: e.WhereTarget()}
			add(e.WhereTarget(), c)
			if key := e.SessionKey(); key != "" {
				add(focus.MirrorSubject(key).String(), c)
			}
		}
	}
	if s.ext.Agents != nil {
		list, err := s.ext.Agents.List(ctx)
		if err != nil {
			s.logger.Printf("session cards: list agents: %v", err)
		}
		busy := map[string]bool{}
		for _, a := range s.activeAgentsList(owner) {
			busy[a.Name] = a.InFlight
		}
		for _, a := range ownedBy(agents.ProvisionedOnly(list), owner) {
			st := ui.SessionCardIdle
			if busy[a.Name] {
				st = ui.SessionCardBusy
			}
			add(focus.AgentSubject(a.ID).String(), ui.SessionCard{Title: fi.agentName(a).Text, Status: st,
				Href: ui.AgentPath(a.Name), Address: a.Name})
		}
	}
	return out
}

// sessionCardStatus is a mirrored window's status as its card's dot says it:
// busy, waiting on its owner, or — anything else, a host that does not report
// status included — idle.
func sessionCardStatus(st string) string {
	switch mirror.NormalizeStatus(st) {
	case mirror.StatusBusy:
		return ui.SessionCardBusy
	case mirror.StatusWaiting:
		return ui.SessionCardWaiting
	}
	return ui.SessionCardIdle
}

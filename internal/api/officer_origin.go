package api

import (
	"context"
	"errors"
	"html"
	"net/http"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/itemorigin"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// "SHOW SESSION" ON AN OFFICER'S NEEDS-YOU ITEMS (operator decision: "Exact
// moment"). Each ask, draft and refusal an officer's instance makes records
// which instance made it, the chat session it was working in and the moment
// in that session (migration 0185's origin_* columns), and the officer page
// draws a "Show session" button that opens the session scrolled to that
// moment (GET /agents/{name}?session=N&at=M: agentMomentMessages, the page's
// moment marker).
//
// 🔴 BOUND TO THE CALLER. withOfficerOrigin is installed by the officer tool
// dispatcher (dispatchOfficerTool) from `a`, the agent its token resolved —
// the same binding that refuses an instance acting for another officer — and
// the stores read the origin from the context alone (internal/itemorigin).
// Nothing in a tool call's arguments is read for it.
//
// 🔴 ONLY THE OWNER'S OWN OFFICER SESSION. A link is drawn only to an agent
// that is the item's owner's officer's own instance (officerOriginHref), the
// database refuses a row whose origin agent is another owner's
// (officer_origin_is_owners, 0185), an escalation records its origin on the
// owner's own notice only (builtin.noticesFor), and the agent page answers
// another owner's agent as a 404 (row-level security).
//
// WHAT "THE MOMENT" IS. An officer's tool calls are not chat_messages rows,
// so the moment is the prompt row of the TURN the call ran in (the per-turn
// record, turnInputOf): the marker is drawn before it, and the turn — its
// prompt, its narration and its tool calls — follows.
// ---------------------------------------------------------------------------

// sessionByKeyer is the agents store's session-by-key read
// (agents.PGStore.SessionByKey), asserted rather than added to agents.Store.
type sessionByKeyer interface {
	SessionByKey(ctx context.Context, agentID int64, sessionKey string) (agents.ChatSession, error)
}

// chatMessageReader is the agents store's one-message read
// (agents.PGStore.ChatMessage), asserted rather than added to agents.Store.
type chatMessageReader interface {
	ChatMessage(ctx context.Context, id int64) (agents.ChatMessage, error)
}

// withOfficerOrigin is ctx carrying where officer instance a's tool call is
// made from, resolved only when a row is made (itemorigin.Resolve):
//
//   - THE MOMENT IS THE TURN'S OWN PROMPT ROW (turnInputOf: the per-turn
//     record every officer turn registers, OfficerTurnContext), read at the
//     call — never "the newest message", which a row landing mid-turn, or a
//     second turn, would move. Its session is that row's own.
//   - A turn muster does not know (none registered, two overlapping, a chat
//     lane's: lanes record no input) records the session as a whole — a
//     chat lane's own session (its chat's key), else the session the
//     instance's primary turns land in — and no moment.
func (s *Server) withOfficerOrigin(ctx context.Context, a agents.Agent) context.Context {
	if a.ID <= 0 || s.ext.Agents == nil {
		return ctx
	}
	chat, lane := chatLaneCall(ctx)
	var input int64
	if !lane {
		input = s.turnInputOf(ctx, a.ID)
	}
	agent := a.ID
	return itemorigin.With(ctx, func(rctx context.Context) (itemorigin.Origin, bool) {
		o := itemorigin.Origin{AgentID: agent}
		if input > 0 {
			if r, ok := s.ext.Agents.(chatMessageReader); ok {
				m, err := r.ChatMessage(rctx, input)
				switch {
				case err == nil && m.AgentID == agent && m.SessionID > 0:
					o.SessionID, o.MessageID = m.SessionID, input
					return o, true
				case err != nil:
					s.logger.Printf("officers: origin of agent %d's call: its turn's prompt %d: %v", agent, input, err)
				}
			}
		}
		if lane {
			if k, ok := s.ext.Agents.(sessionByKeyer); ok {
				if cs, err := k.SessionByKey(rctx, agent, officers.ChatSessionKey(chat)); err == nil {
					o.SessionID = cs.ID
				} else if !errors.Is(err, pgx.ErrNoRows) {
					s.logger.Printf("officers: origin of agent %d's call: its chat session: %v", agent, err)
				}
			}
		} else {
			o.SessionID = s.latestSessionIDs(rctx, []int64{agent})[agent]
		}
		return o, true
	})
}

// officerOriginHref is the "Show session" link of an item of scope sc's
// officer made with origin o, or "" for none:
//
//   - a POD officer: the session scrolled to the moment (ui.AgentMomentHref)
//     when both are recorded, the session as a whole when only it is, and —
//     an item made before 0185, or with no session — the latest session of
//     the instance named by fallback (the draft's own agent, or the
//     officer's first instance);
//   - a HOST officer: its conversation is the instance's MIRRORED session,
//     not the agent row's chat, so the link is the instance's current
//     window's mirrored view (hostOriginHref) — WITHOUT a position: the
//     mirrored view has no message rows a moment could name.
//
// 🔴 The agent named is checked to be ONE OF THIS OFFICER'S INSTANCES of this
// owner (agent owner and name): anything else draws no link.
func (s *Server) officerOriginHref(ctx context.Context, sc officerScope, officer string, o itemorigin.Origin, fallback string) string {
	if s.ext.Agents == nil {
		return ""
	}
	var a agents.Agent
	var err error
	switch {
	case o.AgentID > 0:
		a, err = s.ext.Agents.Get(ctx, o.AgentID)
	case fallback != "":
		a, err = s.ext.Agents.GetByName(ctx, fallback)
	default:
		return ""
	}
	if err != nil || a.OwnerID != sc.Owner || !s.isOfficerInstance(sc.Owner, officer, a.Name) {
		return ""
	}
	if ro, ok := s.runningOfficer(sc.Owner, officer); ok && ro.OnHost() {
		return s.hostOriginHref(ctx, sc, officer, a.Name)
	}
	if o.AgentID > 0 && o.SessionID > 0 {
		return ui.AgentMomentHref(a.Name, o.SessionID, o.MessageID)
	}
	return ui.AgentSessionHref(a.Name, s.latestSessionIDs(ctx, []int64{a.ID})[a.ID])
}

// hostOriginHref is host officer instance name's mirrored view — its newest
// spawn's window, read as the officer's owner as its page reads it
// (officerInstances) — or "" when it has no live window.
func (s *Server) hostOriginHref(ctx context.Context, sc officerScope, officer, name string) string {
	hs := s.officerHostSpawns()
	if hs == nil {
		return ""
	}
	spawns, err := hs.LatestHostSpawns(s.officerPrincipal(ctx, sc.Owner), officer)
	if err != nil {
		s.logger.Printf("officers: read %s's host spawns for Show session: %v", officer, err)
		return ""
	}
	h, ok := spawns[name]
	if !ok || h.Window == "" || h.MissingListings >= officers.HostSpawnMissingToClose {
		return ""
	}
	owner := int64(0)
	if sc.Team {
		owner = sc.Owner
	}
	return ui.MirrorRef{Owner: owner, Host: h.Host, Session: mirror.AgentSpawnSession, Window: h.Window}.ViewPath()
}

// isOfficerInstance reports whether name is one of owner's officer's
// instance names (every slot, chat-only ones included).
func (s *Server) isOfficerInstance(owner int64, officer, name string) bool {
	o, ok := s.runningOfficer(owner, officer)
	if !ok {
		return false
	}
	for _, p := range o.Pools() {
		for n := 1; n <= p.Slots(); n++ {
			if p.InstanceName(n) == name {
				return true
			}
		}
	}
	return false
}

// officerFirstInstance is owner's officer's first instance name ("" when it
// does not run): an item made before 0185 has no origin, and opens it.
func (s *Server) officerFirstInstance(owner int64, officer string) string {
	o, ok := s.runningOfficer(owner, officer)
	if !ok {
		return ""
	}
	return o.Pools()[0].InstanceName(1)
}

// showSessionHTML is the "Show session" button for href ("" draws nothing);
// moment says whether it opens at the moment or the session as a whole.
func showSessionHTML(href string, moment bool) string {
	if href == "" {
		return ""
	}
	what := "whole"
	if moment {
		what = "moment"
	}
	return `<a href="` + html.EscapeString(href) + `" class="` + ui.OfficerShowSessionClass + `" data-show-session="` + what + `">Show session</a>`
}

// agentMomentParam is the agent page's ?at= (ui.AgentMomentParam): a
// chat_messages id, 0 for none or a value that is not one.
func agentMomentParam(r *http.Request) int64 {
	n, err := strconv.ParseInt(r.URL.Query().Get(ui.AgentMomentParam), 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// momentLead is how many messages before a moment the agent page draws, and
// momentReach how many messages back it reads at most to reach one.
const (
	momentLead  = 20
	momentReach = 5 * chatHistoryCap
)

// agentMomentMessages is session sessionID's messages for the agent page: the
// newest chatHistoryCap, as always — and, for a moment older than them, read
// back page by page (at most momentReach messages) until it is in, keeping
// momentLead messages before it. A moment that is not this session's simply
// is not found: the page draws the newest messages and no marker moves.
func (s *Server) agentMomentMessages(ctx context.Context, sessionID, moment int64) []agents.ChatMessage {
	msgs, _ := s.ext.Agents.ListRecentChatMessages(ctx, sessionID, chatHistoryCap)
	if moment <= 0 || len(msgs) == 0 || msgs[0].ID <= moment {
		return msgs
	}
	for len(msgs) < momentReach && msgs[0].ID > moment {
		older, err := s.ext.Agents.ListChatMessagesPage(ctx, sessionID, msgs[0].ID, chatHistoryCap)
		if err != nil || len(older) == 0 {
			break
		}
		msgs = append(older, msgs...)
	}
	// Keep momentLead messages before the moment, and everything after it.
	i := 0
	for i < len(msgs) && msgs[i].ID <= moment {
		i++
	}
	if start := i - momentLead; start > 0 {
		msgs = msgs[start:]
	}
	return msgs
}

// draftShowSessionHTML is draft d's "Show session" on owner's officer's
// page: where it was filed from (d.Origin), else — a draft filed before 0185
// — the instance that filed it (its agent_name), else the officer's first.
func (s *Server) draftShowSessionHTML(ctx context.Context, sc officerScope, officer string, d plugins.Draft, first string) string {
	fallback := first
	if d.AgentName != "" {
		fallback = d.AgentName
	}
	href := s.officerOriginHref(ctx, sc, officer, d.Origin, fallback)
	return showSessionHTML(href, momentHref(href))
}

// refusalShowSessionHTML is refusal rf's "Show session": the session that
// filed the draft it refused (rf.Draft), read as the viewer — "" when that
// draft cannot be read.
func (s *Server) refusalShowSessionHTML(ctx context.Context, sc officerScope, officer string, rf builtin.Refusal, first string) string {
	if s.ext.Plugins == nil || rf.Draft <= 0 {
		return ""
	}
	d, err := s.ext.Plugins.Draft(ctx, rf.Draft)
	if err != nil || d.OwnerID != sc.Owner {
		return ""
	}
	return s.draftShowSessionHTML(ctx, sc, officer, d, first)
}

// momentHref reports whether href opens a session AT a moment (ui.
// AgentMomentParam): the button says which it does.
func momentHref(href string) bool {
	return strings.Contains(href, "&"+ui.AgentMomentParam+"=")
}

// askOrigins is the origin of each of owner's officer asks (builtin.Store.
// AskOrigins), none when it cannot be read (logged: the buttons then open
// each instance's session as a whole).
func (s *Server) askOrigins(ctx context.Context, owner int64, asks []attention.Ask) map[int64]itemorigin.Origin {
	if s.officerStore == nil || len(asks) == 0 {
		return map[int64]itemorigin.Origin{}
	}
	ids := make([]int64, 0, len(asks))
	for _, a := range asks {
		ids = append(ids, a.ID)
	}
	m, err := s.officerStore.AskOrigins(ctx, owner, ids)
	if err != nil {
		s.logger.Printf("officers: read where %d asks were made: %v", len(ids), err)
		return map[int64]itemorigin.Origin{}
	}
	return m
}

// withDraftShowSessions is list (draftListHTMLIn's cards for cards) with
// each card's "Show session" put at the end of its <li>: a draft's card its
// own (draftShowSessionHTML), an outreach batch's card its first entry's.
//
// ⚠ AFTER THE CARDS ARE DRAWN, NOT INSIDE THEM: the draft and batch cards are
// the Asks page's own (plugin_drafts.go, outreach_batch.go), drawn the same
// on every face; only this page adds the button. A card is found by its
// opening <li>'s own attribute and ended at ITS </li> (nested lists counted),
// and a card that is not found gets no button rather than a misplaced one.
func (s *Server) withDraftShowSessions(ctx context.Context, sc officerScope, officer, list string, cards []plugins.DraftCard, first string) string {
	_, batches, _ := groupBatches(cards)
	add := map[string]string{} // the card's opening attribute -> its button
	for _, c := range cards {
		if btn := s.draftShowSessionHTML(ctx, sc, officer, c.Draft, first); btn != "" {
			add[`data-draft-id="`+strconv.FormatInt(c.Draft.ID, 10)+`"`] = btn
		}
	}
	for _, g := range batches {
		all := append(append([]plugins.DraftCard{}, g.entries...), g.waiting...)
		if len(all) == 0 {
			continue
		}
		if btn := s.draftShowSessionHTML(ctx, sc, officer, all[0].Draft, first); btn != "" {
			add[`data-outreach-batch="`+html.EscapeString(g.id)+`"`] = btn
		}
	}
	for attr, btn := range add {
		list = insertAtCardEnd(list, attr, btn)
	}
	return list
}

// insertAtCardEnd is page with ins put just before the </li> closing the
// <li> whose opening tag carries attr; page unchanged when there is none.
func insertAtCardEnd(page, attr, ins string) string {
	at := strings.Index(page, attr)
	if at < 0 {
		return page
	}
	open := strings.LastIndex(page[:at], "<li")
	if open < 0 || strings.Contains(page[open:at], ">") {
		return page // attr is not on an <li>'s own opening tag
	}
	depth := 0
	for i := open; i < len(page); {
		switch {
		case strings.HasPrefix(page[i:], "<li") && (i+3 < len(page) && (page[i+3] == ' ' || page[i+3] == '>')):
			depth++
			i += 3
		case strings.HasPrefix(page[i:], "</li>"):
			depth--
			if depth == 0 {
				return page[:i] + ins + page[i:]
			}
			i += 5
		default:
			i++
		}
	}
	return page
}

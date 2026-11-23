package api

import (
	"context"
	"errors"
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/itemorigin"
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
// in that session (migration 0184's origin_* columns), and the officer page
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
// (officer_origin_is_owners, 0184), an escalation records its origin on the
// owner's own notice only (builtin.noticesFor), and the agent page answers
// another owner's agent as a 404 (row-level security).
//
// WHAT "THE MOMENT" IS. An officer's tool calls are not chat_messages rows:
// a turn's prompt and reply are written at its boundaries. So the moment is
// the newest message of the session when the call was made — the boundary
// just before the turn that made the item — and origin_at says when.
// ---------------------------------------------------------------------------

// sessionByKeyer is the agents store's session-by-key read
// (agents.PGStore.SessionByKey), asserted rather than added to agents.Store.
type sessionByKeyer interface {
	SessionByKey(ctx context.Context, agentID int64, sessionKey string) (agents.ChatSession, error)
}

// withOfficerOrigin is ctx carrying where officer instance a's tool call is
// made from: a, the chat session its turn is in — a chat lane's own session
// (its chat's key), else the session its primary turns are recorded in (the
// one LatestOrCreateSession answers) — and that session's newest message,
// resolved only when a row is made (itemorigin.Resolve).
func (s *Server) withOfficerOrigin(ctx context.Context, a agents.Agent) context.Context {
	if a.ID <= 0 || s.ext.Agents == nil {
		return ctx
	}
	at := time.Now()
	chat, lane := chatLaneCall(ctx)
	agent := a.ID
	return itemorigin.With(ctx, func(rctx context.Context) (itemorigin.Origin, bool) {
		o := itemorigin.Origin{AgentID: agent, At: at}
		var sid int64
		if lane {
			if k, ok := s.ext.Agents.(sessionByKeyer); ok {
				if cs, err := k.SessionByKey(rctx, agent, officers.ChatSessionKey(chat)); err == nil {
					sid = cs.ID
				} else if !errors.Is(err, pgx.ErrNoRows) {
					s.logger.Printf("officers: origin of agent %d's call: its chat session: %v", agent, err)
				}
			}
		} else {
			sid = s.latestSessionIDs(rctx, []int64{agent})[agent]
		}
		if sid > 0 {
			o.SessionID = sid
			if msgs, err := s.ext.Agents.ListRecentChatMessages(rctx, sid, 1); err == nil && len(msgs) > 0 {
				o.MessageID = msgs[len(msgs)-1].ID
			}
		}
		return o, true
	})
}

// officerOriginHref is the "Show session" link of an item owner's officer
// made with origin o, or "" for none: the session scrolled to the moment
// (ui.AgentMomentHref) when both are recorded, the session as a whole when
// only it is, and — an item made before 0184, or with no session — the page
// of the instance named by fallback (the draft's own agent, or the officer's
// first instance), opening its latest session.
//
// 🔴 The agent named is checked to be ONE OF THIS OFFICER'S INSTANCES of this
// owner (agent owner and name): anything else draws no link.
func (s *Server) officerOriginHref(ctx context.Context, owner int64, officer string, o itemorigin.Origin, fallback string) string {
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
	if err != nil || a.OwnerID != owner || !s.isOfficerInstance(owner, officer, a.Name) {
		return ""
	}
	if o.AgentID > 0 && o.SessionID > 0 {
		return ui.AgentMomentHref(a.Name, o.SessionID, o.MessageID)
	}
	return ui.AgentSessionHref(a.Name, s.latestSessionIDs(ctx, []int64{a.ID})[a.ID])
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
// does not run): an item made before 0184 has no origin, and opens it.
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
// page: where it was filed from (d.Origin), else — a draft filed before 0184
// — the instance that filed it (its agent_name), else the officer's first.
func (s *Server) draftShowSessionHTML(ctx context.Context, owner int64, officer string, d plugins.Draft, first string) string {
	fallback := first
	if d.AgentName != "" {
		fallback = d.AgentName
	}
	return showSessionHTML(s.officerOriginHref(ctx, owner, officer, d.Origin, fallback), d.Origin.SessionID > 0 && d.Origin.MessageID > 0)
}

// refusalShowSessionHTML is refusal rf's "Show session": the session that
// filed the draft it refused (rf.Draft), read as the viewer — "" when that
// draft cannot be read.
func (s *Server) refusalShowSessionHTML(ctx context.Context, owner int64, officer string, rf builtin.Refusal, first string) string {
	if s.ext.Plugins == nil || rf.Draft <= 0 {
		return ""
	}
	d, err := s.ext.Plugins.Draft(ctx, rf.Draft)
	if err != nil || d.OwnerID != owner {
		return ""
	}
	return s.draftShowSessionHTML(ctx, owner, officer, d, first)
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
func (s *Server) withDraftShowSessions(ctx context.Context, owner int64, officer, list string, cards []plugins.DraftCard, first string) string {
	_, batches, _ := groupBatches(cards)
	add := map[string]string{} // the card's opening attribute -> its button
	for _, c := range cards {
		if btn := s.draftShowSessionHTML(ctx, owner, officer, c.Draft, first); btn != "" {
			add[`data-draft-id="`+strconv.FormatInt(c.Draft.ID, 10)+`"`] = btn
		}
	}
	for _, g := range batches {
		all := append(append([]plugins.DraftCard{}, g.entries...), g.waiting...)
		if len(all) == 0 {
			continue
		}
		if btn := s.draftShowSessionHTML(ctx, owner, officer, all[0].Draft, first); btn != "" {
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

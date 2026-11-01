package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// WHERE TAPPING AN ATTENTION NOTIFICATION LANDS (operator ask: "When tapping on
// a notification, go straight to the session instead of to the attention queue
// item").
//
// 🔴 RESOLVED WHEN TAPPED, NOT WHEN SENT. Every `attention` push carries ONE
// url, attentionOpenURL(key) — GET /attention/open/<key> — and that route
// decides the target at the moment of the tap (pushTargetFor) and redirects.
// A target fixed at send time goes stale: the window closes, the host drops,
// the agent is destroyed, and the tap lands on a 404 where the queue item
// would still have answered. The url is the tap target on BOTH devices — the
// service worker opens it (web/static/sw.js focusOrOpen) and a paired phone's
// payload v2 carries it as its `url` (internal/push PayloadV2) — so neither
// app decides anything. And the send path reads nothing: attentionNotify
// builds the url from the item's key alone, with no database or hub read
// inline in attention_ask or a task ask.
//
// 🔴 ONE RULE, ONE PLACE: pushTargetFor.
//   - AN OPEN ASK OPENS ITS OWN PAGE, /attention/asks/<id> (operator decision:
//     the landing is a phone-first ask page — attention_ask_page.go): the
//     question, its options and suggested replies, the answer box, and a link
//     to the asking session or agent for context (attentionSessionPath — what
//     the tap opened before the ask page existed).
//   - Something else that HAS a session opens that session's page: a waiting
//     mirrored window and a held question open the window's rendered view
//     (it.card.ref, the address the queue's own "Open the session" link uses).
//
// EVERYTHING ELSE OPENS ITS ITEM ON THE QUEUE PAGE (attentionItemPath) — an
// explicit fallback, never an empty link and never a 404: an ask that is no
// longer open (answered, dismissed, expired), another owner's or a missing
// one (the same answer, so nothing tells them apart), a Hermes window that
// names no conversation, a window that stopped waiting, and any kind this
// rule does not name.
// ---------------------------------------------------------------------------

// attentionOpenPath is the tap route's prefix.
const attentionOpenPath = attentionPath + "/open/"

// attentionOpenURL is what an item's notification opens.
func attentionOpenURL(key string) string { return attentionOpenPath + key }

// attentionOpenKey is a key the tap route takes: an ask's, or a waiting
// window's (sessionKey). Anything else is a 404 — it never reaches a redirect.
var attentionOpenKey = regexp.MustCompile(`^(ask-[1-9][0-9]{0,18}|session-[0-9a-f]{16})$`)

// pushTargetLookupTimeout bounds the reads a tap's resolution makes (the ask,
// the asking agent's row), so a slow database delays the tap only briefly
// before it falls back to the queue.
const pushTargetLookupTimeout = 3 * time.Second

// attentionItemPath is an item's place on the queue page: the fallback target.
func attentionItemPath(key string) string { return attentionPath + "#" + key }

// handleAttentionOpen is GET /attention/open/{key}: the item's target now.
func (s *Server) handleAttentionOpen(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if s.ext.Attention == nil || !attentionOpenKey.MatchString(key) {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), pushTargetLookupTimeout)
	defer cancel()
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, s.openTarget(ctx, key), http.StatusSeeOther)
}

// openTarget is where key's notification opens for ctx's principal now.
func (s *Server) openTarget(ctx context.Context, key string) string {
	owner := requestOwner(ctx)
	if owner <= 0 {
		return attentionItemPath(key)
	}
	var it attentionItem
	found := false
	if rest, ok := strings.CutPrefix(key, "ask-"); ok {
		id, _ := strconv.ParseInt(rest, 10, 64)
		a, err := s.ext.Attention.Get(ctx, id)
		switch {
		case err == nil && a.Owner == owner && a.Status == attention.Open:
			it, found = askItem(a), true
		case err != nil && !errors.Is(err, attention.ErrNotFound):
			s.logger.Printf("attention: the tap target of %s: %v (opening the queue instead)", key, err)
		}
	} else {
		for _, x := range s.waitingSessions(ctx) {
			if x.Key == key {
				it, found = x, true
				break
			}
		}
	}
	if !found {
		return attentionItemPath(key)
	}
	return s.pushTargetFor(ctx, owner, it)
}

// pushTargetFor is where owner's item it opens: an open ask, its own page
// (askPagePath); anything else, its session's page when it has one
// (attentionSessionPath), else its item on the queue page. ctx is owner's
// principal (the tap's request).
func (s *Server) pushTargetFor(ctx context.Context, owner int64, it attentionItem) string {
	if it.Kind == attentionKindAsk && it.AskID > 0 {
		return askPagePath(it.AskID)
	}
	if p := s.attentionSessionPath(ctx, owner, it); p != "" {
		return p
	}
	return attentionItemPath(it.Key)
}

// attentionSessionPath is the page of the session it is about, "" when it has
// none muster can name now.
func (s *Server) attentionSessionPath(ctx context.Context, owner int64, it attentionItem) string {
	c := it.card
	switch it.Kind {
	case attentionKindSession, attentionKindQuestion:
		if it.hermes != nil && it.claudeSession == "" {
			return "" // a Hermes window that names no conversation
		}
		if c.ref.Host == "" || c.ref.Session == "" {
			return ""
		}
		return c.ref.ViewPath()
	case attentionKindAsk:
		switch {
		case c.agentID > 0:
			return s.askingAgentHref(ctx, owner, c.agentID, 0)
		case c.claudeSession != "" && c.ref.Host != "":
			if s.ext.Mirror == nil {
				return ""
			}
			ref, ok := claudeSessionWindow(s.ext.Mirror.Hosts(ctx), c.ref.Host, c.claudeSession)
			if !ok {
				return ""
			}
			return ref.ViewPath()
		case it.systemAsk && it.AskID > 0:
			return s.nudgeSessionPath(ctx, owner, it.AskID)
		}
	}
	return ""
}

// nudgeSessionPath is the page of the session a chief nudge's ask (askID) is
// about, "" when askID is no nudge's ask of owner's or the session cannot be
// named now.
//
// A nudge's ask is raised as muster's OWN (attention.Asker{System: true},
// chief_nudge.go actAsk), so the ask names no session; the session is on the
// decision that raised it (chief_nudges.ask_id, ByAsk). 🔴 JOINED WHEN DRAWN,
// NOT COPIED ONTO THE ASK. The decision row is the one record of which session
// the nudge is about, and resolving it now is the rule every other ask follows
// here: a mirrored session by its Claude session id (claudeSessionWindow, so a
// window renumbered since still resolves and a closed one links nothing), an
// agent through askingAgentHref's owner check. A copy on the ask would need a
// column, and a second asker kind beside System, for a fact that already has
// a home.
//
// 🔴 OWNER-SCOPED TWICE, as askingAgentHref is: ByAsk reads under row-level
// security, and the explicit check holds with it rolled back — another
// owner's decision that names this ask id never becomes a link.
func (s *Server) nudgeSessionPath(ctx context.Context, owner, askID int64) string {
	if s.ext.ChiefNudges == nil {
		return ""
	}
	d, err := s.ext.ChiefNudges.ByAsk(ctx, askID)
	if err != nil {
		if !errors.Is(err, chiefnudge.ErrNotFound) {
			s.logger.Printf("attention: the session of ask %d's chief nudge: %v", askID, err)
		}
		return ""
	}
	if d.Owner != owner || d.AskID != askID {
		return ""
	}
	switch d.Kind {
	case chiefnudge.KindAgent:
		if d.AgentID > 0 {
			// The conversation the nudge is about, not the agent's newest: its
			// answer is relayed into that one (relayNudgeAnswer).
			return s.askingAgentHref(ctx, owner, d.AgentID, d.ChatSessionID)
		}
	case chiefnudge.KindMirror:
		if s.ext.Mirror == nil || d.Host == "" || d.Key == "" {
			return ""
		}
		if ref, ok := claudeSessionWindow(s.ext.Mirror.Hosts(ctx), d.Host, d.Key); ok {
			return ref.ViewPath()
		}
	}
	return ""
}

// askingAgentHref is agent id's page when it is owner's agent, else "": its
// chat session session's (ui.AgentSessionHref) when session > 0, the bare
// page otherwise. The explicit owner check is not only row-level security's:
// with RLS rolled back the read sees every owner's agents, and a tap must
// never open another owner's agent.
func (s *Server) askingAgentHref(ctx context.Context, owner, id, session int64) string {
	if s.ext.Agents == nil {
		return ""
	}
	a, err := s.ext.Agents.Get(ctx, id)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			s.logger.Printf("attention: the tap target of agent %d's ask: %v (opening the queue instead)", id, err)
		}
		return ""
	}
	if a.OwnerID != owner {
		return ""
	}
	// The BARE page for an agent's own ask, on purpose (#237): the tap is
	// resolved when tapped, and the bare URL resolves the agent's latest session
	// at that moment too. A chief nudge's ask names the conversation it is about
	// (nudgeSessionPath), which need not be the latest.
	return ui.AgentSessionHref(a.Name, session)
}

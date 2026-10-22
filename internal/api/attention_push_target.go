package api

import (
	"context"
	"errors"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// WHERE TAPPING AN ATTENTION NOTIFICATION LANDS (operator ask: "When tapping on
// a notification, go straight to the session instead of to the attention queue
// item").
//
// 🔴 ONE RULE, ONE PLACE: pushTargetFor. attentionNotify is the only sender of
// an `attention` push and reads its Data["url"] from here alone, and that one
// field is the tap target on BOTH devices — the service worker opens it
// (web/static/sw.js focusOrOpen) and a paired phone's payload v2 carries it as
// its `url` (internal/push PayloadV2), so neither app decides anything.
//
// A push about something that HAS a session opens that session's page:
//   - a waiting mirrored window and a held question: the window's rendered
//     view (it.card.ref, the address the queue's own "Open the session" link
//     uses);
//   - a dispatched agent's attention_ask: the agent's page, /agents/<name>
//     (resolved from the asker's id now; the owner's agent only);
//   - a mirrored Claude session's attention_ask: the rendered view of the
//     window running that Claude session on its host, resolved now by the rule
//     the Agents-list badges use (claudeSessionWindow).
//
// EVERYTHING ELSE KEEPS ITS ITEM ON THE QUEUE PAGE (attentionItemPath) — an
// explicit fallback, never an empty link: an ask muster raised itself
// (Asker.System: a task mention with no chief), a Hermes window that names no
// conversation (no session key), an asking agent that cannot be read or is not
// the owner's, an asking mirrored session no listed window runs at send time,
// and any kind this rule does not name. (Reply drafts and routed tasks have
// their own pushes and do not come here.)
// ---------------------------------------------------------------------------

// pushTargetLookupTimeout bounds the one read an ask's target may make (the
// asking agent's row), so a slow database delays a notification only briefly.
const pushTargetLookupTimeout = 3 * time.Second

// attentionItemPath is an item's place on the queue page: the fallback target.
func attentionItemPath(key string) string { return attentionPath + "#" + key }

// pushTargetFor is where tapping owner's notification for it opens: its
// session's page when it has one (attentionSessionPath), else its item on the
// queue page.
func (s *Server) pushTargetFor(owner int64, it attentionItem) string {
	if p := s.attentionSessionPath(owner, it); p != "" {
		return p
	}
	return attentionItemPath(it.Key)
}

// attentionSessionPath is the page of the session it is about, "" when it has
// none muster can name now.
func (s *Server) attentionSessionPath(owner int64, it attentionItem) string {
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
			return s.askingAgentPath(owner, c.agentID)
		case c.claudeSession != "" && c.ref.Host != "":
			if s.ext.Mirror == nil {
				return ""
			}
			ref, ok := claudeSessionWindow(s.ext.Mirror.Hosts(s.attentionPrincipal(context.Background(), owner)), c.ref.Host, c.claudeSession)
			if !ok {
				return ""
			}
			return ref.ViewPath()
		}
	}
	return ""
}

// askingAgentPath is agent id's page when it is owner's agent, else "". The
// explicit owner check is not only row-level security's: with RLS rolled back
// the read sees every owner's agents, and a push must never name another
// owner's agent.
func (s *Server) askingAgentPath(owner, id int64) string {
	if s.ext.Agents == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(s.attentionPrincipal(context.Background(), owner), pushTargetLookupTimeout)
	defer cancel()
	a, err := s.ext.Agents.Get(ctx, id)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			s.logger.Printf("attention: the push target of agent %d's ask: %v (linking the queue instead)", id, err)
		}
		return ""
	}
	if a.OwnerID != owner {
		return ""
	}
	return "/agents/" + url.PathEscape(a.Name)
}

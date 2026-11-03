package api

import (
	"context"
	"fmt"
	"strconv"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
)

// ---------------------------------------------------------------------------
// NOTICES ON THE ATTENTION QUEUE (migration 0071; docs/attention.md#notices).
//
// A notice is an attention_asks row of kind 'notice' (attention.KindNotice): a
// POINTER at a session, raised by muster itself — today only by a live chief
// nudge (chief_nudge.go actAsk) — with no options and no answer. The owner
// looks at the session, or dismisses the notice; muster withdraws it when the
// session moves on (reconcileNudge, the same rule a nudge ask had).
//
// 🔴 WHERE IT POINTS IS JOINED WHEN DRAWN, NOT COPIED (the rule
// nudgeSessionPath states): the decision that raised the notice
// (chief_nudges.ask_id) names the session, and a mirrored session's window is
// resolved NOW over the owner's windows by its session key — so a window
// renumbered since still resolves to its new index, and one that no longer
// holds the conversation names none. The notice's asker label is the place as
// the sweep saw it, a fallback for display only (an agent's name; a window
// nobody lists now).
//
// The item a notice becomes (noticeItem) says exactly where: its Source is
// "<host> · <tmux session>:<window index>" (noticeWhere) and its Target the
// "<tmux session>:<window index>" a host's `tmux switch-client -t` takes —
// what `muster nudge next` prints (cmd/muster/nudge.go) from the summary.
// ---------------------------------------------------------------------------

// attentionKindNotice is a notice's item kind.
const attentionKindNotice = "notice"

// noticeWhere is a mirrored window's place as a notice names it: the host,
// then its exact tmux target ("ws · main:1").
func noticeWhere(host, session string, index int) string {
	return host + mirror.QualifierSep + noticeTarget(session, index)
}

// noticeTarget is a tmux session and window index as tmux takes them.
func noticeTarget(session string, index int) string {
	return session + ":" + strconv.Itoa(index)
}

// noticeLabel is the place a notice's asker label keeps: a mirrored window's
// noticeWhere as the sweep saw it, or "agent <name>".
func noticeLabel(c chiefnudge.Session) string {
	if c.Kind == chiefnudge.KindMirror {
		return noticeWhere(c.Host, c.TmuxSession, c.Index)
	}
	return "agent " + c.Label
}

// sessionKeyEntry is the window on host running the conversation whose session
// key is key: a Claude session (claudeSessionEntry's rule — the active pane or
// another pane of the window), or another runtime's runtime-qualified key
// (mirror.Window.SessionKey, "opencode:<id>").
func sessionKeyEntry(hosts []mirror.HostView, host, key string) (mirror.SessionView, bool) {
	if e, ok := claudeSessionEntry(hosts, host, key); ok || key == "" {
		return e, ok
	}
	for _, h := range hosts {
		if h.Host != host {
			continue
		}
		for _, w := range h.Windows {
			if !w.Claude && w.SessionKey() == key {
				return mirror.SessionView{Host: h.Host, Session: w.Session, Window: w.ID, Index: w.Index, Label: w.Label, Online: h.Online}, true
			}
		}
	}
	return mirror.SessionView{}, false
}

// noticeDecisions is the decisions behind the notices among asks, by ask id, in
// ONE read (chiefnudge.Store.ByAsks); empty when there is none. A failed read
// is returned (and the map is empty: each notice then shows its label and NO
// tmux target), so a caller that builds the queue reports itself partial —
// `muster nudge next` must not read "no target" as "none waiting".
func (s *Server) noticeDecisions(ctx context.Context, asks []attention.Ask) (map[int64]chiefnudge.Decision, error) {
	var ids []int64
	for _, a := range asks {
		if a.Kind == attention.KindNotice {
			ids = append(ids, a.ID)
		}
	}
	if len(ids) == 0 || s.ext.ChiefNudges == nil {
		return nil, nil
	}
	out, err := s.ext.ChiefNudges.ByAsks(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("the sessions of %d notice(s): %w", len(ids), err)
	}
	return out, nil
}

// noticeItemNow is notice a as an item, its decision and ctx's principal's
// windows read now (askItemNow's notice half).
func (s *Server) noticeItemNow(ctx context.Context, a attention.Ask) attentionItem {
	ds, err := s.noticeDecisions(ctx, []attention.Ask{a})
	if err != nil {
		s.logger.Printf("attention: %v", err)
	}
	var hosts []mirror.HostView
	if d, ok := ds[a.ID]; ok && d.Kind == chiefnudge.KindMirror && s.ext.Mirror != nil {
		hosts = s.ext.Mirror.Hosts(ctx)
	}
	return noticeItem(a, ds, hosts)
}

// noticeItem is notice a as an attention item: ds is the decisions behind the
// owner's notices (noticeDecisions), hosts the owner's windows.
//
// 🔴 OWNER-CHECKED, as nudgeSessionPath is: a decision of another owner's that
// names this ask id is ignored (row-level security reads none; the check holds
// with it rolled back).
func noticeItem(a attention.Ask, ds map[int64]chiefnudge.Decision, hosts []mirror.HostView) attentionItem {
	key := attentionAskKey(a.ID)
	it := attentionItem{Key: key, Kind: attentionKindNotice, Source: a.Asker.Label, Text: a.Question, Since: a.CreatedAt,
		Link: attentionPath + "#" + key, AskID: a.ID, systemAsk: true}
	if it.Source == "" {
		it.Source = chiefNudgeAskLabel
	}
	d, ok := ds[a.ID]
	if !ok || d.Owner != a.Owner || d.AskID != a.ID {
		return it
	}
	it.Focus = d.Title
	switch d.Kind {
	case chiefnudge.KindAgent:
		it.subject = focus.AgentSubject(d.AgentID)
	case chiefnudge.KindMirror:
		it.subject = focus.MirrorSubject(d.Key)
		it.host = d.Host
		if e, found := sessionKeyEntry(hosts, d.Host, d.Key); found {
			it.Source = noticeWhere(e.Host, e.Session, e.Index)
			it.target = it.Source
			it.Target = noticeTarget(e.Session, e.Index)
			if !e.Online {
				it.Source += whereHostOffline
				it.Offline = true
			}
			return it
		}
		// No window holds the conversation now: the place as the sweep saw it
		// (its label), and no target to jump to.
		it.Source += askNotShown
	}
	return it
}

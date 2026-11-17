package api

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/ZacxDev/muster/internal/officers/builtin"
	"github.com/ZacxDev/muster/internal/sessionstate"
)

// ---------------------------------------------------------------------------
// THE SESSION MINDER (officers O-D; the wizard's "Session minder" template,
// officer_templates.go; docs/officers-builtin.md "Templates").
//
// An officer whose type grants builtin.CapSessionMinder, switched on, MINDS
// its owner's stopped sessions in place of the chief's stall nudges:
//
//   - 🔴 THE CHIEF'S NUDGES SKIP THAT OWNER, in code: sweepOwnerNudges returns
//     before any candidate is read or any chief call made, and
//     episodeChiefJudges answers false so a stop's push never waits on a
//     decision that will not come (it pushes as with no chief). One predicate
//     for both, sessionMinderOf. Pausing or deleting the officer gives the
//     owner their nudges back on the next sweep.
//   - each stop the chief WOULD have judged — stalledSessions, the nudges' own
//     candidate rule: idle past ChiefNudges.Idle, no shell, no turn error, no
//     open ask, with an open episode — becomes ONE item in the officer's queue
//     (minderItemKind, subject "episode:<first episode of the stop>"), fed by
//     the session-state pass. EnqueueOnce keeps it to one item per stop for
//     good: an item already done is not queued again while the stop stays
//     open, nor when the stop resumes after a host drop (episodeRoot).
//
// 🔴 IT NEEDS SESSION STATE (MUSTER_SESSION_STATE=1): episodes are what it is
// fed. With session state off there is nothing to feed it, so it minds
// nothing and suppresses nothing — the chief keeps nudging. A read that fails
// minds nothing either (never a silence). The item carries where the session
// is and its state, NO session text; the officer has no tool that types into
// a session, so it minds by asking its owner (officer_ask_person) or leaving
// the stop alone. The episode's own push is untouched by any of this.
//
// Persons only, as the nudges are: a team's state pass runs no chief and
// feeds no minder.
// ---------------------------------------------------------------------------

// minderItemKind is the kind of a session minder's item: one stopped session.
const minderItemKind = "session.stalled"

// sessionMinderOf is owner's active session minder's officer id
// (builtin.Store.SessionMinder), "" when owner has none — or when session
// state is off (nothing would feed it), there is no officer store, or the read
// fails: a minder that cannot be confirmed never takes the chief's nudges away.
func (s *Server) sessionMinderOf(octx context.Context, owner int64) string {
	if !s.sessionStateOn() || s.officerStore == nil {
		return ""
	}
	officer, ok, err := s.officerStore.SessionMinder(octx, owner)
	if err != nil {
		s.logger.Printf("session minder: owner %d: %v", owner, err)
		return ""
	}
	if !ok {
		return ""
	}
	return officer
}

// feedSessionMinder queues, for owner's active session minder, one item per
// stop among facts (the state pass's one read of the stall facts) that the
// chief's nudges would have judged (stalledSessions). Nothing when owner has
// no minder.
func (s *Server) feedSessionMinder(octx context.Context, owner int64, facts []nudgeCandidate) {
	minder := s.sessionMinderOf(octx, owner)
	if minder == "" {
		return
	}
	now := s.now()
	for _, c := range s.stalledSessions(octx, facts) {
		n, err := minderItem(minder, c, now)
		if err != nil {
			s.logger.Printf("session minder: owner %d: episode %d: %v", owner, c.episodeID, err)
			continue
		}
		if _, _, err := s.officerStore.EnqueueOnce(octx, owner, n, now); err != nil {
			s.logger.Printf("session minder: owner %d: queue episode %d for %s: %v", owner, c.episodeID, minder, err)
		}
	}
}

// minderPayload is a session minder's item payload: where the stopped session
// is and what state it is in — never anything the session wrote.
type minderPayload struct {
	EpisodeID   int64         `json:"episode_id"`
	State       string        `json:"state"`
	Asked       bool          `json:"asked"`
	Classifier  string        `json:"classifier,omitempty"`
	IdleSeconds int64         `json:"idle_seconds"`
	IdleSince   time.Time     `json:"idle_since"`
	Session     minderSession `json:"session"`
}

// minderSession names the session the way muster's own surfaces do.
type minderSession struct {
	Kind          string `json:"kind"`
	Name          string `json:"name,omitempty"`
	Label         string `json:"label,omitempty"`
	Runtime       string `json:"runtime,omitempty"`
	Host          string `json:"host,omitempty"`
	TmuxSession   string `json:"tmux_session,omitempty"`
	WindowIndex   int    `json:"window_index,omitempty"`
	AgentID       int64  `json:"agent_id,omitempty"`
	ChatSessionID int64  `json:"chat_session_id,omitempty"`
	Dispatched    bool   `json:"dispatched,omitempty"`
}

// minderItem is candidate c (stalledSessions, with its open episode) as
// minder's item: subject "episode:<first episode of its stop>", high priority
// when the stop needs the owner.
func minderItem(minder string, c nudgeCandidate, now time.Time) (builtin.NewItem, error) {
	ep := c.episode
	p := minderPayload{EpisodeID: ep.ID, State: ep.State, Asked: ep.Asked, Classifier: ep.Jev.Label,
		IdleSeconds: int64(c.IdleFor / time.Second), IdleSince: c.IdleSince.UTC(),
		Session: minderSession{Kind: c.Kind, Name: c.Name, Label: c.Label, Runtime: c.Runtime, Host: c.Host,
			TmuxSession: c.TmuxSession, WindowIndex: c.Index, AgentID: c.AgentID, ChatSessionID: c.ChatSessionID, Dispatched: c.Dispatched}}
	raw, err := json.Marshal(p)
	if err != nil {
		return builtin.NewItem{}, err
	}
	prio := "normal"
	if ep.State == sessionstate.StateNeedsYou {
		prio = "high"
	}
	return builtin.NewItem{Officer: minder, Kind: minderItemKind, SubjectKey: "episode:" + strconv.FormatInt(episodeRoot(ep), 10),
		Payload: raw, Tier: "judgement", Priority: prio}, nil
}

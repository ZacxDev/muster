package api

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
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
// 🔴 IT MUST BE ABLE TO RUN: an enabled minder the roster load resolved to
// nowhere — or to a host whose mirror is not connected now — (minderRuns)
// suppresses nothing and is fed nothing.
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
// state is off (nothing would feed it), there is no officer store, the read
// fails, or the minder CANNOT RUN (minderRuns): a minder that cannot be
// confirmed to work never takes the chief's nudges away.
func (s *Server) sessionMinderOf(octx context.Context, owner int64) string {
	if !s.sessionStateOn() || s.officerStore == nil {
		return ""
	}
	officer, ok, err := s.officerStore.SessionMinder(octx, owner)
	if err != nil {
		s.logger.Printf("session minder: owner %d: %v", owner, err)
		return ""
	}
	if !ok || !s.minderRuns(octx, owner, officer) {
		return ""
	}
	return officer
}

// minderRuns reports whether owner's officer is in the LIVE roster — what the
// roster load resolved to somewhere it runs (resolveDeclared → resolveRuntime,
// the one runtime rule; the officers page's "running") — and, when that is a
// host, whether the host is CONNECTED now. 🔴 An officer that is switched on
// but resolved to nowhere (no pods, no qualifying host, a refused
// declaration) would mind nothing, so it must not silence the chief either:
// the nudges stay on, as with session state off. No officer runtime: false.
//
// 🔴 A HOST OFFICER'S HOST MUST BE CONNECTED. The runtime rule places a
// "runtime": "host" officer on its named host whenever this muster can run
// host officers at all — it never asks whether that host is connected — and
// an auto officer stays placed on a host that dropped (autoHostFor's grace,
// or for good when nowhere else qualifies). Either is in the roster while its
// host's mirror is gone, and nothing is minding. So a minder on a host counts
// only while the hub sees that host online (officerHostView, the host fleet's
// own check before it starts an instance there): offline, the chief's nudges
// come back on the next sweep. A minder in pods is unchanged.
func (s *Server) minderRuns(octx context.Context, owner int64, officer string) bool {
	if s.officerRT == nil || s.officerRT.reg == nil {
		return false
	}
	o, ok := s.officerRT.reg.ForOwner(owner, officer)
	if !ok {
		return false
	}
	if o.OnHost() {
		_, online := s.officerHostView(s.officerPrincipal(octx, owner), o.Host)
		return online
	}
	return true
}

// feedSessionMinder queues, for owner's active session minder, one item per
// stop among facts (the state pass's one read of the stall facts) that the
// chief's nudges would have judged (stalledSessions). Nothing when owner has
// no minder.
func (s *Server) feedSessionMinder(octx context.Context, owner int64, facts []nudgeCandidate) {
	if s.stopJudgeOf(octx, owner) != judgeMinder {
		return
	}
	minder := s.sessionMinderOf(octx, owner)
	if minder == "" {
		return
	}
	now := s.now()
	for _, c := range s.stalledSessions(octx, facts) {
		// 🔴 ONE JUDGE PER STOP: a stop the chief already judged (while this
		// minder was not running: its host offline, paused) is not fed to it
		// as well — the chief's own record (nudgeEpisodeJudged), the rule its
		// sweep skips a judged stop by. A read that fails feeds nothing.
		if c.episodeID != 0 && s.nudgeEpisodeJudged(octx, c.episodeID) {
			continue
		}
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
	return builtin.NewItem{Officer: minder, Kind: minderItemKind, SubjectKey: minderSubject(ep),
		Payload: raw, Tier: "judgement", Priority: prio}, nil
}

// minderSubject is the subject of a minder's item for the stop ep is in:
// "episode:<first episode of the stop>" (episodeRoot).
func minderSubject(ep sessionstate.Episode) string {
	return "episode:" + strconv.FormatInt(episodeRoot(ep), 10)
}

// minderHandoverSummary is the summary of a minder's item the chief took over.
const minderHandoverSummary = "the chief judged this stop: the session minder was not running when it was due"

// chiefTakesStop is THE HANDOVER of one stop from a session minder to the
// chief, asked by the chief's sweep before it judges candidate c of owner's:
// whether the chief may judge it. 🔴 ONE JUDGE PER STOP. A minder that stops
// running (its host dropped, it was paused) gives its owner's nudges back
// (minderRuns), but stops already fed to it are still in its queue:
//
//   - an item the minder has ACTED ON — claimed under a live lease, waiting
//     on its owner, done or escalated — keeps the stop the minder's: the
//     chief does not judge it (false);
//   - an item still OPEN (or claimed under a lease that has lapsed: an
//     instance that died holding it) is closed here, no_action, BEFORE the
//     chief judges — so the minder, back, never acts on a stop the chief
//     took. ResolveItem refuses an item a live lease holds, so a claim that
//     lands between the read and the close keeps the stop the minder's.
//
// A read or close that fails leaves the stop alone this pass (false): never
// a second judge. No candidate episode (session state off) or no officer
// store: nothing was ever fed, true.
func (s *Server) chiefTakesStop(octx context.Context, owner int64, c nudgeCandidate) bool {
	if c.episodeID == 0 || s.officerStore == nil {
		return true
	}
	items, err := s.officerStore.SubjectItems(octx, owner, minderItemKind, minderSubject(c.episode))
	if err != nil {
		s.logger.Printf("session minder: owner %d: episode %d: read its items: %v; the chief leaves the stop this pass", owner, c.episodeID, err)
		return false
	}
	now := s.now()
	for _, it := range items {
		if it.Status != officers.StatusOpen && it.Status != officers.StatusClaimed {
			return false
		}
	}
	for _, it := range items {
		if _, err := s.officerStore.ResolveItem(octx, owner, it.Officer, it.ID, "no_action", minderHandoverSummary, now); err != nil {
			s.logger.Printf("session minder: owner %d: episode %d: hand item %d of %s to the chief: %v; the chief leaves the stop this pass",
				owner, c.episodeID, it.ID, it.Officer, err)
			return false
		}
	}
	return true
}

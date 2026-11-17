package api

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/fleet"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/prroute"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/sessionstate"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE FLEET READER (internal/fleet; docs/chief.md "Fleet digest"): the ONE
// read of an owner's whole fleet that fleet_list (focus_tools.go) and the
// digest card (fleet_digest.go) both build on, so the chief and the owner see
// the same sessions in the same states.
//
// Per session it reads, as ctx's principal:
//
//   - the session itself: the owner's dispatched agents (Agents.List, the
//     owner's OWN rows only) and the windows of the owner's OWN online hosts
//     (mirrorFocusEntries → Mirror.Hosts);
//   - its open stop: the session-state pass's open episodes (one query) —
//     its fleet state is fleet.StateOf of that and whether the host shows a
//     prompt now;
//   - its pull requests: the ones muster recorded it creating (session_prs,
//     newest first, fleetPRsPerSession of them) and GitHub's state for each,
//     through the App's own read behind a two-minute cache (fleetPRTTL), at
//     most fleetPRReadsPerCall reads a call;
//   - whether a message is queued to it (only where it decides a group: a
//     stall whose latest pull request closed);
//   - its open fleet actions (fleet_actions, one query).
//
// 🔴 OWN HOSTS ONLY, NEVER A TEAM'S. The mirrored sessions come from
// Mirror.Hosts (the principal's own), never VisibleHosts (which adds the
// principal's teams' hosts); the agents are filtered to the owner's own, since
// a member's agent list can hold a team's. A team's sessions belong to its own
// chief. TestTheFleetReadersSubjectSources pins the sources, failing when the
// set grows or shrinks.
//
// 🔴 NO SESSION TEXT AND NO MODEL. The reader reads no transcript; fleet_list's
// owner facts (attachOwnerFacts) are its only transcript read, bounded, and the
// digest never makes it.
// ---------------------------------------------------------------------------

const (
	// fleetPRsPerSession bounds a row's prs: its newest.
	fleetPRsPerSession = 3
	// fleetPRReadsPerCall bounds the GitHub reads one fleet read makes; past
	// it a state is unknown until a later read (the cache keeps the others).
	fleetPRReadsPerCall = 12
	// fleetPRTTL is how long an answer is reused: the two-minute bound.
	fleetPRTTL = 2 * time.Minute
	// fleetPRReadTimeout bounds one GitHub read; fleetPRReadsDeadline all of
	// one call's reads together.
	fleetPRReadTimeout   = 4 * time.Second
	fleetPRReadsDeadline = 8 * time.Second
	// fleetOwnerReadsPerCall bounds fleet_list's transcript reads (the owner
	// facts of the stopped sessions).
	fleetOwnerReadsPerCall = 8
)

// fleetSubject is one of the owner's sessions as the fleet reads it.
type fleetSubject struct {
	// row is its fleet_list row: today's fields, the reader's added by
	// fleetListRows.
	row fleetListRow
	// kind and key name it as the stall sweep does: a mirrored window's
	// session key, or a dispatched agent's id.
	kind, key string
	view      mirror.SessionView // a mirrored session's window
	agent     agents.Agent       // a dispatched agent
	// chief: this agent is the owner's chief (the digest leaves it out; its
	// own open cards are the digest's Needs you asks).
	chief bool
	// waitingNow: its host shows a prompt on screen now.
	waitingNow bool
	// episode is its open stop (nil: none, or the episodes were not read).
	episode *sessionstate.Episode
	// state is its fleet state ("" when the episodes were not read).
	state string
	// prs are its pull requests, newest first; prsRead says they were read
	// (a mirrored session, with the store present and the read ok).
	prs     []fleet.PR
	prsRead bool
	// inFlight: a message is queued to it (read only where it decides a
	// group).
	inFlight bool
	// open is its open fleet actions.
	open int
}

// fleetReadResult is one read of the fleet.
type fleetReadResult struct {
	subjects []fleetSubject
	// stateOn: the open episodes were read, so every state is known.
	stateOn bool
	// openOn: the open fleet actions were read.
	openOn bool
}

// errFleetAgents: the agent list could not be read.
type errFleetAgents struct{ err error }

func (e errFleetAgents) Error() string { return "could not list agents: " + e.err.Error() }

// readFleet is the fleet as ctx's principal sees it, filtered by f (fleet_list's
// active_within_hours; the zero filter keeps every session).
func (s *Server) readFleet(ctx context.Context, f fleetListFilter) (fleetReadResult, error) {
	owner := requestOwner(ctx)
	fi, now := s.focusIndex(ctx), s.now()
	var out fleetReadResult
	if s.ext.Agents != nil {
		list, err := s.ext.Agents.List(ctx)
		if err != nil {
			return out, errFleetAgents{err}
		}
		// A host officer instance's row is never provisioned and is seen as its
		// window (agents.ProvisionedOnly; the hostRowConsumers ledger).
		list = agents.ProvisionedOnly(list)
		isChief := chiefPredicate(list)
		idx := s.instanceIndex(ctx)
		// 🔴 THE OWNER'S OWN (ownedBy): a member's list can hold a team's
		// agents (team sharing), which are the team's chief's to orchestrate.
		for _, a := range ownedBy(list, owner) {
			sub := focus.AgentSubject(a.ID)
			status := liveStatusIndexed(a, idx)
			at := a.CreatedAt
			if u := fi[sub].UpdatedAt; fi[sub].Has() && u.After(at) {
				at = u
			}
			if !f.keep(at, now) {
				continue
			}
			out.subjects = append(out.subjects, fleetSubject{kind: chiefnudge.KindAgent, key: strconv.FormatInt(a.ID, 10), agent: a,
				chief: isChief(a),
				row: fleetListRow{ID: sub.String(), Kind: agents.ResolveKind(a.Kind),
					Name: fi.agentName(a).Text, Code: a.Name, Status: status, Focus: fi[sub].Title,
					focusAgeFields: fi.ageFields(sub, now), FocusProblem: fi[sub].Problem()}})
		}
	}
	seen := map[string]bool{}
	for _, e := range s.mirrorFocusEntries(ctx) {
		if seen[e.SessionKey()] {
			continue
		}
		seen[e.SessionKey()] = true
		sub := focus.MirrorSubject(e.SessionKey())
		st := s.ext.Mirror.AgentState(ctx, e.Host, e.Session, e.Window)
		status := st.Status
		if status == "" {
			status = "unknown"
		}
		tasks := fi[sub].Tasks
		if !f.keep(e.ActiveAt(), now) {
			continue
		}
		r := fleetListRow{ID: sub.String(), Kind: "mirror", Name: fi.mirrorName(e).Text, Host: e.Host,
			Where: e.Where(), Status: status, Focus: fi[sub].Title, focusAgeFields: fi.ageFields(sub, now), FocusProblem: fi[sub].Problem(),
			TaskLink: focus.TaskLink{Keys: tasks.Keys, None: tasks.None, Note: tasks.Note}}
		if at := e.ActiveAt(); !at.IsZero() {
			r.ActiveAt = at.UTC().Format(time.RFC3339)
		}
		if !tasks.SetAt.IsZero() {
			r.TaskSetAt = tasks.SetAt.UTC().Format(time.RFC3339)
		}
		out.subjects = append(out.subjects, fleetSubject{kind: chiefnudge.KindMirror, key: e.SessionKey(), view: e, row: r,
			waitingNow: st.Status == mirror.StatusWaiting})
	}
	out.stateOn = s.attachFleetEpisodes(ctx, out.subjects)
	s.attachFleetPRs(ctx, out.subjects)
	s.attachFleetInFlight(ctx, out.subjects)
	out.openOn = s.attachFleetOpenActions(ctx, out.subjects)
	return out, nil
}

// attachFleetEpisodes sets each subject's open stop and fleet state from ONE
// read of the open episodes; false (no state on any) when session state is
// off or the read failed.
func (s *Server) attachFleetEpisodes(ctx context.Context, subs []fleetSubject) bool {
	if s.ext.Episodes == nil {
		return false
	}
	open, err := s.ext.Episodes.OpenEpisodes(ctx)
	if err != nil {
		s.logger.Printf("fleet: open episodes: %v", err)
		return false
	}
	mirrorEp := map[string]*sessionstate.Episode{}
	agentEp := map[string]*sessionstate.Episode{}
	for i := range open {
		ep := &open[i]
		switch ep.Kind {
		case sessionstate.KindMirror:
			mirrorEp[ep.Key] = ep
		case sessionstate.KindAgent:
			// An agent's stops are per conversation ("<id>:<chat>"): the
			// oldest open one is the agent's (OpenEpisodes is oldest first).
			id, _, _ := strings.Cut(ep.Key, ":")
			if _, ok := agentEp[id]; !ok {
				agentEp[id] = ep
			}
		}
	}
	for i := range subs {
		if subs[i].kind == chiefnudge.KindMirror {
			subs[i].episode = mirrorEp[subs[i].key]
		} else {
			subs[i].episode = agentEp[subs[i].key]
		}
		subs[i].state = fleet.StateOf(subs[i].waitingNow, fleetEpisodeOf(subs[i].episode))
	}
	return true
}

// fleetEpisodeOf is ep as the fleet package reads it (nil for nil).
func fleetEpisodeOf(ep *sessionstate.Episode) *fleet.Episode {
	if ep == nil {
		return nil
	}
	return &fleet.Episode{ID: ep.ID, State: ep.State, Waiting: ep.Waiting, Since: ep.OpenedAt, JevLabel: ep.Jev.Label}
}

// sessionPRLister is the pull-request store's per-session read
// (plugins.PGStore.LatestSessionPRs); a store without it lists none.
type sessionPRLister interface {
	LatestSessionPRs(ctx context.Context, claudeSessions []string, per int) (map[string][]plugins.PRSession, error)
}

// attachFleetPRs sets each mirrored subject's pull requests (newest first)
// and GitHub's state for each, within fleetPRReadsPerCall reads and
// fleetPRReadsDeadline: the latest pull request of a stalled session first,
// as it decides the session's group, then every other latest, then the rest.
// A state not read is unknown.
func (s *Server) attachFleetPRs(ctx context.Context, subs []fleetSubject) {
	lister, ok := s.ext.PRSessions.(sessionPRLister)
	if !ok {
		return
	}
	var keys []string
	for _, sub := range subs {
		if sub.kind == chiefnudge.KindMirror {
			keys = append(keys, sub.key)
		}
	}
	if len(keys) == 0 {
		return
	}
	by, err := lister.LatestSessionPRs(ctx, keys, fleetPRsPerSession)
	if err != nil {
		s.logger.Printf("fleet: session pull requests: %v", err)
		return
	}
	type at struct{ sub, pr int }
	var first, latest, rest []at
	for i := range subs {
		if subs[i].kind != chiefnudge.KindMirror {
			continue
		}
		subs[i].prsRead = true
		subs[i].prs = []fleet.PR{}
		for j, p := range by[subs[i].key] {
			subs[i].prs = append(subs[i].prs, fleet.PR{Repo: p.Repo, Number: p.PR, State: fleet.PRUnknown})
			switch {
			case j == 0 && subs[i].state == fleet.StateStalled:
				first = append(first, at{i, j})
			case j == 0:
				latest = append(latest, at{i, j})
			default:
				rest = append(rest, at{i, j})
			}
		}
	}
	reader := s.ext.FleetPRStates
	if reader == nil {
		return
	}
	rctx, cancel := context.WithTimeout(withPRReadBudget(ctx, fleetPRReadsPerCall), fleetPRReadsDeadline)
	defer cancel()
	for _, a := range append(append(first, latest...), rest...) {
		p := &subs[a.sub].prs[a.pr]
		st, known, err := s.fleetPRs.read(rctx, reader, prroute.Ref{Repo: p.Repo, PR: p.Number}, s.now(), fleetPRReadTimeout, fleetPRTTL, fleetPRTTL)
		if !known {
			if err != nil && err != errPRReadBudget && rctx.Err() == nil {
				s.logger.Printf("fleet: read %s#%d: %v; its state is unknown", p.Repo, p.Number, err)
			}
			continue
		}
		p.State = fleetPRState(st.State, st.Merged)
		if len(st.HeadSHA) >= 12 {
			p.HeadSHA12 = st.HeadSHA[:12]
		}
		p.ClosedAt = st.ClosedAt
	}
}

// fleetPRState is a pull request's fleet state from GitHub's state and merged
// flag.
func fleetPRState(state string, merged bool) string {
	switch {
	case state == "open":
		return fleet.PROpen
	case state == "closed" && merged:
		return fleet.PRMerged
	case state == "closed":
		return fleet.PRClosed
	}
	return fleet.PRUnknown
}

// attachFleetInFlight reads, for each stalled mirrored session whose latest
// pull request is merged or closed — the case where it decides Done or not —
// whether a message is queued to its window (send_queue, queued or sending).
// A failed read counts as in flight: a session is never Done on a guess.
func (s *Server) attachFleetInFlight(ctx context.Context, subs []fleetSubject) {
	for i := range subs {
		sub := &subs[i]
		if sub.kind != chiefnudge.KindMirror || sub.state != fleet.StateStalled || len(sub.prs) == 0 ||
			(sub.prs[0].State != fleet.PRMerged && sub.prs[0].State != fleet.PRClosed) {
			continue
		}
		if s.ext.SendQueue == nil {
			continue
		}
		items, err := s.ext.SendQueue.List(ctx, sendqueue.Target{Host: sub.view.Host, Session: sub.view.Session, Window: sub.view.Window})
		if err != nil {
			s.logger.Printf("fleet: queued sends of %s: %v; counted in flight", sub.row.ID, err)
			sub.inFlight = true
			continue
		}
		for _, it := range items {
			if it.State.Open() {
				sub.inFlight = true
				break
			}
		}
	}
}

// attachFleetOpenActions sets each subject's open fleet actions from ONE read;
// false when there is no store or the read failed.
func (s *Server) attachFleetOpenActions(ctx context.Context, subs []fleetSubject) bool {
	if s.ext.Fleet == nil {
		return false
	}
	counts, err := s.ext.Fleet.OpenCounts(ctx)
	if err != nil {
		s.logger.Printf("fleet: open actions: %v", err)
		return false
	}
	for i := range subs {
		sub := &subs[i]
		if sub.kind == chiefnudge.KindMirror {
			sub.open = counts[sessionstate.KindMirror+":"+sub.key]
			continue
		}
		// An agent's actions are per conversation ("<id>:<chat>").
		for k, n := range counts {
			if strings.HasPrefix(k, sessionstate.KindAgent+":"+sub.key+":") {
				sub.open += n
			}
		}
	}
	return true
}

// chiefOpenCards is the chief's own open ask_owner cards (attention asks
// with OwnerCard, asked by chief), oldest first: the digest's Needs you asks.
func (s *Server) chiefOpenCards(ctx context.Context, chief agents.Agent) []attention.Ask {
	if s.ext.Attention == nil || chief.ID == 0 {
		return nil
	}
	open, err := s.ext.Attention.Open(ctx)
	if err != nil {
		s.logger.Printf("fleet: the chief's open cards: %v", err)
		return nil
	}
	var out []attention.Ask
	for _, a := range open {
		if a.OwnerCard && a.Kind != attention.KindNotice && a.Status == attention.Open && a.Asker.AgentID == chief.ID {
			out = append(out, a)
		}
	}
	return out
}

// fleetSessionHref is where a subject's "Open session" goes: a mirrored
// session's rendered view (its terminal when it has none), or an agent's
// conversation — its stop's, when it has one.
func fleetSessionHref(sub fleetSubject) string {
	if sub.kind == chiefnudge.KindAgent {
		var chat int64
		if sub.episode != nil {
			chat = sub.episode.ChatSessionID
		}
		return ui.AgentSessionHref(sub.agent.Name, chat)
	}
	ref := ui.MirrorRef{Host: sub.view.Host, Session: sub.view.Session, Window: sub.view.Window}
	if sub.view.Claude || sub.view.AgentSession != "" {
		return ref.ViewPath()
	}
	return ref.TerminalPath()
}

// --- fleet_list's owner facts -------------------------------------------------

// fleetOwnerFacts is one stopped session's owner facts: the owner's newest own
// message in it, and how many of muster's own texts came after it.
type fleetOwnerFacts struct {
	lastOwner  time.Time // zero: none read
	sinceOwner int
}

// fleetOwnerCache keeps each episode's facts for the streak they were read
// on: a session whose streak has not moved has no new message, so its facts
// still hold.
type fleetOwnerCache struct {
	mu sync.Mutex
	m  map[int64]fleetOwnerEntry
}

type fleetOwnerEntry struct {
	streak string
	facts  fleetOwnerFacts
}

func (c *fleetOwnerCache) get(ep int64, streak string) (fleetOwnerFacts, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[ep]
	return e.facts, ok && e.streak == streak && streak != ""
}

func (c *fleetOwnerCache) put(ep int64, streak string, f fleetOwnerFacts) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) >= 4096 {
		c.m = map[int64]fleetOwnerEntry{}
	}
	c.m[ep] = fleetOwnerEntry{streak: streak, facts: f}
}

// ownerFactsOf is the facts of subject sub's open stop: from the cache for its
// streak, else ONE transcript read (the stall sweep's own, nudgeTranscript)
// within budget. ok false when the session is not stopped, or the read was
// not made or failed.
func (s *Server) ownerFactsOf(ctx context.Context, sub fleetSubject, budget *int) (fleetOwnerFacts, bool) {
	ep := sub.episode
	if ep == nil {
		return fleetOwnerFacts{}, false
	}
	if f, ok := s.fleetOwners.get(ep.ID, ep.StreakKey); ok {
		return f, true
	}
	if *budget <= 0 {
		return fleetOwnerFacts{}, false
	}
	*budget--
	c := nudgeCandidate{Session: chiefnudge.Session{Kind: ep.Kind, Key: ep.Key, Runtime: ep.Runtime, Host: ep.Host,
		TmuxSession: ep.TmuxSession, Window: ep.Window, AgentID: ep.AgentID, ChatSessionID: ep.ChatSessionID}}
	read, err := s.nudgeTranscript(ctx, c)
	if err != nil {
		s.logger.Printf("fleet: owner facts of %s: %v", sub.row.ID, err)
		return fleetOwnerFacts{}, false
	}
	injected, err := s.ownerInjected(ctx, c.Session)
	if err != nil {
		s.logger.Printf("fleet: owner facts of %s: injected: %v", sub.row.ID, err)
		return fleetOwnerFacts{}, false
	}
	last, ok, after := chiefnudge.SinceOwner(read.Msgs, injected, nudgeInjectedPrefixes)
	f := fleetOwnerFacts{sinceOwner: after}
	if ok {
		f.lastOwner = last.At
	}
	s.fleetOwners.put(ep.ID, ep.StreakKey, f)
	return f, true
}

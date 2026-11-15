package api

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
)

// ---------------------------------------------------------------------------
// OPEN STREAMS FOLLOW THE MEMBERSHIP (team sharing 2b.3;
// docs/design/team-sharing.md A, "Live streams follow the membership").
//
// A request a member holds open on something their TEAM owns — an SSE stream
// subscribed to the team's events, a team host's terminal, Rendered stream or
// subagent read, a team agent's terminal — was admitted with the role the
// member held then. Phase 2a carried forward that it kept that principal for
// as long as it stayed open. This file ends it instead:
//
//   - teamLiveOpen registers the request by (team, person) with the role it
//     NEEDS (a viewer's stream: viewer; a team agent's shell: operator);
//   - closeTeamLive ends, at once, those a removal or a role change in THIS
//     process left the person short of (teamMembershipEnded, teamRoleSet);
//   - sweepTeamLive re-reads every registered person's memberships in ONE
//     query each teamLiveEvery (DisableSweepInterval, 10 s) and ends the
//     same — the removal another replica, or the CLI, made.
//
// A request ended here is cancelled with its cause; an SSE stream's page
// reconnects and subscribes to the person's teams as they are now. A person
// ADDED to a team sees its events from their next reconnect or page load
// (accepted, and stated in the design).
// ---------------------------------------------------------------------------

// teamLiveSet is every open request a member holds on a team's surface.
type teamLiveSet struct {
	mu   sync.Mutex
	open map[[2]int64]map[*teamLive]struct{}
	// sweep starts the periodic sweep with the first registration.
	sweep sync.Once
}

// teamLive is one registered request: the role it needs and its cancel.
type teamLive struct {
	need   authz.TeamRole
	cause  error
	cancel context.CancelCauseFunc
}

// The causes an open team request is ended with.
var (
	// errTeamTerminalClosed: a member's terminal on a team AGENT, once they
	// are no longer an operator of the team.
	errTeamTerminalClosed = errors.New("team terminal closed: no longer an operator of the team")
	// errTeamStreamClosed: a member's view of a team host, or their event
	// stream subscribed to the team, once they are no longer a member at the
	// role it needs.
	errTeamStreamClosed = errors.New("team stream closed: your role in the team changed")
)

// teamLiveSweepEvery is how often sweepTeamLive runs: DisableSweepInterval,
// the session tier's own sweep, outside tests.
func (s *Server) teamLiveSweepEvery() time.Duration {
	if s.teamLiveEvery > 0 {
		return s.teamLiveEvery
	}
	return DisableSweepInterval
}

// teamLiveOpen registers ctx — person's request on team's surface, needing
// role need — and returns the context to serve it under (cancelled by
// closeTeamLive or the sweep, with cause) and its unregister. One request may
// be registered under several teams (an SSE stream subscribes to each of its
// person's teams): each registration is its own, and each done must run.
func (s *Server) teamLiveOpen(ctx context.Context, team, person int64, need authz.TeamRole, cause error) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	done := s.teamLiveAdd(team, person, &teamLive{need: need, cause: cause, cancel: cancel})
	return ctx, func() { done(); cancel(nil) }
}

// teamLiveAdd registers l under (team, person) and returns its unregister.
func (s *Server) teamLiveAdd(team, person int64, l *teamLive) func() {
	key := [2]int64{team, person}
	t := &s.teamLive
	t.mu.Lock()
	if t.open == nil {
		t.open = map[[2]int64]map[*teamLive]struct{}{}
	}
	if t.open[key] == nil {
		t.open[key] = map[*teamLive]struct{}{}
	}
	t.open[key][l] = struct{}{}
	t.mu.Unlock()
	t.sweep.Do(func() { safeGo(s.logger, "team stream sweep", s.runTeamLiveSweep) })
	return func() {
		t.mu.Lock()
		delete(t.open[key], l)
		if len(t.open[key]) == 0 {
			delete(t.open, key)
		}
		t.mu.Unlock()
	}
}

// closeTeamLive ends every request person holds open on team's surface that
// role no longer reaches: all of them when in is false (no longer a member).
func (s *Server) closeTeamLive(team, person int64, role authz.TeamRole, in bool) {
	t := &s.teamLive
	t.mu.Lock()
	defer t.mu.Unlock()
	for l := range t.open[[2]int64{team, person}] {
		if !in || !role.AtLeast(l.need) {
			l.cancel(l.cause)
		}
	}
}

// membershipsReader is the account store's batched membership read
// (users.PGStore.MembershipsAmong), asserted so a store without it sweeps
// nothing (the in-process closes still run).
type membershipsReader interface {
	MembershipsAmong(ctx context.Context, persons []int64) (map[int64][]authz.Membership, error)
}

// runTeamLiveSweep calls sweepTeamLive every teamLiveSweepEvery until the
// server's streams end (shutdown).
func (s *Server) runTeamLiveSweep() {
	t := time.NewTicker(s.teamLiveSweepEvery())
	defer t.Stop()
	end := s.streams.context()
	for {
		select {
		case <-end.Done():
			return
		case <-t.C:
			s.sweepTeamLive(end)
		}
	}
}

// sweepTeamLive re-reads, in one query, the memberships of every person with
// an open team request, and ends each request their role no longer reaches.
// A failed read ends nothing (logged): the in-process closes, and the next
// sweep, still run.
func (s *Server) sweepTeamLive(ctx context.Context) {
	mr, ok := s.ext.Users.(membershipsReader)
	if !ok {
		return
	}
	t := &s.teamLive
	t.mu.Lock()
	seen := map[int64]bool{}
	var persons []int64
	var keys [][2]int64
	for k := range t.open {
		keys = append(keys, k)
		if !seen[k[1]] {
			seen[k[1]] = true
			persons = append(persons, k[1])
		}
	}
	t.mu.Unlock()
	if len(persons) == 0 {
		return
	}
	sort.Slice(persons, func(i, j int) bool { return persons[i] < persons[j] })
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ms, err := mr.MembershipsAmong(qctx, persons)
	if err != nil {
		s.logger.Printf("team streams: sweep could not read memberships (open streams unchanged): %v", err)
		return
	}
	for _, k := range keys {
		role, in := authz.TeamRole(""), false
		for _, m := range ms[k[1]] {
			if m.Team == k[0] {
				role, in = m.Role, true
				break
			}
		}
		s.closeTeamLive(k[0], k[1], role, in)
	}
}

// teamTerminalOpen registers r — person's terminal socket on an agent of team,
// let through by the door — so a removal, a downgrade or the sweep ends it,
// and returns the request to serve under, with its unregister. Then it reads
// the person's role in the team AGAIN, fresh (teamOperatorNow): the door judged
// the request's principal, resolved before a removal or downgrade that may
// have landed since — and that change's close ran before this socket
// registered, so it would never close it. Not an operator now: 403, and nil.
//
// 🔴 A MEMBER'S TERMINAL CLOSES WHEN THEY STOP BEING AN OPERATOR (PR #682
// round 1). The shell holds the team's credentials; the door alone checked
// it only when it opened. A disable and a sign-out end it already (the
// session tier's live requests, accounts.go); a removal and a downgrade
// below operator end it here (teamMembershipEnded, teamRoleSet, and since
// 2b.3 the sweep for one made elsewhere).
func (s *Server) teamTerminalOpen(w http.ResponseWriter, r *http.Request, team int64) (*http.Request, func()) {
	p, _ := authz.From(r.Context())
	ctx, done := s.teamLiveOpen(r.Context(), team, p.UserID, authz.TeamOperator, errTeamTerminalClosed)
	if !s.teamOperatorNow(ctx, p.UserID, team) {
		done()
		http.Error(w, TeamAgentTerminalOperator, http.StatusForbidden)
		return nil, nil
	}
	return r.WithContext(ctx), done
}

// teamEventsOpen registers an SSE stream for each team in its person's
// memberships (Principal.TeamScope, read with the request), at viewer, and
// returns the stream's context and its unregister: a removal from any of
// them ends the stream, and the page reconnects with the person's teams as
// they are then. The teams it subscribes to are teams.
func (s *Server) teamEventsOpen(ctx context.Context) (context.Context, []int64, func()) {
	p, ok := authz.From(ctx)
	if !ok || p.All() {
		return ctx, nil, func() {}
	}
	var teams []int64
	ctx, cancel := context.WithCancelCause(ctx)
	var dones []func()
	for _, m := range p.TeamScope().List() {
		if !m.Role.AtLeast(authz.TeamViewer) {
			continue
		}
		teams = append(teams, m.Team)
		dones = append(dones, s.teamLiveAdd(m.Team, p.UserID, &teamLive{need: authz.TeamViewer, cause: errTeamStreamClosed, cancel: cancel}))
	}
	return ctx, teams, func() {
		for _, d := range dones {
			d()
		}
		cancel(nil)
	}
}

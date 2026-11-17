package api

import (
	"context"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE SIDEBAR'S OFFICERS GROUP — its roster (internal/ui nav_officers.go draws
// it): one ui.NavOfficer per declared officer the viewer can see, their own
// and each team's they are in.
//
// 🔴 CHEAP, AND BATCHED: the sidebar's partial is read on every refresh of
// every page with the rail, and must not wait on a pod or the apiserver
// (handleNavAgents). So:
//
//   - the roster is IN MEMORY: MUSTER_OFFICERS' officers (officerRuntime.env)
//     and the last roster load's DB-declared ones (officerRuntime.declared,
//     every declared row, a misconfigured one included) — no query;
//   - the queued counts are ONE query for every officer (builtin.Store.
//     ItemCounts), and only when an officer is builtin;
//   - the day's spend is ONE query for every officer (builtin.Store.SpentOn,
//     the budget's own sum), and only when an officer has a daily budget;
//     "held" is officers.Officer.OverBudget over it, the cap's own predicate;
//   - the teams' names are one read (teamNames), only when a team's officer
//     is listed.
//
// So a sidebar load adds at most three reads, however many officers there are
// (TestTheOfficerRosterReadsInBatches). Whether an officer runs is read from
// the agent rows the list already holds (AgentCardView.Officer), never here.

// navOfficerReads is what the roster reads from the built-in store
// (*builtin.Store), an interface so a test can count the calls.
type navOfficerReads interface {
	ItemCounts(ctx context.Context, owners []int64, statuses []string) (map[builtin.OfficerRef]int, error)
	SpentOn(ctx context.Context, owners []int64, now time.Time) (map[builtin.OfficerRef]float64, error)
}

// queuedStatuses are the item statuses the sidebar counts as queued: work
// waiting for the officer to claim it.
var queuedStatuses = []string{officers.StatusOpen}

// navOfficers is the Officers group's roster for the request's viewer: none
// when the server keeps no officers (ui.Features.Officers, officersOn) or has
// no officer runtime, or the request has no viewer.
func (s *Server) navOfficers(ctx context.Context) []ui.NavOfficer {
	if !s.officersOn() || s.officerRT == nil {
		return nil
	}
	return s.navOfficerRoster(ctx, s.officerStore)
}

// navRosterEntry is one officer of the roster, before its reads.
type navRosterEntry struct {
	view ui.NavOfficer
	// officer is what runs (the registry's), nil for a declared row that does
	// not run: no budget is known for it.
	officer *officers.Officer
	owner   int64
	builtin bool
}

// navOfficerRoster is navOfficers over reads.
func (s *Server) navOfficerRoster(ctx context.Context, reads navOfficerReads) []ui.NavOfficer {
	viewer := requestOwner(ctx)
	rt := s.officerRT
	if viewer <= 0 || rt == nil {
		return nil
	}
	visible := map[int64]bool{viewer: true}
	if p, ok := authz.From(ctx); ok {
		for _, m := range p.TeamScope().List() {
			visible[m.Team] = true
		}
	}
	var entries []navRosterEntry
	add := func(owner int64, id, name, key string, o *officers.Officer, builtinBackend bool) {
		v := ui.NavOfficer{Key: key, ID: id, Name: name, Href: officerHref(navOfficerScope(owner, viewer), id, "")}
		if owner != viewer {
			v.TeamOwner = owner
		}
		if o != nil {
			v.OnHost = o.OnHost()
			v.Budget = o.Budget.DailyUSD
		}
		entries = append(entries, navRosterEntry{view: v, officer: o, owner: owner, builtin: builtinBackend})
	}
	for i := range rt.env {
		o := rt.env[i]
		if visible[o.Owner] {
			add(o.Owner, o.ID, "", o.Key(), &o, o.IsBuiltin())
		}
	}
	rt.declMu.Lock()
	for _, st := range rt.declared {
		r := st.row
		if !visible[r.Owner] {
			continue
		}
		// An env officer of the same owner and id wins (resolveDeclared): it
		// has its row already.
		if _, ok := rt.envOfficer(r.Owner, r.Officer); ok {
			continue
		}
		var o *officers.Officer
		if st.officer != nil {
			c := *st.officer
			o = &c
		}
		add(r.Owner, r.Officer, r.TypeName, r.Fields.InstanceSlug, o, true)
	}
	rt.declMu.Unlock()
	if len(entries) == 0 {
		return nil
	}

	// The batched reads.
	var queueOwners, spendOwners, teams []int64
	seenQ, seenS, seenT := map[int64]bool{}, map[int64]bool{}, map[int64]bool{}
	for _, e := range entries {
		if e.builtin && !seenQ[e.owner] {
			seenQ[e.owner] = true
			queueOwners = append(queueOwners, e.owner)
		}
		if e.officer != nil && e.officer.Budget.DailyUSD > 0 && !seenS[e.owner] {
			seenS[e.owner] = true
			spendOwners = append(spendOwners, e.owner)
		}
		if e.view.TeamOwner != 0 && !seenT[e.owner] {
			seenT[e.owner] = true
			teams = append(teams, e.owner)
		}
	}
	var queued map[builtin.OfficerRef]int
	queueOK := false
	if len(queueOwners) > 0 && reads != nil {
		var err error
		if queued, err = reads.ItemCounts(ctx, queueOwners, queuedStatuses); err != nil {
			s.logger.Printf("officers: the sidebar's queued counts: %v (drawn without them)", err)
		} else {
			queueOK = true
		}
	}
	var spent map[builtin.OfficerRef]float64
	spendOK := false
	if len(spendOwners) > 0 && reads != nil {
		var err error
		if spent, err = reads.SpentOn(ctx, spendOwners, rt.clock()); err != nil {
			s.logger.Printf("officers: the sidebar's spend read: %v (drawn without a figure)", err)
		} else {
			spendOK = true
		}
	}
	names := map[int64]string{}
	if len(teams) > 0 {
		names = s.teamNames(ctx, teams)
	}

	out := make([]ui.NavOfficer, 0, len(entries))
	for _, e := range entries {
		v := e.view
		ref := builtin.OfficerRef{Owner: e.owner, Officer: v.ID}
		if e.builtin && queueOK {
			n := queued[ref]
			v.Queued = &n
		}
		if e.officer != nil && e.officer.Budget.DailyUSD > 0 && spendOK {
			x := spent[ref]
			v.Spent = &x
			v.Held = e.officer.OverBudget(x)
		}
		if v.TeamOwner != 0 {
			v.Team = names[v.TeamOwner]
		}
		out = append(out, v)
	}
	return out
}

// navOfficerScope is owner's officers' scope for a link drawn to viewer: their
// own, or a team's (owner= in the link).
func navOfficerScope(owner, viewer int64) officerScope {
	if owner == viewer {
		return officerScope{Owner: owner}
	}
	return officerScope{Owner: owner, Team: true}
}

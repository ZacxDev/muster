package api

import (
	"context"
	"fmt"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
)

// ---------------------------------------------------------------------------
// WHETHER A HOST CAN START AN OFFICER, AND WHY AN INSTANCE STARTS
// (officers redesign O-B)
//
// A mirror host advertises its spawn readiness (mirror.SpawnReadiness): its
// -agent-spawn-repos allow-list, scratch spawns, and whether claude is found
// and logged in. muster reads it in ONE place (hostReadyRefusal) for the four
// readers that ask: a "runtime": "auto" officer's placement and
// runtime-options (ownerHostOptions), the start itself (hostStart), and
// `muster doctor` (doctorOfficers). A host that sent none (an older
// muster-mirror) is judged as before: it qualifies, and the host itself
// refuses what it does not do.
// ---------------------------------------------------------------------------

// hostReadyRefusal is why host v cannot start an instance of a host officer
// of repo ("" for none: a scratch directory) now, as its readiness says, or
// "" when nothing it reported stands in the way. needLogin: also refuse a
// host whose Claude Code is not logged in — what auto placement and doctor
// ask; a start of an officer pinned to the host does not, since its window
// shows the login screen the owner can finish there (and the unclaimed
// notice names it).
func hostReadyRefusal(v mirror.HostView, repo string, needLogin bool) string {
	why, _ := hostReadyProblem(v, repo, needLogin)
	return why
}

// hostReadyProblem is hostReadyRefusal with what to do about it on the host
// (the doctor's fix line): THE one reading of a host's readiness.
func hostReadyProblem(v mirror.HostView, repo string, needLogin bool) (why, fix string) {
	r := v.SpawnReady
	if r == nil {
		if repo == "" {
			return "its muster-mirror predates officers with no repository (it needs a current muster-mirror)",
				"upgrade muster-mirror on " + v.Host + " (any current one with -agent-spawn starts an officer with no repository)"
		}
		return "", ""
	}
	switch {
	case r.Unavailable != "":
		return "it starts no agent (" + r.Unavailable + ")",
			"on " + v.Host + ", run muster-mirror with -interactive -agent-spawn (MUSTER_MIRROR_INTERACTIVE=1, MUSTER_MIRROR_AGENT_SPAWN=1) and restart it"
	case r.Claude != mirror.ClaudeFound:
		return "claude is not found there (" + r.ClaudeDetail + ")",
			"on " + v.Host + ", set MUSTER_MIRROR_CLAUDE_PATH (-claude-path) to claude's absolute path (`command -v claude`) in muster-mirror's environment and restart it"
	case repo == "" && !r.Scratch:
		return "its muster-mirror does not start an officer with no repository (an older build)",
			"upgrade muster-mirror on " + v.Host + " (any current one with -agent-spawn starts an officer with no repository), or give the officer a repo"
	case repo != "" && !mirror.SpawnRepoAllowed(r.Repos, repo):
		return "repo " + repo + " is not on its -agent-spawn-repos",
			"on " + v.Host + ", add " + repo + " to MUSTER_MIRROR_AGENT_SPAWN_REPOS (-agent-spawn-repos), clone it under -agent-spawn-roots (~/src), and restart muster-mirror"
	case needLogin && r.Login == mirror.LoginNo:
		return "Claude Code is not logged in there (run `claude` once on it as that user and log in)",
			"on " + v.Host + ", run `claude` once as muster-mirror's user and log in (or `claude setup-token`)"
	}
	return "", ""
}

// hostRepoAllowed is whether v's allow-list takes repo, nil when it cannot be
// told (an older muster-mirror) or repo is "" (a scratch spawn).
func hostRepoAllowed(v mirror.HostView, repo string) *bool {
	if v.SpawnReady == nil || repo == "" {
		return nil
	}
	ok := mirror.SpawnRepoAllowed(v.SpawnReady.Repos, repo)
	return &ok
}

// hostDownReason is why instance spawn row r (the instance's latest; ok
// false: it has none) is not up against its host's listing: the start line's
// reason (officers.InstanceState.Down), so every start and restart says why.
func hostDownReason(r officers.HostSpawn, ok bool, host string, windows []mirror.Window) string {
	switch {
	case !ok:
		return "first start (no window was started for it before)"
	case r.Refusal != "":
		return fmt.Sprintf("restart: its last start (spawn %d) was refused by %s: %s", r.ID, host, r.Refusal)
	case r.Window == "":
		return fmt.Sprintf("restart: its last start (spawn %d) got no answer from %s", r.ID, host)
	}
	if w, live := hostSpawnWindow(r, windows); live && w.ID == r.Window {
		return ""
	}
	return fmt.Sprintf("restart: its window %s on %s is gone (it exited, or was closed or killed outside muster)", r.Window, host)
}

// hostSpawnBudget refuses a start of o past its own spawn budget
// (officers.SpawnBudgetFor: sized from its idle, poll and shortest schedule),
// counted from its spawn rows. The host's own ceiling,
// -agent-spawn-officer-rate, still holds for every officer together.
func (f officerFleet) hostSpawnBudget(ctx context.Context, o officers.Officer) error {
	s := f.s
	st := s.officerHostSpawns()
	b := officers.SpawnBudgetFor(o, s.shortestSchedule(ctx, o))
	now := s.officerRT.now()
	short, err := st.CountHostSpawnsSince(ctx, o.ID, now.Add(-officers.SpawnBudgetShort))
	if err != nil {
		return fmt.Errorf("officers: %s: count its spawns: %w", o.ID, err)
	}
	long, err := st.CountHostSpawnsSince(ctx, o.ID, now.Add(-officers.SpawnBudgetLong))
	if err != nil {
		return fmt.Errorf("officers: %s: count its spawns: %w", o.ID, err)
	}
	if why := b.Refusal(short, long); why != "" {
		return fmt.Errorf("officers: %s: not starting it: %s", o.ID, why)
	}
	return nil
}

// shortestSchedule is the shortest interval between two fires of any of
// builtin officer o's enabled schedules (0: none, or not readable — the
// budget is then sized from its idle and poll alone).
func (s *Server) shortestSchedule(ctx context.Context, o officers.Officer) time.Duration {
	if s.officerStore == nil || !o.IsBuiltin() {
		return 0
	}
	list, err := s.officerStore.ListSchedules(s.officerPrincipal(ctx, o.Owner), o.Owner, o.ID)
	if err != nil {
		return 0
	}
	var best time.Duration
	for _, sc := range list {
		if !sc.Enabled {
			continue
		}
		c, err := builtin.ParseCron(sc.Cron)
		if err != nil {
			continue
		}
		if d := c.ShortestInterval(s.officerRT.now()); d > 0 && (best == 0 || d < best) {
			best = d
		}
	}
	return best
}

// officerKickoff is the wake loop's Kickoff (officers.Config.Kickoff): the
// kickoff with a builtin officer's type grants read now, so a granted
// officer_post_draft (and the contact tools) are listed in its first message.
func (s *Server) officerKickoff(ctx context.Context, o officers.Officer, n int) string {
	g := officers.KickoffGrants{}
	if s.officerStore != nil && o.IsBuiltin() {
		octx := s.officerPrincipal(ctx, o.Owner)
		if ok, err := s.officerStore.TypeGrants(octx, o.Owner, o.ID, builtin.CapOutboundDrafts); err == nil {
			g.OutboundDrafts = ok
		}
		if ok, err := s.officerStore.TypeGrants(octx, o.Owner, o.ID, builtin.CapContacts); err == nil {
			g.Contacts = ok
		}
	}
	return officers.KickoffTextWith(o, n, g)
}

// spawnWhere is a host start's log words for where the instance runs.
func spawnWhere(o officers.Officer) string {
	if o.Repo == "" {
		return "a private scratch directory"
	}
	return "a worktree of " + o.Repo
}

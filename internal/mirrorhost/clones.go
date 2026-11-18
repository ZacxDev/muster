package mirrorhost

import (
	"context"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// WHICH OF THIS HOST'S CLONES A SPAWN IS CUT FROM: the host half of
// agent-clones (the protocol, and why, is internal/mirror/clones.go).
//
// 🔴 THE HOST ANSWERS ONLY WITH WHAT IT FOUND ITSELF, AND ONLY FOR A
// REPOSITORY IT WOULD SPAWN: spawning on (agentSpawnUnavailable), the name's
// shape and the allow-list are checked exactly as a spawn checks them, and the
// clones are clonesOf's — the one enumeration resolveClone resolves a spawn
// against, so a hint muster echoes back from this list is accepted there and
// nothing else is.
//
// LIVE: a clone is live when one of this host's listed windows works in it —
// its cwd's checkout, by repo.go's lookup (the rule the repo chip and
// github_repo are read by: a worktree resolves to its main tree through its
// commondir, with no git process), has that clone as its main working tree.
// A window muster started (its @muster-dispatch tag) is not the owner's own
// session and does not count: an old spawned agent must not make its clone
// look like the one the owner works in.

// clonesListTimeout bounds one agent-clones answer (two git reads per entry of
// the roots, and one window list).
const clonesListTimeout = 15 * time.Second

// agentClones serves an agent-clones and answers agent-clones-list.
func (h *hostSide) agentClones(m mirror.Msg) {
	ans := mirror.Msg{Type: mirror.TypeAgentClonesList, ID: m.ID}
	ctx, cancel := context.WithTimeout(h.ctx, clonesListTimeout)
	defer cancel()
	cl, err := h.c.listClones(ctx, h.c.cfg.Backend, m.CloneRepo)
	if e, ok := err.(*errSpawn); ok {
		ans.Refusal, ans.Error = e.code, e.detail
		h.c.log.Printf("muster-mirror: clone list for %q refused (%s): %s", capField(m.CloneRepo, 140), e.code, e.detail)
	} else {
		ans.Clones = cl
		h.c.log.Printf("muster-mirror: listed %d clone(s) of %s for muster", len(cl), m.CloneRepo)
	}
	_ = h.send(ans)
}

// listClones is this host's clones of repo, live ones marked, or the refusal
// (an *errSpawn).
func (c *Client) listClones(ctx context.Context, b Backend, repo string) ([]mirror.AgentClone, error) {
	sc := c.cfg.AgentSpawn
	if why := agentSpawnUnavailable(c.cfg); why != "" {
		return nil, refuseSpawn(mirror.SpawnUnavailable, "%s", why)
	}
	if !validRepoName(repo) {
		return nil, refuseSpawn(mirror.SpawnNoRepo, "%q is not <owner>/<repo>", capField(repo, 140))
	}
	if !repoAllowed(sc.Repos, repo) {
		return nil, refuseSpawn(mirror.SpawnNoRepo, "repo %s is not on this host's allow-list", repo)
	}
	found := clonesOf(ctx, sc.git(), sc.Roots, repo)
	live := c.liveMainTrees(ctx, b)
	out := make([]mirror.AgentClone, 0, len(found))
	for _, p := range found {
		out = append(out, mirror.AgentClone{Path: p, Live: live[p]})
	}
	return out, nil
}

// liveMainTrees is the main working trees (realpath) the host's own listed
// windows work in — muster-started windows left out — by repo.go's lookup.
// A backend that cannot list windows, or a failed list, has none.
func (c *Client) liveMainTrees(ctx context.Context, b Backend) map[string]bool {
	out := map[string]bool{}
	wl, ok := WindowListerOf(b)
	if !ok {
		return out
	}
	ws, err := wl.Windows(ctx)
	if err != nil {
		return out
	}
	for _, w := range ws {
		if w.Dispatch != 0 || w.Cwd == "" {
			continue
		}
		if main := c.repoNamer().lookup(w.Cwd).main; main != "" {
			out[realOrClean(main)] = true
		}
	}
	return out
}

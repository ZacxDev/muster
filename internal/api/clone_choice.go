package api

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/ZacxDev/muster/internal/mirror"
)

// ---------------------------------------------------------------------------
// WHICH CLONE A HOST DISPATCH STARTS IN (internal/mirror/clones.go has the
// wire and why).
//
// 🔴 MEASURED IN PRODUCTION: a host had two main working trees of the
// repository a host dispatch named — the one the owner's live mirrored
// sessions work in and an older standalone clone — and refused the approved
// start ("no_repo: … has 2 clones"); agent_dispatch had no way to choose.
// Operator decision: "Prefer the live sessions' clone".
//
//   - Before anything is held or reserved, muster asks a host that advertises
//     it (HostView.AgentSpawnClones) for its clones of the repository
//     (agent-clones). The host answers with ITS OWN list, each clone marked
//     live when one of its listed windows works in it.
//   - mirror.ChooseClone decides: the only clone, or the only live clone, is
//     the call's clone (in.Clone, set here as if the chief had passed it, so
//     the held hash and the request_key's hash bind it); otherwise the owner
//     chooses on the CLONE CARD (blankClone) — the approval card, showing the
//     whole request, its options the host's clones and "Don't start".
//   - The completing call passes the tapped clone as `clone`: completeHeld
//     binds it like a tapped repository (the hold's hash with the clone blank,
//     then the clone must be the tapped option's).
//   - The spawn carries it as a HINT (AgentSpawnReq.Clone). The host
//     re-enumerates and refuses one that is not its own (bad_clone).
//
// 🔴 AN OLDER HOST (no AgentSpawnClones) is asked nothing and sent no hint:
// with several clones it refuses no_repo as it always did — never a guess. A
// list muster cannot read (unanswered, invalid) likewise decides nothing.
// ---------------------------------------------------------------------------

// blankClone is the clone card's blank: an approval blank (it shows the whole
// request, and its tap is the owner's say-so for it).
const blankClone = "clone"

// clonePref prefixes a clone card's option values.
const clonePref = "clone:"

// cloneLiveSuffix marks, on the clone card, a clone the owner's live sessions
// work in.
const cloneLiveSuffix = " (your live sessions work here)"

const (
	agentDispatchCloneHostOnly  = "clone is only for a host target, after your owner chose one on a clone card: omit it"
	agentDispatchCloneNotListed = "clone %q is not one of host %q's clones of %s: pass exactly the clone your owner chose on its card, or omit it"
	agentDispatchCloneOldHost   = "host %q does not take a clone (its muster-mirror predates choosing one): omit clone"
	agentDispatchHeldChoseClone = "your owner chose the clone %s for this dispatch: call agent_dispatch with clone set to exactly that"
)

// mirrorCloneLister is the mirror plugin's clone list (*mirror.Service).
type mirrorCloneLister interface {
	AgentClones(ctx context.Context, host, repo string) ([]mirror.AgentClone, error)
}

// hostClones is host's own clones of repo (canonical owner/name) when the host
// lists them: ok false — decide nothing — for a host that does not
// (AgentSpawnClones), or a list that could not be read. refusal is the host's
// own refusal to list (the repository is not on its allow-list, spawning is
// off), which refuses the dispatch before anything is held.
func (s *Server) hostClones(ctx context.Context, host, runtime, repo string) (clones []mirror.AgentClone, ok bool, refusal string) {
	v, listed := s.hostView(ctx, host)
	if !listed || !v.AgentSpawnClones {
		return nil, false, ""
	}
	l, can := s.ext.Mirror.(mirrorCloneLister)
	if !can {
		return nil, false, ""
	}
	cl, err := l.AgentClones(ctx, host, repo)
	var r *mirror.ClonesRefusedError
	switch {
	case err == nil:
		return cl, true, ""
	case errors.As(err, &r):
		return nil, false, spawnRefusalSentence(r.Code, host, firstNonBlank(runtime, "host"), repo, "", r.Detail)
	}
	s.logger.Printf("agent_dispatch: list host %s's clones of %s (deciding nothing): %v", host, repo, err)
	return nil, false, ""
}

// cloneChoice is a host call's clone, settled before anything is held: in.Clone
// (given, or set from mirror.ChooseClone), whether the owner must choose it
// (blank, with choices), or the refusal.
type cloneChoice struct {
	clone   string
	blank   bool
	choices []mirror.AgentClone
	refusal string
}

// chooseClone settles the clone of a call to host for repo (canonical; "" when
// the repository is itself still a blank, which decides nothing). given is the
// call's own clone argument.
func (s *Server) chooseClone(ctx context.Context, t hostTarget, repo, given string) cloneChoice {
	if t.Host == "" || repo == "" {
		if given != "" {
			return cloneChoice{refusal: agentDispatchCloneHostOnly}
		}
		return cloneChoice{}
	}
	cl, ok, why := s.hostClones(ctx, t.Host, t.Runtime, repo)
	switch {
	case why != "":
		return cloneChoice{refusal: why}
	case !ok && given != "":
		return cloneChoice{refusal: fmt.Sprintf(agentDispatchCloneOldHost, t.Host)}
	case !ok:
		return cloneChoice{}
	case given != "":
		// The server checks the host's list too; the host checks again.
		if !slices.ContainsFunc(cl, func(c mirror.AgentClone) bool { return c.Path == given }) {
			return cloneChoice{refusal: fmt.Sprintf(agentDispatchCloneNotListed, given, t.Host, repo)}
		}
		return cloneChoice{clone: given}
	}
	hint, choices := mirror.ChooseClone(cl)
	if hint != "" {
		return cloneChoice{clone: hint}
	}
	if len(choices) > 0 {
		return cloneChoice{blank: true, choices: choices}
	}
	return cloneChoice{} // no clone at all: the host refuses the spawn with its own sentence
}

// cloneLabel is a clone as its card offers it.
func cloneLabel(c mirror.AgentClone) string {
	if c.Live {
		return c.Path + cloneLiveSuffix
	}
	return c.Path
}

// withCloneBlank is blanks with its approval card replaced by the clone card,
// when clone (the call's clone is the owner's to choose): the clone card IS
// the approval card, showing the whole request.
func withCloneBlank(blanks []string, clone bool) []string {
	if !clone {
		return blanks
	}
	out := slices.Clone(blanks)
	for i, b := range out {
		if b == blankApprove {
			out[i] = blankClone
		}
	}
	return out
}

// cloneDependsOn reports whether a hold's clone was settled AFTER it: a hold
// whose host, repository or clone is still a blank was filed with no clone
// (the clone is chosen from that host's clones of that repository), so its
// hash is compared with the call's clone cleared.
func cloneDependsOn(blanks []string) bool {
	return slices.Contains(blanks, "host") || slices.Contains(blanks, blankRepo) || slices.Contains(blanks, blankClone)
}

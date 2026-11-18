package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
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
//   - Before anything is held or reserved — ONCE per request_key (below) —
//     muster asks a host that advertises it (HostView.AgentSpawnClones) for
//     its clones of the repository (agent-clones). The host answers with ITS
//     OWN list, each clone marked live when one of its listed windows works in
//     it. That answer is stored on the hold and the reservation.
//   - mirror.ChooseClone decides from the stored list: the only clone, or the
//     only live clone, is the request's clone (shown on the approval card,
//     carried by the spawn; the hash binds only a clone the CHIEF passed);
//     otherwise the owner
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
// list muster cannot carry (invalid) likewise decides nothing; one that is not
// answered NOW refuses the call, retryable with the same key, before anything
// is held.
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
	agentDispatchCloneOldHost   = "host %q does not offer a clone list muster can use (an older muster-mirror, or a clone path muster cannot carry), so it takes no clone: omit clone"
	agentDispatchHeldChoseClone = "your owner chose the clone %s for this dispatch: call agent_dispatch with clone set to exactly that"
)

// mirrorCloneLister is the mirror plugin's clone list (*mirror.Service).
type mirrorCloneLister interface {
	AgentClones(ctx context.Context, host, repo string) ([]mirror.AgentClone, error)
}

// THE CLONE IS DECIDED ONCE PER request_key (audit D1). The host's list is
// read on the FIRST call that knows the host and the repository; what it said
// is stored — on the hold that call files (heldDetail.CloneDecision) and on the
// reservation that starts (detail.clone_decision) — and every later call with
// that key takes it from there and asks the host nothing (F3). So a retry, a
// "waiting" call, a replay and a completion never re-derive the clone: a host
// that is offline or slow by then, or a live window that moved, cannot turn
// the same request into another one (F1: the same key was refused as reused,
// and following "use a new key" started a SECOND agent in the other clone),
// nor make an approved hold read as changed (F2).
//
// 🔴 THE REQUEST HASH BINDS ONLY THE CHIEF'S clone ARGUMENT, never the clone
// derived from the host's list: the hash is "what the chief sent", and a
// refusal that tells it to use a new key must mean the chief changed something.

// cloneDecision is what one request's host said about its clones: Listed and
// List when it listed them (List may be empty: no clone at all), not Listed
// for a host that does not list them (an older muster-mirror) or whose list
// muster cannot carry. Host and Repo are what it was decided for.
type cloneDecision struct {
	Decided bool                `json:"decided"`
	Host    string              `json:"host"`
	Repo    string              `json:"repo"`
	Listed  bool                `json:"listed,omitempty"`
	List    []mirror.AgentClone `json:"list,omitempty"`
}

// detailCloneDecision is the reservation detail key a host start's clone
// decision is stored under.
const detailCloneDecision = "clone_decision"

// isFor reports whether d was decided for host and repo.
func (d cloneDecision) isFor(host, repo string) bool {
	return d.Decided && d.Host == host && d.Repo == repo
}

// agentDispatchClonesUnread: the host lists its clones but did not answer now.
// Nothing was held or reserved, so the same call again is clean.
const agentDispatchClonesUnread = "host %q's clone list can't be read right now: nothing was held or started; retry with the same request_key"

// decideClone asks host for its clones of repo (canonical owner/name): the
// decision, or the refusal — the host's own refusal to list (the repository
// is not on its allow-list, spawning is off), or a list that cannot be read
// now (agentDispatchClonesUnread). A host that does not advertise
// AgentSpawnClones is asked nothing.
func (s *Server) decideClone(ctx context.Context, host, runtime, repo string) (cloneDecision, string) {
	d := cloneDecision{Decided: true, Host: host, Repo: repo}
	v, listed := s.hostView(ctx, host)
	if !listed || !v.AgentSpawnClones {
		return d, ""
	}
	l, can := s.ext.Mirror.(mirrorCloneLister)
	if !can {
		return d, ""
	}
	cl, err := l.AgentClones(ctx, host, repo)
	var r *mirror.ClonesRefusedError
	switch {
	case err == nil:
		d.Listed, d.List = true, cl
		return d, ""
	case errors.As(err, &r):
		return cloneDecision{}, spawnRefusalSentence(r.Code, host, firstNonBlank(runtime, "host"), repo, "", r.Detail)
	case errors.Is(err, mirror.ErrClonesUnsupported), errors.Is(err, mirror.ErrClonesInvalid):
		// Not a list muster can decide from (the host stopped advertising it, or
		// it holds a path muster cannot carry): no clone, the host's own rule.
		s.logger.Printf("agent_dispatch: host %s's clones of %s decide nothing: %v", host, repo, err)
		return d, ""
	}
	s.logger.Printf("agent_dispatch: list host %s's clones of %s: %v", host, repo, err)
	return cloneDecision{}, fmt.Sprintf(agentDispatchClonesUnread, host)
}

// storedCloneDecision is the decision a request_key already holds for host
// and repo: its live reservation's (the one a call with the key replays),
// else its newest card hold's. ok false when there is none: the caller asks
// the host.
func (s *Server) storedCloneDecision(ctx context.Context, key, host, repo string) (cloneDecision, bool, error) {
	st := s.hostDispatches()
	if st == nil || key == "" {
		return cloneDecision{}, false, nil
	}
	raw, found, err := st.KeyedReservationDetail(ctx, key)
	if err != nil {
		return cloneDecision{}, false, err
	}
	if found {
		var r struct {
			D cloneDecision `json:"clone_decision"`
		}
		_ = json.Unmarshal(raw, &r)
		if r.D.isFor(host, repo) {
			return r.D, true, nil
		}
		// A reservation with no decision for this host and repository — one
		// filed before decisions were stored, or another target: the call
		// replays it or is refused as reused, and starts nothing either way.
		// Asking the host would decide nothing that call can use.
		return cloneDecision{Decided: true, Host: host, Repo: repo}, true, nil
	}
	h, err := st.CardHoldByKey(ctx, key)
	switch {
	case errors.Is(err, plugins.ErrHoldNotFound):
		return cloneDecision{}, false, nil
	case err != nil:
		return cloneDecision{}, false, err
	}
	var d heldDetail
	if json.Unmarshal(h.Detail, &d) == nil && d.CloneDecision != nil && d.CloneDecision.isFor(host, repo) {
		return *d.CloneDecision, true, nil
	}
	return cloneDecision{}, false, nil
}

// cloneChoice is a host call's clone, settled before anything is held: clone
// (the chief's, or the one decision's rule names), whether the owner must
// choose it (blank, with choices), the decision it came from, or the refusal.
type cloneChoice struct {
	clone    string
	blank    bool
	choices  []mirror.AgentClone
	decision cloneDecision
	refusal  string
}

// chooseClone settles the clone of a call under request_key key to host for
// repo (canonical; "" when the repository is itself still a blank, which
// decides nothing). given is the call's own clone argument. The decision is
// the key's stored one when it has one, else the host's answer now.
func (s *Server) chooseClone(ctx context.Context, t hostTarget, repo, given, key string) cloneChoice {
	if t.Host == "" || repo == "" {
		if given != "" {
			return cloneChoice{refusal: agentDispatchCloneHostOnly}
		}
		return cloneChoice{}
	}
	d, ok, err := s.storedCloneDecision(ctx, key, t.Host, repo)
	if err != nil {
		s.logger.Printf("agent_dispatch: read the clone decision of %q: %v", key, err)
		return cloneChoice{refusal: "the dispatch's clone could not be read; try again with the same request_key"}
	}
	if !ok {
		var why string
		if d, why = s.decideClone(ctx, t.Host, t.Runtime, repo); why != "" {
			return cloneChoice{refusal: why}
		}
	}
	c := cloneChoice{decision: d}
	switch {
	case given != "" && !d.Listed:
		c.refusal = fmt.Sprintf(agentDispatchCloneOldHost, t.Host)
	case given != "":
		// The server checks the host's list too; the host checks again.
		if !slices.ContainsFunc(d.List, func(x mirror.AgentClone) bool { return x.Path == given }) {
			c.refusal = fmt.Sprintf(agentDispatchCloneNotListed, given, t.Host, repo)
		}
		c.clone = given
	default:
		c.clone, c.choices = mirror.ChooseClone(d.List)
		c.blank = len(c.choices) > 0
	}
	return c
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

package mirror

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// WHICH CLONE A HOST SPAWN IS CUT FROM (agent-clones / agent-clones-list, and
// AgentSpawnReq.Clone): the wire and the server half. The host half is
// internal/mirrorhost/clones.go.
//
// 🔴 WHY (measured in production). A host had TWO main working trees of one
// repository under its spawn roots — the one the owner's live mirrored
// sessions work in, and an older standalone clone — and refused every spawn
// of it ("no_repo: repo <r> has 2 clones under this host's roots"). Nothing on
// the wire could say which, so the chief was stuck. Operator decision: "Prefer
// the live sessions' clone".
//
//   - agent-clones (muster → host) names a repository. The host answers
//     agent-clones-list with ITS OWN clones of it under its spawn roots — the
//     same enumeration a spawn resolves against — each marked Live when one of
//     the host's own listed windows (not a muster-started agent's) works in a
//     checkout whose main working tree is that clone (a worktree resolves to
//     its main tree through its commondir: mirrorhost's repo.go, the rule the
//     repo chip and github_repo are read by).
//   - muster then decides with ChooseClone — THE one rule: one clone, or one
//     live clone among several, is the hint; otherwise the owner chooses on
//     the approval card, among exactly the clones the host listed.
//   - the spawn carries the choice as AgentSpawnReq.Clone, a HINT: the host
//     re-enumerates its clones and uses the hint only when it is one of them
//     (SpawnBadClone otherwise). Its allow-list and roots checks are
//     unchanged, and nothing muster sends can name a directory the host did not
//     itself find.
//
// 🔴 OPT-IN BY CAPABILITY, BACKWARD-COMPATIBLE BOTH WAYS: a host advertises
// Msg.AgentSpawnClones beside AgentSpawn; muster asks no other host and sends
// no other host a hint. An OLDER HOST never advertises it — and one sent a hint
// anyway ignores the field (JSON) and resolves as it always did: its single
// clone, or no_repo for several. It never picks one on a guess.

const (
	// TypeAgentClones (muster → host): list the host's clones of
	// Msg.CloneRepo. ID is the request id (the stream id space).
	TypeAgentClones = "agent-clones"
	// TypeAgentClonesList (host → muster): its answer, Msg.Clones, or
	// Msg.Refusal (a Spawn* code) and Msg.Error.
	TypeAgentClonesList = "agent-clones-list"
)

// AgentClone is one of a host's clones of a repository (Msg.Clones): Path is
// its MAIN working tree, as the host resolved it (realpath); Live, one of the
// host's own listed windows works in it.
type AgentClone struct {
	Path string `json:"path"`
	Live bool   `json:"live,omitempty"`
}

const (
	// MaxClones bounds an agent-clones-list (the card offers each, beside its
	// "Don't start", within attention's MaxOptions).
	MaxClones = 8
	// MaxClonePath bounds a clone's path, in bytes (it is a card option, with
	// a short suffix, within attention's MaxOptionRunes).
	MaxClonePath = 160
)

// ErrClonesInvalid: a host's list held a path muster cannot carry (or more
// than MaxClones). muster then decides nothing from it: no hint, no card —
// the host's own rule applies (no_repo for several), never a choice among
// the part of the list that happened to fit.
var ErrClonesInvalid = errors.New("mirror: the host's clone list holds a path muster cannot carry")

// ValidClonePath reports whether p can be a clone's path on the wire: absolute,
// already clean, at most MaxClonePath bytes, no control or non-printing
// characters (it is shown on a card and goes into a host's git argv only
// after the host found it itself).
func ValidClonePath(p string) bool {
	if p == "" || len(p) > MaxClonePath || !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return false
	}
	return !strings.ContainsFunc(p, func(r rune) bool { return !unicode.IsPrint(r) })
}

// NormalizeClones is a host's answer as muster takes it: each path once (the
// first kept), or ErrClonesInvalid when any path is invalid or there are more
// than MaxClones — the WHOLE list refused, so no choice is ever made among a
// part of it.
func NormalizeClones(in []AgentClone) ([]AgentClone, error) {
	var out []AgentClone
	seen := map[string]bool{}
	for _, c := range in {
		if !ValidClonePath(c.Path) {
			return nil, ErrClonesInvalid
		}
		if seen[c.Path] {
			continue
		}
		seen[c.Path] = true
		out = append(out, c)
	}
	if len(out) > MaxClones {
		return nil, ErrClonesInvalid
	}
	return out, nil
}

// ChooseClone is THE rule for which clone a spawn is cut from, given the
// host's own list: the only clone, or the only LIVE clone among several, is
// the hint; with several and none or several live, hint is "" and choices is
// the clones for the owner to choose among — live ones first, each group in
// the host's order. No clones: "" and none (the host refuses the spawn itself).
func ChooseClone(clones []AgentClone) (hint string, choices []AgentClone) {
	switch len(clones) {
	case 0:
		return "", nil
	case 1:
		return clones[0].Path, nil
	}
	var live, rest []AgentClone
	for _, c := range clones {
		if c.Live {
			live = append(live, c)
		} else {
			rest = append(rest, c)
		}
	}
	if len(live) == 1 {
		return live[0].Path, nil
	}
	return "", append(live, rest...)
}

var (
	// ErrClonesUnsupported: the host did not advertise AgentSpawnClones.
	ErrClonesUnsupported = errors.New("mirror: this host's muster-mirror does not list its clones")
	// ErrClonesUnanswered: the host did not answer in time.
	ErrClonesUnanswered = errors.New("mirror: the host did not list its clones in time")
)

// ClonesRefusedError is a host's refusal to list (a Spawn* code: the
// repository is not on its allow-list, spawning is off).
type ClonesRefusedError struct{ Code, Detail string }

func (e *ClonesRefusedError) Error() string {
	return "mirror: the host refused to list its clones (" + e.Code + "): " + e.Detail
}

// clonesWait is the server's wait for agent-clones-list: the host runs two git
// reads per entry of its spawn roots.
const clonesWait = 20 * time.Second

// clonesWaitOverride, when set (tests), replaces clonesWait.
var clonesWaitOverride time.Duration

type clonesAnswer struct {
	clones []AgentClone
	err    error
}

// AgentClones asks owner's host for its clones of repo and waits for the
// answer (ClonesReplyResult's). It starts and changes nothing on the host.
func (h *Hub) AgentClones(ctx context.Context, owner int64, host, repo string) (cl []AgentClone, err error) {
	ctx, done := hubOp(ctx, "agent-clones")
	defer func() { done(err) }()
	h.mu.Lock()
	hc := h.hosts[hostKey{owner, host}]
	h.mu.Unlock()
	if hc == nil {
		return nil, fmt.Errorf("%w (host %q)", ErrHostOffline, host)
	}
	if !hc.interactive || !hc.agentSpawnClones {
		return nil, fmt.Errorf("%w (host %q)", ErrClonesUnsupported, host)
	}
	hc.mu.Lock()
	hc.next++
	id := hc.next
	ch := make(chan clonesAnswer, 1)
	hc.cloneLists[id] = ch
	hc.mu.Unlock()
	defer func() {
		hc.mu.Lock()
		delete(hc.cloneLists, id)
		hc.mu.Unlock()
	}()
	if err := writeJSON(ctx, hc.ws, Msg{Type: TypeAgentClones, ID: id, CloneRepo: repo}); err != nil {
		return nil, fmt.Errorf("mirror: send the clone list request to the host: %w", err)
	}
	wait := clonesWait
	if clonesWaitOverride > 0 {
		wait = clonesWaitOverride
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case a := <-ch:
		return a.clones, a.err
	case <-ctx.Done():
		return nil, ErrClonesUnanswered
	case <-t.C:
		return nil, ErrClonesUnanswered
	}
}

// ClonesReplyResult reads an agent-clones-list frame: the clones, or the refusal.
func ClonesReplyResult(m Msg) ([]AgentClone, error) {
	if m.Refusal != "" || m.Error != "" {
		code := m.Refusal
		if code == "" {
			code = SpawnFailed
		}
		return nil, &ClonesRefusedError{Code: code, Detail: m.Error}
	}
	return NormalizeClones(m.Clones)
}

// agentClonesListed hands the host's answer to the request waiting on m.ID, if any.
func (h *Hub) agentClonesListed(hc *hostConn, m Msg) {
	cl, err := ClonesReplyResult(m)
	hc.mu.Lock()
	ch := hc.cloneLists[m.ID]
	delete(hc.cloneLists, m.ID)
	hc.mu.Unlock()
	if ch != nil {
		ch <- clonesAnswer{clones: cl, err: err}
	}
}

// AgentClones asks one of the viewing principal's hosts for its clones of
// repo. See Hub.AgentClones.
func (s *Service) AgentClones(ctx context.Context, host, repo string) ([]AgentClone, error) {
	o, err := owner(ctx)
	if err != nil {
		return nil, err
	}
	return s.hub.AgentClones(ctx, o, host, repo)
}

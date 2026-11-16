package mirror

import (
	"regexp"
	"slices"
	"strings"
)

// WHAT A HOST CAN START FOR AN OFFICER NOW (officers redesign O-B): the host's
// spawn readiness, advertised in its hello (Msg.SpawnReady) and again whenever
// it changes (TypeSpawnReady), so muster can tell — before it asks — whether a
// host officer's start would land, and say why not:
//
//   - its -agent-spawn-repos allow-list (Repos) and whether it starts an
//     officer with no repository in a scratch directory (Scratch);
//   - whether claude resolves on the tmux server's PATH, or -claude-path
//     (Claude, ClaudeDetail);
//   - whether that claude is logged in (Login): Claude Code's own `claude auth
//     status`, read for its yes/no only — never its account, e-mail or token.
//
// 🔴 A HINT, NEVER A GATE ON THE HOST. The host still enforces its own
// allow-list, rate limit and audit log on every spawn (a compromised server
// gains nothing by reading this). muster uses it to choose a host for a
// "runtime": "auto" officer, to refuse a start it knows the host would refuse,
// in `muster doctor`, and to name the likely cause of an instance that never
// claims its item.
//
// BACKWARD-COMPATIBLE BOTH WAYS: an older muster ignores the field and the
// message type; an older host never sends them, and muster reads a nil
// readiness as "unknown" — it qualifies as before.

// TypeSpawnReady (host → muster): the host's spawn readiness changed
// (Msg.SpawnReady): sent only by a host that advertised one in its hello.
const TypeSpawnReady = "spawn-ready"

// What SpawnReadiness.Claude says about the claude binary.
const (
	// ClaudeFound: claude resolves (on the tmux server's PATH, or -claude-path).
	ClaudeFound = "found"
	// ClaudeMissing: it does not; ClaudeDetail says why.
	ClaudeMissing = "missing"
)

// What SpawnReadiness.Login says about Claude Code's login.
const (
	LoginYes = "yes"
	LoginNo  = "no"
	// LoginUnknown: the check could not answer (an older claude with no
	// `auth status`, a timeout, an answer that does not parse).
	LoginUnknown = "unknown"
)

// Bounds of a readiness as muster keeps it (CleanSpawnReady).
const (
	MaxReadyRepos  = 200
	MaxReadyDetail = 200
)

// SpawnReadiness is a host's spawn readiness (Msg.SpawnReady).
type SpawnReadiness struct {
	// Unavailable is why the host starts nothing at all now ("" when it can):
	// its -agent-spawn preconditions (not interactive, spawn off, no git …).
	Unavailable string `json:"unavailable,omitempty"`
	// Repos is the -agent-spawn-repos allow-list: lower-cased <owner>/<repo>
	// and <owner>/* entries (SpawnRepoAllowed reads it).
	Repos []string `json:"repos"`
	// Scratch: the host starts an OFFICER instance that has no repository in
	// a private scratch directory of its own (-agent-spawn-scratch).
	Scratch bool `json:"scratch,omitempty"`
	// Claude is ClaudeFound or ClaudeMissing; ClaudeDetail why it is missing.
	Claude       string `json:"claude"`
	ClaudeDetail string `json:"claudeDetail,omitempty"`
	// Login is LoginYes, LoginNo or LoginUnknown.
	Login string `json:"login"`
}

// readyRepoRE is an allow-list entry's shape as muster keeps it: an
// <owner>/<repo> or <owner>/* (the host's ParseAgentSpawnRepos wrote it).
var readyRepoRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,38}/(\*|[a-z0-9._-]{1,100})$`)

// CleanSpawnReady is r as muster keeps it: nil stays nil; an entry of Repos
// that is not an allow-list entry is dropped (and the list bounded); Claude
// and Login outside their values read as missing and unknown; the free text
// is bounded. A host's word, kept only as a hint.
func CleanSpawnReady(r *SpawnReadiness) *SpawnReadiness {
	if r == nil {
		return nil
	}
	out := SpawnReadiness{Scratch: r.Scratch, Unavailable: capText(r.Unavailable, MaxReadyDetail), Repos: []string{},
		Claude: r.Claude, ClaudeDetail: capText(r.ClaudeDetail, MaxReadyDetail), Login: r.Login}
	for _, e := range r.Repos {
		e = strings.ToLower(strings.TrimSpace(e))
		if len(out.Repos) < MaxReadyRepos && readyRepoRE.MatchString(e) && !strings.Contains(e, "..") && !slices.Contains(out.Repos, e) {
			out.Repos = append(out.Repos, e)
		}
	}
	if out.Claude != ClaudeFound {
		out.Claude = ClaudeMissing
	}
	switch out.Login {
	case LoginYes, LoginNo:
	default:
		out.Login = LoginUnknown
	}
	return &out
}

// capText is s, trimmed, at most n bytes on a rune boundary.
func capText(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// SpawnRepoAllowed reports whether repo (an <owner>/<repo>) is on allow-list
// list: its exact lower-cased name, or the entry "<its owner>/*" — built from
// repo's own owner and compared for EQUALITY, never as a prefix, so
// "acme-evil/x" does not match "acme/*". THE one rule: the host's own check of
// every spawn (internal/mirrorhost) and muster's reading of a readiness.
func SpawnRepoAllowed(list []string, repo string) bool {
	r := strings.ToLower(repo)
	owner, _, _ := strings.Cut(r, "/")
	return slices.Contains(list, r) || slices.Contains(list, owner+"/*")
}

// clone is a copy of r (nil for nil): a HostView never shares the hub's.
func (r *SpawnReadiness) clone() *SpawnReadiness {
	if r == nil {
		return nil
	}
	c := *r
	c.Repos = slices.Clone(r.Repos)
	return &c
}

// Same reports whether r and o say the same thing (nil equals nil only).
func (r *SpawnReadiness) Same(o *SpawnReadiness) bool {
	if r == nil || o == nil {
		return r == nil && o == nil
	}
	return r.Unavailable == o.Unavailable && slices.Equal(r.Repos, o.Repos) && r.Scratch == o.Scratch &&
		r.Claude == o.Claude && r.ClaudeDetail == o.ClaudeDetail && r.Login == o.Login
}

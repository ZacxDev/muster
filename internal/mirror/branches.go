package mirror

import (
	"regexp"
	"strings"
)

// THE BRANCH OF A WINDOW: the git branch a mirrored window's working directory
// has checked out, as its host reads it from the repository's HEAD
// (internal/mirrorhost's branch.go). The chief's session hint and focus_search
// list an online session whose branch a message names (internal/api's
// branchHints) — before, or without, any hook reporting a push of it.
//
// 🔴 IT TRAVELS ON A MESSAGE OF ITS OWN (TypeBranches), NOT ON Window, for the
// reason the repo names do (repos.go): the window list's worst case measures
// 99.8% of MaxFrame (TestTheLargestWindowListFitsOneFrame), and even the
// field's name per window (512 × `,"branch":""`) does not fit the 2 KiB left.
// The host sends the WHOLE map of window id → branch after its window list,
// whenever it changes (an empty one clears muster's), bounded by MaxWindows
// entries of at most MaxBranch bytes: TestTheLargestBranchesMessageFitsOneFrame
// measures its worst case.
//
// 🔴 A BRANCH IS NEVER CUT. A cut name is a DIFFERENT branch's name, which a
// message could then match: a branch outside ValidBranch — longer than
// MaxBranch, or holding a byte the grammar refuses — is dropped whole, on the
// host before it is sent and again on muster when it arrives.
//
// 🔴 UNTRUSTED. The name is a host's words (a branch is whatever someone
// called it): ValidBranch keeps it to a narrow grammar, and every surface that
// shows it fences and sanitizes it like the repo name.
//
// 🔴 NO VERSION BUMP, BACKWARD-COMPATIBLE BOTH WAYS: an older muster's read
// loop has no case for the type and drops the message; an older host never
// sends one, and its windows simply have no branch — they are found by a push
// row (session_prs) as before.

// TypeBranches is the host → muster message carrying Msg.Branches.
const TypeBranches = "branches"

// MaxBranch bounds one branch name, in bytes. A longer one is not sent at all
// (never cut).
const MaxBranch = 255

// branchPattern is a branch name muster keeps: git's own grammar is wider, and
// a name outside this simply names nothing. Every byte is one JSON writes as
// itself (none of '<', '>', '&', '"' or '\\').
var branchPattern = regexp.MustCompile(`^[A-Za-z0-9._/+-]{1,255}$`)

// ValidBranch reports whether s is a branch name muster keeps — on the wire
// (TypeBranches) and in session_prs (internal/prroute's ValidRef is this rule):
// branchPattern (so at most MaxBranch bytes), and no "..", leading '-' or
// trailing '/' (none of which git accepts as a branch).
func ValidBranch(s string) bool {
	return branchPattern.MatchString(s) && !strings.Contains(s, "..") && !strings.HasPrefix(s, "-") && !strings.HasSuffix(s, "/")
}

// NormalizeBranches is in restricted to windows (a host cannot name the branch
// of a window it does not list), each a ValidBranch (any other is dropped,
// never cut); nil when nothing is left. Both halves apply it: the host to
// what it sends, muster to what it keeps. windows must be NormalizeWindows'
// output, so the result has at most MaxWindows entries.
func NormalizeBranches(in map[string]string, windows []Window) map[string]string {
	if len(in) == 0 {
		return nil
	}
	var out map[string]string
	for _, w := range windows {
		v, ok := in[w.ID]
		if !ok || !ValidBranch(v) {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[w.ID] = v
	}
	return out
}

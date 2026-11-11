package mirror

import (
	"strings"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/github"
)

// THE REPO CHIP: which repository a mirrored window — a shell or a Claude Code
// session alike — works in, as a SHORT NAME: the top-level directory's
// basename ("example-app"), or, outside a repository, the working
// directory's own basename. The card shows it as a chip and the Agents filter
// finds the card by it (`repo:`).
//
// 🔴 IT TRAVELS ON A MESSAGE OF ITS OWN (TypeRepos), NOT ON Window. The window
// list is ONE message, and its worst case already measures 98% of MaxFrame
// (1,030,215 of 1,048,576 bytes: TestTheLargestWindowListFitsOneFrame) — 18 KiB
// of room, which a name per window does not fit at any useful length (512 ×
// `,"repo":""` alone is 5 KiB before the name). So the names ride a `repos`
// message the host sends after its window list, whenever they change: a whole
// map of window id → name each time (an empty one clears them), bounded by
// MaxWindows entries of at most MaxRepoName bytes — its own worst case is
// measured by TestTheLargestReposMessageFitsOneFrame, far inside a frame.
//
// 🔴 A NAME, NEVER A PATH. CleanRepoName refuses anything holding a '/', so a
// host cannot put its directory layout on muster's page.
//
// 🔴 NO VERSION BUMP, BACKWARD-COMPATIBLE BOTH WAYS: an older muster's read
// loop has no case for the type and drops the message; an older host never
// sends one, and its cards simply carry no chip.

// TypeRepos is the host → muster message carrying Msg.Repos.
const TypeRepos = "repos"

// MaxRepoName bounds one repo name, in bytes (cut on a rune boundary).
const MaxRepoName = 64

// CleanRepoName is name as the wire carries it: invalid UTF-8 dropped, control
// and format characters turned to spaces or removed and whitespace collapsed
// (the rule every other host-sent line follows), at most MaxRepoName bytes;
// "" when nothing is left, or when it holds a '/' (a path, not a name) or is
// "." or "..".
func CleanRepoName(name string) string {
	s := cleanLine(name, MaxRepoName)
	if s == "" || s == "." || s == ".." || strings.ContainsRune(s, '/') {
		return ""
	}
	if len(s) > MaxRepoName {
		cut := MaxRepoName
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = strings.TrimRight(s[:cut], " ")
	}
	return s
}

// THE GITHUB REPOSITORY BESIDE THE NAME (Msg.GitHubRepos): the <owner>/<name>
// the window's checkout's origin names, on the same repos message. The NAME
// stays the folder's (the chip, `repo:`); the GitHub repository is what the
// chief is told to dispatch with, because a folder can be named differently
// from its repository. 🔴 A HOST'S WORD, NOT AN AUTHORITY: muster checks it
// against the owner's connected repositories (connectedRepoName) before
// anything uses it, like any repository the chief names. Its worst case on the
// wire (MaxWindows × github.ValidRepoName's 140 bytes) is measured beside the
// names' by TestTheLargestReposMessageFitsOneFrame.

// NormalizeGitHubRepos is in restricted to windows (NormalizeRepos' rule),
// each value a GitHub owner/name (github.ValidRepoName) lowercased; an entry
// that is not is dropped; nil when nothing is left.
func NormalizeGitHubRepos(in map[string]string, windows []Window) map[string]string {
	if len(in) == 0 {
		return nil
	}
	var out map[string]string
	for _, w := range windows {
		v, ok := in[w.ID]
		if !ok {
			continue
		}
		if v = strings.ToLower(strings.TrimSpace(v)); !github.ValidRepoName(v) {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[w.ID] = v
	}
	return out
}

// NormalizeRepos is in restricted to windows (a host cannot name the repo of a
// window it does not list), each name through CleanRepoName (an entry left
// empty is dropped); nil when nothing is left. Both halves apply it: the host
// to what it sends, muster to what it keeps. windows must be NormalizeWindows'
// output, so the result has at most MaxWindows entries.
func NormalizeRepos(in map[string]string, windows []Window) map[string]string {
	if len(in) == 0 {
		return nil
	}
	var out map[string]string
	for _, w := range windows {
		v, ok := in[w.ID]
		if !ok {
			continue
		}
		if v = CleanRepoName(v); v == "" {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[w.ID] = v
	}
	return out
}

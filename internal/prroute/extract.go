package prroute

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

// THE AUTHORSHIP EXTRACTOR: which pull requests and branches a session made,
// read from its own transcript's Bash calls and their results.
//
// 🔴 AUTHORSHIP, NEVER MENTION. A signal needs BOTH halves: a Bash call whose
// command runs `gh pr create` (or `git push`) as a command — at the start of
// the line or after a shell separator, never as an argument — AND that call's
// own successful result naming the pull request (or the pushed repository and
// branch). A session that views, lists, reads, fetches, quotes or talks about
// a pull request writes neither half, so an auditing or reading session never
// takes the author's mail. The result is the proof: `gh pr create` prints the
// new pull request's URL, `git push` its "To <remote>" and ref lines.
//
// ⚠ WHAT IT CANNOT SEE, measured against cctranscript's caps: a tool result is
// cut to its first cctranscript.MaxOutput bytes, so a push whose hooks print
// more than that before git's own "To" line (a long pre-push gate) yields no
// signal; and a command run through a wrapper that is not a shell separator
// (`timeout 60 git push`, `make push`) is not read as one.

// Kind is how a session authored a pull request.
type Kind string

const (
	// KindPRCreate: a `gh pr create` the session ran, whose output named the
	// new pull request's URL.
	KindPRCreate Kind = "gh-pr-create"
	// KindPush: a `git push` the session ran, whose output named the remote
	// repository and a branch it updated.
	KindPush Kind = "git-push"
)

// Signal is one authorship the extractor saw: (Repo, PR) for KindPRCreate,
// (Repo, HeadRef) for KindPush. Repo is lower-cased.
type Signal struct {
	Kind    Kind
	Repo    string
	PR      int64
	HeadRef string
}

// maxPending bounds the calls an Extractor remembers while their results are
// outstanding (Claude Code issues parallel calls, and a call whose session
// died never gets one).
const maxPending = 64

// Extractor reads one session's transcript events, in order, across as many
// Feed calls as it takes: a call and its result can arrive in different
// batches, or different scans.
type Extractor struct {
	pending map[string]Kind
	order   []string
}

// command starts at a line start or after a shell separator (; & | ( { or $( ),
// past any leading VAR=value assignments.
const commandStart = `(?m)(?:^|[;&|({]|\$\()\s*(?:[A-Za-z_][A-Za-z0-9_]*=[^\s;&|]*\s+)*`

var (
	ghPRCreate = regexp.MustCompile(commandStart + `gh\s+pr\s+create(?:\s|$)`)
	// git push, past git's global options (-C <path>, -c <k=v>, --flags).
	gitPush = regexp.MustCompile(commandStart + `git(?:\s+-[Cc]\s+\S+|\s+--\S+)*\s+push(?:\s|$)`)

	prURLPattern = regexp.MustCompile(`https://github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)/pull/([0-9]+)\b`)
	// pushTo is git push's "To <remote>" line for a GitHub remote: scp-like,
	// https or ssh, with or without ".git".
	pushTo = regexp.MustCompile(`^To\s+(?:https://(?:[^@/\s]+@)?|ssh://git@|git@)?github\.com[:/]([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+?)(?:\.git)?/?\s*$`)
	// pushRef is one ref line of git push's porcelain-like summary: its flag
	// (' ' fast-forward, '+' forced, '*' new), its summary and "src -> dst".
	// '!' (rejected), '-' (deleted) and '=' (up to date) are not updates.
	pushRef = regexp.MustCompile(`^ ?([ +*])\s*(\[new branch\]|[0-9a-f]{4,40}\.\.\.?[0-9a-f]{4,40})\s+\S+\s+->\s+(\S+)`)
)

// Feed reads evs and returns the authorship signals they complete.
func (x *Extractor) Feed(evs []cctranscript.Event) []Signal {
	var out []Signal
	for _, ev := range evs {
		switch ev.Kind {
		case cctranscript.KindToolCall:
			if ev.Tool != "Bash" || ev.ToolID == "" {
				continue
			}
			cmd := bashCommand(ev)
			switch {
			case ghPRCreate.MatchString(cmd):
				x.remember(ev.ToolID, KindPRCreate)
			case gitPush.MatchString(cmd):
				x.remember(ev.ToolID, KindPush)
			}
		case cctranscript.KindToolResult:
			kind, ok := x.pending[ev.ToolID]
			if !ok {
				continue
			}
			x.forget(ev.ToolID)
			if !ev.OK {
				continue // the command failed: it authored nothing
			}
			switch kind {
			case KindPRCreate:
				if s, ok := createdPR(ev.Output); ok {
					out = append(out, s)
				}
			case KindPush:
				out = append(out, pushedBranches(ev.Output)...)
			}
		}
	}
	return out
}

// Reset forgets the calls still waiting for their result (the stream started
// over on a backfill).
func (x *Extractor) Reset() { x.pending, x.order = nil, nil }

func (x *Extractor) remember(id string, k Kind) {
	if x.pending == nil {
		x.pending = map[string]Kind{}
	}
	if _, ok := x.pending[id]; !ok {
		x.order = append(x.order, id)
	}
	x.pending[id] = k
	for len(x.order) > maxPending {
		delete(x.pending, x.order[0])
		x.order = x.order[1:]
	}
}

func (x *Extractor) forget(id string) {
	delete(x.pending, id)
	for i, o := range x.order {
		if o == id {
			x.order = append(x.order[:i], x.order[i+1:]...)
			break
		}
	}
}

// bashCommand is a Bash call's command: its input's "command", or — when the
// input was cut past parsing (cctranscript.CapInput) — the call's one-line
// Target, which for Bash is the command line.
func bashCommand(ev cctranscript.Event) string {
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal([]byte(ev.Input), &in) == nil && in.Command != "" {
		return in.Command
	}
	return ev.Target
}

// createdPR is the one pull request a gh pr create's output names; false
// when it names none or more than one (a compound command's other output).
func createdPR(out string) (Signal, bool) {
	var got Signal
	for _, m := range prURLPattern.FindAllStringSubmatch(out, -1) {
		n, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil || n <= 0 {
			return Signal{}, false
		}
		s := Signal{Kind: KindPRCreate, Repo: strings.ToLower(m[1]), PR: n}
		if got.Repo != "" && got != s {
			return Signal{}, false
		}
		got = s
	}
	return got, got.Repo != ""
}

// pushedBranches is every branch a git push's output says it updated on a
// GitHub remote, in order: the ref lines under each "To" line.
func pushedBranches(out string) []Signal {
	var sigs []Signal
	repo := ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if m := pushTo.FindStringSubmatch(line); m != nil {
			repo = strings.ToLower(m[1])
			continue
		}
		if strings.HasPrefix(line, "To ") {
			repo = "" // a remote that is not GitHub's
			continue
		}
		if repo == "" {
			continue
		}
		m := pushRef.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		dst := m[3]
		if strings.HasPrefix(dst, "refs/") && !strings.HasPrefix(dst, "refs/heads/") {
			continue // a tag or another namespace
		}
		dst = strings.TrimPrefix(dst, "refs/heads/")
		if !validRef(dst) {
			continue
		}
		sigs = append(sigs, Signal{Kind: KindPush, Repo: repo, HeadRef: dst})
	}
	return sigs
}

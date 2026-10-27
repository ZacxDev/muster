// Package prroute is deterministic PULL-REQUEST → SESSION routing's pure half:
// which pull request a review mail is about (ParseMail), and the shape of an
// authorship report (Signal) — which pull request a session created, or which
// branch it pushed — as a host's PostToolUse hook sends it to POST
// /api/pr-sessions (internal/api/pr_routes.go). It holds no store and makes no
// network call. docs/plugins.md#a-pull-request-review-mail.
package prroute

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Ref is the pull request a review mail names.
type Ref struct {
	// Repo is "<owner>/<repo>", lower-cased (GitHub's names are case-insensitive).
	Repo string
	// PR is the pull request's number.
	PR int64
	// HeadSHA is the reviewed head commit (40 lower-case hex).
	HeadSHA string
	// HeadRef is the pull request's head branch, "" when the mail names none.
	HeadRef string
}

var (
	// RepoPattern is a repository as the mail and a transcript name it.
	RepoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	shaPattern  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	// refPattern is a branch name this feature stores: git's own grammar is
	// wider, and a name outside this simply matches nothing.
	refPattern = regexp.MustCompile(`^[A-Za-z0-9._/+-]{1,255}$`)
	// fenceOpen is a backtick fence's opening line (CommonMark: at most three
	// spaces of indent), its length and info string.
	fenceOpen = regexp.MustCompile("^ {0,3}(`{3,})([^`]*)$")
)

// Kind is how a session authored a pull request.
type Kind string

const (
	// KindPRCreate: the session ran `gh pr create`, which printed the new pull
	// request's URL.
	KindPRCreate Kind = "gh-pr-create"
	// KindPush: the session ran `git push`, which named the remote repository
	// and a branch it updated.
	KindPush Kind = "git-push"
)

// Signal is one authorship report: Repo and PR for KindPRCreate (HeadRef too
// when the create named its head branch), Repo and HeadRef for KindPush.
type Signal struct {
	Kind    Kind
	Repo    string
	PR      int64
	HeadRef string
}

// Valid reports whether s is a well-formed report: a known kind, a repo, a
// positive PR for a create (none for a push), a branch for a push (optional
// for a create), each in this package's grammar.
func (s Signal) Valid() bool {
	if !RepoPattern.MatchString(s.Repo) || (s.HeadRef != "" && !ValidRef(s.HeadRef)) {
		return false
	}
	switch s.Kind {
	case KindPRCreate:
		return s.PR > 0
	case KindPush:
		return s.PR == 0 && s.HeadRef != ""
	}
	return false
}

// ValidRef reports whether s is a branch name this feature stores.
func ValidRef(s string) bool { return validRef(s) }

// validRef is a stored branch name: refPattern, and no "..", leading "-" or
// trailing "/" (none of which git accepts as a branch).
func validRef(s string) bool {
	return refPattern.MatchString(s) && !strings.HasPrefix(s, "-") && !strings.HasSuffix(s, "/")
}

// ParseMail is the pull request mail body names, read from the LAST fenced
// ```json block of the body. ok is false for anything malformed — no such
// block, invalid JSON, a missing or invalid repo, pr or head_sha, or a
// head_ref that is present and not a branch name — and the caller then routes
// nothing by it.
//
// 🔴 THE LAST BLOCK DECIDES, AND AN EARLIER ONE IS NEVER A FALLBACK: the gate
// writes its machine-readable block last, and a body may quote other JSON
// before it (a finding's excerpt). A fence is matched as CommonMark matches
// it — closed only by a backtick run at least as long as the one that opened
// it — so a block the gate fenced with more backticks than any run inside it
// is read whole, and a fenced block of another kind hides what is inside it.
func ParseMail(body string) (Ref, bool) {
	raw, ok := lastJSONBlock(body)
	if !ok {
		return Ref{}, false
	}
	var in struct {
		Repo    *string          `json:"repo"`
		PR      *json.RawMessage `json:"pr"`
		HeadSHA *string          `json:"head_sha"`
		HeadRef *string          `json:"head_ref"`
	}
	if json.Unmarshal([]byte(raw), &in) != nil || in.Repo == nil || in.PR == nil || in.HeadSHA == nil {
		return Ref{}, false
	}
	var pr int64
	// A JSON integer only: "4817" (a string) and 4817.5 are refused.
	if json.Unmarshal(*in.PR, &pr) != nil || pr <= 0 || strings.ContainsAny(string(*in.PR), ".eE\"") {
		return Ref{}, false
	}
	if !RepoPattern.MatchString(*in.Repo) || !shaPattern.MatchString(*in.HeadSHA) {
		return Ref{}, false
	}
	ref := Ref{Repo: strings.ToLower(*in.Repo), PR: pr, HeadSHA: *in.HeadSHA}
	if in.HeadRef != nil {
		if !validRef(*in.HeadRef) {
			return Ref{}, false
		}
		ref.HeadRef = *in.HeadRef
	}
	return ref, true
}

// lastJSONBlock is the content of body's last CLOSED fenced block whose info
// string is "json".
func lastJSONBlock(body string) (string, bool) {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	var last string
	found := false
	for i := 0; i < len(lines); i++ {
		m := fenceOpen.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		width, info := len(m[1]), strings.TrimSpace(m[2])
		end := -1
		for j := i + 1; j < len(lines); j++ {
			if closesFence(lines[j], width) {
				end = j
				break
			}
		}
		if end < 0 {
			// Never closed: CommonMark runs it to the end of the document, so
			// nothing after it is a block either.
			break
		}
		if info == "json" {
			last, found = strings.Join(lines[i+1:end], "\n"), true
		}
		i = end
	}
	return last, found
}

// closesFence reports whether line closes a fence of width backticks: at most
// three spaces, then a run of at least width backticks, then only spaces.
func closesFence(line string, width int) bool {
	t := strings.TrimRight(line, " \t")
	indent := len(t) - len(strings.TrimLeft(t, " "))
	if indent > 3 {
		return false
	}
	t = t[indent:]
	return len(t) >= width && strings.Trim(t, "`") == ""
}

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
	return refPattern.MatchString(s) && !strings.Contains(s, "..") && !strings.HasPrefix(s, "-") && !strings.HasSuffix(s, "/")
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

// Review is what a review mail's machine block says beyond Ref: the round's
// id and its findings' short ids — what HQ's PR-review respond channel takes
// (`finding` as the short id WITH `pr`) to answer one.
type Review struct {
	Ref
	// ReviewID is the block's review_id, "" when it is absent or not in
	// reviewIDPattern.
	ReviewID string
	// Findings is the block's findings that carry a well-formed id, in the
	// block's order, at most MaxReviewFindings.
	Findings []Finding
	// Listed is how many entries the block's findings array holds, ids or
	// not: whether the round HAS findings, which Findings (well-formed ids
	// only, bounded) cannot tell. 0 for a round with none — its mail asks
	// nothing to answer finding by finding.
	Listed int
}

// Finding is one finding's id and severity, both validated: muster prints
// them OUTSIDE the untrusted fence (internal/api's hqReviewPreface), so only
// text in these grammars is kept.
type Finding struct {
	// ID is the finding's short id, "f_" and 12 lower-case hex.
	ID string
	// Severity is the finding's severity (lower-case letters), "" when absent
	// or not one.
	Severity string
}

// MaxReviewFindings bounds Review.Findings: the gate keeps a handful per lane,
// and the paste has a size cap.
const MaxReviewFindings = 50

var (
	findingIDPattern = regexp.MustCompile(`^f_[0-9a-f]{12}$`)
	reviewIDPattern  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	severityPattern  = regexp.MustCompile(`^[a-z]{1,16}$`)
)

// ParseReview is ParseMail with the round's review_id and its findings' ids:
// ok exactly when ParseMail's is (the same block, the same rules). A finding
// without a well-formed id is left out, never repaired; a review_id or
// severity outside its grammar reads as "".
func ParseReview(body string) (Review, bool) {
	ref, ok := ParseMail(body)
	if !ok {
		return Review{}, false
	}
	rv := Review{Ref: ref}
	raw, _ := lastJSONBlock(body)
	var in struct {
		ReviewID any `json:"review_id"`
		Findings []struct {
			ID       any `json:"id"`
			Severity any `json:"severity"`
		} `json:"findings"`
	}
	// A findings field of another shape leaves the review with none.
	_ = json.Unmarshal([]byte(raw), &in)
	if s, isStr := in.ReviewID.(string); isStr && reviewIDPattern.MatchString(s) {
		rv.ReviewID = s
	}
	rv.Listed = len(in.Findings)
	for _, f := range in.Findings {
		id, isStr := f.ID.(string)
		if !isStr || !findingIDPattern.MatchString(id) {
			continue
		}
		sev, _ := f.Severity.(string)
		if !severityPattern.MatchString(sev) {
			sev = ""
		}
		rv.Findings = append(rv.Findings, Finding{ID: id, Severity: sev})
		if len(rv.Findings) == MaxReviewFindings {
			break
		}
	}
	return rv, true
}

// lastJSONBlock is the content of body's last CLOSED fenced block whose info
// string is "json" — false unless only whitespace follows that block.
func lastJSONBlock(body string) (string, bool) {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	var last string
	found, lastEnd := false, -1
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
			last, found, lastEnd = strings.Join(lines[i+1:end], "\n"), true, end
		}
		i = end
	}
	// 🔴 THE BLOCK MUST END THE BODY: only whitespace may follow its closing
	// fence (the gate writes "\n{fence}\n" last). Otherwise a block forged in
	// the prose above — closed, then followed by an unclosed longer fence that
	// swallows the gate's own block — would be the last CLOSED json block.
	if found && strings.TrimSpace(strings.Join(lines[lastEnd+1:], "\n")) != "" {
		return "", false
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

// Package prroute is deterministic PULL-REQUEST → SESSION routing's pure half:
// reading which pull request a review mail is about (ParseMail), and reading,
// from a mirrored Claude Code session's transcript events, which pull requests
// and branches that session AUTHORED (Extractor). It holds no store and makes
// no network call: internal/plugins persists what the extractor finds, and
// internal/api routes a mail by it. docs/plugins.md#a-pull-request-review-mail.
package prroute

import (
	"github.com/ZacxDev/muster/internal/cctranscript"
)

// Ref is the pull request a review mail names.
type Ref struct {
	// Repo is "<owner>/<repo>", lower-cased.
	Repo string
	// PR is the pull request's number.
	PR int64
	// HeadSHA is the reviewed head commit (40 lower-case hex).
	HeadSHA string
	// HeadRef is the pull request's head branch, "" when the mail names none.
	HeadRef string
}

// ParseMail is the pull request mail body names, read from the LAST fenced
// ```json block of the body. ok is false for anything malformed.
func ParseMail(body string) (Ref, bool) {
	return Ref{}, false
}

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
// (Repo, HeadRef) for KindPush.
type Signal struct {
	Kind    Kind
	Repo    string
	PR      int64
	HeadRef string
}

// Extractor reads one session's transcript events, in order, across as many
// Feed calls as it takes.
type Extractor struct{}

// Feed reads evs and returns the authorship signals they complete.
func (x *Extractor) Feed(evs []cctranscript.Event) []Signal { return nil }

// Reset forgets the calls still waiting for their result.
func (x *Extractor) Reset() {}

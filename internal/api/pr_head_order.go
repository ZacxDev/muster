package api

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/githubapp"
)

// ---------------------------------------------------------------------------
// THE NEWEST HEAD WINS (docs/plugins.md#a-pull-request-review-mail): a review
// round supersedes another round's route only when its head commit is the
// same as or NEWER than that round's. Order is the pull request's own commit
// list (GitHub, through the App's pull-request read): a late review of an
// OLDER head supersedes nothing, so it can never hide a newer review. When
// the order cannot be read (no App, a head not among the commits — a force
// push — or an error), muster supersedes by arrival as before, and that route
// is NOT counted as handled (plugins.SupersededRouteError): its message still
// raises the "not acted on" ask.
// ---------------------------------------------------------------------------

// PRCommitsReader lists a pull request's commit shas, oldest first
// (githubapp.App.PullRequestCommits). ClosedPRs.States provides it when it
// can.
type PRCommitsReader interface {
	PullRequestCommits(ctx context.Context, repo string, n int64) ([]string, error)
}

// The production reader (cmd/muster-server wires *githubapp.App as
// ClosedPRs.States) must also list commits, or the head order is never known.
var _ PRCommitsReader = (*githubapp.App)(nil)

// Head orders (compareHeads).
const (
	headOrderUnknown = iota
	headNewerOrSame
	headOlder
)

// prCommitsTimeout bounds one commit-list read: past it the order is unknown.
const prCommitsTimeout = 5 * time.Second

// compareHeads is THE head-order rule: whether head newer (the arriving
// review's) is the same commit as, or comes after, head older in commits
// (oldest first) — headNewerOrSame — or comes before it — headOlder. Either
// head missing from the list is headOrderUnknown. Pure, so its cases are
// pinned one by one.
func compareHeads(commits []string, older, newer string) int {
	if older == "" || newer == "" {
		return headOrderUnknown
	}
	if strings.EqualFold(older, newer) {
		return headNewerOrSame
	}
	io, in := -1, -1
	for i, c := range commits {
		switch {
		case strings.EqualFold(c, older):
			io = i
		case strings.EqualFold(c, newer):
			in = i
		}
	}
	switch {
	case io < 0 || in < 0:
		return headOrderUnknown
	case in > io:
		return headNewerOrSame
	}
	return headOlder
}

// prCommits reads pull request pr's ("owner/repo#n") commits, nil when they
// cannot be read (the order is then unknown).
func (s *Server) prCommits(ctx context.Context, pr string) []string {
	if s.ext.ClosedPRs == nil {
		return nil
	}
	r, ok := s.ext.ClosedPRs.States.(PRCommitsReader)
	if !ok {
		return nil
	}
	repo, num, ok := strings.Cut(pr, "#")
	n, err := strconv.ParseInt(num, 10, 64)
	if !ok || err != nil || n <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, prCommitsTimeout)
	defer cancel()
	commits, err := r.PullRequestCommits(ctx, repo, n)
	if err != nil {
		s.logger.Printf("routes: %s: read its commits for the head order: %v (the order is unknown)", pr, err)
		return nil
	}
	return commits
}

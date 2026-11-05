package officership

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
)

// Orphan is an officer branch no pull request was ever opened from (a refused
// pull request leaves one), older than officers.OrphanBranchAge.
type Orphan struct {
	Repo   string    `json:"repo"`
	Branch string    `json:"branch"`
	SHA    string    `json:"sha"`
	Since  time.Time `json:"since"`
}

// OrphanBranches is the daily orphan report of a draft officer: every branch
// under each ship repository's prefix and the officer's slug that has no pull
// request in any state, whose head commit is older than
// officers.OrphanBranchAge at now. 🔴 IT ONLY LISTS: nothing here deletes a
// branch, and the push gate refuses a draft officer's branch deletion.
// ghFor is the GitHub of each repository (PushGate.ghFor).
func OrphanBranches(ctx context.Context, officerID, primary string, ship *officers.Ship, ghFor func(string) GitHub, now time.Time) ([]Orphan, error) {
	var out []Orphan
	for _, r := range ship.RepoList(primary) {
		gh := ghFor(r.Repo)
		if gh == nil {
			return nil, fmt.Errorf("no GitHub reader for %s", r.Repo)
		}
		prefix := r.BranchPrefix + officers.Slug(officerID) + "/"
		branches, err := gh.Branches(ctx, prefix)
		if err != nil {
			return nil, fmt.Errorf("listing %s's branches: %v", r.Repo, err)
		}
		if len(branches) == 0 {
			continue
		}
		prs, err := gh.ListPRs(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing %s's pull requests: %v", r.Repo, err)
		}
		hasPR := map[string]bool{}
		for _, p := range prs {
			if p.HeadRepo == "" || strings.EqualFold(p.HeadRepo, r.Repo) {
				hasPR[p.HeadRef] = true
			}
		}
		for _, b := range branches {
			if hasPR[b.Name] || !strings.HasPrefix(b.Name, prefix) {
				continue
			}
			at, err := gh.CommitTime(ctx, b.SHA)
			if err != nil {
				return nil, fmt.Errorf("reading %s's head %s: %v", b.Name, b.SHA, err)
			}
			if now.Sub(at) > officers.OrphanBranchAge {
				out = append(out, Orphan{Repo: r.Repo, Branch: b.Name, SHA: b.SHA, Since: at})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Branch < out[j].Branch
	})
	return out, nil
}

// OrphanNotice is the HQ notice of a non-empty orphan report: the
// repository:branch list, and what to do with it.
func OrphanNotice(officerID, channel string, orphans []Orphan) officers.Notice {
	paths := make([]string, 0, len(orphans))
	for _, o := range orphans {
		paths = append(paths, o.Repo+":"+o.Branch)
	}
	summary := fmt.Sprintf("officer %s has %d branch(es) older than %s with no pull request (a refused or abandoned proposal); "+
		"muster never deletes them — delete each by hand once read", officerID, len(orphans), officers.OrphanBranchAge)
	if len(paths) > officers.MaxNoticePaths {
		summary += fmt.Sprintf(" (+%d more)", len(paths)-officers.MaxNoticePaths)
		paths = paths[:officers.MaxNoticePaths]
	}
	return officers.Notice{Paths: paths, Summary: summary, Channel: channel}
}

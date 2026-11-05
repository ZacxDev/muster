package officership

import (
	"context"
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
	return nil, nil // SCAFFOLD: not implemented yet.
}

// OrphanNotice is the HQ notice of a non-empty orphan report. SCAFFOLD.
func OrphanNotice(officerID, channel string, orphans []Orphan) officers.Notice {
	return officers.Notice{}
}

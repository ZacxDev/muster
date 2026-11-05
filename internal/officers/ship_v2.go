package officers

import (
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/githubapp"
)

// ---------------------------------------------------------------------------
// SHIP v2: SEVERAL REPOSITORIES AND THE `draft` STAGE (docs/officers.md
// "Shipping officers").
//
// A `draft` officer opens DRAFT pull requests in up to MaxShipRepos
// repositories and nothing more: it is never offered officer_mark_ready,
// officer_merge_pr, officer_tag_release or officer_rollout_status, its push
// gate refuses a pull request that is not a draft and every edit that would
// make one reviewable, and every pull request it opens or edits is checked
// against the comparison GitHub reports (forbidden paths, removed hunks of the
// kinds a merge must never delete, secret shapes), a body contract and rate
// limits (internal/officership's draft gate).
//
// 🔴 EVERY v2 RULE IS THE `draft` STAGE'S. A shadow, merge or release officer
// parses and behaves exactly as before; the keys only `draft` honours
// (`forbidden_paths`, several `repos`) are refused on the other stages rather
// than accepted and ignored.
// ---------------------------------------------------------------------------

// StageDraft: open DRAFT pull requests only (in one to MaxShipRepos
// repositories). No tool converts, merges or tags; required_checks and
// review_check are not required.
const StageDraft = "draft"

var _ = maxForbiddenPaths

// Limits of a draft-stage officer.
const (
	// MaxShipRepos is how many repositories `ship.repos` may list.
	MaxShipRepos = 3
	// MaxOpenPRsPerRepo is how many of the officer's own pull requests may be
	// open in one repository at once.
	MaxOpenPRsPerRepo = 2
	// MaxNewPRsPerDay is how many pull requests the officer may open per UTC
	// day, across its repositories.
	MaxNewPRsPerDay = 3
	// MaxPRsPerItem is how many pull requests one HQ item may have, in any
	// state, across the officer's repositories.
	MaxPRsPerItem = 1
	// OrphanBranchAge is how old an officer branch with no pull request must be
	// before the daily report lists it.
	OrphanBranchAge = 7 * 24 * time.Hour
	// maxForbiddenPaths bounds one repository's forbidden_paths.
	maxForbiddenPaths = 64
)

// BodySections are the headings a draft-stage pull request body must carry,
// each as a line of its own.
var BodySections = []string{"## Root cause", "## Evidence", "## Test", "## Rollback"}

// BodyFirstLine is the first line a draft-stage officer's pull request body
// must be, exactly.
func BodyFirstLine(officerID string, item int) string {
	return ""
}

// ShipRepo is one repository a shipping officer writes to.
type ShipRepo struct {
	// Repo is owner/name.
	Repo string
	// Base and BranchPrefix are as Ship's, for this repository.
	Base         string
	BranchPrefix string
	// AppID and AppAccount are the officer App and the installation account
	// (the repository's owner) its token for Repo is minted from.
	AppID      int64
	AppAccount githubapp.Account
	// ForbiddenPaths are globs (MatchForbidden) no pull request of the officer
	// may change, add, remove or rename from. Draft stage only.
	ForbiddenPaths []string
}

// RepoList is the officer's ship repositories: Repos when parsed, else the one
// repository primary described by the Ship's own fields (a Ship built outside
// Parse, as tests do).
func (s *Ship) RepoList(primary string) []ShipRepo {
	if s == nil {
		return nil
	}
	if len(s.Repos) > 0 {
		return s.Repos
	}
	return []ShipRepo{{Repo: primary, Base: s.Base, BranchPrefix: s.BranchPrefix, AppID: s.AppID, AppAccount: s.AppAccount}}
}

// RepoFor is the ship repository named repo (compared without case), ok false
// when the officer does not ship to it.
func (s *Ship) RepoFor(primary, repo string) (ShipRepo, bool) {
	for _, r := range s.RepoList(primary) {
		if strings.EqualFold(r.Repo, repo) {
			return r, true
		}
	}
	return ShipRepo{}, false
}

// ForRepo is a copy of s whose Base, BranchPrefix and App are r's: the policy
// every single-repository rule (the own-pull-request rule, the ref rule) is
// asked with for r.
func (s *Ship) ForRepo(r ShipRepo) *Ship {
	c := *s
	c.Base, c.BranchPrefix, c.AppID, c.AppAccount = r.Base, r.BranchPrefix, r.AppID, r.AppAccount
	return &c
}

// IsDraft reports whether the stage is StageDraft.
func (s *Ship) IsDraft() bool { return s != nil && s.Stage == StageDraft }

// MayMarkReady reports whether officer_mark_ready is the officer's: every
// stage but draft.
func (s *Ship) MayMarkReady() bool { return s != nil }

// BranchItem is the HQ item a draft-stage officer branch is for. SCAFFOLD:
// not implemented yet.
func BranchItem(prefix, officerID, branch string) (int, bool) { return 0, false }

// MatchForbidden reports whether p matches forbidden-path glob pat.
// SCAFFOLD: not implemented yet.
func MatchForbidden(pat, p string) bool { return false }

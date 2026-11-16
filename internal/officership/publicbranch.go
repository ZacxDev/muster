package officership

import (
	"context"
	"errors"
	"fmt"

	"github.com/ZacxDev/muster/internal/officers"
)

// MintPublicBranch is officers.ToolPublicBranch: the branch a draft officer's
// instance pushes item's fix to in ship repository repo, a repository marked
// public. muster names it (officers.NewPublicBranch: a neutral segment, the
// instance's slug and a random token — no officer id, no item number),
// records it as this officer's for item (PublicBranches), and answers it. A
// second call for the same item and repository answers the branch already
// recorded: one branch per item and repository.
//
// Refused: a stage other than draft, a repository that is not one of the
// officer's or not public, an item HQ does not show instance holding (or a
// pull-request fix item, which pushes to its pull request's branch), a slug
// that is not lower-case words, a name that matches an internal-details
// pattern or names this officer (its id or slug: officerNamedWhy), and no
// record to keep it in (fail closed).
func (g *PushGate) MintPublicBranch(ctx context.Context, instance, repo string, item int, slug string) (string, error) {
	if !g.Ship.IsDraft() {
		return "", refused("%s is a draft officer's tool", officers.ToolPublicBranch)
	}
	sr, ok := g.Ship.RepoFor(g.Repo, repo)
	if !ok {
		return "", refused("%s is not one of this officer's repositories (%s)", repo, g.repoNames())
	}
	if !sr.Public {
		return "", refused("%s is not marked public: name the branch %s%s/<item>-<slug> yourself", sr.Repo, sr.BranchPrefix, officers.Slug(g.OfficerID))
	}
	if item <= 0 {
		return "", refused("item_id must be the HQ item you hold")
	}
	if g.PublicBranches == nil {
		return "", errors.New("muster keeps no record of this officer's public branches, so it cannot name one")
	}
	if g.ItemClaimed == nil {
		return "", refused("muster cannot check HQ's claim on item %d", item)
	}
	it, err := g.ItemClaimed(ctx, instance, item)
	if err != nil {
		return "", refused("item %d, which HQ does not show %s holding (claim it with %s first): %v", item, instance, officers.ToolClaim, err)
	}
	if IsPRFix(it.Kind) {
		return "", refused("item %d is a fix item (%s): it pushes to the pull request's existing branch (%s), never a new one", item, it.Kind, it.SubjectKey)
	}
	name, err := officers.NewPublicBranch(sr.BranchPrefix, slug)
	if err != nil {
		return "", refused("%v", err)
	}
	// The slug is the instance's words: scanned like any public text before
	// muster records (and the instance pushes) it.
	if why := g.publicTextWhy(sr, "the branch name", name); why != "" {
		return "", refused("%s", why)
	}
	if why := g.officerNamedWhy(sr, "the branch name", name); why != "" {
		return "", refused("%s", why)
	}
	got, err := g.PublicBranches.Mint(ctx, sr.Repo, item, name)
	if err != nil {
		return "", fmt.Errorf("recording the branch: %v", err)
	}
	return got, nil
}

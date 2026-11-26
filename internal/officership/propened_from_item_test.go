package officership

import (
	"context"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// pr-opened's from_item (draftgate.go announceOpened): the officer's own item
// the pull request was opened for, read from the pull request's branch, rides
// every announcement — the create's and the sweep's re-send — and is left out
// (0, so no key) when the branch names no item of the officer.

// TestTheSweepsReannouncementCarriesFromItem: a pending announcement the
// sweep sends again names the item its branch is for (dBranch is item 17's),
// read from the pull request GitHub reports, not from the create.
func TestTheSweepsReannouncementCarriesFromItem(t *testing.T) {
	r := newPROpenedRig()
	r.loops.state[loopKey(dInfra, dPR)] = officers.PRLoopPending
	_ = r.g.Sweep(context.Background())
	if len(r.calls) != 1 || r.calls[0].PR != dPR {
		t.Fatalf("the sweep announced %+v, want #%d once", r.calls, dPR)
	}
	if r.calls[0].FromItem != 17 {
		t.Fatalf("the sweep's announcement of #%d from %s has from_item %d, want 17", dPR, dBranch, r.calls[0].FromItem)
	}
}

// TestAnAnnouncementWhoseBranchNamesNoItemSendsNoFromItem: a pull request
// whose head is not one of the officer's item branches is still announced,
// with no from_item; and one on another item's branch names THAT item (the
// value comes from the branch, not a constant).
func TestAnAnnouncementWhoseBranchNamesNoItemSendsNoFromItem(t *testing.T) {
	r := newPROpenedRig()
	sr, ok := r.g.Ship.RepoFor(r.g.Repo, dInfra)
	if !ok {
		t.Fatalf("no ship repo %s in the fixture", dInfra)
	}
	pr := r.infra.prs[dPR]
	pr.HeadRef = "bot/engineering-example-web/no-item-here"
	if err := r.g.announceOpened(context.Background(), sr, pr, nil); err != nil {
		t.Fatalf("announcing a pull request whose branch names no item: %v", err)
	}
	pr.HeadRef = "bot/engineering-example-web/18-other-fix"
	if err := r.g.announceOpened(context.Background(), sr, pr, nil); err != nil {
		t.Fatalf("announcing item 18's pull request: %v", err)
	}
	if len(r.calls) != 2 {
		t.Fatalf("pr-opened calls %+v, want 2", r.calls)
	}
	if got := r.calls[0].FromItem; got != 0 {
		t.Errorf("a branch that names no item sent from_item %d, want none (0)", got)
	}
	if got := r.calls[1].FromItem; got != 18 {
		t.Errorf("item 18's branch sent from_item %d, want 18", got)
	}
}

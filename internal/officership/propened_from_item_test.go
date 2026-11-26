package officership

import (
	"context"
	"errors"
	"net/http"
	"strings"
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

// hq400 is the HQ API's 400 with body (its JSON error shape).
func hq400(body string) error {
	return &officers.HQError{Status: http.StatusBadRequest, Body: []byte(body)}
}

// unknownKey400 is what an HQ API that predates from_item answers a body
// carrying it: a generic 400 that names no field (its decoder refuses
// unknown keys).
const unknownKey400 = `{"error":"invalid request body","code":"invalid"}`

// announce runs one announcement of the rig's pull request #dPR on branch
// head (sr as the fixture has it, edited by edit), returning its error.
func (r *prOpenedRig) announce(t *testing.T, head string, edit func(*officers.ShipRepo)) error {
	t.Helper()
	sr, ok := r.g.Ship.RepoFor(r.g.Repo, dInfra)
	if !ok {
		t.Fatalf("no ship repo %s in the fixture", dInfra)
	}
	if edit != nil {
		edit(&sr)
	}
	pr := r.infra.prs[dPR]
	pr.HeadRef = head
	return r.g.announceOpened(context.Background(), sr, pr, nil)
}

// TestAPROpenedRefusedOverFromItemIsSentAgainWithoutIt: an HQ API that does
// not know from_item answers the announcement carrying it with a generic
// 400; muster sends it once more without from_item (logged), and the review
// loop starts. A 400 on that second send stays permanent (failed, not sent a
// third time).
//
// Red without the retry: one call, and the announcement failed.
func TestAPROpenedRefusedOverFromItemIsSentAgainWithoutIt(t *testing.T) {
	r := newPROpenedRig()
	r.errs = []error{hq400(unknownKey400)}
	if err := r.announce(t, dBranch, nil); err != nil {
		t.Fatalf("an announcement HQ refused over from_item, then took without it: %v", err)
	}
	if len(r.calls) != 2 || r.calls[0].FromItem != 17 || r.calls[1].FromItem != 0 {
		t.Fatalf("pr-opened calls %+v, want two: from_item 17, then none", r.calls)
	}
	if got := r.loops.state[loopKey(dInfra, dPR)]; got != officers.PRLoopInLoop {
		t.Fatalf("the announcement settled %q, want %q", got, officers.PRLoopInLoop)
	}
	if !strings.Contains(strings.Join(r.logs, "\n"), "again without from_item") {
		t.Fatalf("the resend was not logged: %q", r.logs)
	}

	// The second send refused too: permanent, never a third.
	r = newPROpenedRig()
	r.errs = []error{hq400(unknownKey400), hq400(unknownKey400)}
	if err := r.announce(t, dBranch, nil); err == nil {
		t.Fatal("both sends refused, and the announcement reported success")
	}
	if len(r.calls) != 2 || r.calls[1].FromItem != 0 {
		t.Fatalf("pr-opened calls %+v, want exactly two, the second without from_item", r.calls)
	}
	if got := r.loops.state[loopKey(dInfra, dPR)]; got != officers.PRLoopFailed {
		t.Fatalf("both refused: settled %q, want %q", got, officers.PRLoopFailed)
	}
}

// TestAPROpenedRefusalNotOverFromItemIsNotResent: a 400 that names another
// pr-opened field, a 4xx that is not a 400, and a 400 to an announcement
// that carried no from_item are each HQ's decision: sent once, failed.
// Controls: a 400 naming a field only inside another word, or from_item
// itself, is resent.
func TestAPROpenedRefusalNotOverFromItemIsNotResent(t *testing.T) {
	for name, c := range map[string]struct {
		head string
		err  error
	}{
		"a 400 naming head_sha":            {dBranch, hq400(`{"error":"head_sha must be 40 hex characters","code":"invalid"}`)},
		"a 400 naming base":                {dBranch, hq400(`{"error":"base is not the repository's default branch","code":"invalid"}`)},
		"a 403":                            {dBranch, &officers.HQError{Status: http.StatusForbidden, Body: []byte(unknownKey400)}},
		"a 400 with no from_item to blame": {"bot/engineering-example-web/no-item-here", hq400(unknownKey400)},
	} {
		r := newPROpenedRig()
		r.err = c.err
		if err := r.announce(t, c.head, nil); err == nil {
			t.Errorf("%s: the announcement reported success", name)
		}
		if len(r.calls) != 1 {
			t.Errorf("%s: pr-opened calls %+v, want exactly one", name, r.calls)
		}
	}
	// Controls: a field's name inside another word (pr in pr-opened, base in
	// database) or from_item itself names no other field, and is resent.
	for _, body := range []string{
		`{"error":"pr-opened: invalid request body","code":"invalid"}`,
		`{"error":"json: unknown field \"from_item\"; nothing written to the database","code":"invalid"}`,
	} {
		r := newPROpenedRig()
		r.errs = []error{hq400(body)}
		if err := r.announce(t, dBranch, nil); err != nil || len(r.calls) != 2 {
			t.Errorf("control: a 400 %s: err %v, calls %+v; want resent without from_item", body, err, r.calls)
		}
	}
}

// TestAPublicBranchsAnnouncementReadsFromItemFromTheRecord: in a repository
// marked public the branch names no item, and from_item is the item muster's
// record holds for it (23 here, distinct from the private fixture's 17); a
// record that cannot be read sends no from_item, and the pull request is
// still announced.
//
// Red with branchItem ignoring the record (from_item 0), and with an
// announcement that sends a from_item on a read error.
func TestAPublicBranchsAnnouncementReadsFromItemFromTheRecord(t *testing.T) {
	public := func(sr *officers.ShipRepo) { sr.Public = true }
	r := newPROpenedRig()
	rec := newMemRecord(dOfficer)
	if _, err := rec.Mint(context.Background(), dInfra, 23, dPubBranch); err != nil {
		t.Fatal(err)
	}
	r.g.PublicBranches = rec
	if err := r.announce(t, dPubBranch, public); err != nil {
		t.Fatalf("announcing a public branch's pull request: %v", err)
	}
	if len(r.calls) != 1 || r.calls[0].FromItem != 23 {
		t.Fatalf("pr-opened calls %+v, want one with from_item 23 (the record's)", r.calls)
	}

	// The failing read answers item 23 WITH its error (a partial read), so a
	// from_item taken from it despite the error shows as 23, not as the 0 an
	// absent answer would give anyway.
	r.g.PublicBranches = partialRecord{memRecord: rec, item: 23}
	if err := r.announce(t, dPubBranch, public); err != nil {
		t.Fatalf("announcing with an unreadable record: %v", err)
	}
	if len(r.calls) != 2 || r.calls[1].FromItem != 0 {
		t.Fatalf("pr-opened calls %+v, want a second announcement with no from_item", r.calls)
	}
}

// partialRecord is a record whose every Item read fails, answering item
// alongside the error.
type partialRecord struct {
	*memRecord
	item int
}

func (p partialRecord) Item(context.Context, string, string) (int, bool, error) {
	return p.item, true, errors.New("fake: the database is down mid-read")
}

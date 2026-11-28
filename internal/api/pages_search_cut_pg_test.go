package api

import (
	"slices"
	"strconv"
	"testing"

	"github.com/ZacxDev/muster/internal/pages"
)

// TestANumberFindsItsPagePastTheListsCut: the list reads at most
// pages.ListLimit pages (newest first); a number search for an older page's
// id still lists that page — read by id — and FIRST, ahead of the listed page
// whose title holds the digits. Its waiting state is read too: under
// ?waiting=1 a page past the cut is listed only while it waits (old's question
// is unanswered; done, a markdown page, cannot wait).
//
// At 084a8aa6b the id's page past the cut was never listed, but the ListLimit
// seam did not exist there, so this file does not compile there: past the
// real 500-row cut needs more pages than one owner may hold (MaxPagesPerOwner)
// and a team's beside them. Its regression value is the mutation check
// recorded in the PR (drop the read by id: "lists [room]").
func TestANumberFindsItsPagePastTheListsCut(t *testing.T) {
	defer func(n int) { pages.ListLimit = n }(pages.ListLimit)
	f := newPageFixture(t)
	const q = `[{"type":"question","id":"q1","label":"Which day suits you?"}]`
	done := f.create(t, f.agent, "Old notes", `[{"type":"markdown","text":"# Old"}]`)
	old := f.create(t, f.agent, "Old survey", q)
	room := f.create(t, f.agent, "Room "+strconv.FormatInt(old, 10)+" booking", q)
	newest := f.create(t, f.agent, "Newest notes", `[{"type":"markdown","text":"# Notes"}]`)
	pages.ListLimit = 2

	all := pagesListIDs(t, f.get(t, "/pages", f.ownerHdr()).Body.String())
	if !slices.Equal(sortedIDs(all...), sortedIDs(room, newest)) {
		t.Fatalf("CONTROL: under a cut of 2 the list draws %v, want room %d and newest %d (done %d and old %d cut off)", all, room, newest, done, old)
	}
	sv := strconv.FormatInt(old, 10)
	for _, q := range []string{"%23" + sv, sv, "page+" + sv} {
		if got := pagesListIDs(t, f.get(t, "/pages?q="+q, f.ownerHdr()).Body.String()); !slices.Equal(got, []int64{old, room}) {
			t.Errorf("?q=%s lists %v, want [%d %d]: the id's page, read past the cut, first", q, got, old, room)
		}
	}
	// It waits (its question is unanswered), so ?waiting=1 keeps it.
	if got := pagesListIDs(t, f.get(t, "/pages?waiting=1&q=%23"+sv, f.ownerHdr()).Body.String()); !slices.Equal(got, []int64{old, room}) {
		t.Errorf("?waiting=1&q=#%s lists %v, want [%d %d]", sv, got, old, room)
	}
	// done, past the cut too, is found by its number — and dropped by
	// ?waiting=1, since it cannot wait.
	sd := strconv.FormatInt(done, 10)
	if got := pagesListIDs(t, f.get(t, "/pages?q=%23"+sd, f.ownerHdr()).Body.String()); len(got) == 0 || got[0] != done {
		t.Errorf("?q=#%s lists %v, want done %d first", sd, got, done)
	}
	if got := pagesListIDs(t, f.get(t, "/pages?waiting=1&q=%23"+sd, f.ownerHdr()).Body.String()); slices.Contains(got, done) {
		t.Errorf("?waiting=1&q=#%s lists done %d, which cannot wait: %v", sd, done, got)
	}
	// Another owner's id is never read past the cut.
	other := f.create(t, f.otherAgent, "Someone else's", q)
	if got := pagesListIDs(t, f.get(t, "/pages?q=%23"+strconv.FormatInt(other, 10), f.ownerHdr()).Body.String()); slices.Contains(got, other) {
		t.Errorf("another owner's page %d is listed: %v", other, got)
	}
}

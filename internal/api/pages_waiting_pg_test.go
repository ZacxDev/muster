package api

import (
	"slices"
	"strings"
	"testing"
)

// TestThePagesListWaitingFilterFollowsTheRule: ?waiting=1 through the real
// server lists a page while an approval or a required part of it is
// unanswered at its current version, or while nothing on it is answered —
// and the chip counts those PAGES. Four pages, each answered differently:
//
//   - approved: approval answered, an optional question left → not waiting
//   - allOptional: an optional question and choice, no answer → waiting
//   - oneOptional: the same, its choice answered             → not waiting
//   - requiredLeft: a required question unanswered beside an
//     answered approval                                       → waiting
//
// REGRESSION TEST: red at 084a8aa6b, which listed every page with ANY
// answerable component unanswered — approved and oneOptional too (4 pages,
// chip "4" counting pages with a component left).
func TestThePagesListWaitingFilterFollowsTheRule(t *testing.T) {
	f := newPageFixture(t)
	submit := func(id int64, component, data string) {
		t.Helper()
		if w := f.ownerSubmit(t, id, 1, component, data, f.ownerHdr()); w.Code != 200 {
			t.Fatalf("answer %s of page %d: %d %s", component, id, w.Code, w.Body.String())
		}
	}
	const approvalAndNote = `[{"type":"approval","id":"a1","label":"Ship it?"},{"type":"question","id":"q1","label":"Anything else?"}]`
	approved := f.create(t, f.agent, "Ship approval", approvalAndNote)
	submit(approved, "a1", `{"decision":"approve"}`)
	const optional = `[{"type":"question","id":"q1","label":"Anything else?"},{"type":"choice","id":"c1","label":"Which venue?","options":["Hall","Garden"]}]`
	allOptional := f.create(t, f.agent, "Venue poll", optional)
	oneOptional := f.create(t, f.agent, "Venue poll, answered", optional)
	submit(oneOptional, "c1", `{"choices":["Hall"]}`)
	requiredLeft := f.create(t, f.agent, "Launch date", `[{"type":"approval","id":"a1","label":"Ship it?"},{"type":"question","id":"q1","label":"Which date?","required":true}]`)
	submit(requiredLeft, "a1", `{"decision":"approve"}`)

	w := f.get(t, "/pages?waiting=1", f.ownerHdr())
	if w.Code != 200 {
		t.Fatalf("/pages?waiting=1: %d", w.Code)
	}
	body := w.Body.String()
	if got, want := sortedIDs(pagesListIDs(t, body)...), sortedIDs(allOptional, requiredLeft); !slices.Equal(got, want) {
		t.Errorf("?waiting=1 lists %v, want %v (approved %d, allOptional %d, oneOptional %d, requiredLeft %d)", got, want, approved, allOptional, oneOptional, requiredLeft)
	}
	if !strings.Contains(body, `data-pages-waiting-count>2</span>`) {
		t.Errorf("the chip does not count the 2 waiting pages")
	}
	// CONTROL: unfiltered, all four are listed.
	if got := pagesListIDs(t, f.get(t, "/pages", f.ownerHdr()).Body.String()); len(got) != 4 {
		t.Fatalf("CONTROL: the list draws %v, want 4 pages", got)
	}
}

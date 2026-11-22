package ui

import (
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"
)

// WHAT IT WORKED ON, GROUPED BY TITLE (focus_history.go): consecutive rows
// with one title are one group — the title once, its steps under it — and a
// title change starts a group. Groups are per page.

// historyShape is a page's rows as "title: step|step", in order.
func historyShape(t *testing.T, v FocusHistoryPageView) []string {
	t.Helper()
	var b strings.Builder
	if err := RenderFocusHistoryPage(&b, v); err != nil {
		t.Fatal(err)
	}
	doc := parseFragment(t, b.String())
	var out []string
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "li" && hasAttr(n, "data-focus-history-item") {
			head := focusNodeText(findAttr(n, "data-focus-history-title"))
			var steps []string
			for _, s := range allAttr(n, "data-focus-history-step") {
				steps = append(steps, focusNodeText(findAttr(s, "data-focus-history-description")))
			}
			out = append(out, head+": "+strings.Join(steps, "|"))
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return out
}

func histEntry(title, desc string, minsAgo int) FocusHistoryEntry {
	return FocusHistoryEntry{Title: title, Description: desc, At: time.Now().Add(-time.Duration(minsAgo) * time.Minute)}
}

// TestFocusHistoryGroupsConsecutiveRowsByTitle: a run of one title is one
// group with every step, newest first; a title change starts a group; the
// same title after another is a NEW group (something else came between).
// RED before grouping: every row drew its own title (seven items, the
// operator's "the same title for every entry").
func TestFocusHistoryGroupsConsecutiveRowsByTitle(t *testing.T) {
	v := FocusHistoryPageView{First: true, Entries: []FocusHistoryEntry{
		histEntry("Fixing the tunnel", "Backoff resets too early.", 1),
		histEntry("Fixing the tunnel", "Reproduced it locally.", 2),
		histEntry("Fixing the tunnel", "Reading the reconnect loop.", 3),
		histEntry("Writing the release notes", "Drafted the summary.", 4),
		histEntry("Fixing the tunnel", "First look.", 5),
		histEntry("Triaging the queue", "", 6),
		histEntry("Triaging the queue", "Three left.", 7),
	}}
	want := []string{
		"Fixing the tunnel: Backoff resets too early.|Reproduced it locally.|Reading the reconnect loop.",
		"Writing the release notes: Drafted the summary.",
		"Fixing the tunnel: First look.",
		"Triaging the queue: |Three left.",
	}
	if got := historyShape(t, v); !slices.Equal(got, want) {
		t.Errorf("groups:\n  %q\nwant\n  %q", got, want)
	}
	// Each step keeps its own time.
	var b strings.Builder
	if err := RenderFocusHistoryPage(&b, v); err != nil {
		t.Fatal(err)
	}
	doc := parseFragment(t, b.String())
	if n, m := len(allAttr(doc, "data-focus-history-step")), len(allAttr(doc, "data-focus-history-at")); n != 7 || m != 7 {
		t.Errorf("%d steps with %d times, want 7 and 7", n, m)
	}
	if n := len(allAttr(doc, "data-focus-history-title")); n != 4 {
		t.Errorf("%d titles drawn, want 4 (one per group)", n)
	}
	// Titles are compared as drawn (plained): markup around the same words
	// is the same title.
	if got := GroupFocusHistory([]FocusHistoryEntry{histEntry("<b>Same</b> work", "a", 1), histEntry("Same work", "b", 2)}); len(got) != 1 || got[0].Title != "Same work" || len(got[0].Entries) != 2 {
		t.Errorf("plained titles: %+v, want one group", got)
	}
}

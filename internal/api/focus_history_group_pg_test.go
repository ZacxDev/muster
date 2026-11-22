package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/ui"
)

// TestFocusHistoryRouteContinuesAGroupAcrossItsPages: a run of one title that
// a page boundary cuts in two (the write rule appends a row for each new
// description, internal/focus) is one group: the first page draws its title
// once over its steps, and names the title it ended on in its sentinel's URL;
// the second page draws the run's rest as a continuation (no title) and its
// next title as a group of its own. RED before grouping: every row drew its
// own title, 20 on the first page.
func TestFocusHistoryRouteContinuesAGroupAcrossItsPages(t *testing.T) {
	h := newHistoryFixture(t)
	sub := focus.AgentSubject(h.agent(t, 1, "hist-run-"+strconv.FormatInt(time.Now().UnixNano(), 36)).ID)
	// Oldest first: 3 rows of "hist other", then 22 steps of "hist run": the
	// newest page (20 rows) is the run alone, the next holds its 2 oldest
	// steps and the 3 others.
	write := func(title, desc string) {
		t.Helper()
		if _, err := h.store.Upsert(asUser(1), sub, focus.Write{Title: title, Description: desc, Source: focus.SourceSummarizer}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 3; i++ {
		write("hist other", fmt.Sprintf("other step %02d", i))
	}
	for i := 1; i <= 22; i++ {
		write("hist run", fmt.Sprintf("run step %02d", i))
	}
	code, body := h.get(t, ui.FocusHistoryPath(sub.String(), ""))
	if code != http.StatusOK {
		t.Fatalf("first page: %d %s", code, body)
	}
	if got := historyTitle.FindAllStringSubmatch(body, -1); len(got) != 1 || got[0][1] != "hist run" {
		t.Fatalf("first page's titles: %v, want the run's once", got)
	}
	if n := strings.Count(body, `data-focus-history-step=""`); n != focus.DefaultHistoryPage {
		t.Errorf("first page: %d steps, want %d", n, focus.DefaultHistoryPage)
	}
	m := historyNext.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("the first page names no next page:\n%s", body)
	}
	next := strings.ReplaceAll(m[1], "&amp;", "&")
	if !strings.Contains(next, ui.FocusHistoryPrevParam+"=hist+run") {
		t.Errorf("the next page's URL does not carry the run's title: %s", next)
	}
	code, body = h.get(t, next)
	if code != http.StatusOK {
		t.Fatalf("second page: %d %s", code, body)
	}
	cont := strings.Index(body, "data-focus-history-continued")
	other := strings.Index(body, "hist other")
	if cont < 0 || other < 0 || cont > other {
		t.Fatalf("second page: the run's rest is not a continuation before the next group:\n%s", body)
	}
	if got := historyTitle.FindAllStringSubmatch(body, -1); len(got) != 1 || got[0][1] != "hist other" {
		t.Errorf("second page's titles: %v, want the other's alone", got)
	}
	for _, want := range []string{"run step 02", "run step 01", "other step 03", "other step 01"} {
		if !strings.Contains(body, want) {
			t.Errorf("second page lacks %q", want)
		}
	}
	// The same page with another title as prev: the run's rest is an
	// ordinary group, titled.
	bare := strings.Replace(next, ui.FocusHistoryPrevParam+"=hist+run", ui.FocusHistoryPrevParam+"=hist+else", 1)
	if _, body := h.get(t, bare); strings.Contains(body, "data-focus-history-continued") || len(historyTitle.FindAllString(body, -1)) != 2 {
		t.Errorf("a page whose prev is another title continues nothing:\n%s", body)
	}
}

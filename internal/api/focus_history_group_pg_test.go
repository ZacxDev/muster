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

// TestFocusHistoryRouteGroupsEachPageByTitle: a run of one title (the write
// rule appends a row for each new description, internal/focus) is drawn as one
// group per page: the first page draws the run's title ONCE over its 20 newest
// steps; the next page, groups being per page (operator decision on #999),
// draws the title again over the run's rest, then the earlier title as its own
// group. RED before grouping: every row drew its own title, 20 on the first
// page.
func TestFocusHistoryRouteGroupsEachPageByTitle(t *testing.T) {
	h := newHistoryFixture(t)
	sub := focus.AgentSubject(h.agent(t, 1, "hist-run-"+strconv.FormatInt(time.Now().UnixNano(), 36)).ID)
	// Oldest first: 3 rows of "hist other", then 22 steps of "hist run".
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
	code, body = h.get(t, strings.ReplaceAll(m[1], "&amp;", "&"))
	if code != http.StatusOK {
		t.Fatalf("second page: %d %s", code, body)
	}
	got := historyTitle.FindAllStringSubmatch(body, -1)
	if len(got) != 2 || got[0][1] != "hist run" || got[1][1] != "hist other" {
		t.Errorf("second page's titles: %v, want the run's again, then the other's", got)
	}
	if n := strings.Count(body, `data-focus-history-step=""`); n != 5 {
		t.Errorf("second page: %d steps, want 5", n)
	}
}

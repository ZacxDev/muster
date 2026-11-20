package api

import (
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// bigPayload is an item payload of about n bytes (HQ's, of any size).
func bigPayload(n int) string {
	return `{"evidence":"` + strings.Repeat("x", n) + `"}`
}

// TestABlockedItemsReRunSurvivesAnEscalatedListOverTheCap is the production
// failure: the officer has far more escalated items than the grant blocked,
// big enough that one page of 100 of them is over muster's 1 MiB answer cap.
// Approving the grant must still re-run every item it blocked. Before, the
// re-run read the escalated list as pages of 100, the first page was
// refused as over the cap, and NOTHING was re-run ("escalated items could
// not be read … over muster's 1048576-byte cap"). CONTROL: a 100-item page of
// that list is over the cap, so the fixture reproduces the refusal.
func TestABlockedItemsReRunSurvivesAnEscalatedListOverTheCap(t *testing.T) {
	r := newRerunRig(t)
	for i := 0; i < 120; i++ {
		id := r.hq.AddItem(rrOff, rrKind, "example-org/example-infra#"+strconv.Itoa(8000+i))
		r.hq.SetPayload(id, bigPayload(12<<10))
		r.hq.Escalate(id)
	}
	blocked := []string{r.held("example-org/example-infra#9101"), r.held("example-org/example-infra#9102"), r.held("example-org/example-infra#9103")}
	r.refuse(officers.GitHubConnector)
	for _, id := range blocked {
		r.hq.Escalate(id)
	}
	o, err := r.s.officerRT.reg.Get(rrOff)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.officerRT.hq.Backend(o).AllItems(t.Context(), officers.StatusEscalated); err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("control: the escalated list read as pages of 100 = %v; want refused over the answer cap", err)
	}

	r.approve(rrGitHub)
	got := map[string]bool{}
	for _, rr := range r.hq.Reruns() {
		got[rr.Item] = true
	}
	for _, id := range blocked {
		if !got[id] {
			t.Fatalf("blocked item %s was not re-run (re-runs %+v); log:\n%s", id, r.hq.Reruns(), r.logs.String())
		}
	}
	if len(got) != len(blocked) {
		t.Fatalf("re-ran %d items, want exactly the %d blocked: %+v", len(got), len(blocked), r.hq.Reruns())
	}
	if r.blocks(rrGitHub) != 0 {
		t.Fatalf("%d blocks left unsettled after every one was re-run", r.blocks(rrGitHub))
	}
}

// doneReads counts the HQ reads of the done list.
func (r *rerunRig) doneReads() int {
	n := 0
	for _, q := range r.hq.Requests() {
		if q.Method == "GET" && strings.HasSuffix(q.Path, "/items") && strings.Contains(q.Query, "status=done") {
			n++
		}
	}
	return n
}

// addDone adds n items that finished done (HQ's history).
func (r *rerunRig) addDone(n int) {
	for i := 0; i < n; i++ {
		r.hq.Complete(r.hq.AddItem(rrOff, rrKind, "example-org/example-infra#d"+strconv.Itoa(i)))
	}
}

// TestAStillClaimedBlockedItemReadsNoDonePage: the blocked item is still
// claimed and HQ's done history is long. The approval answers it from the
// short live lists and reads NO page of done (it used to read done, all of
// it, before the live lists). CONTROL: the done list is long enough to take
// several pages, and the item is settled as still being worked.
func TestAStillClaimedBlockedItemReadsNoDonePage(t *testing.T) {
	r := newRerunRig(t)
	item := r.held("example-org/example-infra#9201")
	r.refuse(officers.GitHubConnector)
	r.addDone(4*officers.StatusPageLimit + 3)
	before := r.doneReads()
	r.approve(rrGitHub)
	if n := r.doneReads() - before; n != 0 {
		t.Fatalf("the approval read %d done page(s), want 0: item %s is still claimed; log:\n%s", n, item, r.logs.String())
	}
	if len(r.hq.Reruns()) != 0 || r.blocks(rrGitHub) != 0 {
		t.Fatalf("re-runs %+v, %d blocks unsettled; want none re-run and the claimed item settled", r.hq.Reruns(), r.blocks(rrGitHub))
	}
	if !strings.Contains(r.logs.String(), "item "+item+" is claimed, not escalated") {
		t.Fatalf("no claimed-skip line for %s; log:\n%s", item, r.logs.String())
	}
}

// TestABlockedItemPastThePageBoundIsNotLoggedAsGone: a blocked item HQ no
// longer has, with a done history past officers.MaxStatusPages pages. The
// read stops at the bound and the log says so, naming the list and the
// bound, and the item as not found BEFORE the bound — never "HQ listed no …
// item in any status read". The block stays unsettled. CONTROL: with a short
// done history the same item IS logged as listed in no status.
func TestABlockedItemPastThePageBoundIsNotLoggedAsGone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		done    int
		want    []string
		notWant string
	}{
		{"past the bound", officers.MaxStatusPages*officers.StatusPageLimit + 10,
			[]string{"done list read stopped at the page bound (400 pages of up to 25)", "not found before the page bound of the done list(s)"},
			"in any status read"},
		{"control: a short history", 30, []string{"in any status read"}, "page bound"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRerunRig(t)
			item := r.held("example-org/example-infra#9301")
			r.refuse(officers.GitHubConnector)
			r.hq.Forget(item)
			r.addDone(tc.done)
			r.approve(rrGitHub)
			logs := r.logs.String()
			for _, w := range tc.want {
				if !strings.Contains(logs, w) {
					t.Fatalf("log lacks %q; log:\n%s", w, logs)
				}
			}
			if strings.Contains(logs, tc.notWant) {
				t.Fatalf("log has %q; log:\n%s", tc.notWant, logs)
			}
			if r.blocks(rrGitHub) != 1 {
				t.Fatalf("%d blocks unsettled, want 1 (the item not found stays unsettled)", r.blocks(rrGitHub))
			}
		})
	}
}

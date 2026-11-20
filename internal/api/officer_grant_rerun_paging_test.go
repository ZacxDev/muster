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

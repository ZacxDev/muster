package api

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/officers"
)

// shortRetries makes the re-run's retry waits test-sized.
func shortRetries(t *testing.T, n int) {
	t.Helper()
	saved := rerunRetryDelays
	rerunRetryDelays = make([]time.Duration, n)
	for i := range rerunRetryDelays {
		rerunRetryDelays[i] = 5 * time.Millisecond
	}
	t.Cleanup(func() { rerunRetryDelays = saved })
}

// failItemReads makes HQ answer 503 to the first n item-list reads from now.
func (r *rerunRig) failItemReads(n int32) {
	var left atomic.Int32
	left.Store(n)
	r.hq.Before = func(method, path string) {
		if method == http.MethodGet && strings.HasSuffix(path, "/items") {
			r.hq.SetDown(left.Add(-1) >= 0)
		}
	}
}

// TestAFailedReadOfTheBlockedItemsIsRetried: HQ cannot list the items when
// the grant is approved; the re-run does not give up silently — it logs the
// failure, waits, reads again, and re-runs the blocked item once HQ answers.
// Before, one failed read ended the re-run for good.
func TestAFailedReadOfTheBlockedItemsIsRetried(t *testing.T) {
	shortRetries(t, 4)
	r := newRerunRig(t)
	item := r.held("example-org/example-infra#4401")
	r.refuse(officers.GitHubConnector)
	r.hq.Escalate(item)
	r.failItemReads(2)
	r.approve(rrGitHub)
	if got := r.hq.Reruns(); len(got) != 1 || got[0].Item != item {
		t.Fatalf("re-runs %+v after HQ failed twice then answered; want item %s re-run. log:\n%s", got, item, r.logs.String())
	}
	if n := strings.Count(r.logs.String(), "could not be read"); n != 2 {
		t.Fatalf("%d failed-read lines, want one per failed try (2):\n%s", n, r.logs.String())
	}
	if !strings.Contains(r.logs.String(), "retrying in ") {
		t.Fatalf("no retry line:\n%s", r.logs.String())
	}
}

// TestARerunThatNeverReadsSaysSoAndTheRouteReRunsItLater: HQ stays down for
// every try: the last line says the items stay unsettled and names the route;
// nothing is settled. Once HQ answers, POST /ui/connector-grants/{id}/rerun
// re-runs them. The route refuses a grant that is not approved (409) and one
// it cannot read (404).
func TestARerunThatNeverReadsSaysSoAndTheRouteReRunsItLater(t *testing.T) {
	shortRetries(t, 2)
	r := newRerunRig(t)
	item := r.held("example-org/example-infra#4402")
	r.refuse(officers.GitHubConnector)
	r.hq.Escalate(item)
	r.failItemReads(100)
	r.approve(rrGitHub)
	if got := r.hq.Reruns(); len(got) != 0 {
		t.Fatalf("re-ran %+v while HQ never listed the items", got)
	}
	route := "POST /ui/connector-grants/" + strconv.FormatInt(rrGitHub, 10) + "/rerun"
	if !strings.Contains(r.logs.String(), "after 3 tries") || !strings.Contains(r.logs.String(), route) {
		t.Fatalf("the give-up line does not say how many tries or name the route:\n%s", r.logs.String())
	}
	if r.blocks(rrGitHub) != 1 {
		t.Fatalf("%d unsettled blocks, want the 1 never re-run", r.blocks(rrGitHub))
	}

	r.hq.Before = nil
	r.hq.SetDown(false)
	call := func(id int64) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/ui/connector-grants/"+strconv.FormatInt(id, 10)+"/rerun", nil)
		req.SetPathValue("id", strconv.FormatInt(id, 10))
		w := httptest.NewRecorder()
		r.s.handleConnectorGrantRerun(w, req)
		r.s.WaitForConnectorBlocks()
		return w
	}
	if w := call(rrDriver); w.Code != http.StatusConflict {
		t.Fatalf("re-run of a PENDING grant: %d %s, want 409", w.Code, w.Body)
	}
	// An approved grant of an agent that is no officer instance blocks no item.
	plain := agents.Agent{ID: 3301, Name: "brave-otter", Kind: agents.KindClaudeCode, Status: agents.StatusRunning, OwnerID: 1}
	store := r.s.ext.Agents.(*officerAgents)
	store.mu.Lock()
	store.rows = append(store.rows, plain)
	store.mu.Unlock()
	r.conn.mu.Lock()
	r.conn.grants[3302] = &connectors.Grant{ID: 3302, AgentID: plain.ID, AgentName: plain.Name, Connector: officers.GitHubConnector, Status: connectors.StatusApproved}
	r.conn.mu.Unlock()
	if w := call(3302); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "not an officer instance") {
		t.Fatalf("re-run of a non-officer agent's grant: %d %s, want 409", w.Code, w.Body)
	}
	if w := call(4040); w.Code != http.StatusNotFound {
		t.Fatalf("re-run of an unknown grant: %d %s, want 404", w.Code, w.Body)
	}
	if got := r.hq.Reruns(); len(got) != 0 {
		t.Fatalf("a refused route call re-ran %+v", got)
	}
	if w := call(rrGitHub); w.Code != http.StatusAccepted {
		t.Fatalf("re-run of the approved grant: %d %s, want 202", w.Code, w.Body)
	}
	if got := r.hq.Reruns(); len(got) != 1 || got[0].Item != item {
		t.Fatalf("the route re-ran %+v; want item %s", got, item)
	}
	// Settled: a second call re-runs nothing more.
	if w := call(rrGitHub); w.Code != http.StatusAccepted {
		t.Fatalf("second call: %d", w.Code)
	}
	if got := r.hq.Reruns(); len(got) != 1 {
		t.Fatalf("a second route call re-ran again: %+v", got)
	}
	if g, _ := r.conn.GetGrant(t.Context(), rrGitHub); g.Status != connectors.StatusApproved {
		t.Fatalf("the route changed the grant to %s", g.Status)
	}
}

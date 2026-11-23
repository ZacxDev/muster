package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// Settling a decided grant's blocks (officer_connector_rerun.go,
// settleGrantBlocks): every decision settles the blocks on its grant — an
// approval re-runs the escalated ones, a denial settles them all — and the
// block sweep settles what a decision left (a failed read, a decision made
// before this rule). A block is never re-run twice when a manual re-run races
// the approval. Ids and subjects are made up to equal no constant.
// ---------------------------------------------------------------------------

// deny denies grant id through the one operator decision and waits for its
// background work.
func (r *rerunRig) deny(id int64) {
	r.t.Helper()
	if _, _, err := r.s.decideConnectorGrant(context.Background(), id, false, connectors.DecisionSource{}); err != nil {
		r.t.Fatalf("deny grant %d: %v", id, err)
	}
	r.s.WaitForConnectorBlocks()
}

// setStatus decides grant id in the store only, as a decision made by a
// muster without the settle rule left it.
func (r *rerunRig) setStatus(id int64, status string) {
	r.conn.mu.Lock()
	r.conn.grants[id].Status = status
	r.conn.mu.Unlock()
}

// TestDenyingAGrantSettlesTheBlocksOnIt: a denied grant's blocks used to stay
// unsettled for good — nothing re-runs them and nothing else reads them.
// Denying settles every block on THAT grant and re-runs nothing. CONTROL: the
// same items' blocks on the agent's other, still pending, grant are untouched.
func TestDenyingAGrantSettlesTheBlocksOnIt(t *testing.T) {
	r := newRerunRig(t)
	a, b := r.held("example-org/example-infra#6031"), r.held("example-org/example-infra#6032")
	r.refuse(officers.GitHubConnector)
	r.refuse(rrDriverC)
	r.hq.Escalate(a)
	r.hq.Escalate(b)
	if r.blocks(rrGitHub) != 2 || r.blocks(rrDriver) != 2 {
		t.Fatalf("fixture: %d github and %d driver blocks, want 2 and 2", r.blocks(rrGitHub), r.blocks(rrDriver))
	}
	r.deny(rrGitHub)
	if n := r.blocks(rrGitHub); n != 0 {
		t.Fatalf("%d blocks on the denied grant left unsettled, want 0; log:\n%s", n, r.logs.String())
	}
	if got := r.hq.Reruns(); len(got) != 0 {
		t.Fatalf("a denial re-ran %+v; want nothing", got)
	}
	if n := r.blocks(rrDriver); n != 2 {
		t.Fatalf("control: %d blocks on the still-pending driver grant, want 2 (a denial settles only its own grant)", n)
	}
}

// TestAManualRerunRacingTheApprovalReRunsOnce: the operator's re-run route
// called while the approval's own re-run is in flight. The first re-run's
// POST is held until the racing run has read HQ's item lists (or 1s passes):
// without the per-grant lock both runs read the block unsettled and both ask
// HQ to re-run it. With it, exactly one re-run request is sent.
func TestAManualRerunRacingTheApprovalReRunsOnce(t *testing.T) {
	r := newRerunRig(t)
	item := r.held("example-org/example-infra#6120")
	r.refuse(officers.GitHubConnector)
	r.hq.Escalate(item)

	var listReads atomic.Int32
	var posts atomic.Int32
	r.hq.Before = func(method, path string) {
		switch {
		case method == http.MethodGet && strings.HasSuffix(path, "/items"):
			listReads.Add(1)
		case method == http.MethodPost && strings.HasSuffix(path, "/rerun") && posts.Add(1) == 1:
			// One run reads open, claimed, waiting, escalated (4 lists) before
			// its POST; wait for the other run's reads too.
			deadline := time.Now().Add(time.Second)
			for listReads.Load() < 8 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
		}
	}
	if _, _, err := r.s.decideConnectorGrant(context.Background(), rrGitHub, true, connectors.DecisionSource{}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/ui/connector-grants/"+strconv.FormatInt(rrGitHub, 10)+"/rerun", nil)
	req.SetPathValue("id", strconv.FormatInt(rrGitHub, 10))
	w := httptest.NewRecorder()
	r.s.handleConnectorGrantRerun(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("route: %d %s, want 202", w.Code, w.Body)
	}
	r.s.WaitForConnectorBlocks()
	if got := r.hq.Reruns(); len(got) != 1 || got[0].Item != item {
		t.Fatalf("re-run requests %+v; want exactly one for item %s (the racing run must see the block settled)", got, item)
	}
	if n := r.blocks(rrGitHub); n != 0 {
		t.Fatalf("%d blocks unsettled, want 0", n)
	}
}

// TestTheSweepSettlesWhatADecisionLeft is the production state: blocks on an
// APPROVED grant whose approval could not re-run them (HQ's lists unreadable
// then), and blocks on a grant DENIED before denial settled anything. Nothing
// ever read either again. One sweep pass re-runs the approved grant's
// escalated item, settles its done item without a re-run, and settles the
// denied grant's blocks. A second pass re-runs nothing more. CONTROL: a block
// on a still-pending grant is left alone.
func TestTheSweepSettlesWhatADecisionLeft(t *testing.T) {
	r := newRerunRig(t)
	esc, done := r.held("example-org/example-infra#6201"), r.held("example-org/example-infra#6202")
	r.refuse(officers.GitHubConnector)
	r.hq.Escalate(esc)
	r.hq.Complete(done)
	r.failItemReads(100)
	r.approve(rrGitHub)
	if got := r.hq.Reruns(); len(got) != 0 || r.blocks(rrGitHub) != 2 {
		t.Fatalf("fixture: re-runs %+v and %d blocks; want none and 2 (the approval could not read HQ)", got, r.blocks(rrGitHub))
	}
	r.hq.Before = nil
	r.hq.SetDown(false)

	// the pending driver grant's block (an item held now): left alone by the
	// sweep
	r.held("example-org/example-infra#6203")
	r.refuse(rrDriverC)
	pendingBlocks := r.blocks(rrDriver)
	if pendingBlocks == 0 {
		t.Fatal("fixture: no block on the pending driver grant")
	}

	r.s.sweepConnectorBlocks(context.Background())
	got := r.hq.Reruns()
	if len(got) != 1 || got[0].Item != esc || got[0].Key != officers.GrantRerunKey(rrGitHub) {
		t.Fatalf("sweep re-ran %+v; want only escalated item %s with the grant's key; log:\n%s", got, esc, r.logs.String())
	}
	if n := r.blocks(rrGitHub); n != 0 {
		t.Fatalf("%d blocks on the approved grant unsettled after the sweep, want 0 (the done one is settled without a re-run)", n)
	}
	if n := r.blocks(rrDriver); n != pendingBlocks {
		t.Fatalf("control: the sweep changed the pending grant's blocks %d -> %d", pendingBlocks, n)
	}

	// The driver grant is denied by a decision that settled nothing: the
	// next pass settles its blocks, re-running none.
	r.setStatus(rrDriver, connectors.StatusDenied)
	r.s.sweepConnectorBlocks(context.Background())
	if n := r.blocks(rrDriver); n != 0 {
		t.Fatalf("%d blocks on the denied grant after the sweep, want 0", n)
	}
	if got := r.hq.Reruns(); len(got) != 1 {
		t.Fatalf("a second pass re-ran more: %+v", got)
	}
}

// TestAnApprovedGrantsBlockPastTheWindowIsSettledNotReRun: a block older than
// officers.RerunWindow is never re-run; it is settled, so it does not sit
// unsettled for good. CONTROL: a block inside the window is re-run.
func TestAnApprovedGrantsBlockPastTheWindowIsSettledNotReRun(t *testing.T) {
	r := newRerunRig(t)
	old := r.held("example-org/example-infra#6301")
	r.refuse(officers.GitHubConnector)
	r.hq.Escalate(old)
	r.advance(officers.RerunWindow - 36*time.Hour)
	recent := r.held("example-org/example-infra#6302")
	r.refuse(officers.GitHubConnector)
	r.hq.Escalate(recent)
	r.advance(37 * time.Hour)
	r.approve(rrGitHub)
	if got := r.hq.Reruns(); len(got) != 1 || got[0].Item != recent {
		t.Fatalf("re-runs %+v; want only %s", got, recent)
	}
	if n := r.blocks(rrGitHub); n != 0 {
		t.Fatalf("%d blocks unsettled, want 0: the one past the window is settled, not left", n)
	}
	if !strings.Contains(r.logs.String(), "past the "+officers.RerunWindow.String()+" window") {
		t.Fatalf("no line for the block settled past the window; log:\n%s", r.logs.String())
	}
}

// TestTheSweepPassIsSerialisedWithAnApprovalPerGrant pins that the sweep
// takes the same per-grant lock as the approval: a sweep pass started while an
// approval's re-run of the grant is in flight sends no second re-run.
func TestTheSweepPassIsSerialisedWithAnApprovalPerGrant(t *testing.T) {
	r := newRerunRig(t)
	item := r.held("example-org/example-infra#6401")
	r.refuse(officers.GitHubConnector)
	r.hq.Escalate(item)
	var listReads, posts atomic.Int32
	r.hq.Before = func(method, path string) {
		switch {
		case method == http.MethodGet && strings.HasSuffix(path, "/items"):
			listReads.Add(1)
		case method == http.MethodPost && strings.HasSuffix(path, "/rerun") && posts.Add(1) == 1:
			deadline := time.Now().Add(time.Second)
			for listReads.Load() < 8 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
		}
	}
	if _, _, err := r.s.decideConnectorGrant(context.Background(), rrGitHub, true, connectors.DecisionSource{}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); r.s.sweepConnectorBlocks(context.Background()) }()
	wg.Wait()
	r.s.WaitForConnectorBlocks()
	if got := r.hq.Reruns(); len(got) != 1 || got[0].Item != item {
		t.Fatalf("re-run requests %+v; want exactly one for item %s", got, item)
	}
}

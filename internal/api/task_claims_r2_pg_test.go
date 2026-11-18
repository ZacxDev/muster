package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// CLAIMS, ROUND 2 (#586): an UPSTREAM claim whose claimant session ended is
// released too, and stays released for that snapshot revision; a claim's host
// is only one the mirror listed the session on; and a question card on a
// claimed task offers no dispatch.
//
// ⚠ THIS FILE USES ONLY SYMBOLS THAT EXIST AT #586's PREVIOUS HEAD (the
// fixtures, the routes, the tools by name), so there it compiles and goes red
// for a behavioural reason. Every title, key and id is invented.

// claimantSnap is a reference snapshot at rev whose claimants name session sid.
func claimantSnap(rev, sid string) map[string]any {
	return claimSnap("in_progress", "in progress", map[string]any{"rev": rev,
		"claimants": []map[string]any{{"agent": "ag_presscal01", "agent_session": sid}}})
}

// TestAnUpstreamClaimWhoseSessionEndedIsReleased (Y-a): the upstream claimant's
// session is one the owner's host lists (linked, with that host); the window
// then runs another conversation. The next assignment gets its card; a
// snapshot at the SAME revision still naming the session does not bring the
// claim back; a newer revision naming a live session claims again.
func TestAnUpstreamClaimWhoseSessionEndedIsReleased(t *testing.T) {
	f := newTriageFixture(t, false)
	key := refKey()
	id, m := f.assignRef(t, key, claimantSnap("7", routeSID))
	f.mustBeLeftClaimed(t, id, m, "ag_presscal01")
	f.mirror.set(func(m *routeMirror) { m.claudeSession = triSID2 }) // routeSID's window is gone
	_, m = f.assignRef(t, key, claimantSnap("7", routeSID))
	f.mustGetItsCard(t, id, m)
	// the same revision again: still released
	out := f.call(f.chief, taskRouteAttentionTool, map[string]any{"task_id": id, "note": "the next notice"})
	if strings.Contains(out, `"claimed":true`) {
		t.Fatalf("the released upstream claim came back at the same revision: %s", out)
	}
	// a newer revision (a new pickup) by the same session, running again,
	// claims again
	f.mirror.set(func(m *routeMirror) { m.claudeSession = routeSID })
	f.upsertRef(t, key, claimantSnap("8", routeSID))
	out = f.call(f.chief, taskRouteAttentionTool, map[string]any{"task_id": id, "note": "the next notice"})
	if !strings.Contains(out, `"claimed":true`) {
		t.Fatalf("a newer pickup by a live session is no claim: %s", out)
	}
}

// TestAnUpstreamClaimantWithNoHostIsNeverEnded (Y-a): a claimant session no
// host lists has no host to be judged on, so it is never judged ended — the
// task stays claimed though the host reports and lists windows.
func TestAnUpstreamClaimantWithNoHostIsNeverEnded(t *testing.T) {
	f := newTriageFixture(t, false)
	key := refKey()
	id, m := f.assignRef(t, key, claimantSnap("3", claimantSID))
	f.mustBeLeftClaimed(t, id, m, "ag_presscal01")
	_, m = f.assignRef(t, key, claimantSnap("3", claimantSID))
	f.mustBeLeftClaimed(t, id, m, "ag_presscal01")
}

// TestAClaimsHostIsOnlyOneTheMirrorListed (Y-b): a session no host lists picks
// the task up through the machine route, naming the host "tower" in its own
// header. The claim records NO host — so, though "tower" is online, reports
// status and lists windows without that session, the claim is never judged
// ended: the next assignment is left alone. On a reference (the status only
// queued) and on a copy task (stored, where the request's link carries the
// header's host).
func TestAClaimsHostIsOnlyOneTheMirrorListed(t *testing.T) {
	pickUpFromAMachine := func(t *testing.T, f *triageFixture, id int64, want int) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPatch, "/api/tasks/"+strconv.FormatInt(id, 10)+"/status", strings.NewReader(`{"status":"in_progress"}`))
		req.Header.Set("X-Muster-Session-Id", claimantSID) // not in any host's window list
		req.Header.Set("X-Muster-Host", "tower")
		req = req.WithContext(f.asOwner)
		req.SetPathValue("id", strconv.FormatInt(id, 10))
		rec := httptest.NewRecorder()
		f.srv.handleAPITaskStatus(rec, req)
		if rec.Code != want || len(f.claimedBy(t, id)) != 1 {
			t.Fatalf("the pickup: HTTP %d, claims %q", rec.Code, f.claimedBy(t, id))
		}
	}
	t.Run("a reference", func(t *testing.T) {
		f := newTriageFixture(t, false)
		key := refKey()
		id := f.upsertRef(t, key, claimSnap("open", "to do", nil))
		pickUpFromAMachine(t, f, id, http.StatusAccepted)
		who := f.claimedBy(t, id)[0]
		_, m := f.assignRef(t, key, claimSnap("open", "to do", nil))
		f.mustBeLeftClaimed(t, id, m, who)
		if n, err := f.srv.ReleaseEndedClaims(context.Background()); err != nil || n != 0 {
			t.Fatalf("the sweep released %d (%v) of a claim with no mirror host; want 0", n, err)
		}
	})
	t.Run("a copy task", func(t *testing.T) {
		f := newTriageFixture(t, false)
		key := refKey()
		id := f.fileTask(t, f.tok, key, map[string]any{"create": map[string]any{"title": "Oil the lathe", "body": "Oil it."}})
		pickUpFromAMachine(t, f, id, http.StatusOK)
		who := f.claimedBy(t, id)[0]
		if out := f.notice(t, f.tok, key, noticeAssigned, "upstream: assigned to you", false); out["routed"] != routedChief {
			t.Fatalf("the assignment: %v", out)
		}
		f.srv.WaitForPluginDeliveries()
		m, err := f.eventOf(id)
		if err != nil {
			t.Fatal(err)
		}
		f.mustBeLeftClaimed(t, id, m, who)
		if n, err := f.srv.ReleaseEndedClaims(context.Background()); err != nil || n != 0 {
			t.Fatalf("the sweep released %d (%v) of a claim with no mirror host; want 0", n, err)
		}
	})
}

// TestAQuestionCardOnAClaimedTaskOffersNoDispatch (🟢): with dispatch on, an
// unclaimed task's card offers agent_dispatch (the control); a claimed task's
// question card does not, and agent_dispatch itself refuses the claimed task.
func TestAQuestionCardOnAClaimedTaskOffersNoDispatch(t *testing.T) {
	f := newTriageFixture(t, true)
	free := f.upsertRef(t, refKey(), claimSnap("open", "to do", nil))
	claimed := f.upsertRef(t, refKey(), claimSnap("open", "to do", map[string]any{"claimants": []map[string]any{
		{"agent": "ag_presscal01", "agent_session": claimantSID}}}))
	ask := func(id int64) []string {
		t.Helper()
		out := f.call(f.chief, taskRouteAttentionTool, map[string]any{"task_id": id, "note": "context", "question": "Which die should it use?"})
		if toolRefused(out) {
			t.Fatalf("task %d: %s", id, out)
		}
		asks := f.asksAbout(t, id)
		if len(asks) != 1 {
			t.Fatalf("task %d: %d cards", id, len(asks))
		}
		return asks[0].Options
	}
	hasDispatch := func(opts []string) bool {
		for _, o := range opts {
			if strings.HasPrefix(o, "agent_dispatch on ") {
				return true
			}
		}
		return false
	}
	if opts := ask(free); !hasDispatch(opts) {
		t.Fatalf("control: an unclaimed task's card offers no dispatch: %q", opts)
	}
	if opts := ask(claimed); hasDispatch(opts) {
		t.Fatalf("a claimed task's question card offers dispatch: %q", opts)
	}
	out := f.call(f.chief, agentDispatchTool, map[string]any{"target": "pod/gateway", "task_id": claimed})
	if !toolRefused(out) || !strings.Contains(out, "already claimed") {
		t.Fatalf("agent_dispatch on a claimed task: %s", out)
	}
}

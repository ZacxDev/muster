package api

import (
	"net/http"
	"strings"
	"testing"
)

// CLAIMS, ROUND 1 (#586): a session claim is released by done (any writer),
// by the upstream reopening the task, and by the claiming session ending; an
// upstream claimant needs a session; and a claim suppresses only the
// "pick this up" card — the chief can still ASK the owner about a claimed task.
//
// ⚠ THIS FILE USES ONLY SYMBOLS THAT EXIST AT #586's PREVIOUS HEAD (the
// fixtures, the routes, the tool by name), so there it compiles and goes red
// for a behavioural reason. Every title, key and id is invented.

// pickUp has session routeSID (which the fixture's host lists) pick reference
// id up through the machine route, and asserts the claim.
func (f *triageFixture) pickUp(t *testing.T, id int64, want int) {
	t.Helper()
	if code := f.machineStatus(t, f.asOwner, id, "in_progress", routeSID); code != want {
		t.Fatalf("the session's pickup: HTTP %d; want %d", code, want)
	}
	if got := f.claimedBy(t, id); len(got) != 1 {
		t.Fatalf("fixture: the pickup claimed %q; want one claim", got)
	}
}

// TestCompleteReleasesTheClaim (Y1): a task stored as complete releases its
// session claims, whoever wrote it — the owner's machine write on a copy task,
// and a snapshot that moved a reference to complete.
func TestCompleteReleasesTheClaim(t *testing.T) {
	t.Run("a stored complete", func(t *testing.T) {
		f := newTriageFixture(t, false)
		id := f.fileTask(t, f.tok, refKey(), map[string]any{"create": map[string]any{"title": "Grease the press", "body": "Grease it."}})
		f.pickUp(t, id, http.StatusOK)
		if code := f.machineStatus(t, f.asOwner, id, "complete", ""); code != http.StatusOK {
			t.Fatalf("complete: HTTP %d", code)
		}
		if got := f.claimedBy(t, id); len(got) != 0 {
			t.Fatalf("a complete task is still claimed by %q", got)
		}
	})
	t.Run("the upstream's complete", func(t *testing.T) {
		f := newTriageFixture(t, false)
		key := refKey()
		id := f.upsertRef(t, key, claimSnap("open", "to do", nil))
		f.pickUp(t, id, http.StatusAccepted)
		f.upsertRef(t, key, claimSnap("complete", "complete", nil))
		if st := f.taskJSON(t, id)["status"]; st != "complete" {
			t.Fatalf("fixture: the task is %v; want complete", st)
		}
		if got := f.claimedBy(t, id); len(got) != 0 {
			t.Fatalf("a task the upstream completed is still claimed by %q", got)
		}
	})
}

// TestAnUpstreamReopenReleasesTheSessionClaim (Y1): a session picked the
// reference up and the upstream applied it (in progress); the upstream then
// moves it back to not-started. The session's claim is released and the next
// assignment gets its card. (A snapshot that only confirms the status the task
// already had — the upstream refusing the session's in_progress — releases
// nothing: TestAClaimSurvivesAnUpstreamThatRefusesInProgress.)
func TestAnUpstreamReopenReleasesTheSessionClaim(t *testing.T) {
	f := newTriageFixture(t, false)
	key := refKey()
	id := f.upsertRef(t, key, claimSnap("open", "to do", nil))
	f.pickUp(t, id, http.StatusAccepted)
	f.upsertRef(t, key, claimSnap("in_progress", "in progress", nil))
	if got := f.claimedBy(t, id); len(got) != 1 {
		t.Fatalf("fixture: the upstream's in progress released the claim: %q", got)
	}
	f.upsertRef(t, key, claimSnap("open", "to do", nil))
	if got := f.claimedBy(t, id); len(got) != 0 {
		t.Fatalf("after the upstream reopened the task it is still claimed by %q", got)
	}
	_, m := f.assignRef(t, key, claimSnap("open", "to do", nil))
	f.mustGetItsCard(t, id, m)
}

// TestAClaimWhoseSessionEndedIsReleasedAtTheTriage (Y1): the claiming session's
// window is gone from its host (online, reporting status, listing windows):
// the triage releases the claim and the assignment gets its card.
func TestAClaimWhoseSessionEndedIsReleasedAtTheTriage(t *testing.T) {
	f := newTriageFixture(t, false)
	key := refKey()
	id := f.upsertRef(t, key, claimSnap("open", "to do", nil))
	f.pickUp(t, id, http.StatusAccepted)
	f.mirror.set(func(m *routeMirror) { m.claudeSession = triSID2 }) // the window now runs another conversation
	_, m := f.assignRef(t, key, claimSnap("open", "to do", nil))
	f.mustGetItsCard(t, id, m)
	if got := f.claimedBy(t, id); len(got) != 0 {
		t.Fatalf("the ended session's claim was kept: %q", got)
	}
}

// TestAClaimOnAHostThatCannotSayIsKept: the same, but the host does not report
// status — it cannot say a window is gone — so the claim stands (the control
// for the ended case: the release is the rule's, not any pass's).
func TestAClaimOnAHostThatCannotSayIsKept(t *testing.T) {
	f := newTriageFixture(t, false)
	key := refKey()
	id := f.upsertRef(t, key, claimSnap("open", "to do", nil))
	f.pickUp(t, id, http.StatusAccepted)
	f.mirror.set(func(m *routeMirror) { m.claudeSession, m.statusReport = triSID2, false })
	_, m := f.assignRef(t, key, claimSnap("open", "to do", nil))
	f.mustBeLeftClaimed(t, id, m, f.claimedBy(t, id)[0])
}

// TestAnUpstreamClaimantWithoutASessionIsNoClaim: a claimant naming an agent
// but no session (or one muster would not take) claims nothing: the
// assignment gets its card.
func TestAnUpstreamClaimantWithoutASessionIsNoClaim(t *testing.T) {
	for name, cl := range map[string]map[string]any{
		"no session":         {"agent": "ag_presscal09"},
		"an invalid session": {"agent": "ag_presscal09", "agent_session": "not a session id!"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newTriageFixture(t, false)
			id, m := f.assignRef(t, refKey(), claimSnap("in_progress", "in progress", map[string]any{"claimants": []map[string]any{cl}}))
			f.mustGetItsCard(t, id, m)
		})
	}
}

// TestTheChiefCanStillAskAboutAClaimedTask (Y2): on a claimed task the chief's
// task_route_attention WITHOUT a question raises nothing (nothing to pick up);
// WITH one it raises the card, naming the question and who is working it.
func TestTheChiefCanStillAskAboutAClaimedTask(t *testing.T) {
	f := newTriageFixture(t, false)
	id := f.upsertRef(t, refKey(), claimSnap("open", "to do", map[string]any{"claimants": []map[string]any{
		{"agent": "ag_presscal01", "agent_session": claimantSID}}}))
	if out := f.call(f.chief, taskRouteAttentionTool, map[string]any{"task_id": id, "note": "the assignment"}); !strings.Contains(out, `"claimed":true`) {
		t.Fatalf("a pickup card on a claimed task: %s", out)
	}
	if asks := f.asksAbout(t, id); len(asks) != 0 {
		t.Fatalf("%d card(s) for a claimed task without a question", len(asks))
	}
	out := f.call(f.chief, taskRouteAttentionTool, map[string]any{"task_id": id, "note": "the deadline moved",
		"question": "Should the calibration wait for the new die?"})
	if toolRefused(out) || strings.Contains(out, `"claimed":true`) {
		t.Fatalf("a question about a claimed task: %s", out)
	}
	asks := f.asksAbout(t, id)
	if len(asks) != 1 || !strings.Contains(asks[0].Question, "Your chief asks: Should the calibration wait for the new die?") ||
		!strings.Contains(asks[0].Question, "ag_presscal01 is working this task") {
		t.Fatalf("the question card: %+v", asks)
	}
}

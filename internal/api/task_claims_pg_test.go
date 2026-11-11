package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
)

// CLAIMS through the real routes, triage and Postgres (docs/tasks.md#claims):
// a session's explicit pickup (a status write to in_progress carrying its
// session id) and the upstream's claim (a reference snapshot naming
// claimants: the claiming agent and its session) each mark a task claimed; a
// status alone (in progress upstream, the owner's own in_progress) does not; an assignment of a
// claimed task gets NO "needs you" card; an unclaim reopens it; a snapshot
// with no claimants field is handled; one owner's claim is never another's.
//
// ⚠ THIS FILE USES ONLY SYMBOLS THAT EXIST WITHOUT THE FEATURE (the fixtures,
// the routes, SQL-free reads of the task JSON), so on the pre-change tree it
// compiles and goes red for a BEHAVIOURAL reason. Every title, key, name and
// id is invented.

const (
	claimSnapLabel = "in progress"
	// claimantSID is an upstream claimant's recorded session that NO host lists.
	claimantSID = "5e55a1d0-0000-4000-8000-00000000c1a1"
)

// claimSnap is a reference snapshot, with extra fields merged in.
func claimSnap(status, label string, extra map[string]any) map[string]any {
	r := map[string]any{"title": "Calibrate the widget press", "url": "https://tracker.example.invalid/t/77", "status": status,
		"status_label": label, "rev": "1", "updated_at": time.Now().UTC().Format(time.RFC3339Nano), "body": "Calibrate it."}
	for k, v := range extra {
		r[k] = v
	}
	return r
}

// refKey is a fresh key in the fixture's namespace.
func refKey() string { return "tracker:clm" + strconv.FormatInt(time.Now().UnixNano(), 36) }

// upsertRef files (or refreshes) the reference for key with snap, not routed.
func (f *triageFixture) upsertRef(t *testing.T, key string, snap map[string]any) int64 {
	t.Helper()
	code, out := f.handoff(t, f.tok, map[string]any{"key": key, "reference": snap})
	if code/100 != 2 || out["id"] == nil {
		t.Fatalf("reference upsert %s: %d %v", key, code, out)
	}
	return int64(out["id"].(float64))
}

// assignRef posts the reference for key with snap as an "assigned" notice
// routed to the chief, and answers the task and its event.
func (f *triageFixture) assignRef(t *testing.T, key string, snap map[string]any) (int64, plugins.Message) {
	t.Helper()
	code, out := f.handoff(t, f.tok, map[string]any{"key": key, "reference": snap, "route_to_chief": true, "notice": noticeAssigned,
		"event_id": "ev-" + strconv.FormatInt(time.Now().UnixNano(), 36)})
	if code/100 != 2 || out["routed"] != routedChief {
		t.Fatalf("the assignment of %s: %d %v", key, code, out)
	}
	f.srv.WaitForPluginDeliveries()
	id := int64(out["id"].(float64))
	m, err := f.eventOf(id)
	if err != nil {
		t.Fatal(err)
	}
	return id, m
}

// machineStatus is PATCH /api/tasks/{id}/status as ctx's principal, with the
// session header when session is not "": the HTTP code.
func (f *triageFixture) machineStatus(t *testing.T, ctx context.Context, id int64, status, session string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, "/api/tasks/"+strconv.FormatInt(id, 10)+"/status", strings.NewReader(`{"status":"`+status+`"}`))
	if session != "" {
		req.Header.Set("X-Muster-Session-Id", session)
	}
	req = req.WithContext(ctx)
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	rec := httptest.NewRecorder()
	f.srv.handleAPITaskStatus(rec, req)
	return rec.Code
}

// taskJSON is GET /api/tasks/{id} as the owner, decoded.
func (f *triageFixture) taskJSON(t *testing.T, id int64) map[string]any {
	t.Helper()
	n, err := f.srv.ext.Notes.Get(f.asOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(n)
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// sessionsOf is task id's embedded thread (none: an empty list).
func (f *triageFixture) sessionsOf(t *testing.T, id int64) []any {
	t.Helper()
	links, _ := f.taskJSON(t, id)["sessions"].([]any)
	return links
}

// claimedBy is the claimedBy of each session link of task id that carries a
// claim (claimedAt), in thread order.
func (f *triageFixture) claimedBy(t *testing.T, id int64) []string {
	t.Helper()
	var out []string
	links := f.sessionsOf(t, id)
	for _, l := range links {
		m := l.(map[string]any)
		if m["claimedAt"] != nil {
			out = append(out, m["claimedBy"].(string))
		}
	}
	return out
}

// mustBeLeftClaimed asserts the triage left assignment m of task id alone as
// claimed by who: no card, no chief turn, settled saying so, task_claimed.
func (f *triageFixture) mustBeLeftClaimed(t *testing.T, id int64, m plugins.Message, who string) {
	t.Helper()
	if asks := f.asksAbout(t, id); len(asks) != 0 {
		t.Fatalf("a claimed task got %d \"needs you\" card(s): %q", len(asks), asks[0].Question)
	}
	if n := f.turns.count(); n != 0 {
		t.Fatalf("the chief took %d turn(s) on a claimed task's assignment", n)
	}
	if m.Status != plugins.StatusDelivered || m.SettleReason == nil ||
		*m.SettleReason != "muster: task triage — left it alone: it is already claimed by "+who {
		t.Fatalf("the event: %s, reason %v; want settled as claimed by %q", m.Status, m.SettleReason, who)
	}
	if h := f.handled(t, m); h != "task_claimed" {
		t.Fatalf("handled by %q, want task_claimed", h)
	}
}

// mustGetItsCard asserts the triage put assignment m of task id on the board.
func (f *triageFixture) mustGetItsCard(t *testing.T, id int64, m plugins.Message) {
	t.Helper()
	if asks := f.asksAbout(t, id); len(asks) != 1 || !strings.HasPrefix(asks[0].Question, "Task #"+strconv.FormatInt(id, 10)+" needs you") {
		t.Fatalf("an unclaimed task's card: %+v; want one", asks)
	}
	if h := f.handled(t, m); h != "task_board" {
		t.Fatalf("handled by %q, want task_board", h)
	}
}

// --- a claim from each side marks the task claimed --------------------------

// TestASessionsPickupClaimsTheTaskAndTheTriageSkipsItsCard: a session sets
// the task in_progress through the machine route with its session id (what
// `muster task status` sends). The link records the claim under the mirrored
// session's name, and the task's assignment is left alone.
func TestASessionsPickupClaimsTheTaskAndTheTriageSkipsItsCard(t *testing.T) {
	f := newTriageFixture(t, false)
	f.mirror.set(func(m *routeMirror) { m.label = "gizmo press" })
	key := refKey()
	id := f.upsertRef(t, key, claimSnap("open", "to do", nil))
	if code := f.machineStatus(t, f.asOwner, id, "in_progress", routeSID); code != http.StatusAccepted {
		t.Fatalf("the session's pickup on a reference: HTTP %d; want 202 (queued upstream)", code)
	}
	if got := f.claimedBy(t, id); len(got) != 1 || !strings.Contains(got[0], "gizmo press") {
		t.Fatalf("the claim: %q; want one, under the mirrored session's name", got)
	}
	_, m := f.assignRef(t, key, claimSnap("open", "to do", nil))
	f.mustBeLeftClaimed(t, id, m, f.claimedBy(t, id)[0])
}

// TestAnUpstreamClaimantMarksTheReferenceClaimed: the snapshot names an
// agent working the record (and its recorded session, which no host lists:
// not linked). The task is claimed by that name; the triage leaves it alone.
func TestAnUpstreamClaimantMarksTheReferenceClaimed(t *testing.T) {
	f := newTriageFixture(t, false)
	snap := claimSnap("open", "to discuss", map[string]any{"claimants": []map[string]any{
		{"agent": "ag_presscal01", "agent_session": claimantSID}}})
	id, m := f.assignRef(t, refKey(), snap)
	f.mustBeLeftClaimed(t, id, m, "ag_presscal01")
	for _, l := range f.sessionsOf(t, id) {
		if l.(map[string]any)["sessionId"] == claimantSID {
			t.Fatal("a claimant session no host lists was linked to the task")
		}
	}
}

// TestAnUpstreamClaimantsKnownSessionIsLinked: the claimant's recorded session
// is one a host of the owner lists: it joins the task's thread as worked.
func TestAnUpstreamClaimantsKnownSessionIsLinked(t *testing.T) {
	f := newTriageFixture(t, false)
	snap := claimSnap("open", "to do", map[string]any{"claimants": []map[string]any{
		{"agent": "ag_presscal01", "agent_session": routeSID}}})
	id := f.upsertRef(t, refKey(), snap)
	var role string
	for _, l := range f.sessionsOf(t, id) {
		if m := l.(map[string]any); m["sessionId"] == routeSID {
			role, _ = m["role"].(string)
		}
	}
	if role != "worked" {
		t.Fatalf("the claimant's known session on the thread: role %q; want worked", role)
	}
}

// TestAnUpstreamInProgressStatusAloneIsNoClaim: no claimants field at all
// (what a plugin from before claimants pushes), the upstream in progress. A
// status names nobody: the task is unclaimed, and its assignment gets its
// card.
func TestAnUpstreamInProgressStatusAloneIsNoClaim(t *testing.T) {
	f := newTriageFixture(t, false)
	id, m := f.assignRef(t, refKey(), claimSnap("in_progress", claimSnapLabel, nil))
	f.mustGetItsCard(t, id, m)
}

// TestTheOwnersOwnInProgressIsNoClaim: the owner sets the task in_progress on
// its card (no session): no claim, and the assignment gets its card.
func TestTheOwnersOwnInProgressIsNoClaim(t *testing.T) {
	f := newTriageFixture(t, false)
	key := refKey()
	id := f.upsertRef(t, key, claimSnap("open", "to do", nil))
	req := httptest.NewRequest(http.MethodPatch, "/tasks/"+strconv.FormatInt(id, 10)+"/status",
		strings.NewReader(url.Values{"status": {"in_progress"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(f.asOwner)
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	rec := httptest.NewRecorder()
	f.srv.handleNoteStatus(rec, req)
	if rec.Code/100 != 2 {
		t.Fatalf("the owner's card write: HTTP %d %s", rec.Code, rec.Body)
	}
	if got := f.claimedBy(t, id); len(got) != 0 {
		t.Fatalf("the owner's own in_progress claimed the task: %q", got)
	}
	_, m := f.assignRef(t, key, claimSnap("open", "to do", nil))
	f.mustGetItsCard(t, id, m)
}

// TestAnAgentSettingItsTaskOpenReleasesTheClaim: a session claimed the task;
// the dispatched agent bound to it sets it back to open through its own route
// and through its own tool: every claim is released.
func TestAnAgentSettingItsTaskOpenReleasesTheClaim(t *testing.T) {
	for _, via := range []string{"route", "tool"} {
		f := newTriageFixture(t, false)
		id := f.upsertRef(t, refKey(), claimSnap("open", "to do", nil))
		if _, err := f.pool.Exec(f.asOwner, `UPDATE agents SET note_id = $1 WHERE id = $2`, id, f.worker.ID); err != nil {
			t.Fatal(err)
		}
		if code := f.machineStatus(t, f.asOwner, id, "in_progress", routeSID); code != http.StatusAccepted {
			t.Fatalf("%s: pickup: HTTP %d", via, code)
		}
		if len(f.claimedBy(t, id)) != 1 {
			t.Fatalf("%s: fixture: the pickup claimed nothing", via)
		}
		a, err := f.srv.ext.Agents.Get(f.asOwner, f.worker.ID)
		if err != nil {
			t.Fatal(err)
		}
		switch via {
		case "route":
			w := f.connFixture.call(t, http.MethodPatch, "/agent/task/status", strings.NewReader(`{"status":"open"}`),
				map[string]string{"Authorization": "Bearer " + f.workerTok, "Content-Type": "application/json"})
			if w.Code/100 != 2 {
				t.Fatalf("route: the agent's open: HTTP %d %s", w.Code, w.Body)
			}
		case "tool":
			if out := f.srv.dispatchAgentTool(f.asOwner, a, "agent_set_task_status", `{"status":"open"}`); toolRefused(out) {
				t.Fatalf("tool: the agent's open: %s", out)
			}
		}
		if got := f.claimedBy(t, id); len(got) != 0 {
			t.Fatalf("%s: after the agent's open the task is still claimed by %q", via, got)
		}
	}
}

// --- an unclaimed task still gets its card ---------------------------------

// TestASnapshotWithNoClaimantsFieldAndNoProgressGetsItsCard: no claimants
// field, status open, no session claim: unclaimed, so the card (INVARIANT
// GUARD for the base's behaviour, and the control for the claimed cases).
func TestASnapshotWithNoClaimantsFieldAndNoProgressGetsItsCard(t *testing.T) {
	f := newTriageFixture(t, false)
	id, m := f.assignRef(t, refKey(), claimSnap("open", "to do", nil))
	f.mustGetItsCard(t, id, m)
	// A session that only READ or commented the task is no claim either.
	f2 := newTriageFixture(t, false)
	key := refKey()
	id2 := f2.upsertRef(t, key, claimSnap("open", "to do", map[string]any{"claimants": []map[string]any{}}))
	if code := f2.machineStatus(t, f2.asOwner, id2, "ready_for_review", routeSID); code/100 != 2 {
		t.Fatalf("a non-pickup status write: HTTP %d", code)
	}
	_, m2 := f2.assignRef(t, key, claimSnap("open", "to do", nil))
	f2.mustGetItsCard(t, id2, m2)
}

// --- an unclaim reopens the task -------------------------------------------

// TestAnUnclaimReopensTheTask: a session's claim is released by the task set
// back to open; an upstream claim by a snapshot without it. The next
// assignment then gets its card.
func TestAnUnclaimReopensTheTask(t *testing.T) {
	t.Run("the session's claim, released by open", func(t *testing.T) {
		f := newTriageFixture(t, false)
		key := refKey()
		id := f.upsertRef(t, key, claimSnap("open", "to do", nil))
		if code := f.machineStatus(t, f.asOwner, id, "in_progress", routeSID); code != http.StatusAccepted {
			t.Fatalf("pickup: HTTP %d", code)
		}
		if len(f.claimedBy(t, id)) != 1 {
			t.Fatal("fixture: the pickup claimed nothing")
		}
		if code := f.machineStatus(t, f.asOwner, id, "open", routeSID); code != http.StatusAccepted {
			t.Fatalf("unclaim: HTTP %d", code)
		}
		if got := f.claimedBy(t, id); len(got) != 0 {
			t.Fatalf("after the unclaim the task is still claimed by %q", got)
		}
		_, m := f.assignRef(t, key, claimSnap("open", "to do", nil))
		f.mustGetItsCard(t, id, m)
	})
	t.Run("the upstream's claim, dropped by a later snapshot", func(t *testing.T) {
		f := newTriageFixture(t, false)
		key := refKey()
		claimed := claimSnap("open", "to do", map[string]any{"claimants": []map[string]any{{"agent": "ag_fitter01", "agent_session": claimantSID, "name": "A. Fitter"}}})
		id, first := f.assignRef(t, key, claimed)
		f.mustBeLeftClaimed(t, id, first, "A. Fitter") // the control: the claim was there
		_, m := f.assignRef(t, key, claimSnap("open", "to do", nil))
		f.mustGetItsCard(t, id, m)
	})
}

// TestAMirroredSessionsTaskToolPickupClaimsTheTask: the other pickup path —
// a mirrored session's own task_set_status (on a task in its scope: one it
// filed) — claims under the session's name; its open releases the claim.
func TestAMirroredSessionsTaskToolPickupClaimsTheTask(t *testing.T) {
	f := newTriageFixture(t, false)
	id := f.upsertRef(t, refKey(), claimSnap("open", "to do", nil))
	if _, _, err := f.srv.ext.Notes.LinkSession(f.asOwner, id, notes.SessionLink{SessionID: routeSID, Role: notes.RoleCreated}, false); err != nil {
		t.Fatal(err)
	}
	c := toolCaller{asker: attention.Asker{MirrorHost: "tower", MirrorSession: routeSID, Label: "widget bench"}}
	out := f.srv.toolTaskSetStatus(f.asOwner, c, `{"id":`+strconv.FormatInt(id, 10)+`,"status":"in_progress"}`)
	if toolRefused(out) {
		t.Fatalf("the session's task_set_status: %s", out)
	}
	if got := f.claimedBy(t, id); len(got) != 1 || got[0] != "widget bench" {
		t.Fatalf("the claim: %q; want one, by the session's name", got)
	}
	if out := f.srv.toolTaskSetStatus(f.asOwner, c, `{"id":`+strconv.FormatInt(id, 10)+`,"status":"open"}`); toolRefused(out) {
		t.Fatalf("the session's task_set_status open: %s", out)
	}
	if got := f.claimedBy(t, id); len(got) != 0 {
		t.Fatalf("after open the task is still claimed by %q", got)
	}
}

// --- the claim survives a status the upstream refused ------------------------

// TestAClaimSurvivesAnUpstreamThatRefusesInProgress: the upstream record's
// status set has no "in progress" (a discussion record): the session's
// in_progress is queued, the plugin fails it and pushes a fresh snapshot
// still open. The task stays open — and stays claimed.
func TestAClaimSurvivesAnUpstreamThatRefusesInProgress(t *testing.T) {
	f := newTriageFixture(t, false)
	key := refKey()
	id := f.upsertRef(t, key, claimSnap("open", "to discuss", nil))
	if code := f.machineStatus(t, f.asOwner, id, "in_progress", routeSID); code != http.StatusAccepted {
		t.Fatalf("pickup: HTTP %d; want 202", code)
	}
	// The plugin claims the write and reports it failed, with a fresh snapshot.
	w := f.connFixture.call(t, http.MethodPost, "/plugin/v1/task-writes/claim", strings.NewReader(`{"wait_s":0,"limit":5}`),
		map[string]string{"Authorization": "Bearer " + f.tok, "Content-Type": "application/json"})
	var claim struct {
		Writes []struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		} `json:"writes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &claim); err != nil || len(claim.Writes) != 1 || claim.Writes[0].Status != "in_progress" {
		t.Fatalf("the queued write: %d %s", w.Code, w.Body)
	}
	res, _ := json.Marshal(map[string]any{"ok": false, "error": "the upstream record's type has no status \"in progress\"",
		"reference": claimSnap("open", "to discuss", nil)})
	w = f.connFixture.call(t, http.MethodPost, "/plugin/v1/task-writes/"+strconv.FormatInt(claim.Writes[0].ID, 10)+"/result",
		strings.NewReader(string(res)), map[string]string{"Authorization": "Bearer " + f.tok, "Content-Type": "application/json"})
	if w.Code != http.StatusOK {
		t.Fatalf("the failed result: %d %s", w.Code, w.Body)
	}
	if st := f.taskJSON(t, id)["status"]; st != "open" {
		t.Fatalf("fixture: the task is %v; want open (the upstream refused in_progress)", st)
	}
	if got := f.claimedBy(t, id); len(got) != 1 {
		t.Fatalf("the refused upstream write took the claim with it: %q", got)
	}
	_, m := f.assignRef(t, key, claimSnap("open", "to discuss", nil))
	f.mustBeLeftClaimed(t, id, m, f.claimedBy(t, id)[0])
}

// --- the chief's own call ----------------------------------------------------

// TestTheChiefsRouteAttentionRaisesNoCardForAClaimedTask: the chief's
// task_route_attention on a claimed task answers claimed and files no ask.
func TestTheChiefsRouteAttentionRaisesNoCardForAClaimedTask(t *testing.T) {
	f := newTriageFixture(t, false)
	id := f.upsertRef(t, refKey(), claimSnap("open", "to do", map[string]any{"claimants": []map[string]any{{"agent": "press-calibrator", "agent_session": claimantSID}}}))
	out := f.call(f.chief, taskRouteAttentionTool, map[string]any{"task_id": id, "note": "look at this"})
	if toolRefused(out) || !strings.Contains(out, `"claimed":true`) || !strings.Contains(out, "press-calibrator") {
		t.Fatalf("task_route_attention on a claimed task: %s", out)
	}
	if asks := f.asksAbout(t, id); len(asks) != 0 {
		t.Fatalf("the chief's call raised %d card(s) on a claimed task", len(asks))
	}
}

// --- owner isolation -----------------------------------------------------------

// TestAnotherOwnersPickupIsNoClaim: another owner's session cannot claim the
// owner's task (the task is not theirs to see: 404), and the owner's
// assignment still gets its card.
func TestAnotherOwnersPickupIsNoClaim(t *testing.T) {
	f := newTriageFixture(t, false)
	stranger := f.newMember(t)
	key := refKey()
	id := f.upsertRef(t, key, claimSnap("open", "to do", nil))
	if code := f.machineStatus(t, f.as(stranger), id, "in_progress", routeSID); code != http.StatusNotFound {
		t.Fatalf("another owner's pickup: HTTP %d; want 404", code)
	}
	if got := f.claimedBy(t, id); len(got) != 0 {
		t.Fatalf("another owner's pickup claimed the task: %q", got)
	}
	_, m := f.assignRef(t, key, claimSnap("open", "to do", nil))
	f.mustGetItsCard(t, id, m)
}

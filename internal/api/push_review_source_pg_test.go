package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/push"
)

// The ready-for-review push names WHERE the status change came from, driven
// through the REAL writers end to end (the plugin route, the agent tools, the
// machine and owner routes) over Postgres.
//
// ⚠ THIS FILE USES ONLY SYMBOLS THAT EXIST WITHOUT THE FEATURE, so it compiles
// on the pre-change tree and goes red there for a behavioural reason (every
// title is the unlabelled "Task #N ready for review").

// reviewTitles is every ready-for-review push title the owner's device got for
// task id (its tag is task-<id>-done; other pushes are ignored).
func reviewTitles(p *payloadPusher, endpoint string, id int64) []string {
	var out []string
	for _, pl := range p.to(endpoint) {
		if pl.Tag == fmt.Sprintf("task-%d-done", id) && pl.Type != push.TypeResolved {
			out = append(out, pl.Title)
		}
	}
	return out
}

func TestTheReviewPushNamesItsSourceEndToEnd(t *testing.T) {
	f := newRefFixture(t)
	store := &memPush{}
	dev := ep("owner-phone")
	claim(t, store, f.owner, dev)
	pusher := &payloadPusher{}
	f.srv.ext.Push = push.NewService(store, pusher, push.VAPIDConfig{PublicKey: "k"}, log.New(io.Discard, "", 0))

	expect := func(t *testing.T, id int64, want string) {
		t.Helper()
		f.srv.WaitForPushes()
		if got := reviewTitles(pusher, dev, id); len(got) != 1 || got[0] != want {
			t.Fatalf("task %d's review push titles %q, want exactly [%q]", id, got, want)
		}
	}

	t.Run("an upstream snapshot names the upstream and its namespace", func(t *testing.T) {
		key := f.key(t) // "tracker:<…>": the fixture plugin's namespace
		t0 := time.Now().Add(-time.Hour).Truncate(time.Second)
		id := f.mustUpsert(t, key, refSnap("in_progress", "in progress", t0, nil))
		if again := f.mustUpsert(t, key, refSnap("ready_for_review", "awaiting review", t0.Add(time.Minute), nil)); again != id {
			t.Fatalf("the newer snapshot named task %d, want %d", again, id)
		}
		if st, _, _, _ := f.row(t, id); st != notes.StatusReadyForReview {
			t.Fatalf("the snapshot did not move the task: status %q", st)
		}
		expect(t, id, fmt.Sprintf("Task #%d ready for review · upstream (tracker)", id))
	})

	t.Run("task_set_status by an agent names the agent", func(t *testing.T) {
		n := f.task(t, "agent-tool task")
		args, _ := json.Marshal(map[string]any{"id": n.ID, "status": "ready_for_review"})
		if text := f.srv.toolTaskSetStatus(f.asOwner, callerOfAgent(f.chief), string(args)); strings.Contains(text, `"error"`) {
			t.Fatalf("task_set_status: %s", text)
		}
		expect(t, n.ID, fmt.Sprintf("Task #%d ready for review · agent %s", n.ID, f.chief.Name))
	})

	t.Run("a bound worker's own tool names the worker", func(t *testing.T) {
		n := f.task(t, "worker task")
		w := f.byHand(t, n, "reviewworker")
		if text := f.srv.dispatchAgentTool(f.asOwner, w, "agent_set_task_status", `{"Status":"ready_for_review"}`); strings.Contains(text, `"error"`) {
			t.Fatalf("agent_set_task_status: %s", text)
		}
		expect(t, n.ID, fmt.Sprintf("Task #%d ready for review · agent %s", n.ID, w.Name))
	})

	t.Run("the machine route reads agent", func(t *testing.T) {
		n := f.task(t, "machine task")
		req := httptest.NewRequest(http.MethodPatch, "/api/tasks/"+strconv.FormatInt(n.ID, 10)+"/status",
			strings.NewReader(`{"status":"ready_for_review"}`))
		if rec := f.serve(f.srv.handleAPITaskStatus, req, pv(n.ID)); rec.Code/100 != 2 {
			t.Fatalf("PATCH /api/tasks/%d/status: HTTP %d %s", n.ID, rec.Code, rec.Body)
		}
		expect(t, n.ID, fmt.Sprintf("Task #%d ready for review · agent", n.ID))
	})

	// ⚠ An invariant guard, not regression coverage: the owner's push was
	// unlabelled before this change too, and stays so.
	t.Run("the owner's card control stays unlabelled", func(t *testing.T) {
		n := f.task(t, "owner task")
		req := httptest.NewRequest(http.MethodPatch, "/tasks/"+strconv.FormatInt(n.ID, 10)+"/status",
			strings.NewReader(url.Values{"status": {"ready_for_review"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if rec := f.serve(f.srv.handleNoteStatus, req, pv(n.ID)); rec.Code/100 != 2 {
			t.Fatalf("PATCH /tasks/%d/status: HTTP %d %s", n.ID, rec.Code, rec.Body)
		}
		expect(t, n.ID, fmt.Sprintf("Task #%d ready for review", n.ID))
	})
}

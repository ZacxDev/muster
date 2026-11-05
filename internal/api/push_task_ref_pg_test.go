package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
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

// Task pushes carry the task's external reference and its creator, driven end
// to end over Postgres through the REAL doors: a plugin's reference snapshot
// (with the optional creator) filed and moved by the plugin route, and the
// muster-native creates — the owner's board create, an agent's machine create,
// a producer's hook-token create.

// taskPushes is every push (not a resolved control message) the device got
// under tag, as "title | body".
func taskPushes(p *payloadPusher, endpoint, tag string) []string {
	var out []string
	for _, pl := range p.to(endpoint) {
		if pl.Tag == tag && pl.Type != push.TypeResolved {
			out = append(out, pl.Title+" | "+pl.Body)
		}
	}
	return out
}

func expectPushes(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("pushes:\n%q\nwant:\n%q", got, want)
	}
}

func TestAReferenceSnapshotsCreatorReachesThePushEndToEnd(t *testing.T) {
	f := newViaUpstreamFixture(t)
	timers := &manualTimers{}
	f.srv.taskCreateAfter = timers.after
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)
	key := f.key(t) // "tracker:<id>"
	_, upID := notes.ParseTag(key)
	frag := "tracker " + upID + " · by Pat Example"

	id := f.mustUpsert(t, key, refSnap("in_progress", "in progress", t0, map[string]any{"creator": "Pat Example"}))
	var stored string
	if err := f.pool.QueryRow(f.asOwner, `SELECT coalesce(ext_snapshot->>'creator', '') FROM notes WHERE id = $1`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "Pat Example" {
		t.Fatalf("the stored snapshot's creator is %q, want the plugin's", stored)
	}
	if n := timers.fireAll(); n != 1 {
		t.Fatalf("%d created-push timers, want 1 (a new reference row is a new task)", n)
	}
	f.srv.WaitForPushes()
	expectPushes(t, taskPushes(f.pusher, f.dev, "tasks-new"), "New task | Upstream task ("+frag+")")

	f.mustUpsert(t, key, refSnap("ready_for_review", "awaiting review", t0.Add(time.Minute), map[string]any{"creator": "Pat Example"}))
	f.srv.WaitForPushes()
	expectPushes(t, taskPushes(f.pusher, f.dev, fmt.Sprintf("task-%d-done", id)),
		fmt.Sprintf("Task #%d ready for review · upstream (tracker) | Upstream task (%s)", id, frag))

	// A COPY task the plugin files with `create`: its key tag is the reference,
	// and the plugin (by its label) is the creator muster recorded.
	f.pusher.reset()
	key3 := f.key(t)
	_, upID3 := notes.ParseTag(key3)
	if code, out := f.post(t, f.tok, "/plugin/v1/tasks", map[string]any{"key": key3,
		"create": map[string]any{"title": "Copied task", "body": "b"}}); code != http.StatusCreated {
		t.Fatalf("plugin create: HTTP %d %v", code, out["_raw"])
	}
	timers.fireAll()
	f.srv.WaitForPushes()
	expectPushes(t, taskPushes(f.pusher, f.dev, "tasks-new"), "New task | Copied task (tracker "+upID3+" · by plugin Example tracker)")

	// A snapshot without the field: the reference alone, no empty separator.
	key2 := f.key(t)
	_, upID2 := notes.ParseTag(key2)
	id2 := f.mustUpsert(t, key2, refSnap("ready_for_review", "awaiting review", t0, nil))
	f.srv.WaitForPushes()
	expectPushes(t, taskPushes(f.pusher, f.dev, fmt.Sprintf("task-%d-done", id2)),
		fmt.Sprintf("Task #%d ready for review · upstream (tracker) | Upstream task (tracker %s)", id2, upID2))
}

func TestANativeTasksCreatorReachesThePushEndToEnd(t *testing.T) {
	f := newViaUpstreamFixture(t)
	timers := &manualTimers{}
	f.srv.taskCreateAfter = timers.after

	// apiCreate posts POST /api/tasks as ctx's caller and returns the new id.
	apiCreate := func(t *testing.T, ctx context.Context, title string, hdr map[string]string) int64 {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/tasks",
			strings.NewReader(`{"title":`+strconv.Quote(title)+`,"body":"b"}`)).WithContext(ctx)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		f.srv.handleAPITaskCreate(rec, req)
		var out struct{ ID int64 }
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.ID <= 0 {
			t.Fatalf("POST /api/tasks: HTTP %d %s", rec.Code, rec.Body)
		}
		return out.ID
	}
	created := func(t *testing.T, want string) {
		t.Helper()
		f.pusher.reset()
		if n := timers.fireAll(); n != 1 {
			t.Fatalf("%d created-push timers, want 1", n)
		}
		f.srv.WaitForPushes()
		expectPushes(t, taskPushes(f.pusher, f.dev, "tasks-new"), want)
	}

	t.Run("the chief, as a service caller", func(t *testing.T) {
		apiCreate(t, context.WithValue(f.asOwner, serviceActorCtxKey{}, f.chief.Name), "Chief's task", nil)
		created(t, "New task | Chief's task (by chief)")
	})
	t.Run("a worker agent, as a service caller", func(t *testing.T) {
		w := f.byHand(t, f.task(t, "anchor"), "maker")
		apiCreate(t, context.WithValue(f.asOwner, serviceActorCtxKey{}, w.Name), "Worker's task", nil)
		created(t, "New task | Worker's task (by agent "+w.Name+")")
	})
	t.Run("a producer on the hook token", func(t *testing.T) {
		apiCreate(t, f.asOwner, "Drafted task", map[string]string{headerSource: "drafter"})
		created(t, "New task | Drafted task (by Task drafter)")
	})
	t.Run("an unidentified hook-token caller names no one", func(t *testing.T) {
		apiCreate(t, f.asOwner, "Anonymous task", nil)
		created(t, "New task | Anonymous task")
	})
	// A service actor that is not one of the owner's agents is not named one.
	t.Run("a service caller that is not an agent names no one", func(t *testing.T) {
		apiCreate(t, context.WithValue(f.asOwner, serviceActorCtxKey{}, "some-service"), "Service task", nil)
		created(t, "New task | Service task")
	})

	t.Run("the owner's board create reads 'by you' on its review push", func(t *testing.T) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("body", "Owner-filed task")
		_ = mw.Close()
		req := httptest.NewRequest(http.MethodPost, "/notes", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		if rec := f.serve(f.srv.handleNoteCreate, req, nil); rec.Code != http.StatusOK {
			t.Fatalf("POST /notes: HTTP %d %s", rec.Code, rec.Body)
		}
		var id int64
		if err := f.pool.QueryRow(f.asOwner, `SELECT id FROM notes WHERE body = 'Owner-filed task' ORDER BY id DESC LIMIT 1`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		req = httptest.NewRequest(http.MethodPatch, "/tasks/"+strconv.FormatInt(id, 10)+"/status",
			strings.NewReader(url.Values{"status": {"ready_for_review"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if rec := f.serve(f.srv.handleNoteStatus, req, pv(id)); rec.Code/100 != 2 {
			t.Fatalf("PATCH /tasks/%d/status: HTTP %d %s", id, rec.Code, rec.Body)
		}
		f.srv.WaitForPushes()
		expectPushes(t, taskPushes(f.pusher, f.dev, fmt.Sprintf("task-%d-done", id)),
			fmt.Sprintf("Task #%d ready for review | Owner-filed task (by you)", id))
	})
}

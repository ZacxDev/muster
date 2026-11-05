package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/push"
)

// Task pushes carry the task's external reference and its creator, driven end
// to end over Postgres through the REAL doors: a plugin's reference snapshot
// (with the optional creator) filed and moved by the plugin route, and the
// muster-native create doors (TestNoCreateDoorNamesACreatorItCannotKnow).

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

// TestNoCreateDoorNamesACreatorItCannotKnow drives every muster-native create
// door that records a creator THROUGH THE REAL ROUTER (s.Handler, i.e.
// registerAll and each route's own auth wrapper), with that door's own
// credential, and pins what is stored in notes.created_by and what the task's
// push says:
//
//   - the board (POST /tasks, the owner's session): stored "owner", and the
//     push names no creator (it goes to the owner);
//   - the chief's task_create_gated (POST /mcp, the chief's own token): "chief";
//   - a plugin's create (POST /plugin/v1/tasks, the plugin's token): "plugin:<label>";
//   - POST /api/tasks on the shared hook token: NOTHING, even when the request
//     carries X-Muster-Actor naming the chief. requireHookToken never resolves
//     an actor, so no agent may be recorded from that header; the push names
//     the producer when X-Muster-Source gives one, else no one.
//
// The hook token acts as the operator (machineUserID), so the operator has a
// device too; every other door acts as the fixture's owner.
func TestNoCreateDoorNamesACreatorItCannotKnow(t *testing.T) {
	f := newViaUpstreamFixture(t)
	timers := &manualTimers{}
	f.srv.taskCreateAfter = timers.after
	opDev := ep("operator-phone")
	store := &memPush{}
	claim(t, store, f.owner, f.dev)
	claim(t, store, authz.OperatorUserID, opDev)
	f.srv.ext.Push = push.NewService(store, f.pusher, push.VAPIDConfig{PublicKey: "k"}, log.New(io.Discard, "", 0))
	// The board's session acts as the fixture's owner, not the operator.
	f.srv.sessionUserOverride = func(*http.Request) int64 { return f.owner }

	createdBy := func(t *testing.T, id int64) string {
		t.Helper()
		var v *string
		if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT created_by FROM notes WHERE id = $1`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		if v == nil {
			return "<NULL>"
		}
		return *v
	}
	idByTitle := func(t *testing.T, title string) int64 {
		t.Helper()
		var id int64
		if err := f.pool.QueryRow(authz.System(context.Background()),
			`SELECT id FROM notes WHERE title = $1 OR body = $1 ORDER BY id DESC LIMIT 1`, title).Scan(&id); err != nil {
			t.Fatalf("no task %q: %v", title, err)
		}
		t.Cleanup(func() {
			_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM notes WHERE id = $1`, id)
		})
		return id
	}
	// fresh drains whatever an earlier subtest left pending, so each subtest
	// counts only its own create's push, pass or fail.
	fresh := func() {
		timers.fireAll()
		f.srv.WaitForPushes()
		f.pusher.reset()
	}
	createdPush := func(t *testing.T, dev, want string) {
		t.Helper()
		if n := timers.fireAll(); n != 1 {
			t.Fatalf("%d created-push timers, want 1", n)
		}
		f.srv.WaitForPushes()
		expectPushes(t, taskPushes(f.pusher, dev, "tasks-new"), want)
	}
	hookCreate := func(t *testing.T, title string, hdr map[string]string) int64 {
		t.Helper()
		h := map[string]string{"X-Muster-Token": connTestHookToken, "Content-Type": "application/json"}
		for k, v := range hdr {
			h[k] = v
		}
		rec := f.connFixture.call(t, http.MethodPost, "/api/tasks",
			strings.NewReader(`{"title":`+strconv.Quote(title)+`,"body":"b"}`), h)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /api/tasks: HTTP %d %s", rec.Code, rec.Body)
		}
		return idByTitle(t, title)
	}

	t.Run("the board: owner recorded, no creator named", func(t *testing.T) {
		fresh()
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("body", "Owner-filed task")
		_ = mw.Close()
		rec := f.connFixture.call(t, http.MethodPost, "/tasks", &buf, merge(f.operator(),
			map[string]string{"Content-Type": mw.FormDataContentType()}))
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /tasks: HTTP %d %s", rec.Code, rec.Body)
		}
		id := idByTitle(t, "Owner-filed task")
		if got := createdBy(t, id); got != "owner" {
			t.Fatalf("created_by = %s, want owner", got)
		}
		r, h := form(url.Values{"status": {"ready_for_review"}})
		if rec := f.connFixture.call(t, http.MethodPatch, "/tasks/"+strconv.FormatInt(id, 10)+"/status", r,
			merge(f.operator(), h)); rec.Code/100 != 2 {
			t.Fatalf("PATCH /tasks/%d/status: HTTP %d %s", id, rec.Code, rec.Body)
		}
		f.srv.WaitForPushes()
		expectPushes(t, taskPushes(f.pusher, f.dev, fmt.Sprintf("task-%d-done", id)),
			fmt.Sprintf("Task #%d ready for review | Owner-filed task", id))
	})

	t.Run("the chief's task_create_gated over /mcp: chief", func(t *testing.T) {
		fresh()
		m := f.deliveredMessage(t, f.owner, f.chief.ID, "creator-door-1")
		text, isErr, rpc := f.mcpCall(t, f.chiefTok, "task_create_gated",
			map[string]any{"title": "Gated by the chief", "body": "do it", "gate_reason": "approval", "message_id": m.ID})
		if rpc != nil || isErr {
			t.Fatalf("task_create_gated: rpc=%+v %s", rpc, text)
		}
		id := idByTitle(t, "Gated by the chief")
		if got := createdBy(t, id); got != "chief" {
			t.Fatalf("created_by = %s, want chief", got)
		}
		createdPush(t, f.dev, "New task | Gated by the chief (by chief)")
	})

	t.Run("a plugin's create: plugin:<label>", func(t *testing.T) {
		fresh()
		key := f.key(t)
		_, upID := notes.ParseTag(key)
		if code, out := f.post(t, f.tok, "/plugin/v1/tasks", map[string]any{"key": key,
			"create": map[string]any{"title": "Plugin-filed task", "body": "b"}}); code != http.StatusCreated {
			t.Fatalf("plugin create: HTTP %d %v", code, out["_raw"])
		}
		id := idByTitle(t, "Plugin-filed task")
		if got := createdBy(t, id); got != "plugin:Example tracker" {
			t.Fatalf("created_by = %s, want plugin:Example tracker", got)
		}
		createdPush(t, f.dev, "New task | Plugin-filed task (tracker "+upID+" · by plugin Example tracker)")
	})

	t.Run("the hook token with X-Muster-Actor naming the chief: nothing recorded", func(t *testing.T) {
		fresh()
		id := hookCreate(t, "Hook task claiming the chief", map[string]string{"X-Muster-Actor": f.chief.Name})
		if got := createdBy(t, id); got != "<NULL>" {
			t.Fatalf("created_by = %s, want NULL: the hook token names no caller", got)
		}
		createdPush(t, opDev, "New task | Hook task claiming the chief")
	})

	// The positive control for the case above: the same door, the same device,
	// and a producer it CAN name, so an empty fragment there is the door
	// recording nothing, not a push that cannot carry one.
	t.Run("the hook token with a producer: the producer, nothing recorded", func(t *testing.T) {
		fresh()
		id := hookCreate(t, "Drafted hook task", map[string]string{"X-Muster-Actor": f.chief.Name, headerSource: "drafter"})
		if got := createdBy(t, id); got != "<NULL>" {
			t.Fatalf("created_by = %s, want NULL", got)
		}
		createdPush(t, opDev, "New task | Drafted hook task (by Task drafter)")
	})
}

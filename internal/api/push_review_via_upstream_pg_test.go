package api

import (
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

// A status write on a plugin task REFERENCE is queued, and the row moves only
// when a snapshot reports it. The ready-for-review push then names the write's
// source THROUGH the upstream ("agent <name> via <ns>"), the owner's own write
// stays unlabelled, and only a change the upstream made itself reads
// "upstream (<ns>)". Driven end to end over Postgres: the real agent tool and
// owner route queue the write, the real plugin routes claim it, settle it and
// report the snapshot.
//
// ⚠ THIS FILE USES ONLY SYMBOLS THAT EXIST WITHOUT THE CORRELATION, so it
// compiles on the tree before it and goes red there for a behavioural reason
// (every snapshot move reads "upstream (tracker)").

// viaUpstreamFixture is the reference fixture with a push device on the owner.
type viaUpstreamFixture struct {
	*refFixture
	pusher *payloadPusher
	dev    string
}

func newViaUpstreamFixture(t *testing.T) *viaUpstreamFixture {
	t.Helper()
	f := &viaUpstreamFixture{refFixture: newRefFixture(t), pusher: &payloadPusher{}, dev: ep("owner-phone")}
	store := &memPush{}
	claim(t, store, f.owner, f.dev)
	f.srv.ext.Push = push.NewService(store, f.pusher, push.VAPIDConfig{PublicKey: "k"}, log.New(io.Discard, "", 0))
	return f
}

// titles is task id's ready-for-review push titles so far, oldest first.
func (f *viaUpstreamFixture) titles(id int64) []string {
	f.srv.WaitForPushes()
	return reviewTitles(f.pusher, f.dev, id)
}

func (f *viaUpstreamFixture) expect(t *testing.T, id int64, want ...string) {
	t.Helper()
	if got := f.titles(id); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("task %d's review push titles:\n%q\nwant:\n%q", id, got, want)
	}
}

// claimAll leases every pending write of the fixture plugin; returns their ids.
func (f *viaUpstreamFixture) claimAll(t *testing.T) []string {
	t.Helper()
	code, out := f.post(t, f.tok, "/plugin/v1/task-writes/claim", map[string]any{"wait_s": 0, "limit": 20})
	if code != http.StatusOK {
		t.Fatalf("claim: HTTP %d %v", code, out["_raw"])
	}
	var ids []string
	for _, w := range out["writes"].([]any) {
		ids = append(ids, strconv.FormatInt(int64(w.(map[string]any)["id"].(float64)), 10))
	}
	return ids
}

// settle reports write wid done, with snapshot snap when it is non-nil.
func (f *viaUpstreamFixture) settle(t *testing.T, wid string, snap map[string]any) {
	t.Helper()
	body := map[string]any{"ok": true}
	if snap != nil {
		body["reference"] = snap
	}
	if code, out := f.post(t, f.tok, "/plugin/v1/task-writes/"+wid+"/result", body); code != http.StatusOK || out["state"] != "done" {
		t.Fatalf("result for write %s: HTTP %d %v", wid, code, out["_raw"])
	}
}

// agentSets has a worker bound to reference id set its status through its
// native tool (agent_set_task_status), which queues on a reference.
func (f *viaUpstreamFixture) agentSets(t *testing.T, id int64, name, status string) string {
	t.Helper()
	w := f.byHand(t, notes.Note{ID: id}, name)
	w.NoteID = &id
	if text := f.srv.dispatchAgentTool(f.asOwner, w, "agent_set_task_status", `{"Status":"`+status+`"}`); !strings.Contains(text, `"queued":true`) {
		t.Fatalf("agent_set_task_status %s on reference %d did not queue: %s", status, id, text)
	}
	return w.Name
}

func TestAQueuedWriteConfirmedUpstreamNamesItsSource(t *testing.T) {
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)

	t.Run("an agent's write, confirmed by its result's snapshot, names the agent via the upstream", func(t *testing.T) {
		f := newViaUpstreamFixture(t)
		key := f.key(t)
		id := f.mustUpsert(t, key, refSnap("in_progress", "in progress", t0, nil))
		worker := f.agentSets(t, id, "refworker", notes.StatusReadyForReview)
		if st, _, _, _ := f.row(t, id); st != notes.StatusInProgress {
			t.Fatalf("the queued write moved the row: status %q", st)
		}
		f.expect(t, id) // nothing yet: the write is only queued
		ids := f.claimAll(t)
		if len(ids) != 1 {
			t.Fatalf("claimed %v, want the one queued write", ids)
		}
		f.settle(t, ids[0], refSnap("ready_for_review", "awaiting review", t0.Add(time.Minute), nil))
		if st, _, _, _ := f.row(t, id); st != notes.StatusReadyForReview {
			t.Fatalf("the confirming snapshot did not move the row: status %q", st)
		}
		f.expect(t, id, fmt.Sprintf("Task #%d ready for review · agent %s via tracker", id, worker))
	})

	t.Run("the owner's write, confirmed by a later reconcile snapshot, stays unlabelled", func(t *testing.T) {
		f := newViaUpstreamFixture(t)
		key := f.key(t)
		id := f.mustUpsert(t, key, refSnap("in_progress", "in progress", t0, nil))
		req := httptest.NewRequest(http.MethodPatch, "/tasks/"+strconv.FormatInt(id, 10)+"/status",
			strings.NewReader(url.Values{"status": {"ready_for_review"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if rec := f.serve(f.srv.handleNoteStatus, req, pv(id)); rec.Code != http.StatusAccepted {
			t.Fatalf("the owner's status on a reference: HTTP %d %s, want 202 (queued)", rec.Code, rec.Body)
		}
		ids := f.claimAll(t)
		if len(ids) != 1 {
			t.Fatalf("claimed %v, want the one queued write", ids)
		}
		f.settle(t, ids[0], nil) // done, no snapshot: the reconcile reports it
		f.mustUpsert(t, key, refSnap("ready_for_review", "awaiting review", t0.Add(time.Minute), nil))
		f.expect(t, id, fmt.Sprintf("Task #%d ready for review", id))
	})

	// 🔴 THE STATUS MUST MATCH. The agent asked for in_progress; the upstream
	// then set ready_for_review itself before the write landed. That move is
	// the upstream's, not the agent's.
	t.Run("an upstream change to a different status after a queued write stays the upstream's", func(t *testing.T) {
		f := newViaUpstreamFixture(t)
		key := f.key(t)
		id := f.mustUpsert(t, key, refSnap("open", "to do", t0, nil))
		f.agentSets(t, id, "slowworker", notes.StatusInProgress)
		f.claimAll(t) // leased, not yet applied upstream
		f.mustUpsert(t, key, refSnap("ready_for_review", "awaiting review", t0.Add(time.Minute), nil))
		f.expect(t, id, fmt.Sprintf("Task #%d ready for review · upstream (tracker)", id))
	})

	// 🔴 A WRITE CONFIRMS ONE MOVE. After the agent's ready_for_review landed,
	// the upstream moved the task away and back by itself: the second push is
	// the upstream's, not the agent's again.
	t.Run("a confirmed write does not name a later upstream move back to its status", func(t *testing.T) {
		f := newViaUpstreamFixture(t)
		key := f.key(t)
		id := f.mustUpsert(t, key, refSnap("in_progress", "in progress", t0, nil))
		worker := f.agentSets(t, id, "onceworker", notes.StatusReadyForReview)
		ids := f.claimAll(t)
		if len(ids) != 1 {
			t.Fatalf("claimed %v, want the one queued write", ids)
		}
		f.settle(t, ids[0], refSnap("ready_for_review", "awaiting review", t0.Add(time.Minute), nil))
		f.mustUpsert(t, key, refSnap("in_progress", "reopened", t0.Add(2*time.Minute), nil))
		f.mustUpsert(t, key, refSnap("ready_for_review", "awaiting review", t0.Add(3*time.Minute), nil))
		f.expect(t, id,
			fmt.Sprintf("Task #%d ready for review · agent %s via tracker", id, worker),
			fmt.Sprintf("Task #%d ready for review · upstream (tracker)", id))
	})

	// 🔴 A MOVE SPENDS A WRITE ALREADY DONE UPSTREAM, matched or not. The
	// agent asked for the status the task already had, so its landing moved
	// nothing; the upstream's later move away and back is the upstream's.
	t.Run("a done write that moved nothing does not name a later upstream move", func(t *testing.T) {
		f := newViaUpstreamFixture(t)
		key := f.key(t)
		id := f.mustUpsert(t, key, refSnap("ready_for_review", "awaiting review", t0, nil))
		f.agentSets(t, id, "lateworker", notes.StatusReadyForReview)
		ids := f.claimAll(t)
		if len(ids) != 1 {
			t.Fatalf("claimed %v, want the one queued write", ids)
		}
		f.settle(t, ids[0], refSnap("ready_for_review", "awaiting review", t0.Add(time.Minute), nil))
		f.mustUpsert(t, key, refSnap("in_progress", "reopened", t0.Add(2*time.Minute), nil))
		f.mustUpsert(t, key, refSnap("ready_for_review", "awaiting review", t0.Add(3*time.Minute), nil))
		f.expect(t, id,
			fmt.Sprintf("Task #%d ready for review · upstream (tracker)", id),
			fmt.Sprintf("Task #%d ready for review · upstream (tracker)", id))
	})

	// A write the plugin reports FAILED did not land: the upstream's own move
	// to that status is the upstream's.
	t.Run("a failed write does not name the upstream's move", func(t *testing.T) {
		f := newViaUpstreamFixture(t)
		key := f.key(t)
		id := f.mustUpsert(t, key, refSnap("in_progress", "in progress", t0, nil))
		f.agentSets(t, id, "failworker", notes.StatusReadyForReview)
		ids := f.claimAll(t)
		if len(ids) != 1 {
			t.Fatalf("claimed %v, want the one queued write", ids)
		}
		if code, out := f.post(t, f.tok, "/plugin/v1/task-writes/"+ids[0]+"/result",
			map[string]any{"ok": false, "error": "the upstream refused it"}); code != http.StatusOK || out["state"] != "failed" {
			t.Fatalf("failed result: HTTP %d %v", code, out["_raw"])
		}
		f.mustUpsert(t, key, refSnap("ready_for_review", "awaiting review", t0.Add(time.Minute), nil))
		f.expect(t, id, fmt.Sprintf("Task #%d ready for review · upstream (tracker)", id))
	})

	// ⚠ An invariant guard, not regression coverage: a move with no queued
	// write read "upstream (tracker)" before the correlation too.
	t.Run("a pure upstream change names the upstream", func(t *testing.T) {
		f := newViaUpstreamFixture(t)
		key := f.key(t)
		id := f.mustUpsert(t, key, refSnap("in_progress", "in progress", t0, nil))
		f.mustUpsert(t, key, refSnap("ready_for_review", "awaiting review", t0.Add(time.Minute), nil))
		f.expect(t, id, fmt.Sprintf("Task #%d ready for review · upstream (tracker)", id))
	})
}

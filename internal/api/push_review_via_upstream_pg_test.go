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

	"github.com/ZacxDev/muster/internal/agents"
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
	f.agentSetsAs(t, w, id, status)
	return w.Name
}

// agentSetsAs has the already-created worker w queue status on reference id.
func (f *viaUpstreamFixture) agentSetsAs(t *testing.T, w agents.Agent, id int64, status string) {
	t.Helper()
	if text := f.srv.dispatchAgentTool(f.asOwner, w, "agent_set_task_status", `{"Status":"`+status+`"}`); !strings.Contains(text, `"queued":true`) {
		t.Fatalf("agent_set_task_status %s on reference %d did not queue: %s", status, id, text)
	}
}

// settleAllWithoutSnapshot claims and reports done, with no snapshot, every
// write queued so far — one claim at a time, as the per-task order hands them
// out — and returns how many it settled.
func (f *viaUpstreamFixture) settleAllWithoutSnapshot(t *testing.T) int {
	t.Helper()
	n := 0
	for i := 0; i < 20; i++ {
		ids := f.claimAll(t)
		if len(ids) == 0 {
			return n
		}
		for _, wid := range ids {
			f.settle(t, wid, nil)
			n++
		}
	}
	t.Fatal("settleAllWithoutSnapshot: still claiming after 20 rounds")
	return n
}

// unconfirmed is how many of task id's status writes no snapshot move has
// confirmed or spent yet (task_ref_writes.confirmed_at IS NULL).
func (f *viaUpstreamFixture) unconfirmed(t *testing.T, id int64) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(f.asOwner, `SELECT count(*) FROM task_ref_writes
		WHERE note_id = $1 AND kind = 'status' AND confirmed_at IS NULL`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
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

	// 🔴 A MOVE DOES NOT SPEND A DONE WRITE WHOSE OWN SNAPSHOT IS STILL TO COME
	// (audit round 1). The agent queued in_progress then ready_for_review; the
	// plugin applied both and reported each done without a snapshot; the
	// in_progress snapshot then arrived late. That move confirms the
	// in_progress write, and must leave the later ready_for_review write for
	// the snapshot that reports it. Then the upstream moves the task away and
	// back by itself: that one is the upstream's.
	t.Run("a late snapshot of an earlier write does not spend a later write's confirmation", func(t *testing.T) {
		f := newViaUpstreamFixture(t)
		key := f.key(t)
		id := f.mustUpsert(t, key, refSnap("open", "to do", t0, nil))
		w := f.byHand(t, notes.Note{ID: id}, "twostepworker")
		w.NoteID = &id
		f.agentSetsAs(t, w, id, notes.StatusInProgress)
		f.agentSetsAs(t, w, id, notes.StatusReadyForReview)
		if n := f.settleAllWithoutSnapshot(t); n != 2 {
			t.Fatalf("settled %d writes, want the two queued", n)
		}
		f.mustUpsert(t, key, refSnap("in_progress", "in progress", t0.Add(time.Minute), nil))
		f.mustUpsert(t, key, refSnap("ready_for_review", "awaiting review", t0.Add(2*time.Minute), nil))
		f.mustUpsert(t, key, refSnap("in_progress", "reopened", t0.Add(3*time.Minute), nil))
		f.mustUpsert(t, key, refSnap("ready_for_review", "awaiting review", t0.Add(4*time.Minute), nil))
		f.expect(t, id,
			fmt.Sprintf("Task #%d ready for review · agent %s via tracker", id, w.Name),
			fmt.Sprintf("Task #%d ready for review · upstream (tracker)", id))
	})

	// The NEWEST unconfirmed write for the status is the one a move confirms,
	// and the older one is spent with it: the upstream's later move away and
	// back names neither agent.
	t.Run("of two unconfirmed writes for the same status the newest wins", func(t *testing.T) {
		f := newViaUpstreamFixture(t)
		key := f.key(t)
		id := f.mustUpsert(t, key, refSnap("in_progress", "in progress", t0, nil))
		f.agentSets(t, id, "firstworker", notes.StatusReadyForReview)
		second := f.agentSets(t, id, "secondworker", notes.StatusReadyForReview)
		if n := f.settleAllWithoutSnapshot(t); n != 2 {
			t.Fatalf("settled %d writes, want the two queued", n)
		}
		f.mustUpsert(t, key, refSnap("ready_for_review", "awaiting review", t0.Add(time.Minute), nil))
		f.mustUpsert(t, key, refSnap("in_progress", "reopened", t0.Add(2*time.Minute), nil))
		f.mustUpsert(t, key, refSnap("ready_for_review", "awaiting review", t0.Add(3*time.Minute), nil))
		f.expect(t, id,
			fmt.Sprintf("Task #%d ready for review · agent %s via tracker", id, second),
			fmt.Sprintf("Task #%d ready for review · upstream (tracker)", id))
	})

	// 🔴 A RE-SAVE OF THE STATUS THE ROW ALREADY HAS IS NOT A MOVE, AND
	// CONFIRMS NOTHING. Two identical snapshots raced: both read the old
	// status, the first moved the row and confirmed the newest write, and the
	// second's SetStatus replaced the status with itself. Called here at
	// setNoteStatus — the step both racers reach — so the order is
	// deterministic. Before the guard the second took the older write.
	t.Run("a second identical snapshot does not take an older write", func(t *testing.T) {
		f := newViaUpstreamFixture(t)
		key := f.key(t)
		id := f.mustUpsert(t, key, refSnap("in_progress", "in progress", t0, nil))
		f.agentSets(t, id, "olderworker", notes.StatusReadyForReview)
		f.agentSets(t, id, "newerworker", notes.StatusReadyForReview)
		at := t0.Add(time.Minute)
		f.mustUpsert(t, key, refSnap("ready_for_review", "awaiting review", at, nil))
		if n := f.unconfirmed(t, id); n != 1 {
			t.Fatalf("after the first snapshot %d writes unconfirmed, want 1 (the older, still pending)", n)
		}
		if _, err := f.srv.setNoteStatus(notes.OnlyIfSnapshotAt(f.asOwner, at), writerPluginReference, id, notes.StatusReadyForReview); err != nil {
			t.Fatalf("the second, identical snapshot's status: %v", err)
		}
		if n := f.unconfirmed(t, id); n != 1 {
			t.Fatalf("after the identical second snapshot %d writes unconfirmed, want 1: a re-save took the older write", n)
		}
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

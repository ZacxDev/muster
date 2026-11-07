package api

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/stepup"
)

// task_forward INTO A HOST-SPAWNED WINDOW (host_forward.go): a chief-forward
// send-queue row, addressed by the dispatch record, gated at delivery and
// pasted with the dispatch for the host's own tag check.

// runningOnTask starts a host dispatch on a new task, lists its window (the
// row is then running) and returns the task, the row and the window.
func (f *hostFixture) runningOnTask(t *testing.T) (int64, plugins.HostDispatch, mirror.Window) {
	t.Helper()
	f.srv.ext.SendQueue = sendqueue.NewPG(f.pool)
	task := f.task(t, "Forward me")
	f.ownerTurn(t, f.chief, f.owner, "Do it mirrored on host-bravo.")
	args := hostCall("host/host-bravo/claude", "", true)
	delete(args, "request_key")
	args["task_id"] = task.ID
	id := hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, args))
	win := mirror.Window{Session: "agents", ID: "@" + strconv.FormatInt(id, 10), Claude: true, Dispatch: id,
		ClaudeSession: "0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b", Status: mirror.StatusIdle, StatusSince: 1}
	f.listWindows(win)
	f.srv.reconcileListing(context.Background(), f.owner, "host-bravo", []mirror.Window{win}, time.Now())
	r := f.row(t, id)
	if r.Status != plugins.HostDispatchRunning {
		t.Fatalf("not running: %+v", r)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM send_queue WHERE owner_id = $1`, f.owner)
	})
	return task.ID, r, win
}

// listWindows makes host-bravo list ws (the hub's view the reachability and the
// gate read).
func (f *hostFixture) listWindows(ws ...mirror.Window) {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	for i := range f.m.hosts {
		if f.m.hosts[i].Host == "host-bravo" {
			f.m.hosts[i].Windows = mirror.NormalizeWindows(ws, 0)
		}
	}
}

func (f *hostFixture) forwardRows(t *testing.T, window string) []sendqueue.Item {
	t.Helper()
	items, err := sendqueue.NewPG(f.pool).List(f.asOwner, sendqueue.Target{Host: "host-bravo", Session: "agents", Window: window})
	if err != nil {
		t.Fatal(err)
	}
	var out []sendqueue.Item
	for _, it := range items {
		if it.ChiefForwardDispatch != 0 {
			out = append(out, it)
		}
	}
	return out
}

// TestTaskForwardQueuesAChiefForwardForAHostAgent: with no pod on the task,
// task_forward queues ONE chief-forward row for the host dispatch's window,
// answering where it went. It never targets a window whose listing lacks the
// row's dispatch, or whose session differs: then nothing is queued.
func TestTaskForwardQueuesAChiefForwardForAHostAgent(t *testing.T) {
	f := newHostFixture(t)
	taskID, r, win := f.runningOnTask(t)
	bad := win
	bad.Dispatch = 0
	f.listWindows(bad)
	if out := f.callRaw(f.chief, taskForwardTool, map[string]any{"task_id": taskID, "note": "a comment arrived"}); !strings.Contains(out, "no running agent works task") {
		t.Fatalf("an untagged window was a forward target: %s", out)
	}
	other := win
	other.ClaudeSession = "1c3d4e5f-6071-4829-9bac-1d2e3f4a5b6c"
	f.listWindows(other)
	if out := f.callRaw(f.chief, taskForwardTool, map[string]any{"task_id": taskID, "note": "a comment arrived"}); !strings.Contains(out, "no running agent works task") {
		t.Fatalf("a window running another session was a forward target: %s", out)
	}
	if n := len(f.forwardRows(t, r.Window)); n != 0 {
		t.Fatalf("%d rows queued for unreachable windows", n)
	}
	f.listWindows(win)
	out := f.callRaw(f.chief, taskForwardTool, map[string]any{"task_id": taskID, "note": "a comment arrived"})
	if !strings.Contains(out, `"dispatch":`+strconv.FormatInt(r.ID, 10)) || !strings.Contains(out, "muster delivers it once that agent is idle") {
		t.Fatalf("task_forward: %s", out)
	}
	rows := f.forwardRows(t, r.Window)
	if len(rows) != 1 || rows[0].ChiefForwardDispatch != r.ID || rows[0].Target.Window != r.Window || !strings.Contains(rows[0].Body, "a comment arrived") {
		t.Fatalf("rows %+v", rows)
	}
}

// TestAChiefForwardIsGatedAtDeliveryAndCarriesItsDispatch: delivered, the
// paste carries the dispatch (the host's tag check needs it); a row more than
// 10 minutes old, a row whose dispatch ended, and a row whose window no longer
// matches are each refused with their own detail and never pasted.
func TestAChiefForwardIsGatedAtDeliveryAndCarriesItsDispatch(t *testing.T) {
	f := newHostFixture(t)
	taskID, r, win := f.runningOnTask(t)
	f.callRaw(f.chief, taskForwardTool, map[string]any{"task_id": taskID, "note": "first"})
	it := f.forwardRows(t, r.Window)[0]
	ctx := stepup.WithAuthTime(f.asOwner, time.Now())
	if o := f.srv.sendQueueDeliver(ctx, it); !o.Sent {
		t.Fatalf("delivery refused: %+v", o)
	}
	f.m.mu.Lock()
	pastes := slices.Clone(f.m.pastes)
	f.m.mu.Unlock()
	if len(pastes) != 1 || pastes[0].dispatch != r.ID || pastes[0].window != r.Window || pastes[0].session != "agents" {
		t.Fatalf("the paste: %+v", pastes)
	}
	// Too old.
	old := it
	old.CreatedAt = time.Now().Add(-11 * time.Minute)
	if why := f.srv.sendQueueChiefForwardGate(f.asOwner, old); why != chiefForwardStale {
		t.Fatalf("an 11-minute-old forward: %q", why)
	}
	// The window no longer the dispatch's.
	moved := win
	moved.ID = "@999"
	f.listWindows(moved)
	if why := f.srv.sendQueueChiefForwardGate(f.asOwner, it); !strings.Contains(why, "not the one this forward was queued for") {
		t.Fatalf("a moved window: %q", why)
	}
	f.listWindows(win)
	// The dispatch ended.
	if _, err := f.srv.endHostDispatch(context.Background(), f.owner, r.ID, "window gone", false); err != nil {
		t.Fatal(err)
	}
	if why := f.srv.sendQueueChiefForwardGate(f.asOwner, it); !strings.Contains(why, "has ended") {
		t.Fatalf("an ended dispatch: %q", why)
	}
	if o := f.srv.sendQueueDeliver(ctx, it); o.Sent {
		t.Fatal("a forward for an ended dispatch was delivered")
	}
	f.m.mu.Lock()
	n := len(f.m.pastes)
	f.m.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d pastes; refused forwards were pasted", n)
	}
}

// TestOnlyTaskForwardSetsChiefForwardDispatch: the ledger — the only code in
// internal/api that sets ChiefForwardDispatch on a send-queue row is
// forwardToHostDispatch (toolTaskForward's host branch).
func TestOnlyTaskForwardSetsChiefForwardDispatch(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	var got []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		if strings.Contains(string(b), "ChiefForwardDispatch:") {
			got = append(got, f)
		}
	}
	if !slices.Equal(got, []string{"host_forward.go"}) {
		t.Fatalf("ChiefForwardDispatch is set in %v; want only host_forward.go", got)
	}
	assertCallers(t, ".", "forwardToHostDispatch", []string{"task_routing.go:toolTaskForward"})
}

// TestAChiefForwardIsNeverEditedOrRetried: a chief-forward row is muster's own
// message, like a route's or a nudge's: the edit is refused with the routed
// sentence, and the Retry predicate is false. The control: the same row with
// no origin is editable.
func TestAChiefForwardIsNeverEditedOrRetried(t *testing.T) {
	fwd := sendqueue.Item{ChiefForwardDispatch: 7, State: sendqueue.Queued}
	if msg, _ := sendQueueEditRefusal(fwd); msg != SendQueueRoutedNotEditable {
		t.Fatalf("a forward row's edit refusal: %q", msg)
	}
	if msg, _ := sendQueueEditRefusal(sendqueue.Item{State: sendqueue.Queued}); msg == SendQueueRoutedNotEditable {
		t.Fatal("control: a person's row reads as muster's own")
	}
	failed := fwd
	failed.State = sendqueue.Failed
	if sendQueueRetryable(failed) {
		t.Fatal("a failed forward row is retryable")
	}
}

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// THE PINNED LANE (X-Muster-CC-Pinned: 1, lanes.go): the chief's primary chat,
// which every delivery goes to, is never stopped for another session and never
// refused at the cap (PR #609 round 1 F1).

// turnFor posts one ordinary turn for key, pinned or not, and returns the
// status and body.
func (r *laneRig) turnFor(t *testing.T, key string, pinned bool) (int, string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"input":"a delivery","stream":false}`))
	req.Header.Set("Authorization", "Bearer b")
	req.Header.Set(headerCCSession, key)
	if pinned {
		req.Header.Set(headerCCPinned, "1")
	}
	w := httptest.NewRecorder()
	r.s.gatewayHandler().ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// TestAPinnedLaneIsNeverEvictedNorCountedAgainstTheCap: at a cap of one, the
// pinned primary leaves room for an operator lane; a second operator lane
// evicts the first operator lane — the idle one — never the pinned one, though
// the pinned one is the least recently used.
func TestAPinnedLaneIsNeverEvictedNorCountedAgainstTheCap(t *testing.T) {
	r := newLaneRig(t, 1)
	ctx := context.Background()
	p, f := r.l.acquireIn(ctx, "chief-primary", "", "", laneMode{pinned: true})
	if f != nil {
		t.Fatalf("the pinned lane: %v", f)
	}
	r.l.release(p)
	started(t, p)
	a := r.operator(t, "side-a") // not refused: the pinned lane is not counted
	if r.l.get(p.id) != p {
		t.Fatal("starting an operator lane stopped the pinned one")
	}
	b := r.operator(t, "side-b") // at the cap: evicts an UNPINNED idle lane
	if r.l.get(p.id) != p || r.pane(p.key).kills() != 0 {
		t.Errorf("PINNED LANE EVICTED: in set=%v kills=%d", r.l.get(p.id) == p, r.pane(p.key).kills())
	}
	if r.l.get(a.id) != nil || r.l.get(b.id) != b {
		t.Errorf("the cap was not kept among the unpinned: a in set=%v, b in set=%v", r.l.get(a.id) != nil, r.l.get(b.id) == b)
	}
}

// TestAPinnedTurnGetsItsLaneWithEveryOtherLaneMidTurn: every unpinned lane is
// held by a turn (none can be evicted). An ordinary turn for a new session is
// refused with the typed 503 capacity — and starts no CLI — while a turn for
// the PINNED primary still gets its lane (it is not refused for capacity).
func TestAPinnedTurnGetsItsLaneWithEveryOtherLaneMidTurn(t *testing.T) {
	r := newLaneRig(t, 2)
	ctx := context.Background()
	var held []*lane
	for _, k := range []string{"side-a", "side-b"} {
		ln, f := r.l.acquire(ctx, k, "")
		if f != nil {
			t.Fatal(f)
		}
		held = append(held, ln) // reserved: mid-turn, never evicted
	}
	defer func() {
		for _, ln := range held {
			r.l.release(ln)
		}
	}()
	code, body := r.turnFor(t, "side-c", false)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, `"type":"`+failCapacity+`"`) {
		t.Fatalf("precondition: an ordinary turn at the cap answered %d %s, want 503 %s", code, body, failCapacity)
	}
	code, body = r.turnFor(t, "chief-primary", true)
	if strings.Contains(body, `"type":"`+failCapacity+`"`) {
		t.Fatalf("THE PRIMARY WAS REFUSED AT THE CAP: %d %s", code, body)
	}
	if r.pane("chief-primary") == nil {
		t.Error("the pinned turn started no CLI")
	}
}

// TestACapacityRefusalPastesNothing is what lets muster read a capacity refusal
// as NOT SENT (agentgateway.UnsentCapacity): ccd answers it before any lane
// exists for the session — no CLI is started, so nothing can be pasted into
// one — and the lanes that are running are not touched.
func TestACapacityRefusalPastesNothing(t *testing.T) {
	r := newLaneRig(t, 1)
	held, f := r.l.acquire(context.Background(), "side-a", "")
	if f != nil {
		t.Fatal(f)
	}
	defer r.l.release(held)
	started(t, held)
	code, body := r.turnFor(t, "side-b", false)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, `"type":"`+failCapacity+`"`) {
		t.Fatalf("precondition: %d %s, want 503 %s", code, body, failCapacity)
	}
	if r.pane("side-b") != nil {
		t.Error("A CAPACITY REFUSAL STARTED A CLI for the refused session: it could have pasted into it")
	}
	if r.l.get(laneID("side-b")) != nil {
		t.Error("the refused session has a lane")
	}
	if r.l.get(held.id) != held || r.pane(held.key).kills() != 0 {
		t.Error("the refusal touched the running lane")
	}
}

// TestOneLaneIsPinnedAtATime: a turn pinning another lane unpins the first (a
// fresh primary's old lane), which can then be evicted like any other; a turn
// for a lane without the header unpins it.
func TestOneLaneIsPinnedAtATime(t *testing.T) {
	r := newLaneRig(t, 1)
	ctx := context.Background()
	old, _ := r.l.acquireIn(ctx, "old-primary", "", "", laneMode{pinned: true})
	r.l.release(old)
	started(t, old)
	fresh, _ := r.l.acquireIn(ctx, "new-primary", "", "", laneMode{pinned: true})
	r.l.release(fresh)
	started(t, fresh)
	r.l.mu.Lock()
	oldPinned, freshPinned := old.pinned, fresh.pinned
	r.l.mu.Unlock()
	if oldPinned || !freshPinned {
		t.Fatalf("after the new primary's turn: old pinned=%v, new pinned=%v; want only the new", oldPinned, freshPinned)
	}
	r.operator(t, "side-a") // at the cap among the unpinned: evicts the old primary
	if r.l.get(old.id) != nil {
		t.Error("the unpinned old primary was not evictable")
	}
	if r.l.get(fresh.id) != fresh {
		t.Error("the pinned new primary was evicted")
	}
	again, _ := r.l.acquire(ctx, "new-primary", "")
	r.l.release(again)
	r.l.mu.Lock()
	defer r.l.mu.Unlock()
	if again.pinned {
		t.Error("a turn without the header left the lane pinned")
	}
}

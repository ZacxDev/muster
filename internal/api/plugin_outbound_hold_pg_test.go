package api

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/officers"
)

// A rejection past the draft.refused bound is HELD, never dropped: it is
// filed as a draft.refused item that is not claimable while the officer
// already has maxOpenRefusedItems claimable ones, and the officer sweep
// delivers it (makes it claimable) once one of those is finished.
//
// The expected strings are written out here, not read from the code under
// test.

// refusedCount is how many draft.refused items, in any state, draft id has.
func (w *outboundWorld) refusedCount(id int64) int {
	w.t.Helper()
	var n int
	if err := w.f.pool.QueryRow(authz.System(w.ctx), `SELECT count(*) FROM officer_items WHERE kind = 'draft.refused' AND subject_key = $1`,
		"draft:"+strconv.FormatInt(id, 10)).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

// claimItem is the instance's officer_claim of item; ok false when refused
// — an error, or the non-error "claimed": false of a blocked item.
func (w *outboundWorld) claimItem(item int64) (string, bool) {
	w.t.Helper()
	out := w.srv.dispatchAgentTool(w.ctx, w.agent, officers.ToolClaim, `{"item_id":"`+strconv.FormatInt(item, 10)+`"}`)
	return out, !isToolErr(out) && !strings.Contains(out, `"claimed":false`)
}

// TestARejectionPastTheBoundIsHeldAndDeliveredLater is the regression: the
// officer has 3 open draft.refused items and the owner rejects a 4th draft.
// Before the fix the rejection was dropped ("Its officer was not told") and
// no draft.refused item ever existed for it. Now:
//   - it is filed, HELD (not claimable), and the item the draft was for is
//     finished naming it, saying it is held — never "not told";
//   - a sweep while the 3 are still open delivers nothing (the bound holds);
//   - once one of the 3 is finished, the next sweep delivers it: claimable;
//   - further sweeps change nothing, and it exists exactly once.
func TestARejectionPastTheBoundIsHeldAndDeliveredLater(t *testing.T) {
	w := newOutboundWorld(t, true)
	w.fillRefusedBound()
	item, at := w.fileForItem("x:held", "Rejected at the bound.")
	if rec := w.rejectForm(at, "not this week"); rec.Code != http.StatusSeeOther {
		t.Fatalf("reject: %d %s", rec.Code, rec.Body.String())
	}
	held, p, ok := w.refusedItemOf(at)
	if !ok {
		t.Fatal("the 4th rejection was LOST: no draft.refused item exists for it")
	}
	if p["status"] != "rejected" || p["by"] != "owner" || p["detail"] != "not this week" {
		t.Fatalf("the held draft.refused payload: %v", p)
	}
	st, outcome, summary, _ := w.itemRow(item)
	want := "draft " + strconv.FormatInt(at, 10) + " was rejected by the owner, not posted. Its officer already has 3 open draft.refused items, " +
		"so the work is held on item " + strconv.FormatInt(held, 10) + " and delivered to it when one of them is finished."
	if st != officers.StatusDone || outcome != "no_action" || summary != want {
		t.Fatalf("the item: %s %s %q; want done no_action %q", st, outcome, summary, want)
	}
	if strings.Contains(summary, "not told") {
		t.Fatalf("the item still says the officer was not told: %q", summary)
	}
	if out, ok := w.claimItem(held); ok {
		t.Fatalf("a HELD draft.refused item was claimable at the bound: %s", out)
	} else if !strings.Contains(out, `"held":true`) || !strings.Contains(out, "is held") || strings.Contains(out, "tasks it waits on") {
		t.Fatalf("the held item's claim refusal: %s; want it to say the item is held, not that it waits on tasks", out)
	}
	w.srv.SweepOfficerAttention(w.ctx)
	if out, ok := w.claimItem(held); ok {
		t.Fatalf("a sweep delivered the held item while 3 were still open: %s", out)
	}

	// The officer works one of the three to the end.
	prior, _, ok := w.refusedItemOfSubject("draft:prior-0")
	if !ok {
		t.Fatal("no prior draft.refused item")
	}
	if out, ok := w.claimItem(prior); !ok {
		t.Fatalf("claim prior: %s", out)
	}
	if out := w.srv.dispatchAgentTool(w.ctx, w.agent, officers.ToolComplete, `{"item_id":"`+strconv.FormatInt(prior, 10)+`","outcome":"done","summary":"redrafted"}`); isToolErr(out) {
		t.Fatalf("complete prior: %s", out)
	}

	// The delivering sweep runs WITHOUT an attention store: delivery must not
	// depend on one.
	att := w.srv.ext.Attention
	w.srv.ext.Attention = nil
	w.srv.SweepOfficerAttention(w.ctx)
	w.srv.ext.Attention = att
	if out, ok := w.claimItem(held); !ok {
		t.Fatalf("one of the 3 finished and the sweep ran, but the held rejection was not delivered: %s", out)
	}
	w.srv.SweepOfficerAttention(w.ctx)
	w.srv.SweepOfficerAttention(w.ctx)
	if n := w.refusedCount(at); n != 1 {
		t.Fatalf("%d draft.refused items for draft %d; want exactly 1", n, at)
	}
	if st, _, _, by := w.itemRow(held); st != officers.StatusClaimed || by == "" {
		t.Fatalf("the delivered item: %s held by %q; want claimed by the instance", st, by)
	}
}

// refusedItemOfSubject is the draft.refused item of subject: id, status, ok.
func (w *outboundWorld) refusedItemOfSubject(subject string) (int64, string, bool) {
	w.t.Helper()
	var id int64
	var st string
	err := w.f.pool.QueryRow(authz.System(w.ctx), `SELECT id, status FROM officer_items WHERE kind = 'draft.refused' AND subject_key = $1`, subject).Scan(&id, &st)
	return id, st, err == nil
}

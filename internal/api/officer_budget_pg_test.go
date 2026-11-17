package api

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/plugins"
)

// ---------------------------------------------------------------------------
// The seam between a builtin officer's daily budget / routine batching
// (internal/officers budget.go, builtin budget.go) and the server: who writes
// officer_items.origin, and that EnableOfficers installs the store's routine
// policy from the live roster.
// ---------------------------------------------------------------------------

const budgetedDecl = `[{"id": "` + builtinOfficerID + `", "backend": "builtin", "daily_budget_usd": 10,
  "routine_kinds": ["plugin.note", "plugin.snapshot"], "routine_wake_at": "09:00"}]`

type itemFacts struct {
	origin    string
	notBefore *time.Time
}

func itemFactsOf(t *testing.T, f *connFixture, id int64) itemFacts {
	t.Helper()
	var it itemFacts
	if err := f.pool.QueryRow(dbtest.AsOperator(context.Background()), `SELECT origin, not_before FROM officer_items WHERE id = $1`, id).
		Scan(&it.origin, &it.notBefore); err != nil {
		t.Fatal(err)
	}
	return it
}

// TestTheOwnersItemsAreMarkedAndARoutineItemWaitsForTheDailyWake: through the
// server as it boots an officer with routine_kinds — the admin API (what
// `muster officer item enqueue` calls) marks what it enqueues the owner's,
// routine kind or not, and a routine kind still waits for 09:00 UTC; a
// plugin's message becoming an item is NOT the owner's, and its routine kind
// waits too (EnableOfficers installed the policy: without it nothing waits).
func TestTheOwnersItemsAreMarkedAndARoutineItemWaitsForTheDailyWake(t *testing.T) {
	f := newConnFixture(t)
	srv, store := builtinOfficerServer(t, f, budgetedDecl, &bytes.Buffer{})
	h := srv.Handler()
	if code, out := adminCall(t, h, "PUT", "/api/officers/types/marketing", `{"name":"Marketing","charter":"Draft."}`); code != 200 {
		t.Fatalf("put type: %d %v", code, out)
	}
	if code, out := adminCall(t, h, "PUT", "/api/officers/defs/"+builtinOfficerID, `{}`); code != 200 {
		t.Fatalf("put def: %d %v", code, out)
	}
	enqueue := func(body string) int64 {
		t.Helper()
		code, out := adminCall(t, h, "POST", "/api/officers/defs/"+builtinOfficerID+"/items", body)
		if code != 201 {
			t.Fatalf("enqueue %s: %d %v", body, code, out)
		}
		return int64(out["id"].(float64))
	}
	inWindow := func(nb *time.Time, from time.Time) bool {
		return nb != nil && nb.After(from) && nb.Sub(from) <= 24*time.Hour && nb.UTC().Hour() == 9 && nb.UTC().Minute() == 0 && nb.UTC().Second() == 0
	}

	start := time.Now()
	refused := itemFactsOf(t, f, enqueue(`{"kind":"draft.refused","subject_key":"d1","payload":{"by":"owner"}}`))
	if refused.origin != officers.OriginOwner || refused.notBefore != nil {
		t.Fatalf("the admin API's draft.refused: %+v, want origin owner and due now", refused)
	}
	note := itemFactsOf(t, f, enqueue(`{"kind":"plugin.note","subject_key":"n1"}`))
	if note.origin != officers.OriginOwner || !inWindow(note.notBefore, start) {
		t.Fatalf("the admin API's plugin.note: %+v, want origin owner and not_before the next 09:00 UTC", note)
	}

	link := officerPluginLink{store: store}
	tx, err := f.pool.Begin(dbtest.AsOperator(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	snapID, err := link.EnqueueTx(dbtest.AsOperator(context.Background()), tx, 1, plugins.OfficerItem{Officer: builtinOfficerID, Kind: "plugin.snapshot", SubjectKey: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	replyID, err := link.EnqueueTx(dbtest.AsOperator(context.Background()), tx, 1, plugins.OfficerItem{Officer: builtinOfficerID, Kind: "plugin.reply", SubjectKey: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(dbtest.AsOperator(context.Background())); err != nil {
		t.Fatal(err)
	}
	if snap := itemFactsOf(t, f, snapID); snap.origin != "" || !inWindow(snap.notBefore, start) {
		t.Fatalf("a plugin's plugin.snapshot: %+v, want no origin and not_before the next 09:00 UTC", snap)
	}
	if reply := itemFactsOf(t, f, replyID); reply.origin != "" || reply.notBefore != nil {
		t.Fatalf("a plugin's plugin.reply: %+v, want no origin and due now", reply)
	}
}

// TestOnlyTheOwnersRejectionIsTheOwnersFollowUp: a draft the OWNER rejected
// files a draft.refused item that is the owner's (it bypasses a spent
// budget); one the plugin refused, or muster expired, is not.
func TestOnlyTheOwnersRejectionIsTheOwnersFollowUp(t *testing.T) {
	d := plugins.Draft{ID: 41}
	for _, tc := range []struct {
		how  plugins.OutboundEnd
		by   string
		want string
	}{
		{"", settledByOwner, officers.OriginOwner},
		{"", settledByPlugin, ""},
		{plugins.OutboundExpired, "", ""},
	} {
		e := draftEndFor(d, builtinOfficerID, tc.how, tc.by, "why")
		if e.FollowUp == nil {
			t.Fatalf("%q/%q: no follow-up", tc.how, tc.by)
		}
		if e.FollowUp.Origin != tc.want {
			t.Errorf("draft ended %q by %q: follow-up origin %q, want %q", tc.how, tc.by, e.FollowUp.Origin, tc.want)
		}
		var p map[string]any
		_ = json.Unmarshal(e.FollowUp.Payload, &p)
		if tc.by != "" && p["by"] != tc.by {
			t.Errorf("payload by = %v, want %q (control: the payload still says who)", p["by"], tc.by)
		}
	}
}

func TestTheBudgetNoticeSaysWhatWasSpentAndWhenItResets(t *testing.T) {
	got := budgetBody(officers.BudgetAlert{Spent: 10.49, Budget: 10, Waiting: 3, ResetAt: time.Date(2000, 3, 5, 0, 0, 0, 0, time.UTC)})
	want := "It has spent $10.49 of its $10.00 daily budget, so muster stops waking it until 2000-03-05 00:00 UTC. " +
		"3 item(s) are held back meanwhile; an item you enqueue yourself still wakes it, unless its kind is routine."
	if got != want {
		t.Fatalf("budget notice body:\n got %q\nwant %q", got, want)
	}
}

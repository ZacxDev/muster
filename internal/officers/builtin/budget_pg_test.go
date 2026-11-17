package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
)

// ---------------------------------------------------------------------------
// The built-in side of a builtin officer's daily budget and routine batching
// (budget.go): the store defers a routine item to the next routine_wake_at,
// the claim refuses an officer over its budget everything but its owner's
// non-routine items, and the spend is the day's officer_llm_usage summed.
// ---------------------------------------------------------------------------

const budgetOfficer = "budgetdemo:main"

func budgetOfficerDecl(t *testing.T) officers.Officer {
	t.Helper()
	list, err := officers.Parse(`[{"id": "budgetdemo:main", "backend": "builtin", "daily_budget_usd": 10,
		"routine_kinds": ["plugin.note", "plugin.snapshot"], "routine_wake_at": "09:00"}]`, func(string) string { return "" }, "")
	if err != nil {
		t.Fatal(err)
	}
	return list[0]
}

func budgetStore(t *testing.T) *builtin.Store {
	t.Helper()
	dsn := dbtest.DSN(t)
	wipe := func() {
		dbtest.OwnerExec(t, dsn, `DELETE FROM officer_items WHERE officer = 'budgetdemo:main'`)
		dbtest.OwnerExec(t, dsn, `DELETE FROM officer_llm_usage WHERE officer = 'budgetdemo:main'`)
		dbtest.OwnerExec(t, dsn, `DELETE FROM officer_defs WHERE officer = 'budgetdemo:main'`)
		dbtest.OwnerExec(t, dsn, `DELETE FROM officer_types WHERE key = 'budgetdemo'`)
	}
	wipe()
	t.Cleanup(wipe)
	pool, err := db.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	s := builtin.NewStore(pool)
	if err := s.PutType(ctxOp(), 1, builtin.Type{Key: "budgetdemo", Name: "Budget demo"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutDef(ctxOp(), 1, builtin.Def{Officer: budgetOfficer, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	return s
}

func notBeforeOf(t *testing.T, s *builtin.Store, id int64) *time.Time {
	t.Helper()
	var nb *time.Time
	if err := s.Pool().QueryRow(ctxOp(), `SELECT not_before FROM officer_items WHERE id = $1`, id).Scan(&nb); err != nil {
		t.Fatal(err)
	}
	return nb
}

// TestARoutineItemIsEnqueuedForTheNextRoutineWake: with the policy installed,
// a routine kind gets the next 09:00 UTC, through Enqueue (the admin API) and
// EnqueueTx (a plugin message) alike; a kind not listed is not deferred; an
// explicit not_before is kept. Without the policy (the control), nothing is.
func TestARoutineItemIsEnqueuedForTheNextRoutineWake(t *testing.T) {
	s := budgetStore(t)
	now := time.Date(2000, 3, 4, 12, 0, 0, 0, time.UTC)
	tomorrow9 := time.Date(2000, 3, 5, 9, 0, 0, 0, time.UTC)

	control, _, err := s.Enqueue(ctxOp(), 1, builtin.NewItem{Officer: budgetOfficer, Kind: "plugin.note", SubjectKey: "control"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if nb := notBeforeOf(t, s, control); nb != nil {
		t.Fatalf("with no routine policy a plugin.note was deferred to %s", nb)
	}

	o := budgetOfficerDecl(t)
	s.SetRoutinePolicy(officers.NewRegistry([]officers.Officer{o}).ForOwner)

	note, _, err := s.Enqueue(ctxOp(), 1, builtin.NewItem{Officer: budgetOfficer, Kind: "plugin.note", SubjectKey: "n1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if nb := notBeforeOf(t, s, note); nb == nil || !nb.Equal(tomorrow9) {
		t.Fatalf("a plugin.note enqueued at 12:00 has not_before %v, want %s", nb, tomorrow9)
	}
	reply, _, err := s.Enqueue(ctxOp(), 1, builtin.NewItem{Officer: budgetOfficer, Kind: "plugin.reply", SubjectKey: "r1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if nb := notBeforeOf(t, s, reply); nb != nil {
		t.Fatalf("a plugin.reply (not routine) was deferred to %s", nb)
	}
	explicit := now.Add(time.Hour)
	own, _, err := s.Enqueue(ctxOp(), 1, builtin.NewItem{Officer: budgetOfficer, Kind: "plugin.note", SubjectKey: "n2", NotBefore: &explicit}, now)
	if err != nil {
		t.Fatal(err)
	}
	if nb := notBeforeOf(t, s, own); nb == nil || !nb.Equal(explicit) {
		t.Fatalf("an explicit not_before was replaced: %v, want %s", nb, explicit)
	}

	tx, err := s.Pool().Begin(ctxOp())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	snap, err := s.EnqueueTx(ctxOp(), tx, 1, builtin.NewItem{Officer: budgetOfficer, Kind: "plugin.snapshot", SubjectKey: "s1"}, now.Add(-4*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctxOp()); err != nil {
		t.Fatal(err)
	}
	today9 := time.Date(2000, 3, 4, 9, 0, 0, 0, time.UTC)
	if nb := notBeforeOf(t, s, snap); nb == nil || !nb.Equal(today9) {
		t.Fatalf("a plugin.snapshot enqueued in a transaction at 08:00 has not_before %v, want %s", nb, today9)
	}
}

func bookSpend(t *testing.T, s *builtin.Store, day, pool string, instance int, usd float64) {
	t.Helper()
	if _, err := s.Pool().Exec(ctxOp(), `INSERT INTO officer_llm_usage (owner_id, officer, pool, instance, day, requests, input_tokens, output_tokens,
		cache_read_tokens, cache_write_tokens, cost_usd, estimated_requests) VALUES (1, $1, $2, $3, $4::date, 1, 1, 1, 1, 1, $5, 1)
		ON CONFLICT (owner_id, officer, pool, instance, day) DO UPDATE SET cost_usd = EXCLUDED.cost_usd`, budgetOfficer, pool, instance, day, usd); err != nil {
		t.Fatal(err)
	}
}

func enqueueAs(t *testing.T, s *builtin.Store, kind, subject, origin string, now time.Time) officers.ItemID {
	t.Helper()
	id, created, err := s.Enqueue(ctxOp(), 1, builtin.NewItem{Officer: budgetOfficer, Kind: kind, SubjectKey: subject, Origin: origin}, now)
	if err != nil || !created {
		t.Fatalf("enqueue %s: created %v, %v", kind, created, err)
	}
	return officers.ItemID(strconv.FormatInt(id, 10))
}

// TestASpentBudgetRefusesEveryClaimButTheOwnersOwn: today's spend is two
// pools' rows ($6.00 + $4.49 = $10.49, over a $10 budget; yesterday's $50 is
// not today's). Claim-next skips a plugin item and the owner's ROUTINE item
// and takes the owner's draft.refused; a claim of the plugin item by id is
// refused 409 over_budget. With today's spend back at $9.99, claim-next takes
// the oldest item — the plugin's — as it always did.
func TestASpentBudgetRefusesEveryClaimButTheOwnersOwn(t *testing.T) {
	s := budgetStore(t)
	now := time.Date(2000, 3, 4, 12, 0, 0, 0, time.UTC)
	o := budgetOfficerDecl(t)
	b := builtin.NewBackend(s, o, func() time.Time { return now }, dbtest.AsOperator)

	plugin := enqueueAs(t, s, "plugin.reply", "p1", "", now)
	enqueueAs(t, s, "plugin.note", "n1", officers.OriginOwner, now)
	owner := enqueueAs(t, s, "draft.refused", "d1", officers.OriginOwner, now)

	bookSpend(t, s, "2000-03-04", "judgement", 1, 6.00)
	bookSpend(t, s, "2000-03-04", "routine", 1, 4.49)
	bookSpend(t, s, "2000-03-03", "judgement", 1, 50)
	spent, err := b.SpentToday(context.Background(), now)
	if err != nil || spent < 10.4899 || spent > 10.4901 {
		t.Fatalf("SpentToday = %v, %v; want 10.49 (today's two pools, not yesterday)", spent, err)
	}

	raw, err := b.ClaimNext(context.Background(), "officer-budgetdemo-main-1", 300, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := claimedID(t, raw); got != owner {
		t.Fatalf("over budget, claim-next took item %q, want the owner's draft.refused %q", got, owner)
	}
	raw, err = b.ClaimNext(context.Background(), "officer-budgetdemo-main-1", 300, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := claimedID(t, raw); got != "" {
		t.Fatalf("over budget, claim-next took item %q after the owner's: want nothing", got)
	}
	_, err = b.Claim(context.Background(), plugin, "officer-budgetdemo-main-1", 300, "")
	var he *officers.HQError
	if !errors.As(err, &he) || he.Status != 409 || !strings.Contains(string(he.Body), `"code":"over_budget"`) {
		t.Fatalf("over budget, a claim of the plugin item by id: %v, want 409 over_budget", err)
	}

	bookSpend(t, s, "2000-03-04", "routine", 1, 3.99) // 6.00 + 3.99 = 9.99
	raw, err = b.ClaimNext(context.Background(), "officer-budgetdemo-main-1", 300, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := claimedID(t, raw); got != plugin {
		t.Fatalf("at $9.99 of $10, claim-next took %q, want the oldest item (the plugin's) %q", got, plugin)
	}
}

// claimedID is the id of the item a claim answer hands over, "" for none.
func claimedID(t *testing.T, raw []byte) officers.ItemID {
	t.Helper()
	var out struct {
		Item *struct {
			ID int64 `json:"id"`
		} `json:"item"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("claim answer %s: %v", raw, err)
	}
	if out.Item == nil {
		return ""
	}
	return officers.ItemID(strconv.FormatInt(out.Item.ID, 10))
}

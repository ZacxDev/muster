package builtin_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
)

// ---------------------------------------------------------------------------
// builtin.Backend.PutUsage: one officer_llm_usage row per (pool, instance,
// UTC day), merged with GREATEST because every push re-sends the collector's
// whole 25-hour window (usage.go's comment).
// ---------------------------------------------------------------------------

const usageOfficer = "marketing:example-usage"

type usageRow struct {
	Pool                           string
	Instance                       int
	Req, In, Out, Read, Write, Est int64
	USD                            float64
}

func usageRows(t *testing.T, store *builtin.Store, officer, day string) []usageRow {
	t.Helper()
	rows, err := store.Pool().Query(ctxOp(), `SELECT pool, instance, requests, input_tokens, output_tokens, cache_read_tokens,
		cache_write_tokens, estimated_requests, cost_usd::float8 FROM officer_llm_usage WHERE officer = $1 AND day = $2::date
		ORDER BY pool, instance`, officer, day)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []usageRow
	for rows.Next() {
		var r usageRow
		if err := rows.Scan(&r.Pool, &r.Instance, &r.Req, &r.In, &r.Out, &r.Read, &r.Write, &r.Est, &r.USD); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func usageStore(t *testing.T) *builtin.Store {
	t.Helper()
	dsn := dbtest.DSN(t)
	wipe := func() {
		dbtest.OwnerExec(t, dsn, `DELETE FROM officer_llm_usage WHERE officer LIKE 'marketing:example-usage%' OR officer LIKE 'triage:example-usage%'`)
	}
	wipe()
	t.Cleanup(wipe)
	pool, err := db.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return builtin.NewStore(pool)
}

func bucket(hour time.Time, calls, in, out, read, write int64, model string) cctranscript.UsageBucket {
	return cctranscript.UsageBucket{Session: "s-1", Hour: hour, APICalls: calls, InputTokens: in, OutputTokens: out,
		CacheReadTokens: read, CacheCreationTokens: write, Model: model}
}

// TestAWindowPushBooksEachDayAndALaterPartialWindowNeverShrinksIt: the first
// push books the day's totals (two hours summed); a later push that sees only
// the day's last hour (the window moved past its start) leaves them; a push
// whose day has grown raises them. Re-sending the same window books nothing
// more. Every field differs, so a column fed from the wrong one shows.
func TestAWindowPushBooksEachDayAndALaterPartialWindowNeverShrinksIt(t *testing.T) {
	store := usageStore(t)
	o := officers.Officer{ID: usageOfficer, Owner: 1, MaxInstances: 2}
	b := builtin.NewBackend(store, o, nil, dbtest.AsOperator)
	inst := officers.InstanceName(o.ID, 2)
	day := time.Date(2001, 2, 3, 0, 0, 0, 0, time.UTC)
	h10, h11 := day.Add(10*time.Hour), day.Add(11*time.Hour)
	first := officers.UsageReport{Instance: inst, Buckets: []cctranscript.UsageBucket{
		bucket(h10, 2, 300, 40, 5_000, 600, "claude-sonnet-5-5"),
		bucket(h11, 1, 30, 4, 500, 60, "claude-sonnet-5-5"),
	}}
	// (330×2 + 44×10 + 5,500×0.20 + 660×4) / 1e6 = 0.00484
	booked := usageRow{"judgement", 2, 3, 330, 44, 5_500, 660, 3, 0.00484}
	for i := 0; i < 2; i++ { // the same window twice: GREATEST, never a sum
		if err := b.PutUsage(context.Background(), first); err != nil {
			t.Fatal(err)
		}
	}
	if got := usageRows(t, store, o.ID, "2001-02-03"); len(got) != 1 || got[0] != booked {
		t.Fatalf("after the window (twice): %+v, want [%+v]", got, booked)
	}
	tail := officers.UsageReport{Instance: inst, Buckets: []cctranscript.UsageBucket{bucket(h11, 1, 30, 4, 500, 60, "claude-sonnet-5-5")}}
	if err := b.PutUsage(context.Background(), tail); err != nil {
		t.Fatal(err)
	}
	if got := usageRows(t, store, o.ID, "2001-02-03"); len(got) != 1 || got[0] != booked {
		t.Fatalf("after a window holding only the day's tail: %+v, want it unchanged %+v", got, booked)
	}
	grown := officers.UsageReport{Instance: inst, Buckets: append(first.Buckets, bucket(day.Add(12*time.Hour), 4, 1000, 100, 0, 0, "claude-sonnet-5-5"))}
	if err := b.PutUsage(context.Background(), grown); err != nil {
		t.Fatal(err)
	}
	// + (1000×2 + 100×10) / 1e6 = 0.003
	want := usageRow{"judgement", 2, 7, 1330, 144, 5_500, 660, 7, 0.00784}
	if got := usageRows(t, store, o.ID, "2001-02-03"); len(got) != 1 || got[0] != want {
		t.Fatalf("after the day grew: %+v, want [%+v]", got, want)
	}
}

// TestUsageRowsArePerPoolAndInstance: a tiered officer's routine instance
// books under pool routine, its judgement instance under judgement; one
// instance's push never touches the other's row.
func TestUsageRowsArePerPoolAndInstance(t *testing.T) {
	store := usageStore(t)
	o := officers.Officer{ID: "triage:example-usage", Owner: 1, MaxInstances: 1,
		Tiers: &officers.Tiers{Routine: officers.TierPool{Model: "haiku", MaxInstances: 3}, Judgement: officers.TierPool{Model: "opus", MaxInstances: 1}}}
	b := builtin.NewBackend(store, o, nil, dbtest.AsOperator)
	h := time.Date(2001, 2, 4, 9, 0, 0, 0, time.UTC)
	for _, r := range []officers.UsageReport{
		{Instance: o.Pool(officers.TierRoutine).InstanceName(3), Buckets: []cctranscript.UsageBucket{bucket(h, 1, 1_000_000, 0, 0, 0, "claude-haiku-4-5")}},
		{Instance: o.Pool(officers.TierJudgement).InstanceName(1), Buckets: []cctranscript.UsageBucket{bucket(h, 2, 0, 1_000_000, 0, 0, "claude-opus-5-5")}},
	} {
		if err := b.PutUsage(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	want := []usageRow{
		{"judgement", 1, 2, 0, 1_000_000, 0, 0, 2, 20}, // 1M output of opus-5-5 at $20
		{"routine", 3, 1, 1_000_000, 0, 0, 0, 1, 1},    // 1M input of haiku-4-5 at $1
	}
	got := usageRows(t, store, o.ID, "2001-02-04")
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("rows %+v, want %+v", got, want)
	}
}

// TestAPoolOnTheLLMProxyIsNotBookedTwice: the proxy books its own requests
// into the same row (adding, at the provider's cost); the transcript holds
// the same calls, so PutUsage books nothing for a proxied instance — and
// still books the officer's other pool. A name that is no instance of the
// officer is refused.
func TestAPoolOnTheLLMProxyIsNotBookedTwice(t *testing.T) {
	store := usageStore(t)
	o := officers.Officer{ID: "triage:example-usage-llm", Owner: 1, MaxInstances: 1,
		Tiers: &officers.Tiers{Routine: officers.TierPool{Model: "openai/gpt-x", MaxInstances: 1}, Judgement: officers.TierPool{Model: "opus", MaxInstances: 1}},
		LLM:   &officers.LLM{DailyBudgetUSD: 5}}
	if !o.UsesLLMProxy() {
		t.Fatal("fixture: the routine pool is not on the proxy")
	}
	b := builtin.NewBackend(store, o, nil, dbtest.AsOperator)
	h := time.Date(2001, 2, 5, 9, 0, 0, 0, time.UTC)
	routine := o.Pool(officers.TierRoutine).InstanceName(1)
	if err := b.PutUsage(context.Background(), officers.UsageReport{Instance: routine, Buckets: []cctranscript.UsageBucket{bucket(h, 9, 9, 9, 9, 9, "")}}); err != nil {
		t.Fatal(err)
	}
	if err := b.PutUsage(context.Background(), officers.UsageReport{Instance: o.Pool(officers.TierJudgement).InstanceName(1),
		Buckets: []cctranscript.UsageBucket{bucket(h, 1, 1_000_000, 0, 0, 0, "claude-opus-5-5")}}); err != nil {
		t.Fatal(err)
	}
	got := usageRows(t, store, o.ID, "2001-02-05")
	if want := (usageRow{"judgement", 1, 1, 1_000_000, 0, 0, 0, 1, 4}); len(got) != 1 || got[0] != want {
		t.Fatalf("rows %+v, want only the judgement instance's %+v", got, want)
	}
	err := b.PutUsage(context.Background(), officers.UsageReport{Instance: "officer-someone-else-1", Buckets: []cctranscript.UsageBucket{bucket(h, 1, 1, 1, 1, 1, "")}})
	var he *officers.HQError
	if !errors.As(err, &he) || he.Status != 400 {
		t.Fatalf("a report for another officer's instance: %v, want a 400 refusal", err)
	}
}

// TestListUsageReadsTheWindowNewestFirst: the admin read is the owner's rows
// for the last N UTC days, newest first.
func TestListUsageReadsTheWindowNewestFirst(t *testing.T) {
	store := usageStore(t)
	o := officers.Officer{ID: usageOfficer, Owner: 1, MaxInstances: 1}
	b := builtin.NewBackend(store, o, nil, dbtest.AsOperator)
	inst := officers.InstanceName(o.ID, 1)
	d := time.Date(2001, 3, 10, 0, 0, 0, 0, time.UTC)
	if err := b.PutUsage(context.Background(), officers.UsageReport{Instance: inst, Buckets: []cctranscript.UsageBucket{
		bucket(d.AddDate(0, 0, -2).Add(time.Hour), 1, 1_000_000, 0, 0, 0, "claude-sonnet-5-5"),
		bucket(d.AddDate(0, 0, -1).Add(time.Hour), 1, 0, 1_000_000, 0, 0, "claude-sonnet-5-5"),
		bucket(d.Add(time.Hour), 1, 0, 0, 1_000_000, 0, "claude-sonnet-5-5"),
	}}); err != nil {
		t.Fatal(err)
	}
	now := d.Add(5 * time.Hour)
	got, err := store.ListUsage(ctxOp(), 1, o.ID, 2, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Day != "2001-03-10" || got[0].CostUSD != 0.2 || got[1].Day != "2001-03-09" || got[1].CostUSD != 10 {
		t.Fatalf("ListUsage(2 days) = %+v, want 03-10 ($0.20) then 03-09 ($10)", got)
	}
}

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
	"github.com/ZacxDev/muster/internal/officers/usage"
)

// ---------------------------------------------------------------------------
// A BUILTIN OFFICER'S CLAUDE USAGE REACHES officer_llm_usage — the seam the
// a live builtin officer fell through (~60 items worked, 0 usage rows, every
// complete logged `item cost omitted … reason=no_source`). The
// usage collector was configured with officersServing(…, CapUsage), the
// built-in backend did not serve CapUsage, so a builtin instance's Claude
// usage was read by nothing and booked nowhere. This test runs the whole
// path the server wires — EnableOfficers' collector, its Tick over the
// fleet's up instances, the source read, the built-in backend's PutUsage —
// and reads the row back through the admin route the operator uses.
// ---------------------------------------------------------------------------

// fixedUsage is usage.Source answering one fixed reading for every instance.
type fixedUsage struct{ buckets []cctranscript.UsageBucket }

func (f fixedUsage) Read(context.Context, officers.Officer, string, time.Time) (usage.Reading, error) {
	return usage.Reading{Buckets: f.buckets}, nil
}

func TestABuiltinOfficersClaudeUsageIsBookedPerDayAndShownToItsOwner(t *testing.T) {
	f := newConnFixture(t)
	ctx := dbtest.AsOperator(context.Background())
	dsn := dbtest.DSN(t)
	wipe := func() {
		dbtest.OwnerExec(t, dsn, `TRUNCATE officer_contact_events, officer_contacts, officer_schedules, officer_items, officer_defs, officer_types RESTART IDENTITY`)
		dbtest.OwnerExec(t, dsn, `DELETE FROM officer_llm_usage WHERE officer = '`+builtinOfficerID+`'`)
	}
	wipe()
	t.Cleanup(func() {
		wipe()
		_, _ = f.pool.Exec(dbtest.AsOperator(context.Background()), `DELETE FROM agents WHERE name LIKE 'officer-marketing-example%'`)
	})
	list, err := officers.Parse(`[{"id":"`+builtinOfficerID+`","backend":"builtin"}]`, func(string) string { return "" }, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	store := builtin.NewStore(f.pool)
	if err := store.PutType(ctx, 1, builtin.Type{Key: "marketing", Name: "Marketing"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutDef(ctx, 1, builtin.Def{Officer: builtinOfficerID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	srv := New(nil, AuthConfig{HookToken: connTestHookToken, ServiceToken: connTestServiceToken, UIPassword: connTestPassword}, log.New(logs, "", 0))
	srv.UseExtensions(Extensions{Agents: agents.NewPG(f.pool), Connectors: f.store, Provisioner: stubProvisioner{}, Kinds: stubKinds{pick: "acct-one"},
		Attention: attention.NewPG(f.pool)})
	srv.EnableOfficerStore(store)
	// Two days: claude-sonnet-5-5 at list price on the first, a model the
	// price list does not know (priced pessimistically) on the second. Every
	// token count is distinct, so a column read from the wrong field shows.
	// The days are yesterday and today (UTC), so the admin read's window
	// (relative to the wall clock) holds both.
	today := time.Now().UTC().Truncate(24 * time.Hour)
	yesterday := today.AddDate(0, 0, -1)
	src := fixedUsage{buckets: []cctranscript.UsageBucket{
		{Session: "s-1", Hour: yesterday.Add(22 * time.Hour), APICalls: 3,
			InputTokens: 1000, OutputTokens: 2000, CacheReadTokens: 1_000_000, CacheCreationTokens: 100_000, Model: "claude-sonnet-5-5"},
		{Session: "s-1", Hour: today.Add(time.Hour), APICalls: 1,
			InputTokens: 100, OutputTokens: 10, CacheReadTokens: 7, CacheCreationTokens: 5, Model: "claude-model-a"},
	}}
	if _, err := srv.EnableOfficers(OfficersConfig{Officers: list, InstallSession: discardSession, Builtin: store,
		UsageSource: src, NoUsageRun: true}); err != nil {
		t.Fatal(err)
	}
	startPoolInstance(t, f, srv, builtinOfficerID, "", 1, "fixture-builtin-usage-hooks-token")
	if srv.officerRT.usage == nil {
		t.Fatal("no usage collector")
	}
	srv.officerRT.usage.Tick(context.Background())

	type row struct {
		Day, Pool                      string
		Instance                       int
		Req, In, Out, Read, Write, Est int64
		USD                            float64
	}
	rows, err := f.pool.Query(ctx, `SELECT day::text, pool, instance, requests, input_tokens, output_tokens, cache_read_tokens,
		cache_write_tokens, estimated_requests, cost_usd::float8 FROM officer_llm_usage WHERE officer = $1 ORDER BY day`, builtinOfficerID)
	if err != nil {
		t.Fatal(err)
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.Day, &r.Pool, &r.Instance, &r.Req, &r.In, &r.Out, &r.Read, &r.Write, &r.Est, &r.USD); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// Day 1: (1000×2 + 2000×10 + 1,000,000×0.20 + 100,000×4) / 1e6 = 0.622.
	// Day 2, pessimistic row (10 / 50 / 1.00 / 20): (100×10 + 10×50 + 7×1 + 5×20) / 1e6 = 0.001607.
	want := []row{
		{yesterday.Format("2006-01-02"), "judgement", 1, 3, 1000, 2000, 1_000_000, 100_000, 3, 0.622},
		{today.Format("2006-01-02"), "judgement", 1, 1, 100, 10, 7, 5, 1, 0.001607},
	}
	if len(got) != len(want) {
		t.Fatalf("officer_llm_usage rows %+v, want %+v\nlogs:\n%s", got, want, logs)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// The operator's read: the admin route, as `muster officer usage` calls it.
	req := httptest.NewRequest(http.MethodGet, "/api/officers/defs/"+builtinOfficerID+"/usage?days=2", nil)
	req.Host = "muster.test"
	req.Header.Set("Authorization", "Bearer "+connTestHookToken)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET usage: %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Days  []builtin.UsageDay `json:"days"`
		Total float64            `json:"total_cost_usd"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Days) != 2 || body.Days[0].Day != today.Format("2006-01-02") || body.Total != 0.623607 {
		t.Fatalf("GET usage: %s, want both days newest first and total 0.623607", rec.Body)
	}
}

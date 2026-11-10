package chiefusage

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

func pgUsage(t *testing.T) (*PGStore, *pgxpool.Pool) {
	t.Helper()
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES (2, 'member'), (3, 'member') ON CONFLICT (id) DO NOTHING`)
	dbtest.OwnerExec(t, dsn, `TRUNCATE chief_turn_usage, plugin_message_handling, routing_rules`)
	dbtest.OwnerExec(t, dsn, `DELETE FROM plugins WHERE name LIKE 'usage-%'`)
	return NewPG(pool), pool
}

func as(uid int64) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaSession})
}

// TestRecordTurnAndHandlingStoreWhatTheyAreGiven: the row a turn and a
// handling write, owner-scoped; a later handler replaces an earlier one and a
// fallback rule is kept.
func TestRecordTurnAndHandlingStoreWhatTheyAreGiven(t *testing.T) {
	s, pool := pgUsage(t)
	turn := Turn{Chief: 41, Trigger: Trigger{Kind: TriggerPlugin, Plugin: "example-mail", Sender: "a@example.test", MessageID: 77},
		Outcome: OutcomeSettled, RuleProposed: true, Known: true, Duration: 1500 * time.Millisecond,
		Tokens: Tokens{Input: 1234, Output: 56, CacheRead: 7890, CacheCreation: 12}}
	if err := s.RecordTurn(as(2), turn); err != nil {
		t.Fatal(err)
	}
	var owner, chief, msg, in, out, read, create, ms int64
	var trig, plugin, sender, outcome string
	var proposed, known bool
	if err := pool.QueryRow(authz.System(context.Background()), `SELECT owner_id, chief_agent, trigger, plugin_name, sender, plugin_message_id,
		outcome, rule_proposed, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, usage_known, duration_ms
		FROM chief_turn_usage`).Scan(&owner, &chief, &trig, &plugin, &sender, &msg, &outcome, &proposed, &in, &out, &read, &create, &known, &ms); err != nil {
		t.Fatal(err)
	}
	if owner != 2 || chief != 41 || trig != "plugin" || plugin != "example-mail" || sender != "a@example.test" || msg != 77 ||
		outcome != "settled" || !proposed || in != 1234 || out != 56 || read != 7890 || create != 12 || !known || ms != 1500 {
		t.Fatalf("the stored turn: owner %d chief %d %s %s %s msg %d %s proposed %v tokens %d/%d/%d/%d known %v %dms",
			owner, chief, trig, plugin, sender, msg, outcome, proposed, in, out, read, create, known, ms)
	}
	if err := s.RecordHandling(as(2), Handling{MessageID: 77, Plugin: "example-mail", Sender: "a@example.test", Handler: HandlerChief, FallbackRuleID: 5}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordHandling(as(2), Handling{MessageID: 77, Plugin: "example-mail", Handler: HandlerChief}); err != nil {
		t.Fatal(err)
	}
	var handler string
	var fallback *int64
	if err := pool.QueryRow(authz.System(context.Background()), `SELECT handler, fallback_rule_id FROM plugin_message_handling WHERE plugin_message_id = 77`).
		Scan(&handler, &fallback); err != nil || handler != "chief" || fallback == nil || *fallback != 5 {
		t.Fatalf("the handling after a second chief record: %s %v (%v); want chief, fallback rule 5 kept", handler, fallback, err)
	}
	// Another owner's report sees none of it.
	rep, err := s.Report(as(3), time.Now().Add(-time.Hour))
	if err != nil || rep.Totals.Turns != 0 || rep.Totals.Messages != 0 {
		t.Fatalf("another owner's report: %+v %v; want nothing", rep.Totals, err)
	}
}

// TestTheReportsAggregatesArePinned: a fixture of chief turns, handlings and
// rules for owner 2 (and one turn for owner 3, which must not show), and every
// number of the 7-day report as a literal — the tokens-avoided estimate
// included: rule A's two hits × the average of the three known turns for its
// plugin and sender before it was approved (6500, 2600 and 100000 — that last
// one older than the window, which the average still reads), 36367.
func TestTheReportsAggregatesArePinned(t *testing.T) {
	s, pool := pgUsage(t)
	sys := authz.System(context.Background())
	now := time.Now().UTC()
	approve := now.Add(-48 * time.Hour)
	var mail, other int64
	for name, id := range map[string]*int64{"usage-mail": &mail, "usage-other": &other} {
		if err := pool.QueryRow(sys, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope) VALUES (2, $1, 'Example', $1 || '-hash', 'example')
			RETURNING id`, name).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	var ruleA, ruleB int64
	if err := pool.QueryRow(sys, `INSERT INTO routing_rules (owner_id, plugin_id, plugin_name, match_sender, require_verified, action, settle_reason,
		state, chief_agent, source_message, decided_at, approved_at, hits) VALUES (2, $1, 'usage-mail', 'digest@news.example.test', true, 'settle', 'a newsletter',
		'active', 41, 1, $2, $2, 2) RETURNING id`, mail, approve).Scan(&ruleA); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(sys, `INSERT INTO routing_rules (owner_id, plugin_id, plugin_name, match_thread, action, settle_reason,
		state, chief_agent, source_message, decided_at, approved_at) VALUES (2, $1, 'usage-other', 'thr-1', 'settle', 'chatter',
		'disabled', 41, 2, $2, $2) RETURNING id`, other, approve).Scan(&ruleB); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(sys, `INSERT INTO routing_rules (owner_id, plugin_id, plugin_name, match_sender, action, settle_reason, chief_agent, source_message)
		VALUES (2, $1, 'usage-mail', 'x@example.test', 'settle', 'proposed only', 41, 3)`, mail); err != nil {
		t.Fatal(err)
	}
	turn := func(owner int64, at time.Time, trig, plugin, sender string, known bool, in, out, read, create int64) {
		t.Helper()
		if _, err := pool.Exec(sys, `INSERT INTO chief_turn_usage (owner_id, chief_agent, created_at, trigger, plugin_name, sender, outcome,
			input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, usage_known) VALUES ($1, 41, $2, $3, $4, $5, 'other', $6, $7, $8, $9, $10)`,
			owner, at, trig, plugin, sender, in, out, read, create, known); err != nil {
			t.Fatal(err)
		}
	}
	const digest, others = "digest@news.example.test", "other@news.example.test"
	turn(2, now.Add(-72*time.Hour), "plugin", "usage-mail", digest, true, 1000, 200, 5000, 300) // 6500, before the rule
	turn(2, now.Add(-71*time.Hour), "plugin", "usage-mail", digest, true, 500, 100, 2000, 0)    // 2600, before the rule
	turn(2, now.Add(-70*time.Hour), "plugin", "usage-mail", digest, false, 0, 0, 0, 0)          // unknown: not averaged
	turn(2, now.Add(-240*time.Hour), "plugin", "usage-mail", digest, true, 100000, 0, 0, 0)     // outside the window, before the rule
	turn(2, now.Add(-24*time.Hour), "plugin", "usage-mail", others, true, 300, 50, 0, 0)        // 350
	turn(2, now.Add(-24*time.Hour), "owner_chat", "", "", true, 2000, 400, 10000, 1000)         // 13400
	turn(3, now.Add(-24*time.Hour), "plugin", "usage-mail", digest, true, 999999, 0, 0, 0)      // another owner's
	handled := func(msg int64, plugin, handler string, rule, fallback any, at time.Time) {
		t.Helper()
		if _, err := pool.Exec(sys, `INSERT INTO plugin_message_handling (owner_id, plugin_message_id, plugin_name, handler, rule_id, fallback_rule_id, created_at)
			VALUES (2, $1, $2, $3, $4, $5, $6)`, msg, plugin, handler, rule, fallback, at); err != nil {
			t.Fatal(err)
		}
	}
	h := now.Add(-time.Hour)
	handled(1, "usage-mail", "chief", nil, nil, now.Add(-72*time.Hour))
	handled(2, "usage-mail", "chief", nil, nil, now.Add(-71*time.Hour))
	handled(3, "usage-mail", "chief", nil, nil, now.Add(-70*time.Hour))
	handled(4, "usage-mail", "rule", ruleA, nil, h)
	handled(5, "usage-mail", "rule", ruleA, nil, h)
	handled(6, "usage-mail", "thread", nil, nil, h)
	handled(7, "usage-other", "chief", nil, ruleB, h)
	handled(8, "review-mail", "pr_route", nil, nil, h)
	handled(9, "usage-mail", "chief", nil, nil, now.Add(-240*time.Hour)) // outside the window

	since := now.AddDate(0, 0, -6)
	since = time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, time.UTC)
	rep, err := s.Report(as(2), since)
	if err != nil {
		t.Fatal(err)
	}
	wantTotals := Totals{Turns: 5, UnknownTurns: 1, Tokens: Tokens{Input: 3800, Output: 750, CacheRead: 17000, CacheCreation: 1300},
		Messages: 8, ByRule: 2, ByMuster: 2, ByChief: 4, Fallbacks: 1}
	if rep.Totals != wantTotals {
		t.Errorf("totals = %+v\nwant     %+v", rep.Totals, wantTotals)
	}
	if rep.Totals.Tokens.Total() != 22850 || rep.Totals.WithoutChiefPercent() != 50 {
		t.Errorf("total tokens %d, %d%% without the chief; want 22850, 50%%", rep.Totals.Tokens.Total(), rep.Totals.WithoutChiefPercent())
	}
	wantSources := []Source{{Plugin: "usage-mail", Messages: 6, ByRule: 2, ByMuster: 1, ByChief: 3},
		{Plugin: "review-mail", Messages: 1, ByMuster: 1}, {Plugin: "usage-other", Messages: 1, ByChief: 1}}
	if !reflect.DeepEqual(rep.Sources, wantSources) {
		t.Errorf("sources = %+v\nwant      %+v", rep.Sources, wantSources)
	}
	wantBySource := []TokenRow{{Trigger: "owner_chat", Turns: 1, Tokens: Tokens{Input: 2000, Output: 400, CacheRead: 10000, CacheCreation: 1000}},
		{Trigger: "plugin", Plugin: "usage-mail", Turns: 4, Tokens: Tokens{Input: 1800, Output: 350, CacheRead: 7000, CacheCreation: 300}}}
	if !reflect.DeepEqual(rep.BySource, wantBySource) {
		t.Errorf("by source = %+v\nwant        %+v", rep.BySource, wantBySource)
	}
	wantSenders := []TokenRow{{Trigger: "plugin", Plugin: "usage-mail", Sender: digest, Turns: 3, Tokens: Tokens{Input: 1500, Output: 300, CacheRead: 7000, CacheCreation: 300}},
		{Trigger: "plugin", Plugin: "usage-mail", Sender: others, Turns: 1, Tokens: Tokens{Input: 300, Output: 50}}}
	if !reflect.DeepEqual(rep.TopSenders, wantSenders) {
		t.Errorf("top senders = %+v\nwant          %+v", rep.TopSenders, wantSenders)
	}
	wantRules := []RuleImpact{
		{RuleID: ruleA, Plugin: "usage-mail", Sender: digest, State: "active", Hits: 2, HitsAll: 2, BeforeTurns: 3, AvgBefore: 36367, Avoided: 72734},
		{RuleID: ruleB, Plugin: "usage-other", State: "disabled"},
	}
	if !reflect.DeepEqual(rep.Rules, wantRules) {
		t.Errorf("rules = %+v\nwant    %+v", rep.Rules, wantRules)
	}
	if len(rep.Days) != 7 {
		t.Fatalf("%d days, want 7", len(rep.Days))
	}
	var sum Day
	for _, d := range rep.Days {
		sum.Turns, sum.Messages, sum.Tokens = sum.Turns+d.Turns, sum.Messages+d.Messages, sum.Tokens.Add(d.Tokens)
	}
	if sum.Turns != 5 || sum.Messages != 8 || sum.Tokens.Total() != 22850 {
		t.Errorf("the days sum to %d turns, %d messages, %d tokens; want 5, 8, 22850", sum.Turns, sum.Messages, sum.Tokens.Total())
	}
	// Owner 3 sees its one turn and nothing of owner 2's.
	rep3, err := s.Report(as(3), since)
	if err != nil || rep3.Totals.Turns != 1 || rep3.Totals.Tokens.Input != 999999 || len(rep3.Rules) != 0 || rep3.Totals.Messages != 0 {
		t.Fatalf("owner 3's report: %+v rules %d (%v)", rep3.Totals, len(rep3.Rules), err)
	}
}

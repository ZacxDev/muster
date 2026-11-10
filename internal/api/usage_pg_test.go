package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
)

// TestTheUsagePageRendersTheWindowsTotals: the page over a fixture of turns,
// handlings and an approved rule — the totals, the sources, the costliest
// sender and the rule's estimate as literals, the window switch, and a turn
// outside the 7-day window counted only in the 30-day one.
func TestTheUsagePageRendersTheWindowsTotals(t *testing.T) {
	f := newRulesFixture(t)
	sys := authz.System(context.Background())
	now := time.Now().UTC()
	approve := now.Add(-48 * time.Hour)
	var rule int64
	if err := f.pool.QueryRow(sys, `INSERT INTO routing_rules (owner_id, plugin_id, plugin_name, match_sender, require_verified, action, settle_reason,
		state, chief_agent, source_message, decided_at, approved_at, hits) VALUES ($1, $2, 'rules-mail', $3, true, 'settle', 'a newsletter',
		'active', $4, 1, $5, $5, 1) RETURNING id`, f.owner, f.p.ID, rulesSender, f.chief.ID, approve).Scan(&rule); err != nil {
		t.Fatal(err)
	}
	turn := func(at time.Time, sender string, in, out, read, create int64) {
		t.Helper()
		if _, err := f.pool.Exec(sys, `INSERT INTO chief_turn_usage (owner_id, chief_agent, created_at, trigger, plugin_name, sender, outcome,
			input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, usage_known) VALUES ($1, $2, $3, 'plugin', 'rules-mail', $4, 'settled',
			$5, $6, $7, $8, true)`, f.owner, f.chief.ID, at, sender, in, out, read, create); err != nil {
			t.Fatal(err)
		}
	}
	turn(now.Add(-72*time.Hour), rulesSender, 400, 100, 3000, 500)         // 4000, before the rule
	turn(now.Add(-24*time.Hour), "other@news.example.test", 200, 50, 0, 0) // 250
	turn(now.AddDate(0, 0, -20), rulesSender, 1000, 0, 0, 0)               // 1000, before the rule, outside 7 days
	handled := func(msg int64, handler string, ruleID any, at time.Time) {
		t.Helper()
		if _, err := f.pool.Exec(sys, `INSERT INTO plugin_message_handling (owner_id, plugin_message_id, plugin_name, sender, handler, rule_id, created_at)
			VALUES ($1, $2, 'rules-mail', $3, $4, $5, $6)`, f.owner, msg, rulesSender, handler, ruleID, at); err != nil {
			t.Fatal(err)
		}
	}
	handled(901, "chief", nil, now.Add(-72*time.Hour))
	handled(902, "rule", rule, now.Add(-time.Hour))
	handled(903, "thread", nil, now.Add(-time.Hour))
	handled(904, "chief", nil, now.AddDate(0, 0, -20))

	page := func(q string) string {
		t.Helper()
		w := f.call(t, "GET", usagePath+q, nil, f.operator())
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: %d", usagePath+q, w.Code)
		}
		return w.Body.String()
	}
	body := page("")
	for _, want := range []string{
		`data-testid="usage-page" data-days="7"`,
		`data-testid="usage-totals" data-messages="3" data-by-rule="1" data-by-muster="1" data-by-chief="1" data-turns="2" data-tokens="4250"`,
		`3 plugin messages · 66% without your chief`,
		`data-usage-source="rules-mail"`,
		`<a href="/usage?days=7" aria-current="page"`,
		`data-usage-sender="` + rulesSender + `"`,
		// The rule's estimate: 1 hit in the window × the average of the two
		// known turns before it (4000 and 1000, one outside the window): 2500.
		`data-usage-rule="` + strconv.FormatInt(rule, 10) + `" data-avoided="2500"`,
		`about 2,500 tokens saved (2,500 a turn, averaged over 2 turns before it)`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the 7-day page lacks %q", want)
		}
	}
	if t.Failed() {
		t.Logf("page:\n%s", body)
	}
	body30 := page("?days=30")
	if !strings.Contains(body30, `data-testid="usage-totals" data-messages="4" data-by-rule="1" data-by-muster="1" data-by-chief="2" data-turns="3" data-tokens="5250"`) ||
		!strings.Contains(body30, `<a href="/usage?days=30" aria-current="page"`) {
		t.Errorf("the 30-day page's totals are not the whole fixture")
	}
	// An unknown window is the default.
	if !strings.Contains(page("?days=9"), `data-days="7"`) {
		t.Error("an unknown window is not the 7-day default")
	}
}

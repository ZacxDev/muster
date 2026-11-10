package db_test

import (
	"strings"
	"testing"
)

// TestMigration0105RoutingRulesAndTelemetry: at version 104 none of the three
// tables exists. After 0105, routing_rules refuses a rule with nothing to match
// on, a route with no session (or with a reason), a settle with no reason, a
// ROUTE THAT DOES NOT REQUIRE A VERIFIED SENDER, an active rule with no
// approval time and a subject prefix under three characters; and stores a well-formed settle and route. chief_turn_usage refuses an unknown
// trigger or outcome and negative tokens; plugin_message_handling refuses an
// unknown handler, a rule hit with no rule, a rule id on a chief hand-off's
// fallback that is not the chief's, and a second row for one message.
// Re-applying the file changes nothing.
func TestMigration0105RoutingRulesAndTelemetry(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0105_routing_rules", 104)
	for _, tbl := range []string{"routing_rules", "chief_turn_usage", "plugin_message_handling"} {
		var has bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass('public.'||$1) IS NOT NULL`, tbl).Scan(&has); err != nil || has {
			t.Fatalf("control: at version 104 %s exists=%v (%v); want absent", tbl, has, err)
		}
	}
	plugin := scanID(ctx, t, pool, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope)
		VALUES (1, 'plugin-rules', 'Example inbox', 'pluginhash-rules', 'example') RETURNING id`)

	applyThroughFrom(ctx, t, pool, 105)

	rule := func(cols, vals string) error {
		_, err := pool.Exec(ctx, `INSERT INTO routing_rules (owner_id, plugin_id, plugin_name, chief_agent, source_message, `+cols+`)
			VALUES (1, $1, 'plugin-rules', 7, 9, `+vals+`)`, plugin)
		return err
	}
	for _, c := range []struct{ cols, vals, constraint string }{
		{"action, settle_reason", "'settle', 'noise'", "routing_rules_matches"},
		{"match_sender, action", "'a@example.test', 'route'", "routing_rules_action_fields"},
		{"match_sender, action, route_session, settle_reason, require_verified", "'a@example.test', 'route', 'ses', 'why', true", "routing_rules_action_fields"},
		{"match_sender, action", "'a@example.test', 'settle'", "routing_rules_action_fields"},
		{"match_sender, action, route_session", "'a@example.test', 'route', 'ses'", "routing_rules_route_verified"},
		{"match_sender, action, settle_reason, state, decided_at", "'a@example.test', 'settle', 'noise', 'active', now()", "routing_rules_approved"},
		{"match_sender, action, settle_reason, state", "'a@example.test', 'settle', 'noise', 'declined'", "routing_rules_decided"},
		{"match_sender, match_subject_prefix, action, settle_reason", "'a@example.test', 'ab', 'settle', 'noise'", "routing_rules_match_subject_prefix_check"},
		{"match_sender, match_external_prefix, action, settle_reason", "'a@example.test', 'news-20', 'settle', 'noise'", "routing_rules_match_external_prefix_check"},
		{"match_subject_prefix, action, settle_reason", "'Weekly digest', 'settle', 'noise'", "routing_rules_matches"},
		{"match_external_prefix, action, settle_reason", "'news-2026-11', 'settle', 'noise'", "routing_rules_matches"},
	} {
		if err := rule(c.cols, c.vals); err == nil || !strings.Contains(err.Error(), c.constraint) {
			t.Errorf("rule (%s) = (%s): %v; want refused by %s", c.cols, c.vals, err, c.constraint)
		}
	}
	for _, c := range []struct{ cols, vals string }{
		{"match_sender, action, settle_reason", "'news@example.test', 'settle', 'a newsletter'"},
		{"match_thread, action, route_session, require_verified", "'thread-7', 'route', 'ses_0abc', true"},
		{"match_thread, match_external_prefix, match_subject_prefix, action, settle_reason, state, decided_at, approved_at",
			"'thread-9', 'news-2026-', 'Review of', 'settle', 'reviews', 'active', now(), now()"},
	} {
		if err := rule(c.cols, c.vals); err != nil {
			t.Errorf("a well-formed rule (%s): %v; want stored", c.cols, err)
		}
	}

	turn := func(trigger, outcome string, input int) error {
		_, err := pool.Exec(ctx, `INSERT INTO chief_turn_usage (owner_id, chief_agent, trigger, outcome, input_tokens) VALUES (1, 7, $1, $2, $3)`,
			trigger, outcome, input)
		return err
	}
	for _, c := range []struct {
		trigger, outcome string
		input            int
		constraint       string
	}{
		{"mail", "routed", 1, "chief_turn_usage_trigger_check"},
		{"plugin", "dropped", 1, "chief_turn_usage_outcome_check"},
		{"plugin", "routed", -1, "chief_turn_usage_input_tokens_check"},
	} {
		if err := turn(c.trigger, c.outcome, c.input); err == nil || !strings.Contains(err.Error(), c.constraint) {
			t.Errorf("turn %s/%s/%d: %v; want refused by %s", c.trigger, c.outcome, c.input, err, c.constraint)
		}
	}
	if err := turn("plugin", "settled", 1200); err != nil {
		t.Errorf("a well-formed turn: %v", err)
	}

	handled := func(msg int, handler string, rule, fallback any) error {
		_, err := pool.Exec(ctx, `INSERT INTO plugin_message_handling (owner_id, plugin_message_id, handler, rule_id, fallback_rule_id)
			VALUES (1, $1, $2, $3, $4)`, msg, handler, rule, fallback)
		return err
	}
	for _, c := range []struct {
		msg            int
		handler        string
		rule, fallback any
		constraint     string
	}{
		{1, "inbox", nil, nil, "plugin_message_handling_handler_check"},
		{2, "rule", nil, nil, "plugin_message_handling_rule"},
		{3, "chief", 4, nil, "plugin_message_handling_rule"},
		{4, "thread", nil, 4, "plugin_message_handling_fallback"},
	} {
		if err := handled(c.msg, c.handler, c.rule, c.fallback); err == nil || !strings.Contains(err.Error(), c.constraint) {
			t.Errorf("handling %d %s: %v; want refused by %s", c.msg, c.handler, err, c.constraint)
		}
	}
	if err := handled(10, "rule", 4, nil); err != nil {
		t.Errorf("a rule hit: %v", err)
	}
	if err := handled(11, "chief", nil, 4); err != nil {
		t.Errorf("a rule's fallback to the chief: %v", err)
	}
	if err := handled(10, "chief", nil, nil); err == nil || !strings.Contains(err.Error(), "plugin_message_handling_message") {
		t.Errorf("a second row for one message: %v; want refused by plugin_message_handling_message", err)
	}

	if _, err := pool.Exec(ctx, readMigration(t, "0105_routing_rules_chief_usage.sql")); err != nil {
		t.Fatalf("re-applying 0105: %v", err)
	}
	var rules, turns, handlings int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM routing_rules), (SELECT count(*) FROM chief_turn_usage),
		(SELECT count(*) FROM plugin_message_handling)`).Scan(&rules, &turns, &handlings); err != nil || rules != 3 || turns != 1 || handlings != 2 {
		t.Fatalf("after re-applying 0105: %d rules, %d turns, %d handlings (%v); want 3, 1, 2", rules, turns, handlings, err)
	}
}

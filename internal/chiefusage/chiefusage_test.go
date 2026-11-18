package chiefusage

import (
	"context"
	"testing"
)

// TestOutcomeOfReadsTheToolsInEitherSpelling: the turn's outcome is the
// highest-ranked of its tools' outcomes, in the bare and the mcp__muster__
// spelling alike; a proposed rule is flagged whatever else the turn did; a
// failed turn is failed.
func TestOutcomeOfReadsTheToolsInEitherSpelling(t *testing.T) {
	for _, c := range []struct {
		tools    []string
		failed   bool
		want     string
		proposed bool
	}{
		{nil, false, OutcomeOther, false},
		{[]string{"focus_search"}, false, OutcomeOther, false},
		{[]string{"mcp__muster__plugin_message_settle"}, false, OutcomeSettled, false},
		{[]string{"plugin_message_settle", "routing_rule_propose"}, false, OutcomeSettled, true},
		{[]string{"routing_rule_propose"}, false, OutcomeRuleProposed, true},
		{[]string{"mcp__muster__focus_search", "mcp__muster__route_to_session", "mcp__muster__routing_rule_propose"}, false, OutcomeRouted, true},
		{[]string{"ask_owner", "plugin_message_settle"}, false, OutcomeAsk, false},
		{[]string{"task_create_gated", "task_route_attention"}, false, OutcomeTask, false},
		{[]string{"route_to_session"}, true, OutcomeFailed, false},
		{[]string{"mcp__muster__routing_rule_propose"}, true, OutcomeFailed, true},
		// A tool of another MCP server is no muster tool.
		{[]string{"mcp__hq__route_to_session"}, false, OutcomeOther, false},
	} {
		got, proposed := OutcomeOf(c.tools, c.failed)
		if got != c.want || proposed != c.proposed {
			t.Errorf("OutcomeOf(%v, %v) = %s, %v; want %s, %v", c.tools, c.failed, got, proposed, c.want, c.proposed)
		}
	}
}

// TestTriggerFromDefaultsToOther: a turn no door marked is "other".
func TestTriggerFromDefaultsToOther(t *testing.T) {
	if got := TriggerFrom(context.Background()); got.Kind != TriggerOther {
		t.Fatalf("no trigger: %+v", got)
	}
	want := Trigger{Kind: TriggerPlugin, Plugin: "example-mail", Sender: "a@example.test", MessageID: 9}
	if got := TriggerFrom(WithTrigger(context.Background(), want)); got != want {
		t.Fatalf("TriggerFrom = %+v, want %+v", got, want)
	}
}

func TestTotalsWithoutChiefPercent(t *testing.T) {
	if got := (Totals{Messages: 8, ByRule: 3, ByMuster: 2, ByChief: 3}).WithoutChiefPercent(); got != 62 {
		t.Fatalf("5 of 8 without the chief = %d%%, want 62", got)
	}
	if got := (Totals{}).WithoutChiefPercent(); got != 0 {
		t.Fatalf("no messages = %d%%", got)
	}
}

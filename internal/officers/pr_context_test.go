package officers

import (
	"strings"
	"testing"
)

// The PR context read tool's offer rule and argument decoding
// (pr_context.go). Names and types are literals: they are the contract the
// charters name.

// TestPRContextToolIsTheWebPerformanceJudgementPoolsOnly: officer_pr_context
// to a web-performance officer's JUDGEMENT pool only — never its routine
// pool, never an untiered web-performance officer, never another type (nor a
// type whose name merely contains web-performance).
func TestPRContextToolIsTheWebPerformanceJudgementPoolsOnly(t *testing.T) {
	for _, c := range []struct {
		id, tier string
		want     bool
	}{
		{"web-performance:alpha-web", TierJudgement, true},
		{"web-performance:other", TierJudgement, true},
		{"web-performance:alpha-web", TierRoutine, false},
		{"web-performance:alpha-web", "", false},
		{"release:alpha-web", TierJudgement, false},
		{"task-reconciler:hq", TierJudgement, false},
		{"web-performance-x:alpha-web", TierJudgement, false},
		{"x-web-performance:alpha-web", TierJudgement, false},
	} {
		if got := ToolOffered(Officer{ID: c.id, Tier: c.tier}, "officer_pr_context"); got != c.want {
			t.Errorf("ToolOffered(%s %q, officer_pr_context) = %v, want %v", c.id, c.tier, got, c.want)
		}
	}
}

func TestParsePRContextArgs(t *testing.T) {
	for _, raw := range []string{`{"item_id":42,"pr":7251}`, `{"item_id":"42","pr":7251}`} {
		a, err := ParsePRContextArgs([]byte(raw))
		if err != nil || a.Item != "42" || a.PR != 7251 {
			t.Errorf("%s: %+v %v, want item 42 pr 7251", raw, a, err)
		}
	}
	for _, c := range []struct{ raw, want string }{
		{`{"item_id":42}`, "pr is required"},
		{`{"item_id":42,"pr":null}`, "pr must be the PR number as an integer"},
		{`{"item_id":42,"pr":"7251"}`, "pr must be the PR number as an integer"},
		{`{"item_id":42,"pr":0}`, "pr must be a PR number from 1 to 9999999999"},
		{`{"item_id":42,"pr":-3}`, "pr must be a PR number from 1 to 9999999999"},
		{`{"item_id":42,"pr":72.5}`, "pr must be a PR number from 1 to 9999999999"},
		{`{"item_id":42,"pr":10000000000}`, "pr must be a PR number from 1 to 9999999999"},
		{`{"pr":7251}`, "item_id is required"},
		{`{"item_id":"4/2","pr":7251}`, "item_id is required"},
		{`{"item_id":42,"pr":7251,"repo":"acme"}`, `"repo" is not an argument of officer_pr_context`},
		{`[7251]`, "arguments must be one JSON object"},
	} {
		if _, err := ParsePRContextArgs([]byte(c.raw)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error naming %q", c.raw, err, c.want)
		}
	}
}

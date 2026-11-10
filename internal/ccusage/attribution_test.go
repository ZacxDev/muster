package ccusage

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func officerOfTest(name string) (string, string, bool, bool) {
	switch {
	case strings.HasPrefix(name, "officer-triage-web-r"):
		return "triage:web", "routine", false, true
	case strings.HasPrefix(name, "officer-triage-web-"):
		return "triage:web", "judgement", false, true
	case strings.HasPrefix(name, "officer-release-web-"):
		return "release:web", "", false, true
	case strings.HasPrefix(name, "officer-cheap-web-r"):
		return "cheap:web", "routine", false, true // its routine pool is on a Claude account
	case strings.HasPrefix(name, "officer-cheap-web-"):
		return "cheap:web", "judgement", true, true // its judgement pool is on the LLM proxy
	}
	return "", "", false, false
}

// TestAttributeCountsWhoRanOnEachAccount: officer instances by officer and
// pool, pool workers by pool, every other agent as interactive, jobs by
// requester — each in the window it was active in. The fixture's counts are
// pairwise distinct so a swapped bucket cannot pass.
func TestAttributeCountsWhoRanOnEachAccount(t *testing.T) {
	now := t0
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	agents := []AgentUse{
		{Name: "officer-triage-web-r1", CCAccount: "alpha", LastActive: now},
		{Name: "officer-triage-web-r2", CCAccount: "alpha", LastActive: ago(2 * time.Hour)},
		{Name: "officer-triage-web-1", CCAccount: "alpha", LastActive: ago(3 * Day)}, // week only
		{Name: "officer-release-web-1", CCAccount: "beta", LastActive: ago(time.Hour)},
		{Name: "worker-1", CCAccount: "alpha", WorkerPool: "reviewers", LastActive: now},
		{Name: "worker-2", CCAccount: "alpha", WorkerPool: "reviewers", LastActive: now},
		{Name: "worker-3", CCAccount: "alpha", WorkerPool: "reviewers", LastActive: now},
		{Name: "keen-wren", CCAccount: "alpha", LastActive: ago(Day - time.Minute)},
		{Name: "chief", CCAccount: "alpha", LastActive: ago(Day + time.Minute)}, // week only
		{Name: "old", CCAccount: "alpha", LastActive: ago(Week + time.Minute)},  // neither
		{Name: "nowhere", CCAccount: "", LastActive: now},
		// A judgement instance on the LLM proxy holds no Claude token: its row's
		// account is not one it uses, so it is counted nowhere; its officer's
		// Claude-account routine pool is counted as usual.
		{Name: "officer-cheap-web-1", CCAccount: "alpha", LastActive: now},
		{Name: "officer-cheap-web-r1", CCAccount: "beta", LastActive: now}, // no account
	}
	jobs := []JobCount{
		{CCAccount: "alpha", RequestedBy: "pr-review", Last24h: 4, Last7d: 9},
		{CCAccount: "alpha", RequestedBy: "", Last24h: 0, Last7d: 6},
		{CCAccount: "beta", RequestedBy: "pr-review", Last24h: 5, Last7d: 5},
		{CCAccount: "", RequestedBy: "x", Last24h: 1, Last7d: 1},
	}
	got := Attribute(now, agents, jobs, officerOfTest)
	if len(got) != 2 {
		t.Fatalf("accounts: %v, want alpha and beta", got)
	}
	a := got["alpha"]
	want24 := Usage{
		Officers:          []OfficerUse{{Officer: "triage:web", Pool: "routine", Instances: 2}},
		Jobs:              []JobUse{{RequestedBy: "pr-review", Jobs: 4}},
		PoolWorkers:       []PoolUse{{Pool: "reviewers", Workers: 3}},
		InteractiveAgents: 1,
	}
	if !reflect.DeepEqual(a.Last24h, want24) {
		t.Fatalf("alpha 24h = %+v\nwant %+v", a.Last24h, want24)
	}
	want7 := Usage{
		Officers:          []OfficerUse{{Officer: "triage:web", Pool: "judgement", Instances: 1}, {Officer: "triage:web", Pool: "routine", Instances: 2}},
		Jobs:              []JobUse{{RequestedBy: "", Jobs: 6}, {RequestedBy: "pr-review", Jobs: 9}},
		PoolWorkers:       []PoolUse{{Pool: "reviewers", Workers: 3}},
		InteractiveAgents: 2,
	}
	if !reflect.DeepEqual(a.Last7d, want7) {
		t.Fatalf("alpha 7d = %+v\nwant %+v", a.Last7d, want7)
	}
	b := got["beta"]
	if !reflect.DeepEqual(b.Last24h.Officers, []OfficerUse{{Officer: "cheap:web", Pool: "routine", Instances: 1}, {Officer: "release:web", Instances: 1}}) || b.Last24h.Jobs[0].Jobs != 5 || b.Last24h.InteractiveAgents != 0 {
		t.Fatalf("beta = %+v", b)
	}
}

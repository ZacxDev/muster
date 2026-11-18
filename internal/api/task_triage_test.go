package api

import (
	"testing"

	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
)

// TestDecideTaskTriagePinnedCases is the triage rule's table: every case a
// literal, the clear ones muster's and every doubtful one the chief's.
func TestDecideTaskTriagePinnedCases(t *testing.T) {
	byPR := plugins.SessionHint{ID: "mirror:ses-a", PRs: []string{"acme/widgets#7"}}
	byRepo := plugins.SessionHint{ID: "mirror:ses-b", ByRepo: true, Repo: "widgets"}
	other := plugins.SessionHint{ID: "mirror:ses-c", PRs: []string{"acme/widgets branch fix"}}
	weak := plugins.SessionHint{ID: "mirror:ses-d", Titles: []plugins.HintTitle{{Title: "fixing widgets"}}}
	agentWeak := plugins.SessionHint{ID: "agent:12", Titles: []plugins.HintTitle{{Title: "widgets"}}}
	agentByPR := plugins.SessionHint{ID: "agent:13", PRs: []string{"acme/widgets#7"}}
	claimed := []notes.Claim{{Source: notes.ClaimUpstream, Who: "press-bot"}}
	for _, c := range []struct {
		name string
		f    triageFacts
		want triageDecision
	}{
		{"nothing on it: the board", triageFacts{}, triageDecision{kind: triageBoard}},
		{"a gated task, nothing on it: the board (its Release buttons)", triageFacts{gated: true}, triageDecision{kind: triageBoard}},
		{"one agent on it, no session hinted: forward", triageFacts{forward: 1}, triageDecision{kind: triageForward}},
		{"one session by its pull request: route there", triageFacts{hints: []plugins.SessionHint{byPR}}, triageDecision{kind: triageRoute, subject: "mirror:ses-a"}},
		{"one session by its repository: route there", triageFacts{hints: []plugins.SessionHint{byRepo}}, triageDecision{kind: triageRoute, subject: "mirror:ses-b"}},
		{"a strong match that is ALSO a focus match: still strong", triageFacts{hints: []plugins.SessionHint{{ID: "mirror:ses-a", PRs: byPR.PRs,
			Titles: weak.Titles}}}, triageDecision{kind: triageRoute, subject: "mirror:ses-a"}},
		// The chief's, every one:
		{"two agents on it", triageFacts{forward: 2}, triageDecision{kind: triageChief}},
		{"an agent on it that cannot be sent to", triageFacts{stuck: 1}, triageDecision{kind: triageChief}},
		{"one reachable agent and one stuck", triageFacts{forward: 1, stuck: 1}, triageDecision{kind: triageChief}},
		{"one agent AND a session hinted", triageFacts{forward: 1, hints: []plugins.SessionHint{byPR}}, triageDecision{kind: triageChief}},
		{"one agent on a gated task", triageFacts{forward: 1, gated: true}, triageDecision{kind: triageChief}},
		{"two strong sessions", triageFacts{hints: []plugins.SessionHint{byPR, other}}, triageDecision{kind: triageChief}},
		{"a strong session and a weak one", triageFacts{hints: []plugins.SessionHint{byPR, weak}}, triageDecision{kind: triageChief}},
		{"a weak match only", triageFacts{hints: []plugins.SessionHint{weak}}, triageDecision{kind: triageChief}},
		{"an agent matched by focus only", triageFacts{hints: []plugins.SessionHint{agentWeak}}, triageDecision{kind: triageChief}},
		{"an agent session listed by a pull request is no mirrored session", triageFacts{hints: []plugins.SessionHint{agentByPR}}, triageDecision{kind: triageChief}},
		{"one strong session on a gated task", triageFacts{gated: true, hints: []plugins.SessionHint{byPR}}, triageDecision{kind: triageChief}},
		// A CLAIMED task (notes.Claims): left alone — no card, no chief turn —
		// whatever else is hinted, except the clean forward to its agent.
		{"claimed, nothing else on it: left alone (no card)", triageFacts{claims: claimed}, triageDecision{kind: triageClaimed}},
		{"claimed and gated: left alone", triageFacts{gated: true, claims: claimed}, triageDecision{kind: triageClaimed}},
		{"claimed, one strong session: left alone", triageFacts{claims: claimed, hints: []plugins.SessionHint{byPR}}, triageDecision{kind: triageClaimed}},
		{"claimed, a weak match: left alone", triageFacts{claims: claimed, hints: []plugins.SessionHint{weak}}, triageDecision{kind: triageClaimed}},
		{"claimed, a stuck agent: left alone", triageFacts{claims: claimed, stuck: 1}, triageDecision{kind: triageClaimed}},
		{"claimed, two agents: left alone", triageFacts{claims: claimed, forward: 2}, triageDecision{kind: triageClaimed}},
		{"claimed, one agent on it: still forwarded", triageFacts{claims: claimed, forward: 1}, triageDecision{kind: triageForward}},
		{"claimed, one agent and a hint: left alone", triageFacts{claims: claimed, forward: 1, hints: []plugins.SessionHint{byPR}}, triageDecision{kind: triageClaimed}},
	} {
		if got := decideTaskTriage(c.f); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestToolRefusedReadsBothRefusalShapes(t *testing.T) {
	for out, want := range map[string]bool{
		toolErr("no such task"):                 true,
		routeRefusal(RefusedWindowGone, "gone"): true,
		`{"task_id":1,"forwarded_to":{"id":2}}`: false,
		`not json`:                              true,
	} {
		if got := toolRefused(out); got != want {
			t.Errorf("toolRefused(%s) = %v, want %v", out, got, want)
		}
	}
}

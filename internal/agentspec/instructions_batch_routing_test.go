package agentspec

import (
	"strings"
	"testing"
)

// TestTheBatchRoutingSectionIsPinnedWhole pins the claude-code chief's
// route_many and reconcile section WHOLE against an independently typed
// literal (a keyword guard is walkable by rewording), and the reconcile text
// it quotes.
func TestTheBatchRoutingSectionIsPinnedWhole(t *testing.T) {
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	const reconcile = "Which task do you work under? Record it with your track_task tool (the task key, e.g. hq:<task id>, " +
		"or none with one line if this work is deliberately untracked), then reply with that key, or none plus the one line."
	if ReconcileRequestText != reconcile {
		t.Errorf("the reconcile text has changed:\ngot:  %q\nwant: %q", ReconcileRequestText, reconcile)
	}
	const want = "## The same request to several sessions — one route_many " +
		"The same request to more than one session is ONE `mcp__muster__route_many` call (`subjects`, `text`, " +
		"`owner_asked: true`), never several route_to_session calls: refusals come back in its result, and every answer " +
		"comes back in ONE digest when all have answered or at its deadline. Do not wait or poll for it. " +
		"To reconcile sessions with tasks: call `mcp__muster__fleet_list` with `active_within_hours` and read each " +
		"session's declared `tasks` or `untracked` — the session's own word. To the sessions that declared nothing " +
		"(`undeclared: true` lists them), send ONE route_many with exactly this text: \"" + reconcile + "\" Then compare " +
		"the keys with the open tasks from one compact read (status in progress, ids and titles only). Never match a " +
		"session to a task by its focus title or a keyword search. A fan-out this size belongs in a side chat: if your " +
		"owner asks for it in this chat and it is your primary one, suggest they open a side chat for it."
	if got := norm(chiefBatchRoutingSectionClaudeCode); got != want {
		t.Errorf("the batch routing section has changed.\ngot:  %q\nwant: %q\nThis is a whole-string pin: "+
			"if the reword is intended, retype the literal here and read the diff.", got, want)
	}
}

// TestTheBatchRoutingSectionShipsOnlyToTheClaudeCodeChiefWithRouting: the
// section is in the claude-code chief's prose with routing on — plugins on or
// off — and nowhere else: not with routing off (route_many is not served),
// and never in the Compose chief's (it is not offered route_many).
func TestTheBatchRoutingSectionShipsOnlyToTheClaudeCodeChiefWithRouting(t *testing.T) {
	for _, plugins := range []bool{false, true} {
		if !strings.Contains(ChiefInstructionsClaudeCode(plugins, true), chiefBatchRoutingSectionClaudeCode) {
			t.Errorf("plugins=%v routing on: the claude-code chief lacks the batch routing section", plugins)
		}
		if strings.Contains(ChiefInstructionsClaudeCode(plugins, false), "route_many") {
			t.Errorf("plugins=%v routing off: the claude-code chief is told of route_many", plugins)
		}
		for _, routing := range []bool{false, true} {
			if strings.Contains(ChiefInstructionsCompose(plugins, routing), "route_many") {
				t.Errorf("plugins=%v routing=%v: the Compose chief is told of route_many", plugins, routing)
			}
		}
	}
}

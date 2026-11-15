package api

import (
	"sort"
	"strings"
	"testing"
)

// teamSweepClasses classifies EVERY function in this package that builds a
// push (the codename ledger's set: codenameFirstSurfaces ∪ noCodenameFirst,
// itself pinned to the source by an AST walk) for the team withdrawal sweep
// (team_withdraw.go). Each is either one of withdrawableTeamKinds — a team
// push of that kind is later withdrawn, so a departing member's copy must be
// swept — or "never: <why>": it is never withdrawn from a team member. A new
// push function joins the codename ledger and then fails here until it says
// which, so the sweep and the fan-out cannot drift apart again (#683 round
// 1, 🟡4).
var teamSweepClasses = map[string]string{
	"attention.go attentionNotification":                   "asks",
	"reply_draft_tool.go Server.notifyDraftAwaitingNoting": "drafts",
	"outreach_batch.go Server.notifyBatchAwaiting":         "drafts",
	"push_task.go Server.notifyTaskDone":                   "tasks in review",
	"officer_cap_hold.go Server.officerCapHoldNotice":      "officer cap holds",
	"server.go Server.goNotify":                            "never: the sink; it builds no push of its own",
	"session_push.go Server.episodeNotification":           "never: a session episode is personal until 2b.3's state-pass arm runs for teams",
	"host_dispatch_done.go Server.announceHostOutcome":     "never: host dispatches are personal (host_dispatches is personal)",
	"succession.go Server.armSuccessionPush":               "never: succession is a person's chief's; refused on team sessions (G4)",
	"session_route_notify.go Server.notifyChiefRouted":     "never withdrawn: information only, replaced by its tag",
	"dispatch_hold_tools.go Server.pushSessionDispatch":    "never withdrawn: information only, replaced by its tag",
	"agent_outcome.go Server.pushOutcome":                  "never withdrawn: an outcome is replaced by the next under its tag",
	"chief_owner_release.go Server.applyGateRelease":       "never withdrawn: information only",
	"connectors.go Server.onConnectorRequested":            "never: connector requests are personal (connectors are never shared)",
	"pages.go Server.pageResponded":                        "never: pages are personal",
	"plugin_drafts.go Server.notifyDraftRefused":           "never withdrawn: information only",
	"plugin_drafts.go Server.notifyOfficerNotTold":         "never withdrawn: information only",
	"plugin_pairing.go Server.pairRequested":               "never: pairing requests are personal",
	"push_task.go Server.flushCreatedTasks":                "never withdrawn: replaced by its tag",
	"push_task.go Server.notifyAgentRunning":               "never withdrawn: information only",
	"push_task.go Server.flushCommentedTasks":              "never withdrawn: replaced by its tag",
	"push_task.go Server.teamTaskCommented":                "never withdrawn: replaced by its tag",
}

func TestEveryPushIsClassifiedForTheTeamSweep(t *testing.T) {
	kinds := map[string]bool{}
	for _, k := range withdrawableTeamKinds {
		kinds[k.name] = true
	}
	if len(kinds) < 4 {
		t.Fatalf("positive control: %d withdrawable kinds; want the four 2b.1 ships", len(kinds))
	}
	var want []string
	for f := range codenameFirstSurfaces {
		want = append(want, f)
	}
	for f := range noCodenameFirst {
		want = append(want, f)
	}
	var got []string
	used := map[string]bool{}
	for f, c := range teamSweepClasses {
		got = append(got, f)
		switch {
		case strings.HasPrefix(c, "never"):
		case kinds[c]:
			used[c] = true
		default:
			t.Errorf("%s is classified %q, which is neither a withdrawableTeamKinds name nor \"never…\"", f, c)
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the push functions classified for the team sweep differ from the codename ledger's:\n  got  %v\n  want %v\nSay whether a new push is withdrawn later (and so belongs in withdrawableTeamKinds) or never.", got, want)
	}
	for k := range kinds {
		if !used[k] {
			t.Errorf("withdrawableTeamKinds has %q, which no push function is classified as", k)
		}
	}
}

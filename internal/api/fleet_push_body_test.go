package api

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/fleet"
)

// TestThePushNamesEveryClass: every class a proposal can have (fleet.ClassVerb's)
// has the push's own question, naming the session once — none falls back to
// the generic line.
func TestThePushNamesEveryClass(t *testing.T) {
	if len(fleetPushAsks) != len(fleet.ClassVerb) {
		t.Errorf("%d push questions for %d classes", len(fleetPushAsks), len(fleet.ClassVerb))
	}
	for class := range fleet.ClassVerb {
		ask, ok := fleetPushAsks[class]
		if !ok || strings.Count(ask, "%s") != 1 {
			t.Errorf("class %q: push question %q", class, ask)
		}
	}
}

// TestThePushBodyLiterals pins the push's words for the shapes the pg test
// does not drive: a wrap and a composed answer, the generic line when the
// newest could not be read (and its url naming no proposal), and a name with
// control and format characters.
func TestThePushBodyLiterals(t *testing.T) {
	wrap := fleet.Action{ID: 41, Class: fleet.ClassWrap, Text: "Write your handoff (what is done, what is left, where), then stop.",
		Reason: "its pull request merged", Evidence: fleet.Evidence{Label: "muster-asks-ux", SubjectID: "mirror:k-1"}}
	cases := []struct {
		n          int
		a          fleet.Action
		body, link string
	}{
		{1, wrap, "Wrap 'muster-asks-ux'? Nothing is sent until you approve it.", "/tasks?chief=open&fleet=proposed&proposal=41"},
		{4, fleet.Action{ID: 7, Class: fleet.ClassAnswerComposed, Text: "Keep the old flag.", Evidence: fleet.Evidence{Label: "ledger-cleanup"}},
			"Answer 'ledger-cleanup'? +3 more. Nothing is sent until you approve them.", "/tasks?chief=open&fleet=proposed&proposal=7"},
		{2, fleet.Action{}, "Your chief proposed what to send your sessions. Nothing is sent until you approve it in the fleet digest.",
			"/tasks?chief=open&fleet=proposed"},
		{1, fleet.Action{ID: 9, Class: fleet.ClassClose, Evidence: fleet.Evidence{Label: "a\x1b[2Jb​c\n  d"}},
			"Close 'a [2Jb c d'? Nothing is sent until you approve it.", "/tasks?chief=open&fleet=proposed&proposal=9"},
		// No label: the card's own fallback, the session's fleet id.
		{1, fleet.Action{ID: 3, Class: fleet.ClassContinue, SubjectKey: "k-3", Evidence: fleet.Evidence{SubjectID: "mirror:k-3"}},
			"Continue 'mirror:k-3'? Nothing is sent until you approve it.", "/tasks?chief=open&fleet=proposed&proposal=3"},
	}
	for _, tc := range cases {
		n := fleetPushNotification(tc.n, tc.a)
		if n.Body != tc.body || n.Data["url"] != tc.link || len(n.Data) != 1 || n.AskActions || n.Tag != "fleet-proposals" {
			t.Errorf("push(%d, %+v):\n body %q\n want %q\n data %v (want url %q)", tc.n, tc.a, n.Body, tc.body, n.Data, tc.link)
		}
		if tc.a.Text != "" && strings.Contains(n.Body, tc.a.Text) || tc.a.Reason != "" && strings.Contains(n.Body, tc.a.Reason) {
			t.Errorf("the body carries the proposal's text or reason: %q", n.Body)
		}
	}
}

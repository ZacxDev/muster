package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/focus"
)

// TestASessionQuestionToTheChiefSaysWhereTheWindowIs: the block the chief
// reads names the asking window by askSource — its exact tmux target, or that
// no window shows it (never an invented one).
func TestASessionQuestionToTheChiefSaysWhereTheWindowIs(t *testing.T) {
	s := whereServer(t)
	hosts := s.ext.Mirror.Hosts(s.attentionPrincipal(t.Context(), 1))
	for _, c := range []struct {
		sid, label, want string
	}{
		{whereLabelledSID, "mango:1 @ ws", "\n| where: mango:1 · tmux scratch3:5 @ ws\n"},
		{whereGoneSID, "pear:2 @ ws", "\n| where: pear:2 @ ws (not shown in any mirrored window now)\n"},
	} {
		q := attention.Ask{ID: 7, Asker: attention.Asker{MirrorHost: "ws", MirrorSession: c.sid, Label: c.label}, Question: "Which branch?"}
		if got := renderChiefQuestion(q, askSource(q, hosts), nil, hintTestNonce); !strings.Contains(got, c.want) {
			t.Errorf("%s: block lacks %q:\n%s", c.sid, c.want, got)
		}
	}
}

// TestTheOverviewAndTheRouteCandidatesCarryEachWindowsWhere: the live
// sessions sessions_overview (and the session hints) is built from, and the
// candidates a refused route subject lists, carry each window's where.
func TestTheOverviewAndTheRouteCandidatesCarryEachWindowsWhere(t *testing.T) {
	s := whereServer(t)
	ctx := s.attentionPrincipal(t.Context(), 1)
	want := map[string]string{
		"mirror:" + whereLabelledSID:   "mango:1 · tmux scratch3:5 @ ws",
		"mirror:" + whereUnlabelledSID: "tmux build:7 @ ws",
	}
	live := s.liveSessions(ctx, 0)
	if len(live) != len(want) {
		t.Fatalf("%d live sessions, want %d", len(live), len(want))
	}
	for _, ls := range live {
		if ls.where != want[ls.sub.String()] {
			t.Errorf("%s: where %q, want %q", ls.sub, ls.where, want[ls.sub.String()])
		}
	}
	s.ext.Focus = &memFocus{rows: map[focus.Subject]focus.Focus{}}
	var ov struct {
		Sessions []struct {
			ID    string `json:"id"`
			Where string `json:"where"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(s.toolSessionsOverview(ctx, agents.Agent{ID: 999}, `{}`)), &ov); err != nil || len(ov.Sessions) != len(want) {
		t.Fatalf("sessions_overview: %+v %v", ov, err)
	}
	for _, row := range ov.Sessions {
		if row.Where != want[row.ID] {
			t.Errorf("sessions_overview %s: where %q, want %q", row.ID, row.Where, want[row.ID])
		}
	}
	_, refusal := s.routeSubjectFor(ctx, "nope")
	var r struct {
		Candidates []routeCandidate `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(refusal, "error: ")), &r); err != nil || len(r.Candidates) != len(want) {
		t.Fatalf("refusal %q: %v", refusal, err)
	}
	for _, c := range r.Candidates {
		if c.Where != want[c.Subject] {
			t.Errorf("%s: where %q, want %q", c.Subject, c.Where, want[c.Subject])
		}
	}
}

const hintTestNonce = "0123456789abcdef"

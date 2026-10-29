package ui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestTheWorkChipSaysWhatRunsAndNeverMore: each shape of mirror.WindowWork,
// with pairwise-distinct counts so a chip that swapped the fields or
// hardcoded one cannot pass. "1+" is a lower bound and says so; a count at the
// wire's bound reads "99+"; nothing running draws no chip at all.
func TestTheWorkChipSaysWhatRunsAndNeverMore(t *testing.T) {
	for _, c := range []struct {
		w           mirror.WindowWork
		text, label string
	}{
		{mirror.WindowWork{Subagents: 2, Shells: 1}, "2 subagents · 1 shell", "2 subagents and 1 background shell running"},
		{mirror.WindowWork{Subagents: 1}, "1 subagent", "1 subagent running"},
		{mirror.WindowWork{Shells: 3}, "3 shells", "3 background shells running"},
		{mirror.WindowWork{Shells: 1, ShellsAtLeast: true}, "1+ shell", "at least 1 background shell running"},
		{mirror.WindowWork{Subagents: 4, Shells: 2, ShellsAtLeast: true}, "4 subagents · 2+ shells", "4 subagents and at least 2 background shells running"},
		{mirror.WindowWork{Subagents: mirror.MaxWorkCount, Shells: mirror.MaxWorkCount, ShellsAtLeast: true}, "99+ subagents · 99+ shells", "99+ subagents and 99+ background shells running"},
		{mirror.WindowWork{}, "", ""},
	} {
		if got := workChipText(c.w); got != c.text {
			t.Errorf("workChipText(%+v) = %q, want %q", c.w, got, c.text)
		}
		if got := workChipLabel(c.w); got != c.label {
			t.Errorf("workChipLabel(%+v) = %q, want %q", c.w, got, c.label)
		}
	}
}

// TestADispatchedAgentsCardShowsTheSameWorkChip: a dispatched claude-code
// agent's page (ccd's report, AgentCardView.Work) draws THE work chip in its
// header's status strip — the one component, with its accessible label — and
// none when nothing runs; and its status poll carries the chip it was drawn
// with, so an expired report can be told apart from a current one. (The
// Agents list's card, where this chip was first drawn, is retired.)
//
// Red before: the agent page drew no chip and its poll carried no work.
func TestADispatchedAgentsCardShowsTheSameWorkChip(t *testing.T) {
	w := mirror.WindowWork{Subagents: 3, Shells: 2, ShellsAtLeast: true}
	html := renderNode(t, agentStrip(AgentCardView{ID: 9, Name: "worker", Status: "running", Kind: "claude-code", Work: w}))
	if !regexp.MustCompile(`id="` + AgentStripID + `"[^\n]*data-mirror-work=""`).MatchString(html) {
		t.Fatalf("the chip is not in the agent page's status strip: %s", html)
	}
	if got, want := strings.Count(html, "data-mirror-work"), 1; got != want {
		t.Errorf("the card draws %d chips, want %d", got, want)
	}
	// The mirrored card's own rendering of the same value, byte for byte.
	if chip := renderNode(t, workChip(w)); !strings.Contains(html, chip) {
		t.Errorf("the agent page's chip is not the work chip %s", chip)
	}
	for _, want := range []string{">3 subagents · 2+ shells<", `class="sr-only">3 subagents and at least 2 background shells running<`,
		`/ui/agents/9/status?awaiting=0&amp;work=3.2p"`} {
		if !strings.Contains(html, want) {
			t.Errorf("the card lacks %s", want)
		}
	}
	plain := renderNode(t, agentStrip(AgentCardView{ID: 9, Name: "worker", Status: "running", Kind: "claude-code"}))
	if strings.Contains(plain, "data-mirror-work") || !strings.Contains(plain, `work=0.0"`) {
		t.Errorf("a card with nothing running draws a chip, or its poll does not say none: %s", plain)
	}
}

// TestTheWorkPollTokenTellsEveryChipApart: two reports share a token only when
// they draw the same chip — the fields are not swapped, and a lower bound is
// not an exact count.
func TestTheWorkPollTokenTellsEveryChipApart(t *testing.T) {
	seen := map[string]mirror.WindowWork{}
	for _, w := range []mirror.WindowWork{{}, {Subagents: 2, Shells: 1}, {Subagents: 1, Shells: 2}, {Shells: 1}, {Shells: 1, ShellsAtLeast: true}, {Subagents: 12}, {Subagents: 1, Shells: 2, ShellsAtLeast: true}} {
		tok := WorkPollToken(w)
		if prev, dup := seen[tok]; dup {
			t.Errorf("%+v and %+v share the token %q", prev, w, tok)
		}
		seen[tok] = w
	}
	if got := WorkPollToken(mirror.WindowWork{Subagents: 4, Shells: 7, ShellsAtLeast: true}); got != "4.7p" {
		t.Errorf("WorkPollToken = %q, want %q", got, "4.7p")
	}
}

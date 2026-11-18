package agentoutcome

import (
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
)

// t0 is the fixture's kickoff time; every other instant is an offset from it.
var t0 = time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)

func at(d time.Duration) *time.Time { v := t0.Add(d); return &v }

// started is a running agent kicked off at t0 with activity at t0 and nothing
// else: the row Decide sees right after a dispatch's first turn was sent.
func started() agents.Agent {
	return agents.Agent{ID: 7, Name: "keen-wren", Status: agents.StatusRunning, KickedOff: true,
		Outcome: Running, OutcomeAt: at(0), ActivityAt: at(0), CreatedAt: t0.Add(-time.Minute), UpdatedAt: t0}
}

// th is the operator's defaults — and the fixtures below overshoot them by a
// second or stop a second short, never land on a multiple of the step.
var th = DefaultThresholds()

// TestDecideNoReplySinceKickoff: no first reply and no activity for NoReply is
// stuck with the operator's sentence; a second short is not; activity inside the
// window (a stream event, a work report) moves the clock, so a working turn is
// never flipped. Measured at both sides of the boundary.
func TestDecideNoReplySinceKickoff(t *testing.T) {
	a := started()
	if v := Decide(Input{Agent: a}, t0.Add(th.NoReply-time.Second), th); v.Action != None {
		t.Fatalf("a second short of %s: %+v; want None", th.NoReply, v)
	}
	v := Decide(Input{Agent: a}, t0.Add(th.NoReply+time.Second), th)
	if v.Action != MarkStuck || v.Reason != "no reply since kickoff" {
		t.Fatalf("past %s with no reply: %+v; want MarkStuck %q", th.NoReply, v, ReasonNoReply)
	}
	// Activity four minutes in: five minutes from it is not yet reached.
	a.ActivityAt = at(4 * time.Minute)
	if v := Decide(Input{Agent: a}, t0.Add(th.NoReply+time.Second), th); v.Action != None {
		t.Fatalf("activity at 4m, judged at 5m1s: %+v; want None (the turn is working)", v)
	}
	// A work report now (the pod's subagents running) is activity now.
	a = started()
	if v := Decide(Input{Agent: a, Working: true}, t0.Add(2*th.Silent), th); v.Action != None {
		t.Fatalf("a pod reporting work: %+v; want None", v)
	}
	// A transcript row is activity too (agents.LastActivity, the one definition).
	if v := Decide(Input{Agent: a, LastMessage: t0.Add(3 * time.Minute)}, t0.Add(th.NoReply+time.Second), th); v.Action != None {
		t.Fatalf("a transcript row at 3m: %+v; want None", v)
	}
}

// TestDecideSilentAfterReplying: once it replied, Silent of no activity is
// stuck ("silent for 30 min"); NoReply of quiet is not.
func TestDecideSilentAfterReplying(t *testing.T) {
	a := started()
	a.FirstReplyAt = at(time.Minute)
	a.ActivityAt = at(time.Minute)
	if v := Decide(Input{Agent: a}, t0.Add(time.Minute+th.NoReply+time.Second), th); v.Action != None {
		t.Fatalf("replied, quiet past the NO-REPLY threshold: %+v; want None (the silence threshold applies)", v)
	}
	if v := Decide(Input{Agent: a}, t0.Add(time.Minute+th.Silent-time.Second), th); v.Action != None {
		t.Fatalf("a second short of %s silent: %+v; want None", th.Silent, v)
	}
	v := Decide(Input{Agent: a}, t0.Add(time.Minute+th.Silent+time.Second), th)
	if v.Action != MarkStuck || v.Reason != "silent for 30 min" {
		t.Fatalf("past %s silent: %+v; want MarkStuck %q", th.Silent, v, SilentReason(th.Silent))
	}
	// A configured threshold names itself.
	custom := Thresholds{NoReply: 90 * time.Second, Silent: 2 * time.Hour}
	if v := Decide(Input{Agent: a}, t0.Add(3*time.Hour), custom); v.Reason != "silent for 2 h" {
		t.Fatalf("custom threshold reason %q; want %q", v.Reason, "silent for 2 h")
	}
}

// TestDecideRecoversAndLeavesEndedAgentsAlone: a stuck agent with activity since
// its verdict recovers, one without stays; done, failed, untracked, stopped,
// errored and never-kicked-off agents are not judged at all.
func TestDecideRecoversAndLeavesEndedAgentsAlone(t *testing.T) {
	a := started()
	a.Outcome, a.OutcomeAt = Stuck, at(th.NoReply+time.Second)
	if v := Decide(Input{Agent: a}, t0.Add(time.Hour), th); v.Action != None {
		t.Fatalf("stuck, nothing since: %+v; want None", v)
	}
	a.ActivityAt = at(th.NoReply + 2*time.Second)
	if v := Decide(Input{Agent: a}, t0.Add(time.Hour), th); v.Action != Recover {
		t.Fatalf("stuck, then activity: %+v; want Recover", v)
	}
	for _, c := range []struct {
		name string
		mut  func(*agents.Agent)
	}{
		{"done", func(a *agents.Agent) { a.Outcome = Done }},
		{"failed", func(a *agents.Agent) { a.Outcome = Failed }},
		{"untracked", func(a *agents.Agent) { a.Outcome = "" }},
		{"stopped", func(a *agents.Agent) { a.Status = agents.StatusStopped }},
		{"errored", func(a *agents.Agent) { a.Status = agents.StatusError }},
		{"first turn not delivered yet", func(a *agents.Agent) { a.KickedOff = false }},
	} {
		a := started()
		c.mut(&a)
		if v := Decide(Input{Agent: a}, t0.Add(10*th.Silent), th); v.Action != None {
			t.Errorf("%s: %+v; want None", c.name, v)
		}
	}
}

func TestParseThreshold(t *testing.T) {
	for _, c := range []struct {
		in   string
		want time.Duration
		err  string
	}{
		{"", 5 * time.Minute, ""},
		{"  ", 5 * time.Minute, ""},
		{"90s", 90 * time.Second, ""},
		{"2h", 2 * time.Hour, ""},
		{"1s", time.Second, ""},
		{"500ms", 0, "under 1s"},
		{"soon", 0, "not a duration"},
	} {
		got, err := ParseThreshold(c.in, 5*time.Minute)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%q: err %v; want one containing %q", c.in, err, c.err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%q = %s, %v; want %s", c.in, got, err, c.want)
		}
	}
}

func TestFirstLineAndHumanDuration(t *testing.T) {
	if got := FirstLine("\n\n  Opened draft PR #12  \nmore", 120); got != "Opened draft PR #12" {
		t.Errorf("FirstLine = %q", got)
	}
	if got := FirstLine(strings.Repeat("x", 30), 10); got != strings.Repeat("x", 9)+"…" {
		t.Errorf("FirstLine cut = %q", got)
	}
	for d, want := range map[time.Duration]string{30 * time.Minute: "30 min", 2 * time.Hour: "2 h", 45 * time.Second: "45s", 90 * time.Minute: "90 min"} {
		if got := HumanDuration(d); got != want {
			t.Errorf("HumanDuration(%s) = %q; want %q", d, got, want)
		}
	}
}

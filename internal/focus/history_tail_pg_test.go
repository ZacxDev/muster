package focus

import (
	"fmt"
	"testing"
	"time"
)

// A HOST NAMED IN FULL BY THE MESSAGE, BY ITS NUMBERED TAIL IN THE FOCUS
// (terms.go's tail keys) against a real Postgres. Every name is synthetic.

// fleetAlert is the shape of a chat channel's fleet-alert post, as a chat
// plugin delivers it: a time, the bot's name, then one degraded host by its
// full name, its rack, its GPU UUIDs and durations.
const fleetAlert = "[22:35] Fleet Watch: GPU Fleet — DEGRADED — rack-9-b: 25/25 Ready · 191/200 GPUs below expected " +
	"node/GPU count ⟲ 2 reboots/7d · 5 nodes w/ GPU loss Counts above are Kubernetes-reported and can lag a hardware " +
	"fault — see rows below. Degraded nodes (GPU hardware): gpuhost-4090-7-12 rack-9-b 0/8 GPUs — faulted GPU @ PCI " +
	"4E:00.0 (Xid 79, off bus; kernel log) [UUID GPU-0d1e2f3a-4b5c-6d7e-8f90-a1b2c3d4e5f6] · root cause in 1 boot (7d) " +
	"[first seen 32h45m ago] (worker) [7d: 0 reboots · 32h50m at <8 GPUs]"

// 🔴 TestAHostTheFocusNamesByItsTailGetsAHint: the alert names host
// gpuhost-4090-7-12 in full; the session on that host's GPU fault wrote its
// focus the way a summarizer told never to quote hostnames writes it —
// "node -7-12" — and nothing else the alert carries (no rack, no tool name).
// On the base ref this named nobody: "-7-12" holds no letter, so it was no
// term, and the alert's other identifiers were not in the focus. Now the
// host's numbered tail ("7-12") is a term of the alert's, and the session's
// "-7-12" carries it, so the alert names that session — and only it: not one
// whose focus names host -7-13, nor one with "7-12" inside a longer number.
func TestAHostTheFocusNamesByItsTailGetsAHint(t *testing.T) {
	s, pool := pgStore(t)
	on := AgentSubject(mustAgent(t, pool, 2, "focus-tail-on").ID)
	near := AgentSubject(mustAgent(t, pool, 2, "focus-tail-near").ID)
	longer := AgentSubject(mustAgent(t, pool, 2, "focus-tail-longer").ID)
	write(t, s, 2, on, "GPU worker detection and alerting gap",
		"Investigating why node -7-12 has sat at 0/8 GPUs for a day without a fault report; the GPU looks off the bus.", "")
	write(t, s, 2, near, "GPU worker detection on the second rack",
		"Node -7-13 is back at 8/8 GPUs after its reboot; watching the GPU count.", "")
	write(t, s, 2, longer, "Reconciling GPU invoices",
		"Invoice 17-123 lists 8 GPUs per node; checking the count.", "")
	live := Live{Agents: []int64{on.AgentID(), near.AgentID(), longer.AgentID()}}
	ms, err := s.MatchHistory(as(2), "Discord #fleet-alerts: 1 message\n"+fleetAlert, live, time.Now().Add(-HistoryWindow), 2, 5)
	if err != nil || fmt.Sprint(subjectsOf(ms)) != fmt.Sprint([]string{on.String()}) {
		t.Fatalf("the alert named %v (%v); want [%s], the session on host -7-12", subjectsOf(ms), err, on)
	}
	// What it was listed by, in the message's own words: the tail, and the
	// host it is the tail of.
	if fmt.Sprint(ms[0].Keys) != "[7-12 (of gpuhost-4090-7-12)]" {
		t.Fatalf("the match's keys %q; want the host's tail and the host", ms[0].Keys)
	}
}

// TestAHostNamedInFullOnBothSidesIsOneTerm: a focus naming the host in full
// carries both the host and its tail; they are one identifier, counted once
// and listed once (the host), so a session that names a host in full does
// not outrank one that also shares a second, different identifier.
func TestAHostNamedInFullOnBothSidesIsOneTerm(t *testing.T) {
	s, pool := pgStore(t)
	full := AgentSubject(mustAgent(t, pool, 2, "focus-tail-full").ID)
	write(t, s, 2, full, "GPU fault on gpuhost-4090-7-12", "The GPU is off the bus.", "")
	live := Live{Agents: []int64{full.AgentID()}}
	ms, err := s.MatchHistory(as(2), fleetAlert, live, time.Now().Add(-HistoryWindow), 1, 5)
	if err != nil || len(ms) != 1 {
		t.Fatalf("match: %v %v", subjectsOf(ms), err)
	}
	if fmt.Sprint(ms[0].Keys) != "[gpuhost-4090-7-12]" {
		t.Fatalf("keys %q; want the host once", ms[0].Keys)
	}
	// One identifier, plus the words it shares ("gpu", "bus"…): never two
	// identifiers for one host.
	if ms[0].Terms < 2 {
		t.Fatalf("terms %d; want the host and its shared words", ms[0].Terms)
	}
}

// TestABareNumberPairInAMessageNamesNothing: a message's own "7-12" (a score,
// a range) is not a term — only a host's tail is — so a sender cannot name the
// session above by writing numbers.
func TestABareNumberPairInAMessageNamesNothing(t *testing.T) {
	s, pool := pgStore(t)
	on := AgentSubject(mustAgent(t, pool, 2, "focus-tail-bare").ID)
	write(t, s, 2, on, "GPU worker detection and alerting gap",
		"Investigating why node -7-12 has sat at 0/8 GPUs for a day without a fault report.", "")
	live := Live{Agents: []int64{on.AgentID()}}
	if ms, _ := s.MatchHistory(as(2), "The game ended 7-12; the node count and the GPU report are below.", live,
		time.Now().Add(-HistoryWindow), 1, 5); len(ms) != 0 {
		t.Fatalf("a bare number pair named %v", subjectsOf(ms))
	}
	// Control: the host named in full does.
	if ms, _ := s.MatchHistory(as(2), "gpuhost-4090-7-12 is down; the node count and the GPU report are below.", live,
		time.Now().Add(-HistoryWindow), 2, 5); len(ms) != 1 {
		t.Fatalf("control: the host named %v", subjectsOf(ms))
	}
}

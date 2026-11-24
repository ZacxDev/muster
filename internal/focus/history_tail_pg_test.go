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
	"fault — see rows below. Degraded nodes (GPU hardware): gpuhost-4090-52-23 rack-9-b 0/8 GPUs — faulted GPU @ PCI " +
	"4E:00.0 (Xid 79, off bus; kernel log) [UUID GPU-0d1e2f3a-4b5c-6d7e-8f90-a1b2c3d4e5f6] · root cause in 1 boot (7d) " +
	"[first seen 32h45m ago] (worker) [7d: 0 reboots · 32h50m at <8 GPUs]"

// 🔴 TestAHostTheFocusNamesByItsTailGetsAHint: the alert names host
// gpuhost-4090-52-23 in full; the session on that host's GPU fault wrote its
// focus the way a summarizer told never to quote hostnames writes it —
// "node -52-23" — and nothing else the alert carries (no rack, no tool name).
// On the base ref this named nobody: "-52-23" holds no letter, so it was no
// term, and the alert's other identifiers were not in the focus. Now the
// host's numbered tail ("52-23") is a term of the alert's, and the session's
// "-52-23" carries it, so the alert names that session — and only it: not one
// whose focus names host -52-24, nor one with "52-23" inside a longer number,
// nor one whose focus names ANOTHER host with the same tail in full
// (gpuhost-3090-52-23), nor one whose focus writes a date (2000-11-23).
func TestAHostTheFocusNamesByItsTailGetsAHint(t *testing.T) {
	s, pool := pgStore(t)
	on := AgentSubject(mustAgent(t, pool, 2, "focus-tail-on").ID)
	near := AgentSubject(mustAgent(t, pool, 2, "focus-tail-near").ID)
	longer := AgentSubject(mustAgent(t, pool, 2, "focus-tail-longer").ID)
	other := AgentSubject(mustAgent(t, pool, 2, "focus-tail-other").ID)
	dated := AgentSubject(mustAgent(t, pool, 2, "focus-tail-dated").ID)
	write(t, s, 2, on, "GPU worker detection and alerting gap",
		"Investigating why node -52-23 has sat at 0/8 GPUs for a day without a fault report; the GPU looks off the bus.", "")
	write(t, s, 2, near, "GPU worker detection on the second rack",
		"Node -52-24 is back at 8/8 GPUs after its reboot; watching the GPU count.", "")
	write(t, s, 2, longer, "Reconciling GPU invoices",
		"Invoice 152-230 lists 8 GPUs per node; checking the count.", "")
	write(t, s, 2, other, "GPU fault on the older rack",
		"gpuhost-3090-52-23 is at 0/8 GPUs, the GPU off the bus; filing the fault report.", "")
	write(t, s, 2, dated, "GPU fault reports for the week",
		"The node fault report from 2000-11-23 lists GPUs off the bus.", "")
	live := Live{Agents: []int64{on.AgentID(), near.AgentID(), longer.AgentID(), other.AgentID(), dated.AgentID()}}
	ms, err := s.MatchHistory(as(2), "Discord #fleet-alerts: 1 message\n"+fleetAlert, live, time.Now().Add(-HistoryWindow), 2, 5)
	if err != nil || fmt.Sprint(subjectsOf(ms)) != fmt.Sprint([]string{on.String()}) {
		t.Fatalf("the alert named %v (%v); want [%s], the session on host -52-23", subjectsOf(ms), err, on)
	}
	// What it was listed by, in the message's own words: the tail, and the
	// host it is the tail of.
	if fmt.Sprint(ms[0].Keys) != "[52-23 (of gpuhost-4090-52-23)]" {
		t.Fatalf("the match's keys %q; want the host's tail and the host", ms[0].Keys)
	}
}

// TestAHostNamedInFullOnBothSidesIsOneTerm: a focus row naming the host in
// full AND by its short form carries both the host and its tail; they are one
// identifier, listed once (the host), so naming a host twice does not outrank
// a session that shares a second, different identifier.
func TestAHostNamedInFullOnBothSidesIsOneTerm(t *testing.T) {
	s, pool := pgStore(t)
	full := AgentSubject(mustAgent(t, pool, 2, "focus-tail-full").ID)
	write(t, s, 2, full, "GPU fault on gpuhost-4090-52-23", "Node -52-23's GPU is off the bus.", "")
	live := Live{Agents: []int64{full.AgentID()}}
	ms, err := s.MatchHistory(as(2), fleetAlert, live, time.Now().Add(-HistoryWindow), 1, 5)
	if err != nil || len(ms) != 1 {
		t.Fatalf("match: %v %v", subjectsOf(ms), err)
	}
	if fmt.Sprint(ms[0].Keys) != "[gpuhost-4090-52-23]" {
		t.Fatalf("keys %q; want the host once", ms[0].Keys)
	}
}

// TestABareNumberPairInAMessageNamesNothing: a message's own "52-23" (a score,
// a range) is not a term — only a host's tail is — so a sender cannot name the
// session above by writing numbers, nor by writing a token that is not shaped
// like a host (ab-52-23).
func TestABareNumberPairInAMessageNamesNothing(t *testing.T) {
	s, pool := pgStore(t)
	on := AgentSubject(mustAgent(t, pool, 2, "focus-tail-bare").ID)
	write(t, s, 2, on, "GPU worker detection and alerting gap",
		"Investigating why node -52-23 has sat at 0/8 GPUs for a day without a fault report.", "")
	live := Live{Agents: []int64{on.AgentID()}}
	for _, text := range []string{
		"The game ended 52-23; the node count and the GPU report are below.",
		"ab-52-23 is down; the node count and the GPU report are below.",
	} {
		if ms, _ := s.MatchHistory(as(2), text, live, time.Now().Add(-HistoryWindow), 1, 5); len(ms) != 0 {
			t.Errorf("%q named %v", text, subjectsOf(ms))
		}
	}
	// Control: the host named in full does.
	if ms, _ := s.MatchHistory(as(2), "gpuhost-4090-52-23 is down; the node count and the GPU report are below.", live,
		time.Now().Add(-HistoryWindow), 2, 5); len(ms) != 1 {
		t.Fatalf("control: the host named %v", subjectsOf(ms))
	}
}

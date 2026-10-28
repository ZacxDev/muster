package api

import (
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

// pendingLedger is a ledger holding one call issued at a turn of n tokens.
func pendingLedger(n int64) cctranscript.ToolContext {
	var l cctranscript.ToolContext
	l.Observe(cctranscript.Event{Kind: cctranscript.KindToolCall, ToolID: "toolu_p", Bytes: 23, Usage: &cctranscript.Usage{Tokens: n, Output: 71}})
	return l
}

// measuredBy is the cost l settles for toolu_p once its result and a next turn
// of next tokens arrive (0 when none).
func measuredBy(l cctranscript.ToolContext, next int64) int64 {
	l.Observe(cctranscript.Event{Kind: cctranscript.KindToolResult, ToolID: "toolu_p", Bytes: 909})
	for _, c := range l.Observe(cctranscript.Event{Kind: cctranscript.KindText, Usage: &cctranscript.Usage{Tokens: next, Output: 13}}) {
		if c.Kind == cctranscript.CostMeasured {
			return c.Tokens
		}
	}
	return 0
}

func TestALedgerCheckpointIsTakenOnlyAtTheExactPlace(t *testing.T) {
	var cps ledgerCheckpoints
	k := ledgerKey{owner: 7, host: "host-a", session: "s1", window: "1"}
	cps.put(k, "gen-a", 4401, pendingLedger(30113))

	l, ok := cps.take(k, "gen-a", 4401)
	if !ok {
		t.Fatal("the exact place found no checkpoint")
	}
	// 30113 → 31337: +1224, the call the checkpoint held.
	if got := measuredBy(l, 31337); got != 1224 {
		t.Fatalf("the restored ledger measured %d, want 1224", got)
	}
	for _, c := range []struct {
		name   string
		k      ledgerKey
		gen    string
		offset int64
	}{
		{"another offset", k, "gen-a", 4402},
		{"another transcript", k, "gen-b", 4401},
		{"another viewer", ledgerKey{owner: 8, host: "host-a", session: "s1", window: "1"}, "gen-a", 4401},
		{"another window", ledgerKey{owner: 7, host: "host-a", session: "s1", window: "2"}, "gen-a", 4401},
	} {
		if _, ok := cps.take(c.k, c.gen, c.offset); ok {
			t.Errorf("%s: a checkpoint was taken", c.name)
		}
	}
	// A later batch replaces the window's checkpoint.
	cps.put(k, "gen-a", 5120, pendingLedger(30113))
	if _, ok := cps.take(k, "gen-a", 4401); ok {
		t.Error("a superseded checkpoint was still taken")
	}
	if _, ok := cps.take(k, "gen-a", 5120); !ok {
		t.Error("the latest checkpoint was not taken")
	}
	// A place with no transcript yet records nothing.
	cps.put(ledgerKey{owner: 7, host: "host-a", session: "s9"}, "", 0, pendingLedger(30113))
	if len(cps.m) != 1 {
		t.Errorf("%d checkpoints, want 1", len(cps.m))
	}
}

func TestALedgerCheckpointIsACopyAndExpires(t *testing.T) {
	now := time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)
	cps := ledgerCheckpoints{now: func() time.Time { return now }}
	k := ledgerKey{owner: 3, host: "host-b", session: "s2", window: "0"}
	cps.put(k, "gen-c", 777, pendingLedger(21011))
	a, _ := cps.take(k, "gen-c", 777)
	measuredBy(a, 22222) // moves a, not the stored one
	b, ok := cps.take(k, "gen-c", 777)
	if !ok || measuredBy(b, 22222) != 1211 {
		t.Fatal("taking a checkpoint moved the stored one")
	}
	now = now.Add(ledgerCheckpointTTL + time.Second)
	if _, ok := cps.take(k, "gen-c", 777); ok {
		t.Error("an expired checkpoint was taken")
	}
}

func TestLedgerCheckpointsAreBounded(t *testing.T) {
	var cps ledgerCheckpoints
	for i := 0; i < ledgerCheckpointMax+37; i++ {
		cps.put(ledgerKey{owner: int64(i), host: "h", session: "s"}, "g", 10, pendingLedger(101))
	}
	if len(cps.m) != ledgerCheckpointMax {
		t.Fatalf("%d checkpoints held, want the bound %d", len(cps.m), ledgerCheckpointMax)
	}
}

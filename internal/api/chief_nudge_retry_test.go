package api

import (
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/chiefnudge"
)

// The refused-check ledger on its own (chief_nudge_retry.go); the sweep's use
// of it is chief_nudge_refused_pg_test.go.

func retrySession(key, streak string) chiefnudge.Session {
	return chiefnudge.Session{Kind: chiefnudge.KindMirror, Key: key, StreakKey: streak}
}

// TestNudgeRetryAnAnswerRestartsTheOwnersClocks: once the owner's chief
// answers a check, its lanes were not full throughout, so every refusal clock
// of that owner's restarts — and only that owner's.
func TestNudgeRetryAnAnswerRestartsTheOwnersClocks(t *testing.T) {
	var r nudgeRetries
	t0 := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	mine, theirs := nudgeRetryKey(7, retrySession("a", "since:1")), nudgeRetryKey(8, retrySession("a", "since:1"))
	r.refused(mine, true, t0)
	r.refused(theirs, true, t0)
	r.answered(7)
	later := t0.Add(31 * time.Minute)
	if since, logNow := r.refused(mine, true, later); !since.Equal(later) || !logNow {
		t.Fatalf("owner 7 after an answer: since %s logNow %v, want %s true (the clock restarted)", since, logNow, later)
	}
	if since, logNow := r.refused(theirs, true, later); !since.Equal(t0) || logNow {
		t.Fatalf("owner 8: since %s logNow %v, want %s false (another owner's answer restarts nothing of its)", since, logNow, t0)
	}
}

// TestNudgeRetryIsBounded: an entry is dropped chiefNudgeRetryTTL after it was
// made, and the ledger never holds more than chiefNudgeRetryMax.
func TestNudgeRetryIsBounded(t *testing.T) {
	var r nudgeRetries
	t0 := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	k := nudgeRetryKey(1, retrySession("a", "since:1"))
	r.keepJev(k, chiefnudge.Result{Label: chiefnudge.LabelNeither}, t0)
	if _, ok := r.cachedJev(k, t0.Add(chiefNudgeRetryTTL-time.Second)); !ok {
		t.Fatal("the kept result is gone before its TTL")
	}
	if _, ok := r.cachedJev(k, t0.Add(chiefNudgeRetryTTL)); ok {
		t.Fatal("the kept result outlived its TTL")
	}
	// The expired entry is dropped from memory by the next write, not just
	// hidden from reads.
	r.keepJev(nudgeRetryKey(1, retrySession("b", "since:1")), chiefnudge.Result{}, t0.Add(chiefNudgeRetryTTL))
	if n := len(r.m); n != 1 {
		t.Fatalf("ledger holds %d after a write past the first entry's TTL, want 1", n)
	}
	for i := 0; i < chiefNudgeRetryMax+50; i++ {
		r.keepJev(nudgeRetryKey(2, retrySession("s"+strconv.Itoa(i), "since:1")), chiefnudge.Result{}, t0.Add(time.Duration(i)*time.Millisecond))
	}
	if n := len(r.m); n > chiefNudgeRetryMax {
		t.Fatalf("ledger holds %d, want at most %d", n, chiefNudgeRetryMax)
	}
	if _, ok := r.cachedJev(nudgeRetryKey(2, retrySession("s0", "since:1")), t0); ok {
		t.Fatal("the oldest entry survived the cap")
	}
}

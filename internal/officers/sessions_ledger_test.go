package officers_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
)

// stubSessions is a SessionStore that answers admissions as told and records
// releases; failing makes Release and Resolve fail.
type stubSessions struct {
	mu       sync.Mutex
	admit    officers.Admission
	released []string
	resolved []string
	failing  bool
}

func (s *stubSessions) AdmitSession(context.Context, string, string, string, int, time.Time) (officers.Admission, error) {
	return s.admit, nil
}
func (s *stubSessions) ReleaseSession(_ context.Context, _, _, item string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing {
		return errors.New("release failed in the stub")
	}
	s.released = append(s.released, item)
	return nil
}
func (s *stubSessions) ResolveSession(_ context.Context, _, _, token, item string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing {
		return errors.New("resolve failed in the stub")
	}
	s.resolved = append(s.resolved, token+"->"+item)
	return nil
}
func (s *stubSessions) Sessions(context.Context, string, string) ([]string, error) { return nil, nil }

func cappedPool(t *testing.T) officers.Officer {
	t.Helper()
	list, err := parseOfficer(t, `,"tiers":{"routine":{"model":"haiku"},"judgement":{"model":"opus","max_sessions_per_day":2}}`)
	if err != nil {
		t.Fatal(err)
	}
	return list[0].Pool(officers.TierJudgement)
}

// TestOnlyTheClaimThatInsertedTheItemMayUndoIt (round 1 Y2): Reserve's undo
// releases the item only when THIS admission inserted it. A claim whose
// admission found the item already counted (a racing claim of the same item)
// has an undo that releases nothing — so the loser of a race can never
// release the winner's session.
func TestOnlyTheClaimThatInsertedTheItemMayUndoIt(t *testing.T) {
	o := cappedPool(t)
	for _, c := range []struct {
		inserted bool
		want     int
	}{{false, 0}, {true, 1}} {
		st := &stubSessions{admit: officers.Admission{Admitted: true, Inserted: c.inserted}}
		l := officers.NewJudgementLedger(st, nil, nil)
		undo, err := l.Reserve(context.Background(), o, "x", t0)
		if err != nil {
			t.Fatal(err)
		}
		undo()
		if len(st.released) != c.want {
			t.Errorf("inserted=%v: undo released %v, want %d release(s)", c.inserted, st.released, c.want)
		}
	}
}

// TestALostClaimNextAnswerKeepsItsSession (round 1 Y2): a claim-next whose
// outcome at HQ is unknown (the answer was lost) keeps its session — the
// pending entry resolves to a lasting `unknown:` entry, never back to
// nothing; a known "nothing claimed" gives it back.
func TestALostClaimNextAnswerKeepsItsSession(t *testing.T) {
	o := cappedPool(t)
	st := &stubSessions{admit: officers.Admission{Admitted: true, Inserted: true}}
	l := officers.NewJudgementLedger(st, nil, nil)
	done, err := l.ReserveNext(context.Background(), o, t0)
	if err != nil {
		t.Fatal(err)
	}
	done("", false)
	if len(st.resolved) != 1 || !strings.Contains(st.resolved[0], "->unknown:") {
		t.Fatalf("unknown outcome resolved as %v, want the token kept as an unknown: entry", st.resolved)
	}
	done, _ = l.ReserveNext(context.Background(), o, t0)
	done("", true)
	if len(st.resolved) != 2 || !strings.HasSuffix(st.resolved[1], "->") {
		t.Fatalf("known nothing-claimed resolved as %v, want the token dropped", st.resolved)
	}
}

// TestLedgerWriteFailuresAreLogged (round 1 Y3): a release or resolve that
// fails is logged, naming the officer — never discarded.
func TestLedgerWriteFailuresAreLogged(t *testing.T) {
	o := cappedPool(t)
	st := &stubSessions{admit: officers.Admission{Admitted: true, Inserted: true}, failing: true}
	var logs []string
	l := officers.NewJudgementLedger(st, nil, func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) })
	undo, _ := l.Reserve(context.Background(), o, "x", t0)
	undo()
	done, _ := l.ReserveNext(context.Background(), o, t0)
	done("7", true)
	if len(logs) != 2 || !strings.Contains(logs[0], o.ID) || !strings.Contains(logs[0], "release failed") || !strings.Contains(logs[1], "resolve failed") {
		t.Fatalf("logs = %q, want the release and the resolve failure, naming the officer", logs)
	}
}

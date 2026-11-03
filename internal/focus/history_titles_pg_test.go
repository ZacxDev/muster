package focus

import (
	"fmt"
	"strings"
	"testing"
)

// THE PALETTE'S PREVIOUS TITLES (history_titles.go) against a real Postgres.
// Every fixture is synthetic.

func prevTitlesOf(ts []TitleAt) string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Title)
	}
	return strings.Join(out, "|")
}

// TestPreviousTitlesAreDistinctNewestFirstAndCapped: a title written twice is
// listed once, at its newest; newest first; at most MaxPreviousTitles; the
// current focus's title is included (the caller drops it). Red at origin/main:
// there was no PreviousTitles.
func TestPreviousTitlesAreDistinctNewestFirstAndCapped(t *testing.T) {
	s, pool := pgStore(t)
	a := AgentSubject(mustAgent(t, pool, 2, "focus-pt-order").ID)
	for _, ti := range []string{"Porting the importer", "Reviewing the PR", "Porting the importer", "Tuning the cache"} {
		write(t, s, 2, a, ti, "d", "")
	}
	got, err := s.PreviousTitles(as(2), Live{Agents: []int64{a.AgentID()}})
	if err != nil {
		t.Fatal(err)
	}
	if g := prevTitlesOf(got[a]); g != "Tuning the cache|Porting the importer|Reviewing the PR" {
		t.Fatalf("titles %q", g)
	}

	b := AgentSubject(mustAgent(t, pool, 2, "focus-pt-cap").ID)
	n := MaxPreviousTitles + 3
	for i := 1; i <= n; i++ {
		write(t, s, 2, b, fmt.Sprintf("Step %02d of the rollout", i), "d", "")
	}
	got, err = s.PreviousTitles(as(2), Live{Agents: []int64{b.AgentID()}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got[b]) != MaxPreviousTitles || got[b][0].Title != fmt.Sprintf("Step %02d of the rollout", n) ||
		got[b][MaxPreviousTitles-1].Title != fmt.Sprintf("Step %02d of the rollout", n-MaxPreviousTitles+1) {
		t.Fatalf("capped titles %q, want the newest %d", prevTitlesOf(got[b]), MaxPreviousTitles)
	}
}

// TestPreviousTitlesReadOnlyTheNewestRows: the read is bounded by a ROW COUNT
// per session (TitleHistoryRows), not by the retention — a title older than a
// session's newest TitleHistoryRows rows is not listed, however few distinct
// titles those rows hold. CONTROL: one row fewer of churn and it is listed.
func TestPreviousTitlesReadOnlyTheNewestRows(t *testing.T) {
	s, pool := pgStore(t)
	for _, tc := range []struct {
		churn  int
		listed bool
	}{{TitleHistoryRows, false}, {TitleHistoryRows - 1, true}} {
		a := AgentSubject(mustAgent(t, pool, 2, fmt.Sprintf("focus-pt-rows-%d", tc.churn)).ID)
		write(t, s, 2, a, "The ancient migration", "d", "")
		for i := 0; i < tc.churn; i++ {
			write(t, s, 2, a, []string{"Ping the canary", "Pong the canary"}[i%2], "d", "")
		}
		got, err := s.PreviousTitles(as(2), Live{Agents: []int64{a.AgentID()}})
		if err != nil {
			t.Fatal(err)
		}
		if listed := strings.Contains(prevTitlesOf(got[a]), "The ancient migration"); listed != tc.listed {
			t.Fatalf("churn %d: titles %q; the ancient title listed=%v, want %v", tc.churn, prevTitlesOf(got[a]), listed, tc.listed)
		}
	}
}

// 🔴 TestPreviousTitlesArePerOwner: another owner reads none of a session's
// titles even naming its id in live, and two owners who each have a mirrored
// session under the SAME key each read only their own titles. CONTROL: the
// owner reads its titles through the same Live. ⚠ The guard is row-level
// security under the caller's principal (the query names no owner); the
// mutation sweep runs the read under the system scope and this goes red.
func TestPreviousTitlesArePerOwner(t *testing.T) {
	s, pool := pgStore(t)
	a := AgentSubject(mustAgent(t, pool, 2, "focus-pt-owner").ID)
	write(t, s, 2, a, "Rotating the owner's secret", "d", "")
	shared := MirrorSubject("palette-shared-session")
	write(t, s, 2, shared, "Owner two's window work", "d", "")
	write(t, s, 3, shared, "Owner three's window work", "d", "")
	live := Live{Agents: []int64{a.AgentID()}, Mirrors: []string{shared.Key}}

	mine, err := s.PreviousTitles(as(2), live)
	if err != nil {
		t.Fatal(err)
	}
	if prevTitlesOf(mine[a]) != "Rotating the owner's secret" || prevTitlesOf(mine[shared]) != "Owner two's window work" {
		t.Fatalf("owner two's titles %q / %q, want exactly its own (the CONTROL that its read works, and isolation: "+
			"owner three's title under the shared key must not appear)", prevTitlesOf(mine[a]), prevTitlesOf(mine[shared]))
	}
	theirs, err := s.PreviousTitles(as(3), live)
	if err != nil {
		t.Fatal(err)
	}
	if len(theirs[a]) != 0 {
		t.Fatalf("owner three read owner two's agent's titles: %q", prevTitlesOf(theirs[a]))
	}
	if prevTitlesOf(theirs[shared]) != "Owner three's window work" {
		t.Fatalf("owner three's shared-key titles %q, want only its own", prevTitlesOf(theirs[shared]))
	}
}

// TestPreviousTitlesOfNothingIsNothing: an empty live set reads no rows.
func TestPreviousTitlesOfNothingIsNothing(t *testing.T) {
	s, _ := pgStore(t)
	got, err := s.PreviousTitles(as(2), Live{})
	if err != nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
}

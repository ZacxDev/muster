package focus

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// THE COMMAND PALETTE'S READS of focus history against a real Postgres:
// RecentTitles with a row bound (the in-page previous titles) and SearchTitles
// (the server search of the whole retention). Every fixture is synthetic.

const paletteSince = DefaultHistoryRetention

func paletteTitlesOf(ts []TitleAt) string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Title)
	}
	return strings.Join(out, "|")
}

func hitTitles(hs []HistoryHit) string {
	var out []string
	for _, h := range hs {
		out = append(out, h.Subject.String()+"="+h.Title)
	}
	return strings.Join(out, "|")
}

// TestRecentTitlesRowBoundReadsOnlyTheNewestRows: with maxRows, a title older
// than a session's newest maxRows rows is not listed, however few distinct
// titles those rows hold; CONTROL: one row fewer of churn and it is; and with
// no row bound (0, sessions_overview's) it is listed too. Red at origin/main:
// RecentTitles took no row bound.
func TestRecentTitlesRowBoundReadsOnlyTheNewestRows(t *testing.T) {
	s, pool := pgStore(t)
	for _, tc := range []struct {
		churn, bound int
		listed       bool
	}{{PaletteTitleRows, PaletteTitleRows, false}, {PaletteTitleRows - 1, PaletteTitleRows, true}, {PaletteTitleRows, 0, true}} {
		a := AgentSubject(mustAgent(t, pool, 2, fmt.Sprintf("focus-rt-rows-%d-%d", tc.churn, tc.bound)).ID)
		write(t, s, 2, a, "The ancient migration", "d", "")
		for i := 0; i < tc.churn; i++ {
			write(t, s, 2, a, []string{"Ping the canary", "Pong the canary"}[i%2], "d", "")
		}
		got, err := s.RecentTitles(as(2), Live{Agents: []int64{a.AgentID()}}, time.Now().Add(-paletteSince), tc.bound)
		if err != nil {
			t.Fatal(err)
		}
		if listed := strings.Contains(paletteTitlesOf(got[a]), "The ancient migration"); listed != tc.listed {
			t.Fatalf("churn %d bound %d: titles %q; the ancient title listed=%v, want %v", tc.churn, tc.bound, paletteTitlesOf(got[a]), listed, tc.listed)
		}
	}
}

// 🔴 TestRecentTitlesArePerOwnerUnderASharedKey: two owners each with a
// mirrored session under the SAME key read only their own titles, and another
// owner reads none of an agent's even naming its id. CONTROL: the owner reads
// its own. ⚠ Two guards (row-level security, and the owner named in the
// query); the mutation sweep removes both together to see this go red.
func TestRecentTitlesArePerOwnerUnderASharedKey(t *testing.T) {
	s, pool := pgStore(t)
	a := AgentSubject(mustAgent(t, pool, 2, "focus-rt-owner").ID)
	write(t, s, 2, a, "Rotating the owner secret", "d", "")
	shared := MirrorSubject("palette-shared-rt")
	write(t, s, 2, shared, "Owner two window work", "d", "")
	write(t, s, 3, shared, "Owner three window work", "d", "")
	live := Live{Agents: []int64{a.AgentID()}, Mirrors: []string{shared.Key}}
	since := time.Now().Add(-paletteSince)
	for _, bound := range []int{0, PaletteTitleRows} {
		mine, err := s.RecentTitles(as(2), live, since, bound)
		if err != nil {
			t.Fatal(err)
		}
		if paletteTitlesOf(mine[a]) != "Rotating the owner secret" || paletteTitlesOf(mine[shared]) != "Owner two window work" {
			t.Fatalf("bound %d: owner two's titles %q / %q, want exactly its own", bound, paletteTitlesOf(mine[a]), paletteTitlesOf(mine[shared]))
		}
		theirs, err := s.RecentTitles(as(3), live, since, bound)
		if err != nil {
			t.Fatal(err)
		}
		if len(theirs[a]) != 0 || paletteTitlesOf(theirs[shared]) != "Owner three window work" {
			t.Fatalf("bound %d: owner three read %q / %q, want nothing of the agent and only its own window title", bound, paletteTitlesOf(theirs[a]), paletteTitlesOf(theirs[shared]))
		}
	}
}

// TestSearchTitlesFindsAnyTitleInTheWindowBySubstring: a fragment of a title
// older than the in-page bound is found (RecentTitles with the palette's row
// bound does not list it — the CONTROL that it is out of the page); one hit a
// session, its NEWEST matching title; the title only, not the description;
// case-folded; not outside the window; not a session outside live; nothing
// for a query under MinTitleQueryRunes. Red at origin/main: no SearchTitles.
func TestSearchTitlesFindsAnyTitleInTheWindowBySubstring(t *testing.T) {
	s, pool := pgStore(t)
	a := AgentSubject(mustAgent(t, pool, 2, "focus-st-a").ID)
	b := AgentSubject(mustAgent(t, pool, 2, "focus-st-b").ID)
	dead := AgentSubject(mustAgent(t, pool, 2, "focus-st-dead").ID)
	write(t, s, 2, a, "Census of the quokka colony", "d", "")
	write(t, s, 2, a, "Quokka census, second pass", "d", "")
	for i := 0; i < PaletteTitleRows; i++ {
		write(t, s, 2, a, fmt.Sprintf("Sorting batch %02d", i%20), "the quokka is not in this title", "")
	}
	write(t, s, 2, b, "Tidying the quokka notes", "d", "")
	backdate(t, b, "Tidying the quokka notes", paletteSince+time.Hour)
	write(t, s, 2, dead, "Quokka in a dead session", "d", "")
	live := Live{Agents: []int64{a.AgentID(), b.AgentID()}}
	since := time.Now().Add(-paletteSince)

	recent, err := s.RecentTitles(as(2), live, since, PaletteTitleRows)
	if err != nil || strings.Contains(strings.ToLower(paletteTitlesOf(recent[a])), "quokka") {
		t.Fatalf("CONTROL: the in-page read lists %q (%v); the quokka titles must be beyond its bound", paletteTitlesOf(recent[a]), err)
	}
	hits, err := s.SearchTitles(as(2), "QUOKKA c", live, since, 5)
	if err != nil {
		t.Fatal(err)
	}
	if g := hitTitles(hits); g != a.String()+"=Quokka census, second pass" {
		t.Fatalf("hits %q, want only A's NEWEST matching title — both of A's quokka titles contain the query — (B's is outside the window, the dead session is not live, descriptions do not count)", g)
	}
	if hits, err := s.SearchTitles(as(2), "quokka is not", live, since, 5); err != nil || len(hits) != 0 {
		t.Fatalf("a query only a DESCRIPTION holds: %q %v; want nothing", hitTitles(hits), err)
	}
	if hits, err := s.SearchTitles(as(2), "  qu ", live, since, 5); err != nil || len(hits) != 0 {
		t.Fatalf("a %d-rune query: %q %v; want nothing", 2, hitTitles(hits), err)
	}
	if hits, err := s.SearchTitles(as(2), "quo", live, since, 5); err != nil || len(hits) != 1 {
		t.Fatalf("CONTROL: a %d-rune query: %q %v; want A", MinTitleQueryRunes, hitTitles(hits), err)
	}
	if _, err := s.SearchTitles(as(2), "quokka", live, time.Time{}, 5); err == nil {
		t.Error("a search with no window")
	}
}

// 🔴 TestSearchTitlesIsPerOwner: another owner finds nothing of an agent's
// titles naming its id, and under a shared mirror key only its own title.
// CONTROL: the owner finds its own through the same Live.
func TestSearchTitlesIsPerOwner(t *testing.T) {
	s, pool := pgStore(t)
	a := AgentSubject(mustAgent(t, pool, 2, "focus-st-owner").ID)
	write(t, s, 2, a, "Wombat audit of owner two", "d", "")
	shared := MirrorSubject("palette-shared-st")
	write(t, s, 2, shared, "Wombat window of owner two", "d", "")
	write(t, s, 3, shared, "Wombat window of owner three", "d", "")
	live := Live{Agents: []int64{a.AgentID()}, Mirrors: []string{shared.Key}}
	since := time.Now().Add(-paletteSince)
	mine, err := s.SearchTitles(as(2), "wombat", live, since, 5)
	if err != nil {
		t.Fatal(err)
	}
	if g := hitTitles(mine); !strings.Contains(g, "Wombat audit of owner two") || !strings.Contains(g, "Wombat window of owner two") || strings.Contains(g, "three") {
		t.Fatalf("owner two's hits %q, want exactly its own two", g)
	}
	theirs, err := s.SearchTitles(as(3), "wombat", live, since, 5)
	if err != nil {
		t.Fatal(err)
	}
	if g := hitTitles(theirs); g != shared.String()+"=Wombat window of owner three" {
		t.Fatalf("owner three's hits %q, want only its own window title", g)
	}
}

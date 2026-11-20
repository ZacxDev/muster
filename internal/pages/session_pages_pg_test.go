package pages

import (
	"slices"
	"testing"
)

// TestSessionPagesAreTheSessionsOwnNewestMadeFirst: SessionPages lists the
// pages ONE session created — a mirrored session by its host AND one of its
// session keys, an agent by its id — the caller's own (row-level security),
// newest MADE first: not by last activity (a new version of an old page does
// not lift it), not another session's on the same host, not the same session
// id on another host, not a page made before 0152 recorded its session, not
// another owner's from the same host and session.
func TestSessionPagesAreTheSessionsOwnNewestMadeFirst(t *testing.T) {
	s, pool := pgStore(t)
	two, three := asUser(2), asUser(3)
	b, n := blocksOf(t, qBlocks)
	mk := func(user int, c Creator, title string, age int) int64 {
		t.Helper()
		ctx := asUser(int64(user))
		p, _, err := s.Create(ctx, c, title, b, n)
		if err != nil {
			t.Fatal(err)
		}
		// When it was made, pinned: the order under test is by this.
		tag, err := pool.Exec(ctx, `UPDATE pages SET created_at = now() - make_interval(secs => $2), updated_at = now() - make_interval(secs => $2) WHERE id = $1`, p.ID, float64(age))
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("pin page %d's time: %v (%d rows)", p.ID, err, tag.RowsAffected())
		}
		return p.ID
	}
	inA := Creator{MirrorHost: "workstation", MirrorSession: "s-a", Label: "a"}
	inB := Creator{MirrorHost: "workstation", MirrorSession: "s-b", Label: "b"}
	oldest := mk(2, inA, "A oldest", 300)
	middle := mk(2, inA, "A middle", 200)
	newest := mk(2, inA, "A newest", 100)
	bPage := mk(2, inB, "B", 150)
	mk(2, Creator{MirrorHost: "workstation", Label: "pre-0152"}, "no session recorded", 50)
	mk(2, Creator{MirrorHost: "laptop", MirrorSession: "s-a", Label: "other host"}, "other host", 40)
	theirs := mk(3, inA, "another owner's", 30)
	a1 := mk(2, agentA, "agent first", 90)
	a2 := mk(2, agentA, "agent second", 80)
	mk(2, agentB, "the other agent's", 70)
	// A new version of the OLDEST page: it is now the most recently active.
	if _, _, err := s.AddVersion(two, inA, oldest, "A oldest v2", b, n); err != nil {
		t.Fatal(err)
	}
	if l, err := s.CreateLink(two, middle, LinkLive, "you", false); err != nil || l.State != LinkLive {
		t.Fatalf("link: %v", err)
	}

	ids := func(list []Summary) []int64 {
		out := make([]int64, 0, len(list))
		for _, x := range list {
			out = append(out, x.ID)
		}
		return out
	}
	got, err := s.SessionPages(two, SessionRef{MirrorHost: "workstation", MirrorSessions: []string{"s-a"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{newest, middle, oldest}; !slices.Equal(ids(got), want) {
		t.Fatalf("session s-a's pages = %v, want %v (newest made first, its own only)", ids(got), want)
	}
	// Each with its current version and its live links.
	if got[2].CurrentVersion != 2 || got[1].LiveLinks != 1 || got[0].LiveLinks != 0 {
		t.Errorf("versions/links: %+v", got)
	}
	// A window running two sessions (a split pane) lists both's.
	got, err = s.SessionPages(two, SessionRef{MirrorHost: "workstation", MirrorSessions: []string{"s-b", "s-a"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{newest, bPage, middle, oldest}; !slices.Equal(ids(got), want) {
		t.Errorf("sessions s-a and s-b's pages = %v, want %v", ids(got), want)
	}
	// The agent's.
	got, err = s.SessionPages(two, SessionRef{AgentID: agentA.AgentID})
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{a2, a1}; !slices.Equal(ids(got), want) {
		t.Errorf("agent A's pages = %v, want %v", ids(got), want)
	}
	// The other owner sees only theirs, from the same host and session.
	got, err = s.SessionPages(three, SessionRef{MirrorHost: "workstation", MirrorSessions: []string{"s-a"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{theirs}; !slices.Equal(ids(got), want) {
		t.Errorf("user 3's s-a pages = %v, want %v", ids(got), want)
	}
	// A ref naming no session reads nothing.
	for i, x := range []SessionRef{{}, {MirrorHost: "workstation"}, {MirrorHost: "workstation", MirrorSessions: []string{""}}, {AgentID: 501, MirrorHost: "workstation"}} {
		got, err := s.SessionPages(two, x)
		if err != nil || got != nil {
			t.Errorf("ref %d %+v = %v, %v; want nothing", i, x, ids(got), err)
		}
	}
}

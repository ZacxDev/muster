package pages

import (
	"slices"
	"testing"
)

// TestAWindowsPagesSurviveANewSessionInIt: a mirrored window lists the pages
// made IN it — its host, tmux session and window id, recorded at create
// (0161) — whatever Claude session made them, so a /clear (session A's page,
// the window now running session B) keeps them. A page made in another
// window of the same host, the same window name on another host, the same
// window id in another tmux session, and another owner's page from the same
// host and window are not listed; a page made before the window was recorded
// is listed only while its session runs here.
func TestAWindowsPagesSurviveANewSessionInIt(t *testing.T) {
	s, _ := pgStore(t)
	two, three := asUser(2), asUser(3)
	b, n := blocksOf(t, qBlocks)
	create := func(user int64, c Creator, title string) int64 {
		t.Helper()
		p, _, err := s.Create(asUser(user), c, title, b, n)
		if err != nil {
			t.Fatal(err)
		}
		return p.ID
	}
	inWin := func(host, sess, tmuxSess, win string) Creator {
		return Creator{MirrorHost: host, MirrorSession: sess, MirrorTmuxSession: tmuxSess, MirrorWindow: win, Label: "x"}
	}
	underA := create(2, inWin("workstation", "s-a", "main", "@3"), "made under session A in @3")
	create(2, inWin("workstation", "s-c", "main", "@4"), "another window of the host")
	create(2, inWin("laptop", "s-d", "main", "@3"), "the same window name on another host")
	create(3, inWin("workstation", "s-e", "main", "@3"), "another owner's, same host and window")
	create(2, inWin("workstation", "s-f", "side", "@3"), "the same window id in another tmux session")
	old := create(2, Creator{MirrorHost: "workstation", MirrorSession: "s-b", Label: "x"}, "no window recorded, made by s-b")

	ids := func(list []Summary) []int64 {
		out := make([]int64, 0, len(list))
		for _, x := range list {
			out = append(out, x.ID)
		}
		return out
	}
	// The window @3 of tmux session main now runs session B (a /clear).
	got, err := s.SessionPages(two, SessionRef{MirrorHost: "workstation", MirrorTmuxSession: "main", MirrorWindow: "@3", MirrorSessions: []string{"s-b"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{old, underA}; !slices.Equal(ids(got), want) {
		t.Fatalf("window main:@3 now running s-b lists %v, want %v (its own page made under s-a, and s-b's)", ids(got), want)
	}
	if c := got[1].Creator; c.MirrorTmuxSession != "main" || c.MirrorWindow != "@3" {
		t.Errorf("the page's recorded window: %+v", c)
	}
	// The window alone (no session of a page runs there): its own page only.
	got, err = s.SessionPages(two, SessionRef{MirrorHost: "workstation", MirrorTmuxSession: "main", MirrorWindow: "@3"})
	if want := []int64{underA}; err != nil || !slices.Equal(ids(got), want) {
		t.Errorf("window main:@3 alone lists %v, %v; want %v", ids(got), err, want)
	}
	// Another window of the host: not @3's.
	got, err = s.SessionPages(two, SessionRef{MirrorHost: "workstation", MirrorTmuxSession: "main", MirrorWindow: "@4"})
	if err != nil || len(got) != 1 || got[0].Title != "another window of the host" {
		t.Errorf("window main:@4 lists %+v, %v", got, err)
	}
	// Another owner sees only theirs in the same window.
	got, err = s.SessionPages(three, SessionRef{MirrorHost: "workstation", MirrorTmuxSession: "main", MirrorWindow: "@3"})
	if err != nil || len(got) != 1 || got[0].Title != "another owner's, same host and window" {
		t.Errorf("user 3's main:@3 lists %+v, %v", got, err)
	}
	// Half a window records nothing: both or neither.
	p, _, err := s.Create(two, Creator{MirrorHost: "workstation", MirrorSession: "s-g", MirrorWindow: "@9", Label: "x"}, "half", b, n)
	if err != nil || p.Creator.MirrorWindow != "" || p.Creator.MirrorTmuxSession != "" {
		t.Errorf("a creator with a window id but no tmux session recorded %+v, %v", p.Creator, err)
	}
}

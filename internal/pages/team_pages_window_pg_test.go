package pages

import (
	"testing"
)

// TestAWindowsPagesAreItsOwnersEvenOnATeamsSameNamedHost: a person's mirrored
// window lists the pages made IN it (SessionPages) — and not a page a TEAM
// they are on made from a host of the same name, in a window and session of
// the same names, though since 0165 they may read that team's page. A host's
// name, a tmux window id and a session key are the owner's own; another
// owner's same strings name another machine. CONTROL: the team's page is
// readable by them (so the zero is about the window list, not about RLS), and
// the team itself lists it for the same window.
func TestAWindowsPagesAreItsOwnersEvenOnATeamsSameNamedHost(t *testing.T) {
	f := newTeamFixture(t)
	b, n := blocksOf(t, qBlocks)
	win := Creator{MirrorHost: "rig", MirrorSession: "sess-a", MirrorTmuxSession: "work", MirrorWindow: "@1", Label: "a session on rig"}
	ref := SessionRef{MirrorHost: "rig", MirrorTmuxSession: "work", MirrorWindow: "@1", MirrorSessions: []string{"sess-a"}}
	me := f.person(t, f.creator)
	own, _, err := f.s.Create(me, win, "Mine", b, n)
	if err != nil {
		t.Fatal(err)
	}
	team, _, err := f.s.Create(asTeam(f.teamA), win, "The team's", b, n)
	if err != nil {
		t.Fatal(err)
	}
	if team.OwnerID != f.teamA {
		t.Fatalf("fixture: the team's page is owned by %d, want %d", team.OwnerID, f.teamA)
	}
	if _, err := f.s.Page(me, team.ID); err != nil {
		t.Fatalf("control: a member of team A reads its page: %v", err)
	}
	list, err := f.s.SessionPages(me, ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != own.ID {
		ids := []int64{}
		for _, x := range list {
			ids = append(ids, x.ID)
		}
		t.Errorf("the window's pages for its owner: %v; want only their own page %d (not the team's %d)", ids, own.ID, team.ID)
	}
	// CONTROL: the team lists its own page for the same-named window.
	if tl, err := f.s.SessionPages(asTeam(f.teamA), ref); err != nil || len(tl) != 1 || tl[0].ID != team.ID {
		t.Fatalf("control: the team's window list: %+v %v; want its page %d", tl, err, team.ID)
	}
}

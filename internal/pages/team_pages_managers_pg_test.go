package pages

import (
	"context"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
)

// A team's MANAGERS step in on its pages (operator decision Q1, 0165): when
// the page's creator has LEFT the team, any manager manages the page; and any
// manager may ALWAYS revoke a link, even while the creator is a member, so a
// mistaken Share is undone at once. Held by the database: every statement
// below runs as the application role under forced row-level security.

// TestAManagerStepsInWhenTheCreatorLeaves: once the page's creator is removed
// from the team, a MANAGER makes version 1 current, revokes its link, and
// closes its links (a delete's first step); a VIEWER still does none of it.
// CONTROL: before the creator left, the same manager was refused.
func TestAManagerStepsInWhenTheCreatorLeaves(t *testing.T) {
	f := newTeamFixture(t)
	p, pending := f.createdTeamPage(t, f.creator)
	if err := f.store.SetTeamMember(context.Background(), usersActor(), f.teamA, f.outsider, authz.TeamManager); err != nil {
		t.Fatal(err)
	}
	manager := func() context.Context { return f.person(t, f.outsider) }
	if _, err := f.s.SetCurrent(manager(), p.ID, 1); !refused(err) {
		t.Fatalf("control: a manager changing the version while the creator is a member: %v; want the database's refusal", err)
	}
	if err := f.store.RemoveTeamMember(context.Background(), usersActor(), f.teamA, f.creator); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetCurrent(f.person(t, f.viewerA), p.ID, 1); !refused(err) {
		t.Errorf("a viewer changing the version after the creator left: %v; want the database's refusal", err)
	}
	if _, err := f.s.CloseLinks(f.person(t, f.viewerA), p.ID); !refused(err) {
		t.Errorf("a viewer closing the links after the creator left: %v; want the database's refusal", err)
	}
	if got, err := f.s.SetCurrent(manager(), p.ID, 1); err != nil || got.CurrentVersion != 1 {
		t.Fatalf("a manager changing the version after the creator left: %+v %v", got, err)
	}
	if l, err := f.s.RevokeLink(manager(), p.ID, pending.ID); err != nil || l.State != LinkRevoked {
		t.Fatalf("a manager revoking a link after the creator left: %+v %v", l, err)
	}
	if _, err := f.s.CloseLinks(manager(), p.ID); err != nil {
		t.Fatalf("a manager closing the links after the creator left: %v", err)
	}
	if _, err := f.s.Delete(manager(), p.ID); err != nil {
		t.Fatalf("a manager deleting after the creator left: %v", err)
	}
	if _, err := f.s.Page(asTeam(f.teamA), p.ID); err == nil {
		t.Fatal("a manager's delete after the creator left left the page")
	}
}

// TestAManagerRevokesAViewersShareWhileTheCreatorIsPresent: a VIEWER's Share
// makes the page's link live; a MANAGER of the team revokes it while the
// creator is still a member. An operator (not the creator, not a manager)
// still cannot, and the manager still cannot change the version.
func TestAManagerRevokesAViewersShareWhileTheCreatorIsPresent(t *testing.T) {
	f := newTeamFixture(t)
	s := keyedOn(t, f.s)
	p, _ := f.createdTeamPage(t, f.creator)
	if err := f.store.SetTeamMember(context.Background(), usersActor(), f.teamA, f.outsider, authz.TeamManager); err != nil {
		t.Fatal(err)
	}
	l, created, err := s.ShareLink(f.person(t, f.viewerA), p.ID, "Vic Viewer", false)
	if err != nil || !created || l.State != LinkLive {
		t.Fatalf("a viewer's Share: %+v created=%v %v", l, created, err)
	}
	if _, err := s.RevokeLink(f.person(t, f.both), p.ID, l.ID); !refused(err) {
		t.Errorf("an operator (not the creator) revoking the viewer's link: %v; want the database's refusal", err)
	}
	if _, err := s.SetCurrent(f.person(t, f.outsider), p.ID, 1); !refused(err) {
		t.Errorf("a manager changing the version while the creator is a member: %v; want the database's refusal", err)
	}
	got, err := s.RevokeShareLink(f.person(t, f.outsider), p.ID)
	if err != nil || got.ID != l.ID || got.State != LinkRevoked {
		t.Fatalf("a manager revoking the viewer's live link while the creator is a member: %+v %v", got, err)
	}
	if again, err := s.Link(asTeam(f.teamA), p.ID, l.ID); err != nil || again.State != LinkRevoked {
		t.Fatalf("the link after the manager's revoke: %+v %v; want revoked", again, err)
	}
}

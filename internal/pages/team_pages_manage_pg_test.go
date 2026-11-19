package pages

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/users"
)

// What a member may DO to a team's page (migration 0160), as the database
// holds it: every statement below runs as the application role under forced
// row-level security, so an application check is not what refuses it.

// keyedOn gives s the test link key (reusable links).
func keyedOn(t *testing.T, s *PGStore) *PGStore {
	t.Helper()
	if _, err := s.WithLinkKey(testLinkKey); err != nil {
		t.Fatal(err)
	}
	return s
}

// usersActor is the admin who arranges the fixture's teams.
func usersActor() users.Actor { return users.Actor{UserID: 1, Via: authz.ViaSession} }

// refused reports whether err is the database's refusal (42501).
func refused(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "42501"
}

// createdTeamPage is team A's page recording f.creator as its creator (the
// person whose say-so started its agent), with a pending agent link.
func (f *teamFixture) createdTeamPage(t *testing.T, byUser int64) (Page, Link) {
	t.Helper()
	ctx := asTeam(f.teamA)
	b, n := blocksOf(t, qBlocks)
	c := Creator{AgentID: 7001, Label: "team-worker", ByUser: byUser}
	p, _, err := f.s.Create(ctx, c, "Alpha managed", b, n)
	if err != nil {
		t.Fatal(err)
	}
	if p.Creator.ByUser != byUser {
		t.Fatalf("fixture: the page records creator %d, want %d", p.Creator.ByUser, byUser)
	}
	if _, _, err := f.s.AddVersion(ctx, c, p.ID, "Alpha managed v2", b, n); err != nil {
		t.Fatal(err)
	}
	pending, err := f.s.CreateLink(ctx, p.ID, LinkPending, "team-worker", true)
	if err != nil {
		t.Fatal(err)
	}
	return p, pending
}

// TestOnlyTheCreatorManagesATeamsPage: a member who is not the page's creator
// — a viewer, and an operator who is in both teams — is refused by the
// DATABASE every managing write: making version 1 current, closing its links
// (a delete's first step), revoking a link, and the new link (rotate). A
// delete reaches no row. CONTROL: the creator does every one of them.
func TestOnlyTheCreatorManagesATeamsPage(t *testing.T) {
	f := newTeamFixture(t)
	s := keyedOn(t, f.s)
	p, pending := f.createdTeamPage(t, f.creator)
	// The page's one link exists, so a new link REPLACES it (on a page with
	// none, "new link" makes the first one — what any member's Share does).
	if _, _, err := s.ShareLink(asTeam(f.teamA), p.ID, "team-worker", true); err != nil {
		t.Fatal(err)
	}
	for name, uid := range map[string]int64{"a viewer": f.viewerA, "an operator (not the creator)": f.both} {
		ctx := f.person(t, uid)
		if _, err := s.SetCurrent(ctx, p.ID, 1); !refused(err) {
			t.Errorf("%s making version 1 current: %v; want the database's refusal", name, err)
		}
		if _, err := s.CloseLinks(ctx, p.ID); !refused(err) {
			t.Errorf("%s closing the page's links: %v; want the database's refusal", name, err)
		}
		if _, err := s.RevokeLink(ctx, p.ID, pending.ID); !refused(err) {
			t.Errorf("%s revoking a link: %v; want the database's refusal", name, err)
		}
		if _, _, err := s.RotateShareLink(ctx, p.ID, "x"); !refused(err) {
			t.Errorf("%s making a new link: %v; want the database's refusal", name, err)
		}
		if _, err := s.Delete(ctx, p.ID); err != nil {
			t.Errorf("%s deleting: %v", name, err)
		}
		if got, err := s.Page(asTeam(f.teamA), p.ID); err != nil || got.CurrentVersion != 2 {
			t.Fatalf("after %s's attempts the page is %+v, %v; want it whole, at version 2", name, got, err)
		}
	}
	// CONTROL: the creator.
	ctx := f.person(t, f.creator)
	if got, err := s.SetCurrent(ctx, p.ID, 1); err != nil || got.CurrentVersion != 1 {
		t.Fatalf("the creator making version 1 current: %+v %v", got, err)
	}
	if l, err := s.RevokeLink(ctx, p.ID, pending.ID); err != nil || l.State != LinkRevoked {
		t.Fatalf("the creator revoking a link: %+v %v", l, err)
	}
	if _, _, err := s.RotateShareLink(ctx, p.ID, "Cora Creator"); err != nil {
		t.Fatalf("the creator making a new link: %v", err)
	}
	if _, err := s.CloseLinks(ctx, p.ID); err != nil {
		t.Fatalf("the creator closing the links: %v", err)
	}
	if _, err := s.Delete(ctx, p.ID); err != nil {
		t.Fatalf("the creator deleting: %v", err)
	}
	if _, err := s.Page(asTeam(f.teamA), p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after the creator's delete the page reads %v; want it gone", err)
	}
}

// TestATeamPageWithNoCreatorIsAManagersToManage: a team page that records no
// creator (its agent's starter unknown) is managed by a MANAGER of the team,
// and by no operator. Without this nobody could ever delete it.
func TestATeamPageWithNoCreatorIsAManagersToManage(t *testing.T) {
	f := newTeamFixture(t)
	p, _ := f.createdTeamPage(t, 0)
	if _, err := f.s.SetCurrent(f.person(t, f.both), p.ID, 1); !refused(err) {
		t.Errorf("an operator making version 1 current on a creatorless page: %v; want the database's refusal", err)
	}
	// f.creator is team A's first manager (CreateTeam) made operator after;
	// make the outsider A's manager for this case.
	if err := f.store.SetTeamMember(context.Background(), usersActor(), f.teamA, f.outsider, authz.TeamManager); err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.SetCurrent(f.person(t, f.outsider), p.ID, 1); err != nil || got.CurrentVersion != 1 {
		t.Fatalf("a manager making version 1 current on a creatorless page: %+v %v", got, err)
	}
}

// TestTheCreatorColumnNeverChangesAndIsATeamsOnly: created_by_user is
// written once — the creator themselves cannot rewrite it — and a person's
// own page records none, whatever the statement wrote.
func TestTheCreatorColumnNeverChangesAndIsATeamsOnly(t *testing.T) {
	f := newTeamFixture(t)
	p, _ := f.createdTeamPage(t, f.creator)
	_, err := f.s.pool.Exec(f.person(t, f.creator), `UPDATE pages SET created_by_user = $2 WHERE id = $1`, p.ID, f.viewerA)
	if !refused(err) {
		t.Errorf("the creator rewriting created_by_user: %v; want the database's refusal", err)
	}
	_, err = f.s.pool.Exec(asTeam(f.teamA), `UPDATE pages SET created_by_user = $2 WHERE id = $1`, p.ID, f.viewerA)
	if !refused(err) {
		t.Errorf("the team's agent rewriting created_by_user: %v; want the database's refusal", err)
	}
	b, n := blocksOf(t, qBlocks)
	own, _, err := f.s.Create(f.person(t, f.both), Creator{AgentID: 7002, Label: "mine", ByUser: f.creator}, "Bo's", b, n)
	if err != nil {
		t.Fatal(err)
	}
	if own.Creator.ByUser != 0 {
		t.Errorf("a person's own page records creator %d; want none (its owner is its creator)", own.Creator.ByUser)
	}
}

// TestAnyMemberSharesATeamsPage: a VIEWER's Share makes the page's one link
// live (created), and every other member's Share reveals the SAME link. A
// member makes no other kind of link: a pending one (an agent's ask) is
// refused by the database.
func TestAnyMemberSharesATeamsPage(t *testing.T) {
	f := newTeamFixture(t)
	s := keyedOn(t, f.s)
	p, _ := f.createdTeamPage(t, f.creator)
	first, created, err := s.ShareLink(f.person(t, f.viewerA), p.ID, "Vic Viewer", false)
	if err != nil || !created || first.State != LinkLive || first.OwnerID != f.teamA || !TokenShapeOK(first.Token) {
		t.Fatalf("a viewer's Share: %+v created=%v %v", first, created, err)
	}
	again, created, err := s.ShareLink(f.person(t, f.both), p.ID, "Bo Both", false)
	if err != nil || created || again.Token != first.Token {
		t.Fatalf("another member's Share: %+v created=%v %v; want the same link", again, created, err)
	}
	if _, err := s.CreateLink(f.person(t, f.both), p.ID, LinkPending, "Bo Both", true); !refused(err) {
		t.Errorf("a member filing a pending agent link: %v; want the database's refusal", err)
	}
	if _, _, err := s.ShareLink(f.person(t, f.onlyB), p.ID, "Bea", false); !errors.Is(err, ErrNotFound) {
		t.Errorf("a member of team B only sharing team A's page: %v; want ErrNotFound", err)
	}
}

// TestAMemberDecidesTheTeamAgentsShareCard: a member answering the team
// agent's share card moves its pending link to live — and that is all a
// non-creator does to a link: revoking it after is refused.
func TestAMemberDecidesTheTeamAgentsShareCard(t *testing.T) {
	f := newTeamFixture(t)
	p, pending := f.createdTeamPage(t, f.creator)
	ctx := f.person(t, f.both)
	if l, err := f.s.DecideLink(ctx, pending.ID, true); err != nil || l.State != LinkLive {
		t.Fatalf("a member approving the share card: %+v %v", l, err)
	}
	if _, err := f.s.RevokeLink(ctx, p.ID, pending.ID); !refused(err) {
		t.Errorf("a non-creator revoking the live link: %v; want the database's refusal", err)
	}
}

// TestAMembersResponseIsRecordedAsTheirs: a member's response from the owner
// view is recorded, by_owner, with responder_user = that member — forced by
// the database: a statement naming someone else is recorded as its caller.
// Another member reads it with its sender. A member of B only records none.
// A link's response names nobody.
func TestAMembersResponseIsRecordedAsTheirs(t *testing.T) {
	f := newTeamFixture(t)
	p, _ := f.createdTeamPage(t, f.creator)
	r, err := f.s.AddOwnerResponse(f.person(t, f.viewerA), p.ID, 2, "q1", KindSubmit, []byte(`{"answer":"June"}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if !r.ByOwner || r.ResponderUser != f.viewerA {
		t.Fatalf("a viewer's response: %+v; want by_owner, responder %d", r, f.viewerA)
	}
	got, err := f.s.Responses(f.person(t, f.both), p.ID, 0, 10)
	if err != nil || len(got) != 1 || got[0].ResponderUser != f.viewerA {
		t.Fatalf("another member reads %+v %v; want the viewer's response with its sender", got, err)
	}
	var forged int64
	if err := f.s.pool.QueryRow(f.person(t, f.both), `INSERT INTO page_responses (owner_id, page_id, by_owner, version, component, kind, responder_user)
		VALUES ($1, $2, true, 2, 'q1', 'submit', $3) RETURNING responder_user`, f.teamA, p.ID, f.viewerA).Scan(&forged); err != nil {
		t.Fatal(err)
	}
	if forged != f.both {
		t.Errorf("a response written naming %d is recorded as %d; want its caller %d", f.viewerA, forged, f.both)
	}
	if _, err := f.s.AddOwnerResponse(f.person(t, f.onlyB), p.ID, 2, "q1", KindSubmit, []byte(`{"answer":"x"}`), ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("a member of team B only responding: %v; want ErrNotFound", err)
	}
	// A member never records a LINK's response (by_owner false) as the team.
	_, err = f.s.pool.Exec(f.person(t, f.both), `INSERT INTO page_responses (owner_id, page_id, link_id, by_owner, version, component, kind)
		SELECT $1, $2, id, false, 2, 'q1', 'submit' FROM page_links WHERE page_id = $2 LIMIT 1`, f.teamA, p.ID)
	if !refused(err) {
		t.Errorf("a member writing a link's response on the team's page: %v; want the database's refusal", err)
	}
}

// TestMayManageMatchesTheDatabase: pages.MayManage (what the screen draws and
// the routes refuse) answers as the database's muster_page_manager for every
// reader in the fixture, on a page with a creator and one without.
func TestMayManageMatchesTheDatabase(t *testing.T) {
	f := newTeamFixture(t)
	withCreator, _ := f.createdTeamPage(t, f.creator)
	without, _ := f.createdTeamPage(t, 0)
	readers := map[string]context.Context{"team agent": asTeam(f.teamA), "team B agent": asTeam(f.teamB), "system": authz.System(context.Background())}
	for name, uid := range map[string]int64{"creator": f.creator, "viewer": f.viewerA, "both": f.both, "onlyB": f.onlyB, "outsider": f.outsider, "manager": 0} {
		if uid == 0 {
			continue
		}
		readers[name] = f.person(t, uid)
	}
	seenTrue, seenFalse := false, false
	for name, ctx := range readers {
		p, _ := authz.From(ctx)
		for _, pg := range []Page{withCreator, without} {
			var db bool
			if err := f.s.pool.QueryRow(ctx, `SELECT muster_page_manager($1, nullif($2, 0))`, pg.OwnerID, pg.Creator.ByUser).Scan(&db); err != nil {
				t.Fatal(err)
			}
			if got := MayManage(pg, p); got != db {
				t.Errorf("%s on a page with creator %d: MayManage %v, the database %v", name, pg.Creator.ByUser, got, db)
			}
			seenTrue, seenFalse = seenTrue || db, seenFalse || !db
		}
	}
	if !seenTrue || !seenFalse {
		t.Fatalf("instrument: the readers never produced both answers (true %v, false %v)", seenTrue, seenFalse)
	}
}

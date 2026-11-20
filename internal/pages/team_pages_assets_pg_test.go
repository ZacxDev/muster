package pages

import (
	"context"
	"errors"
	"testing"
)

// A TEAM'S PAGE'S AUTHOR ASSETS (0164's page_assets and page_version_assets)
// follow the page (0165): every member who reads the team's page reads the
// assets its versions use — or its images and video would be drawn broken in
// their view — and nobody else does; no member writes or deletes one.

// teamAssetPage is team's page (teamPage) with an asset its third version
// uses, added as the page's own creator (the team's agent).
func (f *teamFixture) teamAssetPage(t *testing.T, team int64) (Page, Asset) {
	t.Helper()
	p := f.teamPage(t, team, "Media")
	ctx := asTeam(team)
	c := Creator{AgentID: 900 + team}
	a, err := f.s.AddAsset(ctx, c, p.ID, Asset{PublicID: assetID(int(team) * 100), Filename: "chart.png", ContentType: "image/png", Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	b, n := mdBlocks(t, "![chart]("+a.Ref()+")")
	if _, v, err := f.s.AddVersion(ctx, c, p.ID, "Media v3", b, n); err != nil || v.Version != 3 {
		t.Fatalf("the version using the asset: %+v %v", v, err)
	}
	return p, a
}

// assetReach is what ctx reads of page id's asset: through version 3, through
// the current version (0), and in the page's list.
func (f *teamFixture) assetReach(t *testing.T, ctx context.Context, id int64, a Asset) (v3, current bool, listed int) {
	t.Helper()
	vs, err := f.s.VersionAssets(ctx, id, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) == 1 && vs[0].PublicID == a.PublicID {
		v3 = true
	}
	switch _, err := f.s.VersionAsset(ctx, id, 0, a.PublicID); {
	case err == nil:
		current = true
	case !errors.Is(err, ErrNotFound):
		t.Fatal(err)
	}
	list, err := f.s.Assets(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return v3, current, len(list)
}

// TestATeamsPageAssetsAreReadByItsMembersAndNobodyElse: the asset team A's
// page uses is read — through its version, through the current version, and
// listed — by every member of A (the creator, a viewer, a member of both
// teams), and by nobody else: not a member of B only, not a member of no team,
// not team B's agent. CONTROL: the team itself reads it.
func TestATeamsPageAssetsAreReadByItsMembersAndNobodyElse(t *testing.T) {
	f := newTeamFixture(t)
	p, a := f.teamAssetPage(t, f.teamA)
	if v3, cur, n := f.assetReach(t, asTeam(f.teamA), p.ID, a); !v3 || !cur || n != 1 {
		t.Fatalf("control: the team itself reads its page's asset: version 3 %v, current %v, listed %d", v3, cur, n)
	}
	for name, uid := range map[string]int64{"creator": f.creator, "viewer of A": f.viewerA, "member of both": f.both} {
		if v3, cur, n := f.assetReach(t, f.person(t, uid), p.ID, a); !v3 || !cur || n != 1 {
			t.Errorf("%s reads team A's page but not its asset: version 3 %v, current %v, listed %d; want all", name, v3, cur, n)
		}
	}
	for name, ctx := range map[string]context.Context{"member of B only": f.person(t, f.onlyB), "member of no team": f.person(t, f.outsider), "team B's agent": asTeam(f.teamB)} {
		if v3, cur, n := f.assetReach(t, ctx, p.ID, a); v3 || cur || n != 0 {
			t.Errorf("%s reads team A's page's asset: version 3 %v, current %v, listed %d; want none", name, v3, cur, n)
		}
	}
}

// TestNoMemberWritesATeamsPageAssets: a member of the team — even an operator,
// even the person who started its agent — adds no asset row to the team's
// page, changes none and deletes none, nor any version's use of one: the
// page's author (the team's agent) does. CONTROL: the team itself deletes an
// asset no version uses.
func TestNoMemberWritesATeamsPageAssets(t *testing.T) {
	f := newTeamFixture(t)
	p, a := f.teamAssetPage(t, f.teamA)
	for name, uid := range map[string]int64{"creator": f.creator, "viewer of A": f.viewerA, "member of both": f.both} {
		ctx := f.person(t, uid)
		if _, err := f.s.pool.Exec(ctx, `INSERT INTO page_assets (owner_id, page_id, public_id, content_type, size_bytes) VALUES ($1, $2, $3, 'image/png', 1)`,
			f.teamA, p.ID, assetID(int(f.teamA)*100+77)); err == nil {
			t.Errorf("%s added an asset row to team A's page", name)
		}
		for what, q := range map[string]string{
			"renamed an asset":        `UPDATE page_assets SET filename = 'x' WHERE page_id = $1`,
			"deleted an asset":        `DELETE FROM page_assets WHERE page_id = $1`,
			"deleted a version's use": `DELETE FROM page_version_assets WHERE page_id = $1`,
			"changed a version's use": `UPDATE page_version_assets SET version = version WHERE page_id = $1`,
		} {
			tag, err := f.s.pool.Exec(ctx, q, p.ID)
			if err == nil && tag.RowsAffected() != 0 {
				t.Errorf("%s %s on team A's page (%d rows)", name, what, tag.RowsAffected())
			}
		}
		if v3, _, n := f.assetReach(t, asTeam(f.teamA), p.ID, a); !v3 || n != 1 {
			t.Fatalf("after %s's attempts the team's asset is gone: version 3 %v, listed %d", name, v3, n)
		}
	}
	// CONTROL: the team's agent deletes an asset no version uses.
	extra, err := f.s.AddAsset(asTeam(f.teamA), Creator{AgentID: 900 + f.teamA}, p.ID, Asset{PublicID: assetID(int(f.teamA)*100 + 78), ContentType: "image/png", Size: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DeleteAsset(asTeam(f.teamA), Creator{AgentID: 900 + f.teamA}, p.ID, extra.PublicID); err != nil {
		t.Fatalf("control: the team's agent deleting an unused asset: %v", err)
	}
}

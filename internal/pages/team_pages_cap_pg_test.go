package pages

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// MaxPagesPerOwner IS THE OWNER'S OWN PAGES, not every page the caller can
// read. Since 0165 a member reads every page of every team they belong to, so
// a cap counted as "whatever row-level security lets me see" also counted the
// team's pages against the person (and refused their own create while they
// owned few). Each count below is distinct from the others and from the cap,
// and every one that should refuse overshoots it rather than landing on it.
func TestThePageCapCountsOnlyTheOwnersOwnPages(t *testing.T) {
	f := newTeamFixture(t)
	var (
		teamAPages   = MaxPagesPerOwner + 7 // team A: past the cap
		creatorPages = MaxPagesPerOwner - 5 // creator (operator of A): under it
		bothPages    = MaxPagesPerOwner + 3 // both (operator of A, viewer of B): past it
	)
	seed := func(owner int64, n int) {
		t.Helper()
		dbtest.OwnerExec(t, f.dsn, `INSERT INTO pages (owner_id, title, mirror_host)
			SELECT $1, 'seed ' || g, 'seed-host' FROM generate_series(1, $2) g`, owner, n)
	}
	seed(f.teamA, teamAPages)
	seed(f.creator, creatorPages)
	seed(f.both, bothPages)

	b, n := blocksOf(t, qBlocks)
	create := func(ctx context.Context) error {
		c := hostW
		if p, _ := authz.From(ctx); p.Team {
			c = Creator{AgentID: 900 + p.UserID, Label: "team-worker"}
		}
		_, _, err := f.s.Create(ctx, c, "mine", b, n)
		return err
	}

	// A member who owns no page, of a team past the cap, makes their own.
	if err := create(f.person(t, f.viewerA)); err != nil {
		t.Errorf("viewerA owns 0 pages (team A has %d): create: %v; want it made", teamAPages, err)
	}
	// One under the cap by their own count, over it only with the team's.
	if err := create(f.person(t, f.creator)); err != nil {
		t.Errorf("creator owns %d pages (team A has %d): create: %v; want it made", creatorPages, teamAPages, err)
	}

	// The cap still holds on the owner's own pages, and names THEIR count.
	refused := func(who string, ctx context.Context, own int) {
		t.Helper()
		err := create(ctx)
		want := "pages: too many: you have " + strconv.Itoa(own) + " pages, the most one owner may"
		if !errors.Is(err, ErrTooMany) || err.Error() != want {
			t.Errorf("%s: create: %v; want %q", who, err, want)
		}
	}
	refused("both (own pages past the cap)", f.person(t, f.both), bothPages)
	// The team's agent: its own pages only (a team principal holds no
	// memberships, so it never read its members' pages either).
	refused("team A's agent", asTeam(f.teamA), teamAPages)
	if err := create(asTeam(f.teamB)); err != nil {
		t.Errorf("team B's agent owns 0 pages: create: %v; want it made", err)
	}
}

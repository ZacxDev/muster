package attention

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/users"
)

// MaxOpenPerOwner IS THE OWNER'S OWN OPEN ASKS, not every open ask the caller
// can read. attention_asks is team-shared (0127: a viewer reads the team's
// rows), so a cap counted as "whatever row-level security lets me see" also
// counted each of the caller's teams' open asks against the person, and
// refused their own ask while they had none open.
func TestTheOpenAskCapCountsOnlyTheOwnersOwnAsks(t *testing.T) {
	s, pool, dsn := pgStore(t)
	ctx := context.Background()
	dbtest.OwnerExec(t, dsn, `SELECT setval(pg_get_serial_sequence('public.users', 'id'), (SELECT max(id) FROM public.users))`)
	var viewer, member int64
	for _, x := range []struct {
		id   *int64
		name string
	}{{&viewer, "Vic Viewer"}, {&member, "Mo Member"}} {
		if err := pool.QueryRow(authz.System(ctx), `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, x.name).Scan(x.id); err != nil {
			t.Fatal(err)
		}
	}
	us := users.NewPG(pool)
	by := users.Actor{UserID: 1, Via: authz.ViaSession}
	// The package's scratch database outlives a run: a name of this run's.
	team, err := us.CreateTeam(ctx, by, "Askers "+strconv.FormatInt(time.Now().UnixNano()%1e9, 36), member)
	if err != nil {
		t.Fatal(err)
	}
	if err := us.SetTeamMember(ctx, by, team.ID, viewer, authz.TeamViewer); err != nil {
		t.Fatal(err)
	}
	person := func(uid int64) context.Context {
		a, err := us.Account(authz.System(ctx), uid)
		if err != nil {
			t.Fatal(err)
		}
		return authz.WithPrincipal(ctx, authz.Principal{UserID: uid, Role: a.Role, Via: authz.ViaSession, Teams: a.Teams})
	}
	asTeam := authz.WithPrincipal(ctx, authz.Principal{UserID: team.ID, Role: authz.RoleMember, Via: authz.ViaAgent, Team: true})

	// The team: at its cap.
	for range MaxOpenPerOwner {
		if _, err := s.Create(asTeam, New{Asker: session, Question: "the team's"}); err != nil {
			t.Fatalf("the team's ask: %v", err)
		}
	}
	// The member: some of their own open, fewer than the cap.
	for range MaxOpenPerOwner - 7 {
		if _, err := s.Create(person(member), New{Asker: session, Question: "the member's"}); err != nil {
			t.Fatalf("the member's own ask (team has %d open): %v", MaxOpenPerOwner, err)
		}
	}
	// A viewer with none open asks; so does the member, under the cap.
	if _, err := s.Create(person(viewer), New{Asker: session, Question: "mine"}); err != nil {
		t.Errorf("viewer has 0 open asks (their team has %d): %v; want it asked", MaxOpenPerOwner, err)
	}
	if _, err := s.Create(person(member), New{Asker: session, Question: "mine"}); err != nil {
		t.Errorf("member has %d open asks (their team has %d): %v; want it asked", MaxOpenPerOwner-7, MaxOpenPerOwner, err)
	}
	// The cap still holds on each owner's own: the member, filled to it...
	for range 6 {
		if _, err := s.Create(person(member), New{Asker: session, Question: "the member's"}); err != nil {
			t.Fatalf("the member's own ask, filling to the cap: %v", err)
		}
	}
	if _, err := s.Create(person(member), New{Asker: session, Question: "one too many"}); !errors.Is(err, ErrTooMany) {
		t.Errorf("the member at their own cap: %v; want ErrTooMany", err)
	}
	// ...and the team's agent, at its.
	if _, err := s.Create(asTeam, New{Asker: session, Question: "one too many"}); !errors.Is(err, ErrTooMany) {
		t.Errorf("the team's agent at its cap: %v; want ErrTooMany", err)
	}
}

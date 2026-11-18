package users

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
)

// Teams, phase 1 of docs/design/team-sharing.md, against real Postgres.

func addPerson(t *testing.T, pool *pgxpool.Pool, email string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(authz.System(context.Background()),
		`INSERT INTO users (email, role, display_name) VALUES ($1, 'member', $1) RETURNING id`, email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

var admin = Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}

type auditRow struct {
	UserID int64
	Via    string
	Action string
	Object string
	Detail string
}

func teamAudit(t *testing.T, pool *pgxpool.Pool) []auditRow {
	t.Helper()
	rows, err := pool.Query(authz.System(context.Background()),
		`SELECT COALESCE(user_id, 0), via, action, object, detail::text FROM audit_log WHERE action LIKE 'team.%' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[auditRow])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestATeamChangeIsAuditedAsTheActingAdmin: creating a team, adding, re-roling
// and removing a member each write exactly one audit row naming the acting
// user and door, the team, and what changed — and a no-op re-role writes none.
func TestATeamChangeIsAuditedAsTheActingAdmin(t *testing.T) {
	s, pool := storeFor(t)
	ctx := context.Background()
	ana := addPerson(t, pool, "ana@example.test")
	ben := addPerson(t, pool, "ben@example.test")
	by := Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}

	team, err := s.CreateTeam(ctx, by, "  Platform  ", ana)
	if err != nil {
		t.Fatal(err)
	}
	if team.Name != "Platform" {
		t.Errorf("name = %q; want it trimmed", team.Name)
	}
	if err := s.SetTeamMember(ctx, by, team.ID, ben, authz.TeamViewer); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTeamMember(ctx, by, team.ID, ben, authz.TeamViewer); err != nil { // no-op
		t.Fatal(err)
	}
	if err := s.SetTeamMember(ctx, by, team.ID, ben, authz.TeamOperator); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveTeamMember(ctx, by, team.ID, ben); err != nil {
		t.Fatal(err)
	}
	obj := TeamObject(team.ID)
	want := []auditRow{
		{1, "session", "team.create", obj, `{"name": "Platform"}`},
		{1, "session", "team.member.set", obj, fmt.Sprintf(`{"role": "manager", "user": %d}`, ana)},
		{1, "session", "team.member.set", obj, fmt.Sprintf(`{"role": "viewer", "user": %d}`, ben)},
		{1, "session", "team.member.set", obj, fmt.Sprintf(`{"role": "operator", "user": %d, "before": "viewer"}`, ben)},
		{1, "session", "team.member.remove", obj, fmt.Sprintf(`{"role": "operator", "user": %d}`, ben)},
	}
	got := teamAudit(t, pool)
	if len(got) != len(want) {
		t.Fatalf("audit rows = %+v\nwant %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("audit row %d = %+v\nwant          %+v", i, got[i], want[i])
		}
	}
	// added_by is the acting admin, not the member.
	var addedBy int64
	if err := pool.QueryRow(authz.System(ctx), `SELECT added_by FROM team_members WHERE team_id = $1 AND user_id = $2`, team.ID, ana).Scan(&addedBy); err != nil || addedBy != 1 {
		t.Errorf("added_by = %d (%v); want the acting admin, 1", addedBy, err)
	}
}

// TestATeamWriteThatFailsLeavesNoAuditRow: a refused change writes no row
// (the change and its record are one transaction).
func TestATeamWriteThatFailsLeavesNoAuditRow(t *testing.T) {
	s, pool := storeFor(t)
	ctx := context.Background()
	ana := addPerson(t, pool, "ana@example.test")
	team, err := s.CreateTeam(ctx, admin, "Platform", ana)
	if err != nil {
		t.Fatal(err)
	}
	before := len(teamAudit(t, pool))
	other, err := s.CreateTeam(ctx, admin, "Other", ana)
	if err != nil {
		t.Fatal(err)
	}
	before += 2
	for name, c := range map[string]struct {
		do   func() error
		want error
	}{
		"a duplicate name, another case": {func() error { _, err := s.CreateTeam(ctx, admin, "PLATFORM", ana); return err }, ErrTeamNameTaken},
		"an empty name":                  {func() error { _, err := s.CreateTeam(ctx, admin, "   ", ana); return err }, ErrTeamName},
		"a 65-character name":            {func() error { _, err := s.CreateTeam(ctx, admin, strings.Repeat("a", 65), ana); return err }, ErrTeamName},
		"a team as first manager":        {func() error { _, err := s.CreateTeam(ctx, admin, "Third", team.ID); return err }, ErrNotAPerson},
		"a missing first manager":        {func() error { _, err := s.CreateTeam(ctx, admin, "Third", 999999); return err }, ErrNotAPerson},
		"a team as a member":             {func() error { return s.SetTeamMember(ctx, admin, team.ID, other.ID, authz.TeamViewer) }, ErrNotAPerson},
		"a person as the team":           {func() error { return s.SetTeamMember(ctx, admin, ana, ana, authz.TeamViewer) }, ErrNotATeam},
		"an unknown role":                {func() error { return s.SetTeamMember(ctx, admin, team.ID, ana, "owner") }, ErrTeamRole},
		"removing a non-member":          {func() error { return s.RemoveTeamMember(ctx, admin, team.ID, 1) }, ErrNotAMember},
		"removing from a person":         {func() error { return s.RemoveTeamMember(ctx, admin, ana, ana) }, ErrNotATeam},
	} {
		if err := c.do(); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v; want %v", name, err, c.want)
		}
	}
	if after := len(teamAudit(t, pool)); after != before {
		t.Errorf("refused changes wrote %d audit row(s)", after-before)
	}
}

// TestATeamRowIsNeverASignInIdentity: a team is invisible to every lookup a
// sign-in, a session or an admin action resolves a user through, cannot be
// claimed, and the schema refuses to give it a login, an admin role, a
// disable or a change of kind — each refusal by its own constraint.
func TestATeamRowIsNeverASignInIdentity(t *testing.T) {
	s, pool := storeFor(t)
	ctx := context.Background()
	sys := authz.System(ctx)
	ana := addPerson(t, pool, "ana@example.test")
	team, err := s.CreateTeam(ctx, admin, "Platform", ana)
	if err != nil {
		t.Fatal(err)
	}
	// Control: the same lookups find a person.
	if _, err := s.ByID(ctx, ana); err != nil {
		t.Fatalf("control: ByID(person) = %v", err)
	}
	if _, err := s.ByID(ctx, team.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("ByID(team) = %v; want ErrNotFound", err)
	}
	list, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	persons := 0
	for _, u := range list {
		if u.ID == team.ID {
			t.Errorf("ListUsers lists the team")
		}
		if u.ID == ana {
			persons++
		}
	}
	if persons != 1 {
		t.Errorf("control: ListUsers does not list the person")
	}
	if err := s.CreateClaimInvite(ctx, hash("team-claim"), team.ID, 1, time.Now().Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Errorf("CreateClaimInvite(team) = %v; want ErrNotFound", err)
	}
	if err := s.CreateClaimInvite(ctx, hash("person-claim"), addPerson(t, pool, "x@example.test"), 1, time.Now().Add(time.Hour)); err != nil {
		t.Errorf("control: CreateClaimInvite(person) = %v", err)
	}

	for name, q := range map[string]string{
		"a GoTrue subject": `UPDATE users SET gotrue_sub = '22222222-2222-4222-a222-222222222222' WHERE id = $1`,
		"an email":         `UPDATE users SET email = 'team@example.test' WHERE id = $1`,
		"the admin role":   `UPDATE users SET role = 'admin' WHERE id = $1`,
		"a disable":        `UPDATE users SET disabled_at = now() WHERE id = $1`,
		"an empty name":    `UPDATE users SET display_name = ' ' WHERE id = $1`,
	} {
		_, err := pool.Exec(sys, q, team.ID)
		assertConstraint(t, "giving the team "+name, err, "users_team_no_login")
		// Control: the same statement is accepted on a person (not the
		// admin-role one, which is a promotion, and not on user 1).
		if name != "the admin role" {
			p := addPerson(t, pool, "c-"+fmt.Sprint(len(name))+"@example.test")
			if name == "an email" {
				q = `UPDATE users SET email = 'other@example.test' WHERE id = $1`
			}
			if _, err := pool.Exec(sys, q, p); err != nil {
				t.Errorf("control: %s on a person: %v", name, err)
			}
		}
	}
	_, err = pool.Exec(sys, `UPDATE users SET kind = 'person' WHERE id = $1`, team.ID)
	assertMessage(t, "turning the team into a person", err, "users.kind never changes")
	_, err = pool.Exec(sys, `UPDATE users SET kind = 'team' WHERE id = $1`, ana)
	assertMessage(t, "turning a person into a team", err, "users.kind never changes")
	// Control: an update that does not touch kind is accepted on the team.
	if _, err := pool.Exec(sys, `UPDATE users SET display_name = 'Platform 2' WHERE id = $1`, team.ID); err != nil {
		t.Errorf("control: renaming the team: %v", err)
	}
}

// TestAccountIsTheTeamMembersTable is the seam guard for "membership is
// computed once per request in Go": Account's Teams, for every person, equals
// what team_members says — read independently here — through a random series
// of adds, re-roles and removals, and Team is set for exactly the team rows.
func TestAccountIsTheTeamMembersTable(t *testing.T) {
	s, pool := storeFor(t)
	ctx := context.Background()
	sys := authz.System(ctx)
	rng := rand.New(rand.NewSource(7))
	var people, teams []int64
	for i := 0; i < 5; i++ {
		people = append(people, addPerson(t, pool, fmt.Sprintf("p%d@example.test", i)))
	}
	for i := 0; i < 4; i++ {
		tm, err := s.CreateTeam(ctx, admin, fmt.Sprintf("team-%d", i), people[i])
		if err != nil {
			t.Fatal(err)
		}
		teams = append(teams, tm.ID)
	}
	roles := []authz.TeamRole{authz.TeamViewer, authz.TeamOperator, authz.TeamManager}
	fromTable := func(uid int64) authz.Memberships {
		rows, err := pool.Query(sys, `SELECT team_id, role FROM team_members WHERE user_id = $1`, uid)
		if err != nil {
			t.Fatal(err)
		}
		ms, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (authz.Membership, error) {
			var m authz.Membership
			var role string
			err := r.Scan(&m.Team, &role)
			m.Role = authz.TeamRole(role)
			return m, err
		})
		if err != nil {
			t.Fatal(err)
		}
		out, err := authz.NewMemberships(ms)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	nonEmpty := 0
	for step := 0; step < 40; step++ {
		team, person := teams[rng.Intn(len(teams))], people[rng.Intn(len(people))]
		if rng.Intn(3) == 0 {
			_ = s.RemoveTeamMember(ctx, admin, team, person)
		} else if err := s.SetTeamMember(ctx, admin, team, person, roles[rng.Intn(3)]); err != nil {
			t.Fatal(err)
		}
		for _, p := range append([]int64{1}, people...) {
			a, err := s.Account(sys, p)
			if err != nil {
				t.Fatal(err)
			}
			if want := fromTable(p); a.Teams != want || a.Team {
				t.Fatalf("step %d: Account(%d) = %+v; team_members says %q", step, p, a, want)
			}
			if a.Teams != "" {
				nonEmpty++
			}
		}
	}
	// Positive control: the walk saw memberships, so equality is not "" == "".
	if nonEmpty < 20 {
		t.Fatalf("only %d non-empty membership reads; the walk did not exercise the seam", nonEmpty)
	}
	for _, tm := range teams {
		a, err := s.Account(sys, tm)
		if err != nil || !a.Team || a.Teams != "" || a.Role != authz.RoleMember {
			t.Errorf("Account(team %d) = %+v, %v; want Team, no memberships, member", tm, a, err)
		}
	}
	if _, err := s.Account(sys, 999999); !errors.Is(err, ErrNotFound) {
		t.Errorf("Account(missing) = %v; want ErrNotFound", err)
	}
}

func assertConstraint(t *testing.T, what string, err error, constraint string) {
	t.Helper()
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "23514" || pg.ConstraintName != constraint {
		t.Errorf("%s: err = %v; want CHECK %s", what, err, constraint)
	}
}

func assertMessage(t *testing.T, what string, err error, msg string) {
	t.Helper()
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "23514" || !strings.Contains(pg.Message, msg) {
		t.Errorf("%s: err = %v; want a check_violation saying %q", what, err, msg)
	}
}

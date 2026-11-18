package api

import (
	"context"
	"fmt"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/users"
)

// TestAPerOwnerSweepNeverActsAsATeam is #654 round 1's 🔴 regression: with
// the REAL users store (a principal's kind is read, as production reads it),
// creating a team must not make a per-owner sweep build a principal for it.
// Before the fix, chiefnudge's owner list (which session state shares) named
// every enabled users row, the team included; the pass then asked for the
// team's principal and authz.WithPrincipal panicked — in a bare goroutine,
// so the whole server crash-looped. Each pass here runs with a recover so a
// regression is a named failure, not a dead test binary. The CONTROL is that
// the same passes, before the team exists, also run (so "no panic" is not a
// pass that does nothing at all), and that the owner list still names the
// fixture's people.
func TestAPerOwnerSweepNeverActsAsATeam(t *testing.T) {
	f := newStateFixture(t, true)
	f.srv.ext.Users = users.NewPG(f.pool)
	run := func(what string, pass func() error) {
		t.Helper()
		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("PANIC: %v", r)
				}
			}()
			err = pass()
		}()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	passes := func(label string) {
		run(label+": the session-state pass", func() error { return f.srv.SessionStatePass(context.Background()) })
		run(label+": the chief-nudge pass", func() error { _, err := f.srv.ChiefNudgePass(context.Background()); return err })
	}
	passes("control, before any team")

	team, err := users.NewPG(f.pool).CreateTeam(authz.System(context.Background()),
		users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}, "sweep-team-probe", f.member)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		_, _ = f.pool.Exec(bg, `DELETE FROM team_members WHERE team_id = $1`, team.ID)
		_, _ = f.pool.Exec(bg, `DELETE FROM audit_log WHERE object = $1`, users.TeamObject(team.ID))
		_, _ = f.pool.Exec(bg, `DELETE FROM users WHERE id = $1`, team.ID)
	})
	passes("with a team")

	owners, err := f.srv.ext.ChiefNudges.Owners(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	people := 0
	for _, o := range owners {
		if o.ID == team.ID {
			t.Errorf("the per-owner sweeps' owner list names the team %d", team.ID)
		}
		if o.ID == authz.OperatorUserID || o.ID == f.member {
			people++
		}
	}
	if people != 2 {
		t.Errorf("control: the owner list names %d of the fixture's 2 people: %+v", people, owners)
	}
}

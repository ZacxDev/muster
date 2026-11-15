package users

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
)

// rotateAt is token id's rotate_at.
func rotateAt(t *testing.T, s *PGStore, id int64) time.Time {
	t.Helper()
	var at time.Time
	if err := s.pool.QueryRow(authz.System(context.Background()), `SELECT rotate_at FROM mirror_tokens WHERE id = $1`, id).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

// TestTheRotateReasonIsReadFromTheTeamsAudit: the reason is DERIVED from the
// audit row the mark's own transaction wrote (no reason column): a removal
// names the removed operator, a disable names the disabled one, and a later
// trigger moves the mark and its reason forward. The person's id is a fixture
// value (a fresh row) that no constant equals, and the names are pairwise
// distinct, so a reason built from the wrong row is visible.
func TestTheRotateReasonIsReadFromTheTeamsAudit(t *testing.T) {
	s, pool := storeFor(t)
	ctx := context.Background()
	rt := newRotateTeam(t, s, pool, "why")
	if _, err := s.TeamRotateReason(ctx, rt.team, time.Now().Add(time.Hour)); !errors.Is(err, ErrNoRotateReason) {
		t.Fatalf("before any trigger: %v; want ErrNoRotateReason", err)
	}
	if err := s.RemoveTeamMemberAsManager(ctx, Actor{UserID: rt.ana, Via: authz.ViaSession}, rt.team, rt.ben); err != nil {
		t.Fatal(err)
	}
	r, err := s.TeamRotateReason(ctx, rt.team, rotateAt(t, s, rt.token))
	if err != nil {
		t.Fatal(err)
	}
	if r.UserID != rt.ben || r.Name != "why-ben@example.test" || r.Kind != RotateLeft {
		t.Fatalf("the reason = %+v; want ben (%d, why-ben@example.test) left", r, rt.ben)
	}
	// A VIEWER's later removal is no reason: read past it, ben still is.
	if err := s.RemoveTeamMemberAsManager(ctx, Actor{UserID: rt.ana, Via: authz.ViaSession}, rt.team, rt.cal); err != nil {
		t.Fatal(err)
	}
	if r, err := s.TeamRotateReason(ctx, rt.team, time.Now().Add(time.Hour)); err != nil || r.UserID != rt.ben {
		t.Fatalf("after a viewer's removal the reason = %+v, %v; want still ben (%d)", r, err, rt.ben)
	}
	// A disable of a remaining operator moves it forward.
	dee := addPerson(t, pool, "why-dee@example.test")
	if err := s.SetTeamMember(ctx, admin, rt.team, dee, authz.TeamOperator); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetDisabledAs(ctx, &admin, dee, true); err != nil {
		t.Fatal(err)
	}
	r, err = s.TeamRotateReason(ctx, rt.team, rotateAt(t, s, rt.token))
	if err != nil {
		t.Fatal(err)
	}
	if r.UserID != dee || r.Name != "why-dee@example.test" || r.Kind != RotateDisabled {
		t.Fatalf("after dee's disable the reason = %+v; want dee (%d) was disabled", r, dee)
	}
}

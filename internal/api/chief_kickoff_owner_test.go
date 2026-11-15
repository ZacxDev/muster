package api

import (
	"context"
	"io"
	"log"
	"sync/atomic"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
)

// TestAnUnreadableOwnersChiefIsNotKickedOffAsATeams (round 0, item 4): the
// kickoff of a new chief is chosen by its OWNER, and when the owner's account
// cannot be read (and the principal on ctx is not the owner, so it cannot
// answer) the choice is REFUSED — never the team's text for what may be a
// person's chief. ownerIsTeam keeps answering "team" on the same failure for
// its refusal callers (the control: it still does).
//
// Controls: the account readable, a person's owner gets the person's kickoff;
// the owner's own principal answers without any read.
//
// 🔴 RED at 0740e0cb's choice (chiefKickoff(s.ownerIsTeam(ctx, owner))): the
// unreadable owner got ChiefKickoffTeam.
func TestAnUnreadableOwnersChiefIsNotKickedOffAsATeams(t *testing.T) {
	const person = int64(4242)
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	var fail atomic.Bool
	s.ext.Users = failingUsers{fail: &fail}
	// A principal that is not the owner: the read is what decides.
	other := authz.WithPrincipal(context.Background(), authz.Principal{UserID: authz.OperatorUserID, Role: authz.RoleAdmin, Via: authz.ViaSession})

	fail.Store(true)
	if got, err := s.chiefKickoffFor(other, person); err == nil || got != "" {
		t.Errorf("unreadable owner: kickoff %q, %v; want no kickoff and an error (refused, not guessed)", got, err)
	}
	if !s.ownerIsTeam(other, person) {
		t.Errorf("control: ownerIsTeam stopped failing closed (it answers \"team\" on a read error for its refusal callers)")
	}

	fail.Store(false)
	if got, err := s.chiefKickoffFor(other, person); err != nil || got != ChiefKickoffPerson {
		t.Errorf("readable person: kickoff %q, %v; want the person's", got, err)
	}
	fail.Store(true)
	self := authz.WithPrincipal(context.Background(), authz.Principal{UserID: person, Role: authz.RoleMember, Via: authz.ViaSession})
	if got, err := s.chiefKickoffFor(self, person); err != nil || got != ChiefKickoffPerson {
		t.Errorf("the owner's own principal: kickoff %q, %v; want the person's, with no read", got, err)
	}
}

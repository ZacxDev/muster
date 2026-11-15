package credentials

import (
	"context"
	"errors"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/ccpool"
)

// poolRoles: admins by id; FirstAdmin answers first (or err). User 1 is NOT an
// admin here — the demoted-operator deployment — and no id in it equals
// authz.OperatorUserID except that one, so a MarksOwner that answered the
// constant is seen.
type poolRoles struct {
	admins map[int64]bool
	first  int64
	err    error
}

func (r poolRoles) Role(_ context.Context, id int64) (authz.Role, error) {
	if r.admins[id] {
		return authz.RoleAdmin, nil
	}
	return authz.RoleMember, nil
}

func (r poolRoles) FirstAdmin(context.Context) (int64, error) { return r.first, r.err }

// TestMarksOwnerIsThePoolOwnerForEveryAdminAndListedTeam (round 0, items 1
// and 2): every owner on the server pool — an admin whose id is NOT the pool
// owner's, the pool owner itself, a listed team — keeps its marks under ONE
// owner, the plane's EnvPoolOwner (the lowest-id enabled admin), and never
// under user 1 once user 1 is not an admin. A member's and a demoted user 1's
// accounts are their own (false). A failed pool-owner read shares nothing.
//
// 🔴 It is the admin case the earlier suite lacked: there the only admin was
// user 1, the pool owner, so MarksOwner answering onServerPool (teams only)
// left the admin's marks under its own id — the same id — and survived.
func TestMarksOwnerIsThePoolOwnerForEveryAdminAndListedTeam(t *testing.T) {
	const first, second, team = int64(3), int64(7), int64(70)
	env, err := ccpool.NewStatic(map[string]string{"work": envTokWork})
	if err != nil {
		t.Fatal(err)
	}
	p := &Plane{EnvClaude: env, Personal: fakePersonal{}, TeamsOnServerPool: map[int64]bool{team: true},
		Roles: poolRoles{admins: map[int64]bool{first: true, second: true}, first: first}}
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		owner  int64
		want   int64
		shared bool
	}{
		{"an admin who is not the pool owner", second, first, true},
		{"the pool owner", first, first, true},
		{"a listed team", team, first, true},
		{"a member", memberA, 0, false},
		{"user 1, demoted", authz.OperatorUserID, 0, false},
	} {
		if got, shared := p.MarksOwner(ctx, c.owner); got != c.want || shared != c.shared {
			t.Errorf("%s (%d): MarksOwner = %d, %v; want %d, %v", c.name, c.owner, got, shared, c.want, c.shared)
		}
	}
	if got, ok := p.EnvPoolOwner(ctx); got != first || !ok {
		t.Errorf("EnvPoolOwner = %d, %v; want %d, true", got, ok, first)
	}

	failing := *p
	failing.Roles = poolRoles{admins: map[int64]bool{first: true, second: true}, err: errors.New("synthetic: users unreadable")}
	if got, shared := failing.MarksOwner(ctx, second); got != 0 || shared {
		t.Errorf("pool owner unreadable: MarksOwner(admin) = %d, %v; want 0, false (each owner its own marks)", got, shared)
	}
	if got, ok := failing.EnvPoolOwner(ctx); got != 0 || ok {
		t.Errorf("pool owner unreadable: EnvPoolOwner = %d, %v; want 0, false", got, ok)
	}

	// Control: no server pool, nothing is shared — whoever asks.
	none := *p
	none.EnvClaude = nil
	if got, shared := none.MarksOwner(ctx, second); got != 0 || shared {
		t.Errorf("control: no environment pool: MarksOwner(admin) = %d, %v; want 0, false", got, shared)
	}
}

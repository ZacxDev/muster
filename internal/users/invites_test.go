package users

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/authz"
)

var opActor = Actor{UserID: 1, Via: authz.ViaSession}

const subB = "22222222-2222-4222-a222-222222222222"

func okCreate(sub string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return sub, nil }
}

func TestInviteStatesAndAudit(t *testing.T) {
	s, pool := storeFor(t)
	ctx := context.Background()
	now := time.Now()
	pending, err := s.CreateInvite(ctx, opActor, hash("p"), "", authz.RoleMember, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	revoked, _ := s.CreateInvite(ctx, opActor, hash("r"), "", authz.RoleMember, now.Add(time.Hour))
	if err := s.RevokeInvite(ctx, opActor, revoked.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeInvite(ctx, opActor, revoked.ID); !errors.Is(err, ErrNotRevocable) {
		t.Fatalf("revoking twice: %v", err)
	}
	expired, _ := s.CreateInvite(ctx, opActor, hash("e"), "", authz.RoleMember, now.Add(-time.Second))
	redeemed, _ := s.CreateInvite(ctx, opActor, hash("d"), "", authz.RoleAdmin, now.Add(time.Hour))
	u, err := s.RedeemInvite(ctx, hash("d"), "d@example.com", "Dee", okCreate(subA), nil)
	if err != nil || u.DisplayName != "Dee" {
		t.Fatalf("redeem: %+v %v", u, err)
	}
	if r, err := s.Role(authz.System(ctx), u.ID); err != nil || r != authz.RoleAdmin {
		t.Fatalf("the redeemed user's role is %q (%v), want the invite's admin", r, err)
	}
	if err := s.RevokeInvite(ctx, opActor, redeemed.ID); !errors.Is(err, ErrNotRevocable) {
		t.Fatalf("revoking a redeemed invite: %v", err)
	}
	list, err := s.ListInvites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]InviteState{}
	for _, i := range list {
		got[i.ID] = i.State(time.Now())
	}
	want := map[int64]InviteState{pending.ID: InvitePending, revoked.ID: InviteRevoked, expired.ID: InviteExpired, redeemed.ID: InviteRedeemed}
	for id, st := range want {
		if got[id] != st {
			t.Errorf("invite %d: %q, want %q", id, got[id], st)
		}
	}
	for name, h := range map[string][]byte{"revoked": hash("r"), "expired": hash("e"), "redeemed": hash("d"), "unknown": hash("x")} {
		if _, err := s.OpenInvite(ctx, h); !errors.Is(err, ErrInvalidInvite) {
			t.Errorf("open %s: %v", name, err)
		}
	}
	if _, err := s.OpenInvite(ctx, hash("p")); err != nil {
		t.Fatalf("control: open pending: %v", err)
	}
	// A claim link is not a general invite: invisible to the list, to Open and to Revoke.
	if err := s.CreateClaimInvite(ctx, hash("claim"), 1, 1, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenInvite(ctx, hash("claim")); !errors.Is(err, ErrInvalidInvite) {
		t.Fatalf("a claim link opened as an invite: %v", err)
	}
	var claimID int64
	_ = pool.QueryRow(authz.System(ctx), `SELECT id FROM invites WHERE claim_user_id = 1`).Scan(&claimID)
	if err := s.RevokeInvite(ctx, opActor, claimID); !errors.Is(err, ErrNotRevocable) {
		t.Fatalf("revoking a claim link through RevokeInvite: %v", err)
	}
	actions := map[string]int{}
	entries, _ := s.RecentAudit(ctx, 100)
	for _, e := range entries {
		actions[e.Action]++
	}
	if actions["invite.create"] != 4 || actions["invite.revoke"] != 1 || actions["invite.redeem"] != 1 {
		t.Fatalf("audit actions %v", actions)
	}
}

func TestRedeemInviteChecksTheBoundEmailBeforeCreating(t *testing.T) {
	s, _ := storeFor(t)
	ctx := context.Background()
	if _, err := s.CreateInvite(ctx, opActor, hash("b"), "Ann@Example.com", authz.RoleMember, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	called := false
	create := func(context.Context) (string, error) { called = true; return subA, nil }
	if _, err := s.RedeemInvite(ctx, hash("b"), "bob@example.com", "", create, nil); !errors.Is(err, ErrEmailMismatch) || called {
		t.Fatalf("mismatch: %v (create called %v)", err, called)
	}
	u, err := s.RedeemInvite(ctx, hash("b"), "ann@example.COM", "", create, nil)
	if err != nil || u.Email != "Ann@Example.com" {
		t.Fatalf("case-insensitive match: %+v %v", u, err)
	}
	// A bound email already in use is refused at creation.
	if _, err := s.CreateInvite(ctx, opActor, hash("b2"), "ANN@example.com", authz.RoleMember, time.Now().Add(time.Hour)); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("an invite for a taken email: %v", err)
	}
}

func TestRedeemInviteDecidesAnAmbiguousCommitByRereading(t *testing.T) {
	for _, tc := range []struct {
		name     string
		commit   func(ctx context.Context, tx pgx.Tx) error
		wantErr  bool
		wantUndo bool
		wantRow  bool
	}{
		{"committed, reply lost", func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.Commit(ctx); err != nil {
				return err
			}
			return errors.New("connection reset after COMMIT")
		}, false, false, true},
		{"rolled back", func(ctx context.Context, tx pgx.Tx) error {
			_ = tx.Rollback(ctx)
			return errors.New("connection reset before COMMIT")
		}, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, pool := storeFor(t)
			ctx := context.Background()
			if _, err := s.CreateInvite(ctx, opActor, hash("amb"), "", authz.RoleMember, time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			s.commit = tc.commit
			undone := ""
			_, err := s.RedeemInvite(ctx, hash("amb"), "amb@example.com", "", okCreate(subB), func(sub string) { undone = sub })
			if (err != nil) != tc.wantErr || (undone != "") != tc.wantUndo {
				t.Fatalf("err %v undo %q; want err=%v undo=%v", err, undone, tc.wantErr, tc.wantUndo)
			}
			var n int
			_ = pool.QueryRow(authz.System(ctx), `SELECT count(*) FROM users WHERE gotrue_sub = $1::uuid`, subB).Scan(&n)
			if (n == 1) != tc.wantRow {
				t.Fatalf("users rows for the sub: %d", n)
			}
		})
	}
}

func TestTheLastEnabledAdminIsGuarded(t *testing.T) {
	s, pool := storeFor(t)
	ctx := context.Background()
	if _, err := s.SetRole(ctx, opActor, 1, authz.RoleMember); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("demoting the only admin: %v", err)
	}
	if _, err := s.SetDisabled(ctx, 1, true); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("disabling the only admin: %v", err)
	}
	// A DISABLED second admin does not count.
	var second int64
	if err := pool.QueryRow(authz.System(ctx), `INSERT INTO users (role, disabled_at) VALUES ('admin', now()) RETURNING id`).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRole(ctx, opActor, 1, authz.RoleMember); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("demoting the only ENABLED admin: %v", err)
	}
	if _, err := s.SetDisabled(ctx, second, false); err != nil {
		t.Fatal(err)
	}
	// Now there are two: either may go, and a member is never guarded.
	if _, err := s.SetDisabled(ctx, 1, true); err != nil {
		t.Fatalf("disabling one of two admins: %v", err)
	}
	if _, err := s.SetRole(ctx, opActor, second, authz.RoleMember); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("demoting the remaining enabled admin: %v", err)
	}
	if _, err := s.SetDisabled(ctx, 1, false); err != nil {
		t.Fatal(err)
	}
	_, err := s.SetRole(ctx, opActor, second, authz.RoleMember)
	r, _ := s.Role(authz.System(ctx), second)
	if err != nil || r != authz.RoleMember {
		t.Fatalf("demote with another admin: role %q, %v", r, err)
	}
	var member int64
	_ = pool.QueryRow(authz.System(ctx), `INSERT INTO users (role) VALUES ('member') RETURNING id`).Scan(&member)
	if _, err := s.SetDisabled(ctx, member, true); err != nil {
		t.Fatalf("disabling a member: %v", err)
	}
}

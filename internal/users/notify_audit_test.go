package users

import (
	"context"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
)

// TestAnAgentsNotificationsAreTheOwnersRecordNotTheAdminsList: an
// agent.notify row (internal/api notify_owner.go) is read by its owner through
// OwnAudit — their own rows of that action only, even when the read runs with
// the all-rows scope an admin has — and is left off RecentAudit, the admins'
// activity list, where the other actions stay (the control).
func TestAnAgentsNotificationsAreTheOwnersRecordNotTheAdminsList(t *testing.T) {
	s, pool := storeFor(t)
	ctx := context.Background()
	var member int64
	if err := pool.QueryRow(authz.System(ctx), `INSERT INTO users (role, display_name) VALUES ('member', 'Notify Member') RETURNING id`).Scan(&member); err != nil {
		t.Fatal(err)
	}
	as := func(uid int64) context.Context {
		return authz.WithPrincipal(ctx, authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaAgent})
	}
	for _, r := range []struct {
		uid    int64
		action string
	}{{member, NotifyAuditAction}, {member, NotifyAuditAction}, {member, "user.passkey_added"}, {1, NotifyAuditAction}, {1, "invite.create"}} {
		if err := s.Audit(ctx, Actor{UserID: r.uid, Via: authz.ViaAgent}, r.action, "agent:9", map[string]any{"title": "t"}); err != nil {
			t.Fatal(err)
		}
	}
	own, err := s.OwnAudit(as(member), member, NotifyAuditAction, 10)
	if err != nil || len(own) != 2 {
		t.Fatalf("the member's own notify rows: %d %v; want 2", len(own), err)
	}
	for _, e := range own {
		if e.UserID != member || e.Action != NotifyAuditAction || e.Detail["title"] != "t" {
			t.Fatalf("a row that is not the member's own notify: %+v", e)
		}
	}
	// The all-rows scope (an admin's) still reads only the uid it names.
	if all, err := s.OwnAudit(authz.System(ctx), member, NotifyAuditAction, 10); err != nil || len(all) != 2 {
		t.Fatalf("an all-scope read naming the member: %d %v; want their 2", len(all), err)
	}
	// Another principal naming the member reads nothing (row-level security).
	if other, err := s.OwnAudit(as(1), member, NotifyAuditAction, 10); err != nil || len(other) != 0 {
		t.Fatalf("the admin's own principal read the member's rows: %d %v", len(other), err)
	}
	recent, err := s.RecentAudit(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, e := range recent {
		seen[e.Action]++
	}
	if seen[NotifyAuditAction] != 0 || seen["invite.create"] != 1 || seen["user.passkey_added"] != 1 {
		t.Fatalf("the admins' list: %v; want no %s and the other actions", seen, NotifyAuditAction)
	}
}

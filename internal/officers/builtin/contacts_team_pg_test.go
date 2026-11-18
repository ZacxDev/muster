package builtin_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/officers/builtin"
	"github.com/ZacxDev/muster/internal/users"
)

// TestATeamOfficersPeopleAreItsTeamsOnly: a team officer's contacts (owner =
// the team) are read by the team's operators and managers — so a manager
// deciding the team's outreach re-checks the real person, not a hidden row —
// marked by its managers only, and invisible to a viewer, to a stranger and
// to another team, for whom the re-check fails closed.
func TestATeamOfficersPeopleAreItsTeamsOnly(t *testing.T) {
	w := newPGWorld(t, false)
	pool := w.store.Pool()
	sys := authz.System(context.Background())
	us := users.NewPG(pool)
	by := users.Actor{UserID: 1, Via: authz.ViaSession}
	// Other tests of this package plant users at fixed ids: move the
	// sequence past them first.
	if _, err := pool.Exec(sys, `SELECT setval(pg_get_serial_sequence('users', 'id'), (SELECT max(id) FROM users))`); err != nil {
		t.Fatal(err)
	}
	person := func(name string) int64 {
		var id int64
		if err := pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, name+"-"+time.Now().Format("150405.000000")).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	mgr, op, viewer, stranger, otherMgr := person("c-mgr"), person("c-op"), person("c-viewer"), person("c-stranger"), person("c-othermgr")
	team, err := us.CreateTeam(sys, by, "contacts-team-"+time.Now().Format("150405.000000"), mgr)
	if err != nil {
		t.Fatal(err)
	}
	other, err := us.CreateTeam(sys, by, "contacts-other-"+time.Now().Format("150405.000000"), otherMgr)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct {
		id   int64
		role authz.TeamRole
	}{{op, authz.TeamOperator}, {viewer, authz.TeamViewer}} {
		if err := us.SetTeamMember(sys, by, team.ID, m.id, m.role); err != nil {
			t.Fatal(err)
		}
	}
	as := func(uid int64) context.Context {
		a, err := us.Account(sys, uid)
		if err != nil {
			t.Fatal(err)
		}
		return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: a.Role, Via: authz.ViaSession, Teams: a.Teams})
	}
	asTeam := authz.WithPrincipal(context.Background(), authz.Principal{UserID: team.ID, Role: authz.RoleMember, Via: authz.ViaOfficer})
	const officer = "teamkt:main"
	if err := w.store.PutType(asTeam, team.ID, builtin.Type{Key: "teamkt", Name: "Team marketing"}); err != nil {
		t.Fatal(err)
	}
	if err := w.store.PutDef(asTeam, team.ID, builtin.Def{Officer: officer, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM officer_contacts WHERE owner_id = $1`, `DELETE FROM officer_items WHERE owner_id = $1`,
			`DELETE FROM officer_defs WHERE owner_id = $1`, `DELETE FROM officer_types WHERE owner_id = $1`} {
			_, _ = pool.Exec(sys, q, team.ID)
		}
	})
	tx, err := pool.Begin(asTeam)
	if err != nil {
		t.Fatal(err)
	}
	res, err := w.store.RecordEngagementTx(asTeam, tx, team.ID, officer, "example.social", builtin.Person{Handle: "zed", Display: "Zed Person"},
		builtin.Engagement{Kind: builtin.EventReply, Ref: "r1", OurPostRef: "p1", Text: "hi"}, builtin.EngagementSource{PluginID: 1}, w.clock())
	if err != nil {
		t.Fatalf("the team officer's engagement: %v", err)
	}
	if err := tx.Commit(asTeam); err != nil {
		t.Fatal(err)
	}
	blocked := func(ctx context.Context) string {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		why, err := w.store.ContactBlockedTx(ctx, tx, team.ID, res.Contact.ID)
		if err != nil {
			t.Fatal(err)
		}
		return why
	}
	// The re-check is the decider's: a team's drafts are its MANAGERS' to
	// decide (0127/0130), and it reads under a row lock, which only the write
	// role reaches. An operator READS the person.
	if why := blocked(as(mgr)); why != "" {
		t.Errorf("the team's manager re-checks the team's contactable person as %q; want readable and contactable", why)
	}
	if _, _, err := w.store.ContactGet(as(op), team.ID, officer, "example.social", "zed", 0); err != nil {
		t.Errorf("the team's operator cannot read the team's person: %v", err)
	}
	for name, uid := range map[string]int64{"viewer": viewer, "stranger": stranger, "another team's manager": otherMgr} {
		if why := blocked(as(uid)); why != builtin.BlockedUnreadable {
			t.Errorf("%s re-checks the team's person as %q; want %q (never readable)", name, why, builtin.BlockedUnreadable)
		}
		if _, _, err := w.store.ContactGet(as(uid), team.ID, officer, "example.social", "zed", 0); err == nil {
			t.Errorf("%s read the team's person", name)
		}
	}
	// The team's ITEMS are read at viewer (0127): the engagement item names
	// the person by id only, so a viewer reading it reads no person.
	if res.Item == 0 {
		t.Fatal("control: the engagement opened no item")
	}
	var raw string
	if err := pool.QueryRow(as(viewer), `SELECT payload::text FROM officer_items WHERE id = $1`, res.Item).Scan(&raw); err != nil {
		t.Fatalf("control: the team's viewer cannot read the team's items at all: %v", err)
	}
	if strings.Contains(raw, "zed") || strings.Contains(raw, `"hi"`) {
		t.Fatalf("the team's viewer reads the person in the item: %s", raw)
	}
	yes := true
	if _, err := w.store.SetContactFlags(as(op), team.ID, officer, "example.social", "zed", builtin.FlagsSet{DNC: &yes}, w.clock()); err == nil {
		t.Error("the team's operator marked the team's person: only its managers write")
	}
	c, err := w.store.SetContactFlags(as(mgr), team.ID, officer, "example.social", "zed", builtin.FlagsSet{DNC: &yes}, w.clock())
	if err != nil || !c.Flags.DNC {
		t.Fatalf("the team's manager marking the person do-not-contact: %+v %v", c, err)
	}
	if why := blocked(as(mgr)); why != "do_not_contact" {
		t.Fatalf("after the manager's mark the re-check is %q", why)
	}
	_ = other
}

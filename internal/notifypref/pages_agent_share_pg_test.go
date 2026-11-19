package notifypref

import (
	"context"
	"errors"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestPagesAgentShareIsOnUntilTurnedOffAndIsPerUserAndIndependent: "Let my
// agents share pages without asking" reads ON for every person who never
// chose (migration 0159) — with no row, and with a row another switch wrote;
// off is that user's alone, and leaves their other switches as they were;
// another user can neither read nor write it; the system scope reads it.
func TestPagesAgentShareIsOnUntilTurnedOffAndIsPerUserAndIndependent(t *testing.T) {
	p := pgStore(t)
	for _, uid := range []int64{authz.OperatorUserID, 2, 3} {
		if on, err := p.PagesAgentShare(asUser(uid), uid); err != nil || !on {
			t.Fatalf("user %d by default (no row): on=%v err=%v, want on", uid, on, err)
		}
	}
	if err := p.SetTmuxAddress(asUser(2), 2, true); err != nil {
		t.Fatal(err)
	}
	if on, err := p.PagesAgentShare(asUser(2), 2); err != nil || !on {
		t.Fatalf("a row written by the tmux switch: on=%v err=%v, want on (the column default)", on, err)
	}
	if err := p.SetPagesAgentShare(asUser(2), 2, false); err != nil {
		t.Fatal(err)
	}
	if on, err := p.PagesAgentShare(asUser(2), 2); err != nil || on {
		t.Fatalf("user 2 turned it off: on=%v err=%v", on, err)
	}
	if on, _ := p.TmuxAddress(asUser(2), 2); !on {
		t.Fatal("the pages switch turned the tmux switch off")
	}
	if on, _ := p.PagesAgentShare(asUser(3), 3); !on {
		t.Fatal("user 2's off reached user 3")
	}
	if on, _ := p.PagesAgentShare(asUser(authz.OperatorUserID), authz.OperatorUserID); !on {
		t.Fatal("user 2's off reached the operator")
	}
	if on, err := p.PagesAgentShare(authz.System(context.Background()), 2); err != nil || on {
		t.Fatalf("the system scope reads user 2's off: on=%v err=%v", on, err)
	}
	if err := p.SetPagesAgentShare(asUser(2), 2, true); err != nil {
		t.Fatal(err)
	}
	if on, err := p.PagesAgentShare(asUser(2), 2); err != nil || !on {
		t.Fatalf("user 2 turned it on: on=%v err=%v", on, err)
	}
	if _, err := p.PagesAgentShare(asUser(3), 2); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("user 3 read user 2's opt-in: err=%v", err)
	}
	if _, err := p.PagesAgentShare(context.Background(), 2); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("a principal-less read: err=%v", err)
	}
	if on, err := p.PagesAgentShare(authz.System(context.Background()), 2); err != nil || !on {
		t.Fatalf("the system scope reads user 2's on: on=%v err=%v", on, err)
	}
	if err := p.SetPagesAgentShare(asUser(3), 2, false); err == nil {
		t.Fatal("user 3 wrote user 2's opt-in")
	}
	if err := p.SetPagesAgentShare(asUser(2), 2, true); err != nil {
		t.Fatal(err)
	}
	if on, _ := p.PagesAgentShare(asUser(2), 2); !on {
		t.Fatal("user 3's write changed user 2's opt-in")
	}
}

// TestPagesAgentShareIsAlwaysOffForATeam: a TEAM owner (users.kind =
// 'team') with no row reads OFF — nobody signs in as a team, so nobody could
// turn it off for one — read as the team acting principal and as the system;
// a person beside it, also with no row, reads on (the control). Then the
// team's row is forced to TRUE: it still reads off (a team is never
// auto-shared, whatever is stored), while a person's stored true reads on.
func TestPagesAgentShareIsAlwaysOffForATeam(t *testing.T) {
	p := pgStore(t)
	dbtest.OwnerExec(t, dbtest.DSN(t), `INSERT INTO users (id, role, kind, display_name) OVERRIDING SYSTEM VALUE
		VALUES (9001, 'member', 'team', 'pages default team') ON CONFLICT (id) DO NOTHING`)
	team := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 9001, Team: true, Role: authz.RoleMember, Via: authz.ViaAgent})
	if on, err := p.PagesAgentShare(team, 9001); err != nil || on {
		t.Fatalf("a team with no row, as the team: on=%v err=%v, want off", on, err)
	}
	if on, err := p.PagesAgentShare(authz.System(context.Background()), 9001); err != nil || on {
		t.Fatalf("a team with no row, as the system: on=%v err=%v, want off", on, err)
	}
	if on, err := p.PagesAgentShare(asUser(3), 3); err != nil || !on {
		t.Fatalf("control: a person with no row: on=%v err=%v, want on", on, err)
	}
	dbtest.OwnerExec(t, dbtest.DSN(t), `INSERT INTO notify_preferences (owner_id, pages_agent_share) VALUES (9001, true), (3, true)`)
	if on, err := p.PagesAgentShare(team, 9001); err != nil || on {
		t.Fatalf("a team whose row is forced true, as the team: on=%v err=%v, want off", on, err)
	}
	if on, err := p.PagesAgentShare(authz.System(context.Background()), 9001); err != nil || on {
		t.Fatalf("a team whose row is forced true, as the system: on=%v err=%v, want off", on, err)
	}
	if on, err := p.PagesAgentShare(asUser(3), 3); err != nil || !on {
		t.Fatalf("control: a person's stored true: on=%v err=%v, want on", on, err)
	}
}

package notifypref

import (
	"context"
	"errors"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
)

// TestPagesAgentShareIsOffUntilTurnedOnAndIsPerUserAndIndependent: "Let my
// agents share pages without asking" reads off for every user who never
// chose (and for a row another switch wrote); on is that user's alone, and
// leaves their other switches as they were; another user can neither read nor
// write it; the system scope reads it.
func TestPagesAgentShareIsOffUntilTurnedOnAndIsPerUserAndIndependent(t *testing.T) {
	p := pgStore(t)
	for _, uid := range []int64{authz.OperatorUserID, 2, 3} {
		if on, err := p.PagesAgentShare(asUser(uid), uid); err != nil || on {
			t.Fatalf("user %d by default: on=%v err=%v, want off", uid, on, err)
		}
	}
	if err := p.SetTmuxAddress(asUser(2), 2, true); err != nil {
		t.Fatal(err)
	}
	if on, _ := p.PagesAgentShare(asUser(2), 2); on {
		t.Fatal("a row written by the tmux switch reads the pages opt-in on")
	}
	if err := p.SetPagesAgentShare(asUser(2), 2, true); err != nil {
		t.Fatal(err)
	}
	if on, err := p.PagesAgentShare(asUser(2), 2); err != nil || !on {
		t.Fatalf("user 2 turned it on: on=%v err=%v", on, err)
	}
	if on, _ := p.TmuxAddress(asUser(2), 2); !on {
		t.Fatal("the pages opt-in turned the tmux switch off")
	}
	if on, _ := p.PagesAgentShare(asUser(3), 3); on {
		t.Fatal("user 2's opt-in reached user 3")
	}
	if on, _ := p.PagesAgentShare(asUser(authz.OperatorUserID), authz.OperatorUserID); on {
		t.Fatal("user 2's opt-in reached the operator")
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
	if on, _ := p.PagesAgentShare(asUser(2), 2); !on {
		t.Fatal("user 3's write changed user 2's opt-in")
	}
}

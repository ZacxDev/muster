package api

import (
	"context"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ccpool"
)

// selectingKinds is stubKinds with SelectClaudeAccount answered by a REAL account
// pool over the real Postgres store, so the choice reads the same live counts
// (ccpool.PGStore.LiveCounts over the agents table) production does.
type selectingKinds struct {
	stubKinds
	pool *ccpool.Pool
}

func (k selectingKinds) SelectClaudeAccount(ctx context.Context, owner int64, pin string) (string, error) {
	return k.pool.Select(ctx, owner, pin)
}

// TestAStoppedCreateDoesNotSteerTheNextAccountChoice: a create that does not
// start the agent ("save for later") is stored `stopped` in the INSERT, so the
// account pool never counts it live — not even in the window before the
// background dispatch runs. Inserted as `provisioning` (as it was), that
// window counted it against its account, and a create made right after it was
// steered to a different account by Choose's fewest-live rank.
//
// The provisioner here records the dispatch and writes nothing, which holds
// that window open for the whole test: the result must not depend on whether
// the background step has run yet.
func TestAStoppedCreateDoesNotSteerTheNextAccountChoice(t *testing.T) {
	const alpha, beta = "stopped-create-alpha", "stopped-create-beta"
	src, err := ccpool.NewStatic(map[string]string{alpha: "tok-alpha", beta: "tok-beta"})
	if err != nil {
		t.Fatal(err)
	}
	var kinds selectingKinds
	f := newTaskRouteFixture(t, false, false, func(ext *Extensions) { ext.Kinds = &kinds })
	kinds.pool, err = ccpool.New(src, ccpool.NewPG(f.pool))
	if err != nil {
		t.Fatal(err)
	}

	// Control: with nothing on either account the name tiebreak picks alpha.
	if got, err := kinds.pool.Select(f.asOwner, f.owner, ""); err != nil || got != alpha {
		t.Fatalf("control: the first choice is %q (%v); want %q", got, err, alpha)
	}

	saved := func(text string) agents.Agent {
		t.Helper()
		rec, err := f.srv.createAndDispatchAgent(f.asOwner, dispatchParams{
			Kind: agents.KindClaudeCode, NoteText: text, Kickoff: false, TrackOutcome: true,
		})
		if err != nil {
			t.Fatalf("stopped create %q: %v", text, err)
		}
		return rec
	}
	first := saved("first saved agent")
	if first.CCAccount != alpha {
		t.Fatalf("the first stopped create is on %q; want %q", first.CCAccount, alpha)
	}
	if first.Status != agents.StatusStopped {
		t.Errorf("the stopped create was inserted as %q; want %q", first.Status, agents.StatusStopped)
	}
	got, err := agents.NewPG(f.pool).Get(f.asOwner, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != agents.StatusStopped {
		t.Errorf("the stored row reads %q before the background dispatch wrote anything; want %q", got.Status, agents.StatusStopped)
	}

	if got, err := kinds.pool.Select(f.asOwner, f.owner, ""); err != nil || got != alpha {
		t.Errorf("the choice right after a stopped create on %s is %q (%v); want %q (no live agents, name tiebreak)",
			alpha, got, err, alpha)
	}
	second := saved("second saved agent")
	if second.CCAccount != alpha {
		t.Errorf("the second stopped create is on %q; want %q: the first, saved, agent steered it", second.CCAccount, alpha)
	}
}

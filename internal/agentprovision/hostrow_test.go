package agentprovision

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// TestAHostRowIsNeverProvisioned: a host officer instance's row
// (agents.Agent.RunsOnHost, docs/design/officers-on-hosts.md) is refused by
// EVERY lifecycle method before any driver call — Dispatch, Start, StartIfLive,
// Stop, Destroy, ReapplyProfiles — with agents.ErrHostRow, and nothing is
// written to its row. The control: the same row with RunsOnHost cleared
// reaches the driver.
func TestAHostRowIsNeverProvisioned(t *testing.T) {
	ctx := context.Background()
	ops := map[string]func(*Adapter) error{
		"dispatch": func(a *Adapter) error { return a.Dispatch(ctx, fixtureAgentID, true) },
		"start":    func(a *Adapter) error { return a.Start(ctx, fixtureAgentID) },
		"start-if-live": func(a *Adapter) error {
			_, err := a.StartIfLive(ctx, fixtureAgentID, func() (bool, error) { return true, nil })
			return err
		},
		"stop":    func(a *Adapter) error { return a.Stop(ctx, fixtureAgentID) },
		"destroy": func(a *Adapter) error { return a.Destroy(ctx, fixtureAgentID) },
		"reapply": func(a *Adapter) error { return a.ReapplyProfiles(ctx, fixtureAgentID) },
	}
	for name, op := range ops {
		a, store, driver := newAdapter(t, func(s *recordingStore, _ *flakyDriver) {
			s.agent.RunsOnHost, s.agent.Namespace = "host-alpha", ""
		})
		err := op(a)
		if !errors.Is(err, agents.ErrHostRow) {
			t.Errorf("%s on a host row: %v, want agents.ErrHostRow", name, err)
		}
		if driver.creates+driver.updates+driver.destroys+len(driver.scaled) != 0 {
			t.Errorf("%s on a host row reached the driver (creates %d, updates %d, destroys %d, scales %v)", name, driver.creates, driver.updates, driver.destroys, driver.scaled)
		}
		for _, c := range store.calls {
			if !strings.HasPrefix(c, "Get(") {
				t.Errorf("%s on a host row wrote %q", name, c)
			}
		}
	}
	// Control: the refusal is the row's, not the operation's.
	a, _, driver := newAdapter(t, nil)
	if err := a.Stop(ctx, fixtureAgentID); errors.Is(err, agents.ErrHostRow) || len(driver.scaled) == 0 {
		t.Fatalf("control: a provisioned row's stop = %v, scales %v; want it to reach the driver", err, driver.scaled)
	}
}

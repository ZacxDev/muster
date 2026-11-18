package agentprovision

import (
	"context"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// TestStopAndDestroyRunTheBeforeStopHookWhileTheInstanceIsUp: the hook
// installed by UseBeforeStop (the consumer usage collector's pre-stop push) is
// handed the agent's row BEFORE the driver scales it to zero or destroys it —
// the pod's ccd can still answer then — on every Stop and Destroy, whoever
// calls them.
func TestStopAndDestroyRunTheBeforeStopHookWhileTheInstanceIsUp(t *testing.T) {
	for _, op := range []string{"stop", "destroy"} {
		t.Run(op, func(t *testing.T) {
			a, store, driver := newAdapter(t, nil)
			seedInstance(t, a, store, driver)
			var seen []string
			a.UseBeforeStop(func(_ context.Context, ag agents.Agent) {
				seen = append(seen, ag.Name)
				if len(driver.scaled) != 0 || driver.destroys != 0 {
					t.Errorf("the hook ran after the driver (scaled %v, destroys %d)", driver.scaled, driver.destroys)
				}
			})
			var err error
			if op == "stop" {
				err = a.Stop(context.Background(), fixtureAgentID)
			} else {
				err = a.Destroy(context.Background(), fixtureAgentID)
			}
			if err != nil {
				t.Fatalf("%s: %v", op, err)
			}
			if len(seen) != 1 || seen[0] != store.agent.Name {
				t.Fatalf("hook saw %v, want the agent %q once", seen, store.agent.Name)
			}
			if op == "stop" && (len(driver.scaled) != 1 || driver.scaled[0] != 0) {
				t.Fatalf("Stop scaled %v, want [0]", driver.scaled)
			}
			if op == "destroy" && driver.destroys != 1 {
				t.Fatalf("Destroy destroyed %d times, want 1", driver.destroys)
			}
		})
	}
}

// TestNoBeforeStopHookIsNoChange: without the hook Stop still stops (the
// officers' muster and every muster without consumer usage).
func TestNoBeforeStopHookIsNoChange(t *testing.T) {
	a, store, driver := newAdapter(t, nil)
	seedInstance(t, a, store, driver)
	if err := a.Stop(context.Background(), fixtureAgentID); err != nil || len(driver.scaled) != 1 || driver.scaled[0] != 0 {
		t.Fatalf("Stop: %v, scaled %v", err, driver.scaled)
	}
}

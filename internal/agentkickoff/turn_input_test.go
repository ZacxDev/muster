package agentkickoff

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// TestTheTurnContextIsHandedTheFirstTurnsPromptRow: the prompt row the
// deliverer stores BEFORE the send is on the context Config.TurnContext is
// handed (agents.TurnInputRow), so the hook can record the running turn's own
// input (internal/api registerOfficerTurn: the moment an officer's item points
// at). The fake store numbers rows from 1: the deliverer stores the prompt
// (row 1) before the send and the reply (row 2) after it, so the hook sees 1.
func TestTheTurnContextIsHandedTheFirstTurnsPromptRow(t *testing.T) {
	row := ownedRow()
	h := newHarness(t, []provision.Instance{readyInstance("lively-newt", "lively-newt-7f9c-x2", 2)}, row)
	var got int64 = -1
	d, err := New(Config{
		Store: h.store, Instances: h.insts, Gateway: h.gw,
		NamespacePrefix: testPrefix, Owner: ownerID, Now: func() time.Time { return fixedNow },
		TurnContext: func(ctx context.Context, a agents.Agent) context.Context {
			got = agents.TurnInputRow(ctx)
			return ctx
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.d = d
	log := h.tick(t)
	if h.gw.callCount() != 1 {
		t.Fatalf("the gateway was called %d time(s), want one first turn.%s", h.gw.callCount(), transcript(log))
	}
	if got != 1 || h.store.nextMsg != 2 {
		t.Fatalf("TurnContext was handed prompt row %d (rows stored: %d); want row 1, the prompt, before the reply's row 2", got, h.store.nextMsg)
	}
}

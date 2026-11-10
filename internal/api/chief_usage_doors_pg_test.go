package api

import (
	"context"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefusage"
)

// chiefTurnTriggers is every chief_turn_usage trigger recorded for chief, oldest first.
func chiefTurnTriggers(t *testing.T, f *chiefAskFixture) []string {
	t.Helper()
	rows, err := f.pool.Query(authz.System(context.Background()), `SELECT trigger FROM chief_turn_usage WHERE chief_agent = $1 ORDER BY id`, f.chief.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func usageDoorsFixture(t *testing.T) *chiefAskFixture {
	t.Helper()
	f := newChiefAskFixture(t)
	f.srv.ext.ChiefUsage = chiefusage.NewPG(f.pool)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM chief_turn_usage WHERE chief_agent = $1`, f.chief.ID)
	})
	return f
}

// TestASessionsQuestionTurnIsRecordedAsOne: the chief's turn for a mirrored
// session's chief_ask is recorded with its own trigger, never as an owner's
// answer (the delivery path both share).
func TestASessionsQuestionTurnIsRecordedAsOne(t *testing.T) {
	f := usageDoorsFixture(t)
	id, _ := f.chiefAsk(t, focusSID, map[string]any{"question": "Which branch should I base this on?"})
	f.turnFor(t, f.def, "session question #")
	eventuallyAPI(t, "the question turn's usage row", func() bool { return len(chiefTurnTriggers(t, f)) > 0 })
	if got := chiefTurnTriggers(t, f); len(got) != 1 || got[0] != chiefusage.TriggerSessionQuestion {
		t.Fatalf("question %d's turn recorded as %v; want [%s]", id, got, chiefusage.TriggerSessionQuestion)
	}
}

// TestTheMachineDoorsTurnIsRecorded: a chief turn through the machine message
// door (gatewayTurn, as POST /api/agents/{name}/messages runs it) writes its
// usage row, tagged with the door.
func TestTheMachineDoorsTurnIsRecorded(t *testing.T) {
	f := usageDoorsFixture(t)
	if _, err := f.srv.gatewayTurn(asUser(1), f.chief, f.def.SessionKey, "status of the build?", authz.ViaAgentMessage); err != nil {
		t.Fatal(err)
	}
	if got := chiefTurnTriggers(t, f); len(got) != 1 || got[0] != chiefusage.TriggerMachine {
		t.Fatalf("the machine door's turn recorded as %v; want [%s]", got, chiefusage.TriggerMachine)
	}
}

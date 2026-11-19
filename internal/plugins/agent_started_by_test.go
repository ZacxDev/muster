package plugins

import (
	"testing"

	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestAgentStartedByReadsTheReservation: the person a chief start names
// (requested_by_user) is the one AgentStartedBy answers for the agent that
// start recorded — the NEWEST start on the task when it was started twice,
// never an earlier one's person — and 0 for an agent no start recorded and
// for a start that named nobody. Another owner reads none of it. The people
// (31, 32) and agents (980031…) are distinct numbers.
func TestAgentStartedByReadsTheReservation(t *testing.T) {
	s, _, dsn := pgStore(t)
	dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE action LIKE 'task.chief_%'`)
	const task, first, second, nobody = 731, 980031, 980032, 980033
	start := func(agent int64, detail map[string]any) {
		t.Helper()
		if _, err := s.ReserveChiefDispatch(session(2), NoteObject(task), detail, nil); err != nil {
			t.Fatal(err)
		}
		if err := s.RecordChiefDispatchAgent(session(2), NoteObject(task), agent); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReleaseChiefDispatch(session(2), task, agent); err != nil {
			t.Fatal(err)
		}
	}
	start(first, map[string]any{detailRequestedByUser: int64(31)})
	start(second, map[string]any{detailRequestedByUser: int64(32)})
	start(nobody, map[string]any{})
	for agent, want := range map[int64]int64{first: 31, second: 32, nobody: 0, 980099: 0} {
		got, err := s.AgentStartedBy(session(2), agent)
		if err != nil || got != want {
			t.Errorf("AgentStartedBy(%d) = %d, %v; want %d", agent, got, err, want)
		}
	}
	if got, err := s.AgentStartedBy(session(3), second); err != nil || got != 0 {
		t.Errorf("another owner's AgentStartedBy = %d, %v; want 0 (row-level security)", got, err)
	}
}

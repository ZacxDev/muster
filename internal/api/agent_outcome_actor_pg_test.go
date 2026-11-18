package api

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agentoutcome"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/notes"
)

// TestOnlyTheTasksOwnAgentFinishesItByMovingIt (round 2, 🟡2): task_set_status
// is the chief's (every task) and a mirrored session's too. The chief or a
// mirrored session moving an agent's task to review is NOT that agent reporting
// back: its outcome stays running, nothing is pushed for it, and its next
// question is still "waiting for you". The same tool called by the task's OWN
// agent is the done signal (positive control).
//
// 🔴 RED AT 4cca02e3: the chief's move set the agent done (credited to it, no
// push), and the agent's later question was swallowed (done is not waiting).
func TestOnlyTheTasksOwnAgentFinishesItByMovingIt(t *testing.T) {
	o := newOutcomeFixture(t, stubModelCatalog{})
	toReview := func(id int64) string { return fmt.Sprintf(`{"id":%d,"status":%q}`, id, notes.StatusReadyForReview) }

	// The chief moves the agent's task.
	a := o.dispatchGateway(t, "the chief moves it", "openrouter/acme/acme-v4.1-pro")
	chief := o.agent(t, o.chief.ID)
	out := o.srv.toolTaskSetStatus(o.asOwner, toolCaller{agent: &chief, asker: attention.Asker{AgentID: chief.ID}}, toReview(*a.NoteID))
	if strings.Contains(out, `"error"`) {
		t.Fatalf("instrument check: the chief's task_set_status was refused: %s", out)
	}
	if n, err := o.srv.ext.Notes.Get(o.asOwner, *a.NoteID); err != nil || n.Status != notes.StatusReadyForReview {
		t.Fatalf("instrument check: the task did not move (%v, %q)", err, n.Status)
	}
	if got := o.agent(t, a.ID); got.Outcome != agentoutcome.Running {
		t.Fatalf("the chief moving the agent's task: outcome %q; want still running", got.Outcome)
	}
	if n := len(o.outcomePushes(a.ID)); n != 0 {
		t.Fatalf("the chief's move pushed %d agent outcomes; want 0", n)
	}
	o.srv.outcomeAnswered(o.asOwner, o.agent(t, a.ID), "Should I also update the changelog?")
	o.tellWaits(t)
	if got := o.agent(t, a.ID); got.Outcome != agentoutcome.Waiting {
		t.Fatalf("its next question: outcome %q; want waiting (not swallowed)", got.Outcome)
	}
	if n := len(o.outcomePushes(a.ID)); n != 1 {
		t.Fatalf("its next question pushed %d times; want 1", n)
	}

	// A mirrored session moves another agent's task (its writer's own origin).
	m := o.dispatchGateway(t, "a session moves it", "openrouter/acme/acme-v4.1-pro")
	mirror := toolCaller{asker: attention.Asker{MirrorHost: "ws", MirrorSession: "sess-7"}}
	if _, err := o.srv.setNoteStatus(notes.ForAgent(withRefOrigin(o.asOwner, mirror.refOrigin())), writerTaskTool, *m.NoteID, notes.StatusReadyForReview); err != nil {
		t.Fatal(err)
	}
	if got := o.agent(t, m.ID); got.Outcome != agentoutcome.Running {
		t.Fatalf("a mirrored session moving the agent's task: outcome %q; want still running", got.Outcome)
	}

	// Positive control: the task's OWN agent, through the same tool.
	own := o.dispatchGateway(t, "moves its own", "openrouter/acme/acme-v4.1-pro")
	self := o.agent(t, own.ID)
	out = o.srv.toolTaskSetStatus(o.asOwner, toolCaller{agent: &self, asker: attention.Asker{AgentID: self.ID}}, toReview(*own.NoteID))
	if strings.Contains(out, `"error"`) {
		t.Fatalf("instrument check: the agent's own task_set_status was refused: %s", out)
	}
	if got := o.agent(t, own.ID); got.Outcome != agentoutcome.Done {
		t.Fatalf("the agent moving its own task: outcome %q; want done", got.Outcome)
	}
}

package api

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/notes"
)

// The two pg doors of task_kickoff.go's rule (see task_kickoff_sites_test.go
// for the fixture and the assertion).

// pluginTask files the synthetic plugin task as plugin_tasks.go records one.
func (f *taskRouteFixture) pluginTask(t *testing.T) notes.Note {
	t.Helper()
	n, err := f.notes.Create(f.asOwner, notes.Note{Title: kickoffPluginTitle, Body: kickoffPluginBody,
		CreatedBy: notes.CreatedByPlugin("tracker")})
	if err != nil {
		t.Fatal(err)
	}
	if n.CreatedBy == nil || *n.CreatedBy != "plugin:tracker" {
		t.Fatalf("fixture: the task records created_by %v, want plugin:tracker", n.CreatedBy)
	}
	return n
}

// ownerTask files an owner's instruction-shaped task.
func (f *taskRouteFixture) ownerTask(t *testing.T) notes.Note {
	t.Helper()
	n, err := f.notes.Create(f.asOwner, notes.Note{Title: "export job", Body: kickoffOwnerBody, CreatedBy: strPtr(notes.CreatedByOwner)})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestTheChiefsAgentDispatchFencesAPluginTask: agent_dispatch
// (agent_dispatch.go) on a plugin task hands the agent the task inside the
// fence, the rules last; on an owner's task the first message is the body and
// the rules, byte for byte.
func TestTheChiefsAgentDispatchFencesAPluginTask(t *testing.T) {
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a"))
	n := f.pluginTask(t)
	got := f.startedOne(t, f.callRaw(f.chief, agentDispatchTool, map[string]any{"target": "pod/claude-code", "task_id": n.ID}), n.ID)
	assertPluginTextOnlyInsideTheFence(t, "agent_dispatch", got.PendingNote)
	if !strings.HasSuffix(got.PendingNote, chiefDispatchRules) {
		t.Errorf("agent_dispatch: the rules are not last:\n%s", got.PendingNote)
	}
	o := f.ownerTask(t)
	own := f.startedOne(t, f.callRaw(f.chief, agentDispatchTool, map[string]any{"target": "pod/claude-code", "task_id": o.ID}), o.ID)
	if own.PendingNote != kickoffOwnerBody+chiefDispatchRules {
		t.Errorf("agent_dispatch, an owner's task: first message %q\nwant the body and the rules, unchanged", own.PendingNote)
	}
}

// TestAnApprovedDispatchRequestFencesAPluginTask: dispatch_approve
// (dispatch_hold_tools.go) on a plugin task hands the agent the task inside
// the fence after the session's relayed prompt, the rules last; an owner's
// task follows the relay unchanged.
func TestAnApprovedDispatchRequestFencesAPluginTask(t *testing.T) {
	h := newHoldFixture(t)
	for _, plugin := range []bool{true, false} {
		var n notes.Note
		if plugin {
			n = h.pluginTask(t)
		} else {
			n = h.ownerTask(t)
		}
		held := h.request(t, map[string]any{"target": "pod/claude-code", "task_id": n.ID, "prompt": "Start with the slow query."})
		text, isErr, _ := h.tool(t, h.ws, focusSID, dispatchApproveTool, map[string]any{"hold_id": held.HoldID})
		if isErr {
			t.Fatalf("approval: %s", text)
		}
		msg := h.startedOne(t, text, n.ID).PendingNote
		if !strings.HasSuffix(msg, sessionDispatchRules) {
			t.Errorf("dispatch_approve plugin=%v: the rules are not last:\n%s", plugin, msg)
		}
		if plugin {
			assertPluginTextOnlyInsideTheFence(t, "dispatch_approve", msg)
		} else if !strings.HasSuffix(msg, "\n"+kickoffOwnerBody+sessionDispatchRules) {
			t.Errorf("dispatch_approve, an owner's task: first message %q\nwant the relay, then the body and the rules unchanged", msg)
		}
	}
}

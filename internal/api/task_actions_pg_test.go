package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// TestTheActionsOfferedOnATaskAreExactlyTheToolsThatWouldAct pins the
// RELATIONSHIP, not a component: across every combination of who works the task
// (nobody, a running agent, a stopped one) and the owner's dispatch opt-in
// (dispatch off, on for someone else, on for this owner), the actions
// task_route_attention offers the owner are exactly the tools that then get
// past their gate when the chief calls them — task_forward forwards, and
// task_dispatch is not refused for the opt-in. route_to_session is never
// offered (a task event is no_reply). The ask's buttons are the offered
// actions, in order.
func TestTheActionsOfferedOnATaskAreExactlyTheToolsThatWouldAct(t *testing.T) {
	type tally struct{ offered, withheld int }
	seen := map[string]*tally{taskForwardTool: {}, taskDispatchTool: {}}
	for _, dc := range []struct {
		name            string
		dispatch, optIn bool
	}{{"dispatch-off", false, false}, {"dispatch-on-for-another", true, false}, {"dispatch-on-for-this-owner", true, true}} {
		for _, worker := range []string{"", agents.StatusRunning, agents.StatusStopped} {
			t.Run(dc.name+"/worker-"+firstNonBlank(worker, "none"), func(t *testing.T) {
				f := newTaskRouteFixture(t, dc.dispatch, dc.optIn)
				f.srv.taskForwardTurn = func(context.Context, agents.Agent, agents.ChatSession, string) {}
				n := f.task(t, "who takes it")
				if worker != "" {
					if _, err := agents.NewPG(f.pool).Create(f.asOwner, agents.Agent{Name: "tact-" + dc.name + "-" + worker,
						Namespace: "ns-tact", Status: worker, NoteID: &n.ID}); err != nil {
						t.Fatal(err)
					}
				}
				res := f.call(f.chief, taskRouteAttentionTool, map[string]any{"task_id": n.ID, "note": "Who should take it?"})
				var out struct {
					ID      int64    `json:"id"`
					Offered []string `json:"actions_offered"`
				}
				if err := json.Unmarshal([]byte(res), &out); err != nil || out.ID <= 0 || out.Offered == nil {
					t.Fatalf("task_route_attention answered %s", res)
				}
				ask, err := f.srv.ext.Attention.Get(f.asOwner, out.ID)
				if err != nil {
					t.Fatal(err)
				}
				if len(ask.Options) != len(out.Offered) {
					t.Fatalf("buttons %q for actions %q", ask.Options, out.Offered)
				}
				offered := map[string]bool{}
				for i, tool := range out.Offered {
					offered[tool] = true
					if !strings.HasPrefix(ask.Options[i], tool+": ") {
						t.Errorf("button %d %q does not name action %s", i, ask.Options[i], tool)
					}
				}
				if offered[routeToSessionToolName] {
					t.Error("route_to_session was offered for a task event, which it refuses (no_reply)")
				}
				fwd := f.call(f.chief, taskForwardTool, map[string]any{"task_id": n.ID, "note": "over to you"})
				forwarded := !strings.Contains(fwd, `"error"`)
				disp := f.call(f.chief, taskDispatchTool, map[string]any{"task_id": n.ID})
				dispatchAdmitted := !strings.Contains(disp, chiefDispatchOptOut) && !strings.Contains(disp, chiefDispatchOffText)
				if offered[taskForwardTool] != forwarded {
					t.Errorf("task_forward offered=%v, but the tool forwarded=%v: %s", offered[taskForwardTool], forwarded, fwd)
				}
				if offered[taskDispatchTool] != dispatchAdmitted {
					t.Errorf("task_dispatch offered=%v, but its opt-in gate admitted=%v: %s", offered[taskDispatchTool], dispatchAdmitted, disp)
				}
				for tool, tl := range seen {
					if offered[tool] {
						tl.offered++
					} else {
						tl.withheld++
					}
				}
				if len(out.Offered) == 0 && !strings.Contains(ask.Question, "it is yours to handle") {
					t.Errorf("an ask offering nothing does not say so: %q", ask.Question)
				}
			})
		}
	}
	// The comparison is not vacuous: each action was both offered and withheld.
	for tool, tl := range seen {
		if tl.offered == 0 || tl.withheld == 0 {
			t.Errorf("%s was offered %d and withheld %d times across the matrix: the comparison cannot see one side", tool, tl.offered, tl.withheld)
		}
	}
}

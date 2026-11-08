package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
)

// THE TASK READ TOOLS (officer_task_list, officer_task_get): a task-reconciler
// officer reads HQ tasks through HQ's officer task route with its own token;
// no other officer is offered them. Names spelled as literals on purpose.

const (
	officerTaskListTool = "officer_task_list"
	officerTaskGetTool  = "officer_task_get"
	offTR               = "task-reconciler:hq"
	offTRTok            = "fixture-officer-token-taskrec-51ab"
)

func newTaskRig(t *testing.T) *officerRig {
	t.Helper()
	return newOfficerRigWith(t, map[string]string{offTRTok: offTR, offATok: offA}, func(hqURL string) []officers.Officer {
		a := officers.Officer{ID: offTR, HQURL: hqURL, MaxInstances: 1, IdleAfter: 30 * time.Minute, PollInterval: time.Minute, Owner: 1}.WithToken(offTRTok)
		b := officers.Officer{ID: offA, HQURL: hqURL, MaxInstances: 1, IdleAfter: 30 * time.Minute, PollInterval: time.Minute, Owner: 1}.WithToken(offATok)
		return []officers.Officer{a, b}
	})
}

func TestTheTaskToolsAreOfferedToTheTaskReconcilerOnly(t *testing.T) {
	r := newTaskRig(t)
	ctx := context.Background()
	tr := r.addInstance(r.a, 1, 901)
	got := officerToolNameSet(r.s.MCPToolDefsFor(ctx, tr))
	sess, ok := r.s.OfficerClaudeCodeSession(tr)
	if !ok {
		t.Fatal("no officer session")
	}
	for _, n := range []string{officerTaskListTool, officerTaskGetTool} {
		if !got[n] {
			t.Errorf("the task reconciler is not offered %s over MCP", n)
		}
		allowed := false
		for _, m := range sess.AllowMCPTools {
			allowed = allowed || m == n
		}
		if !allowed {
			t.Errorf("the task reconciler's session does not allow %s", n)
		}
	}
	rel := r.addInstance(r.b, 1, 902)
	if got := officerToolNameSet(r.s.MCPToolDefsFor(ctx, rel)); got[officerTaskListTool] || got[officerTaskGetTool] {
		t.Error("the release officer is offered a task tool")
	}
	for _, a := range []struct {
		name string
		id   int64
	}{{"release officer", 902}, {"ordinary agent", officerOrdinaryID}, {"chief", officerChiefID}} {
		r.hq.ResetRequests()
		out := r.s.dispatchAgentTool(ctx, r.agent(t, a.id), officerTaskGetTool, `{"task_id":"fixture-task-1"}`)
		if !isToolErr(out) {
			t.Errorf("%s calling %s: %s, want a refusal", a.name, officerTaskGetTool, out)
		}
		if n := len(r.hq.Requests()); n != 0 {
			t.Errorf("%s's refused task read reached HQ (%d requests)", a.name, n)
		}
	}
}

func TestTaskToolsReadHQWithTheOfficersOwnToken(t *testing.T) {
	r := newTaskRig(t)
	ctx := context.Background()
	a := r.addInstance(r.a, 1, 911)
	r.hq.ResetRequests()
	out := r.s.dispatchAgentTool(ctx, a, officerTaskListTool, `{"open":true,"limit":50}`)
	if isToolErr(out) || !strings.Contains(out, `"fixture-task-1"`) || !strings.Contains(out, `"children_done":2`) {
		t.Fatalf("list: %s", out)
	}
	out = r.s.dispatchAgentTool(ctx, a, officerTaskGetTool, `{"task_id":"868mgz5ap"}`)
	if isToolErr(out) || !strings.Contains(out, `"done_statuses":["complete"]`) {
		t.Fatalf("get: %s", out)
	}
	reqs := r.hq.Requests()
	if len(reqs) != 2 {
		t.Fatalf("HQ saw %d requests, want 2: %+v", len(reqs), reqs)
	}
	if q := reqs[0]; q.Method != "GET" || q.Path != "/api/v1/officers/"+offTR+"/tasks" || q.Query != "limit=50&open=true" || q.Auth != "Bearer "+offTRTok {
		t.Errorf("list went %s %s ?%s with %q", q.Method, q.Path, q.Query, q.Auth)
	}
	if q := reqs[1]; q.Method != "GET" || q.Path != "/api/v1/officers/"+offTR+"/tasks/868mgz5ap" || q.Auth != "Bearer "+offTRTok {
		t.Errorf("get went %s %s with %q", q.Method, q.Path, q.Auth)
	}
}

func TestTaskToolsRefuseLocallyWhatMusterOwns(t *testing.T) {
	r := newTaskRig(t)
	ctx := context.Background()
	a := r.addInstance(r.a, 1, 921)
	for _, c := range []struct{ tool, args string }{
		{officerTaskGetTool, `{"task_id":"../items"}`},
		{officerTaskGetTool, `{}`},
		{officerTaskListTool, `{"assignee":"me"}`},
		{officerTaskListTool, `{"limit":"5"}`},
		{officerTaskListTool, `{"officer_id":"release:alpha-web"}`},
	} {
		r.hq.ResetRequests()
		out := r.s.dispatchAgentTool(ctx, a, c.tool, c.args)
		if !isToolErr(out) {
			t.Errorf("%s %s: %s, want a refusal", c.tool, c.args, out)
		}
		if n := len(r.hq.Requests()); n != 0 {
			t.Errorf("%s %s reached HQ (%d requests)", c.tool, c.args, n)
		}
	}
}

func TestTaskToolsPassHQsFinalRefusalsBack(t *testing.T) {
	r := newTaskRig(t)
	ctx := context.Background()
	a := r.addInstance(r.a, 1, 931)
	out := r.s.dispatchAgentTool(ctx, a, officerTaskGetTool, `{"task_id":"private-1"}`)
	if st, code := obsRefusal(out); st != 404 || code != "not_found" {
		t.Errorf("private task: %s, want refused 404 not_found", out)
	}
	r.hq.TaskReadOff = true
	for _, c := range []struct{ tool, args string }{{officerTaskListTool, `{}`}, {officerTaskGetTool, `{"task_id":"x"}`}} {
		out := r.s.dispatchAgentTool(ctx, a, c.tool, c.args)
		if st, code := obsRefusal(out); st != 403 || code != "task_read_off" || strings.Contains(out, "retryable") {
			t.Errorf("%s with task_read off: %s, want a final refused 403 task_read_off", c.tool, out)
		}
	}
}

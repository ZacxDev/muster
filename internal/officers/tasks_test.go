package officers

import (
	"strings"
	"testing"
)

// The task read tools' argument rules (tasks.go). Tool names and the type are
// spelled as literals: they are the contract the charter names.

func TestTaskToolsAreOfferedToTheTaskReconcilerTypeOnly(t *testing.T) {
	for _, c := range []struct {
		id   string
		want bool
	}{
		{"task-reconciler:hq", true},
		{"task-reconciler:other", true},
		{"release:alpha-web", false},
		{"engineering:hq", false},
		{"task-reconciler-x:hq", false},
		{"x-task-reconciler:hq", false},
	} {
		for _, tool := range []string{"officer_task_list", "officer_task_get"} {
			if got := ToolOffered(Officer{ID: c.id}, tool); got != c.want {
				t.Errorf("ToolOffered(%s, %s) = %v, want %v", c.id, tool, got, c.want)
			}
			// a tiered pool of the same officer is the same type
			if got := ToolOffered(Officer{ID: c.id, Tier: TierRoutine}, tool); got != c.want {
				t.Errorf("ToolOffered(%s routine, %s) = %v, want %v", c.id, tool, got, c.want)
			}
		}
	}
}

// TestTaskReadsSpendNoReadBudget: the task tools are not counted reads.
func TestTaskReadsSpendNoReadBudget(t *testing.T) {
	o := Officer{ID: "task-reconciler:hq", Tier: TierJudgement}
	for _, tool := range []string{"officer_task_list", "officer_task_get"} {
		if CountsRead(o, tool) {
			t.Errorf("CountsRead(%s) = true, want false", tool)
		}
	}
}

func TestTaskListQueryBuildsExactlyTheGivenFilters(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{``, ``},
		{`{}`, ``},
		{`{"open":true}`, `?open=true`},
		{`{"open":false}`, `?open=false`},
		{`{"status":"to do,paused","limit":50}`, `?limit=50&status=to+do%2Cpaused`},
		{`{"since":"2026-11-01T00:00:00Z","cursor":"Mg"}`, `?cursor=Mg&since=2026-11-01T00%3A00%3A00Z`},
		{`{"status":"a&open=true"}`, `?status=a%26open%3Dtrue`},
		{`{"limit":7,"open":null}`, `?limit=7`},
		{`{"cursor":""}`, ``},
	} {
		got, err := TaskListQuery([]byte(c.in))
		if err != nil || got != c.want {
			t.Errorf("TaskListQuery(%s) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func TestTaskListQueryRefusesWhatItCannotCarry(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`[]`, "one JSON object"},
		{`{"assignee":"me"}`, `"assignee" is not an argument`},
		{`{"Open":true}`, `"Open" is not an argument`},
		{`{"open":"true"}`, "open must be true or false"},
		{`{"limit":"5"}`, "limit must be an integer"},
		{`{"limit":5.5}`, "limit must be an integer"},
		{`{"status":["a"]}`, "status must be a string"},
		{`{"since":17}`, "since must be a string"},
	} {
		got, err := TaskListQuery([]byte(c.in))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("TaskListQuery(%s) = %q, %v; want an error with %q", c.in, got, err, c.want)
		}
	}
}

func TestTaskGetIDIsAnHQTaskIDOnly(t *testing.T) {
	for _, ok := range []string{"868aaaa01", "fixture_task_ab12", "fixture-task-with-dashes-cd34", strings.Repeat("a", 64)} {
		if got, err := TaskGetID([]byte(`{"task_id":"` + ok + `"}`)); err != nil || got != ok {
			t.Errorf("TaskGetID(%s) = %q, %v", ok, got, err)
		}
	}
	for _, bad := range []string{`{"task_id":"../items"}`, `{"task_id":"a/b"}`, `{"task_id":"a?x=1"}`, `{"task_id":""}`,
		`{"task_id":"` + strings.Repeat("a", 65) + `"}`, `{"task_id":7}`, `{}`, `{"task_id":"a","x":1}`, `{"task_id":"a b"}`, `{"task_id":"a%2F"}`} {
		if got, err := TaskGetID([]byte(bad)); err == nil {
			t.Errorf("TaskGetID(%s) = %q, want a refusal", bad, got)
		}
	}
}

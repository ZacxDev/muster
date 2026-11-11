package officers

import (
	"reflect"
	"strings"
	"testing"
)

// The gap task tool's offer rule and argument decoding (gap_tasks.go).
// Names and types are literals: they are the contract the charter names.

// TestGapTaskToolIsTheDatastoreOfficersOnly: officer_gap_task to a datastore
// officer's BOTH pools (and an untiered one) — never another type, nor a type
// whose name merely contains "datastore".
func TestGapTaskToolIsTheDatastoreOfficersOnly(t *testing.T) {
	for _, c := range []struct {
		id, tier string
		want     bool
	}{
		{"datastore:east", TierRoutine, true},
		{"datastore:east", TierJudgement, true},
		{"datastore:other", "", true},
		{"task-reconciler:hq", TierRoutine, false},
		{"cluster-anomaly:east", TierJudgement, false},
		{"web-performance:alpha-web", TierJudgement, false},
		{"datastore-x:east", TierRoutine, false},
		{"x-datastore:east", TierRoutine, false},
	} {
		if got := ToolOffered(Officer{ID: c.id, Tier: c.tier}, "officer_gap_task"); got != c.want {
			t.Errorf("ToolOffered(%s %q, officer_gap_task) = %v, want %v", c.id, c.tier, got, c.want)
		}
	}
}

func TestParseGapTaskArgs(t *testing.T) {
	a, err := ParseGapTaskArgs([]byte(`{"item_id":42,"store":"@fleet-x","kind":"no_backup","namespace":null,"tier":"tool","affected":["push-svc","git-host"]}`))
	want := GapTaskArgs{Item: "42", Store: "@fleet-x", Kind: "no_backup", Tier: "tool", Affected: []string{"push-svc", "git-host"}}
	if err != nil || !reflect.DeepEqual(a, want) {
		t.Errorf("full: %+v %v, want %+v", a, err, want)
	}
	if a, err := ParseGapTaskArgs([]byte(`{"item_id":"7","store":"vector-store","kind":"no_metrics"}`)); err != nil || a.Item != "7" || a.Namespace != "" || a.Affected != nil {
		t.Errorf("minimal: %+v %v", a, err)
	}
	for _, c := range []struct{ raw, want string }{
		{`{"item_id":42,"kind":"no_backup"}`, "store is required"},
		{`{"item_id":42,"store":"","kind":"no_backup"}`, "store is required"},
		{`{"item_id":42,"store":"vector-store"}`, "kind is required"},
		{`{"item_id":42,"store":7,"kind":"no_backup"}`, "store must be a string"},
		{`{"item_id":42,"store":"vector-store","kind":"no_backup","tier":1}`, "tier must be a string"},
		{`{"item_id":42,"store":"@t","kind":"no_backup","affected":"push-svc"}`, "affected must be an array"},
		{`{"store":"vector-store","kind":"no_backup"}`, "item_id is required"},
		{`{"item_id":"4/2","store":"vector-store","kind":"no_backup"}`, "item_id is required"},
		{`{"item_id":42,"store":"vector-store","kind":"no_backup","title":"mine"}`, `"title" is not an argument of officer_gap_task`},
		{`{"item_id":42,"store":"vector-store","kind":"no_backup","assignee":"someone"}`, `"assignee" is not an argument of officer_gap_task`},
		{`["vector-store"]`, "arguments must be one JSON object"},
	} {
		if _, err := ParseGapTaskArgs([]byte(c.raw)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error naming %q", c.raw, err, c.want)
		}
	}
}

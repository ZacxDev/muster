package officers_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

func parseCI(t *testing.T, ship map[string]any, ci map[string]any) ([]officers.Officer, error) {
	t.Helper()
	decl := map[string]any{"id": shipID, "hq_url": "https://hq.example.com/api/v1", "repo": "example-org/example-web", "ci_logs": ci}
	if ship != nil {
		decl["ship"] = ship
	}
	raw, _ := json.Marshal([]any{decl})
	return officers.Parse(string(raw), env(shipEnv(t)), "muster-agent-")
}

func validCI() map[string]any {
	return map[string]any{"namespace": "example-ci", "pipeline": "example-pr-check", "service_account": "example-ci-reader"}
}

// TestParseReadsACILogsBlockWithItsDefaults, and offers officer_ci_logs only
// to the officer that has it, and tells it to call it before escalating.
func TestParseReadsACILogsBlockWithItsDefaults(t *testing.T) {
	list, err := parseCI(t, fullShip(), validCI())
	if err != nil {
		t.Fatal(err)
	}
	c := list[0].CILogs
	if c == nil || c.Namespace != "example-ci" || c.Pipeline != "example-pr-check" || c.CommitLabel != "pipeline.jquad.rocks/pr.branch.commit" ||
		c.PRParam != "PR_NUMBER" || c.ServiceAccount != "example-ci-reader" || c.MaxBytes != 16384 {
		t.Fatalf("parsed %+v", c)
	}
	if !officers.ToolOffered(list[0], officers.ToolCILogs) {
		t.Fatal("an officer with ci_logs is not offered officer_ci_logs")
	}
	if k := officers.KickoffText(list[0], 1); !strings.Contains(k, "call officer_ci_logs{pr} BEFORE you escalate") {
		t.Fatalf("the kickoff does not tell the officer to read CI logs before escalating:\n%s", k)
	}
	noCI, err := parseShip(t, fullShip(), shipEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	if officers.ToolOffered(noCI[0], officers.ToolCILogs) || strings.Contains(officers.KickoffText(noCI[0], 1), officers.ToolCILogs) {
		t.Fatal("a shipping officer WITHOUT ci_logs is offered or told about officer_ci_logs")
	}
}

// TestParseRefusesABadCILogsBlock: each required field, a bad value, and a
// ci_logs block without a ship block is a boot refusal naming the field.
func TestParseRefusesABadCILogsBlock(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(m map[string]any)
		ship bool
		want string
	}{
		{"no ship block", func(map[string]any) {}, false, "needs a ship block"},
		{"no namespace", func(m map[string]any) { delete(m, "namespace") }, true, "namespace"},
		{"bad namespace", func(m map[string]any) { m["namespace"] = "Not_A_NS" }, true, "namespace"},
		{"no pipeline", func(m map[string]any) { delete(m, "pipeline") }, true, "pipeline"},
		{"no service account", func(m map[string]any) { delete(m, "service_account") }, true, "service_account"},
		{"bad service account", func(m map[string]any) { m["service_account"] = "Not/AnSA" }, true, "service_account"},
		{"the retired token_file", func(m map[string]any) { m["token_file"] = "/var/run/secrets/x/token" }, true, "unknown field"},
		{"max_bytes too big", func(m map[string]any) { m["max_bytes"] = 1 << 20 }, true, "max_bytes"},
		{"max_bytes too small", func(m map[string]any) { m["max_bytes"] = 100 }, true, "max_bytes"},
		{"bad commit label", func(m map[string]any) { m["commit_label"] = "a b" }, true, "commit_label"},
		{"unknown field", func(m map[string]any) { m["verbs"] = []string{"delete"} }, true, "unknown field"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := validCI()
			c.edit(m)
			var ship map[string]any
			if c.ship {
				ship = fullShip()
			}
			_, err := parseCI(t, ship, m)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want a refusal naming %q", err, c.want)
			}
		})
	}
}

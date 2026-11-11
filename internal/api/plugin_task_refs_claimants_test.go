package api

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/notes"
)

// TestAReferencesClaimantsAreCutNeverRefused: `reference.claimants` is
// optional and cleaned like every other snapshot field — agent and name are
// one line, a claimant whose agent_session is missing or not a session id is
// dropped (a claim is a session's pickup), and at most maxRefClaimants are
// kept. Absent, the snapshot has none.
func TestAReferencesClaimantsAreCutNeverRefused(t *testing.T) {
	decode := func(raw string) pluginRefIn {
		t.Helper()
		var in pluginRefIn
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			t.Fatal(err)
		}
		return in
	}
	base := `"status":"open","updated_at":"2026-11-03T10:00:00Z"`
	if got := decode(`{` + base + `}`).snapshot("tracker:1").Claimants; got != nil {
		t.Fatalf("no claimants field: %+v, want none", got)
	}
	in := decode(`{` + base + `,"claimants":[
		{"agent":"ag_press01","agent_session":"5e55a1d0-0000-4000-8000-00000000c1a1"},
		{"agent":"ag_press\n02","agent_session":"5e55a1d0-0000-4000-8000-00000000c1a2","name":"Q.\nSmith"},
		{"agent":"ag_press03","agent_session":"not a session id!","name":"R. Doe"},
		{"agent":"ag_press04"},
		{"name":"   "}]}`)
	want := []notes.RefClaimant{
		{Agent: "ag_press01", AgentSession: "5e55a1d0-0000-4000-8000-00000000c1a1"},
		{Agent: "ag_press 02", AgentSession: "5e55a1d0-0000-4000-8000-00000000c1a2", Name: "Q. Smith"},
	}
	if got := in.snapshot("tracker:1").Claimants; !reflect.DeepEqual(got, want) {
		t.Fatalf("claimants: %+v, want %+v", got, want)
	}
	many := make([]string, 0, maxRefClaimants+5)
	for i := range maxRefClaimants + 5 {
		many = append(many, `{"agent":"ag_fitter`+strconv.Itoa(i)+`","agent_session":"s`+strconv.Itoa(i)+`"}`)
	}
	got := decode(`{` + base + `,"claimants":[` + strings.Join(many, ",") + `]}`).snapshot("tracker:1").Claimants
	if len(got) != maxRefClaimants || got[0].Agent != "ag_fitter0" || got[maxRefClaimants-1].Agent != "ag_fitter"+strconv.Itoa(maxRefClaimants-1) {
		t.Fatalf("%d claimants kept (first %+v); want the first %d", len(got), got[0], maxRefClaimants)
	}
}

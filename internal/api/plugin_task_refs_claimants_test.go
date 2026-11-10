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
// optional and cleaned like every other snapshot field — a kind other than
// person/agent and a session id that is not one are dropped, a nameless
// claimant is dropped, a name is one line, and at most maxRefClaimants are
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
		{"kind":"agent","name":"press-bot","session":"5e55a1d0-0000-4000-8000-00000000c1a1"},
		{"kind":"robot","name":"Q.\nSmith","session":"not a session id!"},
		{"kind":"person","name":"   "}]}`)
	want := []notes.RefClaimant{
		{Kind: "agent", Name: "press-bot", Session: "5e55a1d0-0000-4000-8000-00000000c1a1"},
		{Name: "Q. Smith"},
	}
	if got := in.snapshot("tracker:1").Claimants; !reflect.DeepEqual(got, want) {
		t.Fatalf("claimants: %+v, want %+v", got, want)
	}
	many := make([]string, 0, maxRefClaimants+5)
	for i := range maxRefClaimants + 5 {
		many = append(many, `{"name":"fitter-`+strconv.Itoa(i)+`"}`)
	}
	got := decode(`{` + base + `,"claimants":[` + strings.Join(many, ",") + `]}`).snapshot("tracker:1").Claimants
	if len(got) != maxRefClaimants || got[0].Name != "fitter-0" || got[maxRefClaimants-1].Name != "fitter-"+strconv.Itoa(maxRefClaimants-1) {
		t.Fatalf("%d claimants kept (first %+v); want the first %d", len(got), got[0], maxRefClaimants)
	}
}

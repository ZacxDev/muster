package officers_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestTheJudgementSessionCapIsConfiguredOnTheJudgementPoolOnly: the daily
// judgement-session cap is `tiers.judgement.max_sessions_per_day`, 1..100.
// Each refusal names the field: on the routine pool (bounded by the `llm`
// budget instead), out of range. Leaving it out is no cap. Asserted on the
// parsed pool's JSON so the test compiles on a tree without the field.
func TestTheJudgementSessionCapIsConfiguredOnTheJudgementPoolOnly(t *testing.T) {
	list, err := parseOfficer(t, `,"tiers":{"routine":{"model":"haiku","max_instances":3},"judgement":{"model":"opus","max_instances":2,"max_sessions_per_day":3}}`)
	if err != nil {
		t.Fatalf("a judgement cap of 3: %v", err)
	}
	b, _ := json.Marshal(list[0].Tiers)
	var got struct {
		Routine   map[string]any `json:"routine"`
		Judgement map[string]any `json:"judgement"`
	}
	_ = json.Unmarshal(b, &got)
	if v, _ := got.Judgement["max_sessions_per_day"].(float64); v != 3 {
		t.Fatalf("the judgement pool reads %s, want max_sessions_per_day 3", b)
	}
	if v, ok := got.Routine["max_sessions_per_day"].(float64); ok && v != 0 {
		t.Fatalf("the routine pool took the judgement cap: %s", b)
	}
	for _, tc := range []struct{ name, judgement, routine, want string }{
		{"on the routine pool", `{"model":"opus"}`, `{"model":"haiku","max_sessions_per_day":3}`, "tiers.routine: max_sessions_per_day"},
		{"over 100", `{"model":"opus","max_sessions_per_day":101}`, `{"model":"haiku"}`, "tiers.judgement: max_sessions_per_day"},
		{"negative", `{"model":"opus","max_sessions_per_day":-1}`, `{"model":"haiku"}`, "tiers.judgement: max_sessions_per_day"},
	} {
		_, err := parseOfficer(t, `,"tiers":{"routine":`+tc.routine+`,"judgement":`+tc.judgement+`}`)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want a refusal containing %q", tc.name, err, tc.want)
		}
	}
	// An untiered officer has no judgement pool to cap.
	if _, err := parseOfficer(t, `,"max_sessions_per_day":3`); err == nil {
		t.Error("an untiered officer took max_sessions_per_day")
	}
}

package builtin

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAMalformedPlanIsNeverHandedOverRaw: a draft.refused payload whose plan
// RefusedDraftCopy cannot read (no item id, or not an object) keeps none of
// its payload or excerpt in the officer's copy, with or without a body.
// CONTROL: a well-formed plan is fenced, its item id kept.
func TestAMalformedPlanIsNeverHandedOverRaw(t *testing.T) {
	const secret = "plan-text-9c41"
	for name, raw := range map[string]string{
		"no item id, no body":    `{"draft_id":7,"plan":{"kind":"k","payload":{"m":"` + secret + `"},"payload_excerpt":"` + secret + `"}}`,
		"no item id, with body":  `{"draft_id":7,"body":"hello","plan":{"kind":"k","payload":{"m":"` + secret + `"}}}`,
		"not an object, no body": `{"draft_id":7,"plan":"` + secret + `"}`,
		"an array, with body":    `{"draft_id":7,"body":"hello","plan":["` + secret + `"]}`,
	} {
		out := string(RefusedDraftCopy(json.RawMessage(raw)))
		if strings.Contains(out, secret) {
			t.Errorf("%s: the officer's copy still carries the plan's text: %s", name, out)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(out), &m); err != nil {
			t.Errorf("%s: not JSON: %v", name, err)
		}
	}
	out := string(RefusedDraftCopy(json.RawMessage(`{"draft_id":7,"plan":{"item_id":3,"kind":"k","payload":{"m":"` + secret + `"}}}`)))
	var m map[string]any
	_ = json.Unmarshal([]byte(out), &m)
	block, _ := m["refused_draft"].(string)
	pl, _ := m["plan"].(map[string]any)
	if !strings.Contains(block, secret) || pl["item_id"] != float64(3) || pl["payload"] != nil {
		t.Fatalf("control: a well-formed plan is not fenced as it should be: %s", out)
	}
}

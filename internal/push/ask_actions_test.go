package push

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAnAsksButtonsFollowItsChips: with the signed actions routed
// (AskActions), payload v2 offers an ask_answer per option and per suggestion
// carrying its exact text, the inline-reply Answer and Dismiss, each naming the
// ask; a chip the fit drops takes its button with it, and the worst case still
// fits with Answer and Dismiss. Without AskActions there are none (the control).
// RED at the PR 1 tip (no `actions`).
func TestAnAsksButtonsFollowItsChips(t *testing.T) {
	p := Payload{Type: "attention", Title: "chief is asking", Body: "Ship?", AskActions: true,
		Ask: &V2Ask{ID: 41, Options: []string{"ship it"}, Suggestions: []string{"wait for Monday"}}}
	b, err := nativeBody(p)
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Actions json.RawMessage `json:"actions"`
	}
	_ = json.Unmarshal(b, &v)
	want := `[{"id":"ask_answer","label":"ship it","target":"41","text":"ship it"},` +
		`{"id":"ask_answer","label":"wait for Monday","target":"41","text":"wait for Monday"},` +
		`{"id":"ask_answer","label":"Answer","target":"41","input":true},{"id":"ask_dismiss","label":"Dismiss","target":"41"}]`
	if string(v.Actions) != want {
		t.Fatalf("actions = %s\nwant %s", v.Actions, want)
	}
	lt := func(n int) string { return strings.Repeat("<", n) }
	chip := []string{lt(MaxNativeChip), lt(MaxNativeChip - 1), lt(MaxNativeChip - 2)}
	worst := Payload{Type: "attention", Title: lt(MaxNativeTitle), Body: lt(MaxNativeBody), AskActions: true,
		Ask: &V2Ask{ID: 123456789, Options: chip, Suggestions: chip}}
	b, err = nativeBody(worst)
	if err != nil {
		t.Fatalf("the worst case with buttons does not fit: %v", err)
	}
	var w PayloadV2
	_ = json.Unmarshal(b, &w)
	if w.Body != worst.Body || len(w.Actions) != 2 || !w.Actions[0].Input || w.Actions[1].ID != ActionAskDismiss {
		t.Fatalf("worst case: body kept=%v, actions %+v", w.Body == worst.Body, w.Actions)
	}
	p.AskActions = false
	if b, _ := nativeBody(p); strings.Contains(string(b), `"actions"`) {
		t.Fatalf("CONTROL: actions without AskActions: %s", b)
	}
}

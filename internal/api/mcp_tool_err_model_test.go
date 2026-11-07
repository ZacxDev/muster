package api

import (
	"testing"

	"github.com/ZacxDev/muster/internal/plugins"
)

// TestAModelRefusalWithSuggestionsIsAToolError: agent_dispatch's model check
// refuses an unknown model with its closest ids (dispatchModelRefusal adds
// model_suggestions). Over MCP — the claude-code chief's transport — the call
// must be flagged isError, or the chief reads the refusal as a success. The key
// set isToolErr accepts is the guard.
func TestAModelRefusalWithSuggestionsIsAToolError(t *testing.T) {
	text := dispatchModelRefusal(modelResolution{Refusal: `model "x" is not in the model catalogue (by id or by name)`,
		Suggestions: []string{"openrouter/vendor-a/coder-7"}}, &plugins.ChiefBudget{})
	if !isToolErr(text) {
		t.Fatalf("a model refusal is not a tool error: %s", text)
	}
	// Control: a refusal without suggestions was always one, and an answer
	// carrying a key outside the refusal set is still a success.
	if !isToolErr(dispatchModelRefusal(modelResolution{Refusal: "x"}, nil)) {
		t.Fatal("control: a model refusal with no suggestions is not a tool error")
	}
	if isToolErr(`{"error":"x","model_suggestions":[],"agent_id":1}`) {
		t.Fatal("control: an answer with a key outside the refusal set reads as an error")
	}
}

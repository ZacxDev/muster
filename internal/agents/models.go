package agents

// The model catalogue: which slug an agent is born with, what it falls back to
// at runtime, and what the dispatch form suggests.
//
// 🔴 A SLUG IS A ROUTE, NOT JUST A NAME, and that is why this lives beside
// modelauth.go rather than beside the code that renders a deployment. The
// provider an agent runtime authenticates against is read from the slug's
// PREFIX, so every constant here is also a claim about which credential the
// agent will need. ValidateModelCredential is the guard that holds the two
// together; TestOwnSlugsPass drives every slug in this file through it, so
// adding an uncredentialed suggestion here fails a test rather than shipping an
// agent that silently runs its whole life on its fallback.

// defaultModel is the slug an agent gets when neither the operator nor the
// deployment chose one. It is kept equal to the option flagged Default in
// ModelOptions below so the built-in default is the same slug everywhere; the
// two are compared by a test rather than by a comment.
const defaultModel = "openrouter/xiaomi/mimo-v2.5"

// defaultModelFallbacks is the ordered list the agent runtime tries when the
// PRIMARY model fails at run time — overloaded, rate-limited, errored.
//
// ⚠ IT IS NOT THE CONFIG PRECEDENCE CHAIN, and conflating the two is easy
// because both are "which model". The primary answers "which slug does this
// agent start on"; this answers "what does it try when that slug fails
// mid-run". They are separate settings with separate failure modes.
var defaultModelFallbacks = []string{"openrouter/deepseek/deepseek-v4-flash-0731"}

// ModelOption is one suggested model for the dispatch model selector. The field
// is typeable (a datalist), so these are suggestions only — any slug the
// configured provider routes may be entered. Default marks the option
// pre-filled into the form.
type ModelOption struct {
	Value   string
	Default bool
}

// ModelOptions are the suggested models surfaced in the dispatch form's model
// selector. Extend this slice to add more suggestions; the field stays typeable
// so unlisted slugs still work.
var ModelOptions = []ModelOption{
	{Value: "openrouter/xiaomi/mimo-v2.5", Default: true},
	{Value: "openrouter/deepseek/deepseek-v4-flash-0731"},
	{Value: "openrouter/auto"},
}

// DefaultModelOption returns the pre-selected model slug (empty if none flagged).
func DefaultModelOption() string { return defaultOptionOf(ModelOptions) }

// defaultOptionOf is DefaultModelOption's body, over an arbitrary list.
//
// 🔴 IT IS SPLIT OUT SO THE FLAG CAN BE TESTED AT ALL. ModelOptions happens to
// flag its FIRST entry, so any test driving DefaultModelOption() directly is
// satisfied by a body that ignores m.Default and returns ModelOptions[0].Value
// — a mutation sweep found exactly that survivor. Reaching the branch requires
// a list whose flagged entry is NOT first, which means a list the test supplies.
func defaultOptionOf(opts []ModelOption) string {
	for _, m := range opts {
		if m.Default {
			return m.Value
		}
	}
	return ""
}

// ResolveModel picks the slug an agent is provisioned with, in precedence
// order: the agent's own model, then the deployment-wide default, then the
// built-in.
//
// 🔴 THE PRECEDENCE IS THE WHOLE FUNCTION, AND IT IS WHY THIS IS NOT INLINED AT
// THE CALL SITE. Upstream the same three-way choice was open-coded inside the
// deployment renderer, so the only thing that could test it was a test that
// rendered a whole deployment and dug the slug back out of a nested map —
// coverage that disappears the moment the renderer changes shape. It is a pure
// choice over two strings; it belongs where it can be asserted as one.
//
// ⚠ EMPTY MEANS UNSET AT BOTH LEVELS. An operator cannot express "no model" by
// clearing the field; that is what makes the fall-through total, and it is why
// the built-in must itself be a slug the agent can authenticate (see
// TestOwnSlugsPass).
func ResolveModel(agentModel, deploymentDefault string) string {
	if agentModel != "" {
		return agentModel
	}
	if deploymentDefault != "" {
		return deploymentDefault
	}
	return defaultModel
}

// ModelFallbacksFor resolves the runtime fallback chain for a configured list:
// the caller's own when it set one, the built-in otherwise.
//
// ⚠ AN EMPTY SLICE MEANS "UNSET", NOT "NO FALLBACKS". Nothing in this package
// can tell the two apart — a caller that genuinely wants an agent with no
// fallback at all has to say so some other way. Stated here because the
// alternative reading is the one a reader arrives with.
func ModelFallbacksFor(configured []string) []string {
	if len(configured) > 0 {
		return configured
	}
	return defaultModelFallbacks
}

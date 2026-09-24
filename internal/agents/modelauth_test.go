package agents

import (
	"errors"
	"strings"
	"testing"
)

// 🔴 THE REGRESSION TEST. This is a real defect, verbatim: an agent was
// provisioned on `z-ai/glm-5.3-flash` — the routing provider's own model id,
// copied without the `openrouter/` routing prefix — and every one of 493
// consecutive turns failed auth ("No API key found for provider \"zai\"") and
// fell back to `openrouter/deepseek/deepseek-v4-flash-0731`, burning ~11 s a
// turn. The agent answered, on the WRONG model, and nothing surfaced.
//
// Before the fix ValidateModelCredential did not exist and nothing compared the
// slug to the credential set, so this case was accepted.
func TestAnUnprefixedRoutedSlugIsRefused(t *testing.T) {
	err := ValidateModelCredential("z-ai/glm-5.3-flash")
	if err == nil {
		t.Fatal("z-ai/glm-5.3-flash was ACCEPTED: an agent provisioned with it has no " +
			"zai credential and runs its whole life on the fallback model")
	}
	if !errors.Is(err, ErrModelNotCredentialed) {
		t.Errorf("refusal does not wrap ErrModelNotCredentialed, so the HTTP layer "+
			"answers 500 instead of 400 with the fix in it: %v", err)
	}
	// The message must carry the CORRECTED slug — the whole point is that the
	// operator gets the fix, not just a rejection.
	if want := "openrouter/z-ai/glm-5.3-flash"; !strings.Contains(err.Error(), want) {
		t.Errorf("refusal does not name the corrected slug %q: %v", want, err)
	}
	// ...and must name the provider it resolved, so the message can be matched
	// against the pod's own log line.
	if !strings.Contains(err.Error(), `"z-ai"`) {
		t.Errorf("refusal does not name the resolved provider: %v", err)
	}
}

// Every slug this package itself produces must PASS, or the guard is a gate
// nobody can get through. These are the real built-ins, read from the package
// rather than retyped, so a future addition of an uncredentialed suggestion
// fails here.
func TestOwnSlugsPass(t *testing.T) {
	var slugs []string
	slugs = append(slugs, defaultModel)
	slugs = append(slugs, defaultModelFallbacks...)
	slugs = append(slugs, ModelFallbacksFor(nil)...)
	for _, m := range ModelOptions {
		slugs = append(slugs, m.Value)
	}
	if len(slugs) < 3 {
		t.Fatalf("positive-control set is suspiciously small (%d) — this test would "+
			"pass vacuously", len(slugs))
	}
	for _, s := range slugs {
		if err := ValidateModelCredential(s); err != nil {
			t.Errorf("this package's own slug %q is refused by its own guard: %v", s, err)
		}
	}
}

// TestDefaultModelIsTheFlaggedOption pins the two halves of the catalogue
// against each other.
//
// 🔴 IT IS A RELATIONSHIP GUARD, NOT A VALUE GUARD, AND THE DISTINCTION IS THE
// POINT. defaultModel and the Default-flagged entry in ModelOptions are two
// independent declarations of "the built-in model" — one read when nothing
// chose a slug, the other pre-filled into the form. Asserting either against a
// literal would keep passing while they disagreed, which is a form that
// pre-fills one model and an agent that is born on another.
//
// ⚠ ONE MUTANT IS EQUIVALENT HERE AND CANNOT BE KILLED FROM THIS TEST, stated
// rather than left for the next sweep to rediscover: replacing
// DefaultModelOption's body with `ModelOptions[0].Value` passes, because
// ModelOptions happens to flag its first entry. Nothing in a test can separate
// the two without reordering production data. What IS pinned is the shared
// body — defaultOptionOf — by TestDefaultOptionOfReadsTheFlag, which supplies
// its own list.
func TestDefaultModelIsTheFlaggedOption(t *testing.T) {
	if got := DefaultModelOption(); got != defaultModel {
		t.Fatalf("DefaultModelOption() = %q but defaultModel = %q: the form pre-fills one "+
			"slug and an agent with no chosen model is born on the other", got, defaultModel)
	}
	flagged := 0
	for _, m := range ModelOptions {
		if m.Default {
			flagged++
		}
	}
	if flagged != 1 {
		t.Errorf("%d options are flagged Default; exactly one must be, or which slug "+
			"DefaultModelOption returns depends on slice order", flagged)
	}
}

// TestDefaultOptionOfReadsTheFlag reaches the branch DefaultModelOption's own
// data cannot.
//
// 🔴 THE FLAGGED ENTRY IS DELIBERATELY NOT FIRST. ModelOptions flags its first
// entry, so every assertion against DefaultModelOption() alone is also
// satisfied by a body that returns ModelOptions[0].Value and never looks at
// m.Default — a mutation sweep scored that mutant SURVIVED against a fully
// green suite. This is the control: feed a list the first-entry shortcut
// cannot get right.
func TestDefaultOptionOfReadsTheFlag(t *testing.T) {
	opts := []ModelOption{
		{Value: "openrouter/vendor/first"},
		{Value: "openrouter/vendor/flagged", Default: true},
		{Value: "openrouter/vendor/third"},
	}
	if got := defaultOptionOf(opts); got != "openrouter/vendor/flagged" {
		t.Errorf("defaultOptionOf = %q, want the FLAGGED entry; returning the first "+
			"entry means the Default flag is not read at all", got)
	}
	// No flag at all means no pre-selection, not a silent first-entry default:
	// a form that pre-fills a model nobody chose is worse than one that does not.
	if got := defaultOptionOf([]ModelOption{{Value: "openrouter/vendor/only"}}); got != "" {
		t.Errorf("defaultOptionOf with nothing flagged = %q, want \"\"", got)
	}
	if got := defaultOptionOf(nil); got != "" {
		t.Errorf("defaultOptionOf(nil) = %q, want \"\"", got)
	}
}

// TestResolveModel drives the three-way precedence.
//
// 🔴 EVERY FIXTURE IS PAIRWISE DISTINCT AND NONE EQUALS defaultModel. That is
// the control that makes this test able to see a mutant: a case whose agent
// model happened to equal the built-in would pass against a body that ignored
// its arguments entirely and always returned the constant.
func TestResolveModel(t *testing.T) {
	const agentSlug = "openrouter/vendor/agent-choice"
	const deploySlug = "openrouter/vendor/deployment-choice"
	if agentSlug == defaultModel || deploySlug == defaultModel || agentSlug == deploySlug {
		t.Fatal("fixtures must be pairwise distinct and distinct from defaultModel, " +
			"or this test cannot distinguish the three branches")
	}
	cases := []struct {
		name   string
		agent  string
		deploy string
		want   string
	}{
		{"the agent's own model wins over both", agentSlug, deploySlug, agentSlug},
		{"the agent's own model wins with no deployment default", agentSlug, "", agentSlug},
		{"the deployment default wins when the agent chose nothing", "", deploySlug, deploySlug},
		{"the built-in wins when nothing is set", "", "", defaultModel},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ResolveModel(c.agent, c.deploy); got != c.want {
				t.Errorf("ResolveModel(%q, %q) = %q, want %q", c.agent, c.deploy, got, c.want)
			}
		})
	}
}

func TestModelFallbacksFor(t *testing.T) {
	// A configured list wins verbatim. The fixture is deliberately NOT the
	// built-in, so a body that ignored its argument cannot pass.
	custom := []string{"openrouter/vendor/a", "openrouter/vendor/b"}
	got := ModelFallbacksFor(custom)
	if len(got) != 2 || got[0] != custom[0] || got[1] != custom[1] {
		t.Errorf("ModelFallbacksFor(custom) = %v, want %v", got, custom)
	}
	// Unset falls back to the built-in, for both spellings of "unset".
	for _, in := range [][]string{nil, {}} {
		got := ModelFallbacksFor(in)
		if len(got) != len(defaultModelFallbacks) || got[0] != defaultModelFallbacks[0] {
			t.Errorf("ModelFallbacksFor(%v) = %v, want the built-in %v", in, got, defaultModelFallbacks)
		}
	}
}

// TestACredentialedSlugIsAccepted is the BEHAVIOURAL half of the seam guard
// described in the carve note at the foot of modelauth.go: a slug routed
// through the very provider this package says it credentials must validate.
//
// ⚠ IT IS DELIBERATELY HALF A GUARD. Upstream this test also rendered the
// deployment's own values and asserted that the provider written into them
// EQUALLED credentialedAuthProvider — the relationship between the two
// independent choices. That renderer did not come across (see the carve note),
// so the other side of the relationship does not exist in this repository yet
// and cannot be asserted. Saying so here is the point: a reader must not count
// this as the seam guard.
func TestACredentialedSlugIsAccepted(t *testing.T) {
	if err := ValidateModelCredential(credentialedAuthProvider + "/vendor/model"); err != nil {
		t.Errorf("a slug routed through the credentialed provider (%q) is refused: %v",
			credentialedAuthProvider, err)
	}
	// The built-in default must itself pass, or an agent provisioned with no
	// operator-chosen model is born broken.
	if err := ValidateModelCredential(defaultModel); err != nil {
		t.Errorf("the built-in default primary %q fails the guard: %v", defaultModel, err)
	}
}

// ModelProvider's parse must match the runtime's own (split on the FIRST "/"),
// including the deliberate ok=false for a slug the provider cannot be read
// from. Fixtures are pairwise distinct and none equals "openrouter", so a mutant
// that hardcodes the credentialed constant cannot survive.
func TestModelProviderParsesLikeTheRuntime(t *testing.T) {
	cases := []struct {
		slug     string
		provider string
		ok       bool
	}{
		// FIRST slash wins: the vendor half stays in the model, not the provider.
		{"openrouter/deepseek/deepseek-v4-flash-0731", "openrouter", true},
		{"z-ai/glm-5.3-flash", "z-ai", true},
		{"groq/llama-3.3-70b", "groq", true},
		// Case-folded, matching the runtime's own normalisation.
		{"OpenRouter/Xiaomi/MiMo", "openrouter", true},
		// Surrounding whitespace is trimmed before the split.
		{"  mistral/large  ", "mistral", true},
		// Not determinable from the slug alone — the runtime falls back to a
		// default provider this service cannot see, so the guard must abstain.
		{"glm-5.3-flash", "", false},
		{"", "", false},
		{"/leading-slash", "", false},
		{"trailing-slash/", "", false},
	}
	for _, c := range cases {
		p, ok := ModelProvider(c.slug)
		if ok != c.ok || p != c.provider {
			t.Errorf("ModelProvider(%q) = (%q, %v), want (%q, %v)", c.slug, p, ok, c.provider, c.ok)
		}
	}
}

// The abstain case must ACCEPT, not refuse. A slug with no "/" is resolved
// inside the agent against a default provider this service cannot observe; refusing
// it would be a guessed refusal, and a guard that blocks valid configuration
// gets disabled rather than fixed.
func TestUndeterminableProviderIsAccepted(t *testing.T) {
	for _, s := range []string{"", "   ", "glm-5.3-flash", "gpt-5"} {
		if err := ValidateModelCredential(s); err != nil {
			t.Errorf("ValidateModelCredential(%q) refused a slug whose provider is not "+
				"knowable from the string: %v", s, err)
		}
	}
}

// A handful of other real uncredentialed providers, so the guard is not pinned
// to the single slug that happened to bite us — every one of these is a live
// extension id in the agent image and an id an operator could plausibly copy
// from a provider's own docs.
func TestOtherUncredentialedProvidersAreRefused(t *testing.T) {
	for _, s := range []string{
		"anthropic/claude-sonnet-4.6",
		"openai/gpt-5",
		"google/gemini-3-pro",
		"deepseek/deepseek-v4",
		"xai/grok-4",
	} {
		err := ValidateModelCredential(s)
		if err == nil {
			t.Errorf("ValidateModelCredential(%q) accepted a provider the pod has no key for", s)
			continue
		}
		if !errors.Is(err, ErrModelNotCredentialed) {
			t.Errorf("ValidateModelCredential(%q) refusal does not wrap the sentinel: %v", s, err)
		}
	}
}

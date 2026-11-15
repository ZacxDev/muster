package officers_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// Fixture keys: invented, in the Anthropic API key shape, pairwise distinct.
const (
	credKeyA = "sk-ant-api03-fixture-security-judgement-key-AAAAAAAAAAAAAAAAAAAAAAAA"
	credKeyB = "sk-ant-api03-fixture-other-officer-key-BBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	credEnvA = "MUSTER_OFFICER_ANTHROPIC_KEY_SECURITY_EXAMPLE_JUDGEMENT"
	credEnvB = "MUSTER_OFFICER_ANTHROPIC_KEY_TRIAGE_EXAMPLE_JUDGEMENT"
)

// credDecl is a tiered officer whose judgement pool declares a credential from
// env, with extra spliced into the judgement pool.
func credDecl(id, env, extra string) string {
	return `{"id":"` + id + `","hq_url":"http://hq.invalid/api/v1",` +
		`"tiers":{"routine":{"model":"haiku"},"judgement":{"model":"opus","credential":{"secret_env":"` + env + `"}` + extra + `}}}`
}

func credEnv(m map[string]string) func(string) string {
	return func(k string) string {
		if strings.HasPrefix(k, officers.TokenEnvPrefix) {
			return "hq-" + k
		}
		return m[k]
	}
}

// TestAPoolCredentialIsThatPoolsAlone: the judgement pool that declares a
// credential carries its key; the same officer's routine pool, another
// officer and an untiered officer carry none. CredentialAgent and
// OnNoClaudeAccount answer exactly the declaring pool's instances.
func TestAPoolCredentialIsThatPoolsAlone(t *testing.T) {
	raw := `[` + credDecl("security:example", credEnvA, "") + `,` +
		`{"id":"triage:example","hq_url":"http://hq.invalid/api/v1","tiers":{"routine":{"model":"haiku"},"judgement":{"model":"opus"}}},` +
		`{"id":"release:example","hq_url":"http://hq.invalid/api/v1"}]`
	list, err := officers.Parse(raw, credEnv(map[string]string{credEnvA: " " + credKeyA + " "}), "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	reg := officers.NewRegistry(list)
	for _, c := range []struct {
		name string
		key  string
	}{
		{"officer-security-example-1", credKeyA},
		{"officer-security-example-r1", ""},
		{"officer-triage-example-1", ""},
		{"officer-triage-example-r1", ""},
		{"officer-release-example-1", ""},
	} {
		p, _, ok := reg.ForAgentName(c.name)
		if !ok {
			t.Fatalf("%s is not an instance", c.name)
		}
		if got := p.PoolCredential().Key(); got != c.key {
			t.Errorf("%s: pool key %q, want %q", c.name, got, c.key)
		}
		_, own := officers.CredentialAgent(p, c.name)
		if own != (c.key != "") || officers.OnNoClaudeAccount(p, c.name) != (c.key != "") {
			t.Errorf("%s: CredentialAgent %v, OnNoClaudeAccount %v; want both %v", c.name, own, officers.OnNoClaudeAccount(p, c.name), c.key != "")
		}
	}
	// A name past the pool's max is no instance, and so on no credential.
	sec, _ := reg.Get("security:example")
	if _, own := officers.CredentialAgent(sec, "officer-security-example-2"); own {
		t.Error("an instance number past the pool's max runs on the pool's credential")
	}
}

// TestAPoolCredentialFailsClosed: every way the key could be missing or wrong
// is a boot refusal that names the variable and never prints a value — never
// a pool that falls back to the shared account pool or setup-token.
func TestAPoolCredentialFailsClosed(t *testing.T) {
	const prefix = "MUSTER_OFFICERS[0]: officer security:example: tiers.judgement.credential"
	setup := "sk-ant-oat01-fixture-a-subscription-setup-token-CCCCCCCCCCCCCCCCCCCCC"
	for _, c := range []struct {
		name, decl string
		env        map[string]string
		want       string
	}{
		{"unset", credDecl("security:example", credEnvA, ""), nil,
			prefix + ": " + credEnvA + " is not set: the pool runs only on its own Anthropic API key and never falls back to " +
				"the shared account pool or setup-token, so it does not start without one"},
		{"blank", credDecl("security:example", credEnvA, ""), map[string]string{credEnvA: "   "},
			prefix + ": " + credEnvA + " is not set: the pool runs only on its own Anthropic API key and never falls back to " +
				"the shared account pool or setup-token, so it does not start without one"},
		{"a setup-token", credDecl("security:example", credEnvA, ""), map[string]string{credEnvA: setup},
			prefix + ": " + credEnvA + " holds a subscription setup-token, not an Anthropic API key (sk-ant-api…): the pool's own " +
				"credential is an API key with a spend cap, never a setup-token"},
		{"not a key", credDecl("security:example", credEnvA, ""), map[string]string{credEnvA: "opaque-value-0123456789"},
			prefix + ": " + credEnvA + " does not hold an Anthropic API key (sk-ant-api…)"},
		{"a shared variable", credDecl("security:example", "MUSTER_AGENT_CC_ACCOUNTS", ""), map[string]string{"MUSTER_AGENT_CC_ACCOUNTS": credKeyA},
			prefix + `.secret_env "MUSTER_AGENT_CC_ACCOUNTS" is not MUSTER_OFFICER_ANTHROPIC_KEY_<NAME> (upper-case letters, digits and _): ` +
				"the key is the officer's own variable, never a deployment-wide one"},
	} {
		_, err := officers.Parse(`[`+c.decl+`]`, credEnv(c.env), "muster-agent-")
		if err == nil || err.Error() != c.want {
			t.Errorf("%s: Parse = %v, want exactly\n%s", c.name, err, c.want)
		}
		if err != nil && (strings.Contains(err.Error(), credKeyA) || strings.Contains(err.Error(), setup)) {
			t.Errorf("%s: the refusal prints the value: %v", c.name, err)
		}
	}
}

// TestAPoolCredentialIsRefusedBesideTheProxyAndWhenShared: a pool on the LLM
// proxy has its model credential already (the OpenRouter key the proxy holds);
// two pools naming one variable, or holding one key, are refused; an unknown
// key in the block is refused. Control: two officers with distinct keys parse.
func TestAPoolCredentialIsRefusedBesideTheProxyAndWhenShared(t *testing.T) {
	env := credEnv(map[string]string{credEnvA: credKeyA, credEnvB: credKeyB, "MUSTER_OFFICER_OPENROUTER_KEY_SECURITY_EXAMPLE": "or-key",
		"MUSTER_OFFICER_ANTHROPIC_KEY_SECURITY_EXAMPLE_ROUTINE": credKeyA})
	for _, c := range []struct{ name, raw, want string }{
		{"judgement on the proxy",
			`[{"id":"security:example","hq_url":"http://hq.invalid/api/v1","tiers":{"routine":{"model":"haiku"},"judgement":{"model":"vendor/model",` +
				`"credential":{"secret_env":"` + credEnvA + `"},"llm":{"daily_budget_usd":5}}}}]`, "credential: this pool reaches its model through muster's LLM proxy"},
		{"routine beside llm",
			`[{"id":"security:example","hq_url":"http://hq.invalid/api/v1","llm":{"daily_budget_usd":5},"tiers":{"routine":{"model":"vendor/model",` +
				`"credential":{"secret_env":"` + credEnvA + `"}},"judgement":{"model":"opus"}}}]`, "credential: this pool reaches its model through muster's LLM proxy"},
		{"one variable, two officers", `[` + credDecl("security:example", credEnvA, "") + `,` + credDecl("triage:example", credEnvA, "") + `]`,
			"MUSTER_OFFICERS: officer security:example's judgement pool and officer triage:example's judgement pool both name " + credEnvA},
		{"one key, two pools",
			`[{"id":"security:example","hq_url":"http://hq.invalid/api/v1","tiers":{"routine":{"model":"haiku","credential":{"secret_env":"MUSTER_OFFICER_ANTHROPIC_KEY_SECURITY_EXAMPLE_ROUTINE"}},` +
				`"judgement":{"model":"opus","credential":{"secret_env":"` + credEnvA + `"}}}}]`,
			"MUSTER_OFFICERS: officer security:example's judgement pool and officer security:example's routine pool hold the same key"},
		{"an unknown key", `[` + strings.Replace(credDecl("security:example", credEnvA, ""), `"secret_env"`, `"value":"x","secret_env"`, 1) + `]`,
			`tiers.judgement.credential must be {"secret_env"`},
	} {
		_, err := officers.Parse(c.raw, env, "muster-agent-")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Parse = %v, want it to say %q", c.name, err, c.want)
		}
		if err != nil && strings.Contains(err.Error(), credKeyA) {
			t.Errorf("%s: the refusal prints the key", c.name)
		}
	}
	if _, err := officers.Parse(`[`+credDecl("security:example", credEnvA, "")+`,`+credDecl("triage:example", credEnvB, "")+`]`, env, "muster-agent-"); err != nil {
		t.Fatalf("control: two officers with their own keys: %v", err)
	}
}

// TestAPoolCredentialIsNeverPrinted: the officer, its pool and the credential
// print without the key under every verb a log line uses, and JSON leaves it
// out; Client.Scrub — the transcript's first filter — replaces the key, and a
// word a cap cut short that ends in its first bytes.
func TestAPoolCredentialIsNeverPrinted(t *testing.T) {
	list, err := officers.Parse(`[`+credDecl("security:example", credEnvA, "")+`]`, credEnv(map[string]string{credEnvA: credKeyA}), "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	o := list[0]
	c := o.PoolCredential()
	for _, s := range []string{fmt.Sprintf("%v %+v %#v %s", o, o, o, o), fmt.Sprintf("%v %+v %#v %s", c, c, c, c),
		fmt.Sprintf("%v %+v %#v", *c, *c, *c), fmt.Sprintf("%v %+v", o.Tiers, *o.Tiers)} {
		if strings.Contains(s, credKeyA) || strings.Contains(s, credKeyA[:24]) {
			t.Errorf("printed the key: %s", s)
		}
	}
	if b, _ := json.Marshal(o); strings.Contains(string(b), credKeyA) {
		t.Errorf("JSON carries the key: %s", b)
	}
	if b, _ := json.Marshal(o.Tiers); strings.Contains(string(b), credKeyA) {
		t.Errorf("the tiers' JSON carries the key: %s", b)
	}
	cl := officers.NewClient(o, http.DefaultClient)
	got := cl.Scrub("export ANTHROPIC_API_KEY=" + credKeyA + " and a cut " + credKeyA[:30] + "…")
	if strings.Contains(got, credKeyA[:12]) || !strings.Contains(got, "[officer API key withheld]") {
		t.Errorf("Scrub left the key: %q", got)
	}
	// Control: another officer's client does not know this key.
	other, _ := officers.Parse(`[{"id":"release:example","hq_url":"http://hq.invalid/api/v1"}]`, credEnv(nil), "muster-agent-")
	if s := officers.NewClient(other[0], http.DefaultClient).Scrub(credKeyA); s != credKeyA {
		t.Errorf("control: another officer's scrub changed the text: %q", s)
	}
}

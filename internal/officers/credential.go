package officers

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// A pool's own model credential: tiers.<pool>.credential, {"secret_env":
// "MUSTER_OFFICER_ANTHROPIC_KEY_<…>"}. See docs/officers.md "A pool's own
// credential".
//
// 🔴 A POOL WITH ITS OWN CREDENTIAL RUNS ON THAT ANTHROPIC API KEY AND NOTHING
// ELSE. Its pods carry the key (ANTHROPIC_API_KEY) and never the shared
// account pool's setup-token; a missing or empty key is a boot refusal, and
// the pod spec refuses an empty one again (agentspec), so there is no fallback
// to the shared pool anywhere. It exists so that untrusted text a pool reads
// (an alert promoted to the judgement tier) can reach only a key that serves
// this one officer's pool, with a spend cap set on the provider side.
//
// The key is held behind an unexported field that encoding/json and fmt never
// print, like the HQ token and the OpenRouter key.

// CredentialEnvPrefix is the prefix every secret_env must carry: the variable
// is the officer's own, never a deployment-wide one (the account pool, the HQ
// token, another officer's key).
const CredentialEnvPrefix = "MUSTER_OFFICER_ANTHROPIC_KEY_"

// credentialEnvRE is a secret_env: the prefix, then upper-case letters,
// digits and underscores.
var credentialEnvRE = regexp.MustCompile(`^` + CredentialEnvPrefix + `[A-Z0-9_]+$`)

// The shapes an Anthropic credential's value takes: an API key, and the
// subscription setup-token the shared account pool holds (credentials.go's
// ClaudeTokenPrefix), which is never accepted here.
const (
	anthropicAPIKeyPrefix   = "sk-ant-api"
	anthropicSetupTokenPrfx = "sk-ant-oat"
)

// Credential is a pool's own model credential.
type Credential struct {
	// SecretEnv names the muster-server variable the key is read from.
	SecretEnv string `json:"secret_env"`
	key       string
}

// Key returns the API key. Its callers are the pod spec (the instance's
// Secret) and the transcript scrub; tests assert it appears nowhere else.
func (c *Credential) Key() string {
	if c == nil {
		return ""
	}
	return c.key
}

// WithKey returns a copy of c carrying key (tests, and wiring outside Parse).
func (c Credential) WithKey(key string) *Credential { c.key = key; return &c }

// String describes c without its key.
func (c Credential) String() string { return "credential from " + c.SecretEnv }

// GoString is String, so %#v cannot print the key either.
func (c Credential) GoString() string { return c.String() }

// PoolCredential is the own credential of the pool o is: tiers.<pool>.credential,
// nil for an untiered officer and for a pool without one. o read as the
// officer itself is its judgement pool.
func (o Officer) PoolCredential() *Credential {
	if o.Tiers == nil {
		return nil
	}
	switch o.Tier {
	case TierRoutine:
		return o.Tiers.Routine.Credential
	case TierJudgement:
		return o.Tiers.Judgement.Credential
	}
	return nil
}

// CredentialAgent is THE rule for whether agent name runs on a pool's own
// credential: an instance of one of o's pools that declares one, by the
// registry's naming rule (parseInstance). It answers that pool. o may be the
// officer or either of its pools.
func CredentialAgent(o Officer, name string) (Officer, bool) {
	if o.Tiers == nil {
		return Officer{}, false
	}
	p, _, ok := parseInstance(o, name)
	if !ok || p.PoolCredential() == nil {
		return Officer{}, false
	}
	return p, true
}

// OnNoClaudeAccount is THE rule for whether instance name of officer o runs
// on none of the shared Claude accounts, whatever account its agent row
// names: a pool on the LLM proxy (ProxyAgent) or a pool with its own
// credential (CredentialAgent). Every consumer that would otherwise credit,
// mark or move that account asks it: the usage report's account, the
// accounts' attribution, and the account marks and failover a failed turn
// would write.
func OnNoClaudeAccount(o Officer, name string) bool {
	if _, _, proxied := ProxyAgent(o, name); proxied {
		return true
	}
	_, own := CredentialAgent(o, name)
	return own
}

// Keys are the own-credential keys of o's pools (none for an untiered
// officer), for the scrub that clears text an instance produced of every
// credential the officer has (Client.Scrub).
func (o Officer) Keys() []string {
	if o.Tiers == nil {
		return nil
	}
	var out []string
	for _, c := range []*Credential{o.Tiers.Judgement.Credential, o.Tiers.Routine.Credential} {
		if k := c.Key(); k != "" {
			out = append(out, k)
		}
	}
	return out
}

// parseCredential reads tiers.<pool>.credential strictly, and its key through
// getenv. where names the field in every refusal; the refusals name the
// variable and never print a value.
func parseCredential(where string, raw json.RawMessage, getenv func(string) string) (*Credential, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var c Credential
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf(`%s must be {"secret_env": "%s<NAME>"}: %v`, where, CredentialEnvPrefix, err)
	}
	c.SecretEnv = strings.TrimSpace(c.SecretEnv)
	if !credentialEnvRE.MatchString(c.SecretEnv) {
		return nil, fmt.Errorf("%s.secret_env %q is not %s<NAME> (upper-case letters, digits and _): the key is the "+
			"officer's own variable, never a deployment-wide one", where, c.SecretEnv, CredentialEnvPrefix)
	}
	// The variable is the prefix and the declared suffix (credentialEnvRE): spelled that way so
	// the read is the prefix's (the environment-variable documentation lint resolves it).
	k := strings.TrimSpace(getenv(CredentialEnvPrefix + strings.TrimPrefix(c.SecretEnv, CredentialEnvPrefix)))
	switch {
	case k == "":
		return nil, fmt.Errorf("%s: %s is not set: the pool runs only on its own Anthropic API key and never falls back "+
			"to the shared account pool or setup-token, so it does not start without one", where, c.SecretEnv)
	case strings.HasPrefix(k, anthropicSetupTokenPrfx):
		return nil, fmt.Errorf("%s: %s holds a subscription setup-token, not an Anthropic API key (%s…): the pool's "+
			"own credential is an API key with a spend cap, never a setup-token", where, c.SecretEnv, anthropicAPIKeyPrefix)
	case !strings.HasPrefix(k, anthropicAPIKeyPrefix):
		return nil, fmt.Errorf("%s: %s does not hold an Anthropic API key (%s…)", where, c.SecretEnv, anthropicAPIKeyPrefix)
	}
	c.key = k
	return &c, nil
}

// validateCredentials refuses two pools — of one officer or of two — whose
// own credentials name one variable or hold one key: a pool's key serves that
// pool alone.
func validateCredentials(list []Officer) error {
	type use struct{ officer, pool string }
	byEnv, byKey := map[string]use{}, map[string]use{}
	for _, o := range list {
		for _, p := range o.Pools() {
			c := p.PoolCredential()
			if c == nil {
				continue
			}
			u := use{o.ID, p.Tier}
			if prev, ok := byEnv[c.SecretEnv]; ok {
				return fmt.Errorf("officer %s's %s pool and officer %s's %s pool both name %s: a pool's own credential "+
					"serves that pool alone", prev.officer, prev.pool, u.officer, u.pool, c.SecretEnv)
			}
			byEnv[c.SecretEnv] = u
			if prev, ok := byKey[c.Key()]; ok {
				return fmt.Errorf("officer %s's %s pool and officer %s's %s pool hold the same key (in %s): a pool's own "+
					"credential serves that pool alone", prev.officer, prev.pool, u.officer, u.pool, c.SecretEnv)
			}
			byKey[c.Key()] = u
		}
	}
	return nil
}

// errCredentialOnProxy is the refusal of a credential on a pool that reaches
// its model through muster's LLM proxy: the proxy holds that pool's key.
var errCredentialOnProxy = errors.New("credential: this pool reaches its model through muster's LLM proxy, which holds " +
	"its key; a pool has one model credential")

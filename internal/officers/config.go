// Package officers is muster's officer RUNTIME: the long-lived, queue-driven
// agents whose durable identity, work queue and claim lock live in an HQ
// server (the "officers" contract), and whose running sessions — instances —
// muster provisions, wakes and scales to zero.
//
// What lives here is the part that knows nothing about muster's server: the
// declared configuration (config.go), the reserved agent slugs (slug.go), the
// HQ client (hq.go), the wake/scale decision and loop (wake.go), the Standard
// Webhooks verifier (webhook.go) and the first message an instance is given
// (kickoff.go). internal/api binds it to agents, the provisioner and the MCP
// tool registry; cmd/muster-server reads the environment and runs the loop.
//
// 🔴 THE OFFICER TOKEN NEVER LEAVES muster-server. It is read from the
// server's own environment (MUSTER_OFFICER_TOKEN_<NAME>), held in [Officer]
// behind a field JSON and fmt never print, and spent only by [Client] on the
// way to HQ. An instance's pod reaches HQ through muster's MCP tools, which
// attach the token server-side; the pod's spec, env, files and first message
// carry no trace of it. See docs/officers.md.
package officers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// KindClaudeCode is the one runtime kind an officer instance runs on. It is
// the same string as agents.KindClaudeCode; it is repeated rather than
// imported so this package stays free of muster's server packages, and
// internal/api asserts the two agree.
const KindClaudeCode = "claude-code"

// Defaults applied to a declared officer that leaves a field out.
const (
	DefaultMaxInstances = 1
	DefaultIdleAfter    = 30 * time.Minute
	DefaultPollInterval = time.Minute
	// MinPollInterval bounds how often muster asks HQ, whatever is declared:
	// a typo of "1s" for "1m" must not become sixty requests a minute per
	// officer.
	MinPollInterval = 10 * time.Second
	// MaxInstancesCap bounds max_instances. Each instance is a pod, a PVC and a
	// Claude account for life, so a fat-fingered 100 is refused at boot.
	MaxInstancesCap = 8
)

// Officer is one declared officer: the HQ identity muster runs instances for.
type Officer struct {
	// ID is HQ's officer id, `<type>:<area>` (e.g. `release:example-web`).
	ID string `json:"id"`
	// HQURL is HQ's API base, including `/api/v1` (e.g.
	// `https://hq.example.com/api/v1`). Paths are appended to it.
	HQURL string `json:"hq_url"`
	// MaxInstances is how many instances may run at once (1..MaxInstancesCap).
	MaxInstances int `json:"max_instances"`
	// IdleAfter is how long the officer must be idle — no claimable item and
	// no live claim held by any of its instances — before its instances are
	// scaled to zero.
	IdleAfter time.Duration `json:"-"`
	// PollInterval is how often HQ's open-items list is read when nothing
	// else wakes the loop.
	PollInterval time.Duration `json:"-"`
	// Owner is the muster user the instance agents belong to (default 1, the
	// operator). Row-level security scopes the agents to that user.
	Owner int64 `json:"owner"`
	// CCAccount optionally pins the Claude account instances run on; empty
	// lets the account pool choose (each instance keeps its account for life).
	CCAccount string `json:"cc_account,omitempty"`

	// token is the officer's HQ token. Unexported, so encoding/json never
	// writes it; String and GoString never print it. Token() is the one read.
	token string
}

// Token returns the officer's HQ token. Its only callers are the HQ client
// and tests asserting it does not appear where it must not.
func (o Officer) Token() string { return o.token }

// WithToken returns o carrying token. Exported for tests and for wiring that
// builds an Officer outside Parse.
func (o Officer) WithToken(token string) Officer { o.token = token; return o }

// String names the officer without its token.
func (o Officer) String() string {
	return fmt.Sprintf("officer %s (hq %s, max %d, idle %s, poll %s)", o.ID, o.HQURL, o.MaxInstances, o.IdleAfter, o.PollInterval)
}

// GoString is String, so %#v cannot print the token either.
func (o Officer) GoString() string { return o.String() }

// declared is the JSON shape of one MUSTER_OFFICERS entry.
type declared struct {
	ID           string `json:"id"`
	HQURL        string `json:"hq_url"`
	MaxInstances int    `json:"max_instances"`
	IdleAfter    string `json:"idle_scale_to_zero_after"`
	PollInterval string `json:"poll_interval"`
	Owner        int64  `json:"owner"`
	Kind         string `json:"kind"`
	CCAccount    string `json:"cc_account"`
}

// idPart is one half of an officer id: lowercase alphanumerics and single
// hyphens, not starting or ending with one.
const idPart = `[a-z0-9]+(?:-[a-z0-9]+)*`

var idRE = regexp.MustCompile(`^` + idPart + `:` + idPart + `$`)

// TokenEnvPrefix is the prefix of the variable each officer's HQ token is read
// from: MUSTER_OFFICER_TOKEN_<NAME>, where NAME is [TokenEnvSuffix] of its id.
const TokenEnvPrefix = "MUSTER_OFFICER_TOKEN_"

// TokenEnvSuffix is the officer id as an environment-variable suffix:
// upper-cased, with every non-alphanumeric byte written as '_'
// (`release:example-web` -> `RELEASE_EXAMPLE_WEB`).
func TokenEnvSuffix(id string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(id) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// Parse reads MUSTER_OFFICERS — a JSON array of officer declarations — and
// each declared officer's token through getenv. Empty raw is no officers.
//
// It refuses (and names the reason) rather than guessing: an unknown field, an
// id that is not `<type>:<area>`, two officers whose slugs collide, a kind
// other than claude-code, a missing token, a base URL that is not http(s).
func Parse(raw string, getenv func(string) string) ([]Officer, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var in []declared
	if err := dec.Decode(&in); err != nil {
		return nil, fmt.Errorf("MUSTER_OFFICERS: not a JSON array of officers: %w", err)
	}
	out := make([]Officer, 0, len(in))
	seenID := map[string]bool{}
	seenSlug := map[string]string{}
	seenSuffix := map[string]string{}
	for i, d := range in {
		o, err := d.officer(getenv)
		if err != nil {
			return nil, fmt.Errorf("MUSTER_OFFICERS[%d]: %w", i, err)
		}
		if seenID[o.ID] {
			return nil, fmt.Errorf("MUSTER_OFFICERS[%d]: officer %q is declared twice", i, o.ID)
		}
		seenID[o.ID] = true
		if other, ok := seenSlug[Slug(o.ID)]; ok {
			return nil, fmt.Errorf("MUSTER_OFFICERS[%d]: officers %q and %q have the same agent slug %q", i, other, o.ID, Slug(o.ID))
		}
		seenSlug[Slug(o.ID)] = o.ID
		if other, ok := seenSuffix[TokenEnvSuffix(o.ID)]; ok {
			return nil, fmt.Errorf("MUSTER_OFFICERS[%d]: officers %q and %q read the same token variable %s%s", i, other, o.ID, TokenEnvPrefix, TokenEnvSuffix(o.ID))
		}
		seenSuffix[TokenEnvSuffix(o.ID)] = o.ID
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (d declared) officer(getenv func(string) string) (Officer, error) {
	o := Officer{
		ID:           strings.TrimSpace(d.ID),
		HQURL:        strings.TrimRight(strings.TrimSpace(d.HQURL), "/"),
		MaxInstances: d.MaxInstances,
		Owner:        d.Owner,
		CCAccount:    strings.TrimSpace(d.CCAccount),
		IdleAfter:    DefaultIdleAfter,
		PollInterval: DefaultPollInterval,
	}
	if !idRE.MatchString(o.ID) {
		return Officer{}, fmt.Errorf("id %q is not <type>:<area> (lowercase letters, digits and single hyphens on each side)", d.ID)
	}
	if k := strings.TrimSpace(d.Kind); k != "" && k != KindClaudeCode {
		return Officer{}, fmt.Errorf("officer %s: kind %q: officers run on %s only", o.ID, k, KindClaudeCode)
	}
	u, err := url.Parse(o.HQURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Officer{}, fmt.Errorf("officer %s: hq_url %q is not an http(s) URL", o.ID, d.HQURL)
	}
	if u.User != nil {
		return Officer{}, fmt.Errorf("officer %s: hq_url must not carry credentials; the token comes from %s%s", o.ID, TokenEnvPrefix, TokenEnvSuffix(o.ID))
	}
	if o.MaxInstances == 0 {
		o.MaxInstances = DefaultMaxInstances
	}
	if o.MaxInstances < 1 || o.MaxInstances > MaxInstancesCap {
		return Officer{}, fmt.Errorf("officer %s: max_instances %d is not 1..%d", o.ID, d.MaxInstances, MaxInstancesCap)
	}
	if o.Owner == 0 {
		o.Owner = 1
	}
	if o.Owner < 0 {
		return Officer{}, fmt.Errorf("officer %s: owner %d is not a user id", o.ID, d.Owner)
	}
	if s := strings.TrimSpace(d.IdleAfter); s != "" {
		v, err := time.ParseDuration(s)
		if err != nil || v <= 0 {
			return Officer{}, fmt.Errorf("officer %s: idle_scale_to_zero_after %q is not a positive duration", o.ID, s)
		}
		o.IdleAfter = v
	}
	if s := strings.TrimSpace(d.PollInterval); s != "" {
		v, err := time.ParseDuration(s)
		if err != nil || v <= 0 {
			return Officer{}, fmt.Errorf("officer %s: poll_interval %q is not a positive duration", o.ID, s)
		}
		o.PollInterval = v
	}
	if o.PollInterval < MinPollInterval {
		return Officer{}, fmt.Errorf("officer %s: poll_interval %s is below the %s floor", o.ID, o.PollInterval, MinPollInterval)
	}
	if n := InstanceName(o.ID, o.MaxInstances); len(n) > MaxInstanceNameLen {
		return Officer{}, fmt.Errorf("officer %s: its instance agent name %q is %d characters, over %d (it becomes a namespace name with the deployment's prefix)",
			o.ID, n, len(n), MaxInstanceNameLen)
	}
	tok := strings.TrimSpace(getenv(TokenEnvPrefix + TokenEnvSuffix(o.ID)))
	if tok == "" {
		return Officer{}, fmt.Errorf("officer %s: no token: set %s%s", o.ID, TokenEnvPrefix, TokenEnvSuffix(o.ID))
	}
	o.token = tok
	return o, nil
}

// ErrUnknownOfficer is a lookup of an id no declared officer has.
var ErrUnknownOfficer = errors.New("officers: no such officer")

// Registry is the declared officers, looked up by id or by instance agent
// name. It is immutable after NewRegistry.
type Registry struct {
	list []Officer
	byID map[string]Officer
}

// NewRegistry indexes list. It does not re-validate: Parse did.
func NewRegistry(list []Officer) *Registry {
	r := &Registry{byID: map[string]Officer{}}
	for _, o := range list {
		r.list = append(r.list, o)
		r.byID[o.ID] = o
	}
	return r
}

// All returns the declared officers, sorted by id.
func (r *Registry) All() []Officer {
	if r == nil {
		return nil
	}
	return append([]Officer(nil), r.list...)
}

// Get returns the officer with id.
func (r *Registry) Get(id string) (Officer, error) {
	if r == nil {
		return Officer{}, ErrUnknownOfficer
	}
	o, ok := r.byID[id]
	if !ok {
		return Officer{}, ErrUnknownOfficer
	}
	return o, nil
}

// ForAgentName answers which declared officer an agent named name is an
// instance of, and which instance (1..MaxInstances). It is THE binding rule:
// the MCP tools act for the officer this returns and no other, and an agent
// whose name it does not parse is not an officer instance at all.
func (r *Registry) ForAgentName(name string) (Officer, int, bool) {
	if r == nil {
		return Officer{}, 0, false
	}
	for _, o := range r.list {
		if n, ok := parseInstance(o, name); ok {
			return o, n, true
		}
	}
	return Officer{}, 0, false
}

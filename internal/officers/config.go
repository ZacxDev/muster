// Package officers is muster's officer RUNTIME: the long-lived, queue-driven
// agents whose durable identity, work queue and claim lock live in an HQ
// server (the "officers" contract), and whose running sessions — instances —
// muster provisions, wakes and scales to zero.
//
// What lives here is the part that knows nothing about muster's server: the
// declared configuration (config.go), the reserved agent slugs (slug.go), the
// HQ client (hq.go), the wake/scale decision and loop (wake.go), and the
// first message and re-prompt an instance is given (kickoff.go, wake.go). internal/api binds it to agents, the provisioner and the MCP
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

	"github.com/ZacxDev/muster/internal/github"
	"github.com/ZacxDev/muster/internal/podagent"
)

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
	// MaxReadRepos bounds read_repos. Each entry is one more repository an
	// approval of the officer's `github` grant reaches, so the list is kept
	// short enough to read on the approve card.
	MaxReadRepos = 5
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
	// lets the account pool choose (an instance keeps its account until an
	// account failover moves it, internal/ccfailover).
	CCAccount string `json:"cc_account,omitempty"`
	// CCFailover opts instances pinned by CCAccount in to account failover
	// (internal/ccfailover): without it a pinned instance stays on its
	// account when that account hits its limit. Unpinned instances fail over
	// whatever it says (unless MUSTER_AGENT_CC_FAILOVER=off).
	CCFailover bool `json:"cc_failover,omitempty"`
	// Model is the model the instances' Claude Code sessions run ("opus", a
	// full model id); "" leaves the account's default.
	Model string `json:"model,omitempty"`
	// Repo is the GitHub repository (owner/name) the officer's instances act
	// on as their OWN repository for connectors; "" for none. It is what the
	// GitHub App connector mints a token for (internal/api Server.agentRepo
	// feeds it to connectors.Caller.Repo), so an approved `github` grant gives
	// an instance exactly this one repository — read-only on a deployment
	// with MUSTER_GITHUB_APP_ACCESS=read. It is NOT checked out: the instance
	// reads it through muster's connector proxy.
	Repo string `json:"repo,omitempty"`
	// ReadRepos are further GitHub repositories (owner/name) the officer's
	// instances may READ through the same `github` connector: a proxied request
	// that targets one of them is served with a READ-ONLY installation token
	// minted for that one repository (internal/githubapp App.Credential),
	// whatever MUSTER_GITHUB_APP_ACCESS says. Lowercased, sorted and unique —
	// so a reordered or re-cased declaration is the same set and does not
	// re-open an approved grant (connectors.RepoScope). Requires Repo, never
	// holds it, at most MaxReadRepos, and accepted beside any ship stage (never
	// one of ship.repos).
	ReadRepos []string `json:"read_repos,omitempty"`
	// PinRepoReadsToItem pins every `github` read of the officer's instances
	// to the item each one holds: the item's payload names, per repository,
	// the ONE commit it may be read at (ItemRepoPins), and the proxy refuses
	// any other read — git, history, pull requests, search, another ref —
	// before a credential exists (connectors.CheckPinnedRead). For replaying
	// a past event against the repository as it was. Requires Repo; refused
	// beside Ship.
	PinRepoReadsToItem bool `json:"pin_repo_reads_to_item,omitempty"`
	// Ship, when set, lets the officer push branches, open pull requests, and
	// (by stage) merge its own pull requests and tag releases in Repo — every
	// write filtered or performed by muster (ship.go). nil for every other
	// officer, whose GitHub access stays read-only.
	Ship *Ship `json:"-"`
	// CILogs, when set (a shipping officer only), offers officer_ci_logs: the
	// failing tests and error lines of the officer's own pull request's CI,
	// read by muster-server from a Tekton namespace (cilogs.go).
	CILogs *CILogs `json:"-"`
	// Transcript, when set and enabled, forwards each instance's live session
	// transcript to HQ while it holds a claimed item
	// (internal/officers/transcript). nil (the default) forwards nothing.
	Transcript *Transcript `json:"-"`
	// Connectors are the connectors, besides `github`, the officer's instances
	// may REQUEST (internal/api officerConnectorRefusal), by name — e.g. a
	// postgres connector for read-only SQL. Declaring one grants nothing: each
	// instance's request is still decided explicitly by the owner, and is never
	// approved from a shared approval. Sorted and deduplicated.
	Connectors []string `json:"connectors,omitempty"`
	// Tiers, when set, splits the officer's instances into two pools — routine
	// and judgement — each with its own model and max instances (tiers.go).
	// nil (no `tiers` block) is one untiered pool: today's behaviour.
	Tiers *Tiers `json:"-"`
	// Tier is the pool this value describes: "" for an untiered officer, else
	// TierJudgement or TierRoutine (Officer.Pool). Parse returns a tiered
	// officer as its judgement pool.
	Tier string `json:"-"`
	// LLM, when set, lets the officer's ROUTINE pool reach its model
	// (tiers.routine.model) through muster's LLM proxy (llm.go,
	// internal/llmproxy), within the daily USD budget and per-item token cap
	// it holds. It needs Tiers. nil (the default) refuses every proxy request
	// for the officer.
	LLM *LLM `json:"-"`
	// Kube, when set, offers the officer's JUDGEMENT instances
	// officer_kube_read over the clusters it names (kube.go). It needs Tiers.
	// nil (the default) offers no cluster read.
	Kube *Kube `json:"-"`

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
	if o.Tiers != nil {
		return fmt.Sprintf("officer %s (hq %s, judgement max %d, routine max %d, idle %s, poll %s)", o.ID, o.HQURL, o.Tiers.Judgement.MaxInstances, o.Tiers.Routine.MaxInstances, o.IdleAfter, o.PollInterval)
	}
	return fmt.Sprintf("officer %s (hq %s, max %d, idle %s, poll %s)", o.ID, o.HQURL, o.MaxInstances, o.IdleAfter, o.PollInterval)
}

// GoString is String, so %#v cannot print the token either.
func (o Officer) GoString() string { return o.String() }

// declared is the JSON shape of one MUSTER_OFFICERS entry.
type declared struct {
	ID           string   `json:"id"`
	HQURL        string   `json:"hq_url"`
	MaxInstances int      `json:"max_instances"`
	IdleAfter    string   `json:"idle_scale_to_zero_after"`
	PollInterval string   `json:"poll_interval"`
	Owner        int64    `json:"owner"`
	CCAccount    string   `json:"cc_account"`
	CCFailover   bool     `json:"cc_failover"`
	Model        string   `json:"model"`
	Repo         string   `json:"repo"`
	ReadRepos    []string `json:"read_repos"`
	PinReads     bool     `json:"pin_repo_reads_to_item"`
	Connectors   []string `json:"connectors"`
	// Ship is parsed by parseShip (ship.go), which reads its own secrets.
	Ship json.RawMessage `json:"ship"`
	// CILogs is parsed by parseCILogs (cilogs.go); it needs Ship.
	CILogs json.RawMessage `json:"ci_logs"`
	// Transcript is parsed by parseTranscript.
	Transcript json.RawMessage `json:"transcript"`
	// Tiers is parsed by parseTiers (tiers.go).
	Tiers json.RawMessage `json:"tiers"`
	// LLM is parsed by parseLLM (llm.go), which reads its own key.
	LLM json.RawMessage `json:"llm"`
	// Kube is parsed by parseKube (kube.go), which reads each cluster's
	// sealed environment.
	Kube json.RawMessage `json:"kube"`
}

// Transcript is an officer's `transcript` block: whether its instances' live
// session transcripts are forwarded to HQ (POST …/items/{item}/transcript)
// while they hold a claimed item.
type Transcript struct {
	Enabled bool `json:"enabled"`
}

// TranscriptEnabled reports whether o's instances' transcripts are forwarded
// to HQ. THE one switch: the forwarder asks it before every read and post.
func (o Officer) TranscriptEnabled() bool { return o.Transcript != nil && o.Transcript.Enabled }

// parseTranscript reads a `transcript` block strictly: an unknown key is a
// refusal, as everywhere in MUSTER_OFFICERS.
func parseTranscript(raw json.RawMessage) (*Transcript, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var t Transcript
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("transcript must be {\"enabled\": true|false}: %v", err)
	}
	return &t, nil
}

// idPart is one half of an officer id: lowercase alphanumerics and single
// hyphens, not starting or ending with one.
const idPart = `[a-z0-9]+(?:-[a-z0-9]+)*`

var idRE = regexp.MustCompile(`^` + idPart + `:` + idPart + `$`)

// connectorNameRE is internal/connectors' connector-name rule (ValidateName),
// repeated here because this package does not import the connector store.
var connectorNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// MayRequestConnector is THE rule for which connectors officer o's instances
// may request and use: `github`, plus the ones o declares in `connectors`.
func MayRequestConnector(o Officer, name string) bool {
	if name == GitHubConnector {
		return true
	}
	// kube-read: the judgement pool of an officer with a `kube` block only
	// (kube.go). Never by declaring it in `connectors`, which is refused.
	if name == KubeConnector {
		return len(o.KubeClusters()) > 0
	}
	for _, c := range o.Connectors {
		if c == name {
			return true
		}
	}
	return false
}

// parseReadRepos checks an officer's `read_repos` against its `repo` and
// returns it canonical: trimmed, lowercased (GitHub names are
// case-insensitive) and sorted. It refuses, naming the entry, a value that is
// not a GitHub owner/name (github.ValidRepoName, `repo`'s own check), an entry
// listed twice (in any case), an entry that is `repo` itself, more than
// MaxReadRepos entries, and any entry when there is no `repo`.
//
// 🔴 read_repos NEEDS repo. The GitHub App refuses a caller with no repository
// of its own (githubapp.App.Credential), and a request whose path names no
// repository (/graphql, /rate_limit) is served with the token for `repo`; an
// officer with read_repos alone would have neither.
func parseReadRepos(repo string, in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if repo == "" {
		return nil, errors.New("read_repos needs `repo`: the officer's own repository is what a request that names " +
			"no repository is served for, and the GitHub App refuses an agent without one")
	}
	if len(in) > MaxReadRepos {
		return nil, fmt.Errorf("read_repos lists %d repositories, over the cap of %d", len(in), MaxReadRepos)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, r := range in {
		r = strings.TrimSpace(r)
		if !github.ValidRepoName(r) {
			return nil, fmt.Errorf("read_repos entry %q is not a GitHub owner/name", r)
		}
		lr := strings.ToLower(r)
		if strings.EqualFold(r, repo) {
			return nil, fmt.Errorf("read_repos entry %q is the officer's own `repo`; remove it", r)
		}
		if seen[lr] {
			return nil, fmt.Errorf("read_repos lists %q twice", r)
		}
		seen[lr] = true
		out = append(out, lr)
	}
	sort.Strings(out)
	return out, nil
}

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
// id that is not `<type>:<area>`, two officers whose slugs collide,
// a missing token, a base URL that is not http(s), a repo that is not a GitHub
// owner/name (github.ValidRepoName, the task repositories' own check), a bad
// read_repos (parseReadRepos). Instances always run on the
// claude-code kind; there is no kind field.
//
// namespacePrefix is the deployment's resolved MUSTER_AGENT_NAMESPACE_PREFIX:
// an officer whose highest instance name would not fit a namespace under it is
// refused here, at boot, rather than when its first instance is created.
func Parse(raw string, getenv func(string) string, namespacePrefix string) ([]Officer, error) {
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
		o, err := d.officer(getenv, MaxInstanceNameLen(namespacePrefix))
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

func (d declared) officer(getenv func(string) string, maxName int) (Officer, error) {
	o := Officer{
		ID:           strings.TrimSpace(d.ID),
		HQURL:        strings.TrimRight(strings.TrimSpace(d.HQURL), "/"),
		MaxInstances: d.MaxInstances,
		Owner:        d.Owner,
		CCAccount:    strings.TrimSpace(d.CCAccount),
		CCFailover:   d.CCFailover,
		Model:        strings.TrimSpace(d.Model),
		Repo:         strings.TrimSpace(d.Repo),
		IdleAfter:    DefaultIdleAfter,
		PollInterval: DefaultPollInterval,
	}
	if !idRE.MatchString(o.ID) {
		return Officer{}, fmt.Errorf("id %q is not <type>:<area> (lowercase letters, digits and single hyphens on each side)", d.ID)
	}
	u, err := url.Parse(o.HQURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Officer{}, fmt.Errorf("officer %s: hq_url %q is not an http(s) URL", o.ID, d.HQURL)
	}
	if u.User != nil {
		return Officer{}, fmt.Errorf("officer %s: hq_url must not carry credentials; the token comes from %s%s", o.ID, TokenEnvPrefix, TokenEnvSuffix(o.ID))
	}
	if o.Model != "" {
		if err := podagent.ValidModel(o.Model); err != nil {
			return Officer{}, fmt.Errorf("officer %s: %w", o.ID, err)
		}
	}
	hasLLM := len(d.LLM) > 0 && string(d.LLM) != "null"
	// A shipping officer may have `llm`: the LLM proxy serves only the ROUTINE
	// pool (llm needs tiers), and a routine pool never holds the ship tools,
	// gate or App token (Officer.Pool).
	if len(d.Tiers) > 0 && string(d.Tiers) != "null" {
		// 🔴 ONE SOURCE PER VALUE. With tiers, the judgement pool's model and
		// max are tiers.judgement's; a top-level one beside it would be a second
		// answer to the same question, so it is refused rather than ranked.
		if o.Model != "" {
			return Officer{}, fmt.Errorf("officer %s: with tiers, set the judgement pool's model as tiers.judgement.model; remove the top-level model", o.ID)
		}
		if d.MaxInstances != 0 {
			return Officer{}, fmt.Errorf("officer %s: with tiers, set the judgement pool's size as tiers.judgement.max_instances; remove the top-level max_instances", o.ID)
		}
		// 🔴 A TIERED SHIPPING OFFICER SHIPS FROM ITS JUDGEMENT POOL ONLY.
		// Officer.Pool(TierRoutine) has no Ship and no CILogs, so a routine
		// instance gets no ship tool, no push gate and no officer App token:
		// it is an ordinary read-only officer instance.
		t, err := parseTiers(d.Tiers, hasLLM)
		if err != nil {
			return Officer{}, fmt.Errorf("officer %s: %w", o.ID, err)
		}
		o.Tiers, o.Tier = t, TierJudgement
		o.Model, o.MaxInstances = t.Judgement.Model, t.Judgement.MaxInstances
	}
	if o.Repo != "" && !github.ValidRepoName(o.Repo) {
		return Officer{}, fmt.Errorf("officer %s: repo %q is not a GitHub owner/name", o.ID, d.Repo)
	}
	readRepos, err := parseReadRepos(o.Repo, d.ReadRepos)
	if err != nil {
		return Officer{}, fmt.Errorf("officer %s: %w", o.ID, err)
	}
	o.ReadRepos = readRepos
	if d.PinReads {
		// 🔴 A PINNED OFFICER NEEDS repo: the App mints only for an agent with
		// a repository, so without one every pinned read would fail with the
		// App's error, and the officer would look pinned while reading nothing.
		if o.Repo == "" {
			return Officer{}, fmt.Errorf("officer %s: pin_repo_reads_to_item needs `repo`: the pinned reads go through the GitHub App, which serves an officer with a repository only", o.ID)
		}
		if len(d.Ship) > 0 && string(d.Ship) != "null" {
			return Officer{}, fmt.Errorf("officer %s: pin_repo_reads_to_item is refused beside `ship`: a shipping officer's requests are its push gate's, which a pinned read cannot narrow", o.ID)
		}
		o.PinRepoReadsToItem = true
	}
	seenConn := map[string]bool{}
	for _, c := range d.Connectors {
		c = strings.TrimSpace(c)
		if !connectorNameRE.MatchString(c) {
			return Officer{}, fmt.Errorf("officer %s: connector %q is not a connector name (lowercase letters, digits and '-', at most 63)", o.ID, c)
		}
		if c == GitHubConnector {
			return Officer{}, fmt.Errorf("officer %s: connectors lists %q, which every officer may already request; remove it", o.ID, c)
		}
		if c == KubeConnector {
			return Officer{}, fmt.Errorf("officer %s: connectors lists %q, which only a `kube` block grants (to the judgement pool); remove it", o.ID, c)
		}
		if !seenConn[c] {
			seenConn[c] = true
			o.Connectors = append(o.Connectors, c)
		}
	}
	sort.Strings(o.Connectors)
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
	for _, p := range o.Pools() {
		if n := p.InstanceName(p.MaxInstances); len(n) > maxName {
			return Officer{}, fmt.Errorf("officer %s: its instance agent name %q is %d characters, over %d (it becomes a namespace name under the deployment's prefix, and a namespace is at most %d)",
				o.ID, n, len(n), maxName, MaxNamespaceLen)
		}
	}
	if len(d.Ship) > 0 && string(d.Ship) != "null" {
		// read_repos beside ship: a READ to one of them is admitted by the push
		// gate (it admits no write outside the ship repositories) and served
		// with the DEPLOYMENT App's read-only token for that one repository
		// (PushGate.ReadCred), never the officer App's; with no deployment App
		// muster-server refuses to boot (config.validateShipReadRepos). A
		// read_repos entry that is also a ship repository is refused below: it
		// would have two answers.
		ship, err := parseShip(o.ID, o.Repo, d.Ship, getenv)
		if err != nil {
			return Officer{}, fmt.Errorf("officer %s: %w", o.ID, err)
		}
		for _, rr := range o.ReadRepos {
			if _, dup := ship.RepoFor(o.Repo, rr); dup {
				return Officer{}, fmt.Errorf("officer %s: read_repos entry %q is one of ship.repos, which the officer already reads; remove it", o.ID, rr)
			}
		}
		o.Ship = ship
	}
	if len(d.CILogs) > 0 && string(d.CILogs) != "null" {
		ci, err := parseCILogs(o.Ship, d.CILogs)
		if err != nil {
			return Officer{}, fmt.Errorf("officer %s: %w", o.ID, err)
		}
		o.CILogs = ci
	}
	if len(d.Transcript) > 0 && string(d.Transcript) != "null" {
		tr, err := parseTranscript(d.Transcript)
		if err != nil {
			return Officer{}, fmt.Errorf("officer %s: %w", o.ID, err)
		}
		o.Transcript = tr
	}
	if len(d.Kube) > 0 && string(d.Kube) != "null" {
		k, err := parseKube(d.Kube, o.Tiers != nil, getenv)
		if err != nil {
			return Officer{}, fmt.Errorf("officer %s: %w", o.ID, err)
		}
		o.Kube = k
	}
	if hasLLM {
		l, err := parseLLM(o, d.LLM, getenv)
		if err != nil {
			return Officer{}, fmt.Errorf("officer %s: %w", o.ID, err)
		}
		o.LLM = l
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
// instance of, and which instance (1..its pool's MaxInstances). It is THE
// binding rule: the MCP tools act for the officer this returns and no other,
// and an agent whose name it does not parse is not an officer instance at all.
//
// The officer returned is the instance's POOL (Officer.Pool): for a tiered
// officer's routine instance, Tier routine with the routine model and max; for
// an untiered officer, the officer itself, as before.
func (r *Registry) ForAgentName(name string) (Officer, int, bool) {
	if r == nil {
		return Officer{}, 0, false
	}
	for _, o := range r.list {
		if p, n, ok := parseInstance(o, name); ok {
			return p, n, true
		}
	}
	return Officer{}, 0, false
}

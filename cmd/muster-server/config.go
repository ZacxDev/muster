package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
)

// defaultPort is the port this service listens on when MUSTER_PORT is unset.
//
// ⚠ IT IS NOT 8104. That is the permission router's port, and the two services
// are expected to run side by side on the same host during the extraction — a
// shared default would make "which process is answering" a question rather than
// a fact.
const defaultPort = 8105

// Environment variable names.
//
// 🔴 EVERY NAME IS SPELLED EXACTLY ONCE, HERE, AND THAT IS NOT TIDINESS. The
// finding this file answers is that internal/api and internal/ui document a
// dozen MUSTER_* variables in their comments and NOTHING READ ANY OF THEM —
// there was no binary to read them. A second spelling in a wiring call is how
// that state comes back one variable at a time: the comment says one name, the
// code reads another, and the operator sets the one the comment names.
//
// 🔴 THE CITATION THAT USED TO CLOSE THIS PARAGRAPH NAMED A GUARD THAT WAS
// NEVER WRITTEN. It read "envNamesAreDocumented asserts these constants against
// the documentation"; enumerated over the module, that identifier had exactly
// one occurrence — the sentence itself. It slipped
// TestEveryCitedTestExistsOrIsLedgered because that gate resolves `Test[A-Z]…`
// names only, and a lower-case one is invisible to it. Cite a guard by its
// `Test*` name, which is the spelling the gate can check; see the KNOWN LIMIT
// banner in internal/modulegate/citations_test.go.
//
// What actually holds this paragraph up is
// TestEveryServerEnvNameIsSpelledOnceAndRead (config_env_names_test.go): every
// environment-variable literal in this binary's sources is one of the constants
// below, and every constant below is read somewhere outside this block.
const (
	envPort     = "MUSTER_PORT"
	envDatabase = "DATABASE_URL"

	envUIPassword    = "MUSTER_UI_PASSWORD"
	envHookToken     = "MUSTER_HOOK_TOKEN"
	envServiceToken  = "MUSTER_SERVICE_TOKEN"
	envSecureCookies = "MUSTER_SECURE_COOKIES"

	envGitHubToken     = "MUSTER_GITHUB_TOKEN"
	envGitHubEncKey    = "MUSTER_GITHUB_ENCRYPTION_KEY"
	envPublicURL       = "MUSTER_PUBLIC_URL"
	envGitHubClientID  = "GITHUB_CLIENT_ID"
	envGitHubClientSec = "GITHUB_CLIENT_SECRET"

	envFaroURL     = "MUSTER_FARO_URL"
	envFaroKey     = "MUSTER_FARO_API_KEY"
	envFaroTracing = "MUSTER_FARO_TRACING"

	envTagAutoDispatch = "MUSTER_TAG_AUTODISPATCH"
	envTaskReapAfter   = "MUSTER_TASK_REAP_AFTER"

	envRouterURL   = "MUSTER_ROUTER_URL"
	envRouterToken = "MUSTER_ROUTER_TOKEN"
	envRouterActor = "MUSTER_ROUTER_ACTOR"

	envStandalone = "MUSTER_STANDALONE"

	// --- agent provisioning (see provisioner.go) ---
	envAgentProvisioner   = "MUSTER_AGENT_PROVISIONER"
	envAgentImageRepo     = "MUSTER_AGENT_IMAGE_REPO"
	envAgentImageTag      = "MUSTER_AGENT_IMAGE_TAG"
	envAgentAPIURL        = "MUSTER_AGENT_API_URL"
	envAgentModel         = "MUSTER_AGENT_MODEL"
	envAgentOpenRouterKey = "MUSTER_AGENT_OPENROUTER_KEY"
	envAgentNamespace     = "MUSTER_AGENT_NAMESPACE"
	envAgentNSPrefix      = "MUSTER_AGENT_NAMESPACE_PREFIX"
	envAgentNSShared      = "MUSTER_AGENT_NAMESPACE_SHARED"
	envAgentWorkspaceKeep = "MUSTER_AGENT_WORKSPACE_PERSIST"
	envAgentStorageClass  = "MUSTER_AGENT_WORKSPACE_STORAGE_CLASS"
	envAgentEndpointTmpl  = "MUSTER_AGENT_ENDPOINT_TEMPLATE"
)

// The values MUSTER_AGENT_PROVISIONER accepts.
//
// 🔴 THE DEFAULT IS none, AND IT HAS TO BE. Every previous revision of this
// binary had NO provisioner at all, so a default that provisioned would change
// what an unchanged deployment does on the next image bump — and the thing it
// would change is "create pods in a cluster". Opting in is a deliberate act.
const (
	provisionerNone = "none"
	provisionerNoop = "noop"
	provisionerK8s  = "kubernetes"
)

// provisionerChoices is every legal value, for the refusal message and for the
// guard that pins the set.
var provisionerChoices = []string{provisionerNone, provisionerNoop, provisionerK8s}

// agentProvisioner resolves the empty value to provisionerNone.
//
// 🔴 IT EXISTS BECAUSE buildApp ACCEPTS A config THAT DID NOT COME FROM
// loadConfig, AND EVERY WIRING TEST IS ONE. loadConfig normalises "" to
// provisionerNone; a config value constructed directly does not, and the first
// draft of the wiring compared the raw field against provisionerNone — so a
// zero-value config never reached the "none" branch at all.
//
// FIVE pre-existing boot tests went red on it, on TWO different branches, and
// that is the only reason it was found: TestReadyzRefusesAnUndeclaredMissingRouter
// and TestReadyzAcceptsADeclaredStandaloneDeployment fell through to the
// "unhandled provisioner name" refusal in buildDriver, while
// TestTheServerListensAndServesHealth,
// TestTheServerServesTheDocumentRootThroughTheUILayer and
// TestTheServerServesTheEmbeddedStylesheet hit the nil-agents-store refusal
// instead, because they configure no database. ⚠ THE COUNT IN THIS COMMENT READ
// "THREE" AND WAS MEASURED WRONG — it was five, and naming only one branch would
// have sent the next reader looking for a single cause. Nothing about a zero-value
// struct announces which of its fields were meant to be defaulted elsewhere.
// missingRouterCredential's own doc records the same property of buildApp for the
// router actor.
func (c config) agentProvisioner() string {
	if c.AgentProvisioner == "" {
		return provisionerNone
	}
	return c.AgentProvisioner
}

// defaultRouterActor is the name this service asserts to the permission router
// when MUSTER_ROUTER_ACTOR is unset. The router validates it as an actor label.
const defaultRouterActor = "muster"

// config is everything this binary reads from the environment, resolved once.
//
// 🔴 IT IS A VALUE, NOT A PILE OF os.Getenv CALLS SCATTERED THROUGH main(), SO
// THE WIRING IS TESTABLE WITHOUT AN ENVIRONMENT. buildApp takes one of these;
// every test below constructs one directly. A wiring bug found by a test that
// had to mutate the process environment is a test that cannot run in parallel
// with any other.
type config struct {
	Port     int
	Database string

	UIPassword    string
	HookToken     string
	ServiceToken  string
	SecureCookies bool

	GitHubToken        string
	GitHubEncKey       []byte
	PublicURL          string
	GitHubClientID     string
	GitHubClientSecret string

	FaroURL     string
	FaroKey     string
	FaroTracing bool

	TagAutoDispatch bool
	TaskReapAfter   time.Duration

	RouterURL   string
	RouterToken string
	RouterActor string

	// Standalone is the operator's EXPLICIT declaration that this deployment has
	// no permission router behind it, and therefore no session transcripts.
	//
	// 🔴 IT EXISTS BECAUSE "NO ROUTER CONFIGURED" AND "FORGOT TO CONFIGURE THE
	// ROUTER" ARE THE SAME OBSERVABLE, AND THEY MUST NOT GET THE SAME
	// BEHAVIOUR. api.Extensions.defects treats a notes store with no session
	// liveness probe as a READINESS DEFECT precisely because the degraded
	// rendering ("no transcript recorded") is indistinguishable from the truth —
	// it is a page that states a falsehood, silently, on every row. This flag is
	// how an operator says the sentence is TRUE here: there is no session store,
	// so no transcript is recorded, and the page is correct.
	//
	// ⚠ SETTING IT DOES NOT MAKE THE SERVER STANDALONE. It records a claim about
	// the deployment. If a router IS configured, the router's probe wins and this
	// flag changes nothing — see buildSessionLiveness.
	Standalone bool

	// AgentProvisioner names the provisioning backend: one of
	// provisionerChoices. Default provisionerNone, which leaves
	// api.Extensions.Provisioner nil and every lifecycle route refusing exactly
	// as it did before this knob existed.
	AgentProvisioner string

	// AgentImageRepo and AgentAPIURL are agentspec.Config's two required fields.
	// Required whenever AgentProvisioner is not none, and refused at boot rather
	// than at the first dispatch — see validate.
	AgentImageRepo string
	AgentAPIURL    string

	AgentImageTag      string
	AgentModel         string
	AgentOpenRouterKey string

	// AgentNamespaceShared puts every instance in AgentNamespace instead of
	// giving each its own.
	//
	// 🔴 IT IS PHRASED AS THE NON-DEFAULT, WHICH IS THE OPPOSITE OF THE DRIVER'S
	// OWN FLAG, AND THAT IS DELIBERATE. k8s.Config.NamespacePerInstance defaults
	// FALSE, so a driver built from a zero config quietly shares one namespace —
	// while the row this service writes says `devpod-<name>`, a namespace that
	// would then hold nothing. Defaulting the deployment to per-instance keeps
	// the row and the cluster agreeing; an operator who wants the shared layout
	// says so and supplies the namespace.
	AgentNamespaceShared bool
	AgentNamespace       string
	AgentNamespacePrefix string

	// AgentWorkspacePersist asks for a workspace that survives a restart.
	//
	// ⚠ IT IS ALSO WHAT DECIDES WHETHER AgentStorageClass IS SENT AT ALL, because
	// the driver's WorkspaceStorageClass is a *string whose three states are nil
	// (refuse persistence), "" (the cluster's default class) and a name — and an
	// environment variable cannot tell unset from empty. So: persist off -> nil;
	// persist on -> a pointer to whatever the class variable holds, empty
	// included.
	AgentWorkspacePersist bool
	AgentStorageClass     string

	AgentEndpointTemplate string
}

// envFlag reads a boolean env switch. Anything other than an explicit
// affirmative is FALSE — an unset, empty or misspelled value must never arm
// something, and a permissive parse is how a typo turns into an armed surface.
func envFlag(getenv func(string) string, name string) bool {
	switch strings.ToLower(strings.TrimSpace(getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// loadConfig resolves the whole configuration from getenv.
//
// ⚠ IT DOES NOT VALIDATE. Parsing and refusing are separate so a test can build
// an intentionally incomplete config and assert on the refusal text, and so a
// single malformed value reports its own name rather than the first `Fatalf`
// the wiring happens to reach. See config.validate.
func loadConfig(getenv func(string) string) (config, error) {
	c := config{
		Port:               defaultPort,
		Database:           strings.TrimSpace(getenv(envDatabase)),
		UIPassword:         getenv(envUIPassword),
		HookToken:          getenv(envHookToken),
		ServiceToken:       getenv(envServiceToken),
		SecureCookies:      envFlag(getenv, envSecureCookies),
		GitHubToken:        strings.TrimSpace(getenv(envGitHubToken)),
		PublicURL:          strings.TrimSpace(getenv(envPublicURL)),
		GitHubClientID:     strings.TrimSpace(getenv(envGitHubClientID)),
		GitHubClientSecret: strings.TrimSpace(getenv(envGitHubClientSec)),
		FaroURL:            strings.TrimSpace(getenv(envFaroURL)),
		FaroKey:            strings.TrimSpace(getenv(envFaroKey)),
		FaroTracing:        envFlag(getenv, envFaroTracing),
		TagAutoDispatch:    envFlag(getenv, envTagAutoDispatch),
		RouterURL:          strings.TrimSpace(getenv(envRouterURL)),
		RouterToken:        strings.TrimSpace(getenv(envRouterToken)),
		RouterActor:        strings.TrimSpace(getenv(envRouterActor)),
		Standalone:         envFlag(getenv, envStandalone),

		AgentProvisioner:      strings.ToLower(strings.TrimSpace(getenv(envAgentProvisioner))),
		AgentImageRepo:        strings.TrimSpace(getenv(envAgentImageRepo)),
		AgentImageTag:         strings.TrimSpace(getenv(envAgentImageTag)),
		AgentAPIURL:           strings.TrimSpace(getenv(envAgentAPIURL)),
		AgentModel:            strings.TrimSpace(getenv(envAgentModel)),
		AgentOpenRouterKey:    strings.TrimSpace(getenv(envAgentOpenRouterKey)),
		AgentNamespaceShared:  envFlag(getenv, envAgentNSShared),
		AgentNamespace:        strings.TrimSpace(getenv(envAgentNamespace)),
		AgentNamespacePrefix:  strings.TrimSpace(getenv(envAgentNSPrefix)),
		AgentWorkspacePersist: envFlag(getenv, envAgentWorkspaceKeep),
		AgentStorageClass:     strings.TrimSpace(getenv(envAgentStorageClass)),
		AgentEndpointTemplate: strings.TrimSpace(getenv(envAgentEndpointTmpl)),
	}
	if c.RouterActor == "" {
		c.RouterActor = defaultRouterActor
	}
	if c.AgentProvisioner == "" {
		c.AgentProvisioner = provisionerNone
	}
	if c.AgentNamespacePrefix == "" {
		// 🔴 THE DEFAULT IS THE STORE'S OWN CONSTANT, NOT A COPY OF ITS VALUE.
		// api writes agents.NamespaceFor(name) into the row; this is the other
		// side of that pair, and a second literal here is how the two drift into
		// a namespace the card names and nothing exists in.
		c.AgentNamespacePrefix = agents.NamespacePrefix
	}

	if v := strings.TrimSpace(getenv(envPort)); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 65535 {
			return config{}, fmt.Errorf("invalid %s %q: must be an integer 1-65535", envPort, v)
		}
		c.Port = p
	}

	if v := strings.TrimSpace(getenv(envTaskReapAfter)); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return config{}, fmt.Errorf("invalid %s %q: %w (want a Go duration, e.g. 168h)", envTaskReapAfter, v, err)
		}
		if d <= 0 {
			return config{}, fmt.Errorf("invalid %s %q: must be positive", envTaskReapAfter, v)
		}
		c.TaskReapAfter = d
	}

	if v := strings.TrimSpace(getenv(envGitHubEncKey)); v != "" {
		key, err := decodeEncryptionKey(v)
		if err != nil {
			return config{}, fmt.Errorf("invalid %s: %w", envGitHubEncKey, err)
		}
		c.GitHubEncKey = key
	}

	return c, nil
}

// decodeEncryptionKey decodes the 32-byte AES key the GitHub token is encrypted
// at rest with. Standard or raw base64, padded or not.
//
// 🔴 THERE IS NO "GENERATE ONE IF UNSET" BRANCH, AND ITS ABSENCE IS THE POINT.
// An ephemeral key would make every previously stored GitHub token undecryptable
// on the next restart, which presents as "the Repos tab forgot my account" with
// nothing in the log — the failure is a DATA loss dressed as a UI bug. Unset
// means the GitHub store is not built at all, which is visible on screen.
func decodeEncryptionKey(v string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(v); err == nil {
			if len(b) != 32 {
				return nil, fmt.Errorf("decodes to %d bytes, want 32 (AES-256)", len(b))
			}
			return b, nil
		}
	}
	return nil, errors.New("not valid base64 (want 32 random bytes, base64-encoded)")
}

// validate refuses a configuration this binary must not serve behind.
//
// 🔴 DATABASE_URL IS REQUIRED, AND THE PRECEDENT THAT MAKES IT OPTIONAL
// ELSEWHERE DOES NOT APPLY HERE. The permission router keeps an in-memory
// fallback because its core object — a pending approval request — is ephemeral
// by nature and a database-less run is a real, useful mode. muster's core object
// is a TASK, and a task board that came up empty because no database was
// configured renders exactly like a task board with no tasks: an empty list, no
// error, nothing logged, nothing for an operator to notice. That is the "lies
// silently" side of the line api.Extensions.defects draws, so it is refused at
// boot rather than served.
//
// ⚠ buildApp DOES accept a config with no DSN, and that is deliberate rather
// than an inconsistency: the assembly path and the deployment policy are
// separate claims, and the boot test needs the first without the second. The
// policy is HERE, it is tested, and main() applies it.
func (c config) validate() error {
	if c.Database == "" {
		return fmt.Errorf("%s is not set: muster is its Postgres database, and a server "+
			"started without one serves an EMPTY board that is indistinguishable from a "+
			"board with no tasks — no error, nothing logged. Set %s", envDatabase, envDatabase)
	}
	if c.Standalone && c.RouterURL != "" {
		return fmt.Errorf("%s=1 and %s are both set: they are contradictory claims about "+
			"whether a permission router exists. Unset one", envStandalone, envRouterURL)
	}
	return c.validateProvisioner()
}

// validateProvisioner refuses an agent-provisioning configuration that could
// only fail later.
//
// 🔴 IT IS A SEPARATE FUNCTION CALLED FROM validate, NOT A BLOCK BESIDE THE
// ROUTER CHECK, FOR THE REASON THE ROUTER CHECK ITSELF RECORDS: MUSTER_STANDALONE
// and MUSTER_ROUTER_URL are mutually exclusive and validate is where that is
// enforced, so a new knob added *beside* the existing checks rather than checked
// *against* them is how a contradictory pair ships unnoticed. Nothing here
// interacts with the router pair — stated because "I checked" and "there was
// nothing to check" are different claims and only one of them is worth writing.
//
// ⚠ EVERY REFUSAL HERE IS ABOUT A COMBINATION THAT WOULD OTHERWISE FAIL AT THE
// FIRST DISPATCH, IN A BACKGROUND GOROUTINE, WHOSE ONLY OUTPUT IS A LOG LINE the
// user who clicked dispatch never sees. That is the whole argument for boot-time
// refusal: the same misconfiguration is either one fatal line at start or a card
// that silently never provisions.
func (c config) validateProvisioner() error {
	named := c.agentProvisioner()
	legal := false
	for _, choice := range provisionerChoices {
		if named == choice {
			legal = true
			break
		}
	}
	if !legal {
		return fmt.Errorf("invalid %s %q: want one of %s", envAgentProvisioner,
			c.AgentProvisioner, strings.Join(provisionerChoices, ", "))
	}
	if named == provisionerNone {
		// Nothing else is read in this mode, so nothing else is refused. An
		// operator who set the image repository and forgot to name a driver is
		// covered by the banner, which says the seam is open.
		return nil
	}
	if c.AgentImageRepo == "" {
		return fmt.Errorf("%s=%s but %s is not set: an agent instance needs a runtime image and "+
			"there is no defensible default for an installation-specific registry. "+
			"agentspec.Build refuses this too, but it refuses inside a dispatch goroutine whose "+
			"only output is a log line", envAgentProvisioner, named, envAgentImageRepo)
	}
	if c.AgentAPIURL == "" {
		return fmt.Errorf("%s=%s but %s is not set: an instance with no base URL cannot reach "+
			"this server, so it would come up healthy and report nothing — the failure would look "+
			"like an idle agent", envAgentProvisioner, named, envAgentAPIURL)
	}
	if named == provisionerK8s && c.AgentNamespaceShared && c.AgentNamespace == "" {
		return fmt.Errorf("%s=1 but %s is not set: the shared layout puts every instance in one "+
			"named namespace and the kubernetes driver requires it. Either name the namespace or "+
			"unset %s and let each instance have its own (%s%s)",
			envAgentNSShared, envAgentNamespace, envAgentNSShared, c.AgentNamespacePrefix, "<name>")
	}
	// ⚠ THERE IS DELIBERATELY NO REFUSAL FOR MUSTER_AGENT_WORKSPACE_PERSIST, and
	// the first draft of this function had one — "the noop driver has no storage
	// to persist to". Measured against provision.DefaultNoopCapabilities, which
	// declares Persistence TRUE: the noop driver accepts a persistent workspace
	// and records it. The refusal would have rejected a working configuration
	// while reading like a safety check.
	return nil
}

// osGetenv is os.Getenv as a value, so main() can hand loadConfig the real
// environment and tests can hand it a map.
func osGetenv(name string) string { return os.Getenv(name) }

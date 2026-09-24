package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
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
)

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
	}
	if c.RouterActor == "" {
		c.RouterActor = defaultRouterActor
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
	return nil
}

// osGetenv is os.Getenv as a value, so main() can hand loadConfig the real
// environment and tests can hand it a map.
func osGetenv(name string) string { return os.Getenv(name) }

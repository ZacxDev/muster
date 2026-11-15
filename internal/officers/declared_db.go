package officers

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// DB-DECLARED OFFICERS (migration 0135; docs/officers-builtin.md "Declared in
// the database")
//
// An officer can be declared in muster's database instead of MUSTER_OFFICERS:
// created, changed, paused and deleted through the admin API with no restart.
// Its runtime fields live on its officer_defs row; its type and def stay its
// charter source, as for any builtin officer.
//
// 🔴 ONE VALIDATION. A Declaration becomes an Officer through the SAME
// declared.officer every MUSTER_OFFICERS entry goes through — the namespace
// length of its instance names, the host-runtime refusals, the repo, read_repos
// and connector rules — so the API refuses exactly what the boot would. Only
// what a declaration cannot say is fixed here: the backend is builtin, and
// there is no tiers, ship, transcript, llm, kube, image or token (the
// once-built machinery EnableOfficers wires from the environment).
// ---------------------------------------------------------------------------

// RuntimeAuto is a DB-declared officer's "pick for me" runtime: a pod when
// this server can run one, else the first of its owner's hosts that qualifies
// (internal/api resolves it on every roster load, and says why when nothing
// qualifies). Never a MUSTER_OFFICERS value.
const RuntimeAuto = "auto"

// Declaration is a DB-declared officer's runtime fields, as officer_defs
// stores them (0135).
type Declaration struct {
	ID    string
	Owner int64
	// Runtime is RuntimePod, RuntimeHost or RuntimeAuto.
	Runtime string
	// Host is the mirror host of a RuntimeHost officer; "" otherwise.
	Host string
	// Repo is the officer's own repository ("" for none).
	Repo string
	// Connectors are the connectors besides github its instances may request.
	Connectors []string
	// ReadRepos are public repositories its instances read with no approval
	// (the standing public read, Officer.StandingReadRepos).
	ReadRepos []string
	// MaxInstances, IdleAfter and PollInterval are 0 for the defaults.
	MaxInstances int
	IdleAfter    time.Duration
	PollInterval time.Duration
	// Model is the pod's model ("" for the account default).
	Model string
	// InstanceSlug is the server-allocated agent slug (AllocateSlug).
	InstanceSlug string
}

// Officer is d as the officer it declares, with runtime and host the runtime
// it runs on (a RuntimeAuto declaration's resolution; for RuntimePod or
// RuntimeHost, d's own), validated by the one declaration check
// (declared.officer). namespacePrefix is the deployment's
// MUSTER_AGENT_NAMESPACE_PREFIX: the instance names must fit a namespace
// under it. A refusal carries its fields (Fields).
func (d Declaration) Officer(runtime, host, namespacePrefix string) (Officer, error) {
	if d.InstanceSlug == "" {
		return Officer{}, fieldErr("id", errors.New("no instance slug was allocated"))
	}
	if runtime == RuntimeAuto || runtime == "" {
		return Officer{}, fieldErr("runtime", fmt.Errorf("runtime %q must be resolved to %q or %q", runtime, RuntimePod, RuntimeHost))
	}
	dd := declared{
		ID:           d.ID,
		MaxInstances: d.MaxInstances,
		Owner:        d.Owner,
		Model:        d.Model,
		Repo:         d.Repo,
		ReadRepos:    d.ReadRepos,
		Connectors:   d.Connectors,
		Backend:      BackendBuiltin,
		Runtime:      runtime,
		Host:         host,
		InstanceSlug: d.InstanceSlug,
	}
	if len(d.ReadRepos) > 0 {
		// A DB officer's read_repos are its standing public read: it holds no
		// App grant of its own to approve them under.
		dd.ReadGrant = ReadGrantStanding
	}
	if d.IdleAfter != 0 {
		dd.IdleAfter = d.IdleAfter.String()
	}
	if d.PollInterval != 0 {
		dd.PollInterval = d.PollInterval.String()
	}
	if d.Owner <= 0 {
		return Officer{}, fieldErr("owner", fmt.Errorf("owner %d is not a user id", d.Owner))
	}
	// getenv is empty: a DB officer reads nothing from the environment (no
	// token, no key, no sealed cluster), so nothing there can change it.
	return dd.officer(func(string) string { return "" }, MaxInstanceNameLen(namespacePrefix), nil)
}

// Validate checks d as declared: a RuntimeAuto declaration must be runnable
// on at least one runtime (a pod, or a host — checked with a placeholder host
// name, since auto picks the host), and its own host must be empty.
func (d Declaration) Validate(namespacePrefix string) error {
	switch d.Runtime {
	case RuntimePod, RuntimeHost:
		_, err := d.Officer(d.Runtime, d.Host, namespacePrefix)
		return err
	case RuntimeAuto:
		if strings.TrimSpace(d.Host) != "" {
			return fieldErr("host", fmt.Errorf("host %q is chosen by muster for \"runtime\": %q; leave it out, or say \"runtime\": %q", d.Host, RuntimeAuto, RuntimeHost))
		}
		_, podErr := d.Officer(RuntimePod, "", namespacePrefix)
		if podErr == nil {
			return nil
		}
		if _, hostErr := d.Officer(RuntimeHost, autoProbeHost, namespacePrefix); hostErr == nil {
			return nil
		}
		return podErr
	}
	return fieldErr("runtime", fmt.Errorf("runtime %q is not %q, %q or %q", d.Runtime, RuntimePod, RuntimeHost, RuntimeAuto))
}

// autoProbeHost is a valid host name Validate checks an auto declaration's
// host form with.
const autoProbeHost = "probe"

// instanceSlugRE is an instance slug: what an id's Slug can be, so its agent names
// stay DNS labels.
var instanceSlugRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// MaxSlugLen bounds an instance slug: officer_defs' CHECK (0135) holds it too.
const MaxSlugLen = 54

// ValidSlug reports whether s may be an instance slug.
func ValidSlug(s string) bool { return len(s) <= MaxSlugLen && instanceSlugRE.MatchString(s) }

// SlugCandidates are the slugs AllocateSlug tries for id, in order: Slug(id),
// then Slug(id)-2 … Slug(id)-9, each cut to fit maxLen. A slug equal to an env
// officer's, or one another owner holds (the 0135 unique index refuses it),
// is passed over.
func SlugCandidates(id string, maxLen int) []string {
	if maxLen > MaxSlugLen {
		maxLen = MaxSlugLen
	}
	base := Slug(id)
	var out []string
	for n := 1; n <= 9; n++ {
		sfx := ""
		if n > 1 {
			sfx = "-" + strconv.Itoa(n)
		}
		b := base
		if len(b)+len(sfx) > maxLen {
			b = strings.TrimRight(b[:max(0, maxLen-len(sfx))], "-")
		}
		if c := b + sfx; ValidSlug(c) {
			out = append(out, c)
		}
	}
	return out
}

// SlugMaxFor is the longest instance slug whose highest instance name
// (`officer-<slug>-<maxInstances>`) fits a namespace under namespacePrefix.
func SlugMaxFor(namespacePrefix string, maxInstances int) int {
	if maxInstances <= 0 {
		maxInstances = DefaultMaxInstances
	}
	return MaxInstanceNameLen(namespacePrefix) - len(InstanceNamePrefix) - 1 - len(strconv.Itoa(maxInstances))
}

// TypeOf is an officer id's type: the part before the colon.
func TypeOf(id string) string {
	t, _, _ := strings.Cut(id, ":")
	return t
}

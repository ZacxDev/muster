// Package agentspec builds a [provision.Spec] from an agent row.
//
// It is plan step 22a of the extraction this project came out of (Phase 3.5). It is
// the FIRST of the three pieces that close doc_seams.go entry 1, and it is
// deliberately the piece that touches neither the provisioner wrappers nor
// the chat path: this package has no HTTP client, no cluster client, and no
// knowledge of any driver.
//
// # What it replaces, and why the shape changed
//
// Upstream, the equivalent code was buildHelmValues — a 428-line function
// returning map[string]any shaped for one vendored Helm chart, supported by
// ~1,000 further lines of helpers. Its output was only meaningful to that
// chart's templates, which is the coupling [provision.Spec] exists to remove.
//
// 🔴 THE SINGLE BIGGEST CHANGE IS THAT FILES ARE DATA. Upstream, every file
// placed into an agent travelled as
//
//	{ printf '%s' '<base64>' | base64 -d > /path; } || :
//
// appended to one giant extraInitCommands string. The `|| :` was not caution: the
// chart's startup script ran under `set -e`, so a bare non-zero exit was a crash
// loop rather than a missing file — and an audit found three shapes where the
// guard had been missed. Every file this package emits is a [provision.File]
// instead, so the driver places it natively and a test can assert the CONTENT
// rather than parsing a shell string. See [provision.File]'s own doc comment.
//
// 🔴 A COROLLARY WORTH STATING: Init survives, and it is not a dumping ground.
// A step belongs there only if it is genuinely imperative — something that must
// RUN, not something that must EXIST. Placing content in Init is how the upstream
// string grew; this package emits exactly one Init entry and argues for it below.
//
// # What this package deliberately does NOT do
//
// Each of these is a named part of another step, not an oversight. They are
// listed here because a reader's first question is "where did the rest of
// buildHelmValues go", and a missing answer reads as a gap.
//
//   - It does not generate INSTRUCTION CONTENT. Upstream that was skill.go's
//     instructionsFor/skillsFor (902 lines), which plan step 23 deletes and
//     ranked item 3 ports. This package takes the rendered text as an input
//     ([Options.Instructions]) and places it as a file. That is the right seam:
//     the mechanism and the prose have different owners and different review
//     needs.
//   - ⚠ IT DOES TOUCH CAIRN NOW, AND THIS BULLET USED TO SAY IT DID NOT. Ranked
//     item 3 ("port the chief→cairn integration") has landed: see cairn.go for
//     the client install, the credential file and the one gate that decides
//     whether either exists, and [ChiefInstructions] for the prose half. What is
//     still true is the SHAPE the old bullet was defending — the coordinates are
//     [Config] fields, eligibility is an [Options] input, and nothing in this
//     package compares an agent name against a constant to decide who gets a
//     credential. Both-unset emits nothing at all, which is the state an
//     installation that has not configured a store runs in.
//   - It does not build the GATEWAY wiring — the bearer derivation, the chat
//     endpoint, the responses model sentinel. That is plan step 22c, and
//     internal/agents/responses.go already carries its own OWED record naming
//     the two inputs a caller must supply.
//   - It does not ADAPT anything to [api.Provisioner]. That is plan step 22b.
//     Nothing here implements a lifecycle method; Build is a pure function.
//
// # Purity, and what it buys
//
// Build reads no environment, no clock, no filesystem and no network. Every
// input arrives as an argument, so the same inputs always produce the same
// [provision.Spec] — which is what makes the golden in agentspec_test.go a
// meaningful pin rather than a snapshot of one machine. If you find yourself
// wanting os.Getenv here, put it in [Config] instead and let the binary resolve
// it.
package agentspec

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// DefaultWorkspacePath is where an agent's working files live when [Config] does
// not say otherwise. It matches what the upstream chart used, because the agent
// runtime images already expect to find a workspace there.
const DefaultWorkspacePath = "/data/workspace"

// DefaultImageTag is used when [Config.ImageTag] is empty.
//
// ⚠ IT IS "latest", AND THAT IS A DEFAULT RATHER THAN A RECOMMENDATION. A
// mutable tag means two instances created a week apart can run different code
// with identical specs, which is exactly the ambiguity that made muster's own
// deployment pin a digest. Callers that care should set [Config.ImageTag]; this
// constant exists so that an unset field produces a runnable spec instead of an
// image reference ending in a bare colon.
const DefaultImageTag = "latest"

// Environment variable names handed to the instance.
//
// 🔴 THESE ARE NOT INVENTED HERE, AND THEY MUST NOT BE RENAMED IN THIS PACKAGE.
// They are the spellings cmd/muster (the in-pod CLI) already reads — see that
// package's envAPIURL/envToken and the comment there, which names the pod case
// explicitly: "Inside a managed AGENT POD it is that pod's OWN per-agent token".
// A rename here would leave the CLI reading names nothing sets, and the failure
// would look like a broken agent rather than a broken spec.
//
// ⚠ THERE IS ONE BASE URL, NOT TWO, AND THAT IS A DELIBERATE NARROWING.
// Upstream injected a ROUTER-side base URL variable *and* a TASK-side one, because
// the permission router and the task board were separate services, and a cutover
// made those two values diverge. muster serves both route families from
// its own mux, so a second variable would have no different value to hold.
// cmd/muster/config.go makes the same argument and cites the test that checks it
// (TestEveryWiredRouteIsServedByThisProject).
const (
	// EnvAPIURL is the base URL the instance's CLI and skills talk to.
	EnvAPIURL = "MUSTER_API_URL"
	// EnvToken carries the agent's OWN per-agent token. It is a SECRET, and
	// Build puts it in [provision.Spec.Secrets] rather than Env for that
	// reason — see the note on Build.
	EnvToken = "MUSTER_HOOK_TOKEN"
	// EnvNodeOptions passes Node heap/flag tuning through to the runtime.
	EnvNodeOptions = "NODE_OPTIONS"
	// EnvGitTerminalPrompt is pinned to "0" unconditionally.
	//
	// 🔴 IT IS A HANG GUARD, NOT A PREFERENCE. Git prompts for credentials on a
	// terminal it believes is interactive. In a pod there is nobody to answer,
	// so a clone against a repository the instance cannot read BLOCKS instead of
	// failing — and a blocked init step is indistinguishable from a slow one
	// until the deadline fires. Failing fast is the whole point.
	EnvGitTerminalPrompt = "GIT_TERMINAL_PROMPT"
	// EnvOpenRouterKey is the shared model-provider credential. Also a secret.
	EnvOpenRouterKey = "OPENROUTER_API_KEY"
	// EnvCairnConfig points the subsystem-store client at [CairnConfigPath].
	//
	// 🔴 IT IS SET ONLY WHEN THE INTEGRATION IS ON, and it is NOT a secret: it
	// names a path, and the credential it names travels as a confidential
	// [provision.File]. It is in this block — and in buildEnv's reserved set —
	// because a caller shadowing it through [Options.ExtraEnv] would point the
	// client at a file muster did not write, which fails as "the store is
	// unreachable" rather than as a configuration mistake. See cairn.go.
	EnvCairnConfig = "CAIRN_CONFIG"
)

// InstructionsFileName is the file [Options.Instructions] is written to, inside
// the workspace.
const InstructionsFileName = "AGENTS.md"

// ConfigKeyModel is the one key Build writes into [provision.Spec.Config].
//
// ⚠ Spec.Config IS OPAQUE TO EVERY DRIVER by contract, so this key is a
// statement about the RUNTIME inside the instance, not about provisioning. It is
// named here so the runtime side and this side agree in one place rather than in
// two string literals.
const ConfigKeyModel = "model"

// Config is the deployment-wide part of a spec: everything that is true of every
// agent in this installation rather than of one agent row.
//
// ⚠ EVERY FIELD IS OPTIONAL EXCEPT ImageRepo AND APIBaseURL, and Build says so
// by returning an error rather than by producing a spec that fails later inside
// a driver. The two required ones are required because there is no defensible
// default: an image repository is installation-specific, and a base URL that
// guessed at a service name would produce an instance that cannot phone home.
type Config struct {
	// ImageRepo is the runtime image WITHOUT a tag, e.g.
	// "ghcr.io/example-org/agent-runtime". Required.
	ImageRepo string
	// ImageTag defaults to [DefaultImageTag].
	ImageTag string
	// APIBaseURL is what [EnvAPIURL] is set to. Required.
	APIBaseURL string
	// OpenRouterAPIKey, when set, is injected as [EnvOpenRouterKey] in the
	// SECRETS list. Empty means the installation expects the runtime image to
	// carry its own provider credentials.
	OpenRouterAPIKey string
	// Model is the primary model slug. Empty means the runtime decides, which
	// is why Build omits the config key entirely rather than writing "".
	Model string
	// ModelFallbacks are tried in order after Model.
	ModelFallbacks []string
	// MemoryRequest and MemoryLimit are passed to the driver verbatim, in the
	// units it understands. See [provision.Resources] on why they are strings.
	MemoryRequest string
	MemoryLimit   string
	// CPURequest and CPULimit likewise.
	CPURequest string
	CPULimit   string
	// NodeOptions, when set, becomes [EnvNodeOptions].
	NodeOptions string
	// WorkspacePath defaults to [DefaultWorkspacePath].
	WorkspacePath string
	// WorkspaceSize is the workspace volume size, e.g. "10Gi". Empty leaves the
	// driver's own default in place.
	WorkspaceSize string
	// WorkspacePersist asks for storage that survives a restart.
	WorkspacePersist bool
	// Labels are added to every spec this Config builds, BEFORE the per-agent
	// labels, which win on a key collision. Nil is fine.
	Labels map[string]string
	// AutosaveExcludePatterns are gitignore patterns for the agent RUNTIME's own
	// bookkeeping files, handed to the work-autosave daemon so it does not
	// force-push them into the agent's repository.
	//
	// 🔴 THERE IS NO DEFAULT, DELIBERATELY. See autosave.go's note: a built-in list
	// names one specific runtime, and is wrong for every other one in two
	// directions at once. Nil is valid and silent. Anchor each entry with a leading
	// "/" unless you mean it to match at every depth.
	AutosaveExcludePatterns []string

	// CairnURL and CairnToken are the hosted subsystem-store coordinates.
	//
	// 🔴 BOTH-OR-NEITHER, AND INERT WHEN UNSET. With either one empty nothing
	// cairn-related is emitted — no install step, no credential file, no
	// environment variable — and the prose half claims nothing. A token without a
	// URL is useless in exactly the case a credential exists for. Read the
	// pair through [Config.CairnConfigured], never field by field.
	//
	// 🔴 CairnToken IS A READ+WRITE KEY ACROSS EVERY SCOPE OF THE STORE, which is
	// why eligibility is an explicit per-call input ([Options.CairnEligible])
	// rather than something every agent gets. cairn.go's header carries the
	// blast-radius argument in full.
	//
	// ⚠ THERE IS DELIBERATELY NO DEFAULT URL. A built-in would collapse the gate
	// to "is the token set", and a store's address is deployment state rather
	// than a constant this package should assert.
	CairnURL   string
	CairnToken string
}

// Options is the per-call part that is neither deployment config nor a column on
// the agent row.
type Options struct {
	// Instructions is the rendered agent-instruction document. When non-empty
	// Build writes it to [InstructionsFileName] inside the workspace.
	//
	// 🔴 ITS CONTENT IS NOT THIS PACKAGE'S BUSINESS, and that separation is the
	// point. Upstream this text was generated by skill.go, which plan step 23
	// deletes; ranked item 3 owns porting the generator. Keeping it an input
	// means item 3 changes what the file SAYS without touching how it is
	// PLACED, and a test here can assert placement without asserting prose.
	Instructions string

	// SeedFiles are extra files to place in the workspace. Keys are names
	// relative to the workspace root; a key containing a path separator is an
	// error rather than a silently-created subdirectory, because the upstream
	// shell form could not express one and a caller writing "a/b.md" is more
	// likely to be confused than deliberate.
	//
	// Upstream these were the chief-only BOOTSTRAP.md / IDENTITY.md / USER.md
	// writes in workspaceSeedInitCommands, selected by comparing the agent name
	// against a hardcoded constant. That selection is a CALLER's policy, not a
	// spec-building rule, so it does not live here.
	SeedFiles map[string]string

	// ExtraEnv is appended after the env this package computes. A name that
	// collides with one of the constants above is an error: silently shadowing
	// [EnvAPIURL] would point an instance at the wrong server, and silently
	// dropping the caller's value would be worse.
	ExtraEnv []provision.EnvVar

	// CairnEligible declares that THIS agent may hold the subsystem-store
	// credential. It is one half of the gate; [Config.CairnConfigured] is the
	// other, and both must be true before anything is emitted.
	//
	// 🔴 IT IS AN INPUT BECAUSE ELIGIBILITY IS A CALLER'S POLICY, exactly as
	// [Options.SeedFiles] argues for its own case: upstream this was an agent name
	// compared against a hardcoded constant inside the spec builder, and a
	// name-to-privilege rule is not a spec-building rule. The caller that knows
	// which agent is the supervisor sets it. Today that is one agent; see
	// cairn.go's header for why widening it is a one-line change here and an
	// unrecoverable one in the store.
	//
	// ⚠ SETTING IT WITH NO CREDENTIAL CONFIGURED IS NOT AN ERROR, it is simply
	// inert. The alternative — refusing — would make an installation that has not
	// configured a store unable to dispatch its supervisor at all.
	CairnEligible bool
}

// Build assembles the spec for one agent.
//
// 🔴 SECRETS GO IN Secrets, NEVER IN Env, AND THE SPLIT IS THE WHOLE REASON
// [provision.Spec] HAS TWO FIELDS. The type system is what makes the
// confidential set enumerable: a driver cannot accidentally log, diff or
// ConfigMap something it was handed through Secrets. Two values qualify here —
// the agent's own per-agent token and the shared provider key — and both are
// routed accordingly. If you add a third, ask which list it belongs in before
// asking where in the function to put it.
//
// The returned spec is validated before it is returned, so a nil error means the
// spec satisfies [provision.Spec.Validate] and every driver's precondition that
// does not depend on the driver's own capabilities. A driver may still refuse it
// through CheckSpec — that is a capability question, not a construction one.
func Build(a agents.Agent, cfg Config, opts Options) (provision.Spec, error) {
	if strings.TrimSpace(cfg.ImageRepo) == "" {
		return provision.Spec{}, fmt.Errorf("agentspec: Config.ImageRepo is required (there is no defensible default for an installation-specific registry)")
	}
	if strings.TrimSpace(cfg.APIBaseURL) == "" {
		return provision.Spec{}, fmt.Errorf("agentspec: Config.APIBaseURL is required (an instance with no base URL cannot reach muster, and a guessed service name would fail at runtime instead of here)")
	}

	ref := provision.Ref{Name: a.Name, ID: a.ID}
	if err := ref.Validate(); err != nil {
		return provision.Spec{}, fmt.Errorf("agentspec: agent %d has an unusable name: %w", a.ID, err)
	}

	workspace := cfg.WorkspacePath
	if workspace == "" {
		workspace = DefaultWorkspacePath
	}

	env, err := buildEnv(cfg, opts)
	if err != nil {
		return provision.Spec{}, err
	}
	files, err := buildFiles(workspace, cfg, opts)
	if err != nil {
		return provision.Spec{}, err
	}

	repo := buildRepo(a, workspace)
	// 🔴 OFF-INSTANCE DURABILITY, GATED ON THERE BEING A REPOSITORY TO PUSH TO.
	// See autosave.go for what this is and why it arrived in this step. The gate is
	// Repo, not a credential check: whether the push can actually authenticate is
	// not knowable here — provision.Repo is DECLARED, NOT CLONED, so nothing in
	// this package has a working tree or a credential helper to test. The daemon
	// answers that question itself at runtime and posts a durability alarm to the
	// agent's own task thread when it cannot push, which is a better place for it
	// than a build-time guess.
	init, err := buildInit(workspace, cfg, opts)
	if err != nil {
		return provision.Spec{}, err
	}
	if repo.URL != "" {
		files = append(files, autosaveFile())
		cmd, err := autosaveInvocation(repo.Path, a.Name, a.ID, cfg.AutosaveExcludePatterns)
		if err != nil {
			return provision.Spec{}, err
		}
		init = append(init, cmd)
	}

	spec := provision.Spec{
		Ref: ref,
		Runtime: provision.Runtime{
			Image:      imageRef(cfg),
			WorkingDir: workspace,
		},
		Env:     env,
		Secrets: buildSecrets(a, cfg),
		Files:   files,
		Init:    init,
		Resources: provision.Resources{
			CPURequest:    cfg.CPURequest,
			CPULimit:      cfg.CPULimit,
			MemoryRequest: cfg.MemoryRequest,
			MemoryLimit:   cfg.MemoryLimit,
		},
		Workspace: provision.Workspace{
			Path:    workspace,
			Size:    cfg.WorkspaceSize,
			Persist: cfg.WorkspacePersist,
		},
		Repo:   repo,
		Config: buildRuntimeConfig(cfg),
		Labels: buildLabels(a, cfg),
	}

	if err := spec.Validate(); err != nil {
		return provision.Spec{}, fmt.Errorf("agentspec: built an invalid spec for agent %q: %w", a.Name, err)
	}
	return spec, nil
}

// imageRef joins the repo and tag. It does NOT validate the result: a registry's
// legal reference shapes are the registry's business, and a parser here would be
// a second, wrong authority — the same argument [provision.Resources] makes for
// keeping quantities as strings.
func imageRef(cfg Config) string {
	tag := cfg.ImageTag
	if tag == "" {
		tag = DefaultImageTag
	}
	return cfg.ImageRepo + ":" + tag
}

// buildEnv computes the non-sensitive environment.
//
// The order is fixed rather than map-derived so that the golden test pins a
// sequence: a spec whose env order varies between runs cannot be compared
// byte-for-byte, and losing that comparison costs more than the ordering
// constrains.
func buildEnv(cfg Config, opts Options) ([]provision.EnvVar, error) {
	env := []provision.EnvVar{
		{Name: EnvAPIURL, Value: cfg.APIBaseURL},
		{Name: EnvGitTerminalPrompt, Value: "0"},
	}
	if cfg.NodeOptions != "" {
		env = append(env, provision.EnvVar{Name: EnvNodeOptions, Value: cfg.NodeOptions})
	}
	// The subsystem-store client's config path. See cairn.go — it names a file,
	// so it is env rather than a secret, and it is absent entirely when the
	// integration is off.
	if cairnEnabled(cfg, opts) {
		env = append(env, provision.EnvVar{Name: EnvCairnConfig, Value: CairnConfigPath})
	}

	// A collision check, not a merge. See Options.ExtraEnv.
	//
	// ⚠ EnvCairnConfig IS RESERVED UNCONDITIONALLY, not only when the integration
	// is on. A reserved set that changed with the configuration would accept a
	// caller's CAIRN_CONFIG in the default deployment and refuse the identical
	// call once a store was configured — a collision check whose answer depends on
	// unrelated state is worse than none.
	reserved := map[string]bool{
		EnvAPIURL:            true,
		EnvGitTerminalPrompt: true,
		EnvNodeOptions:       true,
		EnvToken:             true,
		EnvOpenRouterKey:     true,
		EnvCairnConfig:       true,
	}
	for _, e := range opts.ExtraEnv {
		if reserved[e.Name] {
			return nil, fmt.Errorf("agentspec: Options.ExtraEnv may not set %s — this package computes it, and shadowing it would point the instance at the wrong server or credential", e.Name)
		}
		env = append(env, e)
	}
	return env, nil
}

// buildSecrets computes the sensitive environment. See Build's note on the split.
func buildSecrets(a agents.Agent, cfg Config) []provision.EnvVar {
	var secrets []provision.EnvVar
	if a.HooksToken != "" {
		secrets = append(secrets, provision.EnvVar{Name: EnvToken, Value: a.HooksToken})
	}
	if cfg.OpenRouterAPIKey != "" {
		secrets = append(secrets, provision.EnvVar{Name: EnvOpenRouterKey, Value: cfg.OpenRouterAPIKey})
	}
	return secrets
}

// buildFiles turns instruction text and seed content into data.
//
// Files are emitted in a DETERMINISTIC order — the instructions file first, then
// seeds sorted by name, then the cairn pair — because Options.SeedFiles is a map
// and Go randomises map iteration. Without the sort the golden would fail
// intermittently, which is the worst kind of gate: it trains people to re-run.
//
// The cairn pair goes LAST rather than interleaved with the seeds: neither is in
// the workspace, so sorting them among workspace-relative names would put an
// absolute path in the middle of a list a reader scans as "what is in the
// workspace".
func buildFiles(workspace string, cfg Config, opts Options) ([]provision.File, error) {
	var files []provision.File
	if opts.Instructions != "" {
		files = append(files, provision.File{
			Path:    path.Join(workspace, InstructionsFileName),
			Content: []byte(opts.Instructions),
		})
	}

	names := make([]string, 0, len(opts.SeedFiles))
	for name := range opts.SeedFiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if name == "" {
			return nil, fmt.Errorf("agentspec: Options.SeedFiles has an empty file name")
		}
		if strings.ContainsRune(name, '/') {
			return nil, fmt.Errorf("agentspec: Options.SeedFiles key %q contains a path separator; keys are names relative to the workspace root", name)
		}
		if name == InstructionsFileName && opts.Instructions != "" {
			return nil, fmt.Errorf("agentspec: Options.SeedFiles key %q collides with Options.Instructions, which is already written to that path", name)
		}
		files = append(files, provision.File{
			Path:    path.Join(workspace, name),
			Content: []byte(opts.SeedFiles[name]),
		})
	}

	// 🔴 THE TWO CAIRN FILES MOVE TOGETHER OR NOT AT ALL. The wrapper without the
	// credential is a command that refuses every store call; the credential
	// without the wrapper is a secret placed for nobody. One condition emits both,
	// which is what makes the pair unable to drift apart.
	if cairnEnabled(cfg, opts) {
		if err := checkCairnWorkspace(workspace); err != nil {
			return nil, err
		}
		files = append(files, cairnWrapperFile(workspace), cairnCredentialFile(cfg))
	}
	return files, nil
}

// buildInit returns the imperative steps.
//
// ⚠ IT IS NO LONGER UNCONDITIONALLY EMPTY, AND THIS COMMENT USED TO SAY IT WAS.
// It read "🔴 IT IS EMPTY, DELIBERATELY … When the first real entry arrives, this
// comment is what tells the author why the list was empty rather than lost." The
// first real entry has arrived: the subsystem-store client install (cairn.go).
// The sentence is kept in this corrected form rather than deleted, because the
// argument it was making is the one that still governs what may be added.
//
// 🔴 THE BAR FOR AN ENTRY: IT MUST HAVE TO *RUN*, NOT HAVE TO *EXIST*. Upstream
// this was one joined string with seven contributors, and the reason it grew is
// that appending to it was always the cheapest option. Every contributor that was
// really "place this content" is now a [provision.File]. The cairn entry passes
// the bar for a reason worth stating: it FETCHES from a network at a pinned
// revision and then PROVES what it fetched imports, and neither of those is
// expressible as data — the client is deliberately not vendored here. The file it
// needs on PATH is still a [provision.File]; only the fetch is a step.
//
// Still not here, each for a reason that is a decision rather than an oversight:
//
//   - the in-pod CLI download needs muster's own /agent/muster route and a
//     verified artifact, which is plan step 22b's territory because it is the
//     first thing that requires the instance to reach a live server;
//   - submodule init and git credentials need the repository to have been cloned,
//     and [provision.Repo] is DECLARED, NOT CLONED by design — so whoever does
//     the cloning owns those steps.
//
// The order is fixed and the cairn entry is FIRST, ahead of the autosave
// invocation Build appends after this returns. That ordering is load-bearing in
// one direction only: the autosave entry BACKGROUNDS a daemon, so a step placed
// after it races the daemon's first snapshot, while a step placed before it
// cannot. Nothing here depends on cairn's output, and nothing should — an Init
// entry that needed a previous entry's side effect would be one step split in
// two.
func buildInit(workspace string, cfg Config, opts Options) ([]string, error) {
	var init []string
	if cairnEnabled(cfg, opts) {
		cmd, err := cairnInstallCommand(workspace)
		if err != nil {
			return nil, err
		}
		init = append(init, cmd)
	}
	return init, nil
}

// buildRepo declares the source repository. NOTHING HERE CLONES IT — see
// [provision.Repo], which explains that the upstream template cloned the
// repository itself roughly 250 lines before init commands ran, and why that is
// not reproduced.
//
// Path is derived from the repo NAME rather than from its owner/name pair,
// because the instance works in a directory, not in a namespace.
func buildRepo(a agents.Agent, workspace string) provision.Repo {
	if a.Repo == "" {
		return provision.Repo{}
	}
	return provision.Repo{
		URL:    a.Repo,
		Branch: a.RepoBranch,
		Path:   path.Join(workspace, repoDirName(a.Repo)),
	}
}

// repoDirName takes the last path element of an "owner/name" repo reference.
//
// ⚠ IT IS NOT A URL PARSER. It handles the "owner/name" shape the agent row
// carries, and for anything else it returns the input's last segment, which is
// the same thing `git clone` would name the directory. A trailing slash yields
// the segment before it rather than an empty string, because an empty directory
// name would join to the workspace root and silently make the repo path the
// workspace itself.
func repoDirName(repo string) string {
	trimmed := strings.TrimRight(repo, "/")
	if trimmed == "" {
		return ""
	}
	if i := strings.LastIndex(trimmed, "/"); i >= 0 {
		return trimmed[i+1:]
	}
	return trimmed
}

// buildRuntimeConfig writes the opaque runtime config.
//
// It writes the model key ONLY when there is a model to name. An empty primary
// with a populated fallback list is not expressible upstream either, and writing
// {"primary": ""} would hand the runtime a value it has to special-case — which
// is how "unset" and "empty" stop being distinguishable, the exact defect
// plan §4.2 records against the original config.
func buildRuntimeConfig(cfg Config) map[string]any {
	if cfg.Model == "" && len(cfg.ModelFallbacks) == 0 {
		return nil
	}
	model := map[string]any{}
	if cfg.Model != "" {
		model["primary"] = cfg.Model
	}
	if len(cfg.ModelFallbacks) > 0 {
		// Copied, not aliased: Spec.Config is handed to a driver and a caller
		// mutating its own slice afterwards must not change a built spec.
		fallbacks := make([]string, len(cfg.ModelFallbacks))
		copy(fallbacks, cfg.ModelFallbacks)
		model["fallbacks"] = fallbacks
	}
	return map[string]any{ConfigKeyModel: model}
}

// buildLabels merges the installation's labels with this agent's.
//
// The per-agent labels are applied SECOND so they win a key collision, and the
// map is always freshly allocated so a caller's Config.Labels is never mutated
// by building a spec from it.
func buildLabels(a agents.Agent, cfg Config) map[string]string {
	labels := make(map[string]string, len(cfg.Labels)+2)
	for k, v := range cfg.Labels {
		labels[k] = v
	}
	labels["muster.agent/name"] = a.Name
	if a.Namespace != "" {
		labels["muster.agent/group"] = a.Namespace
	}
	return labels
}

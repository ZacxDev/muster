package agentspec

import (
	"fmt"
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// The claude-code kind's profile: the Claude Code pod (cmd/ccd + the pinned CLI,
// images/claude-code-agent).
//
// Every value below is a property of THAT IMAGE, read from its Dockerfile,
// entrypoint and ccd's own config, not chosen here:
//
//   - port 18789, named provision.DefaultPortName: ccd's listen port
//     (agentspec.DefaultGatewayPort — ccd serves muster's /v1/responses wire on it);
//   - health "/" on that port, startup + liveness and NO readiness (the k8s
//     driver's fixed shape, see k8s/render.go): ccd's `GET /` gates only on its
//     tmux session and its CLI not crash-looping, NEVER on auth or a rate limit,
//     so a rate-limited account cannot get its pod restarted;
//   - uid/gid 1000: the image's `USER 1000:1000`;
//   - the volume at /data: CLAUDE_CONFIG_DIR=/data/claude and
//     CCD_WORKSPACE=/data/workspace are siblings under it, so the conversation
//     transcripts `claude --continue` resumes from survive a pod restart;
//   - resources: the scoping measurement (an idle TUI ~300Mi/128m; long-running
//     real sessions 460-562MiB RSS) — request 512Mi/250m, limit 3Gi, no CPU limit.
const (
	// EnvClaudeOAuthToken is the variable the Claude Code CLI reads its
	// subscription `setup-token` from.
	//
	// 🔴 IT IS PLACED IN EXACTLY ONE PLACE: the claude-code agent's own per-agent
	// Secret (Spec.Secrets, rendered by the k8s driver as the `<agent>-env`
	// Secret, consumed by envFrom). Never Env, never a File, never another kind's
	// spec. TestTheClaudeTokenNameAppearsOnlyInTheClaudeCodeKindsSecret is the seam
	// ledger that holds it there.
	EnvClaudeOAuthToken = "CLAUDE_CODE_OAUTH_TOKEN"

	// ClaudeCodeDataPath is where the agent's persistent volume is mounted.
	ClaudeCodeDataPath = "/data"
	// ClaudeCodeWorkspacePath is the CLI's workspace (the image's CCD_WORKSPACE).
	ClaudeCodeWorkspacePath = "/data/workspace"
	// ClaudeCodeUID is the image's numeric user and group.
	ClaudeCodeUID = 1000
	// DefaultClaudeCodeStorage is the volume size when the profile names none.
	DefaultClaudeCodeStorage = "10Gi"
	// LabelKind is the label a claude-code instance carries naming its kind.
	LabelKind = "muster.agent/kind"
)

// ClaudeCodeResources is the claude-code kind's compute budget.
var ClaudeCodeResources = provision.Resources{CPURequest: "250m", MemoryRequest: "512Mi", MemoryLimit: "3Gi"}

// ClaudeCodeSecurity is the claude-code kind's isolation.
//
// 🔴 NoServiceAccountToken IS TRUE: a shell-capable agent holding a subscription
// token does not also get a Kubernetes API credential. A privilege profile
// granted to such an agent binds RBAC to a ServiceAccount whose token is never
// mounted, so the grant has no effect on it — by design, until a deliberate
// opt-in exists (scoping proposal Q6).
var ClaudeCodeSecurity = provision.Security{
	RunAsUser:             ClaudeCodeUID,
	RunAsGroup:            ClaudeCodeUID,
	FSGroup:               ClaudeCodeUID,
	RunAsNonRoot:          true,
	Restricted:            true,
	NoServiceAccountToken: true,
}

// ClaudeCodeConfig enables the claude-code kind. A nil *ClaudeCodeConfig on
// [Config] means the kind is NOT enabled, and Build refuses a claude-code row.
type ClaudeCodeConfig struct {
	// Image is the FULL image reference, tag or digest included. Required.
	Image string
	// Accounts maps an account name to its setup-token. The agent row names its
	// account (agents.Agent.CCAccount); Build looks the token up here.
	Accounts map[string]string
	// StorageSize is the /data volume's size. Empty means [DefaultClaudeCodeStorage].
	StorageSize string
}

// buildClaudeCode is Build for a claude-code row. Pure, like Build.
//
// 🔴 EVERY MISSING INPUT IS A REFUSAL, NAMING IT. A claude-code pod with no
// image, no account, an unknown account or no hooks token would still be a
// valid-looking Deployment — and would boot into a TUI that cannot answer one
// turn (or a ccd that refuses to start), which reads on the card as a slow agent.
//
// ⚠ Options IS MOSTLY NOT APPLICABLE HERE, AND WHAT IS NOT APPLICABLE IS EITHER
// IGNORED OR REFUSED, BY WHETHER IGNORING IT LIES. Instructions is ignored: the
// task reaches a claude-code agent as its kickoff turn, and placing a read-only
// instructions file inside the CLI's own workspace is a separate decision (it
// would also need the CLI's name for it, CLAUDE.md). SeedFiles, ExtraEnv and
// CairnEligible are REFUSED: a caller that asked for them would otherwise get an
// agent silently without them.
func buildClaudeCode(a agents.Agent, cfg Config, opts Options) (provision.Spec, error) {
	cc := cfg.ClaudeCode
	if cc == nil {
		return provision.Spec{}, fmt.Errorf("agentspec: agent %q is kind %s, which this deployment has not "+
			"enabled (MUSTER_AGENT_KINDS)", a.Name, agents.KindClaudeCode)
	}
	if strings.TrimSpace(cc.Image) == "" {
		return provision.Spec{}, fmt.Errorf("agentspec: ClaudeCodeConfig.Image is required")
	}
	if strings.TrimSpace(cfg.APIBaseURL) == "" {
		return provision.Spec{}, fmt.Errorf("agentspec: Config.APIBaseURL is required (an instance with no base URL cannot reach muster)")
	}
	if len(opts.SeedFiles) > 0 || len(opts.ExtraEnv) > 0 || opts.CairnEligible {
		return provision.Spec{}, fmt.Errorf("agentspec: agent %q is kind %s, which takes no seed files, extra "+
			"environment or subsystem-store credential", a.Name, agents.KindClaudeCode)
	}
	if a.CCAccount == "" {
		return provision.Spec{}, fmt.Errorf("agentspec: agent %q is kind %s but names no Claude account", a.Name, agents.KindClaudeCode)
	}
	token, ok := cc.Accounts[a.CCAccount]
	if !ok || token == "" {
		// The account NAME only — never a token, and not the configured set's
		// values either.
		return provision.Spec{}, fmt.Errorf("agentspec: agent %q runs on Claude account %q, which is not "+
			"configured (MUSTER_AGENT_CC_ACCOUNTS); an agent keeps its account for life, so restore that "+
			"account's token rather than expecting a re-selection", a.Name, a.CCAccount)
	}
	if a.HooksToken == "" {
		return provision.Spec{}, fmt.Errorf("agentspec: agent %q has no hooks token; ccd refuses to start "+
			"without one (it derives the bearer muster sends from it)", a.Name)
	}

	ref := provision.Ref{Name: a.Name, ID: a.ID}
	if err := ref.Validate(); err != nil {
		return provision.Spec{}, fmt.Errorf("agentspec: agent %d has an unusable name: %w", a.ID, err)
	}
	size := cc.StorageSize
	if size == "" {
		size = DefaultClaudeCodeStorage
	}
	labels := buildLabels(a, cfg)
	labels[LabelKind] = agents.KindClaudeCode

	spec := provision.Spec{
		Ref: ref,
		// ⚠ NO WorkingDir: the image's own WORKDIR (/data/workspace) is the shape
		// the operator's live check ran — a fresh 2Gi local-path PVC at /data, the
		// entrypoint's `ccd seed` creating the workspace as uid 1000 — and it
		// worked. Overriding it here would be an unmeasured difference.
		Runtime: provision.Runtime{Image: cc.Image},
		Env: []provision.EnvVar{
			{Name: EnvAPIURL, Value: cfg.APIBaseURL},
			{Name: EnvGitTerminalPrompt, Value: "0"},
		},
		// 🔴 THE ONLY PLACE EnvClaudeOAuthToken IS EVER WRITTEN.
		Secrets: []provision.EnvVar{
			{Name: EnvToken, Value: a.HooksToken},
			{Name: EnvGatewayToken, Value: a.HooksToken},
			{Name: EnvClaudeOAuthToken, Value: token},
		},
		Resources: ClaudeCodeResources,
		Workspace: provision.Workspace{Path: ClaudeCodeDataPath, Size: size, Persist: true},
		Repo:      buildRepo(a, ClaudeCodeWorkspacePath),
		Ports:     []provision.Port{{Name: provision.DefaultPortName, Port: DefaultGatewayPort}},
		Health:    provision.Health{HTTPGetPath: DefaultGatewayHealthPath, PortName: provision.DefaultPortName},
		Security:  ClaudeCodeSecurity,
		Labels:    labels,
	}
	if err := spec.Validate(); err != nil {
		return provision.Spec{}, fmt.Errorf("agentspec: built an invalid spec for agent %q: %w", a.Name, err)
	}
	return spec, nil
}

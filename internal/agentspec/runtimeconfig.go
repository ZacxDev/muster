package agentspec

import (
	"fmt"

	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// THE AGENT RUNTIME'S OWN CONFIGURATION, AND WHY MUSTER CARRIES NONE OF IT.
//
// 🔴 THE DEFECT THIS CLOSES, MEASURED END TO END AGAINST THE IMAGE A DEPLOYMENT
// RUNS: an agent muster provisioned CRASHLOOPED and never served. The container
// exited 78 with
//
//	[gateway] loading configuration…
//	[gateway] resolving authentication…
//	Missing config. Run `<setup>` or set gateway.mode=local (or pass --allow-unconfigured).
//
// and restarted for ever. Four separate facts came out of reproducing it, and
// each one rules out a fix that looks obvious:
//
//  1. THE COMMAND WAS NOT THE FAULT. The image's own entrypoint already starts
//     its gateway with a LAN bind — `docker inspect` shows a two-element
//     entrypoint and a three-element default command that spell exactly that. A
//     spec that sets no command gets the right process. What it lacks is a
//     CONFIGURATION.
//  2. STARTING IT UNCONFIGURED IS NECESSARY AND NOT SUFFICIENT. With the
//     runtime's own unconfigured escape hatch the gateway reaches ready and
//     answers 200 on `/` — and its model-response route answers 404 for EVERY
//     bearer: none, wrong and correct alike. 🔴 A 404 ATTRIBUTES NOTHING,
//     because "route absent" and "credential refused" are the same observable;
//     measured with all three bearers side by side, which is the only way the
//     two are distinguishable.
//  3. WHAT REGISTERS THE ROUTE IS A KEY IN THE RUNTIME'S CONFIGURATION FILE, and
//     that key's spelling is this package's problem, not its business — see
//     below.
//  4. THE RUNTIME DERIVES NO CREDENTIAL FOR ITSELF. agentgateway.HooksSHA256's
//     formula was CORRECT all along; nothing was writing its result anywhere the
//     runtime reads. [EnvGatewayToken] put the hooks token in the container's
//     environment and the container's gateway never looked at it: the
//     configuration FILE is where a gateway credential is read from.
//
// 🔴 SO SOMETHING HAS TO WRITE A VENDOR-SPECIFIC JSON PATH, AND IT MUST NOT BE
// THIS REPOSITORY. Two shapes were available:
//
//	(A) muster holds the configuration template and does the injection itself —
//	    a `jq` expression naming the runtime's own key path.
//	(B) the OPERATOR supplies both the template and the script that installs it;
//	    muster places them, supplies the derived credential as environment, and
//	    runs the script as the container's command.
//
// (B) IS WHAT THIS IMPLEMENTS, FOR THREE REASONS IN INCREASING ORDER OF FORCE.
//
// The first is the one that was obvious from the outside: this module is PUBLIC
// and the runtime's identifier is a denied token, which is why
// cmd/muster-server's MUSTER_AGENT_GATEWAY_MODEL is configuration rather than a
// constant ("the value belongs to the IMAGE"). A key path is the same kind of
// value and belongs in the same place.
//
// The second is that the SCRIPT CANNOT BE WRITTEN HERE AT ALL, and this is the
// fact that settles (A) versus (B) rather than merely favouring one. Overriding
// a container's command in Kubernetes DROPS the image's default arguments as
// well as its entrypoint — `Command` replaces `ENTRYPOINT` and, with `Args`
// empty, `CMD` is discarded. So whatever muster runs instead has to re-exec the
// image's real entrypoint by name, and those names ARE the vendor identifier:
// an init binary's path, the runtime's own executable, its subcommand and its
// bind flag. Under (A) every one of them would be a string literal in this
// package. Under (B) they are three words in the operator's own ConfigMap and
// muster's half is `sh -eu <path>`.
//
// The third is that supplying the DERIVED credential removes a whole class of
// failure rather than relocating it. The reference deployment's script computed
// the gateway credential itself, by piping a prefixed token through sha256 — a
// SECOND implementation of agentgateway.HooksSHA256's formula, in shell, whose
// disagreement with muster's would be a 401 on every turn and would read as a
// bad credential rather than as a mismatched formula. muster already owns that
// derivation; passing the RESULT in means the two halves cannot disagree,
// because there is only one. [RuntimeConfig.DeriveBearer] is that ownership, and
// it is a function rather than a value because the input is a per-agent token.
//
// ⚠ WHAT (B) COSTS, STATED RATHER THAN GLOSSED: an operator who has not created
// the ConfigMap gets a BOOT REFUSAL instead of a working deployment, and the
// refusal is the whole point — see cmd/muster-server's validateProvisioner. The
// alternative to a refusal is this defect: a provisioner that comes up healthy
// and creates crashlooping pods.
//
// 🔴 WHERE THE TEMPLATE IS PLACED MATTERS, AND THE OBVIOUS PLACE IS THE WRONG
// ONE. The runtime REWRITES its own configuration file during startup — measured:
// it seeds an allowed-origins list and logs a config-overwrite line with a
// before/after digest and a backup path. Mounting the template directly at the
// path it reads therefore puts a read-only projected file under a rename, and the
// runtime logs
//
//	failed to persist …: Error: EBUSY: resource busy or locked, rename …
//
// ⚠ AND THEN CARRIES ON. That was measured too, and it corrects a claim worth
// correcting: the EBUSY is real and it is NOT fatal on the image measured — the
// gateway reports "will start with the in-memory value but config was not saved",
// reaches ready and serves. Writing it off as fatal would be wrong; relying on it
// being survivable would be worse, because a value the runtime could not persist
// is a value the next release may need persisted. So the template is placed at
// [RuntimeConfigPath] — a muster-owned path the runtime never reads — and the
// operator's script copies it where the runtime wants it. That location is on the
// container's ORDINARY writable filesystem, which is why this change adds NO
// volume: measured against the image, the runtime runs as uid 0 with a writable
// root filesystem, and this driver sets no security context that would change
// either. 🔴 IF A FUTURE CHANGE ADDS readOnlyRootFilesystem OR A NON-ROOT USER,
// THIS ARGUMENT DIES AND A WRITABLE VOLUME IS OWED.
//
// ⚠ ONE GOTCHA INHERITED FROM THE REFERENCE DEPLOYMENT, NAMED BECAUSE NOTHING
// HERE CAN GUARD IT: that runtime's repair command silently reverts the
// configuration to its last-known-good copy when the file it is given is
// schema-invalid — the backup path in the overwrite log above is that copy. The
// observable is a change that LOOKS applied while the process runs the OLD
// configuration. muster invokes no such command, and an operator's install script
// must not either: a repair step would convert "the template is malformed" from a
// crash with a parse error into a silently stale agent. If one is ever needed, it
// has to compare the file's digest before and after and fail on a revert.
// ---------------------------------------------------------------------------

// RuntimeConfigDir is the directory muster places the operator's bundle in.
//
// ⚠ IT IS NOT UNDER THE WORKSPACE, DELIBERATELY. The workspace is the agent's own
// working storage — a repository checkout, its notes, whatever it writes — and on
// a persistent deployment it SURVIVES the instance. A startup bundle living there
// would be restored from a previous release's content on the next start, and the
// operator's edit would appear not to have taken.
const RuntimeConfigDir = "/muster/runtime"

// RuntimeConfigPath is where [RuntimeConfig.Template] is placed.
//
// ⚠ IT HAS NO FILE EXTENSION BECAUSE THIS PACKAGE DOES NOT KNOW THE FORMAT. The
// template is opaque bytes: muster never parses it, never validates it and never
// edits it. Naming it `.json` would be an assertion about somebody else's schema
// — exactly the assertion this whole file exists to avoid making.
const RuntimeConfigPath = RuntimeConfigDir + "/config"

// RuntimeInstallPath is where [RuntimeConfig.Install] is placed, and the script
// Build runs as the container's command.
const RuntimeInstallPath = RuntimeConfigDir + "/install"

// DefaultGatewayHealthPath is the path a serving gateway answers on.
//
// 🔴 MEASURED, NOT ASSUMED: a live container of the agent runtime image answered
// HTTP 200 on this path both when its model-response route was registered and
// when it was not, so it reports "the gateway process is serving" independently of
// whether chat is configured — which is exactly what a startup probe must
// measure. A probe pointed at the response route instead would hold a healthy
// agent unready whenever its configuration omitted that one key.
//
// ⚠ IT IS A CONSTANT WITH NO OVERRIDE, UNLIKE [DefaultGatewayPort] BESIDE IT, AND
// THE ASYMMETRY IS ARGUED RATHER THAN INHERITED. The port needs an override
// because a wrong port makes the instance UNADDRESSABLE and fails as
// provision.ErrNoEndpoint — a resolution error with nothing pointing at the port.
// A wrong health path fails as a probe failure naming the path, in the pod's own
// events, which is self-diagnosing; and nothing in muster consumes the path, so a
// second environment variable would be surface with no measured need. This
// repository's own finding is that a documented knob nothing reads is worse than
// no knob.
const DefaultGatewayHealthPath = "/"

// RuntimeConfig is the operator-supplied bundle that makes a provisioned agent's
// own gateway start, plus muster's half of the credential it will serve on.
//
// 🔴 ALL THREE FIELDS MOVE TOGETHER OR NONE OF THEM DO — see [Configured], and
// see buildSecrets for the same pattern applied to the token pair. Each pair of
// them without the third is a different silent failure: a template with no script
// is a file nothing reads; a script with no template is a command that fails on a
// missing input, inside a container, at startup; and either without the
// derivation is an installer that writes an EMPTY gateway credential, which is a
// well-formed configuration the runtime accepts and then refuses every turn
// against with a 401.
type RuntimeConfig struct {
	// Template is the runtime's configuration file, verbatim and OPAQUE. muster
	// places it at [RuntimeConfigPath] and never looks inside.
	Template []byte

	// Install is the shell script Build runs as the container's command. It is
	// placed at [RuntimeInstallPath] and executed with `sh -eu`.
	//
	// 🔴 IT MUST END BY EXEC'ING THE IMAGE'S REAL ENTRYPOINT, and nothing in this
	// repository can check that it does. Overriding a container's command
	// discards the image's default arguments as well as its entrypoint, so a
	// script that merely installs a file and exits turns an agent into a pod that
	// completes successfully and never serves. The failure is NOT silent — the
	// instance reports no ready endpoint and this change's startup probe never
	// passes — but it is not diagnosable from here either, which is why it is
	// written down.
	//
	// ⚠ `sh -eu`, SO A FAILING STEP STOPS THE CONTAINER. That is the same
	// decision internal/provision/k8s's init container records at length: the
	// project this came from wrapped its startup script so a failure could not
	// abort the pod, and the cost was that nothing in the block could ever be
	// OBSERVED to fail. A crash loop with a parse error in the log is the better
	// outcome.
	Install []byte

	// DeriveBearer turns an agent's own hooks token into the credential its
	// gateway will accept, and is muster's ONE implementation of that formula.
	//
	// 🔴 IT IS A FUNCTION AND NOT A VALUE BECAUSE THE INPUT IS PER-AGENT, and it
	// is injected rather than imported because this package deliberately holds no
	// HTTP client while the package that owns the formula does — the identical
	// argument [EnvGatewayToken] records for its own name living here. The
	// binary's wiring supplies agentgateway.Runtime.Bearer; nothing else may
	// supply anything else.
	DeriveBearer func(hooksToken string) string
}

// Configured reports whether the bundle is complete enough to install.
//
// 🔴 IT IS AN AND OF ALL THREE AND NOT A CONVENIENCE. Every caller asks this
// question once and emits every piece from that one branch, which is what makes a
// partial install UNEXPRESSIBLE rather than merely discouraged. cmd/muster-server
// refuses an incomplete bundle at boot, so a partial one should never reach here;
// this is the second line of that defence, and it fails toward "install nothing",
// which leaves the pre-existing behaviour rather than a half-written config.
func (r RuntimeConfig) Configured() bool {
	return len(r.Template) > 0 && len(r.Install) > 0 && r.DeriveBearer != nil
}

// runtimeConfigFiles is the bundle as data. See [provision.File] on why content
// travels as bytes rather than as a shell command that writes them.
//
// ⚠ NEITHER FILE IS MARKED Secret, AND THE SCRIPT IS NOT MARKED EXECUTABLE.
// Neither holds a credential — the credential arrives as environment at
// [EnvGatewayBearer] and never touches these bytes — so marking them confidential
// would move them into the driver's secret store for no gain and make them
// invisible to exactly the operator debugging them. The mode is left at the
// driver's default because Build invokes the script through `sh`, which reads it;
// an executable bit would suggest the kernel does.
func runtimeConfigFiles(cfg Config) []provision.File {
	return []provision.File{
		{Path: RuntimeConfigPath, Content: cfg.RuntimeConfig.Template},
		{Path: RuntimeInstallPath, Content: cfg.RuntimeConfig.Install},
	}
}

// runtimeConfigCommand is what the container runs instead of the image's own
// entrypoint. See [RuntimeConfig.Install].
func runtimeConfigCommand() []string {
	return []string{"sh", "-eu", RuntimeInstallPath}
}

// runtimeConfigBearer derives the agent's gateway credential, refusing the one
// input that would produce a well-formed and useless one.
//
// 🔴 AN EMPTY TOKEN IS REFUSED RATHER THAN HASHED, AND THE REFUSAL IS THE WHOLE
// VALUE OF THIS FUNCTION. Every derivation muster has is a hash of a prefixed
// token, so an empty token yields a perfectly well-formed credential that the
// runtime would accept into its configuration and then reject on every turn with
// a 401 — the exact observable buildSecrets' own empty-token branch exists to
// avoid producing. Refusing here surfaces as a spec-build error on the agent's
// row, with the cause named, instead of as a pod that serves an unusable gateway.
//
// ⚠ IT IS NOT REACHABLE ON THE DISPATCH PATH, AND IT IS STILL CHECKED.
// agentprovision.ensureHooksToken mints a token before it builds a spec, so a
// tokenless agent reaching Build means a NEW caller skipped that step — which is
// precisely the case a guard is for, and precisely the case that would otherwise
// present as a 401 attributed to the credential rather than to the caller.
func runtimeConfigBearer(cfg Config, hooksToken string) (string, error) {
	if hooksToken == "" {
		return "", fmt.Errorf("agentspec: a runtime-config bundle is configured but this agent has no "+
			"hooks token: the installer would write an empty gateway credential, which the runtime "+
			"accepts and then refuses every turn against with a 401. The token is minted before the "+
			"spec is built (agentprovision.ensureHooksToken); a caller reaching %s with none has "+
			"skipped that step", RuntimeInstallPath)
	}
	return cfg.RuntimeConfig.DeriveBearer(hooksToken), nil
}

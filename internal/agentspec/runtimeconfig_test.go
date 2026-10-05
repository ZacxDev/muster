package agentspec

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// THE RUNTIME-CONFIG BUNDLE'S GUARDS.
//
// 🔴 WHAT NONE OF THEM CAN BE, STATED FIRST SO A GREEN RUN IS NOT OVERREAD: a
// regression test against the measured defect. The defect was a CONTAINER exiting
// 78 because no configuration file existed, and the input that fixes it —
// [Config.RuntimeConfig] — did not exist at the parent commit, so no test written
// here compiles there. "Red at origin/main" is unavailable by construction; what
// IS available is a mutation sweep over the branch each guard pins, and the
// closing condition in cmd/muster-server/doc_seams.go, which is a live probe.
//
// 🔴 SO EVERY GUARD BELOW PINS A RELATIONSHIP RATHER THAN A VALUE, because the
// failure modes are all "one of the four pieces is missing while the other three
// are present" — and each of those four is a different silent outcome. A test per
// piece, asserting that piece exists, would go green over a bundle installed
// without its credential.
// ---------------------------------------------------------------------------

// bundleConfig is fixtureConfig with the bundle, which is what a deployment runs.
func bundleConfig() Config { return fixtureConfig() }

// noBundleConfig is the same configuration with the bundle removed.
//
// ⚠ IT IS DERIVED FROM THE SAME FIXTURE RATHER THAN WRITTEN OUT, so the ONLY
// difference between the two cases below is the bundle. A separately-written
// "minimal" config would differ in other fields too, and an assertion about what
// disappeared could then be satisfied by a field that was never there.
func noBundleConfig() Config {
	cfg := fixtureConfig()
	cfg.RuntimeConfig = RuntimeConfig{}
	return cfg
}

// TestTheRuntimeConfigBundleInstallsAllFourPiecesOrNone is the seam guard.
//
// 🔴 IT ASSERTS THE WHOLE SET IN BOTH DIRECTIONS, WHICH IS THE ONLY SHAPE THAT
// CAN FAIL FOR A PARTIAL INSTALL. The four pieces are the template file, the
// install script file, the command that runs the script, and the derived
// credential — and each one missing on its own is a DIFFERENT failure that looks
// like something else:
//
//   - no template: the script fails on a missing input, inside the container, at
//     startup, as a crash loop with a shell error;
//   - no script file: the container's command names a path that does not exist,
//     which the kubelet reports as a start failure;
//   - no command: the script is placed and never runs, so the image's own
//     entrypoint starts unconfigured and exits 78 — the original defect, with the
//     fix sitting on disk beside it;
//   - no credential: the installer writes an empty gateway token, which the
//     runtime ACCEPTS and then refuses every turn against with a 401.
//
// ⚠ THE NEGATIVE HALF IS NOT SYMMETRY. With the bundle absent the spec must be
// exactly what it was before this feature existed — most importantly Command must
// stay nil, because an override naming a script that was not placed turns a
// working deployment into a pod that cannot start.
func TestTheRuntimeConfigBundleInstallsAllFourPiecesOrNone(t *testing.T) {
	a := fixtureAgent()
	with := mustBuild(t, a, bundleConfig(), Options{})

	tmpl, ok := fileAt(with, RuntimeConfigPath)
	if !ok {
		t.Fatalf("no file at %s. The install script's input is missing, so the container "+
			"fails on it at startup.\n  files: %v", RuntimeConfigPath, filePaths(with))
	}
	if got := string(tmpl.Content); got != string(bundleConfig().RuntimeConfig.Template) {
		t.Errorf("the template placed at %s is not the one configured.\n  got:  %q\n  want: %q",
			RuntimeConfigPath, got, bundleConfig().RuntimeConfig.Template)
	}
	if tmpl.Secret {
		t.Errorf("%s is marked Secret. It holds no credential — the credential travels as %s "+
			"— and marking it confidential hides it from the operator debugging it",
			RuntimeConfigPath, EnvGatewayBearer)
	}

	script, ok := fileAt(with, RuntimeInstallPath)
	if !ok {
		t.Fatalf("no file at %s, which is the path the container's command names.\n  files: %v",
			RuntimeInstallPath, filePaths(with))
	}
	if got := string(script.Content); got != string(bundleConfig().RuntimeConfig.Install) {
		t.Errorf("the script placed at %s is not the one configured.\n  got:  %q\n  want: %q",
			RuntimeInstallPath, got, bundleConfig().RuntimeConfig.Install)
	}

	// 🔴 THE COMMAND IS PINNED AS A LITERAL SEQUENCE, not compared against
	// runtimeConfigCommand(). An assertion that called the implementation would be
	// satisfied by any command at all, including one that installs the file and
	// exits — which is the piece whose absence reproduces the original defect
	// exactly.
	wantCmd := []string{"sh", "-eu", RuntimeInstallPath}
	if strings.Join(with.Runtime.Command, "\x1f") != strings.Join(wantCmd, "\x1f") {
		t.Errorf("Runtime.Command = %v, want %v. Without this override the image's own "+
			"entrypoint runs, which is MEASURED to exit 78 with `Missing config` while the "+
			"script sits unread on disk.", with.Runtime.Command, wantCmd)
	}
	if len(with.Runtime.Args) != 0 {
		t.Errorf("Runtime.Args = %v, want empty: muster does not know the image's default "+
			"arguments and must not invent them — re-exec'ing the real entrypoint is the "+
			"operator's script's job", with.Runtime.Args)
	}

	if got, ok := envValue(with, EnvRuntimeConfig); !ok || got != RuntimeConfigPath {
		t.Errorf("%s = %q (present=%v), want %q — without it the operator's script has to "+
			"hardcode a muster-owned path", EnvRuntimeConfig, got, ok, RuntimeConfigPath)
	}

	// 🔴 THE CREDENTIAL IS COMPARED AGAINST THE FIXTURE'S OWN DERIVATION APPLIED TO
	// THE ROW'S TOKEN, which is what distinguishes "a bearer was shipped" from "THE
	// bearer was shipped". A Build that hashed the wrong input — the agent's name,
	// the image tag, an empty string — ships a well-formed 64-character credential
	// and 401s every turn.
	wantBearer := fixtureDeriveBearer(a.HooksToken)
	got, ok := secretValue(with, EnvGatewayBearer)
	if !ok {
		t.Fatalf("%s is not in Secrets, so the installed configuration carries an EMPTY "+
			"gateway credential: the runtime accepts it and refuses every turn with a 401",
			EnvGatewayBearer)
	}
	if got != wantBearer {
		t.Errorf("%s = %q, want %q (the configured derivation over the ROW's token)",
			EnvGatewayBearer, got, wantBearer)
	}
	// CONTROL: the derived value must not merely BE the token. A pass-through
	// derivation would satisfy "a secret is present" and send the raw hooks token
	// where a derived one belongs.
	if got == a.HooksToken {
		t.Errorf("%s carries the row's token verbatim rather than a derived value", EnvGatewayBearer)
	}

	// --- and with no bundle, none of the four ---------------------------------
	without := mustBuild(t, a, noBundleConfig(), Options{})
	if _, ok := fileAt(without, RuntimeConfigPath); ok {
		t.Errorf("a config with NO bundle still placed %s", RuntimeConfigPath)
	}
	if _, ok := fileAt(without, RuntimeInstallPath); ok {
		t.Errorf("a config with NO bundle still placed %s", RuntimeInstallPath)
	}
	if without.Runtime.Command != nil {
		t.Errorf("a config with NO bundle set Runtime.Command = %v. The container would run a "+
			"script that was never placed, so it cannot start at all — strictly worse than the "+
			"unconfigured gateway this feature exists to fix.", without.Runtime.Command)
	}
	if _, ok := envValue(without, EnvRuntimeConfig); ok {
		t.Errorf("a config with NO bundle still set %s, pointing the script at a file muster "+
			"did not place", EnvRuntimeConfig)
	}
	if _, ok := secretValue(without, EnvGatewayBearer); ok {
		t.Errorf("a config with NO bundle still shipped %s", EnvGatewayBearer)
	}
}

// TestAnIncompleteRuntimeConfigBundleInstallsNothing pins [RuntimeConfig.Configured]
// through Build rather than by calling it.
//
// 🔴 EACH CASE REMOVES EXACTLY ONE OF THE THREE INPUTS, which is what makes this a
// test of the AND rather than of any one conjunct. A predicate written as an OR
// passes a "complete bundle installs everything" test perfectly and installs a
// half-bundle for each of these three configurations — and the worst of the three,
// a missing derivation, is the one that produces a running agent that 401s.
//
// ⚠ cmd/muster-server REFUSES ALL THREE AT BOOT, so none of them should reach
// Build in production. This is the second line of that defence and it is worth
// having: a future caller of agentspec that is not that binary inherits the
// refusal only if it is HERE.
func TestAnIncompleteRuntimeConfigBundleInstallsNothing(t *testing.T) {
	for _, tc := range []struct {
		missing string
		mutate  func(*RuntimeConfig)
	}{
		{"template", func(r *RuntimeConfig) { r.Template = nil }},
		{"install script", func(r *RuntimeConfig) { r.Install = nil }},
		{"bearer derivation", func(r *RuntimeConfig) { r.DeriveBearer = nil }},
	} {
		t.Run(tc.missing, func(t *testing.T) {
			cfg := bundleConfig()
			tc.mutate(&cfg.RuntimeConfig)
			if cfg.RuntimeConfig.Configured() {
				t.Fatalf("Configured() is true with no %s", tc.missing)
			}
			spec := mustBuild(t, fixtureAgent(), cfg, Options{})
			if spec.Runtime.Command != nil {
				t.Errorf("Runtime.Command = %v with no %s: the container runs a script that "+
					"may not exist, or exists and cannot work", spec.Runtime.Command, tc.missing)
			}
			for _, p := range []string{RuntimeConfigPath, RuntimeInstallPath} {
				if _, ok := fileAt(spec, p); ok {
					t.Errorf("%s was placed with no %s", p, tc.missing)
				}
			}
			if _, ok := secretValue(spec, EnvGatewayBearer); ok {
				t.Errorf("%s was shipped with no %s", EnvGatewayBearer, tc.missing)
			}
		})
	}

	// POSITIVE CONTROL: the complete bundle must install, or every case above is
	// satisfied by a Build that installs nothing ever.
	if spec := mustBuild(t, fixtureAgent(), bundleConfig(), Options{}); spec.Runtime.Command == nil {
		t.Fatal("positive control FAILED: the COMPLETE bundle installed no command either, so " +
			"the three cases above measure nothing")
	}
}

// TestATokenlessAgentWithABundleIsRefusedRatherThanGivenAnEmptyCredential pins
// runtimeConfigBearer's refusal.
//
// 🔴 THE ALTERNATIVE IS NOT AN ERROR, IT IS A RUNNING AGENT THAT 401s. Every
// derivation muster has is a hash over a prefixed token, so an empty token yields
// a perfectly well-formed credential; the installer would write it, the runtime
// would accept it, and every turn would come back 401 attributed to the
// credential. A build error names the cause and lands on the agent's row.
//
// ⚠ buildSecrets ALREADY OMITS THE TOKEN PAIR FOR A TOKENLESS AGENT, which is why
// this is a separate refusal rather than a fourth conjunct of Configured(): the
// bundle is complete, the agent is not.
func TestATokenlessAgentWithABundleIsRefusedRatherThanGivenAnEmptyCredential(t *testing.T) {
	a := fixtureAgent()
	a.HooksToken = ""
	_, err := Build(a, bundleConfig(), Options{})
	if err == nil {
		t.Fatal("Build accepted a tokenless agent with a runtime-config bundle. The installed " +
			"configuration would carry an empty gateway credential, which the runtime accepts " +
			"and then refuses every turn against with a 401.")
	}
	// It must name the mechanism, not just fail: an operator reading this has to
	// know the token is minted elsewhere.
	for _, want := range []string{"hooks token", "401"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q, so the cause is not attributable.\n  got: %v",
				want, err)
		}
	}

	// POSITIVE CONTROL: the SAME agent with a token must build, or this passes over
	// a Build that refuses every tokenless agent whether or not a bundle exists.
	mustBuild(t, fixtureAgent(), bundleConfig(), Options{})
	// CONTROL: a tokenless agent with NO bundle still builds — that path predates
	// this feature and buildSecrets' own empty-token branch is what handles it.
	if _, err := Build(a, noBundleConfig(), Options{}); err != nil {
		t.Errorf("control FAILED: a tokenless agent with no bundle was refused: %v", err)
	}
}

// TestTheBundleIsPlacedOutsideTheWorkspace is an INVARIANT GUARD, not regression
// coverage: nothing has ever placed it inside.
//
// 🔴 IT IS WORTH HAVING ANYWAY BECAUSE THE WORKSPACE CAN BE PERSISTENT. With
// Config.WorkspacePersist the workspace outlives the instance, so a bundle written
// there would be restored from a previous release's content on the next start and
// the operator's edit would appear not to have taken — a stale-config failure with
// no error anywhere. The cheap way to make that unreachable is for the paths not to
// be under the workspace at all, and this pins it.
func TestTheBundleIsPlacedOutsideTheWorkspace(t *testing.T) {
	cfg := bundleConfig()
	// The fixture's workspace, and the default, are different paths — check both,
	// because a bundle path that collided only with the default would be invisible
	// to a fixture that overrides it.
	for _, ws := range []string{cfg.WorkspacePath, DefaultWorkspacePath} {
		for _, p := range []string{RuntimeConfigPath, RuntimeInstallPath} {
			if strings.HasPrefix(p, ws+"/") || p == ws {
				t.Errorf("%s is inside the workspace %s. On a persistent workspace the bundle "+
					"would survive the instance and shadow the next release's content.", p, ws)
			}
		}
	}
}

// TestTheBundlesEnvNamesCannotBeShadowedByACaller extends the reserved-set claim
// to the two names this feature adds.
//
// 🔴 EnvGatewayBearer IS THE ONE WHOSE SHADOW IS WORST, AND IT IS WORSE THAN THE
// EXISTING RESERVED SET'S. Shadowing EnvAPIURL points an instance at the wrong
// server, which fails visibly. Shadowing this one makes the instance CONFIGURE its
// gateway to accept the caller's value while muster sends its own — so every turn
// 401s and the derivation, which is correct, is what gets blamed.
func TestTheBundlesEnvNamesCannotBeShadowedByACaller(t *testing.T) {
	for _, name := range []string{EnvRuntimeConfig, EnvGatewayBearer} {
		t.Run(name, func(t *testing.T) {
			_, err := Build(fixtureAgent(), bundleConfig(), Options{
				ExtraEnv: []provision.EnvVar{{Name: name, Value: "caller-supplied"}},
			})
			if err == nil {
				t.Fatalf("Options.ExtraEnv was allowed to set %s", name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("the refusal does not name %s.\n  got: %v", name, err)
			}
		})
	}

	// 🔴 AND THE RESERVATION MUST HOLD WITH NO BUNDLE CONFIGURED, which is the half
	// buildEnv's own comment argues for: a reserved set that changed with the
	// configuration would accept a caller's value on one deployment and refuse the
	// identical call on another.
	for _, name := range []string{EnvRuntimeConfig, EnvGatewayBearer} {
		if _, err := Build(fixtureAgent(), noBundleConfig(), Options{
			ExtraEnv: []provision.EnvVar{{Name: name, Value: "caller-supplied"}},
		}); err == nil {
			t.Errorf("with no bundle configured, Options.ExtraEnv was allowed to set %s — the "+
				"same call is refused on a deployment that has one", name)
		}
	}
}

// TestEverySpecDeclaresAHealthSignalOnThePortItPublishes is the probe half's
// spec-side guard.
//
// 🔴 IT PINS THE RELATIONSHIP BETWEEN TWO FIELDS, NOT EITHER FIELD. A Health whose
// PortName does not match the declared port is a probe a driver drops — and
// provision.Spec.Validate refuses that, so the failure would be a dispatch error
// rather than a silent one. What this adds is the direction Validate cannot see:
// that the health signal is declared AT ALL. Build returning a zero Health is a
// perfectly valid spec, and it reinstates the measured defect — a crashlooping
// instance reporting 0/1 for ever with nothing timing it out.
//
// ⚠ IT IS ASSERTED FOR BOTH CONFIGURATIONS, because the port is declared
// unconditionally and so is this: an instance provisioned while chat was off must
// still be probed, or turning chat on later would need every instance re-created.
func TestEverySpecDeclaresAHealthSignalOnThePortItPublishes(t *testing.T) {
	for name, cfg := range map[string]Config{"with bundle": bundleConfig(), "no bundle": noBundleConfig()} {
		t.Run(name, func(t *testing.T) {
			spec := mustBuild(t, fixtureAgent(), cfg, Options{})
			if spec.Health.IsZero() {
				t.Fatal("the built spec declares NO health signal, so a driver reports Ready " +
					"from the process state alone and a gateway that never starts looks the " +
					"same as one that did")
			}
			if spec.Health.HTTPGetPath != "/" {
				t.Errorf("health path = %q, want %q — measured: a live container of the agent "+
					"runtime image answers 200 there whether or not its response route is "+
					"registered, so it reports the gateway process rather than the chat config",
					spec.Health.HTTPGetPath, "/")
			}
			if spec.Health.PortName != provision.DefaultPortName {
				t.Errorf("health names port %q, want %q", spec.Health.PortName, provision.DefaultPortName)
			}
			// The relationship: the named port must be one the spec publishes, and it
			// must be the GATEWAY port rather than whatever port happens to be first.
			if got := spec.PortNumber(spec.Health.PortName); got != ResolveGatewayPort(cfg.GatewayPort) {
				t.Errorf("health names a port resolving to %d, but the gateway port is %d",
					got, ResolveGatewayPort(cfg.GatewayPort))
			}
		})
	}
}

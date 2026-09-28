package agentspec

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/provision"
)

// 🔴 EVERY FIXTURE VALUE IN THIS FILE IS PAIRWISE DISTINCT AND DISTINCT FROM EVERY
// CONSTANT THE FILE NAMES. The mutant that hides best is an implementation that
// returns a constant the fixture also happens to produce — expected and wrong are
// then the same bytes and it survives a fully green suite. See
// TestTheStoreFixtureCanSeeAHardcodedConstant, the mechanical control for it.
//
// ⚠ THE TOKEN LITERAL DELIBERATELY CARRIES NO `bearer`/`authorization` NEIGHBOUR.
// tests/leakscan.py's credential rule matches a 20+ character value next to either
// word, so a fixture written as `Authorization: <token>` would trip the repository's
// own gate — which is the gate working, not a false positive.
const (
	// storeURL and storeToken are the configured coordinates. Neither equals the
	// other, neither is a prefix of the other, and neither appears in any constant
	// under test.
	storeURL   = "https://store.example.test:19001"
	storeToken = "fixture-store-credential-c4d17b2e93f6"

	// storeWorkspace is NOT DefaultWorkspacePath, so a mutant that hardcoded the
	// default cannot pass any path assertion here.
	storeWorkspace = "/srv/agent-work"
)

// storeConfig is a Config with the integration fully configured.
func storeConfig() Config {
	cfg := fixtureConfig()
	cfg.WorkspacePath = storeWorkspace
	cfg.CairnURL = storeURL
	cfg.CairnToken = storeToken
	return cfg
}

// storeOpts asks for the integration.
func storeOpts() Options {
	return Options{Instructions: "# supervisor\n\nbody\n", CairnEligible: true}
}

// fileAt returns the spec's file at path, if any.
func fileAt(spec provision.Spec, p string) (provision.File, bool) {
	for _, f := range spec.Files {
		if f.Path == p {
			return f, true
		}
	}
	return provision.File{}, false
}

// specBlob renders a spec as SEARCHABLE text.
//
// 🔴 `json.Marshal` ALONE IS BLIND TO EVERY FILE BODY, AND THAT BLINDNESS WAS
// MEASURED HERE RATHER THAN REASONED ABOUT. [provision.File.Content] is a []byte,
// which encoding/json base64-encodes — so a leak search over the marshalled spec
// finds no plaintext ANYWHERE in a file, including the credential file itself. The
// first draft of TestNoStoreCredentialValueAppearsOutsideItsSecretFile used it and
// its own positive control failed, which is the entire reason that control is
// there: without it the guard would have reported a clean zero over content it
// structurally could not see.
//
// So this walks the struct AND appends every file body verbatim. Anything added to
// [provision.Spec] that is not a []byte is still covered by the marshalled half.
func specBlob(t *testing.T, spec provision.Spec) string {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var b strings.Builder
	b.Write(raw)
	for _, f := range spec.Files {
		b.WriteString("\n")
		b.WriteString(f.Path)
		b.WriteString("\n")
		b.Write(f.Content)
	}
	return b.String()
}

// initEntriesMentioning counts Init entries containing sub.
func initEntriesMentioning(spec provision.Spec, sub string) int {
	n := 0
	for _, e := range spec.Init {
		if strings.Contains(e, sub) {
			n++
		}
	}
	return n
}

// --------------------------------------------------------------------------
// The two paths, pinned by VALUE.
// --------------------------------------------------------------------------

// TestTheTwoCairnPathsArePinnedByValueAndNotByTheirOwnConstants exists because a
// mutation sweep found the hole it fills.
//
// 🔴 EVERY OTHER TEST IN THIS FILE REFERS TO THE PATHS THROUGH THEIR CONSTANTS,
// WHICH MEANS NONE OF THEM CAN SEE THE CONSTANT MOVE. Measured: a sweep whose
// POSITIVE CONTROL was "change CairnConfigPath to another path" reported SURVIVED
// across the whole suite — every assertion moved with the mutant, because expected
// and actual were the same symbol. That is the textbook shape of comparing an
// implementation to itself, and the control is the only reason it was visible.
//
// So the values are written out here, and each one's REASON is a different kind of
// claim:
//
//   - CairnConfigPath is OURS, and the environment variable is what makes an
//     arbitrary path legal. Moving it without moving the variable would leave the
//     client reading its HOME-derived default in an image whose HOME we have never
//     measured.
//   - cairnWrapperPath is a CONTRACT with the runtime image: it must be a directory
//     already on PATH. The autosave daemon is placed in the same one, which is the
//     evidence that this project already relies on it existing.
func TestTheTwoCairnPathsArePinnedByValueAndNotByTheirOwnConstants(t *testing.T) {
	if CairnConfigPath != "/etc/muster/cairn/env" {
		t.Errorf("CairnConfigPath is %q, want \"/etc/muster/cairn/env\".\n"+
			"If the move is intended, retype the literal here AND check that %s still points at "+
			"it — the whole reason the path may be chosen freely is that the variable names it.",
			CairnConfigPath, EnvCairnConfig)
	}
	if cairnWrapperPath != "/usr/local/bin/cairn" {
		t.Errorf("cairnWrapperPath is %q, want \"/usr/local/bin/cairn\".\n"+
			"The file IS the command, so its directory must already be on the image's PATH.",
			cairnWrapperPath)
	}
	if EnvCairnConfig != "CAIRN_CONFIG" {
		t.Errorf("EnvCairnConfig is %q, want \"CAIRN_CONFIG\" — the name the client reads at the "+
			"pinned revision. A rename here leaves the client on its HOME-derived default and the "+
			"credential unread.", EnvCairnConfig)
	}

	// The relationship the first assertion's message names: the variable must
	// actually carry the path into the instance.
	spec := mustBuild(t, fixtureAgent(), storeConfig(), storeOpts())
	got, ok := envValue(spec, "CAIRN_CONFIG")
	if !ok {
		t.Fatal("the instance's environment does not name CAIRN_CONFIG, so the client reads its " +
			"HOME-derived default and never sees the credential")
	}
	if got != "/etc/muster/cairn/env" {
		t.Errorf("CAIRN_CONFIG = %q, want the credential's own path \"/etc/muster/cairn/env\"", got)
	}
	// And the wrapper must be placed at a path spelled here rather than derived.
	if _, ok := fileAt(spec, "/usr/local/bin/cairn"); !ok {
		t.Errorf("no file at /usr/local/bin/cairn; the spec has %v", filePaths(spec))
	}
	// The daemon shares the directory, which is what makes the PATH claim evidenced
	// rather than assumed.
	if !strings.HasPrefix(autosavePath, "/usr/local/bin/") {
		t.Errorf("the autosave daemon is at %q, so /usr/local/bin is no longer a directory this "+
			"project relies on being on PATH and the wrapper's placement needs its own argument",
			autosavePath)
	}
}

// --------------------------------------------------------------------------
// The ledger. An independently-spelled second copy of the revision and its
// module list.
// --------------------------------------------------------------------------

// TestCairnRevAndLibModulesAreOneLedger is the guard the rev's own comment
// promises.
//
// 🔴 IT IS THE ONLY THING THAT CAN SEE A MISSING MODULE, because
// TestCairnFetchAndProbeCoverTheSameModules structurally cannot: the fetch list and
// the import probe are both generated from [cairnLibModules], so an element deleted
// from that slice disappears from BOTH sides and the install reports success for a
// client missing a module. This ledger is a second spelling, so the two can
// disagree and be caught.
//
// The ledger was derived from the AUTHORITATIVE source rather than from the
// implementation: `git ls-tree --name-only <rev> lib/` in a clone of
// github.com/ZacxDev/cairn at this exact revision returns these nine `.py` files
// plus a `README.md`. A second, differently-failing reading of the same fact:
// `git cat-file -e <rev>` resolves, so the revision is a real object rather than a
// typo that would 404 every fetch.
//
// ⚠ IT IS AN INVARIANT GUARD, NOT REGRESSION COVERAGE. Nothing has yet bumped the
// revision in this repository, so this has never gone red on a real defect. It is
// here because the realistic way the install breaks is a rev bump made alone —
// which is a future edit, and this is what makes that edit loud.
func TestCairnRevAndLibModulesAreOneLedger(t *testing.T) {
	const wantRev = "a0745ed7973b0b8143c147e7cfb05e7d9d7b0a0a"
	wantModules := []string{
		"cairn_doctor",
		"cairn_instances",
		"entry_shape",
		"env_aliases",
		"host_identity",
		"subsystem_read_store",
		"subsystem_recall",
		"subsystem_resolver",
		"timeouts",
	}

	if cairnRev != wantRev {
		t.Errorf("cairnRev moved to %q without this ledger moving with it.\n"+
			"A rev bump can add, rename or drop a lib module — including one imported LAZILY, which "+
			"every probe the install runs would pass over. Re-derive the list at the new revision "+
			"(`git ls-tree --name-only <rev> lib/`) and update BOTH halves in one commit.", cairnRev)
	}
	if !slices.Equal(cairnLibModules, wantModules) {
		t.Errorf("cairnLibModules is %v, the ledger says %v.\n"+
			"🔴 TestCairnFetchAndProbeCoverTheSameModules CANNOT catch this — it generates the fetch "+
			"list AND the import probe from this same slice, so a deleted element leaves both sides "+
			"agreeing about a client that is missing a module.", cairnLibModules, wantModules)
	}

	// A revision that is not a full lowercase sha 404s every file. Under this
	// package's hard-failure contract that is a crash loop rather than a silent
	// degradation — loud, but still worth refusing here where it is cheap.
	if len(cairnRev) != 40 || strings.Trim(cairnRev, "0123456789abcdef") != "" {
		t.Errorf("cairnRev %q is not a full lowercase 40-hex sha; the raw-content host 404s anything "+
			"else and every install fails", cairnRev)
	}
	if strings.Trim(cairnRev, "0") == "" {
		t.Error("cairnRev is all zeros — every fetch 404s")
	}
	if len(cairnLibModules) == 0 {
		t.Fatal("cairnLibModules is empty, so the install's import probe would verify nothing and " +
			"pass vacuously")
	}
}

// --------------------------------------------------------------------------
// The fetch and the probe are generated from one list.
// --------------------------------------------------------------------------

// TestCairnFetchAndProbeCoverTheSameModules pins the coupling the module list's
// comment claims.
//
// 🔴 A FILE FETCHED BUT NEVER IMPORTED SHIPS UNVERIFIED; A MODULE IMPORTED BUT
// NEVER FETCHED FAILS EVERY INSTALL. The two lists are what makes the install a
// verification rather than a download, so they have to be the same set — and this
// asserts the RELATIONSHIP in both directions, not the presence of either list.
func TestCairnFetchAndProbeCoverTheSameModules(t *testing.T) {
	cmd, err := cairnInstallCommand(storeWorkspace)
	if err != nil {
		t.Fatalf("cairnInstallCommand: %v", err)
	}

	// The fetch list lives between `for f in ` and the `; do` that terminates it.
	// Taking `; do` as the terminator rather than end-of-line is deliberate: the
	// loop sits inside a `timeout … sh -c '…'` wrapper, so it is not at the start
	// of its line and does not end with one.
	fetchLine, probeLine := "", ""
	for _, l := range strings.Split(cmd, "\n") {
		if i := strings.Index(l, "for f in "); i >= 0 {
			rest := l[i+len("for f in "):]
			if j := strings.Index(rest, "; do"); j >= 0 {
				fetchLine = rest[:j]
			}
		}
		if strings.Contains(l, cairnProbeSentinel) {
			probeLine = l
		}
	}
	if fetchLine == "" || probeLine == "" {
		t.Fatalf("could not locate both the fetch loop and the import probe — this guard has lost "+
			"its anchors and would otherwise report a clean zero over lists it cannot see:\n%s", cmd)
	}

	fetched := strings.Fields(fetchLine)
	if len(fetched) == 0 || fetched[0] != "cairn" {
		t.Errorf("the fetch list must begin with the `cairn` entrypoint; got %v", fetched)
	}
	gotFetched := map[string]bool{}
	for _, f := range fetched[1:] {
		if !strings.HasPrefix(f, "lib/") || !strings.HasSuffix(f, ".py") {
			t.Errorf("unexpected fetch target %q — the client's modules must land as lib/*.py "+
				"siblings of the entrypoint, which is where its own sys.path insert looks", f)
			continue
		}
		gotFetched[strings.TrimSuffix(strings.TrimPrefix(f, "lib/"), ".py")] = true
	}

	i := strings.Index(probeLine, ";import ")
	if i < 0 {
		t.Fatalf("could not find the probe's import list in %q", probeLine)
	}
	rest := probeLine[i+len(";import "):]
	j := strings.Index(rest, ";")
	if j < 0 {
		t.Fatalf("unterminated import list in %q", probeLine)
	}
	gotImported := map[string]bool{}
	for _, m := range strings.Split(rest[:j], ",") {
		gotImported[m] = true
	}

	want := map[string]bool{}
	for _, m := range cairnLibModules {
		want[m] = true
	}
	for m := range want {
		if !gotFetched[m] {
			t.Errorf("module %q is in cairnLibModules but is never FETCHED — the install fails on "+
				"every instance", m)
		}
		if !gotImported[m] {
			t.Errorf("module %q is in cairnLibModules but is never IMPORTED by the probe — it ships "+
				"unverified", m)
		}
	}
	for m := range gotFetched {
		if !want[m] {
			t.Errorf("module %q is fetched but is not in cairnLibModules", m)
		}
	}
	for m := range gotImported {
		if !want[m] {
			t.Errorf("module %q is imported by the probe but is not in cairnLibModules, so it is "+
				"never fetched", m)
		}
	}

	// The probe has to be READ, not merely run: an unread probe passes on a python
	// that wrote a traceback to stderr and exited 0.
	if !strings.Contains(probeLine, "grep -q '^"+cairnProbeSentinel+"$'") {
		t.Errorf("the import probe's sentinel is not checked with an anchored grep — an unread "+
			"probe is not a probe:\n  %s", probeLine)
	}
}

// --------------------------------------------------------------------------
// Wall clock.
// --------------------------------------------------------------------------

// cairnWorstCaseSeconds sums every `timeout -k <kill> <wait>` bound in the entry.
//
// It also returns how many bounds it found, so a caller can refuse a ZERO — a
// parser that matched nothing would otherwise report a reassuring 0 seconds.
func cairnWorstCaseSeconds(t *testing.T, entry string) (total, bounds int) {
	t.Helper()
	const marker = "timeout -k "
	rest := entry
	for {
		i := strings.Index(rest, marker)
		if i < 0 {
			return total, bounds
		}
		fields := strings.Fields(rest[i+len(marker):])
		if len(fields) < 2 {
			t.Fatalf("a `timeout -k` has no kill grace and no wait after it: %q", rest[i:])
		}
		kill, err := strconv.Atoi(fields[0])
		if err != nil {
			t.Fatalf("kill grace %q is not an integer", fields[0])
		}
		wait, err := strconv.Atoi(fields[1])
		if err != nil {
			t.Fatalf("wait %q is not an integer", fields[1])
		}
		total += kill + wait
		bounds++
		rest = rest[i+len(marker):]
	}
}

// TestTheCairnInstallIsBoundedInWallClock recomputes the arithmetic in
// cairnInstallCommand's header from the rendered string, so the figure cannot
// drift into prose only.
//
// 🔴 EVERY STEP MUST BE BOUNDED, AND THE UNBOUNDED SHAPE IS THE DANGEROUS ONE.
// `--connect-timeout` bounds only the connect; a host that answers 200 and then
// trickles is what costs real time, and an unbounded probe on a process that
// ignores SIGTERM never returns at all. A blackholed host and an HTTP error are
// both fast, which is exactly why the slow case hides.
//
// ⚠ THE CEILING IS A STATED BUDGET, NOT A MEASURED ONE, AND SAYING SO IS THE POINT.
// Upstream derived its ceiling from a Helm chart's startupProbe. muster's
// kubernetes driver renders no probes at all and sets no activeDeadlineSeconds, so
// there is nothing in this repository to derive a real ceiling from — the effective
// one is a Deployment's progressDeadlineSeconds, whose kubernetes default is 600 s
// and which nothing here sets. This pins the arithmetic and a budget well inside
// that; it does not claim to have measured a startup envelope.
func TestTheCairnInstallIsBoundedInWallClock(t *testing.T) {
	cmd, err := cairnInstallCommand(storeWorkspace)
	if err != nil {
		t.Fatalf("cairnInstallCommand: %v", err)
	}

	// Written from the header's table, not read back from the code: one fetch-loop
	// bound (60 + 5) plus three probe bounds (10 + 1 each).
	const wantTotal = 65 + 11 + 11 + 11
	const wantBounds = 4

	total, bounds := cairnWorstCaseSeconds(t, cmd)
	if bounds != wantBounds {
		t.Errorf("found %d `timeout -k` bounds, want %d (the fetch loop, the staged --help, the "+
			"staged import probe, the on-PATH --help).\n"+
			"A missing bound is an unbounded step at instance startup, and a missing PROBE is an "+
			"unverified install — neither is a cosmetic change.\n%s", bounds, wantBounds, cmd)
	}
	if total != wantTotal {
		t.Errorf("worst-case wall clock is %d s, the header's arithmetic says %d s.\n"+
			"Either a bound moved and the comment did not, or a step was added without one. "+
			"Both halves have to move together.", total, wantTotal)
	}

	// A stated budget, generously inside the only real ceiling (see the note above).
	const budget = 150
	if total > budget {
		t.Errorf("worst case %d s exceeds the stated budget of %d s", total, budget)
	}

	// Every PROBE — a `--help` run or a `python3 -c` run — must carry a bound.
	//
	// ⚠ A PROBE IS IDENTIFIED BY WHAT IT RUNS, NOT BY MENTIONING python3. The
	// wrapper file's content names python3 inside a string and executes nothing;
	// matching on the interpreter's name alone would count it and then demand a
	// timeout on a line that runs no process.
	probes := 0
	for _, l := range strings.Split(cmd, "\n") {
		if !strings.Contains(l, "--help") && !strings.Contains(l, "python3 -c") {
			continue
		}
		probes++
		if !strings.Contains(l, "timeout -k 1 ") {
			t.Errorf("a probe is not bounded by `timeout -k 1` — the -k is load-bearing for a "+
				"process that ignores SIGTERM:\n  %s", l)
		}
	}
	// A count, so a probe DELETED cannot leave the loop matching nothing and
	// reporting a clean zero.
	if probes != 3 {
		t.Errorf("found %d bounded probes, want 3 (staged --help, staged import, on-PATH --help)", probes)
	}
}

// --------------------------------------------------------------------------
// The gate. This is the relationship guard for the whole integration.
// --------------------------------------------------------------------------

// cairnSurfaces reports, for one built spec, which of the three surfaces the
// integration touches are present.
//
// 🔴 IT RETURNS ALL THREE SO THE ASSERTION CAN PIN THE RELATIONSHIP RATHER THAN A
// COMPONENT. The failure this exists for is not "the credential is missing" — it
// is one surface moving without the others: an install with no credential, a
// credential with nothing that reads it, an environment variable naming a file
// that was not placed.
type cairnSurfaces struct {
	initEntries int
	wrapper     bool
	credential  bool
	env         bool
}

func readCairnSurfaces(spec provision.Spec) cairnSurfaces {
	var s cairnSurfaces
	s.initEntries = initEntriesMentioning(spec, cairnRev)
	_, s.wrapper = fileAt(spec, cairnWrapperPath)
	_, s.credential = fileAt(spec, CairnConfigPath)
	for _, e := range spec.Env {
		if e.Name == EnvCairnConfig {
			s.env = true
		}
	}
	return s
}

func (s cairnSurfaces) all() bool {
	return s.initEntries == 1 && s.wrapper && s.credential && s.env
}

func (s cairnSurfaces) none() bool {
	return s.initEntries == 0 && !s.wrapper && !s.credential && !s.env
}

// TestNothingCairnIsEmittedUnlessBothCoordinatesAreConfigured is the gate, over
// the whole matrix of ways it can be half-on.
//
// 🔴 BOTH-OR-NEITHER, AND "NEITHER" MEANS *NOTHING*, NOT "A HARMLESS EXTRA". A URL
// with no token is useless in exactly the case a credential file exists for, and a
// token with no URL has nowhere to go — so a half-configured installation must look
// exactly like an unconfigured one. The shipping default is both unset.
//
// 🔴 AND IT ASSERTS THE THREE SURFACES MOVE TOGETHER. Each row checks the install
// step, the two files and the environment variable as ONE answer. A guard that
// checked only the credential would stay green while an install step ran for an
// agent with no credential, which is the shape that spends a startup budget for
// nothing; a guard that checked only the install would stay green while a
// read+write credential was placed for an agent that cannot use it.
//
// ⚠ EVERY ROW ALSO CHECKS THE CREDENTIAL BYTES ARE ABSENT FROM THE WHOLE SPEC, not
// just that the file is missing. The hazard is a value LEAKING to another surface,
// and a path-based check cannot see that.
func TestNothingCairnIsEmittedUnlessBothCoordinatesAreConfigured(t *testing.T) {
	cases := []struct {
		name     string
		eligible bool
		url      string
		token    string
		wantAll  bool
	}{
		{name: "both set and eligible", eligible: true, url: storeURL, token: storeToken, wantAll: true},
		{name: "not eligible", eligible: false, url: storeURL, token: storeToken},
		{name: "no url", eligible: true, url: "", token: storeToken},
		{name: "no token", eligible: true, url: storeURL, token: ""},
		{name: "blank url", eligible: true, url: "   ", token: storeToken},
		{name: "blank token", eligible: true, url: storeURL, token: "\t\n"},
		{name: "nothing configured", eligible: true},
		{name: "the shipping default", eligible: false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := storeConfig()
			cfg.CairnURL, cfg.CairnToken = c.url, c.token
			opts := storeOpts()
			opts.CairnEligible = c.eligible

			spec := mustBuild(t, fixtureAgent(), cfg, opts)
			got := readCairnSurfaces(spec)

			if c.wantAll {
				if !got.all() {
					t.Fatalf("the integration is configured and the agent is eligible, but the spec "+
						"is only partly wired: %+v.\nAll three surfaces must be present together — "+
						"the install step, the wrapper, the credential file and the variable naming "+
						"it.", got)
				}
				return
			}
			if !got.none() {
				t.Fatalf("eligible=%v url=%q token=%q emitted %+v; want NOTHING.\n"+
					"A half-configured installation must be indistinguishable from an unconfigured "+
					"one: the client refuses every store call locally without a credential, so an "+
					"install here spends a startup budget for a command that cannot work.",
					c.eligible, c.url, c.token, got)
			}
			// The values must not have reached ANY surface, including ones this
			// test does not know the names of. Through specBlob, which sees file
			// BODIES — a plain json.Marshal does not (see its note).
			blob := specBlob(t, spec)
			for _, secret := range []string{c.url, c.token} {
				if strings.TrimSpace(secret) == "" {
					continue
				}
				if strings.Contains(blob, secret) {
					t.Errorf("the value %q appears in the built spec even though the integration is "+
						"off", secret)
				}
			}
		})
	}
}

// TestTheStoreCredentialIsAConfidentialFileAtMode0600 pins the credential's
// content, its mode and its confidentiality.
//
// 🔴 THE CONTENT IS AN INDEPENDENTLY-SPELLED LITERAL. Building the expected string
// from the same helper the implementation uses would compare the implementation to
// itself and pass for any implementation, including one that wrote the URL into
// both keys. This is written from the client's own contract: the keys it reads at
// the pinned revision, one `KEY=value` per line, newline-terminated.
func TestTheStoreCredentialIsAConfidentialFileAtMode0600(t *testing.T) {
	spec := mustBuild(t, fixtureAgent(), storeConfig(), storeOpts())

	f, ok := fileAt(spec, CairnConfigPath)
	if !ok {
		t.Fatalf("no credential file at %s; the spec has %v", CairnConfigPath, filePaths(spec))
	}

	const want = "CAIRN_URL=https://store.example.test:19001\n" +
		"CAIRN_TOKEN=fixture-store-credential-c4d17b2e93f6\n"
	if got := string(f.Content); got != want {
		t.Errorf("credential file content is\n%q\nwant\n%q\n"+
			"The keys are the ones the client at the pinned revision reads; one per line, "+
			"newline-terminated, nothing else in the file.", got, want)
	}

	if !f.Secret {
		t.Error("the credential file is not marked Secret, so a driver with no confidential store " +
			"would place a read+write store key in an ordinary ConfigMap instead of refusing the spec")
	}
	if got := f.EffectiveMode(); got != fs.FileMode(0o600) {
		t.Errorf("credential file mode is %v, want 0600", got)
	}

	// 🔴 THE PRE-RENAME ALIASES ARE DELIBERATELY ABSENT — see cairn.go's header. A
	// reintroduction is not a harmless extra: the client emits one deprecation line
	// per old key per process, forever, and this installation cannot produce a
	// client old enough to need them.
	for _, alias := range []string{"SUBSYSTEM_STORE_URL", "SUBSYSTEM_STORE_TOKEN"} {
		if strings.Contains(string(f.Content), alias) {
			t.Errorf("the credential file writes the pre-rename key %q. The pinned revision reads "+
				"the current names and warns on every old one it finds; a compatibility window for "+
				"a client this installation cannot produce is noise, not safety.", alias)
		}
	}
}

// TestNoStoreCredentialValueAppearsOutsideItsSecretFile is the leak guard.
//
// 🔴 IT SEARCHES BY VALUE ACROSS THE WHOLE SPEC, NOT BY KEY IN A LIST IT KNOWS
// ABOUT. The mutant this is built for is a value copied to a NEW surface — an env
// entry, a label, an Init line, an opaque Config key — and a guard enumerating the
// surfaces it already thought of cannot see one it has not. So it marshals
// everything, removes the one file that is allowed to hold the bytes, and searches
// what is left.
//
// ⚠ BOTH VALUES ARE SECRET HERE, NOT JUST THE TOKEN. The URL is the store's address
// and travels in the same confidential file; a guard for the token alone would pass
// a spec that leaked the URL into a label.
func TestNoStoreCredentialValueAppearsOutsideItsSecretFile(t *testing.T) {
	spec := mustBuild(t, fixtureAgent(), storeConfig(), storeOpts())

	// Positive control FIRST: the values must be findable at all, or a green here
	// is a claim about a search that could never match. This control is what
	// caught the first draft searching base64 — see specBlob.
	full := specBlob(t, spec)
	for _, secret := range []string{storeURL, storeToken} {
		if !strings.Contains(full, secret) {
			t.Fatalf("positive control: %q is not findable in the built spec AT ALL, so the search "+
				"below could not have found a leak either", secret)
		}
	}

	// Now the same spec with the one file that legitimately holds them removed.
	stripped := spec
	stripped.Files = nil
	for _, f := range spec.Files {
		if f.Path == CairnConfigPath {
			continue
		}
		stripped.Files = append(stripped.Files, f)
	}
	if len(stripped.Files) != len(spec.Files)-1 {
		t.Fatalf("expected to remove exactly one file, removed %d", len(spec.Files)-len(stripped.Files))
	}

	blob := specBlob(t, stripped)
	for name, secret := range map[string]string{"CairnURL": storeURL, "CairnToken": storeToken} {
		if strings.Contains(blob, secret) {
			t.Errorf("Config.%s's value appears somewhere in the spec other than the confidential "+
				"file at %s.\nEvery other surface — env, labels, Init, the opaque runtime config — is "+
				"handled by a driver that may log, diff or ConfigMap it.", name, CairnConfigPath)
		}
	}
}

// TestADriverWithNoConfidentialStoreRefusesTheCredentialRatherThanPlacingIt is the
// seam between this package's Secret flag and a driver's capabilities.
//
// 🔴 IT GOES THROUGH Create, NOT JUST CheckSpec. CheckSpec going red proves the
// RULE exists; Create going red proves the driver reaches it. A driver that
// declared the rule and forgot to call it would pass the first check and place a
// read+write store key in a non-confidential store.
func TestADriverWithNoConfidentialStoreRefusesTheCredentialRatherThanPlacingIt(t *testing.T) {
	spec := mustBuild(t, fixtureAgent(), storeConfig(), storeOpts())

	// Everything ON except the confidential store, so a refusal is attributable to
	// that capability and not to a spec the driver dislikes for another reason.
	caps := provision.DefaultNoopCapabilities
	caps.Secrets = false
	drv := provision.MustNewNoop(provision.NoopCapabilities(caps))

	// Positive control: with the capability ON, the very same spec is accepted.
	if err := provision.CheckSpec(provision.DefaultNoopCapabilities, spec); err != nil {
		t.Fatalf("positive control: a fully-capable driver refuses the spec (%v), so the refusal "+
			"below would not be attributable to the missing capability", err)
	}

	err := drv.Create(context.Background(), spec)
	if err == nil {
		t.Fatal("a driver with no confidential store CREATED from a spec carrying the store " +
			"credential; want a refusal. Silently downgrading it would put a read+write key to the " +
			"operator's knowledge store into an ordinary, readable object.")
	}
	if direct := provision.CheckSpec(caps, spec); direct == nil {
		t.Error("provision.CheckSpec accepted the same spec under the same capabilities, so " +
			"Create's error came from somewhere else and this test is not measuring what it claims")
	}
}

// --------------------------------------------------------------------------
// The seam that a verbatim port would have got wrong.
// --------------------------------------------------------------------------

// TestTheClientIsInstalledWhereBothContainersCanSeeIt is the guard for the single
// defect a faithful port of the upstream shell would have shipped.
//
// 🔴 AN Init STEP AND THE AGENT PROCESS DO NOT SHARE A FILESYSTEM. The kubernetes
// driver renders Init as an initContainer, so anything it writes outside a shared
// volume is discarded before the agent ever runs — and the only writable surface
// both see is the WORKSPACE. Upstream's entry downloaded the client to /opt and
// linked it into /usr/local/bin; ported verbatim it would have installed nothing,
// with a green suite and a success echo in the log. k8s.workspaceVolumeName's own
// comment calls this "the single most common way an init container is written and
// silently does nothing".
//
// So this pins the RELATIONSHIP: every path the install WRITES is under the
// workspace, and the one path outside it — the command on PATH — is a
// [provision.File], which the driver places into both containers.
func TestTheClientIsInstalledWhereBothContainersCanSeeIt(t *testing.T) {
	cfg := storeConfig()
	spec := mustBuild(t, fixtureAgent(), cfg, storeOpts())

	if len(spec.Init) == 0 {
		t.Fatal("no Init entry, so this guard is measuring nothing")
	}
	entry := spec.Init[0]

	// Every destination the entry writes to. Read off the verbs that create or
	// move something, so a new write with a new verb is a parse failure rather
	// than a silent pass.
	writes := 0
	for _, l := range strings.Split(entry, "\n") {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		var targets []string
		switch f[0] {
		case "mkdir":
			targets = f[len(f)-1:]
		case "rm":
			targets = f[len(f)-1:]
		case "mv":
			targets = f[len(f)-2:]
		default:
			continue
		}
		for _, target := range targets {
			writes++
			if !strings.HasPrefix(target, cfg.WorkspacePath+"/") {
				t.Errorf("the install writes to %q, which is OUTSIDE the workspace %q.\n"+
					"🔴 Init runs in a separate container whose own filesystem is discarded when it "+
					"exits. A path outside the shared workspace volume is written and then lost, and "+
					"the install reports success for a client the agent will never see.",
					target, cfg.WorkspacePath)
			}
		}
	}
	// A count, so a parser that stopped matching cannot report a clean zero.
	if writes < 4 {
		t.Errorf("found only %d write targets in the install entry; expected at least 4 (stage "+
			"removal, stage creation, old-install removal, the swap). A parser matching nothing "+
			"would report no violations either.", writes)
	}

	// The one thing that must be OUTSIDE the workspace is the command on PATH, and
	// it must be a File rather than something the shell writes.
	wrapper, ok := fileAt(spec, cairnWrapperPath)
	if !ok {
		t.Fatalf("no file at %s — the command on PATH cannot be created by the install script, "+
			"because that script's writes do not survive into the agent's container",
			cairnWrapperPath)
	}
	if strings.HasPrefix(cairnWrapperPath, cfg.WorkspacePath+"/") {
		t.Errorf("the wrapper is inside the workspace at %s, so it is not on PATH", cairnWrapperPath)
	}
	if got := wrapper.EffectiveMode(); got&0o111 == 0 {
		t.Errorf("the wrapper's mode is %v, which is not executable — the file IS the command", got)
	}
	if wrapper.Secret {
		t.Error("the wrapper is marked Secret; it holds no credential and marking it one would " +
			"make a driver without a confidential store refuse the whole spec over a shell stub")
	}

	// And it must point at where the install actually put the client, or the two
	// halves each work and the pair does not.
	wantTarget := cairnEntrypoint(cfg.WorkspacePath)
	if !strings.Contains(string(wrapper.Content), wantTarget) {
		t.Errorf("the wrapper does not exec %q:\n%s", wantTarget, wrapper.Content)
	}
	// ⚠ THE ENTRYPOINT PATH IS NOT SPELLED IN THE INSTALL, AND THAT IS CORRECT
	// rather than a gap: the entrypoint arrives as a fetched file inside the
	// STAGING directory, and the swap renames that directory into place. So what
	// the install must produce is the DIRECTORY the wrapper's target sits in, plus
	// a fetch of the entrypoint into the staging copy. An earlier draft of this
	// assertion looked for the joined path and failed — a finding about the
	// assertion, not the code, and worth recording so the next reader does not
	// "fix" the install to satisfy it.
	wantDir := cairnClientDir(cfg.WorkspacePath)
	if !strings.Contains(entry, "mv "+cairnStageDir(cfg.WorkspacePath)+" "+wantDir) {
		t.Errorf("the install never moves the verified staging copy to %q, so the wrapper execs a "+
			"path nothing creates:\n%s", wantDir, entry)
	}
	if wantTarget != wantDir+"/cairn" {
		t.Errorf("the wrapper's target %q is not the entrypoint inside %q, so the two halves are "+
			"about different paths", wantTarget, wantDir)
	}
	// Behavioural half: the workspace is a Config field, so both sides must MOVE
	// with it. A structural check type-checks past a hardcoded default.
	other := storeConfig()
	other.WorkspacePath = "/opt/other-workspace"
	otherSpec := mustBuild(t, fixtureAgent(), other, storeOpts())
	otherWrapper, ok := fileAt(otherSpec, cairnWrapperPath)
	if !ok {
		t.Fatal("no wrapper in the second spec")
	}
	if strings.Contains(string(otherWrapper.Content), cfg.WorkspacePath) {
		t.Errorf("the wrapper still names the FIRST workspace %q after the config changed to %q — "+
			"the path is hardcoded somewhere", cfg.WorkspacePath, other.WorkspacePath)
	}
	if !strings.Contains(string(otherWrapper.Content), cairnEntrypoint(other.WorkspacePath)) {
		t.Errorf("the wrapper does not follow Config.WorkspacePath:\n%s", otherWrapper.Content)
	}
}

// TestTheCairnInstallDoesNotSwallowItsOwnFailures pins the deliberate inversion of
// the upstream contract.
//
// 🔴 UPSTREAM'S ENTRY COULD NOT FAIL, AND THAT WAS THE PROBLEM. It was wrapped in
// `{ … } || :` with WARNING echoes, because a bare non-zero exit in that chart's
// startup script was a crash loop rather than a missing file — so nothing in the
// block could ever be OBSERVED to fail. muster's driver runs Init under `sh -eu`
// and states that a failing step should stop the instance. Importing the swallow
// would have imported the workaround without the problem.
//
// ⚠ AND THE COST IS REAL: a transient fetch failure now crash-loops the eligible
// instance. The argument for accepting it is that the prose shipped alongside TELLS
// the agent it has a working client, so a soft failure would ship exactly the
// falsehood the gate exists to prevent, with no channel to report it.
func TestTheCairnInstallDoesNotSwallowItsOwnFailures(t *testing.T) {
	cmd, err := cairnInstallCommand(storeWorkspace)
	if err != nil {
		t.Fatalf("cairnInstallCommand: %v", err)
	}

	for _, swallow := range []string{"|| :", "|| true", "; true", "|| echo", "set +e"} {
		if strings.Contains(cmd, swallow) {
			t.Errorf("the install entry contains %q, which prevents a failure from reaching the "+
				"driver.\nInit runs under `sh -eu` and a failing step is meant to stop the instance "+
				"with a log. A swallowed failure here ships prose claiming a client that is not "+
				"installed — the exact falsehood the configuration gate exists to prevent.", swallow)
		}
	}

	// The positive half: the entry must contain at least one construct whose
	// failure propagates, or the absence above is satisfied by an empty string.
	if !strings.Contains(cmd, "grep -q") {
		t.Error("the install entry has no failing-capable verification step at all, so the absence " +
			"of a swallow proves nothing")
	}

	// 🔴 `curl -sf` IS THE LOAD-BEARING HALF, AND THIS USED TO ASSERT ONLY THE OTHER
	// ONE. Measured against a real 404 on the raw-content host: `curl -s` exits 0
	// and writes a 14-byte body ("404: Not Found"), so `|| exit 1` never fires and
	// the loop walks past the 404 with the flag gone. `curl -sf` exits 22 on the
	// same URL and 0 on a URL that exists. So the `-f` is what turns an HTTP error
	// into a non-zero status; `|| exit 1` only propagates a status that already
	// exists. A sweep confirmed the gap rather than inferring it: `curl -sf` ->
	// `curl -s`, applied alone, SURVIVED all three packages while this test asserted
	// `|| exit 1` and nothing else.
	//
	// ⚠ THE PAYLOAD IMPACT IS BOUNDED, AND SAYING SO IS PART OF THE CLAIM. With a
	// 404 body written in place of a module, `python3 <body> --help` exits 1, so
	// probes 1 and 2 still fail the install. The defect this pins is a fetch loop
	// that reports success over missing files, not a client that ships broken.
	// ⚠ THE FAIL FLAG IS ACCEPTED IN EITHER SPELLING, so that re-ordering curl's
	// flags or writing `--fail` in full is not a red test. What must not change is
	// that the flag is THERE.
	var fetch string
	for _, l := range strings.Split(cmd, "\n") {
		if strings.Contains(l, "curl ") {
			fetch = l
			break
		}
	}
	if fetch == "" {
		t.Fatal("no `curl` line in the install entry, so the two assertions below would pass " +
			"vacuously over a string that fetches nothing")
	}
	if !strings.Contains(fetch, "-sf") && !strings.Contains(fetch, "--fail") {
		t.Errorf("the fetch does not pass curl's fail flag (`-f`/`--fail`):\n  %s\n\n"+
			"Without it curl exits 0 on an HTTP error and writes the error page as the file — "+
			"measured against a real 404: `curl -s` exits 0 with a 14-byte body, `curl -sf` exits "+
			"22 — so `|| exit 1` never fires and the loop reports success with a module replaced "+
			"by an error page.", fetch)
	}
	if !strings.Contains(fetch, "|| exit 1") {
		t.Errorf("the fetch loop does not `|| exit 1` on a failed curl:\n  %s\n\n"+
			"The fail flag makes curl's status non-zero; this is what carries that status out of "+
			"the `sh -c` subshell to the outer `-e`. Without it the loop swallows the failure it "+
			"just detected.", fetch)
	}
}

// TestTheCairnInstallHasNoHeredocAndNoMultiLinePythonPayload WAS HERE AND IS
// DELETED. It is recorded rather than silently dropped because a reader coming from
// upstream will look for it.
//
// 🔴 IT WAS THE WORKAROUND IMPORTED WITHOUT THE PROBLEM. Upstream's reason was
// measured: its chart re-indented every line of the value, so a heredoc terminator
// never matched its opener and a multi-line `python3 -c` payload was an
// IndentationError. That does not hold here, and the test's own doc comment
// conceded it — "muster's driver joins entries with \n and hands them to
// `sh -eu -c`, where a heredoc would in fact survive." A guard whose stated hazard
// cannot occur is the exact mistake cairn.go's header item 2 warns about, applied to
// a test: it cost a reader's attention and bought no coverage.
//
// The entry is still written with neither construct, and cairn.go's "NO HEREDOC"
// section says why that is a style preference here rather than a constraint.
//
// THE CLOSING CONDITION FOR RE-ADDING IT is the first driver that renders
// [provision.Spec.Init] BY A DIFFERENT JOINING RULE than "\n" into `sh -eu -c`. Then
// the hazard is real here, and the person who checks it is the reviewer of whichever
// change adds that driver.

// TestAWorkspacePathThatWouldBreakTheInterpolationIsRefused covers the one place
// caller data reaches two different parsers.
//
// 🔴 THE WORKSPACE PATH IS SPLICED INTO A SHELL COMMAND *AND* INTO A PYTHON STRING
// LITERAL. A quote, a dollar or a backtick breaks out of one or both, and the
// failure would appear at container start as a broken image rather than at build
// time as a configuration mistake.
func TestAWorkspacePathThatWouldBreakTheInterpolationIsRefused(t *testing.T) {
	// Positive control: the fixture path must be ACCEPTED, or every row below
	// passes because the function refuses everything.
	if _, err := cairnInstallCommand(storeWorkspace); err != nil {
		t.Fatalf("positive control: the ordinary workspace %q is refused (%v), so the refusals "+
			"below prove nothing", storeWorkspace, err)
	}

	for _, bad := range []string{
		`/srv/a'b`,
		`/srv/a"b`,
		"/srv/a`b",
		`/srv/a$b`,
		`/srv/a b`,
		"/srv/a\tb",
		`/srv/a\b`,
	} {
		t.Run(fmt.Sprintf("%q", bad), func(t *testing.T) {
			if _, err := cairnInstallCommand(bad); err == nil {
				t.Errorf("workspace %q was accepted; it would break the shell or the python "+
					"string literal the path is spliced into", bad)
			}
			// And through the public door, so the refusal is reachable rather than
			// only true of the helper.
			cfg := storeConfig()
			cfg.WorkspacePath = bad
			if _, err := Build(fixtureAgent(), cfg, storeOpts()); err == nil {
				t.Errorf("Build accepted workspace %q with the integration on", bad)
			}
			// ⚠ THE SAME PATH MUST STILL BUILD WITH THE INTEGRATION OFF. This
			// refusal belongs to the cairn interpolation, not to workspace paths in
			// general, and widening it would break installations that never
			// configured a store.
			off := storeConfig()
			off.WorkspacePath = bad
			off.CairnURL, off.CairnToken = "", ""
			if _, err := Build(fixtureAgent(), off, Options{}); err != nil {
				t.Errorf("Build refused workspace %q with the integration OFF (%v); the refusal "+
					"belongs to the cairn interpolation, not to workspace paths", bad, err)
			}
		})
	}
}

// TestTheInitLedgerIsExactAndTheClientInstallComesFirst pins Init's composition.
//
// 🔴 A COUNT AND AN ORDER, NOT A CONTAINS. Init is a shell contract joined with
// newlines, so an entry appended anywhere is invisible to a check that only asks
// whether the cairn step is present. The ORDER matters in one direction: the
// autosave entry BACKGROUNDS a daemon, so a step after it races the daemon's first
// snapshot while a step before it cannot.
func TestTheInitLedgerIsExactAndTheClientInstallComesFirst(t *testing.T) {
	// A repo-backed agent, so the autosave entry is present too and the ordering
	// claim has two entries to be about.
	a := fixtureAgent()
	if a.Repo == "" {
		t.Fatal("the fixture agent has no repository, so there is no second Init entry and this " +
			"guard cannot see an ordering at all")
	}
	spec := mustBuild(t, a, storeConfig(), storeOpts())

	if len(spec.Init) != 2 {
		t.Fatalf("Init has %d entries, want exactly 2 (the client install, then the autosave "+
			"daemon):\n%#v", len(spec.Init), spec.Init)
	}
	if !strings.Contains(spec.Init[0], cairnRev) {
		t.Errorf("Init[0] is not the client install:\n%s", spec.Init[0])
	}
	if !strings.Contains(spec.Init[1], autosavePath) {
		t.Errorf("Init[1] is not the autosave invocation:\n%s", spec.Init[1])
	}
	if !strings.HasSuffix(strings.TrimSpace(spec.Init[1]), "&") {
		t.Errorf("Init[1] is not backgrounded, so the ordering claim above is about something else:\n%s",
			spec.Init[1])
	}

	// With the integration off, the autosave entry is the ONLY one — i.e. the
	// install did not leave an empty string behind.
	off := storeConfig()
	off.CairnURL, off.CairnToken = "", ""
	offSpec := mustBuild(t, a, off, Options{})
	if len(offSpec.Init) != 1 {
		t.Errorf("with the integration off Init has %d entries, want exactly 1:\n%#v",
			len(offSpec.Init), offSpec.Init)
	}
}

// TestTheStoreFixtureCanSeeAHardcodedConstant is the mechanical control for this
// file's fixture discipline.
//
// 🔴 A FIXTURE THAT CAN ONLY PRODUCE A CONSTANT'S OWN VALUE CANNOT SEE A MUTANT
// THAT HARDCODES THE CONSTANT — it survives a fully green suite. This feeds values
// the constants cannot equal and watches the output move.
func TestTheStoreFixtureCanSeeAHardcodedConstant(t *testing.T) {
	cfg := storeConfig()
	if cfg.WorkspacePath == DefaultWorkspacePath {
		t.Fatalf("storeConfig().WorkspacePath equals DefaultWorkspacePath (%q), so a mutant that "+
			"hardcoded the default would survive every path assertion here", DefaultWorkspacePath)
	}
	if cfg.CairnURL == cfg.CairnToken {
		t.Fatal("the URL and the token are the same bytes, so a mutant writing one into both keys " +
			"would survive the credential-content assertion")
	}
	for _, pair := range [][2]string{
		{cfg.CairnURL, CairnConfigPath},
		{cfg.CairnToken, CairnConfigPath},
		{cfg.CairnURL, cairnWrapperPath},
		{cfg.CairnToken, cairnRev},
	} {
		if strings.Contains(pair[1], pair[0]) || strings.Contains(pair[0], pair[1]) {
			t.Errorf("fixture value %q overlaps the constant %q, so a mutant substituting one for "+
				"the other would survive", pair[0], pair[1])
		}
	}

	// And the output moves with the input, which is what "the fixture can see it"
	// means. Two DIFFERENT credentials, so a mutant that cached the first is caught.
	first := mustBuild(t, fixtureAgent(), cfg, storeOpts())
	second := cfg
	second.CairnToken = "a-different-fixture-credential-7ae0fb14"
	secondSpec := mustBuild(t, fixtureAgent(), second, storeOpts())

	f1, _ := fileAt(first, CairnConfigPath)
	f2, _ := fileAt(secondSpec, CairnConfigPath)
	if string(f1.Content) == string(f2.Content) {
		t.Error("changing Config.CairnToken did not change the credential file, so its value is " +
			"hardcoded somewhere")
	}
	if strings.Contains(string(f2.Content), cfg.CairnToken) {
		t.Errorf("the second credential file still carries the FIRST token:\n%s", f2.Content)
	}
}

// TestTheGoldenViewRedactsAConfidentialFileRatherThanOnlyAConfidentialVariable
// closes a gap this change made reachable.
//
// 🔴 THE GOLDEN IS A CHECKED-IN FILE. goldenView redacted Spec.Secrets — the
// confidential ENVIRONMENT — and that was complete while nothing in this package
// produced a confidential FILE. cairn.go now does, so a store credential added to
// the golden's fixture would have been committed verbatim. The redaction branches on
// the SAME flag a driver branches on, which is what makes a future secret file
// covered without anybody remembering.
//
// ⚠ IT IS AN INVARIANT GUARD. The golden's own fixture does not configure the store,
// so this has never gone red on a committed leak — it is here so that the day
// somebody adds a confidential file to that fixture, the leak does not happen.
func TestTheGoldenViewRedactsAConfidentialFileRatherThanOnlyAConfidentialVariable(t *testing.T) {
	spec := mustBuild(t, fixtureAgent(), storeConfig(), storeOpts())

	// Positive control: the bytes must be present before redaction, or the absence
	// after it proves nothing.
	if !strings.Contains(specBlob(t, spec), storeToken) {
		t.Fatal("positive control: the credential is not in the built spec, so redacting it " +
			"cannot be observed")
	}

	view := goldenView(spec)
	if strings.Contains(specBlob(t, view), storeToken) {
		t.Error("goldenView leaves a confidential file's bytes intact, so a golden regenerated " +
			"from a spec with a store credential would commit it")
	}
	// The PATH must survive — a regression that moved the credential elsewhere is
	// exactly what the golden exists to make loud.
	if _, ok := fileAt(view, CairnConfigPath); !ok {
		t.Errorf("goldenView dropped the confidential file entirely; the golden must still show "+
			"that a file exists at %s", CairnConfigPath)
	}
	// And a NON-secret file's bytes must still be visible, or the redaction is
	// indiscriminate and the golden stops being a useful diff.
	if !strings.Contains(specBlob(t, view), "#!/bin/sh") {
		t.Error("goldenView redacted the non-confidential wrapper too, which would hide the change " +
			"a reviewer most needs to see")
	}
}

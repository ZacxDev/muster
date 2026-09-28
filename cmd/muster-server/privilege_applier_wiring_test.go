package main

import (
	"log"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// THE PRIVILEGE TIER'S WIRING, AND THE ONE PROPERTY THAT IS NOT ABOUT THIS FILE.
//
// 🔴 A nil PrivilegeApply IS THE ONLY ONE OF THE THREE TIERS WHOSE MISTAKE FAILS
// *OPEN*. For Provisioner and Gateway, a typed nil in the interface field makes a
// wrapper STOP REFUSING — bad, and loudly so. For this one the consequence
// inverts: api.Extensions.defects fires on `PrivilegeApply == nil`, so a typed nil
// makes the readiness DEFECT GO QUIET. /readyz then reports ready for the exact
// deployment the check exists to refuse, and the first grant nil-derefs inside a
// handler. One missing `if` turns fail-closed into fail-open, which is why the
// assignment in main.go carries its own paragraph and why the ledger below exists.
// ---------------------------------------------------------------------------

// privilegeApplySiteLedger is every place in this binary's non-test sources that
// ASSIGNS to api.Extensions.PrivilegeApply, with what guards it there.
//
// 🔴 IT IS AN ASSERTED LEDGER AND THE TEST FAILS WHEN THE SET GROWS *OR* SHRINKS,
// which is the only shape that catches the case worth catching. A SECOND
// assignment — a later tier, a test hook, a "temporary" default — is how a
// fail-open assignment arrives without the nil-check the one below carries, and
// a plain allowlist would absorb it silently. Shrinking is a failure too: if
// nothing assigns the field, MUSTER_AGENT_PRIVILEGE_APPLY is an armed switch that
// does nothing, /readyz refuses every provisioner deployment with a database, and
// nothing in this package would go red.
//
// ⚠ IT IS KEYED BY FILE RATHER THAN file:line, because line numbers move whenever
// anything above them is edited and a ledger that reddens on an unrelated edit is
// a ledger people delete. Same reasoning as internal/modulegate's citation ledger.
var privilegeApplySiteLedger = map[string]string{
	"main.go": "the ONE assignment, inside `if priv != nil`. The nil-check is not " +
		"pedantry: buildAgentPlane returns a CONCRETE *agentprivilege.Applier, and " +
		"assigning a typed nil pointer to the interface field yields a NON-nil " +
		"interface — which makes api.Extensions.defects' `PrivilegeApply == nil` " +
		"conjunct false and /readyz report ready over an applier that nil-derefs.",
}

// privilegeApplyAssign matches an assignment to the field, in either of the two
// spellings Go allows for it.
var privilegeApplyAssign = regexp.MustCompile(`\.PrivilegeApply\s*[:]?=[^=]`)

// TestEverySitePopulatingPrivilegeApplyIsOnTheLedger.
func TestEverySitePopulatingPrivilegeApplyIsOnTheLedger(t *testing.T) {
	files := serverSources(t)

	// 🔴 POSITIVE CONTROL ON THE PATTERN, REPORTED AS A NUMBER. A regexp that
	// stopped matching returns an empty set, and an empty set passes a
	// "every match is ledgered" assertion perfectly while measuring nothing —
	// which is exactly the zero this project's rules call indistinguishable from a
	// clean tree. `ext.PrivilegeApply = priv` is known to exist in main.go.
	found := map[string][]string{}
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		lines := strings.Split(string(body), "\n")
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if !privilegeApplyAssign.MatchString(line) {
				continue
			}
			found[f] = append(found[f], trimmed)

			// 🔴 THE GUARD ITSELF, CHECKED TEXTUALLY — AND IT IS A TEXT GUARD, WHICH IS
			// WORSE THAN A BEHAVIOURAL ONE AND IS THE BEST AVAILABLE HERE. A reader can
			// walk it by spelling the same check differently, and it is written down as a
			// known limit rather than presented as coverage.
			//
			// WHY THERE IS NO BEHAVIOURAL VERSION, MEASURED RATHER THAN ASSUMED: the only
			// assembler is buildApp, and the typed-nil trap is observable only when the
			// banner or /readyz reaches the privilege arm — which needs ext.Privilege,
			// which buildApp builds ONLY inside its `cfg.Database != ""` branch. So a
			// test that could see it would need a live Postgres in this package, which
			// has none. The sibling trap for Provisioner IS behaviourally guarded
			// (TestTheDefaultConfigLeavesTheProvisionerInterfaceTRULYNil reads the
			// banner's LIFECYCLE arm, which needs no database) — the asymmetry is the
			// database, not a difference in how much the two matter. If anything this
			// one matters MORE: a typed nil here silences a readiness refusal instead of
			// tripping a wrapper, so it fails OPEN.
			//
			// ⚠ IT LOOKS AT THE PRECEDING SOURCE LINE, SKIPPING BLANKS AND COMMENTS,
			// because main.go's assignment carries a nine-line comment above it and a
			// naive `lines[i-1]` would read prose and report a missing guard.
			guarded := false
			for j := i - 1; j >= 0; j-- {
				prev := strings.TrimSpace(lines[j])
				if prev == "" || strings.HasPrefix(prev, "//") {
					continue
				}
				guarded = strings.Contains(prev, "!= nil") || strings.Contains(prev, "== nil")
				break
			}
			if !guarded {
				t.Errorf("%s:%d assigns api.Extensions.PrivilegeApply with no nil-check on the "+
					"line above it:\n    %s\n"+
					"    buildAgentPlane returns a CONCRETE *agentprivilege.Applier. Assigning a "+
					"typed nil pointer to an interface field yields a NON-nil interface, which "+
					"makes api.Extensions.defects' `PrivilegeApply == nil` conjunct FALSE — so "+
					"/readyz reports READY for the deployment that check exists to refuse, and "+
					"the first grant nil-derefs inside a handler. This is the one tier whose "+
					"missing guard fails OPEN rather than tripping a wrapper.", f, i+1, trimmed)
			}
		}
	}
	if len(found) == 0 {
		t.Fatalf("positive control FAILED: no assignment to .PrivilegeApply was found in any of "+
			"the %d non-test source(s) of this package. Either the pattern has stopped "+
			"matching — in which case every assertion below passes over an empty set — or "+
			"nothing wires the field, which makes %s an armed switch that does nothing and "+
			"/readyz refuse every provisioner deployment with a database.",
			len(files), envAgentPrivApply)
	}

	var names []string
	for f := range found {
		names = append(names, f)
	}
	sort.Strings(names)
	t.Logf("sites assigning api.Extensions.PrivilegeApply: %s", strings.Join(names, ", "))

	for _, f := range names {
		if _, ok := privilegeApplySiteLedger[f]; !ok {
			t.Errorf("%s assigns api.Extensions.PrivilegeApply and is NOT on the ledger:\n    %s\n"+
				"    Add it to privilegeApplySiteLedger in this file with what guards it — and "+
				"check it is inside a nil-check over the CONCRETE pointer. A typed nil here "+
				"silences the readiness defect rather than tripping a wrapper, so this is the "+
				"one tier whose missing guard fails OPEN.", f, strings.Join(found[f], "\n    "))
		}
	}
	for f := range privilegeApplySiteLedger {
		if _, ok := found[f]; !ok {
			t.Errorf("%s is on the PrivilegeApply ledger and no longer assigns the field.\n"+
				"    Either the wiring was removed — in which case %s is now inert and every "+
				"provisioner deployment with a database is permanently unready — or it moved, "+
				"and the ledger entry describing it is a reassurance a reader would trust.",
				f, envAgentPrivApply)
		}
	}
}

// ⚠ TestTheDefaultConfigLeavesPrivilegeApplyTrulyNil WAS HERE AND IS DELETED
// RATHER THAN KEPT WITH A LABEL, and the deletion is recorded because the next
// reader's instinct is to re-add it.
//
// 🔴 THE SWEEP THAT BUILT THIS TIER CREDITED IT WITH ZERO KILLS ACROSS 19 MUTANTS
// (reported by that round, not re-measured here), AND ITS OWN DOC EXPLAINED WHY
// WITHOUT NOTICING — which is the part that WAS re-checked, by reading main.go. It
// built `buildApp(config{})` and asserted the banner does
// not say "privilege APPLY: WIRED" — but BOTH banner arms sit inside the
// provisioner-wired block, which a zero-value config never enters, so the string
// it looked for cannot be emitted by any mutation of the privilege wiring. It was
// an unlabelled invariant guard over a code path the fixture does not reach, and it
// duplicated TestTheDefaultConfigLeavesTheProvisionerInterfaceTRULYNil's identical
// buildApp(config{}) call. Reading as coverage while providing none is worse than
// nothing, because it stops anyone looking.
//
// WHAT COVERS THE PROPERTY IT CLAIMED: TestThePrivilegeTierIsBuiltOnlyWhenArmed
// (both directions of the gate, behaviourally) and
// TestEverySitePopulatingPrivilegeApplyIsOnTheLedger (the assignment is inside a
// concrete-pointer nil-check).

// TestArmingThePrivilegeApplierWithNoProvisionerIsRefusedAtBoot.
//
// 🔴 IT IS THE ONE MISCONFIGURATION THAT WOULD OTHERWISE BE COMPLETELY SILENT.
// With no driver named, buildAgentPlane returns before the applier is built, so
// the variable is ON and does nothing — and unlike every other unwired tier this
// one produces NO readiness defect either, because a nil Provisioner makes the
// first conjunct false. The deployment boots clean, prints nothing about it,
// reports READY, and records every grant unapplied. An operator who set this
// variable is saying they expect grants to be applied.
func TestArmingThePrivilegeApplierWithNoProvisionerIsRefusedAtBoot(t *testing.T) {
	cfg := config{Database: "postgres://unused", AgentPrivilegeApply: true}
	// Control on the instrument: the SAME config without the flag must validate, so
	// a refusal below is attributable to this knob and not to the rest of the
	// config being incomplete.
	if err := cfg.validateProvisioner(); err == nil {
		t.Fatalf("%s=1 with no provisioner was ACCEPTED: the variable is armed and does nothing, "+
			"the pod reports ready, and every grant is recorded unapplied", envAgentPrivApply)
	} else {
		for _, want := range []string{envAgentPrivApply, envAgentProvisioner, provisionerK8s} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not name %q, so an operator cannot act on it: %v", want, err)
			}
		}
	}
	control := cfg
	control.AgentPrivilegeApply = false
	if err := control.validateProvisioner(); err != nil {
		t.Fatalf("control FAILED: the same config WITHOUT %s was also refused (%v), so the "+
			"refusal above is not attributable to this knob", envAgentPrivApply, err)
	}
}

// TestThePrivilegeTierIsBuiltOnlyWhenArmed is the two-way behavioural guard on the
// gate itself.
//
// 🔴 BOTH DIRECTIONS, BECAUSE EACH FAILS DIFFERENTLY AND BADLY. Built when it
// should not be: an image bump starts attempting privileged cluster writes for a
// deployment that never asked, and every grant 403s (see provisioner.go's
// prerequisite 1). NOT built when it should be: the readiness refusal is
// unescapable and the pod never serves.
func TestThePrivilegeTierIsBuiltOnlyWhenArmed(t *testing.T) {
	logger := log.New(&strings.Builder{}, "", 0)

	off := provisionerTestConfig(provisionerNoop)
	if _, _, priv, err := buildAgentPlane(off, stubStore{}, logger); err != nil {
		t.Fatalf("buildAgentPlane(noop, unarmed): %v", err)
	} else if priv != nil {
		t.Errorf("the privilege tier was built with %s unset (%#v). A deployment that did not "+
			"opt in must behave exactly as it did before this knob existed.", envAgentPrivApply, priv)
	}

	on := provisionerTestConfig(provisionerNoop)
	on.AgentPrivilegeApply = true
	_, _, priv, err := buildAgentPlane(on, stubStore{}, logger)
	if err != nil {
		t.Fatalf("buildAgentPlane(noop, armed): %v", err)
	}
	if priv == nil {
		t.Fatalf("%s=1 built no applier, so api.Extensions.PrivilegeApply stays nil and "+
			"/readyz refuses with no way out", envAgentPrivApply)
	}
	// 🔴 IT MUST BE THE SAME BACKEND THE LIFECYCLE TIER USES, not a second one. The
	// lifecycle tier creates the instance's ServiceAccount and this tier binds RBAC
	// to it; two clients built from one configuration agree today and diverge the
	// moment anything about a driver is per-instance, and the failure would be a
	// grant refused as "not managed by this driver" over a ServiceAccount muster
	// demonstrably created. Driver() is the only observable of that from here.
	if got := priv.Driver(); got != provisionerNoop {
		t.Errorf("the applier reports the %q driver while %s names %q", got,
			envAgentProvisioner, provisionerNoop)
	}
}

// probeDriver is a provision.Provisioner whose NAME is unique, so an object built
// over it can be told apart from one built over any driver this binary can
// construct for itself.
//
// ⚠ IT EMBEDS THE INTERFACE RATHER THAN IMPLEMENTING ELEVEN METHODS, and the
// embedded value is a REAL noop driver rather than nil — an embedded nil would
// panic on any method the code under test happens to call, which is a crash
// reported as a wiring defect.
type probeDriver struct{ provision.Provisioner }

func (probeDriver) Driver() string { return "probe-driver" }

// TestThePrivilegeApplierAppliesThroughTheDriverItWasGiven.
//
// 🔴 IT IS THE "ONE DRIVER INSTANCE" INVARIANT, AND IT SURVIVED A MUTATION SWEEP
// BEFORE THIS TEST EXISTED. Replacing `Driver: driver` in buildPrivilegeApplier
// with a freshly-constructed driver left the whole suite green, because every
// driver a test can build here is the noop one and Driver() reads "noop" either
// way — a fixture whose value equals the value a mutant produces cannot see the
// mutant. A probe driver is the control: it feeds a name the mutant CANNOT
// produce.
//
// WHY THE INVARIANT MATTERS, not restated from the doc but the short form: the
// lifecycle tier creates the instance's ServiceAccount and this tier binds cluster
// RBAC to it. Two clients built from one configuration agree today and diverge the
// moment anything about a driver is per-instance, and the failure would be a grant
// refused as "not managed by this driver" over a ServiceAccount muster
// demonstrably created — which reads as a bug in the ownership predicate.
func TestThePrivilegeApplierAppliesThroughTheDriverItWasGiven(t *testing.T) {
	cfg := provisionerTestConfig(provisionerNoop)
	cfg.AgentPrivilegeApply = true

	priv, err := buildPrivilegeApplier(cfg, probeDriver{provision.MustNewNoop()})
	if err != nil {
		t.Fatalf("buildPrivilegeApplier: %v", err)
	}
	if priv == nil {
		t.Fatal("buildPrivilegeApplier returned nil for an armed config")
	}
	if got := priv.Driver(); got != "probe-driver" {
		t.Errorf("the applier reports the %q driver, want %q — it did not apply through the "+
			"driver it was handed, so it is not the instance the lifecycle tier holds", got, "probe-driver")
	}

	// Control on the same instrument: unarmed still builds nothing, even when handed
	// a driver. Without it, a buildPrivilegeApplier that ignored the flag entirely
	// would satisfy the assertion above.
	cfg.AgentPrivilegeApply = false
	if unarmed, err := buildPrivilegeApplier(cfg, probeDriver{provision.MustNewNoop()}); err != nil {
		t.Fatalf("buildPrivilegeApplier(unarmed): %v", err)
	} else if unarmed != nil {
		t.Errorf("buildPrivilegeApplier built %#v with %s unset", unarmed, envAgentPrivApply)
	}
}

// TestLoadConfigReadsThePrivilegeApplyFlag.
//
// 🔴 IT EXISTS BECAUSE A MUTANT THAT MADE loadConfig NEVER READ THE VARIABLE
// SURVIVED EVERYTHING ELSE. Every other guard in this file constructs a config
// VALUE directly — which is what makes them runnable without an environment — so
// none of them touches the one line that connects the operator's variable to the
// field. TestEveryServerEnvNameIsSpelledOnceAndRead's "AND READ" direction does
// not close it either: the identifier stays referenced by validateProvisioner and
// the banner, so the constant is "read" while the value is not. The observable of
// the gap is total: setting the variable does nothing at all, and the readiness
// refusal keeps naming it.
//
// ⚠ IT ASSERTS BOTH DIRECTIONS AND A MISSPELLING. envFlag is deliberately strict
// — anything other than an explicit affirmative is false — so the false case is
// not the trivial half: a mutant that hardcoded `true` passes the first assertion.
func TestLoadConfigReadsThePrivilegeApplyFlag(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"1", true},
		{"true", true},
		{"on", true},
		{"", false},
		{"0", false},
		// A misspelling must NOT arm it: envFlag's own doc says a permissive parse
		// is how a typo turns into an armed surface, and this surface writes cluster
		// RBAC.
		{"yess", false},
	} {
		t.Run("value="+tc.value, func(t *testing.T) {
			cfg, err := loadConfig(func(name string) string {
				if name == envAgentPrivApply {
					return tc.value
				}
				return ""
			})
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if cfg.AgentPrivilegeApply != tc.want {
				t.Errorf("%s=%q gave AgentPrivilegeApply=%v, want %v — the variable an operator "+
					"sets and the field the wiring reads are not connected",
					envAgentPrivApply, tc.value, cfg.AgentPrivilegeApply, tc.want)
			}
		})
	}
}

// TestTheReadinessRefusalNamesTheVariableThatArmsTheApplier is a CROSS-PACKAGE
// spelling gate, and it exists because there is no other way to have one.
//
// 🔴 internal/api's REFUSAL TEXT CONTAINS "MUSTER_AGENT_PRIVILEGE_APPLY" AS A
// STRING LITERAL, WHICH IS A SECOND SPELLING OF A NAME THIS PACKAGE OWNS.
// internal/api cannot import cmd/muster-server, so it cannot reference the
// constant; and TestEveryServerEnvNameIsSpelledOnceAndRead scans only this
// package's own files, so it is structurally blind to that literal. Without this
// test, renaming the constant leaves a readiness refusal telling an operator to
// set a variable the binary no longer reads — the worst kind of stale prose,
// because it is the ONLY instruction a stuck operator gets.
//
// ⚠ IT READS A FILE OUTSIDE ITS OWN PACKAGE, which is unusual here and is the
// cheaper of the two options. The alternative was to assert /readyz's body from
// this package, which needs a privilege.Store stub (thirteen methods) built for
// one string comparison.
func TestTheReadinessRefusalNamesTheVariableThatArmsTheApplier(t *testing.T) {
	const rel = "../../internal/api/ext.go"
	body, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	// Instrument check: prove this is the right file before reading a verdict out
	// of it. A wrong path, or a defects() that moved, would otherwise report the
	// same failure as a renamed variable and send the reader to the wrong place.
	if !strings.Contains(string(body), "func (e Extensions) defects()") {
		t.Fatalf("instrument check FAILED: %s does not declare Extensions.defects, so this test "+
			"is reading the wrong file and its verdict is about nothing", rel)
	}
	if !strings.Contains(string(body), envAgentPrivApply) {
		t.Errorf("%s does not mention %q.\n"+
			"    api.Extensions.defects' privilege entry is the only instruction an operator "+
			"gets when /readyz refuses a wired provisioner over a wired privilege store. If it "+
			"names a variable this binary no longer reads — or names none — the refusal has no "+
			"escape, and the cheapest way out becomes deleting the readiness probe.",
			rel, envAgentPrivApply)
	}
}

// TestThePrivilegeStoreHasNoKnobOfItsOwn pins the ABSENCE three files' prose
// depended on without checking.
//
// 🔴 THE READINESS REFUSAL NAMED THREE ESCAPES AND ONLY TWO WERE REACHABLE. It
// said "set MUSTER_AGENT_PRIVILEGE_APPLY=1, or leave the privilege store unset, or
// leave the provisioner unwired", and config.go and the boot banner said the same.
// The middle one is not a configuration: main.go builds the privilege store
// UNCONDITIONALLY inside the `Database != ""` branch, so "leave the privilege
// store unset" means "run with no database" — which also drops notes, agents,
// runbooks and GitHub. An operator in the middle of a readiness refusal, reading
// three escapes and picking what looks like the cheapest, would have reached for
// one that costs the entire data layer.
//
// 🔴 SO THIS GUARD PINS THE RELATIONSHIP, IN BOTH DIRECTIONS, RATHER THAN THE
// SENTENCE. It derives from the CODE that no such knob exists, and then requires
// the refusal text to enumerate exactly the escapes that do. Add a real
// privilege-store variable later and this reddens — which is the correct outcome,
// because the refusal then owes a third entry. Keep the knob absent and reword the
// refusal into claiming a third escape and it reddens too.
func TestThePrivilegeStoreHasNoKnobOfItsOwn(t *testing.T) {
	// --- (a) the code side: one assignment, guarded only by the database ---
	const mainRel = "main.go"
	body, err := os.ReadFile(mainRel)
	if err != nil {
		t.Fatalf("read %s: %v", mainRel, err)
	}
	lines := strings.Split(string(body), "\n")
	var assignments []int
	for i, l := range lines {
		if strings.Contains(l, "ext.Privilege =") {
			assignments = append(assignments, i)
		}
	}
	// Positive control: the scan must HIT, or everything below is a verdict about
	// an empty set — the reassuring zero that is indistinguishable from a harness
	// wired to nothing.
	if len(assignments) == 0 {
		t.Fatalf("positive control FAILED: no assignment to ext.Privilege in %s. Either the "+
			"store moved — in which case this guard is measuring nothing — or the field is no "+
			"longer populated at all, which would make the readiness defect unreachable.", mainRel)
	}
	if len(assignments) != 1 {
		t.Errorf("ext.Privilege is assigned at %d sites in %s (lines %v). This guard's whole "+
			"claim is that ONE unconditional assignment inside the database branch is why "+
			"\"leave the privilege store unset\" is not an escape; with several, read each one "+
			"before trusting the refusal text.", len(assignments), mainRel, assignments)
	}
	t.Logf("ext.Privilege is assigned once, at %s:%d", mainRel, assignments[0]+1)

	// The nearest enclosing `if cfg.…` above the assignment is the condition that
	// decides whether the store exists at all.
	guard := ""
	for i := assignments[0]; i >= 0; i-- {
		if strings.Contains(lines[i], "if cfg.") {
			guard = strings.TrimSpace(lines[i])
			break
		}
	}
	const wantGuard = `if cfg.Database != "" {`
	if guard != wantGuard {
		t.Errorf("the nearest configuration condition above the ext.Privilege assignment is\n"+
			"    %s\nand this guard expects\n    %s\n"+
			"    If a knob now gates the privilege store, the readiness refusal in "+
			"internal/api/ext.go, cmd/muster-server/config.go's AgentPrivilegeApply doc and the "+
			"boot banner all owe a THIRD escape — they currently say there are two, on the "+
			"strength of this line.", guard, wantGuard)
	}

	// And no field of the configuration mentions the privilege store. reflect
	// rather than a source scan: a field is what the wiring can read.
	rt := reflect.TypeOf(config{})
	var privilegeFields []string
	for i := 0; i < rt.NumField(); i++ {
		if strings.Contains(rt.Field(i).Name, "Privilege") {
			privilegeFields = append(privilegeFields, rt.Field(i).Name)
		}
	}
	sort.Strings(privilegeFields)
	if len(privilegeFields) != 1 || privilegeFields[0] != "AgentPrivilegeApply" {
		t.Errorf("the configuration carries privilege-related field(s) %v; this guard expects "+
			"exactly [AgentPrivilegeApply].\n    A second one is probably the store knob whose "+
			"absence the refusal text argues from.", privilegeFields)
	}

	// --- (b) the prose side: the refusal enumerates exactly those escapes ---
	const extRel = "../../internal/api/ext.go"
	ext, err := os.ReadFile(extRel)
	if err != nil {
		t.Fatalf("read %s: %v", extRel, err)
	}
	const marker = "Provisioner is wired but PrivilegeApply is not"
	start := strings.Index(string(ext), marker)
	if start < 0 {
		t.Fatalf("instrument check FAILED: %s does not contain the privilege defect's opening "+
			"words (%q), so this test is reading the wrong text and its verdict is about nothing",
			extRel, marker)
	}
	entry := string(ext)[start:]
	if end := strings.Index(entry, "return out"); end > 0 {
		entry = entry[:end]
	}

	// Both reachable escapes must be NAMED, because a refusal an operator cannot
	// act on is the shape that gets the readiness probe deleted.
	for _, want := range []string{envAgentPrivApply, envAgentProvisioner} {
		if !strings.Contains(entry, want) {
			t.Errorf("the privilege readiness refusal does not name %q, which is one of the two "+
				"variables that can escape it", want)
		}
	}
	// The COUNT is pinned structurally, by the enumeration's own markers, rather
	// than by a phrase — a phrase is walkable by rewording, and this refusal is
	// text an operator reads under pressure.
	for _, want := range []string{"(1)", "(2)"} {
		if !strings.Contains(entry, want) {
			t.Errorf("the privilege readiness refusal has no %q item. The escapes are enumerated "+
				"so their NUMBER is checkable against the configuration; without the markers this "+
				"guard cannot tell two escapes from three.", want)
		}
	}
	if strings.Contains(entry, "(3)") {
		t.Errorf("the privilege readiness refusal enumerates a THIRD escape while the " +
			"configuration has two (checked above: no privilege-store knob). That is the exact " +
			"defect this guard was written for — the third entry used to be \"leave the " +
			"privilege store unset\", which is not a setting but a deployment with no database.")
	}
	// And the cost of the unreachable one has to be stated, or a reader
	// reconstructs it as a cheap option.
	if !strings.Contains(entry, "no database") {
		t.Errorf("the privilege readiness refusal does not say that leaving the privilege store " +
			"unset means running with NO DATABASE. Without that, the option reads as a cheap " +
			"third escape rather than as the loss of notes, agents, runbooks and GitHub.")
	}
}

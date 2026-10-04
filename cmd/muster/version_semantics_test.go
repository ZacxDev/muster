package main

import (
	"net/http"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// THE TWO VERSION NAMESPACES, AND WHICH SURFACE READS WHICH.
//
// 🔴 WHAT THESE GUARD, STATED AS THE DEFECT THEY CLOSE. `buildVersion` is the
// muster SERVER version this CLI was built against — a semver, pinned to
// `api.BuildVersion`'s DEFAULT by server_pins_test.go. The nix build briefly
// stamped a GIT REVISION into that variable, and TWO operator surfaces printed
// it raw: `warnSkew` ("server 0.2.2, muster built for X") and the route-absent
// 404 ("this client was built against X"). Both then rendered a comparison that
// cannot be made — "0.2.2" is not older, newer or equal to "ba6698e" — on
// precisely the surface where an operator is reasoning about compatibility, and
// an unlabelled revision reads as a real version where the old "dev" default
// read as the placeholder it is. `buildRevision` now carries provenance.
//
// 🔴 THE FIX IS LABELLING, NOT OMITTING, AND IT IS LABELLING ON ALL THREE
// SURFACES. `cliVersion()` is the one place the two values meet, and all three
// readers render it: cobra's `--version`, the route-absent 404 and the skew
// note. Omitting the revision on the operator surfaces was tried and is worse —
// it renders a bare "dev", from which an operator can conclude nothing, where a
// revision can be `git log`ged to establish staleness. So what these tests
// refuse is no longer "a revision on this surface" but "a revision WITHOUT its
// label", plus any change to the rendered sentences at all.
//
// 🔴 THE COMPARISON IS NOT THE RENDER. `warnSkew` still compares `buildVersion`
// alone; comparing the composed string would make every nix-built client differ
// from every server forever. That operand is pinned by
// TestTheSkewComparisonReadsTheServerPinNotTheComposedVersion, and only by it.
//
// 🔴 THE EXPECTATIONS BELOW ARE LITERAL STRINGS, NOT FORMATS BUILT FROM THE
// CODE. Composing an expectation out of `buildVersion` would follow the
// implementation wherever it went; a literal is what makes a swapped variable
// or a reworded sentence visible. The literals assume `buildVersion`'s default
// is "dev" — which server_pins_test.go holds to `api.BuildVersion` — so each
// test states that precondition rather than letting a server bump surface as a
// confusing string diff.
//
// 🔴 THE WHOLE NORMALISED STREAM IS PINNED, NOT A SUBSTRING. A guard on a word
// ("built against") is walkable by rewording, which is the failure mode for a
// test whose subject IS prose. The cost is that a deliberate reword fails these
// tests; that cost is the point — it is what makes the claim machine-readable.
// ---------------------------------------------------------------------------

// revStamp is a rev-shaped value standing in for what `flake.nix` links into
// `buildRevision`. It is deliberately NOT a semver and deliberately not equal
// to `buildVersion`'s default, so a reader that picked the wrong variable
// renders something these assertions cannot miss.
const revStamp = "ba6698e"

// requireDefaultBuildVersion states the precondition the literals below depend
// on, so a server version bump reports itself rather than appearing as an
// unexplained diff in a rendered sentence.
func requireDefaultBuildVersion(t *testing.T) {
	t.Helper()
	if buildVersion != "dev" {
		t.Fatalf("this test's expected strings are written against buildVersion == %q, "+
			"but it is %q. The default moved (server_pins_test.go holds it to api.BuildVersion); "+
			"update the literals in this file to match.", "dev", buildVersion)
	}
}

// withRevision stamps buildRevision for one test, the way the nix build's
// `-X main.buildRevision=…` does at link time.
func withRevision(t *testing.T, rev string) {
	t.Helper()
	prev := buildRevision
	buildRevision = rev
	t.Cleanup(func() { buildRevision = prev })
}

// TestCLIVersionComposesBothHalvesAndLabelsThem pins what `--version` reports.
//
// 🔴 IT IS THE GUARD THAT KEEPS #28's GAIN WHILE THE SPLIT REMOVES ITS
// REGRESSION. The point of stamping at all is that the artefact can answer
// "which muster is installed here?" without `readlink -f` on a store path — so
// the revision MUST appear here. The point of the split is that it must appear
// LABELLED and must not displace the server version.
func TestCLIVersionComposesBothHalvesAndLabelsThem(t *testing.T) {
	cases := []struct {
		name    string
		version string
		rev     string
		want    string
	}{
		{
			// A plain `go build` — `make build`, `make verb-ledger`, and every CI
			// job on a runner with no nix. Nothing is stamped.
			name:    "unstamped: the server version alone",
			version: "dev", rev: "", want: "dev",
		},
		{
			// The nix build. This is the case #28 exists for.
			name:    "revision stamped: both halves, labelled",
			version: "dev", rev: "ba6698e", want: "dev (rev ba6698e)",
		},
		{
			// A release that stamps the server pin too. Neither half displaces
			// the other.
			name:    "both stamped",
			version: "0.2.2", rev: "ba6698e", want: "0.2.2 (rev ba6698e)",
		},
		{
			// `nix build path:.` — no git metadata, so flake.nix's binding is the
			// literal "unknown". Supported on purpose; see cli-version-stamp.sh.
			name:    "no git metadata",
			version: "dev", rev: "unknown", want: "dev (rev unknown)",
		},
		{
			// A build that blanks the server pin WHILE stamping a revision still
			// yields a non-empty string, so cobra keeps the --version flag. ⚠ The
			// conjunction is the whole claim: blanking it with NO revision returns
			// "" and cobra drops the flag — measured, and documented on
			// cliVersion() rather than asserted here, because this table cannot
			// reach that assertion (see the note at the equality check below).
			name:    "blank server pin, revision stamped: still a flag",
			version: "", rev: "ba6698e", want: " (rev ba6698e)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prevV, prevR := buildVersion, buildRevision
			buildVersion, buildRevision = tc.version, tc.rev
			t.Cleanup(func() { buildVersion, buildRevision = prevV, prevR })

			// ⚠ THERE IS DELIBERATELY NO `got == ""` ASSERTION HERE, AND THE
			// REASON IS STRUCTURAL RATHER THAN A JUDGEMENT CALL. One used to
			// follow this check, guarding "cobra drops --version when Version
			// is empty". It was UNREACHABLE: reaching it requires got == ""
			// AND got == tc.want, i.e. a row with want == "" — and such a row
			// makes that very assertion fire, so the only table that executes
			// it is a table in which it fails. Measured at 8ad413e: mutating
			// cliVersion() to `return ""` failed all five rows at the equality
			// above and reached it zero times.
			//
			// The hazard is real but is created by `ldflags`, which no test in
			// this package links. tests/cli-version-stamp.sh's marker control,
			// run in the nix derivation's installCheckPhase against the linked
			// artefact, is what refuses a binary with no --version flag.
			got := cliVersion()
			if got != tc.want {
				t.Fatalf("cliVersion() = %q, want %q (buildVersion=%q buildRevision=%q)",
					got, tc.want, tc.version, tc.rev)
			}
		})
	}
}

// TestTheVersionFlagReportsTheComposedStringThroughTheRealTree closes the gap
// the table test above cannot reach.
//
// 🔴 CALLING cliVersion() PROVES THE FUNCTION, NOT THE WIRING. A mutation that
// points cobra's `Version` field back at `buildVersion` leaves every assertion
// on cliVersion() green while `--version` loses the revision entirely — which
// is #28's defect restored, and only an assertion that goes THROUGH the command
// tree can see it. It is still not a check on the nix build's `ldflags`; that
// is tests/cli-version-stamp.sh, against the installed artefact.
func TestTheVersionFlagReportsTheComposedStringThroughTheRealTree(t *testing.T) {
	requireDefaultBuildVersion(t)
	withRevision(t, revStamp)

	h := newHarness(t)
	got := h.runCLI("--version")

	if got.code != exitOK {
		t.Fatalf("exit = %d, want 0.\nstderr=%q", got.code, got.stderr)
	}
	const want = "harness-invoked-name version dev (rev ba6698e)\n"
	if got.stdout != want {
		t.Fatalf("`--version` output changed.\n got = %q\nwant = %q\n"+
			"Both halves must be present and labelled: the revision is what makes the artefact "+
			"name its own provenance, and the label is what stops it reading as a server version.",
			got.stdout, want)
	}
}

// TestTheRouteAbsentMessageNamesTheServerVersionAndTheLabelledRevision is the
// guard for the consumer that was NOT disclosed when the stamp landed. It pins
// the whole rendered sentence: the server pin, the labelled revision, and the
// prose around them.
//
// 🔴 THE ROUTER'S text/plain 404 IS THE PATH. A handler's 404 is JSON and means
// a missing RESOURCE; the router's own is text/plain and means a missing ROUTE,
// which is the only case that renders this sentence. The harness answers an
// unregistered path with exactly what net/http.ServeMux writes, so this drives
// the real discriminator rather than a fixture shaped to reach the branch.
func TestTheRouteAbsentMessageNamesTheServerVersionAndTheLabelledRevision(t *testing.T) {
	requireDefaultBuildVersion(t)
	withRevision(t, revStamp)

	h := newHarness(t) // h.version defaults to buildVersion, so no skew note
	got := h.runCLI("task", "get", "999")

	if got.code != exitRouteAbsent {
		t.Fatalf("exit = %d, want %d (the router's text/plain 404 is the route-absent path).\nstderr=%q",
			got.code, exitRouteAbsent, got.stderr)
	}
	// 🔴 THE UNLABELLED-REVISION CHECK RUNS BEFORE THE WHOLE-STRING CHECK, AND
	// THE ORDER IS THE DIFFERENCE BETWEEN A LIVE GUARD AND DEAD PROSE. Measured
	// during the previous round of this change: with the string equality first,
	// every mutation that mis-rendered the revision here ALSO changed the
	// sentence, so the equality's Fatalf always fired and the assertion below
	// was never reached — it read as coverage while providing none. Checked
	// first it is reachable, and its message is the one a reader needs.
	//
	// 🔴 WHAT IT REFUSES IS THE REVISION WITHOUT ITS LABEL, NOT THE REVISION.
	// This assertion used to forbid the revision outright; that was the right
	// guard while these surfaces printed `buildVersion` alone, and it is the
	// wrong one now they print `cliVersion()`. A bare "dev" told an operator
	// nothing, so the labelled composition is deliberate. The hazard that
	// SURVIVES that decision is the revision arriving where a server version is
	// expected with nothing marking it — `buildRevision` rendered raw, or the
	// two halves concatenated without "(rev …)". So: remove the one legitimate
	// occurrence, then refuse any that is left.
	labelled := "(rev " + revStamp + ")"
	if strings.Contains(strings.Replace(got.stderr, labelled, "", 1), revStamp) {
		t.Fatalf("the route-absent message renders the build REVISION %q OUTSIDE its %q label. "+
			"Unlabelled, it sits next to the server's own version in the reader's head, and a "+
			"revision cannot be compared to a semver — that is the whole reason buildRevision is "+
			"a separate variable.\nstderr = %q", revStamp, labelled, got.stderr)
	}
	const want = "harness-invoked-name: route GET /api/tasks/999 not found on this server; " +
		"it may predate the route — this client was built against dev (rev ba6698e)\n"
	if got.stderr != want {
		t.Fatalf("the route-absent message changed.\n got = %q\nwant = %q\n"+
			"Both halves must be present and labelled: the server pin is what a compatibility "+
			"comparison is made against, and the revision is what an operator can `git log`.",
			got.stderr, want)
	}
	if got.stdout != "" {
		t.Fatalf("stdout = %q, want empty: a diagnostic must never reach the JSON stream", got.stdout)
	}
}

// TestTheSkewNoteNamesTheServerVersionAndTheLabelledRevision is the same guard
// for the consumer that WAS disclosed. Both readers are pinned because the
// defect was one variable with two readers, and a guard on one of them would
// have reported the relationship as covered while leaving half of it unmeasured.
//
// ⚠ IT PINS WHAT IS RENDERED, NOT WHAT IS COMPARED. The operand lives in
// TestTheSkewComparisonReadsTheServerPinNotTheComposedVersion; this test cannot
// see it, because the note it asserts fires either way.
func TestTheSkewNoteNamesTheServerVersionAndTheLabelledRevision(t *testing.T) {
	requireDefaultBuildVersion(t)
	withRevision(t, revStamp)

	h := newHarness(t)
	h.version = "0.2.2" // a real server semver, differing from the client's pin
	h.json("GET /api/tasks", http.StatusOK, `[]`)

	got := h.runCLI("task", "ls")

	if got.code != exitOK {
		t.Fatalf("exit = %d, want 0.\nstderr=%q", got.code, got.stderr)
	}
	// Unlabelled-revision check first, for the reason stated in the test above:
	// behind the string equality it is unreachable. Same shape, same hazard —
	// a revision beside the server's semver with nothing marking it as one.
	labelled := "(rev " + revStamp + ")"
	if strings.Contains(strings.Replace(got.stderr, labelled, "", 1), revStamp) {
		t.Fatalf("the skew note renders the build REVISION %q OUTSIDE its %q label, beside the "+
			"server's semver %q — a comparison that cannot be made, which is what makes the note "+
			"noise.\nstderr = %q", revStamp, labelled, "0.2.2", got.stderr)
	}
	const want = "note: server 0.2.2, harness-invoked-name built for dev (rev ba6698e)\n"
	if got.stderr != want {
		t.Fatalf("the skew note changed.\n got = %q\nwant = %q\n"+
			"The note names the SERVER pin the comparison was made against, labelled with the "+
			"revision an operator can `git log` — not one in place of the other.",
			got.stderr, want)
	}
}

// TestTheSkewComparisonReadsTheServerPinNotTheComposedVersion pins warnSkew's
// COMPARISON OPERAND, which is the one thing no other test in this file can
// see.
//
// 🔴 THE OPERAND AND THE RENDER ARE DIFFERENT VALUES, AND ONLY SILENCE PROVES
// WHICH ONE IS COMPARED. warnSkew compares `buildVersion` against /health and
// RENDERS `cliVersion()`. A mutation that compares `cliVersion()` instead would
// make a nix-built client — which always carries a revision — differ from EVERY
// server forever, the "fires on every command on every host" failure the
// comment on `buildVersion` names. Here the server agrees with the client's
// server pin while a revision IS stamped, so the correct code is silent and the
// mutant is not.
//
// ⚠ IT IS NOT A NEGATIVE CONTROL FOR THE TEST ABOVE, AND USED TO CLAIM TO BE.
// That claim was false: the test above pins the note's WHOLE string, so it
// already fails if the note stops firing. Nothing here is needed for that.
func TestTheSkewComparisonReadsTheServerPinNotTheComposedVersion(t *testing.T) {
	requireDefaultBuildVersion(t)
	withRevision(t, revStamp)

	h := newHarness(t)
	h.version = buildVersion // the server agrees with the client's SERVER pin
	h.json("GET /api/tasks", http.StatusOK, `[]`)

	got := h.runCLI("task", "ls")
	if got.code != exitOK {
		t.Fatalf("exit = %d, want 0.\nstderr=%q", got.code, got.stderr)
	}
	if got.stderr != "" {
		t.Fatalf("stderr = %q, want silence: the server's version matches this client's SERVER "+
			"pin, so there is no skew to report. A stamped revision must not enter that comparison.",
			got.stderr)
	}
}

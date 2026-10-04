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
// `api.BuildVersion` by server_pins_test.go — and TWO readers print it beside
// the server's own value: `warnSkew` ("server 0.2.2, muster built for X") and
// the route-absent 404 ("this client was built against X"). The nix build
// briefly stamped a GIT REVISION into that variable, so both rendered a
// comparison that cannot be made — "0.2.2" is not older, newer or equal to
// "ba6698e" — on precisely the surface where an operator is reasoning about
// compatibility, and a revision reads as a real version where the old "dev"
// default read as the placeholder it is. `buildRevision` now carries provenance
// and `cliVersion()` is the one place the two meet, labelled.
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
			// 🔴 A BUILD THAT BLANKS THE SERVER PIN MUST STILL LEAVE A NON-EMPTY
			// STRING. cobra DISABLES the --version flag entirely when Version is
			// "", which would delete the flag this project's install check reads —
			// a failure that looks like "the binary has no --version" rather than
			// "someone passed an empty -X".
			name:    "blank server pin still yields a flag",
			version: "", rev: "ba6698e", want: " (rev ba6698e)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prevV, prevR := buildVersion, buildRevision
			buildVersion, buildRevision = tc.version, tc.rev
			t.Cleanup(func() { buildVersion, buildRevision = prevV, prevR })

			got := cliVersion()
			if got != tc.want {
				t.Fatalf("cliVersion() = %q, want %q (buildVersion=%q buildRevision=%q)",
					got, tc.want, tc.version, tc.rev)
			}
			if got == "" {
				t.Fatal("cliVersion() returned the empty string, which makes cobra drop --version entirely")
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

// TestTheRouteAbsentMessageNamesTheServerVersionNotTheRevision is the guard for
// the consumer that was NOT disclosed when the stamp landed.
//
// 🔴 THE ROUTER'S text/plain 404 IS THE PATH. A handler's 404 is JSON and means
// a missing RESOURCE; the router's own is text/plain and means a missing ROUTE,
// which is the only case that renders this sentence. The harness answers an
// unregistered path with exactly what net/http.ServeMux writes, so this drives
// the real discriminator rather than a fixture shaped to reach the branch.
func TestTheRouteAbsentMessageNamesTheServerVersionNotTheRevision(t *testing.T) {
	requireDefaultBuildVersion(t)
	withRevision(t, revStamp)

	h := newHarness(t) // h.version defaults to buildVersion, so no skew note
	got := h.runCLI("task", "get", "999")

	if got.code != exitRouteAbsent {
		t.Fatalf("exit = %d, want %d (the router's text/plain 404 is the route-absent path).\nstderr=%q",
			got.code, exitRouteAbsent, got.stderr)
	}
	// 🔴 THE REVISION CHECK RUNS BEFORE THE WHOLE-STRING CHECK, AND THE ORDER IS
	// THE DIFFERENCE BETWEEN A LIVE GUARD AND DEAD PROSE. Measured during this
	// change: with the string equality first, every mutation that leaked a
	// revision into this sentence ALSO changed the sentence, so the equality's
	// Fatalf always fired and the assertion below was never reached — it read as
	// coverage while providing none. Checked first, it is reachable and it is
	// the message a reader needs; the equality below then catches the OTHER
	// failure, a reword that leaks nothing.
	if strings.Contains(got.stderr, revStamp) {
		t.Fatalf("the route-absent message renders the build REVISION %q. It sits next to the "+
			"server's own version in the reader's head, and a revision cannot be compared to a "+
			"semver — that is the whole reason buildRevision is a separate variable.\nstderr = %q",
			revStamp, got.stderr)
	}
	const want = "harness-invoked-name: route GET /api/tasks/999 not found on this server; " +
		"it may predate the route (this client was built against dev)\n"
	if got.stderr != want {
		t.Fatalf("the route-absent message changed.\n got = %q\nwant = %q", got.stderr, want)
	}
	if got.stdout != "" {
		t.Fatalf("stdout = %q, want empty: a diagnostic must never reach the JSON stream", got.stdout)
	}
}

// TestTheSkewNoteNamesTheServerVersionNotTheRevision is the same guard for the
// consumer that WAS disclosed. Both readers are pinned because the defect was
// one variable with two readers, and a guard on one of them would have reported
// the relationship as covered while leaving half of it unmeasured.
func TestTheSkewNoteNamesTheServerVersionNotTheRevision(t *testing.T) {
	requireDefaultBuildVersion(t)
	withRevision(t, revStamp)

	h := newHarness(t)
	h.version = "0.2.2" // a real server semver, differing from the client's pin
	h.json("GET /api/tasks", http.StatusOK, `[]`)

	got := h.runCLI("task", "ls")

	if got.code != exitOK {
		t.Fatalf("exit = %d, want 0.\nstderr=%q", got.code, got.stderr)
	}
	// Revision check first, for the reason stated in the test above: behind the
	// string equality it is unreachable.
	if strings.Contains(got.stderr, revStamp) {
		t.Fatalf("the skew note renders the build REVISION %q beside the server's semver %q — "+
			"a comparison that cannot be made, which is what makes the note noise.\nstderr = %q",
			revStamp, "0.2.2", got.stderr)
	}
	const want = "note: server 0.2.2, harness-invoked-name built for dev\n"
	if got.stderr != want {
		t.Fatalf("the skew note changed.\n got = %q\nwant = %q", got.stderr, want)
	}
}

// TestAStampedRevisionDoesNotSuppressTheSkewNote is the negative control for
// the test above: without it, "the note names dev" would also pass for a build
// in which the note stopped firing altogether.
//
// 🔴 IT ALSO PINS THE COMPARISON'S OPERAND. warnSkew compares `buildVersion`
// against /health; comparing `cliVersion()` instead would make a nix-built
// client differ from EVERY server forever, which is the "fires on every command
// on every host" failure server_pins_test.go's comment names.
func TestAStampedRevisionDoesNotSuppressTheSkewNote(t *testing.T) {
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

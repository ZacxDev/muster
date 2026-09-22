package provisiontest

import (
	"os"
	"strings"
	"testing"
)

// TestTheContractSuiteMatchesWhatTheDocsClaimAboutIt pins the two numbers the
// package docs and the README state ABOUT this suite.
//
// 🔴 A COUNT IN PROSE IS UNFALSIFIABLE UNLESS SOMETHING READS IT. The provision
// package doc and the README both say the four contract rules are pinned here
// "with one named exception" — rule 2's ErrNotManaged carve-out — and both cite
// "21 cases, none of which mentions ErrNotManaged". Those sentences were written
// after counting by hand, and a hand count is a claim about the moment it was
// made: adding a case, or pinning the carve-out here after all, silently turns
// both documents into confident false statements about their own test suite.
//
// This is what makes the numbers a measurement. If it fails, do not adjust the
// number here alone — the sentences in internal/provision/doc.go and README.md
// are part of the same claim and have to move with it.
func TestTheContractSuiteMatchesWhatTheDocsClaimAboutIt(t *testing.T) {
	// The case list lives inside RunContract, so count it from the source
	// rather than from a second copy of the list that could agree with a wrong
	// docstring.
	src, err := os.ReadFile("contract.go")
	if err != nil {
		t.Fatalf("read contract.go: %v", err)
	}
	text := string(src)

	// 🔴 POSITIVE CONTROL. A counter that matches nothing reports zero, which
	// is indistinguishable from a suite with no cases — and every assertion
	// below would then be about the parser rather than the suite.
	const marker = "\", test"
	n := strings.Count(text, marker)
	t.Logf("contract.go declares %d case(s) (counted on %q)", n, marker)
	if n == 0 {
		t.Fatalf("the case counter matched NOTHING in contract.go, so its number says nothing about "+
			"the suite: the %q shape must have changed", marker)
	}

	const wantCases = 21
	if n != wantCases {
		t.Errorf("RunContract declares %d cases; internal/provision/doc.go and README.md both say %d. "+
			"Update the code and BOTH documents together, or the docs become a false statement about "+
			"their own suite.", n, wantCases)
	}

	// The carve-out claim: this suite deliberately does not pin ErrNotManaged,
	// because a foreign co-named object needs a SHARED backend and Noop's
	// backend is its own map. If that changes, the "one named exception"
	// sentences are wrong in the other direction — they would understate the
	// coverage, which is the safer error but still an error.
	if strings.Contains(text, "ErrNotManaged") {
		t.Errorf("contract.go now mentions ErrNotManaged. That may be an improvement, but " +
			"internal/provision/doc.go, provision.go's ErrNotManaged doc and README.md all state that " +
			"this suite does NOT pin it and that it is pinned in the kubernetes driver instead. Fix " +
			"the three documents in the same change.")
	}
}

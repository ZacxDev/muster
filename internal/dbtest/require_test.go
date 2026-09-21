package dbtest

import "testing"

// ---------------------------------------------------------------------------
// The gate: MUSTER_TEST_REQUIRE_DB turns "no database" from a SKIP into a
// FAILURE.
//
// 🔴 WHY THIS EXISTS AT ALL. Without it a fresh clone runs `go test ./...`,
// every Postgres-backed test skips, and the suite is GREEN having executed none
// of the store logic. That green is indistinguishable — in the output, in CI, in
// a reviewer's head — from a green that ran everything. A skip is invisible in a
// verdict, so the only way to make the requirement real is to make its absence
// a failure, and the only way to make THAT real is to set it in CI.
//
// 🔴 WHAT THESE TESTS CAN AND CANNOT SEE. requireDB takes its getenv as a
// PARAMETER, so its truth table is testable without mutating the process
// environment — which matters because `go test` runs tests in one process and a
// t.Setenv in a parallel test is a cross-test write. Base() itself calls
// os.Getenv and ends the test (Fatalf/Skipf), so a test cannot call it and live
// to assert on the outcome; its behaviour is exercised end to end from the
// CONTRIBUTING recipe and from CI, and the three-way control is documented
// there rather than faked here. What is pinned below is the DECISION function,
// which is the part a future edit is likely to get wrong.
// ---------------------------------------------------------------------------

func TestRequireDBTruthTable(t *testing.T) {
	cases := []struct {
		value string
		want  bool
		why   string
	}{
		{"", false, "unset is the developer default: skip and say so"},
		{"0", false, "an explicit off"},
		{"false", false, "an explicit off"},
		{"FALSE", false, "case is folded, so a shouted off is still off"},
		{"no", false, "an explicit off"},
		{"off", false, "an explicit off"},
		{"  0  ", false, "surrounding whitespace is trimmed; a value pasted with a trailing " +
			"space must not silently arm the gate"},
		{"1", true, "the spelling CI uses"},
		{"true", true, "the other obvious spelling"},
		{"yes", true, "a spelling somebody will reach for"},
		{"on", true, "a spelling somebody will reach for"},
		// 🔴 THE FAILURE DIRECTION, PINNED. An unrecognised value means somebody
		// was TRYING to turn the gate on. Reading it as "off" would disable the
		// control in exactly that case, silently — which is the worse of the two
		// mistakes, because the run then goes green.
		{"enabled", true, "an unrecognised value arms the gate: the person who set it wanted it on"},
		{"please", true, "same"},
	}
	for _, c := range cases {
		got := requireDB(func(k string) string {
			if k != RequireEnvVar {
				t.Fatalf("requireDB read %q; it must read only %s", k, RequireEnvVar)
			}
			return c.value
		})
		if got != c.want {
			t.Errorf("requireDB(%q) = %v, want %v — %s", c.value, got, c.want, c.why)
		}
	}
}

// TestRequireDBReadsOnlyItsOwnVariable pins the relationship between the two
// environment variables: the REQUIRE flag must not be inferred from the DSN.
//
// The hazard is the natural-looking shortcut "if the DSN is set, require it" —
// which inverts the whole design. The flag exists to make an ABSENT DSN fatal;
// deriving it from the DSN's presence makes it vacuous.
func TestRequireDBReadsOnlyItsOwnVariable(t *testing.T) {
	read := map[string]int{}
	getenv := func(k string) string {
		read[k]++
		if k == EnvVar {
			return "postgres://u:p@127.0.0.1:5432/muster_test"
		}
		return ""
	}
	if requireDB(getenv) {
		t.Fatal("requireDB returned true with the REQUIRE flag unset. If it is reading the DSN " +
			"instead, the gate is vacuous: it would only ever demand a database in runs that " +
			"already have one.")
	}
	if read[EnvVar] != 0 {
		t.Errorf("requireDB read %s %d time(s); it must read only %s", EnvVar, read[EnvVar], RequireEnvVar)
	}
	if read[RequireEnvVar] == 0 {
		t.Errorf("requireDB never read %s — it cannot be deciding anything", RequireEnvVar)
	}
}

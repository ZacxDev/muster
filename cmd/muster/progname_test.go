package main

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestProgNameUnitCases pins the pure function.
func TestProgNameUnitCases(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/usr/local/bin/muster", "muster"},
		{"muster", "muster"},
		{"./muster", "muster"},
		{"/opt/x/legacy-alias-probe", "legacy-alias-probe"},
		{"legacy-alias-probe", "legacy-alias-probe"},
		{"muster.exe", "muster"},
		{"  /usr/bin/other-name  ", "other-name"},
		// Degenerate argv[0]s. None of these is a name, so each falls back rather
		// than printing "." or "/" as the command name in an error message.
		{"", defaultProgName},
		{".", defaultProgName},
		{"..", defaultProgName},
		{"/", defaultProgName},
		// 🔴 MEASURED, AND IT IS THE CASE THE ORDER OF OPERATIONS EXISTS FOR:
		// filepath.Base("/usr/bin/") returns "bin", so a check made AFTER Base
		// cannot tell this from an argv[0] of "/usr/bin" naming a binary called
		// `bin`. progName tests the trailing separator on the raw string first.
		{"/usr/bin/", defaultProgName},
		{"relative/dir/", defaultProgName},
		// The control for the line above: the SAME path without the trailing
		// separator IS a name, and must not be swallowed by the rule.
		{"/usr/bin", "bin"},
		// A leading dash would render as a flag everywhere the name is printed.
		{"-notaname", defaultProgName},
	} {
		t.Run(tc.in, func(t *testing.T) {
			if got := progName(tc.in); got != tc.want {
				t.Fatalf("progName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestTheInvokedNameReachesEveryPlaceThatPrintsIt is the in-process half of the
// alias guard.
//
// 🔴 IT DRIVES A NAME THIS SOURCE TREE DOES NOT CONTAIN, and that is the point
// of the mechanism being name-agnostic: an argv[0] equality check against a
// literal would pass a test that used the same literal and tell you nothing
// about any other alias. The three places the name surfaces are the three the
// host-side enforcement hooks and a human actually read.
func TestTheInvokedNameReachesEveryPlaceThatPrintsIt(t *testing.T) {
	const alias = "legacy-alias-probe"

	t.Run("the error prefix", func(t *testing.T) {
		h := newHarness(t)
		h.json("GET /api/tasks/9", http.StatusNotFound, `{"error":"task not found"}`)
		got := h.runCLIAs("/opt/bin/"+alias, nil, strings.NewReader(""), "task", "get", "9")
		if got.code != exitNotFound {
			t.Fatalf("exit = %d, want %d (stderr=%q)", got.code, exitNotFound, got.stderr)
		}
		if !strings.HasPrefix(strings.TrimSpace(got.stderr), alias+": ") {
			t.Fatalf("stderr = %q, want it to begin %q — a caller reads back the command it typed",
				got.stderr, alias+": ")
		}
		// NEGATIVE CONTROL: the default name must NOT appear, or the assertion
		// above could be satisfied by a prefix that merely contains the alias.
		if strings.Contains(got.stderr, defaultProgName+": ") {
			t.Fatalf("stderr = %q still carries the default name's prefix", got.stderr)
		}
	})

	t.Run("the usage line in --help", func(t *testing.T) {
		h := newHarness(t)
		got := h.runCLIAs("/opt/bin/"+alias, nil, strings.NewReader(""), "--help")
		if got.code != exitOK {
			t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
		}
		if !strings.Contains(got.stdout, alias+" [command]") {
			t.Fatalf("--help usage does not name the invoked binary %q:\n%s", alias, got.stdout)
		}
	})

	t.Run("the version-skew note", func(t *testing.T) {
		h := newHarness(t)
		h.version = "0.0.0-newer-than-this-cli"
		h.json("GET /api/agents", http.StatusOK, `[]`)
		got := h.runCLIAs("/opt/bin/"+alias, nil, strings.NewReader(""), "agent", "ls")
		if got.code != exitOK {
			t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
		}
		want := "note: server 0.0.0-newer-than-this-cli, " + alias + " built for " + buildVersion
		if !strings.Contains(got.stderr, want) {
			t.Fatalf("stderr = %q, want it to contain %q", got.stderr, want)
		}
	})
}

// TestTheBinaryAnswersToAnAliasedSymlink is the OUT-OF-PROCESS half, and it is
// the one that actually proves the transitional alias works.
//
// 🔴 THE IN-PROCESS TESTS ABOVE CALL run() WITH A STRING. That proves argv[0]
// is threaded correctly through this package; it does NOT prove that a real
// kernel exec through a symlink produces that argv[0], which is the entire
// operational claim ("symlink the binary under the old name and its guards stay
// armed"). So this one builds the binary, symlinks it, and execs the symlink.
//
// It SKIPS when the toolchain is unavailable or symlinks are not supported,
// because there is genuinely nothing to assert there — and a skip is honest
// where a vacuous pass is not.
func TestTheBinaryAnswersToAnAliasedSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows; the alias is a POSIX deployment mechanism")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("no go toolchain on PATH to build the binary: %v", err)
	}
	dir := t.TempDir()
	real := filepath.Join(dir, defaultProgName)
	build := exec.Command("go", "build", "-o", real, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the binary: %v\n%s", err, out)
	}

	const alias = "legacy-alias-probe"
	link := filepath.Join(dir, alias)
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	// The alias path: exec through the symlink.
	aliasOut, aliasErr := runBinary(t, link, "--help")
	if !strings.Contains(aliasOut, alias+" [command]") {
		t.Fatalf("🔴 the binary invoked through a symlink named %q does not answer to that name.\n"+
			"That is the transitional alias, and without it a host-side guard that arms on the old "+
			"command name goes silently inert.\nstdout:\n%s\nstderr:\n%s", alias, aliasOut, aliasErr)
	}

	// NEGATIVE CONTROL: the SAME binary under its own name must print its own
	// name, so the assertion above is measuring argv[0] rather than a constant
	// that happens to equal the alias.
	realOut, realErr := runBinary(t, real, "--help")
	if !strings.Contains(realOut, defaultProgName+" [command]") {
		t.Fatalf("the binary under its own name does not print it; the alias assertion is not "+
			"measuring argv[0].\nstdout:\n%s\nstderr:\n%s", realOut, realErr)
	}
	if strings.Contains(realOut, alias) {
		t.Fatalf("the non-aliased invocation printed the alias name; the two are not distinguishable")
	}
}

func runBinary(t *testing.T, path string, args ...string) (stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(path, args...)
	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	// --help exits 0; anything else here is a real failure worth reporting.
	if err := cmd.Run(); err != nil {
		t.Fatalf("running %s %v: %v\nstdout:\n%s\nstderr:\n%s", path, args, err, out.String(), errOut.String())
	}
	return out.String(), errOut.String()
}

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeOpenCodeOnPath puts an `opencode` on PATH whose --version prints
// version ("" = no opencode on PATH at all). Not parallel: it sets PATH.
func fakeOpenCodeOnPath(t *testing.T, version string) {
	t.Helper()
	dir := t.TempDir()
	if version != "" {
		script := "#!/bin/sh\n[ \"$1\" = --version ] && echo '" + version + "' && exit 0\nexit 9\n"
		if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

// TestOpenCodePairSaysWhichOpencodeItNeeds: with no registration file, `muster-mirror
// opencode-pair` exits 1 and says what to do FOR THE OPENCODE THIS HOST HAS
// (onboarding finding 8): a 1.x is told muster needs opencode 2 and how to
// upgrade, and is never sent to `opencode service …` (a 2.x command); a 2.x is
// told to start its service; no opencode at all is told to install 2. Each
// fixture's version is distinct from MinMajorVersion's own digit where it can
// be, so a hard-coded "2" cannot pass for a read one.
func TestOpenCodePairSaysWhichOpencodeItNeeds(t *testing.T) {
	for name, c := range map[string]struct {
		version     string
		want, never []string
	}{
		// Round 3: opencode.ai/install gives a 1.x, and the fix a blind
		// install found was the v2 installer; and every case says what a
		// pair needs first, an opencode server that has run once.
		"opencode 1": {"1.19.5",
			[]string{"this host has opencode 1.19.5", "muster needs opencode 2 or later", "`opencode upgrade --major`", "opencode 1 has no background service",
				"`curl -fsSL https://opencode.ai/v2/install | bash`", "run opencode once first (e.g. `opencode serve`"},
			[]string{"`opencode service"}},
		// The boundary: exactly the oldest major muster works with.
		"opencode 2": {"opencode v2.0.32+6ec5682",
			[]string{"opencode 2.0.32+6ec5682 is installed", "`opencode service start`", "run opencode once first (e.g. `opencode serve`"},
			[]string{"upgrade", "muster needs", "v2/install"}},
		"opencode 3": {"opencode v3.4.1+abc1234",
			[]string{"opencode 3.4.1+abc1234 is installed", "`opencode service start`", "run opencode once first (e.g. `opencode serve`"},
			[]string{"upgrade", "muster needs", "v2/install"}},
		"no opencode": {"",
			[]string{"muster needs opencode 2 or later", "`opencode --version` did not run", "install opencode 2 with `curl -fsSL https://opencode.ai/v2/install | bash`",
				"https://opencode.ai/install installs opencode 1", "run opencode once first (e.g. `opencode serve`"},
			[]string{"opencode service start", "upgrade --major", "is not on this PATH"}},
		"no version": {"opencode (dev build)",
			[]string{"names no version", "muster needs opencode 2 or later"},
			[]string{"upgrade --major"}},
	} {
		t.Run(name, func(t *testing.T) {
			fakeOpenCodeOnPath(t, c.version)
			dir := t.TempDir()
			env := map[string]string{"HOME": filepath.Join(dir, "home"), "XDG_STATE_HOME": filepath.Join(dir, "state")}
			var out, errb bytes.Buffer
			rc := run([]string{"opencode-pair"}, func(k string) string { return env[k] }, &out, &errb)
			if rc != 1 {
				t.Fatalf("rc %d, want 1 (a pair that paired nothing is a failure); stderr %q", rc, errb.String())
			}
			msg := errb.String()
			if !strings.Contains(msg, "no opencode service registration file") {
				t.Errorf("the error does not say what is missing: %q", msg)
			}
			for _, w := range c.want {
				if !strings.Contains(msg, w) {
					t.Errorf("missing %q in %q", w, msg)
				}
			}
			for _, n := range c.never {
				if strings.Contains(msg, n) {
					t.Errorf("says %q, which does not fit this opencode: %q", n, msg)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "home", ".muster", "opencode-token")); err == nil {
				t.Error("a token file was written")
			}
		})
	}
}

// TestOpenCodePairFindsOpencodeOffPath: an opencode only in ~/.opencode/bin
// (where its installer puts it), run from a shell whose PATH lacks that
// directory (a blind install, round 3), is named with its directory, and its
// own version decides the advice: a 2.x is told to run opencode once, not to
// install it. The control is the same home with no opencode there.
func TestOpenCodePairFindsOpencodeOffPath(t *testing.T) {
	fakeOpenCodeOnPath(t, "") // nothing on PATH
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	bin := filepath.Join(home, ".opencode", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n[ \"$1\" = --version ] && echo 'opencode v2.3.0' && exit 0\nexit 9\n"
	if err := os.WriteFile(filepath.Join(bin, "opencode"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"HOME": home, "XDG_STATE_HOME": filepath.Join(dir, "state")}
	var out, errb bytes.Buffer
	if rc := run([]string{"opencode-pair"}, func(k string) string { return env[k] }, &out, &errb); rc != 1 {
		t.Fatalf("rc %d, want 1: %q", rc, errb.String())
	}
	msg := errb.String()
	for _, w := range []string{"opencode is at " + filepath.Join(bin, "opencode") + ", which is not on this PATH (put " + bin + " on PATH",
		"opencode 2.3.0 is installed", "run opencode once first (e.g. `opencode serve`"} {
		if !strings.Contains(msg, w) {
			t.Errorf("missing %q in %q", w, msg)
		}
	}
	if strings.Contains(msg, "did not run") || strings.Contains(msg, "v2/install") {
		t.Errorf("an installed opencode was called missing: %q", msg)
	}
	// Control: the same home without it is "not installed".
	if err := os.Remove(filepath.Join(bin, "opencode")); err != nil {
		t.Fatal(err)
	}
	errb.Reset()
	run([]string{"opencode-pair"}, func(k string) string { return env[k] }, &out, &errb)
	if !strings.Contains(errb.String(), "did not run") || strings.Contains(errb.String(), "is not on this PATH") {
		t.Errorf("control: %q", errb.String())
	}
}

// TestTheOpenCodeLauncherPrintsHelp: `muster-mirror opencode -h` (and -help,
// --help) prints the launcher's usage and exits 0, instead of refusing `-h` as
// an unknown flag (onboarding finding 8). It starts nothing: help returns
// before PATH is even looked at. The control is a flag it really does not know.
func TestTheOpenCodeLauncherPrintsHelp(t *testing.T) {
	fakeOpenCodeOnPath(t, "")
	for _, h := range []string{"-h", "-help", "--help"} {
		var out, errb bytes.Buffer
		rc := run([]string{"opencode", h}, func(string) string { return "" }, &out, &errb)
		if rc != 0 || !strings.HasPrefix(errb.String(), "usage: muster-mirror opencode [flags] [<directory>]\n") ||
			!strings.Contains(errb.String(), "-s, --session <id>") || strings.Contains(errb.String(), "unknown flag") {
			t.Errorf("%s: rc %d, output %q", h, rc, errb.String())
		}
	}
	var out, errb bytes.Buffer
	if rc := run([]string{"opencode", "--no-such-flag"}, func(string) string { return "" }, &out, &errb); rc != 2 ||
		!strings.Contains(errb.String(), `unknown flag "--no-such-flag"`) {
		t.Errorf("control: an unknown flag: rc %d %q", rc, errb.String())
	}
}

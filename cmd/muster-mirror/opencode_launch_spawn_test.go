package main

import (
	"errors"
	"slices"
	"testing"
)

// TestAHostSpawnsLauncherRunsExactlyTheBinaryItWasToldAndFailsClosed (audit
// #522 r1): started by the host spawn with --opencode-bin <abs>, the launcher
// checks THAT binary's version and execs exactly it — never opencode on PATH
// (lookPath here names another binary, so a lookup shows) — and the flag is
// not passed on to opencode. With that flag, a version it cannot read, a
// mismatched one, or a service it cannot reach REFUSES the start (exit 1,
// nothing execed), where a person's own start goes on (the controls).
func TestAHostSpawnsLauncherRunsExactlyTheBinaryItWasToldAndFailsClosed(t *testing.T) {
	const checked = "/srv/fixture/checked/opencode"
	r := newLaunchRig(t)
	var versioned []string
	d := r.deps()
	d.version = func(bin string) (string, error) { versioned = append(versioned, bin); return r.tuiVer, r.verErr }
	if rc := runOpenCodeLaunch([]string{"--opencode-bin", checked, "-s", "ses_fixtureSpawn"}, d, &r.stderr); rc != 0 {
		t.Fatalf("rc %d: %s", rc, r.stderr.String())
	}
	if !slices.Equal(r.execd, []string{checked, "opencode", "--session", "ses_fixtureSpawn"}) || !slices.Equal(versioned, []string{checked}) {
		t.Fatalf("execed %q, versioned %q; want exactly %s", r.execd, versioned, checked)
	}
	for _, c := range []struct {
		name    string
		control bool
		prep    func(*launchRig)
	}{
		{"a version that cannot be read", true, func(r *launchRig) { r.verErr = errors.New("exec format error") }},
		// A KNOWN mismatch refuses a person's start too
		// (TestTheLauncherRefusesAVersionMismatchNamingBothAndTheFix): no control.
		{"a mismatched version", false, func(r *launchRig) { r.tuiVer = "opencode v9.9.9" }},
		{"a service it cannot reach", true, func(r *launchRig) { r.srv.Close() }},
	} {
		strict := newLaunchRig(t)
		c.prep(strict)
		if rc := strict.run("--opencode-bin", checked, "-s", "ses_fixtureSpawn"); rc != 1 || strict.execd != nil {
			t.Fatalf("%s, for a spawn: rc %d, execed %q: %s", c.name, rc, strict.execd, strict.stderr.String())
		}
		if !c.control {
			continue
		}
		person := newLaunchRig(t)
		c.prep(person)
		if rc := person.run("-s", "ses_fixtureSpawn"); rc != 0 || person.execd == nil {
			t.Fatalf("%s, a person's start (the control): rc %d: %s", c.name, rc, person.stderr.String())
		}
	}
	if rc := newLaunchRig(t).run("--opencode-bin", "relative/opencode"); rc != 2 {
		t.Fatalf("a relative --opencode-bin: rc %d", rc)
	}
}

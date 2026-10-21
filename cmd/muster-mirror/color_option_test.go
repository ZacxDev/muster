package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// TestColorOptionReachesTheListing: -color-option (or its environment
// variable) is the user option the tmux listing reads — observed in the
// format the listing passes to tmux (a script here, never the operator's
// server) — the default is @muster-color, and a name that is not a plain tmux
// user option is refused before anything is listed.
func TestColorOptionReachesTheListing(t *testing.T) {
	dir := t.TempDir()
	args := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "tmux")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" >> '"+args+"'\n"), 0o755))
	prev := newTmuxBackend
	t.Cleanup(func() { newTmuxBackend = prev })
	listed := 0
	newTmuxBackend = func(s, sp, dir string, h *mirrorhost.HermesSource) mirrorhost.Backend {
		listed++
		return mirrorhost.Tmux{Bin: bin, Claude: mirrorhost.ClaudeLocator{Proc: t.TempDir(), Home: t.TempDir()}}
	}
	formatOf := func(env map[string]string, flags ...string) string {
		t.Helper()
		_ = os.Remove(args)
		if rc, _, errs := runWith(t, env, append([]string{"-list", "-windows"}, flags...)...); rc != 0 {
			t.Fatalf("-list -windows %v: rc %d %s", flags, rc, errs)
		}
		b, _ := os.ReadFile(args)
		return string(b)
	}
	for _, c := range []struct {
		env   map[string]string
		flags []string
		want  string
	}{
		{nil, nil, "@muster-color"},
		{map[string]string{envColorOption: "@from-env"}, nil, "@from-env"},
		{map[string]string{envColorOption: "@from-env"}, []string{"-color-option", "@from_flag"}, "@from_flag"},
	} {
		if got := formatOf(c.env, c.flags...); !strings.Contains(got, ";E:"+c.want+"}") || !strings.Contains(got, "/:"+c.want+"}") {
			t.Fatalf("env %v flags %v: the listing's format %q does not read %s", c.env, c.flags, got, c.want)
		}
	}
	before := listed
	for _, bad := range []string{"muster-color", "@a}b", "@", "@x y"} {
		rc, _, errs := runWith(t, nil, "-list", "-windows", "-color-option", bad)
		if rc != 2 || !strings.Contains(errs, "is not a tmux user option name") {
			t.Fatalf("-color-option %q: rc %d %q, want 2 and the refusal", bad, rc, errs)
		}
	}
	if listed != before {
		t.Fatalf("a refused -color-option still built a backend (%d times)", listed-before)
	}
}

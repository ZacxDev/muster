package mirrorhost

import (
	"slices"
	"testing"
)

// TestASpawnsArgvSkipsPermissionPromptsByDefaultAndIsUnchangedWhenOff pins the
// spawn's argv (spawnArgv) for both runtimes, both ways, as LITERAL words —
// never derived from the constants under test, so a changed flag spelling
// fails here: ON adds Claude Code's `--permission-mode bypassPermissions`
// before `--`, and opencode's `--auto` after the session; OFF is the argv as it
// was before the setting existed. And the setting's default: a zero
// AgentSpawnConfig (no -agent-spawn-skip-permissions=false) is ON.
func TestASpawnsArgvSkipsPermissionPromptsByDefaultAndIsUnchangedWhenOff(t *testing.T) {
	const (
		shell  = "/bin/fixture-sh"
		prompt = "/run/fixture/muster-mirror/prompts/7"
		claude = "/opt/fixture/bin/claude"
		sid    = "00000000-0000-4000-8000-000000000007"
		mm     = "/opt/fixture/bin/muster-mirror"
		oc     = "/opt/fixture/bin/opencode"
		ocSID  = "ses_fixture7"
	)
	wrapper := `p=$(cat -- "$1" && printf x) || exit 97; p=${p%x}; rm -f -- "$1"; shift; exec "$@" "$p"`
	for _, c := range []struct {
		name     string
		opencode bool
		skip     bool
		want     []string
	}{
		{"claude, on", false, true, []string{shell, "-l", "-c", wrapper, shell, prompt, claude, "--session-id", sid, "--permission-mode", "bypassPermissions", "--"}},
		{"claude, off", false, false, []string{shell, "-l", "-c", wrapper, shell, prompt, claude, "--session-id", sid, "--"}},
		{"opencode, on", true, true, []string{shell, "-l", "-c", `exec "$@"`, shell, mm, "opencode", "--opencode-bin", oc, "-s", ocSID, "--auto"}},
		{"opencode, off", true, false, []string{shell, "-l", "-c", `exec "$@"`, shell, mm, "opencode", "--opencode-bin", oc, "-s", ocSID}},
	} {
		bin, id, ocBin := claude, sid, ""
		if c.opencode {
			bin, id, ocBin = mm, ocSID, oc
		}
		if got := spawnArgv(shell, prompt, bin, id, c.opencode, ocBin, c.skip); !slices.Equal(got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
	var zero AgentSpawnConfig
	if zero.KeepPermissionPrompts {
		t.Fatal("a zero AgentSpawnConfig keeps permission prompts; the default must skip them")
	}
}

// TestEveryPermissionWordIsAHostWord: the permission words are argv words tmux
// receives, so each must pass the host-word rule (no trailing ';') and be among
// the enumerated constant words TestNoSpawnConstantWordEndsInASemicolon checks.
func TestEveryPermissionWordIsAHostWord(t *testing.T) {
	for _, w := range []string{"--permission-mode", "bypassPermissions", "--auto"} {
		if !hostWordRE.MatchString(w) {
			t.Errorf("%q is not a host word", w)
		}
		if !slices.Contains(spawnConstantWords, w) {
			t.Errorf("%q is not among spawnConstantWords", w)
		}
	}
}

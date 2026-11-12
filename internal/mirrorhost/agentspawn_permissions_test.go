package mirrorhost

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// TestASpawnsArgvSkipsPermissionPromptsByDefaultAndIsUnchangedWhenOff pins the
// spawn's argv (spawnArgv) for both runtimes, both ways, as LITERAL words —
// never derived from the constants under test, so a changed flag spelling
// fails here: ON adds Claude Code's `--permission-mode bypassPermissions` and
// the bypass screen's flag-scoped consent (`--settings
// {"skipDangerousModePermissionPrompt":true}`) before `--`, and opencode's `--auto` after the session; OFF is the argv as it
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
		{"claude, on", false, true, []string{shell, "-l", "-c", wrapper, shell, prompt, claude, "--session-id", sid, "--permission-mode", "bypassPermissions",
			"--settings", `{"skipDangerousModePermissionPrompt":true}`, "--"}},
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

// TestTheBypassAcceptanceIsTheFlagScopedSetting: a skipping claude spawn
// carries Claude Code's bypass-screen consent as `--settings <inline JSON>` —
// the FLAG settings source, this one process only — and the JSON is EXACTLY
// {"skipDangerousModePermissionPrompt": true} (the key the CLI reads; the
// legacy .claude.json key bypassPermissionsModeAccepted is not it). It sits
// after the permission mode and before `--`, it is an enumerated constant word
// (TestNoSpawnConstantWordEndsInASemicolon), and a spawn in normal permission
// mode, or an opencode one, carries no --settings at all.
func TestTheBypassAcceptanceIsTheFlagScopedSetting(t *testing.T) {
	const (
		shell  = "/bin/fixture-sh"
		prompt = "/run/fixture/muster-mirror/prompts/9"
		claude = "/opt/fixture/bin/claude"
		sid    = "00000000-0000-4000-8000-000000000009"
	)
	on := spawnArgv(shell, prompt, claude, sid, false, "", true)
	i := slices.Index(on, "--settings")
	if i < 0 || i+2 >= len(on) || on[i-1] != "bypassPermissions" || on[i+2] != "--" {
		t.Fatalf("the consent is not between the permission mode and --: %q", on)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(on[i+1]), &got); err != nil {
		t.Fatalf("--settings %q is not JSON: %v", on[i+1], err)
	}
	if want := map[string]any{"skipDangerousModePermissionPrompt": true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("--settings is %v; want exactly %v", got, want)
	}
	if !slices.Contains(spawnConstantWords, on[i+1]) || !slices.Contains(spawnConstantWords, "--settings") {
		t.Fatal("the consent words are not among spawnConstantWords")
	}
	for _, off := range [][]string{
		spawnArgv(shell, prompt, claude, sid, false, "", false),
		spawnArgv(shell, "", "/opt/fixture/bin/muster-mirror", "ses_fixture9", true, "/opt/fixture/bin/opencode", true),
	} {
		for _, w := range off {
			if w == "--settings" || strings.Contains(w, "skipDangerousModePermissionPrompt") {
				t.Fatalf("a spawn that does not skip claude's prompts carries the consent: %q", off)
			}
		}
	}
}

// TestARootMirrorStartsClaudeWithItsPromptsAndOpenCodeWithout: Claude Code
// exits 1 on bypassPermissions as root outside a sandbox of its own, so a root
// mirror (Geteuid 0) gives claude NO permission words — its argv is the
// setting-off argv, word for word — and ClaudeSkipsPermissions (what the hello
// says) is false; opencode's --auto is unaffected. Not root: unchanged. Off is
// off either way. Root is the Geteuid seam: nothing here runs as root.
func TestARootMirrorStartsClaudeWithItsPromptsAndOpenCodeWithout(t *testing.T) {
	const (
		shell  = "/bin/fixture-sh"
		prompt = "/run/fixture/muster-mirror/prompts/8"
		claude = "/opt/fixture/bin/claude"
		sid    = "00000000-0000-4000-8000-000000000008"
		mm     = "/opt/fixture/bin/muster-mirror"
		oc     = "/opt/fixture/bin/opencode"
		ocSID  = "ses_fixture8"
	)
	wrapper := `p=$(cat -- "$1" && printf x) || exit 97; p=${p%x}; rm -f -- "$1"; shift; exec "$@" "$p"`
	claudeOff := []string{shell, "-l", "-c", wrapper, shell, prompt, claude, "--session-id", sid, "--"}
	claudeOn := []string{shell, "-l", "-c", wrapper, shell, prompt, claude, "--session-id", sid, "--permission-mode", "bypassPermissions",
		"--settings", `{"skipDangerousModePermissionPrompt":true}`, "--"}
	ocOff := []string{shell, "-l", "-c", `exec "$@"`, shell, mm, "opencode", "--opencode-bin", oc, "-s", ocSID}
	ocOn := append(slices.Clone(ocOff), "--auto")
	for _, c := range []struct {
		name             string
		euid             int
		keep             bool
		claude, opencode []string
		hello            bool
	}{
		{"root, on", 0, false, claudeOff, ocOn, false},
		{"root, off", 0, true, claudeOff, ocOff, false},
		{"not root, on", 1000, false, claudeOn, ocOn, true},
		{"not root, off", 1000, true, claudeOff, ocOff, false},
	} {
		euid := c.euid
		sc := AgentSpawnConfig{KeepPermissionPrompts: c.keep, Geteuid: func() int { return euid }}
		if got := spawnArgv(shell, prompt, claude, sid, false, "", sc.spawnSkips(false)); !slices.Equal(got, c.claude) {
			t.Errorf("%s: claude\n got %q\nwant %q", c.name, got, c.claude)
		}
		if got := spawnArgv(shell, prompt, mm, ocSID, true, oc, sc.spawnSkips(true)); !slices.Equal(got, c.opencode) {
			t.Errorf("%s: opencode\n got %q\nwant %q", c.name, got, c.opencode)
		}
		if got := sc.ClaudeSkipsPermissions(); got != c.hello {
			t.Errorf("%s: ClaudeSkipsPermissions %v, want %v", c.name, got, c.hello)
		}
	}
}

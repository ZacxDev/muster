package mirrorhost

import (
	"path/filepath"
	"runtime"
	"testing"
)

// TestTheMCPSecretDefaultIsTheUserConfigDir: muster-mirror's default secret
// path is os.UserConfigDir()'s — XDG_CONFIG_HOME on Linux, ~/Library/Application
// Support on macOS — through internal/mirrorpath, the rule `muster doctor`
// calls too. This package's tests also run on CI's macOS runner, which is
// where the darwin arm is measured.
func TestTheMCPSecretDefaultIsTheUserConfigDir(t *testing.T) {
	home, xdg := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	want := filepath.Join(xdg, "muster-mirror", "mcp-secret")
	if runtime.GOOS == "darwin" {
		want = filepath.Join(home, "Library", "Application Support", "muster-mirror", "mcp-secret")
	}
	if got := DefaultMCPSecretFile(); got != want {
		t.Fatalf("DefaultMCPSecretFile() = %q, want %q (%s)", got, want, runtime.GOOS)
	}
}

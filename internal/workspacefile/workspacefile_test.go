package workspacefile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestEveryWayOutOfTheRootIsErrOutsideWhetherOrNotTheTargetExists: `..`, an
// absolute path elsewhere and a symlink that leads out are each ErrOutside —
// a MISSING target outside too (the lexical check, before any symlink is
// read), so a caller can tell "not yours" from "not there". A nested file and
// a symlink that stays inside resolve.
// TestAHiddenFileOrDirectoryIsRefused (round 1 G3): a resolved location
// whose path below the root has a component starting with "." — a dotfile or
// anything in a dot-directory (.ssh, .aws, .git, .env) — is ErrHidden, however
// it was named (directly, or through a symlink that stays inside the root). A
// root that itself sits under a dot-directory is not refused for it.
func TestAHiddenFileOrDirectoryIsRefused(t *testing.T) {
	parent := filepath.Join(t.TempDir(), ".config")
	root := filepath.Join(parent, "work")
	for _, d := range []string{filepath.Join(root, ".ssh"), filepath.Join(root, "sub", ".git")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{".env", ".ssh/id_ed25519", "sub/.git/config", "ok.txt", "sub/ok.txt"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, ".env"), filepath.Join(root, "env.txt")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{".env", ".ssh/id_ed25519", "sub/.git/config", "env.txt", filepath.Join(root, ".env")} {
		if _, err := Resolve(root, p); !errors.Is(err, ErrHidden) {
			t.Errorf("%s: %v, want ErrHidden", p, err)
		}
	}
	// The control: ordinary files under a root inside ".config" resolve.
	for _, p := range []string{"ok.txt", "sub/ok.txt"} {
		if _, err := Resolve(root, p); err != nil {
			t.Errorf("%s: %v, want it resolved", p, err)
		}
	}
}

func TestEveryWayOutOfTheRootIsErrOutsideWhetherOrNotTheTargetExists(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "work")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "secret.txt")
	inside := filepath.Join(root, "sub", "ok.txt")
	for _, f := range []string{outside, inside} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "out.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(inside, filepath.Join(root, "in.txt")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"../secret.txt", outside, "out.txt", "../missing.txt", filepath.Join(parent, "missing.txt"), "sub/../../secret.txt"} {
		if got, err := Resolve(root, p); !errors.Is(err, ErrOutside) {
			t.Errorf("%s: %q, %v; want ErrOutside", p, got, err)
		}
	}
	realInside, _ := filepath.EvalSymlinks(inside)
	for _, p := range []string{"sub/ok.txt", inside, "in.txt"} {
		if got, err := Resolve(root, p); err != nil || got != realInside {
			t.Errorf("%s: %q, %v; want %q", p, got, err, realInside)
		}
	}
	if _, err := Resolve(root, "sub"); err == nil {
		t.Error("a directory resolved as a file")
	}
}

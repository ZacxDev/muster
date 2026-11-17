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

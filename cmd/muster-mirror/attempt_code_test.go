package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestTheAttemptCodeFileIsPrivateAndOnlyItsOwnerRemovesIt: muster-mirror
// leaves its attempt code for `muster doctor` in a 0600 file under a 0700
// directory, and at exit removes it only while it still holds ITS code (a
// newer muster-mirror's file stays).
func TestTheAttemptCodeFileIsPrivateAndOnlyItsOwnerRemovesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg", "muster-mirror", "attempt-code")
	if err := writeAttemptCode(path, "AB12-CD34-EF56"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the code file: %v, mode %v; want 0600", err, fi.Mode())
	}
	if di, _ := os.Stat(filepath.Dir(path)); di.Mode().Perm() != 0o700 {
		t.Fatalf("its directory: mode %v; want 0700", di.Mode())
	}
	if b, _ := os.ReadFile(path); string(b) != "AB12-CD34-EF56\n" {
		t.Fatalf("the code file holds %q", b)
	}
	removeAttemptCode(path, "ZZ99-ZZ99-ZZ99") // another process's code: kept
	if _, err := os.Stat(path); err != nil {
		t.Fatal("a process removed a code file that was not its own")
	}
	removeAttemptCode(path, "AB12-CD34-EF56")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the owner's exit left its code file: %v", err)
	}
}

// TestTheAttemptCodeFileReplacesASymlinkRatherThanFollowingIt: a symlink at
// the code file's path is replaced by a regular 0600 file, and the file it
// pointed at is left untouched.
func TestTheAttemptCodeFileReplacesASymlinkRatherThanFollowingIt(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "attempt-code")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := writeAttemptCode(path, "AB12-CD34-EF56"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the code file after a write over a symlink: %v, mode %v; want a regular 0600 file", err, fi.Mode())
	}
	if b, _ := os.ReadFile(target); string(b) != "keep me\n" {
		t.Fatalf("the symlink's target was written through: %q", b)
	}
}

package compose_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/ZacxDev/muster/internal/provision/compose"
)

// finalName maps a temporary file's path (".spec.json.tmp-123") to the name it
// is renamed to, relative to root.
func finalName(root, p string) string {
	rel, _ := filepath.Rel(root, p)
	base := filepath.Base(rel)
	if i := strings.Index(base, ".tmp-"); i > 0 && strings.HasPrefix(base, ".") {
		rel = filepath.Join(filepath.Dir(rel), base[1:i])
	}
	return filepath.ToSlash(rel)
}

// TestTheSessionsGroupGetsTheFilesAndNotTheSpec: every path the driver writes
// is given its group — SpecGroup for spec.json, driver.json and every
// directory outside files/ (the agent's session must not read its own turn
// bearer), FilesGroup for files/ and below (the managed CLAUDE.md). The group
// ids are pairwise distinct and distinct from the process's own, so a mapping
// that swapped or dropped one cannot pass.
func TestTheSessionsGroupGetsTheFilesAndNotTheSpec(t *testing.T) {
	root := t.TempDir()
	got := map[string]int{}
	d := newDriver(t, compose.Config{Root: root, SpecGroup: 4101, FilesGroup: 4202, Chown: func(p string, gid int) error {
		got[finalName(root, p)] = gid
		return nil
	}})
	if err := d.Create(context.Background(), chiefSpec()); err != nil {
		t.Fatal(err)
	}
	want := map[string]int{
		"chief":                                 4101,
		"chief/spec.json":                       4101,
		"chief/driver.json":                     4101,
		"chief/files":                           4202,
		"chief/files/etc":                       4202,
		"chief/files/etc/claude-code":           4202,
		"chief/files/etc/claude-code/CLAUDE.md": 4202,
		"chief/files/etc/ccd":                   4202,
		"chief/files/etc/ccd/extra.json":        4202,
	}
	for p, g := range want {
		if got[p] != g {
			t.Errorf("%s: group %d, want %d", p, got[p], g)
		}
	}
	if len(got) != len(want) {
		t.Errorf("groups set on %d path(s), want %d: %v", len(got), len(want), got)
	}
}

// TestAGroupThatCannotBeSetStopsTheWriteBeforeTheSpec: a refused group change
// fails the Create, and spec.json — written last — is never written.
func TestAGroupThatCannotBeSetStopsTheWriteBeforeTheSpec(t *testing.T) {
	root := t.TempDir()
	d := newDriver(t, compose.Config{Root: root, SpecGroup: 4101, FilesGroup: 4202, Chown: func(p string, gid int) error {
		if gid == 4202 {
			return errors.New("operation not permitted")
		}
		return nil
	}})
	err := d.Create(context.Background(), chiefSpec())
	if err == nil || !strings.Contains(err.Error(), "group 4202: operation not permitted") {
		t.Fatalf("Create with a failing group change: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "chief", "spec.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("spec.json was written although a file's group could not be set: %v", err)
	}
}

// TestTheRealGroupChangeIsApplied: the default chown, to the process's own
// group (the one group a test can always give), lands on disk.
func TestTheRealGroupChangeIsApplied(t *testing.T) {
	root := t.TempDir()
	gid := os.Getgid()
	if gid == 0 {
		t.Skip("running as group 0, which the driver treats as \"leave as inherited\"")
	}
	d := newDriver(t, compose.Config{Root: root, SpecGroup: gid, FilesGroup: gid})
	if err := d.Create(context.Background(), chiefSpec()); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(root, "chief", "spec.json"))
	if err != nil {
		t.Fatal(err)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Gid) != gid {
		t.Fatalf("spec.json's group: %+v, want %d", fi.Sys(), gid)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Fatalf("spec.json's mode after the group change: %o, want 0640", fi.Mode().Perm())
	}
	if _, err := compose.New(compose.Config{Root: root, Admit: admitAll, SpecGroup: -1}); err == nil {
		t.Fatal("a negative group id was accepted")
	}
}

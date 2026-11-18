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

// TestTheRealGroupChangeIsApplied: the default chown lands on disk. It needs
// two distinct non-zero groups the process is in (New refuses equal groups),
// so a process in fewer skips.
func TestTheRealGroupChangeIsApplied(t *testing.T) {
	root := t.TempDir()
	var gids []int
	all, _ := os.Getgroups()
	for _, g := range append([]int{os.Getgid()}, all...) {
		if g != 0 && (len(gids) == 0 || gids[0] != g) {
			gids = append(gids, g)
		}
		if len(gids) == 2 {
			break
		}
	}
	if len(gids) < 2 {
		t.Skipf("the process is in fewer than two non-zero groups (%v)", gids)
	}
	specGID, filesGID := gids[0], gids[1]
	d := newDriver(t, compose.Config{Root: root, SpecGroup: specGID, FilesGroup: filesGID})
	if err := d.Create(context.Background(), chiefSpec()); err != nil {
		t.Fatal(err)
	}
	for rel, gid := range map[string]int{"spec.json": specGID, "files/etc/claude-code/CLAUDE.md": filesGID} {
		fi, err := os.Stat(filepath.Join(root, "chief", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Gid) != gid {
			t.Fatalf("%s's group: %+v, want %d", rel, fi.Sys(), gid)
		}
		if fi.Mode().Perm() != 0o640 {
			t.Fatalf("%s's mode after the group change: %o, want 0640", rel, fi.Mode().Perm())
		}
	}
	if _, err := compose.New(compose.Config{Root: root, Admit: admitAll, SpecGroup: -1, FilesGroup: 4202}); err == nil {
		t.Fatal("a negative group id was accepted")
	}
}

// TestNewRefusesGroupsThatWouldExposeTheSpec: spec.json holds the agent's
// turn bearer, and the two groups are all that keeps it from the agent's
// session, so New fails closed on a zero (inherited) group and on equal ones.
func TestNewRefusesGroupsThatWouldExposeTheSpec(t *testing.T) {
	for _, c := range []struct {
		name              string
		specGID, filesGID int
		want              string
	}{
		{"SpecGroup 0", 0, 4202, "SpecGroup and FilesGroup are both required (got 0 and 4202)"},
		{"FilesGroup 0", 4101, 0, "SpecGroup and FilesGroup are both required (got 4101 and 0)"},
		{"equal groups", 4101, 4101, "SpecGroup and FilesGroup are both 4101: the agent's group would read spec.json"},
	} {
		_, err := compose.New(compose.Config{Root: "/run/muster-spec", Admit: admitAll, SpecGroup: c.specGID, FilesGroup: c.filesGID})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: New = %v, want an error containing %q", c.name, err, c.want)
		}
	}
	// Positive control: two distinct non-zero groups are accepted.
	if _, err := compose.New(compose.Config{Root: "/run/muster-spec", Admit: admitAll, SpecGroup: 4101, FilesGroup: 4202}); err != nil {
		t.Fatalf("New with distinct groups: %v", err)
	}
}

package mirrorhost

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// trustSpawnedWorktree (design 3.6.1): the ONLY write muster makes to Claude
// Code's configuration. A real git clone and worktree, because check 4 asks git
// whether the clone lists the directory.

// trustStateFixture is a state file with the real file's SHAPE — many projects,
// MCP servers, unknown keys, strings holding < > & — and invented values.
const trustStateFixture = `{
  "numStartups": 412,
  "theme": "dark",
  "userID": "00000000000000000000000000000000000000000000000000000000deadbeef",
  "mcpServers": {
    "muster": {"type": "http", "url": "http://127.0.0.1:47823/mcp", "headers": {"Authorization": "Bearer ${MUSTER_MIRROR_MCP_SECRET:-}"}}
  },
  "tipsHistory": {"shift-enter": 3, "memory-command": 1},
  "someFutureKey": [1, "two", {"three": null}],
  "projects": {
    "/srv/projects/alpha": {"allowedTools": [], "hasTrustDialogAccepted": true, "history": [{"display": "if a < b && c > d"}]},
    "/srv/projects/beta": {"hasTrustDialogAccepted": false, "mcpContextUris": ["<none>"]},
    "/srv/projects/gamma & delta": {"hasTrustDialogAccepted": true, "lastCost": 0.25}
  },
  "oauthAccount": {"emailAddress": "nobody@example.invalid", "organizationName": "R&D <test>"}
}
`

type trustRig struct {
	u                *spawnUnit
	clone, dir, root string
	state            string
}

func newTrustRig(t *testing.T) *trustRig {
	t.Helper()
	u := newSpawnUnit(t)
	r := &trustRig{u: u, root: u.c.cfg.AgentSpawn.Roots[0], state: u.c.cfg.AgentSpawn.ClaudeState}
	r.clone = filepath.Join(r.root, "widgets")
	gitIn(t, "", "init", "-q", "-b", "main", r.clone)
	gitIn(t, r.clone, "commit", "-q", "--allow-empty", "-m", "first")
	if err := os.MkdirAll(filepath.Join(u.dir, "acme__widgets"), 0o700); err != nil {
		t.Fatal(err)
	}
	r.dir = filepath.Join(realOrClean(u.dir), "acme__widgets", "17")
	gitIn(t, r.clone, "worktree", "add", "-q", "-b", "muster/17-x", r.dir)
	if err := os.WriteFile(r.state, []byte(trustStateFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	return r
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@example.invalid", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

func (r *trustRig) trust(dir string, dispatch int64, created bool) error {
	return r.u.c.trustSpawnedWorktree(context.Background(), r.clone, dir, dispatch, created)
}

// TestTheTrustEntryChangesExactlyOneValue: after the write the file decodes to
// the original everywhere except projects[dir], which is exactly
// {"hasTrustDialogAccepted": true}; every '<', '>' and '&' comes through
// unescaped; the file is 0600.
func TestTheTrustEntryChangesExactlyOneValue(t *testing.T) {
	r := newTrustRig(t)
	if err := r.trust(r.dir, 17, true); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(r.state)
	var before, after map[string]any
	_ = json.Unmarshal([]byte(trustStateFixture), &before)
	if err := json.Unmarshal(got, &after); err != nil {
		t.Fatal(err)
	}
	entry := after["projects"].(map[string]any)[r.dir]
	if e, ok := entry.(map[string]any); !ok || len(e) != 1 || e["hasTrustDialogAccepted"] != true {
		t.Fatalf("the entry is %v", entry)
	}
	delete(after["projects"].(map[string]any), r.dir)
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if !bytes.Equal(a, b) {
		t.Fatalf("something else changed:\nbefore %s\nafter  %s", a, b)
	}
	for _, s := range []string{"if a < b && c > d", "<none>", "gamma & delta", "R&D <test>"} {
		if !strings.Contains(string(got), s) {
			t.Errorf("%q is not in the file as written (escaped?)", s)
		}
	}
	if fi, _ := os.Stat(r.state); fi.Mode().Perm() != 0o600 {
		t.Errorf("the state file is %v", fi.Mode().Perm())
	}
}

// TestTheTrustEntryRefusesEverythingElseAndLeavesTheFile: each refusal is its own
// case and detail, and the file's bytes are unchanged (compared byte for byte).
func TestTheTrustEntryRefusesEverythingElseAndLeavesTheFile(t *testing.T) {
	r := newTrustRig(t)
	home, _ := os.UserHomeDir()
	link := filepath.Join(r.u.tmp, "link-to-worktree")
	if err := os.Symlink(r.dir, link); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(filepath.Dir(r.dir), "18")
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name     string
		dir      string
		dispatch int64
		created  bool
		prepare  func()
		why      string
	}{
		{"a symlinked dir", link, 17, true, nil, "not its own real path"},
		{"the agents directory itself", realOrClean(r.u.dir), 17, true, nil, "not a worktree of this spawn under the agents directory"},
		{"a repository-level directory", filepath.Dir(r.dir), 17, true, nil, "not a worktree of this spawn under the agents directory"},
		{"a path one level deeper", filepath.Join(r.dir, "sub"), 17, true, func() { _ = os.MkdirAll(filepath.Join(r.dir, "sub"), 0o700) }, "not a worktree of this spawn under the agents directory"},
		{"another dispatch's directory", sibling, 17, true, nil, "not a worktree of this spawn under the agents directory"},
		{"a sibling this spawn did not create", sibling, 18, false, nil, "this spawn did not create it"},
		// git LISTS this one, so only the created check can refuse it.
		{"a listed worktree this spawn did not create", r.dir, 17, false, nil, "this spawn did not create it"},
		{"a sibling git does not list", sibling, 18, true, nil, "the clone does not list it as a worktree"},
		{"the clone, under a root", realOrClean(r.clone), 17, true, nil, "not a worktree of this spawn under the agents directory"},
		{"home", home, 17, true, nil, "not a worktree of this spawn under the agents directory"},
		{"a relative path", "acme__widgets/17", 17, true, nil, "not an absolute, clean path"},
		{"a missing state file", r.dir, 17, true, func() { _ = os.Remove(r.state) }, "Claude Code's state file cannot be read"},
		{"an unparseable state file", r.dir, 17, true, func() { _ = os.WriteFile(r.state, []byte(`{"projects": `), 0o600) }, "Claude Code's state file does not parse"},
		{"an existing entry for the directory", r.dir, 17, true, func() {
			_ = os.WriteFile(r.state, []byte(`{"projects": {"`+r.dir+`": {"hasTrustDialogAccepted": false}}}`), 0o600)
		}, "an entry for this new directory already exists"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_ = os.WriteFile(r.state, []byte(trustStateFixture), 0o600)
			if c.prepare != nil {
				c.prepare()
			}
			before, berr := os.ReadFile(r.state)
			err := r.trust(c.dir, c.dispatch, c.created)
			e, ok := err.(*errSpawn)
			if !ok || e.code != "failed" || e.detail != "trust entry not written: "+c.why {
				t.Fatalf("refusal %v; want failed %q", err, "trust entry not written: "+c.why)
			}
			after, aerr := os.ReadFile(r.state)
			if (berr == nil) != (aerr == nil) || !bytes.Equal(before, after) {
				t.Fatal("the state file changed on a refusal")
			}
		})
	}
	// The opencode case (check 5): only spawnClaude calls the writer, so an
	// opencode spawn cannot reach it — pinned by its single caller.
	if n := strings.Count(readSelf(t, "agentspawn.go"), "c.trustSpawnedWorktree("); n != 1 {
		t.Errorf("trustSpawnedWorktree has %d callers; want exactly spawnClaude's", n)
	}
}

func readSelf(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestATrustReadBackMismatchIsRefusedAndNeverRestored: a different valid file
// is swapped in between the rename and the read-back, as a concurrent Claude
// Code write would be. The write is refused, and the swapped-in file is LEFT
// EXACTLY AS IT WAS (restoring would revert that other writer).
func TestATrustReadBackMismatchIsRefusedAndNeverRestored(t *testing.T) {
	r := newTrustRig(t)
	other := []byte(`{"numStartups": 413, "projects": {}}` + "\n")
	r.u.c.cfg.AgentSpawn.beforeTrustRead = func(path string) { _ = os.WriteFile(path, other, 0o600) }
	err := r.trust(r.dir, 17, true)
	if e, ok := err.(*errSpawn); !ok || e.detail != "trust entry not written: the file read back differs (another writer?)" {
		t.Fatalf("refusal %v", err)
	}
	if got, _ := os.ReadFile(r.state); !bytes.Equal(got, other) {
		t.Fatalf("the other writer's file was changed: %s", got)
	}
}

package mirrorhost

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestTheSweepRemovesASweptScratchDirectorysTrustEntry (O-B round 0, finding
// 7): the trust entry a scratch spawn wrote goes with its directory — the
// EXACT key only. A live instance's entry, keys that merely start with the
// swept path (…/830, …/83/sub), the scratch root's own key and an unrelated
// project are all left, and nothing else in the file moves. Under Claude
// Code's lock: with the lock held the directory goes but its entry is kept,
// and said so.
func TestTheSweepRemovesASweptScratchDirectorysTrustEntry(t *testing.T) {
	u := newSpawnUnit(t)
	var mu sync.Mutex
	var logged strings.Builder
	u.c.log = log.New(lockedWriter{mu: &mu, b: &logged}, "", 0)
	u.c.cfg.AgentSpawn.TrustLockWait = 50 * time.Millisecond
	root := filepath.Join(u.c.spawnDir(), scratchDirName)
	live, gone := filepath.Join(root, "81"), filepath.Join(root, "83")
	mkdirs := func() {
		t.Helper()
		for _, d := range []string{live, gone} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	keep := []string{live, gone + "0", filepath.Join(gone, "sub"), root, "/srv/projects/alpha & beta"}
	writeState := func() {
		t.Helper()
		projects := map[string]any{gone: map[string]any{"hasTrustDialogAccepted": true, "history": []any{"x < y"}}}
		for _, k := range keep {
			projects[k] = map[string]any{"hasTrustDialogAccepted": true}
		}
		b, _ := json.Marshal(map[string]any{"numStartups": 7, "projects": projects, "theme": "dark"})
		if err := os.WriteFile(u.c.cfg.AgentSpawn.ClaudeState, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	read := func() map[string]any {
		t.Helper()
		var st map[string]any
		got, _ := os.ReadFile(u.c.cfg.AgentSpawn.ClaudeState)
		if err := json.Unmarshal(got, &st); err != nil {
			t.Fatal(err)
		}
		return st
	}

	// Claude Code holds its lock: the directory goes, its entry stays.
	mkdirs()
	writeState()
	lock := u.c.cfg.AgentSpawn.ClaudeState + ".lock"
	if err := os.Mkdir(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	u.c.SweepAgentWorktrees(context.Background(), map[int64]bool{81: true})
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Fatalf("the gone instance's scratch directory is still there: %v", err)
	}
	if _, ok := read()["projects"].(map[string]any)[gone]; !ok {
		t.Fatal("the trust entry was removed while Claude Code held its lock")
	}
	mu.Lock()
	said := strings.Contains(logged.String(), "its Claude Code trust entry kept: Claude Code's state file is locked by another writer")
	mu.Unlock()
	if !said {
		t.Fatalf("a kept trust entry was not logged: %q", logged.String())
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}

	// A directory the sweep cannot remove keeps its entry: the scratch root
	// read-only, so its child cannot be unlinked (as any user but root, whom
	// the mode does not stop — only there is this block not run).
	if os.Geteuid() != 0 {
		mkdirs()
		writeState()
		if err := os.Chmod(root, 0o500); err != nil {
			t.Fatal(err)
		}
		u.c.SweepAgentWorktrees(context.Background(), map[int64]bool{81: true})
		if err := os.Chmod(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(gone); err != nil {
			t.Fatalf("control: the read-only scratch root did not stop the removal: %v", err)
		}
		if _, ok := read()["projects"].(map[string]any)[gone]; !ok {
			t.Fatal("the trust entry of a scratch directory the sweep KEPT was removed")
		}
	}

	// Unlocked: the swept directory's entry, and only it, goes.
	mkdirs()
	writeState()
	u.c.SweepAgentWorktrees(context.Background(), map[int64]bool{81: true})
	st := read()
	got := st["projects"].(map[string]any)
	if _, ok := got[gone]; ok {
		t.Fatalf("the swept scratch directory's trust entry is still there: %v", got)
	}
	for _, k := range keep {
		if _, ok := got[k]; !ok {
			t.Errorf("the sweep removed %q's entry; only %q's may go", k, gone)
		}
	}
	if len(got) != len(keep) || st["numStartups"] != float64(7) || st["theme"] != "dark" || len(st) != 3 {
		t.Fatalf("something else in the state file moved: %v", st)
	}
	if fi, _ := os.Stat(u.c.cfg.AgentSpawn.ClaudeState); fi.Mode().Perm() != 0o600 {
		t.Errorf("the state file is %v after the removal", fi.Mode().Perm())
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("the live instance's scratch directory was touched: %v", err)
	}

	// Called directly, the remover refuses anything but a scratch directory
	// of the agents directory, and leaves the file.
	writeState()
	before, _ := os.ReadFile(u.c.cfg.AgentSpawn.ClaudeState)
	for _, d := range []string{root, filepath.Join(u.c.spawnDir(), "acme__widgets", "83"), filepath.Join(gone, "sub"), filepath.Join(root, "x83")} {
		if why := u.c.untrustScratchDir(d); why != "not a scratch directory under the agents directory" {
			t.Errorf("untrust %s: %q, want the refusal", d, why)
		}
	}
	if after, _ := os.ReadFile(u.c.cfg.AgentSpawn.ClaudeState); string(after) != string(before) {
		t.Fatal("a refused removal changed the state file")
	}
}

// lockedWriter is a log destination safe to read while the sweep writes.
type lockedWriter struct {
	mu *sync.Mutex
	b  *strings.Builder
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

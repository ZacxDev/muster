package agentprovision

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentprovision/worktreetest"
)

// slowFirstUploadPack writes an --upload-pack command whose FIRST run stands
// in for an unshallow cut off mid-transfer: it leaves a partial pack's tmp_*
// files in packDir — what index-pack leaves when a slow transfer is killed
// (measured on a worker: 2.7–16 MB per attempt) — records that it did, and
// stalls until the unshallow's timeout kills it. Every later run is a plain
// upload-pack. It returns the command and the record's path.
func slowFirstUploadPack(t *testing.T, packDir string) (cmd, made string) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "upload-pack.sh")
	made = filepath.Join(dir, "made")
	body := "#!/bin/sh\nflag='" + filepath.Join(dir, "slowed") + "'\n" +
		"if [ ! -e \"$flag\" ]; then : > \"$flag\"\n" +
		"  head -c 4096 /dev/zero > '" + filepath.Join(packDir, "tmp_pack_cut") + "'\n" +
		"  : > '" + filepath.Join(packDir, "tmp_idx_cut") + "'\n" +
		"  : > '" + made + "'\n" +
		"  exec sleep 60\nfi\n" +
		"exec git upload-pack \"$@\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return "sh " + script, made
}

// tmpPacks is every tmp_* file in the repository at dir's pack directory.
func tmpPacks(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(dir, ".git", "objects", "pack"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "tmp_") {
			out = append(out, e.Name())
		}
	}
	return out
}

// runWorktreeScript runs the job worktree script for job id on pull request 7
// at sha, from url with upload-pack up and an unshallow budget of budget
// seconds, returning its combined output and exit error.
func runWorktreeScript(r worktreetest.Repo, root, id, sha, url, up, budget string) (string, error) {
	args := append([]string{"-c", jobOverlayScript, "muster-job-worktree", r.Checkout, filepath.Join(root, ".muster-jobs", id),
		sha, url, "7", "", up, "1", "", budget}, JobOverlayRootFiles...)
	out, err := exec.Command("sh", args...).CombinedOutput()
	return string(out), err
}

// TestAKilledUnshallowLeavesNoTempPackAndBacksOff: an unshallow killed by its
// timeout mid-transfer (slowFirstUploadPack) leaves the partial pack's tmp_*
// files; the script removes the ones made since the attempt started — and only
// those: a tmp_pack older than the attempt (another process's) survives —
// records the failure, and the job is still made. The next job does NOT retry
// (the backoff note, no --unshallow, still shallow); once the backoff has
// passed the next job unshallows and the failure record goes.
func TestAKilledUnshallowLeavesNoTempPackAndBacksOff(t *testing.T) {
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skip("no timeout on PATH")
	}
	root := t.TempDir()
	r := worktreetest.New(t, filepath.Join(root, "widgets"), filepath.Join(root, "markers"))
	deepenOrigin(t, r, 30, 9)
	url := r.Origin

	// Another process's pack in flight, from before the attempt: it stays.
	pack := filepath.Join(r.Checkout, ".git", "objects", "pack")
	foreign := filepath.Join(pack, "tmp_pack_foreign")
	if err := os.WriteFile(foreign, []byte("in flight"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(foreign, old, old); err != nil {
		t.Fatal(err)
	}

	up, made := slowFirstUploadPack(t, pack)
	out, err := runWorktreeScript(r, root, "41", r.Commit, url, up, "2")
	if err != nil {
		t.Fatalf("the job failed when only its unshallow did: %v\n%s", err, out)
	}
	if _, err := os.Stat(made); err != nil {
		t.Fatalf("instrument: the cut-off unshallow never left its tmp_* files: %v\n%s", err, out)
	}
	if !strings.Contains(out, UnshallowFailedNote) {
		t.Fatalf("instrument: the unshallow did not fail (no note):\n%s", out)
	}
	if got := tmpPacks(t, r.Checkout); len(got) != 1 || got[0] != "tmp_pack_foreign" {
		t.Fatalf("tmp_* files after the cut-off unshallow = %q, want only the older tmp_pack_foreign", got)
	}
	mark := filepath.Join(r.Checkout, ".git", UnshallowFailedFile)
	if b, err := os.ReadFile(mark); err != nil || strings.TrimSpace(string(b)) != "1" {
		t.Fatalf("failure record = %q, %v; want 1", b, err)
	}

	// The next job: backed off.
	log := worktreetest.GitShim(t, os.Getenv("PATH"))
	out, err = runWorktreeScript(r, root, "42", r.Commit, url, up, "60")
	if err != nil {
		t.Fatalf("second job: %v\n%s", err, out)
	}
	if !strings.Contains(out, UnshallowBackoffNote) {
		t.Fatalf("the second job did not report the backoff:\n%s", out)
	}
	for _, argv := range worktreetest.GitShimCalls(t, log) {
		for _, a := range argv {
			if a == "--unshallow" {
				t.Fatalf("the second job retried the unshallow inside the backoff: %q", argv)
			}
		}
	}
	if got := worktreetest.Git(t, r.Checkout, "rev-parse", "--is-shallow-repository"); got != "true" {
		t.Fatalf("checkout shallow = %s, want still true", got)
	}

	// After the backoff (1 h for one failure): the next job unshallows.
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(mark, past, past); err != nil {
		t.Fatal(err)
	}
	out, err = runWorktreeScript(r, root, "43", r.Commit, url, up, "60")
	if err != nil {
		t.Fatalf("third job: %v\n%s", err, out)
	}
	if got := worktreetest.Git(t, r.Checkout, "rev-parse", "--is-shallow-repository"); got != "false" {
		t.Fatalf("after the backoff the checkout is still shallow (%s):\n%s", got, out)
	}
	if _, err := os.Lstat(mark); !os.IsNotExist(err) {
		t.Fatalf("the failure record survived a successful unshallow (lstat: %v)", err)
	}
}

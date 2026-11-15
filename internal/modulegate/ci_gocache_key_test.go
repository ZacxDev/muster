package modulegate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// THE `test` JOB'S GO CACHE: WHAT A SAVE IS KEYED ON, AND THE ORDER A RESTORE
// FALLS BACK IN.
//
// 🔴 The runner IMAGE VERSION is in every saved key and is the FIRST thing a
// restore gives up while go.sum is unchanged; once go.sum changes, the last
// prefix takes the newest cache from any image. Operator decision 2026-11-15:
// with two hosted image versions in rotation, a restore that required it left
// shard 0 cold on main run 43741778087. Everything else Go cannot see — the Go version, the
// Postgres service digest, the shard — stays in the fixed prefix that no
// restore key drops, and go.sum is dropped only last.
//
// 🔴 EXACT LINES, in the style of ci_concurrency_test.go: restore keys are
// string PREFIXES, so the order of the fields in the key IS the fallback
// policy, and a reordering that still "mentions" every field changes it.
//
// WHAT THIS CANNOT SEE: it reads the YAML text, not what the cache action
// matched; the `Cache restored from key:` line of a shard's log is that.
// ---------------------------------------------------------------------------

const (
	wantGoCachePrefix = `echo "prefix=gotest-${RUNNER_OS}-${ImageOS}-go${{ steps.go.outputs.go-version }}-pg${pg:0:16}-shard${{ strategy.job-index }}of${{ strategy.job-total }}"`
	wantGoCacheImage  = `echo "image=img${ImageVersion}"`
	wantGoCacheKey    = `key: ${{ steps.gocache.outputs.prefix }}-${{ hashFiles('go.sum') }}-${{ steps.gocache.outputs.image }}-${{ github.run_id }}-${{ github.run_attempt }}`
)

var wantGoCacheRestoreKeys = []string{
	`${{ steps.gocache.outputs.prefix }}-${{ hashFiles('go.sum') }}-${{ steps.gocache.outputs.image }}-`,
	`${{ steps.gocache.outputs.prefix }}-${{ hashFiles('go.sum') }}-`,
	`${{ steps.gocache.outputs.prefix }}-`,
}

// TestCIGoCacheFallsBackAcrossImageVersionsFirst pins the key, the save key
// and the restore-key order of the `test` job's Go cache.
func TestCIGoCacheFallsBackAcrossImageVersionsFirst(t *testing.T) {
	path := filepath.Join(moduleRoot(t), ".github", "workflows", "ci.yml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := jobLines(string(b), "test")
	if len(lines) == 0 {
		t.Fatalf("positive control FAILED: no `test:` job in %s", path)
	}
	count := func(want string) int {
		n := 0
		for _, l := range lines {
			if l == want {
				n++
			}
		}
		return n
	}
	for want, n := range map[string]int{wantGoCachePrefix: 1, wantGoCacheImage: 1, wantGoCacheKey: 2} {
		if got := count(want); got != n {
			t.Errorf("the `test` job has %d line(s)\n  %s\nwant %d (the key step, and the restore AND save keys)", got, want, n)
		}
	}
	at := -1
	for i, l := range lines {
		if l == "restore-keys: |" {
			if at >= 0 {
				t.Fatalf("the `test` job has more than one `restore-keys: |` block")
			}
			at = i
		}
	}
	if at < 0 {
		t.Fatalf("positive control FAILED: no `restore-keys: |` block in the `test` job")
	}
	var got []string
	// Every line of the block, up to the blank line, comment or next step that
	// ends it — not only the `${{` ones, so an added key of any shape counts.
	for _, l := range lines[at+1:] {
		if l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, "- ") {
			break
		}
		got = append(got, l)
	}
	if strings.Join(got, "\n") != strings.Join(wantGoCacheRestoreKeys, "\n") {
		t.Errorf("the `test` job's restore keys are\n  %s\nwant, in this order (image version dropped first, go.sum last)\n  %s",
			strings.Join(got, "\n  "), strings.Join(wantGoCacheRestoreKeys, "\n  "))
	}
}

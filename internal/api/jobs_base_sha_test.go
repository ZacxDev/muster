package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/jobs"
)

// A JOB'S `base_sha` (jobs.go): handed to the worktree (PrepareJobWorktree,
// whose script tests are in internal/agentprovision), read back, refused where
// it means nothing or is not a full id, and compared on a replay.

const testBaseSHA = "abcdefabcdefabcdefabcdefabcdefabcdefabcd"

// TestAJobsBaseSHAReachesTheWorktree: a sha job with base_sha hands it to the
// worktree and reads it back; one without hands none and reads "".
func TestAJobsBaseSHAReachesTheWorktree(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.gw.reply = "done"
	body := map[string]any{"agent": "keen-wren", "prompt": "review", "repo": "acme/widgets", "sha": testFullSHA, "pr": 7}
	with := map[string]any{"base_sha": testBaseSHA}
	for k, v := range body {
		with[k] = v
	}
	if out := f.await(t, f.submit(t, with)); out["status"] != jobs.StatusSucceeded || out["base_sha"] != testBaseSHA {
		t.Fatalf("job = %v (base_sha %v), want succeeded with %s", out["status"], out["base_sha"], testBaseSHA)
	}
	if out := f.await(t, f.submit(t, body)); out["status"] != jobs.StatusSucceeded || out["base_sha"] != "" {
		t.Fatalf("job = %v (base_sha %v), want succeeded with none", out["status"], out["base_sha"])
	}
	made, _ := f.ws.snapshot()
	want := []string{
		"keen-wren#1 acme/widgets#7@" + testFullSHA + " base=" + testBaseSHA,
		"keen-wren#2 acme/widgets#7@" + testFullSHA,
	}
	if strings.Join(made, "|") != strings.Join(want, "|") {
		t.Fatalf("worktrees made = %q, want %q", made, want)
	}
}

// TestBaseSHASubmissionsTheAPIRefuses: abbreviated, upper case, and without a
// sha are 400s recording nothing; a full id on a sha job is accepted (the first
// test).
func TestBaseSHASubmissionsTheAPIRefuses(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	sha := func(base string) map[string]any {
		return map[string]any{"agent": "keen-wren", "prompt": "x", "repo": "acme/widgets", "sha": testFullSHA, "pr": 7, "base_sha": base}
	}
	for _, tc := range []struct {
		name string
		body map[string]any
		want string
	}{
		{"abbreviated", sha(testBaseSHA[:12]), "base_sha must be a full commit id"},
		{"upper case", sha(strings.ToUpper(testBaseSHA)), "base_sha must be a full commit id"},
		{"without a sha", map[string]any{"agent": "keen-wren", "prompt": "x", "base_sha": testBaseSHA}, "base_sha is only for a job with a sha"},
	} {
		code, out := f.call(t, http.MethodPost, "/api/jobs", tc.body, true)
		if msg, _ := out["error"].(string); code != http.StatusBadRequest || !strings.HasPrefix(msg, tc.want) {
			t.Errorf("%s: POST = %d %v, want 400 starting %q", tc.name, code, out, tc.want)
		}
	}
	if n := f.store.count(); n != 0 {
		t.Fatalf("refused submissions recorded %d job(s)", n)
	}
}

// TestAReplayWithADifferentBaseSHAIsRefused: a key replayed without the base
// it was first sent with is a 409 naming base_sha; the same body is the
// deduplicated job.
func TestAReplayWithADifferentBaseSHAIsRefused(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.gw.reply = "done"
	first := map[string]any{"agent": "keen-wren", "prompt": "review", "repo": "acme/widgets", "sha": testFullSHA, "pr": 7,
		"base_sha": testBaseSHA, "idempotency_key": "review-base"}
	id := f.submit(t, first)
	f.await(t, id)
	again := map[string]any{}
	for k, v := range first {
		again[k] = v
	}
	delete(again, "base_sha")
	code, out := f.call(t, http.MethodPost, "/api/jobs", again, true)
	if msg, _ := out["error"].(string); code != http.StatusConflict || !strings.Contains(msg, `has base_sha "`+testBaseSHA+`", not ""`) {
		t.Fatalf("replay without it = %d %v, want 409 naming base_sha", code, out)
	}
	if code, out := f.call(t, http.MethodPost, "/api/jobs", first, true); code != http.StatusOK || out["deduplicated"] != true {
		t.Fatalf("the same replay = %d %v, want 200 deduplicated", code, out)
	}
}

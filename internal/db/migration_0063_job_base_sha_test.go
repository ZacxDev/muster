package db_test

import (
	"strings"
	"testing"
)

// TestMigration0063AddsBaseSHA: a job written at version 62 reads an empty
// base_sha after 0063; base_sha is refused without a sha, abbreviated, or in upper case
// (jobs_base_sha_check), each refusal with a control one value away. And 0063
// adds nothing else: no repo_config_overlay column (it was per job before it
// became the pool's, and was dropped before 0063 was applied anywhere).
func TestMigration0063AddsBaseSHA(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0063_base_sha", 62)
	agent := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace) VALUES (1, 'base-agent', 'ns-base') RETURNING id`)
	const sha = "0123456789abcdef0123456789abcdef01234567"
	const base = "fedcba9876543210fedcba9876543210fedcba98"
	old := scanID(ctx, t, pool, `INSERT INTO jobs (owner_id, agent_id, agent_name, prompt, repo, sha, pr, timeout_s)
		VALUES (1, $1, 'base-agent', 'review', 'acme/widgets', $2, 7, 60) RETURNING id`, agent, sha)

	applyThroughFrom(ctx, t, pool, 63)

	var got string
	if err := pool.QueryRow(ctx, `SELECT base_sha FROM jobs WHERE id = $1`, old).Scan(&got); err != nil || got != "" {
		t.Fatalf("a job from before 0063: base_sha %q (%v); want ''", got, err)
	}
	withBase := func(sha, base string) error {
		pr := 0
		if sha != "" {
			pr = 7
		}
		_, err := pool.Exec(ctx, `INSERT INTO jobs (owner_id, agent_id, agent_name, prompt, repo, sha, pr, timeout_s, base_sha)
			VALUES (1, $1, 'base-agent', 'review', 'acme/widgets', $2, $3, 60, $4)`, agent, sha, pr, base)
		return err
	}
	for name, err := range map[string]error{
		"base_sha without a sha": withBase("", base),
		"base_sha abbreviated":   withBase(sha, base[:12]),
		"base_sha in upper case": withBase(sha, strings.ToUpper(base)),
	} {
		if err == nil || !strings.Contains(err.Error(), "jobs_base_sha_check") {
			t.Errorf("%s: %v, want jobs_base_sha_check to refuse it", name, err)
		}
	}
	if err := withBase(sha, base); err != nil {
		t.Errorf("control: a sha job with a full base_sha: %v", err)
	}
	if err := withBase("", ""); err != nil {
		t.Errorf("control: a job without a sha or a base: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'jobs' AND column_name = 'repo_config_overlay'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("jobs.repo_config_overlay: %d column(s) (%v), want none", n, err)
	}
}

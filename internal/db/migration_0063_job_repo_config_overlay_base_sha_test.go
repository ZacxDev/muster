package db_test

import (
	"strings"
	"testing"
)

// TestMigration0063AddsRepoConfigOverlayAndBaseSHA: a job written at version
// 62 reads repo_config_overlay true after 0063; a sha job may then be stored
// with false, while false without a sha, or with a config_ref, is refused by
// jobs_repo_config_overlay_check; base_sha reads ” on the old row, and is
// refused without a sha or when not a full lower-case id (jobs_base_sha_check).
// Each refusal has a control one value away.
func TestMigration0063AddsRepoConfigOverlayAndBaseSHA(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0063_repo_config_overlay", 62)
	agent := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace) VALUES (1, 'overlay-agent', 'ns-ov') RETURNING id`)
	const sha = "0123456789abcdef0123456789abcdef01234567"
	const cfg = "fedcba9876543210fedcba9876543210fedcba98"
	old := scanID(ctx, t, pool, `INSERT INTO jobs (owner_id, agent_id, agent_name, prompt, repo, sha, pr, timeout_s)
		VALUES (1, $1, 'overlay-agent', 'review', 'acme/widgets', $2, 7, 60) RETURNING id`, agent, sha)

	applyThroughFrom(ctx, t, pool, 63)

	var overlay bool
	if err := pool.QueryRow(ctx, `SELECT repo_config_overlay FROM jobs WHERE id = $1`, old).Scan(&overlay); err != nil || !overlay {
		t.Fatalf("a job from before 0063: repo_config_overlay %v (%v); want true", overlay, err)
	}
	insert := func(sha, cfg string, overlay bool) error {
		pr := 0
		if sha != "" {
			pr = 7
		}
		_, err := pool.Exec(ctx, `INSERT INTO jobs (owner_id, agent_id, agent_name, prompt, repo, sha, pr, timeout_s, config_ref, repo_config_overlay)
			VALUES (1, $1, 'overlay-agent', 'review', 'acme/widgets', $2, $3, 60, $4, $5)`,
			agent, sha, pr, cfg, overlay)
		return err
	}
	if err := insert(sha, "", false); err != nil {
		t.Fatalf("a sha job with repo_config_overlay false: %v", err)
	}
	for name, err := range map[string]error{
		"false without a sha":     insert("", "", false),
		"false with a config_ref": insert(sha, cfg, false),
	} {
		if err == nil || !strings.Contains(err.Error(), "jobs_repo_config_overlay_check") {
			t.Errorf("%s: %v, want jobs_repo_config_overlay_check to refuse it", name, err)
		}
	}
	// Controls: the same rows with the overlay on.
	if err := insert("", "", true); err != nil {
		t.Errorf("control: a job without a sha, overlay on: %v", err)
	}
	if err := insert(sha, cfg, true); err != nil {
		t.Errorf("control: a sha job with a config_ref, overlay on: %v", err)
	}

	// base_sha: '' on the old row; a full id on a sha job only.
	var base string
	if err := pool.QueryRow(ctx, `SELECT base_sha FROM jobs WHERE id = $1`, old).Scan(&base); err != nil || base != "" {
		t.Fatalf("a job from before 0063: base_sha %q (%v); want ''", base, err)
	}
	withBase := func(sha, base string) error {
		pr := 0
		if sha != "" {
			pr = 7
		}
		_, err := pool.Exec(ctx, `INSERT INTO jobs (owner_id, agent_id, agent_name, prompt, repo, sha, pr, timeout_s, base_sha)
			VALUES (1, $1, 'overlay-agent', 'review', 'acme/widgets', $2, $3, 60, $4)`, agent, sha, pr, base)
		return err
	}
	for name, err := range map[string]error{
		"base_sha without a sha": withBase("", cfg),
		"base_sha abbreviated":   withBase(sha, cfg[:12]),
		"base_sha in upper case": withBase(sha, strings.ToUpper(cfg)),
	} {
		if err == nil || !strings.Contains(err.Error(), "jobs_base_sha_check") {
			t.Errorf("%s: %v, want jobs_base_sha_check to refuse it", name, err)
		}
	}
	if err := withBase(sha, cfg); err != nil {
		t.Errorf("control: a sha job with a full base_sha: %v", err)
	}
}

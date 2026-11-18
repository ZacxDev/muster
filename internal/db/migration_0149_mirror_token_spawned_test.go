package db_test

import (
	"testing"
)

// TestMigration0149BackfillsSpawningHostTokens: 0149 adds
// mirror_tokens.spawned_at and marks, at once, every LIVE token whose
// (owner, host) already has a host dispatch row or an officer host spawn —
// and nothing else: not a clean host's token, not another owner's token for
// a host of the same name, not a revoked token.
func TestMigration0149BackfillsSpawningHostTokens(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0149_token_spawned", 148)
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES (2, 'member') ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	tok := func(owner int64, host, hash string, revoked bool) int64 {
		return scanID(ctx, t, pool, `INSERT INTO mirror_tokens (owner_id, host, token_hash, revoked_at)
			VALUES ($1, $2, $3, CASE WHEN $4 THEN now() END) RETURNING id`, owner, host, hash, revoked)
	}
	dispatched := tok(1, "disp-one", "hash-disp", false)
	officer := tok(1, "officer-one", "hash-officer", false)
	clean := tok(1, "clean-one", "hash-clean", false)
	otherOwner := tok(2, "disp-one", "hash-other-owner", false)
	revoked := tok(1, "disp-one", "hash-revoked", true)
	if _, err := pool.Exec(ctx, `INSERT INTO host_dispatches (owner_id, host, runtime, repo, reservation_id)
		VALUES (1, 'disp-one', 'claude', 'acme/widgets', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO officer_host_spawns (owner_id, officer, instance, host)
		VALUES (1, 'marketing:example', 'officer-marketing-example-1', 'officer-one')`); err != nil {
		t.Fatal(err)
	}

	applyThroughFrom(ctx, t, pool, 149)

	for _, c := range []struct {
		name string
		id   int64
		want bool
	}{
		{"a live token whose host has a host dispatch", dispatched, true},
		{"a live token whose host has an officer spawn", officer, true},
		{"a clean host's token", clean, false},
		{"another owner's token for a host of the same name", otherOwner, false},
		{"a revoked token", revoked, false},
	} {
		var marked bool
		if err := pool.QueryRow(ctx, `SELECT spawned_at IS NOT NULL FROM mirror_tokens WHERE id = $1`, c.id).Scan(&marked); err != nil {
			t.Fatal(err)
		}
		if marked != c.want {
			t.Errorf("%s: marked %v, want %v", c.name, marked, c.want)
		}
	}
}

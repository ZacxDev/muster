package db_test

import (
	"testing"

	"github.com/ZacxDev/muster/internal/db"
)

// agentSecretsVersion is 0128_agent_secrets.sql's number.
// 🔴 RENUMBER IT WITH THE FILE (and db.agentSecretsMigrationVersion) at merge.
const agentSecretsVersion = 128

// TestMigration0128AgentSecrets: on a populated database, 0128 moves every
// agent's credentials into agent_secrets under the agent's own owner, leaves an
// agent with none without a row, keeps the old columns as they were (an older
// image reads them), and refuses a credentials row whose owner is not its
// agent's. Re-applying the body — what db.RLSOn does — fills only what a row
// lacks: it never overwrites a credential agent_secrets already holds, and it
// picks up a token an older image wrote to the old column after 0128 ran.
func TestMigration0128AgentSecrets(t *testing.T) {
	ctx := ctx0006(t)
	// The migration just below 0128 that this tree embeds (the number is a
	// placeholder until merge, so the one below may not be 0125 yet).
	ms, err := db.EmbeddedMigrationsForTest()
	if err != nil {
		t.Fatal(err)
	}
	prev := 0
	for _, m := range ms {
		if m.Version < agentSecretsVersion && m.Version > prev {
			prev = m.Version
		}
	}
	pool := freshAt(ctx, t, "ms_0128_agent_secrets", prev)
	if _, err := pool.Exec(ctx, `SELECT 1 FROM agent_secrets`); err == nil {
		t.Fatal("control: before 0128 agent_secrets exists")
	}
	two := scanID(ctx, t, pool, `INSERT INTO users (role) VALUES ('member') RETURNING id`)
	both := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace, hooks_token, gateway_secret)
		VALUES (1, 'sec-both', 'ns-sec-both', 'tok-both', 'gw-both') RETURNING id`)
	none := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace) VALUES (1, 'sec-none', 'ns-sec-none') RETURNING id`)
	other := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace, hooks_token)
		VALUES ($1, 'sec-other', 'ns-sec-other', 'tok-other') RETURNING id`, two)

	applyThroughFrom(ctx, t, pool, agentSecretsVersion)

	type row struct {
		tok, gw string
		owner   int64
	}
	read := func(id int64) (row, bool) {
		var r row
		err := pool.QueryRow(ctx, `SELECT hooks_token, gateway_secret, owner_id FROM agent_secrets WHERE agent_id=$1`, id).
			Scan(&r.tok, &r.gw, &r.owner)
		return r, err == nil
	}
	for _, c := range []struct {
		id   int64
		want row
	}{{both, row{"tok-both", "gw-both", 1}}, {other, row{"tok-other", "", two}}} {
		if got, ok := read(c.id); !ok || got != c.want {
			t.Errorf("agent %d after 0128: agent_secrets %+v (row %v), want %+v", c.id, got, ok, c.want)
		}
	}
	if _, ok := read(none); ok {
		t.Errorf("an agent with no credential got an agent_secrets row")
	}
	var oldTok string
	if err := pool.QueryRow(ctx, `SELECT hooks_token FROM agents WHERE id=$1`, both).Scan(&oldTok); err != nil || oldTok != "tok-both" {
		t.Errorf("0128 changed the old column an older image reads: %q (err %v)", oldTok, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agent_secrets (agent_id, owner_id, hooks_token) VALUES ($1, $2, 'tok-cross')`, none, two); err == nil {
		t.Errorf("accepted a credentials row whose owner is not its agent's")
	} else {
		fkViolation(t, err, "agent_secrets_agent_owner_fkey")
	}

	// The re-apply: one value changed in agent_secrets (it must survive), one
	// token written only to the old column, as an older image would (it must
	// be picked up).
	// Both credentials now differ between agent_secrets and the old columns,
	// so a backfill that preferred the old column for EITHER would show here.
	mustExec(ctx, t, pool, `UPDATE agent_secrets SET gateway_secret='gw-rotated', hooks_token='tok-rotated' WHERE agent_id=$1`, both)
	mustExec(ctx, t, pool, `UPDATE agents SET hooks_token='tok-old-image' WHERE id=$1`, none)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, readMigration(t, "0128_agent_secrets.sql")); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("re-applying 0128: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := read(both); got.gw != "gw-rotated" || got.tok != "tok-rotated" {
		t.Errorf("re-applying 0128 overwrote a credential agent_secrets held: %+v", got)
	}
	if got, ok := read(none); !ok || got.tok != "tok-old-image" {
		t.Errorf("re-applying 0128 did not pick up a token an older image wrote: %+v (row %v)", got, ok)
	}

	// --rls-off then --rls-on: the table is turned off with the rest, and
	// back on, forced, with its policy.
	if err := db.RLSOff(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var on, forced bool
	if err := pool.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid='public.agent_secrets'::regclass`).
		Scan(&on, &forced); err != nil || on || forced {
		t.Fatalf("after RLSOff agent_secrets has rls=%v forced=%v (err %v), want both off", on, forced, err)
	}
	if err := db.RLSOn(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid='public.agent_secrets'::regclass`).
		Scan(&on, &forced); err != nil || !on || !forced {
		t.Fatalf("after RLSOn agent_secrets has rls=%v forced=%v (err %v), want both on", on, forced, err)
	}
	if got, _ := read(both); got.gw != "gw-rotated" || got.tok != "tok-rotated" {
		t.Errorf("RLSOn's re-apply overwrote a credential agent_secrets held: %+v", got)
	}
}

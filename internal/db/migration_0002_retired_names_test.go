package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Migration 0002's guards — the DATABASE half of the agent-name tombstone.
//
// 🔴 WHAT MAKES THESE DIFFERENT FROM THE PGStore GUARDS IN internal/agents.
// Those prove the GENERATOR refuses a retired name when the application deletes
// an agent through PGStore.Delete. The property THIS file pins is the one the
// trigger was chosen for over an application-level insert: the tombstone appears
// for a DELETE that never touches Go at all. An insert in Provisioner.Destroy
// would be a rule every future call site has to remember, and would cover only
// the binary that has it; a trigger has nothing to remember. A test that deletes
// through the store cannot tell the two designs apart — so these delete with raw
// SQL.
//
// ⚠ They run against this package's private, fully-migrated database (dbtest
// copies an already-migrated template), i.e. the schema the whole 0001..0002
// chain actually produced — not a scratch schema built from 0002's body alone. A
// scratch-schema fixture could not see a trigger that failed to attach to the
// real `agents`.

// pinnedConn takes ONE connection out of the pool and holds it for the test.
//
// 🔴 A POOL IS NOT A SESSION, AND THE search_path GUARD BELOW IS A CLAIM ABOUT A
// SESSION. `SET search_path` applies to the connection it runs on; issued
// through a pool, the next statement can land on a different connection and the
// hijack the test believes it installed is simply not in effect. The guard has
// its own control for that, but pinning the connection is what makes the control
// pass for the right reason.
func pinnedConn(t *testing.T) (context.Context, *pgxpool.Conn) {
	t.Helper()
	ctx, pool := migrated(t)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a pinned connection: %v", err)
	}
	t.Cleanup(conn.Release)
	return ctx, conn
}

// insertAgentRow inserts a minimal `agents` row with raw SQL and returns its id.
// It deliberately does NOT go through internal/agents: this file's claim is about
// the database, and importing the store would make internal/db depend on it.
func insertAgentRow(ctx context.Context, t *testing.T, conn *pgxpool.Conn, name string) int64 {
	t.Helper()
	var id int64
	if err := conn.QueryRow(ctx,
		`INSERT INTO agents (name, namespace, status) VALUES ($1, $2, 'provisioning') RETURNING id`,
		name, "devpod-"+name).Scan(&id); err != nil {
		t.Fatalf("insert agents row %q: %v", name, err)
	}
	return id
}

// clearName removes every trace of a name so an assertion cannot be answered by
// a leftover from an earlier run.
//
// 🔴 THE ORDER MATTERS AND THE SECOND TOMBSTONE DELETE IS NOT A TYPO. Clearing
// the `agents` row FIRES THE TRIGGER, which writes the very tombstone the caller
// is trying to clear. Tombstone, agent, tombstone again.
func clearName(ctx context.Context, t *testing.T, conn *pgxpool.Conn, name string) {
	t.Helper()
	for _, stmt := range []string{
		`DELETE FROM public.agent_retired_names WHERE name=$1`,
		`DELETE FROM public.agents WHERE name=$1`,
		`DELETE FROM public.agent_retired_names WHERE name=$1`,
	} {
		if _, err := conn.Exec(ctx, stmt, name); err != nil {
			t.Fatalf("clear %q with %q: %v", name, stmt, err)
		}
	}
}

// TestRetiredNameTriggerIsRegisteredOnAgents is the STRUCTURAL half of the
// closing condition: pg_trigger holds exactly one non-internal trigger on
// `agents`, it is AFTER DELETE FOR EACH ROW, and the function it runs writes to
// the retired-names table.
//
// 🔴 IT IS A LEDGER OF THE WHOLE SET, NOT A SPOT CHECK, so it reddens when the
// set GROWS as well as when it shrinks. A second trigger on `agents` is exactly
// the kind of thing that changes what a DELETE means without any test noticing.
//
// ⚠ AND IT IS HALF A GUARD ON ITS OWN, WHICH IS WHY IT IS NAMED AS THE
// STRUCTURAL HALF. A trigger can be registered with all the right bits and still
// not tombstone anything — the function body is checked here only for the table
// NAME, which is a check on spelling. TestRetiredNameIsTombstonedByARawSQLDelete
// is the behavioural half; neither is sufficient alone.
func TestRetiredNameTriggerIsRegisteredOnAgents(t *testing.T) {
	ctx, conn := pinnedConn(t)

	rows, err := conn.Query(ctx, `
		SELECT t.tgname, t.tgtype, p.proname, pg_get_functiondef(p.oid)
		  FROM pg_trigger t
		  JOIN pg_class c     ON c.oid = t.tgrelid
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		  JOIN pg_proc p      ON p.oid = t.tgfoid
		 WHERE NOT t.tgisinternal
		   AND n.nspname = 'public'
		   AND c.relname = 'agents'
		 ORDER BY t.tgname`)
	if err != nil {
		t.Fatalf("read pg_trigger: %v", err)
	}
	defer rows.Close()

	type trig struct {
		name   string
		tgtype int16
		fn     string
		fnBody string
	}
	var got []trig
	for rows.Next() {
		var tr trig
		if err := rows.Scan(&tr.name, &tr.tgtype, &tr.fn, &tr.fnBody); err != nil {
			t.Fatalf("scan pg_trigger row: %v", err)
		}
		got = append(got, tr)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("pg_trigger rows: %v", err)
	}

	if len(got) != 1 {
		var names []string
		for _, tr := range got {
			names = append(names, tr.name)
		}
		t.Fatalf("`agents` carries %d non-internal trigger(s) %v, want exactly 1 "+
			"(agents_retire_name). A missing one means a destroyed agent's name goes "+
			"straight back into the generator pool; an extra one changes what a DELETE "+
			"does with nothing else in the suite watching.", len(got), names)
	}
	tr := got[0]

	if tr.name != "agents_retire_name" {
		t.Errorf("the trigger on `agents` is named %q, want %q", tr.name, "agents_retire_name")
	}

	// pg_trigger.tgtype bit layout (src/include/catalog/pg_trigger.h):
	//   1<<0 ROW   1<<1 BEFORE   1<<2 INSERT   1<<3 DELETE
	//   1<<4 UPDATE   1<<5 TRUNCATE   1<<6 INSTEAD
	// AFTER is the absence of BEFORE and of INSTEAD.
	const (
		bitRow     = int16(1 << 0)
		bitBefore  = int16(1 << 1)
		bitDelete  = int16(1 << 3)
		bitInstead = int16(1 << 6)
	)
	if tr.tgtype&bitRow == 0 {
		t.Errorf("%s is a STATEMENT-level trigger (tgtype=%d), want FOR EACH ROW. A statement "+
			"trigger has no OLD row, so it cannot know which name was released — a DELETE "+
			"removing three agents would tombstone none of them.", tr.name, tr.tgtype)
	}
	if tr.tgtype&bitDelete == 0 {
		t.Errorf("%s does not fire on DELETE (tgtype=%d); DELETE is the only event that "+
			"releases a name back into the pool.", tr.name, tr.tgtype)
	}
	if tr.tgtype&bitBefore != 0 {
		t.Errorf("%s is a BEFORE trigger (tgtype=%d), want AFTER. BEFORE DELETE runs while the "+
			"row may still be skipped by another BEFORE trigger returning NULL, so it can "+
			"tombstone a name that was never actually released.", tr.name, tr.tgtype)
	}
	if tr.tgtype&bitInstead != 0 {
		t.Errorf("%s is an INSTEAD OF trigger (tgtype=%d), want AFTER — INSTEAD OF replaces the "+
			"delete entirely.", tr.name, tr.tgtype)
	}

	if tr.fn != "muster_retire_agent_name" {
		t.Errorf("%s executes %q, want %q", tr.name, tr.fn, "muster_retire_agent_name")
	}
	// A spelling check, and labelled as one: it says the function TALKS ABOUT the
	// ledger, not that it writes to it correctly. The behavioural guard below is
	// what pins the write.
	if !strings.Contains(tr.fnBody, "agent_retired_names") {
		t.Errorf("%s's function body never mentions agent_retired_names:\n%s", tr.fn, tr.fnBody)
	}
}

// TestRetiredNameIsTombstonedByARawSQLDelete is the call-site-independence
// claim: a DELETE issued by something that has never heard of muster's Go code —
// a psql prompt, a future binary, a cascade from another table — still retires
// the name. This is the BEHAVIOURAL half of the closing condition's first limb.
func TestRetiredNameIsTombstonedByARawSQLDelete(t *testing.T) {
	ctx, conn := pinnedConn(t)

	const name = "zzraw-sqldelete"
	clearName(ctx, t, conn, name)

	var before int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM public.agent_retired_names WHERE name=$1`, name).Scan(&before); err != nil {
		t.Fatalf("count tombstones before: %v", err)
	}
	if before != 0 {
		t.Fatalf("%q is already tombstoned before the test acts (%d rows); the assertion below "+
			"would pass whether or not the trigger fires", name, before)
	}

	id := insertAgentRow(ctx, t, conn, name)

	// The DELETE under test. No Go store, no application purge — just SQL.
	tag, err := conn.Exec(ctx, `DELETE FROM public.agents WHERE id=$1`, id)
	if err != nil {
		t.Fatalf("raw DELETE of agent %d: %v", id, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("raw DELETE removed %d rows, want 1; nothing was deleted so the trigger had no "+
			"reason to fire", tag.RowsAffected())
	}

	var after int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM public.agent_retired_names WHERE name=$1`, name).Scan(&after); err != nil {
		t.Fatalf("count tombstones after: %v", err)
	}
	if after != 1 {
		t.Fatalf("agent_retired_names holds %d rows for %q after a raw SQL DELETE, want 1. The "+
			"name is back in the generator pool (internal/agents/names.go), so the next "+
			"generated agent can be its namesake and inherit the ServiceAccount and "+
			"cluster-scoped ClusterRoleBinding named after the dead one. An application-level "+
			"insert in Provisioner.Destroy would NOT have covered this delete — that is why "+
			"0002 is a trigger.", after, name)
	}

	// Re-deleting the same name must not error: the trigger's ON CONFLICT DO
	// NOTHING is what makes the ledger idempotent, and a name genuinely can be
	// inserted and deleted more than once (see the next test).
	id2 := insertAgentRow(ctx, t, conn, name)
	if _, err := conn.Exec(ctx, `DELETE FROM public.agents WHERE id=$1`, id2); err != nil {
		t.Fatalf("second DELETE of the same name failed: %v — the trigger's INSERT is not "+
			"conflict-tolerant, so destroying a re-provisioned agent would fail outright", err)
	}
}

// TestRetiredNameTombstoneIsSearchPathProof replays the hijack an unqualified
// INSERT in the trigger function is vulnerable to.
//
// 🔴 WHAT IT PINS, AND WHY A STRUCTURAL CHECK WOULD NOT DO. The trigger function
// is SECURITY INVOKER, so an UNQUALIFIED `INSERT INTO agent_retired_names`
// resolves through the CALLING SESSION's search_path. With a decoy table first
// on the path, a `DELETE FROM public.agents` SUCCEEDS and writes the tombstone
// into the decoy — public gets 0 rows, so NameExists never sees it and the name
// goes back into the pool. Silent. That defeats exactly the property 0002 is
// sold on ("any call site, any binary, including a hand-run psql DELETE") — and
// a hand-run psql session is where a non-default search_path comes from.
//
// Asserting `proconfig`, or grepping the function body for "public.", would be a
// check on the SPELLING of the fix. This asserts the OUTCOME, so it holds for
// either spelling and for any third one someone substitutes later.
//
// ⚠ WHAT IT THEREFORE DOES NOT COVER: removing only the `SET search_path` line
// while keeping the `public.` qualification leaves this test green. With nothing
// unqualified in the function body there is no behaviour to observe, so the
// `SET` is unpinned defence-in-depth. 0002's header says why that is accepted
// rather than closed with a proconfig assertion.
//
// The two assertions are a pair on purpose: `shadow == 0` alone is satisfied by
// a trigger that never fires at all, so `public == 1` is the positive control
// that makes the zero mean something.
func TestRetiredNameTombstoneIsSearchPathProof(t *testing.T) {
	ctx, conn := pinnedConn(t)

	const name = "zzsearchpath-hijack"
	const shadowSchema = "zz_shadow_retired_names"

	clearName(ctx, t, conn, name)
	for _, stmt := range []string{
		`DROP SCHEMA IF EXISTS ` + shadowSchema + ` CASCADE`,
		`CREATE SCHEMA ` + shadowSchema,
		`CREATE TABLE ` + shadowSchema + `.agent_retired_names (
			name TEXT PRIMARY KEY, retired_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("fixture %q: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		if _, err := conn.Exec(bg, `RESET search_path`); err != nil {
			t.Errorf("cleanup RESET search_path: %v", err)
		}
		if _, err := conn.Exec(bg, `DROP SCHEMA IF EXISTS `+shadowSchema+` CASCADE`); err != nil {
			t.Errorf("cleanup DROP SCHEMA: %v", err)
		}
	})

	// The decoy is FIRST on the path — the shape a psql session, a restore script
	// or a multi-schema layout produces.
	if _, err := conn.Exec(ctx, `SET search_path TO `+shadowSchema+`, public`); err != nil {
		t.Fatalf("set the hijacked search_path: %v", err)
	}
	// Control for the fixture: prove the hijack is actually in effect, or the whole
	// test is a re-run of TestRetiredNameIsTombstonedByARawSQLDelete.
	var resolved string
	if err := conn.QueryRow(ctx,
		`SELECT n.nspname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		  WHERE c.oid = 'agent_retired_names'::regclass`).Scan(&resolved); err != nil {
		t.Fatalf("resolve the unqualified name under the hijacked path: %v", err)
	}
	if resolved != shadowSchema {
		t.Fatalf("unqualified `agent_retired_names` resolves to %q, want %q — the search_path "+
			"hijack is not in effect and this test proves nothing", resolved, shadowSchema)
	}

	id := insertAgentRow(ctx, t, conn, name)
	if _, err := conn.Exec(ctx, `DELETE FROM public.agents WHERE id=$1`, id); err != nil {
		t.Fatalf("delete the agent under the hijacked search_path: %v", err)
	}

	var inPublic, inShadow int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM public.agent_retired_names WHERE name=$1`, name).Scan(&inPublic); err != nil {
		t.Fatalf("count public tombstones: %v", err)
	}
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM `+shadowSchema+`.agent_retired_names WHERE name=$1`, name).Scan(&inShadow); err != nil {
		t.Fatalf("count shadow tombstones: %v", err)
	}

	if inPublic != 1 {
		t.Errorf("public.agent_retired_names holds %d rows for %q after the DELETE, want 1. The "+
			"trigger resolved its target through the CALLER's search_path, so the tombstone did "+
			"not land where PGStore.NameExists reads it — the name is back in the generator pool "+
			"and nothing reported a problem.", inPublic, name)
	}
	if inShadow != 0 {
		t.Errorf("%s.agent_retired_names holds %d rows for %q, want 0. The tombstone was written "+
			"into a schema the caller controls: any session that can create a schema can redirect "+
			"the ledger and silently free a retired name.", shadowSchema, inShadow, name)
	}
}

// TestRetiredNameStillAllowsInsertingARetiredName pins the decision NOT to
// refuse the insert.
//
// 🔴 IT IS A DECISION, NOT AN OMISSION, and it is the reason 0002 guards the
// GENERATOR rather than the table. The reserved `chief` agent is provisioned
// with a hand-set name (internal/api/chief_provision.go), so a BEFORE INSERT
// refusal on a retired name would make destroying and re-provisioning chief
// impossible with no override — an outage in exchange for a property the
// generator already has. If someone later "hardens" 0002 into a refusal, this
// test is what tells them what they broke.
//
// ⚠ IT IS AN INVARIANT GUARD, NOT REGRESSION COVERAGE: no bug ever made this
// insert fail. It is here to make the scope of 0002 machine-readable.
func TestRetiredNameStillAllowsInsertingARetiredName(t *testing.T) {
	ctx, conn := pinnedConn(t)

	const name = "zzreissued-byhand"
	clearName(ctx, t, conn, name)
	if _, err := conn.Exec(ctx,
		`INSERT INTO public.agent_retired_names (name) VALUES ($1) ON CONFLICT (name) DO NOTHING`,
		name); err != nil {
		t.Fatalf("seed the tombstone: %v", err)
	}

	// The insert the refusal would have blocked.
	id := insertAgentRow(ctx, t, conn, name)
	t.Cleanup(func() {
		if _, err := conn.Exec(context.Background(), `DELETE FROM public.agents WHERE id=$1`, id); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	var live string
	if err := conn.QueryRow(ctx, `SELECT name FROM public.agents WHERE id=$1`, id).Scan(&live); err != nil {
		t.Fatalf("read back the re-inserted agent: %v", err)
	}
	if live != name {
		t.Errorf("re-inserted agent name = %q, want %q", live, name)
	}
}

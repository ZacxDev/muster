package agents

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// The generator half of the agent-name tombstone: a destroyed agent's name must
// never be handed to a new agent.
//
// 🔴 WHY THESE ARE POSTGRES-BACKED AND NOT FAKE-STORE TESTS. The whole change
// lives in the SQL — PGStore.NameExists's second EXISTS and migration 0002's
// AFTER DELETE trigger. A fake store decides for itself what NameExists returns,
// so a fake-store version of the assertion below is GREEN against the broken
// code: it would be testing the test's own stub. The only fixture that can tell
// the fix from its absence is a real database that really deletes a row.
//
// 🔴 AND THE POOL IS NARROWED TO A SINGLE NAME SO THE ASSERTION IS REACHED. With
// the real 16x16 pool, "BuildUniqueAgentName did not return the retired name" is
// a 255/256 coin flip per draw and 10 draws make it a near-certainty — the test
// would pass against the bug ~96% of the time and record a KILL for the wrong
// reason under mutation. The word lists are package-level vars, so narrowing
// them to one adjective and one noun makes the generator's output the retired
// name or nothing — and the control assertion on generateName() proves the
// narrowing took effect rather than assuming it.

// narrowNamePool pins generateName() to exactly one output for the duration of a
// test and returns it. Restored by t.Cleanup.
//
// ⚠ IT MUTATES PACKAGE STATE, so a test using it must not call t.Parallel().
// None here does.
func narrowNamePool(t *testing.T, adj, noun string) string {
	t.Helper()
	prevAdj, prevNouns := adjectives, nouns
	adjectives = []string{adj}
	nouns = []string{noun}
	t.Cleanup(func() { adjectives, nouns = prevAdj, prevNouns })
	return adj + "-" + noun
}

// retiredNameTestStore opens this package's private Postgres, applies the
// migrations and returns a PGStore over it.
func retiredNameTestStore(t *testing.T) (context.Context, *PGStore) {
	t.Helper()
	dsn := dbtest.DSN(t) // skips, or fails, per MUSTER_TEST_REQUIRE_DB

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return ctx, NewPG(pool)
}

// TestBuildUniqueAgentNameNeverReissuesADestroyedAgentsName is the closing
// condition's second limb: create an agent, record its name, delete it, and
// assert the generator cannot hand that name to anyone else.
//
// The four assertions are deliberately ordered cheapest-discriminator-first: the
// narrowing control, then that the row really is gone (so the live-rows half of
// the predicate cannot be what is answering), then the predicate itself, then
// the behaviour callers depend on.
func TestBuildUniqueAgentNameNeverReissuesADestroyedAgentsName(t *testing.T) {
	ctx, store := retiredNameTestStore(t)

	want := narrowNamePool(t, "zzretired", "tombstone")
	if got := generateName(); got != want {
		t.Fatalf("narrowNamePool did not take effect: generateName()=%q, want %q — every "+
			"assertion below would be about the wrong name", got, want)
	}

	// This package's private database is REUSED across runs, so a tombstone left
	// by an earlier run of THIS test would make every assertion below pass
	// without the delete under test having done anything.
	//
	// 🔴 ORDER: the agents row FIRST, the tombstone SECOND. Clearing the row fires
	// the trigger, which writes the very tombstone this is trying to remove.
	for _, stmt := range []string{
		`DELETE FROM public.agents WHERE name=$1`,
		`DELETE FROM public.agent_retired_names WHERE name=$1`,
	} {
		if _, err := store.pool.Exec(ctx, stmt, want); err != nil {
			t.Fatalf("clear leftovers for %q with %q: %v", want, stmt, err)
		}
	}
	if free, err := store.NameExists(ctx, want); err != nil {
		t.Fatalf("NameExists(%q) during setup: %v", want, err)
	} else if free {
		t.Fatalf("%q is already taken before the test creates anything; the assertions below "+
			"would pass whether or not the delete retires it", want)
	}

	created, err := store.Create(ctx, Agent{
		Name:      want,
		Namespace: NamespaceFor(want),
		Status:    StatusProvisioning,
	})
	if err != nil {
		t.Fatalf("create the agent whose name will be retired: %v", err)
	}
	if err := store.Delete(ctx, created.ID); err != nil {
		t.Fatalf("delete agent %d: %v", created.ID, err)
	}

	// The row is really gone — so nothing below can be answered by the live-rows
	// half of NameExists. This is the negative control for the fixture: if Delete
	// were a soft delete the rest of the test would prove nothing about tombstones.
	if _, err := store.GetByName(ctx, want); err == nil {
		t.Fatalf("GetByName(%q) still returns a row after Delete; this fixture cannot "+
			"distinguish a tombstone from a surviving row", want)
	}

	// The mechanism: the name is TAKEN even though no agent holds it.
	exists, err := store.NameExists(ctx, want)
	if err != nil {
		t.Fatalf("NameExists(%q): %v", want, err)
	}
	// t.Errorf, NOT t.Fatalf, and deliberately: a Fatal here would stop the
	// BEHAVIOURAL assertion below from ever executing against broken code, so that
	// assertion could never be watched to fail and would survive mutation testing
	// while proving nothing.
	if !exists {
		t.Errorf("NameExists(%q)=false after the agent holding it was DELETED; the name is back "+
			"in the pool, so a namesake gets the same ServiceAccount and inherits the "+
			"cluster-scoped ClusterRoleBinding named after the dead agent "+
			"(internal/provision/k8s/driver.go)", want)
	}

	// The behaviour: with the pool narrowed to this one name, BuildUniqueAgentName
	// must fail loudly rather than reissue it.
	got, err := BuildUniqueAgentName(ctx, store)
	if err == nil {
		t.Fatalf("BuildUniqueAgentName returned %q — the name of an agent that was DELETED. It "+
			"must exhaust its attempts and error instead of reissuing a retired name", got)
	}
	if got != "" {
		t.Errorf("BuildUniqueAgentName returned both a name (%q) and an error (%v); a caller that "+
			"ignores the error would provision the namesake", got, err)
	}
}

// TestNameExistsTreatsARetiredNameAsTaken isolates the PREDICATE from the
// trigger: a name present ONLY in agent_retired_names — no agents row now and
// none in this test's history — must read as taken, and a name in neither table
// must read as free.
//
// The second half is the positive control for the instrument: without it, a
// NameExists hardwired to `return true, nil` would satisfy the first half.
func TestNameExistsTreatsARetiredNameAsTaken(t *testing.T) {
	ctx, store := retiredNameTestStore(t)

	const retired = "zzonly-inthetombstone"
	if _, err := store.pool.Exec(ctx,
		`INSERT INTO public.agent_retired_names (name) VALUES ($1) ON CONFLICT (name) DO NOTHING`,
		retired); err != nil {
		t.Fatalf("seed agent_retired_names: %v", err)
	}

	exists, err := store.NameExists(ctx, retired)
	if err != nil {
		t.Fatalf("NameExists(%q): %v", retired, err)
	}
	if !exists {
		t.Errorf("NameExists(%q)=false for a name in agent_retired_names; the generator would "+
			"reissue it", retired)
	}

	const free = "zznever-existedatall"
	exists, err = store.NameExists(ctx, free)
	if err != nil {
		t.Fatalf("NameExists(%q): %v", free, err)
	}
	if exists {
		t.Errorf("NameExists(%q)=true for a name in neither agents nor agent_retired_names; this "+
			"predicate says every name is taken, which would exhaust the generator outright", free)
	}
}

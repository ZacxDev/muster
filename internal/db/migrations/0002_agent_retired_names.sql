-- muster 0002 — retire an agent name for all time, ENFORCED BY THE DATABASE.
--
-- 🔴 THE HAZARD. Agent names come from a bounded adjective-noun pool
-- (internal/agents/names.go). `BuildUniqueAgentName` accepts any candidate for
-- which `Store.NameExists` is false, and `NameExists` was
--     SELECT EXISTS(SELECT 1 FROM agents WHERE name=$1)
-- i.e. LIVE ROWS ONLY. `PGStore.Delete` is `DELETE FROM agents WHERE id=$1` — a
-- HARD delete; 0001 gives `agents` no deleted_at column and nothing else in the
-- schema records a name that has been removed. So the instant an agent is
-- destroyed its name is handed back to the pool, and the next generated agent
-- can BE its namesake.
--
-- 🔴 WHY THAT IS A PRIVILEGE BUG AND NOT A COSMETIC ONE. The name is the
-- provisioning driver's `provision.Ref.Name`: it is what the per-instance
-- namespace, the ServiceAccount and the cluster-scoped ClusterRole /
-- ClusterRoleBinding of a granted privilege profile are all NAMED after. Those
-- policy objects are cluster-scoped, so they outlive the namespace, the
-- Deployment and the `agents` row — internal/provision/k8s/driver.go's Destroy
-- says so in its own words ("POLICY OBJECTS OUTLIVE WHAT POINTS AT THEM, AND
-- THAT IS A SECURITY BUG WAITING FOR A NAMESAKE … The next instance to take
-- this name gets the same ServiceAccount — the dangling binding's exact
-- subject — and silently inherits access nobody granted it"), and
-- internal/metrics/metrics.go's AgentRBACTeardown counter exists to alert on
-- exactly the teardown failures that leave one behind.
--
-- 🔴 WHAT THIS DOES *NOT* CLOSE, BECAUSE IT WAS NEVER OPEN IN THIS SERVICE.
-- Upstream wording about namesakes covers deployments whose privilege records
-- are keyed on the agent's NAME. muster's are not, and the difference is
-- measurable in 0001 above: `agent_privileges.agent_id` and
-- `privilege_requests.agent_id` are both
-- `BIGINT NOT NULL REFERENCES agents (id) ON DELETE CASCADE`, so deleting an
-- agent deletes its grants and its requests. `privilege_requests.agent_name` is
-- a display copy — internal/privilege/pgstore.go names it in one column list and
-- one INSERT and branches on it nowhere — so nothing resolves a privilege by
-- name. A namesake therefore inherits NO in-database grant. Do not read this
-- migration as protecting one.
--
-- 🔴 NOR IS THE ORDINARY DESTROY PATH THE HAZARD. internal/agentprovision's
-- Adapter.Destroy deletes the `agents` row ONLY when the driver returned nil or
-- ErrNotFound, and internal/provision/k8s's Destroy folds a `revokeAllPolicies`
-- failure into its `firstErr`. So a teardown that fails to revoke already keeps
-- the row, which already keeps the name out of the pool — the pre-existing code
-- covers the loud case on its own.
--
-- ⚠ SO THE MARGINAL COVERAGE IS TWO NARROWER PATHS, AND THEY ARE THE HONEST
-- JUSTIFICATION FOR THIS FILE:
--   1. An `agents` row deleted WITHOUT going through the adapter — a hand-run
--      `psql` DELETE, a future call site, another binary. Nothing else in the
--      system notices, and this is the case a trigger covers and application
--      code cannot.
--   2. `revokeAllPolicies` returning nil while a cluster-side object survives —
--      a partially-applied grant, an object whose labels diverged from the
--      selector, anything the revoke sweep cannot see. The teardown reports
--      success, the row is deleted, and the name goes back in the pool with an
--      orphan still bound to it.
-- Both are real and both are quiet. Neither is "privilege records are keyed on
-- the name"; that claim belongs to a different codebase.
--
-- Precedent in this tree for rejecting name-keying on exactly this ground:
-- internal/agentspec/autosave.go's autosaveRef is keyed on the agent ID because
-- "the id is monotonic and never reused, so the ref is unique forever".
--
-- 🔴 WHY A TRIGGER RATHER THAN A LINE IN Provisioner.Destroy. An
-- application-level insert is a rule every future call site has to REMEMBER,
-- and it covers only the binary that has it. A trigger fires for a DELETE
-- issued by any call site, in any binary, including a hand-run `psql` DELETE
-- and a cascade from another table. There is nothing to remember.
--
-- 🔴 IT CANNOT BACKFILL, AND THAT IS PERMANENT. The names of agents deleted
-- BEFORE this migration are gone — the row was hard-deleted and nothing
-- recorded the name. This closes the class GOING FORWARD ONLY. A name released
-- before this migration can still be reissued exactly once more (after which
-- the trigger tombstones it). internal/agentspec/autosave.go already argues
-- from that residue: it keeps its ref keyed on the id and says not to
-- "simplify" it to the name on the strength of this table.
--
-- 🔴 THE POOL IS FINITE AND THIS MAKES IT STRICTLY SHRINKING, SO THE GENERATOR
-- HAD TO STOP BEING ABLE TO FAIL. names.go's lists are 16 adjectives x 16 nouns
-- = 256 combinations, and a retired name is never freed. BuildUniqueAgentName
-- takes 10 draws, so with k of 256 consumed it MISSES all ten with probability
-- (k/256)^10 — 0.1% at k=128, 8.5% at k=200, certain at k=256 — and k only ever
-- goes up. An earlier revision of this change turned that miss into an error,
-- i.e. a provisioning outage on a clock, arriving at a moment nobody chose with
-- no cause named anywhere an operator looks.
--
-- The draws are now a FAST PATH: when all ten collide the name is DISCRIMINATED
-- (`brave-heron` -> `brave-heron-2`, `-3`, …) until the store says free, so the
-- namespace is unbounded and provisioning cannot fail for want of a name. The
-- discriminated candidate goes through the SAME `NameExists` predicate, so it is
-- checked against this ledger exactly like a drawn one. Details, and the ceiling
-- re-derived from provision.Ref, live in BuildUniqueAgentName's doc.
--
-- ⚠ Widening the word lists is still not done here, and is now an aesthetic
-- choice rather than a fix: a wider pool changes what every existing name-format
-- guard is drawing from and belongs in its own change with its own guards.
--
-- ⚠ `TRUNCATE agents` BYPASSES THIS ENTIRELY, and no FOR EACH ROW trigger can
-- catch it — TRUNCATE fires only statement-level TRUNCATE triggers. This is
-- DOCUMENTATION, not a gap being left open: there is no `TRUNCATE` of `agents`
-- anywhere in the tree, tests included. If one is ever added it must seed
-- agent_retired_names itself, or add a statement-level TRUNCATE trigger.
--
-- 🔴 WHAT IT DELIBERATELY DOES *NOT* DO: refuse an INSERT of a retired name.
-- A BEFORE INSERT refusal would look stronger and would be an outage. The
-- reserved `chief` agent is provisioned with a HAND-SET name (see
-- internal/api/chief_provision.go), so destroying and re-provisioning chief
-- would then be refused by the database with no override. The tombstone is
-- therefore consulted by the GENERATOR (PGStore.NameExists, which this
-- migration's table makes it read) and by nothing else. Honest scope: this
-- closes the AUTO-GENERATED path. A caller that sets the name by hand never
-- consults NameExists at all and can still reuse a dead agent's name — today
-- the only such caller is chief, and `agents.name ... UNIQUE` still stops a
-- collision with a LIVE agent.
--
-- 🔴 PRECONDITIONS. Two, and neither was stated before; both are now ENFORCED or
-- removed rather than left for a reader to discover.
--
--   1. muster's schema must live in `public`. Every object this file touches is
--      `public.`-qualified, which is not a style choice — the trigger function's
--      INSERT targets `public.agent_retired_names` because PGStore.NameExists
--      reads it there. Applied into a non-public schema (a DSN carrying
--      `?search_path=<other>`, an `ALTER DATABASE … SET search_path`) the
--      UNQUALIFIED spelling of this file created its table and attached its
--      trigger in that schema while the trigger body still wrote to `public`,
--      so the migration applied CLEANLY, with no warning, and every subsequent
--      `DELETE FROM agents` then failed with
--      `relation "public.agent_retired_names" does not exist`. A deployment
--      that worked before 0002 stopped being able to destroy an agent at all.
--      Step 0 below turns that into a refusal AT MIGRATION TIME, which is the
--      only point where it is still cheap.
--   2. The role that APPLIES this file owns the trigger function, and therefore
--      is the role the tombstone INSERT runs as. See the SECURITY DEFINER note
--      on the function below for what that buys and what it costs.
--
-- Convention: ADDITIVE + IDEMPOTENT. Nothing is dropped; CREATE TABLE IF NOT
-- EXISTS / CREATE OR REPLACE FUNCTION / REVOKE / DROP TRIGGER IF EXISTS +
-- CREATE TRIGGER all re-run cleanly. ROLLBACK-SAFE in the narrow sense that a
-- rolled-back binary neither reads nor writes these objects and nothing it does
-- errors:
-- the previous binary's NameExists simply does not consult the table, so during
-- a drain it can still reissue a name retired seconds earlier — bounded by the
-- drain, and the reissued agent is an ordinary agent, not a permanent record.
-- The previous binary's DELETEs fire the trigger regardless, which is the whole
-- reason this is in the database rather than in Go.

-- LOCK BUDGET. `CREATE TRIGGER` takes a ShareRowExclusiveLock on `agents`: it
-- does NOT conflict with a plain concurrent SELECT, only with writes and with
-- other DDL. The realistic blocker is a long-running write transaction on
-- `agents`, not a reader. The timeout is precautionary and exists for the
-- DIAGNOSIS: internal/db/db.go sets statement_timeout=10s and no lock_timeout,
-- and internal/db/migrate.go runs this whole file as ONE tx.Exec, so without it
-- a blocked migration surfaces at 10s as `57014 canceling statement due to
-- statement timeout` — which reads as a defect in this SQL. With it, it fails
-- at 3s with `55P03 canceling statement due to lock timeout`, which names the
-- lock and points at whoever holds `agents`. Either way the transaction aborts,
-- the schema is untouched, and the migration retries on the next start.
--
-- ⚠ THAT IS THE FIRST-APPLICATION LOCK AND THE CLAIM IS NOT BLANKET: a
-- `DROP TRIGGER IF EXISTS` on an ABSENT trigger takes no lock on `agents` at
-- all, but on a PRESENT one it escalates to AccessExclusiveLock. migrate.go
-- skips every version <= the recorded max so this body runs exactly once, but
-- re-running it by hand (a restore, a schema rebuild) is the case where the
-- stronger lock applies.
SET LOCAL lock_timeout = '3s';

-- 0. REFUSE A SCHEMA THIS FILE CANNOT WORK IN, BEFORE IT BUILDS ANYTHING.
--
-- 🔴 THE CONSTRAINT WAS ALREADY REAL; ONLY ITS FAILURE MODE CHANGES HERE. This
-- file hardcodes `public` — the trigger body writes to
-- `public.agent_retired_names` because that is where PGStore.NameExists reads.
-- With `search_path` pointing anywhere else, the UNQUALIFIED spelling this file
-- used to carry put the ledger and the trigger in THAT schema while the body
-- still wrote to `public`: both migrations applied cleanly with no warning, and
-- the deployment then discovered it at the first destroy, as
-- `relation "public.agent_retired_names" does not exist` on every
-- `DELETE FROM agents`. The qualification below closes the split; this check
-- closes the case the qualification cannot — a schema where `public.agents`
-- does not exist at all, where the trigger would have nothing to attach to and
-- the ledger would be written to a table 0001 never created.
--
-- ⚠ IT IS DELIBERATELY A CHECK ON `public.agents` AND NOT ON `current_schema()`.
-- What this file needs is not "the session's default schema is public"; it is
-- "0001's tables are in public". A session can carry any search_path it likes
-- and still be correct, and refusing on the search_path would reject working
-- deployments.
DO $$
BEGIN
    IF to_regclass('public.agents') IS NULL THEN
        RAISE EXCEPTION 'muster migration 0002 requires 0001''s schema in `public`, and '
            'public.agents does not exist. Every object this migration creates is '
            'public-qualified, and the trigger it installs writes to '
            'public.agent_retired_names because that is the table '
            'internal/agents.PGStore.NameExists reads. Applying it against a schema '
            'reached through a non-default search_path used to SUCCEED and leave every '
            'later DELETE FROM agents failing. Point DATABASE_URL at a database whose '
            'muster schema is in public, or migrate that schema into public first.';
        -- 🔴 NO `USING ERRCODE`, DELIBERATELY. It used to say
        -- `USING ERRCODE = 'undefined_table'` (42P01) — which is the SAME code
        -- Postgres raises on its own for `relation "public.agents" does not
        -- exist`, i.e. what the qualified DROP TRIGGER below emits when this
        -- check is absent or asks the wrong question. A mutation sweep caught
        -- that: with the check weakened to an UNQUALIFIED `to_regclass('agents')`
        -- the migration still failed, with Postgres's own 42P01 whose text also
        -- contains "public.agents", so the guard's test matched it and the
        -- mutant SURVIVED. Bare RAISE EXCEPTION is P0001 (raise_exception),
        -- which is what makes this refusal distinguishable from the catalog
        -- error it exists to pre-empt — by SQLSTATE, not by wording.
    END IF;
END
$$;

-- 1. The tombstone ledger. One row per name that has ever been retired.
--
-- ⚠ NO IDENTITY COLUMN, DELIBERATELY: the name IS the key, and a surrogate id
-- would let the same name be tombstoned twice. internal/db/migrate_test.go names
-- this table in tablesWithNoIdentityColumn, and asserts the identity-holding set
-- EQUALS the schema ledger minus that exemption — so adding an id here reddens
-- the check by name.
--
-- ⚠ `public.`-QUALIFIED, AND IT IS NOT REDUNDANT WITH STEP 0. The trigger body
-- writes to `public.agent_retired_names`; an unqualified CREATE resolves its
-- CREATION namespace to the FIRST entry on the applying session's search_path
-- and checks only that one — so with `search_path = <other>, public` it builds a
-- SECOND, empty ledger in `<other>` even though `public.agent_retired_names`
-- already exists, while the trigger keeps writing to `public`. Step 0 cannot see
-- that case: `public.agents` exists, so its precondition is satisfied. Pinned by
-- TestMigrationRefusesASchemaWhoseAgentsTableIsNotInPublic's
-- `search_path_preferring_another_schema` subtest.
CREATE TABLE IF NOT EXISTS public.agent_retired_names (
    name       TEXT PRIMARY KEY,
    retired_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE public.agent_retired_names IS
    'Every agent name that has ever been released by a DELETE on agents. Written by the agents_retire_name trigger, never by application code, so it cannot be forgotten at a new call site. PGStore.NameExists treats a name here as TAKEN, which is what stops a namesake inheriting the cluster-scoped RBAC that was named after a destroyed agent. NOT backfilled: names released before this migration are unrecoverable.';
COMMENT ON COLUMN public.agent_retired_names.retired_at IS
    'When the name was first retired. Diagnostic only — nothing branches on it. A name is reserved for all time, not for a window.';

-- 2. The trigger function.
--
-- RETURN value: for an AFTER trigger the return is ignored, but returning OLD
-- is the conventional, non-surprising choice and keeps the function reusable if
-- it is ever attached BEFORE DELETE.
--
-- 🔴 SECURITY DEFINER, AND IT IS WHAT MAKES THE FILE'S OWN SALES PITCH TRUE.
-- The function was SECURITY INVOKER, so the tombstone INSERT ran with the
-- privileges of whoever issued the DELETE. Measured on Postgres 16: a role
-- holding `GRANT SELECT, INSERT, UPDATE, DELETE ON agents` that does not own the
-- table could no longer delete an agent AT ALL —
--
--     ERROR:  permission denied for table agent_retired_names
--     CONTEXT: SQL statement "INSERT INTO public.agent_retired_names …"
--              PL/pgSQL function public.muster_retire_agent_name() line 3
--
-- — and the row survived. That is marginal-coverage path 1 above, the hand-run
-- `psql` DELETE, erroring instead of tombstoning for every role but the one that
-- ran the migration. SECURITY DEFINER runs the body as the function's OWNER, so
-- any role that can delete from `agents` tombstones the name, and the file no
-- longer has to know a role name it cannot know. The alternative —
-- `GRANT INSERT ON public.agent_retired_names TO <role>` — is narrower but needs
-- exactly that.
--
-- ⚠ THE OLD BEHAVIOUR FAILED CLOSED, WHICH IS WHY THIS WAS NOT AN OUTAGE. The
-- DELETE aborted, so the row stayed and the name stayed out of the pool; and
-- muster's server migrates itself and connects as one role, so the application
-- path never saw it.
--
-- 🔴 WHAT SECURITY DEFINER WIDENS, AND THE REVOKE THAT BOUNDS IT. Three things,
-- all measured rather than reasoned:
--   * A role with DELETE on `agents` now writes to the ledger it has no grant
--     on. That is the intent, and the value it writes is `OLD.name` — a name it
--     could already have chosen by creating the agent.
--   * It removes an ACCIDENTAL barrier: while the function was INVOKER, a role
--     without ledger grants simply could not destroy an agent. Nothing relied on
--     that, and nothing should — `agents` grants are the control, not this.
--   * 🔴 THE ONE THAT NEEDED CLOSING: `CREATE FUNCTION` grants EXECUTE to PUBLIC
--     by default, so any role with CREATE on any schema could attach this
--     function to a table OF ITS OWN and drive arbitrary names into
--     `public.agent_retired_names` as the owner — verified end to end, and a
--     tombstone is never freed, so that is a durable poisoning of the generator
--     pool. `REVOKE EXECUTE … FROM PUBLIC` below refuses that borrow
--     (`permission denied for function`) while the real `agents_retire_name`
--     trigger keeps firing for the same non-owner role: privileges on a trigger
--     function are checked when the TRIGGER is created, not when it fires.
--
-- 🔴 THE `SET search_path` LINE IS THE HIJACK GUARD, AND AN EARLIER REVISION OF
-- THIS PARAGRAPH HAD THE TWO GUARDS' ROLES BACKWARDS. It claimed the function
-- had "no proconfig"; it has had `{search_path=pg_catalog}` all along. Four
-- measured combinations, under a session whose search_path puts a decoy
-- `<schema>.agent_retired_names` first:
--
--   qualified + SET      -> writes to public. Correct.
--   qualified, no SET    -> writes to public. The qualification wins alone.
--   UNQUALIFIED + SET    -> `relation "agent_retired_names" does not exist`;
--                           the DELETE aborts. LOUD, and fails closed.
--   UNQUALIFIED, no SET  -> the DELETE SUCCEEDS and the tombstone lands in the
--                           decoy. public gets nothing, NameExists never sees
--                           it, the name is back in the pool. SILENT.
--
-- So the `SET` is what turns the silent case into a loud one, and `public.` is
-- what makes the loud one work. Both stay.
--
-- ⚠ AND THE CLAIM THAT A FUTURE UNQUALIFIED LINE "INHERITS THE PROTECTION" IS
-- NOT TRUE — do not rely on it. With `search_path = pg_catalog` the only schemas
-- an unqualified reference can reach are pg_catalog and the CALLER'S TEMPORARY
-- schema, which Postgres searches first when it is not listed. A caller with a
-- `CREATE TEMP TABLE agent_retired_names` therefore captures any unqualified
-- write — now with the owner's privileges. Appending `pg_temp` does NOT fix it
-- here: measured, `pg_catalog` and `pg_catalog, pg_temp` both still resolve to
-- the temp table, because there is no competing schema on the path for pg_temp
-- to be demoted below (with `public` on the path the trailing `pg_temp` does
-- change the answer — that is the shape the Postgres documentation's advice is
-- about, and it is not this one). The rule for the next person is therefore the
-- simple one: QUALIFY IT.
--
-- 🔴 ONLY THE QUALIFICATION IS PINNED BY A TEST, and that is deliberate.
-- TestRetiredNameTombstoneIsSearchPathProof kills the unqualified version.
-- Removing only the `SET search_path` line while keeping `public.` leaves that
-- guard green — necessarily, per row two of the table above: there is no
-- behaviour to observe. The only available guard for it would assert that
-- pg_proc.proconfig contains the string, i.e. a check on the fix's SPELLING
-- rather than on any state. Do not "strengthen" it into one; if you add an
-- unqualified reference to this body, add a behavioural case for it instead.
CREATE OR REPLACE FUNCTION public.muster_retire_agent_name() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
BEGIN
    INSERT INTO public.agent_retired_names (name) VALUES (OLD.name)
    ON CONFLICT (name) DO NOTHING;
    RETURN OLD;
END;
$$;

-- Idempotent: revoking a privilege that is not held is a no-op, so this re-runs
-- with the rest of the file. The owner keeps EXECUTE through ownership, which is
-- what lets the CREATE TRIGGER below still refer to the function.
REVOKE EXECUTE ON FUNCTION public.muster_retire_agent_name() FROM PUBLIC;

-- 3. The trigger. Postgres has no `CREATE TRIGGER IF NOT EXISTS`; DROP IF
--    EXISTS + CREATE is the idempotent spelling, and it is safe to re-run
--    because both statements are in the same transaction as the rest of this
--    file.
DROP TRIGGER IF EXISTS agents_retire_name ON public.agents;
CREATE TRIGGER agents_retire_name
    AFTER DELETE ON public.agents
    FOR EACH ROW
    EXECUTE FUNCTION public.muster_retire_agent_name();

-- 🔴 NO SEED OF THE CURRENTLY-LIVE NAMES, deliberately. Seeding
-- `INSERT INTO agent_retired_names SELECT name FROM agents` closes NOTHING — a
-- live agent's name is already refused by the live-rows half of NameExists, and
-- the trigger tombstones it the moment it is deleted — and it costs the table's
-- name its meaning: "retired" would then also hold names in active use, so a
-- reader could no longer trust the word.

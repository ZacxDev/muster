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
-- The `agent_privileges` rows themselves are keyed on `agent_id` and cascade,
-- so the DATABASE's record of a grant does go away with the agent. That is the
-- half that is already safe. The half that is not is everything OUTSIDE this
-- database that was named after the agent — and the only lever muster has over
-- it is to stop the name ever being issued twice.
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
-- ⚠ THE POOL IS FINITE AND THIS MAKES IT STRICTLY SHRINKING. names.go's lists
-- are 16 adjectives x 16 nouns = 256 combinations, and a retired name is never
-- freed. BuildUniqueAgentName takes 10 draws, so with k of 256 taken it fails
-- with probability (k/256)^10 — negligible at k=128 (0.1%), one run in twelve
-- at k=200 (8.5%), and certain at k=256. This migration is what makes that arithmetic
-- load-bearing rather than theoretical. Widening the lists is the answer when
-- it bites; it is deliberately NOT done here, because a wider pool changes what
-- every existing name-format guard is drawing from and belongs in its own
-- change with its own guards.
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
-- Convention: ADDITIVE + IDEMPOTENT. Nothing is dropped; CREATE TABLE IF NOT
-- EXISTS / CREATE OR REPLACE FUNCTION / DROP TRIGGER IF EXISTS + CREATE TRIGGER
-- all re-run cleanly. ROLLBACK-SAFE in the narrow sense that a rolled-back
-- binary neither reads nor writes these objects and nothing it does errors:
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

-- 1. The tombstone ledger. One row per name that has ever been retired.
--
-- ⚠ NO IDENTITY COLUMN, DELIBERATELY: the name IS the key, and a surrogate id
-- would let the same name be tombstoned twice. internal/db/migrate_test.go's
-- TestEveryIdentityColumnIsGeneratedALWAYS counts identity columns against the
-- table ledger and its arithmetic accounts for this table by name.
CREATE TABLE IF NOT EXISTS agent_retired_names (
    name       TEXT PRIMARY KEY,
    retired_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE agent_retired_names IS
    'Every agent name that has ever been released by a DELETE on agents. Written by the agents_retire_name trigger, never by application code, so it cannot be forgotten at a new call site. PGStore.NameExists treats a name here as TAKEN, which is what stops a namesake inheriting the cluster-scoped RBAC that was named after a destroyed agent. NOT backfilled: names released before this migration are unrecoverable.';
COMMENT ON COLUMN agent_retired_names.retired_at IS
    'When the name was first retired. Diagnostic only — nothing branches on it. A name is reserved for all time, not for a window.';

-- 2. The trigger function.
--
-- RETURN value: for an AFTER trigger the return is ignored, but returning OLD
-- is the conventional, non-surprising choice and keeps the function reusable if
-- it is ever attached BEFORE DELETE.
--
-- 🔴 BOTH THE `SET search_path` AND THE `public.` QUALIFICATION ARE
-- LOAD-BEARING. The function is SECURITY INVOKER with no proconfig, so an
-- UNQUALIFIED `INSERT INTO agent_retired_names` resolves through the CALLING
-- SESSION's search_path. With a decoy `<schema>.agent_retired_names` first on
-- the path, a `DELETE FROM public.agents` then SUCCEEDS and writes the
-- tombstone into the decoy — public gets nothing, NameExists never sees it, and
-- the name is back in the pool, silently. That defeats precisely the property
-- this file is sold on ("any call site, any binary, including a hand-run psql
-- DELETE") — and a hand-run psql session is exactly where a non-default
-- search_path comes from.
--
-- The two guards cover DIFFERENT things, which is why both are here:
--   * `public.` stops THIS statement's table being redirected.
--   * `SET search_path = pg_catalog` stops anything ELSE in the body being
--     redirected — today there is nothing else, which is the point: the next
--     person to add a line inherits the protection instead of having to know.
--
-- 🔴 ONLY THE FIRST OF THE TWO IS PINNED BY A TEST, and that is deliberate.
-- TestRetiredNameTombstoneIsSearchPathProof kills the unqualified version.
-- Removing only the `SET search_path` line while keeping `public.` leaves that
-- guard green — necessarily, because with nothing unqualified in the body there
-- is no behaviour to observe. The only available guard for it would assert that
-- pg_proc.proconfig contains the string, i.e. a check on the fix's SPELLING
-- rather than on any state. Do not "strengthen" it into one; if you add an
-- unqualified reference to this body, add a behavioural case for it instead.
--
-- ⚠ CONSEQUENCE, stated because it is a real constraint: `public` is hardcoded,
-- so this function cannot be applied into a scratch schema and still work. That
-- matches how this schema is checked — against the migrated `public` schema,
-- not a scratch one.
CREATE OR REPLACE FUNCTION muster_retire_agent_name() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $$
BEGIN
    INSERT INTO public.agent_retired_names (name) VALUES (OLD.name)
    ON CONFLICT (name) DO NOTHING;
    RETURN OLD;
END;
$$;

-- 3. The trigger. Postgres has no `CREATE TRIGGER IF NOT EXISTS`; DROP IF
--    EXISTS + CREATE is the idempotent spelling, and it is safe to re-run
--    because both statements are in the same transaction as the rest of this
--    file.
DROP TRIGGER IF EXISTS agents_retire_name ON agents;
CREATE TRIGGER agents_retire_name
    AFTER DELETE ON agents
    FOR EACH ROW
    EXECUTE FUNCTION muster_retire_agent_name();

-- 🔴 NO SEED OF THE CURRENTLY-LIVE NAMES, deliberately. Seeding
-- `INSERT INTO agent_retired_names SELECT name FROM agents` closes NOTHING — a
-- live agent's name is already refused by the live-rows half of NameExists, and
-- the trigger tombstones it the moment it is deleted — and it costs the table's
-- name its meaning: "retired" would then also hold names in active use, so a
-- reader could no longer trust the word.

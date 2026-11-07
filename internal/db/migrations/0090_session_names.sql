-- muster 0090 — A SESSION'S STABLE NAME (docs/focus.md#a-sessions-name).
--
-- Every session — a dispatched agent or a mirrored Claude Code / opencode
-- conversation — is headed by a NAME that does not move, with its live focus
-- as the line under it. The name is resolved by ONE rule
-- (internal/sessionname.Resolve): a name the person gave it, else a name
-- muster pinned for a role (the chief, an officer), else the AUTO name frozen
-- from the session's first good focus title, else what it was called before.
--
--   agent_focus.name_auto / name_auto_at   the auto name and when it froze.
--       Written ONCE, by the focus write that first passed the quality bar
--       (focus.Nameable), in the same statement as that focus
--       (focus.PGStore.Upsert): '' until then, and never rewritten after —
--       the upsert's CASE keeps a non-empty one whatever the next title is.
--   agent_focus.name_manual / name_manual_at   the name the person gave a
--       MIRRORED session (focus.PGStore.SetManualName), '' for none. A
--       dispatched agent's manual name stays where it always was —
--       agents.display_name — so the chief's identity (its display name,
--       internal/api chief_agent.go) is unchanged; agent rows never hold one
--       here (agent_focus_name_manual_mirror_only).
--   agents.display_name_source   WHO set agents.display_name: 'default' (the
--       slug, copied), 'dispatch' (derived from the source task at dispatch),
--       'system' (a role muster pinned: the chief, an officer) or 'manual'
--       (the person renamed it). Only 'manual' and 'system' outrank the auto
--       name.
--
-- 🔴 THE NAME IS STORED, NEVER DERIVED FROM HISTORY. Focus history is pruned
-- after 30 days (focus.DefaultHistoryRetention), so "the oldest history row"
-- is not a name: it moves. These columns are on the per-subject row, which
-- lives as long as its session (an agent's dies with it; a mirrored one is
-- pruned a retention after it was last seen — focus.PGStore.PruneAll keeps a
-- row carrying a name, auto or manual, for focus.NameRetention longer).
--
-- 🔴 THE BACKFILL OF display_name_source IS A DECISION, STATED: an existing
-- display name equal to the slug is 'default'; one spelled "#<task id>" or
-- "#<task id> · …" is 'dispatch' (dispatchDisplayName's shape); "officer <id>
-- #<n>" is 'system' (officerFleet.Start's shape); ANY OTHER is 'manual' —
-- the operator's rule "an existing agent DisplayName set by a user counts as
-- manual", and the chief's "chief" is one of them (it was set by a person).
--
-- agent_focus is owned and under FORCE row-level security (0020); new columns
-- inherit both, so nothing here touches a policy, grant or default and
-- db.RLSOn re-applies nothing. Idempotent: every statement can run twice, and
-- the backfill touches only rows still at the column's default.
--
-- Must apply after its predecessor (the contiguity test refuses a gap).
--
-- ROLLBACK (after rolling the image back; an older image never reads them):
--   ALTER TABLE agent_focus DROP CONSTRAINT IF EXISTS agent_focus_name_auto_cap;
--   ALTER TABLE agent_focus DROP CONSTRAINT IF EXISTS agent_focus_name_manual_cap;
--   ALTER TABLE agent_focus DROP CONSTRAINT IF EXISTS agent_focus_name_manual_mirror_only;
--   ALTER TABLE agent_focus DROP COLUMN IF EXISTS name_auto, DROP COLUMN IF EXISTS name_auto_at,
--       DROP COLUMN IF EXISTS name_manual, DROP COLUMN IF EXISTS name_manual_at;
--   ALTER TABLE agents DROP CONSTRAINT IF EXISTS agents_display_name_source_check;
--   ALTER TABLE agents DROP COLUMN IF EXISTS display_name_source;
--   DELETE FROM schema_migrations WHERE version = 90;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.agent_focus
    ADD COLUMN IF NOT EXISTS name_auto      TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS name_auto_at   TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS name_manual    TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS name_manual_at TIMESTAMPTZ;

-- The title's own storage bound (agent_focus_title_cap): an auto name IS a
-- title, and a manual name is held to the same.
ALTER TABLE public.agent_focus DROP CONSTRAINT IF EXISTS agent_focus_name_auto_cap;
ALTER TABLE public.agent_focus ADD CONSTRAINT agent_focus_name_auto_cap CHECK (char_length(name_auto) <= 80);
ALTER TABLE public.agent_focus DROP CONSTRAINT IF EXISTS agent_focus_name_manual_cap;
ALTER TABLE public.agent_focus ADD CONSTRAINT agent_focus_name_manual_cap CHECK (char_length(name_manual) <= 80);
ALTER TABLE public.agent_focus DROP CONSTRAINT IF EXISTS agent_focus_name_manual_mirror_only;
ALTER TABLE public.agent_focus ADD CONSTRAINT agent_focus_name_manual_mirror_only
    CHECK (name_manual = '' OR subject_kind = 'mirror');

ALTER TABLE public.agents ADD COLUMN IF NOT EXISTS display_name_source TEXT NOT NULL DEFAULT 'default';
ALTER TABLE public.agents DROP CONSTRAINT IF EXISTS agents_display_name_source_check;
ALTER TABLE public.agents ADD CONSTRAINT agents_display_name_source_check
    CHECK (display_name_source IN ('default', 'dispatch', 'system', 'manual'));

-- The backfill (see the header). Only rows still at 'default' are touched, so
-- a re-application cannot undo a later write.
UPDATE public.agents SET display_name_source = CASE
        WHEN display_name ~ '^#[0-9]+( · .*)?$' THEN 'dispatch'
        WHEN display_name ~ '^officer \S+ #\S+$' THEN 'system'
        ELSE 'manual' END
 WHERE display_name_source = 'default' AND display_name <> '' AND display_name <> name;

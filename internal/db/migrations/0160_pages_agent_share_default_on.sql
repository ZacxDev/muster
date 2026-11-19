-- muster 0160 — "LET MY AGENTS SHARE PAGES WITHOUT ASKING" IS ON BY DEFAULT
-- (docs/pages.md#when-an-agent-shares; internal/notifypref).
--
--   notify_preferences.pages_agent_share   (0135) DEFAULT false → DEFAULT true:
--       a row written from here on by another switch (Show tmux sessions,
--       the tmux-name-first title) records this one ON. A person with NO row
--       reads on too, in the code (notifypref.PG.PagesAgentShare: a missing
--       row is on for a person; a team owner is ALWAYS off, stored value or
--       not, since nobody can sign in as a team to turn it off).
--
-- 🔴 EXISTING ROWS KEEP THEIR STORED VALUE: NOTHING IS BACKFILLED. A stored
-- false may be a user's real choice (they turned it off on Account), and is
-- indistinguishable from a row another switch wrote under 0135's default, so
-- both stay off; a stored true stays on. Only the DEFAULT moves. (Keeping
-- stored offs and not backfilling is the operator's decision.)
--
-- A changed default on 0094's owned table: its forced owner_isolation policy
-- is untouched, and rollback/rls_off.sql + --rls-on re-apply 0094 unchanged.
-- Must apply after its predecessor (the contiguity test refuses a gap).
-- Numbered 0160 when written: main's newest was 0156 and open pull requests
-- held 0157, 0158 and 0159. 🔴 RENUMBER IT at merge to the next free number
-- (and its test's version constant) if those have not all landed first.
-- Idempotent.
--
-- ROLLBACK (after rolling the image back: an older image reads a missing row
-- as off again). A row written while this was applied keeps the true it was
-- given; the rollback cannot tell it from a user's own choice, so it leaves it:
--   ALTER TABLE public.notify_preferences ALTER COLUMN pages_agent_share SET DEFAULT false;
--   DELETE FROM schema_migrations WHERE version = 160;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.notify_preferences
    ALTER COLUMN pages_agent_share SET DEFAULT true;

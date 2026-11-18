-- muster 0085 — A STORED SESSION KEY NO LONGER NAMES A HERMES SESSION.
--
-- Hermes support was removed: no window carries a Hermes session any more
-- (mirror.NormalizeWindows drops an older host's "hermes" runtime), so nothing
-- writes a "hermes:<id>" key. This deletes the rows that still hold one (an
-- operator decision: on the live database they were 15 agent_focus_history
-- rows and none elsewhere), then narrows the four session-key CHECKs 0052
-- wrote back to a Claude Code id or an opencode key — mirror.SessionKeyPattern,
-- which a test in internal/db reads from the LATEST migration that writes
-- these CHECKs:
--
--   ^([0-9A-Za-z][0-9A-Za-z_-]{0,63}|opencode:[0-9A-Za-z][0-9A-Za-z_-]{0,127})$
--
-- The deletes come first, in every table the CHECKs cover, so the ADD never
-- meets a row it refuses: agent_focus and agent_focus_history (one subject,
-- written together by internal/focus), session_routes (a route to a Hermes
-- window) and attention_asks (an ask raised from one). A Claude Code id never
-- contains ':', so the LIKE matches Hermes keys only.
--
-- Rollback (the deleted rows are not restored; an image from before this
-- migration runs unchanged over it, writing no Hermes key either): re-add each
-- constraint with 0052's expression,
--   ^([0-9A-Za-z][0-9A-Za-z_-]{0,63}|hermes:[0-9A-Za-z][0-9A-Za-z_-]{0,127}|opencode:[0-9A-Za-z][0-9A-Za-z_-]{0,127})$
-- e.g.
--   ALTER TABLE public.session_routes DROP CONSTRAINT session_routes_key_check;
--   ALTER TABLE public.session_routes ADD CONSTRAINT session_routes_key_check
--     CHECK (target_key ~ '<0052''s expression>');
-- (and the same for the other three, as 0052 spells them), then
--   DELETE FROM schema_migrations WHERE version = 85;
--
-- Not re-applied by db.RLSOn; idempotent anyway (DELETE, DROP … IF EXISTS,
-- then ADD).

SET LOCAL lock_timeout = '3s';

DELETE FROM public.agent_focus WHERE subject_kind = 'mirror' AND subject_key LIKE 'hermes:%';
DELETE FROM public.agent_focus_history WHERE subject_kind = 'mirror' AND subject_key LIKE 'hermes:%';
DELETE FROM public.session_routes WHERE target_key LIKE 'hermes:%';
DELETE FROM public.attention_asks WHERE mirror_session LIKE 'hermes:%';

ALTER TABLE public.agent_focus DROP CONSTRAINT IF EXISTS agent_focus_subject_check;
ALTER TABLE public.agent_focus
    ADD CONSTRAINT agent_focus_subject_check CHECK (
        (subject_kind = 'agent' AND agent_id IS NOT NULL AND subject_key = agent_id::text)
        OR (subject_kind = 'mirror' AND agent_id IS NULL
            AND subject_key ~ '^([0-9A-Za-z][0-9A-Za-z_-]{0,63}|opencode:[0-9A-Za-z][0-9A-Za-z_-]{0,127})$'));

ALTER TABLE public.agent_focus_history DROP CONSTRAINT IF EXISTS agent_focus_history_subject_check;
ALTER TABLE public.agent_focus_history
    ADD CONSTRAINT agent_focus_history_subject_check CHECK (
        (subject_kind = 'agent' AND agent_id IS NOT NULL AND subject_key = agent_id::text)
        OR (subject_kind = 'mirror' AND agent_id IS NULL
            AND subject_key ~ '^([0-9A-Za-z][0-9A-Za-z_-]{0,63}|opencode:[0-9A-Za-z][0-9A-Za-z_-]{0,127})$'));

ALTER TABLE public.session_routes DROP CONSTRAINT IF EXISTS session_routes_key_check;
ALTER TABLE public.session_routes
    ADD CONSTRAINT session_routes_key_check
    CHECK (target_key ~ '^([0-9A-Za-z][0-9A-Za-z_-]{0,63}|opencode:[0-9A-Za-z][0-9A-Za-z_-]{0,127})$');

ALTER TABLE public.attention_asks DROP CONSTRAINT IF EXISTS attention_asks_mirror_session_check;
ALTER TABLE public.attention_asks
    ADD CONSTRAINT attention_asks_mirror_session_check
    CHECK (mirror_session IS NULL
        OR mirror_session ~ '^([0-9A-Za-z][0-9A-Za-z_-]{0,63}|opencode:[0-9A-Za-z][0-9A-Za-z_-]{0,127})$');

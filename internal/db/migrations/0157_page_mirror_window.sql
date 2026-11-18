-- muster 0157 — THE TMUX WINDOW A MIRRORED SESSION'S PAGE WAS MADE IN
-- (internal/pages; docs/pages.md "A session's pages").
--
--   pages.mirror_tmux_session, pages.mirror_window   the tmux session NAME and
--       tmux WINDOW ID (@N) of the window whose Claude Code (or opencode)
--       session made the page, as the host listed it when the page_create was
--       bound (internal/api mirror_mcp.go), beside mirror_host. A session
--       view lists the pages made IN ITS WINDOW by these (and, as before, the
--       pages of the sessions it runs now, by mirror_session), so a /clear, a
--       restart or a resume of Claude Code in the window — a new session id —
--       does not lose the window's earlier pages.
--
--       Both NULL, together, for an agent's page, for a remote caller's (no
--       window: mirrored elsewhere), and for every page made before this
--       migration (nothing guesses them).
--
--       ⚠ A tmux window id is tmux's own and stable for the window's life
--       (it survives a renumbering and a rename; never an index, which
--       moves). A tmux SERVER restart starts ids again, so a new window can
--       take an old one's id; the session name beside it narrows that, and
--       the host name keeps another machine's out.
--
-- 0116's owner_isolation policy covers the new columns: they are columns of
-- an owned table, so rollback/rls_off.sql and --rls-on need nothing new.
--
-- Inert without its code: an older image never reads or writes them.
--
-- Must apply after its predecessor, 0156 (the contiguity test refuses a gap):
-- 🔴 RENUMBER IT at merge if main moved. 0155 and 0156 are claimed by other
-- open pull requests (#796, #804).
--
-- ROLLBACK (an older image reads neither column, so roll the image back
-- first, then):
--   ALTER TABLE public.pages DROP CONSTRAINT IF EXISTS pages_mirror_window_check;
--   ALTER TABLE public.pages DROP COLUMN IF EXISTS mirror_window;
--   ALTER TABLE public.pages DROP COLUMN IF EXISTS mirror_tmux_session;
--   DELETE FROM schema_migrations WHERE version = 157;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.pages ADD COLUMN IF NOT EXISTS mirror_tmux_session TEXT;
ALTER TABLE public.pages ADD COLUMN IF NOT EXISTS mirror_window TEXT;
ALTER TABLE public.pages DROP CONSTRAINT IF EXISTS pages_mirror_window_check;
ALTER TABLE public.pages ADD CONSTRAINT pages_mirror_window_check
    CHECK ((mirror_window IS NULL) = (mirror_tmux_session IS NULL)
       AND (mirror_window IS NULL OR (char_length(mirror_window) BETWEEN 1 AND 200
            AND char_length(mirror_tmux_session) BETWEEN 1 AND 200 AND mirror_host <> '')));

-- muster 0106 — THE SESSION A QUEUED MESSAGE WAS WRITTEN FOR
-- (docs/design/opencode-first-class.md, "Panes the launcher did not start").
--
--   send_queue.agent_session   the agent session (an opencode session id) the
--       page's view showed for the target when the message was queued; NULL
--       for none (a Claude Code window, every row before this column). A
--       hand-opened opencode pane's row is delivered only while the window
--       still shows exactly this session, and a row naming none is refused
--       for such a pane — never delivered to whatever session the pane shows
--       by then.
--
-- The column is the wire's ValidAgentSession shape. Nothing else changes:
-- the table's row-level security covers it as it covers every column.

ALTER TABLE send_queue ADD COLUMN agent_session text
    CHECK (agent_session IS NULL OR agent_session ~ '^[0-9A-Za-z][0-9A-Za-z_-]{0,127}$');

-- muster 0120 — A BUILTIN OFFICER'S ASKS AND ESCALATION NOTICES
-- (internal/officers/builtin; docs/design/officers-first-class.md "Day-1
-- scope of the built-in backend").
--
--   officer_items.asks     every Attention ask an instance raised on the item
--                          (officer_ask_person): [{ask_id, question, options,
--                          asked_by, asked_at, status, answer?, resolved_at?}].
--                          The item is `waiting` while one is open; muster
--                          re-opens it with the answers on it once none is.
--   officer_items.notices  who an ESCALATED item asks through Attention:
--                          [{recipient, suppressed, attention_id, settled}], written IN
--                          THE SAME TRANSACTION as the escalating complete, one
--                          entry per recipient (the officer's owner and its
--                          escalate_to), `suppressed` for a shadow officer.
--                          attention_id is null until muster has raised the
--                          ask; it is set once, conditionally, so two
--                          replicas never raise one twice. A recipient's
--                          ANSWER re-opens the item for the officer with the
--                          answer on its asks[] (settled marks each done).
--
-- Columns on officer_items, not tables of their own: both belong to exactly
-- one item and are read with it, so they live under its owner and its
-- row-level security (0118) with nothing new to own.
--
-- Must apply after 0119 (the contiguity test refuses a gap).
--
-- ROLLBACK. An older image never reads either column, so it runs unchanged
-- beside them. To remove them (optionally):
--   DROP INDEX public.officer_items_waiting, public.officer_items_notice_due,
--              public.officer_items_escalation_open;
--   ALTER TABLE public.officer_items DROP COLUMN asks, DROP COLUMN notices;
--   DELETE FROM schema_migrations WHERE version = 120;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.officer_items
    ADD COLUMN IF NOT EXISTS asks JSONB NOT NULL DEFAULT '[]'
        CONSTRAINT officer_items_asks_array CHECK (jsonb_typeof(asks) = 'array' AND jsonb_array_length(asks) <= 20),
    ADD COLUMN IF NOT EXISTS notices JSONB NOT NULL DEFAULT '[]'
        CONSTRAINT officer_items_notices_array CHECK (jsonb_typeof(notices) = 'array' AND jsonb_array_length(notices) <= 21);

-- The asks sweep reads waiting items; the notice sender, escalations with a
-- notice not yet raised.
CREATE INDEX IF NOT EXISTS officer_items_waiting ON public.officer_items (id) WHERE status = 'waiting';
CREATE INDEX IF NOT EXISTS officer_items_notice_due ON public.officer_items (id)
    WHERE notices @> '[{"attention_id": null, "suppressed": false}]';
-- …and escalations whose asks are raised and not yet settled.
CREATE INDEX IF NOT EXISTS officer_items_escalation_open ON public.officer_items (id)
    WHERE notices @> '[{"settled": false}]';

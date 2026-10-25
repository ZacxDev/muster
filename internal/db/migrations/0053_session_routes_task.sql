-- muster 0053 — A ROUTE CAN CARRY A TASK (internal/plugins/routes.go): the
-- chief's route_to_session hands a mirrored session either a plugin message
-- (as since 0021) or one of the owner's tasks. A task route's answer becomes a
-- comment on the task; everything else (the paste, route_reply, the deadline,
-- the hand-back, the audit) is the message route's. docs/plugins.md#routing-a-task-to-a-session.
--
--   session_routes.task_id   the task a route carries; NULL for a message route.
--
-- 🔴 EXACTLY ONE OF plugin_message_id, task_id (session_routes_one_item_check).
-- 🔴 THE TASK IS THE ROUTE OWNER'S, CHECKED BY A TRIGGER, NOT A COMPOSITE KEY:
-- 0027's reason (a composite FK onto notes (id, owner_id) would depend on
-- notes_id_owner_key, which 0006's body drops and re-adds when re-run), and
-- 0027's shape. ON DELETE CASCADE: a deleted task leaves nothing to route.
-- 🔴 AT MOST ONE OPEN ROUTE PER TASK (session_routes_one_open_task), as 0021's
-- session_routes_one_open holds one per message.
--
-- Not re-applied by db.RLSOn (it changes no policy, grant or default); 0021's
-- body, which RLSOn may re-apply, creates the table only IF NOT EXISTS, so a
-- re-apply leaves this in place. Idempotent.
--
-- ROLLBACK (after rolling the image back; an older image cannot read a task route):
--   DELETE FROM session_routes WHERE task_id IS NOT NULL;
--   DROP TRIGGER IF EXISTS session_routes_task_is_owners ON session_routes;
--   DROP FUNCTION IF EXISTS session_routes_task_is_owners();
--   ALTER TABLE session_routes DROP COLUMN IF EXISTS task_id;
--   ALTER TABLE session_routes ALTER COLUMN plugin_message_id SET NOT NULL;
--   DELETE FROM schema_migrations WHERE version = 53;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.session_routes ADD COLUMN IF NOT EXISTS task_id BIGINT;
ALTER TABLE public.session_routes ALTER COLUMN plugin_message_id DROP NOT NULL;

ALTER TABLE public.session_routes DROP CONSTRAINT IF EXISTS session_routes_task_id_fkey;
ALTER TABLE public.session_routes
    ADD CONSTRAINT session_routes_task_id_fkey
    FOREIGN KEY (task_id) REFERENCES public.notes (id) ON DELETE CASCADE;

ALTER TABLE public.session_routes DROP CONSTRAINT IF EXISTS session_routes_one_item_check;
ALTER TABLE public.session_routes ADD CONSTRAINT session_routes_one_item_check
    CHECK ((plugin_message_id IS NULL) <> (task_id IS NULL));

CREATE OR REPLACE FUNCTION public.session_routes_task_is_owners() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.task_id IS NOT NULL
       AND NEW.owner_id IS DISTINCT FROM (SELECT n.owner_id FROM public.notes n WHERE n.id = NEW.task_id) THEN
        RAISE EXCEPTION 'session_routes: owner % is not the owner of task %', NEW.owner_id, NEW.task_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS session_routes_task_is_owners ON public.session_routes;
CREATE TRIGGER session_routes_task_is_owners BEFORE INSERT OR UPDATE OF owner_id, task_id
    ON public.session_routes FOR EACH ROW EXECUTE FUNCTION public.session_routes_task_is_owners();

-- At most one OPEN route per task; also the cascade's index for open routes.
CREATE UNIQUE INDEX IF NOT EXISTS session_routes_one_open_task
    ON public.session_routes (task_id) WHERE state IN ('routed', 'answered');
-- The cascade from notes, for every route.
CREATE INDEX IF NOT EXISTS idx_session_routes_task ON public.session_routes (task_id) WHERE task_id IS NOT NULL;

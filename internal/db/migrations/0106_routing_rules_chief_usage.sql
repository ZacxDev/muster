-- muster 0106 — ROUTING RULES AND CHIEF TOKEN TELEMETRY (docs/plugins.md#routing-rules,
-- docs/usage.md; the stores are internal/plugins/rules.go and
-- internal/chiefusage, the server internal/api/routing_rules.go and usage.go).
--
--   routing_rules   one row per rule the chief PROPOSED: the plugin it is for
--       (composite FK, so a rule cannot name another owner's plugin), what it
--       matches (exact sender, exact thread, a prefix of external_id, a prefix
--       of the subject — each NULL when not matched on; at least one is set),
--       whether the sender
--       must be verified, and its action: 'route' to the mirrored session
--       route_session, or 'settle' with settle_reason. state is 'proposed'
--       until the owner's one tap on its card (ask_id) makes it 'active' or
--       'declined'; the owner may then disable ('disabled') and re-enable it,
--       or delete the row. hits / last_hit_at count the messages it handled;
--       fallbacks / last_fallback_at the ones it matched whose session could
--       not take them (handed to the chief instead). source_message is the
--       plugin message the chief proposed it from.
--
--   chief_turn_usage   one row per turn of an owner's CHIEF: what started it
--       (trigger: a plugin message — its plugin, sender and id — a task event,
--       an owner chat line, a line typed on a session's view, an answer, a
--       session's question, a route's hand-back, the machine message door,
--       the first turn, other), what it did (outcome), and the tokens the
--       runtime reported for it (input, output, cache read, cache creation;
--       usage_known false when the runtime reported none).
--
--   plugin_message_handling   one row per inbound plugin message: who handled
--       it — muster's own deterministic paths (fleet_report, thread,
--       closed_pr, pr_route, mail_resume, task_event_mention, and the task
--       triage's task_forward, task_route and task_board), an approved
--       rule (rule, with rule_id), or the chief (chief; fallback_rule_id set
--       when a rule matched and its session could not take it).
--
-- 🔴 A ROUTE RULE NEVER MATCHES AN UNVERIFIED SENDER: routing_rules_route_verified.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, in 0100's shape. The two
-- telemetry tables name no other table (the chief, the message and the rule are
-- plain ids): a row outlives the message, the plugin and the chief it counted.
--
-- 🔴 RE-APPLIED BY db.RLSOn (rollback.go) WHEN THE TABLES EXIST, SO EVERY
-- STATEMENT IS IDEMPOTENT.
--
-- Must apply after its predecessor (the contiguity test refuses a gap). The
-- number was the next free one on main when written; it may need renumbering
-- at merge (with routingRulesMigrationVersion in internal/db/rollback.go).
--
-- ROLLBACK (after rolling the image back; an older image never reads them):
--   DROP TABLE IF EXISTS public.routing_rules, public.chief_turn_usage, public.plugin_message_handling;
--   DELETE FROM schema_migrations WHERE version = 106;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.routing_rules (
    id                     BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id               BIGINT      NOT NULL
                           DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                           REFERENCES public.users (id),
    plugin_id              BIGINT      NOT NULL,
    plugin_name            TEXT        NOT NULL CHECK (char_length(plugin_name) BETWEEN 1 AND 40),
    match_sender           TEXT        CHECK (match_sender IS NULL OR char_length(match_sender) BETWEEN 1 AND 320),
    match_thread           TEXT        CHECK (match_thread IS NULL OR char_length(match_thread) BETWEEN 1 AND 200),
    match_external_prefix  TEXT        CHECK (match_external_prefix IS NULL OR char_length(match_external_prefix) BETWEEN 8 AND 200),
    match_subject_prefix   TEXT        CHECK (match_subject_prefix IS NULL OR char_length(match_subject_prefix) BETWEEN 3 AND 80),
    require_verified       BOOLEAN     NOT NULL DEFAULT false,
    action                 TEXT        NOT NULL CHECK (action IN ('route', 'settle')),
    route_session          TEXT        CHECK (route_session IS NULL OR char_length(route_session) BETWEEN 1 AND 200),
    settle_reason          TEXT        CHECK (settle_reason IS NULL OR char_length(settle_reason) BETWEEN 1 AND 300),
    state                  TEXT        NOT NULL DEFAULT 'proposed'
                           CHECK (state IN ('proposed', 'active', 'disabled', 'declined')),
    chief_agent            BIGINT      NOT NULL,
    source_message         BIGINT      NOT NULL,
    ask_id                 BIGINT,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at             TIMESTAMPTZ,
    approved_at            TIMESTAMPTZ,
    hits                   BIGINT      NOT NULL DEFAULT 0 CHECK (hits >= 0),
    last_hit_at            TIMESTAMPTZ,
    fallbacks              BIGINT      NOT NULL DEFAULT 0 CHECK (fallbacks >= 0),
    last_fallback_at       TIMESTAMPTZ,
    CONSTRAINT routing_rules_plugin_owner_fkey FOREIGN KEY (plugin_id, owner_id)
        REFERENCES public.plugins (id, owner_id) ON DELETE CASCADE,
    -- WHO or WHERE: an exact sender or thread; a prefix only narrows one
    CONSTRAINT routing_rules_matches CHECK (num_nonnulls(match_sender, match_thread) >= 1),
    -- a route names its session and no reason; a settle its reason and no session
    CONSTRAINT routing_rules_action_fields CHECK (
        (action = 'route' AND route_session IS NOT NULL AND settle_reason IS NULL) OR
        (action = 'settle' AND settle_reason IS NOT NULL AND route_session IS NULL)),
    CONSTRAINT routing_rules_route_verified CHECK (action <> 'route' OR require_verified),
    -- approved exactly when it was ever active; decided exactly when it left 'proposed'
    CONSTRAINT routing_rules_approved CHECK ((approved_at IS NOT NULL) = (state IN ('active', 'disabled'))),
    CONSTRAINT routing_rules_decided CHECK ((decided_at IS NULL) = (state = 'proposed'))
);

ALTER TABLE public.routing_rules ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

CREATE INDEX IF NOT EXISTS routing_rules_owner_plugin ON public.routing_rules (owner_id, plugin_id, state);
CREATE INDEX IF NOT EXISTS routing_rules_owner_ask ON public.routing_rules (owner_id, ask_id) WHERE ask_id IS NOT NULL;

ALTER TABLE public.routing_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.routing_rules FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.routing_rules;
CREATE POLICY owner_isolation ON public.routing_rules
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.routing_rules TO muster_app;

CREATE TABLE IF NOT EXISTS public.chief_turn_usage (
    id                     BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id               BIGINT      NOT NULL
                           DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                           REFERENCES public.users (id),
    chief_agent            BIGINT      NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    trigger                TEXT        NOT NULL CHECK (trigger IN ('plugin', 'task_event', 'owner_chat', 'view',
                                       'answer', 'session_question', 'hand_back', 'machine', 'kickoff', 'other')),
    plugin_name            TEXT        NOT NULL DEFAULT '' CHECK (char_length(plugin_name) <= 40),
    sender                 TEXT        NOT NULL DEFAULT '' CHECK (char_length(sender) <= 320),
    plugin_message_id      BIGINT,
    outcome                TEXT        NOT NULL CHECK (outcome IN ('routed', 'settled', 'ask', 'task',
                                       'rule_proposed', 'other', 'failed')),
    rule_proposed          BOOLEAN     NOT NULL DEFAULT false,
    input_tokens           BIGINT      NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    output_tokens          BIGINT      NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    cache_read_tokens      BIGINT      NOT NULL DEFAULT 0 CHECK (cache_read_tokens >= 0),
    cache_creation_tokens  BIGINT      NOT NULL DEFAULT 0 CHECK (cache_creation_tokens >= 0),
    usage_known            BOOLEAN     NOT NULL DEFAULT false,
    duration_ms            BIGINT      NOT NULL DEFAULT 0 CHECK (duration_ms >= 0)
);

ALTER TABLE public.chief_turn_usage ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

CREATE INDEX IF NOT EXISTS chief_turn_usage_owner_at ON public.chief_turn_usage (owner_id, created_at);

ALTER TABLE public.chief_turn_usage ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.chief_turn_usage FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.chief_turn_usage;
CREATE POLICY owner_isolation ON public.chief_turn_usage
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT ON public.chief_turn_usage TO muster_app;

CREATE TABLE IF NOT EXISTS public.plugin_message_handling (
    id                     BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id               BIGINT      NOT NULL
                           DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                           REFERENCES public.users (id),
    plugin_message_id      BIGINT      NOT NULL,
    plugin_name            TEXT        NOT NULL DEFAULT '' CHECK (char_length(plugin_name) <= 40),
    sender                 TEXT        NOT NULL DEFAULT '' CHECK (char_length(sender) <= 320),
    handler                TEXT        NOT NULL CHECK (handler IN ('fleet_report', 'thread', 'closed_pr', 'pr_route',
                                       'mail_resume', 'task_event_mention', 'task_forward', 'task_route',
                                       'task_board', 'rule', 'chief')),
    rule_id                BIGINT,
    fallback_rule_id       BIGINT,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT plugin_message_handling_rule CHECK ((handler = 'rule') = (rule_id IS NOT NULL)),
    CONSTRAINT plugin_message_handling_fallback CHECK (fallback_rule_id IS NULL OR handler = 'chief')
);

ALTER TABLE public.plugin_message_handling ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

CREATE UNIQUE INDEX IF NOT EXISTS plugin_message_handling_message ON public.plugin_message_handling (owner_id, plugin_message_id);
CREATE INDEX IF NOT EXISTS plugin_message_handling_owner_at ON public.plugin_message_handling (owner_id, created_at);

ALTER TABLE public.plugin_message_handling ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.plugin_message_handling FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.plugin_message_handling;
CREATE POLICY owner_isolation ON public.plugin_message_handling
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE ON public.plugin_message_handling TO muster_app;

-- muster 0088 — AN AGENT'S OUTCOME: every started agent ends in one you can see
-- (docs/provisioner.md, "How an agent's work ends"; internal/agentoutcome; internal/api
-- agent_outcome.go and agent_watchdog.go).
--
--   agents.outcome         '' (not tracked: an agent nobody started with a first
--       turn, and every row from before this migration), 'running' (a started
--       agent working on what it was given), 'waiting' (its turn ended and it is
--       idle WITHOUT saying it finished — a question, a progress report: the
--       owner's move), 'done' (it SAID it finished: it moved its task to
--       review/complete, or called agent_report_done), 'failed' (a turn of it failed:
--       the reason is in outcome_reason and error_message, and its status is
--       `error`) or 'stuck' (it went quiet: no first reply since its kickoff, or
--       no activity for the silence threshold). The CHECK below is the closed set.
--   agents.outcome_reason  why, in muster's words: the failed turn's error, or
--       the watchdog's "no reply since kickoff" / "silent for 30 min". '' with
--       'running' and 'done'.
--   agents.outcome_at      when the outcome last CHANGED. One push per change:
--       every transition is a write conditional on the outcome it leaves, so of
--       two writers (two replicas, a turn and the sweep) exactly one applies it
--       and only that one pushes.
--   agents.activity_at     the newest thing muster SAW the agent do: a turn
--       starting or ending, a stream event of a turn, a work report from its pod.
--       It is the watchdog's evidence of life, beside agents.LastActivity's
--       transcript and row halves. Written at most every few seconds per agent.
--   agents.first_reply_at  when the agent's first reply arrived: before it, the
--       "no reply since kickoff" threshold applies; after it, the silence one.
--   agents.outcome_told_at when a 'waiting' outcome was TOLD — pushed to the
--       owner and noted to the chief, or seen by the owner opening the agent's
--       page (which then tells nothing). NULL while untold; every transition
--       clears it. A wait is told only after the waiting hold (60 s, the one hold
--       rule session state uses) and only if the owner neither replied nor
--       looked in the meantime, claimed by a write conditional on it being NULL,
--       so of two writers exactly one tells.
--   agents.dispatched_by   the chief agent that started it (agent_dispatch), so
--       its outcome is also said in that chief's chat. NULL for any other start.
--       Not a foreign key: like chat_messages.typed_by (0081) it is a pointer a
--       reader joins when it tells the chief, and a destroyed chief simply has
--       nobody to tell.
--
-- 🔴 NONE OF THESE WRITES updated_at. agents.updated_at is the row's last REAL
-- transition, compared by agents.Store.MarkRunning and MarkKickoffStuck to tell
-- whether a row moved under a verdict; an activity write every few seconds would
-- make every such verdict read "moved". The one exception is a failed turn,
-- which writes status = 'error' — a real transition — and moves updated_at with it.
--
-- 🔴 IT IS NOT A SIXTH STATUS. agents.status keeps its five values (its CHECK,
-- agents.DecideReconcile's table and every consumer key on them). `stuck` is an
-- outcome beside a status, as agents.KickoffOwed is a signal beside one: a stuck
-- agent's pod is still `running`, and recovery is new activity, not a restart.
--
-- Must apply after its predecessor (the contiguity test refuses a gap). Not
-- re-applied by db.RLSOn (it changes no policy, grant or default); the columns
-- inherit agents' row-level security. Idempotent.
--
-- ROLLBACK. An image from before 0088 never reads or writes these columns and
-- runs against this schema unchanged. To remove them:
--   ALTER TABLE agents DROP CONSTRAINT IF EXISTS agents_outcome_check;
--   ALTER TABLE agents DROP COLUMN IF EXISTS outcome, DROP COLUMN IF EXISTS outcome_reason,
--     DROP COLUMN IF EXISTS outcome_at, DROP COLUMN IF EXISTS activity_at,
--     DROP COLUMN IF EXISTS first_reply_at, DROP COLUMN IF EXISTS dispatched_by,
--     DROP COLUMN IF EXISTS outcome_told_at;
--   DELETE FROM schema_migrations WHERE version = 88;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.agents ADD COLUMN IF NOT EXISTS outcome TEXT NOT NULL DEFAULT '';
ALTER TABLE public.agents ADD COLUMN IF NOT EXISTS outcome_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE public.agents ADD COLUMN IF NOT EXISTS outcome_at TIMESTAMPTZ;
ALTER TABLE public.agents ADD COLUMN IF NOT EXISTS activity_at TIMESTAMPTZ;
ALTER TABLE public.agents ADD COLUMN IF NOT EXISTS first_reply_at TIMESTAMPTZ;
ALTER TABLE public.agents ADD COLUMN IF NOT EXISTS dispatched_by BIGINT;
ALTER TABLE public.agents ADD COLUMN IF NOT EXISTS outcome_told_at TIMESTAMPTZ;

ALTER TABLE public.agents DROP CONSTRAINT IF EXISTS agents_outcome_check;
ALTER TABLE public.agents ADD CONSTRAINT agents_outcome_check
    CHECK (outcome IN ('', 'running', 'waiting', 'done', 'failed', 'stuck'));

-- The watchdog's one read: the tracked agents still working.
CREATE INDEX IF NOT EXISTS agents_outcome_open ON public.agents (id) WHERE outcome IN ('running', 'stuck');

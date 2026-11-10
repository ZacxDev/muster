-- muster 0110 — THE OFFICER LLM PROXY'S USAGE, PER POOL (internal/llmproxy).
-- docs/officers.md "The LLM proxy".
--
--   officer_llm_usage.pool   'routine' or 'judgement': which of the officer's
--       pools a row's requests were made for. The routine pool spends the
--       officer's top-level `llm` budget and the judgement pool its own
--       (tiers.judgement.llm), and each daily budget is checked against
--       sum(cost_usd) for the officer, ITS POOL and the day — so the two never
--       mix. Every existing row is a routine instance's (the proxy served only
--       the routine pool before this), so the default backfills it 'routine'.
--   officer_llm_usage_one_row is replaced by officer_llm_usage_one_pool_row,
--       UNIQUE (owner_id, officer, pool, instance, day): judgement instance 1
--       and routine instance r1 are different instances on one day.
--   officer_llm_usage_officer_pool_day serves SpentToday.
--
-- 🔴 NUMBERS ONLY, as 0075: the new column holds a pool name.
--
-- Inherits 0075's ownership and forced row-level security; no policy change.
-- Not re-applied by db.RLSOn (re-applying 0075 creates nothing: its table
-- already exists, so its old UNIQUE is not re-added).
--
-- Must apply after 0109 (the contiguity test refuses a gap).
--
-- 🔴 AN OLDER IMAGE CANNOT BOOK USAGE OVER THIS. Its Book is
-- `ON CONFLICT (owner_id, officer, instance, day)`, which needs a unique
-- constraint on exactly those columns; with only the new one present every
-- booking it makes fails ("no unique or exclusion constraint matching") —
-- the request is still served and the spend is lost (counted on
-- muster_officer_llm_book_failures_total), so its budget under-reads.
--
-- 🔴 THIS HAPPENS ON EVERY FORWARD ROLLOUT TOO, NOT ONLY ON A ROLLBACK. The
-- new pod applies this migration at boot, while the old pods still serve
-- (a rolling update with maxUnavailable 0 keeps them until the new ones are
-- ready). From that moment until each old pod exits, every LLM request an old
-- pod proxies is served and its spend is NOT booked: each one logs
-- "usage of $… could NOT be written" and counts on
-- muster_officer_llm_book_failures_total{officer}, and the day's budget
-- under-reads by those requests for every pod. The window is the rollout's
-- own overlap (normally a pod start, minutes); the officer key's
-- provider-side credit limit still bounds it. Watch the rollout complete
-- (kubectl rollout status), then read that counter: anything it counted is
-- spend the day's budget does not know about. Keeping the old constraint
-- beside the new one is not possible: routine r1 and judgement #1 are both
-- instance 1 on one day, which the old key cannot hold.
--
-- ROLLBACK, in this order: (1) remove every `tiers.judgement.llm` from
-- MUSTER_OFFICERS (an older image refuses the unknown key and does not boot);
-- (2) run the statements below (they delete the judgement pool's usage,
-- which an older image has no budget for); (3) roll the image back.
--   DELETE FROM public.officer_llm_usage WHERE pool <> 'routine';
--   ALTER TABLE public.officer_llm_usage DROP CONSTRAINT IF EXISTS officer_llm_usage_one_pool_row;
--   ALTER TABLE public.officer_llm_usage ADD CONSTRAINT officer_llm_usage_one_row UNIQUE (owner_id, officer, instance, day);
--   DROP INDEX IF EXISTS public.officer_llm_usage_officer_pool_day;
--   ALTER TABLE public.officer_llm_usage DROP COLUMN IF EXISTS pool;
--   DELETE FROM schema_migrations WHERE version = 110;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.officer_llm_usage ADD COLUMN IF NOT EXISTS pool TEXT NOT NULL DEFAULT 'routine';
ALTER TABLE public.officer_llm_usage DROP CONSTRAINT IF EXISTS officer_llm_usage_pool_check;
ALTER TABLE public.officer_llm_usage ADD CONSTRAINT officer_llm_usage_pool_check CHECK (pool IN ('routine', 'judgement'));

ALTER TABLE public.officer_llm_usage DROP CONSTRAINT IF EXISTS officer_llm_usage_one_row;
ALTER TABLE public.officer_llm_usage DROP CONSTRAINT IF EXISTS officer_llm_usage_one_pool_row;
ALTER TABLE public.officer_llm_usage ADD CONSTRAINT officer_llm_usage_one_pool_row UNIQUE (owner_id, officer, pool, instance, day);

CREATE INDEX IF NOT EXISTS officer_llm_usage_officer_pool_day ON public.officer_llm_usage (officer, pool, day);

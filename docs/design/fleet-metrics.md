# Fleet metrics: the baseline queries

Status: saved queries, read-only. They measure the loop that exists today
(the [stall sweep](../chief-nudges.md), [session episodes](session-state.md)
and the chief's [usage rows](../usage.md)). They give the baseline that
the chief's fleet work is judged against. Run them before a change and
after it.

Each query is a plain `SELECT` with two parameters:

| parameter | type | meaning |
|---|---|---|
| `$1` | `bigint` | the owner (`users.id`) whose fleet is measured |
| `$2` | `interval` | the window, counted back from now (`'7 days'`, `'14 days'`) |

The queries hold no ids, hostnames or counts of any deployment. Every
number comes from the database they run on.

## Running them

`psql` connects as the login role, which is not under row-level security
([schema.md](../schema.md#row-level-security)), so every query filters on
`owner_id = $1` itself. Read-only, through `PREPARE`:

```
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 <<'SQL'
BEGIN READ ONLY;
PREPARE owner_wait(bigint, interval) AS <paste the query>;
EXECUTE owner_wait(42, '7 days');
ROLLBACK;
SQL
```

Each `sql` block below is one statement. `internal/db`'s
`TestTheFleetMetricQueriesRun` runs every one of them on a migrated database
over a fixed fixture, and checks each result against literal values. A query
that stops matching the schema fails CI.

## What counts as a stop

A **stop** is one `session_episodes` row: a session entered `needs_you` or
`stalled`. A **non-waiting** stop is one with `waiting = false`: the session
was idle, not held on a prompt. It ends with an `end_reason`:

- `owner`: you acted in the session;
- `viewed`: you opened its page;
- `worked`: it carried on by itself;
- `gone`: its window went away;
- `reset`: muster ended it during a rollout.

`gone` and `reset` say nothing about who resolved the stop, so the metrics
leave them out.

⚠ A `worked` end is recorded only after the session has worked continuously
for `EndAfter` (120 s, `internal/sessionstate`), so its `ended_at` comes at
least that late. An `owner` or `viewed` end is recorded at once. A median that
mixes the three is therefore slightly biased: the `worked` stops read longer
than they were.

⚠ A stop that has not ended has no wait to measure yet. Queries 1 and 2 report
the open stops beside the ended ones: their count, and for query 1 their age
so far. A median over ended stops alone reads low while long stops are still
open.

## 1. Owner wait on a stop

Minutes from a non-waiting stop opening to its end, for stops that opened in
the window and ended by `owner`, `viewed` or `worked`: `stops`, and the
median and 90th percentile. **These cover ended stops only.** Beside them,
the non-waiting stops that opened in the window and are still open:
`open_stops`, and the median and 90th percentile of their age so far.

```sql
SELECT count(*) FILTER (WHERE end_reason IN ('owner', 'viewed', 'worked')) AS stops,
       round((percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM ended_at - opened_at))
             FILTER (WHERE end_reason IN ('owner', 'viewed', 'worked')) / 60)::numeric, 1) AS median_min,
       round((percentile_cont(0.9) WITHIN GROUP (ORDER BY extract(epoch FROM ended_at - opened_at))
             FILTER (WHERE end_reason IN ('owner', 'viewed', 'worked')) / 60)::numeric, 1) AS p90_min,
       count(*) FILTER (WHERE ended_at IS NULL) AS open_stops,
       round((percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM now() - opened_at))
             FILTER (WHERE ended_at IS NULL) / 60)::numeric, 1) AS open_median_min,
       round((percentile_cont(0.9) WITHIN GROUP (ORDER BY extract(epoch FROM now() - opened_at))
             FILTER (WHERE ended_at IS NULL) / 60)::numeric, 1) AS open_p90_min
FROM session_episodes
WHERE owner_id = $1 AND opened_at >= now() - $2::interval
  AND NOT waiting AND (end_reason IN ('owner', 'viewed', 'worked') OR ended_at IS NULL)
```

The ended stops' wait, split by the UTC hour the stop opened in (ended stops
only). It shows when stops wait longest, for example while the owner is
asleep.

```sql
SELECT extract(hour FROM opened_at AT TIME ZONE 'UTC')::int AS utc_hour, count(*) AS stops,
       round((percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM ended_at - opened_at)) / 60)::numeric, 1) AS median_min
FROM session_episodes
WHERE owner_id = $1 AND opened_at >= now() - $2::interval
  AND NOT waiting AND end_reason IN ('owner', 'viewed', 'worked')
GROUP BY 1 ORDER BY 1
```

## 2. Stops resolved without the owner

Of the non-waiting stops that opened in the window and have ended (not
`gone` or `reset`): how many ended `worked`, and the share in whole percent.
The share is over **ended stops only**. `open_stops` counts the ones still
open, which may yet end either way. A stop the chief resolves with a fleet
action will count here once fleet actions exist. Until then it counts only
through `worked`.

```sql
SELECT count(*) FILTER (WHERE ended_at IS NOT NULL) AS ended,
       count(*) FILTER (WHERE end_reason = 'worked') AS without_owner,
       coalesce(round(100.0 * count(*) FILTER (WHERE end_reason = 'worked')
             / nullif(count(*) FILTER (WHERE ended_at IS NOT NULL), 0)), 0)::int AS pct,
       count(*) FILTER (WHERE ended_at IS NULL) AS open_stops
FROM session_episodes
WHERE owner_id = $1 AND opened_at >= now() - $2::interval
  AND NOT waiting AND (ended_at IS NULL OR end_reason NOT IN ('gone', 'reset'))
```

## 3. CI waits left to the owner

Stops with a stall decision in the window whose reason is about CI, joined
through `chief_nudges.episode_id`: how many there were, how many the owner
ended, and the median minutes from the decision to that end. Counting
decisions counts stops: a stop holds at most one decision (the unique index
`chief_nudges_one_per_episode`, migration `0078`), and the test pins that. A
decision taken with session state off has no stop and is not counted.

⚠ **This one is a heuristic.** "About CI" is a word match on the chief's own
one-line `reason` (`ci`, `check`, `checks`, as whole words, any case). No
column records why a session stopped. Treat the count as a sample, not an
exact figure.

```sql
SELECT count(*) AS ci_stops,
       count(*) FILTER (WHERE e.end_reason IN ('owner', 'viewed')) AS ended_by_owner,
       round((percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM e.ended_at - n.decided_at))
             FILTER (WHERE e.end_reason IN ('owner', 'viewed')) / 60)::numeric, 1) AS median_min_after_decision
FROM chief_nudges n
JOIN session_episodes e ON e.id = n.episode_id AND e.owner_id = n.owner_id
WHERE n.owner_id = $1 AND n.decided_at >= now() - $2::interval
  AND n.reason ~* '\m(ci|checks?)\M'
```

## 4. Done sessions left idle

Open stops (not ended) of mirrored sessions that reported a pull request,
with the newest pull request each one reported and how long the stop has
been open. `$2` bounds how recently the pull request was last seen.

The database does not hold a pull request's state. To count the **done**
ones, check each listed pull request's state outside the database (for
example `gh pr view <pr> --repo <repo> --json state`). Keep the rows whose
state is `MERGED` or `CLOSED`. Their count and the median `open_hours` are
the metric.

```sql
SELECT e.id AS episode, p.repo, p.pr,
       round((extract(epoch FROM now() - e.opened_at) / 3600)::numeric, 1) AS open_hours
FROM session_episodes e
JOIN LATERAL (
    SELECT repo, pr FROM session_prs
    WHERE owner_id = e.owner_id AND claude_session = e.session_key AND kind = 'gh-pr-create'
      AND last_seen >= now() - $2::interval
    ORDER BY last_seen DESC, id DESC LIMIT 1) p ON true
WHERE e.owner_id = $1 AND e.ended_at IS NULL AND e.session_kind = 'mirror'
ORDER BY e.opened_at
```

## 5. The chief's cost per day

The chief's turns per UTC day: all turns, the input-side
tokens (input + cache read + cache creation) and the output tokens. Then the
stall checks (trigger `nudge_check`, migration `0146`): how many there were,
their input-side tokens, and their share of the day's total tokens in whole
percent.

The rows are whole UTC days, starting the day after the one `now() - $2`
falls in, so the oldest row is never a partial day. The last row is today so
far.

These are token **counts**, not a cost ([usage.md](../usage.md)): a cache
read is billed far below an input token. Turns whose runtime reported no
usage (`usage_known = false`) count as turns with zero tokens, and
`unknown_turns` names how many there were.

```sql
SELECT to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS utc_day,
       count(*) AS turns,
       count(*) FILTER (WHERE NOT usage_known) AS unknown_turns,
       sum(input_tokens + cache_read_tokens + cache_creation_tokens)::bigint AS input_side,
       sum(output_tokens)::bigint AS output,
       count(*) FILTER (WHERE trigger = 'nudge_check') AS checks,
       coalesce(sum(input_tokens + cache_read_tokens + cache_creation_tokens) FILTER (WHERE trigger = 'nudge_check'), 0)::bigint AS checks_input_side,
       coalesce(round(100.0 * sum(input_tokens + output_tokens + cache_read_tokens + cache_creation_tokens) FILTER (WHERE trigger = 'nudge_check')
             / nullif(sum(input_tokens + output_tokens + cache_read_tokens + cache_creation_tokens), 0)), 0)::int AS checks_pct
FROM chief_turn_usage
WHERE owner_id = $1
  AND created_at >= (date_trunc('day', (now() - $2::interval) AT TIME ZONE 'UTC') + interval '1 day') AT TIME ZONE 'UTC'
GROUP BY 1 ORDER BY 1
```

## Not measured yet

- **Owner taps per day** (approvals, rejections, edits and undos of the
  chief's fleet proposals) and the **undo rate** of automatic actions. These
  need the fleet action log, which does not exist yet, so they have no query
  and no baseline. The phase that adds the log adds their queries here.

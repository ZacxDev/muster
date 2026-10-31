# Design: check-ins (a periodic report of every session)

**Status: proposal. Nothing here is built.** The operator settled cadence,
authoring, delivery and contents before this was written. On the draft they
then answered the seven open questions and added one requirement (a tool-less
narrative lane), and in the review's round 0 settled how that lane is made safe
(rollout discipline, no in-band capability signals) and deleted the "compare
with any earlier check-in" view. All of these are recorded under
[Operator decisions](#operator-decisions), and the design follows them. No
questions remain open.

The ask, verbatim: *"Chief occasionally checks all of the current focuses and
what each session is doing, and then creates a check-in report. This way it
becomes very easy for me to understand where everything is at a glance without
needing to check each session individually. It also gives us a historical
record of progress over time."*

Where this page says how muster works today, the code is the authority (see
[decisions.md](../decisions.md)). It cites `path:line` at commit `d55f5bd4`
(`main`, 2026-10-30). The cited files are byte-identical to the first draft's
base, `b3fef26f`.

## Summary

| | Recommendation |
|---|---|
| What a check-in is | A row in muster's own tables: a per-session **snapshot** (built in code), the **delta** against the previous full check-in (computed in code), and an optional **chief narrative** whose every item is tied to a snapshotted session by a foreign key |
| Data layer | Split `buildFleetReport` into a collector and its encoders, and build check-ins on the collector. One builder, not two |
| When | `09:00`, `13:00` and `17:00` in the owner's time zone, **every day, weekends included**; times and zone editable per owner. Plus **Check in now** and a chief tool (at most one per 5 minutes and 24 a day per owner). A scheduled run with no change records a one-line "no change" entry and makes no model call |
| Single run | `UNIQUE (owner_id, slot_at)` plus "one in flight per owner" (a partial unique index). No leader election: muster has none and runs one replica |
| Narrative | One throwaway chief turn in a **tool-less** ccd lane (the stall-check mechanism, `nudgeRoute`, plus the `X-Muster-CC-Toolless` lane mode of the separate PR #373). Strict JSON, validated in code. Its safety rests on **rollout discipline**: restart the chief onto the new image and confirm `"toolless": true` on ccd `/healthz` before turning the narrative on. Any failure publishes the code-only report |
| Delivery | A **Check-ins** page (history, open any one; each shows its changes since the previous check-in) and a push for **every check-in that changed**, scheduled or manual, whose text is code-computed counts only: `3 moved · 2 stuck · 1 needs you`. An unchanged check-in never pushes |
| Retention | **Forever.** No sweep. Snapshots are copies, so focus retention never empties a past check-in. About 50–80 MB per owner per year at 30 sessions; bounds under [Storage growth](#storage-growth) |
| Cost | No change: nothing. Full report: one chief turn on about 5–8k tokens of input, which the chief's account pays. One batched GitHub call for merged state. No transcript is read |
| Effort | Six PRs plus one prerequisite (the tool-less ccd lane, PR #373). The first is a pure refactor; check-ins without the chief are usable after the second |

## Goals

- **At a glance.** One page answers "where is everything": what needs me
  first, then what moved and what is stuck, grouped by repository.
- **Exact facts.** States, focus titles, PRs, stalls, asks and task statuses
  come from muster's rows, computed in code. The model writes commentary on
  those facts. It never supplies them.
- **A durable record.** Each check-in keeps its own copy of what it showed.
  Focus retention ([focus.md](../focus.md), "Retention") deletes the source
  rows within 7 to 30 days; a check-in stays readable after that, and is kept
  for good.
- **Cheap when idle.** A quiet afternoon costs a row, not a model call.
- **Never a new way to fail silently.** A chief that is busy, out of lanes, or
  wrong delays or drops the narrative. The report itself is still published.

## Non-goals

- **Acting on anything.** A check-in never nudges, sends or asks. Stall nudges
  ([chief-nudges.md](../chief-nudges.md)) and attention asks remain the only
  ways muster acts on a stalled session. A check-in only reports their state.
- **Reading transcripts.** A check-in reads stored focus, which the focus pass
  already summarises. It does not trigger summaries and does not open
  transcript streams.
- **Outside delivery** (mail, Discord, the fleet-report mail door) for now.
- **Other owners.** A check-in covers its owner's sessions only, under
  row-level security, like every other read here.
- **Replacing `sessions_overview` / `fleet_report`.** Those stay as the chief's
  live and window tools.

## What exists today

### Focus: the per-session summary

- A periodic pass (`MUSTER_FOCUS_INTERVAL`, default 15m,
  `internal/focus/refresher.go:65-66`) summarises each visible session's
  bounded transcript tail into a title and one to three sentences. It skips
  the summarizer when the tail's sha256 has not changed, and handles at most
  20 sessions per user per pass (`refresher.go:64`).
- History is in `agent_focus_history` (`internal/db/migrations/0022_*.sql:33`).
  A row is appended only when the title or description changes.
- **Retention is why check-ins must snapshot.** History rows older than 30
  days are deleted (`internal/focus/history.go:19`, `retention.go:95`). A
  mirrored session's focus not seen for `MUSTER_FOCUS_RETENTION` (default a
  week, `refresher.go:70`) is deleted too. A check-in that only pointed at
  those rows would lose its content within a month.
- The working directory is stored with focus (`agent_focus.cwd`, migration
  `0065`). Today only the fleet report's mail door reads it
  (`internal/api/fleet_report.go:291-302`). It is **unknown** for dispatched
  agents, Hermes windows and older hosts ([focus.md](../focus.md), "The working
  directory").
- Focus text is **untrusted**: it is a model's summary of text an outside
  party can influence ([focus.md](../focus.md), "Focus is untrusted").

### `fleet_report`: the data layer to reuse

`buildFleetReport` (`internal/api/fleet_report.go:195-438`) builds, in code
from muster's rows, every session with focus activity in a window, ended ones
included. Each session row has kind, name, host, repo (the agent's `Repo`, or
the window's repo chip), state (`waiting` / `active` / `ended`), first and last
seen, up to 20 distinct focus titles, and PRs. A coverage block names the
gaps. The output is capped at 32 KiB by dropping the oldest-active sessions
(`:426-433`), and the window is at most `focus.MaxWindowSpan`, 4 days
(`internal/focus/history_window.go:34`). It runs under the caller's principal
on tables under forced row-level security.

**What it lacks for a check-in:**

| missing | why it matters | where to get it |
|---|---|---|
| live sessions with **no focus activity** in the window | a session idle for two days, waiting on the owner, is exactly the one a check-in must show, and the window read cannot see it | `liveSessions` (`internal/api/sessions_overview.go:113`) |
| focus **descriptions** | titles alone are terse | `Focus.List` (current row) |
| **stall state** | "stuck" is half the report | the stall rule's inputs (`internal/api/chief_nudge.go:298-361`), below |
| **all needs-you items** per session | `pendingBySubject` keeps only the newest item per session (`sessions_overview.go:308-325`) | `attentionItems` (`internal/api/attention.go:512`) |
| a dispatched agent's **task status** | operator decision: dispatched agents with task status | `Agent.NoteID` (`internal/agents/store.go:95`) → the task's status (`internal/taskstatus/taskstatus.go:32-36`) |
| a dispatched agent's **PRs** | `SessionPRs` is asked about mirrored keys only (`fleet_report.go:273-287`) | no source today. Agents' PRs are a coverage gap until PR 6 attributes them by repository and branch |
| PR **merged** state | operator decision: "PRs opened/merged", shown (decision 7) | not recorded: `session_prs.kind` is `gh-pr-create` or `git-push` only (`0055_session_prs.sql:21`). One batched GitHub read per check-in; see [Merged PRs](#merged-prs) |
| **deltas** | the whole point of "since the last check-in" | new: a pure function over two snapshots |
| no cap-by-truncation | a check-in must not silently drop sessions | its own bound (200 sessions, `MaxWindowSubjects`), stated in coverage |

### Chief stall nudges: the stall rule and the throwaway lane

- A 2-minute sweep (`chief_nudge.go:85`, `:148-172`) builds a
  `chiefnudge.Session` for every mirrored Claude Code window on a host that
  reports status, and for every dispatched agent conversation
  (`:289-383`). It fills `Status`, `IdleFor`, `Shell` (background work) and
  `TurnError` (`:308-317`, `:358-361`). `chiefnudge.Exclude`
  (`internal/chiefnudge/chiefnudge.go:196-220`) then decides who is a
  candidate.
- The chief's decision is recorded in `chief_nudges` (migration `0064`) with
  outcomes `would_ask`, `asked` and others (`chiefnudge.go:58-67`), plus the
  Jev ask score.
- **The lane.** Each check is put to the chief under a per-decision session key
  (`nudgeSessionName`, `chief_nudge.go:816-819`). For a `claude-code` chief it
  runs as an explicit, **ephemeral** ccd session (`nudgeRoute`, `:845-850` →
  `withRoute`, `internal/api/session_runtime.go:80-90`), so it never lands in
  the operator's conversation with the chief. `askChiefForNudge`
  (`:861-896`) takes the chief's claim (`ClaimKickoff`, `:869`) and turn gate
  (`enterIdle`, `:880`). Refusals are typed: `LanesFull` and `not_ready` are
  retried for up to 30 minutes (`chief_nudge_retry.go:50`). Any other failure
  is recorded.
- The sweep runs each owner under `attentionPrincipal(ctx, owner)`
  (`chief_nudge.go:231`, `attention.go:239-241`). A check-in does the same.

### Attention, push, the Asks page

- `attentionItems` (`attention.go:512`) is the unified needs-you read: asks,
  held questions, waiting sessions, reply drafts and routed tasks
  (`attention.go:83-88`, `task_routing.go:69`).
- Push: `goNotify` (`internal/api/server.go:1269`) is the one sink (attention
  calls it at `attention.go:595`). Events are a closed list, each with a toggle,
  all on by default (`internal/push/events.go:3-17`). Each owner has a cap of
  30 per 10 minutes (`internal/api/push_routes.go:222-225`).
- The Asks page is a standalone document. The **Chief nudges** tab
  (`internal/api/chief_nudge_ui.go:23-27`) is the precedent for a feature page
  listing recorded decisions. ⚠ That file is outside Tailwind's content globs
  (`chief_nudge_ui.go:18-21`), so new markup must reuse emitted classes or
  live in `internal/ui`.
- The sidebar keeps its pages in a collapsed **More** (`internal/ui/components.go:1000-1014`),
  with Asks as a standalone link (`internal/ui/attention.go:68-74`).

### Periodic passes and replicas

There is **no leader election**. The background loops run ungated, and the
deployment is single-replica on purpose (`cmd/muster-server/doc_seams.go:455-477`,
`main.go:900-912`). Correctness across a rolling update's overlap comes from
row-level idempotency: per-agent claims, conditional writes, and unique keys
such as `chief_nudges_one_per_streak` (`0064_chief_nudges.sql:97`). Check-ins
use the same approach.

### Time zones

muster stores **no per-user time zone**. The only zone in the code is the
officer ship's quiet hours (`internal/officers/ship.go:383-397`).
`time/tzdata` is already linked into `muster-server` (measured: `go list -deps
./cmd/muster-server`), so `time.LoadLocation` works in a bare image.

## Recommended design

### Overview

```
scheduler tick (1m) ──┐
"Check in now" (POST)─┼─▶ claim a row (status=building)      UNIQUE(owner, slot_at)
checkin_now tool ─────┘        │                                one building per owner
                               ▼
                 collect (fleet collector + live sessions + stall + needs-you + tasks)
                               ▼
                 snapshot rows ──▶ delta vs base (last FULL check-in) ── pure code
                               │
          scheduled & empty delta? ──yes──▶ status=no_change (no snapshot copy, no model, no push)
                               │no
                               ▼
                 status=published (the code report is live now)
                               ▼
                 narrative: one ephemeral, TOOL-LESS chief lane turn ──▶ validate ──▶ checkin_items (FK)
                               │ busy/lanes full: retry ≤ 20m; error: record; gateway chief: off
                               ▼
                 changed? push once (counts only), when the narrative settles or at 20m
                 (scheduled, manual and chief alike; unchanged → no push)
```

### Data model

Migration numbers, re-verified against `origin/main` at `d55f5bd4`:
`0066_cc_account_failover.sql` is the highest on `main`. Open PR #370 adds
`0067_postgres_connector.sql`, and no other open PR or remote branch adds a
migration numbered `0067` or above. This proposes **`0068`** and **`0069`**.
Re-check both when implementing.

Both migrations follow the `0060`/`0064` shape. `owner_id` defaults to the
principal (`current_setting('muster.uid')`). `owner_isolation` is under
**FORCE** row-level security, with the `muster.scope = 'all'` escape for
system passes. Every statement is idempotent so `db.RLSOn` can re-apply it.

**`0068_checkins.sql`**

```sql
checkins (
  id               BIGSERIAL PRIMARY KEY,
  owner_id         BIGINT NOT NULL DEFAULT <principal> REFERENCES users(id),
  trigger          TEXT NOT NULL CHECK (trigger IN ('scheduled','manual','chief')),
  slot_at          TIMESTAMPTZ,          -- the scheduled instant (UTC); NULL for manual/chief
  started_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  published_at     TIMESTAMPTZ,
  status           TEXT NOT NULL CHECK (status IN ('building','published','no_change','failed')),
  base_id          BIGINT REFERENCES checkins(id) ON DELETE SET NULL,  -- the full check-in compared against
  window_since     TIMESTAMPTZ NOT NULL,
  window_until     TIMESTAMPTZ NOT NULL,
  n_sessions INT, n_moved INT, n_stuck INT, n_needs_you INT,          -- the headline, computed in code
  coverage         JSONB NOT NULL DEFAULT '{}',                       -- gaps + omitted, as fleet_report's
  narrative_state  TEXT NOT NULL DEFAULT 'none'
                   CHECK (narrative_state IN ('none','pending','ok','partial','failed','off')),
  narrative_detail TEXT,                -- a fixed reason, never model or provider text
  narrative_model  TEXT,
  narrative_dropped INT NOT NULL DEFAULT 0,  -- items refused by validation
  pushed_at        TIMESTAMPTZ,
  CONSTRAINT checkins_one_per_slot UNIQUE (owner_id, slot_at)
);
CREATE UNIQUE INDEX checkins_one_building ON checkins (owner_id) WHERE status = 'building';
CREATE INDEX checkins_owner_recent ON checkins (owner_id, started_at DESC);

checkin_sessions (                       -- the snapshot: a COPY, never a reference to focus rows
  checkin_id   BIGINT NOT NULL REFERENCES checkins(id) ON DELETE CASCADE,
  owner_id     BIGINT NOT NULL DEFAULT <principal>,
  subject      TEXT NOT NULL,            -- 'agent:<id>' | 'mirror:<key>' (focus.Subject.String)
  kind         TEXT NOT NULL,            -- agent | mirror
  name         TEXT NOT NULL,
  host         TEXT NOT NULL DEFAULT '',
  repo         TEXT NOT NULL DEFAULT '', -- the group label
  repo_source  TEXT NOT NULL CHECK (repo_source IN ('agent','chip','cwd','none')),
  state        TEXT NOT NULL CHECK (state IN ('active','waiting','ended')),
  stall        TEXT CHECK (stall IN ('idle','turn_error')),   -- NULL: not stalled
  stalled_on_you BOOLEAN NOT NULL DEFAULT false,
  idle_since   TIMESTAMPTZ,
  focus_title  TEXT, focus_description TEXT, focus_at TIMESTAMPTZ,
  focus_titles JSONB NOT NULL DEFAULT '[]',  -- the window's distinct titles with times (≤ 20)
  prs          JSONB NOT NULL DEFAULT '[]',  -- [{ref:"owner/repo#12", state:"open"|"merged"|"closed"|"unknown", merged_at?}]
  task         JSONB,                        -- {id, title, status} for a dispatched agent
  needs_you    JSONB NOT NULL DEFAULT '[]',  -- [{kind, since, text≤200, key}], ≤ 10 (the rest counted in coverage)
  delta        JSONB NOT NULL DEFAULT '{}',  -- the flags below, vs base
  first_seen   TIMESTAMPTZ, last_seen TIMESTAMPTZ,
  PRIMARY KEY (checkin_id, subject)
);

checkin_items (                          -- the chief's narrative, one claim per row
  checkin_id BIGINT NOT NULL,
  ord        SMALLINT NOT NULL,
  section    TEXT NOT NULL CHECK (section IN ('needs_you','moved','stuck')),
  subject    TEXT NOT NULL,
  text       TEXT NOT NULL CHECK (char_length(text) BETWEEN 1 AND 280),
  owner_id   BIGINT NOT NULL DEFAULT <principal>,
  PRIMARY KEY (checkin_id, ord),
  FOREIGN KEY (checkin_id, subject) REFERENCES checkin_sessions (checkin_id, subject) ON DELETE CASCADE
);
```

🔴 **The composite foreign key is the structural guarantee.** A narrative item
naming a session that is not in that check-in's snapshot cannot be inserted.
This does not depend on matching words; the database refuses the row.

**`0069_checkin_preferences.sql`**: one row per owner, in the shape of
`chief_preferences` (`0060`): `enabled BOOLEAN`, `tz TEXT` (an IANA name,
validated by `time.LoadLocation` on write), `times TEXT[]` (`HH:MM`, 1 to 6
entries, sorted, distinct; default `{09:00,13:00,17:00}`), `updated_at`. The
times apply **every day of the week**. There is no weekday mask (decision 1);
an owner who wants quiet weekends turns check-ins off. **No row means off**, so
owners opt in. That is the rollout gate.

Every `owner_id` here is `REFERENCES users(id)` with no `ON DELETE` action,
the same as every other owner table (`0060`, `0064`). See
[Accounts disabled or deleted](#accounts-disabled-or-deleted).

### Retention: forever (decision 5)

**No sweep.** Check-ins, their snapshots and their narrative items are kept
until the owner's account is purged by hand (below). There is no
`MUSTER_CHECKIN_RETENTION`. `base_id` keeps `ON DELETE SET NULL` so that a
purge, or a later compaction, can delete a base without failing; a dependant's
stored delta stays.

**The trade-off, accepted by the operator.** Snapshots copy focus
descriptions, which are model summaries of transcripts and can hold anything a
session wrote. Keeping them forever means that text outlives the focus
feature's own 30-day bound ([focus.md](../focus.md), "Retention"). It stays
owner-scoped under FORCE row-level security, readable only by its owner and by
system passes.

#### Storage growth

What bounds it, per owner:

| factor | bound | where it comes from |
|---|---|---|
| snapshot row | typically about 1–2 KB (an estimate; **measure it in PR 2** on a real fleet); at most about 10 KB when every capped field is full | title ≤ 300 runes and description ≤ 600 (`internal/focus/focus.go:56-58`), ≤ 20 window titles of ≤ 300 each, ≤ 10 needs-you items of ≤ 200 characters, PR refs, about 300 B of fixed columns |
| sessions per check-in | typically about 30; at most 200 | `MaxWindowSubjects` (`internal/focus/history_window.go:37`) |
| full check-ins per day | 3 scheduled, plus manual/chief ones | the schedule (decision 1); manual and chief are limited to one per 5 minutes and **24 per owner per day** |
| no-change entries | about 0.3 KB each, no snapshot | one `checkins` row |

**Typical estimate (30 sessions, 1.5 KB a row):** 45 KB per full check-in.
Three a day, every day, all changed: about 135 KB a day, or **about 50 MB a
year**. With two manual check-ins a day as well: about 225 KB a day, or **about
80 MB a year**. Ten years is roughly 0.5–0.8 GB per owner. No-change entries
add under 0.5 MB a year.

**Ceiling (every field full, 200 sessions):** 2 MB per check-in. Scheduled
only, that is 6 MB a day, about 2.2 GB a year. With the manual cap used every
day, 27 check-ins a day, it is about 54 MB a day, about 20 GB a year. That
ceiling is why manual and chief check-ins get a daily cap and not only the
5-minute limit: without it, a chief in a loop could take 288 a day. Postgres
compresses large `JSONB` and `TEXT` values (TOAST), but these numbers do not
count on it.

**A later compaction option (noted, not proposed for v1).** If the table
grows past what the operator wants to keep, or if keeping old focus text
forever stops being acceptable, a sweep could **thin** snapshots older than N
days: clear `focus_description`, `focus_titles` and needs-you text, and keep
the structural columns (state, stall, delta flags, PR refs, task status, focus
title). That keeps the record of what moved and what was stuck at roughly a
quarter of the size, and the delta stays readable because it is stored. A
second option is exporting old months as JSON to object storage and deleting
the rows. Neither is built until one of those conditions is real.

### The builder (one collector, two consumers)

**PR 1 (refactor, no behaviour change).** Split `buildFleetReport` into:

- `collectFleet(ctx, exclude, since, until, opts) (fleetCollected, error)`:
  lines `:197-387` today. It returns typed rows (subject, kind, name, host,
  repo, repo source, state, first/last seen, titles, PRs, cwd) and gaps.
- the encoders: the tool's JSON (32 KiB cap) and the mail door's narrowing
  (path filter **before** cap, `:200-206`). These do not change.

The existing `fleet_report_pg_test.go` (549 lines) must stay green
**unchanged**. That is how we show PR 1 changed no behaviour.

**The check-in builder** (`internal/api/checkin_build.go`, under the owner's
principal) calls `collectFleet` with `[base.published_at, now]`, or
`[now-24h, now]` for the first, clamped to `MaxWindowSpan`. It then adds:

1. **Live sessions without focus activity**: the union with `liveSessions`.
   Their focus comes from `Focus.List` (the current row), which may be older
   than the window.
2. **Descriptions and `focus_at`** from `Focus.List`. A row's `focus_at` lets
   the page show "focus 3 h old" when the summariser has not caught up (≤ 20
   sessions per user per pass).
3. **Stall**, from one extracted rule. Move the per-session status assembly in
   `nudgeCandidates` (`chief_nudge.go:298-361`) into
   `stallInputs(ctx) map[focus.Subject]chiefnudge.Session`. The sweep and the
   builder then read the same facts (one rule, one place). Then:
   - `stall = 'turn_error'` when `TurnError` is set (usage limit, auth, API
     error);
   - `stall = 'idle'` when `Status == "idle"`, `IdleFor >= MUSTER_CHIEF_NUDGE_IDLE`
     (read even when nudges are off; default 10m), and no `Shell`;
   - `stalled_on_you` when the newest `chief_nudges` row for the session's
     **current** streak has outcome `would_ask` or `asked`, or a Jev ask score
     at or above the threshold. With nudges off, this is always false. The page
     then says "stall classification is off", rather than implying nobody is
     waiting.
4. **Needs-you**: every `attentionItems` entry tied to the session, plus
   shadow-mode `would_ask` decisions (live ones are already asks). Session-less
   items (reply drafts) become one owner-level count linking to
   `/attention?tab=drafts`. Text is cut to 200 characters with
   `redact.Secrets`. At most 10 items per session are copied, newest first;
   the rest are counted in `coverage`.
5. **Task** for a dispatched agent: `NoteID` → `{id, title, status}`.
6. **Repo grouping**, first match wins:

   | source | label | `repo_source` |
   |---|---|---|
   | the agent's `Repo` (`owner/name`) | `name` | `agent` |
   | the window's repo chip (`mirror.SessionView.Repo`, `internal/mirror/order.go:67-69`) | as given | `chip` |
   | basename of the stored `cwd` (`focus.CleanCwd`) | basename | `cwd` |
   | none of these | **Ungrouped** | `none` |

   The label is copied into the snapshot, so a session ended last month keeps
   its group after its focus row and hub entry are gone. Agent labels drop the
   `owner/` prefix to match the chip's short name. **Known limit, accepted
   (decision 2):** two repositories with the same short name (`alice/api` and
   `bob/api`) share one group, `api`. Keeping `owner/name` for agents would
   instead split one repository across two groups, because a mirrored window's
   chip carries only the short name. See [Risks](#risks).
7. **The chief itself is excluded**, as in `fleet_report`. **Officers are
   shown** (decision 6), as rows marked as officers. They are never marked
   "stalled on you" and never counted in "needs you" for a stall: officers
   escalate through HQ ([chief-nudges.md](../chief-nudges.md)). An officer's
   own `attentionItems` entry, if it has one, is still listed, because that
   is a real ask.

**Bound, not truncation.** The snapshot holds at most 200 sessions
(`MaxWindowSubjects`), most recently active first, with `coverage.omitted`
counted and shown. Unlike the tool, nothing is dropped to fit a byte budget.
The byte budget applies only to the **narrative's input** (below).

### The delta and the "no change" rule

`checkins.Delta(base, cur []Snapshot) []SessionDelta` is a pure function,
table-tested. Per subject it computes these flags:

| flag | true when |
|---|---|
| `new` | in `cur`, not in `base` |
| `ended` | `base.state ∈ {active, waiting}` and (`cur.state = ended`, or absent from `cur` and not live) |
| `focus_changed` | `cur.focus_title ≠ base.focus_title` (exact compare of the cleaned title) |
| `newly_stalled` / `unstalled` | `stall` went from NULL to non-NULL, or back |
| `newly_needs_you` / `needs_you_cleared` | the set of needs-you item **keys** grew, or shrank to empty |
| `state_changed` | `active ↔ waiting` (ended is covered above) |
| `pr_opened` | a PR ref in `cur.prs` not in `base.prs` |
| `pr_merged` | a ref's state became `merged` (read once per check-in; see [Merged PRs](#merged-prs)). A ref whose state is `unknown` sets no flag |
| `task_changed` | the agent's task status differs |

A check-in has **changed** exactly when any session has any flag set.
**Deliberately not counted:** a description-only rewrite (decision 4: the
summariser rephrases, so a new description under the same title is not news),
`last_seen` or `focus_at` moving, a stale focus or focus problem, a PR's state
going from known to `unknown` (a failed GitHub read), and a host going offline
(its sessions' liveness becomes unknown; it is shown as a coverage gap, not as
"ended").

- **Base** = the owner's newest check-in with `status = 'published'`. A
  no-change entry stores no snapshot and records the `base_id` it matched, so
  every delta compares two real snapshots. This also catches a slow drift that
  stays under the per-run threshold across several runs.
- **Scheduled + unchanged** → `status = 'no_change'`. No snapshot copy, no
  model call, no push.
- **Manual or chief + unchanged** → a full report anyway (the owner asked),
  with "No change since 13:00" at the top, and **no push** (see [Push](#push)).

**The headline is code:** `moved` = sessions with any of `new`,
`focus_changed`, `pr_opened`, `pr_merged`, `task_changed` or `ended`;
`stuck` = `stall IS NOT NULL`; `needs you` = sessions with needs-you items,
plus 1 if there are owner-level drafts. A session counts in every column it
qualifies for, and the page says so.

### The scheduler

`RunCheckinSweep` is a 1-minute ticker started in `startBackgroundLoops`. It
does nothing unless `MUSTER_CHECKINS=1`. On each tick, for each owner with
`enabled` preferences whose account is **not disabled** (`users.disabled_at IS
NULL`, the filter focus's `Owners` uses, `internal/focus/pgstore.go:369`):

1. `slot` = the newest configured `HH:MM` in the owner's zone at or before now
   (`time.Date(y, m, d, hh, mm, 0, 0, loc)`, then UTC). Every calendar day
   has the same slots; there is no weekday check (decision 1).
2. If `now - slot ≤ 2h` (the **catch-up window**) and no row exists for
   `(owner, slot)`, `INSERT … status='building'`. A unique violation means
   another tick or pod has it, so skip.
3. Build, publish, then hand off to the narrative step.

**Restarts and missed runs.** A restart within 2 h of a slot runs it late; the
row keeps `slot_at` and the page shows "scheduled 13:00, ran 13:47". Past 2 h
the slot is skipped. It is not backfilled: a snapshot taken now cannot say what
was true at 13:00, and a pile of catch-up reports would bury the real one.
Only the newest missed slot is considered, so a day of downtime yields at most
one late check-in, not six.

**Single run across replicas and overlap.** There is no leader. A slot is
claimed with `checkins_one_per_slot`, and at most one check-in per owner can be
`building` (`checkins_one_building`). That also stops a manual click during a
scheduled run. A `building` row left by a crash is cleared after 15 min:
`status='failed'`, `narrative_detail='interrupted'`. The next slot or click
then proceeds.

**DST.** `time.Date` normalises a spring-forward gap (`02:30` that does not
exist becomes `03:30`). On a fall-back repeat it picks the first instant. The
unique key on the UTC instant means one run either way.

**Manual and chief triggers** insert with `slot_at NULL`. They are refused when
one is building (`409`, "a check-in is already being written") and limited to
**one per 5 minutes** and **24 per day per owner** (the owner's local day),
both counted over manual and chief rows by `started_at` (`429` with the time
the next one is allowed). Scheduled rows do not count against either limit.
The daily cap bounds storage and pushes (both now follow manual check-ins) and
stops a looping chief: see [Storage growth](#storage-growth) and
[Push](#push).

#### Accounts disabled or deleted

- **Disabled** (`users.disabled_at`, `SetDisabledAs`,
  `internal/users/users.go:240-272`): the sweep skips the owner, so no
  scheduled check-ins are written. Disabling also stamps
  `sessions_not_before`, so the owner cannot sign in to read them. **Rows are
  kept**, preferences included. Re-enabling resumes at the
  next slot. Missed slots are not backfilled. The first check-in after a long
  gap compares with the old base: its delta is right, and its activity window
  is clamped (see [Risks](#risks)).
- **Deleted.** muster has no account-deletion path: no non-test code deletes
  from `users` (the same search finds the `devices` and `push` deletes, as a
  control). An operator purging an account by hand must delete the owner's
  `checkins` rows first (snapshots and items cascade) and its
  `checkin_preferences` row, under the system scope. Until they do, the
  foreign key refuses the `users` delete, the same as for every other owner
  table. Because retention is forever, this purge is the **only** way an
  owner's check-ins are removed.

### The chief narrative

**Optional by design.** It runs when the owner's chief is on (`chiefOn`),
`MUSTER_CHECKIN_NARRATIVE` is not `0`, and the chief is a `claude-code` chief
(below). Otherwise `narrative_state='off'` and the report is complete without
it.

**🔴 Hard dependency: the narrative lane MUST be tool-less** (operator
requirement, decision 8). The turn's input is focus text, which an outside
party can influence. At `d55f5bd4` a `claude-code` lane runs with the chief's
full tool set, because ccd has no header to restrict tools
(`internal/agents/turnroute.go:20-47` lists every header it has). Fencing
labels the text; it does not stop an injected instruction from calling a tool.
So:

- **The lane mode is PR #373** (separate, open, the prerequisite "P"). On a
  check route muster's one route builder, `withRoute`, sends
  `X-Muster-CC-Toolless: 1` together with `X-Muster-CC-Ephemeral: 1`, and ccd
  starts that lane's CLI with `--tools= --strict-mcp-config`: no built-in tools
  and no MCP servers. The mode is fixed for the lane's life; a turn whose
  header disagrees with its lane is refused (`400`). ccd's `/healthz` lists
  such a lane with `"toolless": true` while it runs. The narrative uses
  `nudgeRoute` like a stall check, so it gets the header with no code of its
  own.
- **The safety rests on rollout discipline, not on an in-band check**
  (decision Q1, round 0). muster does **not** ask ccd whether it supports the
  mode before the turn, and does **not** look for a confirmation in the
  answer. Instead, after the deploy that carries #373: restart the chief onto
  the new `claude-code-agent` image (the `chief_restart` MCP tool, muster
  #369, does this), then confirm `"toolless": true` on ccd `/healthz` while a
  check lane runs, and only then turn the narrative on (see
  [Rollout](#configuration)). This is the same discipline stall checks
  follow.
- 🔴 **Residual risk, stated plainly.** An older ccd **ignores**
  `X-Muster-CC-Toolless` (`turnroute.go:41-46` shows how it treats a header it
  does not know), so if the narrative is turned on while the chief still runs
  a pre-#373 image, the narrative lane **keeps every tool**, and muster does
  not detect it. Nothing in muster stops that; only the rollout steps do. The
  window is the time between turning the narrative on and the chief being
  restarted, and the narrative flag defaults to on once the feature is on, so
  the rollout keeps it off (`MUSTER_CHECKIN_NARRATIVE=0`) until the `/healthz`
  check has passed. Stall checks carry the same exposure today, from more
  text.
- **A `gateway` chief.** Its turns already carry no tools in the request
  (`agents.RunToollessTurn` via `Gateway.Send`,
  `internal/agentgateway/agentgateway.go:291`). Whether that runtime can still
  reach tools of its own is **not established** here. Until it is, a
  `gateway` chief gets `narrative_state='off'`, `narrative_detail='gateway_chief'`,
  and code-only check-ins. This is a check on the chief's runtime kind, which
  muster already knows, not a capability query.

**Mechanism: the stall check's, plus the tool-less mode.**

- Session key `agent:<chief>:webchat:checkin:<hash(owner, checkin id)>`. That
  is fresh per check-in and never the operator's thread.
- `nudgeRoute`, so a `claude-code` chief runs it in an **ephemeral** ccd lane
  and it never appears in the operator's conversation, with the tool-less
  header set by the same route builder.
  `askChiefForNudge`'s claim and turn gate (generalised to `askChiefThrowaway`,
  shared by both features). Optionally pin a cheaper model through
  `ChatSession.Model` → `X-Muster-CC-Model` (`session_runtime.go:85-87`) with
  `MUSTER_CHECKIN_MODEL`.
- **Busy / lanes full / not ready** (`errNudgeChiefBusy`, `LanesFull`,
  `not_ready`): retried on later ticks for **20 minutes**. This is shorter than
  the stall check's 30 minutes, because a report is time-sensitive. After that
  it is recorded as `narrative_state='failed'`, `narrative_detail='lanes_full'`
  / `'chief_busy'` / `'chief_not_ready'`. Any other error →
  `'chief_error'` after one try. Budget per call: 4 minutes
  (`chiefNudgeTurnBudget`).
- 🔴 `checkin_now` called **from the chief's own turn** cannot get the turn
  gate until that turn ends. The tool returns the code report's id at once,
  and the narrative (and the push, if the check-in changed) arrives after the
  chief's turn.

**Prompt inputs** (fenced as untrusted with an `internal/fence` nonce, as
`chiefnudge.Prompt` does, `internal/chiefnudge/prompt.go:31`):

- for each session, in the snapshot's order: `subject` id, name, kind, repo
  label, state, stall, `stalled_on_you`, focus title, description (≤ 300
  characters), the delta flags, PR refs, task status, and needs-you item kinds
  with their first 120 characters;
- the code headline counts;
- **no hosts, no transcripts, no messages.** Everything is passed through
  `redact.Secrets`.

**Size bound.** The prompt is at most **16 KiB**. Past that, sessions with no
delta, no stall and no needs-you are dropped first, as `"…and 14 unchanged
sessions"`. The code table still shows them. The model is told that it may
write at most 12 items.

**Output schema** (strict JSON, the answer's only content):

```json
{"items": [
  {"section": "needs_you", "session": "mirror:5f1c…", "text": "Waiting for you to pick the migration number before it can open the PR."},
  {"section": "moved",     "session": "agent:41",     "text": "Opened ZacxDev/muster#371 for the connector docs."},
  {"section": "stuck",     "session": "mirror:9a0e…", "text": "Hit the usage limit 2 h ago; nothing since."}
]}
```

**Validation (code, item by item).** A failing item is **dropped**, not
repaired, and the drops are counted in `narrative_dropped`:

1. The whole answer parses as this object (`DisallowUnknownFields`; any other
   text is a parse failure → `narrative_state='failed'`, `'unusable'`).
2. `section` is one of the three.
3. `session` is a subject **in this check-in's snapshot**. The insert's foreign
   key enforces this again.
4. **The section must agree with the code's facts.** `needs_you` requires that
   session to have needs-you items. `stuck` requires `stall` or `stalled_on_you`.
   `moved` requires a "moved" delta flag. So the narrative can comment on the
   table but cannot contradict it, and cannot call a session stuck when the
   code says it is not.
5. `text` is one line, 1–280 characters after `NormalizeAgentText`, with no
   hidden characters (`flows.HiddenCharRefusal`). It may not contain another
   session's **id** (an `agent:`/`mirror:` token other than its own). This is a
   check on muster's id grammar, not on words: the text cannot claim to speak
   for another session.
6. At most 12 items, at most one per (section, session).

All items survive → `ok`. Some → `partial`. None → `failed` (`'no_valid_items'`).

**What validation cannot do.** It cannot tell whether a sentence is a *true*
reading of the facts it is anchored to. A `moved` item about session X can
still misdescribe X's change. The page always shows the code row next to the
narrative item, so a reader can check one against the other.

**Failure is never a missing report.** The code report was published before
the narrative began. A failed narrative shows one muted line ("No summary from
your chief: its lanes were full") and the rest of the page is unchanged.

### The page

**`/checkins`**: a standalone document, like `/attention`. A sidebar link sits
in **More** under Asks (`components.go:1000-1014`), drawn when the feature is
on and the owner has a preferences row. A `Features.CheckIns` flag follows the
`attentionLink` precedent.

- **List, newest first**, 30 per page with **Load older**. A full check-in is a
  card with time, trigger (`scheduled` / `late` / `manual` / `chief`) and the
  headline. Consecutive no-change entries collapse into one line ("No change ·
  13:00, 17:00").
- **Check in now** button: `POST /checkins/now`, session-authenticated and
  same-origin like `/account/push/test`. It redirects to the new check-in,
  which shows "Writing summary…" until the narrative settles. It uses the
  existing SSE stream with a new event, `checkins.changed`.
- **Schedule** (a `<details>` on the page): on/off, time zone (a datalist of
  IANA names, with the browser's `Intl` zone pre-filled), and times
  (pre-filled `09:00`, `13:00`, `17:00`; 1 to 6). The form says "every day,
  weekends included", and offers no weekday mask.
- A manual check-in refused by the limits shows when the next one is allowed
  ("24 manual check-ins today; the next is allowed at 00:00").

**`/checkins/{id}`**, top to bottom:

1. **Needs you**: each item with its session, what it is waiting on and since
   when, and a link to answer (`/attention/open/<key>`). Owner-level drafts
   count.
2. **Summary** (the narrative): *Moved*, *Stuck*, each item linked to its
   session row below and to the session itself.
3. **Sessions, grouped by repo.** Groups are ordered by most recent activity,
   with Ungrouped last. A group label shared by two repositories with one
   short name is shown as one group (the known limit). Officers carry an
   `officer` badge and never a "stalled on you" badge. Each session shows
   name, kind, state, stall badge, focus title (the description in a `<details>`), delta badges (`new`,
   `ended`, `focus changed`, `newly stalled`, `PR opened`, `merged`, `task →
   ready_for_review`), PR links, task chip and `focus 3 h old` when stale.
   An ended or deleted session shows its snapshot and no link.
4. **Coverage**: omitted sessions and gaps (offline hosts, focus off, agents'
   PRs unknown, PR states unknown when the GitHub read failed, needs-you items
   beyond 10, narrative off for a `gateway` chief).

**Changes are always against the previous check-in** (decision D2, round 0).
Each report shows the delta stored with it, computed against its base, the
previous full check-in. There is no "compare with an earlier check-in" select
and no `?compare=` parameter; to see an older state, open that check-in.

**Phone width.** Below `sm`, each session is a card, not a table row. The
needs-you list comes first, so it is above the fold. Every control is at least
44 px tall, matching the sidebar's `min-h-[44px]`. Built in `internal/ui`, so
Tailwind sees the classes. Playwright covers the 390 px layout (below).

### Push

- A new event, `checkin`, in `push.Events`: "A check-in report is ready". It is
  on by default, like every event (operator decision recorded at
  `events.go:3-8`), and has its own toggle on `/account`.
- **Which check-ins push (decision 3):** every check-in that **changed**,
  whatever its trigger: scheduled, **Check in now**, or the chief's
  `checkin_now`. A check-in that did not change never pushes: a scheduled
  no-change entry, and equally a manual or chief check-in whose delta is empty
  (it still gets its full report page). The rule is one predicate,
  `changed`, the same one that decides `no_change`, so there is no
  per-trigger push logic.
- **Text is code-only:** title `Check-in · 13:00` (the slot for a scheduled
  one, the publish time with `· now` for manual and chief ones), body
  `3 moved · 2 stuck · 1 needs you`, zero terms omitted. A check-in can change
  with all three at zero (a session only came unstuck, or an ask was
  answered), so the body then falls back to the number of sessions with any
  delta flag, `2 sessions changed`. It is never empty. The body
  never contains focus or narrative text, the same policy as the `plugin`
  event. Tag `checkin`, so a newer one replaces the older one on the phone.
  Tapping opens `/checkins/<id>`.
- **Once per check-in**, recorded in `pushed_at`, so a restart does not send it
  twice. It is sent when the narrative settles or at the 20-minute bound,
  whichever comes first; with the narrative off (or not yet built), at
  publish. It goes through `goNotify`, so the per-owner cap
  applies.
- **No quiet-hours machinery:** the scheduled times are the owner's own
  choice, and a manual check-in is the owner's own action (or the chief's,
  under the daily cap).

**Rate limits and dedupe, re-checked now that manual check-ins push.** A burst
of manual check-ins cannot become a burst of pushes:

| guard | what it bounds |
|---|---|
| one building per owner (`checkins_one_building`) | a double click or a click during a scheduled run makes no second check-in (`409`) |
| one manual/chief check-in per 5 minutes | at most 2 manual pushes in any 10 minutes, plus a scheduled one: 3 of the owner's 30-per-10-minutes push cap (`push_routes.go:222-225`), so check-ins cannot starve other events |
| 24 manual/chief per day | at most 27 check-in pushes a day, even from a chief calling `checkin_now` in a loop |
| `changed` vs the newest published check-in | each manual check-in becomes the next one's base. A second click minutes later usually finds nothing new and does not push. A push means "something changed since the last report", not "you clicked" |
| `pushed_at` | one push per check-in across restarts |
| tag `checkin` | a newer push replaces the older one on the phone, so even allowed pushes never stack |

**Why no time-window dedupe beyond that** (for example "skip the push if one
went out in the last 15 minutes"): the operator asked for manual check-ins to
push, and a changed check-in is by definition news the last push did not
carry. The tag already collapses them on the phone.

### Chief tools

Registered `chiefOnly`, on the gateway and `/mcp`, beside `fleet_report`
(`internal/api/agent.go:377`, dispatch at `:710`):

| tool | what |
|---|---|
| `checkin_now()` | starts a manual check-in (trigger `chief`), returns `{id, url, status}`; the same 5-minute and 24-a-day limits and one-building rule; its narrative follows the chief's current turn; it pushes like a click on **Check in now** (only if it changed) |
| `checkins_list(since?, until?, limit?)` | headlines of check-ins in a window (no span cap: they are small), newest first, ≤ 50 |
| `checkin_get(id)` | one check-in's code table, its stored delta (against the previous check-in) and narrative, capped at 32 KiB. Narrative and focus text are labelled `untrusted` (`focusUntrustedTail`) |

These give the chief history that focus retention would otherwise erase: "what
was the connector work's state last Tuesday" can be answered years later,
since check-ins are kept for good.

### Merged PRs

Shown (decision 7), at the cost of **one GitHub call per check-in**:

- **Which refs.** The PR refs in the new snapshot whose state is not already
  terminal. `merged` and `closed` are carried forward from the base snapshot
  and never read again, so the call shrinks as PRs land.
- **One call.** One GraphQL query with one alias per ref
  (`repository(owner:, name:) { pullRequest(number:) { state mergedAt } }`),
  at most 100 refs, made with the GitHub App's installation token. muster has
  no GraphQL client today (`internal/githubapp/app.go:423` mentions `/graphql`
  only as a path that names no repository), so this is new code. Refs beyond
  100, or in a repository the installation cannot read, get `state:"unknown"`
  and a coverage line. An installation token reads one installation's
  repositories, so refs spread over several installations would need one query
  each. That is one call in the usual single-installation case; the design
  keeps the bound at one by default and lists the other installations' refs as
  `unknown`.
- **Failure is never a missing report.** A failed or slow call (10 s budget)
  leaves every unread ref `unknown`. An `unknown` sets no delta flag in either
  direction, so a GitHub outage cannot make a check-in "changed" or push.
- Agents' PRs: attributed by their repository and branch, as the first draft
  proposed, so they stop being a coverage gap.

### Configuration

| variable | default | what |
|---|---|---|
| `MUSTER_CHECKINS` | off | `1` runs the sweep and draws the page. **Kill switch**: unset it and restart. Building rows left behind are cleared as `interrupted` on the next boot with it on |
| `MUSTER_CHECKIN_NARRATIVE` | on | `0` publishes code-only reports and never calls the chief |
| `MUSTER_CHECKIN_MODEL` | (pod default) | the model a `claude-code` chief's lane uses for the narrative |
| `MUSTER_CHECKIN_DEFAULT_TIMES` | `09:00,13:00,17:00` | pre-filled for an owner turning check-ins on; applies every day |
| `MUSTER_CHECKIN_MANUAL_PER_DAY` | `24` | the daily cap on manual and chief check-ins per owner |

There is deliberately no retention variable: check-ins are kept forever
(decision 5).

All of these are read at boot and listed in `docs/configuration.md`
(`internal/modulegate/envdoc_test.go` fails otherwise).

**Rollout.** There is no shadow mode, because a check-in takes no action. The
staged gates are: server flag → per-owner opt-in (preferences row) → narrative
flag. The narrative's gate is **rollout discipline** (decision Q1):

1. Deploy muster with check-ins on and `MUSTER_CHECKIN_NARRATIVE=0`.
2. Restart the chief onto the `claude-code-agent` image that carries #373
   (the `chief_restart` MCP tool, muster #369).
3. Confirm `"toolless": true` on ccd `/healthz` for a check lane while one
   runs (a stall check uses the same lane mode).
4. Only then set `MUSTER_CHECKIN_NARRATIVE` back to on.

An older ccd ignores both the ephemeral mark (it can evict an operator lane,
[chief-nudges.md](../chief-nudges.md), "Rolling out") and the tool-less header,
in which case the narrative lane keeps its tools. muster does not detect that;
steps 2 and 3 are what prevent it. See the residual risk under
[The chief narrative](#the-chief-narrative).

### Privacy: what leaves muster

| data | stays in muster | sent to the chief's provider (Anthropic for `claude-code`, OpenRouter for `gateway`) |
|---|---|---|
| snapshot rows (names, hosts, repos, focus, PRs, tasks, asks) | yes, owner-scoped, **kept forever** (until a manual account purge) | names, repo labels, focus titles and descriptions (≤ 300), states, PR refs, task status, ask text (≤ 120), redacted |
| hosts, cwd | yes | **never** |
| transcripts, messages, tool output | not read | **never** |
| the push | | counts only, to the owner's push services |
| PR refs | yes | `owner/repo#n` only, to GitHub, to read merged state |

The chief's account pays. No OpenRouter key is used, except for a `gateway`
chief, which already runs on its owner's key. The focus summariser's calls do
not change. Compared with a stall check, a check-in sends **less**: summaries
the chief can already read with `sessions_overview`, and no transcript text.

### Security and ownership

- Every table is owner-scoped under FORCE row-level security. The sweep runs
  each owner under `attentionPrincipal`. Routes read under the request's
  principal and answer 404 for another owner's id, as `GET /ui/focus/history`
  does.
- **Injection.** Focus text can carry an outside party's words. Inside the
  narrative call it is fenced and labelled. The output can only anchor short
  text to an existing session in an agreeing section, and is rendered escaped.
  🔴 **What fencing does not stop, and what does:** fencing cannot stop an
  injected focus from asking the chief to call a tool. Today a `claude-code`
  lane runs with the chief's full tool set. So the narrative runs in a
  tool-less lane (PR #373), and the rollout confirms on ccd `/healthz` that
  the chief's ccd honours the mode before the narrative is turned on
  ([The chief narrative](#the-chief-narrative)). With a tool-less lane, the
  worst an injection can do is write a wrong sentence, which validation
  confines to an existing session in a section the code agrees with. 🔴 If the
  rollout steps are skipped and the chief runs a pre-#373 ccd, the header is
  ignored and the lane has tools; muster does not detect this. Stall checks
  have the same exposure, from more text (they read transcripts), and the
  same lane mode closes it for them.
- **Manual triggers** (`POST /checkins/now`, `checkin_now`) are capped per
  owner per day, so an injected chief cannot turn check-ins into a push or
  storage flood.
- `POST /checkins/now` is a session route. It is not a machine route, and the
  rate limit is per owner.

### Cost per day

| | per day (3 slots every day, 2 manual) |
|---|---|
| scheduled, unchanged | 0 model calls, 0 GitHub calls |
| full report | 1 chief turn: about 4–5k tokens of fenced input (16 KiB cap) + about 1k of instructions; output ≤ about 1k. Plus 1 GitHub GraphQL call when the snapshot has non-terminal PR refs |
| lane overhead (`claude-code`) | the CLI's own system prompt on each lane start (unmeasured here; stall checks already pay it; a tool-less lane should carry fewer tool definitions, also unmeasured), plus 250–550 MiB for the few minutes the lane lives ([chief-nudges.md](../chief-nudges.md), "Limits") |
| total | ≤ 5 turns at about 5–8k input each, so roughly 25–40k input tokens a day before lane overhead, on the chief's account; ≤ 5 GitHub calls |
| ceiling | 27 turns a day (3 scheduled + the 24-a-day manual cap) |
| database | about 135–225 KB a day, kept forever (see [Storage growth](#storage-growth)) |

The token figures are estimates from the prompt bound. **Measure them in the
PR 4 rollout** from the lane's usage before quoting them.

### Testing

- **Pure functions, table tests:** `Delta`, the no-change rule (one case per
  flag, plus one case per *ignored* change: description-only, `last_seen`,
  host offline, a PR going to `unknown`), the headline and its
  `N sessions changed` fallback, `slot` computation (zones, a DST gap and a DST
  repeat, the catch-up edge at exactly 2 h, a Saturday and a Sunday producing
  the same slots as a weekday), repo grouping order (including two
  `owner/api` repos landing in one `api` group, pinning the accepted limit),
  and narrative validation (one refusal case per rule, each failing on its own
  message).
- **PG tests (`dbtest`):** one row per slot under two concurrent builders, both
  orderings; `checkins_one_building`; RLS: owner B reads none of A's check-ins,
  items or snapshots, with a positive control in which A reads its own; the
  composite FK refuses an item whose subject is not in the snapshot; "a past
  check-in survives focus retention" (run `PruneHistory` and the focus sweep,
  then read the check-in unchanged); the 5-minute and 24-a-day limits (the
  25th manual check-in of a local day is refused, a scheduled one is not, and
  the cap resets at local midnight); a disabled owner gets no scheduled
  check-in, with a positive control in which an enabled owner in the same tick
  does, and keeps its rows; deleting a `users` row with check-ins is refused
  until the purge deletes them.
- **Officers:** an officer row is listed with its badge and is never
  `stalled_on_you`, even with a `would_ask` nudge row for it (the fixture that
  would mark a non-officer).
- **Merged PRs:** a fake GitHub answering one query for all refs (assert one
  request); `merged` carried forward and not re-read; a failed call leaves refs
  `unknown`, sets no flag and does not make the check-in changed.
- **The refactor (PR 1):** `fleet_report_pg_test.go` unchanged and green. Plus
  a **ledger test** that `buildFleetReport` and the check-in builder both reach
  `collectFleet` and nothing else reads `WindowHistory` for a report.
- **The stall seam:** a test that the nudge sweep and the check-in builder get
  identical `stallInputs` for one fixture. This pins the relationship as well
  as each side.
- **Narrative:** a fake gateway returning busy, `LanesFull`, `not_ready`, a
  non-JSON answer, a JSON answer with a foreign session, and a contradicting
  section. In each case the report is published and `narrative_state` /
  `narrative_detail` say why. Mutation-check the section-agreement rule: drop
  it and confirm the "contradicting section" case fails on that rule's message.
- **Tool-less route:** the narrative's request to a fake ccd carries
  `X-Muster-CC-Toolless: 1` and `X-Muster-CC-Ephemeral: 1` (assert the
  headers on the request the fake received). Mutation-check: build the
  narrative's route without the check flag and watch this case fail on the
  missing header. Positive control: an operator-lane turn in the same test
  carries neither. A `gateway` chief gets `'off'`, `'gateway_chief'` and no
  turn (assert zero requests). ccd's own handling of the header (the CLI's
  argv, the `400` on a disagreeing turn, `/healthz`) is #373's test surface,
  not this design's.
- **Push:** exactly one send per check-in across a simulated restart
  (`pushed_at`). A changed **manual** and a changed **chief** check-in each
  push; an unchanged manual check-in and a no-change entry do not. A burst:
  of two clicks 5 minutes apart with nothing new in between, the second does
  not push. The body
  contains no focus text, using a fixture whose focus title is a distinctive
  string, with a negative control in which the title is the body's only
  source. A change that is only `unstalled` produces `1 session changed`.
- **Playwright:** open `/checkins` at 390 px and 1280 px. Needs-you is first
  and above the fold on the phone. Check in now → a new card. An ended
  session's row renders without a link.

## Phased plan

Sizes are calibrated on #355 (fleet_report, +2.6k across 45 files), #360
(nudges, +5.3k) and #366 (lanes, +1.8k), tests included.

| # | PR | contents | size | shippable alone? |
|---|---|---|---|---|
| 1 | `fleet: split the report into a collector and encoders` | `collectFleet`, no behaviour change; `stallInputs` extracted from `nudgeCandidates` | S, ~+400 | yes: refactor |
| 2 | `check-ins: code-only reports` | migrations 0068/0069, `internal/checkins` (types, `Delta`, no-change rule, headline, slot every day), PG store, builder (officers shown, short-name groups), sweep (skips disabled owners), `/checkins` list + detail + schedule + Check in now, manual limits (5 min, 24/day), config, docs. **No retention sweep** | L, ~+3k | **yes: usable without the chief** |
| 3 | `check-ins: push` | `checkin` event, toggle, `pushed_at`, headline text and fallback; pushes every **changed** check-in, scheduled, manual or chief | S, ~+400 | yes |
| P | **prerequisite, separate PR #373:** `ccd: tool-less check lanes` | `X-Muster-CC-Toolless: 1` sent with `X-Muster-CC-Ephemeral: 1` on every check route; ccd starts that lane's CLI with `--tools= --strict-mcp-config`, refuses a disagreeing turn (`400`), and shows `"toolless": true` on `/healthz`. PR 4 must not merge before it is on `main`, and the narrative must not be turned on before the chief runs its image (see [Rollout](#configuration)) | (own PR) | yes |
| 4 | `check-ins: the chief's narrative` | **depends on P.** `askChiefThrowaway` shared with nudges, the narrative on `nudgeRoute` (so the tool-less header comes from the shared route builder), prompt, schema, validation, `checkin_items`, retries, `gateway` chief off, `MUSTER_CHECKIN_NARRATIVE` / `_MODEL`, the rollout steps in `docs/configuration.md` | M, ~+1.2k | yes |
| 5 | `check-ins: chief tools` | `checkin_now` (pushes when changed, daily cap), `checkins_list`, `checkin_get` on gateway + `/mcp` | S, ~+600 | yes |
| 6 | `check-ins: merged PRs` | one batched GraphQL read per check-in through the GitHub App's installation token, terminal states carried forward, `unknown` on failure; attribute agents' PRs by their repo and branch | M, ~+1k | yes; turns on `pr_merged` |

Push moved ahead of the narrative: with push decoupled from the trigger, it
needs only PR 2, and it should not wait on the prerequisite.

## Risks

- **Stale focus.** A check-in reports focus as of its last refresh (up to
  15 min, longer past 20 sessions per pass). The page shows `focus_at`
  instead of hiding it.
- **"Stalled on you" depends on nudges.** With nudges off it is always false.
  The page says the classification is off rather than showing a misleading
  zero.
- **Narrative starvation.** An owner who keeps all three ccd lanes busy gets
  code-only check-ins (`lanes_full`), visibly and with a reason. That is
  acceptable because the code report is the contract.
- **Clamped windows after a long gap.** After more than 4 days without a
  check-in, the "who was active" window is clamped to 4 days. The delta is
  still correct (it compares snapshots), but sessions that started and ended
  inside the gap are missed, and coverage says so.
- **Same-short-name repos share a group** (known limit, accepted in
  decision 2). `alice/api` and `bob/api` both appear under `api`. Each row
  still carries its own PR refs (`owner/repo#n`), so the repository is
  recoverable from the row; only the grouping merges them.
- **Unbounded history** (decision 5). Storage grows about 50–80 MB per owner
  per year at 30 sessions, with a 20 GB-a-year ceiling at every cap. The
  compaction option under [Storage growth](#storage-growth) is the way out if
  that becomes a problem; it is not built ahead of need.
- **The narrative waits on another PR.** Until PR #373 is on `main` and on
  the chief's image, check-ins are code-only. That is by design: code-only is
  a complete report.
- 🔴 **Tool-less safety rests on rollout discipline** (decision Q1). muster
  does not check that the chief's ccd honours `X-Muster-CC-Toolless`. An older
  ccd ignores the header and the narrative lane keeps its tools, so focus text
  an outside party influenced could steer a tool call. The mitigation is
  procedural: keep the narrative off until the chief has been restarted onto
  the #373 image and ccd `/healthz` shows `"toolless": true`. The same holds
  for every later ccd rollback. Accepted by the operator, as for stall
  checks.

## Open questions

None. The seven questions the first draft asked were answered on the draft
and are recorded as decisions 1–7 under
[Operator decisions](#operator-decisions), with decision 8 (the tool-less
narrative lane) added by the operator. Round 0 of the review settled Q1 and
D1–D6, recorded in the same section.

## Recommended improvements (not asked for)

- **A weekly roll-up.** A code-only digest on Monday morning: sessions started
  and ended, PRs opened and merged, and time spent stalled per repo, built from
  the week's snapshots with no model call.
- **A session timeline.** On a session's page, a strip of its check-in
  snapshots: state, stall and focus title at each check-in. It outlives the
  30-day focus history.
- **Link check-ins to tasks.** A task's page lists the check-ins in which its
  agent appeared, and `ready_for_review` deltas link to the review banner.
- **"Time in state."** Store `stall_since` and show "stuck 2 days" rather than
  "idle". Computed from consecutive snapshots, at no extra cost.
- **Tune nudges with check-ins.** "Stalled on you" sessions that recur across
  three check-ins with no ask raised are evidence the nudge threshold is too
  high. A small query over both tables can show this.

## Operator decisions

The first four were answered before this proposal. Decisions 1–8 were
answered on the draft (PR #371): 1–7 settle what the first draft
listed as open questions, and 8 is a requirement the operator added. The design
follows all of them and does not reopen them.

| question | decision |
|---|---|
| Cadence | **Schedule + on demand**: a few fixed local times a day (configurable, owner's zone), a **Check in now** button and a chief tool. A scheduled run with nothing changed records a short "no change" entry instead of a report |
| Authoring | **Code table + chief summary**: the per-session table (focus, state, PRs, stalls, asks) is built in code. The chief writes a short narrative (what moved, what's stuck, what needs you) whose every claim links to a session. A claim naming an unknown session is rejected, structurally |
| Delivery | A **Check-ins page** with history (newest first, open any past one, each showing its changes since the previous) and a **phone push** with a one-line headline opening the report. No outside delivery for now |
| Contents | Changes since the last check-in (new, ended, focus changed, newly stalled, PRs opened/merged). **Needs you first** (open asks, nudge would-asks, sessions stalled on the owner's input). Grouped by repo/project. **Dispatched agents too**, with task status |

**Answered on the draft:**

| # | question | decision | where it lands |
|---|---|---|---|
| 1 | Default times; skip weekends? | **`09:00`, `13:00`, `17:00` in the owner's time zone, every day, weekends included.** Editable per owner (times and zone). No weekday mask | data model (`times` default), scheduler (no weekday check), page (schedule form), cost |
| 2 | Same-short-name repos | **One group, accepted for now**, noted as a known limit | builder (repo grouping), page, risks, testing |
| 3 | Push for manual check-ins | **Manual check-ins push too** (**Check in now** and the chief's `checkin_now`), not only scheduled ones. An unchanged check-in never pushes. Rate limits re-checked: a 24-a-day manual cap added beside the 5-minute limit | scheduler (limits), push (one `changed` rule, burst table), chief tools, security, phased plan (push before narrative) |
| 4 | Description-only change | **Not a change**, as proposed | the no-change rule |
| 5 | Retention | **Forever, no sweep.** Ownership and RLS unchanged; disabled accounts keep their rows; deletion only by manual purge | data model, retention and storage growth, accounts, configuration (no retention variable), privacy, testing |
| 6 | Officers | **Shown, never marked as waiting on the owner**, as proposed | builder, page, testing |
| 7 | Merged PRs | **Shown**: one batched GitHub call per check-in, as proposed | merged PRs, delta, privacy, cost, phased plan (PR 6 no longer optional) |
| 8 | (added) Narrative lane | **The narrative's lane MUST be tool-less.** A separate PR (#373) adds a tool-less ccd lane mode for chief checks; the narrative depends on it. How it is enforced was settled in round 0 (Q1, below) | the chief narrative, security, rollout, testing, phased plan (prerequisite P before PR 4) |

**Settled in the review's round 0** (recorded on PR #371):

| # | question | decision | where it lands |
|---|---|---|---|
| Q1 | Should muster check before the turn that ccd supports the tool-less mode, and look for a confirmation after it, or is rollout discipline enough? | **Rollout discipline only.** No pre-send capability check, no after-turn confirmation, no discarded answers. After a deploy, restart the chief onto the new `claude-code-agent` image (`chief_restart`, #369) and confirm `"toolless": true` on ccd `/healthz` before turning the narrative on, as for stall checks. Residual risk (an older ccd ignores the header and the lane keeps its tools) stated, not engineered away | the chief narrative, configuration (rollout), security, testing, phased plan, risks |
| D1 | Drop `checkins_list` / `checkin_get`? | **Kept** | chief tools |
| D2 | Drop "compare with any earlier check-in"? | **Deleted.** Each report shows its changes against the previous check-in only; no compare select, no `?compare=`, no `compare_to` | page, chief tools, testing, phased plan |
| D3 | Drop the three settings (`_MODEL`, `_DEFAULT_TIMES`, `_MANUAL_PER_DAY`)? | **Kept** | configuration |
| D4 | Reuse the REST PR read instead of a GraphQL client? | **Kept as designed** (one batched GraphQL call) | merged PRs |
| D5 | Drop the after-turn confirmation? | **Moot**: Q1 removed it | |
| D6 | Drop the `checkins.changed` live-update event? | **Kept** | page |

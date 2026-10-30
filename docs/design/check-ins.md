# Design: check-ins (a periodic report of every session)

**Status: proposal. Nothing here is built.** The operator settled cadence,
authoring, delivery and contents before this was written; those answers are
recorded under [Operator decisions](#operator-decisions) and the design follows
them. Questions still open are under [Open questions](#open-questions).

The ask, verbatim: *"Chief occasionally checks all of the current focuses and
what each session is doing, and then creates a check-in report. This way it
becomes very easy for me to understand where everything is at a glance without
needing to check each session individually. It also gives us a historical
record of progress over time."*

Where this page says how muster works today, the code is the authority (see
[decisions.md](../decisions.md)). It cites `path:line` at commit `b3fef26f`
(`main`, 2026-10-30).

## Summary

| | Recommendation |
|---|---|
| What a check-in is | A row in muster's own tables: a per-session **snapshot** (built in code), the **delta** against the previous full check-in (computed in code), and an optional **chief narrative** whose every item is tied to a snapshotted session by a foreign key |
| Data layer | Split `buildFleetReport` into a collector and its encoders, and build check-ins on the collector. One builder, not two |
| When | Owner-chosen local times (default `09:00,13:00,17:00`) plus **Check in now** and a chief tool. A scheduled run with no change records a one-line "no change" entry and makes no model call |
| Single run | `UNIQUE (owner_id, slot_at)` plus "one in flight per owner" (a partial unique index). No leader election: muster has none and runs one replica |
| Narrative | One throwaway ccd lane turn on the chief (the stall-check mechanism, `nudgeRoute`). Strict JSON, validated in code. Any failure publishes the code-only report |
| Delivery | A **Check-ins** page (history, open any one, compare with any earlier one) and a push whose text is code-computed counts only: `3 moved · 2 stuck · 1 needs you` |
| Retention | 365 days, configurable. Snapshots are copies, so focus retention never empties a past check-in |
| Cost | No change: nothing. Full report: one chief turn on about 5–8k tokens of input, which the chief's account pays. No transcript is read |
| Effort | Six PRs. The first is a pure refactor; check-ins without the chief are usable after the second |

## Goals

- **At a glance.** One page answers "where is everything": what needs me
  first, then what moved and what is stuck, grouped by repository.
- **Exact facts.** States, focus titles, PRs, stalls, asks and task statuses
  come from muster's rows, computed in code. The model writes commentary on
  those facts. It never supplies them.
- **A durable record.** Each check-in keeps its own copy of what it showed.
  Focus retention ([focus.md](../focus.md), "Retention") deletes the source
  rows within 7 to 30 days; a check-in stays readable after that.
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
| a dispatched agent's **PRs** | `SessionPRs` is asked about mirrored keys only (`fleet_report.go:273-287`) | no source today. Agents' PRs stay a coverage gap in v1 |
| PR **merged** state | operator decision: "PRs opened/merged" | not recorded: `session_prs.kind` is `gh-pr-create` or `git-push` only (`0055_session_prs.sql:21`). See [Phase 6](#phased-plan) |
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
                 narrative: one ephemeral chief lane turn ──▶ validate ──▶ checkin_items (FK)
                               │ busy/lanes full: retry ≤ 20m; error: record
                               ▼
                 push once (counts only), when the narrative settles or at 20m
```

### Data model

Migration numbers: `0066` is the highest on `main`. Open PR #370 adds
`0067_postgres_connector.sql`, so this proposes **`0068`** and **`0069`**.
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
  prs          JSONB NOT NULL DEFAULT '[]',  -- ["owner/repo#12", …] (+ state when known)
  task         JSONB,                        -- {id, title, status} for a dispatched agent
  needs_you    JSONB NOT NULL DEFAULT '[]',  -- [{kind, since, text≤200, key}]
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
entries, sorted, distinct), `updated_at`. **No row means off**, so owners opt
in. That is the rollout gate.

**Size.** A snapshot row is about 1–2 KB. At 4 full check-ins a day and 30
sessions, that is about 200 KB a day, or about 70 MB a year. A no-change entry
is one small `checkins` row with no snapshot copy.

### Retention (recommended: 365 days)

A daily sweep deletes check-ins older than `MUSTER_CHECKIN_RETENTION`
(default `8760h`). Snapshot and item rows cascade. A deleted base sets its
dependants' `base_id` to NULL; their stored deltas stay. **Why a year and not
forever:** the record is the point and a year costs about 70 MB, but snapshots
copy focus descriptions. Those are model summaries of transcripts and can hold
anything a session wrote. Keeping them forever would quietly undo the focus
feature's 30-day bound on that text. A year is long enough for a "what did we
do this quarter" question. `0` means keep forever, for an operator who chooses
that.

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
   `redact.Secrets`.
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
   `owner/` prefix to match the chip's short name. Two repositories with the
   same short name share a group; see the open questions.
7. **The chief itself is excluded**, as in `fleet_report`. Officers are
   included as rows and marked as officers. They are never marked
   "stalled on you": officers escalate through HQ ([chief-nudges.md](../chief-nudges.md)).

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
| `pr_merged` | a ref's recorded state became `merged` (only once Phase 6 records it) |
| `task_changed` | the agent's task status differs |

A check-in has **changed** exactly when any session has any flag set.
**Deliberately not counted:** a description-only rewrite (the summariser
rephrases), `last_seen` or `focus_at` moving, a stale focus or focus problem,
and a host going offline (its sessions' liveness becomes unknown; it is shown
as a coverage gap, not as "ended").

- **Base** = the owner's newest check-in with `status = 'published'`. A
  no-change entry stores no snapshot and records the `base_id` it matched, so
  every delta compares two real snapshots. This also catches a slow drift that
  stays under the per-run threshold across several runs.
- **Scheduled + unchanged** → `status = 'no_change'`. No snapshot copy, no
  model call, no push.
- **Manual or chief + unchanged** → a full report anyway (the owner asked),
  with "No change since 13:00" at the top.

**The headline is code:** `moved` = sessions with any of `new`,
`focus_changed`, `pr_opened`, `pr_merged`, `task_changed` or `ended`;
`stuck` = `stall IS NOT NULL`; `needs you` = sessions with needs-you items,
plus 1 if there are owner-level drafts. A session counts in every column it
qualifies for, and the page says so.

### The scheduler

`RunCheckinSweep` is a 1-minute ticker started in `startBackgroundLoops`. It
does nothing unless `MUSTER_CHECKINS=1`. On each tick, for each owner with
`enabled` preferences:

1. `slot` = the newest configured `HH:MM` in the owner's zone at or before now
   (`time.Date(y, m, d, hh, mm, 0, 0, loc)`, then UTC).
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
**one per 5 minutes per owner**, checked against `started_at`.

### The chief narrative

**Optional by design.** It runs when the owner's chief is on (`chiefOn`) and
`MUSTER_CHECKIN_NARRATIVE` is not `0`. Otherwise `narrative_state='off'` and
the report is complete without it.

**Mechanism: the stall check's, unchanged.**

- Session key `agent:<chief>:webchat:checkin:<hash(owner, checkin id)>`. That
  is fresh per check-in and never the operator's thread.
- `nudgeRoute`, so a `claude-code` chief runs it in an **ephemeral** ccd lane
  and it never appears in the operator's conversation.
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
  and the narrative arrives after the chief's turn.

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
  IANA names, with the browser's `Intl` zone pre-filled), and times.

**`/checkins/{id}`**, top to bottom:

1. **Needs you**: each item with its session, what it is waiting on and since
   when, and a link to answer (`/attention/open/<key>`). Owner-level drafts
   count.
2. **Summary** (the narrative): *Moved*, *Stuck*, each item linked to its
   session row below and to the session itself.
3. **Sessions, grouped by repo.** Groups are ordered by most recent activity,
   with Ungrouped last. Each session shows name, kind, state, stall badge,
   focus title (the description in a `<details>`), delta badges (`new`,
   `ended`, `focus changed`, `newly stalled`, `PR opened`, `merged`, `task →
   ready_for_review`), PR links, task chip and `focus 3 h old` when stale.
   An ended or deleted session shows its snapshot and no link.
4. **Coverage**: omitted sessions and gaps (offline hosts, focus off, agents'
   PRs unknown).

**Compare.** The default view shows the delta against its base. A **Compare
with…** select lists earlier full check-ins; choosing one recomputes the delta
with the same pure function over the two stored snapshots, so nothing new is
stored. `?compare=<id>` is linkable.

**Phone width.** Below `sm`, each session is a card, not a table row. The
needs-you list comes first, so it is above the fold. Every control is at least
44 px tall, matching the sidebar's `min-h-[44px]`. Built in `internal/ui`, so
Tailwind sees the classes. Playwright covers the 390 px layout (below).

### Push

- A new event, `checkin`, in `push.Events`: "A check-in report is ready". It is
  on by default, like every event (operator decision recorded at
  `events.go:3-8`), and has its own toggle on `/account`.
- **Text is code-only:** title `Check-in · 13:00`, body
  `3 moved · 2 stuck · 1 needs you` (zero terms omitted; all zero → `No change
  that needs you`, but see below). The body never contains focus or narrative
  text, the same policy as the `plugin` event. Tag `checkin`, so a newer one
  replaces the older one on the phone. Tapping opens `/checkins/<id>`.
- **Once per check-in**, recorded in `pushed_at`, so a restart does not send it
  twice. It is sent when the narrative settles or at the 20-minute bound,
  whichever comes first. It goes through `goNotify`, so the per-owner cap
  applies.
- **No push for a no-change entry**, and none for a manual or chief check-in
  (the owner is already looking, or asked the chief). **No quiet-hours
  machinery:** the times are the owner's own choice, and that is the quiet
  hours.

### Chief tools

Registered `chiefOnly`, on the gateway and `/mcp`, beside `fleet_report`
(`internal/api/agent.go:377`, dispatch at `:710`):

| tool | what |
|---|---|
| `checkin_now()` | starts a manual check-in (trigger `chief`), returns `{id, url, status}`; the same 5-minute limit and one-building rule; its narrative follows the chief's current turn |
| `checkins_list(since?, until?, limit?)` | headlines of check-ins in a window (no span cap: they are small), newest first, ≤ 50 |
| `checkin_get(id, compare_to?)` | one check-in's code table, delta (against `compare_to` when given) and narrative, capped at 32 KiB. Narrative and focus text are labelled `untrusted` (`focusUntrustedTail`) |

These give the chief history that focus retention would otherwise erase: "what
was the connector work's state last Tuesday" can be answered a year later.

### Configuration

| variable | default | what |
|---|---|---|
| `MUSTER_CHECKINS` | off | `1` runs the sweep and draws the page. **Kill switch**: unset it and restart. Building rows left behind are cleared as `interrupted` on the next boot with it on |
| `MUSTER_CHECKIN_NARRATIVE` | on | `0` publishes code-only reports and never calls the chief |
| `MUSTER_CHECKIN_MODEL` | (pod default) | the model a `claude-code` chief's lane uses for the narrative |
| `MUSTER_CHECKIN_DEFAULT_TIMES` | `09:00,13:00,17:00` | pre-filled for an owner turning check-ins on |
| `MUSTER_CHECKIN_RETENTION` | `8760h` | `0` keeps forever |

All of these are read at boot and listed in `docs/configuration.md`
(`internal/modulegate/envdoc_test.go` fails otherwise).

**Rollout.** There is no shadow mode, because a check-in takes no action. The
staged gates are: server flag → per-owner opt-in (preferences row) → narrative
flag. As with nudges: **restart the chief on the current `claude-code-agent`
image before turning the narrative on.** An older ccd ignores the ephemeral
mark and can evict an operator lane ([chief-nudges.md](../chief-nudges.md),
"Rolling out").

### Privacy: what leaves muster

| data | stays in muster | sent to the chief's provider (Anthropic for `claude-code`, OpenRouter for `gateway`) |
|---|---|---|
| snapshot rows (names, hosts, repos, focus, PRs, tasks, asks) | yes, owner-scoped, 365 days | names, repo labels, focus titles and descriptions (≤ 300), states, PR refs, task status, ask text (≤ 120), redacted |
| hosts, cwd | yes | **never** |
| transcripts, messages, tool output | not read | **never** |
| the push | | counts only, to the owner's push services |

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
  ⚠ **What fencing does not stop:** a `claude-code` lane runs with the chief's
  full tool set, because ccd has no header to restrict tools
  (`internal/agents/turnroute.go:25-47`). An injected focus could ask the
  chief to call a tool during the narrative turn. Stall checks have the same
  exposure today, from more text (they read transcripts). A tool-less lane
  mode in ccd would close it for both; see the improvements.
- `POST /checkins/now` is a session route. It is not a machine route, and the
  rate limit is per owner.

### Cost per day

| | per day (4 slots, 2 manual) |
|---|---|
| scheduled, unchanged | 0 model calls |
| full report | 1 chief turn: about 4–5k tokens of fenced input (16 KiB cap) + about 1k of instructions; output ≤ about 1k |
| lane overhead (`claude-code`) | the CLI's own system prompt and tools on each lane start (unmeasured here; stall checks already pay it), plus 250–550 MiB for the few minutes the lane lives ([chief-nudges.md](../chief-nudges.md), "Limits") |
| total | ≤ 6 turns, roughly 40–80k input tokens a day before lane overhead, on the chief's account |
| database | about 200 KB a day |

The token figures are estimates from the prompt bound. **Measure them in the
Phase 3 rollout** from the lane's usage before quoting them.

### Testing

- **Pure functions, table tests:** `Delta`, the no-change rule (one case per
  flag, plus one case per *ignored* change: description-only, `last_seen`,
  host offline), the headline, `slot` computation (zones, a DST gap and a DST
  repeat, the catch-up edge at exactly 2 h), repo grouping order, and narrative
  validation (one refusal case per rule, each failing on its own message).
- **PG tests (`dbtest`):** one row per slot under two concurrent builders, both
  orderings; `checkins_one_building`; RLS: owner B reads none of A's check-ins,
  items or snapshots, with a positive control in which A reads its own; the
  composite FK refuses an item whose subject is not in the snapshot; the
  retention cascade; "a past check-in survives focus retention" (run
  `PruneHistory` and the focus sweep, then read the check-in unchanged).
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
- **Push:** exactly one send per check-in across a simulated restart
  (`pushed_at`). None for no-change or manual check-ins. The body contains no
  focus text, using a fixture whose focus title is a distinctive string, with a
  negative control in which the title is the body's only source.
- **Playwright:** open `/checkins` at 390 px and 1280 px. Needs-you is first
  and above the fold on the phone. Check in now → a new card. Compare with an
  older one. An ended session's row renders without a link.

## Phased plan

Sizes are calibrated on #355 (fleet_report, +2.6k across 45 files), #360
(nudges, +5.3k) and #366 (lanes, +1.8k), tests included.

| # | PR | contents | size | shippable alone? |
|---|---|---|---|---|
| 1 | `fleet: split the report into a collector and encoders` | `collectFleet`, no behaviour change; `stallInputs` extracted from `nudgeCandidates` | S, ~+400 | yes: refactor |
| 2 | `check-ins: code-only reports` | migrations 0068/0069, `internal/checkins` (types, `Delta`, no-change rule, headline, slot), PG store, builder, sweep, `/checkins` list + detail + compare + schedule + Check in now, retention, config, docs | L, ~+3k | **yes: usable without the chief** |
| 3 | `check-ins: the chief's narrative` | `askChiefThrowaway` shared with nudges, prompt, schema, validation, `checkin_items`, retries, `MUSTER_CHECKIN_NARRATIVE` / `_MODEL` | M, ~+1.5k | yes |
| 4 | `check-ins: push` | `checkin` event, toggle, `pushed_at`, headline text | S, ~+400 | yes |
| 5 | `check-ins: chief tools` | `checkin_now`, `checkins_list`, `checkin_get` on gateway + `/mcp` | S, ~+600 | yes |
| 6 | `session PRs: record merged` (optional) | read PR state for refs already in `session_prs` through the GitHub App's installation token (`internal/githubapp/app.go:813` lists PRs; `github.PullRequest` needs `merged_at`), and attribute agents' PRs by their repo and branch | M, ~+1k | yes; turns on `pr_merged` |

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

## Open questions

1. **Default times.** Is `09:00,13:00,17:00` right, and should weekends be
   skipped by default (a per-owner "weekdays only" box)?
2. **Same-short-name repos.** Is grouping `alice/api` and `bob/api` together
   under `api` acceptable, or should agent labels keep `owner/name`?
   Mirrored chips only carry the short name, so keeping the owner would split
   one repo across two groups.
3. **Push for manual check-ins.** The proposal sends none. Do you want one when
   you click **Check in now** and leave the page?
4. **"No change" granularity.** Should a description-only change count as
   change? The proposal says no, because the summariser rephrases.
5. **Retention.** Is 365 days right, or should it be forever? Forever keeps
   focus descriptions past their 30-day bound.
6. **Officers.** Should they appear in check-ins at all (shown, never marked
   stalled on you), or be left out like the chief?
7. **Merged PRs (Phase 6).** Is reading PR state from GitHub worth an outbound
   call per open PR per check-in, or is "PRs opened" enough?

## Recommended improvements (not asked for)

- **A weekly roll-up.** A code-only digest on Monday morning: sessions started
  and ended, PRs opened and merged, and time spent stalled per repo, built from
  the week's snapshots with no model call.
- **A session timeline.** On a session's page, a strip of its check-in
  snapshots: state, stall and focus title at each check-in. It outlives the
  30-day focus history.
- **Link check-ins to tasks.** A task's page lists the check-ins in which its
  agent appeared, and `ready_for_review` deltas link to the review banner.
- **A tool-less ccd lane** (`X-Muster-CC-Tools: none`, or a fixed allowlist)
  for narrative and stall-check turns. This closes the injection-to-tool path
  noted under Security for both features.
- **"Time in state."** Store `stall_since` and show "stuck 2 days" rather than
  "idle". Computed from consecutive snapshots, at no extra cost.
- **Tune nudges with check-ins.** "Stalled on you" sessions that recur across
  three check-ins with no ask raised are evidence the nudge threshold is too
  high. A small query over both tables can show this.

## Operator decisions

Answered before this proposal. The design follows them and does not reopen
them.

| question | decision |
|---|---|
| Cadence | **Schedule + on demand**: a few fixed local times a day (configurable, owner's zone), a **Check in now** button and a chief tool. A scheduled run with nothing changed records a short "no change" entry instead of a report |
| Authoring | **Code table + chief summary**: the per-session table (focus, state, PRs, stalls, asks) is built in code. The chief writes a short narrative (what moved, what's stuck, what needs you) whose every claim links to a session. A claim naming an unknown session is rejected, structurally |
| Delivery | A **Check-ins page** with history (newest first, open any past one, compare with the previous) and a **phone push** with a one-line headline opening the report. No outside delivery for now |
| Contents | Changes since the last check-in (new, ended, focus changed, newly stalled, PRs opened/merged). **Needs you first** (open asks, nudge would-asks, sessions stalled on the owner's input). Grouped by repo/project. **Dispatched agents too**, with task status |

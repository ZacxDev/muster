# Design: session state (stalls, waiting sessions, nudges and check-ins as one model)

**Status: proposal. Nothing here is built.** The operator settled the shape
(item, Chief role, states, push, placement, check-ins, phasing, metrics) before
this was written, then answered the draft's open questions; all of it is
recorded under [Operator decisions](#operator-decisions). Three remaining
points carry coordinator defaults, each open to change, under
[Defaults](#defaults-open-to-change). No questions remain open.

The ask, verbatim: *"It seems like Chief generates asks for stalled sessions.
It also generates nudges. And I think there's also a check ins feature that I
talked about previously. … This feels like all three of these features are
part of the same concept. Just not sure how to consolidate them. … I also find
that in the asks that it generates from stalled sessions, I never read its
summary or its options that it gives me. I just go straight to that session to
answer. Same thing with the nudges."*

Where this page says how muster works today, the code is the authority (see
[decisions.md](../decisions.md)). It cites `path:line` at commit `7cbb6b11`
(`main`, 2026-11-03), which includes #415 (a live stall decision raises a
notice, not an answerable ask) and #416 (opencode questions reach the
attention queue).

## Summary

| | Proposal |
|---|---|
| The concept | One **session state** per session, computed in code: `working`, `needs-you`, `stalled`, `done`. A stop is an **episode**; everything the owner sees is a view of states and episodes |
| Where it comes from | Inputs muster already reads: the stall rule's inputs (`stallInputs`), the waiting sessions (`waitingSessions`), the stall classifier's `needs_input` score, `session_prs`, and a PR's merged/closed state (#417's `PullRequestState`) |
| The Chief | Decides per episode **`nudge`, `leave` or `notify`**. No summary, no options, no notice text. Its covering words widen from "the owner's messages in this session" to "the owner's own typed messages in sessions on the **same repository**, plus their messages to the Chief", quoted verbatim. A `needs-you` session is eligible for a nudge when quoted words answer its question. Shadow mode first, then live per owner, under the existing hourly cap |
| Push | **One push per `needs-you` or `stalled` episode**, text built in code: session name, host:window, state and idle time. **No session text.** Tap jumps to the session. Stall and waiting collapse into it. A nudged or left episode does not push |
| Attention | A **"Stalled / waiting"** section of plain rows, no answer box, oldest first. It replaces chief-nudge notices (and the stall asks they replaced). A left session is a quiet row there. Real questions stay above it as answerable asks |
| Check-ins | Saved snapshots of the same state at 09:00, 13:00 and 17:00; the Chief narrative on scheduled runs only. Built on this collector, not alongside it |
| What retires | The notice (`attention.KindNotice`) as the stall surface, `nudgeNoticeText`, the generic ask text, and the Chief's `ask` decision (replaced by a text-free `notify`) |
| Success | Pushes per day go down; the share of stalled episodes ended by a nudge goes up (today: 2 nudges in about 330 decisions over 7 days); zero stall items expire unread |

## Goals

- **The push is the product.** The owner acts from the notification and goes
  straight to the session. What the push says, and that there is exactly one
  per stop, matters more than any page.
- **Facts from code, judgement from the Chief only where it saves a trip.**
  Which session, where, its state and how long are read, not written.
  The Chief's one call per stalled episode is spent on the only thing that
  can spare the owner the trip: a nudge carrying their own words.
- **One state, many views.** The Attention section, the push, a check-in and
  the Chief's own tools all read the same per-session state, so they cannot
  disagree about who is stalled.
- **Never quieter by accident.** A failure anywhere (the Chief, the
  classifier, GitHub, a host that dropped) leaves a session visible as
  stalled or waiting, never hidden.

## Non-goals

- **Answering for the owner.** A nudge pastes the owner's own earlier words;
  the Chief never composes an instruction. Questions a session or agent asks
  through `attention_ask` stay answerable asks, unchanged.
- **Session text in a push.** The push names the session and its state; it
  never quotes it (see [The last line](#the-last-line-why-the-push-does-not-carry-it)).
- **Reading more transcript than today.** The state uses what the sweep and
  the hosts already read.
- **Other owners' sessions.** Row-level security as everywhere else.
- **Rewriting the check-ins design.** This page changes what check-ins read
  from, not what they show.

## What exists today

### The measurement behind this page

Over the 7 days to 2026-11-03, on the operator's deployment:

- **Chief decisions:** `ask` 187 (144 live, 43 shadow), `leave` 146 (93 live,
  53 shadow), `nudge` 2 (1 live, 1 shadow).
- **The live stall asks those decisions raised:** 138 reached a final state,
  **0 answered**. 125 were withdrawn because the session moved on, a mean of
  **102 minutes** after they were raised; 8 expired after 24 hours; 5 were
  still open.
- So the owner read none of the Chief's summaries or options, and the one
  outcome that saved a trip (a nudge) happened twice.

### Stall nudges: the sweep, the streak, the decision

- **Candidates.** Every 2 minutes, `stallInputs`
  (`internal/api/chief_nudge.go:319`) lists the owner's mirrored Claude Code
  and opencode windows and dispatched-agent conversations. `chiefnudge.Exclude`
  (`internal/chiefnudge/chiefnudge.go:204`) keeps one only if its status is
  `idle` (`:213`), it has been idle for `DefaultIdle` (10m, `:105`), and it
  has no background work, turn error or open ask, and this streak is not
  already decided.
- **The streak is the existing episode boundary.** A mirrored window's
  `StreakKey` is `"since:" + <the status's since-time>`
  (`chief_nudge.go:346`); an agent conversation's is `"msg:" + <its last
  message id>` (`:389`). A new turn starts a new streak. Decisions are stored
  once per `(kind, key, streak)` in `chief_nudges` (migration `0064`), so a
  streak is judged once and that holds across restarts.
- **`AnchorKey`** is the owner's latest message id in that session
  (`chiefnudge.Anchor`, `chiefnudge.go:349`). It is a nudge budget, not an
  episode: at most `MaxNudgesPerAnchor` (2, `:113`) nudges per anchor, so a
  fresh owner message resets it.
- **The decision costs two model calls.** `decideNudge` (`chief_nudge.go:475`)
  reads the transcript (`nudgeTranscript`, `:727`), runs the stall classifier
  (Jev over OpenRouter, `:507-523`, `internal/chiefnudge/jev.go`), then asks
  the Chief in a throwaway, tool-less lane (`askChiefForNudge`, `:936`). An
  ask-stall (`AskStall`, `jev.go:297`) defaults to `ask` unless the label is
  `asks_permission` and the nudge's quote passes.
- **The nudge's guard is code.** `CheckQuote` (`chiefnudge.go:427`) accepts a
  nudge only if its quote occurs, word-bounded, in one of the owner's own
  messages to **that session** within `OwnerWindow` (6h, `:115`), selected by
  `OwnerMessages` (`chief_nudge.go:493`). Whether the words *cover* the next
  step is the Chief's judgement, not code's (`chiefnudge.go:420-426`). The
  global `DefaultHourlyCap` is 6 (`:110`).

### #415: the live `ask` is now a notice

- `actAsk` (`chief_nudge.go:1220`) creates an `attention.KindNotice` row
  labelled with where the session is, holding the Chief's message
  (`nudgeNoticeText`, `:1277`; up to 500 runes). A notice has no options, no
  answer, no relay, and does not count toward the 50-open-ask cap
  (`:1243-1250`).
- `reconcileNudge` (`:1288`) withdraws a notice once a *read* state shows the
  session moved on (`:1347-1356`); an unreadable state leaves it for the
  notice's own expiry.
- The notice is pushed through `askFiled`
  (`internal/api/attention_suggest.go:50`) → `attentionNotify`. Its title is
  `"<place> may be waiting on you"`; its body is the focus title and the
  Chief's message (`internal/api/attention.go:856-866`).

So #415 removed the answer box but kept the Chief-written text, which is the
part the operator does not read.

### Waiting sessions and their push

- `waitingSessions` (`attention.go:410`) lists windows whose host reports
  status `waiting`: a permission prompt, an AskUserQuestion, an opencode
  question or permission request (#416), a Hermes ask.
- They push from `attentionMirrorChanged` (`attention.go:960`) when a session
  *starts* waiting. Two dedupes, both **in memory**:
  - a host's first observation in this process is seeded silently
    (`seenHosts`, `:119-122`), so a restart re-notifies nothing;
  - a session pushed within `attentionRepushAfter` (2 minutes, `:943`) is not
    pushed again ("waiting → busy → waiting is one episode", `:942`).
- **The push body is summary only by operator decision**
  (`attention.go:829-835`): `"Waiting: <tool> · <window> @ <host>"`, never
  the command and never what Claude said.

### Can one session be pushed twice for one stop?

- **Not at the same instant.** The stall rule requires status `idle`
  (`chiefnudge.go:213`) and the waiting push requires status `waiting`, so a
  session in one state is not a candidate for the other.
- **Across a stop that changes status, yes.** The two pushes use different
  keys and tags (`attention-ask-<id>` for a notice, `attention-<session key>`
  for a waiting session) and different dedupe stores: the decided streak in
  Postgres, and the in-memory `pushedAt`. A session that waits on a
  permission prompt, is let through, then stops 10 minutes later with a prose
  question gets one push for each. This page makes that one episode (see
  [Episodes](#episodes)).
- Measured only by reading the code; the double-push rate on the live
  deployment was not counted.

### The last line (why the push does not carry it)

- **A waiting Claude Code window carries it.** `mirror.Pending.Last` is "the
  last thing Claude said", capped at 200 runes, cleaned and secret-redacted by
  the host and again by muster (`internal/mirror/pending.go:12-37`, `:152`).
  It is sent only for `StatusWaiting` windows (`pending.go:31-35`) and **never
  for an opencode ask** (`:180-182`). The page shows it as the item's `Detail`
  (`attention.go:176-178`); **by operator decision it never reaches a push**
  (`pending.go:27-28`).
- **An idle (stalled) window does not carry it.** Its last assistant message
  is read only by the stall sweep, from the transcript stream
  (`nudgeTranscript`, `chief_nudge.go:727`; `LastIsAssistant`, `:482`), and it
  is cut to `MaxLastRunes` (4000, `chiefnudge.go:120`) for the Chief and the
  classifier. Today muster deliberately puts **no session text** in a stall
  notice (`jev.go:301-305`).
- A dispatched agent's last message is a database row
  (`ListRecentChatMessages`, `chief_nudge.go:380`).

### Check-ins

`docs/design/check-ins.md` is a proposal; only its PR 1 (the fleet collector
split, #374) and its prerequisite (tool-less lanes, #373) are on `main`. Its
stall column already reads `stallInputs` ("Anything that classifies a session
as stalled reads these", `chief_nudge.go:315-317`).

## Design

### States

Computed in code, per session, on the attention sweep. Each state names its
inputs; none is a model's opinion.

| state | means | inputs |
|---|---|---|
| `working` | busy, or idle for less than the stall threshold, or running background work | host status, `IdleFor`, `Shell` |
| `needs-you` | the session waits on the owner: a waiting window (prompt, question, permission), or an idle session whose last message the classifier scores as asking | `waitingSessions`; for idle sessions the `needs_input` + `asks_permission` score at or above the ask threshold (`AskStall`) |
| `stalled` | idle past the threshold with no background work and no turn error, not `needs-you` | `stallInputs` + `Exclude` |
| `done` | ended, or every PR it opened is merged or closed | the session ended; `session_prs` + `PullRequestState` (#417) |

A session with a turn error, an officer, the chief, and another owner's
session keep today's exclusions.

### Episodes

An episode is one stop of one session, from entering `needs-you` or `stalled`
to leaving both. Its key reuses what exists: the mirror `StreakKey`
(`"since:<t>"`) or the agent `"msg:<id>"`. Persisting episodes in a table
(one row per `(owner, session, streak)`) replaces the in-memory `pushedAt` for
these pushes, so dedupe survives a restart and the double rollout.

**One stop is one episode** (default D1): a `needs-you` → `stalled` change
within a stop updates the episode's state and its row, and does not push
again. A waiting window's episode starts at the status's since-time and the
idle streak that follows inherits it, so the two existing keys map to one
episode rather than two.

### The Chief: nudge, leave or notify

Per `stalled` or `needs-you` episode, at most once:

1. Code builds the candidate exactly as today.
2. **Widened covering words, same repository only:** the owner's own typed
   messages within `OwnerWindow` in this session **and in their other
   sessions on the same repository**, plus their messages to the Chief, each
   tagged with where it came from. A session's repository is what muster
   already records for it (the agent's `Repo`, the window's repo chip, the
   focus `cwd`); a session whose repository is unknown contributes and
   receives no cross-session quotes, only its own messages and the Chief
   chat. `CheckQuote` keeps its guarantee (verbatim, word-bounded,
   owner-authored, not injected) against this wider set.
3. The Chief returns one of:
   - `nudge`: with quote, quote source and covered action. For a `needs-you`
     session, the quote must answer the question the session asked.
   - `leave`: the session needs nothing from the owner now.
   - `notify`: neither; the owner should look.

   No question, no summary, no options.
4. `nudge` → today's send path (send queue for a mirror, a turn for an
   agent), the hourly cap and `MaxNudgesPerAnchor`. A nudge sent ends the
   episode with no push.
5. `leave` → a quiet row on the Attention board, **never a push**.
6. `notify`, a refused quote, a capped nudge, or any failure (the Chief's
   call, an unparseable answer, a lane refusal past its bound) → the episode
   is pushed and listed. A failure is never read as `leave`.

**The classifier stays** (default D2): it splits `needs-you` from `stalled`
for an idle session, and gates the `needs-you` nudge (a `needs-you` nudge is
considered only when the classifier scored the stall as asking).

**View-only runtimes** (default D3): opencode and Hermes windows get the same
push and row as any other session, but are **never nudged**; for them the
Chief's `nudge` is recorded and treated as `notify`, as #415 already treats a
runtime window today.

Shadow first: decisions are recorded on the Chief nudges tab as
`would_send` / `left` / `would_notify`, and nothing is sent, until the owner
is listed live (`MUSTER_CHIEF_NUDGE_LIVE_OWNERS`).

### Push

One push per episode, under the existing `attention` toggle:

- **Title:** the session and where it is, e.g. `alpha:3 @ host-a needs you` or
  `alpha:3 @ host-a is stalled`.
- **Body:** the state and idle time, e.g. `Needs you · idle 12m`. **No session
  text**: no last line, no tool call, no focus title written by a model. This
  keeps both earlier operator decisions: a waiting session's push is summary
  only (`attention.go:829-835`), and muster puts no session text in a stall
  item (`jev.go:301-305`).
- **Tag:** the episode key, so a re-push replaces rather than stacks, and a
  withdrawn episode clears it.
- **Tap:** opens the session (`attentionOpenURL` already resolves an item's
  session; a host's jump key, `muster nudge next`, does the same in tmux).
- **No push** when the episode was nudged or left, or ends before it is
  pushed.

### Attention: "Stalled / waiting"

A section below the answerable items: one row per open episode, oldest first,
showing session, host:window, state and idle time, with a "View in session"
link and no answer box. A left episode is a quiet row (drawn muted, counted
apart from the rest, never pushed). The page may still show a waiting
window's `Detail` (its last words) as it does today; the push never does. It replaces notice rows. Answerable
asks (from agents, sessions, muster's own system asks, opencode questions with
relayable answers) stay above it, unchanged.

### Check-ins on the shared state

Check-ins snapshot this state rather than re-deriving it. Of the check-ins
plan (`docs/design/check-ins.md`, "Phased plan"):

| check-ins PR | effect |
|---|---|
| 2 `code-only reports` | its stall and needs-you columns read the session state instead of `stallInputs` + `attentionItems` separately; the rest stands |
| 3 `push` | unchanged, but its push is a separate, scheduled digest; it never duplicates an episode push |
| 4 `chief narrative` | unchanged, scheduled runs only (operator decision) |
| 5 `chief tools` | gains a `session_state` read the Chief uses for both |
| 6 `merged PRs` | **mostly subsumed**: #417 already reads a PR's state with a repo-scoped token and caches it; check-ins reuse `PullRequestState` instead of a new GraphQL client. Agents' PR attribution remains |

## Risks

- **A wrong widened nudge types as the owner.** Code guarantees the words are
  the owner's; it cannot check they cover *this* step. Quoting from another
  session on the same repository still raises the chance the words were about
  something else ("merge it" meant a different PR). A `needs-you` nudge
  answers a question on the owner's behalf. Shadow mode, the hourly cap,
  `MaxNudgesPerAnchor`, and the Chief nudges tab's audit rows (with the
  quote's source) are the mitigations; none prevents a wrong nudge.
- **Cross-session quoting.** Limiting sources to the same repository keeps a
  quote from crossing into another repository, account or client. It relies
  on the recorded repository being right; a session whose repository is
  unknown gets no cross-session quotes.
- **Push dedupe across restarts.** Today's waiting-session dedupe is in
  memory and seeds silently on a host's first observation. Moving it to a
  persisted episode changes that rule: a restart mid-episode must neither
  re-push nor drop the push it owed. The double rollout makes two processes
  briefly race for the same episode.
- **Push volume still depends on `leave`.** `leave` (146 of 335 decisions
  today) is kept and never pushes, so it holds today's quiet share. Waiting
  windows now push as one episode with the stall that may follow, which
  removes today's second push for one stop.

## Rollout

1. **#415 + #416** deployed (in progress separately).
2. **Session state + episodes + push + Attention section.** Stall notices stop
   being raised; open ones are withdrawn by the existing reconcile. The Chief
   is still asked per streak; `notify` pushes and `leave` lists quietly.
3. **Widened nudging, shadow** for every owner; the operator reviews
   `would_send` decisions on the Chief nudges tab, then lists owners live.
4. **Check-ins** PRs 2–5 on the shared state, PR 6 reduced.

Each is its own PR with tests and the audit ladder. Turning the stall path
off stays the existing kill switch (`MUSTER_CHIEF_NUDGES` unset).

## Open questions

None. The draft's seven questions were answered by the operator (decisions 4,
9, 10 and 11) or carry a default below.

## Defaults (open to change)

Taken by the coordinator, not decided by the operator. Each can be changed
without reworking the rest of the design.

- **D1. One stop is one episode.** A `needs-you` → `stalled` change within a
  stop does not push again.
- **D2. Keep the stall classifier.** It splits `needs-you` from `stalled` and
  gates the `needs-you` nudge.
- **D3. View-only runtimes.** opencode and Hermes windows get the same push and
  row, but are never nudged.

## Operator decisions

Recorded 2026-11-03.

1. **Item shape.** The operator acts from the push notification only, never
   reads the Chief's summary or options on stall asks, and goes straight to
   the session. Measured over 7 days: 138 stall asks resolved, 0 answered;
   125 cancelled because the session moved on (average 102 minutes after the
   ask); 8 expired.
2. **Chief role.** It decides whether to nudge per stalled episode, with no
   summary and no options. Keep nudging and widen it: covering words may be
   the owner's own typed messages in other sessions, plus their messages to
   the Chief, quoted verbatim. Nothing is inferred from rules files. Shadow
   mode first, then live per owner, under the existing hourly cap. (The
   sources are narrowed by decision 10.)
3. **States.** `working` / `needs-you` / `stalled` / `done`, computed in code
   from existing inputs: the stall rule, waiting sessions, `jev_needs_input`,
   `session_prs`, and PR state from #417.
4. **Push.** The operator acts from the push notification alone ("Only the
   push notification"). One push per `needs-you` or `stalled` episode, text
   built in code: session name, host:window, state and idle time, and **no
   session text**. This keeps the earlier decisions at `attention.go:829-835`
   and `jev.go:301-305`. Tap jumps to the session. Stall and waiting collapse
   into that one push. A nudged session sends no push. (The draft also listed
   the session's last line here; the operator never asked for it, and it is
   removed.)
5. **Placement.** A "Stalled / waiting" section of Attention with plain rows
   and no answer box, oldest first. It replaces stall asks and #415's notices.
   Real questions stay as answerable asks above it.
6. **Check-ins.** Kept, as saved snapshots of the same session state at 09:00,
   13:00 and 17:00, with the Chief narrative on scheduled runs only.
7. **Phasing.** (1) deploy #415 + #416; (2) code-built session state, episode
   push and Attention section, retiring stall asks; (3) widened nudging in
   shadow mode; (4) check-ins on the shared state.
8. **Success metrics.** Pushes per day go down; the share of stalled episodes
   ended by a nudge goes up (today 2 nudges of about 330 decisions in 7 days);
   zero stall items expire unread.
9. **`leave` stays.** The Chief keeps its `leave` decision. A left session
   shows as a quiet row on the Attention board and never pushes.
10. **Quote sources: same repository only.** The owner's messages in sessions
    on the same repository, plus their Chief chat. This supersedes "any
    session" in decision 2.
11. **`needs-you` nudges: yes.** A `needs-you` session is eligible for a nudge
    when quoted words answer its question, shadow mode first.

## Effort

Sizes calibrated as in the check-ins plan (#360 nudges +5.3k, #366 lanes
+1.8k), tests included. Rough.

| # | PR | contents | size |
|---|---|---|---|
| 1 | `session-state: states and episodes` | the state function over existing inputs, an episodes table (migration), persisted push dedupe, `session_state` read | M, ~+1.5k |
| 2 | `session-state: episode push and the Attention section` | one text-free push per episode, the "Stalled / waiting" rows (left ones quiet), notices no longer raised, the Chief's `ask` replaced by `notify` | M, ~+1.5k |
| 3 | `chief-nudges: widened covering words (shadow)` | same-repository owner messages and Chief-chat messages as quote sources, source recorded; `needs-you` nudges gated by the classifier; view-only runtimes never nudged; tab shows source | M, ~+1.2k |
| 4 | check-ins PRs 2–5 on the shared state, PR 6 reduced to agents' PR attribution | as in `check-ins.md` | as there, minus PR 6's GraphQL client |

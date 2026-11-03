# Design: session state (stalls, waiting sessions, nudges and check-ins as one model)

**Status: proposal. Nothing here is built.** The operator settled the shape
(item, Chief role, states, push, placement, check-ins, phasing, metrics) before
this was written; those answers are recorded under
[Operator decisions](#operator-decisions). Some consequences of them conflict
with earlier operator decisions or with each other; those are listed under
[Open questions](#open-questions), unanswered.

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
| The Chief | Decides **only nudge or no-nudge** per stalled episode. No summary, no options, no notice text. Its covering words widen from "the owner's messages in this session" to "the owner's own typed messages in any of their sessions, plus their recent messages to the Chief", quoted verbatim. Shadow mode first, then live per owner, under the existing hourly cap |
| Push | **One push per `needs-you` or `stalled` episode**, text built in code: session, host:window, idle time, the session's last line. Tap jumps to the session. Stall and waiting collapse into it. A nudged episode does not push |
| Attention | A **"Stalled / waiting"** section of plain rows, no answer box, oldest first. It replaces chief-nudge notices (and the stall asks they replaced). Real questions stay above it as answerable asks |
| Check-ins | Saved snapshots of the same state at 09:00, 13:00 and 17:00; the Chief narrative on scheduled runs only. Built on this collector, not alongside it |
| What retires | The notice (`attention.KindNotice`) as the stall surface, `nudgeNoticeText`, the generic ask text, and the Chief's `ask` decision |
| Success | Pushes per day go down; the share of stalled episodes ended by a nudge goes up (today: 2 nudges in about 330 decisions over 7 days); zero stall items expire unread |

## Goals

- **The push is the product.** The owner acts from the notification and goes
  straight to the session. What the push says, and that there is exactly one
  per stop, matters more than any page.
- **Facts from code, judgement from the Chief only where it saves a trip.**
  Which session, where, how long, and what it last said are read, not written.
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
- **Reading more transcript than today.** The state uses what the sweep and
  the hosts already read (see [The last line](#the-last-line)).
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
  question gets one push for each. Whether those are one stop or two is a
  definition this page has to make (see [Episodes](#episodes)).
- Measured only by reading the code; the double-push rate on the live
  deployment was not counted.

### The last line

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
these pushes, so dedupe survives a restart and the double rollout. Whether a
`needs-you` → `stalled` transition inside one stop is the same episode is
[open](#open-questions).

### The Chief: nudge or no-nudge

Per stalled (or `needs-you`) episode, at most once:

1. Code builds the candidate exactly as today.
2. **Widened covering words:** the owner's own typed messages in any of
   their sessions within `OwnerWindow`, plus their recent messages to the
   Chief, each tagged with where it came from. `CheckQuote` keeps its
   guarantee (verbatim, word-bounded, owner-authored, not injected) against
   this wider set.
3. The Chief returns `nudge` (with quote, quote source and covered action) or
   `no_nudge`. No question, no summary, no options.
4. `nudge` → today's send path (send queue for a mirror, a turn for an
   agent), the hourly cap and `MaxNudgesPerAnchor`. A nudge sent ends the
   episode's push obligation.
5. `no_nudge`, a refused quote, a capped nudge, or any failure → the episode
   is pushed and listed.

Shadow first: decisions are recorded on the Chief nudges tab as
`would_send` / `would_not`, and nothing is sent, until the owner is listed
live (`MUSTER_CHIEF_NUDGE_LIVE_OWNERS`).

### Push

One push per episode, under the existing `attention` toggle:

- **Title:** the session and where it is, e.g. `alpha:3 @ host-a needs you` or
  `… is stalled`.
- **Body:** `idle 12m`, then the session's last line (see
  [open question 1](#open-questions); today's decisions forbid it).
- **Tag:** the episode key, so a re-push replaces rather than stacks, and a
  withdrawn episode clears it.
- **Tap:** opens the session (`attentionOpenURL` already resolves an item's
  session; a host's jump key, `muster nudge next`, does the same in tmux).
- **No push** when the episode was nudged, or ends before it is pushed.

### Attention: "Stalled / waiting"

A section below the answerable items: one row per open episode, oldest first,
showing session, host:window, state, idle time and the last line, with a
"View in session" link and no answer box. It replaces notice rows. Answerable
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
  session raises the chance the words were about something else ("merge it"
  meant a different PR). Shadow mode, the hourly cap, `MaxNudgesPerAnchor`,
  and the Chief nudges tab's audit rows are the mitigations; none prevents a
  wrong nudge.
- **Cross-session quoting leaks context between sessions.** A nudge pastes
  text the owner wrote to session A into session B, which may be a different
  repository, account or client. The quote must be shown with its source,
  and some sessions may need to be excluded as sources (open).
- **Push dedupe across restarts.** Today's waiting-session dedupe is in
  memory and seeds silently on a host's first observation. Moving it to a
  persisted episode changes that rule: a restart mid-episode must neither
  re-push nor drop the push it owed. The double rollout makes two processes
  briefly race for the same episode.
- **More pushes, not fewer, if `leave` disappears.** Today the Chief's `leave`
  (146 of 335 decisions) raises nothing. If every non-nudged stalled episode
  pushes, push volume rises. The success metric depends on how `done` and
  "stalled but nothing to do" are drawn (open question 2).
- **The last line is session-authored text on a lock screen.** It is the
  input an outside party can most influence (a prompt-injected session), and
  today's push deliberately omits it.

## Rollout

1. **#415 + #416** deployed (in progress separately).
2. **Session state + episodes + push + Attention section.** Stall notices stop
   being raised; open ones are withdrawn by the existing reconcile. The Chief
   is still asked per streak, but only the nudge branch acts.
3. **Widened nudging, shadow** for every owner; the operator reviews
   `would_send` decisions on the Chief nudges tab, then lists owners live.
4. **Check-ins** PRs 2–5 on the shared state, PR 6 reduced.

Each is its own PR with tests and the audit ladder. Turning the stall path
off stays the existing kill switch (`MUSTER_CHIEF_NUDGES` unset).

## Open questions

Unanswered; the design does not pick for them.

1. **The last line in the push.** The operator asked for the session's last
   line in the push. Two earlier operator decisions forbid it: a waiting
   session's push is "summary only … never what Claude said"
   (`attention.go:829-835`), and a stall notice carries "no session text"
   (`jev.go:301-305`). Does the new decision replace both, for Claude Code,
   opencode and agents alike? And for a stalled (idle) session, the line
   comes from the transcript the sweep already reads, not from the host:
   acceptable?
2. **Does `leave` survive in some form?** The Chief's `leave` is 44% of
   decisions and pushes nothing today. Options include: drop it (every
   non-nudged stall pushes), map the classifier's `neither` label to a quiet
   state, or keep a code rule. Which one decides whether pushes go down.
3. **Is `needs-you` → `stalled` within one stop one episode or two?** (A
   permission prompt let through, then a prose question 10 minutes later.)
4. **Which sessions may be a quote's source** for another session's nudge:
   all of the owner's, same repository only, or an exclusion list?
5. **Does a `needs-you` idle session (classifier says it asked) go to the
   Chief for a nudge at all,** or only `stalled` ones? Today an ask-stall
   allows a nudge only for `asks_permission`.
6. **Is the stall classifier still worth its call** once the Chief no longer
   writes an ask? It currently decides ask-vs-nudge; here it would only split
   `needs-you` from `stalled`.
7. **Do opencode and Hermes windows,** which are view-only (never nudged),
   get the same push and row shape?

## Operator decisions

Recorded 2026-11-03.

1. **Item shape.** The operator acts from the push notification only, never
   reads the Chief's summary or options on stall asks, and goes straight to
   the session. Measured over 7 days: 138 stall asks resolved, 0 answered;
   125 cancelled because the session moved on (average 102 minutes after the
   ask); 8 expired.
2. **Chief role.** It decides only nudge or no-nudge per stalled episode, with
   no summary and no options. Keep nudging and widen it: covering words may be
   the owner's own typed messages in any of their sessions, plus their recent
   messages to the Chief, quoted verbatim. Nothing is inferred from rules
   files. Shadow mode first, then live per owner, under the existing hourly
   cap.
3. **States.** `working` / `needs-you` / `stalled` / `done`, computed in code
   from existing inputs: the stall rule, waiting sessions, `jev_needs_input`,
   `session_prs`, and PR state from #417.
4. **Push.** One push per `needs-you` or `stalled` episode, text built in code:
   session, host:window, idle duration, and the session's own last line. Tap
   jumps to the session. Stall and waiting collapse into that one push. A
   nudged session sends no push.
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

## Effort

Sizes calibrated as in the check-ins plan (#360 nudges +5.3k, #366 lanes
+1.8k), tests included. Rough, before the open questions are answered.

| # | PR | contents | size |
|---|---|---|---|
| 1 | `session-state: states and episodes` | the state function over existing inputs, an episodes table (migration), persisted push dedupe, `session_state` read | M, ~+1.5k |
| 2 | `session-state: episode push and the Attention section` | one push per episode, the "Stalled / waiting" rows, notices no longer raised, the Chief's `ask` branch retired | M, ~+1.5k |
| 3 | `chief-nudges: widened covering words (shadow)` | cross-session owner messages and Chief-chat messages as quote sources with their source recorded; `no_nudge` decision; tab shows source | M, ~+1.2k |
| 4 | check-ins PRs 2–5 on the shared state, PR 6 reduced to agents' PR attribution | as in `check-ins.md` | as there, minus PR 6's GraphQL client |

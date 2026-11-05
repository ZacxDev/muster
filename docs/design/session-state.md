# Design: session state (stalls, waiting sessions, nudges and check-ins as one model)

**Status: steps 1, 2, 3a and 3b-1 are built (3a behind its own shadow gate,
decision 15; 3b-1 behind its own, default D4); 3b-2 and 4 are proposals.**
Step 3b was split in two by the operator (decision 16, "Chief chat first").
The operator settled the shape
(item, Chief role, states, push, placement, check-ins, phasing, metrics) before
this was written, then answered the draft's open questions and three more
raised by its review; all of it is recorded under
[Operator decisions](#operator-decisions). Four remaining points carry
coordinator defaults, each open to change, under
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
| The concept | One **session state** per session: `needs-you`, `stalled`, `working`, with `done` as a label only. A stop is an **episode**, keyed on the session; everything the owner sees is a view of states and episodes |
| Where it comes from | Code over inputs muster already reads (host status, `stallInputs`, `waitingSessions`), plus **one model input**: the stall classifier's score, which splits `needs-you` from `stalled` for an idle session and is persisted on the episode |
| Waiting windows | Push in code, never through the Chief, **after a 60 s grace** and only if the window is still waiting then — a prompt answered at the terminal never buzzes ([Push](#push)). Questions muster can answer (a held AskUserQuestion, an opencode question) stay **answerable asks** with full text and are not episodes |
| The Chief | Consulted only for **idle** episodes, and only when it is available. It decides **`nudge`, `leave` or `notify`**. No summary, no options, no notice text. `leave` quiets only a session that asked nothing. Covering words widen to the owner's own typed messages in sessions on the **same repository** plus their typed Chief chat, once muster can attribute them; shadow mode first |
| Push | **One push per episode**, claimed in the database before it is sent (at most once; a graceful shutdown drains it). Text built in code: tmux session, window index, host, state and idle time. **No session text.** Tap opens the session. No push for an episode whose nudge was **sent**, or that was left with no question |
| Attention | A **"Stalled / waiting"** section of plain rows, no answer box, oldest first, replacing chief-nudge notices. Left sessions are quiet rows. Answerable asks stay above it |
| Check-ins | Snapshots of the same state at 09:00, 13:00 and 17:00; the Chief narrative on scheduled runs only (an amendment to `check-ins.md` PR 4) |
| Independence | State, rows and pushes are code-only and run with `MUSTER_CHIEF_NUDGES` unset and with no chief. Only the Chief's call needs them |
| What retires | The stall notice (`attention.KindNotice`'s only user), `nudgeNoticeText`, the generic ask text, the Chief's `ask` decision; then `KindNotice` itself once nothing uses it |
| Success | Pushes per day go down; the share of stalled episodes ended by a sent nudge goes up (today: 2 nudges in about 330 decisions over 7 days); zero `needs-you` episodes end unseen |

## Goals

- **The push is the product.** The owner acts from the notification and goes
  straight to the session. What the push says, and that there is exactly one
  per stop, matters more than any page.
- **Facts from code, judgement only where it saves a trip.** Which session,
  where, its state and how long are read, not written. The classifier's score
  is the one model input to the state, and is named as one. The Chief's call
  is spent on the only thing that can spare the owner the trip: a nudge
  carrying their own words.
- **One state, many views.** The Attention section, the push, a check-in and
  the Chief's own tools read the same per-session state, so they cannot
  disagree about who is stalled.
- **Never quieter by accident.** A failure anywhere (the Chief, the
  classifier, a send, GitHub, a host that dropped, the Chief feature turned
  off) leaves a session visible and pushed, never hidden.

## Non-goals

- **Answering for the owner.** A nudge pastes the owner's own earlier words;
  the Chief never composes an instruction.
- **Session text in a push.** The push names the session and its state; it
  never quotes it (see
  [The last line](#the-last-line-why-the-push-does-not-carry-it)).
- **Reading more transcript than today,** with one named exception: once
  cross-session quotes are on (step 3b), the sweep reads the owner's prompts
  in other sessions on the same repository, within `OwnerWindow`
  ([Covering words](#covering-words)).
- **Other owners' sessions.** Row-level security as everywhere else.
- **Rewriting the check-ins design.** This page changes what check-ins read
  from, and amends one PR (PR 4).

## What exists today

### The measurement behind this page

Over the 7 days to 2026-11-03, on the operator's deployment:

- **Chief decisions:** `ask` 187 (144 live, 43 shadow), `leave` 146 (93 live,
  53 shadow), `nudge` 2 (1 live, 1 shadow).
- **The stall asks those live decisions raised** (138 joined by `ask_id`): 125
  were withdrawn because the session moved on, a mean of **102 minutes** after
  they were raised; 8 expired after 24 hours; 5 were still open. So **133 were
  resolved, and 0 were answered.**
- So the owner read none of the Chief's summaries or options, and the one
  outcome that saved a trip (a nudge) happened twice.

### Stall nudges: the sweep, the streak, the decision

- **Candidates.** Every `ChiefNudgeInterval` (2 minutes,
  `internal/api/chief_nudge.go:94`), `stallInputs` (`:319`) lists the owner's
  mirrored Claude Code and opencode windows and dispatched-agent
  conversations. `chiefnudge.Exclude`
  (`internal/chiefnudge/chiefnudge.go:204`) keeps one only if its status is
  `idle` (`:213`), it has been idle for `DefaultIdle` (10m, `:105`), and it
  has no background work, turn error or open ask, and this streak is not
  already decided.
- **The streak.** A mirrored window's `StreakKey` is
  `"since:" + <the status's since-time>` (`chief_nudge.go:346`); an agent
  conversation's is `"msg:" + <its last message id>` (`:389`). A new status
  starts a new streak, so a waiting status and the idle status after it carry
  **different** keys. Decisions are stored once per
  `(owner, kind, key, streak)` (`chief_nudges_one_per_streak`, migration
  `0064:97`), so a streak is judged once, across restarts.
- **`AnchorKey`** is the owner's latest message id in that session
  (`chiefnudge.Anchor`, `chiefnudge.go:349`): a nudge budget, at most
  `MaxNudgesPerAnchor` (2, `:113`) nudges per anchor.
- **The decision costs two model calls.** `decideNudge` (`chief_nudge.go:475`)
  reads the transcript (`nudgeTranscript`, `:727`), runs the stall classifier
  (Jev over OpenRouter, `:507-523`, `internal/chiefnudge/jev.go`), then asks
  the Chief in a throwaway, tool-less lane (`askChiefForNudge`, `:936`). The
  classifier's label and scores are stored on the decision
  (`jev_label`, `jev_needs_input`, `jev_asks_permission`, `0064:86-88`).
- **The backtested rule** (`chief_nudge.go:572-603`): when the classifier says
  the session asked (`AskStall`, `jev.go:297`), the outcome defaults to an ask.
  A Chief `leave` becomes the ask; a nudge survives only on an
  `asks_permission` stall whose quote passes.
- **The nudge's guard is code.** `CheckQuote` (`chiefnudge.go:427`) accepts a
  nudge only if its quote occurs, word-bounded, in one of the owner's own
  messages to **that session** within `OwnerWindow` (6h, `:115`), selected by
  `OwnerMessages` (`chief_nudge.go:493`) minus the candidate session's
  injected set (`:696-709`). Whether the words *cover* the next step is the
  Chief's judgement (`chiefnudge.go:420-426`). The global `DefaultHourlyCap`
  is 6 (`:110`).
- **The stall path is gated on the feature and a chief.** With
  `MUSTER_CHIEF_NUDGES` unset no stall sweep runs (`cmd/muster-server/main.go:992-993`).

### #415: the live `ask` is now a notice

- `actAsk` (`chief_nudge.go:1220`) creates an `attention.KindNotice` row
  labelled with where the session is, holding the Chief's message
  (`nudgeNoticeText`, `:1277`; up to 500 runes). A notice has no options, no
  answer, no relay, and does not count toward the 50-open-ask cap
  (`:1243-1250`). The stall path is `KindNotice`'s only user
  (`internal/attention/attention.go:149`).
- `reconcileNudge` (`:1288`) withdraws a notice once a *read* state shows the
  session moved on (`:1347-1356`).
- The notice is pushed through `askFiled`
  (`internal/api/attention_suggest.go:50`) → `attentionNotify`. Its body is the
  focus title and the Chief's message (`internal/api/attention.go:856-866`).

So #415 removed the answer box but kept the Chief-written text, which is the
part the operator does not read.

### Waiting sessions and their push

- `waitingSessions` (`attention.go:410`) lists windows whose host reports
  status `waiting`: a permission prompt, an AskUserQuestion, an opencode
  question or permission request (#416), a Hermes ask. Held AskUserQuestions
  and opencode questions come back as single **answerable** items
  (`attention.go:425-429`, `:491-497`), pushed with full text under the
  operator's "Full text always" decision (`attention.go:820-827`).
- They push from `attentionMirrorChanged` (`attention.go:960`) **at once**,
  when a session *starts* waiting. Two dedupes, both **in memory**:
  - a host's first observation in this process is seeded silently
    (`seenHosts`, `:119-122`, `:1010-1014`);
  - a session pushed within `attentionRepushAfter` (2 minutes, `:943`) is not
    pushed again ("waiting → busy → waiting is one episode", `:942`).
- A push is cleared through the same in-memory diff (`:1023-1027`), so a
  session that stopped waiting while muster was down keeps its notification.
- **A plain waiting session's push body is summary only, by operator
  decision** (`attention.go:829-835`): `"Waiting: <tool> · <window> @ <host>"`,
  never the command and never what Claude said. Window names are "often
  LLM-written" (`:832-833`).
- The tap route accepts only `ask-N` and `session-<hex16>` keys
  (`internal/api/attention_push_target.go:62`); a window that stopped waiting
  falls back to the queue page (`:46-51`).

### Can one session be pushed twice for one stop?

- **Not at the same instant.** The stall rule requires status `idle`
  (`chiefnudge.go:213`) and the waiting push requires status `waiting`.
- **Across a stop that changes status, yes.** The two pushes use different
  keys and tags (`attention-ask-<id>` for a notice, `attention-<session key>`
  for a waiting session) and different dedupe stores. A session that waits on
  a permission prompt, is let through, then stops 10 minutes later with a
  prose question gets one push for each. This page makes that one episode
  ([Episodes](#episodes)).
- Measured only by reading the code; the double-push rate on the live
  deployment was not counted.

### The last line (why the push does not carry it)

- **A waiting Claude Code window carries it.** `mirror.Pending.Last` is "the
  last thing Claude said", capped at 200 runes, secret-redacted by the host
  and cleaned again by muster (`internal/mirror/pending.go:12-37`, `:152`,
  `:170-174`). It is sent only for `StatusWaiting` windows (`pending.go:31-35`)
  and never for an opencode ask (`:180-182`). The page shows it as the item's
  `Detail` (`attention.go:176-178`); **by operator decision it never reaches a
  push** (`pending.go:27-28`).
- **An idle (stalled) window does not carry it.** Its last assistant message
  is read only by the stall sweep, from the transcript stream
  (`nudgeTranscript`, `chief_nudge.go:727`), and muster deliberately puts **no
  session text** in a stall notice (`jev.go:301-305`).

### Repositories and message provenance

- **A window's repository is a directory name.** The repo chip is "the git
  top-level directory's name … else that directory's own name"
  (`internal/mirrorhost/repo.go:13-15`). It reads no remote. A dispatched
  agent's `Repo` is `owner/name`.
- **A pane's user messages carry no author.** Claude Code records what was
  typed into the pane as user-role text. muster knows only its *own* pastes
  (the send-queue bodies, the candidate session's injected set,
  `chief_nudge.go:696-709`). Text another agent typed with `tmux send-keys`,
  or a paste from anywhere else, is indistinguishable from the owner typing.
- **A Chief chat message carries no sender.** `agents.ChatMessage` is role,
  content and time (`internal/agents/store.go:215-221`). The Chief chat
  receives session questions, plugin messages, relayed answers and task events
  as turns; muster cannot tell from the row which turns the owner typed.

### Check-ins

`docs/design/check-ins.md` is a proposal; only its PR 1 (the fleet collector
split, #374) and its prerequisite (tool-less lanes, #373) are on `main`. Its
stall column already reads `stallInputs` ("Anything that classifies a session
as stalled reads these", `chief_nudge.go:315-317`). Its narrative runs on
every check-in, manual and Chief ones included (`check-ins.md:752`, PR 4 at
`:941`).

## Design

### States

| state | means | computed by |
|---|---|---|
| `needs-you` | the session waits on the owner: a waiting window (prompt, permission) that is not an answerable ask; or an idle session past the stall threshold that the classifier scores as asking | code, from host status (at once, on the mirror hub's change hook); for an idle session, the **classifier** score at or above the ask threshold (`AskStall`) |
| `stalled` | idle past the threshold, no background work, no turn error, not `needs-you` | code, on the stall sweep (`stallInputs` + `Exclude`) |
| `working` | busy, or idle for less than the threshold, or running background work | code, from host status |

**Precedence:** `needs-you` > `stalled` > `working`. **`done` is a label, not
a state** (round 0 D1): a session that ended, or whose PRs are all merged or
closed (`session_prs` + #417's `PullRequestState`), is labelled `done` on its
row and in check-ins, and that label never removes a row or suppresses a
push. A session waiting on the owner is never hidden as done.

**The classifier is the one model input.** It is not a model's opinion only
in the sense that it is a single, backtested score, not the Chief's prose. It
runs on the stall sweep for each idle candidate past the threshold, as today
(`chief_nudge.go:507-523`), **independent of the Chief**. Its label and scores
are persisted on the episode row, so the attention section, the push and
check-ins read the stored score rather than re-classifying. If the classifier
fails or is not configured, the session is treated as asking for the `leave`
rule (below), so the failure cannot quiet it.

A session with a turn error, an officer, the chief, and another owner's
session keep today's exclusions.

### Answerable asks are not episodes

A held AskUserQuestion, an opencode question or permission request muster can
relay, and a Hermes approval with a prompt id are **answerable asks**, as
today: they keep their own full-text push ("Full text always",
`attention.go:820-827`) and their place above the section. Their window does
not open an episode for that wait, so there is never a second, text-free push
for it.

### Episodes

An episode is one stop of one session.

- **Key:** the session (`kind`, `key`: the Claude session id, the opencode
  runtime key, or the agent conversation), not the waiting or idle streak key,
  plus a sequence number. At most one episode per session is open.
- **Opens** when the session enters `needs-you` or `stalled`.
- **Ends** in one of four ways:
  - **the owner acted in the session** (`end_reason` `owner`), on the next
    state pass: a message of the owner's own written AFTER the episode opened
    (a chief nudge, a relayed answer or a routed message muster pasted in does
    not count, nor does an untimed or fenced one), or, for a waiting episode,
    the session no longer waiting (the prompt was answered). The owner's
    message that preceded the stop (the `owner_last` case) does not end it;
    only a newer one does. A session that waits now, or whose status is
    unknown, is not ended this way (`sessionstate.OwnerActed`). The
    conversation is read for this once after the episode opens (or after a
    restart, so episodes opened before the rule existed are read too), then
    again only when the session's streak moves — a record of what was READ,
    kept apart from the streak every pass writes onto the episode;
  - **the owner opened the session in muster** (`viewed`), at once: the
    document of the session's own page — a mirrored window's rendered view or
    terminal page, or an agent's page for the conversation it draws — under
    the owner's principal (the web UI and the app show the same pages). Not
    the bar, the Agents list, the Asks page with its Stalled / waiting row, the
    tap route on its own (it redirects to the session's page, which counts),
    the view's socket or fragments, a prefetch (`Sec-Purpose` / `Purpose:
    prefetch`) or a `HEAD`, and never another user's request. Looking at the
    window in tmux outside muster cannot be seen. Its own reason, not
    `owner`'s: the end reason records why a push cleared, and "looked" is not
    "answered" (`internal/api/session_viewed.go`);
  - **sustained work** (`worked`): the session has been `working`
    continuously for at least `N` (default 120 s), or a turn completes after
    at least `N` of work. A busy blip shorter than `N` that the owner did not
    start (a quick tool call, a background task's notice) does not end it;
  - **gone** (`gone`): its window left a host that is online and reporting.

  **A handled stop is not opened again on the same streak**
  (`sessionstate.Handled`: `viewed`, and the one-time `reset` below): while
  the session sits where it was, neither the pass nor the waiting hook opens
  a new episode for it, so the stop the owner dealt with is not pushed again.
  Once its streak moves (a new status, a new message) the next stop is a new
  episode as usual. A view ends its episode on the streak the page showed
  (the session's current one, not the one the last pass wrote), so the stop
  the owner looked at is the one treated as handled.

  **`owner` is not handled.** It means the owner answered, so the session
  moved, and any stop after it is new. The episode ends on the pass that READ
  the owner's message, which can be a pass later than the one that first saw
  the session stop again (a failed read is retried), so an `owner` episode can
  end carrying the NEXT stop's streak. Treating that as handled would suppress
  the real next stop's push for as long as the session sat on it.

  **A handled stop opened again anyway is ended again**
  (`sessionstate.PG.EndRepeat`): an open episode whose session is on the
  streak its previous episode ended handled on is ended for that same reason,
  with that streak, and its push withdrawn, on the next pass. The reopen
  guard (`PG.Open`) cannot refuse every one, so this is a permanent repair,
  not a rollout step. Three writers produce one:
  - the image before 0079, still running during a rolling update, opens and
    pushes a session still stopped after the reset;
  - the waiting hook (the older image's, and this one's whenever
    `windowStreakKey` returns `""` because the host reports no status time)
    opens with no streak, and a streak of `""` never matches the guard;
  - a pass whose facts were read before a concurrent view opens on the
    streak it read, which is not the one the view ended on.

  The streak compared is the session's current one, so each is ended within
  one pass, and the repush floor holds the next push. The reset therefore
  needs no manual deploy step.

  **One stop of a session that carries on by itself is one episode**
  (default D1): `needs-you` → `stalled` within it updates its state and does
  not push again.
- **Repush floor:** a session whose previous episode pushed within 2 minutes
  is not pushed again for the next one, the same guard as today's
  `attentionRepushAfter` (`attention.go:943`).

Scenario: three Bash permissions in a row, each approved from tmux within
seconds. Today that is one push (the 2-minute guard); here each approval ends
its episode (the owner acted), and the next prompt's episode is held by the
repush floor and ended by its own approval before the floor passes: still one
push.

### Claiming, pushing and clearing (the episodes table)

One table, `session_episodes`: owner, session kind and key, sequence, state,
classifier label and scores, opened/ended times, `pushed_at`, `cleared_at`,
the Chief decision (if any) and the nudge's send-queue id.

- **Claim before push.** Opening an episode is an
  `INSERT … ON CONFLICT DO NOTHING` on a partial unique index over
  `(owner, kind, key) WHERE ended_at IS NULL`, the pattern of
  `chief_nudges_one_per_streak` (`0064:97`). Only the inserting process
  pushes. That resolves the double rollout's race.
- **`pushed_at` is the push's claim**, written (`UPDATE … WHERE pushed_at IS
  NULL`) immediately BEFORE the push is handed to the sink, by whichever
  process takes the row — so the push is **at most once**. A graceful
  shutdown drains every handed-off push (`WaitForPushes`), so only a crash in
  the moment between the claim and the hand-off loses one. An open row with
  `pushed_at` null and no reason to stay quiet is pushed by the next pass,
  whichever process runs it.
- **Restart.** Open rows survive a restart, so nothing re-pushes. A session
  found `needs-you` or `stalled` with no open row, including one that began
  while muster was down, gets a row and **is pushed**: no row means nobody was
  notified. (This replaces today's silent seeding for these pushes.)
- **Clearing** is from the table: ending an episode sets `ended_at`, sends the
  resolved push for its tag, and sets `cleared_at`. A pass after a restart
  ends and clears any open row whose session is now working past `N`, gone,
  or acted in by its owner, so a notification for a session that moved on
  during downtime is cleared. The same rule withdrew the **backlog**:
  migration 0079 ended every episode open when it applied (`end_reason`
  `reset`, once), and the next pass sent each pushed one's resolved push and
  set its `cleared_at`; a session still on the same streak is not reopened
  (handled), and a reopen by the older image mid-rollout is ended again
  (`EndRepeat`, above, which also repairs the repeats this image's own
  waiting hook and racing passes write).

This table **replaces `KindNotice`'s use for stalls** (round 0 D4). Once
nothing raises a notice, `KindNotice`, `ErrNotice` and its push and tap cases
(`attention.go:856`, `attention_push_target.go:151`) are deleted.

### Who decides what

| episode | decided by | result |
|---|---|---|
| a waiting window, not answerable | **code only, after its grace** | row at once (a plain waiting row while in its grace); push only if still open once the grace has passed. Never sent to the Chief, never nudged |
| an idle `needs-you` (classifier: asked) | code; the Chief **if available** | `nudge` (sent) → no push. Anything else, **`leave` included** → push and row |
| an idle `stalled` (classifier: did not ask) | code; the Chief **if available** | `nudge` (sent) → no push. `leave` → quiet row, no push. `notify` or anything else → push and row |

**`leave` quiets only a session that asked nothing** (operator decision 12).
A session the classifier says asked always pushes once per episode unless a
nudge to it was sent. This keeps the backtested rule
(`chief_nudge.go:572-603`).

**Independence** (F3d): the state, the episodes, the rows and the pushes run
with `MUSTER_CHIEF_NUDGES` unset and for an owner with no chief. Then every
idle episode is `notify`. `MUSTER_CHIEF_NUDGES` becomes the switch for the
Chief's call and nudges only.

### The Chief: nudge, leave or notify

Per **idle** episode, at most once, when the Chief is available:

1. Code builds the candidate as today, with the classifier's stored score.
2. The Chief returns one of:
   - `nudge`: with quote, quote source and covered action. For a `needs-you`
     session, the quote must answer the question the session asked.
   - `leave`: the session needs nothing from the owner now.
   - `notify`: the owner should look.

   No question, no summary, no options.
3. **A nudge is handled only once its paste is sent** (F3b). It is queued
   through today's send path (the send queue for a mirror, a turn for an
   agent), the hourly cap and `MaxNudgesPerAnchor`. The episode's push is
   **held** while the nudge is queued; if the send is held past its bound,
   fails, is cancelled or is capped, the episode is pushed. A queued nudge is
   **dropped** if the session's streak or episode changes before it pastes
   (F4), and the episode is then pushed.
4. **Shadow mode never suppresses a push** (F3c). A shadow `would_send` is
   recorded and the episode is pushed as `notify`.
5. Any failure (the Chief's call, an unparseable answer, a lane refusal past
   its bound) is `notify`, never `leave`.

**`needs-you` nudges** (decision 11) go only to **idle** sessions the
classifier marks as asking. A waiting window (`StatusWaiting`) is never
nudged: the send queue does not paste into one (`sendqueue/dispatcher.go:71-73`),
so a queued nudge would wait for the owner to answer by hand and then paste
into the next, different context.

**View-only runtimes** (default D3): opencode and Hermes windows get the same
push and row, but are **never nudged**; a Chief `nudge` for one is recorded
and treated as `notify`, as #415 already treats a runtime window.

Shadow first: decisions are recorded on the Chief nudges tab, and nothing is
sent, until the owner is listed live (`MUSTER_CHIEF_NUDGE_LIVE_OWNERS`). A
`needs-you` nudge has a **separate** live list on top of that one
(`MUSTER_CHIEF_NUDGE_NEEDS_YOU_LIVE_OWNERS`, decision 15): it is sent only for
an owner on both, and is shadow for everyone else.

### Covering words

What a nudge may quote, in two stages.

**Step 3a: unchanged sources.** The owner's messages in the candidate session
itself, exactly as today: `OwnerMessages` minus that session's injected set,
with a partial read authorising nothing. Only the `needs-you` nudge and the
`nudge`/`leave`/`notify` shape are new.

**Step 3b: widened sources** (decisions 10 and 13), on only once muster can
attribute a message to the owner. Split in two (decision 16, "Chief chat
first"):

- **3b-1 (built, shadow): the Chief chat.** A sender record on Chief-chat
  turns, `chat_messages.typed_by` (migration 0081), written at the one place a
  person's line becomes a turn (`runEnteredTurn`) from the owner's signed-in
  chat doors only (`ownerTypedBy`: the chat page's socket and the composer's
  POST, web or phone). Every other writer leaves it NULL, and a ledger test
  (`TestEveryChatWriterIsClassified`) pins every writer of a chat row with its
  kind. Rows from before 0081 are not owner-typed (no backfill). **The CLI
  is not counted** (decision 13 as amended 2026-11-04, "Exclude the CLI"):
  `muster chief ask` arrives over a token, the same door a session's own
  question uses, which any session of the owner's can hold. With session
  state on, the nudge's quote may come from those typed turns within
  `OwnerWindow`, for any of the owner's sessions; the Chief names the quote's
  source (`quote_source`) and code checks the quote verbatim in that source
  only; an unverifiable one meets the one quote guard
  (`suppressed:quote_unverified`, its source recorded, the stop pushed); the
  Chief chat is read newest first, only the `MaxOwnerMessages` (20) the chief
  is shown, and a failed read contributes nothing. Shadow first: such a nudge
  is live only for an owner also on
  `MUSTER_CHIEF_NUDGE_CHIEF_CHAT_QUOTES_LIVE_OWNERS` (default D4), and so is
  ANY nudge decided while typed Chief-chat lines were in the prompt, whatever
  its quote's source (decision 17; `chief_nudges.chief_chat_in_view`); both
  re-read at the paste, at an agent's turn and at the reconcile retry; the source is recorded on the decision
  (`chief_nudges.quote_source`) and shown on the Chief nudges tab. Same-session
  quotes keep their gates. Session state off is unchanged.
- **3b-2 (proposal): panes.** Host provenance for typed pane input, normalised
  origin remotes, and same-repository cross-session pane quotes. It needs a
  host ship, and same-repository matching waits for host-reported remotes.
  The Chief chat needs neither: it is a source for any of the owner's
  sessions.

The full shape, both halves:

- **Same repository** means the **normalised origin remote** (`owner/name`),
  not the repo chip's directory name: two unrelated checkouts both named
  `api` are different repositories, and two worktrees of one repository are
  the same one. A host reports each window's normalised origin remote; a
  dispatched agent's `Repo` is already `owner/name`. A session with no
  resolvable remote **gives and gets no cross-session quotes**.
- **Owner-typed only.** A quote's source message must be one muster can
  attribute to the owner:
  - **Chief chat:** only turns muster records as typed by the owner in the
    signed-in web or phone chat (decision 13 as amended: not the CLI). Relayed answers, session questions, plugin messages, task
    events and agent-sent turns never count, fenced or not (decision 13).
    Today no such record exists (`store.go:215-221`); step 3b adds a sender
    column written at the point a turn is typed.
  - **Other sessions' panes:** muster cannot tell today whether a pane's
    user-role text was typed at the terminal by the owner or sent there by
    another agent or a paste. Until a host can attribute pane input (a
    provenance record a host writes for typed input), **cross-session pane
    quotes stay off**. Every source session's injected set (muster's own
    pastes) is excluded, and a source with a partial read contributes nothing.
- **Window:** every source, the Chief chat included, is read within
  `OwnerWindow` (6h).
- **The transcript non-goal:** step 3b reads the owner's prompts in other
  same-repository sessions within `OwnerWindow`. That is the one exception to
  "no more transcript than today", and it is built only with the attribution
  above.

### Push

One push per episode, under the existing `attention` toggle:

- **Title:** the window as every attention text names it (`mirror.Where`):
  the host's label for it, then its tmux session, window **index** and host,
  e.g. `gold:3 · tmux alpha:3 @ host-a needs you` or `… is stalled` — resolved
  by the episode's session key when the push is sent, so a renumbered window
  names its new index. When no window holds the session then (a `/clear` or a
  resume gave the window a new conversation), it falls back to the place
  stored on the episode, **still under the host's codename** whenever the host
  labels that tmux session — the stored window as it is listed now, with its
  `<codename>:<index>`, or, when it is gone, the stored target under the
  session's codename with no index (`gold · tmux alpha:3 @ host-a needs
  you`), since an index from another window's label would point at that
  window — and only with no label for that session the bare target
  (`tmux alpha:3 @ host-a needs you`); a window whose host has just
  dropped is followed by `(host offline)`, as a notice's is. *(As first built
  the fallback dropped the label even while the host still gave one; fixed
  2026-11-05, #453.)* A
  dispatched agent is named by its agent name. *(As first built, the title was
  only the stored tmux session and index — the one surface not naming the
  window by its label, which the operator could not place; the label is the
  host's codename from its labels file, not session text, and the waiting
  push this replaced already carried it.)*
- **Body:** the state and idle time, e.g. `Needs you · idle 12m`. **No session
  text**: no last line, no tool call, no focus title. This keeps both earlier
  operator decisions: a plain waiting session's push is summary only
  (`attention.go:829-835`), and muster puts no session text in a stall item
  (`jev.go:301-305`).
- **Tag:** the episode, so a re-push replaces rather than stacks and an ended
  episode clears it.
- **Tap:** opens the session. This needs new route work: the tap route takes
  `ask-N` and `session-<hex16>` only (`attention_push_target.go:62`) and falls
  back to the queue for a window that stopped waiting (`:46-51`). Step 2 adds
  an `episode-<id>` key that resolves to the session's current window (or the
  agent's chat) and falls back to the row on the page.
- **No push** when the episode's nudge was sent, when it was left with no
  question, or when it ended before it was pushed.
- **When a waiting window pushes: after its grace, not at once.** As first
  built (PR 2, #430) a waiting window muster cannot answer was pushed the
  moment the hub's change hook saw it. Measured on a live fleet over 12 hours,
  that buzzed the phone for nearly every permission prompt the owner was about
  to answer at the terminal: of the waiting episodes that ended with the owner
  answering, the median answer came about 20 seconds after the episode opened,
  and all but a handful within 60 seconds. So the hook now opens the episode
  and **holds** it (the verdict `hold`, as an idle stop waiting on the chief
  is held: nothing is written, and its row is a plain waiting row), and the
  state pass pushes it once the grace has passed (60 s, a constant:
  `EpisodeWaitingGrace`; live data, 165 of 169 owner-answered waits inside
  60 s, showed no need to tune it) — **only if the episode is still open**:
  one the owner answered, viewed, worked past, or whose window went, ended in
  between and is never pushed. The pass runs every 30 s, so the push lands
  60..90 s after the window started waiting. The grace runs from the
  episode's opening, recomputed from `opened_at` on every pass; nothing new
  keeps the hold, so a restart mid-hold pushes it on the next pass, and the
  `pushed_at` claim still sends it once across the hook, the pass and
  replicas. Answerable asks are not episodes and are not held.

### Attention: "Stalled / waiting"

A section below the answerable items: one row per open episode, oldest first,
showing the tmux session and window index, host, state, idle time and the
`done` label where it applies, with a "View in session" link and no answer
box. A left episode is a quiet row (drawn muted, counted apart, never pushed).
The page may still show a waiting window's `Detail` (its last words) as it
does today; the push never does. Answerable asks stay above it, unchanged.

### Check-ins on the shared state

Check-ins snapshot this state rather than re-deriving it. Against the
check-ins plan (`docs/design/check-ins.md`, "Phased plan"):

| check-ins PR | effect |
|---|---|
| 2 `code-only reports` | its stall and needs-you columns read the session state and episodes instead of `stallInputs` + `attentionItems` separately; the rest stands |
| 3 `push` | unchanged; its push is a scheduled digest and never duplicates an episode push |
| 4 `chief narrative` | **amended:** the narrative runs on **scheduled** check-ins only (decision 6). Manual and Chief check-ins (`checkin_now`) are code-only. `check-ins.md` is updated to say so in the PR that builds it |
| 5 `chief tools` | gains the `session_state` read, which moves here from this design's PR 1 (round 0 D3) |
| 6 `merged PRs` | **mostly subsumed**: #417 already reads a PR's state with a repo-scoped token and caches it (`internal/api/pr_closed.go:39-43`); check-ins reuse `PullRequestState` instead of a new GraphQL client. Agents' PR attribution remains |

## Risks

- **A wrong nudge types as the owner.** Code guarantees the words are the
  owner's; it cannot check they cover *this* step. A `needs-you` nudge answers
  a question on the owner's behalf, and a cross-session quote ("merge it")
  may have meant something else. Shadow mode, the hourly cap,
  `MaxNudgesPerAnchor`, dropping a nudge whose episode moved on, and the Chief
  nudges tab's audit rows (with the quote's source) are the mitigations; none
  prevents a wrong nudge.
- **Quote provenance.** Widened quoting is only as safe as the attribution
  behind it. That is why step 3b waits for an owner-typed record on both the
  Chief chat and panes, and why a session with no resolvable remote gets no
  cross-session quotes.
- **Push dedupe across restarts.** Moving from in-memory seeding to persisted
  episodes changes behaviour after a restart: a session that began waiting
  during downtime now pushes (by design), and notifications for sessions that
  moved on are now cleared.
- **Push volume.** `leave` (146 of 335 decisions today) still quiets sessions
  that asked nothing, but no longer quiets one the classifier says asked, and
  every idle episode pushes when there is no chief. Merging a waiting window
  and the stall that follows into one episode removes today's second push for
  one stop.
- **The classifier is a dependency of the state.** Its failure is handled
  (treated as asking, so pushed), but its quality now decides which rows can
  be quieted.

## Rollout

1. **#415 + #416** deployed (in progress separately).
2. **Session state, episodes, push, tap route and the Attention section.**
   Code-only, independent of the Chief feature. Stall notices stop being
   raised; open ones are withdrawn by the existing reconcile. The Chief's
   `ask` becomes the text-free `notify`, and the backtested rule keeps an
   asking session pushed.
3. **a.** `needs-you` nudges and the `nudge`/`leave`/`notify` decision, same
   session quotes only, shadow first — under their own live list,
   `MUSTER_CHIEF_NUDGE_NEEDS_YOU_LIVE_OWNERS`, so an owner already live for
   today's stalled nudges stays live for those while their needs-you nudges
   stay shadow (decision 15). Each episode is put to the Chief at most once
   (`chief_nudges.episode_id`, migration 0078). **b.** Owner-typed provenance,
   split (decision 16): **b-1** a Chief-chat sender column (migration 0081),
   then Chief-chat quotes, shadow first under
   `MUSTER_CHIEF_NUDGE_CHIEF_CHAT_QUOTES_LIVE_OWNERS`; **b-2** a host record for
   typed pane input and normalised origin remotes, then same-repository
   cross-session pane quotes, shadow first.
4. **Check-ins** PRs 2–5 on the shared state with PR 4 amended, PR 6 reduced.

Each is its own PR with tests and the audit ladder. `MUSTER_CHIEF_NUDGES`
unset turns off the Chief's call and nudges only; the state and pushes stay.

## Open questions

None. The draft's seven questions and the review's three were answered by the
operator (decisions 4, 9–14, and 13's amendment) or carry a default below.

## Defaults (open to change)

Taken by the coordinator, not decided by the operator. Each can be changed
without reworking the rest of the design.

- **D1. One stop is one episode.** A `needs-you` → `stalled` change within a
  stop does not push again; an episode ends after `N` (120 s) of work, or at
  once when the owner acts in the session (a message of their own after it
  opened, or a waiting prompt answered) or opens it in muster.
- **D2. Keep the stall classifier,** in the roles above: it splits
  `needs-you` from `stalled` for an idle session, and gates the `needs-you`
  nudge.
- **D3. View-only runtimes.** opencode and Hermes windows get the same push and
  row, but are never nudged.
- **D4. Chief-chat quotes' own shadow gate** (step 3b-1). A nudge quoting the
  owner's Chief chat pastes only for an owner on
  `MUSTER_CHIEF_NUDGE_CHIEF_CHAT_QUOTES_LIVE_OWNERS` (same format as the other
  lists) **and** on every list the nudge is otherwise under; unset, every
  Chief-chat quote is shadow. Decision 16 fixes only that 3b-1 ships shadow
  first; the separate list is this default. Decision 17 widens what it gates
  to any nudge decided with the Chief chat in view.

## Operator decisions

Recorded 2026-11-03.

1. **Item shape.** The operator acts from the push notification only, never
   reads the Chief's summary or options on stall asks, and goes straight to
   the session. Measured over 7 days: of 138 stall asks, 125 were cancelled
   because the session moved on (average 102 minutes after the ask), 8
   expired and 5 were still open; 133 resolved, 0 answered.
2. **Chief role.** It decides whether to nudge, with no summary and no
   options. Keep nudging and widen it: covering words may be the owner's own
   typed messages in other sessions, plus their messages to the Chief, quoted
   verbatim. Nothing is inferred from rules files. Shadow mode first, then
   live per owner, under the existing hourly cap. (Narrowed by decisions 10
   and 13.)
3. **States.** `working` / `needs-you` / `stalled` / `done`, from existing
   inputs: the stall rule, waiting sessions, `jev_needs_input`, `session_prs`,
   and PR state from #417. (The review made `done` a label only: round 0 D1.)
4. **Push.** The operator acts from the push notification alone ("Only the
   push notification"). One push per `needs-you` or `stalled` episode, text
   built in code: session, host:window, state and idle time, and **no session
   text**. This keeps the earlier decisions at `attention.go:829-835` and
   `jev.go:301-305`. Tap jumps to the session. Stall and waiting collapse into
   that one push. A nudged session sends no push. (The draft also listed the
   session's last line here; the operator never asked for it, and it is
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
   **zero `needs-you` episodes end unseen** (no push and no visit). (The draft's
   "zero stall items expire unread" could not be measured once notices
   retire.)
9. **`leave` stays.** The Chief keeps its `leave` decision. A left session
   shows as a quiet row on the Attention board and never pushes.
10. **Quote sources: same repository only.** The owner's messages in sessions
    on the same repository, plus their Chief chat. This supersedes "any
    session" in decision 2.
11. **`needs-you` nudges: yes.** A `needs-you` session is eligible for a nudge
    when quoted words answer its question, shadow mode first.
12. **`leave` on an asking session: "Push it anyway".** `leave` quiets only a
    session with no question. A session the classifier says asked always
    pushes once per episode unless it is nudged. This keeps the backtested
    rule (`chief_nudge.go:572-603`).
13. **Chief-chat quotes: owner-typed only.** Only turns muster records as typed
    by the owner (web, phone, CLI) count. Relayed or agent-sent turns never
    count, even unfenced ones. **Amended 2026-11-04 ("Exclude the CLI",
    answering #439's round 0):** owner-typed means turns typed in the
    signed-in web or phone chat only. `muster chief ask` from the CLI does not
    count: it shares the token door any session can hold.
14. **Verbatim quotes stay.** The owner's words are quoted verbatim in this
    public doc ("Keep verbatim").
15. **`needs-you` nudges: a separate shadow gate** (recorded 2026-11-04, while
    building step 3a; it departs from "until the owner is listed live" above).
    The owner already listed in `MUSTER_CHIEF_NUDGE_LIVE_OWNERS` for today's
    stalled nudges keeps those live. A `needs-you` nudge has its own list,
    `MUSTER_CHIEF_NUDGE_NEEDS_YOU_LIVE_OWNERS` (same format), and pastes for
    real only for an owner on **both** lists; otherwise it is shadow — recorded
    on the Chief nudges tab, never pasted, and the episode pushed. Unset, every
    owner's `needs-you` nudges are shadow ("Separate shadow gate").
16. **Step 3b split: "Chief chat first"** (recorded 2026-11-04). Step 3b ships
    as two PRs. **3b-1** is server only: owner-typed provenance for the Chief
    chat (a sender record written where a turn is typed), then Chief-chat
    quotes as covering words, shadow first. **3b-2** follows with host pane provenance, normalised origin remotes and
    same-repository cross-session pane quotes, which need a host ship.
17. **"Shadow whole decision"** (recorded 2026-11-04, answering #439's
    round 1). The Chief sees the owner's typed Chief-chat lines on every stall
    check with session state on, and they can steer a decision whatever it
    quotes. So any nudge decided while Chief-chat lines were in the prompt is
    shadow, whatever its `quote_source`, until the owner is on the Chief-chat
    list (default D4) on top of the existing lists. The decision records it
    (`chief_nudges.chief_chat_in_view`, migration 0081), and the paste gate,
    an agent's turn and the reconcile retry read that stored fact. With no
    typed line in the window the decision is gated as before.

## Effort

Sizes calibrated as in the check-ins plan (#360 nudges +5.3k, #366 lanes
+1.8k), tests included. Rough.

| # | PR | contents | size |
|---|---|---|---|
| 1 | `session-state: states and episodes` | the state function with precedence, the classifier run independent of the Chief and stored on the episode, the `session_episodes` table with claim-before-push, restart and clearing rules | M, ~+1.5k |
| 2 | `session-state: episode push, tap route and the Attention section` | one text-free push per episode, the `episode-<id>` tap route, the "Stalled / waiting" rows (left ones quiet), answerable asks kept apart, notices no longer raised, the Chief's `ask` replaced by `notify`; then `KindNotice` deleted | M, ~+1.8k |
| 3a | `chief-nudges: needs-you nudges, nudge/leave/notify (shadow)` | idle `needs-you` nudges gated by the classifier, sent-only handling, dropped-on-change, shadow never suppressing a push, view-only runtimes never nudged | M, ~+1k |
| 3b-1 | `chief-nudges: owner-typed Chief-chat provenance and Chief-chat quotes (shadow)` | Chief chat sender column (0081) and its writer ledger, Chief-chat quotes with a named, verified source and a partial read giving nothing, their own shadow gate, the source on the decision and the tab | M |
| 3b-2 | `chief-nudges: host pane provenance and same-repository quotes (shadow)` | host provenance for typed pane input, normalised origin remotes, per-source injected sets and partial reads, same-repository cross-session pane quotes | L |
| 4 | check-ins PRs 2–5 on the shared state (PR 4 amended, PR 5 with `session_state`), PR 6 reduced to agents' PR attribution | as in `check-ins.md` | as there, minus PR 6's GraphQL client |

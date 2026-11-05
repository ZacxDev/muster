# Chief stall nudges

A session that stops short — it finished a step and asked "shall I merge?" in
prose, or it is waiting on a question you never saw — sits idle until you look.
**Chief stall nudges** let your [chief](chief.md) find such sessions and either
nudge them on, when one of your own messages to that session already covers the
next step, or point you at the session with a [notice](attention.md#notices) on
the Attention page: where it is, and the chief's short message. A notice has
nothing to answer. You go to the session (a host's jump key takes you there:
[below](#jumping-to-a-notice)).

**It ships in shadow mode.** With the feature on, the chief judges your stalled
sessions and every decision is recorded on the Asks page's **Chief nudges**
tab. Nothing is sent and nothing is asked until an operator lists you as live
(`MUSTER_CHIEF_NUDGE_LIVE_OWNERS`).

**It is a backstop.** The first catch for a session that asked a question in
prose is a per-session Stop hook (outside this repository). The sweep catches
whatever is still idle after that.

## Turning it on

| variable | what it does |
|---|---|
| `MUSTER_CHIEF_NUDGES` | `1` runs the sweep, in shadow mode for every owner. Off by default. Needs a database. |
| `MUSTER_CHIEF_NUDGE_LIVE_OWNERS` | The user ids, comma-separated, whose decisions are **acted on**. Everyone else stays in shadow mode. |
| `MUSTER_CHIEF_NUDGE_NEEDS_YOU_LIVE_OWNERS` | The user ids, comma-separated, whose [needs-you nudges](#needs-you-nudges) are sent too. A needs-you nudge is sent only for an owner on **both** lists; for everyone else it is shadow. Unset: every needs-you nudge is shadow. |
| `MUSTER_CHIEF_NUDGE_CHIEF_CHAT_QUOTES_LIVE_OWNERS` | The user ids, comma-separated, whose nudges [quoting their Chief chat](#chief-chat-quotes) are sent too. Such a nudge is sent only for an owner on this list **and** every list it is otherwise under; for everyone else it is shadow. Unset: every Chief-chat quote is shadow. |
| `MUSTER_CHIEF_NUDGE_IDLE` | How long a session must have been idle to count as stalled. Default `10m`, from `2m` to `24h`. |
| `MUSTER_CHIEF_NUDGE_HOURLY_CAP` | The most live nudges per hour, across every owner. Default `6`. Shadow mode is not capped. |

All of them, and `MUSTER_CHIEF_NUDGE_ASK_THRESHOLD` (below), are read at boot;
the boot log says which mode the sweep is in, how many owners are live for
needs-you nudges, and how many for Chief-chat quotes.

**The kill switch** is unsetting `MUSTER_CHIEF_NUDGES` (or removing an owner
from the live list, or from the needs-you list for their needs-you nudges) and
restarting. The switch reaches messages already queued:
each one is checked again when it would be pasted, and a nudge whose owner is no
longer live, or whose feature is off, fails with *Not sent: chief nudges are
off, or in shadow mode for you, on this server* and is never pasted.

### Rolling out

This is the one place the rollout steps are written down; the other docs and
the code comments link here.

A `claude-code` chief runs each check in a lane that is **throwaway**
(`X-Muster-CC-Ephemeral`) and **tool-less** (`X-Muster-CC-Toolless`,
[below](#a-check-cannot-call-tools)) only if its pod's ccd knows those marks,
and that ccd ships in the same release as the `claude-code-agent` image. An
older ccd ignores both: it keeps every check's CLI, at its lane cap stops an
idle one to start the next — which can be one of your own sessions (see
[Limits](#limits)) — and starts each check's CLI with the chief's full tool
set, its built-in tools and muster's MCP tools. muster neither detects that
nor holds the check back: **this is rollout discipline, not something muster
enforces.** So after deploying this muster:

1. restart the chief onto the new `claude-code-agent` image (the chief can do
   it itself with the `chief_restart` MCP tool);
2. confirm the chief pod's ccd `/healthz` says `"toolless_lanes": true` at
   its top level (an older ccd has no such field);
3. only then turn on (or re-enable) `MUSTER_CHIEF_NUDGES`, and leave it off
   while any chief pod still runs an older image.

Nudges are already on in the current deployment, so there the protection
starts only once `MUSTER_AGENT_CC_IMAGE` has been bumped to the new image
**and** the chief has been restarted onto it: do both as part of the same
rollout.

While a check runs, its lane also shows on `/healthz` with `"toolless": true`.

**Rolling out notices (migration 0071).** A rolling deploy keeps the old pod
serving for a while after the migration has run. Until it terminates, what it
answers is the old code over the new schema:

- it counts notices toward the 50-open-ask cap, so a real question raised
  through it can be refused near the cap;
- it draws a notice as an answerable ask, and an answer it takes is refused by
  the database (`attention_asks_kind_check`), so that answer fails with a 500;
- it can still claim an answer to a nudge ask for a relay (`relay_queued`); the
  new sweep fails such a decision on its next pass, with its audit row, and
  sends nothing.

All three end when the old pod is gone. No step is needed.

**Turning on session state (`MUSTER_SESSION_STATE=1`, migrations 0073 and 0077).**
Flipping it (a restart) and the rolling deploy that carries it have two
one-time effects, and no step fixes either:

- **A notification shown before the flip stays.** A waiting window's
  `attention-session-…` notification, and a notice's, shown by a process with
  the flag off, is not withdrawn by a process with it on: such a window is now
  an episode, outside the old diff that would have cleared it. Swipe it away;
  the episode's own push replaces it on the next stop.
- **Until the old pod is gone, both push.** The old pod keeps pushing
  `attention-session-…` for waiting windows and raising notices, while the new
  one pushes `attention-episode-…`, so one stop can notify twice during the
  overlap. It ends when the old pod terminates.

Turning it back off is the same in reverse: open episode notifications are not
withdrawn by a process with the flag off.

**Rolling out needs-you nudges (migration 0078).** A nudge an older image
queued before the deploy carries no episode. With session state on it is still
held to the [needs-you](#needs-you-nudges) rule when it is sent: one for a stop
the classifier said asked is pasted only for an owner on both live lists, and
fails with the kill switch's words otherwise.

## What counts as stalled

Every 2 minutes the sweep looks at each owner who has a chief (and has it
turned on). It considers:

- every mirrored **Claude Code** window on a host that reports Claude Code's
  status (one entry per Claude session, whatever window shows it);
- every mirrored **opencode** window that names its opencode session, on a host
  that reports status (one entry per opencode session, keyed
  `opencode:<id>`). Its status and status time come from the host's opencode
  event stream, and its messages from the rendered view's backfill, as for
  Claude Code. It is **view only**: muster cannot type into it, so it is never
  nudged; a nudge for it becomes a notice ([Limits](#limits));
- every **dispatched agent** of the owner's.

It never considers an [officer](officers.md) (officers escalate through HQ),
the chief itself, or another user's agents. An agent's **conversations** are
judged one by one (those active in the last 24 hours): the one that stalled is
the one decided about and nudged.

A session is a **candidate** only when all of these hold:

| rule | mirrored window | dispatched agent |
|---|---|---|
| idle | Claude Code reports `idle`, on a host that reports status | running, no turn in flight, no kickoff owed |
| for long enough | idle for at least `MUSTER_CHIEF_NUDGE_IDLE`, measured on muster's clock from when muster first saw that status | its last message is at least that old |
| no background work | no background shell or subagent running | — |
| no turn error | the last turn did not end in a usage limit, an auth failure or an API error, and the send queue is not holding the window | the last message is not a failed turn |
| nothing open | no open attention ask or chief question of its own, and no nudge decision still in flight | same |
| the session spoke last | the newest message is the session's, not yours | same |
| not decided yet | this **idle streak** has no decision | same |

An idle streak is one stretch of idleness: for a window, the time Claude Code
wrote its status; for an agent, its last message. Any new turn starts a new
streak, so each streak is judged at most once.

**Only candidates reach the chief.** A sweep with no candidate makes no model
call. A sweep puts at most 5 sessions to the chiefs, across all owners.

## The stall classifier

Before the chief is asked, the candidate's **last message** is classified:
does it end by asking you something? The model is Jev (TypeSafe), `~typesafe/jev-latest`,
called through OpenRouter's Decisions API (`POST /api/alpha/decisions`, one
`choice` question) on **your OpenRouter key**, resolved as for focus summaries.
The three labels and their rubrics are the operator-approved, backtested ones
(one constant, `chiefnudge.JevRubrics`, pinned by a test):

| label | meaning |
|---|---|
| `needs_input` | it ends asking a question or a decision only you can give (not permission for a step it already proposed) |
| `asks_permission` | it is holding its own next planned step until you approve it ("shall I…") |
| `neither` | a finished report, status, work in progress, or an optional offer |

The classification is **shared with session state** (`internal/api/session_state.go`;
the design, `docs/design/session-state.md`, #425), which runs only with
`MUSTER_SESSION_STATE=1`. Its own pass, every 30 seconds, runs the classifier
**only where this sweep would**: the feature on and your chief on. It runs at
most once per idle streak, and at most 5 transcript reads a pass. It sends
**the same text** (the one function both use: cut, then redacted, as the chief
sees it), and the answer is billed once per streak whichever runs first: each
reads the other's stored answer (this sweep's decision row, the session's
episode). Where the classifier does not run, session state reads no transcript
and counts every idle stop as one that asked you.

With `MUSTER_SESSION_STATE=1` the chief answers **`nudge`, `leave` or
`notify`** (session-state step 3a, [design](design/session-state.md#the-chief-nudge-leave-or-notify)):
no question, no summary, no options. A `notify` raises **no notice** at all:
it is recorded (`notified`, in either mode) and the stop's episode carries the
one text-free push ([attention.md](attention.md#stalled--waiting-session-state)),
which also follows this sweep's other decisions: a nudge counts as handled only
once its paste is **sent** (the stop is then quiet), a queued one holds its
push (for at most 10 minutes), one that failed, was cancelled, capped or
dropped pushes, and a `leave` quiets the stop only when the classifier says the
session asked nothing. Shadow mode never quiets a stop. Any failure (the call,
an unreadable answer, a refusal past its bound) pushes, never quiets. An answer
of `ask` is read as `notify`. Each **episode** (one stop) is put to the chief
at most once: a later idle streak of the same stop is pushed, not judged again
(`chief_nudges.episode_id`, migration 0078), and the sweep waits for the state
pass to open the stop's episode before it judges it.

With session state off, in live mode an **ask** decision raises a
[notice](#live-mode), not a question: "ask" below names the chief's decision, which the Chief nudges tab
records with its question. The chief is asked for no options: a notice has
nothing to choose. (Rows from before notices still show the options they hold.)

**The rule** (from a backtest on 590 real idle stalls): the **ask score** is
P(`needs_input`) + P(`asks_permission`). At or over
`MUSTER_CHIEF_NUDGE_ASK_THRESHOLD` (default `0.9`) the stall is an unasked
question or permission request, and the **default outcome is an ask**: the chief
is told so and writes the question (with session state on, the default is a
`notify`, and no question is written). A "leave" never survives it: it becomes
the ask (the notify). With session state off, a nudge survives it only when
the label is `asks_permission` **and** the quote guard passes. With session
state on, a nudge survives it for either label when the quote guard passes: a
[needs-you nudge](#needs-you-nudges). When the chief gave no question, it is the
**generic ask** — "*<focus title>* is waiting on you. Its last turn looks like
it asked you something." — in which muster puts no session text (its only
words about the session are the chief's own, below). When the chief's answer
gave a reason, the ask
adds it as one more line, "The chief's read: *<reason>*", cut to 300
characters: the chief's own reason from its answer, never the `classifier: …`
prefix muster puts around it in the row's `reason`. It is the chief's words,
held exactly as a chief's question on an ordinary ask is: one line, cut, and
not checked for session text (no ask path has such a check); the prompt tells
the chief, for the reason as for the question, never to paste the session's
text. With no reason,
the generic ask is unchanged. Under the threshold, the chief decides as before.

On an ask-stall, the chief is also told that a message ending with **several**
things waiting on you (a to-do list rather than one question) gets ONE question
summing them up, in its own words. Below the threshold it is not
told this, so a finished report listing next steps stays a leave.

**It never wedges the sweep.** The call is capped at 5 seconds; on any error
(no key, timeout, an HTTP error, an unreadable answer) the error is recorded and
the chief decides alone. The label is the argmax of the returned probabilities,
never the response's own `choice`.

**What leaves for OpenRouter and TypeSafe:** whenever your chief is on (shadow
mode included), the last assistant message of each of your sessions that has
already been idle for `MUSTER_CHIEF_NUDGE_IDLE` — redacted like everything sent
to the chief, and only its last 8 KB — plus the fixed rubrics, sent to TypeSafe's
Jev through OpenRouter on your OpenRouter key.
Nothing for any other session, and never per turn: **once per idle streak**,
when that streak first reaches the chief's check. A check the chief could not
take (busy, refused for capacity, not ready — nothing was sent to it) is retried
on later sweeps with the classification kept in memory from the first try, not
a new call; only a restart of muster, a retry more than an hour after the first,
or more than 512 such streaks held at once classifies that streak again. It runs in **shadow mode too** — measuring
the classifier on real stalls is what shadow mode is for.

Every decision records the label, all three probabilities, the ask score, the
model the response says it served (e.g. `typesafe/jev-1.14-…`) and the call's
latency, or its error, and the tab shows them.

### Needs-you nudges

A session over the line asked you something (`needs_input`) or asked leave
for its next step (`asks_permission`). It is **needs-you**, and with session
state on (`MUSTER_SESSION_STATE=1`, step 3a) it may be nudged for either label
(operator decision 11; with session state off nothing here changes: only an
`asks_permission` stop can be nudged, under the old list alone): the chief is
told that a nudge is accepted only when one of your messages to that session
already **answers the question it asked**, quoting the words that answer it
and naming in `covered_action` the answer they give. The quote guard is the
same code as for any nudge (your own recent words, verbatim); that the words
answer *this* question is the chief's judgement, which code does not check.

**It has its own live gate** (operator decision 2026-11-04, "Separate shadow
gate"): a needs-you nudge is sent only for an owner on **both**
`MUSTER_CHIEF_NUDGE_LIVE_OWNERS` and `MUSTER_CHIEF_NUDGE_NEEDS_YOU_LIVE_OWNERS`.
For everyone else it is shadow: recorded on the Chief nudges tab as
`would_send`, never pasted, not counted toward the hourly cap, and the stop is
pushed. That includes an `asks_permission` stop, which before step 3a was
nudged live on the old list alone (operator decision on #433). Every other nudge (to a session the classifier says asked nothing, or
whose classification failed) keeps `MUSTER_CHIEF_NUDGE_LIVE_OWNERS` alone. The
gate is read again at the paste (and at an agent's turn), from the
classification stored on the decision, so taking an owner off the needs-you
list stops a needs-you nudge already queued (*Not sent: chief nudges are off,
or in shadow mode for you, on this server*).

### Chief-chat quotes

With session state on (`MUSTER_SESSION_STATE=1`, session-state step 3b-1,
[design](design/session-state.md#covering-words)) a nudge's quote may also come
from **your Chief chat**: a line you typed to your chief, quoted for any of
your sessions (operator decision 10). Only lines muster recorded as **typed by
you** count (operator decision 13, as amended 2026-11-04, "Exclude the CLI"):
what you type into the chat page or its composer, signed in on the web or your
phone. The CLI does not count. muster writes that record
(`chat_messages.typed_by`, migration 0081) at the moment the line becomes a
turn, and nowhere else. These never count, fenced or not:

- an answer relayed to the chief;
- a session's question, and `muster chief ask` from the CLI (both come over a
  token, which any of your sessions can hold, so muster cannot tell you typed
  them);
- a plugin message;
- a task event (a forwarded task, a closed pull request);
- an agent's or muster's own turn (a nudge, a route's note or hand-back);
- every line written before this release: nothing is backfilled.

The chief is shown your typed Chief-chat lines from the last 6 hours (the
newest 20, redacted like your session messages), each with its id
(`chief:<id>`), and names where its quote is from (`quote_source`: `session` or
`chief_chat`). muster checks the quote, byte for byte, in **that** source only.
A Chief-chat quote that does not check out (not verbatim in the line it names,
or a source muster does not know) meets the same guard as any quote: it is
recorded as `suppressed:quote_unverified`, with its source (`chief_chat`, or
empty for a source muster does not know), the stop is pushed and nothing is
pasted. muster reads only the newest 20 typed lines
(all the chief is shown); a read that fails contributes nothing: no Chief-chat
quote passes. A quote from the session itself is checked exactly as before. Whether
a Chief-chat line covers *this* session's next step is the chief's judgement,
as for any quote: code only guarantees the words are yours, recent and
verbatim.

**It has its own live gate, shadow first.** The operator decided Chief-chat
quotes ship shadow first (2026-11-04, "Chief chat first"); the gate below is
the coordinator's default for it, open to change (the design's
[Defaults](design/session-state.md#defaults-open-to-change), D4): a nudge
quoting the Chief chat is sent only for an owner
also on `MUSTER_CHIEF_NUDGE_CHIEF_CHAT_QUOTES_LIVE_OWNERS`, on top of every
list it is otherwise under (`MUSTER_CHIEF_NUDGE_LIVE_OWNERS`, and the needs-you
list for a needs-you stop). For everyone else it is shadow: recorded as
`would_send` with its source on the Chief nudges tab, never pasted, and the
stop pushed.

**The whole decision is under that gate when your Chief chat was in view**
(operator decision 2026-11-04, "Shadow whole decision"). The chief sees your
typed Chief-chat lines on every check, and they can steer it even when it
quotes the session. So any nudge decided while at least one typed line was in
its prompt is shadow, whatever its quote's source, until you are on
`MUSTER_CHIEF_NUDGE_CHIEF_CHAT_QUOTES_LIVE_OWNERS` (on top of the other lists).
The decision records that the chat was in view (`chief_nudges.chief_chat_in_view`).
With no typed line in the last 6 hours, nothing changes: a session quote is
gated as before.

The gate is read again at the paste, at an agent's turn and at the sweep's
retry of one, from what is stored on the decision (`chief_nudges.quote_source`
and `chief_chat_in_view`), so taking an owner off the list stops such a nudge
already queued (*Not sent: chief nudges are off, or in shadow mode for you, on
this server*). The pasted nudge
says where the words were written: `[Chief nudge] Sam told the chief at 14:02:
"…". Continue.`

With session state off nothing here runs: the Chief chat is never read and the
chief's prompt is the one from before this step.

Quotes from your **other sessions' panes**, on the same repository, are the
second half of step 3b and are not built yet: muster cannot yet tell a line you
typed into a pane from one another agent sent there.

A **waiting** window (a permission prompt, a question muster cannot answer) is
never nudged: only an idle session is a candidate, and the send queue does not
paste into a waiting one. An opencode or Hermes window is never nudged either
([Limits](#limits)).

## What the chief is asked

For each candidate, the chief gets one bounded call on its own runtime. Every
call is a **fresh context**, under a session key derived from the decision it
is for (`agent:<chief>:webchat:nudge:<hash of owner, session and idle
streak>`), so no check sees another's, none grows with the ones before it, and
none appears in your conversation with the chief:

- a `gateway` chief keys its sessions by that session key, so each check is a
  session of its own there;
- a `claude-code` chief's ccd sends every turn that does not name an explicit
  session to its primary Claude Code CLI, which is your conversation with the
  chief. So the check also names its key as an explicit session
  (`X-Muster-CC-Session`, as a chat session started on the Claude Code runtime
  does), and ccd runs it in a CLI of its own, a *lane*, started with an empty
  conversation on the pod's default model. The check is marked throwaway
  (`X-Muster-CC-Ephemeral: 1`): ccd stops that CLI as soon as the check's
  turn is over, whether it succeeded or failed, so no check CLI stays in the
  pod's memory or holds a lane. It is also marked **tool-less**
  (`X-Muster-CC-Toolless: 1`, always sent with the throwaway mark): ccd starts
  that CLI with no tools at all (see **A check cannot call tools** below).

The call holds the chief's turn lock: a busy chief is skipped until the next
sweep — nothing is recorded, and the same streak is asked about next sweep.

A claude-code chief whose ccd has no free lane for the check answers `503
capacity`, before anything is sent. A check never takes a lane from one of your
own sessions: when every lane (`CCD_MAX_SESSIONS`, default 3) is held by your
sessions with the chief, even idle ones, ccd refuses the check rather than stop
one of them. **Those lanes do not free up on their own** — ccd never stops an
idle session of yours — so the refusal lasts until you close one of those
sessions. A `503 not_ready` is the same kind of refusal: ccd runs no lanes at
all (`CCD_MAX_SESSIONS=0`), or the check's CLI did not reach its prompt within
ccd's 60 seconds. Either refusal is:

- retried on each sweep, with the stall classifier's result kept from the
  first try (above), and logged once per idle streak, on its first refusal;
- after **30 minutes** of refusals, recorded on that session as
  `suppressed:lanes_full` (capacity) or `suppressed:chief_not_ready`, with a
  detail saying why and what to change — so it shows on the Chief nudges tab,
  and that idle streak is not asked about again (the next one is judged
  afresh, and waits its own 30 minutes if the lanes are still full).

While one check is refused, your other stalled sessions wait behind it, and
their 30 minutes start with its: past the bound they are recorded the same way,
on the same sweep or the next. A check the chief does answer restarts every
one of your refusal clocks. The clocks are in memory: a restart of muster
restarts them.

The call's budget is four minutes, for every kind of chief:
the three a check's turn has always had, plus one for ccd to start the lane's
CLI. A call that fails otherwise is recorded on that session as
`suppressed:chief_error` (that idle streak is not retried; the next one is
judged afresh) and the sweep goes on to the next session.

The call carries, fenced as untrusted data:

- the session's focus title;
- the session's **last message** (up to 4,000 characters);
- **your own messages to that session**, up to the 20 newest, each up to 2,000
  characters, each with the id a nudge must quote it by;
- with session state on, **your typed Chief-chat lines** from the last 6 hours,
  the same way ([Chief-chat quotes](#chief-chat-quotes)).

"Your own messages" are the session's user-role messages, leaving out:

- anything muster itself queued into your windows: every routed message and
  every nudge or relay of this feature, matched against the `send_queue` row
  that carried it (the last 7 days), and this feature's own nudges and relays
  for that session. muster's pastes land in Claude Code's transcript as
  ordinary prompts, so this is what keeps them from reading as yours;
- anything that carries a muster fence (a routed message, a delivered answer, a
  forwarded task), or opens the way muster's own pastes open (`Routed to you
  by …`, `[Chief nudge]`, from the same constants the senders use) — a second
  line for a paste whose row is gone;
- for a dispatched agent, its first message (the task's text, which can come
  from a plugin) when muster has read the whole conversation;
- anything **older than 6 hours**, or with **no time** at all.

A window's messages are read from the host's transcript backfill (the newest
300 events within the last 4 MiB), which arrives oldest first
(`cctranscript.Backfill` reads the window backwards and reverses what it keeps).
**A read that cannot vouch for your last six hours authorises no nudge** — the
nudge becomes an ask, with the reason `partial_read`: one that ended before the
host's prompt index arrived (the quiet timer or the deadline), one whose prompts
are out of order, one missing a prompt the index lists inside its span (or the
newest one), and one a cap cut short before reaching six hours back. The same
holds for an agent whose newest 200 messages do not reach six hours back.
A prompt time more than 5 minutes in muster's future is not believed (the
message is untimed). Each prompt now carries its own time on the
wire (`promptAt`, from the transcript record), so a window is held to the same 6
hours as an agent, and its nudge says when you said it. A message is named by
its place in the transcript. An older `muster-mirror` sends no prompt times:
then none of your messages can authorise a nudge, the chief's "nudge" is turned
into an ask, and the decision records why ("… arrived with no time (an older
muster-mirror)").

**A Stop hook's block reason is not your message.** Claude Code records the
reason of a Stop hook that blocks as a user record marked `isMeta`
(`Stop hook feedback: …`); muster's transcript reader drops every `isMeta`
record, so it never reaches the chief as yours (pinned by a test in
`internal/cctranscript`).

**A message over the machine door is yours (operator decision).** For a
dispatched agent, a message sent with `POST /api/agents/{name}/messages` (a
token-authenticated call) counts as your own and can authorise a nudge.
muster records no other source for an agent's chat messages, and that is the
decision, not an oversight.

**What leaves for the model provider.** Each call sends exactly the fenced block
above — the focus title, the session's last message, your own recent
messages to it and, with session state on, your recent typed Chief-chat lines —
plus muster's instructions, with anything shaped like a secret
replaced by `«redacted»` (the same heuristic redaction as `sessions_overview`; a
secret in a shape it does not know passes), to the chief's model provider:
OpenRouter for a `gateway` chief, Anthropic for a `claude-code` chief. No earlier
check is in the context (each call is fresh), and nothing is sent for a session
that is not a candidate. The stall classifier is a separate call, to OpenRouter (above).

The chief answers with one decision:

| decision | meaning |
|---|---|
| `nudge` | The session stopped short of something one of your messages already authorises. It names that message's id, an exact quote from it, and the pending action the quote covers. |
| `ask` | Session state off: the session needs a decision none of your messages covers. It gives the question (live, the notice's message). |
| `notify` | Session state on: you should look at the session. No question, no summary, no options; the stop's episode pushes. |
| `leave` | It finished, is reporting, or is waiting on something that is not you. |

### A check cannot call tools

The check's prompt quotes text from your other sessions' transcripts. It is
redacted and fenced as data, not instructions — but a fence is only prose, and
a transcript written to steer the chief could try to get it to call a tool
during the check. Shadow mode does not help there: it gates only what the sweep
itself sends, not what the chief does.

So a `claude-code` chief's check runs in a CLI that has **no tools**. ccd starts
it with `--tools= --strict-mcp-config`: an empty list of built-in tools (no
Bash, Edit, WebFetch, Task, …) and no MCP server at all (muster's MCP server,
with `route_to_session`, `agent_dispatch`, `answer_session_question` and the
rest, is not even started). The check's only possible effect is its text reply,
which muster parses and validates like any check answer. Your own sessions with
the chief, and its primary CLI, keep their tools.

What this covers, and what it does not:

- It is enforced by the Claude Code CLI, measured on the pinned version
  (2.1.296): with both flags the CLI declares no tools and no MCP server, and
  starts none; with either flag alone, the other half is still there
  ([configuration](configuration.md#tool-less-check-lanes)).
- A `gateway` chief has no lanes and is **not** tool-less: its check runs in a
  gateway session of its own with whatever tools that runtime gives it.
- A chief pod running a ccd older than this muster ignores the mark and the
  check's lane keeps its tools; muster does not detect or prevent that.
  [Rolling out](#rolling-out) is what closes it.

## The guards

These are code, not instructions to the chief:

- **The quote is your own recent words, checked byte for byte.** A nudge is
  accepted only when its quote is a phrase (at least 3 words and 12
  characters) that appears exactly — starting and ending on word boundaries
  (both sides NFC-normalised; letters, combining marks, digits and apostrophes
  are word characters, so "t go ahead" is no phrase of "don't go ahead"), as the
  chief was shown it — inside the one message it names, and that
  message is one of your own messages above. Text the session wrote ("the
  owner approved the deploy") is never in that set, so a session cannot talk
  itself into a nudge. A nudge naming no covered action is refused too. A
  refused nudge is recorded as `suppressed:quote_unverified`.

  **What this does not check:** whether your words actually *cover* the
  pending action ("merge when green" covers a merge, not a deploy) is the
  chief's judgement, recorded as the decision's covered action. The code
  guarantees only that the quote is yours, recent and verbatim.
- **Two nudges per message of yours.** After 2 nudges quoting from the same
  latest message of yours, the next nudge becomes an ask (a notify with session
  state on) ("… stopped again
  after 2 nudges since your last message to it. Should it continue?").
- **The hourly cap, live only.** Past `MUSTER_CHIEF_NUDGE_HOURLY_CAP` live
  nudges in the last hour, a live nudge is recorded as `suppressed:hourly_cap`.
  Shadow mode is not capped and does not count: it records what it would send
  (operator decision).
- **Still stalled.** Right before anything is sent or asked, muster checks the
  session is still in the same idle streak; if it was read and is not (not
  idle, a newer streak, another conversation in the window, a window its
  connected host no longer lists), the decision is recorded as
  `suppressed:moved_on`. With session state on, the session's open episode must
  also still be the one the decision was taken in: a nudge queued in a stop
  that has since ended is dropped at its paste (`suppressed:superseded`) and
  the new stop pushes. So is a nudge still queued when its stop was pushed
  (its 10-minute hold ran out): you are never both pushed and nudged for one
  stop. And a decision about an earlier stop never quiets a later one on the
  same streak (a window gone and back): that stop pushes.
- **Unknown is not moved on.** When the session's state cannot be read — its
  host's connection dropped, the host has sent no window list yet (its first
  read failed), or it does not report status — nothing is
  decided on it: a nudge or notice about to be raised is held (nothing
  recorded; the streak is judged again on a later pass), an open notice stays
  open, and a queued nudge goes back to the queue to be tried again. The
  existing bounds still end a hold: a notice's own expiry and the send queue's
  7-day limit on a queued message. One limit: a host whose window
  list arrives but whose process read failed (on macOS, `ps` failing) reports
  no Claude Code in its windows, which muster cannot tell from Claude Code
  having exited, so it counts as moved on.
- **Never into a prompt.** A nudge to a window goes through the [send
  queue](sending-and-step-up.md#queue-send-when-claude-code-is-idle), which
  waits for idle and refuses while Claude Code is waiting on a prompt or has
  text in its input box.

## Live mode

**A nudge** is a labelled message quoting you, never a message in your voice:

```
[Chief nudge] Sam said at 14:02: "keep going until merged". Continue.
```

The time is when you wrote the quoted message (on an agent's row, or the
transcript record's own time for a window). To a window it is queued (it shows in the
window's queue, and cannot be edited or retried); to an agent it is sent as a
turn when no other turn of the agent's is running. When muster pastes a queued
nudge it checks again that the feature is on, you are live, the window still
holds the same conversation, and the session is still in the idle streak the
chief judged; if it cannot read the session's state, the nudge stays queued and
is tried again.

**An ask decision raises a notice** on your Asks list
([attention.md](attention.md#notices)): a pointer at the session, never a
question. It carries:

- where the session is: for a mirrored window its label, exact tmux target
  and host, as every attention text names a window
  ([attention.md](attention.md#how-a-window-is-named):
  `mango:4 · tmux build:4 @ laptop`), found by the
  session's key over your windows when the notice is drawn, so a window
  renumbered since shows its new index; for an agent, `agent <name>`;
- the session's focus title;
- the chief's short message: "Waiting on you: *<the chief's question>*", or,
  for the generic ask, "Its last turn looks like it asked you something." and
  "The chief's read: *<reason>*" when the chief gave one.

It has **no options, no answer box and no relay**. An answer posted to it is
refused, and the database refuses an answer on a notice row as well. It does
not count toward the 50-open-ask cap, so a cap full of notices can no longer
suppress a nudge (`suppressed:ask_cap`) or hold back a real question. It is
pushed like any item, with no buttons on a phone. Its tap, and its **View in
session** link, open the session: the window now running that conversation, or
the agent's page at the conversation that stalled. Both are found through the
decision's record, since the notice itself is muster's own and names no
session.

**It is withdrawn when the session moves on**, exactly as a nudge ask was, and
only on a state muster actually read: while the session's host is disconnected
(or its state otherwise cannot be read) the notice stays. **Dismiss** withdraws
it at once, and it otherwise expires after 24 hours. Every one of those ends the
decision `ask_closed`, with nothing relayed.

**Nothing is relayed.** Relaying an owner's answer into a session is gone.
Migration 0071 withdrew every nudge ask raised before notices that was still
open (the sweep then closes its decision, `ask_closed`) and failed every relay
still queued, with a bare update that sends no push: the sweep's next pass
closes each such decision and withdraws its notification from your devices
then. A nudge ask answered anyway (one an older replica raised) is closed with
nothing relayed, and a relay an older replica claims during a rolling deploy is
failed ([Rolling out](#rolling-out)). A live window nudge recorded as queued whose
queued message was never written (muster stopped between the two) is failed
after 10 minutes.

### Jumping to a notice

A host can jump to the oldest notice whose session is on it, with the
`muster` CLI and the host's ordinary credential (a personal
token, or a bar-summary token: [cli.md](cli.md#jumping-to-a-chief-nudge)):

```sh
muster nudge next --host laptop     # the oldest one with a tmux target (JSON)
tmux switch-client -t "$(muster nudge next --host laptop | jq -r .target)"
```

`--host` defaults to `MUSTER_MIRROR_HOST`. It exits **11** with nothing on
stdout when no notice with a target is waiting on that host, and **1** when the
queue could only be partly read. It reads the
[attention summary](attention.md#the-summary-api), whose notice items carry
their `host` and `target`.

## The Chief nudges tab

`/attention?tab=nudges` lists your 50 newest decisions, newest first. Each card
shows the session (focus title and host, or agent), the decision, the message
that was or would have been sent, the quote and its message id — and where the
quote is from, your Chief chat or your messages to the session — the covered
action, the chief's reason, the outcome (including which guard suppressed it)
and a **Shadow** or **Live** badge.

## Records

Every decision is a row of `chief_nudges` (migration `0064`, see
[schema.md](schema.md)), and each step writes an audit row on
`chief_nudge:<id>`:

| audit action | when |
|---|---|
| `chief_nudge.would_send` | shadow: a nudge that passed every guard |
| `chief_nudge.would_ask` | shadow: an ask that would have been raised |
| `chief_nudge.notified` | session state on: a notify (no notice; the stop's episode pushes), in either mode |
| `chief_nudge.left` | the chief said leave it |
| `chief_nudge.suppressed` | a guard refused the decision (the row's outcome names which) |
| `chief_nudge.sent` | live: the nudge reached the session |
| `chief_nudge.noticed` | live: the notice was raised |
| `chief_nudge.asked` | live, before notices: an attention ask was raised (older rows) |
| `chief_nudge.relayed` | live, before notices: an answer reached the session (older rows) |
| `chief_nudge.failed` | live: the nudge could not be sent (the row says why) |
| `chief_nudge.ask_closed` | live: the notice (or an older ask) ended: withdrawn, dismissed, expired or answered, with nothing relayed |

The audit rows carry the decision's fields and where it went, never the
session's text. The row itself holds the message, the quote and the question
(and, on a row from before notices, its options and any answer it relayed).

The row's `reason` is the chief's own one-line reason on every decision whose
answer muster could read (left, asked, nudged, suppressed by a guard), prefixed
by muster's when muster changed the decision (`classifier: …`, `partial_read:
…`, `escalated: …`). On an answer muster could not read (`suppressed:invalid`)
it is muster's parse error, not the chief's words. A decision recorded without
an answer from the chief (its call failed, or it refused) has its why in
`detail` instead.

## Limits

- On a `gateway` chief each stall check opens a fresh gateway session (its key
  is per decision). The agent gateway has no call to delete a session, and
  muster keeps no row for these, so they pile up in the gateway's own storage:
  a follow-up is to prune them once the gateway offers a way. Nothing in this
  feature prunes them.
- On a `claude-code` chief each stall check starts a Claude Code CLI in the
  chief's pod (about 250–550 MiB while it runs) and stops it when the check's
  turn ends. With all of ccd's lanes held by your own sessions, checks are
  refused — they are never run by stopping your sessions — and since ccd never
  stops an idle session of yours, they stay refused until you close one: each
  stalled streak is retried for 30 minutes and then recorded
  `suppressed:lanes_full` (above), not nudged or asked about.
  Each check's transcript stays on the chief's volume (a few KiB each); nothing
  prunes those.
- A chief pod whose ccd is older than this muster ignores the throwaway mark.
  It keeps each check's CLI running after the check and, at its lane cap,
  stops the least recently used idle lane to start a new one, which can be one
  of your own sessions (resumed from its transcript on its next turn, with a
  cold start). It also ignores the tool-less mark, so each check's CLI has the
  chief's full tool set. muster does not detect either. See
  [Rolling out](#rolling-out).
- A `gateway` chief's checks are not tool-less (above).
- One replica only, like the send queue and the mirror hub. The per-streak
  unique key keeps two passes from deciding the same streak twice.
- A window on an older `muster-mirror` (no prompt times) is never nudged, only
  pointed at with a notice or left.
- An **opencode** pane is never nudged: muster cannot type into it. Its
  prompts also carry no time (the host's opencode renderer sets none), so none
  of your messages to it could authorise a nudge anyway. A decision to nudge
  it becomes a notice (a notify with session state on), with the reason
  `view_only`, before any other rule.
- An opencode session that has not changed status since its host's event
  stream connected has no status time, so it is not judged until its next turn
  ends. The smallest fix is for the host to seed that time from the session's
  own last-update time when the stream connects.
- For dispatched agents, which user-role messages are yours is decided by what
  muster can tell apart (fenced injections, its own pastes, the kickoff); a
  message over the machine door counts as yours (above).

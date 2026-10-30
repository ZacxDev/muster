# Chief stall nudges

A session that stops short — it finished a step and asked "shall I merge?" in
prose, or it is waiting on a question you never saw — sits idle until you look.
**Chief stall nudges** let your [chief](chief.md) find such sessions and either
nudge them on, when one of your own messages to that session already covers the
next step, or put the question to you on the [Attention](attention.md) page and
pass your answer back.

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
| `MUSTER_CHIEF_NUDGE_IDLE` | How long a session must have been idle to count as stalled. Default `10m`, from `2m` to `24h`. |
| `MUSTER_CHIEF_NUDGE_HOURLY_CAP` | The most live nudges per hour, across every owner. Default `6`. Shadow mode is not capped. |

All four are read at boot; the boot log says which mode the sweep is in.

**The kill switch** is unsetting `MUSTER_CHIEF_NUDGES` (or removing an owner
from the live list) and restarting. The switch reaches messages already queued:
each one is checked again when it would be pasted, and a nudge whose owner is no
longer live, or whose feature is off, fails with *Not sent: chief nudges are
off, or in shadow mode for you, on this server* and is never pasted.

## What counts as stalled

Every 2 minutes the sweep looks at each owner who has a chief (and has it
turned on). It considers:

- every mirrored **Claude Code** window on a host that reports Claude Code's
  status (one entry per Claude session, whatever window shows it);
- every **dispatched agent** of the owner's.

It never considers an [officer](officers.md) (officers escalate through HQ),
the chief itself, or another user's agents. An agent's **conversations** are
judged one by one (those active in the last 24 hours): the one that stalled is
the one decided about, nudged and relayed to.

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

**The rule** (from a backtest on 590 real idle stalls): the **ask score** is
P(`needs_input`) + P(`asks_permission`). At or over
`MUSTER_CHIEF_NUDGE_ASK_THRESHOLD` (default `0.9`) the stall is an unasked
question or permission request, and the **default outcome is an ask**: the chief
is told so and writes the question and 2 to 4 options. A nudge survives it only
when the label is `asks_permission` **and** the quote guard passes; a "leave", or
any other nudge, becomes the ask. When the chief gave no question, it is the
**generic ask** — "*<focus title>* is waiting on you. Its last turn looks like
it asked you something." with the options **Open session** and **Dismiss** — which
carries no word of the session's, and whose options relay nothing (only an
answer you type is relayed). Under the threshold, the chief decides as before.

**It never wedges the sweep.** The call is capped at 5 seconds; on any error
(no key, timeout, an HTTP error, an unreadable answer) the error is recorded and
the chief decides alone. The label is the argmax of the returned probabilities,
never the response's own `choice`.

**What leaves for OpenRouter and TypeSafe:** whenever your chief is on (shadow
mode included), the last assistant message of each of your sessions that has
already been idle for `MUSTER_CHIEF_NUDGE_IDLE` — redacted like everything sent
to the chief, and only its last 8 KB — plus the fixed rubrics, sent to TypeSafe's
Jev through OpenRouter on your OpenRouter key.
Nothing for any other session, and never per turn: only when the sweep has a
candidate. It runs in **shadow mode too** — measuring the classifier on real
stalls is what shadow mode is for.

Every decision records the label, all three probabilities, the ask score, the
model the response says it served (e.g. `typesafe/jev-1.14-…`) and the call's
latency, or its error, and the tab shows them.

## What the chief is asked

For each candidate, the chief gets one bounded call on its own runtime. Every
call is a **fresh context**: its gateway session key is derived from the
decision it is for (`agent:<chief>:webchat:nudge:<hash of owner, session and
idle streak>`), so no check sees another's, none grows with the ones before it,
and none appears in your conversation with the chief. The call holds the
chief's turn lock: a busy chief is skipped until the next sweep. A call that
fails is recorded on that session as `suppressed:chief_error` (that idle streak
is not retried; the next one is judged afresh) and the sweep goes on to the
next session.

The call carries, fenced as untrusted data:

- the session's focus title;
- the session's **last message** (up to 4,000 characters);
- **your own messages to that session**, up to the 20 newest, each up to 2,000
  characters, each with the id a nudge must quote it by.

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
above — the focus title, the session's last message and your own recent
messages to it — plus muster's instructions, with anything shaped like a secret
replaced by `«redacted»` (the same heuristic redaction as `sessions_overview`; a
secret in a shape it does not know passes), to the chief's model provider:
OpenRouter for a `gateway` chief, Anthropic for a `claude-code` chief. No earlier
check is in the context (each call is fresh), and nothing is sent for a session
that is not a candidate. The stall classifier is a separate call, to OpenRouter (above).

The chief answers with one decision:

| decision | meaning |
|---|---|
| `nudge` | The session stopped short of something one of your messages already authorises. It names that message's id, an exact quote from it, and the pending action the quote covers. |
| `ask` | The session needs a decision none of your messages covers. It gives the question and 2 to 4 options. |
| `leave` | It finished, is reporting, or is waiting on something that is not you. |

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
  latest message of yours, the next nudge becomes an ask ("… stopped again
  after 2 nudges since your last message to it. Should it continue?").
- **The hourly cap, live only.** Past `MUSTER_CHIEF_NUDGE_HOURLY_CAP` live
  nudges in the last hour, a live nudge is recorded as `suppressed:hourly_cap`.
  Shadow mode is not capped and does not count: it records what it would send
  (operator decision).
- **Still stalled.** Right before anything is sent or asked, muster checks the
  session is still in the same idle streak; if not, the decision is recorded as
  `suppressed:moved_on`.
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
chief judged.

**An ask** is an attention ask on your queue, asked by *your chief*, naming the
session's focus title and where it runs, the chief's question and its options.
It is pushed like any ask. When you answer it **on the Attention view**, your
answer is claimed for the relay at once and sent into that session **word for
word** by the sweep, which the answer wakes (queued for a window, a turn for an
agent). Nothing else is added to it. An answer given through a mirrored
session's `attention_answer` is recorded and **not** relayed, since a session
fed outside text could write it. If the session moves on before you answer, the
ask is withdrawn.

A window's relay is tied to the stall it answers, as an agent's is: if you
typed into the window after answering (a newer turn), the relay is refused at
its paste and recorded `suppressed:superseded`, never pasted into the newer
turn. An agent's relay goes into the conversation that was asked about (stored on the
decision), not the agent's newest one, and only while that conversation is
still where it stopped. The kill switch is read again when a relay or an
agent's nudge is sent, as the send queue's gate reads it for a window. A relay
not delivered within 2 hours (an agent that stayed busy) is dropped and
recorded failed. A live window nudge recorded as queued whose queued message
was never written (muster stopped between the two) is failed after 10 minutes.

## The Chief nudges tab

`/attention?tab=nudges` lists your 50 newest decisions, newest first. Each card
shows the session (focus title and host, or agent), the decision, the message
that was or would have been sent, the quote and its message id, the covered
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
| `chief_nudge.left` | the chief said leave it |
| `chief_nudge.suppressed` | a guard refused the decision (the row's outcome names which) |
| `chief_nudge.sent` | live: the nudge reached the session |
| `chief_nudge.asked` | live: the attention ask was raised |
| `chief_nudge.relayed` | live: your answer reached the session |
| `chief_nudge.failed` | live: the nudge or relay could not be sent (the row says why) |
| `chief_nudge.ask_closed` | live: the ask ended with nothing to relay |

The audit rows carry the decision's fields and where it went, never the
session's text. The row itself holds the message, the quote, the question and
your relayed answer.

## Limits

- Each stall check opens a fresh gateway session on the chief (its key is per
  decision). The agent gateway has no call to delete a session, and muster
  keeps no row for these, so they are left to the gateway's own storage: a
  follow-up is to prune them once the gateway offers a way.
- One replica only, like the send queue and the mirror hub. The per-streak
  unique key keeps two passes from deciding the same streak twice.
- A window on an older `muster-mirror` (no prompt times) is never nudged, only
  asked about or left.
- For dispatched agents, which user-role messages are yours is decided by what
  muster can tell apart (fenced injections, its own pastes, the kickoff); a
  message over the machine door counts as yours (above).

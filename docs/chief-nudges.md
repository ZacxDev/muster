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
| `MUSTER_CHIEF_NUDGE_HOURLY_CAP` | The most nudges per hour, across every owner, shadow and live together. Default `6`. |

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
the chief itself, or another user's agents.

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

## What the chief is asked

For each candidate, the chief gets one bounded call on its own runtime, under a
gateway session of its own (`agent:<chief>:webchat:nudge`), so the check never
appears in your conversation with it. The call holds the chief's turn lock: a
busy chief is skipped until the next sweep.

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
  from a plugin) when muster has read the whole conversation, and anything
  older than 6 hours.

A mirrored transcript has no times on the wire, so for a window the read is the
host's backfill (the newest 300 events within the last 4 MiB of its transcript)
instead of 6 hours, and its messages carry no time. A message is named by its
place in the transcript; an older `muster-mirror` that does not send it has no
messages a nudge can quote, so its sessions are only ever asked about or left.

**What leaves for the model provider.** Each call sends the text above, with
anything shaped like a secret replaced by `«redacted»` (the same heuristic
redaction as `sessions_overview`; a secret in a shape it does not know passes),
to the chief's model provider: OpenRouter for a `gateway` chief, Anthropic for a
`claude-code` chief. Nothing is sent for a session that is not a candidate.

The chief answers with one decision:

| decision | meaning |
|---|---|
| `nudge` | The session stopped short of something one of your messages already authorises. It names that message's id, an exact quote from it, and the pending action the quote covers. |
| `ask` | The session needs a decision none of your messages covers. It gives the question and 2 to 4 options. |
| `leave` | It finished, is reporting, or is waiting on something that is not you. |

## The guards

These are code, not instructions to the chief:

- **The quote is checked byte for byte.** A nudge is accepted only when its
  quote (at least 4 characters) appears exactly inside the message it names,
  and that message is one of your own messages above. Text the session wrote —
  "the owner approved the deploy" — is never in that set, so a session cannot
  talk itself into a nudge. A nudge with no covered action is refused too. A
  refused nudge is recorded as `suppressed:quote_unverified`.
- **Two nudges per message of yours.** After 2 nudges quoting from the same
  latest message of yours, the next nudge becomes an ask ("… stopped again
  after 2 nudges since your last message to it. Should it continue?").
- **The hourly cap.** Past `MUSTER_CHIEF_NUDGE_HOURLY_CAP` nudges in the last
  hour, a nudge is recorded as `suppressed:hourly_cap`.
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

For a window the time is left out (`Sam said earlier in this session: …`),
because its transcript carries none. To a window it is queued (it shows in the
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

## The Chief nudges tab

`/attention?tab=nudges` lists your 50 newest decisions, newest first. Each card
shows the session (focus title and host, or agent), the decision, the message
that was or would have been sent, the quote and its message id, the covered
action, the chief's reason, the outcome (including which guard suppressed it)
and a **Shadow** or **Live** badge.

At the top, **Repeat stallers** lists sessions with 3 or more decisions in the
last 24 hours.

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

- One replica only, like the send queue and the mirror hub. The per-streak
  unique key keeps two passes from deciding the same streak twice.
- A mirrored session's messages have no times, so its nudge cannot say when you
  said it, and the 6-hour window does not apply to it.
- For dispatched agents, which user-role messages are yours is decided by what
  muster can tell apart (fenced injections, its own pastes, the kickoff). A
  message sent over the machine door is counted as yours.

# The ask log

Your agents ask you things all day: whether to continue, whether to merge,
which of two options to take. The ask log records every one of those
questions, the answer you gave, who gave it and how long it took, so "what
could my agents have answered without me" is a query rather than an
impression.

It is **phase 1** of ask auto-approval
([design](design/ask-autoapproval.md)): telemetry only. Nothing on this page
answers anything for you, proposes an answer or evaluates a rule. Review,
shadow rules and auto-answers are later phases, each off by default.

Turn it on with `MUSTER_ASK_LOG=1` (see [configuration.md](configuration.md)).
It needs a database.

## What is recorded

One row in `asks` per question, whatever kind it was:

| kind | when it is recorded | how it ends |
|---|---|---|
| `plain_text` | A mirrored Claude Code window that names its session goes from busy to idle, or a dispatched agent's turn ends, and the turn's last assistant message passes the pre-filter (below). | The first user message after it is the answer, with what muster can prove about who wrote it (provenance, below). A newer assistant message with no user message in between: `moved_on`. The window gone from its (online) host first, or now showing another Claude session: `gone`. |
| `ask_user_question` | The `AskUserQuestion` hook's registration is accepted. The questions (in the question column), and each one's header, option labels and whether it is multi-select (in options; never the question's text). | The CLI records the result: `answered` with the chosen labels (typed from muster or picked in the terminal), `declined` (dismissed), `superseded` (a newer question replaced it), `expired`. |
| `attention_ask` | An agent or one of your mirrored sessions raises an `attention_ask` (a question, never a notice; muster's own asks are not recorded). | `answered` (by you, or by the chief for a question put to it), `answered_in_session`, `cancelled`, `expired`. |
| `opencode_question`, `opencode_permission` | A mirrored opencode window starts waiting on a named form or permission request. | `answered` when you answer it from the Asks page; `moved_on` when it leaves the window unanswered from muster. |

A plain-text question is captured at **every** turn end, not from session
episodes: a question you answer quickly — before the session counts as idle
for the stall nudges — opens no episode at all, and those quick answers are
exactly what this log is for. When your reply is already in the transcript by
the time muster reads the turn, the question is still recorded, and answered
from the same read.

### The pre-filter

A turn's last message is a question when its **last paragraph** holds a `?`,
or begins with one of: *shall I*, *should I*, *want me to*, *do you want*,
*ready to*, *let me know*. It is checked in code, with no model. A turn end
that fails it writes no row and is counted
(`muster_ask_plain_text_prefilter_total{result="dropped"}`).

### Provenance of a plain-text reply

| provenance | means |
|---|---|
| `muster_send` | the body of a message muster pasted from **your** send queue (or, for a dispatched agent, a line you typed into muster's chat) |
| `nudge` | a chief nudge or relayed answer muster queued |
| `route` | a routed message |
| `pane` | anything muster did not put there and cannot attribute: you typing in the terminal — or anything else that can type into it |

A reply is matched to a queued message by the message's own row, never by its
wording.

Who answered follows the provenance: a `nudge` reply is `answered_by = chief`
and a `route` reply `answered_by = other`, so a reply muster queued never
counts as your own approval. `muster_send` and `pane` replies are yours.

### After a restart

A plain-text question's reply link, and an opencode ask's leaving its window,
live in the memory of the process that opened them. When muster starts, it
closes the rows of those kinds a previous process left open as `unknown`, so
they do not read as unanswered for ever; an answer seen later still replaces
`unknown`. `AskUserQuestion` and `attention_ask` rows are resolved by their own
ids from any process, and are left open.

### Classification

With `MUSTER_ASK_LOG_CLASSIFY=1` (and the stall classifier configured, see
[chief-nudges.md](chief-nudges.md)), a kept plain-text question also carries
the classifier's scores. The answer the stall sweep already has for that
streak is reused; otherwise it is one call on your OpenRouter key. It is off
by default so the volume is measured before the classifier is spent on it.

## Privacy

- **Redacted on the way in.** Every text column — the question, its normal
  form, the options and the answer — passes through muster's one secret
  redactor before it is written, inside the store, whoever calls it.
- **Yours only.** The table is under forced row-level security: no other user,
  and no admin view, can read your asks.
- **Text expires.** A daily sweep clears question and answer text — and the
  option labels — after `MUSTER_ASK_LOG_TEXT_TTL` (30 days by default). The rest of the row — the
  kind, where, the scores, who answered, how long it took, how it ended — is
  kept 180 days, then deleted.
- **Metrics carry no text.** No metric label holds a question, a host, a
  repository or a session.

## Capture never gets in the way

Every write happens off the path that raised or answered the ask, in order. If
the database is unreachable, or a transcript cannot be read, the ask goes to
you exactly as it would without the log; the failure is logged and counted in
`muster_ask_capture_errors_total{kind, stage}` (`read`, `classify`, `write`),
never returned to the agent or to you. A full capture queue counts every
dropped capture but logs at most one line a minute, and the count still
unlogged when the queue drains.

## Metrics

| metric | type | labels |
|---|---|---|
| `muster_asks_total` | counter | `kind` |
| `muster_ask_plain_text_prefilter_total` | counter | `result` (`kept`, `dropped`) |
| `muster_ask_answer_seconds` | histogram | `kind`, `answered_by` (`owner`, `chief`, `other`, `none` for an ask that ended unanswered; a row a restart closed `unknown` is not observed; `auto` is reserved for the later phases) |
| `muster_ask_capture_errors_total` | counter | `kind`, `stage` (`read`, `classify`, `write`) |

## Rollout

Turn it on with classification off, read `muster_ask_plain_text_prefilter_total`
and `muster_asks_total` for a week, then decide whether to turn
classification on.

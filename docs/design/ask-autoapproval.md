# Design: ask telemetry, review, and progressive auto-approval

**Status: proposal. Nothing on this page is built.** The operator settled the
shape (what is recorded, who decides, the only two answers muster may ever
give, and the reversal of an earlier non-goal) before this was written; it is
recorded under [Operator decisions](#operator-decisions). The operator then
answered the draft's three open questions and adopted its five defaults
(decisions 6 to 9). No questions remain open.

The ask, verbatim: *"I still find that agents are asking me things that they
should be able to answer themselves, such as really basic things, just asking
to merge or do a basic continue or proceed. And it's a waste of my time and
slows down agents. Let's make sure that we're collecting telemetry on what
agents are asking me and then having a way to review the asks and set up
progressively more permissive auto-approval. I'm not sure if we can do this
deterministically or if we should include a decision model like JEV."*

Where this page says how muster works today, the code is the authority (see
[decisions.md](../decisions.md)). It cites `path:line` at commit `187e9285`
(`main`).

## Summary

| | Proposal |
|---|---|
| Telemetry | One table, `asks`, for every question an agent puts to its owner: plain-text questions at turn end (the question and the owner's next reply), `AskUserQuestion` cards, `attention_ask`, and opencode questions and permission requests. Redacted on the way in, owner-only under row-level security, question and reply text purged after 30 days |
| Who decides | **Rules decide, JEV proposes.** Only an owner-approved, deterministic rule can answer. JEV (the existing stall classifier) and an LLM only *propose*: a class, a suggested rule, an answer. A hard-coded [never-auto list](#the-never-auto-list) is checked first and always wins |
| What an answer can be | Exactly two fixed texts, built in code: **continue** (a yes to a "shall I continue?" question) and **merge when green** (a yes to a merge question, only when muster has itself read the PR's CI as green and its audit ladder as ended with no 🔴). Never an `AskUserQuestion` option, never a permission prompt, never composed text |
| Progression | Each rule is `off` → `shadow` (logs what it would have answered) → `live`. muster offers promotion once the shadow record agrees with what the owner actually answered; the owner promotes, with a fresh sign-in. A disagreement demotes it to `shadow` at once |
| Delivery | The existing paths, unchanged in their guards: the send queue (as chief nudges use it) for a plain-text question, the attention answer path for an `attention_ask`. Never typed into a waiting prompt |
| Review | A **Log** tab on the Asks page: every ask, what was answered and by whom, what shadow would have done and why not, and **Make a rule from this** |
| Phases | 1 telemetry · 2 review UI and shadow · 3 live `continue` · 4 live `merge when green` |

## Goals

- **Know what agents ask.** Every ask kind the operator named is recorded in
  one place, with the owner's actual answer and how long it took, so "what
  could have been answered without me" is a query, not an impression.
- **Spare the owner the trivial yes.** A question the owner has answered the
  same way many times, and has said muster may answer, is answered within
  seconds instead of waiting for the owner to notice.
- **Deterministic authority.** The thing that answers is a rule the owner
  wrote or accepted, evaluated by code over recorded inputs. A model's output
  can make a rule *easier to write*; it can never make one *fire*.
- **Earned, never assumed.** A rule answers live only after its shadow record
  shows the owner would have given the same answer, and stops at the first sign
  it would not.
- **Fail closed for answering, fail open for telling.** If anything a rule
  depends on cannot be read (the classifier, GitHub, the session's state), the
  ask is not answered and the owner is told as today. Telemetry failing never
  hides an ask.

## Non-goals

- **Composing answers.** muster sends one of two fixed texts. An LLM's
  proposed answer is shown in the review UI and never sent. The [suggested
  replies](../suggested-replies.md) guarantee ("A suggestion is never sent by
  muster") is unchanged.
- **Picking `AskUserQuestion` options, answering a permission prompt, or an
  opencode permission or form.** These are recorded for review only (operator
  decision 3).
- **Recording mirrored permission prompts.** Listed as [later](#later).
- **Answering a dispatched agent's question by changing its instructions.** An
  auto-answer is the same fixed text whoever asked.
- **Learning rules automatically.** muster proposes a rule and offers a
  promotion; it never creates, widens or promotes one on its own.
- **Other owners' asks.** Row-level security as everywhere else; a rule is one
  owner's and matches only that owner's asks.

## What exists today

### The measurement behind this page

Over 7 days on the operator's deployment (`session_episodes`, read-only):

| episodes | count | JEV-labelled |
|---|---|---|
| `needs_you`, waiting (a prompt or a picker on screen) | 1204 | 10 |
| `needs_you`, idle past the stall threshold | 80 | 80 |
| `stalled` | 117 | 116 |

Of the 1284 `needs_you` episodes, 1042 ended with `end_reason = owner`, 1016 of
them waiting ones. In the same 7 days `attention_asks` saw 21 asks answered
on the Asks page, 23 answered in the session, 5 answered by the chief, 187
cancelled, 72 expired and 2 still open.

🔴 **The "~93% unlabelled" gap is structural, not a fault.** Every unlabelled
episode but one is a *waiting* one, and the classifier never runs on a waiting
session: it runs on the stall sweep, for an idle candidate past
`MUSTER_CHIEF_NUDGE_IDLE` ([chief-nudges.md](../chief-nudges.md#what-counts-as-stalled)),
and a waiting session is never idle. Every idle `needs_you` episode is
labelled. So (a) the bulk of what the owner answers today is a prompt or a
picker, which this page records but never answers, and (b) a plain-text
question answered *before* the idle threshold opens no episode at all and is
invisible to today's tables. Phase 1 closes (b); nothing needs to "fix" JEV
for (a).

The table cannot split waiting episodes into permission prompts versus
`AskUserQuestion` pickers versus opencode asks. Phase 1's log can.

### Where each kind of ask lives today

| kind | stored | question text | answer | answered via |
|---|---|---|---|---|
| Plain-text question at turn end | `session_episodes` (0073, 0077, 0079): the `jev_*` scores and `end_reason` only, and only once past the idle threshold | not stored | not stored | the owner types in the pane, or the send queue |
| `AskUserQuestion` | in memory only, `internal/askq/store.go:18-24`; resolved entries kept `Retain = 15 * time.Minute` (`:142`) | in memory | in memory (the chosen option when muster typed it) | opened by `handleMirrorAskOpen` (`internal/api/mirror_ask.go:152`); answered by `AnswerWindow` (`internal/mirror/askanswer.go:111`) as keystrokes, step-up gated (`mirror_ask.go:416`) |
| `attention_ask` | `attention_asks` (0025), kept forever; no repository or working directory | yes | `answer`, `answered_by` (`operator`, or `chief` for a `to_chief` ask since 0050) | `POST /attention/asks/{id}/answer`, `attention_answer`, or a message in the session (`answered_in_session`, 0026) |
| opencode question or permission request | the host's memory (`internal/mirrorhost/opencode_asks.go:52-70`) | live only | not stored | `handleAttentionOpenCodeAnswer` (`internal/api/attention_opencode.go:187`), step-up gated (`:207`), "always" refused (`:32`) |
| Permission prompt (Claude Code) | nowhere; the window's `waiting` status | the host's pending-action summary, live | not stored | the terminal only |

Metrics: `internal/metrics/metrics.go` has one ask-shaped counter,
`muster_mirror_ask_typing_total` (`:105`, answers muster typed into a picker,
by outcome), and nothing that counts asks raised, answered or how fast.

### The classifier and its one door

JEV is `~typesafe/jev-latest` through OpenRouter's Decisions API
(`internal/chiefnudge/jev.go:35-39`), labels `needs_input`, `asks_permission`,
`neither` (`:43-44`), with the backtested ask line `DefaultAskThreshold = 0.9`
(`:87`) on `P(needs_input) + P(asks_permission)`. Every call goes through
`classifyStall` (`internal/api/session_state.go:547`), which cuts and redacts
its input (`stallClassifierText`, `:543`), and a ledger pins its callers to
two (`internal/api/fleet_collector_test.go:236`). Any new caller on this page
goes through that door and updates that ledger.

### The send path and its guards

A chief nudge is queued by `queueNudgeText` (`internal/api/chief_nudge.go:1419`)
into `SendQueue.Enqueue` (`:1426`) with `StepUpAt` stamped by the server and
`ChiefNudgeID` marking the row's origin (`internal/sendqueue/store.go:157-162`):
an origin-marked row is gated on its origin, not on a browser sign-in, is never
edited or retried, and carries at most one origin (`send_queue_one_origin`,
0064). The queue waits for idle and refuses a window that is waiting or has
text in its input; the host's prompt guard re-checks at the moment of typing,
before the paste and again before its Enter
(`internal/mirrorhost/claude_guard.go:15-38`). The nudge guards in
[chief-nudges.md](../chief-nudges.md#the-guards) (still the same streak, the
episode not superseded, unknown is not moved on, two per message of the
owner's, an hourly cap) are the template this design reuses.

### What a merge question could be checked against

- `session_prs` (0055) records which of the owner's Claude Code sessions
  created a pull request or pushed a branch, from the host's PostToolUse hook.
- The GitHub App reads a pull request server-side (`PullRequestState`,
  `internal/githubapp/app.go:892`) with a token minted for one repository and
  `metadata`, `pull_requests` read only (`prReadMintRequest`, `:849`). The App's
  registration already includes `checks: read` and `statuses: read`
  ([github-app.md](../github-app.md#register-the-app)); muster's own read
  token does not request them yet.
- Shipping officers already implement "every required check is green on this
  SHA" (`checksGreen`, `internal/officership/gates.go:392`: a status whose
  latest state is `success`, else the newest completed, successful check run;
  missing is not passed) and "a named review check from a named App passed"
  (`reviewPassed`, `:432`; neutral does not pass).
- The audit ladder's fenced `audit-claims` blocks on a PR
  (`audit-claims round=<n> audited=<from>..<to>`, then numbered claim lines)
  record what a round **claimed fixed**. 🔴 **They carry no severity and no
  verdict.** "The ladder ended with no 🔴" is today a judgement written in a
  comment's prose, which nothing may parse. See
  [Merge when green](#merge-when-green-what-muster-verifies).

## Design

### Who decides

| | may do | may never do |
|---|---|---|
| **A rule** (owner-approved, deterministic) | answer an ask with one of the two fixed texts, at its tier | answer anything the never-auto list names; answer at a tier the owner did not set |
| **JEV** | score an ask; its label and score are rule *inputs* | answer; create or promote a rule |
| **The LLM proposer** | propose a class, a rule and an answer, shown in the Log | be a rule input; answer; create or promote a rule |
| **The never-auto list** (code) | refuse an answer, before any rule is read | be turned off, by a rule, an env var or the owner |
| **The owner** | write, promote, demote, delete rules; undo | promote a rule past what the list allows |

A rule that matched in `live` is still refused if the list refuses the ask,
if any input it needs is unknown, or if a cap is reached. Each refusal is
recorded with its reason.

### The ask log

One table, `asks` (migration: the next free number, 0087 at the time of
writing). Owned and under forced row-level security in 0048's shape,
re-applied by `db.RLSOn`.

| column | type | notes |
|---|---|---|
| `id` | bigint identity | |
| `owner_id` | bigint | `muster.uid` default, as every owned table |
| `kind` | text | `plain_text`, `ask_user_question`, `attention_ask`, `opencode_question`, `opencode_permission` (CHECK; `permission_prompt` is [later](#later)) |
| `runtime` | text | `claude-code`, `opencode`, `agent` |
| `mirror_host`, `mirror_session`, `mirror_window` | text | as on `session_episodes` |
| `claude_session` | text | the Claude Code or opencode session id ('' for an agent) |
| `agent_id`, `chat_session_id` | bigint | a dispatched agent's ask |
| `repo` | text | `owner/name` when known (an agent's `Repo`, or the `session_prs` row of this session), else '' |
| `cwd` | text | the window's working directory as the host reports it, cut to 300 |
| `episode_id` | bigint | `session_episodes(id)`, `ON DELETE SET NULL`; the stop this ask was raised in |
| `source_ref` | text | the source's own id: `attention_asks.id`, the askq id, the opencode ask id; '' for plain text |
| `question` | text | redacted (`redact.Secrets`, `internal/redact/redact.go:85`), cut to 4000; '' once purged |
| `question_norm` | text | the [normalised](#normalisation) question, cut to 500; '' once purged |
| `options` | jsonb | the offered options (`AskUserQuestion`: each question's header, options and multi-select flag; `attention_ask`: its options) |
| `jev_label`, `jev_needs_input`, `jev_asks_permission`, `jev_neither`, `jev_ask_score`, `jev_model`, `jev_error` | | as on `session_episodes`; empty when JEV did not run |
| `proposed_class` | text | the LLM proposer's class: `continue`, `merge`, `choice`, `info`, `permission`, `other`, '' (not run) |
| `proposed_answer` | text | the proposer's suggested answer, redacted, cut to 500; never sent |
| `never_reason` | text | the never-auto list's first refusal for this ask, '' if none (computed at capture, again at delivery) |
| `shadow_rule_id`, `shadow_answer_kind` | | the rule that would have answered in shadow, and with what |
| `answer` | text | what the ask was answered with: the chosen option labels, the attention answer, or the owner's next message for a plain-text question (redacted, cut to 8000; '' once purged) |
| `answered_by` | text | `owner`, `chief`, `auto:<rule_id>`, `shadow` (shadow writes the *would-be* answer only into `shadow_*`; `shadow` here marks an ask nobody else answered before it ended), or '' |
| `answer_provenance` | text | for a plain-text reply: `muster_send` (a send-queue body muster pasted for the owner), `pane` (a user message muster did not paste: the owner typing, or anything else it cannot attribute), `nudge`, `route` |
| `agreement` | text | `agree`, `disagree`, `other`, '' (see [promotion](#promotion-and-demotion)) |
| `outcome` | text | `answered`, `answered_in_session`, `declined`, `expired`, `cancelled`, `superseded`, `moved_on` (the session went on without a recorded answer), `gone` |
| `asked_at`, `answered_at` | timestamptz | `latency` is their difference, computed, not stored |
| `text_purged_at` | timestamptz | set when the text columns are cleared |

One row per ask. A question asked again in a later turn is a new row.
Indexes: `(owner_id, asked_at desc)`, `(owner_id, kind, asked_at desc)`,
`(owner_id, shadow_rule_id)`, and a unique `(owner_id, kind, source_ref)`
where `source_ref <> ''`, so a capture that runs twice records one row.

#### Capture points

| kind | opened | resolved |
|---|---|---|
| `plain_text` | on the mirror hub's change hook (`attentionMirrorChanged`, `internal/api/attention.go:1041`) when a Claude Code window that names its session goes from busy to idle, and for a dispatched agent when a turn ends with an assistant message. The host already reads the window's last messages for the stall sweep and suggested replies; this reads the same `read.Msgs` and takes the last assistant message. **Pre-filter, in code, no model:** its last paragraph contains `?`, or begins with one of a short list (`shall i`, `should i`, `want me to`, `do you want`, `ready to`, `let me know`). A turn end that fails the pre-filter writes no row and increments a counter | when the session next turns busy, or its episode ends: the first user-role message after the question becomes `answer`, with its `answer_provenance`. No message before the window is gone: `gone`. A new assistant turn with no user message in between: `moved_on` |
| `ask_user_question` | in `handleMirrorAskOpen` (`mirror_ask.go:152`), after the store accepts the question | when the askq entry leaves `pending` (`internal/askq/store.go:43-61`): `answered` with the chosen labels when muster typed them; `answered` with `answer = ''` when the picker was answered in the terminal and muster read only that it closed; `declined`, `expired`, `superseded` |
| `attention_ask` | where the attention store creates an ask of kind `ask` (not a `notice`, 0071) | where it leaves `open`: `answer`, `answered_by` (`operator` → `owner`, `chief` → `chief`), `answered_in_session`, `cancelled`, `expired` |
| `opencode_question`, `opencode_permission` | where the server first receives the held ask from the host | `handleAttentionOpenCodeAnswer` (`attention_opencode.go:187`), or the ask leaving the host's list (`moved_on`) |

The `plain_text` capture also writes JEV's scores: when the stall sweep has
already classified this streak, its result is reused (`nudgeRetry.keepJev`);
otherwise one call goes through `classifyStall`, so the ledger at
`fleet_collector_test.go:236` grows by exactly one named caller.

#### Linking the owner's reply to a plain-text question

The reply is the **first user-role message after the question** in the same
Claude Code session, read from the transcript. Its provenance is what muster
can prove: a body muster pasted from the owner's send queue is `muster_send`;
a nudge or a route body is `nudge` or `route`; anything else is `pane`.
[session-state.md](session-state.md#repositories-and-message-provenance)
explains why a pane message has no author: text another agent typed with
`tmux send-keys` is indistinguishable from the owner typing. Promotion counts
`muster_send` and `pane` replies and says so on the rule's page, so the owner
sees how many of its agreements are attributable.

#### Normalisation

`question_norm` is what a rule's pattern is compared with. In code, one
function: NFC; lowercase; the last paragraph only (what the session asked, not
its report); runs of whitespace to one space; a GitHub pull-request URL or
`#<digits>` to `{pr}`; a 7–40 character hex run to `{sha}`; other digit runs
to `{n}`; a token with a `/` and no space to `{path}`; backticked spans to
`{code}`; leading and trailing punctuation stripped. The function is tested
with literal input/output pairs, never with its own output.

#### Redaction and retention

- Every text column passes through `redact.Secrets` before it is written: the
  one redactor, never a second.
- 🔴 **Question and answer text is transcript content.** A daily sweep clears
  `question`, `question_norm`, `answer` and `proposed_answer` on rows older than
  `MUSTER_ASK_LOG_TEXT_TTL` (default `720h`, 30 days), and sets
  `text_purged_at`. The rest of the row (kind, where, scores, class, who
  answered, latency, outcome, rule) is kept 180 days, so rule statistics and
  metrics outlive the text. A rule keeps its own pattern; it does not keep the
  questions it was made from.
- Owner-only: forced row-level security, no admin read path, no cross-owner
  aggregate in the UI. Prometheus labels carry no text, no host, no repository
  and no rule id.
- `attention_asks` keeps its own retention (forever) unchanged; its row here
  is a redacted copy under the TTL.

#### Metrics

| metric | type | labels |
|---|---|---|
| `muster_asks_total` | counter | `kind` |
| `muster_ask_plain_text_prefilter_total` | counter | `result` (`kept`, `dropped`) |
| `muster_ask_answer_seconds` | histogram | `kind`, `answered_by` (`owner`, `chief`, `auto`, `none`) |
| `muster_ask_capture_errors_total` | counter | `kind`, `stage` (`read`, `classify`, `write`) |
| `muster_ask_rule_decisions_total` | counter | `tier` (`shadow`, `live`), `result` (`answered`, `never`, `capped`, `unknown_input`, `moved_on`, `superseded`) |
| `muster_ask_never_total` | counter | `reason` (the list's fixed reason names) |
| `muster_ask_rule_demotions_total` | counter | `cause` (`disagree`, `owner_undo`, `edit`) |

### The never-auto list

Hard-coded, in one function, `never.Check(ask, session) (Cleared, Reason)`,
evaluated before any rule is read, at capture (for the Log) and again at
delivery. No configuration can remove an entry. Each entry has a fixed reason
name; the first that matches is recorded.

| reason | refuses an ask when |
|---|---|
| `kind` | its kind is not `plain_text` or `attention_ask` (so `AskUserQuestion`, opencode asks and permission prompts are never answered) |
| `waiting` | the session reports `waiting`, the host's guard holds it, or its state cannot be read |
| `options` | it offers options (an `attention_ask` with a non-empty `options`; a plain-text question naming two or more alternatives: ` or `, a numbered or lettered list) |
| `multi` | its last paragraph holds more than one `?` |
| `destructive` | it mentions deleting, dropping, truncating, force-pushing, hard resets, `rm`, revoking, rotating, wiping, destroying or overwriting (word list in code) |
| `deploy` | it mentions deploying, releasing, publishing, tagging, rolling out, production, rollback, migration apply |
| `secret` | it mentions a credential, secret, token, key, password, or `redact.HasSecretValue` is true of it |
| `merge_unverified` | it mentions merging and the [merge check](#merge-when-green-what-muster-verifies) did not pass for this exact ask (so a merge is never answered by a `continue` rule) |
| `stepup` | it asks for something muster gates behind a fresh sign-in or the owner's own act: typing into another session, a connector grant, a privilege, an `agent_dispatch` needing `owner_asked`, opencode "always" |
| `outward` | it asks to send, post, reply, email, comment, publish or message outside the session (a reply draft, a PR comment, a message to a person), or to mark a PR ready for review |
| `external_text` | since the owner's last message to this session, the session was routed a message (`session_routes`), received a plugin or review-mail route, or its transcript shows a tool result from a web-fetch or other outside-content tool; the [prompt-injection warning](../attention.md#mirrored-claude-code-sessions) applies |
| `config` | it mentions settings, hooks, permissions, `CLAUDE.md`, guards or muster's own rules |
| `subject` | the session is the chief, an officer, another owner's, or an opencode window (view only) |
| `classifier` | JEV errored, was not configured, or is below the rule's floor for this ask |
| `length` | the normalised question is longer than 300 characters |

The word lists are a **second net**, deliberately wide: a false positive costs
one ask the owner answers by hand. The first net is the rule's pattern, which
must match the whole normalised question.

🔴 **The list is a seam, not a component.** Every path that can deliver an
auto-answer takes a `never.Cleared` value that only `never.Check` can
construct, and a ledger test pins the set of callers of the delivery function,
failing when it grows or shrinks (the `classifyStall` ledger's shape).

### The rule model

`ask_rules`, one row per rule, owned, forced row-level security.

#### Matchers

All must match; an empty matcher is not "anything" unless the table says so.

| matcher | required | meaning |
|---|---|---|
| `kinds` | yes | a subset of `plain_text`, `attention_ask` |
| `patterns` | yes, 1 to 20 | normalised templates; the ask's `question_norm` must **equal** one of them (placeholders match their own token only) |
| `repo_glob` | no ('' = any) | a glob over `repo`, e.g. `acme/*` |
| `scope` | no | at most one of: a `claude_session`, a `mirror_host`, an `agent_id` |
| `jev_min_ask_score` | yes for `plain_text` | default `0.9`, the backtested line |
| `jev_labels` | no | a subset of `needs_input`, `asks_permission` |
| `answer_kind` | yes | `continue` or `merge_when_green` |

The LLM's `proposed_class` is never a matcher. JEV alone never matches: every
rule has a pattern.

#### Answer kinds

Fixed texts, built in code, labelled so the session and the transcript show
who answered:

- `continue`: `[muster auto-answer, rule R<id>] Yes, continue.`
- `merge_when_green`: `[muster auto-answer, rule R<id>] Yes, merge #<n>. CI is
  green on <sha12> and the audit ladder ended with no 🔴 at round <k>. Merge
  only that commit: gh pr merge <n> --match-head-commit <sha40>.`

The second pins the commit, so a push after the check makes the agent's merge
fail rather than merge unverified code.

#### Tiers: off, shadow, live

- `off`: never evaluated.
- `shadow`: evaluated at capture; a match writes `shadow_rule_id` and
  `shadow_answer_kind` on the ask and nothing else. Not capped (it sends
  nothing), as shadow nudges are not.
- `live`: evaluated at capture; a match that passes the list, the caps and the
  [delivery checks](#delivering-an-auto-answer) is answered.

A new rule, including one made from the Log, starts in `shadow`. Any edit that
widens it (a new pattern, a wider glob, a lower score floor, a new kind, a
removed scope) puts a `live` rule back in `shadow`. Narrowing keeps its tier.

Deployment ceiling: `MUSTER_ASK_AUTO` = `off` (default) | `shadow` | `live`.
Below `live`, a `live` rule behaves as `shadow`. `merge_when_green` rules also
need `MUSTER_ASK_AUTO_MERGE=on`.

#### Promotion and demotion

**Agreement**, computed in code over the owner's actual answer to an ask a rule
matched in shadow:

- `agree`: the owner's normalised reply is, in full, one of a fixed set
  (`yes`, `y`, `yep`, `ok`, `okay`, `sure`, `continue`, `proceed`, `go`,
  `go ahead`, `do it`, `keep going`, `carry on`, `lgtm`, `ship it`, plus for
  `merge_when_green`: `merge`, `merge it`, `merge when green`), or an
  `attention_ask` answer that is.
- `disagree`: the reply starts with `no`, `stop`, `wait`, `hold`, `don't`,
  `do not`, `not yet`.
- `other`: anything else, including a reply that agrees and adds instructions
  (the owner said more than the fixed text would).

**Promotion** is offered (a button on the rule's page, a line in the Log) when
all hold over the rule's shadow matches: at least 10 answered ones, from at
least 3 distinct sessions, `agree` on at least 90%, and no `disagree` among the
last 10. The owner promotes with a fresh sign-in (step-up); muster never
promotes. The sign-in stands in for the per-answer step-up the owner is no
longer asked for, the way a route's checks stand in for a routed message's
([plugins.md](../plugins.md#routing-a-task-to-a-session)).

**Demotion** to `shadow` is automatic and immediate on: the owner tapping
**Wrong** on an auto-answer's notification or Log row; the owner's next message
to the session within 10 minutes of an auto-answer being `disagree`; any
widening edit. Each writes an audit row and resets the rule's promotion
window.

#### Caps

- At most **2 auto-answers per message of the owner's** in one session (the
  nudge rule): the third ask is left for the owner.
- `MUSTER_ASK_AUTO_HOURLY_CAP` (default 20) live auto-answers per owner per
  hour; per rule, 10 per hour.
- `merge_when_green`: one per PR head SHA, 3 per owner per hour.
- A capped match is recorded (`capped`), and the ask stays the owner's.

#### Audit trail

Every rule create, edit, tier change and delete, and every live answer, writes
an audit row (`ask_rule.created`, `.edited`, `.promoted`, `.demoted`,
`.deleted`, `ask.auto_answered`) with the rule id, the tier, the ask id and,
for a merge, the PR, head SHA, check summary and audit round read.

### Merge when green: what muster verifies

An ask passes the merge check only if **every** step below reads true, at
capture and again immediately before delivery. Anything that cannot be read is
a failure, and the ask is never auto-answered.

1. **Which PR.** The question names exactly one PR (`{pr}` once): a full URL,
   or `#<n>` resolved against this session's `session_prs` rows (the PR it
   created, or the one open on a branch it pushed). Zero, two, or a PR this
   session did not create or push: refused. A dispatched agent's merge
   question has no `session_prs` row and is refused.
2. **Its state** (the GitHub App, a token for that repository): open, not a
   draft, base is the repository's default branch, and the head SHA is read
   here and held for every step below.
3. **CI green on that SHA.** The same rule as `checksGreen`
   (`officership/gates.go:392`), extracted so both callers share one
   implementation: every commit status context's latest state is `success`,
   and every check run's newest attempt is `completed` with conclusion
   `success` or `skipped`; nothing pending, queued or in progress; at least one
   check present (no checks is not green). This needs muster's PR-read token to
   request `checks: read` and `statuses: read`, which the App's registration
   already has.
4. **Audit ladder ended with no 🔴 on that SHA** (operator decision 6). The
   newest check run named `MUSTER_ASK_AUTO_AUDIT_CHECK` on the head SHA from
   step 2, created by the GitHub App whose slug is
   `MUSTER_ASK_AUTO_AUDIT_APP` (runs from any other App are ignored, as
   `reviewPassed` filters by `ReviewApp`), is `completed` with conclusion
   `success`. Neutral, skipped, failure, pending, absent, or either variable
   unset: not passed. A later commit has no such run, so it voids the verdict.
   Who posts that run, and how its credential is kept from sessions, is
   [The audit verdict poster](#the-audit-verdict-poster).
5. **Unchanged at delivery.** Steps 2 to 4 are read again immediately before
   the answer is queued; a different head SHA, or any step now false, makes
   the ask the owner's (`superseded`).

The answer text then pins the SHA (`--match-head-commit`), so a push between
delivery and the agent's merge fails the merge.

#### The audit verdict poster

The check run is written outside muster, by the audit ladder's own tooling:
the operator's audit-dispatch scripts, which live in a separate, private
repository. That work is part of phase 4's scope.

- **A dedicated GitHub App, used for nothing else.** Permissions:
  `checks: write` and `metadata: read` only, installed on the repositories
  whose merges may be auto-answered. It is neither muster's App nor any App or
  token a session uses, so a session's GitHub credential cannot create a run
  under its slug: GitHub stamps every check run with the App that created it,
  and muster reads only runs from that slug.
- **Its private key never enters a session's reach.** It is held by a small
  poster process that runs as a separate OS user (on a host) or in its own
  namespace with its own Secret (in a cluster). The key file is readable by
  that user only, and is never in an environment variable, a repository, a
  session's working tree or anything mounted into a session or an agent pod.
  The ladder runner never holds the key: it sends the poster a request.
- **The poster checks before it posts.** A request names the repository, PR,
  head SHA, round number and verdict. The poster reads the PR itself (through
  its own installation token) and refuses unless the PR's head is that SHA,
  the PR carries a parseable `audit-claims` block for that round whose range
  ends at that SHA, and the round is at least 2 (the ladder's floor). It then
  posts the run: conclusion `success` for a verdict with no 🔴, `failure`
  otherwise, with the round and the claims block's range in the run's summary.
- **What it proves, and what it does not.** A run under that slug proves the
  ladder tooling recorded "no 🔴" for exactly that commit, after a real round
  posted its claims for it. It does not prove the audit was right. A session
  that can reach the poster can still ask it to post, so the poster's request
  channel is reachable only by the operator's user (a local socket owned by
  the operator, not by the session user, where they differ) and every request
  is logged with its caller. Where sessions run as the operator's own user,
  this is the accepted residual: forging a verdict then also needs a
  well-formed claims block from a round on that exact SHA, which is posted in
  public on the PR.

### Delivering an auto-answer

| kind | path | what stands in for the owner's step-up |
|---|---|---|
| `plain_text`, mirrored Claude Code | the send queue, as `queueNudgeText` does: a new origin column `send_queue.auto_answer_id` (the ask id), added to `send_queue_one_origin`; never edited or retried | at paste: the ask is still the newest assistant message, the session is in the same idle streak, its episode is the one the ask was raised in, and the rule is still `live` |
| `plain_text`, dispatched agent | a turn, as `sendNudgeTurn` sends a nudge | the same checks against the agent's conversation |
| `attention_ask` | the attention store's answer, with `answered_by = 'auto'` (0025's CHECK widened once more) and the audit row naming the rule | the ask is still `open` (the store's one conditional UPDATE) |

🔴 **Never into a prompt.** The queue holds a window that is waiting or has
text in its input, and the host's prompt guard
(`internal/mirrorhost/claude_guard.go`) refuses the paste and its Enter at the
moment of typing. An auto-answer adds no new typing path.

An auto-answer pasted into a session is not the owner's message: it does not
mark the session's other open asks `answered_in_session`
([attention.md](../attention.md#answering): "Only your own messages count").

### Notify and undo

Each live auto-answer clears that ask's own notification. Then (operator
decision 7):

- **For a rule's first 30 days live** (counted from its latest promotion, so a
  demoted and re-promoted rule starts again): one low-urgency notification per
  session, replaced rather than stacked: *"Auto-answered 'continue' in
  `<window>` (rule R<id>). Tap to review."* Tap opens the Log row.
- **After that, a digest:** no notification per answer. One notification a
  day, at 09:00 in the owner's time zone, and only when there was at least one
  answer: *"muster auto-answered N asks yesterday (rule R<id>: n, …)."* Tap
  opens the Log filtered to auto-answers from that day.
- A `merge_when_green` answer always gets its own notification, whatever the
  rule's age.

From the row: **Wrong** demotes the rule; **Stop the session** queues the
owner's own message *"Stop. Disregard the auto-answer and wait for me."* through
the normal, step-up-gated send. A merge cannot be undone by muster; **Wrong**
still demotes the rule.

### The review UI

A **Log** tab on the Asks page (`/attention?tab=log`):

- **List**, newest first: kind, where (the window's `<where>`), repo, the
  question's first line, the answer and who gave it, latency, and a **shadow**
  column: "would answer *continue* (R12)", "never: deploy", or nothing.
- **Filters**: kind, repo, host, session, answered by, proposed class, shadow
  would-answer, never reason, rule, time range.
- **Make a rule from this** on any `plain_text` or `attention_ask` row: a
  prefilled rule (kind, this repo, the row's `question_norm` as the pattern,
  JEV floor at the default, answer kind from the proposed class when it is
  `continue` or `merge`). It is created in `shadow`. Rows sharing its
  `question_norm` are counted on the form ("matches 14 past asks, you agreed
  with 13").
- **Rules** (`/attention?tab=rules`): each rule's tier, matchers, matches,
  agreement, last 20 matches, promote (when offered), demote, edit, delete.
- Rows whose text was purged show "text expired" and keep everything else.

### JEV's role and cost

- **When it runs.** As today on the stall sweep; and, new, once per
  plain-text turn end that passes the pre-filter, reusing the streak's result
  when the sweep already has one. Never on `AskUserQuestion` or opencode asks
  (their kind is enough).
- **Cost.** One Decisions call per kept turn end, on the owner's key, capped at
  `MaxClassifyBytes` (8 KiB) input and `DefaultClassifyTimeout` (5 s). Phase 1
  ships the pre-filter counter first, with JEV at capture behind
  `MUSTER_ASK_LOG_CLASSIFY` (default off), so the call volume is measured
  before it is spent.
- **The unlabelled gap** needs no fix for this design: it is waiting sessions,
  which no rule may answer ([above](#the-measurement-behind-this-page)).
- **The LLM proposer** (phase 2) runs only when the owner opens a Log row or
  asks for proposals over a filter, never per ask, through the same
  OpenRouter key and the same redacted, cut input as suggested replies.

## Risks

- **A wrong `continue`.** A question the pattern matched but whose context made
  "yes" wrong. Mitigated by whole-question patterns, the list, the shadow
  record, two per owner message and immediate demotion; accepted residual: the
  session ran one step it should not have, and the owner was told.
- **A forged audit verdict.** Sessions commonly hold the owner's own GitHub
  identity, so a PR comment is a signal they could write. The verdict is
  therefore a check run from a dedicated App whose key no session holds
  (decision 6). The residual risk, a session asking the poster to post, is
  stated in [The audit verdict poster](#the-audit-verdict-poster).
- **A digest hides a wrong answer for up to a day.** Accepted by decision 7
  once a rule has had 30 days of per-session notifications. Demotion on
  disagreement still applies at once.
- **Pattern drift.** Agents phrase the same question many ways; rules may need
  many patterns. Cost: more shadow time, never a looser match.
- **Pane replies are not attributed.** Agreement may count a message another
  agent typed. The rule page shows the split.
- **Telemetry holds transcript text.** Bounded by redaction, the TTL and
  owner-only row-level security.

## Phases

Each phase is its own PR, off by default, and changes nothing for an owner who
does not turn it on. Every regression test is shown red on the pre-change
commit and green after; the PR states that matrix.

### Phase 1: telemetry capture

- **Files.** `internal/db/migrations/0087_asks.sql` (or the next free number);
  `internal/asklog/` (store, normalise, purge sweep); capture calls in
  `internal/api/attention.go` (turn end), `mirror_ask.go` and the askq
  resolution path, the attention store's create and resolve, and
  `attention_opencode.go`; metrics in `internal/metrics/metrics.go`;
  `MUSTER_ASK_LOG`, `MUSTER_ASK_LOG_TEXT_TTL`, `MUSTER_ASK_LOG_CLASSIFY` in
  `cmd/muster-server/config.go` and [configuration.md](../configuration.md);
  a row in [schema.md](../schema.md); a user page `docs/asks.md`.
- **Tests.** One capture test per kind against the test database, asserting
  literal column values; the pre-filter with a table of literal kept and
  dropped questions; normalisation with literal pairs; the reply link
  (provenance for each source); the purge sweep clears exactly the text
  columns; row-level security (another owner reads nothing); the
  `classifyStall` ledger grows by exactly one; redaction (a secret-shaped
  value in a question is stored redacted). **Mutation targets:** the pre-filter
  list, the provenance switch, the TTL comparison, the redaction call (removing
  it must fail the redaction test).
- **Rollout.** Deploy with `MUSTER_ASK_LOG=on`, classification off; read
  `muster_ask_plain_text_prefilter_total` and `muster_asks_total` for a week;
  then turn classification on.

### Phase 2: review UI and shadow

- **Files.** `ask_rules` migration; `internal/askrules/` (matcher, tiers,
  agreement, promotion offer); `internal/askrules/never/` (the list);
  the LLM proposer; `internal/ui/` Log and Rules tabs; routes
  `GET /attention?tab=log|rules`, `POST /attention/rules`,
  `POST /attention/rules/{id}` (edit, promote, demote, delete; promote
  step-up gated).
- **Tests.** A table test for each never-auto reason with a question that
  trips only that reason, plus a positive control that trips none; the matcher
  with literal templates (placeholders match only their own token; a superset
  question does not match); shadow writes only `shadow_*`; a widening edit
  drops `live` to `shadow`, a narrowing one does not; promotion offer at
  exactly the thresholds (9 answered: no; 10: yes) with fixture counts that
  overshoot, not land on, each boundary; promote without a fresh sign-in is
  refused. **Seam guard:** the ledger of `never.Check` callers and of
  `never.Cleared` constructors. **Mutation targets:** each list entry deleted
  in turn (its table case must go red with its own reason name), the equality
  in the matcher swapped for a prefix test, the promotion comparisons.
- **Rollout.** `MUSTER_ASK_AUTO=shadow`. The owner writes rules from the Log
  and watches their shadow record. Nothing is answered.

### Phase 3: live `continue`

- **Files.** `internal/api/ask_auto.go` (the deliverer); the
  `send_queue.auto_answer_id` origin and the `answered_by = 'auto'` CHECK
  (one migration); the notification and the Wrong/Stop actions; the
  disagreement demotion.
- **Tests.** The behavioural seam case: a `live` rule matching a question that
  trips each never-auto reason queues nothing (the send queue is empty, the
  ask records the reason), and the same rule on a harmless question queues
  exactly one row, with the fixed text, byte for byte. Delivery checks: a new
  assistant message, a new streak, a superseded episode, a demoted rule each
  drop the row at paste. Caps: the third ask after one owner message is not
  answered; the hourly cap. A waiting window holds the row (and the host guard
  test still refuses the paste). An auto-answer does not mark other asks
  `answered_in_session`. **Mutation targets:** each delivery check, the cap
  counters, the tier comparison (`shadow` must never send).
- **Rollout.** `MUSTER_ASK_AUTO=live` on the operator's deployment, one
  promoted rule at a time, the Log watched for a week before the next.

### Phase 4: merge when green

- **Files.** `checksGreen` extracted from `internal/officership/gates.go` to a
  shared package both callers use; `internal/askrules/mergegate.go`; the
  PR-read mint widened to `checks: read`, `statuses: read` (muster's own read,
  never injected into an agent); the audit-verdict reader (the check run filtered
  by App slug, sharing `reviewPassed`'s shape); `MUSTER_ASK_AUTO_MERGE`,
  `MUSTER_ASK_AUTO_AUDIT_CHECK`, `MUSTER_ASK_AUTO_AUDIT_APP`.
- **Runner-side work, outside this repository.** In the operator's private
  audit-dispatch tooling: register the dedicated verdict App; build the poster
  ([The audit verdict poster](#the-audit-verdict-poster)) with its key under a
  separate user or namespace; have the ladder runner, when a round ends with no
  🔴, send the poster `{repo, pr, head_sha, round, verdict}`, and send a
  `failure` verdict when a round has a 🔴. Its tests live in that repository:
  the poster's refusals (head moved, no claims block for the round, round below
  2) each shown red, and a check that the key path is unreadable by the
  session user.
- **Tests.** Against a fake GitHub with literal fixtures: green, one pending,
  one failed, one neutral, none at all, status-versus-run precedence, a newer
  failed attempt over an older success; the verdict run present, absent,
  neutral, failed, on another SHA, from another App slug, with the variables
  unset; the head moving between check and delivery; a question naming two PRs
  or a PR this session did not create. The officership gate's existing tests
  must still pass against the extracted function. **Mutation targets:**
  `completed`, `success`, missing-is-not-green, the SHA binding, the App-slug
  filter, the re-read at delivery.
- **Rollout.** The poster and App first, posting runs that nothing reads yet;
  then `MUSTER_ASK_AUTO_MERGE=on` with a `merge_when_green` rule in `shadow`;
  promotion only after its shadow record meets the criteria.

### Later

- **Mirrored permission prompts** as a recorded kind (`permission_prompt`),
  from the host's pending-action summary, logged only. The measurement above
  suggests they are most of what the owner answers.

## Open questions

None. The draft's three were answered by the operator (decisions 6 to 8), and
its defaults were adopted as listed (decision 9).

## Operator decisions

1. **What is recorded.** Plain-text questions at turn end (the last assistant
   message and the owner's next reply); `AskUserQuestion` cards (questions,
   options, chosen answer); `attention_ask` and opencode permission and form
   asks, in one log. Not mirrored permission prompts for now ([later](#later)).
2. **Who decides: "Rules decide, JEV proposes."** Owner-approved deterministic
   rules are the only thing that can auto-answer. JEV and an LLM propose a
   classification, a suggested rule and an answer. Shadow mode first. Rules are
   promoted progressively by the owner. A built-in never-auto list always
   wins.
3. **The only answers.** (a) A generic continue / proceed / yes to a "shall I
   continue"-style question; (b) "merge when green": yes to a merge question
   only when the PR's CI is green and its audit ladder ended with no 🔴, both
   verified by muster; if either cannot be verified, the ask is never
   auto-answered. Not picking `AskUserQuestion` options, not allowing
   permission prompts: those stay logged only.
4. **This reverses two recorded rules, for those two answers only:** the
   non-goal "Answering for the owner… the Chief never composes an
   instruction" ([session-state.md](session-state.md#non-goals)) and "Only the
   operator answers" ([attention.md](../attention.md#answering)). Each now
   points here. The Chief still composes nothing: the two answers are fixed
   texts, and a rule, not the Chief, sends them.
5. **Deliverable.** This design first; phase 1 as its own PR after it.
6. **The audit verdict is a check run** (the draft's open question 1, option
   (b)): a configured check name from a configured GitHub App that sessions
   hold no credentials for, on the head SHA, `completed`/`success`. The ladder
   runner posts it, and that runner-side work is in phase 4's scope
   ([The audit verdict poster](#the-audit-verdict-poster)).
7. **Notifications** (open question 2): per session, replaced rather than
   stacked, for a rule's first month live; then a daily digest
   ([Notify and undo](#notify-and-undo)). Merge answers always notify on their
   own.
8. **A `continue` rule never answers an `attention_ask` that offers options**
   (open question 3): refused, as the never-auto list's `options` reason.
9. **The draft's defaults are adopted as listed:**
   - text TTL 30 days, metadata 180 days;
   - promotion is offered at 10 answered shadow matches, from 3 sessions, at
     least 90% agree and no disagree in the last 10, with no early promotion;
   - caps of 2 auto-answers per owner message per session, 20 per owner per
     hour and 10 per rule per hour, and for merges 1 per head SHA and 3 per
     hour;
   - a 10-minute disagreement window after an auto-answer;
   - JEV at capture off until the pre-filter volume is measured.

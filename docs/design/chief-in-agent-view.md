# Design: the chief in an agent's view (succession: replace or spin off, in place)

**Status: phase 0 measured, phases 1 and 2 built (see [Phase 0 results](#phase-0-results-2026-11-08),
[Phase 1 as built](#phase-1-as-built) and [Phase 2 as built](#phase-2-as-built)); phases 3–4 are
proposed.** Phase 2 follows the operator's decision of 2026-11-09 on how a view
line authorises a succession (a one-tap confirm card), which changes 4.2 and D5
as written below; [Phase 2 as built](#phase-2-as-built) says how. It builds on
[chief-dispatch.md](chief-dispatch.md) (phase 3 built for claude, #513/#516;
phase 4, opencode on an OpenRouter model, in draft as #522) and reuses its
target strings, `host_dispatches` row, reconciler, held cards and owner-turn
gate unchanged. Citations are `file:function` at `origin/main` `317116b4`
unless they name #522. A claim nothing confirmed is marked **UNVERIFIED**.
The operator answered this page's six open questions. Their answers are
recorded as [Decisions](#decisions-2026-11-08) D1-D6, and the text below follows
them.

## Summary

The operator is looking at an agent's rendered view, say a mirrored opencode
session that has finished and written a handoff and a kickoff message. They flip
the composer from **to the agent** to **to the chief, about this agent**, and type
*"copy the kickoff out of this session's tail, open a new opencode session and
run it"*. The chief does it. The new session appears in the view they are
already looking at, without any manual switch, and muster records that the new
session continues the old one.

- **One new door.** `POST /mirror/view/chief` is a third owner chat door beside
  `handleAgentWS` and `handleAgentSend`. It stores the owner's typed text
  verbatim, plus a server-resolved **subject** (host, tmux address, session key,
  runtime, dispatch) in a new `chat_messages.about` column, and runs the turn
  through `runEnteredTurn` with `typedBy = ownerTypedBy(ctx)`. So the turn is
  an owner's typed turn, and `ownerTurnFor` passes exactly as it does for the
  chief's own chat.
- **Two new read paths, one extended tool:**
  - `session_context {subject}` (read-only): the subject's runtime, host, repo,
    branch, model and status, plus a redacted tail. Each message in the tail
    has a content-addressed `ref`, and each fenced block in it is listed.
  - `agent_dispatch` gains `successor_of`, `succession: replace|spinoff`, and
    `prompt_from: {ref, block}`. muster copies the kickoff **verbatim** from the
    subject's transcript. The chief chooses *which* block, never the words.
  - Everything else is today's `agent_dispatch`: the targets, the model rules,
    the held cards, the caps, `request_key`, and `owner_asked`.
- **The binding is structural.** `successor_of` must equal the subject recorded
  on the owner's in-flight typed message, the one `ownerTurnFor` returns. A
  chief talked into anything by transcript text cannot point a succession at
  another session, cannot write the kickoff's words, and cannot act outside an
  owner's typed turn.
- **A lineage row, `session_successions`,** links old to new: the mode, who
  asked, the chief's chat message, and the successor's `host_dispatches` id.
  The reconciler already matches the new window to its dispatch through
  `@muster-dispatch`. When the successor's row turns `running`, muster emits
  `lineage.changed` on the per-owner `/events` stream. The open view (phone or
  desktop) fetches its successor and navigates to it.
- **Replace vs spin-off.** Both jump the view to the successor.
  - **Spin-off** leaves the old session alone.
  - **Replace** marks the old one *superseded*: a banner on its view, and its
    sidebar row folds into a "Superseded" group (collapse only, like Idle).
  - Muster can close no window today (D7 and decision 17 in
    chief-dispatch.md), so **closing the old window is phase 3, and only for
    windows the chief itself started**. muster never closes a window the
    operator opened by hand ([D1](#decisions-2026-11-08)).
- **When the owner's words name no mode,** muster asks with a one-tap
  Replace / Spin off card ([D2](#decisions-2026-11-08)).

---

## 1. Current state, measured from code

### 1.1 What the rendered view's composer can do, per runtime

There is one composer: `mirrorComposer` in `internal/ui/mirror_compose.go`,
drawn by `ui.MirrorViewPage` (`internal/ui/mirror_view.go:155`). The server
decides its state in `handleMirrorViewPage` (`internal/api/mirror_view.go:63-144`):

| window | composer | gate |
|---|---|---|
| Claude Code, `-interactive` host | **Send** ("Message Claude…"), send queue, suggested replies | `POST /mirror/view/send` → `mirrorSend`: same-origin, `-interactive`, **fresh sign-in** (step-up), then the host pastes only into a pane still running Claude Code |
| opencode, host advertised `-opencode-send`, pane launcher-bound (`Window.AgentSend`) | **Send** ("Message opencode…") and the queue; no suggested replies | the runtime's gated send (`RuntimeSpec.GatedSend`, `internal/mirror/runtime.go:104-123`): opencode's API, never keys, only while idle |
| opencode, either condition missing | **disabled**, saying why (`mirror.SendBlocked`, `internal/mirror/opencode_send.go:161`: `SendOff` or `Unbound`) | the hub (`lookupAgent`) and the host's paste guard refuse it again |
| any runtime, host not `-interactive` | disabled, `MirrorReadOnlyNote` | |

**opencode is not view-only today**, which corrects the brief. Both runtimes
have `Caps.Send: true`. An opencode window can be sent to when its host runs
`-opencode-send` and the pane was started by `muster-mirror opencode`, which
every host-spawned opencode window is (#522). A window in neither state shows
the composer disabled with the reason.

The page is **its own document** (`MirrorViewPage` returns a `Doctype`). It is
not inside the app shell, so the shell's chief slide-out (`ui.ChiefPanel()`,
`internal/ui/components.go:405`) is **not on it**. Today, talking to the chief
from an agent's view means leaving it.

### 1.2 What the chief can already read about a session

| tool | what it gives | limits that matter here |
|---|---|---|
| `sessions_overview` (`internal/api/sessions_overview.go`) | each live session: id, kind, name, host, status, focus and its 6 h history, pending attention, and its last messages | **3 messages, 300 runes each** (`overviewTranscriptMessages`, `overviewTranscriptRunes`). A kickoff does not fit. **Claude Code windows only**: `liveSessions` reads `mirrorClaudeEntries` (`internal/api/focus.go:197`), which needs `e.Claude && e.ClaudeSession != ""`, so an opencode window is never listed |
| `focus_search`, `fleet_list`, `fleet_report` | focus titles and summaries, compact | not the transcript |
| `focusSessions.Tail` (`internal/api/focus.go:248`) | **already reads opencode**: `KindMirror` goes through `mirrorFocusEntryFor` → `mirrorFocusEntries`, which keys on `SessionKey()`, including `opencode:<ses_…>` → `mirrorTail` | an internal reader, not a tool. Tool output is dropped (`focusEntryOf`); entries carry `Role`, `Text`, `Cwd`, and **no stable message id** (`focus.Entry`, `internal/focus/tail.go:27`). On the wire, `cctranscript.Event.Line` names prompts only (`internal/cctranscript/event.go:97-101`) |
| `redact.Secrets` | the one redactor, already applied to the overview's tail | |

Stale comment: `mirrorFocusEntries` says *"muster sends only to Claude Code"*
(`internal/api/focus.go:166-168`). That stopped being true when opencode's gated
send shipped. Phase 1 fixes the comment.

What the chief cannot get today:
- which session the owner is looking at;
- a tail long enough to hold a kickoff;
- any opencode session in `sessions_overview`.

### 1.3 Dispatch targets, and which are built

`agent_dispatch` (`internal/api/agent_dispatch.go:155-181`, args
`agentDispatchIn`) takes `target`, `prompt` (≤ 8 KiB), `task_id`, `repo`,
`base`, `model`, `owner_asked`, `worker_pool` and `request_key`, with
`DisallowUnknownFields`.

| target | built |
|---|---|
| `pod/claude-code`, `pod/gateway` + `model` | yes (phase 1, #428) |
| `host/<host>/claude` | yes (phase 3, #513/#516). `toolHostDispatch` (`internal/api/host_dispatch.go:438`): `owner_asked` and `request_key` required, `connectedRepoName` checked, and a held card for a blank host or model |
| `host/<host>/opencode` + OpenRouter `model` | **#522, draft.** The model is checked against the host's own `GET /api/model` under `openrouter`. The flow is worktree → `POST /api/session` → window `muster-mirror opencode -s <id>` → bind → first prompt queued. `spawnPrompt` gains a `runtime` argument. `MUSTER_CHIEF_DISPATCH_MODELS` feeds the model card |

Facts this design relies on:
- **A host spawn always cuts a fresh worktree** from `origin/<base>` (3.3). It
  never reuses the predecessor's checkout.
- **muster can close no window.** No kill message exists (D7). The only
  `kill-window` is the spawn's own undo
  (`internal/mirrorhost/agentspawn.go:995`).
- **The window ↔ dispatch match is already solved.** The `@muster-dispatch`
  tag (`mirror.DispatchOption`) appears in each listing (`Window.Dispatch`,
  `SessionView.Dispatch`), and `reconcileListing`
  (`internal/api/host_dispatch_reconcile.go:124`) moves a row to `running`
  with `tmux_window` and `agent_session`.
- **Each state change of a dispatch row** is broadcast to the owner as
  `agent.changed` with id `host-dispatch-<id>`
  (`internal/api/host_dispatch.go:783`) on the per-owner SSE bus (`GET
  /events`, `internal/api/server.go:641`; `internal/sse`, delivery by
  `OwnerID` only).

### 1.4 The owner-turn gate this design leans on

`ownerTurnFor` (`internal/api/chief_owner_release.go`) proves that the chief's
running turn is a message the owner **typed** into a signed-in chat door.

- `ownerTypedBy` accepts `authz.ViaSession` only.
- The marker is set by `markOwnerTurn` once ccd accepts the text.
- Every other CLI in the pod must be idle, and the call must come over MCP.

It returns the owner's chat message id. That id is what lets a tool read
**what the owner was looking at** when they typed, which is the hinge of
section 4's binding.

`runEnteredTurn` (`internal/api/agents.go:2198`) is the one function that writes
the user row with `TypedBy`. Two tests pin its callers:
- `TestOnlyTheOwnersChatDoorsMarkAnOwnerTurn` pins who passes a real `typedBy`;
- `TestEveryChatWriterIsClassified` pins every chat-row writer.

Both ledgers grow by exactly one in phase 1.

---

## 2. UX

### 2.1 The composer toggle

This is one switch, the first control of the composer's input row, left of
the paperclip: a 44px icon button (a bot for the agent, a crown for the
chief) that is a `role="switch"` checkbox. It is drawn only when the owner has
a chief that is on (`chiefpref`). (Phase 1 first shipped it as a two-segment
"`<runtime>` | Chief" switch; see "Phase 1 as built".)

```
[♛][📎]  Ask the chief about this session…                        [➤]
```

- **To the agent** (default): today's composer, unchanged.
- **To the chief, about this agent:**
  - the placeholder reads "Ask the chief about this session…";
  - the queue caret, the suggested replies and the `/` menu are hidden;
  - the mic and the paperclip stay. Dictation works, and an upload's URL goes
    in the text as it does in the chief's chat;
  - Send posts to `POST /mirror/view/chief`.
- **It works when the agent composer is disabled.** That covers an opencode
  window that is not launcher-bound, a read-only host, and an offline host
  whose transcript is still cached. The chief mode is a write to the chief's
  chat, not to the pane. So it needs neither `-interactive` nor step-up, the
  same as the chief's own chat today. A *succession* still needs a
  spawn-capable host, and the dispatch refuses otherwise.
- **The choice persists per window** in `localStorage`, under the draft key's
  scheme (`MirrorDraftKey(..., "chief", ...)`), and each mode keeps its own
  draft. A reload lands in the mode last used, and the default is the agent.
- **No script:** it is a real `<form>`. The switch is a checkbox the
  stylesheet follows: it shows the chief's Send, whose `formaction` is the
  chief's route, so a browser submit still reaches the right door — with
  script disabled and when the chief script throws (the composer's "real
  form" rule, `mirror_compose.go`).

### 2.2 What is attached

The **browser sends only the address** (`host`, `session`, `window`, as it
does today), the text and a nonce. Everything else is **resolved on the
server** from the hub's state through `mirrorEntry`, which is owner-scoped. The
page cannot claim a subject the owner does not have.

| field | source |
|---|---|
| `session_key` | `SessionView.SessionKey()`: the Claude UUID, or `opencode:<ses_…>` |
| `runtime`, `host`, `tmux` (session:window) | the entry |
| `dispatch` | `SessionView.Dispatch` (0 for an owner-opened window) |
| `repo` | the `host_dispatches` row's full `owner/name` when `dispatch > 0`. Otherwise the short `SessionView.Repo`, resolved to a connected repository only when exactly one matches |
| `base`, `branch`, `model` | the dispatch row, when there is one |
| `cwd` | `focus` `LatestCwd` |

The **transcript tail is not attached to the message.** The chief fetches it
with `session_context` only when it needs it. That keeps every turn small,
which matters for a Flash-class model, and the tail is read fresh, not as a
snapshot taken when the message was typed.

**What the chief's turn says.** The turn text is rendered at delivery as a
muster header plus the owner's verbatim words:

```
[muster] Your owner is viewing an opencode session and wrote this about it.
subject: opencode:ses_4f2a…   host: <host>   window: agents:@7   repo: <owner>/<repo>
dispatch: 812 (yours, started 2h ago)   status: idle
Read it with session_context {subject}. To start a session that continues it,
call agent_dispatch with successor_of: <subject> and succession: replace or spinoff.
---
copy the kickoff out of this session's tail and open a new opencode session and run the kickoff
```

`chat_messages.content` stores only the owner's line. The header is rendered
from `about`, so the owner's words, quoted later as covering words
(`chief_chat_quotes.go`), are exactly what they typed.

### 2.3 Where the chief's replies show: the chief strip

This is a strip between the transcript and the composer, drawn only in chief
mode. Collapsed, it shows the last reply's first line and a state chip.
Expanded, it is at most 45 % of the visible viewport on a phone (390×844, the
size the browser suite measures) and is scrollable.

- **It re-mounts the existing chat, never a second one:** `agentChatPane` and
  `agentChatScript`, the same `#chat-log` partial fed on `sse:chat.reply`. This
  is the rule the slide-out already follows
  (`internal/ui/chief_panel.go:44-48`).
- **The thread is a chief chat session per lineage root** (D6), keyed
  `about:<root session key>`. An exchange about a session therefore stays out
  of the chief's default conversation, and it **follows the successor**: the
  new session's view opens the same thread (2.5).
  **UNVERIFIED**, measured in phase 0: a non-default session with no runtime
  runs in the chief's original CLI (chief.md, "Sessions on more than one
  runtime"). Whether ccd keeps a separate conversation per session key there,
  and whether `ownerTurnFor` passes for a turn in a non-default session, are
  both to be measured. If either fails, the fallback is the chief's default
  session, with the strip filtered to rows whose `about` names this lineage.
- The strip's **live dispatch card** is the existing one: `/ui/dispatch-card?host_dispatch=<id>`
  (`Server.hostDispatchCardSource`). Its states are "Starting on `<host>`…",
  then running, then refused with its sentence. It is the same card the
  chief's chat shows, so the two cannot disagree.

### 2.4 What the operator sees while the chief works

| moment | strip | rest of the view |
|---|---|---|
| sent | the owner's line and "Chief is working…" (the chat's own queued and working states, including "queued: still finishing its previous turn") | unchanged; the agent's transcript keeps streaming |
| chief reading | tool chips (`session_context`) as the chat already draws tool steps | — |
| held card (no host, no model) | the `ask_owner` card **inline in the strip** (today's chat card) and on the Attention view and the phone | — |
| dispatch accepted | the live card: "Starting `opencode · <model>` on `<host>`…", plus the **kickoff exactly as sent**: muster's excerpt, collapsible, with its byte count | a slim banner: "A successor is starting · Stay here" |
| successor `running` | the card links to it | **the view follows** (2.5) |
| refused or failed | the card's sentence (`spawnRefusalSentence`) and the chief's reply | no jump; the composer stays in chief mode with the draft cleared |

### 2.5 "The view follows the successor"

1. The view page opens `EventSource('/events')` (the pattern in
   `internal/ui/attention.go:151`) **only while a succession it started, or
   one naming its session, is open**. The page learns that state from a
   `data-lineage` attribute drawn by the server, or from the dispatch reply
   in the strip.
2. On `lineage.changed` with an id it knows, it fetches
   `GET /mirror/view/successor?host=&session=&window=`, which answers JSON:
   `{state: starting|running|failed, mode, successor: {view_path, title,
   host, runtime}}`.
3. On `running`, the banner reads "Opening the new session…" for 1.5 s, with
   **Stay here**, and then the page navigates to the successor's
   `MirrorRef.ViewPath()`. The jump uses `location.assign`, so Back returns to
   the predecessor. If the view is framed (the Terminal ↔ Rendered swap,
   `internal/ui/view_swap.go`), the **top** window navigates, as the swap
   already does for links that leave the window.
4. **A missed event** (phone asleep, SSE dropped): on `visibilitychange` back
   to visible, and on SSE reconnect, the page fetches the successor route
   once. A view opened later on the predecessor shows a banner instead of
   jumping ("Continued in `<title>` → open" for replace, "Spun off `<title>`
   → open" for spin-off). An automatic jump happens **only in the view that
   asked**, at most once per lineage row: the page records the row id in
   `sessionStorage` before navigating.
5. **Phone, and not looking:** if no open view acknowledged the jump within
   30 s, the owner gets one push ("`<title>` started on `<host>`") that
   deep-links to the successor's view. This uses the existing push path
   (`internal/push`); the Android shell opens it like any push deep link
   (android.md).
6. **The successor's view** shows a header line, "Continues `<old title>` ·
   replace/spin-off · open previous", and its chief strip opens the same
   thread.

**Desktop and phone are the same mechanism.** It is a page-level navigation in
one document, with no shell state to reconcile. Pinned composer, switcher and
draft keys are per window and come with the successor's own page load.

### 2.6 Failure states

| what goes wrong | the owner sees |
|---|---|
| no chief, or the chief is off | no toggle, or a disabled "Chief is off · turn it on" link to Account → Chief |
| `agent_dispatch` not offered (no task routing, not in `MUSTER_CHIEF_DISPATCH_OWNERS`, compose install) | the chief answers questions; a succession is refused with today's sentence, said in the strip |
| subject gone (window closed, host offline) before the dispatch | `session_context` answers "not visible now"; a `prompt_from` dispatch is refused ("the session's transcript can no longer be read") |
| kickoff block not found, or too large | refused with its own sentence (4.3). The chief says so; nothing starts |
| host refuses (`bad_model`, `no_repo`, `unavailable`, …) | the live card's sentence, as today |
| the reply never comes | the card's "muster has not seen the window yet"; the row is settled by the 180 s countdown (3.7). No jump |
| the window is up but the first prompt was refused (opencode) | `failed, prompt not accepted`; the window stays and gets the orphan notice (3.11). No jump, because the lineage follows the row's status |
| replace, but the owner keeps typing into the old one | nothing stops them. The old view's banner says it was superseded, and its composer still works |

---

## 3. Data model

### 3.1 `chat_messages.about` (migration `00NN_chat_about`)

> **As built (phase 1, migration 0098; round 0 D1):** `about` is `text null`
> holding the session **key only**. The rest of the subject builds the
> chief's header in memory and is not stored. See
> [Phase 1 as built](#phase-1-as-built).

`about jsonb null`, written **only** by `POST /mirror/view/chief`, holding
`{session_key, runtime, host, tmux_session, tmux_window, dispatch, repo, base,
branch, model}` as resolved in 2.2. Null on every other row.
`TestEveryChatWriterIsClassified` gains the new door as the one writer of a
non-null `about`.

### 3.2 `session_successions` (same migration), under RLS on `owner_id`

| column | type | |
|---|---|---|
| `id` | bigserial | the id in `lineage.changed` |
| `owner_id` | bigint | RLS |
| `root_key` | text | the lineage root's session key (the thread key, 2.3): the predecessor's own `root_key`, or its `session_key` if it has none |
| `subject_key` | text | the predecessor's session key |
| `subject_host`, `subject_tmux_session`, `subject_tmux_window` | text | where it was when the owner asked |
| `subject_dispatch` | bigint null | its `host_dispatches` id, when the chief started it |
| `mode` | text | `replace` or `spinoff` (CHECK) |
| `requested_by` | bigint | the user, from the owner's turn |
| `owner_message` | bigint | the chat row `ownerTurnFor` returned, which carries `about` |
| `chief_agent`, `chief_session` | bigint | the chief and the thread |
| `request_key` | text | the dispatch's; `UNIQUE (owner_id, request_key)` |
| `host_dispatch_id` | bigint | `REFERENCES host_dispatches`: the successor |
| `excerpt_sha256` | text null | the copied kickoff's hash (4.3) |
| `subject_state` | text | `active` or `superseded` (replace only, set when the successor runs) |
| `notified_at` | timestamptz null | the push in 2.5 (5) went out |
| `created_at` | timestamptz | |

**No status column.** The successor's state **is** its `host_dispatches.status`
(`pending`, `running` or `ended`, with `refusal`). A second status would be a
second source of truth for "did it start", and the two could drift.

**The writer ledger** follows the `host_dispatches` pattern (3.7: an asserted
ledger over the Go source, failing if the set grows *or* shrinks):

1. **Insert:** inside `agent_dispatch`'s `onReserved` hook, in the **same
   transaction** as the `pending` `host_dispatches` insert, and only when
   `successor_of` is set. A refused reservation writes neither row.
2. **`MarkSuperseded`:** sets `subject_state = 'superseded'` where `mode =
   'replace'`. Its only caller is the reconciler, in the same step that moves
   the successor's row to `running`.
3. **`SetSuccessionNotified`:** its only caller is the 30 s push timer.

### 3.3 Reconciliation with mirroring

Nothing new is needed to find the window. The successor is a host dispatch:
- the host tags its window `@muster-dispatch=<id>`;
- the listing carries it;
- `reconcileListing` moves the row to `running` with `tmux_window` and
  `agent_session`.

The successor's view path is therefore `MirrorRef{Host: row.host, Session:
"agents", Window: row.tmux_window}.ViewPath()`, a pure function of the row. It
is never a guess from titles or transcripts. The session key (`Window.SessionKey`)
follows from `agent_session`, so focus, routes and asks that key by session
link to the lineage the moment the reply lands. This is the property 3.7's
live card already relies on.

**What emits `lineage.changed`:** `endHostDispatch` and the reconciler's
`running` transition already broadcast `agent.changed` /
`host-dispatch-<id>`. Both also call `broadcastTo(owner, "lineage.changed",
"succession-<id>")` when a `session_successions` row names that dispatch.
That is one lookup by the indexed `host_dispatch_id`. The event names the
owner and carries no text, so it adds no data path across users
(`internal/sse` drops a zero owner).

**Lookups the view makes:**
- *"my successor"*: by `subject_key` = the entry's `SessionKey()`, newest first;
- *"my predecessor"*: by the entry's `Dispatch` →
  `session_successions.host_dispatch_id`.

Both are indexed, and both are read as the owner.

**Live slots.** A successor counts toward the chief's live and daily caps like
any dispatch. A *replaced* predecessor that is itself a host dispatch **still
holds its slot** until its window ends. Without phase 3, a replace therefore
costs one extra slot until the owner closes the old window. The strip says so
when the caps are tight (the dispatch answer already carries the budget).

---

## 4. Chief tools

These are designed for a weaker model. Every tool takes explicit ids, there is
no free-form instruction channel, every failure is its own fixed sentence, and
a succession needs **two calls** (read, then dispatch).

### 4.1 `session_context` (new; chief-only; read-only; no approval)

```
session_context {subject: string, messages?: int (default 8, max 20)}
→ {
  subject, runtime, host, tmux: "agents:@7", status, cwd,
  repo: "<owner>/<repo>" | null, base, branch, model, dispatch: 812 | 0,
  suggested_target: "host/<host>/opencode", suggested_model: "<id>" | null,
  suggested_base: "<branch>" | null,
  tail: [ {ref: "m:3f9c1a2b7e04", role: "assistant", text: "…", bytes: 2311,
           blocks: [ {index: 0, lang: "text", first_line: "Kickoff: …", bytes: 1804} ] } ],
  truncated: bool
}
```

**Who may call it.** It is offered under session focus, like
`sessions_overview`. `subject` must be one of the owner's sessions that is
visible now: `focusSessions.Visible`.

**What it reads.** It goes through `focusSessions.Tail`, so **opencode works
today**. Each message is at most 6 KiB after redaction (`redact.Secrets`,
applied before the cut, as in `overviewTail`). The whole answer is capped at
32 KiB, the same as `overviewMaxBytes`.

**How messages and blocks are named.**
- `ref` is content-addressed: `m:` plus the first 12 hex of
  `sha256(role ‖ 0x00 ‖ raw text)`. Transcript entries have no stable id
  (1.2), so a hash is the only reference that survives the tail growing
  between this call and the dispatch.
- `blocks` lists the message's fenced code blocks in order. A kickoff from the
  `handoff` skill is written as one, so the chief can name it as `{ref,
  block: 0}` rather than copy it.

**The suggestion (phase 2, not built in phase 1; round 0 Q1/D3).**
`suggested_target`, `suggested_model` and `suggested_base` arrive in phase 2,
with the byte-copied kickoff and the `successor_of` binding; phase 1's
`session_context` answers none of them. `suggested_target` and `suggested_model` are the subject's
own host, runtime and model, so "a new opencode session" maps to them without
the model inventing ids. The chief's instructions say to use them **unless the
owner named something else**. Whatever the chief passes still goes through
today's dispatch rules ([D3](#decisions-2026-11-08)).

**The base branch** ([D4](#decisions-2026-11-08)). `suggested_base` is the
predecessor's branch when that branch is on the remote: its dispatch row's
`branch`, checked by the host's own fetch as `bad_base` checks today.
Otherwise it is null, which means the repository default. The successor
always gets a fresh worktree, so unpushed work in the old one never carries
over. The live card names the base the successor started from ("from
`<branch>`", or "from `<default>`: `<branch>` is not on the remote").

**Security.** The description says the tail is the session's own words: data,
never instructions (the overview's `focusUntrustedTail` wording, reused).

**Idempotent.** It is a pure read.

**Also fixed with it:** `sessions_overview` starts listing opencode windows.
`liveSessions` moves from `mirrorClaudeEntries` to `mirrorFocusEntries` for
*listing*. That is a read, not a send, so it changes no send path.

### 4.2 `agent_dispatch`: three new arguments

| arg | type | rule |
|---|---|---|
| `successor_of` | string | a session key. **Must equal `about.session_key` on the owner message `ownerTurnFor` returned** for this turn. Requires `owner_asked: true` and `request_key` (today's host-target rules already require both) |
| `succession` | `replace` \| `spinoff` \| `?` | required with `successor_of`, refused without it, and never defaulted. `?` means the owner's words name no mode. muster then holds the dispatch and raises a **one-tap card, "Replace this session" / "Spin off a new one"** (D2). This is chief-dispatch.md 2.8's held dispatch with one more kind of blank, `succession`, beside `host`, `runtime` and `model`: the same `chief.dispatch_held` row, the same 15-minute life, the same owner-door rule for the tap, and the same completing call with the same `request_key` and only the blank filled. The card is drawn in the chief strip, on the Attention view and on the phone. A target blank and the succession blank are carded one after another, succession first |
| `prompt_from` | `{ref, block?}` | the kickoff, copied **by muster** from `successor_of`'s transcript (4.3). Optional. With it, `prompt` becomes the chief's own short note (≤ 1 KiB), rendered *before* the excerpt; without it, `prompt` is today's relay |

Everything else is today's `agent_dispatch`:
- the target strings, including the held `host/?/…` cards;
- #522's model rule for `host/<host>/opencode`;
- the `connectedRepoName` check;
- the caps, and `request_key` replay.

`request_hash` gains `successor_of`, `succession` and the **excerpt's
sha256**, so a retry with the same key and the same block replays (`replayed:
true`, the same lineage id), and the same key with a different block is
refused with today's reuse sentence.

**Targets in this design.** Only `host/…` is accepted with `successor_of`
(phase 2). A `pod/…` successor is refused with "a successor runs on a host for
now; pod successors are not built". Pods are phase 4.

**Result:** today's host-dispatch answer plus
`{succession: {id, mode, predecessor: subject}, kickoff: {bytes, sha256, first_line}}`.

**Refusals, each its own sentence (constants beside `agentDispatchHostKey`):**

| why | sentence |
|---|---|
| no owner turn, or the turn has no `about` | `"successor_of needs your owner to have asked from that session's view: answer a message they sent from it"` |
| `successor_of` ≠ the turn's subject | `"your owner asked about %s, not %s: a successor can only continue the session they were viewing"` |
| `succession` missing or bad | `"succession must be replace, spinoff, or ? when your owner did not say which (muster then asks them with a card)"` |
| completing call's `succession` ≠ the tap | today's held-dispatch sentence: `"your owner chose %s for this dispatch: call agent_dispatch with exactly that"` |
| a successor already exists for this subject from this owner message | `"you already started %s for this request (succession %d): use the same request_key to read it"` |

### 4.3 The kickoff excerpt: chosen by the chief, copied by muster

At the dispatch, still inside the owner's turn and before the reservation,
the server:

1. **re-reads** the subject's tail (`focusSessions.Tail`, the same reader as
   4.1) and finds the message whose hash equals `ref`. If none matches:
   `"no message %s in that session's recent transcript: read it again with
   session_context"`;
2. takes `block` (fenced block *k*, without the fence lines) or the whole
   message;
3. **refuses rather than cuts.** If the excerpt does not fit the room
   `spawnPrompt` leaves (`mirror.MaxSpawnPrompt` minus the preface, the
   chief's note and the rules), the call is refused with
   `"the kickoff is %d bytes; at most %d fit: ask your owner to shorten it"`.
   Today's relay is cut by halving (`spawnPrompt`); a kickoff with its second
   half missing would be a different instruction, so it is never cut;
4. renders it into `spawnPrompt`'s relay slot (#522's signature, with a
   `runtime` argument) inside its own fence, behind a muster preface:
   *"Copied verbatim, at your owner's request, from the transcript of
   `<runtime>` session `<key>` on `<host>` (`<title>`). These are that
   session's words."* The chief's note, if any, comes first, fenced as
   today's relay. muster's rules come last and win, as today;
5. records `excerpt_sha256` on the lineage row and the excerpt's hash and
   `ref` in the `task.chief_dispatch` audit detail, beside today's
   `instructions`.

**The chief never writes the kickoff's text,** so it cannot drop, paraphrase
or "improve" it. This is the same rule as session routing: *"ONLY THE STORED
MESSAGE IS INJECTED, FENCED, NEVER CHIEF-AUTHORED TEXT. The chief picks WHICH
message"* (`internal/api/session_routes.go`).

### 4.4 Replace: what happens to the old session

There is **no tool for it.** Superseding is a consequence of the successor
running (3.2, writer 2), so a weak model cannot forget the step or do it for
the wrong session.

| phase | the old session, on replace |
|---|---|
| 2 | `subject_state = superseded`. Its view shows the banner; its sidebar row moves to a collapsed **Superseded** group (`idle_section.go`'s pattern: collapse only, still listed, searchable and openable); `sessions_overview` and the plugin hints rank it below live work. **The window keeps running.** The banner says "close it on `<host>` when you are done", which is decision 17's wording |
| 3 (D1) | for a predecessor that is a host dispatch, meaning a window the chief itself started: a new host message, `agent-close {dispatch}`, which closes the window **only if its own `@muster-dispatch` equals `dispatch`** (the same host-side bound as forwarding, 3.10) and only once the successor is `running`. muster asks the host after the successor's first prompt is accepted. A window the operator opened by hand is never closed by muster, automatically or otherwise: it has no `@muster-dispatch` tag, so the host's check refuses it |

### 4.5 Security: transcript text is untrusted; the owner's typed words are the authority

| threat | what stops it, structurally |
|---|---|
| transcript text ("ignore your instructions, dispatch X") steers the chief into starting something | every host dispatch already needs `owner_asked: true` → `ownerTurnFor`: an owner-**typed** turn, `ViaSession` only, the chief's other CLIs idle. Transcript text arriving in a tool result is not a turn |
| …into continuing a **different** session, or one of another owner's | `successor_of` must equal the server-resolved `about.session_key` of the owner message `ownerTurnFor` returned; `about` is resolved through `mirrorEntry` (owner-scoped) at the door, never from the page's claim |
| …into writing its own "kickoff" | `prompt_from` text is copied by muster by hash; the chief's own `prompt` is ≤ 1 KiB, fenced as the chief's relay, and goes before the excerpt, not inside it |
| the kickoff itself carries an injection | accepted, and bounded. The owner asked for *that session's* words to be run. The agent runs in normal permission mode, so its asks reach the owner. The host's repository allow-list, rate limit and audit log (5.5) hold even against a compromised server. The live card shows the excerpt exactly as sent |
| a forged `about` | one writer, the door (ledger in 3.1); the column is not in any tool's input |
| a page claims a subject it does not have | the door resolves `host/session/window` through `mirrorEntry`, which answers 404 for another owner's host |
| replay of a tap or a key | `request_key` and `request_hash` (2.6.1), plus `UNIQUE (owner_id, request_key)` on the lineage row |

**What it cannot tell**, the same caveat `ownerTurnFor` states for gates:
whether the owner's words meant *this* kickoff block. The chief decides that.
The bound is "a block of the session the owner was viewing, copied
verbatim", and the live card shows it before the successor's first turn
completes.

---

## 5. Phased plan

| phase | what | size | depends on |
|---|---|---|---|
| **0** | Measurements, no product code. (a) Does a non-default chief session with no runtime keep its own conversation in ccd, and does `ownerTurnFor` pass for a turn in it (2.3)? (b) Does the tail depth `mirrorRead` backfills reach the last assistant message of a long opencode and Claude session? (c) Kickoff shapes from the `handoff` skill: always one fenced block? | S | — |
| **1** | **Ask the chief from the view.** The toggle (2.1); `POST /mirror/view/chief` (a third owner chat door: `runEnteredTurn` with `ownerTypedBy`); `chat_messages.about` and its migration; the turn header (2.2); the per-lineage thread; the chief strip re-mounting `agentChatPane` (2.3); `session_context` (4.1); `sessions_overview` lists opencode; the stale comment fixed. No dispatch changes | M | main |
| **2** | **Succession.** `successor_of`, `succession` (with the held `?` and its one-tap card, D2) and `prompt_from` on `agent_dispatch` (4.2, 4.3); the inherited host, runtime and model and `suggested_base` (D3, D4); `session_successions` and its writer ledger (3.2); `lineage.changed`; `GET /mirror/view/successor`; the view-follow (2.5); the banners and the Superseded group (4.4, phase 2 row); the 30 s push; the chief's instructions (both runtimes, `agentspec`) and the tool description pins. **Claude successors work on main; opencode successors need #522** | L | 1; **#522** for `host/<host>/opencode` |
| **3** | **Replace closes the old window.** `agent-close` on the wire and the host (tag-bound, successor-running-bound, rate-limited like forwards); D7 and decision 17 reopened for this one case, windows the chief started (D1) | M | 2 |
| **4** | Later: pod successors (a dispatched agent's chat or Terminal view as subject; successor `/agents/{name}`) | M | 2 |

### Test strategy

- **Go, PG suite (`internal/api`):**
  - the binding: `successor_of` equal to, different from, or absent from the
    turn's `about`, each refused with its own sentence;
  - a non-owner door, such as `chief ask` over the hook token, never carries
    `about`;
  - `prompt_from`: hash found, not found, block out of range, too large
    (refused, not cut). The kickoff that reaches `AgentSpawnReq.Prompt` is
    byte-identical to the transcript block (a pinned literal fixture, not one
    derived from the code under test);
  - `request_key` replay and reuse, with the excerpt's hash in the hash;
  - replace vs spin-off `subject_state`;
  - `lineage.changed` goes to the owner only, and nothing goes for a dispatch
    that is not a succession.
- **Ledgers:**
  - `TestOnlyTheOwnersChatDoorsMarkAnOwnerTurn` gains exactly the new door;
  - `TestEveryChatWriterIsClassified` gains `about`'s one writer;
  - a new `session_successions` writer ledger fails when the set grows or
    shrinks;
  - `TestThePageAndTheHubAgreeOnWhatCanBeSent` is unchanged, because chief
    mode is not a pane send.
- **Mutation checks:**
  - drop the `successor_of` comparison, and the binding test must fail with
    its sentence;
  - read `about` from the request instead of the owner message;
  - cut instead of refusing an oversized excerpt;
  - emit `lineage.changed` before `running`;
  - a positive control in each batch.
- **Host (`-tags tmuxit`):** phase 2 adds no host code. Phase 3's
  `agent-close`: a tag mismatch is refused with no `kill-window`; a match is
  closed once; the positive control is one close.
- **e2e (Playwright, phone 390×844 and desktop):** this is the click path the
  rules require, and it is not optional.
  - The fake mirror host (`cmd/muster-mirror/e2emirror.go`) learns
    `agent-spawn`: it answers `agent-spawned` and lists a new window tagged
    with the dispatch.
  - The chief is a scripted chief: a new fixture that makes the chief's MCP
    calls (`session_context`, then `agent_dispatch`) over the connect port
    while the owner's turn is marked. No e2e chief that calls tools exists
    today; `e2e/ccfix` only seeds rows. Without that fixture this spec
    cannot be written, so it is part of phase 2's size.
  - The spec:
    1. open an opencode window's rendered view;
    2. flip the toggle and type the request;
    3. see the strip go working → card → "Opening the new session…";
    4. **assert `location` is the successor's view** without any click, that
       its header says "Continues …", and that Back returns to the
       predecessor with the Superseded banner (replace) or none (spin-off);
    5. a variant puts the page in the background before `running` and asserts
       the catch-up fetch on `visibilitychange`.
  - Neither phase 3 nor #522 built an e2e spec for host dispatch. This is the
    first, and it covers both runtimes.
- **Live acceptance** (the operator's host, after #522's host changes): a real
  opencode session with a real handoff, asked from the phone. The record notes
  the time from send to jump, and the successor's first message compared with
  the kickoff block byte for byte.

---

## Decisions (2026-11-08)

The operator answered the first draft's six open questions. These are binding,
and this page does not reopen them.

1. **D1: what replace does to the old session.**
   - **Phase 2:** replace marks the old session superseded (the banner, and its
     sidebar row in the collapsed Superseded group), and its window keeps
     running.
   - **Phase 3:** muster may close the old window, but **only a window the
     chief itself started**. That means a host dispatch whose own
     `@muster-dispatch` tag the host checks (4.4).
   - **Never:** muster never closes, automatically or otherwise, a window the
     operator opened by hand.
2. **D2: replace or spin-off, unspecified.** When the owner's words name
   neither, muster asks with a **one-tap card**, "Replace this session" or
   "Spin off a new one". There is no default. The mechanism is the held
   dispatch with a `succession` blank (4.2).
3. **D3: setup.** The successor inherits the viewed session's host, runtime
   and model (`suggested_target`, `suggested_model`, 4.1), under the existing
   dispatch rules: the same targets, model checks, held cards, caps and
   approvals, and no new configuration. **The operator's words override**
   anything inherited.
4. **D4: branch.**
   - The successor starts from the old session's branch when that branch is on
     the remote; otherwise from the repository default.
   - An owner-opened window has no dispatch row to name its branch, so it
     takes the default.
   - The live card says which base was used.
   - Unpushed work in the old worktree never carries over, because a successor
     always gets a fresh worktree (`suggested_base`, 4.1).
5. **D5: no extra confirm tap.** There is no "Start with this kickoff" card.
   The owner's typed message is the authority, as for every dispatch today,
   and the chief strip shows the exact kickoff that was sent (2.4).
6. **D6: thread.** There is one chief thread per succession chain, keyed by
   the lineage root (2.3), **if phase 0 confirms it works**: a non-default
   chief session keeps its own conversation, and `ownerTurnFor` passes in it.
   If it does not, the fallback in 2.3 applies: the chief's default session,
   with the strip filtered to this lineage's `about` rows.

## Decisions (2026-11-09)

7. **D7: a view line authorises nothing by itself; the owner's tap on a
   card does.** The chief proposes, and the owner's one tap on the card it
   raises (on the view, the Attention view and the phone) is the
   authorisation. Transcript text can at most cause a proposal. This
   replaces D5's "no confirm tap" and 4.2's binding to `ownerTurnFor`.
8. **D8: one primitive.** The confirm card is a generic chief proposal with
   a kind: one card, one tap path, one binding rule. Succession is its first
   kind and ending a session its second.
9. **D9: ending a session on the owner's explicit request.** On the owner's
   explicit request, and only then, the chief may propose ending ANY of the
   owner's sessions: a mirrored window on their hosts (Claude Code or
   opencode), windows the owner opened by hand included, or any pod agent of
   theirs, whoever started it. Never the chief itself, and never an
   officer's instance (both are delete-protected).
   - **Binding:** the session the owner's view line is about, or one the
     owner named in a line typed in the chief's own chat.
   - **Authorisation:** the tap. Nothing ends without it.
   - **How:** gracefully where that is safe, then closed. A Claude Code
     agent at an idle prompt with an empty input box is asked to exit
     (`/exit`, through the Claude prompt guard), and its window is closed
     once it exits, or after about 30 s whatever it does. One waiting on the
     owner (a permission prompt, a picker, a dialog) or holding a draft is
     typed nothing, and its window is closed at once. An opencode window is
     closed at once (#549 round 1). A pod agent goes through muster's own
     stop and destroy.
   - **What it changes:** this supersedes D1's "never closes a window the
     operator opened by hand" **for the explicit-request case only**. An
     automatic close on replace (phase 3) stays limited to windows the chief
     itself started.

---

## Phase 0 results (2026-11-08)

Measured from the code at `origin/main` `1b029ed9`, checked read-only against a
running deployment, and against a sample of real Claude Code and opencode
transcripts. Each answer states its conclusion; this page carries no
deployment's identifiers or counts.

### (a) A thread per succession chain: **no — the fallback applies (D6)**

**From code.** A chief chat session with no runtime (`chat_sessions.runtime =
''`) is routed as *legacy*: `withRoute` (`internal/api/session_runtime.go`)
returns the context unchanged for it, so the turn carries no
`X-Muster-CC-Session` header, and ccd runs every such turn in its **one
primary CLI** (`cmd/ccd/lanes.go`: "one WITHOUT it is the primary's"). A
non-default session with no runtime is therefore a second muster transcript
over the **same** ccd conversation: the chief would see the view's exchange
mixed into its default conversation anyway, while muster showed it as a thread
of its own. That is the opposite of what D6 asks for.

Only a `claude-code`-runtime session (a ccd *lane*) has a conversation of its
own. It is not a substitute here:
- it is a Claude Code CLI of its own (hundreds of MiB, under
  `CCD_MAX_SESSIONS`, default 3), started with none of the chief's context;
- `ownerTurnFor` passes for a lane turn only while the primary and every other
  CLI are idle (`onlyTheOwnersLaneWorks`). By code it would pass; it has
  **never been measured live**.

`ownerTurnFor` itself reads no session: for a turn in a no-runtime session the
runtime's acceptance names lane `""` (the primary), exactly as for the default
session. By code it passes; it is **UNVERIFIED live**, and moot given the
above.

**In a running deployment (read-only):** the chief's sessions all have no
runtime, so they all share the chief's default conversation. No lane session
has been opened, and no owner turn has ever been marked in a lane.

**So phase 1 builds the fallback:** the view's line goes to the chief's
**default** session, and the strip shows that session's rows whose `about`
names the viewed session, with the replies that follow each one.
**This contradicts 2.3 as written** (a session keyed `about:<root key>`), which
is not built. A per-lineage thread would need a lane session per chain, which
is a decision of its own (memory, the CLI cap, model choice).

### (b) How much transcript the tail reaches: **the newest end, always**

**From code.**

| runtime | backfill (`mirrorRead`) | per event |
|---|---|---|
| Claude Code | the last `BackfillWindow` (4 MiB) of the transcript file, at most `MaxBackfillEvents` (300) events (`internal/mirror/wire.go`, `internal/mirrorhost/transcript.go`) | text cut at `cctranscript.MaxText` (32 KiB) |
| opencode | the newest `openCodeBackfill` (200) messages, then the same 300-event cap (`internal/mirrorhost/opencode_stream.go`) | the same |

Both read from the **end**, so a session's last assistant message is always
in the tail, however long the session is. `session_context` cuts each message
at 6 KiB after redaction.

⚠ **A risk measured from code, not seen live.** `mirrorRead` ends a read on a
750 ms gap between batches (`focusTailQuiet`), or at 15 s, before the prompt
index arrives. Batches arrive oldest first, so a slow host cuts the **newest**
end, which is where a kickoff is. A partial read returns what it got.

**In a running deployment (read-only):** the focus refresher already reads
opencode sessions' tails through `focusSessions.Tail`, the reader
`session_context` uses.

**A second finding, a contradiction with 2.2:** opencode tails carry no
working directory (opencode's prompt events record none). So
`focus.LatestCwd`, the `cwd` source in 2.2's table, is `""` for every
opencode session, and `session_context` answers `cwd: ""` for one; only Claude
Code sessions have a `cwd`.

### (c) What a kickoff block looks like: **one untagged fenced block, nearly always**

Measured over a sample of real assistant messages carrying the handoff
skill's kickoff line (`/resume — continue`), from both runtimes:

- nearly all kickoffs are **one fenced block**, almost always **untagged**,
  and almost always the message's **last** block;
- a few are **not fenced at all** (written after a `---` rule), and those
  occur in real kickoffs, not only in mentions of one;
- nearly all fit in 1 KiB.

The handoff skill's own tool prints the block fenced (its `kickoff_block`
function). So `{ref, block: 0}` names the kickoff in almost every case.

**Phase 2 must keep `block` optional** (the whole message), as 4.2 already
has it, because a few real kickoffs are not fenced at all.
`internal/mdfence` (phase 1) lists blocks by CommonMark's fence rules: a
backtick or tilde fence indented at most three spaces, closed by the same
character at least as long.

## Phase 1 as built

Phase 1 follows section 5's phase 1, with the phase 0 adjustments above and
the operator's round 0 decisions on PR #537:

- **`POST /mirror/view/chief`** (`internal/api/view_chief.go`). It is the
  third owner chat door. It reads the address, the text and the nonce, and
  nothing else.
  - It refuses a principal that is not a signed-in browser session (403)
    before anything is stored.
  - It resolves the subject through `mirrorEntry`, which is owner-scoped
    (`viewChiefSubject` → `sessionSubject`).
  - It runs the turn through `startAgentTurn` → `runEnteredTurn` with
    `ownerTypedBy(ctx)`, on the chief's **default** session. The line is
    recorded as typed (`typed_by`).
  - **No owner release from a view** (round 1): a line with a subject sets no
    owner-turn marker (`ownerTurnTyper`), so `ownerTurnFor` — `owner_asked`
    on `agent_dispatch` and on a gate release — refuses while its turn runs.
    ⚠ This changes the summary's and 4.2's premise for phase 2, which bound
    `successor_of` to the message `ownerTurnFor` returns: phase 2 needs its
    own decision on how a view line authorises a succession.
  - **It answers nothing** (round 1): no `askerAddressed`, so the chief's
    open asks stay open, settled only from its own chat.
  - It has the same wrappers as `POST /agents/{name}/send`: a session, the
    gateway wired, same-origin.
  - It works when the agent composer is disabled.
- **`chat_messages.about`**, migration **0098**: nullable `TEXT` holding the
  viewed session's **key only** (round 0, D1), written only through
  `runEnteredTurn` from this door. Its rollback is in the file's header.
  - `content` holds only the owner's words.
  - The runtime gets them behind muster's header (`viewChiefTurnText`), built
    from the view as it is when the line is sent and never stored. The phase 1
    header leaves out the `agent_dispatch … successor_of` sentence, which is
    phase 2's.
  - This changes 3.1, which stored the whole resolved subject. Phase 2's
    binding needs only the key (`successor_of` must equal it).
- **The ledgers.**
  - `TestOnlyTheOwnersChatDoorsMarkAnOwnerTurn` grows by exactly the new door.
  - `TestOnlyTheViewDoorBindsASubject` is the new writer ledger for `about`.
    It sits beside `TestEveryChatWriterIsClassified`, whose `typed_by` set is
    unchanged.
- **The composer switch** (`internal/ui/view_chief.go`): ONE always-present
  control, the input row's first, left of the paperclip — a checkbox
  (`#mv-to-chief`, `role="switch"`, checked = chief mode) inside a label
  (`#mv-to`) drawn as a 44px icon button showing the current target's glyph (a
  bot for the agent, a crown for the chief). Its accessible name is fixed
  ("Send to the chief"), its checked state is the mode, and its description is
  hidden text for the current mode, picked by the stylesheet. Its form owner is
  an empty form of its own (`#mv-to-form`), so Enter on it never submits the
  composer; the composer's script makes Enter toggle it. (It replaced a
  two-radio "`<runtime>` | Chief" switch above the row.)
  - Chief mode is drawn in the review status pair (`st-review-fg` /
    `st-review-bg`): the composer's top edge, the box's ring and fill, the
    switch, and the chief's Send. Agent mode is the composer as it was.
  - ONE stylesheet rule (`#mv-compose:has(#mv-to-chief:checked)`) decides
    which Send shows. The chief's Send carries the chief route as its
    `formaction`, and the composer's Enter submits through the Send shown, so
    the mode works with script, without script, and when the chief script
    throws.
  - A disabled agent composer's box is drawn enabled (marked
    `data-agent-off`, its agent Send still disabled) when the chief mode is
    offered, so the chief mode can send without the script.
  - The script adds the mode remembered per window (`localStorage`,
    `MirrorDraftKey(…, "chief", …)`), the form's action, the placeholder, the
    box disabled again in agent mode on a disabled agent composer, and the
    strip's live refresh.
- **The chief strip.**
  - `GET /mirror/view/chief/log` serves the chief chat's own renderer
    (`ui.ChatLogBody`, surface `view-strip`), narrowed to this session's
    exchange (`viewChiefThread`), within the chief's newest 200 rows
    (`chatHistoryCap`).
  - It refreshes on `chat.reply`, on a send, and every 20 s while the view is
    in chief mode.
  - It is a native `<details>`, scrolling within 45 dvh.
- **`session_context {subject, messages?}`** (`internal/api/session_context.go`):
  chief-only, read-only, on MCP, offered with the focus tools.
  - The details: runtime, host, tmux address, window, title, status, `cwd`,
    and for a session the chief started, its dispatch's repo, base, branch
    and model.
  - The tail: up to 20 messages, each redacted then cut at 6 KiB, with
    `ref = "m:" + sha256(role‖0x00‖raw)[:12]` and its fenced blocks listed.
  - A 32 KiB cap that drops the oldest messages first, and the untrusted note.
  - **No `suggested_target` or `suggested_model`** (round 0, Q1/D3): what a
    successor runs on arrives in phase 2, together with the byte-copied
    kickoff (`prompt_from`) and the `successor_of` binding.
    `TestSessionContextAnswersExactlyItsKeys` pins the answer's key set.
- **`sessions_overview`** now lists opencode windows (`liveSessions` reads
  `mirrorFocusEntries`), with `runtime: "opencode"`. The plugin session hint
  does **not** suggest them (`hintableSessions`, round 1): a hint is an offer
  to route, and `route_to_session` takes Claude Code windows only. The stale "muster sends
  only to Claude Code" comment is corrected.

**Phase 2 brings, with the succession itself:**
- `suggested_target` and `suggested_model` (D3).
- `suggested_base` (D4), which also needs the host's remote check.
- `about.repo`-style resolution of a window's repository to a connected one,
  where a dispatch needs it.

**Left for later, stated:**
- One draft for the box in both modes (2.1 keeps one per mode).
- On a view whose agent composer is disabled, chief mode has no mic or
  paperclip, and the box needs the script to be enabled.
- The strip's collapsed one-line state (2.3) is a native `<details>` summary.
- The Compose chief is not offered `session_context`. Its pre-allowed tools
  are an operator decision (`ComposeChiefAllowedMCPTools`).
- The strip attributes a reply by position. A user row of another kind,
  written while a view turn is still running, would cut that reply out of the
  strip; it stays in the chief's own chat. **UNVERIFIED** against production
  interleavings.

## Phase 2 as built

**The operator's decision (2026-11-09), answering phase 1's open question.** A
view line marks no owner turn, so it cannot authorise a start by itself. The
authorisation is a **one-tap confirm card**: the chief *prepares* a succession,
muster shows the owner the exact kickoff and settings, and the owner's tap is
what starts it. Transcript text can at most cause a proposal, never a start.
This replaces 4.2's binding to the message `ownerTurnFor` returns, and D5's "no
confirm tap": the confirm card is the only tap (there is no second "start with
this kickoff" step).

- **One primitive: the chief's proposal** (operator decision, 2026-11-09):
  an action the chief may PREPARE about one of the owner's sessions but never
  take. `chief_proposals` (migration **0100**; this replaces 3.2's
  `session_successions`) is one table with a `kind` (`succession` now,
  `end_session` reserved for the next phase), and every kind shares, by
  construction (`internal/api/chief_proposal.go`): ONE binding rule
  (`bindViewSubject`), ONE record and card (`fileProposal`,
  `ui.ProposalCard`), ONE tap path (`proposalForAnswer`, answerAskFrom's
  hook) and ONE tap route on the view (`POST /mirror/view/proposal`).
- **`succession_propose`** (`internal/api/succession.go`), chief-only, on MCP,
  offered with `agent_dispatch` (task routing on, the owner opted in) when the
  mirror, focus and attention stores are wired. No new configuration.
  - Arguments (unknown ones refused): `successor_of`, `succession`
    (`replace` | `spinoff` | `?`), `prompt_from: {ref, block?}`, and only when
    the owner's words say so, `target`, `model`, `repo`, `base`.
    `TestTheSuccessionProposeKeysArePinned` pins both key sets.
  - **The binding.** The newest user row the OWNER typed in the chief's
    default session must be a view line (`about` set, within the turn
    budget); `successor_of` must equal its `about`. The subject's place is
    read again from the hub, owner-scoped. Anything else is refused with its
    own sentence. (Changed in round 4: this was the chief's newest user row of
    any kind, so a plugin message delivered behind the view line refused it.)
  - **The kickoff.** muster re-reads the transcript and copies the message
    whose `ref` matches — its fenced block when `block` is given, else the
    whole message (phase 0 found unfenced kickoffs) — byte for byte. Too long
    for the spawn prompt: refused (`the kickoff is N bytes; at most M fit`),
    never cut. A control character a runtime refuses: refused, never altered.
  - **What it inherits (D3).** The subject's host and runtime; for opencode,
    the model of a session the chief started (an owner-opened opencode window
    has none muster knows, so the chief must pass the one the owner named);
    the repository of a session the chief started, else the window's short
    repository name when exactly one connected repository has it. The owner's
    words override each.
  - **The base (D4).** The old session's branch for a session the chief
    started, else the repository default. At the start, a `bad_base` refusal
    of that branch starts once more from the default (a second reservation,
    the row relinked); the card and the view say which base was used.
  - It records the proposal (a `chief_proposals` row of kind `succession`)
    and raises the card (`raiseOwnerCard`: on the Attention view and the phone, an
    owner card answered only through the owner's own doors). Its question
    holds the kickoff whole when it fits, its options are "Start it" / "Not
    now", or "Replace this session" / "Spin off a new one" / "Not now" when
    `succession` was `?` (D2). It ends the chief's turn and starts nothing.
  - **A kickoff that does not fit the card's question whole** (operator
    decision, #546 round 0 Q1) is startable only from the session's view,
    where it is shown whole. On the Attention view and the phone its card
    offers only "Review on the session's view". A start from there, whether
    that button or Start sent anyway, is refused server-side
    (`proposalSurfaceRefusal`) and the card stays open. A kickoff that fits
    whole still starts from anywhere.
- **The tap** is `answerAskFrom`'s proposal hook (`proposalForAnswer`),
  before the answer is recorded — the same path as the Attention view, the
  phone, and the view's own card (`POST /mirror/view/proposal`, which accepts
  only the card of the session that view shows). It refuses anything but the
  owner's signed-in browser or paired phone, and any answer that is not one of
  the card's buttons. Start dispatches through the host dispatch machinery
  unchanged: `hostSpawnable`, `connectedRepoName`, the chief's caps
  (`ReserveHostDispatch`), `spawnReserved` and the reconciler. The succession
  is linked to its dispatch **in the reservation's transaction, single use**:
  a second link of a decided row rolls the whole reservation back. A start
  refused before anything was reserved leaves the card open with its sentence.
- **The successor's first message** is muster's spawn preface, a line naming
  the session the kickoff was copied from, the kickoff exactly as copied
  between two rules, then muster's rules.
- **The lineage.** When the successor's dispatch turns running (the
  reconciler), a replace's predecessor is marked superseded and the owner's
  stream gets `lineage.changed` (`succession-<id>`, no text); the event also
  goes out on a proposal, a decision, a start and an end. 30 s after running,
  if no view opened the successor (`successor_seen_at`), the owner gets one
  push linking to it.
- **The view.** `GET /mirror/view/successor` answers `{id, state, mode,
  card_html, successor: {view_path, title, host, runtime}}`. The view draws the
  card (the kickoff in full, the settings, the buttons), then "A successor is
  starting…", then "Superseded. Continued in …" (replace) or "Spun off …". The
  view that saw the succession open follows it once it runs: "Opening the new
  session…" for 1.5 s with Stay here, then `location.assign` (the top window
  when framed), at most once per succession (`sessionStorage`). It asks again
  on `visibilitychange`, on a stream reconnect and on a back-forward restore.
  A view opened after the successor ran shows the banner and does not jump.
  The successor's view says "Continues “…” · replace · open previous".
- **The sidebar.** A superseded session folds into a collapsed "Superseded (N)"
  group (collapse only; the window keeps running and is listed). Its window is
  not closed (D1: phase 3).
- **The chief is told** in the view turn's header, only when the tool is
  offered: call `succession_propose` with the subject, the mode or `?`, and
  `prompt_from`, and that nothing starts until the owner taps. The agentspec
  instructions are unchanged: the header reaches both runtimes on exactly the
  turns it applies to.
- `session_context` is unchanged: its `suggested_*` keys were not re-added,
  because the proposal inherits the settings itself.

**Round 1 (audit of #546):**
- The tool's `kickoff.first_line` is redacted with `redact.Secrets` (the
  session_context redactor). A proposal card's answer is **never delivered
  back** as an attention answer: a tap is not a request for a turn, so its
  delivery is closed (`MarkWaited`) and no turn is queued. The chief gets
  muster's own sentence instead, a chat note that costs no turn ("Your owner
  tapped Start on your proposal N (succession of <key>)…", or "declined"),
  with no kickoff text. The Answered list says muster recorded the answer.
- A start is a **tapped button**: the view's card, or the Attention view's
  option button (`optionTapped`). Typed text that spells a button accepts
  nothing.
- **Never from the phone:** a proposal card's push carries no chips and no
  signed answer actions, and asks for no suggested replies
  (`raiseProposalCard`). A device answer that would start a succession is
  refused server-side. The surfaces that may start one are those that
  showed the whole kickoff: the session's view always, and the Attention
  view when the kickoff fits its card.
- A kickoff holding invisible formatting is refused, never altered: a bidi
  embedding, override or isolate (U+202A–202E, U+2066–2069), a zero-width
  space, word joiner or byte order mark (U+200B, U+2060, U+FEFF), or a tag
  character (U+E0000–E007F). Round 2 narrowed this from every format
  character: an emoji sequence's zero-width joiner, the ZWNJ of Persian and
  the Indic scripts, the left-to-right and right-to-left marks and a soft
  hyphen are ordinary text.
- The card promises the fall back to the default branch only where it runs
  (D4: the inherited branch of a session the chief started).

**Round 2 (delta audit of #546):**
- **The kickoff never reaches the chief's model raw, by construction.** The
  card's attention ask holds muster's own sentence only ("Start a session that
  continues “…”? … Read the kickoff (N bytes, copied exactly from the
  session) in muster: on the session's view, or on this card in the Attention
  view."), never kickoff text. The kickoff is stored only on the
  `chief_proposals` row. The owner's card draws it from that row: the
  session's view, and the Attention view's card and the ask page (both for
  the owner's own principal only). So `attention_list`, `attention_wait`,
  the push, the delivery sweep (even when closing the delivery fails) and any
  later reader of the ask see no kickoff. A ledger test pins that the only
  writer of a proposal's ask is `fileProposal`, whose question is built by
  a builder that reads the kickoff's length only.
- Round 3: a card whose proposal row (or ask) cannot be read when it is
  drawn shows "muster could not load this card's proposal just now… reload"
  and offers no start button, on the Attention view and the ask page (round
  4: nor an end-session card's End it). The
  Answered list says muster recorded a proposal card's answer only once its
  delivery was closed; otherwise it says where the answer went.
- The cut rule is unchanged in effect and reads the row's kickoff length: a
  kickoff over the room the question leaves of an ask's limit is drawn cut on
  the Attention view, whose card then offers only "Review on the session's
  view".

**Left for later, stated:**
- The 30 s push to a successor nobody opened is an in-process timer: a muster
  restart in those 30 s loses it (the successor still runs and is listed).
- The view's card shows its own kickoff in full; the Attention view's card
  shows it whole only when it fits the question's limit, and the phone's push
  shows only the question.
- An owner line typed in the chief's own chat while a card is open settles the
  oldest open owner card in session (the owner-card rule); a succession card
  settled that way starts nothing and reads as expired on the view.
- No phase 3 (closing the replaced window) and no pod successors.

## End-session as built

This section follows D9. It is the chief proposal's second kind
(`end_session`), built on the same binding, card and tap.

- **`end_session_propose {session}`** (`internal/api/end_session.go`) is
  offered wherever `succession_propose` is. The session is bound in one of
  two ways:
  1. **From a view line:** the shared rule (`bindViewSubject`). The session
     the chief names must be the one the line is about.
  2. **From the chief's own chat:** the turn must be the owner's own typed
     turn (`ownerTurnFor`), and the session may be any of the owner's: a
     mirrored session's key (owner-scoped through the hub), or `agent:<name>`
     for any pod agent of the owner's. The chief itself and an officer's
     instance are refused at the proposal, with no card. A plugin message, a
     session's words or muster's own note never binds.
- A mirrored session's host must advertise agent-close. Otherwise the
  proposal is refused, saying so.
- **The card** reads "End this session?", with the session, where it runs and
  what will happen, and the buttons End it / Not now. Its attention ask's
  question (the Attention view, the phone, `attention_list`) is muster's own
  sentence naming exactly which session: its label, then host · tmux session
  · window (or "your agent “name”"), then what happens. It holds no
  transcript text.
- **The tap** (the same `proposalForAnswer`):
  - Everything that can refuse runs first, and a refusal leaves the card
    open: the window still listed, still the card's own window (the host,
    tmux session and window the card named), the host able to close, the
    agent still the owner's and not protected.
  - Then `StartEnd` (accepted, ending) and the end itself:
    - A mirrored window: muster sends the host **agent-close** for exactly
      that window, naming the conversation it must be running
      (`AgentCloseReq.Key`; `mirror.Hub.CloseAgent`, whose one caller is
      `closeEndedWindow`).
    - A pod agent: `destroyAgent`, as the owner's destroy does.
  - `RecordEnd` stores ended or failed, with a one-line detail.
  - The view's proposal region shows "Ending this session…", then "Session
    ended." or the failure. Once the window is gone, the view reads the
    proposal by id (`GET /mirror/view/proposal?id=`).
- **agent-close, the host half** (`internal/mirrorhost/agentclose.go`):
  - It is advertised (`Msg.AgentClose`) only by an `-interactive` v2 host
    whose backend lists windows, pastes into a pane and closes a window.
  - The request names one window (its tmux session and window id) and the
    conversation muster expects there. The host refuses anything else, with
    nothing typed or closed:
    - a window it does not list in that session (`not_listed`);
    - a window running no agent (`no_agent`);
    - a window running another conversation, in its active pane or any
      other (`mismatch`).
  - **Claude Code:** the paste guard, then the Claude prompt guard
    (`claudeReady`: its session status, its screen, an EMPTY input box).
    Only if all pass is `/exit` typed, through the checked paste
    (`PasteChecked`), which checks again before the paste and before its
    Enter. Any refusal types nothing (or withholds the Enter), and the
    window is closed at once, reported not graceful with the reason. Enter
    on a permission prompt would approve it, on a picker would take its
    first option, and after a draft would submit the draft. After `/exit`
    the host re-lists every 300 ms:
    - the window gone is a graceful end;
    - the agent's OWN pane (its pane id and process, read at the start) no
      longer running it is also graceful, and the window is closed if it
      runs no other agent;
    - otherwise, after the grace (30 s) the host lists the window once more
      and closes it only if it still runs that conversation, reported not
      graceful.
  - **opencode:** closed at once, reported not graceful. muster types into
    no opencode pane (the paste guard refuses it), and the opencode server
    API the host uses (`internal/opencodehost`) has no call that exits its
    TUI.
- **Ship note:** the hosts run `muster-mirror` as pinned by the operator's own host
  flake. Agent-close reaches a host only once that host runs a release with
  it, which needs a flake bump after the release. Until then the host does
  not advertise it, and the chief's proposal is refused with that sentence.

**Left for later, stated:**
- The card is not drawn in the chief's own chat. From that path the owner
  taps it on the Attention view or the phone (a chat tap is their next typed
  message, which settles an owner card without accepting it).
- No graceful exit for opencode: its window is closed at once. A graceful
  path would need an exit call in opencode's server API.

## Round 4: four defects seen live (2026-11-09)

Observed on the release with succession phase 2 and end-session.

- **A view line with other inputs queued behind it was refused.** The owner
  asked from a view; plugin mail delivered right after it was its own turn
  each; the chief answered the view line inside one of those turns, and
  `succession_propose` was refused ("the message you are answering was not
  typed on a session's view"), because the binding read the chief's newest
  user row of any kind. Now ONE predicate (`ownerViewLine`, behind
  `bindViewSubject` and both propose tools) takes the newest row the OWNER
  typed (`typed_by`, which only the owner's signed-in chat doors set). A row
  nobody typed (a plugin delivery, a relayed answer, muster's note) is passed
  over and never authorises: it has no subject. A line the owner typed in the
  chief's own chat after the view line is the newest owner line, and has no
  subject. The freshness window (the turn budget, from the line) and the
  subject rule are unchanged.
  - **Single use.** One owner line authorises at most ONE proposal of each
    kind for a session: `CreateProposal` refuses a second row of that kind
    with the same `owner_message` and `subject_key`
    (`ErrProposalLineUsed`, under a transaction-scoped advisory lock), and
    `fileProposal` asks first so no card is raised. Before, the binding to
    the newest row did this implicitly in most cases (muster's note after a
    proposal was the newest row); now that such rows are passed over, it is
    explicit. The message id is not passed by the chief: the server derives
    the line itself, as before.
  - Accepted, stated: a plugin message delivered within the budget behind an
    unused view line can still steer the chief into proposing for that
    view's session, once per kind. It could always do so inside the view
    line's own turn (transcript text can cause a proposal), and nothing
    starts or ends without the owner's tap on a card that shows what would
    happen.
- **The update pill covered the card.** At phone width the "Updated · Reload"
  pill sat on a succession card's kickoff and an end card's subtitle: it
  slides only past controls, and the card arrives by the view's own fetch,
  after the page settled. `pwa.js` now avoids a proposal card whole
  (`[data-proposal]`), and the view's script fires `muster:layout` when it
  draws the card (as does any `<details>` toggle), so a pill on it moves off.
- **A question the proposal answered stayed open.** The chief asked about the
  session (ask_owner or attention_ask, e.g. "which repository?" after a
  refusal); the owner answered with a new line on the view, which settles
  none of the chief's asks; the chief proposed; the ask stayed open until it
  expired. Now an ask records the session it was raised about
  (`attention_asks.about_key`, migration **0103**), set ONLY by muster, and
  only when the turn's own input IS the owner's view line
  (`chiefAskSubject`: no plugin message, relayed answer or note behind it).
  Filing a proposal withdraws the chief's own open asks with that
  `about_key` (`attention.WithdrawAbout`): `cancelled`, with
  `settled_reason` ("superseded: your chief proposed a … of this session
  (proposal N) …"), which `attention_wait` returns as `reason`. Never matched
  by text: an ask with no subject, another session's, another asker's, a
  question to the chief, a notice, and the proposal's own card are not
  touched. muster's own cards for the chief (a held dispatch's) carry no
  subject.
- **An unreadable end card still offered End it.** Round 3 kept the start
  buttons off a card whose proposal row could not be read, but not End it.
  Which kind the card is cannot be known then, so every kind's accept is left
  off (`proposalStartLabels`), and the note says the proposal could not be
  loaded.
- A dialog that opens in the milliseconds between the last check and the
  Enter is not caught: the Claude prompt guard is a check before the keys,
  not a lock (`claude_guard.go`).

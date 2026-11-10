# opencode as a first-class runtime (API send first)

**Status: partly built.** Phase 1 (the launcher and the API send) is built.
Phase 3's context and model readout is built, and so are three things this
design first left out: sending to a hand-opened pane whose binding the host
verifies ([Panes the launcher did not start](#panes-the-launcher-did-not-start)),
turn errors, and answering a whole question form from the Rendered view
([Asks from the view](#asks-from-the-view)). Phase 2, phase 3's cost and
phase 4 are not built. Each section below says which.

[opencode.md](opencode.md) is the as-built reference: what muster relies on
from opencode's server today (pairing, the read-only view, asks), with each
fact read from upstream. This proposal is a separate file on purpose. That
file says what the code does, and this one says what it should do; mixed
together, the reference would describe code that does not exist. When a
phase lands, its as-built facts move into opencode.md, and the reasoning
stays here.

**Upstream source** is <https://github.com/anomalyco/opencode> at `6ec5682`,
the build `opencode v2.0.32+6ec5682` that the existing fixtures were captured
from (`internal/opencodehost/opencodetest/testdata/*-v2.0.32.sse`). Upstream
paths below are relative to that repository. They were read in source, not
measured against a running server; phase 1's test plan captures the frames
that pin them. ⚠ `6ec5682` is **not** an ancestor of the `v2.0.32` tag
(`843a6c5`): the installed build reports `6ec5682`, so that is the commit read.

## Goal and non-goals

**Goal:** an opencode window gets what a Claude Code window gets, reached
through opencode's own API, never through keystrokes:

- sending from the Rendered view, with everything that comes with it: the
  optimistic send and its pending bubble, the send queue ("when idle"),
  attachments and dictation;
- the slash/command menu, listing opencode's own commands;
- a model and cost/token readout;
- chief nudges, which today are only a notice for opencode.

**Non-goals:**

- typing into an opencode pane. The paste guard stays, and the Terminal
  view's composer stays refused there;
- ~~sending to a pane that was **not** started by the launcher~~. The operator
  asked for it later, and it is built for a pane whose title binds it
  unambiguously and verifiably
  ([below](#panes-the-launcher-did-not-start)); every other hand-opened
  pane keeps the title-matched view and stays view-only, saying why;
- interrupt/stop and suggested replies (see [Later](#later));
- an opencode server other than the host's shared one (`--standalone`,
  `--server`): those are not viewed today either
  (`internal/mirrorhost/opencode.go:511-514`).

## Today's state

opencode is in the runtime seam with view and prompts, and no send:
`Capabilities{View: true, Prompts: true}` (`internal/mirror/runtime.go:86-93`).
A send is refused at three layers:

1. **Server, page:** the composer is drawn disabled with no queue
   (`internal/api/mirror_view.go:97-105`).
2. **Server, hub:** `lookupAgent` admits only `Caps.Send`
   (`internal/mirror/transcript.go:174-192`).
3. **Host:** the paste guard refuses a pane whose tree holds opencode
   (`internal/mirrorhost/paneguard.go:55-57`, `openCodeShape` at `:327`), and
   its words say so (`internal/mirror/wire.go:1351-1353`).

The pane-to-session map is a heuristic: the TUI's title `OC | <title>`
matched against root sessions (`opencode.go:526-546`, `SourceTitle`). The
heuristic is why sending stays off. A title names no id, it is cut at 40
characters, two sessions can share it, and a user can turn it off.

### Parity matrix

"CC" is Claude Code. A phase number is the phase in which opencode gets the
feature; "—" means it stays out.

| feature | CC today | opencode today | opencode target | phase |
|---|---|---|---|---|
| rendered view | transcript tail | API messages (title-matched) | same, plus launcher-bound panes | 1 |
| send from the Rendered view | paste + Enter | refused ×3 | API `session.prompt`, launcher-bound panes, and hand-opened panes whose binding is verified | 1, **built**; hand-opened **built** |
| optimistic send / pending bubble / Not confirmed | yes | composer disabled | same contract; a retry is deduplicated by opencode (below) | 1 |
| send queue ("when idle") | yes | no queue drawn | yes, gated on opencode's own status time | 1 |
| attachments | `[attached: …]` line with a signed link (`docs/uploads.md`) | — | the same text line (the agent fetches it) | 1 |
| dictation | inserts text in the box | box disabled | the same (browser-side only) | 1 |
| answering asks | keys into the picker | API reply (`OpenCodeAnswer`), one short single-select only | the whole form from the view's card (`OpenCodeForms`, `OpenCodeReply.Picks`), and permissions | **built** |
| attention queue, push | yes | yes | unchanged | done |
| slash/command menu | host catalog + CC built-ins | menu shows **CC's** built-ins | `GET /api/command`, sent through `session.command` | 2 |
| context readout | "124k context" (`internal/ui/mirror_view.go:644-659`) | none | context of the last step / model limit | 3, **built** |
| model name | not shown | not shown | `model.id` from the assistant message | 3, **built** |
| cost | not shown | not shown | session `cost` (USD), as opencode reports it | 3 |
| per-tool context cost | measured | estimate (`internal/cctranscript/toolctx.go:83`) | unchanged (later: step tokens) | — |
| turn errors held by the queue | `TurnError` (`internal/mirror/turnerror.go:134`) | none | `TurnError` from the session's messages (kind, time, quick count; never a usage limit) | **built** |
| chief nudges | queued paste | notice only (`internal/api/chief_nudge.go:926-936`) | queued API send, bound panes only | 4 |
| interrupt / stop | not in muster | — | `session.interrupt` | later |
| suggested replies | yes | blocked (`internal/api/suggest.go:343`) | — | later |

## What opencode's API provides

### Posting a prompt

`POST /api/session/{sessionID}/prompt`, operationId `session.prompt`
(`packages/protocol/src/groups/session.ts:320-340`): "Durably admit one session
input and schedule agent-loop execution unless resume is false."

- **Body:** `{id?, text, files?, agents?, skills?, metadata?, delivery?, resume?}`.
  `text` is required, and `additionalProperties: false` applies, so there are
  **no agent or model fields**: the session's own selection is used
  (`POST …/agent`, `POST …/model`).
- **`id`:** an optional client-supplied message id. It must start with `msg_`
  (`packages/schema/src/session-message.ts:23-30`).
  - The same id again, on the same session and type, returns the record stored
    first, with 200 and **without re-reading the payload**
    (`packages/core/src/session/session.ts:155-171`, `reconcile` in
    `packages/core/src/session/inbox.ts:155-167`).
  - The same id on another session answers 409.
  - **This makes a send idempotent by its id** (see [delivery](#delivery-confirmation)).
- **`files`:** `{uri, name?, description?, mention?}` (`packages/schema/src/prompt-input.ts:9-14`).
  - `uri` is `data:` or `file:` only; any other scheme answers 400
    (`packages/core/src/session/prompt.ts:97-106`).
  - The mime type is sniffed from the bytes, and a data URL's declared type
    is ignored (`prompt.ts:104`, `packages/core/src/mime.ts`).
  - Each file is at most 20 MiB (`prompt.ts:203`).
- **Response:** 200 `{data: Session.Inbox.User}`, which is `{id, sessionID, time,
  type: "user", payload, delivery}` (`packages/schema/src/session-inbox.ts:52-59`).
  - `data.id` becomes the user message's id in `GET …/message`.
  - **It answers on admission, not on completion:** `execution.wake` only
    starts or flags the run (`packages/core/src/session/run-coordinator.ts:136-145`).
  - Errors are 400 (a bad field), 404 (no such session) and 409 (an id conflict).
- **A busy session is never refused and never interrupted.**
  - `delivery: "steer"` (the default, `session.ts:163`) is pulled into the
    running turn at its next step boundary.
  - `"queue"` waits for the turn to end and is taken one at a time at idle
    (`inbox.ts:44-48`).
- **Events, in order:** `session.inbox.enqueued {sessionID, inboxID, item}`, then
  `session.inbox.delivered {sessionID, inboxID}` (the input is now a visible
  user message, id = inboxID), then `session.execution.started`,
  `session.step.*`, and `session.execution.succeeded | failed | interrupted`
  (`packages/schema/src/session-event.ts:223-271`).

### Commands

- **List:** `GET /api/command` returns `{location, data: [{name, description?}]}`
  (`packages/protocol/src/groups/command.ts:9-20`). It has no source, kind
  or argument fields. It depends on a directory:
  - pass `?location[directory]=…`, or the server uses its own working
    directory (`packages/server/src/location.ts:73-75`);
  - `command.updated` fires when the list changes.
- **Run:** `POST /api/session/{id}/command {name, text, files?, delivery?}`
  answers 204 with **no message id and no idempotency key**, and 404 for an
  unknown command (`session.ts:342-360`).
- 🔴 **`/name args` sent as a prompt is NOT a command.** The server does not
  parse a slash; the TUI does, then calls `session.command`
  (`packages/tui/src/prompt/parse.ts:18-26`,
  `packages/tui/src/component/prompt/index.tsx:1318-1334`). A prompt
  beginning `/review` reaches the model as literal text.

### Model, tokens and cost

- **An assistant message** carries `model {id, providerID, variant?}`,
  `cost?` (USD) and `tokens? {input, output, reasoning, cache {read, write}}`
  (`packages/schema/src/session-message.ts:213-237`).
- **A session** carries the sums, `cost` and `tokens`, live as
  `session.usage.updated` (`packages/schema/src/session.ts:31-59`,
  `session-event.ts:182-189`).
- **Context fill** is not a session sum. The TUI shows the last assistant
  message's tokens over `limit.context` (`packages/tui/src/util/session.ts:45-61`),
  and the limit comes from `GET /api/model` (`Model.Info.limit`,
  `packages/schema/src/model.ts:103-126`).

### Which session a TUI shows

This decides the launcher.

- **`opencode -s/--session <id>`** is "Session ID to continue, or to create if
  it does not exist" (`packages/cli/src/commands/commands.ts:51-55`). An id
  that does not exist yet becomes the id of the session that the TUI's
  **first typed prompt** creates (`packages/cli/src/commands/handlers/default.ts:53-61, 113-115`).
- **`POST /api/session` takes a client `id`** (and `location {directory}`),
  so a session can exist before the TUI opens it
  (`packages/protocol/src/groups/session.ts:142-152`).
- **Nothing reports which session a TUI is showing.**
  - There is no endpoint and no event for it, and v2 has **no `/tui/*` control
    routes**.
  - The terminal title is `OC | <title>` (cut to 37 characters plus "…" past
    40), or `OpenCode` on the home screen or while the title is a fallback,
    and it can be turned off (`packages/tui/src/app.tsx:613-639`).
  - `session.viewed` is partial: it fires only when the TUI focuses a session
    with an unread idle transition.
- **The TUI can switch sessions** (`<leader>l` lists them, `<leader>n` starts
  a new one, `packages/tui/src/config/keybind.ts:110-111`). Outside the TUI,
  only the title changes.
- ⚠ **A TUI whose version differs from the running service restarts the
  service** (`mismatch: "replace"`, `default.ts:38-46`).

### Interrupt (for later)

`POST /api/session/{id}/interrupt?resume=<bool>` answers 200 `{interrupted}`
(false when there was nothing to stop) and emits `session.execution.interrupted
{sessionID, reason: "user"}` (`session.ts:670-689`).

### Routes the token reaches

The existing pair token can do far more than muster uses. It reaches
`POST /api/session/{id}/shell`, `POST /api/pty`, `POST /api/shell` and
`POST /api/experimental/fs/write` (`packages/protocol/openapi.json`). **It is
a shell-equivalent credential for the host's user, and is today already.**
See [Security](#security).

## Architecture of the API send

### The launcher record

**CLI:** `muster-mirror opencode [opencode flags…] [<directory>]`. It is a
subcommand of the host binary, beside `opencode-pair`
(`cmd/muster-mirror/main.go:144-146`), because the host binary already holds
the opencode token, the tmux-socket derivation (`tmuxsocket.go`) and the
per-tenant record pattern (`hookconfig.go`). `muster` (`cmd/muster`) is the
API client for muster's server and holds none of those.

It does, in order:

1. **Refuse what cannot be bound.** It refuses to bind when any of these
   holds, and then **runs opencode unbound** with a one-line reason on
   stderr. It never fails the start.
   - It is not inside tmux (`$TMUX` or `$TMUX_PANE` is empty).
   - It was given `--standalone` or `--server` (not the shared server).
   - It was given `-c/--continue` (which session that resumes is not known
     in advance).
   - There is no opencode token (`~/.muster/opencode-token`).
   - A subcommand was named (`run`, `serve`, …: not the TUI,
     `opencode.go:50-53`).
2. **Refuse a version mismatch** (decided). The launcher compares the
   `opencode` binary it will exec (`opencode --version`) with the running
   service's (`GET /api/info`), after removing a leading `opencode ` and `v`
   from each. If they differ, it **refuses to start** and names both
   versions and the fix: "the opencode TUI is <a> but the running service is
   <b>; starting it would restart the service and can sign every client
   out. Run `opencode service restart` so they match, then start it again."
   Upstream's own default would restart the service silently
   (`default.ts:38-46`).
3. **Choose the session.**
   - With `-s <id>`, it is that id. The id must exist on the shared server
     as a **root** session (`GET /api/session/{id}`, `parentID` null), or
     binding is refused.
   - Without `-s`, the launcher mints an id in upstream's own format: `ses_` +
     the time-descending identifier of `packages/schema/src/identifier.ts`. It
     then creates the session with `POST /api/session {id, location:
     {directory}}`, where the directory is the positional argument or the
     working directory.
   - Creating it **first** means the session exists before the TUI starts, so
     a send from the phone works before anyone types in the terminal. With a
     merely reserved id, the session would not exist until the TUI's first
     typed prompt, and `session.prompt` would answer 404 until then.
4. **Write the record**, then **`exec`** `opencode -s <id> [the rest]`.
   - `exec` keeps the pid, so the record can name the TUI's own pid before
     it runs.
   - The launcher passes only arguments it has parsed; anything it does not
     recognise is refused, never forwarded blind.

**Where the record lives:** `~/.muster/opencode-launch/<socket-key>/<pane>.json`.

- The file is 0600 and the directory 0700. It is written atomically
  (`internal/atomicfile`).
- `<socket-key>` is a digest of the tmux socket path that `$TMUX` names.
  `<pane>` is `$TMUX_PANE` with its `%` removed.
- One file per pane: a new launch in the same pane replaces the old record.

```json
{"v": 1, "socket": "/tmp/tmux-<uid>/default", "pane": "%12",
 "pid": 4242, "pidStart": 123456789, "session": "ses_…",
 "server": "http://127.0.0.1:<port>", "directory": "/…", "created": 946684800}
```

- `pidStart` is field 22 of `/proc/<pid>/stat`, so a reused pid does not
  match.
- `server` is the url from the token file at launch, so a record made against
  another server is detectably foreign.

**How a pane maps to a session.** `openCodeRuntime.LocatePanes` already finds
the TUI's pid on the pane's terminal (`opencode.go:100-124`). Before the title
match, `resolve` (`opencode.go:500`) looks for this daemon's record for the
pane. The record **binds** the pane, with a new `SourceLauncher`, when all of
the following hold:

- **Socket:** the record names this daemon's tmux socket. That is
  multi-tenant routing: a tenant's daemon reads only its own server's
  records, as `hookconfig.go` does.
- **Process:** the pid **and** its start time equal the TUI's that the walk
  found.
- **Server:** the record's server is the token file's.
- **Session:** the session exists and is a root session.
Any failure falls through to today's title match, which leaves the pane
view-only, and its reason becomes the card's note (`AgentNote`).

**The title cross-check is a warning, never a gate** (decided). It is the
only signal a TUI gives of a session switch, and it **never decides a send's
target or whether a send happens**. The title is consistent with the record
while it is one of:

- `OC | <t>`, where `t` matches the record's session title by
  `opencodehost.MatchTitle`'s own rule (exact, or a prefix when cut at "…");
- `OpenCode`, while the record's session has no title or a fallback one (a
  fresh session);
- unreadable or empty, while the session's title has not changed since the
  last read. This allows for titles turned off.

Any other title keeps the binding and raises a **warning**: the window's
note (`AgentNote`) and the Rendered view's composer say "This pane's title
shows a different opencode session than the one muster sends to (<title>).
Messages still go to the session shown here." It clears as soon as the title
is consistent again. Two sessions that share a title cannot be told apart, a
named, residual gap; since the check only warns, a gap costs a missing
warning, never a misdirected send.

⚠ **A missed switch does not misdirect a send**, and this is the property
that matters. The send goes to the **recorded** session id, and the Rendered
view muster shows is drawn from that same id. What a person reads in muster
and where the message lands cannot diverge. A missed switch only means the
window's card names a pane that is showing another conversation.

**Lifecycle.**

| event | what happens |
|---|---|
| the TUI exits | pid gone: the record is stale; the daemon deletes it on its next pass, and the pane (now a shell) is not an opencode window at all |
| a TUI restarted in the pane without the launcher | the pid differs: the record is stale and deleted; the new TUI is title-matched, view-only |
| tmux detach / attach, a muster-mirror restart | the pane id and pid are unchanged: the binding survives; the daemon reads records from disk on every listing, and holds no state of its own |
| the TUI switches session | the binding stays (sends still go to the recorded session, which the view shows); the title warning shows until the title is consistent again |
| the session is deleted (`session.deleted`) | the binding is dropped and the record deleted |
| the service restarts (TUI version mismatch, upgrade) | the token may be refused if the password changed (opencode.md); the window shows the token's reason, as today; the record survives |
| `muster-mirror opencode -s <id>` in another pane on the same id | both panes bind to one session: allowed, and both windows view and send to it |

**Read-only daemons** (`-opencode-read-only`, `cmd/muster-mirror/main.go:179`)
read records the same way. A record is written by the launcher, never by a
daemon, so there is no writer conflict. **Stale-record cleanup** is done only
by the daemon whose socket the record names.

### Host side

- **The client.** It gains one write, `Client.Prompt(ctx, session, id, text)`,
  which is `POST /api/session/{id}/prompt {id, text, delivery: "queue"}`.
  - **No `files`** in phase 1: an attachment is the text line it already is
    (see [Phase 1](#phase-1-launcher-send-queue-optimistic-send-attachments-dictation)).
  - **`delivery: "queue"`, never steer.** muster sends only to an idle
    session, and if the session went busy in between, `queue` holds the
    message until the turn ends. Steer would push it into a running turn,
    which no Claude send ever does.
  - The launcher's `CreateSession` and `GetSession` live in
    `internal/opencodehost`, so the request ledger keeps one owner.
- **The seam.** `gatedSender` comes back (as at
  `git show 646ca63e^:internal/mirrorhost/runtime.go:65-69`).
  - `openCodeRuntime` implements `gatedSend(h, m)`.
  - `hostSide.paste` routes a `Msg.Runtime == "opencode"` paste to it
    **before** `typeInto`, as Hermes's was (`646ca63e^:internal/mirrorhost/transcript.go:670-678`).
  - Nothing else changes in the paste path.
- **The host's checks, at the moment of sending.** These do not depend on
  muster's:
  1. this host is `-interactive` **and** `-opencode-send`, and holds a token;
  2. the session and window are listed (`h.allowed`), and the text passes
     `mirror.SanitizePaste`;
  3. the window, re-resolved **now**, is launcher-bound (`SourceLauncher`)
     to the session muster named;
  4. its status is idle, its `StatusSince` equals `m.Since` (the time muster
     decided on), and no ask is pending under it (`opencode_asks.go`).
- **Delivery confirmation.**
  - 200 means `TypePasted` OK (the message is in the session's inbox:
    admitted, durable).
  - 400 or 404 means a refusal with its reason; nothing was sent.
  - 401 means the token's reason (opencode.md).
  - A transport error **after the request was written**, or a timeout, is
    reported as unconfirmed.
- **The paste guard stays.** `openCodeShape` keeps refusing keystrokes into an
  opencode pane, the Terminal view's composer included. The API send is not
  `typeInto`, and the guard's ledger (`internal/mirrorhost/runtime_interface_test.go:95-102`)
  is unchanged.

### Delivery confirmation

The composer's contract (`docs/sending-and-step-up.md`, "A sent message shows
at once") holds as it is: OK, refused, or **Not confirmed**.

What opencode adds is that a retry is harmless:

- **The message id is derived from the send's nonce.** muster already keys
  "Send again" on the nonce (`internal/api/mirror_view.go:779-801`), and a
  queued row has its own (`sendQueueNonce`, `internal/api/send_queue.go:818`).
  The server sends `Msg.MessageID`, which is `msg_` + an id in upstream's
  ascending format, with its time part taken from the nonce's first attempt
  and its random part from an HMAC of the nonce.
- **The same nonce therefore always posts the same id**, and opencode returns
  the first admission instead of a second message (`inbox.ts:155-167`).
- A **Send again** after Not confirmed can land at most once. The page still
  says Not confirmed and still asks; only what the button can do is safer.

⚠ Whether any upstream listing **orders** messages by id was not
established. The ascending format is used so that it would not matter.

### Wire and hello

- **`Msg.OpenCodeSend` (hello):** the host advertises that it sends to opencode
  through the API. The host sets it when it is `-interactive`, was started
  with `-opencode-send`, and holds a token (beside `openCodeAnswer`,
  `opencode_asks.go:399-405`, and `client.go:375`).
  - The hub records it as `gatedSends[RuntimeOpenCode] = hello.OpenCodeSend &&
    hello.Interactive` (beside `runtimeAnswers`, `internal/mirror/hub.go:711-716`).
  - No version bump, backward-compatible both ways: an older host never sets
    it, and an older muster ignores it.
- **`Window.AgentSend`:** the host sends to this window's session (it is
  launcher-bound). It is bounded and cleared for any runtime that is not
  `GatedSend`, like `AgentSession` (`internal/mirror/wire.go:560-578`).
  - The page and the hub need it per window. `Caps.Send` is per runtime, and
    a title-matched opencode window must stay view-only.
- **`Msg.Since` and `Msg.MessageID` on the paste.** `Msg.Runtime` already
  exists.

### Server: RuntimeSpec

The opencode entry gains `Caps.Send: true`. The fields of the removed Hermes runtime come back
(`646ca63e^:internal/mirror/runtime.go:56-69`): `GatedSend`, `PasteTimeout`,
`SendOff` and `SendOffErr`. `Answers` becomes `Advertises(hello) (send,
answer)`.

`lookupAgent` (`transcript.go:174-192`) admits a `GatedSend` window only when
its host advertised the send **and** the window is `AgentSend`. `PasteWindow`
refuses a `GatedSend` window unless it is idle with a status time (Hermes's
idle gate). Like the Claude prompt guard (`transcript.go:437-442`), a queued
paste is superseded rather than failed.

The refusals, each in the person's words:

| refusal | when | words (draft) |
|---|---|---|
| `ErrOpenCodeSendOff` | the host did not advertise `OpenCodeSend` | "This host's muster-mirror does not send to opencode (it needs -interactive and -opencode-send). Type it in the terminal." |
| `ErrOpenCodeUnbound` | not `AgentSend` | "muster can't be sure which session this opencode shows: it wasn't started with `muster-mirror opencode`. Type it in the terminal, or restart it that way." |
| busy / waiting / unknown | the idle gate | the queue's words for opencode (`send_queue.go:803-805` gains an entry) |
| session gone (404) | the session was deleted | "That opencode session no longer exists." |
| token refused (401) | as today | the token's reason (opencode.md) |

The page (`mirror_view.go:97-105`) draws the composer enabled, with its queue,
when the window is `AgentSend` on a host that advertised the send. Otherwise
it keeps the disabled composer, with the refusal's words in place of "isn't
supported yet".

### The queue

The queue already decides on `AgentState` (`internal/mirror/agentstate.go:115-147`).
An opencode window has a host status and status time from the event stream.
The rule that the next send waits for a newer status
(`internal/sendqueue/dispatcher.go:134`) holds because a prompt starts
`session.execution.started`, a status write newer than the one held at the
send.

What changes:

- `AgentState.Session` is set for opencode too (today Claude only,
  `agentstate.go:135-137`);
- the queue gets opencode's words (`send_queue.go:803`).

## Panes the launcher did not start

**Built** (the operator's later ask, lifting a non-goal). Nothing upstream
names the session a TUI shows ([above](#which-session-a-tui-shows)), so a
hand-opened pane's binding can only be the title match, made strict enough to
send to and checked again at the moment of sending
(`internal/mirrorhost/opencode_bind.go`). The host binds it — `Window.AgentBind
"verified"`, a field of its own so that an older muster keeps such a pane
view-only — when:

- the TUI is a client of the shared server and the server answered its
  session list — every session, roots AND children (`GET /api/session` with no
  `parentID` filter lists both);
- the event stream is open, so the session list is current (each session
  event re-reads it) — else `stream-down`;
- exactly one session — of all of them, roots and children — carries the
  title: the whole title, or for a title opencode cut with `…`, every title
  that starts with what precedes it. Two root sessions sharing it are no match
  (no view, as before). A child (a subagent's session) sharing it, exactly or
  as the start of a cut title, is `title-shared`: a TUI can show a child
  session, and its title is then the pane's. A cut title that starts exactly
  one session binds;
- the whole session list was read: one cut short at the client's page bound
  (its last page still naming a next one) shows nothing unique — else
  `list-cut`;
- the TUI's command line names no session or the same one — else
  `argv-session`.

Each code is the composer's reason, in words (`mirror.AgentBindWords`).

**A send never reaches another session than the view shows.** The composer
carries the session its view is drawing (`agent_session`, kept equal to the
stream's session by the page's script). A message names the session shown
when it was PRESSED, not the field as it stands when it is finally posted: one
held behind a sign-in confirm can be posted after the view moved to another
session (`composer_pending.go`, `p.shown`). A queued row stores it
(`send_queue.agent_session`, migration 0106). The hub refuses the send — a
person's, or a queued row's, which then fails rather than waiting — when its
window list names another session now, and refuses a hand-opened pane's send
that names none (an older page or row); a launcher-bound pane's send naming
none still goes, its record naming the session (`mirror.ShownSessionCheck`).
The paste then carries that session as `Msg.AgentSession`. The host
re-resolves the pane, re-reading its title, and refuses
(`opencode-moved`) when the pane resolves to any other session, and refuses a
verified pane's paste that names none (an older muster's). For a verified pane
it then reads the session through the API (`GET /api/session/{id}`): it must
still be a root session whose title the pane's carries (the same rule, a cut
title its prefix). Counting children in the uniqueness check is what closes
the gap of a child titled like a root: that case is `title-shared`, refused.

The directory is not used: the reference measured it differing for some
correct matches ([opencode.md](opencode.md)), so a gate on it would refuse
correct panes and add nothing a mismatch could not already be.

## Asks from the view

**Built.** The operator reported answering opencode's questions only from the
raw terminal. Diagnosed against this host's own opencode server, read-only:

- **The view drew no card at all** for an opencode ask: its live question
  region listed only Claude Code's hook-registered questions
  (`handleMirrorAsks`), and an opencode question showed only as a `question`
  tool row.
- **The Attention item offered no answer for most questions.** The host
  offered options only for one single-select question of at most 4 options,
  each at most 40 runes (`offerable`), and the wire then dropped the ask's id
  too (`CleanPending`). The question tool commonly asks several questions in
  one call, and its option labels often run past 40 characters, so a typical
  question could only be answered in the terminal; the rest were answerable
  from Attention, but never from the view.
- The pipeline itself worked: the asks were seen (`form.created`), and no
  answer was refused on the wire. The host's own log showed no opencode
  answer being attempted, consistent with no surface offering one.

As built: every question-tool form keeps its id on the wire (its options only
when an Attention button can answer it). The view's region draws an opencode
window's ask as the same card a Claude Code question gets: the whole form,
read from the host on demand (`TypeOpenCodeForm` → `OpenCodeForm`, advertised
as `Msg.OpenCodeForms`), and Allow once / Reject for a permission. A submit
answers every question at once as `OpenCodeReply.Picks` — option indexes into
the form the host sent, and an answer of one's own only where `custom` — which
the hub checks against the ask the window waits on now, and the host checks
again against the ask it holds, mapping each index to the option's value and
posting to the session the ASK names (a subagent's child, for one under it).
An Attention item for a form it cannot answer with a button says to open the
session.

## Security

**Gating.** A send to opencode passes exactly the gates of a Claude send:

- a fresh step-up sign-in (`stepup.Fresh`, `transcript.go:410`, re-checked at
  delivery for the queue);
- an `-interactive` host;
- the principal's own host;
- in addition, the host's `-opencode-send` (off by default) and a
  launcher-bound window.

Each is checked on muster **and** again on the host, which does not trust
muster's.

**🔴 The token-scope risk, recorded explicitly.** The opencode pair token has
**no scope and no per-token revocation** (opencode.md, "What the token cannot
do"). The routes above show that it is a **shell on this host as its user**:

- `session.shell`, `pty.create`, `shell.create` and `experimental.fs.write`
  all accept it;
- muster-mirror already holds it, so this design adds no new credential;
- the only revocation is rotating opencode's password, which signs out every
  client.

What this design changes is **what reaches the token's holder from outside**:
today, muster can cause a permission or form reply; with it, muster can
cause a prompt.

**What a compromised muster server could do**, with every host-side check
holding:

- **Can:** on hosts that run `-interactive -opencode-send`, post prompts into
  **launcher-bound** sessions only, while they are idle. A prompt can ask the
  agent to run anything its permissions allow, and **everything** in a
  session started with `--auto`. That is the same blast radius a compromised
  server has over Claude Code windows on an `-interactive` host today.
- **Cannot:** read the token; call any other route (`internal/opencodehost`'s
  request ledger pins prompt, create, reply, mint and GETs, and nothing else);
  reach an unbound session, a `--standalone` one, or a session on a
  non-loopback server (`CheckLocal`, `client.go:46-59`).

**A compromised host daemon, or a leaked token file** gives everything the
token gives: a shell. That is today's risk, unchanged.

**The launcher's own write.** `POST /api/session` creates a session and runs
nothing. It is called only by the launcher, at the operator's own terminal,
and the ledger pins its caller.

**Audit logging.**

- The host logs every send and refusal, as it logs pastes and answers today
  (`opencode_asks.go:432-435`): session, window, the opencode session id and
  message id, and the outcome. It never logs the text.
- The hub's `paste` span records the outcome (`internal/mirror/trace.go`).
- The launcher logs the binding it wrote: pane, pid, session id.

## Phases

Every phase keeps the flag off by default. The rollout of each is the same:

1. muster first, then each host's pinned muster-mirror build, on both hosts;
2. `-opencode-send` turned on per host, by hand;
3. the operator's own panes started with `muster-mirror opencode`.

An older host never advertises the send, so the order cannot break a
working window.

### Phase 1: launcher, send, queue, optimistic send, attachments, dictation

**Attachments and dictation come cheaply.** An attachment is already a text
line carrying a signed link (`docs/uploads.md`), and dictation fills the box
in the browser. Both reach opencode as text with no new code. Native `files`
(data URIs) would need the host to fetch the upload. That is not needed for
parity, so it goes to [Later](#later).

**A slash command in phase 1.** If the message's first word is `/<name>` and
`<name>` is in `GET /api/command`, the send is **refused**: "opencode slash
commands come in a later phase; type it in the terminal". Without this, the
command would reach the model as literal text, which a person would take for
a run command. Any other text beginning with `/` (a path) is sent as is.

**Files touched:**

- `cmd/muster-mirror/opencode_launch.go` (new): the launcher.
- `cmd/muster-mirror/main.go`: the subcommand, and the `-opencode-send` flag
  and its env var, documented in `docs/configuration.md`.
- `internal/opencodehost/client.go`: `Prompt`, `CreateSession`, `GetSession`,
  and `Commands` (the read the slash refusal needs).
- `internal/opencodehost/launch.go` (new): the record type, read and write,
  and the id formats.
- `internal/mirrorhost/opencode.go`: the record lookup in `resolve`,
  `SourceLauncher`, the title warning, stale-record cleanup, and
  `AgentSend`.
- `internal/mirrorhost/runtime.go`: `gatedSender`.
- `internal/mirrorhost/opencode_send.go` (new): `gatedSend`.
- `internal/mirrorhost/transcript.go`: the route ahead of `typeInto`.
- `internal/mirrorhost/client.go`: the hello.
- `internal/mirror/runtime.go`, `wire.go`, `hub.go`, `transcript.go`,
  `agentstate.go`: the spec fields, `OpenCodeSend`, `AgentSend`,
  `Since`/`MessageID`, `lookupAgent`, the idle gate, and the refusals.
- `internal/api/mirror_view.go`: the composer.
- `internal/api/send_queue.go`: the words.
- `internal/api/focus_tools.go:36`: "mirrored Claude Code windows" becomes
  "mirrored agent windows".
- `docs/design/opencode.md`, `docs/sending-and-step-up.md`, `docs/mirroring.md`.

**Ledgers that must change on purpose:**

- `internal/opencodehost/ledger_test.go:80`
  (`TestTheClientMakesOnlyGETsExceptTheMintAndTheReplies`) gains
  `prompt http.MethodPost path` (called only from `Prompt`) and
  `createSession http.MethodPost "/api/session"` (called only from the
  launcher's `CreateSession`). Its name and doc change to say so.
- `internal/mirror/runtime_ledger_test.go:126`: the new opencode sites
  (`hub.go` gatedSends, `transcript.go`'s idle gate, `send_queue.go`'s words,
  `agentstate.go`).
- `internal/mirrorhost/runtime_interface_test.go:63`
  (`TestOpenCodeIsViewedAndAnsweredButNeverSentTo`) becomes "viewed, answered,
  and sent to through the API only". It asserts `Caps.Send`, `GatedSend`,
  `Advertises` reading `OpenCodeSend` and no other flag, `gatedSender`
  present, and the paste guard **still** refusing.
- The `gatedSender` ⇔ `spec.GatedSend` pairing test from Hermes comes back
  (`646ca63e^:internal/mirrorhost/runtime_interface_test.go:45-52`).

**Test plan:**

- **Capture first.** Run a real `opencode serve` (v2.0.32+6ec5682) in a scratch
  home, as `asks-v2.0.32.sse` was captured (opencode.md, "Asks").
  - Record a `session.create` with an id, a `session.prompt` with an id on an
    idle session, the same id again, and a prompt with `delivery: "queue"` on
    a busy session.
  - Save the HTTP bodies and the SSE frames as
    `opencodetest/testdata/prompt-v2.0.32.{json,sse}`.
  - The fake (`opencodetest/fake.go:273-293`) gains the prompt, create and
    get-session routes from those frames, **not** from upstream's client test,
    whose fixture uses `data:{text}` where the wire says `payload`
    (`packages/client/test/promise.test.ts:1080-1089`).
- **Go unit, against the fake:**
  - a send posts once, with the derived id;
  - a retry with the same nonce posts the same id, and the fake returns the
    first admission;
  - busy, waiting and unknown are refused; a stale `Since` is refused;
  - unbound and title-contradicted windows are refused;
  - 404 and 401 map to their refusals;
  - a timeout is unconfirmed;
  - a slash name in the catalog is refused, and a path is sent;
  - the launcher: refusal cases, the record fields, the `exec` argv, a stale
    pid, a reused pid (start time), a foreign socket, a foreign server.
- **Red/green:** each refusal and ledger test is shown **red** on a mutant
  (the gate deleted, the flag ignored, `typeInto` reached) and green at HEAD.
  The PR reports the matrix. The title cross-check gets a positive control: a
  record whose title contradicts **must** raise the warning, and a send must
  still go to the recorded session.
- **e2e:**
  - `cmd/muster-mirror/e2eopencode.go:21-43` gains a launcher-bound window
    (an ops line `{"op": "bind", "pane", "session"}` writing a record the real
    resolver reads).
  - `e2e/tests/opencode-view.spec.ts` gains a spec that sends from the
    Rendered view, sees the bubble become the session's row, queues while
    busy and delivers on idle, and dismisses a Not confirmed.
  - The existing read-only assertions (`:135-137`, `:273-275`) stay, for a
    **title-matched** window.
- **Live, by hand, before calling it done:** start `muster-mirror opencode` in
  a real pane, send from the phone, and watch the TUI draw the message. That
  the TUI shows an API-posted prompt live is expected from its event
  subscription, but not established.

### Phase 2: slash/command menu

The host answers `TypeCatalog` for an opencode window with `GET /api/command
?location[directory]=<session's directory>`
(`internal/mirrorhost/catalog.go:597-620` gains a runtime branch through the
seam). A new source, `SourceRuntime`, joins `internal/mirror/catalog.go:67-75`.
`internal/api/mirror_complete.go:92` must **not** merge Claude Code's built-ins into an
opencode window's menu, which it does today.

A send whose first word names a listed command goes through `session.command
{name, text: args}` instead of a prompt, which lifts phase 1's refusal.

⚠ `session.command` answers 204 with no message id and no idempotency key, so
a command **cannot** be deduplicated. A command send that times out stays Not
confirmed, as a Claude paste does today. Its busy semantics ("immediately",
`delivery` optional) are to be captured before this phase is built.

- Ledgers: `ledger_test.go` adds `command`; `runtime_ledger_test.go` adds the
  catalog site.
- Tests: the fake gains `GET /api/command` and the command route, captured;
  unit tests cover the menu and routing; e2e covers the menu's entries on an
  opencode window and the absence of Claude's built-ins.

### Phase 3: model and cost/token display

**Built, except the cost**, and not quite as drafted below. As built:

- `opencodehost.Message` gains `model` and `tokens` (`usage.go`). The context
  is `input + cache.read + cache.write` of the NEWEST assistant message whose
  sum is above zero (a step still streaming has none), so the number means
  what a Claude Code view's does: what the model was sent for the latest
  request. opencode's `input` is net of the cache (measured in
  `testdata/prompt-v2.0.32.json`: a turn's two steps read 3533 + 1792, then
  91 + 5248). Output and reasoning are NOT added, as the TUI adds them: that
  would make the two runtimes' numbers mean different things.
- It does NOT ride `Event.Usage`: that would feed the tool-context ledger
  (`toolctx.go`), and an opencode step's usage arrives only once the step
  ends, after its tool results, so the ledger would attribute one call's
  growth to the next. It is `Msg.Context` (`mirror.AgentContext`: tokens,
  limit, model) on the view's stream instead, sent after the backfill and
  whenever it moves; the page reads it as the header's readout,
  "`<model> · 47k / 188k context`", with a tooltip saying how it is made.
- The limit is `limit.context` of the model (provider and id) in `GET
  /api/model`, read once per host every 10 minutes, and again at most once a
  minute for a model the list lacks. A model it does not name shows the count
  alone.
- The cost is not built.

The draft:

- `opencodehost.Message` (`internal/opencodehost/message.go:8-28`) gains
  `model`, `cost` and `tokens`.
- `Render` puts `Usage` on the event of the last assistant message: context is
  the sum of its tokens, as the TUI computes it.
- The host reads `limit.context` from `GET /api/model` (cached per host; when to re-read it is a phase-3 detail).
- The header's readout (`internal/ui/mirror_view.go:644-659`) becomes "<model>
  · 124k / 200k context · $0.42" for opencode. The cost is the session's sum
  (`session.usage.updated`), labelled as opencode's own estimate. Claude Code's
  readout is unchanged.
- Tests: the render fixtures gain usage frames from the phase-1 capture; a
  unit test pins the context sum against a hand-computed value; a fixture
  with distinct input, output, reasoning and cache values catches a dropped
  term.

### Phase 4: chief nudges

- `viewOnly` (`internal/api/chief_nudge.go:926`) becomes "the window cannot be
  sent to": not `Caps.Send`, not `AgentSend`, or the host did not advertise.
  A bound opencode window is then nudged through the queue, as Claude Code is.
- An unbound one keeps `EscalationViewOnly`
  (`internal/chiefnudge/chiefnudge.go:611`), whose words say why.
- `nudgeWindowKey` (`chief_nudge.go:593-603`) is unchanged.
- Tests: the existing nudge tests run on a bound and on an unbound opencode
  window. The bound one is a queued send carrying `NudgeLabel`; the unbound
  one is a notice.

### Later

- **Interrupt/stop:** `session.interrupt` behind its own host flag and gate.
- **Suggested replies:** `suggest.go:343`.
- **Native attachments:** `files` with `data:` URIs fetched by the host, at
  most 20 MiB each.
- **Turn errors:** `session.execution.failed` mapped to `TurnError`
  (`turnerror.go:134`), so the queue holds after a limit.
- **Measured per-tool context cost:** from `session.step.ended` tokens.

## Decided

The operator answered the open questions of the first draft:

1. **The launcher creates the session** (`POST /api/session` with an id it
   chose) before it starts the TUI, so a session takes a send before anything
   is typed in the terminal.
2. **The launcher is `muster-mirror opencode`**, a subcommand of the host
   binary. A short shell alias (such as `oc`) ships separately, with the
   operator's own host configuration.
3. **The title cross-check stays, as a warning and falsifier only.** It never
   decides a send's target, and never blocks a send
   ([above](#the-launcher-record)).
4. **A TUI/service version mismatch is refused** (`GET /api/info` against
   `opencode --version`), with a message naming both versions and the fix
   ([above](#the-launcher-record)).

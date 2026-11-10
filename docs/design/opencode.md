# opencode: what muster can rely on from its server

Notes for muster's opencode runtime (`internal/mirror/runtime.go`,
`internal/mirrorhost/opencode*.go`, `internal/opencodehost`): a rendered view,
and its asks (permission requests, questions) in the attention queue,
answerable through the API; and a send through the API to a pane the
launcher started ([below](#as-built-the-api-send); designed in
[opencode-first-class.md](opencode-first-class.md)). Facts are read from upstream source, <https://github.com/anomalyco/opencode>,
at commit `6ec5682` (opencode v2.0.32+6ec5682). They are not measured against
a running server.

## Credentials: can muster hold a dedicated, revocable token?

**Dedicated and terminal-free, yes. Individually revocable or scoped, no.**

### How a token is issued

Issuing a token takes two HTTP calls. This is what `opencode pair` does
(`packages/cli/src/commands/handlers/pair.ts`), and nothing needs a terminal.

1. **`POST /api/pair`** returns `{code, expires_in}`.
   - The code is single-use: 16 random bytes, base64url, held in the server's
     memory with a 5-minute TTL (`packages/server/src/pairing.ts`).
   - The route is under `/api`, so it needs the server's credential: the HTTP
     Basic password (user `opencode`), or an existing token.
2. **`GET /auth/connect/{code}`** redeems the code
   (`packages/server/src/handlers/server.ts`).
   - It is unauthenticated (`security: []` in `packages/protocol/openapi.json`).
     The code is the credential.
   - With `Accept: application/json` it returns `{token}`.
   - A browser navigation (an `Accept` with `text/html`) gets a cookie,
     `opencode_session_<port>`, instead.

### What the token is (`packages/server/src/auth.ts`)

- It is `"<expires>.<sig>"`. The signature is base64url
  HMAC-SHA256(key, expires), with `key = HMAC-SHA256(password,
  "opencode-session-v1")`.
- It lives 30 days (`SESSION_TTL_SECONDS`).
- It is **accepted anywhere the password is** ("Session tokens issued by pairing
  links are accepted anywhere the password is"). It goes in HTTP Basic as the
  password, with user `opencode`.

### What the token cannot do

- **No scope.** A token reaches the whole API: every session's prompt,
  interrupt, permission and form replies.
- **No per-token revocation.** Tokens are stateless; the server keeps no list.
  The only way to revoke one is to rotate the server password, which revokes
  every token at once ("rotating the password revokes every session"). That
  also disconnects every other paired client.
- **Self-renewal.** A holder of a valid token can call `POST /api/pair` and
  redeem a fresh 30-day token, indefinitely. So a client can renew itself
  without a terminal, but so can a leaked token, until the password is rotated.

### Consequences for muster's API phase

- Muster can hold a credential of its own, distinct from the service password.
  It belongs in a mode-600 file referenced by path, never in this repository.
- "Revoke muster's access" means "rotate the opencode service password". Say
  so in the operator docs; there is no narrower switch.
- Renew before the 30 days run out, and show the person when it is not
  working. muster-mirror renews within 10 days of expiry. When a renewal has
  failed, when the token is within 3 days of expiry, or when it has expired,
  the opencode window's rendered view shows the reason as its first notice
  row, and `muster-mirror -list -opencode` prints the same line. The lines are
  "opencode token expires in <n> — renewal failing: <reason>", "opencode token
  expires in <n> and has not been renewed", and "the opencode token expired
  <time>: pair this host again with `muster-mirror opencode-pair`"
  (`Credentials.Health`). A 401 from any authenticated read says "opencode
  refused the token: pair this host again with `muster-mirror opencode-pair`",
  a transport failure "the opencode server cannot be reached: <why>"; the next
  answered read clears either.
- Per-client revocation or a scoped token would need an upstream change. None
  exists at `6ec5682`.

### As built: pairing and renewal

- `muster-mirror opencode-pair` mints the host's first token. It reads the url
  and password from the service's registration file (`<state>/opencode/service-<channel>.json`,
  `{id, version, url, pid, password}`, mode 0600; `packages/client/src/effect/service.ts`
  calls it "the complete discovery contract"), refuses a url that is not
  loopback, and makes the two calls above. It writes `{url, token}` to
  `~/.muster/opencode-token`, 0600, and prints the token's fingerprint and
  expiry only.
- The daemon never reads the registration file: the server's url comes from
  the token file.
- The daemon renews the token within 10 days of its 30-day expiry, with the
  token itself, and writes the new one to the same file before using it. A
  failing renewal, a token within 3 days of expiry, and an expired token are
  shown in the view and in `-list -opencode` (above).
- The token file is the daemon's only source of a token: a token handed over
  any other way could not be renewed.
- `internal/opencodehost` sends only GETs, except the token mint that pairing
  and renewal make (`POST /api/pair`, then `GET /auth/connect/{code}`) and the
  answer to an ask (`reply`, below). A test reads the package's source and
  pins the set of requests it can send, and who calls the two writes.
  That is a claim about this package: other packages get the token only
  through `Credentials.Client()`, never the token itself.

### As built: the read-only view

- **Capabilities**: view yes, prompts yes since "Asks" below, send yes
  since "The API send" below (to a launcher-bound pane only)
  (`mirror.RuntimeSpec`). An
  opencode window is on the wire (`Window.Runtime`), keyed `opencode:<id>`
  (migration 0052 widens the four session-key CHECKs).
- **Pane to session**: the pane title, `OC | <title>` (a cut title ends in
  `…` and is a prefix match), against the server's ROOT sessions
  (`GET /api/session?parentID=null`); exactly one match or none. A
  title-matched pane is sent to only when the host VERIFIES the binding
  (a whole unique title, an open event stream, no contradicting `-s`, and the
  session re-read at the moment of sending:
  [Panes the launcher did not start](opencode-first-class.md#panes-the-launcher-did-not-start));
  any other is view-only, saying why. The launcher record below names the
  session deterministically
  ([designed](opencode-first-class.md#the-launcher-record)).
- **Context readout**: the newest completed step's `input + cache.read +
  cache.write`, its model, and the model's `limit.context` from `GET
  /api/model`, on the view's stream (`Msg.Context`;
  [phase 3](opencode-first-class.md#phase-3-model-and-costtoken-display)).
- **Turn errors**: an idle window's latest turn, read from its messages, as
  `Window.TurnError` (the queue's breaker holds on it).
- **Messages**: `GET /api/session/{id}/message` (newest first, reversed),
  re-read when the event stream names the session, and every tenth poll
  regardless. Rendered by `opencodehost.Render`: prompts, final text, tool
  calls by name and target, results by ok/error; never reasoning, a tool's
  input, or its output.
- **Status**: `session.status` on `GET /api/event`, seeded on each connect
  from `GET /api/session/active`; unknown while the stream is down.
- **What the person sees when there is no view**: the window's card carries
  the host's reason (`Window.AgentNote`): not paired, a refused or expired
  token, an ambiguous or unmatched title, a TUI on its own server. The token's
  health line (above) is the first row of a view that has one, and the card's
  note.
- **One stream per host**, opened on the first opencode pane, reconnecting
  (1 s doubling to 30 s; a refused token waits a minute).

## Asks: permission requests and questions

Read from upstream at `6ec5682` and **measured against a real `opencode serve`
(v2.0.32+6ec5682)** started in a scratch home with no credentials: the frames
below are what it sent, saved as `internal/opencodehost/opencodetest/testdata/asks-v2.0.32.sse`
(the location directory replaced), and the statuses are what it answered.

### What the server says

| ask | raised by (event) | settled by (event) | listed (pending only) | answered by |
|---|---|---|---|---|
| permission request | `permission.asked`: the `Permission.Request` — `{id "per…", sessionID, action, resources, save?, metadata?, source?, message?}` | `permission.replied` `{sessionID, requestID, reply}` | `GET /api/session/{id}/permission` | `POST /api/session/{id}/permission/{requestID}/reply` `{"decision": once \| always \| reject}` → 204; 404 once settled |
| form (the question tool's) | `form.created`: `{form: Form.Info}` — 🔴 its `sessionID` is NESTED under `data.form` | `form.replied` `{id, sessionID, answer}`, `form.cancelled` `{id, sessionID}` | `GET /api/session/{id}/form` | `POST /api/session/{id}/form/{formID}/reply` `{"answer": {"q0": "<value>"}}` → 204; 409 once settled; 400 for an answer the field cannot take |

- The question tool (`packages/core/src/tool/plugin/question.ts`) raises one
  form per call: `metadata.kind "question"`, one field per question, key
  `q<i>`, `title` its header, `description` the question, `type` `string` or
  `multiselect`, `options {value, label}` (value = label), `custom: true`.
- A permission request is not blocking on the server's side of the API
  (`POST …/permission` answers `{id, effect}` at once); the tool call that
  raised it waits for the reply. While it waits, `session.status` stays
  `busy`: there is no "waiting" status, so the host derives it from the ask.
- A subagent's ask names the CHILD session; `session.created` names its
  `parentID`.
- **An interrupted turn** (captured from a REAL turn of the server's default
  free model, `testdata/abort-v2.0.32.sse`): a pending **form** is cancelled
  (`form.cancelled`; `packages/core/src/form.ts:157-158`), but a pending
  **permission request** is dropped from the server's map with NO event
  (`permission.ts:251-262` only deletes it in an `Effect.ensuring`): the
  stream shows `permission.asked`, then `session.execution.interrupted`, and
  nothing that names the request. So the host clears a session's permission
  requests on the turn's end (`session.execution.succeeded`, `failed`,
  `interrupted` — one per busy period, `session/execution.ts`).
- **The v2 server publishes no `session.status`** at 6ec5682 (only its schema
  declares it; no publisher, and none in a real turn's frames). The turn's
  status is its lifecycle: `session.execution.started` busy, its terminal
  event idle — which the host now reads (`Event.Status`).
- Before this, `Event.SessionID` read only `data.sessionID`, so a
  `form.created` named no session at all: the host did not even re-read the
  view for it.

### As built

- **Host** (`internal/mirrorhost/opencode_asks.go`): the source keeps every
  pending ask from the stream (raised, then settled wherever it was answered),
  each child session's parent, and — once per stream open, for each matched
  root session — its pending lists (an ask raised while the stream was down).
  A matched window with a pending ask under its session is `waiting`, its
  `Pending` the newest ask (`mirror.PendingPermission`: summary
  "<action>: <resources>", tool the action, choices `once reject`;
  `mirror.PendingQuestion`: the first question, "(and N more questions)",
  tool `question`, its form's id always, and its option labels only when it
  is ONE single-select question of at most 4 options whose labels the wire
  would not change). A raised or settled ask sends the listing at once. The
  Rendered view answers the whole form by its id
  ([Asks from the view](opencode-first-class.md#asks-from-the-view)).
- **A stream gap holds the asks.** While the stream is down, and after a
  reconnect until each matched session's pending lists are re-read, the asks
  last known are reported as they are; the re-read is the one place an ask
  settled in the gap leaves. Clearing them at the drop would withdraw the
  owner's notification, and the same ask back within the queue's re-push
  window would never be pushed again.
- **An event line over 4 MiB** is discarded as it is read and the stream goes
  on (it used to end it); the host treats the skip as a gap (re-read).
- **Wire**: `Pending.Options` (one label per line) beside `Kind`, `PromptID`,
  `Choices`; `RuntimeSpec.AskKinds` keeps each runtime's kinds its own. An
  opencode Pending at its bounds is never larger than a Claude one, so the
  measured frame bound holds.
- **Server**: the attention queue draws a question as a *question* item and
  a permission request as a *session* item, so the push is the one a Claude
  Code window gets (`internal/api/attention_opencode.go`).
- **Answer**: `-interactive` hosts with an opencode token advertise
  `Msg.OpenCodeAnswer` — the same gate as an answer typed into Claude Code's
  picker (owner decision; an opt-in flag was considered and dropped). The hub re-checks the listed ask and the offered
  answer (`Hub.AnswerOpenCodeAsk`), the host re-checks the window and sends
  `ReplyPermission` (once or reject — "always" is refused before a byte is
  sent) or `ReplyForm` (the option's value, found by its label).

## As built: the API send

The design and the operator's decisions are in
[opencode-first-class.md](opencode-first-class.md). Upstream at `6ec5682`, and **measured
against a real `opencode serve` (v2.0.32+6ec5682)** in a scratch home: the
bodies are `internal/opencodehost/opencodetest/testdata/prompt-v2.0.32.json`
and the frames `prompt-v2.0.32.sse` (location directories replaced).

### What the server does

- `POST /api/session/{id}/prompt {id, text, delivery}` answers **200 on
  admission**, `{data: {id, sessionID, payload: {text}, delivery}}`; the turn
  runs after. **The same `id` again** on the same session answers 200 with the
  record stored FIRST: measured, a second post carrying other text came back
  with the first text, and the session held one user message. The same id on
  another session answers 409 (`ConflictError`); an unknown session 404
  (`SessionNotFoundError`).
- `delivery: "queue"` holds the message while the session is busy and takes it
  when the turn ends; the default, `"steer"`, would push it into the running
  turn. Measured: a second queued prompt sent mid-turn ran after the first, in
  the same execution.
- Events: `session.inbox.enqueued`, then `session.execution.started`, then
  `session.inbox.delivered` (the input is now a visible user message, its id
  the one posted).
- `POST /api/session {id, location: {directory}}` creates a session under the
  caller's id; `opencode -s <id>` then opens it.
- `GET /api/command` lists the built-ins (`init`, `review`) only for a request
  naming a directory (`location[directory]`); without one it answered none.
- `GET /api/info` names the service's version as `2.0.32+6ec5682`;
  `opencode --version` says `opencode v2.0.32+6ec5682`.

### The launcher

- `muster-mirror opencode [flags] [<directory>]` passes on `-s/--session`,
  `--prompt`, `--auto`, `-c/--continue`, `--standalone`, `--server`,
  `--log-level`, `--print-logs` and one directory; anything else is refused
  (exit 2), never forwarded.
- It **refuses a version mismatch** (exit 1): the TUI's `--version` against
  the running service's `GET /api/info`, naming both and the fix
  (`opencode service restart`), on EVERY path — bound or not — whenever the
  service answers. Upstream's TUI would restart the service to match, which
  can sign other clients out. A `--version` that fails or times out is said
  on stderr, and the TUI starts: only a known mismatch refuses.
- It creates the session first (a fresh `ses_` id in upstream's format, in
  the directory), or uses `-s <id>` (created when it does not exist, as
  opencode's own `-s` does; a subagent's session is not bound).
- It writes the record `~/.muster/opencode-launch/<digest of the tmux
  socket>/<pane number>.json` (0600, directory 0700): `{v, socket, pane, pid,
  pidStart, session, server, directory, created}`, its own pid and start time
  (`exec` keeps both), then execs `opencode --session <id> …`.
- Outside tmux, with `--standalone`, `--server` or `-c`, without a token, or
  when the service cannot be reached, it starts opencode **unbound** and says
  why on stderr.

### Binding a pane

- The daemon reads records for ITS tmux server only. A record binds its pane
  (`SourceLauncher`, `Window.AgentSend`) when it names the TUI the walk found
  (pid and start time), the token file's server, and a root session that
  server has. Otherwise the pane is title-matched, view-only.
- A record whose pid or start time is not the pane's TUI is stale and is
  removed; at most once a minute the daemon also removes records whose
  process is gone.
- **The title never decides a bound pane's session.** A title naming another
  session (the TUI switched) leaves the binding and raises a warning, the
  window's note: "This pane's title shows a different opencode session (…)
  than the one muster sends to; messages still go to the session shown
  here." A send goes to the recorded session, which is the one the Rendered
  view shows.

### Sending

- **Gates, each checked by muster and again by the host:** a fresh sign-in;
  an `-interactive` host started with `-opencode-send` and holding a token,
  advertised as `Msg.OpenCodeSend`; a launcher-bound window; the session idle
  since exactly the status time muster decided on (`Msg.Since`), with no ask
  pending. A queued message to a window that is not idle is decided on again.
- The host posts with `delivery: "queue"` under `Msg.MessageID`, derived by
  muster from the send's key: its nonce, or for a message held as
  may-have-landed, the nonce of its first try. So a **Send again** after
  "Not confirmed" posts the same id, and opencode admits it at most once.
  Upstream orders a session's messages by sequence, not by id.
- A message whose first word is `/<name>`, for a command the session's
  directory lists, is refused: a prompt would hand the model the command as
  text. A path (`/etc/hosts …`) is sent.
- Outcomes: 200 is sent; 404 "no longer exists"; a refused or unreachable
  post "not sent"; a post with no answer (a timeout, a dropped connection) is
  **unconfirmed**, never "not sent".
- The host logs each send's session id and message id, never the text.
- A Send again whose id opencode holds in ANOTHER session (409: the pane was
  started on another session since the first try) is refused as such, and
  muster forgets that first try: the next send is a new message. Only an
  explicit Send again reuses a held message's key; a queued message always
  goes under its own.
- ⚠ **Known: the send is advertised at connect.** `Msg.OpenCodeSend`, like
  `Msg.OpenCodeAnswer`, is part of the hello, so a host paired AFTER its
  muster-mirror connected advertises the send only from its next connection:
  restart muster-mirror after `muster-mirror opencode-pair`.
- `internal/opencodehost`'s request ledger now names its four writes: mint,
  reply, prompt (from `Prompt` only) and createSession (from `CreateSession`
  only).


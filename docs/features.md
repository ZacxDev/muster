# Features: what each one needs, and how to turn it on

muster's **core** works with nothing but the Compose stack (`deploy/docker-compose.yml`:
muster and its own Postgres) and, on each of your machines, tmux and
`muster-mirror`. Everything else on this page is **optional**: off until you set
it, and each one names exactly what it needs.

**Turning a feature on** is the same every time, on the server (the hub):

1. add the lines its section gives to `~/muster/.env` (the file
   [INSTALL.md](../INSTALL.md) made from `deploy/.env.example`, which already
   holds every optional line, commented out, grouped as below);
2. if the section names a **profile**, add it to `COMPOSE_PROFILES` in the same
   file (`COMPOSE_PROFILES=stt,s3` for two), or pass `--profile <name>`;
3. run `docker compose up -d --wait` in `~/muster`.

The Compose file forwards every optional setting from `.env`, empty unless you
set it, so you never edit `docker-compose.yml`. An empty value is the same as an
unset one, with one exception: `MUSTER_SESSION_STATE`, which is on unless `.env`
sets it, so there an empty value means **off** ([session state](#session-state)).
Secrets go in `.env` only.

**Checking a feature is on:** `muster doctor`, and on releases that have it,
the **Features** page (`/account/features`), list every feature as on, off or
misconfigured, and say what an off one needs. Many features also log their own
line when muster starts, saying whether they are on and why; each section below
quotes it where there is one. Read them with

```sh
docker compose logs muster | grep -E 'dictation:|file attachments:|web push:'
```

(or `docker compose logs muster | less` for everything muster logged at start).

## Contents

| feature | what it gives you | needs | default | how to enable |
|---|---|---|---|---|
| [Core](#core) | mirrored sessions, chat and Terminal views, typing into sessions, tasks, the Asks page, search, the installable app, the CLI and `muster doctor` | the Compose stack, tmux and `muster-mirror` on each machine, an HTTPS address (Tailscale) | on | [INSTALL.md](../INSTALL.md) |
| ↳ [Database](#database), [Mirror](#mirror), [Password sign-in](#password-sign-in), [Machine token](#machine-token) | the core's own parts | set up by INSTALL.md | on | INSTALL.md |
| [Push](#push) | phone notifications | VAPID keys (generated), HTTPS | on in INSTALL.md | 3 `.env` lines |
| [Dictation](#dictation) | a microphone on every message box | a speech-to-text service: bundled (CPU or GPU) or your API key | off (INSTALL.md asks) | `--profile stt` + 1 line |
| [Attachments](#attachments) | attach files in the message box; agents get signed links | an S3 store: bundled or your own | off | `--profile s3` + 2 lines |
| [Suggested replies](#suggested-replies) | reply chips when a session hands back to you | an OpenRouter key | off until the key is set, then off for each user until they turn them on (no database: until `MUSTER_SUGGEST=on`) | 1 line |
| [Focus](#focus) | a title and summary per session, searchable | an OpenRouter key | off until the key is set | the same line |
| [Agent ask tool](#agent-ask-tool) | a session asks you a question and waits for the answer | per-machine setup of `muster-mirror` and Claude Code | off | per machine |
| [Ask cards](#ask-cards) | Claude Code's multiple-choice questions answered from muster | a Claude Code hook per machine | off | per machine |
| [Session state](#session-state) | one push per stop; stalled / waiting rows on the Asks page | nothing | **on** in the Compose stack, upgrades included | 1 line to turn off |
| [Ask log](#ask-log) | every question an agent asks you, with your answer and how long it took, recorded for review | nothing | off | 1 line |
| [Pages](#pages) | agents publish pages — showcases, questions, forms, uploads, their own script in a sandbox — and ask before a link anyone can open goes live | `MUSTER_PUBLIC_URL`; attachments for uploads | off | 1 line |
| [Connectors](#connectors) | stored credentials; members' own OpenRouter keys | an encryption key | off | 1 line |
| [GitHub App](#github-app) | muster's GitHub identity; one-repository tokens for agents | a GitHub App you create, connectors | off | 4 lines |
| [Accounts](#accounts) | per-user sign-in, passkeys, invites, roles | **a GoTrue server you run**, a second port | off (one shared password) | 4 lines + an override file |
| [Browser flows](#browser-flows) | a queue of scripted browser jobs | **a runner you host** | the queue is on | nothing in `.env` |
| [Telemetry](#telemetry) | front-end monitoring, OpenTelemetry traces | a Faro collector or an OTLP endpoint | off | 1 line each |
| [Android](#android) | the native app's passkeys and push | the app (early: does little yet) | off | 2–3 lines |
| [Service token](#service-token) | another service writes to muster as the admin | a service that calls muster | off | 1 line |
| [Other settings](#other-settings) | timings, host icons | nothing | defaults | 1 line each |
| [Chief](#chief) | a fleet agent: what each session is on, routing work to sessions; home opens its chat | the `chief` profile, the connector key, a Claude subscription token or an OpenRouter key | off | `--profile chief` + 2 lines + a credential |
| **Advanced** | | | | |
| [Dispatch](#dispatch) | muster starts agents in their own pods | **Kubernetes**: muster deployed in a cluster, manifests you write | off | not in the Compose stack |
| ↳ [Agent gateway](#agent-gateway), [Claude Code agents](#claude-code-agents) | chat with agents; the `claude-code` agent kind | dispatch, an agent image | off | not in the Compose stack |
| [Chief session runtimes](#chief-session-runtimes) | a Claude model picker per chief session | a `claude-code` chief on Kubernetes | off | not in the Compose stack |
| [Chief nudges](#chief-nudges) | the chief nudges stalled sessions (shadow mode first) | the chief on Kubernetes, an OpenRouter key | off | not in the Compose stack |
| [Plugins](#plugins) | external systems (an inbox) reach your chief; reply drafts | **a plugin process you run**, the chief | off | 1 line + more |
| ↳ [Plugin viewers](#plugin-viewers), [Plugin tasks](#plugin-tasks), [Session routing](#session-routing), [Task routing](#task-routing), [PR mail routing](#pr-mail-routing), [Fleet reports](#fleet-reports), [Mail resume](#mail-resume) | what builds on plugins (session routing also works without them, for your own typed requests and tasks) | plugins, and mostly the chief | off | 1 line each |
| [Memo relay](#memo-relay) | cairn memos delivered to dispatched agents | **cairn you host**, dispatch | off | not in the Compose stack |
| [Approval router](#approval-router) | transcript links, router notifications, agent permission cards | **an external permission router you run** | off (standalone) | 3 lines |
| [Officers](#officers) | standing agents fed by an HQ queue | **HQ**, dispatch | off | not in the Compose stack |

## Core

Needs no optional variable and no extra service. `deploy/.env.example`'s
required block (`POSTGRES_PASSWORD`, `MUSTER_UI_PASSWORD`, `MUSTER_HOOK_TOKEN`,
`MUSTER_VERSION`) and `MUSTER_PUBLIC_URL` are all of it. You get:

- every tmux window on every machine as a card, a Claude Code window as a chat
  (the Rendered view) and any window in the Terminal view (Linux hosts)
  ([mirroring.md](mirroring.md));
- typing into a session from your phone, on machines whose `muster-mirror`
  runs with `-interactive` (INSTALL.md asks, per machine), behind a step-up
  check ([sending-and-step-up.md](sending-and-step-up.md));
- the session switcher, sidebar and search, context badges, the task board
  ([tasks.md](tasks.md)) and the Asks page ([attention.md](attention.md));
- the installable app, the `muster` CLI and `muster doctor` ([cli.md](cli.md));
- the server switcher, for moving between several muster servers in one
  browser: no setting, the list is kept in the browser
  ([push.md](push.md#several-servers-the-server-switcher)).

Not in the core, and set up separately: [the agent ask tool](#agent-ask-tool)
and [ask cards](#ask-cards).

The core's parts, each with its own line in the boot log:

### Database

Postgres, which the Compose stack runs itself (the `pgdata` volume). Required:
muster refuses to start without `DATABASE_URL`, which the compose file sets.
**Check:** the `persistence:` line, and `/readyz` answering 200.

### Mirror

Mirrored sessions: every tmux window on every machine whose `muster-mirror`
holds a mirror token ([mirroring.md](mirroring.md)). The compose file sets
`MUSTER_MIRROR=1`; each machine is connected in INSTALL.md Step 7.
**Check:** `mirrored sessions: ENABLED (…)`, and `muster doctor`'s
`mirror-hosts` and `this-host`.

### Password sign-in

One shared password for the web UI, `MUSTER_UI_PASSWORD` (12 characters or
more; shorter, every page is refused and the container is unhealthy).
[Accounts](#accounts) replaces it with per-user sign-in. [auth.md](auth.md).
**Check:** `human web tier: CONFIGURED …`.

### Machine token

`MUSTER_HOOK_TOKEN`, the secret every `muster-mirror`, hook and the CLI
presents; it acts as the admin. Required by the compose file, and muster
refuses the `.env.example` placeholder. **Check:** `machine hook tier: ENFORCED …`.

## Optional features

### Push

Web Push notifications to your phone and browsers: a session waiting on you, an
ask, a task ready for review. [push.md](push.md).

- **Needs:** a VAPID key pair (muster generates it) and an HTTPS address (a
  service worker needs one). [INSTALL.md](../INSTALL.md) sets this up.
- **`.env`:** all three or none.

  ```sh
  docker compose run --rm --no-deps muster vapid generate >> .env   # appends the two keys
  ```

  then add `MUSTER_VAPID_SUBJECT=mailto:you@example.com`.
  `MUSTER_PUSH_EXTRA_HOSTS` adds your own push hosts (a self-hosted ntfy).
- **Profile:** none.
- **Check:** `web push: ENABLED`. Then **Account → Notifications → Send a
  test notification**.

### Dictation

A microphone on every message box, with a live waveform while it records: ✓
puts the transcript in the box for you to check before Send, ↑ transcribes and
sends it through the box's own Send. [dictation.md](dictation.md) has the
protocol and limits.

- **Needs:** a speech-to-text service that answers OpenAI's
  `POST /v1/audio/transcriptions`. Three ways:
  1. **`stt` profile (bundled, CPU).** [speaches](https://github.com/speaches-ai/speaches),
     a faster-whisper server, pinned by digest. No GPU; the model
     (`Systran/faster-whisper-small` unless you choose another) downloads on
     its first start, about 0.5 GB. Allow 20 GB of disk for a local model.
  2. **`stt-cohere` profile (bundled, GPU).** muster's own image,
     `ghcr.io/zacxdev/muster-stt-cohere`, around Cohere's transcription model:
     more accurate. Needs an NVIDIA GPU with about 6 GB free, the NVIDIA
     container toolkit, amd64, and a Hugging Face token from an account that
     accepted the model's licence.
  3. **Your own OpenAI-compatible API key.** No container; recordings go to
     that provider.
- **`.env`, option 1** (one line, plus the profile):

  ```sh
  COMPOSE_PROFILES=stt
  MUSTER_STT_TOKEN=<a random secret>    # e.g. openssl rand -hex 32
  ```

  With only the token set, muster points itself at the `stt-whisper` service
  (`MUSTER_STT_URL=http://stt-whisper:8000`) and both sides use the same model.
  Set `MUSTER_STT_MODEL` to change the model for both.
- **`.env`, option 2:**

  ```sh
  COMPOSE_PROFILES=stt-cohere
  MUSTER_STT_URL=http://stt-cohere:8000
  MUSTER_STT_TOKEN=<a random secret>
  MUSTER_STT_MODEL=CohereLabs/cohere-transcribe-03-2026   # ignored by the server; keeps the log line accurate
  HF_TOKEN=<your Hugging Face token>
  ```

- **`.env`, option 3:** `MUSTER_STT_URL=https://api.openai.com`,
  `MUSTER_STT_TOKEN=<your key>`, `MUSTER_STT_MODEL=whisper-1`. No profile.
- **Check:** `dictation: ON — MUSTER_STT_TOKEN is set; …`, which names the URL
  and model. `muster doctor`'s `dictation` check transcribes a bundled voice
  sample through muster. Off, no microphone is drawn.

### Attachments

Attach files in the message box (the paperclip, paste, or drop). The agent gets
a short-lived link signed by muster, on muster's own address.
[uploads.md](uploads.md).

- **Needs:** an S3-compatible store and `MUSTER_PUBLIC_URL` (every link is
  written on it; INSTALL.md sets it). The `s3` profile bundles one:
  [Silo](https://github.com/pgsty/silo), pgsty's maintained fork of MinIO
  (upstream MinIO is archived and no longer publishes images), pinned by
  digest, not published on the host. It creates the bucket and a lifecycle
  rule (expire `uploads/` after 8 days) when it starts.
- **`.env`, with the bundled store** (two lines, plus the profile):

  ```sh
  COMPOSE_PROFILES=s3
  S3_STORE_SECRET_KEY=<a random secret, 8+ characters>    # the store's root password, and muster's secret key
  MUSTER_UPLOAD_SIGNING_KEY=<32+ random bytes>            # e.g. openssl rand -base64 48
  ```

  With the store's secret set, muster uses `http://s3:9000`, bucket
  `muster-uploads`, access key `muster` and that secret — the store's own —
  for whichever `MUSTER_S3_*` setting `.env` leaves unset.
- **`.env`, with your own store:** no profile and **no**
  `S3_STORE_SECRET_KEY`; set `MUSTER_S3_ENDPOINT`, `MUSTER_S3_BUCKET`,
  `MUSTER_S3_ACCESS_KEY`, `MUSTER_S3_SECRET_KEY` (and `MUSTER_S3_REGION` if not
  `us-east-1`) plus the signing key. Nothing is filled in for you here: one
  left out is reported as `OFF (MISCONFIGURED)` naming it, never replaced by
  the bundled store's. Do not leave `S3_STORE_SECRET_KEY` in `.env` beside your
  own `MUSTER_S3_ENDPOINT`: the compose file then refuses the pair (attachments
  `OFF (MISCONFIGURED)`, the bucket naming `S3_STORE_SECRET_KEY`) rather than
  send the bundled store's key to your endpoint. The key's
  policy and the lifecycle rule are in [uploads.md](uploads.md#configuration).
- **Limits:** `MUSTER_UPLOAD_MAX_BYTES` (25 MiB), `MUSTER_UPLOAD_LINK_TTL` (24h).
- **Check:** `file attachments: ON — files go to MUSTER_S3_ENDPOINT=…`. A
  partial set says `OFF (MISCONFIGURED)` and names what is missing; it never
  stops muster. A line starting `uploads: the bucket answered at boot` means
  muster reached the bucket.

### Suggested replies

Up to three reply chips when a mirrored Claude Code session hands back to you;
a tap fills the box, never sends. [suggested-replies.md](suggested-replies.md).

- **Needs:** an [OpenRouter](https://openrouter.ai) API key. Suggestions are
  generated on the session owner's key. With one shared password everyone is
  the admin, whose key is this one; members of a multi-user install store
  their own (that needs [connectors](#connectors)).
- **`.env`:** `MUSTER_AGENT_OPENROUTER_KEY=<your key>`. Optional:
  `MUSTER_SUGGEST_MODEL` (default `openai/gpt-6-luna`), `MUSTER_SUGGEST=off`
  to turn them off for everyone, `MUSTER_SUGGEST=on` to turn them on for
  everyone on a server with **no database** (there is no per-user switch
  there; with a database it changes nothing).
- **Default:** off until you set `MUSTER_AGENT_OPENROUTER_KEY` in `.env`:
  without a key no suggestion can be made, and muster reports the feature as
  off, needing that variable. With the key set, suggestions are still **off
  for each user until they turn them on**: a user who has never chosen gets
  no chips and no model call is made for them. With no database nobody can
  choose, so they are off for everyone unless `MUSTER_SUGGEST=on`.
- **Check:** `muster doctor` or the Features page; then turn them on in
  **Account → Suggested replies** (the per-user switch: on, and off again),
  and chips appear above the box when a session finishes a turn with its
  Rendered view open.

### Focus

A short title and a sentence or three per session saying what it is doing now;
search finds a session by it, and the chief reads it. A session's first good
focus title also becomes its stable [name](focus.md#a-sessions-name), which you
can change with **Rename…**. [focus.md](focus.md).

- **Needs:** the same OpenRouter key as [suggested replies](#suggested-replies).
  Off until you set `MUSTER_AGENT_OPENROUTER_KEY` in `.env`; muster then
  reports it as off, needing that variable.
- **`.env`:** `MUSTER_AGENT_OPENROUTER_KEY=<your key>`. Optional:
  `MUSTER_FOCUS_INTERVAL` (15m; `0` turns it off), `MUSTER_FOCUS_RETENTION`,
  `MUSTER_FOCUS_MODEL` (default `scaledown/summarize`; any OpenRouter model
  that keeps to the two-line `Title:` / `Description:` format).
- **Check:** `muster doctor` or the Features page; after a pass (every 15
  minutes), search (`/`) finds a session by its focus title.

### Agent ask tool

A Claude Code session asks you a question (`attention_ask`) and waits for your
answer (`attention_wait`) instead of stopping; the question shows on the Asks
page and your phone. [attention.md](attention.md#mirrored-claude-code-sessions).

- **Needs, on each machine:** `muster-mirror` serving its local MCP endpoint,
  and Claude Code configured to use it. Nothing on the server.
- **Steps, on each machine** (INSTALL.md Step 11 does these when you ask for it):
  1. Turn on the endpoint: add `MUSTER_MIRROR_MCP_LISTEN=127.0.0.1:47823` to
     `~/.config/muster-mirror/env` (INSTALL.md: `mi_save ASK_TOOLS yes`, then
     re-run Step 7's last block) and restart `muster-mirror`. On its first start
     it writes a secret to `~/.config/muster-mirror/mcp-secret` (mode 600).
  2. Register it with Claude Code, for this user:

     ```sh
     claude mcp add-json --scope user muster '{"type":"http","url":"http://127.0.0.1:47823/mcp","headers":{"Authorization":"Bearer ${MUSTER_MIRROR_MCP_SECRET:-}","X-Muster-Pane":"${TMUX_PANE:-}","X-Muster-Tmux":"${TMUX:-}"}}'
     ```

     The single quotes matter: Claude Code expands the `${…}` itself, in each
     session.
  3. Export the secret in the shell tmux starts (`~/.zshrc` or `~/.bashrc`):

     ```sh
     export MUSTER_MIRROR_MCP_SECRET="$(cat ~/.config/muster-mirror/mcp-secret 2>/dev/null)"
     ```

- **Check:** the host's log (`journalctl --user -u muster-mirror` on Linux)
  says `muster-mirror: MCP endpoint for this host's Claude Code sessions at
  http://127.0.0.1:47823/mcp`, or `the MCP endpoint is OFF, … <reason>`; in a
  **new** Claude Code session started in tmux, `/mcp` lists `muster` connected
  with its tools.

### Ask cards

When Claude Code shows a multiple-choice question (`AskUserQuestion`), the
session's Rendered view shows the same question as a form you can answer from
your phone. [mirroring.md](mirroring.md#answering-claudes-questions).

- **Needs, on each machine:** a Claude Code `PreToolUse` hook. INSTALL.md's
  `muster-mirror` service already runs with `MUSTER_MIRROR_WRITE_HOOK_CONFIG=1`,
  which the hook relies on.
- **Steps:** add this entry to `~/.claude/settings.json` under `hooks`,
  keeping any hooks already there (append it to an existing `PreToolUse`
  list):

  ```json
  "PreToolUse": [
    {"matcher": "AskUserQuestion",
     "hooks": [{"type": "command", "command": "muster-mirror ask", "timeout": 10}]}
  ]
  ```

  `muster-mirror` must be on the `PATH` Claude Code runs hooks with (INSTALL.md
  puts it in `~/.local/bin`).
- **Check:** ask a Claude Code session in tmux to ask you a multiple-choice
  question; the Rendered view shows it as a form.

### Chief

Your fleet agent. Ask it what every session and agent is working on and who is
on what, and have it route a task, a request or an idea to the session already
on it. Plugin messages and your sessions' `chief_ask` questions go to it, and
home opens its chat. [chief.md](chief.md).

- **Needs:** the `chief` profile (a [claude-code-agent](provisioner.md#agent-kinds)
  container that muster configures through a shared volume, with no Docker
  socket), the [connector store](#connectors)'s key, and **one credential**:
  - a Claude subscription token (`claude setup-token`), or
  - an [OpenRouter](https://openrouter.ai) key, through muster's LLM proxy, on
    `deepseek/deepseek-v4.1-flash` unless `MUSTER_CHIEF_LLM_MODEL` names
    another model. 🔴 **muster adds no budget of its own: set a credit limit on
    the key itself** at [openrouter.ai/settings/keys](https://openrouter.ai/settings/keys).
    That limit is the only spending cap.
- **`.env`:**

  ```sh
  COMPOSE_PROFILES=chief                 # add it to the one COMPOSE_PROFILES line
  MUSTER_CHIEF_RUNTIME=compose
  MUSTER_CONNECTOR_ENCRYPTION_KEY=<openssl rand -base64 32>   # if not set already
  MUSTER_CHIEF_CLAUDE_TOKEN=<claude setup-token>   # OR MUSTER_AGENT_OPENROUTER_KEY=<key>
  ```

  Then `docker compose up -d --wait`, and **Account → Chief → Turn the chief
  on**. The subscription token wins when both are set.
- **Routing work to sessions** also needs [session routing](#session-routing):
  `MUSTER_SESSION_ROUTING=1` (not plugins: an idea you type in the chief's chat
  and your tasks are routed without them). Account → Chief says whether it is on.
- **Summaries** of what each session is doing need no key of their own. With
  [focus](#focus) on, the chief also reads each session's focus title.
- **Default:** off. Without the profile and the runtime, Account → Chief says
  how to turn it on, and a session's `chief_ask` is told to use
  `attention_ask` instead.
- **What it does not do in the Compose stack:**
  - one chief per install: the admin's (user 1). Other users' Account page says
    so;
  - no other agents: agent dispatch still needs [Kubernetes](#dispatch);
  - no Terminal view (its output is in `docker compose logs chief`);
  - no `muster chief replace`;
  - amd64 hosts only: the `claude-code-agent` image has no arm64 build;
  - its **network is not confined**: its container reaches the internet. It
    is on a network of its own with muster, and cannot reach Postgres, S3 or
    STT.

  Details are in [chief.md](chief.md#on-the-docker-compose-install).
- **Turning it off** stops the Claude Code CLI in the container. The container
  stays up and idle.
- **Removing it:** taking `chief` out of `COMPOSE_PROFILES` does NOT stop its
  container, even with `--remove-orphans`. Turn it Off, run
  `docker compose --profile chief rm -sf chief chief-init`, take its lines out of
  `.env`, and delete its volumes ([chief.md](chief.md#on-the-docker-compose-install)
  has the commands).
- **Check:** `chief runtime: COMPOSE (MUSTER_CHIEF_RUNTIME=compose) — …` in
  `docker compose logs muster`; Account → Chief reads **On.** and names its
  credential; `muster doctor` lists `chief` on. With the chief on and the
  container down, Account → Chief says the container is not answering.
- **On Kubernetes** the chief runs in its own pod instead, through the
  [agent provisioner](#dispatch) ([chief.md](chief.md)).

### Session state

Every stop of a session becomes an episode, pushed **once** (naming the
session, never quoting it), and open stops are listed as rows of the Asks page's
**Waiting** group ([attention.md](attention.md#stalled--waiting-session-state)).

- **On by default in the Compose stack.** The compose file sets
  `MUSTER_SESSION_STATE=1` unless `.env` says otherwise. (The server binary on
  its own defaults to off.)
- **Upgrading switches it on.** An install made before this default, upgraded
  with the new `docker-compose.yml`, moves to one push per stop at its next
  start. To keep the old notifications, put `MUSTER_SESSION_STATE=0` in `.env`.
- **Needs:** nothing beyond the core: only the database it already has. Its
  stall classifier runs only where [chief nudges](#chief-nudges) run.
- **Turning it off:** `MUSTER_SESSION_STATE=0` in `.env` (an empty value does
  the same; only `1`, `true`, `yes` or `on` is on). Off, there are no episodes
  and no stalled / waiting rows, and notifications are as before session
  state: a push when a window starts waiting, and the chief's stall notice,
  instead of one push per stop.
- **Check:** `session state: ON (MUSTER_SESSION_STATE=1) — …`; off, the line
  reads `session state: OFF — MUSTER_SESSION_STATE is unset` (for `0` too).

### Ask log

Every question an agent asks you — a plain-text question at the end of a
turn, an `AskUserQuestion` picker, an `attention_ask`, an opencode form or
permission request — is recorded with your answer, who gave it and how long
it took ([asks.md](asks.md)). Telemetry only: nothing is answered for you.

- **Needs:** nothing beyond the core's database.
- **Turn it on:** `MUSTER_ASK_LOG=1` in `.env`. `MUSTER_ASK_LOG_TEXT_TTL`
  (default `720h`) is how long question and answer text is kept;
  `MUSTER_ASK_LOG_CLASSIFY=1` also stores the stall classifier's scores (it
  needs [chief nudges](#chief-nudges)' classifier).
- **Privacy:** redacted on the way in, yours only, text cleared after the TTL.
- **Check:** `ask log: ON (MUSTER_ASK_LOG=1) — …`; off, the line reads
  `ask log: OFF — MUSTER_ASK_LOG is unset; …`.

### Pages

Agents publish small pages for you — a showcase, questions, a form, file
uploads, an approval, or their own script in a sandbox — and ask you before a
link that anyone can open goes live. What viewers send comes back to the agent
as data, and you are notified. [pages.md](pages.md).

- **Needs:** the core's database, and `MUSTER_PUBLIC_URL` (links are written on
  it). An `upload` block needs [attachments](#attachments).
- **Turn it on:** `MUSTER_PAGES=1` in `.env`.
- **Safety:** a page is served sandboxed, with no network; it never runs as
  muster. You approve every link an agent asks for; you can revoke any link.
- **Check:** `shareable pages: ON (MUSTER_PAGES=1) — …`; off, the line reads
  `shareable pages: OFF — MUSTER_PAGES is unset`.

### Connectors

A sealed store of HTTP credentials (and read-only Postgres connectors) that
agents use through muster's proxy without seeing the secret; it also holds each
member's own Claude token and OpenRouter key. [connectors.md](connectors.md).

- **Needs:** a 32-byte key. Agents using connectors need [dispatch](#dispatch);
  the store itself and members' credentials do not.
- **`.env`:** `MUSTER_CONNECTOR_ENCRYPTION_KEY=<head -c32 /dev/urandom | base64>`.
  Keep it: losing it loses every stored secret. Optionally
  `MUSTER_DIAL_DENY_PRIVATE=1` refuses postgres connectors to loopback and
  private addresses
  ([connectors.md](connectors.md#refusing-private-addresses)).
- **Check:** `connector store: ENABLED — …`; the Connectors tab appears.

### GitHub App

muster's GitHub identity for its admins, and agent tokens limited to one
repository for an hour. [github-app.md](github-app.md).

- **Needs:** a GitHub App you register (github-app.md walks through it) and
  [connectors](#connectors). The tokens are for [dispatched agents](#dispatch).
- **`.env`:** `MUSTER_GITHUB_APP_ID`, `MUSTER_GITHUB_APP_PRIVATE_KEY` (the PEM
  file's content on one line, in double quotes, with `\n` for each line break),
  `MUSTER_GITHUB_APP_ACCOUNTS` (`<login>:<id>,…`), and optionally
  `MUSTER_GITHUB_APP_ACCESS=read`.
- **Check:** `github app: ON — …`.

### Accounts

Per-user accounts instead of one shared password: passkeys, invites, admin and
member roles, kept apart by row-level security. [auth.md](auth.md).

- **Advanced. Needs a [GoTrue](https://github.com/supabase/auth) server you run
  yourself**, reachable from the muster container, issuing tokens as auth.md
  describes; the Compose stack does not include one. It also needs a second
  listener, the only one that serves browsers in this mode.
- **`.env`:**

  ```sh
  MUSTER_AUTH_MODE=gotrue
  MUSTER_GOTRUE_URL=http://<your gotrue>:9999
  MUSTER_GOTRUE_JWT_SECRET=<GoTrue's HS256 secret, 32+ characters>
  MUSTER_BROWSER_PORT=8106
  ```

- **Publish the browser port** with a `docker-compose.override.yml` beside
  `docker-compose.yml` (Compose reads it automatically), and point
  `tailscale serve` at 8106 instead of 8105:

  ```yaml
  services:
    muster:
      ports:
        - "127.0.0.1:8106:8106"
  ```

- **Then:** `docker compose exec muster /muster-server invite --claim-admin`
  prints the link that binds your GoTrue login to the admin.
- **Check:** `human web tier: GOTRUE (MUSTER_AUTH_MODE=gotrue) — …` and
  `gotrue: CONFIGURED`. This mode is not exercised by the Compose stack's own
  tests.

### Browser flows

Named, scripted jobs that run in a real browser you host, each approved by its
owner. [browser-flows.md](browser-flows.md).

- **Needs: a runner you host.** This repository has none, only the protocol.
  Without one, queued jobs expire.
- **`.env`:** nothing turns it on: the queue exists whenever there is a
  database. `MUSTER_FLOW_AGENT_DENY_WORDS` (your host, cluster and project
  names, comma-separated) keeps them out of agent-written flow text, and
  silences the boot warning about it.
- **Check:** open `/flows` (there is no menu link); a runner token is minted
  there.

### Telemetry

Front-end monitoring through Grafana Faro, and OpenTelemetry traces of opening a
session, browser to host. [telemetry.md](telemetry.md).

- **Needs:** a Faro collector, or an OTLP/HTTP endpoint (Tempo, an
  OpenTelemetry Collector), that you run.
- **`.env`:** `MUSTER_FARO_UPSTREAM=<your faro.receiver collect URL>` (through
  muster's own relay) or `MUSTER_FARO_URL`, with `MUSTER_FARO_API_KEY`;
  `OTEL_EXPORTER_OTLP_ENDPOINT=http://<collector>:4318` for traces, and
  optionally `MUSTER_TRACE_SAMPLE_RATIO`, `OTEL_SERVICE_NAME`,
  `OTEL_RESOURCE_ATTRIBUTES`, `OTEL_EXPORTER_OTLP_HEADERS`.
- **Check:** without an endpoint nothing is traced and nothing is logged about
  it; a bad tracing setting logs `telemetry: tracing stays off: …`.

### Android

The native Android app ([muster-android](https://github.com/ZacxDev/muster-android))
using this site's passkeys, and push through your own ntfy. ⚠ The app is at an
early stage and does little yet.

- **`.env`:** `MUSTER_ANDROID_PACKAGE` and `MUSTER_ANDROID_CERT_SHA256`
  (together), optionally `MUSTER_ANDROID_ALLOW_PIN_KEYS=1`,
  `MUSTER_PUSH_EXTRA_HOSTS` and, behind a single sign-on proxy,
  `MUSTER_ANDROID_SSO_ORIGINS`. [configuration.md](configuration.md#muster-server-optional-features).
- **Check:** `android passkeys: /.well-known/assetlinks.json serves …`.

### Service token

A service-to-service secret that acts as the admin, for another service of yours
that writes to muster (each request also names its actor in `X-Muster-Actor`).

- **`.env`:** `MUSTER_SERVICE_TOKEN=<32+ random characters>`.
- **Check:** `service-to-service tier: CONFIGURED …`; unset, those routes answer
  503 and the line says `DISABLED (fail-closed)`.

### Other settings

Tuning, none of which needs anything else. Each is one `.env` line; defaults
and ranges are in [configuration.md](configuration.md).

| variable | what it changes |
|---|---|
| `MUSTER_STEPUP_WINDOW` | how recent a sign-in must be to type into a session (12h) |
| `MUSTER_SEND_QUEUE_IDLE` | how idle a session must be before a queued message is sent (30s) |
| `MUSTER_TASK_REAP_AFTER` | when an idle task is tagged `stale` (168h) |
| `MUSTER_AGENTS_IDLE_AFTER` | when a session folds under **Idle** in the sidebar (14d) |
| `MUSTER_HOST_ICONS` | an icon per machine, e.g. `laptop=laptop,tower=server` |
| `MUSTER_LIVE_HEARTBEAT` | the live streams' heartbeat (15s) |
| `MUSTER_PUBLIC_URL`, `MUSTER_SECURE_COOKIES`, `MUSTER_HTTP_PORT`, `MUSTER_BIND` | the address and port (INSTALL.md sets them) |

## Advanced features

These need infrastructure you host yourself. Most of them build on
[dispatch](#dispatch), which needs Kubernetes.

### Dispatch

muster starts a coding agent on a task in its own pod, talks to it, stops and
deletes it. [provisioner.md](provisioner.md).

- **Needs Kubernetes.** The provisioner uses in-cluster credentials only, so
  muster itself must be deployed inside the cluster, with RBAC and manifests you
  write (the repository ships the image, no manifests), plus an agent image:
  `images/claude-code-agent`, or a `gateway`-kind image you supply.
- **Not available in the Compose stack.** `docker-compose.yml` does not forward
  `MUSTER_AGENT_PROVISIONER`, `MUSTER_AGENT_GATEWAY`, `MUSTER_AGENT_KINDS` or the
  rest of the agent settings ([configuration.md](configuration.md#muster-server-agent-provisioning)),
  since they cannot work there. On Kubernetes, agents are started by the
  chief's `agent_dispatch` or by
  [`muster agent create`](provisioner.md#creating-an-agent-without-the-chief).
- **Check:** `agent provisioning: LIFECYCLE …` instead of `UNWIRED`.

### Agent gateway

Chat with a dispatched agent, and delivery of its first turn (the kickoff):
`MUSTER_AGENT_GATEWAY=hooks-sha256` with the runtime's config and install
script, for a `gateway`-kind agent image you supply
([provisioner.md](provisioner.md#the-two-switches)). Needs [dispatch](#dispatch);
not available in the Compose stack. **Check:** `agent provisioning CHAT: WIRED …`.

### Claude Code agents

The `claude-code` agent kind: the Claude Code CLI in a pod (`images/claude-code-agent`),
on Claude subscription tokens, with worker pools and the jobs API
([provisioner.md](provisioner.md), [jobs.md](jobs.md)). `MUSTER_AGENT_KINDS=gateway,claude-code`,
`MUSTER_AGENT_CC_IMAGE`, `MUSTER_AGENT_CC_ACCOUNTS` and a
`MUSTER_AGENT_CC_TOKEN_<NAME>` per account. Needs [dispatch](#dispatch); not
available in the Compose stack.

### Chief session runtimes

A picker for the runtime (and Claude model) of each chief chat session:
`MUSTER_CHIEF_SESSION_RUNTIMES=claude-code`, with a `claude-code` chief
([chief.md](chief.md#sessions-on-more-than-one-runtime)). Not available in the
Compose stack. **Check:** `chief session runtimes: ON (…)`.

### Chief nudges

Your chief finds sessions that stopped short, and either nudges them with your
own words or points you at them. Ships in **shadow mode**: decisions are
recorded on the Asks page's Chief nudges tab, nothing is sent.
[chief-nudges.md](chief-nudges.md).

- **Needs** the [chief](#chief) on Kubernetes (in-cluster [dispatch](#dispatch)),
  and the OpenRouter key (`MUSTER_AGENT_OPENROUTER_KEY`) for its stall
  classifier.
- **Not available in the Compose stack**: `MUSTER_CHIEF_NUDGES` and the other
  `MUSTER_CHIEF_NUDGE_*` settings are not forwarded, like the rest of the
  agent plane. In a Kubernetes deployment, `MUSTER_CHIEF_NUDGES=1` turns it on
  and `MUSTER_CHIEF_NUDGE_LIVE_OWNERS=<user ids>` acts on it for those users;
  the knobs are in chief-nudges.md.
- **Check:** `chief nudges: ON in SHADOW (MUSTER_CHIEF_NUDGES=1) — …`.

### Plugins

Out-of-process plugins bring another system's messages (an inbox, ClankMail)
to your chief and send the replies you approve. [plugins.md](plugins.md).

- **Needs a plugin process you run** (this repository has the ClankMail one,
  `muster-clankmail`), and the [chief](#chief) to act on what arrives: without
  an agent gateway, messages queue and the log warns.
- **`.env`:** `MUSTER_PLUGINS=1`. The features below build on it; their
  grammar is in [configuration.md](configuration.md#muster-server-optional-features).
- **Check:** `plugins: ENABLED (MUSTER_PLUGINS=1) — …`; `/plugins` (no menu
  link) mints plugin tokens.

### Plugin viewers

A **viewer** plugin (such as a preview gateway) gets a chip on your session
views that opens its own origin, signed in as you, through a one-time
[hand-off code](plugins.md#viewer-hand-off).

- **Needs** [plugins](#plugins), and an admin's allowlist of the origins a
  viewer may be given: `MUSTER_PLUGIN_ORIGINS=https://*.<zone>` (its grammar is
  in [configuration.md](configuration.md#muster-server-optional-features)). A
  `Host` under it is refused on muster's own listeners.
- **Check:** `plugin origins: ON …`; `/plugins` offers a Viewer checkbox; a
  viewer plugin's [chip](plugins.md#the-session-view-chip) is on a mirrored
  window's Rendered and Terminal views.

### Plugin tasks

A plugin files one task per upstream record (`POST /plugin/v1/tasks`).
`MUSTER_PLUGIN_TASK_NAMESPACES=<tag namespaces>`, with plugins on; no chief
needed. **Check:** `plugin tasks: ON …`.

### Session routing

The chief hands one of your mirrored sessions an idea you typed in its chat
([plugins.md](plugins.md#routing-your-own-request-to-a-session)), one of your
tasks, or a plugin message, and relays the session's answer
([plugins.md](plugins.md#routing-a-message-to-a-session)).
`MUSTER_SESSION_ROUTING=1`, with the [chief](#chief). It does **not** need
plugins: without them it routes your typed requests and tasks, and no plugin
message. `MUSTER_ROUTE_RELAY_ALLOW` lists the senders whose answers go out
without your approval. **Check:** `session routing: ON (…)` (`… WITHOUT
plugins` when they are off).

### Task routing

The chief routes a task a plugin filed: to the Asks page, or to the agent
working it ([plugins.md](plugins.md#routing-a-task-to-the-chief)).
`MUSTER_TASK_ROUTING=1`, with plugins on and the [chief](#chief).
**Check:** `task routing: ON …`.

### PR mail routing

Review mail about a pull request goes to the mirrored session that opened it
([plugins.md](plugins.md#a-pull-request-review-mail)).
`MUSTER_PR_MAIL_SENDERS=<owner>/<handle>,…`, with session routing on, an
agent-mail plugin, and a host hook reporting who authored what.
**Check:** `pull-request routing: ON (…)`.

### Fleet reports

muster answers a "what did my sessions do" request itself
([plugins.md](plugins.md#a-fleet-report-request)).
`MUSTER_FLEET_REPORT_SENDERS` with `MUSTER_FLEET_REPORT_PATHS`, plugins on, an
agent-mail plugin, and [focus](#focus). **Check:** `fleet-report requests: ON (…)`.

### Mail resume

Mail for a session that has ended resumes it in a new tmux window
([plugins.md](plugins.md#resuming-an-ended-session)).
`MUSTER_MAIL_RESUME_ALLOW`, with session routing on and a host running
`muster-mirror -resume-spawn`; `MUSTER_MAIL_RESUME_OFF=1` is the kill switch.
**Check:** `mail resume: ON (…)`.

### Memo relay

Delivers [cairn](https://github.com/ZacxDev/cairn) scope memos to dispatched
agents, as agent turns. [memo-relay.md](memo-relay.md).

- **Needs cairn, which you host,** a read-only cairn credential, and
  [dispatch](#dispatch) with an agent gateway.
- **Not available in the Compose stack**: `MUSTER_MEMO_URL`,
  `MUSTER_MEMO_TOKEN` and `MUSTER_MEMO_SCOPE_MAP` are not forwarded, since the
  relay delivers only to dispatched agents.
- **Check:** `memo relay: ON — …` (`CONFIGURED BUT NOT RUNNING` names what is
  missing).

### Approval router

muster beside a separate **permission router**: a service that records Claude
Code sessions and routes their permission prompts. It adds transcript links,
router notifications and the directory picker; its approval gate takes agents'
permission prompts. [configuration.md](configuration.md#muster-server-permission-router-and-approvals).

- **Needs a permission router you run.** None is in this repository.
- **`.env`:**

  ```sh
  MUSTER_STANDALONE=0                 # the two refuse to boot together
  MUSTER_ROUTER_URL=https://<your router>
  MUSTER_ROUTER_TOKEN=<its credential>
  ```

  The approval gate alone (`MUSTER_APPROVAL_GATE_URL`,
  `MUSTER_APPROVAL_GATE_TOKEN`) is allowed beside standalone.
  `MUSTER_ROUTER_ACTOR` is the actor name muster sends (default `muster`); the
  header names it and the token are sent in, `MUSTER_ROUTER_ACTOR_HEADER` and
  `MUSTER_ROUTER_TOKEN_HEADER`, default to `X-Router-Actor` and
  `X-Router-Token`.
- **Check:** `permission router: CONFIGURED — …`, or `approval relay gate:
  DEDICATED — …` for the gate alone.

### Officers

Standing Claude Code agents with a job description, woken by a queue kept in
**HQ**, an external server, or in muster's own database (the
[built-in backend](officers-builtin.md)). [officers.md](officers.md).

Each officer's instances run on one of two runtimes (`runtime` in its
`MUSTER_OFFICERS` entry):

- **Pods** (the default): **needs [dispatch](#dispatch) with the `claude-code`
  kind, and the connector store's agent door** (`MUSTER_CONNECT_PORT`,
  `MUSTER_AGENT_CONNECT_URL`); HQ or the built-in backend. Not available in the
  Compose stack.
- **Host-spawned sessions** (`"runtime": "host"`, `"host": "<machine>"`): each
  instance is a Claude Code window on ONE of your machines, started by its
  `muster-mirror`. **Works in the Compose stack**: needs the database, the
  built-in backend, and a `muster-mirror` on that machine with `-interactive
  -agent-spawn` and the officer's `repo` on `-agent-spawn-repos`. Outbound
  drafts (`officer_post_draft`, approved by the owner) and `connectors`
  (approved per request; not `github`) work. Not available there: HQ, tiers,
  models, `read_repos`, the LLM proxy, kube reads, ship gates and transcripts
  (each a boot refusal naming the field, or not offered). 🔴 The sessions run
  as that machine's user with no pod isolation — muster's approval is the
  sanctioned path for its posts, not a boundary:
  [officers-builtin.md](officers-builtin.md#officers-in-the-compose-stack-host-sessions),
  [design](design/officers-on-hosts.md).
- In the Compose stack `MUSTER_OFFICERS` is forwarded; a pod officer in it
  refuses the boot (there is no provisioner). The per-officer
  `MUSTER_OFFICER_*_<NAME>` variables are not forwarded (a host officer needs
  none).
- **Check:** `officers: ON — N declared in MUSTER_OFFICERS (H on HQ, B on the built-in backend; P in pods, S on hosts); …`, then one `officers: <id> backend …` line per officer, and for a host officer `officers: <id> on: runtime host on <machine> …`.

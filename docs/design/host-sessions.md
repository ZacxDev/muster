# Design: start a tmux-backed session on a host from muster

**Status: proposed. Nothing here is implemented.** This page proposes letting
the operator start a **new** session on one of their own machines from muster
(the web UI and the PWA now, the Android shell later). The session is a real
tmux session on that host, and muster mirrors it with no extra step. The same
session can then be driven from muster **and** from a terminal on the host
with `tmux attach`.

Where this page says how muster works today, it cites `file:line` at commit
`d46b22e1`, and the code is the authority ([decisions.md](../decisions.md)).
Paths that start with `devrc/` are in the operator's dotfiles repository
(`~/workspace/devrc`), which was read but not changed. A claim that nothing
confirmed is marked **UNVERIFIED**.

## Summary

The design in one line each:

- **Transport: push, on the WebSocket that already exists.** Each host's
  `muster-mirror` already holds one outbound WebSocket that muster pushes
  pastes, attaches and scrolls down. A new `spawn` message travels the same
  way, with a request id and a matching `spawned` reply. muster still never
  connects in to a host, and the host never polls.
- **The host decides what may run.** A new host flag, `-spawn`, plus a
  host-side file, `spawn.json`, lists the **profiles** (a fixed argv each, such
  as `claude`), the **directory roots**, the session-name scheme and the
  limits. muster sends only names it was offered: a profile name, a directory
  key, an optional model from that profile's own list, and a request id. It
  never sends argv, a path, or environment.
- **What gets created:** a new tmux session on the operator's existing tmux
  server, started in the chosen repository. By default it runs `claude` with
  no extra flags. The session is tagged with a tmux user option,
  `@muster-request <id>`, so its identity lives on the tmux object itself.
- **No discovery race:** the host creates the session, forces a listing with
  the existing `RefreshNow`, and only then sends `spawned`. The socket is
  ordered, so muster has the new window before it hears that the request
  succeeded. The listing also carries each window's request id, so muster
  links the window to the request by id, never by guessing.
- **Security:** treated as remote command execution on the operator's
  workstation. It is off by default on both sides. Only the host's owner may
  use it, it needs the same fresh sign-in (step-up) as typing into a mirrored
  session, it is capped and rate-limited on the host, and each spawn and kill
  writes an `audit_log` row. Revoking the host's mirror token stops it within
  30 s.
- **Lifecycle:** **Kill** from muster ends only sessions that carry a muster
  request id. When the program exits, the pane drops to the user's login
  shell in the same directory. If the host is offline, the button is disabled
  and nothing is queued. After a reboot the session is gone, and the row says
  `lost`.
- **First slice (about 8–10 engineer-days):** pick a host, a repository and a
  profile, start the session, land on its mirrored view, and kill it. No
  initial prompt, no free paths, no Android.

## Goals

- From muster's Agents list, **New session** starts Claude Code in a chosen
  repository on a chosen host. Within seconds it opens as an ordinary mirrored
  session: rendered view, terminal, message box, attention and MCP tools.
- `tmux attach -t <name>` on the host reaches **the same** session: one tmux
  session, two clients.
- The host enforces its own allowlist. A compromised muster, or a stolen
  muster session, can start only what the host's own file allows, where it
  allows it, as often as it allows it.
- The design fits the operator's habits: sessions on the default tmux server,
  windows named after their working directory, Claude Code started as plain
  `claude`, and the repositories in `~/workspace`.
- Every spawn and every kill can be audited: who, which host, which profile,
  which directory, and when.

## Non-goals

- **Hermes.** `internal/hermeshost` is a host pattern for *reading* a Hermes
  pane, and the Hermes agent kind is being removed in a parallel PR. Nothing
  here depends on either.
- **A general remote shell.** Free command lines and free paths are out.
  Arbitrary execution already exists through `-interactive`
  ([mirroring.md](../mirroring.md#read-only-interactive-and-sending)), and this
  feature does not widen it.
- **Kubernetes agents.** Dispatched `claude-code` agents stay what they are
  ([provisioner.md](../provisioner.md)). A host session is a *mirrored
  session* muster started, not an agent row.
- **Waking a sleeping host, or starting tmux.** If the host is offline, or has
  no tmux server running, the request is refused (see
  [environment](#environment)).
- **Managing the operator's tmux layout.** No windows are moved and no
  sessions are renamed. Nothing is killed except sessions muster itself
  started.

## What exists today

### Mirroring: the host dials out, muster pushes down

- **One binary per host:** `muster-mirror` (`cmd/muster-mirror`, logic in
  `internal/mirrorhost`). It lists the host's tmux windows and opens **one**
  WebSocket to muster at `/mirror/connect` (`internal/mirror/wire.go:73`).
  Terminal attaches and rendered views are multiplexed over it
  ([mirroring.md](../mirroring.md#how-it-connects)). It opens no listener of its
  own except the optional loopback MCP endpoint
  (`internal/mirrorhost/client.go:6-8`).
- **Authentication:** a per-host mirror token (`mmt_`, 32 random bytes,
  `internal/mirror/wire.go:78`, `internal/mirror/store.go:62-68`), sent as
  `Authorization: Bearer` (`internal/mirrorhost/client.go:296-299`). Only its
  sha256 is stored, in `mirror_tokens`, under row-level security on the owner
  (`internal/db/migrations/0008_mirror_tokens.sql:24-56`). It is minted and
  revoked only from the server CLI (`cmd/muster-server/mirror_cli.go:18-20`).
  The hub refuses a host name the token was not minted for
  (`internal/mirror/hub.go:648-650`). The token is re-checked every 30 s
  (`internal/mirror/service.go:22-25`).
- **There is no hosts table.** A host exists while its connection does, keyed
  in memory by `(owner, host)` (`internal/mirror/hub.go:64-67`), and it stays
  listed as offline for 24 h (`internal/mirror/hub.go:59`). The tunnel is
  in-process state, so muster runs one replica
  ([mirroring.md](../mirroring.md#tokens-revocation-rate-limits)).
- **Capabilities are announced in `hello`** without a version bump
  (`internal/mirror/wire.go:171-400`; [mirroring.md](../mirroring.md#rolling-upgrades)).
  The protocol is v2 (`internal/mirror/wire.go:62,65`).
- **Discovery** is one `tmux list-panes -a -F windowsFormat` per refresh
  (`internal/mirrorhost/perwindow.go:149,187`), every 5 s by default
  (`internal/mirrorhost/client.go:220-221`). A listing is sent only when it
  changed. Every session on the server is registered automatically. A window
  is addressed by `(owner, host, tmux session, window id @N)`
  (`internal/mirror/wire.go:440-449`).
- **An immediate refresh already exists.** `Client.RefreshNow` asks the
  refresh goroutine to read and send now, and returns when that is done
  (`internal/mirrorhost/client.go:185-205`). The MCP endpoint calls it so that
  muster hears of a new Claude session before the tool call arrives. This
  design reuses it for the same reason.
- **Commands go down the socket.** The host's read loop dispatches `open`,
  `resize`, `close`, `watch`, `paste`, `scroll`, `answer`, `cancel-ask`,
  `catalog` and `subagent-tail` (`internal/mirrorhost/client.go:415-454`). A
  paste is a request with an id: the hub keeps a reply channel per id and
  waits with a timeout, and an answer that never comes is *unconfirmed*, not
  *failed* (`internal/mirror/transcript.go:578-613`). `spawn` copies that
  shape.
- **The host runs only tmux.** Every `exec.Command` in `internal/mirrorhost`
  runs tmux: list, `load-buffer` and `paste-buffer`
  (`internal/mirrorhost/claude.go:733-770`), copy-mode `send-keys -X`
  (`internal/mirrorhost/scroll.go:130`), and attach
  (`internal/mirrorhost/tmux.go:118-137`). No path creates a session or a
  window today.
- **`-interactive` is already a shell.** An interactive client can switch to
  any session or open a new window (`cmd/muster-mirror/main.go:158`;
  [mirroring.md](../mirroring.md#read-only-interactive-and-sending)).

### Linking a pane to a Claude Code session

The host walks the pane's process tree to `claude`, then finds its session id
in this order: the pane map (`claude-p<N>.json`), then
`$CLAUDE_CONFIG_DIR/sessions/<pid>.json`, then a guess from the newest
transcript in its working directory (`internal/mirrorhost/claude.go:140-145`,
`:409`, `:505`; [mirroring.md](../mirroring.md#how-a-panes-transcript-is-found)).
The id goes out as `claudeSession` (`internal/mirror/wire.go:508`). A new
session needs nothing extra here: once `claude` runs in the pane, the
existing lookup finds it.

### The send gate

Typing into a mirrored session needs a sign-in within `MUSTER_STEPUP_WINDOW`
(default 12 h, bounds 1 min–24 h, `internal/stepup/stepup.go:46-54`). Freshness
belongs to one browser session (`internal/stepup/stepup.go:12-28`). You confirm
with a passkey or the password ([sending-and-step-up.md](../sending-and-step-up.md#step-up-confirm-its-you)).
It gates the interactive attach (`internal/api/mirror.go:313`), both sends
(`internal/api/mirror_view.go:362`), `POST /send-queue`
(`internal/api/send_queue.go:252`) and ask answers
(`internal/api/mirror_ask.go:408`). The mirror service checks again on its own
(`internal/mirror/service.go:307`).

**Mirror sends write no `audit_log` row.** The only trace is a server log
line (`internal/api/mirror_view.go:526,530`). `audit_log` exists
(`internal/db/migrations/0006_accounts_schema.sql:324-335`), and invites,
flows and plugins write to it (for example `internal/users/invites.go:8`).
This design writes to it.

### The MCP server for mirrored sessions

`muster-mirror -mcp-listen` serves muster's tools (`attention_ask` and the
others) on loopback (`cmd/muster-mirror/main.go:164`,
`internal/mirrorhost/mcpproxy.go:332`). It binds each call to the pane's
Claude session and forwards it with the mirror token to `POST /mirror/mcp`
(`internal/mirrorhost/mcpproxy.go:44-58`). The operator configures
`mcpServers.muster` by hand in `~/.claude.json`, with
`Authorization: Bearer ${MUSTER_MIRROR_MCP_SECRET:-}` and the pane headers
([attention.md](../attention.md#mirrored-claude-code-sessions)). The shell that
starts Claude Code must export the secret. On the operator's host the unit
listens on `127.0.0.1:17823` (`devrc/nix/home.nix:548-549`), and
`~/.claude.json` names that port. How the operator's shell exports
`MUSTER_MIRROR_MCP_SECRET` was not found: **UNVERIFIED**. A spawned session
gets the secret explicitly (see [environment](#environment)), so it does not
depend on that.

### The claude-code agent kind: what carries over

- **ccd's tmux command is the model for ours:** `tmux new-session -d -P -F …
  -s <target> -x 200 -y 50 -c <dir> -e K=V … <argv> ; set-option -w
  remain-on-exit on` (`cmd/ccd/tmux.go:267-270`). The pod's primary session is
  `cc` (`cmd/ccd/main.go:127`). `cc-<laneID>` is a per-chat **lane**
  (`cmd/ccd/lanes.go:560`), not the primary session.
- **The rest is cluster-only.** `provision.Provisioner`
  (`internal/provision/provision.go:106-200`) has `Scale`, `Endpoint` and
  `StreamLogs`, which suit a Deployment, not a tmux session. The claude-code
  spec builds Secrets, a PVC and a NetworkPolicy
  (`internal/agentspec/claudecode.go:195-321`). Its terminal is a Kubernetes
  exec (`internal/agents/terminal.go:48-52`), not `muster-mirror`. Its MCP
  config is seeded into the pod's `.claude.json` with the agent's own token
  (`cmd/ccd/seed.go:150-173`, `cmd/ccd/connect.go:56-76`).
- **ccd is not reused on the host.** It is a supervisor: it owns the session,
  respawns the CLI, serves a chat gateway and needs a hook socket. The
  operator wants a plain tmux session they own. What carries over is the
  command shape and the `remain-on-exit` idea.

### The operator's habits (read from devrc)

- **Hosts:** two, `workbench` and `laptop`. `MUSTER_MIRROR_HOST` is chosen by
  `isLaptop` (`devrc/nix/home.nix:511`, `:636`). Both run
  `muster-mirror` as a systemd user unit with `MUSTER_MIRROR_INTERACTIVE=1`
  (`devrc/nix/muster-mirror.nix:129-148`, `devrc/nix/home.nix:459-496`). The
  unit's PATH holds only tmux (`devrc/nix/home.nix:506`), and
  `TMUX_TMPDIR=%t` makes it share the operator's tmux server.
- **Sessions:** mostly 20 scratch slots, `scratch` and `scratch2`–`scratch20`,
  each with a codename (`devrc/scripts/tmux-scratch-slots.sh:105-126`). Each
  slot has an Alt+key popup toggle that attaches or creates it
  (`devrc/nix/programs/tmux/default.nix:31-35`). muster shows a slot by its
  codename through `MUSTER_MIRROR_LABELS_FILE` (`devrc/nix/home.nix:518-519`).
  Repository-named sessions also exist
  (`devrc/scripts/tmux-reply-agent:668-675`). A session tmux names with a bare
  number is renamed after its directory (`devrc/.tmux.conf:214`).
- **Windows** are named automatically after the directory's basename, with
  ` ●` added while `claude` runs (`devrc/.tmux.conf:69-71`).
- **Working directory:** inherited from the current pane. The workspace is
  `~/workspace` (`devrc/nix/home.nix:4`). There is no repository picker and no
  zoxide.
- **CLI:** plain `claude`. No `--dangerously-skip-permissions` anywhere in
  devrc. The model is set in `~/.claude/settings.json` (`opus[1m]`), and the
  permission mode is `default`.
- **After a reboot** `devrc/scripts/tmux-session-restore.py` re-creates
  windows and runs `claude --resume <id>` (`:7`, `:542`, `:2350`).
- **An existing launcher:** the clawgate reply agent's `new-session` write
  kind runs `new-window -c <cwd>` and types the caller's text
  (`devrc/scripts/tmux-reply-agent:613-619`, `:658-733`). It checks only that
  the cwd is absolute and exists (`:571-595`). It takes PATH from
  `tmux show-environment -g PATH` so `claude` resolves (`:622-655`). That PATH
  trick matters here too.

## Options compared

### Request path

| option | how a request reaches the host | latency | new attack surface | fits "muster never connects to a host" | verdict |
|---|---|---|---|---|---|
| **A message on the mirror WebSocket** | muster pushes `spawn`, the host replies `spawned` | one round trip | one message type, behind a host flag | ✓ | **recommended** |
| The host polls a muster route | the host GETs pending requests | up to the poll interval | a new route and a pending-request queue | ✓ | ✗ the socket already exists; a queue keeps stale intent alive |
| muster SSHes to the host | `ssh host tmux new-session …` | one connection | an SSH key in muster that reaches the workstation | ✗ | ✗ |
| Type it through Live keys | muster opens an interactive attach and types `tmux new-session …` | seconds, screen-dependent | none new, but no allowlist at all | ✓ | ✗ unbounded, racy, and screen-scraped |
| The reply agent's `new-session` (devrc) | clawgate → `tmux-reply-agent` | — | types arbitrary caller text; lives outside muster | — | ✗ no allowlist, wrong repository |

### What runs the session on the host

| option | operator can `tmux attach` | allowlist on the host | reuse | verdict |
|---|---|---|---|---|
| **`muster-mirror` runs `tmux new-session` on the operator's own server** | ✓ the same server they use | `spawn.json` | the paste request/reply, `RefreshNow`, ccd's command shape | **recommended** |
| Run `ccd` on the host | only through ccd's private socket; ccd owns respawn | ccd has none | lanes, supervision | ✗ heavier, and the session is not theirs |
| A `host` provisioner driver making agent rows | — | — | `agentprovision` lifecycle | ✗ the interface is pod-shaped (`Scale`, `Endpoint`, `StreamLogs`); revisit only if host sessions should be dispatch targets |
| A separate daemon (`muster-spawnd`) | ✓ | its own | none | ✗ a second credential and process for what one message does |

## Recommended design

### Overview

```mermaid
sequenceDiagram
  participant U as Browser / PWA
  participant M as muster-server
  participant H as muster-mirror (host)
  participant T as tmux server
  U->>M: GET /mirror/hosts/{host}/new (form: dirs, profiles from the host's hello)
  U->>M: POST /mirror/hosts/{host}/sessions {request_id, profile, dir, model?}
  M->>M: same-origin, owner, step-up, host advertises spawn, row "requested", audit
  M->>H: {"type":"spawn","id":n,"request":"<uuid>","profile":"claude","dir":"muster"}
  H->>H: validate against spawn.json, rate limit, cap, ledger lookup
  H->>T: new-session -d -s <name> -c <dir> -e … -- <argv>; set-option @muster-request <uuid>
  H->>H: RefreshNow (the listing with the new window, tagged with its request id)
  H-->>M: sessions/windows listing
  H-->>M: {"type":"spawned","id":n,"session":…,"window":"@N","pane":"%N"}
  M->>M: row "running", audit result
  M-->>U: redirect to the mirrored view of host/session/@N
```

### Which hosts are eligible, and choosing one

A host is offered in **New session** only when all of these hold:

- it is connected now, and its token is owned by the signed-in user (the
  hub's `(owner, host)` key already scopes this);
- it is on protocol v2 and its hello announces `spawn: true` with at least
  one profile and one directory;
- the server runs with `MUSTER_HOST_SESSIONS=1`. That is a new variable, off by
  default, with a row in [configuration.md](../configuration.md).

The sheet lists the eligible hosts. It preselects the host this browser used
last (remembered per browser, like the Interactive choice), else the one
with the most recent activity. The directory list depends on the host, so
the host is picked first. An offline or read-only host is shown greyed out,
with the reason.

### Host configuration: `spawn.json`

`muster-mirror -spawn` (or `MUSTER_MIRROR_SPAWN=1`) turns the feature on. It
reads `~/.config/muster-mirror/spawn.json` (`-spawn-file` overrides the path).
If the file is missing or invalid, the host says `spawn: false` in its hello
and logs why. That is never fatal. On the operator's hosts, Home Manager
generates the file, like the labels and snippets files
(`devrc/nix/home.nix:518-519`, `:5456`).

```json
{
  "roots": ["~/workspace"],
  "profiles": {
    "claude":   {"argv": ["claude"], "models": ["opus", "sonnet"], "modelFlag": "--model"},
    "opencode": {"argv": ["opencode"]}
  },
  "sessionPool": ["scratch12", "scratch13", "scratch14"],
  "sessionPrefix": "m-",
  "maxLive": 6,
  "maxPerHour": 12,
  "afterExit": "shell"
}
```

- **`roots`:** the directories under which a session may start. The host
  offers each **direct child that holds a `.git`** (a directory, or a
  worktree's `.git` file, the test `internal/mirrorhost/repo.go` already
  uses). The offer is a list of `{key, label}`, where the key is the child's
  name. A root itself is never offered.
- **`profiles`:** the only programs that may run. Each one is a fixed argv.
  An optional `models` list is the only part muster may vary: muster sends a
  model name, and the host appends `modelFlag <name>` only if the name is in
  the list. The host **refuses to load** a profile whose argv contains
  `--dangerously-skip-permissions` or `--permission-mode bypassPermissions`
  unless the profile sets `"allowBypass": true`. That is a deliberate,
  greppable opt-in.
- **`sessionPool` / `sessionPrefix`:** see [naming](#tmux-naming).
- **`maxLive`:** how many muster-started sessions may exist at once (counted
  from tmux, not memory). **`maxPerHour`:** spawns allowed per rolling hour,
  kept in the ledger, so a restart does not reset it.
- **`afterExit`:** `shell` (the default) or `remain` (see
  [lifecycle](#lifecycle)).

The hello carries the profiles' names and model lists and the directory
offer, never argv. It is re-sent when the file or the set of repositories
changes.

### The wire

Two message types, announced by a hello field. There is no version bump,
because an older muster ignores an unknown field, as with `Labels`
(`internal/mirror/wire.go:180-189`):

- `hello.spawn`: `{profiles: [{name, models}], dirs: [{key, label}], maxLive}`.
- `spawn` (muster → host): `{id, request, profile, dir, model?}`. `request`
  is a UUID that muster mints when it renders the form. `dir` is a key from
  the host's own offer.
- `spawned` (host → muster): `{id, request, session, window, pane, reused?}`
  or `{id, request, error: <code>}`.
- `kill` (muster → host): `{id, request}`. `killed` is the reply.
- Each window in a listing gains `request`: the value of its session's
  `@muster-request` option, if set. An older muster ignores it.

The error codes are fixed strings, so the UI can say why:
`spawn_disabled`, `unknown_profile`, `unknown_dir`, `unknown_model`,
`max_live`, `rate_limited`, `no_tmux_server`, `name_taken`, `tmux_failed`,
`exec_not_found`.

On the server, `Hub.Spawn` copies `pasteTo`'s request/reply bookkeeping
(`internal/mirror/transcript.go:578-613`), with a 10 s timeout. A timeout after
the write is **unconfirmed**: the row says so, and it is settled by the next
listing that carries the request id.

### What gets created

#### tmux naming

- **Session name:** the first `sessionPool` entry that does not exist as a
  session, else `<sessionPrefix><6 hex of the request id>`, for example
  `m-3f9a1c`. A pool fits the operator's habits: a spawned session becomes a
  scratch slot with its Alt+key toggle and its codename label already wired
  (`devrc/nix/programs/tmux/default.nix:31-35`, `devrc/nix/home.nix:518-519`).
  Without a pool, the `m-` name never matches the bare-number rename hook
  (`devrc/.tmux.conf:214`). Whether the pool is wanted is
  [open question 1](#open-questions).
- **Window name:** none is set. tmux's automatic rename names it after the
  directory and adds ` ●` while `claude` runs, as every other window does
  (`devrc/.tmux.conf:69-71`).
- **Identity:** `set-option -t =<session> @muster-request <uuid>`. A user
  option lives on the tmux session itself, so the host can tell muster's
  sessions from the operator's after its own restart, and muster can link a
  window to its request without guessing.

#### The command

```sh
tmux -u new-session -d -P -F '#{session_id}\t#{window_id}\t#{pane_id}' \
  -s <name> -c <resolved dir> -x 200 -y 50 \
  -e PATH=<tmux global PATH> -e MUSTER_MIRROR_MCP_SECRET=<secret> \
  -e MUSTER_REQUEST=<uuid> \
  -- <argv…>
tmux set-option -t =<name> @muster-request <uuid>
```

- It is run as an argv, never through a shell string. With `afterExit:
  "shell"`, the argv becomes
  `[$SHELL, -lc, '"$@"; exec "$SHELL" -l', muster-session, <argv…>]`: the
  profile's words are positional parameters, never interpolated.
- `-x 200 -y 50` is ccd's size (`cmd/ccd/tmux.go:267-270`). The first attached
  client resizes it.
- The directory is `root/key`, resolved with `filepath.EvalSymlinks`. It must
  still be under a root, be a directory, and hold `.git`. Each check is redone
  at spawn time, not just when the list was built.

#### Environment

- **The tmux server must already be running.** `new-session` from the
  systemd unit, with no server, would *start* the server with the unit's
  environment, whose PATH holds only tmux (`devrc/nix/home.nix:506`). Every
  session the operator opened later would inherit that broken server. So the
  host checks `tmux has-session` (any session) first, and refuses with
  `no_tmux_server`. **UNVERIFIED:** whether starting a server from the unit
  would break more than PATH; refusing avoids finding out.
- **PATH** comes from `tmux show-environment -g PATH`, as the reply agent
  does (`devrc/scripts/tmux-reply-agent:622-655`), so `claude` resolves as it
  does in the operator's shells. `argv[0]` is resolved against that PATH at
  spawn time, and `exec_not_found` is returned if it is missing.
- **`MUSTER_MIRROR_MCP_SECRET`** is read from `-mcp-secret-file`, which the
  host already holds (`internal/mirrorhost/mcpproxy.go:358`). So the spawned
  `claude` reaches the muster MCP server through the operator's existing
  `~/.claude.json` entry, whether or not their shell exports the secret. If
  `-mcp-listen` is off, it is not set.
- **`MUSTER_REQUEST`:** for scripts and hooks. Nothing reads it in slice 1.
- **Nothing from muster.** No environment value, flag or text comes from the
  request.

#### Idempotency, and an existing name

- The **request id** is minted when the form renders. A double submit, a
  retry after an unconfirmed reply, or a reconnect re-sends the same id.
- The host keeps a small **ledger** (`~/.local/state/muster-mirror/spawns.jsonl`):
  request id, session name, time, result. On a known id it answers with the
  existing session (`reused: true`) **if** a session carrying that id is still
  there. It never starts a second one.
- If the chosen name exists **without** our id (the operator made `scratch12`
  between listing and spawn), the host takes the next pool entry, then
  `m-<hex>`. If `m-<hex>` itself exists with a different id, it refuses with
  `name_taken`. It never attaches to, renames or reuses a session muster did
  not start.
- On the server, the row's `request_id` is unique. A second POST with the
  same id returns the first row's result.

### Mirroring handoff

1. The host runs `new-session` and `set-option`.
2. The host calls `RefreshNow` (`internal/mirrorhost/client.go:185-205`), which
   reads and sends the listing in the refresh goroutine. The new window is in
   it, tagged with `request`.
3. Only after `RefreshNow` returns does the host write `spawned`. The
   WebSocket delivers in order, so muster processes the listing first.
4. muster checks that the listing it holds has a window on `session` with
   `request == <id>`. If it does not, the row is `failed: not_listed`, a bug
   to surface, not to paper over. muster then redirects the browser to the
   mirrored view (`/mirror/…/<host>/<session>/<@N>`).
5. The Claude session id arrives on a later refresh, once `claude` writes its
   sessions file (the existing lookup, `internal/mirrorhost/claude.go:140-145`).
   Until then the view shows the terminal and the rendered view says
   "starting". The row stores the Claude session id when it first appears, for
   relinking after a reboot (Phase 3).

`tmux attach -t <name>` on the host reaches the same session. muster's viewer
attaches with `-f read-only,ignore-size` or `attach -r`
([mirroring.md](../mirroring.md#read-only-interactive-and-sending)), so the
operator's own client keeps its size.

### Server side

- **Route:** `POST /mirror/hosts/{host}/sessions` (form or JSON), on the
  session tier. It needs the same-origin check, the host's owner, and step-up
  (below). There is no `mst_` or machine-token route in slice 1. The form
  comes from `GET /mirror/hosts/{host}/new`.
- **Kill:** `POST /mirror/host-sessions/{request_id}/kill`, with step-up.
- **Table `host_sessions`** (the next free migration, `0047_…`): `id`,
  `owner_id` (FORCE row-level security, like `mirror_tokens`), `request_id`
  (UUID, unique), `host`, `profile`, `dir_key`, `model`, `tmux_session`,
  `window_id`, `pane_id`, `claude_session`, `status` (`requested`,
  `running`, `unconfirmed`, `failed`, `exited`, `killed`, `lost`), `error`,
  `stepup_at`, `created_at`, `ended_at`. Rows that ended more than 30 days
  ago are swept.
- **Audit:** `audit_log` rows with `action` `host_session.spawn`,
  `host_session.kill` and `host_session.refused`. `object` is
  `host:<host>`, and `detail` holds `{request_id, profile, dir_key, model,
  result, error}`. The same row is written on the server's refusals (stale
  step-up, host not eligible) and on the host's.
- **UI:** a **New session** button on the Agents list, next to the mirrored
  sessions. It opens a sheet with host, repository (searchable), profile and
  model, and a **Start** button. A spawned session's card has a small
  "started from muster" mark and a **Kill** action.

## Security

### The threat

**This is remote command execution on the operator's workstation, started from
a web app.** Its worst case is not "an attacker opens a terminal". It is "an
attacker starts an agent with tool access in a checkout that holds
credentials". The design bounds what can be started, where, how often, and by
whom, and records each one.

### Who may do it

| layer | check |
|---|---|
| server | `MUSTER_HOST_SESSIONS=1`; a signed-in browser session; the same-origin check; the host's token belongs to this user (the hub's `(owner, host)` key) |
| server | step-up: a sign-in inside `MUSTER_STEPUP_WINDOW`, re-checked by the mirror service before the message is written, as sends are (`internal/mirror/service.go:307`) |
| host | `-spawn` is set, and `spawn.json` loaded |
| host | the profile, directory and model are in its own lists; the live and hourly caps hold |
| host | the tmux server already exists |

Members can own hosts too, since tokens carry `owner_id`. Spawning is allowed
only on one's own host. An admin cannot spawn on a member's host: the hub
keys on the owner, and nothing here adds an admin path.

### Step-up, compared with the send gate

| action | gate today or proposed |
|---|---|
| watch a mirrored session | none |
| send, queue, attach interactively | fresh sign-in, 12 h (today) |
| **start a host session** | fresh sign-in, 12 h, the same gate (proposed) |
| **kill a host session** | fresh sign-in, 12 h (proposed); it destroys running work, so the rule that "refusing is never the risky direction" does not apply |

Why not a stricter, per-spawn passkey? On a host that runs `-interactive`,
which both of the operator's hosts do (`devrc/nix/home.nix:459-496`), the
12-hour gate already admits a shell: open Live keys and type anything.
A spawn bounded by `spawn.json` is **strictly less** than that. A stricter
gate on the weaker door would add friction without closing anything. On a
host that runs `-spawn` **without** `-interactive`, a spawn is new power. That
case is [open question 4](#open-questions). The recommendation there is a
per-spawn passkey (the 5-minute `CredentialMaxAge` rule,
`internal/stepup/stepup.go:64`) for such hosts. The host reports its mode in
its hello, so muster can tell.

### The host does not trust muster

- muster sends **names**, and the host maps each to its own value. An unknown
  name is refused. Unknown JSON fields are refused, not ignored.
- No argv, path, environment or text from muster reaches `exec` or tmux.
- The directory is re-resolved at spawn time and must stay under a root
  after symlinks.
- The caps live on the host. `maxPerHour` survives the host's restart through
  the ledger.
- The host logs every spawn, kill and refusal to its own journal (stdout
  under the systemd unit), with the request id. That record does not depend
  on muster's database.
- `kill` applies only to sessions whose `@muster-request` matches. A
  compromised muster cannot kill the operator's own sessions through it.

### What a stolen muster session could do

| attacker holds | can do |
|---|---|
| a session cookie **outside** the step-up window | list hosts and sessions; nothing here |
| a cookie **inside** the window | start up to `maxLive` sessions, at most `maxPerHour`, of the allowed profiles, in the offered repositories, and then type into them. With `-interactive` on, they could already do anything at all. |
| a `mst_` or machine token | nothing: the route is session-tier only |
| the host's mirror token | impersonate the host to muster; it cannot make the host spawn anything (spawns are commands *to* the host) |
| muster's server or database | send any `spawn` the host accepts, and nothing beyond `spawn.json` |

A spawned `claude` keeps the operator's permission mode (`default`), so its
tool calls still ask for permission through the operator's usual approval
path. That is a mitigation, not a boundary: an attacker who can type into the
session can also answer its prompts from muster.

### Revocation and kill switches

- **Revoke the mirror token** (`muster-server mirror token revoke <id>`): the
  connection ends within 30 s (`internal/mirror/service.go:22-25`), and with it
  every spawn path.
- **On the host:** drop `-spawn`, or delete `spawn.json`, and restart the
  unit. Sessions already created stay. They are ordinary tmux sessions.
- **On the server:** unset `MUSTER_HOST_SESSIONS`.
- **Sign out everywhere / disable the account:** ends the browser session
  that would carry the step-up.

## Lifecycle

| event | what happens |
|---|---|
| program exits, `afterExit: shell` | the pane becomes the user's login shell in the same directory; the window stays mirrored; the row stays `running`. This matches how the operator runs `claude` today, inside a shell |
| program exits, `afterExit: remain` | tmux keeps the dead pane (`remain-on-exit on`); the host reports `pane_dead`; the row becomes `exited` |
| **Kill** from muster | `kill-session -t =<name>`, after the `@muster-request` check; the row becomes `killed`; an audit row |
| the operator kills it on the host | the next listing lacks it; the row becomes `exited` |
| host offline | New session is disabled for it; nothing is queued, because a spawn intent from an hour ago is not an intent now; existing rows keep their state until the host is back |
| reply never came | `unconfirmed`; settled by the next listing: the request id seen means `running`, not seen means `failed` |
| `muster-mirror` restarts | sessions are untouched; the next hello re-lists them with their `request` tags |
| host reboots | the tmux server and its sessions are gone; rows whose session is missing become `lost` once the host reconnects. The operator's restore script may re-create a window and `claude --resume` it (`devrc/scripts/tmux-session-restore.py:542`), but without the `@muster-request` option (**UNVERIFIED**: whether it copies user options). Phase 3 relinks by Claude session id |
| row cleanup | ended rows are swept after 30 days; muster never kills an idle session on its own |

## Phased plan

Rough engineer-days, including tests and docs.

| phase | what | host | server + UI | depends on |
|---|---|---|---|---|
| **1 (first slice)** | `-spawn`, `spawn.json` (roots, profiles, models, caps, pool, `afterExit`), the directory offer in hello, `spawn`/`spawned`/`kill`/`killed`, the `@muster-request` tag in listings, the ledger, the no-tmux-server refusal, PATH and MCP secret in the environment, `RefreshNow` before the reply. `MUSTER_HOST_SESSIONS`, `host_sessions` and its migration, `Hub.Spawn`/`Kill`, the two routes with step-up, audit rows, the New session sheet, the redirect, the Kill action. The devrc Home Manager change that writes `spawn.json` is separate, in that repository | 3.5–4.5 | 4–5 | — |
| 2 | An initial prompt, delivered through the existing send queue once the window reports `idle` (its step-up is the spawn's); recent directories per host; a `muster` CLI command over a `mst_` route, if wanted | 0.5 | 2–3 | 1 |
| 3 | Lifecycle hardening: relink after a reboot by Claude session id, `exited`/`lost` reconciliation tests, the sweep, a per-host "muster-started" filter | 1 | 1.5–2 | 1 |
| 4 | Android: a signed device action `host_session_spawn` under `/api/device/actions/`, using the native step-up ([android.md](android.md#which-actions-are-gated)) | — | 1–1.5 (plus app work) | 1, Android Phase 2 |
| 5, optional | A worktree per session: a profile option to `git worktree add` a fresh branch under the repository before starting | 1–1.5 | 0.5 | 1 |

**First slice: Phase 1, about 7.5–9.5 engineer-days.** It is the smallest
slice that is useful by itself: start Claude Code in a chosen repository on a
chosen host, drive it from the phone, attach to it at the desk, and kill it.

### Tests the first slice needs

- **Host, against a real tmux server** (the suite already drives tmux for
  mirrorhost):
  - an unknown profile, directory, model and field are each refused with
    their code;
  - a symlink out of a root is refused;
  - a `..` key is refused;
  - an existing name without our tag is never reused;
  - a repeated request id returns the same session;
  - `maxLive` and `maxPerHour` hold across a host restart;
  - no tmux server means no server is started;
  - `kill` refuses a session without the matching tag;
  - argv words containing `;`, `$(…)` and spaces arrive in the program
    verbatim.
- **Ordering:** a test that `spawned` is written only after the listing that
  contains the window. Mutation-check it: move the reply before
  `RefreshNow`, and the test must go red.
- **Server:**
  - stale step-up is refused before any message is written;
  - another owner's host is refused;
  - an older host (no `spawn` in hello) is not offered;
  - the audit row is written for success and for each refusal;
  - a duplicate request id returns the first result.
- **Browser:** the New session click path ends on the mirrored view of the
  new window (the Playwright suite, [testing.md](../testing.md)).

## Risks

- **Starting a tmux server with the wrong environment.** The no-server
  refusal handles it. The risk is a later change dropping that check, so the
  check gets its own test.
- **The operator's scratch-slot scheme lives in devrc, not muster.** If the
  pool in `spawn.json` and `tmux-scratch-slots.sh` drift, a spawned session
  could take a slot whose codename the operator thinks is free. Generating
  both from the one Nix table removes the drift.
- **The restore script after a reboot** re-creates sessions muster
  started, without their tag. Until Phase 3 they come back as ordinary
  mirrored windows, and their rows say `lost`. That is safe, but untidy.
- **Two spawns in the same instant** both see `scratch12` free. tmux's
  `new-session -s` fails on a name that exists, so the second takes the next
  name. The host serialises spawns with a mutex anyway.
- **A single muster replica** (the tunnel is in-process). Not new, but spawn
  inherits it.
- **Profiles drifting between the two hosts.** Both come from the same Nix
  module, so the drift is in one place.
- **`opencode` profile:** muster mirrors any pane, but it has a rendered view
  only for Claude Code (and Hermes). An `opencode` session is terminal-only in
  muster. Expected, not a bug, and the sheet should say so.

## Open questions

1. **Session naming: a scratch slot from a pool, or always `m-<hex>`?** A pool
   gives the operator their Alt+key toggle and codename at once. `m-<hex>` is
   simpler and never touches their slots. **Recommended: a pool of the upper
   slots (for example `scratch15`–`scratch20`), falling back to `m-<hex>`.**
2. **Directories: repositories under `~/workspace` only, or also a free path
   under a root?** **Recommended: repositories only in slice 1, recent
   directories in Phase 2, never a free path.**
3. **When the program exits: drop to a shell, or keep the dead pane?**
   **Recommended: drop to the login shell (`afterExit: shell`)**, which is how
   the operator runs `claude` today.
4. **On a host with `-spawn` but without `-interactive`, should a spawn need a
   per-spawn passkey (5 min) instead of the 12-hour window?** **Recommended:
   yes, for such hosts only.** The operator's two hosts run `-interactive`, so
   this does not affect them today.
5. **An initial prompt in slice 1?** **Recommended: no.** It is Phase 2,
   through the send queue, so it gets the queue's delivery-once and its
   step-up re-check for free.
6. **Profiles: `claude` only, or `opencode` too?** **Recommended: `claude` in
   slice 1, with model choices `opus` and `sonnet`; add `opencode` once
   someone wants it, since it has no rendered view.**
7. **Should bypass-permission flags be allowed at all?** **Recommended: the
   host refuses them unless the profile sets `allowBypass: true`, and the
   operator's Nix module sets no such profile.**

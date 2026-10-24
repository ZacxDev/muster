# Design: start a tmux-backed session on a host from muster

**Status: proposed; the design's decisions are made** (by the operator,
2026-10-24; see [decisions](#decisions)). **Nothing here is implemented.**
Every other section is the recommended design. This page proposes letting
the operator start a **new** session on one of their own machines from muster
(the web UI and the PWA now, the Android shell later). The session is a real
tmux session on that host, and muster mirrors it with no extra step. The same
session can then be driven from muster **and** from a terminal on the host
with `tmux attach`.

Where this page says how muster works today, it cites `file:line` at commit
`39116aa1`, and the code is the authority ([decisions.md](../decisions.md)).
Paths that start with `devrc/` are in the operator's dotfiles repository
(`~/workspace/devrc`), which was read but not changed. A claim that nothing
confirmed is marked **UNVERIFIED**.

## Summary

The design in one line each:

- **Transport: push, on the WebSocket that already exists.** Each host's
  `muster-mirror` already holds one outbound WebSocket that muster pushes
  pastes, attaches and scrolls down. A new `spawn` message travels the same
  way, with a matching `spawned` reply. muster still never connects in to a
  host, and the host never polls.
- **The host decides what may run.** A new host flag, `-spawn`, plus a
  host-side file, `spawn.json`, lists the **profiles** (a fixed argv each:
  `claude` and `opencode`), the **directory roots**, the session-name pool
  and one live cap. muster sends only two names it was offered: a profile
  name and a directory key. It never sends argv, a path, or environment.
- **What gets created:** a new tmux session on the operator's existing tmux
  server, started in the chosen repository. It runs plain `claude` (the
  model comes from the operator's own settings) or plain `opencode`, which
  is terminal-only in muster. The host tags the session with a tmux user
  option, `@muster-spawned`, in the same tmux call that creates it. Kill
  acts only on sessions that carry the tag.
- **No discovery race:** the host creates the session, forces a listing with
  the existing `RefreshNow`, and only then sends `spawned`. The socket is
  ordered, so muster has the new window before it hears that the spawn
  succeeded, and the redirect lands on a window muster already lists.
- **Security:** treated as remote command execution on the operator's
  workstation. It is off by default on both sides. Only the host's owner may
  use it, only on a host that runs `-interactive`, and it needs the same
  fresh sign-in (step-up) as typing into a mirrored session. The host bounds
  it with its allowlist and a live cap. Revoking the host's mirror token
  stops it within 30 s.
- **Lifecycle:** **Kill** from muster ends only tagged sessions. When the
  program exits, the pane drops to the user's login shell in the same
  directory. If the host is offline, the button is disabled and nothing is
  queued. After a reboot the session is gone, like any other tmux session.
- **First slice (Phase 1, about 6.5–9 engineer-days):** pick a host, a
  repository and a profile (`claude` or `opencode`), start the session, land
  on its mirrored window, and kill it. It has no server table, no host
  ledger, no audit rows, no model picker, no initial prompt, no free paths,
  and no Android.

## Goals

- From muster's Agents list, **New session** starts Claude Code or opencode
  in a chosen repository on a chosen host. Within seconds it opens as an
  ordinary mirrored session. A Claude Code session gets the rendered view,
  terminal, message box, attention and MCP tools. An opencode session gets
  the terminal only ([the opencode profile](#the-opencode-profile)).
- `tmux attach -t <name>` on the host reaches **the same** session: one tmux
  session, two clients.
- The host enforces its own allowlist. A compromised muster, or a stolen
  muster session, can start only what the host's own file allows, where it
  allows it, as often as it allows it.
- The design fits the operator's habits: sessions on the default tmux server,
  windows named after their working directory, Claude Code started as plain
  `claude` and opencode as plain `opencode`, and the repositories in
  `~/workspace`.
- Slice 1 stays small. Sends, the greater power on these hosts, write no
  audit rows, and slice 1 adds no table and no audit rows of its own. History
  and audit are a later phase, if wanted ([phased plan](#phased-plan)).

## Non-goals

- **Hermes.** `internal/hermeshost` is a host pattern for *reading* a Hermes
  pane. The `hermes` agent kind was removed in #249, and Hermes host
  mirroring stays. Nothing here depends on either, and there is no Hermes
  spawn profile.
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
  (`internal/mirror/hub.go:653`). The token is re-checked every 30 s
  (`internal/mirror/service.go:22-25`).
- **There is no hosts table.** A host exists while its connection does, keyed
  in memory by `(owner, host)` (`internal/mirror/hub.go:64-67`), and it stays
  listed as offline for 24 h (`internal/mirror/hub.go:59`). The tunnel is
  in-process state, so muster runs one replica
  ([mirroring.md](../mirroring.md#tokens-revocation-rate-limits)).
- **Capabilities are announced in `hello`** without a version bump
  (`internal/mirror/wire.go:171-441`; [mirroring.md](../mirroring.md#rolling-upgrades)).
  The protocol is v2 (`internal/mirror/wire.go:62,65`).
- **Discovery** is one `tmux list-panes -a -F windowsFormat` per refresh
  (`internal/mirrorhost/perwindow.go:149,187`), every 5 s by default
  (`internal/mirrorhost/client.go:220-221`). A listing is sent only when it
  changed. Every session on the server is registered automatically. A window
  is addressed by `(owner, host, tmux session, window id @N)`
  (`internal/mirror/wire.go:443-453`).
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
  *failed* (`internal/mirror/transcript.go:648-683`). `spawn` copies that
  shape.
- **The host runs only tmux.** Every `exec.Command` in `internal/mirrorhost`
  runs tmux: list, `load-buffer` and `paste-buffer`
  (`internal/mirrorhost/claude.go:739-776`), copy-mode `send-keys -X`
  (`internal/mirrorhost/scroll.go:130`), and attach
  (`internal/mirrorhost/tmux.go:118-137`). No path creates a session or a
  window today.
- **`-interactive` is already a shell.** An interactive client can switch to
  any session or open a new window (`cmd/muster-mirror/main.go:164`;
  [mirroring.md](../mirroring.md#read-only-interactive-and-sending)).

### Linking a pane to a Claude Code session

The host walks the pane's process tree to `claude`, then finds its session id
in this order: the pane map (`claude-p<N>.json`), then
`$CLAUDE_CONFIG_DIR/sessions/<pid>.json`, then a guess from the newest
transcript in its working directory (`internal/mirrorhost/claude.go:145-153`,
`:417`, `:513`; [mirroring.md](../mirroring.md#how-a-panes-transcript-is-found)).
The id goes out as `claudeSession` (`internal/mirror/wire.go:514`). A new
session needs nothing extra here: once `claude` runs in the pane, the
existing lookup finds it.

### The send gate

Typing into a mirrored session needs a sign-in within `MUSTER_STEPUP_WINDOW`
(default 12 h, bounds 1 min–24 h, `internal/stepup/stepup.go:46-54`). Freshness
belongs to one browser session (`internal/stepup/stepup.go:12-28`). You confirm
with a passkey or the password ([sending-and-step-up.md](../sending-and-step-up.md#step-up-confirm-its-you)).
It gates the interactive attach (`internal/api/mirror.go:313`), both sends
(`internal/api/mirror_view.go:364`), `POST /send-queue`
(`internal/api/send_queue.go:252`) and ask answers
(`internal/api/mirror_ask.go:408`). The mirror service checks again on its own
(`internal/mirror/service.go:307`).

**Mirror sends write no `audit_log` row.** The only trace is a server log
line (`internal/api/mirror_view.go:533,537`). `audit_log` exists
(`internal/db/migrations/0006_accounts_schema.sql:324-335`), and invites,
flows and plugins write to it (for example `internal/users/invites.go:8`).
This design writes to it.

### The MCP server for mirrored sessions

`muster-mirror -mcp-listen` serves muster's tools (`attention_ask` and the
others) on loopback (`cmd/muster-mirror/main.go:170`,
`internal/mirrorhost/mcpproxy.go:332`). It binds each call to the pane's
Claude session and forwards it with the mirror token to `POST /mirror/mcp`
(`internal/mirrorhost/mcpproxy.go:44-58`). The operator configures
`mcpServers.muster` by hand in `~/.claude.json`, with
`Authorization: Bearer ${MUSTER_MIRROR_MCP_SECRET:-}` and the pane headers
([attention.md](../attention.md#mirrored-claude-code-sessions)). That
spelling needs the shell that starts Claude Code to export the secret. The
operator does **not** do that. Their `~/.claude.json` entry uses a
`headersHelper` (`~/.claude.json:9096`, `devrc/scripts/muster-mcp/muster-mcp-headers.py:1-30`).
The helper reads the 0600 secret file on every connection and prints the
`Authorization` and pane headers. Its docstring says why: an exported secret
would be inherited by every Bash tool call the session makes. The unit listens
on `127.0.0.1:17823` (`devrc/nix/home.nix:548-549`). A spawned `claude` gets
the MCP tools from that same user-scope entry, with nothing added by the
spawn (see [environment](#environment)).

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
  (`internal/agentspec/claudecode.go:195-319`). Its terminal is a Kubernetes
  exec (`internal/agents/terminal.go:48-52`), not `muster-mirror`. Its MCP
  config is seeded into the pod's `.claude.json` with the agent's own token
  (`cmd/ccd/seed.go:150-173`, `cmd/ccd/connect.go:56-76`).
- **ccd is not reused on the host.** It is a supervisor: it owns the session,
  respawns the CLI, serves a chat gateway and needs a hook socket. The
  operator wants a plain tmux session they own. What carries over is the
  command shape, including the `;`-chained `set-option` in the same tmux
  call.

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
  U->>M: POST /mirror/hosts/{host}/sessions {profile, dir}
  M->>M: same-origin, owner, step-up, host announces spawn and -interactive
  M->>H: {"type":"spawn","id":n,"profile":"claude","dir":"muster"}
  H->>H: validate against spawn.json, -interactive, live cap, tmux server up
  H->>T: new-session -d -s <name> -c <dir> -e PATH=… -- <argv> ; set-option @muster-spawned 1
  H->>H: RefreshNow (the listing with the new window)
  H-->>M: sessions/windows listing
  H-->>M: {"type":"spawned","id":n,"session":…,"window":"@N","pane":"%N"}
  M-->>U: redirect to the mirrored window host/session/@N
```

### Which hosts are eligible, and choosing one

A host is offered in **New session** only when all of these hold:

- it is connected now, and its token is owned by the signed-in user (the
  hub's `(owner, host)` key already scopes this);
- it is on protocol v2, and its hello announces `spawn` with at least one
  profile and one directory;
- it runs `-interactive` (the hub already records it,
  `internal/mirror/hub.go:101-102`). A host that runs `-spawn` without
  `-interactive` is **refused** ([decision 9](#decisions)). The sheet shows it
  greyed out with the reason: "this host is read-only; starting sessions
  needs `-interactive`". The host refuses too (below);
- the server runs with `MUSTER_HOST_SESSIONS=1`. That is a new variable, off by
  default, with a row in [configuration.md](../configuration.md).

The sheet lists the eligible hosts. It preselects the host this browser used
last (remembered per browser, like the Interactive choice), else the one
with the most recent activity. The directory list depends on the host, so
the host is picked first. An offline or ineligible host is shown greyed out,
with the reason.

### Host configuration: `spawn.json`

`muster-mirror -spawn` (or `MUSTER_MIRROR_SPAWN=1`) turns the feature on. It
reads `~/.config/muster-mirror/spawn.json` (`-spawn-file` overrides the path).
If the file is missing or invalid, or the host is not `-interactive`, the
hello announces `spawn: {disabled: "<reason>"}` and the host logs why. That
is never fatal. On the operator's hosts, Home Manager generates the file,
like the labels and snippets files (`devrc/nix/home.nix:518-519`, `:5456`).

```json
{
  "roots": ["~/workspace"],
  "profiles": {
    "claude":   {"argv": ["claude"],   "label": "Claude Code"},
    "opencode": {"argv": ["opencode"], "label": "opencode", "terminalOnly": true}
  },
  "sessionPool": ["scratch15", "scratch16", "scratch17", "scratch18", "scratch19", "scratch20"],
  "sessionPrefix": "m-",
  "maxLive": 6
}
```

- **`roots`:** the directories under which a session may start. The host
  offers each **direct child that holds a `.git`** (a directory, or a
  worktree's `.git` file, the test `internal/mirrorhost/repo.go` already
  uses). The offer is a list of `{key, label}`, where the key is the child's
  name. A root itself is never offered.
- **`profiles`:** the only programs that may run. Each one is a fixed argv.
  muster chooses a profile by name, and nothing else about it.
  `terminalOnly` is display only: muster says the session has no rendered
  view, and lands on the terminal.
- **Permission-bypass flags are refused** ([decision 7](#decisions)). The host
  **refuses to load** a profile whose argv holds a bypass flag for its
  program, unless the profile sets `"allowBypass": true`. That is a deliberate,
  greppable opt-in, and the operator's Nix module sets it on no profile. The
  flags are matched as `--flag value` and as `--flag=value`. Both spellings
  are one check, and missing the second would let it through.

  | program (basename of `argv[0]`) | bypass flags refused |
  |---|---|
  | `claude` | `--dangerously-skip-permissions`; `--permission-mode` with the value `bypassPermissions` |
  | `opencode` | `--auto` ("Auto-approve permissions that are not explicitly denied", `opencode --help`, v2.0.32) |

  The table covers flags only. What a program's own config allows is outside
  it (see [the opencode profile](#the-opencode-profile)).
- **`sessionPool` / `sessionPrefix`:** see [naming](#tmux-naming).
- **`maxLive`:** how many tagged (muster-started) sessions may exist at once.
  The host counts them from tmux at spawn time, with one `list-sessions` call,
  so the count holds across a restart without being stored anywhere. Why keep
  it when everything else was cut: a retried form, a stuck client or a
  scripted loop could otherwise fill the workstation with agents, each a
  process tree using real memory and API spend. That would happen by
  accident, not by attack. Live keys could start more by hand, so the cap is
  a guard against mistakes, not a security boundary.

The hello carries the profiles' names, labels and `terminalOnly`, the
directory offer and `maxLive`, never argv. It is re-sent when the file or the
set of repositories changes.

### The wire

Two request/reply pairs, announced by a hello field. There is no version
bump, because an older muster ignores an unknown field, as with `Labels`
(`internal/mirror/wire.go:180-189`):

- `hello.spawn`: `{profiles: [{name, label, terminalOnly}], dirs: [{key, label}], maxLive}`,
  or `{disabled: "<reason>"}`.
- `spawn` (muster → host): `{id, profile, dir}`. `dir` is a key from the
  host's own offer.
- `spawned` (host → muster): `{id, session, window, pane}` or
  `{id, error: <code>}`.
- `kill` (muster → host): `{id, session}`. `killed` is the reply:
  `{id}` or `{id, error: <code>}`.
- Each window in a listing gains `spawned: true` when its session carries the
  `@muster-spawned` tag. muster uses it only to decide where to show
  **Kill**. An older muster ignores it.

The error codes are fixed strings, so the UI can say why:
`spawn_disabled`, `not_interactive`, `unknown_profile`, `unknown_dir`,
`unknown_field`, `max_live`, `no_tmux_server`, `name_taken`, `tmux_failed`,
`exec_not_found`; and for `kill`, `no_such_session` and `not_muster_session`.

On the server, `Hub.Spawn` and `Hub.Kill` copy `pasteTo`'s request/reply
bookkeeping (`internal/mirror/transcript.go:648-683`), with a 10 s timeout.
A timeout after the write is **unconfirmed**, as for a paste. The page then
says "the host did not confirm; the session may have started, so check the
Agents list" and does not retry by itself. muster keeps no record of spawns,
so there is nothing to reconcile.

### What gets created

#### tmux naming

- **Session name:** the first `sessionPool` entry that does not exist as a
  session, else `<sessionPrefix><6 random hex>`, for example `m-3f9a1c`.
  **Decided ([1](#decisions)):** the operator's pool is the upper slots,
  `scratch15`–`scratch20`, falling back to `m-<hex>`. Their Nix module
  generates the pool from the same slot table as
  `devrc/scripts/tmux-scratch-slots.sh`. A spawned session becomes a scratch
  slot, so its Alt+key toggle and codename label are already wired
  (`devrc/nix/programs/tmux/default.nix:31-35`, `devrc/nix/home.nix:518-519`).
  The `m-` name never matches the bare-number rename hook
  (`devrc/.tmux.conf:214`).
- **Window name:** none is set. tmux's automatic rename names it after the
  directory and adds ` ●` while `claude` runs, as every other window does
  (`devrc/.tmux.conf:69-71`).
- **Identity: the `@muster-spawned` tag**, set by the host in the same tmux
  call that creates the session (below). The name alone is not enough: the
  operator can open `scratch15` with its Alt+key toggle, and that session must
  never be killable from muster. A tmux user option lives on the session
  itself, so it survives a `muster-mirror` restart and needs no record on
  either side.

#### The command

```sh
tmux -u new-session -d -P -F '#{session_id}\t#{window_id}\t#{pane_id}' \
  -s <name> -c <resolved dir> -x 200 -y 50 \
  -e PATH=<tmux global PATH> \
  -- <login shell> -lc '"$@"; exec "$0" -l' <login shell> <argv…> \
  \; set-option -t =<name> @muster-spawned 1
```

- It is run as an argv, never through a shell string built from input. The
  `-c` script is a constant. The login shell's path (from the user's passwd
  entry) is its `$0`, and the profile's words are its positional parameters,
  never interpolated. When the program exits, the pane execs the
  user's login shell in the same directory ([decision 3](#decisions)).
- The tag is set in the **same** tmux invocation (`;`-chained, as ccd chains
  `set-option`, `cmd/ccd/tmux.go:267-270`). A session that exists therefore
  carries the tag, and a host crash between two calls cannot leave an
  untagged muster session behind.
- `-x 200 -y 50` is ccd's size. The first attached client resizes it.
- The directory is `root/key`, resolved with `filepath.EvalSymlinks`. It must
  still be under a root, be a directory, and hold `.git`. Each check is redone
  at spawn time, not just when the list was built.
- An existing name is never reused. The host picks only a name that does not
  exist. If `new-session -s` fails because the name appeared in the meantime,
  the host tries the next pool entry, then a fresh `m-<hex>`, and gives up
  with `name_taken`. Spawns on one host are serialised with a mutex.

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
- **The MCP secret is never put in the environment.** A spawned `claude`
  reaches the muster MCP server through the user's own `mcpServers.muster`
  entry. The operator's entry uses a `headersHelper` that reads the secret
  file on each connection, so that no shell or tool call inherits it (see
  [the MCP server](#the-mcp-server-for-mirrored-sessions)). A user whose entry
  uses the documented `${MUSTER_MIRROR_MCP_SECRET:-}` header gets no MCP in
  spawned sessions until they switch to a helper. The Phase 1 docs say so in
  [attention.md](../attention.md#mirrored-claude-code-sessions).
- **Nothing from muster.** No environment value, flag or text comes from the
  request.

#### The opencode profile

[Decision 6](#decisions) puts `opencode` in slice 1 with `claude`. The
profile matches how the operator runs it today:

```json
"opencode": {"argv": ["opencode"], "label": "opencode", "terminalOnly": true}
```

- **The argv is bare `opencode`.** The operator's live opencode panes run
  exactly that, with no arguments, each started in its project directory
  (read from `ps` on 2026-10-24). The directory comes from tmux's `-c`, so
  the optional `[<directory>]` argument (`opencode --help`) is not passed. A
  session is resumed with `opencode --session <id>`
  (`devrc/scripts/find-session.py:744`), which slice 1 does not offer. The
  binary is opencode v2.0.32+6ec5682, from the `opencode-v2` flake input
  (`devrc/flake.nix:87`).
- **No model choice.** The TUI has no model flag: `-m/--model
  provider/model#variant` exists only on `opencode run` (`opencode run
  --help`, v2.0.32). The TUI takes its model from config: the operator's
  default is `openrouter/z-ai/glm-5.3-flash`
  (`devrc/scripts/opencode/opencode.jsonc:173`). In slice 1 neither profile
  takes a model ([decision 8](#decisions)), so this costs nothing: the sheet
  says "model from opencode's config" for opencode, and "model from your
  Claude Code settings" for claude. The
  `flash`, `mimo` and `pro` aliases belong to the `opencode-dispatch` wrapper
  (`devrc/scripts/opencode/opencode-dispatch:127-130`), not to opencode, and
  are not offered. **UNVERIFIED:** whether an environment variable
  (a config-dir override such as `OPENCODE_CONFIG_DIR`,
  `devrc/flake.nix:1012`) could choose a model per launch. Slice 1 does not
  rely on one.
- **Its bypass flag is `--auto`**, which the host refuses (above). **But the
  argv check is weak for opencode.** The operator's config starts from v2's
  allow-everything base policy and adds only deny rules
  (`devrc/scripts/opencode/opencode.jsonc:72-76`, `:126-128`, `:254-280`).
  Their real control is the guard plugin
  (`devrc/scripts/opencode/plugin/guard.js`). So a bare `opencode` is already
  close to unrestricted. Leaving out `--auto` changes only the few `ask`
  rules, such as `.env` reads. The design states that rather than claim the
  flag table restricts opencode. The guard is disabled by
  `DEVRC_GUARD_DISABLE=1` (`devrc/scripts/opencode/plugin/guard.js:144`), and
  no spawn sets environment from muster, so muster cannot turn it off.
- **The shared background service.** An opencode TUI attaches to a shared
  `opencode serve --service` if one is running, and its shell tools then run
  in **that** process's environment, not the spawned pane's. The operator's
  service was started from a tmux pane, so it has their shell's environment.
  Bare `opencode` keeps that habit. `--standalone` would give the session a
  private server in its own pane's environment. It is not used, to match how
  the operator runs it. **UNVERIFIED:** if no service is running, a spawned
  `opencode` starts one, which then carries the spawned pane's environment
  (PATH from tmux's global environment). The guard needs `python3` on that
  PATH and fails closed without it (`devrc/scripts/opencode/plugin/guard.js:36`,
  `:135`). Whether tmux's global PATH has `python3` on both hosts was not
  checked.
- **Credentials are files, not environment.** The OpenRouter key is in
  `~/.local/share/opencode/auth.json` (`devrc/SECRETS.md:571-579`), so
  `HOME` and the XDG paths are all the spawned pane needs. It inherits them
  from the tmux server.
- **What muster shows.** muster has a rendered view only for Claude Code (and
  Hermes, whose host mirroring stays after #249), so an opencode session is
  **terminal-only**. The New session sheet says so next to the profile, and
  the redirect lands on the terminal. The message box still pastes into it,
  because hosts announce pasting into non-Claude panes
  ([mirroring.md](../mirroring.md#rolling-upgrades)). The **send queue** does
  not apply: it waits on a Claude Code status that an opencode pane never
  reports. opencode's config has no `muster` MCP server
  (`devrc/scripts/opencode/opencode.jsonc`, no match for `muster`), so the
  attention tools are Claude Code's only.

### Mirroring handoff

1. The host runs `new-session`, with the tag set in the same call.
2. The host calls `RefreshNow` (`internal/mirrorhost/client.go:185-205`), which
   reads and sends the listing in the refresh goroutine. The new window is in
   it, with `spawned: true`.
3. Only after `RefreshNow` returns does the host write `spawned`. The
   WebSocket delivers in order, so muster processes the listing first.
4. muster redirects the browser to the mirrored window
   (`/mirror/…/<host>/<session>/<@N>`), or to its terminal for a
   `terminalOnly` profile. The window is already in muster's list, so the
   page never shows "no such window".
5. For `claude`, the Claude session id arrives on a later refresh, once
   `claude` writes its sessions file (the existing lookup,
   `internal/mirrorhost/claude.go:145-153`). Until then the view shows the
   terminal, and the rendered view says "starting".

`tmux attach -t <name>` on the host reaches the same session. muster's viewer
attaches with `-f read-only,ignore-size` or `attach -r`
([mirroring.md](../mirroring.md#read-only-interactive-and-sending)), so the
operator's own client keeps its size.

### Server side

- **Spawn route:** `POST /mirror/hosts/{host}/sessions` (form), on the session
  tier. It requires the same-origin check, the host's owner, an
  `-interactive` host and step-up ([below](#step-up-compared-with-the-send-gate)).
  There is no `mst_` or machine-token route. The form comes from
  `GET /mirror/hosts/{host}/new`. Its **Start** button disables on submit, so a
  double click sends one request. A second submit that gets through starts a
  second session, bounded by `maxLive` and removable with Kill. That is
  accepted rather than reconciled.
- **Kill route:** `POST /mirror/hosts/{host}/sessions/{session}/kill`, with
  step-up and a confirm dialog. muster sends `kill` only for a window that its
  listing marks `spawned`. The host checks the tag again itself.
- **No table, no rows.** muster stores nothing about spawns. The listing is
  the state: a spawned session is a mirrored session with `spawned: true`.
- **Logs:** a server log line per spawn, kill and refusal, as sends have
  (`internal/api/mirror_view.go:533,537`).
- **UI:** a **New session** button on the Agents list, next to the mirrored
  sessions. It opens a sheet with host, repository (searchable) and profile.
  Each profile shows where its model comes from, and opencode shows
  "terminal-only in muster". A spawned session's card has a small "started
  from muster" mark and a **Kill** action.

## Security

### The threat

**This is remote command execution on the operator's workstation, started from
a web app.** Its worst case is not "an attacker opens a terminal". It is "an
attacker starts an agent with tool access in a checkout that holds
credentials". The design bounds what can be started, where, by whom, and how
many at once. It is enabled only where a stronger power, Live keys, already
exists behind the same gate.

### Who may do it

| layer | check |
|---|---|
| server | `MUSTER_HOST_SESSIONS=1`; a signed-in browser session; the same-origin check; the host's token belongs to this user (the hub's `(owner, host)` key) |
| server | the host announced `-interactive`; otherwise refused with `not_interactive` |
| server | step-up: a sign-in inside `MUSTER_STEPUP_WINDOW`, re-checked by the mirror service before the message is written, as sends are (`internal/mirror/service.go:307`) |
| host | `-spawn` and `-interactive` are both set, and `spawn.json` loaded |
| host | the profile and directory are in its own lists; `maxLive` holds |
| host | the tmux server already exists |

Members can own hosts too, since tokens carry `owner_id`. Spawning is allowed
only on one's own host. An admin cannot spawn on a member's host: the hub
keys on the owner, and nothing here adds an admin path.

### Step-up, compared with the send gate

| action | gate |
|---|---|
| watch a mirrored session | none (today) |
| send, queue, attach interactively | fresh sign-in, 12 h (today) |
| **start a host session** (only on an `-interactive` host) | fresh sign-in, 12 h, the same gate ([decision 9](#decisions)) |
| start a host session on a host without `-interactive` | **refused** in slice 1 ([decision 9](#decisions)) |
| **kill a host session** | fresh sign-in, 12 h; it destroys running work, so the rule that "refusing is never the risky direction" does not apply |

Why the same gate, and not a stricter one? On a host that runs
`-interactive`, which both of the operator's hosts do
(`devrc/nix/home.nix:459-496`), the 12-hour gate already admits a shell: open
Live keys and type anything. A spawn bounded by `spawn.json` is **strictly
less** than that. A stricter gate on the weaker door would add friction
without closing anything.

On a host **without** `-interactive`, a spawn would be new power. So slice 1
refuses it, on the server and on the host. The per-spawn passkey that would
make it acceptable (a 5-minute confirmation, the age of `CredentialMaxAge`,
`internal/stepup/stepup.go:64`) is built only when such a host exists
([phased plan](#phased-plan), "when needed").

### The host does not trust muster

- muster sends **names**, and the host maps each to its own value. An unknown
  name is refused, and so is an unknown JSON field.
- No argv, path, environment or text from muster reaches `exec` or tmux.
- The directory is re-resolved at spawn time and must stay under a root
  after symlinks.
- The host refuses to spawn unless it runs `-interactive` itself. It does not
  rely on muster's check.
- `maxLive` is counted on the host, from tmux.
- `kill` applies only to a session that carries `@muster-spawned`. The host
  reads the tag from tmux when the request arrives. A compromised muster
  cannot kill the operator's own sessions through `kill`, though it could
  through Live keys, as today.
- The host logs every spawn, kill and refusal to its own journal (stdout
  under the systemd unit). That trace does not depend on muster.

### What a stolen muster session could do

| attacker holds | can do |
|---|---|
| a session cookie **outside** the step-up window | list hosts and sessions; nothing here |
| a cookie **inside** the window | start up to `maxLive` concurrent sessions of the allowed profiles in the offered repositories, type into them, and kill them. With `-interactive` on, which spawning requires, they could already do anything at all through Live keys. |
| a `mst_` or machine token | nothing: the routes are session-tier only |
| the host's mirror token | impersonate the host to muster; it cannot make the host spawn anything (spawns are commands *to* the host) |
| muster's server or database | send any `spawn` the host accepts, and nothing beyond `spawn.json` |

A spawned `claude` keeps the operator's permission mode (`default`), so its
tool calls still ask for permission through the operator's usual approval
path. That is a mitigation, not a boundary: an attacker who can type into the
session can also answer its prompts from muster. For opencode the
corresponding mitigation is weaker
([the opencode profile](#the-opencode-profile)).

### Revocation and kill switches

- **Revoke the mirror token** (`muster-server mirror token revoke <id>`): the
  connection ends within 30 s (`internal/mirror/service.go:22-25`), and with it
  every spawn path.
- **On the host:** drop `-spawn` (or `-interactive`), or delete `spawn.json`,
  and restart the unit. Sessions already created stay. They are ordinary tmux
  sessions.
- **On the server:** unset `MUSTER_HOST_SESSIONS`.
- **Sign out everywhere / disable the account:** ends the browser session
  that would carry the step-up.

## Lifecycle

| event | what happens |
|---|---|
| program exits | the pane becomes the user's login shell in the same directory, and the window stays mirrored ([decision 3](#decisions)). This matches how the operator runs `claude` today, inside a shell |
| **Kill** from muster | `kill-session -t =<name>`, after the tag check; the next listing lacks it |
| the operator kills it on the host | the next listing lacks it |
| host offline | New session is disabled for it, and nothing is queued: a spawn intent from an hour ago is not an intent now |
| reply never came | the page says the spawn is unconfirmed; the Agents list shows whether the session exists |
| `muster-mirror` restarts | sessions are untouched; the next listing still marks them `spawned`, from the tag |
| host reboots | the tmux server and its sessions are gone, like any other. The operator's restore script may re-create a window and `claude --resume` it (`devrc/scripts/tmux-session-restore.py:542`); it comes back as an ordinary mirrored window, without the tag (**UNVERIFIED**: whether the script copies user options), so muster offers no Kill for it |
| cleanup | muster never kills an idle session on its own; there is no muster-side state to clean |

## What slice 1 left out

Round 0 of the PR audit listed deletion candidates D1–D12. The operator cut
D1–D4, D7 and D8's dependent parts from slice 1, and kept Kill (D5)
([decision 8](#decisions)). This section records where each one went.

| id | item | disposition | why |
|---|---|---|---|
| D1 | the `host_sessions` table, its seven states, the 30-day sweep, `stepup_at` | **moved** to Phase 3, if wanted | the listing already tells muster what exists; a table pays off only for history and relinking after a reboot |
| D2 | the host ledger (`spawns.jsonl`) and request-id idempotency | **deleted** | the form stops a double submit; a rare duplicate is bounded by `maxLive` and removed with Kill, which is cheaper than reconciling across restarts |
| D3 | `audit_log` rows for spawn, kill, refused | **moved** to Phase 3, together with rows for mirror sends | sends, the greater power on these hosts, write none today; auditing spawns alone would audit the weaker door |
| D4 | `maxPerHour` persisted in the ledger; `maxLive` | **`maxPerHour` deleted; `maxLive` kept**, counted from tmux, not stored | a live cap guards against an accidental loop filling the workstation, at the cost of one `list-sessions` call; an hourly rate limit bounds an attacker who has Live keys anyway |
| D5 | Kill route, action and its check | **kept** ([decision 8](#decisions)) | simplified: the host identifies muster's sessions by the `@muster-spawned` tag, not a request id |
| D6 | the per-spawn passkey for a host without `-interactive` | **moved** to "when needed"; slice 1 refuses such hosts ([decision 9](#decisions)) | no such host exists |
| D7 | the claude model picker (`models`, `modelFlag`) | **deleted** | claude starts plain, with the model from the operator's settings ([decision 8](#decisions)) |
| D8 | the `request` listing field, the `not_listed` check, settling unconfirmed spawns | **deleted**, except a boolean `spawned` listing flag kept for Kill | the request id served D1 and D2; the ordering test pins what `not_listed` guarded; with no rows there is nothing to settle |
| D9 | bypass-table extensions: unknown `argv[0]` requires `allowBypass`, and `--flag=value` parsing | **`--flag=value` kept; the unknown-program rule deleted** | the `=value` spelling is the same check, and without it `--permission-mode=bypassPermissions` slips through; requiring `allowBypass` for every new program would make the opt-in routine and so meaningless |
| D10 | `afterExit: remain` | **deleted** | the operator chose the shell ([decision 3](#decisions)); the wrapper is now unconditional |
| D11 | the `exportMcpSecret` profile opt-in | **deleted** | the operator's MCP entry uses a `headersHelper`; a user of the env form is told to switch |
| D12 | the `MUSTER_REQUEST` environment variable | **deleted** | nothing read it |

## Phased plan

### How the estimates were made

Each estimate is built bottom-up from scratch, for this revision. Every phase
is split into work items, and each item is sized in half-day steps. The
sizes include the item's own unit tests and are compared with the nearest
existing code it copies: the paste request/reply in `internal/mirror`, the
hello capability fields, the step-up-gated mirror routes, and one Playwright
click path. A range's low end assumes the copied pattern fits as it is. The
high end allows one surprise per item. Nothing from the previous estimate was
carried over.

**Phase 1 (first slice): host 3–4, server and UI 3.5–5, total 6.5–9.**

| item | what | days |
|---|---|---|
| H1 | `-spawn`, `spawn.json` loading and validation, the bypass refusal in both spellings, the repository offer, the hello field and its disabled reasons | 1 |
| H2 | the spawn handler: the `-interactive`, profile, directory, `maxLive` and tmux-server checks; pool naming with retry; PATH from tmux's global environment; the `new-session` call with the tag and the login-shell wrapper; `RefreshNow` before the reply | 1–1.5 |
| H3 | the kill handler with its tag check; the `spawned` listing flag | 0.5 |
| H4 | host tests against a real tmux server, with stub `claude` and `opencode` programs and the mutation-checked ordering test | 0.5–1 |
| S1 | wire types, `Hub.Spawn` and `Hub.Kill` (copying `pasteTo`), keeping the hello's spawn offer in host state | 0.5–1 |
| S2 | the two routes: owner, `-interactive`, step-up, `MUSTER_HOST_SESSIONS` and its configuration row, log lines | 0.5–1 |
| S3 | UI: the New session button and sheet (host, repository, profile, the terminal-only and ineligible-host notices), the redirect, the Kill action and its confirm | 1–1.5 |
| S4 | server route tests and the Playwright click path | 1 |
| S5 | docs: a mirroring.md section, the configuration row, a decisions.md entry | 0.5 |

The devrc Home Manager change that writes `spawn.json`, with the pool
generated from the scratch-slot table, is separate, in that repository, and
not counted.

### All phases

| phase | what | host | server + UI | depends on |
|---|---|---|---|---|
| **1 (first slice)** | the table above | 3–4 | 3.5–5 | — |
| 2 | An initial prompt for `claude`, delivered through the existing send queue once the window reports `idle`, with the spawn's step-up as the queued row's (1). opencode never reports that status, so it gets none through the queue; whether opencode's `--prompt` submits on launch or only fills the input is **UNVERIFIED**, and is checked before offering it. Recent directories per host (host 0.5, server 0.5–1). A `muster` CLI command over a `mst_` route, if wanted (0.5–1) | 0.5 | 2–3 | 1 |
| 3, if wanted | History and audit: the `host_sessions` table, migration and row-level security (D1; 1–1.5); `audit_log` rows for spawn, kill **and** mirror sends together (D3; 0.5–1); relinking after a reboot by Claude session id, which needs a request-id tag on the session and in the listing (host 0.5–1, server 0.5) | 0.5–1 | 2–3 | 1 |
| 4 | Android: a signed device action `host_session_spawn` under `/api/device/actions/`, using the native step-up ([android.md](android.md#which-actions-are-gated)) | — | 1–1.5 (plus app work) | 1, Android Phase 2 |
| 5, optional | A worktree per session: a profile option to `git worktree add` a fresh branch under the repository before starting | 1–1.5 | 0.5 | 1 |
| when needed | The per-spawn passkey (5 min) for a host that runs `-spawn` without `-interactive` (D6), which replaces that host's refusal | 0–0.5 | 1–1.5 | 1 |

**Totals for Phases 1–5: host 5–7, server and UI 9–13, so 14–20
engineer-days, plus Android app work for Phase 4.** The "when needed" item
adds 1–2 if it is ever built. Sums: host 3–4 + 0.5 + 0.5–1 + 0 + 1–1.5;
server 3.5–5 + 2–3 + 2–3 + 1–1.5 + 0.5.

The first slice is the smallest that is useful by itself: start Claude Code
or opencode in a chosen repository on a chosen host, drive it from the phone,
attach to it at the desk, and kill it.

### Tests the first slice needs

- **Host, against a real tmux server** (the suite already drives tmux for
  mirrorhost):
  - an unknown profile, directory and JSON field are each refused with their
    code;
  - a symlink out of a root, and a `..` key, are refused;
  - a host without `-interactive` announces spawn as disabled and refuses a
    `spawn` with `not_interactive`;
  - no tmux server means `no_tmux_server`, and no server is started;
  - `maxLive` counts tagged sessions from tmux, so it holds across a host
    restart;
  - an existing session name is never reused, and the next pool entry is
    taken;
  - every created session carries `@muster-spawned`;
  - `kill` refuses an untagged session with `not_muster_session`, including
    an operator-made `scratch15`;
  - **both profiles' argv arrive verbatim.** Stub `claude` and `opencode`
    programs on the tmux global PATH record their argv, and the test expects
    exactly `claude` and exactly `opencode`. A profile argv word holding `;`,
    `$(…)` or spaces arrives unchanged;
  - **opencode is offered and refused like claude.** It is in the hello's
    profile list with `terminalOnly`, and a misspelled profile is refused
    with `unknown_profile`;
  - **the bypass refusal:** a profile with `--dangerously-skip-permissions`,
    `--permission-mode bypassPermissions`,
    `--permission-mode=bypassPermissions` or, for opencode, `--auto` does not
    load without `allowBypass: true`, and loads with it;
  - when the program exits, the pane is a login shell in the same directory.
- **Ordering:** a test that `spawned` is written only after the listing that
  contains the window. Mutation-check it: move the reply before
  `RefreshNow`, and the test must go red.
- **Server:**
  - stale step-up is refused before any message is written, for spawn and
    for kill;
  - another owner's host is refused;
  - a host without `-interactive` is refused with `not_interactive`;
  - an older host (no `spawn` in its hello) is not offered;
  - Kill is offered only on a `spawned` window;
  - a `terminalOnly` profile redirects to the terminal, and the sheet shows
    the notice.
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
- **The restore script after a reboot** re-creates sessions muster started,
  without their tag. They come back as ordinary mirrored windows with no Kill
  in muster. That is safe, but untidy.
- **An unconfirmed spawn, retried by hand, can start a second session.**
  There is no idempotency (D2 was deleted). `maxLive` bounds it and Kill
  removes it.
- **A single muster replica** (the tunnel is in-process). Not new, but spawn
  inherits it.
- **Profiles drifting between the two hosts.** Both come from the same Nix
  module, so the drift is in one place.
- **The `opencode` profile is terminal-only, and its argv check is weak.**
  muster has a rendered view only for Claude Code (and Hermes, whose host
  mirroring stays), so the sheet says so. Also, the operator's opencode config
  already allows nearly everything, so refusing `--auto` bounds little. The
  bound on an opencode spawn is the directory list, `maxLive` and the
  operator's guard plugin, not the flag table
  ([the opencode profile](#the-opencode-profile)).
- **opencode's shared service** carries the environment of whichever pane
  started it, so a spawned opencode's shell tools may run in a different
  environment than its pane (**UNVERIFIED** in detail;
  [the opencode profile](#the-opencode-profile)).

## Decisions

The operator made each of these decisions on 2026-10-24. The rest of this page
follows them. Where a later decision changed an earlier one, the earlier one
says so.

1. **Session naming:** a pool of the upper scratch slots, `scratch15`–`scratch20`,
   falling back to `m-<hex>`. The pool is generated from the same Nix table as
   `devrc/scripts/tmux-scratch-slots.sh` ([naming](#tmux-naming)).
2. **Directories:** repositories directly under `~/workspace` only in slice 1,
   recent directories in Phase 2, never a free path.
3. **When the program exits:** drop to the login shell.
4. **Step-up:** ~~a per-spawn passkey (5 min) on a host that runs `-spawn`
   without `-interactive`~~ **superseded by decision 9.** On an `-interactive`
   host it is the same 12-hour gate as sends.
5. **Initial prompt:** none in slice 1. Phase 2 adds it through the send queue.
6. **Profiles:** both `claude` and `opencode` in slice 1. The New session sheet
   says that an opencode session is terminal-only in muster
   ([the opencode profile](#the-opencode-profile)). This differs from the
   original recommendation, which was `claude` only. Its model choice for
   claude (`opus`, `sonnet`) is **superseded by decision 8**.
7. **Bypass flags:** the host refuses permission-bypass flags unless a profile
   sets `allowBypass: true`, and the operator's Nix module sets it on no
   profile. This covers opencode's `--auto` as well as Claude Code's flags
   ([the bypass table](#host-configuration-spawnjson)).
8. **Slice 1 is the minimal cut plus Kill.** Slice 1 contains:
   - the host allowlist: repositories, the claude and opencode argv, and the
     bypass refusal;
   - `spawn`/`spawned`, with `RefreshNow` before the reply;
   - the step-up-gated route, the New session sheet and the redirect;
   - a Kill that acts only on sessions muster started.

   Cut from slice 1: the `host_sessions` table, the host ledger and
   idempotency, `audit_log` rows, persisted caps, the claude model picker
   (claude starts plain), and whatever of the request-id plumbing only those
   needed. Their dispositions are in
   [what slice 1 left out](#what-slice-1-left-out).
9. **Hosts without `-interactive`:** slice 1 refuses to spawn on them, with a
   clear reason. The per-spawn passkey is built only when such a host exists.

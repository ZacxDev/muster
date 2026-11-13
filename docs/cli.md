# The `muster` CLI

`cmd/muster` is a machine client for muster's JSON API, used by agents, scripts
and you. It builds as the `muster` binary.

## Verbs

The verb set is pinned in two places:

- `tests/verb-ledger.sh`, run by `make verb-ledger`, interrogates the built
  binary;
- `cmd/muster/tree_test.go` walks the command tree.

| group | verbs |
|---|---|
| tasks | `task ls`, `task get`, `task create`, `task comment`, `task status` |
| an agent's own task | `agent task get`, `agent task comment`, `agent task status` |
| agents | `agent ls`, `agent messages`, `agent resolve`, `agent create` ([creating an agent without the chief](provisioner.md#creating-an-agent-without-the-chief)), `chief ask`, `chief replace` ([replacing the chief](chief.md#replacing-the-chief-on-another-kind)) |
| browser flows | `flow enqueue`, `flow ls`, `flow status`, `flow cancel` |
| credentials | `token ls`, `token revoke`, `whoami` |
| session routing | `route reply` (a mirrored session's answer to a [routed message](plugins.md#routing-a-message-to-a-session)) |
| plugins | `plugins retry <message-id>` ([retry an unconfirmed message](plugins.md#retrying-an-unconfirmed-message)), `plugins deliver-to <plugin-id> officer:<id>` / `--clear` ([bind a plugin to an officer](plugins.md#a-plugin-bound-to-an-officer)) |
| setup | `doctor` ([below](#muster-doctor)) |
| chief nudges | `nudge next` (a host's oldest chief-nudge notice as a tmux target: [below](#jumping-to-a-chief-nudge)) |
| several servers | `tenant list`, `tenant current`, `tenant use`, `tenant add`, `tenant remove` ([below](#several-servers-tenants)) |

Global flags: `--api-url`, `--token`, `--env-file` (default
`~/.muster/muster.env`), `--tenant` and `--timeout`.

## Configuration

The CLI looks for each setting in three places, from lowest to highest priority:

1. the env file;
2. the environment;
3. flags.

| setting | variable | flag |
|---|---|---|
| API URL (**no default**; it must be an absolute http(s) URL) | `MUSTER_API_URL` | `--api-url` |
| credential | `MUSTER_TOKEN` (older name: `MUSTER_HOOK_TOKEN`; `MUSTER_TOKEN` wins within one source) | `--token` |

The usual credential is a **personal API token** (`mst_…`). Mint it in the
browser at `/account/tokens`; it acts as you ([auth.md](auth.md)). Tokens cannot
be minted from the CLI. `muster token ls` and `muster token revoke <id>` manage
them, and `muster whoami` says who a credential acts as.

`agent create` is the one exception: it presents `MUSTER_AGENT_CREATE_TOKEN`, a
token of scope **Create agents** (`agent-create`), and never sends
`MUSTER_TOKEN`. The server refuses every other token on `POST /api/agents`, a
full-access one included, so the ordinary credential cannot create agents.
Where it is read from:

- **One tenant:** from the env file, then the environment, like the token. An
  exported `MUSTER_API_URL` alone still pairs with the file's tokens here (the
  one-tenant path mixes sources field by field), so the file's agent-create
  token follows it, exactly as the file's `MUSTER_TOKEN` does.
- **More than one tenant:** the pair rule below covers it too. It comes from
  the selected tenant's file, or from the environment only beside an
  environment `MUSTER_API_URL` + `MUSTER_TOKEN` pair. An exported one with
  neither makes it unusable for that tenant: `agent create` refuses (exit 2,
  nothing sent) and says where to put it; every other verb runs as usual,
  since none of them sends it.
- **A flag pair** (`--api-url` + `--token`) cannot carry one: there is no flag
  for it, and an exported one beside a flag pair names no server's pair. On a
  host with more than one tenant, `agent create` with a flag pair is therefore
  refused; use the tenant's file or an environment pair.

## Several servers (tenants)

A **tenant** is a separate muster server, at its own URL, with its own
credential. On this host each one is an env file:

```text
~/.muster/muster.env          # tenant "default": today's file, unchanged
~/.muster/tenants/work.env    # tenant "work" (0600; the directory 0700)
~/.muster/current             # one line naming the current tenant; absent = default
```

```sh
printf '%s\n' "$TOKEN" | muster tenant add work --api-url https://muster.example.com
muster tenant list                 # name, label, origin, current; never a token
muster tenant use work             # writes ~/.muster/current
muster --tenant default task ls    # one call on another tenant
muster tenant current              # which tenant, and whether flag/env/file/default chose it
muster tenant use --clear          # deletes ~/.muster/current
muster tenant remove work
```

- **Selection**, highest first: `--tenant`, `MUSTER_TENANT`, `~/.muster/current`,
  `default`. `--env-file` means "this file, not a tenant": it is refused next to
  `--tenant` and wins over `MUSTER_TENANT` and `current`.
- `tenant add` reads the token from **stdin**, never argv (`--token` is
  refused there). It refuses the name `default`, a name that exists, and an
  origin another tenant already has. It checks the token with
  `GET /api/whoami` before it writes the file, and prints the origin and the
  principal.
- A tenant file readable by group or others, or a symlink, is refused. An
  optional `MUSTER_TENANT_LABEL=` line in it sets the label `tenant list` shows.
- `tenant use` re-targets every script on this host that runs the CLI without
  naming a tenant. Such a script should set `MUSTER_TENANT`.

**The pair rule.** A host with no tenant file (no entry at all under
`~/.muster/tenants/`), no `current` file, and nothing setting `MUSTER_TENANT` or
`--tenant` resolves exactly as before, field by field ([configuration](#configuration)).
Everywhere else the selected file supplies the URL and the token **as a pair**.
A URL override (`MUSTER_API_URL`, `--api-url`) and a token override
(`MUSTER_TOKEN`, `MUSTER_HOOK_TOKEN`, `--token`) are accepted only together, and
only from one kind of source: `--api-url` with `--token`, or `MUSTER_API_URL` with
`MUSTER_TOKEN`/`MUSTER_HOOK_TOKEN`. One without the other, or a flag paired with
an exported variable, exits 2 and sends nothing. This is what stops one server's
token reaching another server. `tenant use --clear` alone does not lift it while
tenant files remain. `doctor --deploy-env` follows it too: a deploy file that
sets `MUSTER_PUBLIC_URL` without `MUSTER_HOOK_TOKEN` is refused rather than
paired with the selected tenant's token.

`tenant add` writes the file atomically (a temporary file, then a link that
never replaces an existing tenant), refuses a symlinked `~/.muster/tenants/`,
tightens a group- or world-accessible one to 0700, and warns on stderr when the
origin is `http://` (the token then travels in cleartext). A file under
`~/.muster/tenants/` named with `--env-file` gets the same 0600 and
not-a-symlink checks as `--tenant`, and so does a symlink elsewhere that points
at one (it is refused as a symlinked tenant file).

Any entry under `~/.muster/tenants/` keeps the host off the zero-touch path,
including one that is not a tenant (a `.add-*` file left by an interrupted
`tenant add`, say). `tenant list` names such entries on stderr, and so does a
pair-rule refusal, so you can see why; remove what you do not need.

The design behind tenants is in [docs/design/multi-tenant.md](design/multi-tenant.md).

**What is printed.** stdout stays JSON. With more than one tenant, a tenant
chosen by `~/.muster/current` is named on stderr (`tenant: work (muster.example.com)`);
an explicit `--tenant` or `MUSTER_TENANT` is not echoed. With more than one
tenant, the version-skew note, the credential error and the route-absent error
name the tenant too, and the credential error names that tenant's file.

## Installing it from elsewhere

This repository is public, so the CLI can be fetched and pinned by revision. In
a downstream flake, add `muster.url = "github:ZacxDev/muster"` and use
`muster.packages.${system}.muster-cli`.

- **The output is `muster-cli`, and the binary is `muster`.** The output is not
  named `muster`, because that name belongs to the service.
- **The binary answers to whatever name you invoke it as.** It uses `argv[0]` for
  its help text, its error prefix and its version-skew note. So you can symlink
  it under another name, and nothing else changes.

```sh
nix run github:ZacxDev/muster#muster-cli -- task ls
go build -o muster ./cmd/muster         # or plain Go
```

## `muster doctor`

Checks a muster installation end to end from the machine it runs on, and
prints how to fix each failure. [INSTALL.md](../INSTALL.md) ends with it.

```sh
muster doctor --deploy-env ~/muster/.env    # on the machine running the server
muster doctor --api-url https://<your-muster-address>   # on any other machine
```

On a machine other than the server, the doctor needs the server's address:
`--api-url`, or `MUSTER_API_URL` in the environment or in the env file. The API
URL has no default, so without one the doctor stops before checking anything.
INSTALL.md's Step 10 passes `--api-url` there.

| check | PASS means | read from |
|---|---|---|
| `server` | `GET /health` answers muster's document, over **https** | the server |
| `manifest` | `/manifest.webmanifest` is a standalone manifest with icons | the server |
| `hook-token` | the server enforces a hook token and accepts this one | `GET /api/doctor` |
| `mirror-hosts` | every host holding a live mirror token is connected | `GET /api/doctor` (the tunnel hub) |
| `this-host` | the server accepts THIS machine's mirror token and holds a connection from it | `GET /mirror/host`, with the token muster-mirror's settings file names |
| `mirror-service` | the `muster-mirror` user service is active here | `systemctl --user is-active` (Linux), `launchctl print` (macOS) |
| `tmux` | tmux is installed here | `tmux -V` |
| `sign-in` | the operator password signs in, and the session opens the board | `POST /login`, then `GET /tasks` |
| `push` | the VAPID keys are set | `GET /api/push/vapid-public-key` (signed in), else `GET /api/doctor` |
| `dictation` | a bundled 1.7 s spoken sample comes back as text through `POST /transcribe` | the server and its speech-to-text service |
| `features` | no feature the server reports is **misconfigured** (asked for, with a dependency missing); a feature that is merely off is not a failure. Every feature is listed below the checks, with its state and the line that turns it on | `GET /api/doctor` |
| `mcp-endpoint` | this machine's `muster-mirror` serves its local MCP endpoint (`MUSTER_MIRROR_MCP_LISTEN` in its settings) and has written its secret where muster-mirror keeps it (`MUSTER_MIRROR_MCP_SECRET_FILE`, else the user config directory: `$XDG_CONFIG_HOME` or `~/.config` on Linux, `~/Library/Application Support` on macOS). Unset is a SKIP: the agent ask tool is optional | this machine's muster-mirror settings |
| `mcp-registration` | one of Claude Code's scopes for this directory registers that endpoint (any loopback spelling: `localhost`, `127.0.0.1`, `::1`; same port, path `/mcp`), so sessions here have muster's tools. A registration in only the local or project scope passes and says sessions elsewhere have none. A FAIL prints the exact `claude mcp add-json` command | `~/.claude.json` (user scope, and `projects[<this directory>]`), `./.mcp.json` |
| `ask-hook` | Claude Code runs `muster-mirror ask` before `AskUserQuestion` (ask cards; the hook's matcher is a regular expression, so `AskUserQuestion\|Bash` counts). Absent is a SKIP with the hook to add | `~/.claude/settings.json` |

- **Feature off:** a route whose feature is off on the server (session routing for
  `route reply`, plugins for `plugins retry`, …) answers a typed 404, and the CLI
  prints `feature <id> is disabled on this server — enable with: …` and exits 12.
  A plain 404 from a server that predates the route is still exit 7.
- **Exit status:** 0 when no check FAILs, 1 when any does. A `SKIP` (a check
  that cannot be made from this machine, or `--skip`) never fails the run.
- **Output:** the report and the QR code go to **stderr**; stdout is one JSON
  document, `{"url", "ok", "checks": [{"name", "status", "detail", "fix"}]}`,
  as for every other verb, wherever the two streams go. To read only the
  report (a terminal, or an agent whose shell merges the streams), send stdout
  away: `muster doctor >/dev/null`. INSTALL.md's Step 10 does.
- **Credentials:** the server-side checks need a token (the hook token, or a
  personal token; a personal token sees only its owner's hosts); without one
  they SKIP. `--deploy-env` reads the compose `.env`: its `MUSTER_PUBLIC_URL`
  and `MUSTER_HOOK_TOKEN` beat the env file and the environment (only
  `--api-url` / `--token` beat them), and its `MUSTER_UI_PASSWORD` is used for
  the sign-in, push and dictation checks. 🔴 The password is sent only to that
  file's `MUSTER_PUBLIC_URL`; pointed at any other server, sign-in SKIPs.
- **`this-host`** reads muster-mirror's settings file (`--mirror-env`, default
  `~/.config/muster-mirror/env`) for its token file and its `MUSTER_MIRROR_URL`,
  and asks THAT server with the token. 🔴 The token is never sent anywhere
  else: when doctor is checking a different URL, this check SKIPs and names
  the mirror's server. A refused token is a FAIL; so is a connection under this
  host's name made with another token (two machines minted under one name). It
  SKIPs on a machine with no settings file.
- **Flags:** `--skip <check>[,<check>…]`, `--mirror-env`, `--mirror-unit` (default
  `muster-mirror.service`), `--mirror-label` (default
  `io.github.zacxdev.muster-mirror`).
- **The QR code** of the URL is drawn with `qrencode` when it is installed; the
  URL is printed either way.
- `GET /api/doctor` is behind the armed hook token: a server with no
  `MUSTER_HOOK_TOKEN` answers 503 with `"unarmed": true`, which is what the
  `hook-token` check reports.

## Jumping to a chief nudge

A live [chief nudge](chief-nudges.md#live-mode) is a [notice](attention.md#notices)
on your Asks list: a pointer at a session that looks like it is waiting on you.
`nudge next` prints the oldest notice whose session is on one host, with its
tmux target, for a key binding to jump to:

```sh
muster nudge next --host laptop   # the oldest notice that has a target, one JSON object
tmux switch-client -t "$(muster nudge next --host laptop | jq -r .target)"
```

- **Host:** `--host`, or `MUSTER_MIRROR_HOST` when it is not given (the name
  the host's muster-mirror reports). Neither is a usage error (exit 2).
- **What a notice is:** `key`, `host`, `target` (`<tmux session>:<window
  index>`, resolved by the server when asked, so a renumbered window resolves to
  its new index; `""` when no mirrored window holds the session now), `title`,
  `why`, `since` and `age_secs`, and `url` when the server knows its public URL.
  A notice with no target is skipped.
- **Exit status of `next`:** 0 with the notice on stdout; **11** with nothing on
  stdout when no notice with a target is waiting on that host; 1 when the
  queue could only be partly read and none was found (it does not claim none
  is waiting); and the usual codes otherwise (3 for a refused credential).
- **Credential:** a personal token or a bar-summary token. It reads the
  [attention summary](attention.md#the-summary-api), which is a user's own, so
  the shared hook token is refused (exit 3).

## Other binaries

| binary | what it is |
|---|---|
| `muster-server` | The service. Subcommands: `mirror token create <host>`, `mirror token ls`, `mirror token revoke <id>`, `vapid generate`, `invite --claim-admin`, `user disable <id>`, `user enable <id>`. |
| `muster-migrate` | Applies the schema to `DATABASE_URL`. The server also migrates itself at boot. `--rls-off` and `--rls-on` are the row-level-security rollback tools ([schema.md](schema.md)). |
| `muster-mirror` | The host agent for [mirrored sessions](mirroring.md). |
| `ccd` | The supervisor inside the `claude-code` agent image ([provisioner.md](provisioner.md)). |

The container image (`Dockerfile`) ships `/muster-server`, `/muster-migrate` and
`/agent-cli/muster`. The flake builds `muster-cli`, `muster-migrate`,
`muster-mirror` and a migrator image (`migrate-image`).

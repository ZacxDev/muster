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
| agents | `agent ls`, `agent messages`, `agent resolve`, `chief ask`, `chief replace` ([replacing the chief](chief.md#replacing-the-chief-on-another-kind)) |
| browser flows | `flow enqueue`, `flow ls`, `flow status`, `flow cancel` |
| credentials | `token ls`, `token revoke`, `whoami` |
| session routing | `route reply` (a mirrored session's answer to a [routed message](plugins.md#routing-a-message-to-a-session)) |
| plugins | `plugins retry <message-id>` ([retry an unconfirmed message](plugins.md#retrying-an-unconfirmed-message)) |
| setup | `doctor` ([below](#muster-doctor)) |

Global flags: `--api-url`, `--token`, `--env-file` (default
`~/.muster/muster.env`) and `--timeout`.

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
muster doctor                               # on any other machine
```

| check | PASS means | read from |
|---|---|---|
| `server` | `GET /health` answers muster's document, over **https** | the server |
| `manifest` | `/manifest.webmanifest` is a standalone manifest with icons | the server |
| `hook-token` | the server enforces a hook token and accepts this one | `GET /api/doctor` |
| `mirror-hosts` | every host holding a live mirror token is connected | `GET /api/doctor` (the tunnel hub) |
| `mirror-service` | the `muster-mirror` user service is active here | `systemctl --user is-active` (Linux), `launchctl print` (macOS) |
| `tmux` | tmux is installed here | `tmux -V` |
| `sign-in` | the operator password signs in, and the session opens the board | `POST /login`, then `GET /tasks` |
| `push` | the VAPID keys are set | `GET /api/push/vapid-public-key` (signed in), else `GET /api/doctor` |
| `dictation` | a bundled 1.7 s spoken sample comes back as text through `POST /transcribe` | the server and its speech-to-text service |

- **Exit status:** 0 when no check FAILs, 1 when any does. A `SKIP` (a check
  that cannot be made from this machine, or `--skip`) never fails the run.
- **Output:** the report and the QR code go to **stderr**; stdout is one JSON
  document, `{"url", "ok", "checks": [{"name", "status", "detail", "fix"}]}`,
  as for every other verb.
- **Credentials:** the server-side checks need a token (the hook token, or a
  personal token of an admin); without one they SKIP. `--deploy-env` reads the
  compose `.env`: its `MUSTER_UI_PASSWORD` for the sign-in, push and dictation
  checks, and its `MUSTER_PUBLIC_URL` and `MUSTER_HOOK_TOKEN` only where the
  env file, the environment and the flags set no URL or token.
- **Flags:** `--skip <check>[,<check>…]`, `--mirror-unit` (default
  `muster-mirror.service`), `--mirror-label` (default
  `io.github.zacxdev.muster-mirror`).
- **The QR code** of the URL is drawn with `qrencode` when it is installed; the
  URL is printed either way.
- `GET /api/doctor` is behind the armed hook token: a server with no
  `MUSTER_HOOK_TOKEN` answers 503 with `"unarmed": true`, which is what the
  `hook-token` check reports.

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

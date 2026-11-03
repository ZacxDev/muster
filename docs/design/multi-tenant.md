# Design: one client, several muster servers

**Status: phases 1 (CLI) and 2 (mirror) are implemented; phase 3 (Android) is
not.** The operator decisions below are **decided**, and the operator took the
design's recommendation on every question it raised
([decisions taken](#9-decisions-taken)), except decision 9, which was reversed
after review because its recommendation rested on a false claim, decision 7,
where the operator dropped the optional server instance identity, so no server
change remains, and decision 5, which the measurement before phase 2 changed:
a host's MCP endpoint answers a session from another tmux server as a server
with **no tools** instead of refusing it.

This page proposes letting each muster **client** (the `muster` CLI, the
`muster-mirror` host agent, and the Android app) talk to **more than one
muster server** at once. Here a *tenant* is a separate muster deployment at its
own URL, with its own account and credential on it. Nothing on the server side
becomes multi-tenant: each server stays exactly what it is today, and the
clients learn to hold several of them apart.

Where this page says how muster works today, it cites `file:line` at muster
commit `3907f23a`, and the code is the authority ([decisions.md](../decisions.md)).
Android citations are to [muster-android][ma] at commit `7af3250`, written
`android:<path>:<line>`. A claim nothing confirmed is marked **UNVERIFIED**.
Hostnames are placeholders: `muster.example.com`, `team.example.org`.

[ma]: https://github.com/ZacxDev/muster-android/tree/7af3250b750781e2360e2dfd620e32e3fb34bdda

## Summary

The design in one line each:

- **A tenant is an env file.** The CLI already reads `KEY=VALUE` files. Tenant
  `work` is `~/.muster/tenants/work.env`; today's `~/.muster/muster.env` *is*
  tenant `default`, untouched. A one-line pointer file names the current one.
  No new file format, and no migration step.
- **URL and credential travel as a pair.** A tenant's token is only ever sent
  to that tenant's origin. Today's resolver mixes sources field by field, which
  is harmless with one server and a credential leak with two. Once anything
  selects a tenant (a flag, `MUSTER_TENANT` or a `current` file) or any tenant
  file exists under `~/.muster/tenants/`, a lone URL or token override is
  refused.
- **CLI:** `muster tenant list|add|use|remove|current`, a `--tenant` flag and
  `MUSTER_TENANT`, credentials in 0600 files (no keyring), and the active
  tenant named on **stderr** so stdout stays pure JSON.
- **Mirror: one process per tenant, one tmux server per tenant.** A systemd
  template unit `muster-mirror@<tenant>` runs today's daemon against its own
  env file. The tenant that gets a session is decided by **which tmux server it
  runs in**, not by rules. That is a **routing** boundary, not a security
  boundary: anything running as the same Unix user, an interactive terminal or
  an agent included, can read every tenant's credentials. So `-interactive` is
  only for a tenant trusted as much as the host itself, and one Unix user per
  tenant is the real isolation option. Small daemon changes: each daemon
  records its tmux socket in its hook record (a per-tenant one, or today's
  legacy one), the `AskUserQuestion` hook picks the record whose socket matches
  `$TMUX`, and the daemon gains a reader-only opencode mode.
- **Android: one pairing per tenant.** A device credential, step-up key,
  UnifiedPush registration (`instance` = tenant id), notification channel group
  and WebView profile per tenant. Today's pairing becomes tenant `default` and
  keeps its push instance and channel ids, so an upgrade loses no push. Every
  notification, tap and signed action carries its tenant, and an action is
  signed and sent to the tenant its push arrived on, never to "the current
  one".
- **Server: no change.** Clients pin the **origin**, and that alone binds each
  credential to its server.
- **Order:** CLI, then mirror, then Android. Each phase ships alone and rolls
  back alone.
- **Fixed separately:** the mirror's redirect-following, a credential leak path
  even with one server (branch `fix/mirror-refuse-redirects`).

## Operator decisions

Decided; this page does not reopen them.

- **Clients in scope:** "Android app, CLI, Host mirror". The PWA is **out of
  scope**: a browser already isolates servers by origin.
- **A tenant is "Separate servers":** a different muster deployment at a
  different URL, each with its own account and credential.
- **Switching: "Active + switcher".** One tenant is in view at a time and is
  switched quickly; **every tenant still delivers notifications**.
- **Deliverable: "Trace + design doc PR".** A read-only trace, then this page,
  opened as a draft PR, with phases and open decisions. No code.
- **Open decisions: "Yes, use recommendations".** Every question this page
  raised takes its own recommendation ([decisions taken](#9-decisions-taken));
  decision 9 was later reversed, after review, and the optional instance
  identity (decision 7) was then dropped: "Drop it".

## Part 1: how each client connects today

### The CLI (`cmd/muster`)

| question | today | source |
|---|---|---|
| server URL | `MUSTER_API_URL`, from the env file, then the environment, then `--api-url` | `cmd/muster/config.go:40`, `:139-153`, `cmd/muster/root.go:160` |
| default env file | `~/.muster/muster.env`, fixed, not derived from the program name | `cmd/muster/config.go:22`, `cmd/muster/root.go:163` |
| credential | `MUSTER_TOKEN` (or its old name `MUSTER_HOOK_TOKEN`), same three sources, `--token` last. It is a personal `mst_` token, the shared hook secret, or an agent pod's own token, depending on who runs the CLI | `cmd/muster/config.go:41-59`, `:146-153` |
| where it is stored | in the env file. INSTALL.md writes `MUSTER_API_URL` and `MUSTER_TOKEN` there, umask 077 | `INSTALL.md:883-884` |
| outbound connections | plain HTTPS REST to `<apiURL><path>`, `Authorization: Bearer` on every verb; one unauthenticated `GET /health` per process for the version-skew note. No websocket, SSE, push or polling | `cmd/muster/client.go:236`, `:270-272`, `:477-500` |
| redirects | **never followed**: `CheckRedirect` returns `ErrUseLastResponse`, and a cross-host `Location` is reported as an SSO proxy | `cmd/muster/client.go:185`, `:307-318`, `:403-412` |
| cached or persisted | nothing. Config is resolved once per process | `cmd/muster/root.go:77-94` |

**What assumes exactly one server:**

1. `config` holds one `apiURL` and one `token` (`cmd/muster/config.go:66-69`).
   The comment at `cmd/muster/config.go:24-39` explains why **one URL is
   enough**: every route the CLI calls is served by one muster mux, which a test
   checks against `internal/api/testdata/routes.golden`. That argument is about
   *routes*, not *deployments*. It stays true per tenant, and says nothing about
   several deployments.
2. 🔴 **The resolver mixes sources field by field** (`cmd/muster/config.go:146-153`):
   the URL can come from the environment while the token comes from the file. With
   one server both name the same place. With two, an exported `MUSTER_API_URL`
   for tenant A plus tenant B's file sends **B's token to A**. This is the main
   one-server assumption that multi-tenant turns into a leak.
3. The auth error names the one default file (`cmd/muster/client.go:337-340`).
4. The skew probe is "one probe against one server" by design (`cmd/muster/client.go:466-472`).
5. `whoami` reports the user and token, not which server answered
   (`internal/api/api_tokens.go:218-228`).

### The mirror (`cmd/muster-mirror`, `internal/mirrorhost`)

| question | today | source |
|---|---|---|
| server URL | `-url` / `MUSTER_MIRROR_URL`; one value | `cmd/muster-mirror/main.go:94`, `:172` |
| credential | a **mirror token** `mmt_…`, minted on the server per host name (`muster-server mirror token create <host>`). Read from `-token-file` / `MUSTER_MIRROR_TOKEN_FILE`, or the value in `MUSTER_MIRROR_TOKEN`. A token without the `mmt_` prefix is refused | `cmd/muster-mirror/main.go:9-11`, `:173`, `:312-324`, `internal/mirrorhost/client.go:256-258`, `internal/mirror/wire.go:78` |
| where it is stored | INSTALL.md: `~/.config/muster-mirror/token` (0600) beside an env file `~/.config/muster-mirror/env`. The code's own default for the hook is `~/.muster/mirror-token` | `INSTALL.md:697-711`, `cmd/muster-mirror/hookconfig.go:31` |
| host identity | `-host`, default the short hostname; the token must have been minted for it | `cmd/muster-mirror/main.go:131-140`, `:174` |
| the tunnel | **one outbound WebSocket** to `<url>/mirror/connect` (`wss`), bearer token, reconnecting with jittered backoff 1 s to 60 s, 1 min after a refused token | `internal/mirrorhost/client.go:153-174`, `:275-300`, `:322-329`, `internal/mirror/wire.go:73` |
| send queue and paste | **no host-side polling.** The server's dispatcher pushes `paste`, `answer` and `cancel-ask` down the tunnel; the host replies `pasted` | `internal/mirrorhost/client.go:465-477`, `internal/mirror/wire.go:144-167` |
| local MCP endpoint | optional `-mcp-listen` on a **literal loopback address** (INSTALL examples use port 47823; a home-manager deployment uses 17823). It forwards every JSON-RPC call to `<url>/mirror/mcp` **with the same mirror token**, adding the caller's Claude session id on `tools/call` | `cmd/muster-mirror/main.go:183`, `:342-356`, `internal/mirrorhost/mcpproxy.go:25-45`, `:58`, `:284` |
| MCP secret | `-mcp-secret-file`, default `os.UserConfigDir()/muster-mirror/mcp-secret`, created 0600 | `cmd/muster-mirror/main.go:184`, `internal/mirrorhost/mcpproxy.go:352-360` |
| MCP caller check | every POST must carry the secret, no `Origin`, and an `X-Muster-Tmux` naming **the tmux server this mirror lists**; otherwise it is refused before anything is forwarded | `internal/mirrorhost/mcpproxy.go:180-196`, `:219-240` |
| `AskUserQuestion` hook | `muster-mirror ask`, a short-lived Claude Code hook. With no `-url`, it reads **one** record the daemon wrote, `~/.muster/mirror-hook.json` (URL, token-file path, Hermes status dir; no secret), and POSTs to `/mirror/ask`. The recorded token goes **only** to the recorded URL. A record with no token file (the daemon records one only when it was given `-token-file`) means the hook's default, `~/.muster/mirror-token` | `cmd/muster-mirror/hookconfig.go:13-50`, `cmd/muster-mirror/main.go:390-403`, `cmd/muster-mirror/ask.go:63-110`, `:83-86` |
| opencode | a **local** opencode pair token at `~/.muster/opencode-token`, renewed by the daemon. It is a credential for the host's opencode server, not for muster | `cmd/muster-mirror/opencode.go:19-40`, `:122-128` |
| Hermes | status files written by `muster-mirror hermes-hook` into a status dir, which the hook finds through the same one hook record | `cmd/muster-mirror/hermes.go:46-53`, `:79-103` |
| service | INSTALL.md writes one `muster-mirror.service` user unit (launchd agent `io.github.zacxdev.muster-mirror` on macOS) with `EnvironmentFile=~/.config/muster-mirror/env`. `muster doctor` checks that one unit and that one env file | `INSTALL.md:713-731`, `cmd/muster/doctor.go:54-58`, `:565-580` |
| traces | OTLP attribute `host` only | `cmd/muster-mirror/main.go:425-426` |

**What assumes exactly one server:**

1. One URL, one token, one host name per process
   (`cmd/muster-mirror/main.go:326-336`). This is fine: one process per tenant is
   the recommendation below.
2. 🔴 **One hook record per user** (`cmd/muster-mirror/hookconfig.go:35-37`). Two
   daemons that both write it overwrite each other, and the last writer gets
   **every** session's questions. `/mirror/ask` registers a question for any
   valid session id under the calling host without checking that host lists the
   session (`internal/api/mirror_ask.go:164-173`), so a question from tenant B's
   session would be opened on tenant A. A never shows it: muster shows a
   question only in a window the calling host lists as running that Claude
   session (`internal/api/mirror_ask.go:142-144`), and A's host does not list
   B's sessions. So the question appears on neither server and can only be
   answered in the terminal.
3. One MCP listen address and secret file per daemon. A second daemon on the same
   port fails to listen, logs it, and mirrors **without** MCP: that failure is
   deliberately non-fatal (`cmd/muster-mirror/main.go:342-356`), so it is quiet.
4. One systemd unit name and one env path, assumed by `muster doctor`.
5. One opencode token file, renewed by every daemon that finds it; there is no
   reader-only mode (`cmd/muster-mirror/opencode.go:122-128`, `main.go:420`).
6. 🔴 **Two clients in the mirror follow redirects, unlike the CLI.** The tunnel
   client has no `CheckRedirect` (`internal/mirrorhost/client.go:268-269`,
   `internal/mirrorhost/wire.go:40-56`), and coder/websocket v1.8.14 installs one
   that follows them (its `dial.go:89-101`). The MCP proxy uses a zero
   `http.Client{}` (`internal/mirrorhost/mcpproxy.go:290-293`). Go forwards the
   `Authorization` header on a redirect to the **same hostname or any subdomain
   of it, ignoring scheme and port** (`net/http` `shouldCopyHeaderOnRedirect`,
   Go 1.26). So a redirect to `http://muster.example.com:8080` or to
   `x.muster.example.com` carries the mirror token. This exists today; it
   matters more once a host holds several tokens. It is being **fixed
   separately**, ahead of and independent of this design (branch
   `fix/mirror-refuse-redirects`); the rest of this page assumes that fix has
   landed. (The `ask` hook and the
   opencode client already refuse redirects: `cmd/muster-mirror/ask.go:110`,
   `internal/opencodehost/client.go:100`.)

**What is already per-server by construction:** a mirror token belongs to one
host on one server, the tunnel carries everything else, and the server pushes
pastes. So "send-queue polling per tenant" (a requirement below) needs no
design: there is none.

### The Android app (muster-android)

| question | today | source |
|---|---|---|
| server URL | asked on first run; must be an absolute `https` origin; kept as `scheme://host[:port]` | `android:feature/shell/.../OriginPolicy.kt:11-38`, `android:app/.../MainActivity.kt:131-139`, `:155` |
| credential | a **device credential** `mdv_…`, confined to `/api/device/*`, redeemed from a 60 s single-use pairing code that a signed-in WebView page hands over the bridge | `android:core/auth/.../Pairing.kt:10-120`, [auth.md](../auth.md#paired-devices) |
| where it is stored | **one** `DeviceRecord` (origin, token, device id, step-up key alias, push hosts and status) sealed with AES-256-GCM under a Keystore key, in `noBackupFilesDir/device.sealed` | `android:core/auth/.../DeviceStore.kt:29-56`, `:96-143`, `android:app/.../MusterApp.kt:44-47` |
| step-up | one EC P-256 Keystore key per pairing, alias in the record | `android:core/auth/.../Pairing.kt:58-72`, [android.md](android.md#the-step-up-key) |
| REST | OkHttp to `<origin>/api/device/*` (config, push, pair, unpair, challenge, actions), **redirects refused** | `android:core/net/.../DeviceApi.kt:29-34`, `:81-215` |
| push | UnifiedPush, registered with **one fixed instance `"default"`**; `onMessage` ignores the instance and reads the one record; undecrypted messages are dropped first | `android:app/.../push/MusterPushService.kt:24-31`, `:53-58`, `android:feature/push/.../PushHandler.kt:29-32` |
| background | a WorkManager job reads `/api/device/config` so an ended pairing is forgotten; no page refresh | `android:app/.../refresh/BackgroundRefresh.kt:6-25` |
| WebView | the one origin loads in the WebView, other origins in a Custom Tab; the bridge and document-start script are restricted to that exact origin | `android:feature/shell/.../OriginPolicy.kt:13-19`, `android:feature/shell/.../BridgeMessage.kt:66` |
| cookies | the **default** `CookieManager`; changing server (only while unpaired) calls `removeAllCookies` | `android:app/.../MainActivity.kt:501-506`, `android:app/.../shell/ShellLocalState.kt:54-60` |
| cached | one shell snapshot `shell-snapshot.jpg` whose meta records its origin | `android:app/.../shell/ShellSnapshot.kt:45-49`, `android:app/.../MainActivity.kt:132` |
| notifications | six fixed channel ids, of which two (`asks`, `tasks`) are created today, with no channel group; a notification's tag is the server's tag, or `muster:<type>:<id>` | `android:feature/notify/.../Channels.kt:17-22`, `:35-45`, `android:feature/notify/.../AndroidNotifier.kt:64`, `:194` |
| taps | `PendingIntent` to `MainActivity` with the tag in the data URI and a `path` extra; no server in it | `android:app/.../MusterApp.kt:93-99` |
| signed actions | the step-up activity reads **the** pairing and signs against its origin | `android:feature/stepup/.../StepUpActivity.kt:110`, `:128`, `android:app/.../MusterApp.kt:55-60` |

**What assumes exactly one server:** all of it. The record is a singleton; the
push instance is a constant; a tag from two servers can collide (both servers
number asks from 1, so `attention-ask-41` can exist on each); a tap opens a path
on whichever origin is current; and a notification button on tenant B's
notification would be **signed and sent to the current pairing's origin**.

### Server identity

There is **no stable instance identity** a client can pin or show:

- `GET /health` returns `status`, `version`, `uptime` (`internal/api/server.go:906-913`).
- `GET /api/whoami` returns the principal, not the server (`internal/api/api_tokens.go:218-228`).
- `GET /api/device/config` returns `server_version`, push settings and feature
  switches (`internal/api/devices.go:346-361`).
- The tunnel's `welcome` carries the protocol version and host name
  (`internal/mirror/hub.go:754`).
- `MUSTER_PUBLIC_URL` exists as configuration (`internal/api/auth.go:81-84`) but
  is not served as an identity.
- The VAPID public key is per server and stable, but it is session- or
  device-gated (`internal/api/push_routes.go:74`, `internal/api/devices.go:349`)
  and exists only where push is configured. It is a fingerprint for Android
  only, not a general identity.

## Requirements pass: what to drop or simplify

Each requirement in the brief, questioned before designing. These rows were
candidate requirements proposed at dispatch, not things the operator asked for.

| requirement | verdict |
|---|---|
| A tenants file (`~/.config/muster/tenants.toml`) | **Simplify.** The CLI already parses `KEY=VALUE` env files (`cmd/muster/config.go:74-109`) and `--env-file` already selects one per call. A tenant is one env file; a TOML registry is a second format, a second parser and a migration. |
| OS keyring for credentials | **Drop.** Every other muster credential on a host is a 0600 file (mirror token, MCP secret, opencode token, env file), the CLI runs on headless hosts, in agent pods and under nix, and a keyring is absent or locked in most of those. |
| A pinned server identity | **Keep as the origin pin.** Pin the **origin**: it is what a credential is bound to, it needs no server change, and for "Separate servers" it alone keeps each credential on its own server. A server-served instance id was considered and dropped ([decision 7](#9-decisions-taken)). |
| A display colour or badge | **Keep for Android, drop for the CLI.** A terminal gets a label on stderr; colour adds nothing a label does not. |
| A single multi-server mirror | **Drop.** Today's binary already runs one tenant cleanly; several processes isolate faults (see [mirror](#3-the-mirror)). They do not isolate credentials: every process runs as the same Unix user. |
| Per-session or per-repo routing rules | **Drop.** A rule is enforced only on the listing; an interactive terminal can switch to any session of its tmux server (`cmd/muster-mirror/main.go:177`). Route by **tmux server** instead. |
| Send-queue polling per tenant | **Drop as moot.** The host never polls; pastes come down each tenant's own tunnel. |
| Android per-tenant WebView data | **Keep.** Without it, switching means signing in again each time, which breaks "switched quickly". |
| Lock-screen privacy per tenant | **Simplify.** Add the tenant's label to every notification; keep the operator's existing "full text always" rule; no hide switch. |
| A server change | **Drop.** No phase needs one ([server changes](#7-server-changes)). |

## Part 2: the design

### 1. The tenant model

A tenant has:

| field | meaning | where |
|---|---|---|
| **name** | a short local key, `[a-z0-9-]{1,32}`; `default` is reserved for today's setup | file name |
| **label** | what a person reads ("work", "team"); defaults to the name | `MUSTER_TENANT_LABEL` |
| **base URL** | the server, `https://…`; its **origin** (scheme, host, effective port) is what the credential is bound to | `MUSTER_API_URL` |
| **credential** | the token for that server | `MUSTER_TOKEN` |
| **colour** | Android only, chosen at pairing | the app's record |

**On disk (CLI):**

```text
~/.muster/muster.env          # tenant "default", exactly today's file
~/.muster/tenants/work.env    # tenant "work"   (0600, dir 0700)
~/.muster/tenants/team.env    # tenant "team"
~/.muster/current             # one line: the current tenant's name; absent = default
```

A tenant file is today's env file plus optional keys, so `muster --env-file
~/.muster/tenants/work.env …` works on **today's** CLI with no code at all:
that is the zero-code baseline for the CLI.

**Precedence.** Selecting the tenant, highest first:

1. `--tenant <name>`
2. `MUSTER_TENANT`
3. `~/.muster/current`
4. `default`

`--env-file` stays as it is and means "this file, not a tenant": combining it
with `--tenant` is refused (exit 2). Next to `MUSTER_TENANT` or a `current`
file, `--env-file` wins (both are ignored for that call). The pair rule below
applies to it whenever the call is off the zero-touch path: a `MUSTER_TENANT`,
a `current` file or a tenant file each means the host holds several tenants.

Inside the selected tenant the **file is authoritative for the pair**:

- **The zero-touch path** is: no `--tenant`, no `MUSTER_TENANT`, **no
  `current` file, and no entry at all under `~/.muster/tenants/`** (a refused or
  malformed file counts as an entry). Only there
  is resolution **exactly today's**, field by field
  (`cmd/muster/config.go:146-153`). Agent pods take it: they carry only
  environment, injected by `internal/agentspec`, and have neither a `current`
  file nor tenant files. The tenant-files condition is what makes the path
  safe: without it, a host where `muster tenant add work` ran but `muster
  tenant use` never did would resolve a lone `MUSTER_API_URL` for `work`
  against `default`'s token in `~/.muster/muster.env`, and send it to `work`.
  The same goes for `--env-file ~/.muster/tenants/work.env` with a
  `MUSTER_HOOK_TOKEN` in the environment, which today beats the file's
  `MUSTER_TOKEN` (`cmd/muster/config.go:148`, `:150`).
- **Every other resolution applies the pair rule**, including a tenant chosen
  by `current`, `current` naming `default`, and `default` reached on a host
  that has any tenant file. The rule covers every
  override of the file: `MUSTER_API_URL`, `MUSTER_TOKEN`, its old name
  `MUSTER_HOOK_TOKEN` (still read, `cmd/muster/config.go:55-59`, and the name
  agent pods and hook hosts are given, `internal/agentspec/agentspec.go:173`),
  `--api-url` and `--token`. A URL override and a token override are accepted
  only **as a pair** (both given). One without the other is refused with exit
  2, naming the tenant and the variable, and **no request is sent**. This
  closes [one-server assumption 2](#the-cli-cmdmuster).
- `current` is per-user state, so `muster tenant use` also re-targets every
  script on that host that runs the CLI without naming a tenant, background
  hook scripts included. Such a script should set `MUSTER_TENANT`. Under the
  pair rule, a script that exports only a token is refused on any host with a
  `current` file or a tenant file; its token is never sent to the newly current
  tenant.
- **The way back.** `muster tenant use --clear` deletes `current`, so selection
  falls back to `default`. The host is back on the zero-touch path once no
  tenant files remain either (`muster tenant remove` each one); until then the
  pair rule still applies, since the host still holds several tenants.

**Zero-touch migration:** nothing moves. A host with no tenant files under
`~/.muster/tenants/` and no `current` file, where nothing sets `--tenant` or
`MUSTER_TENANT`, behaves byte for byte as today. `muster tenant list` on it
prints one row, `default`, read from `~/.muster/muster.env`.

### 2. The CLI

Commands, in the kubectl-context style:

| command | does |
|---|---|
| `muster tenant list` | one JSON row per tenant: name, label, origin, current. Never a token |
| `muster tenant current` | the current tenant and where that choice came from (flag, env, file, default) |
| `muster tenant use <name>` | writes `~/.muster/current` atomically |
| `muster tenant use --clear` | deletes `~/.muster/current`, so selection falls back to `default` (the zero-touch path also needs no tenant files: [the tenant model](#1-the-tenant-model)) |
| `muster tenant add <name> --api-url <url>` | refuses a name that is `default` or already exists, and an origin another tenant already has (two tenants on one origin would make "which tenant" ambiguous); creates the 0600 file; reads the token from **stdin** (never argv, as today: `cmd/muster/config.go:62-65`); then calls `/api/whoami` and prints the origin and principal it is about to pin |
| `muster tenant remove <name>` | deletes the file; refuses `default`; clears `current` if it pointed there |

**Credentials: 0600 files, recommended over an OS keyring** (reasons in the
[requirements pass](#requirements-pass-what-to-drop-or-simplify)). The CLI
refuses a tenant file readable by group or others, as `EnsureMCPSecret` already
does for the MCP secret (`internal/mirrorhost/mcpproxy.go:364`, `:418`).

**Showing the active tenant.** stdout is JSON and nothing else
(`cmd/muster/root.go:62-63`), so the tenant goes to **stderr**, like the skew
note (`cmd/muster/client.go:474-476`):

```text
tenant: work (muster.example.com)
```

It is printed when more than one tenant exists **and** the tenant was chosen
implicitly (by `current`). An explicit `--tenant` or `MUSTER_TENANT` prints
nothing: the caller already said which. When more than one tenant exists, the
skew note, the auth error and the route-absent error name the tenant too, since
"401 from which server?" is the first question an operator asks. With one
tenant they stay as they are today. The zero-touch path has no tenant files, so
`default` is its only tenant, and it stays byte for byte the same.

The origin pin needs no check at connect time: the tenant file is the pair, so
the token only ever goes to its own origin. The skew probe is unchanged and
still never turns a working command into a failing one
(`cmd/muster/client.go:474-476`).

### 3. The mirror

#### Simplest option first: one process per tenant

```ini
# ~/.config/systemd/user/muster-mirror@.service
[Unit]
Description=muster-mirror (%i)
After=network-online.target
ConditionPathExists=%h/.config/muster-mirror/%i/token

[Service]
# "default" is reserved: today's muster-mirror.service is tenant default.
ExecCondition=/bin/sh -c 'test "%i" != default'
# Specifiers expand here, not inside the EnvironmentFile. A value the
# EnvironmentFile sets overrides these, so the env file must not set them.
Environment=MUSTER_MIRROR_HOOK_CONFIG=%h/.muster/mirror-hook.d/%i.json
Environment=MUSTER_MIRROR_WRITE_HOOK_CONFIG=1
Environment=MUSTER_MIRROR_OPENCODE_READ_ONLY=1
EnvironmentFile=%h/.config/muster-mirror/%i/env
ExecStart=<bin>/muster-mirror
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
```

Each `%i/env` names its own `MUSTER_MIRROR_URL`, `MUSTER_MIRROR_TOKEN_FILE`,
`MUSTER_MIRROR_TMUX_SOCKET`, `MUSTER_MIRROR_MCP_LISTEN` and
`MUSTER_MIRROR_MCP_SECRET_FILE`. The token must come from
`MUSTER_MIRROR_TOKEN_FILE`, not `MUSTER_MIRROR_TOKEN`, because the hook record
names the token file (see [the hook](#hook-secrets-and-the-askuserquestion-hook)).

The unit sets the hook record's path, `MUSTER_MIRROR_WRITE_HOOK_CONFIG` and the
opencode reader-only mode, but an env file overrides the unit's `Environment=`,
so the unit alone does not stop an env file from repointing the record. What
does: today's env file sets neither the record path nor the reader-only mode
(it sets only `MUSTER_MIRROR_WRITE_HOOK_CONFIG=1`, the value the unit sets
anyway, `INSTALL.md:703-708`); and if an env file does point the record at the
legacy path, the daemon's refusal backstops it
([the hook](#hook-secrets-and-the-askuserquestion-hook)). A copied env file
would also carry today's `MUSTER_MIRROR_INTERACTIVE` and `default`'s
`MUSTER_MIRROR_TOKEN_FILE`, which the unit cannot override either. So
INSTALL.md's "second server" section writes a **fresh** env file, never a copy,
with `MUSTER_MIRROR_INTERACTIVE=0` unless the tenant is trusted as much as the
host ([decision 9](#9-decisions-taken)).

Today's `muster-mirror.service` stays as it is and **is** tenant `default`.
INSTALL.md's "second server" section refuses the tenant name `default`. On
macOS one launchd agent per tenant would be the equivalent, but it has **not
been measured** on a Mac, so INSTALL.md documents only the systemd template
and says so.

**Measured on systemd 261** (its man pages, plus throwaway template units in
the user manager):

- `EnvironmentFile=` overrides `Environment=`, as `systemd.exec` says
  ("Settings from these files override settings made with Environment="): an
  env file setting `MUSTER_MIRROR_HOOK_CONFIG` to the legacy path beat the
  unit's `%h/.muster/mirror-hook.d/%i.json`.
- Specifiers expand in `Environment=` but not inside the env file: the unit's
  value came out as the user's home plus `/.muster/mirror-hook.d/work.json`,
  while `%h/x/%i` in the env file stayed literal. A transient
  `systemd-run -p Environment=` does not expand specifiers either, so a
  transient unit cannot test this; the test needs a real template unit.
- `ExecCondition=` skipped instance `default` with `Result=success`, and
  `Restart=always` with `RestartSec=1` gave `NRestarts=0`: a skipped condition
  does not loop. `ExecCondition=` needs systemd 243 or later ("Added in version
  243", `systemd.service`); older systemd ignores the key, so the refusal in
  INSTALL.md's "second server" section is the real guard there.

| | one process per tenant | one multi-server process |
|---|---|---|
| code in the daemon | small: the hook records and reader-only opencode ([in full](#mirror-changes-in-full)) | a tenant loop around everything in `main.go`, per-tenant state in `mirrorhost` |
| isolation | the OS: one tenant crashing, stuck or rate-limited cannot touch another | goroutines in one process; a panic or a stuck lock takes every tenant |
| restart or upgrade one tenant | `systemctl --user restart muster-mirror@work` | all at once |
| shared work (tmux listing, transcript reads) | done once per tenant | done once |
| verdict | **recommended** | not worth it: the shared work is cheap at a few tenants |

#### Which tenant gets a host's sessions

**Recommendation: the tmux server decides.** Tenant `default` mirrors the
default tmux server; tenant `work` mirrors `tmux -L work`
(`-tmux-socket work`, `cmd/muster-mirror/main.go:175`).

Why the tmux server and not rules:

- **It is the routing unit an interactive attach already respects.** An
  interactive attach can switch to any session of its server
  (`cmd/muster-mirror/main.go:177`). A per-session or per-repo filter would hide
  a session from tenant B's listing while B's interactive terminal could still
  switch into it.
- **The listing already gates everything else.** Opens, pastes, answers and
  scrolls are refused for a session or window the host did not list
  (`internal/mirrorhost/client.go:741-758`).
- **The MCP endpoint already routes by it.** Every request whose `$TMUX` names
  another server is refused before forwarding (`internal/mirrorhost/mcpproxy.go:180-196`);
  as built in phase 2 it is still never forwarded, and is answered as a server
  with no tools ([the local MCP endpoint](#the-local-mcp-endpoint)).
- **It is visible to the person.** Starting work for a tenant means opening a
  terminal on that server; a session's tenant cannot change because its working
  directory moved.

🔴 **The tmux server is a routing boundary, not a security boundary against
anything running as the same Unix user.** The `-interactive` flag's own help
calls an interactive terminal "effectively a SHELL here"
(`cmd/muster-mirror/main.go:177`). A shell in tenant `team`'s tmux server can
`cat` tenant `default`'s env file, mirror token and MCP secret (every tenant's
credentials are 0600 files owned by the one user), or run
`tmux -L <other> send-keys` into another tenant's sessions. An agent in a
`work` session can do the same, read another tenant's MCP secret, and send any
`X-Muster-Tmux` it likes, since the MCP check compares a header the caller
supplies (`internal/mirrorhost/mcpproxy.go:188-190`). So the MCP refusal keeps
honest callers on their own tenant; it enforces nothing against a hostile one.
What follows:

- `-interactive` is allowed only on a tenant **trusted as much as the host
  itself**, since anyone who can open its terminals holds every tenant's
  credentials on that host ([decision 9](#9-decisions-taken)).
- **One Unix user per tenant is the real isolation option.** Each tenant's
  daemon, tmux server and files then sit under their own user, and the OS
  keeps them apart. This design does not require it, but nothing in it stops
  it: every path above is under `%h`.

The cost of routing by tmux server is that the operator runs one tmux server
per tenant. That is [decided](#9-decisions-taken); the alternative, a
session-name filter, would be acceptable only for **read-only** tenants.

"Mirror all of them to every tenant" stays possible (two daemons on one tmux
server), but then both servers can see, and if interactive, type into, every
session, and only one of them should run MCP. It also turns the
`AskUserQuestion` hook off for that tmux server: both daemons' records name the
same socket, two records match, and the hook fails open, so every question
stays in the terminal ([the hook](#hook-secrets-and-the-askuserquestion-hook)).

#### The local MCP endpoint

Each tenant's daemon serves MCP on **its own loopback port** with **its own
secret file**, and forwards with **its own token**. Claude Code registers one
MCP server per tenant (`muster-work`, `muster-team`, …), each with its URL and
its own secret in its headers. In a session on tenant `work`'s tmux server,
only `muster-work` offers tools. Every other tenant's endpoint sees `$TMUX`
name a tmux server it does not mirror, forwards **nothing** upstream, and
answers the session itself as a healthy server with **no tools**
([decision 5](#9-decisions-taken)):

| request | answer from an endpoint whose tmux server is not the session's |
|---|---|
| `server/discover` (Claude Code's first POST) | JSON-RPC `-32601` "method not found", what muster's own MCP server answers to it, so Claude Code goes on to `initialize` as it does against the right server |
| `initialize` | a valid result: the protocol revision negotiated as muster does, the `tools` capability, `serverInfo`, and **no** `instructions` |
| a notification (`notifications/initialized`) | `202`, no body |
| `tools/list` | `{"tools": []}` |
| `ping` | `{}`, as muster's own server answers it (`internal/api/mcp.go`); MCP requires an answer |
| `tools/call` | refused: JSON-RPC `-32602` |
| anything else | refused: JSON-RPC `-32601` |
| not one JSON-RPC request (a batch, garbage) | JSON-RPC `-32600`, as muster |
| `GET` (Claude Code's fallback after a failure) | `405`, before the tmux check, for every caller: the transport's "no server-initiated stream", the same healthy answer the right server's sessions get |

The secret and `Origin` checks still come first, unchanged. The same answers
apply to a session outside tmux and to an endpoint that cannot read its own
tmux server. The first such caller is logged once per daemon.

So the tools a session sees are its own tenant's. That is routing, not
enforcement: a process in the session can read another tenant's secret and
send that tenant's `$TMUX` value ([above](#which-tenant-gets-a-hosts-sessions)).

**Measured before phase 2, on Claude Code 2.1.289** (which decided this): a
`400` on `initialize`, or straight after it, puts the full error text into the
model's system prompt in **every** session, telling the model to report the
failure, and `/mcp` shows ✘. An empty tool list leaves nothing in the model's
context: `claude mcp list` shows "✔ Connected" and `/mcp` shows only "⚠ no
tools". Claude Code's requests were, in order: `POST server/discover`, `POST
initialize`, `POST notifications/initialized`, `POST tools/list`, and, after a
failure, a `GET`. So the refusal the design first proposed was the noisy case,
and the local router it named as the fallback is not needed either: the empty
tool list is quiet, with one registration per tenant.

#### Hook secrets and the `AskUserQuestion` hook

- The **MCP secret** becomes per tenant (`%h/.config/muster-mirror/<tenant>/mcp-secret`).
  It already is a flag; only the default changes, and only for non-default
  tenants.
- The **hook record** must stop being one file. The daemon has no tenant name:
  it writes its record to `MUSTER_MIRROR_HOOK_CONFIG`, else
  `~/.muster/mirror-hook.json`, when `MUSTER_MIRROR_WRITE_HOOK_CONFIG` is set
  (`cmd/muster-mirror/hookconfig.go:34-38`, `:53-67`, `main.go:388-403`). So the
  template unit sets the path to `~/.muster/mirror-hook.d/<tenant>.json` (see
  the unit above). Daemon changes:
  - **Every** record the daemon writes, the legacy
    `~/.muster/mirror-hook.json` included, gains the **tmux socket path** the
    daemon mirrors. The daemon **derives** it from its configuration rather
    than querying tmux, because the record is written once at start, often
    before the tmux server exists (`main.go:388-403`; with no server,
    `display-message -p '#{socket_path}'` fails): `-S <path>` is used exactly
    as given (tmux does not resolve symlinks for `-S`; measured on tmux 3.7c,
    a relative `-S rel.sock` is reported, and put in `$TMUX`, as `rel.sock`),
    and `-L <name>` or the default server is
    `realpath(${TMUX_TMPDIR:-/tmp})/tmux-<uid>/<name>` (tmux resolves the
    directory there). **As built:** a `TMUX_TMPDIR` that does not resolve
    falls back to `realpath(/tmp)`, which is what tmux does (measured on tmux
    3.7c: `TMUX_TMPDIR` naming a missing directory put the socket in
    `/tmp/tmux-<uid>/`). Both forms equal the first field of `$TMUX`. No daemon
    writes a record without a socket (stronger than "no template instance"):
    if the path cannot be derived, it records nothing and logs why.
  - A daemon that is **not** tenant `default`'s own refuses to overwrite a
    legacy record that records a **different** socket, and logs why. "Not
    tenant `default`'s own" means it has `-tmux-socket`/`-tmux-socket-path`,
    or `MUSTER_MIRROR_HOOK_CONFIG` names the legacy path explicitly. This
    catches an env file that overrides the unit's path, once the `default`
    daemon has written its socket into the legacy record. So INSTALL.md
    upgrades and restarts today's unit before it enables any template
    instance. **As built (audit round 1):** tenant `default`'s own daemon (the
    default tmux server, no `MUSTER_MIRROR_HOOK_CONFIG`) **always** overwrites
    the legacy record. Its socket derivation changes when `TMUX_TMPDIR` is
    edited, when its unit moves off `-tmux-socket`, or when `TMUX_TMPDIR`
    stops resolving and tmux's `/tmp` is used; refusing it then locked it out
    of its own record at every later start, and every question failed open.
    A single daemon that moves **onto** `-tmux-socket` is indistinguishable
    from a misconfigured template instance and is refused; its fix is to
    delete the legacy record once. A legacy record with no socket (written by
    an older daemon) is overwritten as today, which keeps a single-daemon host
    on `-tmux-socket` or `-tmux-socket-path` working after the upgrade.
  - A daemon refuses to write a record under `mirror-hook.d/` without a token
    file (its token came from `MUSTER_MIRROR_TOKEN`), and logs why: such a
    record would make the hook fall back to the default token file, which is
    another tenant's.
- **How `muster-mirror ask` chooses a record.** The hook does not inherit the
  daemon's environment, so it reads every `~/.muster/mirror-hook.d/*.json` plus
  the legacy record, and compares the socket path in `$TMUX` (the part before
  the first comma):
  - Any record that records a socket path, per-tenant or legacy, matches when
    that path equals the one in `$TMUX`. Both come from tmux, and tmux resolves
    symlinks in the socket directory before it reports either one (measured,
    see the next point), so they compare equal as strings.
  - Only an **old** legacy record, with no socket path, falls back to the
    default-server rule: it matches when `filepath.EvalSymlinks` of the path in
    `$TMUX` equals `filepath.EvalSymlinks` of
    `${TMUX_TMPDIR:-/tmp}/tmux-<uid>/default` (as built, the derived default
    socket above, so with the same `/tmp` fallback). Comparing the literal path
    would not work: with `TMUX_TMPDIR` pointing at a symlink, `$TMUX` and
    `#{socket_path}` both showed the **resolved** directory (measured, tmux
    3.7c on Linux). On macOS `/tmp` is a symlink to `/private/tmp`, so `$TMUX`
    reads `/private/tmp/tmux-<uid>/default` there and a literal comparison
    would never match, silently stopping every question from reaching the
    phone on every macOS host (`INSTALL.md:154` supports Darwin). The macOS
    case is inferred from the Linux measurement, not run on a Mac.
  - Anything else fails open (the question stays in the terminal), as every
    other failure does today: no `$TMUX`, no match, or more than one match.
  - A per-tenant record must name its `tokenFile`. The hook never applies
    `DefaultTokenFile` to one (today it does for a record without one,
    `cmd/muster-mirror/ask.go:83-86`); a per-tenant record without a token file
    fails open. **As built (audit round 1):** a per-tenant record's URL is sent
    only that record's token file: `MUSTER_MIRROR_TOKEN`,
    `MUSTER_MIRROR_TOKEN_FILE` and `-token-file` given to the hook are
    ignored, since one hook serves every tenant and whatever it was given is
    at most one tenant's credential. The legacy record keeps today's rule (the
    environment's token, when there is one).
  - Today's rule stays: a recorded token goes only to its recorded URL
    (`cmd/muster-mirror/ask.go:63-100`).
  - **As built:** when the hook itself is given `MUSTER_MIRROR_HOOK_CONFIG`,
    it reads that one record only, still matched by socket. With no `$TMUX`
    the hook now fails open where it used to send the question to the legacy
    record's server; muster never showed such a question anyway, since it
    shows one only in a window the calling host lists.
- The **Hermes status dir** can stay shared: each daemon reads only the windows
  in its own tmux server.
- The **opencode token** is a credential for the host's opencode, not for muster.
  Today every daemon that finds the token file renews it
  (`cmd/muster-mirror/opencode.go:122-128`). Exactly one daemon should renew it:
  today's unit, tenant `default`. Every template instance runs with a new
  reader-only flag (`-opencode-read-only`, `MUSTER_MIRROR_OPENCODE_READ_ONLY`,
  set by the unit): it reads the token file, re-reads it when it changes, and
  never renews it. **As built (audit round 1):** a reader whose token file
  does not exist (or cannot be read) at its start keeps re-reading it and uses
  it once it holds a token, so a tenant started before the host was paired
  needs no restart. Whether two renewers can invalidate each other's token is
  **UNVERIFIED**, so the design avoids the question.

#### Port collisions

Each tenant's port is **explicit** in its env file; there is no derived default.
A collision is non-fatal by design (`cmd/muster-mirror/main.go:342-356`), so
it is **silent**: the second daemon mirrors without MCP and only logs it. The
optional `muster doctor --tenant` check (see "Later (optional)" below) is where
a quiet collision would become a FAIL.

#### Send-queue polling

None to design: each tenant's server pushes its pastes down its own tunnel
(`internal/mirrorhost/client.go:465-477`).

#### Mirror changes, in full

1. The daemon's hook record: record the tmux socket path (derived from the
   daemon's configuration, never queried from `#{socket_path}`: see
   [the hook](#hook-secrets-and-the-askuserquestion-hook)) in every record,
   the legacy one included; refuse to let a daemon that is not tenant
   `default`'s own overwrite a legacy record that records a different socket
   (tenant `default`'s own daemon always may); refuse a
   per-tenant record without a token file
   ([the hook](#hook-secrets-and-the-askuserquestion-hook)).
2. `ask`: choose the hook record whose recorded socket matches `$TMUX`; an old
   legacy record with no socket matches only the default tmux server, compared
   after `filepath.EvalSymlinks` on both sides; fail open otherwise; never apply
   the default token file to a per-tenant record.
3. opencode: the reader-only flag `-opencode-read-only`
   (`MUSTER_MIRROR_OPENCODE_READ_ONLY`), so exactly one daemon renews the token.
4. INSTALL.md: a "second server" section that upgrades and restarts today's
   unit first (so the legacy record records its socket), writes the template
   unit and a fresh env file per tenant (never a copy of today's), refuses the
   tenant name `default`, and states the `-interactive` rule
   ([decision 9](#9-decisions-taken)).
5. The MCP proxy: a caller from another tmux server gets a healthy server with
   no tools and nothing forwarded ([the local MCP endpoint](#the-local-mcp-endpoint),
   [decision 5](#9-decisions-taken)).

Refusing redirects on the tunnel and the MCP proxy is **not** in this list: it
is independent of tenants and is fixed separately (branch
`fix/mirror-refuse-redirects`; see
[one-server assumption 6](#the-mirror-cmdmuster-mirror-internalmirrorhost)).

**Later (optional):** `muster doctor --tenant`, checking that tenant's unit,
env, token and MCP port (it matters because a
[port collision](#port-collisions) is otherwise silent); and a `tenant`
attribute on traces and in the start log line.

### 4. Android

#### Zero-code baseline, and why it falls short

- **A work profile** (or Private Space) runs a second copy of the app with its
  own data, Keystore keys and ntfy app. It works today. It stops at two or three
  tenants, there is no switcher (switching is a profile switch), it needs a
  second ntfy distributor holding a second foreground socket, and a work profile
  brings a device-policy app the operator may not want.
- **A second install** needs a second application id, which is a build change,
  and still gives no switcher, two update streams, and two sets of channels the
  user cannot tell apart.

Both fail "Active + switcher". Neither needs a design.

#### The model

`DeviceRecord` becomes one record **per tenant**, plus the app-wide current
tenant:

```kotlin
data class TenantRecord(
    val id: String,              // local; "default" for the migrated pairing,
                                 // else random; never sent to a server
    val label: String,
    val colour: Int,
    val origin: String,          // the pin for the credential
    val token: String?, val deviceId: Long?,
    val keyAlias: String?, val keyKind: String?, val keyLevel: String?,
    val pushHosts: List<String>, val pushStatus: PushStatus,
)
data class Tenants(val current: String?, val all: List<TenantRecord>)
```

**Storage, and why it is two files.** Today's app opens `device.sealed` only
with the AAD `<app-id>/device-record/v1` (the real constant is the app's package id; see `DeviceStore.kt`), and treats anything else as
corrupt, which unpairs the phone (`android:core/auth/.../DeviceStore.kt:155-162`,
`:176`). So a version bump inside `device.sealed` would unpair any phone that
is downgraded. Instead:

- The tenants live in a **new** sealed file, `tenants.sealed`, under the same
  Keystore record key and sealing scheme
  (`android:core/auth/.../DeviceStore.kt:96-179`) with its own AAD,
  `<app-id>/tenants/v1`.
- `device.sealed` stays a valid **v1** `DeviceRecord`: a copy of tenant
  `default`, rewritten whenever that tenant changes, and empty (unpaired) if
  that tenant is removed. A downgraded app reads it as today and keeps working
  for that tenant; it ignores `tenants.sealed`.
- **Telling the old app's change from a torn write.** `tenants.sealed` records
  a digest (SHA-256) of the `device.sealed` bytes the new app last wrote. A
  change to tenant `default` writes `device.sealed` first, then
  `tenants.sealed` with the new digest. At start:
  - `device.sealed` matching the recorded digest: `tenants.sealed` is
    authoritative.
  - `device.sealed` differing from it: only then does it count as a change the
    old app made (after a downgrade and a second upgrade), and the new app
    takes tenant `default`'s fields from it. A kill between the two writes
    lands here too, and that is the right answer: `device.sealed` already holds
    the change (a rotated credential, say), so taking it keeps the change
    rather than reverting it. Writing `tenants.sealed` first would get this
    backwards.
  - If that `default` now names an origin another tenant already has (the old
    app re-paired it to that server), the new app refuses it: tenant `default`
    becomes unpaired and the app says the old version paired it to a server
    another tenant already holds, so the user can remove that device in that
    server's Account → Devices. Two tenants never share an origin, which
    pairing selection relies on ([pairings](#pairings-and-step-up)).

**Migration:** on first start, with no `tenants.sealed`, the existing record
becomes the single tenant, current, labelled by its host, with the id
`default`. Nothing re-pairs. The id `default` is reserved for it; a tenant
added later gets a random id.

**The migrated tenant keeps today's names**, so the upgrade loses nothing:

- Its UnifiedPush instance stays `"default"`, today's fixed instance
  (`android:app/.../push/MusterPushService.kt:52-56`). Its existing
  registration at the distributor keeps delivering, with no re-registration
  and without the app being opened.
- Its channels keep their legacy ids (`asks`, `conversations`, …,
  `android:feature/notify/.../Channels.kt:17-22`), so the user's channel
  settings survive, and they move into its channel group
  ([notifications](#notifications-taps-and-deep-links)). New tenants use
  `<tenant>.asks` and so on.

#### Pairings and step-up

- **One device credential per tenant.** Pairing is unchanged per server: a
  signed-in page on that tenant's origin hands a code over that WebView's bridge,
  and the bridge's origin check (`android:feature/shell/.../BridgeMessage.kt:66`)
  now selects **which tenant** the code belongs to. A code that arrives on
  tenant A's origin can only pair tenant A. That selection is unambiguous
  because two tenants never share an origin: adding a server whose origin
  another tenant already has is refused.
- **One step-up key per tenant.** Aliases are already per pairing
  (`android:core/auth/.../Pairing.kt:58`); the key is registered with that
  tenant's server only.
- "Re-confirm this phone" and unpair are per tenant. Unpairing one tenant never
  touches another's key or registration.

#### Push

- **One UnifiedPush registration per tenant, `instance` = tenant id** (so
  `"default"` for the migrated tenant, as today). The connector documents
  `instance` as its multiple-registration mechanism
  ([UnifiedPush android connector](https://unifiedpush.org/kdoc/connector/),
  accessed for this design). Each registration has its own endpoint and its own
  RFC 8291 keys, registered at its own server with its own VAPID key.
- `onMessage(message, instance)` looks the tenant up by `instance`; an unknown
  instance is dropped, after the existing undecrypted-first drop
  (`android:feature/push/.../PushHandler.kt:29-32`).
- **Misattribution is cryptographically excluded**, not just by convention: a
  message that decrypts under instance B's keys was encrypted by the holder of
  B's subscription keys, which only the phone and tenant B's server have. A
  server cannot produce a push that decrypts as another tenant's.
- One ntfy distributor serves every tenant's registration. **UNVERIFIED:** that
  one ntfy server accepts endpoints for several apps' instances from one phone
  with no extra configuration (expected, since each is an `up*` topic). Each
  tenant's server must list that ntfy host in its own `MUSTER_PUSH_EXTRA_HOSTS`.

#### Notifications, taps and deep links

- **A channel group per tenant**, holding that tenant's channels, ids
  `<tenant>.asks` and so on, except tenant `default`, which keeps the legacy
  ids (see [the model](#the-model)). The user can mute one tenant in system
  settings. Tenant `default`'s existing channels move into its group:
  `NotificationChannel.setGroup()` is "Only modifiable before the channel is
  submitted to createNotificationChannel(), unless the channel is not
  currently part of a group", and today's channels have no group. Today the
  app declares six channel ids (`android:feature/notify/.../Channels.kt:17-22`)
  but creates only `asks` and `tasks`, with no `setGroup` call and no channel
  group anywhere in the app (`Channels.kt:35-45`); the other four are created
  by the phases that post to them.
- **Tags are namespaced:** `<tenant>:<server tag>`. This removes the
  `attention-ask-41`-on-both-servers collision. A server's "withdraw" push
  (`android:feature/push/.../PushHandler.kt:36`) clears the **namespaced** tag
  of the tenant it arrived on; for tenant `default` it also clears the bare
  tag, for notifications drawn before the upgrade.
- Every notification shows the tenant's **label as its subtext** and its colour.
- **A tap carries the tenant** in the `PendingIntent`'s data URI
  (`muster-tap://<tenant>/<tag>`), which also keeps `filterEquals` distinct
  across tenants (the reason the tag is in the data today:
  `android:app/.../MusterApp.kt:85-92`). `MainActivity` switches to that tenant,
  then loads the path on **that** tenant's origin.
- 🔴 **A button carries the tenant, and the step-up activity uses only that
  tenant's pairing** (today it reads the one pairing:
  `android:feature/stepup/.../StepUpActivity.kt:110`). A button never resolves
  "the current tenant".

#### WebView data and cookies

- **One WebView profile per tenant**, through androidx.webkit's multi-profile
  API: `ProfileStore.getOrCreateProfile(name)` and
  `WebViewCompat.setProfile(webView, name)`, behind
  `WebViewFeature.MULTI_PROFILE`. Each profile has its own `CookieManager`,
  `WebStorage` and service workers, and nothing is shared between profiles;
  `CookieManager.getInstance()` keeps operating on the default profile. First
  in androidx.webkit 1.9.0-alpha01 (October 2023), first stable in 1.9.0
  (November 2023) ([androidx.webkit release notes](https://developer.android.com/jetpack/androidx/releases/webkit),
  accessed for this design). **UNVERIFIED:** that `setProfile` must be called
  before the WebView loads anything; the reference does not say, so the design
  calls it at construction.
- `WebView.setDataDirectorySuffix` (API 28) is **not** an alternative: it sets
  one data directory per **process**, before WebView is first used, so tenants
  would need one process each.
- Today's `CookieManager.getInstance().removeAllCookies` on change-server
  (`android:app/.../MainActivity.kt:504`) becomes "delete that tenant's profile"
  (`ProfileStore.deleteProfile`) on remove.
- **A second tenant requires WebView `MULTI_PROFILE`.** On an old WebView
  provider without it, the app keeps working for its one tenant and, when the
  user tries to add a second, asks them to update Android System WebView. There
  is no cookie swap: the shell never reads cookies in native code
  ([android.md](android.md#the-shell)), and it should not start.
- The shell snapshot, the bridge's allowed origin, the document-start script's
  origin rules and the background refresh job are all per tenant.

#### Switcher UX

- A chip in the top bar with the tenant's colour and label. Tap: a sheet listing
  tenants by label and colour; tap one to switch. Each tenant keeps its own
  WebView (or one is recreated on switch to bound memory; the profile keeps the
  cookies either way), so switching is instant and keeps each tenant's place.
- Adding a tenant: "Add server" in the sheet, the existing first-run flow for
  the new origin, then pairing from that origin's Account → Devices.

#### Lock-screen privacy

The operator's "full text always" for asks stays as decided
([android.md](android.md#notifications)). With several tenants the lock screen
also shows **which** server is asking, by label.

### 5. Security invariants

| invariant | how it holds |
|---|---|
| A credential is only ever sent to the origin it was minted for | CLI: the tenant file is the pair; off the zero-touch path (any `--tenant`, `MUSTER_TENANT`, `current` file or tenant file under `~/.muster/tenants/`), a lone URL or token override, `MUSTER_HOOK_TOKEN` included, is refused. On the zero-touch path resolution is today's, field by field, and a lone override or an `--env-file` mix can still send a token to another origin; that is kept for compatibility and is **not** covered by this invariant. Mirror: one process, one URL, one token; the hook picks the record whose recorded socket matches `$TMUX` (an old legacy record with no socket only on the default server, compared after resolving symlinks; otherwise fail open), a per-tenant record always names its own token file, and "recorded token to recorded URL" holds. Android: each tenant's `DeviceApi` is built from that tenant's origin; buttons and taps carry the tenant. |
| Redirects across origins are refused | CLI and Android already refuse every redirect (`cmd/muster/client.go:185`, `android:core/net/.../DeviceApi.kt:34`). The mirror's tunnel and MCP proxy follow them today (Go forwards the bearer to same-host and subdomain targets); that is fixed separately, before this design (branch `fix/mirror-refuse-redirects`), and this invariant depends on it. |
| Each tenant is pinned to its server | By origin, always, by construction: every client sends a tenant's credential only to the origin stored with it, which alone keeps the credential on its own server. |
| Nothing leaks between tenants in caches or logs | Separate files, processes, WebView profiles, snapshots and notification namespaces. Logs and traces name the tenant label, never a token. `muster tenant list` never prints a token. **Not against the same Unix user:** every tenant's files on a host belong to one user, so an interactive terminal or an agent on any tenant can read them all; isolating that needs one Unix user per tenant ([the mirror](#which-tenant-gets-a-hosts-sessions)). |
| A push cannot be misattributed | Per-tenant UnifiedPush instance with its own keys; the decrypted-only rule; tenant-namespaced tags; a button signs for the tenant it came from. |

**Residual risk, accepted:** nothing detects a different muster deployment
put behind a tenant's origin (a server rebuilt or replaced at the same URL);
clients keep sending that tenant's credential there ([decision 7](#9-decisions-taken)).

### 6. Isolation: one tenant down or slow

- **CLI:** one tenant per invocation; nothing else is contacted.
- **Mirror:** one process per tenant, each with its own backoff
  (`internal/mirrorhost/client.go:275-300`). A down server costs only its own
  reconnect loop.
- **Android:** per-tenant work (config reads, push registration, pairing) runs
  as separate jobs with their own timeouts; a slow tenant never blocks the
  switcher, which renders from local state.

### 7. Server changes

**None.** Clients pin the origin, and for "Separate servers" that alone binds
each credential to its server: a token is only ever sent to the origin it was
stored with. An optional server instance identity was proposed and dropped
([decision 7](#9-decisions-taken)).

### 8. Phasing

**Order (decided): CLI, mirror, Android.**

Why this order, not Android first: the CLI phase fixes the credential-mixing
leak and fixes the tenant model's shape for little cost; the mirror phase is
mostly configuration, plus small daemon and hook changes that make the hook
safe. Android is the largest phase and the one that delivers "every tenant
notifies"; it depends on nothing in phases 1 or 2, so if the operator's need
turns out to be phone-first it can move ahead of the mirror without other
changes.

**Phase 1: CLI.**
- Build: the tenant directory, `current`, `--tenant`, `MUSTER_TENANT`, the pair
  rule, `muster tenant …`, the stderr label, tenant-aware errors.
- Test plan: a seam test that a lone override with a selected tenant sends
  **no** request (a counting test server, positive control: the same call with
  the pair sends one), run for each of: a lone `MUSTER_API_URL`, a lone
  `MUSTER_TOKEN`, a lone `MUSTER_HOOK_TOKEN`, and a tenant chosen only by the
  `current` file (both `work` and `default`); a lone `MUSTER_API_URL` with
  `tenants/work.env` present and no `current` file sending no request;
  `--env-file ~/.muster/tenants/work.env` with only `MUSTER_HOOK_TOKEN` in the
  environment sending no request; `--env-file` next to a `current` file
  applying the pair rule; `muster tenant use --clear` deleting `current`; the
  default path resolving byte-identically to today on fixtures with no tenant
  files and no `current` file, its errors and skew note included; a 0644 tenant file refused; stdout
  still parsing as JSON with the label on stderr; the verb ledger
  (`tests/verb-ledger.sh`) extended for the new verbs.
- Rollback: delete `~/.muster/tenants/` and `~/.muster/current`; an older binary
  never reads them.

**Phase 2: mirror.**
- Before building: [measured](#to-measure-before-phase-2) how Claude Code
  shows a refused MCP server; the answer changed decision 5 (no tools instead
  of a refusal) and kept per-tenant registrations.
- Prerequisite: the separate redirect fix (branch
  `fix/mirror-refuse-redirects`) has landed. It is not part of this phase and
  carries its own 302 test.
- Build: the five [mirror changes](#mirror-changes-in-full): per-tenant hook
  records with their socket path, the hook choosing by `$TMUX`, the opencode
  reader-only flag, INSTALL.md's template unit, and the MCP proxy's empty tool
  list for another tmux server's sessions. `muster doctor --tenant`
  and the trace attribute are later and optional.
- Test plan: two daemons against two test servers on two tmux servers; a
  question from each tenant's session lands on its own server and not the other
  (count both); a question from a third tmux server with no record fails open
  and reaches **neither** server (count both, positive control: the same
  question from a recorded server reaches one); the legacy record left
  byte-identical by a template instance whose env file was copied from today's,
  and by one whose env file also points `MUSTER_MIRROR_HOOK_CONFIG` at the
  legacy path (refused because the legacy record records the default
  server's socket); with `TMUX_TMPDIR` a symlink to a real directory, a
  question from the default server matched by a new legacy record (recorded
  socket) and by an old one with no socket (the `EvalSymlinks` fallback), and
  reaching that server (positive control for the macOS `/tmp` →
  `/private/tmp` case, which this Linux test stands in for); a single daemon
  on `-tmux-socket` whose legacy record, rewritten with its socket, matches its
  own sessions; a daemon started before its tmux server exists, then the
  server started: a question from it still reaches that tenant (the record's
  socket was derived, not queried); two daemons on one tmux server making the
  hook fail open; a
  per-tenant daemon given only `MUSTER_MIRROR_TOKEN` writing
  no record; a per-tenant record without a token file never reading the default
  token file; only tenant `default` renewing the opencode token; from the wrong tmux
  server, MCP `server/discover`, `initialize`, a notification and `tools/list`
  answered as a healthy server with zero tools, `tools/call` refused, and
  **upstream counting zero requests** (positive control: the right tmux
  server's same requests are forwarded); one server killed while the other
  keeps mirroring.
- Rollback: `systemctl --user disable --now muster-mirror@<tenant>` and delete
  its record in `~/.muster/mirror-hook.d/`; the default unit's configuration
  never changed. The hook then uses the legacy record for the socket it
  records (or, for an old record with none, the default tmux server), and fails
  open for any other. An older hook binary ignores the recorded socket and
  reads the legacy record as today.

**Phase 3: Android.**
- Build: the tenant records and migration, per-tenant push instances, channel
  groups, namespaced tags, tenant-carrying taps and buttons, WebView profiles,
  the switcher.
- Test plan: migration from a real single-tenant sealed record; after that
  migration, a push to the existing instance `"default"` still arriving and
  drawing, with no re-registration and the app not opened (negative control: a
  push to an unknown instance dropped); the legacy channel ids and their
  settings kept after they move into tenant `default`'s group; a withdraw clearing both the namespaced and a pre-upgrade bare
  tag; `device.sealed` still opening under AAD v1 after a change to tenant
  `default`, and an old build of the app reading it as paired; the app killed
  between writing `device.sealed` and `tenants.sealed` during a change to
  tenant `default` (a rotated credential), then restarted, keeping the new
  credential and no other tenant changed (controls: a `device.sealed` changed
  by an old build is taken; one whose digest matches changes nothing); a re-pair on an old build to another tenant's origin
  refused on the next upgrade, leaving no two tenants on one origin; a push for
  instance B decrypting only under B and drawing only in B's group; equal server
  tags from two tenants producing two notifications; a button from tenant B
  reaching only B's test server (and A's server counting zero); removing one
  tenant leaving the other's cookies, key and registration intact; on a WebView
  without `MULTI_PROFILE` (emulator image), adding a second tenant refused with
  the update-WebView prompt and the first tenant still working.
- Rollback: a downgrade keeps working for tenant `default`, because
  `device.sealed` stays a v1 record of that tenant and the old app never opens
  `tenants.sealed` ([the model](#the-model)). The other tenants are invisible
  to the old app until it is upgraded again; tenant `default` keeps its push
  instance and channels throughout. A phone that never had a pairing before the
  upgrade has no tenant `default`, so a downgrade shows it unpaired.

### 9. Decisions taken

The operator answered "Yes, use recommendations": each question this page
raised takes the recommendation it gave. Two exceptions: decision 7, an
optional item the operator dropped, and decision 9, which changed after review
(see the note under it).

1. **Tenant config format:** one env file per tenant plus a `current` pointer,
   not a `tenants.toml`.
2. **Credential storage:** 0600 files, not an OS keyring.
3. **Showing the active tenant:** a stderr label only when the tenant was
   chosen implicitly and more than one exists; `--tenant` is not required for
   writes.
4. **Mirror routing:** one tmux server per tenant, not session-name filters.
5. **MCP with several tenants:** one registration per tenant. Each session
   gets tools only from its own tmux server's endpoint (routing, not
   enforcement).

   **Changed by the measurement before phase 2 (operator decision).** This
   first read "relying on the tmux-server refusal": an endpoint refused
   `initialize` (HTTP 400) from another tmux server. Measured on Claude Code
   2.1.289, that 400 puts the full error text into the model's system prompt
   in every session, telling it to report the failure, and `/mcp` shows ✘. So
   the endpoint now answers such a session **itself**, with a valid
   `initialize`, `202` for notifications, an empty `tools/list` and muster's
   "method not found" for `server/discover`, refuses `tools/call` and anything
   else, and forwards nothing upstream ([the local MCP endpoint](#the-local-mcp-endpoint)).
   An empty tool list leaves nothing in the model's context: `claude mcp list`
   shows "✔ Connected" and `/mcp` only "⚠ no tools". No local router is
   needed.
6. **Phase order:** CLI, mirror, Android.
7. **Server instance identity: dropped by the operator.** The design offered
   an optional server-served instance id, to tell a different deployment
   behind the same origin. The operator dropped it: each tenant's credential
   is bound to its own origin, and a different deployment behind the same
   origin is out of scope. Clients pin the origin only, and no server change
   remains.
8. **The mirror's redirect-following:** fixed now, as its own change, outside
   this design (branch `fix/mirror-refuse-redirects`).
9. **Interactive on several tenants:** `-interactive` is allowed only on a
   tenant trusted as much as the host itself. An interactive terminal is a
   shell as the host's Unix user, so it can read every tenant's credentials on
   that host and type into every tmux server. Where a tenant is not trusted
   that far, the real isolation option is one Unix user per tenant
   ([the mirror](#which-tenant-gets-a-hosts-sessions)).

   **Changed after review.** This decision first read "`-interactive` is
   allowed on more than one tenant per host, since each tenant's terminals are
   confined to its own tmux server". That recommendation rested on a false
   claim: a tmux server is a routing boundary, not a security boundary against
   anything running as the same Unix user, and the `-interactive` help itself
   calls an interactive terminal "effectively a SHELL here"
   (`cmd/muster-mirror/main.go:177`). The decision is reversed accordingly.

Two former questions are settled in the body and need no entry: a second
Android tenant requires WebView `MULTI_PROFILE` ([WebView data](#webview-data-and-cookies)),
and the lock screen gets a tenant label with no hide switch
([lock-screen privacy](#lock-screen-privacy)).

#### To measure before phase 2

- **How Claude Code displays an MCP server that refuses `initialize`.**
  **Measured on Claude Code 2.1.289: noisy.** A 400 on `initialize`, or
  straight after it, puts the error text into the model's system prompt in
  every session and shows ✘ in `/mcp`; an empty tool list shows "✔ Connected"
  in `claude mcp list` and "⚠ no tools" in `/mcp`, with nothing in the model's
  context. Claude Code sends `server/discover`, then `initialize`, then
  `notifications/initialized`, then `tools/list`, and on a failure falls back
  to a `GET`. Decision 5 changed accordingly: no tools rather than a refusal,
  still one registration per tenant, no router.

## Sources

- muster at `3907f23a`: the files cited inline.
- [muster-android at `7af3250`][ma]: the files cited inline.
- coder/websocket v1.8.14, `dial.go`, its redirect handling.
- Go 1.26 `net/http`, `shouldCopyHeaderOnRedirect` and `isDomainOrSubdomain`.
- [androidx.webkit release notes](https://developer.android.com/jetpack/androidx/releases/webkit)
  and the `WebViewCompat.setProfile` reference (accessed for this design).
- [UnifiedPush Android connector](https://unifiedpush.org/kdoc/connector/)
  (accessed for this design), on `instance`.

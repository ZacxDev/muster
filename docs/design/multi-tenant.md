# Design: one client, several muster servers

**Status: proposed. Nothing here is implemented.** The operator decisions
below are **decided**. Every other section is the recommended design, and it
ends in [open decisions](#9-open-decisions-for-the-operator) the operator still
has to make.

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
  is harmless with one server and a credential leak with two; with a tenant
  selected, a lone URL or token override is refused.
- **CLI:** `muster tenant list|add|use|remove|current`, a `--tenant` flag and
  `MUSTER_TENANT`, credentials in 0600 files (no keyring), and the active
  tenant named on **stderr** so stdout stays pure JSON.
- **Mirror: one process per tenant, one tmux server per tenant.** A systemd
  template unit `muster-mirror@<tenant>` runs today's binary unchanged against
  its own env file. The tenant that gets a session is decided by **which tmux
  server it runs in**, not by rules: that is the only boundary an interactive
  terminal cannot cross, and the local MCP endpoint already enforces it. The
  `AskUserQuestion` hook learns to pick its tenant by `$TMUX`.
- **Android: one pairing per tenant.** A device credential, step-up key,
  UnifiedPush registration (`instance` = tenant), notification channel group
  and WebView profile per tenant. Every notification, tap and signed action
  carries its tenant, and an action is signed and sent to the tenant its push
  arrived on, never to "the current one".
- **Server: one small, optional change.** An `instance` object (`id`, `name`)
  on `/health`, so a client can pin and display which server it reached.
  Clients pin the **origin** from day one and the instance id once it is served.
- **Order:** CLI, then mirror, then the server's instance identity, then
  Android. Each phase ships alone and rolls back alone.

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
| `AskUserQuestion` hook | `muster-mirror ask`, a short-lived Claude Code hook. With no `-url`, it reads **one** record the daemon wrote, `~/.muster/mirror-hook.json` (URL, token-file path, Hermes status dir; no secret), and POSTs to `/mirror/ask`. The recorded token goes **only** to the recorded URL | `cmd/muster-mirror/hookconfig.go:13-50`, `cmd/muster-mirror/main.go:390-403`, `cmd/muster-mirror/ask.go:63-110` |
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
   session would be opened on tenant A. Whether A then shows it anywhere is
   **UNVERIFIED**; the terminal picker would wait on the wrong server either way.
3. One MCP listen address and secret file per daemon. A second daemon on the same
   port fails to listen, logs it, and mirrors **without** MCP: that failure is
   deliberately non-fatal (`cmd/muster-mirror/main.go:342-356`), so it is quiet.
4. One systemd unit name and one env path, assumed by `muster doctor`.
5. One opencode token file, renewed by every daemon that finds it.
6. 🔴 **Two clients in the mirror follow redirects, unlike the CLI.** The tunnel
   client has no `CheckRedirect` (`internal/mirrorhost/client.go:268-269`,
   `internal/mirrorhost/wire.go:40-56`), and coder/websocket v1.8.14 installs one
   that follows them (its `dial.go:89-101`). The MCP proxy uses a zero
   `http.Client{}` (`internal/mirrorhost/mcpproxy.go:290-293`). Go forwards the
   `Authorization` header on a redirect to the **same hostname or any subdomain
   of it, ignoring scheme and port** (`net/http` `shouldCopyHeaderOnRedirect`,
   Go 1.26). So a redirect to `http://muster.example.com:8080` or to
   `x.muster.example.com` carries the mirror token. This exists today; it
   matters more once a host holds several tokens. (The `ask` hook and the
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
| notifications | six fixed channels; a notification's tag is the server's tag, or `muster:<type>:<id>` | `android:feature/notify/.../Channels.kt:17-22`, `android:feature/notify/.../AndroidNotifier.kt:64`, `:194` |
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

Each requirement in the brief, questioned before designing.

| requirement | verdict |
|---|---|
| A tenants file (`~/.config/muster/tenants.toml`) | **Simplify.** The CLI already parses `KEY=VALUE` env files (`cmd/muster/config.go:74-109`) and `--env-file` already selects one per call. A tenant is one env file; a TOML registry is a second format, a second parser and a migration. |
| OS keyring for credentials | **Drop.** Every other muster credential on a host is a 0600 file (mirror token, MCP secret, opencode token, env file), the CLI runs on headless hosts, in agent pods and under nix, and a keyring is absent or locked in most of those. |
| A pinned server identity | **Keep, in two steps.** Pin the **origin** now: it is what a credential is bound to, and it needs no server change. Pin an **instance id** once the server serves one, which adds detection of a different deployment behind the same origin, plus a name to display. |
| A display colour or badge | **Keep for Android, drop for the CLI.** A terminal gets a label on stderr; colour adds nothing a label does not. |
| A single multi-server mirror | **Drop.** Today's binary already runs one tenant cleanly; several processes are the isolation (see [mirror](#3-the-mirror)). |
| Per-session or per-repo routing rules | **Drop.** A rule is enforced only on the listing; an interactive terminal can switch to any session of its tmux server (`cmd/muster-mirror/main.go:177`). Route by **tmux server** instead. |
| Send-queue polling per tenant | **Drop as moot.** The host never polls; pastes come down each tenant's own tunnel. |
| Android per-tenant WebView data | **Keep.** Without it, switching means signing in again each time, which breaks "switched quickly". |
| Lock-screen privacy per tenant | **Simplify.** Add the tenant's label to every notification; keep the operator's existing "full text always" rule; defer a per-tenant hide switch (an [open decision](#9-open-decisions-for-the-operator)). |
| A server change | **Keep it optional.** Nothing in phases 1 and 2 needs it. |

## Part 2: the design

### 1. The tenant model

A tenant has:

| field | meaning | where |
|---|---|---|
| **name** | a short local key, `[a-z0-9-]{1,32}`; `default` is reserved for today's setup | file name |
| **label** | what a person reads ("work", "team"); defaults to the name | `MUSTER_TENANT_LABEL` |
| **base URL** | the server, `https://…`; its **origin** (scheme, host, effective port) is what the credential is bound to | `MUSTER_API_URL` |
| **credential** | the token for that server | `MUSTER_TOKEN` |
| **pinned identity** | the server's instance id, recorded on first contact once the server serves one | `MUSTER_TENANT_INSTANCE_ID` |
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
with `--tenant` is refused (exit 2).

Inside the selected tenant the **file is authoritative for the pair**:

- For tenant `default` with no `current` file and no `--tenant` or
  `MUSTER_TENANT`, resolution is **exactly today's**, field by field. This is
  the zero-touch path, and agent pods (which carry only environment, injected by
  `internal/agentspec`) take it.
- For any explicitly selected tenant, an exported `MUSTER_API_URL` or
  `MUSTER_TOKEN`, or `--api-url` or `--token`, is accepted only **as a pair**
  (both given). One without the other is refused with exit 2, naming the tenant
  and the variable, and **no request is sent**. This closes
  [one-server assumption 2](#the-cli-cmdmuster).

**Zero-touch migration:** nothing moves. A host with no `tenants/` directory and
no `current` file behaves byte for byte as today. `muster tenant list` on it
prints one row, `default`, read from `~/.muster/muster.env`.

### 2. The CLI

Commands, in the kubectl-context style:

| command | does |
|---|---|
| `muster tenant list` | one JSON row per tenant: name, label, origin, current, instance id if pinned. Never a token |
| `muster tenant current` | the current tenant and where that choice came from (flag, env, file, default) |
| `muster tenant use <name>` | writes `~/.muster/current` atomically |
| `muster tenant add <name> --api-url <url>` | creates the 0600 file; reads the token from **stdin** (never argv, as today: `cmd/muster/config.go:62-65`); then calls `/health` and `/api/whoami` and prints the identity it is about to pin |
| `muster tenant remove <name>` | deletes the file; refuses `default`; clears `current` if it pointed there |

**Credentials: 0600 files, recommended over an OS keyring** (reasons in the
[requirements pass](#requirements-pass-what-to-drop-or-simplify)). The CLI
refuses a tenant file readable by group or others, as `EnsureMCPSecret` already
does for the MCP secret (`internal/mirrorhost/mcpproxy.go:360-363`).

**Showing the active tenant.** stdout is JSON and nothing else
(`cmd/muster/root.go:62-63`), so the tenant goes to **stderr**, like the skew
note (`cmd/muster/client.go:474-476`):

```text
tenant: work (muster.example.com)
```

It is printed when more than one tenant exists **and** the tenant was chosen
implicitly (by `current`). An explicit `--tenant` or `MUSTER_TENANT` prints
nothing: the caller already said which. The skew note, the auth error and the
route-absent error name the tenant too, since "401 from which server?" is the
first question an operator asks.

**Identity check on connect.** The skew probe already calls `/health` once per
process (`cmd/muster/client.go:477-500`). When the tenant has a pinned instance
id and `/health` serves a different one, the CLI exits with a new code before
the verb runs. A server that serves no id is accepted (origin pin only).

### 3. The mirror

#### Simplest option first: one process per tenant

```ini
# ~/.config/systemd/user/muster-mirror@.service
[Unit]
Description=muster-mirror (%i)
After=network-online.target
ConditionPathExists=%h/.config/muster-mirror/%i/token

[Service]
EnvironmentFile=%h/.config/muster-mirror/%i/env
ExecStart=<bin>/muster-mirror
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
```

Each `%i/env` names its own `MUSTER_MIRROR_URL`, `MUSTER_MIRROR_TOKEN_FILE`,
`MUSTER_MIRROR_TMUX_SOCKET`, `MUSTER_MIRROR_MCP_LISTEN` and
`MUSTER_MIRROR_MCP_SECRET_FILE`. Today's `muster-mirror.service` stays as it is
and **is** tenant `default`. On macOS the same thing is one launchd agent per
tenant, label `io.github.zacxdev.muster-mirror.<tenant>`.

| | one process per tenant | one multi-server process |
|---|---|---|
| code in the daemon | almost none (the hook, below) | a tenant loop around everything in `main.go`, per-tenant state in `mirrorhost` |
| isolation | the OS: one tenant crashing, stuck or rate-limited cannot touch another | goroutines in one process; a panic or a stuck lock takes every tenant |
| restart or upgrade one tenant | `systemctl --user restart muster-mirror@work` | all at once |
| shared work (tmux listing, transcript reads) | done once per tenant | done once |
| verdict | **recommended** | not worth it: the shared work is cheap at a few tenants |

#### Which tenant gets a host's sessions

**Recommendation: the tmux server decides.** Tenant `default` mirrors the
default tmux server; tenant `work` mirrors `tmux -L work`
(`-tmux-socket work`, `cmd/muster-mirror/main.go:175`).

Why the tmux server and not rules:

- **It is the boundary an interactive terminal cannot cross.** An interactive
  attach can switch to any session of its server (`cmd/muster-mirror/main.go:177`).
  A per-session or per-repo filter would hide a session from tenant B's listing
  while B's interactive terminal could still switch into it.
- **The listing already gates everything else.** Opens, pastes, answers and
  scrolls are refused for a session or window the host did not list
  (`internal/mirrorhost/client.go:741-758`).
- **The MCP endpoint already enforces it.** Every request whose `$TMUX` names
  another server is refused before forwarding (`internal/mirrorhost/mcpproxy.go:180-196`).
- **It is visible to the person.** Starting work for a tenant means opening a
  terminal on that server; a session's tenant cannot change because its working
  directory moved.

The cost is that the operator runs one tmux server per tenant. That is an
[open decision](#9-open-decisions-for-the-operator); the alternative, a
session-name filter, is acceptable only for **read-only** tenants.

"Mirror all of them to every tenant" stays possible (two daemons on one tmux
server), but then both servers can see, and if interactive, type into, every
session, and only one of them should run MCP.

#### The local MCP endpoint

Each tenant's daemon serves MCP on **its own loopback port** with **its own
secret file**, and forwards with **its own token**. Claude Code registers one
MCP server per tenant (`muster-work`, `muster-team`, …), each with its URL and
a headers helper reading that tenant's secret. In a session on tenant `work`'s
tmux server, only `muster-work` answers; the others refuse `initialize` because
`$TMUX` names the wrong server. So the tools a session sees are its own
tenant's.

**UNVERIFIED:** how Claude Code shows an MCP server that refuses `initialize`
(a quiet "failed" entry, or something noisier). If it is noisy, the fallback is
a small local router: one port that dispatches by `X-Muster-Tmux` to the right
tenant's endpoint. That is an [open decision](#9-open-decisions-for-the-operator).

#### Hook secrets and the `AskUserQuestion` hook

- The **MCP secret** becomes per tenant (`%h/.config/muster-mirror/<tenant>/mcp-secret`).
  It already is a flag; only the default changes, and only for non-default
  tenants.
- The **hook record** must stop being one file. Each daemon writes
  `~/.muster/mirror-hook.d/<tenant>.json`, adding the **tmux socket path** it
  mirrors. `muster-mirror ask` reads `$TMUX`, picks the record whose socket
  matches, and keeps today's rule that a recorded token goes only to its
  recorded URL (`cmd/muster-mirror/ask.go:63-100`). With no match it fails open
  (the question stays in the terminal), as every other failure does today.
  Today's `~/.muster/mirror-hook.json` stays readable as tenant `default`'s.
- The **Hermes status dir** can stay shared: each daemon reads only the windows
  in its own tmux server.
- The **opencode token** is a credential for the host's opencode, not for muster.
  Exactly one daemon renews it (tenant `default`, or a named owner); the others
  read it. Whether two renewers can invalidate each other's token is
  **UNVERIFIED**, so the design avoids the question.

#### Port collisions

Each tenant's port is **explicit** in its env file; there is no derived default.
A collision is non-fatal by design (`cmd/muster-mirror/main.go:342-356`), so
`muster doctor` gains a per-tenant check that the MCP endpoint answers, which
is where a quiet collision becomes a FAIL.

#### Send-queue polling

None to design: each tenant's server pushes its pastes down its own tunnel
(`internal/mirrorhost/client.go:465-477`).

#### Mirror changes, in full

1. `ask`: choose the hook record by `$TMUX`; daemons write per-tenant records.
2. Refuse redirects on the tunnel and the MCP proxy (independent of tenants;
   see [one-server assumption 6](#the-mirror-cmdmuster-mirror-internalmirrorhost)).
3. A `tenant` attribute on traces and in the start log line.
4. `muster doctor --tenant`: check that tenant's unit, env, token and MCP port.
5. INSTALL.md: a "second server" section that writes the template unit.

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
    val id: String,              // local, random; never sent to a server
    val label: String,
    val colour: Int,
    val origin: String,          // the pin for the credential
    val instanceId: String?,     // pinned once the server serves one
    val token: String?, val deviceId: Long?,
    val keyAlias: String?, val keyKind: String?, val keyLevel: String?,
    val pushHosts: List<String>, val pushStatus: PushStatus,
)
data class Tenants(val current: String?, val all: List<TenantRecord>)
```

Still one sealed file, still under the same Keystore record key and AAD scheme
(`android:core/auth/.../DeviceStore.kt:96-179`), with a version bump in the AAD.
**Migration:** an existing record becomes the single tenant, labelled by its
host, current. Nothing re-pairs.

#### Pairings and step-up

- **One device credential per tenant.** Pairing is unchanged per server: a
  signed-in page on that tenant's origin hands a code over that WebView's bridge,
  and the bridge's origin check (`android:feature/shell/.../BridgeMessage.kt:66`)
  now selects **which tenant** the code belongs to. A code that arrives on
  tenant A's origin can only pair tenant A.
- **One step-up key per tenant.** Aliases are already per pairing
  (`android:core/auth/.../Pairing.kt:58`); the key is registered with that
  tenant's server only.
- "Re-confirm this phone" and unpair are per tenant. Unpairing one tenant never
  touches another's key or registration.

#### Push

- **One UnifiedPush registration per tenant, `instance` = tenant id.** The
  connector documents `instance` as its multiple-registration mechanism
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

- **A channel group per tenant**, holding the six channels
  (`android:feature/notify/.../Channels.kt:17-22`), ids `<tenant>.asks` and so
  on. The user can mute one tenant in system settings.
- **Tags are namespaced:** `<tenant>:<server tag>`. This removes the
  `attention-ask-41`-on-both-servers collision.
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
- **Without `MULTI_PROFILE`** (an old WebView provider), the app pairs a second
  tenant for push and actions, but only one tenant gets a signed-in WebView;
  opening another asks to sign in again there. Recommended over a cookie swap,
  which would copy session cookies through native code that today never reads
  them ([android.md](android.md#the-shell)).
- The shell snapshot, the bridge's allowed origin, the document-start script's
  origin rules and the background refresh job are all per tenant.

#### Switcher UX

- A chip in the top bar with the tenant's colour and label. Tap: a sheet listing
  tenants with their unread counts; tap one to switch. Each tenant keeps its own
  WebView (or one is recreated on switch to bound memory; the profile keeps the
  cookies either way), so switching is instant and keeps each tenant's place.
- Adding a tenant: "Add server" in the sheet, the existing first-run flow for
  the new origin, then pairing from that origin's Account → Devices.

#### Lock-screen privacy

The operator's "full text always" for asks stays as decided
([android.md](android.md#notifications)). With several tenants the lock screen
also shows **which** server is asking, by label. A per-tenant "hide content on
the lock screen" switch is deferred to an
[open decision](#9-open-decisions-for-the-operator).

### 5. Security invariants

| invariant | how it holds |
|---|---|
| A credential is only ever sent to the origin it was minted for | CLI: the tenant file is the pair; a lone URL or token override is refused. Mirror: one process, one URL, one token; the hook picks its record by tmux server and keeps "recorded token to recorded URL". Android: each tenant's `DeviceApi` is built from that tenant's origin; buttons and taps carry the tenant. |
| Redirects across origins are refused | CLI and Android already refuse every redirect (`cmd/muster/client.go:185`, `android:core/net/.../DeviceApi.kt:34`). The mirror's tunnel and MCP proxy must be changed to refuse them, since Go forwards the bearer to same-host and subdomain targets. |
| The pinned identity is verified on connect | Origin: always, by construction. Instance id: checked against `/health` by the CLI and mirror, and against `/api/device/config` by the app, once served; a mismatch stops that tenant and says so. |
| Nothing leaks between tenants in caches or logs | Separate files, processes, WebView profiles, snapshots and notification namespaces. Logs and traces name the tenant label, never a token. `muster tenant list` never prints a token. |
| A push cannot be misattributed | Per-tenant UnifiedPush instance with its own keys; the decrypted-only rule; tenant-namespaced tags; a button signs for the tenant it came from. |

### 6. Isolation: one tenant down or slow

- **CLI:** one tenant per invocation; nothing else is contacted.
- **Mirror:** one process per tenant, each with its own backoff
  (`internal/mirrorhost/client.go:275-300`). A down server costs only its own
  reconnect loop.
- **Android:** per-tenant work (config reads, push registration, pairing) runs
  as separate jobs with their own timeouts; a slow tenant never blocks the
  switcher, which renders from local state. A tenant that fails its identity
  check is shown as such and does not stop the others.

### 7. Server changes

**None are required** for phases 1 and 2. Clients pin the origin.

**One small, recommended change** for phase 3: an `instance` object on
`GET /health`, also in `GET /api/device/config` and the tunnel's `welcome`:

```json
{"status": "ok", "version": "…", "uptime": 12.3,
 "instance": {"id": "6f0c…", "name": "team"}}
```

- `id`: random, generated once on first boot and kept in the database, so it
  survives restarts and redeploys, and changes when it is a different deployment.
- `name`: `MUSTER_INSTANCE_NAME`, else the host of `MUSTER_PUBLIC_URL`, else
  omitted.
- `/health` is unauthenticated, so the id is public. It is an identifier, not a
  secret, and must never be used as one.

### 8. Phasing

**Recommended order: CLI, mirror, server identity, Android.**

Why this order, not Android first: the CLI phase fixes the credential-mixing
leak and fixes the tenant model's shape for little cost; the mirror phase is
mostly configuration and makes the hook safe; the identity change is small and
lands before the phone needs it. Android is the largest phase and the one that
delivers "every tenant notifies", so if the operator's need is phone-first, it
can move ahead of the mirror (an
[open decision](#9-open-decisions-for-the-operator)); it depends on nothing in
phases 1 and 2.

**Phase 1: CLI.**
- Build: the tenant directory, `current`, `--tenant`, `MUSTER_TENANT`, the pair
  rule, `muster tenant …`, the stderr label, tenant-aware errors.
- Test plan: a seam test that a lone `MUSTER_API_URL` with a selected tenant
  sends **no** request (a counting test server, positive control: the same call
  with the pair sends one); the default path resolving byte-identically to today
  on fixtures with no `tenants/` directory; a 0644 tenant file refused; stdout
  still parsing as JSON with the label on stderr; the verb ledger
  (`tests/verb-ledger.sh`) extended for the new verbs.
- Rollback: delete `~/.muster/tenants/` and `~/.muster/current`; an older binary
  never reads them.

**Phase 2: mirror.**
- Build: per-tenant hook records chosen by `$TMUX`; refuse redirects on tunnel
  and MCP proxy; tenant in traces; `muster doctor --tenant`; INSTALL.md's
  template unit.
- Test plan: two daemons against two test servers on two tmux servers; a
  question from each tenant's session lands on its own server and not the other
  (count both); an MCP `initialize` from the wrong tmux server refused; a 302
  from the test server to a same-host other port carries **no**
  `Authorization` (red against today's code first); one server killed while the
  other keeps mirroring.
- Rollback: `systemctl --user disable --now muster-mirror@<tenant>`; the
  default unit never changed. The hook falls back to the legacy record.

**Phase 3: server instance identity.**
- Build: the `instance` object, its migration, the env variable.
- Test plan: the id is stable across a restart and differs between two
  databases; the routes golden updated; `docs/configuration.md` and its test.
- Rollback: an older client ignores the field; a newer client treats its absence
  as origin-only pinning.

**Phase 4: Android.**
- Build: the tenant records and migration, per-tenant push instances, channel
  groups, namespaced tags, tenant-carrying taps and buttons, WebView profiles,
  the switcher.
- Test plan: migration from a real single-tenant sealed record; a push for
  instance B decrypting only under B and drawing only in B's group; equal server
  tags from two tenants producing two notifications; a button from tenant B
  reaching only B's test server (and A's server counting zero); removing one
  tenant leaving the other's cookies, key and registration intact; a WebView
  without `MULTI_PROFILE` (emulator image) taking the one-signed-in-tenant path.
- Rollback: the app keeps reading the old record format until the migration has
  succeeded, and a downgrade reads tenant 0 back as the single record (the
  sealed file keeps a single-record copy for one release).

### 9. Open decisions for the operator

Each has a recommendation.

1. **Tenant config format.** One env file per tenant plus a `current` pointer,
   or a `tenants.toml`. *Recommend env files:* no new format, no migration, and
   `--env-file` already works on today's CLI.
2. **Credential storage.** 0600 files, or an OS keyring. *Recommend 0600
   files:* consistent with every other muster credential, and works headless.
3. **Showing the active tenant.** A stderr label when the tenant was chosen
   implicitly and more than one exists, or on every command, or requiring
   `--tenant` for writes. *Recommend the implicit-only label;* forcing `--tenant`
   on writes breaks every existing script.
4. **Mirror routing.** One tmux server per tenant, or session-name filters.
   *Recommend tmux servers;* allow a filter only for a read-only tenant, if ever.
5. **MCP with several tenants.** One registration per tenant relying on the
   tmux-server refusal, or a local router on one port. *Recommend per-tenant
   registrations,* and build the router only if Claude Code shows refused
   servers noisily (UNVERIFIED).
6. **Phase order.** CLI, mirror, identity, Android, or Android second.
   *Recommend CLI first either way;* move Android ahead of the mirror if the
   phone is the main need.
7. **Server instance identity.** Add `instance` to `/health`, or pin origins
   only. *Recommend adding it:* it is small, and it is the only way a client can
   tell two deployments apart behind one origin or show a server's own name.
8. **Android WebView without multi-profile.** One signed-in tenant at a time, or
   copying cookies on switch. *Recommend one at a time:* the shell never reads
   cookies in native code today, and it should not start.
9. **Lock-screen privacy per tenant.** Label only, or a per-tenant "hide
   content" switch. *Recommend label only now,* the switch later if a tenant's
   content is more sensitive than the operator's own.
10. **The mirror's redirect-following.** Fix it now as its own change, or with
    phase 2. *Recommend now:* it is a credential leak path with one server too.
11. **Interactive on several tenants.** Allow `-interactive` on more than one
    tenant per host. *Recommend allowing it,* since each tenant's terminals are
    confined to its own tmux server.

## Sources

- muster at `3907f23a`: the files cited inline.
- [muster-android at `7af3250`][ma]: the files cited inline.
- coder/websocket v1.8.14, `dial.go`, its redirect handling.
- Go 1.26 `net/http`, `shouldCopyHeaderOnRedirect` and `isDomainOrSubdomain`.
- [androidx.webkit release notes](https://developer.android.com/jetpack/androidx/releases/webkit)
  and the `WebViewCompat.setProfile` reference (accessed for this design).
- [UnifiedPush Android connector](https://unifiedpush.org/kdoc/connector/)
  (accessed for this design), on `instance`.

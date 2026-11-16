# A second muster server on one machine

Only when the person asks for it: this machine's sessions should also appear in
**another** muster deployment (its own URL, its own account). Skip this
otherwise. The design is [design/multi-tenant.md](design/multi-tenant.md),
phase 2.

This page assumes the machine was set up with [INSTALL.md](../INSTALL.md) (or
`install.sh`, [quickstart-local.md](quickstart-local.md)), so today's
`muster-mirror.service` runs. **Every block below starts by loading
INSTALL.md's helpers** (`~/.config/muster-install/lib.sh`), which hold this
machine's OS, host name and binary directory: run the block of INSTALL.md's
"Preflight (every machine)" first if this machine was set up with `install.sh` alone (it changes nothing
else). A block that cannot load them stops instead of guessing (without them,
`$OS` is empty and a Linux machine would take the macOS branch).

How it works: one `muster-mirror` per muster server, and **one tmux server per
muster server**. The service INSTALL.md set up stays exactly as it is and is
tenant `default`: it mirrors the default tmux server. Each other server is a
tenant with a short name (`[a-z0-9-]`, at most 32 characters, never
`default`), run by the template unit `muster-mirror@<tenant>`, and it mirrors
the tmux server `tmux -L <tenant>`. A session belongs to the server whose tmux
server it runs in: starting work for tenant `work` means opening a terminal
with `tmux -L work`.

🔴 **The tmux server decides where a session goes. It is not a security
boundary.** Every tenant's token, env file and MCP secret belong to this one
Unix user, so anything running as that user, an agent in any tenant's session
or an interactive terminal included, can read them all and type into every tmux
server. So give a tenant `MUSTER_MIRROR_INTERACTIVE=1` only if it is trusted as
much as this machine itself; an interactive terminal is a shell here. Where a
tenant is not trusted that far, the real isolation is one Unix user per tenant.

## 1. Upgrade and restart today's service first

Its hook record must name its tmux server before any tenant starts (a daemon
that is not tenant `default`'s own, meaning it has
`MUSTER_MIRROR_TMUX_SOCKET`/`MUSTER_MIRROR_TMUX_SOCKET_PATH` or a
`MUSTER_MIRROR_HOOK_CONFIG` naming `~/.muster/mirror-hook.json`, refuses to
overwrite that record when it names another tmux server, and the refusal needs
the name there). Today's service itself, on the default tmux server with no
`MUSTER_MIRROR_HOOK_CONFIG`, always rewrites its own record, so editing its
`TMUX_TMPDIR` never locks it out. A single service that later moves ONTO
`MUSTER_MIRROR_TMUX_SOCKET` is refused like a tenant: delete
`~/.muster/mirror-hook.json` once and restart it. Install the new binary
(INSTALL.md, "Install muster on this machine"), then:

```sh
. "$HOME/.config/muster-install/lib.sh" || exit 1
[ -n "${OS:-}" ] || { echo "NO HELPERS STATE: run the block of INSTALL.md's Preflight first"; exit 1; }
if [ "$OS" = linux ]; then systemctl --user restart muster-mirror.service
else launchctl kickstart -k "gui/$(id -u)/io.github.zacxdev.muster-mirror"; fi
sleep 3
grep -q '"tmuxSocket"' "$HOME/.muster/mirror-hook.json" && echo "legacy record names its tmux server" ||
  echo "NO tmuxSocket in ~/.muster/mirror-hook.json: the service is not upgraded (or runs without MUSTER_MIRROR_WRITE_HOOK_CONFIG=1); stop here"
```

## 2. Name the tenant and mint its token

Mint it on the other server, for this machine's host name (`echo
"$MIRROR_HOST"` after loading the helpers), as INSTALL.md's "Another machine"
does, but against that server's hub. Write it to
`~/.config/muster-mirror/<tenant>/token` without echoing it. The name is saved,
so the later blocks use it:

```sh
. "$HOME/.config/muster-install/lib.sh" || exit 1
t=work            # the person's choice
case "$t" in
  default) echo "REFUSED: \"default\" is today's muster-mirror.service; pick another name"; exit 1 ;;
  *[!a-z0-9-]*|'') echo "REFUSED: a tenant name is a-z, 0-9 and -"; exit 1 ;;
esac
[ "${#t}" -le 32 ] || { echo "REFUSED: at most 32 characters"; exit 1; }
mi_save TENANT "$t"
tcfg="$HOME/.config/muster-mirror/$TENANT"
mkdir -p "$tcfg" && chmod 700 "$tcfg"
[ -s "$tcfg/token" ] || echo "write the other server's mirror token to $tcfg/token first"
```

## 3. Write a FRESH env file for it, never a copy of today's

A copy would carry today's `MUSTER_MIRROR_TOKEN_FILE` (tenant `default`'s
credential) and today's `MUSTER_MIRROR_INTERACTIVE`, and the unit cannot
override either. The token must come from a file (`MUSTER_MIRROR_TOKEN_FILE`):
a tenant given its token in `MUSTER_MIRROR_TOKEN` writes no hook record. The
MCP port is explicit and must differ from every other tenant's (and from
today's service's; `muster-mirror -h` names its default); a port in use is not
an error, the daemon then only logs that its MCP endpoint is off. The env file
must **not** set `MUSTER_MIRROR_HOOK_CONFIG` or
`MUSTER_MIRROR_OPENCODE_READ_ONLY`: the unit sets them, and an env file
overrides the unit.

```sh
. "$HOME/.config/muster-install/lib.sh" || exit 1
[ -n "${TENANT:-}" ] || { echo "NO TENANT: run block 2 first"; exit 1; }
OTHER_URL=https://team.example.org   # the other server
MCP_PORT=CHOOSE-A-FREE-PORT          # free, and not another tenant's
TENANT_INTERACTIVE=0                 # 1 only for a tenant trusted as much as this machine
case "$MCP_PORT" in *[!0-9]*|'') echo "REFUSED: MCP_PORT must be a port number"; exit 1 ;; esac
tcfg="$HOME/.config/muster-mirror/$TENANT"
chmod 600 "$tcfg/token"
{
  echo "MUSTER_MIRROR_URL=$OTHER_URL"
  echo "MUSTER_MIRROR_TOKEN_FILE=$tcfg/token"
  echo "MUSTER_MIRROR_HOST=$MIRROR_HOST"
  echo "MUSTER_MIRROR_TMUX_SOCKET=$TENANT"
  echo "MUSTER_MIRROR_MCP_LISTEN=127.0.0.1:$MCP_PORT"
  echo "MUSTER_MIRROR_MCP_SECRET_FILE=$tcfg/mcp-secret"
  echo "MUSTER_MIRROR_INTERACTIVE=$TENANT_INTERACTIVE"
  echo "PATH=$(dirname "$(command -v tmux)"):$MUSTER_BIN:/usr/local/bin:/usr/bin:/bin"
} > "$tcfg/env"
chmod 600 "$tcfg/env"
```

If the person sets `TMUX_TMPDIR`, add the same line here too.

## 4. Install the template unit once, and start the tenant

On Linux:

```sh
. "$HOME/.config/muster-install/lib.sh" || exit 1
[ "${OS:-}" = linux ] || { echo "per-tenant daemons are Linux only (see below)"; exit 1; }
[ -n "${TENANT:-}" ] || { echo "NO TENANT: run block 2 first"; exit 1; }
mkdir -p "$HOME/.config/systemd/user"
cat > "$HOME/.config/systemd/user/muster-mirror@.service" <<UNIT
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
ExecStart=$MUSTER_BIN/muster-mirror
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
UNIT
systemctl --user daemon-reload
systemctl --user enable --now "muster-mirror@$TENANT.service"
sleep 3
systemctl --user is-active "muster-mirror@$TENANT.service"
journalctl --user -u "muster-mirror@$TENANT.service" -o cat -n 50 --no-pager | grep -E '^muster-mirror: |recorded|could not record' | tail -n 5
```

`ExecCondition=` needs systemd 243 or later; an older systemd ignores it, so
the name check in block 2 is the guard there. If `is-active` says
`activating`, the daemon exits as it starts and is being restarted: the
`muster-mirror: …` line above is its reason.

On macOS, per-tenant daemons have not been measured. Tell the person this and
stop here on a Mac; the single-tenant service is unchanged.

The log must show `recorded …/mirror-hook.d/<tenant>.json for muster-mirror
ask: tmux server …/tmux-<uid>/<tenant>`. A line saying `could not record`
gives the reason, and the hook then keeps that tenant's questions in the
terminal.

## 5. MCP (optional)

Register the tenant's endpoint as its own Claude Code MCP server beside the
default one, named `muster-<tenant>`, with its own port and its own secret
variable (see [attention.md](attention.md#client-configuration)):

```json
"muster-work": {
  "type": "http",
  "url": "http://127.0.0.1:<MCP_PORT>/mcp",
  "headers": {
    "Authorization": "Bearer ${MUSTER_MIRROR_MCP_SECRET_WORK:-}",
    "X-Muster-Pane": "${TMUX_PANE:-}",
    "X-Muster-Tmux": "${TMUX:-}"
  }
}
```

A session gets tools only from the endpoint of its own tmux server; every other
tenant's endpoint answers it as a connected server with no tools ("⚠ no tools"
in `/mcp`), never as an error.

What else changes: only tenant `default`'s service renews the opencode token;
each tenant reads it, and a tenant started before the host was paired picks
the token file up once `muster-mirror opencode-pair` writes it. The hook sends a
tenant's server only the token file its record names, never a
`MUSTER_MIRROR_TOKEN` or `MUSTER_MIRROR_TOKEN_FILE` from the hook's
environment. `AskUserQuestion` cards go to the server of the session's
tmux server; a session in a tmux server no daemon mirrors, or in one that two
daemons mirror, keeps its question in the terminal.

## Removing a tenant

`systemctl --user disable --now muster-mirror@<tenant>`, then delete
`~/.muster/mirror-hook.d/<tenant>.json` and `~/.config/muster-mirror/<tenant>/`.
Today's service never changed, so nothing else needs undoing.

# Installing muster

> **This file is written for an AI coding agent to follow** (Claude Code, Codex,
> opencode, Gemini CLI or any other agent that can run shell commands). A person
> starts it by pasting this one prompt into their agent:

```text
Set up muster for me. Download its install guide with `curl -fsSL -o muster-install.md https://github.com/ZacxDev/muster/releases/latest/download/INSTALL.md`, read that whole file and follow it step by step. If that download fails, stop and tell me what it printed; never work from another copy of the guide, such as the one on the main branch, or from memory. Ask me everything you need in one go up front, then do the rest yourself. Where the guide and the machine disagree, trust `muster doctor`, each command's `--help` and the code at https://github.com/ZacxDev/muster over memory.
```

Each release attaches its own copy of this file, so the guide an agent
downloads that way installs exactly the release it came with.

When it is done, the person has:

- the muster server running on one machine (the **hub**), in Docker, at a
  `http://localhost:…` address this machine's browser opens, with nothing
  reachable from any other machine;
- this machine's tmux sessions in muster: every Claude Code, Codex, opencode
  or Gemini CLI session running in tmux appears as a card they can read and
  answer;
- a passing `muster doctor`;
- **if they want it** (Step 3): muster on their phone and their other machines'
  sessions in it, over HTTPS on their own private Tailscale network and
  nowhere else;
- **if they ask for them** (Step 6): optional features, such as dictation or
  file attachments.

## How to follow this guide (agent: read this first)

- **Run the commands yourself**, in order, in a POSIX shell (`sh`, `bash` or
  `zsh`). Every step is safe to re-run: it checks what already exists and keeps
  it. If you stop half way, start again from Step 0.
- **Each shell block starts by loading the helpers** written in Step 0
  (`. "$HOME/.config/muster-install/lib.sh"`). They hold the values earlier
  steps found, so it does not matter whether your shell keeps variables between
  commands.
- **This guide holds no facts that live elsewhere.** Ports, versions, image
  tags, model names and settings are owned by the code. When you need one, ask
  the source: `muster doctor`, a command's `--help`,
  [docs/configuration.md](docs/configuration.md) for every setting,
  [docs/features.md](docs/features.md) for every optional feature, or the
  code itself. If this guide and one of them disagree, they win.
- **Ask the person the Step 1 questions all at once.** After that, do not stop
  to ask unless a step says "ask".
- **Never print a secret.** Passwords, tokens and keys go straight into files
  with mode `600`. Do not echo them, do not paste them into your reply, and do
  not put them on a command line other than as shown.
- **Ask for `sudo` once, up front.** Step 0 prints a line starting `ASK UP FRONT`
  for everything that needs `sudo`, a package install, or the person's own
  hands (a Tailscale sign-in link, a setting in the Tailscale admin console).
  Step 1 asks about all of them in the same message as its other questions.
  Use `sudo` only for what the person approved there.
- Every step ends with **If this fails**. Follow it before trying anything
  else. If you are still stuck, stop and tell the person exactly which step and
  command failed and what it printed.
- Nothing here opens a port to the internet. The server listens on this
  machine's loopback interface; Step 3 adds Tailscale, a private network
  between the person's own devices.

---

## Step 0: Preflight (every machine)

Write the helpers, then detect the machine. This creates
`~/.config/muster-install/`; it changes nothing else.

```sh
mkdir -p "$HOME/.config/muster-install" && chmod 700 "$HOME/.config/muster-install"
cat > "$HOME/.config/muster-install/lib.sh" <<'LIB'
# Helpers for INSTALL.md. Safe to source any number of times.
MI_DIR="$HOME/.config/muster-install"
[ -f "$MI_DIR/state" ] && . "$MI_DIR/state"
# The release this copy of the guide came with: scripts/release-build.sh
# stamps it into each release's INSTALL.md. Unstamped (a copy read from the
# repository), the guide installs the newest release.
MI_GUIDE_RELEASE="@MUSTER_RELEASE@"
MUSTER_DIR="${MUSTER_DIR:-$HOME/muster}"
MUSTER_BIN="${MUSTER_BIN:-$HOME/.local/bin}"
MI_MIRROR_ENV="$HOME/.config/muster-mirror/env"
# install.sh's first line in muster-mirror's settings: the file is install.sh's.
MI_MARK="# written by muster's install.sh; edit freely, lines it does not own are kept"
# A container's shell often has no USER; later steps name it.
USER="${USER:-$(id -un)}"; export USER
# macOS: the App Store Tailscale puts its CLI inside the app, not on PATH.
# (MI_TS_APP replaces that path; for testing only.)
if ! command -v tailscale >/dev/null 2>&1 && [ -x "${MI_TS_APP:-/Applications/Tailscale.app/Contents/MacOS/Tailscale}" ]; then
  tailscale() { "${MI_TS_APP:-/Applications/Tailscale.app/Contents/MacOS/Tailscale}" "$@"; }
fi
# mi_save KEY VALUE: remember a value for later steps (and later shells).
# The state file and its temporary copy are written under umask 077 (mode 600).
mi_save() {
  (umask 077
   touch "$MI_DIR/state"
   grep -v "^$1=" "$MI_DIR/state" > "$MI_DIR/state.tmp" || true
   printf "%s='%s'\n" "$1" "$2" >> "$MI_DIR/state.tmp" && mv "$MI_DIR/state.tmp" "$MI_DIR/state") || return 1
  eval "$1='$2'"
}
# mi_rand [BYTES]: a random hex string (2 characters per byte).
mi_rand() { head -c "${1:-24}" /dev/urandom | od -An -tx1 | tr -d ' \n'; }
# mi_valid_version V: V is a release version, X.Y.Z and nothing else.
mi_valid_version() { printf '%s\n' "$1" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; }
# mi_set_version V: remember V and the release URL derived from it. Refuses
# anything that is not X.Y.Z, so a failed lookup is never saved.
# (MUSTER_RELEASE_OVERRIDE replaces the GitHub URL; for testing only.)
mi_set_version() {
  mi_valid_version "$1" || { echo "mi_set_version: '$1' is not a version (X.Y.Z); nothing saved" >&2; return 1; }
  mi_save MUSTER_VERSION "$1"
  mi_save MUSTER_RELEASE_BASE "${MUSTER_RELEASE_OVERRIDE:-https://github.com/ZacxDev/muster/releases/download/v$1}"
}
# mi_release: choose the release to install: this guide's own when a release
# stamped it, else the newest (GitHub's /releases/latest redirect, then its
# API). Saved only if it is X.Y.Z.
mi_release() {
  if mi_valid_version "$MI_GUIDE_RELEASE"; then mi_set_version "$MI_GUIDE_RELEASE"; return; fi
  v=$(curl -fsSLI -o /dev/null -w '%{url_effective}' https://github.com/ZacxDev/muster/releases/latest | sed -n 's#.*/tag/v\([^/]*\)$#\1#p')
  mi_valid_version "$v" || v=$(curl -fsSL https://api.github.com/repos/ZacxDev/muster/releases/latest | sed -n 's/.*"tag_name": *"v\{0,1\}\([^"]*\)".*/\1/p' | head -n 1)
  mi_set_version "$v"
}
# mi_verify FILE: check FILE (in the current directory) against ./checksums.txt.
mi_verify() {
  line=$(grep "[ *]$1\$" checksums.txt) || { echo "mi_verify: $1 is not listed in checksums.txt" >&2; return 1; }
  if command -v sha256sum >/dev/null 2>&1; then printf '%s\n' "$line" | sha256sum -c -
  else printf '%s\n' "$line" | shasum -a 256 -c -; fi
}
# mi_override: the person's Compose override beside docker-compose.yml, the
# file Compose itself would read (the first of its four names, in its order);
# empty when there is none.
mi_override() {
  for f in compose.override.yml compose.override.yaml docker-compose.override.yml docker-compose.override.yaml; do
    [ -e "$MUSTER_DIR/$f" ] && { echo "$MUSTER_DIR/$f"; return 0; }
  done
  return 0
}
# mi_dc ARGS: docker compose on the hub's stack, with that override when there
# is one (an explicit -f turns Compose's automatic override off; install.sh
# includes it the same way).
mi_dc() {
  mi_ov=$(mi_override)
  if [ -n "$mi_ov" ]; then
    docker compose -f "$MUSTER_DIR/docker-compose.yml" -f "$mi_ov" -p "${MUSTER_PROJECT:-muster}" "$@"
  else
    docker compose -f "$MUSTER_DIR/docker-compose.yml" -p "${MUSTER_PROJECT:-muster}" "$@"
  fi
}
# mi_logs ARGS: the stack's logs (`mi_logs --tail=80 muster-init muster`)
# WITHOUT the line muster-init prints the generated sign-in password on (it
# carries this marker): read logs only this way, so the password never lands
# in the agent's output.
MI_SECRET_MARK='[muster-secret]'
mi_logs() { mi_dc logs --no-color "$@" 2>&1 | grep -vF "$MI_SECRET_MARK"; }
# mi_local_url / mi_local_port: the hub's own http://localhost:<port> address,
# from the settings install.sh wrote for this machine's muster-mirror (empty
# before Step 2, and on a joining machine).
mi_local_url() { sed -n 's#^MUSTER_MIRROR_URL=\(http://localhost:[0-9]*\)/*$#\1#p' "$MI_MIRROR_ENV" 2>/dev/null | tail -n 1; }
mi_local_port() { mi_local_url | sed -n 's#.*:\([0-9]*\)$#\1#p'; }
# env_get KEY / env_set KEY VALUE: read and write the hub's .env, beside its
# docker-compose.yml (created, mode 600, when missing). A KEY line is any
# spelling Compose reads: indented, after `export`, with spaces around `=`;
# Compose takes the LAST one, and so does env_get. env_set replaces the first
# and drops the rest, so no line it leaves can shadow its value. It passes the
# value through the environment, never argv, and writes with umask 077.
env_get() {
  [ -f "$MUSTER_DIR/.env" ] &&
    sed -n "s/^[[:space:]]*\(export[[:space:]]\{1,\}\)\{0,1\}$1[[:space:]]*=[[:space:]]*//p" "$MUSTER_DIR/.env" |
    sed 's/[[:space:]]*$//' | tail -n 1
}
env_set() {
  (umask 077
   [ -f "$MUSTER_DIR/.env" ] || : > "$MUSTER_DIR/.env"
   MI_K="$1" MI_V="$2" awk 'BEGIN{k=ENVIRON["MI_K"]; v=ENVIRON["MI_V"]; done=0; re="^[ \t]*(export[ \t]+)?" k "[ \t]*="}
     $0 ~ re {if(!done){print k"="v; done=1}; next} {print} END{if(!done) print k"="v}' \
     "$MUSTER_DIR/.env" > "$MUSTER_DIR/.env.tmp" && cat "$MUSTER_DIR/.env.tmp" > "$MUSTER_DIR/.env")
  rm -f "$MUSTER_DIR/.env.tmp"
  chmod 600 "$MUSTER_DIR/.env"
}
# env_secret KEY [BYTES]: generate KEY unless it already holds a value.
env_secret() { [ -n "$(env_get "$1")" ] || env_set "$1" "$(mi_rand "${2:-24}")"; }
# mi_profiles ADD [DROP]: .env's COMPOSE_PROFILES with ADD in it once (none
# when empty) and every entry starting with DROP taken out; every other
# profile is kept, so turning one feature on never drops another's.
mi_profiles() {
  out=""
  for p in $(env_get COMPOSE_PROFILES | tr ',' ' '); do
    [ "$p" = "$1" ] && continue
    [ -n "${2:-}" ] && case "$p" in "$2"*) continue ;; esac
    out="${out:+$out,}$p"
  done
  [ -n "$1" ] && out="${out:+$out,}$1"
  printf '%s\n' "$out"
}
# mi_foreign [PROJECT]: print every sign of a muster this guide did NOT make —
# containers of the compose project PROJECT (default muster) running from
# another directory, or (before this guide started one) any of its volumes.
# Returns 1 when it found any.
mi_foreign() {
  p="${1:-muster}"; found=0
  if command -v docker >/dev/null 2>&1; then
    for c in $(docker ps -a -q --filter "label=com.docker.compose.project=$p" 2>/dev/null); do
      d=$(docker inspect -f '{{index .Config.Labels "com.docker.compose.project.working_dir"}}' "$c" 2>/dev/null)
      if [ "${GUIDE_STARTED:-}" != 1 ] || [ "$d" != "$MUSTER_DIR" ]; then
        echo "FOREIGN container $(docker inspect -f '{{.Name}}' "$c") (compose project $p, from ${d:-unknown})"; found=1
      fi
    done
    if [ "${GUIDE_STARTED:-}" != 1 ]; then
      for v in $(docker volume ls -q --filter "label=com.docker.compose.project=$p" 2>/dev/null); do
        echo "FOREIGN volume $v (compose project $p)"; found=1
      done
    fi
  fi
  [ "$found" = 0 ]
}
# mi_serve_state: what `tailscale serve` already does on this machine —
# "none", "ours" (exactly https / -> this hub's local port), "other", or
# "unready" (Tailscale is not installed, not running, or signed out: `serve
# status` can answer "No serve config" while signed out, so that is asked
# first).
mi_serve_state() {
  tailscale status >/dev/null 2>&1 || { echo unready; return; }
  st=$(tailscale serve status 2>&1) || { echo unready; return; }
  if printf '%s\n' "$st" | grep -qi 'no serve config'; then echo none; return; fi
  if printf '%s\n' "$st" | grep -qi 'funnel'; then echo other; return; fi
  urls=$(printf '%s\n' "$st" | grep -Eo 'https?://[^ ]+' | grep -v '127.0.0.1')
  maps=$(printf '%s\n' "$st" | grep -E '^\|--')
  port=$(mi_local_port)
  if [ -n "$port" ] && [ "$(printf '%s\n' "$urls" | grep -c .)" = 1 ] && ! printf '%s\n' "$urls" | grep -q ':[0-9]*/*$' &&
     [ "$maps" = "|-- / proxy http://127.0.0.1:$port" ]; then echo ours; else echo other; fi
}
LIB
. "$HOME/.config/muster-install/lib.sh"

case "$(uname -s)" in Linux) mi_save OS linux ;; Darwin) mi_save OS darwin ;; *) echo "UNSUPPORTED OS: $(uname -s)" ;; esac
case "$(uname -m)" in x86_64|amd64) mi_save ARCH amd64 ;; aarch64|arm64) mi_save ARCH arm64 ;; *) echo "UNSUPPORTED ARCH: $(uname -m)" ;; esac
mi_save HOSTNAME_SHORT "$(hostname -s 2>/dev/null || hostname | cut -d. -f1)"
mi_save MIRROR_HOST "$(printf '%s' "$HOSTNAME_SHORT" | tr 'A-Z' 'a-z')"
distro=$(. /etc/os-release 2>/dev/null && echo "${ID:-}")
echo "os=$OS arch=$ARCH host=$MIRROR_HOST${distro:+ distro=$distro}"
for t in curl tar tmux tailscale docker qrencode; do
  printf '%-11s %s\n' "$t" "$(command -v "$t" || echo MISSING)"
done
docker compose version 2>/dev/null || echo "docker compose: MISSING"
docker info >/dev/null 2>&1 && echo "docker: usable" || echo "docker: NOT usable by $USER (not running, or $USER is not in the docker group)"
tailscale status >/dev/null 2>&1 && echo "tailscale: logged in" || echo "tailscale: NOT logged in (or not running)"
df -Pk "$HOME" | awk 'NR==2 {printf "free disk in $HOME: %.1f GB\n", $4/1048576}'
# What is already here.
if [ -e "$MUSTER_DIR/docker-compose.yml" ] && [ -e "$MUSTER_DIR/.muster-install" ]; then echo "existing muster: install.sh's, in $MUSTER_DIR (Step 2 updates it)"
elif [ -e "$MUSTER_DIR/docker-compose.yml" ]; then echo "existing muster: NOT install.sh's, in $MUSTER_DIR (an earlier version of this guide, or the person's own)"
fi
mi_foreign && echo "foreign muster: none"
if [ ! -e "$MI_MIRROR_ENV" ]; then echo "muster-mirror settings: none"
elif grep -qxF "$MI_MARK" "$MI_MIRROR_ENV"; then echo "muster-mirror settings: install.sh's (kept and updated)"
else echo "muster-mirror settings: NOT install.sh's ($MI_MIRROR_ENV: an earlier version of this guide, or the person's own)"; fi
echo "tailscale serve: $(mi_serve_state)"
# Everything below needs sudo or the person's own hands. Step 1 asks about
# all of it in one message, so nothing stops half way for a password or a link.
# On macOS nothing here needs sudo: Homebrew, the Tailscale app and Docker
# Desktop install as the person.
for t in curl tar tmux; do
  command -v "$t" >/dev/null 2>&1 && continue
  if [ "$OS" = darwin ]; then echo "ASK UP FRONT: the person installs $t (\`brew install $t\`, no sudo; required on every machine)"
  else echo "ASK UP FRONT: sudo to install $t (required on every machine)"; fi
done
if ! command -v docker >/dev/null 2>&1 || ! docker compose version >/dev/null 2>&1; then
  if [ "$OS" = darwin ]; then echo "ASK UP FRONT (hub only): the person installs Docker Desktop (no sudo)"
  else echo "ASK UP FRONT (hub only): sudo to install Docker Engine with the Compose v2 plugin (the lines are under \"Docker on Linux\" below the output)"; fi
fi
if [ "$OS" = linux ] && command -v docker >/dev/null 2>&1 && ! docker info >/dev/null 2>&1; then
  echo "ASK UP FRONT (hub only, Linux): sudo to start Docker (\`sudo systemctl enable --now docker\`) and to add $USER to the docker group (\`sudo usermod -aG docker \"\$USER\"\`); the group applies only to a NEW login, so the person logs out and in again (or you run docker as \`sg docker -c '…'\` until then)"
fi
if ! command -v tailscale >/dev/null 2>&1; then
  if [ "$OS" = darwin ]; then echo "ASK UP FRONT (for the phone and other machines): the person installs the Tailscale app (App Store or https://tailscale.com/download, no sudo) and signs in with it"
  else echo "ASK UP FRONT (for the phone and other machines): sudo to install Tailscale (https://tailscale.com/download), then to run \`tailscale up\`, which prints a sign-in link the person must open"; fi
elif ! tailscale status >/dev/null 2>&1; then
  if [ "$OS" = darwin ]; then echo "ASK UP FRONT (for the phone and other machines): the person signs in with the Tailscale app (no sudo)"
  else echo "ASK UP FRONT (for the phone and other machines): sudo to run \`tailscale up\`, which prints a sign-in link the person must open"; fi
fi
# `status --json` is indented over many lines: compare it without whitespace.
ts=$(tailscale status --json 2>/dev/null | tr -d '[:space:]')
if ! printf '%s\n' "$ts" | grep -q '"MagicDNSEnabled":true' || ! printf '%s\n' "$ts" | grep -q '"CertDomains":\["'; then
  echo "ASK UP FRONT (hub, for the phone and other machines): the person turns on MagicDNS and HTTPS Certificates in the Tailscale admin console (https://login.tailscale.com/admin/dns)"
fi
# The operator, only when the prefs show it is not this user (unset, someone
# else, or left out of the JSON). If the prefs cannot be read, nothing is
# asked: Step 3's "access denied" line covers it.
if [ "$OS" = linux ] && [ "$(id -u)" != 0 ]; then
  op=$(tailscale debug prefs 2>/dev/null | tr -d '[:space:]')
  case "$op" in
    *"\"OperatorUser\":\"$USER\""*) ;;
    *'"OperatorUser":'*|'{'*) echo "ASK UP FRONT (hub, Linux, for the phone): sudo to run \`tailscale set --operator=\"\$USER\"\`, so Step 3 can run \`tailscale serve\` without sudo" ;;
  esac
fi
if ! command -v qrencode >/dev/null 2>&1; then
  if [ "$OS" = darwin ]; then echo "ASK UP FRONT (optional): the person installs qrencode (\`brew install qrencode\`, no sudo), so \`muster doctor\` can draw the phone's QR code"
  else echo "ASK UP FRONT (optional): sudo to install qrencode, so \`muster doctor\` can draw the phone's QR code"; fi
fi
if [ "$OS" = linux ] && ! command -v loginctl >/dev/null 2>&1; then
  echo "NOTE: no loginctl (WSL, or a container without systemd): muster-mirror cannot linger after logout. On WSL, turn systemd on (\`[boot] systemd=true\` in /etc/wsl.conf, then \`wsl --shutdown\`) and re-run Step 0; elsewhere, muster-mirror runs while the person is logged in"
elif [ "$OS" = linux ] && [ "$(loginctl show-user "$USER" -p Linger --value 2>/dev/null)" != yes ]; then
  echo "ASK UP FRONT (Linux): sudo to run \`loginctl enable-linger \"\$USER\"\`, so muster-mirror keeps running after the person logs out (Step 2)"
fi
unset ts op distro
```

Read the output and note:

| what | needed on | if it is missing |
|---|---|---|
| `os`, `arch` | every machine | Linux and macOS on amd64 or arm64 only. Stop on anything else and tell the person. On Windows (Git Bash, MSYS2, Cygwin) only the `muster` CLI runs, not `muster-mirror` (it needs tmux): tell the person, and offer WSL, which is Linux and follows this guide as one ([docs/releases.md](docs/releases.md)). macOS hosts are experimental: see [docs/mirroring.md](docs/mirroring.md) for what a Mac can show, and tell the person. |
| `curl`, `tar` | every machine | Install them with the system package manager (ask first). |
| `tmux` | every machine | Install it (`sudo apt install tmux`, `sudo dnf install tmux`, `brew install tmux`; ask first). muster shows tmux windows, so this is required. Any packaged version works. |
| `docker`, `docker compose`, `docker: usable` | the hub only | See **Docker on Linux** below, or Docker Desktop on macOS. |
| free disk | the hub | At least 10 GB, more for a local dictation model ([docs/features.md](docs/features.md#dictation)). |
| `tailscale`, logged in | the phone and other machines only (Step 3) | Install from <https://tailscale.com/download>, then `tailscale up` (on Linux, `sudo tailscale up`) prints a sign-in URL: give it to the person to open, and wait. Every machine and the phone must sign in to the **same** tailnet. |
| `qrencode` | optional | Lets `muster doctor` draw the phone's QR code. |

**Docker on Linux** (the hub only; ask first, it needs `sudo`). Docker's
own script installs Docker Engine with the Compose v2 plugin on Ubuntu, Debian
and Fedora:

```text
curl -fsSL https://get.docker.com | sudo sh
```

Or by hand. Ubuntu, from its own packages:

```text
sudo apt-get update && sudo apt-get install -y docker.io docker-compose-v2
```

Fedora, from Docker's package repository
(<https://docs.docker.com/engine/install/fedora/>):

```text
sudo dnf -y install dnf-plugins-core
sudo dnf config-manager addrepo --from-repofile https://download.docker.com/linux/fedora/docker-ce.repo
sudo dnf install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
```

Debian: Docker's own script, or <https://docs.docker.com/engine/install/debian/>
(its own packaged Compose is not v2 on every release). Then, whichever way:

```text
sudo systemctl enable --now docker
sudo usermod -aG docker "$USER"
```

🔴 **The docker group applies only to a new login.** The shell you run in now
still cannot use Docker: ask the person to log out and in again (a new ssh
session, or a new desktop login) and start this guide again from Step 0. Until
then a single command can run as `sg docker -c '…'`. Membership of the docker
group is root on this machine, so it is the person's decision. Check with
`docker compose version` and `docker info`.

🔴 **If `foreign muster:` printed a `FOREIGN` line, STOP and ask the person.**
This machine already runs a muster (or another stack under its compose project)
that this guide did not set up. Do not take it over, do not stop it, do not
reuse its project or data. Show them the lines. Go on only if they choose to
use this machine as a joining machine (Step 1, question 1: yes), or after they
have dealt with it themselves.

If `existing muster:` says **NOT install.sh's**, it is an install made by an
earlier version of this guide (or by hand): do not run Step 2 on it. Update it
as "An install from an earlier version of this guide" (at the end) shows.

If `muster-mirror settings:` says **NOT install.sh's**, Step 1 asks whether
they may be taken over (question 3).

If `tailscale serve:` is `unready`, Tailscale is not up yet (see the
`tailscale` row above); that is not a conflict. 🔴 **If it is `other`**, this
machine already serves something through Tailscale (another site, another
port, or Funnel). Step 3 will not change it; ask the person before the hub's
HTTPS goes on this machine.

**If this fails:** the helper file is plain text; check
`~/.config/muster-install/lib.sh` exists and re-run this block.

---

## Step 1: Ask the person, once

Ask all of these in **one** message. Give your recommendation for each, based
on what Step 0 found. Record each answer with `mi_save` as shown.

1. **Is muster already running on another of your machines?**
   - **No** (the usual case): this machine becomes the hub.
     `mi_save ROLE hub`
   - **Yes**: this machine joins it. Ask for the hub's `https://…` URL
     (`mi_save MUSTER_URL https://…`) and `mi_save ROLE join`. This machine
     needs Tailscale (question 4 covers it) and a mirror token minted on the
     hub: "Another machine" in Step 3 gets it, then Step 2 connects this
     machine.
2. *(hub only)* **Do you want muster on your phone, or other machines' sessions
   in it?** Yes adds Tailscale (Step 3): HTTPS on the person's own private
   network, which the phone needs for the app, notifications and the
   microphone. No keeps muster on this machine's browser only; Step 3 can be
   done any time later. `mi_save TAILSCALE yes` or `mi_save TAILSCALE no`.
3. **May muster type into this machine's terminals?** (asked per machine.)
   Explain: **yes** lets you send messages to your agents from muster, which is
   the main point of it. It also means anyone signed in to your muster can type
   anything into any tmux window on this machine, which is a shell as your
   user, after a recent sign-in or password confirmation
   ([docs/mirroring.md](docs/mirroring.md)). **No** keeps the machine
   read-only: you can watch every session but not answer. Recommend **yes** on
   machines the person owns. `mi_save INTERACTIVE 1` (yes) or
   `mi_save INTERACTIVE 0` (no).

   If Step 0 printed `muster-mirror settings: NOT install.sh's`, ask too
   whether the install may take those settings over: every line it does not
   manage (such as a `TMUX_TMPDIR` the person added) is kept. Yes:
   `mi_save ADOPT_MIRROR 1`. No: stop on this machine and say why.
4. **What needs `sudo` or the person's own hands.** List every line Step 0
   printed starting `ASK UP FRONT` (leave out the `hub only` ones on a joining
   machine, and the `for the phone and other machines` ones when question 2 is
   no), say what each is for, and ask for `sudo` for all of them at once. Say
   plainly:
   - **Docker** (hub): the install lines are in Step 0's **Docker on Linux**;
     the docker group needs a new login before this guide can go on.
   - **Tailscale missing or signed out:** installing it and `tailscale up`
     need `sudo`, and `tailscale up` prints a **sign-in link the person must
     open** in a browser. Every machine and the phone sign in to the same
     tailnet.
   - **MagicDNS and HTTPS Certificates** (hub, with Tailscale): ask the person
     to turn both on now in the Tailscale admin console,
     <https://login.tailscale.com/admin/dns>. Without them Step 3 cannot give
     muster an HTTPS address.
   - **`qrencode`** (optional) and **`loginctl enable-linger`** (Linux) need
     `sudo` too. Ask in this same message. Record the linger answer:
     `mi_save LINGER yes` or `mi_save LINGER no`.
   - **macOS:** nothing needs `sudo`. Homebrew packages, the Tailscale app
     and Docker Desktop install as the person; ask them to install what is
     missing and to sign in with the Tailscale app.
   - A `NOTE: no loginctl` line (WSL, a container): tell the person
     muster-mirror runs only while they are logged in, and the WSL fix it names.

   If the agent's shell cannot answer a `sudo` password prompt, give the
   person the exact commands to run in their own terminal instead.
5. *(hub only)* **Any optional feature now?** Point them at
   <https://github.com/ZacxDev/muster/blob/main/docs/features.md>, which lists
   every one with what it needs. The usual ones are dictation (a microphone in
   every message box: a bundled CPU or GPU service, or their own
   speech-to-text API key) and file attachments. Note what they choose for
   Step 6; turn nothing on that they did not ask for. A key or token they type
   into this chat passes through the agent's provider: if they prefer, they
   write it to a file themselves in a terminal,
   `read -rs K && (umask 077; printf '%s' "$K" > ~/.config/muster-install/<name>)`.

**Then, before Step 2,** do what the person approved, in this order: install
the missing packages (and, after a docker group change, wait for their new
login and restart from Step 0); with Tailscale, on Linux,
`sudo tailscale up --operator="$USER"` (or `sudo tailscale set --operator="$USER"`
if Tailscale is already signed in), on macOS the person signs in with the
Tailscale app. Give the person the sign-in link and wait until
`tailscale status` succeeds.

**If this fails:** if the person does not answer a question, use the
recommendation and say so. If they decline `sudo` for something required
(`tmux` everywhere, Docker on the hub, Tailscale on a joining machine), stop
and say which step needs it.

---

## Step 2: Install muster on this machine

On the **hub** this runs the same installer as muster's one-line local install,
`install.sh` from the release ([docs/quickstart-local.md](docs/quickstart-local.md)):
it starts the server in Docker in `~/muster`, with every secret generated;
installs `muster-mirror` and the `muster` CLI (checksum-verified) in
`~/.local/bin`; runs `muster-mirror` as a user service so this machine's tmux
windows show up; and ends with `muster doctor`. On a **joining** machine
(`ROLE=join`), it installs only `muster-mirror` and the CLI, of the release
the hub runs, and connects this machine to the hub; it needs the mirror token
from "Another machine" in Step 3 first.

`muster-mirror`'s settings (`~/.config/muster-mirror/env`) are merged, never
overwritten: the installer rewrites only the lines it manages and keeps every
other line. On a fresh hub the installer also writes muster's own address,
`MUSTER_PUBLIC_URL=http://localhost:<port>`, into `~/muster/.env` (attachments
and pages write every link on it; Step 3 replaces it with the HTTPS address)
and says so; an update never writes it.

```sh
. "$HOME/.config/muster-install/lib.sh"
mi_release || { echo "VERSION LOOKUP FAILED (see If this fails)"; exit 1; }
rel="$MI_DIR/release-$MUSTER_VERSION"
mkdir -p "$rel" && cd "$rel" || exit 1
curl -fsSL -o checksums.txt "$MUSTER_RELEASE_BASE/checksums.txt" &&
  curl -fsSL -o install.sh "$MUSTER_RELEASE_BASE/install.sh" &&
  mi_verify install.sh || { echo "DOWNLOAD FAILED (see If this fails)"; exit 1; }
# The installer names what it can do on its "# capabilities:" line (an older
# release's has none): a join or an adopt it cannot do is refused here.
for want in $([ "${ROLE:-hub}" = join ] && echo join) ${ADOPT_MIRROR:+adopt}; do
  grep -Eq "^# capabilities:(.* )?$want( |\$)" install.sh ||
    { echo "OLD INSTALLER: release $MUSTER_VERSION's install.sh cannot $want (see If this fails)"; exit 1; }
done
if [ "${ROLE:-hub}" = join ]; then
  [ -s "$HOME/.config/muster-mirror/token" ] || { echo "NO TOKEN: get this machine's token first (Step 3, Another machine)"; exit 1; }
  export MUSTER_JOIN_URL="$MUSTER_URL"
else
  [ -e "$MUSTER_DIR/.muster-install" ] || [ -e "$MUSTER_DIR/docker-compose.yml" ] || mi_save GUIDE_STARTED 1
fi
# The release's own assets (a test points MUSTER_RELEASE_OVERRIDE elsewhere).
[ -n "${MUSTER_RELEASE_OVERRIDE:-}" ] && export MUSTER_RELEASE_BASE="$MUSTER_RELEASE_OVERRIDE"
rc=0
MUSTER_DIR="$MUSTER_DIR" MUSTER_BIN="$MUSTER_BIN" MUSTER_MIRROR_HOST="$MIRROR_HOST" \
  MUSTER_MIRROR_INTERACTIVE="${INTERACTIVE:-0}" MUSTER_LINGER="${LINGER:-}" \
  MUSTER_MIRROR_ADOPT="${ADOPT_MIRROR:-}" MUSTER_PRINT_PASSWORD=no \
  sh ./install.sh </dev/null || rc=$?
if [ "${ROLE:-hub}" != join ] && [ -n "$(mi_local_url)" ]; then
  # Once it has answered healthy, this install may hold data: "Starting over"
  # refuses to delete it from then on.
  curl -fsS -o /dev/null "$(mi_local_url)/health" && mi_save GUIDE_IN_USE 1
  case "${MUSTER_URL:-}" in https://*) ;; *) mi_save MUSTER_URL "$(mi_local_url)" ;; esac
fi
echo "install.sh exited $rc; muster: ${MUSTER_URL:-unknown}"
# Does a NEW login shell find the CLI? (This shell's PATH may predate
# $MUSTER_BIN existing; many profiles add ~/.local/bin only once it exists.)
# `env -i`: the login shell builds PATH from the profile alone, not from ours.
[ "$(env -i HOME="$HOME" USER="$USER" "${SHELL:-sh}" -lc 'command -v muster' 2>/dev/null)" = "$MUSTER_BIN/muster" ] ||
  echo "NOTE: a new login shell does not find $MUSTER_BIN/muster: add $MUSTER_BIN to PATH in the shell profile"
```

The installer prints one line per doctor check (`PASS`, `FAIL` or `SKIP`, with
a `fix:` line under each failure). It **does not print the sign-in password**
here (`MUSTER_PRINT_PASSWORD=no`); it prints the command that shows it, for the
person to run in their own terminal. An optional feature that is off (dictation,
for one) reports `SKIP`, never `FAIL`.

**If this fails:**
- `VERSION LOOKUP FAILED`: GitHub was unreachable or rate-limited. Nothing was
  saved. Ask the person for the newest version on
  <https://github.com/ZacxDev/muster/releases> (without the `v`), run
  `mi_set_version <it>`, and re-run this block.
- `DOWNLOAD FAILED` or a checksum `FAILED`: re-run once (a partial download).
  A second mismatch: stop and tell the person.
- `OLD INSTALLER`: the release this guide chose predates joining or adopting
  muster-mirror settings. Stop and tell the person; do not install it another
  way. A guide downloaded from a release installs that release, which has both.
- The installer refuses, **changing nothing**, when it finds a muster or
  muster-mirror settings it did not make: its last line says which. Do not
  work around it; see Step 0's notes and ask the person.
- `the stack did not come up`: the lines above it are the reason (a setting
  muster refuses names its variable). `mi_logs --tail=80 muster-init muster`
  shows more.
- `muster-mirror is not running: it exits as it starts`: the line under it is
  muster-mirror's own reason (a refused token, an unreachable URL, a setting it
  rejects). Fix that and re-run this block. `journalctl --user -u
  muster-mirror.service -n 30` (Linux) or `~/Library/Logs/muster-mirror.log`
  (macOS) has the rest.
- A doctor `FAIL`: work through its `fix:` line, then re-run this block (or
  Step 5's check). `muster doctor --help` explains each check.
- `NOTE: a new login shell does not find …`: add
  `export PATH="$HOME/.local/bin:$PATH"` to the person's shell profile (ask
  first), or use the full path. No `NOTE` means a new terminal finds `muster`
  already; change nothing.
- `… does not work with release … and ~/muster/.env`: a
  `docker-compose.override.yml` there needs `.env` lines that are gone
  (accounts mode turned off by deleting them, the override left behind).
  Nothing was changed. Ask the person which they meant: delete the override
  too ([docs/auth.md](docs/auth.md#a-gotrue-server-beside-the-compose-stack),
  "Turn it off"), or put the lines back; then re-run this block.
- `linger is OFF`: the person declined, or `sudo` needs a password the
  agent cannot type: give them `sudo loginctl enable-linger "$USER"` to run.
- It runs but no sessions appear: the person's tmux uses a different socket
  than the service. If they set `TMUX_TMPDIR`, add the same line to
  `~/.config/muster-mirror/env` (it is kept on every re-run) and re-run this
  block.
- One `muster-mirror` log line is expected and is not an error:
  `opencode: not paired on this host …`. It matters only to people who run
  opencode on this machine; they pair it once with `muster-mirror opencode-pair`
  (see `docs/mirroring.md`, "opencode's token").

---

## Step 3: Add your phone and other machines (Tailscale)

Skip this step when the person answered no to Step 1's question 2. It can be
done at any time later, by re-running Step 0 and this step.

### On the hub: HTTPS

`tailscale serve` gives the hub a real HTTPS certificate for its tailnet name
and forwards to muster's local port. Only devices on the tailnet can reach it.
This adds muster's mapping only when nothing is served yet, and changes nothing
else. It then tells muster its address (`MUSTER_PUBLIC_URL` in the hub's
`.env`) and restarts it:

```sh
. "$HOME/.config/muster-install/lib.sh"
port=$(mi_local_port)
[ -n "$port" ] || { echo "NO LOCAL INSTALL: run Step 2 on this machine first"; exit 1; }
case "$(mi_serve_state)" in
  none) tailscale serve --bg --https=443 "$port" ;;
  ours) echo "tailscale serve already forwards https to muster; kept" ;;
  unready) echo "TAILSCALE NOT READY: Tailscale is not running or not signed in here (see If this fails)"
        tailscale status ;;
  *)    echo "STOP: tailscale serve already serves something else here (or Funnel is on):"
        tailscale serve status ;;
esac
if [ "$(mi_serve_state)" = ours ]; then
  mi_save MUSTER_URL "$(tailscale serve status 2>/dev/null | grep -Eo 'https://[^ ]+' | head -n 1 | sed 's:/$::')"
  echo "MUSTER_URL=$MUSTER_URL"
  env_set MUSTER_PUBLIC_URL "$MUSTER_URL"
  mi_dc up -d --wait
  curl -fsS "$MUSTER_URL/health" && echo
fi
```

The last `curl` must print `{"status":"ok",…}`: Tailscale's HTTPS reaches
muster. `MUSTER_URL` is an `https://<machine>.<tailnet>.ts.net` address.

muster believes no forwarding header by default, so behind `tailscale serve`
every device has the same client address in muster's per-address limits. That
is the safe default; leave it. If you later put muster behind a reverse proxy
of your own instead, set `MUSTER_TRUSTED_PROXIES` and `MUSTER_CLIENT_IP_HEADER`
in `.env` as
[Client addresses behind a proxy](docs/configuration.md#client-addresses-behind-a-proxy)
describes, for that proxy.

**If this fails:**
- `TAILSCALE NOT READY`: Tailscale is not installed, not running, or not
  signed in here (a signed-out Tailscale is `unready` too). This is not
  another install: bring it up (Step 0's `tailscale` row), check
  `tailscale status`, then re-run this block.
- `STOP`: this machine's Tailscale already serves something on the https port
  (or has Funnel on). Do not replace it. Show the person the status and ask:
  they can free that port themselves, or put the hub on another machine.
- "serve is not enabled on your tailnet" or a link to enable HTTPS: MagicDNS
  and HTTPS Certificates are still off (Step 1, question 4 asked for them).
  Give the person the link; they turn both on in the Tailscale admin console
  (DNS page). Then re-run this block.
- "access denied" on Linux: the operator was not set. Run
  `sudo tailscale set --operator="$USER"` once (approved in Step 1), then
  re-run.
- `up --wait` fails: `mi_logs --tail=80 muster-init muster`; muster names
  the setting it refuses.

### The phone

1. **Join the tailnet.** Install the Tailscale app (Google Play or the App
   Store) and sign in with the same account as the machines. The muster address
   only works on the tailnet.
2. **Open muster.** Step 5's check prints a QR code of the address when
   `qrencode` is installed; scan it with the camera. Or type the address
   (`echo "$MUSTER_URL"` after loading the helpers).
3. **Sign in.** Do **not** print the password yourself; tell the person to run
   this on the hub and copy it into their password manager:
   `docker compose -f ~/muster/docker-compose.yml -p muster exec muster /muster-server show-secret ui-password`
4. **Install it to the home screen.**
   - **Android (Chrome):** tap **Install** in muster's header, or the ⋮ menu
     → **Add to Home screen** → **Install**.
   - **iPhone (Safari):** the Share button → **Add to Home Screen** → **Add**.
     Then always open muster from that icon: iOS allows notifications only for
     an installed app.
5. **Turn on notifications.** In the installed app, tap **Enable** in the
   header and allow. Then **Account → Notifications → Send a test
   notification**.
6. **About passkeys.** This setup uses one shared password, and "Confirm it's
   you" (before typing into a session) asks for that password again. Passkeys
   come with per-user accounts ([docs/features.md](docs/features.md#accounts)),
   which this guide does not set up.
7. **Optional: the native Android app** at
   <https://github.com/ZacxDev/muster-android>.

### Another machine

Each machine has its own **mirror token**, minted on the hub. It lets that
machine's `muster-mirror` connect, and nothing else. Run this on the **joining**
machine, after its Step 0 and Step 1 (answer 1: yes). Skip it if
`~/.config/muster-mirror/token` already holds a token. Ask the person which way:

**If you may reach the hub over `ssh`** (the person gives the hub's ssh name; put it in place of `HUB`):

```sh
. "$HOME/.config/muster-install/lib.sh"
tok="$HOME/.config/muster-mirror/token"
mkdir -p "$HOME/.config/muster-mirror" && chmod 700 "$HOME/.config/muster-mirror"
if [ -s "$tok" ]; then echo "kept existing token"
else
  # BatchMode: fail instead of prompting. A login shell, so Docker is on
  # PATH (macOS too). Only the marked line is the token, whatever the
  # hub's shell start-up files print.
  out=$(ssh -o BatchMode=yes HUB sh -l -s -- "$MIRROR_HOST" <<'REMOTE'
t=$(docker compose -f "$HOME/muster/docker-compose.yml" -p muster exec -T muster /muster-server mirror token create "$1") &&
printf 'MUSTER-TOKEN:%s\n' "$t"
REMOTE
)
  t=$(printf '%s\n' "$out" | sed -n 's/^MUSTER-TOKEN://p' | tail -n 1)
  if [ -n "$t" ]; then (umask 077; printf '%s\n' "$t" > "$tok") && echo "minted token for $MIRROR_HOST"
  else echo "MINT FAILED over ssh: nothing written"; fi
  unset out t
fi
```

**Otherwise**, give the person this line to run on the hub, with this
machine's name (`echo "$MIRROR_HOST"`) filled in, and ask them to paste what it
prints:
`docker compose -f ~/muster/docker-compose.yml -p muster exec -T muster /muster-server mirror token create <this machine's name>`.
Write the pasted token without echoing it:
`(umask 077; mkdir -p ~/.config/muster-mirror; printf '%s' 'THE-TOKEN' > ~/.config/muster-mirror/token)`.

Then run **Step 2** on this machine: with `ROLE=join` it connects to the hub.

**If this fails:** `MINT FAILED over ssh`: check `ssh HUB true` works without a
password prompt, or use the paste way. A token the hub refuses (401 in Step 2):
the token does not match a live token for this exact host name. On the hub,
`… mirror token ls` shows each host; mint a new one (delete the old file
first) and revoke the stale one with `… mirror token revoke <id>`.

For a machine whose sessions should also appear in a **second** muster server
(its own URL, its own account), see
[docs/multi-tenant-hosts.md](docs/multi-tenant-hosts.md). Only when the person
asks for it.

---

## Step 4: Run your agents in tmux

Tell the person this. muster shows **tmux windows**. Any coding agent started inside tmux on a
connected machine appears as a card, and a Claude Code window opens as a chat.
These are **mirrored sessions**: the person's own sessions. A message from
muster is typed into that same session, so it is all there when they are back
at the keyboard. This guide sets up nothing else: it starts no agent of its
own, and dispatched agents (muster starting new sessions in Kubernetes) stay
off (<https://github.com/ZacxDev/muster/blob/main/docs/kubernetes.md>).

A session the person already has open **outside** tmux is invisible to muster,
and starting a second one in tmux makes a separate conversation. To bring the
one they have into tmux, give them this link: <https://github.com/ZacxDev/muster/blob/main/docs/mirroring.md#bring-a-session-you-already-have-into-tmux>.

Give the person these one-liners (`-A` reattaches if the session already
exists, so they are safe to repeat):

```text
tmux new -A -s work claude      # Claude Code
tmux new -A -s codex codex      # Codex
tmux new -A -s oc opencode      # opencode
tmux new -A -s gem gemini       # Gemini CLI
```

Another agent in the same session: `tmux new-window -t work claude`. Detach
with `Ctrl-b d`; the agent keeps running and stays in muster.

**opencode: version 2 or later.** muster needs opencode 2+, and
`curl -fsSL https://opencode.ai/install | bash` installs opencode **1**
(`opencode --version` says which). Install 2 with
`curl -fsSL https://opencode.ai/v2/install | bash` (it goes in
`~/.opencode/bin`, which its installer adds to PATH in `~/.bashrc`), or upgrade a 1.x with
`opencode upgrade --major`. Then start opencode once (the TUI above, or
`opencode serve`), so it writes the file `muster-mirror opencode-pair` reads,
and pair this machine: `muster-mirror opencode-pair`
([docs/mirroring.md](docs/mirroring.md), "opencode's token"). The pair
command says which of these is missing.

**No chief, on purpose.** muster's *chief* is an optional agent that reports
on the rest of the fleet. It is off by default and this guide does not turn it
on ([docs/features.md](docs/features.md#chief)).

---

## Step 5: Check it

On the **hub**, the doctor checks the server, sign-in, this machine's
muster-mirror and tmux, and lists every feature. The password and token go into
a private file it reads, never onto a command line; with Tailscale added, a
second run checks the HTTPS address the phone uses and draws its QR code:

```sh
. "$HOME/.config/muster-install/lib.sh"
local_url=$(mi_local_url); d="$MI_DIR/doctor.env"
(umask 077; {
  echo "MUSTER_PUBLIC_URL=$local_url"
  echo "MUSTER_HOOK_TOKEN=$(mi_dc exec -T muster /muster-server show-secret hook-token)"
  echo "MUSTER_UI_PASSWORD=$(mi_dc exec -T muster /muster-server show-secret ui-password)"
} > "$d")
"$MUSTER_BIN/muster" doctor --deploy-env "$d" >/dev/null; rc=$?
rm -f "$d"
case "${MUSTER_URL:-}" in https://*) "$MUSTER_BIN/muster" doctor --api-url "$MUSTER_URL" >/dev/null || rc=$? ;; esac
echo "doctor: exit $rc"
```

On **every other machine**:

```sh
. "$HOME/.config/muster-install/lib.sh"
"$MUSTER_BIN/muster" doctor --api-url "$MUSTER_URL" >/dev/null
```

There, the checks that need the server's credentials report SKIP, and
`this-host` asks the hub, with this machine's own mirror token, whether it
sees this machine connected.

`muster doctor` prints one line per check, `PASS`, `FAIL` or `SKIP`, and a
`fix:` line under each failure, on stderr. It exits non-zero if anything
failed. Its stdout is the same result as one JSON document, for scripts; the
`>/dev/null` above drops that copy, so you read the report once. What each
check proves: `muster doctor --help`, and the table in
[docs/cli.md](docs/cli.md#muster-doctor).

Work through each `FAIL` with its `fix:` line and the matching step above, then
run the doctor again. **Done means the hub's doctor exits 0**, every machine's
doctor shows `this-host`, `mirror-service` and `tmux` passing, the person can
open muster and see a session, and, with Tailscale added, the person confirms
on the phone that they can see a session and that the test notification
arrived.

**A hub with no browser of its own** (a server reached only by ssh): muster
listens on 127.0.0.1, so open it from the person's own computer through an ssh
tunnel, `ssh -L <port>:127.0.0.1:<port> <user>@<hub>` with the hub's port
(`mi_local_port`, 8105 by default), then `http://localhost:<port>` in that
computer's browser. Tailscale (Step 3) is the way that needs no tunnel.

**A command sent over ssh** (`ssh <machine> 'muster …'`) runs without a login
shell, so it reads no profile and `~/.local/bin` is not on its PATH: write
`~/.local/bin/muster` (and `~/.local/bin/muster-mirror`) in full there.

Then tell the person, briefly:

- the address, and the command that shows the password (Step 3, the phone);
- **muster shows their own sessions.** Each card is a session running in tmux
  on one of their machines (a mirrored session). A message from muster goes
  into that same session, so it is all there when they are back at the
  keyboard. muster starts no session of its own;
- **start Claude Code inside tmux**, with the one-liners from Step 4. A
  session outside tmux does not appear;
- **a Claude Code session opens in the Rendered view**, a chat. The Terminal
  view is one tap away in the header, as the fallback. If a window opens in the
  Terminal view only, the line at its top says why;
- that `muster doctor` re-checks everything;
- that the phone and other machines are Step 3, any time (if they were left
  out), and that more is optional (Step 6).

---

## Step 6: Optional features

Only what the person asks for. Everything above is muster's core; every other
feature is optional and off.
**<https://github.com/ZacxDev/muster/blob/main/docs/features.md>** is the one
page that lists them, with what each needs (an extra service, a key, your own
infrastructure) and the exact `.env` lines and compose profile. Follow that
page for the facts; this guide does not repeat them.

Turning a feature on is the same every time, on the hub: its lines go into
`~/muster/.env` (beside `docker-compose.yml`; `env_set` creates it, mode 600),
its compose profile (if it has one) into `COMPOSE_PROFILES` there, and then
`mi_dc up -d --wait --quiet-pull` brings the server up with them (`--quiet-pull`
keeps a new image's download progress out of your output). Use the helpers so no
secret reaches a command line or your reply:

- a random secret the section asks for: `env_secret <NAME> <bytes>` (kept on a
  re-run);
- a key the person gave in a file: `env_set <NAME> "$(cat "$MI_DIR/<file>")"`,
  then `rm "$MI_DIR/<file>"`;
- any other value: `env_set <NAME> <value>`;
- the profile: `env_set COMPOSE_PROFILES "$(mi_profiles <profile>)"` (keeps
  every other profile; `mi_profiles <new> <prefix>` replaces one, such as one
  dictation profile with another).

**Example: file attachments.** The section
([docs/features.md](docs/features.md#attachments)) names a profile and two
secrets:

```sh
. "$HOME/.config/muster-install/lib.sh"
env_secret S3_STORE_SECRET_KEY 24        # the store's root password, and muster's key
env_secret MUSTER_UPLOAD_SIGNING_KEY 48  # signs the links agents are given
env_set COMPOSE_PROFILES "$(mi_profiles s3)"
echo "profiles: $(env_get COMPOSE_PROFILES)"
mi_dc up -d --wait --quiet-pull
```

Check each feature with Step 5's doctor (its feature list says on, off or
misconfigured, and what an off one needs) or the start-up line its section
quotes.

**Per machine: the agent ask tool and ask cards.** They let a Claude Code
session ask the person a question and wait for the answer, and show Claude's
multiple-choice questions as forms in muster. They change the person's own
Claude Code configuration on that machine: ask before doing them. The steps are
under "Agent ask tool" and "Ask cards" in
<https://github.com/ZacxDev/muster/blob/main/docs/features.md#agent-ask-tool>.
A line they add to `~/.config/muster-mirror/env` is kept when Step 2 runs
again.

**If this fails:**
- `up --wait` fails with the new profile: `mi_logs --tail=80 <service>`.
  Each one names the `.env` line it is missing.
- The feature's log line, or the doctor's feature list, says `OFF` or
  `MISCONFIGURED`: it names the variable. Fix it in `.env` and run
  `mi_dc up -d --wait` again.

---

## Updating and removing

- **Update**: download the newer guide (the prompt's `curl`), then on the hub
  run its Step 0 and Step 2, and on every other machine the same (Step 2 there
  installs the release the hub runs). Step 5 checks it.
- **Change the sign-in password** (if it leaked, or to choose a new one): a
  `MUSTER_UI_PASSWORD` in the hub's `.env` wins over the generated one, so
  this sets a new random one and restarts muster with it. The person reads the
  new one with the `show-secret ui-password` command (Step 3, the phone), which
  shows it from then on; you never print it:

  ```sh
  . "$HOME/.config/muster-install/lib.sh"
  env_set MUSTER_UI_PASSWORD "$(mi_rand 18)" && mi_dc up -d --wait --quiet-pull && echo "PASSWORD CHANGED"
  ```

- **Verify what was downloaded** (optional; Step 2 already checks every
  file against the release's `checksums.txt`): each release asset, each
  program in it and the release's muster image have a signed build-provenance
  attestation. With
  the GitHub CLI signed in, a pass names the workflow and commit that built it;
  anything else means stop and tell the person:

  ```sh
  . "$HOME/.config/muster-install/lib.sh"
  gh attestation verify "$MUSTER_BIN/muster" --repo ZacxDev/muster &&
    gh attestation verify "$MUSTER_BIN/muster-mirror" --repo ZacxDev/muster &&
    gh attestation verify "oci://ghcr.io/zacxdev/muster:$MUSTER_VERSION" --repo ZacxDev/muster && echo "ATTESTED"
  ```

  Releases before the one that introduced attestations have none (the
  command says so). [docs/releases.md](docs/releases.md#verifying-a-download)
  has the rest.
- **Remove a machine:** stop its service (`systemctl --user disable --now
  muster-mirror.service`, or `launchctl bootout gui/$(id -u)/io.github.zacxdev.muster-mirror`),
  and revoke its token on the hub (`mi_dc exec -T muster /muster-server mirror
  token ls`, then `… revoke <id>`). If the programs were installed as system
  packages (`MUSTER_PACKAGE=deb|rpm`; this guide never does), also
  `sudo apt remove muster muster-mirror` or `sudo dnf remove muster muster-mirror`
  (ask first: it removes them for every user of the machine).
- **Remove muster's HTTPS mapping**, and only it: `tailscale serve --https=443
  off` when `mi_serve_state` says `ours`. Never `tailscale serve reset`: it
  removes everything else this machine serves too.
- **Remove everything** (ask first; it deletes all tasks and history): on the
  hub, `mi_dc down -v`, remove muster's HTTPS mapping as above, then remove
  `~/muster`, `~/.config/muster-mirror`, `~/.config/muster-install` and
  `~/.muster`.

### An install from an earlier version of this guide

An earlier guide made `~/muster` itself, with a `.env` holding its own
passwords (Step 0 says `existing muster: NOT install.sh's`). It keeps working
unchanged: Step 2 refuses to touch it, and nothing here moves its data. To
update its server to this guide's release:

```sh
. "$HOME/.config/muster-install/lib.sh"
mi_release || { echo "VERSION LOOKUP FAILED"; exit 1; }
tmp=$(mktemp -d) && cd "$tmp" || exit 1
curl -fsSL -o checksums.txt "$MUSTER_RELEASE_BASE/checksums.txt" &&
  curl -fsSL -o docker-compose.yml "$MUSTER_RELEASE_BASE/docker-compose.yml" &&
  mi_verify docker-compose.yml && cp docker-compose.yml "$MUSTER_DIR/docker-compose.yml" &&
  env_set MUSTER_VERSION "$MUSTER_VERSION" && mi_dc up -d --wait && curl -fsS "$MUSTER_URL/health" && echo
cd / && rm -rf "$tmp"
```

Then update each machine's `muster-mirror`, the hub's included, by running
Step 2 there with `ROLE=join` and the hub's `https://` address as `MUSTER_URL`
(answer Step 1's question 3 with the settings take-over: yes); every line the
earlier guide or the person added to `~/.config/muster-mirror/env` is kept.

### Starting over (a failed first install only)

Only for this guide's **own fresh** install: Step 2 started it, and muster has
**never** answered healthy (once it has, it may hold data, and the block
refuses). It also refuses any install this guide did not start, and prints
exactly what would be deleted:

```sh
. "$HOME/.config/muster-install/lib.sh"
cd "$MUSTER_DIR" || exit 1
proj=${MUSTER_PROJECT:-muster}
if [ "${GUIDE_STARTED:-}" != 1 ] || [ "${GUIDE_IN_USE:-}" = 1 ] || ! mi_foreign "$proj"; then
  echo "REFUSED: this is not this guide's own fresh install. Delete nothing; ask the person."
else
  echo "This would DELETE, in compose project $proj ($MUSTER_DIR):"
  docker ps -a --filter "label=com.docker.compose.project=$proj" --format '  container {{.Names}}'
  docker volume ls --filter "label=com.docker.compose.project=$proj" --format '  volume {{.Name}}'
  [ "${MI_CONFIRM_DELETE:-}" = "$proj" ] && mi_dc down -v && mi_save GUIDE_STARTED 0 ||
    echo "Nothing deleted. Show the person this list; only if they agree, re-run with MI_CONFIRM_DELETE=$proj."
fi
```

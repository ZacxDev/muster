# Installing muster

> **This file is written for an AI coding agent to follow** (Claude Code, Codex,
> opencode, Gemini CLI or any other agent that can run shell commands). A person
> starts it by pasting this one prompt into their agent:

```text
Set up muster for me: read https://raw.githubusercontent.com/ZacxDev/muster/main/INSTALL.md and follow it step by step. Ask me everything you need in one go up front, then do the rest yourself.
```

When it is done, the person has:

- the muster server running on one machine (the **hub**), reachable over HTTPS
  on their private Tailscale network and nowhere else;
- each of their machines (the hub, and any others) showing its tmux sessions in
  muster, so every Claude Code, Codex, opencode or Gemini CLI session running
  in tmux appears as a card they can read and answer;
- muster installed on their phone's home screen, signed in, with notifications
  and voice dictation working;
- a passing `muster doctor`.

## How to follow this guide (agent: read this first)

- **Run the commands yourself**, in order, in a POSIX shell (`sh`, `bash` or
  `zsh`). Every step is safe to re-run: it checks what already exists and keeps
  it. If you stop half way, start again from Step 0.
- **Each shell block starts by loading the helpers** written in Step 0
  (`. "$HOME/.config/muster-install/lib.sh"`). They hold the values earlier
  steps found, so it does not matter whether your shell keeps variables between
  commands.
- **Ask the person questions only in Step 1**, all at once. After that, do not
  stop to ask unless a step says "ask".
- **Never print a secret.** Passwords, tokens and keys go straight into files
  with mode `600`. Do not echo them, do not paste them into your reply, and do
  not put them on a command line other than as shown.
- **Ask before using `sudo`** or installing system packages, unless the person
  allowed it in Step 1.
- Every step ends with **If this fails**. Follow it before trying anything
  else. If you are still stuck, stop and tell the person exactly which step and
  command failed and what it printed.
- Nothing here opens a port to the internet. Everything is reached through
  Tailscale.

---

## Step 0: Preflight (on every machine)

Write the helpers, then detect the machine. This creates
`~/.config/muster-install/`; it changes nothing else.

```sh
mkdir -p "$HOME/.config/muster-install" && chmod 700 "$HOME/.config/muster-install"
cat > "$HOME/.config/muster-install/lib.sh" <<'LIB'
# Helpers for INSTALL.md. Safe to source any number of times.
MI_DIR="$HOME/.config/muster-install"
[ -f "$MI_DIR/state" ] && . "$MI_DIR/state"
MUSTER_DIR="${MUSTER_DIR:-$HOME/muster}"
MUSTER_BIN="${MUSTER_BIN:-$HOME/.local/bin}"
# mi_save KEY VALUE: remember a value for later steps (and later shells).
mi_save() {
  touch "$MI_DIR/state" && chmod 600 "$MI_DIR/state"
  grep -v "^$1=" "$MI_DIR/state" > "$MI_DIR/state.tmp" || true
  printf "%s='%s'\n" "$1" "$2" >> "$MI_DIR/state.tmp" && mv "$MI_DIR/state.tmp" "$MI_DIR/state"
  eval "$1='$2'"
}
# mi_rand [BYTES]: a random hex string (2 characters per byte).
mi_rand() { head -c "${1:-24}" /dev/urandom | od -An -tx1 | tr -d ' \n'; }
# mi_verify FILE: check FILE (in the current directory) against ./checksums.txt.
mi_verify() {
  line=$(grep "[ *]$1\$" checksums.txt) || { echo "mi_verify: $1 is not listed in checksums.txt" >&2; return 1; }
  if command -v sha256sum >/dev/null 2>&1; then printf '%s\n' "$line" | sha256sum -c -
  else printf '%s\n' "$line" | shasum -a 256 -c -; fi
}
# env_get KEY / env_set KEY VALUE: read and write the server's .env (hub only).
env_get() { sed -n "s/^$1=//p" "$MUSTER_DIR/.env" | tail -n 1; }
env_example() { sed -n "s/^$1=//p" "$MUSTER_DIR/.env.example" | tail -n 1; }
env_set() {
  tmp="$MUSTER_DIR/.env.tmp"
  awk -v k="$1" -v v="$2" 'BEGIN{done=0} index($0, k"=")==1 {if(!done){print k"="v; done=1}; next} {print} END{if(!done) print k"="v}' \
    "$MUSTER_DIR/.env" > "$tmp" && cat "$tmp" > "$MUSTER_DIR/.env" && rm -f "$tmp"
}
# env_secret KEY [BYTES]: generate KEY unless it already holds a real value
# (an empty value, or the placeholder .env.example ships, is not real).
env_secret() {
  cur=$(env_get "$1")
  if [ -z "$cur" ] || [ "$cur" = "$(env_example "$1")" ]; then env_set "$1" "$(mi_rand "${2:-24}")"; fi
}
LIB
. "$HOME/.config/muster-install/lib.sh"

case "$(uname -s)" in Linux) mi_save OS linux ;; Darwin) mi_save OS darwin ;; *) echo "UNSUPPORTED OS: $(uname -s)" ;; esac
case "$(uname -m)" in x86_64|amd64) mi_save ARCH amd64 ;; aarch64|arm64) mi_save ARCH arm64 ;; *) echo "UNSUPPORTED ARCH: $(uname -m)" ;; esac
mi_save HOSTNAME_SHORT "$(hostname -s 2>/dev/null || hostname | cut -d. -f1)"
mi_save MIRROR_HOST "$(printf '%s' "$HOSTNAME_SHORT" | tr 'A-Z' 'a-z')"
echo "os=$OS arch=$ARCH host=$MIRROR_HOST"
for t in curl tar tmux tailscale docker qrencode nvidia-smi; do
  printf '%-11s %s\n' "$t" "$(command -v "$t" || echo MISSING)"
done
docker compose version 2>/dev/null || echo "docker compose: MISSING"
tailscale status >/dev/null 2>&1 && echo "tailscale: logged in" || echo "tailscale: NOT logged in (or not running)"
df -Pk "$HOME" | awk 'NR==2 {printf "free disk in $HOME: %.1f GB\n", $4/1048576}'
nvidia-smi --query-gpu=name,memory.total --format=csv,noheader 2>/dev/null || echo "gpu: none detected"
docker info --format '{{json .Runtimes}}' 2>/dev/null | grep -q nvidia && echo "docker gpu runtime: yes" || echo "docker gpu runtime: no"
```

Read the output and note:

| what | needed on | if it is missing |
|---|---|---|
| `os`, `arch` | every machine | Linux and macOS on amd64 or arm64 only. Stop on anything else and tell the person. |
| `curl`, `tar` | every machine | Install them with the system package manager (ask first). |
| `tmux` | every machine | Install it (`apt install tmux`, `dnf install tmux`, `brew install tmux`; ask first). muster shows tmux windows, so this is required. |
| `tailscale`, logged in | every machine | Install from <https://tailscale.com/download>. Then `tailscale up` (on Linux, `sudo tailscale up`) prints a sign-in URL: give it to the person to open, and wait. Every machine and the phone must sign in to the **same** tailnet. |
| `docker` and `docker compose` | the hub only | Install Docker Engine (Linux) or Docker Desktop (macOS) with the Compose v2 plugin. |
| free disk | the hub | At least 10 GB; 20 GB if dictation will run locally (Step 1). |
| `qrencode` | optional | Lets `muster doctor` draw the phone's QR code. Install it if allowed. |
| gpu and `docker gpu runtime` | the hub, for GPU dictation | An NVIDIA GPU with at least 6 GB of memory **and** a Docker GPU runtime (the NVIDIA Container Toolkit) makes the best dictation option available. |

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
     (`mi_save MUSTER_URL https://…`) and how this machine will receive its
     mirror token: the person can paste it, or you may fetch it from the hub
     over `ssh` if they allow that. `mi_save ROLE join`, then skip to Step 6.
2. *(hub only)* **Which other machines should show their sessions?** Names
   only, for example `laptop, desktop`. They can also be added at any time
   later. `mi_save OTHER_HOSTS "laptop desktop"` (lowercase, spaces between,
   each the machine's short hostname).
3. *(hub only)* **How should voice dictation work?** Offer these, recommending
   the first that fits:
   - **Local, on the GPU — recommended when Step 0 found an NVIDIA GPU with
     6 GB or more and a Docker GPU runtime.** The Cohere transcribe model: the
     most accurate option, and audio never leaves the hub. It needs a Hugging
     Face access token from an account that has accepted the licence of
     `CohereLabs/cohere-transcribe-03-2026` (the model is gated), and its first
     start can take up to 30 minutes. Ask for the token and write it without
     echoing it: `(umask 077; printf '%s' 'THE-TOKEN' > "$MI_DIR/hf-token")`,
     then `mi_save STT local-gpu`.
   - **Local, on the CPU.** Whisper. Works on any hub, slower and less
     accurate. Audio never leaves the hub. `mi_save STT local-cpu`
   - **Your own API key.** Any OpenAI-compatible speech-to-text API: no
     hardware needed, but recordings are sent to that provider. Ask for the
     base URL (default `https://api.openai.com`), the key, and the model
     (default `whisper-1`). `mi_save STT api`, `mi_save STT_API_URL …`,
     `mi_save STT_API_MODEL …`; write the key to a file without echoing it:
     `(umask 077; printf '%s' 'THE-KEY' > "$MI_DIR/stt-api-key")`.
   - **None for now.** `mi_save STT none`
4. **May muster type into this machine's terminals?** (`-interactive`, asked
   per machine.) Explain: **yes** lets you send messages to your agents from
   your phone, which is the main point of muster. It also means anyone who
   signs in to your muster, and confirms the password again, can type anything
   into any tmux window on this machine, which is a shell as your user. **No**
   keeps the machine read-only: you can watch every session but not answer.
   Recommend **yes** on machines the person owns.
   `mi_save INTERACTIVE 1` (yes) or `mi_save INTERACTIVE 0` (no).
5. **May I install missing packages, and use `sudo` where a step needs it?**
   (Only ask if Step 0 found something missing, or on Linux, where Step 7 runs
   `loginctl enable-linger`.) `mi_save SUDO_OK 1` or `mi_save SUDO_OK 0`.

**If this fails:** if the person does not answer a question, use the
recommendation and say so.

---

## Step 2: HTTPS through Tailscale (hub)

`tailscale serve` gives the hub a real HTTPS certificate for its tailnet name and
forwards to muster on `127.0.0.1:8105`. Only devices on the tailnet can reach it.

```sh
. "$HOME/.config/muster-install/lib.sh"
tailscale serve --bg 8105
tailscale serve status
mi_save MUSTER_URL "$(tailscale serve status 2>/dev/null | grep -o 'https://[^ ]*' | head -n 1 | sed 's:/$::')"
echo "MUSTER_URL=$MUSTER_URL"
```

`MUSTER_URL` must be an `https://…` address ending in the tailnet's domain
(`https://<machine>.<tailnet>.ts.net`). muster is not running yet, so opening it
now fails; that is expected.

**If this fails:**
- "serve is not enabled on your tailnet" or a link to enable HTTPS: give the
  person the link. They must turn on **MagicDNS** and **HTTPS Certificates** in
  the Tailscale admin console (DNS page). Then re-run this block.
- "access denied" on Linux: run `sudo tailscale set --operator="$USER"` once
  (ask first), then re-run.
- `MUSTER_URL` is empty: run `tailscale serve status` and copy the `https://`
  address by hand into `mi_save MUSTER_URL …`.

---

## Step 3: Download the release (hub)

```sh
. "$HOME/.config/muster-install/lib.sh"
if [ -z "${MUSTER_VERSION:-}" ]; then
  mi_save MUSTER_VERSION "$(curl -fsSL https://api.github.com/repos/ZacxDev/muster/releases/latest \
    | sed -n 's/.*"tag_name": *"v\{0,1\}\([^"]*\)".*/\1/p' | head -n 1)"
fi
mi_save MUSTER_RELEASE_BASE "${MUSTER_RELEASE_BASE:-https://github.com/ZacxDev/muster/releases/download/v$MUSTER_VERSION}"
echo "version=$MUSTER_VERSION"
mkdir -p "$MUSTER_DIR" && cd "$MUSTER_DIR"
curl -fsSL -o checksums.txt "$MUSTER_RELEASE_BASE/checksums.txt"
curl -fsSL -o docker-compose.yml "$MUSTER_RELEASE_BASE/docker-compose.yml"
curl -fsSL -o env.example "$MUSTER_RELEASE_BASE/env.example"
mi_verify docker-compose.yml && mi_verify env.example && cp env.example .env.example
```

The release attaches the settings template as `env.example`, without the
leading dot, because GitHub renames asset names that start with one. It is
kept here as `.env.example`.

Then **read `.env.example` in full.** It is the authority on every setting the
compose file uses. The steps below set the ones this guide needs; follow its
comments for anything else.

**If this fails:**
- `version=` is empty: the GitHub API was rate-limited or unreachable. Open
  <https://github.com/ZacxDev/muster/releases>, take the newest version
  number (without the `v`), run `mi_save MUSTER_VERSION <it>` and re-run.
- A checksum `FAILED`: delete the downloaded files and re-run once. A second
  mismatch: stop and tell the person.

---

## Step 4: Configure the server (hub)

This fills in `~/muster/.env`. Existing values are kept, so re-running never
changes a password or key that is already in use. Values still set to
`.env.example`'s placeholder (`CHANGE-ME`) are generated.

```sh
. "$HOME/.config/muster-install/lib.sh"
cd "$MUSTER_DIR"
[ -f .env ] || cp .env.example .env
chmod 600 .env
env_set MUSTER_VERSION "$MUSTER_VERSION"
env_secret POSTGRES_PASSWORD 24
env_secret MUSTER_UI_PASSWORD 12     # the sign-in password: 24 characters (12 is the minimum)
env_secret MUSTER_HOOK_TOKEN 32      # the machine API's token
env_set MUSTER_PUBLIC_URL "$MUSTER_URL"
env_set MUSTER_SECURE_COOKIES 1
# Web push keys, generated once, with the release image itself.
if [ -z "$(env_get MUSTER_VAPID_PRIVATE_KEY)" ]; then
  docker run --rm "${MUSTER_IMAGE:-ghcr.io/zacxdev/muster}:$MUSTER_VERSION" vapid generate > "$MI_DIR/vapid.tmp" &&
    env_set MUSTER_VAPID_PUBLIC_KEY "$(sed -n 's/^MUSTER_VAPID_PUBLIC_KEY=//p' "$MI_DIR/vapid.tmp")" &&
    env_set MUSTER_VAPID_PRIVATE_KEY "$(sed -n 's/^MUSTER_VAPID_PRIVATE_KEY=//p' "$MI_DIR/vapid.tmp")"
  rm -f "$MI_DIR/vapid.tmp"
fi
env_set MUSTER_VAPID_SUBJECT "$MUSTER_URL"
# Hand the push keys to the muster container, unless the compose file already does.
if ! grep -q MUSTER_VAPID_PUBLIC_KEY docker-compose.yml; then
  cat > docker-compose.override.yml <<'YML'
# Written by INSTALL.md: passes the web push keys from .env to muster.
services:
  muster:
    environment:
      MUSTER_VAPID_PUBLIC_KEY: ${MUSTER_VAPID_PUBLIC_KEY:-}
      MUSTER_VAPID_PRIVATE_KEY: ${MUSTER_VAPID_PRIVATE_KEY:-}
      MUSTER_VAPID_SUBJECT: ${MUSTER_VAPID_SUBJECT:-}
YML
fi
grep -c '^[A-Z_]*=CHANGE-ME$' .env | sed 's/^/placeholders left: /'
```

`placeholders left` must be `0` (the commented-out dictation blocks do not
count, because they start with `#`). Then dictation, by the answer saved in
Step 1. Run **one** of these:

```sh
. "$HOME/.config/muster-install/lib.sh"
case "$STT" in
  local-gpu)
    env_set COMPOSE_PROFILES stt-cohere
    env_set MUSTER_STT_URL http://stt-cohere:8000
    env_secret MUSTER_STT_TOKEN 32
    env_set HF_TOKEN "$(cat "$MI_DIR/hf-token")" && rm -f "$MI_DIR/hf-token" ;;
  local-cpu)
    env_set COMPOSE_PROFILES stt-whisper
    env_set MUSTER_STT_URL http://stt-whisper:8000
    env_secret MUSTER_STT_TOKEN 32
    [ -n "$(env_get MUSTER_STT_MODEL)" ] || env_set MUSTER_STT_MODEL Systran/faster-whisper-small ;;
  api)
    env_set MUSTER_STT_URL "$STT_API_URL"
    env_set MUSTER_STT_MODEL "$STT_API_MODEL"
    env_set MUSTER_STT_TOKEN "$(cat "$MI_DIR/stt-api-key")" && rm -f "$MI_DIR/stt-api-key" ;;
  none) echo "dictation stays off" ;;
esac
echo "dictation: $STT"
```

- **`local-gpu`** reads the Hugging Face token Step 1 wrote to
  `~/.config/muster-install/hf-token`. Its account must have accepted the
  licence of `CohereLabs/cohere-transcribe-03-2026` (the model is gated).
- **`api`**: `MUSTER_STT_URL` must be the scheme and host only
  (`https://api.openai.com`). muster appends `/v1/audio/transcriptions`, so a
  provider whose endpoint lives under another path cannot be used directly.
- **`none`**: dictation is off, and the doctor's dictation check fails. Pass
  `--skip dictation` to it.

**If this fails:**
- `docker run … vapid generate` cannot pull the image: run
  `docker pull ghcr.io/zacxdev/muster:$MUSTER_VERSION` and read its error
  (network, or a version that does not exist; check Step 3).
- To start over completely, the person must agree first: `docker compose down
  -v` deletes the database, then remove `~/muster/.env`.

---

## Step 5: Start the server (hub)

```sh
. "$HOME/.config/muster-install/lib.sh"
cd "$MUSTER_DIR"
docker compose pull
docker compose up -d --wait --wait-timeout 600
docker compose ps
curl -fsS http://127.0.0.1:8105/health && echo
curl -fsS "$MUSTER_URL/health" && echo
```

Both `curl`s must print `{"status":"ok",…}`. The first proves muster is up, the
second that Tailscale's HTTPS reaches it.

A local dictation service downloads its model on first start. Whisper takes a
few minutes, and the GPU (Cohere) model can take up to 30. If `up --wait`
times out while only the `stt-…` service is still `starting`, that is the
download: wait a few minutes and re-run this block until it is `healthy`.

**If this fails:**
- `up --wait` fails or `muster` is unhealthy: `docker compose logs --tail=80
  muster`. muster refuses to start on a bad setting and names the variable in
  that log (a `MUSTER_UI_PASSWORD` under 12 characters is the usual one). Fix it
  in `.env` and re-run this block.
- "port is already allocated" for 8105: something else uses the port. Find it
  with `ss -ltnp 'sport = :8105'` (Linux) or `lsof -iTCP:8105 -sTCP:LISTEN`
  (macOS) and tell the person; do not stop it yourself.
- The second `curl` fails but the first works: re-run Step 2, and check
  `tailscale serve status` forwards to `http://127.0.0.1:8105`.
- The `stt-cohere` service fails: `docker compose logs --tail=80 stt-cohere`.
  If the model download is refused, the Hugging Face account has not accepted
  the model's licence. If no GPU is found, the Docker GPU runtime is missing.
  Either way, offer the person the CPU or API option and redo Step 4's
  dictation block.

---

## Step 6: Install the binaries (every machine)

`muster-mirror` shows this machine's tmux sessions in muster. `muster` is the
command-line client, used here for `muster doctor`. Both are checksum-verified.

On a **joining** machine, first take the version from the hub:

```sh
. "$HOME/.config/muster-install/lib.sh"
mi_save MUSTER_VERSION "$(curl -fsS "$MUSTER_URL/health" | sed -n 's/.*"version": *"\([^"]*\)".*/\1/p')"
mi_save MUSTER_RELEASE_BASE "${MUSTER_RELEASE_BASE:-https://github.com/ZacxDev/muster/releases/download/v$MUSTER_VERSION}"
echo "version=$MUSTER_VERSION"
```

Then, on every machine:

```sh
. "$HOME/.config/muster-install/lib.sh"
mkdir -p "$MUSTER_BIN"
tmp=$(mktemp -d) && cd "$tmp"
curl -fsSL -o checksums.txt "$MUSTER_RELEASE_BASE/checksums.txt"
for name in muster-mirror muster; do
  asset="${name}_${OS}_${ARCH}.tar.gz"
  curl -fsSL -o "$asset" "$MUSTER_RELEASE_BASE/$asset" &&
    mi_verify "$asset" &&
    mkdir -p "x-$name" && tar -xzf "$asset" -C "x-$name" &&
    bin=$(find "x-$name" -type f -name "$name" | head -n 1) &&
    [ -n "$bin" ] && install -m 755 "$bin" "$MUSTER_BIN/$name" || { echo "FAILED: $name"; break; }
done
cd / && rm -rf "$tmp"
"$MUSTER_BIN/muster-mirror" -version
"$MUSTER_BIN/muster" --version
case ":$PATH:" in *":$MUSTER_BIN:"*) ;; *) echo "NOTE: add $MUSTER_BIN to PATH in the shell profile" ;; esac
```

**If this fails:**
- A `FAILED` line or a checksum `FAILED`: re-run once (a partial download).
  A second mismatch: stop and tell the person; do not install that file.
- `muster-mirror -version` says "cannot execute": the wrong `OS`/`ARCH` was
  detected; check Step 0's output.
- If `$MUSTER_BIN` is not on `PATH`, add `export PATH="$HOME/.local/bin:$PATH"`
  to the person's shell profile (ask first), or use the full path below.

---

## Step 7: Connect this machine's sessions (every machine)

Each machine has its own **mirror token**, minted on the hub. It lets that
machine's `muster-mirror` connect, and nothing else.

**On the hub**, mint a token for the hub itself and for each machine named in
Step 1. A token is shown once, so it goes straight into a file. Existing tokens
are kept:

```sh
. "$HOME/.config/muster-install/lib.sh"
cd "$MUSTER_DIR"
mkdir -p "$HOME/.config/muster-mirror" "$MUSTER_DIR/join" && chmod 700 "$HOME/.config/muster-mirror" "$MUSTER_DIR/join"
mint() { # mint HOST FILE
  if [ -s "$2" ]; then echo "kept existing token for $1"; return 0; fi
  (umask 077; docker compose exec -T muster /muster-server mirror token create "$1" > "$2.tmp") &&
    [ -s "$2.tmp" ] && mv "$2.tmp" "$2" && echo "minted token for $1"
}
mint "$MIRROR_HOST" "$HOME/.config/muster-mirror/token"
for h in ${OTHER_HOSTS:-}; do mint "$h" "$MUSTER_DIR/join/$h.token"; done
docker compose exec -T muster /muster-server mirror token ls
```

For each other machine, its token is now in `~/muster/join/<host>.token` on the
hub. Get it to that machine's `~/.config/muster-mirror/token` (mode `600`):
over `ssh` if the person allowed it, for example
`ssh <host> 'mkdir -p ~/.config/muster-mirror && umask 077 && cat > ~/.config/muster-mirror/token' < ~/muster/join/<host>.token`,
or by having the person run this guide there (it asks for the hub's URL and
the token).

**On a joining machine**, if the person pastes the token to you, write it
without echoing it: `(umask 077; mkdir -p ~/.config/muster-mirror; printf '%s' 'THE-TOKEN' > ~/.config/muster-mirror/token)`.

**On every machine**, write the service's settings and start it.
It runs as a user service, so it starts at boot and restarts if it stops.

```sh
. "$HOME/.config/muster-install/lib.sh"
cfg="$HOME/.config/muster-mirror"
[ -s "$cfg/token" ] || { echo "NO TOKEN in $cfg/token: finish the token part above first"; exit 1; }
chmod 600 "$cfg/token"
tmux_dir=$(dirname "$(command -v tmux)")
{
  echo "MUSTER_MIRROR_URL=$MUSTER_URL"
  echo "MUSTER_MIRROR_TOKEN_FILE=$cfg/token"
  echo "MUSTER_MIRROR_HOST=$MIRROR_HOST"
  echo "MUSTER_MIRROR_INTERACTIVE=${INTERACTIVE:-0}"
  echo "MUSTER_MIRROR_WRITE_HOOK_CONFIG=1"
  echo "PATH=$tmux_dir:$MUSTER_BIN:/usr/local/bin:/usr/bin:/bin"
} > "$cfg/env"
chmod 600 "$cfg/env"

if [ "$OS" = linux ]; then
  mkdir -p "$HOME/.config/systemd/user"
  cat > "$HOME/.config/systemd/user/muster-mirror.service" <<UNIT
[Unit]
Description=muster-mirror: show this machine's tmux sessions in muster
After=network-online.target

[Service]
EnvironmentFile=$cfg/env
ExecStart=$MUSTER_BIN/muster-mirror
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
UNIT
  systemctl --user daemon-reload
  systemctl --user enable muster-mirror.service
  systemctl --user restart muster-mirror.service
  sleep 3
  systemctl --user is-active muster-mirror.service
  journalctl --user -u muster-mirror.service -n 15 --no-pager
else
  label=io.github.zacxdev.muster-mirror
  plist="$HOME/Library/LaunchAgents/$label.plist"
  mkdir -p "$HOME/Library/LaunchAgents" "$HOME/Library/Logs"
  {
    echo '<?xml version="1.0" encoding="UTF-8"?>'
    echo '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">'
    echo '<plist version="1.0"><dict>'
    echo "<key>Label</key><string>$label</string>"
    echo "<key>ProgramArguments</key><array><string>$MUSTER_BIN/muster-mirror</string></array>"
    echo '<key>EnvironmentVariables</key><dict>'
    while IFS='=' read -r k v; do echo "<key>$k</key><string>$v</string>"; done < "$cfg/env"
    echo '</dict>'
    echo '<key>RunAtLoad</key><true/><key>KeepAlive</key><true/>'
    echo "<key>StandardErrorPath</key><string>$HOME/Library/Logs/muster-mirror.log</string>"
    echo '</dict></plist>'
  } > "$plist"
  launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
  launchctl bootstrap "gui/$(id -u)" "$plist"
  sleep 3
  launchctl print "gui/$(id -u)/$label" | grep -m1 'state ='
  tail -n 15 "$HOME/Library/Logs/muster-mirror.log"
fi
```

The service must be `active` (Linux) or `state = running` (macOS), and its log
must show it connected, with no repeated errors.

On Linux, so the service keeps running after the person logs out:
`loginctl enable-linger "$USER"` (it may need `sudo`; ask, per Step 1).

What `-interactive` (`MUSTER_MIRROR_INTERACTIVE=1`) grants: muster can open a
**typing** terminal on this machine and paste messages into its tmux panes,
after the person confirms their password again. A tmux client can switch to any
session or open a new window, so this is a shell as this user. With `0` the
machine is read-only in muster. To change it later, edit `MUSTER_MIRROR_INTERACTIVE`
in `~/.config/muster-mirror/env` and restart the service (on macOS, re-run the
block above).

**If this fails:**
- The log says the token was refused (401): the token file does not match a
  live token for this exact host name. On the hub, `docker compose exec -T
  muster /muster-server mirror token ls` shows each host; the name must equal
  `MUSTER_MIRROR_HOST`. Mint a new one (delete the old file first) and revoke
  the stale one with `… mirror token revoke <id>`.
- The log says it cannot reach the URL: this machine is not on the tailnet, or
  the URL is wrong. `tailscale status`, then `curl -fsS "$MUSTER_URL/health"`.
- `systemctl --user` says "Failed to connect to bus": there is no user session
  (for example over a plain `ssh` without a login). Run
  `loginctl enable-linger "$USER"` (ask first), log in again, and re-run.
- It runs but no sessions appear: tmux uses a different socket for the service
  than for the person. If the person sets `TMUX_TMPDIR`, add the same line to
  `~/.config/muster-mirror/env` and restart.

---

## Step 8: Run your agents in tmux (tell the person)

muster shows **tmux windows**. Any coding agent started inside tmux on a
connected machine appears as a card, and a Claude Code window opens as a chat.
Give the person these one-liners (`-A` reattaches if the session already
exists, so they are safe to repeat):

```sh
tmux new -A -s work claude      # Claude Code
tmux new -A -s codex codex      # Codex
tmux new -A -s oc opencode      # opencode
tmux new -A -s gem gemini       # Gemini CLI
```

Another agent in the same session: `tmux new-window -t work claude`. Detach
with `Ctrl-b d`; the agent keeps running and stays in muster.

---

## Step 9: The phone

1. **Join the tailnet.** Install the Tailscale app (Google Play or the App
   Store) and sign in with the same account as the machines. The muster address
   only works on the tailnet.
2. **Open muster.** On the hub, `muster doctor` (Step 10) prints a QR code of
   the address when `qrencode` is installed; scan it with the camera. Or type
   the address (`echo "$MUSTER_URL"`).
3. **Sign in.** The password is in the hub's `.env`. Do **not** print it
   yourself; tell the person to run this on the hub and copy it into their
   password manager:
   `grep '^MUSTER_UI_PASSWORD=' ~/muster/.env | cut -d= -f2-`
   A sign-in lasts 30 days.
4. **Install it to the home screen.**
   - **Android (Chrome):** tap **Install** in muster's header, or the ⋮ menu
     → **Add to Home screen** → **Install**.
   - **iPhone (Safari):** the Share button → **Add to Home Screen** → **Add**.
     Then always open muster from that icon: iOS allows notifications only for
     an installed app.
5. **Turn on notifications.** In the installed app, tap **Enable** in the
   header and allow. Then **Account → Notifications → Send a test
   notification**.
6. **Allow the microphone.** Open any session, tap the microphone in the
   message box and allow access. Speak, tap again, and the words appear in the
   box for you to check before Send.
7. **About passkeys.** This setup uses one shared password, and "Confirm it's
   you" (before typing into a session) asks for that password again. Passkeys
   come with per-user accounts (GoTrue mode, see `docs/auth.md`), which this
   guide does not set up.
8. **Optional: the native Android app** at
   <https://github.com/ZacxDev/muster-android>.

---

## Step 10: Verify

On the **hub**, give the CLI the address and token, then run the doctor:

```sh
. "$HOME/.config/muster-install/lib.sh"
mkdir -p "$HOME/.muster" && chmod 700 "$HOME/.muster"
[ -f "$HOME/.muster/muster.env" ] || {
  (umask 077; { echo "MUSTER_API_URL=$MUSTER_URL"; echo "MUSTER_TOKEN=$(sed -n 's/^MUSTER_HOOK_TOKEN=//p' "$MUSTER_DIR/.env")"; } > "$HOME/.muster/muster.env")
}
"$MUSTER_BIN/muster" doctor --deploy-env "$MUSTER_DIR/.env"
```

On **every other machine**, run `"$MUSTER_BIN/muster" doctor --api-url "$MUSTER_URL"`.
There, the checks that need the server's credentials report SKIP; the hub's run
covers them.

`muster doctor` prints one line per check, `PASS`, `FAIL` or `SKIP`, and a
`fix:` line under each failure. It exits non-zero if anything failed. Its
checks:

| check | what it proves |
|---|---|
| `server` | the address answers over HTTPS |
| `manifest` | the phone can install muster |
| `hook-token` | the machine API has a token, and this one is accepted |
| `migrations` | the database schema is current |
| `mirror-hosts` | every machine with a token is connected (the server's view) |
| `mirror-service` | this machine's `muster-mirror` service is running |
| `tmux` | tmux is installed here |
| `sign-in` | the password signs in and opens the board |
| `push` | notification keys are set |
| `dictation` | a bundled voice sample is transcribed through muster |

Work through each `FAIL` with its `fix:` line and the matching step above, then
run the doctor again. **Done means the hub's doctor exits 0**, every machine's
doctor shows `mirror-service` and `tmux` passing, and the person confirms on
the phone that they can see a session and that the test notification arrived.

Then tell the person, briefly: the address, where the password is (step 9.3),
the tmux one-liners (Step 8), and that `muster doctor` re-checks everything.

---

## Updating and removing

- **Update:** on the hub, `mi_save MUSTER_VERSION <new>` and
  `mi_save MUSTER_RELEASE_BASE https://github.com/ZacxDev/muster/releases/download/v<new>`,
  re-run Steps 3, 5 and 6, then restart each machine's service (Step 7's last
  block). `.env` is kept.
- **Remove a machine:** stop its service (`systemctl --user disable --now
  muster-mirror.service`, or `launchctl bootout gui/$(id -u)/io.github.zacxdev.muster-mirror`),
  and revoke its token on the hub (`docker compose exec -T muster /muster-server
  mirror token ls`, then `… revoke <id>`).
- **Remove everything** (ask first; it deletes all tasks and history): on the
  hub, `cd ~/muster && docker compose down -v`, `tailscale serve reset`, then
  remove `~/muster`, `~/.config/muster-mirror`, `~/.config/muster-install` and
  `~/.muster`.

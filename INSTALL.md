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
  in tmux appears as a card they can read and answer (muster's message box
  does not send to opencode yet; its Terminal view types into it);
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
# mi_verify FILE: check FILE (in the current directory) against ./checksums.txt.
mi_verify() {
  line=$(grep "[ *]$1\$" checksums.txt) || { echo "mi_verify: $1 is not listed in checksums.txt" >&2; return 1; }
  if command -v sha256sum >/dev/null 2>&1; then printf '%s\n' "$line" | sha256sum -c -
  else printf '%s\n' "$line" | shasum -a 256 -c -; fi
}
# env_get KEY / env_set KEY VALUE: read and write the server's .env (hub only).
# env_set passes the value through the environment, never argv, and writes
# with umask 077.
env_get() { sed -n "s/^$1=//p" "$MUSTER_DIR/.env" | tail -n 1; }
env_example() { sed -n "s/^$1=//p" "$MUSTER_DIR/.env.example" | tail -n 1; }
env_set() {
  (umask 077
   MI_K="$1" MI_V="$2" awk 'BEGIN{k=ENVIRON["MI_K"]; v=ENVIRON["MI_V"]; done=0}
     index($0, k"=")==1 {if(!done){print k"="v; done=1}; next} {print} END{if(!done) print k"="v}' \
     "$MUSTER_DIR/.env" > "$MUSTER_DIR/.env.tmp" && cat "$MUSTER_DIR/.env.tmp" > "$MUSTER_DIR/.env")
  rm -f "$MUSTER_DIR/.env.tmp"
}
# env_secret KEY [BYTES]: generate KEY unless it already holds a real value
# (an empty value, or the placeholder .env.example ships, is not real).
env_secret() {
  cur=$(env_get "$1")
  if [ -z "$cur" ] || [ "$cur" = "$(env_example "$1")" ]; then env_set "$1" "$(mi_rand "${2:-24}")"; fi
}
# mi_foreign [PROJECT]: print every sign of a muster this guide did NOT make —
# containers or volumes of the compose project PROJECT (default muster) that
# are not this guide's, or something answering on 127.0.0.1:8105 before this
# guide started one. Returns 1 when it found any.
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
  if [ "${GUIDE_STARTED:-}" != 1 ] && curl -s -o /dev/null --max-time 3 http://127.0.0.1:8105/; then
    echo "FOREIGN listener on 127.0.0.1:8105"; found=1
  fi
  [ "$found" = 0 ]
}
# mi_serve_state: what `tailscale serve` already does on this machine —
# "none", "ours" (exactly https:443 / -> 127.0.0.1:8105), "other", or
# "unready" (the command failed: Tailscale is not installed, running or up).
mi_serve_state() {
  st=$(tailscale serve status 2>&1) || { echo unready; return; }
  if printf '%s\n' "$st" | grep -qi 'no serve config'; then echo none; return; fi
  if printf '%s\n' "$st" | grep -qi 'funnel'; then echo other; return; fi
  urls=$(printf '%s\n' "$st" | grep -Eo 'https?://[^ ]+' | grep -v '127.0.0.1')
  maps=$(printf '%s\n' "$st" | grep -E '^\|--')
  if [ "$(printf '%s\n' "$urls" | grep -c .)" = 1 ] && ! printf '%s\n' "$urls" | grep -q ':[0-9]*/*$' &&
     [ "$maps" = "|-- / proxy http://127.0.0.1:8105" ]; then echo ours; else echo other; fi
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
mi_foreign && echo "existing muster: none"
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
if ! command -v docker >/dev/null 2>&1; then
  if [ "$OS" = darwin ]; then echo "ASK UP FRONT (hub only): the person installs Docker Desktop (no sudo)"
  else echo "ASK UP FRONT (hub only): sudo to install Docker with the Compose plugin"; fi
fi
if ! command -v tailscale >/dev/null 2>&1; then
  if [ "$OS" = darwin ]; then echo "ASK UP FRONT: the person installs the Tailscale app (App Store or https://tailscale.com/download, no sudo) and signs in with it"
  else echo "ASK UP FRONT: sudo to install Tailscale (https://tailscale.com/download), then to run \`tailscale up\`, which prints a sign-in link the person must open"; fi
elif ! tailscale status >/dev/null 2>&1; then
  if [ "$OS" = darwin ]; then echo "ASK UP FRONT: the person signs in with the Tailscale app (no sudo)"
  else echo "ASK UP FRONT: sudo to run \`tailscale up\`, which prints a sign-in link the person must open"; fi
fi
# `status --json` is indented over many lines: compare it without whitespace.
ts=$(tailscale status --json 2>/dev/null | tr -d '[:space:]')
if ! printf '%s\n' "$ts" | grep -q '"MagicDNSEnabled":true' || ! printf '%s\n' "$ts" | grep -q '"CertDomains":\["'; then
  echo "ASK UP FRONT (hub only): the person turns on MagicDNS and HTTPS Certificates in the Tailscale admin console (https://login.tailscale.com/admin/dns) before you start"
fi
# The operator, only when the prefs show it is not this user (unset, someone
# else, or left out of the JSON). If the prefs cannot be read, nothing is
# asked: Step 2's "access denied" line covers it.
if [ "$OS" = linux ] && [ "$(id -u)" != 0 ]; then
  op=$(tailscale debug prefs 2>/dev/null | tr -d '[:space:]')
  case "$op" in
    *"\"OperatorUser\":\"$USER\""*) ;;
    *'"OperatorUser":'*|'{'*) echo "ASK UP FRONT (hub only, Linux): sudo to run \`tailscale set --operator=\"\$USER\"\`, so Step 2 can run \`tailscale serve\` without sudo" ;;
  esac
fi
if ! command -v qrencode >/dev/null 2>&1; then
  if [ "$OS" = darwin ]; then echo "ASK UP FRONT (optional): the person installs qrencode (\`brew install qrencode\`, no sudo), so \`muster doctor\` can draw the phone's QR code"
  else echo "ASK UP FRONT (optional): sudo to install qrencode, so \`muster doctor\` can draw the phone's QR code"; fi
fi
if [ "$OS" = linux ] && ! command -v loginctl >/dev/null 2>&1; then
  echo "NOTE: no loginctl (WSL, or a container without systemd): muster-mirror cannot linger after logout. On WSL, turn systemd on (\`[boot] systemd=true\` in /etc/wsl.conf, then \`wsl --shutdown\`) and re-run Step 0; elsewhere, Step 7's service runs while the person is logged in"
elif [ "$OS" = linux ] && [ "$(loginctl show-user "$USER" -p Linger --value 2>/dev/null)" != yes ]; then
  echo "ASK UP FRONT (Linux): sudo to run \`loginctl enable-linger \"\$USER\"\`, so muster-mirror keeps running after the person logs out (Step 7)"
fi
unset ts op
```

Read the output and note:

| what | needed on | if it is missing |
|---|---|---|
| `os`, `arch` | every machine | Linux and macOS on amd64 or arm64 only. Stop on anything else and tell the person. |
| `curl`, `tar` | every machine | Install them with the system package manager (ask first). |
| `tmux` | every machine | Install it (`apt install tmux`, `dnf install tmux`, `brew install tmux`; ask first). muster shows tmux windows, so this is required. |
| `tailscale`, logged in | every machine | Asked up front (Step 1, question 4). Install from <https://tailscale.com/download>. Then `tailscale up` (on Linux, `sudo tailscale up`) prints a sign-in URL: give it to the person to open, and wait. Every machine and the phone must sign in to the **same** tailnet. |
| `docker` and `docker compose` | the hub only | Install Docker Engine (Linux) or Docker Desktop (macOS) with the Compose v2 plugin. |
| free disk | the hub | At least 10 GB; 20 GB if dictation will run locally (Step 1). |
| `qrencode` | optional | Lets `muster doctor` draw the phone's QR code. Install it if the person allowed it in Step 1. |
| gpu and `docker gpu runtime` | the hub, for GPU dictation | An NVIDIA GPU with at least 6 GB of memory **and** a Docker GPU runtime (the NVIDIA Container Toolkit), on **amd64**, makes the best dictation option available. |

🔴 **If any `FOREIGN` line printed, STOP and ask the person.** This machine
already runs a muster (or something else on port 8105) that this guide did not
set up. Do not take it over, do not stop it, do not reuse its project or data.
Show them the lines. Go on only if they choose to use this machine as a
joining machine (Step 1, question 1: yes), or after they have dealt with it
themselves.

If `tailscale serve:` is `unready`, Tailscale is not up yet (see the
`tailscale` row above); that is not a conflict.

🔴 **If `tailscale serve:` is `other`**, this machine already serves something
through Tailscale (another site, another port, or Funnel). Step 2 will not
change it; ask the person before the hub goes on this machine.

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
     (`mi_save MUSTER_URL https://…`) and `mi_save ROLE join`. Ask question
     3 as well (it is per machine), then skip to Step 6. Its mirror token is
     minted on the hub in Step 7.

   Other machines are added later by pasting the same prompt into the agent
   on each of them.
2. *(hub only)* **How should voice dictation work?** Offer these, recommending
   the first that fits:
   - **Local, on the GPU — recommended when Step 0 found an NVIDIA GPU with
     6 GB or more, a Docker GPU runtime, and `arch=amd64`** (its image is
     amd64 only). The Cohere transcribe model: the most accurate option, and
     audio never leaves the hub. It needs a Hugging Face access token from an
     account that has accepted the licence of
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

   A token or key the person types into this chat passes through the agent's
   provider. If they prefer, they can write the file themselves in a terminal:
   `read -rs K && (umask 077; printf '%s' "$K" > ~/.config/muster-install/stt-api-key)`
   (or `hf-token`).
3. **May muster type into this machine's terminals?** (`-interactive`, asked
   per machine.) Explain: **yes** lets you send messages to your agents from
   your phone, which is the main point of muster. It also means anyone signed
   in to your muster can type anything into any tmux window on this machine,
   which is a shell as your user: within 12 hours of signing in or confirming
   the password, without being asked again, and after that once they confirm
   the password. **No** keeps the machine read-only: you can watch every
   session but not answer. Recommend **yes** on machines the person owns.
   `mi_save INTERACTIVE 1` (yes) or `mi_save INTERACTIVE 0` (no).
4. **What needs `sudo` or the person's own hands.** List every
   line Step 0 printed starting `ASK UP FRONT` (leave out the `hub only` ones on a
   joining machine), say what each is for, and ask for `sudo` for all of them
   at once. Say plainly:
   - **Tailscale missing or signed out:** installing it and `tailscale up`
     need `sudo`, and `tailscale up` prints a **sign-in link the person must
     open** in a browser. Every machine and the phone sign in to the same
     tailnet.
   - **MagicDNS and HTTPS Certificates** (hub): ask the person to turn both on
     now in the Tailscale admin console, <https://login.tailscale.com/admin/dns>.
     Without them Step 2 cannot give muster an HTTPS address.
   - **`qrencode`** (optional) and **`loginctl enable-linger`** (Linux, Step 7)
     need `sudo` too. Ask in this same message. Record the linger answer:
     `mi_save LINGER yes` or `mi_save LINGER no` (Step 7 skips it on `no`).
   - **macOS:** nothing needs `sudo`. Homebrew packages, the Tailscale app
     and Docker Desktop install as the person; ask them to install what is
     missing and to sign in with the Tailscale app.
   - A `NOTE: no loginctl` line (WSL, a container): tell the person
     muster-mirror runs only while they are logged in, and the WSL fix it names.

   If the agent's shell cannot answer a `sudo` password prompt, give the
   person the exact commands to run in their own terminal instead.

**Then, before Step 2,** do what the person approved, in this order: install
the missing packages; on Linux, `sudo tailscale up --operator="$USER"` (or
`sudo tailscale set --operator="$USER"` if Tailscale is already signed in);
on macOS, the person signs in with the Tailscale app. Give the person the
sign-in link and wait until `tailscale status` succeeds. `enable-linger`
waits for Step 7.

**If this fails:** if the person does not answer a question, use the
recommendation and say so. If they decline `sudo` for something required
(`tmux`, Tailscale, Docker on the hub), stop and say which step needs it.

---

## Step 2: HTTPS through Tailscale (hub)

`tailscale serve` gives the hub a real HTTPS certificate for its tailnet name and
forwards to muster on `127.0.0.1:8105`. Only devices on the tailnet can reach it.
This adds muster's mapping only when nothing is served yet, and changes nothing
else:

```sh
. "$HOME/.config/muster-install/lib.sh"
case "$(mi_serve_state)" in
  none) tailscale serve --bg --https=443 8105 ;;
  ours) echo "tailscale serve already forwards https to muster; kept" ;;
  unready) echo "TAILSCALE NOT READY: \`tailscale serve status\` failed (see If this fails)"
        tailscale serve status ;;
  *)    echo "STOP: tailscale serve already serves something else here (or Funnel is on):"
        tailscale serve status ;;
esac
if [ "$(mi_serve_state)" = ours ]; then
  mi_save MUSTER_URL "$(tailscale serve status 2>/dev/null | grep -Eo 'https://[^ ]+' | head -n 1 | sed 's:/$::')"
  echo "MUSTER_URL=$MUSTER_URL"
fi
```

`MUSTER_URL` must be an `https://…` address ending in the tailnet's domain
(`https://<machine>.<tailnet>.ts.net`). muster is not running yet, so opening it
now fails; that is expected.

**If this fails:**
- `TAILSCALE NOT READY`: Tailscale is not installed, not running, or not
  signed in here. This is not another install: bring it up (Step 0's
  `tailscale` row: install it, start it, `tailscale up`), check
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

---

## Step 3: Download the release (hub)

```sh
. "$HOME/.config/muster-install/lib.sh"
if ! mi_valid_version "${MUSTER_VERSION:-}"; then
  # The newest release: GitHub's /releases/latest redirect (not rate-limited),
  # then its API as a second source. Saved only if it is X.Y.Z.
  v=$(curl -fsSLI -o /dev/null -w '%{url_effective}' https://github.com/ZacxDev/muster/releases/latest | sed -n 's#.*/tag/v\([^/]*\)$#\1#p')
  mi_valid_version "$v" || v=$(curl -fsSL https://api.github.com/repos/ZacxDev/muster/releases/latest | sed -n 's/.*"tag_name": *"v\{0,1\}\([^"]*\)".*/\1/p' | head -n 1)
  mi_set_version "$v" || echo "VERSION LOOKUP FAILED (see If this fails)"
fi
if mi_valid_version "${MUSTER_VERSION:-}"; then
  mi_set_version "$MUSTER_VERSION"
  echo "version=$MUSTER_VERSION"
  mkdir -p "$MUSTER_DIR" && cd "$MUSTER_DIR"
  curl -fsSL -o checksums.txt "$MUSTER_RELEASE_BASE/checksums.txt"
  curl -fsSL -o docker-compose.yml "$MUSTER_RELEASE_BASE/docker-compose.yml"
  curl -fsSL -o env.example "$MUSTER_RELEASE_BASE/env.example"
  mi_verify docker-compose.yml && mi_verify env.example && cp env.example .env.example
fi
```

The release attaches the settings template as `env.example`, without the
leading dot, because GitHub renames asset names that start with one. It is
kept here as `.env.example`.

The checksums prove the files arrived intact. They come from the same place
as the files, so they do not prove who published them.

Then **read `.env.example` in full.** It is the authority on every setting the
compose file uses. The steps below set the ones this guide needs; follow its
comments for anything else.

**If this fails:**
- `VERSION LOOKUP FAILED`: GitHub was unreachable or rate-limited. Nothing was
  saved. Ask the person for the newest version on
  <https://github.com/ZacxDev/muster/releases> (without the `v`), run
  `mi_set_version <it>` (it refuses anything that is not `X.Y.Z`), and re-run
  this block.
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
[ -f .env ] || (umask 077; cp .env.example .env)
chmod 600 .env
env_set MUSTER_VERSION "$MUSTER_VERSION"
env_secret POSTGRES_PASSWORD 24
env_secret MUSTER_UI_PASSWORD 12     # the sign-in password: 24 characters (12 is the minimum)
env_secret MUSTER_HOOK_TOKEN 32      # the machine API's token
env_set MUSTER_PUBLIC_URL "$MUSTER_URL"
env_set MUSTER_SECURE_COOKIES 1
# Web push keys: all three or none. muster refuses to start on a partial set.
if [ -z "$(env_get MUSTER_VAPID_PRIVATE_KEY)" ] || [ -z "$(env_get MUSTER_VAPID_PUBLIC_KEY)" ]; then
  # The keys come on stdout; docker's own errors (a failed pull, a missing
  # image) go to vapid.err and are printed if it fails.
  keys=$(docker run --rm "${MUSTER_IMAGE:-ghcr.io/zacxdev/muster}:$MUSTER_VERSION" vapid generate 2>"$MI_DIR/vapid.err") || keys=""
  pub=$(printf '%s\n' "$keys" | sed -n 's/^MUSTER_VAPID_PUBLIC_KEY=//p')
  priv=$(printf '%s\n' "$keys" | sed -n 's/^MUSTER_VAPID_PRIVATE_KEY=//p')
  if [ -n "$pub" ] && [ -n "$priv" ]; then
    env_set MUSTER_VAPID_PUBLIC_KEY "$pub"; env_set MUSTER_VAPID_PRIVATE_KEY "$priv"
    env_set MUSTER_VAPID_SUBJECT "$MUSTER_URL"
    rm -f "$MI_DIR/vapid.err"
  else
    echo "PUSH KEYS NOT GENERATED: none of the three were written (see If this fails). docker said:"
    sed 's/^/  /' "$MI_DIR/vapid.err"
  fi
  unset keys pub priv
fi
grep -c '^[A-Z_]*=CHANGE-ME$' .env | sed 's/^/placeholders left: /'
```

`placeholders left` must be `0` (the commented-out dictation blocks do not
count, because they start with `#`). Then dictation, by the answer saved in
Step 1. This block is also how to **switch** options later: `mi_save STT <new>`
(and, for `api`, write the key file first), then re-run it and Step 5. Each
option sets the profile, URL and model together, and never writes an empty
value over a token or key that is already there.

```sh
. "$HOME/.config/muster-install/lib.sh"
cd "$MUSTER_DIR"
prev=$(env_get COMPOSE_PROFILES); url=$(env_get MUSTER_STT_URL)
# Is the API option in effect now? (No profile, and a URL that is not a local service.)
was_api=0
case "$url" in ""|http://stt-*) ;; *) [ -n "$prev" ] || was_api=1 ;; esac
# Leaving the API option: .env holds the key's only copy, so keep it aside.
if [ "$was_api" = 1 ] && [ "$STT" != api ] && [ ! -s "$MI_DIR/stt-api-key" ]; then
  (umask 077; env_get MUSTER_STT_TOKEN > "$MI_DIR/stt-api-key")
fi
case "$STT" in
  local-gpu)
    [ "$prev" = stt-cohere ] || env_set MUSTER_STT_TOKEN "$(mi_rand 32)"
    env_set COMPOSE_PROFILES stt-cohere
    env_set MUSTER_STT_URL http://stt-cohere:8000
    env_set MUSTER_STT_MODEL ""
    if [ -s "$MI_DIR/hf-token" ]; then env_set HF_TOKEN "$(cat "$MI_DIR/hf-token")" && rm -f "$MI_DIR/hf-token"; fi
    case "$(env_get HF_TOKEN)" in ""|CHANGE-ME) echo "MISSING: the Hugging Face token (Step 1, question 2)" ;; esac ;;
  local-cpu)
    [ "$prev" = stt-whisper ] || env_set MUSTER_STT_TOKEN "$(mi_rand 32)"
    env_set COMPOSE_PROFILES stt-whisper
    env_set MUSTER_STT_URL http://stt-whisper:8000
    # A faster-whisper model is kept (someone chose a larger one); anything else is reset.
    case "$(env_get MUSTER_STT_MODEL)" in Systran/faster-whisper-*) ;; *) env_set MUSTER_STT_MODEL Systran/faster-whisper-small ;; esac ;;
  api)
    api_url=${STT_API_URL:-}; api_model=${STT_API_MODEL:-}
    if [ "$was_api" = 1 ]; then api_url=${api_url:-$url}; api_model=${api_model:-$(env_get MUSTER_STT_MODEL)}; fi
    key=0; [ -s "$MI_DIR/stt-api-key" ] && key=1
    [ "$was_api" = 1 ] && [ -n "$(env_get MUSTER_STT_TOKEN)" ] && key=1
    if [ -z "$api_url" ]; then echo "MISSING: the API's URL (mi_save STT_API_URL https://…); nothing changed"
    elif [ -z "$api_model" ]; then echo "MISSING: the API's model (mi_save STT_API_MODEL …); nothing changed"
    elif [ "$key" = 0 ]; then echo "MISSING: the API key (write it as Step 1, question 2 shows); nothing changed"
    else
      env_set COMPOSE_PROFILES ""
      env_set MUSTER_STT_URL "$api_url"
      env_set MUSTER_STT_MODEL "$api_model"
      if [ -s "$MI_DIR/stt-api-key" ]; then env_set MUSTER_STT_TOKEN "$(cat "$MI_DIR/stt-api-key")" && rm -f "$MI_DIR/stt-api-key"; fi
    fi ;;
  none)
    env_set COMPOSE_PROFILES ""; env_set MUSTER_STT_URL ""; env_set MUSTER_STT_TOKEN ""; env_set MUSTER_STT_MODEL "" ;;
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
- `PUSH KEYS NOT GENERATED`: `docker run … vapid generate` failed, so no push
  keys were written (muster starts without notifications). Read the error
  printed under it (kept in `~/.config/muster-install/vapid.err`): usually
  the network, Docker not running, or a version that does not exist (check
  Step 3). Fix that, then re-run this block, which re-runs the `docker run`.
- `MISSING: …`: write the token or key file as Step 1 shows, then re-run the
  dictation block. Nothing already in `.env` was replaced.
- To start over, see "Starting over" at the end of this guide. Never run
  `docker compose down -v` yourself: it deletes a database.

---

## Step 5: Start the server (hub)

```sh
. "$HOME/.config/muster-install/lib.sh"
cd "$MUSTER_DIR"
proj=$(docker compose config 2>/dev/null | sed -n 's/^name: *//p' | head -n 1)
if mi_foreign "${proj:-muster}"; then
  mi_save GUIDE_STARTED 1
  env_set MUSTER_VERSION "$MUSTER_VERSION"
  docker compose pull
  docker compose up -d --wait --wait-timeout 600
  docker compose ps
  # The first healthy start marks this install as IN USE: from here on it
  # may hold data, and "Starting over" refuses to delete it.
  curl -fsS http://127.0.0.1:8105/health && echo && mi_save GUIDE_IN_USE 1
  curl -fsS "$MUSTER_URL/health" && echo
else
  echo "STOP: another muster (or another service on 8105) is here; nothing was started. Ask the person."
fi
```

Both `curl`s must print `{"status":"ok",…}`, with this release's `version`. The
first proves muster is up, the second that Tailscale's HTTPS reaches it.

A local dictation service downloads its model on first start. Whisper takes a
few minutes, and the GPU (Cohere) model can take up to 30. If `up --wait`
times out while only the `stt-…` service is still `starting`, that is the
download: wait a few minutes and re-run this block until it is `healthy`.

**If this fails:**
- `STOP`: see Step 0. Do not take over or stop the other install.
- "port is already allocated" for 8105: something else took the port after
  this guide's install started. Find it with `ss -ltnp 'sport = :8105'`
  (Linux) or `lsof -iTCP:8105 -sTCP:LISTEN` (macOS) and tell the person; do
  not stop it yourself.
- `up --wait` fails or `muster` is unhealthy: `docker compose logs --tail=80
  muster`. muster refuses to start on a bad setting and names the variable in
  that log (a `MUSTER_UI_PASSWORD` under 12 characters is the usual one). Fix it
  in `.env` and re-run this block.
- The second `curl` fails but the first works: re-run Step 2, and check
  `tailscale serve status` forwards to `http://127.0.0.1:8105`.
- A pull fails with **`denied`**, **`unauthorized`** or **`not found`** for
  an image: that image is not published, or not public, yet. It is not a
  licence or GPU problem. Tell the person which image. For `stt-cohere`, offer
  the CPU or API option for now (`mi_save STT local-cpu`, then Step 4's
  dictation block, then this block).
- The `stt-cohere` service starts but fails: `docker compose logs --tail=80
  stt-cohere`. If the model download is refused, the Hugging Face account has
  not accepted the model's licence. If no GPU is found, the Docker GPU runtime
  is missing. Either way, offer the CPU or API option as above.

---

## Step 6: Install the binaries (every machine)

`muster-mirror` shows this machine's tmux sessions in muster. `muster` is the
command-line client, used here for `muster doctor`. Both are checksum-verified.

On a **joining** machine, first take the version from the hub:

```sh
. "$HOME/.config/muster-install/lib.sh"
v=$(curl -fsS "$MUSTER_URL/health" | sed -n 's/.*"version": *"\([^"]*\)".*/\1/p')
mi_set_version "$v" && echo "version=$MUSTER_VERSION" || echo "COULD NOT READ THE HUB'S VERSION (see If this fails)"
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
# Does a NEW login shell find it? (This shell's PATH may predate
# $MUSTER_BIN existing; many profiles add ~/.local/bin only once it exists.)
# `env -i`: the login shell builds PATH from the profile alone, not from ours.
[ "$(env -i HOME="$HOME" USER="$USER" "${SHELL:-sh}" -lc 'command -v muster' 2>/dev/null)" = "$MUSTER_BIN/muster" ] ||
  echo "NOTE: a new login shell does not find $MUSTER_BIN/muster: add $MUSTER_BIN to PATH in the shell profile"
```

**If this fails:**
- `COULD NOT READ THE HUB'S VERSION`: nothing was saved. Check this machine is
  on the tailnet (`tailscale status`) and the hub's URL is right
  (`curl -fsS "$MUSTER_URL/health"`), then re-run that block.
- A `FAILED` line or a checksum `FAILED`: re-run once (a partial download).
  A second mismatch: stop and tell the person; do not install that file.
- `muster-mirror -version` says "cannot execute": the wrong `OS`/`ARCH` was
  detected; check Step 0's output.
- `NOTE: a new login shell does not find …`: add
  `export PATH="$HOME/.local/bin:$PATH"` to the person's shell profile (ask
  first), or use the full path below. No `NOTE` means a new terminal finds
  `muster` already; change nothing.

---

## Step 7: Connect this machine's sessions (every machine)

Each machine has its own **mirror token**, minted on the hub. It lets that
machine's `muster-mirror` connect, and nothing else.

**On the hub**, mint the hub's own token. A token is shown once, so it goes
straight into a file. An existing token is kept:

```sh
. "$HOME/.config/muster-install/lib.sh"
cd "$MUSTER_DIR"
mkdir -p "$HOME/.config/muster-mirror" && chmod 700 "$HOME/.config/muster-mirror"
tok="$HOME/.config/muster-mirror/token"
if [ -s "$tok" ]; then echo "kept existing token for $MIRROR_HOST"
else
  (umask 077; docker compose exec -T muster /muster-server mirror token create "$MIRROR_HOST" > "$tok.tmp") &&
    [ -s "$tok.tmp" ] && mv "$tok.tmp" "$tok" && echo "minted token for $MIRROR_HOST"
fi
docker compose exec -T muster /muster-server mirror token ls
```

**On a joining machine**, skip this if `~/.config/muster-mirror/token` already
holds a token. Otherwise the token is minted on the hub, for this machine's
name (`echo "$MIRROR_HOST"`), when this machine joins. Ask the person which
way:

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
t=$(docker compose -f "$HOME/muster/docker-compose.yml" exec -T muster /muster-server mirror token create "$1") &&
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
machine's name filled in, and ask them to paste what it prints:
`docker compose -f ~/muster/docker-compose.yml exec -T muster /muster-server mirror token create <this machine's name>`.
Write the pasted token without echoing it:
`(umask 077; mkdir -p ~/.config/muster-mirror; printf '%s' 'THE-TOKEN' > ~/.config/muster-mirror/token)`.

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
  # Keep it running after the person logs out. On Debian and most other
  # distributions this always needs sudo (asked in Step 1). `sudo -n` never
  # waits for a password the agent cannot type: it fails, and the person runs it.
  if [ "${LINGER:-}" = no ]; then echo "linger: skipped (declined in Step 1); muster-mirror stops when the person logs out"
  elif ! command -v loginctl >/dev/null 2>&1; then echo "linger: no loginctl here (WSL or a container); see Step 0's NOTE"
  elif [ "$(loginctl show-user "$USER" -p Linger --value 2>/dev/null)" = yes ]; then echo "linger: already on"
  else sudo -n loginctl enable-linger "$USER" && echo "linger: on" ||
    echo "LINGER NOT ON: ask the person to run, in their own terminal: sudo loginctl enable-linger $USER"
  fi
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
must show it connected, with no repeated errors. On Linux, `linger: on` (or
`already on`) means it keeps running after the person logs out.

One log line is expected and is not an error:
`opencode: not paired on this host …`. It matters only to people who run
opencode on this machine; they pair it once with `muster-mirror opencode-pair`
(see `docs/mirroring.md`, "opencode's token") so muster can show opencode
sessions as chats.

What `-interactive` (`MUSTER_MIRROR_INTERACTIVE=1`) grants: muster can open a
**typing** terminal on this machine and paste messages into its tmux panes,
for 12 hours after the person signs in or confirms the password, and again
after they confirm it once more. A tmux client can switch to any
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
  `sudo loginctl enable-linger "$USER"` (approved in Step 1), log in again, and
  re-run.
- It runs but no sessions appear: tmux uses a different socket for the service
  than for the person. If the person sets `TMUX_TMPDIR`, add the same line to
  `~/.config/muster-mirror/env` and restart.

---

## Step 8: Run your agents in tmux (tell the person)

muster shows **tmux windows**. Any coding agent started inside tmux on a
connected machine appears as a card, and a Claude Code window opens as a chat.
These are **mirrored sessions**: the person's own sessions. A message from
muster is typed into that same session, so it is all there when they are back
at the keyboard. This guide sets up nothing else: it starts no agent of its
own, and dispatched agents (muster starting new sessions in Kubernetes) stay
off.

A session the person already has open **outside** tmux is invisible to muster,
and starting a second one in tmux makes a separate conversation. To bring the
one they have into tmux, give them this link: <https://github.com/ZacxDev/muster/blob/main/docs/mirroring.md#bring-a-session-you-already-have-into-tmux>.

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
"$MUSTER_BIN/muster" doctor --deploy-env "$MUSTER_DIR/.env" >/dev/null
```

`--deploy-env` makes the doctor check the hub that `.env` names, whatever
`~/.muster/muster.env` or the environment point at, and it sends the password
to that hub only.

On **every other machine**, run:

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
`>/dev/null` above drops that copy, so you read the report once. Its checks:

| check | what it proves |
|---|---|
| `server` | the address answers over HTTPS |
| `manifest` | the phone can install muster |
| `hook-token` | the machine API has a token, and this one is accepted |
| `mirror-hosts` | every machine with a token is connected (the server's view) |
| `this-host` | the hub accepts this machine's mirror token and holds its connection |
| `mirror-service` | this machine's `muster-mirror` service is running |
| `tmux` | tmux is installed here |
| `sign-in` | the password signs in and opens the board |
| `push` | notification keys are set |
| `dictation` | a bundled voice sample is transcribed through muster |

Work through each `FAIL` with its `fix:` line and the matching step above, then
run the doctor again. **Done means the hub's doctor exits 0**, every machine's
doctor shows `this-host`, `mirror-service` and `tmux` passing, and the person confirms on
the phone that they can see a session and that the test notification arrived.

Then tell the person, briefly:

- the address, and where the password is (step 9.3);
- **muster shows their own sessions.** Each card is a session running in tmux
  on one of their machines (a mirrored session). A message from muster goes
  into that same session, so it is all there when they are back at the
  keyboard. muster starts no session of its own;
- **start Claude Code inside tmux**, with the one-liners from Step 8. A
  session outside tmux does not appear. To bring one they already have:
  <https://github.com/ZacxDev/muster/blob/main/docs/mirroring.md#bring-a-session-you-already-have-into-tmux>;
- **a Claude Code session opens in the Rendered view**, a chat. The Terminal
  view is one tap away in the header, as the fallback. If a window opens in the
  Terminal view only, the line at its top says why. On a Mac, Claude Code
  windows open in the Terminal view only, for now;
- that `muster doctor` re-checks everything.

---

## Updating and removing

- **Update** (on the hub, then each machine): set the new version, which
  `.env` and every download follow:
  ```sh
  . "$HOME/.config/muster-install/lib.sh"
  mi_set_version X.Y.Z
  ```
  Then on the hub re-run Step 3 and Step 5 (Step 5 writes the version into
  `.env`, pulls the new image and restarts the server), and on every machine
  re-run that `mi_set_version`, Step 6, and Step 7's last block (which
  restarts muster-mirror). Re-run Step 10 to check.
- **Remove a machine:** stop its service (`systemctl --user disable --now
  muster-mirror.service`, or `launchctl bootout gui/$(id -u)/io.github.zacxdev.muster-mirror`),
  and revoke its token on the hub (`docker compose exec -T muster /muster-server
  mirror token ls`, then `… revoke <id>`).
- **Remove muster's HTTPS mapping**, and only it: `tailscale serve --https=443
  off` when `mi_serve_state` says `ours`. Never `tailscale serve reset`: it
  removes everything else this machine serves too.
- **Remove everything** (ask first; it deletes all tasks and history): on the
  hub, `cd ~/muster && docker compose down -v`, remove muster's HTTPS mapping
  as above, then remove `~/muster`, `~/.config/muster-mirror`,
  `~/.config/muster-install` and `~/.muster`.

### Starting over (a failed first install only)

Only for this guide's **own fresh** install: Step 5 started it, and muster has
**never** answered healthy (once it has, it may hold data, and the block
refuses). It also refuses any install this guide did not start, and prints
exactly what would be deleted:

```sh
. "$HOME/.config/muster-install/lib.sh"
cd "$MUSTER_DIR" || exit 1
proj=$(docker compose config 2>/dev/null | sed -n 's/^name: *//p' | head -n 1)
if [ "${GUIDE_STARTED:-}" != 1 ] || [ "${GUIDE_IN_USE:-}" = 1 ] || ! mi_foreign "${proj:-muster}"; then
  echo "REFUSED: this is not this guide's own fresh install. Delete nothing; ask the person."
else
  echo "This would DELETE, in compose project ${proj:-muster} ($MUSTER_DIR):"
  docker ps -a --filter "label=com.docker.compose.project=${proj:-muster}" --format '  container {{.Names}}'
  docker volume ls --filter "label=com.docker.compose.project=${proj:-muster}" --format '  volume {{.Name}}'
  [ "${MI_CONFIRM_DELETE:-}" = "${proj:-muster}" ] && docker compose down -v && mi_save GUIDE_STARTED 0 ||
    echo "Nothing deleted. Show the person this list; only if they agree, re-run with MI_CONFIRM_DELETE=${proj:-muster}."
fi
```

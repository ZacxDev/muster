#!/bin/sh
# capabilities: join adopt
# muster on this machine, in one line:
#
#   curl -fsSL https://github.com/ZacxDev/muster/releases/latest/download/install.sh | sh
#
# What it does, every step safe to run again:
#   1. checks Docker (with Compose v2) is here and usable;
#   2. refuses, changing nothing, if a muster it did not make is already here
#      (another compose project, the directory, the port, this machine's
#      muster-mirror settings);
#   3. downloads the release's docker-compose.yml into ~/muster, checks it
#      against the release's checksums.txt, and starts the stack with
#      `docker compose up -d --wait` (no .env: muster-init generates the
#      secrets, docs/quickstart-local.md);
#   4. installs muster-mirror and the muster CLI (checksum-verified) into
#      ~/.local/bin, mints this machine's mirror token, and runs muster-mirror
#      as a user service (systemd on Linux, launchd on macOS) against
#      http://localhost:<port>; on Linux it turns on linger for this user
#      (`loginctl enable-linger`, asking before it uses sudo) so the mirror
#      survives logout and reboot, and says plainly when it could not;
#   5. runs `muster doctor`, and exits with its verdict.
#
# JOINING ANOTHER MACHINE'S muster (MUSTER_JOIN_URL=https://<hub>): 2's
# stack checks and 3 above are skipped (no Docker needed); 4 installs the
# binaries of the release the hub runs, uses the mirror token already in
# ~/.config/muster-mirror/token (minted on the hub:
# INSTALL.md, "Another machine"), and points muster-mirror at the hub; 5's
# doctor checks the hub from here.
#
# The mirror token never goes on a command line (other users can read argv in
# `ps`): curl reads its Authorization header from a 0600 file (`-H @file`).
#
# Facts it does not keep: the version is the release this file came from (a
# release stamps it; an unstamped copy asks GitHub for the newest release),
# and the port is whatever that release's docker-compose.yml publishes.
#
# Settings (environment): MUSTER_DIR (default ~/muster), MUSTER_PROJECT
# (compose project, default muster), MUSTER_BIN (default ~/.local/bin),
# MUSTER_MIRROR_HOST (default this machine's short hostname), MUSTER_LINGER
# (yes/no: answer the linger question without asking), MUSTER_JOIN_URL (join
# that hub instead of starting a server here), MUSTER_MIRROR_INTERACTIVE (0/1:
# write it into muster-mirror's settings; unset keeps what is there),
# MUSTER_MIRROR_ADOPT=1 (take over muster-mirror settings this script did not
# write, keeping every line it does not own), MUSTER_PRINT_PASSWORD=no (print
# the command that shows the sign-in password, not the password: for an agent,
# whose output is read by its provider). For testing only: MUSTER_RELEASE_BASE
# (where the assets are), MUSTER_IMAGE (a local image).
#
# The capabilities line at the top names the settings above a guide may rely
# on (join: MUSTER_JOIN_URL; adopt: MUSTER_MIRROR_ADOPT): INSTALL.md's
# "Install muster on this machine" reads it, without running this file, and
# refuses an installer without them.
set -eu

# 🔴 EVERYTHING IS IN main(), CALLED ON THE LAST LINE. Under `curl … | sh` the
# shell reads the script as it runs it; a function body is read whole before
# any of it runs, so a command that reads stdin, or a download cut short,
# cannot run half a script.
main() {
RELEASE="@MUSTER_RELEASE@"            # scripts/release-build.sh stamps X.Y.Z here
REPO="https://github.com/ZacxDev/muster"
DIR="${MUSTER_DIR:-$HOME/muster}"
PROJECT="${MUSTER_PROJECT:-muster}"
BIN="${MUSTER_BIN:-$HOME/.local/bin}"
CFG="$HOME/.config/muster-mirror"
MARK="# written by muster's install.sh; edit freely, lines it does not own are kept"
PHONE_DOC="$REPO/blob/main/docs/quickstart-local.md#add-your-phone"
JOIN="${MUSTER_JOIN_URL:-}"; JOIN="${JOIN%/}"
case "${MUSTER_MIRROR_INTERACTIVE:-}" in ""|0|1) ;; *) die "MUSTER_MIRROR_INTERACTIVE must be 0 or 1" ;; esac

# --- 1. preflight -------------------------------------------------------------
for t in curl tar uname sed grep; do have "$t" || die "this needs $t; install it and run again"; done
if [ -n "$JOIN" ]; then
  case "$JOIN" in
    https://*|http://localhost:*|http://127.0.0.1:*|http://\[::1\]:*) ;;
    *) die "MUSTER_JOIN_URL must be the hub's https:// address (muster-mirror sends its token over https only)" ;;
  esac
else
have docker || die "Docker is not installed. Install Docker Engine (Linux) or Docker Desktop (macOS): https://docs.docker.com/engine/install/"
docker compose version >/dev/null 2>&1 || die "Docker Compose v2 is missing (\`docker compose version\` failed): install the docker-compose-plugin package"
docker info >/dev/null 2>&1 || die "Docker is installed but this user cannot use it. Start Docker, or on Linux add yourself to the docker group (sudo usermod -aG docker \"\$USER\", then log in again)"
fi
if have sha256sum; then sha() { sha256sum "$@"; }; elif have shasum; then sha() { shasum -a 256 "$@"; }; else die "this needs sha256sum or shasum"; fi
case "$(uname -s)" in Linux) OS=linux ;; Darwin) OS=darwin ;; *) die "unsupported OS $(uname -s): Linux and macOS only" ;; esac
case "$(uname -m)" in x86_64|amd64) ARCH=amd64 ;; aarch64|arm64) ARCH=arm64 ;; *) die "unsupported CPU $(uname -m)" ;; esac
have tmux || say "note: tmux is not installed; muster shows tmux windows, so install it to see your sessions"

if [ -n "$JOIN" ]; then
  # The hub's own release, so this machine's muster-mirror matches its server.
  v=$(curl -fsS -m 10 "$JOIN/health" 2>/dev/null | sed -n 's/.*"version": *"\([0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\)".*/\1/p')
  [ -n "$v" ] || die "$JOIN/health did not answer with muster's version: is this machine on the tailnet (\`tailscale status\`), and is the address right?"
  RELEASE="$v"
fi
case "$RELEASE" in
  @*) # Not a release's copy: the newest release (GitHub's redirect).
    RELEASE=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$REPO/releases/latest" | sed -n 's#.*/tag/v\([0-9][0-9.]*\)$#\1#p')
    [ -n "$RELEASE" ] || die "could not find the newest release on $REPO/releases; download install.sh from a release instead" ;;
esac
BASE="${MUSTER_RELEASE_BASE:-$REPO/releases/download/v$RELEASE}"
# </dev/null: under `curl … | sh` stdin IS this script, and `docker compose
# exec` would read (and eat) the rest of it.
# The person's docker-compose.override.yml beside it, when there is one: an
# explicit -f turns Compose's automatic override off, and a re-run that
# dropped it would drop what it adds (accounts mode's browser port).
dc() {
  if [ -e "$DIR/docker-compose.override.yml" ]; then
    docker compose -f "$DIR/docker-compose.yml" -f "$DIR/docker-compose.override.yml" -p "$PROJECT" "$@" </dev/null
  else
    docker compose -f "$DIR/docker-compose.yml" -p "$PROJECT" "$@" </dev/null
  fi
}
# muster-init prints the generated sign-in password once, on a line carrying
# this marker (cmd/muster-server/secretfiles.go, secretLineMarker): every log
# this script shows drops it, so the password never reaches an agent's output.
SECRET_MARK='[muster-secret]'

# --- 2. refuse a muster this script did not make ------------------------------
if [ -z "$JOIN" ]; then
if [ -e "$DIR/docker-compose.yml" ] && [ ! -e "$DIR/.muster-install" ]; then
  die "$DIR already holds a muster this script did not install (an earlier INSTALL.md's, or your own). Nothing was changed. Update it the way it was installed, or install beside it: MUSTER_DIR=<dir> MUSTER_PROJECT=<name>"
fi
others=$(docker ps -a --filter "label=com.docker.compose.project=$PROJECT" --format '{{.Label "com.docker.compose.project.working_dir"}}' | sort -u | grep -vx "$DIR" || true)
[ -z "$others" ] || die "a compose project named $PROJECT already runs from $(echo "$others" | head -n 1), not $DIR. Nothing was changed. Pick another name: MUSTER_PROJECT=<name>"
fi
if [ -e "$CFG/env" ] && ! grep -qxF "$MARK" "$CFG/env" && [ "${MUSTER_MIRROR_ADOPT:-}" != 1 ]; then
  die "this machine's muster-mirror is already set up ($CFG/env, not written by this script: an earlier INSTALL.md's, or your own). Nothing was changed. To keep its extra lines and let this script manage the rest, run again with MUSTER_MIRROR_ADOPT=1."
fi

# --- 3. the stack -----------------------------------------------------------------
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM
fetch() { curl -fsSL -o "$tmp/$1" "$BASE/$1" || die "could not download $BASE/$1"; }
verify() { (cd "$tmp" && grep "  $1\$" checksums.txt | sha -c - >/dev/null 2>&1) || die "$1 does not match the release's checksums.txt; nothing was installed"; }
fetch checksums.txt
if [ -n "$JOIN" ]; then
  URL="$JOIN"
else
fetch docker-compose.yml; verify docker-compose.yml
port=$(docker compose -f "$tmp/docker-compose.yml" -p "$PROJECT" config 2>/dev/null | sed -n 's/^ *published: "\{0,1\}\([0-9][0-9]*\)"\{0,1\}$/\1/p' | head -n 1)
[ -n "$port" ] || die "the release's docker-compose.yml publishes no port"
URL="http://localhost:$port"
if [ -z "$(docker ps -a -q --filter "label=com.docker.compose.project=$PROJECT")" ] &&
   curl -fsS -m 3 -o /dev/null "http://127.0.0.1:$port/health" 2>/dev/null; then
  die "something already answers on 127.0.0.1:$port, and it is not this install. Nothing was changed."
fi
mkdir -p "$DIR"
cp "$tmp/docker-compose.yml" "$DIR/docker-compose.yml"
printf 'installed-by: install.sh\nrelease: %s\n' "$RELEASE" > "$DIR/.muster-install"
say "starting muster $RELEASE in $DIR (compose project $PROJECT)"
# --quiet-pull: the image downloads' progress bars are hundreds of KB that
# bury this script's report (onboarding round 3); a failed pull still says why.
if ! dc up -d --wait --quiet-pull --wait-timeout 300; then
  { dc logs --no-color --tail 15 muster-init muster 2>&1 || true; } | grep -vF "$SECRET_MARK" >&2 || true
  die "the stack did not come up; the reason is in the lines above (\`docker compose -f $DIR/docker-compose.yml -p $PROJECT logs muster-init muster | grep -vF '$SECRET_MARK'\`)"
fi
curl -fsS -m 5 -o /dev/null "$URL/readyz" || die "muster is up but $URL/readyz does not answer"
fi

# --- 4. this machine: muster-mirror and the CLI -----------------------------------
mkdir -p "$BIN"
for name in muster-mirror muster; do
  asset="${name}_${OS}_${ARCH}.tar.gz"
  fetch "$asset"; verify "$asset"
  mkdir -p "$tmp/x-$name" && tar -xzf "$tmp/$asset" -C "$tmp/x-$name" "$name" || die "$asset does not hold $name"
  cp "$tmp/x-$name/$name" "$BIN/$name.new" && chmod 755 "$BIN/$name.new" && mv "$BIN/$name.new" "$BIN/$name"
done
mkdir -p "$CFG" && chmod 700 "$CFG"
host="${MUSTER_MIRROR_HOST:-$(hostname 2>/dev/null | cut -d. -f1)}"
[ -n "$host" ] || host=this-machine
tok="$CFG/token"
# The token's Authorization header, in a 0600 file curl reads (`-H @file`):
# never in curl's argv, which every user of this machine can read in `ps`.
auth="$tmp/auth-header"
authfile() { (umask 077; printf 'Authorization: Bearer %s\n' "$(cat "$tok")" > "$auth"); }
# A kept token must still be live (a `down -v` starts a new database).
[ -s "$tok" ] && authfile
if [ -s "$tok" ] && [ "$(curl -s -o /dev/null -w '%{http_code}' -H @"$auth" "$URL/mirror/host")" = 401 ]; then
  [ -z "$JOIN" ] || die "$URL refused this machine's mirror token ($tok): mint a new one on the hub for host $host (INSTALL.md), write it there, and run this again"
  say "this machine's mirror token is not known to this server any more; minting a new one"
  rm -f "$tok"
fi
if [ ! -s "$tok" ] && [ -n "$JOIN" ]; then
  die "no mirror token in $tok. On the hub, mint one for this machine: docker compose -f ~/muster/docker-compose.yml exec -T muster /muster-server mirror token create $host; write it to $tok (mode 600) and run this again"
fi
if [ ! -s "$tok" ]; then
  (umask 077; dc exec -T muster /muster-server mirror token create "$host" > "$tok.tmp") && [ -s "$tok.tmp" ] && mv "$tok.tmp" "$tok" ||
    die "could not mint a mirror token for $host"
  say "minted this machine's mirror token ($host)"
fi
chmod 600 "$tok"
authfile
# The settings: the lines this script owns are rewritten, every other line kept.
# MUSTER_MIRROR_INTERACTIVE is owned only when it is given; unset, the line
# already there (or its absence: read-only) is kept.
owned='MUSTER_MIRROR_URL|MUSTER_MIRROR_TOKEN_FILE|MUSTER_MIRROR_HOST|MUSTER_MIRROR_WRITE_HOOK_CONFIG|PATH'
[ -z "${MUSTER_MIRROR_INTERACTIVE:-}" ] || owned="$owned|MUSTER_MIRROR_INTERACTIVE"
tmux_dir=$(dirname "$(command -v tmux 2>/dev/null || echo /usr/bin/tmux)")
{
  echo "$MARK"
  # An owned key in any spelling a shell or systemd reads is owned: indented,
  # or after `export`.
  [ -e "$CFG/env" ] && grep -vxF "$MARK" "$CFG/env" | grep -Ev "^[[:space:]]*(export[[:space:]]+)?($owned)=" || true
  echo "MUSTER_MIRROR_URL=$URL"
  echo "MUSTER_MIRROR_TOKEN_FILE=$tok"
  echo "MUSTER_MIRROR_HOST=$host"
  echo "MUSTER_MIRROR_WRITE_HOOK_CONFIG=1"
  [ -z "${MUSTER_MIRROR_INTERACTIVE:-}" ] || echo "MUSTER_MIRROR_INTERACTIVE=$MUSTER_MIRROR_INTERACTIVE"
  echo "PATH=$tmux_dir:$BIN:/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin"
} > "$tmp/mirror-env"
(umask 077; cp "$tmp/mirror-env" "$CFG/env")

service=none
if [ "$OS" = linux ] && systemctl --user show-environment >/dev/null 2>&1; then
  mkdir -p "$HOME/.config/systemd/user"
  cat > "$HOME/.config/systemd/user/muster-mirror.service" <<UNIT
[Unit]
Description=muster-mirror: show this machine's tmux sessions in muster
After=network-online.target

[Service]
EnvironmentFile=$CFG/env
ExecStart=$BIN/muster-mirror
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
UNIT
  systemctl --user daemon-reload
  systemctl --user enable muster-mirror.service >/dev/null 2>&1
  # mirror_reason reads the journal from here on: an earlier run's reason is not this one's.
  since=$(date '+%Y-%m-%d %H:%M:%S')
  systemctl --user restart muster-mirror.service
  restarts0=$(systemctl --user show muster-mirror.service -p NRestarts --value 2>/dev/null || echo 0)
  case "$restarts0" in ''|*[!0-9]*) restarts0=0 ;; esac
  service=systemd
  linger
elif [ "$OS" = darwin ]; then
  label=io.github.zacxdev.muster-mirror
  plist="$HOME/Library/LaunchAgents/$label.plist"
  mkdir -p "$HOME/Library/LaunchAgents" "$HOME/Library/Logs"
  {
    echo '<?xml version="1.0" encoding="UTF-8"?>'
    echo '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">'
    echo '<plist version="1.0"><dict>'
    echo "<key>Label</key><string>$label</string>"
    echo "<key>ProgramArguments</key><array><string>$BIN/muster-mirror</string></array>"
    echo '<key>EnvironmentVariables</key><dict>'
    grep -E '^[A-Z_]+=' "$CFG/env" | while IFS='=' read -r k v; do echo "<key>$k</key><string>$v</string>"; done
    echo '</dict>'
    echo '<key>RunAtLoad</key><true/><key>KeepAlive</key><true/>'
    echo "<key>StandardErrorPath</key><string>$HOME/Library/Logs/muster-mirror.log</string>"
    echo '</dict></plist>'
  } > "$plist"
  launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
  launchctl bootstrap "gui/$(id -u)" "$plist"
  service=launchd
else
  say "no systemd user session here (a container, or WSL without systemd): muster-mirror is installed but not started."
  say "run it yourself, e.g. in tmux: set -a; . $CFG/env; set +a; $BIN/muster-mirror"
fi

# Wait for this machine to connect before the doctor looks. A muster-mirror
# that exits at once (a bad setting, a refused URL) is restarted every few
# seconds and reads `activating`, never `failed`: say so, with ITS reason line,
# instead of waiting the whole 30s.
if [ "$service" != none ]; then
  i=0
  until curl -fsS -H @"$auth" "$URL/mirror/host" 2>/dev/null | grep -q '"online": *true'; do
    if crashloop; then
      say "muster-mirror is not running: it exits as it starts and is restarted (it shows as \`activating\`). Its reason:"
      say "    $(mirror_reason)"
      break
    fi
    i=$((i + 1)); [ "$i" -le 30 ] || { say "muster-mirror has not connected after 30s; the doctor below says why"; break; }
    sleep 1
  done
fi

# --- 5. the doctor -------------------------------------------------------------------
# Dictation and the other optional features are not skipped here: the doctor
# reports a feature the server has off as SKIP, never as a failure.
skip=""
[ "$service" = none ] && skip="mirror-service,this-host,mirror-hosts"
rc=0
if [ -n "$JOIN" ]; then
  # No server credential on a joining machine: the server-side checks SKIP,
  # and this-host asks the hub, with this machine's own token, if it is seen.
  "$BIN/muster" doctor --api-url "$URL" --mirror-env "$CFG/env" ${skip:+--skip "$skip"} >/dev/null || rc=$?
  echo
  say "this machine's sessions now show in muster at $URL"
  [ "$rc" = 0 ] || say "the doctor found a problem (above); fix it and run this again — it is safe to"
  exit "$rc"
fi
pw=$(dc exec -T muster /muster-server show-secret ui-password)
deploy="$tmp/doctor.env"
(umask 077; {
  echo "MUSTER_PUBLIC_URL=$URL"
  echo "MUSTER_HOOK_TOKEN=$(dc exec -T muster /muster-server show-secret hook-token)"
  echo "MUSTER_UI_PASSWORD=$pw"
} > "$deploy")
"$BIN/muster" doctor --deploy-env "$deploy" --mirror-env "$CFG/env" ${skip:+--skip "$skip"} >/dev/null || rc=$?
rm -f "$deploy"

echo
show="docker compose -f $DIR/docker-compose.yml -p $PROJECT exec muster /muster-server show-secret ui-password"
# Accounts mode (MUSTER_AUTH_MODE=gotrue): $URL is the machine-token port,
# which refuses a browser; a person signs in on the browser listener, at the
# host port that publishes it (docs/guides/gotrue.md).
browser_port=$(dc config 2>/dev/null | sed -n 's/^ *MUSTER_BROWSER_PORT: *"\{0,1\}\([0-9][0-9]*\)"\{0,1\}$/\1/p' | head -n 1)
if dc config 2>/dev/null | grep -Eq '^ *MUSTER_AUTH_MODE: *"?gotrue"?$'; then
  hp=""
  [ -z "$browser_port" ] || hp=$(dc port muster "$browser_port" 2>/dev/null | sed -n 's/.*:\([0-9][0-9]*\)$/\1/p' | head -n 1)
  if [ -n "$hp" ]; then
    say "muster is at http://localhost:$hp (accounts mode: browsers sign in on the browser listener; $URL serves machine tokens only and refuses a browser)"
  else
    say "accounts mode: browsers sign in only on the browser listener (MUSTER_BROWSER_PORT${browser_port:+=$browser_port}), which no host port publishes:"
    say "    add it to $DIR/docker-compose.override.yml (docs/guides/gotrue.md) and run this again. $URL serves machine tokens only"
  fi
  say "sign in with your own account (\`docker compose -f $DIR/docker-compose.yml -p $PROJECT exec muster /muster-server invite --claim-admin\` makes the first)"
  [ "$rc" = 0 ] || say "the doctor found a problem (above); fix it and run this again — it is safe to"
  exit "$rc"
fi
case "${MUSTER_PRINT_PASSWORD:-yes}" in
  no|0)
    say "muster is at $URL — open it in this machine's browser. The sign-in password is not printed here; show it with:"
    say "    $show" ;;
  *)
    say "muster is at $URL — open it in this machine's browser and sign in with:"
    say "    $pw"
    say "show the password again: $show" ;;
esac
say "on your phone: add HTTPS with Tailscale, $PHONE_DOC"
[ "$rc" = 0 ] || say "the doctor found a problem (above); fix it and run this again — it is safe to"
exit "$rc"
}

say() { printf 'muster-install: %s\n' "$*"; }
die() { printf 'muster-install: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

# crashloop: muster-mirror's systemd service is restarting over and over since
# this run started it: NRestarts grew by two or more, or it sat in
# auto-restart on two polls in a row. One restart is not a loop (a mirror that
# lost its first connection). Its fatal reason is the last line it printed
# starting "muster-mirror: " since then (mirror_reason); its ordinary log
# lines carry a timestamp instead. launchd (macOS) is not watched: no Mac
# measurement backs a reading of `launchctl print`, so the wait there is the
# plain 30 seconds and the doctor says why.
crashloop() {
  [ "$service" = systemd ] || return 1
  n=$(systemctl --user show muster-mirror.service -p NRestarts --value 2>/dev/null || echo 0)
  sub=$(systemctl --user show muster-mirror.service -p SubState --value 2>/dev/null || echo)
  if [ "$sub" = auto-restart ]; then held=$((${held:-0} + 1)); else held=0; fi
  [ "${n:-0}" -ge $((restarts0 + 2)) ] 2>/dev/null || [ "$held" -ge 2 ]
}
mirror_reason() {
  r=$(journalctl --user -u muster-mirror.service --since "$since" -o cat --no-pager 2>/dev/null | grep '^muster-mirror: ' | tail -n 1)
  # Without a per-user journal (volatile storage), the user unit's lines are
  # in the system journal, which --user-unit reads.
  [ -n "$r" ] || r=$(journalctl --user-unit muster-mirror.service --since "$since" -o cat --no-pager 2>/dev/null | grep '^muster-mirror: ' | tail -n 1)
  printf '%s\n' "${r:-(it printed no reason; journalctl --user -u muster-mirror.service -n 30)}"
}

# linger: muster-mirror is a systemd USER service, which runs only while this
# user is logged in unless the user has linger on: without it the mirror stops
# at logout and does not start at boot, so this machine's sessions go missing
# from muster until someone logs in. Already on: nothing to do. Else
# `loginctl enable-linger` as this user (polkit often allows it for oneself),
# else sudo — asked first, on the terminal (MUSTER_LINGER=yes/no answers
# without asking). MUSTER_LINGER=no changes nothing at all: not the sudo
# step, and not the polkit one either. Never fatal: when it stays off, it says
# what that means.
linger() {
  u=$(id -un)
  if ! have loginctl; then
    say "no loginctl here: muster-mirror runs while you are logged in, and may not start at boot"; return 0
  fi
  [ "$(loginctl show-user "$u" -p Linger --value 2>/dev/null)" = yes ] && return 0
  ans="${MUSTER_LINGER:-}"
  case "$ans" in
    n|N|no|NO)
      say "linger left OFF for $u (MUSTER_LINGER=$ans): muster-mirror stops when you log out and does not start at boot."
      say "Turn it on any time: loginctl enable-linger $u (or with sudo)"; return 0 ;;
  esac
  if loginctl enable-linger "$u" >/dev/null 2>&1; then
    say "turned on linger for $u: muster-mirror keeps running after logout and starts at boot"; return 0
  fi
  tty=/dev/null; (: </dev/tty) 2>/dev/null && tty=/dev/tty
  if [ -z "$ans" ] && [ "$tty" = /dev/tty ] && have sudo; then
    say "muster-mirror needs linger to keep running after you log out and to start at boot."
    say "that takes: sudo loginctl enable-linger $u"
    printf 'muster-install: run it now (sudo may ask for your password)? [y/N] ' >/dev/tty
    read -r ans </dev/tty || ans=
  fi
  case "$ans" in
    y|Y|yes|YES)
      if have sudo && sudo loginctl enable-linger "$u" <"$tty"; then
        say "turned on linger for $u: muster-mirror keeps running after logout and starts at boot"; return 0
      fi ;;
  esac
  say "linger is OFF for $u: muster-mirror stops when you log out and does not start at boot, so this machine's"
  say "sessions are missing from muster until you log in again. Turn it on any time: sudo loginctl enable-linger $u"
}

main "$@"

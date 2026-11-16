#!/usr/bin/env bash
# deploy/install.sh end to end, as a person runs it: `curl -fsSL
# <release>/install.sh | sh`, as a FRESH user of this machine.
#
#   tests/install-sh.sh <dist-dir> <image-repo>      e.g. dist muster-local
#
# <dist-dir> holds the release assets scripts/release-build.sh built (0.0.0);
# they are served on loopback as the "release" (MUSTER_RELEASE_BASE), so the
# script downloads and checksum-verifies them exactly as from GitHub.
# <image-repo>:0.0.0 is the muster image (MUSTER_IMAGE).
#
# 🔴 CI ONLY. It creates system users, enables linger and runs real systemd
# user services: it refuses to run unless CI=true (GitHub Actions sets it).
#
# Asserts:
#   * the first run exits 0 (its doctor passed); muster-mirror's user service
#     is active; this machine's mirror is connected; the stack answers on the
#     compose file's own port (8105);
#   * a second run exits 0 and changes nothing: the same sign-in password,
#     the same mirror token, and a line added by hand to muster-mirror's
#     settings is still there (only the lines it owns are rewritten);
#   * the mirror token never appears in curl's argv (which every user can read
#     in `ps`): the second run's curl goes through a shim that logs its
#     arguments; POSITIVE CONTROL, the log holds the token-authenticated
#     /mirror/host calls (so the shim saw them), and none carries the token;
#   * REFUSALS, each changing nothing: a second install under the same compose
#     project from another directory; a ~/muster this script did not make (a
#     second fresh user with an INSTALL.md-style directory); and this
#     machine's muster-mirror settings written by someone else;
#   * JOIN (MUSTER_JOIN_URL, a third fresh user): refused over settings it did
#     not write, then with MUSTER_MIRROR_ADOPT=1 they are MERGED (a hand line
#     kept, owned lines once), the machine connects and this-host passes;
#   * a crash-looping muster-mirror (a setting it rejects) is named with its
#     own reason line, and the run exits non-zero without waiting 30s.
set -euo pipefail

[ $# -eq 2 ] || { echo "usage: $0 <dist-dir> <image-repo>" >&2; exit 2; }
[ "${CI:-}" = true ] || { echo "install-sh: CI only (it creates users and services); CI=true is not set" >&2; exit 2; }
DIST="$(cd "$1" && pwd)"; IMAGE="$2"
[ -f "$DIST/install.sh" ] || { echo "install-sh: no $DIST/install.sh" >&2; exit 2; }
docker image inspect "$IMAGE:0.0.0" >/dev/null || { echo "install-sh: no image $IMAGE:0.0.0" >&2; exit 2; }

HTTP_PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')"
(cd "$DIST" && exec python3 -m http.server "$HTTP_PORT" --bind 127.0.0.1 >/dev/null 2>&1) &
http_pid=$!
REL="http://127.0.0.1:$HTTP_PORT"
for _ in $(seq 20); do curl -fsS -o /dev/null "$REL/checksums.txt" && break; sleep 0.5; done

fresh_user() {
  local u="$1"
  sudo useradd -m -s /bin/bash "$u"
  sudo usermod -aG docker "$u"
  sudo loginctl enable-linger "$u"
  local uid; uid="$(id -u "$u")"
  # systemd's own socket, not the D-Bus one: `systemctl --user` talks to the
  # manager over $XDG_RUNTIME_DIR/systemd/private, and the runner has no
  # D-Bus user session (on the runner /run/user/<uid>/bus did not appear in 30s).
  # sudo: the runtime dir is the user's own, mode 0700.
  for _ in $(seq 60); do sudo test -S "/run/user/$uid/systemd/private" && return 0; sleep 1; done
  echo "install-sh: $u's user manager did not start: $(sudo ls -la "/run/user/$uid" 2>&1); $(systemctl status "user@$uid.service" --no-pager 2>&1 | head -5)" >&2; return 1
}
# as <user> <command>: a login-like shell of that user with its user manager.
as() {
  local u="$1"; shift
  local uid; uid="$(id -u "$u")"
  sudo -u "$u" -H env XDG_RUNTIME_DIR="/run/user/$uid" \
    MUSTER_RELEASE_BASE="$REL" MUSTER_IMAGE="$IMAGE" PATH="/usr/local/bin:/usr/bin:/bin" \
    bash -c "cd ~ && $*"
}
U=muster-install-test; U2=muster-install-other
cleanup() {
  as "$U" 'docker compose -f ~/muster/docker-compose.yml -p muster down -v --remove-orphans' >/dev/null 2>&1 || true
  kill "$http_pid" 2>/dev/null || true
}
trap cleanup EXIT
fail() { echo "FAIL: $1"; as "$U" 'systemctl --user status muster-mirror.service --no-pager; journalctl --user -u muster-mirror.service -n 30 --no-pager' 2>&1 || true; exit 1; }
one_liner() { as "$1" "curl -fsSL $REL/install.sh | ${2:-} sh"; }

fresh_user "$U"
as "$U" 'tmux new-session -d -s install-test-session "sleep 3600"'

# --- the first run --------------------------------------------------------------
start=$(date +%s)
one_liner "$U" > /tmp/install-1.log 2>&1 || { cat /tmp/install-1.log; fail "the first run exited non-zero"; }
cat /tmp/install-1.log
echo "install-sh: the first run took $(( $(date +%s) - start ))s"
[ "$(as "$U" 'systemctl --user is-active muster-mirror.service')" = active ] || fail "muster-mirror.service is not active"
curl -fsS http://127.0.0.1:8105/health | grep -q '"status":"ok"' || fail "nothing answers on the compose file's port 8105"
tok="$(as "$U" 'cat ~/.config/muster-mirror/token')"
curl -fsS -H "Authorization: Bearer $tok" http://127.0.0.1:8105/mirror/host | grep -q '"online":true' || fail "this machine's mirror is not connected"
grep -q 'PASS  this-host' /tmp/install-1.log || fail "the doctor did not PASS this-host"
grep -q 'PASS  sign-in' /tmp/install-1.log || fail "the doctor did not PASS sign-in"
pw1="$(as "$U" 'docker compose -f ~/muster/docker-compose.yml -p muster exec -T muster /muster-server show-secret ui-password')"
grep -qF "$pw1" /tmp/install-1.log || fail "the first run did not print the sign-in password"
echo "ok   first run: exit 0, doctor passed, muster-mirror.service active and connected, password shown"

# --- the second run: idempotent --------------------------------------------------
as "$U" 'echo MUSTER_MIRROR_INTERACTIVE=0 >> ~/.config/muster-mirror/env'
# curl's argv, logged by a shim first on the install's PATH.
shim=/tmp/install-curl-shim; argv=/tmp/install-curl-argv.log
mkdir -p "$shim"; : > "$argv"; chmod 666 "$argv"
printf '#!/bin/sh\nprintf "%%s\\n" "$*" >> %s\nexec %s "$@"\n' "$argv" "$(command -v curl)" > "$shim/curl"; chmod 755 "$shim/curl"
one_liner "$U" "PATH=$shim:\$PATH" > /tmp/install-2.log 2>&1 || { cat /tmp/install-2.log; fail "the second run exited non-zero"; }
pw2="$(as "$U" 'docker compose -f ~/muster/docker-compose.yml -p muster exec -T muster /muster-server show-secret ui-password')"
[ "$pw1" = "$pw2" ] || fail "the second run changed the sign-in password"
[ "$(as "$U" 'cat ~/.config/muster-mirror/token')" = "$tok" ] || fail "the second run replaced the mirror token"
as "$U" 'grep -qx MUSTER_MIRROR_INTERACTIVE=0 ~/.config/muster-mirror/env' || fail "the second run dropped a hand-added settings line"
[ "$(as "$U" 'grep -c ^MUSTER_MIRROR_URL= ~/.config/muster-mirror/env')" = 1 ] || fail "the second run duplicated a line it owns"
if grep -q 'minted' /tmp/install-2.log; then fail "the second run minted a token"; fi
echo "ok   second run: exit 0, same password, same token, the hand-added line kept"
calls="$(grep -c '/mirror/host' "$argv" || true)"
[ "$calls" -ge 2 ] || fail "positive control: the curl shim logged $calls /mirror/host call(s), want the token check and the connect wait"
if grep -qF -- "$tok" "$argv"; then fail "the mirror token is on curl's command line: $(grep -F -- "$tok" "$argv" | head -1 | sed "s/$tok/<token>/")"; fi
echo "ok   the mirror token is never in curl's argv ($calls token-authenticated calls logged, none carries it)"

# --- refusals, each changing nothing ----------------------------------------------
# The containers by id, name and state: a recreated or restarted one differs.
# (Not .Status, whose "Exited (0) 4 seconds ago" changes by itself.)
before="$(docker ps -a --format '{{.ID}} {{.Names}} {{.State}} {{.CreatedAt}}' | sort)"
if one_liner "$U" 'MUSTER_DIR=$HOME/elsewhere' > /tmp/install-3.log 2>&1; then fail "a second install under the same project was NOT refused"; fi
grep -q 'compose project named muster already runs from' /tmp/install-3.log || { cat /tmp/install-3.log; fail "the same-project refusal does not say why"; }
as "$U" 'test ! -e ~/elsewhere' || fail "the refused run created ~/elsewhere"
echo "ok   refused: a second install under the same compose project, nothing created"

fresh_user "$U2"
as "$U2" 'mkdir -p ~/muster && printf "services: {}\n" > ~/muster/docker-compose.yml'
if one_liner "$U2" 'MUSTER_PROJECT=other' > /tmp/install-4.log 2>&1; then fail "a ~/muster this script did not make was NOT refused"; fi
grep -q 'did not install' /tmp/install-4.log || { cat /tmp/install-4.log; fail "the foreign-directory refusal does not say why"; }
as "$U2" 'rm -rf ~/muster && mkdir -p ~/.config/muster-mirror && echo MUSTER_MIRROR_URL=https://elsewhere.example > ~/.config/muster-mirror/env'
if one_liner "$U2" 'MUSTER_PROJECT=other' > /tmp/install-5.log 2>&1; then fail "muster-mirror settings someone else wrote were NOT refused"; fi
grep -q 'already set up' /tmp/install-5.log || { cat /tmp/install-5.log; fail "the foreign-mirror refusal does not say why"; }
as "$U2" 'test ! -e ~/muster' || fail "the refused run created ~/muster"
[ "$(docker ps -a --format '{{.ID}} {{.Names}} {{.State}} {{.CreatedAt}}' | sort)" = "$before" ] ||
  fail "a refused run changed the containers"
echo "ok   refused: a ~/muster and muster-mirror settings it did not make, nothing changed"

# --- joining: another machine's muster-mirror (a fresh user) --------------------
# MUSTER_JOIN_URL: no stack, the hub's release, the token minted on the hub.
# Its muster-mirror settings were written by someone else (an earlier
# INSTALL.md): refused until MUSTER_MIRROR_ADOPT=1, then MERGED, the hand line kept.
U3=muster-install-join
fresh_user "$U3"
as "$U3" 'tmux new-session -d -s join-session "sleep 3600"'
jtok="$(as "$U" 'docker compose -f ~/muster/docker-compose.yml -p muster exec -T muster /muster-server mirror token create joiner')"
[ -n "$jtok" ] || fail "could not mint the joining machine's token"
printf '%s\n' "$jtok" | as "$U3" 'mkdir -p ~/.config/muster-mirror && chmod 700 ~/.config/muster-mirror && umask 077 && cat > ~/.config/muster-mirror/token'
as "$U3" 'printf "MUSTER_MIRROR_URL=https://old-hub.example\nMUSTER_MIRROR_INTERACTIVE=0\nMY_HAND_ADDED_LINE=kept\n" > ~/.config/muster-mirror/env'
join="MUSTER_JOIN_URL=http://localhost:8105 MUSTER_MIRROR_HOST=joiner MUSTER_MIRROR_INTERACTIVE=1"
if one_liner "$U3" "$join" > /tmp/install-6.log 2>&1; then cat /tmp/install-6.log; fail "joining over settings it did not write was NOT refused"; fi
grep -q 'MUSTER_MIRROR_ADOPT=1' /tmp/install-6.log || { cat /tmp/install-6.log; fail "the refusal does not say how to allow it"; }
as "$U3" 'grep -qx MUSTER_MIRROR_URL=https://old-hub.example ~/.config/muster-mirror/env' || fail "the refused join changed the settings"
one_liner "$U3" "$join MUSTER_MIRROR_ADOPT=1" > /tmp/install-7.log 2>&1 || { cat /tmp/install-7.log; fail "the join exited non-zero"; }
cat /tmp/install-7.log
[ "$(as "$U3" 'systemctl --user is-active muster-mirror.service')" = active ] || fail "the joining machine's muster-mirror.service is not active"
curl -fsS -H "Authorization: Bearer $jtok" http://127.0.0.1:8105/mirror/host | grep -q '"online":true' || fail "the joining machine is not connected"
grep -q 'PASS  this-host' /tmp/install-7.log || fail "the joining machine's doctor did not PASS this-host"
for want in MY_HAND_ADDED_LINE=kept MUSTER_MIRROR_INTERACTIVE=1 MUSTER_MIRROR_URL=http://localhost:8105; do
  [ "$(as "$U3" "grep -cx '$want' ~/.config/muster-mirror/env")" = 1 ] || fail "after the join the settings do not hold $want exactly once"
done
if as "$U3" 'grep -q old-hub ~/.config/muster-mirror/env'; then fail "the join kept the old hub's URL"; fi
echo "ok   joined: refused unasked, then merged (the hand line kept), connected, this-host passed"

# --- a crash-looping muster-mirror is named, with its own reason ------------------
# A setting muster-mirror rejects makes it exit at start; systemd restarts it
# and it reads `activating`. The installer must say so, with the line
# muster-mirror printed, and exit non-zero — not wait out its 30 seconds.
journal_lines="$(as "$U3" 'journalctl --user -u muster-mirror.service -o cat --no-pager 2>/dev/null; journalctl --user-unit muster-mirror.service -o cat --no-pager 2>/dev/null' | grep -c . || true)"
as "$U3" 'echo MUSTER_MIRROR_AGENT_SPAWN_SKIP_PERMISSIONS=maybe >> ~/.config/muster-mirror/env'
start=$(date +%s)
if one_liner "$U3" "$join" > /tmp/install-8.log 2>&1; then cat /tmp/install-8.log; fail "a crash-looping muster-mirror exited 0"; fi
took=$(( $(date +%s) - start ))
cat /tmp/install-8.log
grep -q 'exits as it starts' /tmp/install-8.log || fail "the crash loop was not named"
if grep -q 'has not connected after 30s' /tmp/install-8.log; then fail "it waited out 30s on a crash loop (${took}s)"; fi
if [ "$journal_lines" -gt 0 ]; then
  grep -q 'muster-mirror: MUSTER_MIRROR_AGENT_SPAWN_SKIP_PERMISSIONS="maybe" is not a bool' /tmp/install-8.log ||
    fail "the crash loop's reason line was not printed (the journal holds $journal_lines line(s) for the unit)"
  echo "ok   crash loop: named with muster-mirror's own reason line, in ${took}s"
else
  echo "SKIP crash-loop reason line: this runner keeps no journal for user units (positive control found 0 lines); named in ${took}s"
fi
as "$U3" 'systemctl --user disable --now muster-mirror.service' >/dev/null 2>&1 || true
echo "install-sh: PASS"

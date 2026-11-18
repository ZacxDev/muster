#!/usr/bin/env bash
# The claude-code-agent image as the docker compose install runs it (`ccd
# compose`, cmd/ccd/compose.go), with the REAL entrypoint, ccd, tmux and CLI:
#
#   images/claude-code-agent/compose-smoke.sh <image>:<tag>
#
# Run on the HOST (it drives `docker compose`), by CI's ccd-image job. The
# chief-init and chief services are deploy/docker-compose.yml's OWN (profile
# chief), unchanged: the spec volume, user 0:1001, the capabilities, the
# instructions volume and the healthcheck all come from that file. The only service
# added here is `writer`, a uid-65532 stand-in for muster that writes the spec
# as muster's compose driver does (files first, spec.json last, each by
# rename, 0640), so this script can write specs muster never would.
#
# tests/compose-up.sh's chief phase runs the same file with muster itself and
# checks what it shares with this one: awaiting_spec before any spec, uid 1000
# cannot read spec.json, Off leaves no CLI, a prompt is denied. This script
# checks only what needs a hand-written spec:
#   1. the instructions volume at /etc/claude-code: the CLI's user (uid 1000) reads a 0640 group-1000 file
#      there, sees an atomic-rename update to it, and cannot write there;
#   2. replicas 1 starts the real ccd and CLI: the status reaches running/ready,
#      the proxied /healthz reports the session started, a turn without the
#      bearer reaches ccd (its 401), and /data/.muster-epoch records the epoch;
#      the secrets reached the CLI's environment (the turn bearer did not) and
#      appear in no log line; ccd and the CLI run as uid 1000, gid 1000, groups
#      exactly 1000, with no capabilities, HOME /home/node, in /data/workspace;
#      the supervisor as uid 0;
#   3. a new generation (after /exit left a transcript to resume) is a new child:
#      a new boot id and ccd pid, the same supervisor pid, /healthz 200 with the
#      session started again and the CLI resumed (`cli_mode` continue) — no
#      crash_loop — and /data kept (same epoch);
#   4. a NEW epoch: /data was emptied before the start (the planted file is
#      gone, the CLI starts fresh) and the new epoch recorded;
#   5. a spec version it does not know: state error, no CLI;
#   6. spec.json removed: awaiting_spec, "muster removed this chief's spec", no CLI.
#
# ⚠ NO REAL CREDENTIAL, NO TURN: the OAuth token is deliberately invalid (as
# cc-smoke's), which is enough for the CLI to start and fire SessionStart.
set -euo pipefail

IMG="${1:?usage: compose-smoke.sh <image>:<tag>}"
[[ "$IMG" == *:* ]] || { echo "compose-smoke: the image needs a tag: $IMG" >&2; exit 2; }
ROOT="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
project="ccsmoke$$"
turn_secret="compose-smoke-turn-$RANDOM$RANDOM-not-real"
oauth="sk-ant-oat01-SMOKE-DELIBERATELY-INVALID"

fail() {
  echo "compose-smoke: FAIL: $*" >&2
  dc ps -a >&2 || true
  dc logs --no-color chief 2>&1 | tail -80 >&2 || true
  exit 1
}
dc() {
  docker compose -p "$project" -f "$ROOT/deploy/docker-compose.yml" -f "$work/writer.yml" \
    --env-file "$work/env" --profile chief "$@"
}
cleanup() { dc down -v --remove-orphans >/dev/null 2>&1 || true; rm -rf "$work"; }
trap cleanup EXIT

# The file's required variables (only the chief's services are started), and
# the image under test as the chief's.
cat >"$work/env" <<ENV
MUSTER_VERSION=${IMG##*:}
CHIEF_IMAGE=${IMG%:*}
POSTGRES_PASSWORD=compose-smoke-unused
MUSTER_UI_PASSWORD=compose-smoke-unused
MUSTER_HOOK_TOKEN=compose-smoke-unused
ENV
cat >"$work/writer.yml" <<YAML
services:
  writer:
    profiles: ["chief"]
    image: $IMG
    user: "65532:1001"
    entrypoint: ["sleep", "infinity"]
    init: true
    network_mode: none
    depends_on: {chief-init: {condition: service_completed_successfully}}
    volumes: [chief-spec:/run/muster-spec, chief-instructions:/run/muster-spec/chief/files/etc/claude-code]
YAML

# put PATH (relative to the chief's spec dir) with stdin, as muster writes: a
# temporary file in the same directory, 0640, renamed into place.
put() {
  # shellcheck disable=SC2016 # expanded by the writer's sh, on purpose
  dc exec -T writer sh -euc 'f="/run/muster-spec/chief/$1"; cat >"$f.tmp"; chmod 0640 "$f.tmp"; mv "$f.tmp" "$f"' sh "$1"
}
spec() {  # spec VERSION GENERATION REPLICAS EPOCH
  printf '{"version":%s,"name":"chief","generation":%s,"replicas":%s,"epoch":"%s","env":[{"name":"CCD_SMOKE_ENV","value":"plain"}],"secrets":[{"name":"MUSTER_GATEWAY_BEARER","value":"%s"},{"name":"CLAUDE_CODE_OAUTH_TOKEN","value":"%s"}]}' \
    "$1" "$2" "$3" "$4" "$turn_secret" "$oauth" | put spec.json
}
# in_chief: as the CLI's user (uid 1000, as a tool call would run); as_sup: as
# the service's own user (0:1001), the supervisor's.
in_chief() { dc exec -T -u 1000:1000 chief "$@"; }
as_sup() { dc exec -T chief "$@"; }
status() { as_sup curl -s http://127.0.0.1:18789/compose/status; }
field() { python3 -c 'import json,sys; v=json.load(sys.stdin)[sys.argv[1]]; print(str(v).lower() if isinstance(v,bool) else v)' "$1"; }
# http PATH [curl args]: "<code> <body>" from inside the chief, through the supervisor.
http() {
  local p="$1"; shift
  in_chief curl -s -w '\n%{http_code}' "$@" "http://127.0.0.1:18789$p" \
    | python3 -c 'import sys; b, _, c = sys.stdin.read().rpartition("\n"); print(c, b.strip())'
}
wait_status() {  # wait_status DESCRIPTION PYTHON-EXPR-ON-s (s = the status dict)
  local st=""
  for _ in $(seq 1 180); do
    st="$(status 2>/dev/null || true)"
    if [[ -n "$st" ]] && python3 -c 'import json,sys; s=json.loads(sys.argv[1]); sys.exit(0 if eval(sys.argv[2]) else 1)' "$st" "$2" 2>/dev/null; then
      echo "$st"; return 0
    fi
    sleep 1
  done
  fail "waited 180s for $1; last status: $st"
}
wait_healthz() {  # wait_healthz GLOB: the proxied /healthz until "<code> <body>" matches
  local out=""
  for _ in $(seq 1 180); do
    out="$(http /healthz || true)"
    # shellcheck disable=SC2053 # a glob on purpose
    [[ "$out" == $1 ]] && { echo "$out"; return 0; }
    sleep 1
  done
  fail "the proxied /healthz never matched $1; last: $out"
}
procs() { as_sup ps -eo pid=,args= ; }
# ids PID: "<uid> <gid> <groups> <CapEff>" of a process, from its status.
ids() {
  # shellcheck disable=SC2016 # awk's own fields
  as_sup awk '/^Uid:/ {u=$2} /^Gid:/ {g=$2} /^Groups:/ {$1=""; gr=substr($0,2)} /^CapEff:/ {c=$2}
    END {print u, g, gr, c}' "/proc/$1/status"
}
sup_pid() { procs | awk '$2 == "ccd" && $3 == "compose" { print $1 }'; }
no_cli_left() {  # no claude, tmux or ccd serve process in the chief
  local p
  p="$(procs)"
  if grep -Eq '(^|[ /])(claude|tmux)( |$)|ccd serve' <<<"$p"; then
    fail "$1: processes remain: $p"
  fi
}

# Up, with no spec yet (tests/compose-up.sh checks what that state reads).
dc up -d --wait chief-init writer chief || fail "docker compose up --wait"
sup1="$(sup_pid)"; [[ -n "$sup1" ]] || fail "no ccd compose process: $(procs)"
echo "compose-smoke: the real chief service is up (deploy/docker-compose.yml); supervisor pid $sup1"

# 1. The instructions volume: files/etc/claude-code as the writer (muster) sees
# it, /etc/claude-code as the chief does.
printf 'v1\n' | put files/etc/claude-code/CLAUDE.md
[[ "$(in_chief cat /etc/claude-code/CLAUDE.md)" == v1 ]] || fail "the chief does not read /etc/claude-code/CLAUDE.md"
[[ "$(in_chief stat -c '%a %g' /etc/claude-code/CLAUDE.md)" == "640 1000" ]] || fail "CLAUDE.md is $(in_chief stat -c '%a %g' /etc/claude-code/CLAUDE.md)"
printf 'v2\n' | put files/etc/claude-code/CLAUDE.md
[[ "$(in_chief cat /etc/claude-code/CLAUDE.md)" == v2 ]] || fail "a rename update is not visible at /etc/claude-code"
if in_chief sh -c 'echo x > /etc/claude-code/planted' 2>/dev/null; then fail "the chief could write /etc/claude-code"; fi
echo "compose-smoke: /etc/claude-code: 0640 readable by uid 1000, rename updates visible, read-only"
# 2. replicas 1: the real ccd and CLI.
spec 1 1 1 aa01
st="$(wait_status 'generation 1 running' 's["state"]=="running" and s["ready"] and s["generation"]==1')"
boot1="$(field boot <<<"$st")"
wait_healthz '200 {*"session":"started"*' >/dev/null
out="$(http /v1/responses -X POST -d '{"input":"x"}')"
[[ "$out" == '401 '* ]] || fail "a turn without the bearer did not reach ccd (want its 401): $out"
[[ "$(in_chief cat /data/.muster-epoch)" == aa01 ]] || fail "/data/.muster-epoch is '$(in_chief cat /data/.muster-epoch 2>&1)'"
serve1="$(procs | awk '$2 == "ccd" && $3 == "serve" { print $1 }')"
cli="$(in_chief tmux list-panes -t cc -F '#{pane_pid}')"
for p in "$serve1" "$cli"; do
  [[ "$(ids "$p")" == "1000 1000 1000 0000000000000000" ]] \
    || fail "pid $p ($(as_sup cat "/proc/$p/comm")) runs as '$(ids "$p")', want uid 1000 gid 1000 groups 1000, no capabilities"
done
[[ "$(ids "$sup1" | cut -d' ' -f1-2)" == "0 1001" ]] || fail "the supervisor runs as '$(ids "$sup1")', want uid 0 gid 1001"
in_chief sh -c "tr '\\0' '\\n' </proc/$cli/environ" | grep -qx 'HOME=/home/node' || fail "the CLI's HOME is not /home/node"
[[ "$(in_chief readlink "/proc/$cli/cwd")" == /data/workspace ]] || fail "the CLI runs in $(in_chief readlink "/proc/$cli/cwd")"
# The positive control for the log check below: the secret DID reach the CLI.
in_chief sh -c "tr '\\0' '\\n' </proc/$cli/environ" | grep -qxF "CLAUDE_CODE_OAUTH_TOKEN=$oauth" \
  || fail "the CLI's environment lacks the spec's CLAUDE_CODE_OAUTH_TOKEN"
in_chief sh -c "tr '\\0' '\\n' </proc/$cli/environ" | grep -q '^MUSTER_GATEWAY_BEARER=' \
  && fail "the turn bearer reached the CLI's environment (ccd must unset it)"
in_chief sh -c 'echo planted > /data/smoke-marker'
echo "compose-smoke: generation 1 running (boot $boot1, ccd pid $serve1): session started, turn reached ccd, epoch recorded"

# 3. A new generation, with a transcript to resume: /exit leaves one (cc-smoke step 5).
in_chief tmux send-keys -t cc -l '/exit'
sleep 0.3
in_chief tmux send-keys -t cc Enter
wait_healthz '200 {*"cli_starts":2*"session":"started"*' >/dev/null
spec 1 2 1 aa01
st="$(wait_status 'generation 2 running' 's["state"]=="running" and s["ready"] and s["generation"]==2')"
boot2="$(field boot <<<"$st")"
[[ "$boot2" != "$boot1" ]] || fail "generation 2 kept boot $boot1"
hz="$(wait_healthz '200 {*"session":"started"*')"
[[ "$hz" == *'"supervisor":{"cli":"running","cli_mode":"continue","cli_starts":1'* ]] \
  || fail "after the restart ccd did not resume cleanly: $hz"
serve2="$(procs | awk '$2 == "ccd" && $3 == "serve" { print $1 }')"
[[ -n "$serve2" && "$serve2" != "$serve1" ]] || fail "generation 2's ccd is pid '$serve2' (generation 1's: $serve1)"
[[ "$(sup_pid)" == "$sup1" ]] || fail "the supervisor's pid moved: $(sup_pid), was $sup1"
[[ "$(in_chief cat /data/smoke-marker)" == planted ]] || fail "the same epoch lost /data"
echo "compose-smoke: generation 2 is a new child (boot $boot2, ccd pid $serve2) that resumed: $hz"

# 4. A new epoch: /data emptied first.
spec 1 3 1 bb02
wait_status 'generation 3 running' 's["state"]=="running" and s["ready"] and s["generation"]==3' >/dev/null
hz="$(wait_healthz '200 {*"session":"started"*')"
if in_chief test -e /data/smoke-marker; then fail "a new epoch did not empty /data"; fi
[[ "$(in_chief cat /data/.muster-epoch)" == bb02 ]] || fail "the new epoch was not recorded"
[[ "$hz" == *'"cli_mode":"fresh"'* ]] || fail "after the wipe the CLI did not start fresh: $hz"
echo "compose-smoke: new epoch -> /data emptied, epoch bb02 recorded, a fresh CLI"

# 5. A version this image does not know.
spec 2 4 1 bb02
st="$(wait_status error 's["state"]=="error"')"
[[ "$(field reason <<<"$st")" == "spec.json is version 2; this image reads version 1 only" ]] || fail "version 2: $st"
no_cli_left "version 2"
echo "compose-smoke: version 2 refused: $(field reason <<<"$st")"

# 6. Muster removes the spec.
spec 1 4 1 bb02
wait_status 'generation 4 running' 's["state"]=="running" and s["ready"] and s["generation"]==4' >/dev/null
dc exec -T writer rm /run/muster-spec/chief/spec.json
st="$(wait_status awaiting_spec 's["state"]=="awaiting_spec"')"
[[ "$(field reason <<<"$st")" == "muster removed this chief's spec" ]] || fail "spec removed: $st"
no_cli_left "spec removed"
[[ "$(sup_pid)" == "$sup1" ]] || fail "the supervisor's pid moved: $(sup_pid), was $sup1"

# The secrets in no log line (the control above showed they passed through).
logs="$(dc logs --no-color chief 2>&1)"
grep -q 'ccd compose: started the chief' <<<"$logs" || fail "control: the logs hold no start line"
if grep -qF -e "$turn_secret" -e "$oauth" <<<"$logs"; then fail "a secret from spec.json is in the chief's logs"; fi
echo "compose-smoke: no secret in $(wc -l <<<"$logs") log lines"
# A measurement, printed: whether stopping the tmux server alone ended what each
# stopped chief left, or the descendant sweep had to kill something.
if grep -q 'killing what the stopped chief left running' <<<"$logs"; then
  echo "compose-smoke: the sweep killed: $(grep 'killing what the stopped chief left running' <<<"$logs")"
else
  echo "compose-smoke: stopping the tmux server alone ended everything every stopped chief left (the sweep killed nothing)"
fi
echo "compose-smoke: PASS"

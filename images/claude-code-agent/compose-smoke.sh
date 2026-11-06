#!/usr/bin/env bash
# The claude-code-agent image as a `docker compose` service (`ccd compose`,
# cmd/ccd/compose.go), with the REAL entrypoint, ccd, tmux and CLI:
#
#   images/claude-code-agent/compose-smoke.sh <image>
#
# Run on the HOST (it drives `docker compose`), by CI's ccd-image job. It lays
# the service out as the compose install does: a spec volume a root one-shot
# prepares (setgid 02750 directories owned by uid 65532: group 1001 for the
# spec's own, group 1000 for files/ and below), written by a uid-65532 writer
# standing in for muster (files first, spec.json last, each by rename, 0640),
# mounted read-only into the chief, whose files/etc/claude-code is mounted at
# /etc/claude-code through a volume `subpath`. The chief runs as user 0:1001
# with every capability dropped but SETUID, SETGID and KILL, no-new-privileges,
# `init: true`, `restart: unless-stopped`, the healthcheck on /compose/status.
#
# It asserts:
#   1. before any spec, `up --wait` passes, the status is awaiting_spec, /healthz
#      is 503 and a turn is a typed not_ready 503 — and no CLI runs;
#   2. the subpath mount: the CLI's user (uid 1000) reads a 0640 group-1000 file
#      there, sees an atomic-rename update to it, and cannot write there — and
#      CANNOT read spec.json (0640, group 1001), which the supervisor's user can;
#   3. replicas 1 starts the real ccd and CLI: the status reaches running/ready,
#      the proxied /healthz reports the session started, a turn without the
#      bearer reaches ccd (its 401), and /data/.muster-epoch records the epoch;
#      the secrets reached the CLI's environment and appear in no log line; ccd
#      and the CLI run as uid 1000, gid 1000, groups exactly 1000, with no
#      capabilities, HOME /home/node, in /data/workspace; the supervisor as uid 0;
#      and a permission prompt (`ccd approve`, as the CLI's hooks run it) is
#      denied at once, where the same hook without CCD_APPROVE_UNANSWERED defers;
#   4. a new generation (after /exit left a transcript to resume) is a new child:
#      a new boot id and ccd pid, the same supervisor pid, /healthz 200 with the
#      session started again and the CLI resumed (`cli_mode` continue) — no
#      crash_loop — and /data kept (same epoch);
#   5. replicas 0: state off, NO claude, tmux or `ccd serve` process left, the
#      supervisor's pid unchanged, the container still running with no restart,
#      /healthz 503;
#   6. replicas 1 with a NEW epoch: /data was emptied before the start (the
#      planted file is gone, the CLI starts fresh) and the new epoch recorded;
#   7. a spec version it does not know: state error, no CLI;
#   8. spec.json removed: awaiting_spec, "muster removed this chief's spec", no CLI.
#
# ⚠ NO REAL CREDENTIAL, NO TURN: the OAuth token is deliberately invalid (as
# cc-smoke's), which is enough for the CLI to start and fire SessionStart.
set -euo pipefail

IMG="${1:?usage: compose-smoke.sh <image>}"
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
dc() { docker compose -p "$project" -f "$work/compose.yml" "$@"; }
cleanup() { dc down -v --remove-orphans >/dev/null 2>&1 || true; rm -rf "$work"; }
trap cleanup EXIT

cat >"$work/compose.yml" <<YAML
services:
  spec-init:
    image: $IMG
    user: "0"
    entrypoint: ["sh", "-euc"]
    command:
      - |
        install -d -o 65532 -g 1001 -m 02750 /run/muster-spec /run/muster-spec/chief
        install -d -o 65532 -g 1000 -m 02750 /run/muster-spec/chief/files \
          /run/muster-spec/chief/files/etc /run/muster-spec/chief/files/etc/claude-code
    volumes: [spec:/run/muster-spec]
  writer:
    image: $IMG
    user: "65532:1001"
    entrypoint: ["sleep", "infinity"]
    init: true
    depends_on: {spec-init: {condition: service_completed_successfully}}
    volumes: [spec:/run/muster-spec]
  chief:
    image: $IMG
    user: "0:1001"
    cap_drop: [ALL]
    cap_add: [SETUID, SETGID, KILL]
    security_opt: ["no-new-privileges:true"]
    init: true
    restart: unless-stopped
    depends_on: {spec-init: {condition: service_completed_successfully}}
    environment:
      CCD_SPEC_DIR: /run/muster-spec/chief
    volumes:
      - spec:/run/muster-spec:ro
      - type: volume
        source: spec
        target: /etc/claude-code
        read_only: true
        volume: {subpath: chief/files/etc/claude-code}
      - data:/data
    healthcheck:
      test: ["CMD", "curl", "-fsS", "http://127.0.0.1:18789/compose/status"]
      interval: 2s
      timeout: 3s
      retries: 30
volumes:
  spec: {}
  data: {}
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

# 1. No spec yet.
dc up -d --wait spec-init writer chief || fail "docker compose up --wait"
st="$(status)"
[[ "$(field state <<<"$st")" == awaiting_spec ]] || fail "before any spec: $st"
out="$(http /healthz)"; [[ "$out" == '503 {"compose":{"state":"awaiting_spec"'* ]] || fail "/healthz with no spec: $out"
out="$(http /v1/responses -X POST -d '{}')"
[[ "$out" == '503 {"error":{"type":"not_ready","code":"not_ready","message":"waiting for muster to write this chief'* ]] \
  || fail "a turn with no spec: $out"
no_cli_left "before any spec"
sup1="$(sup_pid)"; [[ -n "$sup1" ]] || fail "no ccd compose process: $(procs)"
echo "compose-smoke: no spec -> awaiting_spec, /healthz 503, turn not_ready; up --wait passed; supervisor pid $sup1"

# 2. The subpath mount of files/etc/claude-code.
printf 'v1\n' | put files/etc/claude-code/CLAUDE.md
[[ "$(in_chief cat /etc/claude-code/CLAUDE.md)" == v1 ]] || fail "the chief does not read /etc/claude-code/CLAUDE.md"
[[ "$(in_chief stat -c '%a %g' /etc/claude-code/CLAUDE.md)" == "640 1000" ]] || fail "CLAUDE.md is $(in_chief stat -c '%a %g' /etc/claude-code/CLAUDE.md)"
printf 'v2\n' | put files/etc/claude-code/CLAUDE.md
[[ "$(in_chief cat /etc/claude-code/CLAUDE.md)" == v2 ]] || fail "a rename update is not visible through the subpath mount"
if in_chief sh -c 'echo x > /etc/claude-code/planted' 2>/dev/null; then fail "the chief could write /etc/claude-code"; fi
echo "compose-smoke: /etc/claude-code: 0640 readable by uid 1000, rename updates visible, read-only"
# The spec is the supervisor's alone: the positive control first (its user
# reads it), then the CLI's user is refused.
spec 1 1 0 aa01
[[ "$(as_sup stat -c '%a %g' /run/muster-spec/chief/spec.json)" == "640 1001" ]] \
  || fail "spec.json is $(as_sup stat -c '%a %g' /run/muster-spec/chief/spec.json), want 640 group 1001"
as_sup cat /run/muster-spec/chief/spec.json | grep -qF "$turn_secret" || fail "control: the supervisor's user cannot read spec.json"
if out="$(in_chief cat /run/muster-spec/chief/spec.json 2>&1)"; then fail "uid 1000 read spec.json"; fi
[[ "$out" == *"Permission denied"* ]] || fail "uid 1000's read of spec.json failed, but not as Permission denied: $out"
echo "compose-smoke: spec.json (640, group 1001): readable by the supervisor's user, Permission denied to uid 1000"

# 3. replicas 1: the real ccd and CLI.
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
# A permission prompt in this chief is DENIED, never deferred to a terminal no
# one can reach: the PermissionRequest hook (`ccd approve`) run as the CLI's
# hooks run it, as uid 1000 with the CLI's own environment, which `ccd compose`
# gave CCD_APPROVE_UNANSWERED=deny. The control: the same hook with that one
# variable removed defers (prints nothing), so the deny is the flag's.
approve_as_cli() {  # approve_as_cli [env -u args…]: the hook's stdout; its stderr in $work/approve.err
  # shellcheck disable=SC2016 # expanded by the container's bash, on purpose
  printf '%s' '{"tool_name":"Bash","tool_input":{"command":"true"},"permission_mode":"default"}' \
    | dc exec -T -u 1000:1000 chief bash -c 'mapfile -d "" e <"/proc/$1/environ"; shift; exec env -i "${e[@]}" env "$@" ccd approve' \
      sh "$cli" "$@" 2>"$work/approve.err"
}
in_chief sh -c "tr '\\0' '\\n' </proc/$cli/environ" | grep -qx 'CCD_APPROVE_UNANSWERED=deny' \
  || fail "the CLI's environment lacks CCD_APPROVE_UNANSWERED=deny"
out="$(approve_as_cli)"
[[ "$out" == '{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny"}}}' ]] \
  || fail "a permission prompt in the chief was not denied: stdout '$out', stderr $(cat "$work/approve.err")"
grep -q 'no operator can answer a permission prompt in this container' "$work/approve.err" \
  || fail "the denial does not say why: $(cat "$work/approve.err")"
out="$(approve_as_cli -u CCD_APPROVE_UNANSWERED)"
[[ -z "$out" ]] || fail "control: without the flag the hook did not defer: '$out'"
echo "compose-smoke: a permission prompt in the chief is denied (the flag ccd compose sets); without it, deferred"
echo "compose-smoke: generation 1 running (boot $boot1, ccd pid $serve1): session started, turn reached ccd, epoch recorded"

# 4. A new generation, with a transcript to resume: /exit leaves one (cc-smoke step 5).
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

# 5. replicas 0.
spec 1 2 0 aa01
wait_status off 's["state"]=="off" and not s["ready"] and s["boot"]==""' >/dev/null
no_cli_left "replicas 0"
[[ "$(sup_pid)" == "$sup1" ]] || fail "replicas 0: the supervisor's pid moved: $(sup_pid), was $sup1"
cid="$(dc ps -q chief)"
[[ "$(docker inspect -f '{{.State.Running}} {{.RestartCount}}' "$cid")" == "true 0" ]] \
  || fail "replicas 0: the container is $(docker inspect -f '{{.State.Status}} restarts={{.RestartCount}}' "$cid")"
out="$(http /healthz)"; [[ "$out" == '503 {"compose":{"state":"off"'* ]] || fail "/healthz while off: $out"
echo "compose-smoke: replicas 0 -> off: no claude, tmux or ccd serve left; supervisor $sup1 and the container still up"

# 6. A new epoch: /data emptied first.
spec 1 3 1 bb02
wait_status 'generation 3 running' 's["state"]=="running" and s["ready"] and s["generation"]==3' >/dev/null
hz="$(wait_healthz '200 {*"session":"started"*')"
if in_chief test -e /data/smoke-marker; then fail "a new epoch did not empty /data"; fi
[[ "$(in_chief cat /data/.muster-epoch)" == bb02 ]] || fail "the new epoch was not recorded"
[[ "$hz" == *'"cli_mode":"fresh"'* ]] || fail "after the wipe the CLI did not start fresh: $hz"
echo "compose-smoke: new epoch -> /data emptied, epoch bb02 recorded, a fresh CLI"

# 7. A version this image does not know.
spec 2 4 1 bb02
st="$(wait_status error 's["state"]=="error"')"
[[ "$(field reason <<<"$st")" == "spec.json is version 2; this image reads version 1 only" ]] || fail "version 2: $st"
no_cli_left "version 2"
echo "compose-smoke: version 2 refused: $(field reason <<<"$st")"

# 8. Muster removes the spec.
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

#!/usr/bin/env bash
# Bring the production compose stack up for real and prove muster serves.
#
#   tests/compose-up.sh <image-repo> <tag>     e.g. muster-local 1.2.3
#   make compose-up-test IMAGE=muster-local VERSION=1.2.3
#   COMPOSE_UP_PROFILES="s3" tests/compose-up.sh …   (which optional phases run;
#                                                     default "s3 stt chief", "" for none)
#
# Uses deploy/docker-compose.yml UNCHANGED, with MUSTER_IMAGE pointing at a
# locally built image (`make image IMAGE=… VERSION=…`), a unique project name
# and a free host port, so it never touches another stack. The default stack
# first, with NO optional variable set; then each optional profile with exactly
# the .env lines docs/features.md gives for it. (stt-cohere needs a GPU and is
# not brought up here.)
#
# Asserts, after `docker compose up -d --wait` returned 0:
#   * both containers report health "healthy";
#   * GET /health is 200 and reports the image's version;
#   * GET /readyz is 200 (database reachable, wiring sound);
#   * GET /login serves the sign-in page;
#   * the shared password signs in (POST /login sets a session cookie);
#   * NEGATIVE CONTROL: a wrong password does not;
#   * NEGATIVE CONTROL: a second stack whose MUSTER_UI_PASSWORD is an unedited
#     placeholder (CHANGE-ME, under 12 characters) must FAIL `up --wait` — the
#     web tier refuses itself while /readyz stays 200, and only the
#     healthcheck's /login probe turns that into "unhealthy";
#   * NEGATIVE CONTROL: MUSTER_HOOK_TOKEN=CHANGE-ME (a public admin bearer)
#     must make muster refuse to boot;
#   * muster did not restart on the fresh volume (the Postgres init race);
#   * Web Push keys minted with `docker compose run … vapid generate` reach
#     muster: the signed-in key route serves the generated public key;
#   * with no optional variable, dictation and attachments log plain OFF (not
#     MISCONFIGURED): the conditional defaults stay out of the default stack;
#   * session state is ON by default (`session state: ON`), and
#     MUSTER_SESSION_STATE=0 or an EMPTY value in .env turns it OFF;
#   * profile s3 (COMPOSE_PROFILES=s3 + S3_STORE_SECRET_KEY +
#     MUSTER_UPLOAD_SIGNING_KEY + MUSTER_PUBLIC_URL): `up --wait` succeeds,
#     muster logs `file attachments: ON` naming the bundled store, a file
#     uploaded through POST /uploads comes back byte for byte through the
#     agent's signed link, and (NEGATIVE CONTROL) a tampered signature is 404;
#   * profile stt (COMPOSE_PROFILES=stt + MUSTER_STT_TOKEN only): `up --wait`
#     succeeds once the model is on disk, muster logs `dictation: ON` naming
#     the stt-whisper service, and two SPOKEN clips (espeak-ng) sent to
#     muster's own POST /transcribe come back as their words — each clip's
#     transcript has its own words and not the other's, so an empty or fixed
#     answer cannot pass; NEGATIVE CONTROL: the STT service refuses a wrong
#     bearer token and accepts muster's.
#   * profile chief (the chief profile, MUSTER_CHIEF_RUNTIME=compose, the
#     connector key and an OpenRouter key; the model is a fake behind muster's
#     LLM proxy, tests/compose-chief.override.yml): `up --wait` succeeds with
#     the chief off and its supervisor awaiting its spec; the chief reaches
#     muster and NEITHER Postgres NOR the model directly; Account → Chief names
#     the credential, the model and the key's credit-limit link; turned on, the
#     supervisor runs it; uid 1000 (the agent) cannot read spec.json and can
#     read its CLAUDE.md, the supervisor can read the spec; a chat turn's reply
#     carries a marker that only a task on the board held — so the CLI called
#     the model, the model called muster's tool over MCP, and the REAL tool
#     result came back (the check is first shown to refuse a marker nobody
#     wrote); the model is offered exactly the chief's pre-allowed muster
#     tools (no task_comment); a call to task_comment anyway is refused and
#     leaves no comment; an offered tool the chief does not pre-allow (Bash
#     `touch`) is DENIED, the file is not created, and the turn ends instead of
#     hanging; every model request is pinned to deepseek/deepseek-v4.1-flash and
#     carries the owner's key and nothing else; turned off, the supervisor
#     reads off, no claude process runs, the model hears nothing for 15s, and
#     the container stays up; then it is removed exactly as docs/chief.md says
#     (rm -sf chief chief-init, down, the three chief volumes) and nothing of it
#     is left. COMPOSE_UP_CHIEF_BREAK=marker skips the task, and the phase must
#     then fail.
# Always tears down (`down -v`, every profile), including on failure.
set -euo pipefail

[ $# -eq 2 ] || { echo "usage: $0 <image-repo> <tag>" >&2; exit 2; }
IMAGE="$1"; TAG="$2"
# CDPATH unset: with it exported, `cd` to a relative path PRINTS the directory,
# and ROOT became two lines (measured: every `make` target here broke).
ROOT="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
docker image inspect "$IMAGE:$TAG" >/dev/null 2>&1 || { echo "no local image $IMAGE:$TAG" >&2; exit 2; }
PHASES="${COMPOSE_UP_PROFILES-s3 stt chief}"
case " $PHASES " in *" stt "*)
  command -v espeak-ng >/dev/null 2>&1 || { echo "the stt phase needs espeak-ng (it speaks the test clips)" >&2; exit 2; } ;;
esac

PROJECT="muster-compose-test-$$-$RANDOM"
PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')"
tmp="$(mktemp -d)"
PASSWORD="ui-pass-fixture-$RANDOM"  # >= 12 characters, whatever $RANDOM is
cat > "$tmp/env" <<EOF
MUSTER_IMAGE=$IMAGE
MUSTER_VERSION=$TAG
MUSTER_HTTP_PORT=$PORT
POSTGRES_PASSWORD=pg-$RANDOM$RANDOM
MUSTER_UI_PASSWORD=$PASSWORD
MUSTER_HOOK_TOKEN=hook-token-fixture-$RANDOM
MUSTER_SECURE_COOKIES=0
EOF
dc() { docker compose -f "$ROOT/deploy/docker-compose.yml" --env-file "$tmp/env" -p "$PROJECT" "$@"; }
# Every profile, so the teardown also removes the optional services and volumes.
cleanup() {
  dc --profile stt --profile s3 --profile chief down -v --remove-orphans >/dev/null 2>&1 || true
  FAKE_LLM_DIR="$ROOT/tests/fake-llm" docker compose -f "$ROOT/deploy/docker-compose.yml" -f "$ROOT/tests/compose-chief.override.yml" \
    --env-file "$tmp/env" -p "$PROJECT" --profile chief down -v --remove-orphans >/dev/null 2>&1 || true
  rm -rf "$tmp"
}
trap cleanup EXIT

fail() { echo "FAIL: $1"; dc ps -a || true; dc logs --no-color --tail 40 muster || true; exit 1; }

echo "compose-up: project $PROJECT, port $PORT, image $IMAGE:$TAG"
# Web Push keys, minted the way .env.example tells a user to: the image's own
# generator through compose, appended to the env file.
dc run --rm --no-deps muster vapid generate >> "$tmp/env" 2>/dev/null || fail "vapid generate through compose failed"
echo "MUSTER_VAPID_SUBJECT=mailto:ops@example.com" >> "$tmp/env"
VAPID_PUB="$(sed -n 's/^MUSTER_VAPID_PUBLIC_KEY=//p' "$tmp/env")"
[ -n "$VAPID_PUB" ] || fail "vapid generate printed no MUSTER_VAPID_PUBLIC_KEY"
start=$(date +%s)
dc up -d --wait --wait-timeout 180 || fail "docker compose up -d --wait did not succeed"
echo "compose-up: --wait returned 0 after $(( $(date +%s) - start ))s"

for svc in postgres muster; do
  h="$(docker inspect -f '{{.State.Health.Status}}' "$(dc ps -q "$svc")")"
  [ "$h" = healthy ] || fail "$svc health is '$h'"
  echo "ok   $svc: healthy"
done
# A fresh volume must not cost muster a crash: before the TCP healthcheck and
# the boot connect retry, muster's first connect could lose the race with
# Postgres's init and the container restarted.
rc="$(docker inspect -f '{{.RestartCount}}' "$(dc ps -q muster)")"
[ "$rc" = 0 ] || fail "muster restarted $rc time(s) during a fresh bring-up"
echo "ok   muster: 0 restarts on a fresh volume"

base="http://127.0.0.1:$PORT"
body="$(curl -fsS "$base/health")" || fail "/health"
python3 -c 'import json,sys; d=json.loads(sys.argv[1]); assert d["status"]=="ok" and d["version"]==sys.argv[2], d' "$body" "$TAG" ||
  fail "/health body $body does not report version $TAG"
echo "ok   /health: $body"
code="$(curl -sS -o /dev/null -w '%{http_code}' "$base/readyz")"
[ "$code" = 200 ] || fail "/readyz answered $code"
echo "ok   /readyz: 200"
# ⚠ EVERY CHECK BELOW CAPTURES, THEN GREPS — never `cmd | grep -q`. Under
# pipefail, grep -q exits at the first match and a writer still writing dies of
# SIGPIPE (141), so the pipeline reports a match as a FAILURE. It did: the
# placeholder-hook-token check failed with that very line in the dumped log
# (muster restart-loops, so `dc logs` writes one line per restart, and how many
# it has to write is timing).
login="$(curl -fsS "$base/login")" || fail "/login did not answer 200"
grep -q 'Sign in to muster' <<<"$login" || fail "/login is not the sign-in page"
echo "ok   /login: sign-in page"

# Sign in with the shared password; a wrong one is the negative control.
signin() {
  curl -sS -o /dev/null -D - -X POST --data-urlencode "password=$1" "$base/login" | tr -d '\r'
}
good="$(signin "$PASSWORD")"; bad="$(signin "not-$PASSWORD")"
grep -qi '^set-cookie:' <<<"$good" || fail "the right password set no cookie: $good"
if grep -qi '^set-cookie:' <<<"$bad"; then fail "a WRONG password set a cookie: $bad"; fi
echo "ok   sign-in: the password sets a session cookie, a wrong one does not"

# The push keys reached muster: the signed-in key route serves the generated key.
cookie="$(grep -i '^set-cookie:' <<<"$good" | head -1 | sed -E 's/^[Ss]et-[Cc]ookie: ([^;]*).*/\1/')"
key="$(curl -fsS -H "Cookie: $cookie" "$base/api/push/vapid-public-key")" || fail "/api/push/vapid-public-key did not answer 200"
grep -qF "$VAPID_PUB" <<<"$key" || fail "the push key route served '$key', not the generated key"
echo "ok   web push: the generated VAPID key is served"

# No optional variable: the optional features are plain OFF. A conditional
# default that leaked into the default stack (an endpoint with no secret) would
# say OFF (MISCONFIGURED) here.
boot="$(dc logs --no-color muster 2>&1)"
grep -q 'dictation: OFF — MUSTER_STT_TOKEN is unset (MUSTER_STT_URL: unset' <<<"$boot" ||
  fail "with no optional variable, the dictation line is not plain OFF with MUSTER_STT_URL unset: $(grep 'dictation:' <<<"$boot")"
grep -q 'file attachments: OFF — none of the required variables is set' <<<"$boot" ||
  fail "with no optional variable, attachments are not plain OFF: $(grep 'file attachments:' <<<"$boot")"
echo "ok   default stack: dictation and attachments are plain OFF"
grep -q 'session state: ON (MUSTER_SESSION_STATE=1)' <<<"$boot" ||
  fail "the default stack does not log session state ON: $(grep 'session state:' <<<"$boot")"
echo "ok   default stack: session state: ON (the compose default)"

# Session state off from .env: 0, and an EMPTY value (the compose default must
# not turn an empty value back into 1).
for v in 0 ""; do
  dc down -v --remove-orphans >/dev/null 2>&1
  cp "$tmp/env" "$tmp/env.ss"; echo "MUSTER_SESSION_STATE=$v" >> "$tmp/env"
  dc up -d --wait --wait-timeout 180 >/dev/null 2>&1 || fail "up --wait failed with MUSTER_SESSION_STATE='$v'"
  ss="$(dc logs --no-color muster 2>&1)"
  grep -q 'session state: OFF — MUSTER_SESSION_STATE is unset' <<<"$ss" ||
    fail "MUSTER_SESSION_STATE='$v' does not log session state OFF: $(grep 'session state:' <<<"$ss")"
  mv "$tmp/env.ss" "$tmp/env"
  echo "ok   MUSTER_SESSION_STATE='$v' in .env: session state: OFF"
done

# Negative control: the placeholder password, same file, the stack brought up again.
dc down -v --remove-orphans >/dev/null 2>&1
sed -i "s/^MUSTER_UI_PASSWORD=.*/MUSTER_UI_PASSWORD=CHANGE-ME/" "$tmp/env"
if dc up -d --wait --wait-timeout 180 >/dev/null 2>&1; then
  fail "up --wait SUCCEEDED with MUSTER_UI_PASSWORD=CHANGE-ME (the sign-in page is refused)"
fi
h="$(docker inspect -f '{{.State.Health.Status}}' "$(dc ps -q muster)")"
[ "$h" = unhealthy ] || fail "with the placeholder password muster is '$h', want unhealthy"
health="$(docker inspect -f '{{range .State.Health.Log}}{{.Output}}{{end}}' "$(dc ps -q muster)")"
grep -q 'MUSTER_UI_PASSWORD' <<<"$health" || fail "the health log does not name MUSTER_UI_PASSWORD"
echo "ok   placeholder password: up --wait fails, muster unhealthy, health log names MUSTER_UI_PASSWORD"

# Negative control: the placeholder HOOK token (the admin bearer), with a good
# UI password. muster must refuse to boot, naming the variable.
dc down -v --remove-orphans >/dev/null 2>&1
sed -i -e "s/^MUSTER_UI_PASSWORD=.*/MUSTER_UI_PASSWORD=$PASSWORD/" -e "s/^MUSTER_HOOK_TOKEN=.*/MUSTER_HOOK_TOKEN=CHANGE-ME/" "$tmp/env"
if dc up -d --wait --wait-timeout 90 >/dev/null 2>&1; then
  fail "up --wait SUCCEEDED with MUSTER_HOOK_TOKEN=CHANGE-ME (a public admin bearer)"
fi
logs="$(dc logs --no-color muster 2>&1)"
grep -q 'MUSTER_HOOK_TOKEN is the placeholder' <<<"$logs" ||
  fail "muster's log does not say MUSTER_HOOK_TOKEN is the placeholder"
echo "ok   placeholder hook token: muster refuses to boot, naming MUSTER_HOOK_TOKEN"

# The optional phases start from the good settings again, with a public URL
# this machine can open (the agent links are written on it).
dc down -v --remove-orphans >/dev/null 2>&1
sed -i "s/^MUSTER_HOOK_TOKEN=.*/MUSTER_HOOK_TOKEN=hook-token-fixture-$RANDOM/" "$tmp/env"
echo "MUSTER_PUBLIC_URL=$base" >> "$tmp/env"
cp "$tmp/env" "$tmp/env.core"

signin_cookie() {
  signin "$PASSWORD" | grep -i '^set-cookie:' | head -1 | sed -E 's/^[Ss]et-[Cc]ookie: ([^;]*).*/\1/'
}

phase_s3() {
  cp "$tmp/env.core" "$tmp/env"
  # Exactly the lines docs/features.md#attachments gives.
  { echo "COMPOSE_PROFILES=s3"
    echo "S3_STORE_SECRET_KEY=s3-secret-$RANDOM$RANDOM"
    echo "MUSTER_UPLOAD_SIGNING_KEY=signing-key-fixture-$RANDOM$RANDOM-0123456789abcdef"; } >> "$tmp/env"
  start=$(date +%s)
  dc up -d --wait --wait-timeout 300 || fail "[s3] docker compose up -d --wait did not succeed"
  echo "compose-up: [s3] --wait returned 0 after $(( $(date +%s) - start ))s"
  h="$(docker inspect -f '{{.State.Health.Status}}' "$(dc ps -q s3)")"
  [ "$h" = healthy ] || fail "[s3] the store's health is '$h'"
  logs="$(dc logs --no-color muster 2>&1)"
  grep -q 'file attachments: ON — files go to MUSTER_S3_ENDPOINT=http://s3:9000 bucket MUSTER_S3_BUCKET=muster-uploads' <<<"$logs" ||
    fail "[s3] muster does not log attachments ON on the bundled store: $(grep 'file attachments:' <<<"$logs")"
  echo "ok   [s3] file attachments: ON, on http://s3:9000 bucket muster-uploads"
  # muster reads the bucket's lifecycle once at boot. On MinIO the
  # incomplete-upload warning always appears (docs/uploads.md); the EXPIRY one
  # must not: the store's start script installed that rule. Only a boot that
  # lost the race with the store (it says so) cannot tell.
  if grep -q 'the bucket could not be reached at boot' <<<"$logs"; then
    echo "note [s3] muster booted before the store answered; the lifecycle was not read at boot"
  elif grep -q 'NO lifecycle rule expiring uploads/' <<<"$logs"; then
    fail "[s3] the bundled store's bucket has no expiry rule for uploads/"
  else
    grep -q 'NO lifecycle rule aborting incomplete multipart' <<<"$logs" ||
      fail "[s3] muster logged no lifecycle verdict at boot: $(grep 'uploads:' <<<"$logs" | head -3)"
    echo "ok   [s3] muster read the bucket at boot: its uploads/ expiry rule is in place"
  fi
  cookie="$(signin_cookie)"
  [ -n "$cookie" ] || fail "[s3] could not sign in"
  head -c 3000 /dev/urandom | base64 > "$tmp/upload.txt"
  reply="$(curl -fsS -H "Cookie: $cookie" -H "Origin: $base" -F "file=@$tmp/upload.txt;filename=notes.txt" "$base/uploads")" ||
    fail "[s3] POST /uploads failed"
  link="$(python3 -c 'import json,re,sys; m=re.search(r"(https?://\S+/f/[^ \x27\]]+)", json.loads(sys.argv[1])["line"]); print(m.group(1) if m else "")' "$reply")"
  case "$link" in "$base"/f/*) ;; *) fail "[s3] the upload's line has no signed link on $base: $reply" ;; esac
  curl -fsS -o "$tmp/download.txt" "$link" || fail "[s3] the signed link did not answer: $link"
  cmp -s "$tmp/upload.txt" "$tmp/download.txt" || fail "[s3] the file that came back through the link differs from the upload"
  echo "ok   [s3] a file uploaded through POST /uploads comes back byte for byte through the agent's signed link"
  bad="$(printf '%s' "$link" | sed -E 's/sig=([0-9A-Za-z_-])/sig=X\1/')"
  [ "$bad" != "$link" ] || fail "[s3] could not tamper with the link's signature: $link"
  code="$(curl -sS -o /dev/null -w '%{http_code}' "$bad")"
  [ "$code" = 404 ] || fail "[s3] a tampered signature answered $code, want 404"
  echo "ok   [s3] NEGATIVE CONTROL: a tampered signature is 404"
  dc --profile s3 down -v --remove-orphans >/dev/null 2>&1
}

phase_stt() {
  cp "$tmp/env.core" "$tmp/env"
  token="stt-token-$RANDOM$RANDOM"
  # Exactly the lines docs/features.md#dictation gives for the stt profile.
  { echo "COMPOSE_PROFILES=stt"; echo "MUSTER_STT_TOKEN=$token"; } >> "$tmp/env"
  start=$(date +%s)
  # The first start downloads the model (about 0.5 GB) before it is healthy.
  dc up -d --wait --wait-timeout 1500 || fail "[stt] docker compose up -d --wait did not succeed"
  echo "compose-up: [stt] --wait returned 0 after $(( $(date +%s) - start ))s"
  logs="$(dc logs --no-color muster 2>&1)"
  grep -q 'dictation: ON — MUSTER_STT_TOKEN is set; .*MUSTER_STT_URL=http://stt-whisper:8000, MUSTER_STT_MODEL: Systran/faster-whisper-small' <<<"$logs" ||
    fail "[stt] muster does not log dictation ON on the stt service: $(grep 'dictation:' <<<"$logs")"
  echo "ok   [stt] dictation: ON, on http://stt-whisper:8000 with Systran/faster-whisper-small"
  # The service checks the token muster presents: a wrong one is refused,
  # muster's is accepted.
  probe='import sys,urllib.request
try: print(urllib.request.urlopen(urllib.request.Request("http://127.0.0.1:8000/v1/models", headers={"Authorization": "Bearer " + sys.argv[1]}), timeout=30).status)
except urllib.error.HTTPError as e: print(e.code)'
  wrong="$(dc exec -T stt-whisper python -c "$probe" "not-$token")"; right="$(dc exec -T stt-whisper python -c "$probe" "$token")"
  case "$wrong" in 401|403) ;; *) fail "[stt] the STT service answered $wrong to a WRONG token" ;; esac
  [ "$right" = 200 ] || fail "[stt] the STT service answered $right to muster's token"
  echo "ok   [stt] NEGATIVE CONTROL: the STT service refuses a wrong token ($wrong) and accepts muster's (200)"
  cookie="$(signin_cookie)"
  [ -n "$cookie" ] || fail "[stt] could not sign in"
  espeak-ng -s 140 -w "$tmp/fox.wav" "The quick brown fox jumps over the lazy dog."
  espeak-ng -s 140 -w "$tmp/apples.wav" "Seven green apples are sitting on the kitchen table."
  transcribe() {
    curl -sS -w '\n%{http_code}' -H "Cookie: $cookie" -H "Origin: $base" -H 'Content-Type: audio/wav' \
      --data-binary "@$1" "$base/transcribe"
  }
  norm() { tr 'A-Z' 'a-z' | tr -c 'a-z\n' ' ' | tr -s ' '; }
  fox="$(transcribe "$tmp/fox.wav")"; apples="$(transcribe "$tmp/apples.wav")"
  [ "$(tail -n 1 <<<"$fox")" = 200 ] || fail "[stt] POST /transcribe answered: $fox"
  [ "$(tail -n 1 <<<"$apples")" = 200 ] || fail "[stt] POST /transcribe answered: $apples"
  fox_t="$(sed '$d' <<<"$fox" | norm)"; apples_t="$(sed '$d' <<<"$apples" | norm)"
  echo "compose-up: [stt] heard \"$fox_t\" and \"$apples_t\""
  grep -q 'quick brown fox' <<<"$fox_t" && grep -q 'lazy dog' <<<"$fox_t" ||
    fail "[stt] the fox clip was not transcribed as its words: \"$fox_t\""
  grep -q 'apples' <<<"$apples_t" && grep -q 'kitchen table' <<<"$apples_t" ||
    fail "[stt] the apples clip was not transcribed as its words: \"$apples_t\""
  if grep -q 'fox' <<<"$apples_t" || grep -q 'apples' <<<"$fox_t"; then
    fail "[stt] the two clips' transcripts share words: the answer does not follow the audio"
  fi
  echo "ok   [stt] two spoken clips come back through muster's POST /transcribe as their own words"
  dc --profile stt down -v --remove-orphans >/dev/null 2>&1
}

# --- the chief (profile chief) ------------------------------------------------
# docs/features.md#chief's lines, on the OpenRouter credential, with muster's
# LLM proxy pointed at a fake model (tests/compose-chief.override.yml) so the
# chief's real Claude Code CLI runs real turns and real muster tool calls with
# no key. CHIEF_IMAGE is a locally built claude-code-agent image
# (COMPOSE_UP_CHIEF_IMAGE, default claude-code-agent-local, tagged $TAG).
#
# COMPOSE_UP_CHIEF_BREAK=marker is the POSITIVE CONTROL OF THIS PHASE: it skips
# writing the task whose marker the tool result must carry, and the phase must
# then FAIL at the reply check. Run it by hand; it is how the check was seen to
# go red.
dcc() {
  FAKE_LLM_DIR="$ROOT/tests/fake-llm" docker compose -f "$ROOT/deploy/docker-compose.yml" \
    -f "$ROOT/tests/compose-chief.override.yml" --env-file "$tmp/env" -p "$PROJECT" --profile chief "$@"
}
hook() { sed -n 's/^MUSTER_HOOK_TOKEN=//p' "$tmp/env"; }
supervisor_status() { dcc exec -T chief curl -fsS http://127.0.0.1:18789/compose/status 2>/dev/null; }
status_field() { python3 -c 'import json,sys; print(json.loads(sys.stdin.read() or "{}").get(sys.argv[1], ""))' "$1"; }
fake_ledger() { dcc exec -T fake-llm python -c 'import urllib.request; print(urllib.request.urlopen("http://127.0.0.1:8080/ledger").read().decode())'; }

# What the chief's container shows when a turn does not end: its log, the
# CLI's screen, the hook's environment and the settings the CLI runs with.
chief_diagnostics() {
  echo "---- chief diagnostics ----"
  dcc logs --no-color --tail 60 chief 2>&1 || true
  echo "---- the CLI's screen (tmux cc) ----"
  dcc exec -T -u 1000:1000 chief tmux capture-pane -p -t cc 2>&1 | tail -40 || true
  echo "---- CCD_APPROVE_UNANSWERED / CCD_ALLOW_MCP_TOOLS in the claude process ----"
  dcc exec -T -u 1000:1000 chief sh -c 'for p in $(pgrep -u 1000 -x claude); do tr "\0" "\n" < /proc/$p/environ | grep -E "^CCD_(APPROVE_UNANSWERED|ALLOW_MCP_TOOLS)=" ; done' 2>&1 || true
  echo "---- permissions in the CLI's settings ----"
  dcc exec -T -u 1000:1000 chief sh -c 'grep -A12 "\"permissions\"" /data/claude/settings.json' 2>&1 | head -20 || true
  echo "---- fake model ledger ----"
  fake_ledger 2>&1 | tail -c 3000 || true
}

phase_chief() {
  local chief_image="${COMPOSE_UP_CHIEF_IMAGE:-claude-code-agent-local}"
  docker image inspect "$chief_image:$TAG" >/dev/null 2>&1 || fail "[chief] no local image $chief_image:$TAG (docker build -f images/claude-code-agent/Dockerfile -t $chief_image:$TAG .)"
  cp "$tmp/env.core" "$tmp/env"
  local orkey="sk-or-v1-FAKE-compose-smoke-$RANDOM$RANDOM"
  # Exactly the lines docs/features.md#chief gives (the OpenRouter credential),
  # and the local image.
  { echo "CHIEF_IMAGE=$chief_image"
    echo "MUSTER_CHIEF_RUNTIME=compose"
    echo "MUSTER_CONNECTOR_ENCRYPTION_KEY=$(python3 -c 'import base64,os; print(base64.b64encode(os.urandom(32)).decode())')"
    echo "MUSTER_AGENT_OPENROUTER_KEY=$orkey"; } >> "$tmp/env"
  dcc up -d --wait --wait-timeout 240 || fail "[chief] up --wait did not succeed"
  for svc in chief fake-llm; do
    h="$(docker inspect -f '{{.State.Health.Status}}' "$(dcc ps -q "$svc")")"
    [ "$h" = healthy ] || fail "[chief] $svc health is '$h'"
  done
  local st; st="$(supervisor_status)"
  [ "$(status_field state <<<"$st")" = awaiting_spec ] || fail "[chief] before it is turned on the supervisor reads $st, want awaiting_spec"
  echo "ok   [chief] up --wait succeeds with the chief off: its container is healthy and awaiting its spec"
  boot="$(dcc logs --no-color muster 2>&1)"
  grep -q 'chief runtime: COMPOSE (MUSTER_CHIEF_RUNTIME=compose)' <<<"$boot" || fail "[chief] muster logged no chief runtime line"

  # Its network: muster yes; Postgres and the fake model no (the model only
  # through muster's proxy).
  dcc exec -T chief timeout 5 bash -c '</dev/tcp/muster/8105' 2>/dev/null || fail "[chief] the chief cannot reach muster:8105 (positive control)"
  if dcc exec -T chief timeout 5 bash -c '</dev/tcp/postgres/5432' 2>/dev/null; then fail "[chief] the chief reaches postgres:5432"; fi
  if dcc exec -T chief timeout 5 bash -c '</dev/tcp/fake-llm/8080' 2>/dev/null; then fail "[chief] the chief reaches the model directly, not through muster"; fi
  echo "ok   [chief] the chief reaches muster, and neither Postgres nor the model directly"

  cookie="$(signin_cookie)"
  [ -n "$cookie" ] || fail "[chief] could not sign in"
  account="$(curl -fsS -H "Cookie: $cookie" "$base/account")" || fail "[chief] /account"
  grep -q 'id="chief-toggle"' <<<"$account" || fail "[chief] the admin's Account page draws no chief switch"
  grep -qF "Runs on your OpenRouter key, on deepseek/deepseek-v4.1-flash" <<<"$account" ||
    fail "[chief] the Account page does not name the OpenRouter credential and model"
  grep -qF "https://openrouter.ai/settings/keys" <<<"$account" || fail "[chief] the Account page does not link the key's credit limit"
  echo "ok   [chief] Account → Chief: a switch, the OpenRouter credential, its model, and the key's limit"

  # The board holds a task whose title carries a marker nothing else holds.
  local marker="MARK-$RANDOM$RANDOM$RANDOM"
  if [ "${COMPOSE_UP_CHIEF_BREAK:-}" != marker ]; then
    task="$(curl -fsS -H "Authorization: Bearer $(hook)" -H 'Content-Type: application/json' \
      -d "{\"title\":\"$marker is on the compose smoke\",\"body\":\"the chief smoke test\"}" "$base/api/tasks")" ||
      fail "[chief] could not file the marker task"
    task_id="$(python3 -c 'import json,sys; d=json.loads(sys.argv[1]); print(d.get("id") or d.get("task", {}).get("id") or "")' "$task")"
    [ -n "$task_id" ] || fail "[chief] the task API answered no id: $task"
  fi

  # The admin turns the chief on.
  loc="$(curl -sS -o /dev/null -D - -H "Cookie: $cookie" -H "Origin: $base" --data 'on=1' "$base/account/chief" | tr -d '\r' | sed -n 's/^[Ll]ocation: //p')"
  case "$loc" in *chief=saved*) ;; *) fail "[chief] turning the chief on answered Location: $loc" ;; esac
  local deadline=$(( $(date +%s) + 300 ))
  until st="$(supervisor_status)" && [ "$(status_field state <<<"$st")" = running ] && [ "$(status_field ready <<<"$st")" = True ]; do
    [ "$(date +%s)" -lt "$deadline" ] || fail "[chief] the chief never became running and ready: $st"
    sleep 3
  done
  echo "ok   [chief] turned on: the supervisor runs the chief, ready ($(status_field generation <<<"$st"))"

  # The session cannot read its spec (its turn bearer); its instructions it can.
  if dcc exec -T -u 1000:1000 chief cat /run/muster-spec/chief/spec.json >/dev/null 2>&1; then
    fail "[chief] uid 1000 (the agent) reads spec.json"
  fi
  dcc exec -T -u 1000:1000 chief cat /etc/claude-code/CLAUDE.md 2>/dev/null | grep -q '^# muster supervisor' ||
    fail "[chief] uid 1000 cannot read the managed CLAUDE.md"
  dcc exec -T chief cat /run/muster-spec/chief/spec.json >/dev/null 2>&1 || fail "[chief] the supervisor cannot read spec.json (positive control)"
  echo "ok   [chief] the agent reads its CLAUDE.md and not its spec; the supervisor reads the spec"

  # The loop: muster → the chief → the CLI → muster's proxy → the fake model →
  # a tool call over MCP to muster → real board data → the model → the reply.
  local reply="" tries=0
  while :; do
    reply="$(curl -sS -m 180 -H "Authorization: Bearer $(hook)" -H 'Content-Type: application/json' \
      -d '{"message":"Who is on what right now?"}' "$base/api/agents/chief/messages")"
    grep -q '"reply"' <<<"$reply" && break
    tries=$((tries + 1)); [ "$tries" -lt 6 ] || fail "[chief] the chief never answered: $reply"
    sleep 10
  done
  # ⚠ THE CHECK, AND ITS OWN CONTROL. reply_names prints whether the reply
  # carries a marker; it is shown to say no for one nobody wrote, so a check
  # that matched anything could not pass here.
  reply_names() { python3 -c 'import json,sys; r=json.loads(sys.argv[1]).get("reply",""); sys.exit(0 if ("SEEN:" in r and sys.argv[2] in r) else 1)' "$1" "$2"; }
  if reply_names "$reply" "MARK-nobodywrotethis"; then fail "[chief] the reply check matches a marker nobody wrote"; fi
  reply_names "$reply" "$marker" || fail "[chief] the chief's reply does not carry the marker from the real tool result: $reply"
  echo "ok   [chief] the chief's reply carries $marker, which only the board held: muster's tool result went through"

  # The muster tools the model is offered are exactly the chief's pre-allowed
  # ones (agentspec.ComposeChiefAllowedMCPTools, as muster serves them here):
  # never task_comment, task_set_status, … — tools it could not run.
  local led
  led="$(fake_ledger)" || fail "[chief] could not read the fake model's ledger"
  # EQUAL, not a subset: an allowed tool that stopped being served is red too.
  # This stack sets no MUSTER_SESSION_ROUTING, so route_to_session is not
  # served; it is expected exactly when the env file turns routing on.
  local routing=0
  grep -qx 'MUSTER_SESSION_ROUTING=1' "$tmp/env" && routing=1
  python3 - "$led" "$routing" <<'PY' || fail "[chief] the chief's offered muster tools are not exactly its pre-allowed set: $led"
import json, sys
expected = {"sessions_overview", "fleet_list", "focus_search", "fleet_report", "tasks_list", "task_get", "attention_list"}
if sys.argv[2] == "1":
    expected.add("route_to_session")
offered = {n[len("mcp__muster__"):] for r in json.loads(sys.argv[1]) for n in r.get("tool_names") or []
           if n.startswith("mcp__muster__")}
assert offered == expected, f"offered {sorted(offered)}, want exactly {sorted(expected)} (extra {sorted(offered - expected)}, missing {sorted(expected - offered)})"
print(f"ok   [chief] the model is offered exactly the pre-allowed muster tools this stack serves: {sorted(offered)}")
PY

  # task_comment, which it is not offered, named anyway (the fake does): the
  # CLI refuses it, the turn ends, and it does not run.
  reply="$(curl -sS -m 150 -H "Authorization: Bearer $(hook)" -H 'Content-Type: application/json' \
    -d "{\"message\":\"Now comment on it, please. TASK-${task_id:-0}\"}" "$base/api/agents/chief/messages")" || {
    chief_diagnostics
    fail "[chief] a turn naming an unoffered tool did not end within 150s: ${reply:-no reply}"
  }
  python3 -c 'import json,sys; r=json.loads(sys.argv[1]).get("reply",""); sys.exit(0 if r.startswith("COMMENT-RESULT:error:") else 1)' "$reply" ||
    fail "[chief] a call to the unoffered task_comment did not end in an error: $reply"
  # And it did not run: the task holds no comment from it. (The call was a
  # valid task_comment on the marker task, so a run would have left one.)
  if [ -n "${task_id:-}" ]; then
    got="$(curl -fsS -H "Authorization: Bearer $(hook)" "$base/api/tasks/$task_id")" || fail "[chief] GET /api/tasks/$task_id"
    if grep -q 'fake-llm comment that must never land' <<<"$got"; then fail "[chief] the unoffered task_comment ran: $got"; fi
  fi
  echo "ok   [chief] the unoffered task_comment, called anyway, is refused and leaves no comment"

  # An offered tool the chief does NOT pre-allow: its prompt is denied at once
  # (nobody can answer one in this container), so the turn ends, saying so,
  # instead of hanging. The reply is the fake's report of the error tool result.
  reply="$(curl -sS -m 150 -H "Authorization: Bearer $(hook)" -H 'Content-Type: application/json' \
    -d '{"message":"Now touch the file, please."}' "$base/api/agents/chief/messages")" || {
    chief_diagnostics
    fail "[chief] an un-allowed tool's turn did not end within 150s (a prompt left hanging?): ${reply:-no reply}"
  }
  python3 -c 'import json,sys; r=json.loads(sys.argv[1]).get("reply",""); sys.exit(0 if r.startswith("BASH-RESULT:error:") and "deni" in r.lower() else 1)' "$reply" ||
    fail "[chief] an un-allowed tool's turn did not end denied: $reply"
  if dcc exec -T chief test -e /tmp/fake-llm-touch-must-never-land; then fail "[chief] the denied Bash touch ran"; fi
  echo "ok   [chief] an un-allowed tool (Bash touch) is denied and the turn ends, not hangs"

  # The provider saw only the pinned model and the owner's key.
  led="$(fake_ledger)" || fail "[chief] could not read the fake model's ledger"
  python3 - "$led" "$orkey" <<'PY' || fail "[chief] the fake model's ledger: $led"
import json, sys
led, key = json.loads(sys.argv[1]), sys.argv[2]
msgs = [r for r in led if r.get("path", "").startswith("/v1/messages")]
assert msgs, "no /v1/messages request reached the provider"
assert not [r for r in led if r.get("unexpected")], f"unexpected paths: {[r['path'] for r in led if r.get('unexpected')]}"
assert all(r["model"] == "deepseek/deepseek-v4.1-flash" for r in msgs), sorted({r["model"] for r in msgs})
assert all(r["authorization"] == "Bearer " + key and not r["x_api_key"] for r in msgs), "a request carried another credential"
assert any(r["tool_result"] and r["answered"].startswith("text:SEEN:") for r in msgs), "no request carried the tool result"
print(f"ok   [chief] {len(msgs)} model request(s), each pinned to deepseek/deepseek-v4.1-flash and carrying the owner's key only")
PY

  # Off idles it: the CLI stops, the container stays, and the model hears nothing more.
  loc="$(curl -sS -o /dev/null -D - -H "Cookie: $cookie" -H "Origin: $base" --data 'on=0' "$base/account/chief" | tr -d '\r' | sed -n 's/^[Ll]ocation: //p')"
  case "$loc" in *chief=saved*) ;; *) fail "[chief] turning the chief off answered Location: $loc" ;; esac
  deadline=$(( $(date +%s) + 90 ))
  until st="$(supervisor_status)" && [ "$(status_field state <<<"$st")" = off ]; do
    [ "$(date +%s)" -lt "$deadline" ] || fail "[chief] the chief did not stop: $st"
    sleep 2
  done
  if left="$(dcc exec -T chief pgrep -a -u 1000 -f claude 2>/dev/null)"; then
    # What is left, whose child it is, what the supervisor and muster say.
    echo "---- uid 1000 processes matching claude ----"; echo "$left"
    echo "---- every process (pid ppid user args) ----"; dcc exec -T chief ps -eo pid=,ppid=,user=,args= 2>&1 || true
    echo "---- supervisor status ----"; supervisor_status || true
    echo "---- spec.json generation/replicas ----"
    # Those three fields only: the spec also holds the turn bearer.
    dcc exec -T chief grep -oE '"(generation|replicas|epoch)":[^,]*' /run/muster-spec/chief/spec.json 2>&1 || true
    chief_diagnostics
    fail "[chief] a claude process is still running after Off: $left"
  fi
  local n1 n2
  n1="$(fake_ledger | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))')"
  sleep 15
  n2="$(fake_ledger | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))')"
  [ "$n1" = "$n2" ] || fail "[chief] the model was sent $((n2 - n1)) request(s) after Off"
  [ "$(docker inspect -f '{{.State.Running}}' "$(dcc ps -q chief)")" = true ] || fail "[chief] Off stopped the container"
  echo "ok   [chief] Off: the supervisor reads off, no claude process, no model request in 15s, the container still up"

  # Removing it, exactly as docs/chief.md "Removing it" says (step 3, the .env
  # edit, is this script's next phase starting from env.core).
  dcc rm -sf chief chief-init >/dev/null 2>&1 || fail "[chief] docker compose --profile chief rm -sf chief chief-init failed"
  [ -z "$(dcc ps -a -q chief chief-init 2>/dev/null)" ] || fail "[chief] the chief's containers are still there after rm -sf"
  dcc down >/dev/null 2>&1
  local v
  for v in chief-spec chief-instructions chief-data; do
    docker volume inspect "${PROJECT}_$v" >/dev/null 2>&1 || fail "[chief] there is no volume ${PROJECT}_$v to remove (the docs name it)"
  done
  docker volume rm "${PROJECT}_chief-spec" "${PROJECT}_chief-instructions" "${PROJECT}_chief-data" >/dev/null ||
    fail "[chief] the documented docker volume rm failed"
  for v in chief-spec chief-instructions chief-data; do
    if docker volume inspect "${PROJECT}_$v" >/dev/null 2>&1; then fail "[chief] ${PROJECT}_$v survived its removal"; fi
  done
  echo "ok   [chief] removed as docs/chief.md says: rm -sf chief chief-init, then down and the three volumes"
  dcc down -v --remove-orphans >/dev/null 2>&1
}

for phase in $PHASES; do
  case "$phase" in
    s3) phase_s3 ;;
    stt) phase_stt ;;
    chief) phase_chief ;;
    *) echo "unknown phase '$phase' in COMPOSE_UP_PROFILES (s3, stt, chief)" >&2; exit 2 ;;
  esac
done
echo "compose-up: PASS (optional phases: ${PHASES:-none})"

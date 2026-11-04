#!/usr/bin/env bash
# Bring the production compose stack up for real and prove muster serves.
#
#   tests/compose-up.sh <image-repo> <tag>     e.g. muster-local 1.2.3
#   make compose-up-test IMAGE=muster-local VERSION=1.2.3
#   COMPOSE_UP_PROFILES="s3" tests/compose-up.sh …   (which optional phases run;
#                                                     default "s3 stt", "" for none)
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
#   * profile s3 (COMPOSE_PROFILES=s3 + MUSTER_S3_SECRET_KEY +
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
# Always tears down (`down -v`, every profile), including on failure.
set -euo pipefail

[ $# -eq 2 ] || { echo "usage: $0 <image-repo> <tag>" >&2; exit 2; }
IMAGE="$1"; TAG="$2"
# CDPATH unset: with it exported, `cd` to a relative path PRINTS the directory,
# and ROOT became two lines (measured: every `make` target here broke).
ROOT="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
docker image inspect "$IMAGE:$TAG" >/dev/null 2>&1 || { echo "no local image $IMAGE:$TAG" >&2; exit 2; }
PHASES="${COMPOSE_UP_PROFILES-s3 stt}"
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
cleanup() { dc --profile stt --profile s3 down -v --remove-orphans >/dev/null 2>&1 || true; rm -rf "$tmp"; }
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
    echo "MUSTER_S3_SECRET_KEY=s3-secret-$RANDOM$RANDOM"
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

for phase in $PHASES; do
  case "$phase" in
    s3) phase_s3 ;;
    stt) phase_stt ;;
    *) echo "unknown phase '$phase' in COMPOSE_UP_PROFILES (s3, stt)" >&2; exit 2 ;;
  esac
done
echo "compose-up: PASS (optional phases: ${PHASES:-none})"

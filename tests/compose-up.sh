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
# FIRST, THE ZERO-CONFIG STACK (the local quickstart): the compose file with NO
# .env at all — only MUSTER_IMAGE/MUSTER_VERSION/MUSTER_HTTP_PORT in the
# process environment, which point it at the local image and a free port.
# Asserts:
#   * `up --wait` returns 0 and /readyz is 200;
#   * muster-init printed a generated sign-in password exactly once, and
#     `exec muster /muster-server show-secret ui-password` shows the same one;
#     it signs in (a wrong one does not); the generated hook token opens
#     /api/doctor; push is ON with the generated keypair and the subject
#     mailto:muster@localhost (never the http URL);
#   * the session cookie of a sign-in from the http://localhost page (its
#     Origin) is NOT Secure, and one without an Origin (curl) IS (the
#     MUSTER_SECURE_COOKIES=1 default);
#   * `down` + `up` (the volume kept) keeps the password, and muster-init then
#     prints none;
#   * THE DATABASE PASSWORD IS NEVER OVERWRITTEN (#707 round 0, F1): with the
#     stack up, an .env setting a DIFFERENT POSTGRES_PASSWORD makes `up --wait`
#     fail on muster-init, quickly, naming POSTGRES_PASSWORD and ALTER USER,
#     and EVERY file on the secrets volume is byte-identical afterwards
#     (sha256, read straight off the volume); then `up` without that .env is
#     healthy and the password still signs in (muster still reaches its
#     database). Before the fix the file was overwritten and the database
#     login broke for good;
#   * with COMPOSE_UP_MIRROR=<muster-mirror binary> (and tmux): a mirror
#     pointed at http://localhost:<port> with NO -allow-http connects, the
#     server sees the host online, and a tmux session named with a fresh
#     marker appears in the signed-in sidebar; NEGATIVE CONTROL: the same
#     binary refuses http to a non-loopback address, naming cleartext;
#   * with COMPOSE_UP_BROWSERS=1: e2e/localhost's Playwright projects
#     (Chromium and WebKit) sign in on http://localhost:<port>, and each
#     browser sends its session cookie back (a session-tier route opens).
# Then the .env stack below, which is an install from BEFORE generated
# secrets: it must boot exactly as it did (its .env's password, token and push
# keys; muster-init generates nothing for it).
# And (#707 round 2, F3) that install's first new-compose run with its
# POSTGRES_PASSWORD left out is refused, writing nothing, and the line put
# back is adopted.
#
# Asserts, after `docker compose up -d --wait` returned 0:
#   * both containers report health "healthy";
#   * GET /health is 200 and reports the image's version;
#   * GET /readyz is 200 (database reachable, wiring sound);
#   * GET /login serves the sign-in page;
#   * the shared password signs in (POST /login sets a session cookie);
#   * NEGATIVE CONTROL: a wrong password does not;
#   * NEGATIVE CONTROL: a second stack whose MUSTER_UI_PASSWORD is an unedited
#     placeholder (CHANGE-ME, under 12 characters) must FAIL `up --wait` on
#     muster-init, naming MUSTER_UI_PASSWORD, with muster never started (the
#     server itself would boot and refuse every browser page; its healthcheck's
#     /login probe stays as the second line of defence);
#   * NEGATIVE CONTROL: MUSTER_HOOK_TOKEN=CHANGE-ME (a public admin bearer)
#     must make muster refuse to boot — muster-init refuses it, naming it;
#   * FAIL FAST: a setting the server refuses at boot (an http push subject)
#     makes `up --wait` fail within COMPOSE_UP_FAIL_FAST_BOUND seconds (45),
#     muster-init's log ends with the server's `config:` refusal naming the
#     variable, and muster itself never started;
#   * muster did not restart on the fresh volume (the Postgres init race);
#   * the .env install gets NO generated secret (the secrets volume holds
#     only Postgres's password and DATABASE_URL), and muster-init says it
#     generated nothing;
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
# sha256 of an empty file: muster-init's lock file on the secrets volume.
EMPTY_SHA256=e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
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
trap 'zero_cleanup; cleanup' EXIT

fail() { echo "FAIL: $1"; dc ps -a || true; dc logs --no-color --tail 40 muster || true; exit 1; }

echo "compose-up: project $PROJECT, port $PORT, image $IMAGE:$TAG"

# --- the zero-config stack: no .env at all -----------------------------------
# dz: the compose file as a clone runs it. `env -i` so nothing of this shell
# reaches it but the three test-harness lines (and docker's own variables);
# --env-file /dev/null so no deploy/.env, even one a developer has, is read
# (ZENV names another, for the database-password phase).
dz() {
  env -i PATH="$PATH" HOME="$HOME" ${DOCKER_HOST:+DOCKER_HOST="$DOCKER_HOST"} \
    MUSTER_IMAGE="$IMAGE" MUSTER_VERSION="$TAG" MUSTER_HTTP_PORT="$PORT" \
    docker compose -f "$ROOT/deploy/docker-compose.yml" --env-file "${ZENV:-/dev/null}" -p "$PROJECT-zero" "$@"
}
# Every file on the zero-config stack's secrets volume with its sha256, read
# straight off the volume (whatever state the stack's containers are in).
zvolume() {
  docker run --rm --network none -v "$PROJECT-zero_secrets:/s:ro" --entrypoint sh postgres:16-alpine \
    -c 'cd /s && ls -A | LC_ALL=C sort | while read -r f; do sha256sum "$f"; done'
}
zfail() { echo "FAIL [zero-config]: $1"; dz ps -a || true; dz logs --no-color --tail 40 muster-init muster || true; exit 1; }
zero_cleanup() { dz down -v --remove-orphans >/dev/null 2>&1 || true; }
phase_zero() {
  local base="http://localhost:$PORT" start
  start=$(date +%s)
  dz up -d --wait --wait-timeout 180 || zfail "docker compose up -d --wait with no .env did not succeed"
  echo "compose-up: [zero-config] --wait returned 0 after $(( $(date +%s) - start ))s, no .env"
  [ "$(curl -sS -o /dev/null -w '%{http_code}' "$base/readyz")" = 200 ] || zfail "/readyz is not 200"
  echo "ok   [zero-config] /readyz: 200"
  local init pw shown
  init="$(dz logs --no-color muster-init 2>&1)"
  pw="$(dz exec -T muster /muster-server show-secret ui-password)" || zfail "show-secret ui-password failed"
  [ "${#pw}" -ge 12 ] || zfail "the generated password is ${#pw} characters"
  shown="$(grep -c -F -- "$pw" <<<"$init" || true)"
  [ "$shown" = 1 ] || zfail "muster-init printed the password $shown time(s), want exactly once"
  grep -q 'show-secret ui-password' <<<"$init" || zfail "muster-init does not say how to show the password again"
  echo "ok   [zero-config] muster-init printed the generated password once; show-secret shows the same one"
  local login
  login="$(curl -sS -o /dev/null -D - -X POST -H "Origin: $base" --data-urlencode "password=$pw" "$base/login" | tr -d '\r')"
  grep -qi '^set-cookie: muster_session=' <<<"$login" || zfail "the generated password does not sign in: $login"
  if grep -i '^set-cookie: muster_session=' <<<"$login" | grep -qi '; *secure'; then
    zfail "a sign-in from the http://localhost page got a Secure cookie (WebKit drops it): $login"
  fi
  local curlish
  curlish="$(curl -sS -o /dev/null -D - -X POST --data-urlencode "password=$pw" "$base/login" | tr -d '\r')"
  grep -i '^set-cookie: muster_session=' <<<"$curlish" | grep -qi '; *secure' ||
    zfail "a sign-in with no Origin got a cookie that is not Secure (the MUSTER_SECURE_COOKIES=1 default): $curlish"
  if curl -sS -o /dev/null -D - -X POST -H "Origin: $base" --data-urlencode "password=not-$pw" "$base/login" | grep -qi '^set-cookie: muster_session='; then
    zfail "a WRONG password signed in"
  fi
  echo "ok   [zero-config] it signs in (a wrong one does not); the localhost page's cookie is not Secure, curl's is"
  local hook
  hook="$(dz exec -T muster /muster-server show-secret hook-token)" || zfail "show-secret hook-token failed"
  [ "$(curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $hook" "$base/api/doctor")" = 200 ] ||
    zfail "the generated hook token does not open /api/doctor"
  [ "$(curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer not-$hook" "$base/api/doctor")" != 200 ] ||
    zfail "a WRONG hook token opens /api/doctor"
  echo "ok   [zero-config] the generated hook token opens /api/doctor (a wrong one does not)"
  local boot cookie key
  boot="$(dz logs --no-color muster 2>&1)"
  grep -q 'web push: ENABLED .*(subject "mailto:muster@localhost")' <<<"$boot" ||
    zfail "push is not ON with the mailto: subject: $(grep 'web push:' <<<"$boot")"
  cookie="$(grep -i '^set-cookie: muster_session=' <<<"$login" | head -1 | sed -E 's/^[Ss]et-[Cc]ookie: ([^;]*).*/\1/')"
  key="$(curl -fsS -H "Cookie: $cookie" "$base/api/push/vapid-public-key")" || zfail "the push key route did not answer"
  grep -q '"key":"B' <<<"$key" || zfail "the push key route served $key"
  echo "ok   [zero-config] push is ON with the generated keypair, subject mailto:muster@localhost"

  # The mirror, on loopback http with no flag.
  if [ -n "${COMPOSE_UP_MIRROR:-}" ]; then
    command -v tmux >/dev/null 2>&1 || zfail "COMPOSE_UP_MIRROR is set but tmux is not installed"
    local sock="cu-$$-$RANDOM" marker="cu-mark-$RANDOM$RANDOM" mtok="$tmp/mirror-token" mpid i nav out
    (umask 077; dz exec -T muster /muster-server mirror token create cu-host > "$mtok") || zfail "could not mint a mirror token"
    tmux -L "$sock" new-session -d -s "$marker" 'sleep 600'
    env -u MUSTER_MIRROR_ALLOW_HTTP "$COMPOSE_UP_MIRROR" -url "$base" -token-file "$mtok" -host cu-host -tmux-socket "$sock" \
      > "$tmp/mirror.log" 2>&1 &
    mpid=$!
    i=0
    until curl -fsS -H "Authorization: Bearer $(cat "$mtok")" "$base/mirror/host" 2>/dev/null | grep -q '"online":true'; do
      i=$((i + 1))
      if [ "$i" -gt 30 ] || ! kill -0 "$mpid" 2>/dev/null; then
        cat "$tmp/mirror.log"; kill "$mpid" 2>/dev/null; tmux -L "$sock" kill-server 2>/dev/null
        zfail "muster-mirror on $base (no -allow-http) did not connect"
      fi
      sleep 1
    done
    i=0
    until nav="$(curl -fsS -H "Cookie: $cookie" "$base/ui/nav/agents")" && grep -q -- "$marker" <<<"$nav"; do
      i=$((i + 1))
      if [ "$i" -gt 20 ]; then
        kill "$mpid" 2>/dev/null; tmux -L "$sock" kill-server 2>/dev/null
        zfail "the mirrored tmux session $marker never appeared in the sidebar"
      fi
      sleep 1
    done
    kill "$mpid" 2>/dev/null; wait "$mpid" 2>/dev/null || true; tmux -L "$sock" kill-server 2>/dev/null || true
    echo "ok   [zero-config] muster-mirror on $base with no -allow-http connected; tmux session $marker is in the sidebar"
    # NEGATIVE CONTROL: plain http to another machine (TEST-NET-1) is refused.
    out="$(env -u MUSTER_MIRROR_ALLOW_HTTP timeout 10 "$COMPOSE_UP_MIRROR" -url http://192.0.2.10:8105 -token-file "$mtok" -host cu-host 2>&1)" &&
      zfail "muster-mirror ran against http://192.0.2.10 with no -allow-http"
    grep -q cleartext <<<"$out" || zfail "muster-mirror's refusal of http://192.0.2.10 does not say cleartext: $out"
    echo "ok   [zero-config] NEGATIVE CONTROL: muster-mirror refuses http to a non-loopback address, naming cleartext"
  else
    echo "note [zero-config] the mirror check did not run (set COMPOSE_UP_MIRROR to a muster-mirror binary; CI does)"
  fi

  # Real browsers: Chromium AND WebKit sign in on http://localhost.
  if [ "${COMPOSE_UP_BROWSERS:-}" = 1 ]; then
    MUSTER_LOCAL_URL="$base" MUSTER_LOCAL_PASSWORD="$pw" \
      npx --prefix "$ROOT/e2e" playwright test -c "$ROOT/e2e/localhost/playwright.config.ts" --reporter=list ||
      zfail "the Playwright sign-in on $base failed (Chromium or WebKit; output above)"
    echo "ok   [zero-config] Chromium and WebKit sign in on $base, and each browser sends its session cookie back"
  else
    echo "note [zero-config] the browser check did not run (COMPOSE_UP_BROWSERS=1; CI does)"
  fi

  # Kept across a restart: the same password, and none printed again.
  dz down --remove-orphans >/dev/null 2>&1
  dz up -d --wait --wait-timeout 180 >/dev/null 2>&1 || zfail "the second up --wait (volumes kept) failed"
  [ "$(dz exec -T muster /muster-server show-secret ui-password)" = "$pw" ] || zfail "the password changed across a restart"
  if dz logs --no-color muster-init 2>&1 | grep -q 'generated'; then zfail "muster-init generated something on the second start"; fi
  curl -fsS -o /dev/null -X POST -H "Origin: $base" --data-urlencode "password=$pw" "$base/login" || zfail "the kept password no longer signs in"
  echo "ok   [zero-config] down + up keeps the password, and muster-init generates and prints nothing"

  # The database password is never overwritten from .env (F1).
  local before after took init_log
  before="$(zvolume)"
  # 6 secrets + muster-init's lock file (#707 round 3), which is always empty
  grep -q 'postgres_password' <<<"$before" && [ "$(grep -vc '  \.lock$' <<<"$before")" = 6 ] &&
    grep -qx "$EMPTY_SHA256  .lock" <<<"$before" ||
    zfail "positive control: the secrets volume read as: $before"
  printf 'POSTGRES_PASSWORD=pg-from-an-env-%s\n' "$RANDOM$RANDOM" > "$tmp/zero.env"
  start=$(date +%s)
  if ZENV="$tmp/zero.env" dz up -d --wait --wait-timeout 120 >/dev/null 2>&1; then
    zfail "up --wait SUCCEEDED with a POSTGRES_PASSWORD that is not the database's"
  fi
  took=$(( $(date +%s) - start ))
  init_log="$(ZENV="$tmp/zero.env" dz logs --no-color muster-init 2>&1 | tail -n 15)"
  grep -q 'config: POSTGRES_PASSWORD differs from the database' <<<"$init_log" && grep -q 'ALTER USER' <<<"$init_log" ||
    zfail "muster-init's refusal does not name POSTGRES_PASSWORD and ALTER USER: $init_log"
  after="$(zvolume)"
  [ "$after" = "$before" ] || zfail "the refused run CHANGED the secrets volume:
before: $before
after:  $after"
  [ "$took" -le 45 ] || zfail "the refusal took ${took}s, over the 45s bound"
  echo "ok   [zero-config] a different POSTGRES_PASSWORD in .env is refused in ${took}s on muster-init, naming the way out; every secret file is byte-identical"
  rm -f "$tmp/zero.env"
  dz up -d --wait --wait-timeout 180 >/dev/null 2>&1 || zfail "up --wait WITHOUT that .env failed (the database login broke)"
  [ "$(curl -sS -o /dev/null -w '%{http_code}' "$base/readyz")" = 200 ] || zfail "/readyz is not 200 after the .env was removed"
  curl -fsS -o /dev/null -X POST -H "Origin: $base" --data-urlencode "password=$pw" "$base/login" || zfail "the password no longer signs in after the .env was removed"
  [ "$(zvolume)" = "$before" ] || zfail "the volume changed when the stack came back up without the .env"
  echo "ok   [zero-config] without that .env the stack is healthy again on the same database password"
  zero_cleanup
}
phase_zero

# --- the .env stack: an install from before generated secrets ---------------
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
# An .env install from before generated secrets: nothing is generated for it.
held="$(dc exec -T postgres ls /run/muster-secrets | LC_ALL=C sort | tr '\n' ' ')"
[ "$held" = "database_url postgres_password " ] ||
  fail "the .env install's secrets volume holds '$held', want only database_url and postgres_password (nothing generated)"
if dc logs --no-color muster-init 2>&1 | grep -q generated; then fail "muster-init generated something for an .env install"; fi
echo "ok   .env install: muster-init generated nothing (the volume holds only Postgres's password and DATABASE_URL)"

# AN OLD INSTALL'S FIRST NEW-COMPOSE RUN WITHOUT ITS POSTGRES_PASSWORD (#707
# round 2, F3). An install from before muster-init has a database and NO kept
# password; emptying the secrets volume makes this stack exactly that. Its
# .env then left without POSTGRES_PASSWORD: `up --wait` must fail on
# muster-init, naming the existing database and the line to put back, and
# write NOTHING (before the fix it generated a password the database never
# had). The line put back: healthy, the password kept, and it signs in.
dc stop muster >/dev/null 2>&1 || fail "could not stop muster"
docker run --rm --network none -v "${PROJECT}_secrets:/s" --entrypoint sh postgres:16-alpine -c 'rm -f /s/postgres_password /s/database_url' ||
  fail "could not empty the secrets volume"
grep -v '^POSTGRES_PASSWORD=' "$tmp/env" > "$tmp/env.nopg"
dcn() { docker compose -f "$ROOT/deploy/docker-compose.yml" --env-file "$tmp/env.nopg" -p "$PROJECT" "$@"; }
if dcn up -d --wait --wait-timeout 120 >/dev/null 2>&1; then
  fail "an existing database with no kept password and no POSTGRES_PASSWORD came up (a generated password was used)"
fi
init_log="$(dcn logs --no-color muster-init 2>&1 | tail -n 20)"
grep -q 'config: POSTGRES_PASSWORD is not set, and the existing database needs the password it was created with' <<<"$init_log" &&
  grep -q 'Put that line back in .env' <<<"$init_log" && grep -q 'Lost it? Give the database a new one, in this order:' <<<"$init_log" ||
  fail "muster-init's refusal does not name the existing database, the line to put back and the lost-it steps: $init_log"
# Nothing but muster-init's lock file, empty (it predates this run: the first up made it).
held="$(docker run --rm --network none -v "${PROJECT}_secrets:/s:ro" --entrypoint sh postgres:16-alpine -c 'cd /s && ls -A | LC_ALL=C sort | while read -r f; do sha256sum "$f"; done')"
[ "$held" = "$EMPTY_SHA256  .lock" ] || fail "the refused run wrote to the secrets volume (want only the empty .lock): $held"
echo "ok   .env install, first new-compose run without POSTGRES_PASSWORD: refused on muster-init, naming the line to put back; nothing written"
dc up -d --wait --wait-timeout 180 >/dev/null 2>&1 || fail "with POSTGRES_PASSWORD put back, up --wait failed"
[ "$(curl -sS -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/readyz")" = 200 ] || fail "/readyz is not 200 after POSTGRES_PASSWORD was put back"
kept="$(docker run --rm --network none -v "${PROJECT}_secrets:/s:ro" --entrypoint cat postgres:16-alpine /s/postgres_password)"
[ "$kept" = "$(sed -n 's/^POSTGRES_PASSWORD=//p' "$tmp/env")" ] || fail "the kept postgres_password is not the .env's POSTGRES_PASSWORD"
echo "ok   .env install: POSTGRES_PASSWORD put back is adopted (kept on the volume) and muster reaches its database"

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
cookie="$(grep -i '^set-cookie: muster_session=' <<<"$good" | head -1 | sed -E 's/^[Ss]et-[Cc]ookie: ([^;]*).*/\1/')"
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

# Negative control: the placeholder password, same file, the stack brought up
# again. muster-init refuses it (the server would boot and refuse every
# browser page), so muster never starts.
dc down -v --remove-orphans >/dev/null 2>&1
sed -i "s/^MUSTER_UI_PASSWORD=.*/MUSTER_UI_PASSWORD=CHANGE-ME/" "$tmp/env"
if dc up -d --wait --wait-timeout 180 >/dev/null 2>&1; then
  fail "up --wait SUCCEEDED with MUSTER_UI_PASSWORD=CHANGE-ME (the sign-in page is refused)"
fi
last="$(dc logs --no-color muster-init 2>&1 | tail -n 3)"
grep -q 'config: MUSTER_UI_PASSWORD is shorter than 12 characters' <<<"$last" ||
  fail "muster-init's last lines do not name MUSTER_UI_PASSWORD: $last"
[ -z "$(dc ps -a -q muster)" ] || [ "$(docker inspect -f '{{.State.StartedAt}}' "$(dc ps -a -q muster)")" = 0001-01-01T00:00:00Z ] ||
  fail "muster was started although muster-init refused the placeholder password"
echo "ok   placeholder password: up --wait fails on muster-init, naming MUSTER_UI_PASSWORD; muster never started"

# Negative control: the placeholder HOOK token (the admin bearer), with a good
# UI password. muster must refuse to boot, naming the variable.
dc down -v --remove-orphans >/dev/null 2>&1
sed -i -e "s/^MUSTER_UI_PASSWORD=.*/MUSTER_UI_PASSWORD=$PASSWORD/" -e "s/^MUSTER_HOOK_TOKEN=.*/MUSTER_HOOK_TOKEN=CHANGE-ME/" "$tmp/env"
if dc up -d --wait --wait-timeout 90 >/dev/null 2>&1; then
  fail "up --wait SUCCEEDED with MUSTER_HOOK_TOKEN=CHANGE-ME (a public admin bearer)"
fi
logs="$(dc logs --no-color muster-init muster 2>&1)"
grep -q 'MUSTER_HOOK_TOKEN is the placeholder' <<<"$logs" ||
  fail "muster-init's log does not say MUSTER_HOOK_TOKEN is the placeholder"
echo "ok   placeholder hook token: muster refuses to boot (muster-init), naming MUSTER_HOOK_TOKEN"

# FAIL FAST: a boot refusal fails `up --wait` in seconds, not after a timeout.
# (Before muster-init, muster restart-looped and `up --wait` sat until the
# healthcheck gave up: at least start_period 20s + 6 x 10s, and 8 minutes
# measured on a blind install.)
dc down -v --remove-orphans >/dev/null 2>&1
sed -i "s/^MUSTER_HOOK_TOKEN=.*/MUSTER_HOOK_TOKEN=hook-token-fixture-$RANDOM/" "$tmp/env"
cp "$tmp/env" "$tmp/env.ff"; echo "MUSTER_VAPID_SUBJECT=http://127.0.0.1:8105" >> "$tmp/env"
bound="${COMPOSE_UP_FAIL_FAST_BOUND:-45}"
start=$(date +%s)
if dc up -d --wait --wait-timeout 300 >/dev/null 2>&1; then
  fail "up --wait SUCCEEDED with an http push subject (muster refuses it at boot)"
fi
took=$(( $(date +%s) - start ))
[ "$took" -le "$bound" ] || fail "up --wait took ${took}s to fail on a boot refusal, over the ${bound}s bound"
last="$(dc logs --no-color muster-init 2>&1 | tail -n 3)"
grep -q 'config: .*MUSTER_VAPID_SUBJECT.*must be mailto' <<<"$last" ||
  fail "muster-init's last lines are not the server's refusal naming MUSTER_VAPID_SUBJECT: $last"
[ -z "$(dc ps -a -q muster)" ] || [ "$(docker inspect -f '{{.State.StartedAt}}' "$(dc ps -a -q muster)")" = 0001-01-01T00:00:00Z ] ||
  fail "muster was started although muster-init refused"
echo "ok   fail fast: a boot refusal fails up --wait in ${took}s (bound ${bound}s), muster-init's last line names MUSTER_VAPID_SUBJECT, muster never started"
mv "$tmp/env.ff" "$tmp/env"

# The optional phases start from the good settings again, with a public URL
# this machine can open (the agent links are written on it).
dc down -v --remove-orphans >/dev/null 2>&1
echo "MUSTER_PUBLIC_URL=$base" >> "$tmp/env"
cp "$tmp/env" "$tmp/env.core"

signin_cookie() {
  signin "$PASSWORD" | grep -i '^set-cookie: muster_session=' | head -1 | sed -E 's/^[Ss]et-[Cc]ookie: ([^;]*).*/\1/'
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
  # must not: the store's start script installed that rule. muster starts
  # only once the store is healthy (depends_on s3, service_healthy), so a boot
  # that lost the race with the store is a failure, not a note.
  if grep -q 'the bucket could not be reached at boot' <<<"$logs"; then
    fail "[s3] muster booted before the store had made its bucket: $(grep 'could not be reached at boot' <<<"$logs" | head -2)"
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
expected = {"sessions_overview", "fleet_list", "focus_search", "fleet_report", "tasks_list", "task_get", "attention_list",
            "ask_owner"}
if sys.argv[2] == "1":
    expected.add("route_to_session")
offered = {n[len("mcp__muster__"):] for r in json.loads(sys.argv[1]) for n in r.get("tool_names") or []
           if n.startswith("mcp__muster__")}
assert offered == expected, f"offered {sorted(offered)}, want exactly {sorted(expected)} (extra {sorted(offered - expected)}, missing {sorted(expected - offered)})"
print(f"ok   [chief] the model is offered exactly the pre-allowed muster tools this stack serves: {sorted(offered)}")
PY

  # task_comment, which it is not offered, named anyway (the fake does): the
  # CLI refuses it, the turn ends, and it does not run.
  # chief_say sends one chat turn and prints the reply. A "busy" answer means
  # the session was still finishing the previous turn and this one never ran
  # (nothing was sent to the model), so it is sent again, for up to 60s; any
  # other answer is returned as it is.
  chief_say() {
    local out n=0
    while :; do
      out="$(curl -sS -m 150 -H "Authorization: Bearer $(hook)" -H 'Content-Type: application/json' \
        -d "$1" "$base/api/agents/chief/messages")" || { printf '%s' "$out"; return 1; }
      grep -q '"type\\":\\"busy\\"\|another turn is already being driven' <<<"$out" || { printf '%s' "$out"; return 0; }
      n=$((n + 1)); [ "$n" -lt 12 ] || { printf '%s' "$out"; return 0; }
      sleep 5
    done
  }
  reply="$(chief_say "{\"message\":\"Now comment on it, please. TASK-${task_id:-0}\"}")" || {
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
  reply="$(chief_say '{"message":"Now touch the file, please."}')" || {
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

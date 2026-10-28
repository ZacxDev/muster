#!/usr/bin/env bash
# Bring the production compose stack up for real and prove muster serves.
#
#   tests/compose-up.sh <image-repo> <tag>     e.g. muster-local 1.2.3
#   make compose-up-test IMAGE=muster-local VERSION=1.2.3
#
# Uses deploy/docker-compose.yml UNCHANGED, with MUSTER_IMAGE pointing at a
# locally built image (`make image IMAGE=… VERSION=…`), a unique project name
# and a free host port, so it never touches another stack. Default profile only
# (postgres + muster): the dictation services need a GPU or a model download.
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
#     muster: the signed-in key route serves the generated public key.
# Always tears down (`down -v`), including on failure.
set -euo pipefail

[ $# -eq 2 ] || { echo "usage: $0 <image-repo> <tag>" >&2; exit 2; }
IMAGE="$1"; TAG="$2"
# CDPATH unset: with it exported, `cd` to a relative path PRINTS the directory,
# and ROOT became two lines (measured: every `make` target here broke).
ROOT="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
docker image inspect "$IMAGE:$TAG" >/dev/null 2>&1 || { echo "no local image $IMAGE:$TAG" >&2; exit 2; }

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
cleanup() { dc down -v --remove-orphans >/dev/null 2>&1 || true; rm -rf "$tmp"; }
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
curl -fsS "$base/login" | grep -q 'Sign in to muster' || fail "/login is not the sign-in page"
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

# Negative control: the placeholder password, same file, the stack brought up again.
dc down -v --remove-orphans >/dev/null 2>&1
sed -i "s/^MUSTER_UI_PASSWORD=.*/MUSTER_UI_PASSWORD=CHANGE-ME/" "$tmp/env"
if dc up -d --wait --wait-timeout 180 >/dev/null 2>&1; then
  fail "up --wait SUCCEEDED with MUSTER_UI_PASSWORD=CHANGE-ME (the sign-in page is refused)"
fi
h="$(docker inspect -f '{{.State.Health.Status}}' "$(dc ps -q muster)")"
[ "$h" = unhealthy ] || fail "with the placeholder password muster is '$h', want unhealthy"
docker inspect -f '{{range .State.Health.Log}}{{.Output}}{{end}}' "$(dc ps -q muster)" | grep -q 'MUSTER_UI_PASSWORD' ||
  fail "the health log does not name MUSTER_UI_PASSWORD"
echo "ok   placeholder password: up --wait fails, muster unhealthy, health log names MUSTER_UI_PASSWORD"

# Negative control: the placeholder HOOK token (the admin bearer), with a good
# UI password. muster must refuse to boot, naming the variable.
dc down -v --remove-orphans >/dev/null 2>&1
sed -i -e "s/^MUSTER_UI_PASSWORD=.*/MUSTER_UI_PASSWORD=$PASSWORD/" -e "s/^MUSTER_HOOK_TOKEN=.*/MUSTER_HOOK_TOKEN=CHANGE-ME/" "$tmp/env"
if dc up -d --wait --wait-timeout 90 >/dev/null 2>&1; then
  fail "up --wait SUCCEEDED with MUSTER_HOOK_TOKEN=CHANGE-ME (a public admin bearer)"
fi
dc logs --no-color muster 2>&1 | grep -q 'MUSTER_HOOK_TOKEN is the placeholder' ||
  fail "muster's log does not say MUSTER_HOOK_TOKEN is the placeholder"
echo "ok   placeholder hook token: muster refuses to boot, naming MUSTER_HOOK_TOKEN"
echo "compose-up: PASS"

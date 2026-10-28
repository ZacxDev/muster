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
#     healthcheck's /login probe turns that into "unhealthy".
# Always tears down (`down -v`), including on failure.
set -euo pipefail

[ $# -eq 2 ] || { echo "usage: $0 <image-repo> <tag>" >&2; exit 2; }
IMAGE="$1"; TAG="$2"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
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
MUSTER_HOOK_TOKEN=hook-$RANDOM$RANDOM
MUSTER_SECURE_COOKIES=0
EOF
dc() { docker compose -f "$ROOT/deploy/docker-compose.yml" --env-file "$tmp/env" -p "$PROJECT" "$@"; }
cleanup() { dc down -v --remove-orphans >/dev/null 2>&1 || true; rm -rf "$tmp"; }
trap cleanup EXIT

fail() { echo "FAIL: $1"; dc ps -a || true; dc logs --no-color --tail 40 muster || true; exit 1; }

echo "compose-up: project $PROJECT, port $PORT, image $IMAGE:$TAG"
start=$(date +%s)
dc up -d --wait --wait-timeout 180 || fail "docker compose up -d --wait did not succeed"
echo "compose-up: --wait returned 0 after $(( $(date +%s) - start ))s"

for svc in postgres muster; do
  h="$(docker inspect -f '{{.State.Health.Status}}' "$(dc ps -q "$svc")")"
  [ "$h" = healthy ] || fail "$svc health is '$h'"
  echo "ok   $svc: healthy"
done

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
echo "compose-up: PASS"

#!/usr/bin/env bash
# deploy/docker-compose.yml, run as a CLONE runs it: the images and the version
# the file DEFAULTS to (pulled from GHCR), nothing overridden — no MUSTER_IMAGE,
# no MUSTER_VERSION, no .env. muster-init must complete and /readyz answer 200.
#
#   tests/compose-default-image.sh                 (CI: release-dry-run)
#   COMPOSE_DEFAULT_IMAGE_FILE=<copy> COMPOSE_DEFAULT_IMAGE_FORCE=1 tests/compose-default-image.sh
#                                                  (a stamped copy, for the controls)
#
# 🔴 WHY THIS EXISTS (#707 round 2, F1). Every other compose test runs the
# image the CI run BUILT (MUSTER_IMAGE=muster-local), so none of them could
# see that the file at HEAD needs a newer muster than the release it defaults
# to: 0.231.0 under a compose that runs compose-init stopped with "DATABASE_URL
# is not set", and a clone of main could not start. This runs the published
# default.
#
# THE BOOTSTRAP. While the file needs the NEXT release (x-muster-min-version,
# not yet cut; scripts/check-compose-default.sh --state says `bootstrap`), no
# published image can run it, and this SKIPS, saying so — that state is
# allowed only in the window between merging such a change and releasing it.
# Any other state runs it, including a failing check (the live result is
# then the evidence). COMPOSE_DEFAULT_IMAGE_FORCE=1 runs it regardless.
#
# SERVED means all of: `up -d --wait` succeeds, muster-init exited 0, and
# /readyz answers 200 within COMPOSE_DEFAULT_IMAGE_TIMEOUT seconds (default
# 240). Anything else — muster-init failing, the server refusing or crashing,
# no ready state in time — is NOT SERVED, whatever the reason.
#
# COMPOSE_DEFAULT_IMAGE_EXPECT=fail is the NEGATIVE CONTROL's mode (CI: the
# release below x-muster-min-version, #707 round 3): it passes only when the
# stack did NOT serve, by ANY of those failures — never pinned to one failure's
# text — and FAILS if it served. The images are pulled first, as a harness
# step: one that cannot be fetched is a harness error (exit 2), never "did not
# serve", so the control cannot pass by failing to download.
#
# Uses a unique project name and a free port; always tears down (`down -v`).
# Exit 0 = the expected outcome (served; with EXPECT=fail, did not serve; or
# the bootstrap skip), 1 = the other one, 2 = harness (pull, an overridden image,
# usage).
set -euo pipefail

ROOT="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
FILE="${COMPOSE_DEFAULT_IMAGE_FILE:-$ROOT/deploy/docker-compose.yml}"
EXPECT="${COMPOSE_DEFAULT_IMAGE_EXPECT:-serve}"
TIMEOUT="${COMPOSE_DEFAULT_IMAGE_TIMEOUT:-240}"
[[ "$EXPECT" =~ ^(serve|fail)$ && "$TIMEOUT" =~ ^[0-9]+$ ]] ||
  { echo "compose-default-image: COMPOSE_DEFAULT_IMAGE_EXPECT must be serve|fail and COMPOSE_DEFAULT_IMAGE_TIMEOUT seconds" >&2; exit 2; }
cur="$("$ROOT/scripts/stamp-compose-version.sh" --current < "$FILE")"

if [ -z "${COMPOSE_DEFAULT_IMAGE_FORCE:-}" ] && [ "$EXPECT" = serve ]; then   # a negative control never skips
  state="$(CHECK_COMPOSE_FILE="$FILE" "$ROOT/scripts/check-compose-default.sh" --state 2>/dev/null || echo fail)"
  if [ "$state" = bootstrap ]; then
    echo "SKIP compose-default-image: the bootstrap — this file needs the next release, not yet cut, and defaults to $cur, which cannot run it (scripts/check-compose-default.sh). Release right after merging."
    exit 0
  fi
fi

PROJECT="muster-default-image-$$-$RANDOM"
PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')"
tmp="$(mktemp -d)"
# `env -i`: nothing of this shell reaches compose but the port (never the image
# or the version); --env-file /dev/null: no deploy/.env, even a developer's.
d() {
  env -i PATH="$PATH" HOME="$HOME" ${DOCKER_HOST:+DOCKER_HOST="$DOCKER_HOST"} MUSTER_HTTP_PORT="$PORT" \
    docker compose -f "$FILE" --env-file /dev/null -p "$PROJECT" "$@"
}
trap 'd down -v --remove-orphans >/dev/null 2>&1 || true; rm -rf "$tmp"' EXIT
image="$(d config --images | grep -m1 '/muster:' || true)"
echo "compose-default-image: project $PROJECT, port $PORT, the file's default $cur ($image), expect $EXPECT"
harness() { echo "HARNESS compose-default-image: $1"; exit 2; }
[ "$image" = "ghcr.io/zacxdev/muster:$cur" ] || harness "compose resolves muster's image to '$image', not the default ghcr.io/zacxdev/muster:$cur (something overrode it)"
# The images first: a missing one is a harness error, not a failure to serve.
for i in $(d config --images | sort -u); do
  docker image inspect "$i" >/dev/null 2>&1 || docker pull -q "$i" >/dev/null 2>&1 || harness "cannot pull $i"
done

# not_served REASON: the stack did not serve. Under EXPECT=fail that is the pass.
not_served() {
  d ps -a || true
  d logs --no-color --tail 20 muster-init muster || true
  if [ "$EXPECT" = fail ]; then
    echo "ok   compose-default-image: did not serve, as the negative control requires: $1"
    exit 0
  fi
  echo "FAIL compose-default-image: $1"
  exit 1
}
start=$SECONDS
d up -d --wait --wait-timeout "$TIMEOUT" > "$tmp/up" 2>&1 || { cat "$tmp/up"; not_served "up -d --wait on $cur did not succeed"; }
[ "$(docker inspect -f '{{.State.ExitCode}}' "$(d ps -a -q muster-init)")" = 0 ] || not_served "muster-init did not exit 0"
code=000
while :; do
  code="$(curl -sS -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/readyz" 2>/dev/null || true)"
  [ "$code" != 200 ] && [ $(( SECONDS - start )) -lt "$TIMEOUT" ] || break
  sleep 2
done
[ "$code" = 200 ] || not_served "/readyz answered $code, not 200, within ${TIMEOUT}s"
if [ "$EXPECT" = fail ]; then
  echo "FAIL compose-default-image: NEGATIVE CONTROL FAILED: $cur SERVED this file (muster-init exit 0, /readyz 200); it was expected not to"
  exit 1
fi
echo "ok   compose-default-image: the file at HEAD runs its default release $cur as a clone runs it (muster-init exit 0, /readyz 200)"

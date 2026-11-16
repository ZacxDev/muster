#!/usr/bin/env bash
# deploy/docker-compose.yml defaults to the LATEST RELEASE, and that release's
# images exist.
#
#   scripts/check-compose-default.sh            check (CI: release-dry-run)
#   scripts/check-compose-default.sh --bump     rewrite the default to the latest release
#   scripts/check-compose-default.sh --images   also `docker manifest inspect` every image
#                                               the default names (needs the network)
#   scripts/check-compose-default.sh --images-only   only that probe (its negative control
#                                               runs it on a copy stamped with a version
#                                               never released: CHECK_COMPOSE_FILE)
#
# A clone runs `docker compose -f deploy/docker-compose.yml up` with no .env,
# so the version written in that file is what a new user gets. It must be a
# RELEASE (an image CI published under a version tag, never a moving channel),
# and the NEWEST one: a default left behind would hand every new clone an old
# server, and the schema it migrates to.
#
# "The newest release" is the highest vX.Y.Z tag on a commit this one CONTAINS,
# leaving out a tag on this very commit: the release tag vN lands on a commit
# whose own copy cannot already say N (the tag comes after the commit), so the
# tag run for vN, and the main run of that commit, check against vN-1, and every
# commit after it against vN. So after each release the repository's copy is
# bumped once (--bump, in a pull request), and CI fails until it is.
#
# The default is read with scripts/stamp-compose-version.sh --current and
# rewritten with its stamp, the one spelling release-build.sh uses.
#
# Needs the tags (CI: actions/checkout with fetch-depth 0). Exit 0 ok, 1 the
# default is behind / ahead / an image is missing, 2 harness (no tags, usage).
set -euo pipefail

ROOT="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE="${CHECK_COMPOSE_FILE:-$ROOT/deploy/docker-compose.yml}"
REPO="${CHECK_COMPOSE_REPO:-$ROOT}"
mode="${1:-check}"
case "$mode" in check|--bump|--images|--images-only) ;; *) echo "usage: $0 [--bump|--images|--images-only]" >&2; exit 2 ;; esac

cur="$("$ROOT/scripts/stamp-compose-version.sh" --current < "$COMPOSE")" || exit 1

images() {
  # Every image the default names: `<repo>:${MUSTER_VERSION:-X.Y.Z}`, whatever
  # override variable wraps the repository.
  local repos r ok missing=0
  repos="$(grep -oE '\$\{[A-Z_]+:-[a-z0-9./-]+\}:\$\{MUSTER_VERSION:-' "$COMPOSE" | sed -E 's/^\$\{[A-Z_]+:-([^}]*)\}.*/\1/' | sort -u)"
  [ "$(grep -c . <<<"$repos")" -ge 3 ] || { echo "check-compose-default: read only $(grep -c . <<<"$repos") image(s) from the compose file: $repos" >&2; return 2; }
  for r in $repos; do
    ok=0
    for _ in 1 2 3; do
      if docker manifest inspect "$r:$cur" >/dev/null 2>&1; then ok=1; break; fi
      sleep 5
    done
    if [ "$ok" = 1 ]; then echo "ok   $r:$cur exists"
    else echo "FAIL: $r:$cur does not exist (docker manifest inspect): a fresh clone could not pull it" >&2; missing=1; fi
  done
  return "$missing"
}
if [ "$mode" = --images-only ]; then images; exit $?; fi

# The newest release tag on a commit HEAD contains, not counting HEAD's own.
here="$(git -C "$REPO" tag --points-at HEAD --list 'v*')"
latest="$(git -C "$REPO" tag --merged HEAD --list 'v*' | grep -Ex 'v[0-9]+\.[0-9]+\.[0-9]+' |
  grep -vxF -f <(printf '%s\n' "$here" | grep . || echo '#none#') | sort -V | tail -1 || true)"
[ -n "$latest" ] || { echo "check-compose-default: no vX.Y.Z release tag is visible below HEAD (fetch the tags: fetch-depth 0)" >&2; exit 2; }
latest="${latest#v}"

if [ "$mode" = --bump ]; then
  if [ "$cur" = "$latest" ]; then echo "check-compose-default: already $latest"; exit 0; fi
  tmp="$(mktemp)"
  "$ROOT/scripts/stamp-compose-version.sh" "$latest" < "$COMPOSE" > "$tmp" || { rm -f "$tmp"; exit 1; }
  cat "$tmp" > "$COMPOSE"; rm -f "$tmp"
  echo "check-compose-default: deploy/docker-compose.yml now defaults to $latest (was $cur)"
  exit 0
fi

if [ "$cur" != "$latest" ]; then
  echo "FAIL: deploy/docker-compose.yml defaults to $cur, but the newest release before this commit is v$latest." >&2
  echo "      A clone must run the newest release. Bump it: scripts/check-compose-default.sh --bump" >&2
  exit 1
fi
echo "ok   deploy/docker-compose.yml defaults to $cur, the newest release (v$latest)"

if [ "$mode" = --images ]; then images; exit $?; fi

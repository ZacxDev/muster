#!/usr/bin/env bash
# Stamp a release version into deploy/docker-compose.yml's image default.
#
#   scripts/stamp-compose-version.sh 1.2.3 < deploy/docker-compose.yml > dist/docker-compose.yml
#
# The repository's compose file tags every image ${MUSTER_VERSION:-main}: a
# clone runs the image CI publishes from main's newest commit. A release's copy
# must run THAT release with no .env, so every `${MUSTER_VERSION:-main}` becomes
# `${MUSTER_VERSION:-<version>}` — MUSTER_VERSION in .env still pins another.
#
# 🔴 THE ONE SPELLING OF THE STAMP. scripts/release-build.sh writes the release
# asset with it and tests/compose-check.sh checks a stamped copy runs the
# release in every service, so the two cannot drift. It refuses input with no
# default to stamp, and output that still names `main`.
#
# Exit 0 ok, 1 nothing (or not everything) was stamped, 2 usage.
set -euo pipefail

[ $# -eq 1 ] || { echo "usage: $0 <X.Y.Z> < docker-compose.yml" >&2; exit 2; }
v="${1#v}"
[[ "$v" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "stamp-compose-version: '$1' is not X.Y.Z" >&2; exit 2; }

in="$(cat)"
n="$(grep -o '\${MUSTER_VERSION:-main}' <<<"$in" | wc -l)"
[ "$n" -gt 0 ] || { echo "stamp-compose-version: no \${MUSTER_VERSION:-main} default to stamp" >&2; exit 1; }
out="${in//\$\{MUSTER_VERSION:-main\}/\$\{MUSTER_VERSION:-$v\}}"
if grep -q 'MUSTER_VERSION:-main' <<<"$out"; then
  echo "stamp-compose-version: a \${MUSTER_VERSION:-main} survived the stamp" >&2; exit 1
fi
[ "$(grep -o "\${MUSTER_VERSION:-$v}" <<<"$out" | wc -l)" -eq "$n" ] ||
  { echo "stamp-compose-version: stamped a different number of defaults than there were ($n)" >&2; exit 1; }
printf '%s\n' "$out"

#!/usr/bin/env bash
# Stamp a release version into deploy/docker-compose.yml's image default.
#
#   scripts/stamp-compose-version.sh 1.2.3 < deploy/docker-compose.yml > dist/docker-compose.yml
#   scripts/stamp-compose-version.sh --current < deploy/docker-compose.yml     # prints the default
#
# Every image muster builds is tagged ${MUSTER_VERSION:-X.Y.Z}: with no .env a
# compose file runs the release written in it. The repository's copy holds the
# LATEST RELEASE (scripts/check-compose-default.sh warns, then fails CI, when it is behind,
# and bumps it with --bump); a release's own copy is stamped with that release
# (scripts/release-build.sh). MUSTER_VERSION in .env still pins another.
#
# 🔴 THE ONE SPELLING OF THE STAMP. release-build.sh writes the release asset
# with it, check-compose-default.sh bumps the repository's copy with it, and
# tests/compose-check.sh checks a stamped copy runs the release in every
# service, so they cannot drift. It refuses input with no default to stamp, or
# whose defaults are not all one X.Y.Z version (a `main` channel included: a
# clone never runs a moving tag), and checks every default was stamped.
#
# Exit 0 ok, 1 nothing (or not everything) was stamped / no single default,
# 2 usage.
set -euo pipefail

[ $# -eq 1 ] || { echo "usage: $0 <X.Y.Z> | --current   (reads the compose file on stdin)" >&2; exit 2; }

in="$(cat)"
# Every ${MUSTER_VERSION...} reference, and the X.Y.Z defaults among them.
refs="$(grep -o '\${MUSTER_VERSION[^}]*}' <<<"$in" || true)"
n="$(grep -c . <<<"$refs" || true)"
[ "$n" -gt 0 ] || { echo "stamp-compose-version: no \${MUSTER_VERSION:-X.Y.Z} default to stamp" >&2; exit 1; }
bad="$(grep -Ev '^\$\{MUSTER_VERSION:-[0-9]+\.[0-9]+\.[0-9]+\}$' <<<"$refs" || true)"
[ -z "$bad" ] || { echo "stamp-compose-version: a default that is not a release version: $(head -1 <<<"$bad")" >&2; exit 1; }
cur="$(sed -E 's/^\$\{MUSTER_VERSION:-(.*)\}$/\1/' <<<"$refs" | sort -u)"
[ "$(grep -c . <<<"$cur")" -eq 1 ] || { echo "stamp-compose-version: the defaults are not one version: $(tr '\n' ' ' <<<"$cur")" >&2; exit 1; }

if [ "$1" = --current ]; then
  printf '%s\n' "$cur"
  exit 0
fi

v="${1#v}"
[[ "$v" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "stamp-compose-version: '$1' is not X.Y.Z" >&2; exit 2; }
out="${in//\$\{MUSTER_VERSION:-$cur\}/\$\{MUSTER_VERSION:-$v\}}"
[ "$(grep -o "\${MUSTER_VERSION:-$v}" <<<"$out" | wc -l)" -eq "$n" ] ||
  { echo "stamp-compose-version: stamped a different number of defaults than there were ($n)" >&2; exit 1; }
printf '%s\n' "$out"

#!/usr/bin/env bash
# deploy/docker-compose.yml defaults to a release this file can run, the
# NEWEST one (after a short grace period), and that release's images exist.
#
#   scripts/check-compose-default.sh                 check (CI: release-dry-run)
#   scripts/check-compose-default.sh --state         the check, printing only its state:
#                                                    ok | grace | bootstrap (exit 0), else exit 1
#   scripts/check-compose-default.sh --bump [X.Y.Z]  rewrite the default to X.Y.Z (default: the
#                                                    newest release below HEAD); non-interactive
#   scripts/check-compose-default.sh --images        also `docker manifest inspect` every image
#                                                    the default names (needs the network)
#   scripts/check-compose-default.sh --images-only   only that probe (its negative control
#                                                    runs it on a copy stamped with a version
#                                                    never released: CHECK_COMPOSE_FILE)
#
# A clone runs `docker compose -f deploy/docker-compose.yml up` with no .env,
# so the version written in that file is what a new user gets. It must be a
# RELEASE (an image CI published under a version tag, never a moving channel)
# that can run THIS file, and the newest one.
#
# THE RULES, in this order. Versions compare as versions; "latest" is the
# newest vX.Y.Z tag on a commit HEAD contains, NOT counting a tag on HEAD
# itself (the tag vN lands on a commit whose own copy cannot already say N, so
# the tag run of vN checks against vN-1); "previous" is the newest release
# below latest.
#
#   1. MINIMUM. The file states the oldest muster it runs:
#        x-muster-min-version: "X.Y.Z"
#      (it uses what that release added). A default below it FAILS — a clone
#      would pull an image that cannot start this file (#707 round 2, F1:
#      0.231.0 under a compose that runs compose-init: "DATABASE_URL is not
#      set") — with ONE exception, THE BOOTSTRAP: the minimum is the next
#      release, not yet cut (exactly next-minor(latest), the version
#      `muster-ops` mints next), and the default is latest. That is the state
#      of the pull request that starts using the new feature, and of main
#      between its merge and the release it waits for: it WARNS (state
#      `bootstrap`), and the merge procedure is "merge, then release at once"
#      (the release flow then bumps the default; see below). A minimum two
#      releases out, or one that is released while the default stays below it,
#      FAILS.
#   2. NEWEST. The default equals latest: ok. The default equals previous, and
#      latest was tagged less than CHECK_COMPOSE_GRACE_DAYS (default 3) days
#      ago: WARNS (state `grace`) — the release flow (`muster-ops verify`)
#      opens the bump pull request right after it pushes the tag, and every
#      pull request and main stay green while that is merged. Past the grace
#      period, older than previous, or AHEAD of latest: FAILS.
#
# A WARN prints a `::warning::` line in GitHub Actions and exits 0.
#
# --bump X.Y.Z is what the release flow runs, in a worktree off main, right
# after it tags vX.Y.Z: it needs the tag vX.Y.Z on HEAD or below it (HEAD
# itself counts here: main may not have moved since the tag), never asks
# anything, and prints exactly one line on stdout:
#   check-compose-default: deploy/docker-compose.yml now defaults to X.Y.Z (was A.B.C)
#   check-compose-default: already X.Y.Z
# The default is read with scripts/stamp-compose-version.sh --current and
# rewritten with its stamp, the one spelling release-build.sh uses.
#
# Test hooks: CHECK_COMPOSE_FILE, CHECK_COMPOSE_REPO, CHECK_COMPOSE_NOW (unix
# seconds), CHECK_COMPOSE_GRACE_DAYS. Needs the tags (CI: actions/checkout with
# fetch-depth 0). Exit 0 ok (or a warning), 1 the default fails a rule / an
# image is missing, 2 harness (no tags, usage, an unreadable minimum).
set -euo pipefail

ROOT="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE="${CHECK_COMPOSE_FILE:-$ROOT/deploy/docker-compose.yml}"
REPO="${CHECK_COMPOSE_REPO:-$ROOT}"
GRACE_DAYS="${CHECK_COMPOSE_GRACE_DAYS:-3}"
NOW="${CHECK_COMPOSE_NOW:-$(date +%s)}"
mode="${1:-check}"
bump_to=""
case "$mode" in
  check|--state|--images|--images-only) [ $# -le 1 ] || mode=usage ;;
  --bump) bump_to="${2:-}"; [ $# -le 2 ] || mode=usage ;;
  *) mode=usage ;;
esac
[ "$mode" != usage ] || { echo "usage: $0 [--state|--bump [X.Y.Z]|--images|--images-only]" >&2; exit 2; }
[[ "$GRACE_DAYS" =~ ^[0-9]+$ && "$NOW" =~ ^[0-9]+$ ]] || { echo "check-compose-default: CHECK_COMPOSE_GRACE_DAYS / CHECK_COMPOSE_NOW must be integers" >&2; exit 2; }

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

# a <= b, as versions.
vle() { [ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | head -1)" = "$1" ]; }
next_minor() { local a b; IFS=. read -r a b _ <<<"$1"; echo "$a.$((b + 1)).0"; }

# Every release tag HEAD contains (newest last); $1=1 keeps a tag on HEAD itself.
releases() {
  local here
  here="$(git -C "$REPO" tag --points-at HEAD --list 'v*')"
  git -C "$REPO" tag --merged HEAD --list 'v*' | grep -Ex 'v[0-9]+\.[0-9]+\.[0-9]+' |
    { if [ "$1" = 1 ]; then cat; else grep -vxF -f <(printf '%s\n' "$here" | grep . || echo '#none#'); fi; } |
    sed 's/^v//' | sort -V || true
}

if [ "$mode" = --bump ]; then
  if [ -z "$bump_to" ]; then
    bump_to="$(releases 0 | tail -1)"
    [ -n "$bump_to" ] || { echo "check-compose-default: no vX.Y.Z release tag is visible below HEAD (fetch the tags)" >&2; exit 2; }
  fi
  bump_to="${bump_to#v}"
  [[ "$bump_to" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "check-compose-default: --bump '$bump_to' is not X.Y.Z" >&2; exit 2; }
  releases 1 | grep -qxF "$bump_to" ||
    { echo "check-compose-default: there is no release tag v$bump_to on HEAD or below it (fetch the tags; a version is bumped to only once it is tagged)" >&2; exit 2; }
  if [ "$cur" = "$bump_to" ]; then echo "check-compose-default: already $bump_to"; exit 0; fi
  tmp="$(mktemp)"
  "$ROOT/scripts/stamp-compose-version.sh" "$bump_to" < "$COMPOSE" > "$tmp" || { rm -f "$tmp"; exit 1; }
  cat "$tmp" > "$COMPOSE"; rm -f "$tmp"
  echo "check-compose-default: deploy/docker-compose.yml now defaults to $bump_to (was $cur)"
  exit 0
fi

# The minimum the file states: exactly one top-level line.
mins="$(grep -E '^x-muster-min-version:' "$COMPOSE" || true)"
[ "$(grep -c . <<<"$mins")" = 1 ] ||
  { echo "check-compose-default: the compose file must state its minimum muster once, as x-muster-min-version: \"X.Y.Z\" (found $(grep -c . <<<"$mins"))" >&2; exit 2; }
min="$(sed -nE 's/^x-muster-min-version:[[:space:]]*"?([0-9]+\.[0-9]+\.[0-9]+)"?[[:space:]]*$/\1/p' <<<"$mins")"
[ -n "$min" ] || { echo "check-compose-default: x-muster-min-version is not \"X.Y.Z\": $mins" >&2; exit 2; }

rels="$(releases 0)"
latest="$(tail -1 <<<"$rels")"
[ -n "$latest" ] || { echo "check-compose-default: no vX.Y.Z release tag is visible below HEAD (fetch the tags: fetch-depth 0)" >&2; exit 2; }
previous="$(grep -vxF "$latest" <<<"$rels" | tail -1 || true)"

state=ok
warn=()
failed() {
  local l
  for l in "$@"; do echo "FAIL: $l" >&2; done
  exit 1
}
say_warn() { warn+=("$1"); }

# 1. The minimum.
if ! vle "$min" "$cur"; then
  # (next-minor(latest) is above latest, so this is also "the minimum is unreleased")
  if [ "$min" = "$(next_minor "$latest")" ] && [ "$cur" = "$latest" ]; then
    state=bootstrap
    say_warn "deploy/docker-compose.yml needs muster $min (x-muster-min-version), the NEXT release, not yet cut; it defaults to $cur, the newest release, which cannot run it. A clone of main cannot start until $min is released: merge, then release at once (the release flow bumps the default)."
  elif vle "$min" "$latest"; then
    failed "deploy/docker-compose.yml defaults to $cur, but this file needs muster $min or newer (x-muster-min-version), and a release that can run it is out (v$latest): a clone would pull an image that cannot start it." \
      "      Bump it: scripts/check-compose-default.sh --bump $latest"
  else
    failed "deploy/docker-compose.yml needs muster $min (x-muster-min-version) and defaults to $cur. The only unreleased minimum allowed is the NEXT release, $(next_minor "$latest"), with the default at the newest release, v$latest (the bootstrap)."
  fi
fi

# 2. The newest release.
if [ "$cur" = "$latest" ]; then
  :
elif [ -n "$previous" ] && [ "$cur" = "$previous" ]; then
  tagged="$(git -C "$REPO" for-each-ref --format='%(creatordate:unix)' "refs/tags/v$latest")"
  [[ "$tagged" =~ ^[0-9]+$ ]] || { echo "check-compose-default: cannot read when v$latest was tagged" >&2; exit 2; }
  age=$(( NOW - tagged ))
  if [ "$age" -lt $(( GRACE_DAYS * 86400 )) ]; then
    state=grace
    say_warn "deploy/docker-compose.yml defaults to $cur; v$latest was released $(( age / 3600 ))h ago. The release flow opens the bump pull request (\"compose: default to v$latest\"); merge it. This fails once v$latest is ${GRACE_DAYS} days old."
  else
    failed "deploy/docker-compose.yml defaults to $cur, but the newest release before this commit is v$latest, released $(( age / 86400 )) days ago (the grace period is ${GRACE_DAYS} days)." \
      "      A clone must run the newest release. Bump it: scripts/check-compose-default.sh --bump $latest"
  fi
elif vle "$cur" "$latest"; then
  failed "deploy/docker-compose.yml defaults to $cur, but the newest release before this commit is v$latest (older than the previous release, ${previous:-none}: no grace period)." \
    "      A clone must run the newest release. Bump it: scripts/check-compose-default.sh --bump $latest"
else
  failed "deploy/docker-compose.yml defaults to $cur, AHEAD of the newest release before this commit (v$latest): a clone would pull an image that does not exist."
fi

# --state prints the state word ONLY (callers compare it), in Actions too.
if [ "$mode" = --state ]; then echo "$state"; exit 0; fi
for w in "${warn[@]+"${warn[@]}"}"; do
  if [ -n "${GITHUB_ACTIONS:-}" ]; then echo "::warning title=compose default::$w"; fi
  echo "WARN $w"
done
case "$state" in
  ok) echo "ok   deploy/docker-compose.yml defaults to $cur, the newest release (v$latest), and this file needs $min or newer" ;;
  *)  echo "ok   ($state) deploy/docker-compose.yml defaults to $cur; the newest release is v$latest; this file needs $min" ;;
esac

if [ "$mode" = --images ]; then images; exit $?; fi

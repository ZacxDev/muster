#!/usr/bin/env bash
# Rewrite CHANGELOG.md from every vX.Y.Z tag (scripts/release-notes.py) and
# check it lists the release it is bumped for.
#
#   scripts/changelog-bump.sh [X.Y.Z]      default: the release deploy/docker-compose.yml defaults to
#
# The release flow runs this in the same worktree, right after
# `scripts/check-compose-default.sh --bump X.Y.Z`, and commits both files in
# its post-release pull request ("compose: default to vX.Y.Z"); `make
# changelog` runs it by hand. tests/release_notes_test.py fails while
# CHANGELOG.md lacks the release the compose file defaults to, so the two
# cannot drift apart on main.
#
# Needs the tags (git fetch --tags) and `gh` (each pull request's labels).
# Prints exactly one line on stdout:
#   changelog-bump: CHANGELOG.md now lists vX.Y.Z (<n> releases)
#   changelog-bump: CHANGELOG.md already lists vX.Y.Z (unchanged)
# CHANGELOG.md is replaced only by a complete, checked rendering (written to a
# temporary file first). Exit 0 ok; 1 the rendering failed or lacks vX.Y.Z
# (CHANGELOG.md untouched); 2 usage.
#
# Test hooks: CHANGELOG_REPO (the git repository, default this one),
# CHANGELOG_FILE, CHANGELOG_GH_REPO (default ZacxDev/muster).
set -euo pipefail

ROOT="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
REPO="${CHANGELOG_REPO:-$ROOT}"
FILE="${CHANGELOG_FILE:-$REPO/CHANGELOG.md}"
GH_REPO="${CHANGELOG_GH_REPO:-ZacxDev/muster}"
die() { echo "changelog-bump: $1" >&2; exit "${2:-1}"; }

[ $# -le 1 ] || die "usage: $0 [X.Y.Z]" 2
v="${1:-}"
if [ -z "$v" ]; then
  v="$("$ROOT/scripts/stamp-compose-version.sh" --current < "$REPO/deploy/docker-compose.yml")" ||
    die "could not read the compose default from $REPO/deploy/docker-compose.yml" 2
fi
v="${v#v}"
[[ "$v" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "'$v' is not X.Y.Z" 2

tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
python3 "$ROOT/scripts/release-notes.py" collect --repo "$GH_REPO" --git-dir "$REPO" --all > "$tmp/data.json" ||
  die "collecting the pull requests failed; CHANGELOG.md is unchanged"
python3 "$ROOT/scripts/release-notes.py" changelog --data "$tmp/data.json" > "$tmp/CHANGELOG.md" ||
  die "rendering the changelog failed; CHANGELOG.md is unchanged"
grep -qxF "## v$v" "$tmp/CHANGELOG.md" ||
  die "the rendered changelog has no v$v (is the tag fetched? git fetch --tags); CHANGELOG.md is unchanged"
n="$(grep -c '^## v' "$tmp/CHANGELOG.md")"
if [ -f "$FILE" ] && cmp -s "$tmp/CHANGELOG.md" "$FILE"; then
  echo "changelog-bump: CHANGELOG.md already lists v$v (unchanged)"
else
  cat "$tmp/CHANGELOG.md" > "$FILE"
  echo "changelog-bump: CHANGELOG.md now lists v$v ($n releases)"
fi

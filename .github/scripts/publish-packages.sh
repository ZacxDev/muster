#!/usr/bin/env bash
# Publish a release's generated Homebrew formulas and Scoop manifest to the
# tap and bucket repositories. Used by the `release` job in
# .github/workflows/ci.yml, AFTER the release is published (the files name its
# assets' URLs); tested by tests/publish-packages-test.sh against file:// repos.
#
#   publish-packages.sh <X.Y.Z> <dist-dir>
#
#   <dist>/muster.rb, <dist>/muster-mirror.rb  -> ZacxDev/homebrew-muster  Formula/
#   <dist>/muster.json                         -> ZacxDev/scoop-muster     bucket/
#
# Each repository gets ONE commit, "muster X.Y.Z", on its default branch.
#
#   * MUSTER_PACKAGES_TOKEN unset or empty: prints that it skipped, exit 0
#     (a fork, or before the operator adds the secret).
#   * Idempotent: files already identical (a re-run) -> no commit, exit 0.
#   * 🔴 NEVER BACKWARDS: a repository whose file already names a NEWER version
#     (a re-run of an older tag) is left alone.
#   * A push rejected because the branch moved is retried from a fresh clone
#     (3 attempts).
#
# 🔴 THE TOKEN NEVER REACHES argv OR A FILE: git gets it as an http.extraheader
# through GIT_CONFIG_COUNT/KEY/VALUE environment variables, so neither `ps`, a
# clone's .git/config nor git's own messages carry it. The token is a
# fine-grained PAT for those two repositories only, Contents: read and write
# (docs/releases.md, "Homebrew and Scoop").
#
# Test hooks: PACKAGES_REMOTE_BASE (default https://github.com), PACKAGES_TAP,
# PACKAGES_BUCKET (owner/name). Exit 0 published/skipped/unchanged; 1 failed;
# 2 usage.
set -euo pipefail

die() { echo "publish-packages: $1" >&2; exit "${2:-1}"; }
say() { echo "publish-packages: $*"; }
[ $# -eq 2 ] || die "usage: $0 <X.Y.Z> <dist-dir>" 2
V="${1#v}"; DIST="$2"
[[ "$V" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "version '$1' is not X.Y.Z" 2
for f in muster.rb muster-mirror.rb muster.json; do [ -f "$DIST/$f" ] || die "no $DIST/$f" 2; done

if [ -z "${MUSTER_PACKAGES_TOKEN:-}" ]; then
  say "skipped: MUSTER_PACKAGES_TOKEN is not set, so the tap and the bucket are not updated (the formulas and manifest are on the release)"
  exit 0
fi

BASE="${PACKAGES_REMOTE_BASE:-https://github.com}"
TAP="${PACKAGES_TAP:-ZacxDev/homebrew-muster}"
BUCKET="${PACKAGES_BUCKET:-ZacxDev/scoop-muster}"

auth="$(printf 'x-access-token:%s' "$MUSTER_PACKAGES_TOKEN" | base64 | tr -d '\n')"
export GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0="http.https://github.com/.extraheader" GIT_CONFIG_VALUE_0="AUTHORIZATION: basic $auth"
export GIT_AUTHOR_NAME="muster release" GIT_AUTHOR_EMAIL="41898282+github-actions[bot]@users.noreply.github.com"
export GIT_COMMITTER_NAME="$GIT_AUTHOR_NAME" GIT_COMMITTER_EMAIL="$GIT_AUTHOR_EMAIL"
export GIT_TERMINAL_PROMPT=0

work="$(mktemp -d)"; trap 'rm -rf "$work"' EXIT

# version_in <file>: the version a formula or manifest names, or nothing.
version_in() {
  [ -f "$1" ] || return 0
  case "$1" in
    *.rb) sed -n 's/^  version "\([0-9][0-9.]*\)"$/\1/p' "$1" | head -n 1 ;;
    *.json) sed -n 's/^  "version": "\([0-9][0-9.]*\)",$/\1/p' "$1" | head -n 1 ;;
  esac
}

# publish <owner/name> <dir in the repo> <files...>
publish() {
  local repo="$1" dir="$2"; shift 2
  local attempt clone f cur
  for attempt in 1 2 3; do
    clone="$work/$(tr / _ <<<"$repo")-$attempt"
    git clone -q --depth 1 "$BASE/$repo.git" "$clone" || die "could not clone $repo"
    mkdir -p "$clone/$dir"
    for f in "$@"; do
      cur="$(version_in "$clone/$dir/$f")"
      if [ -n "$cur" ] && [ "$cur" != "$V" ] && [ "$(printf '%s\n%s\n' "$cur" "$V" | sort -V | tail -n 1)" = "$cur" ]; then
        say "$repo: $dir/$f already names $cur, newer than $V; left alone"
        return 0
      fi
    done
    for f in "$@"; do cp "$DIST/$f" "$clone/$dir/$f"; done
    git -C "$clone" add -- "$dir"
    if git -C "$clone" diff --cached --quiet; then
      say "$repo: already at $V ($dir/: $*); nothing to commit"
      return 0
    fi
    git -C "$clone" commit -q -m "muster $V" -m "From the muster v$V release's assets." -- "$dir"
    if git -C "$clone" push -q origin HEAD 2>"$work/push.err"; then
      say "$repo: committed and pushed muster $V ($dir/: $*)"
      return 0
    fi
    say "$repo: push attempt $attempt refused: $(head -n 1 "$work/push.err"); retrying from a fresh clone"
  done
  die "$repo: could not push muster $V after 3 attempts"
}

publish "$TAP" Formula muster.rb muster-mirror.rb
publish "$BUCKET" bucket muster.json

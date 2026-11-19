#!/usr/bin/env bash
# .github/scripts/publish-packages.sh against two local bare repositories
# (file://) standing in for the tap and the bucket. No network, no token.
#
#   tests/publish-packages-test.sh [publisher]     (CI: test, shard 0)
#
#   * MUSTER_PACKAGES_TOKEN unset: says it skipped, exit 0, NO commit anywhere;
#   * set: Formula/muster.rb + Formula/muster-mirror.rb in the tap, and
#     bucket/muster.json in the bucket, byte-identical to dist, ONE commit each
#     named "muster 1.2.3";
#   * a re-run commits nothing (idempotent);
#   * the next version commits again; an OLDER version afterwards changes
#     nothing (never backwards);
#   * the token is not in the script's output (it reaches git by environment only).
# Exit 0 all pass; 1 a case failed; 2 harness error.
set -uo pipefail

ROOT="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
PUB="${1:-$ROOT/.github/scripts/publish-packages.sh}"
[ -x "$PUB" ] || { echo "publish-packages-test: $PUB is not executable" >&2; exit 2; }
work="$(mktemp -d)"; trap 'rm -rf "$work"' EXIT
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
TOKEN="github_pat_FAKE0123456789"

# The two "GitHub" repositories: bare, default branch main, a README each.
mkdir -p "$work/gh/Owner"
for r in tap bucket; do
  git init -q --bare -b main "$work/gh/Owner/$r.git"
  git clone -q "$work/gh/Owner/$r.git" "$work/seed-$r" 2>/dev/null
  echo "# $r" > "$work/seed-$r/README.md"
  git -C "$work/seed-$r" add README.md
  git -C "$work/seed-$r" -c user.name=t -c user.email=t@example.invalid commit -q -m init
  git -C "$work/seed-$r" push -q origin HEAD:main
done

# dist <version>: that version's REAL files, from scripts/release-manifests.py
# (so the "never backwards" version read is tested on the format it reads).
dist() {
  local d="$work/dist-$1" a i=0; mkdir -p "$d"
  case "$1" in [0-9]*.[0-9]*.[0-9]*) ;; *) echo "$d"; return ;; esac
  for a in muster_darwin_amd64.tar.gz muster_darwin_arm64.tar.gz muster_linux_amd64.tar.gz muster_linux_arm64.tar.gz \
           muster-mirror_darwin_amd64.tar.gz muster-mirror_darwin_arm64.tar.gz muster-mirror_linux_amd64.tar.gz \
           muster-mirror_linux_arm64.tar.gz muster_windows_amd64.zip muster_windows_arm64.zip; do
    i=$((i + 1)); printf '%064x  %s\n' "$i" "$a"
  done > "$d/sums"
  python3 "$ROOT/scripts/release-manifests.py" --version "$1" --repo Owner/proj --checksums "$d/sums" --out "$d" || exit 2
  rm -f "$d/sums"
  echo "$d"
}
run() { # <version> [token]
  env PACKAGES_REMOTE_BASE="file://$work/gh" PACKAGES_TAP=Owner/tap PACKAGES_BUCKET=Owner/bucket \
    ${2:+MUSTER_PACKAGES_TOKEN="$2"} "$PUB" "$1" "$(dist "$1")" > "$work/out" 2>&1
}
commits() { git --git-dir "$work/gh/Owner/$1.git" rev-list --count main; }
show() { git --git-dir "$work/gh/Owner/$1.git" show "main:$2" 2>/dev/null; }

fails=0; passes=0
check() { if eval "$2"; then echo "PASS  $1"; passes=$((passes + 1)); else echo "FAIL  $1"; sed 's/^/        /' "$work/out"; fails=$((fails + 1)); fi; }

run 1.2.3; rc=$?
check "no token: exit 0 and says it skipped" '[ $rc -eq 0 ] && grep -q "skipped: MUSTER_PACKAGES_TOKEN is not set" "$work/out"'
check "no token: no commit in either repository" '[ "$(commits tap)" = 1 ] && [ "$(commits bucket)" = 1 ]'

run 1.2.3 "$TOKEN"; rc=$?
check "token: exit 0" '[ $rc -eq 0 ]'
check "tap: Formula/muster.rb and Formula/muster-mirror.rb are dist's bytes" \
  '[ "$(show tap Formula/muster.rb)" = "$(cat "$work/dist-1.2.3/muster.rb")" ] && [ "$(show tap Formula/muster-mirror.rb)" = "$(cat "$work/dist-1.2.3/muster-mirror.rb")" ]'
check "bucket: bucket/muster.json is dist's bytes" '[ "$(show bucket bucket/muster.json)" = "$(cat "$work/dist-1.2.3/muster.json")" ]'
check "one commit each, named \"muster 1.2.3\"" \
  '[ "$(commits tap)" = 2 ] && [ "$(commits bucket)" = 2 ] && [ "$(git --git-dir "$work/gh/Owner/tap.git" log -1 --format=%s main)" = "muster 1.2.3" ] && [ "$(git --git-dir "$work/gh/Owner/bucket.git" log -1 --format=%s main)" = "muster 1.2.3" ]'
check "each README is kept, nothing else is added to either repository" \
  '[ "$(git --git-dir "$work/gh/Owner/tap.git" ls-tree -r --name-only main | tr "\n" " ")" = "Formula/muster-mirror.rb Formula/muster.rb README.md " ] &&
   [ "$(git --git-dir "$work/gh/Owner/bucket.git" ls-tree -r --name-only main | tr "\n" " ")" = "README.md bucket/muster.json " ]'
check "the token is not in the output" '! grep -qF "$TOKEN" "$work/out"'

run 1.2.3 "$TOKEN"; rc=$?
check "re-run: exit 0, no new commit (idempotent)" '[ $rc -eq 0 ] && [ "$(commits tap)" = 2 ] && [ "$(commits bucket)" = 2 ] && grep -q "already at 1.2.3" "$work/out"'

run 1.3.0 "$TOKEN"; rc=$?
check "next version: one more commit each, the files updated" \
  '[ $rc -eq 0 ] && [ "$(commits tap)" = 3 ] && [ "$(commits bucket)" = 3 ] && show bucket bucket/muster.json | grep -q "\"version\": \"1.3.0\"" && show tap Formula/muster-mirror.rb | grep -q "version \"1.3.0\""'
run 1.2.4 "$TOKEN"; rc=$?
check "an OLDER version afterwards changes nothing" \
  '[ $rc -eq 0 ] && [ "$(commits tap)" = 3 ] && [ "$(commits bucket)" = 3 ] && show tap Formula/muster.rb | grep -q "version \"1.3.0\"" && grep -q "newer than 1.2.4" "$work/out"'

run 1.2 "$TOKEN"; rc=$?
check "a version that is not X.Y.Z: exit 2" '[ $rc -eq 2 ]'

echo "publish-packages-test: $passes passed, $fails failed"
[ "$fails" -eq 0 ] && [ "$passes" -eq 12 ]

#!/usr/bin/env bash
# scripts/changelog-bump.sh on a scratch repository with a fake `gh`.
#
#   tests/changelog-bump-test.sh      (CI: test, shard 0)
#
#   * default version = the compose file's default: the bump writes a
#     CHANGELOG.md that lists it, newest first, and says so on ONE line;
#   * a re-run says "already" and leaves the file byte-identical;
#   * a version with no tag, or gh failing: exit 1, CHANGELOG.md UNCHANGED.
# Exit 0 all pass; 1 a case failed; 2 harness error.
set -uo pipefail

ROOT="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
BUMP="$ROOT/scripts/changelog-bump.sh"
work="$(mktemp -d)"; trap 'rm -rf "$work"' EXIT
repo="$work/repo"; mkdir -p "$repo/deploy" "$work/bin"
g() { git -C "$repo" -c user.name=t -c user.email=t@example.invalid -c commit.gpgsign=false -c tag.gpgsign=false "$@"; }
git init -q -b main "$repo"
# a compose file in the shape stamp-compose-version.sh reads
cp "$ROOT/deploy/docker-compose.yml" "$repo/deploy/docker-compose.yml"
"$ROOT/scripts/stamp-compose-version.sh" 1.1.0 < "$ROOT/deploy/docker-compose.yml" > "$repo/deploy/docker-compose.yml"
g add deploy; g commit -q -m initial; g tag v1.0.0
g commit -q --allow-empty -m "feat: one (#1)"; g tag v1.1.0
cat > "$work/bin/gh" <<'GH'
#!/bin/sh
[ -n "${FAKE_GH_FAIL:-}" ] && { echo "HTTP 502" >&2; exit 1; }
echo '[{"number": 1, "title": "feat: one", "url": "u1", "labels": []}]'
GH
chmod +x "$work/bin/gh"
run() { env PATH="$work/bin:$PATH" CHANGELOG_REPO="$repo" "$@" "$BUMP" ${BUMP_ARG:-} > "$work/out" 2>&1; }

fails=0; passes=0
check() { if eval "$2"; then echo "PASS  $1"; passes=$((passes + 1)); else echo "FAIL  $1"; sed 's/^/        /' "$work/out"; fails=$((fails + 1)); fi; }

run; rc=$?
check "bump: exit 0, one line naming the compose default v1.1.0" \
  '[ $rc -eq 0 ] && [ "$(cat "$work/out")" = "changelog-bump: CHANGELOG.md now lists v1.1.0 (2 releases)" ]'
check "CHANGELOG.md lists v1.1.0 above v1.0.0, with its pull request" \
  '[ "$(grep "^## v" "$repo/CHANGELOG.md" | tr "\n" " ")" = "## v1.1.0 ## v1.0.0 " ] && grep -qxF -- "- one ([#1](u1))" "$repo/CHANGELOG.md"'
cp "$repo/CHANGELOG.md" "$work/before"
run; rc=$?
check "re-run: already, byte-identical" \
  '[ $rc -eq 0 ] && grep -q "already lists v1.1.0 (unchanged)" "$work/out" && cmp -s "$repo/CHANGELOG.md" "$work/before"'
BUMP_ARG=1.2.0 run; rc=$?
check "a version with no tag: exit 1, CHANGELOG.md unchanged" \
  '[ $rc -eq 1 ] && grep -q "has no v1.2.0" "$work/out" && cmp -s "$repo/CHANGELOG.md" "$work/before"'
g commit -q --allow-empty -m "fix: two (#2)"; g tag v1.2.0
BUMP_ARG=1.2.0 run FAKE_GH_FAIL=1; rc=$?
check "gh failing: exit 1, CHANGELOG.md unchanged" '[ $rc -eq 1 ] && cmp -s "$repo/CHANGELOG.md" "$work/before"'
BUMP_ARG=1.2 run; rc=$?
check "not X.Y.Z: exit 2" '[ $rc -eq 2 ]'

echo "changelog-bump-test: $passes passed, $fails failed"
[ "$fails" -eq 0 ] && [ "$passes" -eq 6 ]

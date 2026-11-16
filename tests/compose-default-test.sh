#!/usr/bin/env bash
# scripts/check-compose-default.sh against a scratch repository: the rule that
# the repository's deploy/docker-compose.yml defaults to the newest release.
#
#   tests/compose-default-test.sh        (CI: release-dry-run)
#
# Cases, each asserting the exit code AND the line that says why:
#   * the default IS the newest release tag below HEAD           -> ok
#   * the default is an OLDER release                             -> FAIL, "Bump it"
#   * a tag ON HEAD itself is not counted (the tag run of vN, whose
#     commit cannot already say N)                                -> ok
#   * a commit AFTER that tagged one must say N                   -> FAIL
#   * versions compare as versions (0.10.0 is newer than 0.9.0); a
#     non-release tag (v0.11.0-rc1) is not a release               -> as such
#   * a `main` (moving) default is refused                         -> rc 1
#   * no release tag at all (a shallow clone)                      -> rc 2
#   * --bump rewrites every default to the newest release, and the check
#     then passes
# No network, no docker.
set -euo pipefail

ROOT="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
CHECK="$ROOT/scripts/check-compose-default.sh"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
repo="$tmp/repo"
g() { git -C "$repo" -c user.name=t -c user.email=t@example.com -c commit.gpgsign=false -c tag.gpgsign=false "$@"; }
compose() { # a compose file with two image defaults at $1
  printf 'services:\n  a:\n    image: ${A_IMAGE:-ghcr.io/x/a}:${MUSTER_VERSION:-%s}\n  b:\n    image: ${B_IMAGE:-ghcr.io/x/b}:${MUSTER_VERSION:-%s}\n' "$1" "$1" > "$repo/c.yml"
}
commit() { compose "$1"; g add c.yml; g commit -q --allow-empty -m "$2"; }
run() { CHECK_COMPOSE_FILE="$repo/c.yml" CHECK_COMPOSE_REPO="$repo" "$CHECK" "$@" > "$tmp/out" 2>&1; echo $?; }
fail=0
expect() { # <name> <want rc> <want text> <got rc>
  if [ "$4" = "$2" ] && grep -qF -- "$3" "$tmp/out"; then echo "ok   $1"
  else echo "FAIL $1: rc $4 (want $2), output: $(cat "$tmp/out")"; fail=1; fi
}

mkdir -p "$repo"; git init -q "$repo"
commit 0.1.0 c1
expect "no release tag at all is a harness error" 2 "no vX.Y.Z release tag" "$(run || true)"
g tag v0.1.0
commit 0.1.0 c2; g tag v0.2.0
commit 0.2.0 c3
expect "the default is the newest release" 0 "defaults to 0.2.0, the newest release (v0.2.0)" "$(run || true)"
commit 0.1.0 c4
expect "an older default is refused" 1 "defaults to 0.1.0, but the newest release before this commit is v0.2.0" "$(run || true)"
grep -qF 'Bump it: scripts/check-compose-default.sh --bump' "$tmp/out" || { echo "FAIL the refusal does not say how to bump"; fail=1; }
commit 0.2.0 c5; g tag v0.3.0
expect "a tag on HEAD itself is not counted" 0 "the newest release (v0.2.0)" "$(run || true)"
g commit -q --allow-empty -m c6
expect "a commit after the tagged one must say its version" 1 "the newest release before this commit is v0.3.0" "$(run || true)"
commit 0.3.0 c7; g tag v0.9.0; commit 0.9.0 c8; g tag v0.10.0; g tag v0.11.0-rc1
g commit -q --allow-empty -m c9
expect "0.10.0 is newer than 0.9.0, and an rc tag is not a release" 1 "the newest release before this commit is v0.10.0" "$(run || true)"
expect "--bump rewrites the default" 0 "now defaults to 0.10.0 (was 0.9.0)" "$(run --bump || true)"
[ "$(grep -c 'MUSTER_VERSION:-0.10.0}' "$repo/c.yml")" = 2 ] || { echo "FAIL --bump did not rewrite both defaults: $(cat "$repo/c.yml")"; fail=1; }
expect "after --bump the check passes" 0 "defaults to 0.10.0, the newest release (v0.10.0)" "$(run || true)"
compose main
expect "a moving main default is refused" 1 "not a release version" "$(run || true)"
exit "$fail"

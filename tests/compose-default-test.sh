#!/usr/bin/env bash
# scripts/check-compose-default.sh against a scratch repository: the rules
# that deploy/docker-compose.yml defaults to a release that can run it
# (x-muster-min-version), and to the newest one after a grace period.
#
#   tests/compose-default-test.sh        (CI: release-dry-run)
#
# Every case asserts the exit code AND the line that says why. Tags get fixed,
# DIFFERENT dates (a check that read the wrong tag's date is caught), and the
# clock is CHECK_COMPOSE_NOW. Fixture versions are chosen so no two of
# latest, previous, next-minor(latest) and the minimum coincide by accident.
#
# THE NEWEST RELEASE
#   * no release tag at all (a shallow clone)                       -> rc 2
#   * the default IS the newest release tag below HEAD              -> ok
#   * the PREVIOUS release, inside the grace period (3 days)        -> rc 0, WARN, state grace
#     ... one second before the period ends                          -> grace
#     ... exactly when it ends, and after                            -> FAIL
#     ... and CHECK_COMPOSE_GRACE_DAYS sets its length               -> FAIL after 1 day at 1
#   * older than the previous release, even inside the period       -> FAIL
#   * AHEAD of the newest release                                    -> FAIL
#   * a tag ON HEAD itself is not counted                            -> ok
#   * 0.10.0 is newer than 0.9.0; an rc tag is not a release
#   * a `main` (moving) default                                      -> rc 1
# THE MINIMUM (x-muster-min-version)
#   * NEGATIVE CONTROL: a default image too old for the file, the
#     minimum released                                               -> FAIL
#   * the bootstrap: minimum = next-minor(latest), default = latest  -> rc 0, WARN, state bootstrap
#     ... for 3 days from the commit that RAISED the minimum           -> then FAIL
#     ... counted from that commit, not HEAD; CHECK_COMPOSE_BOOTSTRAP_DAYS sets it
#     ... the minimum raised today; not committed yet (counts from now) -> bootstrap
#   * a minimum two releases out                                     -> FAIL
#   * a next-PATCH minimum (not what the release flow mints)         -> FAIL
#   * the bootstrap minimum with a STALE default                     -> FAIL
#   * a minimum above the default inside the grace period            -> FAIL (grace never rescues it)
#   * no minimum line, or two                                        -> rc 2
# --bump
#   * --bump X.Y.Z to a tag ON HEAD (main not moved since the tag)   -> rewrites, then the check passes
#   * --bump to a version never tagged                               -> rc 2, file unchanged
#   * --bump with no version: the newest release below HEAD; "already"
# No network, no docker.
set -euo pipefail

ROOT="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
CHECK="$ROOT/scripts/check-compose-default.sh"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
repo="$tmp/repo"
DAY=86400
T0=1700000000          # every commit and tag is dated T0 + an offset (seconds)
at=0                   # the offset the next commit/tag gets
g() {
  GIT_AUTHOR_DATE="@$((T0 + at)) +0000" GIT_COMMITTER_DATE="@$((T0 + at)) +0000" \
    git -C "$repo" -c user.name=t -c user.email=t@example.com -c commit.gpgsign=false -c tag.gpgsign=false "$@"
}
tag() { g tag -a -m "$1" "$1"; }   # annotated, as muster-ops pushes them
MIN=0.1.0
compose() { # a compose file with two image defaults at $1, and the minimum $MIN (or none: MIN=)
  { [ -z "$MIN" ] || printf 'x-muster-min-version: "%s"\n\n' "$MIN"
    printf 'services:\n  a:\n    image: ${A_IMAGE:-ghcr.io/x/a}:${MUSTER_VERSION:-%s}\n  b:\n    image: ${B_IMAGE:-ghcr.io/x/b}:${MUSTER_VERSION:-%s}\n' "$1" "$1"
  } > "$repo/c.yml"
}
commit() { compose "$1"; g add c.yml; g commit -q --allow-empty -m "$2"; }
NOW=$T0
run() { CHECK_COMPOSE_FILE="$repo/c.yml" CHECK_COMPOSE_REPO="$repo" CHECK_COMPOSE_NOW="$NOW" "$CHECK" "$@" > "$tmp/out" 2>&1; echo $?; }
fail=0
expect() { # <name> <want rc> <want text> <got rc>
  if [ "$4" = "$2" ] && grep -qF -- "$3" "$tmp/out"; then echo "ok   $1"
  else echo "FAIL $1: rc $4 (want $2), output: $(cat "$tmp/out")"; fail=1; fi
}
state() { # <name> <want state>
  local got; got="$(CHECK_COMPOSE_FILE="$repo/c.yml" CHECK_COMPOSE_REPO="$repo" CHECK_COMPOSE_NOW="$NOW" "$CHECK" --state 2>/dev/null || echo "rc=$?")"
  if [ "$got" = "$2" ]; then echo "ok   $1: --state says $2"; else echo "FAIL $1: --state says '$got', want $2"; fail=1; fi
}

mkdir -p "$repo"; git init -q "$repo"
commit 0.1.0 c1
expect "no release tag at all is a harness error" 2 "no vX.Y.Z release tag" "$(run || true)"
tag v0.1.0                                  # v0.1.0 at T0
at=$((10 * DAY)); commit 0.1.0 c2; tag v0.2.0   # v0.2.0 at T0+10d
at=$((11 * DAY)); commit 0.2.0 c3
NOW=$((T0 + 11 * DAY))
expect "the default is the newest release" 0 "defaults to 0.2.0, the newest release (v0.2.0)" "$(run || true)"
state "the newest release" ok

# The grace period: v0.2.0 is 1 day old; the default is the previous release.
commit 0.1.0 c4
expect "the previous release inside the grace period WARNS" 0 "WARN deploy/docker-compose.yml defaults to 0.1.0; v0.2.0 was released 24h ago" "$(run || true)"
grep -qF 'compose: default to v0.2.0' "$tmp/out" || { echo "FAIL the grace warning does not name the bump pull request"; fail=1; }
state "the previous release inside the grace period" grace
NOW=$((T0 + 13 * DAY - 1))
expect "one second before the period ends it still WARNS" 0 "WARN deploy/docker-compose.yml defaults to 0.1.0" "$(run || true)"
NOW=$((T0 + 13 * DAY))
expect "exactly when the period ends it FAILS" 1 "released 3 days ago (the grace period is 3 days)" "$(run || true)"
grep -qF 'Bump it: scripts/check-compose-default.sh --bump 0.2.0' "$tmp/out" || { echo "FAIL the refusal does not say how to bump"; fail=1; }
NOW=$((T0 + 30 * DAY))
expect "long after the period it FAILS" 1 "released 20 days ago" "$(run || true)"
state "past the grace period" "rc=1"
NOW=$((T0 + 11 * DAY))
expect "CHECK_COMPOSE_GRACE_DAYS=1 ends the period after 1 day (the length is not hardcoded)" 1 "(the grace period is 1 days)" "$(CHECK_COMPOSE_GRACE_DAYS=1 run || true)"

# Older than the previous release: no grace, even on the day of the release.
at=$((20 * DAY)); commit 0.2.0 c5; tag v0.3.0     # v0.3.0 at T0+20d
at=$((20 * DAY + 60)); commit 0.1.0 c6
NOW=$((T0 + 20 * DAY + 3600))
expect "older than the previous release FAILS inside the period" 1 "older than the previous release, 0.2.0: no grace period" "$(run || true)"
commit 0.4.0 c7
expect "a default AHEAD of the newest release FAILS" 1 "defaults to 0.4.0, AHEAD of the newest release before this commit (v0.3.0)" "$(run || true)"

# A tag on HEAD itself is not counted (the tag run of vN).
commit 0.3.0 c8; tag v0.4.0
expect "a tag on HEAD itself is not counted" 0 "the newest release (v0.3.0)" "$(run || true)"

# Versions compare as versions; an rc tag is not a release.
at=$((40 * DAY)); commit 0.4.0 c9; tag v0.9.0
at=$((41 * DAY)); commit 0.9.0 c10; tag v0.10.0; tag v0.11.0-rc1
at=$((41 * DAY + 60)); g commit -q --allow-empty -m c11
NOW=$((T0 + 60 * DAY))
expect "0.10.0 is newer than 0.9.0, and an rc tag is not a release" 1 "the newest release before this commit is v0.10.0" "$(run || true)"

# --bump with no version: the newest release below HEAD.
expect "--bump rewrites the default to the newest release" 0 "now defaults to 0.10.0 (was 0.9.0)" "$(run --bump || true)"
[ "$(grep -c 'MUSTER_VERSION:-0.10.0}' "$repo/c.yml")" = 2 ] || { echo "FAIL --bump did not rewrite both defaults: $(cat "$repo/c.yml")"; fail=1; }
expect "after --bump the check passes" 0 "defaults to 0.10.0, the newest release (v0.10.0)" "$(run || true)"
expect "--bump again says already" 0 "check-compose-default: already 0.10.0" "$(run --bump || true)"
[ "$(wc -l < "$tmp/out")" = 1 ] || { echo "FAIL --bump printed more than one line: $(cat "$tmp/out")"; fail=1; }

# --bump X.Y.Z to a tag ON HEAD: the release flow, main not moved since the tag.
g add c.yml; g commit -q -m c12; at=$((50 * DAY)); tag v0.12.0
expect "--bump 0.12.0 to the tag on HEAD rewrites" 0 "check-compose-default: deploy/docker-compose.yml now defaults to 0.12.0 (was 0.10.0)" "$(run --bump 0.12.0 || true)"
[ "$(wc -l < "$tmp/out")" = 1 ] || { echo "FAIL --bump X.Y.Z printed more than one line: $(cat "$tmp/out")"; fail=1; }
g add c.yml; g commit -q -m c13
expect "and the bump commit then passes" 0 "defaults to 0.12.0, the newest release (v0.12.0)" "$(run || true)"
cp "$repo/c.yml" "$tmp/before"
expect "--bump to a version never tagged is refused" 2 "there is no release tag v0.13.0 on HEAD or below it" "$(run --bump 0.13.0 || true)"
cmp -s "$repo/c.yml" "$tmp/before" || { echo "FAIL a refused --bump changed the file"; fail=1; }
expect "--bump to a non-version is refused" 2 "is not X.Y.Z" "$(run --bump latest || true)"

# THE MINIMUM. Releases now: ... v0.10.0 (T0+41d), v0.12.0 (T0+50d); latest
# 0.12.0, previous 0.10.0, next-minor(latest) 0.13.0.
NOW=$((T0 + 51 * DAY))   # v0.12.0 is 1 day old: inside the grace period
MIN=0.12.0; commit 0.10.0 c14
expect "NEGATIVE CONTROL: a default too old for the file (the minimum released) FAILS, even in the grace period" 1 \
  "defaults to 0.10.0, but this file needs muster 0.12.0 or newer (x-muster-min-version), and a release that can run it is out (v0.12.0)" "$(run || true)"
state "a default below a released minimum" "rc=1"
MIN=0.11.0; commit 0.10.0 c15
expect "a released minimum above the default FAILS" 1 "needs muster 0.11.0 or newer (x-muster-min-version), and a release that can run it is out (v0.12.0)" "$(run || true)"
MIN=0.13.0; commit 0.12.0 c16
expect "the bootstrap (minimum = the next release, default = the newest) WARNS" 0 \
  "WARN deploy/docker-compose.yml needs muster 0.13.0 (x-muster-min-version), the NEXT release, not yet cut; it defaults to 0.12.0" "$(run || true)"
state "the bootstrap" bootstrap
GITHUB_ACTIONS=true state "the bootstrap, in GitHub Actions (no ::warning on --state's stdout)" bootstrap
expect "in GitHub Actions the warning is also a ::warning annotation" 0 "::warning title=compose default::deploy/docker-compose.yml needs muster 0.13.0" "$(GITHUB_ACTIONS=true run || true)"

# THE BOOTSTRAP WINDOW (#707 round 3): 3 days from the commit that RAISED the
# minimum (c16, at T0+50d), not from HEAD, the tag, or now.
c16="$(git -C "$repo" rev-parse --short HEAD)"
expect "inside the bootstrap window the warning names the raising commit" 0 "The minimum was raised in commit $c16, 24h ago; this fails 3 days after that" "$(run || true)"
NOW=$((T0 + 53 * DAY - 1))
state "the bootstrap one second before its window ends" bootstrap
NOW=$((T0 + 53 * DAY))
expect "the bootstrap FAILS exactly when its window ends" 1 "has needed muster 0.13.0 (x-muster-min-version), the NEXT release, since commit $c16, 3 days ago, and 0.13.0 is still not released: the bootstrap is allowed for 3 days" "$(run || true)"
grep -qF 'Release 0.13.0 now (muster-ops ship)' "$tmp/out" || { echo "FAIL the bootstrap refusal does not say how out"; fail=1; }
state "past the bootstrap window" "rc=1"
at=$((52 * DAY)); g commit -q --allow-empty -m c16b   # a later commit, the line untouched
NOW=$((T0 + 54 * DAY))
expect "the window counts from the commit that raised the minimum, not HEAD" 1 "since commit $c16, 4 days ago" "$(run || true)"
expect "CHECK_COMPOSE_BOOTSTRAP_DAYS sets its length" 0 "this fails 5 days after that" "$(CHECK_COMPOSE_BOOTSTRAP_DAYS=5 run || true)"
# The minimum raised TODAY (lowered, then raised again an hour ago): a new window.
at=$((60 * DAY - 7200)); MIN=0.12.0; commit 0.12.0 c16c
at=$((60 * DAY - 3600)); MIN=0.13.0; commit 0.12.0 c16d
NOW=$((T0 + 60 * DAY))
expect "the minimum raised today is in the bootstrap" 0 "The minimum was raised in commit $(git -C "$repo" rev-parse --short HEAD), 1h ago" "$(run || true)"
state "the minimum raised today" bootstrap
# Not committed yet: counted from now (a branch being written).
NOW=$((T0 + 90 * DAY))
expect "control: that line, committed 30 days ago, FAILS" 1 "since commit $(git -C "$repo" rev-parse --short HEAD), 30 days ago" "$(run || true)"
sed -i 's/^x-muster-min-version: "0.13.0"$/x-muster-min-version:  "0.13.0"/' "$repo/c.yml"   # the same minimum, a line no commit has
expect "a minimum line not committed yet counts from now" 0 "raised in this working tree (not committed yet), 0h ago" "$(run || true)"
NOW=$((T0 + 51 * DAY)); at=$((60 * DAY))
MIN=0.14.0; commit 0.12.0 c17
expect "a minimum two releases out FAILS" 1 "The only unreleased minimum allowed is the NEXT release, 0.13.0" "$(run || true)"
MIN=0.12.1; commit 0.12.0 c18
expect "a next-PATCH minimum FAILS (the release flow mints the next minor)" 1 "needs muster 0.12.1 (x-muster-min-version) and defaults to 0.12.0" "$(run || true)"
MIN=0.13.0; commit 0.10.0 c19
expect "the bootstrap minimum with a stale default FAILS" 1 "The only unreleased minimum allowed is the NEXT release, 0.13.0, with the default at the newest release, v0.12.0" "$(run || true)"
MIN=0.10.0; commit 0.10.0 c20
expect "a minimum at or below the default passes the minimum (here: the grace period)" 0 "WARN deploy/docker-compose.yml defaults to 0.10.0; v0.12.0 was released 24h ago" "$(run || true)"
MIN=; commit 0.12.0 c21
expect "no minimum line is a harness error" 2 "must state its minimum muster once" "$(run || true)"
MIN=0.1.0; commit 0.12.0 c22; printf 'x-muster-min-version: "0.1.0"\n' >> "$repo/c.yml"
expect "two minimum lines is a harness error" 2 "must state its minimum muster once" "$(run || true)"
compose main
expect "a moving main default is refused" 1 "not a release version" "$(run || true)"
exit "$fail"

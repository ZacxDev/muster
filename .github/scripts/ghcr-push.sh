#!/usr/bin/env bash
# Publish a locally built image to GHCR without ever moving a tag that is
# already there. Used by the `image-*` jobs in .github/workflows/ci.yml.
#
#   ghcr-push.sh probe <remote-ref>                    print present|absent
#   ghcr-push.sh push  <local-image> <remote-ref> reuse   absent: push; present: keep it
#   ghcr-push.sh push  <local-image> <remote-ref> refuse  absent: push; present: FAIL
#   ghcr-push.sh push  <local-image> <remote-ref> release absent: push; present: keep it
#       ONLY IF its org.opencontainers.image.revision label is $GHCR_REVISION
#       (this commit) on every platform, else FAIL
#   ghcr-push.sh buildx <remote-ref> reuse|refuse|release <platforms> -- <docker buildx build args…>
#       the same probe, then a MULTI-ARCH `docker buildx build --platform
#       <platforms> --push -t <remote-ref> <args…>`. A multi-platform image
#       cannot be loaded into the local image store, so it is built and pushed
#       in one step instead of `push`'s tag-and-push of a local image.
#
# 🔴 A PUBLISHED TAG IS NEVER OVERWRITTEN, AND THAT IS THE WHOLE POINT OF THIS
# SCRIPT. A docker build is not byte-reproducible, so re-running a `main` run
# (`gh run rerun`) would otherwise re-point `<image>:<sha>` at a different
# digest than the one a deploy may already have pinned — the same hazard
# devrc's `muster-ops release` guards against for the Harbor tags. `reuse` is for
# tags that name a commit (the same commit gives the same source, so the
# existing image stands); `refuse` is for a release version, where a second
# image under one version is a mistake to stop on, not to paper over.
#
# 🔴 `release` IS `refuse` MADE IDEMPOTENT, FOR THE vX.Y.Z TAG RUNS. A tag can be
# pushed again, a tag run can be re-run (`gh run rerun --failed` after a flaky
# job), and a release job can fail after the images went up. Under `refuse`
# each of those turned a published, correct image into a red run. `release`
# keeps an existing version when — and only when — the registry's copy was
# built from THIS commit (its revision label, read per platform); a version
# already holding a DIFFERENT commit's image is still refused, which is the
# mistake `refuse` exists to stop.
#
# 🔴 ABSENCE IS CLASSIFIED, NEVER INFERRED FROM A FAILURE. `probe` reports
# `absent` only on the registry's own not-found answers; any other failure (auth,
# network, rate limit) exits 2 with the registry's message. Otherwise a broken
# login would read as "nothing published yet" and the push would decide what a
# tag means.
#
# Exit codes: 0 done / probe answered; 1 refused (tag taken under `refuse`, or
# under `release` by another commit's image, or a push failed); 2 harness error (bad arguments, a probe it could not classify).
set -euo pipefail

die() { echo "ghcr-push: $1" >&2; exit "${2:-1}"; }

# Before any probe: a missing docker prints "command not found", which the
# not-found classification below would otherwise read as "absent".
command -v docker >/dev/null 2>&1 || die "docker is not on PATH" 2

probe() {  # present | absent on stdout; exit 2 on anything else
  local ref="$1" out
  if out="$(docker manifest inspect "$ref" 2>&1)"; then
    echo present; return 0
  fi
  # GHCR's answers for a missing tag (`manifest unknown`) and for a package that
  # has never been published (measured anonymously: also `manifest unknown`).
  if grep -qiE 'manifest unknown|name unknown|no such manifest' <<<"$out"; then
    echo absent; return 0
  fi
  echo "ghcr-push: could not tell whether $ref exists: $out" >&2
  exit 2
}

summary() { [ -n "${GITHUB_STEP_SUMMARY:-}" ] && printf '%s\n' "$*" >> "$GITHUB_STEP_SUMMARY" || true; }

revision_of() {  # the revision label(s) of a remote ref, one per line, deduplicated
  docker buildx imagetools inspect "$1" --format '{{json .Image}}' | python3 -c '
import json, sys
d = json.load(sys.stdin)
imgs = [d] if "config" in d else list(d.values())
revs = sorted({(i.get("config") or {}).get("Labels", {}).get("org.opencontainers.image.revision", "") for i in imgs})
print("\n".join(revs))'
}

# on_present <ref> <mode>: the ONE decision for a tag that is already published.
# Exits 0 (kept) or dies (refused).
on_present() {
  local ref="$1" mode="$2" d revs
  case "$mode" in
    refuse) die "$ref is already published; a release version is never re-pushed — cut the next version" ;;
    release)
      [ -n "${GHCR_REVISION:-}" ] || die "mode release needs GHCR_REVISION (the commit this run builds)" 2
      revs="$(revision_of "$ref")"
      [ "$revs" = "$GHCR_REVISION" ] ||
        die "$ref is already published from a DIFFERENT commit (revision: ${revs:-none}; this run: $GHCR_REVISION) — a release version is never re-pointed; cut the next version" ;;
  esac
  d="$(digest_of "$ref")"
  echo "ghcr-push: $ref already published at $d ($mode) — kept, not overwritten"
  summary "- \`$ref@$d\` (already published, kept)"
  exit 0
}

check_mode() { case "$1" in reuse|refuse|release) ;; *) die "mode must be reuse, refuse or release, got '$1'" 2 ;; esac; }

digest_of() {  # the digest the registry serves for a ref
  docker buildx imagetools inspect "$1" --format '{{json .Manifest}}' | python3 -c 'import json,sys; print(json.load(sys.stdin)["digest"])'
}

cmd="${1:-}"
case "$cmd" in
  probe)
    [ $# -eq 2 ] || die "usage: $0 probe <remote-ref>" 2
    probe "$2"
    ;;
  push)
    [ $# -eq 4 ] || die "usage: $0 push <local-image> <remote-ref> reuse|refuse|release" 2
    local_img="$2"; ref="$3"; mode="$4"
    check_mode "$mode"
    docker image inspect "$local_img" >/dev/null 2>&1 || die "no local image $local_img" 2
    state="$(probe "$ref")"
    [ "$state" = present ] && on_present "$ref" "$mode"
    docker tag "$local_img" "$ref"
    docker push "$ref"
    d="$(digest_of "$ref")"
    echo "ghcr-push: pushed $ref@$d"
    summary "- \`$ref@$d\` (pushed)"
    ;;
  buildx)
    [ $# -ge 5 ] && [ "$5" = "--" ] || die "usage: $0 buildx <remote-ref> reuse|refuse|release <platforms> -- <build args…>" 2
    ref="$2"; mode="$3"; platforms="$4"; shift 5
    check_mode "$mode"
    [ -n "$platforms" ] || die "no platforms given" 2
    state="$(probe "$ref")"
    [ "$state" = present ] && on_present "$ref" "$mode"
    docker buildx build --platform "$platforms" --push -t "$ref" "$@"
    d="$(digest_of "$ref")"
    echo "ghcr-push: pushed $ref@$d ($platforms)"
    summary "- \`$ref@$d\` (pushed, $platforms)"
    ;;
  *)
    die "usage: $0 probe <remote-ref> | push <local-image> <remote-ref> reuse|refuse|release | buildx <remote-ref> reuse|refuse|release <platforms> -- <args…>" 2
    ;;
esac

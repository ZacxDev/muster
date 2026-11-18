#!/usr/bin/env bash
# Publish dist/ as the GitHub Release for a tag — idempotently. Used by the
# `release` job in .github/workflows/ci.yml; tested by tests/release-publish-test.sh
# with a fake `gh`.
#
#   release-publish.sh <tag> <dist-dir> <notes-header-file>
#
# No release for the tag: create it with every asset in <dist-dir>, GitHub's
# generated notes (the PRs merged since the previous release tag) under the
# header file, and --verify-tag (never creates a tag that was not pushed).
#
# A release that EXISTS — a re-pushed tag, a re-run, or an attempt that died
# half-way — is COMPLETED, never replaced:
#   * every asset it already holds must be byte-identical to this build's
#     (the build is reproducible: same commit, same bytes) — a mismatch, or an
#     asset this build does not produce, fails without touching it;
#   * the assets it is missing are uploaded;
#   * 🔴 a DRAFT is published. `gh release create … <files>` creates a draft,
#     uploads, then publishes; a job killed mid-upload leaves a draft that a
#     re-run would otherwise "complete" and report green while nobody can see it.
#     (`gh release view <tag>` resolves drafts by their pending tag: gh's
#     FetchRelease runs a published lookup and a draft lookup and takes either.)
#
# Finally the published asset names must equal dist's (GitHub renames some
# names, e.g. a leading period) and the release must not be a draft.
#
# Exit 0 published/complete; 1 refused or failed; 2 usage.
set -euo pipefail

die() { echo "release-publish: $1" >&2; exit "${2:-1}"; }
[ $# -eq 3 ] || die "usage: $0 <tag> <dist-dir> <notes-header-file>" 2
tag="$1"; dist="$2"; header="$3"
[ -d "$dist" ] || die "no dist directory $dist" 2
[ -f "$header" ] || die "no notes header $header" 2

names_in_dist() { (cd "$dist" && find . -maxdepth 1 -type f -printf '%f\n' | LC_ALL=C sort); }

if gh release view "$tag" --json tagName >/dev/null 2>&1; then
  draft="$(gh release view "$tag" --json isDraft --jq .isDraft)"
  have="$(gh release view "$tag" --json assets --jq '.assets[].name' | LC_ALL=C sort)"
  echo "release-publish: $tag exists (draft: $draft); checking it against this build"
  work="$(mktemp -d)"; trap 'rm -rf "$work"' EXIT
  missing=()
  while IFS= read -r name; do
    [ -n "$name" ] || continue
    [ -f "$dist/$name" ] || die "release $tag holds '$name', which this build does not produce; refusing to touch it"
  done <<<"$have"
  if [ -n "$have" ]; then
    gh release download "$tag" --dir "$work" --clobber
    while IFS= read -r name; do
      [ -n "$name" ] || continue
      cmp -s "$work/$name" "$dist/$name" ||
        die "release $tag holds a DIFFERENT '$name' than this commit builds; refusing to replace published assets"
    done <<<"$have"
  fi
  while IFS= read -r name; do
    grep -qxF "$name" <<<"$have" || missing+=("$dist/$name")
  done < <(names_in_dist)
  if [ "${#missing[@]}" -gt 0 ]; then
    echo "release-publish: uploading ${#missing[@]} missing asset(s): ${missing[*]##*/}"
    gh release upload "$tag" "${missing[@]}"
  fi
  if [ "$draft" = true ]; then
    echo "release-publish: $tag is a DRAFT (an earlier attempt died before publishing it); publishing"
    gh release edit "$tag" --draft=false
  fi
else
  files=()
  while IFS= read -r name; do files+=("$dist/$name"); done < <(names_in_dist)
  gh release create "$tag" "${files[@]}" \
    --verify-tag \
    --title "muster $tag" \
    --notes-file "$header" \
    --generate-notes
fi

published="$(gh release view "$tag" --json assets --jq '.assets[].name' | LC_ALL=C sort)"
[ "$published" = "$(names_in_dist)" ] || die "the release's asset names differ from dist:
$(diff <(names_in_dist) <(echo "$published") || true)"
[ "$(gh release view "$tag" --json isDraft --jq .isDraft)" = false ] || die "release $tag is still a draft"
echo "release-publish: $tag is published and complete"

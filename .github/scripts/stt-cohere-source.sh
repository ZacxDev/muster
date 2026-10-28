#!/usr/bin/env bash
# Decide whether a release tag must BUILD muster-stt-cohere or may RETAG the
# previous release's image. Used by the `image-stt-cohere` job in
# .github/workflows/ci.yml; tested by tests/stt-cohere-source-test.sh.
#
#   stt-cohere-source.sh <commit> <this-tag>     prints `build` or `retag <prev-tag>`
#
# `retag <prev>` when, and only when, all of these hold:
#   * <prev> is the highest strict-semver `vX.Y.Z` tag reachable from <commit>
#     other than <this-tag> (pre-release spellings such as v1.2.3-rc1 ignored);
#   * `git diff --quiet <prev> <commit> -- images/stt-cohere/` says the
#     directory is byte-identical. A <prev> that predates the directory reads
#     as CHANGED (added files), so it builds — measured by the test's first
#     case. (The diff would read "unchanged" only if the directory were absent
#     on BOTH sides, and then there is nothing to build or retag: the caller's
#     registry probe finds no <prev> image and its build fails loudly.)
# Anything else is `build`. The registry check (does <prev>'s image exist?) is
# the caller's, which has the credentials.
#
# WHY: the image is ~9 GB, and a rebuild re-resolves apt's ffmpeg against a
# moving archive. Retagging keeps a release that changed nothing here on the
# exact bytes the previous one shipped.
set -euo pipefail
[ $# -eq 2 ] || { echo "usage: $0 <commit> <this-tag>" >&2; exit 2; }
commit="$1"; this="$2"
dir=images/stt-cohere
prev="$(git tag --merged "$commit" --sort=-v:refname |
  grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | grep -vxF "$this" | head -n 1 || true)"
if [ -z "$prev" ]; then echo build; exit 0; fi
if git diff --quiet "$prev" "$commit" -- "$dir/"; then echo "retag $prev"; else echo build; fi

#!/usr/bin/env bash
# .github/scripts/release-publish.sh against a FAKE `gh` that keeps a release's
# state on disk (exists / draft / assets), so every branch runs without GitHub.
#
#   tests/release-publish-test.sh [publisher]     (make release-publish-test)
#
# `publisher` defaults to the script; it is a parameter so the same cases can
# be pointed at another implementation (that is how the draft case was shown
# red against the job's previous inline logic).
#
# Cases: a fresh release; an existing complete one (a no-op); a DRAFT left by a
# died attempt, with and without checksums.txt (completed AND published); an
# existing asset with different bytes (refused, untouched); an existing asset
# this build does not produce (refused, untouched).
# Exit 0 all pass; 1 a case failed; 2 harness error.
set -uo pipefail

# CDPATH unset: with it exported, `cd` to a relative path PRINTS the directory,
# and ROOT became two lines (measured: every `make` target here broke).
ROOT="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
PUBLISH="${1:-$ROOT/.github/scripts/release-publish.sh}"
[ -x "$PUBLISH" ] || { echo "release-publish-test: $PUBLISH is not an executable file" >&2; exit 2; }
TAG=v1.2.3
work="$(mktemp -d)"; trap 'rm -rf "$work"' EXIT

# --- the fake gh -------------------------------------------------------------
mkdir -p "$work/bin"
cat > "$work/bin/gh" <<'GH'
#!/usr/bin/env bash
# State: $GH_STATE/{exists,draft,assets/,calls}
st="$GH_STATE"; echo "$*" >> "$st/calls"
[ "$1" = release ] || { echo "fake gh: unsupported $*" >&2; exit 2; }
sub="$2"; tag="$3"; shift 3
case "$sub" in
  view)
    [ -f "$st/exists" ] || { echo "release not found" >&2; exit 1; }
    case "$*" in
      *isDraft*) cat "$st/draft" ;;
      *assets*) ls "$st/assets" | LC_ALL=C sort ;;
      *) echo "{}" ;;
    esac ;;
  download)
    [ -f "$st/exists" ] || exit 1
    dir=""; while [ $# -gt 0 ]; do [ "$1" = --dir ] && { dir="$2"; shift; }; shift; done
    cp "$st/assets/"* "$dir/" 2>/dev/null; exit 0 ;;
  upload)
    for f in "$@"; do case "$f" in --*) ;; *) cp "$f" "$st/assets/" ;; esac; done ;;
  edit)
    case "$*" in *--draft=false*) echo false > "$st/draft" ;; esac ;;
  create)
    [ -f "$st/exists" ] && { echo "already exists" >&2; exit 1; }
    touch "$st/exists"; echo false > "$st/draft"; mkdir -p "$st/assets"
    while [ $# -gt 0 ]; do
      case "$1" in
        --title|--notes-file|--notes|--notes-start-tag|--target) shift ;;  # flags with a value
        --*) ;;
        *) cp "$1" "$st/assets/" ;;
      esac
      shift
    done ;;
  *) echo "fake gh: unsupported release $sub" >&2; exit 2 ;;
esac
GH
chmod +x "$work/bin/gh"

# --- a dist ------------------------------------------------------------------
dist="$work/dist"; mkdir -p "$dist"
for n in muster_linux_amd64.tar.gz muster-mirror_linux_amd64.tar.gz docker-compose.yml env.example; do
  echo "content of $n" > "$dist/$n"
done
(cd "$dist" && sha256sum muster* docker-compose.yml env.example > checksums.txt)
echo "## header" > "$work/header.md"

fails=0; passes=0
# run_case <name> <want-rc> <setup-fn> <check-fn>
run_case() {
  local name="$1" want="$2" setup="$3" check="$4"
  CASE="$name"
  export GH_STATE="$work/state-$name"; rm -rf "$GH_STATE"; mkdir -p "$GH_STATE/assets"; echo false > "$GH_STATE/draft"
  "$setup"
  ( cd "$work" && PATH="$work/bin:$PATH" GITHUB_REF_NAME="$TAG" GITHUB_REPOSITORY_OWNER=Example \
      "$PUBLISH" "$TAG" "$dist" "$work/header.md" ) > "$work/out-$name" 2>&1
  local rc=$?
  local why=""
  if [ "$want" = 0 ] && [ "$rc" -ne 0 ]; then why="exit $rc, want 0"
  elif [ "$want" != 0 ] && [ "$rc" -eq 0 ]; then why="exit 0, want a refusal"
  else why="$("$check")"; fi
  if [ -z "$why" ]; then echo "PASS  $name"; passes=$((passes + 1))
  else echo "FAIL  $name: $why"; sed 's/^/        /' "$work/out-$name"; fails=$((fails + 1)); fi
}
assets_eq_dist() { diff <(ls "$GH_STATE/assets" | LC_ALL=C sort) <(ls "$dist" | LC_ALL=C sort) >/dev/null || echo "assets != dist"; }
published() { [ "$(cat "$GH_STATE/draft")" = false ] || echo "release is still a DRAFT"; }

none() { :; }
check_fresh() {
  [ -f "$GH_STATE/exists" ] || { echo "no release created"; return; }
  assets_eq_dist; published
  grep -q -- '--verify-tag' "$GH_STATE/calls" || echo "create without --verify-tag"
  grep -q -- '--generate-notes' "$GH_STATE/calls" || echo "create without --generate-notes"
}
complete() { touch "$GH_STATE/exists"; cp "$dist"/* "$GH_STATE/assets/"; }
check_noop() {
  assets_eq_dist; published
  ! grep -qE '^release (upload|edit|create)' "$GH_STATE/calls" || echo "a complete release was modified: $(grep -E '^release (upload|edit|create)' "$GH_STATE/calls")"
}
draft_partial() { touch "$GH_STATE/exists"; echo true > "$GH_STATE/draft"; cp "$dist/checksums.txt" "$dist/docker-compose.yml" "$GH_STATE/assets/"; }
draft_no_checksums() { touch "$GH_STATE/exists"; echo true > "$GH_STATE/draft"; cp "$dist/docker-compose.yml" "$GH_STATE/assets/"; }
check_completed() { assets_eq_dist; published; }
different() { complete; echo "tampered" > "$GH_STATE/assets/muster_linux_amd64.tar.gz"; }
foreign() { complete; echo "x" > "$GH_STATE/assets/muster-server_linux_amd64.tar.gz"; }
check_untouched() {
  # A refusal must be THE refusal: a crash (exit 127, a typo) also exits non-zero.
  grep -q 'refusing to' "$work/out-$CASE" || { echo "exited non-zero without a refusal message"; return; }
  ! grep -qE '^release (upload|edit|create)' "$GH_STATE/calls" || echo "a refused release was modified"
}

run_case fresh 0 none check_fresh
run_case complete-is-a-noop 0 complete check_noop
run_case draft-partial-is-completed-and-published 0 draft_partial check_completed
run_case draft-without-checksums-is-completed-and-published 0 draft_no_checksums check_completed
run_case different-bytes-refused 1 different check_untouched
run_case foreign-asset-refused 1 foreign check_untouched

echo "release-publish-test: $passes passed, $fails failed"
[ "$passes" -eq 6 ] || exit 1
[ "$fails" -eq 0 ]

#!/usr/bin/env bash
# .github/scripts/ghcr-push.sh's step outputs (digest=, pushed=yes|no), against
# a FAKE docker that keeps each remote ref's existence on disk. The image jobs
# attest provenance only on pushed=yes: "yes" for an image this run did not
# push would attest another run's build as this one's, and "no" for one it
# did push would leave that image unattested.
#
#   tests/ghcr-push-output-test.sh        (CI: test, shard 0)
#
# Exit 0 all pass; 1 a case failed; 2 harness error.
set -uo pipefail

ROOT="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
PUSH="${1:-$ROOT/.github/scripts/ghcr-push.sh}"
work="$(mktemp -d)"; trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin"
# The fake: $FAKE_ST/<ref with / and : as _> exists = that ref is published.
# Every ref's digest is sha256:d1, and its revision label is $FAKE_REV.
cat > "$work/bin/docker" <<'D'
#!/usr/bin/env bash
st="$FAKE_ST"; echo "$*" >> "$st/calls"
key() { printf '%s' "$1" | tr '/:' '__'; }
last() { eval "printf '%s' \"\${$#}\""; }
tagarg() { while [ $# -gt 0 ]; do [ "$1" = -t ] && { printf '%s' "$2"; return; }; shift; done; }
case "$1 $2" in
  "manifest inspect") [ -f "$st/$(key "$3")" ] && { echo '{}'; exit 0; }; echo "manifest unknown" >&2; exit 1 ;;
  "image inspect") exit 0 ;;
  "tag "*) exit 0 ;;
  "push "*) touch "$st/$(key "$2")" ;;
  "buildx build") touch "$st/$(key "$(tagarg "$@")")" ;;
  "buildx imagetools")
    case "$3" in
      create) touch "$st/$(key "$(tagarg "$@")")" ;;
      inspect)
        case "$*" in
          *Manifest*) echo '{"digest": "sha256:d1"}' ;;
          *Image*) echo "{\"config\": {\"Labels\": {\"org.opencontainers.image.revision\": \"$FAKE_REV\"}}}" ;;
        esac ;;
    esac ;;
  *) echo "fake docker: unsupported $*" >&2; exit 2 ;;
esac
D
chmod +x "$work/bin/docker"

fails=0; passes=0
# run_case <name> "<published refs>" <want pushed> <ghcr-push args...>
run_case() {
  local name="$1" published="$2" want="$3"; shift 3
  export FAKE_ST="$work/st-$name"; mkdir -p "$FAKE_ST"
  for r in $published; do touch "$FAKE_ST/$(printf '%s' "$r" | tr '/:' '__')"; done
  local out="$work/out-$name"; : > "$out"
  if PATH="$work/bin:$PATH" GITHUB_OUTPUT="$out" GHCR_REVISION=abc FAKE_REV=abc "$PUSH" "$@" > "$work/log-$name" 2>&1; then
    got="$(tr '\n' ' ' < "$out")"
    if [ "$got" = "digest=sha256:d1 pushed=$want " ]; then echo "PASS  $name -> pushed=$want"; passes=$((passes + 1)); return; fi
    echo "FAIL  $name: outputs '$got', want 'digest=sha256:d1 pushed=$want '"
  else
    echo "FAIL  $name: exit $?"; sed 's/^/        /' "$work/log-$name"
  fi
  fails=$((fails + 1))
}
A=ghcr.io/o/i:1; B=ghcr.io/o/i:2
run_case push-absent "" yes push img:1 "$A" reuse
run_case push-present-kept "$A" no push img:1 "$A" reuse
run_case buildx-absent "" yes buildx "$A" release linux/amd64 -- .
run_case buildx-present-same-commit-kept "$A" no buildx "$A" release linux/amd64 -- .
run_case retag-new "$A" no retag "$A" "$B"
run_case retag-already-there "$A $B" no retag "$A" "$B"

# Without GITHUB_OUTPUT (a local run) it writes nothing and still pushes.
export FAKE_ST="$work/st-local"; mkdir -p "$FAKE_ST"
if PATH="$work/bin:$PATH" GHCR_REVISION=abc FAKE_REV=abc "$PUSH" push img:1 "$A" reuse > /dev/null 2>&1 &&
   [ -f "$FAKE_ST/ghcr.io_o_i_1" ]; then
  echo "PASS  no GITHUB_OUTPUT: pushes, writes no outputs"; passes=$((passes + 1))
else echo "FAIL  no GITHUB_OUTPUT"; fails=$((fails + 1)); fi

echo "ghcr-push-output-test: $passes passed, $fails failed"
[ "$fails" -eq 0 ] && [ "$passes" -eq 7 ]

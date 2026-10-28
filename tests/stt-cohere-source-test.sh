#!/usr/bin/env bash
# .github/scripts/stt-cohere-source.sh against a scratch git repository.
#   tests/stt-cohere-source-test.sh
# Exit 0 all pass; 1 a case failed.
set -uo pipefail
# CDPATH unset: with it exported, `cd` to a relative path PRINTS the directory,
# and ROOT became two lines (measured: every `make` target here broke).
ROOT="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="$ROOT/.github/scripts/stt-cohere-source.sh"
work="$(mktemp -d)"; trap 'rm -rf "$work"' EXIT
g() { git -C "$work" -c user.name=t -c user.email=t@example.com "$@"; }
g init -q
commit() { for p in images README; do [ -e "$work/$p" ] && g add -- "$p"; done; g commit -q --allow-empty -m "$1"; g rev-parse HEAD; }

passes=0; fails=0
expect() {  # <name> <commit> <this-tag> <want>
  local got; got="$(cd "$work" && "$SRC" "$2" "$3" 2>&1)"
  if [ "$got" = "$4" ]; then echo "PASS  $1"; passes=$((passes + 1))
  else echo "FAIL  $1: got '$got', want '$4'"; fails=$((fails + 1)); fi
}

c0="$(commit "before the image")"; g tag v0.9.0 "$c0"
mkdir -p "$work/images/stt-cohere"; echo A > "$work/images/stt-cohere/Dockerfile"
c1="$(commit "image added")"; g tag v0.10.0 "$c1"
expect "first tag with the image: the previous tag predates it -> build" "$c1" v0.10.0 build
g tag v0.99.0 "$c1"
expect "a second tag on the same commit -> retag" "$c1" v0.99.0 "retag v0.10.0"
echo "unrelated" > "$work/README"; c2="$(commit "unrelated change")"
g tag v0.99.1-rc1 "$c2"
expect "image unchanged since the previous tag -> retag (rc tags ignored)" "$c2" v0.99.1 "retag v0.99.0"
echo B > "$work/images/stt-cohere/Dockerfile"; c3="$(commit "image changed")"
expect "image changed since the previous tag -> build" "$c3" v0.99.2 build
expect "no previous tag reachable -> build" "$c0" v0.9.0 build

echo "stt-cohere-source-test: $passes passed, $fails failed"
[ "$passes" -eq 5 ] && [ "$fails" -eq 0 ]

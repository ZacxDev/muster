#!/usr/bin/env bash
# The development MUSTER_HOOK_TOKEN `make run` uses (muster-server does not
# start without one). Prints the token on stdout.
#
#   scripts/dev-hook-token.sh [FILE]     (default: .dev.env)
#
# - MUSTER_HOOK_TOKEN already in the environment wins, unchanged.
# - Otherwise FILE holds `export MUSTER_HOOK_TOKEN=<64 hex digits>`, made
#   once (mode 600) and reused.
#
# 🔴 IT NEVER WRITES OR REUSES A SHORT VALUE. The token comes from /dev/urandom
# through `od` (coreutils, in every dev shell) rather than `openssl`, which the
# dev shell does not carry — an earlier version wrote `$(openssl rand …)` and,
# without openssl, persisted an EMPTY token that every later run reused. A
# generated value that is not 64 hex digits is refused before it is written,
# and a FILE whose value is not one is refused rather than used.
set -euo pipefail

file=${1:-.dev.env}

if [ -n "${MUSTER_HOOK_TOKEN:-}" ]; then
  printf '%s\n' "$MUSTER_HOOK_TOKEN"
  exit 0
fi

valid() { [[ $1 =~ ^[0-9a-f]{64}$ ]]; }

if [ -e "$file" ]; then
  tok=$(sed -n 's/^export MUSTER_HOOK_TOKEN=//p' "$file" | head -n 1)
  if ! valid "$tok"; then
    echo "dev-hook-token: $file does not hold a 64-hex-digit MUSTER_HOOK_TOKEN; delete it to make a new one" >&2
    exit 1
  fi
  printf '%s\n' "$tok"
  exit 0
fi

tok=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
if ! valid "$tok"; then
  echo "dev-hook-token: could not generate a token (got ${#tok} characters from /dev/urandom); nothing written" >&2
  exit 1
fi
(umask 077 && printf 'export MUSTER_HOOK_TOKEN=%s\n' "$tok" > "$file")
printf '%s\n' "$tok"

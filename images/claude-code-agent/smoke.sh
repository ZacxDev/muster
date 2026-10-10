#!/usr/bin/env bash
# The image's built-in smoke test:
#
#   docker run --rm <image> cc-smoke
#
# It asserts, INSIDE the built image:
#   1. the container runs as uid 1000, not root;
#   2. the pinned CLI is the one on PATH;
#   3. the entrypoint's seed produced the onboarding/trust/hook shape (`ccd seed --check`);
#   4. ccd's /healthz answers with JSON;
#   5. the TUI reached its prompt: /healthz reports session_started=true, which only
#      a SessionStart hook can set — so this also proves the seeded hook wiring
#      reaches ccd through `ccd hook`.
#
# ⚠ IT NEEDS NO REAL CREDENTIAL AND MUST NEVER BE GIVEN ONE. Without any token the
# CLI stops at its login-method screen and never fires SessionStart, so a
# deliberately INVALID token is supplied when none is set; the auth verdict is then
# printed (expected: auth_failed, or probe_error offline) but not asserted, because
# it depends on reaching the provider's API.
set -euo pipefail

fail() { echo "cc-smoke: FAIL: $*" >&2; exit 1; }

[[ "$(id -u)" == "1000" ]] || fail "running as uid $(id -u), expected 1000"
echo "cc-smoke: uid $(id -u) ok"

want="${CLAUDE_CODE_VERSION:?the image sets CLAUDE_CODE_VERSION}"
got="$(claude --version 2>/dev/null || true)"
[[ "$got" == "$want "* ]] || fail "claude --version is '$got', expected '$want …'"
echo "cc-smoke: claude $got ok"

export CLAUDE_CODE_OAUTH_TOKEN="${CLAUDE_CODE_OAUTH_TOKEN:-sk-ant-oat01-SMOKE-DELIBERATELY-INVALID}"
export HOOKS_TOKEN="${HOOKS_TOKEN:-smoke-hooks-token}"
export CCD_PROBE_RETRY_INTERVAL="${CCD_PROBE_RETRY_INTERVAL:-1h}"

/usr/local/bin/cc-entrypoint >/tmp/cc-entrypoint.log 2>&1 &
ep=$!

healthz() {
  # bash's /dev/tcp: the image carries no curl, and needs none for this.
  exec 3<>/dev/tcp/127.0.0.1/18789 || return 1
  printf 'GET /healthz HTTP/1.0\r\nHost: localhost\r\n\r\n' >&3
  local body
  body="$(cat <&3)"
  exec 3<&-
  printf '%s' "${body#*$'\r\n\r\n'}"
}

body=""
for _ in $(seq 1 120); do
  kill -0 "$ep" 2>/dev/null || { cat /tmp/cc-entrypoint.log >&2; fail "the entrypoint exited"; }
  body="$(healthz 2>/dev/null || true)"
  [[ "$body" == *'"session_started":true'* ]] && break
  sleep 1
done
echo "cc-smoke: /healthz -> $body"
[[ "$body" == "{"* ]] || { cat /tmp/cc-entrypoint.log >&2; fail "/healthz did not answer with JSON"; }
[[ "$body" == *'"session_started":true'* ]] || {
  tmux capture-pane -p -t cc >&2 || true
  fail "the TUI never reached its prompt (no SessionStart hook within 120s); the pane is above"
}
echo "cc-smoke: session started ok"

# The auth verdict is REPORTED, not asserted (see the header).
for _ in $(seq 1 90); do
  body="$(healthz 2>/dev/null || true)"
  [[ "$body" == *'"auth":"unknown"'* ]] || break
  sleep 1
done
echo "cc-smoke: auth verdict (not asserted): $body"

ccd seed --check --config-dir "$CLAUDE_CONFIG_DIR" --workspace "$CCD_WORKSPACE" \
  --settings-template /etc/ccd/settings.json || fail "seeded config has the wrong shape"

kill "$ep" 2>/dev/null || true
tmux kill-server 2>/dev/null || true
echo "cc-smoke: PASS"

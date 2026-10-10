#!/usr/bin/env bash
# The claude-code-agent image's entrypoint: seed the CLI's config, start the
# interactive session in tmux, then run ccd in the foreground.
#
# 🔴 ccd IS THE FOREGROUND PROCESS, SO ITS EXIT IS THE CONTAINER'S EXIT. The TUI
# lives in a detached tmux session beside it, which ccd creates and SUPERVISES
# (CCD_SUPERVISE=1, cmd/ccd/supervise.go): a TUI that exits (an operator typing
# /exit, a crash) is restarted in the same pane with backoff; repeated exits in a
# short window are reported as crash_loop and fail /healthz, so Kubernetes
# restarts the pod.
#
# 🔴 EVERY START RESUMES THE SAME CONVERSATION WHEN THERE IS ONE: ccd runs
# `claude --continue` when CLAUDE_CONFIG_DIR already holds a transcript for the
# workspace, and plain `claude` only when it holds none. CLAUDE_CONFIG_DIR must
# therefore be on a persistent volume (as must the workspace) — wiring that volume
# is the provisioner's job, not this image's.
set -euo pipefail

# `docker run <image> cc-smoke` (or any command): run it instead.
if [[ $# -gt 0 ]]; then
  exec "$@"
fi

: "${CLAUDE_CONFIG_DIR:?CLAUDE_CONFIG_DIR must be set}"
: "${CCD_WORKSPACE:?CCD_WORKSPACE must be set}"
CCD_TMUX_TARGET="${CCD_TMUX_TARGET:-cc}"

# A subscription token is the ONLY credential this image is meant to carry. The
# entrypoint refuses to start when any of these is set (non-empty) in ITS
# environment:
#   ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN — outrank CLAUDE_CODE_OAUTH_TOKEN in
#     the CLI's precedence and would silently move the session onto API billing;
#   CLAUDE_CODE_USE_BEDROCK, CLAUDE_CODE_USE_VERTEX — move the session onto a
#     cloud provider's credentials and billing;
#   ANTHROPIC_BASE_URL — sends every request, the subscription token included, to
#     another endpoint. No deployment of this image documents a gateway, so there
#     is no reason here to allow it.
# The same variables set through settings.json's `env` are covered by `ccd seed`,
# which removes `env` and `apiKeyHelper` from <CLAUDE_CONFIG_DIR>/settings.json
# (cmd/ccd/seed.go settingsForbidden). NOT covered: a .claude/settings*.json in
# the workspace, and any other variable the CLI may read.
for v in ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN CLAUDE_CODE_USE_BEDROCK CLAUDE_CODE_USE_VERTEX ANTHROPIC_BASE_URL; do
  if [[ -n "${!v:-}" ]]; then
    echo "cc-entrypoint: $v is set; only CLAUDE_CODE_OAUTH_TOKEN may select the session's credential and endpoint. Refusing." >&2
    exit 1
  fi
done

ccd seed --config-dir "$CLAUDE_CONFIG_DIR" --workspace "$CCD_WORKSPACE" \
  --settings-template /etc/ccd/settings.json

cd "$CCD_WORKSPACE"
# ccd creates the tmux session itself, AFTER its hook listener is bound — the
# TUI's SessionStart hook is ccd's readiness signal for turns.
export CCD_TMUX_TARGET
export CCD_SUPERVISE=1
exec ccd serve

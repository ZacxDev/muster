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

# A subscription token is the ONLY credential this image is meant to carry. These
# outrank CLAUDE_CODE_OAUTH_TOKEN in the CLI's precedence and would silently move
# the session onto API billing.
for v in ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN; do
  if [[ -n "${!v:-}" ]]; then
    echo "cc-entrypoint: $v is set; it outranks CLAUDE_CODE_OAUTH_TOKEN and would switch billing. Refusing." >&2
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

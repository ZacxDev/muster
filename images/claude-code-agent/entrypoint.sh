#!/usr/bin/env bash
# The claude-code-agent image's entrypoint: seed the CLI's config, start the
# interactive session in tmux, then run ccd in the foreground.
#
# 🔴 ccd IS THE FOREGROUND PROCESS, SO ITS EXIT IS THE CONTAINER'S EXIT. The TUI
# lives in a detached tmux session beside it, which ccd creates and SUPERVISES
# (CCD_SUPERVISE=1, cmd/ccd/supervise.go): a TUI that exits (an operator typing
# /exit, a crash) is restarted in the same pane with backoff; repeated exits in a
# short window are reported as crash_loop and fail /healthz, so Kubernetes
# restarts the pod. So does a tmux server that went away or was replaced
# (terminal_lost): ccd never starts a second CLI beside one it can no longer see.
#
# 🔴 EVERY START RESUMES THE SAME CONVERSATION WHEN THERE IS ONE: ccd runs
# `claude --resume <id>` — the primary's recorded session id, else the newest
# interactive, non-lane transcript for the workspace (never a bare `--continue`,
# which could pick a lane's conversation) — and plain `claude` only when there is
# none. CLAUDE_CONFIG_DIR must
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

# The credential check (credentials.sh, installed beside this script's image
# at /usr/local/lib/cc/credentials.sh): a subscription token is the session's
# only credential, and ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN,
# CLAUDE_CODE_USE_BEDROCK, CLAUDE_CODE_USE_VERTEX and ANTHROPIC_BASE_URL are
# refused — except under the scoped CCD_LLM_ROUTE=muster (an officer's routine
# instance reaching its model through muster's LLM proxy), where it sets the
# base URL and token itself. It runs before anything is seeded or started.
# shellcheck source=images/claude-code-agent/credentials.sh
source /usr/local/lib/cc/credentials.sh
cc_check_credentials || exit 1

ccd seed --config-dir "$CLAUDE_CONFIG_DIR" --workspace "$CCD_WORKSPACE" \
  --settings-template /etc/ccd/settings.json

# muster's connector store (cmd/ccd/connect.go): when this pod is offered it,
# route git through muster's proxy for the `github` connector IF this agent
# already holds an approved grant (a pod restarted after an approval comes back
# wired). Best effort and bounded: it never blocks or fails the start, and it
# never files a request — asking is the session's decision. From then on
# `ccd serve` keeps the routing in step with the grants (approve, revoke).
if [[ -n "${MUSTER_CONNECT_URL:-}" ]]; then
  ccd connect-git --timeout 5s github || true
fi

cd "$CCD_WORKSPACE"
# ccd creates the tmux session itself, AFTER its hook listener is bound — the
# TUI's SessionStart hook is ccd's readiness signal for turns.
export CCD_TMUX_TARGET
export CCD_SUPERVISE=1
exec ccd serve

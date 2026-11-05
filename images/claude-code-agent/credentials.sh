# shellcheck shell=bash
# The claude-code-agent image's credential check, sourced by the entrypoint
# (installed at /usr/local/lib/cc/credentials.sh) and run before it seeds or
# starts anything: `cc_check_credentials || exit 1`.
#
# A subscription token (CLAUDE_CODE_OAUTH_TOKEN) is the credential this image
# is meant to carry, and these variables are refused when set (non-empty):
#   ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN — outrank CLAUDE_CODE_OAUTH_TOKEN in
#     the CLI's precedence and would silently move the session onto API billing;
#   CLAUDE_CODE_USE_BEDROCK, CLAUDE_CODE_USE_VERTEX — move the session onto a
#     cloud provider's credentials and billing;
#   ANTHROPIC_BASE_URL — sends every request, the credential included, to
#     another endpoint.
#
# 🔴 THE ONE EXCEPTION IS SCOPED: CCD_LLM_ROUTE=muster. A routine instance of
# an officer that declares `llm` (docs/officers.md "The LLM proxy") reaches its
# model through muster's LLM proxy, on the connect URL the pod already uses,
# authenticated by the pod's own muster token. muster attaches the provider
# key upstream; the pod holds none. Under the route, and only under it:
#   - ANTHROPIC_BASE_URL is set to EXACTLY $MUSTER_CONNECT_URL/llm, and
#     ANTHROPIC_AUTH_TOKEN to the pod's MUSTER_HOOK_TOKEN. Either one already
#     present is refused, even when it equals that value: nothing produces
#     one, so one present was set by something else;
#   - CLAUDE_CODE_OAUTH_TOKEN must be ABSENT: a routed pod that also held a
#     subscription would have two credentials and one would be unused;
#   - ANTHROPIC_API_KEY, Bedrock and Vertex stay refused;
#   - the muster host is appended to NO_PROXY / no_proxy, so a proxy in the
#     pod's environment never carries the pod's token (the CLI sends plain
#     http through HTTPS_PROXY).
# Any other CCD_LLM_ROUTE value is refused.
#
# The same variables set through settings.json's `env` are covered by removing
# `env` and `apiKeyHelper` from <CLAUDE_CONFIG_DIR>/settings.json: by `ccd
# seed`, and by ccd's supervisor again before every in-pod restart of the CLI
# (cmd/ccd/seed.go settingsForbidden). NOT covered: a .claude/settings*.json in
# the workspace, a key written while a CLI is running (until its next start),
# and any other variable the CLI may read.
#
# Refusals never print a value, only the variable's name.

cc_refuse() {
  echo "cc-entrypoint: $1 Refusing." >&2
  return 1
}

cc_check_credentials() {
  local route="${CCD_LLM_ROUTE:-}" v
  case "$route" in
    "" | muster) ;;
    *) cc_refuse "CCD_LLM_ROUTE is set to an unknown route; the only route is \"muster\"." || return 1 ;;
  esac
  for v in ANTHROPIC_API_KEY CLAUDE_CODE_USE_BEDROCK CLAUDE_CODE_USE_VERTEX; do
    if [[ -n "${!v:-}" ]]; then
      cc_refuse "$v is set; only CLAUDE_CODE_OAUTH_TOKEN (or CCD_LLM_ROUTE=muster) may select the session's credential and endpoint." || return 1
    fi
  done
  if [[ -z "$route" ]]; then
    for v in ANTHROPIC_AUTH_TOKEN ANTHROPIC_BASE_URL; do
      if [[ -n "${!v:-}" ]]; then
        cc_refuse "$v is set; only CLAUDE_CODE_OAUTH_TOKEN (or CCD_LLM_ROUTE=muster) may select the session's credential and endpoint." || return 1
      fi
    done
    return 0
  fi

  # CCD_LLM_ROUTE=muster
  if [[ -n "${CLAUDE_CODE_OAUTH_TOKEN:-}" ]]; then
    cc_refuse "CLAUDE_CODE_OAUTH_TOKEN is set with CCD_LLM_ROUTE=muster; a routed pod reaches its model only through muster's LLM proxy." || return 1
  fi
  if [[ -z "${MUSTER_CONNECT_URL:-}" ]]; then
    cc_refuse "CCD_LLM_ROUTE=muster but MUSTER_CONNECT_URL is not set; there is no proxy to route to." || return 1
  fi
  if [[ -z "${MUSTER_HOOK_TOKEN:-}" ]]; then
    cc_refuse "CCD_LLM_ROUTE=muster but MUSTER_HOOK_TOKEN is not set; the proxy authenticates the pod by it." || return 1
  fi
  for v in ANTHROPIC_BASE_URL ANTHROPIC_AUTH_TOKEN; do
    if [[ -n "${!v:-}" ]]; then
      cc_refuse "$v is set under CCD_LLM_ROUTE=muster; the route sets it itself, and nothing else may." || return 1
    fi
  done
  export ANTHROPIC_BASE_URL="${MUSTER_CONNECT_URL}/llm"
  export ANTHROPIC_AUTH_TOKEN="$MUSTER_HOOK_TOKEN"

  # The muster host: MUSTER_CONNECT_URL without its scheme, path, user info
  # and port.
  local host="${MUSTER_CONNECT_URL#*://}"
  host="${host%%/*}"
  host="${host##*@}"
  if [[ "$host" == \[* ]]; then
    host="${host%%]*}]"
  else
    host="${host%%:*}"
  fi
  export NO_PROXY="${NO_PROXY:+$NO_PROXY,}$host"
  export no_proxy="${no_proxy:+$no_proxy,}$host"
}

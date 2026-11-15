#!/usr/bin/env bash
# The Helm chart on a real cluster (kind), end to end. CI's `chart-kind` job
# runs it once per RBAC layout; do NOT run it on a shared workstation.
#
#   tests/chart/kind-e2e.sh <image repo> <image tag> <default|least-privilege>
#
# Needs: a current kubectl context on a throwaway cluster that already holds
# <repo>:<tag> (kind load docker-image), helm, kubectl, curl, jq.
#
# What it shows, in order — each step's evidence is read from the server's
# answer or the cluster's objects, never inferred from a command's exit:
#   1. the MINIMAL chart (every value at its default) installs; /readyz is 200;
#   2. the generated operator password signs in (a wrong one does not);
#   3. an UPGRADE that turns the agent plane on (kubernetes provisioner, the
#      claude-code kind, the given RBAC layout) keeps every generated secret
#      (the same password still signs in, the hook token is unchanged);
#   4. an agent created through the API (POST /api/agents with an agent-create
#      token minted on /account/tokens) gets its namespace, its pod and its
#      NetworkPolicy — and, in the least-privilege layout, the RoleBinding
#      muster writes for itself;
#   5. destroying it (DELETE /api/agents/<name>) removes the namespace;
#   6. muster's log holds no RBAC refusal from any of it.
#
# 🔴 THE AGENT IS NOT A REAL CLAUDE SESSION. A real claude-code agent needs a
# Claude subscription token, which CI does not have: the account's token is a
# fixture string, and MUSTER_AGENT_CC_IMAGE is the muster image as a stand-in,
# so the pod is created (that is what this checks: the objects the provisioner
# writes and the rights it needs to write them) but does not run ccd.
set -euo pipefail

REPO="${1:?image repo}"; TAG="${2:?image tag}"; LAYOUT="${3:?default|least-privilege}"
NS=muster
REL=ci
FULL="$REL-muster"
ROOT="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
CHART="$ROOT/charts/muster"
WORK="$(mktemp -d)"
PF_PID=""
fail() { echo "FAIL $*" >&2; dump; exit 1; }
dump() {
  echo "--- diagnostics" >&2
  kubectl -n "$NS" get all,secret,networkpolicy -o wide >&2 || true
  kubectl get ns >&2 || true
  kubectl -n "$NS" logs "deploy/$FULL" --tail=200 >&2 || true
}
cleanup() { [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true; rm -rf "$WORK"; }
trap cleanup EXIT

# port_forward: one forward to the server's main port, killed by its own PID.
port_forward() {
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true
  PORT=$((20000 + RANDOM % 20000))
  kubectl -n "$NS" port-forward "svc/$FULL" "$PORT:8105" >"$WORK/pf.log" 2>&1 &
  PF_PID=$!
  BASE="http://127.0.0.1:$PORT"
  for _ in $(seq 60); do
    curl -fsS -o /dev/null "$BASE/health" 2>/dev/null && return 0
    sleep 1
  done
  fail "port-forward to $FULL never answered /health ($(cat "$WORK/pf.log"))"
}
readyz() {
  local code
  for _ in $(seq 60); do
    code="$(curl -sS -o /dev/null -w '%{http_code}' "$BASE/readyz" || true)"
    [ "$code" = 200 ] && { echo "ok   /readyz: 200"; return 0; }
    sleep 2
  done
  fail "/readyz never answered 200 (last: $code): $(curl -sS "$BASE/readyz" || true)"
}
secret_val() { kubectl -n "$NS" get secret "$FULL-generated" -o "jsonpath={.data.$1}" | base64 -d; }
signin_cookie() {
  curl -sS -o /dev/null -D - -X POST --data-urlencode "password=$1" "$BASE/login" | tr -d '\r' |
    sed -n -E 's/^[Ss]et-[Cc]ookie: ([^;]*).*/\1/p' | head -1
}
rbac_refusals() { kubectl -n "$NS" logs "deploy/$FULL" --tail=-1 | grep -Ei 'forbidden|403 Forbidden' || true; }

common=(--namespace "$NS" --set image.repository="$REPO" --set image.tag="$TAG" --set image.pullPolicy=Never
        --set postgres.storage.size=1Gi --wait --timeout 6m)

# --- 1. minimal install ---------------------------------------------------------
kubectl create namespace "$NS"
helm install "$REL" "$CHART" "${common[@]}" -f "$CHART/ci/minimal.yaml"
port_forward
readyz

# --- 2. the generated password signs in --------------------------------------------
PASSWORD="$(secret_val MUSTER_UI_PASSWORD)"; HOOK="$(secret_val MUSTER_HOOK_TOKEN)"
[ "${#PASSWORD}" -ge 12 ] || fail "the generated UI password is ${#PASSWORD} characters"
cookie="$(signin_cookie "$PASSWORD")"; bad="$(signin_cookie "not-$PASSWORD")"
[ -n "$cookie" ] || fail "the generated password set no session cookie"
[ -z "$bad" ] || fail "a WRONG password set a cookie (the control): $bad"
echo "ok   sign-in: the generated password sets a session cookie, a wrong one does not"

# --- 3. upgrade: the agent plane on, secrets kept --------------------------------------
kubectl -n "$NS" create secret generic ci-claude-accounts --from-literal=MUSTER_AGENT_CC_TOKEN_CI=sk-ant-oat01-ci-fixture
helm upgrade "$REL" "$CHART" "${common[@]}" -f "$CHART/ci/minimal.yaml" \
  --set agents.enabled=true \
  --set agents.gatewayImage.repository="$REPO" --set agents.gatewayImage.tag="$TAG" \
  --set agents.gateway.enabled=true --set agents.gateway.model=ci-model \
  --set-string agents.gateway.runtimeConfig='{}' --set-string agents.gateway.runtimeInstall='true' \
  --set agents.claudeCode.enabled=true --set agents.claudeCode.image="$REPO:$TAG" \
  --set 'agents.claudeCode.accounts={ci}' --set agents.claudeCode.tokensSecret=ci-claude-accounts \
  --set agents.rbac.layout="$LAYOUT"
kubectl -n "$NS" rollout status "deploy/$FULL" --timeout=5m
port_forward
readyz
[ "$(secret_val MUSTER_UI_PASSWORD)" = "$PASSWORD" ] || fail "the upgrade REPLACED the generated UI password"
[ "$(secret_val MUSTER_HOOK_TOKEN)" = "$HOOK" ] || fail "the upgrade REPLACED the generated hook token"
cookie="$(signin_cookie "$PASSWORD")"
[ -n "$cookie" ] || fail "after the upgrade the first install's password no longer signs in"
echo "ok   upgrade: the generated secrets are kept and the first install's password still signs in"
boot="$(kubectl -n "$NS" logs "deploy/$FULL" --tail=-1)"
grep -q 'agent provisioning: UNWIRED' <<<"$boot" && fail "after the upgrade the boot banner still says agent provisioning: UNWIRED"

# --- 4. an agent through the API -----------------------------------------------------
page="$(curl -fsS -H "Cookie: $cookie" -H "Origin: $BASE" --data-urlencode name=ci-e2e --data-urlencode scope=agent-create \
  "$BASE/account/tokens")" || fail "POST /account/tokens"
TOKEN="$(grep -oE 'mst_[A-Za-z0-9_-]{16,}' <<<"$page" | head -1)"
[ -n "$TOKEN" ] || fail "the token page shows no new mst_ token"
resp="$(curl -sS -w '\n%{http_code}' -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  --data '{"kind":"claude-code","start":true,"prompt":"Reply with the word ok and nothing else."}' "$BASE/api/agents")"
code="$(tail -1 <<<"$resp")"; body="$(sed '$d' <<<"$resp")"
[ "$code" = 201 ] || fail "POST /api/agents answered $code: $body"
NAME="$(jq -r .agent.name <<<"$body")"; AGENT_NS="$(jq -r .agent.namespace <<<"$body")"
[ -n "$NAME" ] && [ "$NAME" != null ] || fail "POST /api/agents named no agent: $body"
echo "ok   POST /api/agents: 201, agent $NAME (namespace $AGENT_NS)"

want_objects() {
  for _ in $(seq 90); do
    if kubectl get ns "$AGENT_NS" >/dev/null 2>&1 &&
       [ -n "$(kubectl -n "$AGENT_NS" get pods -o name 2>/dev/null)" ] &&
       [ -n "$(kubectl -n "$AGENT_NS" get networkpolicy -o name 2>/dev/null)" ]; then
      return 0
    fi
    sleep 2
  done
  return 1
}
want_objects || fail "the agent's namespace, pod and NetworkPolicy did not all appear"
echo "ok   agent objects: namespace $AGENT_NS, pod $(kubectl -n "$AGENT_NS" get pods -o name | head -1), $(kubectl -n "$AGENT_NS" get networkpolicy -o name | head -1)"
# The policy admits muster's own pods: its selector is the chart's server labels.
pol="$(kubectl -n "$AGENT_NS" get networkpolicy -o json)"
jq -e --arg ns "$NS" '[.items[].spec.ingress[]?.from[]? | select(.namespaceSelector.matchLabels["kubernetes.io/metadata.name"] == $ns and .podSelector.matchLabels["app.kubernetes.io/instance"] == "'"$REL"'")] | length > 0' <<<"$pol" >/dev/null ||
  fail "the agent's NetworkPolicy does not admit the release's server pods: $pol"
echo "ok   NetworkPolicy: admits pods labelled app.kubernetes.io/instance=$REL in namespace $NS"
if [ "$LAYOUT" = least-privilege ]; then
  kubectl -n "$AGENT_NS" get rolebinding muster-provisioner -o json |
    jq -e '.roleRef.name | endswith("-agent-namespace")' >/dev/null ||
    fail "least-privilege: no muster-provisioner RoleBinding to the agent-namespace ClusterRole in $AGENT_NS"
  echo "ok   least-privilege: muster bound itself in $AGENT_NS (RoleBinding muster-provisioner)"
fi

# --- 5. destroy -------------------------------------------------------------------
code="$(curl -sS -o "$WORK/rm.json" -w '%{http_code}' -X DELETE -H "Authorization: Bearer $HOOK" "$BASE/api/agents/$NAME")"
case "$code" in 200|202|204) ;; *) fail "DELETE /api/agents/$NAME answered $code: $(cat "$WORK/rm.json")" ;; esac
gone=""
for _ in $(seq 120); do
  if ! kubectl get ns "$AGENT_NS" >/dev/null 2>&1; then gone=1; break; fi
  sleep 2
done
[ -n "$gone" ] || fail "the agent's namespace $AGENT_NS was not removed after DELETE"
echo "ok   destroy: DELETE /api/agents/$NAME answered $code and namespace $AGENT_NS is gone"

# --- 6. no RBAC refusal in the server's log -------------------------------------------
lines="$(kubectl -n "$NS" logs "deploy/$FULL" --tail=-1 | wc -l)"
[ "$lines" -gt 5 ] || fail "instrument check: the server log has only $lines line(s)"
refused="$(rbac_refusals)"
[ -z "$refused" ] || fail "muster's log holds RBAC refusals ($LAYOUT layout):
$refused"
echo "ok   no 'forbidden' in $lines lines of the server log ($LAYOUT layout)"
echo "PASS chart kind e2e ($LAYOUT)"

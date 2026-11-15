#!/usr/bin/env bash
# The Helm chart on a real cluster (kind), end to end. CI's `chart-kind` job
# runs it once per leg; do NOT run it on a shared workstation.
#
#   tests/chart/kind-e2e.sh <image repo> <image tag> <least-privilege|cluster-wide|services>
#
# Needs: a current kubectl context on a throwaway cluster that already holds
# <repo>:<tag> (kind load docker-image), helm, kubectl, curl, jq; the
# `services` leg also needs espeak-ng and the muster CLI on PATH.
#
# Each step's evidence is read from the server's answer or the cluster's
# objects, never inferred from a command's exit.
#
# THE AGENT-PLANE LEGS (least-privilege, cluster-wide), in order:
#   1. the MINIMAL chart (every value at its default) installs; /readyz is 200;
#   2. the generated operator password signs in (a wrong one does not);
#   3. an UPGRADE that turns the agent plane on (kubernetes provisioner, the
#      claude-code kind, the given RBAC layout, the admission policy at its
#      default: on) keeps every generated secret (the same password still
#      signs in, the hook token is unchanged);
#   4. the admission policy is ENFORCED for muster's ServiceAccount: creating
#      a namespace outside the agent prefix, and deleting one, are refused
#      with the policy's own message (and, least-privilege, a RoleBinding
#      outside it), while the same impersonated requests inside the prefix
#      succeed (the control: the refusal is the policy's, not RBAC's);
#   5. an agent created through the API (POST /api/agents with an agent-create
#      token minted on /account/tokens), under that policy, gets its
#      namespace, its pod and its NetworkPolicy — and, least-privilege, the
#      RoleBinding muster writes for itself;
#   6. destroying it (DELETE /api/agents/<name>) removes the namespace;
#   7. muster's log holds no RBAC refusal from any of it.
#
# THE SERVICES LEG: the bundled GoTrue, S3 store and speech-to-text service
# RUNNING, in gotrue mode with push and the connector store on:
#   1. a claim link (muster-server invite --claim-admin) binds a GoTrue login,
#      and a sign-in through the bundled GoTrue opens the board (a wrong
#      password is 401);
#   2. a file uploaded through POST /uploads lands in the bundled store
#      (read back with the store's own client) and comes back byte for byte
#      through its signed link (a tampered signature is 404);
#   3. the STT service refuses a wrong token and accepts muster's; the
#      dictation check's sample (cmd/muster/doctor_sample.wav) and a second
#      spoken clip come back through POST /transcribe as their own words;
#   4. `muster doctor`, run exactly as the install's notes print it, passes
#      its hook-token check (the control: pointed at the browser listener, as
#      the notes once did, it fails);
#   5. an upgrade turning push and the connector store OFF, then ON, keeps
#      the same VAPID pair and connector key, and the server serves that
#      public key (the control: deleting both VAPID keys, as the docs say to
#      rotate, DOES give a new pair, which the server boots with);
#   6. the POSTGRES_PASSWORD rotation docs/kubernetes.md gives: muster boots
#      on the new password and the old one is refused.
#
# 🔴 THE AGENT IS NOT A REAL CLAUDE SESSION. A real claude-code agent needs a
# Claude subscription token, which CI does not have: the account's token is a
# fixture string, and MUSTER_AGENT_CC_IMAGE is the muster image as a stand-in,
# so the pod is created (that is what this checks: the objects the provisioner
# writes and the rights it needs to write them) but does not run ccd.
set -euo pipefail

REPO="${1:?image repo}"; TAG="${2:?image tag}"; LEG="${3:?least-privilege|cluster-wide|services}"
case "$LEG" in least-privilege|cluster-wide|services) ;; *) echo "unknown leg '$LEG'" >&2; exit 2 ;; esac
NS=muster
REL=ci
FULL="$REL-muster"
SA="system:serviceaccount:$NS:$FULL"
PREFIX=muster-agent-
ROOT="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
CHART="$ROOT/charts/muster"
WORK="$(mktemp -d)"
PF_PID=""; BPF_PID=""; DPF_PID=""
fail() { echo "FAIL $*" >&2; dump; exit 1; }
dump() {
  echo "--- diagnostics" >&2
  kubectl -n "$NS" get all,secret,networkpolicy -o wide >&2 || true
  kubectl get ns >&2 || true
  kubectl -n "$NS" logs "deploy/$FULL" --tail=200 >&2 || true
  if [ "$LEG" = services ]; then
    for c in gotrue stt; do kubectl -n "$NS" logs "deploy/$FULL-$c" --all-containers --tail=60 >&2 || true; done
    kubectl -n "$NS" logs "$FULL-s3-0" --tail=60 >&2 || true
  fi
}
cleanup() {
  for p in "$PF_PID" "$BPF_PID" "$DPF_PID"; do [ -n "$p" ] && kill "$p" 2>/dev/null || true; done
  rm -rf "$WORK"
}
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
        --set postgres.storage.size=1Gi --wait)

# =============================================================================
# THE SERVICES LEG
# =============================================================================
services_leg() {
  local PUBLIC_PORT=18107
  PUBLIC="http://127.0.0.1:$PUBLIC_PORT"
  local svc=(--timeout 20m -f "$CHART/ci/minimal.yaml"
    --set publicUrl="$PUBLIC" --set secureCookies=false
    --set auth.mode=gotrue --set gotrue.enabled=true
    --set attachments.enabled=true
    --set dictation.enabled=true --set stt.model=Systran/faster-whisper-base.en)
  local on=(--set push.enabled=true --set push.subject=mailto:ci@example.com --set connectors.enabled=true)
  local off=(--set push.enabled=false --set connectors.enabled=false)

  kubectl create namespace "$NS"
  local start=$SECONDS
  helm install "$REL" "$CHART" "${common[@]}" "${svc[@]}" "${on[@]}" | tee "$WORK/install.out"
  echo "kind-e2e: [services] install --wait returned after $((SECONDS - start))s"
  port_forward
  readyz
  # The browser listener, on the port publicUrl names (links are written on it).
  kubectl -n "$NS" port-forward "svc/$FULL" "$PUBLIC_PORT:browser" >"$WORK/bpf.log" 2>&1 &
  BPF_PID=$!
  local up=""
  for _ in $(seq 60); do
    [ "$(curl -sS -o /dev/null -w '%{http_code}' "$PUBLIC/login" || true)" = 200 ] && { up=1; break; }
    sleep 1
  done
  [ -n "$up" ] || fail "[services] the browser listener never served GET /login ($(cat "$WORK/bpf.log"))"
  local boot; boot="$(kubectl -n "$NS" logs "deploy/$FULL" --tail=-1)"

  # --- 1. GoTrue: claim, then a sign-in through it ---------------------------
  local link token email=admin@example.com pw
  link="$(kubectl -n "$NS" exec "deploy/$FULL" -c muster -- /muster-server invite --claim-admin)" ||
    fail "[gotrue] muster-server invite --claim-admin"
  case "$link" in "$PUBLIC/auth/claim#"*) ;; *) fail "[gotrue] the claim link is not on $PUBLIC: $link" ;; esac
  token="${link#*#}"
  pw="ci-gotrue-$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  local code
  code="$(curl -sS -o "$WORK/claim.html" -w '%{http_code}' -H "Origin: $PUBLIC" --data-urlencode "token=$token" \
    --data-urlencode "email=$email" --data-urlencode "password=$pw" --data-urlencode "password2=$pw" "$PUBLIC/auth/claim")"
  [ "$code" = 303 ] || fail "[gotrue] POST /auth/claim answered $code: $(sed 's/<[^>]*>//g' "$WORK/claim.html" | tr -s ' \n' ' ' | head -c 400)"
  gotrue_signin() { # $1 = password; prints the status code, writes the cookie header to $WORK/cookie
    curl -sS -o /dev/null -D "$WORK/login.h" -w '%{http_code}' -H "Origin: $PUBLIC" \
      --data-urlencode "email=$email" --data-urlencode "password=$1" "$PUBLIC/login"
    tr -d '\r' <"$WORK/login.h" | sed -n -E 's/^[Ss]et-[Cc]ookie: ([^;]*).*/\1/p' | paste -sd ';' - >"$WORK/cookie"
  }
  code="$(gotrue_signin "$pw")"; COOKIE="$(cat "$WORK/cookie")"
  [ "$code" = 303 ] && [ -n "$COOKIE" ] || fail "[gotrue] signing in with the claimed login answered $code (cookie: '$COOKIE')"
  code="$(curl -sS -o /dev/null -w '%{http_code}' -H "Cookie: $COOKIE" "$PUBLIC/tasks")"
  [ "$code" = 200 ] || fail "[gotrue] the GoTrue session does not open the board ($code)"
  code="$(gotrue_signin "not-$pw")"
  # (the cookie file holds a lone newline when no cookie was set: compare its text)
  [ "$code" = 401 ] && [ -z "$(cat "$WORK/cookie")" ] || fail "[gotrue] a WRONG password answered $code with cookie '$(cat "$WORK/cookie")'"
  grep -q "login: user 1 signed in" <(kubectl -n "$NS" logs "deploy/$FULL" --tail=-1) ||
    fail "[gotrue] muster's log has no 'login: user 1 signed in'"
  echo "ok   [gotrue] a claimed login signs in through the bundled GoTrue and opens the board; a wrong password is 401"

  # --- 2. an attachment, into the bundled store ------------------------------
  grep -q "file attachments: ON — files go to MUSTER_S3_ENDPOINT=http://$FULL-s3:9000" <<<"$boot" ||
    fail "[s3] muster does not log attachments ON on the bundled store: $(grep 'file attachments:' <<<"$boot")"
  local store=(kubectl -n "$NS" exec "$FULL-s3-0" -c s3 --)
  local before; before="$( ("${store[@]}" mc ls --recursive muster/muster-uploads/uploads/ 2>/dev/null || true) | wc -l)"
  [ "$before" = 0 ] || fail "[s3] the bucket already holds $before upload(s)"
  head -c 3000 /dev/urandom | base64 >"$WORK/upload.txt"
  local reply
  reply="$(curl -fsS -H "Cookie: $COOKIE" -H "Origin: $PUBLIC" -F "file=@$WORK/upload.txt;filename=notes.txt" "$PUBLIC/uploads")" ||
    fail "[s3] POST /uploads failed"
  local objs; objs="$("${store[@]}" mc ls --recursive --json muster/muster-uploads/uploads/ | jq -r .key)"
  [ "$(grep -c . <<<"$objs")" = 1 ] || fail "[s3] after one upload the bundled store holds: '$objs'"
  "${store[@]}" mc cat "muster/muster-uploads/uploads/$objs" >"$WORK/in-store.txt"
  cmp -s "$WORK/upload.txt" "$WORK/in-store.txt" || fail "[s3] the object in the bundled store is not the uploaded file"
  local flink
  flink="$(python3 -c 'import json,re,sys; m=re.search(r"(https?://\S+/f/[^ \x27\]]+)", json.loads(sys.argv[1])["line"]); print(m.group(1) if m else "")' "$reply")"
  case "$flink" in "$PUBLIC"/f/*) ;; *) fail "[s3] the upload's line has no signed link on $PUBLIC: $reply" ;; esac
  curl -fsS -o "$WORK/download.txt" "$flink" || fail "[s3] the signed link did not answer: $flink"
  cmp -s "$WORK/upload.txt" "$WORK/download.txt" || fail "[s3] the file that came back through the link differs"
  local bad; bad="$(printf '%s' "$flink" | sed -E 's/sig=([0-9A-Za-z_-])/sig=X\1/')"
  [ "$bad" != "$flink" ] || fail "[s3] could not tamper with the link's signature: $flink"
  code="$(curl -sS -o /dev/null -w '%{http_code}' "$bad")"
  [ "$code" = 404 ] || fail "[s3] a tampered signature answered $code, want 404"
  echo "ok   [s3] an upload lands in the bundled store (uploads/$objs, byte for byte) and comes back through its link; a tampered one is 404"

  # --- 3. dictation, through the bundled STT ---------------------------------
  grep -q "dictation: ON — .*MUSTER_STT_URL=http://$FULL-stt:8000, MUSTER_STT_MODEL: Systran/faster-whisper-base.en" <<<"$boot" ||
    fail "[stt] muster does not log dictation ON on the bundled service: $(grep 'dictation:' <<<"$boot")"
  local stt_token; stt_token="$(secret_val MUSTER_STT_TOKEN)"
  local probe='import sys,urllib.request
try: print(urllib.request.urlopen(urllib.request.Request("http://127.0.0.1:8000/v1/models", headers={"Authorization": "Bearer " + sys.argv[1]}), timeout=30).status)
except urllib.error.HTTPError as e: print(e.code)'
  local wrong right
  wrong="$(kubectl -n "$NS" exec "deploy/$FULL-stt" -- python -c "$probe" "not-$stt_token")"
  right="$(kubectl -n "$NS" exec "deploy/$FULL-stt" -- python -c "$probe" "$stt_token")"
  case "$wrong" in 401|403) ;; *) fail "[stt] the STT service answered $wrong to a WRONG token" ;; esac
  [ "$right" = 200 ] || fail "[stt] the STT service answered $right to muster's token"
  echo "ok   [stt] the STT service refuses a wrong token ($wrong) and accepts muster's (200)"
  espeak-ng -s 140 -w "$WORK/apples.wav" "Seven green apples are sitting on the kitchen table."
  transcribe() {
    curl -sS -w '\n%{http_code}' --max-time 180 -H "Cookie: $COOKIE" -H "Origin: $PUBLIC" -H 'Content-Type: audio/wav' \
      --data-binary "@$1" "$PUBLIC/transcribe"
  }
  norm() { tr 'A-Z' 'a-z' | tr -c 'a-z\n' ' ' | tr -s ' '; }
  local t0=$SECONDS sample apples sample_t apples_t
  sample="$(transcribe "$ROOT/cmd/muster/doctor_sample.wav")"; apples="$(transcribe "$WORK/apples.wav")"
  [ "$(tail -n 1 <<<"$sample")" = 200 ] || fail "[stt] POST /transcribe (the dictation check's sample) answered: $sample"
  [ "$(tail -n 1 <<<"$apples")" = 200 ] || fail "[stt] POST /transcribe answered: $apples"
  sample_t="$(sed '$d' <<<"$sample" | norm)"; apples_t="$(sed '$d' <<<"$apples" | norm)"
  echo "kind-e2e: [stt] heard \"$sample_t\" and \"$apples_t\" in $((SECONDS - t0))s"
  grep -q 'testing' <<<"$sample_t" && grep -q 'dictation' <<<"$sample_t" ||
    fail "[stt] the dictation check's sample (\"testing muster dictation\") was heard as \"$sample_t\""
  grep -q 'apples' <<<"$apples_t" && grep -q 'kitchen' <<<"$apples_t" ||
    fail "[stt] the apples clip was heard as \"$apples_t\""
  if grep -q 'apples' <<<"$sample_t" || grep -q 'dictation' <<<"$apples_t"; then
    fail "[stt] the two clips' transcripts share words: the answer does not follow the audio"
  fi
  echo "ok   [stt] the dictation check's sample and a second clip come back through POST /transcribe as their own words"

  # --- 4. muster doctor, as the notes print it -------------------------------
  helm get notes "$REL" -n "$NS" >"$WORK/notes.txt"
  local pf_line doctor_cmd
  pf_line="$(grep -E "^ +kubectl -n $NS port-forward svc/$FULL [0-9]+:http &$" "$WORK/notes.txt" | sed 's/^ *//')" ||
    fail "[doctor] the notes print no port-forward to the main port: $(cat "$WORK/notes.txt")"
  doctor_cmd="$(sed -n '/^ *MUSTER_TOKEN=/,/muster doctor/p' "$WORK/notes.txt" | sed -e 's/^ *//' -e 's/\\$//' | tr '\n' ' ')"
  grep -q 'muster doctor --api-url http://localhost:[0-9]*' <<<"$doctor_cmd" ||
    fail "[doctor] the notes' doctor command does not use the port-forward: $doctor_cmd"
  eval "$pf_line" >"$WORK/dpf.log" 2>&1
  DPF_PID=$!
  local dport; dport="$(grep -oE '[0-9]+:http' <<<"$pf_line" | cut -d: -f1)"
  for _ in $(seq 30); do curl -fsS -o /dev/null "http://localhost:$dport/health" 2>/dev/null && break; sleep 1; done
  eval "$doctor_cmd" >"$WORK/doctor.json" 2>"$WORK/doctor.err" || true
  local hook; hook="$(jq -r '.checks[] | select(.name == "hook-token") | .status' "$WORK/doctor.json")"
  [ "$hook" = PASS ] || fail "[doctor] as the notes print it, doctor's hook-token check is '$hook': $(cat "$WORK/doctor.err")"
  # The control: the old notes' URL, the browser listener, refuses the token.
  MUSTER_TOKEN="$(secret_val MUSTER_HOOK_TOKEN)" muster doctor --api-url "$PUBLIC" >"$WORK/doctor-bad.json" 2>/dev/null || true
  hook="$(jq -r '.checks[] | select(.name == "hook-token") | .status' "$WORK/doctor-bad.json")"
  [ "$hook" = FAIL ] || fail "[doctor] CONTROL: against the browser listener the hook-token check is '$hook', want FAIL"
  echo "ok   [doctor] as the notes print it, doctor's hook-token check passes (against the browser listener it fails)"

  # --- 5. turned off and on again, the keys are kept -------------------------
  push_key() { curl -fsS -H "Cookie: $COOKIE" "$PUBLIC/api/push/vapid-public-key" | jq -r .key; }
  local pub priv conn
  pub="$(secret_val MUSTER_VAPID_PUBLIC_KEY)"; priv="$(secret_val MUSTER_VAPID_PRIVATE_KEY)"; conn="$(secret_val MUSTER_CONNECTOR_ENCRYPTION_KEY)"
  [ -n "$pub" ] && [ -n "$priv" ] && [ -n "$conn" ] || fail "[keys] the install generated no VAPID pair or connector key"
  [ "$(push_key)" = "$pub" ] || fail "[keys] the server does not serve the generated VAPID public key"
  upgrade_to() { helm upgrade "$REL" "$CHART" "${common[@]}" "${svc[@]}" "$@" >/dev/null; kubectl -n "$NS" rollout status "deploy/$FULL" --timeout=5m >/dev/null; }
  upgrade_to "${off[@]}"
  [ "$(secret_val MUSTER_VAPID_PUBLIC_KEY)" = "$pub" ] && [ "$(secret_val MUSTER_VAPID_PRIVATE_KEY)" = "$priv" ] &&
    [ "$(secret_val MUSTER_CONNECTOR_ENCRYPTION_KEY)" = "$conn" ] || fail "[keys] turning push and connectors OFF dropped or changed a key"
  upgrade_to "${on[@]}"
  port_forward; readyz
  [ "$(secret_val MUSTER_VAPID_PUBLIC_KEY)" = "$pub" ] && [ "$(secret_val MUSTER_VAPID_PRIVATE_KEY)" = "$priv" ] ||
    fail "[keys] turning push off and on again minted a NEW VAPID pair"
  [ "$(secret_val MUSTER_CONNECTOR_ENCRYPTION_KEY)" = "$conn" ] || fail "[keys] turning connectors off and on again minted a NEW connector key"
  wait_browser
  [ "$(push_key)" = "$pub" ] || fail "[keys] after off/on the server serves another VAPID public key"
  echo "ok   [keys] push and connectors OFF then ON: the same VAPID pair and connector key, and the same served public key"
  # The control, and the docs' rotation: delete BOTH VAPID keys -> a new pair.
  kubectl -n "$NS" patch secret "$FULL-generated" --type json \
    -p '[{"op":"remove","path":"/data/MUSTER_VAPID_PUBLIC_KEY"},{"op":"remove","path":"/data/MUSTER_VAPID_PRIVATE_KEY"}]' >/dev/null
  upgrade_to "${on[@]}"
  kubectl -n "$NS" rollout restart "deploy/$FULL" >/dev/null; kubectl -n "$NS" rollout status "deploy/$FULL" --timeout=5m >/dev/null
  port_forward; readyz
  local npub; npub="$(secret_val MUSTER_VAPID_PUBLIC_KEY)"
  [ -n "$npub" ] && [ "$npub" != "$pub" ] || fail "[keys] CONTROL: deleting both VAPID keys did not give a new pair ('$npub')"
  wait_browser
  [ "$(push_key)" = "$npub" ] || fail "[keys] CONTROL: the server does not serve the rotated public key"
  echo "ok   [keys] CONTROL: deleting both VAPID keys and upgrading gives a new pair, which the server boots with and serves"

  # --- 6. the POSTGRES_PASSWORD rotation the docs give -----------------------
  local old new; old="$(secret_val POSTGRES_PASSWORD)"; new="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  kubectl -n "$NS" exec "$FULL-postgres-0" -c postgres -- \
    psql -U muster -d muster -v ON_ERROR_STOP=1 -c "ALTER ROLE muster PASSWORD '$new'" >/dev/null
  kubectl -n "$NS" patch secret "$FULL-generated" --type merge -p "{\"stringData\":{\"POSTGRES_PASSWORD\":\"$new\"}}" >/dev/null
  upgrade_to "${on[@]}"
  kubectl -n "$NS" rollout restart "deploy/$FULL" >/dev/null; kubectl -n "$NS" rollout status "deploy/$FULL" --timeout=5m >/dev/null
  grep -q ":$new@" <<<"$(secret_val DATABASE_URL)" || fail "[postgres] the upgrade did not rebuild DATABASE_URL from the new password"
  port_forward; readyz
  wait_browser
  code="$(gotrue_signin "$pw")"
  [ "$code" = 303 ] || fail "[postgres] after the rotation a sign-in answered $code"
  [ "$(pg_try "$new")" = Succeeded ] || fail "[postgres] the NEW password does not connect: $(cat "$WORK/pg.log")"
  [ "$(pg_try "$old")" = Failed ] && grep -q 'password authentication failed' "$WORK/pg.log" ||
    fail "[postgres] the OLD password still connects, or failed for another reason: $(cat "$WORK/pg.log")"
  echo "ok   [postgres] the documented rotation: muster runs on the new password (a sign-in works), the old one is refused"

  echo "PASS chart kind e2e (services)"
}

# wait_browser: the browser forward, after a rollout, answers again.
wait_browser() {
  [ -n "$BPF_PID" ] && kill "$BPF_PID" 2>/dev/null || true
  kubectl -n "$NS" port-forward "svc/$FULL" "18107:browser" >"$WORK/bpf.log" 2>&1 &
  BPF_PID=$!
  for _ in $(seq 60); do
    [ "$(curl -sS -o /dev/null -w '%{http_code}' "$PUBLIC/login" || true)" = 200 ] && return 0
    sleep 1
  done
  fail "the browser listener never answered after the rollout ($(cat "$WORK/bpf.log"))"
}

# pg_try <password>: a one-off psql pod against the bundled Postgres over the
# network (password-checked, unlike the pod's own socket); prints its phase.
pg_try() {
  local p="pgcheck-$RANDOM" ph=""
  kubectl -n "$NS" run "$p" --restart=Never --image=postgres:16-alpine --image-pull-policy=IfNotPresent \
    --env "PGPASSWORD=$1" --env PGCONNECT_TIMEOUT=10 --command -- psql -h "$FULL-postgres" -U muster -d muster -tAc 'select 1' >/dev/null
  for _ in $(seq 90); do
    ph="$(kubectl -n "$NS" get pod "$p" -o jsonpath='{.status.phase}')"
    case "$ph" in Succeeded|Failed) break ;; esac
    sleep 2
  done
  kubectl -n "$NS" logs "$p" >"$WORK/pg.log" 2>&1 || true
  kubectl -n "$NS" delete pod "$p" --wait=false >/dev/null 2>&1 || true
  echo "$ph"
}

if [ "$LEG" = services ]; then
  services_leg
  exit 0
fi

# =============================================================================
# THE AGENT-PLANE LEGS
# =============================================================================
LAYOUT="$LEG"
common+=(--timeout 6m)

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
  --set-string agents.gateway.runtimeConfig=ci-runtime-config --set-string agents.gateway.runtimeInstall=true \
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

# --- 4. the admission policy, enforced ------------------------------------------------
POLICY="$NS-$FULL-agent-namespaces"
kubectl get validatingadmissionpolicy "$POLICY" >/dev/null || fail "no ValidatingAdmissionPolicy $POLICY (it is on by default)"
kubectl create namespace ci-bystander >/dev/null
# as_sa <args>: kubectl as muster's ServiceAccount; stdout+stderr to $WORK/as.out.
as_sa() { kubectl --as="$SA" "$@" >"$WORK/as.out" 2>&1; }
ns_msg="muster may only create or delete namespaces whose name starts with $PREFIX"
# A new policy takes a moment to reach the API server's admission chain: the
# refusal is retried (a fresh name each time), and a namespace an early,
# unenforced create made is removed as the admin.
refused_create=""
for i in $(seq 30); do
  if as_sa create namespace "ci-not-an-agent-$i"; then
    kubectl delete namespace "ci-not-an-agent-$i" --wait=false >/dev/null 2>&1 || true
    sleep 2; continue
  fi
  grep -q "$ns_msg" "$WORK/as.out" && { refused_create=1; break; }
  fail "creating a non-prefix namespace as muster failed, but not by the policy: $(cat "$WORK/as.out")"
done
[ -n "$refused_create" ] || fail "the admission policy never refused muster a namespace outside $PREFIX"
as_sa delete namespace ci-bystander && fail "muster DELETED the namespace ci-bystander (outside $PREFIX)"
grep -q "$ns_msg" "$WORK/as.out" || fail "deleting ci-bystander as muster failed, but not by the policy: $(cat "$WORK/as.out")"
kubectl get namespace ci-bystander >/dev/null || fail "ci-bystander is gone"
# The control: the SAME impersonated requests inside the prefix succeed, so the
# refusals above are the policy's, not RBAC's or impersonation's.
as_sa create namespace "${PREFIX}ci-probe" || fail "CONTROL: muster could not create ${PREFIX}ci-probe: $(cat "$WORK/as.out")"
as_sa delete namespace "${PREFIX}ci-probe" --wait=false || fail "CONTROL: muster could not delete ${PREFIX}ci-probe: $(cat "$WORK/as.out")"
echo "ok   admission policy: muster is refused creating or deleting a namespace outside $PREFIX, and allowed inside it"
if [ "$LAYOUT" = least-privilege ]; then
  role="$(kubectl -n "$NS" get deploy "$FULL" -o json | jq -r '.spec.template.spec.containers[0].env[] | select(.name == "MUSTER_AGENT_NAMESPACE_CLUSTERROLE") | .value')"
  as_sa -n ci-bystander create rolebinding ci-escalate --clusterrole="$role" --serviceaccount="$NS:$FULL" &&
    fail "muster bound itself into ci-bystander (outside $PREFIX)"
  grep -q "muster may only write or delete RoleBindings in namespaces whose name starts with $PREFIX" "$WORK/as.out" ||
    fail "the RoleBinding in ci-bystander was refused, but not by the policy: $(cat "$WORK/as.out")"
  echo "ok   admission policy: muster is refused a RoleBinding outside $PREFIX"
fi

# --- 5. an agent through the API, under the policy -----------------------------------
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
  echo "ok   least-privilege: muster bound itself in $AGENT_NS (RoleBinding muster-provisioner), under the admission policy"
fi

# --- 6. destroy -------------------------------------------------------------------
code="$(curl -sS -o "$WORK/rm.json" -w '%{http_code}' -X DELETE -H "Authorization: Bearer $HOOK" "$BASE/api/agents/$NAME")"
case "$code" in 200|202|204) ;; *) fail "DELETE /api/agents/$NAME answered $code: $(cat "$WORK/rm.json")" ;; esac
gone=""
for _ in $(seq 120); do
  if ! kubectl get ns "$AGENT_NS" >/dev/null 2>&1; then gone=1; break; fi
  sleep 2
done
[ -n "$gone" ] || fail "the agent's namespace $AGENT_NS was not removed after DELETE"
echo "ok   destroy: DELETE /api/agents/$NAME answered $code and namespace $AGENT_NS is gone (under the admission policy)"

# --- 7. no RBAC refusal in the server's log -------------------------------------------
lines="$(kubectl -n "$NS" logs "deploy/$FULL" --tail=-1 | wc -l)"
[ "$lines" -gt 5 ] || fail "instrument check: the server log has only $lines line(s)"
refused="$(rbac_refusals)"
[ -z "$refused" ] || fail "muster's log holds RBAC refusals ($LAYOUT layout):
$refused"
echo "ok   no 'forbidden' in $lines lines of the server log ($LAYOUT layout)"
echo "PASS chart kind e2e ($LAYOUT)"

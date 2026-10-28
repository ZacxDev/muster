#!/usr/bin/env bash
# deploy/docker-compose.yml is valid, and has the shape the install guide needs.
#
#   tests/compose-check.sh            (make compose-check)
#
# Static: runs `docker compose config` (no image is pulled, nothing starts) for
# the default stack and for each dictation profile, against deploy/.env.example
# with its CHANGE-ME values filled in. Asserts:
#   * the service set per profile is exactly what docs/dictation.md describes;
#   * EVERY long-running service has a healthcheck (`docker compose up --wait`
#     needs them); the only exemption is a ONE-SHOT (restart "no") that a
#     healthy-checked service waits on with service_completed_successfully;
#   * muster is published on 127.0.0.1:8105 only, with the contract's settings;
#   * the stt-whisper image is pinned by digest;
#   * NEGATIVE CONTROL: the repository's .env.example as shipped (MUSTER_VERSION
#     empty) is REFUSED — the compose file must not start an unversioned image.
#
# Exit 0 ok, 1 an assertion failed, 2 harness error (no docker compose).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE="$ROOT/deploy/docker-compose.yml"
EXAMPLE="$ROOT/deploy/.env.example"
docker compose version >/dev/null 2>&1 || { echo "compose-check: docker compose is not available" >&2; exit 2; }

tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT

# The example, filled in the way a user would.
sed -e 's/^MUSTER_VERSION=$/MUSTER_VERSION=1.2.3/' -e 's/=CHANGE-ME$/=fixture-secret/' "$EXAMPLE" > "$tmp/env"
printf 'MUSTER_STT_TOKEN=fixture-stt\nHF_TOKEN=fixture-hf\n' >> "$tmp/env"

fail=0
for profile in default stt-cohere stt-whisper; do
  args=(-f "$COMPOSE" --env-file "$tmp/env" -p compose-check)
  [ "$profile" = default ] || args+=(--profile "$profile")
  docker compose "${args[@]}" config --format json > "$tmp/$profile.json" || {
    echo "FAIL [$profile]: docker compose config refused the file"; fail=1; continue; }
  python3 - "$profile" "$tmp/$profile.json" <<'PY' || fail=1
import json, sys
profile, path = sys.argv[1], sys.argv[2]
cfg = json.load(open(path))
svcs = cfg["services"]
want = {"default": {"postgres", "muster"},
        "stt-cohere": {"postgres", "muster", "stt-cohere"},
        "stt-whisper": {"postgres", "muster", "stt-whisper", "stt-whisper-init"}}[profile]
errs = []
if set(svcs) != want:
    errs.append(f"services {sorted(svcs)} != {sorted(want)}")
awaited = {dep for s in svcs.values() for dep, c in (s.get("depends_on") or {}).items()
           if c.get("condition") == "service_completed_successfully"}
for name, s in svcs.items():
    hc = s.get("healthcheck") or {}
    if hc.get("disable") or not hc.get("test"):
        if name in awaited and s.get("restart") == "no":
            continue  # a one-shot; its dependent's healthcheck covers it
        errs.append(f"{name} has no healthcheck")
m = svcs.get("muster", {})
ports = m.get("ports", [])
if not (len(ports) == 1 and ports[0].get("host_ip") == "127.0.0.1"
        and str(ports[0].get("published")) == "8105" and ports[0].get("target") == 8105):
    errs.append(f"muster ports {ports} are not exactly 127.0.0.1:8105->8105")
env = m.get("environment", {})
for k, v in {"MUSTER_MIRROR": "1", "MUSTER_STANDALONE": "1"}.items():
    if env.get(k) != v:
        errs.append(f"muster {k}={env.get(k)!r}, want {v!r}")
for k in ("MUSTER_UI_PASSWORD", "MUSTER_HOOK_TOKEN", "DATABASE_URL"):
    if not env.get(k):
        errs.append(f"muster {k} is empty")
if m.get("image") != "ghcr.io/zacxdev/muster:1.2.3":
    errs.append(f"muster image {m.get('image')!r}, want ghcr.io/zacxdev/muster:1.2.3")
if m.get("depends_on", {}).get("postgres", {}).get("condition") != "service_healthy":
    errs.append("muster does not wait for a healthy postgres")
if "stt-whisper" in svcs and "@sha256:" not in svcs["stt-whisper"]["image"]:
    errs.append("stt-whisper image is not pinned by digest")
if "stt-cohere" in svcs:
    if svcs["stt-cohere"]["image"] != "ghcr.io/zacxdev/muster-stt-cohere:1.2.3":
        errs.append(f"stt-cohere image {svcs['stt-cohere']['image']!r}")
    if svcs["stt-cohere"]["environment"].get("STT_API_TOKEN") != "fixture-stt":
        errs.append("stt-cohere does not share MUSTER_STT_TOKEN as STT_API_TOKEN")
if "stt-whisper" in svcs and svcs["stt-whisper"]["environment"].get("API_KEY") != "fixture-stt":
    errs.append("stt-whisper does not share MUSTER_STT_TOKEN as API_KEY")
for e in errs:
    print(f"FAIL [{profile}]: {e}")
if not errs:
    print(f"ok   [{profile}]: services {sorted(svcs)}, every long-running one with a healthcheck")
sys.exit(1 if errs else 0)
PY
done

# Negative control: the shipped example has MUSTER_VERSION empty and must be refused.
if docker compose -f "$COMPOSE" --env-file "$EXAMPLE" -p compose-check config >/dev/null 2>"$tmp/neg.err"; then
  echo "FAIL [negative control]: the unfilled .env.example was ACCEPTED (an unversioned image would start)"; fail=1
elif grep -q MUSTER_VERSION "$tmp/neg.err"; then
  echo "ok   [negative control]: the unfilled .env.example is refused, naming MUSTER_VERSION"
else
  echo "FAIL [negative control]: refused, but not for MUSTER_VERSION: $(cat "$tmp/neg.err")"; fail=1
fi

exit "$fail"

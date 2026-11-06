#!/usr/bin/env bash
# deploy/docker-compose.yml is valid, and has the shape the install guide needs.
#
#   tests/compose-check.sh            (make compose-check)
#
# Static: runs `docker compose config` (no image is pulled, nothing starts) for
# the default stack and for each profile (stt, its older name stt-whisper,
# stt-cohere, s3, and stt+s3 together), against deploy/.env.example with its
# CHANGE-ME values filled in. Asserts:
#   * the service set per profile is exactly what docs/features.md describes;
#   * EVERY long-running service has a healthcheck (`docker compose up --wait`
#     needs them); the only exemption is a ONE-SHOT (restart "no") that a
#     healthy-checked service waits on with service_completed_successfully;
#   * muster is published on 127.0.0.1:8105 only, with the contract's settings;
#   * the stt-whisper and s3 images are pinned by digest;
#   * postgres's healthcheck probes TCP (`-h 127.0.0.1`), the way muster
#     connects — the socket answers "ready" during initdb's socket-only server;
#   * the MUSTER_VAPID_* push keys reach muster's environment;
#   * muster and stt-whisper resolve the SAME model, with MUSTER_STT_MODEL
#     unset (both take the shared default) and set (both take it): a model
#     speaches installs that muster never asks for fails every recording;
#   * stt-whisper's healthcheck (executed here) still refuses an empty model,
#     naming the variable;
#   * s3: muster's endpoint, bucket and key pair are the store's own (the
#     store's root credentials ARE muster's key pair), and the store is not
#     published on the host;
#   * EMPTY .env: with only the required lines, `config` prints NO warning,
#     and every optional variable muster is given resolves to EMPTY — the
#     conditional defaults included — except the three with fixed defaults
#     (standalone, secure cookies, and session state, ON by default);
#   * MUSTER_SESSION_STATE set EMPTY or 0 in .env reaches muster as that value
#     (both off), not as the default 1;
#   * YOUR OWN S3 store (endpoint + secret, no S3_STORE_SECRET_KEY) is never
#     handed the bundled store's bucket or access key;
#   * your own endpoint WITH S3_STORE_SECRET_KEY left set resolves to a bucket
#     muster refuses, so the bundled credentials are never sent there;
#   * the bundled store's start script refuses the CHANGE-ME placeholder;
#   * NEGATIVE CONTROL: the repository's .env.example as shipped (MUSTER_VERSION
#     empty) is REFUSED — the compose file must not start an unversioned image.
#
# Exit 0 ok, 1 an assertion failed, 2 harness error (no docker compose).
set -euo pipefail

# CDPATH unset: with it exported, `cd` to a relative path PRINTS the directory,
# and ROOT became two lines (measured: every `make` target here broke).
ROOT="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE="$ROOT/deploy/docker-compose.yml"
EXAMPLE="$ROOT/deploy/.env.example"
docker compose version >/dev/null 2>&1 || { echo "compose-check: docker compose is not available" >&2; exit 2; }

tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT

# The example, filled in the way a user would.
sed -e 's/^MUSTER_VERSION=$/MUSTER_VERSION=1.2.3/' -e 's/=CHANGE-ME$/=fixture-secret/' "$EXAMPLE" > "$tmp/env"
printf 'MUSTER_STT_TOKEN=fixture-stt\nHF_TOKEN=fixture-hf\n' >> "$tmp/env"
printf 'MUSTER_VAPID_PUBLIC_KEY=fixture-vapid-pub\nMUSTER_VAPID_PRIVATE_KEY=fixture-vapid-priv\nMUSTER_VAPID_SUBJECT=mailto:ops@example.com\n' >> "$tmp/env"
printf 'S3_STORE_SECRET_KEY=fixture-s3-secret\nMUSTER_UPLOAD_SIGNING_KEY=fixture-signing-key-fixture-signing-key\n' >> "$tmp/env"
# The same, with a model chosen: both sides of the STT pair must follow it.
{ cat "$tmp/env"; echo "MUSTER_STT_MODEL=Systran/faster-whisper-medium"; } > "$tmp/env-model"
# The chief: its profile's two lines (the connector key is in the example).
{ cat "$tmp/env"; echo "MUSTER_CHIEF_RUNTIME=compose"; } > "$tmp/env-chief"

fail=0
for profile in default stt stt-whisper stt-cohere s3 stt,s3 stt+model chief; do
  envfile="$tmp/env"; [ "$profile" = stt+model ] && envfile="$tmp/env-model"; [ "$profile" = chief ] && envfile="$tmp/env-chief"
  args=(-f "$COMPOSE" --env-file "$envfile" -p compose-check)
  case "$profile" in
    default) ;;
    stt,s3) args+=(--profile stt --profile s3) ;;
    stt+model) args+=(--profile stt) ;;
    *) args+=(--profile "$profile") ;;
  esac
  docker compose "${args[@]}" config --format json > "$tmp/$profile.json" || {
    echo "FAIL [$profile]: docker compose config refused the file"; fail=1; continue; }
  python3 - "$profile" "$tmp/$profile.json" <<'PY' || fail=1
import json, sys
profile, path = sys.argv[1], sys.argv[2]
cfg = json.load(open(path))
svcs = cfg["services"]
whisper = {"stt-whisper", "stt-whisper-init"}
want = {"default": {"postgres", "muster"},
        "stt": {"postgres", "muster"} | whisper,
        "stt+model": {"postgres", "muster"} | whisper,
        "stt-whisper": {"postgres", "muster"} | whisper,
        "stt-cohere": {"postgres", "muster", "stt-cohere"},
        "s3": {"postgres", "muster", "s3"},
        "stt,s3": {"postgres", "muster", "s3"} | whisper,
        "chief": {"postgres", "muster", "chief", "chief-init"}}[profile]
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
for pinned in ("stt-whisper", "s3"):
    if pinned in svcs and "@sha256:" not in svcs[pinned]["image"]:
        errs.append(f"{pinned} image is not pinned by digest")
if "stt-cohere" in svcs:
    if svcs["stt-cohere"]["image"] != "ghcr.io/zacxdev/muster-stt-cohere:1.2.3":
        errs.append(f"stt-cohere image {svcs['stt-cohere']['image']!r}")
    if svcs["stt-cohere"]["environment"].get("STT_API_TOKEN") != "fixture-stt":
        errs.append("stt-cohere does not share MUSTER_STT_TOKEN as STT_API_TOKEN")
if "stt-whisper" in svcs and svcs["stt-whisper"]["environment"].get("API_KEY") != "fixture-stt":
    errs.append("stt-whisper does not share MUSTER_STT_TOKEN as API_KEY")
pg_test = " ".join(svcs.get("postgres", {}).get("healthcheck", {}).get("test", []))
if "-h 127.0.0.1" not in pg_test:
    errs.append(f"postgres healthcheck {pg_test!r} does not probe TCP (-h 127.0.0.1)")
for k, v in {"MUSTER_VAPID_PUBLIC_KEY": "fixture-vapid-pub", "MUSTER_VAPID_PRIVATE_KEY": "fixture-vapid-priv",
             "MUSTER_VAPID_SUBJECT": "mailto:ops@example.com"}.items():
    if env.get(k) != v:
        errs.append(f"muster {k}={env.get(k)!r}, want the .env value {v!r}")
if "stt-whisper" in svcs:
    import subprocess, os as _os
    w = svcs["stt-whisper"]
    want_model = "Systran/faster-whisper-medium" if profile == "stt+model" else "Systran/faster-whisper-small"
    got = (env.get("MUSTER_STT_MODEL"), w["environment"].get("STT_WHISPER_MODEL"))
    if got != (want_model, want_model):
        errs.append(f"muster asks for model {got[0]!r} but stt-whisper installs {got[1]!r}; both must be {want_model!r}")
    if env.get("MUSTER_STT_URL") != "http://stt-whisper:8000":
        errs.append(f"with only MUSTER_STT_TOKEN set, muster's MUSTER_STT_URL is {env.get('MUSTER_STT_URL')!r}, not the stt service")
    hc = w["healthcheck"]["test"]
    code = hc[hc.index("-c") + 1]
    r = subprocess.run(["python3", "-c", code], capture_output=True, text=True,
                       env={**_os.environ, "API_KEY": "fixture-stt", "STT_WHISPER_MODEL": ""})
    if r.returncode == 0 or "MUSTER_STT_MODEL" not in r.stderr:
        errs.append(f"stt-whisper healthcheck with no model: rc={r.returncode} stderr={r.stderr.strip()!r}; want a refusal naming MUSTER_STT_MODEL")
if "s3" in svcs:
    s3 = svcs["s3"]
    se = s3["environment"]
    pairs = {"MUSTER_S3_ENDPOINT": ("http://s3:9000", env.get("MUSTER_S3_ENDPOINT")),
             "MUSTER_S3_BUCKET": (se.get("MUSTER_S3_BUCKET"), env.get("MUSTER_S3_BUCKET")),
             "MUSTER_S3_ACCESS_KEY": (se.get("MINIO_ROOT_USER"), env.get("MUSTER_S3_ACCESS_KEY")),
             "MUSTER_S3_SECRET_KEY": (se.get("MINIO_ROOT_PASSWORD"), env.get("MUSTER_S3_SECRET_KEY"))}
    for k, (store, mine) in pairs.items():
        if not store or store != mine:
            errs.append(f"s3 store has {k}={store!r} but muster is given {mine!r}")
    if se.get("MINIO_ROOT_PASSWORD") != "fixture-s3-secret":
        errs.append("the store's root password is not the .env's S3_STORE_SECRET_KEY")
    if s3.get("ports"):
        errs.append(f"the s3 store is published on the host: {s3['ports']}")
# The chief's container and muster's side of it (deploy/docker-compose.yml,
# internal/provision/compose's "Who can read what").
def nets(s):
    n = s.get("networks") or {}
    return set(n) if isinstance(n, dict) else set(n)
def mounts(s):
    return [(v.get("type"), v.get("source"), v.get("target"), bool(v.get("read_only")), (v.get("volume") or {}).get("subpath"))
            for v in s.get("volumes", [])]
if nets(m) != {"default", "chief"}:
    errs.append(f"muster networks {sorted(nets(m))}, want default and chief")
if ("volume", "chief-spec", "/run/muster-spec", False, None) not in mounts(m):
    errs.append(f"muster does not mount chief-spec read-write at /run/muster-spec: {mounts(m)}")
if sorted(str(g) for g in m.get("group_add", [])) != ["1000", "1001"]:
    errs.append(f"muster group_add {m.get('group_add')}, want 1000 and 1001 (it gives the spec and the files their groups)")
for other in ("postgres", "s3", "stt-whisper", "stt-cohere"):
    if other in svcs and "chief" in nets(svcs[other]):
        errs.append(f"{other} is on the chief's network")
chief_env = {k: env.get(k) for k in ("MUSTER_CHIEF_RUNTIME",)}
want_env = {"MUSTER_CHIEF_RUNTIME": "compose"}
# The runtime's own defaults (cmd/muster-server composechief.go) must match
# where this file mounts the spec volume and what muster listens on.
if ("volume", "chief-spec", "/run/muster-spec", False, None) not in mounts(m) or m.get("environment", {}).get("MUSTER_PORT") != "8105":
    errs.append("muster's spec mount or port no longer matches the Compose chief runtime's defaults (/run/muster-spec, 8105)")
if profile == "chief" and chief_env != want_env:
    errs.append(f"muster's chief settings {chief_env}, want {want_env}")
if profile != "chief" and any(chief_env.values()):
    errs.append(f"muster's chief settings are not empty without MUSTER_CHIEF_RUNTIME: {chief_env}")
if "chief" in svcs:
    c = svcs["chief"]
    if c.get("image") != "ghcr.io/zacxdev/claude-code-agent:1.2.3":
        errs.append(f"chief image {c.get('image')!r}, want ghcr.io/zacxdev/claude-code-agent:1.2.3 (the release's)")
    if c.get("user") != "0:1001" or not c.get("init"):
        errs.append(f"chief user {c.get('user')!r} init {c.get('init')!r}: the supervisor runs as 0:1001 under an init")
    if c.get("cap_drop") != ["ALL"] or sorted(c.get("cap_add", [])) != ["KILL", "SETGID", "SETUID"]:
        errs.append(f"chief capabilities drop {c.get('cap_drop')} add {c.get('cap_add')}: want ALL dropped, SETUID SETGID KILL added")
    if "no-new-privileges:true" not in c.get("security_opt", []):
        errs.append("chief lacks no-new-privileges")
    if nets(c) != {"chief"}:
        errs.append(f"chief networks {sorted(nets(c))}, want only chief (off Postgres's)")
    if c.get("ports"):
        errs.append(f"chief is published on the host: {c['ports']}")
    if c.get("environment", {}).get("CCD_SPEC_DIR") != "/run/muster-spec/chief":
        errs.append(f"chief CCD_SPEC_DIR {c.get('environment', {}).get('CCD_SPEC_DIR')!r}")
    cm = mounts(c)
    for want_m in (("volume", "chief-spec", "/run/muster-spec", True, None),
                   ("volume", "chief-spec", "/etc/claude-code", True, "chief/files/etc/claude-code"),
                   ("volume", "chief-data", "/data", False, None)):
        if want_m not in cm:
            errs.append(f"chief lacks the mount {want_m}: {cm}")
    hc = " ".join(c.get("healthcheck", {}).get("test", []))
    if "/compose/status" not in hc:
        errs.append(f"chief healthcheck {hc!r} does not probe the supervisor's /compose/status")
    if c.get("depends_on", {}).get("chief-init", {}).get("condition") != "service_completed_successfully":
        errs.append("chief does not wait for chief-init")
    ci = svcs["chief-init"]
    if ci.get("restart") != "no" or ci.get("network_mode") != "none":
        errs.append(f"chief-init restart {ci.get('restart')!r} network_mode {ci.get('network_mode')!r}: a one-shot with no network")
for e in errs:
    print(f"FAIL [{profile}]: {e}")
if not errs:
    print(f"ok   [{profile}]: services {sorted(svcs)}, every long-running one with a healthcheck")
sys.exit(1 if errs else 0)
PY
done

# EMPTY .env: only the required lines. No warning, and every optional value
# muster is given is empty — so a feature can only be on because .env says so.
printf 'MUSTER_VERSION=1.2.3\nPOSTGRES_PASSWORD=p\nMUSTER_UI_PASSWORD=ui-password-fixture\nMUSTER_HOOK_TOKEN=hook-token-fixture\n' > "$tmp/env-min"
if docker compose -f "$COMPOSE" --env-file "$tmp/env-min" -p compose-check config --format json > "$tmp/min.json" 2> "$tmp/min.err"; then
  if [ -s "$tmp/min.err" ]; then
    echo "FAIL [empty .env]: docker compose config warned: $(cat "$tmp/min.err")"; fail=1
  fi
  python3 - "$tmp/min.json" <<'PY' || fail=1
import json, sys
env = json.load(open(sys.argv[1]))["services"]["muster"]["environment"]
fixed = {"DATABASE_URL", "MUSTER_PORT", "MUSTER_MIRROR", "MUSTER_UI_PASSWORD", "MUSTER_HOOK_TOKEN"}
defaults = {"MUSTER_STANDALONE": "1", "MUSTER_SECURE_COOKIES": "1", "MUSTER_SESSION_STATE": "1"}
errs = [f"{k}={env.get(k)!r}, want {v!r}" for k, v in defaults.items() if env.get(k) != v]
set_ = sorted(k for k, v in env.items() if v and k not in fixed and k not in defaults)
if set_:
    errs.append(f"optional variables that are NOT empty with nothing in .env: {set_}")
optional = [k for k in env if k not in fixed and k not in defaults]
# Positive control: the walk saw the forwarded variables at all.
if len(optional) < 60:
    errs.append(f"positive control FAILED: only {len(optional)} optional variables reached muster")
for e in errs:
    print(f"FAIL [empty .env]: {e}")
if not errs:
    print(f"ok   [empty .env]: no warning; all {len(optional)} optional variables muster is given are empty")
sys.exit(1 if errs else 0)
PY
else
  echo "FAIL [empty .env]: docker compose config refused it: $(cat "$tmp/min.err")"; fail=1
fi

# Session state is on by default, and .env can turn it off with 0 OR an empty
# value: `${…:-1}` would turn an empty value back into 1.
for v in "" 0; do
  { cat "$tmp/env-min"; echo "MUSTER_SESSION_STATE=$v"; } > "$tmp/env-ss"
  got="$(docker compose -f "$COMPOSE" --env-file "$tmp/env-ss" -p compose-check config --format json |
    python3 -c 'import json,sys; print(repr(json.load(sys.stdin)["services"]["muster"]["environment"]["MUSTER_SESSION_STATE"]))')"
  if [ "$got" = "'$v'" ]; then echo "ok   [session state]: MUSTER_SESSION_STATE='$v' in .env reaches muster as '$v' (off)"
  else echo "FAIL [session state]: MUSTER_SESSION_STATE='$v' in .env reached muster as $got"; fail=1; fi
done

# Your OWN store, with its access key and bucket forgotten: the bundled store's
# defaults must not fill them in silently (muster then says MISCONFIGURED).
{ cat "$tmp/env-min"; echo "MUSTER_S3_ENDPOINT=https://s3.example.com"; echo "MUSTER_S3_SECRET_KEY=own-store-secret"; } > "$tmp/env-own"
got="$(docker compose -f "$COMPOSE" --env-file "$tmp/env-own" -p compose-check config --format json |
  python3 -c 'import json,sys; e=json.load(sys.stdin)["services"]["muster"]["environment"]; print(" ".join(repr(e[k]) for k in ("MUSTER_S3_ENDPOINT","MUSTER_S3_BUCKET","MUSTER_S3_ACCESS_KEY","MUSTER_S3_SECRET_KEY")))')"
if [ "$got" = "'https://s3.example.com' '' '' 'own-store-secret'" ]; then
  echo "ok   [own s3 store]: no bundled bucket or access key is filled in for it"
else
  echo "FAIL [own s3 store]: endpoint, bucket, access key, secret resolved to $got"; fail=1
fi

# Your own endpoint AND the bundled store's secret left in .env: the bundled
# access key and password must never reach your endpoint. The bucket carries a
# note that is not a bucket name, so muster refuses the set and sends nothing.
{ cat "$tmp/env-min"; echo "MUSTER_S3_ENDPOINT=https://s3.example.com"; echo "S3_STORE_SECRET_KEY=bundled-secret"; } > "$tmp/env-both"
bucket="$(docker compose -f "$COMPOSE" --env-file "$tmp/env-both" -p compose-check config --format json |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["services"]["muster"]["environment"]["MUSTER_S3_BUCKET"])')"
if printf '%s' "$bucket" | grep -Eq '^[a-z0-9.-]{3,63}$'; then
  echo "FAIL [own s3 store + bundled secret]: the bucket resolved to a valid name ('$bucket'), so the bundled credentials would be sent"; fail=1
elif printf '%s' "$bucket" | grep -q 'S3_STORE_SECRET_KEY'; then
  echo "ok   [own s3 store + bundled secret]: the bucket is refused, naming S3_STORE_SECRET_KEY"
else
  echo "FAIL [own s3 store + bundled secret]: the bucket ('$bucket') does not say why"; fail=1
fi

# The bundled store refuses .env.example's placeholder as its password. The
# start script's CHECKS (everything before it starts the server) are run here:
# the placeholder must be refused, naming it, and a real secret must get past
# them (negative control).
checks="$(docker compose -f "$COMPOSE" --env-file "$tmp/env" -p compose-check --profile s3 config --format json |
  python3 -c '
import json, sys
ep = json.load(sys.stdin)["services"]["s3"]["entrypoint"]
# docker compose config keeps the $$ escapes of the file; the container sees $.
script = ep[ep.index("-c") + 1].replace("$$", "$")
head = script.split("\nsilo server", 1)
if len(head) != 2:
    sys.exit("the s3 start script no longer starts the server with `silo server`")
print(head[0] + "\necho CHECKS-PASSED")')" || { echo "FAIL [s3 store]: could not read its start script"; fail=1; }
for pw in CHANGE-ME a-real-secret-123; do
  out="$(env -i MINIO_ROOT_USER=muster MINIO_ROOT_PASSWORD="$pw" /bin/sh -c "$checks" 2>&1 || true)"
  if grep -q 'expected\|syntax error\|not found' <<<"$out"; then
    echo "FAIL [s3 store]: the start script's checks did not run cleanly: $out"; fail=1; continue
  fi
  case "$pw" in
    CHANGE-ME) if grep -q 'placeholder CHANGE-ME' <<<"$out" && ! grep -q CHECKS-PASSED <<<"$out"; then
                 echo "ok   [s3 store]: S3_STORE_SECRET_KEY=CHANGE-ME is refused, naming the placeholder"
               else echo "FAIL [s3 store]: the placeholder password was not refused: $out"; fail=1; fi ;;
    *) if grep -q CHECKS-PASSED <<<"$out"; then echo "ok   [s3 store]: a real secret passes the checks (control)"
       else echo "FAIL [s3 store]: a real secret was refused: $out"; fail=1; fi ;;
  esac
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

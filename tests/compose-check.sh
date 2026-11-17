#!/usr/bin/env bash
# deploy/docker-compose.yml is valid, and has the shape the install guide needs.
#
#   tests/compose-check.sh            (make compose-check)
#
# Static: runs `docker compose config` (no image is pulled, nothing starts) for
# the default stack and for each profile (stt, its older name stt-whisper,
# stt-cohere, s3, and stt+s3 together), against deploy/.env.example with its
# CHANGE-ME values filled in (and its three generated secrets, commented out in
# the example, uncommented: an .env that sets its own). Asserts:
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
#   * muster waits for a healthy s3 store (its bucket made) when the profile
#     is on, and does not require it when it is off (`required: false`);
#   * s3: muster's endpoint, bucket and key pair are the store's own (the
#     store's root credentials ARE muster's key pair), and the store is not
#     published on the host;
#   * muster-init (the secrets + boot-check one-shot) is in every profile: the
#     same image as muster, no network, restart "no", awaited by BOTH postgres
#     and muster; its environment IS muster's (the anchor) plus
#     POSTGRES_PASSWORD; Postgres reads its password from the file it writes
#     (POSTGRES_PASSWORD_FILE, never the variable), and muster its DATABASE_URL;
#   * NO .env AT ALL (the zero-config contract): `config` succeeds with NO
#     warning, runs ghcr.io/zacxdev/muster:<the file's default release>, and
#     every optional variable
#     muster is given resolves to EMPTY — the conditional defaults, the UI
#     password and the hook token included (muster-init generates those) —
#     except the three with fixed defaults (standalone, secure cookies, and
#     session state, ON by default);
#   * MUSTER_SESSION_STATE set EMPTY or 0 in .env reaches muster as that value
#     (both off), not as the default 1;
#   * YOUR OWN S3 store (endpoint + secret, no S3_STORE_SECRET_KEY) is never
#     handed the bundled store's bucket or access key;
#   * your own endpoint WITH S3_STORE_SECRET_KEY left set resolves to a bucket
#     muster refuses, so the bundled credentials are never sent there;
#   * the bundled store's start script refuses the CHANGE-ME placeholder;
#   * THE VERSION: the shipped .env.example (MUSTER_VERSION empty) runs the
#     file's own default (a release X.Y.Z; scripts/check-compose-default.sh
#     holds it to the newest), never an empty tag; the copy a release
#     stamps (scripts/stamp-compose-version.sh, which release-build runs) runs
#     that release in EVERY service with no .env; and NEGATIVE CONTROL, a file
#     whose default was removed is caught by the same image check — the
#     compose file must never start an unversioned image.
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
# The release the repository's copy defaults to (its one spelling).
DEFAULT_VERSION="$("$ROOT/scripts/stamp-compose-version.sh" --current < "$COMPOSE")" ||
  { echo "compose-check: deploy/docker-compose.yml has no single X.Y.Z default" >&2; exit 1; }
export DEFAULT_VERSION

# The example, filled in the way a user would — its generated secrets
# (commented out in the example) set too, as an .env that sets its own does.
# (#707 round 0, D2/F1: an uncommented POSTGRES_PASSWORD=CHANGE-ME, copied
# into a zero-config stack's .env, used to overwrite its database password.)
grep -qx '#POSTGRES_PASSWORD=' "$EXAMPLE" && grep -qx '#MUSTER_UI_PASSWORD=' "$EXAMPLE" && grep -qx '#MUSTER_HOOK_TOKEN=' "$EXAMPLE" &&
  ! grep -Eq '^(POSTGRES_PASSWORD|MUSTER_UI_PASSWORD|MUSTER_HOOK_TOKEN)=' "$EXAMPLE" ||
  { echo "FAIL [example]: deploy/.env.example must carry its three generated secrets commented out and EMPTY (#NAME=), and never set"; exit 1; }
echo "ok   [example]: the three generated secrets are commented out and empty in deploy/.env.example"
sed -e 's/^MUSTER_VERSION=$/MUSTER_VERSION=1.2.3/' -e 's/=CHANGE-ME$/=fixture-secret/' \
  -e 's/^#\(POSTGRES_PASSWORD\|MUSTER_UI_PASSWORD\|MUSTER_HOOK_TOKEN\)=$/\1=fixture-secret/' "$EXAMPLE" > "$tmp/env"
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
core = {"postgres", "muster", "muster-init"}
want = {"default": core,
        "stt": core | whisper,
        "stt+model": core | whisper,
        "stt-whisper": core | whisper,
        "stt-cohere": core | {"stt-cohere"},
        "s3": core | {"s3"},
        "stt,s3": core | {"s3"} | whisper,
        "chief": core | {"chief", "chief-init"}}[profile]
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
for k in ("MUSTER_UI_PASSWORD", "MUSTER_HOOK_TOKEN"):
    if env.get(k) != "fixture-secret":
        errs.append(f"muster {k}={env.get(k)!r}: the .env's value does not reach muster")
# The secrets volume: muster-init writes it, Postgres and muster read it.
SECRETS = "/run/muster-secrets"
want_files = {"DATABASE_URL_FILE": "database_url", "MUSTER_UI_PASSWORD_FILE": "ui_password",
              "MUSTER_HOOK_TOKEN_FILE": "hook_token", "MUSTER_VAPID_PUBLIC_KEY_FILE": "vapid_public_key",
              "MUSTER_VAPID_PRIVATE_KEY_FILE": "vapid_private_key"}
for k, f in want_files.items():
    if env.get(k) != f"{SECRETS}/{f}":
        errs.append(f"muster {k}={env.get(k)!r}, want {SECRETS}/{f}")
if "DATABASE_URL" in env:
    errs.append("muster is given DATABASE_URL directly; it must read muster-init's DATABASE_URL_FILE")
init = svcs.get("muster-init", {})
if init.get("image") != m.get("image"):
    errs.append(f"muster-init image {init.get('image')!r} is not muster's {m.get('image')!r}")
if init.get("restart") != "no" or init.get("network_mode") != "none":
    errs.append(f"muster-init restart {init.get('restart')!r} network_mode {init.get('network_mode')!r}: a one-shot with no network")
if (init.get("command") or [None])[0] != "compose-init":
    errs.append(f"muster-init command {init.get('command')!r} does not run compose-init")
# It sees Postgres's data directory READ-ONLY, at the path it is told to check
# (an existing database never gets a generated password: #707 round 2, F3).
icmd = init.get("command") or []
pgd = icmd[icmd.index("-postgres-data") + 1] if "-postgres-data" in icmd[:-1] else None
pg_vol = [v for v in svcs.get("postgres", {}).get("volumes", []) if v.get("source") == "pgdata"]
init_vol = [v for v in init.get("volumes", []) if v.get("source") == "pgdata"]
if not (pgd and pg_vol and pg_vol[0].get("target") == pgd and len(init_vol) == 1
        and init_vol[0].get("target") == pgd and init_vol[0].get("read_only") is True):
    errs.append(f"muster-init -postgres-data {pgd!r} is not Postgres's data volume mounted read-only "
                f"(postgres {pg_vol}, muster-init {init_vol})")
ienv = dict(init.get("environment") or {})
if ienv.pop("POSTGRES_PASSWORD", None) is None:
    errs.append("muster-init is not given POSTGRES_PASSWORD")
if ienv != env:
    diff = sorted(set(ienv.items()) ^ set(env.items()))[:6]
    errs.append(f"muster-init's environment is not muster's (it checks what muster gets): {diff}")
for svc in ("muster", "postgres"):
    if svcs.get(svc, {}).get("depends_on", {}).get("muster-init", {}).get("condition") != "service_completed_successfully":
        errs.append(f"{svc} does not wait for muster-init to complete")
pg = svcs.get("postgres", {})
pge = pg.get("environment", {})
if "POSTGRES_PASSWORD" in pge or pge.get("POSTGRES_PASSWORD_FILE") != f"{SECRETS}/postgres_password":
    errs.append(f"postgres environment {sorted(pge)}: it must read POSTGRES_PASSWORD_FILE={SECRETS}/postgres_password only")
if m.get("image") != "ghcr.io/zacxdev/muster:1.2.3":
    errs.append(f"muster image {m.get('image')!r}, want ghcr.io/zacxdev/muster:1.2.3")
if m.get("depends_on", {}).get("postgres", {}).get("condition") != "service_healthy":
    errs.append("muster does not wait for a healthy postgres")
# The bundled store: muster probes its bucket once at boot, so it must start
# after the store is healthy (the bucket exists) — and, the store being
# optional, never REQUIRE it (the default stack has no s3 service).
s3dep = m.get("depends_on", {}).get("s3")
if s3dep is None or s3dep.get("condition") != "service_healthy" or s3dep.get("required") is not False:
    errs.append(f"muster's depends_on s3 is {s3dep!r}: want condition service_healthy, required false "
                "(else muster probes the bucket before the store has made it)")
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
if ("volume", "chief-instructions", "/run/muster-spec/chief/files/etc/claude-code", False, None) not in mounts(m):
    errs.append(f"muster does not mount chief-instructions where its driver writes files/etc/claude-code: {mounts(m)}")
# No volume subpath anywhere: Compose before 2.25 refuses the WHOLE file over one
# (the old-Compose tier below measures that), chief profile or not.
for name, sv in svcs.items():
    for mt in mounts(sv):
        if mt[4] is not None:
            errs.append(f"{name} mounts a volume subpath {mt}: Compose before 2.25 refuses the whole file")
for svc, ro in (("muster", True), ("postgres", True), ("muster-init", False)):
    if ("volume", "secrets", SECRETS, ro, None) not in mounts(svcs.get(svc, {})):
        errs.append(f"{svc} does not mount the secrets volume at {SECRETS} {'read-only' if ro else 'read-write'}: {mounts(svcs.get(svc, {}))}")
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
                   ("volume", "chief-instructions", "/etc/claude-code", True, None),
                   ("volume", "chief-data", "/data", False, None)):
        if want_m not in cm:
            errs.append(f"chief lacks the mount {want_m}: {cm}")
    hc = " ".join(c.get("healthcheck", {}).get("test", []))
    if "/compose/status" not in hc:
        errs.append(f"chief healthcheck {hc!r} does not probe the supervisor's /compose/status")
    if c.get("depends_on", {}).get("chief-init", {}).get("condition") != "service_completed_successfully":
        errs.append("chief does not wait for chief-init")
    ci = mounts(svcs.get("chief-init", {}))
    if ("volume", "chief-instructions", "/v/chief/files/etc/claude-code", False, None) not in ci:
        errs.append(f"chief-init does not prepare the instructions volume at files/etc/claude-code: {ci}")
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

# NO .env AT ALL — the zero-config contract (clone, then `docker compose up`).
# An empty env file, and an environment with nothing of the shell's in it (a
# MUSTER_VERSION exported in the caller's shell would hide a missing default).
# No warning, the default release image, and every optional value muster is given is
# empty — so a feature can only be on because .env says so.
: > "$tmp/env-min"
bare() { env -i PATH="$PATH" HOME="$HOME" docker compose "$@"; }
if bare -f "$COMPOSE" --env-file "$tmp/env-min" -p compose-check config --format json > "$tmp/min.json" 2> "$tmp/min.err"; then
  if [ -s "$tmp/min.err" ]; then
    echo "FAIL [no .env]: docker compose config warned: $(cat "$tmp/min.err")"; fail=1
  fi
  python3 - "$tmp/min.json" <<'PY' || fail=1
import json, sys
svcs = json.load(open(sys.argv[1]))["services"]
env = svcs["muster"]["environment"]
fixed = {"MUSTER_PORT", "MUSTER_MIRROR", "DATABASE_URL_FILE", "MUSTER_UI_PASSWORD_FILE", "MUSTER_HOOK_TOKEN_FILE",
         "MUSTER_VAPID_PUBLIC_KEY_FILE", "MUSTER_VAPID_PRIVATE_KEY_FILE"}
defaults = {"MUSTER_STANDALONE": "1", "MUSTER_SECURE_COOKIES": "1", "MUSTER_SESSION_STATE": "1"}
errs = [f"{k}={env.get(k)!r}, want {v!r}" for k, v in defaults.items() if env.get(k) != v]
import os
want_img = "ghcr.io/zacxdev/muster:" + os.environ["DEFAULT_VERSION"]
for svc in ("muster", "muster-init"):
    if svcs[svc]["image"] != want_img:
        errs.append(f"with no .env, {svc} runs {svcs[svc]['image']!r}, want {want_img}")
for k in ("MUSTER_UI_PASSWORD", "MUSTER_HOOK_TOKEN"):
    if env.get(k) != "":
        errs.append(f"with no .env, {k}={env.get(k)!r}: muster-init's generated one must be read, not a default")
if svcs["muster-init"]["environment"].get("POSTGRES_PASSWORD") != "":
    errs.append("with no .env, muster-init is handed a POSTGRES_PASSWORD; it must generate one")
set_ = sorted(k for k, v in env.items() if v and k not in fixed and k not in defaults)
if set_:
    errs.append(f"optional variables that are NOT empty with nothing in .env: {set_}")
optional = [k for k in env if k not in fixed and k not in defaults]
# Positive control: the walk saw the forwarded variables at all.
if len(optional) < 60:
    errs.append(f"positive control FAILED: only {len(optional)} optional variables reached muster")
for e in errs:
    print(f"FAIL [no .env]: {e}")
if not errs:
    print(f"ok   [no .env]: no warning; the {want_img} image; all {len(optional)} optional variables muster is given are empty")
sys.exit(1 if errs else 0)
PY
else
  echo "FAIL [no .env]: docker compose config refused it: $(cat "$tmp/min.err")"; fail=1
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

# --- the version every image runs ---------------------------------------------
# images_at <compose file> <env file> <want tag>: every muster-built image of
# every profile runs <want tag>, and `config` warned of nothing. The ONE check
# the three cases below share, so its negative control is a control of it.
images_at() {
  local file="$1" envf="$2" want="$3"
  bare -f "$file" --env-file "$envf" -p compose-check --profile stt-cohere --profile chief config --format json \
    > "$tmp/ver.json" 2> "$tmp/ver.err" || { echo "config refused: $(cat "$tmp/ver.err")"; return 1; }
  if [ -s "$tmp/ver.err" ]; then echo "config warned: $(cat "$tmp/ver.err")"; return 1; fi
  python3 - "$tmp/ver.json" "$want" <<'PY'
import json, sys
svcs, want = json.load(open(sys.argv[1]))["services"], sys.argv[2]
ours = {"muster": "ghcr.io/zacxdev/muster", "muster-init": "ghcr.io/zacxdev/muster",
        "stt-cohere": "ghcr.io/zacxdev/muster-stt-cohere", "chief": "ghcr.io/zacxdev/claude-code-agent",
        "chief-init": "ghcr.io/zacxdev/claude-code-agent"}
bad = [f"{k}={svcs[k]['image']!r}" for k, repo in ours.items() if svcs[k]["image"] != f"{repo}:{want}"]
if bad:
    sys.exit("not on " + want + ": " + ", ".join(bad))
PY
}
# The shipped example leaves MUSTER_VERSION empty: the file's default applies.
if out="$(images_at "$COMPOSE" "$EXAMPLE" "$DEFAULT_VERSION" 2>&1)"; then
  echo "ok   [version]: the shipped .env.example (MUSTER_VERSION empty) runs the file's default, $DEFAULT_VERSION, in every service"
else echo "FAIL [version]: the shipped .env.example: $out"; fail=1; fi
# The release's copy: stamped, it runs that release with no .env.
"$ROOT/scripts/stamp-compose-version.sh" 1.2.3 < "$COMPOSE" > "$tmp/stamped.yml" || { echo "FAIL [version]: stamping failed"; fail=1; }
if out="$(images_at "$tmp/stamped.yml" "$tmp/env-min" 1.2.3 2>&1)"; then
  echo "ok   [version]: the release-stamped copy runs 1.2.3 in every service, with no .env"
else echo "FAIL [version]: the release-stamped copy: $out"; fail=1; fi
# NEGATIVE CONTROL: the default removed and MUSTER_VERSION set EMPTY (so compose
# has nothing to warn about), the IMAGE check itself must catch the empty tag.
sed "s/\${MUSTER_VERSION:-$DEFAULT_VERSION}/\${MUSTER_VERSION}/g" "$COMPOSE" > "$tmp/nodefault.yml"
grep -q 'MUSTER_VERSION:-' "$tmp/nodefault.yml" && { echo "FAIL [version, negative control]: a default survived the removal"; fail=1; }
echo 'MUSTER_VERSION=' > "$tmp/env-emptyver"
if out="$(images_at "$tmp/nodefault.yml" "$tmp/env-emptyver" "$DEFAULT_VERSION" 2>&1)"; then
  echo "FAIL [version, negative control]: a file with no version default PASSED the image check (an unversioned image would start)"; fail=1
elif grep -q "not on $DEFAULT_VERSION: muster=" <<<"$out"; then
  echo "ok   [version, negative control]: a file with no version default is caught by the image check"
else echo "FAIL [version, negative control]: refused, but not by the image check: $out"; fail=1; fi

# docs/auth.md's GoTrue override (onboarding round 3: a blind install had to
# guess every value). It parses with this compose file; the GoTrue it adds
# issues what muster checks (issuer and audience read from config.go, the
# secret muster's own), runs the image the e2e GoTrue tests run, is awaited
# healthy by muster, and is the address the doc's .env gives muster; the
# browser port it publishes is the doc's MUSTER_BROWSER_PORT, on 127.0.0.1.
if python3 - "$ROOT" "$COMPOSE" "$tmp" <<'PY'
import json, re, subprocess, sys
from pathlib import Path
root, compose, tmp = Path(sys.argv[1]), sys.argv[2], Path(sys.argv[3])
doc = (root / "docs/auth.md").read_text()
sec = doc.split("### A GoTrue server beside the Compose stack", 1)
if len(sec) != 2:
    sys.exit("docs/auth.md has no 'A GoTrue server beside the Compose stack' section")
body = re.split(r"\n#{2,3} ", sec[1], 1)[0]
yml = re.search(r"```yaml\n(.*?)```", body, re.S)
dotenv = re.search(r"```sh\n(.*?)```", body, re.S)
if not yml or not dotenv:
    sys.exit("the section lost its yaml override or its .env block")
want_env = {}
for line in dotenv.group(1).splitlines():
    m = re.match(r"^([A-Z_]+)=(\S+)", line)
    if m:
        want_env[m.group(1)] = m.group(2)
(tmp / "gotrue-override.yml").write_text(yml.group(1))
env = {**want_env, "MUSTER_GOTRUE_JWT_SECRET": "fixture-gotrue-jwt-secret-0123456789abcdef",
       "GOTRUE_DB_PASSWORD": "fixture-gotrue-db"}
(tmp / "env-gotrue").write_text("".join(f"{k}={v}\n" for k, v in env.items()))
r = subprocess.run(["docker", "compose", "-f", compose, "-f", str(tmp / "gotrue-override.yml"), "--env-file",
                    str(tmp / "env-gotrue"), "-p", "compose-check", "config", "--format", "json"],
                   capture_output=True, text=True)
if r.returncode:
    sys.exit(f"docker compose config refused the override: {r.stderr.strip()}")
svcs = json.loads(r.stdout)["services"]
errs = []
# Turned off by deleting its .env lines, the override left behind: Compose
# must refuse it, which is what install.sh's pre-update check relies on.
(tmp / "env-gotrue-gone").write_text("COMPOSE_PROFILES=\n")
r = subprocess.run(["docker", "compose", "-f", compose, "-f", str(tmp / "gotrue-override.yml"), "--env-file",
                    str(tmp / "env-gotrue-gone"), "-p", "compose-check", "config", "-q"], capture_output=True, text=True)
if r.returncode == 0 or "required variable" not in r.stderr:
    errs.append(f"the override without its .env lines still parses (rc {r.returncode}): install.sh could not refuse it")
gt, m = svcs.get("gotrue"), svcs.get("muster", {})
if not gt or "gotrue-db" not in svcs:
    sys.exit(f"services {sorted(svcs)}: no gotrue / gotrue-db")
src = (root / "cmd/muster-server/config.go").read_text()
iss = re.search(r'gotrueIssuer\s*=\s*"([^"]+)"', src).group(1)
aud = re.search(r'gotrueAudience\s*=\s*"([^"]+)"', src).group(1)
ge = gt["environment"]
for k, v in {"GOTRUE_JWT_ISSUER": iss, "GOTRUE_JWT_AUD": aud,
             "GOTRUE_JWT_SECRET": env["MUSTER_GOTRUE_JWT_SECRET"]}.items():
    if ge.get(k) != v:
        errs.append(f"gotrue {k}={ge.get(k)!r}, muster checks {v!r}")
e2e = re.search(r"(?m)^    image: (supabase/gotrue:\S+)$", (root / "e2e/gotrue/compose.yml").read_text()).group(1)
if gt["image"] != e2e:
    errs.append(f"gotrue image {gt['image']!r} is not the one the e2e tests run ({e2e!r})")
gurl = re.match(r"^http://([a-z0-9-]+):(\d+)$", want_env.get("MUSTER_GOTRUE_URL", ""))
if not gurl or gurl.group(1) != "gotrue" or gurl.group(2) != ge.get("GOTRUE_API_PORT"):
    errs.append(f"the doc's MUSTER_GOTRUE_URL {want_env.get('MUSTER_GOTRUE_URL')!r} is not the gotrue service's port {ge.get('GOTRUE_API_PORT')!r}")
if m.get("environment", {}).get("MUSTER_GOTRUE_URL") != want_env.get("MUSTER_GOTRUE_URL"):
    errs.append("muster is not given the doc's MUSTER_GOTRUE_URL")
bp = want_env.get("MUSTER_BROWSER_PORT")
if not any(str(p.get("target")) == bp and str(p.get("published")) == bp and p.get("host_ip") == "127.0.0.1"
           for p in m.get("ports", [])):
    errs.append(f"muster ports {m.get('ports')} do not publish the doc's MUSTER_BROWSER_PORT={bp} on 127.0.0.1")
if want_env.get("MUSTER_PUBLIC_URL") != f"http://localhost:{bp}":
    errs.append(f"the doc's MUSTER_PUBLIC_URL {want_env.get('MUSTER_PUBLIC_URL')!r} is not the browser listener")
if m.get("depends_on", {}).get("gotrue", {}).get("condition") != "service_healthy":
    errs.append("muster does not wait for a healthy gotrue")
for n in ("gotrue", "gotrue-db"):
    if not (svcs[n].get("healthcheck") or {}).get("test"):
        errs.append(f"{n} has no healthcheck")
if errs:
    sys.exit("; ".join(errs))
PY
then echo "ok   [gotrue override]: docs/auth.md's override parses, issues what muster checks, and publishes the browser port"
else echo "FAIL [gotrue override]: see the line above"; fail=1; fi

# --- the oldest Compose this file must parse on ------------------------------
# Compose validates the WHOLE file against its own schema before it applies any
# profile, so a key an old Compose does not know refuses `up` for every user,
# whether or not they use the profile that needs it (#474 round 1 F1: a volume
# `subpath`, which Compose refuses before 2.25). COMPOSE_CHECK_OLD names an old
# docker-compose binary; CI pins one (.github/workflows/ci.yml).
if [ -n "${COMPOSE_CHECK_OLD:-}" ]; then
  old="$COMPOSE_CHECK_OLD"
  oldv="$("$old" version --short 2>/dev/null)" || { echo "compose-check: COMPOSE_CHECK_OLD=$old does not run" >&2; exit 2; }
  # Negative control first: the same binary must REFUSE the file with a subpath
  # mount put back, or it could not see the break this tier exists for.
  python3 - "$COMPOSE" "$tmp/subpath.yml" <<'PY'
import sys
s = open(sys.argv[1]).read()
anchor = "      - chief-data:/data\n"
assert s.count(anchor) == 1, "the chief's data mount moved; update this control"
s = s.replace(anchor, anchor + "      - type: volume\n        source: chief-spec\n        target: /mnt/control\n"
              "        volume:\n          subpath: chief/files\n")
open(sys.argv[2], "w").write(s)
PY
  if "$old" -f "$tmp/subpath.yml" --env-file "$tmp/env" -p compose-check config -q 2>"$tmp/old-neg.err"; then
    echo "FAIL [compose $oldv, negative control]: a volume subpath was ACCEPTED; this binary cannot see the break"; fail=1
  elif grep -q subpath "$tmp/old-neg.err"; then
    echo "ok   [compose $oldv, negative control]: a volume subpath is refused, naming it"
  else
    echo "FAIL [compose $oldv, negative control]: refused, but not for subpath: $(cat "$tmp/old-neg.err")"; fail=1
  fi
  for profile in default stt s3 chief; do
    envfile="$tmp/env"; [ "$profile" = chief ] && envfile="$tmp/env-chief"
    args=(-f "$COMPOSE" --env-file "$envfile" -p compose-check)
    [ "$profile" = default ] || args+=(--profile "$profile")
    if "$old" "${args[@]}" config -q 2>"$tmp/old.err"; then
      echo "ok   [compose $oldv, $profile]: the file parses"
    else
      echo "FAIL [compose $oldv, $profile]: $(cat "$tmp/old.err")"; fail=1
    fi
  done
  # The zero-config stack on the old Compose: no .env, and muster-init's
  # environment (a YAML merge of muster's anchor) resolves to muster's own.
  if env -i PATH="$PATH" HOME="$HOME" "$old" -f "$COMPOSE" --env-file "$tmp/env-min" -p compose-check config --format json \
      > "$tmp/old-min.json" 2>"$tmp/old.err" && [ ! -s "$tmp/old.err" ] &&
     python3 -c 'import json,sys; s=json.load(open(sys.argv[1]))["services"]; e=dict(s["muster-init"]["environment"]); e.pop("POSTGRES_PASSWORD"); sys.exit(0 if e == s["muster"]["environment"] and s["muster"]["image"].endswith(":"+sys.argv[2]) else 1)' "$tmp/old-min.json" "$DEFAULT_VERSION"; then
    echo "ok   [compose $oldv, no .env]: the file parses, and muster-init's merged environment is muster's"
  else
    echo "FAIL [compose $oldv, no .env]: $(cat "$tmp/old.err")"; fail=1
  fi
else
  echo "note compose-check: the old-Compose tier did not run (set COMPOSE_CHECK_OLD to a docker-compose binary; CI does)"
fi

exit "$fail"

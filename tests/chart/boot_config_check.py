#!/usr/bin/env python3
"""Does muster-server ACCEPT the configuration a chart value set renders?

The chart turns values into environment variables; muster-server refuses at
boot any combination it cannot run (docs/configuration.md: "The server checks
its configuration at boot and refuses to start on an invalid or
contradictory combination"). Rendering and schema-validating a value set
proves neither that the variables are spelled the way the server reads them
nor that their combination boots. This runs the REAL binary on the rendered
environment and reads what it says.

  python3 tests/chart/boot_config_check.py <muster-server binary> <rendered.yaml> [--expect-refusal REGEX]

How the environment is rebuilt from the rendered manifests:
  * the server Deployment's `muster` container: `value`s as written;
    `secretKeyRef`s read from the rendered Secrets (the chart's generated
    one), else from FIXTURE_SECRETS below (the `existingSecret`s a value set
    names, which a real install makes itself); `configMapKeyRef`s from the
    rendered ConfigMaps; `envFrom` secretRefs from FIXTURE_SECRETS whole.
    A reference that resolves to nothing is a FAILURE, not an empty value.
  * DATABASE_URL is replaced by a closed loopback port: config validation
    runs before the database is dialled, so a value set the server accepts
    gets as far as `db: connect attempt …` and keeps retrying, and one it
    refuses exits with a `config:` line first.

Verdict, read from the server's own output (never its exit status alone):
  ACCEPTED: alive at the deadline, its log shows a database connect attempt, and
  no `config:` refusal line was printed.
  REFUSED:  a `config:` line. With --expect-refusal the line must match.
"""
import argparse
import base64
import os
import re
import subprocess
import sys
import tempfile

import yaml

DEADLINE_S = 8


def rsa_pem() -> str:
    # The GitHub App key must PARSE (a key that does not parse refuses to
    # boot), so make a throwaway one here rather than commit a PEM.
    with tempfile.TemporaryDirectory() as d:
        p = os.path.join(d, "k.pem")
        subprocess.run(["openssl", "genrsa", "-out", p, "2048"], check=True, capture_output=True)
        return open(p).read()


def fixture_secrets() -> dict:
    """The Secrets a value set names as existingSecret, with values of the
    shape the server checks (lengths, encodings). Names match charts/muster/ci/."""
    return {
        "muster-database": {"DATABASE_URL": "postgres://muster:x@db.example.invalid:5432/muster"},
        "muster-auth": {"MUSTER_UI_PASSWORD": "fixture-password-0123", "MUSTER_HOOK_TOKEN": "fixture-hook-token-0123456789"},
        "muster-claude-accounts": {
            "MUSTER_AGENT_CC_TOKEN_TEAM_A": "sk-ant-oat01-fixture-a",
            "MUSTER_AGENT_CC_TOKEN_TEAM_B": "sk-ant-oat01-fixture-b",
        },
        "muster-officer-tokens": {"MUSTER_OFFICER_TOKEN_RELEASE_EXAMPLE_WEB": "hq-fixture-officer-token"},
        "muster-github-app": {"MUSTER_GITHUB_APP_PRIVATE_KEY": rsa_pem()},
        "muster-openrouter": {"MUSTER_AGENT_OPENROUTER_KEY": "sk-or-v1-fixture"},
    }


def load(path):
    with open(path) as f:
        return [d for d in yaml.safe_load_all(f) if d]


def server_container(docs):
    deps = [d for d in docs if d["kind"] == "Deployment"
            and d["metadata"].get("labels", {}).get("app.kubernetes.io/component") == "server"]
    if len(deps) != 1:
        sys.exit(f"boot-config: want exactly one server Deployment, found {len(deps)}")
    cs = [c for c in deps[0]["spec"]["template"]["spec"]["containers"] if c["name"] == "muster"]
    if len(cs) != 1:
        sys.exit("boot-config: the server Deployment has no `muster` container")
    return cs[0]


def build_env(docs, fixtures):
    secrets, cms = {}, {}
    for d in docs:
        if d["kind"] == "Secret":
            data = {k: base64.b64decode(v).decode() for k, v in (d.get("data") or {}).items()}
            data.update(d.get("stringData") or {})
            secrets[d["metadata"]["name"]] = data
        elif d["kind"] == "ConfigMap":
            cms[d["metadata"]["name"]] = d.get("data") or {}
    for name, data in fixtures.items():
        secrets.setdefault(name, data)
    c = server_container(docs)
    env = {}
    for src in c.get("envFrom") or []:
        ref = src.get("secretRef", {}).get("name")
        if ref not in secrets:
            sys.exit(f"boot-config: envFrom names Secret {ref!r}, which is neither rendered nor a fixture")
        env.update(secrets[ref])
    for e in c.get("env") or []:
        if "value" in e:
            env[e["name"]] = str(e["value"])
            continue
        vf = e.get("valueFrom", {})
        if "secretKeyRef" in vf:
            r = vf["secretKeyRef"]
            try:
                env[e["name"]] = secrets[r["name"]][r["key"]]
            except KeyError:
                sys.exit(f"boot-config: {e['name']} reads Secret {r['name']}/{r['key']}, which does not resolve")
        elif "configMapKeyRef" in vf:
            r = vf["configMapKeyRef"]
            try:
                env[e["name"]] = cms[r["name"]][r["key"]]
            except KeyError:
                sys.exit(f"boot-config: {e['name']} reads ConfigMap {r['name']}/{r['key']}, which does not resolve")
        else:
            sys.exit(f"boot-config: {e['name']} has a valueFrom this check cannot resolve: {vf}")
    env["DATABASE_URL"] = "postgres://muster:x@127.0.0.1:1/muster?sslmode=disable"
    return env


def run(binary, env):
    with tempfile.TemporaryDirectory() as tmp:
        env = dict(env, HOME=tmp, TMPDIR=tmp)
        p = subprocess.Popen([binary], env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        try:
            out, _ = p.communicate(timeout=DEADLINE_S)
            alive = False
        except subprocess.TimeoutExpired:
            p.kill()
            out, _ = p.communicate()
            alive = True
    return alive, out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("binary")
    ap.add_argument("rendered")
    ap.add_argument("--expect-refusal", metavar="REGEX")
    a = ap.parse_args()
    env = build_env(load(a.rendered), fixture_secrets())
    alive, out = run(a.binary, env)
    refusals = [l for l in out.splitlines() if re.search(r"\bconfig: ", l)]
    dialled = "db: connect attempt" in out
    name = os.path.basename(a.rendered)
    if a.expect_refusal:
        if refusals and any(re.search(a.expect_refusal, l) for l in refusals):
            print(f"ok   {name}: REFUSED as expected: {refusals[0][:200]}")
            return 0
        print(f"FAIL {name}: expected a refusal matching {a.expect_refusal!r}; alive={alive} "
              f"refusals={refusals[:2]}\n--- output ---\n{out[-3000:]}")
        return 1
    if alive and dialled and not refusals:
        print(f"ok   {name}: ACCEPTED ({len(env)} variables; reached the db connect)")
        return 0
    print(f"FAIL {name}: alive={alive} dialled={dialled} refusals={refusals[:3]}\n--- output ---\n{out[-3000:]}")
    return 1


if __name__ == "__main__":
    sys.exit(main())

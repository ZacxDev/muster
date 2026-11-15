#!/usr/bin/env python3
"""What the Helm chart renders, and refuses to render, for values a test set
does not cover.

  python3 tests/chart/chart_render_test.py        (helm on PATH; CI's `chart` job)

Every case reads the RENDERED objects (or helm's refusal), never the template
source. HELM and CHART override the helm binary and the chart directory, and
RELEASE_BUILD the release script, so the same cases can be run against an older
copy of the chart (that is how each was shown to fail before its fix).
"""
import os
import shutil
import subprocess
import tempfile
import unittest

import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
HELM = os.environ.get("HELM", "helm")
CHART = os.environ.get("CHART", os.path.join(ROOT, "charts", "muster"))
RELEASE_BUILD = os.environ.get("RELEASE_BUILD", os.path.join(ROOT, "scripts", "release-build.sh"))

# The agent plane's smallest valid values.
AGENTS = ["agents.enabled=true", "agents.gatewayImage.repository=registry.example.com/agent"]


def helm(sets=(), files=(), strings=()):
    """Render the chart; returns (returncode, objects, stderr)."""
    cmd = [HELM, "template", "t", CHART, "-n", "muster", "--set", "image.tag=1.2.3"]
    for f in files:
        cmd += ["-f", os.path.join(CHART, "ci", f) if not os.path.isabs(f) else f]
    for s in sets:
        cmd += ["--set", s]
    for s in strings:
        cmd += ["--set-string", s]
    p = subprocess.run(cmd, capture_output=True, text=True)
    objs = [d for d in yaml.safe_load_all(p.stdout) if d] if p.returncode == 0 else []
    return p.returncode, objs, p.stderr


def render(case, **kw):
    rc, objs, err = helm(**kw)
    if rc != 0:
        case.fail(f"the chart refused to render {kw}: {err}")
    if not objs:
        case.fail(f"instrument check: {kw} rendered no object")
    return objs


def refused(case, needle, **kw):
    rc, _, err = helm(**kw)
    case.assertNotEqual(rc, 0, f"{kw} rendered; it must be refused (naming {needle!r})")
    case.assertIn(needle, err, f"{kw} was refused, but not for {needle!r}: {err}")


def kinds(objs, kind):
    return [o for o in objs if o["kind"] == kind]


def server_env(objs):
    deps = [o for o in kinds(objs, "Deployment")
            if o["metadata"]["labels"].get("app.kubernetes.io/component") == "server"]
    assert len(deps) == 1, f"want one server Deployment, found {len(deps)}"
    c = [c for c in deps[0]["spec"]["template"]["spec"]["containers"] if c["name"] == "muster"][0]
    return {e["name"]: e for e in c.get("env") or []}


def secret_refs(objs):
    """Every Secret name any rendered pod reads (env, envFrom)."""
    out = set()
    for o in objs:
        spec = (((o.get("spec") or {}).get("template") or {}).get("spec")) or {}
        for c in (spec.get("containers") or []) + (spec.get("initContainers") or []):
            for e in c.get("env") or []:
                ref = (e.get("valueFrom") or {}).get("secretKeyRef")
                if ref:
                    out.add(ref["name"])
            for e in c.get("envFrom") or []:
                if "secretRef" in e:
                    out.add(e["secretRef"]["name"])
    return out


class Preview(unittest.TestCase):
    """The preview gateway is not in the chart (v1): its values are refused and
    nothing renders it."""

    def test_preview_values_are_refused(self):
        # A COMPLETE preview configuration (one the chart with the gateway
        # rendered), so the refusal is the schema's, not a missing value's.
        refused(self, "additional properties 'preview' not allowed",
                sets=["plugins=true", "publicUrl=https://muster.example.com", "preview.enabled=true", "preview.domain=preview.example.com",
                      "preview.pluginTokensSecret=preview-tokens"])

    def test_no_value_set_renders_a_preview_object(self):
        for f in sorted(os.listdir(os.path.join(CHART, "ci"))):
            text = yaml.safe_dump_all(render(self, files=[f]))
            for word in ("muster-preview", "PREVIEW_", "MUSTER_PLUGIN_ORIGINS"):
                self.assertNotIn(word, text, f"ci/{f} renders {word}")


class BundledServiceSwitches(unittest.TestCase):
    """The bundled store and STT run on attachments/dictation alone; the dead
    s3.enabled / stt.enabled switches are refused, not silently ignored."""

    def test_dead_switches_are_refused(self):
        refused(self, "at '/s3': additional properties 'enabled' not allowed", sets=["s3.enabled=true"])
        refused(self, "at '/stt': additional properties 'enabled' not allowed", sets=["stt.enabled=true"])

    def test_the_bundles_follow_their_feature(self):
        # Positive control: the features that DO switch them render them.
        objs = render(self, sets=["attachments.enabled=true", "dictation.enabled=true"])
        names = {o["metadata"]["name"] for o in objs}
        self.assertIn("t-muster-s3", names)
        self.assertIn("t-muster-stt", names)
        names = {o["metadata"]["name"] for o in render(self)}
        self.assertNotIn("t-muster-s3", names)
        self.assertNotIn("t-muster-stt", names)


class GoTrueOfYourOwn(unittest.TestCase):
    """auth.mode=gotrue without the bundled GoTrue must name a GoTrue and its
    JWT secret; otherwise the server would point at a GoTrue that does not run."""

    base = ["auth.mode=gotrue", "publicUrl=https://muster.example.com"]

    def test_no_url_is_refused(self):
        refused(self, "gotrue.url", sets=self.base)
        refused(self, "gotrue.url", sets=self.base + ["gotrue.existingSecret=mine"])

    def test_no_secret_is_refused(self):
        refused(self, "gotrue.existingSecret", sets=self.base + ["gotrue.url=https://auth.example.com"])

    def test_both_render_and_are_used(self):
        env = server_env(render(self, sets=self.base + ["gotrue.url=https://auth.example.com", "gotrue.existingSecret=mine"]))
        self.assertEqual(env["MUSTER_GOTRUE_URL"]["value"], "https://auth.example.com")
        self.assertEqual(env["MUSTER_GOTRUE_JWT_SECRET"]["valueFrom"]["secretKeyRef"]["name"], "mine")

    def test_the_bundled_one_needs_neither(self):
        env = server_env(render(self, sets=self.base + ["gotrue.enabled=true"]))
        self.assertEqual(env["MUSTER_GOTRUE_URL"]["value"], "http://t-muster-gotrue:9999")


class SecretsGenerateFalse(unittest.TestCase):
    """secrets.generate=false: no Secret is made, nothing reads one, and a
    feature that is on without its existingSecret is refused by name."""

    def test_every_secret_yours_renders_no_generated_secret(self):
        objs = render(self, files=["own-secrets.yaml"])
        self.assertEqual([o["metadata"]["name"] for o in kinds(objs, "Secret")], [])
        refs = secret_refs(objs)
        self.assertNotIn("t-muster-generated", refs)
        # Positive control: the pods DO read Secrets, the user's.
        self.assertTrue({"muster-auth", "muster-postgres", "muster-push", "muster-gotrue"} <= refs, refs)

    def test_a_missing_one_is_named(self):
        refused(self, "push.existingSecret", files=["own-secrets.yaml"], sets=["push.existingSecret="])
        refused(self, "postgres.existingSecret", files=["own-secrets.yaml"], sets=["postgres.existingSecret="])
        refused(self, "auth.existingSecret", files=["own-secrets.yaml"], sets=["auth.existingSecret="])

    def test_bundled_gotrue_reads_its_database_password_from_yours(self):
        objs = render(self, files=["own-secrets.yaml"], sets=["gotrue.enabled=true", "gotrue.url="])
        refs = secret_refs([o for o in objs if o["metadata"]["name"] == "t-muster-gotrue"])
        self.assertEqual(refs, {"muster-gotrue", "muster-postgres"})

    def test_generate_true_makes_one(self):
        # Positive control: the default does make the Secret.
        objs = render(self)
        self.assertEqual([o["metadata"]["name"] for o in kinds(objs, "Secret")], ["t-muster-generated"])


class AgentRBAC(unittest.TestCase):
    """Least-privilege RBAC and its admission policy are the default; the wide
    layout is opt-in and grants no `list` on namespaces."""

    def test_default_is_least_privilege_with_the_policy(self):
        objs = render(self, sets=AGENTS)
        self.assertIn("MUSTER_AGENT_NAMESPACE_CLUSTERROLE", server_env(objs))
        vaps = kinds(objs, "ValidatingAdmissionPolicy")
        self.assertEqual(len(vaps), 1)
        self.assertEqual(len(kinds(objs, "ValidatingAdmissionPolicyBinding")), 1)
        spec = vaps[0]["spec"]
        rules = {(tuple(r["resources"]), tuple(r["operations"])) for r in spec["matchConstraints"]["resourceRules"]}
        self.assertEqual(rules, {(("namespaces",), ("CREATE", "DELETE")),
                                 (("rolebindings",), ("CREATE", "UPDATE", "DELETE"))})
        exprs = [v["expression"] for v in spec["validations"]]
        self.assertIn('request.resource.resource != "namespaces" || request.name.startsWith("muster-agent-")', exprs)
        self.assertIn('request.resource.resource != "rolebindings" || request.namespace.startsWith("muster-agent-")', exprs)
        self.assertEqual(spec["matchConditions"][0]["expression"],
                         'request.userInfo.username == "system:serviceaccount:muster:t-muster"')

    def test_the_policy_follows_the_prefix(self):
        objs = render(self, sets=AGENTS + ["agents.namespacePrefix=team-x-"])
        exprs = " ".join(v["expression"] for v in kinds(objs, "ValidatingAdmissionPolicy")[0]["spec"]["validations"])
        self.assertIn('startsWith("team-x-")', exprs)
        self.assertNotIn("muster-agent-", exprs)

    def test_the_policy_can_be_turned_off(self):
        objs = render(self, sets=AGENTS + ["agents.rbac.admissionPolicy.enabled=false"])
        self.assertEqual(kinds(objs, "ValidatingAdmissionPolicy"), [])

    def test_cluster_wide_is_opt_in_and_lists_no_namespaces(self):
        objs = render(self, sets=AGENTS + ["agents.rbac.layout=cluster-wide"])
        self.assertNotIn("MUSTER_AGENT_NAMESPACE_CLUSTERROLE", server_env(objs))
        ns_rules = [r for o in kinds(objs, "ClusterRole") for r in o["rules"] if "namespaces" in r["resources"]]
        self.assertEqual([sorted(r["verbs"]) for r in ns_rules], [["create", "delete", "get"]])
        self.assertEqual(len(kinds(objs, "ValidatingAdmissionPolicy")), 1, "the policy is on in this layout too")

    def test_the_old_layout_name_is_refused(self):
        refused(self, "layout", sets=AGENTS + ["agents.rbac.layout=default"])


class ReleaseBuildUsageExit(unittest.TestCase):
    """scripts/release-build.sh exits 2 (usage/setup), not 1 (a check failed),
    when deploy/.env.example is missing — as for the compose file beside it."""

    def test_missing_env_example_exits_2(self):
        with tempfile.TemporaryDirectory() as d:
            os.makedirs(os.path.join(d, "scripts"))
            os.makedirs(os.path.join(d, "deploy"))
            shutil.copy(RELEASE_BUILD, os.path.join(d, "scripts", "release-build.sh"))
            open(os.path.join(d, "deploy", "docker-compose.yml"), "w").close()
            p = subprocess.run(["bash", os.path.join(d, "scripts", "release-build.sh"), "1.2.3"],
                               capture_output=True, text=True, env=dict(os.environ, DIST=os.path.join(d, "dist")))
            self.assertIn("deploy/.env.example is missing", p.stderr)
            self.assertEqual(p.returncode, 2, p.stderr)
            # Control: with it present, the next missing piece is named instead.
            open(os.path.join(d, "deploy", ".env.example"), "w").close()
            p = subprocess.run(["bash", os.path.join(d, "scripts", "release-build.sh"), "1.2.3"],
                               capture_output=True, text=True, env=dict(os.environ, DIST=os.path.join(d, "dist")))
            self.assertNotIn(".env.example is missing", p.stderr)
            self.assertIn("charts/muster/Chart.yaml is missing", p.stderr)
            self.assertEqual(p.returncode, 2, p.stderr)


if __name__ == "__main__":
    unittest.main(verbosity=2)

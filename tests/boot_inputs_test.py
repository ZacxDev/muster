#!/usr/bin/env python3
"""Every way the repository starts muster-server still satisfies its boot rules.

muster-server refuses to boot without a non-empty MUSTER_HOOK_TOKEN, and
serves /metrics only on MUSTER_METRICS_PORT. Both are invisible to a job that
does not run on a draft (kind, e2e, the release dry run), so this checks the
INPUTS those jobs use, in milliseconds:

  * the e2e harness's one server launcher (e2e/lib/server.ts startServer)
    passes MUSTER_HOOK_TOKEN, and no e2e spec, script, chart value set or
    Compose smoke sets it to an empty value;
  * nothing fetches /metrics from a main-listener base URL.

  python3 tests/boot_inputs_test.py
"""
import os
import re
import unittest

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

# Where test and install inputs live (not the docs, not Go: Go fixtures are
# validated by the Go suite, which runs on drafts).
SCAN_DIRS = ["e2e", "tests", "scripts", "images", "charts", "deploy"]
SCAN_EXT = (".ts", ".mjs", ".js", ".sh", ".py", ".yaml", ".yml", ".env", ".example", ".tpl")

# An EMPTY assignment of the token, in the shapes these files use:
#   MUSTER_HOOK_TOKEN: ''   MUSTER_HOOK_TOKEN: ""   MUSTER_HOOK_TOKEN=   --set ...hookToken=
EMPTY_TOKEN = re.compile(r"""MUSTER_HOOK_TOKEN\s*(:\s*(''|""|``)\s*[,}]|=\s*$|=\s*['"]{2})""")

# /metrics fetched from a base that is a main/browser listener in these files.
MAIN_METRICS = re.compile(r"""(origin|baseURL|BASE|PUBLIC|base)\b[^\n]{0,8}['"]?\s*\+?\s*['"`]?/metrics\b|\$\{?(BASE|PUBLIC)\}?/metrics""")


def scanned_files():
    for d in SCAN_DIRS:
        for dirpath, dirnames, filenames in os.walk(os.path.join(ROOT, d)):
            dirnames[:] = [x for x in dirnames if x not in ("node_modules", "__pycache__")]
            for name in filenames:
                if name.endswith(SCAN_EXT) or name == ".env.example":
                    yield os.path.join(dirpath, name)


def lines(path):
    with open(path, encoding="utf-8", errors="replace") as f:
        for n, line in enumerate(f, 1):
            yield n, line.rstrip("\n")


class BootInputs(unittest.TestCase):
    def test_the_patterns_can_match(self):
        # Positive controls: each pattern matches the shape it exists for.
        for s in ("MUSTER_HOOK_TOKEN: '',", 'MUSTER_HOOK_TOKEN: ""}', "MUSTER_HOOK_TOKEN=", "MUSTER_HOOK_TOKEN=''"):
            self.assertRegex(s, EMPTY_TOKEN)
        for s in ("MUSTER_HOOK_TOKEN: hookToken,", "MUSTER_HOOK_TOKEN=fixture-secret", "MUSTER_HOOK_TOKEN: ${MUSTER_HOOK_TOKEN:-}"):
            self.assertNotRegex(s, EMPTY_TOKEN)
        for s in ("rig.origin + '/metrics'", 'curl "$BASE/metrics"', "${PUBLIC}/metrics"):
            self.assertRegex(s, MAIN_METRICS)
        self.assertNotRegex("`http://127.0.0.1:${telemetryMetricsPort}/metrics`", MAIN_METRICS)
        self.assertGreater(sum(1 for _ in scanned_files()), 50, "the walk found almost nothing")

    def test_the_e2e_launcher_arms_the_hook_tier(self):
        with open(os.path.join(ROOT, "e2e", "lib", "server.ts")) as f:
            src = f.read()
        body = src[src.index("export async function startServer"):]
        body = body[: body.index("stdio:")]
        self.assertIn("MUSTER_HOOK_TOKEN: hookToken", body,
                      "startServer no longer passes MUSTER_HOOK_TOKEN: every e2e server would refuse to boot")

    def test_nothing_sets_an_empty_hook_token(self):
        bad = []
        for path in scanned_files():
            rel = os.path.relpath(path, ROOT)
            for n, line in lines(path):
                stripped = line.lstrip()
                if stripped.startswith("#"):  # commented examples (.env.example)
                    continue
                if EMPTY_TOKEN.search(line) and "dev_hook_token_test" not in rel and "boot_inputs_test" not in rel:
                    bad.append(f"{rel}:{n}: {line.strip()}")
        self.assertEqual(bad, [], "an empty MUSTER_HOOK_TOKEN refuses boot")

    def test_nothing_scrapes_metrics_from_a_main_listener(self):
        bad = []
        for path in scanned_files():
            rel = os.path.relpath(path, ROOT)
            if rel == os.path.join("tests", "boot_inputs_test.py"):
                continue
            for n, line in lines(path):
                if MAIN_METRICS.search(line):
                    bad.append(f"{rel}:{n}: {line.strip()}")
        self.assertEqual(bad, [], "/metrics is served only on MUSTER_METRICS_PORT")


if __name__ == "__main__":
    unittest.main(verbosity=2)

#!/usr/bin/env python3
"""goselect.py: the selection, the fallback rules, the always-run scan and the
miss ledger, on synthetic `go list` graphs (no `go`, no git).

    python3 tests/goselect_test.py

Each case changes ONE thing against a baseline that selects a strict subset,
so a case cannot pass because the whole suite was selected for another reason.
"""

from __future__ import annotations

import io
import json
import os
import sys
import tempfile
import unittest
from contextlib import redirect_stdout
from pathlib import Path
from types import SimpleNamespace

sys.path.insert(0, str(Path(__file__).resolve().parent))
import goselect  # noqa: E402

M = "example.test/m"
ROOT = "/r"


def pkg(path: str, deps: list[str], tests: bool = True, embed: list[str] | None = None) -> list[dict]:
    """The `go list -test -json` entries for one package."""
    ip = f"{M}/{path}" if path else M
    out = [{"ImportPath": ip, "Dir": f"{ROOT}/{path}".rstrip("/"), "EmbedFiles": embed or [],
            "TestGoFiles": [f"{os.path.basename(path) or 'm'}_test.go"] if tests else []}]
    if tests:
        out += [{"ImportPath": f"{ip} [{ip}.test]", "ForTest": ip},
                {"ImportPath": f"{ip}.test", "Deps": [f"{M}/{d}" for d in deps] + [f"{ip} [{ip}.test]", "fmt"]}]
    return out


# leaf <- mid <- top; other is unrelated; nontested has no tests; web embeds static/.
ENTRIES = (pkg("leaf", []) + pkg("mid", ["leaf"]) + pkg("top", ["mid", "leaf"]) + pkg("other", [])
           + pkg("nontested", [], tests=False) + pkg("web", [], embed=["static/app.js"]))
ALLP, TESTED = goselect.graph(ENTRIES, ROOT)
# Weights that keep every reverse-dependency selection here under 50%.
COUNTS = {f"{M}/{p}": n for p, n in {"leaf": 1, "mid": 1, "top": 1, "other": 10, "web": 1}.items()}


def sel(changed: list[str], always: dict[str, str] | None = None, counts=COUNTS) -> dict:
    return goselect.select(changed, ALLP, TESTED, always or {}, counts)


def names(r: dict) -> list[str]:
    return [s.removeprefix(M + "/") for s in r["selected"]]


class Selection(unittest.TestCase):
    def test_a_leaf_change_selects_every_test_binary_containing_it(self):
        r = sel(["leaf/leaf.go"])
        self.assertIsNone(r["fallback"])
        self.assertEqual(names(r), ["leaf", "mid", "top"])

    def test_a_mid_change_does_not_select_what_it_imports(self):
        self.assertEqual(names(sel(["mid/mid.go"])), ["mid", "top"])

    def test_a_test_file_change_selects_only_its_package(self):
        self.assertEqual(names(sel(["leaf/leaf_test.go"])), ["leaf"])

    def test_testdata_selects_its_package_only(self):
        self.assertEqual(names(sel(["leaf/testdata/golden.txt"])), ["leaf"])

    def test_an_embedded_file_is_a_change_to_its_package(self):
        r = sel(["web/static/app.js"])
        self.assertEqual(names(r), ["web"])
        self.assertIn("imports", r["why"][f"{M}/web"])

    def test_a_package_without_tests_still_selects_its_importers(self):
        entries = ENTRIES + pkg("uses", ["nontested"])
        allp, tested = goselect.graph(entries, ROOT)
        r = goselect.select(["nontested/x.go"], allp, tested, {}, COUNTS | {f"{M}/uses": 1})
        self.assertEqual(names(r), ["uses"])

    def test_a_file_outside_every_package_selects_only_the_always_run_set(self):
        self.assertEqual(names(sel(["docs/x.md"])), [])
        r = sel(["docs/x.md"], always={f"{M}/other": "reads ../docs"})
        self.assertEqual(names(r), ["other"])
        self.assertTrue(r["why"][f"{M}/other"].startswith("always-run"))

    def test_counts(self):
        r = sel(["mid/mid.go"])
        self.assertEqual((r["selected_tests"], r["total_tests"], r["total_packages"]), (2, 14, 5))


class Fallback(unittest.TestCase):
    def test_every_fallback_rule_runs_everything_and_names_its_reason(self):
        for rule, why in goselect.FALLBACK:
            path = rule + "x/y.go" if rule.endswith("/") else rule
            with self.subTest(rule=rule):
                r = sel(["leaf/leaf_test.go", path])
                self.assertEqual(r["fallback"], f"{path}: {why}")
                self.assertEqual(r["selected_tests"], r["total_tests"])

    def test_the_fallback_list_is_exactly_this(self):
        # Literal, so deleting a rule from goselect.py fails here (the loop above
        # derives its cases from the list itself and cannot see a deletion).
        self.assertEqual([r for r, _ in goselect.FALLBACK], [
            "go.mod", "go.sum", "package.json", "package-lock.json", "e2e/package.json", "e2e/package-lock.json",
            "flake.nix", "flake.lock", ".github/", "tests/", "Makefile", "docker-compose.test.yml",
            "internal/dbtest/", "internal/db/migrations/", "cmd/muster-migrate/"])
        self.assertTrue(all(why for _, why in goselect.FALLBACK))

    def test_a_prefix_rule_is_a_directory_not_a_string_prefix(self):
        self.assertIsNone(goselect.fallback_for("testsuite/x.go"))
        self.assertIsNone(goselect.fallback_for("go.modfoo"))
        self.assertIsNotNone(goselect.fallback_for("tests/goselect.py"))

    def test_more_than_half_of_the_tests_runs_everything(self):
        r = sel(["leaf/leaf.go"], counts=COUNTS | {f"{M}/top": 100})
        self.assertIn("(> 50%)", r["fallback"])
        self.assertIsNone(sel(["leaf/leaf.go"])["fallback"])

    def test_exactly_half_is_not_more_than_half(self):
        counts = {f"{M}/leaf": 1, f"{M}/mid": 0, f"{M}/top": 0, f"{M}/other": 1, f"{M}/web": 0}
        self.assertIsNone(sel(["leaf/leaf.go"], counts=counts)["fallback"])

    def test_the_selector_failing_falls_back(self):
        import subprocess
        here = os.getcwd()
        with tempfile.TemporaryDirectory() as d:
            subprocess.run(["git", "init", "-q", d], check=True)
            out = f"{d}/s.json"
            try:
                for base, want in (("", "selector error: no base revision"), ("0000000", "selector error: no base revision")):
                    os.chdir(d)
                    with redirect_stdout(io.StringIO()):
                        goselect.cmd_select(SimpleNamespace(base=base, head="HEAD", out=out))
                    self.assertTrue(json.loads(Path(out).read_text())["fallback"].startswith(want), base)
                os.chdir(tempfile.gettempdir())  # not a repository: still a fallback, never a raise
                with redirect_stdout(io.StringIO()):
                    goselect.cmd_select(SimpleNamespace(base="HEAD~1", head="HEAD", out=out))
                self.assertTrue(json.loads(Path(out).read_text())["fallback"].startswith("selector error:"))
            finally:
                os.chdir(here)


class AlwaysRun(unittest.TestCase):
    def scan(self, text: str) -> dict:
        with tempfile.TemporaryDirectory() as d:
            os.makedirs(f"{d}/p")
            Path(f"{d}/p/p_test.go").write_text(text)
            return goselect.scan_always({"x/p": {"dir": "p", "testfiles": ["p_test.go"]}}, d)

    def test_each_sign_is_seen(self):
        for src in ['os.ReadFile("../../docs/a.md")', 'filepath.Join("..", "web")', "root := moduleRoot(t)",
                    "r := repoRoot(t)", "_, f, _, _ := runtime.Caller(0)", 'exec.Command("go", "list")',
                    'exec.CommandContext(ctx, "go", "build")']:
            with self.subTest(src=src):
                self.assertIn("x/p", self.scan(f"package p\nfunc f() {{ {src} }}\n"))

    def test_a_package_reading_only_its_own_files_is_not_always_run(self):
        self.assertEqual(self.scan('package p\nfunc f() { os.ReadFile("testdata/a.txt"); exec.Command("git") }\n'), {})


class Ledger(unittest.TestCase):
    def ledger(self, selection: dict, results: dict, mode: str = "shadow") -> tuple[dict, str]:
        with tempfile.TemporaryDirectory() as d:
            Path(f"{d}/s.json").write_text(json.dumps(selection))
            Path(f"{d}/r.json").write_text(json.dumps(results))
            buf = io.StringIO()
            env = os.environ.pop("GITHUB_STEP_SUMMARY", None)
            try:
                with redirect_stdout(buf):
                    goselect.cmd_ledger(SimpleNamespace(selection=f"{d}/s.json", results=f"{d}/r.json", run_id="7",
                                                        sha="abc", event="pull_request", shard="0", mode=mode,
                                                        out=f"{d}/l.json"))
            finally:
                if env is not None:
                    os.environ["GITHUB_STEP_SUMMARY"] = env
            return json.loads(Path(f"{d}/l.json").read_text()), buf.getvalue()

    SEL = {"selected": ["p/a"], "fallback": None, "total_packages": 2}
    RES = {"p/a\tTestA": "pass", "p/a\tTestB": "fail", "p/b\tTestC": "fail", "p/b\tTestD": "pass", "p/b\tTestE": "skip"}

    def test_a_failure_outside_the_selection_is_a_miss(self):
        rec, out = self.ledger(self.SEL, self.RES)
        self.assertEqual(rec["failed"], ["p/a.TestB", "p/b.TestC"])
        self.assertEqual(rec["failed_outside_selection"], ["p/b.TestC"])
        self.assertEqual((rec["selected_tests"], rec["total_tests"], rec["selected_packages"]), (2, 4, 1))
        self.assertEqual(out.splitlines()[-1], "SELECTIVE-LEDGER " + json.dumps(rec, sort_keys=True))
        for k in ("run_id", "sha", "event", "fallback"):
            self.assertIn(k, rec)

    def test_a_fallback_run_has_no_misses(self):
        rec, _ = self.ledger(self.SEL | {"fallback": "go.mod: x"}, self.RES)
        self.assertEqual(rec["failed_outside_selection"], [])
        self.assertEqual(rec["fallback"], "go.mod: x")
        self.assertEqual(rec["selected_tests"], rec["total_tests"])

    def test_unreadable_results_say_so(self):
        rec, _ = self.ledger(self.SEL, {})
        self.assertFalse(rec["results_read"])


if __name__ == "__main__":
    unittest.main()

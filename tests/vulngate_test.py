#!/usr/bin/env python3
"""vulngate.py's verdict and the CI job's wiring to it (no network).

    python3 tests/vulngate_test.py

The streams below have govulncheck's own shape (`config`, `osv` and `finding`
messages; a symbol-level finding's trace starts at a function). Each refusal
case differs from a passing one in exactly one respect and asserts its own
message, so it cannot pass because a different branch answered the same way.
"""

from __future__ import annotations

import contextlib
import datetime as dt
import io
import json
import re
import sys
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT / ".github" / "scripts"))
import vulngate  # noqa: E402
from vulngate import Malformed, parse_allowlist, parse_stream, verdict  # noqa: E402

TODAY = dt.date(2000, 6, 15)
A, B = "GO-2000-0101", "GO-2000-0202"

CONFIG = {"config": {"scanner_name": "govulncheck", "scan_level": "symbol", "scan_mode": "source"}}


def osv(i):
    return {"osv": {"id": i, "summary": f"summary of {i}"}}


def called(i):
    return {"finding": {"osv": i, "trace": [
        {"module": "stdlib", "package": "net/http", "receiver": "*Server", "function": "Serve"},
        {"module": "example.com/m", "package": "example.com/m/internal/api", "function": "Run"}]}}


def imported_only(i):
    return {"finding": {"osv": i, "trace": [{"module": "stdlib", "package": "net/http"}]}}


def module_only(i):
    return {"finding": {"osv": i, "trace": [{"module": "stdlib", "version": "v1.0.0"}]}}


def stream(*objs) -> str:
    # govulncheck writes indented, concatenated objects, not one per line.
    return "\n".join(json.dumps(o, indent=2) for o in objs)


def run(objs, allow_text=""):
    return verdict(parse_stream(stream(*objs)), parse_allowlist(allow_text), TODAY)


class Verdict(unittest.TestCase):
    def test_clean_scan_passes(self):
        rc, lines = run([CONFIG])
        self.assertEqual(rc, 0, lines)
        self.assertIn("0 reachable", lines[-1])

    def test_unlisted_reachable_advisory_fails_and_is_named(self):
        rc, lines = run([CONFIG, osv(A), called(A)])
        self.assertEqual(rc, 1)
        self.assertTrue(any(l.startswith(f"REACHABLE {A}: summary of {A}") for l in lines), lines)
        self.assertTrue(any("http.*Server.Serve <- api.Run" in l for l in lines), lines)

    def test_listed_unexpired_reachable_advisory_passes(self):
        rc, lines = run([CONFIG, osv(A), called(A)], f"{A} 2000-07-01 no fixed release yet\n")
        self.assertEqual(rc, 0, lines)
        self.assertIn(f"allowed {A} until 2000-07-01 (line 1): no fixed release yet", lines)

    def test_expiry_date_itself_is_still_honoured(self):
        rc, lines = run([CONFIG, osv(A), called(A)], f"{A} 2000-06-15 last day\n")
        self.assertEqual(rc, 0, lines)

    def test_listed_but_expired_reachable_advisory_fails_naming_the_entry(self):
        rc, lines = run([CONFIG, osv(A), called(A)], f"{A} 2000-06-14 was waiting on upstream\n")
        self.assertEqual(rc, 1)
        self.assertTrue(any(l.startswith(f"EXPIRED allowlist entry {A} (line 1, expired 2000-06-14)") for l in lines), lines)
        self.assertFalse(any(l.startswith(f"allowed {A}") for l in lines), lines)

    def test_expired_entry_fails_even_when_no_longer_reported(self):
        rc, lines = run([CONFIG], f"{A} 2000-06-14 stale\n")
        self.assertEqual(rc, 1)
        self.assertTrue(any(l.startswith(f"EXPIRED allowlist entry {A}") for l in lines), lines)

    def test_unexpired_entry_no_longer_reported_passes_with_a_note(self):
        rc, lines = run([CONFIG], f"{A} 2000-07-01 waiting\n")
        self.assertEqual(rc, 0, lines)
        self.assertTrue(any(l.startswith(f"note: allowlist entry {A}") for l in lines), lines)

    def test_an_entry_for_one_advisory_does_not_cover_another(self):
        rc, lines = run([CONFIG, osv(A), called(A), osv(B), called(B)], f"{A} 2000-07-01 waiting\n")
        self.assertEqual(rc, 1)
        self.assertTrue(any(l.startswith(f"REACHABLE {B}") for l in lines), lines)
        self.assertFalse(any(l.startswith(f"REACHABLE {A}") for l in lines), lines)

    def test_imported_or_module_level_findings_do_not_fail(self):
        rc, lines = run([CONFIG, osv(A), imported_only(A), osv(B), module_only(B)])
        self.assertEqual(rc, 0, lines)

    def test_the_checked_in_allowlist_parses_and_has_nothing_expired(self):
        allow = parse_allowlist((ROOT / ".github" / "govulncheck-allowlist.txt").read_text())
        today = dt.datetime.now(dt.timezone.utc).date()
        expired = [e.osv for e in allow.values() if e.expires < today]
        self.assertEqual(expired, [], "expired entries in .github/govulncheck-allowlist.txt")


class Malformedness(unittest.TestCase):
    def assertMalformed(self, text, needle):
        with self.assertRaises(Malformed) as cm:
            parse_allowlist(text)
        self.assertIn(needle, str(cm.exception))

    def test_comments_and_blank_lines_are_ignored(self):
        self.assertEqual(parse_allowlist("# header\n\n   # indented\n"), {})
        self.assertEqual(set(parse_allowlist(f"{A} 2000-07-01 why  # trailing\n")), {A})

    def test_missing_reason(self):
        self.assertMalformed(f"{A} 2000-07-01\n", "line 1: want")

    def test_bad_id(self):
        self.assertMalformed("CVE-2000-1234 2000-07-01 why\n", "is not a Go advisory ID")

    def test_bad_date(self):
        self.assertMalformed(f"{A} 2000-13-01 why\n", "is not a YYYY-MM-DD date")
        self.assertMalformed(f"{A} soon why\n", "is not a YYYY-MM-DD date")

    def test_duplicate_id(self):
        self.assertMalformed(f"{A} 2000-07-01 one\n{A} 2000-07-02 two\n", "already listed on line 1")

    def test_stream_without_config_is_refused(self):
        with self.assertRaises(Malformed) as cm:
            parse_stream(stream(osv(A), called(A)))
        self.assertIn("no `config` message", str(cm.exception))
        with self.assertRaises(Malformed):
            parse_stream("")

    def test_garbage_stream_is_refused(self):
        with self.assertRaises(Malformed) as cm:
            parse_stream(stream(CONFIG) + "\n{not json")
        self.assertIn("not a govulncheck JSON stream", str(cm.exception))


class Main(unittest.TestCase):
    """The exit codes the CI step sees."""

    def main(self, allow_text, stdin_text, tmp_name):
        p = Path(self.tmp) / tmp_name
        p.write_text(allow_text)
        old = sys.stdin
        sys.stdin = io.StringIO(stdin_text)
        out, err = io.StringIO(), io.StringIO()
        try:
            with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
                rc = vulngate.main(["--allowlist", str(p), "--today", TODAY.isoformat()])
        finally:
            sys.stdin = old
        return rc, out.getvalue(), err.getvalue()

    def setUp(self):
        import tempfile
        self._d = tempfile.TemporaryDirectory()
        self.tmp = self._d.name

    def tearDown(self):
        self._d.cleanup()

    def test_exit_codes(self):
        self.assertEqual(self.main("", stream(CONFIG), "a")[0], 0)
        self.assertEqual(self.main("", stream(CONFIG, osv(A), called(A)), "b")[0], 1)
        rc, _, err = self.main("bogus line\n", stream(CONFIG), "c")
        self.assertEqual(rc, 2)
        self.assertIn("allowlist line 1", err)
        rc, _, err = self.main("", "", "d")
        self.assertEqual(rc, 2)
        self.assertIn("no `config` message", err)

    def test_missing_allowlist_file_is_a_refusal(self):
        old = sys.stdin
        sys.stdin = io.StringIO(stream(CONFIG))
        try:
            with contextlib.redirect_stderr(io.StringIO()):
                rc = vulngate.main(["--allowlist", str(Path(self.tmp) / "absent"), "--today", TODAY.isoformat()])
        finally:
            sys.stdin = old
        self.assertEqual(rc, 2)


class Wiring(unittest.TestCase):
    """The govulncheck job must hand its scan to this verdict, with this allowlist."""

    def job(self) -> str:
        ci = (ROOT / ".github" / "workflows" / "ci.yml").read_text()
        m = re.search(r"(?ms)^  govulncheck:\n(.*?)(?=^  [A-Za-z0-9_-]+:\n)", ci)
        self.assertIsNotNone(m, "no `govulncheck:` job in ci.yml")
        return m.group(1)

    def test_job_scans_in_json_and_runs_the_gate_with_the_allowlist(self):
        job = self.job()
        runs = [l.strip() for l in job.splitlines() if "govulncheck@" in l or "vulngate" in l]
        scan = [l for l in runs if "govulncheck@" in l and "./..." in l]
        self.assertEqual(len(scan), 1, runs)
        self.assertIn("-format json", scan[0])
        self.assertRegex(scan[0], r">\s*govulncheck\.json")
        self.assertTrue(any("python3 .github/scripts/vulngate.py --allowlist .github/govulncheck-allowlist.txt < govulncheck.json" in l
                            for l in runs), runs)
        self.assertTrue(any("python3 tests/vulngate_test.py" in l for l in runs), runs)


if __name__ == "__main__":
    unittest.main(verbosity=1)

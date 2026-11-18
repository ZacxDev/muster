#!/usr/bin/env python3
"""tag_gate.py's decision and publish gate, on synthetic API answers and
`needs` contexts (no network).

    python3 tests/tag_gate_test.py

Every retest/refusal case differs from the one SKIP case (or the one PASSING
gate) in exactly one respect and asserts its own reason, so it cannot pass
because a different branch happened to answer the same way.
"""

from __future__ import annotations

import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / ".github" / "scripts"))
import tag_gate  # noqa: E402
from tag_gate import decide, gate  # noqa: E402

SHA = "a" * 40


def run(**kw) -> dict:
    r = {"id": 7, "head_sha": SHA, "head_branch": "main", "event": "push",
         "status": "completed", "conclusion": "success", "created_at": "2026-11-13T10:00:00Z"}
    r.update(kw)
    return r


def answer(*runs) -> callable:
    return lambda: {"workflow_runs": list(runs)}


class Decide(unittest.TestCase):
    def test_skips_when_mains_run_succeeded(self):
        retest, why = decide("tag", SHA, answer(run()))
        self.assertFalse(retest)
        self.assertIn("main run 7", why)

    def retests(self, reason: str, ref_type: str, fetch):
        retest, why = decide(ref_type, SHA, fetch)
        self.assertTrue(retest, why)
        self.assertIn(reason, why)

    def test_not_a_tag(self):
        def boom():
            raise AssertionError("a non-tag run must not look anything up")
        self.retests("not a tag run", "branch", boom)

    def test_lookup_error(self):
        def boom():
            raise RuntimeError("gh api exited 1: HTTP 403")
        self.retests("could not read main's runs", "tag", boom)

    def test_malformed_answer(self):
        self.retests("could not read main's runs", "tag", lambda: {"message": "Not Found"})

    def test_no_runs(self):
        self.retests("no completed push run", "tag", answer())

    def test_other_sha(self):
        self.retests("no completed push run", "tag", answer(run(head_sha="b" * 40)))

    def test_other_branch(self):
        self.retests("no completed push run", "tag", answer(run(head_branch="feature")))

    def test_other_event(self):
        self.retests("no completed push run", "tag", answer(run(event="pull_request")))

    def test_still_running(self):
        self.retests("no completed push run", "tag", answer(run(status="in_progress", conclusion=None)))

    def test_failed(self):
        self.retests("concluded 'failure'", "tag", answer(run(conclusion="failure")))

    def test_latest_run_decides(self):
        old_green = run(id=1, created_at="2026-11-13T09:00:00Z")
        new_red = run(id=2, created_at="2026-11-13T11:00:00Z", conclusion="failure")
        self.retests("main run 2", "tag", answer(old_green, new_red))
        retest, _ = decide("tag", SHA, answer(run(id=1, conclusion="failure", created_at="2026-11-13T09:00:00Z"),
                                              run(id=2, created_at="2026-11-13T11:00:00Z")))
        self.assertFalse(retest)


def needs(verdict_result="success", retest="false", **jobs) -> dict:
    n = {"main-verdict": {"result": verdict_result, "outputs": {"retest": retest} if retest else {}}}
    for k, v in (jobs or {"test": "skipped", "e2e": "skipped"}).items():
        n[k] = {"result": v, "outputs": {}}
    return n


class Gate(unittest.TestCase):
    def passes(self, n):
        ok, lines = gate(n)
        self.assertTrue(ok, lines)

    def refuses(self, reason, n):
        ok, lines = gate(n)
        self.assertFalse(ok, lines)
        self.assertTrue(any(reason in l for l in lines), lines)

    def test_skipped_tests_behind_a_green_main_run(self):
        self.passes(needs())

    def test_every_job_succeeded_on_a_non_tag_run(self):
        self.passes(needs("skipped", None, test="success", e2e="success"))

    def test_every_job_succeeded_after_a_retest(self):
        self.passes(needs("success", "true", test="success", e2e="success"))

    def test_skip_without_a_decision(self):
        self.refuses("no main run vouched", needs("success", "true"))

    def test_skip_when_the_verdict_job_gave_no_answer(self):
        self.refuses("no main run vouched", needs("success", None))

    def test_skip_on_a_non_tag_run(self):
        self.refuses("no main run vouched", needs("skipped", None))

    def test_skip_when_the_verdict_job_failed(self):
        self.refuses("no main run vouched", needs("failure", "false", test="success", e2e="skipped"))

    def test_verdict_job_failed(self):
        self.refuses("REFUSED: main-verdict failure", needs("failure", None, test="success", e2e="success"))

    def test_a_failed_test_job(self):
        self.refuses("REFUSED: test: failure", needs("skipped", None, test="failure", e2e="success"))

    def test_a_cancelled_test_job(self):
        self.refuses("REFUSED: e2e: cancelled", needs(test="skipped", e2e="cancelled"))

    def test_verdict_job_not_needed(self):
        self.refuses("does not need", {"test": {"result": "success"}})


if __name__ == "__main__":
    unittest.main(verbosity=1)

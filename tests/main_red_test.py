#!/usr/bin/env python3
"""main_red.py's verdict and its issue actions, against a recorded fake `gh`
(no network).

    python3 tests/main_red_test.py

Each action case asserts the exact `gh` calls made, so a case cannot pass
because a different branch happened to make no call.
"""

from __future__ import annotations

import json
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / ".github" / "scripts"))
from main_red import JOB_NAME, act, verdict  # noqa: E402

REPO = "o/r"
SHA = "a" * 40
URL = "https://example.test/run/1"


def needs(**results) -> str:
    # job ids carry hyphens (main-verdict); kwargs spell them with underscores
    return json.dumps({k.replace("_", "-"): {"result": v, "outputs": {}} for k, v in results.items()})


RUN = 500  # this run's number; the fakes list runs around it
MAIN_RED_DONE = "completed success"      # a newer run's main-red that has acted
MAIN_RED_RUNNING = "in_progress null"    # ... still deciding
MAIN_RED_SKIPPED = "completed skipped"   # ... skipped: the run was cancelled


class FakeGh:
    """runs: [(run_number, main-red job "status conclusion" or None)], newest
    first as GitHub lists them; run ids are 9000 + run_number. The run list
    answers `run_number id`; each run's job list answers its main-red job."""

    def __init__(self, runs=((RUN, MAIN_RED_RUNNING), (499, MAIN_RED_DONE)), open_issues="",
                 runs_rc=0, jobs_rc=0, list_rc=0, write_rc=0):
        self.calls: list[list[str]] = []
        self.runs, self.open = list(runs), open_issues
        self.runs_rc, self.jobs_rc, self.list_rc, self.write_rc = runs_rc, jobs_rc, list_rc, write_rc

    def __call__(self, args):
        self.calls.append(args)
        if args[0] == "api" and "/actions/workflows/" in args[1]:
            return self.runs_rc, "".join(f"{n} {9000 + n}\n" for n, _ in self.runs)
        if args[0] == "api" and "/jobs" in args[1]:
            run_id = int(args[1].split("/actions/runs/")[1].split("/")[0])
            job = dict(self.runs).get(run_id - 9000)
            return self.jobs_rc, (job + "\n") if job else ""
        if args[:2] == ["issue", "list"]:
            return self.list_rc, self.open
        if args[:2] in (["issue", "create"], ["issue", "close"], ["issue", "comment"]):
            return self.write_rc, ""
        return 0, ""

    def verbs(self):
        return [" ".join(c[:2]) if c[0] != "api" else ("api jobs" if "/jobs" in c[1] else "api") for c in self.calls]


class Verdict(unittest.TestCase):
    def test_a_failed_job_is_red(self):
        self.assertEqual(verdict(needs(build="success", test="failure", main_verdict="skipped")), "red")

    def test_success_and_main_verdicts_skip_are_green(self):
        self.assertEqual(verdict(needs(build="success", test="success", main_verdict="skipped")), "green")

    def test_any_other_skip_is_an_undecided_skip_and_red(self):
        self.assertEqual(verdict(needs(build="success", docshots="skipped", main_verdict="skipped")), "red")

    def test_a_cancelled_job_decides_nothing_even_beside_a_failure(self):
        self.assertEqual(verdict(needs(build="cancelled", test="failure")), "none")

    def test_an_unreadable_or_empty_context_decides_nothing(self):
        for raw in ("", "not json", "[]", "{}", json.dumps({"x": {}})):
            with self.subTest(raw=raw):
                self.assertEqual(verdict(raw), "none")


class Act(unittest.TestCase):
    def test_red_with_no_open_issue_opens_one_assigned_to_the_owner(self):
        gh = FakeGh()
        self.assertEqual(act(REPO, SHA, URL, "red", gh, assignee="owner1", run_number=RUN), "opened the main-red issue")
        self.assertEqual(gh.verbs(), ["api", "issue list", "label create", "issue create"])
        create = gh.calls[-1]
        self.assertEqual(create[create.index("--title") + 1], "main is red")
        self.assertEqual(create[create.index("--label") + 1], "main-red")
        self.assertIn(SHA, create[create.index("--body") + 1])
        self.assertEqual(create[create.index("--assignee") + 1], "owner1")

    def test_a_failed_create_or_close_says_so(self):
        self.assertEqual(act(REPO, SHA, URL, "red", FakeGh(write_rc=1), run_number=RUN), "FAILED to open the main-red issue")
        self.assertEqual(act(REPO, SHA, URL, "green", FakeGh(open_issues="9", write_rc=1), run_number=RUN), "closed #9 (FAILED to close)")

    def test_red_with_open_issues_comments_on_the_oldest_and_opens_none(self):
        gh = FakeGh(open_issues="12\n9\n")
        self.assertEqual(act(REPO, SHA, URL, "red", gh, run_number=RUN), "commented on #9")
        self.assertEqual(gh.verbs(), ["api", "issue list", "issue comment"])
        self.assertEqual(gh.calls[-1][2], "9")

    def test_green_closes_every_open_issue(self):
        gh = FakeGh(open_issues="9\n12\n")
        self.assertEqual(act(REPO, SHA, URL, "green", gh, run_number=RUN), "closed #9, #12")
        self.assertEqual(gh.verbs(), ["api", "issue list", "issue comment", "issue close", "issue comment", "issue close"])
        self.assertEqual([c[2] for c in gh.calls if c[:2] == ["issue", "close"]], ["9", "12"])

    def test_green_with_nothing_open_does_nothing(self):
        gh = FakeGh()
        self.assertEqual(act(REPO, SHA, URL, "green", gh, run_number=RUN), "green, no open issue")
        self.assertEqual(gh.verbs(), ["api", "issue list"])

    def test_a_late_older_green_after_a_newer_red_does_not_close(self):
        # Run 500 is green but finishes AFTER run 501's main-red opened the issue.
        gh = FakeGh(runs=[(501, MAIN_RED_DONE), (RUN, MAIN_RED_RUNNING)], open_issues="9")
        self.assertIn("run #501, whose main-red has already acted", act(REPO, SHA, URL, "green", gh, run_number=RUN))
        self.assertEqual(gh.verbs(), ["api", "api jobs"])

    def test_a_late_older_red_after_a_newer_green_does_not_reopen(self):
        gh = FakeGh(runs=[(502, MAIN_RED_DONE), (RUN, MAIN_RED_RUNNING)])
        self.assertIn("run #502", act(REPO, SHA, URL, "red", gh, run_number=RUN))
        self.assertNotIn("issue create", gh.verbs())

    def test_a_newer_run_still_building_images_after_its_main_red_acted_still_blocks(self):
        # Audit round 1 on #872: a newer RUN stays in_progress ~10 min after its
        # main-red acted (the image jobs). What decides is its main-red JOB.
        gh = FakeGh(runs=[(501, MAIN_RED_DONE), (RUN, MAIN_RED_RUNNING)], open_issues="9")
        act(REPO, SHA, URL, "green", gh, run_number=RUN)
        self.assertNotIn("issue close", gh.verbs())

    def test_the_newest_green_closes_even_while_a_newer_push_is_still_running(self):
        # #843: every green finished after the next push, so "only main's head"
        # never closed it. A newer run whose main-red has not acted does not block.
        gh = FakeGh(runs=[(501, MAIN_RED_RUNNING), (RUN, MAIN_RED_RUNNING), (499, MAIN_RED_DONE)], open_issues="9")
        self.assertEqual(act(REPO, SHA, URL, "green", gh, run_number=RUN), "closed #9")
        self.assertEqual(gh.verbs(), ["api", "api jobs", "issue list", "issue comment", "issue close"])

    def test_a_newer_run_with_no_main_red_job_yet_does_not_block(self):
        gh = FakeGh(runs=[(501, None), (RUN, MAIN_RED_RUNNING)])
        self.assertEqual(act(REPO, SHA, URL, "red", gh, run_number=RUN), "opened the main-red issue")

    def test_the_newest_red_opens(self):
        gh = FakeGh(runs=[(RUN, MAIN_RED_RUNNING)])
        self.assertEqual(act(REPO, SHA, URL, "red", gh, run_number=RUN), "opened the main-red issue")
        self.assertEqual(gh.verbs(), ["api", "issue list", "label create", "issue create"])

    def test_a_newer_cancelled_run_decides_nothing_and_does_not_block(self):
        gh = FakeGh(runs=[(501, MAIN_RED_SKIPPED), (RUN, MAIN_RED_RUNNING)], open_issues="9")
        self.assertEqual(act(REPO, SHA, URL, "green", gh, run_number=RUN), "closed #9")

    def test_the_run_and_job_lists_are_asked_for_main_pushes_of_ci_yml(self):
        gh = FakeGh(runs=[(501, MAIN_RED_RUNNING), (RUN, MAIN_RED_RUNNING)])
        act(REPO, SHA, URL, "green", gh, run_number=RUN)
        self.assertEqual(gh.calls[0][1], "repos/o/r/actions/workflows/ci.yml/runs?branch=main&event=push&per_page=50")
        self.assertEqual(gh.calls[1][1], "repos/o/r/actions/runs/9501/jobs?per_page=100")
        self.assertIn('"main red? (open or close the main-red issue)"', gh.calls[1][3])

    def test_a_failed_run_job_or_issue_lookup_does_nothing(self):
        for kw, runs in (({"runs_rc": 1}, None), ({"jobs_rc": 1}, [(501, MAIN_RED_DONE)]), ({"list_rc": 1}, None)):
            with self.subTest(**kw):
                gh = FakeGh(**kw) if runs is None else FakeGh(runs=runs, **kw)
                self.assertIn("nothing to do", act(REPO, SHA, URL, "red", gh, run_number=RUN))
                self.assertNotIn("issue create", gh.verbs())

    def test_job_name_is_ci_yml_main_red_jobs_name(self):
        # The name newer_finished() matches a run's main-red job by must be the
        # one ci.yml gives it, or no newer run ever reads as having decided.
        ci = (Path(__file__).resolve().parent.parent / ".github" / "workflows" / "ci.yml").read_text()
        self.assertTrue(f"    name: {JOB_NAME}\n" in ci, f"ci.yml has no job named {JOB_NAME!r}")

    def test_no_verdict_makes_no_call(self):
        gh = FakeGh()
        self.assertIn("nothing to do", act(REPO, SHA, URL, "none", gh, run_number=RUN))
        self.assertEqual(gh.calls, [])


if __name__ == "__main__":
    unittest.main(verbosity=2)

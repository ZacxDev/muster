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
from main_red import act, verdict  # noqa: E402

REPO = "o/r"
SHA = "a" * 40
URL = "https://example.test/run/1"


def needs(**results) -> str:
    # job ids carry hyphens (main-verdict); kwargs spell them with underscores
    return json.dumps({k.replace("_", "-"): {"result": v, "outputs": {}} for k, v in results.items()})


class FakeGh:
    def __init__(self, head=SHA, open_issues="", head_rc=0, list_rc=0, write_rc=0):
        self.calls: list[list[str]] = []
        self.head, self.open, self.head_rc, self.list_rc = head, open_issues, head_rc, list_rc
        self.write_rc = write_rc

    def __call__(self, args):
        self.calls.append(args)
        if args[0] == "api":
            return self.head_rc, self.head + "\n"
        if args[:2] == ["issue", "list"]:
            return self.list_rc, self.open
        if args[:2] in (["issue", "create"], ["issue", "close"], ["issue", "comment"]):
            return self.write_rc, ""
        return 0, ""

    def verbs(self):
        return [" ".join(c[:2]) if c[0] != "api" else "api" for c in self.calls]


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
        self.assertEqual(act(REPO, SHA, URL, "red", gh, assignee="owner1"), "opened the main-red issue")
        self.assertEqual(gh.verbs(), ["api", "issue list", "label create", "issue create"])
        create = gh.calls[-1]
        self.assertEqual(create[create.index("--title") + 1], "main is red")
        self.assertEqual(create[create.index("--label") + 1], "main-red")
        self.assertIn(SHA, create[create.index("--body") + 1])
        self.assertEqual(create[create.index("--assignee") + 1], "owner1")

    def test_a_failed_create_or_close_says_so(self):
        self.assertEqual(act(REPO, SHA, URL, "red", FakeGh(write_rc=1)), "FAILED to open the main-red issue")
        self.assertEqual(act(REPO, SHA, URL, "green", FakeGh(open_issues="9", write_rc=1)), "closed #9 (FAILED to close)")

    def test_red_with_open_issues_comments_on_the_oldest_and_opens_none(self):
        gh = FakeGh(open_issues="12\n9\n")
        self.assertEqual(act(REPO, SHA, URL, "red", gh), "commented on #9")
        self.assertEqual(gh.verbs(), ["api", "issue list", "issue comment"])
        self.assertEqual(gh.calls[-1][2], "9")

    def test_green_closes_every_open_issue(self):
        gh = FakeGh(open_issues="9\n12\n")
        self.assertEqual(act(REPO, SHA, URL, "green", gh), "closed #9, #12")
        self.assertEqual(gh.verbs(), ["api", "issue list", "issue comment", "issue close", "issue comment", "issue close"])
        self.assertEqual([c[2] for c in gh.calls if c[:2] == ["issue", "close"]], ["9", "12"])

    def test_green_with_nothing_open_does_nothing(self):
        gh = FakeGh()
        self.assertEqual(act(REPO, SHA, URL, "green", gh), "green, no open issue")
        self.assertEqual(gh.verbs(), ["api", "issue list"])

    def test_an_older_sha_than_main_head_does_nothing(self):
        gh = FakeGh(head="b" * 40)
        self.assertIn("is not main's head", act(REPO, SHA, URL, "red", gh))
        self.assertEqual(gh.verbs(), ["api"])

    def test_a_failed_head_lookup_or_list_does_nothing(self):
        for kw in ({"head_rc": 1}, {"list_rc": 1}):
            with self.subTest(**kw):
                gh = FakeGh(**kw)
                self.assertIn("nothing to do", act(REPO, SHA, URL, "red", gh))
                self.assertNotIn("issue create", gh.verbs())

    def test_no_verdict_makes_no_call(self):
        gh = FakeGh()
        self.assertIn("nothing to do", act(REPO, SHA, URL, "none", gh))
        self.assertEqual(gh.calls, [])


if __name__ == "__main__":
    unittest.main(verbosity=2)

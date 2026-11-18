#!/usr/bin/env python3
"""goshard.py's partition and its refusals, on synthetic lists (no `go`).

    python3 tests/goshard_test.py

The partition test is the seam: over every shard of an N-shard plan, each
listed test is run by exactly one shard and each required test is required by
exactly the shard that runs it. Each refusal case asserts its own message, so
it cannot pass because a different check rejected the same input.
"""

from __future__ import annotations

import re
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import goshard  # noqa: E402
from goshard import Refused, plan  # noqa: E402

SPLIT = "example.test/m/internal/api"
PKGS = ["example.test/m", "example.test/m/internal/db", SPLIT, "example.test/m/cmd/x"]
# Pairwise-distinct, unsorted, with names that are prefixes of each other
# (TestA / TestAB) so an unanchored -run would over-match, plus non-test
# tokens `go test -list` also prints.
NAMES = ["TestZ", "TestA", "TestAB", "ExampleE", "FuzzF", "TestM", "TestB", "TestC",
         "ok", SPLIT, "0.012s", "BenchmarkSkipped"]
TESTS = sorted(n for n in NAMES if goshard.NAME.match(n))
REQ = ["TestAB", "TestM"]


def ran(args: list[str], universe: list[str]) -> list[str]:
    """Which of `universe` a `-run` arg would select, as `go test` matches it."""
    rx = re.compile(args[args.index("-run") + 1])
    return [n for n in universe if rx.search(n)]


class Partition(unittest.TestCase):
    def test_every_test_runs_in_exactly_one_shard(self):
        for of in (2, 3, 4, 5, 9, 10):
            with self.subTest(of=of):
                seen: list[str] = []
                required: list[str] = []
                for k in range(1, of):
                    try:
                        args, vargs = plan(k, of, [], SPLIT, NAMES, REQ)
                    except Refused as e:  # more shards than tests
                        self.assertIn("got no tests", str(e))
                        self.assertGreater(of - 1, len(TESTS))
                        continue
                    self.assertEqual(args[-1], SPLIT)
                    got = ran(args, TESTS)
                    seen += got
                    required += vargs[1::2]
                    self.assertTrue(set(vargs[1::2]) <= set(got), "a shard requires a test it does not run")
                if of - 1 <= len(TESTS):
                    self.assertEqual(sorted(seen), TESTS)
                    self.assertEqual(sorted(required), sorted(REQ))

    def test_every_test_runs_in_exactly_one_process_of_one_shard(self):
        # The multi-process plan: each api shard's share dealt again over
        # `procs` processes. Over every (shard, process) pair each listed test
        # runs exactly once and each required test is required exactly once.
        for of in (2, 3, 4):
            for procs in (1, 2, 3):
                with self.subTest(of=of, procs=procs):
                    seen: list[str] = []
                    required: list[str] = []
                    if (of - 1) * procs > len(TESTS):  # more parts than tests
                        self.assertRaisesRegex(Refused, "got no tests", lambda: [
                            plan(k, of, [], SPLIT, NAMES, REQ, procs, p)
                            for k in range(1, of) for p in range(1, procs + 1)])
                        continue
                    for k in range(1, of):
                        for p in range(1, procs + 1):
                            args, vargs = plan(k, of, [], SPLIT, NAMES, REQ, procs, p)
                            got = ran(args, TESTS)
                            seen += got
                            required += vargs[1::2]
                            self.assertTrue(set(vargs[1::2]) <= set(got))
                    self.assertEqual(sorted(seen), TESTS)
                    self.assertEqual(sorted(required), sorted(REQ))

    def test_procs_one_is_the_one_process_plan(self):
        # Literal, not derived: --procs 1 deals the old way, test i to shard
        # (i mod (of-1)) + 1, over the 8 sorted names.
        want = {3: [["ExampleE", "TestA", "TestB", "TestM"], ["FuzzF", "TestAB", "TestC", "TestZ"]],
                4: [["ExampleE", "TestAB", "TestM"], ["FuzzF", "TestB", "TestZ"], ["TestA", "TestC"]]}
        for of, shards in want.items():
            for k, names in enumerate(shards, 1):
                with self.subTest(of=of, shard=k):
                    self.assertEqual(ran(plan(k, of, [], SPLIT, NAMES, [], 1, 1)[0], TESTS), names)

    def test_two_shards_of_three_processes_split_by_literal_name(self):
        # Literal expectation, not derived from plan(): 8 sorted names dealt
        # round-robin over 6 parts (shard 1 = parts 0-2, shard 2 = parts 3-5).
        self.assertEqual(TESTS, ["ExampleE", "FuzzF", "TestA", "TestAB", "TestB", "TestC", "TestM", "TestZ"])
        got = [ran(plan(k, 3, [], SPLIT, NAMES, [], 3, p)[0], TESTS)
               for k in (1, 2) for p in (1, 2, 3)]
        self.assertEqual(got, [["ExampleE", "TestM"], ["FuzzF", "TestZ"], ["TestA"],
                               ["TestAB"], ["TestB"], ["TestC"]])

    def test_shard_zero_is_every_package_but_the_split(self):
        args, vargs = plan(0, 4, PKGS, SPLIT, [], REQ)
        self.assertEqual(args, ["example.test/m", "example.test/m/internal/db", "example.test/m/cmd/x"])
        self.assertEqual(vargs, [])


class Refusals(unittest.TestCase):
    def refuses(self, msg: str, *a):
        with self.assertRaises(Refused) as cm:
            plan(*a)
        self.assertIn(msg, str(cm.exception))

    def test_one_shard(self):
        self.refuses("need at least 2 shards", 0, 1, PKGS, SPLIT, NAMES, [])

    def test_shard_out_of_range(self):
        self.refuses("is not in 0..3", 4, 4, PKGS, SPLIT, NAMES, [])

    def test_split_not_listed(self):
        self.refuses("is not one of the 3 packages", 0, 4, [p for p in PKGS if p != SPLIT], SPLIT, [], [])

    def test_no_tests_listed(self):
        self.refuses("named no tests", 1, 4, [], SPLIT, ["ok", SPLIT], [])

    def test_required_test_absent(self):
        self.refuses("REQUIRED test(s) not in", 2, 4, [], SPLIT, NAMES, REQ + ["TestGone"])

    def test_more_shards_than_tests(self):
        self.refuses("got no tests", len(TESTS) + 1, len(TESTS) + 2, [], SPLIT, NAMES, [])

    def test_process_out_of_range(self):
        self.refuses("process 4 is not in 1..3", 1, 3, [], SPLIT, NAMES, [], 3, 4)

    def test_shard_zero_is_one_process(self):
        self.refuses("shard 0 runs as one process", 0, 3, PKGS, SPLIT, [], [], 3, 2)

    def test_unsafe_token(self):
        self.refuses("would word-split or glob", 0, 2, ["example.test/m dir", SPLIT], SPLIT, [], [])


if __name__ == "__main__":
    unittest.main(verbosity=2)

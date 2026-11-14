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

    def test_unsafe_token(self):
        self.refuses("would word-split or glob", 0, 2, ["example.test/m dir", SPLIT], SPLIT, [], [])


if __name__ == "__main__":
    unittest.main(verbosity=2)

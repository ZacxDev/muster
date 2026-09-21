#!/usr/bin/env python3
"""Read `go test -json` and refuse a run that skipped, or that ran nothing.

    go test -json ./... | python3 tests/verdict.py

🔴 WHY THIS EXISTS AT ALL: `go test` EXITS 0 WITH SKIPPED TESTS. So the exit
status of a test run cannot distinguish "everything passed" from "everything
skipped", and muster's Postgres-backed tests default to skipping when they
cannot find a database. MUSTER_TEST_REQUIRE_DB=1 is supposed to make a skip
impossible; this is the independent read that says whether it did.

🔴 IT REPORTS THE PAIR, NEVER JUST THE ZERO. "0 skipped" is indistinguishable
from a parser wired to nothing, so the count of tests that produced a VERDICT is
printed beside it and a zero there is a failure. That is the positive control:
the number has to be able to move.

⚠ IT COUNTS TESTS, NOT PACKAGES. `go test -json` emits an event per package too,
with no "Test" key, and counting those would inflate every number here by the
package count — a plausible-looking total that is not what anybody means.

Exit codes:
  0  every test produced a verdict and none skipped
  1  something skipped, or nothing ran
  2  the input could not be read as `go test -json` output
"""

from __future__ import annotations

import json
import sys


def main(stream) -> int:
    verdicts: list[str] = []
    skipped: list[str] = []
    lines = 0
    parsed = 0
    for line in stream:
        line = line.strip()
        if not line:
            continue
        lines += 1
        try:
            e = json.loads(line)
        except ValueError:
            # `go test -json` interleaves build output that is not JSON. That is
            # normal and not an error — but if NOTHING parses, the input is not
            # what this script thinks it is, and that is checked below.
            continue
        if not isinstance(e, dict):
            continue
        parsed += 1
        test = e.get("Test")
        if not test:
            # A package-level event. See the note in the docstring.
            continue
        action = e.get("Action")
        name = f"{e.get('Package', '?')}.{test}"
        if action in ("pass", "fail"):
            verdicts.append(name)
        elif action == "skip":
            skipped.append(name)

    if lines == 0:
        print("verdict: COULD NOT RUN — stdin was empty. Pipe `go test -json` into this.",
              file=sys.stderr)
        return 2
    if parsed == 0:
        print(f"verdict: COULD NOT RUN — {lines} line(s) on stdin and NONE of them "
              f"parsed as JSON. This is not `go test -json` output.", file=sys.stderr)
        return 2

    print(f"tests that produced a verdict: {len(verdicts)}")
    print(f"tests that SKIPPED:            {len(skipped)}")

    if not verdicts:
        print()
        print("FAIL: zero tests produced a verdict. Whatever the exit code of the run "
              "was, it is a claim about nothing.")
        return 1

    if skipped:
        print()
        print("FAIL: tests skipped in a run that was told to require a database.")
        print("Every skip here is a test nobody ran and nobody was told about:")
        for n in sorted(skipped):
            print(f"  SKIPPED  {n}")
        print()
        print("Either MUSTER_TEST_REQUIRE_DB did not reach them — check that they take "
              "their DSN from dbtest.Base/dbtest.DSN and not from os.Getenv directly — "
              "or they skip for a reason of their own that needs stating.")
        return 1

    print()
    print("ok: every test produced a verdict, and the count above is what makes that "
          "a measurement rather than a silence")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.stdin))

#!/usr/bin/env python3
"""Plan one shard of the Go suite: which `go test` arguments, and which
`--require` names for tests/verdict.py, this CI shard runs.

    python3 tests/goshard.py --shard I --of N --split ./internal/api \\
        --args-out ARGS --verdict-out VARGS [--require NAME]... [-- GO_TEST_FLAGS...]

🔴 WHY. `go test ./...` ran in ONE job, and ONE package was that job:
internal/api took 950-1522 s of a 20-31 min job (main runs 43473563533 and
43478905382, 2026-11-13) — its 2,678 top-level tests are serial and share one
database — while every other package together summed 1,113-1,405 s spread over
the runner's cores. `-p` cannot split a package, so the job's wall time was
internal/api's. CI therefore runs N jobs, each with its own Postgres:

  shard 0      every package `go list ./...` names EXCEPT --split
  shard 1..N-1 the --split package's tests, round-robin over their sorted
               names (`go test -list`), as one anchored `-run` alternation

🔴 TOTAL BY CONSTRUCTION. Shard k takes every name whose index in the sorted
list is congruent to k-1 mod N-1, so over shards 1..N-1 every listed test runs
exactly once — provided every shard is handed the SAME N. ci.yml passes the
runner's own `strategy.job-index` / `strategy.job-total`, not matrix values, so
a matrix edit cannot leave a shard index unrun or run one twice.

🔴 A REQUIRED TEST IS REQUIRED BY EXACTLY ONE SHARD. Each `--require` name must
be in the --split package's list (a rename, deletion or build tag that drops
it fails EVERY api shard here, loudly), and is passed on to verdict.py only by
the shard that runs it. Shard 0 requires nothing.

⚠ THE ARGUMENT FILES ARE WORD-SPLIT BY THE CALLER (`$(cat FILE)`), so every
token is refused here unless it is free of whitespace and glob characters.
Test names are Go identifiers and package paths are import paths; neither
can contain one, and this is the check that says so.

Exit codes: 0 planned; 1 refused (see the message); 2 `go` failed.
"""

from __future__ import annotations

import argparse
import re
import subprocess
import sys

NAME = re.compile(r"^(Test|Example|Fuzz)\w*$")
UNSAFE = re.compile(r"[\s*?\[\]]")


class Refused(Exception):
    pass


def plan(shard: int, of: int, packages: list[str], split: str,
         names: list[str], require: list[str]) -> tuple[list[str], list[str]]:
    """The `go test` args and verdict.py args for one shard (pure; no `go`).

    `packages` is `go list ./...` (shard 0 only), `names` the split package's
    `go test -list` output (shards 1.. only); `split` is its import path."""
    if of < 2:
        raise Refused(f"--of {of}: need at least 2 shards (the rest, and one for {split})")
    if not 0 <= shard < of:
        raise Refused(f"--shard {shard} is not in 0..{of - 1}")
    if shard == 0:
        rest = [p for p in packages if p != split]
        if len(rest) != len(packages) - 1:
            raise Refused(f"{split} is not one of the {len(packages)} packages `go list ./...` "
                          f"names, so shard 0 would run it too")
        if not rest:
            raise Refused("shard 0 has no packages to run")
        args, vargs = rest, []
    else:
        tests = sorted(n for n in names if NAME.match(n))
        if not tests:
            raise Refused(f"`go test -list` named no tests in {split}: a shard of nothing is a green "
                          f"verdict about nothing")
        absent = sorted(set(require) - set(tests))
        if absent:
            raise Refused(f"REQUIRED test(s) not in {split}'s list (renamed, deleted, or filtered out "
                          f"by a build tag): {', '.join(absent)}")
        mine = tests[shard - 1::of - 1]
        if not mine:
            raise Refused(f"shard {shard} of {of} got no tests: {split} has only {len(tests)}")
        args = ["-run", "^(" + "|".join(mine) + ")$", split]
        vargs = [a for n in sorted(set(require) & set(mine)) for a in ("--require", n)]
    bad = [a for a in args + vargs if UNSAFE.search(a)]
    if bad:
        raise Refused(f"argument(s) the caller would word-split or glob: {bad}")
    return args, vargs


def go(argv: list[str]) -> list[str]:
    r = subprocess.run(["go", *argv], capture_output=True, text=True)
    if r.returncode != 0:
        sys.stderr.write(r.stdout + r.stderr)
        raise SystemExit(f"goshard: `go {' '.join(argv)}` exited {r.returncode}") from None
    return r.stdout.split()


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--shard", type=int, required=True)
    ap.add_argument("--of", type=int, required=True)
    ap.add_argument("--split", required=True, help="the package split across shards 1.., e.g. ./internal/api")
    ap.add_argument("--require", action="append", default=[])
    ap.add_argument("--args-out", required=True)
    ap.add_argument("--verdict-out", required=True)
    ap.add_argument("flags", nargs="*", help="after --: the go test flags the run uses (so -list reuses its build)")
    a = ap.parse_args()
    split = go(["list", a.split])[0]
    packages = go(["list", "./..."]) if a.shard == 0 else []
    names = go(["test", *a.flags, "-list", ".", split]) if a.shard != 0 else []
    try:
        args, vargs = plan(a.shard, a.of, packages, split, names, a.require)
    except Refused as e:
        print(f"goshard: REFUSED: {e}", file=sys.stderr)
        return 1
    with open(a.args_out, "w") as f:
        f.write("\n".join(args) + "\n")
    with open(a.verdict_out, "w") as f:
        f.write("".join(v + "\n" for v in vargs))
    if a.shard == 0:
        print(f"shard 0 of {a.of}: {len(args)} packages (every package but {split})")
    else:
        n = args[1].count("|") + 1
        print(f"shard {a.shard} of {a.of}: {n} of {split}'s {sum(1 for x in names if NAME.match(x))} "
              f"top-level tests; requires: {' '.join(vargs[1::2]) or 'none'}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

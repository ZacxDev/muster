#!/usr/bin/env python3
"""Give every file and directory git tracks an mtime derived from its CONTENT.

    python3 tests/gomtime.py            # in the checkout, right before `go test`

🔴 WHY: Go's test cache keys a test's result on the files the test OPENED, and
for a file it records only (size, mode, mtime) — never the bytes
(cmd/go/internal/test: hashOpen/hashWriteStat). actions/checkout stamps every
file with the moment of the checkout, so a test that reads a repository file
(testdata, ci.yml, a doc it lints) can never be answered from a restored cache:
every run is a new mtime.

The fix is NOT a constant mtime. With a constant, a same-size edit would keep
(size, mode, mtime) and the cache would replay a result for bytes the test
never saw. Here the mtime is a function of the git object id (blob for a file,
tree for a directory): same content, same mtime; changed content, a different
mtime (48 bits of the object id, as nanoseconds after 2001-09-09, so always far
older than the cache's 2 s too-new cutoff). That keeps Go's own guarantee —
re-run whenever an input changed — and makes it reachable from a fresh checkout.

A tracked file that differs from HEAD (an earlier step rewrote it) is left
alone: its fresh mtime means "re-run", which is the safe answer. Untracked
files are never touched. Symlinks and submodules are skipped.

Exit codes: 0 done; 1 `git` failed.
"""

from __future__ import annotations

import os
import subprocess
import sys

BASE_NS = 1_000_000_000 * 1_000_000_000  # 2001-09-09T01:46:40Z, in ns


def mtime_ns(object_id: str) -> int:
    """A fixed past instant plus 48 bits of the object id, in nanoseconds."""
    return BASE_NS + int(object_id[:12], 16)


def git(*args: str) -> bytes:
    return subprocess.run(["git", *args], check=True, capture_output=True).stdout


def entries(ls_tree: bytes) -> list[tuple[str, str, str]]:
    """(mode, object id, path) from `git ls-tree -z` output."""
    out = []
    for rec in ls_tree.split(b"\0"):
        if not rec:
            continue
        meta, path = rec.split(b"\t", 1)
        mode, _kind, oid = meta.decode().split(" ")
        out.append((mode, oid, os.fsdecode(path)))
    return out


def main() -> int:
    try:
        root = git("rev-parse", "--show-toplevel").decode().strip()
        dirty = {os.fsdecode(p) for p in git("diff", "--name-only", "-z", "HEAD").split(b"\0") if p}
        tree = entries(git("-C", root, "ls-tree", "-r", "-t", "-z", "HEAD"))
        top = git("-C", root, "rev-parse", "HEAD^{tree}").decode().strip()
    except subprocess.CalledProcessError as e:
        sys.stderr.write((e.stderr or b"").decode())
        print(f"gomtime: `git {' '.join(e.cmd[1:])}` exited {e.returncode}", file=sys.stderr)
        return 1
    files = dirs = 0
    # Files first, then directories deepest-first, so nothing set here is
    # disturbed by a later step of this same loop.
    for mode, oid, path in tree:
        if mode in ("100644", "100755") and path not in dirty:
            ns = mtime_ns(oid)
            try:
                os.utime(os.path.join(root, path), ns=(ns, ns), follow_symlinks=False)
                files += 1
            except FileNotFoundError:
                pass
    for mode, oid, path in sorted((e for e in tree if e[0] == "040000"), key=lambda e: -e[2].count("/")):
        ns = mtime_ns(oid)
        os.utime(os.path.join(root, path), ns=(ns, ns))
        dirs += 1
    ns = mtime_ns(top)
    os.utime(root, ns=(ns, ns))
    print(f"gomtime: content-addressed mtimes on {files} files and {dirs + 1} directories "
          f"({len(dirty)} tracked file(s) differ from HEAD and keep a fresh mtime)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

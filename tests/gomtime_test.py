#!/usr/bin/env python3
"""gomtime.py on a throwaway git repository.

    python3 tests/gomtime_test.py

The property Go's test cache needs: a same-SIZE edit must still move the
mtime (Go keys a file on size+mode+mtime only), equal content must give an
equal mtime across checkouts, and a file that differs from HEAD keeps the
fresh mtime that means "re-run".
"""

from __future__ import annotations

import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import gomtime  # noqa: E402


def sh(cwd: Path, *args: str) -> None:
    subprocess.run(args, cwd=cwd, check=True, capture_output=True)


def checkout(tmp: Path, name: str, content: str) -> Path:
    d = tmp / name
    (d / "pkg").mkdir(parents=True)
    (d / "pkg" / "data.txt").write_text(content)
    sh(d, "git", "init", "-q")
    sh(d, "git", "add", "pkg/data.txt")
    sh(d, "git", "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "x")
    return d


def run(d: Path) -> None:
    subprocess.run([sys.executable, str(HERE / "gomtime.py")], cwd=d, check=True, capture_output=True)


def mt(p: Path) -> int:
    return os.stat(p).st_mtime_ns


class GoMtime(unittest.TestCase):
    def test_same_content_same_mtime_and_same_size_edit_moves_it(self) -> None:
        with tempfile.TemporaryDirectory() as t:
            tmp = Path(t)
            a, b, c = checkout(tmp, "a", "AAAA\n"), checkout(tmp, "b", "AAAA\n"), checkout(tmp, "c", "BBBB\n")
            for d in (a, b, c):
                run(d)
            f = lambda d: d / "pkg" / "data.txt"  # noqa: E731
            self.assertEqual(mt(f(a)), mt(f(b)), "equal content must give equal mtimes")
            self.assertEqual(os.stat(f(a)).st_size, os.stat(f(c)).st_size)
            self.assertNotEqual(mt(f(a)), mt(f(c)), "a same-size edit must move the mtime")
            self.assertEqual(mt(a / "pkg"), mt(b / "pkg"), "equal trees must give equal directory mtimes")
            self.assertNotEqual(mt(a / "pkg"), mt(c / "pkg"))
            self.assertLess(mt(f(a)), 1_100_000_000 * 10**9, "the mtime must be far in the past")

    def test_a_file_that_differs_from_head_keeps_its_fresh_mtime(self) -> None:
        with tempfile.TemporaryDirectory() as t:
            d = checkout(Path(t), "d", "AAAA\n")
            (d / "pkg" / "data.txt").write_text("CCCC\n")
            before = mt(d / "pkg" / "data.txt")
            run(d)
            self.assertEqual(mt(d / "pkg" / "data.txt"), before)

    def test_mtime_ns_is_the_object_id(self) -> None:
        self.assertEqual(gomtime.mtime_ns("000000000001ffff"), gomtime.BASE_NS + 1)
        self.assertNotEqual(gomtime.mtime_ns("aaaaaaaaaaaa"), gomtime.mtime_ns("aaaaaaaaaaab"))


if __name__ == "__main__":
    unittest.main()

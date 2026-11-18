#!/usr/bin/env python3
"""Repository properties of the release:

  * every asset docs/releases.md lists is one scripts/release-build.sh builds,
    and every asset it builds is listed (its `expected` array is the contract
    the script verifies dist/ against, so docs = expected = dist);
  * no muster-mirror build for Windows anywhere (it needs tmux);
  * every `uses:` in .github/workflows/*.yml is pinned to a full commit SHA
    (a tag or branch can be moved to other code after review), with the
    version it was resolved from in a trailing comment.

    python3 tests/release_platforms_test.py
"""

from __future__ import annotations

import re
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
BUILD = ROOT / "scripts" / "release-build.sh"
DOC = ROOT / "docs" / "releases.md"
WORKFLOWS = sorted((ROOT / ".github" / "workflows").glob("*.y*ml"))


def built_assets(text: str) -> set[str]:
    """The literal `expected=( ... )` array of release-build.sh, version as <version>."""
    m = re.search(r"^expected=\(\n(.*?)^\)$", text, re.S | re.M)
    if not m:
        raise AssertionError("release-build.sh has no literal expected=( ... ) array")
    words = m.group(1).replace('"', "").split()
    return {w.replace("$VERSION", "<version>") for w in words}


def table_rows(text: str, header_start: str) -> list[list[str]]:
    lines = text.splitlines()
    for i, l in enumerate(lines):
        if l.startswith(header_start):
            rows = []
            for r in lines[i + 2:]:
                if not r.startswith("|"):
                    break
                rows.append([c.strip() for c in r.strip().strip("|").split("|")])
            return rows
    raise AssertionError(f"docs/releases.md has no table starting {header_start!r}")


def documented_assets(text: str) -> set[str]:
    out: set[str] = set()
    header = None
    for l in text.splitlines():
        if l.startswith("| program |"):
            header = [c.strip() for c in l.strip().strip("|").split("|")]
            break
    if header is None:
        raise AssertionError("docs/releases.md has no `| program | ...` platform table")
    platforms = [h.split() for h in header[1:]]
    for row in table_rows(text, "| program |"):
        prog = row[0].strip("`")
        for (os_, arch), cell in zip(platforms, row[1:]):
            if cell in ("—", "-", ""):
                continue
            for fmt in (f.strip() for f in cell.split(",")):
                out.add(f"{prog}_{os_}_{arch}.{fmt}")
    for row in table_rows(text, "| asset |"):
        out.add(row[0].strip("`"))
    return out


class Platforms(unittest.TestCase):
    def test_docs_and_build_list_the_same_assets(self):
        built = built_assets(BUILD.read_text())
        documented = documented_assets(DOC.read_text())
        self.assertEqual(sorted(documented - built), [], "documented in docs/releases.md but not built by release-build.sh")
        self.assertEqual(sorted(built - documented), [], "built by release-build.sh but not in docs/releases.md")

    def test_the_matrix_has_what_was_decided(self):
        built = built_assets(BUILD.read_text())
        for a in ("muster_windows_amd64.zip", "muster_windows_arm64.zip",
                  "muster_linux_amd64.deb", "muster-mirror_linux_arm64.rpm",
                  "muster.rb", "muster-mirror.rb", "muster.json"):
            self.assertIn(a, built)
        self.assertEqual([a for a in built if a.startswith("muster-mirror_windows")], [],
                         "muster-mirror needs tmux: no Windows build")

    def test_parser_sees_a_difference(self):
        # CONTROL: the comparison above can go red. A row's cell dropped from
        # the doc must show up as built-but-undocumented.
        doc = DOC.read_text().replace("| tar.gz | tar.gz | zip | zip |", "| tar.gz | tar.gz | zip | — |", 1)
        self.assertIn("muster_windows_arm64.zip", built_assets(BUILD.read_text()) - documented_assets(doc))


PIN_RE = re.compile(r"^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+(/[^@\s]+)?@[0-9a-f]{40}$")
USES_RE = re.compile(r"^\s*(?:-\s*)?uses:\s*(['\"]?)([^\s'\"#]+)\1\s*(#.*)?$")


def unpinned(text: str) -> list[str]:
    bad = []
    for n, line in enumerate(text.splitlines(), 1):
        if not re.match(r"^\s*(-\s*)?uses:", line):
            continue
        m = USES_RE.match(line)
        if not m:
            bad.append(f"{n}: unparsable: {line.strip()}")
            continue
        ref, comment = m.group(2), (m.group(3) or "")
        if ref.startswith("./"):
            continue
        if ref.startswith("docker://"):
            if "@sha256:" not in ref:
                bad.append(f"{n}: {ref} (a docker action must be pinned by digest)")
            continue
        if not PIN_RE.match(ref):
            bad.append(f"{n}: {ref} (not a 40-hex commit)")
        elif not re.match(r"#\s*v\d", comment):
            bad.append(f"{n}: {ref} (no `# vX.Y.Z` comment naming the version it pins)")
    return bad


class ActionsArePinned(unittest.TestCase):
    def test_every_action_is_sha_pinned(self):
        self.assertTrue(WORKFLOWS, "no workflow files found")
        seen = 0
        for wf in WORKFLOWS:
            text = wf.read_text()
            seen += len(re.findall(r"^\s*(-\s*)?uses:", text, re.M))
            with self.subTest(workflow=wf.name):
                self.assertEqual(unpinned(text), [])
        self.assertGreater(seen, 20, "the scan found almost no `uses:` lines; is it reading the workflows?")

    def test_checker_refuses_what_it_must(self):
        sha = "0123456789abcdef0123456789abcdef01234567"
        self.assertEqual(unpinned(f"      - uses: actions/checkout@{sha} # v6.0.1\n"), [])
        self.assertEqual(unpinned(f"        uses: actions/cache/restore@{sha} # v4.2.0\n"), [])
        self.assertEqual(unpinned("      - uses: ./.github/actions/local\n"), [])
        for line in ("      - uses: actions/checkout@v6\n",
                     "      - uses: actions/checkout@main\n",
                     f"      - uses: actions/checkout@{sha[:12]} # v6\n",
                     f"      - uses: actions/checkout@{sha}\n",
                     "      - uses: docker://alpine:3.20\n",
                     "        uses: 'docker/login-action@v3'\n"):
            with self.subTest(line=line.strip()):
                self.assertNotEqual(unpinned(line), [])


if __name__ == "__main__":
    unittest.main(verbosity=1)

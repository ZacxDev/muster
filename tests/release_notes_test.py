#!/usr/bin/env python3
"""scripts/release-notes.py: the grouping, the title-prefix fallback, empty
sections, ordering, and the exact text of the notes and the changelog, on
fixture pull-request lists; and `collect` against a scratch git repository
with a fake `gh` (no network).

    python3 tests/release_notes_test.py

The rendered outputs are pinned as WHOLE literal strings: a test that only
looked for a heading would pass a reordered or duplicated section.
"""

from __future__ import annotations

import importlib.util
import json
import os
import subprocess
import sys
import tempfile
import textwrap
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SCRIPT = ROOT / "scripts" / "release-notes.py"
spec = importlib.util.spec_from_file_location("release_notes", SCRIPT)
rn = importlib.util.module_from_spec(spec)
assert spec.loader is not None
spec.loader.exec_module(rn)

REPO = "Example/proj"


def pr(n: int, title: str, *labels: str) -> dict:
    return {"number": n, "title": title, "url": f"https://github.com/{REPO}/pull/{n}", "labels": list(labels)}


class Grouping(unittest.TestCase):
    def test_labels_decide_first(self):
        self.assertEqual(rn.group_of(pr(1, "officers: x", "release:fix")), "Fixes")
        # a label beats a contradicting title prefix
        self.assertEqual(rn.group_of(pr(2, "feat: x", "release:docs")), "Docs")
        # GitHub's default labels
        self.assertEqual(rn.group_of(pr(3, "x", "bug")), "Fixes")
        self.assertEqual(rn.group_of(pr(4, "x", "enhancement")), "Features")
        self.assertEqual(rn.group_of(pr(5, "x", "documentation")), "Docs")
        # case-insensitive
        self.assertEqual(rn.group_of(pr(6, "x", "Release:Security")), "Security")

    def test_several_labels_take_the_first_group_in_order(self):
        self.assertEqual(rn.group_of(pr(1, "x", "release:feature", "release:security")), "Security")
        self.assertEqual(rn.group_of(pr(2, "x", "release:internal", "release:fix")), "Fixes")

    def test_unknown_labels_fall_through_to_the_title(self):
        self.assertEqual(rn.group_of(pr(1, "fix: y", "good first issue")), "Fixes")

    def test_title_prefix_fallback(self):
        cases = {
            "feat: a": "Features",
            "feat(ntfy): a": "Features",
            "fix(ui)!: a": "Fixes",
            "harden: a": "Security",
            "security(auth): a": "Security",
            "docs(managed-network): a": "Docs",
            "ci: a": "Internal",
            "chore: a": "Internal",
            "e2e: a": "Internal",
            "test: a": "Internal",
            "deploy: compose default 1 -> 2": "Internal",
            "Feat: capitalised": "Features",
            "officers: a scope, not a type": "Other changes",
            "Design: a scope, not a type": "Other changes",
            "teams, phase 3: a": "Other changes",
            "no prefix at all": "Other changes",
            "feat no colon": "Other changes",
        }
        for title, want in cases.items():
            with self.subTest(title=title):
                self.assertEqual(rn.group_of(pr(1, title)), want)

    def test_skip_label_leaves_it_out(self):
        self.assertIsNone(rn.group_of(pr(1, "feat: a", "skip-changelog")))
        self.assertIsNone(rn.group_of(pr(2, "fix: a", "release:skip", "release:fix")))
        g = rn.grouped([pr(1, "feat: a", "release:skip"), pr(2, "feat: b")])
        self.assertEqual([p["number"] for p in g["Features"]], [2])

    def test_ordering_is_by_number_whatever_the_input_order(self):
        g = rn.grouped([pr(30, "fix: c"), pr(4, "fix: a"), pr(17, "fix: b")])
        self.assertEqual([p["number"] for p in g["Fixes"]], [4, 17, 30])


class Titles(unittest.TestCase):
    def test_display_title(self):
        self.assertEqual(rn.display_title("feat(ntfy): urgent template (#1135)"), "**ntfy:** urgent template")
        self.assertEqual(rn.display_title("fix: a crash"), "a crash")
        self.assertEqual(rn.display_title("officers: price it (the alias) (#798)"), "officers: price it (the alias)")
        # an unknown type keeps its prefix: it is the reader's only context
        self.assertEqual(rn.display_title("compose: default to v1"), "compose: default to v1")


NOTES_FIXTURE = {
    "repo": REPO,
    "releases": [{
        "tag": "v1.3.0",
        "prev": "v1.2.0",
        "prs": [
            pr(12, "fix(mirror): reconnect after a restart (#12)"),
            pr(10, "feat: Windows CLI builds", "release:highlight"),
            pr(11, "harden: refuse a symlinked token file"),
            pr(14, "officers: a sidebar group"),
            pr(13, "ci: pin every action by SHA"),
            pr(15, "docs: release verification", "release:skip"),
        ],
    }],
}

NOTES_WANT = textwrap.dedent("""\
    ## Highlights

    1 security change, 1 feature, 1 fix, 1 internal change and 1 other change since v1.2.0.

    **Highlights:**

    - Windows CLI builds ([#10](https://github.com/Example/proj/pull/10))

    ## Security

    - refuse a symlinked token file ([#11](https://github.com/Example/proj/pull/11))

    ## Features

    - Windows CLI builds ([#10](https://github.com/Example/proj/pull/10))

    ## Fixes

    - **mirror:** reconnect after a restart ([#12](https://github.com/Example/proj/pull/12))

    ## Internal

    - pin every action by SHA ([#13](https://github.com/Example/proj/pull/13))

    ## Other changes

    - officers: a sidebar group ([#14](https://github.com/Example/proj/pull/14))

    **Full changelog:** https://github.com/Example/proj/compare/v1.2.0...v1.3.0

    ## Install

    the install block
    """)


class Notes(unittest.TestCase):
    def test_whole_notes_literal(self):
        self.assertEqual(rn.render_notes(NOTES_FIXTURE, "## Install\n\nthe install block\n"), NOTES_WANT)

    def test_empty_sections_are_omitted(self):
        out = rn.render_notes({"repo": REPO, "releases": [{"tag": "v1.0.1", "prev": "v1.0.0", "prs": [pr(3, "fix: a")]}]}, "")
        self.assertEqual(out, textwrap.dedent("""\
            ## Highlights

            1 fix since v1.0.0.

            ## Fixes

            - a ([#3](https://github.com/Example/proj/pull/3))

            **Full changelog:** https://github.com/Example/proj/compare/v1.0.0...v1.0.1
            """))

    def test_no_pull_requests(self):
        out = rn.render_notes({"repo": REPO, "releases": [{"tag": "v1.0.1", "prev": "v1.0.0", "prs": []}]}, "## Install\n")
        self.assertEqual(out, "## Highlights\n\nNo pull requests were merged since v1.0.0.\n\n"
                              "**Full changelog:** https://github.com/Example/proj/compare/v1.0.0...v1.0.1\n\n## Install\n")

    def test_only_skipped_pull_requests_read_as_none(self):
        out = rn.render_notes({"repo": REPO, "releases": [{"tag": "v1.0.1", "prev": "v1.0.0",
                                                           "prs": [pr(3, "fix: a", "skip-changelog")]}]}, "")
        self.assertIn("No pull requests were merged since v1.0.0.", out)
        self.assertNotIn("#3", out)

    def test_first_release(self):
        out = rn.render_notes({"repo": REPO, "releases": [{"tag": "v0.1.0", "prev": None, "prs": []}]}, "")
        self.assertEqual(out, "## Highlights\n\nThe first release tagged in this repository.\n")

    def test_unusable_input_is_refused(self):
        with self.assertRaises(rn.Unusable):
            rn.render_notes({"releases": [{"tag": "v1.2", "prev": "v1.1.0", "prs": []}]}, "")
        with self.assertRaises(rn.Unusable):
            rn.render_notes({"releases": [{"tag": "v1.2.0", "prev": "v1.1.0", "prs": [{"number": 1}]}]}, "")


CHANGELOG_WANT = rn.CHANGELOG_HEAD.rstrip() + "\n" + textwrap.dedent("""\

    ## v0.10.0

    1 feature since v0.9.0.

    ### Features

    - ten ([#20](https://github.com/Example/proj/pull/20))

    **Full changelog:** https://github.com/Example/proj/compare/v0.9.0...v0.10.0

    ## v0.9.0

    No pull requests were merged since v0.8.0.

    **Full changelog:** https://github.com/Example/proj/compare/v0.8.0...v0.9.0

    ## v0.8.0

    The first release tagged in this repository.
    """)


class Changelog(unittest.TestCase):
    def test_whole_changelog_literal_newest_first_by_semver(self):
        # input order scrambled; v0.10.0 must sort ABOVE v0.9.0 (numeric, not text)
        data = {"repo": REPO, "releases": [
            {"tag": "v0.9.0", "prev": "v0.8.0", "prs": []},
            {"tag": "v0.8.0", "prev": None, "prs": []},
            {"tag": "v0.10.0", "prev": "v0.9.0", "prs": [pr(20, "feat: ten")]},
        ]}
        self.assertEqual(rn.render_changelog(data), CHANGELOG_WANT)

    def test_same_data_same_groups_as_the_notes(self):
        rel = NOTES_FIXTURE["releases"][0]
        notes = rn.render_notes(NOTES_FIXTURE, "")
        log = rn.render_changelog(NOTES_FIXTURE)
        for p in rel["prs"]:
            self.assertEqual(f"#{p['number']}]" in notes, f"#{p['number']}]" in log, p)

    def test_empty_is_refused(self):
        with self.assertRaises(rn.Unusable):
            rn.render_changelog({"releases": []})


class RepositoryChangelog(unittest.TestCase):
    """CHANGELOG.md in the repository is this script's output, not hand-edited
    prose: every release heading is a vX.Y.Z tag, newest first, and it starts
    with the generated header."""

    def test_checked_in_changelog_has_the_generated_shape(self):
        text = (ROOT / "CHANGELOG.md").read_text(encoding="utf-8")
        self.assertTrue(text.startswith(rn.CHANGELOG_HEAD.rstrip() + "\n"), "CHANGELOG.md does not start with the generated header")
        tags = [l[3:] for l in text.splitlines() if l.startswith("## ")]
        self.assertGreater(len(tags), 0)
        keys = []
        for t in tags:
            m = rn.SEMVER_TAG_RE.match(t)
            self.assertIsNotNone(m, f"CHANGELOG.md heading {t!r} is not a vX.Y.Z tag")
            keys.append(tuple(int(x) for x in m.groups()))
        self.assertEqual(keys, sorted(keys, reverse=True), "CHANGELOG.md is not newest first")
        self.assertEqual(len(keys), len(set(keys)), "a release is listed twice")

    def test_checked_in_changelog_lists_the_release_the_compose_file_defaults_to(self):
        # The version source: deploy/docker-compose.yml's default, which the
        # release flow's bump pull request moves to each new release — the
        # same pull request that runs scripts/changelog-bump.sh.
        cur = subprocess.run([str(ROOT / "scripts" / "stamp-compose-version.sh"), "--current"],
                             stdin=open(ROOT / "deploy" / "docker-compose.yml"), check=True,
                             capture_output=True, text=True).stdout.strip()
        self.assertRegex(cur, r"^\d+\.\d+\.\d+$")
        headings = [l for l in (ROOT / "CHANGELOG.md").read_text(encoding="utf-8").splitlines() if l.startswith("## ")]
        self.assertIn(f"## v{cur}", headings,
                      f"CHANGELOG.md has no v{cur}, the release deploy/docker-compose.yml defaults to: run scripts/changelog-bump.sh")


class Collect(unittest.TestCase):
    """`collect` on a scratch repository: which tag is the previous one, and
    which commits are pull requests."""

    def git(self, *a: str) -> str:
        return subprocess.run(["git", "-C", self.dir, *a], check=True, capture_output=True, text=True).stdout

    def commit(self, subject: str) -> None:
        self.git("commit", "-q", "--allow-empty", "-m", subject)

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.dir = os.path.join(self.tmp.name, "repo")
        os.mkdir(self.dir)
        env = {"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.invalid",
               "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.invalid"}
        os.environ.update(env)
        self.git("init", "-q", "-b", "main")
        self.commit("initial")
        self.git("tag", "v1.0.0")
        self.commit("feat: one (#1)")
        self.git("tag", "v1.1.0-rc1")          # not strict semver: never a previous tag
        self.commit("pushed without a pull request")
        self.commit("Merge pull request #2 from someone/branch")
        self.commit("fix(x): three (#3)")
        self.git("tag", "v1.1.0")
        # a fake gh: `gh pr list ...` prints the merged pull requests
        bindir = os.path.join(self.tmp.name, "bin")
        os.mkdir(bindir)
        listed = [
            {"number": 1, "title": "feat: one", "url": "u1", "labels": [{"name": "release:highlight"}]},
            {"number": 2, "title": "docs: two", "url": "u2", "labels": []},
            {"number": 3, "title": "fix(x): three", "url": "u3", "labels": []},
        ]
        with open(os.path.join(bindir, "gh"), "w") as f:
            f.write("#!/bin/sh\necho \"$*\" >> \"$GH_LOG\"\ncat <<'J'\n" + json.dumps(listed) + "\nJ\n")
        os.chmod(os.path.join(bindir, "gh"), 0o755)
        self.env = dict(os.environ, PATH=bindir + os.pathsep + os.environ["PATH"],
                        GH_LOG=os.path.join(self.tmp.name, "gh.log"))

    def tearDown(self):
        self.tmp.cleanup()

    def collect(self, *args: str) -> dict:
        out = subprocess.run([sys.executable, str(SCRIPT), "collect", "--repo", REPO, "--git-dir", self.dir, *args],
                             env=self.env, check=True, capture_output=True, text=True).stdout
        return json.loads(out)

    def test_one_tag(self):
        d = self.collect("--tag", "v1.1.0")
        rel = d["releases"][0]
        self.assertEqual(rel["tag"], "v1.1.0")
        self.assertEqual(rel["prev"], "v1.0.0", "the rc tag is not strict semver and must be skipped")
        self.assertEqual([p["number"] for p in rel["prs"]], [3, 2, 1], "git log order, both squash and merge subjects")
        self.assertEqual(rel["prs"][2]["labels"], ["release:highlight"])
        self.assertEqual(rel["prs"][1]["title"], "docs: two", "the pull request's title, not the commit subject")

    def test_all_tags(self):
        d = self.collect("--all")
        self.assertEqual([(r["tag"], r["prev"]) for r in d["releases"]], [("v1.0.0", None), ("v1.1.0", "v1.0.0")])

    def test_unknown_tag_fails(self):
        r = subprocess.run([sys.executable, str(SCRIPT), "collect", "--repo", REPO, "--git-dir", self.dir, "--tag", "v9.9.9"],
                           env=self.env, capture_output=True, text=True)
        self.assertEqual(r.returncode, 1)

    def test_a_pull_request_gh_does_not_know_fails(self):
        self.commit("feat: four (#4)")
        self.git("tag", "v1.2.0")
        r = subprocess.run([sys.executable, str(SCRIPT), "collect", "--repo", REPO, "--git-dir", self.dir, "--tag", "v1.2.0"],
                           env=self.env, capture_output=True, text=True)
        self.assertEqual(r.returncode, 1)
        self.assertIn("#4", r.stderr)


class InstallBlock(unittest.TestCase):
    """scripts/release-install-block.sh: the block under the notes."""

    BLOCK = ROOT / "scripts" / "release-install-block.sh"
    DL = "https://github.com/Example/Proj/releases/download/v1.2.3"
    CLANKMAIL = ("\nClankMail plugin: the muster image no longer ships `/muster-clankmail` (earlier releases did). "
                 "Use its own image, `ghcr.io/example/muster-clankmail` "
                 "(https://github.com/ZacxDev/muster-clankmail; docs/clankmail.md).\n")
    ATTEST = ("Every asset is in `checksums.txt`. This release's assets, the programs inside its archives and "
              "packages, and its `ghcr.io/example/muster:1.2.3` image have signed build-provenance attestations "
              "from the workflow that built them. (The `muster-stt-cohere` and `claude-code-agent` images are "
              "attested only by the run that first pushed them, so one carried over from an earlier release may "
              "have none.)\n")

    def block(self, tag="v1.2.3", tap="no-tap") -> str:
        return subprocess.run([str(self.BLOCK), tag, "Example/Proj", tap], check=True, capture_output=True, text=True).stdout

    def test_block_names_this_release_and_the_verify_commands(self):
        out = self.block()
        self.assertTrue(out.startswith("## Install\n"))
        for want in (f"`curl -fsSL {self.DL}/install.sh | sh`",
                     f"scoop install {self.DL}/muster.json",
                     "muster_windows_amd64.zip",
                     # GHCR names are lowercase whatever the owner's case
                     "`ghcr.io/example/muster:1.2.3`",
                     "\n## Verify\n",
                     "sha256sum --check --ignore-missing checksums.txt\n",
                     "gh attestation verify muster_linux_amd64.tar.gz --repo Example/Proj\n",
                     "gh attestation verify oci://ghcr.io/example/muster:1.2.3 --repo Example/Proj\n"):
            self.assertIn(want, out)
        self.assertIn(f"helm install muster {self.DL}/muster-1.2.3.tgz", out)

    def test_attestation_sentence_names_only_what_this_release_attests(self):
        out = self.block()
        self.assertIn(self.ATTEST, out)
        self.assertNotIn("every image", out)

    def test_homebrew_tap_line_only_with_tap(self):
        tap, no = self.block(tap="tap"), self.block(tap="no-tap")
        self.assertIn("- **Homebrew:** `brew install example/muster/muster example/muster/muster-mirror` "
                      "(the tap example/homebrew-muster carries this release's formulas).\n", tap)
        self.assertNotIn("brew install", no)
        self.assertIn("- **Homebrew:** this release's `muster.rb` and `muster-mirror.rb`, installed from a local tap "
                      "(no published tap carries them yet: [docs/releases.md]"
                      "(https://github.com/Example/Proj/blob/v1.2.3/docs/releases.md#homebrew-and-scoop)).\n", no)

    def test_clankmail_notice_below_0_250_0_only(self):
        for tag in ("v0.248.0", "v0.249.9", "v0.9.0"):
            with self.subTest(tag=tag):
                self.assertIn(self.CLANKMAIL, self.block(tag=tag))
        for tag in ("v0.250.0", "v0.250.1", "v1.0.0"):
            with self.subTest(tag=tag):
                self.assertNotIn("ClankMail", self.block(tag=tag))

    def test_block_refuses_bad_arguments(self):
        for args in (["1.2.3", "Example/Proj", "tap"], ["v1.2.3", "no-slash", "tap"], ["v1.2.3", "Example/Proj"],
                     ["v1.2.3", "Example/Proj", "maybe"]):
            with self.subTest(args=args):
                r = subprocess.run([str(self.BLOCK), *args], capture_output=True, text=True)
                self.assertEqual(r.returncode, 2)


class CLI(unittest.TestCase):
    def test_notes_cli_end_to_end(self):
        with tempfile.TemporaryDirectory() as t:
            data, inst = os.path.join(t, "d.json"), os.path.join(t, "i.md")
            Path(data).write_text(json.dumps(NOTES_FIXTURE))
            Path(inst).write_text("## Install\n\nthe install block\n")
            out = subprocess.run([sys.executable, str(SCRIPT), "notes", "--data", data, "--install", inst],
                                 check=True, capture_output=True, text=True).stdout
            self.assertEqual(out, NOTES_WANT)

    def test_unusable_data_exits_2(self):
        with tempfile.TemporaryDirectory() as t:
            data = os.path.join(t, "d.json")
            Path(data).write_text("[1, 2]")
            r = subprocess.run([sys.executable, str(SCRIPT), "notes", "--data", data], capture_output=True, text=True)
            self.assertEqual(r.returncode, 2)


if __name__ == "__main__":
    unittest.main(verbosity=1)

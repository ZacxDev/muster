#!/usr/bin/env python3
"""The onboarding docs stay one source per fact: the guides, their index and
docs/features.md agree.

    python3 tests/docs_guides_test.py

Static. What it pins:

* THE GUIDES AND THEIR INDEX. The set of docs/guides/*.md files is exactly the
  set the table under docs/README.md's "Set it up" heading links, in both
  directions: a guide nobody can find, and a row for a guide that is gone, both
  fail. (That each link resolves is tests/features_config_test.py's job.)
* EACH GUIDE'S SHAPE. Its `##` headings are exactly GUIDE_PARTS, in order: what
  it adds, what it needs, how to turn it on, how to check it, how to turn it
  off. The operator's ask, held as structure rather than as words in a body.
* NO OWNED LITERAL IN A GUIDE. A guide links docs/configuration.md (or the code)
  for a port or a model id; it never spells one. The whole file is read, code
  fences, inline code and URLs included; only a relative link target (the
  `(path#anchor)` of a link that is not http/https) is skipped. Ports: any 4-
  or 5-digit number not joined to a letter, digit, `-` or `_` on either side,
  nor to a `.` before it or a `.` and digit after it (so `8105.` at the end of
  a sentence, `localhost:8105` and `--bg 8105` all count; `1.2345` and
  `step-8105`, `2026.11` do not). Model ids: every backticked value in a
  configuration.md row whose variable names a MODEL (read from that page, so
  a new default is covered without editing this file), anywhere in the file;
  and any backticked `owner/name` token that is not a path in this
  repository.
* FEATURE PAGES ARE IN docs/features.md. Every page docs/README.md lists under
  "Using it" is linked from docs/features.md, the page that says what every
  feature needs, except the pages in NOT_A_FEATURE, each with its reason. The
  exemptions are a ledger: one that names a page not on that list, or a page
  docs/features.md now links, fails, so the list cannot only grow.
* EVERY SECTION HAS A CONTENTS ROW. Every `###` heading of docs/features.md is
  a `(#anchor)` link in its Contents table: a section added without a row
  fails. (A row whose anchor is not a heading is a broken link, which
  tests/features_config_test.py's link check fails; the feature registry's
  ids being headings at all is cmd/muster-server's
  TestEveryRegistryIDIsAFeaturesHeading, which reads them from the registry.)

Each check also runs, through the same function, on planted input it must
fail, so a check that cannot fail is reported rather than passed.
"""

from __future__ import annotations

import re
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from features_config_test import slug  # noqa: E402  (one slug rule, not two)

ROOT = Path(__file__).resolve().parent.parent
DOCS = ROOT / "docs"
GUIDES = DOCS / "guides"
INDEX = DOCS / "README.md"
FEATURES = DOCS / "features.md"
CONFIG_MD = DOCS / "configuration.md"

INDEX_SECTION = "Set it up: the core first, then one feature at a time"
GUIDE_PARTS = ["What it adds", "What it needs", "Turn it on", "Check it", "Turn it off"]

# Pages listed under "Using it" in docs/README.md that are not a feature, so
# docs/features.md need not link them. Each with its reason.
NOT_A_FEATURE = {
    "onboarding.md": "how to install muster with one prompt; features.md starts from an install",
    "glossary.md": "the words the documents use, not a feature",
}


def section(md: str, title: str, level: int = 2) -> str:
    """The text under the heading `title` at `level`, up to the next heading of
    that level or higher (code fences ignored for the end)."""
    lines, out, inside, fenced = md.splitlines(), [], False, False
    for line in lines:
        if line.lstrip().startswith("```"):
            fenced = not fenced
        m = None if fenced else re.match(r"^(#{1,6})\s+(.*?)\s*$", line)
        if m and inside and len(m.group(1)) <= level:
            break
        if m and len(m.group(1)) == level and m.group(2) == title:
            inside = True
            continue
        if inside:
            out.append(line)
    return "\n".join(out)


def strip_code_fences(md: str) -> str:
    out, fenced = [], False
    for line in md.splitlines():
        if line.lstrip().startswith("```"):
            fenced = not fenced
            continue
        if not fenced:
            out.append(line)
    return "\n".join(out)


REL_LINK_TARGET = re.compile(r"\]\((?!https?://)[^)]*\)")
PORTLIKE = re.compile(r"(?<![\w.-])\d{4,5}(?![\w-]|\.\d)")
BACKTICK = re.compile(r"`([^`\n]+)`")
OWNER_NAME = re.compile(r"^[A-Za-z][A-Za-z0-9_.-]*/[A-Za-z0-9_.:-]+$")


def model_defaults() -> set[str]:
    """Every backticked value in a configuration.md table row whose variable's
    name contains MODEL: the model ids that page owns."""
    out = set()
    for line in CONFIG_MD.read_text().splitlines():
        m = re.match(r"^\|\s*`([A-Z0-9_]+)`\s*\|(.*)$", line)
        if m and "MODEL" in m.group(1):
            out |= {v for v in BACKTICK.findall(m.group(2)) if OWNER_NAME.match(v) or re.match(r"^[\w.-]+-\d", v)}
    return out


def owned_literals(text: str, models: set[str]) -> list[str]:
    """What a guide must not spell: ports, the configuration page's model ids,
    and backticked owner/name tokens that are not a path here. Everything but
    a relative link target is read, code fences and URLs included."""
    prose = REL_LINK_TARGET.sub("]", text)
    found = [f"port-like number {n}" for n in PORTLIKE.findall(prose)]
    found += [f"model id {m}" for m in sorted(models) if m in prose]
    for tok in BACKTICK.findall(prose):
        if OWNER_NAME.match(tok) and tok not in models and not (ROOT / tok).exists() and not (DOCS / tok).exists():
            found.append(f"owner/name token {tok} (not a path in this repository)")
    return found


def guide_files() -> set[str]:
    return {p.name for p in GUIDES.glob("*.md")}


def index_guides(md: str) -> set[str]:
    return set(re.findall(r"\]\(guides/([\w.-]+\.md)(?:#[^)]*)?\)", section(md, INDEX_SECTION)))


def guide_parts(md: str) -> list[str]:
    return re.findall(r"^##\s+(.*?)\s*$", strip_code_fences(md), re.M)


def misshapen_guides(guides: dict[str, str]) -> dict[str, list[str]]:
    """Each guide whose `##` headings are not exactly GUIDE_PARTS, with them."""
    return {name: parts for name, md in sorted(guides.items()) if (parts := guide_parts(md)) != GUIDE_PARTS}


def using_it_pages(md: str) -> list[str]:
    block = md.split("**Using it**", 1)[1].split("**Running it**", 1)[0]
    return re.findall(r"^- \[[^\]]*\]\(([\w./-]+\.md)\)", block, re.M)


def features_links(md: str) -> set[str]:
    return set(re.findall(r"\]\(([\w./-]+\.md)(?:#[^)]*)?\)", strip_code_fences(md)))


def unlinked_feature_pages(index_md: str, features_md: str) -> list[str]:
    """Pages the index lists under Using it that features.md never links,
    NOT_A_FEATURE aside."""
    linked = features_links(features_md)
    return [p for p in using_it_pages(index_md) if p not in linked and p not in NOT_A_FEATURE]


def contents_anchors(md: str) -> set[str]:
    table = section(md, "Contents")
    return set(re.findall(r"\]\(#([a-z0-9-]+)\)", table))


def h3_slugs(md: str) -> set[str]:
    return {slug(h) for h in re.findall(r"^###\s+(.*?)\s*$", strip_code_fences(md), re.M)}


class Guides(unittest.TestCase):
    def test_the_guides_and_the_index_table_agree(self) -> None:
        index = INDEX.read_text()
        files, linked = guide_files(), index_guides(index)
        self.assertGreaterEqual(len(files), 5, "positive control: the guides directory was read")
        self.assertGreaterEqual(len(linked), 5, f"positive control: the {INDEX_SECTION!r} table was read")
        self.assertEqual(sorted(files - linked), [], "guides docs/README.md's table does not link")
        self.assertEqual(sorted(linked - files), [], "rows in docs/README.md's table for guides that do not exist")
        # Negative controls, through index_guides: a row removed from the
        # table is gone from what it reads, and a guide linked from OUTSIDE
        # the table's section is not read as a row.
        some = sorted(linked)[0]
        cut = "\n".join(l for l in index.splitlines() if f"](guides/{some})" not in l)
        self.assertNotIn(some, index_guides(cut), "control: a removed row was still read")
        outside = index + "\n## Elsewhere\n\n[x](guides/planted.md)\n"
        self.assertNotIn("planted.md", index_guides(outside), "control: a link outside the table was read as a row")
        inside = index.replace(f"](guides/{some})", f"](guides/{some}) [p](guides/planted.md)", 1)
        self.assertIn("planted.md", index_guides(inside), "control: a planted row was not read")

    def test_every_guide_has_the_five_parts_in_order(self) -> None:
        guides = {p.name: p.read_text() for p in GUIDES.glob("*.md")}
        self.assertEqual(misshapen_guides(guides), {}, f"each guide's ## headings must be exactly {GUIDE_PARTS}")
        # Negative controls, through misshapen_guides: two parts swapped, one
        # missing, and an extra one each fail; a `##` inside a code fence does
        # not count as a part.
        good = "\n".join(f"## {p}\n\ntext\n" for p in GUIDE_PARTS)
        swapped = good.replace("## Check it", "## TMP").replace("## Turn it on", "## Check it").replace("## TMP", "## Turn it on")
        planted = {
            "good.md": good,
            "swapped.md": swapped,
            "missing.md": good.replace("## Turn it off\n", ""),
            "extra.md": good + "\n## Troubleshooting\n",
            "fenced.md": good + "\n```sh\n## not a heading\n```\n",
        }
        self.assertEqual(sorted(misshapen_guides(planted)), ["extra.md", "missing.md", "swapped.md"])

    def test_no_guide_spells_a_port_or_model_id(self) -> None:
        models = model_defaults()
        self.assertGreaterEqual(len(models), 3, f"positive control: model ids read from configuration.md: {models}")
        bad = {p.name: f for p in sorted(GUIDES.glob("*.md")) if (f := owned_literals(p.read_text(), models))}
        self.assertEqual(bad, {}, "a guide spells a literal that configuration.md or the code owns: link instead")
        # Negative controls: each kind, planted, is seen, in each place a
        # guide could put it; a relative link target, a repository path and
        # a version number are not.
        some_model = sorted(models)[0]
        planted = f"Listen on 8105, model `{some_model}`, or `Example-Org/new-model-2`."
        self.assertEqual(len(owned_literals(planted, models)), 3, owned_literals(planted, models))
        for case in [
            "The port is 8105.",
            "Open `http://localhost:8105`.",
            "Open <http://localhost:8105> or [it](http://localhost:8105).",
            "```sh\ntailscale serve --bg 8105\n```",
            "```sh\nMUSTER_SUGGEST_MODEL=openai/gpt-6-luna\n```",
        ]:
            self.assertNotEqual(owned_literals(case, models), [], f"control: {case!r} went unseen")
        self.assertEqual(owned_literals("[x](../a.md#step-8105) `deploy/docker-compose.yml` v1.2345 release 2026.11", models), [])


class FeaturesPage(unittest.TestCase):
    def test_every_feature_page_in_the_index_is_linked_from_features_md(self) -> None:
        index, features = INDEX.read_text(), FEATURES.read_text()
        pages, linked = using_it_pages(index), features_links(features)
        self.assertGreaterEqual(len(pages), 20, "positive control: docs/README.md's Using it list was read")
        self.assertIn("dictation.md", linked, "positive control: docs/features.md's links were read")
        missing = unlinked_feature_pages(index, features)
        self.assertEqual(missing, [], "pages docs/README.md lists under Using it that docs/features.md never links")
        # Negative controls, through unlinked_feature_pages: a page planted in
        # Using it, and a link cut from features.md, are each seen; a link in
        # a code fence does not count as one.
        planted = index.replace("**Using it**\n", "**Using it**\n\n- [planted.md](planted.md) — x\n", 1)
        self.assertEqual(unlinked_feature_pages(planted, features), ["planted.md"], "control: a planted page went unseen")
        cut = re.sub(r"\]\(dictation\.md(#[^)]*)?\)", "]", features)
        self.assertEqual(unlinked_feature_pages(index, cut), ["dictation.md"], "control: a cut link went unseen")
        fenced = f"{cut}\n```\n[d](dictation.md)\n```\n"
        self.assertEqual(unlinked_feature_pages(index, fenced), ["dictation.md"], "control: a fenced link counted")
        # The ledger, both ways: every exemption is a listed page, and still needed.
        self.assertEqual(sorted(set(NOT_A_FEATURE) - set(pages)), [], "NOT_A_FEATURE names a page not in Using it")
        self.assertEqual(sorted(set(NOT_A_FEATURE) & linked), [], "NOT_A_FEATURE names a page features.md now links")

    def test_every_section_has_a_contents_row(self) -> None:
        md = FEATURES.read_text()
        table, sections = contents_anchors(md), h3_slugs(md)
        self.assertGreaterEqual(len(table), 30, "positive control: the Contents table's anchors were read")
        self.assertIn("dictation", sections, "positive control: the ### headings were read")
        self.assertEqual(sorted(sections - table), [], "### sections with no row in the Contents table")
        # Negative control: a section added without a row is seen.
        self.assertIn("planted", h3_slugs(md + "\n### Planted\n") - table)


if __name__ == "__main__":
    unittest.main(verbosity=2)

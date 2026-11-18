#!/usr/bin/env python3
"""The repository's front door: the community files, the README and the docs
index, and internal notes kept out of the public tree.

    python3 tests/repo_front_door_test.py

Static. What it pins:

* COMMUNITY FILES. Every file in COMMUNITY_FILES exists and is not empty.
* ISSUE TEMPLATES PARSE. Every .github/ISSUE_TEMPLATE/*.yml is a YAML mapping,
  read by load_yaml_subset below (stdlib only: the dev shell's python has no
  PyYAML). It reads the block-YAML subset the templates use and REFUSES
  anything outside it, so it can reject valid YAML but never accept a file
  GitHub could not parse: a template that needs more YAML than it reads
  fails here, and the fix is to widen the reader, with a control.
  config.yml's `blank_issues_enabled` is a boolean. Every other template is an
  issue form: a name, a description and a non-empty `body` list.
* ONE PRIVATE CHANNEL FOR SECURITY AND CONDUCT REPORTS: this repository's own
  private vulnerability report form. Its URL is DERIVED from go.mod's module
  path (github.com/<owner>/<repo> -> https://github.com/<owner>/<repo>/
  security/advisories/new), never spelled here, so a fork or a rename that
  leaves the docs pointing at another repository's form fails. config.yml's
  FIRST contact link has exactly that URL (parsed: https, github.com, that
  path, no query), and SECURITY.md and CODE_OF_CONDUCT.md each carry a
  Markdown link whose TARGET is that URL (the link text is not read).
  Whether the form is switched on is a repository setting no static test can
  see; `gh api repos/<owner>/<repo>/private-vulnerability-reporting` reads it.
* NO INTERNAL NOTES. No tracked path is under claudedocs/, no tracked file
  names a note that moved out (MOVED_NOTES), and no Markdown link points into
  claudedocs/. (`claudedocs/**` also appears in the officers' docs-only gate,
  its docs and tests, as an example of ANOTHER repository's paths; those are
  globs in prose and code, never links, so they are not references to this
  tree.)
* LINKS AND IMAGES RESOLVE in README.md and docs/README.md: every relative
  Markdown link, anchor and image (tests/features_config_test.py's
  broken_links, the one link rule), plus every `<a href="path">`, which that
  rule does not read (the feature grid links its cards with one).
* THE README's BADGES: CI status, latest release, licence and Go version.
* THE INSTALL TABLE IS WHAT A RELEASE SHIPS. Every release asset the README's
  "Install options" section names (with `<os>`, `<arch>` and `<X.Y.Z>`
  expanded) is in scripts/release-build.sh's literal asset contract, and
  every asset of that contract except the two that are not an install option
  (NOT_AN_INSTALL_OPTION) is named there. A seam, so it fails when either
  side moves: a platform the README offers that no release builds, or an
  asset a release builds that the README does not offer.

Each check also runs, through the same function, on planted input it must
fail, so a check that cannot fail is reported rather than passed.
"""

from __future__ import annotations

import itertools
import json
import re
import subprocess
import sys
import unittest
from pathlib import Path
from urllib.parse import urlsplit

sys.path.insert(0, str(Path(__file__).resolve().parent))
from features_config_test import MD_LINK, broken_links, strip_code  # noqa: E402  (one link rule, not two)

ROOT = Path(__file__).resolve().parent.parent
README = "README.md"
DOCS_INDEX = "docs/README.md"
TEMPLATES = ROOT / ".github/ISSUE_TEMPLATE"
RELEASE_BUILD = ROOT / "scripts/release-build.sh"



def repo_slug(gomod: str) -> str:
    """<owner>/<repo> from go.mod's `module github.com/<owner>/<repo>`."""
    m = re.search(r"(?m)^module github\.com/([\w.-]+/[\w.-]+)\s*$", gomod)
    assert m, "go.mod's module path is not github.com/<owner>/<repo>"
    return m.group(1)


ADVISORY_URL = f"https://github.com/{repo_slug((ROOT / 'go.mod').read_text())}/security/advisories/new"

COMMUNITY_FILES = [
    "README.md",
    "LICENSE",
    "CONTRIBUTING.md",
    "CODE_OF_CONDUCT.md",
    "SECURITY.md",
    ".github/pull_request_template.md",
    ".github/ISSUE_TEMPLATE/config.yml",
    ".github/ISSUE_TEMPLATE/bug_report.yml",
    ".github/ISSUE_TEMPLATE/feature_request.yml",
]

# Internal handoff notes that moved to the maintainers' private notes. A file
# that names one now points at nothing a reader of this repository can open.
MOVED_NOTES = ["handoff-chief-routing-gaps"]

# Assets every release builds that are not a way to install muster.
NOT_AN_INSTALL_OPTION = {
    "INSTALL.md": "the guide the one prompt downloads; the quick start names it",
    "checksums.txt": "the sha256 of the other assets",
}

BADGES = {
    "CI status": "https://github.com/ZacxDev/muster/actions/workflows/ci.yml/badge.svg",
    "latest release": "https://img.shields.io/github/v/release/ZacxDev/muster",
    "licence": "https://img.shields.io/github/license/ZacxDev/muster",
    "Go version": "https://img.shields.io/github/go-mod/go-version/ZacxDev/muster",
}

PLACEHOLDERS = {"<os>": ["linux", "darwin"], "<arch>": ["amd64", "arm64"]}
ASSET_TOKEN = re.compile(r"`([\w<>.-]+\.(?:tar\.gz|tgz|yml|sh)|env\.example)`")
A_HREF = re.compile(r"<a[^>]*\shref=\"([^\"]+)\"")
CLAUDEDOCS_LINK = re.compile(r"\]\((?:\./|(?:\.\./)+)?claudedocs(?:/|\))")


def tracked() -> list[str]:
    out = subprocess.run(["git", "-C", str(ROOT), "ls-files", "-z"], capture_output=True, text=True, check=True).stdout
    return [p for p in out.split("\0") if p]


def text_of(path: str) -> str | None:
    try:
        return (ROOT / path).read_text()
    except (UnicodeDecodeError, IsADirectoryError, FileNotFoundError):
        return None


def missing_community_files(root: Path) -> list[str]:
    return [f for f in COMMUNITY_FILES if not (root / f).is_file() or not (root / f).read_text().strip()]


class YAMLSubsetError(ValueError):
    pass


_KEY = re.compile(r"^([A-Za-z_][\w-]*):(?: (.*))?$")
_PLAIN_BAD_START = tuple("[]{},&*!|>'\"%@`#?:-")


def _scalar(v: str, where: str):
    v = v.strip()
    if v.startswith('"'):
        try:
            out = json.loads(v)
        except ValueError as e:
            raise YAMLSubsetError(f"{where}: bad double-quoted string {v!r}: {e}") from None
        if not isinstance(out, str):
            raise YAMLSubsetError(f"{where}: {v!r} is not a string")
        return out
    if v.startswith("'"):
        if len(v) < 2 or not v.endswith("'") or "'" in v[1:-1].replace("''", ""):
            raise YAMLSubsetError(f"{where}: bad single-quoted string {v!r}")
        return v[1:-1].replace("''", "'")
    if v.startswith("["):
        try:
            out = json.loads(v)  # a flow sequence, in its JSON spelling only
        except ValueError as e:
            raise YAMLSubsetError(f"{where}: bad flow sequence {v!r}: {e}") from None
        if not isinstance(out, list):
            raise YAMLSubsetError(f"{where}: {v!r} is not a sequence")
        return out
    if not v or v.startswith(_PLAIN_BAD_START) or ": " in v or " #" in v or v.endswith(":"):
        raise YAMLSubsetError(f"{where}: {v!r} is outside the plain-scalar subset (quote it)")
    return {"true": True, "false": False, "null": None, "~": None}.get(v, v)


def load_yaml_subset(text: str):
    """Block YAML, the subset the issue templates use: mappings, `- `
    sequences (a mapping may start on the dash's line), plain, double- or
    single-quoted scalars, JSON-spelled flow sequences and `|` literal
    blocks; full-line comments and blank lines. Anything else, a tab in the
    indent, a duplicate key or an indent it did not expect raises
    YAMLSubsetError."""
    lines = []
    for n, raw in enumerate(text.split("\n"), 1):
        body = raw.rstrip()
        indent = len(body) - len(body.lstrip(" "))
        if body[indent:indent + 1] == "\t":
            raise YAMLSubsetError(f"line {n}: a tab in the indent")
        lines.append((n, indent, body[indent:], raw))
    pos = 0

    def skip():
        nonlocal pos
        while pos < len(lines) and (not lines[pos][2] or lines[pos][2].startswith("#")):
            pos += 1

    def literal(parent: int):
        nonlocal pos
        block = []
        while pos < len(lines) and (not lines[pos][2] or lines[pos][1] > parent):
            block.append(lines[pos][3])
            pos += 1
        while block and not block[-1].strip():
            block.pop()
        if not block:
            raise YAMLSubsetError(f"line {lines[pos - 1][0] if pos else 0}: an empty | block")
        cut = min(len(b) - len(b.lstrip(" ")) for b in block if b.strip())
        return "\n".join(b[cut:] for b in block) + "\n"

    def node(indent: int, first: str | None = None):
        nonlocal pos
        skip()
        if first is None and (pos >= len(lines) or lines[pos][1] != indent):
            raise YAMLSubsetError(f"line {lines[pos][0] if pos < len(lines) else 'EOF'}: expected a block at indent {indent}")
        head = first if first is not None else lines[pos][2]
        if head == "-" or head.startswith("- "):
            return seq(indent)
        return mapping(indent, first)

    def seq(indent: int):
        nonlocal pos
        out = []
        while True:
            skip()
            if pos >= len(lines) or lines[pos][1] != indent or not (lines[pos][2] == "-" or lines[pos][2].startswith("- ")):
                break
            n, _, content, _ = lines[pos]
            rest = content[1:].strip()
            if not rest:
                pos += 1
                out.append(node(indent + 2))
            elif _KEY.match(rest):
                out.append(mapping(indent + 2, rest))
            else:
                pos += 1
                out.append(_scalar(rest, f"line {n}"))
        return out

    def mapping(indent: int, first: str | None = None):
        nonlocal pos
        out = {}
        while True:
            if first is None:
                skip()
                if pos >= len(lines) or lines[pos][1] < indent:
                    break
                if lines[pos][1] > indent:
                    raise YAMLSubsetError(f"line {lines[pos][0]}: unexpected indent")
                content = lines[pos][2]
            else:
                content, first = first, None
            n = lines[pos][0]
            m = _KEY.match(content)
            if not m:
                if content == "-" or content.startswith("- "):
                    break
                raise YAMLSubsetError(f"line {n}: {content!r} is not `key: value`")
            key, value = m.group(1), (m.group(2) or "").strip()
            if key in out:
                raise YAMLSubsetError(f"line {n}: duplicate key {key!r}")
            pos += 1
            if value == "|":
                out[key] = literal(indent)
            elif value:
                out[key] = _scalar(value, f"line {n}")
            else:
                skip()
                if pos < len(lines) and lines[pos][1] > indent:
                    out[key] = node(lines[pos][1])
                elif pos < len(lines) and lines[pos][1] == indent and (lines[pos][2] == "-" or lines[pos][2].startswith("- ")):
                    out[key] = seq(indent)
                else:
                    out[key] = None
        return out

    skip()
    if pos >= len(lines):
        raise YAMLSubsetError("an empty document")
    doc = node(0)
    skip()
    if pos < len(lines):
        raise YAMLSubsetError(f"line {lines[pos][0]}: content after the document")
    return doc


def template_problems(files: dict[str, str]) -> list[str]:
    """What is wrong with the issue templates FILES ({name: YAML text})."""
    errs = []
    if "config.yml" not in files:
        errs.append("config.yml is missing")
    for name, text in sorted(files.items()):
        try:
            doc = load_yaml_subset(text)
        except YAMLSubsetError as e:
            errs.append(f"{name} does not parse as YAML: {e}")
            continue
        if not isinstance(doc, dict):
            errs.append(f"{name} is not a YAML mapping")
            continue
        if name == "config.yml":
            if not isinstance(doc.get("blank_issues_enabled"), bool):
                errs.append("config.yml: blank_issues_enabled is not a boolean")
        else:
            for key in ("name", "description"):
                if not isinstance(doc.get(key), str) or not doc[key].strip():
                    errs.append(f"{name}: no {key}")
            if not isinstance(doc.get("body"), list) or not doc["body"]:
                errs.append(f"{name}: no body (an issue form needs at least one field)")
    return errs


def security_route_problems(config: dict, docs: dict[str, str], advisory: str = ADVISORY_URL) -> list[str]:
    """Where the private report channel is not ADVISORY: config.yml's first
    contact link, and a Markdown link target in each of DOCS ({name: text})."""
    errs = []
    want = urlsplit(advisory)
    links = config.get("contact_links") if isinstance(config, dict) else None
    first = links[0] if isinstance(links, list) and links and isinstance(links[0], dict) else {}
    got = urlsplit(str(first.get("url", "")))
    if (got.scheme, got.netloc, got.path.rstrip("/"), got.query, got.fragment) != (
            "https", want.netloc, want.path, "", ""):
        errs.append(f"config.yml: the first contact link is {first.get('url')!r}, not this repository's "
                    f"private report form {advisory}")
    for name, text in sorted(docs.items()):
        targets = {m.group(1).strip("<>") for m in MD_LINK.finditer(strip_code(text))}
        if advisory not in targets:
            errs.append(f"{name}: no link targets this repository's private report form {advisory}")
    return errs


def internal_note_refs(files: dict[str, str | None]) -> list[str]:
    """Tracked paths under claudedocs/, and files that name a moved note or link into claudedocs/."""
    errs = [f"{p} is tracked under claudedocs/" for p in files if p.startswith("claudedocs/")]
    for path, text in sorted(files.items()):
        if text is None or path == "tests/repo_front_door_test.py":
            continue
        errs += [f"{path} names the moved note {n}" for n in MOVED_NOTES if n in text]
        if path.endswith(".md") and CLAUDEDOCS_LINK.search(strip_code(text)):
            errs.append(f"{path} links into claudedocs/")
    return errs


def broken_html_links(files: dict[str, str]) -> list[str]:
    """`<a href>` links whose relative target file is missing (broken_links
    reads Markdown links and images, not HTML anchors)."""
    errs = []
    for path, text in files.items():
        for target in A_HREF.findall(strip_code(text)):
            if re.match(r"^[a-z][a-z0-9+.-]*:", target) or target.startswith("//"):
                continue
            rel = (Path(path).parent / target.split("#", 1)[0]).as_posix()
            if not (ROOT / rel).is_file():
                errs.append(f"{path}: link {target} -> {rel} does not exist")
    return errs


def missing_badges(readme: str) -> list[str]:
    return [name for name, url in BADGES.items() if url not in readme]


def release_contract() -> set[str]:
    """scripts/release-build.sh's `expected=( … )`, with $VERSION as <X.Y.Z>."""
    m = re.search(r"(?ms)^expected=\((.*?)^\)", RELEASE_BUILD.read_text())
    assert m, "scripts/release-build.sh has no expected=( … ) asset contract"
    return {t.strip('"').replace("$VERSION", "<X.Y.Z>") for t in m.group(1).split()}


def install_section(readme: str) -> str:
    m = re.search(r"(?ms)^## Install options\n(.*?)(?=^## |\Z)", readme)
    return m.group(1) if m else ""


def expand(token: str) -> set[str]:
    names = [k for k in PLACEHOLDERS if k in token]
    out = set()
    for combo in itertools.product(*(PLACEHOLDERS[k] for k in names)):
        t = token
        for k, v in zip(names, combo):
            t = t.replace(k, v)
        out.add(t)
    return out


def install_table_drift(section: str, contract: set[str]) -> list[str]:
    named = set().union(*(expand(t) for t in ASSET_TOKEN.findall(section))) if section else set()
    errs = [f"the README offers {a}, which no release builds" for a in sorted(named - contract)]
    errs += [f"a release builds {a}, which the README's install options do not name"
             for a in sorted(contract - named - set(NOT_AN_INSTALL_OPTION))]
    if not section:
        errs.insert(0, "README.md has no '## Install options' section")
    return errs


class FrontDoor(unittest.TestCase):
    def test_community_files_exist(self) -> None:
        self.assertEqual(missing_community_files(ROOT), [])
        # Negative control: a root without them reports every one.
        self.assertEqual(missing_community_files(ROOT / "no-such-dir"), COMMUNITY_FILES)

    def test_issue_templates_parse(self) -> None:
        files = {p.name: p.read_text() for p in TEMPLATES.glob("*.yml")} if TEMPLATES.is_dir() else {}
        self.assertGreaterEqual(len(files), 3, "positive control: found almost no issue templates")
        self.assertEqual(template_problems(files), [])
        # Negative controls: bad YAML, a string where a boolean belongs and a
        # form with no fields are each reported.
        planted = dict(files)
        planted["config.yml"] = "blank_issues_enabled: 'no'\ncontact_links: []\n"
        planted["broken.yml"] = "name: [unclosed\n"
        planted["empty.yml"] = "name: x\ndescription: y\nbody: []\n"
        errs = template_problems(planted)
        self.assertEqual([e for e in errs if not e.startswith("broken.yml")], [
            "config.yml: blank_issues_enabled is not a boolean",
            "empty.yml: no body (an issue form needs at least one field)",
        ])
        self.assertEqual(len([e for e in errs if e.startswith("broken.yml does not parse as YAML")]), 1, errs)

    def test_the_yaml_subset_reader(self) -> None:
        # Positive control: every construct the templates use, read to an
        # exact, literal structure (never derived from the reader itself).
        doc = ("# c\nname: Bug report\nlabels: [\"bug\"]\nok: false\nbody:\n  - type: markdown\n"
               "    attributes:\n      value: |\n        Line one.\n\n        Line two.\n"
               "  - type: input\n    id: v\n    attributes:\n      description: \"`x --y`, z.\"\n"
               "      q: 'it''s'\nlinks:\n- url: https://example.com/a\n  about: plain text, here.\n")
        self.assertEqual(load_yaml_subset(doc), {
            "name": "Bug report", "labels": ["bug"], "ok": False,
            "body": [{"type": "markdown", "attributes": {"value": "Line one.\n\nLine two.\n"}},
                     {"type": "input", "id": "v", "attributes": {"description": "`x --y`, z.", "q": "it's"}}],
            "links": [{"url": "https://example.com/a", "about": "plain text, here."}],
        })
        # Negative controls: each is refused, never read as something else.
        for bad in ("name: [unclosed\n", "a: b: c\n", "a: `x`\n", "a: 1\na: 2\n", "a:\n  b: 1\n    c: 2\n",
                    "a:\n\tb: 1\n", "a: \"open\n", "a: 'open\n", "just text\n", "a: x # note\n", "a: |\n", ""):
            with self.assertRaises(YAMLSubsetError, msg=repr(bad)):
                load_yaml_subset(bad)

    def test_security_and_conduct_reports_go_to_the_private_form(self) -> None:
        self.assertEqual(repo_slug("module github.com/Some-Org/a.b\n\ngo 1.0\n"), "Some-Org/a.b")
        self.assertEqual(urlsplit(ADVISORY_URL).path.count("/"), 5, f"positive control: {ADVISORY_URL}")
        config = load_yaml_subset((TEMPLATES / "config.yml").read_text())
        docs = {n: (ROOT / n).read_text() for n in ("SECURITY.md", "CODE_OF_CONDUCT.md")}
        self.assertEqual(security_route_problems(config, docs), [])
        # Negative controls. Another repository's form (what a fork that kept
        # these files would show), the form's URL only as link TEXT, the form
        # moved to second place, and a query string are each reported.
        other = ADVISORY_URL.replace(urlsplit(ADVISORY_URL).path.split("/")[1], "someone-else", 1)
        self.assertEqual(security_route_problems(config, docs, other), [
            f"config.yml: the first contact link is {ADVISORY_URL!r}, not this repository's private report form {other}",
            f"CODE_OF_CONDUCT.md: no link targets this repository's private report form {other}",
            f"SECURITY.md: no link targets this repository's private report form {other}",
        ])
        text_only = {"SECURITY.md": f"Report it at [{ADVISORY_URL}](https://github.com/)."}
        self.assertEqual(security_route_problems(config, text_only), [
            f"SECURITY.md: no link targets this repository's private report form {ADVISORY_URL}"])
        swapped = {"contact_links": list(reversed(config["contact_links"]))}
        self.assertEqual(len(security_route_problems(swapped, {})), 1)
        queried = {"contact_links": [{"url": ADVISORY_URL + "?x=1"}]}
        self.assertEqual(len(security_route_problems(queried, {})), 1)
        self.assertEqual(security_route_problems({"contact_links": [{"url": ADVISORY_URL + "/"}]}, {}), [],
                         "control: a trailing slash is the same page")

    def test_no_internal_notes_in_the_tree(self) -> None:
        files = {p: text_of(p) for p in tracked()}
        self.assertGreater(len(files), 1000, "positive control: git ls-files found almost nothing")
        self.assertEqual(internal_note_refs(files), [])
        # Negative controls: a tracked note, a file naming a moved note, and a
        # link into claudedocs/ are each reported; a glob in prose is not.
        planted = {
            "claudedocs/x.md": "# x\n",
            "docs/a.md": f"see {MOVED_NOTES[0]}.md\n",
            "docs/b.md": "[notes](../claudedocs/plan.md)\n",
            "docs/c.md": "allowed paths: `claudedocs/**`, claudedocs/notes.sh\n",
        }
        self.assertEqual(internal_note_refs(planted), [
            "claudedocs/x.md is tracked under claudedocs/",
            f"docs/a.md names the moved note {MOVED_NOTES[0]}",
            "docs/b.md links into claudedocs/",
        ])

    def test_readme_and_docs_index_links_and_images_resolve(self) -> None:
        files = {p: (ROOT / p).read_text() for p in (README, DOCS_INDEX)}
        links = sum(len(re.findall(r"\]\((?!https?://)", strip_code(t))) for t in files.values())
        self.assertGreater(links, 40, "positive control: the link pattern found almost nothing")
        self.assertEqual(broken_links(files), [])
        self.assertEqual(broken_html_links(files), [])
        # Negative controls: a missing page, a missing anchor, a missing
        # image (both spellings) and a missing <a href> page are each
        # reported, in either file; a fenced one is not.
        planted = {README: files[README] + "\n[a](docs/nope.md) [b](docs/README.md#nowhere) "
                                           "<img src=\"docs/img/nope.png\"> ![c](docs/img/nope2.png) "
                                           "<a href=\"docs/nope3.md\">x</a>\n"
                                           "```\n[d](fenced-nope.md)\n```\n",
                   DOCS_INDEX: files[DOCS_INDEX] + "\n[e](nope.md)\n"}
        self.assertEqual(sorted(broken_links(planted)), sorted([
            "README.md: docs/nope.md -> docs/nope.md does not exist",
            "README.md: docs/README.md#nowhere -> #nowhere is not a heading of docs/README.md",
            "README.md: docs/img/nope.png -> docs/img/nope.png does not exist",
            "README.md: docs/img/nope2.png -> docs/img/nope2.png does not exist",
            "docs/README.md: nope.md -> docs/nope.md does not exist",
        ]))
        self.assertEqual(broken_html_links(planted), [
            "README.md: link docs/nope3.md -> docs/nope3.md does not exist",
        ])

    def test_readme_badges(self) -> None:
        readme = (ROOT / README).read_text()
        self.assertEqual(missing_badges(readme), [])
        self.assertEqual(missing_badges(""), list(BADGES), "control: an empty README went unseen")

    def test_install_options_are_what_a_release_ships(self) -> None:
        contract = release_contract()
        self.assertIn("muster-<X.Y.Z>.tgz", contract, "positive control: the contract parse found no chart")
        self.assertGreaterEqual(len(contract), 14, "positive control: the contract parse found almost nothing")
        section = install_section((ROOT / README).read_text())
        self.assertEqual(install_table_drift(section, contract), [])
        # Negative controls: a platform no release builds, and an asset the
        # section drops, are each reported.
        self.assertEqual(install_table_drift(section.replace("`muster-<X.Y.Z>.tgz`", "") +
                                             "\n`muster_<os>_<arch>.zip.tar.gz`\n", contract), [
            "the README offers muster_darwin_amd64.zip.tar.gz, which no release builds",
            "the README offers muster_darwin_arm64.zip.tar.gz, which no release builds",
            "the README offers muster_linux_amd64.zip.tar.gz, which no release builds",
            "the README offers muster_linux_arm64.zip.tar.gz, which no release builds",
            "a release builds muster-<X.Y.Z>.tgz, which the README's install options do not name",
        ])


if __name__ == "__main__":
    unittest.main(verbosity=2)

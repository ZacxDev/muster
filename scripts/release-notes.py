#!/usr/bin/env python3
"""A muster release's notes and the repository's CHANGELOG.md, from ONE data set:
the pull requests merged between release tags.

    # what the `release` job runs (needs git history with tags, and `gh`):
    scripts/release-notes.py collect --repo ZacxDev/muster --tag v1.2.3 > data.json
    scripts/release-notes.py notes --data data.json --install install.md > notes.md

    # CHANGELOG.md, every release tag (`make changelog`):
    scripts/release-notes.py collect --repo ZacxDev/muster --all > all.json
    scripts/release-notes.py changelog --data all.json > CHANGELOG.md

`collect` is the only part that reads git or GitHub; `notes` and `changelog`
are pure functions of its JSON, which is what tests/release_notes_test.py
pins with fixture pull-request lists.

WHICH PULL REQUESTS: the first-parent commits in `<previous tag>..<tag>` whose
subject ends in `(#N)` (a squash merge) or starts `Merge pull request #N`. The
previous tag is the newest strict-semver tag (vX.Y.Z) BELOW this one that is
an ancestor of it. The oldest tag has no previous one: it is listed as the
first tagged release, with no pull-request list. A commit pushed without a pull
request is not listed.

GROUPS, in this order, each omitted when empty:
    Security, Features, Fixes, Docs, Internal, Other changes
A pull request's group comes from its LABELS first (the first group, in the
order above, that one of its labels names; LABEL_GROUPS below), else from its
TITLE's conventional-commit type (`feat:`, `fix(scope):`, `harden!:`;
PREFIX_GROUPS below), else it is "Other changes". The label `skip-changelog`
leaves a pull request out entirely; `highlight` also lists it under
Highlights. Within a group: by pull-request number, ascending.

Each line links its pull request. A conventional type is dropped from the
line (the group says it) and a scope is kept in bold: `feat(ntfy): x` reads
`**ntfy:** x`.

Exit 0 ok; 1 a git/gh lookup failed; 2 usage or unusable input.
"""

from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys

GROUPS = ["Security", "Features", "Fixes", "Docs", "Internal", "Other changes"]

# Label name (lowercase) -> group. GitHub's default labels are included so a
# pull request labelled with them sorts sensibly before the release:* set exists.
LABEL_GROUPS = {
    "release:security": "Security",
    "security": "Security",
    "release:feature": "Features",
    "enhancement": "Features",
    "release:fix": "Fixes",
    "bug": "Fixes",
    "release:docs": "Docs",
    "documentation": "Docs",
    "release:internal": "Internal",
}
SKIP_LABELS = {"release:skip", "skip-changelog"}
HIGHLIGHT_LABELS = {"release:highlight", "highlight"}

# Conventional-commit type (lowercase) -> group.
PREFIX_GROUPS = {
    "security": "Security",
    "sec": "Security",
    "harden": "Security",
    "feat": "Features",
    "feature": "Features",
    "fix": "Fixes",
    "bugfix": "Fixes",
    "hotfix": "Fixes",
    "docs": "Docs",
    "doc": "Docs",
    "ci": "Internal",
    "chore": "Internal",
    "build": "Internal",
    "test": "Internal",
    "tests": "Internal",
    "e2e": "Internal",
    "refactor": "Internal",
    "deps": "Internal",
    "style": "Internal",
    "deploy": "Internal",
}

PREFIX_RE = re.compile(r"^(?P<type>[A-Za-z][A-Za-z0-9-]*)(?:\((?P<scope>[^()]*)\))?!?:\s*(?P<rest>.+)$")
SQUASH_RE = re.compile(r"\(#(\d+)\)\s*$")
MERGE_RE = re.compile(r"^Merge pull request #(\d+)\b")
SEMVER_TAG_RE = re.compile(r"^v(\d+)\.(\d+)\.(\d+)$")


class Unusable(Exception):
    """Input that cannot be turned into notes; exit 2."""


# --- the pure part ------------------------------------------------------------

def labels_of(pr: dict) -> list[str]:
    return [str(l).lower() for l in pr.get("labels", [])]


def group_of(pr: dict) -> str | None:
    """The pull request's group, or None when it is left out (skip label)."""
    labels = labels_of(pr)
    if SKIP_LABELS & set(labels):
        return None
    from_labels = {LABEL_GROUPS[l] for l in labels if l in LABEL_GROUPS}
    for g in GROUPS:
        if g in from_labels:
            return g
    m = PREFIX_RE.match(pr["title"].strip())
    if m and m.group("type").lower() in PREFIX_GROUPS:
        return PREFIX_GROUPS[m.group("type").lower()]
    return "Other changes"


def display_title(title: str) -> str:
    """The title as a line shows it: no trailing (#N), no conventional type."""
    t = SQUASH_RE.sub("", title).strip()
    m = PREFIX_RE.match(t)
    if m and m.group("type").lower() in PREFIX_GROUPS:
        rest = m.group("rest").strip()
        scope = (m.group("scope") or "").strip()
        return f"**{scope}:** {rest}" if scope else rest
    return t


def line(pr: dict) -> str:
    return f"- {display_title(pr['title'])} ([#{pr['number']}]({pr['url']}))"


def grouped(prs: list[dict]) -> dict[str, list[dict]]:
    out: dict[str, list[dict]] = {g: [] for g in GROUPS}
    for pr in sorted(prs, key=lambda p: int(p["number"])):
        g = group_of(pr)
        if g is not None:
            out[g].append(pr)
    return out


def plural(n: int, one: str, many: str) -> str:
    return f"{n} {one if n == 1 else many}"


COUNT_WORDS = {
    "Security": ("security change", "security changes"),
    "Features": ("feature", "features"),
    "Fixes": ("fix", "fixes"),
    "Docs": ("docs change", "docs changes"),
    "Internal": ("internal change", "internal changes"),
    "Other changes": ("other change", "other changes"),
}


def summary(groups: dict[str, list[dict]]) -> str:
    parts = [plural(len(groups[g]), *COUNT_WORDS[g]) for g in GROUPS if groups[g]]
    if not parts:
        return ""
    if len(parts) == 1:
        return parts[0]
    return ", ".join(parts[:-1]) + " and " + parts[-1]


FIRST = "The first release tagged in this repository."


def release_lines(rel: dict, level: str) -> list[str]:
    """One release: the summary line, its highlights, then each group under a
    `level` heading. Shared by the notes (`##`) and the changelog (`###`)."""
    prev = rel.get("prev")
    if not prev:
        return [FIRST]
    groups = grouped(rel.get("prs", []))
    s = summary(groups)
    out = [f"{s[0].upper()}{s[1:]} since {prev}." if s else f"No pull requests were merged since {prev}."]
    highlights = [p for g in GROUPS for p in groups[g] if HIGHLIGHT_LABELS & set(labels_of(p))]
    if highlights:
        out += ["", "**Highlights:**", ""] + [line(p) for p in highlights]
    for g in GROUPS:
        if groups[g]:
            out += ["", f"{level} {g}", ""] + [line(p) for p in groups[g]]
    return out


def compare_link(repo: str, rel: dict) -> list[str]:
    if not rel.get("prev"):
        return []
    return ["", f"**Full changelog:** https://github.com/{repo}/compare/{rel['prev']}...{rel['tag']}"]


def check_release(rel: dict) -> None:
    if not isinstance(rel, dict) or not SEMVER_TAG_RE.match(str(rel.get("tag", ""))):
        raise Unusable(f"a release needs a vX.Y.Z tag: {rel!r}")
    for pr in rel.get("prs", []):
        for k in ("number", "title", "url"):
            if k not in pr:
                raise Unusable(f"pull request without {k!r} in {rel['tag']}: {pr!r}")


def render_notes(data: dict, install: str) -> str:
    """One release's notes: summary, highlights, groups, then the install block."""
    rel = data["releases"][0] if "releases" in data else data
    check_release(rel)
    repo = data.get("repo", "ZacxDev/muster")
    out = ["## Highlights", ""] + release_lines(rel, "##") + compare_link(repo, rel)
    text = "\n".join(out).rstrip() + "\n"
    if install.strip():
        text += "\n" + install.rstrip() + "\n"
    return text


CHANGELOG_HEAD = """# Changelog

Every muster release, newest first: the pull requests merged between one
release tag and the one before it, grouped as each release's own notes are.

Generated by `scripts/release-notes.py` (`make changelog`) from the same data
as the release notes; do not edit it by hand, rewrite it.
"""


def render_changelog(data: dict) -> str:
    rels = data.get("releases")
    if not isinstance(rels, list) or not rels:
        raise Unusable("changelog data needs a non-empty `releases` list")
    for r in rels:
        check_release(r)
    repo = data.get("repo", "ZacxDev/muster")

    def key(r: dict) -> tuple[int, int, int]:
        m = SEMVER_TAG_RE.match(r["tag"])
        return (int(m.group(1)), int(m.group(2)), int(m.group(3)))  # type: ignore[union-attr]

    out = [CHANGELOG_HEAD.rstrip()]
    for r in sorted(rels, key=key, reverse=True):
        out += ["", f"## {r['tag']}", ""] + release_lines(r, "###") + compare_link(repo, r)
    return "\n".join(out).rstrip() + "\n"


# --- collect: the only part that reads git and GitHub ------------------------

def run(cmd: list[str]) -> str:
    try:
        return subprocess.run(cmd, check=True, capture_output=True, text=True).stdout
    except (OSError, subprocess.CalledProcessError) as e:
        err = getattr(e, "stderr", "") or str(e)
        print(f"release-notes: {' '.join(cmd)} failed: {err.strip()}", file=sys.stderr)
        sys.exit(1)


def semver_tags(git: list[str]) -> list[str]:
    tags = [t for t in run(git + ["tag", "--list", "v*"]).split() if SEMVER_TAG_RE.match(t)]
    return sorted(tags, key=lambda t: tuple(int(x) for x in SEMVER_TAG_RE.match(t).groups()))  # type: ignore[union-attr]


def previous_tag(git: list[str], tags: list[str], tag: str) -> str | None:
    below = tags[: tags.index(tag)]
    for t in reversed(below):
        r = subprocess.run(git + ["merge-base", "--is-ancestor", t, tag], capture_output=True)
        if r.returncode == 0:
            return t
        if r.returncode != 1:
            print(f"release-notes: git merge-base {t} {tag} failed: {r.stderr.decode().strip()}", file=sys.stderr)
            sys.exit(1)
    return None


def pr_numbers(git: list[str], prev: str, tag: str) -> list[int]:
    nums: list[int] = []
    for subject in run(git + ["log", "--first-parent", "--format=%s", f"{prev}..{tag}"]).splitlines():
        m = SQUASH_RE.search(subject) or MERGE_RE.match(subject)
        if m and int(m.group(1)) not in nums:
            nums.append(int(m.group(1)))
    return nums


def collect(repo: str, git_dir: str, tag: str | None, every: bool) -> dict:
    git = ["git", "-C", git_dir]
    tags = semver_tags(git)
    if every:
        wanted = tags
    else:
        if tag not in tags:
            print(f"release-notes: {tag} is not a vX.Y.Z tag here (git tag --list)", file=sys.stderr)
            sys.exit(1)
        wanted = [tag]
    ranges = [(t, previous_tag(git, tags, t)) for t in wanted]
    needed = {n for t, p in ranges if p for n in pr_numbers(git, p, t)}
    meta: dict[int, dict] = {}
    if needed:
        listed = json.loads(run(["gh", "pr", "list", "--repo", repo, "--state", "merged",
                                 "--limit", str(max(1000, 2 * max(needed))),
                                 "--json", "number,title,labels,url"]))
        meta = {int(p["number"]): p for p in listed}
    releases = []
    for t, p in ranges:
        prs = []
        for n in (pr_numbers(git, p, t) if p else []):
            m = meta.get(n)
            if m is None:
                print(f"release-notes: #{n} (in {p}..{t}) is not a merged pull request of {repo}", file=sys.stderr)
                sys.exit(1)
            prs.append({"number": n, "title": m["title"], "url": m["url"],
                        "labels": sorted(l["name"] for l in m.get("labels", []))})
        releases.append({"tag": t, "prev": p, "prs": prs})
    return {"repo": repo, "releases": releases}


# --- CLI ---------------------------------------------------------------------

def load(path: str) -> dict:
    try:
        with open(path, encoding="utf-8") as f:
            d = json.load(f)
    except (OSError, ValueError) as e:
        raise Unusable(f"cannot read {path}: {e}") from e
    if not isinstance(d, dict):
        raise Unusable(f"{path} is not a JSON object")
    return d


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    sub = ap.add_subparsers(dest="cmd", required=True)
    c = sub.add_parser("collect")
    c.add_argument("--repo", required=True)
    c.add_argument("--git-dir", default=".")
    g = c.add_mutually_exclusive_group(required=True)
    g.add_argument("--tag")
    g.add_argument("--all", action="store_true")
    n = sub.add_parser("notes")
    n.add_argument("--data", required=True)
    n.add_argument("--install", help="markdown appended below the notes (the install block)")
    ch = sub.add_parser("changelog")
    ch.add_argument("--data", required=True)
    a = ap.parse_args(argv)
    try:
        if a.cmd == "collect":
            json.dump(collect(a.repo, a.git_dir, a.tag, a.all), sys.stdout, indent=1, sort_keys=True)
            sys.stdout.write("\n")
        elif a.cmd == "notes":
            install = ""
            if a.install:
                with open(a.install, encoding="utf-8") as f:
                    install = f.read()
            sys.stdout.write(render_notes(load(a.data), install))
        else:
            sys.stdout.write(render_changelog(load(a.data)))
    except Unusable as e:
        print(f"release-notes: {e}", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))

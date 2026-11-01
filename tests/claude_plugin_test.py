#!/usr/bin/env python3
"""The Claude Code plugin marketplace in this repository, pinned.

    python3 tests/claude_plugin_test.py

`/plugin marketplace add ZacxDev/muster` reads `.claude-plugin/marketplace.json`
at the repository root; `npx skills add ZacxDev/muster` reads the same file and
searches each listed plugin's `skills/` directory. What this pins: the
marketplace and its one plugin are both named `muster` (the README's install
command depends on it), the plugin's source is a path inside this repository,
each skill's frontmatter names it the way both tools accept, the setup skill
sends the agent to the canonical INSTALL.md URL (and to its definition of done)
instead of carrying a copy of its steps, and the plugin holds nothing but its
manifest and skills: no hooks, MCP servers, agents or anything else that would
run on its own once installed.

The schema is Claude Code's (`claude plugin validate` checks the manifests);
this test does not depend on that binary being installed.
"""

from __future__ import annotations

import json
import re
import subprocess
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
MARKETPLACE = ROOT / ".claude-plugin" / "marketplace.json"
INSTALL_URL = "https://raw.githubusercontent.com/ZacxDev/muster/main/INSTALL.md"
DONE_ANCHOR = "Done means"
SKILL_NAME = re.compile(r"^[a-z0-9]+(-[a-z0-9]+)*$")


def frontmatter(path: Path) -> dict[str, str]:
    text = path.read_text()
    m = re.match(r"---\n(.*?)\n---\n", text, re.S)
    if not m:
        raise AssertionError(f"{path} has no YAML frontmatter")
    out = {}
    for line in m.group(1).splitlines():
        key, sep, value = line.partition(":")
        if sep and not line.startswith(" "):
            out[key.strip()] = value.strip()
    return out


def guide_text() -> str:
    return (ROOT / "INSTALL.md").read_text()


def marketplace() -> dict:
    return json.loads(MARKETPLACE.read_text())


def plugin_root() -> Path:
    (entry,) = marketplace()["plugins"]
    return (ROOT / entry["source"]).resolve()


class Marketplace(unittest.TestCase):
    def test_shape(self):
        m = marketplace()
        self.assertEqual(m["name"], "muster")
        self.assertTrue(m["owner"]["name"])
        self.assertEqual([p["name"] for p in m["plugins"]], ["muster"])
        self.assertTrue(m["plugins"][0]["description"])

    def test_source_is_a_plugin_inside_the_repository(self):
        (entry,) = marketplace()["plugins"]
        src = entry["source"]
        self.assertIsInstance(src, str)
        self.assertTrue(src.startswith("./"), src)
        self.assertNotIn("..", src)
        root = plugin_root()
        self.assertTrue(root.is_relative_to(ROOT), root)
        manifest = json.loads((root / ".claude-plugin" / "plugin.json").read_text())
        self.assertEqual(manifest["name"], "muster")

    def test_plugin_holds_only_its_manifest_and_skills(self):
        # Hooks, .mcp.json, agents, commands, bin/ ... would all load or run
        # by themselves once installed; adding one must be a deliberate change.
        root = plugin_root()
        self.assertEqual(sorted(p.name for p in root.iterdir()), [".claude-plugin", "skills"])
        self.assertEqual(sorted(p.name for p in (root / ".claude-plugin").iterdir()), ["plugin.json"])
        for path in (root / "skills").rglob("*"):
            if path.is_file():
                self.assertEqual(path.name, "SKILL.md", path)


class Skills(unittest.TestCase):
    def skills(self) -> dict[str, Path]:
        return {p.parent.name: p for p in sorted((plugin_root() / "skills").glob("*/SKILL.md"))}

    def test_the_skill_set(self):
        self.assertEqual(sorted(self.skills()), ["muster-setup"])

    def test_frontmatter(self):
        for name, path in self.skills().items():
            fm = frontmatter(path)
            self.assertEqual(fm.get("name"), name, path)
            self.assertRegex(name, SKILL_NAME)
            self.assertGreater(len(fm.get("description", "")), 40, path)

    def test_setup_skill_points_at_the_canonical_guide(self):
        text = self.skills()["muster-setup"].read_text()
        self.assertIn(INSTALL_URL, text)
        # The README's one-prompt install names the same URL.
        self.assertIn(INSTALL_URL, (ROOT / "README.md").read_text())
        # It carries no copy of the guide's commands, so the two cannot drift.
        self.assertNotIn("```", text)

    def test_setup_skill_defers_to_the_guides_definition_of_done(self):
        text = self.skills()["muster-setup"].read_text()
        self.assertIn(f'"{DONE_ANCHOR}"', text)
        # The sentence it names must exist in the guide, exactly once.
        self.assertEqual(guide_text().count(DONE_ANCHOR), 1)


SKILLS_ADD = "npx skills add ZacxDev/muster --skill muster-setup"
# YAML that turns a plain scalar into something other than one string: a
# mapping (": "), a comment (" #"), or a leading indicator character.
YAML_PLAIN_BREAKERS = (": ", " #")
YAML_INDICATORS = tuple("-?:,[]{}#&*!|>'\"%@`")


def tracked_files() -> list[str]:
    # Committed files, plus untracked ones not ignored: what a push would
    # publish, so a stray copy fails here before it is committed.
    out = subprocess.run(
        ["git", "-C", str(ROOT), "ls-files", "-z", "--cached", "--others", "--exclude-standard"],
        check=True,
        capture_output=True,
    ).stdout
    return [p for p in out.decode().split("\0") if p]


def skills_cli_plugin_dirs() -> list[Path]:
    """The directories the `skills` CLI (vercel-labs/skills 1.7.4,
    getPluginSkillPaths) adds to its search for each marketplace plugin: the
    plugin's own `skills/` directory always, plus the parent of each declared
    `skills` entry. It ignores a plugin whose `source` (or a `pluginRoot`)
    does not start with "./"."""
    m = marketplace()
    root = m.get("metadata", {}).get("pluginRoot")
    if root is not None and not root.startswith("./"):
        return []
    dirs = []
    for p in m["plugins"]:
        src = p.get("source")
        if src is not None and not (isinstance(src, str) and src.startswith("./")):
            continue
        base = ROOT / (root or "") / (src or "")
        for declared in p.get("skills") or []:
            if declared.startswith("./"):
                dirs.append((base / declared).parent.resolve())
        dirs.append((base / "skills").resolve())
    return dirs


class SkillsCli(unittest.TestCase):
    """`npx skills add ZacxDev/muster` installs the plugin's own SKILL.md.

    Measured with skills 1.7.4 against this repository: it finds exactly
    `muster-setup` and installs a byte-identical copy. A second SKILL.md in any
    of the CLI's standard locations (`skills/`, `.claude/skills/`, the root,
    ...) is searched BEFORE the plugin's directory, so a copy there with the
    same name silently replaces the plugin's skill for skills.sh users while
    Claude Code keeps installing the original. One file serves both tools."""

    PLUGIN_SKILL = "plugins/muster/skills/muster-setup/SKILL.md"

    def test_the_plugins_skill_is_the_only_skill_file(self):
        found = [p for p in tracked_files() if p.rsplit("/", 1)[-1] == "SKILL.md"]
        self.assertEqual(found, [self.PLUGIN_SKILL])

    def test_the_cli_searches_the_plugins_skill_directory(self):
        # Through the marketplace, not by luck: when nothing turns up in the
        # searched directories the CLI falls back to a recursive walk, which
        # would also find this file today, but only while it is the repo's only
        # SKILL.md anywhere.
        skill_dir = (ROOT / self.PLUGIN_SKILL).parent.resolve()
        self.assertIn(skill_dir.parent, skills_cli_plugin_dirs())

    def test_frontmatter_parses_as_two_plain_strings(self):
        # The CLI parses frontmatter as YAML and skips a skill whose `name` or
        # `description` is missing or not a string; frontmatter() above reads
        # lines, so it would accept a value YAML reads as something else.
        fm = frontmatter(ROOT / self.PLUGIN_SKILL)
        for key in ("name", "description"):
            value = fm.get(key, "")
            self.assertTrue(value, key)
            for breaker in YAML_PLAIN_BREAKERS:
                self.assertNotIn(breaker, value, key)
            self.assertFalse(value.startswith(YAML_INDICATORS), key)
        # A `metadata` block can mark a skill `internal: true`, which the CLI
        # hides from a plain `skills add`.
        self.assertNotIn("metadata", fm)


class Readme(unittest.TestCase):
    def test_install_from_inside_claude_code(self):
        text = (ROOT / "README.md").read_text()
        self.assertIn("/plugin marketplace add ZacxDev/muster", text)
        self.assertIn("/plugin install muster@muster", text)

    def test_install_with_the_skills_cli(self):
        # Other repositories publish unrelated skills named `muster`, so the
        # README names this repository and the skill explicitly.
        text = (ROOT / "README.md").read_text()
        self.assertIn(f"\n{SKILLS_ADD}\n", text)
        self.assertIn("muster by ZacxDev", text)
        # The skill the command names is one the plugin actually ships.
        self.assertIn(SKILLS_ADD.rsplit(" ", 1)[-1], Skills().skills())


if __name__ == "__main__":
    unittest.main()

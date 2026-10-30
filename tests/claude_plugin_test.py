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


class Readme(unittest.TestCase):
    def test_install_from_inside_claude_code(self):
        text = (ROOT / "README.md").read_text()
        self.assertIn("/plugin marketplace add ZacxDev/muster", text)
        self.assertIn("/plugin install muster@muster", text)


if __name__ == "__main__":
    unittest.main()

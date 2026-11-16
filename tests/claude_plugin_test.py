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
# The newest release's own copy of the guide (scripts/release-build.sh attaches it).
INSTALL_URL = "https://github.com/ZacxDev/muster/releases/latest/download/INSTALL.md"
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



# --- the prompt and the skill hold no facts, only references ------------------

# The references a prompt or the skill may carry: the repository, a file on
# its main branch (on GitHub or raw), and the NEWEST release's asset. Only
# these are taken out before the fact check, and none may hold a digit, so a
# URL-borne fact (http://localhost:<port>, a version-pinned release URL, a
# blob at a tag) is still a fact.
REFERENCE_URLS = re.compile(
    r"https://github\.com/ZacxDev/muster"
    r"(?:/blob/main/[A-Za-z_./-]+(?:#[a-z-]+)?|/releases/latest/download/[A-Za-z_.-]+)?"
    r"|https://raw\.githubusercontent\.com/ZacxDev/muster/main/[A-Za-z_./-]+"
    r"(?![^\s`>)\]])")
# A fact a prompt must not carry: any digit (a version, a port, a step number,
# a duration), a setting's value (NAME=…), or an image reference.
FACT_RES = (
    ("a digit (a version, port, step number or duration)", re.compile(r"\d")),
    ("a setting's value (NAME=...)", re.compile(r"\b[A-Z][A-Z0-9_]{2,}=")),
    ("an image reference", re.compile(r"ghcr\.io|docker\.io|:latest\b")),
)


def facts(text: str) -> list[str]:
    """What TEXT states that the code owns: every match of FACT_RES once the
    reference URLs (REFERENCE_URLS, which are allowed) are taken out; any other
    URL is checked like the rest of the text."""
    bare = REFERENCE_URLS.sub(" ", text)
    return [f"{why}: {m.group(0)!r} in ...{bare[max(0, m.start() - 30):m.end() + 30]!r}..."
            for why, rx in FACT_RES for m in rx.finditer(bare)]


def the_prompt() -> str:
    m = re.search(r"```text\n(.*?)\n```", (ROOT / "README.md").read_text(), re.S)
    assert m, "README.md has no ```text block (the one prompt)"
    return m.group(1)


class Drift(unittest.TestCase):
    """The one prompt (README's copy; internal/ui's onboarding test pins the
    other three byte-identical to it) and the setup skill are pasted and
    installed where no release can update them, so they hold no fact of their
    own: no version, port, image tag, setting value or step number — only
    where facts live (URLs, doc paths, `muster doctor`, `--help`)."""

    def skill(self) -> str:
        text = (plugin_root() / "skills/muster-setup/SKILL.md").read_text()
        return text.split("\n---\n", 1)[1]  # the body; the frontmatter is the description

    def test_the_prompt_holds_no_facts(self) -> None:
        self.assertEqual(facts(the_prompt()), [])

    def test_the_skill_holds_no_facts(self) -> None:
        self.assertEqual(facts(self.skill()), [])
        self.assertEqual(facts(frontmatter(plugin_root() / "skills/muster-setup/SKILL.md")["description"]), [])

    def test_the_fact_check_sees_each_kind_of_fact(self) -> None:
        # Negative controls: each kind, planted into the real prompt, is seen,
        # a URL-borne one included (#728 round 0: every URL used to be taken
        # out, so a version-pinned release URL or a port passed).
        p = the_prompt()
        for planted in ("open http-less port 8105", "release v0.235.0", "set MUSTER_SECURE_COOKIES=0",
                        "image ghcr.io/zacxdev/muster", "the guide's Step 7",
                        "open http://localhost:8105", "https://github.com/ZacxDev/muster/releases/download/v1.2.3/INSTALL.md",
                        "https://github.com/ZacxDev/muster/blob/v0.235.0/INSTALL.md",
                        "https://github.com/ZacxDev/muster/releases/latest/download/muster_linux_amd64.tar.gz",
                        "https://raw.githubusercontent.com/ZacxDev/muster/v0.235.0/INSTALL.md"):
            self.assertTrue(facts(p + " " + planted), f"control: {planted!r} went unseen")
        # Positive control: the references themselves are not facts.
        for ref in ("https://github.com/ZacxDev/muster", INSTALL_URL,
                    "https://github.com/ZacxDev/muster/blob/main/docs/configuration.md",
                    "<https://github.com/ZacxDev/muster/blob/main/docs/features.md#agent-ask-tool>",
                    "https://raw.githubusercontent.com/ZacxDev/muster/main/INSTALL.md"):
            self.assertEqual(facts("see " + ref + " here"), [], f"control: {ref!r} is a reference, not a fact")

    def test_the_prompt_downloads_the_releases_guide(self) -> None:
        # The finding: "read <url>" lets a fetch tool hand the agent a summary
        # of a program to run, and the URL was main's copy while the guide
        # installs the newest release. The prompt downloads the RELEASE asset.
        p = the_prompt()
        self.assertIn(f"curl -fsSL -o muster-install.md {INSTALL_URL}", p)
        self.assertNotIn("raw.githubusercontent.com", p)
        self.assertNotRegex(p, r"\bread https?://", "the prompt tells the agent to READ a URL")
        skill = self.skill()
        self.assertIn(f"curl -fsSL -o muster-install.md {INSTALL_URL}", skill)

    def test_every_section_the_skill_names_exists(self) -> None:
        # The skill refers to the guide's sections by TITLE ("<title>" section),
        # never a step number: each must be a heading of INSTALL.md.
        titles = {re.sub(r"^Step \d+: ", "", m) for m in re.findall(r"(?m)^#{2,4} (.+?)\s*$", guide_text())}
        named = re.findall(r'"([^"]+)" section', " ".join(self.skill().split()))
        self.assertGreaterEqual(len(named), 2, f"positive control: the skill names few sections: {named}")
        for t in named:
            self.assertIn(t, titles, f"the skill names the guide's {t!r} section, which is not a heading")
        self.assertNotIn("No such title", titles, "control: an invented title is not a heading")


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

    def test_description_parses_as_a_plain_string(self):
        # The CLI parses frontmatter as YAML and skips a skill whose
        # `description` is not a string; frontmatter() above reads lines, so it
        # would accept a value YAML reads as something else.
        value = frontmatter(ROOT / self.PLUGIN_SKILL).get("description", "")
        self.assertTrue(value)
        for breaker in YAML_PLAIN_BREAKERS:
            self.assertNotIn(breaker, value)
        self.assertFalse(value.startswith(YAML_INDICATORS))


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

#!/usr/bin/env python3
"""The docs' "bring a session you already have into tmux" commands, run for real.

    python3 tests/docs_tmux_test.py

docs/mirroring.md tells a person how to move the Claude Code conversation they
have into tmux, so muster can mirror it. Two ways that advice was wrong, each
pinned here against a REAL tmux (a private server, `-L muster-docs-it-<rand>`,
`-f /dev/null`; never the operator's) and a fake `claude` on PATH:

  * `tmux new -A -s work '<cmd>'` ATTACHES to an existing `work` and IGNORES
    <cmd> — the reporting user already had a `work` session, so nothing was
    resumed. Case "work exists".
  * `claude --continue` (and Esc in the `--resume` picker) EXITS when there is
    nothing to resume (measured: status 1 on Claude Code 2.1.289), closing the
    tmux window. The fake claude exits 1 for --continue/--resume, so the
    advice must keep the window alive.

The command under test is taken from the docs (the first sh block of the
"Mirrored sessions and dispatched agents" section, and the inline
already-inside-tmux variant), never restated here: a test that copied the
command would keep passing while the docs said something else.

Needs tmux and util-linux `script` (for the attaching client's pty). A missing
tool FAILS the run; it never skips.
"""

from __future__ import annotations

import os
import re
import secrets
import shutil
import subprocess
import tempfile
import time
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
DOC = Path(os.environ.get("MI_MIRRORING_DOC", ROOT / "docs/mirroring.md"))
REAL_TMUX = shutil.which("tmux")
SCRIPT = shutil.which("script")

FAKE_CLAUDE = """#!/bin/sh
# The fake CLI: nothing to resume (as with no conversation, or Esc in the
# picker), else a session that keeps running.
for a in "$@"; do
  case "$a" in --resume|--continue) echo "No conversation found to continue"; exit 1 ;; esac
done
exec sleep 300
"""


def section() -> str:
    text = DOC.read_text()
    start = text.index("## Mirrored sessions and dispatched agents")
    end = text.index("\n## ", start + 3)
    return text[start:end]


def documented_command() -> str:
    m = re.search(r"```sh\n(.*?)```", section(), re.S)
    assert m, "the section has no sh block"
    lines = [l.split("  #")[0].strip() for l in m.group(1).splitlines() if l.strip() and not l.lstrip().startswith("#")]
    assert lines, "the sh block is empty"
    return lines[0]


def inside_tmux_command() -> str | None:
    m = re.search(r"Already inside tmux,\s*`([^`]+)`", section())
    return m.group(1) if m else None


class DocsTmux(unittest.TestCase):
    def setUp(self) -> None:
        self.assertIsNotNone(REAL_TMUX, "tmux is not installed; this test fails rather than skips")
        self.assertIsNotNone(SCRIPT, "util-linux `script` is not installed; this test fails rather than skips")
        self.tmp = Path(tempfile.mkdtemp(prefix="docs-tmux-"))
        self.sock = "muster-docs-it-" + secrets.token_hex(4)
        self.bin = self.tmp / "bin"
        self.bin.mkdir()
        (self.bin / "claude").write_text(FAKE_CLAUDE)
        # Every `tmux` the documented command runs reaches the private server.
        (self.bin / "tmux").write_text(f'#!/bin/sh\nexec "{REAL_TMUX}" -f /dev/null -L {self.sock} "$@"\n')
        for f in self.bin.iterdir():
            f.chmod(0o755)
        self.project = self.tmp / "project"
        self.project.mkdir()
        self.env = {k: v for k, v in os.environ.items() if k not in ("TMUX", "TMUX_PANE")}
        self.env.update(PATH=f"{self.bin}:{os.environ['PATH']}", SHELL="/bin/sh", TERM="xterm-256color")

    def tearDown(self) -> None:
        self.tmux("kill-server")
        shutil.rmtree(self.tmp, ignore_errors=True)

    def tmux(self, *args: str) -> subprocess.CompletedProcess:
        return subprocess.run([str(self.bin / "tmux"), *args], env=self.env, capture_output=True, text=True)

    def run_attaching(self, command: str) -> None:
        """Run command (which attaches a client) on a pty from the project
        directory, then detach that client."""
        p = subprocess.Popen([SCRIPT, "-qec", command, "/dev/null"], cwd=self.project, env=self.env,
                             stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        time.sleep(2.5)
        self.tmux("detach-client", "-a")
        self.tmux("detach-client")
        try:
            p.wait(timeout=5)
        except subprocess.TimeoutExpired:
            p.kill()
            p.wait()

    def claude_windows(self) -> list[str]:
        """Live panes in the project directory whose start command runs claude."""
        out = self.tmux("list-panes", "-a", "-F", "#{pane_dead}\t#{pane_current_path}\t#{pane_start_command}").stdout
        rows = [l.split("\t") for l in out.splitlines()]
        return [r[2] for r in rows if len(r) == 3 and r[0] == "0" and r[1] == str(self.project) and "claude" in r[2]]

    def test_instrument_can_see_a_claude_window_and_can_miss_one(self) -> None:
        # Positive control: a window running the fake claude IS counted.
        self.tmux("new-session", "-d", "-s", "ctl", "-c", str(self.project), "claude")
        time.sleep(0.5)
        self.assertEqual(len(self.claude_windows()), 1, "positive control: a live claude window must be seen")
        # Negative control: one that exits (nothing to resume) is NOT.
        self.tmux("new-window", "-d", "-t", "ctl", "-c", str(self.project), "claude --continue")
        time.sleep(0.5)
        self.assertEqual(len(self.claude_windows()), 1, "negative control: an exited window must not be counted")

    def test_the_documented_command_works_when_work_already_exists(self) -> None:
        self.tmux("new-session", "-d", "-s", "work", "-c", "/", "sleep 300")
        self.run_attaching(documented_command())
        self.assertEqual(len(self.claude_windows()), 1,
                         f"`{documented_command()}` with `work` already running left no live claude window "
                         f"in the project directory:\n{self.tmux('list-panes', '-a', '-F', '#{session_name}:#{window_index} dead=#{pane_dead} #{pane_current_path} #{pane_start_command}').stdout}")

    def test_the_documented_command_works_when_work_does_not_exist(self) -> None:
        self.run_attaching(documented_command())
        self.assertEqual(len(self.claude_windows()), 1,
                         f"`{documented_command()}` on a fresh server left no live claude window in the project directory")

    def test_the_inside_tmux_variant_works(self) -> None:
        cmd = inside_tmux_command()
        self.assertIsNotNone(cmd, "the section names no already-inside-tmux command")
        self.tmux("new-session", "-d", "-s", "work", "-c", "/", "sleep 300")
        subprocess.run(["sh", "-c", cmd], cwd=self.project, env=self.env, capture_output=True, timeout=10)
        time.sleep(1)
        self.assertEqual(len(self.claude_windows()), 1, f"`{cmd}` left no live claude window in the project directory")


class OneCopy(unittest.TestCase):
    """D3: the remedy lives in ONE place. Every other doc links to it, so a fix
    to the command (F1, F2) cannot leave a stale copy behind — the way the
    broken `tmux new -A -s work 'claude --continue'` once sat in five files."""

    def test_only_mirroring_md_spells_the_remedy(self) -> None:
        pattern = re.compile(r"claude --(resume|continue)")
        self.assertTrue(pattern.search(section()), "positive control: the canonical section spells the remedy")
        for rel in ("README.md", "INSTALL.md", "docs/onboarding.md"):
            hits = [l for l in (ROOT / rel).read_text().splitlines() if pattern.search(l)]
            self.assertEqual(hits, [], f"{rel} spells the remedy itself; link to docs/mirroring.md instead")


if __name__ == "__main__":
    unittest.main()

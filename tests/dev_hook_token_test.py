#!/usr/bin/env python3
"""scripts/dev-hook-token.sh: `make run`'s development MUSTER_HOOK_TOKEN.

It must hand back a 64-hex-digit token, make it ONCE and reuse it, never need
openssl, and never write or reuse an empty or short value.

  python3 tests/dev_hook_token_test.py
"""
import os
import re
import shutil
import stat
import subprocess
import tempfile
import unittest

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SCRIPT = os.path.join(ROOT, "scripts", "dev-hook-token.sh")
HEX64 = re.compile(r"^[0-9a-f]{64}$")


def run(path_env, workdir, env_extra=None):
    env = {"PATH": path_env, "HOME": workdir}
    env.update(env_extra or {})
    return subprocess.run(["bash", SCRIPT, os.path.join(workdir, ".dev.env")],
                          cwd=workdir, env=env, capture_output=True, text=True)


def path_without_openssl(tmp):
    """A PATH holding only what the script may use, and no openssl."""
    bindir = os.path.join(tmp, "bin")
    os.mkdir(bindir)
    for tool in ("bash", "od", "tr", "sed", "head", "cat"):
        found = shutil.which(tool)
        assert found, f"{tool} not on PATH"
        os.symlink(found, os.path.join(bindir, tool))
    return bindir


class DevHookToken(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.path = path_without_openssl(self.tmp)
        self.file = os.path.join(self.tmp, ".dev.env")

    def tearDown(self):
        shutil.rmtree(self.tmp)

    def test_makes_one_without_openssl_and_reuses_it(self):
        self.assertIsNone(shutil.which("openssl", path=self.path), "control: openssl must be absent here")
        first = run(self.path, self.tmp)
        self.assertEqual(first.returncode, 0, first.stderr)
        tok = first.stdout.strip()
        self.assertRegex(tok, HEX64)
        with open(self.file) as f:
            self.assertEqual(f.read(), f"export MUSTER_HOOK_TOKEN={tok}\n")
        self.assertEqual(stat.S_IMODE(os.stat(self.file).st_mode), 0o600)
        second = run(self.path, self.tmp)
        self.assertEqual(second.returncode, 0, second.stderr)
        self.assertEqual(second.stdout.strip(), tok, "a second run must reuse the token, not make another")

    def test_an_empty_or_short_file_is_refused_not_reused(self):
        for content in ("export MUSTER_HOOK_TOKEN=\n", "export MUSTER_HOOK_TOKEN=abc123\n", "", "garbage\n"):
            with open(self.file, "w") as f:
                f.write(content)
            r = run(self.path, self.tmp)
            self.assertNotEqual(r.returncode, 0, f"{content!r} was accepted: {r.stdout!r}")
            self.assertEqual(r.stdout.strip(), "", f"{content!r}: a token was printed anyway")
            self.assertIn("64-hex-digit", r.stderr)

    def test_a_failed_generation_writes_nothing(self):
        # An `od` that yields nothing stands in for a generator that is missing
        # or broken: the script must refuse, and leave no file behind.
        os.remove(os.path.join(self.path, "od"))
        with open(os.path.join(self.path, "od"), "w") as f:
            f.write("#!/bin/sh\nexit 0\n")
        os.chmod(os.path.join(self.path, "od"), 0o755)
        r = run(self.path, self.tmp)
        self.assertNotEqual(r.returncode, 0, r.stdout)
        self.assertFalse(os.path.exists(self.file), "an empty token was written")

    def test_the_environment_wins(self):
        r = run(self.path, self.tmp, {"MUSTER_HOOK_TOKEN": "operator-chosen-token-0123456789"})
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(r.stdout.strip(), "operator-chosen-token-0123456789")
        self.assertFalse(os.path.exists(self.file), "the environment's token must not create a file")


if __name__ == "__main__":
    unittest.main(verbosity=2)

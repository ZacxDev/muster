#!/usr/bin/env python3
"""leakscan.py's denylist: the keyed digest, the missing-key verdict, and
matching a name anywhere inside an identifier.

    python3 tests/leakscan_test.py

Every name here is synthetic (`widgetco`, `acme-widget`), and so is the key. The
expected digests are literals computed with `openssl dgst -sha256 -hmac`, not
with the code under test.
"""

from __future__ import annotations

import contextlib
import hashlib
import io
import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import leakscan  # noqa: E402

KEY = b"test-key-not-a-secret-0123456789abcdef"
OTHER_KEY = b"another-test-key-not-a-secret-987654321"

# printf '%s' widgetco | openssl dgst -sha256 -hmac test-key-not-a-secret-0123456789abcdef
WIDGETCO_HMAC = "117bba2bd689362f1391a6e709d409dc24e8da74700488a26031e9ade70c8c7f"
# printf '%s' widget-co | openssl dgst -sha256 -hmac test-key-not-a-secret-0123456789abcdef
WIDGET_CO_HMAC = "625b2e1e966e976c2baf0fc2e39c6b87a754285822068607f7e57e488f87d80a"

REHASH = HERE.parent / "scripts" / "leakscan-rehash.py"


def deny(*names: str, key: bytes = KEY) -> leakscan.Denylist:
    probe = leakscan.Denylist(key, [])
    return leakscan.Denylist(key, [probe.digest(n) for n in names])


class KeyedDigest(unittest.TestCase):
    def test_the_digest_is_hmac_sha256_of_the_normalised_name(self):
        d = leakscan.Denylist(KEY, [])
        self.assertEqual(d.digest("widgetco"), WIDGETCO_HMAC)
        # Case and `_` are folded before the HMAC, so these are one name.
        self.assertEqual(d.digest("Widget_Co"), WIDGET_CO_HMAC)

    def test_an_unkeyed_sha256_entry_matches_nothing(self):
        bare = hashlib.sha256(b"widgetco").hexdigest()
        self.assertEqual(leakscan.Denylist(KEY, [bare]).match("uses widgetco"), [])
        self.assertEqual(leakscan.Denylist(KEY, [WIDGETCO_HMAC]).match("uses widgetco"),
                         ["widgetco"])

    def test_a_list_made_with_another_key_matches_nothing(self):
        made_elsewhere = leakscan.Denylist(OTHER_KEY, []).digest("widgetco")
        self.assertNotEqual(made_elsewhere, WIDGETCO_HMAC)
        self.assertEqual(leakscan.Denylist(KEY, [made_elsewhere]).match("widgetco"), [])

    def test_a_short_key_is_refused(self):
        with self.assertRaises(leakscan.DenylistError):
            leakscan.leakscan_key({leakscan.LEAKSCAN_KEY_ENV: "short"})

    def test_whitespace_around_the_key_is_dropped(self):
        env = {leakscan.LEAKSCAN_KEY_ENV: KEY.decode() + "\n"}
        self.assertEqual(leakscan.leakscan_key(env), KEY)


class RehashScript(unittest.TestCase):
    def run_rehash(self, stdin: str, key: bytes | None):
        env = {k: v for k, v in os.environ.items() if k != leakscan.LEAKSCAN_KEY_ENV}
        if key is not None:
            env[leakscan.LEAKSCAN_KEY_ENV] = key.decode()
        return subprocess.run([sys.executable, str(REHASH)], input=stdin, env=env,
                              capture_output=True, text=True)

    def test_its_output_is_a_list_the_scanner_loads_and_matches(self):
        r = self.run_rehash("WidgetCo\n# a comment\n\nwidgetco\n", KEY)
        self.assertEqual(r.returncode, 0, r.stderr)
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "list.txt"
            path.write_text(r.stdout)
            digests = leakscan.load_denylist(path)
        self.assertEqual(digests, [WIDGETCO_HMAC])  # deduplicated after folding
        self.assertEqual(leakscan.Denylist(KEY, digests).match("X-Widgetco-Id"),
                         ["widgetco"])

    def test_no_key_is_refused_and_writes_nothing(self):
        r = self.run_rehash("widgetco\n", None)
        self.assertEqual(r.returncode, 2)
        self.assertEqual(r.stdout, "")

    def test_an_unreachable_name_is_refused_without_echoing_it(self):
        r = self.run_rehash("widgetco\nqzx\n", KEY)
        self.assertEqual(r.returncode, 2)
        self.assertEqual(r.stdout, "")
        self.assertIn("line 2", r.stderr)
        self.assertNotIn("qzx", r.stderr)


def ci_env(event: str, payload: dict | None = None, tmp: str | None = None) -> dict:
    env = {"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": event}
    if payload is not None:
        fd, name = tempfile.mkstemp(suffix=".json", dir=tmp)
        os.close(fd)
        path = Path(name)
        path.write_text(json.dumps(payload))
        env["GITHUB_EVENT_PATH"] = str(path)
    return env


def pr_payload(head: str | None, base: str = "owner/repo") -> dict:
    return {"repository": {"full_name": base},
            "pull_request": {"head": {"repo": {"full_name": head} if head else None}}}


class MissingKey(unittest.TestCase):
    def test_who_needs_the_key(self):
        with tempfile.TemporaryDirectory() as tmp:
            cases = [
                ({}, False),
                (ci_env("push"), True),
                (ci_env("workflow_dispatch"), True),
                (ci_env("pull_request", pr_payload("owner/repo"), tmp), True),
                (ci_env("pull_request", pr_payload("someone/fork"), tmp), False),
                (ci_env("pull_request", pr_payload(None), tmp), False),
                # An unreadable payload is NOT taken for a fork.
                (ci_env("pull_request"), True),
            ]
            for env, want in cases:
                with self.subTest(env=env):
                    self.assertEqual(leakscan.key_required(env)[0], want)

    def self_test(self, env: dict) -> tuple[int, str]:
        """The real self-test, with a well-formed 16-entry list so the only
        thing varying between cases is the key and the environment."""
        digests = [f"{i:064x}" for i in range(1, leakscan.DENIED_COUNT + 1)]
        out = io.StringIO()
        with mock.patch.object(leakscan, "load_denylist", return_value=digests), \
                contextlib.redirect_stdout(out):
            rc = leakscan.self_test(env)
        leakscan.ACTIVE_DENYLIST = None
        return rc, out.getvalue()

    def test_ci_without_the_key_fails_closed(self):
        rc, out = self.self_test(ci_env("push"))
        self.assertEqual(rc, 2, out)
        self.assertIn(f"FAIL  {leakscan.LEAKSCAN_KEY_ENV} is not set", out)

    def test_a_local_run_without_the_key_warns_and_skips_only_that_check(self):
        rc, out = self.self_test({})
        self.assertEqual(rc, 0, out)
        self.assertIn(f"WARN  {leakscan.LEAKSCAN_KEY_ENV} is not set", out)
        self.assertIn("PASS  private-ip", out)  # the other rules still ran

    def test_ci_with_the_key_passes(self):
        env = ci_env("push")
        env[leakscan.LEAKSCAN_KEY_ENV] = KEY.decode()
        rc, out = self.self_test(env)
        self.assertEqual(rc, 0, out)
        self.assertIn("PASS  denied-identifier", out)

    def test_the_placeholder_list_fails(self):
        out = io.StringIO()
        with mock.patch.object(leakscan, "load_denylist", return_value=[]), \
                contextlib.redirect_stdout(out):
            rc = leakscan.self_test({leakscan.LEAKSCAN_KEY_ENV: KEY.decode()})
        leakscan.ACTIVE_DENYLIST = None
        self.assertEqual(rc, 2)
        self.assertIn("PLACEHOLDER", out.getvalue())


class AnywhereInTheIdentifier(unittest.TestCase):
    def test_a_one_word_name_is_found_anywhere_inside_an_identifier(self):
        d = deny("widgetco")
        for line in [
            'req.Header.Set("X-Widgetco-Token", t)',   # the middle of a header
            'host := "edge-widgetco"',                 # the tail of a compound
            "go build ./cmd/widgetcoctl",              # glued into a longer word
            "c := NewMyWidgetcoClient()",              # one camelCase word
            "WIDGETCO_URL=https://example.com",        # upper case, `_`
            "widgetco",
        ]:
            with self.subTest(line=line):
                self.assertEqual(d.match(line), ["widgetco"])

    def test_ordinary_names_that_only_spell_it_across_a_boundary_pass(self):
        d = deny("widgetco")
        for line in [
            "func TestWidgetCompanyRoute(t *testing.T)",  # across camelCase words
            "isWidgetCoordinate := true",
            'name = "widget-co"',                          # across a separator
            "widgetc",                                     # shorter
        ]:
            with self.subTest(line=line):
                self.assertEqual(d.match(line), [])

    def test_a_compound_name_needs_whole_segments(self):
        d = deny("acme-widget")
        self.assertEqual(d.match('id = "x-acme-widget-y"'), ["acme-widget"])
        self.assertEqual(d.match('id = "acme-widgets"'), [])

    def test_a_public_word_is_cut_out_before_the_walk(self):
        d = deny("widgetco")
        line = "import github.com/WidgetCorp/app and widgetcorp/app"
        self.assertEqual(d.match(line), ["widgetco"])  # the control: it is there
        with mock.patch.object(leakscan, "PUBLIC_WORDS", ("widgetcorp",)):
            self.assertEqual(deny("widgetco").match(line), [])

    def test_the_wire_protocol_marker_excuses_only_the_denied_rule(self):
        marked = f'const H = "X-Widgetco-Id"  // {leakscan.WIRE_PROTOCOL_MARKER}'
        with mock.patch.object(leakscan, "ACTIVE_DENYLIST", deny("widgetco")):
            leakscan.WIRE_EXCUSED.clear()
            self.assertEqual(
                [f.rule for f in leakscan.scan_text('const H = "X-Widgetco-Id"')],
                ["denied-identifier"])
            self.assertEqual(leakscan.scan_text(marked), [])
            self.assertEqual(len(leakscan.WIRE_EXCUSED), 1)
            leakscan.WIRE_EXCUSED.clear()


if __name__ == "__main__":
    unittest.main()

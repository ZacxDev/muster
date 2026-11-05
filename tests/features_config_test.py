#!/usr/bin/env python3
"""docs/features.md, deploy/docker-compose.yml and deploy/.env.example agree.

    python3 tests/features_config_test.py

Static, no Docker. What it pins:

* ANCHORS. docs/features.md has a heading for each of this page's own anchors
  (REQUIRED_ANCHORS), by GitHub's slug rule, each exactly once. The feature
  registry's ids are NOT listed here: the Go registry owns them, and
  cmd/muster-server's TestEveryRegistryIDIsAFeaturesHeading checks every one is
  a heading of this page (and every link a hint writes resolves), reading the
  ids from the registry itself — so there is one id list, not two.
* LINKS. Every `docs/features.md#<id>` written in the compose file, the env
  template and INSTALL.md names one of its headings.
* TWO DIRECTIONS between the template and the compose file: every variable
  .env.example names (commented or not) is interpolated by the compose file,
  and every variable the compose file interpolates is in .env.example. One
  direction alone would let a documented line do nothing, or a forwarded
  setting go undocumented.
* EVERY SERVER VARIABLE IS ACCOUNTED FOR. Each name in muster-server's
  environment const block (cmd/muster-server/config.go) is either forwarded
  into muster's environment by the compose file or listed in NOT_FORWARDED
  with its reason — never both, and NOT_FORWARDED names only real variables.
  And the compose file forwards nothing configuration.md does not document
  (a misspelt forward would otherwise be a silent no-op).
* Every optional block of .env.example names its docs/features.md section.

Each check runs its own negative control on a mutated copy, so a check that
cannot fail is reported rather than passed.
"""

from __future__ import annotations

import re
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
FEATURES = ROOT / "docs/features.md"
COMPOSE = ROOT / "deploy/docker-compose.yml"
EXAMPLE = ROOT / "deploy/.env.example"
CONFIG_GO = ROOT / "cmd/muster-server/config.go"
CONFIG_MD = ROOT / "docs/configuration.md"
INSTALL = ROOT / "INSTALL.md"

# This page's own anchors that other files link to. The feature registry's ids
# are checked by the Go side (cmd/muster-server features_test.go,
# TestEveryRegistryIDIsAFeaturesHeading), which reads them from the registry:
# a second, hand-copied list here could only drift from it (#441 round 0, D7).
REQUIRED_ANCHORS = ["core", "ask-cards", "other-settings"]

# Server variables the compose file does NOT forward, each with why. The agent
# plane is the one group: its provisioner uses Kubernetes in-cluster
# credentials only (cmd/muster-server/provisioner.go), so none of it can work
# in the Compose stack, and docs/features.md#dispatch says so.
AGENT_PLANE = "agent plane: needs muster running inside Kubernetes (docs/features.md#dispatch)"
# The Compose chief's own variables, until the compose file's `chief` profile
# forwards them (the pull request that adds the profile removes this group).
COMPOSE_CHIEF_PENDING = "the Compose chief runtime: forwarded with the `chief` profile, which a later change adds"
NOT_FORWARDED = {
    **{name: COMPOSE_CHIEF_PENDING for name in [
        "MUSTER_CHIEF_RUNTIME", "MUSTER_CHIEF_SPEC_DIR", "MUSTER_CHIEF_CLAUDE_TOKEN",
    ]},
    **{name: AGENT_PLANE for name in [
        "MUSTER_AGENT_PROVISIONER", "MUSTER_AGENT_GATEWAY", "MUSTER_AGENT_GATEWAY_MODEL",
        "MUSTER_AGENT_GATEWAY_PORT", "MUSTER_AGENT_RUNTIME_CONFIG", "MUSTER_AGENT_RUNTIME_INSTALL",
        "MUSTER_AGENT_IMAGE_REPO", "MUSTER_AGENT_IMAGE_TAG", "MUSTER_AGENT_API_URL", "MUSTER_AGENT_MODEL",
        "MUSTER_AGENT_NAMESPACE", "MUSTER_AGENT_NAMESPACE_PREFIX", "MUSTER_AGENT_NAMESPACE_SHARED",
        "MUSTER_AGENT_NAMESPACE_CLUSTERROLE", "MUSTER_AGENT_WORKSPACE_PERSIST",
        "MUSTER_AGENT_WORKSPACE_STORAGE_CLASS", "MUSTER_AGENT_ENDPOINT_TEMPLATE", "MUSTER_AGENT_CAIRN_URL",
        "MUSTER_AGENT_CAIRN_TOKEN", "MUSTER_AGENT_KINDS", "MUSTER_AGENT_CC_IMAGE", "MUSTER_AGENT_CC_ACCOUNTS",
        "MUSTER_AGENT_CC_TOKEN_", "MUSTER_AGENT_CC_STORAGE_SIZE", "MUSTER_AGENT_CC_RATE_LIMIT_COOLDOWN",
        "MUSTER_AGENT_CC_MAX_CONCURRENT_JOBS", "MUSTER_AGENT_CC_FAILOVER", "MUSTER_AGENT_CC_BYPASS_POOLS",
        "MUSTER_AGENT_CC_POOL_DEPLOY_KEYS", "MUSTER_AGENT_CC_EGRESS_LOCKED_POOLS",
        "MUSTER_AGENT_NETPOL_FROM_NAMESPACE", "MUSTER_AGENT_NETPOL_FROM_POD_LABELS",
        "MUSTER_AGENT_REAP_STOPPED_AFTER", "MUSTER_CHECKOUT_WAIT_TIMEOUT", "MUSTER_CONNECT_PORT",
        "MUSTER_AGENT_CONNECT_URL", "MUSTER_OFFICERS", "MUSTER_CHIEF_SESSION_RUNTIMES",
        "MUSTER_CHIEF_DISPATCH_OWNERS",
        # The memo relay delivers only to dispatched agents, through their gateway.
        "MUSTER_MEMO_URL", "MUSTER_MEMO_TOKEN", "MUSTER_MEMO_SCOPE_MAP",
    ]},
    **{name: "needs the chief, which needs in-cluster dispatch (docs/features.md#chief-nudges)" for name in [
        "MUSTER_CHIEF_NUDGES", "MUSTER_CHIEF_NUDGE_IDLE", "MUSTER_CHIEF_NUDGE_HOURLY_CAP",
        "MUSTER_CHIEF_NUDGE_LIVE_OWNERS", "MUSTER_CHIEF_NUDGE_NEEDS_YOU_LIVE_OWNERS",
        "MUSTER_CHIEF_NUDGE_ASK_THRESHOLD", "MUSTER_CHIEF_NUDGE_CHIEF_CHAT_QUOTES_LIVE_OWNERS",
    ]},
    "MUSTER_TAG_AUTODISPATCH": "reserved: logs and counts eligible tasks, dispatches nothing",
    **{name: "test fixture: read only by a `-tags e2eterm` build" for name in [
        "MUSTER_E2E_HOLD_READY_DIR", "MUSTER_E2E_TERM_WINDOW", "MUSTER_E2E_GITHUB_API",
        "MUSTER_E2E_OPENROUTER_MODELS", "MUSTER_E2E_SUGGEST_URL", "MUSTER_E2E_GOTRUE_SKEW",
        "MUSTER_E2E_AGENT_ENDPOINT",
    ]},
}

# Variables .env.example names that compose reads itself rather than
# interpolating into the file.
COMPOSE_NATIVE = {"COMPOSE_PROFILES"}

SLUG_DROP = re.compile(r"[^\w\- ]", re.UNICODE)


def slug(heading: str) -> str:
    """GitHub's heading anchor: lower-case, punctuation dropped, spaces to '-'."""
    return SLUG_DROP.sub("", heading.strip().lower()).replace(" ", "-")


def headings(md: str) -> list[str]:
    out, fenced = [], False
    for line in md.splitlines():
        if line.lstrip().startswith("```"):
            fenced = not fenced
            continue
        m = re.match(r"^#{1,6}\s+(.*?)\s*#*\s*$", line)
        if m and not fenced:
            out.append(slug(m.group(1)))
    return out


def missing_anchors(md: str) -> list[str]:
    slugs = headings(md)
    return [a for a in REQUIRED_ANCHORS if slugs.count(a) != 1]


def compose_interpolated(text: str) -> set[str]:
    """Every ${NAME…} the compose file interpolates, nested ones included.
    `$${…}` is an escaped dollar (a shell variable inside a command), and a
    whole-line `#` comment is prose, so neither counts."""
    names = set()
    for line in text.splitlines():
        if line.lstrip().startswith("#"):
            continue
        names |= set(re.findall(r"(?<!\$)\$\{([A-Za-z_][A-Za-z0-9_]*)", line))
    return names


def example_names(text: str) -> set[str]:
    return set(re.findall(r"(?m)^#?([A-Z][A-Z0-9_]*)=", text))


def muster_env_keys(text: str) -> set[str]:
    """The keys of the muster service's `environment:` mapping."""
    keys, in_muster, in_env = set(), False, False
    for line in text.splitlines():
        if re.match(r"^  [a-z0-9-]+:\s*$", line):
            in_muster, in_env = line.strip() == "muster:", False
            continue
        if in_muster and re.match(r"^    environment:\s*$", line):
            in_env = True
            continue
        if in_env:
            if re.match(r"^    \S", line):
                in_env = False
                continue
            m = re.match(r"^      ([A-Z][A-Z0-9_]*):", line)
            if m:
                keys.add(m.group(1))
    return keys


def server_env_names(go: str) -> set[str]:
    return set(re.findall(r'(?m)^\s*env[A-Za-z0-9_]*\s*=\s*"([A-Z0-9_]+)"\s*$', go))


def unaccounted(server: set[str], forwarded: set[str], ledger: dict[str, str]) -> list[str]:
    errs = []
    for name in sorted(server):
        if name in forwarded and name in ledger:
            errs.append(f"{name} is forwarded AND listed as not forwarded")
        elif name not in forwarded and name not in ledger:
            errs.append(f"{name}: muster-server reads it, but the compose file does not forward it and "
                        f"NOT_FORWARDED does not say why")
    for name in sorted(set(ledger) - server):
        errs.append(f"NOT_FORWARDED lists {name}, which muster-server does not read")
    return errs


def features_links(text: str) -> set[str]:
    return set(re.findall(r"docs/features\.md#([a-z0-9-]+)", text))


class FeaturesConfig(unittest.TestCase):
    def setUp(self) -> None:
        self.features = FEATURES.read_text()
        self.compose = COMPOSE.read_text()
        self.example = EXAMPLE.read_text()

    def test_every_required_anchor_is_a_heading_once(self) -> None:
        self.assertEqual(missing_anchors(self.features), [])
        # Negative controls: a removed heading, and a second one with the same
        # text (GitHub renames it to `-1`, the link still lands on the first).
        cut = re.sub(r"(?m)^### Ask cards$", "### Question cards", self.features)
        self.assertEqual(missing_anchors(cut), ["ask-cards"], "control: a removed anchor went unseen")
        dup = self.features + "\n### Ask cards\n"
        self.assertEqual(missing_anchors(dup), ["ask-cards"], "control: a duplicated anchor went unseen")

    def test_every_features_link_resolves(self) -> None:
        slugs = set(headings(self.features))
        sources = {"deploy/docker-compose.yml": self.compose, "deploy/.env.example": self.example,
                   "INSTALL.md": INSTALL.read_text()}
        total = 0
        for name, text in sources.items():
            links = features_links(text)
            total += len(links)
            for a in sorted(links - slugs):
                self.fail(f"{name} links docs/features.md#{a}, which is not a heading")
        self.assertGreater(total, 20, "positive control: the link pattern found almost nothing")
        self.assertEqual(features_links("see docs/features.md#nope") - slugs, {"nope"}, "control: a bad link went unseen")

    def test_example_and_compose_name_the_same_variables(self) -> None:
        interp = compose_interpolated(self.compose)
        named = example_names(self.example) - COMPOSE_NATIVE
        self.assertGreater(len(interp), 60, "positive control: few interpolations found")
        self.assertEqual(sorted(named - interp), [], ".env.example names variables the compose file never reads")
        self.assertEqual(sorted(interp - named), [], "the compose file reads variables .env.example does not name")
        # Negative controls, one per direction.
        self.assertIn("MUSTER_HOST_ICONS", interp - compose_interpolated(
            self.compose.replace("MUSTER_HOST_ICONS: ${MUSTER_HOST_ICONS:-}", "")))
        self.assertIn("MUSTER_EXTRA_TYPO", example_names(self.example + "#MUSTER_EXTRA_TYPO=1\n") - interp)
        # The escaped shell variables of the s3 start script are not compose's.
        self.assertNotIn("pid", interp)

    def test_every_server_variable_is_forwarded_or_ledgered(self) -> None:
        server = server_env_names(CONFIG_GO.read_text())
        forwarded = muster_env_keys(self.compose)
        self.assertGreater(len(server), 100, "positive control: the const block parse found few names")
        self.assertGreater(len(forwarded), 60, "positive control: the muster environment parse found few keys")
        self.assertEqual(unaccounted(server, forwarded, NOT_FORWARDED), [])
        # Negative controls: an unforwarded name with no reason, and a stale ledger entry.
        self.assertTrue(any("MUSTER_HOST_ICONS" in e for e in
                            unaccounted(server, forwarded - {"MUSTER_HOST_ICONS"}, NOT_FORWARDED)))
        self.assertTrue(any("MUSTER_GONE" in e for e in
                            unaccounted(server, forwarded, {**NOT_FORWARDED, "MUSTER_GONE": "x"})))

    def test_compose_forwards_only_documented_variables(self) -> None:
        documented = set(re.findall(r"`([A-Z][A-Z0-9_]*)`", CONFIG_MD.read_text()))
        forwarded = muster_env_keys(self.compose)
        self.assertEqual(sorted(forwarded - documented), [],
                         "the compose file forwards names docs/configuration.md does not document (a typo is a no-op)")
        self.assertIn("MUSTER_STT_TOKN", (forwarded | {"MUSTER_STT_TOKN"}) - documented, "control")

    def test_every_optional_block_of_the_example_names_its_section(self) -> None:
        optional = self.example.split("# OPTIONAL FEATURES", 1)
        self.assertEqual(len(optional), 2, ".env.example has no OPTIONAL FEATURES banner")
        blocks = [b for b in re.split(r"\n\s*\n", optional[1]) if re.search(r"(?m)^#?[A-Z][A-Z0-9_]*=", b)]
        self.assertGreater(len(blocks), 15, "positive control: few optional blocks found")
        # A block names its section itself, or sits under a `# --- … docs/features.md#x ---` header.
        current = None
        for b in re.split(r"\n\s*\n", optional[1]):
            m = re.search(r"docs/features\.md#([a-z0-9-]+)", b)
            if re.search(r"(?m)^# --- ", b):
                current = m.group(1) if m else None
            if re.search(r"(?m)^#?[A-Z][A-Z0-9_]*=", b) and not (m or current):
                self.fail(f"this .env.example block names no docs/features.md section:\n{b}")


if __name__ == "__main__":
    unittest.main(verbosity=2)

#!/usr/bin/env python3
"""Refuse SENSITIVE content in this repository.

muster was extracted from a private, single-operator deployment. The source
material carries live hostnames, private addresses, a container registry, real
task numbers and dated incident narration. This gate exists so none of that can
come back — and so the repository can one day be made public without a hand
audit being the only thing standing between it and the internet.

🔴 THE REPOSITORY IS PRIVATE TODAY AND THIS GATE IS NOT A SUBSTITUTE FOR THAT.
A clean run means "the scrub held", not "this is safe to publish". Publication
is a separate decision with a separate review.

🔴 SCOPE: SECURITY, NOT TIDINESS. This blocks the things that would cause harm
if published — credentials, reachable hostnames, real private network
addresses, the specific private identifiers the extraction removed, and dated
incident references. It does NOT police names or dates by heuristic. A gate
that fires hundreds of times for nothing is a gate someone turns off, and then
the real findings ship too.

🔴 A CLEAN RUN IS NOT EVIDENCE UNTIL THE CONTROLS HAVE BEEN WATCHED TO WORK.
A scanner wired to nothing reports zero exactly like a clean tree does, so this
module ships its own controls and runs them on EVERY invocation:

  * NEGATIVE — each rule must refuse a REALISTIC sensitive string. Realistic,
    not a textbook fixture: a scanner that only recognises `example.com` passes
    a real leak, and scanners notoriously allowlist their own canonical
    examples and then scan clean.
  * POSITIVE — the matcher must be able to produce a NON-ZERO count at all.
  * NARROWNESS — legitimate content must NOT be refused, so the gate stays
    usable.

Exit codes:
  0  no findings (and every control behaved)
  1  findings — sensitive content is present
  2  the gate itself could not run, or a control misbehaved. NOT a pass.
"""

from __future__ import annotations

import argparse
import hashlib
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent

# --------------------------------------------------------------------------
# PRIVATE ADDRESSES
#
# 🔴 THE ALLOWLIST IS AN EXPLICIT LITERAL SET, NOT A RANGE, SO ANYTHING NEW
# FAILS CLOSED. A private address that is not on this list is treated as real
# topology and refused.
#
# New examples should prefer RFC5737 TEST-NET (192.0.2.0/24, 198.51.100.0/24,
# 203.0.113.0/24), which are reserved for documentation and need no
# allowlisting at all.
# --------------------------------------------------------------------------
DOC_ADDRESSES = {
    # The conventional Kubernetes pod/service CIDR examples, which any document
    # about deploying muster on a cluster will spell.
    "10.244.0.0",
    "10.96.0.0",
}

_PRIVATE_IP = re.compile(
    r"\b(?:10\.\d{1,3}\.\d{1,3}\.\d{1,3}"
    r"|192\.168\.\d{1,3}\.\d{1,3}"
    r"|172\.(?:1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3})\b"
)

# --------------------------------------------------------------------------
# DENIED IDENTIFIERS — the real project, repository, cluster, host and domain
# names the extraction removed from this tree, so they cannot come back.
#
# 🔴 STORED AS DIGESTS, AND THAT IS THE WHOLE DESIGN, NOT A FLOURISH. A
# denylist of private names written out in plaintext is a file that PUBLISHES
# exactly the names it exists to remove — the gate would be the leak, and it
# would be the most-grepped file in the repository. SHA-256 of the lowercased
# identifier lets this file recognise a name it does not contain.
#
# 🔴 WHAT THAT COSTS, STATED RATHER THAN HIDDEN: nobody can verify from inside
# this repository that these digests are digests of the RIGHT strings. That is
# not a defect to fix, it is the property being bought — a set you could audit
# from here would be a set you could read from here. So the controls below
# prove the MECHANISM end to end (tokenise → prefix → digest → refuse) using
# synthetic sentinels that travel the identical code path, and DENIED_COUNT
# pins the set size so it cannot shrink unnoticed.
#
# ⚠ MATCHING IS EXACT OVER `-`/`_` SEGMENT PREFIXES, NEVER SUBSTRING. Each
# identifier-shaped token on the line is lowercased, `_` is folded to `-`, and
# every leading segment run is hashed: `a-b-c` offers `a`, `a-b`, `a-b-c`. So a
# one-word entry catches every compound built on it, while a compound entry
# catches only itself and its extensions.
#
# ⚠ THE ILLUSTRATIONS BELOW USE THE SENTINELS, NEVER A REAL ENTRY. An earlier
# version of this pattern in a sibling project illustrated the rule with two
# real names three lines under "a set you could audit from here would be a set
# you could read from here", and a grep over the public repo still hit them.
# Sentinels only.
#
#   canarytoken-ci-jx5fq      FIRES  a one-word entry catches every compound
#   CANARYTOKEN_TEST_TMPFS    FIRES  `_`→`-` and case are folded
#   canarytokens              MISS   a longer WORD is a different token
#   redacted-canary-scoped    MISS   a compound entry does not match a sibling
#   scoped-canarytoken        MISS   the walk is over PREFIXES; a denied entry
#                                    in the TAIL is a declared LIMIT of the rule
#
# TO ADD A NAME: `python3 -c 'import hashlib;print(hashlib.sha256(b"thename").hexdigest())'`
# and bump DENIED_COUNT. Removing one has to be argued for in a diff.
# --------------------------------------------------------------------------
DENIED_IDENTIFIER_DIGESTS = {
    # 16 real identifiers from the private deployment, plus the 2 sentinels.
    # Sorted, so a diff that adds one is a one-line diff.
    "16d74232d666243e3dd9711daaef2b7538f849efaa62cf19f91a97e82c420e34",
    "1ac8ca4c444febcb84f6de0da68d3ed09124e9466a7d70e9b4d0137c71d1f10a",
    "216016a8050f7df7d7302341465e5746c918aeea1e49c8cab4708e5a2159f383",
    "32f0ac140a77947727372050caefe08e650a0fd9f502df5659a57f64e9349294",
    "3c3e5b7f6bc16e849f457ed07d5cc060c103a0b7d49caad0fb99a37957f35301",
    "51d69dd9872309699e0b9737bdd48e9ca1de150412c8dd72549391257434dfbb",
    "6686bf96fc55109d308a0f411cd3cf0b9c24b95be8f684e50e8abd0d4d3645d1",
    "80aa2096db0a98483535de29a8df66eb1e6a079dd97619edeb715fd71a07cc56",
    "8be0b5445fca6eb16d14c6f4eaff1cf1d1a3362ef660bfb2b66e74bf309b7e61",
    "8dd1039e919e30153eda15e3e32a5a4db5dd83439711fe3376c66028e3893c30",
    "956176df8ba67c84dce4fa987fc6c2f1bc4f788aef92090d5216f7a9e1e5c1f2",
    "96a4bc2602655473120fcc571ee3d8cfe5f8801f8038ccc06323d305e323331c",
    "bb47f602459de234156e3b164b68cbef045a787668c335c4bd44ab9f0d88a5da",
    "d41f11e5fa5f9aebda4a1878ace020141dec8db3a5bf39519b29baab8732a9fb",
    "d85b40105dd0b9cdbe46a1ee1b96abdf1474ae96fb86bd28ca701f44f017ac3b",
    "e7e03ea356c666c09f11f088309ef6e93e84fd74cf6eeb213e522d5de55e89e6",
    # --- synthetic sentinels; these two are the controls, and they are the
    # --- ONLY entries whose plaintext appears in this file.
    hashlib.sha256(b"canarytoken").hexdigest(),
    hashlib.sha256(b"redacted-canary-scope").hexdigest(),
}

#: 🔴 PINNED SO THE SET CANNOT SHRINK UNNOTICED. A digest quietly deleted takes
#: a name off the gate with no other symptom — the tree still scans clean,
#: which is exactly what it would do if the name had genuinely been removed.
#:
#: ⚠ ONE OF THE 16 IS A PREFIX OF ANOTHER AS A *WORD*, NOT AS A SEGMENT, AND
#: THAT IS WHY BOTH ARE LISTED. The walk is over `-`/`_` segment runs, so a
#: single-token name that merely STARTS with another entry is a different token
#: and is not caught by it — see the sentinel table above. Compound CLI names
#: therefore need their own entry.
DENIED_COUNT = 18

DENY_CANARY = "redacted-canary-scope"
DENY_CANARY_WORD = "canarytoken"

_IDENTIFIER = re.compile(r"[A-Za-z][A-Za-z0-9]*(?:[-_][A-Za-z0-9]+)*")


def denied_identifiers(line: str) -> list[str]:
    """Every denied identifier this line spells, in order of first appearance."""
    out: list[str] = []
    for m in _IDENTIFIER.finditer(line):
        parts = m.group(0).lower().replace("_", "-").split("-")
        for i in range(1, len(parts) + 1):
            candidate = "-".join(parts[:i])
            if hashlib.sha256(candidate.encode()).hexdigest() in DENIED_IDENTIFIER_DIGESTS:
                if candidate not in out:
                    out.append(candidate)
    return out


# --------------------------------------------------------------------------
# DATED INCIDENT REFERENCES — "X was measured on <a real date>".
#
# 🔴 THE RULE IS THE CLAIM SHAPE, NOT THE DATE. What leaks is the pairing of a
# specific observation with a specific day on a real deployment. The date alone
# is not sensitive, and a date in FIXTURE data is not an incident reference.
# Policing every date is the version of this gate that fires hundreds of times
# and gets switched off.
#
# Three spellings, because prose puts the verb on either side of the stamp,
# plus the continuation shape where a wrapped comment line STARTS with the date
# and the verb sits on the line above — which a line-oriented scanner cannot
# see any other way.
#
# YEAR 2000 IS ALLOWED: that is the synthetic date a fixture should use when it
# genuinely needs one. `2006-01-02` is allowed because it is Go's `time`
# reference instant and appears in every layout string.
# --------------------------------------------------------------------------
_DATE = r"(?:19|20)\d\d-\d\d-\d\d"
SYNTHETIC_DATE_YEAR = "2000"
GO_REFERENCE_DATE = "2006-01-02"

_DATED_CLAIM = re.compile(
    r"(?i)\b(?:measured|re-measured|observed|reproduced|verified|recurred"
    r"|rotated|landed|regressed|reported|deployed)\b[^.\n]{0,48}?(" + _DATE + r")"
)
# ⚠ THE TRAILING GUARD IS `(?![\d-])`, NOT `\b`, AND A CONTROL IS WHY. A real
# stamp is often a full RFC 3339 instant — `at 2026-08-23T00:37Z` — and `\b`
# after `23` demands a non-word character, which `T` is not. The `\b` version
# matches the bare-date spelling and silently skips every timestamped one,
# reading as a working rule.
_DATED_STAMP = re.compile(
    r"(?i)\b(?:on|at|since|until|during|as of)\s+(" + _DATE + r")(?![\d-])"
)
_DATED_CONTINUATION = re.compile(r"^\s*(?:#|//|--)\s*(" + _DATE + r")(?![\d-])")


def dated_incident_stamps(line: str) -> list[str]:
    """Every real-dated incident stamp on this line."""
    out: list[str] = []
    for rx in (_DATED_CLAIM, _DATED_STAMP, _DATED_CONTINUATION):
        for m in rx.finditer(line):
            d = m.group(1)
            if d.startswith(SYNTHETIC_DATE_YEAR) or d == GO_REFERENCE_DATE:
                continue
            if d not in out:
                out.append(d)
    return out


# --------------------------------------------------------------------------
# RULES: (name, pattern, why)
# --------------------------------------------------------------------------
RULES: list[tuple[str, str, str]] = [
    (
        "credential",
        r"-----BEGIN [A-Z ]*PRIVATE KEY-----"
        r"|-----BEGIN CERTIFICATE-----"
        r"|\bAKIA[0-9A-Z]{16}\b"
        r"|\bgh[pousr]_[A-Za-z0-9]{20,}"
        r"|\bxox[abprs]-[A-Za-z0-9-]{10,}"
        r"|\bAGE-SECRET-KEY-[A-Z0-9]+"
        # 🔴 A SCOPED `(?i:…)`, NOT A BARE `(?i)`. Python refuses a global
        # inline flag that is not at the start of the expression, and this one
        # sits in the middle of an alternation — it would raise at import,
        # which is the right failure (a crashing gate is visible; a silently
        # disabled one is not), but the scoped form is what actually works.
        # 🔴 THE SEPARATOR IS OPTIONAL: `Authorization: Bearer <token>` puts a
        # SPACE between the scheme and the value, so a pattern demanding `:` or
        # `=` immediately before the value matches neither word and refuses
        # nothing while reading as a working rule.
        r"|(?i:\b(?:authorization|bearer)\s*[:=]?\s*[\"']?[A-Za-z0-9+/_.-]{20,})",
        "a credential, or something shaped like one. Nothing in this repository "
        "may carry a real secret — not in a fixture, not in a comment, not "
        "'temporarily'",
    ),
    (
        "private-hostname",
        # A hostname on a private/lab TLD, or a bare `*.local`/`*.lan` name.
        # These are reachable names on somebody's network and say where things
        # live.
        r"\b[A-Za-z0-9][A-Za-z0-9-]*\.(?:lan|local|internal|home|homelab)\b"
        r"|\b[A-Za-z0-9][A-Za-z0-9-]*\.(?:lan|local|internal)\.[A-Za-z]{2,}\b",
        "a private or lab hostname — it names real infrastructure. Use "
        "`example.com` / `muster.example` in documentation",
    ),
    (
        "registry-ref",
        # An image reference whose registry is not a public one. The tell is a
        # host-looking first segment carrying a dot or a port, followed by a
        # path and a tag.
        r"\b(?!(?:ghcr\.io|docker\.io|quay\.io|registry\.k8s\.io|gcr\.io|"
        r"public\.ecr\.aws|mcr\.microsoft\.com)\b)"
        r"[a-z0-9][a-z0-9.-]*\.[a-z]{2,}(?::\d+)?/[a-z0-9._/-]+:[A-Za-z0-9._-]+",
        "a container image on a registry that is not a public one — it names "
        "private infrastructure and the image will not pull for anyone else",
    ),
    (
        "operator-identity",
        # A personal address, or a filesystem path under somebody's home.
        r"\b[A-Za-z0-9._%+-]+@(?:gmail|outlook|hotmail|yahoo|proton(?:mail)?)\.[a-z]{2,}\b"
        r"|/home/[a-z][a-z0-9_-]*/"
        r"|/Users/[A-Za-z][A-Za-z0-9_-]*/",
        "an operator identity — a personal address, or an absolute path under "
        "somebody's home directory. Both name a person and neither is "
        "reproducible on another machine",
    ),
]


class Finding:
    __slots__ = ("path", "line", "rule", "text", "why")

    def __init__(self, path: str, line: int, rule: str, text: str, why: str):
        self.path, self.line, self.rule, self.text, self.why = path, line, rule, text, why

    def __str__(self) -> str:
        return (f"{self.path}:{self.line}: [{self.rule}] {self.text.strip()[:110]}"
                f"\n      -> {self.why}")


_COMPILED = [(n, re.compile(p), w) for n, p, w in RULES]


def scan_text(text: str, path: str = "<memory>") -> list[Finding]:
    out: list[Finding] = []
    for n, line in enumerate(text.splitlines(), start=1):
        for name, rx, why in _COMPILED:
            if rx.search(line):
                out.append(Finding(path, n, name, line, why))
        # Private addresses are matched separately so the documentation
        # allowlist can be applied per-OCCURRENCE rather than per-line: one real
        # address on a line full of examples must still be caught.
        for m in _PRIVATE_IP.finditer(line):
            if m.group(0) not in DOC_ADDRESSES:
                out.append(Finding(
                    path, n, "private-ip", line,
                    f"{m.group(0)} is a real private address — network topology. "
                    f"Use RFC5737 TEST-NET (192.0.2.0/24) for examples",
                ))
        # The last two rules are per-OCCURRENCE for the same reason, and the
        # message has to say WHICH.
        for ident in denied_identifiers(line):
            out.append(Finding(
                path, n, "denied-identifier", line,
                f"{ident!r} is a denied identifier — a real project, repository, "
                f"cluster, host or domain name from the private deployment muster "
                f"was extracted from. Replace it with something synthetic; there "
                f"are no exceptions to add it to",
            ))
        for stamp in dated_incident_stamps(line):
            out.append(Finding(
                path, n, "dated-incident", line,
                f"{stamp} pins a measurement to a real day on a real deployment. "
                f"Keep the MECHANISM, drop the particulars — or pin it to a COMMIT, "
                f"which is the reproducible form. A fixture that genuinely needs a "
                f"date should use a year-{SYNTHETIC_DATE_YEAR} one",
            ))
    return out


class Skipped:
    """A file the scan did NOT read, carrying the reason it did not.

    🔴 THE REASON IS NOT DECORATION. A skip with no stated cause is a silent
    gap; naming it is what lets a reader tell "binary, correctly ignored" from
    "the gate cannot see this".
    """

    __slots__ = ("path", "why")

    def __init__(self, path: str, why: str):
        self.path, self.why = path, why

    def __str__(self) -> str:
        return f"{self.path} — {self.why}"


def enumerate_repo(root: Path) -> list[str]:
    """Files git knows about, plus untracked-but-not-ignored ones.

    🔴 `git ls-files` ALONE IS BLIND to a file not yet added, and "I forgot to
    git add it" is not a reason for a leak to ship. The `-z` framing is part of
    the contract too: without it git QUOTES a non-ASCII path under the default
    `core.quotePath`, and every downstream message then names a filename that
    does not exist.
    """
    try:
        r = subprocess.run(
            ["git", "-C", str(root), "ls-files", "--cached", "--others",
             "--exclude-standard", "-z"],
            capture_output=True, text=True, check=True,
        )
    except (subprocess.CalledProcessError, FileNotFoundError) as e:
        print(f"leakscan: COULD NOT RUN — git enumeration failed: {e}", file=sys.stderr)
        raise SystemExit(2)
    return [x for x in r.stdout.split("\0") if x]


BINARY_SNIFF_BYTES = 8000


def is_binary(data: bytes) -> bool:
    """git's own rule: a NUL byte within the first BINARY_SNIFF_BYTES.

    Borrowed rather than invented so this gate's idea of "binary" is the same
    one `git diff` and `git grep` already act on here.
    """
    return b"\0" in data[:BINARY_SNIFF_BYTES]


#: 🔴 THE GATE DOES NOT SCAN ITSELF, AND THE EXEMPTION IS A REAL HOLE STATED
#: PLAINLY RATHER THAN A TECHNICALITY. This file must CONTAIN a realistic
#: credential, a realistic private address, a realistic lab hostname and a
#: realistic registry reference, because those are its negative controls and a
#: control built from a textbook fixture proves nothing. Scanning itself would
#: therefore refuse itself — 24 findings, every one of them a control doing its
#: job — and the only two ways out are to weaken the controls or to exempt the
#: file. The controls are the thing that makes every clean report meaningful, so
#: the file is exempt.
#:
#: 🔴 WHAT THAT COSTS: a real leak pasted into THIS file is invisible to this
#: gate. Nothing here closes that. Review a diff to `tests/leakscan.py` by hand,
#: and treat a new entry in the sample lists as the place a real value is most
#: likely to arrive by copy-paste.
#:
#: ⚠ AN EXEMPT FILE IS STILL NAMED IN THE OUTPUT as a SKIPPED line, never
#: silently dropped. A count of files scanned cannot distinguish a clean tree
#: from a partly-read one, which is the ambiguity this accounting exists to
#: remove.
SKIP_FILES = {"tests/leakscan.py"}


def partition_tracked_files() -> tuple[list[Path], list[Skipped]]:
    """Split every enumerated file into exactly two buckets: scan, or skip.

    🔴 EVERY ENUMERATED FILE LANDS IN EXACTLY ONE OF THEM. There is no branch
    that quietly drops a file, so `scanned + skipped` equals the enumeration
    exactly — and `main` prints both counts and names every skip, so the
    reconciliation is visible in the OUTPUT rather than merely true in the code.

    🔴 UNREADABLE IS NOT CLEAN. A file the gate cannot open is the one case where
    neither bucket is honest, so it stops the run rather than being counted as
    skipped.
    """
    scan: list[Path] = []
    skipped: list[Skipped] = []
    for rel in enumerate_repo(ROOT):
        p = ROOT / rel
        if rel in SKIP_FILES:
            skipped.append(Skipped(rel, "the gate's own fixtures, exempt by name — "
                                        "review changes to it BY HAND"))
            continue
        if not p.is_file():
            skipped.append(Skipped(rel, "not a regular file (submodule, symlink or removed)"))
            continue
        try:
            with p.open("rb") as fh:
                head = fh.read(BINARY_SNIFF_BYTES)
        except OSError as e:
            print(f"leakscan: COULD NOT READ {rel}: {e}", file=sys.stderr)
            raise SystemExit(2)
        if is_binary(head):
            skipped.append(Skipped(
                rel, f"binary: a NUL byte within the first {BINARY_SNIFF_BYTES} bytes"))
            continue
        scan.append(p)
    return scan, skipped


# --------------------------------------------------------------------------
# CONTROLS
#
# 🔴 THE NEGATIVE CONTROLS ARE BUILT FROM REALISTIC DATA, NOT TEXTBOOK
# FIXTURES. A scanner tested only against `example.com` and `secret123` passes
# a real leak, and the canonical-example trap is exactly how a scanner ends up
# allowlisting the shapes it exists to catch. The STRUCTURE of each sample
# below is the real thing; the CONTENT is synthetic, because realistic content
# here would be the leak.
# --------------------------------------------------------------------------
NEGATIVE_CONTROLS = [
    ("private-hostname",
     'MUSTER_API_URL = "https://muster.workshed.lan/api/tasks"'),
    ("private-hostname",
     "    // the gateway resolves agent-7.devpod.internal on the cluster network"),
    ("registry-ref",
     'image: registry.workshed.lan/library/muster:0.4.2'),
    ("registry-ref",
     '  IMAGE = "harbor.internal.example.net:5000/agents/runner:2026.5.7"'),
    ("private-ip",
     "NODE = '192.168.50.250'  # the k3s node"),
    ("private-ip",
     "    endpoint: 172.20.4.9:30302"),
    ("credential",
     'Authorization: Bearer ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8'),
    ("credential",
     "    token = 'AGE-SECRET-KEY-1QQPQZRJ8NXW4K2VHM7YD3TG6LS9FCAE0BU5XP'"),
    ("operator-identity",
     "    repo = '/home/jrandom/workspace/muster'"),
    ("operator-identity",
     "maintainer: jrandom.dev@gmail.com"),
    ("denied-identifier",
     f'    cluster = "{DENY_CANARY_WORD}-ci-jx5fq"'),
    ("denied-identifier",
     f'    SCOPE_ENV = "{DENY_CANARY.upper().replace("-", "_")}_TMPFS"'),
    ("dated-incident",
     "# MEASURED 2026-09-01: the reconciler dropped 3 of 14 kickoffs under load"),
    ("dated-incident",
     "    the migration landed and the gate went green on 2026-08-20"),
    ("dated-incident",
     "# 2026-08-29: chief has never run on its configured model"),
]

POSITIVE_CONTROL = "trusted = '172.16.4.9'  # a real private address"

#: 🔴 CONTENT THAT MUST **NOT** BE REFUSED — these pin the rules' narrowness.
#: A false positive here is not cosmetic: it is how a security gate gets
#: disabled, and then the real findings ship alongside the noise.
ALLOWED_CONTROLS = [
    ('    "10.244.0.0/16",  # a pod CIDR: every pod in the cluster',
     "the standard Kubernetes pod-CIDR example"),
    ("MUSTER_API_URL=https://muster.example.com/api/tasks",
     "a documentation hostname"),
    ('image: ghcr.io/zacxdev/muster:sha-0123456789abcdef',
     "a PUBLIC registry — ghcr.io is where muster's own images would live, and "
     "refusing it would red every deployment example"),
    ("image: postgres:16-alpine",
     "an official Docker Hub image with no registry host at all"),
    ("resp = client.get('http://127.0.0.1:55432/healthz')",
     "loopback: not private topology, and it is what the compose file serves"),
    ("TEST_DSN := postgres://muster:muster@127.0.0.1:55432/muster_test?sslmode=disable",
     "the throwaway compose credential, which is published on purpose and "
     "opens nothing"),
    ('go run ./cmd/muster-migrate  # under $HOME, spelled portably',
     "a home-relative path written portably rather than as an absolute one"),
    ("nix run github:ZacxDev/muster -- --help",
     "the repository's OWN public URL: the owner handle is not denied, and "
     "denying it would red every import path in the tree"),
    (f'    assert normalise("{DENY_CANARY_WORD}s") == "{DENY_CANARY_WORD}s"',
     "a longer WORD that merely STARTS with a one-word denied entry: one token, "
     "one candidate, and it is not the entry"),
    (f'    scope = tmp / "{DENY_CANARY}d"   # one letter longer',
     "a token in which a denied COMPOUND is a literal substring — a substring "
     "is not a segment run"),
    (f'    legacy = "scoped-{DENY_CANARY_WORD}"',
     "a denied entry in the TAIL of a compound. The walk is over PREFIXES, so "
     "this is a declared LIMIT of the rule rather than a false negative to fix"),
    ("# a home lab, two boxes and a switch",
     "an English phrase whose words are not a denied identifier"),
    ('auditTimeLayout = "2006-01-02T15:04:05-07:00"',
     "Go's `time` reference instant, which every layout string spells"),
    (f'// parsed at {GO_REFERENCE_DATE}T15:04:05-07:00, the reference instant',
     "Go's reference instant after a preposition, timestamp and all — the one "
     "shape the `(?![\\d-])` boundary exists for"),
    (f'# MEASURED {SYNTHETIC_DATE_YEAR}-01-05 in the fixture world: the row reads exactly this',
     f"a year-{SYNTHETIC_DATE_YEAR} date in a full CLAIM shape — the synthetic "
     f"date this gate steers toward, so the remedy must not be refused"),
    (f'#   - {SYNTHETIC_DATE_YEAR}-01-05: the bullet this fixture appends',
     "the same allowance reached through the CONTINUATION shape, which is a "
     "different pattern and needs its own reach"),
    ("# MEASURED at `465f8a3`: a single CREATE DATABASE took over 90 seconds",
     "a measurement pinned to a COMMIT rather than a day, which is the "
     "reproducible form and is what this rule steers toward"),
    ("    Measured on this repository: one package went from 24s to 522s",
     "a measurement with the DATE already dropped — the remedy must pass"),
]


def self_test() -> int:
    ok = True

    print("== SET SIZE: the denylist cannot shrink unnoticed ==")
    if len(DENIED_IDENTIFIER_DIGESTS) == DENIED_COUNT:
        print(f"  PASS  {DENIED_COUNT} denied identifiers")
    else:
        print(f"  FAIL  DENIED_IDENTIFIER_DIGESTS holds "
              f"{len(DENIED_IDENTIFIER_DIGESTS)}, DENIED_COUNT says {DENIED_COUNT}. "
              f"A digest removed takes a name off the gate with no other symptom.")
        ok = False

    print("== POSITIVE CONTROL: the matcher can produce a non-zero count ==")
    hits = scan_text(POSITIVE_CONTROL, "<positive-control>")
    if hits:
        print(f"  PASS  {len(hits)} finding(s) on a line that certainly contains one")
    else:
        print("  FAIL  0 findings — the matcher is wired to nothing, so every "
              "clean report below is meaningless")
        ok = False

    print("== NEGATIVE CONTROL: each rule refuses a REALISTIC sensitive string ==")
    covered = set()
    for expected, sample in NEGATIVE_CONTROLS:
        names = {f.rule for f in scan_text(sample, "<negative-control>")}
        if expected in names:
            print(f"  PASS  {expected:20} refused")
            covered.add(expected)
        else:
            print(f"  FAIL  {expected:20} NOT refused — rule is inert")
            print(f"        sample: {sample[:70]}")
            ok = False

    # 🔴 EVERY RULE MUST HAVE A NEGATIVE CONTROL. Without this, a rule added
    # with no control is a rule nobody has watched go red — which is the state
    # this whole module exists to refuse.
    declared = {n for n, _, _ in RULES} | {"private-ip", "denied-identifier",
                                           "dated-incident"}
    uncovered = declared - covered
    if uncovered:
        print(f"  FAIL  no negative control for: {sorted(uncovered)} — those "
              f"rules have never been watched to go red")
        ok = False

    print("== NARROWNESS: legitimate content must NOT be refused ==")
    for sample, why in ALLOWED_CONTROLS:
        found = scan_text(sample, "<allowed>")
        if not found:
            print(f"  PASS  allowed: {why}")
        else:
            print(f"  FAIL  FALSE POSITIVE on {why}")
            print(f"        sample: {sample[:70]}")
            for f in found:
                print(f"        matched [{f.rule}]")
            ok = False

    return 0 if ok else 2


def main(argv: list[str] | None = None) -> int:
    """🔴 `argv` IS A PARAMETER SO THIS IS DRIVABLE FROM A TEST.

    With `parse_args()` reading `sys.argv` unconditionally, calling `main()`
    under a test runner makes argparse see the RUNNER's arguments and exit 2 —
    so the only tests that could exist would be structural ones about the
    functions underneath, and the actual verdict a user sees would be
    unreachable. `None` still means `sys.argv` for the real entry point.
    """
    ap = argparse.ArgumentParser(
        description=(__doc__ or "refuse sensitive content").splitlines()[0])
    ap.add_argument("--self-test", action="store_true")
    ap.add_argument("--quiet", action="store_true")
    args = ap.parse_args(argv)

    if args.self_test:
        return self_test()

    # The controls run on EVERY invocation: a scan whose matcher is broken must
    # not be able to report a reassuring 0.
    if self_test() != 0:
        print("\nleakscan: COULD NOT VOUCH — a control misbehaved. Exit 2, not a "
              "pass.", file=sys.stderr)
        return 2
    print()

    files, skipped = partition_tracked_files()
    if not files:
        print("leakscan: COULD NOT RUN — enumerated 0 files. A zero here is a "
              "broken enumeration, not a clean tree.", file=sys.stderr)
        return 2

    # 🔴 NAME EVERY SKIP, ALWAYS — INCLUDING UNDER `--quiet`. An unread file is
    # the one thing a leak gate's output must never leave implicit: a count of
    # files scanned cannot distinguish a clean tree from a partly-read one.
    for s in skipped:
        print(f"  SKIPPED  {s}")

    findings: list[Finding] = []
    for f in files:
        try:
            findings.extend(
                scan_text(f.read_text(encoding="utf-8", errors="replace"),
                          str(f.relative_to(ROOT))))
        except OSError as e:
            print(f"leakscan: COULD NOT READ {f}: {e}", file=sys.stderr)
            return 2

    print(f"== UNDER TEST: {len(files)} file(s) scanned, {len(skipped)} skipped ==")
    if findings:
        for f in findings:
            print(f"  {f}")
        print(f"\nleakscan: {len(findings)} finding(s) across {len(files)} file(s) "
              f"— REFUSING")
        return 1

    if not args.quiet:
        print(f"  0 findings across {len(files)} file(s)")
        print("\nleakscan: clean — and the controls above are what make that a "
              "measurement rather than a claim")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

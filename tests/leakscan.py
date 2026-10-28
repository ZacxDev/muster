#!/usr/bin/env python3
"""Refuse SENSITIVE content in this repository.

muster was extracted from a private, single-operator deployment. The source
material carries live hostnames, private addresses, a container registry, real
task numbers and dated incident narration. This gate exists so none of that can
come back into a public repository without a hand audit being the only thing
standing between it and the internet.

🔴 THIS GATE IS NOT A SUBSTITUTE FOR REVIEW. A clean run means "the rules held",
not "this is safe to publish": prose describing a private deployment spells no
value any rule extracts.

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
  * SELF-AUDIT — the file this gate EXEMPTS from the scan (see SKIP_FILES) is
    audited by VALUE: every address, hostname, registry reference, credential,
    denied name and dated stamp it spells must be objectively unroutable,
    objectively synthetic, or DECLARED with the reason it is safe. The
    exemption was unaudited for the whole life of the private repository, and
    what hid in it was the LAN address of a real single-node cluster, sitting
    in a negative control that read exactly like its synthetic neighbours.

Exit codes:
  0  no findings (and every control behaved)
  1  findings — sensitive content is present
  2  the gate itself could not run, or a control misbehaved. NOT a pass.
"""

from __future__ import annotations

import argparse
import hashlib
import hmac
import json
import os
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

# The three RFC 1918 blocks THEMSELVES — "10.0.0.0/8", "172.16.0.0/12",
# "192.168.0.0/16". A block's NAME is not a host inside it and says nothing
# about anybody's network: it is the same string in every firewall rule ever
# published. The Kubernetes driver's NetworkPolicy has to spell all three, as the
# ranges an isolated agent may NOT reach.
#
# 🔴 THE BASE ADDRESS IS ALLOWED ONLY WITH ITS OWN MASK, AND THAT IS THE WHOLE
# DIFFERENCE BETWEEN A BLOCK'S NAME AND SOMEBODY'S SUBNET. `10.0.0.0/8` is RFC
# 1918; `10.0.0.0/24` and `192.168.0.0/24` are subnet definitions — topology —
# and so is the bare address with no mask at all. The first draft of this
# allowance listed the three base addresses in DOC_ADDRESSES, which matches the
# dotted quad and ignores what follows, so all of those passed.
#
# 🔴 AND THE MATCH IS ON THE WHOLE ADDRESS. A host that merely shares a base's
# first three octets is still refused. NEGATIVE_CONTROLS pins each of these.
DOC_BLOCKS = {
    "10.0.0.0": "/8",
    "172.16.0.0": "/12",
    "192.168.0.0": "/16",
}


def _is_named_block(line: str, m: "re.Match[str]") -> bool:
    """Whether the address `m` matched in `line` is an RFC 1918 block's NAME:
    its base address followed by exactly its own mask."""
    mask = DOC_BLOCKS.get(m.group(0))
    if mask is None:
        return False
    rest = line[m.end():]
    # The mask, and then not another digit: "/8" must not excuse "/80"-shaped
    # text, and "/1" must not be read as a prefix of "/16".
    return rest.startswith(mask) and not rest[len(mask):len(mask) + 1].isdigit()

_PRIVATE_IP = re.compile(
    r"\b(?:10\.\d{1,3}\.\d{1,3}\.\d{1,3}"
    r"|192\.168\.\d{1,3}\.\d{1,3}"
    r"|172\.(?:1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3})\b"
)

# --------------------------------------------------------------------------
# DENIED IDENTIFIERS — the real project, repository, cluster, host and domain
# names the extraction removed from this tree, so they cannot come back.
#
# 🔴 STORED AS KEYED DIGESTS (HMAC-SHA256), AND THE KEY IS THE WHOLE DESIGN. A
# denylist written out in plaintext PUBLISHES exactly the names it exists to
# remove. An UNSALTED hash does not fix that for short names: this list used to
# be bare SHA-256, and one guess list of plausible words reversed every entry.
# Keyed, a digest says nothing to anyone without the key, so the list in
# DENYLIST_FILE can be public while the names stay private.
#
# The key comes from the environment (LEAKSCAN_KEY_ENV). In CI it is the
# repository Actions secret of the same name. It is never in this tree.
#
#   * CI, on this repository's own pushes and pull requests: a missing key FAILS
#     CLOSED (exit 2). A gate that quietly skips its own check reports exactly
#     what a clean tree reports.
#   * A pull request from a FORK: GitHub does not pass secrets to it, so the
#     check is skipped with a WARNING. The run after merge, on `main`, has the
#     key and fails closed.
#   * Locally, with no key: WARNING, and only this check is skipped. Every
#     other rule still runs.
#
# 🔴 WHAT THAT COSTS, STATED RATHER THAN HIDDEN: nobody can verify from inside
# this repository that the digests are digests of the RIGHT strings. That is
# the property being bought. So the controls prove the MECHANISM end to end
# (tokenise → walk → HMAC → refuse) with synthetic sentinels that are HMACed
# with the same key at run time and travel the identical code path, and
# DENIED_COUNT pins the size of the list so it cannot shrink unnoticed.
#
# ⚠ MATCHING IS "ANYWHERE INSIDE AN IDENTIFIER". Each identifier-shaped token is
# lowercased, `_` is folded to `-`, and it is split into `-` segments. Then:
#
#   * a ONE-WORD entry matches as a substring of any WORD of a segment, so a
#     name inside a header (`X-<name>-Token`), at the tail of a compound
#     (`<host>-<name>`), glued into a longer word (`<name>ctl`) or a camelCase
#     word of an identifier (`My<Name>Client`) is caught;
#   * a COMPOUND entry matches as any contiguous run of WHOLE segments.
#
# 🔴 A MIXED-CASE SEGMENT IS SPLIT AT ITS camelCase BOUNDARIES FIRST, AND THAT IS
# THE FALSE-POSITIVE FIX, NOT A LOOPHOLE TO CLOSE. Without it a denied word
# turned up inside ordinary Go names whose camelCase words merely spell it
# across a boundary (a test name, a const), and a
# gate that refuses ordinary names gets switched off. A name written in
# camelCase as SEVERAL words is still caught, as a run of whole words joined
# together: a run starts and ends on a boundary, so it is matched whole, and an
# ordinary name whose words only spell an entry across a boundary is not.
#
# Every substring of DENIED_MIN_LEN..DENIED_MAX_LEN characters is HMACed, which
# is why a one-word entry must fall in that range (the rehash script refuses one
# that does not), and why the length of an entry is not revealed by the walk.
#
# ⚠ THE ILLUSTRATIONS USE THE SENTINELS, NEVER A REAL ENTRY:
#
#   canarytoken-ci-jx5fq      FIRES  a one-word entry, as a segment
#   X-Canarytoken-Id          FIRES  ... in the middle of a compound
#   legacy-canarytoken        FIRES  ... at its tail
#   canarytokens              FIRES  ... glued into a longer word
#   MyCanarytokenClient       FIRES  ... as one camelCase word
#   CANARYTOKEN_TEST_TMPFS    FIRES  `_`→`-` and case are folded
#   newCanaryTokenClient      FIRES  ... as a run of whole camelCase words
#   isCanaryTokenized         MISS   the run would end inside a word
#   canary-token              MISS   a one-word entry split by a separator is a
#                                    declared LIMIT of the rule
#   redacted-canary-scoped    MISS   a compound entry needs WHOLE segments
#
# ⚠ PUBLIC_WORDS are cut out of a segment before the substring walk: public
# names the repository must always be able to spell, such as the module path's
# owner handle. The walk never sees them, whatever the list holds.
#
# TO CHANGE THE LIST: put the plaintext names, one per line, in a file OUTSIDE
# this repository and run
#   MUSTER_LEAKSCAN_KEY=… python3 scripts/leakscan-rehash.py < names.txt \
#       > tests/leakscan_denylist.txt
# then set DENIED_COUNT. Removing a name has to be argued for in a diff.
# --------------------------------------------------------------------------
LEAKSCAN_KEY_ENV = "MUSTER_LEAKSCAN_KEY"

#: A shorter key is a typo, not a secret. `openssl rand -hex 32` gives 64.
LEAKSCAN_KEY_MIN_BYTES = 32

DENYLIST_FILE = ROOT / "tests" / "leakscan_denylist.txt"

#: 🔴 PINNED SO THE LIST CANNOT SHRINK UNNOTICED. A digest quietly deleted takes
#: a name off the gate with no other symptom: the tree still scans clean, which
#: is what it would do if the name had really been removed. An EMPTY list is
#: the placeholder, and it fails this check on purpose.
DENIED_COUNT = 18

#: The substring walk's bounds. A one-word entry outside them could never match.
DENIED_MIN_LEN = 4
DENIED_MAX_LEN = 32

#: Public names cut out before the walk (see the header above): the
#: repository owner's handle, which the module path and the image namespace
#: spell in every import.
PUBLIC_WORDS = ("zacxdev",)

#: Files the denied-identifier rule does not read, each with the reason. Every
#: other rule still applies to them, and a path here that the enumeration does
#: not find fails the run, so an entry cannot outlive its file.
DENIED_EXEMPT_FILES = {
    "internal/focus/dict/words.txt":
        "a third-party English word list (SCOWL, see dict/COPYRIGHT), embedded "
        "verbatim: its lines are dictionary words, not identifiers anybody chose",
}

DENY_CANARY = "redacted-canary-scope"
DENY_CANARY_WORD = "canarytoken"

#: A denied name that is NOT permitted even in an exempt file: the negative
#: control for the self-audit's denied branch. ASSEMBLED, never spelled: a
#: literal here would be a value the audit must refuse, in the file it reads.
DENY_CANARY_UNPERMITTED = "redacted-canary-" + "unpermitted"

#: The sentinels are HMACed with the run's own key, so they reach the matcher by
#: the same path as the real entries.
DENY_SENTINELS = (DENY_CANARY_WORD, DENY_CANARY, DENY_CANARY_UNPERMITTED)

_NAME = re.compile(r"^[a-z0-9]+(?:-[a-z0-9]+)*$")
_HEX64 = re.compile(r"^[0-9a-f]{64}$")


class DenylistError(Exception):
    """The denylist or its key cannot be used. Never a pass."""


def normalise_name(name: str) -> str:
    """The one spelling a name is HMACed in: lowercase, `_` folded to `-`."""
    return name.strip().lower().replace("_", "-")


def name_problem(name: str) -> str | None:
    """Why a NORMALISED name cannot be a denylist entry, or None if it can."""
    if not _NAME.match(name):
        return "only letters, digits and single `-`/`_` separators are allowed"
    if "-" not in name and not DENIED_MIN_LEN <= len(name) <= DENIED_MAX_LEN:
        return (f"a one-word entry must be {DENIED_MIN_LEN} to {DENIED_MAX_LEN} "
                f"characters, or the substring walk can never reach it")
    return None


def leakscan_key(env=None) -> bytes | None:
    """The HMAC key from the environment, or None when it is not set.

    Surrounding whitespace is dropped, so a key file with a trailing newline
    and the same value pasted as a secret give the same key.
    """
    env = os.environ if env is None else env
    raw = env.get(LEAKSCAN_KEY_ENV, "").strip()
    if not raw:
        return None
    key = raw.encode()
    if len(key) < LEAKSCAN_KEY_MIN_BYTES:
        raise DenylistError(
            f"{LEAKSCAN_KEY_ENV} is set but shorter than {LEAKSCAN_KEY_MIN_BYTES} "
            f"bytes. Generate one with `openssl rand -hex 32`")
    return key


def key_required(env=None) -> tuple[bool, str]:
    """Whether a missing key is a FAILURE here, and why.

    🔴 IT FAILS CLOSED. Any GitHub Actions run is treated as needing the key
    unless GitHub's own event payload shows a pull request from ANOTHER
    repository — the one case where GitHub withholds secrets, so the key cannot
    be there whatever anybody does. A payload that cannot be read counts as
    "this repository", not as a fork.
    """
    env = os.environ if env is None else env
    if env.get("GITHUB_ACTIONS") != "true":
        return False, "a local run"
    if env.get("GITHUB_EVENT_NAME") not in ("pull_request", "pull_request_target"):
        return True, f"a CI {env.get('GITHUB_EVENT_NAME') or 'run'} on this repository"
    try:
        with open(env["GITHUB_EVENT_PATH"], encoding="utf-8") as fh:
            payload = json.load(fh)
        base = payload["repository"]["full_name"]
        head_repo = payload["pull_request"]["head"]["repo"]
        head = head_repo["full_name"] if head_repo else None
    except (KeyError, TypeError, OSError, ValueError):
        return True, "a CI pull request whose event payload could not be read"
    if head != base:
        return False, "a pull request from a fork, which GitHub gives no secrets"
    return True, "a CI pull request from this repository"


def load_denylist(path: Path = DENYLIST_FILE) -> list[str]:
    """The committed digests. `#` lines and blank lines are ignored.

    🔴 A MALFORMED LINE STOPS THE RUN. A line that is not a digest matches
    nothing, which is the silent shrink DENIED_COUNT exists to catch.
    """
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except OSError as e:
        raise DenylistError(f"cannot read {path}: {e}") from e
    out: list[str] = []
    for n, line in enumerate(lines, start=1):
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        if not _HEX64.match(line):
            raise DenylistError(f"{path.name}:{n} is not a 64-hex-digit digest")
        out.append(line)
    if len(set(out)) != len(out):
        raise DenylistError(f"{path.name} lists the same digest twice")
    return out


_CAMEL_BOUNDARY = re.compile(r"(?<=[a-z0-9])(?=[A-Z])|(?<=[A-Z])(?=[A-Z][a-z])")


def camel_words(segment: str) -> list[str]:
    """A MIXED-case segment split at its camelCase boundaries; any other
    segment whole. `XMLHttpRequest` is three words, `MyWidgetClient` three,
    and `widgetctl` or `WIDGET` one."""
    if segment.islower() or segment.isupper():
        return [segment]
    return [w for w in _CAMEL_BOUNDARY.split(segment) if w]


class Denylist:
    """A keyed matcher over the committed digests plus the sentinels."""

    def __init__(self, key: bytes, digests: list[str] | set[str]):
        self._mac = hmac.new(key, digestmod=hashlib.sha256)
        self._digests = set(digests) | {self.digest(s) for s in DENY_SENTINELS}
        self._cache: dict[str, list[str]] = {}

    def digest(self, name: str) -> str:
        m = self._mac.copy()
        m.update(normalise_name(name).encode())
        return m.hexdigest()

    def _token(self, token: str) -> list[str]:
        hits = self._cache.get(token)
        if hits is not None:
            return hits
        hits = []
        candidates: list[str] = []
        norm = normalise_name(token)
        parts = norm.split("-")
        # The public words are cut out BEFORE the camelCase split, or a public
        # handle written in camelCase would be split into a denied word.
        cut = token.replace("_", "-")
        for word in PUBLIC_WORDS:
            low = cut.lower()
            at = low.find(word)
            while at != -1:
                cut = cut[:at] + "-" * len(word) + cut[at + len(word):]
                at = low.find(word, at + len(word))
        segs = [[w.lower() for w in camel_words(seg)] for seg in cut.split("-")]
        words = [w for seg in segs for w in seg]
        # One-word entries written in camelCase: every run of two or more WHOLE
        # camelCase words, joined. A run starts and ends on a boundary, so it is
        # matched whole, never as a substring.
        for seg in segs:
            for i in range(len(seg)):
                for j in range(i + 2, len(seg) + 1):
                    candidates.append("".join(seg[i:j]))
        # Compound entries: every contiguous run of two or more whole segments.
        for i in range(len(parts)):
            for j in range(i + 2, len(parts) + 1):
                candidates.append("-".join(parts[i:j]))
        # One-word entries: every substring of every WORD of every segment.
        for piece in words:
            n = len(piece)
            for size in range(DENIED_MIN_LEN, min(n, DENIED_MAX_LEN) + 1):
                for start in range(n - size + 1):
                    candidates.append(piece[start:start + size])
        for c in candidates:
            if c not in hits and self.digest(c) in self._digests:
                hits.append(c)
        self._cache[token] = hits
        return hits

    def match(self, line: str) -> list[str]:
        out: list[str] = []
        for m in _IDENTIFIER.finditer(line):
            for hit in self._token(m.group(0)):
                if hit not in out:
                    out.append(hit)
        return out


#: The run's matcher. None = no key, and the denied-identifier check is off;
#: whether that is a warning or a failure is `key_required`'s call, made in
#: `self_test`.
ACTIVE_DENYLIST: Denylist | None = None

_IDENTIFIER = re.compile(r"[A-Za-z][A-Za-z0-9]*(?:[-_][A-Za-z0-9]+)*")


def denied_identifiers(line: str) -> list[str]:
    """Every denied identifier this line spells, in order of first appearance."""
    if ACTIVE_DENYLIST is None:
        return []
    return ACTIVE_DENYLIST.match(line)


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
# stamp is often a full RFC 3339 instant — `at 1999-08-23T00:37Z` — and `\b`
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
        #
        # ⚠ ONE NAME IS EXCUSED, EXACTLY: `CLAUDE.local.md`, the Claude Code
        # CLI's personal memory file, which the machine job API names because
        # it replaces it (internal/agentprovision jobworkspace.go). It is a
        # FILE NAME, not a host, and both alternatives below would read it as
        # one. The lookahead excuses a match only where it STARTS at that exact
        # spelling AND a word boundary follows the `md`: so any other
        # `*.local.md` is still refused, and so is the name run on into more
        # word characters (`…mdx…`). A non-word character after the `md` — a
        # dot or a hyphen, as in `….md.example` or `….md-x` — IS a word
        # boundary, so those spellings are excused too. (Pinned by
        # NEGATIVE_CONTROLS and ALLOWED_CONTROLS.)
        r"\b(?!CLAUDE\.local\.md\b)[A-Za-z0-9][A-Za-z0-9-]*\.(?:lan|local|internal|home(?:lab)?)\b"
        r"|\b(?!CLAUDE\.local\.md\b)[A-Za-z0-9][A-Za-z0-9-]*\.(?:lan|local|internal)\.[A-Za-z]{2,}\b",
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
            if m.group(0) not in DOC_ADDRESSES and not _is_named_block(line, m):
                out.append(Finding(
                    path, n, "private-ip", line,
                    f"{m.group(0)} is a real private address — network topology. "
                    f"Use RFC5737 TEST-NET (192.0.2.0/24) for examples",
                ))
        # The last two rules are per-OCCURRENCE for the same reason, and the
        # message has to say WHICH.
        idents = [] if path in DENIED_EXEMPT_FILES else denied_identifiers(line)
        for ident in idents:
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
#: 🔴 WHAT THAT USED TO COST, AND WHAT WAS ACTUALLY LOST: this note used to end
#: "a real leak pasted into THIS file is invisible to this gate. Nothing here
#: closes that. Review a diff by hand." The hand review is what happened, and it
#: is what failed: a `private-ip` negative control carried the LAN address of a
#: real single-node cluster from the first commit of this file, through the
#: pre-publication review, and out into public history. The rule matched that
#: shape, the scan would have refused the line, and the docstring above asserted
#: the fixtures were synthetic — the file was simply never read by anything.
#:
#: 🔴 SO THE EXEMPTION IS NOW NARROW: exempt from the SCAN, audited by VALUE.
#: `audit_exempt_files` below reads every file in this set and holds each
#: sensitive value it spells to one of three bars, failing closed on anything
#: new. What it still accepts is written out there rather than implied here.
#:
#: ⚠ AN EXEMPT FILE IS STILL NAMED IN THE OUTPUT as a SKIPPED line, never
#: silently dropped. A count of files scanned cannot distinguish a clean tree
#: from a partly-read one, which is the ambiguity this accounting exists to
#: remove.
SKIP_FILES = {"tests/leakscan.py"}


# --------------------------------------------------------------------------
# THE EXEMPT FILES ARE AUDITED BY VALUE
#
# 🔴 A SCAN EXEMPTION IS A HIDING PLACE, AND SOMETHING HID IN THIS ONE. The
# defect this section exists for was not a broken rule: it was that the one file
# nothing reads is also the one file that must CONTAIN realistic sensitive
# strings, so a real value pasted into the sample lists looks exactly like its
# synthetic neighbours and nothing — not the gate, not the docstring, not the
# pre-publication review — is positioned to tell the difference.
#
# 🔴 THE AUDIT IS ATTACHED TO `SKIP_FILES` ITSELF, NOT TO THIS FILE'S NAME, so
# an exemption can never again be blind: adding a file to that set enrols it
# here, and the audit fails if it cannot read what the set names.
#
# Every value an exempt file spells must clear one of three bars:
#
#   1. OBJECTIVELY UNROUTABLE OR ALREADY ARGUED — a reserved documentation or
#      loopback address needs no argument from anybody (RFC 5737, RFC 1122), and
#      neither does one of the conventional cluster CIDRs already argued for in
#      DOC_ADDRESSES, nor the base address of an RFC 1918 block argued for in
#      DOC_BLOCKS.
#      ⚠ THE LAST OF THOSE IS WIDER HERE THAN IN THE SCAN, AND KNOWINGLY. The
#      scan allows a block base only with its own mask; this audit is held to
#      VALUES, and the value is the dotted quad, so it passes the base whatever
#      follows it. It has to: this file's own negative controls spell the bases
#      with the WRONG mask, on purpose. The cost is that a real subnet written
#      on a block base (`192.168.0.0/24`) inside an exempt file passes this
#      audit — review such a line by hand.
#   2. OBJECTIVELY SYNTHETIC — a dated stamp older than this project can be, and
#      (for denied names) this module's own sentinels. Neither needs judgement.
#   3. DECLARED — everything else must appear in EXEMPT_FIXTURE_VALUES with the
#      reason it is safe. This is the bar that needs a human, and it is the one
#      that would have caught the address: writing `192.168.<real>.<real>` into a
#      table whose every row asserts "this is not anybody's" is a claim somebody
#      has to make in a diff, rather than one more plausible-looking sample line.
#
# 🔴 WHAT THIS STILL ACCEPTS — STATED, BECAUSE A GUARD THAT READS AS COVERAGE
# WHILE PROVIDING NONE IS WORSE THAN NO GUARD:
#
#   * A REAL VALUE DECLARED ANYWAY. Nothing here can tell a real RFC1918
#     address, lab hostname or credential from an invented one; it can only
#     force it to be written down twice, in a table that says why it is safe.
#     The one class where that is not true is the denied names: the only way to
#     declare one is to spell it, in plaintext, in a public file — which is a
#     diff nobody merges by accident.
#   * PROSE. A real incident narrated in a comment or in a fixture's TEXT leaks
#     without spelling any value this audit extracts. That is how a real agent's
#     name and its real symptom rode in beside the address — both in fixtures,
#     only one of them a value. Read fixture text, not only fixture values.
#   * IPv6, and the gate has no IPv6 rule at all: a ULA (`fd00::/8`) address is
#     invisible to every rule above, so it is invisible here too.
#   * EVERY SCANNED FILE'S FIXTURES. This audits the files the gate SKIPS, which
#     is where the blindness was. A Go test fixture is covered by the scan — by
#     the rules the scan has, which is a different and narrower claim than "it
#     contains nothing real".
# --------------------------------------------------------------------------

#: Address prefixes reserved for documentation or loopback: nobody can be
#: running anything on them, so they need no declaration.
RESERVED_ADDRESS_PREFIXES = (
    "192.0.2.",      # RFC 5737 TEST-NET-1
    "198.51.100.",   # RFC 5737 TEST-NET-2
    "203.0.113.",    # RFC 5737 TEST-NET-3
    "127.",          # RFC 1122 loopback
)

#: 🔴 WHY THE TABLE HAS PRIVATE ADDRESSES IN IT AT ALL, WHICH IS THE FIRST
#: QUESTION TO ASK OF IT: `_PRIVATE_IP` matches 10/8, 172.16/12 and 192.168/16
#: and NOTHING ELSE, so a TEST-NET address in the `private-ip` negative control
#: makes that control inert and the rule stops being watched to go red. The
#: controls therefore have to spell RFC1918 addresses. The only defence left is
#: choosing ones that are demonstrably nobody's, so each was grepped against the
#: deployment muster was extracted from and matched zero files — `10.255.` and
#: `172.20.4.9` and `172.16.4.9`, zero each, against 109 files for the address
#: this replaced.
#:
#: 🔴 EACH ROW IS A CLAIM SOMEBODY MADE. A row is not evidence a value is
#: synthetic; it is a record that a human asserted it, with a reason, where a
#: reviewer can disagree. That is the whole of what this bar buys.
EXEMPT_FIXTURE_VALUES: dict[str, str] = {
    # -- addresses the private-ip rule needs, none of them anybody's ----------
    "10.255.255.1":
        "the last /16 of RFC1918's 10/8 — nothing provisions a node there, and "
        "it matches zero files in the source deployment",
    "172.20.4.9":
        "inside 172.16/12 but outside every subnet the source deployment uses; "
        "zero files",
    "172.16.4.9":
        "the POSITIVE_CONTROL address, same argument; zero files",
    "172.16.0.9":
        "a host sharing three octets with an allowlisted BLOCK BASE "
        "(DOC_BLOCKS), which is the control that the allowance is the whole "
        "base address and not a prefix; zero files in the source deployment",
    # -- hostnames, all on invented or IANA-reserved domains ------------------
    "notes.local":
        "the look-alike negative control for the one excused file name: an "
        "invented `.local` name, zero occurrences in the source deployment",
    "CLAUDE.local":
        "the excused file name's look-alike negative control "
        "(`CLAUDE.local.mdx.example`): a public tool's file-name stem, not a "
        "host, under an RFC 2606 domain",
    "workshed.lan":
        "an invented lab domain: zero occurrences in the source deployment, "
        "whose lab hostnames are spelled differently",
    "devpod.internal":
        "a generic cluster-internal name — `devpod` is a public product name, "
        "not a private identifier, and this hostname resolves nowhere real",
    "registry.internal":
        "a generic registry hostname under `.internal`, itself under "
        "`example.net` (RFC 2606). Renamed during the sweep: the label it used "
        "before named a specific product, and matched the first label of the "
        "source deployment's own registry hostname",
    # -- registry references: private-registry SHAPE, invented host ----------
    "registry.workshed.lan/library/muster:0.4.2":
        "the invented lab domain above, carrying a plausible tag",
    "registry.internal.example.net:5000/agents/runner:2000.1.2":
        "host:port plus a calver tag, which is the shape that must be refused. "
        "The tag is a year-2000 one on purpose; it was a real image tag from "
        "the source deployment until the sweep",
    # -- credentials: real SHAPE, and each is structurally not a key ---------
    "-----BEGIN CERTIFICATE-----":
        "the credential rule's own pattern literal, not a certificate: there is "
        "no key material after it anywhere in this file",
    "Bearer ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8":
        "a token of the right shape typed as a keyboard pattern (A1b2C3...), "
        "which no generator emits",
    "AGE-SECRET-KEY-1QQPQZRJ8NXW4K2VHM7YD3TG6LS9FCAE0BU5XP":
        "53 characters where a real age key is 74 — structurally too short to "
        "decrypt anything, whatever it looks like",
    # -- operator identity: the canonical placeholder person -----------------
    "/home/jrandom/":
        "J. Random Hacker, the placeholder user; no such account exists",
    "jrandom.dev@gmail.com":
        "the same placeholder as an address",
}

#: 🔴 PINNED SO A ROW CANNOT ARRIVE AS ONE MORE LINE IN A LONG TABLE. Adding a
#: value is a two-line diff, and the second line is this number — which is the
#: difference between a reviewer skimming a table and a reviewer being told the
#: table grew.
EXEMPT_FIXTURE_VALUE_COUNT = 16

#: Dated stamps in an exempt file must predate this year. A control cannot use
#: the year-SYNTHETIC_DATE_YEAR convention the rule steers toward, because the
#: rule SKIPS that year and the control would be inert — so it uses a date from
#: before this project could have observed anything.
EXEMPT_DATE_YEAR_CEILING = 2010

#: 🔴 THE POSITIVE CONTROL FOR THE AUDIT ITSELF: a floor under how many
#: sensitive values it must EXTRACT. These files are the gate's fixture lists;
#: an audit that reports "nothing undeclared" having extracted 0 values has read
#: nothing, and is indistinguishable in the output from a clean one. Measured on
#: this tree at the commit that added this line: 49.
EXEMPT_AUDIT_MIN_VALUES = 35

#: 🔴 WIDER THAN `_PRIVATE_IP` ON PURPOSE — every dotted quad, not only the
#: private ranges. A real PUBLIC address names a real host just as precisely as a
#: private one, and the scan has no rule for public addresses at all, so the
#: scan's rule set cannot be the whole of what an exempt file is held to.
_ANY_IPV4 = re.compile(r"\b\d{1,3}(?:\.\d{1,3}){3}\b")


def sensitive_values(text: str) -> list[tuple[int, str, str]]:
    """Every sensitive-looking VALUE in `text`, as (line, rule, value).

    Per-OCCURRENCE, and the matched substring rather than the line: a verdict
    about a LINE cannot be held to a table of values, and "this line contains
    something" is exactly the granularity that let one real value sit in a list
    of synthetic ones.
    """
    out: list[tuple[int, str, str]] = []
    for n, line in enumerate(text.splitlines(), start=1):
        for name, rx, _why in _COMPILED:
            for m in rx.finditer(line):
                out.append((n, name, m.group(0)))
        for m in _ANY_IPV4.finditer(line):
            out.append((n, "address", m.group(0)))
        for ident in denied_identifiers(line):
            out.append((n, "denied-identifier", ident))
        for stamp in dated_incident_stamps(line):
            out.append((n, "dated-incident", stamp))
    return out


def undeclared(rule: str, value: str) -> str | None:
    """Why `value` may not appear in an exempt file — or None if it may."""
    if rule == "denied-identifier":
        if value in (DENY_CANARY, DENY_CANARY_WORD):
            return None
        return ("a DENIED identifier. Only this module's own sentinels may "
                "appear in an exempt file, and there is no declaration that "
                "would make a real one acceptable — writing it down would BE "
                "the leak the digest denylist exists to prevent")
    if rule == "dated-incident":
        if int(value[:4]) < EXEMPT_DATE_YEAR_CEILING:
            return None
        return (f"a dated stamp from {value[:4]}. A control needs a date the "
                f"rule will not skip, so it cannot use the year-"
                f"{SYNTHETIC_DATE_YEAR} convention — use a pre-"
                f"{EXEMPT_DATE_YEAR_CEILING} one, which cannot pin an "
                f"observation to a day on any real deployment")
    # ⚠ A block base passes HERE without its mask: this audit is held to VALUES,
    # and the value is the dotted quad. The scan above is what requires the mask.
    if rule == "address" and (value.startswith(RESERVED_ADDRESS_PREFIXES)
                              or value in DOC_ADDRESSES
                              or value in DOC_BLOCKS):
        return None
    if value in EXEMPT_FIXTURE_VALUES:
        return None
    return ("not declared in EXEMPT_FIXTURE_VALUES. A file exempt from the scan "
            "may only spell a sensitive value the table names, with the reason "
            "it is safe written beside it")


def audit_exempt_files() -> bool:
    """Audit every file the gate exempts from the scan. True = clean.

    🔴 UNREADABLE IS NOT CLEAN, HERE TOO. An exemption naming a file nothing can
    open audits nothing, and would report exactly what a clean file reports.
    """
    ok = True
    extracted = 0
    for rel in sorted(SKIP_FILES):
        try:
            text = (ROOT / rel).read_text(encoding="utf-8")
        except OSError as e:
            print(f"  FAIL  cannot read the exempt file {rel}: {e} — an "
                  f"exemption that names an unreadable file audits nothing")
            ok = False
            continue
        values = sensitive_values(text)
        extracted += len(values)
        bad = [(n, r, v, why) for n, r, v in values
               if (why := undeclared(r, v)) is not None]
        for n, r, v, why in bad:
            print(f"  FAIL  {rel}:{n}: [{r}] {v!r}")
            print(f"        -> {why}")
        if bad:
            ok = False
        else:
            print(f"  PASS  {rel}: {len(values)} sensitive value(s), every one "
                  f"accounted for")

    if len(EXEMPT_FIXTURE_VALUES) != EXEMPT_FIXTURE_VALUE_COUNT:
        print(f"  FAIL  EXEMPT_FIXTURE_VALUES holds "
              f"{len(EXEMPT_FIXTURE_VALUES)}, EXEMPT_FIXTURE_VALUE_COUNT says "
              f"{EXEMPT_FIXTURE_VALUE_COUNT}. A row added without the count is "
              f"a declaration nobody was told about.")
        ok = False

    if extracted < EXEMPT_AUDIT_MIN_VALUES:
        print(f"  FAIL  extracted {extracted} value(s) from "
              f"{len(SKIP_FILES)} exempt file(s), fewer than "
              f"{EXEMPT_AUDIT_MIN_VALUES}. These files ARE the gate's fixture "
              f"lists; a count this low means the extractor stopped seeing "
              f"whole classes, and 0 means it read nothing.")
        ok = False
    else:
        print(f"  PASS  {extracted} value(s) extracted — the audit is reading "
              f"something (floor {EXEMPT_AUDIT_MIN_VALUES})")
    return ok


def audit_negative_controls() -> list[tuple[str, str, str]]:
    """(label, expected rule, sample) — each must be REFUSED by the audit.

    🔴 EVERY SAMPLE IS ASSEMBLED AT RUNTIME, NEVER SPELLED, AND THAT IS FORCED
    RATHER THAN CUTE. A control for a value-audit cannot be a literal in the
    file the audit reads: the audit would refuse its own control, and the only
    ways out would be exempting the control — a hole the size of the thing being
    tested — or deleting it. So each value is built from pieces that are not
    sensitive on their own, and the assembly is the thing to read.

    🔴 THE EXPECTED RULE IS ASSERTED, NOT JUST "SOMETHING FIRED". A sample
    refused by a different branch is a control that passes while the branch it
    was written for is inert.
    """
    modern = str(EXEMPT_DATE_YEAR_CEILING + 16)
    return [
        ("an undeclared private address", "address",
         "NODE = '192.168." + "7.7'  # a shape the private-ip rule matches"),
        ("an undeclared lab hostname", "private-hostname",
         "API = 'https://muster.backup" + ".lan/api/tasks'"),
        ("a denied identifier that is not a sentinel", "denied-identifier",
         f'    cluster = "{DENY_CANARY_UNPERMITTED}-ci-jx5fq"'),
        ("a dated stamp inside an exempt file", "dated-incident",
         f"# MEASURED {modern}-08-23: the reconciler dropped kickoffs"),
    ]


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
    # The one excused file name must not excuse its look-alikes.
    ("private-hostname",
     "    // notes.local.md is served from the workshed box"),
    ("private-hostname",
     "    base = 'https://CLAUDE.local.mdx.example/api'"),
    ("private-hostname",
     "    // the gateway resolves agent-7.devpod.internal on the cluster network"),
    ("registry-ref",
     'image: registry.workshed.lan/library/muster:0.4.2'),
    ("registry-ref",
     '  IMAGE = "registry.internal.example.net:5000/agents/runner:2000.1.2"'),
    ("private-ip",
     "NODE = '10.255.255.1'  # the k3s node"),
    ("private-ip",
     "    endpoint: 172.20.4.9:30080"),
    # 🔴 THE LINE ALSO SPELLS AN ALLOWLISTED BLOCK, ON PURPOSE. The block must
    # not excuse the host beside it (the allowance is per-occurrence), and a host
    # that shares the base's first three octets must not ride in on it.
    ("private-ip",
     "    except: [172.16.0.0/12]  # the node itself is 172.16.0.9"),
    # 🔴 A BLOCK'S BASE WITH A DIFFERENT MASK IS A SUBNET, NOT THE BLOCK, and so
    # is the base with no mask. Each is one of the three bases, so between them
    # every DOC_BLOCKS row has a control that its mask is what is checked.
    ("private-ip",
     "    podSubnet: 10.0.0.0/24"),
    ("private-ip",
     "    lan: 192.168.0.0/24  # the office network"),
    ("private-ip",
     "    cidr: 172.16.0.0/120"),
    ("private-ip",
     "    route add 192.168.0.0 dev eth0"),
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
    # 🔴 ANYWHERE IN THE IDENTIFIER, NOT ONLY AT ITS START. Each of these was a
    # MISS under the old prefix-only walk: the middle of a header name, the tail
    # of a compound, and a longer word built on the entry.
    ("denied-identifier",
     f'    req.Header.Set("X-{DENY_CANARY_WORD.capitalize()}-Id", id)'),
    ("denied-identifier",
     f'    host = "edge-{DENY_CANARY_WORD}"'),
    ("denied-identifier",
     f'    bin = "{DENY_CANARY_WORD}ctl"'),
    ("denied-identifier",
     f'    c := newMy{DENY_CANARY_WORD.capitalize()}Client()'),
    # The natural camelCase spelling of a one-word entry: whole words joined.
    ("denied-identifier",
     f'    c := new{DENY_CANARY_WORD[:6].capitalize()}{DENY_CANARY_WORD[6:].capitalize()}Client()'),
    # ⚠ THESE THREE DATES ARE DELIBERATELY PRE-2010, AND THAT IS THE ONLY
    # SPELLING AVAILABLE. A control for the dated-incident rule cannot use the
    # year-2000 convention the rule steers toward, because the rule SKIPS that
    # year — the control would be inert. A date older than this project can
    # possibly be keeps the rule watched while being unable to pin an
    # observation to any real day on any real deployment, and the self-audit
    # below enforces exactly that. The narration is invented too: a fixture
    # carrying a real agent's name and a real symptom leaks without spelling
    # any value an audit can extract, and one here did.
    ("dated-incident",
     "# MEASURED 1999-09-01: the reconciler dropped 3 of 14 kickoffs under load"),
    ("dated-incident",
     "    the migration landed and the gate went green on 1999-08-20"),
    ("dated-incident",
     "# 1999-08-29: the nightly sweeper has never run on its configured queue"),
]

POSITIVE_CONTROL = "trusted = '172.16.4.9'  # a real private address"

#: 🔴 CONTENT THAT MUST **NOT** BE REFUSED — these pin the rules' narrowness.
#: A false positive here is not cosmetic: it is how a security gate gets
#: disabled, and then the real findings ship alongside the noise.
ALLOWED_CONTROLS = [
    ("  JobOverlayRootFiles = []string{\"CLAUDE.md\", \"CLAUDE.local.md\", \"AGENTS.md\"}",
     "the Claude Code CLI's personal memory FILE NAME, the one name the "
     "private-hostname rule excuses"),
    ('    "10.244.0.0/16",  # a pod CIDR: every pod in the cluster',
     "the standard Kubernetes pod-CIDR example"),
    ('    Except: []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"},',
     "the three RFC 1918 blocks named as CIDRs — block names, not hosts"),
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
    (f'    scope = tmp / "{DENY_CANARY}d"   # one letter longer',
     "a token in which a denied COMPOUND is a literal substring — a compound "
     "entry matches whole segments only"),
    (f'    ok := is{DENY_CANARY_WORD[:6].capitalize()}{DENY_CANARY_WORD[6:].capitalize()}ized(x)',
     "a one-word entry whose camelCase run would END INSIDE a word: the "
     "false-positive shape the camelCase split exists for"),
    (f'    words = "{DENY_CANARY_WORD[:6]}-{DENY_CANARY_WORD[6:]}"',
     "a one-word entry split by a separator: a declared LIMIT of the rule, "
     "pinned so a change to it is a decision"),
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


def activate_denylist(env=None) -> tuple[bool, bool]:
    """Load the denylist and its key into ACTIVE_DENYLIST, printing the verdict.

    Returns (ok, active): `ok` is False when the run must not pass, `active`
    is whether the denied-identifier check runs at all.
    """
    global ACTIVE_DENYLIST
    ACTIVE_DENYLIST = None
    ok = True

    print("== SET SIZE: the denylist cannot shrink unnoticed ==")
    try:
        digests = load_denylist()
    except DenylistError as e:
        print(f"  FAIL  {e}")
        return False, False
    if not digests:
        print(f"  FAIL  {DENYLIST_FILE.name} is the PLACEHOLDER: it lists no "
              f"digests. Regenerate it with scripts/leakscan-rehash.py and the "
              f"real key before this can pass.")
        ok = False
    elif len(digests) != DENIED_COUNT:
        print(f"  FAIL  {DENYLIST_FILE.name} lists {len(digests)}, DENIED_COUNT "
              f"says {DENIED_COUNT}. A digest removed takes a name off the gate "
              f"with no other symptom.")
        ok = False
    else:
        print(f"  PASS  {DENIED_COUNT} denied identifiers")

    print(f"== KEY: {LEAKSCAN_KEY_ENV} ==")
    try:
        key = leakscan_key(env)
    except DenylistError as e:
        print(f"  FAIL  {e}")
        return False, False
    required, why = key_required(env)
    if key is None:
        if required:
            print(f"  FAIL  {LEAKSCAN_KEY_ENV} is not set, and this is {why}. "
                  f"The denied-identifier check cannot run without it, and a "
                  f"gate that skips a check must not report a pass. Set the "
                  f"repository Actions secret {LEAKSCAN_KEY_ENV}.")
            return False, False
        print(f"  WARN  {LEAKSCAN_KEY_ENV} is not set ({why}): the "
              f"denied-identifier check is SKIPPED. Every other rule still runs.")
        return ok, False
    ACTIVE_DENYLIST = Denylist(key, digests)
    print(f"  PASS  key present ({why}); the denied-identifier check is on")
    return ok, True


def self_test(env=None) -> int:
    ok, denied_on = activate_denylist(env)

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
        if expected == "denied-identifier" and not denied_on:
            continue
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
    declared = {n for n, _, _ in RULES} | {"private-ip", "dated-incident"}
    if denied_on:
        declared.add("denied-identifier")
    else:
        print("  WARN  denied-identifier controls skipped: no key")
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

    # 🔴 THE AUDIT'S OWN CONTROLS RUN BEFORE THE AUDIT, for the reason the whole
    # module exists: an audit that cannot refuse reports "everything accounted
    # for" exactly the way a clean file does.
    print("== SELF-AUDIT CONTROL: the value audit must REFUSE, per branch ==")
    for label, expected, sample in audit_negative_controls():
        if expected == "denied-identifier" and not denied_on:
            continue
        refused = {r for n, r, v in sensitive_values(sample)
                   if undeclared(r, v) is not None}
        if expected in refused:
            print(f"  PASS  {expected:20} refused: {label}")
        else:
            print(f"  FAIL  {expected:20} NOT refused — that branch of the "
                  f"audit is inert")
            print(f"        sample: {sample[:70]}")
            print(f"        refused instead: {sorted(refused) or 'nothing'}")
            ok = False

    # The narrowness half of the same claim: the real exempt files, whose values
    # are all declared, must produce NO refusal. A guard that fires on its own
    # repository is a guard someone deletes.
    print("== SELF-AUDIT: the files the gate EXEMPTS, audited by VALUE ==")
    if not audit_exempt_files():
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
    scanned = {str(f.relative_to(ROOT)) for f in files}
    for rel, why in sorted(DENIED_EXEMPT_FILES.items()):
        if rel not in scanned:
            print(f"  FAIL  {rel} is exempt from the denied-identifier rule but was "
                  f"not scanned at all — remove the stale exemption")
            return 1
        print(f"  NOT DENYLISTED  {rel} — {why}")
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

#!/usr/bin/env python3
"""The govulncheck verdict, with a checked-in, expiring allowlist.

    go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 -format json ./... > govulncheck.json
    python3 .github/scripts/vulngate.py --allowlist .github/govulncheck-allowlist.txt < govulncheck.json

govulncheck's text mode exits 3 on any REACHABLE advisory and has no way to
accept one. That is right until an advisory is published with no fixed release
yet: then there is no bump to make, and a gate with no override blocks every
merge and release until upstream ships one. So the scan runs in JSON mode
(which exits 0 whatever it finds) and this script is the verdict:

  exit 0  every reachable advisory is on the allowlist with an unexpired date
  exit 1  a reachable advisory is NOT on the allowlist, or an entry has EXPIRED
  exit 2  the allowlist is malformed, or the scan output is unusable

"Reachable" is govulncheck's symbol level: a finding whose trace starts at a
function, i.e. muster's code calls the vulnerable symbol. Module- and
package-level findings (imported but not called) never fail the gate, which is
exactly what text mode's exit 3 means.

🔴 AN EXPIRED ENTRY FAILS EVEN IF THE ADVISORY IS NO LONGER REPORTED. An entry
is a dated promise to look again; a gate that let it lapse silently would turn
the allowlist into a permanent mute. Remove the entry, or re-date it with a
reason.

🔴 THE SCAN MUST HAVE RUN. An empty or truncated stream has no findings, which
reads exactly like a clean tree, so a stream with no `config` message is a
refusal (exit 2), not a pass.

Allowlist format, one entry per line, `#` starts a comment:

    GO-YYYY-NNNN  YYYY-MM-DD  reason (free text, required)

The date is the last day the entry is honoured (UTC).
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
import re
import sys
from dataclasses import dataclass

ID_RE = re.compile(r"^GO-\d{4}-\d{4,}$")


class Malformed(Exception):
    """The allowlist or the scan output cannot be trusted; exit 2."""


@dataclass(frozen=True)
class Entry:
    osv: str
    expires: dt.date
    reason: str
    line: int


def parse_allowlist(text: str) -> dict[str, Entry]:
    entries: dict[str, Entry] = {}
    for n, raw in enumerate(text.splitlines(), 1):
        line = raw.split("#", 1)[0].strip()
        if not line:
            continue
        parts = line.split(None, 2)
        if len(parts) < 3:
            raise Malformed(f"allowlist line {n}: want `GO-YYYY-NNNN YYYY-MM-DD reason`, got {raw.strip()!r}")
        osv, date, reason = parts
        if not ID_RE.match(osv):
            raise Malformed(f"allowlist line {n}: {osv!r} is not a Go advisory ID (GO-YYYY-NNNN)")
        try:
            expires = dt.date.fromisoformat(date)
        except ValueError:
            raise Malformed(f"allowlist line {n}: {date!r} is not a YYYY-MM-DD date") from None
        if osv in entries:
            raise Malformed(f"allowlist line {n}: {osv} is already listed on line {entries[osv].line}")
        entries[osv] = Entry(osv, expires, reason.strip(), n)
    return entries


def parse_stream(text: str) -> list[dict]:
    """govulncheck -format json writes a stream of concatenated JSON objects."""
    dec = json.JSONDecoder()
    objs, i = [], 0
    while True:
        while i < len(text) and text[i].isspace():
            i += 1
        if i >= len(text):
            break
        try:
            obj, i = dec.raw_decode(text, i)
        except json.JSONDecodeError as e:
            raise Malformed(f"scan output is not a govulncheck JSON stream: {e}") from None
        objs.append(obj)
    if not any(isinstance(o, dict) and "config" in o for o in objs):
        raise Malformed("scan output has no `config` message: govulncheck did not run, "
                        "and an empty result must not read as a clean one")
    return objs


def reachable(objs: list[dict]) -> dict[str, list[str]]:
    """advisory ID -> example call paths, symbol level only."""
    out: dict[str, list[str]] = {}
    for o in objs:
        f = o.get("finding") if isinstance(o, dict) else None
        if not f:
            continue
        trace = f.get("trace") or []
        if not trace or not trace[0].get("function"):
            continue
        path = " <- ".join(
            ".".join(p for p in (fr.get("package", "").rsplit("/", 1)[-1], fr.get("receiver", ""), fr.get("function", "")) if p)
            for fr in trace
        )
        out.setdefault(f["osv"], []).append(path)
    return out


def summaries(objs: list[dict]) -> dict[str, str]:
    return {o["osv"]["id"]: o["osv"].get("summary", "") for o in objs
            if isinstance(o, dict) and isinstance(o.get("osv"), dict) and "id" in o["osv"]}


def verdict(objs: list[dict], allow: dict[str, Entry], today: dt.date) -> tuple[int, list[str]]:
    lines: list[str] = []
    found = reachable(objs)
    summ = summaries(objs)
    failed = False

    for e in sorted(allow.values(), key=lambda e: e.osv):
        if e.expires < today:
            failed = True
            lines.append(f"EXPIRED allowlist entry {e.osv} (line {e.line}, expired {e.expires}): "
                         f"remove it, or re-date it with a fresh reason. Reason was: {e.reason}")

    for osv in sorted(found):
        e = allow.get(osv)
        paths = found[osv]
        head = f"{osv}: {summ.get(osv, '')}".rstrip(": ")
        if e is None:
            failed = True
            lines.append(f"REACHABLE {head}\n    e.g. {paths[0]}\n    "
                         f"fix: the bump `govulncheck` names; if no fixed release exists, "
                         f"add a dated entry to the allowlist")
        elif e.expires >= today:
            lines.append(f"allowed {osv} until {e.expires} (line {e.line}): {e.reason}")

    for osv in sorted(set(allow) - set(found)):
        if allow[osv].expires >= today:
            lines.append(f"note: allowlist entry {osv} (line {allow[osv].line}) is no longer reachable; remove it")

    lines.append(f"{len(found)} reachable advisory(ies), {len(allow)} allowlist entr(y/ies): "
                 + ("FAIL" if failed else "ok"))
    return (1 if failed else 0), lines


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n", 1)[0])
    ap.add_argument("--allowlist", required=True)
    ap.add_argument("--today", help="YYYY-MM-DD (UTC); default: today. For tests.")
    a = ap.parse_args(argv)
    today = dt.date.fromisoformat(a.today) if a.today else dt.datetime.now(dt.timezone.utc).date()
    try:
        with open(a.allowlist, encoding="utf-8") as fh:
            allow = parse_allowlist(fh.read())
        objs = parse_stream(sys.stdin.read())
    except (Malformed, OSError) as e:
        print(f"vulngate: {e}", file=sys.stderr)
        return 2
    rc, lines = verdict(objs, allow, today)
    print("\n".join(lines))
    return rc


if __name__ == "__main__":
    sys.exit(main())

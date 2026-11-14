#!/usr/bin/env python3
"""A vX.Y.Z tag run's test jobs: re-run them, or rely on the same commit's main run?

    tag_gate.py decide --repo OWNER/NAME --workflow ci.yml --sha SHA --ref-type tag
        prints `retest=true|false` (for $GITHUB_OUTPUT) and why, on stdout
    tag_gate.py gate   (reads the `needs` context as JSON from $NEEDS)
        exit 0 when every test job may stand behind a publish, else 1

🔴 WHY. A tag is pushed by the release tooling onto a commit `main` already
built, and the tag run used to re-run the whole suite on it. Measured
2026-10-30 → 11-13: 110 tag runs, ALL on a commit whose main `ci.yml` run had
concluded success, 72 runner-hours of wall; 11 of them went red anyway, 10 in
a test job (flakes in e2e, the Go suite, macOS, the release dry run), and
each red run held that release back, because the downstream release tool takes
only a sha whose CI runs ALL succeeded. Operator decision 2026-11-13 (PR #655):
on a tag, skip the test jobs when that commit's main run succeeded; the image
and release jobs still run (they build, smoke and publish the shipped bits).

🔴 FAIL-CLOSED: `decide` answers `retest=false` ONLY when the lookup succeeded
and the LATEST completed `push` run of this workflow on branch `main` for
exactly this sha concluded `success`. A lookup error, no such run, a run that
is still in progress, or a latest run that did not succeed all answer
`retest=true`, and the tag run tests as it always did. `decide` never fails the
job over the lookup: a red here would itself block the release.

`gate` is publish-gate's check: every job it needs either SUCCEEDED, or was
SKIPPED on a run whose `main-verdict` job succeeded and answered
`retest=false`. `main-verdict` itself may be skipped (every non-tag run).
Anything else — a failure, a cancellation, a skip nobody decided — refuses.
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
from typing import Callable

VERDICT_JOB = "main-verdict"


def decide(ref_type: str, sha: str, fetch: Callable[[], object]) -> tuple[bool, str]:
    """(retest, reason). `fetch` returns the runs-list API's JSON, or raises."""
    if ref_type != "tag":
        return True, f"not a tag run (ref type {ref_type!r}): the suite runs"
    try:
        doc = fetch()
        runs = doc["workflow_runs"]
        mine = [r for r in runs
                if r["head_sha"] == sha and r["head_branch"] == "main"
                and r["event"] == "push" and r["status"] == "completed"]
    except Exception as e:  # noqa: BLE001 — every lookup failure is fail-closed
        return True, f"could not read main's runs for {sha} ({type(e).__name__}: {e}): the suite runs"
    if not mine:
        return True, f"no completed push run on main for {sha}: the suite runs"
    latest = max(mine, key=lambda r: (r["created_at"], r["id"]))
    if latest["conclusion"] != "success":
        return True, (f"main run {latest['id']} for {sha} concluded {latest['conclusion']!r}, "
                      f"not success: the suite runs")
    return False, (f"main run {latest['id']} for {sha} concluded success: the test jobs are "
                   f"skipped; the image and release jobs run")


def gate(needs: dict) -> tuple[bool, list[str]]:
    """(ok, lines) over the `needs` context of publish-gate."""
    v = needs.get(VERDICT_JOB)
    if v is None:
        return False, [f"REFUSED: publish-gate does not need `{VERDICT_JOB}`, so a skip cannot be read"]
    decided = v.get("result") == "success" and (v.get("outputs") or {}).get("retest") == "false"
    ok, lines = True, [f"{VERDICT_JOB}: {v.get('result')} (retest={(v.get('outputs') or {}).get('retest')!r})"]
    if v.get("result") not in ("success", "skipped"):
        ok = False
        lines.append(f"REFUSED: {VERDICT_JOB} {v.get('result')}")
    for job, n in sorted(needs.items()):
        if job == VERDICT_JOB:
            continue
        r = n.get("result")
        if r == "success" or (r == "skipped" and decided):
            lines.append(f"{job}: {r}")
        else:
            ok = False
            why = "skipped, and no main run vouched for this commit" if r == "skipped" else r
            lines.append(f"REFUSED: {job}: {why}")
    return ok, lines


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    sub = ap.add_subparsers(dest="cmd", required=True)
    d = sub.add_parser("decide")
    d.add_argument("--repo", required=True)
    d.add_argument("--workflow", required=True)
    d.add_argument("--sha", required=True)
    d.add_argument("--ref-type", required=True)
    sub.add_parser("gate")
    a = ap.parse_args()
    if a.cmd == "decide":
        def fetch():
            out = subprocess.run(
                ["gh", "api", f"repos/{a.repo}/actions/workflows/{a.workflow}/runs"
                              f"?head_sha={a.sha}&branch=main&event=push&per_page=50"],
                capture_output=True, text=True, timeout=60)
            if out.returncode != 0:
                raise RuntimeError(f"gh api exited {out.returncode}: {out.stderr.strip()}")
            return json.loads(out.stdout)
        retest, why = decide(a.ref_type, a.sha, fetch)
        print(f"retest={'true' if retest else 'false'}")
        print(why, file=sys.stderr)
        return 0
    try:
        needs = json.loads(os.environ["NEEDS"])
    except (KeyError, ValueError) as e:
        print(f"gate: REFUSED: cannot read $NEEDS ({e})", file=sys.stderr)
        return 1
    ok, lines = gate(needs)
    print("\n".join(lines))
    return 0 if ok else 1


if __name__ == "__main__":
    raise SystemExit(main())

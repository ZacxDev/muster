#!/usr/bin/env python3
"""Keep ONE open `main-red` issue while `main` is red, and close it when `main` is green.

    main_red.py --repo OWNER/NAME --sha SHA --run-url URL   (reads `needs` as JSON from $NEEDS)

🔴 WHY. Nothing told anyone when a push to `main` went red: main HEAD 4c3d54f
(2026-11-17) was red and nobody was notified. GitHub notifies a repository's
watchers about a new issue and its comments, so one issue is the notification,
with no new host wiring. Operator decision 2026-11-18: "A `main-red` job in
ci.yml: on a failed push to main it opens/updates one issue; the next green main
closes it (that's its closing condition)."

THE VERDICT is the jobs ci.yml's `main-red` job needs, which are exactly
publish-gate's (pinned by TestCIMainRedNeedsWhatPublishGateNeeds):
  red     any of them FAILED
  none    any was CANCELLED (a cancelled run decides nothing), or NEEDS is unreadable
  green   otherwise (success, or skipped: main-verdict is skipped on every non-tag run)

🔴 ONLY THE CURRENT HEAD OF `main` DECIDES. Runs finish out of order; an older
run finishing red after a newer one went green must not reopen the issue. So the
script acts only when SHA is main's head at the time it runs; any other sha (or a
failed head lookup) is a no-op that says so.

ACTIONS
  red, no open issue     create it (title "main is red", label main-red)
  red, open issue(s)     comment on the oldest
  green, open issue(s)   comment on each and close it
  green, none open       nothing

It never fails the job over the issue API: a red here would itself read as main
being red. Exit 0 always, after printing what it did; 2 on a usage error.
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
from typing import Callable

LABEL = "main-red"
TITLE = "main is red"

Gh = Callable[[list[str]], tuple[int, str]]


def real_gh(args: list[str]) -> tuple[int, str]:
    r = subprocess.run(["gh", *args], capture_output=True, text=True)
    if r.returncode != 0:
        sys.stderr.write(r.stderr)
    return r.returncode, r.stdout


def verdict(needs_json: str) -> str:
    try:
        needs = json.loads(needs_json)
        results = [v["result"] for v in needs.values()]
    except (ValueError, TypeError, KeyError, AttributeError):
        return "none"
    if not results:
        return "none"
    if "cancelled" in results:
        return "none"
    if "failure" in results:
        return "red"
    return "green"


def act(repo: str, sha: str, run_url: str, state: str, gh: Gh = real_gh) -> str:
    if state == "none":
        return "no verdict (a job was cancelled, or the needs context was unreadable): nothing to do"
    rc, head = gh(["api", f"repos/{repo}/commits/main", "--jq", ".sha"])
    head = head.strip()
    if rc != 0 or not head:
        return "could not read main's head: nothing to do"
    if head != sha:
        return f"{sha[:12]} is not main's head ({head[:12]}): a newer run decides"
    rc, out = gh(["issue", "list", "--repo", repo, "--label", LABEL, "--state", "open",
                  "--json", "number", "--jq", ".[].number"])
    if rc != 0:
        return "could not list the main-red issues: nothing to do"
    open_issues = sorted(int(n) for n in out.split())
    if state == "red":
        body = f"main is red at {sha} — {run_url}"
        if open_issues:
            gh(["issue", "comment", str(open_issues[0]), "--repo", repo, "--body", "Still red: " + body])
            return f"commented on #{open_issues[0]}"
        gh(["label", "create", LABEL, "--repo", repo, "--color", "B60205",
            "--description", "main's CI is red; closed by the next green main run", "--force"])
        gh(["issue", "create", "--repo", repo, "--title", TITLE, "--label", LABEL, "--body",
            body + "\n\nThis issue closes itself on the next green run of ci.yml on main."])
        return "opened the main-red issue"
    for n in open_issues:
        gh(["issue", "comment", str(n), "--repo", repo, "--body", f"main is green again at {sha} — {run_url}"])
        gh(["issue", "close", str(n), "--repo", repo])
    return f"closed {', '.join('#%d' % n for n in open_issues)}" if open_issues else "green, no open issue"


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--repo", required=True)
    ap.add_argument("--sha", required=True)
    ap.add_argument("--run-url", required=True)
    a = ap.parse_args()
    state = verdict(os.environ.get("NEEDS", ""))
    print(f"main-red: verdict={state}: {act(a.repo, a.sha, a.run_url, state)}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

#!/usr/bin/env python3
"""Keep ONE open `main-red` issue while `main` is red, and close it when `main` is green.

    main_red.py --repo OWNER/NAME --sha SHA --run-number N --run-url URL --assignee LOGIN
                (reads `needs` as JSON from $NEEDS)

🔴 WHY. Nothing told anyone when a push to `main` went red: main HEAD 4c3d54f
(2026-11-17) was red and nobody was notified. GitHub notifies a repository's
watchers about a new issue and its comments, so one issue is the notification,
with no new host wiring. Operator decision 2026-11-18: "A `main-red` job in
ci.yml: on a failed push to main it opens/updates one issue; the next green main
closes it (that's its closing condition)."

THE VERDICT is the jobs ci.yml's `main-red` job needs — publish-gate's, plus
docshots (pinned by TestCIMainRedNeedsWhatPublishGateNeeds):
  none    any was CANCELLED (a cancelled run decides nothing), or NEEDS is unreadable
  red     any FAILED, or any was SKIPPED other than main-verdict (skipped on every
          non-tag run): on a push to main every other job runs, so its skip is
          one nobody decided, which tag_gate.py gate refuses too
  green   otherwise

The issue is assigned to --assignee (the repository owner): GitHub notifies an
assignee whether or not they watch the repository.

🔴 THE NEWEST FINISHED VERDICT DECIDES. Runs finish out of order, and with every
`main` push now running in full a run usually finishes AFTER a newer push. The
first rule here was "act only if SHA is still main's head", and that never let a
green run close the issue during a burst of merges: each green finished behind a
newer push, so the issue stayed open through an hour of green (#843). Now this
run acts unless a NEWER `main` push run of ci.yml (higher run number) has already
DECIDED: its own `main-red` job has completed successfully. That is the job, not
the run: a run only completes ~10 min after its main-red acted (the image jobs run
after it), and keying on the run let an older run act in that window and flap
the issue the newer one had just set (audit round 1 on #872). A newer run whose
main-red has not finished does not stop this one, because it will decide in turn;
a newer run that was cancelled (its main-red skipped) decides nothing. A failed
lookup is a no-op that says so.

⚠ Two main-red jobs running at the same moment (seconds apart) can both act, in
either order: neither has completed when the other looks.

⚠ It does not classify an infrastructure failure (a download that failed in a
setup step) as anything but red. The run's failed step names are readable, but
they mix setup with deliberate checks, so no reliable rule tells them apart; such
a red opens or comments on the issue like any other.

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


SKIP_OK = {"main-verdict"}


def verdict(needs_json: str) -> str:
    try:
        needs = json.loads(needs_json)
        results = {k: v["result"] for k, v in needs.items()}
    except (ValueError, TypeError, KeyError, AttributeError):
        return "none"
    if not results:
        return "none"
    if "cancelled" in results.values():
        return "none"
    if any(r == "failure" or (r == "skipped" and k not in SKIP_OK) for k, r in results.items()):
        return "red"
    return "green"


JOB_NAME = "main red? (open or close the main-red issue)"  # ci.yml's main-red job's name


def newer_finished(repo: str, run_number: int, gh: Gh) -> tuple[bool, str]:
    """(ok, what): whether a newer `main` push run of ci.yml has already DECIDED,
    i.e. its own main-red job completed successfully. ok is False when a list
    cannot be read."""
    rc, out = gh(["api", f"repos/{repo}/actions/workflows/ci.yml/runs?branch=main&event=push&per_page=50",
                  "--jq", '.workflow_runs[] | "\\(.run_number) \\(.id)"'])
    if rc != 0:
        return False, ""
    for line in out.splitlines():
        parts = line.split()
        if len(parts) != 2 or not parts[0].isdigit() or not parts[1].isdigit():
            continue
        n, run_id = int(parts[0]), parts[1]
        if n <= run_number:
            continue
        rc, jobs = gh(["api", f"repos/{repo}/actions/runs/{run_id}/jobs?per_page=100",
                       "--jq", f'.jobs[] | select(.name == "{JOB_NAME}") | "\\(.status) \\(.conclusion)"'])
        if rc != 0:
            return False, ""
        if any(j.split() == ["completed", "success"] for j in jobs.splitlines()):
            return True, f"run #{n}, whose main-red has already acted"
    return True, ""


def act(repo: str, sha: str, run_url: str, state: str, gh: Gh = real_gh, assignee: str = "", *,
        run_number: int) -> str:
    if state == "none":
        return "no verdict (a job was cancelled, or the needs context was unreadable): nothing to do"
    ok, newer = newer_finished(repo, run_number, gh)
    if not ok:
        return "could not list ci.yml's main runs: nothing to do"
    if newer:
        return f"{sha[:12]} (run #{run_number}) is older than {newer}: nothing to do"
    rc, out = gh(["issue", "list", "--repo", repo, "--label", LABEL, "--state", "open",
                  "--json", "number", "--jq", ".[].number"])
    if rc != 0:
        return "could not list the main-red issues: nothing to do"
    open_issues = sorted(int(n) for n in out.split())
    if state == "red":
        body = f"main is red at {sha} — {run_url}"
        if open_issues:
            rc, _ = gh(["issue", "comment", str(open_issues[0]), "--repo", repo, "--body", "Still red: " + body])
            return f"commented on #{open_issues[0]}" if rc == 0 else f"FAILED to comment on #{open_issues[0]}"
        gh(["label", "create", LABEL, "--repo", repo, "--color", "B60205",
            "--description", "main's CI is red; closed by the next green main run", "--force"])
        create = ["issue", "create", "--repo", repo, "--title", TITLE, "--label", LABEL, "--body",
                  body + "\n\nThis issue closes itself on the next green run of ci.yml on main."]
        if assignee:
            create += ["--assignee", assignee]
        rc, _ = gh(create)
        return "opened the main-red issue" if rc == 0 else "FAILED to open the main-red issue"
    done = []
    for n in open_issues:
        gh(["issue", "comment", str(n), "--repo", repo, "--body", f"main is green again at {sha} — {run_url}"])
        rc, _ = gh(["issue", "close", str(n), "--repo", repo])
        done.append(f"#{n}" if rc == 0 else f"#{n} (FAILED to close)")
    return f"closed {', '.join(done)}" if open_issues else "green, no open issue"


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--repo", required=True)
    ap.add_argument("--sha", required=True)
    ap.add_argument("--run-number", type=int, required=True)
    ap.add_argument("--run-url", required=True)
    ap.add_argument("--assignee", default="")
    a = ap.parse_args()
    state = verdict(os.environ.get("NEEDS", ""))
    print(f"main-red: verdict={state}: {act(a.repo, a.sha, a.run_url, state, assignee=a.assignee, run_number=a.run_number)}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

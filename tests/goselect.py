#!/usr/bin/env python3
"""Which Go test packages a change can affect — computed, logged, and (in
SHADOW mode, the default) NOT acted on: the full suite still runs, and the
ledger records every failing test that fell outside the selection (a MISS).

    python3 tests/goselect.py select --base REV [--head REV] --out SEL.json
    python3 tests/goselect.py ledger --selection SEL.json --results RES.json \\
        --run-id ID --sha SHA --event EV --shard N --mode shadow|enforce --out LEDGER.json
    python3 tests/goselect.py apply --selection SEL.json --args ARGS   # enforce only

🔴 SELECTION. A test package is selected when its TEST BINARY (`go list -test
-deps`: the package, its _test.go imports, and everything they import) contains
a package the diff changed, or when the diff touched its own test files,
testdata or embedded files. A package's _test.go change selects that package
only; a non-test .go change selects every package whose test binary contains it.

🔴 FAIL OPEN. The whole suite is selected (`fallback` names why) when the base
cannot be diffed, `go list` fails, the diff touches a path in FALLBACK below, or
the selection is more than half of all tests anyway. FALLBACK is the one list.

🔴 ALWAYS-RUN. A test that reads the repository by PATH (a "../" literal,
filepath.Join("..", …), a module-root helper, runtime.Caller) or runs the `go`
tool over other packages is invisible to an import graph, so every package with
such a test is selected on every run. Found by scanning each package's
_test.go files with ALWAYS_RUN_SIGNS on every run, not from a pinned list, so a
new such test joins the set the day it lands; ALWAYS_RUN_EXTRA pins what the
scan cannot see.

Exit codes: 0 always for `select` and `ledger` (an error becomes a fallback or a
note; this script must never change a verdict); `apply` exits 1 on bad input.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys

# 🔴 THE FALLBACK LIST: a changed path matching one of these runs everything.
# (prefix or exact path, reason). One place; tests/goselect_test.py pins each.
FALLBACK: list[tuple[str, str]] = [
    ("go.mod", "module manifest: the dependency set and the go/toolchain line every package builds with"),
    ("go.sum", "dependency lockfile"),
    ("package.json", "JS manifest (the e2e harness and the CSS build)"),
    ("package-lock.json", "JS lockfile"),
    ("e2e/package.json", "JS manifest of the e2e harness"),
    ("e2e/package-lock.json", "JS lockfile of the e2e harness"),
    ("flake.nix", "toolchain pin (the nix devshell and builds)"),
    ("flake.lock", "toolchain lockfile"),
    (".github/", "CI workflow and its scripts"),
    ("tests/", "the CI test harness: verdict.py, goshard.py, gomtime.py and this selector"),
    ("Makefile", "the test runner's entry point (make test)"),
    ("docker-compose.test.yml", "the test database every Postgres-backed package uses"),
    ("internal/dbtest/", "global test setup: every Postgres-backed test's database comes from it"),
    ("internal/db/migrations/", "the template schema every Postgres-backed package copies (migrated before the suite)"),
    ("cmd/muster-migrate/", "builds that template database"),
]

# A selection above this share of all tests runs everything instead.
MAX_SHARE = 0.5

# 🔴 ALWAYS-RUN SIGNS, matched against each _test.go file's text.
ALWAYS_RUN_SIGNS: list[tuple[str, re.Pattern[str]]] = [
    ("a relative path that leaves the package directory", re.compile(r'"\.\./')),
    ("filepath.Join starting at ..", re.compile(r'filepath\.Join\(\s*"\.\."')),
    ("a module- or repo-root helper", re.compile(r'\b[A-Za-z]*(?:[Mm]odule|[Rr]epo)Root\(')),
    ("runtime.Caller (locating files by source position)", re.compile(r'\bruntime\.Caller\(')),
    ("runs the go tool", re.compile(r'exec\.Command(?:Context)?\((?:[^,()]+,\s*)?"go"')),
]
# Packages the scan cannot see but that read the tree by path: (import path
# suffix after the module, reason). Empty is a claim, re-checked by the
# discovery recorded in the PR that added this file.
ALWAYS_RUN_EXTRA: list[tuple[str, str]] = []

TEST_FUNC = re.compile(r"^func (?:Test|Example|Fuzz)\w*\(", re.M)


def fallback_for(path: str) -> str | None:
    for rule, why in FALLBACK:
        if path == rule or (rule.endswith("/") and path.startswith(rule)):
            return f"{path}: {why}"
    return None


def parse_list(stream: str) -> list[dict]:
    """`go list -json` prints concatenated objects; decode them all."""
    dec, i, out = json.JSONDecoder(), 0, []
    while True:
        while i < len(stream) and stream[i].isspace():
            i += 1
        if i >= len(stream):
            return out
        obj, i = dec.raw_decode(stream, i)
        out.append(obj)


def graph(entries: list[dict], root: str) -> tuple[dict, dict]:
    """(every package, the packages with tests), each {import path: {dir
    (repo-relative), embed, testembed, testfiles, deps}}; `deps` is the test
    binary's packages with the " [p.test]" variants folded."""
    pkgs: dict[str, dict] = {}
    deps: dict[str, set[str]] = {}
    for e in entries:
        ip = e.get("ImportPath", "")
        if e.get("ForTest"):
            continue
        if ip.endswith(".test"):
            deps[ip[: -len(".test")]] = {d.split(" [", 1)[0] for d in e.get("Deps", [])}
            continue
        if " [" in ip:
            continue
        rel = os.path.relpath(e["Dir"], root)
        pkgs[ip] = {
            "dir": "" if rel == "." else rel,
            "embed": set(e.get("EmbedFiles") or []),
            "testembed": set((e.get("TestEmbedFiles") or []) + (e.get("XTestEmbedFiles") or [])),
            "testfiles": (e.get("TestGoFiles") or []) + (e.get("XTestGoFiles") or []),
        }
    for ip, p in pkgs.items():
        p["deps"] = deps.get(ip, set()) | {ip}
    return pkgs, {ip: p for ip, p in pkgs.items() if p["testfiles"]}


def owner(path: str, dirs: dict[str, str]) -> str | None:
    """The package whose directory is the nearest ancestor of `path`."""
    d = os.path.dirname(path)
    while True:
        if d in dirs:
            return dirs[d]
        if not d:
            return None
        d = os.path.dirname(d)


def select(changed: list[str], allp: dict, tested: dict, always: dict[str, str],
           total_tests: dict[str, int]) -> dict:
    """Pure: the selection for `changed` over graph() output."""
    dirs = {p["dir"]: ip for ip, p in allp.items()}
    why: dict[str, str] = {ip: f"always-run: {r}" for ip, r in always.items() if ip in tested}
    fallback = None
    for f in changed:
        fb = fallback_for(f)
        if fb:
            fallback = fallback or fb
            continue
        ip = owner(f, dirs)
        if ip is None:
            continue  # not under any Go package: only the always-run set can read it
        p = allp[ip]
        rel = os.path.relpath(f, p["dir"]) if p["dir"] else f
        direct = os.path.dirname(f) == p["dir"]
        is_go = f.endswith(".go")
        if (direct and is_go and not f.endswith("_test.go")) or rel in p["embed"]:
            # A change to the package itself: every test binary containing it.
            for t, tp in tested.items():
                if ip in tp["deps"]:
                    why.setdefault(t, f"imports {ip} ({f})")
        elif ip in tested:
            why.setdefault(ip, f"its own test file, testdata or embedded file ({f})")
    total = sum(total_tests.values())
    chosen = sum(total_tests.get(ip, 0) for ip in why)
    if fallback is None and total and chosen > MAX_SHARE * total:
        fallback = f"selects {chosen} of {total} tests (> {int(MAX_SHARE * 100)}%)"
    return {"selected": sorted(why), "why": why, "fallback": fallback,
            "selected_tests": total if fallback else chosen, "total_tests": total,
            "total_packages": len(tested)}


def read(path: str) -> str:
    with open(path, encoding="utf-8", errors="replace") as f:
        return f.read()


def scan_always(tested: dict, root: str) -> dict[str, str]:
    out: dict[str, str] = {}
    for ip, p in tested.items():
        for name in p["testfiles"]:
            try:
                text = read(os.path.join(root, p["dir"], name))
            except OSError:
                continue
            for label, rx in ALWAYS_RUN_SIGNS:
                if rx.search(text):
                    out.setdefault(ip, f"{label} in {os.path.join(p['dir'], name)}")
                    break
            if ip in out:
                break
    return out


def count_tests(tested: dict, root: str) -> dict[str, int]:
    n = {}
    for ip, p in tested.items():
        c = 0
        for name in p["testfiles"]:
            try:
                c += len(TEST_FUNC.findall(read(os.path.join(root, p["dir"], name))))
            except OSError:
                pass
        n[ip] = c
    return n


def run(*args: str, cwd: str | None = None) -> str:
    r = subprocess.run(args, cwd=cwd, capture_output=True, text=True)
    if r.returncode != 0:
        raise RuntimeError(f"`{' '.join(args)}` exited {r.returncode}: {r.stderr.strip()[:500]}")
    return r.stdout


def cmd_select(a) -> int:
    res: dict = {"base": a.base, "head": a.head}
    try:
        root = run("git", "rev-parse", "--show-toplevel").strip()
        if not a.base or set(a.base) == {"0"}:
            raise RuntimeError("no base revision (a new branch, or a run that is full by design)")
        changed = [f for f in run("git", "diff", "--no-renames", "--name-only", "-z", a.base, a.head, cwd=root).split("\0") if f]
        entries = parse_list(run("go", "list", "-e", "-test", "-json=ImportPath,Dir,ForTest,Deps,TestGoFiles,"
                                 "XTestGoFiles,EmbedFiles,TestEmbedFiles,XTestEmbedFiles", "./...", cwd=root))
        allp, tested = graph(entries, root)
        if not tested:
            raise RuntimeError("`go list` named no package with tests")
        always = scan_always(tested, root)
        mod = run("go", "list", "-m", cwd=root).strip()
        for suffix, why in ALWAYS_RUN_EXTRA:
            always.setdefault(f"{mod}/{suffix}", f"pinned: {why}")
        res |= select(changed, allp, tested, always, count_tests(tested, root))
        res["changed"] = changed
        res["always"] = sorted(always)
    except Exception as e:  # noqa: BLE001 — fail open, never fail the job
        res |= {"selected": [], "why": {}, "fallback": f"selector error: {e}", "selected_tests": None,
                "total_tests": None, "total_packages": None, "changed": [], "always": []}
    if res.get("fallback") and res.get("total_packages") is not None:
        res["selected_tests"] = res["total_tests"]
    with open(a.out, "w") as f:
        json.dump(res, f, indent=1, sort_keys=True)
    sel = "ALL (fallback)" if res["fallback"] else f"{len(res['selected'])} of {res['total_packages']} packages, " \
          f"{res['selected_tests']} of {res['total_tests']} top-level tests"
    print(f"goselect: base {a.base} → {a.head}: {len(res['changed'])} changed file(s); selection: {sel}")
    if res["fallback"]:
        print(f"goselect: FALLBACK (full suite): {res['fallback']}")
    for ip in res["selected"]:
        print(f"  selected {ip}: {res['why'][ip]}")
    return 0


def load_results(path: str) -> dict[str, str]:
    try:
        with open(path) as f:
            return json.load(f)
    except (OSError, ValueError):
        return {}


def cmd_ledger(a) -> int:
    with open(a.selection) as f:
        sel = json.load(f)
    results = load_results(a.results)  # {"pkg\ttest": "pass"|"fail"|"skip"}
    full = bool(sel.get("fallback"))
    chosen = set(sel.get("selected", []))
    failed = sorted(k for k, v in results.items() if v == "fail")
    miss = [] if full else [k for k in failed if k.split("\t", 1)[0] not in chosen]
    ran = [k for k in results if results[k] in ("pass", "fail")]
    rec = {
        "v": 1, "run_id": a.run_id, "sha": a.sha, "event": a.event, "shard": a.shard, "mode": a.mode,
        "fallback": sel.get("fallback"),
        "selected_packages": None if full else len(chosen), "total_packages": sel.get("total_packages"),
        "selected_tests": len(ran) if full else sum(1 for k in ran if k.split("\t", 1)[0] in chosen),
        "total_tests": len(ran),
        "failed": [k.replace("\t", ".") for k in failed],
        "failed_outside_selection": [k.replace("\t", ".") for k in miss],
        "results_read": bool(results),
    }
    with open(a.out, "w") as f:
        json.dump(rec, f, sort_keys=True)
    print("SELECTIVE-LEDGER " + json.dumps(rec, sort_keys=True))
    summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary:
        verdict = "a FULL verdict" if a.mode == "shadow" or full else "SCOPED, not a full verdict"
        picked = "ALL (fallback)" if full else f"{len(chosen)} of {rec['total_packages']} packages"
        lines = [f"### Go test selection ({a.mode.upper()})", "",
                 f"- selection: **{picked}** — {rec['selected_tests']} of {rec['total_tests']} tests that ran in this shard",
                 f"- fallback: {sel.get('fallback') or 'none'}",
                 f"- failed: {len(failed)}; failed OUTSIDE the selection (misses): **{len(miss)}**",
                 f"- this shard's go test was {verdict}" + (" (shadow: the full suite ran)" if a.mode == "shadow" else "")]
        lines += [f"  - MISS {m}" for m in rec["failed_outside_selection"]]
        with open(summary, "a") as f:
            f.write("\n".join(lines) + "\n")
    return 0


def cmd_apply(a) -> int:
    """ENFORCE mode only: narrow this shard's go-test args to the selection.
    Prints `run=true|false` for $GITHUB_OUTPUT. A fallback leaves them alone."""
    with open(a.selection) as f:
        sel = json.load(f)
    if sel.get("fallback"):
        print("run=true")
        print(f"goselect apply: fallback ({sel['fallback']}) — the shard runs in full", file=sys.stderr)
        return 0
    chosen = set(sel["selected"])
    with open(a.args) as f:
        args = f.read().split()
    if "-run" in args:  # a split shard: `-run ^(…)$ <pkg>`
        if len(args) != 3 or args[0] != "-run":
            print(f"goselect apply: cannot read the shard's args {args}", file=sys.stderr)
            return 1
        full_pkg = run("go", "list", args[2]).strip()
        print(f"run={'true' if full_pkg in chosen else 'false'}")
        return 0
    pkgs = [p for p in args if p in chosen]
    with open(a.args, "w") as f:
        f.write("\n".join(pkgs) + "\n")
    print(f"run={'true' if pkgs else 'false'}")
    print(f"goselect apply: {len(pkgs)} of {len(args)} packages of this shard selected — SCOPED, not a full verdict",
          file=sys.stderr)
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    sub = ap.add_subparsers(dest="cmd", required=True)
    s = sub.add_parser("select")
    s.add_argument("--base", default="")
    s.add_argument("--head", default="HEAD")
    s.add_argument("--out", required=True)
    s.set_defaults(fn=cmd_select)
    l = sub.add_parser("ledger")  # noqa: E741
    for k in ("selection", "results", "run-id", "sha", "event", "shard", "out"):
        l.add_argument(f"--{k}", required=True)
    l.add_argument("--mode", choices=("shadow", "enforce"), required=True)
    l.set_defaults(fn=cmd_ledger)
    p = sub.add_parser("apply")
    p.add_argument("--selection", required=True)
    p.add_argument("--args", required=True)
    p.set_defaults(fn=cmd_apply)
    a = ap.parse_args()
    return a.fn(a)


if __name__ == "__main__":
    raise SystemExit(main())

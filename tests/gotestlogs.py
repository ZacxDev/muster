#!/usr/bin/env python3
"""TEMPORARY (discovery): dump every test log Go's cache holds, keyed by test ID.

python3 tests/gotestlogs.py OUT.json   -- reads $(go env GOCACHE)
"""
import json, os, subprocess, sys
cache = subprocess.run(["go", "env", "GOCACHE"], capture_output=True, text=True, check=True).stdout.strip()
out = {}
for sub in os.listdir(cache):
    d = os.path.join(cache, sub)
    if not os.path.isdir(d):
        continue
    for name in os.listdir(d):
        if not name.endswith("-a"):
            continue
        try:
            parts = open(os.path.join(d, name)).read().split()
        except OSError:
            continue
        if len(parts) < 3:
            continue
        oid = parts[2]
        p = os.path.join(cache, oid[:2], oid + "-d")
        try:
            with open(p, "rb") as f:
                head = f.read(11)
                if head != b"# test log\n":
                    continue
                out[name[:-2]] = f.read().decode("utf-8", "replace")
        except OSError:
            continue
json.dump(out, open(sys.argv[1], "w"))
print(f"gotestlogs: {len(out)} test logs from {cache}")

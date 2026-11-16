#!/usr/bin/env bash
# deploy/install.sh's linger(), against a fake loginctl (no systemd, no sudo):
#
#   tests/install-linger-test.sh        (CI: release-dry-run)
#
#   * MUSTER_LINGER=no changes NOTHING: no `loginctl enable-linger` at all,
#     not even the unprivileged one polkit may allow (#707 round 2), and it
#     says linger was left off;
#   * CONTROL: with MUSTER_LINGER unset the same fake IS asked to enable it
#     (so the "not called" above is not a harness that never calls);
#   * linger already on: nothing is asked.
set -euo pipefail

ROOT="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/bin"
# The fake records every call; show-user answers $FAKE_LINGER; enable-linger succeeds.
cat > "$tmp/bin/loginctl" <<'F'
#!/bin/sh
echo "$*" >> "$FAKE_LOG"
case "$1" in show-user) echo "${FAKE_LINGER:-no}" ;; esac
exit 0
F
chmod +x "$tmp/bin/loginctl"
# install.sh with its last line (`main "$@"`) dropped, so linger() can be called alone.
sed '$d' "$ROOT/deploy/install.sh" > "$tmp/lib.sh"
grep -q '^linger() {' "$tmp/lib.sh" || { echo "harness: no linger() in install.sh"; exit 2; }
run() { # <MUSTER_LINGER or -> <FAKE_LINGER>
  : > "$tmp/log"
  env PATH="$tmp/bin:$PATH" FAKE_LOG="$tmp/log" FAKE_LINGER="$2" ${1:+MUSTER_LINGER="$1"} \
    sh -c ". '$tmp/lib.sh'; linger" > "$tmp/out" 2>&1 </dev/null
}
fail=0
check() { if eval "$2"; then echo "ok   $1"; else echo "FAIL $1: calls [$(tr '\n' ';' < "$tmp/log")] output: $(cat "$tmp/out")"; fail=1; fi; }

run no no
check "MUSTER_LINGER=no: loginctl enable-linger is never called" '! grep -q enable-linger "$tmp/log"'
check "MUSTER_LINGER=no: it says linger was left off" 'grep -q "linger left OFF" "$tmp/out"'
run "" no
check "CONTROL, MUSTER_LINGER unset: enable-linger IS called (the self-enable polkit may allow)" 'grep -q "^enable-linger " "$tmp/log"'
run yes yes
check "linger already on: nothing is enabled" '! grep -q enable-linger "$tmp/log"'
exit "$fail"

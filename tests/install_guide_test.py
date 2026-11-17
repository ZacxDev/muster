#!/usr/bin/env python3
"""INSTALL.md's shell blocks, run for real against faked machines.

    python3 tests/install_guide_test.py            # the guide in this checkout
    MI_GUIDE=path/to/INSTALL.md python3 tests/install_guide_test.py
    MT_DOC=path/to/multi-tenant.md python3 tests/install_guide_test.py

Each case finds a block of INSTALL.md by what it contains, runs it in a fresh
`sh` with HOME in a temp directory and fake `docker`, `curl`, `tailscale`,
`ssh`, `systemctl` and `journalctl` first on PATH (every call logged), and
asserts what the block WROTE (.env, the state file, muster-mirror's settings)
or CALLED. Nothing touches this machine's Docker, Tailscale, services or
network.

Step 2 runs the REAL deploy/install.sh: the fake curl serves a fake release
(this checkout's install.sh, stamped, plus tiny muster and muster-mirror
binaries in real tarballs and a real checksums.txt), so the guide's own
download, checksum and hand-over are exercised end to end, and install.sh's
merge of muster-mirror's settings is what the env-merge cases observe.

What it pins: a failed version lookup is never saved; muster-mirror's
settings are MERGED (a hand-added line such as TMUX_TMPDIR survives) and never
taken over unasked; a crash-looping muster-mirror prints its own reason line;
an existing muster or Tailscale config is never taken over (a signed-out
Tailscale is `unready`, not `none`); "start over" deletes only this guide's own
fresh install; a secret never reaches argv; the ssh mint keeps only the token;
a release's copy of the guide installs that release; the second-server page's
blocks load the helpers (else Linux takes the macOS branch).
"""

from __future__ import annotations

import hashlib
import io
import json
import os
import platform
import re
import stat
import subprocess
import tarfile
import tempfile
import textwrap
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
GUIDE = Path(os.environ.get("MI_GUIDE", ROOT / "INSTALL.md"))
MT_DOC = Path(os.environ.get("MT_DOC", ROOT / "docs/multi-tenant-hosts.md"))
URL = "https://hub.example-tailnet.ts.net"
MARK = "# written by muster's install.sh; edit freely, lines it does not own are kept"
RELEASE = "9.9.9"


def sh_blocks(path: Path) -> list[str]:
    text = path.read_text()
    out = []
    for m in re.finditer(r"^([ \t]*)```sh\n(.*?)^\1```", text, re.S | re.M):
        body = m.group(2)
        lines = body.split("\n")
        ind = min((len(l) - len(l.lstrip(" ")) for l in lines if l.strip()), default=0)
        out.append("\n".join(l[ind:] for l in lines))
    return out


def blocks() -> list[str]:
    return sh_blocks(GUIDE)


def block(*needles: str) -> str:
    for b in blocks():
        if all(n in b for n in needles):
            return b
    raise AssertionError(f"INSTALL.md has no sh block containing {needles!r}")


def headings(md: str) -> list[str]:
    """Every heading's text, outside code fences, with a leading "Step N: " dropped."""
    out, fenced = [], False
    for line in md.splitlines():
        if line.lstrip().startswith("```"):
            fenced = not fenced
            continue
        m = re.match(r"^#{1,6}\s+(.*?)\s*$", line)
        if m and not fenced:
            out.append(re.sub(r"^Step \d+: ", "", m.group(1)))
    return out


FAKE_DOCKER = r"""#!/bin/sh
echo "docker $*" >> "$FAKE_LOG"
case "$*" in
  *"mirror token create"*) eval "h=\${$#}"; echo "minted mirror token 9 for host $h" >&2; echo "mmt_token-for-$h"; exit 0 ;;
  *"show-secret ui-password"*) echo "pw-fixture-0123456789"; exit 0 ;;
  *"show-secret hook-token"*) echo "hook-fixture-0123456789"; exit 0 ;;
  "compose version"*) [ "${FAKE_NO_COMPOSE:-0}" = 1 ] && exit 1; echo "Docker Compose version v2.29.0"; exit 0 ;;
  info*) [ "${FAKE_DOCKER_INFO_FAIL:-0}" = 1 ] && exit 1; exit 0 ;;
  compose*" config"*) echo "name: muster"; echo "services:"; echo '        published: "8105"'; exit 0 ;;
  compose*" up "*) [ "${FAKE_UP_FAIL:-0}" = 1 ] && { echo "container muster-muster-1 is unhealthy" >&2; exit 1; }; exit 0 ;;
  compose*" logs"*) [ -n "${FAKE_COMPOSE_LOGS:-}" ] && printf '%s\n' "$FAKE_COMPOSE_LOGS"; exit 0 ;;
esac
case "$1 $2" in
  "ps -a")
    case "$*" in *--format*) for i in ${FAKE_PS_IDS:-}; do echo "  container name-$i"; done ;;
                 *) for i in ${FAKE_PS_IDS:-}; do echo "$i"; done ;; esac; exit 0 ;;
  "volume ls")
    case "$*" in *--format*) for v in ${FAKE_VOLUMES:-}; do echo "  volume $v"; done ;;
                 *) for v in ${FAKE_VOLUMES:-}; do echo "$v"; done ;; esac; exit 0 ;;
  "inspect -f")
    id=$4; case "$3" in *working_dir*) eval "echo \"\${FAKE_WD_$id:-}\"" ;; *) echo "/name-$id" ;; esac; exit 0 ;;
esac
exit 0
"""

# curl: the release's assets come from FAKE_RELEASE_DIR (by file name); the
# hub, the local stack and the mirror's host route answer as the case says.
FAKE_CURL = r"""#!/bin/sh
echo "curl $*" >> "$FAKE_LOG"
url=""; out=""; prev=""; w=0
for a in "$@"; do
  [ "$prev" = "-o" ] && out=$a
  [ "$a" = "-w" ] && w=1
  case "$a" in http*) url=$a ;; esac
  prev=$a
done
case "$url" in
  *releases/latest) [ -n "${FAKE_LATEST_URL:-}" ] || exit 22; printf '%s' "$FAKE_LATEST_URL"; exit 0 ;;
  *api.github.com*) printf '%s\n' "${FAKE_API_BODY:-}"; exit 0 ;;
  http://127.0.0.1:*/health) [ "${FAKE_PORT_TAKEN:-0}" = 1 ] && exit 0; exit 7 ;;
  */mirror/host)
    if [ "$w" = 1 ]; then printf '%s' "${FAKE_MIRROR_CODE:-200}"
    elif [ "${FAKE_MIRROR_OFFLINE:-0}" = 1 ]; then echo '{"online": false}'
    else echo '{"online": true}'; fi; exit 0 ;;
  */readyz) exit 0 ;;
  http://localhost:*/health) echo '{"status":"ok"}'; exit 0 ;;
  */health) printf '%s\n' "${FAKE_HEALTH:-}"; exit 0 ;;
esac
if [ -n "$out" ]; then
  if [ -n "${FAKE_RELEASE_DIR:-}" ]; then
    f="$FAKE_RELEASE_DIR/${url##*/}"
    [ -f "$f" ] || exit 22
    cp "$f" "$out"; exit 0
  fi
  printf 'downloaded %s\n' "$url" > "$out"; exit 0
fi
exit 0
"""

FAKE_TAILSCALE = r"""#!/bin/sh
echo "tailscale $*" >> "$FAKE_LOG"
case "$*" in
  "serve status"*)
    [ "${FAKE_SERVE_FAIL:-0}" = 1 ] && { echo "failed to connect to local tailscaled; it doesn't appear to be running" >&2; exit 1; }
    cat "$FAKE_SERVE"; exit 0 ;;
  "serve --bg --https=443 "*)
    printf 'https://hub.example-tailnet.ts.net (tailnet only)\n|-- / proxy http://127.0.0.1:%s\n' "${4}" > "$FAKE_SERVE"; exit 0 ;;
  "debug prefs"*)
    [ -n "${FAKE_TS_PREFS:-}" ] || { echo "unknown subcommand" >&2; exit 1; }
    printf '%s\n' "$FAKE_TS_PREFS"; exit 0 ;;
  "status --json"*)
    [ "${FAKE_TS_LOGGED_OUT:-0}" = 1 ] && exit 1
    printf '%s\n' "${FAKE_TS_JSON:-}"; exit 0 ;;
  "status"*) [ "${FAKE_TS_LOGGED_OUT:-0}" = 1 ] && { echo "Logged out." >&2; exit 1; }; exit 0 ;;
esac
exit 0
"""

# The hub, over ssh: noisy start-up files, then the script on stdin, run by sh
# with the fake docker first on PATH.
FAKE_SSH = r"""#!/bin/sh
echo "ssh $*" >> "$FAKE_LOG"
echo "Welcome to the hub! (motd on stdout)"
while [ "$1" != "sh" ] && [ $# -gt 0 ]; do shift; done
shift
[ "$1" = "-l" ] && shift
sh "$@"; rc=$?
echo "Connection to hub closed. (logout noise on stdout)"
exit $rc
"""

FAKE_LOGINCTL = r"""#!/bin/sh
echo "loginctl $*" >> "$FAKE_LOG"
case "$1" in show-user) echo "${FAKE_LINGER:-yes}" ;; esac
exit 0
"""

FAKE_SUDO = r"""#!/bin/sh
echo "sudo $*" >> "$FAKE_LOG"
[ "$1" = -n ] && shift
[ "${FAKE_SUDO:-ok}" = fail ] && { echo "sudo: a password is required" >&2; exit 1; }
exec "$@"
"""

# uname, with `-s` replaced by FAKE_UNAME_S when set (a macOS run on Linux).
FAKE_UNAME = r"""#!/bin/sh
[ "$1" = -s ] && [ -n "${FAKE_UNAME_S:-}" ] && { echo "$FAKE_UNAME_S"; exit 0; }
exec "$REAL_UNAME" "$@"
"""

# systemctl: logs; a user manager is there; the service's restart count and
# sub-state are the case's (FAKE_NRESTARTS, FAKE_SUBSTATE), or a sequence, one
# word per call, the last repeated (FAKE_NRESTARTS_SEQ, FAKE_SUBSTATE_SEQ).
FAKE_SYSTEMCTL = r"""#!/bin/sh
echo "systemctl $*" >> "$FAKE_LOG"
nth() { # NAME: the next word of FAKE_<NAME>_SEQ, the last one repeated
  f="$FAKE_LOG.$1"; n=$(cat "$f" 2>/dev/null || echo 0); n=$((n + 1)); echo "$n" > "$f"
  eval "set -- \$FAKE_$1_SEQ"; i=1; v=""
  for w; do v=$w; [ "$i" -ge "$n" ] && break; i=$((i + 1)); done
  echo "$v"
}
case "$*" in
  *show-environment*) exit 0 ;;
  *NRestarts*) if [ -n "${FAKE_NRESTARTS_SEQ:-}" ]; then nth NRESTARTS; else echo "${FAKE_NRESTARTS:-0}"; fi ;;
  *SubState*) if [ -n "${FAKE_SUBSTATE_SEQ:-}" ]; then nth SUBSTATE; else echo "${FAKE_SUBSTATE:-running}"; fi ;;
  *is-active*) echo "${FAKE_ACTIVE:-active}" ;;
esac
exit 0
"""

# journalctl: FAKE_JOURNAL is this run's lines; FAKE_JOURNAL_OLD, lines from
# before it, which `--since` leaves out as the real one does.
FAKE_JOURNALCTL = r"""#!/bin/sh
echo "journalctl $*" >> "$FAKE_LOG"
case " $* " in *" --since "*) ;; *) [ -n "${FAKE_JOURNAL_OLD:-}" ] && printf '%s\n' "$FAKE_JOURNAL_OLD" ;; esac
[ -n "${FAKE_JOURNAL:-}" ] && printf '%s\n' "$FAKE_JOURNAL"
exit 0
"""

FAKE_LOGGER = r"""#!/bin/sh
echo "$(basename "$0") $*" >> "$FAKE_LOG"
exit 0
"""

# A login shell, `"$SHELL" -lc CMD`, as a real one behaves: it keeps the PATH
# it was started with (none under `env -i`: then a system default), and its
# profile prepends the directory named in $HOME/.profile-path, if any.
FAKE_LOGIN_SHELL = r"""#!/bin/sh
[ "$1" = -lc ] || exit 9
PATH="${PATH:-/usr/bin:/bin}"
[ -f "$HOME/.profile-path" ] && { read -r d < "$HOME/.profile-path"; PATH="$d:$PATH"; }
export PATH
exec /bin/sh -c "$2"
"""

# awk, recorded: every argv awk is called with (env_set's secrets must not be
# in it), then the real awk.
FAKE_AWK = r"""#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_AWK_ARGV"
exec "$REAL_AWK" "$@"
"""

# The release's binaries: the CLI logs its arguments (doctor prints a report on
# stderr and its JSON on stdout, as the real one does); the mirror only logs.
FAKE_MUSTER_BIN = """#!/bin/sh
echo "muster $*" >> "$FAKE_LOG"
case "$1" in doctor) echo "PASS  server" >&2; echo '{"ok": true, "checks": []}' ;; esac
exit 0
"""
FAKE_MIRROR_BIN = """#!/bin/sh
echo "muster-mirror $*" >> "$FAKE_LOG"
exit 0
"""


def host_os_arch() -> tuple[str, str]:
    os_ = {"Linux": "linux", "Darwin": "darwin"}[platform.system()]
    arch = {"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[platform.machine()]
    return os_, arch


class Guide(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = Path(tempfile.mkdtemp(prefix="mi-guide-"))
        self.homedir = self.tmp / "home"
        self.homedir.mkdir()
        self.bin = self.tmp / "bin"
        self.bin.mkdir()
        self.log = self.tmp / "calls.log"
        self.log.touch()
        self.serve_file = self.tmp / "serve-status"
        self.serve_file.write_text("No serve config\n")
        self.awk_argv = self.tmp / "awk-argv"
        self.awk_argv.touch()
        real_awk = subprocess.run(["sh", "-c", "command -v awk"], capture_output=True, text=True).stdout.strip()
        real_uname = subprocess.run(["sh", "-c", "command -v uname"], capture_output=True, text=True).stdout.strip()
        for name, body in [("docker", FAKE_DOCKER), ("curl", FAKE_CURL), ("tailscale", FAKE_TAILSCALE),
                           ("ssh", FAKE_SSH), ("awk", FAKE_AWK), ("loginctl", FAKE_LOGINCTL), ("sudo", FAKE_SUDO),
                           ("systemctl", FAKE_SYSTEMCTL), ("journalctl", FAKE_JOURNALCTL), ("qrencode", FAKE_LOGGER),
                           ("launchctl", FAKE_LOGGER), ("tmux", FAKE_LOGGER), ("login-shell", FAKE_LOGIN_SHELL),
                           ("uname", FAKE_UNAME)]:
            p = self.bin / name
            p.write_text(body)
            p.chmod(0o755)
        self.env = {
            "HOME": str(self.homedir), "PATH": f"{self.bin}:{os.environ['PATH']}", "FAKE_LOG": str(self.log),
            "FAKE_SERVE": str(self.serve_file), "FAKE_AWK_ARGV": str(self.awk_argv), "REAL_AWK": real_awk,
            "REAL_UNAME": real_uname,
        }
        self.run_block(block('cat > "$HOME/.config/muster-install/lib.sh"'))
        self.log.write_text("")  # Step 0's own probes are not the case's calls
        self.lib = f'. "{self.homedir}/.config/muster-install/lib.sh"\n'
        self.muster = self.homedir / "muster"
        self.cfg = self.homedir / ".config/muster-mirror"

    def tearDown(self) -> None:
        subprocess.run(["rm", "-rf", str(self.tmp)])

    def run_block(self, script: str, **env: str) -> str:
        p = subprocess.run(["sh", "-c", script], env={**self.env, **env}, capture_output=True, text=True,
                           cwd=self.tmp, timeout=120)
        return p.stdout + p.stderr

    def without(self, *names: str) -> str:
        """A PATH on which NAMES do not exist: the fakes minus NAMES, then a
        directory of links to every other program on this machine's PATH."""
        sysbin = self.tmp / ("sys-" + "-".join(names))
        if sysbin.exists():
            return f"{self.tmp / ('fakes-' + '-'.join(names))}:{sysbin}"
        sysbin.mkdir()
        for d in os.environ["PATH"].split(":"):
            if not os.path.isdir(d):
                continue
            for n in os.listdir(d):
                f = os.path.join(d, n)
                if n in names or (sysbin / n).exists() or not os.access(f, os.X_OK) or os.path.isdir(f):
                    continue
                (sysbin / n).symlink_to(f)
        fakes = self.tmp / ("fakes-" + "-".join(names))
        fakes.mkdir()
        for f in self.bin.iterdir():
            if f.name not in names:
                (fakes / f.name).symlink_to(f)
        path = f"{fakes}:{sysbin}"
        check = subprocess.run(["sh", "-c", "for n in " + " ".join(names) + "; do command -v $n && exit 1; done; exit 0"],
                               env={"PATH": path}, capture_output=True, text=True)
        assert check.returncode == 0, f"still on PATH: {check.stdout}"
        return path

    def sh(self, script: str, **env: str) -> str:
        return self.run_block(self.lib + textwrap.dedent(script), **env)

    def calls(self) -> str:
        return self.log.read_text()

    def state(self) -> str:
        p = self.homedir / ".config/muster-install/state"
        return p.read_text() if p.exists() else ""

    def dotenv(self) -> dict[str, str]:
        out = {}
        for line in (self.muster / ".env").read_text().splitlines():
            if "=" in line and not line.startswith("#"):
                k, v = line.split("=", 1)
                out[k] = v
        return out

    def hub(self, **saved: str) -> None:
        """A hub after Step 2: install.sh's ~/muster and muster-mirror settings."""
        self.muster.mkdir(exist_ok=True)
        (self.muster / "docker-compose.yml").write_text("services: {}\n")
        (self.muster / ".muster-install").write_text("installed-by: install.sh\n")
        self.cfg.mkdir(parents=True, exist_ok=True)
        (self.cfg / "env").write_text(f"{MARK}\nMUSTER_MIRROR_URL=http://localhost:8105\nPATH=/usr/bin\n")
        saved = {"MUSTER_URL": "http://localhost:8105", "OS": "linux", **saved}
        self.sh("".join(f"mi_save {k} '{v}'\n" for k, v in saved.items()))

    # --- a fake release, and Step 2 against it --------------------------------

    def release(self) -> Path:
        """The release's assets: this checkout's install.sh (stamped), tiny
        muster and muster-mirror binaries in real tarballs, a compose file and
        a real checksums.txt."""
        rel = self.tmp / "release"
        if rel.exists():
            return rel
        rel.mkdir()
        src = (ROOT / "deploy/install.sh").read_text()
        self.assertIn('RELEASE="@MUSTER_RELEASE@"', src, "deploy/install.sh lost its release stamp line")
        (rel / "install.sh").write_text(src.replace('RELEASE="@MUSTER_RELEASE@"', f'RELEASE="{RELEASE}"', 1))
        (rel / "docker-compose.yml").write_text("name: muster\nservices: {}\n")
        # Both OSes, so a macOS run (FAKE_UNAME_S=Darwin) on Linux finds its assets.
        _, arch = host_os_arch()
        for os_, (name, body) in [(o, nb) for o in ("linux", "darwin")
                                  for nb in (("muster", FAKE_MUSTER_BIN), ("muster-mirror", FAKE_MIRROR_BIN))]:
            buf = io.BytesIO()
            with tarfile.open(fileobj=buf, mode="w:gz") as tf:
                data = body.encode()
                ti = tarfile.TarInfo(name)
                ti.size, ti.mode = len(data), 0o755
                tf.addfile(ti, io.BytesIO(data))
            (rel / f"{name}_{os_}_{arch}.tar.gz").write_bytes(buf.getvalue())
        lines = [f"{hashlib.sha256(f.read_bytes()).hexdigest()}  {f.name}" for f in sorted(rel.iterdir())]
        (rel / "checksums.txt").write_text("\n".join(lines) + "\n")
        return rel

    def install_env(self, **env: str) -> dict[str, str]:
        return {"FAKE_RELEASE_DIR": str(self.release()), "MUSTER_RELEASE_OVERRIDE": "https://release.example/dl",
                "FAKE_LATEST_URL": f"https://github.com/ZacxDev/muster/releases/tag/v{RELEASE}",
                "FAKE_HEALTH": f'{{"status":"ok","version":"{RELEASE}"}}', "USER": "tester",
                "SHELL": str(self.bin / "login-shell"), **env}

    def step2(self, **env: str) -> str:
        return self.run_block(self.lib + block("MUSTER_MIRROR_INTERACTIVE"), **self.install_env(**env))

    def joining(self, legacy_env: str | None = None, **saved: str) -> None:
        """A joining machine after Step 1 and its token: the hub's URL saved."""
        self.cfg.mkdir(parents=True, exist_ok=True)
        (self.cfg / "token").write_text("mmt_fixture\n")
        (self.cfg / "token").chmod(0o600)
        if legacy_env is not None:
            (self.cfg / "env").write_text(legacy_env)
        saved = {"OS": "linux", "ROLE": "join", "MUSTER_URL": URL, "MIRROR_HOST": "laptop", "INTERACTIVE": "1", **saved}
        self.sh("".join(f"mi_save {k} '{v}'\n" for k, v in saved.items()))

    def mirror_env(self) -> list[str]:
        return (self.cfg / "env").read_text().splitlines()

    # --- Step 2: muster-mirror's settings are merged, never overwritten --------

    LEGACY = ("MUSTER_MIRROR_URL=https://old-hub.example-tailnet.ts.net\nMUSTER_MIRROR_TOKEN_FILE=/old/token\n"
              "MUSTER_MIRROR_HOST=laptop\nMUSTER_MIRROR_INTERACTIVE=0\nMUSTER_MIRROR_WRITE_HOOK_CONFIG=1\n"
              "TMUX_TMPDIR=/run/user/1000/tmux-here\nPATH=/old/path\n")

    def test_step2_merges_the_mirror_settings_keeping_hand_added_lines(self) -> None:
        # The finding: Step 7 wrote ~/.config/muster-mirror/env with `>`, so a
        # TMUX_TMPDIR the person had added was silently dropped and sessions
        # vanished. Now install.sh rewrites only the lines it owns.
        self.joining(legacy_env=self.LEGACY, ADOPT_MIRROR="1")
        out = self.step2()
        lines = self.mirror_env()
        self.assertIn("TMUX_TMPDIR=/run/user/1000/tmux-here", lines, f"a hand-added line was dropped\n{out}")
        self.assertIn(f"MUSTER_MIRROR_URL={URL}", lines, f"positive control: the settings were rewritten\n{out}")
        for key, want in (("MUSTER_MIRROR_URL", URL), ("MUSTER_MIRROR_INTERACTIVE", "1"),
                          ("MUSTER_MIRROR_TOKEN_FILE", str(self.cfg / "token"))):
            got = [l for l in lines if l.startswith(key + "=")]
            self.assertEqual(got, [f"{key}={want}"], f"{key} is not written exactly once with the new value")
        self.assertEqual(sum(l.startswith("PATH=") for l in lines), 1)
        # And a second run changes nothing it does not own.
        (self.cfg / "env").write_text((self.cfg / "env").read_text() + "MUSTER_MIRROR_MCP_LISTEN=127.0.0.1:1\n")
        self.step2()
        again = self.mirror_env()
        self.assertIn("MUSTER_MIRROR_MCP_LISTEN=127.0.0.1:1", again)
        self.assertIn("TMUX_TMPDIR=/run/user/1000/tmux-here", again)
        self.assertEqual(sum(l.startswith("MUSTER_MIRROR_URL=") for l in again), 1)

    def test_step2_never_takes_over_mirror_settings_unasked(self) -> None:
        self.joining(legacy_env=self.LEGACY)
        out = self.step2()
        self.assertIn("MUSTER_MIRROR_ADOPT=1", out, f"the refusal does not say how to allow it\n{out}")
        self.assertEqual((self.cfg / "env").read_text(), self.LEGACY, "settings it did not write were changed")
        self.assertNotIn("systemctl --user restart", self.calls())

    def test_step2_join_points_the_mirror_at_the_hub_and_checks_from_here(self) -> None:
        self.joining()
        out = self.step2()
        self.assertIn(f"MUSTER_MIRROR_URL={URL}", self.mirror_env(), out)
        self.assertIn(MARK, self.mirror_env())
        self.assertIn(f"muster doctor --api-url {URL}", self.calls(), "the joining machine's doctor did not run")
        self.assertNotIn("compose", self.calls(), "a joining machine touched Docker")
        self.assertNotIn("show-secret", self.calls())

    def test_step2_join_without_a_token_says_so_and_installs_nothing(self) -> None:
        self.joining()
        (self.cfg / "token").unlink()
        out = self.step2()
        self.assertIn("NO TOKEN", out)
        self.assertFalse((self.homedir / ".local/bin/muster-mirror").exists())

    def test_step2_crash_loop_prints_the_mirrors_own_reason(self) -> None:
        # The finding: a crash-looping muster-mirror reads `activating`, and
        # `journalctl -n 15` buries its reason in restart noise. The installer
        # names the line muster-mirror printed (they start "muster-mirror: ";
        # its ordinary log lines carry a timestamp) and does not wait 30s.
        self.joining()
        reason = 'muster-mirror: MUSTER_MIRROR_AGENT_SPAWN_SKIP_PERMISSIONS="maybe" is not a bool'
        journal = "\n".join([
            "2026/11/16 10:00:00 muster-mirror 9.9.9 starting",
            reason,
            "muster-mirror.service: Main process exited, code=exited, status=2/INVALIDARGUMENT",
            "muster-mirror.service: Failed with result 'exit-code'.",
            "muster-mirror.service: Scheduled restart job, restart counter is at 4.",
        ])
        out = self.step2(FAKE_MIRROR_OFFLINE="1", FAKE_SUBSTATE="auto-restart", FAKE_JOURNAL=journal)
        self.assertIn("exits as it starts", out, out)
        self.assertIn("    " + reason, out, f"the mirror's own reason was not printed\n{out}")
        self.assertNotIn("Scheduled restart job", out, "the journal's restart noise was printed instead of the reason")
        self.assertNotIn("has not connected after 30s", out, "it waited the whole 30s on a crash loop")

    # --- Step 2: a crash loop is a loop, and its reason is this run's ---------

    def nosleep(self) -> str:
        """PATH with a `sleep` that returns at once, so the 30-poll wait ends fast."""
        d = self.tmp / "nosleep"
        if not d.exists():
            d.mkdir()
            (d / "sleep").write_text("#!/bin/sh\nexit 0\n")
            (d / "sleep").chmod(0o755)
        return f"{d}:{self.env['PATH']}"

    def test_step2_one_restart_is_not_a_crash_loop(self) -> None:
        # #728 round 0: ONE restart since this run's own (a mirror that lost
        # its first connection) was called a crash loop. It takes two.
        self.joining()
        out = self.step2(FAKE_MIRROR_OFFLINE="1", FAKE_NRESTARTS_SEQ="0 1", PATH=self.nosleep())
        self.assertNotIn("exits as it starts", out, f"one restart was called a crash loop\n{out}")
        self.assertIn("has not connected after 30s", out, f"positive control: the wait ran to its end\n{out}")
        # control: two restarts are a loop
        self.log.write_text("")
        for f in self.tmp.glob("calls.log.*"):
            f.unlink()
        out = self.step2(FAKE_MIRROR_OFFLINE="1", FAKE_NRESTARTS_SEQ="0 2", PATH=self.nosleep())
        self.assertIn("exits as it starts", out, f"control: two restarts were not called a loop\n{out}")

    def test_step2_one_auto_restart_poll_is_not_a_crash_loop(self) -> None:
        # `auto-restart` on ONE poll is one restart; held across two polls it is a loop.
        self.joining()
        out = self.step2(FAKE_MIRROR_OFFLINE="1", FAKE_SUBSTATE_SEQ="auto-restart running", PATH=self.nosleep())
        self.assertNotIn("exits as it starts", out, f"one auto-restart poll was called a crash loop\n{out}")
        self.assertIn("has not connected after 30s", out, f"positive control: the wait ran to its end\n{out}")
        self.log.write_text("")
        for f in self.tmp.glob("calls.log.*"):
            f.unlink()
        out = self.step2(FAKE_MIRROR_OFFLINE="1", FAKE_SUBSTATE_SEQ="running auto-restart", PATH=self.nosleep())
        self.assertIn("exits as it starts", out, f"control: auto-restart on two polls was not called a loop\n{out}")

    def test_step2_macos_waits_plainly(self) -> None:
        # #728 round 0 (D2): the launchd crash-loop reading (`launchctl print`,
        # "last exit code") had no test and no Mac measurement; it is gone. On
        # macOS the wait is the plain one and the doctor says why.
        self.joining(OS="darwin")
        out = self.step2(FAKE_UNAME_S="Darwin", FAKE_MIRROR_OFFLINE="1", PATH=self.nosleep())
        self.assertIn("launchctl bootstrap", self.calls(), f"positive control: the launchd path ran\n{out}")
        self.assertNotIn("launchctl print", self.calls(), "it still reads launchctl print for a crash loop")
        self.assertIn("has not connected after 30s", out, out)

    def test_step2_crash_loop_reason_is_from_this_run(self) -> None:
        # The reason is the last "muster-mirror: " line SINCE this run started
        # the service, not the last of the journal's 200, which may be an
        # earlier run's.
        self.joining()
        old = "muster-mirror: an EARLIER run's reason"
        out = self.step2(FAKE_MIRROR_OFFLINE="1", FAKE_SUBSTATE="auto-restart", FAKE_JOURNAL_OLD=old,
                         FAKE_JOURNAL="2026/11/16 10:00:00 muster-mirror 9.9.9 starting")
        self.assertIn("exits as it starts", out, f"positive control: the loop was named\n{out}")
        self.assertNotIn(old, out, f"an earlier run's reason was printed as this one's\n{out}")
        self.assertIn("it printed no reason", out, out)
        journal = [l for l in self.calls().splitlines() if l.startswith("journalctl ")]
        self.assertTrue(journal and all(" --since " in l for l in journal), journal)
        # control: this run's own reason is still printed
        self.log.write_text("")
        out = self.step2(FAKE_MIRROR_OFFLINE="1", FAKE_SUBSTATE="auto-restart", FAKE_JOURNAL_OLD=old,
                         FAKE_JOURNAL="muster-mirror: THIS run's reason")
        self.assertIn("    muster-mirror: THIS run's reason", out, out)

    # --- Step 2: owned keys in any spelling are owned -------------------------

    def test_step2_takes_export_and_indented_owned_lines_as_its_own(self) -> None:
        # #728 round 0: `export MUSTER_MIRROR_URL=…` or an indented owned line
        # was kept as a "line it does not own", beside the new one.
        legacy = ("export MUSTER_MIRROR_URL=https://old-hub.example-tailnet.ts.net\n"
                  "  MUSTER_MIRROR_HOST=old-laptop\n"
                  "\tPATH=/old/path\n"
                  "export  MUSTER_MIRROR_TOKEN_FILE=/old/token\n"
                  "export TMUX_TMPDIR=/run/user/1000/tmux-here\n")
        self.joining(legacy_env=legacy, ADOPT_MIRROR="1")
        out = self.step2()
        lines = self.mirror_env()
        self.assertIn(f"MUSTER_MIRROR_URL={URL}", lines, f"positive control: the settings were rewritten\n{out}")
        for stale in ("old-hub", "old-laptop", "/old/path", "/old/token"):
            self.assertFalse([l for l in lines if stale in l], f"an owned line ({stale}) was kept as a duplicate: {lines}")
        key = lambda l: re.sub(r"^\s*(export\s+)?", "", l).split("=", 1)[0]
        for k in ("MUSTER_MIRROR_URL", "MUSTER_MIRROR_HOST", "MUSTER_MIRROR_TOKEN_FILE", "PATH"):
            self.assertEqual(sum(key(l) == k for l in lines), 1, f"{k} is not written exactly once: {lines}")
        self.assertIn("export TMUX_TMPDIR=/run/user/1000/tmux-here", lines, "a line it does not own was dropped")

    # --- Step 2: a failed start never shows the password ----------------------

    def secret_marker(self) -> str:
        m = re.search(r'secretLineMarker = "([^"]+)"', (ROOT / "cmd/muster-server/secretfiles.go").read_text())
        self.assertIsNotNone(m, "cmd/muster-server/secretfiles.go has no secretLineMarker")
        return m.group(1)

    PW = "pw-generated-by-muster-init-0123456789"

    def init_logs(self) -> str:
        """muster-init's and muster's logs as `docker compose logs` prints them,
        with the generated password's line exactly as muster-init prints it."""
        return "\n".join([
            "muster-init-1  | muster-init: generated muster's sign-in password (shown this once):",
            f"muster-init-1  | muster-init: {self.secret_marker()}     {self.PW}",
            "muster-init-1  | muster-init: show it again any time: docker compose exec muster /muster-server show-secret ui-password",
            "muster-init-1  | muster-init: configuration OK",
            "muster-1       | muster-server: MUSTER_EXAMPLE_SETTING=\"x\" is not a bool",
        ])

    def test_step2_a_failed_start_never_shows_the_password(self) -> None:
        # #728 round 0: install.sh printed `logs --tail 15 muster-init muster`
        # on a failed start, and muster-init's first run prints the generated
        # password: into the agent's transcript.
        self.sh("mi_save OS linux\nmi_save ROLE hub\nmi_save MIRROR_HOST hubbox\nmi_save INTERACTIVE 0")
        out = self.step2(FAKE_UP_FAIL="1", FAKE_COMPOSE_LOGS=self.init_logs())
        self.assertIn("the stack did not come up", out, f"positive control: the failure path ran\n{out}")
        self.assertIn('MUSTER_EXAMPLE_SETTING="x" is not a bool', out, f"positive control: the logs were shown\n{out}")
        self.assertNotIn(self.PW, out, f"the generated password reached the agent's output\n{out}")
        # The command it suggests for more is the filtered one too.
        hint = [l for l in out.splitlines() if "the stack did not come up" in l]
        self.assertTrue(hint and self.secret_marker() in hint[0], f"the suggested logs command does not drop the password line: {hint}")

    def test_mi_logs_never_shows_the_password(self) -> None:
        # The guide's own way to read the stack's logs (Step 2's and Step 3's
        # "If this fails"): mi_logs, never a bare `mi_dc logs`.
        self.hub()
        out = self.sh("mi_logs --tail=80 muster-init muster", FAKE_COMPOSE_LOGS=self.init_logs())
        self.assertIn("configuration OK", out, f"positive control: the logs were shown\n{out}")
        self.assertNotIn(self.PW, out, out)
        raw = self.sh("mi_dc logs --tail=80 muster-init muster", FAKE_COMPOSE_LOGS=self.init_logs())
        self.assertIn(self.PW, raw, "control: the unfiltered logs hold the password, so the fixture can see it")
        unfiltered = [l for l in GUIDE.read_text().splitlines() if "mi_dc logs" in l and not l.startswith("mi_logs()")]
        self.assertEqual(unfiltered, [], "INSTALL.md tells the agent to read the logs unfiltered")

    # --- Step 2: an installer that cannot do what the guide asks --------------

    def reseal(self, edit) -> None:
        """Rewrite the fake release's install.sh with EDIT, and its checksum."""
        rel = self.release()
        (rel / "install.sh").write_text(edit((rel / "install.sh").read_text()))
        lines = [f"{hashlib.sha256(f.read_bytes()).hexdigest()}  {f.name}" for f in sorted(rel.iterdir())
                 if f.name != "checksums.txt"]
        (rel / "checksums.txt").write_text("\n".join(lines) + "\n")

    def test_step2_refuses_join_or_adopt_with_an_installer_that_lacks_it(self) -> None:
        # An unstamped guide installs the newest release; an installer from
        # before join and adopt would set up a hub where a join was asked for.
        drop = lambda s: re.sub(r"(?m)^# capabilities:.*\n", "", s)
        self.reseal(drop)
        self.joining()
        out = self.step2()
        self.assertIn("OLD INSTALLER", out, out)
        self.assertNotIn("muster-mirror", " ".join(self.calls().splitlines()), "it ran the old installer")
        self.assertFalse((self.homedir / ".local/bin/muster-mirror").exists())
        # adopt alone: a hub whose mirror settings are someone else's
        self.log.write_text("")
        self.sh("mi_save ROLE hub\nmi_save ADOPT_MIRROR 1")
        out = self.step2()
        self.assertIn("OLD INSTALLER", out, out)
        self.assertNotIn("compose", self.calls(), "it ran the old installer")
        # control: the same old installer still installs a plain hub
        self.log.write_text("")
        self.sh("mi_save ADOPT_MIRROR ''")
        out = self.step2()
        self.assertNotIn("OLD INSTALLER", out, out)
        self.assertIn("muster doctor --deploy-env", self.calls(), f"control: a plain hub install ran\n{out}")

    def test_step2_hub_installs_locally_and_records_the_local_address(self) -> None:
        self.sh("mi_save OS linux\nmi_save ROLE hub\nmi_save MIRROR_HOST hubbox\nmi_save INTERACTIVE 0")
        out = self.step2()
        self.assertIn("MUSTER_MIRROR_URL=http://localhost:8105", self.mirror_env(), out)
        self.assertIn("MUSTER_URL='http://localhost:8105'", self.state())
        self.assertIn("GUIDE_STARTED='1'", self.state())
        self.assertIn("GUIDE_IN_USE='1'", self.state())
        self.assertIn("muster doctor --deploy-env", self.calls(), "the installer's doctor did not run")
        self.assertNotIn("pw-fixture-0123456789", out, "the sign-in password reached the agent's output")
        self.assertIn("show-secret ui-password", out, "the command that shows the password was not printed")
        self.assertIn("mirror token create hubbox", self.calls())

    def test_step2_keeps_the_https_address_on_a_rerun(self) -> None:
        self.sh(f"mi_save OS linux\nmi_save ROLE hub\nmi_save MIRROR_HOST hubbox\nmi_save MUSTER_URL '{URL}'")
        self.step2()
        self.assertIn(f"MUSTER_URL='{URL}'", self.state(), "a re-run replaced the hub's https address with localhost")

    def test_step2_refuses_an_earlier_guides_install(self) -> None:
        self.sh("mi_save OS linux\nmi_save ROLE hub\nmi_save MIRROR_HOST hubbox")
        self.muster.mkdir()
        (self.muster / "docker-compose.yml").write_text("services: {}\n")
        (self.muster / ".env").write_text("POSTGRES_PASSWORD=keep-me\n")
        out = self.step2()
        self.assertIn("did not install", out, out)
        self.assertNotIn("compose", " ".join(l for l in self.calls().splitlines() if " up " in l))
        self.assertEqual((self.muster / ".env").read_text(), "POSTGRES_PASSWORD=keep-me\n")
        self.assertNotIn("GUIDE_STARTED='1'", self.state(), "an earlier install was marked as this guide's own")

    def test_step2_respects_a_declined_linger(self) -> None:
        self.joining(LINGER="no")
        out = self.step2(FAKE_LINGER="no")
        self.assertIn("systemctl --user restart muster-mirror.service", self.calls(), f"positive control\n{out}")
        self.assertNotIn("enable-linger", self.calls(), out)

    def test_the_install_sets_up_mirrored_sessions_only(self) -> None:
        # The README, onboarding.md and Step 4 tell the person this guide
        # starts no session of its own. That is a claim about what it WRITES:
        # the host's service settings are exactly these keys (no
        # MUSTER_MIRROR_RESUME_SPAWN, the one way a host starts a Claude Code
        # process), and the compose file and env template name no provisioner
        # and no mail resume. An INVARIANT guard.
        self.joining()
        self.step2()
        keys = sorted(l.split("=", 1)[0] for l in self.mirror_env() if l.strip() and not l.startswith("#"))
        self.assertEqual(keys, ["MUSTER_MIRROR_HOST", "MUSTER_MIRROR_INTERACTIVE", "MUSTER_MIRROR_TOKEN_FILE",
                                "MUSTER_MIRROR_URL", "MUSTER_MIRROR_WRITE_HOOK_CONFIG", "PATH"])
        compose = (ROOT / "deploy/docker-compose.yml").read_text()
        example = (ROOT / "deploy/.env.example").read_text()
        self.assertRegex(compose, r"(?m)^      MUSTER_HOOK_TOKEN: ", "positive control: the env block is where this looks")
        for key in ("MUSTER_AGENT_PROVISIONER", "MUSTER_AGENT_GATEWAY"):
            self.assertNotRegex(compose, rf"(?m)^\s+{key}:", f"the compose file forwards {key}: dispatch could be turned on")
        self.assertRegex(compose, r"(?m)^      MUSTER_MAIL_RESUME_ALLOW: \$\{MUSTER_MAIL_RESUME_ALLOW:-\}$",
                         "MUSTER_MAIL_RESUME_ALLOW must be forwarded with an EMPTY default")
        for key in ("MUSTER_AGENT_PROVISIONER", "MUSTER_AGENT_GATEWAY", "MUSTER_MAIL_RESUME_ALLOW"):
            self.assertNotRegex(example, rf"(?m)^{key}=", f".env.example sets {key}: the install would no longer be mirrored-only")
        for text in (compose, example):
            self.assertNotIn("RESUME_SPAWN", text)

    def test_step2_names_the_opencode_line_muster_mirror_logs(self) -> None:
        # The seam: the guide quotes the daemon's start-up line as expected, so
        # the line it quotes must be the one the code logs.
        src = (ROOT / "cmd/muster-mirror/opencode.go").read_text()
        m = re.search(r'openCodeNotPairedWhy = "([^"]*)"', src)
        self.assertIsNotNone(m, "cmd/muster-mirror/opencode.go no longer has openCodeNotPairedWhy")
        quoted = re.search(r"`opencode: ([^`…]*) …`", self.section("## Step 2:"))
        self.assertIsNotNone(quoted, "Step 2 does not quote the opencode line")
        self.assertTrue(m.group(1).startswith(quoted.group(1)),
                        f"Step 2 quotes {quoted.group(1)!r}; muster-mirror logs {m.group(1)!r}")

    # --- Step 2: the PATH note only when it is needed --------------------------

    def test_step2_no_path_note_when_a_login_shell_finds_muster(self) -> None:
        self.joining()
        (self.homedir / ".profile-path").write_text(f"{self.homedir}/.local/bin\n")
        self.assertNotIn("NOTE: a new login shell", self.step2())

    def test_step2_path_note_when_a_login_shell_does_not(self) -> None:
        self.joining()
        # The agent's own PATH has the directory; a new login (no profile
        # line) would not. Only `env -i` keeps the check from inheriting it.
        out = self.step2(PATH=f"{self.homedir}/.local/bin:{self.env['PATH']}")
        self.assertIn(f"NOTE: a new login shell does not find {self.homedir}/.local/bin/muster", out)

    # --- the version: a failed lookup is never saved; a release pins its own --

    def test_failed_lookup_saves_nothing(self) -> None:
        self.joining()
        out = self.run_block(self.lib + block("MUSTER_MIRROR_INTERACTIVE"),
                             **self.install_env(FAKE_LATEST_URL="", FAKE_API_BODY='{"message":"API rate limit exceeded"}'))
        self.assertIn("VERSION LOOKUP FAILED", out)
        self.assertNotIn("MUSTER_VERSION=", self.state(), f"a failed lookup was saved\n{out}")
        self.assertNotIn("install.sh", self.calls(), "it downloaded with no version")
        self.assertNotEqual(self.sh("mi_set_version v1.2.3 && echo SAVED").strip(), "SAVED", "a v-prefixed version was accepted")

    def test_lookup_takes_the_latest_redirect(self) -> None:
        self.sh("mi_release", FAKE_LATEST_URL="https://github.com/ZacxDev/muster/releases/tag/v2.3.4")
        self.assertIn("MUSTER_VERSION='2.3.4'", self.state())

    def test_a_releases_copy_of_the_guide_installs_that_release(self) -> None:
        # scripts/release-build.sh stamps MI_GUIDE_RELEASE into the INSTALL.md
        # it publishes; that copy must choose its own release without asking
        # GitHub for the newest. The control: the unstamped guide asks.
        text = GUIDE.read_text()
        line = 'MI_GUIDE_RELEASE="@MUSTER_RELEASE@"'
        self.assertEqual(text.count(line), 1, "INSTALL.md must hold exactly one stampable MI_GUIDE_RELEASE line")
        stamped = self.tmp / "INSTALL.stamped.md"
        stamped.write_text(text.replace(line, 'MI_GUIDE_RELEASE="4.5.6"'))
        lib_block = [b for b in sh_blocks(stamped) if 'cat > "$HOME/.config/muster-install/lib.sh"' in b][0]
        self.run_block(lib_block)
        self.log.write_text("")
        self.sh("mi_release", FAKE_LATEST_URL="https://github.com/ZacxDev/muster/releases/tag/v7.7.7")
        self.assertIn("MUSTER_VERSION='4.5.6'", self.state())
        self.assertNotIn("releases/latest", self.calls(), "a stamped guide still asked for the newest release")
        # control: the repository's unstamped copy asks
        self.run_block(block('cat > "$HOME/.config/muster-install/lib.sh"'))
        self.sh("mi_release", FAKE_LATEST_URL="https://github.com/ZacxDev/muster/releases/tag/v7.7.7")
        self.assertIn("MUSTER_VERSION='7.7.7'", self.state())

    # --- an existing muster is never taken over -------------------------------

    def test_step0_reports_another_install(self) -> None:
        out = self.run_block(block('cat > "$HOME/.config/muster-install/lib.sh"'), FAKE_PS_IDS="abc", FAKE_WD_abc="/srv/x")
        self.assertIn("FOREIGN container", out)

    def test_step0_tells_an_earlier_guides_install_and_mirror_settings_apart(self) -> None:
        self.muster.mkdir()
        (self.muster / "docker-compose.yml").write_text("services: {}\n")
        self.cfg.mkdir(parents=True)
        (self.cfg / "env").write_text(self.LEGACY)
        out = self.step0()
        self.assertIn("existing muster: NOT install.sh's", out)
        self.assertIn("muster-mirror settings: NOT install.sh's", out)
        (self.muster / ".muster-install").write_text("installed-by: install.sh\n")
        (self.cfg / "env").write_text(MARK + "\n" + self.LEGACY)
        out = self.step0()
        self.assertIn("existing muster: install.sh's", out)
        self.assertIn("muster-mirror settings: install.sh's", out)

    # --- starting over deletes only this guide's fresh install ----------------

    def start_over(self, **env: str) -> str:
        return self.run_block(self.lib + block("down -v"), **env)

    def test_start_over_refuses_unless_its_own_fresh_install(self) -> None:
        self.hub()
        out = self.start_over(MI_CONFIRM_DELETE="muster")
        self.assertIn("REFUSED", out, "it would delete an install the guide never started")
        self.hub(GUIDE_STARTED="1", GUIDE_IN_USE="1")
        self.assertIn("REFUSED", self.start_over(MI_CONFIRM_DELETE="muster", FAKE_PS_IDS="abc", FAKE_WD_abc=str(self.muster)),
                      "it would delete an install that has run (and may hold data)")
        self.hub(GUIDE_STARTED="1", GUIDE_IN_USE="0")
        self.assertIn("REFUSED", self.start_over(MI_CONFIRM_DELETE="muster", FAKE_PS_IDS="abc", FAKE_WD_abc="/srv/x"),
                      "it would delete another install's containers")
        self.assertNotIn("down -v", self.calls())

    def test_start_over_names_what_it_deletes_and_needs_the_confirmation(self) -> None:
        self.hub(GUIDE_STARTED="1")
        own = dict(FAKE_PS_IDS="abc", FAKE_WD_abc=str(self.muster), FAKE_VOLUMES="muster_pgdata")
        out = self.start_over(**own)
        self.assertIn("container name-abc", out)
        self.assertIn("volume muster_pgdata", out)
        self.assertNotIn("down -v", self.calls(), "it deleted without the confirmation")
        self.start_over(MI_CONFIRM_DELETE="muster", **own)
        self.assertIn("down -v", self.calls())

    # --- Tailscale serve is added to, never replaced --------------------------

    def serve(self, status: str, **env: str) -> str:
        self.hub()
        self.serve_file.write_text(status)
        return self.run_block(self.lib + block("tailscale serve --bg"), **env)

    def test_serve_adds_only_muster_when_empty(self) -> None:
        self.serve("No serve config\n")
        self.assertIn("tailscale serve --bg --https=443 8105", self.calls())
        self.assertIn(f"MUSTER_URL='{URL}'", self.state())
        self.assertEqual(self.dotenv()["MUSTER_PUBLIC_URL"], URL, "muster was not told its https address")
        self.assertRegex(self.calls(), r"docker compose .* up -d --wait", "muster was not restarted with it")

    def test_serve_uses_the_installs_own_port(self) -> None:
        # The port is install.sh's (the compose file's), never one the guide
        # spells: a hub on another port is served on THAT port.
        self.hub()
        (self.cfg / "env").write_text(f"{MARK}\nMUSTER_MIRROR_URL=http://localhost:9123\n")
        self.serve_file.write_text("No serve config\n")
        self.run_block(self.lib + block("tailscale serve --bg"))
        self.assertIn("tailscale serve --bg --https=443 9123", self.calls())

    def test_serve_keeps_its_own_mapping(self) -> None:
        self.serve(f"{URL} (tailnet only)\n|-- / proxy http://127.0.0.1:8105\n")
        self.assertNotIn("--bg", self.calls())
        self.assertIn(f"MUSTER_URL='{URL}'", self.state())

    def test_serve_stops_on_someone_elses_site(self) -> None:
        out = self.serve(f"{URL} (tailnet only)\n|-- / proxy http://127.0.0.1:3000\n")
        self.assertNotIn("--bg", self.calls(), out)
        self.assertNotIn(f"MUSTER_URL='{URL}'", self.state())

    def test_serve_stops_on_funnel(self) -> None:
        # The mapping is muster's own, so ONLY the Funnel check can stop it.
        out = self.serve(f"{URL} (Funnel on)\n|-- / proxy http://127.0.0.1:8105\n")
        self.assertNotIn("--bg", self.calls())
        self.assertIn("STOP", out)
        self.assertNotIn(f"MUSTER_URL='{URL}'", self.state(), "a Funnel-exposed mapping was adopted as muster's")

    def test_serve_unready_is_not_a_foreign_stop(self) -> None:
        out = self.serve("No serve config\n", FAKE_SERVE_FAIL="1")
        self.assertIn("TAILSCALE NOT READY", out)
        self.assertNotIn("STOP", out)
        self.assertNotIn("--bg", self.calls())
        pre = self.run_block(block('cat > "$HOME/.config/muster-install/lib.sh"'), FAKE_SERVE_FAIL="1")
        self.assertIn("tailscale serve: unready", pre)

    def test_a_signed_out_tailscale_is_unready_not_none(self) -> None:
        # The finding: signed out, `tailscale serve status` still answers "No
        # serve config", so the helper said `none` and Step 2 ran `serve` on a
        # Tailscale that could not serve. Signed out is `unready`.
        self.hub()
        self.serve_file.write_text("No serve config\n")
        self.assertEqual(self.sh("mi_serve_state", FAKE_TS_LOGGED_OUT="1").strip(), "unready")
        self.assertEqual(self.sh("mi_serve_state").strip(), "none", "control: signed in with nothing served is none")

    def test_removal_turns_off_only_musters_mapping(self) -> None:
        # Prose, so a weaker guard: no block runs `serve reset`, every mention
        # of it is a "Never", and removal names muster's own mapping.
        for b in blocks():
            self.assertNotRegex(b, r"tailscale serve reset", "a block wipes every serve mapping")
        text = GUIDE.read_text()
        for m in re.finditer(r"tailscale serve reset", text):
            self.assertIn("Never", text[max(0, m.start() - 40):m.start()], "the guide tells the agent to reset serve")
        self.assertRegex(text, r"tailscale serve --https=443\s+off", "removal does not name muster's mapping")

    # --- optional features: the generic helpers -------------------------------

    def test_attachments_generates_both_secrets_once(self) -> None:
        self.hub()
        self.sh("env_set COMPOSE_PROFILES stt")
        b = self.lib + block("env_secret S3_STORE_SECRET_KEY", "mi_profiles s3")
        out = self.run_block(b)
        env = self.dotenv()
        self.assertEqual(env["COMPOSE_PROFILES"], "stt,s3", out)
        self.assertRegex(env["S3_STORE_SECRET_KEY"], r"^[0-9a-f]{48}$")
        self.assertRegex(env["MUSTER_UPLOAD_SIGNING_KEY"], r"^[0-9a-f]{96}$")
        first = (env["S3_STORE_SECRET_KEY"], env["MUSTER_UPLOAD_SIGNING_KEY"])
        self.run_block(b)
        env = self.dotenv()
        self.assertEqual((env["S3_STORE_SECRET_KEY"], env["MUSTER_UPLOAD_SIGNING_KEY"]), first, "a re-run rotated a key")
        self.assertEqual(env["COMPOSE_PROFILES"], "stt,s3", "a re-run added s3 twice")
        self.assertEqual(stat.S_IMODE((self.muster / ".env").stat().st_mode), 0o600)

    def test_env_set_keeps_secrets_out_of_argv_and_the_file_private(self) -> None:
        self.hub()
        self.sh("umask 022\nenv_set MUSTER_STT_TOKEN sk-secret-in-argv-check")
        self.assertNotIn("sk-secret-in-argv-check", self.awk_argv.read_text())
        self.assertEqual(stat.S_IMODE((self.muster / ".env").stat().st_mode), 0o600)
        self.assertFalse((self.muster / ".env.tmp").exists())
        self.assertEqual(self.dotenv()["MUSTER_STT_TOKEN"], "sk-secret-in-argv-check")

    def test_mi_profiles_replaces_only_its_own(self) -> None:
        self.hub()
        self.sh("env_set COMPOSE_PROFILES s3,stt")
        self.assertEqual(self.sh("mi_profiles stt-cohere stt").strip(), "s3,stt-cohere")
        self.assertEqual(self.sh('mi_profiles "" stt').strip(), "s3")

    # --- the ssh mint ---------------------------------------------------------

    def test_ssh_mint_keeps_only_the_token(self) -> None:
        self.sh("mi_save MIRROR_HOST laptop")
        out = self.run_block(self.lib + block("ssh ", "mirror token create").replace("HUB", "hub.example"))
        tok = (self.cfg / "token").read_text().strip()
        self.assertEqual(tok, "mmt_token-for-laptop", out)
        self.assertIn("BatchMode=yes", self.calls())
        again = self.run_block(self.lib + block("ssh ", "mirror token create").replace("HUB", "hub.example"))
        self.assertIn("kept existing token", again)

    # --- Step 0: ask for sudo and the person's hands up front -----------------

    def step0(self, **env: str) -> str:
        return self.run_block(block('cat > "$HOME/.config/muster-install/lib.sh"'), **env)

    # Indented over many lines, as the real `tailscale status --json` prints it.
    TS_ON = json.dumps({"Version": "1.0.0", "CertDomains": ["hub.example-tailnet.ts.net"],
                        "CurrentTailnet": {"Name": "example-tailnet", "MagicDNSEnabled": True}}, indent=2)

    def asks(self, out: str) -> list[str]:
        return [l for l in out.splitlines() if l.startswith("ASK UP FRONT")]

    def test_step0_asks_up_front_for_a_signed_out_tailscale(self) -> None:
        out = self.step0(FAKE_TS_LOGGED_OUT="1", FAKE_TS_JSON=self.TS_ON)
        self.assertTrue([a for a in self.asks(out) if "sudo to run `tailscale up`, which prints a sign-in link the person must open" in a],
                        out)
        signed_in = self.step0(FAKE_TS_JSON=self.TS_ON)
        self.assertFalse([a for a in self.asks(signed_in) if "tailscale up" in a],
                         f"a signed-in Tailscale was asked about:\n{signed_in}")

    def test_step0_asks_up_front_to_install_a_missing_tailscale(self) -> None:
        out = self.step0(PATH=self.without("tailscale"))
        self.assertTrue([a for a in self.asks(out) if "sudo to install Tailscale (https://tailscale.com/download)" in a], out)

    def test_step0_asks_for_magicdns_and_https_until_both_are_on(self) -> None:
        def wanted(out: str) -> bool:
            return bool([a for a in self.asks(out) if "turns on MagicDNS and HTTPS Certificates" in a
                         and "https://login.tailscale.com/admin/dns" in a])
        self.assertTrue(wanted(self.step0()))
        only_dns = '{"CertDomains": null, "CurrentTailnet": {"MagicDNSEnabled": true}}'
        self.assertTrue(wanted(self.step0(FAKE_TS_JSON=only_dns)), "HTTPS Certificates off was not asked for")
        only_certs = '{"CertDomains": ["hub.example-tailnet.ts.net"], "CurrentTailnet": {"MagicDNSEnabled": false}}'
        self.assertTrue(wanted(self.step0(FAKE_TS_JSON=only_certs)), "MagicDNS off was not asked for")
        self.assertFalse(wanted(self.step0(FAKE_TS_JSON=self.TS_ON)), "asked although both are on")

    def test_step0_asks_for_qrencode_and_linger_in_the_same_batch(self) -> None:
        def has(out: str, s: str) -> bool:
            return bool([a for a in self.asks(out) if s in a])
        out = self.step0(PATH=self.without("qrencode"), FAKE_LINGER="no", FAKE_TS_JSON=self.TS_ON)
        self.assertTrue(has(out, "sudo to install qrencode"), out)
        if os.uname().sysname == "Linux":
            self.assertTrue(has(out, 'loginctl enable-linger "$USER"'), out)
        done = self.step0(FAKE_LINGER="yes", FAKE_TS_JSON=self.TS_ON)
        self.assertFalse(has(done, "qrencode"))
        self.assertFalse(has(done, "enable-linger"))

    @unittest.skipUnless(os.uname().sysname == "Linux", "the docker group is Linux")
    def test_step0_asks_for_the_docker_group_and_says_a_new_login_is_needed(self) -> None:
        # The finding: no Linux install lines for Docker and Compose v2, and no
        # docker-group step; a blind run added the group and then could not
        # use Docker in the same shell.
        out = self.step0(FAKE_DOCKER_INFO_FAIL="1", FAKE_TS_JSON=self.TS_ON)
        group = [a for a in self.asks(out) if "usermod -aG docker" in a]
        self.assertTrue(group, out)
        self.assertIn("NEW login", group[0])
        self.assertFalse([a for a in self.asks(self.step0(FAKE_TS_JSON=self.TS_ON)) if "docker group" in a],
                         "control: a usable Docker was asked about")
        compose = self.step0(FAKE_NO_COMPOSE="1", FAKE_TS_JSON=self.TS_ON)
        self.assertTrue([a for a in self.asks(compose) if "Compose v2" in a], compose)
        text = self.section("## Step 0:")
        for want in ("get.docker.com", "docker-compose-v2", "docker-compose-plugin", 'usermod -aG docker "$USER"',
                     "applies only to a new login"):
            self.assertIn(want, text, f"Step 0 has no {want!r}")

    def section(self, heading: str) -> str:
        text = GUIDE.read_text()
        start = text.index(heading)
        end = text.find("\n## ", start + len(heading))
        return text[start:end if end > 0 else len(text)]

    def test_step1_asks_for_everything_step0_flagged(self) -> None:
        # Prose, so a weaker guard: Step 1's one batch of questions names the
        # ASK UP FRONT lines and each thing a fresh run stopped half way for.
        s1 = " ".join(self.section("## Step 1:").split())
        for want in ("starting `ASK UP FRONT`", "sign-in link the person must open", "MagicDNS and HTTPS Certificates",
                     "https://login.tailscale.com/admin/dns", "`qrencode`", "`loginctl enable-linger`",
                     "ask for `sudo` for all of them at once"):
            self.assertIn(want, s1, f"Step 1 does not ask about {want!r}")

    def operator(self, prefs: str = "") -> list[str]:
        out = self.step0(USER="tester", FAKE_TS_JSON=self.TS_ON, **({"FAKE_TS_PREFS": prefs} if prefs else {}))
        return [a for a in self.asks(out) if "--operator" in a]

    @unittest.skipUnless(os.uname().sysname == "Linux" and os.getuid() != 0, "the operator ask is Linux, non-root")
    def test_step0_asks_for_the_operator_only_when_unset(self) -> None:
        unset = json.dumps({"ControlURL": "https://controlplane.example", "OperatorUser": ""}, indent=2)
        other = json.dumps({"OperatorUser": "someone-else"}, indent=2)
        mine = json.dumps({"ControlURL": "https://controlplane.example", "OperatorUser": "tester"}, indent=2)
        self.assertEqual(len(self.operator(unset)), 1, "an unset operator was not asked for")
        self.assertEqual(len(self.operator(other)), 1, "another user's operator was not asked for")
        self.assertEqual(self.operator(mine), [], "asked although the operator is this user")
        self.assertEqual(self.operator(), [], "asked although the prefs could not be read")

    def test_step0_without_loginctl_has_no_linger_ask(self) -> None:
        if os.uname().sysname != "Linux":
            self.skipTest("Linux only")
        out = self.step0(PATH=self.without("loginctl"), FAKE_LINGER="no", FAKE_TS_JSON=self.TS_ON)
        self.assertFalse([a for a in self.asks(out) if "linger" in a], out)
        self.assertIn("NOTE: no loginctl (WSL, or a container without systemd)", out)

    def test_step0_on_macos_asks_for_no_sudo(self) -> None:
        out = self.step0(PATH=self.without("tmux", "docker", "qrencode", "tailscale"), FAKE_UNAME_S="Darwin",
                         MI_TS_APP=str(self.tmp / "no-such-app"))
        asks = self.asks(out)
        self.assertIn("os=darwin", out)
        for want in ("brew install tmux", "Docker Desktop", "the Tailscale app", "brew install qrencode"):
            self.assertTrue([a for a in asks if want in a], f"no ask about {want!r}:\n{out}")
        self.assertFalse([a for a in asks if "sudo to" in a], f"sudo asked on macOS:\n{out}")

    def test_macos_app_store_tailscale_is_found(self) -> None:
        path = self.without("tailscale")
        app = str(self.bin / "tailscale")  # the fake, at the app's CLI path
        out = self.step0(PATH=path, FAKE_UNAME_S="Darwin", MI_TS_APP=app, FAKE_TS_JSON=self.TS_ON)
        self.assertFalse([a for a in self.asks(out) if "Tailscale app" in a], f"declared missing:\n{out}")
        self.assertNotRegex(out, r"(?m)^tailscale +MISSING$")
        self.assertIn("tailscale: logged in", out)
        # and every later block reaches it through lib.sh
        self.hub()
        self.run_block(self.lib + block("tailscale serve --bg"), PATH=f"{path}", MI_TS_APP=app)
        self.assertIn("tailscale serve --bg --https=443 8105", self.calls())
        self.assertIn(f"MUSTER_URL='{URL}'", self.state())

    def test_mi_save_writes_the_state_file_mode_600(self) -> None:
        state = self.homedir / ".config/muster-install/state"
        state.unlink()
        self.sh("umask 022\nmi_save SOME_KEY value")
        self.assertEqual(stat.S_IMODE(state.stat().st_mode), 0o600)
        state.chmod(0o644)  # one written before this fix
        self.sh("umask 022\nmi_save SOME_KEY other")
        self.assertEqual(stat.S_IMODE(state.stat().st_mode), 0o600)
        self.assertIn("SOME_KEY='other'", self.state())

    # --- Step 5: the doctor's report, once ------------------------------------

    def test_step5_shows_only_doctors_report_and_never_puts_a_secret_on_argv(self) -> None:
        self.hub(MUSTER_URL=URL)
        bindir = self.homedir / ".local/bin"
        bindir.mkdir(parents=True)
        fake = bindir / "muster"
        fake.write_text('#!/bin/sh\necho "muster $*" >> "$FAKE_LOG"\n'
                        'echo "PASS  server" >&2\necho "1 passed, 0 failed" >&2\n'
                        'echo \'{"ok": true, "checks": []}\'\n')
        fake.chmod(0o755)
        hub_block = block("show-secret hook-token", "doctor --deploy-env")
        other = [b for b in blocks() if 'doctor --api-url "$MUSTER_URL"' in b and "show-secret" not in b]
        self.assertTrue(other, "Step 5 has no block for the other machines")
        for b in (hub_block, other[0]):
            out = self.run_block(self.lib + b)
            self.assertIn("PASS  server", out, "positive control: the doctor ran")
            self.assertNotIn('"checks"', out, f"the JSON copy reached the agent:\n{out}")
        muster_calls = [l for l in self.calls().splitlines() if l.startswith("muster ")]
        self.assertTrue(any("--deploy-env" in l for l in muster_calls), "positive control: the hub's doctor ran")
        self.assertTrue(any(f"--api-url {URL}" in l for l in muster_calls), "the HTTPS address was not checked")
        for l in self.calls().splitlines():
            self.assertNotIn("pw-fixture-0123456789", l, "the password reached a command line")
        self.assertFalse((self.homedir / ".config/muster-install/doctor.env").exists(), "the deploy file was left behind")


class Prose(unittest.TestCase):
    """What the guide must NOT hold, and where its references must land."""

    def test_no_fact_tables_the_code_owns(self) -> None:
        # The doctor's check list is docs/cli.md's (pinned to doctor.go); the
        # guide had its own copy, 10 checks against the code's 14.
        text = GUIDE.read_text()
        self.assertNotRegex(text, r"(?m)^\| check \| what it proves \|", "INSTALL.md has its own doctor table again")
        self.assertIn("docs/cli.md#muster-doctor", text, "INSTALL.md does not point at the doctor's table")
        # Model ids and image versions belong to docs/features.md and the code.
        for fact in ("cohere-transcribe-03-2026", "faster-whisper-small", "whisper-1", "47823", "127.0.0.1:8105"):
            self.assertNotIn(fact, text, f"INSTALL.md spells {fact!r}, a fact the code owns")

    def test_cli_doc_doctor_table_is_the_codes_check_list(self) -> None:
        # docs/cli.md's doctor table lists exactly the checks doctor.go runs,
        # in order, read from its doctorChecks list (never a count written
        # here), so a check added to the code fails this until the table has
        # its row. INVARIANT GUARD: it holds at origin/main too; the drift it
        # closes was INSTALL.md's own copy of the table, which is gone. (A
        # static check here, not a Go test: the nix build of cmd/muster
        # carries no docs/.)
        def code(src: str) -> list[str]:
            m = re.search(r"(?s)var doctorChecks = \[\]string\{(.*?)\}", src)
            return re.findall(r'"([a-z-]+)"', m.group(1)) if m else []

        def table(md: str) -> list[str]:
            i = md.find("## `muster doctor`")
            if i < 0:
                return []
            sec = md[i + 1:]
            j = sec.find("\n## ")
            sec = sec if j < 0 else sec[:j]
            return re.findall(r"(?m)^\| `([a-z-]+)` \|", sec)

        src = (ROOT / "cmd/muster/doctor.go").read_text()
        md = (ROOT / "docs/cli.md").read_text()
        checks = code(src)
        self.assertGreater(len(checks), 5, "positive control: doctorChecks was parsed")
        self.assertEqual(table(md), checks, "docs/cli.md's doctor table is not doctor.go's check list")
        self.assertNotEqual(table(md.replace("| `push` |", "| push-renamed |", 1)), checks, "control: a dropped row")
        self.assertNotEqual(code(src.replace('"ask-hook",', '"ask-hook", "officers",', 1)), table(md),
                            "control: a check added to the code")

    def test_the_release_publishes_the_guide(self) -> None:
        # The prompt downloads .../releases/latest/download/INSTALL.md, so a
        # release must attach it. scripts/release-build.sh's own self-check
        # makes dist/ EXACTLY its literal `expected` list (CI's
        # release-dry-run runs it on every pull request); this pins that the
        # list holds INSTALL.md, and that the script stamps it.
        src = (ROOT / "scripts/release-build.sh").read_text()
        m = re.search(r"(?ms)^expected=\((.*?)^\)", src)
        self.assertIsNotNone(m, "scripts/release-build.sh has no expected=( … ) asset list")
        assets = m.group(1).split()
        self.assertIn("install.sh", assets, "positive control: the list was parsed")
        self.assertIn("INSTALL.md", assets, "the release does not publish INSTALL.md")
        self.assertRegex(src, r'MI_GUIDE_RELEASE=.*"\$ROOT/INSTALL\.md" > "\$DIST/INSTALL\.md"',
                         "release-build.sh does not stamp the guide it publishes")
        self.assertRegex(src, r'grep -qx "MI_GUIDE_RELEASE=\\"\$VERSION\\"" "\$DIST/INSTALL\.md"',
                         "release-build.sh does not check the published guide pins its release")

    def test_the_second_server_section_moved_out(self) -> None:
        self.assertNotIn("## A second muster server", GUIDE.read_text().replace("### A second", "## A second"))
        self.assertIn("docs/multi-tenant-hosts.md", GUIDE.read_text())

    def test_every_install_section_the_code_names_exists(self) -> None:
        # Fix lines and comments name a guide section by its TITLE
        # (INSTALL.md, "Another machine"), never a step number that a
        # restructure silently re-points.
        titles = set(headings(GUIDE.read_text()))
        named, numbered = [], []
        for p in [*ROOT.glob("cmd/**/*.go"), *ROOT.glob("internal/**/*.go"), ROOT / "deploy/install.sh",
                  *ROOT.glob("docs/*.md"), *ROOT.glob("docs/guides/*.md")]:
            src = p.read_text()
            named += [(p.name, " ".join(t.split())) for t in re.findall(r'INSTALL\.md(?:\'s)?,?\s\\?"([^"\\]+)\\?"', src)]
            numbered += [(p.name, m) for m in re.findall(r"INSTALL\.md(?:'s)?,? [Ss]tep \d+", src)]
        self.assertGreater(len(named), 5, "positive control: the pattern found almost no section names")
        self.assertEqual(numbered, [], "code or docs name an INSTALL.md step by number")
        for f, t in named:
            self.assertIn(t, titles, f"{f} names INSTALL.md section {t!r}, which is not a heading")
        self.assertNotIn("No such section", titles, "control")

    # Every file that may point into the guide (INSTALL.md itself excepted).
    def guide_referrers(self) -> list[Path]:
        return [*ROOT.glob("cmd/**/*.go"), *ROOT.glob("internal/**/*.go"), ROOT / "deploy/install.sh",
                *ROOT.glob("docs/*.md"), *ROOT.glob("docs/guides/*.md"), ROOT / "README.md", ROOT / "AGENTS.md", ROOT / "CONTRIBUTING.md",
                *ROOT.glob("plugins/**/*.md")]

    @staticmethod
    def stale_guide_refs(name: str, src: str, helpers: set[str], keys: set[str]) -> list[str]:
        """A paragraph that mentions INSTALL.md and also a step NUMBER (anywhere
        in it, not only next to the name: "(INSTALL.md: …, then re-run Step
        7's last block)"), a guide helper the guide does not define, or an
        `mi_save KEY` the guide never reads."""
        out = []
        for para in re.split(r"\n[ \t]*(?://|#)?[ \t]*\n", src):
            if "INSTALL.md" not in para:
                continue
            flat = " ".join(para.split())
            out += [f"{name}: {m.group(0)!r} in {flat[:160]!r}" for m in re.finditer(r"\b[Ss]tep \d+", flat)]
        for m in re.finditer(r"\bmi_[a-z_]+", src):
            if m.group(0) not in helpers:
                out.append(f"{name}: {m.group(0)} is not a helper INSTALL.md defines")
        for m in re.finditer(r"\bmi_save ([A-Z_]+)", src):
            if m.group(1) not in keys:
                out.append(f"{name}: mi_save {m.group(1)}, a key the guide never reads")
        return out

    def test_no_stale_reference_into_the_guide(self) -> None:
        # #728 round 0 (D3): docs/features.md told the reader "INSTALL.md:
        # `mi_save ASK_TOOLS yes`, then re-run Step 7's last block" — neither
        # exists. Step numbers move and helpers go; references outside the
        # guide name sections by title (the test above) and helpers it has.
        guide = GUIDE.read_text()
        helpers = set(re.findall(r"(?m)^\s*(mi_[a-z_]+)\(\)", guide)) | set(re.findall(r"\b(MI_[A-Z_]+)=", guide))
        keys = set(re.findall(r"\$\{?([A-Z_]+)", guide))
        self.assertIn("mi_save", helpers, "positive control: the guide's helpers were parsed")
        self.assertIn("ROLE", keys, "positive control: the guide's keys were parsed")
        mt = MT_DOC.read_text()
        bad = []
        for p in self.guide_referrers():
            src = p.read_text()
            # docs/multi-tenant-hosts.md runs the guide's helpers with keys of its own.
            k = keys | set(re.findall(r"\$\{?([A-Z_]+)", mt)) if p == MT_DOC else keys
            bad += self.stale_guide_refs(p.name, src, helpers, k)
        self.assertEqual(bad, [], "a reference into INSTALL.md that the guide no longer has")
        # Controls, each kind planted into the real docs/features.md: seen.
        feat = (ROOT / "docs/features.md").read_text()
        for planted in ("Add the line (INSTALL.md says so;\nthen re-run Step 7's last block).",
                        "(INSTALL.md: `mi_save ASK_TOOLS yes`)",
                        "(INSTALL.md: `mi_no_such_helper`)"):
            self.assertTrue(self.stale_guide_refs("features.md", feat + "\n\n" + planted + "\n", helpers, keys),
                            f"control: {planted!r} went unseen")
        self.assertEqual(self.stale_guide_refs("x", "See INSTALL.md, \"Check it\".\n\nStep 7 of the jobs list.\n", helpers, keys), [],
                         "control: a step number in a paragraph that does not mention INSTALL.md is not a guide reference")


class MultiTenantPage(unittest.TestCase):
    """docs/multi-tenant-hosts.md: every block loads INSTALL.md's helpers.

    The finding: in INSTALL.md those blocks did not, so `$OS` was empty and a
    Linux machine took the `else` branch, `launchctl`. Each block must load
    the helpers, and the restart block, run on a Linux machine whose helpers
    say so, must call systemctl and never launchctl."""

    def setUp(self) -> None:
        self.g = Guide("test_mi_save_writes_the_state_file_mode_600")
        self.g.setUp()

    def tearDown(self) -> None:
        self.g.tearDown()

    def doc_blocks(self) -> list[str]:
        self.assertTrue(MT_DOC.exists(), f"{MT_DOC} is missing")
        return sh_blocks(MT_DOC)

    def test_every_block_loads_the_helpers(self) -> None:
        bs = self.doc_blocks()
        self.assertGreaterEqual(len(bs), 4, "positive control: the page's blocks were found")
        for b in bs:
            first = next(l for l in b.splitlines() if l.strip() and not l.lstrip().startswith("#"))
            self.assertTrue(first.startswith('. "$HOME/.config/muster-install/lib.sh"'),
                            f"a block does not start by loading the helpers:\n{b[:200]}")

    def test_the_restart_block_takes_the_linux_branch_on_linux(self) -> None:
        restart = [b for b in self.doc_blocks() if "restart muster-mirror.service" in b]
        self.assertTrue(restart, "the page has no restart block")
        self.g.sh("mi_save OS linux")
        self.g.run_block(restart[0])
        calls = self.g.calls()
        self.assertIn("systemctl --user restart muster-mirror.service", calls, calls)
        self.assertNotIn("launchctl", calls, "a Linux machine took the macOS branch")

    def test_the_tenant_name_reaches_the_later_blocks(self) -> None:
        bs = self.doc_blocks()
        name = [b for b in bs if "mi_save TENANT" in b]
        self.assertTrue(name, "the naming block does not save the tenant for the later blocks")
        self.g.run_block(name[0])
        later = [b for b in bs if "MUSTER_MIRROR_TMUX_SOCKET=$TENANT" in b][0]
        out = self.g.run_block(later.replace("CHOOSE-A-FREE-PORT", "47999"))
        env = (self.g.homedir / ".config/muster-mirror/work/env").read_text()
        self.assertIn("MUSTER_MIRROR_TMUX_SOCKET=work", env, out)


if __name__ == "__main__":
    unittest.main(verbosity=2)

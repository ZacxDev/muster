#!/usr/bin/env python3
"""INSTALL.md's shell blocks, run for real against faked machines.

    python3 tests/install_guide_test.py            # the guide in this checkout
    MI_GUIDE=path/to/INSTALL.md python3 tests/install_guide_test.py

Each case finds a block of INSTALL.md by what it contains, runs it in a fresh
`sh` with HOME in a temp directory and fake `docker`, `curl`, `tailscale` and
`ssh` first on PATH (every call logged), and asserts what the block WROTE
(.env, the state file, the token file) or CALLED. Nothing touches this
machine's Docker, Tailscale or network.

What it pins (the round-1 audit on #322): push keys are all three or none;
Step 5 brings .env's MUSTER_VERSION to the chosen release; re-running or
switching the dictation option never loses a key and never leaves a stale
profile or model; a failed version lookup is never saved; an existing muster
or Tailscale config is never taken over; "start over" deletes only this
guide's own fresh install; a secret never reaches argv; the ssh mint keeps
only the token.
"""

from __future__ import annotations

import json
import os
import re
import stat
import subprocess
import tempfile
import textwrap
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
GUIDE = Path(os.environ.get("MI_GUIDE", ROOT / "INSTALL.md"))
URL = "https://hub.example-tailnet.ts.net"


def blocks() -> list[str]:
    text = GUIDE.read_text()
    out = []
    for m in re.finditer(r"^([ \t]*)```sh\n(.*?)^\1```", text, re.S | re.M):
        body = m.group(2)
        lines = body.split("\n")
        ind = min((len(l) - len(l.lstrip(" ")) for l in lines if l.strip()), default=0)
        out.append("\n".join(l[ind:] for l in lines))
    return out


def block(*needles: str) -> str:
    for b in blocks():
        if all(n in b for n in needles):
            return b
    raise AssertionError(f"INSTALL.md has no sh block containing {needles!r}")


FAKE_DOCKER = r"""#!/bin/sh
echo "docker $*" >> "$FAKE_LOG"
case "$*" in *"mirror token create"*)
  eval "h=\${$#}"; echo "minted mirror token 9 for host $h" >&2; echo "mmt_token-for-$h"; exit 0 ;;
esac
case "$1 $2" in
  "run --rm")
    [ "${FAKE_VAPID:-ok}" = fail ] && { echo "pull denied" >&2; exit 1; }
    echo "MUSTER_VAPID_PUBLIC_KEY=BPUBFAKE"; echo "MUSTER_VAPID_PRIVATE_KEY=privfake"; exit 0 ;;
  "ps -a")
    case "$*" in *--format*) for i in ${FAKE_PS_IDS:-}; do echo "  container name-$i"; done ;;
                 *) for i in ${FAKE_PS_IDS:-}; do echo "$i"; done ;; esac; exit 0 ;;
  "volume ls")
    case "$*" in *--format*) for v in ${FAKE_VOLUMES:-}; do echo "  volume $v"; done ;;
                 *) for v in ${FAKE_VOLUMES:-}; do echo "$v"; done ;; esac; exit 0 ;;
  "inspect -f")
    id=$4; case "$3" in *working_dir*) eval "echo \"\${FAKE_WD_$id:-}\"" ;; *) echo "/name-$id" ;; esac; exit 0 ;;
  "compose config") echo "name: muster"; echo "services:"; exit 0 ;;
esac
exit 0
"""

FAKE_CURL = r"""#!/bin/sh
echo "curl $*" >> "$FAKE_LOG"
case "$*" in
  *127.0.0.1:8105/health*) echo '{"status":"ok"}'; exit 0 ;;
  *127.0.0.1:8105*) [ "${FAKE_PORT_8105:-0}" = 1 ] && exit 0; exit 7 ;;
  *-fsSLI*releases/latest*) printf '%s' "${FAKE_LATEST_URL:-}"; [ -n "${FAKE_LATEST_URL:-}" ] || exit 22; exit 0 ;;
  *api.github.com*) printf '%s\n' "${FAKE_API_BODY:-}"; exit 0 ;;
  */health*) printf '%s\n' "${FAKE_HEALTH:-}"; exit 0 ;;
esac
out=""; prev=""
for a in "$@"; do [ "$prev" = "-o" ] && out=$a; prev=$a; url=$a; done
[ -n "$out" ] && { printf 'downloaded %s\n' "$url" > "$out"; exit 0; }
exit 0
"""

FAKE_TAILSCALE = r"""#!/bin/sh
echo "tailscale $*" >> "$FAKE_LOG"
case "$*" in
  "serve status"*)
    [ "${FAKE_SERVE_FAIL:-0}" = 1 ] && { echo "failed to connect to local tailscaled; it doesn't appear to be running" >&2; exit 1; }
    cat "$FAKE_SERVE"; exit 0 ;;
  "serve --bg --https=443 8105")
    printf 'https://hub.example-tailnet.ts.net (tailnet only)\n|-- / proxy http://127.0.0.1:8105\n' > "$FAKE_SERVE"; exit 0 ;;
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
# A real hub's login shell would set its own PATH; this one keeps the fakes.
[ "$1" = "-l" ] && shift
sh "$@"; rc=$?
echo "Connection to hub closed. (logout noise on stdout)"
exit $rc
"""

# loginctl and sudo: linger is FAKE_LINGER ("yes" by default); sudo runs its
# command (the fakes first on PATH) unless FAKE_SUDO=fail. systemctl and
# journalctl only log: no block may reach this machine's user services.
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

FAKE_LOGGER = r"""#!/bin/sh
echo "$(basename "$0") $*" >> "$FAKE_LOG"
case "$*" in *is-active*) echo active ;; esac
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
                           ("systemctl", FAKE_LOGGER), ("journalctl", FAKE_LOGGER), ("qrencode", FAKE_LOGGER),
                           ("tmux", FAKE_LOGGER), ("login-shell", FAKE_LOGIN_SHELL), ("uname", FAKE_UNAME)]:
            p = self.bin / name
            p.write_text(body)
            p.chmod(0o755)
        self.env = {
            "HOME": str(self.homedir), "PATH": f"{self.bin}:{os.environ['PATH']}", "FAKE_LOG": str(self.log),
            "FAKE_SERVE": str(self.serve_file), "FAKE_AWK_ARGV": str(self.awk_argv), "REAL_AWK": real_awk,
            "REAL_UNAME": real_uname,
        }
        self.run_block(block('cat > "$HOME/.config/muster-install/lib.sh"'))
        self.lib = f'. "{self.homedir}/.config/muster-install/lib.sh"\n'
        self.muster = self.homedir / "muster"

    def tearDown(self) -> None:
        subprocess.run(["rm", "-rf", str(self.tmp)])

    def run_block(self, script: str, **env: str) -> str:
        p = subprocess.run(["sh", "-c", script], env={**self.env, **env}, capture_output=True, text=True,
                           cwd=self.tmp, timeout=60)
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

    def hub(self, env_example: str = "", **saved: str) -> None:
        """A hub at Step 4: .env.example and .env in place, values saved."""
        self.muster.mkdir(exist_ok=True)
        ex = "MUSTER_VERSION=1.0.0\nPOSTGRES_PASSWORD=CHANGE-ME\nMUSTER_UI_PASSWORD=CHANGE-ME\nMUSTER_HOOK_TOKEN=CHANGE-ME\n" \
             "#MUSTER_STT_TOKEN=CHANGE-ME\n" + env_example
        (self.muster / ".env.example").write_text(ex)
        (self.muster / ".env").write_text(ex)
        (self.muster / ".env").chmod(0o600)
        saved = {"MUSTER_URL": URL, "MUSTER_VERSION": "1.0.0", **saved}
        self.sh("".join(f"mi_save {k} '{v}'\n" for k, v in saved.items()))

    # --- Y1: push keys, all three or none ------------------------------------

    def test_failed_key_generation_writes_no_push_key(self) -> None:
        self.hub()
        out = self.run_block(self.lib + block("env_secret POSTGRES_PASSWORD"), FAKE_VAPID="fail")
        env = self.dotenv()
        self.assertIn("vapid generate", self.calls(), "positive control: the block tried to generate keys")
        for k in ("MUSTER_VAPID_PUBLIC_KEY", "MUSTER_VAPID_PRIVATE_KEY", "MUSTER_VAPID_SUBJECT"):
            self.assertEqual(env.get(k, ""), "", f"{k} written although generation failed\n{out}")

    def test_generated_keys_are_all_three(self) -> None:
        self.hub()
        self.run_block(self.lib + block("env_secret POSTGRES_PASSWORD"))
        env = self.dotenv()
        self.assertEqual((env.get("MUSTER_VAPID_PUBLIC_KEY"), env.get("MUSTER_VAPID_PRIVATE_KEY"), env.get("MUSTER_VAPID_SUBJECT")),
                         ("BPUBFAKE", "privfake", URL))

    # --- Y2: Step 5 brings .env to the chosen version -------------------------

    def test_step5_writes_the_chosen_version_before_it_pulls(self) -> None:
        self.hub()
        self.sh("mi_set_version 1.1.0")
        self.run_block(self.lib + block("docker compose up -d --wait"))
        self.assertEqual(self.dotenv()["MUSTER_VERSION"], "1.1.0")
        self.assertIn("compose pull", self.calls())

    # --- Y3 and Y4: dictation re-runs and switches ----------------------------

    def dictation(self, **saved: str) -> str:
        self.sh("".join(f"mi_save {k} '{v}'\n" for k, v in saved.items()))
        return self.run_block(self.lib + block('case "$STT" in'))

    def write_secret(self, name: str, value: str) -> None:
        p = self.homedir / ".config/muster-install" / name
        p.write_text(value)
        p.chmod(0o600)

    def test_rerunning_api_keeps_the_key(self) -> None:
        self.hub()
        self.write_secret("stt-api-key", "sk-the-real-key")
        self.dictation(STT="api", STT_API_URL="https://api.openai.com", STT_API_MODEL="whisper-1")
        self.assertEqual(self.dotenv()["MUSTER_STT_TOKEN"], "sk-the-real-key")
        self.dictation(STT="api")
        self.assertEqual(self.dotenv()["MUSTER_STT_TOKEN"], "sk-the-real-key", "a re-run replaced the key")

    def test_rerunning_gpu_keeps_the_hf_token(self) -> None:
        self.hub()
        self.write_secret("hf-token", "hf_real")
        self.dictation(STT="local-gpu")
        self.dictation(STT="local-gpu")
        env = self.dotenv()
        self.assertEqual(env["HF_TOKEN"], "hf_real", "a re-run replaced the Hugging Face token")
        first = env["MUSTER_STT_TOKEN"]
        self.dictation(STT="local-gpu")
        self.assertEqual(self.dotenv()["MUSTER_STT_TOKEN"], first, "a re-run rotated the local service's token")

    def test_gpu_to_api_is_a_clean_swap(self) -> None:
        self.hub()
        self.write_secret("hf-token", "hf_real")
        self.dictation(STT="local-gpu")
        self.write_secret("stt-api-key", "sk-the-real-key")
        self.dictation(STT="api", STT_API_URL="https://api.openai.com", STT_API_MODEL="whisper-1")
        env = self.dotenv()
        self.assertEqual((env["COMPOSE_PROFILES"], env["MUSTER_STT_URL"], env["MUSTER_STT_MODEL"], env["MUSTER_STT_TOKEN"]),
                         ("", "https://api.openai.com", "whisper-1", "sk-the-real-key"))

    def test_api_to_cpu_is_a_clean_swap(self) -> None:
        self.hub()
        self.write_secret("stt-api-key", "sk-the-real-key")
        self.dictation(STT="api", STT_API_URL="https://api.openai.com", STT_API_MODEL="whisper-1")
        self.dictation(STT="local-cpu")
        env = self.dotenv()
        self.assertEqual((env["COMPOSE_PROFILES"], env["MUSTER_STT_URL"], env["MUSTER_STT_MODEL"]),
                         ("stt-whisper", "http://stt-whisper:8000", "Systran/faster-whisper-small"))
        self.assertNotEqual(env["MUSTER_STT_TOKEN"], "sk-the-real-key", "the API key was handed to the local service")
        self.assertRegex(env["MUSTER_STT_TOKEN"], r"^[0-9a-f]{64}$")

    def test_cpu_to_api_without_a_key_asks_and_changes_nothing(self) -> None:
        self.hub()
        self.dictation(STT="local-cpu")
        before = self.dotenv()
        out = self.dictation(STT="api", STT_API_URL="https://api.openai.com", STT_API_MODEL="whisper-1")
        self.assertIn("MISSING: the API key", out)
        self.assertEqual(self.dotenv(), before, "it switched to the API with the local service's token as the key")

    def test_step5_marks_the_install_in_use_once_healthy(self) -> None:
        self.hub()
        self.run_block(self.lib + block("docker compose up -d --wait"))
        self.assertIn("GUIDE_IN_USE='1'", self.state())
        own = dict(FAKE_PS_IDS="abc", FAKE_WD_abc=str(self.muster), FAKE_VOLUMES="muster_pgdata")
        self.assertIn("REFUSED", self.start_over(MI_CONFIRM_DELETE="muster", **own))
        self.assertNotIn("compose down", self.calls())

    def test_none_keeps_the_api_key_for_later(self) -> None:
        self.hub()
        self.write_secret("stt-api-key", "sk-the-real-key")
        self.dictation(STT="api", STT_API_URL="https://api.openai.com", STT_API_MODEL="whisper-1")
        self.dictation(STT="none")
        self.assertEqual(self.dotenv()["MUSTER_STT_TOKEN"], "", "dictation is still on")
        out = self.dictation(STT="api")
        self.assertNotIn("MISSING", out)
        self.assertEqual(self.dotenv()["MUSTER_STT_TOKEN"], "sk-the-real-key", "the key was lost on the way through none")

    def test_cpu_resets_a_non_whisper_model(self) -> None:
        self.hub()
        self.write_secret("stt-api-key", "sk-the-real-key")
        self.dictation(STT="api", STT_API_URL="https://api.example-provider.test", STT_API_MODEL="openai/whisper-large-v3")
        self.dictation(STT="local-cpu")
        self.assertEqual(self.dotenv()["MUSTER_STT_MODEL"], "Systran/faster-whisper-small")
        self.sh("env_set MUSTER_STT_MODEL Systran/faster-whisper-large-v3")
        self.dictation(STT="local-cpu")
        self.assertEqual(self.dotenv()["MUSTER_STT_MODEL"], "Systran/faster-whisper-large-v3", "a chosen whisper model was reset")

    def test_api_without_a_url_asks_and_changes_nothing(self) -> None:
        self.hub()
        self.dictation(STT="local-cpu")
        before = self.dotenv()
        self.write_secret("stt-api-key", "sk-the-real-key")
        out = self.dictation(STT="api")
        self.assertIn("MISSING: the API's URL", out)
        self.assertEqual(self.dotenv(), before, "a half-switched dictation config was written")

    # --- Y5: a failed version lookup is never saved ---------------------------

    def test_failed_lookup_saves_nothing_and_recovers(self) -> None:
        self.muster.mkdir()
        out = self.run_block(self.lib + block("curl -fsSL -o docker-compose.yml"),
                             FAKE_LATEST_URL="", FAKE_API_BODY='{"message":"API rate limit exceeded"}')
        self.assertNotIn("MUSTER_VERSION=", self.state(), f"a failed lookup was saved\n{out}")
        self.assertNotIn("MUSTER_RELEASE_BASE=", self.state())
        self.assertNotIn("docker-compose.yml", self.calls(), "it downloaded with no version")
        self.sh("mi_set_version 1.2.3")
        self.run_block(self.lib + block("curl -fsSL -o docker-compose.yml"))
        self.assertIn("/releases/download/v1.2.3/docker-compose.yml", self.calls())
        self.assertNotEqual(self.sh("mi_set_version v1.2.3 && echo SAVED"), "SAVED", "a v-prefixed version was accepted")
        self.assertIn("MUSTER_VERSION='1.2.3'", self.state())

    def test_lookup_takes_the_latest_redirect(self) -> None:
        self.muster.mkdir()
        self.run_block(self.lib + block("curl -fsSL -o docker-compose.yml"),
                       FAKE_LATEST_URL="https://github.com/ZacxDev/muster/releases/tag/v2.3.4")
        self.assertIn("MUSTER_VERSION='2.3.4'", self.state())

    def test_joining_machine_saves_no_unreadable_version(self) -> None:
        self.sh(f"mi_save MUSTER_URL '{URL}'")
        self.run_block(self.lib + block('$MUSTER_URL/health" | sed'), FAKE_HEALTH="<html>tailnet login</html>")
        self.assertNotIn("MUSTER_VERSION=", self.state())
        self.run_block(self.lib + block('$MUSTER_URL/health" | sed'), FAKE_HEALTH='{"status":"ok","version":"1.4.0"}')
        self.assertIn("MUSTER_VERSION='1.4.0'", self.state())
        self.assertIn("MUSTER_RELEASE_BASE='https://github.com/ZacxDev/muster/releases/download/v1.4.0'", self.state())

    # --- Y6: an existing muster is never taken over ---------------------------

    def test_step5_stops_on_another_installs_container(self) -> None:
        self.hub()
        out = self.run_block(self.lib + block("docker compose up -d --wait"), FAKE_PS_IDS="abc", FAKE_WD_abc="/srv/other-muster")
        self.assertNotIn("compose up", self.calls(), f"it started over another install\n{out}")
        self.assertIn("STOP", out)

    def test_step5_stops_on_a_fresh_machine_with_8105_taken(self) -> None:
        self.hub()
        out = self.run_block(self.lib + block("docker compose up -d --wait"), FAKE_PORT_8105="1")
        self.assertNotIn("compose up", self.calls(), out)

    def test_step5_stops_on_leftover_volumes(self) -> None:
        self.hub()
        self.run_block(self.lib + block("docker compose up -d --wait"), FAKE_VOLUMES="muster_pgdata")
        self.assertNotIn("compose up", self.calls())

    def test_step5_rerun_on_its_own_install_proceeds(self) -> None:
        self.hub(GUIDE_STARTED="1")
        self.run_block(self.lib + block("docker compose up -d --wait"), FAKE_PS_IDS="abc",
                       FAKE_WD_abc=str(self.muster), FAKE_PORT_8105="1")
        self.assertIn("compose up", self.calls(), "the guide refused its own install")

    def test_step0_reports_another_install(self) -> None:
        out = self.run_block(block('cat > "$HOME/.config/muster-install/lib.sh"'), FAKE_PS_IDS="abc", FAKE_WD_abc="/srv/x")
        self.assertIn("FOREIGN container", out)

    # --- Y6: starting over deletes only this guide's fresh install ------------

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
        self.assertNotIn("compose down", self.calls())

    def test_start_over_names_what_it_deletes_and_needs_the_confirmation(self) -> None:
        self.hub(GUIDE_STARTED="1")
        own = dict(FAKE_PS_IDS="abc", FAKE_WD_abc=str(self.muster), FAKE_VOLUMES="muster_pgdata")
        out = self.start_over(**own)
        self.assertIn("container name-abc", out)
        self.assertIn("volume muster_pgdata", out)
        self.assertNotIn("compose down", self.calls(), "it deleted without the confirmation")
        self.start_over(MI_CONFIRM_DELETE="muster", **own)
        self.assertIn("compose down -v", self.calls())

    # --- Y7: Tailscale serve is added to, never replaced ----------------------

    def serve(self, status: str) -> str:
        self.serve_file.write_text(status)
        return self.run_block(self.lib + block("mi_save MUSTER_URL"))

    def test_serve_adds_only_muster_when_empty(self) -> None:
        self.serve("No serve config\n")
        self.assertIn("tailscale serve --bg --https=443 8105", self.calls())
        self.assertIn(f"MUSTER_URL='{URL}'", self.state())

    def test_serve_keeps_its_own_mapping(self) -> None:
        self.serve(f"{URL} (tailnet only)\n|-- / proxy http://127.0.0.1:8105\n")
        self.assertNotIn("--bg", self.calls())
        self.assertIn(f"MUSTER_URL='{URL}'", self.state())

    def test_serve_stops_on_someone_elses_site(self) -> None:
        out = self.serve(f"{URL} (tailnet only)\n|-- / proxy http://127.0.0.1:3000\n")
        self.assertNotIn("--bg", self.calls(), out)
        self.assertNotIn("MUSTER_URL", self.state())

    def test_serve_stops_on_funnel(self) -> None:
        # The mapping is muster's own, so ONLY the Funnel check can stop it.
        out = self.serve(f"{URL} (Funnel on)\n|-- / proxy http://127.0.0.1:8105\n")
        self.assertNotIn("--bg", self.calls())
        self.assertIn("STOP", out)
        self.assertNotIn("MUSTER_URL", self.state(), "a Funnel-exposed mapping was adopted as muster's")

    def test_serve_unready_is_not_a_foreign_stop(self) -> None:
        self.serve_file.write_text("No serve config\n")
        out = self.run_block(self.lib + block("mi_save MUSTER_URL"), FAKE_SERVE_FAIL="1")
        self.assertIn("TAILSCALE NOT READY", out)
        self.assertNotIn("STOP", out)
        self.assertNotIn("--bg", self.calls())
        self.assertNotIn("MUSTER_URL", self.state())
        pre = self.run_block(block('cat > "$HOME/.config/muster-install/lib.sh"'), FAKE_SERVE_FAIL="1")
        self.assertIn("tailscale serve: unready", pre)

    def test_removal_turns_off_only_musters_mapping(self) -> None:
        # Prose, so a weaker guard: no block runs `serve reset`, every mention
        # of it is a "Never", and removal names muster's own mapping.
        for b in blocks():
            self.assertNotRegex(b, r"tailscale serve reset", "a block wipes every serve mapping")
        text = GUIDE.read_text()
        for m in re.finditer(r"tailscale serve reset", text):
            self.assertIn("Never", text[max(0, m.start() - 40):m.start()], "the guide tells the agent to reset serve")
        self.assertRegex(text, r"tailscale serve --https=443\s+off", "removal does not name muster's mapping")

    # --- nits: secrets, the ssh mint ------------------------------------------

    def test_env_set_keeps_secrets_out_of_argv_and_the_file_private(self) -> None:
        self.hub()
        self.sh("env_set MUSTER_STT_TOKEN sk-secret-in-argv-check")
        self.assertNotIn("sk-secret-in-argv-check", self.awk_argv.read_text())
        mode = stat.S_IMODE((self.muster / ".env").stat().st_mode)
        self.assertEqual(mode, 0o600)
        self.assertFalse((self.muster / ".env.tmp").exists())
        self.assertEqual(self.dotenv()["MUSTER_STT_TOKEN"], "sk-secret-in-argv-check")

    def test_ssh_mint_keeps_only_the_token(self) -> None:
        self.sh("mi_save MIRROR_HOST laptop")
        out = self.run_block(self.lib + block("ssh ", "mirror token create").replace("HUB", "hub.example"))
        tok = (self.homedir / ".config/muster-mirror/token").read_text().strip()
        self.assertEqual(tok, "mmt_token-for-laptop", out)
        self.assertIn("BatchMode=yes", self.calls())
        again = self.run_block(self.lib + block("ssh ", "mirror token create").replace("HUB", "hub.example"))
        self.assertIn("kept existing token", again)

    # --- fresh-run friction: ask for sudo and the person's hands up front -----

    def step0(self, **env: str) -> str:
        return self.run_block(block('cat > "$HOME/.config/muster-install/lib.sh"'), **env)

    # Indented over many lines, as the real `tailscale status --json` prints it.
    TS_ON = json.dumps({"Version": "1.0.0", "CertDomains": ["hub.example-tailnet.ts.net"],
                        "CurrentTailnet": {"Name": "example-tailnet", "MagicDNSEnabled": True}}, indent=2)

    def asks(self, out: str) -> list[str]:
        return [l for l in out.splitlines() if l.startswith("ASK UP FRONT")]

    def test_step0_asks_up_front_for_a_signed_out_tailscale(self) -> None:
        out = self.step0(FAKE_TS_LOGGED_OUT="1", FAKE_TS_JSON=self.TS_ON)
        self.assertIn("ASK UP FRONT: sudo to run `tailscale up`, which prints a sign-in link the person must open",
                      self.asks(out), out)
        signed_in = self.step0(FAKE_TS_JSON=self.TS_ON)
        self.assertFalse([a for a in self.asks(signed_in) if "tailscale up" in a],
                         f"a signed-in Tailscale was asked about:\n{signed_in}")

    def test_step0_asks_up_front_to_install_a_missing_tailscale(self) -> None:
        out = self.step0(PATH=self.without("tailscale"))
        self.assertIn("ASK UP FRONT: sudo to install Tailscale (https://tailscale.com/download), then to run "
                      "`tailscale up`, which prints a sign-in link the person must open", self.asks(out), out)

    def test_step0_asks_for_magicdns_and_https_until_both_are_on(self) -> None:
        want = ("ASK UP FRONT (hub only): the person turns on MagicDNS and HTTPS Certificates in the Tailscale "
                "admin console (https://login.tailscale.com/admin/dns) before you start")
        self.assertIn(want, self.asks(self.step0()))
        only_dns = '{"CertDomains": null, "CurrentTailnet": {"MagicDNSEnabled": true}}'
        self.assertIn(want, self.asks(self.step0(FAKE_TS_JSON=only_dns)), "HTTPS Certificates off was not asked for")
        only_certs = '{"CertDomains": ["hub.example-tailnet.ts.net"], "CurrentTailnet": {"MagicDNSEnabled": false}}'
        self.assertIn(want, self.asks(self.step0(FAKE_TS_JSON=only_certs)), "MagicDNS off was not asked for")
        self.assertNotIn(want, self.asks(self.step0(FAKE_TS_JSON=self.TS_ON)), "asked although both are on")

    def test_step0_asks_for_qrencode_and_linger_in_the_same_batch(self) -> None:
        qr = "ASK UP FRONT (optional): sudo to install qrencode, so `muster doctor` can draw the phone's QR code"
        linger = ('ASK UP FRONT (Linux): sudo to run `loginctl enable-linger "$USER"`, so muster-mirror keeps '
                  "running after the person logs out (Step 7)")
        out = self.step0(PATH=self.without("qrencode"), FAKE_LINGER="no", FAKE_TS_JSON=self.TS_ON)
        self.assertIn(qr, self.asks(out), out)
        if os.uname().sysname == "Linux":
            self.assertIn(linger, self.asks(out), out)
        done = self.step0(FAKE_LINGER="yes", FAKE_TS_JSON=self.TS_ON)
        self.assertNotIn(qr, self.asks(done))
        self.assertNotIn(linger, self.asks(done))

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

    # --- fresh-run friction: Step 4 shows why the push keys failed -----------

    def test_failed_key_generation_prints_dockers_error(self) -> None:
        self.hub()
        out = self.run_block(self.lib + block("env_secret POSTGRES_PASSWORD"), FAKE_VAPID="fail")
        self.assertIn("PUSH KEYS NOT GENERATED", out)
        self.assertIn("  pull denied", out, f"docker's own error was hidden\n{out}")
        self.assertEqual((self.homedir / ".config/muster-install/vapid.err").read_text(), "pull denied\n")
        env = self.dotenv()
        for k in ("MUSTER_VAPID_PUBLIC_KEY", "MUSTER_VAPID_PRIVATE_KEY", "MUSTER_VAPID_SUBJECT"):
            self.assertEqual(env.get(k, ""), "", f"{k} written although generation failed")
        # A later success clears the stale error.
        self.run_block(self.lib + block("env_secret POSTGRES_PASSWORD"))
        self.assertFalse((self.homedir / ".config/muster-install/vapid.err").exists())
        fix = " ".join(self.section("## Step 4:").split("**If this fails:**")[1].split())
        self.assertNotIn("docker pull", fix, "the fix still sends the agent to `docker pull`")
        self.assertIn("Read the error printed under it", fix)

    # --- fresh-run friction: Step 7 turns linger on with sudo -----------------

    def mirror_service(self, **env: str) -> str:
        self.sh(f"mi_save OS linux\nmi_save MUSTER_URL '{URL}'\nmi_save MIRROR_HOST laptop\nmi_save INTERACTIVE 1")
        cfg = self.homedir / ".config/muster-mirror"
        cfg.mkdir(parents=True, exist_ok=True)
        (cfg / "token").write_text("mmt_fixture\n")
        return self.run_block(self.lib + block("MUSTER_MIRROR_WRITE_HOOK_CONFIG=1"), USER="tester", **env)

    def test_step7_enables_linger_with_sudo(self) -> None:
        out = self.mirror_service(FAKE_LINGER="no")
        self.assertIn("sudo -n loginctl enable-linger tester", self.calls(), out)
        self.assertIn("linger: on", out)

    def test_step7_leaves_linger_alone_when_on(self) -> None:
        out = self.mirror_service(FAKE_LINGER="yes")
        self.assertIn("systemctl --user restart muster-mirror.service", self.calls(), "positive control: the Linux branch ran")
        self.assertNotIn("enable-linger", self.calls(), out)
        self.assertIn("linger: already on", out)

    def test_step7_hands_linger_to_the_person_when_sudo_fails(self) -> None:
        out = self.mirror_service(FAKE_LINGER="no", FAKE_SUDO="fail")
        self.assertIn("LINGER NOT ON: ask the person to run, in their own terminal: sudo loginctl enable-linger tester", out)

    def test_step7_names_the_opencode_line_muster_mirror_logs(self) -> None:
        # The seam: the guide quotes the daemon's start-up line as expected, so
        # the line it quotes must be the one the code logs.
        src = (ROOT / "cmd/muster-mirror/opencode.go").read_text()
        m = re.search(r'openCodeNotPairedWhy = "([^"]*)"', src)
        self.assertIsNotNone(m, "cmd/muster-mirror/opencode.go no longer has openCodeNotPairedWhy")
        quoted = re.search(r"`opencode: ([^`…]*) …`", self.section("## Step 7:"))
        self.assertIsNotNone(quoted, "Step 7 does not quote the opencode line")
        self.assertTrue(m.group(1).startswith(quoted.group(1)),
                        f"Step 7 quotes {quoted.group(1)!r}; muster-mirror logs {m.group(1)!r}")

    # --- fresh-run friction: Step 6's PATH note only when it is needed --------

    def install_binaries(self, profile_dir: str = "", **env: str) -> str:
        self.sh("mi_save OS linux\nmi_save ARCH amd64\nmi_set_version 1.0.0")
        bindir = self.homedir / ".local/bin"
        bindir.mkdir(parents=True)
        muster = bindir / "muster"
        muster.write_text("#!/bin/sh\necho muster 1.0.0\n")
        muster.chmod(0o755)
        if profile_dir:
            (self.homedir / ".profile-path").write_text(profile_dir + "\n")
        return self.run_block(self.lib + block('mi_verify "$asset"'), SHELL=str(self.bin / "login-shell"), **env)

    def test_step6_no_path_note_when_a_login_shell_finds_muster(self) -> None:
        out = self.install_binaries(f"{self.homedir}/.local/bin")
        self.assertNotIn("NOTE:", out, "a PATH note although a new login shell finds muster")

    def test_step6_path_note_when_a_login_shell_does_not(self) -> None:
        out = self.install_binaries()
        self.assertIn(f"NOTE: a new login shell does not find {self.homedir}/.local/bin/muster", out)

    def test_step6_path_check_ignores_this_shells_path(self) -> None:
        # The agent's own PATH has the directory; a new login (no profile line)
        # would not. Only `env -i` keeps the check from inheriting it.
        out = self.install_binaries(PATH=f"{self.homedir}/.local/bin:{self.env['PATH']}")
        self.assertIn(f"NOTE: a new login shell does not find {self.homedir}/.local/bin/muster", out,
                      "the login shell inherited this shell's PATH")

    # --- fresh-run friction: the state file is private ------------------------

    def test_mi_save_writes_the_state_file_mode_600(self) -> None:
        state = self.homedir / ".config/muster-install/state"
        state.unlink()
        self.sh("umask 022\nmi_save SOME_KEY value")
        self.assertEqual(stat.S_IMODE(state.stat().st_mode), 0o600)
        state.chmod(0o644)  # one written before this fix
        self.sh("umask 022\nmi_save SOME_KEY other")
        self.assertEqual(stat.S_IMODE(state.stat().st_mode), 0o600)
        self.assertIn("SOME_KEY='other'", self.state())

    # --- final pass: D1, D2, Y2, G4 -------------------------------------------

    def test_step10_shows_only_doctors_report(self) -> None:
        # D1: the CLI keeps stdout one JSON document; the guide drops it.
        self.hub()
        bindir = self.homedir / ".local/bin"
        bindir.mkdir(parents=True)
        fake = bindir / "muster"
        fake.write_text('#!/bin/sh\necho "PASS  server" >&2\necho "1 passed, 0 failed" >&2\n'
                        'echo \'{"ok": true, "checks": []}\'\n')
        fake.chmod(0o755)
        for b in (block('doctor --deploy-env "$MUSTER_DIR/.env"'), block('doctor --api-url "$MUSTER_URL"')):
            out = self.run_block(self.lib + b)
            self.assertIn("PASS  server", out, "positive control: the doctor ran")
            self.assertNotIn('"checks"', out, f"the JSON copy reached the agent:\n{out}")

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

    def test_step7_skips_linger_the_person_declined(self) -> None:
        self.sh("mi_save LINGER no")
        out = self.mirror_service(FAKE_LINGER="no")
        self.assertIn("systemctl --user restart muster-mirror.service", self.calls(), "positive control: the Linux branch ran")
        self.assertNotIn("enable-linger", self.calls(), out)
        self.assertIn("linger: skipped (declined in Step 1)", out)

    def test_step7_without_loginctl_asks_for_no_sudo(self) -> None:
        out = self.mirror_service(PATH=self.without("loginctl"))
        self.assertNotIn("sudo", self.calls(), out)
        self.assertIn("linger: no loginctl here", out)

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
        self.run_block(self.lib + block("mi_save MUSTER_URL"), PATH=path, MI_TS_APP=app)
        self.assertIn("tailscale serve --bg --https=443 8105", self.calls())
        self.assertIn(f"MUSTER_URL='{URL}'", self.state())


if __name__ == "__main__":
    unittest.main(verbosity=2)

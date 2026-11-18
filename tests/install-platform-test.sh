#!/usr/bin/env bash
# deploy/install.sh's platform detection, its package-manager choice and its
# checksum refusal — with NO network, Docker, systemd or sudo: every tool that
# would reach outside is a fake on a PATH that holds nothing else.
#
#   tests/install-platform-test.sh [install.sh]     (CI: release-dry-run)
#
# The argument defaults to deploy/install.sh; it is a parameter so the cases
# can be pointed at an older copy (that is how each was shown red before the
# change: `git show <base>:deploy/install.sh > /tmp/old.sh`).
#
#   * platform(): the uname matrix — Linux, macOS, Windows (Git Bash/MINGW,
#     MSYS2, Cygwin) on x86_64/aarch64/arm64, and refusals (FreeBSD, riscv64);
#   * Windows, end to end: refused before anything runs or changes, naming
#     the release's Windows zip and Scoop manifest; NEGATIVE CONTROL, the same
#     run as Linux gets past that point (so the refusal is Windows's own);
#   * install_method(): dpkg (else rpm) as root or with `sudo -n` -> a package;
#     sudo needing a password, macOS, MUSTER_BIN set, MUSTER_PACKAGE=tarball,
#     no package manager, or a release without packages -> the archives;
#     MUSTER_PACKAGE=deb on a release without one, or a bogus value -> refused;
#   * a joining machine end to end (fake curl serves fixture assets): the
#     archives into MUSTER_BIN; a .deb through `sudo dpkg -i` when dpkg and
#     passwordless sudo are there; and a CHECKSUM MISMATCH (archive or
#     package) refused with nothing installed and no dpkg run.
#
# Exit 0 all pass; 1 a case failed; 2 harness error.
set -uo pipefail

ROOT="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
INSTALL="${1:-$ROOT/deploy/install.sh}"
[ -f "$INSTALL" ] || { echo "install-platform-test: no $INSTALL" >&2; exit 2; }
work="$(mktemp -d)"; trap 'rm -rf "$work"' EXIT

# --- a PATH with only the real tools install.sh uses, plus fakes -------------
real="$work/real"; mkdir -p "$real"
for t in sh tar gzip sed grep sha256sum mktemp rm mkdir cp chmod mv cat cut dirname date sleep sort head tr env ls wc printf; do
  p="$(command -v "$t" 2>/dev/null)" || { echo "harness: $t is not on PATH" >&2; exit 2; }
  case "$p" in /*) ln -s "$p" "$real/$t" ;; esac
done
fake="$work/fake"; mkdir -p "$fake"
mkfake() { printf '#!/bin/sh\n%s\n' "$2" > "$fake/$1"; chmod +x "$fake/$1"; }
# Every call to a fake is logged: "<tool> <args>".
mkfake uname 'echo "uname $*" >> "$FAKE_LOG"; case "$1" in -s) echo "$FAKE_S" ;; -m) echo "$FAKE_M" ;; esac'
mkfake hostname 'echo testhost'
mkfake id 'case "$1" in -u) echo "${FAKE_UID:-1000}" ;; -un) echo tester ;; esac'
mkfake docker 'echo "docker $*" >> "$FAKE_LOG"; exit 1'
mkfake systemctl 'echo "systemctl $*" >> "$FAKE_LOG"; exit 1'
mkfake tmux 'exit 0'
# curl: /health answers the hub's version; -o downloads copy from $FAKE_REL;
# -w %{http_code} answers 200.
mkfake curl 'echo "curl $*" >> "$FAKE_LOG"
out=""; url=""; code=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift ;;
    -w) code=1; shift ;;
    -H|-m) shift ;;
    -*) ;;
    *) url="$1" ;;
  esac
  shift
done
case "$url" in
  */health) echo "{\"status\": \"ok\", \"version\": \"1.2.3\"}" ;;
  */mirror/host) [ -n "$code" ] && printf 200 || echo "{\"online\": true}" ;;
  *) f="$FAKE_REL/${url##*/}"; [ -f "$f" ] || exit 22; [ -z "$out" ] || [ "$out" = /dev/null ] || cp "$f" "$out" ;;
esac'
# Package managers and sudo exist only where a case puts them (opt/).
opt="$work/opt"; mkdir -p "$opt"
mkopt() { printf '#!/bin/sh\n%s\n' "$2" > "$opt/$1"; chmod +x "$opt/$1"; }
# dpkg -i <file>: "installs" by copying the fixture program into $MUSTER_PACKAGE_BIN.
mkopt dpkg 'echo "dpkg $*" >> "$FAKE_LOG"; [ "$1" = -i ] || exit 0
n=${2##*/}; n=${n%%_*}; cp "$FAKE_SRC/$n" "$MUSTER_PACKAGE_BIN/$n"'
mkopt rpm 'echo "rpm $*" >> "$FAKE_LOG"'
mkopt sudo-ok 'echo "sudo $*" >> "$FAKE_LOG"; [ "$1" = -n ] && shift; "$@"'
mkopt sudo-pw 'echo "sudo $*" >> "$FAKE_LOG"; [ "$1" = -n ] && { echo "sudo: a password is required" >&2; exit 1; }; exit 1'

# with <tools...> -- : the opt tools a case has (sudo-ok/sudo-pw become sudo).
casebin=""
with() {
  casebin="$work/case-$RANDOM$RANDOM"; mkdir -p "$casebin"
  while [ $# -gt 0 ] && [ "$1" != -- ]; do
    case "$1" in sudo-ok|sudo-pw) ln -s "$opt/$1" "$casebin/sudo" ;; *) ln -s "$opt/$1" "$casebin/$1" ;; esac
    shift
  done
}

# --- fixture release (version 1.2.3) ------------------------------------------
rel="$work/rel"; mkdir -p "$rel" "$work/src"
for n in muster muster-mirror; do
  printf '#!/bin/sh\necho "%s $*" >> "$FAKE_LOG"\nexit 0\n' "$n" > "$work/src/$n"; chmod +x "$work/src/$n"
  for a in amd64 arm64; do
    tar -C "$work/src" -czf "$rel/${n}_linux_${a}.tar.gz" "$n"
    echo "deb $n $a" > "$rel/${n}_linux_${a}.deb"
    echo "rpm $n $a" > "$rel/${n}_linux_${a}.rpm"
  done
done
(cd "$rel" && sha256sum ./* | sed 's#  \./#  #' > checksums.txt)
# The same release before packages existed.
relold="$work/rel-old"; mkdir -p "$relold"
cp "$rel"/*.tar.gz "$relold/"; (cd "$relold" && sha256sum ./* | sed 's#  \./#  #' > checksums.txt)

fails=0; passes=0
ok() { echo "PASS  $1"; passes=$((passes + 1)); }
bad() { echo "FAIL  $1: $2"; [ -f "$work/out" ] && sed 's/^/        /' "$work/out"; fails=$((fails + 1)); }

# The functions alone: install.sh without its last line (`main "$@"`).
sed '$d' "$INSTALL" > "$work/lib.sh"
for fn in platform install_method; do
  grep -q "^$fn() {" "$work/lib.sh" || { bad "harness" "install.sh has no $fn()"; }
done

# --- 1. platform(): the uname matrix -----------------------------------------
detect() { # <uname -s> <uname -m> -> "OS/ARCH"
  : > "$work/log"
  env -i PATH="$fake:$real" FAKE_LOG="$work/log" FAKE_S="$1" FAKE_M="$2" \
    sh -c ". '$work/lib.sh'; platform; echo \"\$OS/\$ARCH\"" 2>&1
}
while read -r s m want; do
  got="$(detect "$s" "$m")"
  if [ "$got" = "$want" ]; then ok "platform: $s $m -> $want"; else bad "platform: $s $m" "got '$got', want '$want'"; fi
done <<'M'
Linux x86_64 linux/amd64
Linux aarch64 linux/arm64
Linux arm64 linux/arm64
Darwin x86_64 darwin/amd64
Darwin arm64 darwin/arm64
MINGW64_NT-10.0-22631 x86_64 windows/amd64
MSYS_NT-10.0-22631 x86_64 windows/amd64
CYGWIN_NT-10.0-22631 x86_64 windows/amd64
MINGW64_NT-10.0-26100 aarch64 windows/arm64
Windows_NT ARM64 windows/arm64
FreeBSD amd64 /amd64
Linux riscv64 linux/
M

# --- 2. the whole script, joining a hub: fake curl serves $FAKE_REL -----------
# run_install <uname -s> <uname -m> <release dir> [VAR=value...]
run_install() {
  local s="$1" m="$2" r="$3"; shift 3
  home="$work/home-$RANDOM$RANDOM"; mkdir -p "$home/.config/muster-mirror" "$home/pkgbin"
  echo "tok-123" > "$home/.config/muster-mirror/token"; chmod 600 "$home/.config/muster-mirror/token"
  : > "$work/log"
  env -i HOME="$home" PATH="$casebin:$fake:$real" FAKE_LOG="$work/log" FAKE_S="$s" FAKE_M="$m" FAKE_REL="$r" FAKE_SRC="$work/src" \
    MUSTER_JOIN_URL=https://hub.example MUSTER_RELEASE_BASE=https://rel.example MUSTER_PACKAGE_BIN="$home/pkgbin" \
    "$@" sh "$INSTALL" > "$work/out" 2>&1 </dev/null
  rc=$?
}
with --

run_install MINGW64_NT-10.0-22631 x86_64 "$rel"
if [ "$rc" -ne 0 ] && grep -q 'muster_windows_amd64.zip' "$work/out" && grep -q 'scoop install .*muster.json' "$work/out" &&
   grep -q 'tmux' "$work/out" && ! grep -qE '^(curl|docker|systemctl) ' "$work/log" && [ ! -e "$home/.local" ]; then
  ok "Windows (Git Bash): refused before any download, naming the zip and the Scoop manifest"
else bad "Windows (Git Bash)" "rc=$rc; calls: $(tr '\n' ';' < "$work/log")"; fi
run_install MSYS_NT-10.0 aarch64 "$rel"
if [ "$rc" -ne 0 ] && grep -q 'muster_windows_arm64.zip' "$work/out"; then ok "Windows (MSYS2, arm64): refused, naming the arm64 zip"
else bad "Windows (MSYS2, arm64)" "rc=$rc"; fi
# NEGATIVE CONTROL: the same run as Linux is not refused for its platform.
run_install Linux x86_64 "$rel"
if [ "$rc" -eq 0 ] && ! grep -q 'muster_windows' "$work/out"; then ok "CONTROL: the same run on Linux is not refused"
else bad "CONTROL: the same run on Linux" "rc=$rc"; fi

# --- 3. install_method(): which way the programs are installed ---------------
# method <release dir> <tools...> -- [VAR=value...] ; prints the method (or "refused")
method() {
  local r="$1"; shift
  with "$@"; while [ "$1" != -- ]; do shift; done; shift
  : > "$work/log"
  env -i HOME="$work" PATH="$casebin:$fake:$real" FAKE_LOG="$work/log" "$@" \
    sh -c ". '$work/lib.sh'; OS=\${T_OS:-linux}; ARCH=amd64; RELEASE=1.2.3; BIN=\$HOME/.local/bin; install_method '$r/checksums.txt'" \
    2>"$work/err" || echo refused
}
mcase() { # <description> <want> <release dir> <tools...> -- [VAR=value...]
  local d="$1" want="$2"; shift 2
  local got; got="$(method "$@")"
  # A refusal must be THE refusal (a missing function also "fails").
  if [ "$want" = refused ] && ! grep -q 'MUSTER_PACKAGE=' "$work/err"; then got="refused without naming MUSTER_PACKAGE"; fi
  if [ "$got" = "$want" ]; then ok "install_method: $d -> $want"; else bad "install_method: $d" "got '$got', want '$want' ($(cat "$work/err"))"; fi
}
mcase "dpkg, root" deb "$rel" dpkg -- FAKE_UID=0
mcase "dpkg, sudo without a password" deb "$rel" dpkg sudo-ok --
mcase "rpm only, sudo without a password" rpm "$rel" rpm sudo-ok --
mcase "dpkg and rpm: dpkg wins" deb "$rel" dpkg rpm sudo-ok --
mcase "dpkg, sudo needs a password" tarball "$rel" dpkg sudo-pw --
mcase "dpkg, no sudo at all" tarball "$rel" dpkg --
mcase "no package manager" tarball "$rel" sudo-ok --
mcase "macOS" tarball "$rel" dpkg sudo-ok -- T_OS=darwin
mcase "MUSTER_BIN set" tarball "$rel" dpkg sudo-ok -- MUSTER_BIN=/somewhere
mcase "MUSTER_PACKAGE=tarball" tarball "$rel" dpkg sudo-ok -- MUSTER_PACKAGE=tarball
mcase "a release with no packages" tarball "$relold" dpkg sudo-ok --
mcase "MUSTER_PACKAGE=rpm without rpm's sudo -n (asked as given)" rpm "$rel" -- MUSTER_PACKAGE=rpm
mcase "MUSTER_PACKAGE=deb, a release with no packages" refused "$relold" dpkg sudo-ok -- MUSTER_PACKAGE=deb
mcase "MUSTER_PACKAGE=deb on macOS" refused "$rel" dpkg sudo-ok -- MUSTER_PACKAGE=deb T_OS=darwin
# A bogus MUSTER_PACKAGE is refused by main's preflight, before anything runs.
with dpkg sudo-ok --
run_install Linux x86_64 "$rel" MUSTER_PACKAGE=snap
if [ "$rc" -ne 0 ] && grep -q 'MUSTER_PACKAGE must be' "$work/out" && ! grep -q '^curl ' "$work/log"; then ok "MUSTER_PACKAGE=snap: refused before any download"
else bad "MUSTER_PACKAGE=snap" "rc=$rc"; fi

# --- 4. end to end: what gets installed, and the checksum refusal -------------
with --
mb="$home/bin"
run_install Linux x86_64 "$rel" MUSTER_BIN="$mb"
if [ "$rc" -eq 0 ] && [ -x "$mb/muster" ] && [ -x "$mb/muster-mirror" ] && grep -q "^muster doctor " "$work/log" &&
   ! grep -qE '^(dpkg|sudo) ' "$work/log"; then ok "archives: both programs in MUSTER_BIN, its doctor run"
else bad "archives into MUSTER_BIN" "rc=$rc; calls: $(tr '\n' ';' < "$work/log")"; fi

with dpkg sudo-ok --
run_install Linux aarch64 "$rel"
if [ "$rc" -eq 0 ] && grep -q '^sudo dpkg -i .*/muster-mirror_linux_arm64\.deb$' "$work/log" &&
   grep -q '^sudo dpkg -i .*/muster_linux_arm64\.deb$' "$work/log" &&
   grep -q "^muster doctor " "$work/log" && [ ! -e "$home/.local/bin/muster" ] &&
   grep -q "PATH=.*$home/pkgbin" "$home/.config/muster-mirror/env"; then
  ok "packages: dpkg + passwordless sudo installs both .deb files, and the doctor is the package's"
else bad "packages via sudo dpkg" "rc=$rc; calls: $(tr '\n' ';' < "$work/log")"; fi

# CHECKSUM MISMATCH: a tampered asset is refused, nothing installed. (For the
# archive this is an INVARIANT guard: install.sh refused it before packages
# existed too. The package case is new.)
tam="$work/rel-tampered"; rm -rf "$tam"; cp -R "$rel" "$tam"; echo tampered > "$tam/muster_linux_amd64.tar.gz"
with --
run_install Linux x86_64 "$tam" MUSTER_BIN="$work/tbin"
if [ "$rc" -ne 0 ] && grep -q 'muster_linux_amd64.tar.gz does not match the release.s checksums.txt' "$work/out" &&
   [ ! -e "$work/tbin/muster" ] && ! grep -q '^muster doctor' "$work/log"; then ok "a tampered archive is refused, the CLI not installed"
else bad "tampered archive" "rc=$rc; $(ls "$work/tbin" 2>&1)"; fi
cp -R "$rel/." "$tam/"; echo tampered > "$tam/muster_linux_amd64.deb"
with dpkg sudo-ok --
run_install Linux x86_64 "$tam"
if [ "$rc" -ne 0 ] && grep -q 'muster_linux_amd64.deb does not match' "$work/out" && ! grep -q 'dpkg -i' "$work/log"; then
  ok "a tampered package is refused before ANY package is installed"
else bad "tampered package" "rc=$rc; calls: $(tr '\n' ';' < "$work/log")"; fi

echo "install-platform-test: $passes passed, $fails failed"
[ "$fails" -eq 0 ] && [ "$passes" -ge 30 ]

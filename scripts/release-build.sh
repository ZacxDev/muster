#!/usr/bin/env bash
# Build the exact asset set a muster GitHub Release publishes, into dist/.
#
#   scripts/release-build.sh <version>        e.g. 1.2.3 or v1.2.3
#   DIST=/some/dir scripts/release-build.sh 1.2.3
#
# The release job in .github/workflows/ci.yml runs THIS script on a vX.Y.Z tag
# and uploads dist/* as the release's assets; the `release-dry-run` job runs it
# on every pull request with a placeholder version. So what a tag publishes is
# what a pull request already built — there is no second recipe to drift.
#
# Assets (OS x ARCH = linux|darwin x amd64|arm64 unless noted):
#   muster-mirror_<os>_<arch>.tar.gz     the per-machine mirror daemon
#   muster_<os>_<arch>.tar.gz            the CLI
#   muster-server_linux_<arch>.tar.gz    the server (linux only)
#   docker-compose.yml                   deploy/docker-compose.yml, verbatim
#   env.example                          deploy/.env.example, MUSTER_VERSION filled in
#                                        (save it as .env — see below)
#   checksums.txt                        sha256 of every file above
#
# 🔴 THE ENV FILE IS PUBLISHED AS `env.example`, WITHOUT THE LEADING DOT. GitHub
# renames a release asset whose name starts with a period (it does not keep
# `.env.example`), so the only download URL that can be documented in advance is
# .../releases/download/vX.Y.Z/env.example. Download it as `.env`.
#
# Version stamps (each binary's own version flag prints <version>):
#   muster-server  -X internal/api.BuildVersion   `muster-server -version`
#   muster-mirror  -X main.buildRevision          `muster-mirror -version`
#   muster (CLI)   -X main.buildVersion (+ main.buildRevision = git sha)
#                                                 `muster --version`
#
# 🔴 THE SCRIPT VERIFIES ITS OWN OUTPUT BEFORE IT EXITS 0: the asset set must be
# EXACTLY the list above (nothing missing, nothing extra), checksums.txt must
# verify, every archive must hold its one binary, and every binary this host
# can execute must print the version. A release that uploads "whatever landed
# in dist/" is a release nobody checked.
#
# Needs: go, GNU tar, gzip, sha256sum. Exit 0 ok, 1 a check failed, 2 usage.
set -euo pipefail

die() { echo "release-build: $1" >&2; exit "${2:-1}"; }

[ $# -eq 1 ] || die "usage: $0 <version>   (X.Y.Z, a leading v is stripped)" 2
VERSION="${1#v}"
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "version '$1' is not X.Y.Z" 2

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST="${DIST:-$ROOT/dist}"
MODULE="github.com/ZacxDev/muster"
REV="$(git -C "$ROOT" rev-parse --short=7 HEAD 2>/dev/null || echo unknown)"
# Reproducible archive metadata: the commit's own time, not the build's.
SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-$(git -C "$ROOT" log -1 --format=%ct 2>/dev/null || echo 0)}"

for tool in go tar gzip sha256sum; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is not on PATH" 2
done
tar --version 2>/dev/null | grep -q 'GNU tar' || die "GNU tar is required (reproducible --sort/--mtime)" 2

[ -f "$ROOT/deploy/docker-compose.yml" ] || die "deploy/docker-compose.yml is missing" 2
[ -f "$ROOT/deploy/.env.example" ] || die "deploy/.env.example is missing" 2

rm -rf "$DIST"
mkdir -p "$DIST"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

OSES=(linux darwin)
ARCHES=(amd64 arm64)

# build <pkg> <binary-name> <os> <arch> <ldflags-X...>
build() {
  local pkg="$1" name="$2" os="$3" arch="$4"; shift 4
  local x=() flag
  for flag in "$@"; do x+=("-X" "$flag"); done
  local out="$WORK/${name}_${os}_${arch}"
  mkdir -p "$out"
  echo "build: $name $os/$arch"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go -C "$ROOT" build -trimpath -ldflags="-s -w ${x[*]}" -o "$out/$name" "./cmd/$pkg"
  cp "$ROOT/LICENSE" "$ROOT/README.md" "$out/"
  tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="@$SOURCE_DATE_EPOCH" \
    -C "$out" -cf - "$name" LICENSE README.md | gzip -n -9 > "$DIST/${name}_${os}_${arch}.tar.gz"
}

for os in "${OSES[@]}"; do
  for arch in "${ARCHES[@]}"; do
    build muster-mirror muster-mirror "$os" "$arch" "main.buildRevision=$VERSION"
    build muster muster "$os" "$arch" "main.buildVersion=$VERSION" "main.buildRevision=$REV"
  done
done
for arch in "${ARCHES[@]}"; do
  build muster-server muster-server linux "$arch" "$MODULE/internal/api.BuildVersion=$VERSION"
done

cp "$ROOT/deploy/docker-compose.yml" "$DIST/docker-compose.yml"
# The attached .env.example pins the release it came from; the repository copy
# leaves MUSTER_VERSION empty so the compose file refuses to start without one.
grep -q '^MUSTER_VERSION=$' "$ROOT/deploy/.env.example" ||
  die "deploy/.env.example has no empty 'MUSTER_VERSION=' line to fill in"
sed "s/^MUSTER_VERSION=\$/MUSTER_VERSION=$VERSION/" "$ROOT/deploy/.env.example" > "$DIST/env.example"

(cd "$DIST" && find . -maxdepth 1 -type f ! -name checksums.txt -printf '%f\n' | LC_ALL=C sort |
  xargs sha256sum > checksums.txt)

# --- self-verification ------------------------------------------------------
# 🔴 THE CONTRACT, SPELLED LITERALLY — never derived from OSES/ARCHES above.
# A list built from the same loop variables that drive the build cannot notice
# the loop shrinking: dropping arm64 there would drop it here too and pass
# (measured: that mutant survived the derived version of this check).
expected=(
  muster-mirror_linux_amd64.tar.gz  muster-mirror_linux_arm64.tar.gz
  muster-mirror_darwin_amd64.tar.gz muster-mirror_darwin_arm64.tar.gz
  muster_linux_amd64.tar.gz         muster_linux_arm64.tar.gz
  muster_darwin_amd64.tar.gz        muster_darwin_arm64.tar.gz
  muster-server_linux_amd64.tar.gz  muster-server_linux_arm64.tar.gz
  docker-compose.yml env.example checksums.txt
)

want="$(printf '%s\n' "${expected[@]}" | LC_ALL=C sort)"
have="$(cd "$DIST" && find . -maxdepth 1 -type f -printf '%f\n' | LC_ALL=C sort)"
[ "$want" = "$have" ] || die "asset set mismatch:
$(diff <(echo "$want") <(echo "$have") || true)"

(cd "$DIST" && sha256sum --check --quiet checksums.txt) || die "checksums.txt does not verify"
[ "$(wc -l < "$DIST/checksums.txt")" -eq $(( ${#expected[@]} - 1 )) ] ||
  die "checksums.txt does not list every asset"
grep -q "^MUSTER_VERSION=$VERSION\$" "$DIST/env.example" || die "env.example does not pin $VERSION"
# No asset may start with a period (GitHub would rename it; see the header).
if (cd "$DIST" && find . -maxdepth 1 -type f -name '.*' | grep -q .); then die "an asset name starts with '.'"; fi

host_os="$(go env GOHOSTOS)"; host_arch="$(go env GOHOSTARCH)"
ran=0
for f in "$DIST"/*.tar.gz; do
  base="$(basename "$f" .tar.gz)"            # e.g. muster-mirror_linux_amd64
  name="${base%_*_*}"
  members="$(tar -tzf "$f" | LC_ALL=C sort | tr '\n' ' ')"
  [ "$members" = "LICENSE README.md $name " ] || die "$f holds '$members', want '$name LICENSE README.md'"
  [ "$base" = "${name}_${host_os}_${host_arch}" ] || continue
  xdir="$WORK/run-$name"; mkdir -p "$xdir"; tar -xzf "$f" -C "$xdir"
  flag=-version; [ "$name" = muster ] && flag=--version
  out="$("$xdir/$name" "$flag" 2>&1)" || die "$name $flag exited non-zero: $out"
  case "$out" in
    *"$VERSION"*) echo "verify: $name ($host_os/$host_arch) reports: $out"; ran=$((ran + 1)) ;;
    *) die "$name -version printed '$out', which does not carry $VERSION" ;;
  esac
done
# Positive control on the loop above: on a linux or darwin amd64/arm64 host it
# must have executed the native mirror and CLI (and the server on linux).
case "$host_os/$host_arch" in
  linux/amd64|linux/arm64) [ "$ran" -eq 3 ] || die "ran $ran native binaries, want 3" ;;
  darwin/amd64|darwin/arm64) [ "$ran" -eq 2 ] || die "ran $ran native binaries, want 2" ;;
esac

echo "release-build: $VERSION (rev $REV) -> $DIST"
(cd "$DIST" && cat checksums.txt)

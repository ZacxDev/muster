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
# Assets (ARCH = amd64|arm64; docs/releases.md lists them for people, and
# tests/release_platforms_test.py holds that list and `expected` below equal):
#   muster-mirror_<os>_<arch>.tar.gz     the per-machine mirror daemon (linux, darwin)
#   muster_<os>_<arch>.tar.gz            the CLI (linux, darwin)
#   muster_windows_<arch>.zip            the CLI on Windows (muster.exe). NO Windows
#                                        muster-mirror: it drives tmux, which Windows
#                                        does not have (under WSL, use the linux build)
#   muster_linux_<arch>.deb / .rpm       the CLI as a package (/usr/bin/muster)
#   muster-mirror_linux_<arch>.deb / .rpm  muster-mirror as a package
#                                        (/usr/bin/muster-mirror; recommends tmux),
#                                        built by nfpm from deploy/packaging/nfpm.yaml
#   muster.rb, muster-mirror.rb          Homebrew formulas, and
#   muster.json                          the Scoop manifest, GENERATED from this
#                                        release's checksums (scripts/release-manifests.py);
#                                        attached to the release, not published to a
#                                        tap or bucket
#   docker-compose.yml                   deploy/docker-compose.yml, its image default
#                                        stamped with <version>
#                                        (scripts/stamp-compose-version.sh): with no
#                                        .env it runs this release
#   env.example                          deploy/.env.example, MUSTER_VERSION filled in
#                                        (save it as .env — see below)
#   muster-<version>.tgz                 the Helm chart (charts/muster), its `version`
#                                        AND `appVersion` set to <version> — so the
#                                        chart's default image tag is this release's
#                                        (docs/kubernetes.md)
#   install.sh                           deploy/install.sh, its release stamped:
#                                        `curl -fsSL <release>/install.sh | sh`
#   INSTALL.md                           the agent install guide, its release stamped
#                                        (MI_GUIDE_RELEASE), so the guide the one
#                                        prompt downloads (.../latest/download/INSTALL.md)
#                                        installs the release it came with
#   checksums.txt                        sha256 of every file above
#
# ⚠ NO SERVER BINARY, BY DECISION. The server ships as the container image
# (ghcr.io/<owner>/muster:<version>, run by docker-compose.yml); a bare
# muster-server tarball would be a second, unsupported way to run it.
#
# 🔴 THE ENV FILE IS PUBLISHED AS `env.example`, WITHOUT THE LEADING DOT. GitHub
# renames a release asset whose name starts with a period (it does not keep
# `.env.example`), so the only download URL that can be documented in advance is
# .../releases/download/vX.Y.Z/env.example. Download it as `.env`.
#
# Version stamps (each binary's own version flag prints <version>):
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
# BINARY_SUMS=<file>: also write the sha256 of every built binary (the files
# inside the archives and packages) there, one `<sha256>  <dir>` line each,
# for the release job's provenance attestation of each binary
# (actions/attest-build-provenance `subject-checksums`).
#
# NFPM=<path>: the nfpm binary to use; unset, nfpm $NFPM_VERSION is built with
# `go install` (Go's checksum database verifies the module).
#
# REPO=<owner/name> (default $GITHUB_REPOSITORY, else ZacxDev/muster): the
# repository the formulas' and the manifest's download URLs name.
#
# Needs: go, GNU tar, gzip, sha256sum, python3. Exit 0 ok, 1 a check failed, 2 usage.
set -euo pipefail

die() { echo "release-build: $1" >&2; exit "${2:-1}"; }

[ $# -eq 1 ] || die "usage: $0 <version>   (X.Y.Z, a leading v is stripped)" 2
VERSION="${1#v}"
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "version '$1' is not X.Y.Z" 2

# CDPATH unset: with it exported, `cd` to a relative path PRINTS the directory,
# and ROOT became two lines (measured: every `make` target here broke).
ROOT="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
DIST="${DIST:-$ROOT/dist}"
REV="$(git -C "$ROOT" rev-parse --short=7 HEAD 2>/dev/null || echo unknown)"
# Reproducible archive metadata: the commit's own time, not the build's.
SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-$(git -C "$ROOT" log -1 --format=%ct 2>/dev/null || echo 0)}"

for tool in go tar gzip sha256sum python3; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is not on PATH" 2
done
tar --version 2>/dev/null | grep -q 'GNU tar' || die "GNU tar is required (reproducible --sort/--mtime)" 2

[ -f "$ROOT/deploy/docker-compose.yml" ] || die "deploy/docker-compose.yml is missing" 2
[ -f "$ROOT/deploy/.env.example" ] || die "deploy/.env.example is missing" 2
[ -f "$ROOT/charts/muster/Chart.yaml" ] || die "charts/muster/Chart.yaml is missing" 2
[ -f "$ROOT/deploy/install.sh" ] || die "deploy/install.sh is missing" 2
[ -f "$ROOT/INSTALL.md" ] || die "INSTALL.md is missing" 2
[ -f "$ROOT/deploy/packaging/nfpm.yaml" ] || die "deploy/packaging/nfpm.yaml is missing" 2
REPO="${REPO:-${GITHUB_REPOSITORY:-ZacxDev/muster}}"
# The newest nfpm whose go.mod asks for no newer Go than this repository's.
NFPM_VERSION=v2.46.0

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

# The CLI on Windows, as a zip (what Windows opens natively and Scoop installs).
# python3's zipfile, not `zip`: each entry's time is SOURCE_DATE_EPOCH in UTC
# and its mode is fixed, so the bytes depend on neither the clock, the time
# zone nor the umask (`zip` stores local time).
for arch in "${ARCHES[@]}"; do
  out="$WORK/muster_windows_${arch}"
  mkdir -p "$out"
  echo "build: muster windows/$arch"
  CGO_ENABLED=0 GOOS=windows GOARCH="$arch" \
    go -C "$ROOT" build -trimpath -ldflags="-s -w -X main.buildVersion=$VERSION -X main.buildRevision=$REV" -o "$out/muster.exe" ./cmd/muster
  cp "$ROOT/LICENSE" "$ROOT/README.md" "$out/"
  python3 - "$DIST/muster_windows_${arch}.zip" "$SOURCE_DATE_EPOCH" "$out" muster.exe LICENSE README.md <<'PY'
import sys, time, zipfile
dst, epoch, src, *names = sys.argv[1:]
stamp = time.gmtime(max(int(epoch), 315532800))[:6]  # a zip's clock starts in 1980
with zipfile.ZipFile(dst, "w") as z:
    for n in sorted(names):
        info = zipfile.ZipInfo(n, date_time=stamp)
        info.compress_type = zipfile.ZIP_DEFLATED
        info.create_system = 3  # unix: external_attr below is read as a mode
        info.external_attr = (0o755 if n.endswith(".exe") else 0o644) << 16
        with open(f"{src}/{n}", "rb") as f:
            z.writestr(info, f.read(), compresslevel=9)
PY
done

# The .deb and .rpm packages (linux), of the binaries built above.
if [ -n "${NFPM:-}" ]; then
  [ -x "$NFPM" ] || die "NFPM=$NFPM is not an executable" 2
else
  echo "build: nfpm $NFPM_VERSION"
  GOBIN="$WORK/tools" go install "github.com/goreleaser/nfpm/v2/cmd/nfpm@$NFPM_VERSION" ||
    die "could not build nfpm $NFPM_VERSION" 2
  NFPM="$WORK/tools/nfpm"
fi
for name in muster muster-mirror; do
  case "$name" in
    muster) desc="Command-line client for a muster server" ;;
    muster-mirror) desc="Shows this machine's tmux sessions in muster" ;;
  esac
  for arch in "${ARCHES[@]}"; do
    cfg="$WORK/nfpm-${name}-${arch}.yaml"
    sed -e "s#@NAME@#$name#g" -e "s#@ARCH@#$arch#g" -e "s#@VERSION@#$VERSION#g" \
        -e "s#@BIN@#$WORK/${name}_linux_${arch}/$name#g" -e "s#@LICENSE@#$ROOT/LICENSE#g" \
        -e "s#@DESCRIPTION@#$desc#g" -e '/^[[:space:]]*#/d' "$ROOT/deploy/packaging/nfpm.yaml" > "$cfg"
    [ "$name" = muster-mirror ] || sed -i '/# mirror-only$/d' "$cfg"
    if grep -q '@[A-Z]*@' "$cfg"; then
      die "deploy/packaging/nfpm.yaml has a placeholder this script does not fill: $(grep -o '@[A-Z]*@' "$cfg" | head -n 1)"
    fi
    for fmt in deb rpm; do
      echo "build: $name linux/$arch .$fmt"
      SOURCE_DATE_EPOCH="$SOURCE_DATE_EPOCH" "$NFPM" package --config "$cfg" --packager "$fmt" \
        --target "$DIST/${name}_linux_${arch}.$fmt" >/dev/null ||
        die "nfpm could not build ${name}_linux_${arch}.$fmt"
    done
  done
done

# The Helm chart: charts/muster with its placeholders (version 0.0.0,
# appVersion "0.0.0" — never hand-edited) set to this release, archived the way
# `helm package` lays a chart out (one top-level directory named after it), but
# with GNU tar's reproducible flags, so a re-run of the same commit builds the
# same bytes (release-publish refuses to replace an asset with different ones).
# The chart's ci/ value sets are test fixtures and are not shipped.
chart_src="$ROOT/charts/muster"
for line in '^version: 0.0.0$' '^appVersion: "0.0.0"$'; do
  [ "$(grep -c "$line" "$chart_src/Chart.yaml")" = 1 ] ||
    die "charts/muster/Chart.yaml must hold exactly one line matching $line (the placeholder this script fills in)"
done
mkdir -p "$WORK/chart"
cp -R "$chart_src" "$WORK/chart/muster"
rm -rf "$WORK/chart/muster/ci"
sed -i -e "s/^version: 0.0.0\$/version: $VERSION/" -e "s/^appVersion: \"0.0.0\"\$/appVersion: \"$VERSION\"/" "$WORK/chart/muster/Chart.yaml"
tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="@$SOURCE_DATE_EPOCH" \
  -C "$WORK/chart" -cf - muster | gzip -n -9 > "$DIST/muster-$VERSION.tgz"

"$ROOT/scripts/stamp-compose-version.sh" "$VERSION" < "$ROOT/deploy/docker-compose.yml" > "$DIST/docker-compose.yml" ||
  die "could not stamp $VERSION into docker-compose.yml"
grep -q '^RELEASE="@MUSTER_RELEASE@"' "$ROOT/deploy/install.sh" || die "deploy/install.sh has no RELEASE=\"@MUSTER_RELEASE@\" line to stamp"
sed "s/^RELEASE=\"@MUSTER_RELEASE@\"/RELEASE=\"$VERSION\"/" "$ROOT/deploy/install.sh" > "$DIST/install.sh"
chmod 755 "$DIST/install.sh"
[ "$(grep -c '^MI_GUIDE_RELEASE="@MUSTER_RELEASE@"$' "$ROOT/INSTALL.md")" = 1 ] ||
  die "INSTALL.md must hold exactly one MI_GUIDE_RELEASE=\"@MUSTER_RELEASE@\" line to stamp"
sed "s/^MI_GUIDE_RELEASE=\"@MUSTER_RELEASE@\"\$/MI_GUIDE_RELEASE=\"$VERSION\"/" "$ROOT/INSTALL.md" > "$DIST/INSTALL.md"
# The attached .env.example pins the release it came from; the repository copy
# leaves MUSTER_VERSION empty, so the compose file's own default applies.
grep -q '^MUSTER_VERSION=$' "$ROOT/deploy/.env.example" ||
  die "deploy/.env.example has no empty 'MUSTER_VERSION=' line to fill in"
sed "s/^MUSTER_VERSION=\$/MUSTER_VERSION=$VERSION/" "$ROOT/deploy/.env.example" > "$DIST/env.example"

# The Homebrew formulas and the Scoop manifest name the archives by URL and
# hash, so they are generated from the archives' checksums; checksums.txt then
# covers them as well.
(cd "$DIST" && find . -maxdepth 1 -type f -printf '%f\n' | LC_ALL=C sort | xargs sha256sum) > "$WORK/pre-checksums.txt"
python3 "$ROOT/scripts/release-manifests.py" --version "$VERSION" --repo "$REPO" \
  --checksums "$WORK/pre-checksums.txt" --out "$DIST" ||
  die "could not generate the Homebrew formulas and the Scoop manifest"

(cd "$DIST" && find . -maxdepth 1 -type f ! -name checksums.txt -printf '%f\n' | LC_ALL=C sort |
  xargs sha256sum > checksums.txt)

# Every binary, as built (before it went into an archive or a package): the
# subjects of the release job's per-binary provenance attestation.
if [ -n "${BINARY_SUMS:-}" ]; then
  (cd "$WORK" && sha256sum muster-mirror_*_*/muster-mirror muster_*_*/muster muster_windows_*/muster.exe) |
    sed -E 's#  ([^/]+)/[^/]+$#  \1#' > "$BINARY_SUMS"
  [ "$(wc -l < "$BINARY_SUMS")" -eq 10 ] || die "BINARY_SUMS lists $(wc -l < "$BINARY_SUMS") binaries, want 10"
fi

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
  muster_windows_amd64.zip          muster_windows_arm64.zip
  muster_linux_amd64.deb            muster_linux_arm64.deb
  muster_linux_amd64.rpm            muster_linux_arm64.rpm
  muster-mirror_linux_amd64.deb     muster-mirror_linux_arm64.deb
  muster-mirror_linux_amd64.rpm     muster-mirror_linux_arm64.rpm
  muster.rb muster-mirror.rb muster.json
  docker-compose.yml env.example install.sh INSTALL.md checksums.txt
  "muster-$VERSION.tgz"
)

want="$(printf '%s\n' "${expected[@]}" | LC_ALL=C sort)"
have="$(cd "$DIST" && find . -maxdepth 1 -type f -printf '%f\n' | LC_ALL=C sort)"
[ "$want" = "$have" ] || die "asset set mismatch:
$(diff <(echo "$want") <(echo "$have") || true)"

(cd "$DIST" && sha256sum --check --quiet checksums.txt) || die "checksums.txt does not verify"
[ "$(wc -l < "$DIST/checksums.txt")" -eq $(( ${#expected[@]} - 1 )) ] ||
  die "checksums.txt does not list every asset"
grep -q "^MUSTER_VERSION=$VERSION\$" "$DIST/env.example" || die "env.example does not pin $VERSION"
chart_yaml="$(tar -xzOf "$DIST/muster-$VERSION.tgz" muster/Chart.yaml)" || die "the chart archive holds no muster/Chart.yaml"
grep -qx "version: $VERSION" <<<"$chart_yaml" || die "the chart's version is not $VERSION"
grep -qx "appVersion: \"$VERSION\"" <<<"$chart_yaml" || die "the chart's appVersion is not $VERSION"
if tar -tzf "$DIST/muster-$VERSION.tgz" | grep -q '^muster/ci/'; then die "the chart archive ships its ci/ value sets"; fi
grep -q "^RELEASE=\"$VERSION\"" "$DIST/install.sh" || die "install.sh does not pin $VERSION"
grep -qx "MI_GUIDE_RELEASE=\"$VERSION\"" "$DIST/INSTALL.md" || die "INSTALL.md does not pin $VERSION"
[ "$("$ROOT/scripts/stamp-compose-version.sh" --current < "$DIST/docker-compose.yml")" = "$VERSION" ] ||
  die "docker-compose.yml does not default to $VERSION in every image"
# The Windows zips: the CLI and nothing else, and a Windows (PE) program.
for arch in "${ARCHES[@]}"; do
  z="$DIST/muster_windows_${arch}.zip"
  members="$(python3 -c 'import sys, zipfile; print(" ".join(sorted(zipfile.ZipFile(sys.argv[1]).namelist())))' "$z")"
  [ "$members" = "LICENSE README.md muster.exe" ] || die "$z holds '$members', want 'LICENSE README.md muster.exe'"
  magic="$(python3 -c 'import sys, zipfile; print(zipfile.ZipFile(sys.argv[1]).read("muster.exe")[:2].decode("latin-1"))' "$z")"
  [ "$magic" = MZ ] || die "$z: muster.exe is not a Windows program"
done
# The packages: a deb is an ar archive, an rpm starts with the rpm lead. And
# 🔴 neither carries the building machine's name: nfpm writes the hostname
# into an rpm unless deploy/packaging/nfpm.yaml fixes it, which leaks it and
# changes the bytes on every runner.
host_name="$(hostname 2>/dev/null || uname -n)"
for f in "$DIST"/*.deb "$DIST"/*.rpm; do
  case "$f" in
    *.deb) [ "$(head -c 7 "$f")" = '!<arch>' ] || die "$f is not a deb (ar) archive" ;;
    *.rpm) [ "$(head -c 4 "$f" | od -An -tx1 | tr -d ' \n')" = edabeedb ] || die "$f is not an rpm" ;;
  esac
  if [ "${#host_name}" -ge 4 ] && grep -aqF "$host_name" "$f"; then die "$f carries this machine's hostname ($host_name)"; fi
done
# The formulas and the manifest name this release's own archives, with the
# hashes checksums.txt verifies.
for f in muster.rb muster-mirror.rb muster.json; do
  grep -q "releases/download/v$VERSION/" "$DIST/$f" || die "$f does not name release v$VERSION's assets"
done
for a in muster_darwin_arm64.tar.gz muster_linux_amd64.tar.gz muster-mirror_darwin_amd64.tar.gz \
         muster-mirror_linux_arm64.tar.gz muster_windows_amd64.zip muster_windows_arm64.zip; do
  h="$(grep "  $a\$" "$DIST/checksums.txt" | cut -d' ' -f1)"
  [ -n "$h" ] && grep -qF "$h" "$DIST/muster.rb" "$DIST/muster-mirror.rb" "$DIST/muster.json" ||
    die "no formula or manifest carries $a's sha256 from checksums.txt"
done
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
# must have executed the native mirror and CLI.
case "$host_os/$host_arch" in
  linux/amd64|linux/arm64|darwin/amd64|darwin/arm64)
    [ "$ran" -eq 2 ] || die "ran $ran native binaries, want 2" ;;
esac

echo "release-build: $VERSION (rev $REV) -> $DIST"
(cd "$DIST" && cat checksums.txt)
